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
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestHandleTemplateFileList(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md":    "# Agent",
		"home/.bashrc": "export FOO=bar",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/files", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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

	// Verify storage was updated: the write migrated the legacy row to the
	// blob layout and stored the new content as a blob.
	if updated.Layout != store.TemplateLayoutBlobs {
		t.Errorf("expected blob layout after write, got %q", updated.Layout)
	}
	storedContent := stor.content[blobObjectPath(updated, "# Updated Content\n\nNew instructions.")]
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
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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

func TestHandleTemplateFileWrite_ConflictHash(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	body := `{"content": "new", "expectedHash": "sha256:wronghash"}`
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/templates/%s/files/CLAUDE.md", tid("tmpl-test-1")),
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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

	// The commit migrated the legacy row: the remaining file is a blob, and
	// the removed file was not copied in. Commits no longer delete objects
	// themselves (the blob garbage collector does, later).
	if string(stor.content[blobObjectPath(updated, "# Agent")]) != "# Agent" {
		t.Error("expected remaining file to be stored as a blob")
	}
	if _, ok := stor.content[blobObjectPath(updated, "# bashrc")]; ok {
		t.Error("expected removed file not to be copied into the blob store")
	}
}

func TestHandleTemplateFileDelete_NotFound(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)

	createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md": "# Agent",
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/templates/tmpl-test-1/files/nonexistent.md", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
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
	stored := stor.content[blobObjectPath(updated, "key: value\n")]
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
	req.Header.Set("Authorization", "Bearer "+testDevToken)
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
	stored := stor.content[blobObjectPath(updated, "# New Content")]
	if string(stored) != "# New Content" {
		t.Errorf("unexpected stored content: %s", string(stored))
	}
}

func TestHandleTemplateFileWrite_UpdatesHarness(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"scion-agent.yaml": "harness_config: claude\n",
	})

	// Write a new scion-agent.yaml that changes the harness
	body := `{"content": "default_harness_config: gemini-web\n"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get updated template: %v", err)
	}
	if updated.Harness != "gemini-cli" {
		t.Errorf("expected harness 'gemini-cli', got %q", updated.Harness)
	}
	if updated.DefaultHarnessConfig != "gemini-web" {
		t.Errorf("expected defaultHarnessConfig 'gemini-web', got %q", updated.DefaultHarnessConfig)
	}
}

// A write to another file re-derives the index from the stored
// scion-agent.yaml, which is unchanged, so the index stays the same.
func TestHandleTemplateFileWrite_NonConfigFileDoesNotChangeHarness(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"CLAUDE.md":        "# Agent",
		"scion-agent.yaml": "default_harness_config: claude-web\n",
	})

	body := `{"content": "default_harness_config: gemini\n"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/templates/"+tmpl.ID+"/files/CLAUDE.md",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get updated template: %v", err)
	}
	if updated.Harness != "claude" {
		t.Errorf("expected harness to remain 'claude', got %q", updated.Harness)
	}
	if updated.DefaultHarnessConfig != "claude-web" {
		t.Errorf("expected defaultHarnessConfig 'claude-web', got %q", updated.DefaultHarnessConfig)
	}
}

func TestHandleTemplateFileDelete_ResetsHarness(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	ctx := context.Background()

	tmpl := createTestTemplate(t, s, stor, map[string]string{
		"scion-agent.yaml": "default_harness_config: gemini-web\n",
		"CLAUDE.md":        "# Agent",
	})

	// Update harness to match config file
	tmpl.Harness = "gemini"
	if err := setTemplateContentForTest(ctx, s, tmpl); err != nil {
		t.Fatalf("failed to update template: %v", err)
	}

	// Delete the config file
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/templates/"+tmpl.ID+"/files/scion-agent.yaml", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	updated, err := s.GetTemplate(ctx, tmpl.ID)
	if err != nil {
		t.Fatalf("failed to get updated template: %v", err)
	}
	// Template name is "test-template" which doesn't match any known harness
	if updated.Harness != "" {
		t.Errorf("expected empty harness after config deletion, got %q", updated.Harness)
	}
}
