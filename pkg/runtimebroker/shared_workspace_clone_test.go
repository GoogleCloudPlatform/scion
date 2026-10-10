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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A shared-plain git project's workspace clone settings reach StartOptions
// without turning on the per-agent clone mode: the workspace stays mounted
// and no in-container clone env is set.
func TestBuildStartContext_SharedWorkspaceClone_KeepsWorkspaceMount(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	workspace := t.TempDir()

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectPath: "/some/path",
		Config: &CreateAgentConfig{
			Workspace:       workspace,
			SharedWorkspace: true,
			SharedWorkspaceClone: &api.GitCloneConfig{
				URL:    "https://github.com/org/shared.git",
				Branch: "main",
				Depth:  intPtr(0),
			},
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	require.NoError(t, err)
	require.NotNil(t, sc.Opts.SharedWorkspaceClone)
	assert.Equal(t, "https://github.com/org/shared.git", sc.Opts.SharedWorkspaceClone.URL)
	assert.Equal(t, "main", sc.Opts.SharedWorkspaceClone.Branch)
	assert.Nil(t, sc.Opts.GitClone)
	assert.True(t, sc.Opts.SharedWorkspace)
	assert.Equal(t, workspace, sc.Opts.Workspace, "the shared workspace must stay mounted")
	for _, key := range []string{"SCION_GIT_CLONE_URL", "SCION_GIT_BRANCH", "SCION_GIT_DEPTH"} {
		assert.NotContains(t, sc.Opts.Env, key)
	}
}

func TestBuildStartContext_SharedWorkspaceClone_IgnoredWithoutSharedWorkspaceOrWithGitClone(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	shared := &api.GitCloneConfig{URL: "https://github.com/org/shared.git", Branch: "main"}

	// Not a shared workspace.
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-1",
		ProjectPath: "/some/path",
		Config: &CreateAgentConfig{
			Workspace:            t.TempDir(),
			SharedWorkspaceClone: shared,
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	require.NoError(t, err)
	assert.Nil(t, sc.Opts.SharedWorkspaceClone)

	// A per-agent clone request wins.
	sc, err = srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-2",
		ProjectPath: "/some/path",
		Config: &CreateAgentConfig{
			SharedWorkspace:      true,
			SharedWorkspaceClone: shared,
			GitClone:             &api.GitCloneConfig{URL: "https://github.com/org/repo.git"},
		},
		HTTPRequest: httptest.NewRequest("POST", "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	require.NoError(t, err)
	assert.Nil(t, sc.Opts.SharedWorkspaceClone)
	assert.NotNil(t, sc.Opts.GitClone)
}

const sharedWorkspaceCloneJSON = `"sharedWorkspace": true,
		"sharedWorkspaceClone": {"url": "https://github.com/org/shared.git", "branch": "main", "depth": 0}`

func TestCreateAgent_SharedWorkspaceClone_PassedThrough(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()
	body := `{"name": "shared-agent", "config": {"template": "claude", "workspace": "` + t.TempDir() + `", ` + sharedWorkspaceCloneJSON + `}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	assert.True(t, mgr.lastSharedWorkspace)
	require.NotNil(t, mgr.lastSharedWorkspaceClone)
	assert.Equal(t, "https://github.com/org/shared.git", mgr.lastSharedWorkspaceClone.URL)
	require.NotNil(t, mgr.lastSharedWorkspaceClone.Depth)
	assert.Equal(t, 0, *mgr.lastSharedWorkspaceClone.Depth)
	assert.Nil(t, mgr.lastGitClone)
	assert.NotContains(t, mgr.lastEnv, "SCION_GIT_CLONE_URL")
}

func TestStartAgent_SharedWorkspaceClone_PassedThrough(t *testing.T) {
	srv, mgr := newTestServerWithGitCloneCapture()
	body := `{` + sharedWorkspaceCloneJSON + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/shared-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	assert.True(t, mgr.lastSharedWorkspace)
	require.NotNil(t, mgr.lastSharedWorkspaceClone)
	assert.Equal(t, "https://github.com/org/shared.git", mgr.lastSharedWorkspaceClone.URL)
	assert.Nil(t, mgr.lastGitClone)
	assert.NotContains(t, mgr.lastEnv, "SCION_GIT_CLONE_URL")
}

// The wire field name is shared with the hub (RemoteAgentConfig).
func TestCreateAgentConfig_SharedWorkspaceCloneJSON(t *testing.T) {
	var cfg CreateAgentConfig
	require.NoError(t, json.Unmarshal([]byte(`{`+sharedWorkspaceCloneJSON+`}`), &cfg))
	require.NotNil(t, cfg.SharedWorkspaceClone)
	assert.Equal(t, "https://github.com/org/shared.git", cfg.SharedWorkspaceClone.URL)
	assert.True(t, cfg.SharedWorkspace)
}
