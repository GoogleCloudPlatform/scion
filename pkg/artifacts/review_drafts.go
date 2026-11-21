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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Durable review drafts (ptone/scion#7116, ptone/scion#8470). Each reviewer
// principal may hold at most one saved draft per artifact, addressed strictly
// by (artifact, caller principal) with optimistic integer revisions and no
// administrator, steward, owner or publisher override. Draft writes skip
// checkReview so in-progress work (including zero marks or edits outside
// marks) is never stranded, and draft writes never notify the artifact owner.

// FeatureReviewDrafts is the server-side experiment that gates the
// review-draft routes. It equals experiments.ArtifactsReviewDrafts.
const FeatureReviewDrafts = "hub.artifacts_review_drafts"

const (
	// MaxDraftBodyBytes caps each of body and carried.body on a saved review
	// draft (1 MiB).
	MaxDraftBodyBytes = 1 << 20
	// MaxDraftsPerPrincipal caps the saved review drafts one principal may
	// hold across the hub at once.
	MaxDraftsPerPrincipal = 200
	// DraftTTL is how long an untouched review draft is kept after its last
	// save (updated_at) before the maintenance reaper removes it.
	DraftTTL = 30 * 24 * time.Hour

	// maxReviewDraftRequestBytes caps the raw HTTP request body of PUT
	// review-draft before JSON decoding (body + carried.body + JSON envelope).
	maxReviewDraftRequestBytes int64 = 2*MaxDraftBodyBytes + 64<<10
)

// Error codes of the review-draft routes.
const (
	// CodeDraftConflict refuses a conditional PUT or DELETE whose expected
	// revision does not match the stored row. Its details carry "rev",
	// "savedAt" and "base", and never carry the draft body or note.
	CodeDraftConflict = "draft_conflict"
	// CodeDraftTooLarge refuses a draft PUT whose body or carried.body (or
	// request envelope) exceeds MaxDraftBodyBytes. Its details carry "limit".
	CodeDraftTooLarge = "draft_too_large"
	// CodeTooManyDrafts refuses a draft insert when the caller already holds
	// MaxDraftsPerPrincipal saved drafts.
	CodeTooManyDrafts = "too_many_drafts"
)

// reviewDraftSegment is the path segment of the per-artifact review-draft routes.
const reviewDraftSegment = "review-draft"

// ReviewDraftResponse is the body of GET /api/v1/artifacts/{id}/review-draft.
// Draft is null when the caller has no saved draft on the artifact, keeping
// "no draft" distinct from 404 "no access".
type ReviewDraftResponse struct {
	Draft      *ReviewDraftInfo `json:"draft"`
	CurrentSeq int              `json:"currentSeq"`
}

// ReviewDraftInfo describes the caller's saved draft review.
type ReviewDraftInfo struct {
	Rev       int               `json:"rev"`
	Base      int               `json:"base"`
	Entry     string            `json:"entry"`
	Body      string            `json:"body"`
	Note      string            `json:"note"`
	Carried   *CarriedDraftInfo `json:"carried"`
	SavedAt   time.Time         `json:"savedAt"`
	CreatedAt time.Time         `json:"createdAt"`
}

// CarriedDraftInfo holds the read-only earlier review text kept when a review
// draft was moved from an older base version to a newer one.
type CarriedDraftInfo struct {
	Base int    `json:"base"`
	Body string `json:"body"`
}

// PutReviewDraftResponse answers PUT /api/v1/artifacts/{id}/review-draft.
type PutReviewDraftResponse struct {
	Rev        int       `json:"rev"`
	SavedAt    time.Time `json:"savedAt"`
	CurrentSeq int       `json:"currentSeq"`
}

// putReviewDraftRequest is the decoded JSON body of PUT
// /api/v1/artifacts/{id}/review-draft. Pointer fields distinguish omitted/null
// required fields from valid zero values (rev=0, body="").
type putReviewDraftRequest struct {
	Rev           *int               `json:"rev"`
	Base          int                `json:"base"`
	Entry         string             `json:"entry"`
	Body          *string            `json:"body"`
	Note          string             `json:"note"`
	Carried       *carriedDraftInput `json:"carried"`
	ClientSavedAt *string            `json:"clientSavedAt"`
}

type carriedDraftInput struct {
	Base int     `json:"base"`
	Body *string `json:"body"`
}

// ReapReviewDrafts runs one TTL reap pass (Store.ReapReviewDrafts at
// now - DraftTTL) and writes a count-only summary entry to the artifact
// decision log when any drafts were removed or an error occurred. It runs
// whether or not FeatureReviewDrafts is enabled.
func ReapReviewDrafts(ctx context.Context, st Store, now time.Time, limit int, log *slog.Logger) (int, error) {
	if log == nil {
		log = discardLogger
	}
	reaped, err := st.ReapReviewDrafts(ctx, now.Add(-DraftTTL), limit)
	if reaped > 0 || err != nil {
		outcome, level := logOutcomeAllowed, slog.LevelInfo
		attrs := make([]slog.Attr, 0, 7)
		if err != nil {
			outcome, level = logOutcomeError, slog.LevelWarn
			attrs = append(attrs, slog.String(logKeyReason, logReasonStoreError))
		}
		attrs = append(attrs,
			slog.String(logKeyAction, logActionDraftReapPass),
			slog.String(logKeyOutcome, outcome),
			slog.String(logKeyPrincipalKind, retentionActor.Kind),
			slog.String(logKeyPrincipalID, retentionActor.Ref),
			slog.String(logKeyAuthority, retentionActor.Authority),
			slog.Int(logKeyCount, reaped),
		)
		log.LogAttrs(ctx, level, "artifacts: "+logActionDraftReapPass, attrs...)
	}
	return reaped, err
}

// handleReviewDraft implements GET, PUT and DELETE
// /api/v1/artifacts/{id}/review-draft in the exact C2 authorization order:
//  0. FeatureEnabled (off -> 404), method check (405), Principal (none -> 404)
//  1. readableArtifact (unreadable -> 404)
//  2. canWriteErr (no review/write access -> 403)
//  3. PUT only: CurrentSeq > 0 (409 nothing_to_review)
//  4. PUT and DELETE only: LimitDraftWrite per-principal rate limit (429)
//  5. PUT only: http.MaxBytesReader body decode and field validation
//
// Only authorized principals create limiter entries, no body byte is read
// before authorization and rate limiting, and the row is always addressed by
// (id, caller principal) with no override for any role.
func (s *Service) handleReviewDraft(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	if !s.featureEnabled(ctx, FeatureReviewDrafts) {
		writeNotFound(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
		return
	}
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok {
		writeNotFound(w)
		return
	}
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	access, err := s.canWriteErr(ctx, b, a)
	if err != nil {
		writeGrantsReadFailed(w, r, err)
		return
	}
	if access == accessNone {
		s.logWriteRefusal(r, a)
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish versions of this artifact")
		return
	}
	if r.Method == http.MethodPut && a.CurrentSeq == 0 {
		writeError(w, http.StatusConflict, CodeNothingToReview, "a review needs an artifact with a published version")
		return
	}
	if r.Method != http.MethodGet && !s.chargePrincipal(w, LimitDraftWrite, kind, ref) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetReviewDraft(w, r, b, a, kind, ref)
	case http.MethodDelete:
		s.handleDeleteReviewDraft(w, r, b, a, kind, ref, access)
	case http.MethodPut:
		s.handlePutReviewDraft(w, r, b, a, kind, ref)
	}
}

func (s *Service) handleGetReviewDraft(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, kind, ref string) {
	ctx := r.Context()
	d, err := b.store.GetReviewDraft(ctx, a.ID, kind, ref)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusOK, ReviewDraftResponse{Draft: nil, CurrentSeq: a.CurrentSeq})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: get review draft failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the review draft")
		return
	}
	var carried *CarriedDraftInfo
	if d.CarriedBaseSeq != nil && d.CarriedBody != nil {
		carried = &CarriedDraftInfo{Base: *d.CarriedBaseSeq, Body: *d.CarriedBody}
	}
	writeJSON(w, http.StatusOK, ReviewDraftResponse{
		Draft: &ReviewDraftInfo{
			Rev:       d.Rev,
			Base:      d.BaseSeq,
			Entry:     d.EntryPath,
			Body:      d.Body,
			Note:      d.Note,
			Carried:   carried,
			SavedAt:   d.UpdatedAt,
			CreatedAt: d.CreatedAt,
		},
		CurrentSeq: a.CurrentSeq,
	})
}

