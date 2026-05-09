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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/go-jose/go-jose/v4/jwt"
)

// testDevToken is the development token used for testing.
const testDevToken = "scion_dev_test_token_for_unit_tests_1234567890"

// testServer creates a test server with an in-memory SQLite store.
// The server is configured with dev auth enabled using testDevToken.
func testServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken // Enable dev auth for testing
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv, s
}

// doRequest performs an HTTP request against the test server.
// It automatically includes the dev auth token for authenticated endpoints.
func doRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Add dev auth token for authenticated endpoints
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doRequestNoAuth performs an HTTP request without authentication.
// Use this for testing unauthenticated access or auth endpoints themselves.
func doRequestNoAuth(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doRequestRaw performs an HTTP request with raw bytes as the body.
// Useful for testing malformed request bodies.
func doRequestRaw(t *testing.T, srv *Server, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ============================================================================
// Health Endpoint Tests
// ============================================================================

func TestHealthz(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/healthz", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", resp.Status)
	}

	// ScionVersion should be populated (may be "unknown" in test builds)
	if resp.ScionVersion == "" {
		t.Error("expected scionVersion to be non-empty")
	}
}

func TestReadyz(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ready" {
		t.Errorf("expected status 'ready', got %q", resp["status"])
	}
}

// ============================================================================
// Agent Endpoint Tests
// ============================================================================

