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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"

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

func (s *Server) handleHarnessConfigFiles(w http.ResponseWriter, r *http.Request, id, filePath string) {
	if filePath == "" {
		switch r.Method {
		case http.MethodGet:
			s.handleHarnessConfigFileList(w, r, id)
		case http.MethodPost:
			s.handleHarnessConfigFileUpload(w, r, id)
		default:
			MethodNotAllowed(w)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleHarnessConfigFileRead(w, r, id, filePath)
	case http.MethodPut:
		s.handleHarnessConfigFileWrite(w, r, id, filePath)
	case http.MethodDelete:
		s.handleHarnessConfigFileDelete(w, r, id, filePath)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) handleHarnessConfigFileList(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

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

func (s *Server) handleHarnessConfigFileRead(w http.ResponseWriter, r *http.Request, id, filePath string) {
	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
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
	defer reader.Close()

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

func (s *Server) handleHarnessConfigFileWrite(w http.ResponseWriter, r *http.Request, id, filePath string) {
	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if hc.Locked {
		Forbidden(w)
		return
	}

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
	objectPath := hc.StoragePath + "/" + filePath
	_, err = stor.Upload(ctx, objectPath, strings.NewReader(req.Content), storage.UploadOptions{
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

func (s *Server) handleHarnessConfigFileUpload(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if hc.Locked {
		Forbidden(w)
		return
	}

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		BadRequest(w, "Failed to parse multipart form: "+err.Error())
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

	files := hc.Files
	entries := make([]HarnessConfigFileEntry, 0, len(r.MultipartForm.File))
	for filePath, headers := range r.MultipartForm.File {
		if len(headers) == 0 {
			continue
		}

		src, err := headers[0].Open()
		if err != nil {
			BadRequest(w, "Failed to open multipart file: "+err.Error())
			return
		}
		data, err := io.ReadAll(src)
		src.Close()
		if err != nil {
			BadRequest(w, "Failed to read multipart file: "+err.Error())
			return
		}

		objectPath := hc.StoragePath + "/" + filePath
		_, err = stor.Upload(ctx, objectPath, strings.NewReader(string(data)), storage.UploadOptions{})
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
	if err := s.store.UpdateHarnessConfig(ctx, hc); err != nil {
		RuntimeError(w, "Failed to update harness config manifest")
		return
	}

	writeJSON(w, http.StatusOK, HarnessConfigFileUploadResponse{
		Files: entries,
		Hash:  hc.ContentHash,
	})
}

func (s *Server) handleHarnessConfigFileDelete(w http.ResponseWriter, r *http.Request, id, filePath string) {
	ctx := r.Context()

	hc, err := s.store.GetHarnessConfig(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if hc.Locked {
		Forbidden(w)
		return
	}

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
