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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countGitClones puts a git wrapper first on PATH that records every
// "git clone" before running the real git, and returns a func reporting how
// many clones ran so far.
func countGitClones(t *testing.T) func() int {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "clones.log")
	// The clone runs with host-git hardening args (-c key=value) before the
	// subcommand, so scan every argument for "clone" rather than only $1.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = clone ]; then echo clone >> '" + logPath + "'; break; fi\n" +
		"done\n" +
		"exec '" + realGit + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, err := os.ReadFile(logPath)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		require.NoError(t, err)
		return strings.Count(string(data), "clone\n")
	}
}

// sharedPlainInput is the input the Kubernetes init container builds for a
// shared-plain git project: the marker and the lock live in the project's
// provisioning state directory beside the workspace (ProjectStateDir).
func sharedPlainInput(hostPath, cloneURL string) ProvisionInput {
	depth := 0
	in := ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-shared-clone",
		Mode:        store.SharingModeSharedPlain,
		SentinelDir: ProjectStateDir(hostPath),
	}
	if cloneURL != "" {
		in.GitClone = &api.GitCloneConfig{URL: cloneURL, Branch: "main", Depth: &depth}
	}
	return in
}

func writeMarker(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ProvisionSentinelFile), []byte("provisioned\n"), 0644))
}

// markWorkspace writes the marker of the workspace at ws where
// sharedPlainInput keeps it: in its provisioning state directory.
func markWorkspace(t *testing.T, ws string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(ProjectStateDir(ws), 0o755))
	writeMarker(t, ProjectStateDir(ws))
}

// newWorkspaceDir returns a new, empty workspace directory with a parent
// of its own, so its provisioning state directory (ProjectStateDir) is not
// shared with another workspace of the same test.
func newWorkspaceDir(t *testing.T) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.Mkdir(ws, 0o755))
	return ws
}

// stateSentinel is the marker path of the workspace at ws.
func stateSentinel(ws string) string {
	return filepath.Join(ProjectStateDir(ws), ProvisionSentinelFile)
}

func assertNoCloneScratch(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), cloneTempDirPrefix), "leftover clone scratch dir %s", e.Name())
	}
}

func TestProvisionShared_SharedPlain_ClonesOnceThenReuses(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := newWorkspaceDir(t)

	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, stateSentinel(hostPath))
	assertNoCloneScratch(t, hostPath)
	assert.Equal(t, 1, clones())

	// Nothing of the provisioning shows in git's view.
	out, err := exec.Command("git", "-C", hostPath, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.NotContains(t, string(out), ProvisionSentinelFile)

	// A later agent reuses the workspace.
	require.NoError(t, os.WriteFile(filepath.Join(hostPath, "agent-work.txt"), []byte("work"), 0644))
	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.Equal(t, 1, clones(), "a provisioned workspace must not be cloned again")
	assert.FileExists(t, filepath.Join(hostPath, "agent-work.txt"))
}

func TestProvisionShared_SharedPlain_ConcurrentStartsCloneOnce(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := newWorkspaceDir(t)

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = ProvisionShared(sharedPlainInput(hostPath, bareRepo))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "start %d", i)
	}
	assert.Equal(t, 1, clones())
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, stateSentinel(hostPath))
	assertNoCloneScratch(t, hostPath)
}

func TestProvisionShared_SharedPlain_FailedCloneWritesNoMarker(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := newWorkspaceDir(t)
	missing := filepath.Join(t.TempDir(), "missing.git")
	AllowLocalCloneFixture(t, missing)

	err := ProvisionShared(sharedPlainInput(hostPath, missing))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git clone "+missing)
	assert.NoFileExists(t, stateSentinel(hostPath))
	assert.NoDirExists(t, filepath.Join(hostPath, ".git"))
	assertNoCloneScratch(t, hostPath)

	// The next start clones.
	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, stateSentinel(hostPath))
}

