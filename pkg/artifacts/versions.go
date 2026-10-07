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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Two-step publish (design D9): a publisher creates a pending version from
// a manifest, uploads each file the response lists, then finalizes. The
// version becomes ready, and the artifact's current version, only at
// finalize, after every file of the manifest has arrived.

const (
	// PendingVersionTTL is how long a pending version may wait for its
	// files and its finalize before it is reaped.
	PendingVersionTTL = 24 * time.Hour

	// MaxPendingVersions is how many pending versions one artifact may
	// have at a time.
	MaxPendingVersions = 4

	// maxManifestBytes caps the JSON body of a create request.
	maxManifestBytes = 1 << 20

	maxKeyBytes   = 256
	maxNoteRunes  = 2000
	maxListedPath = 20
)

// CodeIncomplete is the error code of a finalize refused because files of
// the manifest have not been uploaded. The error's details list them under
// "missing".
const CodeIncomplete = "incomplete"

// handleCreate implements POST /api/v1/artifacts with a JSON manifest. It
// creates an artifact with a pending first version or, when the caller
// already owns an artifact with the requested key in the scope, appends a
// pending version to it.
func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, ok := decodeManifest(w, r)
	if !ok {
		return
	}
	kind, ref, home, ok := s.host.Principal(ctx)
	if !ok {
		if s.writeMissingScope(w, r) {
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = home
	}
	if scope == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "scope is required")
		return
	}
	if !s.host.Permits(ctx, scope, PermissionCreate) && s.writeMissingScope(w, r) {
		return
	}
	if !s.host.Permits(ctx, scope, PermissionCreate) || !s.host.Authorize(ctx, scope, PermissionCreate) {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish artifacts in this scope")
		return
	}
	key := req.Key
	if err := validKey(key); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	b, ok := s.backend()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact storage is not configured")
		return
	}
	if !validateManifest(w, req, b.currentLimits(ctx)) {
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = path.Base(req.Entry)
	}
	if !utf8.ValidString(title) || utf8.RuneCountInString(title) > maxTitleRunes {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("title must be valid UTF-8 of at most %d characters", maxTitleRunes))
		return
	}

	if key != "" {
		existing, err := b.store.GetArtifactByKey(ctx, ScopeKindProject, scope, kind, ref, key)
		if err == nil {
			s.appendVersion(w, r, b, existing, req)
			return
		}
		if !errors.Is(err, ErrNotFound) {
			slog.ErrorContext(ctx, "artifacts: key lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact")
			return
		}
	}

	now := time.Now().UTC()
	a := &Artifact{
		ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: scope,
		OwnerKind: kind, OwnerRef: ref, Key: key, Title: title, CreatedAt: now, UpdatedAt: now,
	}
	v, files := pendingVersion(a.ID, req, kind, ref, now, nil)
	v.Seq = 1
	g := Grant{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: scope,
		Permission: GrantRead, CreatedByRef: PrincipalRef(kind, ref), CreatedAt: now,
	}
	err := b.store.CreatePending(ctx, a, v, files, []Grant{g})
	if errors.Is(err, ErrConflict) {
		// Another publish took the key first: append to its artifact.
		existing, lookErr := b.store.GetArtifactByKey(ctx, ScopeKindProject, scope, kind, ref, key)
		if lookErr == nil {
			s.appendVersion(w, r, b, existing, req)
			return
		}
		err = lookErr
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: create pending version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the artifact")
		return
	}
	writePending(w, a, v, files)
}

// handleCreateVersion implements POST /api/v1/artifacts/{id}/versions.
func (s *Service) handleCreateVersion(w http.ResponseWriter, r *http.Request, id string) {
	req, ok := decodeManifest(w, r)
	if !ok {
		return
	}
	if req.Title != "" || req.Key != "" || req.Scope != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "title, key and scope apply only when creating an artifact")
		return
	}
	b, a, ok := s.writableArtifact(w, r, id)
	if !ok {
		return
	}
	if !validateManifest(w, req, b.currentLimits(r.Context())) {
		return
	}
	s.appendVersion(w, r, b, a, req)
}

