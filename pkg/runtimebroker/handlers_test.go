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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	mu                    sync.Mutex
	agents                []api.AgentInfo
	startCalls            int
	stopCalls             int
	deleteCalls           int
	startErr              error
	provisionErr          error
	stopErr               error
	listErr               error
	deleteTargetErr       error
	messageErr            error
	lastStartOpts         api.StartOptions
	lastDeleteProjectPath string
	lastDeleteAgentID     string
	lastDeleteContainerID string
	lastDeleteFiles       bool
	lastStopAgentID       string
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
	agent := &api.AgentInfo{
		ID:    "test-container-id",
		Name:  opts.Name,
		Phase: "running",
	}
	m.mu.Lock()
	m.agents = append(m.agents, *agent)
	m.mu.Unlock()
	return agent, nil
}

func (m *mockManager) Stop(ctx context.Context, agentID string, projectPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopCalls++
	m.lastStopAgentID = agentID
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

func (m *mockManager) DeleteTarget(ctx context.Context, agentName, containerID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastDeleteProjectPath = projectPath
	m.lastDeleteAgentID = agentName
	m.lastDeleteContainerID = containerID
	m.lastDeleteFiles = deleteFiles
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

func (m *mockManager) MessageRaw(ctx context.Context, agentID, projectID string, keys string) error {
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

	return New(cfg, mgr, rt)
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
		t.Fatal("expected capabilities to be present")
	}
	if !resp.Capabilities.EmptyPerAgentWorkspace {
		t.Error("expected capabilities.emptyPerAgentWorkspace to be true (design #2703 P2)")
	}
}

// TestHostInfo_EmptyPerAgentFollowsDefaultRuntime pins that /api/v1/info
// advertises EmptyPerAgentWorkspace per default runtime (false for one that
// opts out, as Cloud Run does), matching the heartbeat.
func TestHostInfo_EmptyPerAgentFollowsDefaultRuntime(t *testing.T) {
	srv := newTestServer(t)
	srv.runtime = &noEmptyPerAgentTestRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	var resp BrokerInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Capabilities == nil {
		t.Fatal("expected capabilities to be present")
	}
	if resp.Capabilities.EmptyPerAgentWorkspace {
		t.Error("capabilities.emptyPerAgentWorkspace = true, want false for a default runtime that opts out")
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

// TestListAgents_GroveIDQueryParamNotHonoured verifies that a groveId-only
// query no longer scopes the agent list: the filter built for the manager
// must carry no scion.project_id key, so the list falls back to unscoped
// (matching every agent) rather than silently re-honouring the legacy alias.
func TestListAgents_GroveIDQueryParamNotHonoured(t *testing.T) {
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{ID: "container-1", Name: "test-agent-1", Phase: "running"},
		},
	}
	srv := newTestServerWithManager(t, mgr)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents?groveId=p1", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	if v, ok := mgr.LastListFilter()["scion.project_id"]; ok {
		t.Errorf("groveId-only query must not scope the list filter, got scion.project_id=%q", v)
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

// TestCreateAgentRejectsMultiSegmentName is the regression anchor for the
// isSingleCleanPathElement check added to createAgent. req.Name is joined
// onto a directory as a single path segment all the way down to
// GetAgentDir, so a name that isn't exactly one clean segment must be
// rejected here rather than reaching that join. GetAgentDir/GetAgent (in
// pkg/agent) enforce the same constraint independently before their own
// stale-directory removal branch; this covers the same shape earlier, at
// the request boundary, for every name this check can see regardless of
// what a caller intended.
//
// "a\\b" covers backslash specifically: isSingleCleanPathElement must
// reject it on every platform, not only when os.PathSeparator is '\' (i.e.
// on Windows). '\' is a path separator on Windows and a general
// path-injection vector everywhere else, so it must be rejected regardless
// of GOOS.
func TestCreateAgentRejectsMultiSegmentName(t *testing.T) {
	srv := newTestServer(t)

	for _, name := range []string{"../sibling", "a/../..", "..", ".", "a/b", "/etc", `a\b`, "a\x00b"} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(CreateAgentRequest{Name: name})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("name %q: expected status %d, got %d: %s", name, http.StatusBadRequest, w.Code, w.Body.String())
			}
		})
	}
}

// TestCreateAgentRejectsInvalidProjectSlugBeforeGCSBootstrap is the
// regression test for the GCS-bootstrap workspace directory being built
// from an unvalidated ProjectSlug: a slug of ".." joined onto
// ~/.scion/projects resolves to ~/.scion itself, which must never be
// created or synced into. Asserts the request is rejected and that nothing
// is created under the global directory at all.
func TestCreateAgentRejectsInvalidProjectSlugBeforeGCSBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)

	for _, slug := range []string{"..", ".", "a/../..", "/etc", `a\b`} {
		t.Run(slug, func(t *testing.T) {
			body, err := json.Marshal(CreateAgentRequest{
				Name:                 "agent1",
				ProjectSlug:          slug,
				WorkspaceStoragePath: "workspaces/project-123/project-workspace",
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("slug %q: expected status %d, got %d: %s", slug, http.StatusBadRequest, w.Code, w.Body.String())
			}

			globalDir, err := config.GetGlobalDir()
			if err != nil {
				t.Fatalf("GetGlobalDir: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(globalDir, "projects")); !os.IsNotExist(statErr) {
				t.Errorf("slug %q: expected no projects directory to be created, stat returned: %v", slug, statErr)
			}
		})
	}
}

// TestCreateAgentRejectsWorkspaceDirOutsideRootViaSymlink is a second,
// independent regression test for the same GCS-bootstrap validation gate as
// TestCreateAgentRejectsInvalidProjectSlugBeforeGCSBootstrap, but one that
// the single-path-element check on ProjectSlug cannot catch: an ordinary,
// single-component slug whose directory entry under ~/.scion/projects is
// itself a symlink resolving outside workspaceRoot. Every example slug in
// the sibling test above ("..", ".", "a/../..", "/etc", `a\b`) is already
// refused by isSingleCleanPathElement before the handler ever reaches
// runtime.ValidateWorkspaceSource, so neither test alone proves that second,
// independent gate actually runs. This one reaches it with a slug that
// passes the first check cleanly.
func TestCreateAgentRejectsWorkspaceDirOutsideRootViaSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("GetGlobalDir: %v", err)
	}
	projectsDir := filepath.Join(globalDir, "projects")
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		t.Fatalf("MkdirAll(projectsDir): %v", err)
	}

	// outsideTarget is outside ~/.scion entirely, so a workspace dir that
	// resolves into it must be refused regardless of the slug's own
	// spelling.
	outsideTarget := t.TempDir()
	const slug = "outside-slug"
	if err := os.Symlink(outsideTarget, filepath.Join(projectsDir, slug)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	body, err := json.Marshal(CreateAgentRequest{
		Name:                 "agent1",
		ProjectSlug:          slug,
		WorkspaceStoragePath: "workspaces/project-123/project-workspace",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}

	// The outside target must not have been written into: a 500 from a later
	// stage (storage bucket not configured, GCS sync failure, and so on)
	// would also leave it untouched, so status code alone does not prove
	// the validation gate is what stopped this request before MkdirAll or
	// SyncFromGCS ran.
	entries, err := os.ReadDir(outsideTarget)
	if err != nil {
		t.Fatalf("ReadDir(outsideTarget): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected outside target to remain empty, found: %v", entries)
	}
}

// TestCreateAgentFullStart_HarnessConfigNotFound proves the fix for
// ptone/scion#1316 fault 3: when Start fails because a configured
// harness-config name does not resolve anywhere the broker looked, the
// broker must report a 404 naming the resource instead of the blanket 502
// every other provisioning failure gets.
func TestCreateAgentFullStart_HarnessConfigNotFound(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("failed to find harness-config %q: %w", "antigravity", config.ErrHarnessConfigNotFound)

	body := `{"name": "new-agent", "config": {"template": "claude", "harness": "antigravity"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "antigravity") {
		t.Errorf("expected response to name the unresolved resource, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_OtherErrorStaysRuntimeError proves that a
// provisioning failure unrelated to naming (e.g. a runtime/infra failure)
// still gets the generic 502-mapped RuntimeError, not a 404 — the
// classification in dispatchCreateErrorResponse and its broker-side
// counterpart must be narrow.
func TestCreateAgentFullStart_OtherErrorStaysRuntimeError(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = fmt.Errorf("docker daemon unreachable")

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
}

// TestCreateAgentProvisionOnly_TemplateNotFound is the ProvisionOnly-path
// counterpart of TestCreateAgentFullStart_HarnessConfigNotFound, covering the
// other named resource (template) and the other dispatch branch (Provision).
func TestCreateAgentProvisionOnly_TemplateNotFound(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.provisionErr = fmt.Errorf("failed to load template: %w",
		fmt.Errorf("template %s not found: %w", "missing-template", config.ErrTemplateNotFound))

	body := `{"name": "new-agent", "provisionOnly": true, "config": {"template": "missing-template"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing-template") {
		t.Errorf("expected response to name the unresolved resource, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_SkillResolutionRateLimited proves that a required
// skill reference failing to resolve because of GitHub rate limiting
// surfaces as a 429, naming the ref and the cause, instead of the generic
// 500 the "other error" branch maps to (#2546).
func TestCreateAgentFullStart_SkillResolutionRateLimited(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/my-skill@main",
		Code:    agent.SkillErrCodeRateLimited,
		Message: "GitHub API request to /repos/example-org/example-skills/commits/main rate limited",
	}

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status %d, got %d: %s", http.StatusTooManyRequests, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/my-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate_limited") {
		t.Errorf("expected response to name the cause, got: %s", w.Body.String())
	}
}

// TestCreateAgentProvisionOnly_SkillResolutionNotFound is the ProvisionOnly
// counterpart, covering the not-found cause mapped to 404.
func TestCreateAgentProvisionOnly_SkillResolutionNotFound(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.provisionErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/missing-skill@main",
		Code:    agent.SkillErrCodeNotFound,
		Message: `skill "missing-skill" not found in repo example-org/example-skills at ref main`,
	}

	body := `{"name": "new-agent", "provisionOnly": true, "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/missing-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not_found") {
		t.Errorf("expected response to name the cause, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_SkillResolutionUpstreamUnavailableBecomes502
// proves that GitHub itself failing (5xx after retries exhausted) surfaces as
// 502, the one 5xx code in the mapping that still names the ref and the
// cause, instead of either the pre-fix 500 or the briefly-considered 400
// default (#2546 R3, O1).
func TestCreateAgentFullStart_SkillResolutionUpstreamUnavailableBecomes502(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/my-skill@main",
		Code:    agent.SkillErrCodeUpstreamUnavailable,
		Message: "GitHub API error (503) while resolving commit, retries exhausted",
	}

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadGateway, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/my-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream_unavailable") {
		t.Errorf("expected response to name the cause, got: %s", w.Body.String())
	}
}

// TestCreateAgentFullStart_SkillResolutionDefaultCodeStaysInternalError
// proves that an uncategorized SkillResolutionError.Code (e.g. a Hub-side
// PreResolvedSkills code this broker version does not recognize) stays on
// the existing 500 path, matching the "any other error" branch, rather than
// being guessed at as a 4xx (#2546 R3).
func TestCreateAgentFullStart_SkillResolutionDefaultCodeStaysInternalError(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	mgr.startErr = &agent.SkillResolutionError{
		URI:     "gh://example-org/example-skills/my-skill@main",
		Code:    "storage_error",
		Message: "skill my-skill version abc123 has storage files missing",
	}

	body := `{"name": "new-agent", "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gh://example-org/example-skills/my-skill@main") {
		t.Errorf("expected response to name the unresolved skill ref, got: %s", w.Body.String())
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
	if mgr.StopCalls() != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.StartCalls())
	}
	if mgr.LastStartOpts().Name != "test-agent-1" {
		t.Fatalf("expected restart to start agent 'test-agent-1', got %q", mgr.LastStartOpts().Name)
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
	if mgr.StopCalls() != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.StartCalls())
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
	if mgr.StopCalls() != 1 {
		t.Fatalf("expected Stop to be called once, got %d", mgr.StopCalls())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.StartCalls())
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
	if !mgr.LastStartOpts().BrokerMode {
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

func TestAgentLogsReadsFileWhenSlugEmpty(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

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
	for _, tmpl := range []string{"default", "claude"} {
		if err := os.MkdirAll(filepath.Join(dotScion, "templates", tmpl), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Create agent.log at the path derived from Name (not Slug)
	agentHome := filepath.Join(dotScion, "agents", "my-agent", "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	logContent := "hello from agent.log"
	if err := os.WriteFile(filepath.Join(agentHome, "agent.log"), []byte(logContent), 0644); err != nil {
		t.Fatal(err)
	}

	getLogsCalled := false
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		GetLogsFunc: func(_ context.Context, _ string) (string, error) {
			getLogsCalled = true
			return "", fmt.Errorf("should not be called")
		},
	}

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:          "container-abc",
				Name:        "my-agent",
				Slug:        "",       // empty slug — handler must fall back to Name
				ProjectPath: dotScion, // matches production: ProjectPath is the resolved .scion directory
				Phase:       "running",
			},
		},
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, mgr, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/my-agent/logs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != logContent {
		t.Fatalf("expected body %q, got %q", logContent, body)
	}
	if getLogsCalled {
		t.Fatal("runtime.GetLogs should not have been called when agent.log is readable")
	}
}

func TestAgentLogsFallbackUsesContainerID(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

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
	for _, tmpl := range []string{"default", "claude"} {
		if err := os.MkdirAll(filepath.Join(dotScion, "templates", tmpl), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// No agent.log on disk — forces fallback to container logs
	var receivedID string
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		GetLogsFunc: func(_ context.Context, id string) (string, error) {
			receivedID = id
			return "container log output", nil
		},
	}

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:          "myproject--foo", // project-prefixed container name
				ContainerID: "myproject--foo",
				Name:        "foo",
				Slug:        "",
				Phase:       "running",
			},
		},
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, mgr, rt)

	// Request uses the slug "foo", not the full container ID
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/foo/logs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if receivedID != "myproject--foo" {
		t.Fatalf("expected GetLogs to receive container ID %q, got %q", "myproject--foo", receivedID)
	}
	if body := w.Body.String(); body != "container log output" {
		t.Fatalf("expected body %q, got %q", "container log output", body)
	}
}

// TestAgentLogsRuntimeNotSupported verifies that a runtime declining logs
// outright (runtime.ErrLogsNotSupported — a runtime's contract) maps to an
// explicit 501 with the runtime_logs_unsupported code and the
// sentinel's own fixed message body, even when the runtime wraps the
// sentinel with extra identifying text: errors.Is matches through a wrap,
// so the response must use the sentinel's own Error() text, never the
// wrapped error's, or a wrapper could leak an id through this path.
func TestAgentLogsRuntimeNotSupported(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

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
	for _, tmpl := range []string{"default", "claude"} {
		if err := os.MkdirAll(filepath.Join(dotScion, "templates", tmpl), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// No agent.log on disk — forces fallback to the runtime's GetLogs, the
	// logs-declined path this fixture stands in for. The wrap carries text
	// that must never reach the response body.
	const wrappedIdentifier = "scion-hostile-namespace/leaked-actor-name"
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "noattach" },
		GetLogsFunc: func(_ context.Context, _ string) (string, error) {
			return "", fmt.Errorf("noattach: get actor %s: %w", wrappedIdentifier, runtime.ErrLogsNotSupported)
		},
	}

	mgr := &mockManager{
		agents: []api.AgentInfo{
			{
				ID:    "noattach-actor",
				Name:  "noattach-actor",
				Slug:  "",
				Phase: "running",
			},
		},
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	srv := New(cfg, mgr, rt)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/noattach-actor/logs", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusNotImplemented, w.Code, w.Body.String())
	}

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v; body: %s", err, w.Body.String())
	}
	if resp.Error.Code != ErrCodeRuntimeLogsUnsupported {
		t.Errorf("expected code %q, got %q", ErrCodeRuntimeLogsUnsupported, resp.Error.Code)
	}
	if resp.Error.Message != runtime.ErrLogsNotSupported.Error() {
		t.Errorf("expected message %q, got %q", runtime.ErrLogsNotSupported.Error(), resp.Error.Message)
	}

	body := w.Body.String()
	if strings.Contains(body, "noattach-actor") {
		t.Errorf("response body leaks the agent/actor name: %s", body)
	}
	if strings.Contains(body, wrappedIdentifier) {
		t.Errorf("response body leaks the wrapped identifier: %s", body)
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

	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt), mgr
}

