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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cloudRunTestHomeLeaf is a Cloud Run agent home leaf name next to its
// workspace in agents/<slug> (projectfs AgentHome: home-<agentID>).
const cloudRunTestHomeLeaf = "home-6f1c2b3a-4d5e-4f60-8a9b-0c1d2e3f4a5b"

// cloudRunTestAgentDir lays out a Cloud Run clone-per-agent agent on the
// export as the runtime does (cloudrun_runtime.go): its own workspace with
// a clone in it, and its home next to it in agents/<slug>.
func cloudRunTestAgentDir(t *testing.T, mountRoot, name string) string {
	t.Helper()
	agentDir := nfsTestAgentDir(mountRoot, name)
	ws := filepath.Join(agentDir, provision.AgentWorkspaceDir)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git"), 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "README.md"), []byte("hi\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, cloudRunTestHomeLeaf), 0o770))
	return agentDir
}

// Delete on the Cloud Run runtime removes the agent's own workspace on the
// export, leaves its home (the hub removes that) and other agents alone,
// and never touches a worktree of the same name under the shared checkout:
// worktree removal stays Kubernetes-only.
func TestRemoveNFSAgentFiles_CloudRunRemovesAgentWorkspace(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	agentDir := cloudRunTestAgentDir(t, mountRoot, "test-agent")
	otherDir := cloudRunTestAgentDir(t, mountRoot, "other-agent")
	runAgentWorkspacePurgeInline(t)
	// A directory where a Kubernetes worktree of this agent would be: it
	// must survive, which shows RemoveNFSWorktree did nothing.
	sharedWS := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "workspace")
	worktree := filepath.Join(sharedWS, "worktrees", "test-agent")
	require.NoError(t, os.MkdirAll(worktree, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "keep"), []byte("x"), 0o644))

	m := kubernetesTestManager("cloudrun")
	path, err := m.RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Empty(t, path, "worktree removal is a no-op on Cloud Run")

	paths, err := m.RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Equal(t, []string{agentDir}, paths)
	assert.NoDirExists(t, filepath.Join(agentDir, provision.AgentWorkspaceDir), "the agent's workspace is removed")
	assert.DirExists(t, filepath.Join(agentDir, cloudRunTestHomeLeaf), "the agent's home is the hub's to remove")
	assert.FileExists(t, filepath.Join(otherDir, provision.AgentWorkspaceDir, "README.md"), "other agents are untouched")
	assert.FileExists(t, filepath.Join(worktree, "keep"), "the shared checkout's worktrees are untouched")

	// Control: on a runtime outside the gate nothing is removed.
	readme := filepath.Join(otherDir, provision.AgentWorkspaceDir, "README.md")
	paths, err = kubernetesTestManager("docker").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "other-agent")
	require.NoError(t, err)
	assert.Empty(t, paths)
	assert.FileExists(t, readme)
}