func TestProvisionShared_SharedPlain_CloneErrorHidesCredentials(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	hostPath := newWorkspaceDir(t)
	// Nothing listens on port 1, so git fails without any network access.
	rawURL := "https://user:s3cr3t-token@127.0.0.1:1/org/repo.git?access_token=qtoken"

	err := ProvisionShared(sharedPlainInput(hostPath, rawURL))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://127.0.0.1:1/org/repo.git")
	assert.NotContains(t, err.Error(), "s3cr3t-token")
	assert.NotContains(t, err.Error(), "qtoken")
	assert.NoFileExists(t, stateSentinel(hostPath))
}

func TestCloneError_NamesMissingCredentials(t *testing.T) {
	rawURL := "https://tok123@github.com/org/private.git"
	output := "Cloning into '/workspace/.scion-clone-1'...\n" +
		"fatal: could not read Username for 'https://tok123@github.com': terminal prompts disabled\n"

	err := cloneError(rawURL, output, errors.New("exit status 128"), false, "")
	msg := err.Error()
	assert.Contains(t, msg, "git clone https://github.com/org/private.git")
	assert.Contains(t, msg, "needs credentials")
	assert.Contains(t, msg, "without a git token")
	assert.NotContains(t, msg, "tok123")

	notFound := cloneError("https://github.com/org/gone.git", "remote: Repository not found.\n", nil, false, "")
	assert.Contains(t, notFound.Error(), "not found")
	assert.Contains(t, notFound.Error(), "no credentials")

	other := cloneError("https://github.com/org/repo.git", "fatal: something else\n", nil, false, "")
	assert.Equal(t, "git clone https://github.com/org/repo.git: fatal: something else", other.Error())
}

// C1: a workspace provisioned earlier keeps its content. A marker with any
// entry next to it (here a file, and no .git) is never cloned into or
// cleared.
func TestProvisionShared_SharedPlain_MarkedWorkspaceWithContentUntouched(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := newWorkspaceDir(t)
	markWorkspace(t, hostPath)
	require.NoError(t, os.WriteFile(filepath.Join(hostPath, "notes.txt"), []byte("user data"), 0644))

	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.Equal(t, 0, clones())
	assert.NoDirExists(t, filepath.Join(hostPath, ".git"))
	assert.NoFileExists(t, filepath.Join(hostPath, "README.md"))
	data, err := os.ReadFile(filepath.Join(hostPath, "notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, "user data", string(data))
}

// A workspace provisioned empty before the init container received the
// clone settings (marker only) is cloned, and the marker is kept.
func TestProvisionShared_SharedPlain_MarkedEmptyWorkspaceIsCloned(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := newWorkspaceDir(t)
	markWorkspace(t, hostPath)
	// A clone scratch directory left by an earlier start does not count as
	// content.
	require.NoError(t, os.MkdirAll(filepath.Join(hostPath, cloneTempDirPrefix+"old", "partial"), 0755))
	// An in-workspace shared-dir mount point does not count as content.
	require.NoError(t, os.MkdirAll(filepath.Join(hostPath, ".scion-volumes", "scratchpad"), 0755))

	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.Equal(t, 1, clones())
	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, stateSentinel(hostPath))
	assert.DirExists(t, filepath.Join(hostPath, ".scion-volumes", "scratchpad"))
	assertNoCloneScratch(t, hostPath)

	// The next start finds content and leaves it alone.
	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.Equal(t, 1, clones())
}

func TestProvisionShared_SharedPlain_MarkedEmptyConcurrentStartsCloneOnce(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := newWorkspaceDir(t)
	markWorkspace(t, hostPath)

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = ProvisionShared(sharedPlainInput(hostPath, bareRepo))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "start %d", i)
	}
	assert.Equal(t, 1, clones())
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
}

