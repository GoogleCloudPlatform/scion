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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stateDirLayout returns a project directory with the workspace and the
// provisioning state directory side by side, as the Kubernetes init
// container sees them through its two subPath mounts.
func stateDirLayout(t *testing.T) (workspace, stateDir string) {
	t.Helper()
	projectDir := t.TempDir()
	workspace = filepath.Join(projectDir, "workspace")
	stateDir = ProjectStateDir(workspace)
	require.NoError(t, os.MkdirAll(workspace, 0o770))
	require.NoError(t, os.MkdirAll(stateDir, 0o770))
	return workspace, stateDir
}

// stateDirInput is the init container's ProvisionInput when the pod mounts
// the provisioning state directory.
func stateDirInput(workspace, stateDir, origin string, mode store.WorkspaceSharingMode) ProvisionInput {
	return ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: workspace, Backend: "nfs"},
		ProjectID:   "proj-state-dir",
		Mode:        mode,
		SentinelDir: stateDir,
		NFSUID:      os.Getuid(),
		NFSGID:      os.Getgid(),
		GitClone:    &api.GitCloneConfig{URL: origin, Branch: "main"},
	}
}

// assertWorkspaceClean checks that the workspace root holds nothing of the
// provisioning bookkeeping (AC-L1, AC-L3): no sentinel, no lock and no lock
// litter, nothing but the clone, and that git status is clean.
func assertWorkspaceClean(t *testing.T, workspace string) {
	t.Helper()
	assert.NoFileExists(t, filepath.Join(workspace, ProvisionSentinelFile))
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName))
	entries, err := os.ReadDir(workspace)
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || name == "README.md" || name == "worktrees" {
			continue
		}
		t.Errorf("unexpected entry in the workspace root: %s", name)
	}
	out, err := exec.Command("git", "-C", workspace, "status", "--porcelain", "--ignored=no").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)), "git status of the workspace")
}

// The Kubernetes init container with the state directory mounted (#2670):
// concurrent provisioners clone once, the sentinel is written in the state
// directory only, and the workspace root stays clean.
func TestProvisionShared_StateDir_ConcurrentSameProject_SentinelOutsideWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	workspace, stateDir := stateDirLayout(t)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = ProvisionShared(stateDirInput(workspace, stateDir, origin, store.SharingModeSharedPlain))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}

	assert.FileExists(t, filepath.Join(workspace, "README.md"))
	assert.FileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
	assert.NoDirExists(t, filepath.Join(stateDir, provisionFileLockName))
	assertWorkspaceClean(t, workspace)
}

// Worktree-per-agent takes the lock on every start: the worktree is added,
// and the workspace root still holds no sentinel and shows nothing in git
// status.
func TestProvisionShared_StateDir_WorktreePerAgent(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	workspace, stateDir := stateDirLayout(t)

	for _, agent := range []string{"agent-1", "agent-2"} {
		in := stateDirInput(workspace, stateDir, origin, store.SharingModeWorktreePerAgent)
		in.AgentID = agent
		in.AgentName = agent
		in.MountedWorktree = true
		in.RequireChownSuccess = true
		in.GitClone.Branch = ""
		require.NoError(t, ProvisionShared(in))
		assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, agent), workspace), agent)
	}
	assert.FileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
	assertWorkspaceClean(t, workspace)
}

// AC-L4 (flipped): a provisioner of an image from before the state
// directory takes only the workspace-root lock and checks only the
// workspace-root sentinel. Against a workspace current code provisioned it
// finds no sentinel, so it would provision again: the documented upgrade
// behaviour (such images must be rebuilt).
func TestProvisionShared_StateDir_PreStateDirProvisionerSeesUnprovisioned(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	workspace, stateDir := stateDirLayout(t)
	require.NoError(t, ProvisionShared(stateDirInput(workspace, stateDir, origin, store.SharingModeSharedPlain)))
	require.FileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))

	// The simulated old provisioner: its lock in the workspace root, its
	// sentinel check there too.
	old, err := acquireFileLock(t.Context(), workspace)
	require.NoError(t, err, "nothing of the current provisioning holds the workspace-root lock")
	oldSeesProvisioned := SentinelPresent(workspace)
	require.NoError(t, old.release())
	assert.False(t, oldSeesProvisioned, "a pre-state-dir provisioner must see the workspace as unprovisioned")
}

