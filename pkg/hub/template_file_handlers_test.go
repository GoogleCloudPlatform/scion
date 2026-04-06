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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

// contentMockStorage extends mockStorage to also store file content for
// Download support in template file handler tests.
type contentMockStorage struct {
	mockStorage
	content map[string][]byte
}

func newContentMockStorage(bucket string) *contentMockStorage {
	return &contentMockStorage{
		mockStorage: mockStorage{
			bucket:  bucket,
			objects: make(map[string]*storage.Object),
		},
		content: make(map[string][]byte),
	}
}

func (m *contentMockStorage) Upload(_ context.Context, objectPath string, reader io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	obj := &storage.Object{
		Name:     objectPath,
		Size:     int64(len(data)),
		Metadata: opts.Metadata,
	}
	m.objects[objectPath] = obj
	m.content[objectPath] = data
	return obj, nil
}

func (m *contentMockStorage) Download(_ context.Context, objectPath string) (io.ReadCloser, *storage.Object, error) {
	data, ok := m.content[objectPath]
	if !ok {
		return nil, nil, storage.ErrNotFound
	}
	obj := m.objects[objectPath]
	return io.NopCloser(bytes.NewReader(data)), obj, nil
}

func (m *contentMockStorage) Delete(_ context.Context, objectPath string) error {
	delete(m.objects, objectPath)
	delete(m.content, objectPath)
	return nil
}

func (m *contentMockStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	_, ok := m.content[objectPath]
	return ok, nil
}

// testTemplateFileServer creates a Server with content-aware mock storage.
func testTemplateFileServer(t *testing.T) (*Server, store.Store, *contentMockStorage) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping: sqlite driver not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	cfg := DefaultServerConfig()
	srv := New(cfg, s)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	return srv, s, stor
}

// createTestTemplate creates a template in the store with the given files
// pre-populated in storage.
func createTestTemplate(t *testing.T, s store.Store, stor *contentMockStorage, files map[string]string) *store.Template {
	t.Helper()
	ctx := context.Background()

	tmpl := &store.Template{
		ID:            "tmpl-test-1",
		Name:          "test-template",
		Slug:          "test-template",
		Harness:       "claude",
		Scope:         "global",
		Status:        store.TemplateStatusActive,
		StoragePath:   "templates/global/test-template",
		StorageBucket: "test-bucket",
		Updated:       time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC),
	}

	templateFiles := make([]store.TemplateFile, 0, len(files))
	for path, content := range files {
		objectPath := tmpl.StoragePath + "/" + path
		stor.content[objectPath] = []byte(content)
		stor.objects[objectPath] = &storage.Object{
			Name: objectPath,
			Size: int64(len(content)),
		}

		templateFiles = append(templateFiles, store.TemplateFile{
			Path: path,
			Size: int64(len(content)),
			Hash: "sha256:placeholder",
		})
	}
	tmpl.Files = templateFiles
	tmpl.ContentHash = computeContentHash(templateFiles)

	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to create test template: %v", err)
	}

	return tmpl
}

func TestHandleTemplateFileList(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md":    "# Agent",
		"home/.bashrc": "export FOO=bar",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/files", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp TemplateFileListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TotalCount != 2 {
		t.Errorf("expected 2 files, got %d", resp.TotalCount)
	}

	// Verify files are present
	paths := make(map[string]bool)
	for _, f := range resp.Files {
		paths[f.Path] = true
	}
	if !paths["CLAUDE.md"] || !paths["home/.bashrc"] {
		t.Errorf("expected CLAUDE.md and home/.bashrc in response, got %v", paths)
	}
}

func TestHandleTemplateFileRead(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# My Agent\n\nInstructions here.",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/files/CLAUDE.md", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp TemplateFileContentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Path != "CLAUDE.md" {
		t.Errorf("expected path CLAUDE.md, got %s", resp.Path)
	}
	if resp.Content != "# My Agent\n\nInstructions here." {
		t.Errorf("unexpected content: %s", resp.Content)
	}
	if resp.Encoding != "utf-8" {
		t.Errorf("expected encoding utf-8, got %s", resp.Encoding)
	}
}

func TestHandleTemplateFileRead_NotFound(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "content",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/files/nonexistent.md", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTemplateFileWrite(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Old Content",
	})

	body := `{"content": "# Updated Content\n\nNew instructions."}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/CLAUDE.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp TemplateFileWriteResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Path != "CLAUDE.md" {
		t.Errorf("expected path CLAUDE.md, got %s", resp.Path)
	}
	if !strings.HasPrefix(resp.Hash, "sha256:") {
		t.Errorf("expected sha256 hash, got %s", resp.Hash)
	}

	// Verify content hash was recomputed in the database
	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get updated template: %v", err)
	}
	if updated.ContentHash == tmpl.ContentHash {
		t.Error("expected content hash to change after file write")
	}

	// Verify storage was updated
	storedContent := stor.content[tmpl.StoragePath+"/CLAUDE.md"]
	if string(storedContent) != "# Updated Content\n\nNew instructions." {
		t.Errorf("unexpected stored content: %s", string(storedContent))
	}
}

func TestHandleTemplateFileWrite_NewFile(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	body := `{"content": "export PATH=$PATH:/usr/local/bin"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/home/.bashrc",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify file was added to manifest
	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get updated template: %v", err)
	}
	if len(updated.Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(updated.Files))
	}
}