func TestAgentList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project first (agents reference projects)
	project := &store.Project{
		ID:        "project_test123",
		Slug:      "test-project",
		Name:      "Test Project",
		GitRemote: "https://github.com/test/repo",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create some test agents
	for i := 0; i < 3; i++ {
		agent := &store.Agent{
			ID:           "agent_" + string(rune('a'+i)),
			Slug:         "test-agent-" + string(rune('a'+i)),
			Name:         "Test Agent " + string(rune('A'+i)),
			ProjectID:      project.ID,
			Phase:        string(state.PhaseStopped),
			StateVersion: 1,
			Created:      time.Now(),
			Updated:      time.Now(),
		}
		if err := s.CreateAgent(ctx, agent); err != nil {
			t.Fatalf("failed to create agent: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListAgentsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Agents) != 3 {
		t.Errorf("expected 3 agents, got %d", len(resp.Agents))
	}

	if resp.TotalCount != 3 {
		t.Errorf("expected total 3, got %d", resp.TotalCount)
	}
}

func TestAgentCreate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker first
	broker := &store.RuntimeBroker{
		ID:     "host_test123",
		Slug:   "test-host",
		Name:   "Test Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a project with default runtime broker
	project := &store.Project{
		ID:                     "project_abc123",
		Slug:                   "my-project",
		Name:                   "My Project",
		GitRemote:              "github.com/test/repo",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Register the broker as a provider to the project
	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	body := map[string]interface{}{
		"name":    "New Agent",
		"projectId": project.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be set")
	}

	if resp.Agent.Slug != "new-agent" {
		t.Errorf("expected agentId 'new-agent', got %q", resp.Agent.Slug)
	}

	if resp.Agent.ID == "" {
		t.Error("expected ID to be set")
	}

	if resp.Agent.Phase != string(state.PhaseCreated) {
		t.Errorf("expected status 'pending', got %q", resp.Agent.Phase)
	}

	if resp.Agent.RuntimeBrokerID != broker.ID {
		t.Errorf("expected runtimeBrokerId %q, got %q", broker.ID, resp.Agent.RuntimeBrokerID)
	}
}

// TestAgentCreate_NoTask tests that creating an agent without a task succeeds
// and leaves the agent in pending status (provision-only, for "scion create").
func TestAgentCreate_NoTask(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "host_notask",
		Slug:   "notask-host",
		Name:   "No Task Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a project with default runtime broker
	project := &store.Project{
		ID:                     "project_notask",
		Slug:                   "notask-project",
		Name:                   "No Task Project",
		GitRemote:              "github.com/test/notask",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Register the broker as a provider
	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Create agent without a task via /api/v1/agents
	body := map[string]interface{}{
		"name":    "Taskless Agent",
		"projectId": project.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be set")
	}

	if resp.Agent.Phase != string(state.PhaseCreated) {
		t.Errorf("expected status 'pending', got %q", resp.Agent.Phase)
	}

	if resp.Agent.Slug != "taskless-agent" {
		t.Errorf("expected slug 'taskless-agent', got %q", resp.Agent.Slug)
	}
}

// TestAgentCreate_NoTaskViaProject tests creating an agent without a task via the project endpoint.
func TestAgentCreate_NoTaskViaProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "host_notask_project",
		Slug:   "notask-project-host",
		Name:   "No Task Project Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a project with default runtime broker
	project := &store.Project{
		ID:                     "project_notask_project",
		Slug:                   "notask-project-ep",
		Name:                   "No Task Project EP",
		GitRemote:              "github.com/test/notask-project",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Register the broker as a provider
	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Create agent without a task via /api/v1/projects/{id}/agents
	body := map[string]interface{}{
		"name": "Project Taskless Agent",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be set")
	}

	if resp.Agent.Phase != string(state.PhaseCreated) {
		t.Errorf("expected status 'pending', got %q", resp.Agent.Phase)
	}
}

// TestAgentCreate_AttachNoTask tests that creating an agent with attach=true but no task
// succeeds. Tasks are always optional; attach signals interactive mode to the harness.
func TestAgentCreate_AttachNoTask(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "host_attach",
		Slug:   "attach-host",
		Name:   "Attach Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a project with default runtime broker
	project := &store.Project{
		ID:                     "project_attach",
		Slug:                   "attach-project",
		Name:                   "Attach Project",
		GitRemote:              "github.com/test/attach",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Register the broker as a provider
	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Create agent with attach=true but no task
	body := map[string]interface{}{
		"name":    "Attach Agent",
		"projectId": project.ID,
		"attach":  true,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent to be set")
	}

	// Without a dispatcher, agent stays in pending status (dispatch is a no-op)
	// but the request itself should succeed
	if resp.Agent.Slug != "attach-agent" {
		t.Errorf("expected slug 'attach-agent', got %q", resp.Agent.Slug)
	}
}

// TestAgentCreate_SingleProvider tests that when a project has no default runtime broker
// but has exactly one online provider, that provider is used automatically.
func TestAgentCreate_SingleProvider(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "host_single",
		Slug:   "single-host",
		Name:   "Single Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a project WITHOUT a default runtime broker
	project := &store.Project{
		ID:        "project_single",
		Slug:      "single-project",
		Name:      "Single Project",
		GitRemote: "github.com/test/single",
		// Note: DefaultRuntimeBrokerID is NOT set
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Register the broker as the only provider to the project
	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Create agent without specifying runtimeBrokerId
	body := map[string]interface{}{
		"name":    "Auto Resolved Agent",
		"projectId": project.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should automatically use the single provider
	if resp.Agent.RuntimeBrokerID != broker.ID {
		t.Errorf("expected runtimeBrokerId %q (single provider), got %q", broker.ID, resp.Agent.RuntimeBrokerID)
	}
}

// TestAgentCreate_SingleOfflineProvider ensures a single provider is not auto-selected
// unless it is online.
func TestAgentCreate_SingleOfflineProvider(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     "host_single_offline",
		Slug:   "single-host-offline",
		Name:   "Single Host Offline",
		Status: store.BrokerStatusOffline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	project := &store.Project{
		ID:        "project_single_offline",
		Slug:      "single-project-offline",
		Name:      "Single Project Offline",
		GitRemote: "github.com/test/single-offline",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOffline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	body := map[string]interface{}{
		"name":    "No Auto Resolve Agent",
		"projectId": project.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected status 422, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeNoRuntimeBroker {
		t.Fatalf("expected error code %q, got %q", ErrCodeNoRuntimeBroker, errResp.Error.Code)
	}
}

// TestAgentCreate_MultipleProviders tests that when a project has multiple online providers
// but no default runtime broker, an error is returned requiring explicit selection.
func TestAgentCreate_MultipleProviders(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two runtime brokers
	broker1 := &store.RuntimeBroker{
		ID:     "host_multi1",
		Slug:   "multi-host-1",
		Name:   "Multi Host 1",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker1); err != nil {
		t.Fatalf("failed to create runtime broker 1: %v", err)
	}

	broker2 := &store.RuntimeBroker{
		ID:     "host_multi2",
		Slug:   "multi-host-2",
		Name:   "Multi Host 2",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker2); err != nil {
		t.Fatalf("failed to create runtime broker 2: %v", err)
	}

	// Create a project WITHOUT a default runtime broker
	project := &store.Project{
		ID:        "project_multi",
		Slug:      "multi-project",
		Name:      "Multi Project",
		GitRemote: "github.com/test/multi",
		// Note: DefaultRuntimeBrokerID is NOT set
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Register both brokers as providers to the project
	contrib1 := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker1.ID,
		BrokerName: broker1.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib1); err != nil {
		t.Fatalf("failed to add project provider 1: %v", err)
	}

	contrib2 := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker2.ID,
		BrokerName: broker2.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib2); err != nil {
		t.Fatalf("failed to add project provider 2: %v", err)
	}

	// Attempt to create agent without specifying runtimeBrokerId
	body := map[string]interface{}{
		"name":    "Ambiguous Agent",
		"projectId": project.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	// Should fail with 422 because multiple brokers are available and explicit selection is required
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected status 422, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Error.Code != ErrCodeNoRuntimeBroker {
		t.Errorf("expected error code %q, got %q", ErrCodeNoRuntimeBroker, errResp.Error.Code)
	}

	// Should include available brokers in the response details
	availableBrokers, ok := errResp.Error.Details["availableBrokers"].([]interface{})
	if !ok {
		t.Fatalf("expected availableBrokers in error details, got %v", errResp.Error.Details)
	}
	if len(availableBrokers) != 2 {
		t.Errorf("expected 2 available brokers in error, got %d", len(availableBrokers))
	}
}

func TestAgentGetByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project and agent
	project := &store.Project{
		ID:        "project_xyz",
		Slug:      "project-xyz",
		Name:      "Project XYZ",
		GitRemote: "https://github.com/test/repo",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:           "agent_test1",
		Slug:         "test-agent",
		Name:         "Test Agent",
		ProjectID:      project.ID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/agent_test1", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Agent
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != "agent_test1" {
		t.Errorf("expected ID 'agent_test1', got %q", resp.ID)
	}
}

func TestAgentNotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/nonexistent", nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Error.Code != ErrCodeNotFound {
		t.Errorf("expected error code %q, got %q", ErrCodeNotFound, resp.Error.Code)
	}
}

func TestAgentDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project and agent
	project := &store.Project{
		ID:        "project_del",
		Slug:      "project-del",
		Name:      "Project Del",
		GitRemote: "https://github.com/test/repo",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:           "agent_delete",
		Slug:         "delete-me",
		Name:         "Delete Me",
		ProjectID:      project.ID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/agent_delete", nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify agent is deleted
	_, err := s.GetAgent(ctx, "agent_delete")
	if err == nil {
		t.Error("expected agent to be deleted")
	}
}

// ============================================================================
// Project Endpoint Tests
// ============================================================================

func TestProjectList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		project := &store.Project{
			ID:        "project_" + string(rune('a'+i)),
			Slug:      "project-" + string(rune('a'+i)),
			Name:      "Project " + string(rune('A'+i)),
			GitRemote: "https://github.com/test/repo" + string(rune('a'+i)),
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		if err := s.CreateProject(ctx, project); err != nil {
			t.Fatalf("failed to create project: %v", err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListProjectsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Projects) != 2 {
		t.Errorf("expected 2 projects, got %d", len(resp.Projects))
	}
}

func TestProjectRegister(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"gitRemote": "https://github.com/test/my-project.git",
		"name":      "My Project",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)

	// Project register always returns 200 (idempotent)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp RegisterProjectResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Project.ID == "" {
		t.Error("expected project ID to be set")
	}

	if !resp.Created {
		t.Error("expected created to be true for new project")
	}

	// The git remote should be normalized (no scheme, no .git suffix)
	if resp.Project.GitRemote != "github.com/test/my-project" {
		t.Errorf("expected normalized git remote 'github.com/test/my-project', got %q", resp.Project.GitRemote)
	}
}

func TestProjectRegisterIdempotent(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"gitRemote": "https://github.com/test/idempotent-repo",
		"name":      "Idempotent Repo",
	}

	// First registration
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 RegisterProjectResponse
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp1.Created {
		t.Error("expected created to be true for first registration")
	}

	// Second registration with same git remote
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200 for idempotent call, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 RegisterProjectResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should return the same project
	if resp1.Project.ID != resp2.Project.ID {
		t.Errorf("expected same project ID on idempotent call, got %q and %q", resp1.Project.ID, resp2.Project.ID)
	}

	// Second call should not have created=true
	if resp2.Created {
		t.Error("expected created to be false on second call")
	}
}

func TestProjectRegisterCaseInsensitive(t *testing.T) {
	srv, _ := testServer(t)

	// First registration with "Global" (title case)
	body1 := map[string]interface{}{
		"name": "Global",
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 RegisterProjectResponse
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp1.Created {
		t.Error("expected created to be true for first registration")
	}

	// Second registration with "global" (lowercase) - should match existing project
	body2 := map[string]interface{}{
		"name": "global",
	}

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200 for idempotent call, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 RegisterProjectResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should return the same project (case-insensitive match)
	if resp1.Project.ID != resp2.Project.ID {
		t.Errorf("expected same project ID for case-insensitive match, got %q and %q", resp1.Project.ID, resp2.Project.ID)
	}

	// Second call should not have created=true
	if resp2.Created {
		t.Error("expected created to be false for case-insensitive match")
	}
}

func TestProjectRegisterMultipleGitRemoteMatches(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Pre-create two projects for the same git remote.
	project1 := &store.Project{
		ID:        "project-1",
		Name:      "widgets",
		Slug:      "widgets",
		GitRemote: "github.com/acme/widgets",
	}
	project2 := &store.Project{
		ID:        "project-2",
		Name:      "widgets (2)",
		Slug:      "widgets-2",
		GitRemote: "github.com/acme/widgets",
	}
	if err := s.CreateProject(ctx, project1); err != nil {
		t.Fatalf("failed to create project1: %v", err)
	}
	if err := s.CreateProject(ctx, project2); err != nil {
		t.Fatalf("failed to create project2: %v", err)
	}

	// Register with the same git remote — should create a new project
	// and include matches for disambiguation.
	body := map[string]interface{}{
		"name":      "widgets",
		"gitRemote": "https://github.com/acme/widgets.git",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp RegisterProjectResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// A new project should be created (not linked to either existing one).
	if !resp.Created {
		t.Error("expected created=true when multiple git remote matches exist")
	}

	// The response should include the two existing matches.
	if len(resp.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(resp.Matches))
	}

	matchIDs := map[string]bool{}
	for _, m := range resp.Matches {
		matchIDs[m.ID] = true
	}
	if !matchIDs["project-1"] || !matchIDs["project-2"] {
		t.Errorf("expected matches to include project-1 and project-2, got %v", resp.Matches)
	}

	// The newly created project should have a serial slug.
	// NextAvailableSlug fills gaps, so with "widgets" and "widgets-2" taken,
	// the next available is "widgets-1".
	if resp.Project.Slug != "widgets-1" {
		t.Errorf("expected serial slug 'widgets-1', got %q", resp.Project.Slug)
	}
}

func TestProjectRegisterBrokerDeduplication(t *testing.T) {
	srv, _ := testServer(t)

	// Register a project with a broker
	body1 := map[string]interface{}{
		"name":      "Test Project",
		"gitRemote": "https://github.com/test/dedup-host",
		"broker": map[string]interface{}{
			"name":    "test-host",
			"version": "1.0.0",
		},
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 RegisterProjectResponse
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	brokerID1 := resp1.Broker.ID

	// Register another project with the same broker name (case-insensitive)
	body2 := map[string]interface{}{
		"name":      "Another Project",
		"gitRemote": "https://github.com/test/another-project",
		"broker": map[string]interface{}{
			"name":    "TEST-HOST", // Different case
			"version": "1.0.1",
		},
	}

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 RegisterProjectResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should reuse the same broker (case-insensitive match)
	if resp1.Broker.ID != resp2.Broker.ID {
		t.Errorf("expected same broker ID for case-insensitive match, got %q and %q", brokerID1, resp2.Broker.ID)
	}

	// The version should be updated
	if resp2.Broker.Version != "1.0.1" {
		t.Errorf("expected broker version to be updated to '1.0.1', got %q", resp2.Broker.Version)
	}
}

func TestProjectRegisterWithBrokerID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// First, create a broker directly (simulating Phase 1 + 2 of two-phase flow)
	broker := &store.RuntimeBroker{
		ID:     "host_twophase_test",
		Name:   "Two Phase Test Host",
		Slug:   "two-phase-test-host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Now register project with brokerId (Phase 3)
	body := map[string]interface{}{
		"name":      "Two Phase Project",
		"gitRemote": "https://github.com/test/twophase-project",
		"brokerId":  broker.ID,
		"path":      "/path/to/project/.scion",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp RegisterProjectResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Project.ID == "" {
		t.Error("expected project ID to be set")
	}

	if !resp.Created {
		t.Error("expected created to be true for new project")
	}

	// Broker should be populated in response
	if resp.Broker == nil {
		t.Error("expected broker to be set in response")
	} else if resp.Broker.ID != broker.ID {
		t.Errorf("expected broker ID %q, got %q", broker.ID, resp.Broker.ID)
	}

	// Should NOT have secretKey (two-phase flow doesn't generate secrets in project registration)
	if resp.SecretKey != "" {
		t.Error("expected secretKey to be empty in new two-phase flow")
	}

	// Verify provider was created
	providers, err := s.GetProjectProviders(ctx, resp.Project.ID)
	if err != nil {
		t.Fatalf("failed to get providers: %v", err)
	}
	if len(providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(providers))
	}
	if providers[0].BrokerID != broker.ID {
		t.Errorf("expected provider broker ID %q, got %q", broker.ID, providers[0].BrokerID)
	}
	if providers[0].LocalPath != "/path/to/project/.scion" {
		t.Errorf("expected localPath '/path/to/project/.scion', got %q", providers[0].LocalPath)
	}
}

func TestProjectRegisterWithInvalidBrokerID(t *testing.T) {
	srv, _ := testServer(t)

	// Try to register project with non-existent brokerId
	body := map[string]interface{}{
		"name":      "Invalid Host Project",
		"gitRemote": "https://github.com/test/invalid-host-project",
		"brokerId":  "non-existent-host-id",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 (validation error), got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Error.Code != ErrCodeValidationError {
		t.Errorf("expected error code %q, got %q", ErrCodeValidationError, errResp.Error.Code)
	}
}

func TestAddProvider(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:        "project_contrib_test",
		Slug:      "contrib-test",
		Name:      "Provider Test Project",
		GitRemote: "https://github.com/test/contrib-test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a broker
	broker := &store.RuntimeBroker{
		ID:     "host_contrib_test",
		Name:   "Provider Test Host",
		Slug:   "contrib-test-host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add provider via API
	body := map[string]interface{}{
		"brokerId":  broker.ID,
		"localPath": "/home/user/project/.scion",
		"mode":      "connected",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/providers", body)
	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp AddProviderResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Provider == nil {
		t.Fatal("expected provider in response")
	}
	if resp.Provider.BrokerID != broker.ID {
		t.Errorf("expected broker ID %q, got %q", broker.ID, resp.Provider.BrokerID)
	}
	if resp.Provider.LocalPath != "/home/user/project/.scion" {
		t.Errorf("expected localPath, got %q", resp.Provider.LocalPath)
	}

	// Verify project now has default runtime broker set
	updatedProject, err := s.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("failed to get updated project: %v", err)
	}
	if updatedProject.DefaultRuntimeBrokerID != broker.ID {
		t.Errorf("expected default runtime broker to be set to %q, got %q", broker.ID, updatedProject.DefaultRuntimeBrokerID)
	}
}

func TestListProviders(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:      "project_list_contrib",
		Slug:    "list-contrib",
		Name:    "List Providers Project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create and add a broker as provider
	broker := &store.RuntimeBroker{
		ID:     "host_list_contrib",
		Name:   "List Providers Host",
		Slug:   "list-contrib-host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/test/path",
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add provider: %v", err)
	}

	// List providers
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/providers", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string][]store.ProjectProvider
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	providers := resp["providers"]
	if len(providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(providers))
	}
	if providers[0].BrokerID != broker.ID {
		t.Errorf("expected broker ID %q, got %q", broker.ID, providers[0].BrokerID)
	}
}

func TestProjectGetByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:        "project_gettest",
		Slug:      "get-test",
		Name:      "Get Test",
		GitRemote: "https://github.com/test/get-test",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/project_gettest", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.Project
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != "project_gettest" {
		t.Errorf("expected ID 'project_gettest', got %q", resp.ID)
	}
}

// ============================================================================
// RuntimeBroker Endpoint Tests
// ============================================================================

func TestRuntimeBrokerList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:            "host_test1",
		Name:          "Test Host",
		Slug:          "test-host",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}
}

func TestRuntimeBrokerListByName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two brokers with different names
	broker1 := &store.RuntimeBroker{
		ID:            "host_name_test1",
		Name:          "Alpha Host",
		Slug:          "alpha-host",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	broker2 := &store.RuntimeBroker{
		ID:            "host_name_test2",
		Name:          "Beta Host",
		Slug:          "beta-host",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker1); err != nil {
		t.Fatalf("failed to create runtime broker 1: %v", err)
	}
	if err := s.CreateRuntimeBroker(ctx, broker2); err != nil {
		t.Fatalf("failed to create runtime broker 2: %v", err)
	}

	// Test filter by exact name
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?name=Alpha+Host", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}
	if len(resp.Brokers) > 0 && resp.Brokers[0].Name != "Alpha Host" {
		t.Errorf("expected broker name 'Alpha Host', got %q", resp.Brokers[0].Name)
	}

	// Test case-insensitive filter
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?name=beta+host", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}

	// Test no match
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?name=nonexistent", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 0 {
		t.Errorf("expected 0 brokers, got %d", len(resp.Brokers))
	}
}

func TestRuntimeBrokerDeleteCascadesProviders(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a broker
	broker := &store.RuntimeBroker{
		ID:      "broker_cascade_test",
		Name:    "Cascade Test Broker",
		Slug:    "cascade-test-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create two projects, one with default_runtime_broker_id pointing to this broker
	project1 := &store.Project{
		ID:                     "project_cascade_1",
		Name:                   "Cascade Project 1",
		Slug:                   "cascade-project-1",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	project2 := &store.Project{
		ID:      "project_cascade_2",
		Name:    "Cascade Project 2",
		Slug:    "cascade-project-2",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project1); err != nil {
		t.Fatalf("failed to create project 1: %v", err)
	}
	if err := s.CreateProject(ctx, project2); err != nil {
		t.Fatalf("failed to create project 2: %v", err)
	}

	// Add broker as provider to both projects
	for _, projectID := range []string{project1.ID, project2.ID} {
		provider := &store.ProjectProvider{
			ProjectID:    projectID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			Status:     store.BrokerStatusOnline,
		}
		if err := s.AddProjectProvider(ctx, provider); err != nil {
			t.Fatalf("failed to add project provider for %s: %v", projectID, err)
		}
	}

	// Verify providers exist before deletion
	providers1, err := s.GetProjectProviders(ctx, project1.ID)
	if err != nil {
		t.Fatalf("failed to get providers for project 1: %v", err)
	}
	if len(providers1) != 1 {
		t.Fatalf("expected 1 provider for project 1, got %d", len(providers1))
	}

	// Delete the broker via the API
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/runtime-brokers/"+broker.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the broker is gone
	_, err = s.GetRuntimeBroker(ctx, broker.ID)
	if err == nil {
		t.Error("expected broker to be deleted, but it still exists")
	}

	// Verify provider records are gone from both projects
	providers1, err = s.GetProjectProviders(ctx, project1.ID)
	if err != nil {
		t.Fatalf("failed to get providers for project 1 after deletion: %v", err)
	}
	if len(providers1) != 0 {
		t.Errorf("expected 0 providers for project 1 after broker deletion, got %d", len(providers1))
	}

	providers2, err := s.GetProjectProviders(ctx, project2.ID)
	if err != nil {
		t.Fatalf("failed to get providers for project 2 after deletion: %v", err)
	}
	if len(providers2) != 0 {
		t.Errorf("expected 0 providers for project 2 after broker deletion, got %d", len(providers2))
	}

	// Verify default_runtime_broker_id was cleared on project1
	g1, err := s.GetProject(ctx, project1.ID)
	if err != nil {
		t.Fatalf("failed to get project 1 after deletion: %v", err)
	}
	if g1.DefaultRuntimeBrokerID != "" {
		t.Errorf("expected default_runtime_broker_id to be cleared on project 1, got %q", g1.DefaultRuntimeBrokerID)
	}
}

func TestRuntimeBrokerGetByID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:            "host_gettest",
		Name:          "Get Test Host",
		Slug:          "get-test-host",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/host_gettest", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.RuntimeBroker
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ID != "host_gettest" {
		t.Errorf("expected ID 'host_gettest', got %q", resp.ID)
	}
}

func TestRuntimeBrokerGetByID_CreatedByName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user to be the broker creator
	if err := s.CreateUser(ctx, &store.User{
		ID:          "user_broker_creator",
		Email:       "creator@test.com",
		DisplayName: "Broker Creator",
		Role:        "member",
		Status:      "active",
	}); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:            "broker_createdby_test",
		Name:          "CreatedBy Test Broker",
		Slug:          "createdby-test-broker",
		Status:        store.BrokerStatusOnline,
		CreatedBy:     "user_broker_creator",
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/broker_createdby_test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp RuntimeBrokerWithCapabilities
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.CreatedByName != "Broker Creator" {
		t.Errorf("expected createdByName 'Broker Creator', got %q", resp.CreatedByName)
	}

	// Dev user is admin, so should have all capabilities
	if resp.Cap == nil {
		t.Fatal("expected capabilities to be set")
	}
	found := false
	for _, action := range resp.Cap.Actions {
		if action == "dispatch" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'dispatch' in capabilities, got %v", resp.Cap.Actions)
	}
}

func TestRuntimeBrokerGetByID_CreatedByNameFallsBackToEmail(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user with no display name
	if err := s.CreateUser(ctx, &store.User{
		ID:     "user_no_display",
		Email:  "nodisplay@test.com",
		Role:   "member",
		Status: "active",
	}); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:            "broker_email_fallback",
		Name:          "Email Fallback Broker",
		Slug:          "email-fallback-broker",
		Status:        store.BrokerStatusOnline,
		CreatedBy:     "user_no_display",
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/broker_email_fallback", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp RuntimeBrokerWithCapabilities
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.CreatedByName != "nodisplay@test.com" {
		t.Errorf("expected createdByName 'nodisplay@test.com', got %q", resp.CreatedByName)
	}
}

func TestRuntimeBrokerList_Capabilities(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:            "broker_caps_list",
		Name:          "Caps List Broker",
		Slug:          "caps-list-broker",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersWithCapsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Fatalf("expected 1 broker, got %d", len(resp.Brokers))
	}

	if resp.Brokers[0].Cap == nil {
		t.Fatal("expected capabilities to be set on listed broker")
	}
}

func TestRuntimeBrokerList_CreatedByName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user to be the broker creator
	if err := s.CreateUser(ctx, &store.User{
		ID:          "user_list_creator",
		Email:       "listcreator@test.com",
		DisplayName: "List Creator",
		Role:        "member",
		Status:      "active",
	}); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:            "broker_list_createdby",
		Name:          "List CreatedBy Broker",
		Slug:          "list-createdby-broker",
		Status:        store.BrokerStatusOnline,
		CreatedBy:     "user_list_creator",
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersWithCapsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Fatalf("expected 1 broker, got %d", len(resp.Brokers))
	}

	if resp.Brokers[0].CreatedByName != "List Creator" {
		t.Errorf("expected createdByName 'List Creator', got %q", resp.Brokers[0].CreatedByName)
	}
}

func TestRuntimeBrokerListWithProjectLocalPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:         "project_localpath_test",
		Name:       "Local Path Test Project",
		Slug:       "local-path-test",
		Visibility: store.VisibilityPrivate,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:            "host_localpath_test",
		Name:          "Local Path Test Host",
		Slug:          "local-path-test-host",
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: time.Now(),
		Created:       time.Now(),
		Updated:       time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add broker as project provider with a local path
	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/path/to/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, contrib); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// List runtime brokers filtered by project - should include localPath
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?projectId=project_localpath_test", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListRuntimeBrokersWithProviderResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp.Brokers))
	}

	if resp.Brokers[0].ID != "host_localpath_test" {
		t.Errorf("expected broker ID 'host_localpath_test', got %q", resp.Brokers[0].ID)
	}

	if resp.Brokers[0].LocalPath != "/path/to/project/.scion" {
		t.Errorf("expected localPath '/path/to/project/.scion', got %q", resp.Brokers[0].LocalPath)
	}

	// List all runtime brokers (no project filter) - should NOT include localPath field structure
	// (uses ListRuntimeBrokersResponse, not ListRuntimeBrokersWithProviderResponse)
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 ListRuntimeBrokersResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp2.Brokers) != 1 {
		t.Errorf("expected 1 broker, got %d", len(resp2.Brokers))
	}
}

