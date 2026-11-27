// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/google/uuid"
)

// Single-file publish query parameters.
const (
	// paramName selects the single-file fast path and names the file. A
	// later two-step publish posts a JSON manifest without it.
	paramName = "name"
	// paramTitle is the artifact title; it defaults to the file name.
	paramTitle = "title"
	// paramScope is the home scope; it defaults to the caller's own.
	paramScope = "scope"
	// paramExpiresIn optionally sets the artifact's retention at publish time
	// (e.g. "720h", "30d", or "never").
	paramExpiresIn = "expires_in"
)

// MinAgentExpiry is the shortest publish-time (and agent-managed) expiry
// allowed for an agent (ptone/scion#4533, ptone/scion#8155).
const MinAgentExpiry = 24 * time.Hour

// maxExpiresInDays caps parsed day durations so time.Duration never overflows.
const maxExpiresInDays = 36500

// ParseExpiresIn parses a publish-time retention value: empty ("") means
// unset (use hub default); "never" (case-insensitive) clears expiry; "<N>d"
// is N whole days; otherwise standard Go duration syntax (e.g. "720h").
// Non-positive, malformed, or overflowing values return an error.
func ParseExpiresIn(raw string) (dur time.Duration, never bool, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false, nil
	}
	if strings.EqualFold(s, "never") {
		return 0, true, nil
	}
	if s[len(s)-1] == 'd' || s[len(s)-1] == 'D' {
		digits := s[:len(s)-1]
		if digits == "" {
			return 0, false, fmt.Errorf("invalid duration %q: use a duration such as 720h, 30d, or never", raw)
		}
		for _, c := range digits {
			if c < '0' || c > '9' {
				return 0, false, fmt.Errorf("invalid duration %q: use a duration such as 720h, 30d, or never", raw)
			}
		}
		days, perr := strconv.Atoi(digits)
		if perr != nil || days <= 0 || days > maxExpiresInDays {
			return 0, false, fmt.Errorf("invalid duration %q: days must be between 1 and %d", raw, maxExpiresInDays)
		}
		return time.Duration(days) * 24 * time.Hour, false, nil
	}
	d, perr := time.ParseDuration(s)
	if perr != nil {
		return 0, false, fmt.Errorf("invalid duration %q: use a duration such as 720h, 30d, or never", raw)
	}
	if d <= 0 || d > time.Duration(maxExpiresInDays)*24*time.Hour {
		return 0, false, fmt.Errorf("duration %q must be positive and at most %dd", raw, maxExpiresInDays)
	}
	return d, false, nil
}

// resolvePublishExpiry validates raw expires_in/expiresIn against caller kind
// and hub retention limits, returning the target expiry timestamp, whether an
// explicit expiry was requested, and any validation error message.
func resolvePublishExpiry(kind, raw string, l Limits, now time.Time) (*time.Time, bool, string) {
	if strings.TrimSpace(raw) == "" {
		return retentionExpiry(l, now), false, ""
	}
	dur, never, err := ParseExpiresIn(raw)
	if err != nil {
		return nil, false, err.Error()
	}
	if kind == PrincipalKindAgent {
		if never {
			if l.DefaultRetention > 0 {
				return nil, false, "cannot set expiry to never when the hub has a default retention"
			}
			return nil, true, ""
		}
		if dur < MinAgentExpiry {
			return nil, false, "expiry must be at least 24 hours from now"
		}
		if l.DefaultRetention > 0 && dur > l.DefaultRetention {
			return nil, false, fmt.Sprintf("expiry exceeds the hub's default retention of %s", l.DefaultRetention)
		}
		t := now.Add(dur).UTC().Truncate(time.Microsecond)
		return &t, true, ""
	}
	if never {
		return nil, true, ""
	}
	t := now.Add(dur).UTC().Truncate(time.Microsecond)
	return &t, true, ""
}

// expiryLengthens reports whether changing an artifact's expiry from current
// to target lengthens (or clears) a finite expiry. A nil current expiry means
// no limit, so any finite target is a shortening.
func expiryLengthens(current, target *time.Time) bool {
	if current == nil {
		return false
	}
	if target == nil {
		return true
	}
	return target.After(*current)
}

