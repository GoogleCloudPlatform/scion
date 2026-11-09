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

//go:build unix

package provision

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appendGitExclude never reads an over-cap exclude file: the pattern is
// appended without the duplicate check, after a newline because the file
// does not end with one, so the exclude is not lost.
func TestAppendGitExclude_OverCapAppendsWithoutRead(t *testing.T) {
	ws := t.TempDir()
	info := filepath.Join(ws, ".git", "info")
	require.NoError(t, os.MkdirAll(info, 0o755))
	exclude := filepath.Join(info, "exclude")
	require.NoError(t, os.WriteFile(exclude, []byte("worktrees/\n"), 0o644))
	const size = 64 << 20
	require.NoError(t, os.Truncate(exclude, size)) // sparse; the last byte is NUL

	require.NoError(t, appendGitExclude(ws, "/x"))
	fi, err := os.Stat(exclude)
	require.NoError(t, err)
	assert.Equal(t, int64(size+len("\n/x\n")), fi.Size())
	f, err := os.Open(exclude)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	tail := make([]byte, 4)
	_, err = f.ReadAt(tail, size)
	require.NoError(t, err)
	assert.Equal(t, "\n/x\n", string(tail))
}

// paddedGitdir is a valid "gitdir: <target>" line followed by enough
// whitespace to exceed the pointer cap; TrimSpace would accept it.
func paddedGitdir(target string) []byte {
	return []byte("gitdir: " + target + "\n" + strings.Repeat(" ", gitPointerMaxBytes))
}

// newLinkedWorktree makes <base>/.git/worktrees/<name> with a gitdir file
// pointing back to <base>/worktrees/<name>/.git, and that worktree's .git
// file; it returns the base and the worktree path.
func newLinkedWorktree(t *testing.T, name string) (base, wt string) {
	t.Helper()
	base = filepath.Join(t.TempDir(), "workspace")
	admin := filepath.Join(base, ".git", "worktrees", name)
	wt = filepath.Join(base, "worktrees", name)
	require.NoError(t, os.MkdirAll(admin, 0o755))
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(wt, ".git")+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+admin+"\n"), 0o644))
	return base, wt
}

// The fixture is a real linked worktree for these checks.
func TestBoundedGitPointers_Fixture(t *testing.T) {
	base, wt := newLinkedWorktree(t, "agent-1")
	assert.True(t, IsRealWorktreeDir(wt, base))
	_, ok := worktreeAdminDir(wt, base)
	assert.True(t, ok)
	assert.Equal(t, linkedWorktreeOK, linkedWorktreeState(base, wt))
}

// An over-cap worktree .git file is not read, even when its content (after
// TrimSpace) is a valid pointer.
func TestIsRealWorktreeDir_OverCapGitFileRefused(t *testing.T) {
	base, wt := newLinkedWorktree(t, "agent-1")
	admin := filepath.Join(base, ".git", "worktrees", "agent-1")
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), paddedGitdir(admin), 0o644))
	assert.False(t, IsRealWorktreeDir(wt, base))
	_, ok := worktreeAdminDir(wt, base)
	assert.False(t, ok)
}

// An over-cap admin gitdir file is not read: the entry counts as a mismatch.
func TestLinkedWorktreeState_OverCapGitdirRefused(t *testing.T) {
	base, wt := newLinkedWorktree(t, "agent-1")
	gitdir := filepath.Join(base, ".git", "worktrees", "agent-1", "gitdir")
	require.NoError(t, os.WriteFile(gitdir, paddedGitdir(filepath.Join(wt, ".git"))[len("gitdir: "):], 0o644))
	assert.Equal(t, linkedWorktreeMismatch, linkedWorktreeState(base, wt))
}

