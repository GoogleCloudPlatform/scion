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
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxTemplateFileSize is the maximum file size (in bytes) that can be read
// inline via the template file content endpoint. Larger files should be
// downloaded via signed URLs.
const maxTemplateFileSize = 1 << 20 // 1 MB

// TemplateFileListResponse is the response for listing template files.
type TemplateFileListResponse struct {
	Files      []TemplateFileEntry `json:"files"`
	TotalSize  int64               `json:"totalSize"`
	TotalCount int                 `json:"totalCount"`
}

// TemplateFileEntry is a single file in the template file listing.
type TemplateFileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
	Mode    string `json:"mode"`
}

// TemplateFileContentResponse is the response for reading a template file.
type TemplateFileContentResponse struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
	ModTime  string `json:"modTime"`
	Encoding string `json:"encoding"`
	Hash     string `json:"hash,omitempty"`
}

// TemplateFileUploadResponse is the response after uploading template files.
type TemplateFileUploadResponse struct {
	Files []TemplateFileEntry `json:"files"`
	Hash  string              `json:"hash"`
}

// TemplateFileWriteRequest is the request body for writing a template file.
type TemplateFileWriteRequest struct {
	Content      string `json:"content"`
	ExpectedHash string `json:"expectedHash,omitempty"`
}

// TemplateFileWriteResponse is the response after writing a template file.
type TemplateFileWriteResponse struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Hash    string `json:"hash"`
	ModTime string `json:"modTime"`
}

