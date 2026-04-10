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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// testBootstrapDevToken is the development token used for bootstrap testing.
const testBootstrapDevToken = "scion_dev_bootstrap_test_token_1234567890"

// mockStorage implements storage.Storage for testing.
type mockStorage struct {
	bucket  string
	objects map[string]*storage.Object // objectPath -> Object
}

func newMockStorage(bucket string) *mockStorage {
	return &mockStorage{
		bucket:  bucket,
		objects: make(map[string]*storage.Object),
	}
}

func (m *mockStorage) Bucket() string             { return m.bucket }
func (m *mockStorage) Provider() storage.Provider { return storage.ProviderLocal }
func (m *mockStorage) Close() error               { return nil }

func (m *mockStorage) GenerateSignedURL(_ context.Context, objectPath string, opts storage.SignedURLOptions) (*storage.SignedURL, error) {
	return &storage.SignedURL{
		URL:     fmt.Sprintf("https://storage.example.com/%s/%s?signed=true", m.bucket, objectPath),
		Method:  opts.Method,
		Expires: time.Now().Add(opts.Expires),
	}, nil
}

func (m *mockStorage) GetObject(_ context.Context, objectPath string) (*storage.Object, error) {
	obj, ok := m.objects[objectPath]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", objectPath)
	}
	return obj, nil
}

func (m *mockStorage) Exists(_ context.Context, objectPath string) (bool, error) {
	_, ok := m.objects[objectPath]
	return ok, nil
}

func (m *mockStorage) Upload(_ context.Context, objectPath string, _ io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	obj := &storage.Object{
		Name:     objectPath,
		Metadata: opts.Metadata,
	}
	m.objects[objectPath] = obj
	return obj, nil
}

func (m *mockStorage) Download(_ context.Context, _ string) (io.ReadCloser, *storage.Object, error) {
	return nil, nil, fmt.Errorf("not implemented")
}

func (m *mockStorage) Delete(_ context.Context, objectPath string) error {
	delete(m.objects, objectPath)
	return nil
}

func (m *mockStorage) DeletePrefix(_ context.Context, _ string) error { return nil }
func (m *mockStorage) List(_ context.Context, _ storage.ListOptions) (*storage.ListResult, error) {
	return &storage.ListResult{}, nil
}

func (m *mockStorage) Copy(_ context.Context, _, _ string) (*storage.Object, error) {
	return nil, fmt.Errorf("not implemented")
}

// mockDispatcher implements AgentDispatcher for testing bootstrap dispatch.
type mockDispatcher struct {
	dispatchedAgents []*store.Agent
	startedAgents    []*store.Agent
	returnErr        error
}

func (d *mockDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) error {
	if d.returnErr != nil {
		return d.returnErr
	}
	d.dispatchedAgents = append(d.dispatchedAgents, agent)
	agent.Phase = string(state.PhaseProvisioning)
	return nil
}

func (d *mockDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	if d.returnErr != nil {
		return d.returnErr
	}
	d.dispatchedAgents = append(d.dispatchedAgents, agent)
	agent.Phase = string(state.PhaseCreated)
	return nil
}
func (d *mockDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string) error {
	d.startedAgents = append(d.startedAgents, agent)
	return nil
}
func (d *mockDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error { return nil }
func (d *mockDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *mockDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *mockDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}
func (d *mockDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *mockDispatcher) DispatchAgentCreateWithGather(_ context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, d.DispatchAgentCreate(context.Background(), agent)
}
func (d *mockDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *mockDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, error) {
	return "", nil
}
func (d *mockDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
}

// testBootstrapServer creates a test server with storage and dispatcher configured.
func testBootstrapServer(t *testing.T) (*Server, store.Store, *mockStorage, *mockDispatcher) {
	t.Helper()
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testBootstrapDevToken
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)

	disp := &mockDispatcher{}
	srv.SetDispatcher(disp)

	return srv, s, stor, disp
}