func TestHandleTemplateFileWrite_LockedTemplate(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	// Lock the template
	tmpl.Locked = true
	if err := s.UpdateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to lock template: %v", err)
	}

	body := `{"content": "new content"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/CLAUDE.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTemplateFileWrite_ConflictHash(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	body := `{"content": "new", "expectedHash": "sha256:wronghash"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/tmpl-test-1/files/CLAUDE.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTemplateFileDelete(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md":    "# Agent",
		"home/.bashrc": "# bashrc",
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/home/.bashrc", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify file was removed from manifest
	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get updated template: %v", err)
	}
	if len(updated.Files) != 1 {
		t.Errorf("expected 1 file after delete, got %d", len(updated.Files))
	}
	if updated.Files[0].Path != "CLAUDE.md" {
		t.Errorf("expected remaining file to be CLAUDE.md, got %s", updated.Files[0].Path)
	}

	// Verify removed from storage
	if _, ok := stor.content[tmpl.StoragePath+"/home/.bashrc"]; ok {
		t.Error("expected file to be removed from storage")
	}
}

func TestHandleTemplateFileDelete_LockedTemplate(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	tmpl.Locked = true
	if err := s.UpdateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to lock template: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/CLAUDE.md", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTemplateFileDelete_NotFound(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/templates/tmpl-test-1/files/nonexistent.md", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// templateMultipartRequest creates a multipart form request for template file upload tests.
func templateMultipartRequest(t *testing.T, templateID string, files map[string][]byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for fieldName, content := range files {
		part, err := writer.CreateFormFile(fieldName, fieldName)
		if err != nil {
			t.Fatalf("failed to create form file: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("failed to write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/templates/"+templateID+"/files", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestHandleTemplateFileUpload(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	req := templateMultipartRequest(t, tmpl.ID, map[string][]byte{
		"config.yaml": []byte("key: value\n"),
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp TemplateFileUploadResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Files) != 1 {
		t.Fatalf("expected 1 uploaded file, got %d", len(resp.Files))
	}
	if resp.Files[0].Path != "config.yaml" {
		t.Errorf("expected path config.yaml, got %s", resp.Files[0].Path)
	}
	if resp.Hash == "" {
		t.Error("expected non-empty content hash")
	}

	// Verify manifest updated
	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get template: %v", err)
	}
	if len(updated.Files) != 2 {
		t.Errorf("expected 2 files in manifest, got %d", len(updated.Files))
	}

	// Verify storage
	stored := stor.content[tmpl.StoragePath+"/config.yaml"]
	if string(stored) != "key: value\n" {
		t.Errorf("unexpected stored content: %s", string(stored))
	}
}

func TestHandleTemplateFileUpload_MultipleFiles(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{})

	req := templateMultipartRequest(t, tmpl.ID, map[string][]byte{
		"CLAUDE.md":    []byte("# Instructions"),
		"home/.bashrc": []byte("export FOO=bar"),
		"config.json":  []byte(`{"setting": true}`),
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp TemplateFileUploadResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Files) != 3 {
		t.Errorf("expected 3 uploaded files, got %d", len(resp.Files))
	}

	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get template: %v", err)
	}
	if len(updated.Files) != 3 {
		t.Errorf("expected 3 files in manifest, got %d", len(updated.Files))
	}
}

func TestHandleTemplateFileUpload_LockedTemplate(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	tmpl.Locked = true
	if err := s.UpdateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to lock template: %v", err)
	}

	req := templateMultipartRequest(t, tmpl.ID, map[string][]byte{
		"config.yaml": []byte("key: value"),
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTemplateFileUpload_NoFiles(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	// Send an empty multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/templates/"+tmpl.ID+"/files", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleTemplateFileUpload_OverwriteExisting(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Old Content",
	})

	oldHash := tmpl.ContentHash

	req := templateMultipartRequest(t, tmpl.ID, map[string][]byte{
		"CLAUDE.md": []byte("# New Content"),
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify manifest has exactly 1 file (not duplicated)
	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get template: %v", err)
	}
	if len(updated.Files) != 1 {
		t.Errorf("expected 1 file (no duplicate), got %d", len(updated.Files))
	}

	// Verify content hash changed
	if updated.ContentHash == oldHash {
		t.Error("expected content hash to change after overwrite")
	}

	// Verify storage updated
	stored := stor.content[tmpl.StoragePath+"/CLAUDE.md"]
	if string(stored) != "# New Content" {
		t.Errorf("unexpected stored content: %s", string(stored))
	}
}
