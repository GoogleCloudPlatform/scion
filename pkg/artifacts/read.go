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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// paramStream asks the service to serve file bytes itself rather than
// redirect to a signed object-store URL. It is the general "hub serves the
// bytes" mode: the web renderer uses it to fetch text, which a cross-origin
// redirect to the object store would block, and later transforms of a file
// imply it. With the local storage provider the service always streams, so
// the parameter changes nothing there.
const paramStream = "stream"

// signedURLTTL is the lifetime of the object-store URL a file read
// redirects to. It only has to outlive the redirect.
const signedURLTTL = 5 * time.Minute

// HeaderRemoteStatus marks the 404 of a remote image whose fetch failed.
const HeaderRemoteStatus = "X-Artifact-Remote-Status"

// remoteCacheControl is the caching of a streamed remote image requested
// by an explicit version: its path is fixed to its source URL within one
// immutable version, so it never changes.
const remoteCacheControl = "private, max-age=31536000, immutable"

// fileCSP is the Content-Security-Policy of every streamed file response.
// A file opened directly in the browser gets an opaque origin and no
// script, so nothing served from the hub's origin can act on it.
const fileCSP = "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; sandbox"

// canRead reports whether the caller may read artifact a. The checks run in
// a fixed order:
//
//  1. Host.Permits: the caller's credential must allow artifact.read in the
//     artifact's home scope. Necessary on every path; nothing below can
//     reach past it.
//  2. The owner (implicit admin), or Host.Authorize for artifact.read in the
//     home scope.
//  3. An unexpired artifact_grant row: a principal grant matching the
//     caller, or a scope grant for a scope the host authorizes.
//
// An expired artifact is unreadable to everyone.
func (s *Service) canRead(ctx context.Context, b backend, a *Artifact) bool {
	kind, ref, _, ok := s.host.Principal(ctx)
	if !ok {
		return false
	}
	now := time.Now()
	if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false
	}
	// 1. Credential.
	if !s.host.Permits(ctx, a.ScopeRef, PermissionRead) {
		return false
	}
	// 2. Owner, or host policy in the home scope.
	if kind == a.OwnerKind && ref == a.OwnerRef {
		return true
	}
	if s.host.Authorize(ctx, a.ScopeRef, PermissionRead) {
		return true
	}
	// 3. Grants.
	grants, err := b.store.ListGrants(ctx, a.ID)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: list grants failed", "error", err)
		return false
	}
	for _, g := range grants {
		if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
			continue
		}
		if g.Permission != GrantRead && g.Permission != GrantWrite && g.Permission != GrantAdmin {
			continue
		}
		switch g.SubjectKind {
		case SubjectPrincipal:
			if g.SubjectRef == PrincipalRef(kind, ref) {
				return true
			}
		case SubjectScope:
			if g.SubjectRef != "" && g.SubjectRef != a.ScopeRef && s.host.Authorize(ctx, g.SubjectRef, PermissionRead) {
				return true
			}
		}
	}
	return false
}

// readableArtifact loads artifact id and checks the caller may read it. It
// writes the response and returns ok=false when the artifact is absent or
// unreadable (both 404 with the same body) or the service is unconfigured.
func (s *Service) readableArtifact(w http.ResponseWriter, r *http.Request, id string) (backend, *Artifact, bool) {
	b, ok := s.backend()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "artifact storage is not configured")
		return b, nil, false
	}
	a, err := b.store.GetArtifact(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeNotFound(w)
		return b, nil, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: get artifact failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the artifact")
		return b, nil, false
	}
	if !s.canRead(r.Context(), b, a) {
		writeNotFound(w)
		return b, nil, false
	}
	return b, a, true
}

// readyVersion returns version seq of a, or its current version when seq
// is 0, provided it is ready. ok=false means the response was written.
func readyVersion(w http.ResponseWriter, r *http.Request, b backend, a *Artifact, seq int) (*Version, bool) {
	if seq == 0 {
		seq = a.CurrentSeq
	}
	if seq == 0 {
		writeNotFound(w)
		return nil, false
	}
	v, err := b.store.GetVersion(r.Context(), a.ID, seq)
	if errors.Is(err, ErrNotFound) || (err == nil && v.State != VersionStateReady) {
		writeNotFound(w)
		return nil, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: get version failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
		return nil, false
	}
	return v, true
}