// ============================================================================
// Two-Phase Broker Registration Tests
// ============================================================================

// testServerWithBrokerAuth creates a test server with broker auth enabled.
func testServerWithBrokerAuth(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return srv, s
}

func TestBrokerRegistrationTwoPhaseFlow(t *testing.T) {
	srv, _ := testServerWithBrokerAuth(t)

	// Phase 1: Create broker registration (requires admin auth)
	createBody := map[string]interface{}{
		"name":         "two-phase-host",
		"capabilities": []string{"sync", "attach"},
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/brokers", createBody)
	if rec1.Code != http.StatusCreated {
		t.Errorf("Phase 1: expected status 201, got %d: %s", rec1.Code, rec1.Body.String())
	}

	var createResp CreateBrokerRegistrationResponse
	if err := json.NewDecoder(rec1.Body).Decode(&createResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}

	if createResp.BrokerID == "" {
		t.Error("expected brokerId to be set")
	}
	if createResp.JoinToken == "" {
		t.Error("expected joinToken to be set")
	}
	if createResp.ExpiresAt.IsZero() {
		t.Error("expected expiresAt to be set")
	}

	// Phase 2: Complete broker join (unauthenticated - join token is auth)
	joinBody := map[string]interface{}{
		"brokerId":     createResp.BrokerID,
		"joinToken":    createResp.JoinToken,
		"hostname":     "test-machine",
		"version":      "1.0.0",
		"capabilities": []string{"sync", "attach"},
	}

	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join", joinBody)
	if rec2.Code != http.StatusOK {
		t.Errorf("Phase 2: expected status 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var joinResp BrokerJoinResponse
	if err := json.NewDecoder(rec2.Body).Decode(&joinResp); err != nil {
		t.Fatalf("failed to decode join response: %v", err)
	}

	if joinResp.SecretKey == "" {
		t.Error("expected secretKey to be set")
	}
	if joinResp.BrokerID != createResp.BrokerID {
		t.Errorf("expected brokerId %q, got %q", createResp.BrokerID, joinResp.BrokerID)
	}

	// Phase 3: Register project with brokerId
	projectBody := map[string]interface{}{
		"name":      "Two Phase Project",
		"gitRemote": "https://github.com/test/twophase",
		"brokerId":  joinResp.BrokerID,
	}

	rec3 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", projectBody)
	if rec3.Code != http.StatusOK {
		t.Errorf("Phase 3: expected status 200, got %d: %s", rec3.Code, rec3.Body.String())
	}

	var projectResp RegisterProjectResponse
	if err := json.NewDecoder(rec3.Body).Decode(&projectResp); err != nil {
		t.Fatalf("failed to decode project response: %v", err)
	}

	if !projectResp.Created {
		t.Error("expected project to be created")
	}
	if projectResp.Broker == nil {
		t.Error("expected broker in response")
	} else if projectResp.Broker.ID != joinResp.BrokerID {
		t.Errorf("expected broker ID %q, got %q", joinResp.BrokerID, projectResp.Broker.ID)
	}

	// The new flow should NOT return a secretKey from project registration
	if projectResp.SecretKey != "" {
		t.Error("expected secretKey to be empty in new two-phase flow")
	}
}

func TestBrokerJoinWithInvalidToken(t *testing.T) {
	srv, _ := testServerWithBrokerAuth(t)

	// Phase 1: Create broker
	createBody := map[string]interface{}{
		"name": "invalid-token-host",
	}

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/brokers", createBody)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("failed to create broker: %s", rec1.Body.String())
	}

	var createResp CreateBrokerRegistrationResponse
	if err := json.NewDecoder(rec1.Body).Decode(&createResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}

	// Try to join with invalid token
	joinBody := map[string]interface{}{
		"brokerId":  createResp.BrokerID,
		"joinToken": "invalid_token",
		"hostname":  "test-machine",
	}

	rec2 := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/brokers/join", joinBody)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for invalid token, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// ============================================================================
// Template Endpoint Tests
// ============================================================================

func TestTemplateList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	template := &store.Template{
		ID:         "tmpl_test1",
		Slug:       "test-template",
		Name:       "Test Template",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateTemplate(ctx, template); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/templates", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListTemplatesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Templates) != 1 {
		t.Errorf("expected 1 template, got %d", len(resp.Templates))
	}
}

func TestTemplateListByProjectID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	now := time.Now()

	// Create a global template
	if err := s.CreateTemplate(ctx, &store.Template{
		ID: "tmpl_global1", Slug: "global-tmpl", Name: "Global Template",
		Harness: "claude", Scope: "global",
		Visibility: store.VisibilityPublic, Status: "active",
		Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create global template: %v", err)
	}

	// Create a project-scoped template for project "project_abc"
	if err := s.CreateTemplate(ctx, &store.Template{
		ID: "tmpl_project1", Slug: "project-tmpl", Name: "Project Template",
		Harness: "gemini", Scope: "project", ScopeID: "project_abc",
		Visibility: store.VisibilityPublic, Status: "active",
		Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create project template: %v", err)
	}

	// Create a project-scoped template for a different project
	if err := s.CreateTemplate(ctx, &store.Template{
		ID: "tmpl_project2", Slug: "other-project-tmpl", Name: "Other Project Template",
		Harness: "claude", Scope: "project", ScopeID: "project_xyz",
		Visibility: store.VisibilityPublic, Status: "active",
		Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("failed to create other project template: %v", err)
	}

	// Query with projectId=project_abc should return global + project_abc templates only
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/templates?projectId=project_abc", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListTemplatesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TotalCount != 2 {
		t.Errorf("expected 2 templates (global + project_abc), got %d", resp.TotalCount)
	}

	// Verify we got the right templates
	ids := map[string]bool{}
	for _, tmpl := range resp.Templates {
		ids[tmpl.ID] = true
	}
	if !ids["tmpl_global1"] {
		t.Error("expected global template in results")
	}
	if !ids["tmpl_project1"] {
		t.Error("expected project_abc template in results")
	}
	if ids["tmpl_project2"] {
		t.Error("did not expect project_xyz template in results")
	}
}

func TestTemplateCreate(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"slug":       "new-template",
		"name":       "New Template",
		"harness":    "claude",
		"scope":      "global",
		"visibility": "private",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", body)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateTemplateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Template == nil {
		t.Fatalf("expected template in response, got nil")
	}

	if resp.Template.Slug != "new-template" {
		t.Errorf("expected slug 'new-template', got %q", resp.Template.Slug)
	}

	if resp.Template.Visibility != store.VisibilityPrivate {
		t.Errorf("expected visibility 'private', got %q", resp.Template.Visibility)
	}
}

// ============================================================================
// User Endpoint Tests
// ============================================================================

func TestUserList(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          "user_test1",
		Email:       "test@example.com",
		DisplayName: "Test User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/users", nil)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListUsersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Expect 2 users: the seeded dev user + the test-created user
	if len(resp.Users) != 2 {
		t.Errorf("expected 2 users, got %d", len(resp.Users))
	}
}

func TestUserCreate_Forbidden(t *testing.T) {
	srv, _ := testServer(t)

	body := map[string]interface{}{
		"email":       "newuser@example.com",
		"displayName": "New User",
		"role":        "admin",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/users", body)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Error Handling Tests
// ============================================================================

func TestMethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	// Try PATCH on /healthz which doesn't support it
	rec := doRequest(t, srv, http.MethodPatch, "/healthz", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestInvalidJSON(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project first
	project := &store.Project{
		ID:        "project_invalid",
		Slug:      "invalid-project",
		Name:      "Invalid Project",
		GitRemote: "https://github.com/test/invalid",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader([]byte("{invalid json")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// CORS Tests
// ============================================================================

func TestCORSHeaders(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:3000")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	corsOrigin := rec.Header().Get("Access-Control-Allow-Origin")
	if corsOrigin != "http://localhost:3000" {
		t.Errorf("expected CORS origin 'http://localhost:3000', got %q", corsOrigin)
	}
}

func TestCORSPreflight(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/agents", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected status 204, got %d", rec.Code)
	}

	corsOrigin := rec.Header().Get("Access-Control-Allow-Origin")
	if corsOrigin != "http://localhost:3000" {
		t.Errorf("expected CORS origin 'http://localhost:3000', got %q", corsOrigin)
	}
}

func TestProjectCreateIdempotent(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateProjectRequest{
		ID:        "deterministic-id-1234",
		Name:      "My Project",
		Slug:      "my-project",
		GitRemote: "github.com/acme/widgets",
	}

	// First create — should return 201
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var project1 store.Project
	if err := json.NewDecoder(rec.Body).Decode(&project1); err != nil {
		t.Fatalf("failed to decode first response: %v", err)
	}
	if project1.ID != "deterministic-id-1234" {
		t.Errorf("expected ID %q, got %q", "deterministic-id-1234", project1.ID)
	}

	// Second create with same ID — should return 200 with same project
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second create: expected status 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var project2 store.Project
	if err := json.NewDecoder(rec2.Body).Decode(&project2); err != nil {
		t.Fatalf("failed to decode second response: %v", err)
	}
	if project2.ID != project1.ID {
		t.Errorf("idempotent create returned different ID: %q vs %q", project2.ID, project1.ID)
	}
	if project2.Name != project1.Name {
		t.Errorf("idempotent create returned different name: %q vs %q", project2.Name, project1.Name)
	}
}

func TestProjectCreateWithSlug(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateProjectRequest{
		Name: "My Project",
		Slug: "custom-slug",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var project store.Project
	if err := json.NewDecoder(rec.Body).Decode(&project); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if project.Slug != "custom-slug" {
		t.Errorf("expected slug %q, got %q", "custom-slug", project.Slug)
	}

	// Without slug — should auto-derive from name
	body2 := CreateProjectRequest{
		Name: "Auto Slug Project",
	}

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var project2 store.Project
	if err := json.NewDecoder(rec2.Body).Decode(&project2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if project2.Slug != "auto-slug-project" {
		t.Errorf("expected auto-derived slug %q, got %q", "auto-slug-project", project2.Slug)
	}
}

// ============================================================================
// Template Slug Display Tests
// ============================================================================

// TestAgentCreate_StoresTemplateSlug verifies that when an agent is created with
// a template ID, the agent's Template field is set to the human-friendly slug
// instead of the UUID.
func TestAgentCreate_StoresTemplateSlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "host_tmpl_slug",
		Slug:   "tmpl-host",
		Name:   "Template Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a project
	project := &store.Project{
		ID:                     "project_tmpl_slug",
		Slug:                   "tmpl-project",
		Name:                   "Template Project",
		GitRemote:              "github.com/test/tmpl-repo",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Register broker as provider
	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	// Create a template with a known slug
	tmpl := &store.Template{
		ID:         "tmpl_uuid_123",
		Slug:       "my-claude-template",
		Name:       "My Claude Template",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	// Create agent referencing template by its ID (simulating CLI behavior)
	body := map[string]interface{}{
		"name":     "Slug Test Agent",
		"projectId":  project.ID,
		"template": tmpl.ID,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent in response")
	}

	// The Template field should contain the slug, not the UUID
	if resp.Agent.Template != "my-claude-template" {
		t.Errorf("expected agent.Template to be slug %q, got %q", "my-claude-template", resp.Agent.Template)
	}

	// The TemplateID in AppliedConfig should still have the UUID
	if resp.Agent.AppliedConfig == nil {
		t.Fatal("expected AppliedConfig to be set")
	}
	if resp.Agent.AppliedConfig.TemplateID != tmpl.ID {
		t.Errorf("expected AppliedConfig.TemplateID %q, got %q", tmpl.ID, resp.Agent.AppliedConfig.TemplateID)
	}
}

// TestEnrichAgents_ResolvesTemplateSlug verifies that enrichAgents populates
// the Template field with the slug from TemplateID for agents that were created
// before this fix (with UUIDs stored in Template).
func TestEnrichAgents_ResolvesTemplateSlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a template
	tmpl := &store.Template{
		ID:         "tmpl_enrich_123",
		Slug:       "enriched-template",
		Name:       "Enriched Template",
		Harness:    "gemini",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	// Simulate an agent created before the fix: Template has UUID, TemplateID in AppliedConfig
	agents := []store.Agent{
		{
			ID:       "agent_old_uuid",
			Slug:     "old-agent",
			Name:     "Old Agent",
			Template: tmpl.ID, // UUID stored as template (the old behavior)
			AppliedConfig: &store.AgentAppliedConfig{
				TemplateID: tmpl.ID,
			},
		},
	}

	srv.enrichAgents(ctx, agents)

	// enrichAgents should have replaced the UUID with the slug
	if agents[0].Template != "enriched-template" {
		t.Errorf("expected enriched Template %q, got %q", "enriched-template", agents[0].Template)
	}
}

// TestEnrichAgent_ResolvesTemplateSlug verifies the single-agent enrichment path.
func TestEnrichAgent_ResolvesTemplateSlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a template
	tmpl := &store.Template{
		ID:         "tmpl_enrich_single",
		Slug:       "single-enriched",
		Name:       "Single Enriched",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	if err := s.CreateTemplate(ctx, tmpl); err != nil {
		t.Fatalf("failed to create template: %v", err)
	}

	agent := &store.Agent{
		ID:       "agent_single_enrich",
		Slug:     "single-agent",
		Name:     "Single Agent",
		Template: tmpl.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			TemplateID: tmpl.ID,
		},
	}

	srv.enrichAgent(ctx, agent, nil, nil)

	if agent.Template != "single-enriched" {
		t.Errorf("expected enriched Template %q, got %q", "single-enriched", agent.Template)
	}
}

func TestOutboundMessage_UnknownRecipient(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "msg-project",
		Slug:       "msg-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	rb := &store.RuntimeBroker{
		ID:       "broker-msg",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, rb); err != nil {
		t.Fatal(err)
	}

	agent := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "sender",
		Slug:            "sender",
		ProjectID:         project.ID,
		Phase:           "running",
		RuntimeBrokerID: "broker-msg",
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:nonexistent@example.com",
		Msg:       "hello",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:  jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdent))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown recipient, got %d: %s", rr.Code, rr.Body.String())
	}
}
