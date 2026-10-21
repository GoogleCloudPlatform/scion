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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// mockManager implements agent.Manager for testing.
//
// mu guards every mutable bookkeeping field below (the counters and the
// last-* capture fields), because more than one Server-owned goroutine can
// call into the same mockManager concurrently — most notably HeartbeatService,
// which runs its own background goroutine per hub connection and calls
// List() on a timer, independently of whatever the test's own goroutine is
// doing with the same manager. A shared, unsynchronized field written by
// both racing writers is exactly the shape of bug `go test -race` exists to
// catch (and did: concurrent heartbeat goroutines writing lastListFilter).
// Every write below holds mu; every external (test-file) read goes through
// the matching accessor method below, which also holds mu — direct field
// access from outside this file is the bug this comment exists to prevent
// from coming back.
type mockManager struct {
	mu sync.Mutex
	// createHandles are launch resources Start reports creating (each after
	// a checkpoint); beforeCreate, when set, runs before the i-th one.
	createHandles []api.ResourceHandle
	beforeCreate  func(i int)
	// startErrAfterCreate, when set, fails Start after createHandles were
	// created.
	startErrAfterCreate    error
	agents                 []api.AgentInfo
	startCalls             int
	stopCalls              int
	deleteCalls            int
	startErr               error
	provisionErr           error
	stopErr                error
	listErr                error
	deleteTargetErr        error
	messageErr             error
	lastStartOpts          api.StartOptions
	lastDeleteProjectPath  string
	lastDeleteAgentID      string
	lastDeleteContainerID  string
	lastDeleteRunID        string
	lastDeleteFiles        bool
	lastDeleteRemoveBranch bool
	lastStopAgentID        string
	lastStopRunID          string
	// lastStartCtx captures the context passed to Start, so tests can assert
	// on what was attached to it (e.g. a skill resolver, #1960) without a
	// real container runtime or ProvisionAgent call.
	lastStartCtx context.Context
	// lastListFilter captures the filter map passed to List, so tests can
	// assert on which keys the handler builds from query parameters.
	lastListFilter map[string]string
	// sendKeysFunc, when set, backs SendKeys so keys-handler tests can
	// control its return value (including the three agentkeys sentinels)
	// and capture its arguments, without needing a real AgentManager/tmux.
	sendKeysFunc func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error

	// preflightErr, when set, is returned by Preflight (e.g.
	// config.ErrTemplateNotFound, for the async-create admission 404 case).
	preflightErr      error
	preflightCalls    int
	lastPreflightOpts api.StartOptions

	// cleanupLaunchCalls/lastCleanupLaunchHandles record CleanupLaunch
	// invocations so tests can assert on report-first failure cleanup.
	cleanupLaunchCalls       int
	lastCleanupLaunchHandles []agent.ResourceHandle
	cleanupLaunchErr         error
}

func (m *mockManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	if m.provisionErr != nil {
		return nil, m.provisionErr
	}
	return &api.ScionConfig{}, nil
}

func (m *mockManager) Preflight(ctx context.Context, opts api.StartOptions) error {
	m.preflightCalls++
	m.lastPreflightOpts = opts
	return m.preflightErr
}

// SetOwner accepts a flat instance's owner scope (the mock filters nothing).
func (m *mockManager) SetOwner(agent.OwnerScope) {}

func (m *mockManager) CleanupLaunch(ctx context.Context, handles []agent.ResourceHandle) error {
	m.cleanupLaunchCalls++
	m.lastCleanupLaunchHandles = handles
	return m.cleanupLaunchErr
}

func (m *mockManager) Reprovision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	return &api.ScionConfig{}, nil
}

func (m *mockManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.mu.Lock()
	m.startCalls++
	m.lastStartOpts = opts
	m.lastStartCtx = ctx
	startErr := m.startErr
	m.mu.Unlock()
	if startErr != nil {
		return nil, startErr
	}
	// Simulate a runtime creating launch resources: a checkpoint before
	// each create, then the creation hooks.
	m.mu.Lock()
	handles := append([]api.ResourceHandle(nil), m.createHandles...)
	beforeCreate := m.beforeCreate
	m.mu.Unlock()
	for i, h := range handles {
		if beforeCreate != nil {
			beforeCreate(i)
		}
		if opts.Checkpoint != nil {
			if err := opts.Checkpoint(ctx, "launching"); err != nil {
				return nil, err
			}
		}
		if opts.OnResourceCreated != nil {
			opts.OnResourceCreated(h)
		}
		if opts.ObserveResourceCreated != nil {
			opts.ObserveResourceCreated(h)
		}
	}
	m.mu.Lock()
	errAfter := m.startErrAfterCreate
	m.mu.Unlock()
	if errAfter != nil {
		return nil, errAfter
	}
	agent := &api.AgentInfo{
		ID:    "test-container-id",
		Name:  opts.Name,
		Phase: "running",
		RunID: opts.RunID, // as pkg/agent.Start labels the new entry
	}
	m.mu.Lock()
	m.agents = append(m.agents, *agent)
	m.mu.Unlock()
	return agent, nil
}

