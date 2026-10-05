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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeysCmd_RequiresExactArgs(t *testing.T) {
	// No args — should fail
	err := keysCmd.Args(keysCmd, []string{})
	require.Error(t, err)

	// One arg — should fail
	err = keysCmd.Args(keysCmd, []string{"agent1"})
	require.Error(t, err)

	// Two args — should pass
	err = keysCmd.Args(keysCmd, []string{"agent1", "Escape"})
	require.NoError(t, err)

	// Three args — should fail (ExactArgs(2))
	err = keysCmd.Args(keysCmd, []string{"agent1", "Escape", "extra"})
	require.Error(t, err)
}

func TestKeysCmd_HasCorrectUse(t *testing.T) {
	assert.Equal(t, "keys <agent-name> <keystrokes>", keysCmd.Use)
}

func TestKeysCmd_IsRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "keys" {
			found = true
			break
		}
	}
	assert.True(t, found, "keys command should be registered on rootCmd")
}

// ---------------------------------------------------------------------------
// Hub-aware keys: sendKeysViaHub must POST {"keys": ...} to the dedicated
// /keys route (never /message) for both agent
// and human senders, and must never retry or fall back on failure.
// ---------------------------------------------------------------------------

type capturedKeysRequest struct {
	Method string
	Path   string
	Keys   string
}

// newKeysMockHubServer builds a Hub mock that answers only the project-scoped
// /keys route, recording each request's path and decoded body.
func newKeysMockHubServer(t *testing.T, projectID string, resp agentkeys.Response, status int) (*httptest.Server, *[]capturedKeysRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedKeysRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost:
			var body agentkeys.Request
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			captured = append(captured, capturedKeysRequest{Method: r.Method, Path: r.URL.Path, Keys: body.Keys})
			mu.Unlock()
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return server, &captured
}

func TestSendKeysViaHub_PostsToKeysRoute(t *testing.T) {
	projectID := "project-keys-route"
	resp := agentkeys.Response{Status: agentkeys.StatusDispatched, OperationID: "op-1", AgentID: "agent-id-1"}
	server, captured := newKeysMockHubServer(t, projectID, resp, http.StatusOK)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.NoError(t, err)

	require.Len(t, *captured, 1)
	got := (*captured)[0]
	assert.Equal(t, "/api/v1/projects/"+projectID+"/agents/target-agent/keys", got.Path,
		"keys must POST to the dedicated /keys route, never /message")
	assert.Equal(t, "Escape", got.Keys)
}

func TestSendKeysViaHub_ErrorWrapsHubError(t *testing.T) {
	projectID := "project-keys-error"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "agent_not_running",
				"message": "target is not running",
				"details": map[string]interface{}{"operation_id": "op-err"},
			},
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent_not_running")

	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr), "expected the outcome code/operation_id to survive as a wrapped *apiclient.APIError")
	assert.Equal(t, "agent_not_running", apiErr.Code)
	require.NotNil(t, apiErr.Details)
	assert.Equal(t, "op-err", apiErr.Details["operation_id"])
}

// TestSendKeysViaHub_NoReplay proves the CLI's hub path, end to end, sends
// the keys request exactly once even when the client is built with retries
// and the server answers with a 5xx — the binding "never replayed" guarantee
// exercised at the command layer, not just inside hubclient.
func TestSendKeysViaHub_NoReplay(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL, hubclient.WithRetry(5, time.Millisecond))
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-no-replay"}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.Error(t, err)
	assert.EqualValues(t, 1, atomic.LoadInt32(&hits), "scion keys must never replay even with client-side retries configured")
}

// ---------------------------------------------------------------------------
// Cross-project refusal (UX layer): keys must not work as a cross-project
// command. The authoritative refusal is hub-side (ExecuteAgentKeys); this is
// the CLI-side check that fails fast without a round trip.
// ---------------------------------------------------------------------------

func TestKeysCmd_RunE_CrossProjectTarget_Refused(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	// Hermetic: a regression in the cross-project refusal would otherwise
	// let this reach a live Hub (this test process's own ambient container
	// sets a real SCION_HUB_ENDPOINT/credentials) before the refusal fires.
	server, hits := newCountingHubServer(t)
	defer server.Close()
	setHermeticHubEnv(t, server)
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	t.Setenv("SCION_PROJECT", "own-project")

	cmd := &cobra.Command{Use: "keys"}
	cmd.Flags().StringVarP(&projectPath, "project", "g", "", "")
	require.NoError(t, cmd.Flags().Set("project", "other-project"))

	err := keysCmd.RunE(cmd, []string{"target-agent", "Escape"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-project")
	assert.EqualValues(t, 0, atomic.LoadInt32(hits), "the refusal must fire before any Hub request")
}

// ---------------------------------------------------------------------------
// Local-mode project isolation (AC): a fixture with the same agent name in
// two local projects must resolve uniquely within the *selected* project and
// fail ambiguity rather than falling back to an unscoped first match.
// ---------------------------------------------------------------------------

// writeLocalProjectSettings creates a minimal .scion project directory whose
// settings.json carries the given Hub-linked project ID — the same identity
// pkg/agent/run.go labels a locally-started container with (settings.Hub.ProjectID),
// independent of the Hub endpoint being reachable.
func writeLocalProjectSettings(t *testing.T, dir, hubProjectID string) string {
	t.Helper()
	scionDir := filepath.Join(dir, ".scion")
	require.NoError(t, os.MkdirAll(scionDir, 0755))
	// Deliberately no "schema_version" field — matches the working pattern
	// already established by setupEnvProjectWithHubProjectID in
	// hub_env_test.go. (Adding one routes this fixture through a different,
	// unrelated versioned-settings load path that errors on an ambient
	// default; omitting it is the tested, stable shape.)
	settings := map[string]interface{}{
		"project_id": "test-project-" + hubProjectID,
		"hub": map[string]interface{}{
			"projectId": hubProjectID,
		},
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.json"), data, 0644))
	return dir
}

// filteringMockRuntime wraps runtime.MockRuntime's List with real label
// filtering semantics (every filter key/value must match the entry's
// Labels), matching how a real container runtime's label filter behaves.
// The sendkeys_test.go mocks in pkg/agent ignore the filter entirely because
// resolution there is scoped by the test setting up exactly the agents it
// wants returned; here the whole point under test is that the filter itself
// enforces project isolation, so a faithful mock is the test.
func filteringMockRuntime(agents []api.AgentInfo, exec func(id string, cmd []string)) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			var out []api.AgentInfo
			for _, a := range agents {
				if runtime.LabelsMatchFilter(a.Labels, filter) {
					out = append(out, a)
				}
			}
			return out, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			if exec != nil {
				exec(id, cmd)
			}
			return "", nil
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			if exec != nil {
				exec(id, cmd)
			}
			return "", nil
		},
	}
}

