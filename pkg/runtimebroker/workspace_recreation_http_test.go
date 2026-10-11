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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// These tests drive the broker's start and restart handlers over HTTP
// (ptone/scion#2157). They use only the handlers' request and Manager.Start
// behaviour, so they also run unchanged against code that predates the
// change.

// recreationHubClient is stubHubClient plus the skill services the start
// handler's skill-resolver setup reads; they are never called by these tests.
type recreationHubClient struct {
	stubHubClient
}

func (c *recreationHubClient) Skills() hubclient.SkillService { return nil }

func (c *recreationHubClient) SkillRegistries() hubclient.SkillRegistryService { return nil }

// startWithTemplateHub returns a test server with one Hub connection whose
// local storage holds the global template "web-dev", that template's on-disk
// directory, and a counter of the Hub template lookups (a hydration).
func startWithTemplateHub(t *testing.T) (*Server, *mockManager, string, *atomic.Int32) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)
	stor, dir := newLocalStorageWithTemplate(t, "web-dev", true)
	lookups := &atomic.Int32{}
	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{
		Name:         "hub-1",
		LocalStorage: stor,
		HubClient: &recreationHubClient{stubHubClient{templates: &stubTemplateService{
			getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) {
				lookups.Add(1)
				return &hubclient.Template{ID: "tpl-uuid", Slug: "web-dev", Scope: "global"}, nil
			},
		}}},
		Hydrator: newTestHydrator(t),
	}
	srv.hubMu.Unlock()
	return srv, srv.manager.(*mockManager), dir, lookups
}

func postStart(t *testing.T, srv *Server, agentName, projectDir string) {
	t.Helper()
	body := `{
		"projectPath": "` + projectDir + `",
		"templateName": "web-dev",
		"templateId": "tpl-uuid",
		"templateHash": "sha256:abc"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
}

// TestStartAgent_MissingStateProvisionsFromHubTemplate: when the broker no
// longer has the agent's state, a start carrying the template identity
// provisions the agent from its own (hydrated) template, not the default.
func TestStartAgent_MissingStateProvisionsFromHubTemplate(t *testing.T) {
	srv, mgr, templateDir, _ := startWithTemplateHub(t)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	postStart(t, srv, "gone-agent", projectDir)
	require.Equal(t, 1, mgr.startCalls)
	assert.Equal(t, templateDir, mgr.lastStartOpts.Template)
	assert.Equal(t, "web-dev", mgr.lastStartOpts.TemplateName)
}

// TestStartAgent_SurvivingStateKeepsTemplateUnset: when the agent's state
// survived, the template identity changes nothing: no hydration, no
// template path and no SCION_TEMPLATE (the agent starts from its own state
// as before).
func TestStartAgent_SurvivingStateKeepsTemplateUnset(t *testing.T) {
	srv, mgr, _, lookups := startWithTemplateHub(t)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentDir := config.GetAgentDir(projectDir, "kept-agent", false)
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness":"claude"}`), 0o644))

	postStart(t, srv, "kept-agent", projectDir)
	require.Equal(t, 1, mgr.startCalls)
	assert.Empty(t, mgr.lastStartOpts.Template)
	_, set := mgr.lastStartOpts.Env["SCION_TEMPLATE"]
	assert.False(t, set)
	assert.Zero(t, lookups.Load(), "the template must not be hydrated")
}

// writeProvisionedAgent writes a provisioned agent state directory (one with
// scion-agent.json) for agentName in the project at projectPath, resolved
// the way the broker resolves it.
func writeProvisionedAgent(t *testing.T, projectPath, agentName string) {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	require.NoError(t, err)
	agentDir := config.GetAgentDir(projectDir, agentName, false)
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness":"claude"}`), 0o644))
}

// addLiveContainer lists a running container for agentName, recorded in the
// project at projectPath, on the mock manager.
func addLiveContainer(mgr *mockManager, agentName, projectPath string) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.agents = append(mgr.agents, api.AgentInfo{
		ID:              "container-" + agentName,
		ContainerID:     "container-" + agentName,
		Name:            agentName,
		Slug:            agentName,
		ProjectPath:     projectPath,
		Phase:           "running",
		ContainerStatus: "Up 1 hour",
	})
}

func postRecreationRestart(t *testing.T, srv *Server, agentName, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/restart", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
}

// TestRestartAgent_LiveContainerAndSurvivingStateUnchanged covers the common
// restart: the agent's container is running and its broker-side state
// exists, and a new Hub names the same project with the recreation inputs.
// The project comes from the request, nothing is cleared (FreshProvision
// stays false), no template path is set and nothing is hydrated.
func TestRestartAgent_LiveContainerAndSurvivingStateUnchanged(t *testing.T) {
	srv, mgr, _, lookups := startWithTemplateHub(t)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	writeProvisionedAgent(t, projectDir, "live-agent")
	// The container records a different project path, so the assertion
	// below tells which one the start uses.
	containerDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(containerDir, 0o755))
	addLiveContainer(mgr, "live-agent", containerDir)

	postRecreationRestart(t, srv, "live-agent", `{
		"resolvedEnv": {"SCION_AGENT_ID": "agent-uuid-live"},
		"projectPath": "`+projectDir+`",
		"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main"},
		"templateName": "web-dev",
		"templateId": "tpl-uuid",
		"templateHash": "sha256:abc"
	}`)
	require.Equal(t, 1, mgr.startCalls)
	require.Equal(t, 1, mgr.stopCalls, "the live container is stopped first")
	opts := mgr.lastStartOpts
	assert.Equal(t, projectDir, opts.ProjectPath, "the request's project wins over the container's recorded path")
	assert.False(t, opts.FreshProvision)
	assert.Empty(t, opts.Template)
	assert.Zero(t, lookups.Load(), "the template must not be hydrated")
}

