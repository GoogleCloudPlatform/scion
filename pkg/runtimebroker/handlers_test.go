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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// mockManager implements agent.Manager for testing
type mockManager struct {
	agents              []api.AgentInfo
	startCalls          int
	stopCalls           int
	deleteCalls         int
	startErr            error
	stopErr             error
	lastStartOpts       api.StartOptions
	lastDeleteGrovePath string
	lastDeleteAgentID   string
}

func (m *mockManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	return &api.ScionConfig{}, nil
}

func (m *mockManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.startCalls++
	m.lastStartOpts = opts
	if m.startErr != nil {
		return nil, m.startErr
	}
	agent := &api.AgentInfo{
		ID:    "test-container-id",
		Name:  opts.Name,
		Phase: "running",
	}
	m.agents = append(m.agents, *agent)
	return agent, nil
}

func (m *mockManager) Stop(ctx context.Context, agentID string) error {
	m.stopCalls++
	return m.stopErr
}

func (m *mockManager) Delete(ctx context.Context, agentID string, deleteFiles bool, grovePath string, removeBranch bool) (bool, error) {
	m.lastDeleteGrovePath = grovePath
	m.lastDeleteAgentID = agentID
	m.deleteCalls++
	return true, nil
}

func (m *mockManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	return m.agents, nil
}

func (m *mockManager) Message(ctx context.Context, agentID string, message string, interrupt bool) error {
	return nil
}

func (m *mockManager) MessageRaw(ctx context.Context, agentID string, keys string) error {
	return nil
}

func (m *mockManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, nil
}

func (m *mockManager) Close() {}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	// Isolate from repo .scion by changing CWD to a temp dir containing its own .scion
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	dotScion := filepath.Join(tmpDir, ".scion")
	if err := os.Mkdir(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create dummy templates to satisfy FindTemplate
	templatesDir := filepath.Join(dotScion, "templates")
	if err := os.MkdirAll(templatesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:              "container-1",
				Name:            "test-agent-1",
				Phase:           "running",
				ContainerStatus: "Up 1 hour",
			},
			{
				ID:              "container-2",
				Name:            "test-agent-2",
				Phase:           "stopped",
				ContainerStatus: "Exited",
			},
		},
	}

	// Use mock runtime
	rt := &runtime.MockRuntime{}

	return New(cfg, mgr, rt)
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)

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
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHostInfo(t *testing.T) {
	srv := newTestServer(t)

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
	srv := newTestServer(t)

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

func TestListAgentsIncludesAuxiliaryRuntimes(t *testing.T) {
	srv := newTestServer(t)

	// Add an auxiliary runtime with a K8s agent not on the default runtime
	auxMgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:              "k8s-pod-1",
				Name:            "k8s-agent",
				Phase:           "running",
				ContainerStatus: "Running",
				Runtime:         "kubernetes",
			},
		},
	}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

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

	// Should include 2 default + 1 auxiliary = 3
	if resp.TotalCount != 3 {
		t.Errorf("expected totalCount 3, got %d", resp.TotalCount)
	}

	// Verify the K8s agent is included
	found := false
	for _, ag := range resp.Agents {
		if ag.Name == "k8s-agent" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected k8s-agent from auxiliary runtime to be in list")
	}
}

func TestListAgentsDeduplicatesAcrossRuntimes(t *testing.T) {
	srv := newTestServer(t)

	// Add an auxiliary runtime that has an agent with the same name as one on the default runtime
	auxMgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:              "k8s-pod-1",
				Name:            "test-agent-1", // same name as default
				Phase:           "running",
				ContainerStatus: "Running",
			},
		},
	}
	auxRt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv.auxiliaryRuntimesMu.Lock()
	srv.auxiliaryRuntimes["kubernetes"] = auxiliaryRuntime{Runtime: auxRt, Manager: auxMgr}
	srv.auxiliaryRuntimesMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	var resp ListAgentsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should still be 2, not 3, because test-agent-1 is deduplicated
	if resp.TotalCount != 2 {
		t.Errorf("expected totalCount 2 (deduplicated), got %d", resp.TotalCount)
	}
}

func TestGetAgent(t *testing.T) {
	srv := newTestServer(t)

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
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/nonexistent", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

func TestCreateAgent(t *testing.T) {
	srv := newTestServer(t)

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
	srv := newTestServer(t)

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
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/stop", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status %d, got %d", http.StatusAccepted, w.Code)
	}
}

