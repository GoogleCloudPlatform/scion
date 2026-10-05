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
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initBareRepoWithTree returns a bare repository whose main branch has
// README.md and src/a.txt.
func initBareRepoWithTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "tree.git")
	run(t, "git", "init", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(dir, "work")
	run(t, "git", "clone", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("# Tree\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(work, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "src", "a.txt"), []byte("a\n"), 0o644))
	runIn(t, work, "git", "add", "-A")
	runIn(t, work, "git", "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-m", "tree")
	runIn(t, work, "git", "push", "origin", "main")
	return bare
}

// interruptedClone leaves ws as a clone that was stopped while its entries
// were being moved into place: a finished clone with its manifest in a
// scratch directory, with moveNames already moved into ws.
func interruptedClone(t *testing.T, ws, bare string, moveNames ...string) string {
	t.Helper()
	scratch := filepath.Join(ws, cloneTempDirPrefix+"stopped")
	run(t, "git", "clone", bare, scratch)
	require.NoError(t, writeCloneManifest(scratch))
	for _, n := range moveNames {
		require.NoError(t, os.Rename(filepath.Join(scratch, n), filepath.Join(ws, n)))
	}
	return scratch
}

func assertCleanCheckout(t *testing.T, ws string) {
	t.Helper()
	assert.DirExists(t, filepath.Join(ws, ".git"))
	assert.FileExists(t, filepath.Join(ws, "README.md"))
	assert.FileExists(t, filepath.Join(ws, "src", "a.txt"))
	assertNoCloneScratch(t, ws)
	out, err := exec.Command("git", "-C", ws, "status", "--porcelain").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Empty(t, strings.TrimSpace(string(out)))
}

// A marked workspace whose clone was stopped mid-move is finished on the
// next start, not treated as provisioned.
func TestProvisionShared_SharedPlain_MarkedInterruptedMoveIsFinished(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bare := initBareRepoWithTree(t)
	ws := t.TempDir()
	writeMarker(t, ws)
	interruptedClone(t, ws, bare, "README.md")
	clones := countGitClones(t)

	require.NoError(t, ProvisionShared(sharedPlainInput(ws, bare)))
	assert.Equal(t, 0, clones(), "the finished clone is moved, not cloned again")
	assertCleanCheckout(t, ws)
}

// The same on an unmarked workspace: the move is finished and the marker
// written, instead of every later start being refused.
func TestProvisionShared_SharedPlain_UnmarkedInterruptedMoveIsFinished(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bare := initBareRepoWithTree(t)
	ws := t.TempDir()
	interruptedClone(t, ws, bare, "README.md", "src")
	// Stopped before anything was moved.
	ws2 := t.TempDir()
	interruptedClone(t, ws2, bare)
	clones := countGitClones(t)

	require.NoError(t, ProvisionShared(sharedPlainInput(ws, bare)))
	assert.Equal(t, 0, clones())
	assertCleanCheckout(t, ws)
	assert.FileExists(t, filepath.Join(ws, ProvisionSentinelFile))

	require.NoError(t, ProvisionShared(sharedPlainInput(ws2, bare)))
	assert.Equal(t, 0, clones())
	assertCleanCheckout(t, ws2)
}

// An entry that did not come from the interrupted clone stops the resume;
// nothing is moved or removed.
func TestProvisionShared_SharedPlain_InterruptedMoveWithForeignEntryRefused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bare := initBareRepoWithTree(t)
	for _, marked := range []bool{true, false} {
		ws := t.TempDir()
		if marked {
			writeMarker(t, ws)
		}
		scratch := interruptedClone(t, ws, bare, "README.md")
		require.NoError(t, os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("agent"), 0o644))

		err := ProvisionShared(sharedPlainInput(ws, bare))
		require.Error(t, err, "marked=%v", marked)
		assert.Contains(t, err.Error(), "interrupted clone")
		assert.Contains(t, err.Error(), "notes.txt")
		assert.NoDirExists(t, filepath.Join(ws, ".git"))
		assert.DirExists(t, filepath.Join(scratch, ".git"), "the clone is kept for a later resume")
		data, rerr := os.ReadFile(filepath.Join(ws, "notes.txt"))
		require.NoError(t, rerr)
		assert.Equal(t, "agent", string(data))
		if !marked {
			assert.NoFileExists(t, filepath.Join(ws, ProvisionSentinelFile))
		}
	}
}

// A clone entry missing from both places cannot be finished.
func TestProvisionShared_SharedPlain_InterruptedMoveMissingEntryRefused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bare := initBareRepoWithTree(t)
	ws := t.TempDir()
	scratch := interruptedClone(t, ws, bare, "README.md")
	require.NoError(t, os.RemoveAll(filepath.Join(scratch, "src")))

	err := ProvisionShared(sharedPlainInput(ws, bare))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing src")
	assert.FileExists(t, filepath.Join(ws, "README.md"))
	assert.NoDirExists(t, filepath.Join(ws, ".git"))
}

// gitWritesIntoWorkspace puts a git wrapper first on PATH that writes name
// (with content) into ws when a clone starts, standing in for an agent that
// is already running in the workspace.
func gitWritesIntoWorkspace(t *testing.T, ws, name, content string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	target := filepath.Join(ws, name)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = clone ]; then printf '%s' '" + content + "' > '" + target + "'; fi\n" +
		"exec '" + realGit + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A file written into the workspace while the clone runs is kept, and the
