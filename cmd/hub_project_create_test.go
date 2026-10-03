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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectCreateMockHub is a minimal Hub that records project create bodies
// and validates workspaceMode like the real server (400 for unknown modes
// and for worktree-per-agent without a git remote).
type projectCreateMockHub struct {
	mu      sync.Mutex
	creates []map[string]interface{}
}

func (m *projectCreateMockHub) lastCreate(t *testing.T) map[string]interface{} {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.creates, "no project create request reached the hub")
	return m.creates[len(m.creates)-1]
}

func (m *projectCreateMockHub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}, "totalCount": 0})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodPost:
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			m.mu.Lock()
			m.creates = append(m.creates, body)
			m.mu.Unlock()

			mode, _ := body["workspaceMode"].(string)
			remote, _ := body["gitRemote"].(string)
			msg := ""
			switch mode {
			case "", "shared", "per-agent":
			case "worktree-per-agent":
				if remote == "" {
					msg = `workspace mode "worktree-per-agent" requires a git remote`
				}
			default:
				msg = `invalid workspace mode "` + mode + `": must be one of "shared", "per-agent", "worktree-per-agent"`
			}
			if msg != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "validation_error", "message": msg},
				})
				return
			}
			labels := map[string]string{}
			if mode != "" {
				labels["scion.dev/workspace-mode"] = mode
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "p-1", "name": body["name"], "slug": body["slug"], "gitRemote": remote, "labels": labels,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// setupProjectCreateTest wires a mock hub and resets the create flags.
func setupProjectCreateTest(t *testing.T) *projectCreateMockHub {
	t.Helper()
	origHome := os.Getenv("HOME")
	origProjectPath := projectPath
	origSlug, origName, origBranch, origMode := hubProjectCreateSlug, hubProjectCreateName, hubProjectCreateBranch, hubProjectCreateMode
	origJSON, origFormat, origYes, origNonInteractive := hubOutputJSON, outputFormat, autoConfirm, nonInteractive
	t.Cleanup(func() {
		_ = os.Setenv("HOME", origHome)
		projectPath = origProjectPath
		hubProjectCreateSlug, hubProjectCreateName, hubProjectCreateBranch, hubProjectCreateMode = origSlug, origName, origBranch, origMode
		hubOutputJSON, outputFormat, autoConfirm, nonInteractive = origJSON, origFormat, origYes, origNonInteractive
	})

	mock := &projectCreateMockHub{}
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	projectPath = setupEnvProject(t, tmpHome, server.URL)

	hubProjectCreateSlug, hubProjectCreateName, hubProjectCreateBranch, hubProjectCreateMode = "", "", "", ""
	hubOutputJSON, autoConfirm, nonInteractive = false, true, true
	return mock
}

func TestHubProjectCreateCmd_Args(t *testing.T) {
	assert.Equal(t, "create [git-url]", hubProjectCreateCmd.Use)
	assert.NoError(t, hubProjectCreateCmd.Args(hubProjectCreateCmd, nil), "no URL must be accepted")
	assert.NoError(t, hubProjectCreateCmd.Args(hubProjectCreateCmd, []string{"https://github.com/acme/widgets.git"}))
	assert.Error(t, hubProjectCreateCmd.Args(hubProjectCreateCmd, []string{"a", "b"}))

	f := hubProjectCreateCmd.Flags().Lookup("workspace-mode")
	require.NotNil(t, f, "--workspace-mode flag must exist")
	assert.Equal(t, "", f.DefValue)
}

func TestRunHubProjectCreate_NoURL_HubManaged(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateName = "Scratch Pad"
	hubProjectCreateMode = "per-agent"

	require.NoError(t, runHubProjectCreate(hubProjectCreateCmd, nil))

	body := mock.lastCreate(t)
	assert.Equal(t, "Scratch Pad", body["name"])
	assert.Equal(t, "scratch-pad", body["slug"])
	assert.Equal(t, "per-agent", body["workspaceMode"])
	assert.NotContains(t, body, "gitRemote", "hub-managed create must not send a git remote")
	assert.NotContains(t, body, "labels", "hub-managed create must not send git labels")
}

func TestRunHubProjectCreate_NoURL_DefaultModeOmitted(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateName = "notes"

	require.NoError(t, runHubProjectCreate(hubProjectCreateCmd, nil))
	assert.NotContains(t, mock.lastCreate(t), "workspaceMode", "no flag must leave the mode to the server default")
}

func TestRunHubProjectCreate_NoURL_RequiresName(t *testing.T) {
	mock := setupProjectCreateTest(t)
	err := runHubProjectCreate(hubProjectCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--name is required")
	assert.Empty(t, mock.creates)
}

func TestRunHubProjectCreate_NoURL_RejectsBranch(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateName = "notes"
	hubProjectCreateBranch = "main"
	err := runHubProjectCreate(hubProjectCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--branch requires a git URL")
	assert.Empty(t, mock.creates)
}

func TestRunHubProjectCreate_URLWithWorkspaceMode(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateBranch = "main" // skip git ls-remote
	hubProjectCreateMode = "worktree-per-agent"

	require.NoError(t, runHubProjectCreate(hubProjectCreateCmd, []string{"https://github.com/acme/widgets.git"}))

	body := mock.lastCreate(t)
	assert.Equal(t, "worktree-per-agent", body["workspaceMode"])
	assert.Equal(t, "github.com/acme/widgets", body["gitRemote"])
	labels, _ := body["labels"].(map[string]interface{})
	assert.Equal(t, "main", labels["scion.dev/default-branch"])
	assert.NotContains(t, labels, "scion.dev/workspace-mode", "the mode travels in workspaceMode, not as a raw label")
}

func TestRunHubProjectCreate_InvalidModePassedThroughToServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		mode    string
		wantErr string
	}{
		{name: "unknown mode", args: nil, mode: "bogus", wantErr: `invalid workspace mode "bogus"`},
		{name: "worktree without git", args: nil, mode: "worktree-per-agent", wantErr: "requires a git remote"},
		{name: "unknown mode with URL", args: []string{"https://github.com/acme/widgets.git"}, mode: "clone", wantErr: `invalid workspace mode "clone"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupProjectCreateTest(t)
			hubProjectCreateName = "notes"
			hubProjectCreateBranch = ""
			if len(tc.args) > 0 {
				hubProjectCreateBranch = "main"
			}
			hubProjectCreateMode = tc.mode

			err := runHubProjectCreate(hubProjectCreateCmd, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to create project")
			assert.Contains(t, err.Error(), tc.wantErr, "the hub's 400 message must reach the user")
			assert.Equal(t, tc.mode, mock.lastCreate(t)["workspaceMode"], "the CLI must not filter the mode itself")
		})
	}
}

func TestWorkspaceBootstrapNotice(t *testing.T) {
	ignored := "workspace files were ignored: this project gives each agent an empty workspace directory"
	for _, tc := range []struct {
		name       string
		sent, urls int
		warnings   []string
		wantLines  []string
		wantShown  bool
	}{
		{name: "no files sent", sent: 0, urls: 0, warnings: []string{ignored}},
		{name: "uploading", sent: 3, urls: 3},
		{name: "local broker workspace", sent: 3, urls: 0, wantLines: []string{"Using local workspace on broker."}},
		{name: "files ignored by hub", sent: 3, urls: 0, warnings: []string{ignored},
			wantLines: []string{"Warning: " + ignored}, wantShown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, shown := workspaceBootstrapNotice(tc.sent, tc.urls, tc.warnings)
			assert.Equal(t, tc.wantLines, lines)
			assert.Equal(t, tc.wantShown, shown)
		})
	}
}