// TestCreateAgentWithHubCredentials tests that Hub authentication env vars are passed to agent.
// This verifies the fix from progress-report.md: RuntimeBroker sets SCION_HUB_URL, SCION_AUTH_TOKEN, SCION_AGENT_ID.
func TestCreateAgentWithHubCredentials(t *testing.T) {
	srv, mgr := newTestServerWithEnvCapture()

	body := `{
		"name": "test-agent",
		"id": "agent-uuid-123",
		"projectId": "project-uuid-456",
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

	// Check SCION_PROJECT_ID
	if got := mgr.lastEnv["SCION_PROJECT_ID"]; got != "project-uuid-456" {
		t.Errorf("expected SCION_PROJECT_ID='project-uuid-456', got %q", got)
	}
	if _, ok := mgr.lastEnv["SCION_GROVE_ID"]; ok {
		t.Errorf("expected SCION_GROVE_ID to be absent, got %q", mgr.lastEnv["SCION_GROVE_ID"])
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

// TestCreateAgentProvisionOnly_Reprovision_CallsReprovisionNotProvision is
// the design §3.4 Amendment A2.5 regression test: a
// provisionOnly+reprovision request must call Manager.Reprovision, never
// Manager.Provision, and the response must echo reprovisioned:true — the
// echo the hub's whole fail-closed dispatch design rests on
// (design §3.4 Amendment A2.2(a)).
func TestCreateAgentProvisionOnly_Reprovision_CallsReprovisionNotProvision(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "reprovisioned-agent",
		"id": "agent-uuid-reprov",
		"slug": "reprovisioned-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Reprovision to be called")
	}
	if mgr.provisionCalled {
		t.Error("expected Provision NOT to be called when reprovision=true")
	}
	if mgr.startCalled {
		t.Error("expected Start NOT to be called for provision-only")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Reprovisioned {
		t.Error("expected Reprovisioned=true in the response")
	}
}

// TestCreateAgentProvisionOnly_SetsFreshProvision proves a plain
// provisionOnly create (no reprovision) sets opts.FreshProvision, so
// Manager.Provision -> GetAgent clears a leftover populated workspace from a
// same-named agent, extending GoogleCloudPlatform/scion#1931's create-only
// wipe gate to the ProvisionOnly path.
func TestCreateAgentProvisionOnly_SetsFreshProvision(t *testing.T) {
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
	if !mgr.lastOpts.FreshProvision {
		t.Error("expected opts.FreshProvision to be true for a plain provisionOnly create")
	}
}

// TestCreateAgentProvisionOnly_Reprovision_NeverSetsFreshProvision proves a
// reprovision (reincarnation) create never sets opts.FreshProvision, even
// though it is still an opCreate dispatch: reincarnation targets an existing
// agent's workspace, and FreshProvision would let GetAgent wipe it.
func TestCreateAgentProvisionOnly_Reprovision_NeverSetsFreshProvision(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "reprovisioned-agent",
		"id": "agent-uuid-reprov",
		"slug": "reprovisioned-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if mgr.lastOpts.FreshProvision {
		t.Error("expected opts.FreshProvision to be false for a reprovision create")
	}
}

// TestCreateAgentProvisionOnly_PlainProvision_DoesNotEchoReprovisioned is the
// reverse of the above: a plain provisionOnly request (no reprovision) must
// call Manager.Provision, never Manager.Reprovision, and must NOT echo
// reprovisioned:true — a broker that always echoed true regardless of which
// branch ran would defeat the hub's mandatory-echo fail-closed check.
func TestCreateAgentProvisionOnly_PlainProvision_DoesNotEchoReprovisioned(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "plain-provisioned-agent",
		"id": "agent-uuid-plain",
		"slug": "plain-provisioned-agent",
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
		t.Error("expected Provision to be called")
	}
	if mgr.reprovisionCalled {
		t.Error("expected Reprovision NOT to be called when reprovision is unset")
	}

	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Reprovisioned {
		t.Error("expected Reprovisioned to be false/absent for a plain provision")
	}
}

// TestCreateAgentProvisionOnly_ReprovisionError_ReturnsErrorNoEcho covers a
// plain (unwrapped, not agent.ErrReprovisionRefused) Reprovision failure: the
// handler must return the generic 500 — not the 409 the sentinel-wrapped
// path gets — and must not echo reprovisioned:true for a request that never
// actually succeeded. The body must carry the fixed, identity-free
// "Failed to provision agent" message, never Reprovision's own raw error
// text, which could carry a runtime-specific detail this response must not
// disclose.
func TestCreateAgentProvisionOnly_ReprovisionError_ReturnsErrorNoEcho(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	mgr.reprovisionErr = errors.New("boom: transient broker failure")

	body := `{
		"name": "reprovision-fail-agent",
		"id": "agent-uuid-reprov-fail",
		"slug": "reprovision-fail-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a plain (non-refusal) Reprovision error, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Reprovision to have been attempted")
	}
	if strings.Contains(w.Body.String(), "boom: transient broker failure") {
		t.Errorf("the error body must never surface Reprovision's own raw error text, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to provision agent") {
		t.Errorf("expected the fixed, identity-free provision-failure message, got: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"reprovisioned":true`) {
		t.Errorf("a failed Reprovision must never echo reprovisioned:true, got: %s", w.Body.String())
	}
}

// TestCreateAgentProvisionOnly_ReprovisionRefused_Returns409 covers design
// §3.4 Amendment A4.2: a Reprovision failure that wraps agent.ErrReprovisionRefused
// (workspace preconditions, running-container check) must surface as 409
// Conflict specifically, not a generic 500 — so the reincarnate worker's
// failure message and any future caller-side retry logic can tell "refused
// to run" apart from an actual provisioning error.
func TestCreateAgentProvisionOnly_ReprovisionRefused_Returns409(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	mgr.reprovisionErr = fmt.Errorf("%w: agent %q container is still running; stop it first", agent.ErrReprovisionRefused, "reprovision-409-agent")

	body := `{
		"name": "reprovision-409-agent",
		"id": "agent-uuid-reprov-409",
		"slug": "reprovision-409-agent",
		"provisionOnly": true,
		"reprovision": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for a refused reprovision, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Reprovision to have been attempted")
	}
	if !strings.Contains(w.Body.String(), "reprovision refused") {
		t.Errorf("expected the error body to surface the refusal reason, got: %s", w.Body.String())
	}
}

// snapshotFileTimes walks dir and records the ModTime of every regular file
// found under it, keyed by path relative to dir. Used to prove that NO
// write happened anywhere under a directory tree across an operation —
// including a content-identical rewrite (e.g. RegisterSharer's atomic
// temp-file-plus-rename always changes ModTime, even when the marshaled
// JSON is byte-identical to what was already there), which a before/after
// content comparison alone would miss.
func snapshotFileTimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		out[rel] = info.ModTime()
		return nil
	})
	return out
}

// TestCreateAgentProvisionOnly_Reprovision_WorktreePerAgent_Returns409 is the
// design §3.4 Amendment A23.1/A23.2 (review p1b-r1 R2, p1b-r2 R2-T)
// regression test: a reprovision request for a worktree-per-agent project
// must be refused with 409 before buildStartContext ever runs —
// buildStartContext's tryProvisionWorktree finds or creates the agent's
// worktree and hands Manager.Reprovision an explicit-mount-shaped
// StartOptions (GitClone=nil, Workspace=<worktree path>), a mode reincarnate
// has neither designed for nor reviewed, and whose OWN failure path can
// `git worktree remove --force` / os.RemoveAll the agent's LIVE worktree
// (round-1 review FYI-2) if ProvisionShared ever errors on it.
//
// The fixture is realistic, not a stand-in: a real git-backed project
// (config.gitClone set, matching what populateAgentConfig always sends for a
// worktree-per-agent agent), a real ProjectPath, and the agent's own base
// clone + worktree ALREADY provisioned on this broker — exactly the state a
// second reprovision of a running agent finds. Manager.Reprovision/Provision
// must never be called, the pre-seeded sentinel file in the agent's worktree
// must survive untouched, and — the property a placement-blind assertion on
// the manager mock alone cannot prove — NOTHING under the base clone
// (including worktrees/ and the .git/ sharer registry) may be written to at
// all: buildStartContext, and therefore tryProvisionWorktree, must never
// run. A refusal placed anywhere AFTER buildStartContext (e.g. immediately
// before the Manager.Reprovision call) would let tryProvisionWorktree run
// its (safe, in this fixture) JOIN path, which still rewrites the sharer
// registry file and would be caught by the whole-tree ModTime snapshot even
// though Manager.Reprovision itself is never reached in that JOIN case
// either.
func TestCreateAgentProvisionOnly_Reprovision_WorktreePerAgent_Returns409(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	srv, mgr := newTestServerWithProvisionCapture()

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}
	agentID := "worktree-reprov-agent"

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-seed the agent's own base clone + worktree, as an earlier
	// successful provision on this broker would have left them.
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: agentID,
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: agentID, AgentName: agentID, GitClone: gc,
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	base := resolved.HostPath
	worktreePath := provision.WorktreePath(base, agentID)
	sentinelPath := filepath.Join(worktreePath, "uncommitted-work.txt")
	if err := os.WriteFile(sentinelPath, []byte("hours of agent work"), 0644); err != nil {
		t.Fatal(err)
	}

	beforeTimes := snapshotFileTimes(t, base)

	reqBody, err := json.Marshal(CreateAgentRequest{
		Name:          agentID,
		ID:            "agent-uuid-worktree-reprov",
		Slug:          agentID,
		ProjectID:     "p1",
		ProjectPath:   projectPath,
		ProvisionOnly: true,
		Reprovision:   true,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config: &CreateAgentConfig{
			Template: "claude",
			GitClone: gc,
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for a worktree-per-agent reprovision, got %d: %s", w.Code, w.Body.String())
	}
	if mgr.reprovisionCalled {
		t.Error("expected Manager.Reprovision NOT to be called: the refusal must happen before buildStartContext, which is what would otherwise call it")
	}
	if mgr.provisionCalled {
		t.Error("expected Manager.Provision NOT to be called either")
	}
	if !strings.Contains(w.Body.String(), "worktree-per-agent") {
		t.Errorf("expected the error body to name the worktree-per-agent refusal, got: %s", w.Body.String())
	}

	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatalf("sentinel file in the agent's live worktree was lost: %v", err)
	}
	if string(data) != "hours of agent work" {
		t.Fatalf("sentinel content changed: %q", data)
	}

	afterTimes := snapshotFileTimes(t, base)
	if !reflect.DeepEqual(beforeTimes, afterTimes) {
		t.Fatalf("base clone / worktree tree was written to -- buildStartContext (and therefore tryProvisionWorktree) must never run before the refusal:\nbefore=%v\nafter=%v", beforeTimes, afterTimes)
	}
}

// TestCreateAgentProvisionOnly_Reprovision_SharedWorkspace_StillCallsReprovision
// is the companion negative control for the R2 fix above: a reprovision
// request for a shared-workspace (not worktree-per-agent) project must still
// reach Manager.Reprovision as normal — the new gate must not over-refuse
// every WorkspaceMode.
func TestCreateAgentProvisionOnly_Reprovision_SharedWorkspace_StillCallsReprovision(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "shared-reprov-agent",
		"id": "agent-uuid-shared-reprov",
		"slug": "shared-reprov-agent",
		"provisionOnly": true,
		"reprovision": true,
		"workspaceMode": "shared",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a shared-workspace reprovision, got %d: %s", w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled {
		t.Error("expected Manager.Reprovision to be called for a shared-workspace reprovision")
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

// TestCreateAgentHubEndpointFromProjectSettings tests that hub endpoint is resolved
// from the project's settings.yaml when projectPath is provided.
func TestCreateAgentHubEndpointFromProjectSettings(t *testing.T) {
	t.Run("request hub endpoint takes priority over project settings", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a project directory with settings.yaml containing hub.endpoint
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "project-endpoint-agent",
			"hubEndpoint": "http://localhost:9810",
			"projectPath": "` + projectDir + `",
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

		// Request hub endpoint takes priority over project settings (project settings
		// are only a fallback when no endpoint is provided by dispatch/broker).
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from request, got %q", got)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from request, got %q", got)
		}
	})

	t.Run("project settings used when request hub endpoint empty", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  endpoint: "https://hub.example.com"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "project-fallback-agent",
			"projectPath": "` + projectDir + `",
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
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com' from project settings, got %q", got)
		}
	})

	t.Run("no project path falls back to request endpoint", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		body := `{
			"name": "no-project-agent",
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

// TestCreateAgentProjectHubEndpointSuppressedWhenDisabled tests that project endpoint
// is suppressed when hub.enabled=false, while dispatcher-provided endpoint still works.
func TestCreateAgentProjectHubEndpointSuppressedWhenDisabled(t *testing.T) {
	t.Run("project hub endpoint suppressed when hub disabled", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a project directory with hub.enabled=false but endpoint configured
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  enabled: false
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		body := `{
			"name": "project-disabled-agent",
			"projectPath": "` + projectDir + `",
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

		// Project endpoint should NOT be used when hub.enabled=false
		if _, exists := mgr.lastEnv["SCION_HUB_ENDPOINT"]; exists {
			t.Error("expected SCION_HUB_ENDPOINT to NOT be set when project has hub.enabled=false")
		}
		if _, exists := mgr.lastEnv["SCION_HUB_URL"]; exists {
			t.Error("expected SCION_HUB_URL to NOT be set when project has hub.enabled=false")
		}
	})

	t.Run("dispatcher endpoint still works when project hub disabled", func(t *testing.T) {
		srv, mgr := newTestServerWithEnvCapture()

		// Create a project directory with hub.enabled=false
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := `hub:
  enabled: false
  endpoint: "https://scionhub.loophole.site"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		// Dispatcher provides its own hub endpoint (authoritative in hosted mode)
		body := `{
			"name": "dispatcher-endpoint-agent",
			"hubEndpoint": "https://hub.authoritative.com",
			"projectPath": "` + projectDir + `",
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

// TestCreateAgentHubManagedProjectSettingsEndpoint tests that createAgent with a
// hub-managed project (ProjectSlug set, no ProjectPath) correctly resolves the project
// path and uses project settings hub.endpoint from the .scion subdirectory.
func TestCreateAgentHubManagedProjectSettingsEndpoint(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "http://localhost:9810" // broker's default (combo mode)
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Set up a hub-managed project directory at the expected path.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}
	projectPath := filepath.Join(globalDir, "projects", "settings-test-project")
	scionDir := filepath.Join(projectPath, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create .scion dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(projectPath) })

	// Place settings.yaml in the .scion subdirectory (hub-managed project layout)
	settingsContent := "hub:\n  endpoint: https://hub.external.example.com\n"
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings.yaml: %v", err)
	}

	// Send createAgent request with projectSlug but no projectPath
	body := `{
		"name": "hub-managed-agent",
		"projectSlug": "settings-test-project",
		"hubEndpoint": "http://localhost:9810",
		"config": {"template": "claude", "workspace": "` + projectPath + `"}
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

	// Request hub endpoint takes priority over project settings (project settings
	// are only a fallback when no endpoint is provided by dispatch/broker).
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from request, got %q", got)
	}
	if got := mgr.lastEnv["SCION_HUB_URL"]; got != "http://localhost:9810" {
		t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from request, got %q", got)
	}
}

// TestResolveProjectSettingsDir tests the helper function that resolves the
// settings directory for both linked and hub-managed projects.
func TestResolveProjectSettingsDir(t *testing.T) {
	t.Run("linked project - settings at projectPath directly", func(t *testing.T) {
		// Linked project: projectPath = /path/to/project/.scion, settings.yaml is there
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: https://example.com\n"), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		result := resolveProjectSettingsDir(projectDir)
		if result != projectDir {
			t.Errorf("expected %q, got %q", projectDir, result)
		}
	})

	t.Run("hub-managed project - settings in .scion subdirectory", func(t *testing.T) {
		// Hub-managed project: projectPath = ~/.scion/projects/<slug>, settings in .scion/
		projectDir := t.TempDir()
		scionDir := filepath.Join(projectDir, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("failed to create .scion dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("hub:\n  endpoint: https://example.com\n"), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		result := resolveProjectSettingsDir(projectDir)
		if result != scionDir {
			t.Errorf("expected %q (with .scion), got %q", scionDir, result)
		}
	})

	t.Run("no settings file - returns original path", func(t *testing.T) {
		projectDir := t.TempDir()
		result := resolveProjectSettingsDir(projectDir)
		if result != projectDir {
			t.Errorf("expected %q (original path), got %q", projectDir, result)
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
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
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

	t.Run("container endpoint overrides localhost even with project settings", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.Debug = true
		cfg.ContainerHubEndpoint = "http://host.containers.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt)

		// Create a project directory with settings.yaml containing hub.endpoint
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatal(err)
		}
		settingsContent := `schema_version: "1"
hub:
  enabled: true
  endpoint: "https://tunnel.example.com"
`
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatal(err)
		}

		body := fmt.Sprintf(`{
			"name": "test-agent",
			"hubEndpoint": "http://localhost:8080",
			"projectPath": %q
		}`, projectDir)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}

		// ContainerHubEndpoint override applies last to localhost endpoints;
		// project settings are only a fallback when no dispatch/broker endpoint exists.
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
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
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

		// Create a project dir with kubernetes settings so resolveManagerForOpts
		// matches the "kubernetes" runtime without trying to create a real manager.
		projectDir := filepath.Join(t.TempDir(), ".scion")
		_ = os.MkdirAll(projectDir, 0755)
		k8sSettings := `schema_version: "1"
profiles:
  local:
    runtime: kubernetes
runtimes:
  kubernetes:
    type: kubernetes
`
		_ = os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(k8sSettings), 0644)

		body := fmt.Sprintf(`{
			"name": "test-agent",
			"hubEndpoint": "http://localhost:8080",
			"projectPath": %q
		}`, projectDir)
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

// TestGCPIdentityBlockRejectedOnKubernetes_HTTPStatus pins that the
// Kubernetes/"block" rejection (ptone/scion#2328) reaches the HTTP caller as
// the 400 startContextError.Status actually names, on all three dispatch
// endpoints — not the generic 500 runtime_error every buildStartContext
// error used to collapse into before writeStartContextError (errors.go)
// started honoring Status.
func TestGCPIdentityBlockRejectedOnKubernetes_HTTPStatus(t *testing.T) {
	newKubernetesServer := func(t *testing.T) *Server {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		origWd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origWd) })
		dotScion := filepath.Join(tmpDir, ".scion")
		if err := os.Mkdir(dotScion, 0755); err != nil {
			t.Fatal(err)
		}
		settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes:
        type: kubernetes
`
		if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.ForceRuntime = "kubernetes"
		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		return New(cfg, mgr, rt)
	}

	assertRejected := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
		}
		var resp ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode error response: %v (body: %s)", err, w.Body.String())
		}
		if resp.Error.Code != ErrCodeValidationError {
			t.Errorf("expected error code %q, got %q", ErrCodeValidationError, resp.Error.Code)
		}
		if !strings.Contains(resp.Error.Message, "Kubernetes") {
			t.Errorf("expected the error message to name the Kubernetes runtime, got %q", resp.Error.Message)
		}
	}

	t.Run("create", func(t *testing.T) {
		srv := newKubernetesServer(t)
		body := `{"name": "gcp-block-k8s-agent", "config": {"gcpIdentity": {"metadata_mode": "block"}}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assertRejected(t, w)
	})

	t.Run("start", func(t *testing.T) {
		srv := newKubernetesServer(t)
		body := `{"resolvedEnv": {"SCION_METADATA_MODE": "block"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/gcp-block-k8s-start/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assertRejected(t, w)
	})

	t.Run("restart", func(t *testing.T) {
		srv := newKubernetesServer(t)
		body := `{"resolvedEnv": {"SCION_METADATA_MODE": "block"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/gcp-block-k8s-restart/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		assertRejected(t, w)
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
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
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
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
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
	lastEnv            map[string]string
	lastGitClone       *api.GitCloneConfig
	lastWorkspace      string
	lastProjectPath    string
	lastBranch         string
	lastFreshProvision bool
}

func (m *gitCloneCapturingManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	m.lastEnv = opts.Env
	m.lastGitClone = opts.GitClone
	m.lastWorkspace = opts.Workspace
	m.lastProjectPath = opts.ProjectPath
	m.lastBranch = opts.Branch
	m.lastFreshProvision = opts.FreshProvision
	return m.mockManager.Start(ctx, opts)
}

func newTestServerWithGitCloneCapture() (*Server, *gitCloneCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &gitCloneCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

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

	// Verify workspace and projectPath were cleared
	if mgr.lastWorkspace != "" {
		t.Errorf("expected workspace to be empty in git clone mode, got '%s'", mgr.lastWorkspace)
	}
	if mgr.lastProjectPath != "" {
		t.Errorf("expected projectPath to be empty in git clone mode, got '%s'", mgr.lastProjectPath)
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

// TestStartAgentWithGitCloneOnly proves the start handler decodes a
// top-level gitClone field the same way create's config.gitClone is decoded
// (GoogleCloudPlatform/scion#1931), so a workspace that did not survive a
// stop can be recreated on start.
func TestStartAgentWithGitCloneOnly(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"gitClone": {
			"url": "https://github.com/example/repo.git",
			"branch": "develop",
			"depth": 1
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/git-clone-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
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
	if _, ok := mgr.lastEnv["SCION_AGENT_BRANCH"]; ok {
		t.Errorf("expected SCION_AGENT_BRANCH to be unset when no top-level branch is sent, got %q", mgr.lastEnv["SCION_AGENT_BRANCH"])
	}
	if mgr.lastGitClone == nil || mgr.lastGitClone.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected GitClone to be passed through to StartOptions, got %+v", mgr.lastGitClone)
	}
	// A start dispatch must never set FreshProvision: GitClone being present
	// means the workspace may need recreating on a runtime that dropped it,
	// not that a same-named leftover should be wiped (GoogleCloudPlatform/scion#1931).
	if mgr.lastFreshProvision {
		t.Error("expected opts.FreshProvision=false for a start dispatch, got true")
	}
}

// TestStartAgentWithGitCloneAndBranch mirrors TestCreateAgentWithGitCloneAndBranch
// for the start path: the top-level branch (agent's checkout branch) and the
// gitClone's own branch (the clone source ref) are independent.
func TestStartAgentWithGitCloneAndBranch(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"branch": "my-feature",
		"gitClone": {
			"url": "https://github.com/example/repo.git",
			"branch": "main",
			"depth": 1
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/branch-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
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

// TestStartAgentOldHubPayloadHasNoWorkspaceFields proves a start request
// without gitClone/branch/workspaceMode (an older Hub, or a non-git project)
// behaves exactly as it did before those fields existed: no git-clone env is
// injected and the request still succeeds.
func TestStartAgentOldHubPayloadHasNoWorkspaceFields(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"resolvedEnv": {"SCION_HUB_ENDPOINT": "https://hub.example.com"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/old-payload-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastGitClone != nil {
		t.Errorf("expected GitClone to be nil for an old-shape payload, got %+v", mgr.lastGitClone)
	}
	if mgr.lastBranch != "" {
		t.Errorf("expected opts.Branch to be empty for an old-shape payload, got %q", mgr.lastBranch)
	}
	// SCION_WORKSPACE_MODE is excluded: it always defaults to shared-plain
	// when no workspace mode is supplied, independent of gitClone/branch.
	for _, key := range []string{"SCION_GIT_CLONE_URL", "SCION_GIT_BRANCH", "SCION_GIT_DEPTH", "SCION_AGENT_BRANCH"} {
		if _, ok := mgr.lastEnv[key]; ok {
			t.Errorf("expected %s to be unset for an old-shape payload, got %q", key, mgr.lastEnv[key])
		}
	}
}

// TestStartAgentIgnoresUnknownJSONKey proves an unrecognized top-level key in
// the start request body (e.g. from a newer Hub sending a field this broker
// version doesn't know about yet) does not break decoding of the fields this
// broker does recognize. The unknown key is placed BEFORE the recognized
// fields in the JSON object. The assertions check that gitClone and
// workspaceMode — sent in the same payload — were actually applied, not
// just that the response status was 202: a vacuous assertion (e.g. only
// checking GitClone is nil) would pass even if decoding silently stopped
// after the unknown key.
func TestStartAgentIgnoresUnknownJSONKey(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{
		"someFutureField": {"nested": "value"},
		"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main"},
		"workspaceMode": "clone-per-agent"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/unknown-key-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastGitClone == nil || mgr.lastGitClone.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected GitClone to be applied despite the preceding unknown key, got %+v", mgr.lastGitClone)
	}
	if got := mgr.lastEnv["SCION_WORKSPACE_MODE"]; got != "clone-per-agent" {
		t.Errorf("expected SCION_WORKSPACE_MODE='clone-per-agent' despite the preceding unknown key, got %q", got)
	}
}

// TestStartAgentBranchOnlyPropagatesToOpts proves a start request carrying
// only a top-level branch (no gitClone) still reaches opts.Branch: the
// broker-side cfg-building condition includes startReq.Branch != "" as one
// of its triggers, not only GitClone.
func TestStartAgentBranchOnlyPropagatesToOpts(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()

	body := `{"branch": "my-feature"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/branch-only-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastBranch != "my-feature" {
		t.Errorf("expected opts.Branch='my-feature', got %q", mgr.lastBranch)
	}
	if mgr.lastGitClone != nil {
		t.Errorf("expected GitClone to remain nil for a branch-only start, got %+v", mgr.lastGitClone)
	}
}

// TestStartAndCreate_GitWorkspaceEnvParity proves create and start inject the
// identical set of SCION_GIT_*, SCION_AGENT_BRANCH, and SCION_WORKSPACE_*
// keys and values for equivalent GitClone/Branch/WorkspaceMode inputs
// (GoogleCloudPlatform/scion#1931) — the two paths must not drift.
func TestStartAndCreate_GitWorkspaceEnvParity(t *testing.T) {
	const parityKeys = "SCION_GIT_CLONE_URL,SCION_GIT_BRANCH,SCION_GIT_DEPTH,SCION_AGENT_BRANCH,SCION_WORKSPACE_MODE,SCION_WORKSPACE_GIT"
	keys := strings.Split(parityKeys, ",")

	createSrv, createMgr := newTestServerWithGitCloneCapture()
	createBody := `{
		"name": "parity-agent",
		"workspaceMode": "clone-per-agent",
		"config": {
			"template": "claude",
			"branch": "my-feature",
			"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main", "depth": 1}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	createSrv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	startSrv, startMgr := newTestServerWithGitCloneCapture()
	startBody := `{
		"branch": "my-feature",
		"workspaceMode": "clone-per-agent",
		"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main", "depth": 1}
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/parity-agent/start", strings.NewReader(startBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	startSrv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	for _, key := range keys {
		createVal, createOK := createMgr.lastEnv[key]
		startVal, startOK := startMgr.lastEnv[key]
		if createOK != startOK || createVal != startVal {
			t.Errorf("%s: create=%q(present=%v) start=%q(present=%v), want identical", key, createVal, createOK, startVal, startOK)
		}
	}

	// FreshProvision is the one input that must NOT be at parity: create
	// dispatches it true (safe to wipe a same-named leftover), start leaves
	// it false (must preserve a workspace that survived a stop).
	if !createMgr.lastFreshProvision {
		t.Error("expected opts.FreshProvision=true for a create dispatch, got false")
	}
	if startMgr.lastFreshProvision {
		t.Error("expected opts.FreshProvision=false for a start dispatch, got true")
	}
}

func TestResolveManagerForOpts_NoProfile(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{Name: "test-agent"}
	mgr, _ := srv.resolveManagerForOpts(opts)

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
	mgr, _ := srv.resolveManagerForOpts(opts)

	// Profile not found in settings should return the default manager
	if mgr != srv.manager {
		t.Error("expected default manager when profile not found in settings")
	}
}

func TestResolveManagerForOpts_ProfileWithDifferentRuntime(t *testing.T) {
	// Create a temp project directory with settings that specify a different runtime
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Write settings.yaml with a profile that specifies runtime "container"
	// (which differs from the broker's "docker" runtime)
	settingsYAML := `schema_version: "1"
profiles:
  apple:
    runtime: container
runtimes:
  container:
    type: container
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""

	opts := api.StartOptions{
		Name:        "test-agent",
		Profile:     "apple",
		ProjectPath: projectPath,
	}
	mgr, _ := srv.resolveManagerForOpts(opts)

	// Profile specifies "container" runtime which differs from mock's "mock",
	// so we should get a different manager
	if mgr == srv.manager {
		t.Error("expected a different manager when profile specifies a different runtime")
	}
}

// TestResolveManagerForOpts_NilRuntimeResolverFallsBack proves that a nil
// srv.resolveAuxiliaryRuntime does not panic when settings resolve to a
// runtime other than the broker's default. New() always sets
// resolveAuxiliaryRuntime to agent.ResolveRuntime, so this only matters for
// a Server built without New() (e.g. a test literal, or some future
// construction path) — but resolveManagerForOpts falls back to that exact
// same function rather than a stand-in, so the fallback resolves
// identically to production.
//
// Isolated from ambient SCION_* env and HOME: without that isolation, an
// ambient SCION_AUTO_EXPOSE_PORTS collides with the struct-typed
// auto_expose_ports settings key, LoadEffectiveSettings fails to decode,
// and resolveManagerForOpts returns the default manager before ever
// reaching the nil-resolver fallback this test is meant to exercise.
func TestResolveManagerForOpts_NilRuntimeResolverFallsBack(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())

	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	settingsYAML := `schema_version: "1"
profiles:
  apple:
    runtime: container
runtimes:
  container:
    type: container
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""
	srv.resolveAuxiliaryRuntime = nil

	opts := api.StartOptions{
		Name:        "test-agent",
		Profile:     "apple",
		ProjectPath: projectPath,
	}

	// Must not panic.
	mgr, _ := srv.resolveManagerForOpts(opts)

	// Profile specifies "container" runtime which differs from mock's
	// "mock", so the fallback must have actually resolved a new manager
	// rather than silently keeping the default.
	if mgr == srv.manager {
		t.Error("expected a different manager when the nil-resolver fallback resolves a different runtime")
	}
}

func TestResolveManagerForOpts_ProfileWithSameRuntime(t *testing.T) {
	// Create a temp project directory with settings that specify the same runtime as the broker
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Write settings with profile whose runtime matches the broker's runtime ("docker")
	settingsYAML := `schema_version: "1"
profiles:
  local:
    runtime: docker
runtimes:
  docker:
    type: docker
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()

	opts := api.StartOptions{
		Name:        "test-agent",
		Profile:     "local",
		ProjectPath: projectPath,
	}
	mgr, _ := srv.resolveManagerForOpts(opts)

	// Profile specifies "docker" runtime which matches the broker's runtime,
	// so we should get the same manager
	if mgr != srv.manager {
		t.Error("expected default manager when profile resolves to same runtime")
	}
}

// TestResolveManagerForOpts_EnvCollision_HonoursProjectRuntime is the
// end-to-end regression test for
// https://github.com/ptone/scion/issues/2447 ("runtimebroker: project
// runtime settings silently ignored when a colliding SCION_* env var is
// set"). SCION_AUTO_EXPOSE_PORTS is a variable the hub sets inside agent
// containers; a broker started inside one (e.g. `scion server start` run
// from an agent) inherits it in its own process environment.
// config.LoadEffectiveSettings merges every SCION_*-prefixed process env var
// through koanf, and SCION_AUTO_EXPOSE_PORTS used to map to the
// struct-typed "auto_expose_ports" key, making Unmarshal fail. Before the
// fix, resolveManagerForOpts discarded that error silently (`vs, _, _ :=`)
// and fell back to the broker's default manager/runtime — so the project's
// configured runtime was never honoured.
//
// This asserts the actual runtime of the returned manager, not merely that
// it differs from the broker's default, since a weaker `mgr != srv.manager`
// check also passes if resolution lands on some other wrong runtime.
func TestResolveManagerForOpts_EnvCollision_HonoursProjectRuntime(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_AUTO_EXPOSE_PORTS", "true")

	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}

	// A profile remapped to a non-default runtime ("container"), active by
	// default so opts.Profile can stay empty, matching how the broker
	// resolves a project's own settings with no explicit --profile flag.
	settingsYAML := `schema_version: "1"
active_profile: apple
profiles:
  apple:
    runtime: container
runtimes:
  container:
    type: container
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	opts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectPath,
	}
	mgr, rtName := srv.resolveManagerForOpts(opts)

	// The broker's default runtime is "docker" (see
	// newTestServerWithProvisionCapture); the project's active profile
	// resolves to "container", so the manager actually returned must run on
	// "container" — not merely be some manager other than the default.
	if rtName != "container" {
		t.Errorf("expected resolveManagerForOpts to report runtime %q, got %q (the SCION_AUTO_EXPOSE_PORTS collision was not handled)", "container", rtName)
	}
	am, ok := mgr.(*agent.AgentManager)
	if !ok {
		t.Fatalf("expected *agent.AgentManager, got %T", mgr)
	}
	if got := am.Runtime.Name(); got != "container" {
		t.Errorf("expected the project's configured runtime %q, got %q (the SCION_AUTO_EXPOSE_PORTS collision was not handled)", "container", got)
	}

	// Decoding succeeds once the colliding var is excluded from the env
	// provider, so no warning should be logged on this path.
	if logged := logBuf.String(); strings.Contains(logged, "level=WARN") {
		t.Errorf("expected no WARN log on the successful collision path, got: %s", logged)
	}
}

// TestResolveManagerForOpts_SettingsDecodeErrorIsLogged proves
// resolveManagerForOpts no longer swallows a settings decode error
// silently: it must log a warning identifying the project directory and the
// error, then fall back to the broker's default manager. A malformed
// settings.yaml (not the SCION_AUTO_EXPOSE_PORTS collision, which the
// mapper fix above avoids entirely) is used to force a real decode error
// deterministically.
func TestResolveManagerForOpts_SettingsDecodeErrorIsLogged(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())

	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}
	// profiles is declared as a scalar here instead of a map, forcing
	// VersionedSettings' Unmarshal to fail.
	malformedYAML := `schema_version: "1"
profiles: "not-a-map"
`
	if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(malformedYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServerWithProvisionCapture()
	srv.config.ForceRuntime = ""
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	opts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectPath,
	}
	mgr, rtName := srv.resolveManagerForOpts(opts)

	if mgr != srv.manager {
		t.Error("expected default manager when settings fail to decode")
	}
	if rtName != srv.runtime.Name() {
		t.Errorf("expected the broker's default runtime %q when settings fail to decode, got %q", srv.runtime.Name(), rtName)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("expected a WARN-level log entry for the settings decode error, got: %s", logged)
	}
	if !strings.Contains(logged, projectPath) {
		t.Errorf("expected the logged warning to include the project dir %q, got: %s", projectPath, logged)
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

func TestProjectSlugWorkspacePath(t *testing.T) {
	// Verify the workspace directory path for hub-managed projects uses
	// ~/.scion/projects/<slug>/ instead of the worktree-based path.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}

	expected := filepath.Join(globalDir, "projects", "my-test-project")

	// Simulate the logic from the handler: when ProjectSlug is set,
	// use the conventional path.
	projectSlug := "my-test-project"
	workspaceDir := filepath.Join(globalDir, "projects", projectSlug)

	if workspaceDir != expected {
		t.Errorf("expected workspace dir %q, got %q", expected, workspaceDir)
	}

	// When ProjectSlug is empty, the default worktree path is used.
	worktreeBase := "/tmp/test-worktrees"
	agentName := "test-agent"
	defaultDir := filepath.Join(worktreeBase, agentName, "workspace")
	expectedDefault := "/tmp/test-worktrees/test-agent/workspace"
	if defaultDir != expectedDefault {
		t.Errorf("expected default workspace dir %q, got %q", expectedDefault, defaultDir)
	}
}

func TestCreateAgentRequest_ProjectSlugField(t *testing.T) {
	// Verify ProjectSlug is properly serialized/deserialized in CreateAgentRequest.
	reqJSON := `{
		"name": "project-agent",
		"projectSlug": "my-hub-project",
		"workspaceStoragePath": "workspaces/project-123/project-workspace"
	}`

	var req CreateAgentRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if req.ProjectSlug != "my-hub-project" {
		t.Errorf("expected ProjectSlug 'my-hub-project', got '%s'", req.ProjectSlug)
	}
	if req.WorkspaceStoragePath != "workspaces/project-123/project-workspace" {
		t.Errorf("expected WorkspaceStoragePath 'workspaces/project-123/project-workspace', got '%s'", req.WorkspaceStoragePath)
	}
}

func TestCreateAgentProjectSlugResolvesProjectPath(t *testing.T) {
	// When ProjectSlug is set and ProjectPath is empty (hub-managed project with no
	// local provider path), the handler should resolve ProjectPath to the
	// conventional ~/.scion/projects/<slug>/ path so the agent is created in the
	// correct project instead of the broker's local project.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "hub-managed-agent",
		"id": "agent-uuid-123",
		"slug": "hub-managed-agent",
		"projectId": "project-abc",
		"projectSlug": "my-hub-project",
		"provisionOnly": true,
		"config": {"template": "claude", "workspace": "/hub/projects/my-hub-project"}
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

	expectedPath := filepath.Join(globalDir, "projects", "my-hub-project")
	if mgr.lastOpts.ProjectPath != expectedPath {
		t.Errorf("expected ProjectPath %q, got %q", expectedPath, mgr.lastOpts.ProjectPath)
	}
}

func TestCreateAgentProjectSlugNotUsedWhenProjectPathSet(t *testing.T) {
	// When both ProjectPath and ProjectSlug are set, ProjectPath takes precedence
	// (the broker has a local provider path for this project).
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "local-project-agent",
		"id": "agent-uuid-456",
		"slug": "local-project-agent",
		"projectId": "project-def",
		"projectSlug": "my-hub-project",
		"projectPath": "/projects/my-local-project/.scion",
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

	// ProjectPath should remain as explicitly provided, not overridden by ProjectSlug
	if mgr.lastOpts.ProjectPath != "/projects/my-local-project/.scion" {
		t.Errorf("expected ProjectPath %q, got %q", "/projects/my-local-project/.scion", mgr.lastOpts.ProjectPath)
	}
}

// TestStartAgentProjectSettingsFallbackHubEndpoint verifies that the startAgent
// handler uses project settings hub.endpoint only as a fallback when no broker
// config or dispatch endpoint is available.
func TestStartAgentProjectSettingsFallbackHubEndpoint(t *testing.T) {
	t.Run("linked project with settings at projectPath", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:9810"
		cfg.Debug = true
		cfg.ForceRuntime = "mock"

		mgr := &provisionCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt)

		// Linked project: projectPath ends in .scion, settings.yaml is directly there
		projectDir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatalf("failed to create project dir: %v", err)
		}
		settingsContent := "hub:\n  endpoint: https://hub.production.example.com\n"
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		body := fmt.Sprintf(`{"projectPath": %q}`, projectDir)
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

		// Broker config HubEndpoint takes priority over project settings
		if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
		}
		if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from broker config, got %q", got)
		}
	})

	t.Run("hub-managed project with settings in .scion subdirectory", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:9810"
		cfg.Debug = true
		cfg.ForceRuntime = "mock"

		mgr := &provisionCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		srv := New(cfg, mgr, rt)

		// Hub-managed project: projectPath is the workspace parent (~/.scion/projects/<slug>),
		// settings.yaml lives in the .scion subdirectory
		projectDir := t.TempDir()
		scionDir := filepath.Join(projectDir, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatalf("failed to create .scion dir: %v", err)
		}
		settingsContent := "hub:\n  endpoint: https://hub.native.example.com\n"
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
			t.Fatalf("failed to write settings.yaml: %v", err)
		}

		body := fmt.Sprintf(`{"projectPath": %q}`, projectDir)
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

		// Broker config HubEndpoint takes priority over project settings
		if got := mgr.lastOpts.Env["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_ENDPOINT='http://localhost:9810' from broker config, got %q", got)
		}
		if got := mgr.lastOpts.Env["SCION_HUB_URL"]; got != "http://localhost:9810" {
			t.Errorf("expected SCION_HUB_URL='http://localhost:9810' from broker config, got %q", got)
		}
	})
}

// TestStartAgentBrokerConfigUsedWhenNoProjectSettings verifies that the broker's
// config HubEndpoint is used as fallback when project settings don't specify one.
func TestStartAgentBrokerConfigUsedWhenNoProjectSettings(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.HubEndpoint = "http://localhost:9810"
	cfg.Debug = true
	cfg.ForceRuntime = "mock"

	mgr := &provisionCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Create a temp project dir with settings.yaml but no hub endpoint
	projectDir := t.TempDir()
	settingsContent := "harnesses:\n  claude:\n    model: sonnet\n"
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write settings.yaml: %v", err)
	}

	body := fmt.Sprintf(`{"projectPath": %q}`, projectDir)
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

	// Without project settings hub.endpoint, broker config should be used
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
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := `{"resolvedEnv": {"SCION_HUB_ENDPOINT": "http://hub.example.com:8080", "SCION_PROJECT_ID": "project-1"}}`
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
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
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
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
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

// TestHTTPStartAndRestart_DispatchedEndpointOutranksLocalhostBroker is the
// regression test for GoogleCloudPlatform/scion#1931: on a co-located
// (combo) broker, the broker's own HubEndpoint is often a localhost address
// (its own view of the Hub). On the kubernetes runtime, an agent started or
// restarted through the broker's HTTP handlers must still get the Hub's
// dispatched public endpoint, in parity with what create gives it — not the
// broker's localhost view, which means nothing inside the pod.
func TestHTTPStartAndRestart_DispatchedEndpointOutranksLocalhostBroker(t *testing.T) {
	const dispatched = "https://hub.example.com"

	// A dedicated isolated server, rather than newTestServerWithRuntime: the
	// runtime name here ("kubernetes") must also be the settings-resolved
	// runtime type, or resolveManagerForOpts detects a mismatch against the
	// injected mock runtime and builds a real auxiliary manager instead of
	// using the capturing mock — bypassing the very capture this test needs.
	newSrv := func(t *testing.T) (*Server, *envCapturingManager) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())

		origWd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origWd) })

		dotScion := filepath.Join(tmpDir, ".scion")
		if err := os.Mkdir(dotScion, 0755); err != nil {
			t.Fatal(err)
		}
		// No explicit "type" for the "kubernetes" runtime entry: ResolveRuntime
		// falls back to the map key as the effective type, so it resolves to
		// "kubernetes" — matching the injected runtime's Name() below — and
		// resolveManagerForOpts uses the broker's own (capturing) manager.
		settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes: {}
`
		if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatal(err)
		}
		templatesDir := filepath.Join(dotScion, "templates")
		if err := os.MkdirAll(filepath.Join(templatesDir, "default"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
			t.Fatal(err)
		}

		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:8080" // combo broker's own (loopback) view
		cfg.ForceRuntime = "kubernetes"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		return New(cfg, mgr, rt), mgr
	}

	assertDispatched := func(t *testing.T, mgr *envCapturingManager) {
		t.Helper()
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != dispatched {
			t.Errorf("SCION_HUB_ENDPOINT = %q, want the Hub-dispatched %q (not the broker's own localhost endpoint)", got, dispatched)
		}
		if got := mgr.lastEnv["SCION_HUB_URL"]; got != dispatched {
			t.Errorf("SCION_HUB_URL = %q, want the Hub-dispatched %q", got, dispatched)
		}
	}

	t.Run("start", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, dispatched)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})

	t.Run("restart", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, dispatched)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})

	t.Run("start: request field wins over a differing resolvedEnv value", func(t *testing.T) {
		srv, mgr := newSrv(t)
		const stale = "https://stale-in-resolved-env.example.com"
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, stale)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})

	t.Run("restart: request field wins over a differing resolvedEnv value", func(t *testing.T) {
		srv, mgr := newSrv(t)
		const stale = "https://stale-in-resolved-env.example.com"
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatched, stale)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertDispatched(t, mgr)
	})
}

// TestHTTPStartAndRestart_DockerComboBridgeOverrideMatchesCreate proves
// parity for the docker combo case in GoogleCloudPlatform/scion#1931: create
// sends the dispatched endpoint as its request-level HubEndpoint; start and
// restart send it as their own request-level HubEndpoint and in
// ResolvedEnv["SCION_HUB_ENDPOINT"], as the Hub does; and — for both a
// public dispatched endpoint and a localhost one that then goes through the
// container bridge override — all three operations resolve to the same
// SCION_HUB_ENDPOINT for the broker's ContainerHubEndpoint configuration.
func TestHTTPStartAndRestart_DockerComboBridgeOverrideMatchesCreate(t *testing.T) {
	newSrv := func() (*Server, *envCapturingManager) {
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEndpoint = "http://localhost:8080" // combo broker's own (loopback) view
		cfg.ContainerHubEndpoint = "http://host.docker.internal:8080"
		cfg.ForceRuntime = "mock"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
		return New(cfg, mgr, rt), mgr
	}

	cases := []struct {
		name       string
		dispatched string
		want       string
	}{
		{name: "public dispatched endpoint", dispatched: "https://hub.example.com", want: "https://hub.example.com"},
		{name: "localhost dispatched endpoint goes through the bridge override", dispatched: "http://localhost:8080", want: "http://host.docker.internal:8080"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("create", func(t *testing.T) {
				srv, mgr := newSrv()
				body := fmt.Sprintf(`{"name": "test-agent", "hubEndpoint": %q}`, tc.dispatched)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusCreated {
					t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
				}
				if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != tc.want {
					t.Errorf("create: SCION_HUB_ENDPOINT = %q, want %q", got, tc.want)
				}
			})

			t.Run("http-start", func(t *testing.T) {
				srv, mgr := newSrv()
				body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, tc.dispatched, tc.dispatched)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusAccepted {
					t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
				}
				if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != tc.want {
					t.Errorf("http-start: SCION_HUB_ENDPOINT = %q, want %q (same as create)", got, tc.want)
				}
			})

			t.Run("http-restart", func(t *testing.T) {
				srv, mgr := newSrv()
				body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, tc.dispatched, tc.dispatched)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()

				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusAccepted {
					t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
				}
				if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != tc.want {
					t.Errorf("http-restart: SCION_HUB_ENDPOINT = %q, want %q (same as create)", got, tc.want)
				}
			})
		})
	}
}

// TestHTTPStartAndRestart_ConnectionHeaderRescuesLocalhostDispatch proves the
// connection-endpoint localhost rescue is wired all the way from the
// X-Scion-Hub-Connection header through the HTTP start and restart handlers,
// not just inside the resolver: a registered connection naming a public
// endpoint rescues a localhost dispatched value on kubernetes, the same way
// it does on create with the same header.
func TestHTTPStartAndRestart_ConnectionHeaderRescuesLocalhostDispatch(t *testing.T) {
	const (
		connectionName = "conn1"
		connEndpoint   = "https://hub.connection.example.com"
		dispatchLocal  = "http://localhost:9090"
	)

	// A dedicated isolated server: the runtime name here ("kubernetes") must
	// also be the settings-resolved runtime type, or resolveManagerForOpts
	// detects a mismatch against the injected mock runtime and builds a real
	// auxiliary manager instead of using the capturing mock.
	newSrv := func(t *testing.T) (*Server, *envCapturingManager) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())

		origWd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		tmpDir := t.TempDir()
		if err := os.Chdir(tmpDir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(origWd) })

		dotScion := filepath.Join(tmpDir, ".scion")
		if err := os.Mkdir(dotScion, 0755); err != nil {
			t.Fatal(err)
		}
		settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
