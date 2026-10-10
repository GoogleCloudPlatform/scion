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
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const maxHarnessConfigFileSize = 1 << 20 // 1 MB

type HarnessConfigFileListResponse = TemplateFileListResponse
type HarnessConfigFileEntry = TemplateFileEntry
type HarnessConfigFileContentResponse = TemplateFileContentResponse
type HarnessConfigFileUploadResponse = TemplateFileUploadResponse
type HarnessConfigFileWriteRequest = TemplateFileWriteRequest
type HarnessConfigFileWriteResponse = TemplateFileWriteResponse

func (s *Server) handleHarnessConfigFiles(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig, filePath string) {
	if filePath == "" {
		switch r.Method {
		case http.MethodGet:
			s.handleHarnessConfigFileList(w, r, hc)
		case http.MethodPost:
			s.handleHarnessConfigFileUpload(w, r, hc)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleHarnessConfigFileRead(w, r, hc, filePath)
	case http.MethodPut:
		s.handleHarnessConfigFileWrite(w, r, hc, filePath)
	case http.MethodDelete:
		s.handleHarnessConfigFileDelete(w, r, hc, filePath)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) handleHarnessConfigFileList(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {

	var totalSize int64
	entries := make([]HarnessConfigFileEntry, len(hc.Files))
	for i, f := range hc.Files {
		entries[i] = HarnessConfigFileEntry{
			Path:    f.Path,
			Size:    f.Size,
			ModTime: hc.Updated.UTC().Format("2006-01-02T15:04:05Z"),
			Mode:    f.Mode,
		}
		totalSize += f.Size
	}

	writeJSON(w, http.StatusOK, HarnessConfigFileListResponse{
		Files:      entries,
		TotalSize:  totalSize,
		TotalCount: len(entries),
	})
}

func (s *Server) handleHarnessConfigFileRead(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig, filePath string) {
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	ctx := r.Context()

	var found *store.TemplateFile
	for i := range hc.Files {
		if hc.Files[i].Path == filePath {
			found = &hc.Files[i]
			break
		}
	}
	if found == nil {
		NotFound(w, "Harness config file")
		return
	}

	// Raw binary download for the local storage proxy flow (the URLs
	// handleHarnessConfigDownload rewrites file:// to). Serves exactly the
	// file the lookup above resolved; it only skips the JSON envelope and
	// the inline size limit. Mirrors handleTemplateFileRead.
	if r.URL.Query().Get("raw") != "" || strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		stor := s.GetStorage()
		if stor == nil {
			RuntimeError(w, "Storage not configured")
			return
		}

		objectPath := hc.StoragePath + "/" + filePath
		reader, _, err := stor.Download(ctx, objectPath)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				NotFound(w, "Harness config file")
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

	if found.Size > maxHarnessConfigFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"File too large for inline viewing. Use the download endpoint instead.", nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	objectPath := hc.StoragePath + "/" + filePath
	reader, _, err := stor.Download(ctx, objectPath)
	if err != nil {
		if err == storage.ErrNotFound {
			NotFound(w, "Harness config file")
			return
		}
		RuntimeError(w, "Failed to read file from storage")
		return
	}
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(io.LimitReader(reader, maxHarnessConfigFileSize+1))
	if err != nil {
		RuntimeError(w, "Failed to read file content")
		return
	}

	if int64(len(data)) > maxHarnessConfigFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"File too large for inline viewing. Use the download endpoint instead.", nil)
		return
	}

	writeJSON(w, http.StatusOK, HarnessConfigFileContentResponse{
		Path:     filePath,
		Content:  string(data),
		Size:     int64(len(data)),
		ModTime:  hc.Updated.UTC().Format("2006-01-02T15:04:05Z"),
		Encoding: "utf-8",
		Hash:     found.Hash,
	})
}

func (s *Server) handleHarnessConfigFileWrite(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig, filePath string) {
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	ctx := r.Context()

	// Limit request body size for both JSON and raw content paths.
	r.Body = http.MaxBytesReader(w, r.Body, maxHarnessConfigFileSize+4096)

	var req HarnessConfigFileWriteRequest
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxHarnessConfigFileSize+1))
		if err != nil {
			BadRequest(w, "Failed to read request body")
			return
		}
		if int64(len(data)) > maxHarnessConfigFileSize {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"File too large for inline upload.", nil)
			return
		}
		req.Content = string(data)
	}

	if req.ExpectedHash != "" {
		for _, f := range hc.Files {
			if f.Path == filePath && f.Hash != req.ExpectedHash {
				writeError(w, http.StatusConflict, ErrCodeConflict,
					"File has been modified since last read", nil)
				return
			}
		}
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	content := []byte(req.Content)
	if isHarnessConfigYAMLPath(filePath) && refuseUnusableProvisionerYAML(w, hc.Name, content) {
		return
	}
	objectPath := hc.StoragePath + "/" + filePath
	_, err := stor.Upload(ctx, objectPath, strings.NewReader(req.Content), storage.UploadOptions{
		ContentType: "text/plain; charset=utf-8",
	})
	if err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}

	sum := sha256.Sum256(content)
	fileHash := "sha256:" + hex.EncodeToString(sum[:])
	fileSize := int64(len(content))

	updated := false
	for i := range hc.Files {
		if hc.Files[i].Path == filePath {
			hc.Files[i].Hash = fileHash
			hc.Files[i].Size = fileSize
			hc.Files[i].Mode = "0644"
			updated = true
			break
		}
	}
	if !updated {
		hc.Files = append(hc.Files, store.TemplateFile{
			Path: filePath,
			Size: fileSize,
			Hash: fileHash,
			Mode: "0644",
		})
	}

	hc.ContentHash = computeContentHash(hc.Files)

	if isHarnessConfigYAMLPath(filePath) {
		if entry, err := config.ParseHarnessConfigYAML(content); err == nil {
			if hc.Config == nil {
				hc.Config = &store.HarnessConfigData{}
			}
			hc.Config.Image = entry.Image
			applyModelConfigFromEntry(hc, entry)
		}
	}

	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		RuntimeError(w, "Failed to update harness config manifest")
		return
	}

	writeJSON(w, http.StatusOK, HarnessConfigFileWriteResponse{
		Path:    filePath,
		Size:    fileSize,
		Hash:    fileHash,
		ModTime: hc.Updated.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

func (s *Server) handleHarnessConfigFileUpload(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig) {
	ctx := r.Context()

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
		ValidationError(w, "at least one file is required", nil)
		return
	}

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// Write parts in a stable order.
	partPaths := make([]string, 0, len(r.MultipartForm.File))
	for filePath := range r.MultipartForm.File {
		partPaths = append(partPaths, filePath)
	}
	slices.Sort(partPaths)

	// Check every part that resolves to config.yaml before any part is
	// written, so a refused upload leaves storage and the record unchanged.
	for _, filePath := range partPaths {
		headers := r.MultipartForm.File[filePath]
		if !isHarnessConfigYAMLPath(filePath) || len(headers) == 0 {
			continue
		}
		data, err := readMultipartFile(headers[0])
		if err != nil {
			BadRequest(w, "Failed to read multipart file: "+err.Error())
			return
		}
		if refuseUnusableProvisionerYAML(w, hc.Name, data) {
			return
		}
	}

	files := hc.Files
	entries := make([]HarnessConfigFileEntry, 0, len(partPaths))
	for _, filePath := range partPaths {
		headers := r.MultipartForm.File[filePath]
		if err := validateWorkspaceFilePath(filePath); err != nil {
			BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
			return
		}
		if len(headers) == 0 {
			continue
		}

		src, err := headers[0].Open()
		if err != nil {
			BadRequest(w, "Failed to open multipart file: "+err.Error())
			return
		}
		data, err := io.ReadAll(src)
		closeErr := src.Close()
		if err != nil {
			BadRequest(w, "Failed to read multipart file: "+err.Error())
			return
		}
		if closeErr != nil {
			BadRequest(w, "Failed to close multipart file: "+closeErr.Error())
			return
		}

		objectPath := hc.StoragePath + "/" + filePath
		_, err = stor.Upload(ctx, objectPath, bytes.NewReader(data), storage.UploadOptions{})
		if err != nil {
			RuntimeError(w, "Failed to write file to storage")
			return
		}

		sum := sha256.Sum256(data)
		fileHash := "sha256:" + hex.EncodeToString(sum[:])
		fileSize := int64(len(data))

		updated := false
		for i := range files {
			if files[i].Path == filePath {
				files[i].Hash = fileHash
				files[i].Size = fileSize
				files[i].Mode = "0644"
				updated = true
				break
			}
		}
		if !updated {
			files = append(files, store.TemplateFile{
				Path: filePath,
				Size: fileSize,
				Hash: fileHash,
				Mode: "0644",
			})
		}

		entries = append(entries, HarnessConfigFileEntry{
			Path:    filePath,
			Size:    fileSize,
			ModTime: hc.Updated.UTC().Format("2006-01-02T15:04:05Z"),
			Mode:    "0644",
		})
	}

	hc.Files = files
	hc.ContentHash = computeContentHash(hc.Files)

	if entry, ok := extractHarnessConfigEntryFromStorage(ctx, stor, hc.StoragePath); ok {
		if entry.Image != "" {
			if hc.Config == nil {
				hc.Config = &store.HarnessConfigData{}
			}
			hc.Config.Image = entry.Image
		}
		applyModelConfigFromEntry(hc, entry)
	}

	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		RuntimeError(w, "Failed to update harness config manifest")
		return
	}

	writeJSON(w, http.StatusOK, HarnessConfigFileUploadResponse{
		Files: entries,
		Hash:  hc.ContentHash,
	})
}

