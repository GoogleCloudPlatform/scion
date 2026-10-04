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
	"time"

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

// B1: a broker that cannot search the state directory (one the node created
// and the init container gave to the agents' user and group) falls back to
// the legacy sentinel, and otherwise leaves the worktree directory to the
// node instead of failing the create.
func TestEnsureNFSWorktreeLeaf_StateDirUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	resolved := resolveTestNFSWorkspace(t, mountRoot)
	require.NoError(t, os.MkdirAll(resolved.HostPath, 0o770))
	stateDir := provision.ProjectStateDir(resolved.HostPath)
	require.NoError(t, os.MkdirAll(stateDir, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(stateDir, provision.ProvisionSentinelFile), []byte("x"), 0o644))
	require.NoError(t, os.Chmod(stateDir, 0))
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o770) })
	rel := resolved.ServerRelativePath
	hostBase, err := filepath.EvalSymlinks(resolved.HostBase)
	require.NoError(t, err)
	wt := filepath.Join(resolved.HostPath, "worktrees", "agent-1")

	_, err = nfsSharedCheckoutProvisioned(hostBase, rel)
	require.Error(t, err)
	assert.True(t, isNFSLeafPermissionError(err), "%v", err)
	ok, err := ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
	require.NoError(t, err)
	assert.False(t, ok, "left to the node")
	assert.NoDirExists(t, wt)

	// With the legacy sentinel the checkout counts as provisioned.
	require.NoError(t, os.WriteFile(filepath.Join(resolved.HostPath, provision.ProvisionSentinelFile), []byte("x"), 0o644))
	provisioned, err := nfsSharedCheckoutProvisioned(hostBase, rel)
	require.NoError(t, err)
	assert.True(t, provisioned)
	ok, err = ensureNFSWorktreeLeaf("kubernetes", resolved, "ws-pv", "agent-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.DirExists(t, wt)
}

// N1: a broker that may not write in the state directory removes the
// worktree under the legacy lock only, instead of leaving it in place.
func TestRemoveNFSWorktree_StateDirNotWritableUsesLegacyLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f, ws := setupNFSWorktreeRemoval(t)
	runPurgeInline(t)
	stateDir := provision.ProjectStateDir(ws)
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	require.NoError(t, os.Chmod(stateDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	// The legacy lock is still taken: while a live holder (fresh owner and
	// heartbeat) has it, the removal waits nfsWorktreeLockWait, gives up and
	// leaves the worktree in place.
	legacyLock := filepath.Join(ws, ".scion-provision.lock")
	require.NoError(t, os.Mkdir(legacyLock, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(legacyLock, "owner"), []byte("0123456789abcdef0123456789abcdef"), 0o660))
	require.NoError(t, os.WriteFile(filepath.Join(legacyLock, "heartbeat"), []byte("0123456789abcdef0123456789abcdef"), 0o660))
	origWait := nfsWorktreeLockWait
	nfsWorktreeLockWait = 1500 * time.Millisecond
	t.Cleanup(func() { nfsWorktreeLockWait = origWait })
	_, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.Error(t, err, "the removal must wait for the legacy lock")
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws), "left in place")

	require.NoError(t, os.RemoveAll(legacyLock))
	path, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.NoDirExists(t, path)
	assert.NoDirExists(t, filepath.Join(stateDir, ".scion-provision.lock"))
	assert.NoDirExists(t, legacyLock)
}

// N1: the same when the broker may not create the state directory at all.
func TestRemoveNFSWorktree_StateDirNotCreatableUsesLegacyLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f, ws := setupNFSWorktreeRemoval(t)
	runPurgeInline(t)
	projectDir := filepath.Dir(ws)
	require.NoError(t, os.RemoveAll(provision.ProjectStateDir(ws)))
	require.NoError(t, os.Chmod(projectDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

	path, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.NoDirExists(t, path)
	assert.NoDirExists(t, provision.ProjectStateDir(ws))
}

// With a permission error on both locations, the first one (the state
// directory's) is returned, as a permission error.
func TestNFSSharedCheckoutProvisioned_PermissionErrorOnBoth(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	hostBase := t.TempDir()
	rel := filepath.Join("projects", testNFSWorkspaceProjectID, "workspace")
	ws := filepath.Join(hostBase, rel)
	stateDir := provision.ProjectStateDir(ws)
	for _, dir := range []string{ws, stateDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Chmod(dir, 0))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}

	provisioned, err := nfsSharedCheckoutProvisioned(hostBase, rel)
	require.Error(t, err)
	assert.False(t, provisioned)
	assert.True(t, isNFSLeafPermissionError(err), "%v", err)
	assert.Contains(t, err.Error(), filepath.Join(stateDir, provision.ProvisionSentinelFile), "the first (state directory) error is returned")
}
