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
)

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
	permitted := s.host.Permits(ctx, scope, PermissionCreate)
	if !permitted && s.writeMissingScope(w, r) {
		return
	}
	if !permitted || !s.host.Authorize(ctx, scope, PermissionCreate) {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed to publish artifacts in this scope")
		return
	}
	b, ok := s.backend()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact storage is not configured")
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

	now := time.Now().UTC()
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
		CreatedAt: now, UpdatedAt: now, ExpiresAt: retentionExpiry(b.currentLimits(ctx), now),
	}
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
	if err := b.store.CreatePublished(ctx, a, v, files, []Grant{g}); err != nil {
		slog.ErrorContext(ctx, "artifacts: publish failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the artifact")
		return
	}
	writeJSON(w, http.StatusCreated, ArtifactResponse{Artifact: artifactInfo(a), Version: versionInfo(v, files), Warnings: warnings})
}

// CodeMissingScope is the error code of a 403 answered to a publish whose
// credential lacks a scope publishing needs. The error's details carry the
// scope's name under "scope".
const CodeMissingScope = "missing_scope"

// writeMissingScope answers 403 missing_scope when the host reports that
// the caller's credential lacks a scope for publishing. It reports whether
// it wrote the response. Only the publish path calls it: publishing names
// no existing artifact, so the answer describes the caller's own
// credential and nothing else.
func (s *Service) writeMissingScope(w http.ResponseWriter, r *http.Request) bool {
	ex, ok := s.host.(ScopeExplainer)
	if !ok {
		return false
	}
	scope := ex.MissingScope(r.Context(), PermissionCreate)
	if scope == "" {
		return false
	}
	writeJSON(w, http.StatusForbidden, errorResponse{Error: errorBody{
		Code:    CodeMissingScope,
		Message: "the credential does not carry the " + scope + " scope needed to publish artifacts",
		Details: map[string]any{"scope": scope},
	}})
	return true
}

// putBlob stores the spooled body at its content address unless a blob
// with that digest already exists.
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

// storeBlob stores the bytes open yields at digest's content address
// unless a blob with that digest already exists. It is the only way bytes
// are written, so every write touches the blob first: the blob sweep then
// spares a blob this publish relies on, and a sweep deleting it finishes
// before the existence check below.
func storeBlob(ctx context.Context, b backend, digest, mediaType string, open func() (io.Reader, error)) error {
	marked, err := b.store.TouchBlob(ctx, digest, blobClock())
	if err != nil {
		return err
	}
	p := BlobPath(b.hubID, digest)
	// A blob that is marked may have a delete on its way to the object
	// store; store it again so this publish does not depend on the old
	// object (see deleteBlob), and clear the mark only once that worked.
	if !marked {
		exists, err := b.blobs.Exists(ctx, p)
		if err != nil {
			return fmt.Errorf("check blob: %w", err)
		}
		if exists {
			return nil
		}
	}
	body, err := open()
	if err != nil {
		return err
	}
	if _, err := b.blobs.Upload(ctx, p, body, storage.UploadOptions{ContentType: mediaType}); err != nil {
		return fmt.Errorf("upload blob: %w", err)
	}
	if marked {
		return b.store.ClearBlobMark(ctx, digest)
	}
	return nil
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
