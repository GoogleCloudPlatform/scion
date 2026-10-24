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

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

func presenceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// presenceProject makes a project directory (with its .scion) at a fresh
// path; withRepo makes the project a git repository with one commit.
func presenceProject(t *testing.T, withRepo bool) (root string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents"), 0o755))
	if withRepo {
		presenceGit(t, root, "init", "-q", "-b", "main")
		require.NoError(t, os.WriteFile(filepath.Join(root, "README"), []byte("x"), 0o644))
		presenceGit(t, root, "add", "README")
		presenceGit(t, root, "commit", "-q", "-m", "init")
	}
	return root
}

// TestAgentFilesPresence: each location deleteAgentFiles removes makes the
// agent's files present, and deleteAgentFiles' removal makes them absent.
func TestAgentFilesPresence(t *testing.T) {
	t.Run("nothing", func(t *testing.T) {
		root := presenceProject(t, false)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""))
	})
	t.Run("agent directory", func(t *testing.T) {
		root := presenceProject(t, false)
		require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", "worker", "home"), 0o755))
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", root, ""))
		_, err := DeleteAgentFilesForHubProject("worker", root, "", true)
		require.NoError(t, err)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""))
	})
	t.Run("branch named after the agent", func(t *testing.T) {
		root := presenceProject(t, true)
		presenceGit(t, root, "branch", "worker")
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", root, ""))
		_, err := DeleteAgentFilesForHubProject("worker", root, "", true)
		require.NoError(t, err)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""))
	})
	t.Run("worktree entry whose directory is gone", func(t *testing.T) {
		root := presenceProject(t, true)
		wt := filepath.Join(root, config.DotScion, "agents", "worker", "workspace")
		presenceGit(t, root, "worktree", "add", "-q", "--detach", wt)
		require.NoError(t, os.RemoveAll(filepath.Join(root, config.DotScion, "agents", "worker")))
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", root, ""), "the stale entry is the agent's")
		_, err := DeleteAgentFilesForHubProject("worker", root, "", true)
		require.NoError(t, err)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""))
	})
	t.Run("shared-base worktree", func(t *testing.T) {
		root := presenceProject(t, false)
		base := filepath.Join(root, "workspace")
		require.NoError(t, os.MkdirAll(base, 0o755))
		presenceGit(t, base, "init", "-q", "-b", "main")
		require.NoError(t, os.MkdirAll(filepath.Join(base, "worktrees", "worker"), 0o755))
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", root, ""))
		require.NoError(t, os.RemoveAll(filepath.Join(base, "worktrees", "worker")))
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""))
	})
	t.Run("split-storage agent directory", func(t *testing.T) {
		root := presenceProject(t, true)
		scionDir := filepath.Join(root, config.DotScion)
		require.NoError(t, config.WriteProjectID(scionDir, "3b9e7a1c-2d4f-4e6a-8b0c-1d2e3f4a5b6c"))
		ext, err := config.GetGitProjectExternalAgentsDir(scionDir)
		require.NoError(t, err)
		require.NotEmpty(t, ext, "the project has split storage")
		require.NoError(t, os.MkdirAll(filepath.Join(ext, "worker", "home"), 0o755))
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", root, ""))
		_, err = DeleteAgentFilesForHubProject("worker", root, "", true)
		require.NoError(t, err)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""))
	})
	t.Run("state directory from the Hub project ID", func(t *testing.T) {
		root := presenceProject(t, false)
		const hubID = "6d5c4b3a-2f1e-4d0c-9b8a-7f6e5d4c3b2a"
		projectDir, err := config.GetResolvedProjectDir(root)
		require.NoError(t, err)
		stateDir := hubStateDirToRemove(projectDir, "worker", hubID, "")
		require.NotEmpty(t, stateDir, "a state directory distinct from the in-project one")
		require.NoError(t, os.MkdirAll(stateDir, 0o755))
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", root, hubID))
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""), "found only through the Hub project ID")
		_, err = DeleteAgentFilesForHubProject("worker", root, hubID, true)
		require.NoError(t, err)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, hubID))
	})
	t.Run("global project workspace", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		globalDir, err := config.GetGlobalDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(globalDir, "workspace", "worker"), 0o755))
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", globalDir, ""))
		_, err = DeleteAgentFilesForHubProject("worker", globalDir, "", true)
		require.NoError(t, err)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", globalDir, ""))
	})
	t.Run("sharer registry entry", func(t *testing.T) {
		root := presenceProject(t, true)
		projectDir, err := config.GetResolvedProjectDir(root)
		require.NoError(t, err)
		wt := filepath.Join(projectDir, "agents", "worker", "workspace")
		require.NoError(t, provision.RegisterSharer(root, projectDir, "worker", wt, "worker"))
		assert.Equal(t, FilesPresent, AgentFilesPresence("worker", root, ""), "registered as a sharer, with no directory")
		_, err = DeleteAgentFilesForHubProject("worker", root, "", true)
		require.NoError(t, err)
		assert.Equal(t, FilesAbsent, AgentFilesPresence("worker", root, ""))
	})
	t.Run("unknown", func(t *testing.T) {
		root := presenceProject(t, false)
		assert.Equal(t, FilesUnknown, AgentFilesPresence("worker", filepath.Join(root, "missing"), ""), "no project directory")
		assert.Equal(t, FilesUnknown, AgentFilesPresence("../escape", root, ""), "an invalid name")
	})
}

func TestFilesPresenceString(t *testing.T) {
	assert.Equal(t, "absent", FilesAbsent.String())
	assert.Equal(t, "present", FilesPresent.String())
	assert.Equal(t, "unknown", FilesUnknown.String())
	var zero FilesPresence
	assert.Equal(t, FilesUnknown, zero, "the zero value is never a proof")
}