func twoProjectSameNameAgents(projA, projB string) []api.AgentInfo {
	return []api.AgentInfo{
		{
			Name:        "builder",
			ContainerID: "container-a",
			ProjectID:   projA,
			Phase:       string(state.PhaseRunning),
			Labels: map[string]string{
				"scion.name":               "builder",
				"agent_id":                 "agent-a",
				projectkeys.LabelProjectID: projA,
			},
		},
		{
			Name:        "builder",
			ContainerID: "container-b",
			ProjectID:   projB,
			Phase:       string(state.PhaseRunning),
			Labels: map[string]string{
				"scion.name":               "builder",
				"agent_id":                 "agent-b",
				projectkeys.LabelProjectID: projB,
			},
		},
	}
}

func TestResolveLocalKeysTarget_SameNameTwoProjects_ResolvesSelectedProjectOnly(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp) // isolate from any ambient ~/.scion global settings
	dirA := writeLocalProjectSettings(t, filepath.Join(tmp, "proj-a"), "proj-a-id")
	writeLocalProjectSettings(t, filepath.Join(tmp, "proj-b"), "proj-b-id")

	agents := twoProjectSameNameAgents("proj-a-id", "proj-b-id")
	mockRT := filteringMockRuntime(agents, nil)
	mgr := agent.NewManager(mockRT)
	defer mgr.Close()

	projectPath = dirA
	target, scope, err := resolveLocalKeysTarget(context.Background(), mgr, "builder")
	require.NoError(t, err)
	assert.Equal(t, "proj-a-id", scope.hubProjectID)
	assert.Empty(t, scope.projectPath, "a Hub-linked project must scope by project ID, not path")
	assert.Equal(t, "container-a", target.ContainerID, "must resolve to project A's agent, not project B's same-named one")
	assert.Equal(t, "agent-a", target.Labels["agent_id"])
}

func TestResolveLocalKeysTarget_SelectsOtherProjectWhenSwitched(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	writeLocalProjectSettings(t, filepath.Join(tmp, "proj-a"), "proj-a-id")
	dirB := writeLocalProjectSettings(t, filepath.Join(tmp, "proj-b"), "proj-b-id")

	agents := twoProjectSameNameAgents("proj-a-id", "proj-b-id")
	mockRT := filteringMockRuntime(agents, nil)
	mgr := agent.NewManager(mockRT)
	defer mgr.Close()

	projectPath = dirB
	target, scope, err := resolveLocalKeysTarget(context.Background(), mgr, "builder")
	require.NoError(t, err)
	assert.Equal(t, "proj-b-id", scope.hubProjectID)
	assert.Equal(t, "container-b", target.ContainerID)
	assert.Equal(t, "agent-b", target.Labels["agent_id"])
}

// TestResolveLocalKeysTarget_NoHubProjectID_UsesPathScope proves that a
// project with NO Hub-linked project ID
// must still resolve successfully, scoped by its resolved project-config
// directory path (agent.Manager.SendKeysLocal's scope) rather than being
// refused outright.
func TestResolveLocalKeysTarget_NoHubProjectID_UsesPathScope(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// A project directory with no settings.json at all: no Hub-linked
	// project ID exists. resolveLocalKeysTarget must still resolve by path.
	unlinkedDir := filepath.Join(tmp, "unlinked")
	require.NoError(t, os.MkdirAll(unlinkedDir, 0755))
	resolvedDir, err := filepath.EvalSymlinks(unlinkedDir)
	require.NoError(t, err)

	agents := []api.AgentInfo{
		{
			Name:        "builder",
			ContainerID: "container-local",
			Phase:       string(state.PhaseRunning),
			Labels: map[string]string{
				"scion.name":                 "builder",
				"agent_id":                   "agent-local",
				projectkeys.LabelProjectPath: resolvedDir,
			},
		},
	}
	mockRT := filteringMockRuntime(agents, nil)
	mgr := agent.NewManager(mockRT)
	defer mgr.Close()

	projectPath = unlinkedDir
	target, scope, err := resolveLocalKeysTarget(context.Background(), mgr, "builder")
	require.NoError(t, err)
	assert.Empty(t, scope.hubProjectID, "an unlinked project must not invent a Hub project ID")
	assert.Equal(t, resolvedDir, scope.projectPath)
	assert.Equal(t, "container-local", target.ContainerID)
	assert.Equal(t, "agent-local", target.Labels["agent_id"])
}

