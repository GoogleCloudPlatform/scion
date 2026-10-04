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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2670: the broker creates the project's provisioning state directory next
// to the workspace, with the same leaf treatment, before the pod mounts it.
func TestEnsureNFSWorkspaceLeaf_CreatesProvisionStateDir(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)

	prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
	require.NoError(t, err)
	assert.True(t, prepared)
	stateDir := provision.ProjectStateDir(res.HostPath)
	assert.Equal(t, filepath.Join(filepath.Dir(res.HostPath), "provision"), stateDir)
	assert.Equal(t, statMode(t, res.HostPath).Mode&0o7777, statMode(t, stateDir).Mode&0o7777)
	assert.Equal(t, uint32(0o2775), statMode(t, stateDir).Mode&0o7777)
}

// A symlink at the state directory's path fails the start instead of
// being followed.
func TestEnsureNFSWorkspaceLeaf_ProvisionStateDirSymlinkFails(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)
	stateDir := provision.ProjectStateDir(res.HostPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(stateDir), 0o755))
	target := t.TempDir()
	require.NoError(t, os.Symlink(target, stateDir))

	_, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil)
	require.Error(t, err)
}

// Clone-per-agent keeps its sentinel in the agent directory: no state
// directory is created.
func TestEnsureNFSAgentWorkspaceLeaf_NoProvisionStateDir(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)

	_, err := ensureNFSAgentWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", nil, "agent-1")
	require.NoError(t, err)
	assert.NoDirExists(t, provision.ProjectStateDir(res.HostPath))
}

// The broker's worktree directory is created once the shared checkout is
// provisioned, whether the sentinel is in the state directory (#2670) or in
// the workspace root (the layout before it).
func TestEnsureNFSWorktreeLeaf_SentinelLocations(t *testing.T) {
	for name, sentinelDir := range map[string]func(ws string) string{
		"state dir":      provision.ProjectStateDir,
		"legacy in root": func(ws string) string { return ws },
	} {
		t.Run(name, func(t *testing.T) {
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			resolved := resolveTestNFSWorkspace(t, mountRoot)
			require.NoError(t, os.MkdirAll(resolved.HostPath, 0o770))
			wt := filepath.Join(resolved.HostPath, "worktrees", "agent-1")

			_, err := ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
			require.NoError(t, err)
			assert.NoDirExists(t, wt, "not provisioned yet")

			dir := sentinelDir(resolved.HostPath)
			require.NoError(t, os.MkdirAll(dir, 0o770))
			require.NoError(t, os.WriteFile(filepath.Join(dir, provision.ProvisionSentinelFile), []byte("x"), 0o644))
			ok, err := ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
			require.NoError(t, err)
			assert.True(t, ok)
			assert.DirExists(t, wt)
		})
	}
}

// The delete path takes the provisioning lock in the state directory too
// (after the legacy one): it creates the directory if needed and leaves no
// live lock behind.
func TestRemoveNFSWorktree_UsesProvisionStateDir(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	runPurgeInline(t)
	stateDir := provision.ProjectStateDir(ws)
	require.NoError(t, os.RemoveAll(stateDir))

	path, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.NoDirExists(t, path)
	assert.DirExists(t, stateDir)
	assert.Equal(t, uint32(0o2775), statMode(t, stateDir).Mode&0o7777)
	assert.NoDirExists(t, filepath.Join(stateDir, ".scion-provision.lock"))
	assert.NoDirExists(t, filepath.Join(ws, ".scion-provision.lock"))
}

// A symlink at the state directory's path makes the removal fail and leave
// the worktree in place.
func TestRemoveNFSWorktree_ProvisionStateDirSymlinkFails(t *testing.T) {
	f, ws := setupNFSWorktreeRemoval(t)
	runPurgeInline(t)
	stateDir := provision.ProjectStateDir(ws)
	require.NoError(t, os.RemoveAll(stateDir))
	require.NoError(t, os.Symlink(t.TempDir(), stateDir))

	_, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.Error(t, err)
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws))
}
