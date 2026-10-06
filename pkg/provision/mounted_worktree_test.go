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

package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLockWait is the lock wait the removal tests pass.
const testLockWait = 30 * time.Second

// removedDirs lists the directories RemoveMountedWorktree moved aside.
func removedDirs(t *testing.T, workspace string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(workspace, "worktrees", removedWorktreePrefix+"*"))
	require.NoError(t, err)
	return dirs
}

// mountedWorktreeInput is the input the Kubernetes init container builds
// for worktree-per-agent mode: the workspace is both the shared checkout
// and the sentinel/lock directory.
func mountedWorktreeInput(workspace, origin, agentID, branch string) ProvisionInput {
	return ProvisionInput{
		Resolved:            ResolvedWorkspace{HostPath: workspace},
		ProjectID:           "proj-1",
		AgentID:             agentID,
		AgentName:           branch,
		Mode:                store.SharingModeWorktreePerAgent,
		GitClone:            &api.GitCloneConfig{URL: origin},
		NFSUID:              os.Getuid(),
		NFSGID:              os.Getgid(),
		SentinelDir:         workspace,
		RequireChownSuccess: true,
		MountedWorktree:     true,
	}
}

// recordLchown replaces lchownFile for the test, recording every path and
// failing for paths where fail returns true.
func recordLchown(t *testing.T, fail func(string) bool) *[]string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	orig := lchownFile
	lchownFile = func(name string, uid, gid int) error {
		mu.Lock()
		paths = append(paths, name)
		mu.Unlock()
		if fail != nil && fail(name) {
			return errors.New("operation not permitted")
		}
		return nil
	}
	t.Cleanup(func() { lchownFile = orig })
	return &paths
}

// The worktree is added after the shared checkout's one-time chown, so the
// agent's directory, the worktrees/ directory and the shared .git (the
// worktree's admin directory, the new branch ref) are chowned afterwards.
func TestProvisionShared_MountedWorktree_ChownsWorktreeAndGitDir(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	paths := recordLchown(t, nil)

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))

	want := []string{
		filepath.Join(workspace, "worktrees"),
		filepath.Join(workspace, "worktrees", "agent-1"),
		filepath.Join(workspace, "worktrees", "agent-1", "README.md"),
		filepath.Join(workspace, ".git", "worktrees", "agent-1"),
		filepath.Join(workspace, ".git", "refs", "heads", "agent-one"),
	}
	for _, p := range want {
		assert.Contains(t, *paths, p, "not chowned after the worktree was added")
	}

	// A restart reuses the worktree and chowns only what it rewrote: the
	// shared .git/config and the sharer marker, not the worktree.
	*paths = nil
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	assert.Contains(t, *paths, filepath.Join(workspace, ".git", "config"))
	assert.Contains(t, *paths, sharerPath(workspace, "agent-one"))
	for _, p := range *paths {
		assert.False(t, strings.HasPrefix(p, filepath.Join(workspace, "worktrees")), "reused worktree chowned again: %s", p)
		assert.False(t, strings.HasPrefix(p, filepath.Join(workspace, ".git", "worktrees")), "reused admin entry chowned again: %s", p)
	}

	// A second agent: only its own worktree and git entries are chowned,
	// not the first agent's or the rest of the shared checkout.
	*paths = nil
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "feature/two")))
	for _, p := range []string{
		filepath.Join(workspace, "worktrees", "agent-2", "README.md"),
		filepath.Join(workspace, ".git", "worktrees", "agent-2"),
		filepath.Join(workspace, ".git", "refs", "heads", "feature"),
		filepath.Join(workspace, ".git", "refs", "heads", "feature", "two"),
		filepath.Join(workspace, ".git", "logs", "refs", "heads", "feature", "two"),
	} {
		assert.Contains(t, *paths, p)
	}
	for _, p := range *paths {
		assert.False(t, strings.HasPrefix(p, filepath.Join(workspace, "worktrees", "agent-1")), "another agent's worktree chowned: %s", p)
		assert.NotEqual(t, filepath.Join(workspace, "README.md"), p, "the shared checkout's files are chowned only when first provisioned")
		assert.False(t, strings.HasPrefix(p, filepath.Join(workspace, ".git", "objects")), "objects chowned again: %s", p)
	}
}

