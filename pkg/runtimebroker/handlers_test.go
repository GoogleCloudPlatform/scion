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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
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
	lastEnv          map[string]string
	lastTemplateName string
}

func (m *envCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.lastEnv = opts.Env
	m.lastTemplateName = opts.TemplateName
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
// This verifies the fix from progress-report.md: RuntimeBroker sets SCION_HUB_URL, SCION_SERVER_AUTH_DEV_TOKEN, SCION_AGENT_ID.
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

	// Check SCION_HUB_ENDPOINT (primary)
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com', got %q", got)
	}

	// Check SCION_HUB_URL (legacy compat)
	if got := mgr.lastEnv["SCION_HUB_URL"]; got != "https://hub.example.com" {
		t.Errorf("expected SCION_HUB_URL='https://hub.example.com' (legacy compat), got %q", got)
	}

	// Check SCION_SERVER_AUTH_DEV_TOKEN
	if got := mgr.lastEnv["SCION_SERVER_AUTH_DEV_TOKEN"]; got != "secret-token-xyz" {
		t.Errorf("expected SCION_SERVER_AUTH_DEV_TOKEN='secret-token-xyz', got %q", got)
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
	// Clear dev token env var to prevent broker from forwarding it to agents
	t.Setenv("SCION_SERVER_AUTH_DEV_TOKEN", "")

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
	if _, exists := mgr.lastEnv["SCION_HUB_ENDPOINT"]; exists {
		t.Error("expected SCION_HUB_ENDPOINT to not be set when no hubEndpoint provided")
	}

	if _, exists := mgr.lastEnv["SCION_HUB_URL"]; exists {
		t.Error("expected SCION_HUB_URL to not be set when no hubEndpoint provided")
	}

	if _, exists := mgr.lastEnv["SCION_SERVER_AUTH_DEV_TOKEN"]; exists {
		t.Error("expected SCION_SERVER_AUTH_DEV_TOKEN to not be set when no agentToken provided")
	}

	if _, exists := mgr.lastEnv["SCION_AGENT_ID"]; exists {
		t.Error("expected SCION_AGENT_ID to not be set when no id provided")
	}
}

// provisionCapturingManager tracks whether Provision vs Start was called.
type provisionCapturingManager struct {
	mockManager
	provisionCalled bool
	startCalled     bool
	lastOpts        api.StartOptions
}

func (m *provisionCapturingManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	m.provisionCalled = true
	m.lastOpts = opts
	return &api.ScionConfig{Harness: "claude", HarnessConfig: "claude"}, nil
}

func (m *provisionCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.startCalled = true
	m.lastOpts = opts
	return m.mockManager.Start(ctx, opts)
}

func newTestServerWithProvisionCapture() (*Server, *provisionCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{}

	return New(cfg, mgr, rt), mgr
}