runtimes:
    kubernetes: {}
`
		if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatal(err)
		}
		templatesDir := filepath.Join(dotScion, "templates")
		if err := os.MkdirAll(filepath.Join(templatesDir, "default"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(templatesDir, "claude"), 0755); err != nil {
			t.Fatal(err)
		}

		creds := makeTestCreds(connectionName, "test-broker-id", connEndpoint)
		cfg := DefaultServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.BrokerName = "test-host"
		cfg.HubEnabled = true
		cfg.HubEndpoint = "http://localhost:8080" // combo broker's own (loopback) view
		cfg.InMemoryCredentials = creds
		cfg.BrokerAuthEnabled = false
		cfg.ForceRuntime = "kubernetes"

		mgr := &envCapturingManager{}
		rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
		return New(cfg, mgr, rt), mgr
	}

	assertRescued := func(t *testing.T, mgr *envCapturingManager) {
		t.Helper()
		if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != connEndpoint {
			t.Errorf("SCION_HUB_ENDPOINT = %q, want the connection endpoint %q (not the broker's own localhost, and not the raw localhost dispatch)", got, connEndpoint)
		}
	}

	t.Run("create", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"name": "test-agent", "hubEndpoint": %q}`, dispatchLocal)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", connectionName)
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
		}
		assertRescued(t, mgr)
	})

	t.Run("http-start", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatchLocal, dispatchLocal)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/start", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", connectionName)
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertRescued(t, mgr)
	})

	t.Run("http-restart", func(t *testing.T) {
		srv, mgr := newSrv(t)
		body := fmt.Sprintf(`{"hubEndpoint": %q, "resolvedEnv": {"SCION_HUB_ENDPOINT": %q}}`, dispatchLocal, dispatchLocal)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Hub-Connection", connectionName)
		w := httptest.NewRecorder()

		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
		}
		assertRescued(t, mgr)
	})
}