// TestResolveLocalKeysTarget_NoProjectAtAll_FailsClearly covers the one
// remaining hard refusal: no Hub-linked project ID AND no resolvable
// project directory at all (e.g. run outside any scion project with no
// --project given) has no stable identity to scope the call to.
func TestResolveLocalKeysTarget_NoProjectAtAll_FailsClearly(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	mgr := agent.NewManager(filteringMockRuntime(nil, nil))
	defer mgr.Close()

	// No project marker reachable from cwd (a fresh temp dir), HOME unset
	// so even the global-dir fallback cannot resolve, and every hub-context
	// env var cleared — this test process's own ambient container sets
	// SCION_HUB_ENDPOINT/SCION_PROJECT_ID, which would otherwise make
	// config.IsHubContext() true and FindProjectRoot synthesize a path
	// anyway (see FindProjectRoot's "Hub context fallback"). This is the
	// one combination that makes config.GetResolvedProjectDir return "".
	t.Chdir(t.TempDir())
	t.Setenv("HOME", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_PROJECT_ID", "")

	projectPath = ""
	_, _, err := resolveLocalKeysTarget(context.Background(), mgr, "builder")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not resolve a project")
}

func TestResolveLocalKeysTarget_NotFoundInSelectedProject(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dirA := writeLocalProjectSettings(t, filepath.Join(tmp, "proj-a"), "proj-a-id")

	// Only project B has an agent named "builder".
	agents := twoProjectSameNameAgents("proj-a-id", "proj-b-id")[1:]
	mockRT := filteringMockRuntime(agents, nil)
	mgr := agent.NewManager(mockRT)
	defer mgr.Close()

	projectPath = dirA
	_, _, err := resolveLocalKeysTarget(context.Background(), mgr, "builder")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// writeUnlinkedProjectWithCreatedAgents creates an unlinked local project
// (a .scion directory with no Hub-linked project ID) containing on-disk
// created agents with no container, and returns the project directory to
// pass as --project. These agents are found only by agent.List's
// created-agent scan, never by the runtime layer.
func writeUnlinkedProjectWithCreatedAgents(t *testing.T, root string, names ...string) string {
	t.Helper()
	dir := filepath.Join(root, "unlinked")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".scion"), 0755))
	resolved, err := config.GetResolvedProjectDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, resolved)
	for _, n := range names {
		agentDir := filepath.Join(resolved, "agents", n)
		require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte("{}"), 0644))
	}
	return dir
}

// TestResolveLocalKeysTarget_UnlinkedCreatedAgents covers name resolution
// in an unlinked local project whose agents exist only on disk (no
// container): a name matching one of them is "exists but is not running",
// never a delivery target, and an unknown name is "not found" rather than
// "ambiguous" or the project's sole agent.
func TestResolveLocalKeysTarget_UnlinkedCreatedAgents(t *testing.T) {
	tests := []struct {
		name       string
		agents     []string
		target     string
		notRunning bool
		wantErrIn  string
	}{
		{name: "created agent among two", agents: []string{"builder", "reviewer"}, target: "reviewer", notRunning: true, wantErrIn: "agent 'reviewer' exists in project"},
		{name: "sole created agent", agents: []string{"builder"}, target: "builder", notRunning: true, wantErrIn: "is not running"},
		{name: "unknown name among two agents", agents: []string{"builder", "reviewer"}, target: "missing", wantErrIn: "not found"},
		{name: "unknown name with a single agent", agents: []string{"builder"}, target: "missing", wantErrIn: "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origProjectPath := projectPath
			defer func() { projectPath = origProjectPath }()

			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			projectPath = writeUnlinkedProjectWithCreatedAgents(t, tmp, tt.agents...)

			mgr := agent.NewManager(filteringMockRuntime(nil, nil))
			defer mgr.Close()

			target, _, err := resolveLocalKeysTarget(context.Background(), mgr, tt.target)
			require.Error(t, err)
			assert.Empty(t, target.Name, "an error must not carry a target")
			assert.Contains(t, err.Error(), tt.wantErrIn)
			assert.NotContains(t, err.Error(), "ambiguous")
			assert.Equal(t, tt.notRunning, errors.Is(err, agentkeys.ErrAgentNotRunning))
			if tt.notRunning {
				assert.NotContains(t, err.Error(), "not found")
			}
		})
	}
}

// TestResolveLocalKeysTarget_RunningContainerWithCreatedSibling covers an
// unlinked project holding one running container and one on-disk created
// sibling: the running name resolves to the container, and the created
// name is "not running" rather than resolving to the container.
func TestResolveLocalKeysTarget_RunningContainerWithCreatedSibling(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	projectPath = writeUnlinkedProjectWithCreatedAgents(t, tmp, "reviewer")
	resolved, err := config.GetResolvedProjectDir(projectPath)
	require.NoError(t, err)

	running := []api.AgentInfo{{
		Name:        "builder",
		ContainerID: "container-builder",
		Phase:       string(state.PhaseRunning),
		Labels: map[string]string{
			"scion.agent":                "true",
			"scion.name":                 "builder",
			"agent_id":                   "agent-builder",
			projectkeys.LabelProjectPath: resolved,
		},
	}}
	mgr := agent.NewManager(filteringMockRuntime(running, nil))
	defer mgr.Close()

	target, scope, err := resolveLocalKeysTarget(context.Background(), mgr, "builder")
	require.NoError(t, err)
	assert.Equal(t, "container-builder", target.ContainerID)
	assert.Equal(t, "agent-builder", target.Labels["agent_id"])
	assert.Empty(t, scope.hubProjectID)
	assert.Equal(t, resolved, scope.projectPath)

	target, _, err = resolveLocalKeysTarget(context.Background(), mgr, "reviewer")
	require.Error(t, err)
	assert.ErrorIs(t, err, agentkeys.ErrAgentNotRunning)
	assert.Contains(t, err.Error(), "agent 'reviewer' exists in project")
	assert.Empty(t, target.ContainerID, "the created sibling must never resolve to the running container")
}

// TestResolveLocalKeysTarget_HubLinkedCreatedAgent covers a Hub-linked
// project, whose lookup is scoped by project ID and so never sees on-disk
// created agents directly: a name matching one still reports "not
// running", and an unknown name still reports "not found".
func TestResolveLocalKeysTarget_HubLinkedCreatedAgent(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir := writeLocalProjectSettings(t, filepath.Join(tmp, "linked"), "linked-id")
	resolved, err := config.GetResolvedProjectDir(dir)
	require.NoError(t, err)
	agentDir := filepath.Join(resolved, "agents", "reviewer")
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte("{}"), 0644))
	projectPath = dir

	mgr := agent.NewManager(filteringMockRuntime(nil, nil))
	defer mgr.Close()

	_, _, err = resolveLocalKeysTarget(context.Background(), mgr, "reviewer")
	require.Error(t, err)
	assert.ErrorIs(t, err, agentkeys.ErrAgentNotRunning)
	assert.Contains(t, err.Error(), "is not running")

	_, _, err = resolveLocalKeysTarget(context.Background(), mgr, "missing")
	require.Error(t, err)
	assert.NotErrorIs(t, err, agentkeys.ErrAgentNotRunning)
	assert.Contains(t, err.Error(), "not found")
}