// applyPublishExpiry applies an explicit publish-time expiry to a, recording
// the artifact_event with actor_kind = kind and authority = owner and writing
// the decision log entry.
func (s *Service) applyPublishExpiry(ctx context.Context, r *http.Request, b backend, a *Artifact, exp *time.Time, kind, ref string) (*Artifact, error) {
	actor := &Actor{Kind: kind, Ref: ref, Authority: AuthorityOwner}
	updated, err := b.store.SetExpiry(ctx, a.ID, exp, actor)
	if err != nil {
		return nil, err
	}
	action := logActionExpirySet
	if exp == nil {
		action = logActionExpiryCleared
	}
	s.logAllowed(r, logEntry{action: action, artifact: updated, authority: AuthorityOwner})
	return updated, nil
}

// HeaderContentSHA256 optionally carries the hex SHA-256 of the request
// body. When present the service rejects a body that does not match.
const HeaderContentSHA256 = "X-Content-SHA256"

const maxTitleRunes = 512

// BlobPath returns the object path of the blob with the given lowercase hex
// SHA-256 digest: hubs/{hub-id}/artifacts/blobs/sha256/<aa>/<bb>/<hex>.
// Blobs are content-addressed and immutable, so one blob serves every file,
// version and artifact with the same bytes.
func BlobPath(hubID, digest string) string {
	return "hubs/" + hubID + "/artifacts/blobs/sha256/" + digest[0:2] + "/" + digest[2:4] + "/" + digest
}

// canCreateIn reports whether the caller may publish artifacts in scope:
// its credential permits artifact.create there and the host authorizes it.
// It is the create decision: handlePublish and handleCreate refuse with 403
// when it is false (explaining a missing credential scope first), a move
// refuses a target project it is false for, and the list reports it as
// canCreate.
func canCreateIn(ctx context.Context, host Host, scope string) bool {
	return host.Permits(ctx, scope, PermissionCreate) && host.Authorize(ctx, scope, PermissionCreate)
}