// TestRestartAgent_ContainerScanSuppliesSettingsFallback proves restartAgent's
// container-scan branch (used when the request carries no projectPath) feeds
// the scanned agent's ProjectPath into buildStartContext, so the
// project-settings fallback reaches the resolver on restart and is not
// silently dropped by an empty ProjectPath.
func TestRestartAgent_ContainerScanSuppliesSettingsFallback(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  endpoint: https://settings.example.com\n"), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	// No broker-level HubEndpoint: the settings fallback must supply it.

	mgr := &envCapturingManager{}
	mgr.agents = []api.AgentInfo{
		{Name: "test-agent", ProjectPath: projectDir, Labels: map[string]string{"scion.project_id": "p1"}},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/restart?projectId=p1", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if got := mgr.lastEnv["SCION_HUB_ENDPOINT"]; got != "https://settings.example.com" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want the settings-supplied endpoint %q (container-scan projectPath must reach the resolver)", got, "https://settings.example.com")
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
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
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

func TestStartAgentProjectSlugResolvesProjectPath(t *testing.T) {
	// When the startAgent handler receives projectSlug with no projectPath
	// (hub-managed project), it should resolve ProjectPath from the slug.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"projectSlug": "my-hub-project"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/hub-managed-agent/start", strings.NewReader(body))
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

	expectedPath := filepath.Join(globalDir, "projects", "my-hub-project")
	if mgr.lastOpts.ProjectPath != expectedPath {
		t.Errorf("expected ProjectPath %q, got %q", expectedPath, mgr.lastOpts.ProjectPath)
	}
}

func TestStartAgentProjectSlugNotUsedWhenProjectPathSet(t *testing.T) {
	// When startAgent receives both projectPath and projectSlug,
	// projectPath takes precedence.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"projectPath": "/projects/my-local-project/.scion",
		"projectSlug": "my-hub-project"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/local-project-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	expectedPath := "/projects/my-local-project/.scion"
	if mgr.lastOpts.ProjectPath != expectedPath {
		t.Errorf("expected ProjectPath %q, got %q", expectedPath, mgr.lastOpts.ProjectPath)
	}
}

// TestStartAgentLegacyGroveFieldsNotHonoured verifies that a start request
// carrying only the retired grovePath/groveSlug fields resolves no project
// path: the legacy fallback that used to promote them into
// ProjectPath/ProjectSlug is gone.
func TestStartAgentLegacyGroveFieldsNotHonoured(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"grovePath": "/projects/my-local-project/.scion", "groveSlug": "my-hub-project"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/legacy-fields-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	if mgr.lastOpts.ProjectPath != "" {
		t.Errorf("expected legacy grovePath to be ignored, got ProjectPath %q", mgr.lastOpts.ProjectPath)
	}
}

func TestStartAgentInlineConfigModelUpdatesExistingAgentConfig(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "configured-agent"
	agentDir := config.GetAgentDir(projectDir, agentName, false)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(cfgPath, []byte(`{"harness":"gemini","max_turns":3}`), 0644); err != nil {
		t.Fatalf("failed to write scion-agent.json: %v", err)
	}

	body := fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"model": "gemini-2.5-pro",
			"max_turns": 7
		}
	}`, projectDir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read updated scion-agent.json: %v", err)
	}
	var updated api.ScionConfig
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatalf("failed to parse updated scion-agent.json: %v", err)
	}

	if updated.Harness != "gemini" {
		t.Errorf("expected existing harness to be preserved, got %q", updated.Harness)
	}
	if updated.Model != "gemini-2.5-pro" {
		t.Errorf("expected model %q, got %q", "gemini-2.5-pro", updated.Model)
	}
	if updated.MaxTurns != 7 {
		t.Errorf("expected max_turns 7, got %d", updated.MaxTurns)
	}
}

// TestStartAgent_RejectsTraversalName is the regression anchor for
// startAgent's containment check: id must be a single, clean path element
// that resolves under the project's agents root before any of
// applyInlineConfigUpdate, GetSavedProfile, GetSavedPhase, or mgr.Start runs,
// checked independently of whatever the request routing already filtered.
func TestStartAgent_RejectsTraversalName(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	projectDir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(filepath.Join(projectDir, "agents"), 0755); err != nil {
		t.Fatalf("failed to create projectDir/agents: %v", err)
	}
	// Sentinel representing project-level state that must remain untouched
	// when the request's agent id fails containment.
	sentinelPath := filepath.Join(projectDir, "scion-agent.json")
	const sentinelContent = `{"harness":"sentinel-do-not-touch"}`
	if err := os.WriteFile(sentinelPath, []byte(sentinelContent), 0644); err != nil {
		t.Fatalf("failed to write sentinel: %v", err)
	}

	body := fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"model": "gemini-2.5-pro"
		}
	}`, projectDir)
	// Decodes to id="..", action="start".
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/..%2Fstart", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
	if mgr.startCalled {
		t.Fatal("expected Start not to be called")
	}

	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatalf("sentinel must survive untouched, but read failed: %v", err)
	}
	if string(data) != sentinelContent {
		t.Fatalf("sentinel must survive unmodified, got: %s", data)
	}
}