// TestSendKeysLocal_CreatedOnlyTarget_RejectsAsNotRunning covers the
// delivery entry point: a created-only target is rejected with the
// agent_not_running code and no keystrokes are executed anywhere.
func TestSendKeysLocal_CreatedOnlyTarget_RejectsAsNotRunning(t *testing.T) {
	origProjectPath := projectPath
	origFormat := outputFormat
	defer func() { projectPath = origProjectPath; outputFormat = origFormat }()
	outputFormat = "json"

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	projectPath = writeUnlinkedProjectWithCreatedAgents(t, tmp, "reviewer")

	var execs []string
	mgr := agent.NewManager(filteringMockRuntime(nil, func(id string, _ []string) { execs = append(execs, id) }))
	defer mgr.Close()

	var sendErr error
	stdout := captureStdout(t, func() {
		sendErr = sendKeysLocalWithManager(context.Background(), mgr, "reviewer", "Escape")
	})
	require.Error(t, sendErr)
	assert.Empty(t, execs, "no keystrokes may be delivered to a created-only agent")
	assert.Contains(t, stdout, string(agentkeys.OutcomeAgentNotRunning))
	assert.NotContains(t, stdout, string(agentkeys.OutcomeNotFound))
}

// ---------------------------------------------------------------------------
// JSON mode (AC): `scion keys` in JSON mode must emit exactly one parseable
// result on stdout, with no progress text mixed in.
// ---------------------------------------------------------------------------

func TestSendKeysViaHub_JSONMode_EmitsExactlyOneParseableResult(t *testing.T) {
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()
	outputFormat = "json"

	projectID := "project-keys-json"
	resp := agentkeys.Response{Status: agentkeys.StatusDispatched, OperationID: "op-json-1", AgentID: "agent-json-1"}
	server, _ := newKeysMockHubServer(t, projectID, resp, http.StatusOK)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	var cmdErr error
	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() {
			cmdErr = sendKeysViaHub(hubCtx, "target-agent", "Escape")
		})
		assert.Empty(t, stderr, "JSON mode must suppress progress/status text entirely")
	})
	require.NoError(t, cmdErr)

	// Exactly one JSON value: a Decoder that successfully reads one value
	// and then reports io.EOF (not another value) proves there is nothing
	// else on stdout — no progress text, no second result.
	dec := json.NewDecoder(strings.NewReader(stdout))
	var result ActionResult
	require.NoError(t, dec.Decode(&result), "stdout must contain exactly one parseable JSON result")
	assert.Equal(t, "success", result.Status)
	assert.Equal(t, "keys", result.Command)
	assert.Equal(t, "target-agent", result.Agent)
	require.NotNil(t, result.Details)
	assert.Equal(t, "op-json-1", result.Details["operation_id"])

	var extra json.RawMessage
	err = dec.Decode(&extra)
	assert.ErrorIs(t, err, io.EOF, "stdout must contain nothing after the single JSON result")
}

// TestSendKeysViaHub_OldHub_FailsClearly_NeverFallsBackToMessage proves the
// binding obligation "never auto-fall back to raw messaging against an old
// Hub; fail clearly": an old Hub with no /keys route at all answers with a
// plain, non-agentkeys-shaped 404 (the generic mux "not found", not the
// keys handler's own envelope). sendKeysViaHub must surface a clear error —
// never silently retry through /message, even though this mock server would
// happily accept that request and report success if it were ever made.
func TestSendKeysViaHub_OldHub_FailsClearly_NeverFallsBackToMessage(t *testing.T) {
	var messageRouteHit int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case strings.HasSuffix(r.URL.Path, "/keys"):
			// An old Hub's generic action dispatcher: a 404 in the same
			// envelope every other Hub route uses, naming the action rather
			// than the keys outcome — it has never heard of this route, so
			// no operation_id is ever minted.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "not_found",
					"message": "Action not found",
				},
			})
		case strings.HasSuffix(r.URL.Path, "/message"):
			atomic.AddInt32(&messageRouteHit, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-old-hub"}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.Error(t, err, "an old Hub without /keys must fail clearly, not silently succeed")
	assert.EqualValues(t, 0, atomic.LoadInt32(&messageRouteHit),
		"must never fall back to /message when /keys is unavailable")
	assert.Contains(t, err.Error(), "does not support the keys operation, or the project was not found",
		"a 404 with no operation_id (old Hub, or AK-21e's project-not-found) must be reported as this shared shape, not a generic failure")
}

// ---------------------------------------------------------------------------
// sendKeysLocalWithManager end to end, with a mock manager/runtime, proving
// the exact delivery payload (AC2) reaches the runtime with no added Enter.
// ---------------------------------------------------------------------------

// cmdExecRecord captures one Exec/ExecWithStdin call (target container ID,
// argv, and the full stdin payload) for the end-to-end delivery assertions
// below — unlike filteringMockRuntime's exec callback, which only reports
// argv.
type cmdExecRecord struct {
	id    string
	argv  string
	stdin string
}

// filteringMockRuntimeWithStdin is filteringMockRuntime's sibling for tests
// that need to inspect the exact stdin payload SendKeys/SendKeysLocal
// delivers (the tmux script) and which container it targeted, not just argv.
func filteringMockRuntimeWithStdin(agents []api.AgentInfo, captured *[]cmdExecRecord) *runtime.MockRuntime {
	record := func(id string, cmd []string, stdin io.Reader) {
		data := ""
		if stdin != nil {
			buf := make([]byte, 4096)
			n, _ := stdin.Read(buf)
			data = string(buf[:n])
		}
		*captured = append(*captured, cmdExecRecord{id: id, argv: strings.Join(cmd, " "), stdin: data})
	}
	return &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			var out []api.AgentInfo
			for _, a := range agents {
				if runtime.LabelsMatchFilter(a.Labels, filter) {
					out = append(out, a)
				}
			}
			return out, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			record(id, cmd, nil)
			return "", nil
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			record(id, cmd, stdin)
			return "", nil
		},
	}
}