func (s *Service) handleDeleteReviewDraft(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, kind, ref string, access writeAccess) {
	ctx := r.Context()
	expectRev := 0
	if q := r.URL.Query(); q.Has("rev") {
		vals := q["rev"]
		if len(vals) != 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "rev must be a positive integer")
			return
		}
		n, ok := parseSeq(vals[0])
		if !ok {
			writeError(w, http.StatusBadRequest, "bad_request", "rev must be a positive integer")
			return
		}
		expectRev = n
	}
	deleted, err := b.store.DeleteReviewDraft(ctx, a.ID, kind, ref, expectRev)
	var conflict *DraftConflictError
	switch {
	case errors.As(err, &conflict):
		writeDraftConflict(w, conflict)
		return
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: delete review draft failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not delete the review draft")
		return
	}
	if deleted {
		s.logVersion(r, logActionReviewDraftDeleted, a, 0, access)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handlePutReviewDraft(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, kind, ref string) {
	ctx := r.Context()
	req, clientSavedAt, ok := decodePutReviewDraft(w, r)
	if !ok {
		return
	}
	if req.Base < 1 || req.Base > a.CurrentSeq {
		writeError(w, http.StatusBadRequest, "bad_request", "base must be a published version of this artifact")
		return
	}
	baseVer, err := b.store.GetVersion(ctx, a.ID, req.Base)
	if errors.Is(err, ErrNotFound) || (err == nil && baseVer.State != VersionStateReady) {
		writeError(w, http.StatusBadRequest, "bad_request", "base must be a published version of this artifact")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: get version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return
	}
	if req.Entry == "" || req.Entry != baseVer.EntryPath {
		writeError(w, http.StatusBadRequest, "bad_request", "entry must match the base version's entry path")
		return
	}

	var carriedBase *int
	var carriedBody *string
	if req.Carried != nil {
		if req.Carried.Base < 1 || req.Carried.Base >= req.Base {
			writeError(w, http.StatusBadRequest, "bad_request", "carried.base must be a published version older than base")
			return
		}
		cv, err := b.store.GetVersion(ctx, a.ID, req.Carried.Base)
		if errors.Is(err, ErrNotFound) || (err == nil && cv.State != VersionStateReady) {
			writeError(w, http.StatusBadRequest, "bad_request", "carried.base must be a published version older than base")
			return
		}
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: get version failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
			return
		}
		cb := req.Carried.Base
		cs := *req.Carried.Body
		carriedBase = &cb
		carriedBody = &cs
	}

	stored, err := b.store.PutReviewDraft(ctx, ReviewDraft{
		ArtifactID:     a.ID,
		PrincipalKind:  kind,
		PrincipalRef:   ref,
		BaseSeq:        req.Base,
		EntryPath:      req.Entry,
		Body:           *req.Body,
		Note:           req.Note,
		CarriedBaseSeq: carriedBase,
		CarriedBody:    carriedBody,
		ClientSavedAt:  clientSavedAt,
	}, *req.Rev, time.Now().UTC(), MaxDraftsPerPrincipal)
	var conflict *DraftConflictError
	switch {
	case errors.As(err, &conflict):
		writeDraftConflict(w, conflict)
		return
	case errors.Is(err, ErrTooManyDrafts):
		writeError(w, http.StatusTooManyRequests, CodeTooManyDrafts,
			fmt.Sprintf("you have %d saved draft reviews; submit or discard some", MaxDraftsPerPrincipal))
		return
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: save review draft failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not save the review draft")
		return
	}
	writeJSON(w, http.StatusOK, PutReviewDraftResponse{
		Rev:        stored.Rev,
		SavedAt:    stored.UpdatedAt,
		CurrentSeq: a.CurrentSeq,
	})
}