// appendVersion adds a pending version to a, which the caller may write.
// Files identical (same path and digest) to the current version's need no
// upload.
func (s *Service) appendVersion(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, req *CreateVersionRequest) {
	ctx := r.Context()
	kind, ref, _, _ := s.host.Principal(ctx)
	if !s.canWrite(ctx, b, a) {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish versions of this artifact")
		return
	}
	var current map[string]File
	if a.CurrentSeq > 0 {
		if cv, err := b.store.GetVersion(ctx, a.ID, a.CurrentSeq); err == nil {
			if files, err := b.store.ListFiles(ctx, cv.ID); err == nil {
				current = make(map[string]File, len(files))
				for _, f := range files {
					if f.Origin == FileOriginUpload && f.SHA256 != "" {
						current[f.Path] = f
					}
				}
			}
		}
	}
	v, files := pendingVersion(a.ID, req, kind, ref, time.Now().UTC(), current)
	err := b.store.CreateVersion(ctx, v, files, MaxPendingVersions)
	switch {
	case errors.Is(err, ErrTooManyPending):
		writeError(w, http.StatusConflict, "too_many_pending",
			fmt.Sprintf("the artifact already has %d versions waiting to be finalized", MaxPendingVersions))
		return
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: create version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the version")
		return
	}
	writePending(w, a, v, files)
}

// pendingVersion builds a pending version and its manifest rows. A file
// that matches one in current (same path and digest) is recorded as
// already received.
func pendingVersion(artifactID string, req *CreateVersionRequest, kind, ref string, now time.Time, current map[string]File) (*Version, []File) {
	v := &Version{
		ID: uuid.NewString(), ArtifactID: artifactID, Kind: VersionKindPublish, EntryPath: req.Entry,
		Note: strings.TrimSpace(req.Note), FileCount: len(req.Files), CreatedByKind: kind, CreatedByRef: ref,
		CreatedAt: now, State: VersionStatePending,
	}
	files := make([]File, 0, len(req.Files))
	for _, m := range req.Files {
		v.TotalBytes += m.Size
		f := File{
			VersionID: v.ID, Path: m.Path, Size: m.Size, SHA256: m.SHA256,
			MediaType: manifestMediaType(m.Path, m.MediaType), Pending: true,
		}
		if prev, ok := current[m.Path]; ok && prev.SHA256 == m.SHA256 && prev.Size == m.Size {
			f.MediaType, f.Pending = prev.MediaType, false
		}
		files = append(files, f)
	}
	return v, files
}

func writePending(w http.ResponseWriter, a *Artifact, v *Version, files []File) {
	resp := PendingVersionResponse{Artifact: artifactInfo(a), Version: versionInfo(v, files), Upload: UploadInfo{Required: []string{}}}
	for _, f := range files {
		if f.Pending {
			resp.Upload.Required = append(resp.Upload.Required, f.Path)
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

// decodeManifest reads and decodes the JSON body of a create request.
func decodeManifest(w http.ResponseWriter, r *http.Request) (*CreateVersionRequest, bool) {
	if r.ContentLength > maxManifestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the manifest is too large")
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return nil, false
	}
	if len(body) > maxManifestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the manifest is too large")
		return nil, false
	}
	var req CreateVersionRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be a JSON version manifest")
		return nil, false
	}
	return &req, true
}

// validKey checks a publisher-chosen key: optional, valid UTF-8 without
// control characters, at most maxKeyBytes.
func validKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > maxKeyBytes || !utf8.ValidString(key) || strings.TrimSpace(key) != key {
		return fmt.Errorf("key must be valid UTF-8 of at most %d bytes without surrounding spaces", maxKeyBytes)
	}
	for _, c := range key {
		if c < 0x20 || c == 0x7f {
			return errors.New("key must not contain control characters")
		}
	}
	return nil
}

