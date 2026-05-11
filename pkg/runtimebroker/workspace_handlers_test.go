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

package runtimebroker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// mockAgentManager implements agent.Manager for testing workspace handlers.
type mockAgentManager struct {
	agents []api.AgentInfo
}

func (m *mockAgentManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	return nil, nil
}

func (m *mockAgentManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	return nil, nil
}

func (m *mockAgentManager) Stop(ctx context.Context, name string) error {
	return nil
}

func (m *mockAgentManager) Delete(ctx context.Context, name string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	return true, nil
}

func (m *mockAgentManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	return m.agents, nil
}

func (m *mockAgentManager) Message(ctx context.Context, name, projectID, message string, interrupt bool) error {
	return nil
}

func (m *mockAgentManager) MessageRaw(ctx context.Context, name, projectID string, keys string) error {
	return nil
}

func (m *mockAgentManager) Watch(ctx context.Context, name string) (<-chan api.StatusEvent, error) {
	return nil, nil
}

func (m *mockAgentManager) Close() {}

// Ensure mockAgentManager implements agent.Manager
var _ agent.Manager = (*mockAgentManager)(nil)

func TestWorkspaceUploadValidation(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	tests := []struct {
		name       string
		body       interface{}
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing agentId",
			body:       WorkspaceUploadRequest{StoragePath: "workspaces/g/a"},
			wantStatus: http.StatusBadRequest,
			wantCode:   ErrCodeValidationError,
		},
		{
			name:       "missing storagePath",
			body:       WorkspaceUploadRequest{Slug: "test-agent"},
			wantStatus: http.StatusBadRequest,
			wantCode:   ErrCodeValidationError,
		},
		{
			name:       "missing bucket when not configured",
			body:       WorkspaceUploadRequest{Slug: "test-agent", StoragePath: "workspaces/g/a"},
			wantStatus: http.StatusBadRequest,
			wantCode:   ErrCodeValidationError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			srv.handleWorkspaceUpload(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", rec.Code, tt.wantStatus)
			}

			var errResp ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("failed to decode error response: %v", err)
			}

			if errResp.Error.Code != tt.wantCode {
				t.Errorf("got error code %s, want %s", errResp.Error.Code, tt.wantCode)
			}
		})
	}
}

func TestWorkspaceApplyValidation(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	tests := []struct {
		name       string
		body       interface{}
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing agentId",
			body:       WorkspaceApplyRequest{StoragePath: "workspaces/g/a"},
			wantStatus: http.StatusBadRequest,
			wantCode:   ErrCodeValidationError,
		},
		{
			name:       "missing storagePath",
			body:       WorkspaceApplyRequest{Slug: "test-agent"},
			wantStatus: http.StatusBadRequest,
			wantCode:   ErrCodeValidationError,
		},
		{
			name:       "missing bucket when not configured",
			body:       WorkspaceApplyRequest{Slug: "test-agent", StoragePath: "workspaces/g/a"},
			wantStatus: http.StatusBadRequest,
			wantCode:   ErrCodeValidationError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/apply", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			srv.handleWorkspaceApply(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", rec.Code, tt.wantStatus)
			}

			var errResp ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("failed to decode error response: %v", err)
			}

			if errResp.Error.Code != tt.wantCode {
				t.Errorf("got error code %s, want %s", errResp.Error.Code, tt.wantCode)
			}
		})
	}
}