// expectedSendKeysScript is a golden reimplementation of pkg/agent's
// unexported sendKeysScript/tmuxOctalEscape (.design/agent-keys-contract.md
// §2.3: one tmux send-keys command, keys embedded as a fully octal-escaped,
// double-quoted argument), used to assert the exact delivered payload from
// outside that package.
func expectedSendKeysScript(target, keys string) string {
	const octalDigits = "01234567"
	var b strings.Builder
	for i := 0; i < len(keys); i++ {
		c := keys[i]
		b.WriteByte('\\')
		b.WriteByte(octalDigits[(c>>6)&07])
		b.WriteByte(octalDigits[(c>>3)&07])
		b.WriteByte(octalDigits[c&07])
	}
	return fmt.Sprintf("send-keys -t %s -- \"%s\"\n", target, b.String())
}

func TestSendKeysLocalWithManager_HubLinkedProject_ExactDelivery(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dirA := writeLocalProjectSettings(t, filepath.Join(tmp, "proj-a"), "proj-a-id")

	agent1 := twoProjectSameNameAgents("proj-a-id", "proj-b-id")[0]
	var captured []cmdExecRecord
	mockRT := filteringMockRuntimeWithStdin([]api.AgentInfo{agent1}, &captured)
	mgr := agent.NewManager(mockRT)
	defer mgr.Close()

	projectPath = dirA
	err := sendKeysLocalWithManager(context.Background(), mgr, "builder", "Escape")
	require.NoError(t, err)

	require.NotEmpty(t, captured)
	last := captured[len(captured)-1]
	assert.Equal(t, "container-a", last.id, "delivery must target the resolved container")
	assert.Equal(t, "tmux source-file -", last.argv)
	assert.Equal(t, expectedSendKeysScript("scion:0", "Escape"), last.stdin,
		"the exact delivered keys must match byte for byte (an exact match also proves no Enter was appended)")
}

func TestSendKeysLocalWithManager_UnlinkedProject_ExactDelivery(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	unlinkedDir := filepath.Join(tmp, "unlinked")
	require.NoError(t, os.MkdirAll(unlinkedDir, 0755))
	resolvedDir, err := filepath.EvalSymlinks(unlinkedDir)
	require.NoError(t, err)

	fixture := api.AgentInfo{
		Name:        "builder",
		ContainerID: "container-local",
		Phase:       string(state.PhaseRunning),
		Labels: map[string]string{
			"scion.name":                 "builder",
			"agent_id":                   "agent-local",
			projectkeys.LabelProjectPath: resolvedDir,
		},
	}
	var captured []cmdExecRecord
	mockRT := filteringMockRuntimeWithStdin([]api.AgentInfo{fixture}, &captured)
	mgr := agent.NewManager(mockRT)
	defer mgr.Close()

	projectPath = unlinkedDir
	err = sendKeysLocalWithManager(context.Background(), mgr, "builder", "C-c")
	require.NoError(t, err)

	require.NotEmpty(t, captured)
	last := captured[len(captured)-1]
	assert.Equal(t, "container-local", last.id, "delivery must target the resolved container")
	assert.Equal(t, "tmux source-file -", last.argv)
	assert.Equal(t, expectedSendKeysScript("scion:0", "C-c"), last.stdin,
		"the exact delivered keys must match byte for byte (an exact match also proves no Enter was appended)")
}

// ---------------------------------------------------------------------------
// An empty keys body must be rejected locally before any network call
// (Hub) or List call (local), not sent through to be rejected server-side.
// ---------------------------------------------------------------------------

func TestSendKeysViaHub_EmptyKeys_RejectedBeforeNetworkCall(t *testing.T) {
	var hit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-empty"}

	err = sendKeysViaHub(hubCtx, "target-agent", "")
	require.Error(t, err)
	assert.False(t, hit, "an empty keys value must be rejected locally, before any request reaches the Hub")
}

func TestSendKeysLocalWithManager_EmptyKeys_RejectedBeforeListCall(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	var listCalled bool
	mgr := agent.NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			listCalled = true
			return nil, nil
		},
	})
	defer mgr.Close()

	err := sendKeysLocalWithManager(context.Background(), mgr, "target-agent", "")
	require.Error(t, err)
	assert.False(t, listCalled, "an empty keys value must be rejected before any target resolution")
}

// ---------------------------------------------------------------------------
// A 5xx with no contract body (or a 503 from something other than the keys
// handler itself) must classify as unknown, never a definite rejection.
// ---------------------------------------------------------------------------

func newKeysMockHubServerFailing(t *testing.T, contentType string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
}

func TestSendKeysViaHub_BareHTML5xx_IsUnknownNeverRejected(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"502_bad_gateway", http.StatusBadGateway},
		{"504_gateway_timeout", http.StatusGatewayTimeout},
		{"500_internal_server_error", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newKeysMockHubServerFailing(t, "text/html", tc.status, "<html><body>gateway error</body></html>")
			defer server.Close()

			client, err := hubclient.New(server.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-bare-5xx"}

			err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "keys unknown for agent",
				"a bare HTML 5xx with no contract body must classify as unknown, never a definite rejection")
			assert.NotContains(t, err.Error(), "keys rejected",
				"must never be reported as safe to retry")
		})
	}
}