func TestStartAgentInlineConfigPassedForProvisionOnStart(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	projectDir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}

	body := fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"model": "gemini-2.5-pro"
		}
	}`, projectDir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/provision-on-start-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	if mgr.lastOpts.InlineConfig == nil {
		t.Fatal("expected InlineConfig to be passed to Start")
	}
	if mgr.lastOpts.InlineConfig.Model != "gemini-2.5-pro" {
		t.Errorf("expected inline model %q, got %q", "gemini-2.5-pro", mgr.lastOpts.InlineConfig.Model)
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

func TestCreateAgentProjectSlugInitializesScionDir(t *testing.T) {
	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	defer restore()

	restoreGit := config.OverrideIsGitRepo(func() bool { return true })
	defer restoreGit()

	// When ProjectSlug is set and the broker has no .scion subdirectory for
	// the hub-managed project, the handler should create it so that
	// ResolveProjectPath resolves to projects/<slug>/.scion (not projects/<slug>).
	// This prevents agents from being created at the wrong directory level.

	// Use a temporary directory to simulate the project workspace.
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, "test-project")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatalf("failed to create test project dir: %v", err)
	}

	// Verify .scion does NOT exist yet
	scionDir := filepath.Join(projectPath, ".scion")
	if _, err := os.Stat(scionDir); !os.IsNotExist(err) {
		t.Fatal(".scion should not exist before initialization")
	}

	// Verify ResolveProjectPath does NOT resolve to .scion when it doesn't exist
	resolved, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		t.Fatalf("ResolveProjectPath failed: %v", err)
	}
	if resolved != projectPath {
		t.Errorf("before init: expected ResolveProjectPath to return %q, got %q", projectPath, resolved)
	}

	// Initialize .scion (mirrors what the handler now does)
	if err := config.InitProject(scionDir, nil); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Verify .scion was created
	if info, err := os.Stat(scionDir); err != nil || !info.IsDir() {
		t.Fatal(".scion directory should exist after InitProject")
	}

	// Verify ResolveProjectPath now resolves to the .scion subdirectory
	resolved, _, err = config.ResolveProjectPath(projectPath)
	if err != nil {
		t.Fatalf("ResolveProjectPath failed: %v", err)
	}
	if resolved != scionDir {
		t.Errorf("after init: expected ResolveProjectPath to resolve to %q, got %q", scionDir, resolved)
	}
}

// ============================================================================
// Project Cleanup Endpoint Tests
// ============================================================================

func TestDeleteProject_RemovesDirectory(t *testing.T) {
	srv := newTestServer(t)

	// Create a temporary projects directory structure
	tmpHome := t.TempDir()
	projectsDir := filepath.Join(tmpHome, ".scion", "projects")
	projectDir := filepath.Join(projectsDir, "test-project")
	scionDir := filepath.Join(projectDir, ".scion")

	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatalf("failed to create test project dir: %v", err)
	}

	// Write a dummy file so we can verify deletion
	dummyFile := filepath.Join(scionDir, "settings.yaml")
	if err := os.WriteFile(dummyFile, []byte("test: true"), 0o644); err != nil {
		t.Fatalf("failed to write dummy file: %v", err)
	}

	// Override HOME so config.GetGlobalDir resolves to our temp dir
	t.Setenv("HOME", tmpHome)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/test-project", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify directory was removed
	if _, err := os.Stat(projectDir); !os.IsNotExist(err) {
		t.Errorf("expected project directory to be removed, but it still exists")
	}
}

func TestDeleteProject_NonExistent_Returns204(t *testing.T) {
	srv := newTestServer(t)

	tmpHome := t.TempDir()
	// Create the projects parent but NOT the specific project directory
	projectsDir := filepath.Join(tmpHome, ".scion", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	t.Setenv("HOME", tmpHome)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/nonexistent-project", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for non-existent project, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProject_PathTraversal_Blocked(t *testing.T) {
	srv := newTestServer(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Attempt path traversal
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/..%2F..%2Fetc", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal attempt, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFindAgentInHubManagedProjects(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create hub-managed project structure with an agent directory
	projectSlug := "my-project"
	scionDir := filepath.Join(tmpHome, ".scion", "projects", projectSlug, ".scion")
	agentDir := filepath.Join(scionDir, "agents", "test-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// Should find the agent in the hub-managed project
	result, _ := findAgentInHubManagedProjects("test-agent", "")
	if result != scionDir {
		t.Errorf("expected %q, got %q", scionDir, result)
	}

	// Should not find a non-existent agent
	result, _ = findAgentInHubManagedProjects("nonexistent-agent", "")
	if result != "" {
		t.Errorf("expected empty string for nonexistent agent, got %q", result)
	}

	// Should handle missing projects directory gracefully
	t.Setenv("HOME", t.TempDir())
	result, _ = findAgentInHubManagedProjects("test-agent", "")
	if result != "" {
		t.Errorf("expected empty string when projects dir missing, got %q", result)
	}
}

func TestDeleteAgent_HubManagedProject_NoContainer(t *testing.T) {
	// Verify that deleting an agent in a hub-managed project resolves the correct
	// project path even when the container doesn't exist (e.g. created-only
	// agent, pruned container).
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{
		agents: []api.AgentInfo{}, // No containers
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create hub-managed project with an agent directory and config file
	projectSlug := "hub-project"
	scionDir := filepath.Join(tmpHome, ".scion", "projects", projectSlug, ".scion")
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

	// Verify the mock manager's Delete was called with the correct project path
	if mgr.DeleteCalls() != 1 {
		t.Fatalf("expected 1 Delete call, got %d", mgr.DeleteCalls())
	}
	if mgr.LastDeleteProjectPath() != scionDir {
		t.Errorf("expected projectPath %q, got %q", scionDir, mgr.LastDeleteProjectPath())
	}
	if mgr.LastDeleteAgentID() != agentName {
		t.Errorf("expected agentID %q, got %q", agentName, mgr.LastDeleteAgentID())
	}
}

// TestDeleteAgent_RejectsTraversalName is the regression anchor for
// deleteAgent's containment check: an agent id must be a single, clean path
// element that resolves under the project's agents root before any file
// operation runs, checked independently of whatever the request routing
// already filtered.
func TestDeleteAgent_RejectsTraversalName(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{agents: []api.AgentInfo{}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectSlug := "hub-project"
	scionDir := filepath.Join(tmpHome, ".scion", "projects", projectSlug, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatalf("failed to create scionDir/agents: %v", err)
	}
	// Sentinel representing project-level state that must remain untouched
	// when the request's agent id fails containment.
	markerPath := filepath.Join(scionDir, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("failed to write marker: %v", err)
	}

	// Decodes to id="..", action="".
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/agents/..%2F?deleteFiles=true&removeBranch=false", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Fatalf("expected no Delete calls, got %d", mgr.DeleteCalls())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("project directory must survive untouched, but stat failed: %v", err)
	}
	if _, err := os.Stat(scionDir); err != nil {
		t.Fatalf("project .scion directory must survive untouched, but stat failed: %v", err)
	}
}

// TestDeleteAgent_ContainerOnlyRemovalSurvivesBadName is the converse of
// TestDeleteAgent_RejectsTraversalName: a request that never asks for file
// cleanup (the default -- no deleteFiles, no softDelete) must still remove a
// legacy agent recorded under a bad name, by container ID alone, with the
// containment check never entering the picture. Runtime.Delete acts on
// containerID, not on any path built from the name, so nothing here is
// unsafe to allow. This is the independent, non-file-touching removal path
// that keeps the containment fix from stranding a pre-existing bad-named
// agent: the check added for the traversal case above must gate only
// requests that actually touch a file, not every request naming a bad
// agent.
func TestDeleteAgent_ContainerOnlyRemovalSurvivesBadName(t *testing.T) {
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{Name: "..", ContainerID: "legacy-bad-name-container"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	// No deleteFiles, no softDelete: a plain container-record removal.
	// Decodes to id="..", action="".
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/..%2F", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 {
		t.Fatalf("expected 1 Delete call (container-only removal must still succeed), got %d", mgr.DeleteCalls())
	}
	if mgr.LastDeleteFiles() {
		t.Fatal("expected deleteFiles to be false for a container-only removal")
	}
}

// TestDeleteAgent_InvalidIDWithDeleteFilesRejected proves that a DELETE with
// deleteFiles=true and an id that is not a single valid path element is
// still rejected: deleteAgent's own gate (isSingleCleanPathElement plus
// containment) is reached and enforced even though handleAgentByID exempts
// a bare DELETE from its own gate. Unlike
// TestDeleteAgent_ContainerOnlyRemovalSurvivesBadName, this request does
// touch files, so deleteAgent's own validation must still apply.
func TestDeleteAgent_InvalidIDWithDeleteFilesRejected(t *testing.T) {
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{Name: `a\b`, ContainerID: "bad-name-container"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/a%5Cb?deleteFiles=true", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Fatalf("expected no Delete call for a rejected id, got %d", mgr.DeleteCalls())
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
	// Create a temp directory to act as the project path with agent files
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	agentDir := filepath.Join(projectPath, "agents", "fail-agent")
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
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	body := fmt.Sprintf(`{
		"name": "fail-agent",
		"projectPath": %q,
		"config": {"task": "do something"}
	}`, projectPath)
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

// TestBuildInfoProfiles_CloudRunSandbox_Task92 is the broker-level pin test
// for task #92. When the broker runs cloudrun-sandbox and the settings define
// a "default" profile with runtime cloudrun-sandbox, buildInfoProfiles must
// return that profile. Before the fix, the workstation defaults (local/docker,
// remote/kubernetes) were seeded; buildInfoProfiles filtered out docker
// (local-only) and returned only remote/kubernetes — which cannot work on
// this tier. With the fix, the "default" profile survives alongside
// "remote" (from embedded defaults merge), and autoSelectProfile does NOT
// fire (length > 1), so "Use broker default" is shown — which resolves to
// active_profile "default" → cloudrun-sandbox.
//
// The assertion is on the PROFILE LIST contents, not just the selected value.
// The bug was a list of length one containing the WRONG entry.
func TestBuildInfoProfiles_CloudRunSandbox_Task92(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Seed settings matching the fixed Cloud Run sandbox template:
	// active_profile=default, profiles.default.runtime=cloudrun-sandbox
	// LoadEffectiveSettings also merges the embedded defaults, which add
	// profiles.local (docker) and profiles.remote (kubernetes). The filter
	// in buildInfoProfiles then drops local (local-only) but keeps both
	// default and remote.
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: default
runtimes:
  cloudrun-sandbox:
    type: cloudrun-sandbox
profiles:
  default:
    runtime: cloudrun-sandbox
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	profiles := srv.buildInfoProfiles("cloudrun-sandbox")

	// Build a lookup map for assertions.
	byName := make(map[string]BrokerProfile, len(profiles))
	for _, p := range profiles {
		byName[p.Name] = p
	}

	// CRITICAL ASSERTION: "default" profile with type "cloudrun-sandbox" MUST
	// be present. This is the fix — before task #92, no cloudrun-sandbox
	// profile existed.
	defaultP, hasDefault := byName["default"]
	if !hasDefault {
		names := make([]string, len(profiles))
		for i, p := range profiles {
			names[i] = fmt.Sprintf("%s(%s)", p.Name, p.Type)
		}
		t.Fatalf("profile 'default' missing; profiles are: %v", names)
	}
	if defaultP.Type != "cloudrun-sandbox" {
		t.Errorf("profile 'default' type = %q, want %q", defaultP.Type, "cloudrun-sandbox")
	}
	if !defaultP.Available {
		t.Error("profile 'default' should be available")
	}

	// GUARD ASSERTION: "local" profile (docker) MUST be filtered out. If it
	// is present, the filter is not working and local-only profiles leak to
	// a non-local broker.
	if _, hasLocal := byName["local"]; hasLocal {
		t.Error("profile 'local' (docker) should be filtered out on a cloudrun-sandbox broker")
	}

	// The profile list must have length > 1 so that autoSelectProfile does
	// NOT fire. "remote" (from embedded defaults merge) is expected alongside
	// "default". With length > 1, the UI shows "Use broker default" which
	// resolves to active_profile "default" → cloudrun-sandbox → works.
	if len(profiles) < 2 {
		t.Errorf("expected >= 2 profiles (so autoSelectProfile does not fire), got %d", len(profiles))
	}
}

// TestBuildInfoProfiles_OldWorkstationDefaults_Task92_Regression is the RED
// counterpart of the pin test. When the WORKSTATION defaults are seeded
// (local/docker + remote/kubernetes) and the broker runtime is cloudrun-sandbox,
// buildInfoProfiles returns only remote/kubernetes — which is the bug.
//
// This test documents the defective state so the pin test's GREEN is meaningful.
// If someone re-introduces the old defaults for Cloud Run sandbox, this test
// shows what would happen: the UI would see only "remote (kubernetes)".
func TestBuildInfoProfiles_OldWorkstationDefaults_Task92_Regression(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Seed the OLD (broken) workstation defaults: local/docker + remote/kubernetes.
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	brokenSettingsYAML := `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(brokenSettingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	profiles := srv.buildInfoProfiles("cloudrun-sandbox")

	// Document the bug: docker is filtered out (local-only), only kubernetes
	// remains. This is the defect — the profile list contains only an entry
	// that cannot work on this tier.
	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile (regression confirms filter), got %d", len(profiles))
	}
	if profiles[0].Name != "remote" || profiles[0].Type != "kubernetes" {
		t.Errorf("regression: expected remote/kubernetes, got %s/%s", profiles[0].Name, profiles[0].Type)
	}
	// This is the symptom: the ONLY available profile is kubernetes, which
	// cannot work on Cloud Run sandbox. autoSelectProfile() in the UI would
	// auto-select it because profiles.length === 1.
}