// handlePublish implements the single-file fast path: the raw request body
// becomes the only file of a new artifact's first version, which is ready
// at once. The artifact is owned by the caller, homed in the requested or
// the caller's scope, and readable by that scope through a synthetic grant
// (design D7).
func (s *Service) handlePublish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if !q.Has(paramName) {
		writeError(w, http.StatusBadRequest, "bad_request", "missing ?name=: only the single-file publish is supported")
		return
	}
	name, err := cleanFilePath(q.Get(paramName))
	if err != nil || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, "bad_request", "name must be a plain file name")
		return
	}
	if isReservedPath(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "names under "+RemotePrefix+" are reserved")
		return
	}
	title := strings.TrimSpace(q.Get(paramTitle))
	if title == "" {
		title = name
	}
	if !utf8.ValidString(title) || utf8.RuneCountInString(title) > maxTitleRunes {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("title must be valid UTF-8 of at most %d characters", maxTitleRunes))
		return
	}
	var wantDigest string
	if h := r.Header.Get(HeaderContentSHA256); h != "" {
		wantDigest = strings.ToLower(strings.TrimSpace(h))
		if len(wantDigest) != sha256.Size*2 {
			writeError(w, http.StatusBadRequest, "bad_request", HeaderContentSHA256+" must be a hex SHA-256 digest")
			return
		}
	}

	kind, ref, home, ok := s.host.Principal(ctx)
	if !ok {
		if s.writeMissingScope(w, r) {
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	scope := strings.TrimSpace(q.Get(paramScope))
	if scope == "" {
		scope = home
	}
	if scope == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "scope is required")
		return
	}
	if !canCreateIn(ctx, s.host, scope) {
		// A credential that does not permit artifact.create may be
		// explained; the decision itself is canCreateIn's.
		s.logCreateRefusal(r, scope)
		if !s.host.Permits(ctx, scope, PermissionCreate) && s.writeMissingScope(w, r) {
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish artifacts in this scope")
		return
	}
	b, ok := s.backend()
	if !ok {
		writeBackendUnavailable(w, b, "artifact storage is not configured")
		return
	}
	now := time.Now().UTC()
	limits := b.currentLimits(ctx)
	expiresAt, explicitExpiry, expErr := resolvePublishExpiry(kind, q.Get(paramExpiresIn), limits, now)
	if expErr != "" {
		writeError(w, http.StatusBadRequest, "bad_request", expErr)
		return
	}

	// Reject an oversized body before reading or writing anything.
	limit := b.maxFileBytes(ctx)
	if r.ContentLength > limit {
		writeTooLarge(w, limit)
		return
	}
	spool, err := spoolBody(r.Body, limit)
	if errors.Is(err, errTooLarge) {
		writeTooLarge(w, limit)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return
	}
	defer spool.Close()
	if wantDigest != "" && wantDigest != spool.digest {
		writeError(w, http.StatusBadRequest, "digest_mismatch", "body does not match "+HeaderContentSHA256)
		return
	}

	mediaType := detectMediaType(name, r.Header.Get("Content-Type"), spool.head)
	if err := s.putBlob(ctx, b, spool, mediaType); err != nil {
		slog.ErrorContext(ctx, "artifacts: blob write failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not store the file")
		return
	}

	versionID := uuid.NewString()
	f := File{VersionID: versionID, Path: name, Size: spool.size, SHA256: spool.digest, MediaType: mediaType}
	files := []File{f}
	totalBytes := spool.size
	var warnings []string
	if mediaType == mediaTypeMarkdown {
		// Remote images are fetched now, once, so opening the artifact
		// never makes the hub fetch anything.
		window, truncated, err := spool.window()
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: read spooled markdown failed", "error", err)
		}
		remote, warn := s.remoteImages(ctx, w, b, versionID, window, truncated, versionUsage{files: 1, bytes: spool.size})
		warnings = warn
		for _, rf := range remote {
			files = append(files, rf)
			totalBytes += rf.Size
		}
	}
	if mediaType == mediaTypeHTML {
		if window, _, err := spool.window(); err == nil && htmlHasRemoteImages(window) {
			warnings = append(warnings, WarnHTMLRemoteImages)
		}
	}
	a := &Artifact{
		ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: scope,
		OwnerKind: kind, OwnerRef: ref, Title: title, CurrentSeq: 1,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: retentionExpiry(limits, now),
	}
	// The delegating user of an agent creating the artifact, resolved by
	// the host from the agent's delegation chain (ptone/scion#6028); never
	// from the request.
	a.SponsorRef = s.sponsorFor(ctx, kind, ref, a.ScopeKind)
	v := &Version{
		ID: versionID, ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: name,
		TotalBytes: totalBytes, FileCount: len(files), CreatedByKind: kind, CreatedByRef: ref,
		CreatedAt: now, State: VersionStateReady,
	}
	g := Grant{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: scope,
		Permission: GrantRead, CreatedByRef: PrincipalRef(kind, ref), CreatedAt: now,
	}
	// A failure here can leave an unreferenced blob behind; blobs are
	// content-addressed, so a retry reuses it and the blob sweep reclaims it.
	grants := []Grant{g}
	// The automatic review grant for the delegating user commits with the
	// artifact, or not at all (ptone/scion#7134).
	if sg, ok := s.sponsorReviewGrant(r, a, now); ok {
		grants = append(grants, sg)
	}
	if err := b.store.CreatePublished(ctx, a, v, files, grants); err != nil {
		slog.ErrorContext(ctx, "artifacts: publish failed", "error", err)
		s.logVersionError(r, a, 1, logReasonStoreError)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the artifact")
		return
	}
	if explicitExpiry {
		updated, err := s.applyPublishExpiry(ctx, r, b, a, expiresAt, kind, ref)
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: set publish expiry failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not set the artifact expiry")
			return
		}
		a = updated
	}
	s.logAllowed(r, logEntry{action: logActionPublished, artifact: a, seq: 1, authority: logAuthorityOwner})
	s.logSponsorReviewGrants(r, a, grants)
	writeJSON(w, http.StatusCreated, ArtifactResponse{Artifact: artifactInfo(a), Version: versionInfo(v, files), Warnings: warnings})
}

