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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestWorkspaceSyncFrom(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/agent-123/workspace/sync-from" {
			t.Errorf("expected path /api/v1/agents/agent-123/workspace/sync-from, got %s", r.URL.Path)
		}

		// Check request body if provided
		if r.ContentLength > 0 {
			var req struct {
				ExcludePatterns []string `json:"excludePatterns"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("failed to decode request: %v", err)
			}
			if len(req.ExcludePatterns) != 1 || req.ExcludePatterns[0] != ".git/**" {
				t.Errorf("expected excludePatterns ['.git/**'], got %v", req.ExcludePatterns)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncFromResponse{
			Manifest: &transfer.Manifest{
				Version:     "1.0",
				ContentHash: "sha256:abc123",
				Files: []transfer.FileInfo{
					{Path: "src/main.go", Size: 1024, Hash: "sha256:def456"},
					{Path: "README.md", Size: 256, Hash: "sha256:789abc"},
				},
			},
			DownloadURLs: []transfer.DownloadURLInfo{
				{Path: "src/main.go", URL: "https://storage.example.com/main.go", Size: 1024, Hash: "sha256:def456"},
				{Path: "README.md", URL: "https://storage.example.com/readme.md", Size: 256, Hash: "sha256:789abc"},
			},
			Expires: time.Now().Add(15 * time.Minute),
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Workspace().SyncFrom(context.Background(), "agent-123", &SyncFromOptions{
		ExcludePatterns: []string{".git/**"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Manifest == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(resp.Manifest.Files) != 2 {
		t.Errorf("expected 2 files in manifest, got %d", len(resp.Manifest.Files))
	}
	if len(resp.DownloadURLs) != 2 {
		t.Errorf("expected 2 download URLs, got %d", len(resp.DownloadURLs))
	}
	if resp.DownloadURLs[0].Path != "src/main.go" {
		t.Errorf("expected first file path 'src/main.go', got %q", resp.DownloadURLs[0].Path)
	}
}

func TestWorkspaceSyncTo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/agent-456/workspace/sync-to" {
			t.Errorf("expected path /api/v1/agents/agent-456/workspace/sync-to, got %s", r.URL.Path)
		}

		var req struct {
			Files []transfer.FileInfo `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if len(req.Files) != 3 {
			t.Errorf("expected 3 files, got %d", len(req.Files))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncToResponse{
			UploadURLs: []transfer.UploadURLInfo{
				{Path: "src/main.go", URL: "https://storage.example.com/upload/main.go", Method: "PUT"},
				{Path: "src/lib.go", URL: "https://storage.example.com/upload/lib.go", Method: "PUT"},
			},
			ExistingFiles: []string{"README.md"}, // Already exists, skip upload
			Expires:       time.Now().Add(15 * time.Minute),
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	files := []transfer.FileInfo{
		{Path: "src/main.go", Size: 1024, Hash: "sha256:new123"},
		{Path: "src/lib.go", Size: 512, Hash: "sha256:new456"},
		{Path: "README.md", Size: 256, Hash: "sha256:existing"},
	}

	resp, err := client.Workspace().SyncTo(context.Background(), "agent-456", files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.UploadURLs) != 2 {
		t.Errorf("expected 2 upload URLs, got %d", len(resp.UploadURLs))
	}
	if len(resp.ExistingFiles) != 1 {
		t.Errorf("expected 1 existing file, got %d", len(resp.ExistingFiles))
	}
	if resp.ExistingFiles[0] != "README.md" {
		t.Errorf("expected existing file 'README.md', got %q", resp.ExistingFiles[0])
	}
}

func TestWorkspaceSyncToFinalize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/agent-789/workspace/sync-to/finalize" {
			t.Errorf("expected path /api/v1/agents/agent-789/workspace/sync-to/finalize, got %s", r.URL.Path)
		}

		var req struct {
			Manifest *transfer.Manifest `json:"manifest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		if req.Manifest == nil {
			t.Error("expected non-nil manifest in request")
		}
		if len(req.Manifest.Files) != 2 {
			t.Errorf("expected 2 files in manifest, got %d", len(req.Manifest.Files))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncToFinalizeResponse{
			Applied:          true,
			ContentHash:      "sha256:finalhash",
			FilesApplied:     2,
			BytesTransferred: 1536,
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	manifest := &transfer.Manifest{
		Version: "1.0",
		Files: []transfer.FileInfo{
			{Path: "src/main.go", Size: 1024, Hash: "sha256:abc"},
			{Path: "src/lib.go", Size: 512, Hash: "sha256:def"},
		},
	}

	resp, err := client.Workspace().FinalizeSyncTo(context.Background(), "agent-789", manifest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Applied {
		t.Error("expected applied=true")
	}
	if resp.FilesApplied != 2 {
		t.Errorf("expected 2 files applied, got %d", resp.FilesApplied)
	}
	if resp.BytesTransferred != 1536 {
		t.Errorf("expected 1536 bytes transferred, got %d", resp.BytesTransferred)
	}
	if resp.ContentHash != "sha256:finalhash" {
		t.Errorf("expected content hash 'sha256:finalhash', got %q", resp.ContentHash)
	}
}

func TestWorkspaceGetStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/agent-status/workspace" {
			t.Errorf("expected path /api/v1/agents/agent-status/workspace, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(WorkspaceStatusResponse{
			Slug:       "agent-status",
			ProjectID:  "grove-xyz",
			StorageURI: "gs://bucket/workspaces/grove-xyz/agent-status/",
			LastSync: &WorkspaceSyncInfo{
				Direction:   "from",
				Timestamp:   time.Now().Add(-1 * time.Hour),
				ContentHash: "sha256:lastsync",
				FileCount:   15,
				TotalSize:   102400,
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Workspace().GetStatus(context.Background(), "agent-status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Slug != "agent-status" {
		t.Errorf("expected agent ID 'agent-status', got %q", resp.Slug)
	}
	if resp.ProjectID != "grove-xyz" {
		t.Errorf("expected project ID 'grove-xyz', got %q", resp.ProjectID)
	}
	if resp.StorageURI == "" {
		t.Error("expected non-empty storage URI")
	}
	if resp.LastSync == nil {
		t.Fatal("expected non-nil LastSync")
	}
	if resp.LastSync.Direction != "from" {
		t.Errorf("expected direction 'from', got %q", resp.LastSync.Direction)
	}
	if resp.LastSync.FileCount != 15 {
		t.Errorf("expected 15 files, got %d", resp.LastSync.FileCount)
	}
}

func TestWorkspaceSyncFromNilOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// With nil options, request body should be nil or empty
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncFromResponse{
			Manifest: &transfer.Manifest{
				Version: "1.0",
				Files:   []transfer.FileInfo{},
			},
			DownloadURLs: []transfer.DownloadURLInfo{},
			Expires:      time.Now().Add(15 * time.Minute),
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Workspace().SyncFrom(context.Background(), "agent-empty", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Manifest == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(resp.Manifest.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(resp.Manifest.Files))
	}
}

func TestWorkspaceServiceAvailable(t *testing.T) {
	client, err := New("https://hub.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.Workspace() == nil {
		t.Error("expected non-nil workspace service")
	}
}

// Error handling tests

func TestWorkspaceSyncFrom_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "not_found",
				"message": "Agent not found",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	_, err := client.Workspace().SyncFrom(context.Background(), "nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestWorkspaceSyncFrom_Conflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "conflict",
				"message": "Agent is not running",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	_, err := client.Workspace().SyncFrom(context.Background(), "stopped-agent", nil)
	if err == nil {
		t.Fatal("expected error for 409 response")
	}
}

func TestWorkspaceSyncTo_BadRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "validation_error",
				"message": "files list is required",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	_, err := client.Workspace().SyncTo(context.Background(), "agent-123", []transfer.FileInfo{})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestWorkspaceSyncToFinalize_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "unauthorized",
				"message": "Invalid or missing authorization token",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	manifest := &transfer.Manifest{Version: "1.0", Files: []transfer.FileInfo{}}
	_, err := client.Workspace().FinalizeSyncTo(context.Background(), "agent-123", manifest)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestWorkspaceGetStatus_InternalServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "internal_error",
				"message": "Database error",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	_, err := client.Workspace().GetStatus(context.Background(), "agent-123")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestWorkspaceSyncFrom_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid json`))
	}))
	defer server.Close()

	client, _ := New(server.URL)
	_, err := client.Workspace().SyncFrom(context.Background(), "agent-123", nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestWorkspaceSyncTo_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not json at all`))
	}))
	defer server.Close()

	client, _ := New(server.URL)
	files := []transfer.FileInfo{{Path: "test.txt", Size: 100, Hash: "sha256:abc"}}
	_, err := client.Workspace().SyncTo(context.Background(), "agent-123", files)
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestWorkspaceGetStatus_GatewayTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "gateway_timeout",
				"message": "Runtime Broker unreachable",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	_, err := client.Workspace().GetStatus(context.Background(), "agent-123")
	if err == nil {
		t.Fatal("expected error for 504 response")
	}
}

func TestWorkspaceSyncFrom_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay response to allow context cancellation
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, _ := New(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := client.Workspace().SyncFrom(ctx, "agent-123", nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestWorkspaceSyncTo_Forbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "forbidden",
				"message": "Access denied to agent",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	files := []transfer.FileInfo{{Path: "test.txt", Size: 100, Hash: "sha256:abc"}}
	_, err := client.Workspace().SyncTo(context.Background(), "agent-123", files)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestWorkspaceSyncToFinalize_BadGateway(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "runtime_error",
				"message": "Failed to apply workspace",
			},
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	manifest := &transfer.Manifest{
		Version: "1.0",
		Files:   []transfer.FileInfo{{Path: "test.txt", Size: 100, Hash: "sha256:abc"}},
	}
	_, err := client.Workspace().FinalizeSyncTo(context.Background(), "agent-123", manifest)
	if err == nil {
		t.Fatal("expected error for 502 response")
	}
}

func TestWorkspaceGetStatus_NoLastSync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(WorkspaceStatusResponse{
			Slug:       "agent-new",
			ProjectID:  "project-1",
			StorageURI: "gs://bucket/workspaces/project-1/agent-new/",
			LastSync:   nil, // No sync yet
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Workspace().GetStatus(context.Background(), "agent-new")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.LastSync != nil {
		t.Error("expected nil LastSync for new agent")
	}
	if resp.Slug != "agent-new" {
		t.Errorf("agent ID = %q, want %q", resp.Slug, "agent-new")
	}
}

func TestWorkspaceSyncFrom_EmptyManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncFromResponse{
			Manifest: &transfer.Manifest{
				Version:     "1.0",
				ContentHash: "",
				Files:       []transfer.FileInfo{}, // Empty workspace
			},
			DownloadURLs: []transfer.DownloadURLInfo{},
			Expires:      time.Now().Add(15 * time.Minute),
		})
	}))
	defer server.Close()

	client, _ := New(server.URL)
	resp, err := client.Workspace().SyncFrom(context.Background(), "empty-agent", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Manifest == nil {
		t.Fatal("expected non-nil manifest")
	}
	if len(resp.Manifest.Files) != 0 {
		t.Errorf("expected empty files list, got %d", len(resp.Manifest.Files))
	}
	if len(resp.DownloadURLs) != 0 {
		t.Errorf("expected empty download URLs, got %d", len(resp.DownloadURLs))
	}
}