// TestBuildInfoProfiles_FallbackFires_Task92 verifies the architect's Shape B
// claim by execution: when ALL profiles are dropped by buildInfoProfiles
// (both local/docker and remote/kubernetes filtered out), the existing
// len(profiles)==0 fallback fires and returns exactly one synthetic profile
// {Name: "default", Type: defaultRuntimeType, Available: true}.
//
// With defaultRuntimeType="cloudrun-sandbox", this means the fallback produces
// the correct profile for the single-node tier. autoSelectProfile in the UI
// fires (length == 1), and the profile resolves to cloudrun-sandbox.
//
// This test also verifies the workstation case (docker broker) is unaffected
// by the filter: when the broker IS local, the filter condition
// !isLocalOnlyRuntime(defaultRuntimeType) is false, so NO profiles are
// dropped — kubernetes remains available as a legitimate product feature.
//
// These three facts were measured by a verification instrument that was
// deleted under a stop order (architect 03:08). This committed pin restores
// them as reproducible evidence.
func TestBuildInfoProfiles_FallbackFires_Task92(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Seed the ORIGINAL workstation defaults — the settings that exist
	// on a fresh deploy without the task-92 fix.
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  kubernetes:
    type: kubernetes
profiles:
  local:
    runtime: docker
  remote:
    runtime: kubernetes
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}

	// === FACT 1: Current filter behavior on cloudrun-sandbox broker ===
	// With workstation defaults, only remote/kubernetes survives the filter.
	// local/docker is dropped because isLocalOnlyRuntime("docker")=true and
	// the broker is non-local (!isLocalOnlyRuntime("cloudrun-sandbox")=true).
	profiles := srv.buildInfoProfiles("cloudrun-sandbox")
	if len(profiles) != 1 {
		t.Fatalf("FACT 1: expected 1 profile (remote/kubernetes only), got %d", len(profiles))
	}
	if profiles[0].Name != "remote" || profiles[0].Type != "kubernetes" {
		t.Errorf("FACT 1: expected remote/kubernetes, got %s/%s", profiles[0].Name, profiles[0].Type)
	}

	// === FACT 2: Filter predicate analysis ===
	// isLocalOnlyRuntime("kubernetes") is false — this is WHY kubernetes
	// survives the filter. The filter asks "is this local-only?" when it
	// should ask "can this broker serve this runtime?".
	if isLocalOnlyRuntime("kubernetes") {
		t.Fatal("FACT 2: isLocalOnlyRuntime('kubernetes') should be false — this is load-bearing")
	}
	if !isLocalOnlyRuntime("docker") {
		t.Fatal("FACT 2: isLocalOnlyRuntime('docker') should be true")
	}

	// === FACT 3: Workstation broker preserves all profiles ===
	// On a docker broker, !isLocalOnlyRuntime("docker") = !true = false,
	// so the filter condition is false for ALL profiles → nothing is dropped.
	// This means kubernetes IS available on a workstation, which is correct
	// (a workstation legitimately offers k8s as a product feature).
	workstationProfiles := srv.buildInfoProfiles("docker")
	byName := make(map[string]BrokerProfile, len(workstationProfiles))
	for _, p := range workstationProfiles {
		byName[p.Name] = p
	}
	if _, ok := byName["local"]; !ok {
		t.Error("FACT 3: local profile should be present on docker broker")
	}
	if _, ok := byName["remote"]; !ok {
		t.Error("FACT 3: remote profile should be present on docker broker — workstation legitimately offers k8s")
	}
	if len(workstationProfiles) < 2 {
		t.Errorf("FACT 3: expected >= 2 profiles on docker broker, got %d", len(workstationProfiles))
	}

	// === SUPPLEMENTARY: len(profiles)==0 fallback produces correct result ===
	// The existing fallback at handlers.go:226-230 returns:
	//   {Name: "default", Type: defaultRuntimeType, Available: true}
	// To exercise this, seed settings with ONLY local-only profiles. On a
	// cloudrun-sandbox broker, all local-only profiles are dropped → the
	// filter loop produces an empty list → the fallback fires.
	fallbackDir := t.TempDir()
	fallbackScionDir := filepath.Join(fallbackDir, ".scion")
	if err := os.MkdirAll(fallbackScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Override ALL profiles (including embedded defaults' "remote") to
	// local-only runtimes. koanf merge overwrites nested map values, so
	// profiles.remote.runtime = podman replaces the embedded kubernetes.
	localOnlyYAML := `schema_version: "1"
active_profile: local
runtimes:
  docker:
    type: docker
  podman:
    type: podman
profiles:
  local:
    runtime: docker
  remote:
    runtime: podman
`
	if err := os.WriteFile(filepath.Join(fallbackScionDir, "settings.yaml"), []byte(localOnlyYAML), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Setenv("HOME", fallbackDir)
	// Note: origHome defer from the top of this function will restore HOME.

	fallbackProfiles := srv.buildInfoProfiles("cloudrun-sandbox")
	if len(fallbackProfiles) != 1 {
		t.Fatalf("FALLBACK: expected 1 profile from len==0 fallback, got %d", len(fallbackProfiles))
	}
	if fallbackProfiles[0].Name != "default" {
		t.Errorf("FALLBACK: expected name 'default', got %q", fallbackProfiles[0].Name)
	}
	if fallbackProfiles[0].Type != "cloudrun-sandbox" {
		t.Errorf("FALLBACK: expected type 'cloudrun-sandbox', got %q", fallbackProfiles[0].Type)
	}
}