// handlePublishFromAttachment implements POST /api/v1/artifacts with a JSON
// body containing fromAttachment (ptone/scion#4599). It creates a new artifact
// (or appends a ready version when key matches an existing artifact owned by
// the caller in scope) pointing directly at the attachment's existing blob,
// copying no bytes and leaving the attachment unchanged.
func (s *Service) handlePublishFromAttachment(w http.ResponseWriter, r *http.Request, req *CreateVersionRequest) {
	ctx := r.Context()
	if len(req.Files) > 0 || req.Entry != "" || req.Kind != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "fromAttachment cannot be combined with files, entry or kind")
		return
	}
	key := req.Key
	if err := validKey(key); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !utf8.ValidString(req.Note) || utf8.RuneCountInString(req.Note) > maxNoteRunes {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("note must be valid UTF-8 of at most %d characters", maxNoteRunes))
		return
	}
	title := strings.TrimSpace(req.Title)
	if title != "" && (!utf8.ValidString(title) || utf8.RuneCountInString(title) > maxTitleRunes) {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("title must be valid UTF-8 of at most %d characters", maxTitleRunes))
		return
	}
	scope := strings.TrimSpace(req.ScopeRef)
	if s2 := strings.TrimSpace(req.Scope); s2 != "" {
		if scope != "" && scope != s2 {
			writeError(w, http.StatusBadRequest, "bad_request", "scope and scopeRef must match when both are set")
			return
		}
		scope = s2
	}

	kind, ref, home, ok := s.host.Principal(ctx)
	if !ok {
		if s.writeMissingScope(w, r) {
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if scope == "" {
		scope = home
	}
	if scope == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "scope is required")
		return
	}
	b, ok := s.backend()
	if !ok {
		writeBackendUnavailable(w, b, "artifact storage is not configured")
		return
	}
	now := time.Now().UTC()
	limits := b.currentLimits(ctx)
	expiresAt, explicitExpiry, expErr := resolvePublishExpiry(kind, req.ExpiresIn, limits, now)
	if expErr != "" {
		writeError(w, http.StatusBadRequest, "bad_request", expErr)
		return
	}

	// Gate 1: caller must be able to read the attachment (phase 1 message rule).
	// Absent, malformed, or unreadable attachments all return the exact same 404.
	attID := strings.TrimPrefix(strings.TrimSpace(req.FromAttachment), AttachmentRefScheme)
	if !canonicalID(attID) {
		writeNotFound(w)
		return
	}
	att, err := b.store.GetAttachment(ctx, attID)
	if errors.Is(err, ErrNotFound) {
		writeNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: get attachment failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the attachment")
		return
	}
	readable, err := s.canReadErr(ctx, b, att)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: attachment read check failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the attachment")
		return
	}
	if !readable {
		writeNotFound(w)
		return
	}
	attFile, err := attachmentFile(ctx, b, att)
	if errors.Is(err, ErrNotFound) {
		writeNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: read attachment file failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the attachment")
		return
	}

	// Gate 2: caller must pass the publish gate in the target scope
	// (artifact.create, or write on the existing keyed artifact).
	var existing *Artifact
	if key != "" {
		existing, err = b.store.GetArtifactByKey(ctx, ScopeKindProject, scope, kind, ref, key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			slog.ErrorContext(ctx, "artifacts: key lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact")
			return
		}
	}
	if existing == nil && !canCreateIn(ctx, s.host, scope) {
		s.logCreateRefusal(r, scope)
		if !s.host.Permits(ctx, scope, PermissionCreate) && s.writeMissingScope(w, r) {
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish artifacts in this scope")
		return
	}
	if attFile.Size > limits.MaxFileBytes {
		writeTooLarge(w, limits.MaxFileBytes)
		return
	}

	srcMsgID := ""
	if att.ScopeKind == ScopeKindMessage {
		srcMsgID = att.ScopeRef
	}
	versionID := uuid.NewString()
	baseFile := File{
		VersionID: versionID,
		Path:      attFile.Path,
		Size:      attFile.Size,
		SHA256:    attFile.SHA256,
		MediaType: attFile.MediaType,
	}
	files := []File{baseFile}
	totalBytes := attFile.Size
	var warnings []string

	// Helper to append to an existing keyed artifact, checking write access,
	// expiry, and open reviews before committing.
	appendToExisting := func(target *Artifact) {
		access, werr := s.canWriteErr(ctx, b, target)
		if werr != nil {
			writeGrantsReadFailed(w, r, werr)
			return
		}
		if access == accessNone {
			s.logWriteRefusal(r, target)
			if !s.host.Permits(ctx, target.ScopeRef, PermissionCreate) && s.writeMissingScope(w, r) {
				return
			}
			writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish versions of this artifact")
			return
		}
		if refuseKind(w, access, VersionKindPublish) {
			s.logRefusal(r, logActionWrite, logReasonReviewOnly, target)
			return
		}
		if explicitExpiry {
			if target.OwnerKind != kind || target.OwnerRef != ref {
				writeError(w, http.StatusForbidden, "forbidden", "only the artifact owner may set expiry at publish time")
				return
			}
			if kind == PrincipalKindAgent && limits.DefaultRetention > 0 && expiryLengthens(target.ExpiresAt, expiresAt) {
				writeError(w, http.StatusBadRequest, "bad_request", "cannot lengthen an artifact's expiry when the hub has a default retention")
				return
			}
		}
		openWarn, ok := s.checkOpenReviews(w, r, b, target, req.Force)
		if !ok {
			return
		}
		allWarnings := append(append([]string(nil), openWarn...), warnings...)
		priorWasReview := false
		if target.OwnerKind == PrincipalKindUser && target.CurrentSeq > 0 && s.inboxHintsActive(ctx) {
			if prior, perr := b.store.GetVersion(ctx, target.ID, target.CurrentSeq); perr == nil && prior.Kind == VersionKindReview {
				priorWasReview = true
			}
		}
		v := &Version{
			ID:                 versionID,
			ArtifactID:         target.ID,
			Kind:               VersionKindPublish,
			EntryPath:          attFile.Path,
			Note:               strings.TrimSpace(req.Note),
			TotalBytes:         totalBytes,
			FileCount:          len(files),
			CreatedByKind:      kind,
			CreatedByRef:       ref,
			CreatedAt:          now,
			State:              VersionStateReady,
			SourceAttachmentID: att.ID,
			SourceMessageID:    srcMsgID,
		}
		updated, aerr := b.store.AppendPublishedVersion(ctx, v, files)
		switch {
		case errors.Is(aerr, ErrAttachmentNotShareable):
			writeAttachmentNotShareable(w)
			return
		case errors.Is(aerr, ErrNotFound):
			writeNotFound(w)
			return
		case aerr != nil:
			slog.ErrorContext(ctx, "artifacts: append published version failed", "error", aerr)
			s.logVersionError(r, target, v.Seq, logReasonStoreError)
			writeError(w, http.StatusInternalServerError, "internal", "could not record the version")
			return
		}
		if explicitExpiry {
			expUpdated, eerr := s.applyPublishExpiry(ctx, r, b, updated, expiresAt, kind, ref)
			if eerr != nil {
				slog.ErrorContext(ctx, "artifacts: set publish expiry failed", "error", eerr)
				writeError(w, http.StatusInternalServerError, "internal", "could not set the artifact expiry")
				return
			}
			updated = expUpdated
		}
		s.logVersion(r, logActionVersionFinalized, updated, v.Seq, access)
		if priorWasReview && updated.CurrentSeq == v.Seq && updated.OwnerKind == PrincipalKindUser {
			s.notifyInboxChanged(ctx, updated.OwnerRef)
		}
		vi := versionInfo(v, files)
		if v.SourceMessageID != "" {
			s.populateSourceMessageRoute(ctx, vi, v.SourceMessageID)
		}
		writeJSON(w, http.StatusCreated, ArtifactResponse{Artifact: artifactInfo(updated), Version: vi, Warnings: allWarnings})
	}

	// Run Markdown remote-image fetching / HTML remote-image warning before
	// committing (for an existing artifact, check write access first so a
	// refused caller triggers no remote fetches).
	if existing != nil {
		access, werr := s.canWriteErr(ctx, b, existing)
		if werr != nil {
			writeGrantsReadFailed(w, r, werr)
			return
		}
		if access == accessNone {
			s.logWriteRefusal(r, existing)
			if !s.host.Permits(ctx, existing.ScopeRef, PermissionCreate) && s.writeMissingScope(w, r) {
				return
			}
			writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish versions of this artifact")
			return
		}
	}
	extra, warn := s.attachmentPublishExtras(w, r, b, versionID, &baseFile)
	warnings = warn
	for _, rf := range extra {
		files = append(files, rf)
		totalBytes += rf.Size
	}

	if existing != nil {
		appendToExisting(existing)
		return
	}

	if title == "" {
		title = attFile.Path
	}
	a := &Artifact{
		ID:         uuid.NewString(),
		ScopeKind:  ScopeKindProject,
		ScopeRef:   scope,
		OwnerKind:  kind,
		OwnerRef:   ref,
		Key:        key,
		Title:      title,
		CurrentSeq: 1,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  retentionExpiry(limits, now),
	}
	a.SponsorRef = s.sponsorFor(ctx, kind, ref, a.ScopeKind)
	v := &Version{
		ID:                 versionID,
		ArtifactID:         a.ID,
		Seq:                1,
		Kind:               VersionKindPublish,
		EntryPath:          attFile.Path,
		Note:               strings.TrimSpace(req.Note),
		TotalBytes:         totalBytes,
		FileCount:          len(files),
		CreatedByKind:      kind,
		CreatedByRef:       ref,
		CreatedAt:          now,
		State:              VersionStateReady,
		SourceAttachmentID: att.ID,
		SourceMessageID:    srcMsgID,
	}
	g := Grant{
		ID:           uuid.NewString(),
		ArtifactID:   a.ID,
		SubjectKind:  SubjectScope,
		SubjectRef:   scope,
		Permission:   GrantRead,
		CreatedByRef: PrincipalRef(kind, ref),
		CreatedAt:    now,
	}
	grants := []Grant{g}
	if sg, ok := s.sponsorReviewGrant(r, a, now); ok {
		grants = append(grants, sg)
	}
	err = b.store.CreatePublished(ctx, a, v, files, grants)
	if errors.Is(err, ErrConflict) {
		// Another publish took the key first: append to its artifact.
		if raced, lookErr := b.store.GetArtifactByKey(ctx, ScopeKindProject, scope, kind, ref, key); lookErr == nil {
			appendToExisting(raced)
			return
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: publish from attachment failed", "error", err)
		s.logVersionError(r, a, 1, logReasonStoreError)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the artifact")
		return
	}
	if explicitExpiry {
		updated, eerr := s.applyPublishExpiry(ctx, r, b, a, expiresAt, kind, ref)
		if eerr != nil {
			slog.ErrorContext(ctx, "artifacts: set publish expiry failed", "error", eerr)
			writeError(w, http.StatusInternalServerError, "internal", "could not set the artifact expiry")
			return
		}
		a = updated
	}
	s.logAllowed(r, logEntry{action: logActionPublished, artifact: a, seq: 1, authority: logAuthorityOwner})
	s.logSponsorReviewGrants(r, a, grants)
	vi := versionInfo(v, files)
	if v.SourceMessageID != "" {
		s.populateSourceMessageRoute(ctx, vi, v.SourceMessageID)
	}
	writeJSON(w, http.StatusCreated, ArtifactResponse{Artifact: artifactInfo(a), Version: vi, Warnings: warnings})
}

// attachmentPublishExtras runs remote-image fetching for Markdown entries and
// remote-image warnings for HTML entries when publishing from an existing
// attachment blob.
func (s *Service) attachmentPublishExtras(w http.ResponseWriter, r *http.Request, b backend, versionID string, entry *File) ([]File, []string) {
	ctx := r.Context()
	if entry.MediaType == mediaTypeHTML {
		if window, _, err := s.entryWindow(r, b, entry); err == nil && htmlHasRemoteImages(window) {
			return nil, []string{WarnHTMLRemoteImages}
		}
		return nil, nil
	}
	if entry.MediaType != mediaTypeMarkdown {
		return nil, nil
	}
	rc, _, err := b.blobs.Download(ctx, BlobPath(b.hubID, entry.SHA256))
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: read markdown attachment failed", "error", err)
		return nil, nil
	}
	window, err := readWindow(rc, entry.Size)
	_ = rc.Close()
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: read markdown attachment failed", "error", err)
		return nil, nil
	}
	return s.remoteImages(ctx, w, b, versionID, window, entry.Size > imageScanWindow, versionUsage{files: 1, bytes: entry.Size})
}