// A sentinel directory that is the workspace, or inside it, is refused
// before anything is created or locked: the provisioning bookkeeping is
// never kept in the workspace root (AC-L8).
func TestProvisionShared_SentinelDirInWorkspaceRefused(t *testing.T) {
	origin := initBareGitRepo(t)
	for _, sub := range []string{"", "provision"} {
		workspace := filepath.Join(t.TempDir(), "workspace")
		in := stateDirInput(workspace, filepath.Join(workspace, sub), origin, store.SharingModeSharedPlain)
		err := ProvisionShared(in)
		require.ErrorContains(t, err, "outside the workspace")
		assert.NoDirExists(t, workspace, "nothing is created")
	}
}

// The first lock is taken first: while another holder has it, the ordered
// acquisition (WithWorkspaceProvisionLocks: the project root, then the
// state directory) does not take (or even create) the second lock; once it
// is released, both are taken.
func TestAcquireOrderedFileLocks_FirstFirst(t *testing.T) {
	workspace, stateDir := stateDirLayout(t)
	projectRoot := filepath.Dir(workspace)
	other, err := acquireFileLock(t.Context(), projectRoot)
	require.NoError(t, err)

	type result struct {
		held heldLock
		err  error
	}
	done := make(chan result, 1)
	go func() {
		h, err := acquireOrderedFileLocks(context.Background(), []string{projectRoot, stateDir}, 30*time.Second)
		done <- result{h, err}
	}()

	time.Sleep(1500 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("acquired while the first lock was held (err=%v)", r.err)
	default:
	}
	assert.NoDirExists(t, filepath.Join(stateDir, provisionFileLockName), "the second lock must not be taken before the first")

	require.NoError(t, other.release())
	r := <-done
	require.NoError(t, r.err)
	assert.DirExists(t, filepath.Join(projectRoot, provisionFileLockName))
	assert.DirExists(t, filepath.Join(stateDir, provisionFileLockName))
	assert.True(t, r.held.stillOwned())
	require.NoError(t, r.held.release())
	assert.NoDirExists(t, filepath.Join(projectRoot, provisionFileLockName))
	assert.NoDirExists(t, filepath.Join(stateDir, provisionFileLockName))
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName))
}

// When the second lock cannot be taken, the first one is released again.
func TestAcquireOrderedFileLocks_ReleasesFirstOnFailure(t *testing.T) {
	workspace, stateDir := stateDirLayout(t)
	projectRoot := filepath.Dir(workspace)
	other, err := acquireFileLock(t.Context(), stateDir)
	require.NoError(t, err)
	defer func() { _ = other.release() }()

	_, err = acquireOrderedFileLocks(t.Context(), []string{projectRoot, stateDir}, 1500*time.Millisecond)
	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(projectRoot, provisionFileLockName), "the first lock is released")
}

// A removal takes the state-directory lock, and only that one (AC-L1):
// while another holder has it, the removal gives up and leaves the
// worktree, and nothing is created in the workspace root.
func TestRemoveMountedWorktree_StateDirLock(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	stateDir := ProjectStateDir(workspace)
	held, err := acquireFileLock(t.Context(), stateDir)
	require.NoError(t, err)

	err = RemoveMountedWorktree(t.Context(), workspace, stateDir, "agent-1", 1500*time.Millisecond)
	require.Error(t, err)
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName))

	require.NoError(t, held.release())
	require.NoError(t, RemoveMountedWorktree(t.Context(), workspace, stateDir, "agent-1", testLockWait))
	assert.NoDirExists(t, WorktreePath(workspace, "agent-1"))
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName))
}