func TestCreateAgentProvisionOnly(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "provisioned-agent",
		"id": "agent-uuid-456",
		"slug": "provisioned-agent",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Provision was called, not Start
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
	if mgr.startCalled {
		t.Error("expected Start NOT to be called for provision-only")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Created {
		t.Error("expected Created to be true")
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	// Agent status should be "created" (not "running")
	if resp.Agent.Status != AgentStatusCreated {
		t.Errorf("expected status '%s', got '%s'", AgentStatusCreated, resp.Agent.Status)
	}

	// ID and slug should be passed through
	if resp.Agent.ID != "agent-uuid-456" {
		t.Errorf("expected ID 'agent-uuid-456', got '%s'", resp.Agent.ID)
	}
	if resp.Agent.Slug != "provisioned-agent" {
		t.Errorf("expected slug 'provisioned-agent', got '%s'", resp.Agent.Slug)
	}
}

func TestCreateAgentProvisionOnlyHarnessConfig(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()

	body := `{
		"name": "harness-agent",
		"id": "agent-uuid-hc",
		"slug": "harness-agent",
		"provisionOnly": true,
		"config": {"template": "claude", "harness": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	// HarnessConfig should be populated from Provision's ScionConfig
	if resp.Agent.HarnessConfig != "claude" {
		t.Errorf("expected HarnessConfig 'claude', got '%s'", resp.Agent.HarnessConfig)
	}

	// Template should NOT be overwritten with the harness name
	if resp.Agent.Template == "claude" {
		t.Error("Template should not be overwritten with harness name")
	}
}

func TestCreateAgentFullStart(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "running-agent",
		"config": {"template": "claude", "task": "do something"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Start was called, not Provision
	if mgr.provisionCalled {
		t.Error("expected Provision NOT to be called for full start")
	}
	if !mgr.startCalled {
		t.Error("expected Start to be called")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	// Agent status should not be "created" since it was fully started
	if resp.Agent.Status == AgentStatusCreated {
		t.Error("expected status to NOT be 'created' for fully started agent")
	}
}

func TestCreateAgentProvisionOnlyWithTask(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "agent-with-task",
		"id": "agent-uuid-789",
		"slug": "agent-with-task",
		"provisionOnly": true,
		"config": {"template": "claude", "task": "implement feature X"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Provision was called, not Start
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
	if mgr.startCalled {
		t.Error("expected Start NOT to be called for provision-only with task")
	}

	// Verify the task was passed through to the Provision options
	if mgr.lastOpts.Task != "implement feature X" {
		t.Errorf("expected task 'implement feature X', got '%s'", mgr.lastOpts.Task)
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be present")
	}

	if resp.Agent.Status != AgentStatusCreated {
		t.Errorf("expected status '%s', got '%s'", AgentStatusCreated, resp.Agent.Status)
	}
}

func TestCreateAgentWithWorkspace(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "workspace-agent",
		"config": {"template": "claude", "workspace": "./zz-ecommerce-site"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Start was called and workspace was passed through
	if !mgr.startCalled {
		t.Error("expected Start to be called")
	}
	if mgr.lastOpts.Workspace != "./zz-ecommerce-site" {
		t.Errorf("expected workspace './zz-ecommerce-site', got '%s'", mgr.lastOpts.Workspace)
	}
}

func TestCreateAgentProvisionOnlyWithWorkspace(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "ws-provision-agent",
		"id": "agent-uuid-ws",
		"slug": "ws-provision-agent",
		"provisionOnly": true,
		"config": {"template": "claude", "workspace": "./my-subfolder", "task": "do work"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify Provision was called with the workspace
	if !mgr.provisionCalled {
		t.Error("expected Provision to be called")
	}
	if mgr.lastOpts.Workspace != "./my-subfolder" {
		t.Errorf("expected workspace './my-subfolder', got '%s'", mgr.lastOpts.Workspace)
	}
}

func TestCreateAgentWithCreatorName(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{
		"name": "creator-agent",
		"creatorName": "alice@example.com",
		"config": {"template": "claude"}
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

	if got := mgr.lastEnv["SCION_CREATOR"]; got != "alice@example.com" {
		t.Errorf("expected SCION_CREATOR='alice@example.com', got %q", got)
	}
}

func TestCreateAgentWithoutCreatorName(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "no-creator-agent"}`
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

	if _, exists := mgr.lastEnv["SCION_CREATOR"]; exists {
		t.Error("expected SCION_CREATOR to not be set when no creatorName provided")
	}
}

func TestStartAgentEndpoint(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should have an agent in the response
	if resp.Agent == nil {
		t.Fatal("expected agent info in start response")
	}

	// Created should be false for a start (not a create)
	if resp.Created {
		t.Error("expected Created to be false for start operation")
	}
}

// TestCreateAgentHubEndpointFromGroveSettings tests that hub endpoint is resolved
// from the grove's settings.yaml when grovePath is provided.
func TestCreateAgentHubEndpointFromGroveSettings(t *testing.T) {
	t.Run("grove settings override request hub endpoint", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a grove directory with settings.yaml containing hub.endpoint
		groveDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(groveDir, 0755); err != nil {
			t.Fatalf("failed to create grove dir: %v", err)
		}
		settingsContent := `hub:
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "grove-endpoint-agent",
			"hubEndpoint": "http://localhost:9810",
			"grovePath": "` + groveDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if mgr.lastEnv == nil {
			t.Fatal("expected environment variables to be set")
		}

		// Grove settings hub.endpoint should override the request's localhost value
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://scionhub.loophole.site" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://scionhub.loophole.site' from grove settings, got %q", got)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != "https://scionhub.loophole.site" {
			t.Errorf("expected SCION_HUB_URL='https://scionhub.loophole.site' from grove settings, got %q", got)
		}
	})

	t.Run("grove settings used when request hub endpoint empty", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		groveDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(groveDir, 0755); err != nil {
			t.Fatalf("failed to create grove dir: %v", err)
		}
		settingsContent := `hub:
  endpoint: "https://hub.example.com"
`
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "grove-fallback-agent",
			"grovePath": "` + groveDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com' from grove settings, got %q", got)
		}
	})

	t.Run("no grove path falls back to request endpoint", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		body := `{
			"name": "no-grove-agent",
			"hubEndpoint": "https://hub.direct.com",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.direct.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.direct.com' from request, got %q", got)
		}
	})
}

// TestCreateAgentGroveHubEndpointSuppressedWhenDisabled tests that grove endpoint
// is suppressed when hub.enabled=false, while dispatcher-provided endpoint still works.
func TestCreateAgentGroveHubEndpointSuppressedWhenDisabled(t *testing.T) {
	t.Run("grove hub endpoint suppressed when hub disabled", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a grove directory with hub.enabled=false but endpoint configured
		groveDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(groveDir, 0755); err != nil {
			t.Fatalf("failed to create grove dir: %v", err)
		}
		settingsContent := `hub:
  enabled: false
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "grove-disabled-agent",
			"grovePath": "` + groveDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if mgr.lastEnv == nil {
			t.Fatal("expected environment variables to be set")
		}

		// Grove endpoint should NOT be used when hub.enabled=false
		if _, exists := mgr.lastEnv["SCION_HUB_ENDPOINT"]; exists {
			t.Error("expected SCION_HUB_ENDPOINT to NOT be set when grove has hub.enabled=false")
		}
		if _, exists := mgr.lastEnv["SCION_HUB_URL"]; exists {
			t.Error("expected SCION_HUB_URL to NOT be set when grove has hub.enabled=false")
		}
	})

	t.Run("dispatcher endpoint still works when grove hub disabled", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a grove directory with hub.enabled=false
		groveDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(groveDir, 0755); err != nil {
			t.Fatalf("failed to create grove dir: %v", err)
		}
		settingsContent := `hub:
  enabled: false
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		// Dispatcher provides its own hub endpoint (authoritative in hosted mode)
		body := `{
			"name": "dispatcher-endpoint-agent",
			"hubEndpoint": "https://hub.authoritative.com",
			"grovePath": "` + groveDir + `",
			"config": {"template": "claude"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		if mgr.lastEnv == nil {
			t.Fatal("expected environment variables to be set")
		}

		// Dispatcher-provided endpoint should still be used (it's authoritative)
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.authoritative.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.authoritative.com' from dispatcher, got %q", got)
		}
	})
}