// CodeMissingScope is the error code of a 403 answered to a publish whose
// credential lacks a scope publishing needs. The error's details carry
// every missing scope under "scopes" and the first of them under "scope"
// (the only key older hubs send).
const CodeMissingScope = "missing_scope"

// ScopesExplainer is an optional extension of Host, preferred over
// ScopeExplainer when the host implements both: it names every scope the
// caller lacks rather than only the first (ptone/scion#4571). The same
// rules apply: consulted only on the publish path, and a pure description
// that never grants anything.
type ScopesExplainer interface {
	// MissingScopes reports the names of the credential scopes the caller
	// of ctx lacks for permission (one of the Permission constants), or
	// none under the conditions where ScopeExplainer.MissingScope reports
	// "".
	MissingScopes(ctx context.Context, permission string) []string
}

// missingScopes asks the host which scopes the caller lacks for
// permission, through ScopesExplainer when the host implements it and
// ScopeExplainer otherwise.
func (s *Service) missingScopes(ctx context.Context, permission string) []string {
	if ex, ok := s.host.(ScopesExplainer); ok {
		var out []string
		for _, scope := range ex.MissingScopes(ctx, permission) {
			if scope != "" && !slices.Contains(out, scope) {
				out = append(out, scope)
			}
		}
		return out
	}
	if ex, ok := s.host.(ScopeExplainer); ok {
		if scope := ex.MissingScope(ctx, permission); scope != "" {
			return []string{scope}
		}
	}
	return nil
}