// handleTemplateFiles dispatches template file operations.
// filePath is empty for listing, non-empty for single-file operations.
//
// Authorization is enforced here rather than in each leaf handler, which is a
// deliberate departure from the sibling template actions. Those gate
// per-handler, and the reason this route was reachable unauthenticated is that
// the whole file was written without one: the /api/v1/templates/ route is
// classified RoutePolicy, which passes through by design and delegates
// enforcement to the handler, and that delegation was simply never honoured
// here. Gating at the single dispatch point means a sub-handler added later
// inherits the check instead of having to remember it.
//
// filePath is validated for the same reason it cannot be trusted downstream:
// it is concatenated into a storage object path and, on write, recorded in the
// template manifest. The Go mux cleans a literal "../" out of the request path
// before routing, but r.URL.Path is already percent-decoded by then, so an
// encoded "..%2f" arrives here intact.
func (s *Server) handleTemplateFiles(w http.ResponseWriter, r *http.Request, templateID, filePath string) {
	if filePath != "" {
		if err := validateWorkspaceFilePath(filePath); err != nil {
			BadRequest(w, "Invalid file path: "+err.Error())
			return
		}
	}

	template, err := s.store.GetTemplate(r.Context(), templateID)
	if err != nil {
		writeStoreErr(w, err, "Template")
		return
	}

	// SECURITY-GATE: authorize access to this specific template before any
	// storage read, storage write or manifest change. Reads require read;
	// everything that mutates template content requires update, matching the
	// upload and finalize actions, which also mutate content rather than the
	// template record itself. A read denial reads as 404 (ptone/scion#1916),
	// matching getTemplateV2; a write denial stays 403.
	if r.Method == http.MethodGet {
		if !s.authorizeTemplateReadRoute(w, r, template) {
			return
		}
	} else if !requireTemplateProfileWriter(w, r, template) {
		return
	} else if !s.authorize(w, r, templateResource(template), ActionUpdate) {
		return
	}

	if filePath == "" {
		// Collection endpoint: GET = list, POST = upload
		switch r.Method {
		case http.MethodGet:
			s.handleTemplateFileList(w, r, template)
		case http.MethodPost:
			s.handleTemplateFileUpload(w, r, template)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}

	// Single-file endpoint
	switch r.Method {
	case http.MethodGet:
		s.handleTemplateFileRead(w, r, template, filePath)
	case http.MethodPut:
		s.handleTemplateFileWrite(w, r, template, filePath)
	case http.MethodDelete:
		s.handleTemplateFileDelete(w, r, template, filePath)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

// handleTemplateFileList returns the file manifest for a template.
func (s *Server) handleTemplateFileList(w http.ResponseWriter, r *http.Request, template *store.Template) {

	var totalSize int64
	entries := make([]TemplateFileEntry, len(template.Files))
	for i, f := range template.Files {
		entries[i] = TemplateFileEntry{
			Path:    f.Path,
			Size:    f.Size,
			ModTime: template.Updated.UTC().Format("2006-01-02T15:04:05Z"),
			Mode:    f.Mode,
		}
		totalSize += f.Size
	}

	writeJSON(w, http.StatusOK, TemplateFileListResponse{
		Files:      entries,
		TotalSize:  totalSize,
		TotalCount: len(entries),
	})
}

// handleTemplateFileRead returns the content of a single template file.
// Supports two modes:
//   - Accept: application/octet-stream — streams raw binary content (used by the
//     local storage proxy flow for downloads)
//   - Default — returns JSON with content, size, hash (existing behavior, 1MB limit)
func (s *Server) handleTemplateFileRead(w http.ResponseWriter, r *http.Request, template *store.Template, filePath string) {
	ctx := r.Context()

	raw := r.URL.Query().Get("raw") != "" || strings.Contains(r.Header.Get("Accept"), "application/octet-stream")

	// A raw read that names a hash serves that blob of this template (the
	// local-storage download URLs of a blob row carry it). The hash does not
	// have to be in the current manifest, so a hydration that started
	// before a commit still gets the version it began with
	// (ptone/scion#4221).
	if hash := r.URL.Query().Get("hash"); raw && hash != "" && isBlobLayout(template) {
		hex, ok := templateBlobHex(hash)
		if !ok {
			BadRequest(w, "Invalid hash")
			return
		}
		stor := s.GetStorage()
		if stor == nil {
			RuntimeError(w, "Storage not configured")
			return
		}
		s.streamTemplateObject(w, r, stor, templateBlobPath(template.StoragePath, hex), filePath)
		return
	}

	// Find the file in the manifest
	var found *store.TemplateFile
	for i := range template.Files {
		if template.Files[i].Path == filePath {
			found = &template.Files[i]
			break
		}
	}
	if found == nil {
		NotFound(w, "Template file")
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Raw binary download for local storage proxy flow
	if raw {
		s.streamTemplateObject(w, r, stor, templateObjectPath(template, *found), filePath)
		return
	}

	// JSON response with size limit
	if found.Size > maxTemplateFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"File too large for inline viewing. Use the download endpoint instead.", nil)
		return
	}

	objectPath := templateObjectPath(template, *found)
	reader, _, err := stor.Download(ctx, objectPath)
	if err != nil {
		if err == storage.ErrNotFound {
			NotFound(w, "Template file")
			return
		}
		RuntimeError(w, "Failed to read file from storage")
		return
	}
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(io.LimitReader(reader, maxTemplateFileSize+1))
	if err != nil {
		RuntimeError(w, "Failed to read file content")
		return
	}

	if int64(len(data)) > maxTemplateFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"File too large for inline viewing. Use the download endpoint instead.", nil)
		return
	}

	writeJSON(w, http.StatusOK, TemplateFileContentResponse{
		Path:     filePath,
		Content:  string(data),
		Size:     int64(len(data)),
		ModTime:  template.Updated.UTC().Format("2006-01-02T15:04:05Z"),
		Encoding: "utf-8",
		Hash:     found.Hash,
	})
}

// streamTemplateObject streams one stored object as a raw file download
// named after filePath.
func (s *Server) streamTemplateObject(w http.ResponseWriter, r *http.Request, stor storage.Storage, objectPath, filePath string) {
	reader, obj, err := stor.Download(r.Context(), objectPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			NotFound(w, "Template file")
			return
		}
		RuntimeError(w, "Failed to read file from storage")
		return
	}
	defer func() { _ = reader.Close() }()

	safeName := filepath.Base(filePath)
	contentDisposition := mime.FormatMediaType("attachment", map[string]string{"filename": safeName})

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition)
	setDownloadContentLength(w, obj)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, reader); err != nil {
		slog.Error("Error streaming file to client", "path", objectPath, "error", err)
	}
}