// A symlinked worktree .git file is not followed by worktreeAdminDir (it
// was before; IsRealWorktreeDir already refused it), nor a symlinked gitdir.
func TestBoundedGitPointers_SymlinksNotFollowed(t *testing.T) {
	base, wt := newLinkedWorktree(t, "agent-1")
	admin := filepath.Join(base, ".git", "worktrees", "agent-1")
	elsewhere := filepath.Join(t.TempDir(), "dotgit")
	require.NoError(t, os.WriteFile(elsewhere, []byte("gitdir: "+admin+"\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(wt, ".git")))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(wt, ".git")))
	_, ok := worktreeAdminDir(wt, base)
	assert.False(t, ok, "a symlinked .git is not followed")
	assert.False(t, IsRealWorktreeDir(wt, base))

	base2, wt2 := newLinkedWorktree(t, "agent-2")
	gitdir := filepath.Join(base2, ".git", "worktrees", "agent-2", "gitdir")
	other := filepath.Join(t.TempDir(), "gitdir")
	require.NoError(t, os.WriteFile(other, []byte(filepath.Join(wt2, ".git")+"\n"), 0o644))
	require.NoError(t, os.Remove(gitdir))
	require.NoError(t, os.Symlink(other, gitdir))
	assert.Equal(t, linkedWorktreeMismatch, linkedWorktreeState(base2, wt2), "a symlinked gitdir is not followed")
}

// readSmallFileNoFollow: a FIFO never blocks and is refused; a device is
// refused; a file at the cap is read.
func TestReadSmallFileNoFollow(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o644))
	done := make(chan error, 1)
	go func() { _, err := readSmallFileNoFollow(fifo, gitPointerMaxBytes); done <- err }()
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("readSmallFileNoFollow blocked on a FIFO")
	}
	_, err := readSmallFileNoFollow("/dev/zero", gitPointerMaxBytes)
	assert.Error(t, err)

	ok := filepath.Join(dir, "ok")
	require.NoError(t, os.WriteFile(ok, make([]byte, gitPointerMaxBytes), 0o644))
	data, err := readSmallFileNoFollow(ok, gitPointerMaxBytes)
	require.NoError(t, err)
	assert.Len(t, data, gitPointerMaxBytes)
	require.NoError(t, os.WriteFile(ok, make([]byte, gitPointerMaxBytes+1), 0o644))
	_, err = readSmallFileNoFollow(ok, gitPointerMaxBytes)
	assert.ErrorIs(t, err, errFileTooLarge)
}

// The start-branch record is read bounded and without following a symlink:
// an over-cap record (a valid branch padded with whitespace) reads as none,
// and a missing record still reads as none.
func TestStartBranch_BoundedRead(t *testing.T) {
	base, wt := newLinkedWorktree(t, "agent-1")
	assert.Empty(t, startBranch(base, wt), "a missing record reads as none")
	record := filepath.Join(base, ".git", "worktrees", "agent-1", startBranchFile)
	require.NoError(t, os.WriteFile(record, []byte("agent-one\n"), 0o644))
	assert.Equal(t, "agent-one", startBranch(base, wt))
	require.NoError(t, os.WriteFile(record, []byte("agent-one\n"+strings.Repeat(" ", gitPointerMaxBytes)), 0o644))
	assert.Empty(t, startBranch(base, wt), "an over-cap record is not read")
}

// ValidateWorktreeForBase reads the candidate's .git and the admin back-link
// bounded and without following a symlink.
func TestValidateWorktreeForBase_BoundedReads(t *testing.T) {
	base, wt := newLinkedWorktree(t, "agent-1")
	require.NoError(t, ValidateWorktreeForBase(base, wt), "fixture")
	admin := filepath.Join(base, ".git", "worktrees", "agent-1")

	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), paddedGitdir(admin), 0o644))
	assert.Error(t, ValidateWorktreeForBase(base, wt), "an over-cap .git is not read")

	base2, wt2 := newLinkedWorktree(t, "agent-2")
	gitdir := filepath.Join(base2, ".git", "worktrees", "agent-2", "gitdir")
	require.NoError(t, os.WriteFile(gitdir, paddedGitdir(filepath.Join(wt2, ".git"))[len("gitdir: "):], 0o644))
	assert.Error(t, ValidateWorktreeForBase(base2, wt2), "an over-cap back-link is not read")
}