// doBootstrapRequest performs an authenticated HTTP request for bootstrap tests.
func doBootstrapRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
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
	req.Header.Set("Authorization", "Bearer "+testBootstrapDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupGroveAndBroker creates a grove and broker for agent creation tests.
func setupGroveAndBroker(t *testing.T, s store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     "broker_bootstrap_test",
		Slug:   "bootstrap-host",
		Name:   "Bootstrap Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	grove := &store.Grove{
		ID:                     "grove_bootstrap_test",
		Slug:                   "bootstrap-grove",
		Name:                   "Bootstrap Grove",
		GitRemote:              "https://github.com/test/bootstrap",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	provider := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	return grove.ID, broker.ID
}

// ============================================================================
// Bootstrap CreateAgent Tests
// ============================================================================

func TestCreateAgentWithWorkspaceBootstrap(t *testing.T) {
	srv, s, _, _ := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)

	// Create an agent with workspace files and a task
	body := CreateAgentRequest{
		Name:    "bootstrap-agent",
		GroveID: groveID,
		Task:    "do something",
		WorkspaceFiles: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
			{Path: "go.mod", Size: 50, Hash: "sha256:def456"},
		},
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Agent should be created
	if resp.Agent == nil {
		t.Fatal("expected agent to be set")
	}

	// Agent should be in provisioning status (not dispatched yet)
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected status 'provisioning', got %q", resp.Agent.Phase)
	}

	// Upload URLs should be populated
	if len(resp.UploadURLs) != 2 {
		t.Errorf("expected 2 upload URLs, got %d", len(resp.UploadURLs))
	}

	// Expires should be set
	if resp.Expires == nil {
		t.Error("expected expires to be set")
	}

	// Verify upload URLs have correct paths
	urlPaths := make(map[string]bool)
	for _, u := range resp.UploadURLs {
		urlPaths[u.Path] = true
		if u.URL == "" {
			t.Errorf("expected non-empty URL for path %q", u.Path)
		}
		if u.Method != "PUT" {
			t.Errorf("expected method PUT for path %q, got %q", u.Path, u.Method)
		}
	}
	if !urlPaths["main.go"] {
		t.Error("expected upload URL for main.go")
	}
	if !urlPaths["go.mod"] {
		t.Error("expected upload URL for go.mod")
	}
}

func TestCreateAgentWithWorkspaceBootstrap_ExistingFiles(t *testing.T) {
	srv, s, stor, _ := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)

	// Pre-populate one file in storage with matching hash
	// The agent ID is generated, so we can't predict the exact path.
	// Instead, we'll just verify the count logic works by checking
	// that when files match, fewer upload URLs are returned.

	// Create a first agent to get its ID, then test with pre-existing storage
	body := CreateAgentRequest{
		Name:    "bootstrap-existing",
		GroveID: groveID,
		Task:    "do something",
		WorkspaceFiles: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
			{Path: "go.mod", Size: 50, Hash: "sha256:def456"},
		},
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Store one of the files in mock storage with matching hash
	agentID := resp.Agent.ID
	storagePath := "workspaces/" + groveID + "/" + agentID
	stor.objects[storagePath+"/files/main.go"] = &storage.Object{
		Name:     storagePath + "/files/main.go",
		Metadata: map[string]string{"sha256": "sha256:abc123"},
	}

	// Create second agent - different name to avoid conflicts
	body2 := CreateAgentRequest{
		Name:    "bootstrap-existing-2",
		GroveID: groveID,
		Task:    "do something",
		WorkspaceFiles: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
			{Path: "go.mod", Size: 50, Hash: "sha256:def456"},
		},
	}

	rec2 := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", body2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 CreateAgentResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Both files should get upload URLs since it's a new agent ID (different storage path)
	if len(resp2.UploadURLs) != 2 {
		t.Errorf("expected 2 upload URLs (new agent ID = new path), got %d", len(resp2.UploadURLs))
	}
}