func TestCloudRunKeepSharedCheckout(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	resolved := resolveTestNFSWorkspace(t, mountRoot)

	t.Run("existing agent without record or workspace keeps the shared checkout", func(t *testing.T) {
		keep, reason := cloudRunKeepSharedCheckout(false, t.TempDir(), resolved, "test-agent")
		assert.True(t, keep)
		assert.Contains(t, reason, "keeps the shared checkout")
	})
	t.Run("an existing home in the agent directory does not count", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(nfsTestAgentDir(mountRoot, "homed-agent"), cloudRunTestHomeLeaf), 0o770))
		keep, _ := cloudRunKeepSharedCheckout(false, t.TempDir(), resolved, "homed-agent")
		assert.True(t, keep, "Cloud Run homes live in agents/<slug>; only the workspace decides")
	})
	t.Run("a create gets its own workspace", func(t *testing.T) {
		keep, _ := cloudRunKeepSharedCheckout(true, t.TempDir(), resolved, "test-agent")
		assert.False(t, keep)
	})
	t.Run("a recorded agent gets its own workspace", func(t *testing.T) {
		dir := t.TempDir()
		recordCloudRunAgentWorkspace(dir, "test-agent")
		require.FileExists(t, filepath.Join(dir, cloudRunAgentWorkspaceRecordFile))
		keep, _ := cloudRunKeepSharedCheckout(false, dir, resolved, "test-agent")
		assert.False(t, keep)
	})
	t.Run("the Kubernetes record does not count", func(t *testing.T) {
		dir := t.TempDir()
		recordNFSAgentDir(dir, "test-agent")
		keep, _ := cloudRunKeepSharedCheckout(false, dir, resolved, "test-agent")
		assert.True(t, keep, "every create writes nfs-agent-dir.json, including Cloud Run agents that used the shared checkout")
	})
	t.Run("an existing own workspace is kept", func(t *testing.T) {
		cloudRunTestAgentDir(t, mountRoot, "ws-agent")
		keep, _ := cloudRunKeepSharedCheckout(false, t.TempDir(), resolved, "ws-agent")
		assert.False(t, keep)
	})
	t.Run("an unmounted export decides nothing", func(t *testing.T) {
		gone := resolved
		gone.HostBase = filepath.Join(t.TempDir(), "not-mounted")
		keep, _ := cloudRunKeepSharedCheckout(false, t.TempDir(), gone, "test-agent")
		assert.False(t, keep)
	})
}

// A fresh Cloud Run clone-per-agent start on a shared Layout records the
// agent's own workspace and keeps clone-per-agent; a later start of an
// agent with no record and no own workspace keeps the shared checkout,
// with shared-plain sent to the runtime.
func TestStart_CloudRunClonePerAgentRecordAndKeepShared(t *testing.T) {
	gc := &api.GitCloneConfig{URL: "https://example.com/repo.git"}

	cfg, _, ext, _, err := sharedLayoutStart(t, "cloudrun", func(o *api.StartOptions) {
		o.GitClone = gc
		o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(ext, cloudRunAgentWorkspaceRecordFile), "a create records the agent's own workspace")
	assert.Contains(t, cfg.Env, "SCION_WORKSPACE_MODE=clone-per-agent")

	cfg, _, ext, _, err = sharedLayoutStart(t, "cloudrun", func(o *api.StartOptions) {
		o.GitClone = gc
		o.Env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
		o.FreshProvision = false
	})
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(ext, cloudRunAgentWorkspaceRecordFile))
	assert.Contains(t, cfg.Env, "SCION_WORKSPACE_MODE=shared-plain", "an existing agent keeps the shared checkout")
	assert.NotContains(t, cfg.Env, "SCION_WORKSPACE_MODE=clone-per-agent")
}

// A provision-only create records the agent's own workspace, so its later
// start (not a create) keeps clone-per-agent instead of being mistaken for
// an existing agent that worked in the shared checkout.
func TestProvisionThenStart_CloudRunClonePerAgentKeepsOwnWorkspace(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	var cfg runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "cloudrun" },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			cfg = rc
			return "mock-id", nil
		},
	})
	env := func() map[string]string {
		return map[string]string{"SCION_PROJECT_ID": testNFSWorkspaceProjectID, "SCION_WORKSPACE_MODE": "clone-per-agent"}
	}
	opts := api.StartOptions{Name: "test-agent", ProjectPath: f.projectScionDir, NoAuth: true, GitClone: testGitClone}
	provisionOpts := opts
	provisionOpts.Env = env()
	provisionOpts.FreshProvision = true
	_, err := mgr.Provision(context.Background(), provisionOpts)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(f.projectScionDir, "agents", "test-agent", cloudRunAgentWorkspaceRecordFile))

	opts.Env = env()
	_, err = mgr.Start(context.Background(), opts)
	require.NoError(t, err)
	assert.Contains(t, cfg.Env, "SCION_WORKSPACE_MODE=clone-per-agent")
	assert.NotContains(t, cfg.Env, "SCION_WORKSPACE_MODE=shared-plain")
}
