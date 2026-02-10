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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/runtime"
)

// mockManager implements agent.Manager for testing
type mockManager struct {
	agents []api.AgentInfo
}

func (m *mockManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	return &api.ScionConfig{}, nil
}

func (m *mockManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	agent := &api.AgentInfo{
		ID:     "test-container-id",
		Name:   opts.Name,
		Status: "running",
	}
	m.agents = append(m.agents, *agent)
	return agent, nil
}

func (m *mockManager) Stop(ctx context.Context, agentID string) error {
	return nil
}

func (m *mockManager) Delete(ctx context.Context, agentID string, deleteFiles bool, grovePath string, removeBranch bool) (bool, error) {
	return true, nil
}

func (m *mockManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	return m.agents, nil
}

func (m *mockManager) Message(ctx context.Context, agentID string, message string, interrupt bool) error {
	return nil
}

func (m *mockManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, nil
}

func newTestServer() *Server {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:              "container-1",
				Name:            "test-agent-1",
				Status:          "running",
				ContainerStatus: "Up 1 hour",
			},
			{
				ID:              "container-2",
				Name:            "test-agent-2",
				Status:          "stopped",
				ContainerStatus: "Exited",
			},
		},
	}

	// Use mock runtime
	rt := &runtime.MockRuntime{}

	return New(cfg, mgr, rt)
}

func TestHealthz(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got '%s'", resp.Status)
	}
}

func TestReadyz(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHostInfo(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp BrokerInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.BrokerID != "test-broker-id" {
		t.Errorf("expected brokerId 'test-broker-id', got '%s'", resp.BrokerID)
	}

	if resp.Capabilities == nil {
		t.Error("expected capabilities to be present")
	}
}

func TestListAgents(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp ListAgentsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(resp.Agents))
	}

	if resp.TotalCount != 2 {
		t.Errorf("expected totalCount 2, got %d", resp.TotalCount)
	}
}

func TestGetAgent(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/test-agent-1", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp AgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Name != "test-agent-1" {
		t.Errorf("expected name 'test-agent-1', got '%s'", resp.Name)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/nonexistent", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

func TestCreateAgent(t *testing.T) {
	srv := newTestServer()

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Created {
		t.Error("expected Created to be true")
	}

	if resp.Agent == nil {
		t.Error("expected agent to be present")
	}
}

func TestCreateAgentMissingName(t *testing.T) {
	srv := newTestServer()

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestStopAgent(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/stop", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status %d, got %d", http.StatusAccepted, w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer()

	// PUT on /api/v1/agents should not be allowed
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

// envCapturingManager captures the environment variables passed to Start().
// Used for testing that Hub credentials are properly set.
type envCapturingManager struct {
	mockManager
	lastEnv map[string]string
}

func (m *envCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.lastEnv = opts.Env
	return m.mockManager.Start(ctx, opts)
}

func newTestServerWithEnvCapture() (*Server, *envCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true

	mgr := &envCapturingManager{}

	// Use mock runtime
	rt := &runtime.MockRuntime{}

	return New(cfg, mgr, rt), mgr
}

// TestCreateAgentWithHubCredentials tests that Hub authentication env vars are passed to agent.
// This verifies the fix from progress-report.md: RuntimeBroker sets SCION_HUB_URL, SCION_HUB_TOKEN, SCION_AGENT_ID.
func TestCreateAgentWithHubCredentials(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{
		"name": "test-agent",
		"id": "agent-uuid-123",
		"hubEndpoint": "https://hub.example.com",
		"agentToken": "secret-token-xyz",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Hub credentials were passed to the manager
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	// Check SCION_HUB_URL
	if got := mgr.lastEnv["SCION_HUB_URL"]; got != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_URL='https://hub.example.com', got %q", got)
	}

	// Check SCION_HUB_TOKEN
	if got := mgr.lastEnv["SCION_HUB_TOKEN"]; got != "secret-token-xyz" {
		t.Errorf("expected SCION_HUB_TOKEN='secret-token-xyz', got %q", got)
	}

	// Check SCION_AGENT_ID
	if got := mgr.lastEnv["SCION_AGENT_ID"]; got != "agent-uuid-123" {
		t.Errorf("expected SCION_AGENT_ID='agent-uuid-123', got %q", got)
	}
}

// TestCreateAgentWithDebugMode tests that SCION_DEBUG env var is set when debug mode is enabled.
// This verifies Fix 4 from progress-report.md: Pass SCION_DEBUG env var.
func TestCreateAgentWithDebugMode(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "debug-agent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify SCION_DEBUG was set
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	if got := mgr.lastEnv["SCION_DEBUG"]; got != "1" {
		t.Errorf("expected SCION_DEBUG='1' when server in debug mode, got %q", got)
	}
}

// TestCreateAgentWithResolvedEnv tests that resolvedEnv from Hub is merged with config.Env.
func TestCreateAgentWithResolvedEnv(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	// resolvedEnv contains Hub-provided secrets and variables
	// config.Env contains explicit overrides (takes precedence)
	body := `{
		"name": "env-merge-agent",
		"resolvedEnv": {
			"SECRET_KEY": "hub-secret",
			"SHARED_VAR": "from-hub"
		},
		"config": {
			"env": ["EXPLICIT_VAR=explicit-value", "SHARED_VAR=from-config"]
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	// Check that resolvedEnv was applied
	if got := mgr.lastEnv["SECRET_KEY"]; got != "hub-secret" {
		t.Errorf("expected SECRET_KEY='hub-secret' from resolvedEnv, got %q", got)
	}

	// Check that config.Env was applied
	if got := mgr.lastEnv["EXPLICIT_VAR"]; got != "explicit-value" {
		t.Errorf("expected EXPLICIT_VAR='explicit-value' from config.Env, got %q", got)
	}

	// Check that config.Env takes precedence over resolvedEnv
	if got := mgr.lastEnv["SHARED_VAR"]; got != "from-config" {
		t.Errorf("expected SHARED_VAR='from-config' (config.Env should override resolvedEnv), got %q", got)
	}
}

// TestCreateAgentWithoutHubCredentials tests agent creation without Hub integration.
func TestCreateAgentWithoutHubCredentials(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "local-agent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Env should still be set (at minimum SCION_DEBUG since debug mode is on)
	if mgr.lastEnv == nil {
		t.Fatal("expected environment to be initialized")
	}

	// Hub credentials should NOT be present
	if _, exists := mgr.lastEnv["SCION_HUB_URL"]; exists {
		t.Error("expected SCION_HUB_URL to not be set when no hubEndpoint provided")
	}

	if _, exists := mgr.lastEnv["SCION_HUB_TOKEN"]; exists {
		t.Error("expected SCION_HUB_TOKEN to not be set when no agentToken provided")
	}

	if _, exists := mgr.lastEnv["SCION_AGENT_ID"]; exists {
		t.Error("expected SCION_AGENT_ID to not be set when no id provided")
	}
}
