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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// testWorkspaceDevToken is the development token used for workspace testing.
const testWorkspaceDevToken = "scion_dev_workspace_test_token_1234567890"

// testWorkspaceServer creates a test server for workspace handler tests.
func testWorkspaceServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testWorkspaceDevToken
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv, s
}

// createTestProject creates a project for tests that need to create agents.
// It uses projectID to generate unique slug and git remote to avoid unique constraint violations.
func createTestProject(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	project := &store.Project{
		ID:        projectID,
		Slug:      projectID, // Use projectID as slug to ensure uniqueness
		Name:      "Test Project " + projectID,
		GitRemote: "https://github.com/test/" + projectID, // Unique git remote per project
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
}

func TestWorkspaceRoutesParsing(t *testing.T) {
	tests := []struct {
		name           string
		url            string
		expectedID     string
		expectedAction string
	}{
		{
			name:           "workspace status",
			url:            "/api/v1/agents/agent-123/workspace",
			expectedID:     "agent-123",
			expectedAction: "workspace",
		},
		{
			name:           "workspace sync-from",
			url:            "/api/v1/agents/agent-123/workspace/sync-from",
			expectedID:     "agent-123",
			expectedAction: "workspace/sync-from",
		},
		{
			name:           "workspace sync-to",
			url:            "/api/v1/agents/agent-123/workspace/sync-to",
			expectedID:     "agent-123",
			expectedAction: "workspace/sync-to",
		},
		{
			name:           "workspace sync-to finalize",
			url:            "/api/v1/agents/agent-123/workspace/sync-to/finalize",
			expectedID:     "agent-123",
			expectedAction: "workspace/sync-to/finalize",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.url, nil)
			id, action := extractAction(req, "/api/v1/agents")

			if id != tt.expectedID {
				t.Errorf("extractAction() id = %q, want %q", id, tt.expectedID)
			}
			if action != tt.expectedAction {
				t.Errorf("extractAction() action = %q, want %q", action, tt.expectedAction)
			}
		})
	}
}

