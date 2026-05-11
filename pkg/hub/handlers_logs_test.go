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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testServerNoCloudLogs creates a test server and explicitly disables the
// logQueryService so cloud-logs endpoints return 501.
func testServerNoCloudLogs(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.logQueryService = nil
	return srv, s
}

func createTestAgent(t *testing.T, s store.Store) *store.Agent {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "test-project-" + api.NewUUID()[:8],
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agent := &store.Agent{
		ID:      api.NewUUID(),
		Name:    "test-agent-" + api.NewUUID()[:8],
		ProjectID: project.ID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return agent
}

func TestHandleAgentCloudLogs_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/cloud-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}

	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error.Message != "Cloud Logging is not configured" {
		t.Errorf("message = %q, want %q", resp.Error.Message, "Cloud Logging is not configured")
	}
	if resp.Error.Code != "not_implemented" {
		t.Errorf("code = %q, want %q", resp.Error.Code, "not_implemented")
	}
}

func TestHandleAgentCloudLogsStream_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/cloud-logs/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestHandleAgentCloudLogs_MethodNotAllowed(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	// POST should not be allowed for cloud-logs
	req := httptest.NewRequest("POST", "/api/v1/agents/"+agent.ID+"/cloud-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleAgentCloudLogs_AgentNotFound(t *testing.T) {
	srv, _ := testServerNoCloudLogs(t)

	req := httptest.NewRequest("GET", "/api/v1/agents/nonexistent/cloud-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// When logQueryService is nil, returns 501 before looking up the agent.
	// This is correct: "Cloud Logging is not configured" supersedes agent resolution.
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestHandleAgentCloudLogs_QueryParameterParsing(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	// When logQueryService is nil, all valid requests return 501 (auth + params parsed first)
	since := time.Now().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	until := time.Now().Format(time.RFC3339Nano)
	req := httptest.NewRequest("GET",
		"/api/v1/agents/"+agent.ID+"/cloud-logs?tail=50&since="+since+"&until="+until+"&severity=ERROR",
		nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should get 501 since logQueryService is nil
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

// ---------------------------------------------------------------------------
// Message-logs endpoint tests
// ---------------------------------------------------------------------------

func TestHandleAgentMessageLogs_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/message-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}

	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error.Message != "Cloud Logging is not configured" {
		t.Errorf("message = %q, want %q", resp.Error.Message, "Cloud Logging is not configured")
	}
}

func TestHandleAgentMessageLogsStream_NotConfigured(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/message-logs/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
}

func TestHandleAgentMessageLogs_MethodNotAllowed(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("POST", "/api/v1/agents/"+agent.ID+"/message-logs", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandleAgentCloudLogs_Unauthenticated(t *testing.T) {
	srv, s := testServerNoCloudLogs(t)
	agent := createTestAgent(t, s)

	req := httptest.NewRequest("GET", "/api/v1/agents/"+agent.ID+"/cloud-logs", nil)
	// No auth header
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should get 404 or 401 - the auth middleware should prevent access
	if w.Code == http.StatusOK || w.Code == http.StatusNotImplemented {
		t.Errorf("status = %d, expected auth error", w.Code)
	}
}
