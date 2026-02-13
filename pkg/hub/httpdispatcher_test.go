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

	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/store/sqlite"
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
	createCalled   bool
	startCalled    bool
	stopCalled     bool
	restartCalled  bool
	deleteCalled   bool
	messageCalled  bool
	lastBrokerID string
	lastEndpoint   string
	lastAgentID    string
	lastMessage    string
	lastInterrupt  bool
	lastCreateReq  *RemoteCreateAgentRequest
	lastDeleteOpts struct{ deleteFiles, removeBranch bool }
	returnErr      error
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
			Status:          "running",
			ContainerStatus: "Up 5 seconds",
		},
		Created: true,
	}, nil
}

func (m *mockRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, task string) error {
	m.startCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	m.stopCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error {
	m.restartCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch bool) error {
	m.deleteCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastDeleteOpts.deleteFiles = deleteFiles
	m.lastDeleteOpts.removeBranch = removeBranch
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, message string, interrupt bool) error {
	m.messageCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastMessage = message
	m.lastInterrupt = interrupt
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID string) (bool, error) {
	return false, m.returnErr
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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

	agent := &store.Agent{
		ID:            "agent-1",
		Name:          "test-agent",
		GroveID:       "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			Harness: "claude",
			Task:    "Fix a bug",
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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

	agent := &store.Agent{
		ID:            "agent-1",
		Name:          "test-agent",
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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

	agent := &store.Agent{
		ID:            "agent-1",
		Name:          "test-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentDelete(ctx, agent, true, false)
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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

	agent := &store.Agent{
		ID:            "agent-1",
		Name:          "test-agent",
		RuntimeBrokerID: "host-1",
	}

	err := dispatcher.DispatchAgentMessage(ctx, agent, "Hello, agent!", true)
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
				Status:          "running",
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

	err := client.StopAgent(context.Background(), "host-1", server.URL, "test-agent")
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

	err := client.DeleteAgent(context.Background(), "host-1", server.URL, "test-agent", true, false)
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

	err := client.MessageAgent(context.Background(), "host-1", server.URL, "test-agent", "Hello!", true)
	if err != nil {
		t.Fatalf("MessageAgent failed: %v", err)
	}
}

func TestHTTPAgentDispatcher_DispatchAgentCreate_WithGroveProviderPath(t *testing.T) {
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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			Harness: "claude",
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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

	agent := &store.Agent{
		ID:              "agent-1",
		Name:            "test-agent",
		Slug:            "test-agent",
		GroveID:         "grove-1",
		RuntimeBrokerID: "host-1",
		AppliedConfig: &store.AgentAppliedConfig{
			Harness:   "claude",
			Task:      "do something",
			Workspace: "./subfolder",
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
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false)

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
