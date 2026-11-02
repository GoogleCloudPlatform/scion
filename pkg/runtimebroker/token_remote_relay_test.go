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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relayTokenRemote is the hub's recorded remote the relay tests send.
const relayTokenRemote = "github.com/example/repo"

// serveTokenRemote sends body to path on srv and requires want.
func serveTokenRemote(t *testing.T, srv *Server, path, body string, want int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	require.Equal(t, want, w.Code, w.Body.String())
}

// The hub's token remote (gitClone.tokenRemote) survives the broker on every
// dispatch: create, start and restart decode it from the wire, keep it in
// StartOptions.GitClone, and relay it unchanged to the agent container as
// api.EnvGitTokenRemote, which sciontool init reads.
func TestTokenRemote_RelayedOnCreateStartRestart(t *testing.T) {
	gitClone := `{"url": "https://github.com/example/repo.git", "branch": "main", "depth": 1, "tokenRemote": "` + relayTokenRemote + `"}`

	t.Run("create", func(t *testing.T) {
		srv, mgr := newTestServerWithGitCloneCapture()
		serveTokenRemote(t, srv, "/api/v1/agents",
			`{"name": "relay-agent", "config": {"template": "claude", "gitClone": `+gitClone+`}}`, http.StatusCreated)
		require.NotNil(t, mgr.lastGitClone)
		assert.Equal(t, relayTokenRemote, mgr.lastGitClone.TokenRemote)
		assert.Equal(t, relayTokenRemote, mgr.lastEnv[api.EnvGitTokenRemote])
	})

	t.Run("start", func(t *testing.T) {
		srv, mgr := newTestServerWithGitCloneCapture()
		serveTokenRemote(t, srv, "/api/v1/agents/relay-agent/start", `{"gitClone": `+gitClone+`}`, http.StatusAccepted)
		require.NotNil(t, mgr.lastGitClone)
		assert.Equal(t, relayTokenRemote, mgr.lastGitClone.TokenRemote)
		assert.Equal(t, relayTokenRemote, mgr.lastEnv[api.EnvGitTokenRemote])
	})

	t.Run("restart", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		srv := newTestServer(t)
		mgr := srv.manager.(*mockManager)
		projectDir := filepath.Join(t.TempDir(), ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		serveTokenRemote(t, srv, "/api/v1/agents/gone-agent/restart", `{
			"resolvedEnv": {"SCION_AGENT_ID": "agent-uuid-relay"},
			"projectPath": "`+projectDir+`",
			"harnessConfig": "claude",
			"gitClone": `+gitClone+`
		}`, http.StatusAccepted)
		require.Equal(t, 1, mgr.startCalls)
		require.NotNil(t, mgr.lastStartOpts.GitClone)
		assert.Equal(t, relayTokenRemote, mgr.lastStartOpts.GitClone.TokenRemote)
		assert.Equal(t, relayTokenRemote, mgr.lastStartOpts.Env[api.EnvGitTokenRemote])
	})
}