// TestSendKeysViaHub_Bodyless502_ClearErrorNeverEmptyCode proves a 502
// with no body at all (a proxy or load balancer in front of the Hub) gives
// a clear "unknown" error, a failing command (and therefore a non-zero
// exit code from Execute), and a non-empty outcome code in both text and
// JSON mode.
func TestSendKeysViaHub_Bodyless502_ClearErrorNeverEmptyCode(t *testing.T) {
	for _, format := range []string{"", "json"} {
		t.Run("format="+format, func(t *testing.T) {
			origFormat := outputFormat
			defer func() { outputFormat = origFormat }()
			outputFormat = format

			server := newKeysMockHubServerFailing(t, "", http.StatusBadGateway, "")
			defer server.Close()

			client, err := hubclient.New(server.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-bodyless-502"}

			var cmdErr error
			stdout := captureStdout(t, func() {
				cmdErr = sendKeysViaHub(hubCtx, "target-agent", "Escape")
			})
			require.Error(t, cmdErr, "a bodyless 502 must fail the command so Execute exits non-zero")
			assert.Contains(t, cmdErr.Error(), "keys unknown for agent 'target-agent'")
			assert.Contains(t, cmdErr.Error(), "502")
			assert.Contains(t, cmdErr.Error(), "check before resending")
			assert.NotContains(t, cmdErr.Error(), ": :", "no empty fields in the error text")
			// apiclient fills a bodyless 502 with the generic internal_error
			// code; the CLI must replace it with the keys outcome.
			assert.Contains(t, cmdErr.Error(), "no keys outcome from the Hub")

			if format == "json" {
				var result ActionResult
				require.NoError(t, json.Unmarshal([]byte(stdout), &result))
				assert.Equal(t, "error", result.Status)
				assert.Equal(t, "unknown", result.Details["outcome"])
				assert.Equal(t, string(agentkeys.OutcomeKeysOutcomeUnknown), result.Details["code"],
					"a bodyless 502 must report keys_outcome_unknown, not the client's generic internal_error")
				assert.Contains(t, result.Message, "HTTP 502 Bad Gateway")
			}
		})
	}
}

// TestClassifyHubKeysError_EmptyCodeNeverSurfaces covers an API error that
// carries no code and no message at all, the shape a proxy can produce:
// the classified result always has a non-empty code and message.
func TestClassifyHubKeysError_EmptyCodeNeverSurfaces(t *testing.T) {
	cases := []struct {
		status      int
		wantOutcome keysOutcomeStatus
		wantCode    string
	}{
		{http.StatusBadGateway, keysOutcomeUnknown, string(agentkeys.OutcomeKeysOutcomeUnknown)},
		{http.StatusGatewayTimeout, keysOutcomeUnknown, string(agentkeys.OutcomeKeysOutcomeUnknown)},
		{http.StatusServiceUnavailable, keysOutcomeUnknown, string(agentkeys.OutcomeKeysOutcomeUnknown)},
		{http.StatusBadRequest, keysOutcomeRejected, "http_400"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			res := classifyHubKeysError(&apiclient.APIError{StatusCode: tc.status})
			assert.Equal(t, tc.wantOutcome, res.Outcome)
			assert.Equal(t, tc.wantCode, res.Code)
			assert.NotEmpty(t, res.Message)
		})
	}
}

// TestClassifyHubKeysError_GenericCodeWithoutOperationID covers the shapes
// apiclient.ParseErrorResponse really produces for a response that did not
// come from the keys handler: a generic status-derived or proxy code and no
// operation_id. An ambiguous status maps to keys_outcome_unknown; a keys
// outcome code, or a response carrying an operation_id, keeps its code.
func TestClassifyHubKeysError_GenericCodeWithoutOperationID(t *testing.T) {
	withOp := map[string]interface{}{"operation_id": "op-1"}
	cases := []struct {
		name        string
		err         *apiclient.APIError
		wantOutcome keysOutcomeStatus
		wantCode    string
	}{
		{"bodyless 502", &apiclient.APIError{StatusCode: 502, Code: "internal_error", Message: "Bad Gateway"}, keysOutcomeUnknown, "keys_outcome_unknown"},
		{"bodyless 504", &apiclient.APIError{StatusCode: 504, Code: "internal_error", Message: "Gateway Timeout"}, keysOutcomeUnknown, "keys_outcome_unknown"},
		{"proxy 503", &apiclient.APIError{StatusCode: 503, Code: "service_unavailable", Message: "upstream unhealthy"}, keysOutcomeUnknown, "keys_outcome_unknown"},
		{"hub 500 with operation_id", &apiclient.APIError{StatusCode: 500, Code: "internal_error", Message: "unexpected", Details: withOp}, keysOutcomeUnknown, "internal_error"},
		{"hub keys_outcome_unknown", &apiclient.APIError{StatusCode: 502, Code: "keys_outcome_unknown", Message: "broker failed", Details: withOp}, keysOutcomeUnknown, "keys_outcome_unknown"},
		{"definite 400 keeps generic code", &apiclient.APIError{StatusCode: 400, Code: "invalid_request", Message: "Bad Request"}, keysOutcomeRejected, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := classifyHubKeysError(tc.err)
			assert.Equal(t, tc.wantOutcome, res.Outcome)
			assert.Equal(t, tc.wantCode, res.Code)
			assert.NotEmpty(t, res.Message)
		})
	}
}

func TestSendKeysViaHub_JSON500InternalError_IsUnknownNotRejected(t *testing.T) {
	server := newKeysMockHubServerFailing(t, "application/json", http.StatusInternalServerError,
		`{"error":{"code":"internal_error","message":"unexpected error"}}`)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-500-json"}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys unknown for agent",
		"a 500 internal_error must classify as unknown, not a definite rejection")
	assert.Contains(t, err.Error(), "check before resending",
		"an unknown outcome must tell the caller a resend could be a second injection, never invite a blind retry")
}

func TestSendKeysViaHub_503WithoutKeysUnavailableCode_IsUnknownNotRejected(t *testing.T) {
	// A proxy or load balancer's own 503 (e.g. "service_unavailable" from an
	// upstream health check), not the Hub's own keys_unavailable: only the
	// Hub's own, explicit keys_unavailable 503 proves dispatch definitively
	// did not start. Anything else wearing a 503 must stay unknown.
	server := newKeysMockHubServerFailing(t, "application/json", http.StatusServiceUnavailable,
		`{"error":{"code":"service_unavailable","message":"upstream unhealthy"}}`)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-503-generic"}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys unknown for agent",
		"a 503 not carrying the Hub's own keys_unavailable code must classify as unknown")
}