// validateManifest checks a manifest against the path rules and limits and
// writes a 400 or 413 when it fails.
func validateManifest(w http.ResponseWriter, req *CreateVersionRequest, l Limits) bool {
	bad := func(msg string) bool {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return false
	}
	switch req.Kind {
	case "", VersionKindPublish:
	case VersionKindReview:
		return bad("review versions are not supported yet")
	default:
		return bad("kind must be publish")
	}
	if !utf8.ValidString(req.Note) || utf8.RuneCountInString(req.Note) > maxNoteRunes {
		return bad(fmt.Sprintf("note must be valid UTF-8 of at most %d characters", maxNoteRunes))
	}
	if len(req.Files) == 0 {
		return bad("the manifest lists no files")
	}
	if len(req.Files) > l.MaxFiles {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("a version may hold at most %d files", l.MaxFiles))
		return false
	}
	paths := make(map[string]bool, len(req.Files))
	var total int64
	for i := range req.Files {
		m := &req.Files[i]
		if _, err := cleanFilePath(m.Path); err != nil {
			return bad(fmt.Sprintf("invalid file path %q", truncate(m.Path, 80)))
		}
		if isReservedPath(m.Path) {
			return bad("paths under " + RemotePrefix + " are reserved")
		}
		if paths[m.Path] {
			return bad(fmt.Sprintf("file %q is listed twice", m.Path))
		}
		paths[m.Path] = true
		m.SHA256 = strings.ToLower(m.SHA256)
		if !isHexDigest(m.SHA256) {
			return bad(fmt.Sprintf("file %q needs a hex SHA-256 digest", m.Path))
		}
		if m.Size < 0 {
			return bad(fmt.Sprintf("file %q has a negative size", m.Path))
		}
		if m.Size > l.MaxFileBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("file %q exceeds the %d byte limit", m.Path, l.MaxFileBytes))
			return false
		}
		total += m.Size
		if total > l.MaxBundleBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("the files exceed the %d byte version limit", l.MaxBundleBytes))
			return false
		}
		if len(m.MediaType) > 255 {
			return bad(fmt.Sprintf("file %q has an invalid media type", m.Path))
		}
	}
	// A path may not also be a directory of another path, so a bundle
	// always writes out to a file tree.
	for p := range paths {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if paths[dir] {
				return bad(fmt.Sprintf("%q is both a file and a directory", dir))
			}
		}
	}
	if req.Entry == "" {
		if len(req.Files) != 1 {
			return bad("entry is required for a manifest of several files")
		}
		req.Entry = req.Files[0].Path
	}
	if !paths[req.Entry] {
		return bad("entry must name a file of the manifest")
	}
	return true
}

// manifestMediaType is the media type recorded for a file before its
// bytes arrive: from its extension, else the declared type, else
// application/octet-stream, which the upload then refines by sniffing.
func manifestMediaType(name, declared string) string {
	if mt, ok := extMediaTypes[strings.ToLower(path.Ext(name))]; ok {
		return mt
	}
	if declared != "" {
		if mt, _, err := mime.ParseMediaType(declared); err == nil {
			mt = strings.ToLower(mt)
			if mt != "application/octet-stream" && !strings.HasPrefix(mt, "multipart/") {
				return mt
			}
		}
	}
	return "application/octet-stream"
}

func isHexDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// canWrite reports whether the caller may append versions to artifact a:
// its credential must permit publishing (artifact.create) in the home
// scope, and it must own the artifact or hold an unexpired write or admin
// grant (a principal grant for it, or a scope grant for a scope the host
// authorizes it to publish in). The home scope's own read grant never
// confers write. Publishing a version is publishing; ownership and grants
// decide which artifacts.
func (s *Service) canWrite(ctx context.Context, b backend, a *Artifact) bool {
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok {
		return false
	}
	now := time.Now()
	if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false
	}
	if !s.host.Permits(ctx, a.ScopeRef, PermissionCreate) {
		return false
	}
	if kind == a.OwnerKind && ref == a.OwnerRef {
		return true
	}
	grants, err := b.store.ListGrants(ctx, a.ID)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list grants failed", "error", err)
		return false
	}
	for _, g := range grants {
		if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
			continue
		}
		if g.Permission != GrantWrite && g.Permission != GrantAdmin {
			continue
		}
		switch g.SubjectKind {
		case SubjectPrincipal:
			if g.SubjectRef == PrincipalRef(kind, ref) {
				return true
			}
		case SubjectScope:
			if g.SubjectRef != "" && s.host.Authorize(ctx, g.SubjectRef, PermissionCreate) {
				return true
			}
		}
	}
	return false
}

