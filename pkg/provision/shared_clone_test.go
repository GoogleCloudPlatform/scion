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
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = clone ]; then echo clone >> '" + logPath + "'; fi\n" +
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
// shared-plain git project: only the workspace is mounted, so the marker
// and the lock live in the workspace itself.
func sharedPlainInput(hostPath, cloneURL string) ProvisionInput {
	depth := 0
	in := ProvisionInput{
		Resolved:    ResolvedWorkspace{HostPath: hostPath, Backend: "nfs"},
		ProjectID:   "proj-shared-clone",
		Mode:        store.SharingModeSharedPlain,
		SentinelDir: hostPath,
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
	hostPath := t.TempDir()

	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
	assertNoCloneScratch(t, hostPath)
	assert.Equal(t, 1, clones())

	// The marker is kept out of git's view.
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
	hostPath := t.TempDir()

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
	assert.FileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
	assertNoCloneScratch(t, hostPath)
}

func TestProvisionShared_SharedPlain_FailedCloneWritesNoMarker(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing.git")

	err := ProvisionShared(sharedPlainInput(hostPath, missing))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git clone "+missing)
	assert.NoFileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
	assert.NoDirExists(t, filepath.Join(hostPath, ".git"))
	assertNoCloneScratch(t, hostPath)

	// The next start clones.
	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
}

func TestProvisionShared_SharedPlain_CloneErrorHidesCredentials(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	hostPath := t.TempDir()
	// Nothing listens on port 1, so git fails without any network access.
	rawURL := "https://user:s3cr3t-token@127.0.0.1:1/org/repo.git?access_token=qtoken"

	err := ProvisionShared(sharedPlainInput(hostPath, rawURL))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://127.0.0.1:1/org/repo.git")
	assert.NotContains(t, err.Error(), "s3cr3t-token")
	assert.NotContains(t, err.Error(), "qtoken")
	assert.NoFileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
}

func TestCloneError_NamesMissingCredentials(t *testing.T) {
	rawURL := "https://tok123@github.com/org/private.git"
	output := "Cloning into '/workspace/.scion-clone-1'...\n" +
		"fatal: could not read Username for 'https://tok123@github.com': terminal prompts disabled\n"

	err := cloneError(rawURL, output, errors.New("exit status 128"))
	msg := err.Error()
	assert.Contains(t, msg, "git clone https://github.com/org/private.git")
	assert.Contains(t, msg, "needs credentials")
	assert.Contains(t, msg, "without a git token")
	assert.NotContains(t, msg, "tok123")

	notFound := cloneError("https://github.com/org/gone.git", "remote: Repository not found.\n", nil)
	assert.Contains(t, notFound.Error(), "not found")
	assert.Contains(t, notFound.Error(), "no credentials")

	other := cloneError("https://github.com/org/repo.git", "fatal: something else\n", nil)
	assert.Equal(t, "git clone https://github.com/org/repo.git: fatal: something else", other.Error())
}

// C1: a workspace provisioned earlier keeps its content. A marker with any
// entry next to it (here a file, and no .git) is never cloned into or
// cleared.
func TestProvisionShared_SharedPlain_MarkedWorkspaceWithContentUntouched(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := t.TempDir()
	writeMarker(t, hostPath)
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
	hostPath := t.TempDir()
	writeMarker(t, hostPath)
	// Provisioning artifacts left by earlier starts do not count as content.
	require.NoError(t, os.MkdirAll(filepath.Join(hostPath, provisionFileLockName+".evict-0123"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(hostPath, cloneTempDirPrefix+"old", "partial"), 0755))

	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.Equal(t, 1, clones())
	assert.DirExists(t, filepath.Join(hostPath, ".git"))
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.FileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
	assertNoCloneScratch(t, hostPath)

	// The next start finds content and leaves it alone.
	require.NoError(t, ProvisionShared(sharedPlainInput(hostPath, bareRepo)))
	assert.Equal(t, 1, clones())
}

func TestProvisionShared_SharedPlain_MarkedEmptyConcurrentStartsCloneOnce(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	clones := countGitClones(t)
	hostPath := t.TempDir()
	writeMarker(t, hostPath)

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
	writeMarker(t, empty)
	withFile := t.TempDir()
	writeMarker(t, withFile)
	require.NoError(t, os.WriteFile(filepath.Join(withFile, "a"), nil, 0644))
	withDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withDir, ".scion-volumes", "x"), 0755))
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
	hostPath := t.TempDir()
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
	assert.NoFileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
}

// Entries Scion itself places in a fresh workspace do not block the clone:
// a leftover scratch dir from an interrupted attempt is removed, and the
// in-workspace shared-dir mount points are kept.
func TestGitCloneWorkspace_RemovesOwnScratchKeepsSharedDirMounts(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bareRepo := initBareGitRepo(t)
	hostPath := t.TempDir()
	leftover := filepath.Join(hostPath, cloneTempDirPrefix+"123")
	require.NoError(t, os.MkdirAll(filepath.Join(leftover, ".git"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(leftover, "README.md"), []byte("partial"), 0644))
	mountPoint := filepath.Join(hostPath, ".scion-volumes", "shared")
	require.NoError(t, os.MkdirAll(mountPoint, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(mountPoint, "data"), []byte("shared"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(hostPath, "worktrees"), 0755))

	in := sharedPlainInput(hostPath, bareRepo)
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
	hostPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostPath, "README.md"), []byte("# Test\n"), 0644))

	err := ProvisionShared(sharedPlainInput(hostPath, bareRepo))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "README.md")
	assert.FileExists(t, filepath.Join(hostPath, "README.md"))
	assert.NoFileExists(t, filepath.Join(hostPath, ProvisionSentinelFile))
}