// handleTemplateFileWrite writes content to a template file.
// Supports two modes:
//   - JSON body (Content-Type: application/json): existing behavior with TemplateFileWriteRequest
//   - Raw binary body (any other Content-Type): writes raw request body to storage directly.
//     Used by the local storage proxy flow where clients PUT file content to hub URLs
//     instead of file:// signed URLs.
func (s *Server) handleTemplateFileWrite(w http.ResponseWriter, r *http.Request, template *store.Template, filePath string) {
	ctx := r.Context()

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Detect raw binary upload vs JSON request
	contentType := r.Header.Get("Content-Type")
	if contentType != "" && contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
		s.handleTemplateFileWriteRaw(w, r, template, filePath, stor)
		return
	}

	var req TemplateFileWriteRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Optimistic concurrency: check hash if provided
	if req.ExpectedHash != "" {
		for _, f := range template.Files {
			if f.Path == filePath && f.Hash != req.ExpectedHash {
				writeError(w, http.StatusConflict, ErrCodeConflict,
					"File has been modified since last read", nil)
				return
			}
		}
	}

	content := []byte(req.Content)
	if refuseUnusableBundledHarnessConfig(w, filePath, content) {
		return
	}

	// Store the content as a blob, then commit it.
	fileHash, blobPath, err := s.writeTemplateFileBlob(ctx, stor, template, content)
	if err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}
	fileSize := int64(len(content))

	next := upsertTemplateFile(template.Files, store.TemplateFile{Path: filePath, Size: fileSize, Hash: fileHash})
	if err := s.commitTemplateFiles(ctx, template, next, commitOpts{written: map[string]bool{blobPath: true}}); err != nil {
		writeTemplateCommitError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, TemplateFileWriteResponse{
		Path:    filePath,
		Size:    fileSize,
		Hash:    fileHash,
		ModTime: template.Updated.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// handleTemplateFileWriteRaw handles raw binary PUT uploads to a template file.
// Used by the local storage proxy flow: instead of file:// signed URLs, the hub
// returns HTTP URLs pointing to this endpoint, and the client PUTs file content directly.
func (s *Server) handleTemplateFileWriteRaw(w http.ResponseWriter, r *http.Request, template *store.Template, filePath string, stor storage.Storage) {
	ctx := r.Context()

	defer func() { _ = r.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(r.Body, maxUploadFileSize+1))
	if err != nil {
		RuntimeError(w, "Failed to read request body")
		return
	}
	if int64(len(data)) > maxUploadFileSize {
		BadRequest(w, fmt.Sprintf("File %q exceeds 50MB limit", filePath))
		return
	}

	if refuseUnusableBundledHarnessConfig(w, filePath, data) {
		return
	}

	// An upload URL (?uploadId=) only stages the file: finalize hashes and
	// commits the whole upload in one compare-and-swap, so nothing here
	// touches the row or what readers see (ptone/scion#4221).
	if uploadID := r.URL.Query().Get("uploadId"); uploadID != "" {
		if !validTemplateUploadID(uploadID) {
			BadRequest(w, "Invalid uploadId")
			return
		}
		staged := templateStagedObjectPath(s.templateContentBase(template), uploadID, filePath)
		if _, err := stor.Upload(ctx, staged, bytes.NewReader(data), storage.UploadOptions{
			ContentType: "application/octet-stream",
		}); err != nil {
			RuntimeError(w, "Failed to write file to storage")
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// Store the content as a blob, then commit it.
	fileHash, blobPath, err := s.writeTemplateFileBlob(ctx, stor, template, data)
	if err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}
	fileSize := int64(len(data))

	next := upsertTemplateFile(template.Files, store.TemplateFile{Path: filePath, Size: fileSize, Hash: fileHash})
	if err := s.commitTemplateFiles(ctx, template, next, commitOpts{written: map[string]bool{blobPath: true}}); err != nil {
		writeTemplateCommitError(w, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleTemplateFileUpload handles multipart file uploads to a template.
func (s *Server) handleTemplateFileUpload(w http.ResponseWriter, r *http.Request, template *store.Template) {
	ctx := r.Context()

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Apply total request body size limit
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadTotalSize)

	if err := r.ParseMultipartForm(maxUploadTotalSize); err != nil {
		if err.Error() == "http: request body too large" {
			BadRequest(w, "Request body exceeds 100MB limit")
			return
		}
		BadRequest(w, "Invalid multipart form: "+err.Error())
		return
	}

	if r.MultipartForm == nil || len(r.MultipartForm.File) == 0 {
		ValidationError(w, "No files provided", nil)
		return
	}

	// Read and check every part before uploading any, so a refused part
	// (an invalid path, an oversized file, or an unusable bundled
	// harness-config) leaves storage untouched.
	type uploadPart struct {
		path string
		data []byte
	}
	var parts []uploadPart
	for fieldName, fileHeaders := range r.MultipartForm.File {
		for _, fh := range fileHeaders {
			relPath := fieldName

			if err := validateWorkspaceFilePath(relPath); err != nil {
				BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", relPath, err.Error()))
				return
			}

			if fh.Size > maxUploadFileSize {
				BadRequest(w, fmt.Sprintf("File %q exceeds 50MB limit", relPath))
				return
			}

			src, err := fh.Open()
			if err != nil {
				RuntimeError(w, "Failed to open uploaded file")
				return
			}

			data, err := io.ReadAll(src)
			_ = src.Close()
			if err != nil {
				RuntimeError(w, "Failed to read uploaded file")
				return
			}

			if refuseUnusableBundledHarnessConfig(w, relPath, data) {
				return
			}
			parts = append(parts, uploadPart{path: relPath, data: data})
		}
	}

	var uploaded []TemplateFileEntry
	next := template.Files
	written := make(map[string]bool, len(parts))
	for _, part := range parts {
		relPath, data := part.path, part.data

		// Store the content as a blob; the commit below makes it visible.
		fileHash, blobPath, err := s.writeTemplateFileBlob(ctx, stor, template, data)
		if err != nil {
			RuntimeError(w, "Failed to upload file to storage")
			return
		}
		written[blobPath] = true
		fileSize := int64(len(data))

		next = upsertTemplateFile(next, store.TemplateFile{Path: relPath, Size: fileSize, Hash: fileHash})

		uploaded = append(uploaded, TemplateFileEntry{
			Path:    relPath,
			Size:    fileSize,
			ModTime: template.Updated.UTC().Format("2006-01-02T15:04:05Z"),
			Mode:    "0644",
		})
	}

	if err := s.commitTemplateFiles(ctx, template, next, commitOpts{written: written}); err != nil {
		writeTemplateCommitError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, TemplateFileUploadResponse{
		Files: uploaded,
		Hash:  template.ContentHash,
	})
}

// handleTemplateFileDelete removes a file from a template.
func (s *Server) handleTemplateFileDelete(w http.ResponseWriter, r *http.Request, template *store.Template, filePath string) {
	ctx := r.Context()

	found := false
	for i := range template.Files {
		if template.Files[i].Path == filePath {
			found = true
			break
		}
	}
	if !found {
		NotFound(w, "Template file")
		return
	}

	if s.GetStorage() == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// The commit drops the file from the manifest and re-derives the index
	// (removing scion-agent.yaml clears DefaultHarnessConfig and AgentConfig).
	// The removed file's blob stays for in-flight hydrations until the blob
	// garbage collector's grace period has passed.
	if err := s.commitTemplateFiles(ctx, template, removeTemplateFile(template.Files, filePath), commitOpts{}); err != nil {
		writeTemplateCommitError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