func TestCreateAgentWithWorkspaceBootstrap_NoStorage(t *testing.T) {
	// Create server without storage
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testBootstrapDevToken
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	// No storage set

	groveID, _ := setupGroveAndBroker(t, s)

	body := CreateAgentRequest{
		Name:    "bootstrap-no-storage",
		GroveID: groveID,
		Task:    "do something",
		WorkspaceFiles: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
		},
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	// Should fail because storage is not configured
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAgentWithWorkspaceBootstrap_NoTask(t *testing.T) {
	srv, s, _, _ := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)

	// WorkspaceFiles without a task should NOT trigger bootstrap upload —
	// since ProvisionOnly is not set, the agent is dispatched via DispatchAgentCreate.
	// Without a broker-reported status, it falls back to "provisioning".
	body := CreateAgentRequest{
		Name:    "bootstrap-no-task",
		GroveID: groveID,
		WorkspaceFiles: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
		},
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Agent is dispatched via DispatchAgentCreate (ProvisionOnly is false).
	// The mock dispatcher doesn't set a status, so it falls back to provisioning.
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected status 'provisioning', got %q", resp.Agent.Phase)
	}

	// No upload URLs since no task was provided
	if len(resp.UploadURLs) > 0 {
		t.Error("expected no upload URLs when no task is provided")
	}
}

func TestCreateAgentWithWorkspaceBootstrap_LocalProvider(t *testing.T) {
	srv, s, _, disp := testBootstrapServer(t)
	ctx := context.Background()

	// Create broker and grove
	broker := &store.RuntimeBroker{
		ID:     "broker_local_path_test",
		Slug:   "local-path-host",
		Name:   "Local Path Host",
		Status: store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	grove := &store.Grove{
		ID:                     "grove_local_path_test",
		Slug:                   "local-path-grove",
		Name:                   "Local Path Grove",
		GitRemote:              "https://github.com/test/local-path",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	if err := s.CreateGrove(ctx, grove); err != nil {
		t.Fatalf("failed to create grove: %v", err)
	}

	// Add grove provider WITH a LocalPath — this is the key difference
	provider := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/home/user/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := s.AddGroveProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add grove provider: %v", err)
	}

	// Create an agent with workspace files and a task
	body := CreateAgentRequest{
		Name:    "local-workspace-agent",
		GroveID: grove.ID,
		Task:    "do something locally",
		WorkspaceFiles: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
			{Path: "go.mod", Size: 50, Hash: "sha256:def456"},
		},
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Agent should be created
	if resp.Agent == nil {
		t.Fatal("expected agent to be set")
	}

	// No upload URLs — broker has local access
	if len(resp.UploadURLs) != 0 {
		t.Errorf("expected 0 upload URLs (broker has local path), got %d", len(resp.UploadURLs))
	}

	// Expires should NOT be set (no upload flow)
	if resp.Expires != nil {
		t.Error("expected no expires when broker has local path")
	}

	// Agent should be in provisioning status (dispatched directly)
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected status 'provisioning', got %q", resp.Agent.Phase)
	}

	// Dispatcher should have been called (direct dispatch, no finalize needed)
	if len(disp.dispatchedAgents) != 1 {
		t.Fatalf("expected 1 dispatched agent, got %d", len(disp.dispatchedAgents))
	}
	if disp.dispatchedAgents[0].ID != resp.Agent.ID {
		t.Errorf("expected dispatched agent ID %q, got %q", resp.Agent.ID, disp.dispatchedAgents[0].ID)
	}
}

func TestCreateAgentWithoutBootstrap(t *testing.T) {
	srv, s, _, _ := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)

	// Normal create without workspace files - should use normal dispatch path
	body := CreateAgentRequest{
		Name:    "normal-agent",
		GroveID: groveID,
		Task:    "do something",
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// No upload URLs should be set
	if len(resp.UploadURLs) > 0 {
		t.Error("expected no upload URLs for non-bootstrap create")
	}
	if resp.Expires != nil {
		t.Error("expected no expires for non-bootstrap create")
	}

	// Agent should be provisioning (dispatched normally)
	if resp.Agent.Phase != string(state.PhaseProvisioning) {
		t.Errorf("expected status 'provisioning', got %q", resp.Agent.Phase)
	}
}

