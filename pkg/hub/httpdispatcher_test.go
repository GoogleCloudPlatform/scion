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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

// createTestStore creates an in-memory SQLite store for testing.
func createTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	return s
}

// mockRuntimeBrokerClient is a mock implementation of RuntimeBrokerClient for testing.
type mockRuntimeBrokerClient struct {
	createCalled     bool
	startCalled      bool
	stopCalled       bool
	restartCalled    bool
	deleteCalled     bool
	messageCalled    bool
	cleanupCalled    bool
	lastBrokerID     string
	lastEndpoint     string
	lastAgentID      string
	lastTask         string
	lastGrovePath    string
	lastGroveSlug    string
	lastMessage      string
	lastInterrupt    bool
	lastResolvedEnv  map[string]string
	lastInlineConfig *api.ScionConfig
	lastCreateReq    *RemoteCreateAgentRequest
	lastDeleteOpts   struct{ deleteFiles, removeBranch bool }
	returnErr        error
	cleanupErr       error
	startReturnResp  *RemoteAgentResponse // custom start response if set
	cleanupCalls     int
	cleanupSlugs     []string
}

func (m *mockRuntimeBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	m.createCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastCreateReq = req
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:              req.ID,
			ContainerID:     "container-123",
			Slug:            req.Slug,
			Name:            req.Name,
			Phase:           string(state.PhaseRunning),
			ContainerStatus: "Up 5 seconds",
		},
		Created: true,
	}, nil
}

func (m *mockRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, task, grovePath, groveSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir) (*RemoteAgentResponse, error) {
	m.startCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastTask = task
	m.lastGrovePath = grovePath
	m.lastGroveSlug = groveSlug
	m.lastResolvedEnv = resolvedEnv
	m.lastInlineConfig = inlineConfig
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	if m.startReturnResp != nil {
		return m.startReturnResp, nil
	}
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:              agentID,
			Name:            agentID,
			Phase:           string(state.PhaseRunning),
			ContainerStatus: "Up 5 seconds",
		},
	}, nil
}

func (m *mockRuntimeBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) error {
	m.stopCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, resolvedEnv map[string]string) error {
	m.restartCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	m.deleteCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastDeleteOpts.deleteFiles = deleteFiles
	m.lastDeleteOpts.removeBranch = removeBranch
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, command []string, timeout int) (string, error) {
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	if m.returnErr != nil {
		return "", m.returnErr
	}
	return "mock exec output", nil
}

func (m *mockRuntimeBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	m.messageCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastMessage = message
	m.lastInterrupt = interrupt
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) (bool, error) {
	return false, m.returnErr
}

func (m *mockRuntimeBrokerClient) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{ID: agentID, Name: agentID, Phase: string(state.PhaseRunning)},
	}, m.returnErr
}

func (m *mockRuntimeBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, tail int) (string, error) {
	return "", nil
}

func (m *mockRuntimeBrokerClient) CleanupGrove(ctx context.Context, brokerID, brokerEndpoint, groveSlug string) error {
	m.cleanupCalled = true
	m.cleanupCalls++
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.cleanupSlugs = append(m.cleanupSlugs, groveSlug)
	return m.cleanupErr
}

func (m *mockRuntimeBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	m.createCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastCreateReq = req
	if m.returnErr != nil {
		return nil, nil, m.returnErr
	}
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:    req.ID,
			Slug:  req.Slug,
			Name:  req.Name,
			Phase: string(state.PhaseRunning),
		},
		Created: true,
	}, nil, nil
}