// An empty stateDir means ProjectStateDir(base): a holder of that lock
// blocks the removal. A stateDir in the workspace is refused.
func TestRemoveMountedWorktree_DefaultStateDirAndRefusal(t *testing.T) {
	workspace := provisionTwoMountedWorktrees(t)
	held, err := acquireFileLock(t.Context(), ProjectStateDir(workspace))
	require.NoError(t, err)
	err = RemoveMountedWorktree(t.Context(), workspace, "", "agent-1", 1500*time.Millisecond)
	require.Error(t, err)
	assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	require.NoError(t, held.release())

	for _, dir := range []string{workspace, filepath.Join(workspace, "provision")} {
		err := RemoveMountedWorktree(t.Context(), workspace, dir, "agent-1", testLockWait)
		require.ErrorContains(t, err, "outside the workspace")
		assert.True(t, IsRealWorktreeDir(WorktreePath(workspace, "agent-1"), workspace))
	}
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName))
}

// The recursive chown skips the lock artifacts in every lock directory it is
// given, but still chowns same-named entries anywhere else.
func TestChownProjectTreeExcluding_SkipsLockArtifactsInEachDir(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "provision")
	nested := filepath.Join(root, "sub")
	for _, dir := range []string{
		filepath.Join(root, provisionFileLockName),
		filepath.Join(stateDir, provisionFileLockName+".stage-x"),
		filepath.Join(nested, provisionFileLockName),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	paths := recordLchown(t, func(string) bool { return false })

	require.NoError(t, chownProjectTreeExcluding(t.Context(), root, []string{root, stateDir, ""}, os.Getuid(), os.Getgid()))
	assert.NotContains(t, *paths, filepath.Join(root, provisionFileLockName))
	assert.NotContains(t, *paths, filepath.Join(stateDir, provisionFileLockName+".stage-x"))
	assert.Contains(t, *paths, filepath.Join(nested, provisionFileLockName))
	assert.Contains(t, *paths, stateDir)
}

func TestSentinelPresent(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	assert.False(t, SentinelPresent(a, "", b))
	require.NoError(t, writeSentinel(filepath.Join(b, ProvisionSentinelFile)))
	assert.True(t, SentinelPresent(a, "", b))
	assert.False(t, SentinelPresent(a))
}

// A state directory that cannot be searched (EACCES) counts as "no
// sentinel".
func TestSentinelPresent_UnreadableDirIsAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	stateDir := t.TempDir()
	require.NoError(t, writeSentinel(filepath.Join(stateDir, ProvisionSentinelFile)))
	require.NoError(t, os.Chmod(stateDir, 0))
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	assert.False(t, SentinelPresent(stateDir))
}

func TestProjectStateDir(t *testing.T) {
	assert.Equal(t, "/mnt/share/projects/p1/provision", ProjectStateDir("/mnt/share/projects/p1/workspace"))
	assert.Equal(t, filepath.Join("projects", "p1", "provision"), ProjectStateDir(filepath.Join("projects", "p1", "workspace")))
}