// writableArtifact loads an artifact the caller may write. An artifact the
// caller cannot read answers 404, like a missing one; one it can read but
// not write answers 403.
func (s *Service) writableArtifact(w http.ResponseWriter, r *http.Request, id string) (backend, *Artifact, bool) {
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return b, nil, false
	}
	if !s.canWrite(r.Context(), b, a) {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish versions of this artifact")
		return b, nil, false
	}
	return b, a, true
}

// pendingVersionOf loads pending version seq of a for its creator. Another
// caller gets 403; a version that is not pending, 409.
func (s *Service) pendingVersionOf(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, seq int) (*Version, bool) {
	v, err := b.store.GetVersion(r.Context(), a.ID, seq)
	if errors.Is(err, ErrNotFound) {
		writeNotFound(w)
		return nil, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: get version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return nil, false
	}
	kind, ref, _, _ := s.host.Principal(r.Context())
	if v.CreatedByKind != kind || v.CreatedByRef != ref {
		writeError(w, http.StatusForbidden, "forbidden", "only the publisher of a pending version may upload to it")
		return nil, false
	}
	if v.State != VersionStatePending {
		writeError(w, http.StatusConflict, "conflict", "the version is not pending")
		return nil, false
	}
	return v, true
}

// handlePutFile implements PUT /{id}/versions/{seq}/files/{path}: the raw
// body is the file's bytes. It must match the manifest's size and digest
// (X-Content-SHA256, when sent, must equal the manifest digest too).
// Uploading the same file again is allowed while the version is pending.
func (s *Service) handlePutFile(w http.ResponseWriter, r *http.Request, id string, seq int, filePath string) {
	ctx := r.Context()
	if _, err := cleanFilePath(filePath); err != nil {
		writeNotFound(w)
		return
	}
	b, a, ok := s.writableArtifact(w, r, id)
	if !ok {
		return
	}
	v, ok := s.pendingVersionOf(w, r, b, a, seq)
	if !ok {
		return
	}
	f, err := b.store.GetFile(ctx, v.ID, filePath)
	if errors.Is(err, ErrNotFound) || (err == nil && f.Origin != FileOriginUpload) {
		writeError(w, http.StatusNotFound, "not_found", "the file is not in the version's manifest")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: get file failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the file")
		return
	}
	if h := r.Header.Get(HeaderContentSHA256); h != "" && strings.ToLower(strings.TrimSpace(h)) != f.SHA256 {
		writeError(w, http.StatusBadRequest, "digest_mismatch", HeaderContentSHA256+" does not match the manifest")
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != f.Size {
		writeError(w, http.StatusBadRequest, "size_mismatch", "Content-Length does not match the manifest")
		return
	}
	spool, err := spoolBody(r.Body, f.Size)
	if errors.Is(err, errTooLarge) {
		writeError(w, http.StatusBadRequest, "size_mismatch", "the body is larger than the manifest says")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return
	}
	defer spool.Close()
	if spool.size != f.Size {
		writeError(w, http.StatusBadRequest, "size_mismatch", "the body is smaller than the manifest says")
		return
	}
	if spool.digest != f.SHA256 {
		writeError(w, http.StatusBadRequest, "digest_mismatch", "the body does not match the manifest digest")
		return
	}
	mediaType := detectMediaType(filePath, f.MediaType, spool.head)
	if err := s.putBlob(ctx, b, spool, mediaType); err != nil {
		slog.ErrorContext(ctx, "artifacts: blob write failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not store the file")
		return
	}
	switch err := b.store.MarkReceived(ctx, v.ID, filePath, mediaType); {
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "the version is not pending")
		return
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: mark received failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the file")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleFinalize implements POST /{id}/versions/{seq}/finalize: it checks
// every manifest file has arrived, flips the version to ready and makes it
// the artifact's current version (unless a later version already is).
func (s *Service) handleFinalize(w http.ResponseWriter, r *http.Request, id string, seq int) {
	ctx := r.Context()
	b, a, ok := s.writableArtifact(w, r, id)
	if !ok {
		return
	}
	v, ok := s.pendingVersionOf(w, r, b, a, seq)
	if !ok {
		return
	}
	files, err := b.store.ListFiles(ctx, v.ID)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list files failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return
	}
	var missing []string
	for _, f := range files {
		if f.Pending {
			missing = append(missing, f.Path)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		count := len(missing)
		if len(missing) > maxListedPath {
			missing = missing[:maxListedPath]
		}
		writeJSON(w, http.StatusConflict, errorResponse{Error: errorBody{
			Code:    CodeIncomplete,
			Message: fmt.Sprintf("%d file(s) of the manifest have not been uploaded", count),
			Details: map[string]any{"missing": missing, "missingCount": count},
		}})
		return
	}
	// Claim the version before any remote fetch, so that one finalize
	// request completes it and concurrent ones answer 409 at once.
	switch err := b.store.ClaimFinalize(ctx, a.ID, seq); {
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "the version is not pending or not complete")
		return
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: claim finalize failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not finalize the version")
		return
	}
	extra, warnings := s.finalizeExtras(w, r, b, v, files)
	updated, err := b.store.FinalizeVersion(ctx, a.ID, seq, extra)
	if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
		// Let the publisher try again.
		if rerr := b.store.ReleaseFinalize(context.WithoutCancel(ctx), a.ID, seq); rerr != nil {
			slog.ErrorContext(ctx, "artifacts: release finalize failed", "error", rerr)
		}
	}
	switch {
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "the version is not pending or not complete")
		return
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: finalize failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not finalize the version")
		return
	}
	ready, err := b.store.GetVersion(ctx, a.ID, seq)
	if err == nil {
		files, err = b.store.ListFiles(ctx, ready.ID)
	}
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: read finalized version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return
	}
	writeJSON(w, http.StatusOK, ArtifactResponse{Artifact: artifactInfo(updated), Version: versionInfo(ready, files), Warnings: warnings})
}