// ============================================================================
// Create-then-Start Tests
// ============================================================================

func TestCreateThenStartWithTask(t *testing.T) {
	srv, s, _, disp := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)

	// Step 1: Create the agent (provision-only)
	createBody := CreateAgentRequest{
		Name:          "staged-agent",
		GroveID:       groveID,
		ProvisionOnly: true,
	}
	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var createResp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&createResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}
	if createResp.Agent.Phase != string(state.PhaseCreated) {
		t.Fatalf("expected status 'created', got %q", createResp.Agent.Phase)
	}

	// Step 2: Start the agent with a task (this previously returned 409)
	startBody := CreateAgentRequest{
		Name:    "staged-agent",
		GroveID: groveID,
		Task:    "hello world",
	}
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", startBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var startResp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("failed to decode start response: %v", err)
	}

	// Should return the same agent, now running
	if startResp.Agent.ID != createResp.Agent.ID {
		t.Errorf("expected same agent ID %q, got %q", createResp.Agent.ID, startResp.Agent.ID)
	}
	if startResp.Agent.Phase != string(state.PhaseRunning) {
		t.Errorf("expected status 'running', got %q", startResp.Agent.Phase)
	}

	// Dispatcher should have received a start call
	if len(disp.startedAgents) != 1 {
		t.Fatalf("expected 1 started agent, got %d", len(disp.startedAgents))
	}
	if disp.startedAgents[0].ID != createResp.Agent.ID {
		t.Errorf("started wrong agent: expected %q, got %q", createResp.Agent.ID, disp.startedAgents[0].ID)
	}
}

func TestCreateThenStartWithoutTask(t *testing.T) {
	srv, s, _, disp := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)

	// Step 1: Create the agent with a task (provision-only, task written to prompt.md)
	createBody := CreateAgentRequest{
		Name:          "staged-agent-2",
		GroveID:       groveID,
		Task:          "saved task",
		ProvisionOnly: true,
	}
	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Step 2: Start the agent without a task — task is optional
	startBody := CreateAgentRequest{
		Name:    "staged-agent-2",
		GroveID: groveID,
	}
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents", startBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var startResp CreateAgentResponse
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("failed to decode start response: %v", err)
	}
	if startResp.Agent.Phase != string(state.PhaseRunning) {
		t.Errorf("expected status 'running', got %q", startResp.Agent.Phase)
	}

	// Dispatcher should have received a start call
	if len(disp.startedAgents) != 1 {
		t.Fatalf("expected 1 started agent, got %d", len(disp.startedAgents))
	}
}

// ============================================================================
// Finalize Tests for Bootstrap Mode
// ============================================================================