// clone is discarded, on both the marked and the unmarked path.
func TestProvisionShared_SharedPlain_WorkspaceWrittenDuringCloneKept(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	bare := initBareRepoWithTree(t)
	for _, marked := range []bool{true, false} {
		ws := t.TempDir()
		if marked {
			writeMarker(t, ws)
		}
		gitWritesIntoWorkspace(t, ws, "README.md", "agent edit")

		err := ProvisionShared(sharedPlainInput(ws, bare))
		require.Error(t, err, "marked=%v", marked)
		assert.Contains(t, err.Error(), "changed during the clone")
		data, rerr := os.ReadFile(filepath.Join(ws, "README.md"))
		require.NoError(t, rerr)
		assert.Equal(t, "agent edit", string(data))
		assert.NoDirExists(t, filepath.Join(ws, ".git"))
		assert.NoDirExists(t, filepath.Join(ws, "src"))
		assertNoCloneScratch(t, ws)
		if !marked {
			assert.NoFileExists(t, filepath.Join(ws, ProvisionSentinelFile))
		}
	}
}

// The move never replaces an existing entry; on failure the entries it
// moved go back to src.
func TestMoveDirContentsUp_NeverReplaces(t *testing.T) {
	dest := t.TempDir()
	src := filepath.Join(dest, cloneTempDirPrefix+"x")
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("clone"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "b.txt"), []byte("clone"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "b.txt"), []byte("mine"), 0o644))

	err := moveDirContentsUp(src, dest)
	require.Error(t, err)
	assert.True(t, errors.Is(err, fs.ErrExist), "got %v", err)
	data, rerr := os.ReadFile(filepath.Join(dest, "b.txt"))
	require.NoError(t, rerr)
	assert.Equal(t, "mine", string(data))
	assert.NoFileExists(t, filepath.Join(dest, "a.txt"), "moved entries go back to src")
	assert.NoDirExists(t, filepath.Join(dest, ".git"))
	for _, n := range []string{".git", "a.txt", "b.txt"} {
		_, statErr := os.Lstat(filepath.Join(src, n))
		assert.NoError(t, statErr, "src keeps %s", n)
	}
}

// .git is always renamed last, and a failure before it leaves no .git in
// the workspace.
func TestMoveDirContentsUp_MovesGitLast(t *testing.T) {
	newSrc := func(dest string) string {
		src := filepath.Join(dest, cloneTempDirPrefix+"x")
		require.NoError(t, os.MkdirAll(filepath.Join(src, ".git"), 0o755))
		for _, n := range []string{"a.txt", "z.txt", cloneManifestFile} {
			require.NoError(t, os.WriteFile(filepath.Join(src, n), nil, 0o644))
		}
		return src
	}

	orig := moveRenameFile
	t.Cleanup(func() { moveRenameFile = orig })

	dest := t.TempDir()
	src := newSrc(dest)
	var order []string
	moveRenameFile = func(oldpath, newpath string) error {
		order = append(order, filepath.Base(oldpath))
		return orig(oldpath, newpath)
	}
	require.NoError(t, moveDirContentsUp(src, dest))
	require.NotEmpty(t, order)
	assert.Equal(t, ".git", order[len(order)-1])
	assert.NotContains(t, order, cloneManifestFile, "the manifest stays in the scratch dir")

	dest2 := t.TempDir()
	src2 := newSrc(dest2)
	moveRenameFile = func(oldpath, newpath string) error {
		if filepath.Base(oldpath) == "z.txt" {
			return errors.New("injected")
		}
		return orig(oldpath, newpath)
	}
	require.Error(t, moveDirContentsUp(src2, dest2))
	assert.NoDirExists(t, filepath.Join(dest2, ".git"))
	assert.DirExists(t, filepath.Join(src2, ".git"))
}

func TestRenameNoReplace(t *testing.T) {
	for name, rename := range map[string]func(string, string) error{
		"renameNoReplace": renameNoReplace,
		"fallback":        renameNoReplaceFallback,
	} {
		dir := t.TempDir()
		a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
		require.NoError(t, os.WriteFile(a, []byte("a"), 0o644))
		require.NoError(t, os.WriteFile(b, []byte("b"), 0o644))
		err := rename(a, b)
		assert.True(t, errors.Is(err, fs.ErrExist), "%s: got %v", name, err)
		data, _ := os.ReadFile(b)
		assert.Equal(t, "b", string(data), name)

		c := filepath.Join(dir, "c")
		require.NoError(t, rename(a, c), name)
		assert.FileExists(t, c, name)
		assert.NoFileExists(t, a, name)
	}
}

func TestRedactCloneURL_Shapes(t *testing.T) {
	cases := map[string]string{
		"git@github.com:org/repo.git":                  "github.com:org/repo.git",
		"github.com:org/repo.git":                      "github.com:org/repo.git",
		"/srv/git/repo.git":                            "/srv/git/repo.git",
		"ssh://git@github.com/org/repo.git":            "ssh://github.com/org/repo.git",
		"https://tok@github.com/org/repo.git?x=1#frag": "https://github.com/org/repo.git",
	}
	for in, want := range cases {
		assert.Equal(t, want, redactCloneURL(in), in)
	}
	err := cloneError("git@github.com:org/private.git", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n", nil)
	assert.Contains(t, err.Error(), "git clone github.com:org/private.git")
}