// decodePutReviewDraft reads and validates a PUT review-draft payload through
// http.MaxBytesReader. Error responses use fixed static messages so no body,
// carried body, note or field fragment is ever echoed or logged (C7).
func decodePutReviewDraft(w http.ResponseWriter, r *http.Request) (putReviewDraftRequest, *time.Time, bool) {
	var req putReviewDraftRequest
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReviewDraftRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeDraftTooLarge(w)
			return req, nil, false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return req, nil, false
	}
	// Check raw UTF-8 and NUL before json.Decoder, which silently substitutes
	// U+FFFD for invalid UTF-8 bytes inside JSON strings.
	if !utf8.Valid(rawBody) || bytes.IndexByte(rawBody, 0) >= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "the request body must be valid UTF-8 without NUL bytes")
		return req, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request",
			`the body must be {"rev": <int>, "base": <version>, "entry": <path>, "body": <text>, "note": <text>}`)
		return req, nil, false
	}
	if req.Rev == nil || *req.Rev < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "rev must be a non-negative integer")
		return req, nil, false
	}
	if req.Body == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body is required")
		return req, nil, false
	}
	if len(*req.Body) > MaxDraftBodyBytes {
		writeDraftTooLarge(w)
		return req, nil, false
	}
	if !utf8.ValidString(*req.Body) || strings.IndexByte(*req.Body, 0) >= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "body must be valid UTF-8 without NUL bytes")
		return req, nil, false
	}
	if req.Carried != nil {
		if req.Carried.Body == nil {
			writeError(w, http.StatusBadRequest, "bad_request", "carried.body is required when carried is set")
			return req, nil, false
		}
		if len(*req.Carried.Body) > MaxDraftBodyBytes {
			writeDraftTooLarge(w)
			return req, nil, false
		}
		if !utf8.ValidString(*req.Carried.Body) || strings.IndexByte(*req.Carried.Body, 0) >= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "carried.body must be valid UTF-8 without NUL bytes")
			return req, nil, false
		}
	}
	if !utf8.ValidString(req.Note) || strings.IndexByte(req.Note, 0) >= 0 || utf8.RuneCountInString(req.Note) > maxNoteRunes {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("note must be valid UTF-8 of at most %d characters", maxNoteRunes))
		return req, nil, false
	}
	if !utf8.ValidString(req.Entry) || strings.IndexByte(req.Entry, 0) >= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "entry must be valid UTF-8 without NUL bytes")
		return req, nil, false
	}
	var clientSaved *time.Time
	if req.ClientSavedAt != nil {
		parsed, err := time.Parse(time.RFC3339Nano, *req.ClientSavedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "clientSavedAt must be an RFC 3339 timestamp")
			return req, nil, false
		}
		utc := parsed.UTC()
		clientSaved = &utc
	}
	return req, clientSaved, true
}

func writeDraftTooLarge(w http.ResponseWriter) {
	writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: errorBody{
		Code:    CodeDraftTooLarge,
		Message: fmt.Sprintf("the draft review exceeds the %d byte limit", MaxDraftBodyBytes),
		Details: map[string]any{"limit": MaxDraftBodyBytes},
	}})
}

func writeDraftConflict(w http.ResponseWriter, ce *DraftConflictError) {
	var savedAt any
	if !ce.SavedAt.IsZero() {
		savedAt = ce.SavedAt
	}
	writeJSON(w, http.StatusConflict, errorResponse{Error: errorBody{
		Code:    CodeDraftConflict,
		Message: "this draft review was changed elsewhere",
		Details: map[string]any{
			"rev":     ce.Rev,
			"savedAt": savedAt,
			"base":    ce.BaseSeq,
		},
	}})
}
