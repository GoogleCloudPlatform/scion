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

// Worktree removal and provisioning in the shared checkout remove only the
// agent's own worktree entry, never every entry whose directory is missing:
// another worktree that this process cannot see keeps its registration
// (ptone/scion#5071).

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addHiddenSiblingWorktree adds a worktree on branch "sibling" in a
// directory outside the shared checkout at base, then moves its parent away
// so its path is missing from this process's view. It returns the sibling
// path and a function that puts it back (also run at cleanup).
func addHiddenSiblingWorktree(t *testing.T, base string) (string, func()) {
	t.Helper()
	other := t.TempDir()
	volume := filepath.Join(other, "volume")
	sibling := filepath.Join(volume, "sibling")
	require.NoError(t, os.MkdirAll(volume, 0o755))
	worktreeGit(t, base, "worktree", "add", "-b", "sibling", sibling)
	hidden := filepath.Join(other, "volume-hidden")
	require.NoError(t, os.Rename(volume, hidden))
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		require.NoError(t, os.Rename(hidden, volume))
	}
	t.Cleanup(restore)
	return sibling, restore
}

// requireSiblingIntact puts the sibling back and checks that git still
// knows it and that git commands still work inside it.
func requireSiblingIntact(t *testing.T, base, sibling string, restore func()) {
	t.Helper()
	restore()
	assert.Contains(t, worktreeGit(t, base, "worktree", "list", "--porcelain"), "branch refs/heads/sibling",
		"sibling worktree entry was removed")
	out, err := exec.Command("git", "-C", sibling, "status", "--short").CombinedOutput()
	assert.NoError(t, err, "git status in the sibling worktree: %s", out)
}

// staleOwnEntry provisions a mounted worktree for agent-1 and moves its
// directory aside, so git keeps an entry at the agent's own path whose
// directory is missing. It returns the workspace, the origin and the
// agent's worktree path.
func staleOwnEntry(t *testing.T) (workspace, origin, worktreePath string) {
	t.Helper()
	origin = initBareGitRepo(t)
	workspace = filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))
	worktreePath = WorktreePath(workspace, "agent-1")
	_, err := moveWorktreeAside(workspace, "agent-1", removedWorktreePrefix)
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "setup: git keeps the entry")
	return workspace, origin, worktreePath
}

func TestRemoveMountedWorktree_KeepsHiddenSiblingEntry(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	sibling, restore := addHiddenSiblingWorktree(t, workspace)

	require.NoError(t, RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", testLockWait))

	assert.NoDirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "the removed worktree's entry should be gone")
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-2"), workspace))
	requireSiblingIntact(t, workspace, sibling, restore)
}

// A worktree entry whose directory is missing is not returned, and is not
// pruned; the entry holding the branch is reported as the holder.
func TestFindWorktreeForBranch_SkipsMissingPathKeepsEntry(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	sibling, restore := addHiddenSiblingWorktree(t, workspace)

	path, holder, err := findWorktreeForBranch(t.Context(), workspace, "sibling")
	require.NoError(t, err)
	assert.Empty(t, path)
	assert.Equal(t, sibling, holder)

	path, holder, err = findWorktreeForBranch(t.Context(), workspace, "agent-two")
	require.NoError(t, err)
	assert.Equal(t, WorktreePath(workspace, "agent-2"), path)
	assert.Empty(t, holder)
	requireSiblingIntact(t, workspace, sibling, restore)
}