func TestSendKeysViaHub_503WithKeysUnavailableCode_IsDefiniteRejection(t *testing.T) {
	// Control: the Hub's own keys_unavailable 503 IS a definite rejection
	// (contract §2.5: "dispatch definitively did not start").
	server := newKeysMockHubServerFailing(t, "application/json", http.StatusServiceUnavailable,
		`{"error":{"code":"keys_unavailable","message":"no broker route","details":{"operation_id":"op-503"}}}`)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-503-keys-unavailable"}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys rejected for agent",
		"the Hub's own keys_unavailable 503 is a definite non-delivery, safe to classify as rejected")
}

// TestSendKeysViaHub_JSONMode_FailureEmitsOneResultWithUnknownOutcome proves
// JSON mode emits exactly one parseable result on failure too, with the
// outcome visible in Details.
func TestSendKeysViaHub_JSONMode_FailureEmitsOneResultWithUnknownOutcome(t *testing.T) {
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()
	outputFormat = "json"

	server := newKeysMockHubServerFailing(t, "text/html", http.StatusBadGateway, "<html>gateway</html>")
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-json-fail"}

	var cmdErr error
	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() {
			cmdErr = sendKeysViaHub(hubCtx, "target-agent", "Escape")
		})
		assert.Empty(t, stderr, "JSON mode must suppress progress text entirely, including on failure")
	})
	require.Error(t, cmdErr)

	dec := json.NewDecoder(strings.NewReader(stdout))
	var result ActionResult
	require.NoError(t, dec.Decode(&result), "stdout must contain exactly one parseable JSON result even on failure")
	assert.Equal(t, "error", result.Status)
	require.NotNil(t, result.Details)
	assert.Equal(t, "unknown", result.Details["outcome"])

	var extra json.RawMessage
	decErr := dec.Decode(&extra)
	assert.ErrorIs(t, decErr, io.EOF, "stdout must contain nothing after the single JSON result")

	// #2184 Verification: "outcome_unknown does not say delivered or
	// suggest blind retry" -- checked against both the JSON message and the
	// returned error text, since either could regress independently. "retry"
	// is allowed only inside the two fixed, non-inviting phrases this
	// package uses ("do not retry [automatically]", "before resending");
	// any other occurrence would read as an invitation.
	combined := strings.ToLower(result.Message + " " + cmdErr.Error())
	assert.NotContains(t, combined, "delivered")
	sanitized := strings.ReplaceAll(combined, "do not retry", "")
	sanitized = strings.ReplaceAll(sanitized, "before resending", "")
	assert.NotContains(t, sanitized, "retry",
		"unexpected bare 'retry' outside the allowed fixed phrases: %q", combined)
}

// ---------------------------------------------------------------------------
// resolveLocalKeysTarget's own "is ambiguous" branch, distinct from
// pkg/agent's own ambiguity handling, needs its own coverage.
// ---------------------------------------------------------------------------

func TestSendKeysLocalWithManager_Ambiguous_NoExec(t *testing.T) {
	origProjectPath := projectPath
	defer func() { projectPath = origProjectPath }()

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dirA := writeLocalProjectSettings(t, filepath.Join(tmp, "proj-a"), "proj-a-id")

	a := api.AgentInfo{
		Name:        "builder",
		ContainerID: "container-a1",
		Phase:       string(state.PhaseRunning),
		Labels: map[string]string{
			"scion.name":               "builder",
			"agent_id":                 "agent-a1",
			projectkeys.LabelProjectID: "proj-a-id",
		},
	}
	b := a
	b.ContainerID = "container-a2"
	b.Labels = map[string]string{
		"scion.name":               "builder",
		"agent_id":                 "agent-a2",
		projectkeys.LabelProjectID: "proj-a-id",
	}

	var captured []cmdExecRecord
	mockRT := filteringMockRuntimeWithStdin([]api.AgentInfo{a, b}, &captured)
	mgr := agent.NewManager(mockRT)
	defer mgr.Close()

	projectPath = dirA
	err := sendKeysLocalWithManager(context.Background(), mgr, "builder", "Escape")
	require.Error(t, err)
	// Pin resolveLocalKeysTarget's own wording specifically: pkg/agent's
	// resolution error also contains "ambiguous", so that substring alone
	// would still pass even if this CLI-level branch were removed.
	assert.Contains(t, err.Error(), "containers match in project")
	assert.Empty(t, captured, "an ambiguous local target must never be delivered to")
}

// TestSendKeysViaHub_RateLimited_SurfacesRetryAfter_NoRetry proves the
// CLI must surface a 429 keys_rate_limited response's Retry-After header
// (parsed by apiclient.ParseErrorResponse into APIError.RetryAfterSeconds),
// in both JSON and human-readable form, and must never auto-retry the
// request regardless.
func TestSendKeysViaHub_RateLimited_SurfacesRetryAfter_NoRetry(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "keys_rate_limited",
				"message": "rate limit exceeded",
				"details": map[string]interface{}{"operation_id": "op-429"},
			},
		})
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: "project-429"}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keys_rate_limited")
	// assert.Contains(err.Error(), "7") would match
	// almost anything containing a 7. Assert the actual suffix format
	// instead.
	assert.Contains(t, err.Error(), "Retry-After: 7s", "the Retry-After value must be surfaced in the human-readable error")
	assert.Contains(t, strings.ToLower(err.Error()), "do not retry",
		"must not read as an invitation to auto-retry")

	if got := atomic.LoadInt32(&requestCount); got != 1 {
		t.Fatalf("expected exactly 1 request to the keys endpoint (no automatic retry), got %d", got)
	}

	// JSON mode: the same value must be machine-readable.
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()
	outputFormat = "json"

	var cmdErr error
	stdout := captureStdout(t, func() {
		cmdErr = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	})
	require.Error(t, cmdErr)

	var result ActionResult
	require.NoError(t, json.NewDecoder(strings.NewReader(stdout)).Decode(&result))
	require.NotNil(t, result.Details)
	retryAfter, ok := result.Details["retry_after_seconds"]
	require.True(t, ok, "expected retry_after_seconds in JSON details, got: %v", result.Details)
	// JSON numbers decode as float64 via map[string]interface{}.
	assert.Equal(t, float64(7), retryAfter)

	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Fatalf("expected exactly 2 total requests across both calls (no automatic retry), got %d", got)
	}
}

