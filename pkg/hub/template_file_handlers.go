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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"gopkg.in/yaml.v3"
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

const scionAgentConfigFile = "scion-agent.yaml"

// detectHarnessFromContent parses scion-agent.yaml content and returns
// the harness type and default harness config name.
// Falls back to templateName-based inference.
func detectHarnessFromContent(data []byte, templateName string) templateConfigInfo {
	var raw struct {
		HarnessConfig        string `yaml:"harness_config"`
		DefaultHarnessConfig string `yaml:"default_harness_config"`
		Harness              string `yaml:"harness"`
	}

	// Normalize hyphenated keys to underscored before parsing
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err == nil {
		if node.Kind == yaml.DocumentNode {
			for _, child := range node.Content {
				if child.Kind == yaml.MappingNode {
					for i := 0; i < len(child.Content); i += 2 {
						key := child.Content[i]
						if key.Kind == yaml.ScalarNode {
							key.Value = strings.ReplaceAll(key.Value, "-", "_")
						}
					}
				}
			}
		}
		_ = node.Decode(&raw)
	}

	if raw.HarnessConfig != "" {
		return templateConfigInfo{
			Harness:              inferHarnessFromName(raw.HarnessConfig),
			DefaultHarnessConfig: raw.HarnessConfig,
		}
	}
	if raw.DefaultHarnessConfig != "" {
		return templateConfigInfo{
			Harness:              inferHarnessFromName(raw.DefaultHarnessConfig),
			DefaultHarnessConfig: raw.DefaultHarnessConfig,
		}
	}
	if raw.Harness != "" {
		return templateConfigInfo{Harness: raw.Harness}
	}
	return templateConfigInfo{Harness: inferHarnessFromName(templateName)}
}

// handleTemplateFiles dispatches template file operations.
// filePath is empty for listing, non-empty for single-file operations.
func (s *Server) handleTemplateFiles(w http.ResponseWriter, r *http.Request, templateID, filePath string) {
	if filePath == "" {
		// Collection endpoint: GET = list, POST = upload
		switch r.Method {
		case http.MethodGet:
			s.handleTemplateFileList(w, r, templateID)
		case http.MethodPost:
			s.handleTemplateFileUpload(w, r, templateID)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	// Single-file endpoint
	switch r.Method {
	case http.MethodGet:
		s.handleTemplateFileRead(w, r, templateID, filePath)
	case http.MethodPut:
		s.handleTemplateFileWrite(w, r, templateID, filePath)
	case http.MethodDelete:
		s.handleTemplateFileDelete(w, r, templateID, filePath)
	default:
		MethodNotAllowed(w)
	}
}

// handleTemplateFileList returns the file manifest for a template.
func (s *Server) handleTemplateFileList(w http.ResponseWriter, r *http.Request, templateID string) {
	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, templateID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

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
func (s *Server) handleTemplateFileRead(w http.ResponseWriter, r *http.Request, templateID, filePath string) {
	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, templateID)
	if err != nil {
		writeErrorFromErr(w, err, "")
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
	if r.URL.Query().Get("raw") != "" || strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		objectPath := template.StoragePath + "/" + filePath
		reader, _, err := stor.Download(ctx, objectPath)
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
		w.Header().Set("Content-Length", strconv.FormatInt(found.Size, 10))
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, reader); err != nil {
			slog.Error("Error streaming file to client", "path", objectPath, "error", err)
		}
		return
	}

	// JSON response with size limit
	if found.Size > maxTemplateFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"File too large for inline viewing. Use the download endpoint instead.", nil)
		return
	}

	objectPath := template.StoragePath + "/" + filePath
	reader, _, err := stor.Download(ctx, objectPath)
	if err != nil {
		if err == storage.ErrNotFound {
			NotFound(w, "Template file")
			return
		}
		RuntimeError(w, "Failed to read file from storage")
		return
	}
	defer reader.Close()

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