// PrepareStateDir fails closed on anything but a real directory, and with
// fixOwnership changes only the directory itself.
func TestPrepareStateDir(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "provision")
		require.NoError(t, os.Mkdir(dir, 0o755))
		child := filepath.Join(dir, "keep")
		require.NoError(t, os.WriteFile(child, nil, 0o600))

		require.NoError(t, PrepareStateDir(dir, os.Getuid(), os.Getgid(), false))
		fi, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), "unchanged without fixOwnership")

		require.NoError(t, PrepareStateDir(dir, os.Getuid(), os.Getgid(), true))
		var st syscall.Stat_t
		require.NoError(t, syscall.Stat(dir, &st))
		assert.Equal(t, uint32(stateDirMode), st.Mode&0o7777)
		fi, err = os.Stat(child)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "no recursion")
	})
	t.Run("symlink", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "target")
		require.NoError(t, os.Mkdir(target, 0o755))
		link := filepath.Join(base, "provision")
		require.NoError(t, os.Symlink(target, link))
		for _, fix := range []bool{false, true} {
			err := PrepareStateDir(link, os.Getuid(), os.Getgid(), fix)
			require.ErrorContains(t, err, "not a directory")
		}
		fi, err := os.Stat(target)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), "the symlink target is untouched")
	})
	t.Run("regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "provision")
		require.NoError(t, os.WriteFile(file, nil, 0o644))
		for _, fix := range []bool{false, true} {
			require.ErrorContains(t, PrepareStateDir(file, os.Getuid(), os.Getgid(), fix), "not a directory")
		}
	})
	t.Run("missing", func(t *testing.T) {
		require.Error(t, PrepareStateDir(filepath.Join(t.TempDir(), "provision"), os.Getuid(), os.Getgid(), true))
	})
	t.Run("bad path", func(t *testing.T) {
		for _, p := range []string{"", "relative/provision", "/", "/scion-provision/", "/a/../scion-provision"} {
			require.Error(t, PrepareStateDir(p, os.Getuid(), os.Getgid(), true), p)
		}
	})
	t.Run("chown refused, other owner", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "provision")
		require.NoError(t, os.Mkdir(dir, 0o755))
		if os.Geteuid() == 0 {
			t.Skip("root may chown")
		}
		require.Error(t, PrepareStateDir(dir, os.Getuid()+1, os.Getgid(), true))
	})
}

// An older pod can still write a sentinel into the workspace root after the
// state directory has its own. Current code neither reads it nor adds a git
// exclude for it (AC-L8); the hub's one-shot promotion removes it.
func TestProvisionShared_StateDir_WorkspaceRootSentinelNotExcluded(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	workspace, stateDir := stateDirLayout(t)
	require.NoError(t, ProvisionShared(stateDirInput(workspace, stateDir, origin, store.SharingModeSharedPlain)))
	require.FileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
	require.NoError(t, writeSentinel(filepath.Join(workspace, ProvisionSentinelFile)))

	require.NoError(t, ProvisionShared(stateDirInput(workspace, stateDir, origin, store.SharingModeSharedPlain)))
	exclude, err := os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.NotContains(t, strings.Split(string(exclude), "\n"), "/"+ProvisionSentinelFile)
	assert.FileExists(t, filepath.Join(workspace, ProvisionSentinelFile), "left for the promotion step")
}

// SentinelDirs lists what ProvisionShared checks: the sentinel directory
// alone (AC-L7).
func TestProvisionInput_SentinelDirs(t *testing.T) {
	ws := "/p/workspace"
	assert.Equal(t, []string{"/p"}, ProvisionInput{Resolved: ResolvedWorkspace{HostPath: ws}}.SentinelDirs())
	assert.Equal(t, []string{"/s"}, ProvisionInput{Resolved: ResolvedWorkspace{HostPath: ws}, SentinelDir: "/s"}.SentinelDirs())
}

// N5: the combined lock depends on the first lock as well: when it is
// lost, the combined ctx is cancelled and stillOwned reports false, even
// though the second lock is still held.
func TestAcquireOrderedFileLocks_LosingFirstLockCancels(t *testing.T) {
	origHeartbeat := provisionLockHeartbeatInterval
	provisionLockHeartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { provisionLockHeartbeatInterval = origHeartbeat })

	workspace, stateDir := stateDirLayout(t)
	projectRoot := filepath.Dir(workspace)
	held, err := acquireOrderedFileLocks(context.Background(), []string{projectRoot, stateDir}, 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.release() })
	require.True(t, held.stillOwned())

	firstLock := filepath.Join(projectRoot, provisionFileLockName)
	require.NoError(t, os.RemoveAll(firstLock)) // force-evict the first lock
	_, err = tryCreateFileLock(firstLock)       // a successor acquires it
	require.NoError(t, err)

	select {
	case <-held.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("losing the first lock did not cancel the combined ctx")
	}
	assert.False(t, held.stillOwned())
	assert.DirExists(t, filepath.Join(stateDir, provisionFileLockName), "the second lock is still held")
	require.NoError(t, held.release())
	assert.NoDirExists(t, filepath.Join(stateDir, provisionFileLockName))
	assert.DirExists(t, firstLock, "the successor's lock is untouched")
}