// readAgentBranch: an over-cap record (a valid branch padded with
// whitespace) or a symlinked one reads as none.
func TestReadAgentBranch_BoundedRead(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, AgentBranchFile)
	assert.Empty(t, readAgentBranch(rec))
	require.NoError(t, os.WriteFile(rec, []byte("scion/agent-1\n"), 0o644))
	assert.Equal(t, "scion/agent-1", readAgentBranch(rec))
	require.NoError(t, os.WriteFile(rec, []byte("scion/agent-1\n"+strings.Repeat(" ", gitPointerMaxBytes)), 0o644))
	assert.Empty(t, readAgentBranch(rec), "an over-cap record is not read")
	target := filepath.Join(t.TempDir(), "branch")
	require.NoError(t, os.WriteFile(target, []byte("scion/agent-1\n"), 0o644))
	require.NoError(t, os.Remove(rec))
	require.NoError(t, os.Symlink(target, rec))
	assert.Empty(t, readAgentBranch(rec), "a symlinked record is not followed")
}

// A sharer marker above its cap is refused (an error, not a silently
// dropped registration), even when its content is valid JSON.
func TestReadMarker_OverCapRefused(t *testing.T) {
	base := t.TempDir()
	p := sharerPath(base, "agent-one")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("{}"+strings.Repeat(" ", sharerMarkerMaxBytes)), 0o644))
	_, err := readMarker(base, "", p)
	assert.ErrorIs(t, err, errFileTooLarge)
	m, err := readMarker(base, "", filepath.Join(filepath.Dir(p), "absent.json"))
	assert.NoError(t, err, "a missing marker is still no marker")
	assert.Nil(t, m)
}

// The clone manifest in the workspace's scratch directory is read bounded.
func TestReadCloneManifest_OverCapRefused(t *testing.T) {
	scratch := t.TempDir()
	body := cloneManifestHeader + "\nurl=x\nbranch=main\n\nREADME.md\n"
	require.NoError(t, os.WriteFile(filepath.Join(scratch, cloneManifestFile), []byte(body+strings.Repeat("#", cloneManifestMaxBytes)), 0o644))
	_, err := readCloneManifest(scratch)
	assert.ErrorIs(t, err, errFileTooLarge)
}

// fillSharerRegistry creates n marker files (empty JSON objects, no sharers)
// in base's registry directory and returns the directory.
func fillSharerRegistry(t *testing.T, base string, n int) string {
	t.Helper()
	dir := filepath.Join(base, ".git", sharerDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for i := 0; i < n; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("m%05d.json", i)), []byte("{}"), 0o644))
	}
	return dir
}

