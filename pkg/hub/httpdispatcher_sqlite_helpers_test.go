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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// envScopeTestHubID is the hub instance ID used by the scope-precedence tests.
const envScopeTestHubID = "hub-envscope-1"

// envScopeTestAgent returns an agent wired to every scope the hub env resolver
// knows about, so that all four scopes are applicable.
func envScopeTestAgent() *store.Agent {
	return &store.Agent{
		ID:              "agent-envscope-1",
		Name:            "envscope-agent",
		Slug:            "envscope-agent",
		ProjectID:       "project-envscope-1",
		OwnerID:         "user-envscope-1",
		RuntimeBrokerID: "broker-envscope-1",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
}

// envScopeTestScopeID maps a scope constant to the scope ID used by
// envScopeTestAgent for that scope.
func envScopeTestScopeID(t *testing.T, scope string) string {
	t.Helper()
	switch scope {
	case store.ScopeHub:
		return envScopeTestHubID
	case store.ScopeProject:
		return "project-envscope-1"
	case store.ScopeUser:
		return "user-envscope-1"
	case store.ScopeRuntimeBroker:
		return "broker-envscope-1"
	default:
		t.Fatalf("unknown scope %q", scope)
		return ""
	}
}

// newEnvScopeDispatcher builds a dispatcher over a fresh in-memory store with
// the hub ID set, and seeds key=value pairs in the requested scopes.
func newEnvScopeDispatcher(t *testing.T, key string, valuesByScope map[string]string) (*HTTPAgentDispatcher, store.Store) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)

	for scope, value := range valuesByScope {
		if _, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           key,
			Value:         value,
			Scope:         scope,
			ScopeID:       envScopeTestScopeID(t, scope),
			InjectionMode: store.InjectionModeAlways,
		}); err != nil {
			t.Fatalf("seeding %s-scoped env var: %v", scope, err)
		}
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(envScopeTestHubID)
	return d, memStore
}

// mockKeysBrokerClient embeds the full mockRuntimeBrokerClient stub so it
// satisfies RuntimeBrokerClient for the HTTPAgentDispatcher.client field, and
// adds ExecuteKeys so it also satisfies agentkeys.BrokerClient — proving
// HTTPAgentDispatcher.DispatchAgentKeys reaches the configured client via
// that type assertion.
type mockKeysBrokerClient struct {
	*mockRuntimeBrokerClient
	calls         int
	lastAgentSlug string
	lastReq       agentkeys.BrokerRequest
	lastBrokerID  string
	lastBrokerEP  string
	result        agentkeys.BrokerResult
	err           error
}

// mockGitTokenSecretBackend supports both Resolve (for normal secret resolution)
// and Get (for the GITHUB_TOKEN exemption under NoAuth). It simulates a project
// with GITHUB_TOKEN stored as a project-scoped secret alongside LLM auth secrets.
type mockGitTokenSecretBackend struct {
	secrets      []secret.SecretWithValue
	getResponses map[string]*secret.SecretWithValue
	// getScopedResponses maps "name:scope" to a response, allowing
	// scope-aware lookups (e.g. project vs user scope for GITHUB_TOKEN).
	// When set for a key, it takes precedence over getResponses.
	getScopedResponses map[string]*secret.SecretWithValue
	getCalls           []getCall // track which secrets were fetched via Get
}

// createTestStore creates an in-memory SQLite store for testing.
func createTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	return s
}

// mockRuntimeBrokerClient is a mock implementation of RuntimeBrokerClient for testing.
type mockRuntimeBrokerClient struct {
	lastDeleteProjectPathQuery string
	createCalled               bool
	startCalled                bool
	stopCalled                 bool
	lastStopRunID              string
	restartCalled              bool
	deleteCalled               bool
	messageCalled              bool
	cleanupCalled              bool
	resetAuthCalled            bool
	lastResetToken             string
	lastResetTransportToken    string
	lastBrokerID               string
	lastEndpoint               string
	lastAgentID                string
	lastTask                   string
	lastProjectPath            string
	lastProjectSlug            string
	lastMessage                string
	lastInterrupt              bool
	lastResolvedEnv            map[string]string
	lastRestartResolvedEnv     map[string]string
	lastStartExtras            StartExtras
	lastRestartExtras          StartExtras
	lastInlineConfig           *api.ScionConfig
	lastCreateReq              *RemoteCreateAgentRequest
	lastDeleteOpts             struct {
		deleteFiles, removeBranch bool
		localOnly                 bool
		runID                     string
		notAfter                  time.Time
	}
	returnErr            error
	cleanupErr           error
	startReturnResp      *RemoteAgentResponse // custom start response if set
	restartReturnResp    *RemoteAgentResponse // custom restart response if set
	cleanupCalls         int
	cleanupSlugs         []string
	cleanupProjectIDs    []string
	createWithGatherFunc func(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)
	// startCallCount and failFirstStartWith let a test simulate a
	// hash-mismatch-then-retry sequence: the first StartAgent call fails with
	// failFirstStartWith, and the second (and later) calls succeed.
	startCallCount     int
	failFirstStartWith error
}

