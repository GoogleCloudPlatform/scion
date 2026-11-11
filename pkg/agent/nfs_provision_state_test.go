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

// These tests exercise unix-only, Windows-gated agent storage code.
//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2670: the broker creates the project's provisioning state directory next
// to the workspace, with the same leaf treatment, before the pod mounts it.
func TestEnsureNFSWorkspaceLeaf_CreatesProvisionStateDir(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)

	prepared, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, runtime.NFSPodShare{ClaimName: "ws-pv"}, nil)
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

	_, err := ensureNFSWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, runtime.NFSPodShare{ClaimName: "ws-pv"}, nil)
	require.Error(t, err)
}

// Clone-per-agent keeps its sentinel in the agent directory: no state
// directory is created.
func TestEnsureNFSAgentWorkspaceLeaf_NoProvisionStateDir(t *testing.T) {
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot)

	_, err := ensureNFSAgentWorkspaceLeaf("kubernetes", testNFSWorkspaceProjectID, res, runtime.NFSPodShare{ClaimName: "ws-pv"}, nil, "agent-1")
	require.NoError(t, err)
	assert.NoDirExists(t, provision.ProjectStateDir(res.HostPath))
}

// The broker's worktree directory is created once the shared checkout is
// provisioned, which only the state-directory sentinel says (#2670). A
// sentinel in the workspace root (the layout before it) is not read
// (AC-L7): the checkout still counts as unprovisioned.
func TestEnsureNFSWorktreeLeaf_SentinelLocations(t *testing.T) {
	for name, tc := range map[string]struct {
		sentinelDir func(ws string) string
		provisioned bool
	}{
		"state dir":      {sentinelDir: provision.ProjectStateDir, provisioned: true},
		"workspace root": {sentinelDir: func(ws string) string { return ws }, provisioned: false},
	} {
		t.Run(name, func(t *testing.T) {
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			resolved := resolveTestNFSWorkspace(t, mountRoot)
			require.NoError(t, os.MkdirAll(resolved.HostPath, 0o770))
			wt := filepath.Join(resolved.HostPath, "worktrees", "agent-1")

			_, err := ensureNFSWorktreeLeaf("kubernetes", resolved, runtime.NFSPodShare{ClaimName: "ws-pv"}, "agent-1")
			require.NoError(t, err)
			assert.NoDirExists(t, wt, "not provisioned yet")

			dir := tc.sentinelDir(resolved.HostPath)
			require.NoError(t, os.MkdirAll(dir, 0o770))
			require.NoError(t, os.WriteFile(filepath.Join(dir, provision.ProvisionSentinelFile), []byte("x"), 0o644))
			ok, err := ensureNFSWorktreeLeaf("kubernetes", resolved, runtime.NFSPodShare{ClaimName: "ws-pv"}, "agent-1")
			require.NoError(t, err)
			assert.True(t, ok)
			if tc.provisioned {
				assert.DirExists(t, wt)
			} else {
				assert.NoDirExists(t, wt, "a workspace-root sentinel must not count")
			}
		})
	}
}

// The delete path takes the provisioning lock in the state directory, and
// only there (AC-L1): it creates the directory if needed and leaves no live
// lock behind, and nothing is created in the workspace root.
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
// and the init container gave to the agents' user and group) leaves the
// worktree directory to the node instead of failing the create. A sentinel
// in the workspace root does not change that (AC-L7).
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
	ok, err := ensureNFSWorktreeLeaf("kubernetes", resolved, runtime.NFSPodShare{ClaimName: "ws-pv"}, "agent-1")
	require.NoError(t, err)
	assert.False(t, ok, "left to the node")
	assert.NoDirExists(t, wt)

	// A workspace-root sentinel is not read: still left to the node.
	require.NoError(t, os.WriteFile(filepath.Join(resolved.HostPath, provision.ProvisionSentinelFile), []byte("x"), 0o644))
	_, err = nfsSharedCheckoutProvisioned(hostBase, rel)
	require.Error(t, err)
	assert.True(t, isNFSLeafPermissionError(err), "%v", err)
	ok, err = ensureNFSWorktreeLeaf("kubernetes", resolved, runtime.NFSPodShare{ClaimName: "ws-pv"}, "agent-1")
	require.NoError(t, err)
	assert.False(t, ok, "left to the node")
	assert.NoDirExists(t, wt)
}

// A broker that may not write in the state directory cannot take the
// provisioning lock, so the removal fails and leaves the worktree in place
// (AC-L1): no lock is ever taken in the workspace root.
func TestRemoveNFSWorktree_StateDirNotWritableLeftInPlace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f, ws := setupNFSWorktreeRemoval(t)
	runPurgeInline(t)
	stateDir := provision.ProjectStateDir(ws)
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	require.NoError(t, os.Chmod(stateDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	_, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "left in place")
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws), "left in place")
	assertNoWorkspaceRootLock(t, ws)
}

// The same when the broker may not create the state directory at all.
func TestRemoveNFSWorktree_StateDirNotCreatableLeftInPlace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f, ws := setupNFSWorktreeRemoval(t)
	runPurgeInline(t)
	projectDir := filepath.Dir(ws)
	require.NoError(t, os.RemoveAll(provision.ProjectStateDir(ws)))
	require.NoError(t, os.Chmod(projectDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

	_, err := kubernetesTestManager("kubernetes").RemoveNFSWorktree(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.Error(t, err)
	assert.True(t, provision.IsRealWorktreeDir(provision.WorktreePath(ws, "test-agent"), ws), "left in place")
	assert.NoDirExists(t, provision.ProjectStateDir(ws))
	assertNoWorkspaceRootLock(t, ws)
}

// assertNoWorkspaceRootLock checks that nothing named like the provisioning
// lock is in the workspace root.
func assertNoWorkspaceRootLock(t *testing.T, ws string) {
	t.Helper()
	entries, err := os.ReadDir(ws)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".scion-provision.lock"), "lock entry %s in the workspace root", e.Name())
	}
}

// A permission error on the state directory is returned, as a permission
// error naming the state-directory sentinel.
func TestNFSSharedCheckoutProvisioned_PermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	hostBase := t.TempDir()
	rel := filepath.Join("projects", testNFSWorkspaceProjectID, "workspace")
	ws := filepath.Join(hostBase, rel)
	stateDir := provision.ProjectStateDir(ws)
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	require.NoError(t, os.Chmod(stateDir, 0))
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	provisioned, err := nfsSharedCheckoutProvisioned(hostBase, rel)
	require.Error(t, err)
	assert.False(t, provisioned)
	assert.True(t, isNFSLeafPermissionError(err), "%v", err)
	assert.Contains(t, err.Error(), filepath.Join(stateDir, provision.ProvisionSentinelFile))
}

// The workspace root is not looked at (AC-L7): with no state directory, a
// sentinel in the workspace root, and the workspace root unreadable, the
// checkout is reported unprovisioned without an error.
func TestNFSSharedCheckoutProvisioned_WorkspaceRootIgnored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	resolved := resolveTestNFSWorkspace(t, mountRoot)
	ws := resolved.HostPath
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, provision.ProvisionSentinelFile), []byte("x"), 0o644))
	require.NoDirExists(t, provision.ProjectStateDir(ws))
	require.NoError(t, os.Chmod(ws, 0))
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })
	hostBase, err := filepath.EvalSymlinks(resolved.HostBase)
	require.NoError(t, err)

	provisioned, err := nfsSharedCheckoutProvisioned(hostBase, resolved.ServerRelativePath)
	require.NoError(t, err)
	assert.False(t, provisioned)
}
