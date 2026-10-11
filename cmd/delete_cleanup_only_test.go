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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCleanupOnlyState isolates the package flags --cleanup-only reads.
func withCleanupOnlyState(t *testing.T) {
	t.Helper()
	orig := saveDeleteTestState()
	origCleanup, origFormat := deleteCleanupOnly, outputFormat
	t.Cleanup(func() {
		orig.restore()
		deleteCleanupOnly, outputFormat = origCleanup, origFormat
	})
	t.Setenv("HOME", t.TempDir())
	deleteForce = false
	preserveBranch = true
	deleteStopped = false
	outputFormat = ""
}

// cleanupOnlyHub serves GET for one project's agents: agentStatus maps an
// agent name to the status code returned (200 answers with a live agent,
// 299 is shorthand for 200 with a soft-deleted agent). Unknown names get 404.
func cleanupOnlyHub(t *testing.T, projectID string, agentStatus map[string]int) *HubContext {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/api/v1/projects/" + projectID + "/agents/"
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) {
			t.Errorf("unexpected hub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		w.Header().Set("Content-Type", "application/json")
		switch agentStatus[name] {
		case http.StatusOK:
			_, _ = w.Write([]byte(`{"id":"id-` + name + `","name":"` + name + `","phase":"running"}`))
		case 299:
			_, _ = w.Write([]byte(`{"id":"id-` + name + `","name":"` + name + `","deletedAt":"2026-10-10T10:00:00Z"}`))
		case http.StatusInternalServerError:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"agent_not_found","message":"not found"}}`))
		}
	}))
	t.Cleanup(server.Close)
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}
}

// cleanupOnlyProject creates a project directory and points projectPath at it.
func cleanupOnlyProject(t *testing.T) string {
	t.Helper()
	projectDir := filepath.Join(t.TempDir(), "project", ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(projectDir, "agents"), 0o755))
	projectPath = projectDir
	return projectDir
}

func TestDeleteCleanupOnly_HubStillHasAgentRefuses(t *testing.T) {
	withCleanupOnlyState(t)
	projectDir := cleanupOnlyProject(t)
	agentDir := createAgentDir(t, projectDir, "alive")
	hubCtx := cleanupOnlyHub(t, "p1", map[string]int{"alive": http.StatusOK})

	var err error
	captureStderr(t, func() { err = deleteAgentsCleanupOnly(hubCtx, nil, []string{"alive"}) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent still exists on the Hub; run 'scion delete alive'")
	assert.DirExists(t, agentDir, "files of an agent the Hub still has are kept")

	// --force does not override a hub that answered that the agent exists.
	deleteForce = true
	captureStderr(t, func() { err = deleteAgentsCleanupOnly(hubCtx, nil, []string{"alive"}) })
	require.Error(t, err)
	assert.DirExists(t, agentDir)
}

func TestDeleteCleanupOnly_HubHasNoAgentRemovesFiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "not found", status: http.StatusNotFound},
		{name: "soft-deleted", status: 299},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCleanupOnlyState(t)
			projectDir := cleanupOnlyProject(t)
			agentDir := createAgentDir(t, projectDir, "gone")
			hubCtx := cleanupOnlyHub(t, "p1", map[string]int{"gone": tc.status})

			var err error
			stderr := captureStderr(t, func() { err = deleteAgentsCleanupOnly(hubCtx, nil, []string{"gone"}) })
			require.NoError(t, err)
			assert.NoDirExists(t, agentDir)
			assert.Contains(t, stderr, "Removed local files for agent 'gone'.")
		})
	}
}

func TestDeleteCleanupOnly_MixedAgents(t *testing.T) {
	withCleanupOnlyState(t)
	projectDir := cleanupOnlyProject(t)
	aliveDir := createAgentDir(t, projectDir, "alive")
	goneDir := createAgentDir(t, projectDir, "gone")
	hubCtx := cleanupOnlyHub(t, "p1", map[string]int{"alive": http.StatusOK})

	var err error
	captureStderr(t, func() { err = deleteAgentsCleanupOnly(hubCtx, nil, []string{"alive", "gone"}) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alive: agent still exists on the Hub")
	assert.NotContains(t, err.Error(), "gone:")
	assert.DirExists(t, aliveDir)
	assert.NoDirExists(t, goneDir)
}

func TestDeleteCleanupOnly_HubUnreachable(t *testing.T) {
	unreachable := errors.New("hub at http://127.0.0.1:1 is not responding: connection refused")

	t.Run("refuses without --force", func(t *testing.T) {
		withCleanupOnlyState(t)
		projectDir := cleanupOnlyProject(t)
		agentDir := createAgentDir(t, projectDir, "orphan")

		err := deleteAgentsCleanupOnly(nil, unreachable, []string{"orphan"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the Hub could not be checked")
		assert.Contains(t, err.Error(), "not responding")
		assert.Contains(t, err.Error(), "--force")
		assert.DirExists(t, agentDir)
	})

	t.Run("removes files with --force", func(t *testing.T) {
		withCleanupOnlyState(t)
		deleteForce = true
		projectDir := cleanupOnlyProject(t)
		agentDir := createAgentDir(t, projectDir, "orphan")
		resolved, _, err := config.ResolveProjectPath(projectDir)
		require.NoError(t, err)
		hubsync.UpdateSyncedAgents(resolved, []string{"orphan", "other"})

		stderr := captureStderr(t, func() { err = deleteAgentsCleanupOnly(nil, unreachable, []string{"orphan"}) })
		require.NoError(t, err)
		assert.Contains(t, stderr, "removing local files anyway because of --force")
		assert.NoDirExists(t, agentDir)

		// The synced entry goes too, so a later agent with the same name is
		// not taken for a stale one.
		st, err := config.LoadProjectState(resolved)
		require.NoError(t, err)
		assert.Equal(t, []string{"other"}, st.SyncedAgents)
	})

	t.Run("agent lookup failure refuses without --force", func(t *testing.T) {
		withCleanupOnlyState(t)
		projectDir := cleanupOnlyProject(t)
		agentDir := createAgentDir(t, projectDir, "flaky")
		hubCtx := cleanupOnlyHub(t, "p1", map[string]int{"flaky": http.StatusInternalServerError})

		var err error
		captureStderr(t, func() { err = deleteAgentsCleanupOnly(hubCtx, nil, []string{"flaky"}) })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not check the agent on the Hub")
		assert.DirExists(t, agentDir)

		deleteForce = true
		captureStderr(t, func() { err = deleteAgentsCleanupOnly(hubCtx, nil, []string{"flaky"}) })
		require.NoError(t, err)
		assert.NoDirExists(t, agentDir)
	})

	t.Run("no hub in use refuses without --force", func(t *testing.T) {
		withCleanupOnlyState(t)
		projectDir := cleanupOnlyProject(t)
		agentDir := createAgentDir(t, projectDir, "local")

		err := deleteAgentsCleanupOnly(nil, nil, []string{"local"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no Hub is in use")
		assert.Contains(t, err.Error(), "--force")
		assert.Contains(t, err.Error(), "scion delete <agent>", "local-only mode: point at a plain delete")
		assert.DirExists(t, agentDir)
	})
}

// gitRun runs git in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

func TestDeleteCleanupOnly_PreserveBranch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		preserve   bool
		wantBranch bool
	}{
		{name: "--preserve-branch keeps the branch", preserve: true, wantBranch: true},
		{name: "default deletes the branch", preserve: false, wantBranch: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCleanupOnlyState(t)
			t.Setenv("SCION_HOST_UID", "")
			preserveBranch = tc.preserve

			repo := filepath.Join(t.TempDir(), "repo")
			require.NoError(t, os.MkdirAll(repo, 0o755))
			gitRun(t, repo, "init", "-b", "main")
			require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("x\n"), 0o644))
			gitRun(t, repo, "add", "-A")
			gitRun(t, repo, "commit", "-m", "init")
			t.Chdir(repo)

			scionDir := filepath.Join(repo, ".scion")
			agentDir := filepath.Join(scionDir, "agents", "wt-agent")
			require.NoError(t, os.MkdirAll(agentDir, 0o755))
			require.NoError(t, util.CreateWorktree(filepath.Join(agentDir, "workspace"), "wt-agent"))
			projectPath = scionDir

			hubCtx := cleanupOnlyHub(t, "p1", nil)
			var err error
			captureStderr(t, func() { err = deleteAgentsCleanupOnly(hubCtx, nil, []string{"wt-agent"}) })
			require.NoError(t, err)
			assert.NoDirExists(t, agentDir)

			branches := gitRun(t, repo, "branch", "--list", "wt-agent")
			assert.Equal(t, tc.wantBranch, strings.Contains(branches, "wt-agent"), "branches: %q", branches)
		})
	}
}

func TestDeleteCmd_CleanupOnlyFlag(t *testing.T) {
	f := deleteCmd.Flags().Lookup("cleanup-only")
	require.NotNil(t, f, "--cleanup-only must be registered")
	assert.Equal(t, "false", f.DefValue)

	withCleanupOnlyState(t)
	deleteCleanupOnly = true
	deleteStopped = true
	err := deleteCmd.Args(deleteCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--cleanup-only cannot be combined with --stopped")
}