// Only the hub's GitClone sets the token remote: a request env entry naming
// one (config.env or the resolved env) is dropped, whether or not the
// GitClone carries its own, and a GitClone without a token remote sets
// none, so the in-container clone gets no token (fail closed).
func TestTokenRemote_OnlyFromGitClone(t *testing.T) {
	const injected = "mirror.example/org/repo"
	for _, tc := range []struct {
		name, gitClone, want string
	}{
		{"GitClone with token remote", `{"url": "https://github.com/example/repo.git", "tokenRemote": "` + relayTokenRemote + `"}`, relayTokenRemote},
		{"GitClone without token remote", `{"url": "https://github.com/example/repo.git"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr := newTestServerWithGitCloneCapture()
			serveTokenRemote(t, srv, "/api/v1/agents", `{
				"name": "relay-agent",
				"resolvedEnv": {"`+api.EnvGitTokenRemote+`": "`+injected+`"},
				"config": {
					"template": "claude",
					"env": ["`+api.EnvGitTokenRemote+`=`+injected+`"],
					"gitClone": `+tc.gitClone+`
				}
			}`, http.StatusCreated)
			got, set := mgr.lastEnv[api.EnvGitTokenRemote]
			if tc.want == "" {
				assert.False(t, set, "no token remote may be set without the hub's, got %q", got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("no GitClone", func(t *testing.T) {
		srv, mgr := newTestServerWithGitCloneCapture()
		serveTokenRemote(t, srv, "/api/v1/agents", `{
			"name": "relay-agent",
			"config": {"template": "claude", "env": ["`+api.EnvGitTokenRemote+`=`+injected+`"]}
		}`, http.StatusCreated)
		assert.NotContains(t, mgr.lastEnv, api.EnvGitTokenRemote)
	})
}

// A shared-plain project's workspace clone settings keep the token remote on
// the way to StartOptions (the Kubernetes runtime's provisioning init
// container reads it from there).
func TestTokenRemote_SharedWorkspaceCloneKeepsTokenRemote(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectPath: "/some/path",
		Config: &CreateAgentConfig{
			Workspace:       t.TempDir(),
			SharedWorkspace: true,
			SharedWorkspaceClone: &api.GitCloneConfig{
				URL:         "https://github.com/example/repo.git",
				Branch:      "main",
				Depth:       intPtr(0),
				TokenRemote: relayTokenRemote,
			},
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	require.NoError(t, err)
	require.NotNil(t, sc.Opts.SharedWorkspaceClone)
	assert.Equal(t, relayTokenRemote, sc.Opts.SharedWorkspaceClone.TokenRemote)
}

// A shared-plain project's agent gets the hub's token remote in its env, so
// sciontool init scopes the shared workspace's credential helper to the
// recorded remote's host; without one it gets none.
func TestTokenRemote_SharedWorkspaceEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	build := func(tokenRemote string) map[string]string {
		t.Helper()
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectPath: "/some/path",
			Config: &CreateAgentConfig{
				Workspace:       t.TempDir(),
				SharedWorkspace: true,
				Env:             []string{api.EnvGitTokenRemote + "=mirror.example/org/shared"},
				SharedWorkspaceClone: &api.GitCloneConfig{
					URL:         "https://mirror.example/org/shared.git",
					TokenRemote: tokenRemote,
				},
			},
			HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
			Operation:   opCreate,
		})
		require.NoError(t, err)
		return sc.Opts.Env
	}
	assert.Equal(t, relayTokenRemote, build(relayTokenRemote)[api.EnvGitTokenRemote])
	assert.NotContains(t, build(""), api.EnvGitTokenRemote, "no token remote without the hub's; the request env entry is dropped")
}

// The classifications of the clone URL and token remote keys follow the
// keys: a classification that arrived with a dropped request env entry does
// not survive the drop; when the broker sets the key from GitClone, it is
// classified by the broker; a nil classification map (an older hub) stays nil.
func TestTokenRemote_NoStaleEnvClassification(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	build := func(gc *api.GitCloneConfig, cls map[string]api.EnvKind) *startContext {
		t.Helper()
		sc, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "agent-1",
			ProjectPath: "/some/path",
			ResolvedEnv: map[string]string{api.EnvGitTokenRemote: "mirror.example/org/repo", "SCION_GIT_CLONE_URL": "https://mirror.example/org/repo.git"},
			Config: &CreateAgentConfig{
				Env:      []string{api.EnvGitTokenRemote + "=mirror.example/org/repo", "SCION_GIT_CLONE_URL=https://mirror.example/org/repo.git"},
				GitClone: gc,
			},
			EnvClassifications: cls,
			HTTPRequest:        httptest.NewRequest("POST", "/api/v1/agents", nil),
			Operation:          opCreate,
		})
		require.NoError(t, err)
		return sc
	}
	hubCls := func() map[string]api.EnvKind {
		return map[string]api.EnvKind{api.EnvGitTokenRemote: api.EnvKindPlain, "SCION_GIT_CLONE_URL": api.EnvKindPlain}
	}

	sc := build(nil, hubCls())
	assert.NotContains(t, sc.EnvClassifications, api.EnvGitTokenRemote, "no stale classification for a dropped key")
	assert.NotContains(t, sc.EnvClassifications, "SCION_GIT_CLONE_URL", "no stale classification for a dropped key")

	sc = build(&api.GitCloneConfig{URL: "https://github.com/example/repo.git", TokenRemote: relayTokenRemote}, hubCls())
	assert.Equal(t, api.EnvKindPlain, sc.EnvClassifications[api.EnvGitTokenRemote])
	assert.Equal(t, api.EnvKindSecretInjected, sc.EnvClassifications["SCION_GIT_CLONE_URL"], "the broker's own classification")

	sc = build(nil, nil)
	assert.Nil(t, sc.EnvClassifications, "an older hub's nil classifications stay nil")
}