func (m *mockManager) Stop(ctx context.Context, agentID, projectPath, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopCalls++
	m.lastStopAgentID = agentID
	m.lastStopRunID = runID
	return m.stopErr
}

// StopTarget records the resolved entry the broker stops; it counts as a
// stop call, like Stop, so existing stop assertions hold either way.
func (m *mockManager) StopTarget(ctx context.Context, ref runtime.RunRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopCalls++
	m.lastStopAgentID = ref.ID
	m.lastStopRunID = ref.RunID
	return m.stopErr
}

func (m *mockManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastDeleteProjectPath = projectPath
	m.lastDeleteAgentID = agentID
	m.deleteCalls++
	return true, nil
}

func (m *mockManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastDeleteProjectPath = projectPath
	m.lastDeleteAgentID = agentName
	m.lastDeleteContainerID = ref.ID
	m.lastDeleteRunID = ref.RunID
	m.lastDeleteFiles = deleteFiles
	m.lastDeleteRemoveBranch = removeBranch
	m.deleteCalls++
	if m.deleteTargetErr != nil {
		return false, m.deleteTargetErr
	}
	return true, nil
}

func (m *mockManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastListFilter = filter
	if m.listErr != nil {
		return nil, m.listErr
	}
	// Honor a scion.name filter so a slug lookup against a multi-agent
	// fixture resolves to the one matching agent instead of an ambiguous
	// match. Real docker and k8s runtimes filter on the scion.name label
	// only; the Name match here stands in for that label on unlabelled
	// fixtures (such as newTestServer's), where real runtimes would have
	// filled Name from the label.
	if name, ok := filter["scion.name"]; ok {
		var out []api.AgentInfo
		for _, a := range m.agents {
			if a.Name == name || a.Labels["scion.name"] == name {
				out = append(out, a)
			}
		}
		return out, nil
	}
	return m.agents, nil
}

// --- Locked accessors for mockManager's mutable bookkeeping fields ---
//
// Every one of these guards the same mu the methods above lock, so a test
// reading, say, StartCalls() while a concurrent HeartbeatService goroutine
// is inside Start() always sees a consistent value instead of racing it.
// Methods, not exported fields, so they're promoted the same way onto
// filteringMockManager/scopedThenFailManager/countingListManager (which all
// embed mockManager). An embedding type that overrides List, or adds its
// own mutable counters, must lock mu itself.

func (m *mockManager) StartCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCalls
}

func (m *mockManager) LastStartOpts() api.StartOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStartOpts
}

func (m *mockManager) LastStartCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStartCtx
}

func (m *mockManager) StopCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopCalls
}

func (m *mockManager) LastStopAgentID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStopAgentID
}

func (m *mockManager) DeleteCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteCalls
}

// SetDeleteCalls resets the deleteCalls counter — tests use this between
// sub-cases that reuse the same mockManager instance — under the same lock
// every other mutation of this field uses.
func (m *mockManager) SetDeleteCalls(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = n
}

func (m *mockManager) LastDeleteProjectPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastDeleteProjectPath
}

func (m *mockManager) LastDeleteAgentID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastDeleteAgentID
}

func (m *mockManager) LastDeleteContainerID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastDeleteContainerID
}

func (m *mockManager) LastDeleteFiles() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastDeleteFiles
}

func (m *mockManager) LastListFilter() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastListFilter
}

func (m *mockManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	return m.messageErr
}

func (m *mockManager) SendKeys(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
	if m.sendKeysFunc != nil {
		return m.sendKeysFunc(ctx, projectID, agentSlug, expectedAgentID, keys)
	}
	return nil
}

func (m *mockManager) SendKeysLocal(ctx context.Context, projectPath, agentSlug, expectedAgentID, keys string) error {
	if m.sendKeysFunc != nil {
		return m.sendKeysFunc(ctx, projectPath, agentSlug, expectedAgentID, keys)
	}
	return nil
}

func (m *mockManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, nil
}

func (m *mockManager) Close() {}