// writeMissingScope answers 403 missing_scope when the host reports that
// the caller's credential lacks scopes for publishing. It reports whether
// it wrote the response. Only the publish path calls it: publishing names
// no existing artifact, so the answer describes the caller's own
// credential and nothing else.
func (s *Service) writeMissingScope(w http.ResponseWriter, r *http.Request) bool {
	scopes := s.missingScopes(r.Context(), PermissionCreate)
	if len(scopes) == 0 {
		return false
	}
	noun := "scope"
	if len(scopes) > 1 {
		noun = "scopes"
	}
	writeJSON(w, http.StatusForbidden, errorResponse{Error: errorBody{
		Code:    CodeMissingScope,
		Message: "the credential does not carry the " + strings.Join(scopes, " and ") + " " + noun + " needed to publish artifacts",
		Details: map[string]any{"scope": scopes[0], "scopes": scopes},
	}})
	return true
}

// putBlob stores the spooled body at its content address (see storeBlob).
func (s *Service) putBlob(ctx context.Context, b backend, sp *spooled, mediaType string) error {
	return storeBlob(ctx, b, sp.digest, mediaType, func() (io.Reader, error) {
		if _, err := sp.file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind spool: %w", err)
		}
		return sp.file, nil
	})
}

// blobClock is the time blob touches are recorded at. A variable so tests
// can run publishes and sweeps on one simulated clock.
var blobClock = time.Now