// mockSecretBackend is a test implementation of secret.SecretBackend that
// returns a fixed set of secrets from Resolve.
type mockSecretBackend struct {
	secrets []secret.SecretWithValue
}

// setupFinalizeEnvTest creates a broker, project, project provider and agent
// ready for DispatchFinalizeEnv tests.
func setupFinalizeEnvTest(t *testing.T, ctx context.Context, memStore store.Store, asNeededVars []store.EnvVar) *store.Agent {
	t.Helper()

	broker := &store.RuntimeBroker{
		ID:       tid("broker-finalize"),
		Name:     "finalize-broker",
		Slug:     "finalize-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	project := &store.Project{
		ID:   tid("project-finalize"),
		Name: "finalize-project",
		Slug: "finalize-project",
	}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	provider := &store.ProjectProvider{
		ProjectID:  tid("project-finalize"),
		BrokerID:   tid("broker-finalize"),
		BrokerName: "finalize-broker",
		LocalPath:  "/home/user/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	if err := memStore.AddProjectProvider(ctx, provider); err != nil {
		t.Fatalf("failed to add project provider: %v", err)
	}

	for i, v := range asNeededVars {
		v.Scope = "project"
		v.ScopeID = tid("project-finalize")
		v.InjectionMode = store.InjectionModeAsNeeded
		if v.ID == "" {
			v.ID = tid("ev-finalize-" + string(rune('0'+i)))
		}
		asNeededVars[i] = v
		if err := memStore.CreateEnvVar(ctx, &asNeededVars[i]); err != nil {
			t.Fatalf("failed to create env var %q: %v", v.Key, err)
		}
	}

	return &store.Agent{
		ID:              tid("agent-finalize"),
		Name:            "finalize-agent",
		Slug:            "finalize-agent",
		ProjectID:       tid("project-finalize"),
		OwnerID:         "owner-1",
		RuntimeBrokerID: tid("broker-finalize"),
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
}

func intPtr(i int) *int { return &i }

func (m *mockKeysBrokerClient) ExecuteKeys(ctx context.Context, brokerID, brokerEndpoint, agentSlug string, req agentkeys.BrokerRequest) (agentkeys.BrokerResult, error) {
	m.calls++
	m.lastBrokerID = brokerID
	m.lastBrokerEP = brokerEndpoint
	m.lastAgentSlug = agentSlug
	m.lastReq = req
	if m.err != nil {
		return agentkeys.BrokerResult{}, m.err
	}
	return m.result, nil
}

func (m *mockGitTokenSecretBackend) Get(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	m.getCalls = append(m.getCalls, getCall{Name: name, Scope: scope, ScopeID: scopeID})
	// Check scope-specific responses first
	if m.getScopedResponses != nil {
		if sv, ok := m.getScopedResponses[name+":"+scope]; ok {
			return sv, nil
		}
	}
	if sv, ok := m.getResponses[name]; ok {
		return sv, nil
	}
	return nil, nil
}

func (m *mockGitTokenSecretBackend) Set(_ context.Context, _ *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	return false, nil, nil
}

func (m *mockGitTokenSecretBackend) Delete(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockGitTokenSecretBackend) List(_ context.Context, _ secret.Filter) ([]secret.SecretMeta, error) {
	return nil, nil
}

func (m *mockGitTokenSecretBackend) GetMeta(_ context.Context, _, _, _ string) (*secret.SecretMeta, error) {
	return nil, nil
}

func (m *mockGitTokenSecretBackend) UpdateMeta(_ context.Context, _ *secret.UpdateMetaInput) (*secret.SecretMeta, error) {
	return nil, nil
}

func (m *mockGitTokenSecretBackend) Resolve(_ context.Context, _, _, _ string, _ *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return m.secrets, nil
}

func (m *mockGitTokenSecretBackend) HubID() string { return "test-hub" }

func (m *mockGitTokenSecretBackend) FetchValues(_ context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	results := make(map[string]secret.FetchResult, len(metas))
	for _, meta := range metas {
		results[meta.ID] = secret.FetchResult{Err: store.ErrNotFound}
	}
	return results, nil
}

type getCall struct {
	Name    string
	Scope   string
	ScopeID string
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

func (m *mockRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	m.startCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastTask = task
	m.lastProjectPath = projectPath
	m.lastProjectSlug = projectSlug
	m.lastResolvedEnv = resolvedEnv
	m.lastInlineConfig = inlineConfig
	m.lastStartExtras = extras
	m.startCallCount++
	if m.startCallCount == 1 && m.failFirstStartWith != nil {
		return nil, m.failFirstStartWith
	}
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

func (m *mockRuntimeBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, runID string) error {
	m.stopCalled = true
	m.lastStopRunID = runID
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	m.restartCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastRestartResolvedEnv = resolvedEnv
	m.lastRestartExtras = extras
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return m.restartReturnResp, nil
}

func (m *mockRuntimeBrokerClient) ResetAuthAgent(_ context.Context, _, _, _, _, token, transportToken string) error {
	m.resetAuthCalled = true
	m.lastResetToken = token
	m.lastResetTransportToken = transportToken
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, opts DeleteAgentOptions) error {
	m.deleteCalled = true
	m.lastDeleteProjectPathQuery = deleteProjectPathQuery(ctx)
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastDeleteOpts.deleteFiles = opts.DeleteFiles
	m.lastDeleteOpts.removeBranch = opts.RemoveBranch
	m.lastDeleteOpts.runID = opts.RunID
	m.lastDeleteOpts.notAfter = opts.NotAfter
	m.lastDeleteOpts.localOnly = opts.LocalOnly
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	if m.returnErr != nil {
		return "", 0, m.returnErr
	}
	return "mock exec output", 0, nil
}

func (m *mockRuntimeBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	m.messageCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastAgentID = agentID
	m.lastMessage = message
	m.lastInterrupt = interrupt
	return m.returnErr
}

func (m *mockRuntimeBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error) {
	return false, m.returnErr
}

func (m *mockRuntimeBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	return "", nil
}

func (m *mockRuntimeBrokerClient) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug, projectID string) error {
	m.cleanupCalled = true
	m.cleanupCalls++
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.cleanupSlugs = append(m.cleanupSlugs, projectSlug)
	m.cleanupProjectIDs = append(m.cleanupProjectIDs, projectID)
	return m.cleanupErr
}

func (m *mockRuntimeBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	m.createCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastCreateReq = req
	if m.createWithGatherFunc != nil {
		return m.createWithGatherFunc(ctx, brokerID, brokerEndpoint, req)
	}
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
		// Reprovisioned mirrors a real (non-stale) broker's echo: it only
		// runs Manager.Reprovision, and only sets this, when the request
		// asked for it. See runtimebroker/handlers.go's ProvisionOnly branch.
		Reprovisioned: req.Reprovision,
	}, nil, nil
}

func (m *mockSecretBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	return nil, nil
}
func (m *mockSecretBackend) Set(ctx context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	return false, nil, nil
}
func (m *mockSecretBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	return nil
}
func (m *mockSecretBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	return nil, nil
}
func (m *mockSecretBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	return nil, nil
}
func (m *mockSecretBackend) UpdateMeta(ctx context.Context, input *secret.UpdateMetaInput) (*secret.SecretMeta, error) {
	return nil, nil
}
func (m *mockSecretBackend) Resolve(ctx context.Context, userID, projectID, brokerID string, opts *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return m.secrets, nil
}
func (m *mockSecretBackend) HubID() string { return "test-hub" }
func (m *mockSecretBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	results := make(map[string]secret.FetchResult, len(metas))
	for _, meta := range metas {
		results[meta.ID] = secret.FetchResult{Err: store.ErrNotFound}
	}
	return results, nil
}