// setupTestScionEnv isolates the test from the repo's own .scion directory by
// switching to a temp CWD with its own settings/templates/harness-configs, so
// buildStartContext (used by start/restart) can resolve a harness config
// without touching real project state.
func setupTestScionEnv(t *testing.T) {
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

	// Create templates with scion-agent.yaml so harness-config resolution
	// finds a harness_config value instead of falling through to the
	// embedded default ("gemini") which has no on-disk directory.
	for _, tpl := range []string{"default", "claude"} {
		tplDir := filepath.Join(dotScion, "templates", tpl)
		if err := os.MkdirAll(tplDir, 0755); err != nil {
			t.Fatal(err)
		}
		cfg := "harness_config: " + tpl + "\n"
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(cfg), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Create harness-config directories so FindHarnessConfigDir can resolve them.
	for _, hc := range []string{"default", "claude"} {
		hcDir := filepath.Join(dotScion, "harness-configs", hc)
		if err := os.MkdirAll(hcDir, 0755); err != nil {
			t.Fatal(err)
		}
		cfg := "harness: " + hc + "\nimage: test-image:" + hc + "\n"
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(cfg), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// asyncLaunchCleanupWait bounds how long a test server's cleanup waits for
// its async launches to end (endAsyncLaunchesAtCleanup).
const asyncLaunchCleanupWait = 10 * time.Second

// endAsyncLaunchesAtCleanup ends srv's async launches before the test's
// temp dirs are removed (ptone/scion#5142). A launch accepted by a test can
// still be running when the test body returns (for example after a
// deferred close of a blocked Start), and it writes its launch journal and
// markers under the test's HOME. If t.TempDir's RemoveAll runs at the same
// moment, it fails with "directory not empty". Registered after the
// caller's temp dirs, so it runs before their removal (cleanups run last
// registered, first). It drains srv as a shutdown would: a launch still
// running is cancelled and reports broker_restarted to the test hub (the
// test has already returned, so no assertion sees it), and the drain waits,
// bounded, for every accepted launch to finish (its done channel closes
// only after its last journal and marker write). Only accepted launches
// (the drain's set) are waited for, not every launch registry record: some
// tests register records directly that no launch ever finishes. It then
// closes the journal.
func endAsyncLaunchesAtCleanup(t *testing.T, srv *Server) {
	t.Helper()
	t.Cleanup(func() {
		srv.DrainLaunches(context.Background(), asyncLaunchCleanupWait)
		srv.launchDrain.mu.Lock()
		n := len(srv.launchDrain.active)
		srv.launchDrain.mu.Unlock()
		if n > 0 {
			t.Errorf("%d async launch(es) still running %v after the test; its temp dirs may not be removable", n, asyncLaunchCleanupWait)
		}
		if srv.launchJournal != nil {
			srv.launchJournal.Close()
		}
	})
}

// newTestServerWithManager wires up a Server with the given agent.Manager
// (e.g. a *filteringMockManager for tests that need List to honor the filter
// map) plus the same isolated .scion environment newTestServer uses, so
// restart's buildStartContext path resolves cleanly.
func newTestServerWithManager(t *testing.T, mgr agent.Manager) *Server {
	t.Helper()
	setupTestScionEnv(t)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"

	// NameFunc returns "mock" to match ForceRuntime so resolveManagerForOpts
	// returns the mock manager directly instead of creating a real one.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "mock" }}

	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	return srv
}

func newTestServer(t *testing.T) *Server {
	t.Helper()

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

	return newTestServerWithManager(t, mgr)
}

// envCapturingManager captures the environment variables passed to Start().
// Used for testing that Hub credentials are properly set. The embedded
// mockManager's own lastStartOpts captures the full options struct, for
// tests that need a field with no dedicated lastXxx accessor here (e.g.
// ResolvedKubernetesServiceAccountName).
type envCapturingManager struct {
	mockManager
	lastEnv           map[string]string
	lastRunID         string
	lastTemplateName  string
	lastHarnessConfig string
}

func (m *envCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.lastEnv = opts.Env
	m.lastRunID = opts.RunID
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

	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
}

// provisionCapturingManager tracks whether Provision, Reprovision or Start
// was called. Provision and Reprovision use DISTINCT flags (design §3.4
// Amendment A2.5): the whole fail-closed design rests on the broker calling
// exactly one of them per the request's Reprovision flag, and a shared flag
// cannot catch a regression where handleCreateAgent's branch is inverted or
// deleted.
type provisionCapturingManager struct {
	mockManager
	provisionCalled   bool
	reprovisionCalled bool
	startCalled       bool
	lastOpts          api.StartOptions
	reprovisionErr    error
	// lastProvisionCtx captures the context passed to Provision (#1960).
	lastProvisionCtx context.Context
	// lastReprovisionCtx captures the context passed to Reprovision.
	lastReprovisionCtx context.Context
	// onStart, when set, runs at the start of Start, so a test can observe
	// what the agent's start would read.
	onStart func(ctx context.Context, opts api.StartOptions)
}

func (m *provisionCapturingManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	m.provisionCalled = true
	m.lastOpts = opts
	m.lastProvisionCtx = ctx
	return &api.ScionConfig{Harness: "claude", HarnessConfig: "claude"}, nil
}

func (m *provisionCapturingManager) Preflight(ctx context.Context, opts api.StartOptions) error {
	return nil
}

func (m *provisionCapturingManager) CleanupLaunch(ctx context.Context, handles []agent.ResourceHandle) error {
	return nil
}

func (m *provisionCapturingManager) Reprovision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	m.reprovisionCalled = true
	m.lastReprovisionCtx = ctx
	m.lastOpts = opts
	if m.reprovisionErr != nil {
		return nil, m.reprovisionErr
	}
	return &api.ScionConfig{Harness: "claude", HarnessConfig: "claude"}, nil
}

func (m *provisionCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.startCalled = true
	m.lastOpts = opts
	if m.onStart != nil {
		m.onStart(ctx, opts)
	}
	return m.mockManager.Start(ctx, opts)
}

func newTestServerWithProvisionCapture() (*Server, *provisionCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
}