// A failed chown of the worktree is fatal unless best-effort chown was
// requested, the same rule as for the shared checkout.
func TestProvisionShared_MountedWorktree_ChownFailure(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	// Let the first provisioning succeed, then fail chowns inside worktrees/.
	require.NoError(t, func() error {
		recordLchown(t, nil)
		return ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one"))
	}())
	recordLchown(t, func(p string) bool { return strings.Contains(p, string(filepath.Separator)+"worktrees") })

	in := mountedWorktreeInput(workspace, origin, "agent-2", "agent-two")
	err := ProvisionShared(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chown")

	in = mountedWorktreeInput(workspace, origin, "agent-3", "agent-three")
	in.RequireChownSuccess = false
	assert.NoError(t, ProvisionShared(in), "best-effort chown must not fail provisioning")
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-3"), workspace))
}

// With a git too old for relative worktree paths, the agent's directory is
// created empty, so the agent container clones its own checkout into it.
func TestProvisionShared_MountedWorktree_OldGitLeavesEmptyDir(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	recordLchown(t, nil)
	orig := gitSupportsRelativeWorktrees
	gitSupportsRelativeWorktrees = func() bool { return false }
	t.Cleanup(func() { gitSupportsRelativeWorktrees = orig })

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))

	entries, err := os.ReadDir(WorktreePath(workspace, "agent-1"))
	require.NoError(t, err, "the agent's directory must exist for its mount")
	assert.Empty(t, entries)
	_, err = os.Stat(filepath.Join(workspace, ProvisionSentinelFile))
	assert.NoError(t, err, "the shared checkout is still provisioned")
}

// Without MountedWorktree (the broker's own host-side worktree flow) an
// existing directory that is not a worktree is still refused, empty or not.
func TestProvisionShared_WorktreeWithoutMountedFlag_RefusesExistingDir(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	recordLchown(t, nil)
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))

	require.NoError(t, os.MkdirAll(WorktreePath(workspace, "agent-2"), 0o770))
	in := mountedWorktreeInput(workspace, origin, "agent-2", "agent-two")
	in.MountedWorktree = false
	err := ProvisionShared(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a git worktree of this checkout")
}

// A symlink or a non-empty directory without its own .git at the agent's
// path is never reused, even with MountedWorktree.
func TestProvisionShared_MountedWorktree_RefusesOtherEntries(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	recordLchown(t, nil)
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))

	target := t.TempDir()
	require.NoError(t, os.Symlink(target, WorktreePath(workspace, "agent-2")))
	err := ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "agent-two"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a git worktree of this checkout")

	dir := WorktreePath(workspace, "agent-3")
	require.NoError(t, os.MkdirAll(dir, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644))
	err = ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-3", "agent-three"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a git worktree of this checkout")
}

// A MountedWorktree dispatch refuses a sharer-registry entry that is not a
// genuine worktree of this checkout, instead of creating a second worktree
// for the requested branch, matching upstream's own outcome for this
// candidate shape. The local (non-Mounted) path is unaffected: see
// TestProvision_EnsureWorktree_RegistryNamesDirectChildNonWorktree_Refused
// (pkg/provision/provision_test.go) for its current, separately-tracked
// behavior on the same input shape.
func TestProvisionShared_MountedWorktree_RegistryNamesNonWorktree_Refused(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "branch-one")))

	// Register an entry for a different branch that names a non-worktree
	// directory under worktrees/ — in-tree-shaped, but not a worktree.
	nonWorktreeDir := WorktreePath(workspace, "agent-x")
	require.NoError(t, os.MkdirAll(nonWorktreeDir, 0o770))
	require.NoError(t, RegisterSharer(workspace, "", "branch-two", nonWorktreeDir, "some-other-agent"))

	err := ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "branch-two"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "which is not a direct worktree of this checkout; refusing to join it")
	assert.NoDirExists(t, WorktreePath(workspace, "agent-2"), "no worktree should be created for the refused dispatch")
}

// provisionTwoMountedWorktrees provisions a shared checkout with worktrees
// for agent-1 and agent-2 and returns the workspace.
func provisionTwoMountedWorktrees(t *testing.T) string {
	t.Helper()
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "agent-two")))
	return workspace
}