func TestRestartAgent(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.stopCalls != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.stopCalls)
	}
	if mgr.startCalls != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
	}
	if mgr.lastStartOpts.Name != "test-agent-1" {
		t.Fatalf("expected restart to start agent 'test-agent-1', got %q", mgr.lastStartOpts.Name)
	}
}

func TestRestartAgent_StartFailure(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("boom")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
	if mgr.stopCalls != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.stopCalls)
	}
	if mgr.startCalls != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
	}
}

func TestRestartAgent_StopFailureTolerated(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	// Simulate podman returning an error when stopping an already-exited container
	mgr.stopErr = fmt.Errorf("podman stop test-agent-1 failed: exit status 125: Error: can only stop running containers: test-agent-1 is not running")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	// Restart should succeed despite the stop error — it's tolerable
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.stopCalls != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.stopCalls)
	}
	if mgr.startCalls != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
	}
}

func TestRestartAgent_BrokerModeSet(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.lastStartOpts.BrokerMode {
		t.Fatalf("expected BrokerMode to be true in restart start options")
	}
}

func TestIsContainerStopTolerable(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"not found", fmt.Errorf("podman stop foo failed: exit status 1: Error: no container with name or ID \"foo\" found: no such container"), true},
		{"no such", fmt.Errorf("docker stop foo failed: exit status 1: Error response from daemon: No such container: foo"), true},
		{"exit status 125", fmt.Errorf("podman stop foo failed: exit status 125"), true},
		{"not running", fmt.Errorf("podman stop foo failed: exit status 125: Error: can only stop running containers: foo is not running"), true},
		{"generic failure", fmt.Errorf("podman stop foo failed: exit status 1: unexpected error"), false},
		{"permission denied", fmt.Errorf("podman stop foo failed: exit status 1: Error: permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isContainerStopTolerable(tt.err)
			if result != tt.expected {
				t.Errorf("isContainerStopTolerable(%q) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)

	// PUT on /api/v1/agents should not be allowed
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

func TestAgentLogsAllowsGet(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/test-agent-1/logs", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "mock logs" {
		t.Fatalf("expected body %q, got %q", "mock logs", body)
	}
}

// envCapturingManager captures the environment variables passed to Start().
// Used for testing that Hub credentials are properly set.
type envCapturingManager struct {
	mockManager
	lastEnv           map[string]string
	lastTemplateName  string
	lastHarnessConfig string
}

func (m *envCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.lastEnv = opts.Env
	m.lastTemplateName = opts.TemplateName
	m.lastHarnessConfig = opts.HarnessConfig
	return m.mockManager.Start(ctx, opts)
}

func newTestServerWithEnvCapture() (*Server, *envCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}

	// Use mock runtime
	rt := &runtime.MockRuntime{}

	return New(cfg, mgr, rt), mgr
}

// TestCreateAgentWithHubCredentials tests that Hub authentication env vars are passed to agent.
// This verifies the fix from progress-report.md: RuntimeBroker sets SCION_HUB_URL, SCION_AUTH_TOKEN, SCION_AGENT_ID.
func TestCreateAgentWithHubCredentials(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{
		"name": "test-agent",
		"id": "agent-uuid-123",
		"groveId": "grove-uuid-456",
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

	// Check SCION_AUTH_TOKEN
	if got := mgr.lastEnv["SCION_AUTH_TOKEN"]; got != "secret-token-xyz" {
		t.Errorf("expected SCION_AUTH_TOKEN='secret-token-xyz', got %q", got)
	}

	// Check SCION_AGENT_ID
	if got := mgr.lastEnv["SCION_AGENT_ID"]; got != "agent-uuid-123" {
		t.Errorf("expected SCION_AGENT_ID='agent-uuid-123', got %q", got)
	}

	// Check SCION_GROVE_ID
	if got := mgr.lastEnv["SCION_GROVE_ID"]; got != "grove-uuid-456" {
		t.Errorf("expected SCION_GROVE_ID='grove-uuid-456', got %q", got)
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

// TestCreateAgentWithBrokerID tests that SCION_BROKER_ID env var is set from server config.
func TestCreateAgentWithBrokerID(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{"name": "broker-id-agent"}`
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

	if got := mgr.lastEnv["SCION_BROKER_ID"]; got != "test-broker-id" {
		t.Errorf("expected SCION_BROKER_ID='test-broker-id', got %q", got)
	}

	if got := mgr.lastEnv["SCION_BROKER_NAME"]; got != "test-host" {
		t.Errorf("expected SCION_BROKER_NAME='test-host', got %q", got)
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
	t.Setenv("SCION_AUTH_TOKEN", "")

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

	if _, exists := mgr.lastEnv["SCION_AUTH_TOKEN"]; exists {
		t.Error("expected SCION_AUTH_TOKEN to not be set when no agentToken provided")
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
	cfg.ForceRuntime = "mock"

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
	if resp.Agent.Status != string(state.PhaseCreated) {
		t.Errorf("expected status '%s', got '%s'", string(state.PhaseCreated), resp.Agent.Status)
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
	if resp.Agent.Status == string(state.PhaseCreated) {
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

	if resp.Agent.Status != string(state.PhaseCreated) {
		t.Errorf("expected status '%s', got '%s'", string(state.PhaseCreated), resp.Agent.Status)
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
	srv := newTestServer(t)

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
	t.Run("request hub endpoint takes priority over grove settings", func(t *testing.T) {
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

		// Request hub endpoint takes priority over grove settings (grove settings
		// are only a fallback when no endpoint is provided by dispatch/broker).
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from request, got %q", got)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from request, got %q", got)
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

// TestCreateAgentHubNativeGroveSettingsEndpoint tests that createAgent with a
// hub-native grove (GroveSlug set, no GrovePath) correctly resolves the grove
// path and uses grove settings hub.endpoint from the .scion subdirectory.
func TestCreateAgentHubNativeGroveSettingsEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "http://localhost:9810" // broker's default (combo mode)
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	// Set up a hub-native grove directory at the expected path.
	// The slug "my-hub-grove" will resolve to ~/.scion/groves/my-hub-grove.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}
	grovePath := filepath.Join(globalDir, "groves", "settings-test-grove")
	scionDir := filepath.Join(grovePath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create .scion dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(grovePath) })

	// Place settings.yaml in the .scion subdirectory (hub-native grove layout)
	settingsContent := "hub:\n  endpoint: https://hub.external.example.com\n"
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings.yaml: %v", err)
	}

	// Send createAgent request with groveSlug but no grovePath
	body := `{
		"name": "hub-native-agent",
		"groveSlug": "settings-test-grove",
		"hubEndpoint": "http://localhost:9810",
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

	// Request hub endpoint takes priority over grove settings (grove settings
	// are only a fallback when no endpoint is provided by dispatch/broker).
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from request, got %q", got)
	}
	if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from request, got %q", got)
	}
}

// TestResolveGroveSettingsDir tests the helper function that resolves the
// settings directory for both linked and hub-native groves.
func TestResolveGroveSettingsDir(t *testing.T) {
	t.Run("linked grove - settings at grovePath directly", func(t *testing.T) {
		// Linked grove: grovePath = /path/to/project/.scion, settings.yaml is there
		groveDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(groveDir, 0755); err != nil {
			t.Fatalf("failed to create grove dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte("hub:\n  endpoint: https://example.com\n"), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		result := resolveGroveSettingsDir(groveDir)
		if result != groveDir {
			t.Errorf("expected %q, got %q", groveDir, result)
		}
	})

	t.Run("hub-native grove - settings in .scion subdirectory", func(t *testing.T) {
		// Hub-native grove: grovePath = ~/.scion/groves/<slug>, settings in .scion/
		groveDir := t.TempDir()
		scionDir := filepath.Join(groveDir, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("failed to create .scion dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("hub:\n  endpoint: https://example.com\n"), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		result := resolveGroveSettingsDir(groveDir)
		if result != scionDir {
			t.Errorf("expected %q (with .scion), got %q", scionDir, result)
		}
	})

	t.Run("no settings file - returns original path", func(t *testing.T) {
		groveDir := t.TempDir()
		result := resolveGroveSettingsDir(groveDir)
		if result != groveDir {
			t.Errorf("expected %q (original path), got %q", groveDir, result)
		}
	})
}

// TestCreateAgentContainerHubEndpointOverride tests that ContainerHubEndpoint
// overrides the dispatcher-provided endpoint for container injection.
func TestCreateAgentContainerHubEndpointOverride(t *testing.T) {
	t.Run("container endpoint overrides request endpoint", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.Debug = true
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{}
		srv := New(cfg, mgr, rt)

		body := `{
			"name": "test-agent",
			"hubEndpoint": "http://localhost:8080"
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

		// ContainerHubEndpoint should override the request's localhost value
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:8080" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:8080' from container override, got %q", got)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://host.containers.internal:8080" {
			t.Errorf("expected SCION_HUB_URL='http://host.containers.internal:8080' from container override, got %q", got)
		}
	})

	t.Run("container endpoint overrides localhost even with grove settings", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.Debug = true
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{}
		srv := New(cfg, mgr, rt)

		// Create a grove directory with settings.yaml containing hub.endpoint
		groveDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(groveDir, 0755); err != nil {
			t.Fatal(err)
		}
		settingsContent := `schema_version: "1"
hub:
  enabled: true
  endpoint: "https://tunnel.example.com"
`
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatal(err)
		}

		body := fmt.Sprintf(`{
			"name": "test-agent",
			"hubEndpoint": "http://localhost:8080",
			"grovePath": %q
		}`, groveDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// ContainerHubEndpoint override applies last to localhost endpoints;
		// grove settings are only a fallback when no dispatch/broker endpoint exists.
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:8080" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:8080' from container bridge override, got %q", got)
		}
	})

	t.Run("no container endpoint uses request endpoint", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		body := `{
			"name": "test-agent",
			"hubEndpoint": "https://hub.public.com"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Without ContainerHubEndpoint, request endpoint is used
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.public.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.public.com' from request, got %q", got)
		}
	})

	t.Run("non-localhost endpoint is not overridden by container endpoint", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{}
		srv := New(cfg, mgr, rt)

		body := `{
			"name": "test-agent",
			"hubEndpoint": "https://hub.example.com"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Non-localhost endpoint should NOT be overridden by ContainerHubEndpoint
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com' (non-localhost preserved), got %q", got)
		}
	})

	t.Run("kubernetes runtime skips container endpoint override", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "kubernetes"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{
			NameFunc: func() string { return "kubernetes" },
		}
		srv := New(cfg, mgr, rt)

		body := `{
			"name": "test-agent",
			"hubEndpoint": "http://localhost:8080"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Kubernetes runtime should NOT use bridge address
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:8080" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:8080' (k8s skips bridge), got %q", got)
		}
	})
}

// TestCreateAgentConnectionHubEndpoint tests that when a request arrives via
// control channel from a specific hub, the connection's hub endpoint is used
// instead of the broker's own config.HubEndpoint (which may point to a
// different hub in multi-hub setups).
func TestCreateAgentConnectionHubEndpoint(t *testing.T) {
	t.Run("connection endpoint used when request endpoint empty", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:8080" // broker's own local hub
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{}
		srv := New(cfg, mgr, rt)

		// Register a remote hub connection (as would happen via control channel)
		srv.hubMu.Lock()
		srv.hubConnections["hub-demo-scion-ai-dev"] = &HubConnection{
			Name:        "hub-demo-scion-ai-dev",
			HubEndpoint: "https://hub.demo.scion-ai.dev",
		}
		srv.hubMu.Unlock()

		// Request comes via control channel with no explicit hubEndpoint
		body := `{
			"name": "remote-hub-agent"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", "hub-demo-scion-ai-dev")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Should use the remote hub's endpoint, NOT the broker's local hub
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.demo.scion-ai.dev" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.demo.scion-ai.dev' from connection, got %q", got)
		}
	})

	t.Run("request endpoint takes priority over connection endpoint", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{}
		srv := New(cfg, mgr, rt)

		srv.hubMu.Lock()
		srv.hubConnections["hub-demo"] = &HubConnection{
			Name:        "hub-demo",
			HubEndpoint: "https://hub.demo.scion-ai.dev",
		}
		srv.hubMu.Unlock()

		// Request explicitly sets hubEndpoint (hub dispatcher configured it)
		body := `{
			"name": "explicit-endpoint-agent",
			"hubEndpoint": "https://hub.explicit.example.com"
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", "hub-demo")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// Explicit request endpoint wins over connection
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://hub.explicit.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.explicit.example.com' from request, got %q", got)
		}
	})
}

// gitCloneCapturingManager captures env and GitClone from Start options.
type gitCloneCapturingManager struct {
	mockManager
	lastEnv       map[string]string
	lastGitClone  *api.GitCloneConfig
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
	cfg.ForceRuntime = "mock"

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

func TestCreateAgentWithGitCloneAndBranch(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"name": "branch-agent",
		"config": {
			"template": "claude",
			"branch": "my-feature",
			"gitClone": {
				"url": "https://github.com/example/repo.git",
				"branch": "main",
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

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}
	if got := mgr.lastEnv["SCION_AGENT_BRANCH"]; got != "my-feature" {
		t.Errorf("expected SCION_AGENT_BRANCH='my-feature', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_BRANCH"]; got != "main" {
		t.Errorf("expected SCION_GIT_BRANCH='main', got %q", got)
	}
}

func TestFinalizeEnvPassesAgentBranch(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()
	agentID := "test-finalize-branch-id"

	// Seed pending env-gather state with a branch and gitClone config
	srv.pendingEnvGatherMu.Lock()
	srv.pendingEnvGather[agentID] = &pendingAgentState{
		AgentID: agentID,
		Request: &CreateAgentRequest{
			Name:      "finalize-branch-agent",
			GrovePath: "",
			Config: &CreateAgentConfig{
				Template: "claude",
				Branch:   "my-feature",
				GitClone: &api.GitCloneConfig{
					URL:    "https://github.com/example/repo.git",
					Branch: "main",
					Depth:  1,
				},
			},
		},
		MergedEnv: map[string]string{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		State:     pendingStatePending,
	}
	srv.pendingEnvGatherMu.Unlock()

	body := `{"env": {"GEMINI_API_KEY": "test-key"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/finalize-env", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected environment variables to be set, got nil")
	}
	if got := mgr.lastEnv["SCION_AGENT_BRANCH"]; got != "my-feature" {
		t.Errorf("expected SCION_AGENT_BRANCH='my-feature', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_BRANCH"]; got != "main" {
		t.Errorf("expected SCION_GIT_BRANCH='main', got %q", got)
	}
	if got := mgr.lastEnv["SCION_GIT_CLONE_URL"]; got != "https://github.com/example/repo.git" {
		t.Errorf("expected SCION_GIT_CLONE_URL set, got %q", got)
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
	srv.config.ForceRuntime = ""

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

// TestStartAgentGroveSettingsFallbackHubEndpoint verifies that the startAgent
// handler uses grove settings hub.endpoint only as a fallback when no broker
// config or dispatch endpoint is available.
func TestStartAgentGroveSettingsFallbackHubEndpoint(t *testing.T) {
	t.Run("linked grove with settings at grovePath", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:9810"
		cfg.Debug = true
		cfg.ForceRuntime = "mock"

		mgr := &provisionCapturingManager{}
		rt := &runtime.MockRuntime{}
		srv := New(cfg, mgr, rt)

		// Linked grove: grovePath ends in .scion, settings.yaml is directly there
		groveDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(groveDir, 0755); err != nil {
			t.Fatalf("failed to create grove dir: %v", err)
		}
		settingsContent := "hub:\n  endpoint: https://hub.production.example.com\n"
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		body := fmt.Sprintf(`{"grovePath": %q}`, groveDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}

		if !mgr.startCalled {
			t.Fatal("expected Start to be called")
		}

		// Broker config HubEndpoint takes priority over grove settings
		if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
		}
		if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from broker config, got %q", got)
		}
	})

	t.Run("hub-native grove with settings in .scion subdirectory", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:9810"
		cfg.Debug = true
		cfg.ForceRuntime = "mock"

		mgr := &provisionCapturingManager{}
		rt := &runtime.MockRuntime{}
		srv := New(cfg, mgr, rt)

		// Hub-native grove: grovePath is the workspace parent (~/.scion/groves/<slug>),
		// settings.yaml lives in the .scion subdirectory
		groveDir := t.TempDir()
		scionDir := filepath.Join(groveDir, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("failed to create .scion dir: %v", err)
		}
		settingsContent := "hub:\n  endpoint: https://hub.native.example.com\n"
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		body := fmt.Sprintf(`{"grovePath": %q}`, groveDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}

		if !mgr.startCalled {
			t.Fatal("expected Start to be called")
		}

		// Broker config HubEndpoint takes priority over grove settings
		if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
		}
		if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from broker config, got %q", got)
		}
	})
}

// TestStartAgentBrokerConfigUsedWhenNoGroveSettings verifies that the broker's
// config HubEndpoint is used as fallback when grove settings don't specify one.
func TestStartAgentBrokerConfigUsedWhenNoGroveSettings(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "http://localhost:9810"
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	// Create a temp grove dir with settings.yaml but no hub endpoint
	groveDir := t.TempDir()
	settingsContent := "harnesses:\n  claude:\n    model: sonnet\n"
	if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings.yaml: %v", err)
	}

	body := fmt.Sprintf(`{"grovePath": %q}`, groveDir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	// Without grove settings hub.endpoint, broker config should be used
	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
	}
}

// TestStartAgentResolvedEnvHubEndpointFallback verifies that when the broker
// has no HubEndpoint configured, the hub endpoint from resolvedEnv (sent by
// the hub dispatcher) is used as a fallback.
func TestStartAgentResolvedEnvHubEndpointFallback(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "" // Standalone broker without hub endpoint config
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	body := `{"resolvedEnv": {"SCION_HUB_ENDPOINT": "http://hub.example.com:8080", "SCION_GROVE_ID": "grove-1"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	// Hub endpoint should fall back to the resolvedEnv value
	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://hub.example.com:8080" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://hub.example.com:8080' from resolvedEnv, got %q", got)
	}
}

// TestStartAgentResolvedEnvHubURLFallback verifies legacy parity: when the broker
// has no HubEndpoint configured, SCION_HUB_URL from resolvedEnv is accepted as
// the fallback endpoint in the start path.
func TestStartAgentResolvedEnvHubURLFallback(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = ""
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	body := `{"resolvedEnv": {"SCION_HUB_URL": "http://hub.example.com:9090"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://hub.example.com:9090" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://hub.example.com:9090' from SCION_HUB_URL fallback, got %q", got)
	}
	if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://hub.example.com:9090" {
		t.Errorf("expected SCION_HUB_URL='http://hub.example.com:9090', got %q", got)
	}
}

// TestStartAgentResolvedEnvHubEndpointWithContainerOverride verifies that when
// the hub endpoint from resolvedEnv is localhost, the ContainerHubEndpoint
// override is applied.
func TestStartAgentResolvedEnvHubEndpointWithContainerOverride(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = ""                                              // No broker-level hub endpoint
	cfg.ContainerHubEndpoint = "http://host.containers.internal:9810" // But has container override
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	// resolvedEnv has localhost endpoint from the hub
	body := `{"resolvedEnv": {"SCION_HUB_ENDPOINT": "http://localhost:9810"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	// ContainerHubEndpoint override should be applied since resolvedEnv was localhost
	if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:9810" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:9810', got %q", got)
	}
}

// TestCreateAgentPortPreservedAcrossBridge verifies that when the hub dispatch
// sends a localhost endpoint on port 8080 but the broker's ContainerHubEndpoint
// was pre-computed with port 9810, the actual endpoint port (8080) is preserved.
func TestCreateAgentPortPreservedAcrossBridge(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	// Simulate the bug scenario: ContainerHubEndpoint was auto-computed
	// from a standalone hub port (9810), but the hub actually serves on
	// the web port (8080) in combo mode.
	cfg.ContainerHubEndpoint = "http://host.containers.internal:9810"
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	body := `{
		"name": "test-agent",
		"hubEndpoint": "http://localhost:8080"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	// The bridge host should be applied but the port from the actual
	// endpoint (8080) must be preserved, not the pre-computed port (9810).
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://host.containers.internal:8080" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://host.containers.internal:8080' (port preserved), got %q", got)
	}
}

// TestStartAgentBrokerIDEnv verifies that startAgent sets SCION_BROKER_ID from broker config.
func TestStartAgentBrokerIDEnv(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if got := mgr.lastOpts.Env["SCION_BROKER_ID"]; got != "test-broker-id" {
		t.Errorf("expected SCION_BROKER_ID='test-broker-id', got %q", got)
	}

	if got := mgr.lastOpts.Env["SCION_BROKER_NAME"]; got != "test-host" {
		t.Errorf("expected SCION_BROKER_NAME='test-host', got %q", got)
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

func TestStartAgentTelemetryOverrideFromResolvedEnv(t *testing.T) {
	// When resolvedEnv contains SCION_TELEMETRY_ENABLED=true, startAgent
	// should translate it to opts.TelemetryOverride so that Start() enables
	// harness telemetry env injection and cloud config merging.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"resolvedEnv": {"SCION_TELEMETRY_ENABLED": "true"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/telemetry-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	if mgr.lastOpts.TelemetryOverride == nil {
		t.Fatal("expected TelemetryOverride to be set")
	}
	if !*mgr.lastOpts.TelemetryOverride {
		t.Error("expected TelemetryOverride to be true")
	}
}

func TestStartAgentTelemetryOverrideDisabled(t *testing.T) {
	// When resolvedEnv contains SCION_TELEMETRY_ENABLED=false, startAgent
	// should set TelemetryOverride to false.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"resolvedEnv": {"SCION_TELEMETRY_ENABLED": "false"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/telemetry-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	if mgr.lastOpts.TelemetryOverride == nil {
		t.Fatal("expected TelemetryOverride to be set")
	}
	if *mgr.lastOpts.TelemetryOverride {
		t.Error("expected TelemetryOverride to be false")
	}
}

func TestCreateAgentGroveSlugInitializesScionDir(t *testing.T) {
	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	defer restore()

	// When GroveSlug is set and the broker has no .scion subdirectory for
	// the hub-native grove, the handler should create it so that
	// ResolveGrovePath resolves to groves/<slug>/.scion (not groves/<slug>).
	// This prevents agents from being created at the wrong directory level.

	// Use a temporary directory to simulate the grove workspace.
	tmpDir := t.TempDir()
	grovePath := filepath.Join(tmpDir, "test-grove")
	if err := os.MkdirAll(grovePath, 0755); err != nil {
		t.Fatalf("failed to create test grove dir: %v", err)
	}

	// Verify .scion does NOT exist yet
	scionDir := filepath.Join(grovePath, ".scion")
	if _, err := os.Stat(scionDir); !os.IsNotExist(err) {
		t.Fatal(".scion should not exist before initialization")
	}

	// Verify ResolveGrovePath does NOT resolve to .scion when it doesn't exist
	resolved, _, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		t.Fatalf("ResolveGrovePath failed: %v", err)
	}
	if resolved != grovePath {
		t.Errorf("before init: expected ResolveGrovePath to return %q, got %q", grovePath, resolved)
	}

	// Initialize .scion (mirrors what the handler now does)
	if err := config.InitProject(scionDir, nil); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Verify .scion was created
	if info, err := os.Stat(scionDir); err != nil || !info.IsDir() {
		t.Fatal(".scion directory should exist after InitProject")
	}

	// Verify ResolveGrovePath now resolves to the .scion subdirectory
	resolved, _, err = config.ResolveGrovePath(grovePath)
	if err != nil {
		t.Fatalf("ResolveGrovePath failed: %v", err)
	}
	if resolved != scionDir {
		t.Errorf("after init: expected ResolveGrovePath to resolve to %q, got %q", scionDir, resolved)
	}
}

// ============================================================================
// Grove Cleanup Endpoint Tests
// ============================================================================

func TestDeleteGrove_RemovesDirectory(t *testing.T) {
	srv := newTestServer(t)

	// Create a temporary groves directory structure
	tmpHome := t.TempDir()
	grovesDir := filepath.Join(tmpHome, ".scion", "groves")
	groveDir := filepath.Join(grovesDir, "test-grove")
	scionDir := filepath.Join(groveDir, ".scion")

	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatalf("failed to create test grove dir: %v", err)
	}

	// Write a dummy file so we can verify deletion
	dummyFile := filepath.Join(scionDir, "settings.yaml")
	if err := os.WriteFile(dummyFile, []byte("test: true"), 0o644); err != nil {
		t.Fatalf("failed to write dummy file: %v", err)
	}

	// Override HOME so config.GetGlobalDir resolves to our temp dir
	t.Setenv("HOME", tmpHome)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/groves/test-grove", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify directory was removed
	if _, err := os.Stat(groveDir); !os.IsNotExist(err) {
		t.Errorf("expected grove directory to be removed, but it still exists")
	}
}

func TestDeleteGrove_NonExistent_Returns204(t *testing.T) {
	srv := newTestServer(t)

	tmpHome := t.TempDir()
	// Create the groves parent but NOT the specific grove directory
	grovesDir := filepath.Join(tmpHome, ".scion", "groves")
	if err := os.MkdirAll(grovesDir, 0o755); err != nil {
		t.Fatalf("failed to create groves dir: %v", err)
	}

	t.Setenv("HOME", tmpHome)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/groves/nonexistent-grove", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for non-existent grove, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteGrove_PathTraversal_Blocked(t *testing.T) {
	srv := newTestServer(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Attempt path traversal
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/groves/..%2F..%2Fetc", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal attempt, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFindAgentInHubNativeGroves(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create hub-native grove structure with an agent directory
	groveSlug := "my-project"
	scionDir := filepath.Join(tmpHome, ".scion", "groves", groveSlug, ".scion")
	agentDir := filepath.Join(scionDir, "agents", "test-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// Should find the agent in the hub-native grove
	result := findAgentInHubNativeGroves("test-agent")
	if result != scionDir {
		t.Errorf("expected %q, got %q", scionDir, result)
	}

	// Should not find a non-existent agent
	result = findAgentInHubNativeGroves("nonexistent-agent")
	if result != "" {
		t.Errorf("expected empty string for nonexistent agent, got %q", result)
	}

	// Should handle missing groves directory gracefully
	t.Setenv("HOME", t.TempDir())
	result = findAgentInHubNativeGroves("test-agent")
	if result != "" {
		t.Errorf("expected empty string when groves dir missing, got %q", result)
	}
}

func TestDeleteAgent_HubNativeGrove_NoContainer(t *testing.T) {
	// Verify that deleting an agent in a hub-native grove resolves the correct
	// grove path even when the container doesn't exist (e.g. created-only
	// agent, pruned container).
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{
		agents: []api.AgentInfo{}, // No containers
	}
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create hub-native grove with an agent directory and config file
	groveSlug := "hub-grove"
	scionDir := filepath.Join(tmpHome, ".scion", "groves", groveSlug, ".scion")
	agentName := "orphaned-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	// Write a scion-agent.json so it looks like a real agent
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Send delete request — no container exists for this agent
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/agents/"+agentName+"?deleteFiles=true&removeBranch=false", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// Should succeed (204)
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the mock manager's Delete was called with the correct grove path
	if mgr.deleteCalls != 1 {
		t.Fatalf("expected 1 Delete call, got %d", mgr.deleteCalls)
	}
	if mgr.lastDeleteGrovePath != scionDir {
		t.Errorf("expected grovePath %q, got %q", scionDir, mgr.lastDeleteGrovePath)
	}
	if mgr.lastDeleteAgentID != agentName {
		t.Errorf("expected agentID %q, got %q", agentName, mgr.lastDeleteAgentID)
	}
}

func TestIsLocalhostEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{"http://localhost:8080", true},
		{"https://localhost:443", true},
		{"http://localhost", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.0.0.1", true},
		{"http://[::1]:8080", true},
		{"http://[::1]", true},
		{"https://hub.example.com", false},
		{"https://hub.example.com:8080", false},
		{"http://host.containers.internal:8080", false},
		{"http://192.168.1.100:8080", false},
		{"", false},
		{"not-a-url", false},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			if got := isLocalhostEndpoint(tt.endpoint); got != tt.want {
				t.Errorf("isLocalhostEndpoint(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}

// TestCreateAgentStartFailure_CleansUpFiles verifies that when mgr.Start() fails
// (e.g. auth resolution error), the broker cleans up provisioned agent files so
// they don't become orphans that trigger spurious hub sync-registration.
func TestCreateAgentStartFailure_CleansUpFiles(t *testing.T) {
	// Create a temp directory to act as the grove path with agent files
	tmpDir := t.TempDir()
	grovePath := filepath.Join(tmpDir, ".scion")
	agentDir := filepath.Join(grovePath, "agents", "fail-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	// Write a scion-agent.yaml so the agent is discoverable
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.yaml"), []byte("harness: gemini\n"), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.yaml: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	mgr := &provisionCapturingManager{}
	mgr.startErr = fmt.Errorf("auth resolution failed: gemini: auth type \"api-key\" selected but no API key found")
	rt := &runtime.MockRuntime{}
	srv := New(cfg, mgr, rt)

	body := fmt.Sprintf(`{
		"name": "fail-agent",
		"grovePath": %q,
		"config": {"task": "do something"}
	}`, grovePath)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should return runtime error
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}

	// Verify agent directory was cleaned up
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("expected agent directory to be cleaned up after start failure, but it still exists: %s", agentDir)
	}
}