// An agent whose own worktree directory went missing (its entry left
// behind) gets its worktree again; only its own entry is removed.
func TestProvisionShared_OwnStaleEntry_KeepsHiddenSibling(t *testing.T) {
	workspace, origin, worktreePath := staleOwnEntry(t)
	sibling, restore := addHiddenSiblingWorktree(t, workspace)

	require.NoError(t, ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")))

	assert.True(t, IsRealWorktreeDir(worktreePath, workspace))
	assert.Equal(t, linkedWorktreeOK, linkedWorktreeState(workspace, worktreePath))
	requireSiblingIntact(t, workspace, sibling, restore)
}

// A branch held by another worktree at a path that is not the agent's own
// is not taken over, even when that worktree's directory is missing from
// this process's view: provisioning fails with an error that names the
// holder and how to clear it, and the holder keeps its registration.
func TestProvisionShared_BranchHeldByHiddenWorktree_ClearErrorAndKeepsEntry(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	origin := worktreeGit(t, workspace, "remote", "get-url", "origin")
	sibling, restore := addHiddenSiblingWorktree(t, workspace)

	err := ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-3", "sibling"))
	require.Error(t, err, "expected ProvisionShared to fail for a branch held by another worktree")
	assert.Contains(t, err.Error(), "git worktree remove "+sibling, "error should name the holder and how to clear it")
	assert.Contains(t, err.Error(), "Start the agent with a different branch", "mounted mode uses the branch-in-another-worktree advice")
	requireSiblingIntact(t, workspace, sibling, restore)
}

// An empty directory at the agent's own path (made for the pod mount) that
// git still has an entry for: the directory is removed, the entry removed,
// and the directory created again with the same mode; then provisioning
// adds the worktree into it.
func TestClearOwnStaleWorktreeEntry_EmptyDirRegistered(t *testing.T) {
	workspace, origin, worktreePath := staleOwnEntry(t)
	// As after a removal: the agent is no longer in the sharer registry.
	_, err := UnregisterSharerElsewhere(workspace, "agent-1", "")
	require.NoError(t, err)
	// The broker makes the leaf setgid (2775); the mode must survive.
	require.NoError(t, os.Mkdir(worktreePath, 0o775))
	require.NoError(t, os.Chmod(worktreePath, fs.ModeSetgid|0o775))
	sibling, restore := addHiddenSiblingWorktree(t, workspace)
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")

	require.NoError(t, clearOwnStaleWorktreeEntry(t.Context(), in, workspace, worktreePath))

	assert.NoDirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "the own stale entry should be removed")
	fi, err := os.Stat(worktreePath)
	require.NoError(t, err, "the empty directory should be created again")
	assert.Equal(t, fs.ModeSetgid|0o775, fi.Mode()&(fs.ModePerm|fs.ModeSetgid|fs.ModeSticky))
	entries, err := os.ReadDir(worktreePath)
	require.NoError(t, err)
	assert.Empty(t, entries)

	require.NoError(t, ProvisionShared(in))
	assert.True(t, IsRealWorktreeDir(worktreePath, workspace))
	requireSiblingIntact(t, workspace, sibling, restore)
}

// A non-empty directory at the agent's own path is never touched, even when
// git has an entry for it.
func TestClearOwnStaleWorktreeEntry_NonEmptyDirLeftAlone(t *testing.T) {
	workspace, origin, worktreePath := staleOwnEntry(t)
	require.NoError(t, os.Mkdir(worktreePath, 0o755))
	kept := filepath.Join(worktreePath, "keep.txt")
	require.NoError(t, os.WriteFile(kept, []byte("x\n"), 0o644))
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")

	require.NoError(t, clearOwnStaleWorktreeEntry(t.Context(), in, workspace, worktreePath))

	assert.FileExists(t, kept)
	assert.DirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "the entry of a non-empty directory should be kept")
}

// When the own entry cannot be removed (here: it is locked), the empty
// directory is created again and the error names the path and how to clear
// the entry; no other entry is touched.
func TestClearOwnStaleWorktreeEntry_RemovalFails_ClearError(t *testing.T) {
	workspace, origin, worktreePath := staleOwnEntry(t)
	require.NoError(t, os.Mkdir(worktreePath, 0o755))
	worktreeGit(t, workspace, "worktree", "lock", worktreePath)
	sibling, restore := addHiddenSiblingWorktree(t, workspace)
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")

	err := clearOwnStaleWorktreeEntry(t.Context(), in, workspace, worktreePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remove the empty directory")
	assert.Contains(t, err.Error(), "git worktree unlock worktrees/agent-1")
	assert.Contains(t, err.Error(), "git worktree remove worktrees/agent-1")
	_, advice, _ := strings.Cut(err.Error(), "Make sure")
	assert.NotContains(t, advice, worktreePath, "the advice does not show the init container's path")
	assert.DirExists(t, worktreePath, "the empty directory should be created again")
	assert.DirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "the locked entry is kept")
	requireSiblingIntact(t, workspace, sibling, restore)
}

// The removal of the agent's own entry runs through SafeGitCommand, so it
// is refused like every other git call in the shared checkout when the
// checkout's .git has a commondir file.
func TestClearOwnStaleWorktreeEntry_UsesSafeGitCommand(t *testing.T) {
	workspace, origin, worktreePath := staleOwnEntry(t)
	commondir := filepath.Join(workspace, ".git", "commondir")
	require.NoError(t, os.WriteFile(commondir, []byte(".\n"), 0o644))
	t.Cleanup(func() { _ = os.Remove(commondir) })
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")

	err := clearOwnStaleWorktreeEntry(t.Context(), in, workspace, worktreePath)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCommondirPresent)
	require.NoError(t, os.Remove(commondir))
	assert.DirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "the entry is kept when the command is refused")
}

// When the empty directory cannot be removed (here: its parent is not
// writable; a mount point gives EBUSY), nothing else is touched and the
// error says to remove the directory first.
func TestClearOwnStaleWorktreeEntry_RmdirFails_ClearError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	workspace, origin, worktreePath := staleOwnEntry(t)
	require.NoError(t, os.Mkdir(worktreePath, 0o755))
	worktreesDir := filepath.Dir(worktreePath)
	require.NoError(t, os.Chmod(worktreesDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(worktreesDir, 0o755) })
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")

	err := clearOwnStaleWorktreeEntry(t.Context(), in, workspace, worktreePath)
	require.NoError(t, os.Chmod(worktreesDir, 0o755))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remove the empty directory")
	assert.DirExists(t, worktreePath)
	assert.DirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "the entry is kept")
}