// Removal takes only worktrees/<name>: the worktree, its git entry and its
// sharer registry entry go; the branch, the shared checkout and the other
// agent's worktree stay.
func TestRemoveMountedWorktree_RemovesOnlyThatWorktree(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	other := filepath.Join(WorktreePath(workspace, "agent-2"), "work.txt")
	require.NoError(t, os.WriteFile(other, []byte("in progress\n"), 0o644))
	dirty := filepath.Join(WorktreePath(workspace, "agent-1"), "uncommitted.txt")
	require.NoError(t, os.WriteFile(dirty, []byte("x\n"), 0o644))

	require.NoError(t, RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", testLockWait))

	assert.NoDirExists(t, WorktreePath(workspace, "agent-1"))
	assert.NoDirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"))
	// The files are moved aside until PurgeRemovedWorktrees deletes them.
	removed := removedDirs(t, workspace)
	require.Len(t, removed, 1)
	assert.FileExists(t, filepath.Join(removed[0], "uncommitted.txt"))
	require.NoError(t, PurgeRemovedWorktrees(workspace))
	assert.Empty(t, removedDirs(t, workspace))
	assert.FileExists(t, other)
	assert.FileExists(t, other)
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-2"), workspace))
	_, _, found, err := FindBranchForAgent(workspace, "", "agent-1")
	require.NoError(t, err)
	assert.False(t, found, "agent-1 should be dropped from the sharer registry")
	sharers, _, err := ListSharers(workspace, "", "agent-two")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-2"}, sharers)
	out, err := gitInSharedCheckout(t.Context(), workspace, workspace, "branch", "--list", "agent-one")
	require.NoError(t, err)
	assert.Contains(t, out, "agent-one", "the branch is kept")

	// Removing again, or an agent that never had a worktree, is a no-op.
	require.NoError(t, RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", testLockWait))
	require.NoError(t, RemoveMountedWorktree(t.Context(), workspace, "", "agent-9", testLockWait))
}

// Names that are not agent slugs are refused before any path is built.
func TestRemoveMountedWorktree_RejectsNonSlugNames(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	for _, name := range []string{"", ".", "..", "../agent-1", "a/b", `a\b`, "Agent-1", "agent-1/.", "a.b"} {
		err := RemoveMountedWorktree(t.Context(), workspace, "", name, testLockWait)
		assert.Error(t, err, "name %q", name)
	}
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-2"), workspace))
}

// Anything at worktrees/<name> that is not a worktree of this checkout is
// left in place: a symlink (and what it points to), a regular file, a
// separate clone, a non-empty plain directory. An empty directory (made
// for the mount, never filled) is removed.
func TestRemoveMountedWorktree_LeavesOtherEntries(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	wtDir := filepath.Join(workspace, "worktrees")

	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(wtDir, "linked")))
	err := RemoveMountedWorktree(t.Context(), workspace, "", "linked", testLockWait)
	assert.ErrorContains(t, err, "left in place")
	assert.FileExists(t, keep)
	fi, lerr := os.Lstat(filepath.Join(wtDir, "linked"))
	require.NoError(t, lerr)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink)

	// A symlink to another agent's worktree is not that worktree.
	require.NoError(t, os.Symlink(WorktreePath(workspace, "agent-2"), filepath.Join(wtDir, "alias")))
	assert.ErrorContains(t, RemoveMountedWorktree(t.Context(), workspace, "", "alias", testLockWait), "left in place")
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-2"), workspace))

	require.NoError(t, os.WriteFile(filepath.Join(wtDir, "file"), []byte("x"), 0o644))
	assert.ErrorContains(t, RemoveMountedWorktree(t.Context(), workspace, "", "file", testLockWait), "left in place")
	assert.FileExists(t, filepath.Join(wtDir, "file"))

	clone := filepath.Join(wtDir, "cloned")
	require.NoError(t, os.MkdirAll(filepath.Join(clone, ".git"), 0o755))
	err = RemoveMountedWorktree(t.Context(), workspace, "", "cloned", testLockWait)
	assert.ErrorIs(t, err, ErrSeparateCheckout)
	assert.DirExists(t, filepath.Join(clone, ".git"))

	plain := filepath.Join(wtDir, "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(plain, "f"), []byte("x"), 0o644))
	assert.ErrorContains(t, RemoveMountedWorktree(t.Context(), workspace, "", "plain", testLockWait), "left in place")
	assert.FileExists(t, filepath.Join(plain, "f"))

	empty := filepath.Join(wtDir, "empty")
	require.NoError(t, os.MkdirAll(empty, 0o770))
	require.NoError(t, RemoveMountedWorktree(t.Context(), workspace, "", "empty", testLockWait))
	assert.NoDirExists(t, empty)

	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-2"), workspace))
}