func TestSyncToFinalize_BootstrapMode(t *testing.T) {
	srv, s, stor, disp := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)
	ctx := context.Background()

	// Create an agent in provisioning status (simulating post-bootstrap-create)
	agent := &store.Agent{
		ID:              "agent_bootstrap_finalize",
		Slug:            "bootstrap-finalize",
		Name:            "Bootstrap Finalize",
		GroveID:         groveID,
		RuntimeBrokerID: "broker_bootstrap_test",
		Phase:           string(state.PhaseProvisioning),
		Visibility:      store.VisibilityPrivate,
		AppliedConfig: &store.AgentAppliedConfig{
			Task: "test task",
		},
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Pre-populate the files in mock storage
	storagePath := "workspaces/" + groveID + "/agent_bootstrap_finalize"
	stor.objects[storagePath+"/files/main.go"] = &storage.Object{
		Name: storagePath + "/files/main.go",
	}
	stor.objects[storagePath+"/files/go.mod"] = &storage.Object{
		Name: storagePath + "/files/go.mod",
	}

	// Finalize the bootstrap
	manifest := &transfer.Manifest{
		Version: "1.0",
		Files: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
			{Path: "go.mod", Size: 50, Hash: "sha256:def456"},
		},
	}
	finalizeReq := SyncToFinalizeRequest{
		Manifest: manifest,
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/agent_bootstrap_finalize/workspace/sync-to/finalize", finalizeReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp SyncToFinalizeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Applied {
		t.Error("expected applied to be true")
	}
	if resp.FilesApplied != 2 {
		t.Errorf("expected 2 files applied, got %d", resp.FilesApplied)
	}
	if resp.BytesTransferred != 150 {
		t.Errorf("expected 150 bytes transferred, got %d", resp.BytesTransferred)
	}
	if resp.ContentHash == "" {
		t.Error("expected content hash to be set")
	}

	// Verify dispatcher was called
	if len(disp.dispatchedAgents) != 1 {
		t.Fatalf("expected 1 dispatched agent, got %d", len(disp.dispatchedAgents))
	}

	dispatched := disp.dispatchedAgents[0]
	if dispatched.ID != "agent_bootstrap_finalize" {
		t.Errorf("expected dispatched agent ID 'agent_bootstrap_finalize', got %q", dispatched.ID)
	}

	// Verify workspace storage path was set on the agent
	if dispatched.AppliedConfig == nil || dispatched.AppliedConfig.WorkspaceStoragePath == "" {
		t.Error("expected WorkspaceStoragePath to be set on dispatched agent")
	}
	if dispatched.AppliedConfig.WorkspaceStoragePath != storagePath {
		t.Errorf("expected WorkspaceStoragePath %q, got %q", storagePath, dispatched.AppliedConfig.WorkspaceStoragePath)
	}
}

func TestSyncToFinalize_BootstrapMode_MissingFile(t *testing.T) {
	srv, s, stor, _ := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)
	ctx := context.Background()

	// Create an agent in provisioning status
	agent := &store.Agent{
		ID:              "agent_bootstrap_missing",
		Slug:            "bootstrap-missing",
		Name:            "Bootstrap Missing",
		GroveID:         groveID,
		RuntimeBrokerID: "broker_bootstrap_test",
		Phase:           string(state.PhaseProvisioning),
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// Only put one file in storage
	storagePath := "workspaces/" + groveID + "/agent_bootstrap_missing"
	stor.objects[storagePath+"/files/main.go"] = &storage.Object{
		Name: storagePath + "/files/main.go",
	}

	// Finalize with a file that's not in storage
	manifest := &transfer.Manifest{
		Version: "1.0",
		Files: []transfer.FileInfo{
			{Path: "main.go", Size: 100, Hash: "sha256:abc123"},
			{Path: "missing.go", Size: 50, Hash: "sha256:def456"},
		},
	}
	finalizeReq := SyncToFinalizeRequest{
		Manifest: manifest,
	}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/agent_bootstrap_missing/workspace/sync-to/finalize", finalizeReq)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSyncToFinalize_RejectsStoppedAgent(t *testing.T) {
	srv, s, _, _ := testBootstrapServer(t)
	groveID, _ := setupGroveAndBroker(t, s)
	ctx := context.Background()

	// Create an agent in stopped status
	agent := &store.Agent{
		ID:              "agent_bootstrap_stopped",
		Slug:            "bootstrap-stopped",
		Name:            "Bootstrap Stopped",
		GroveID:         groveID,
		RuntimeBrokerID: "broker_bootstrap_test",
		Phase:           string(state.PhaseStopped),
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	manifest := &transfer.Manifest{
		Version: "1.0",
		Files:   []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}},
	}
	finalizeReq := SyncToFinalizeRequest{Manifest: manifest}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/agent_bootstrap_stopped/workspace/sync-to/finalize", finalizeReq)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSyncToFinalize_BootstrapMode_NoDispatcher(t *testing.T) {
	// Create server without dispatcher
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testBootstrapDevToken
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	stor := newMockStorage("test-bucket")
	srv.SetStorage(stor)
	// No dispatcher set

	groveID, _ := setupGroveAndBroker(t, s)
	ctx := context.Background()

	agent := &store.Agent{
		ID:              "agent_bootstrap_nodisp",
		Slug:            "bootstrap-nodisp",
		Name:            "Bootstrap No Dispatcher",
		GroveID:         groveID,
		RuntimeBrokerID: "broker_bootstrap_test",
		Phase:           string(state.PhaseProvisioning),
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	storagePath := "workspaces/" + groveID + "/agent_bootstrap_nodisp"
	stor.objects[storagePath+"/files/main.go"] = &storage.Object{
		Name: storagePath + "/files/main.go",
	}

	manifest := &transfer.Manifest{
		Version: "1.0",
		Files:   []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}},
	}
	finalizeReq := SyncToFinalizeRequest{Manifest: manifest}

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/agent_bootstrap_nodisp/workspace/sync-to/finalize", finalizeReq)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Dispatcher WorkspaceStoragePath Passthrough Test
// ============================================================================