// unusableProvisionerInYAML returns the refusal for configYAML when it
// parses and declares a provisioner block that could never provision an agent
// (builtin type, or no command), and nil otherwise. Content that does not
// parse is left to the existing handling. It is the single content check
// shared by the file write, multipart upload and import/reimport paths
// (ptone/scion#3133, ptone/scion#4181).
func unusableProvisionerInYAML(name string, configYAML []byte) *harness.UnusableProvisionerError {
	entry, err := config.ParseHarnessConfigYAML(configYAML)
	if err != nil {
		return nil
	}
	return harness.CheckProvisionerUsable(name, nil, entry)
}

// writeUnusableProvisioner writes the 422 harness_config_unusable answer that
// harness-config finalize returns for an unusable provisioner block.
func writeUnusableProvisioner(w http.ResponseWriter, perr *harness.UnusableProvisionerError) {
	writeError(w, http.StatusUnprocessableEntity, harnessConfigUnusableErrorCode, perr.PublicMessage(), nil)
}

// refuseUnusableProvisionerYAML writes 422 harness_config_unusable and returns
// true when configYAML parses and declares a provisioner block that could
// never provision an agent (builtin type, or no command). This is the same
// check and response as harness-config finalize (ptone/scion#3133). Content
// that does not parse is left to the existing handling.
func refuseUnusableProvisionerYAML(w http.ResponseWriter, name string, configYAML []byte) bool {
	if perr := unusableProvisionerInYAML(name, configYAML); perr != nil {
		writeUnusableProvisioner(w, perr)
		return true
	}
	return false
}

