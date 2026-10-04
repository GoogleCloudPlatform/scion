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
		LegacyDir:   workspace,
		NFSUID:      os.Getuid(),
		NFSGID:      os.Getgid(),
		GitClone:    &api.GitCloneConfig{URL: origin, Branch: "main"},
	}
}

// assertWorkspaceClean checks that the workspace root holds no sentinel and
// no live lock, that nothing but the clone and transient lock litter (which
// is git-excluded) is in it, and that git status is clean.
func assertWorkspaceClean(t *testing.T, workspace string) {
	t.Helper()
	assert.NoFileExists(t, filepath.Join(workspace, ProvisionSentinelFile))
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName))
	entries, err := os.ReadDir(workspace)
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || name == "README.md" || name == "worktrees" || strings.HasPrefix(name, provisionFileLockName+".") {
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

// A workspace provisioned before the state directory existed has its
// sentinel in the workspace root. It still counts as provisioned (nothing is
// cloned; the clone URL here does not exist), the new sentinel is not
// written, and the legacy sentinel is added to the git excludes.
func TestProvisionShared_StateDir_LegacySentinelHonoured(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	workspace, stateDir := stateDirLayout(t)
	legacy := ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: workspace, Backend: "nfs"},
		ProjectID:   "proj-state-dir",
		Mode:        store.SharingModeSharedPlain,
		SentinelDir: workspace,
		GitClone:    &api.GitCloneConfig{URL: origin, Branch: "main"},
	}
	require.NoError(t, ProvisionShared(legacy))
	require.FileExists(t, filepath.Join(workspace, ProvisionSentinelFile))

	for _, mode := range []store.WorkspaceSharingMode{store.SharingModeSharedPlain, store.SharingModeWorktreePerAgent} {
		t.Run(string(mode), func(t *testing.T) {
			in := stateDirInput(workspace, stateDir, filepath.Join(t.TempDir(), "missing.git"), mode)
			if mode == store.SharingModeWorktreePerAgent {
				in.AgentID = "agent-1"
				in.AgentName = "agent-1"
				in.MountedWorktree = true
			}
			require.NoError(t, ProvisionShared(in))
			assert.NoFileExists(t, filepath.Join(stateDir, ProvisionSentinelFile), "no re-provisioning")

			exclude, err := os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
			require.NoError(t, err)
			assert.Contains(t, strings.Split(string(exclude), "\n"), "/"+ProvisionSentinelFile)
			out, err := exec.Command("git", "-C", workspace, "status", "--porcelain", "--ignored=no").CombinedOutput()
			require.NoError(t, err, string(out))
			assert.NotContains(t, string(out), ProvisionSentinelFile)
		})
	}
}

// An init container without the state-directory mount (an older runtime)
// keeps the sentinel in the workspace, and a new clone excludes it from git.
func TestProvisionShared_SentinelInWorkspace_ExcludedFromGit(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	origin := initBareGitRepo(t)
	workspace := t.TempDir()
	require.NoError(t, ProvisionShared(ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: workspace, Backend: "nfs"},
		ProjectID:   "proj-legacy-layout",
		Mode:        store.SharingModeSharedPlain,
		SentinelDir: workspace,
		GitClone:    &api.GitCloneConfig{URL: origin, Branch: "main"},
	}))
	exclude, err := os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Contains(t, strings.Split(string(exclude), "\n"), "/"+ProvisionSentinelFile)
	out, err := exec.Command("git", "-C", workspace, "status", "--porcelain", "--ignored=no").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)))
}

// The legacy lock is taken first: while an older provisioner holds it, the
// ordered acquisition does not take (or even create) the state-directory
// lock; once it is released, both are taken.
func TestAcquireOrderedFileLocks_LegacyFirst(t *testing.T) {
	workspace, stateDir := stateDirLayout(t)
	old, err := acquireFileLock(t.Context(), workspace)
	require.NoError(t, err)

	type result struct {
		held heldLock
		err  error
	}
	done := make(chan result, 1)
	go func() {
		h, err := acquireOrderedFileLocks(context.Background(), []string{workspace, stateDir}, 30*time.Second)
		done <- result{h, err}
	}()

	time.Sleep(1500 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("acquired while the legacy lock was held (err=%v)", r.err)
	default:
	}
	assert.NoDirExists(t, filepath.Join(stateDir, provisionFileLockName), "the state lock must not be taken before the legacy lock")

	require.NoError(t, old.release())
	r := <-done
	require.NoError(t, r.err)
	assert.DirExists(t, filepath.Join(workspace, provisionFileLockName))
	assert.DirExists(t, filepath.Join(stateDir, provisionFileLockName))
	assert.True(t, r.held.stillOwned())
	require.NoError(t, r.held.release())
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName))
	assert.NoDirExists(t, filepath.Join(stateDir, provisionFileLockName))
}

// When the second lock cannot be taken, the first one is released again.
func TestAcquireOrderedFileLocks_ReleasesFirstOnFailure(t *testing.T) {
	workspace, stateDir := stateDirLayout(t)
	other, err := acquireFileLock(t.Context(), stateDir)
	require.NoError(t, err)
	defer func() { _ = other.release() }()

	_, err = acquireOrderedFileLocks(t.Context(), []string{workspace, stateDir}, 1500*time.Millisecond)
	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(workspace, provisionFileLockName), "the legacy lock is released")
}

// A removal with the state directory takes its lock too: while another
// holder has the state-directory lock, the removal gives up and leaves the
// worktree, and releases the legacy lock it took first.
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