// handleGetArtifact serves the metadata of an artifact and its current
// version.
func (s *Service) handleGetArtifact(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	if a.CurrentSeq == 0 {
		// An artifact whose first version is still pending is shown only to
		// its owner until that version is finalized.
		kind, ref, _, _ := s.host.Principal(r.Context())
		if kind != a.OwnerKind || ref != a.OwnerRef {
			writeNotFound(w)
			return
		}
	}
	resp := ArtifactResponse{Artifact: artifactInfo(a)}
	if a.CurrentSeq > 0 {
		v, ok := readyVersion(w, r, b, a, 0)
		if !ok {
			return
		}
		files, err := b.store.ListFiles(r.Context(), v.ID)
		if err != nil {
			slog.ErrorContext(r.Context(), "artifacts: list files failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not read the version")
			return
		}
		resp.Version = versionInfo(v, files)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetFile serves one file of version seq (0 = current) of an
// artifact.
func (s *Service) handleGetFile(w http.ResponseWriter, r *http.Request, id string, seq int, filePath string) {
	if _, err := cleanFilePath(filePath); err != nil {
		writeNotFound(w)
		return
	}
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	v, ok := readyVersion(w, r, b, a, seq)
	if !ok {
		return
	}
	f, err := b.store.GetFile(r.Context(), v.ID, filePath)
	if errors.Is(err, ErrNotFound) {
		writeNotFound(w)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "artifacts: get file failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read the file")
		return
	}
	if f.Origin == FileOriginRemote && (f.FetchStatus != FetchStatusOK || f.SHA256 == "") {
		// A remote image whose fetch failed has no bytes. The header lets
		// the renderer show its placeholder; it carries no reason.
		w.Header().Set(HeaderRemoteStatus, FetchStatusFailed)
		writeNotFound(w)
		return
	}
	if f.SHA256 == "" || f.Pending {
		// A manifest entry with no content has no bytes to serve.
		writeNotFound(w)
		return
	}
	// A remote image is cached as immutable only on a versioned URL; the
	// current-version URL can point at another version later.
	serveFile(w, r, b, f, deliveryFor(r, b), seq > 0 && f.Origin == FileOriginRemote)
}

// delivery is how file bytes reach the client.
type delivery int

const (
	// deliverStream: the service reads the blob and writes it.
	deliverStream delivery = iota
	// deliverRedirect: the service answers 302 to a short-lived signed
	// object-store URL, so the bytes do not pass through the hub.
	deliverRedirect
)

// deliveryFor picks the delivery for a request. The local provider has no
// URL a client could follow, so it always streams. Otherwise ?stream=1
// streams and the default redirects. Transforms of the bytes (a later
// phase) must also stream, and belong here.
func deliveryFor(r *http.Request, b backend) delivery {
	if b.blobs.Provider() == storage.ProviderLocal {
		return deliverStream
	}
	switch r.URL.Query().Get(paramStream) {
	case "1", "true":
		return deliverStream
	}
	return deliverRedirect
}

// serveFile writes a file in the given delivery. Authorization has already
// happened. The headers that make a file safe to serve (disposition,
// nosniff, private caching) are set here for every delivery, so all read
// routes, now and in later phases, share one code path.
func serveFile(w http.ResponseWriter, r *http.Request, b backend, f *File, how delivery, immutable bool) {
	ctx := r.Context()
	disposition := contentDisposition(f.MediaType, f.Path)
	ctype := responseContentType(f.MediaType)
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", disposition)
	h.Set("Referrer-Policy", "no-referrer")

	blobPath := BlobPath(b.hubID, f.SHA256)
	if how == deliverRedirect {
		signed, err := b.blobs.GenerateSignedURL(ctx, blobPath, storage.SignedURLOptions{
			Method: http.MethodGet, Expires: signedURLTTL,
			ResponseContentType: ctype, ResponseContentDisposition: disposition,
		})
		if err != nil {
			slog.ErrorContext(ctx, "artifacts: sign download failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not read the file")
			return
		}
		h.Set("Cache-Control", "private, no-store")
		h.Set("Location", signed.URL)
		w.WriteHeader(http.StatusFound)
		return
	}

	etag := `"sha256:` + f.SHA256 + `"`
	h.Set("ETag", etag)
	if immutable {
		h.Set("Cache-Control", remoteCacheControl)
	} else {
		h.Set("Cache-Control", "private, no-cache")
	}
	h.Set("Content-Security-Policy", fileCSP)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	rc, _, err := b.blobs.Download(ctx, blobPath)
	if err != nil {
		slog.ErrorContext(ctx, "artifacts: blob read failed", "error", err)
		h.Del("Content-Length")
		h.Del("Content-Type")
		writeError(w, http.StatusInternalServerError, "internal", "could not read the file")
		return
	}
	defer func() { _ = rc.Close() }()
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		slog.WarnContext(ctx, "artifacts: blob stream interrupted", "error", err)
	}
}