// handleTemplateFileWrite writes content to a template file.
// Supports two modes:
//   - JSON body (Content-Type: application/json): existing behavior with TemplateFileWriteRequest
//   - Raw binary body (any other Content-Type): writes raw request body to storage directly.
//     Used by the local storage proxy flow where clients PUT file content to hub URLs
//     instead of file:// signed URLs.
func (s *Server) handleTemplateFileWrite(w http.ResponseWriter, r *http.Request, templateID, filePath string) {
	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, templateID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if template.Locked {
		Forbidden(w)
		return
	}

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

	// Upload content to storage
	objectPath := template.StoragePath + "/" + filePath
	content := []byte(req.Content)
	_, err = stor.Upload(ctx, objectPath, strings.NewReader(req.Content), storage.UploadOptions{
		ContentType: "text/plain; charset=utf-8",
	})
	if err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}

	// Compute file hash
	h := sha256.Sum256(content)
	fileHash := "sha256:" + hex.EncodeToString(h[:])
	fileSize := int64(len(content))

	// Update the manifest
	fileFound := false
	for i := range template.Files {
		if template.Files[i].Path == filePath {
			template.Files[i].Size = fileSize
			template.Files[i].Hash = fileHash
			fileFound = true
			break
		}
	}
	if !fileFound {
		// New file — add to manifest
		template.Files = append(template.Files, store.TemplateFile{
			Path: filePath,
			Size: fileSize,
			Hash: fileHash,
		})
	}

	// Recompute content hash
	template.ContentHash = computeContentHash(template.Files)

	// Re-detect harness type and default harness config when the config file changes
	if filePath == scionAgentConfigFile {
		cfgInfo := detectHarnessFromContent(content, template.Name)
		template.Harness = cfgInfo.Harness
		template.DefaultHarnessConfig = cfgInfo.DefaultHarnessConfig
	}

	if err := s.store.UpdateTemplate(ctx, template); err != nil {
		writeErrorFromErr(w, err, "")
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

	// Upload to storage
	objectPath := template.StoragePath + "/" + filePath
	_, err = stor.Upload(ctx, objectPath, bytes.NewReader(data), storage.UploadOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}

	// Compute file hash
	h := sha256.Sum256(data)
	fileHash := "sha256:" + hex.EncodeToString(h[:])
	fileSize := int64(len(data))

	// Update the manifest
	fileFound := false
	for i := range template.Files {
		if template.Files[i].Path == filePath {
			template.Files[i].Size = fileSize
			template.Files[i].Hash = fileHash
			fileFound = true
			break
		}
	}
	if !fileFound {
		template.Files = append(template.Files, store.TemplateFile{
			Path: filePath,
			Size: fileSize,
			Hash: fileHash,
		})
	}

	// Recompute content hash
	template.ContentHash = computeContentHash(template.Files)

	// Re-detect harness type and default harness config when the config file changes
	if filePath == scionAgentConfigFile {
		cfgInfo := detectHarnessFromContent(data, template.Name)
		template.Harness = cfgInfo.Harness
		template.DefaultHarnessConfig = cfgInfo.DefaultHarnessConfig
	}

	if err := s.store.UpdateTemplate(ctx, template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleTemplateFileUpload handles multipart file uploads to a template.
func (s *Server) handleTemplateFileUpload(w http.ResponseWriter, r *http.Request, templateID string) {
	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, templateID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if template.Locked {
		Forbidden(w)
		return
	}

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

	var uploaded []TemplateFileEntry

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
			src.Close()
			if err != nil {
				RuntimeError(w, "Failed to read uploaded file")
				return
			}

			// Upload to storage
			objectPath := template.StoragePath + "/" + relPath
			_, err = stor.Upload(ctx, objectPath, bytes.NewReader(data), storage.UploadOptions{
				ContentType: "application/octet-stream",
			})
			if err != nil {
				RuntimeError(w, "Failed to upload file to storage")
				return
			}

			// Compute file hash
			h := sha256.Sum256(data)
			fileHash := "sha256:" + hex.EncodeToString(h[:])
			fileSize := int64(len(data))

			// Update or add to manifest
			fileFound := false
			for i := range template.Files {
				if template.Files[i].Path == relPath {
					template.Files[i].Size = fileSize
					template.Files[i].Hash = fileHash
					fileFound = true
					break
				}
			}
			if !fileFound {
				template.Files = append(template.Files, store.TemplateFile{
					Path: relPath,
					Size: fileSize,
					Hash: fileHash,
				})
			}

			// Re-detect harness type and default harness config when the config file changes
			if relPath == scionAgentConfigFile {
				cfgInfo := detectHarnessFromContent(data, template.Name)
				template.Harness = cfgInfo.Harness
				template.DefaultHarnessConfig = cfgInfo.DefaultHarnessConfig
			}

			uploaded = append(uploaded, TemplateFileEntry{
				Path:    relPath,
				Size:    fileSize,
				ModTime: template.Updated.UTC().Format("2006-01-02T15:04:05Z"),
				Mode:    "0644",
			})
		}
	}

	// Recompute content hash
	template.ContentHash = computeContentHash(template.Files)

	if err := s.store.UpdateTemplate(ctx, template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, TemplateFileUploadResponse{
		Files: uploaded,
		Hash:  template.ContentHash,
	})
}

// handleTemplateFileDelete removes a file from a template.
func (s *Server) handleTemplateFileDelete(w http.ResponseWriter, r *http.Request, templateID, filePath string) {
	ctx := r.Context()

	template, err := s.store.GetTemplate(ctx, templateID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if template.Locked {
		Forbidden(w)
		return
	}

	// Find and remove the file from the manifest
	idx := -1
	for i := range template.Files {
		if template.Files[i].Path == filePath {
			idx = i
			break
		}
	}
	if idx == -1 {
		NotFound(w, "Template file")
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Delete from storage
	objectPath := template.StoragePath + "/" + filePath
	if err := stor.Delete(ctx, objectPath); err != nil && err != storage.ErrNotFound {
		RuntimeError(w, "Failed to delete file from storage")
		return
	}

	// Remove from manifest
	template.Files = append(template.Files[:idx], template.Files[idx+1:]...)

	// Recompute content hash
	template.ContentHash = computeContentHash(template.Files)

	// Re-detect harness type when the config file is removed
	if filePath == scionAgentConfigFile {
		template.Harness = inferHarnessFromName(template.Name)
	}

	if err := s.store.UpdateTemplate(ctx, template); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
