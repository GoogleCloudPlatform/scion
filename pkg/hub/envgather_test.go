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
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// envGatherMockBrokerClient extends mockRuntimeBrokerClient with env-gather methods.
type envGatherMockBrokerClient struct {
	mockRuntimeBrokerClient

	// Env-gather fields
	createWithGatherCalled bool
	finalizeCalled         bool
	gatherReturnEnvReqs    *RemoteEnvRequirementsResponse
	lastFinalizeAgentID    string
	lastFinalizeEnv        map[string]string
}

func (m *envGatherMockBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	m.createWithGatherCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastCreateReq = req
	if m.returnErr != nil {
		return nil, nil, m.returnErr
	}
	if m.gatherReturnEnvReqs != nil {
		return nil, m.gatherReturnEnvReqs, nil
	}
	// All env satisfied
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:     req.ID,
			Slug:   req.Slug,
			Name:   req.Name,
			Status: "running",
		},
		Created: true,
	}, nil, nil
}

func (m *envGatherMockBrokerClient) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	m.finalizeCalled = true
	m.lastFinalizeAgentID = agentID
	m.lastFinalizeEnv = env
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:     agentID,
			Name:   agentID,
			Status: "running",
		},
		Created: true,
	}, nil
}

// TestEnvGather_HubDispatch_AllSatisfied tests that when env-gather is enabled
// and all env vars are satisfied, the agent starts normally.
func TestEnvGather_HubDispatch_AllSatisfied(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	project := &store.Project{
		ID:   "project-1",
		Name: "test-project",
		Slug: "test-project",
	}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	// Add provider so broker can serve this project
	if err := memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  "project-1",
		BrokerID: "broker-1",
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:         "project-1",
		RuntimeBrokerID: "broker-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	envReqs, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreateWithGather failed: %v", err)
	}

	if envReqs != nil {
		t.Errorf("expected nil env requirements (all satisfied), got %+v", envReqs)
	}

	if !mockClient.createWithGatherCalled {
		t.Error("expected CreateAgentWithGather to be called")
	}

	// Request should have GatherEnv set
	if mockClient.lastCreateReq != nil && !mockClient.lastCreateReq.GatherEnv {
		t.Error("expected GatherEnv=true in request")
	}
}

// TestEnvGather_HubDispatch_NeedsGather tests that when the broker returns 202
// with env requirements, the dispatcher relays them correctly.
func TestEnvGather_HubDispatch_NeedsGather(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "broker-2",
		Name:     "test-broker-2",
		Slug:     "test-broker-2",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "agent-2",
			Required: []string{"API_KEY", "SECRET"},
			HubHas:   []string{"API_KEY"},
			Needs:    []string{"SECRET"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())

	agent := &store.Agent{
		ID:              "agent-2",
		Name:            "test-agent-2",
		Slug:            "test-agent-2",
		ProjectID:         "project-1",
		RuntimeBrokerID: "broker-2",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	envReqs, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreateWithGather failed: %v", err)
	}

	if envReqs == nil {
		t.Fatal("expected env requirements, got nil")
	}

	if len(envReqs.Needs) != 1 || envReqs.Needs[0] != "SECRET" {
		t.Errorf("expected needs=[SECRET], got %v", envReqs.Needs)
	}
	if len(envReqs.HubHas) != 1 || envReqs.HubHas[0] != "API_KEY" {
		t.Errorf("expected hubHas=[API_KEY], got %v", envReqs.HubHas)
	}
}