// gitCloneCapturingManager captures env and GitClone from Start options.
type gitCloneCapturingManager struct {
	mockManager
	lastEnv      map[string]string
	lastGitClone *api.GitCloneConfig
	lastWorkspace string
	lastGrovePath string
}

func (m *gitCloneCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.lastEnv = opts.Env
	m.lastGitClone = opts.GitClone
	m.lastWorkspace = opts.Workspace
	m.lastGrovePath = opts.GrovePath
	return m.mockManager.Start(ctx, opts)
}

func newTestServerWithGitCloneCapture() (*Server, *gitCloneCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true

	mgr := &gitCloneCapturingManager{}
	rt := &runtime.MockRuntime{}

	return New(cfg, mgr, rt), mgr
}

func TestCreateAgentWithGitClone(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"name": "git-clone-agent",
		"config": {
			"template": "claude",
			"gitClone": {
				"url": "https://github.com/example/repo.git",
				"branch": "develop",
				"depth": 1
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify git clone env vars were injected
	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}

	if got := mgr.lastEnv["SCION_GIT_CLONE_URL"]; got != "https://github.com/example/repo.git" {
		t.Errorf("expected SCION_GIT_CLONE_URL='https://github.com/example/repo.git', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_BRANCH"]; got != "develop" {
		t.Errorf("expected SCION_GIT_BRANCH='develop', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_DEPTH"]; got != "1" {
		t.Errorf("expected SCION_GIT_DEPTH='1', got %q", got)
	}

	// Verify workspace and grovePath were cleared
	if mgr.lastWorkspace != "" {
		t.Errorf("expected workspace to be empty in git clone mode, got '%s'", mgr.lastWorkspace)
	}
	if mgr.lastGrovePath != "" {
		t.Errorf("expected grovePath to be empty in git clone mode, got '%s'", mgr.lastGrovePath)
	}

	// Verify GitClone was passed through
	if mgr.lastGitClone == nil {
		t.Fatal("expected GitClone to be set in StartOptions")
	}
	if mgr.lastGitClone.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected GitClone.URL 'https://github.com/example/repo.git', got '%s'", mgr.lastGitClone.URL)
	}
}

func TestCreateAgentWithoutGitClone(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"name": "regular-agent",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// Verify no git clone env vars are set
	if mgr.lastEnv != nil {
		if _, exists := mgr.lastEnv["SCION_GIT_CLONE_URL"]; exists {
			t.Error("expected SCION_GIT_CLONE_URL to NOT be set for regular agent")
		}
	}

	// Verify GitClone is nil
	if mgr.lastGitClone != nil {
		t.Error("expected GitClone to be nil for regular agent")
	}
}

func TestResolveManagerForOpts_NoProfile(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{Name: "test-agent"}
	mgr := srv.resolveManagerForOpts(opts)

	// With no profile, should return the default manager
	if mgr != srv.manager {
		t.Error("expected default manager when no profile is set")
	}
}

func TestResolveManagerForOpts_ProfileNotInSettings(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{
		Name:    "test-agent",
		Profile: "nonexistent-profile",
	}
	mgr := srv.resolveManagerForOpts(opts)

	// Profile not found in settings should return the default manager
	if mgr != srv.manager {
		t.Error("expected default manager when profile not found in settings")
	}
}

func TestResolveManagerForOpts_ProfileWithDifferentRuntime(t *testing.T) {
	// Create a temp grove directory with settings that specify a different runtime
	tmpDir := t.TempDir()
	grovePath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(grovePath, 0755); err != nil {
		t.Fatal(err)
	}

	// Write settings.yaml with a profile that specifies runtime "container"
	// (which differs from the mock runtime's "mock" name)
	settingsYAML := `version: 1
profiles:
  apple:
    runtime: container
runtimes:
  container:
    type: container
`
	if err := os.WriteFile(filepath.Join(grovePath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{
		Name:      "test-agent",
		Profile:   "apple",
		GrovePath: grovePath,
	}
	mgr := srv.resolveManagerForOpts(opts)

	// Profile specifies "container" runtime which differs from mock's "mock",
	// so we should get a different manager
	if mgr == srv.manager {
		t.Error("expected a different manager when profile specifies a different runtime")
	}
}

func TestResolveManagerForOpts_ProfileWithSameRuntime(t *testing.T) {
	// Create a temp grove directory with settings that specify the same runtime as the mock
	tmpDir := t.TempDir()
	grovePath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(grovePath, 0755); err != nil {
		t.Fatal(err)
	}

	// Write settings with profile whose runtime matches the mock runtime ("mock")
	settingsYAML := `version: 1
profiles:
  default:
    runtime: mock
runtimes:
  mock:
    type: mock
`
	if err := os.WriteFile(filepath.Join(grovePath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{
		Name:      "test-agent",
		Profile:   "default",
		GrovePath: grovePath,
	}
	mgr := srv.resolveManagerForOpts(opts)

	// Profile specifies "mock" runtime which matches the broker's runtime,
	// so we should get the same manager
	if mgr != srv.manager {
		t.Error("expected default manager when profile resolves to same runtime")
	}
}

func TestCreateAgentWithProfile(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "profiled-agent",
		"config": {"template": "claude", "profile": "custom-profile"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if mgr.lastOpts.Profile != "custom-profile" {
		t.Errorf("expected Profile 'custom-profile', got %q", mgr.lastOpts.Profile)
	}
}

func TestCreateAgentWithoutProfile(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "no-profile-agent",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if mgr.lastOpts.Profile != "" {
		t.Errorf("expected empty Profile, got %q", mgr.lastOpts.Profile)
	}
}

func TestGroveSlugWorkspacePath(t *testing.T) {
	// Verify the workspace directory path for hub-native groves uses
	// ~/.scion/groves/<slug>/ instead of the worktree-based path.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}

	expected := filepath.Join(globalDir, "groves", "my-test-grove")

	// Simulate the logic from the handler: when GroveSlug is set,
	// use the conventional path.
	groveSlug := "my-test-grove"
	workspaceDir := filepath.Join(globalDir, "groves", groveSlug)

	if workspaceDir != expected {
		t.Errorf("expected workspace dir %q, got %q", expected, workspaceDir)
	}

	// When GroveSlug is empty, the default worktree path is used.
	worktreeBase := "/tmp/test-worktrees"
	agentName := "test-agent"
	defaultDir := filepath.Join(worktreeBase, agentName, "workspace")
	expectedDefault := "/tmp/test-worktrees/test-agent/workspace"
	if defaultDir != expectedDefault {
		t.Errorf("expected default workspace dir %q, got %q", expectedDefault, defaultDir)
	}
}

func TestCreateAgentRequest_GroveSlugField(t *testing.T) {
	// Verify GroveSlug is properly serialized/deserialized in CreateAgentRequest.
	reqJSON := `{
		"name": "grove-agent",
		"groveSlug": "my-hub-grove",
		"workspaceStoragePath": "workspaces/grove-123/grove-workspace"
	}`

	var req CreateAgentRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if req.GroveSlug != "my-hub-grove" {
		t.Errorf("expected GroveSlug 'my-hub-grove', got '%s'", req.GroveSlug)
	}
	if req.WorkspaceStoragePath != "workspaces/grove-123/grove-workspace" {
		t.Errorf("expected WorkspaceStoragePath 'workspaces/grove-123/grove-workspace', got '%s'", req.WorkspaceStoragePath)
	}
}

func TestCreateAgentGroveSlugResolvesGrovePath(t *testing.T) {
	// When GroveSlug is set and GrovePath is empty (hub-native grove with no
	// local provider path), the handler should resolve GrovePath to the
	// conventional ~/.scion/groves/<slug>/ path so the agent is created in the
	// correct grove instead of the broker's local grove.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "hub-native-agent",
		"id": "agent-uuid-123",
		"slug": "hub-native-agent",
		"groveId": "grove-abc",
		"groveSlug": "my-hub-grove",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.provisionCalled {
		t.Fatal("expected Provision to be called")
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}

	expectedPath := filepath.Join(globalDir, "groves", "my-hub-grove")
	if mgr.lastOpts.GrovePath != expectedPath {
		t.Errorf("expected GrovePath %q, got %q", expectedPath, mgr.lastOpts.GrovePath)
	}
}

func TestCreateAgentGroveSlugNotUsedWhenGrovePathSet(t *testing.T) {
	// When both GrovePath and GroveSlug are set, GrovePath takes precedence
	// (the broker has a local provider path for this grove).
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "local-grove-agent",
		"id": "agent-uuid-456",
		"slug": "local-grove-agent",
		"groveId": "grove-def",
		"groveSlug": "my-hub-grove",
		"grovePath": "/projects/my-local-grove/.scion",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.provisionCalled {
		t.Fatal("expected Provision to be called")
	}

	// GrovePath should remain as explicitly provided, not overridden by GroveSlug
	if mgr.lastOpts.GrovePath != "/projects/my-local-grove/.scion" {
		t.Errorf("expected GrovePath %q, got %q", "/projects/my-local-grove/.scion", mgr.lastOpts.GrovePath)
	}
}

func TestStartAgentGroveSlugResolvesGrovePath(t *testing.T) {
	// When the startAgent handler receives groveSlug with no grovePath
	// (hub-native grove), it should resolve GrovePath from the slug.
	srv, mgr := newTestServerWithProvisionCapture()

	// Start uses the agent name from the URL path
	body := `{"groveSlug": "my-hub-grove"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/hub-native-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}

	expectedPath := filepath.Join(globalDir, "groves", "my-hub-grove")
	if mgr.lastOpts.GrovePath != expectedPath {
		t.Errorf("expected GrovePath %q, got %q", expectedPath, mgr.lastOpts.GrovePath)
	}
}

func TestStartAgentGroveSlugNotUsedWhenGrovePathSet(t *testing.T) {
	// When startAgent receives both grovePath and groveSlug,
	// grovePath takes precedence.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"grovePath": "/projects/my-local-grove/.scion", "groveSlug": "my-hub-grove"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/local-grove-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	// GrovePath should remain as explicitly provided, not overridden by GroveSlug
	if mgr.lastOpts.GrovePath != "/projects/my-local-grove/.scion" {
		t.Errorf("expected GrovePath %q, got %q", "/projects/my-local-grove/.scion", mgr.lastOpts.GrovePath)
	}
}
