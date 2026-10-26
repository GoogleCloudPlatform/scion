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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusedCloneURLs are clone URLs provisioning refuses
// (provision.CheckHostCloneURL); each names the host refused.example where
// it has one, so a test can check the error never names it.
var refusedCloneURLs = []string{
	"/srv/refused.example/repo",
	"file:///srv/refused.example/repo",
	"http://refused.example/org/repo.git",
	"ext::sh -c refused.example",
	"-urefused.example",
}

// assertCloneURLRefusal checks a 422 refusal with the fixed refusal text
// after mode, naming no URL.
func assertCloneURLRefusal(t *testing.T, status int, message, mode string) {
	t.Helper()
	assert.Equal(t, http.StatusUnprocessableEntity, status)
	assert.Contains(t, message, mode+": "+provision.ErrHostCloneURLRefused.Error())
	assert.NotContains(t, message, "refused.example", "the refusal names no URL")
}

// clonePathServer is a broker whose dispatches resolve to runtimeName.
func clonePathServer(runtimeName string) (*Server, *gitCloneCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"
	mgr := &gitCloneCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return runtimeName }}
	return New(cfg, mgr, rt, config.RuntimeBuildRecord{}), mgr
}

// The clone-per-agent path refuses a clone URL provisioning refuses on
// create, start and restart, for every runtime the broker dispatches to
// (the in-container clone on Docker/Podman/Apple and Cloud Run, and the
// Kubernetes provisioning init container): 422, the agent manager is never
// asked to start the agent, and no clone setting reaches it.
func TestCloneURLRefusal_ClonePerAgentEveryRuntime(t *testing.T) {
	for _, rtName := range []string{"docker", "podman", "container", "kubernetes", "cloudrun"} {
		for _, cloneURL := range refusedCloneURLs {
			gitClone := `{"url": "` + strings.ReplaceAll(cloneURL, `"`, `\"`) + `", "branch": "main"}`
			for _, tc := range []struct{ name, path, body string }{
				{"create", "/api/v1/agents", `{"name": "refused-agent", "config": {"template": "claude", "gitClone": ` + gitClone + `}}`},
				{"start", "/api/v1/agents/refused-agent/start", `{"gitClone": ` + gitClone + `}`},
			} {
				t.Run(rtName+"/"+tc.name+"/"+cloneURL, func(t *testing.T) {
					srv, mgr := clonePathServer(rtName)
					req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					srv.Handler().ServeHTTP(w, req)
					assertCloneURLRefusal(t, w.Code, w.Body.String(), "clone-per-agent")
					assert.Nil(t, mgr.lastGitClone, "the agent must not be started with the refused URL")
					assert.NotContains(t, mgr.lastEnv, "SCION_GIT_CLONE_URL")
				})
			}
		}
	}
}

// The restart path refuses the same way (an agent with no container, so the
// request's inputs are used).
func TestCloneURLRefusal_Restart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/gone-agent/restart", strings.NewReader(`{
		"resolvedEnv": {"SCION_AGENT_ID": "agent-uuid-refused"},
		"projectPath": "`+projectDir+`",
		"harnessConfig": "claude",
		"gitClone": {"url": "http://refused.example/org/repo.git", "branch": "main"}
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	assertCloneURLRefusal(t, w.Code, w.Body.String(), "clone-per-agent")
	assert.Zero(t, mgr.startCalls, "the agent must not be started")
}

// The worktree-per-agent dispatches that fall back to the in-container clone
// before provisioning runs (no project path for a host-side worktree, or a
// Kubernetes dispatch, where host-side provisioning is skipped) refuse the
// URL instead of falling back.
func TestCloneURLRefusal_WorktreeFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		runtime     string
		projectPath string
	}{
		{"no project path", "docker", ""},
		{"kubernetes dispatch", "kubernetes", "/some/path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContextRuntime(t, cfg, tc.runtime)
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "agent-1", AgentID: "agent-1", ProjectID: "p1", ProjectSlug: "proj",
				ProjectPath:   tc.projectPath,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: "file:///srv/refused.example/repo", Branch: "main"}},
				HTTPRequest:   httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:     opCreate,
			})
			require.Nil(t, sc, "no start context may be built for a refused URL")
			var sce *startContextError
			require.ErrorAs(t, err, &sce)
			assertCloneURLRefusal(t, sce.Status, sce.Message, "clone-per-agent")
		})
	}
}

// A shared-plain project's workspace clone settings (the Kubernetes
// provisioning init container's clone) are refused the same way.
func TestCloneURLRefusal_SharedWorkspaceClone(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	for _, cloneURL := range refusedCloneURLs {
		t.Run(cloneURL, func(t *testing.T) {
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "agent-1",
				ProjectPath: "/some/path",
				Config: &CreateAgentConfig{
					Workspace:            t.TempDir(),
					SharedWorkspace:      true,
					SharedWorkspaceClone: &api.GitCloneConfig{URL: cloneURL, Branch: "main"},
				},
				HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
				Operation:   opCreate,
			})
			require.Nil(t, sc, "no start context may be built for a refused URL")
			var sce *startContextError
			require.ErrorAs(t, err, &sce)
			assertCloneURLRefusal(t, sce.Status, sce.Message, "shared-workspace")
		})
	}
}

// Accepted clone URLs (https, ssh, scp-style, git) still dispatch on the
// clone-per-agent path.
func TestCloneURLRefusal_AcceptedURLsDispatch(t *testing.T) {
	for _, cloneURL := range []string{
		"https://github.com/org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"git@github.com:org/repo.git",
		"git://github.com/org/repo.git",
	} {
		t.Run(cloneURL, func(t *testing.T) {
			srv, mgr := clonePathServer("docker")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(
				`{"name": "ok-agent", "config": {"template": "claude", "gitClone": {"url": "`+cloneURL+`"}}}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			assert.Equal(t, cloneURL, mgr.lastEnv["SCION_GIT_CLONE_URL"])
		})
	}
}

// The in-container clone URL comes only from the hub's GitClone, after the
// clone URL check: a request env entry (resolvedEnv or config.env) naming
// one is dropped, with or without a GitClone.
func TestCloneURLRefusal_RequestEnvCannotSetCloneURL(t *testing.T) {
	const injected = "file:///srv/refused.example/repo"
	for _, tc := range []struct{ name, gitClone, want string }{
		{"no GitClone", "", ""},
		{"GitClone", `, "gitClone": {"url": "https://github.com/org/repo.git"}`, "https://github.com/org/repo.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr := clonePathServer("docker")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(`{
				"name": "env-agent",
				"resolvedEnv": {"SCION_GIT_CLONE_URL": "`+injected+`"},
				"config": {"template": "claude", "env": ["SCION_GIT_CLONE_URL=`+injected+`"]`+tc.gitClone+`}
			}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			got, set := mgr.lastEnv["SCION_GIT_CLONE_URL"]
			if tc.want == "" {
				assert.False(t, set, "a request env clone URL reached the agent: %q", got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