// When the entry is removed but the directory cannot be created again, the
// error says so (not that the entry was kept).
func TestClearOwnStaleWorktreeEntry_RecreateFails_ClearError(t *testing.T) {
	workspace, origin, worktreePath := staleOwnEntry(t)
	require.NoError(t, os.Mkdir(worktreePath, 0o755))
	sibling, restore := addHiddenSiblingWorktree(t, workspace)
	orig := recreateEmptyDir
	recreateEmptyDir = func(string, os.FileMode, int, int) error { return errors.New("recreate refused") }
	t.Cleanup(func() { recreateEmptyDir = orig })
	in := mountedWorktreeInput(workspace, origin, "agent-1", "agent-one")

	err := clearOwnStaleWorktreeEntry(t.Context(), in, workspace, worktreePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not create its empty directory again with its owner and mode")
	assert.Contains(t, err.Error(), "recreate refused")
	assert.NoDirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-1"), "the own entry was removed")
	requireSiblingIntact(t, workspace, sibling, restore)
}

// An empty directory at the agent's own path that git has no entry for is
// left alone.
func TestClearOwnStaleWorktreeEntry_EmptyDirNotRegisteredLeftAlone(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	origin := worktreeGit(t, workspace, "remote", "get-url", "origin")
	worktreePath := WorktreePath(workspace, "agent-3")
	require.NoError(t, os.Mkdir(worktreePath, 0o755))
	// Held open, the directory shows a link count of 0 once it is removed,
	// even if a new one gets the same inode number.
	dir, err := os.Open(worktreePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dir.Close() })
	in := mountedWorktreeInput(workspace, origin, "agent-3", "agent-three")

	require.NoError(t, clearOwnStaleWorktreeEntry(t.Context(), in, workspace, worktreePath))

	fi, err := dir.Stat()
	require.NoError(t, err)
	assert.NotZero(t, fi.Sys().(*syscall.Stat_t).Nlink, "the directory should not be replaced")
}

// Outside MountedWorktree the error shows the path as is and does not
// describe an NFS export.
func TestStaleEntryError_LocalMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace", "worktrees", "agent-1")
	in := ProvisionInput{Resolved: ResolvedWorkspace{HostPath: filepath.Dir(filepath.Dir(path))}, AgentID: "agent-1"}
	for _, state := range []staleEntryState{staleEntryKept, staleEntryDirKept, staleEntryDirNotRecreated} {
		msg := staleEntryError(in, path, state, errors.New("x")).Error()
		assert.Contains(t, msg, path)
		assert.NotContains(t, msg, "NFS")
	}
}

func TestParseWorktreeList(t *testing.T) {
	out := "worktree /repo\nHEAD 1111\nbranch refs/heads/main\n\n" +
		"worktree \"/w/with\\ttab and \\303\\251\"\nHEAD 2222\nbranch refs/heads/b1\n\n" +
		"worktree \"/w/bad\\q\"\nHEAD 3333\ndetached\n\n" +
		"worktree /w/plain path\nHEAD 4444\nbranch refs/heads/b2\nprunable gitdir file points to non-existent location\n"
	got := parseWorktreeList(out)
	want := []worktreeEntry{
		{path: "/repo", branch: "main"},
		{path: "/w/with\ttab and \u00e9", branch: "b1"},
		{path: "\"/w/bad\\q\"", branch: ""},
		{path: "/w/plain path", branch: "b2"},
	}
	assert.Equal(t, want, got)
}

// A branch held by a worktree entry inside the shared checkout whose
// directory is missing: in mounted mode the error names it relative to the
// shared checkout, never by the init container's path, and the entry is
// kept.
func TestProvisionShared_BranchHeldInsideCheckout_RelativePathInError(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	origin := worktreeGit(t, workspace, "remote", "get-url", "origin")
	_, err := moveWorktreeAside(workspace, "agent-2", removedWorktreePrefix)
	require.NoError(t, err)

	err = ProvisionShared(mountedWorktreeInput(workspace, origin, "agent-3", "agent-two"))
	require.Error(t, err, "expected ProvisionShared to fail for a branch held by another worktree")
	assert.Contains(t, err.Error(), "git worktree remove worktrees/agent-2", "the holder is named relative to the shared checkout")
	assert.NotContains(t, err.Error(), WorktreePath(workspace, "agent-2"), "the init container's path is not shown")
	assert.DirExists(t, filepath.Join(workspace, ".git", "worktrees", "agent-2"), "the holder's entry is kept")
}