// TestRestartAgent_LiveContainerAndSurvivingStateUnchanged_HubNative is the
// hub-native variant: the request names the project by slug only.
func TestRestartAgent_LiveContainerAndSurvivingStateUnchanged_HubNative(t *testing.T) {
	srv, mgr, _, lookups := startWithTemplateHub(t)
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	projectPath := filepath.Join(globalDir, "projects", "native-proj")
	require.NoError(t, os.MkdirAll(filepath.Join(projectPath, ".scion"), 0o755))
	writeProvisionedAgent(t, projectPath, "native-agent")
	resolved, err := config.GetResolvedProjectDir(projectPath)
	require.NoError(t, err)
	addLiveContainer(mgr, "native-agent", resolved)

	postRecreationRestart(t, srv, "native-agent", `{
		"resolvedEnv": {"SCION_AGENT_ID": "agent-uuid-native"},
		"projectSlug": "native-proj",
		"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main"},
		"templateName": "web-dev",
		"templateId": "tpl-uuid",
		"templateHash": "sha256:abc"
	}`)
	require.Equal(t, 1, mgr.startCalls)
	opts := mgr.lastStartOpts
	assert.Equal(t, projectPath, opts.ProjectPath, "the project is resolved from the slug")
	assert.False(t, opts.FreshProvision)
	assert.Empty(t, opts.Template)
	assert.Zero(t, lookups.Load(), "the template must not be hydrated")
}

// TestRestartAgent_UsesRequestInputsWithoutContainer proves a restart of an
// agent with no container (on Kubernetes, stopping an agent deletes its
// pod) resolves the agent's project from the request and carries the
// workspace-recreation inputs to Manager.Start, as a start does.
func TestRestartAgent_UsesRequestInputsWithoutContainer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	body := `{
		"resolvedEnv": {"SCION_AGENT_ID": "agent-uuid-2157"},
		"projectPath": "` + projectDir + `",
		"harnessConfig": "claude",
		"gitClone": {"url": "https://github.com/example/repo.git", "branch": "main"},
		"branch": "feature-branch",
		"templateName": "web-dev"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/gone-agent/restart", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	require.Equal(t, 1, mgr.startCalls)
	opts := mgr.lastStartOpts
	assert.Equal(t, projectDir, opts.ProjectPath, "the project comes from the request, not a container")
	require.NotNil(t, opts.GitClone, "the restart must carry the git clone settings")
	assert.Equal(t, "https://github.com/example/repo.git", opts.GitClone.URL)
	assert.Equal(t, "feature-branch", opts.Branch)
	assert.Equal(t, "claude", opts.HarnessConfig)
	assert.Equal(t, "https://github.com/example/repo.git", opts.Env["SCION_GIT_CLONE_URL"])
	assert.False(t, opts.FreshProvision, "a restart never clears an existing workspace")
}

// TestRestartAgent_OlderHubFallsBackToContainerProject proves a restart from
// an older Hub (no project in the request) still takes the project from the
// agent's container, and builds no start config.
func TestRestartAgent_OlderHubFallsBackToContainerProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	mgr.mu.Lock()
	mgr.agents = append(mgr.agents, api.AgentInfo{
		ID:              "container-2157",
		ContainerID:     "container-2157",
		Name:            "kept-agent",
		Slug:            "kept-agent",
		ProjectPath:     projectDir,
		Phase:           "running",
		ContainerStatus: "Up 1 hour",
	})
	mgr.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/kept-agent/restart", strings.NewReader(`{"resolvedEnv": {"FOO": "bar"}}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	opts := mgr.lastStartOpts
	assert.Equal(t, projectDir, opts.ProjectPath)
	assert.Nil(t, opts.GitClone)
	_, set := opts.Env["SCION_GIT_CLONE_URL"]
	assert.False(t, set)
}

// TestRestartAgent_PassesResolvedSecretsLikeStart: the resolved secrets a
// restart request carries reach Manager.Start the same way a start
// request's do (fake values only).
func TestRestartAgent_PassesResolvedSecretsLikeStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	body := `{
		"resolvedEnv": {"FOO": "bar"},
		"resolvedSecrets": [{"name": "FAKE_CREDS", "type": "file", "target": "~/.fake/creds.json", "value": "fake-secret-value", "source": "user"}]
	}`
	got := map[string][]api.ResolvedSecret{}
	for _, op := range []string{"start", "restart"} {
		srv := newTestServer(t)
		mgr := srv.manager.(*mockManager)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/"+op, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusAccepted, w.Code, "%s: %s", op, w.Body.String())
		got[op] = mgr.lastStartOpts.ResolvedSecrets
	}
	require.Len(t, got["start"], 1)
	assert.Equal(t, got["start"], got["restart"], "restart must hand Manager.Start the secrets start does")
}