// The registry listing is chunked and capped: above sharerMarkersMax markers
// only that many names are kept and the scan reports truncation; a normal
// registry is listed whole and sorted.
func TestListSharerMarkers_Capped(t *testing.T) {
	base := t.TempDir()
	dir := fillSharerRegistry(t, base, 3)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub.json"), 0o755)) // a directory is not a marker
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o644))
	names, truncated, err := listSharerMarkers(dir)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Equal(t, []string{"m00000.json", "m00001.json", "m00002.json"}, names)

	big := fillSharerRegistry(t, t.TempDir(), sharerMarkersMax+5)
	names, truncated, err = listSharerMarkers(big)
	require.NoError(t, err)
	assert.True(t, truncated)
	assert.Len(t, names, sharerMarkersMax)

	_, _, err = listSharerMarkers(filepath.Join(t.TempDir(), "absent"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

// Above the cap, "not registered" cannot be stated: FindBranchForAgent and
// UnregisterSharerElsewhere return errSharerRegistryTooLarge (on the base
// they listed everything and reported not found / nothing rewritten).
func TestSharerRegistry_OverCapFailsClosed(t *testing.T) {
	base := t.TempDir()
	fillSharerRegistry(t, base, sharerMarkersMax+1)
	_, _, found, err := FindBranchForAgent(base, "", "agent-1")
	assert.False(t, found)
	assert.ErrorIs(t, err, errSharerRegistryTooLarge)
	_, err = UnregisterSharerElsewhere(base, "agent-1", "")
	assert.ErrorIs(t, err, errSharerRegistryTooLarge)

	small := t.TempDir()
	fillSharerRegistry(t, small, 2)
	_, _, found, err = FindBranchForAgent(small, "", "agent-1")
	assert.NoError(t, err)
	assert.False(t, found)
}

// A registry of exactly sharerMarkersMax markers is read whole and is not
// truncated; one marker more truncates the scan, which keeps
// sharerMarkersMax names.
func TestListSharerMarkers_ExactMarkerCap(t *testing.T) {
	dir := fillSharerRegistry(t, t.TempDir(), sharerMarkersMax)
	names, truncated, err := listSharerMarkers(dir)
	require.NoError(t, err)
	assert.False(t, truncated, "exactly the cap is not truncated")
	assert.Len(t, names, sharerMarkersMax)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "zz-one-more.json"), []byte("{}"), 0o644))
	names, truncated, err = listSharerMarkers(dir)
	require.NoError(t, err)
	assert.True(t, truncated, "one marker beyond the cap truncates")
	assert.Len(t, names, sharerMarkersMax)
}

// A registry of exactly sharerScanMaxEntries entries is read whole; one
// entry more truncates the scan. The entry cap is lowered for the test.
func TestListSharerMarkers_ExactEntryCap(t *testing.T) {
	old := sharerScanMaxEntries
	sharerScanMaxEntries = 8
	t.Cleanup(func() { sharerScanMaxEntries = old })
	dir := fillSharerRegistry(t, t.TempDir(), 2)
	for i := 0; i < 6; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("note%d.txt", i)), nil, 0o644))
	}
	names, truncated, err := listSharerMarkers(dir)
	require.NoError(t, err)
	assert.False(t, truncated, "exactly the entry cap is not truncated")
	assert.Len(t, names, 2)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "note-one-more.txt"), nil, 0o644))
	_, truncated, err = listSharerMarkers(dir)
	require.NoError(t, err)
	assert.True(t, truncated, "one entry beyond the cap truncates")
}

// At exactly the marker cap, "not registered" can still be stated: no
// error. Above it the error names the registry directory, so an operator
// knows where to remove stale markers.
func TestSharerRegistry_ExactCapNotRefused(t *testing.T) {
	base := t.TempDir()
	dir := fillSharerRegistry(t, base, sharerMarkersMax)
	_, _, found, err := FindBranchForAgent(base, "", "agent-1")
	require.NoError(t, err)
	assert.False(t, found)
	_, err = UnregisterSharerElsewhere(base, "agent-1", "")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "zz-one-more.json"), []byte("{}"), 0o644))
	_, _, _, err = FindBranchForAgent(base, "", "agent-1")
	require.ErrorIs(t, err, errSharerRegistryTooLarge)
	assert.Contains(t, err.Error(), dir)
	assert.Contains(t, err.Error(), "remove stale marker files")
}

// A symlinked registry directory is not followed.
func TestListSharerMarkers_SymlinkedDirRefused(t *testing.T) {
	base := t.TempDir()
	target := fillSharerRegistry(t, t.TempDir(), 1)
	require.NoError(t, os.MkdirAll(filepath.Join(base, ".git"), 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(base, ".git", sharerDir)))
	_, _, err := listSharerMarkers(filepath.Join(base, ".git", sharerDir))
	assert.Error(t, err)
}
