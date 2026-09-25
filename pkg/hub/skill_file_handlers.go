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

package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxSkillFileSize mirrors the per-file limit enforced by handleSkillFinalize.
const maxSkillFileSize = 10 * 1024 * 1024 // 10 MB

// withSkillVersionUploadQuery appends the skill version as a query parameter
// to hub-proxied upload URLs produced by rewriteLocalUploadURLs. Skill files
// are stored per version (<storagePath>/<version>/<path>), so the files
// endpoint needs the version to locate the object. Non-hub URLs (e.g. GCS
// signed URLs) are left untouched.
func withSkillVersionUploadQuery(urls []UploadURLInfo, version string) []UploadURLInfo {
	for i := range urls {
		urls[i].URL = appendSkillVersionQuery(urls[i].URL, version)
	}
	return urls
}

// withSkillVersionDownloadQuery is the download counterpart of
// withSkillVersionUploadQuery.
func withSkillVersionDownloadQuery(urls []DownloadURLInfo, version string) []DownloadURLInfo {
	for i := range urls {
		urls[i].URL = appendSkillVersionQuery(urls[i].URL, version)
	}
	return urls
}

func appendSkillVersionQuery(rawURL, version string) string {
	if version == "" || !strings.Contains(rawURL, "/api/v1/skills/") {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "version=" + url.QueryEscape(version)
}

// validSkillFilePath rejects empty, absolute, or traversing file paths.
func validSkillFilePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return false
		}
	}
	return true
}

// handleSkillFiles serves /api/v1/skills/{id}/files/{path}?version={v}.
// This is the hub proxy endpoint that local-storage upload/download URLs are
// rewritten to (see rewriteLocalUploadURLs / rewriteLocalDownloadURLs).
// Cloud storage uses signed URLs and never routes through here.
func (s *Server) handleSkillFiles(w http.ResponseWriter, r *http.Request, skillID, filePath string) {
	if filePath == "" {
		NotFound(w, "Skill file")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleSkillFileRead(w, r, skillID, filePath)
	case http.MethodPut:
		s.handleSkillFileWrite(w, r, skillID, filePath)
	default:
		MethodNotAllowed(w)
	}
}

// skillFileRequestVersion extracts and validates the version query parameter.
func skillFileRequestVersion(w http.ResponseWriter, r *http.Request, filePath string) (string, bool) {
	if !validSkillFilePath(filePath) {
		BadRequest(w, "Invalid file path")
		return "", false
	}
	version := r.URL.Query().Get("version")
	if version == "" {
		ValidationError(w, "version query parameter is required", nil)
		return "", false
	}
	if _, err := semver.NewVersion(version); err != nil {
		ValidationError(w, fmt.Sprintf("invalid semver version %q", version), nil)
		return "", false
	}
	return version, true
}

// handleSkillFileRead streams a single file of a skill version.
func (s *Server) handleSkillFileRead(w http.ResponseWriter, r *http.Request, skillID, filePath string) {
	ctx := r.Context()

	version, ok := skillFileRequestVersion(w, r, filePath)
	if !ok {
		return
	}

	// Capability URL (#1792): a request carrying signature parameters is
	// authorized by the signature alone, in place of a principal, and only for
	// the exact skill/version/file it was issued for. Once signature
	// parameters are present the request never falls back to principal
	// authorization — an invalid, expired or mismatched signature is a 401.
	// UnifiedAuthMiddleware admits unauthenticated signed requests on the
	// strength of this check, so it must stay unconditional. It runs before
	// the skill lookup so an unauthenticated caller learns nothing about
	// which skill IDs exist.
	signed := hasSkillFileSignature(r)
	if signed && !s.verifySkillFileSignature(r, skillID, version, filePath, time.Now()) {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "invalid or expired download signature", nil)
		return
	}

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeSkillLookupError(w, err)
		return
	}
	if signed {
		// The signature is bound to the requested ID; the store must agree.
		if skill.ID != skillID {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "invalid or expired download signature", nil)
			return
		}
	} else if !s.authorizeSkillFileRead(w, r, skill) {
		return
	}

	s.serveSkillFile(w, r, skill, version, filePath)
}

// authorizeSkillFileRead applies principal authorization to an unsigned skill
// file read — the same authorization as handleSkillDownload. It writes the
// error response and returns false when the read is not allowed.
func (s *Server) authorizeSkillFileRead(w http.ResponseWriter, r *http.Request, skill *store.Skill) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if skill.Visibility != store.VisibilityPublic {
		if identity == nil {
			NotFound(w, "Skill")
			return false
		}
		decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
		if !decision.Allowed {
			NotFound(w, "Skill")
			return false
		}
	}
	return true
}

// serveSkillFile streams filePath of skill@version once the read is authorized.
func (s *Server) serveSkillFile(w http.ResponseWriter, r *http.Request, skill *store.Skill, version, filePath string) {
	ctx := r.Context()
	sv, err := s.store.GetSkillVersionByNumber(ctx, skill.ID, version)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var found *store.TemplateFile
	for i := range sv.Files {
		if sv.Files[i].Path == filePath {
			found = &sv.Files[i]
			break
		}
	}
	if found == nil {
		NotFound(w, "Skill file")
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	versionPath := skill.StoragePath + "/" + sv.Version
	objectPath := versionPath + "/" + filePath
	reader, _, err := stor.Download(ctx, objectPath)
	if err != nil && errors.Is(err, storage.ErrNotFound) {
		if legacy := s.legacyFallbackPath(versionPath); legacy != "" {
			reader, _, err = stor.Download(ctx, legacy+"/"+filePath)
		}
	}
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			NotFound(w, "Skill file")
			return
		}
		RuntimeError(w, "Failed to read file from storage")
		return
	}
	defer func() { _ = reader.Close() }()

	contentDisposition := mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(filePath)})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition)
	if found.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(found.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, reader); err != nil {
		slog.Error("Error streaming skill file to client", "path", objectPath, "error", err)
	}
}

// handleSkillFileWrite stores the raw request body as a file of a draft
// skill version. The version manifest is recorded later by finalize.
func (s *Server) handleSkillFileWrite(w http.ResponseWriter, r *http.Request, skillID, filePath string) {
	ctx := r.Context()
	defer func() { _ = r.Body.Close() }()

	version, ok := skillFileRequestVersion(w, r, filePath)
	if !ok {
		return
	}

	skill, err := s.store.GetSkill(ctx, skillID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Same authorization as handleSkillUpload.
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required", nil)
		return
	}
	decision := s.authzService.CheckAccess(ctx, identity, skillResource(skill), ActionUpdate)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "You do not have permission to upload files for this skill", nil)
		return
	}

	// Published versions are immutable. A missing version is allowed: the
	// upload endpoint issues URLs before a draft necessarily exists.
	sv, err := s.store.GetSkillVersionByNumber(ctx, skillID, version)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErrorFromErr(w, err, "")
		return
	}
	if err == nil && sv.Status != store.SkillVersionStatusDraft {
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("version %s is %s and immutable", version, sv.Status), nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxSkillFileSize+1))
	if err != nil {
		RuntimeError(w, "Failed to read request body")
		return
	}
	if int64(len(data)) > maxSkillFileSize {
		BadRequest(w, fmt.Sprintf("File %q exceeds 10MB limit", filePath))
		return
	}

	objectPath := skill.StoragePath + "/" + version + "/" + filePath
	if _, err := stor.Upload(ctx, objectPath, bytes.NewReader(data), storage.UploadOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}

	w.WriteHeader(http.StatusOK)
}