// Blob write retries: a blob write is idempotent (the object always gets
// the same bytes), so a failed write is retried, at most blobWriteAttempts
// times in all and within blobWriteDeadline, with a growing pause; the
// last error is returned. Variables so tests can shorten them.
var (
	blobWriteAttempts = 5
	blobWriteDeadline = 60 * time.Second
	blobWriteBackoff  = 200 * time.Millisecond
)

// storeBlob stores the bytes open yields at digest's content address. It
// is the only way bytes are written. It touches the blob first (waiting
// for a sweep that holds the blob's state, and keeping the sweep off the
// blob for the grace period), then always uploads, overwriting any object
// already there: the write never relies on an object that is already
// there, so a delete the sweep issued earlier (which names the generation
// it saw) can never remove the bytes this write stores. There is still
// one object per digest. open is called again for each attempt.
func storeBlob(ctx context.Context, b backend, digest, mediaType string, open func() (io.Reader, error)) error {
	if err := b.store.TouchBlob(ctx, digest, blobClock()); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, blobWriteDeadline)
	defer cancel()
	pause := blobWriteBackoff
	var err error
	for attempt := 1; attempt <= blobWriteAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("upload blob: %w (after %d attempts)", err, attempt-1)
			case <-time.After(pause):
			}
			pause *= 2
		}
		var body io.Reader
		if body, err = open(); err != nil {
			return err
		}
		if _, err = b.blobs.Upload(ctx, BlobPath(b.hubID, digest), body, storage.UploadOptions{ContentType: mediaType, Idempotent: true}); err == nil {
			return nil
		}
	}
	return fmt.Errorf("upload blob: %w (after %d attempts)", err, blobWriteAttempts)
}

