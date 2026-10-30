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
	"mime/multipart"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const maxHarnessConfigFileSize = 1 << 20 // 1 MB

// ErrCodeHarnessConfigYAMLRequired is the 400 code for deleting a
// harness-config's config.yaml through the file API (ptone/scion#4219).
const ErrCodeHarnessConfigYAMLRequired = "harness_config_yaml_required"

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

	raw := r.URL.Query().Get("raw") != "" || strings.Contains(r.Header.Get("Accept"), "application/octet-stream")

	// A raw read that names a hash serves that blob of this harness-config
	// (the local-storage download URLs of a blob row carry it). The hash
	// does not have to be in the current manifest, so a hydration that
	// started before a commit still gets the version it began with
	// (ptone/scion#4219).
	if hash := r.URL.Query().Get("hash"); raw && hash != "" && isHarnessConfigBlobLayout(hc) {
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
		s.streamHarnessConfigObject(w, r, stor, templateBlobPath(hc.StoragePath, hex), filePath)
		return
	}

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
	if raw {
		stor := s.GetStorage()
		if stor == nil {
			RuntimeError(w, "Storage not configured")
			return
		}
		s.streamHarnessConfigObject(w, r, stor, s.harnessConfigObjectPath(hc, *found), filePath)
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

	objectPath := s.harnessConfigObjectPath(hc, *found)
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

// streamHarnessConfigObject streams one stored object as a raw file download
// named after filePath.
func (s *Server) streamHarnessConfigObject(w http.ResponseWriter, r *http.Request, stor storage.Storage, objectPath, filePath string) {
	reader, obj, err := stor.Download(r.Context(), objectPath)
	if err != nil || reader == nil {
		if err == nil || errors.Is(err, storage.ErrNotFound) {
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
	setDownloadContentLength(w, obj)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, reader); err != nil {
		slog.Error("Error streaming file to client", "path", objectPath, "error", err)
	}
}

// handleHarnessConfigFileWrite writes one file and commits it, or, with
// ?uploadId= (the local-storage upload URLs), only stages it for a later
// finalize.
func (s *Server) handleHarnessConfigFileWrite(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig, filePath string) {
	if err := validateWorkspaceFilePath(filePath); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", filePath, err.Error()))
		return
	}
	// The manifest holds canonical paths, so "./config.yaml" writes the
	// config.yaml entry.
	filePath = path.Clean(filePath)

	ctx := r.Context()

	// An upload URL (?uploadId=) only stages the file: finalize hashes and
	// commits the whole upload in one compare-and-swap, so nothing here
	// touches the row or what readers see (ptone/scion#4219).
	if uploadID := r.URL.Query().Get("uploadId"); uploadID != "" {
		s.stageHarnessConfigUpload(w, r, hc, filePath, uploadID)
		return
	}

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
	// Refuse before writing the object, so refused content never reaches
	// storage; the commit runs the same check.
	if isHarnessConfigYAMLPath(filePath) && refuseUnusableProvisionerYAML(w, hc.Name, content) {
		return
	}
	// Store the content as a blob, then commit it.
	fileHash, blobPath, err := s.writeHarnessConfigFileBlob(ctx, stor, hc, content)
	if err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}
	fileSize := int64(len(content))

	next := upsertTemplateFile(hc.Files, store.TemplateFile{Path: filePath, Size: fileSize, Hash: fileHash, Mode: newFileMode(hc.Files, filePath)})
	if err := s.commitHarnessConfigFiles(ctx, hc, next, harnessConfigCommitOpts{
		content: map[string][]byte{filePath: content},
		written: map[string]bool{blobPath: true},
	}); err != nil {
		writeHarnessConfigCommitError(w, err)
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

	cleanup, err := parseMultipartUpload(r, s.uploadFormMaxMemory())
	defer cleanup()
	if err != nil {
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

	next := hc.Files
	written := make(map[string]bool, len(partPaths))
	content := make(map[string][]byte, len(partPaths))
	entries := make([]HarnessConfigFileEntry, 0, len(partPaths))
	for _, partPath := range partPaths {
		headers := r.MultipartForm.File[partPath]
		if err := validateWorkspaceFilePath(partPath); err != nil {
			BadRequest(w, fmt.Sprintf("Invalid file path %q: %s", partPath, err.Error()))
			return
		}
		if len(headers) == 0 {
			continue
		}
		filePath := path.Clean(partPath)

		data, err := readMultipartFile(headers[0])
		if err != nil {
			BadRequest(w, "Failed to read multipart file: "+err.Error())
			return
		}

		// Store the content as a blob; the commit below makes it visible.
		fileHash, blobPath, err := s.writeHarnessConfigFileBlob(ctx, stor, hc, data)
		if err != nil {
			RuntimeError(w, "Failed to write file to storage")
			return
		}
		fileSize := int64(len(data))

		mode := newFileMode(next, filePath)
		next = upsertTemplateFile(next, store.TemplateFile{Path: filePath, Size: fileSize, Hash: fileHash, Mode: mode})
		written[blobPath] = true
		content[filePath] = data

		entries = append(entries, HarnessConfigFileEntry{
			Path:    filePath,
			Size:    fileSize,
			ModTime: hc.Updated.UTC().Format("2006-01-02T15:04:05Z"),
			Mode:    mode,
		})
	}

	if err := s.commitHarnessConfigFiles(ctx, hc, next, harnessConfigCommitOpts{content: content, written: written}); err != nil {
		writeHarnessConfigCommitError(w, err)
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

// unusableProvisionerImportError is returned by the import driver when one or
// more discovered harness-configs declare an unusable provisioner block. The
// import is refused before anything is persisted.
type unusableProvisionerImportError struct {
	// perrs holds one refusal per unusable harness-config, ordered by name.
	perrs []*harness.UnusableProvisionerError
}

// Error returns the public message, which carries no filesystem paths, since
// the streaming import endpoints send it to the client as-is.
func (e *unusableProvisionerImportError) Error() string { return e.publicMessage() }

// publicMessage joins the public message of every refused harness-config.
func (e *unusableProvisionerImportError) publicMessage() string {
	msgs := make([]string, 0, len(e.perrs))
	for _, perr := range e.perrs {
		msgs = append(msgs, perr.PublicMessage())
	}
	return strings.Join(msgs, " ")
}

// writeUnusableProvisionerImportError writes the finalize-equivalent 422 and
// returns true when err is an unusableProvisionerImportError.
func writeUnusableProvisionerImportError(w http.ResponseWriter, err error) bool {
	var ierr *unusableProvisionerImportError
	if errors.As(err, &ierr) {
		writeError(w, http.StatusUnprocessableEntity, harnessConfigUnusableErrorCode, ierr.publicMessage(), nil)
		return true
	}
	return false
}

// checkHarnessConfigDirsProvisioner applies unusableProvisionerInYAML to the
// config.yaml of every discovered harness-config directory and returns one
// unusableProvisionerImportError naming all refused configs, ordered by name,
// or nil when none is refused. A config.yaml that cannot be read is left to
// the existing import handling.
func checkHarnessConfigDirsProvisioner(dirs []resourceDir) error {
	var perrs []*harness.UnusableProvisionerError
	for _, rd := range dirs {
		data, err := readLocalHarnessConfigFile(rd.path, "config.yaml")
		if err != nil {
			continue
		}
		if perr := unusableProvisionerInYAML(rd.name, data); perr != nil {
			perrs = append(perrs, perr)
		}
	}
	if len(perrs) == 0 {
		return nil
	}
	slices.SortStableFunc(perrs, func(a, b *harness.UnusableProvisionerError) int {
		return strings.Compare(a.Name, b.Name)
	})
	return &unusableProvisionerImportError{perrs: perrs}
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

	// A harness-config without config.yaml is invalid (ptone/scion#4219).
	if isHarnessConfigYAMLPath(filePath) {
		writeError(w, http.StatusBadRequest, ErrCodeHarnessConfigYAMLRequired,
			"config.yaml cannot be deleted: a harness config needs it; edit it instead", nil)
		return
	}

	ctx := r.Context()

	if _, ok := templateFileByPath(hc.Files, filePath); !ok {
		NotFound(w, "Harness config file")
		return
	}

	if s.GetStorage() == nil {
		RuntimeError(w, "Storage not configured")
		return
	}

	// The commit drops the entry from the manifest and re-derives the
	// index. The removed file's blob stays for in-flight hydrations until
	// the blob garbage collector's grace period has passed.
	if err := s.commitHarnessConfigFiles(ctx, hc, removeTemplateFile(hc.Files, filePath), harnessConfigCommitOpts{}); err != nil {
		writeHarnessConfigCommitError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"path":    filePath,
		"message": fmt.Sprintf("Deleted %s", filePath),
	})
}

// stageHarnessConfigUpload stores the request body as the staged file
// filePath of upload uploadID (a local-storage upload URL). It writes
// nothing but the staged object: the finalize commit hashes and moves it.
// The body may be as large as a signed-URL upload allows.
func (s *Server) stageHarnessConfigUpload(w http.ResponseWriter, r *http.Request, hc *store.HarnessConfig, filePath, uploadID string) {
	if !validTemplateUploadID(uploadID) {
		BadRequest(w, "Invalid uploadId")
		return
	}
	stor := s.GetStorage()
	if stor == nil {
		RuntimeError(w, "Storage not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadFileSize+1)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "File too large for upload.", nil)
			return
		}
		BadRequest(w, "Failed to read request body")
		return
	}
	if _, err := stor.Upload(r.Context(), s.harnessConfigStagedObjectPath(hc, uploadID, filePath), bytes.NewReader(data), storage.UploadOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		RuntimeError(w, "Failed to write file to storage")
		return
	}
	w.WriteHeader(http.StatusOK)
}