// The removal holds the project's provisioning lock: while another holder
// has it, the removal waits up to lockWait and then gives up, leaving the
// worktree in place.
func TestRemoveMountedWorktree_WaitsForProvisioningLock(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	held, err := acquireFileLock(t.Context(), workspace)
	require.NoError(t, err)
	defer func() { _ = held.release() }()

	start := time.Now()
	err = RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", 1500*time.Millisecond)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 20*time.Second)
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	assert.Empty(t, removedDirs(t, workspace))
}

// worktreeGit runs git in dir and returns its trimmed output.
func worktreeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitInSharedCheckout(t.Context(), dir, dir, args...)
	require.NoError(t, err, "git %v: %s", args, out)
	return out
}

// A removal that stopped partway (the worktree moved aside but not yet
// dropped from git, or its files only partly deleted) does not stop an
// agent created again with the same name, and the leftovers are deleted
// by the next purge.
func TestRemoveMountedWorktree_InterruptedThenRecreate(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "agent-two")))

	// Stopped after the move: git still has the worktree's entry.
	aside, err := moveWorktreeAside(workspace, "agent-1", removedWorktreePrefix)
	require.NoError(t, err)
	assert.DirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"))
	assert.NoFileExists(t, filepath.Join(aside, ".git"), "the moved directory is no longer linked to git")
	// Stopped while deleting the files.
	require.NoError(t, os.Remove(filepath.Join(aside, "README.md")))

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	assert.Equal(t, linkedWorktreeOK, linkedWorktreeState(workspace, WorktreePath(workspace, "agent-1")))
	assert.Equal(t, "agent-one", currentBranch(t.Context(), WorktreePath(workspace, "agent-1")))
	assert.FileExists(t, filepath.Join(WorktreePath(workspace, "agent-1"), "README.md"))

	require.NoError(t, PurgeRemovedWorktrees(workspace))
	assert.NoDirExists(t, aside)
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-2"), workspace))
	assert.NotContains(t, worktreeGit(t, workspace, "status", "--porcelain", "--untracked-files=all"), "worktrees")
}

// The removal only touches worktrees/<name> inside a plain worktrees/
// directory.
func TestRemoveMountedWorktree_RefusesSymlinkedWorktreesDir(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.Rename(filepath.Join(workspace, "worktrees"), elsewhere))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(workspace, "worktrees")))
	keep := filepath.Join(elsewhere, removedWorktreePrefix+"x-1", "keep")
	require.NoError(t, os.MkdirAll(filepath.Dir(keep), 0o755))
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))

	err := RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", testLockWait)
	assert.ErrorContains(t, err, "is not a plain directory")
	assert.DirExists(t, filepath.Join(elsewhere, "agent-1"))
	assert.NoError(t, PurgeRemovedWorktrees(workspace))
	assert.DirExists(t, filepath.Join(elsewhere, "agent-1"))
	assert.FileExists(t, keep, "the purge does not follow a symlinked worktrees/ directory")
}

// A worktree whose git entry points to another directory is left in place
// by the removal, and stops the agent's start with an error naming it.
func TestMountedWorktree_AdminEntryPointsElsewhere(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	gitdir := filepath.Join(workspace, ".git", "worktrees", "agent-1", "gitdir")
	require.NoError(t, os.WriteFile(gitdir, []byte(filepath.Join(t.TempDir(), "other", ".git")+"\n"), 0o644))

	err := ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "points to another directory")
	assert.Contains(t, err.Error(), "worktrees/agent-1 of the shared checkout of project proj-1")
	assert.NotContains(t, err.Error(), workspace)

	err = RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", testLockWait)
	assert.ErrorContains(t, err, "left in place")
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
}