func TestWorkspaceUploadMethodNotAllowed(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/v1/workspace/upload", nil)
			rec := httptest.NewRecorder()

			srv.handleWorkspaceUpload(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestWorkspaceApplyMethodNotAllowed(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/v1/workspace/apply", nil)
			rec := httptest.NewRecorder()

			srv.handleWorkspaceApply(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("got status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestWorkspaceUploadAgentNotFound(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StorageBucket = "test-bucket"
	mgr := &mockAgentManager{agents: []api.AgentInfo{}} // No agents
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := WorkspaceUploadRequest{
		Slug:        "nonexistent-agent",
		StoragePath: "workspaces/grove/agent",
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleWorkspaceUpload(rec, req)

	// Agent not found should result in a runtime error (since we can't find the workspace path)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("got status %d, want error status", rec.Code)
	}
}

func TestWorkspaceApplyAgentNotFound(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StorageBucket = "test-bucket"
	mgr := &mockAgentManager{agents: []api.AgentInfo{}} // No agents
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := WorkspaceApplyRequest{
		Slug:        "nonexistent-agent",
		StoragePath: "workspaces/grove/agent",
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/apply", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleWorkspaceApply(rec, req)

	// Agent not found should result in a runtime error (since we can't find the workspace path)
	if rec.Code != http.StatusInternalServerError && rec.Code != http.StatusNotFound {
		t.Errorf("got status %d, want error status", rec.Code)
	}
}

func TestBuildWorkspaceManifest(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Create a temporary directory with test files
	tmpDir, err := os.MkdirTemp("", "workspace-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	testFiles := map[string]string{
		"file1.txt":        "content1",
		"subdir/file2.txt": "content2",
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(tmpDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
	}

	// Build manifest
	manifest, err := srv.buildWorkspaceManifest(tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to build manifest: %v", err)
	}

	// Verify manifest
	if manifest.Version != "1.0" {
		t.Errorf("got version %s, want 1.0", manifest.Version)
	}

	if len(manifest.Files) != 2 {
		t.Errorf("got %d files, want 2", len(manifest.Files))
	}

	// Check that files are present
	fileMap := make(map[string]bool)
	for _, f := range manifest.Files {
		fileMap[f.Path] = true
	}

	for path := range testFiles {
		if !fileMap[path] {
			t.Errorf("missing file in manifest: %s", path)
		}
	}
}

func TestBuildWorkspaceManifestWithExcludes(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Create a temporary directory with test files
	tmpDir, err := os.MkdirTemp("", "workspace-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files including some that should be excluded
	testFiles := map[string]string{
		"file1.txt":               "content1",
		"node_modules/package.js": "content2",
		".git/config":             "content3",
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(tmpDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
	}

	// Build manifest with default excludes
	manifest, err := srv.buildWorkspaceManifest(tmpDir, []string{"node_modules/**"})
	if err != nil {
		t.Fatalf("failed to build manifest: %v", err)
	}

	// Should only have file1.txt (git and node_modules excluded)
	if len(manifest.Files) != 1 {
		t.Errorf("got %d files, want 1 (expected excludes to work)", len(manifest.Files))
	}

	if len(manifest.Files) > 0 && manifest.Files[0].Path != "file1.txt" {
		t.Errorf("got file %s, want file1.txt", manifest.Files[0].Path)
	}
}

func TestCountWorkspaceFiles(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Create a temporary directory with test files
	tmpDir, err := os.MkdirTemp("", "workspace-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	testFiles := map[string]string{
		"file1.txt":        "content1",
		"subdir/file2.txt": "content2content2",
	}

	var expectedSize int64
	for path, content := range testFiles {
		fullPath := filepath.Join(tmpDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
		expectedSize += int64(len(content))
	}

	count, size := srv.countWorkspaceFiles(tmpDir)

	if count != 2 {
		t.Errorf("got count %d, want 2", count)
	}

	if size != expectedSize {
		t.Errorf("got size %d, want %d", size, expectedSize)
	}
}

func TestWorkspaceRoutesRegistered(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	handler := srv.Handler()

	// Test that workspace routes are registered
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/workspace/upload"},
		{http.MethodPost, "/api/v1/workspace/apply"},
	}

	for _, route := range routes {
		t.Run(route.path, func(t *testing.T) {
			// Make a request without body to verify route exists
			req := httptest.NewRequest(route.method, route.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			// Should get bad request (missing body), not 404
			if rec.Code == http.StatusNotFound {
				t.Errorf("route %s %s not registered", route.method, route.path)
			}
		})
	}
}

func TestApplyFilePermissions(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Create a temporary directory with test files
	tmpDir, err := os.MkdirTemp("", "workspace-perm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	testFile := filepath.Join(tmpDir, "test.sh")
	if err := os.WriteFile(testFile, []byte("#!/bin/bash\necho hello"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Apply executable permission
	files := []transfer.FileInfo{
		{Path: "test.sh", Mode: "0755"},
	}

	err = srv.applyFilePermissions(tmpDir, files)
	if err != nil {
		t.Fatalf("applyFilePermissions failed: %v", err)
	}

	// Check file mode
	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}

	expectedMode := os.FileMode(0755)
	if info.Mode().Perm() != expectedMode {
		t.Errorf("file mode = %o, want %o", info.Mode().Perm(), expectedMode)
	}
}

func TestApplyFilePermissions_InvalidMode(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	tmpDir, err := os.MkdirTemp("", "workspace-perm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test file
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Try to apply invalid mode
	files := []transfer.FileInfo{
		{Path: "test.txt", Mode: "invalid"},
	}

	err = srv.applyFilePermissions(tmpDir, files)
	// Should return an error but not panic
	if err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestApplyFilePermissions_EmptyMode(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	tmpDir, err := os.MkdirTemp("", "workspace-perm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test file
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Apply empty mode - should be skipped
	files := []transfer.FileInfo{
		{Path: "test.txt", Mode: ""}, // Empty mode should be skipped
	}

	err = srv.applyFilePermissions(tmpDir, files)
	if err != nil {
		t.Errorf("unexpected error for empty mode: %v", err)
	}

	// Verify file mode is unchanged
	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}

	// Mode should still be 0644 (approximately - depends on umask)
	if info.Mode().Perm()&0600 != 0600 {
		t.Errorf("file mode unexpectedly changed: %o", info.Mode().Perm())
	}
}

func TestApplyFilePermissions_MissingFile(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	tmpDir, err := os.MkdirTemp("", "workspace-perm-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Try to apply permissions to non-existent file
	files := []transfer.FileInfo{
		{Path: "nonexistent.txt", Mode: "0644"},
	}

	err = srv.applyFilePermissions(tmpDir, files)
	// Should return an error
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestWorkspaceUploadRequest_JSONSerialization(t *testing.T) {
	req := WorkspaceUploadRequest{
		Slug:            "agent-123",
		StoragePath:     "workspaces/grove-1/agent-123",
		Bucket:          "my-bucket",
		ExcludePatterns: []string{".git/**", "node_modules/**"},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed WorkspaceUploadRequest
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.Slug != "agent-123" {
		t.Errorf("agent ID = %q, want %q", parsed.Slug, "agent-123")
	}
	if parsed.StoragePath != "workspaces/grove-1/agent-123" {
		t.Errorf("storage path = %q, want %q", parsed.StoragePath, "workspaces/grove-1/agent-123")
	}
	if parsed.Bucket != "my-bucket" {
		t.Errorf("bucket = %q, want %q", parsed.Bucket, "my-bucket")
	}
	if len(parsed.ExcludePatterns) != 2 {
		t.Errorf("exclude patterns count = %d, want 2", len(parsed.ExcludePatterns))
	}
}

func TestWorkspaceUploadResponse_JSONSerialization(t *testing.T) {
	resp := WorkspaceUploadResponse{
		Manifest: &transfer.Manifest{
			Version:     "1.0",
			ContentHash: "sha256:abc123",
			Files: []transfer.FileInfo{
				{Path: "main.go", Size: 1024, Hash: "sha256:def456"},
			},
		},
		UploadedFiles: 1,
		UploadedBytes: 1024,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed WorkspaceUploadResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.UploadedFiles != 1 {
		t.Errorf("uploaded files = %d, want 1", parsed.UploadedFiles)
	}
	if parsed.UploadedBytes != 1024 {
		t.Errorf("uploaded bytes = %d, want 1024", parsed.UploadedBytes)
	}
	if parsed.Manifest == nil {
		t.Fatal("manifest should not be nil")
	}
	if len(parsed.Manifest.Files) != 1 {
		t.Errorf("manifest files count = %d, want 1", len(parsed.Manifest.Files))
	}
}

func TestWorkspaceApplyRequest_JSONSerialization(t *testing.T) {
	req := WorkspaceApplyRequest{
		Slug:        "agent-456",
		StoragePath: "workspaces/grove-2/agent-456",
		Bucket:      "other-bucket",
		Manifest: &transfer.Manifest{
			Version: "1.0",
			Files: []transfer.FileInfo{
				{Path: "app.py", Size: 512, Hash: "sha256:xyz789", Mode: "0755"},
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed WorkspaceApplyRequest
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.Slug != "agent-456" {
		t.Errorf("agent ID = %q, want %q", parsed.Slug, "agent-456")
	}
	if parsed.Manifest == nil {
		t.Fatal("manifest should not be nil")
	}
	if parsed.Manifest.Files[0].Mode != "0755" {
		t.Errorf("file mode = %q, want %q", parsed.Manifest.Files[0].Mode, "0755")
	}
}

func TestWorkspaceApplyResponse_JSONSerialization(t *testing.T) {
	resp := WorkspaceApplyResponse{
		Applied:          true,
		FilesApplied:     15,
		BytesTransferred: 102400,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed WorkspaceApplyResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if !parsed.Applied {
		t.Error("expected applied=true")
	}
	if parsed.FilesApplied != 15 {
		t.Errorf("files applied = %d, want 15", parsed.FilesApplied)
	}
	if parsed.BytesTransferred != 102400 {
		t.Errorf("bytes transferred = %d, want 102400", parsed.BytesTransferred)
	}
}

func TestWorkspaceUpload_InvalidJSON(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	bodyBytes := []byte(`{invalid json`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleWorkspaceUpload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWorkspaceApply_InvalidJSON(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	bodyBytes := []byte(`{invalid json`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/apply", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleWorkspaceApply(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWorkspaceUpload_WithBucketInRequest(t *testing.T) {
	cfg := DefaultServerConfig()
	// No bucket configured on server
	cfg.StorageBucket = ""
	mgr := &mockAgentManager{agents: []api.AgentInfo{}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Bucket provided in request
	body := WorkspaceUploadRequest{
		Slug:        "test-agent",
		StoragePath: "workspaces/grove/agent",
		Bucket:      "request-bucket",
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/upload", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleWorkspaceUpload(rec, req)

	// Should fail because agent is not found, but bucket validation should pass
	// (the error will be about the agent, not the bucket)
	if rec.Code == http.StatusBadRequest {
		var errResp ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err == nil {
			if errResp.Error.Message == "bucket is required (not configured on broker)" {
				t.Error("bucket in request should have been used instead of failing")
			}
		}
	}
}

func TestWorkspaceApply_WithBucketInRequest(t *testing.T) {
	cfg := DefaultServerConfig()
	// No bucket configured on server
	cfg.StorageBucket = ""
	mgr := &mockAgentManager{agents: []api.AgentInfo{}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Bucket provided in request
	body := WorkspaceApplyRequest{
		Slug:        "test-agent",
		StoragePath: "workspaces/grove/agent",
		Bucket:      "request-bucket",
	}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/apply", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleWorkspaceApply(rec, req)

	// Should fail because agent is not found, but bucket validation should pass
	if rec.Code == http.StatusBadRequest {
		var errResp ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err == nil {
			if errResp.Error.Message == "bucket is required (not configured on broker)" {
				t.Error("bucket in request should have been used instead of failing")
			}
		}
	}
}

func TestCountWorkspaceFiles_EmptyDirectory(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Create an empty temp directory
	tmpDir, err := os.MkdirTemp("", "workspace-empty-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	count, size := srv.countWorkspaceFiles(tmpDir)

	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
}

func TestCountWorkspaceFiles_NestedDirectories(t *testing.T) {
	cfg := DefaultServerConfig()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Create temp directory with nested structure
	tmpDir, err := os.MkdirTemp("", "workspace-nested-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create nested files
	testFiles := map[string]string{
		"a.txt":           "content1",
		"dir1/b.txt":      "content22",
		"dir1/dir2/c.txt": "content333",
		"dir3/d.txt":      "content4444",
	}

	var expectedSize int64
	for path, content := range testFiles {
		fullPath := filepath.Join(tmpDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file: %v", err)
		}
		expectedSize += int64(len(content))
	}

	count, size := srv.countWorkspaceFiles(tmpDir)

	if count != 4 {
		t.Errorf("count = %d, want 4", count)
	}
	if size != expectedSize {
		t.Errorf("size = %d, want %d", size, expectedSize)
	}
}

// ============================================================================
// Project Workspace Upload Handler Tests (Phase 3: Linked Project Relay)
// ============================================================================

func TestProjectWorkspaceUpload_MissingProjectID(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := ProjectWorkspaceUploadRequest{
		StoragePath:   "workspaces/test/grove-workspace",
		WorkspacePath: "/tmp/test",
	}

	rec := doProjectUploadRequest(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectWorkspaceUpload_MissingStoragePath(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := ProjectWorkspaceUploadRequest{
		ProjectID:       "grove-123",
		WorkspacePath: "/tmp/test",
	}

	rec := doProjectUploadRequest(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectWorkspaceUpload_MissingWorkspacePath(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := ProjectWorkspaceUploadRequest{
		ProjectID:     "grove-123",
		StoragePath: "workspaces/test/grove-workspace",
	}

	rec := doProjectUploadRequest(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectWorkspaceUpload_NoBucket(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := ProjectWorkspaceUploadRequest{
		ProjectID:       "grove-123",
		StoragePath:   "workspaces/test/grove-workspace",
		WorkspacePath: "/tmp/test",
	}

	rec := doProjectUploadRequest(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectWorkspaceUpload_NonExistentPath(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.StorageBucket = "test-bucket"
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := ProjectWorkspaceUploadRequest{
		ProjectID:       "grove-123",
		StoragePath:   "workspaces/test/grove-workspace",
		WorkspacePath: "/nonexistent/path/12345",
	}

	rec := doProjectUploadRequest(t, srv, body)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProjectWorkspaceUpload_MethodNotAllowed(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	mgr := &mockAgentManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspace/grove-upload", nil)
	rec := httptest.NewRecorder()
	srv.handleProjectWorkspaceUpload(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func doProjectUploadRequest(t *testing.T, srv *Server, body ProjectWorkspaceUploadRequest) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("failed to marshal body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/grove-upload", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleProjectWorkspaceUpload(rec, req)
	return rec
}