// TestEnvGather_HubDispatch_FinalizeEnv tests that DispatchFinalizeEnv properly
// sends gathered env to the broker.
func TestEnvGather_HubDispatch_FinalizeEnv(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "broker-3",
		Name:     "test-broker-3",
		Slug:     "test-broker-3",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())

	agent := &store.Agent{
		ID:              "agent-3",
		Name:            "test-agent-3",
		Slug:            "test-agent-3",
		ProjectID:         "project-1",
		RuntimeBrokerID: "broker-3",
	}

	gatheredEnv := map[string]string{
		"SECRET":  "gathered-secret-value",
		"API_KEY": "gathered-api-key",
	}

	err := dispatcher.DispatchFinalizeEnv(ctx, agent, gatheredEnv)
	if err != nil {
		t.Fatalf("DispatchFinalizeEnv failed: %v", err)
	}

	if !mockClient.finalizeCalled {
		t.Error("expected FinalizeEnv to be called")
	}
	if mockClient.lastFinalizeEnv["SECRET"] != "gathered-secret-value" {
		t.Errorf("expected SECRET=gathered-secret-value, got %q", mockClient.lastFinalizeEnv["SECRET"])
	}
}

// TestEnvGather_HubHandler_202Response tests the full Hub handler flow:
// when GatherEnv is true and the broker returns 202, the Hub returns 202
// to the CLI with EnvGather response.
func TestEnvGather_HubHandler_202Response(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create project
	project := &store.Project{ID: "project-gather", Name: "gather-project", Slug: "gather-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	// Create broker
	broker := &store.RuntimeBroker{
		ID: "broker-gather", Name: "gather-broker", Slug: "gather-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// Add provider with local path so template can be resolved locally
	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-gather", BrokerID: "broker-gather",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Set up dispatcher with mock that returns env requirements
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"GEMINI_API_KEY"},
			Needs:    []string{"GEMINI_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Create agent with GatherEnv=true
	reqBody := map[string]interface{}{
		"name":      "gather-agent",
		"projectId":   "project-gather",
		"template":  "claude",
		"gatherEnv": true,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if resp.EnvGather == nil {
		t.Fatal("expected EnvGather to be set in response")
	}

	if len(resp.EnvGather.Needs) != 1 || resp.EnvGather.Needs[0] != "GEMINI_API_KEY" {
		t.Errorf("expected needs=[GEMINI_API_KEY], got %v", resp.EnvGather.Needs)
	}

	// Agent should be in provisioning status
	if resp.Agent == nil {
		t.Fatal("expected agent in response")
	}
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected agent status=%q, got %q", string(state.PhaseProvisioning), resp.Agent.Phase)
	}
}

// TestEnvGather_HubHandler_ProjectRoute_202Response tests env-gather via the
// project-scoped route /api/v1/projects/{projectId}/agents which is the path the CLI uses.
func TestEnvGather_HubHandler_ProjectRoute_202Response(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create project
	project := &store.Project{ID: "project-gather-route", Name: "gather-route-project", Slug: "gather-route-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	// Create broker
	broker := &store.RuntimeBroker{
		ID: "broker-gather-route", Name: "gather-route-broker", Slug: "gather-route-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// Add provider with local path so template can be resolved locally
	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-gather-route", BrokerID: "broker-gather-route",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Set up dispatcher with mock that returns env requirements
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"GEMINI_API_KEY"},
			Needs:    []string{"GEMINI_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Create agent via project-scoped route with GatherEnv=true
	reqBody := map[string]interface{}{
		"name":      "gather-route-agent",
		"template":  "claude",
		"gatherEnv": true,
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID), reqBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if resp.EnvGather == nil {
		t.Fatal("expected EnvGather to be set in response")
	}

	if len(resp.EnvGather.Needs) != 1 || resp.EnvGather.Needs[0] != "GEMINI_API_KEY" {
		t.Errorf("expected needs=[GEMINI_API_KEY], got %v", resp.EnvGather.Needs)
	}

	// Agent should be in provisioning status
	if resp.Agent == nil {
		t.Fatal("expected agent in response")
	}
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected agent status=%q, got %q", string(state.PhaseProvisioning), resp.Agent.Phase)
	}

	// Verify the dispatcher was called with gather (not regular create)
	if !mockClient.createWithGatherCalled {
		t.Error("expected CreateAgentWithGather to be called, but it wasn't")
	}
}

// TestEnvGather_HubHandler_SubmitEnv tests the env submission endpoint.
func TestEnvGather_HubHandler_SubmitEnv(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create project
	project := &store.Project{ID: "project-submit", Name: "submit-project", Slug: "submit-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	// Create broker
	broker := &store.RuntimeBroker{
		ID: "broker-submit", Name: "submit-broker", Slug: "submit-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// Create agent in provisioning state (as if 202 was already returned)
	agent := &store.Agent{
		ID:              "agent-submit",
		Name:            "submit-agent",
		Slug:            "submit-agent",
		ProjectID:         "project-submit",
		RuntimeBrokerID: "broker-submit",
		Phase:           string(state.PhaseProvisioning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}
	if err := st.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	// Set up dispatcher
	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Submit env
	reqBody := map[string]interface{}{
		"env": map[string]string{
			"GEMINI_API_KEY": "test-api-key-value",
		},
	}

	path := "/api/v1/projects/project-submit/agents/submit-agent/env"
	rec := doRequest(t, srv, http.MethodPost, path, reqBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if !mockClient.finalizeCalled {
		t.Error("expected FinalizeEnv to be called on broker")
	}

	if mockClient.lastFinalizeEnv["GEMINI_API_KEY"] != "test-api-key-value" {
		t.Errorf("expected GEMINI_API_KEY=test-api-key-value, got %q", mockClient.lastFinalizeEnv["GEMINI_API_KEY"])
	}

	// Agent should be updated to running
	updated, err := st.GetAgent(ctx, "agent-submit")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Phase != string(state.PhaseRunning) {
		t.Errorf("expected agent status=running, got %q", updated.Phase)
	}
}

// TestEnvGather_HubHandler_SubmitEnv_InvalidState tests that env submission
// is rejected when the agent is not in a valid state.
func TestEnvGather_HubHandler_SubmitEnv_InvalidState(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create project
	project := &store.Project{ID: "project-invalid", Name: "invalid-project", Slug: "invalid-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	// Create agent in running state (not valid for env submission)
	agent := &store.Agent{
		ID:      "agent-invalid",
		Name:    "invalid-agent",
		Slug:    "invalid-agent",
		ProjectID: "project-invalid",
		Phase:   string(state.PhaseRunning),
	}
	if err := st.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}

	reqBody := map[string]interface{}{
		"env": map[string]string{"KEY": "value"},
	}

	path := "/api/v1/projects/project-invalid/agents/invalid-agent/env"
	rec := doRequest(t, srv, http.MethodPost, path, reqBody)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEnvGather_HubEnvResolution tests that the Hub resolves env vars from
// its storage (user/project scopes) during env-gather dispatch.
func TestEnvGather_HubEnvResolution(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create project
	project := &store.Project{ID: "project-env", Name: "env-project", Slug: "env-project"}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	// Create broker
	broker := &store.RuntimeBroker{
		ID: "broker-env", Name: "env-broker", Slug: "env-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	// Store env vars in project scope
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      "env-1",
		Key:     "GROVE_API_KEY",
		Value:   "project-key-value",
		Scope:   "project",
		ScopeID: "project-env",
	}); err != nil {
		t.Fatal(err)
	}

	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, true, slog.Default())

	agent := &store.Agent{
		ID:              "agent-env",
		Name:            "env-agent",
		Slug:            "env-agent",
		ProjectID:         "project-env",
		RuntimeBrokerID: "broker-env",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	_, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreateWithGather failed: %v", err)
	}

	// The request to the broker should include the project env var
	if mockClient.lastCreateReq == nil {
		t.Fatal("expected CreateReq to be captured")
	}
	if mockClient.lastCreateReq.ResolvedEnv["GROVE_API_KEY"] != "project-key-value" {
		t.Errorf("expected GROVE_API_KEY=project-key-value in resolved env, got %q",
			mockClient.lastCreateReq.ResolvedEnv["GROVE_API_KEY"])
	}
}

// TestEnvGather_HubHandler_RetryAfterCancel_GlobalRoute tests that when an agent
// is stuck in "provisioning" (e.g. env-gather was cancelled) and a new create
// request with GatherEnv=true arrives via the global route, the stale agent is
// deleted and a fresh env-gather flow returns 202.
func TestEnvGather_HubHandler_RetryAfterCancel_GlobalRoute(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-retry-global", Name: "retry-global-project", Slug: "retry-global-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-retry-global", Name: "retry-global-broker", Slug: "retry-global-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-retry-global", BrokerID: "broker-retry-global",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a previous cancelled env-gather: agent exists in "provisioning" status
	staleAgent := &store.Agent{
		ID:              "stale-agent-global",
		Name:            "retry-agent",
		Slug:            "retry-agent",
		ProjectID:         "project-retry-global",
		RuntimeBrokerID: "broker-retry-global",
		Phase:           string(state.PhaseProvisioning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}
	if err := st.CreateAgent(ctx, staleAgent); err != nil {
		t.Fatal(err)
	}

	// Set up dispatcher that returns env requirements (simulating missing env)
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"GEMINI_API_KEY"},
			Needs:    []string{"GEMINI_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Second create request with GatherEnv=true
	reqBody := map[string]interface{}{
		"name":      "retry-agent",
		"projectId":   "project-retry-global",
		"template":  "claude",
		"gatherEnv": true,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)

	// Should get 202 (env-gather needed), NOT 200 (agent started without env)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if resp.EnvGather == nil {
		t.Fatal("expected EnvGather in response (env should be re-gathered)")
	}

	if len(resp.EnvGather.Needs) != 1 || resp.EnvGather.Needs[0] != "GEMINI_API_KEY" {
		t.Errorf("expected needs=[GEMINI_API_KEY], got %v", resp.EnvGather.Needs)
	}

	// The stale agent should have been deleted
	if mockClient.deleteCalled {
		// Dispatcher was used to clean up on broker side - good
	}

	// A new agent should have been created (different ID from stale)
	if resp.Agent == nil {
		t.Fatal("expected agent in response")
	}
	if resp.Agent.ID == "stale-agent-global" {
		t.Error("expected a new agent ID, got the stale agent ID")
	}
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected status=%q, got %q", string(state.PhaseProvisioning), resp.Agent.Phase)
	}

	// The old agent should no longer exist in the store
	_, err := st.GetAgent(ctx, "stale-agent-global")
	if err != store.ErrNotFound {
		t.Errorf("expected stale agent to be deleted, got err=%v", err)
	}
}

// TestEnvGather_BuildResponse_SecretScope tests that buildEnvGatherResponse
// annotates keys with scope "secret" when the Hub's secret backend has a
// matching secret for the agent's owner or project.
func TestEnvGather_BuildResponse_SecretScope(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Create a user secret for API_KEY
	if err := st.CreateSecret(ctx, &store.Secret{
		ID:             "sec-1",
		Key:            "API_KEY",
		EncryptedValue: "encrypted-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "API_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        "owner-1",
	}); err != nil {
		t.Fatal(err)
	}

	// Set up the secret backend on the server
	backend := secret.NewLocalBackend(st, "test-hub-id")
	srv.SetSecretBackend(backend)

	agent := &store.Agent{
		ID:      "agent-scope-test",
		Name:    "scope-test-agent",
		OwnerID: "owner-1",
		ProjectID: "project-1",
	}

	brokerReqs := &RemoteEnvRequirementsResponse{
		AgentID:  "agent-scope-test",
		Required: []string{"API_KEY", "OTHER_KEY"},
		HubHas:   []string{"API_KEY", "OTHER_KEY"},
		Needs:    []string{},
	}

	resp := srv.buildEnvGatherResponse(ctx, agent, brokerReqs)

	// Verify API_KEY is annotated as "secret"
	var apiKeySource, otherKeySource *EnvSource
	for i := range resp.HubHas {
		switch resp.HubHas[i].Key {
		case "API_KEY":
			apiKeySource = &resp.HubHas[i]
		case "OTHER_KEY":
			otherKeySource = &resp.HubHas[i]
		}
	}

	if apiKeySource == nil {
		t.Fatal("expected API_KEY in hubHas")
	}
	if apiKeySource.Scope != "secret" {
		t.Errorf("expected API_KEY scope='secret', got %q", apiKeySource.Scope)
	}

	if otherKeySource == nil {
		t.Fatal("expected OTHER_KEY in hubHas")
	}
	// OTHER_KEY has no matching secret, so it stays as "hub"
	if otherKeySource.Scope != "hub" {
		t.Errorf("expected OTHER_KEY scope='hub', got %q", otherKeySource.Scope)
	}
}

// TestEnvGather_SecretInfoRelay tests that SecretInfo is relayed from the
// broker response through to the CLI-facing EnvGatherResponse.
func TestEnvGather_SecretInfoRelay(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-si-relay", Name: "si-relay-project", Slug: "si-relay-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-si-relay", Name: "si-relay-broker", Slug: "si-relay-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-si-relay", BrokerID: "broker-si-relay",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Mock broker returns SecretInfo in env requirements
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"API_KEY", "CUSTOM_TOKEN"},
			Needs:    []string{"CUSTOM_TOKEN"},
			HubHas:   []string{"API_KEY"},
			SecretInfo: map[string]SecretKeyInfo{
				"CUSTOM_TOKEN": {Description: "Token for custom integration", Source: "settings"},
			},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	reqBody := map[string]interface{}{
		"name":      "si-relay-agent",
		"projectId":   "project-si-relay",
		"template":  "claude",
		"gatherEnv": true,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if resp.EnvGather == nil {
		t.Fatal("expected EnvGather to be set")
	}

	// SecretInfo should be relayed
	if resp.EnvGather.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be relayed")
	}
	info, ok := resp.EnvGather.SecretInfo["CUSTOM_TOKEN"]
	if !ok {
		t.Fatal("expected CUSTOM_TOKEN in SecretInfo")
	}
	if info.Description != "Token for custom integration" {
		t.Errorf("expected description='Token for custom integration', got %q", info.Description)
	}
	if info.Source != "settings" {
		t.Errorf("expected source='settings', got %q", info.Source)
	}
}

// TestEnvGather_SecretInfoRelayType tests that the Type field is relayed from
// the broker response through the Hub to the CLI-facing EnvGatherResponse.
func TestEnvGather_SecretInfoRelayType(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-si-type", Name: "si-type-project", Slug: "si-type-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-si-type", Name: "si-type-broker", Slug: "si-type-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-si-type", BrokerID: "broker-si-type",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Mock broker returns SecretInfo with Type fields
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"ENV_SECRET", "FILE_CERT", "VAR_TOKEN"},
			Needs:    []string{"ENV_SECRET", "FILE_CERT", "VAR_TOKEN"},
			SecretInfo: map[string]SecretKeyInfo{
				"ENV_SECRET": {Description: "Environment secret", Source: "settings", Type: "environment"},
				"FILE_CERT":  {Description: "TLS certificate", Source: "template", Type: "file"},
				"VAR_TOKEN":  {Description: "Variable token", Source: "settings", Type: "variable"},
			},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	reqBody := map[string]interface{}{
		"name":      "si-type-agent",
		"projectId":   "project-si-type",
		"template":  "claude",
		"gatherEnv": true,
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if resp.EnvGather == nil {
		t.Fatal("expected EnvGather to be set")
	}
	if resp.EnvGather.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be relayed")
	}

	// Verify Type is relayed for each key
	tests := []struct {
		key      string
		wantType string
	}{
		{"ENV_SECRET", "environment"},
		{"FILE_CERT", "file"},
		{"VAR_TOKEN", "variable"},
	}
	for _, tc := range tests {
		info, ok := resp.EnvGather.SecretInfo[tc.key]
		if !ok {
			t.Errorf("expected %s in SecretInfo", tc.key)
			continue
		}
		if info.Type != tc.wantType {
			t.Errorf("expected %s type=%q, got %q", tc.key, tc.wantType, info.Type)
		}
	}
}

// TestNonGatherEnv_MissingEnvVars_Returns422 tests that when GatherEnv is NOT
// set and the broker reports missing env vars, the Hub returns 422 and cleans up.
func TestNonGatherEnv_MissingEnvVars_Returns422(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-nogather-missing", Name: "nogather-missing-project", Slug: "nogather-missing-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-nogather-missing", Name: "nogather-missing-broker", Slug: "nogather-missing-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-nogather-missing", BrokerID: "broker-nogather-missing",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Mock broker returns env requirements with missing keys
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"ANTHROPIC_API_KEY", "CUSTOM_SECRET"},
			HubHas:   []string{},
			Needs:    []string{"ANTHROPIC_API_KEY", "CUSTOM_SECRET"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Create agent WITHOUT GatherEnv (simulating web/API caller)
	reqBody := map[string]interface{}{
		"name":     "nogather-missing-agent",
		"projectId":  "project-nogather-missing",
		"template": "claude",
		// gatherEnv is NOT set — this is the non-CLI path
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)

	// Should get 422 — missing env vars
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal("failed to decode error response:", err)
	}

	if errResp.Error.Code != ErrCodeMissingEnvVars {
		t.Errorf("expected error code %q, got %q", ErrCodeMissingEnvVars, errResp.Error.Code)
	}

	// Details should include missingKeys
	missingKeys, ok := errResp.Error.Details["missingKeys"]
	if !ok {
		t.Fatal("expected missingKeys in error details")
	}
	keys, ok := missingKeys.([]interface{})
	if !ok {
		t.Fatalf("expected missingKeys to be a slice, got %T", missingKeys)
	}
	if len(keys) != 2 {
		t.Errorf("expected 2 missing keys, got %d", len(keys))
	}

	// Agent should have been cleaned up from the store
	result, err := st.ListAgents(ctx, store.AgentFilter{ProjectID: "project-nogather-missing"}, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 {
		t.Errorf("expected agent to be cleaned up, but found %d agents", len(result.Items))
	}
}

// TestNonGatherEnv_MissingEnvVars_ProjectRoute_Returns422 tests the same scenario
// via the project-scoped route /api/v1/projects/{projectId}/agents.
func TestNonGatherEnv_MissingEnvVars_ProjectRoute_Returns422(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-nogather-route", Name: "nogather-route-project", Slug: "nogather-route-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-nogather-route", Name: "nogather-route-broker", Slug: "nogather-route-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-nogather-route", BrokerID: "broker-nogather-route",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Mock broker returns env requirements with missing keys
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"ANTHROPIC_API_KEY"},
			HubHas:   []string{},
			Needs:    []string{"ANTHROPIC_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Create agent via project-scoped route WITHOUT GatherEnv
	reqBody := map[string]interface{}{
		"name":     "nogather-route-agent",
		"template": "claude",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID), reqBody)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal("failed to decode error response:", err)
	}

	if errResp.Error.Code != ErrCodeMissingEnvVars {
		t.Errorf("expected error code %q, got %q", ErrCodeMissingEnvVars, errResp.Error.Code)
	}

	// Agent should have been cleaned up
	result, err := st.ListAgents(ctx, store.AgentFilter{ProjectID: "project-nogather-route"}, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 {
		t.Errorf("expected agent to be cleaned up, but found %d agents", len(result.Items))
	}
}

// TestNonGatherEnv_AllSatisfied_Returns201 tests that when GatherEnv is NOT set
// and all env vars are satisfied, the agent is created normally with 201.
func TestNonGatherEnv_AllSatisfied_Returns201(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-nogather-ok", Name: "nogather-ok-project", Slug: "nogather-ok-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-nogather-ok", Name: "nogather-ok-broker", Slug: "nogather-ok-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-nogather-ok", BrokerID: "broker-nogather-ok",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Mock broker returns nil env requirements (all satisfied)
	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Create agent WITHOUT GatherEnv — all env satisfied
	reqBody := map[string]interface{}{
		"name":     "nogather-ok-agent",
		"projectId":  "project-nogather-ok",
		"template": "claude",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", reqBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if resp.Agent == nil {
		t.Fatal("expected agent in response")
	}

	// Agent should exist in the store
	agent, err := st.GetAgent(ctx, resp.Agent.ID)
	if err != nil {
		t.Fatalf("expected agent to exist in store: %v", err)
	}
	if agent.Phase != string(state.PhaseProvisioning) && agent.Phase != string(state.PhaseRunning) {
		t.Errorf("expected agent status provisioning or running, got %q", agent.Phase)
	}

	// The dispatcher should have used CreateAgentWithGather (not regular Create)
	if !mockClient.createWithGatherCalled {
		t.Error("expected CreateAgentWithGather to be called")
	}
}

// TestEnvGather_HubHandler_RetryAfterCancel_ProjectRoute tests the same retry
// scenario via the project-scoped route /api/v1/projects/{projectId}/agents.
func TestEnvGather_HubHandler_RetryAfterCancel_ProjectRoute(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-retry-route", Name: "retry-route-project", Slug: "retry-route-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-retry-route", Name: "retry-route-broker", Slug: "retry-route-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-retry-route", BrokerID: "broker-retry-route",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a previous cancelled env-gather: agent exists in "provisioning" status
	staleAgent := &store.Agent{
		ID:              "stale-agent-route",
		Name:            "retry-route-agent",
		Slug:            "retry-route-agent",
		ProjectID:         "project-retry-route",
		RuntimeBrokerID: "broker-retry-route",
		Phase:           string(state.PhaseProvisioning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}
	if err := st.CreateAgent(ctx, staleAgent); err != nil {
		t.Fatal(err)
	}

	// Set up dispatcher that returns env requirements (simulating missing env)
	mockClient := &envGatherMockBrokerClient{
		gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{
			AgentID:  "will-be-set",
			Required: []string{"GEMINI_API_KEY"},
			Needs:    []string{"GEMINI_API_KEY"},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Second create request via project-scoped route with GatherEnv=true
	reqBody := map[string]interface{}{
		"name":      "retry-route-agent",
		"template":  "claude",
		"gatherEnv": true,
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID), reqBody)

	// Should get 202 (env-gather needed), NOT 200 (agent started without env)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if resp.EnvGather == nil {
		t.Fatal("expected EnvGather in response (env should be re-gathered)")
	}

	if len(resp.EnvGather.Needs) != 1 || resp.EnvGather.Needs[0] != "GEMINI_API_KEY" {
		t.Errorf("expected needs=[GEMINI_API_KEY], got %v", resp.EnvGather.Needs)
	}

	// A new agent should have been created (different ID from stale)
	if resp.Agent == nil {
		t.Fatal("expected agent in response")
	}
	if resp.Agent.ID == "stale-agent-route" {
		t.Error("expected a new agent ID, got the stale agent ID")
	}
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected status=%q, got %q", string(state.PhaseProvisioning), resp.Agent.Phase)
	}

	// The old agent should no longer exist in the store
	_, err := st.GetAgent(ctx, "stale-agent-route")
	if err != store.ErrNotFound {
		t.Errorf("expected stale agent to be deleted, got err=%v", err)
	}
}

// TestProjectRoute_ResolvesUserScopedEnvVars verifies that agents created via
// the project-scoped route (/api/v1/projects/{projectId}/agents) properly resolve
// user-scoped env vars. This is a regression test for a bug where
// createProjectAgent did not set OwnerID on the agent, causing user-scoped
// env vars and secrets to be silently skipped during dispatch.
func TestProjectRoute_ResolvesUserScopedEnvVars(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-owner-env", Name: "owner-env-project", Slug: "owner-env-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-owner-env", Name: "owner-env-broker", Slug: "owner-env-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-owner-env", BrokerID: "broker-owner-env",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Store a user-scoped env var for the dev-user (dev auth identity)
	if err := st.CreateEnvVar(ctx, &store.EnvVar{
		ID:      "env-owner-1",
		Key:     "GEMINI_API_KEY",
		Value:   "user-scoped-gemini-key",
		Scope:   "user",
		ScopeID: DevUserID,
	}); err != nil {
		t.Fatal(err)
	}

	// Mock broker that captures the dispatch request
	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	srv.SetDispatcher(dispatcher)

	// Create agent via project-scoped route (simulates the sync flow)
	reqBody := map[string]interface{}{
		"name":     "owner-env-agent",
		"template": "claude",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID), reqBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the mock broker received the user-scoped env var
	if mockClient.lastCreateReq == nil {
		t.Fatal("expected dispatch request to be captured")
	}
	if val, ok := mockClient.lastCreateReq.ResolvedEnv["GEMINI_API_KEY"]; !ok {
		t.Error("expected GEMINI_API_KEY in ResolvedEnv — user-scoped env var was not resolved (OwnerID not set?)")
	} else if val != "user-scoped-gemini-key" {
		t.Errorf("expected GEMINI_API_KEY=%q, got %q", "user-scoped-gemini-key", val)
	}

	// Also verify the agent record has OwnerID set
	var resp CreateAgentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal("failed to decode response:", err)
	}
	if resp.Agent == nil {
		t.Fatal("expected agent in response")
	}

	agent, err := st.GetAgent(ctx, resp.Agent.ID)
	if err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}
	if agent.OwnerID == "" {
		t.Error("expected OwnerID to be set on agent created via project route")
	}
	if agent.OwnerID != DevUserID {
		t.Errorf("expected OwnerID=%q, got %q", DevUserID, agent.OwnerID)
	}
}