// When git no longer has an entry for the agent's worktree (for example
// after a git worktree prune while the directory was briefly missing), the
// directory is moved aside, its files kept, and a new worktree is added.
func TestProvisionShared_MountedWorktree_MissingAdminEntry(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	work := filepath.Join(WorktreePath(workspace, "agent-1"), "work.txt")
	require.NoError(t, os.WriteFile(work, []byte("in progress\n"), 0o644))
	require.NoError(t, os.RemoveAll(filepath.Join(workspace, ".git", "worktrees", "agent-1")))
	assert.Equal(t, linkedWorktreeNoAdmin, linkedWorktreeState(workspace, WorktreePath(workspace, "agent-1")))

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	assert.Equal(t, linkedWorktreeOK, linkedWorktreeState(workspace, WorktreePath(workspace, "agent-1")))
	assert.Equal(t, "agent-one", currentBranch(t.Context(), WorktreePath(workspace, "agent-1")))
	stale, err := filepath.Glob(filepath.Join(workspace, "worktrees", staleWorktreePrefix+"agent-1-*"))
	require.NoError(t, err)
	require.Len(t, stale, 1)
	assert.FileExists(t, filepath.Join(stale[0], "work.txt"))
	assert.NoFileExists(t, filepath.Join(stale[0], ".git"), "the moved directory is plain files")
	assert.FileExists(t, filepath.Join(stale[0], ".git.moved"))

	// The purge deletes only removed worktrees, not moved-aside ones.
	require.NoError(t, PurgeRemovedWorktrees(workspace))
	assert.DirExists(t, stale[0])
}

// The branch is used exactly as given, slashes included.
func TestProvisionShared_MountedWorktree_BranchUsedAsGiven(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "feature/Login_Fix")))
	assert.Equal(t, "feature/Login_Fix", currentBranch(t.Context(), WorktreePath(workspace, "agent-1")))
	sharers, _, err := ListSharers(workspace, "", "feature/Login_Fix")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-1"}, sharers)

	// An empty branch falls back to the agent's name.
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "")))
	assert.Equal(t, "agent-2", currentBranch(t.Context(), WorktreePath(workspace, "agent-2")))
}

// A branch name git would not accept stops the start before anything is
// added.
func TestProvisionShared_MountedWorktree_InvalidBranch(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	for _, branch := range []string{"-x", "a..b", "a b", "a~1", "feature/", "a.lock", "HEAD", "a//b", "@{-1}"} {
		err := ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", branch))
		require.Error(t, err, "branch %q", branch)
		assert.Contains(t, err.Error(), "is not a valid branch name", "branch %q", branch)
		assert.NoDirExists(t, WorktreePath(workspace, "agent-2"), "branch %q", branch)
	}
}

// checkBranchName reports a name as invalid only when git rejects it. When
// git cannot be run, the error says so and does not call the name invalid.
func TestCheckBranchName(t *testing.T) {
	base := initBareGitRepo(t)
	require.NoError(t, checkBranchName(t.Context(), base, "feature/login"))

	err := checkBranchName(t.Context(), base, "a..b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a valid branch name")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = checkBranchName(ctx, base, "feature/login")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "running git check-ref-format")
	assert.NotContains(t, err.Error(), "is not a valid branch name")
}

// A kept worktree on another branch is not reused for a different
// requested branch, and the requested branch is not registered for it.
func TestProvisionShared_MountedWorktree_KeptWorktreeOnOtherBranch(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))

	// The failed start still chowns the files it rewrote.
	paths := recordLchown(t, nil)
	err := ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "feature/new"))
	require.Error(t, err)
	assert.Contains(t, *paths, filepath.Join(workspace, ".git", "config"))
	for _, want := range []string{`is on branch "agent-one", not on the requested branch "feature/new"`, `Start the agent with branch "agent-one"`, "worktrees/agent-1 of"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.NotContains(t, err.Error(), workspace)
	sharers, _, err := ListSharers(workspace, "", "feature/new")
	require.NoError(t, err)
	assert.Empty(t, sharers)
	assert.Equal(t, "agent-one", currentBranch(t.Context(), WorktreePath(workspace, "agent-1")))
}

// An agent that switched branches in its own worktree restarts with the
// branch it was started with: the worktree is reused as it is, and its
// current branch is not registered as the start branch.
func TestProvisionShared_MountedWorktree_RestartAfterAgentSwitchedBranch(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	wt := WorktreePath(workspace, "agent-1")
	worktreeGit(t, wt, "switch", "-c", "agent-one-part2")

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	assert.Equal(t, "agent-one-part2", currentBranch(t.Context(), wt))
	sharers, _, err := ListSharers(workspace, "", "agent-one-part2")
	require.NoError(t, err)
	assert.Empty(t, sharers)

	// The restart rule reads the worktree's start-branch record, not the
	// sharer registry.
	_, err = UnregisterSharerElsewhere(workspace, "agent-1", "")
	require.NoError(t, err)
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	require.NoError(t, os.Remove(filepath.Join(workspace, ".git", "worktrees", "agent-1", startBranchFile)))
	err = ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `is on branch "agent-one-part2", not on the requested branch "agent-one"`)

	// Another agent asking for the first agent's current branch still gets
	// the branch-in-use error.
	err = ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "agent-one-part2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already checked out in worktrees/agent-1 of")
	assert.Contains(t, err.Error(), "git worktree remove worktrees/agent-1, run in the shared checkout")
	assert.NotContains(t, err.Error(), workspace)
}