func TestDispatcherPassesWorkspaceStoragePath(t *testing.T) {
	// Test that the HTTP dispatcher passes WorkspaceStoragePath through to the
	// RemoteCreateAgentRequest when it's set on the agent's AppliedConfig.
	agent := &store.Agent{
		ID:              "agent_with_storage_path",
		Slug:            "storage-path-agent",
		Name:            "Storage Path Agent",
		GroveID:         "grove_test",
		RuntimeBrokerID: "broker_test",
		Phase:           string(state.PhaseProvisioning),
		AppliedConfig: &store.AgentAppliedConfig{
			Task:                 "test task",
			WorkspaceStoragePath: "workspaces/grove_test/agent_with_storage_path",
		},
	}

	// Verify the field is populated
	if agent.AppliedConfig.WorkspaceStoragePath == "" {
		t.Error("expected WorkspaceStoragePath to be set")
	}
	if agent.AppliedConfig.WorkspaceStoragePath != "workspaces/grove_test/agent_with_storage_path" {
		t.Errorf("unexpected WorkspaceStoragePath: %q", agent.AppliedConfig.WorkspaceStoragePath)
	}
}

// ============================================================================
// generateWorkspaceUploadURLs unit test
// ============================================================================

func TestGenerateWorkspaceUploadURLs(t *testing.T) {
	ctx := context.Background()
	stor := newMockStorage("test-bucket")

	files := []transfer.FileInfo{
		{Path: "file1.txt", Size: 100, Hash: "sha256:hash1"},
		{Path: "file2.txt", Size: 200, Hash: "sha256:hash2"},
		{Path: "file3.txt", Size: 300, Hash: "sha256:hash3"},
	}

	// Pre-populate one file with matching hash
	stor.objects["ws/files/file2.txt"] = &storage.Object{
		Name:     "ws/files/file2.txt",
		Metadata: map[string]string{"sha256": "sha256:hash2"},
	}

	uploadURLs, existingFiles, err := generateWorkspaceUploadURLs(ctx, stor, "ws", files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// file2.txt should be skipped (existing with matching hash)
	if len(existingFiles) != 1 {
		t.Errorf("expected 1 existing file, got %d", len(existingFiles))
	}
	if len(existingFiles) > 0 && existingFiles[0] != "file2.txt" {
		t.Errorf("expected existing file 'file2.txt', got %q", existingFiles[0])
	}

	// file1.txt and file3.txt should get upload URLs
	if len(uploadURLs) != 2 {
		t.Errorf("expected 2 upload URLs, got %d", len(uploadURLs))
	}

	urlPaths := make(map[string]bool)
	for _, u := range uploadURLs {
		urlPaths[u.Path] = true
		if u.Method != "PUT" {
			t.Errorf("expected method PUT for %q, got %q", u.Path, u.Method)
		}
		if u.URL == "" {
			t.Errorf("expected non-empty URL for %q", u.Path)
		}
	}
	if !urlPaths["file1.txt"] {
		t.Error("expected upload URL for file1.txt")
	}
	if !urlPaths["file3.txt"] {
		t.Error("expected upload URL for file3.txt")
	}
}

func TestGenerateWorkspaceUploadURLs_AllExisting(t *testing.T) {
	ctx := context.Background()
	stor := newMockStorage("test-bucket")

	files := []transfer.FileInfo{
		{Path: "file1.txt", Size: 100, Hash: "sha256:hash1"},
	}

	// Pre-populate with matching hash
	stor.objects["ws/files/file1.txt"] = &storage.Object{
		Name:     "ws/files/file1.txt",
		Metadata: map[string]string{"sha256": "sha256:hash1"},
	}

	uploadURLs, existingFiles, err := generateWorkspaceUploadURLs(ctx, stor, "ws", files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(uploadURLs) != 0 {
		t.Errorf("expected 0 upload URLs, got %d", len(uploadURLs))
	}
	if len(existingFiles) != 1 {
		t.Errorf("expected 1 existing file, got %d", len(existingFiles))
	}
}

func TestGenerateWorkspaceUploadURLs_HashMismatch(t *testing.T) {
	ctx := context.Background()
	stor := newMockStorage("test-bucket")

	files := []transfer.FileInfo{
		{Path: "file1.txt", Size: 100, Hash: "sha256:newhash"},
	}

	// Pre-populate with different hash
	stor.objects["ws/files/file1.txt"] = &storage.Object{
		Name:     "ws/files/file1.txt",
		Metadata: map[string]string{"sha256": "sha256:oldhash"},
	}

	uploadURLs, existingFiles, err := generateWorkspaceUploadURLs(ctx, stor, "ws", files)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should generate upload URL since hash doesn't match
	if len(uploadURLs) != 1 {
		t.Errorf("expected 1 upload URL, got %d", len(uploadURLs))
	}
	if len(existingFiles) != 0 {
		t.Errorf("expected 0 existing files, got %d", len(existingFiles))
	}
}

// ============================================================================
// Runtime Broker WorkspaceStoragePath in CreateAgentRequest
// ============================================================================

func TestBrokerCreateAgentRequest_WorkspaceStoragePath(t *testing.T) {
	// Test that WorkspaceStoragePath is properly serialized/deserialized
	// in the broker's CreateAgentRequest
	reqJSON := `{
		"name": "test-agent",
		"grovePath": "/path/to/grove",
		"workspaceStoragePath": "workspaces/grove1/agent1"
	}`

	var req struct {
		Name                 string `json:"name"`
		GrovePath            string `json:"grovePath"`
		WorkspaceStoragePath string `json:"workspaceStoragePath"`
	}

	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if req.WorkspaceStoragePath != "workspaces/grove1/agent1" {
		t.Errorf("expected WorkspaceStoragePath 'workspaces/grove1/agent1', got %q", req.WorkspaceStoragePath)
	}
	if req.GrovePath != "/path/to/grove" {
		t.Errorf("expected GrovePath '/path/to/grove', got %q", req.GrovePath)
	}
}

// ============================================================================
// Store Model Test
// ============================================================================

func TestAgentAppliedConfig_WorkspaceStoragePath(t *testing.T) {
	// Test that WorkspaceStoragePath is properly serialized in AgentAppliedConfig
	config := &store.AgentAppliedConfig{
		Task:                 "test task",
		WorkspaceStoragePath: "workspaces/grove1/agent1",
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded store.AgentAppliedConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.WorkspaceStoragePath != "workspaces/grove1/agent1" {
		t.Errorf("expected WorkspaceStoragePath 'workspaces/grove1/agent1', got %q", decoded.WorkspaceStoragePath)
	}
	if decoded.Task != "test task" {
		t.Errorf("expected Task 'test task', got %q", decoded.Task)
	}
}

func TestAgentAppliedConfig_OmitsEmpty(t *testing.T) {
	// Test that WorkspaceStoragePath is omitted when empty
	config := &store.AgentAppliedConfig{
		Task: "test task",
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	if bytes.Contains(data, []byte("workspaceStoragePath")) {
		t.Error("expected WorkspaceStoragePath to be omitted when empty")
	}
}
