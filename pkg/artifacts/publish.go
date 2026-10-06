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
	if !s.host.Permits(ctx, scope, PermissionCreate) || !s.host.Authorize(ctx, scope, PermissionCreate) {
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
	a := &Artifact{
		ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: scope,
		OwnerKind: kind, OwnerRef: ref, Title: title, CurrentSeq: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	v := &Version{
		ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: name,
		TotalBytes: spool.size, FileCount: 1, CreatedByKind: kind, CreatedByRef: ref,
		CreatedAt: now, State: VersionStateReady,
	}
	f := File{VersionID: v.ID, Path: name, Size: spool.size, SHA256: spool.digest, MediaType: mediaType}
	g := Grant{
		ID: uuid.NewString(), ArtifactID: a.ID, SubjectKind: SubjectScope, SubjectRef: scope,
		Permission: GrantRead, CreatedByRef: PrincipalRef(kind, ref), CreatedAt: now,
	}
	// A failure here can leave an unreferenced blob behind; blobs are
	// content-addressed, so a retry reuses it and the blob sweep reclaims it.
	if err := b.store.CreatePublished(ctx, a, v, []File{f}, []Grant{g}); err != nil {
		slog.ErrorContext(ctx, "artifacts: publish failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the artifact")
		return
	}
	writeJSON(w, http.StatusCreated, ArtifactResponse{Artifact: artifactInfo(a), Version: versionInfo(v, []File{f})})
}

// putBlob stores the spooled body at its content address unless a blob
// with that digest already exists.
func (s *Service) putBlob(ctx context.Context, b backend, sp *spooled, mediaType string) error {
	p := BlobPath(b.hubID, sp.digest)
	exists, err := b.blobs.Exists(ctx, p)
	if err != nil {
		return fmt.Errorf("check blob: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := sp.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind spool: %w", err)
	}
	if _, err := b.blobs.Upload(ctx, p, sp.file, storage.UploadOptions{ContentType: mediaType}); err != nil {
		return fmt.Errorf("upload blob: %w", err)
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