// TestProjectRoute_ResolvesUserScopedSecrets verifies that agents created via
// the project-scoped route properly resolve user-scoped secrets. This is the
// counterpart to TestProjectRoute_ResolvesUserScopedEnvVars for the secret backend.
func TestProjectRoute_ResolvesUserScopedSecrets(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-owner-secret", Name: "owner-secret-project", Slug: "owner-secret-project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	broker := &store.RuntimeBroker{
		ID: "broker-owner-secret", Name: "owner-secret-broker", Slug: "owner-secret-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	if err := st.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}

	if err := st.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: "project-owner-secret", BrokerID: "broker-owner-secret",
		LocalPath: "/tmp/test-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Store a user-scoped secret for the dev-user
	backend := secret.NewLocalBackend(st, "test-hub-id")
	_, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name:       "GEMINI_API_KEY",
		Value:      "secret-gemini-key",
		SecretType: secret.TypeEnvironment,
		Scope:      secret.ScopeUser,
		ScopeID:    DevUserID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mock broker that captures the dispatch request
	mockClient := &envGatherMockBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, mockClient, true, slog.Default())
	dispatcher.SetSecretBackend(backend)
	srv.SetDispatcher(dispatcher)

	// Create agent via project-scoped route
	reqBody := map[string]interface{}{
		"name":     "owner-secret-agent",
		"template": "claude",
	}

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID), reqBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the mock broker received the user-scoped secret
	if mockClient.lastCreateReq == nil {
		t.Fatal("expected dispatch request to be captured")
	}

	found := false
	for _, rs := range mockClient.lastCreateReq.ResolvedSecrets {
		if rs.Name == "GEMINI_API_KEY" {
			found = true
			if rs.Value != "secret-gemini-key" {
				t.Errorf("expected secret value %q, got %q", "secret-gemini-key", rs.Value)
			}
			if rs.Source != "user" {
				t.Errorf("expected source %q, got %q", "user", rs.Source)
			}
		}
	}
	if !found {
		t.Error("expected GEMINI_API_KEY in ResolvedSecrets — user-scoped secret was not resolved (OwnerID not set?)")
	}
}