func TestHTTPAgentDispatcher_DispatchAgentCreate(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker with an endpoint
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Task:          "Fix a bug",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Error("expected CreateAgent to be called")
	}
	if mockClient.lastEndpoint != "http://localhost:9800" {
		t.Errorf("expected endpoint http://localhost:9800, got %s", mockClient.lastEndpoint)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStop(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentStop(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentStop failed: %v", err)
	}

	if !mockClient.stopCalled {
		t.Error("expected StopAgent to be called")
	}
	if mockClient.lastAgentID != "test-agent" {
		t.Errorf("expected agent ID 'test-agent', got '%s'", mockClient.lastAgentID)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentDelete(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentDelete(ctx, agent, true, false, false, time.Time{})
	if err != nil {
		t.Fatalf("DispatchAgentDelete failed: %v", err)
	}

	if !mockClient.deleteCalled {
		t.Error("expected DeleteAgent to be called")
	}
	if !mockClient.lastDeleteOpts.deleteFiles {
		t.Error("expected deleteFiles to be true")
	}
	if mockClient.lastDeleteOpts.removeBranch {
		t.Error("expected removeBranch to be false")
	}
}

func TestHTTPAgentDispatcher_DispatchAgentMessage(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentMessage(ctx, agent, "Hello, agent!", true, nil)
	if err != nil {
		t.Fatalf("DispatchAgentMessage failed: %v", err)
	}

	if !mockClient.messageCalled {
		t.Error("expected MessageAgent to be called")
	}
	if mockClient.lastMessage != "Hello, agent!" {
		t.Errorf("expected message 'Hello, agent!', got '%s'", mockClient.lastMessage)
	}
	if !mockClient.lastInterrupt {
		t.Error("expected interrupt to be true")
	}
}

func TestHTTPRuntimeBrokerClient_CreateAgent(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents" {
			t.Errorf("expected /api/v1/agents, got %s", r.URL.Path)
		}

		var req RemoteCreateAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}

		resp := RemoteAgentResponse{
			Agent: &RemoteAgentInfo{
				ID:              req.ID,
				ContainerID:     "container-123",
				Slug:            req.Slug,
				Name:            req.Name,
				Phase:           "running",
				ContainerStatus: "Up 5 seconds",
			},
			Created: true,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()

	req := &RemoteCreateAgentRequest{
		ID:      "hub-uuid-1",
		Slug:    "agent-1",
		Name:    "test-agent",
		GroveID: "grove-1",
	}

	resp, err := client.CreateAgent(context.Background(), "host-1", server.URL, req)
	if err != nil {
		t.Fatalf("CreateAgent failed: %v", err)
	}

	if !resp.Created {
		t.Error("expected Created to be true")
	}
	if resp.Agent.ContainerID != "container-123" {
		t.Errorf("expected container ID 'container-123', got '%s'", resp.Agent.ContainerID)
	}
}

func TestHTTPRuntimeBrokerClient_StartAgent_InvalidJSONFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/test-agent/start" {
			t.Errorf("expected /api/v1/agents/test-agent/start, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{not valid json}`))
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()
	_, err := client.StartAgent(context.Background(), "host-1", server.URL, "test-agent", "", "", "", "", "", nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected StartAgent to fail on invalid JSON response")
	}
	if !strings.Contains(err.Error(), "failed to decode response") {
		t.Fatalf("expected decode error, got: %v", err)
	}
}

func TestHTTPRuntimeBrokerClient_StopAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/test-agent/stop" {
			t.Errorf("expected /api/v1/agents/test-agent/stop, got %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()

	err := client.StopAgent(context.Background(), "host-1", server.URL, "test-agent", "")
	if err != nil {
		t.Fatalf("StopAgent failed: %v", err)
	}
}

func TestHTTPRuntimeBrokerClient_DeleteAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/test-agent" {
			t.Errorf("expected /api/v1/agents/test-agent, got %s", r.URL.Path)
		}

		// Check query params
		if r.URL.Query().Get("deleteFiles") != "true" {
			t.Error("expected deleteFiles=true")
		}
		if r.URL.Query().Get("removeBranch") != "false" {
			t.Error("expected removeBranch=false")
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()

	err := client.DeleteAgent(context.Background(), "host-1", server.URL, "test-agent", "", true, false, false, time.Time{})
	if err != nil {
		t.Fatalf("DeleteAgent failed: %v", err)
	}
}

func TestHTTPRuntimeBrokerClient_MessageAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/agents/test-agent/message" {
			t.Errorf("expected /api/v1/agents/test-agent/message, got %s", r.URL.Path)
		}

		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}

		if req["message"] != "Hello!" {
			t.Errorf("expected message 'Hello!', got '%v'", req["message"])
		}
		if req["interrupt"] != true {
			t.Errorf("expected interrupt true, got %v", req["interrupt"])
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()

	err := client.MessageAgent(context.Background(), "host-1", server.URL, "test-agent", "", "Hello!", true, nil)
	if err != nil {
		t.Fatalf("MessageAgent failed: %v", err)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_WithGroveProviderPath(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create the grove with a GitRemote so it is treated as a linked grove
	// (not hub-native). This ensures buildCreateRequest looks up the
	// provider's LocalPath instead of sending a groveSlug.
	grove := &store.Grove{
		ID:        "grove-1",
		Name:      "test-grove",
		Slug:      "test-grove",
		GitRemote: "https://github.com/example/repo.git",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add a grove provider record WITH a local path
	provider := &store.GroveProvider{
		GroveID:    "grove-1",
		BrokerID:   "broker-1",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/projects/myproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "broker-1",
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	if mockClient.lastCreateReq.GrovePath != "/home/user/projects/myproject/.scion" {
		t.Errorf("expected GrovePath '/home/user/projects/myproject/.scion', got '%s'", mockClient.lastCreateReq.GrovePath)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_MissingBrokerEndpoint(t *testing.T) {
	// When a broker has no HTTP endpoint configured (e.g. control-channel-only
	// brokers behind NAT), the dispatcher should still pass the call through
	// to the client. The HybridBrokerClient will route via the control channel
	// when connected; the HTTP transport will fail with a clear error if not.
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:     "host-1",
		Name:   "test-host",
		Slug:   "test-host",
		Status: store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("expected DispatchAgentCreate to succeed (client handles empty endpoint), got: %v", err)
	}
	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called even with empty endpoint")
	}
	if mockClient.lastEndpoint != "" {
		t.Errorf("expected empty endpoint, got %q", mockClient.lastEndpoint)
	}
}

func TestBrokerHTTPTransport_RejectsEmptyEndpoint(t *testing.T) {
	transport := newBrokerHTTPTransport(false, nil)
	_, err := transport.CreateAgent(context.Background(), "broker-1", "", &RemoteCreateAgentRequest{})
	if err == nil {
		t.Fatal("expected error when endpoint is empty")
	}
	if !strings.Contains(err.Error(), "no HTTP endpoint configured") {
		t.Fatalf("expected clear endpoint error, got: %v", err)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_WithoutGroveProviderPath(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create the grove (required by FK constraint)
	grove := &store.Grove{
		ID:   "grove-1",
		Name: "test-grove",
		Slug: "test-grove",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add a grove provider record WITHOUT a local path (simulating auto-provide)
	provider := &store.GroveProvider{
		GroveID:    "grove-1",
		BrokerID:   "broker-1",
		BrokerName: "test-broker",
		LocalPath:  "",
		Status:     store.BrokerStatusOnline,
		LinkedBy:   "auto-provide",
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "broker-1",
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	// When auto-provide didn't set a path, GrovePath should be empty
	if mockClient.lastCreateReq.GrovePath != "" {
		t.Errorf("expected empty GrovePath for auto-provided broker, got '%s'", mockClient.lastCreateReq.GrovePath)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentProvision(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker with an endpoint
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentProvision(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentProvision failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called for provision")
	}

	// Verify ProvisionOnly flag is set in the request
	if !mockClient.lastCreateReq.ProvisionOnly {
		t.Error("expected ProvisionOnly to be true in the request")
	}

	// Verify it sent to the correct endpoint
	if mockClient.lastEndpoint != "http://localhost:9800" {
		t.Errorf("expected endpoint 'http://localhost:9800', got '%s'", mockClient.lastEndpoint)
	}

	// Verify broker ID was passed
	if mockClient.lastBrokerID != "host-1" {
		t.Errorf("expected brokerID 'host-1', got '%s'", mockClient.lastBrokerID)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentProvision_NoBroker(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		RuntimeBrokerID: "", // No broker assigned
	}

	err := dispatcher.DispatchAgentProvision(ctx, agent)
	if err == nil {
		t.Fatal("expected error when no runtime broker is assigned")
	}

	if mockClient.createCalled {
		t.Fatal("CreateAgent should not be called when no broker is assigned")
	}
}

func TestHTTPAgentDispatcher_DispatchAgentProvision_PassesTaskThrough(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			Task: "implement feature X",
		},
	}

	err := dispatcher.DispatchAgentProvision(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentProvision failed: %v", err)
	}

	// Verify ProvisionOnly is set
	if !mockClient.lastCreateReq.ProvisionOnly {
		t.Error("expected ProvisionOnly to be true for DispatchAgentProvision")
	}

	// Verify the task was passed through in the config
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if mockClient.lastCreateReq.Config.Task != "implement feature X" {
		t.Errorf("expected task 'implement feature X', got '%s'", mockClient.lastCreateReq.Config.Task)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_WithWorkspace(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Task:          "do something",
			Workspace:     "./subfolder",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if mockClient.lastCreateReq.Config.Workspace != "./subfolder" {
		t.Errorf("expected Workspace './subfolder', got '%s'", mockClient.lastCreateReq.Config.Workspace)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_WithCreatorName(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Task:          "do something",
			CreatorName:   "alice@example.com",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}

	// Verify CreatorName is propagated to the remote request
	if mockClient.lastCreateReq.CreatorName != "alice@example.com" {
		t.Errorf("expected CreatorName 'alice@example.com', got '%s'", mockClient.lastCreateReq.CreatorName)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_WithoutCreatorName(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	// Verify CreatorName is empty when not set in AppliedConfig
	if mockClient.lastCreateReq.CreatorName != "" {
		t.Errorf("expected empty CreatorName, got '%s'", mockClient.lastCreateReq.CreatorName)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_DoesNotSetProvisionOnly(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			Task: "do something",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	// Verify ProvisionOnly is NOT set for regular create
	if mockClient.lastCreateReq.ProvisionOnly {
		t.Error("expected ProvisionOnly to be false for regular DispatchAgentCreate")
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_WithGroveProviderPath(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create the grove with a GitRemote so it is treated as a linked grove
	grove := &store.Grove{
		ID:        "grove-1",
		Name:      "test-grove",
		Slug:      "test-grove",
		GitRemote: "https://github.com/example/repo.git",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add a grove provider record with a local path
	provider := &store.GroveProvider{
		GroveID:    "grove-1",
		BrokerID:   "broker-1",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/projects/myproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "broker-1",
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "do task")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}
	if mockClient.lastGrovePath != "/home/user/projects/myproject/.scion" {
		t.Errorf("expected grovePath '/home/user/projects/myproject/.scion', got '%s'", mockClient.lastGrovePath)
	}
	if mockClient.lastTask != "do task" {
		t.Errorf("expected task 'do task', got '%s'", mockClient.lastTask)
	}

	// Verify broker response was applied to the agent
	if agent.Phase != "running" {
		t.Errorf("expected agent status 'running', got '%s'", agent.Phase)
	}
	// With a local provider path, groveSlug should not be set
	if mockClient.lastGroveSlug != "" {
		t.Errorf("expected empty groveSlug when provider has local path, got %q", mockClient.lastGroveSlug)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentStart_IncludesAgentIdentity verifies that
// DispatchAgentStart injects SCION_AGENT_ID and SCION_AGENT_SLUG into resolvedEnv
// so the container can report status back to the Hub.
func TestHTTPAgentDispatcher_DispatchAgentStart_IncludesAgentIdentity(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	grove := &store.Grove{
		ID:        "grove-1",
		Name:      "test-grove",
		Slug:      "test-grove",
		GitRemote: "https://github.com/example/repo.git",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	provider := &store.GroveProvider{
		GroveID:    "grove-1",
		BrokerID:   "broker-1",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/projects/myproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-uuid-123",
		Name:            "test-agent",
		Slug:            "test-agent-slug",
		GroveID:         "grove-1",
		RuntimeBrokerID: "broker-1",
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}

	// Verify SCION_AGENT_ID is included in resolvedEnv
	if v, ok := mockClient.lastResolvedEnv["SCION_AGENT_ID"]; !ok {
		t.Error("expected SCION_AGENT_ID in resolvedEnv, but not found")
	} else if v != "agent-uuid-123" {
		t.Errorf("expected SCION_AGENT_ID='agent-uuid-123', got %q", v)
	}

	// Verify SCION_AGENT_SLUG is included in resolvedEnv
	if v, ok := mockClient.lastResolvedEnv["SCION_AGENT_SLUG"]; !ok {
		t.Error("expected SCION_AGENT_SLUG in resolvedEnv, but not found")
	} else if v != "test-agent-slug" {
		t.Errorf("expected SCION_AGENT_SLUG='test-agent-slug', got %q", v)
	}

	// Verify SCION_GROVE_ID is included in resolvedEnv
	if v, ok := mockClient.lastResolvedEnv["SCION_GROVE_ID"]; !ok {
		t.Error("expected SCION_GROVE_ID in resolvedEnv, but not found")
	} else if v != "grove-1" {
		t.Errorf("expected SCION_GROVE_ID='grove-1', got %q", v)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_HubNativeGrove(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a hub-native grove (no git remote)
	grove := &store.Grove{
		ID:   "grove-hub",
		Name: "My Hub Grove",
		Slug: "my-hub-grove",
		// No GitRemote — this is a hub-native grove
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a runtime broker with no local provider path for this grove
	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-hub-1",
		Name:            "hub-agent",
		Slug:            "hub-agent",
		GroveID:         "grove-hub",
		RuntimeBrokerID: "broker-1",
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}
	// No local provider path — grovePath should be empty
	if mockClient.lastGrovePath != "" {
		t.Errorf("expected empty grovePath for hub-native grove, got %q", mockClient.lastGrovePath)
	}
	// GroveSlug should be set so the broker can resolve the path
	if mockClient.lastGroveSlug != "my-hub-grove" {
		t.Errorf("expected groveSlug 'my-hub-grove', got %q", mockClient.lastGroveSlug)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_GroveSlugSetForGitRemoteWithoutLocalPath(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a grove with a git remote but no local provider path.
	// The broker needs the groveSlug to resolve agent directories under
	// ~/.scion/groves/<slug>/ instead of falling back to the global grove.
	grove := &store.Grove{
		ID:        "grove-git",
		Name:      "Git Grove",
		Slug:      "git-grove",
		GitRemote: "https://github.com/user/repo.git",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-git-1",
		Name:            "git-agent",
		Slug:            "git-agent",
		GroveID:         "grove-git",
		RuntimeBrokerID: "broker-1",
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}
	// Git-remote grove without a local provider path should have groveSlug set
	// so the broker resolves agent dirs under ~/.scion/groves/<slug>/
	if mockClient.lastGroveSlug != "git-grove" {
		t.Errorf("expected groveSlug='git-grove' for git grove without local path, got %q", mockClient.lastGroveSlug)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_ResolvesEnvFromStorage(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a grove
	grove := &store.Grove{
		ID:   "grove-env",
		Name: "env-test-grove",
		Slug: "env-test-grove",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "broker-env",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add a grove provider with a local path
	provider := &store.GroveProvider{
		GroveID:    "grove-env",
		BrokerID:   "broker-env",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	// Store an env var in grove scope (simulating API key stored in hub)
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      "ev-grove-1",
		Key:     "GEMINI_API_KEY",
		Value:   "test-api-key-123",
		Scope:   "grove",
		ScopeID: "grove-env",
	}); err != nil {
		t.Fatalf("failed to set env var: %v", err)
	}

	// Store a user-scoped env var
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      "ev-user-1",
		Key:     "CUSTOM_VAR",
		Value:   "user-value",
		Scope:   "user",
		ScopeID: "owner-1",
	}); err != nil {
		t.Fatalf("failed to set env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-env",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-env",
		OwnerID:         "owner-1",
		RuntimeBrokerID: "broker-env",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "gemini",
			Env:           map[string]string{"EXISTING_VAR": "from-config"},
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}

	// Verify resolved env contains the stored env vars
	if mockClient.lastResolvedEnv == nil {
		t.Fatal("expected resolvedEnv to be non-nil")
	}

	// Config env should be present
	if v, ok := mockClient.lastResolvedEnv["EXISTING_VAR"]; !ok || v != "from-config" {
		t.Errorf("expected EXISTING_VAR='from-config', got '%s' (ok=%v)", v, ok)
	}

	// Grove-scoped env should be present
	if v, ok := mockClient.lastResolvedEnv["GEMINI_API_KEY"]; !ok || v != "test-api-key-123" {
		t.Errorf("expected GEMINI_API_KEY='test-api-key-123', got '%s' (ok=%v)", v, ok)
	}

	// User-scoped env should be present
	if v, ok := mockClient.lastResolvedEnv["CUSTOM_VAR"]; !ok || v != "user-value" {
		t.Errorf("expected CUSTOM_VAR='user-value', got '%s' (ok=%v)", v, ok)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_ConfigEnvTakesPrecedence(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create grove and broker
	grove := &store.Grove{
		ID:   "grove-prec",
		Name: "precedence-test",
		Slug: "precedence-test",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-prec",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Store an env var that conflicts with config env
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      "ev-prec-1",
		Key:     "API_KEY",
		Value:   "storage-value",
		Scope:   "grove",
		ScopeID: "grove-prec",
	}); err != nil {
		t.Fatalf("failed to set env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-prec",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-prec",
		RuntimeBrokerID: "broker-prec",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "gemini",
			Env:           map[string]string{"API_KEY": "config-value"},
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	// Config env should take precedence over storage env
	if v := mockClient.lastResolvedEnv["API_KEY"]; v != "config-value" {
		t.Errorf("expected config env to take precedence, got API_KEY='%s' (wanted 'config-value')", v)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_StorageOverridesEmptyConfigEnv(t *testing.T) {
	// When AppliedConfig.Env has a key with an empty value (passthrough marker),
	// storage env should override it so that hub-stored secrets are available.
	ctx := context.Background()
	memStore := createTestStore(t)

	grove := &store.Grove{
		ID:   "grove-empty-env",
		Name: "empty-env-test",
		Slug: "empty-env-test",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-empty-env",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Store an env var that should override the empty config value
	if err := memStore.CreateEnvVar(ctx, &store.EnvVar{
		ID:      "ev-empty-1",
		Key:     "GEMINI_API_KEY",
		Value:   "stored-api-key",
		Scope:   "grove",
		ScopeID: "grove-empty-env",
	}); err != nil {
		t.Fatalf("failed to set env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-empty-env",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-empty-env",
		RuntimeBrokerID: "broker-empty-env",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "gemini",
			// Empty value = passthrough marker; storage should fill it in
			Env: map[string]string{
				"GEMINI_API_KEY": "",
				"EXPLICIT_VAR":   "explicit-value",
			},
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	// Storage env should override the empty config value
	if v := mockClient.lastResolvedEnv["GEMINI_API_KEY"]; v != "stored-api-key" {
		t.Errorf("expected storage to override empty config env, got GEMINI_API_KEY='%s' (wanted 'stored-api-key')", v)
	}

	// Non-empty config env should still take precedence
	if v := mockClient.lastResolvedEnv["EXPLICIT_VAR"]; v != "explicit-value" {
		t.Errorf("expected EXPLICIT_VAR='explicit-value', got '%s'", v)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_InjectsDevToken(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetDevAuthToken("my-dev-token")

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}

	// Verify SCION_DEV_TOKEN was injected into ResolvedEnv
	if mockClient.lastCreateReq.ResolvedEnv == nil {
		t.Fatal("expected ResolvedEnv to be non-nil")
	}
	if mockClient.lastCreateReq.ResolvedEnv["SCION_DEV_TOKEN"] != "my-dev-token" {
		t.Errorf("expected SCION_DEV_TOKEN='my-dev-token', got %q",
			mockClient.lastCreateReq.ResolvedEnv["SCION_DEV_TOKEN"])
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_NoDevToken(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	// Do NOT set dev auth token

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	// Verify SCION_DEV_TOKEN is NOT in ResolvedEnv when devAuthToken is empty
	if mockClient.lastCreateReq.ResolvedEnv != nil {
		if _, exists := mockClient.lastCreateReq.ResolvedEnv["SCION_DEV_TOKEN"]; exists {
			t.Error("expected SCION_DEV_TOKEN NOT to be present when devAuthToken is empty")
		}
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_DevTokenMergesWithExistingEnv(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetDevAuthToken("my-dev-token")

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Env: map[string]string{
				"EXISTING_VAR": "existing-value",
			},
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	// Verify both existing env and SCION_DEV_TOKEN are present
	if mockClient.lastCreateReq.ResolvedEnv["EXISTING_VAR"] != "existing-value" {
		t.Errorf("expected EXISTING_VAR='existing-value', got %q",
			mockClient.lastCreateReq.ResolvedEnv["EXISTING_VAR"])
	}
	if mockClient.lastCreateReq.ResolvedEnv["SCION_DEV_TOKEN"] != "my-dev-token" {
		t.Errorf("expected SCION_DEV_TOKEN='my-dev-token', got %q",
			mockClient.lastCreateReq.ResolvedEnv["SCION_DEV_TOKEN"])
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_AppliesBrokerResponse(t *testing.T) {
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
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{
		startReturnResp: &RemoteAgentResponse{
			Agent: &RemoteAgentInfo{
				ID:              "container-abc",
				Name:            "test-agent",
				Phase:           string(state.PhaseRunning),
				ContainerStatus: "Up 10 seconds",
				Template:        "claude",
				Runtime:         "docker",
			},
		},
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "broker-1",
		Phase:           string(state.PhaseCreated),
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	// Verify broker response fields were applied
	if agent.Phase != "running" {
		t.Errorf("expected status 'running', got '%s'", agent.Phase)
	}
	if agent.ContainerStatus != "Up 10 seconds" {
		t.Errorf("expected containerStatus 'Up 10 seconds', got '%s'", agent.ContainerStatus)
	}
	if agent.Template != "claude" {
		t.Errorf("expected template 'claude', got '%s'", agent.Template)
	}
	if agent.Runtime != "docker" {
		t.Errorf("expected runtime 'docker', got '%s'", agent.Runtime)
	}
	if agent.RuntimeState != "container:container-abc" {
		t.Errorf("expected runtimeState 'container:container-abc', got '%s'", agent.RuntimeState)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_PropagatesGitClone(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-gc-1",
		Name:            "git-clone-agent",
		Slug:            "git-clone-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Task:          "implement feature",
			GitClone: &api.GitCloneConfig{
				URL:    "https://github.com/example/repo.git",
				Branch: "develop",
				Depth:  1,
			},
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if mockClient.lastCreateReq.Config.GitClone == nil {
		t.Fatal("expected GitClone to be propagated in config")
	}
	if mockClient.lastCreateReq.Config.GitClone.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected GitClone URL 'https://github.com/example/repo.git', got '%s'",
			mockClient.lastCreateReq.Config.GitClone.URL)
	}
	if mockClient.lastCreateReq.Config.GitClone.Branch != "develop" {
		t.Errorf("expected GitClone Branch 'develop', got '%s'",
			mockClient.lastCreateReq.Config.GitClone.Branch)
	}
	if mockClient.lastCreateReq.Config.GitClone.Depth != 1 {
		t.Errorf("expected GitClone Depth 1, got %d",
			mockClient.lastCreateReq.Config.GitClone.Depth)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_PropagatesProfile(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-profile-1",
		Name:            "profile-agent",
		Slug:            "profile-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Task:          "do something",
			Profile:       "custom-profile",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if mockClient.lastCreateReq.Config.Profile != "custom-profile" {
		t.Errorf("expected Profile 'custom-profile', got '%s'", mockClient.lastCreateReq.Config.Profile)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_PropagatesGroveSlug_HubNative(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a hub-native grove (no GitRemote)
	grove := &store.Grove{
		ID:   "grove-hub-native",
		Name: "Hub Native Grove",
		Slug: "hub-native-grove",
		// No GitRemote = hub-native
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-hub-native",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	if mockClient.lastCreateReq.GroveSlug != "hub-native-grove" {
		t.Errorf("expected GroveSlug 'hub-native-grove', got '%s'", mockClient.lastCreateReq.GroveSlug)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_GroveSlugSet_GitGrove(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a git-backed grove (has GitRemote) without a local provider path.
	grove := &store.Grove{
		ID:        "grove-git",
		Name:      "Git Grove",
		Slug:      "git-grove",
		GitRemote: "github.com/test/repo",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-git",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	// Git-backed groves without a local provider path should have GroveSlug set
	// so the broker resolves agent dirs under ~/.scion/groves/<slug>/
	if mockClient.lastCreateReq.GroveSlug != "git-grove" {
		t.Errorf("expected GroveSlug='git-grove' for git-backed grove without local path, got '%s'", mockClient.lastCreateReq.GroveSlug)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_EmptyProfile(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-no-profile-1",
		Name:            "no-profile-agent",
		Slug:            "no-profile-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Task:          "do something",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if mockClient.lastCreateReq.Config.Profile != "" {
		t.Errorf("expected empty Profile, got '%s'", mockClient.lastCreateReq.Config.Profile)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_NoGroveSlug_LocalPathGrove(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a linked grove with a local provider path.
	// This grove has a GitRemote so it is treated as a linked grove (not hub-native).
	// Even though the broker has the repo locally, all hub-linked groves with a
	// git remote use clone-based provisioning (HTTPS + GitHub token).
	grove := &store.Grove{
		ID:        "grove-local",
		Name:      "Local Grove",
		Slug:      "local-grove",
		GitRemote: "https://github.com/example/local-project.git",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add a grove provider record WITH a local path
	provider := &store.GroveProvider{
		GroveID:    "grove-local",
		BrokerID:   "broker-1",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/projects/myproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-local",
		RuntimeBrokerID: "broker-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Workspace:     "/should/be/cleared",
			// GitClone is set by populateAgentConfig for any grove with a
			// GitRemote. For linked groves (broker has local path), GitClone
			// is preserved so the broker uses clone-based provisioning
			// (HTTPS + GitHub token) rather than local worktrees.
			GitClone: &api.GitCloneConfig{
				URL:    "https://github.com/example/local-project.git",
				Branch: "main",
				Depth:  1,
			},
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}

	// A non-git grove with a local provider path should NOT have GroveSlug set.
	// GroveSlug is only for hub-native groves (no local path on the broker).
	if mockClient.lastCreateReq.GroveSlug != "" {
		t.Errorf("expected empty GroveSlug for local-path grove, got '%s'", mockClient.lastCreateReq.GroveSlug)
	}

	// The GrovePath should be set from the provider
	if mockClient.lastCreateReq.GrovePath != "/home/user/projects/myproject/.scion" {
		t.Errorf("expected GrovePath '/home/user/projects/myproject/.scion', got '%s'", mockClient.lastCreateReq.GrovePath)
	}

	// Config.Workspace should be cleared when a local provider path exists,
	// because the workspace is derived from the grove path, not the hub-native convention.
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if mockClient.lastCreateReq.Config.Workspace != "" {
		t.Errorf("expected empty Workspace for local-path grove, got '%s'", mockClient.lastCreateReq.Config.Workspace)
	}

	// GitClone should be preserved for linked groves with a git remote — all
	// hub-linked groves use clone-based provisioning (HTTPS + GitHub token).
	if mockClient.lastCreateReq.Config.GitClone == nil {
		t.Fatal("expected GitClone to be preserved for linked grove with git remote")
	}
	if mockClient.lastCreateReq.Config.GitClone.URL != "https://github.com/example/local-project.git" {
		t.Errorf("expected GitClone URL 'https://github.com/example/local-project.git', got '%s'",
			mockClient.lastCreateReq.Config.GitClone.URL)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentCreate_LinkedGroveNoGitRemote verifies
// that a linked grove without a git remote (registered via CLI link, not via
// git URL) uses the provider's LocalPath rather than being treated as hub-native.
func TestHTTPAgentDispatcher_DispatchAgentCreate_LinkedGroveNoGitRemote(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a linked grove WITHOUT a GitRemote — this is what happens when
	// a user links a local project via `scion hub groves link`.
	grove := &store.Grove{
		ID:   "grove-linked-no-git",
		Name: "Linked No Git Grove",
		Slug: "linked-no-git",
		// No GitRemote — looks like hub-native, but has a provider path
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-1",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Add a grove provider record WITH a local path
	provider := &store.GroveProvider{
		GroveID:    "grove-linked-no-git",
		BrokerID:   "broker-1",
		BrokerName: "test-broker",
		LocalPath:  "/Users/user/dev/projects/my-project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-linked-no-git",
		RuntimeBrokerID: "broker-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Workspace:     "/should/be/cleared",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}

	// Provider path must take precedence — should NOT be treated as hub-native
	if mockClient.lastCreateReq.GroveSlug != "" {
		t.Errorf("expected empty GroveSlug for linked grove with provider path, got '%s'", mockClient.lastCreateReq.GroveSlug)
	}

	// The GrovePath should be set from the provider
	if mockClient.lastCreateReq.GrovePath != "/Users/user/dev/projects/my-project/.scion" {
		t.Errorf("expected GrovePath '/Users/user/dev/projects/my-project/.scion', got '%s'", mockClient.lastCreateReq.GrovePath)
	}

	// Config.Workspace should be cleared when a local provider path exists
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if mockClient.lastCreateReq.Config.Workspace != "" {
		t.Errorf("expected empty Workspace for linked grove with provider path, got '%s'", mockClient.lastCreateReq.Config.Workspace)
	}
}

func TestBuildCreateRequest_ResolvesStorageEnvVars(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Store a user-scoped env var
	envVar := &store.EnvVar{
		ID:      "ev-1",
		Key:     "GEMINI_API_KEY",
		Value:   "stored-key-value",
		Scope:   "user",
		ScopeID: "user-1",
	}
	if err := memStore.CreateEnvVar(ctx, envVar); err != nil {
		t.Fatalf("failed to create env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		OwnerID:         "user-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "TestBuildCreateRequest")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	if req.ResolvedEnv == nil {
		t.Fatal("expected ResolvedEnv to be non-nil")
	}
	if req.ResolvedEnv["GEMINI_API_KEY"] != "stored-key-value" {
		t.Errorf("expected GEMINI_API_KEY='stored-key-value', got %q", req.ResolvedEnv["GEMINI_API_KEY"])
	}
}

func TestBuildCreateRequest_ConfigEnvOverridesStorage(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Store a user-scoped env var with the same key as config env
	envVar := &store.EnvVar{
		ID:      "ev-1",
		Key:     "MY_KEY",
		Value:   "storage-value",
		Scope:   "user",
		ScopeID: "user-1",
	}
	if err := memStore.CreateEnvVar(ctx, envVar); err != nil {
		t.Fatalf("failed to create env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		OwnerID:         "user-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{
				"MY_KEY": "config-value",
			},
		},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "TestBuildCreateRequest")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	// Config value should win over storage value
	if req.ResolvedEnv["MY_KEY"] != "config-value" {
		t.Errorf("expected config value to override storage: got %q", req.ResolvedEnv["MY_KEY"])
	}
}

func TestBuildCreateRequest_ResolvesGroveAndUserScopes(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create grove and broker
	grove := &store.Grove{
		ID:   "grove-1",
		Name: "test-grove",
		Slug: "test-grove",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Store a grove-scoped env var
	groveEnv := &store.EnvVar{
		ID:      "ev-grove",
		Key:     "SHARED_KEY",
		Value:   "grove-value",
		Scope:   "grove",
		ScopeID: "grove-1",
	}
	if err := memStore.CreateEnvVar(ctx, groveEnv); err != nil {
		t.Fatalf("failed to create grove env var: %v", err)
	}

	// Store a user-scoped env var with the same key (higher precedence)
	userEnv := &store.EnvVar{
		ID:      "ev-user",
		Key:     "SHARED_KEY",
		Value:   "user-value",
		Scope:   "user",
		ScopeID: "user-1",
	}
	if err := memStore.CreateEnvVar(ctx, userEnv); err != nil {
		t.Fatalf("failed to create user env var: %v", err)
	}

	// Store a grove-only env var
	groveOnly := &store.EnvVar{
		ID:      "ev-grove-only",
		Key:     "GROVE_ONLY_KEY",
		Value:   "grove-only-value",
		Scope:   "grove",
		ScopeID: "grove-1",
	}
	if err := memStore.CreateEnvVar(ctx, groveOnly); err != nil {
		t.Fatalf("failed to create grove-only env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		OwnerID:         "user-1",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "TestBuildCreateRequest")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	// User scope should take precedence over grove scope
	if req.ResolvedEnv["SHARED_KEY"] != "user-value" {
		t.Errorf("expected user-scoped value to win: got %q", req.ResolvedEnv["SHARED_KEY"])
	}

	// Grove-only key should also be present
	if req.ResolvedEnv["GROVE_ONLY_KEY"] != "grove-only-value" {
		t.Errorf("expected GROVE_ONLY_KEY='grove-only-value', got %q", req.ResolvedEnv["GROVE_ONLY_KEY"])
	}
}

func TestDispatchAgentCreate_IncludesStorageEnvVars(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Store user-scoped env vars
	envVar := &store.EnvVar{
		ID:      "ev-1",
		Key:     "API_TOKEN",
		Value:   "secret-token-123",
		Scope:   "user",
		ScopeID: "user-1",
	}
	if err := memStore.CreateEnvVar(ctx, envVar); err != nil {
		t.Fatalf("failed to create env var: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		OwnerID:         "user-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}

	// Verify that storage env vars are included in the request sent to the broker
	if mockClient.lastCreateReq.ResolvedEnv == nil {
		t.Fatal("expected ResolvedEnv to be non-nil")
	}
	if mockClient.lastCreateReq.ResolvedEnv["API_TOKEN"] != "secret-token-123" {
		t.Errorf("expected API_TOKEN='secret-token-123', got %q",
			mockClient.lastCreateReq.ResolvedEnv["API_TOKEN"])
	}
}

func TestBuildCreateRequest_PropagatesHarnessName(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-harness-1",
		Name:            "harness-agent",
		Slug:            "harness-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "gemini",
			Task:          "do something",
		},
	}

	req, err := dispatcher.buildCreateRequest(ctx, agent, "TestPropagatesHarness")
	if err != nil {
		t.Fatalf("buildCreateRequest failed: %v", err)
	}

	if req.Config == nil {
		t.Fatal("expected config to be present")
	}
	if req.Config.HarnessConfig != "gemini" {
		t.Errorf("expected HarnessConfig 'gemini', got '%s'", req.Config.HarnessConfig)
	}
}

// Tests verifying that the dispatcher sends agent.Slug (not agent.Name) to the broker.

func TestHTTPAgentDispatcher_DispatchAgentStop_UsesSlugNotName(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "My Special Agent!",
		Slug:            "my-special-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentStop(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentStop failed: %v", err)
	}

	if mockClient.lastAgentID != "my-special-agent" {
		t.Errorf("expected slug 'my-special-agent' to be dispatched, got '%s'", mockClient.lastAgentID)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentDelete_UsesSlugNotName(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "slug Stres$@ . / test",
		Slug:            "slug-stres-test",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentDelete(ctx, agent, true, true, false, time.Time{})
	if err != nil {
		t.Fatalf("DispatchAgentDelete failed: %v", err)
	}

	if mockClient.lastAgentID != "slug-stres-test" {
		t.Errorf("expected slug 'slug-stres-test' to be dispatched, got '%s'", mockClient.lastAgentID)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentRestart_UsesSlugNotName(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "My Special Agent!",
		Slug:            "my-special-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentRestart(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentRestart failed: %v", err)
	}

	if mockClient.lastAgentID != "my-special-agent" {
		t.Errorf("expected slug 'my-special-agent' to be dispatched, got '%s'", mockClient.lastAgentID)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentMessage_UsesSlugNotName(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "My Special Agent!",
		Slug:            "my-special-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentMessage(ctx, agent, "hello", false, nil)
	if err != nil {
		t.Fatalf("DispatchAgentMessage failed: %v", err)
	}

	if mockClient.lastAgentID != "my-special-agent" {
		t.Errorf("expected slug 'my-special-agent' to be dispatched, got '%s'", mockClient.lastAgentID)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_IncludesAgentIDAndSlug(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "broker-id-test",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	grove := &store.Grove{
		ID:   "grove-id-test",
		Name: "test-grove",
		Slug: "test-grove",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	provider := &store.GroveProvider{
		GroveID:    "grove-id-test",
		BrokerID:   "broker-id-test",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-uuid-123",
		Name:            "my-agent",
		Slug:            "my-agent",
		GroveID:         "grove-id-test",
		RuntimeBrokerID: "broker-id-test",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "do something")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}

	// Verify SCION_AGENT_ID is set to the agent's UUID
	if v, ok := mockClient.lastResolvedEnv["SCION_AGENT_ID"]; !ok || v != "agent-uuid-123" {
		t.Errorf("expected SCION_AGENT_ID='agent-uuid-123', got '%s' (ok=%v)", v, ok)
	}

	// Verify SCION_AGENT_SLUG is set to the agent's slug
	if v, ok := mockClient.lastResolvedEnv["SCION_AGENT_SLUG"]; !ok || v != "my-agent" {
		t.Errorf("expected SCION_AGENT_SLUG='my-agent', got '%s' (ok=%v)", v, ok)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentStart_IncludesInlineConfig verifies that
// DispatchAgentStart passes the agent's InlineConfig to the broker so that
// config changes made after provisioning (e.g. max_turns) are applied.
func TestHTTPAgentDispatcher_DispatchAgentStart_IncludesInlineConfig(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "broker-inline",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	grove := &store.Grove{
		ID:   "grove-inline",
		Name: "test-grove",
		Slug: "test-grove",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	provider := &store.GroveProvider{
		GroveID:    "grove-inline",
		BrokerID:   "broker-inline",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	inlineCfg := &api.ScionConfig{
		MaxTurns:    3,
		MaxDuration: "30m",
	}

	agent := &store.Agent{
		ID:              "agent-inline-cfg",
		Name:            "inline-agent",
		Slug:            "inline-agent",
		GroveID:         "grove-inline",
		RuntimeBrokerID: "broker-inline",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			InlineConfig:  inlineCfg,
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}

	if mockClient.lastInlineConfig == nil {
		t.Fatal("expected InlineConfig to be passed to StartAgent")
	}
	if mockClient.lastInlineConfig.MaxTurns != 3 {
		t.Errorf("expected MaxTurns=3, got %d", mockClient.lastInlineConfig.MaxTurns)
	}
	if mockClient.lastInlineConfig.MaxDuration != "30m" {
		t.Errorf("expected MaxDuration='30m', got '%s'", mockClient.lastInlineConfig.MaxDuration)
	}
}

func TestDispatchAgentStart_IncludesHubEndpoint(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	dispatcher.SetHubEndpoint("http://hub.example.com:8080")

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		OwnerID:         "user-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}

	if mockClient.lastResolvedEnv == nil {
		t.Fatal("expected resolvedEnv to be non-nil")
	}

	// Verify hub endpoint is included in resolvedEnv
	if ep, ok := mockClient.lastResolvedEnv["SCION_HUB_ENDPOINT"]; !ok {
		t.Error("expected SCION_HUB_ENDPOINT in resolvedEnv")
	} else if ep != "http://hub.example.com:8080" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want %q", ep, "http://hub.example.com:8080")
	}

	// Verify agent identity vars are also present
	if mockClient.lastResolvedEnv["SCION_AGENT_ID"] != "agent-1" {
		t.Errorf("SCION_AGENT_ID = %q, want %q", mockClient.lastResolvedEnv["SCION_AGENT_ID"], "agent-1")
	}
	if mockClient.lastResolvedEnv["SCION_GROVE_ID"] != "grove-1" {
		t.Errorf("SCION_GROVE_ID = %q, want %q", mockClient.lastResolvedEnv["SCION_GROVE_ID"], "grove-1")
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_PropagatesSharedWorkspace(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	// Create a shared-workspace git grove
	grove := &store.Grove{
		ID:        "grove-shared-ws",
		Name:      "Shared WS",
		Slug:      "shared-ws",
		GitRemote: "github.com/test/shared",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "host-1",
		Name:     "test-host",
		Slug:     "test-host",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-shared-1",
		Name:            "shared-agent",
		Slug:            "shared-agent",
		GroveID:         "grove-shared-ws",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Workspace:     "/home/user/.scion/groves/shared-ws",
		},
	}

	err := dispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		t.Fatalf("DispatchAgentCreate failed: %v", err)
	}

	if !mockClient.createCalled {
		t.Fatal("expected CreateAgent to be called")
	}
	if mockClient.lastCreateReq.Config == nil {
		t.Fatal("expected config to be present")
	}
	if !mockClient.lastCreateReq.Config.SharedWorkspace {
		t.Error("expected SharedWorkspace=true for shared-workspace git grove")
	}
	if mockClient.lastCreateReq.Config.GitClone != nil {
		t.Error("expected GitClone to be nil for shared-workspace grove")
	}
}

// TestHTTPAgentDispatcher_DispatchAgentStart_InjectsGCPIdentityEnv verifies
// that DispatchAgentStart injects GCP identity env vars from the agent's
// AppliedConfig into resolvedEnv so the broker can configure the metadata
// server sidecar on the start path (used by "Create & Edit" flow).
func TestHTTPAgentDispatcher_DispatchAgentStart_InjectsGCPIdentityEnv(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	grove := &store.Grove{
		ID:        "grove-gcp",
		Name:      "gcp-grove",
		Slug:      "gcp-grove",
		GitRemote: "https://github.com/example/repo.git",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-gcp",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	provider := &store.GroveProvider{
		GroveID:    "grove-gcp",
		BrokerID:   "broker-gcp",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/projects/myproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-gcp-1",
		Name:            "gcp-agent",
		Slug:            "gcp-agent",
		GroveID:         "grove-gcp",
		RuntimeBrokerID: "broker-gcp",
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    "sa-123",
				ServiceAccountEmail: "sa@proj.iam.gserviceaccount.com",
				ProjectID:           "my-project",
			},
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if !mockClient.startCalled {
		t.Fatal("expected StartAgent to be called")
	}

	// Verify GCP identity env vars
	if v := mockClient.lastResolvedEnv["SCION_METADATA_MODE"]; v != "assign" {
		t.Errorf("expected SCION_METADATA_MODE='assign', got %q", v)
	}
	if v := mockClient.lastResolvedEnv["SCION_METADATA_SA_EMAIL"]; v != "sa@proj.iam.gserviceaccount.com" {
		t.Errorf("expected SCION_METADATA_SA_EMAIL='sa@proj.iam.gserviceaccount.com', got %q", v)
	}
	if v := mockClient.lastResolvedEnv["SCION_METADATA_PROJECT_ID"]; v != "my-project" {
		t.Errorf("expected SCION_METADATA_PROJECT_ID='my-project', got %q", v)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentStart_GCPBlockMode(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	grove := &store.Grove{
		ID:        "grove-gcp-block",
		Name:      "gcp-grove",
		Slug:      "gcp-grove",
		GitRemote: "https://github.com/example/repo.git",
	}
	if err := memStore.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	broker := &store.RuntimeBroker{
		ID:       "broker-gcp-block",
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	provider := &store.GroveProvider{
		GroveID:    "grove-gcp-block",
		BrokerID:   "broker-gcp-block",
		BrokerName: "test-broker",
		LocalPath:  "/home/user/projects/myproject/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	agent := &store.Agent{
		ID:              "agent-gcp-block",
		Name:            "gcp-agent",
		Slug:            "gcp-agent",
		GroveID:         "grove-gcp-block",
		RuntimeBrokerID: "broker-gcp-block",
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			},
		},
	}

	err := dispatcher.DispatchAgentStart(ctx, agent, "")
	if err != nil {
		t.Fatalf("DispatchAgentStart failed: %v", err)
	}

	if v := mockClient.lastResolvedEnv["SCION_METADATA_MODE"]; v != "block" {
		t.Errorf("expected SCION_METADATA_MODE='block', got %q", v)
	}
	// SA details should NOT be present for block mode
	if v := mockClient.lastResolvedEnv["SCION_METADATA_SA_EMAIL"]; v != "" {
		t.Errorf("expected empty SCION_METADATA_SA_EMAIL in block mode, got %q", v)
	}
}