// In worktree-per-agent mode every pod takes the file lock, so LockWait
// raises the wait above the default budget; a lower value is ignored.
func TestFileLockWait(t *testing.T) {
	def := defaultFileLockWait()
	assert.Equal(t, def, fileLockWait(ProvisionInput{}))
	assert.Equal(t, def, fileLockWait(ProvisionInput{LockWait: def / 2}))
	assert.Equal(t, def+time.Minute, fileLockWait(ProvisionInput{LockWait: def + time.Minute}))
}

// A pod that starts while another one holds the lock waits up to LockWait
// when that is longer than the default budget.
func TestProvisionShared_MountedWorktree_WaitsUpToLockWait(t *testing.T) {
	withFastFileLockTiming(t)
	origRetries := fileLockRetries
	fileLockRetries = 5 // default budget ~100ms
	t.Cleanup(func() { fileLockRetries = origRetries })
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))

	held, err := acquireFileLock(t.Context(), workspace)
	require.NoError(t, err)
	err = ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "agent-two"))
	require.Error(t, err, "without LockWait the default budget runs out")

	released := make(chan struct{})
	go func() {
		time.Sleep(600 * time.Millisecond)
		_ = held.release()
		close(released)
	}()
	in := mountedWorktreeInput(workspace, origin, "agent-2", "agent-two")
	in.LockWait = 20 * time.Second
	require.NoError(t, ProvisionShared(in))
	<-released
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-2"), workspace))
}

// The worktree-per-agent layout needs git 2.48 or later. The version is
// read from git --version, including vendor suffixes.
func TestGitSupportsRelativeWorktrees(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"2.47.0", false},
		{"2.47.3", false},
		{"2.39.5 (Apple Git-154)", false},
		{"2.47.1.windows.1", false},
		{"1.99.0", false},
		{"2.48.0", true},
		{"2.48.1.windows.1", true},
		{"2.48.1 (Apple Git-155)", true},
		{"2.49.0.rc1", true},
		{"2.100.0", true},
		{"3.0.0", true},
		{"unknown", false},
	}
	dir := t.TempDir()
	for i, tc := range cases {
		fake := filepath.Join(dir, fmt.Sprintf("git%d", i))
		script := fmt.Sprintf("#!/bin/sh\necho 'git version %s'\n", tc.version)
		require.NoError(t, os.WriteFile(fake, []byte(script), 0o755))
		t.Setenv("SCION_GIT_BINARY", fake)
		assert.Equal(t, tc.want, gitSupportsRelativeWorktrees(), "git version %s", tc.version)
	}
	t.Setenv("SCION_GIT_BINARY", filepath.Join(dir, "missing"))
	assert.False(t, gitSupportsRelativeWorktrees(), "no git binary")
}

// An agent that switched branches and is created again (its files kept)
// with the branch it is on now reuses its worktree under that branch only:
// the branch it started on is free for another agent.
func TestProvisionShared_MountedWorktree_SwitchThenRecreate(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	wt := WorktreePath(workspace, "agent-1")
	worktreeGit(t, wt, "switch", "-c", "agent-one-part2")

	paths := recordLchown(t, nil)
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one-part2")))
	assert.Contains(t, *paths, filepath.Join(workspace, ".git", "worktrees", "agent-1", startBranchFile), "the rewritten start-branch record is chowned")
	assert.Equal(t, "agent-one-part2", currentBranch(t.Context(), wt))
	sharers, _, err := ListSharers(workspace, "", "agent-one")
	require.NoError(t, err)
	assert.Empty(t, sharers)
	sharers, _, err = ListSharers(workspace, "", "agent-one-part2")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-1"}, sharers)

	// The old branch is free for another agent.
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "agent-one")))
	assert.Equal(t, "agent-one", currentBranch(t.Context(), WorktreePath(workspace, "agent-2")))

	// The recreated agent now restarts with the branch it was created with.
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one-part2")))
	err = ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `is on branch "agent-one-part2", not on the requested branch "agent-one"`)
}