func TestWorkspaceStatusHandler(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()

	now := time.Now()

	// Create the project first (foreign key dependency)
	createTestProject(t, s, "project_test_1")

	// Create a test agent
	agent := &store.Agent{
		ID:           "agent_workspace_test_1",
		Slug:         "workspace-test-agent",
		Name:         "test-agent",
		ProjectID:      "project_test_1",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Test workspace status endpoint
	req := httptest.NewRequest("GET", "/api/v1/agents/agent_workspace_test_1/workspace", nil)
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("workspace status returned status %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp WorkspaceStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Slug != "agent_workspace_test_1" {
		t.Errorf("response AgentID = %q, want %q", resp.Slug, "agent_workspace_test_1")
	}
	if resp.ProjectID != "project_test_1" {
		t.Errorf("response ProjectID = %q, want %q", resp.ProjectID, "project_test_1")
	}
}

func TestWorkspaceStatusHandler_AgentNotFound(t *testing.T) {
	srv, _ := testWorkspaceServer(t)

	req := httptest.NewRequest("GET", "/api/v1/agents/nonexistent/workspace", nil)
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("workspace status for nonexistent agent returned status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestWorkspaceSyncFromHandler_AgentNotRunning(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_test")

	// Create a stopped agent
	agent := &store.Agent{
		ID:           "agent_stopped_1",
		Slug:         "stopped-agent",
		Name:         "stopped-agent",
		ProjectID:      "project_test",
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/agents/agent_stopped_1/workspace/sync-from", nil)
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 409 Conflict because agent is not running
	if rec.Code != http.StatusConflict {
		t.Errorf("sync-from for stopped agent returned status %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestWorkspaceSyncToHandler_EmptyFiles(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_syncto")

	agent := &store.Agent{
		ID:           "agent_syncto_test",
		Slug:         "sync-to-test-agent",
		Name:         "test-agent",
		ProjectID:      "project_syncto",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Send request with empty files list
	body := `{"files": []}`
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_syncto_test/workspace/sync-to", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 400 Bad Request because files list is required
	if rec.Code != http.StatusBadRequest {
		t.Errorf("sync-to with empty files returned status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWorkspaceSyncToFinalizeHandler_MissingManifest(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_finalize")

	agent := &store.Agent{
		ID:           "agent_finalize_test",
		Slug:         "finalize-test-agent",
		Name:         "test-agent",
		ProjectID:      "project_finalize",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Send request without manifest
	body := `{}`
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_finalize_test/workspace/sync-to/finalize", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 400 Bad Request because manifest is required
	if rec.Code != http.StatusBadRequest {
		t.Errorf("finalize without manifest returned status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWorkspaceRoutesRequireAuth(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_auth")

	agent := &store.Agent{
		ID:           "agent_auth_test",
		Slug:         "auth-test-agent",
		Name:         "test-agent",
		ProjectID:      "project_auth",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	tests := []struct {
		name   string
		method string
		url    string
	}{
		{"workspace status", "GET", "/api/v1/agents/agent_auth_test/workspace"},
		{"sync-from", "POST", "/api/v1/agents/agent_auth_test/workspace/sync-from"},
		{"sync-to", "POST", "/api/v1/agents/agent_auth_test/workspace/sync-to"},
		{"sync-to finalize", "POST", "/api/v1/agents/agent_auth_test/workspace/sync-to/finalize"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.url, nil)
			// No authorization header
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			// Should return 401 Unauthorized (no auth token provided)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s without auth returned status %d, want %d", tt.name, rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestSyncFromResponse_JSONSerialization(t *testing.T) {
	resp := SyncFromResponse{
		Manifest: &transfer.Manifest{
			Version:     "1.0",
			ContentHash: "sha256:abc123",
			Files: []transfer.FileInfo{
				{Path: "src/main.go", Size: 1024, Hash: "sha256:def456"},
			},
		},
		DownloadURLs: []transfer.DownloadURLInfo{
			{Path: "src/main.go", URL: "https://storage.example.com/file", Size: 1024, Hash: "sha256:def456"},
		},
		Expires: time.Date(2026, 2, 3, 10, 45, 0, 0, time.UTC),
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal SyncFromResponse: %v", err)
	}

	var parsed SyncFromResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal SyncFromResponse: %v", err)
	}

	if parsed.Manifest.Version != "1.0" {
		t.Errorf("manifest version = %q, want %q", parsed.Manifest.Version, "1.0")
	}
	if len(parsed.DownloadURLs) != 1 {
		t.Errorf("download URLs count = %d, want 1", len(parsed.DownloadURLs))
	}
}

func TestSyncToResponse_JSONSerialization(t *testing.T) {
	resp := SyncToResponse{
		UploadURLs: []transfer.UploadURLInfo{
			{
				Path:   "src/main.go",
				URL:    "https://storage.example.com/upload",
				Method: "PUT",
				Headers: map[string]string{
					"Content-Type": "application/octet-stream",
				},
			},
		},
		ExistingFiles: []string{"README.md"},
		Expires:       time.Date(2026, 2, 3, 10, 45, 0, 0, time.UTC),
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal SyncToResponse: %v", err)
	}

	var parsed SyncToResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal SyncToResponse: %v", err)
	}

	if len(parsed.UploadURLs) != 1 {
		t.Errorf("upload URLs count = %d, want 1", len(parsed.UploadURLs))
	}
	if len(parsed.ExistingFiles) != 1 || parsed.ExistingFiles[0] != "README.md" {
		t.Errorf("existing files = %v, want [README.md]", parsed.ExistingFiles)
	}
}

func TestWorkspaceSyncFromHandler_StorageNotConfigured(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Use unique IDs for this test
	projectID := "project_nostor_syncfrom"
	agentID := "agent_nostor_syncfrom"

	// Create the project first
	createTestProject(t, s, projectID)

	// Create a running agent (no RuntimeBrokerID to avoid FK constraint)
	agent := &store.Agent{
		ID:           agentID,
		Slug:         "no-storage-agent",
		Name:         "test-agent",
		ProjectID:      projectID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Server has no storage configured - test should return runtime error (502 Bad Gateway)
	req := httptest.NewRequest("POST", "/api/v1/agents/"+agentID+"/workspace/sync-from", nil)
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 502 Bad Gateway because storage is not configured (RuntimeError)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("sync-from without storage returned status %d, want %d; body: %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}

	// Verify error message mentions storage
	body := rec.Body.String()
	if !strings.Contains(body, "Storage not configured") {
		t.Errorf("error message should mention storage not configured, got: %s", body)
	}
}

func TestWorkspaceSyncToHandler_StorageNotConfigured(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_syncto_no_storage")

	agent := &store.Agent{
		ID:           "agent_syncto_no_storage",
		Slug:         "sync-to-no-storage-agent",
		Name:         "test-agent",
		ProjectID:      "project_syncto_no_storage",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Send request with files but no storage configured
	body := `{"files": [{"path": "test.txt", "size": 100, "hash": "sha256:abc123"}]}`
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_syncto_no_storage/workspace/sync-to", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 502 Bad Gateway because storage is not configured (RuntimeError)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("sync-to without storage returned status %d, want %d; body: %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func TestWorkspaceSyncToFinalizeHandler_StorageNotConfigured(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_finalize_no_storage")

	agent := &store.Agent{
		ID:           "agent_finalize_no_storage",
		Slug:         "finalize-no-storage-agent",
		Name:         "test-agent",
		ProjectID:      "project_finalize_no_storage",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Send request with manifest but no storage configured
	body := `{"manifest": {"version": "1.0", "files": [{"path": "test.txt", "size": 100, "hash": "sha256:abc123"}]}}`
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_finalize_no_storage/workspace/sync-to/finalize", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 502 Bad Gateway because storage is not configured (RuntimeError)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("finalize without storage returned status %d, want %d; body: %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func TestWorkspaceSyncToFinalizeHandler_AgentNotRunning(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_finalize_stopped")

	// Create a stopped agent
	agent := &store.Agent{
		ID:           "agent_finalize_stopped",
		Slug:         "finalize-stopped-agent",
		Name:         "stopped-agent",
		ProjectID:      "project_finalize_stopped",
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	body := `{"manifest": {"version": "1.0", "files": [{"path": "test.txt", "size": 100, "hash": "sha256:abc123"}]}}`
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_finalize_stopped/workspace/sync-to/finalize", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 409 Conflict because agent is not running
	if rec.Code != http.StatusConflict {
		t.Errorf("finalize for stopped agent returned status %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestWorkspaceMethodNotAllowed(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_method")

	agent := &store.Agent{
		ID:           "agent_method_test",
		Slug:         "method-test-agent",
		Name:         "test-agent",
		ProjectID:      "project_method",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	tests := []struct {
		name           string
		method         string
		url            string
		expectedStatus int
	}{
		// workspace status - GET only
		{"workspace status with POST", "POST", "/api/v1/agents/agent_method_test/workspace", http.StatusMethodNotAllowed},
		{"workspace status with PUT", "PUT", "/api/v1/agents/agent_method_test/workspace", http.StatusMethodNotAllowed},
		{"workspace status with DELETE", "DELETE", "/api/v1/agents/agent_method_test/workspace", http.StatusMethodNotAllowed},

		// sync-from - POST only
		{"sync-from with GET", "GET", "/api/v1/agents/agent_method_test/workspace/sync-from", http.StatusMethodNotAllowed},
		{"sync-from with PUT", "PUT", "/api/v1/agents/agent_method_test/workspace/sync-from", http.StatusMethodNotAllowed},
		{"sync-from with DELETE", "DELETE", "/api/v1/agents/agent_method_test/workspace/sync-from", http.StatusMethodNotAllowed},

		// sync-to - POST only
		{"sync-to with GET", "GET", "/api/v1/agents/agent_method_test/workspace/sync-to", http.StatusMethodNotAllowed},
		{"sync-to with PUT", "PUT", "/api/v1/agents/agent_method_test/workspace/sync-to", http.StatusMethodNotAllowed},
		{"sync-to with DELETE", "DELETE", "/api/v1/agents/agent_method_test/workspace/sync-to", http.StatusMethodNotAllowed},

		// sync-to/finalize - POST only
		{"finalize with GET", "GET", "/api/v1/agents/agent_method_test/workspace/sync-to/finalize", http.StatusMethodNotAllowed},
		{"finalize with PUT", "PUT", "/api/v1/agents/agent_method_test/workspace/sync-to/finalize", http.StatusMethodNotAllowed},
		{"finalize with DELETE", "DELETE", "/api/v1/agents/agent_method_test/workspace/sync-to/finalize", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.url, nil)
			req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("%s returned status %d, want %d", tt.name, rec.Code, tt.expectedStatus)
			}
		})
	}
}

func TestWorkspaceSyncToHandler_InvalidJSON(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_invalid_json")

	agent := &store.Agent{
		ID:           "agent_invalid_json",
		Slug:         "invalid-json-agent",
		Name:         "test-agent",
		ProjectID:      "project_invalid_json",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Send invalid JSON
	body := `{invalid json`
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_invalid_json/workspace/sync-to", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 400 Bad Request
	if rec.Code != http.StatusBadRequest {
		t.Errorf("sync-to with invalid JSON returned status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWorkspaceSyncToFinalizeHandler_InvalidJSON(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_finalize_invalid")

	agent := &store.Agent{
		ID:           "agent_finalize_invalid",
		Slug:         "finalize-invalid-agent",
		Name:         "test-agent",
		ProjectID:      "project_finalize_invalid",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Send invalid JSON
	body := `{not valid`
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_finalize_invalid/workspace/sync-to/finalize", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 400 Bad Request
	if rec.Code != http.StatusBadRequest {
		t.Errorf("finalize with invalid JSON returned status %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestSyncToFinalizeResponse_JSONSerialization(t *testing.T) {
	resp := SyncToFinalizeResponse{
		Applied:          true,
		ContentHash:      "sha256:abc123",
		FilesApplied:     5,
		BytesTransferred: 10240,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal SyncToFinalizeResponse: %v", err)
	}

	var parsed SyncToFinalizeResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal SyncToFinalizeResponse: %v", err)
	}

	if !parsed.Applied {
		t.Error("expected applied=true")
	}
	if parsed.ContentHash != "sha256:abc123" {
		t.Errorf("content hash = %q, want %q", parsed.ContentHash, "sha256:abc123")
	}
	if parsed.FilesApplied != 5 {
		t.Errorf("files applied = %d, want 5", parsed.FilesApplied)
	}
	if parsed.BytesTransferred != 10240 {
		t.Errorf("bytes transferred = %d, want 10240", parsed.BytesTransferred)
	}
}

func TestWorkspaceStatusResponse_JSONSerialization(t *testing.T) {
	now := time.Now()
	resp := WorkspaceStatusResponse{
		Slug:       "agent-123",
		ProjectID:    "project-456",
		StorageURI: "gs://bucket/workspaces/project-456/agent-123/",
		LastSync: &WorkspaceSyncInfo{
			Direction:   "from",
			Timestamp:   now,
			ContentHash: "sha256:xyz789",
			FileCount:   10,
			TotalSize:   102400,
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal WorkspaceStatusResponse: %v", err)
	}

	var parsed WorkspaceStatusResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal WorkspaceStatusResponse: %v", err)
	}

	if parsed.Slug != "agent-123" {
		t.Errorf("agent ID = %q, want %q", parsed.Slug, "agent-123")
	}
	if parsed.ProjectID != "project-456" {
		t.Errorf("project ID = %q, want %q", parsed.ProjectID, "project-456")
	}
	if parsed.StorageURI != "gs://bucket/workspaces/project-456/agent-123/" {
		t.Errorf("storage URI = %q, want %q", parsed.StorageURI, "gs://bucket/workspaces/project-456/agent-123/")
	}
	if parsed.LastSync == nil {
		t.Fatal("expected non-nil LastSync")
	}
	if parsed.LastSync.Direction != "from" {
		t.Errorf("direction = %q, want %q", parsed.LastSync.Direction, "from")
	}
	if parsed.LastSync.FileCount != 10 {
		t.Errorf("file count = %d, want 10", parsed.LastSync.FileCount)
	}
}

func TestWorkspaceUnknownAction(t *testing.T) {
	srv, s := testWorkspaceServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create the project first
	createTestProject(t, s, "project_unknown")

	agent := &store.Agent{
		ID:           "agent_unknown_action",
		Slug:         "unknown-action-agent",
		Name:         "test-agent",
		ProjectID:      "project_unknown",
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      now,
		Updated:      now,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Request with unknown workspace action
	req := httptest.NewRequest("POST", "/api/v1/agents/agent_unknown_action/workspace/unknown-action", nil)
	req.Header.Set("Authorization", "Bearer "+testWorkspaceDevToken)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	// Should return 404 Not Found
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown workspace action returned status %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestBrokerError_Error(t *testing.T) {
	tests := []struct {
		name     string
		err      *brokerError
		expected string
	}{
		{
			name:     "with brokerID",
			err:      &brokerError{brokerID: "host-123", msg: "connection failed"},
			expected: "broker host-123: connection failed",
		},
		{
			name:     "without brokerID",
			err:      &brokerError{statusCode: 500, msg: "internal error"},
			expected: "internal error",
		},
		{
			name:     "empty error",
			err:      &brokerError{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.err.Error()
			if result != tt.expected {
				t.Errorf("brokerError.Error() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestErrBrokerNotConnected(t *testing.T) {
	err := errBrokerNotConnected("host-abc")
	if err == nil {
		t.Fatal("expected non-nil error")
	}

	expected := "broker host-abc: broker not connected via control channel"
	if err.Error() != expected {
		t.Errorf("error message = %q, want %q", err.Error(), expected)
	}
}

func TestErrRuntimeBrokerError(t *testing.T) {
	err := errRuntimeBrokerError(503, "service unavailable")
	if err == nil {
		t.Fatal("expected non-nil error")
	}

	expected := "service unavailable"
	if err.Error() != expected {
		t.Errorf("error message = %q, want %q", err.Error(), expected)
	}

	hostErr, ok := err.(*brokerError)
	if !ok {
		t.Fatal("expected *brokerError type")
	}
	if hostErr.statusCode != 503 {
		t.Errorf("status code = %d, want 503", hostErr.statusCode)
	}
}

func TestSyncHubNativeWorkspaceBack_SkipsGitProject(t *testing.T) {
	srv, st := testWorkspaceServer(t)
	ctx := context.Background()

	// Create a git-backed project (has GitRemote)
	project := &store.Project{
		ID:        "project-git-sync",
		Slug:      "project-git-sync",
		Name:      "Git Project",
		GitRemote: "github.com/test/repo",
	}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:      "agent-sync-1",
		ProjectID: "project-git-sync",
	}

	// This should return without doing anything for git-backed projects
	srv.syncHubNativeWorkspaceBack(ctx, agent, "workspaces/project-git-sync/agent-sync-1")
	// No panic/error = success
}

func TestSyncHubNativeWorkspaceBack_SkipsColocatedBroker(t *testing.T) {
	srv, st := testWorkspaceServer(t)
	ctx := context.Background()

	// Create a hub-native project
	project := &store.Project{
		ID:   "project-colo-sync",
		Slug: "project-colo-sync",
		Name: "Hub Native Colo",
		// No GitRemote = hub-native
	}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a broker with local path (colocated)
	broker := &store.RuntimeBroker{
		ID:       "broker-colo",
		Name:     "colo-broker",
		Slug:     "colo-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}
	provider := &store.ProjectProvider{
		ProjectID:    "project-colo-sync",
		BrokerID:   "broker-colo",
		BrokerName: "colo-broker",
		LocalPath:  "/home/user/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := st.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add provider: %v", err)
	}

	agent := &store.Agent{
		ID:              "agent-colo-sync",
		ProjectID:         "project-colo-sync",
		RuntimeBrokerID: "broker-colo",
	}

	// Should skip sync because broker has local path
	srv.syncHubNativeWorkspaceBack(ctx, agent, "workspaces/project-colo-sync/project-workspace")
	// No panic/error = success
}

func TestSyncHubNativeWorkspaceBack_NoProjectID(t *testing.T) {
	srv, _ := testWorkspaceServer(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:      "agent-no-project",
		ProjectID: "", // No project ID
	}

	// Should return immediately
	srv.syncHubNativeWorkspaceBack(ctx, agent, "some-path")
	// No panic/error = success
}