// Only shared-plain git workspaces are cloned on the marked-but-empty path.
func TestMarkedWorkspaceNeedsClone(t *testing.T) {
	empty := t.TempDir()
	markWorkspace(t, empty)
	withFile := t.TempDir()
	markWorkspace(t, withFile)
	require.NoError(t, os.WriteFile(filepath.Join(withFile, "a"), nil, 0644))
	// In-workspace shared-dir mount points (empty) are not content.
	withMountPoint := t.TempDir()
	markWorkspace(t, withMountPoint)
	require.NoError(t, os.MkdirAll(filepath.Join(withMountPoint, ".scion-volumes", "x"), 0755))
	// A file in a .scion-volumes child that is not a shared-dir mount is.
	withVolumeFile := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withVolumeFile, ".scion-volumes", "x"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(withVolumeFile, ".scion-volumes", "x", "f"), nil, 0644))
	// ... unless it is the mount of a shared dir.
	sharedMount := sharedPlainInput(withVolumeFile, "file:///repo.git")
	sharedMount.Resolved.SharedDirs = map[string]ResolvedSharedDir{"x": {HostPath: filepath.Join(withVolumeFile, ".scion-volumes", "x")}}
	// A file directly in .scion-volumes is content.
	volumesRootFile := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(volumesRootFile, ".scion-volumes"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(volumesRootFile, ".scion-volumes", "f"), nil, 0644))
	// An empty worktrees directory is not content; a non-empty one is.
	emptyWorktrees := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(emptyWorktrees, "worktrees"), 0755))
	fullWorktrees := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(fullWorktrees, "worktrees", "a"), 0755))
	withDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withDir, "src"), 0755))
	// A file (not a directory) under the scratch prefix is not an artifact.
	scratchFile := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(scratchFile, cloneTempDirPrefix+"x"), nil, 0644))

	worktree := sharedPlainInput(empty, "file:///repo.git")
	worktree.Mode = store.SharingModeWorktreePerAgent

	cases := []struct {
		name string
		in   ProvisionInput
		want bool
	}{
		{"shared-plain marker only", sharedPlainInput(empty, "file:///repo.git"), true},
		{"no clone settings", sharedPlainInput(empty, ""), false},
		{"worktree-per-agent", worktree, false},
		{"user file", sharedPlainInput(withFile, "file:///repo.git"), false},
		{"other directory", sharedPlainInput(withDir, "file:///repo.git"), false},
		{"empty shared-dir mount point", sharedPlainInput(withMountPoint, "file:///repo.git"), true},
		{"file in a .scion-volumes child", sharedPlainInput(withVolumeFile, "file:///repo.git"), false},
		{"shared-dir mount with content", sharedMount, true},
		{"file directly in .scion-volumes", sharedPlainInput(volumesRootFile, "file:///repo.git"), false},
		{"empty worktrees dir", sharedPlainInput(emptyWorktrees, "file:///repo.git"), true},
		{"non-empty worktrees dir", sharedPlainInput(fullWorktrees, "file:///repo.git"), false},
		{"file named like scratch dir", sharedPlainInput(scratchFile, "file:///repo.git"), false},
		{"missing workspace", sharedPlainInput(filepath.Join(empty, "nope"), "file:///repo.git"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, markedWorkspaceNeedsClone(tc.in))
		})
	}
}

// C2: with no marker and no .git, a workspace holding anything the clone did
// not create is refused, not cleared.
func TestProvisionShared_SharedPlain_UnmarkedNonEmptyWorkspaceRefused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := newWorkspaceDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(hostPath, "keep.txt"), []byte("keep"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(hostPath, "src"), 0755))

	err := ProvisionShared(sharedPlainInput(hostPath, bareRepo))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not empty")
	assert.Contains(t, err.Error(), "keep.txt, src")
	assert.Contains(t, err.Error(), "refusing to clone into it or clear it")
	assert.Equal(t, 0, clones())
	assert.FileExists(t, filepath.Join(hostPath, "keep.txt"))
	assert.DirExists(t, filepath.Join(hostPath, "src"))
	assert.NoDirExists(t, filepath.Join(hostPath, ".git"))
	assert.NoFileExists(t, stateSentinel(hostPath))
}