func writeTooLarge(w http.ResponseWriter, limit int64) {
	writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
		fmt.Sprintf("file exceeds the %d byte limit", limit))
}

var errTooLarge = errors.New("body exceeds the limit")

// spooled is a request body copied to a temporary file, with its size,
// digest and first bytes (for media type sniffing).
type spooled struct {
	file   *os.File
	size   int64
	digest string
	head   []byte
}

// window returns the spooled body's first imageScanWindow bytes and
// whether the body is longer.
func (s *spooled) window() (string, bool, error) {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return "", false, err
	}
	text, err := readWindow(s.file, s.size)
	return text, s.size > imageScanWindow, err
}

// readWindow reads at most imageScanWindow bytes of r, a body of size
// bytes, into one string.
func readWindow(r io.Reader, size int64) (string, error) {
	var b strings.Builder
	b.Grow(int(min(max(size, 0), imageScanWindow)))
	_, err := io.Copy(&b, io.LimitReader(r, imageScanWindow))
	return b.String(), err
}

func (s *spooled) Close() {
	_ = s.file.Close()
	_ = os.Remove(s.file.Name())
}

// spoolBody copies body to a temporary file while hashing it, reading at
// most limit+1 bytes so an oversized body is detected without being stored.
func spoolBody(body io.Reader, limit int64) (*spooled, error) {
	f, err := os.CreateTemp("", "scion-artifact-*")
	if err != nil {
		return nil, err
	}
	sp := &spooled{file: f}
	h := sha256.New()
	head := &headBuffer{limit: 512}
	n, err := io.Copy(io.MultiWriter(f, h, head), io.LimitReader(body, limit+1))
	if err == nil && n > limit {
		err = errTooLarge
	}
	if err != nil {
		sp.Close()
		return nil, err
	}
	sp.size, sp.digest, sp.head = n, hex.EncodeToString(h.Sum(nil)), head.buf
	return sp, nil
}

// headBuffer keeps the first limit bytes written to it.
type headBuffer struct {
	buf   []byte
	limit int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.limit - len(h.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		h.buf = append(h.buf, p[:room]...)
	}
	return len(p), nil
}