// finalizeExtras returns the manifest rows the hub adds to a version at
// finalize, with warnings for the publisher: the remote images of a
// markdown entry, fetched now so that reading the version never fetches
// anything.
func (s *Service) finalizeExtras(w http.ResponseWriter, r *http.Request, b backend, v *Version, files []File) ([]File, []string) {
	ctx := r.Context()
	var entry *File
	for i := range files {
		if files[i].Path == v.EntryPath {
			entry = &files[i]
		}
	}
	if entry == nil || entry.SHA256 == "" {
		return nil, nil
	}
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
		slog.ErrorContext(ctx, "artifacts: read markdown entry failed", "error", err)
		return nil, nil
	}
	window, err := readWindow(rc, entry.Size)
	_ = rc.Close()
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: read markdown entry failed", "error", err)
		return nil, nil
	}
	return s.remoteImages(ctx, w, b, v.ID, window, entry.Size > imageScanWindow, versionUsage{files: v.FileCount, bytes: v.TotalBytes})
}

// handleListVersions implements GET /{id}/versions: the ready versions,
// newest first, without their files.
func (s *Service) handleListVersions(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	versions, err := b.store.ListVersions(r.Context(), a.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: list versions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the versions")
		return
	}
	resp := VersionListResponse{Versions: make([]VersionInfo, 0, len(versions))}
	for i := range versions {
		vi := versionInfo(&versions[i], nil)
		vi.Files = nil
		resp.Versions = append(resp.Versions, *vi)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetVersion implements GET /{id}/versions/{seq}: one ready version
// with its files.
func (s *Service) handleGetVersion(w http.ResponseWriter, r *http.Request, id string, seq int) {
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	v, ok := readyVersion(w, r, b, a, seq)
	if !ok {
		return
	}
	files, err := b.store.ListFiles(r.Context(), v.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: list files failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return
	}
	writeJSON(w, http.StatusOK, ArtifactResponse{Artifact: artifactInfo(a), Version: versionInfo(v, files)})
}

// ReapPending reaps versions left pending longer than PendingVersionTTL
// (see Store.ReapPending), at most limit per call. The host calls it
// periodically.
func (s *Service) ReapPending(ctx context.Context, limit int) (int, error) {
	b, ok := s.backend()
	if !ok {
		return 0, nil
	}
	return b.store.ReapPending(ctx, time.Now().Add(-PendingVersionTTL), limit)
}