// Once an agent has switched branches in its worktree, the branch it was
// started on can be taken by another agent, and the first agent still
// restarts with the branch it was started with.
func TestProvisionShared_MountedWorktree_SwitchThenOtherAgentTakesOldBranch(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	wt := WorktreePath(workspace, "agent-1")
	worktreeGit(t, wt, "switch", "-c", "agent-one-part2")

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-2", "agent-one")))
	assert.Equal(t, "agent-one", currentBranch(t.Context(), WorktreePath(workspace, "agent-2")))

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	assert.Equal(t, "agent-one-part2", currentBranch(t.Context(), wt))

	// A third agent asking for agent-one is refused: agent-2 has it.
	err := ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-3", "agent-one"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already checked out in worktrees/agent-2 of")
}

// Removal drops the agent from every branch in the sharer registry.
func TestRemoveMountedWorktree_DropsAgentFromEveryBranch(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	require.NoError(t, RegisterSharer(workspace, "", "old-branch", WorktreePath(workspace, "agent-1"), "agent-1"))
	require.NoError(t, RegisterSharer(workspace, "", "old-branch", WorktreePath(workspace, "agent-2"), "agent-2"))

	require.NoError(t, RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", testLockWait))
	for _, branch := range []string{"agent-one", "old-branch"} {
		sharers, _, err := ListSharers(workspace, "", branch)
		require.NoError(t, err)
		assert.NotContains(t, sharers, "agent-1", "branch %s", branch)
	}
	sharers, _, err := ListSharers(workspace, "", "old-branch")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent-2"}, sharers)
}

// The caller's cancellation does not stop a removal: the steps under the
// lock always finish.
func TestRemoveMountedWorktree_IgnoresCallerCancellation(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, RemoveMountedWorktree(ctx, workspace, "", "agent-1", testLockWait))
	assert.NoDirExists(t, WorktreePath(workspace, "agent-1"))
	assert.NotContains(t, worktreeGit(t, workspace, "worktree", "list"), "agent-1")
}

// When an agent's worktree was removed by hand and the agent is started
// with another branch, the new worktree is registered under the new branch
// only.
func TestProvisionShared_MountedWorktree_NewWorktreeDropsOldRegistration(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	worktreeGit(t, workspace, "worktree", "remove", "worktrees/agent-1")

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "feature/next")))
	assert.Equal(t, "feature/next", currentBranch(t.Context(), WorktreePath(workspace, "agent-1")))
	sharers, _, err := ListSharers(workspace, "", "agent-one")
	require.NoError(t, err)
	assert.Empty(t, sharers)
	branch, _, found, err := FindBranchForAgent(workspace, "", "agent-1")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "feature/next", branch)
}

// The start-branch record is replaced as a whole: a symlink at its name is
// replaced by a regular file, its target is left unchanged, and only a
// regular file is read as a record.
func TestProvisionShared_MountedWorktree_StartBranchRecordReplaced(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	wt := WorktreePath(workspace, "agent-1")
	worktreeGit(t, wt, "switch", "-c", "agent-one-part2")

	record := filepath.Join(workspace, ".git", "worktrees", "agent-1", startBranchFile)
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("agent-one\n"), 0o644))
	require.NoError(t, os.Remove(record))
	require.NoError(t, os.Symlink(target, record))
	assert.Empty(t, startBranch(workspace, wt), "a symlink is not read as a record")

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one-part2")))
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "agent-one\n", string(data), "the symlink's target is unchanged")
	fi, err := os.Lstat(record)
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
	assert.Equal(t, "agent-one-part2", startBranch(workspace, wt))
	leftovers, err := filepath.Glob(record + ".tmp-*")
	require.NoError(t, err)
	assert.Empty(t, leftovers)
}

// Without MountedWorktree no start-branch record is written.
func TestProvisionShared_WorktreeWithoutMountedFlag_NoStartBranchRecord(t *testing.T) {
	origin := initBareGitRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")
	in.MountedWorktree = false
	require.NoError(t, ProvisionShared(in))
	admin, ok := worktreeAdminDir(WorktreePath(workspace, "agent-1"), workspace)
	require.True(t, ok)
	assert.NoFileExists(t, filepath.Join(admin, startBranchFile))
}