// setupGitRepoWithRemote creates a temp git repo with the given origin
// remote URL and chdirs into it for the duration of the test (restoring the
// original working directory on cleanup) -- GetProjectID/getProjectIDForKeys
// resolve the git remote via util.GetGitRemote(), which shells out to `git
// remote get-url origin` in the current directory, with no injectable seam.
func setupGitRepoWithRemote(t *testing.T, remoteURL string) {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("remote", "add", "origin", remoteURL)

	origWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origWd) })
}

// TestGetProjectID_AmbiguousGitRemote_FailsForKeys covers
// ptone/scion#2200: the keys CLI path must fail with a message naming
// --project when several projects share the queried git remote, instead of
// GetProjectID's existing (and, for every other command, unchanged)
// behavior of silently picking resp.Projects[0]. Asserts both halves: the
// keys-scoped resolver fails, and the shared GetProjectID used by every
// other command is untouched.
func TestGetProjectID_AmbiguousGitRemote_FailsForKeys(t *testing.T) {
	const remoteURL = "https://github.com/example/ambiguous-repo.git"
	setupGitRepoWithRemote(t, remoteURL)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		if r.URL.Path == "/api/v1/projects" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"projects": []map[string]interface{}{
					{"id": "project-first", "name": "first", "slug": "first"},
					{"id": "project-second", "name": "second", "slug": "second"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	_, err = getProjectIDForKeys(hubCtx)
	require.Error(t, err, "expected the keys path to fail on an ambiguous git remote")
	assert.Contains(t, err.Error(), "--project")

	// Negative control: GetProjectID (used by every other command) is
	// unchanged -- it still silently resolves to the first match.
	id, err := GetProjectID(hubCtx)
	require.NoError(t, err, "GetProjectID must remain unaffected by the keys-scoped fix")
	assert.Equal(t, "project-first", id)
}

// TestSendKeysViaHub_AmbiguousGitRemote_FailsClosed_NoDispatch covers a gap:
// TestGetProjectID_AmbiguousGitRemote_FailsForKeys only
// proved getProjectIDForKeys itself fails closed, never that the keys CLI
// path (sendKeysViaHub, what `scion keys` actually calls) is wired to it
// instead of GetProjectID. Mutating cmd/keys.go back to call GetProjectID
// made that test suite keep passing, because nothing drove sendKeysViaHub
// with an empty hubCtx.ProjectID and an ambiguous git remote. This test
// does: it asserts the returned error names --project, and -- the
// decisive check a unit test on the resolver alone cannot make -- that the
// fake Hub's /keys endpoint never received a single POST.
func TestSendKeysViaHub_AmbiguousGitRemote_FailsClosed_NoDispatch(t *testing.T) {
	const remoteURL = "https://github.com/example/ambiguous-repo-e2e.git"
	setupGitRepoWithRemote(t, remoteURL)

	var keysPOSTs int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
			return
		}
		if r.URL.Path == "/api/v1/projects" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"projects": []map[string]interface{}{
					{"id": "project-e2e-first", "name": "first", "slug": "first"},
					{"id": "project-e2e-second", "name": "second", "slug": "second"},
				},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/keys") {
			atomic.AddInt32(&keysPOSTs, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "dispatched", "operation_id": "op-should-not-happen"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	// hubCtx.ProjectID deliberately empty: this is the exact shape
	// sendKeysViaHub receives from CheckHubAvailabilityForAgent when no
	// --project flag was given and settings carry no project_id, forcing
	// the git-remote resolution path that fails closed on ambiguity.
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	err = sendKeysViaHub(hubCtx, "target-agent", "C-c")
	require.Error(t, err, "expected the keys CLI path to fail on an ambiguous git remote")
	assert.Contains(t, err.Error(), "--project")

	if got := atomic.LoadInt32(&keysPOSTs); got != 0 {
		t.Fatalf("expected zero POSTs to the keys endpoint, got %d -- sendKeysViaHub must fail before ever resolving to a project to dispatch against", got)
	}
}

// TestKeysCmd_HelpTextNoSequenceClaim covers the A.6 gap: pins that
// keysCmd's help text explicitly disclaims sequence/macro support (each
// invocation delivers exactly one key) and that every Examples line passes
// exactly one key argument -- never a space-separated run like "Up Up
// Enter" that would read as a supported multi-key sequence (contract §2.3
// deviation).
func TestKeysCmd_HelpTextNoSequenceClaim(t *testing.T) {
	long := keysCmd.Long
	require.Contains(t, long, "not a sequence",
		"help text must explicitly disclaim sequence/macro support")

	lines := strings.Split(long, "\n")
	var exampleLines []string
	inExamples := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Examples:" {
			inExamples = true
			continue
		}
		if inExamples && trimmed != "" {
			exampleLines = append(exampleLines, trimmed)
		}
	}
	require.NotEmpty(t, exampleLines, "expected at least one Examples line")

	for _, line := range exampleLines {
		require.True(t, strings.HasPrefix(line, "scion keys "), "unexpected example line shape: %q", line)
		rest := strings.TrimPrefix(line, "scion keys ")
		fields := strings.Fields(rest)
		require.Len(t, fields, 2, "example %q must be exactly <agent> <one-key-argument>, never a multi-key sequence", line)
		keyArg := strings.Trim(fields[1], `"`)
		assert.NotContains(t, keyArg, " ", "example %q must pass exactly one key, not a space-separated sequence", line)
	}
}