// unusableProvisionerImportError is returned by the import driver when a
// discovered harness-config declares an unusable provisioner block. The
// import is refused before anything is persisted.
type unusableProvisionerImportError struct {
	perr *harness.UnusableProvisionerError
}

// Error returns the public message, which carries no filesystem paths, since
// the streaming import endpoints send it to the client as-is.
func (e *unusableProvisionerImportError) Error() string { return e.perr.PublicMessage() }

// writeUnusableProvisionerImportError writes the finalize-equivalent 422 and
// returns true when err is an unusableProvisionerImportError.
func writeUnusableProvisionerImportError(w http.ResponseWriter, err error) bool {
	var ierr *unusableProvisionerImportError
	if errors.As(err, &ierr) {
		writeUnusableProvisioner(w, ierr.perr)
		return true
	}
	return false
}

// checkHarnessConfigDirProvisioner applies unusableProvisionerInYAML to the
// config.yaml of a discovered harness-config directory. A config.yaml that
// cannot be read is left to the existing import handling.
func checkHarnessConfigDirProvisioner(name, dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return nil
	}
	if perr := unusableProvisionerInYAML(name, data); perr != nil {
		return &unusableProvisionerImportError{perr: perr}
	}
	return nil
}

// isHarnessConfigYAMLPath reports whether a harness-config file path names
// the top-level config.yaml once cleaned, so spellings such as
// "./config.yaml" get the same checks as the exact name.
func isHarnessConfigYAMLPath(filePath string) bool {
	return path.Clean(filePath) == "config.yaml"
}

// readMultipartFile reads one multipart file part fully.
func readMultipartFile(fh *multipart.FileHeader) ([]byte, error) {
	src, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	data, err := io.ReadAll(src)
	closeErr := src.Close()
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close: %w", closeErr)
	}
	return data, nil
}

func (s *Server) handleHarnessConfigFileDelete(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig, filePath string) {
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}

	ctx := r.Context()

	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	remaining := hc.Files[:0]
	found := false
	for _, f := range hc.Files {
		if f.Path == filePath {
			found = true
			continue
		}
		remaining = append(remaining, f)
	}
	if !found {
		NotFound(w, "Harness config file")
		return
	}

	if err := stor.Delete(ctx, hc.StoragePath+"/"+filePath); err != nil && err != storage.ErrNotFound {
		RuntimeError(w, "Failed to delete file from storage")
		return
	}

	hc.Files = remaining
	hc.ContentHash = computeContentHash(hc.Files)
	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		RuntimeError(w, "Failed to update harness config manifest")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"path":    filePath,
		"message": fmt.Sprintf("Deleted %s", filePath),
	})
}