// Entries Scion itself places in a fresh workspace do not block the clone:
// a leftover scratch dir from an interrupted attempt is removed, and the
// in-workspace shared-dir mount points are kept.
func TestGitCloneWorkspace_RemovesOwnScratchKeepsSharedDirMounts(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := newWorkspaceDir(t)
	leftover := filepath.Join(hostPath, cloneTempDirPrefix+"123")
	require.NoError(t, os.MkdirAll(filepath.Join(leftover, ".git"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(leftover, "README.md"), []byte("partial"), 0644))
	mountPoint := filepath.Join(hostPath, ".scion-volumes", "shared")
	require.NoError(t, os.MkdirAll(mountPoint, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(mountPoint, "data"), []byte("shared"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(hostPath, "worktrees"), 0755))

	in := sharedPlainInput(hostPath, bareRepo)
	in.Resolved.SharedDirs = map[string]ResolvedSharedDir{"shared": {HostPath: mountPoint}}
	require.NoError(t, gitCloneWorkspace(context.Background(), in, func() bool { return true }))
	assert.NoDirExists(t, leftover)
	assert.FileExists(t, filepath.Join(mountPoint, "data"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assertNoCloneScratch(t, hostPath)
}

// A clone that stopped after moving some files but before .git (always moved
// last) leaves files without a marker or .git: the next start refuses
// instead of clearing them.
func TestProvisionShared_SharedPlain_InterruptedMoveRefusedNotCleared(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := newWorkspaceDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(hostPath, "README.md"), []byte("# Test\n"), 0644))

	err := ProvisionShared(sharedPlainInput(hostPath, bareRepo))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "README.md")
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.NoFileExists(t, stateSentinel(hostPath))
}

// sharedPlainStateDirInput is the input a current sciontool builds in the
// Kubernetes init container: the marker and the lock live in the mounted
// provision state directory, and nothing in the workspace.
func sharedPlainStateDirInput(t *testing.T, cloneURL string) (ProvisionInput, string) {
	t.Helper()
	projectDir := t.TempDir()
	workspace := filepath.Join(projectDir, "workspace")
	stateDir := filepath.Join(projectDir, ProvisionStateDirName)
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.MkdirAll(stateDir, 0o755))
	in := sharedPlainInput(workspace, cloneURL)
	in.SentinelDir = stateDir
	return in, stateDir
}

func TestProvisionShared_SharedPlain_StateDir_ConcurrentStartsCloneOnce(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	in, stateDir := sharedPlainStateDirInput(t, bareRepo)

	const n = 3
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = ProvisionShared(in)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "start %d", i)
	}
	ws := in.Resolved.HostPath
	assert.Equal(t, 1, clones())
	assert.FileExists(t, filepath.Join(ws, "README.md"))
	assert.FileExists(t, filepath.Join(stateDir, ProvisionSentinelFile))
	assert.NoFileExists(t, filepath.Join(ws, ProvisionSentinelFile), "no sentinel in the workspace root (AC-L3)")
	assertNoCloneScratch(t, ws)
	out, err := exec.Command("git", "-C", ws, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)), "provisioning files must stay out of git status")
}

func TestProvisionShared_SharedPlain_StateDir_MarkedEmptyIsCloned(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)

	// Marker in the state directory.
	in, stateDir := sharedPlainStateDirInput(t, bareRepo)
	writeMarker(t, stateDir)
	require.NoError(t, ProvisionShared(in))
	assert.Equal(t, 1, clones())
	assert.FileExists(t, filepath.Join(in.Resolved.HostPath, "README.md"))

	// A marker in the workspace root only (written by an image from before
	// the state directory) is not read: the workspace is unprovisioned, the
	// marker is workspace content, so the clone refuses and leaves it.
	legacy, legacyState := sharedPlainStateDirInput(t, bareRepo)
	writeMarker(t, legacy.Resolved.HostPath)
	err := ProvisionShared(legacy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ProvisionSentinelFile)
	assert.Equal(t, 1, clones())
	assert.FileExists(t, filepath.Join(legacy.Resolved.HostPath, ProvisionSentinelFile))
	assert.NoFileExists(t, filepath.Join(legacyState, ProvisionSentinelFile))
}
