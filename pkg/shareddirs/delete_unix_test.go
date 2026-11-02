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

package shareddirs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Concurrent deletions of one tree converge: no call fails because
// another removed entries first, and the tree is gone afterwards.
func TestDeleteTree_ConcurrentDeletion_Tolerated(t *testing.T) {
	hostBase := t.TempDir()
	leaf := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	for i := 0; i < 50; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(leaf, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o644))
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = DeleteTree(hostBase, "projects/pid-1")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "concurrent call %d must not fail just because another call raced it", i)
	}

	_, statErr := os.Stat(filepath.Join(hostBase, "projects", "pid-1"))
	assert.True(t, os.IsNotExist(statErr), "pid-1 must be fully removed after the concurrent calls settle")
}

// TestRemoveTreeContents_DepthCapEnforced: a child
// directory that would cross maxDeleteTreeDepth is left in place (named in
// the returned, aggregated error) rather than recursed into, but a sibling
// entry at the same level is still removed.
//
// This calls removeTreeContents directly at a depth already at the cap,
// rather than physically constructing a maxDeleteTreeDepth-deep directory
// tree: a real tree that deep is exactly the "each level literally doubles
// the reconstructed pathname" case some sandboxed/virtualized filesystem
// backends impose their own (much shorter, unrelated) limit on, which would
// make this test exercise the host environment's filesystem instead of this
// package's own, independent depth check. TestRemoveTreeContents_DepthCapLoweredRealChain
// below covers a real, physically deep chain instead.
func TestRemoveTreeContents_DepthCapEnforced(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "too-deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sibling.txt"), []byte("x"), 0o644))

	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	err = removeTreeContents(fd, maxDeleteTreeDepth)
	require.Error(t, err, "a child crossing the cap must be reported, not silently skipped")
	assert.Contains(t, err.Error(), fmt.Sprintf("%d", maxDeleteTreeDepth))

	_, statErr := os.Stat(filepath.Join(dir, "too-deep"))
	assert.NoError(t, statErr, "the over-deep subtree must be left in place, not partially removed")
	_, statErr = os.Stat(filepath.Join(dir, "sibling.txt"))
	assert.True(t, os.IsNotExist(statErr), "a sibling entry at the same level must still be removed")
}

// TestRemoveTreeContents_DepthCapLoweredRealChain: with the cap lowered to
// a small number, a real
// directory chain deeper than the cap is left in place (reported back as an
// aggregated error), while sibling files and directories at shallower depths
// are still fully removed -- a single over-deep subtree does not abort
// cleanup of everything else under the same parent.
func TestRemoveTreeContents_DepthCapLoweredRealChain(t *testing.T) {
	old := maxDeleteTreeDepth
	maxDeleteTreeDepth = 8
	t.Cleanup(func() { maxDeleteTreeDepth = old })

	dir := t.TempDir()

	// A real 10-level-deep chain, rooted at "chain" (so "chain" itself is
	// depth 1 relative to dir, and the deepest directory, level10, is depth
	// 10 -- past the lowered cap of 8). The cap actually fires while
	// reading level7's own contents (level7 is where "level8" is refused),
	// so level7Path is kept to plant siblings at exactly that level.
	var level7Path string
	deepPath := filepath.Join(dir, "chain")
	for i := 1; i <= 10; i++ {
		deepPath = filepath.Join(deepPath, fmt.Sprintf("level%d", i))
		if i == 7 {
			level7Path = deepPath
		}
	}
	require.NoError(t, os.MkdirAll(deepPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deepPath, "leaf.txt"), []byte("deep"), 0o644))

	// Sibling FILES inside level7, alongside "level8" (the child that
	// actually crosses the cap). These sit in the SAME removeTreeContents
	// call frame as the cap check itself, so they are what actually
	// distinguishes "skip level8, continue with the rest of level7's
	// entries" from "abort the rest of level7 too" -- a shallower sibling
	// (of "chain" itself, below) is in a different call frame and can't
	// tell the two apart. (A sibling DIRECTORY at this exact depth would
	// itself cross the cap and be left in place regardless, so it can't
	// serve as a control here -- only files, which the depth cap never
	// applies to, can.) Several files, not just one, so the assertion
	// below does not depend on directory-read order putting "level8"
	// last.
	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(level7Path, fmt.Sprintf("level7-sibling-%d.txt", i)), []byte("x"), 0o644))
	}

	// Siblings of "chain" at dir's top level.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sibling-dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sibling-dir", "inner.txt"), []byte("y"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sibling-file.txt"), []byte("x"), 0o644))

	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	err = removeTreeContents(fd, 0)
	require.Error(t, err, "the over-deep chain must be reported as a partial cleanup")
	assert.Contains(t, err.Error(), "8")

	_, statErr := os.Stat(deepPath)
	assert.NoError(t, statErr, "the over-deep chain's deepest directory must be left in place, untouched")
	_, statErr = os.Stat(filepath.Join(dir, "chain"))
	assert.NoError(t, statErr, "the chain's root is left in place too, since it is not empty")

	for i := 0; i < 5; i++ {
		_, statErr := os.Stat(filepath.Join(level7Path, fmt.Sprintf("level7-sibling-%d.txt", i)))
		assert.True(t, os.IsNotExist(statErr), "sibling file %d AT THE CAP LEVEL must still be removed, not abandoned alongside the over-deep subtree", i)
	}

	_, statErr = os.Stat(filepath.Join(dir, "sibling-dir"))
	assert.True(t, os.IsNotExist(statErr), "a sibling directory must still be fully removed")
	_, statErr = os.Stat(filepath.Join(dir, "sibling-file.txt"))
	assert.True(t, os.IsNotExist(statErr), "a sibling file must still be removed")
}

// The depth cap counts from the tree DeleteTree removes: a chain exactly
// at the cap is removed completely; one level past it is reported and left
// in place, with its root.
func TestDeleteTree_DepthCapEntryPoint_StartsAtZero(t *testing.T) {
	old := maxDeleteTreeDepth
	maxDeleteTreeDepth = 4
	t.Cleanup(func() { maxDeleteTreeDepth = old })

	hostBase := t.TempDir()

	// Exactly at the cap: must be removed completely, including the
	// project dir itself, since nothing is left behind under it.
	atCap := filepath.Join(hostBase, "projects", "pid-at-cap", "shared-dirs")
	deepAtCap := atCap
	for i := 1; i <= 4; i++ {
		deepAtCap = filepath.Join(deepAtCap, fmt.Sprintf("level%d", i))
	}
	require.NoError(t, os.MkdirAll(deepAtCap, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deepAtCap, "leaf.txt"), []byte("x"), 0o644))

	_, err := DeleteTree(hostBase, "projects/pid-at-cap/shared-dirs")
	require.NoError(t, err)
	_, statErr := os.Stat(atCap)
	assert.True(t, os.IsNotExist(statErr), "a chain exactly at the cap, entered through DeleteTree, must be fully removed")

	// One level past the cap: the deepest directory must be reported and
	// left in place, so the project dir itself survives (not empty).
	overCap := filepath.Join(hostBase, "projects", "pid-over-cap", "shared-dirs")
	deepOverCap := overCap
	for i := 1; i <= 5; i++ {
		deepOverCap = filepath.Join(deepOverCap, fmt.Sprintf("level%d", i))
	}
	require.NoError(t, os.MkdirAll(deepOverCap, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deepOverCap, "leaf.txt"), []byte("x"), 0o644))

	_, err = DeleteTree(hostBase, "projects/pid-over-cap/shared-dirs")
	require.Error(t, err, "a chain one level past the cap, entered through DeleteTree, must be reported")

	_, statErr = os.Stat(deepOverCap)
	assert.NoError(t, statErr, "the over-cap directory must be left in place, untouched")
	_, statErr = os.Stat(overCap)
	assert.NoError(t, statErr, "the tree's root must survive: it is not empty")
}

// TestReadDirNames_RemovedDirectory_TreatedAsEmpty: a directory removed out
// from under an already-open fd must read back as
// "nothing left", the same as a directory that was simply always empty —
// never surfaced as a "read directory: no such file or directory" failure.
func TestReadDirNames_RemovedDirectory_TreatedAsEmpty(t *testing.T) {
	parent := t.TempDir()
	sub := filepath.Join(parent, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))

	fd, err := unix.Open(sub, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	// Remove the directory via its path while fd (opened above) is still
	// held open, then read through the now-orphaned fd.
	require.NoError(t, os.Remove(sub))

	names, err := readDirNames(fd)
	assert.NoError(t, err, "reading a concurrently-removed directory must not be treated as a failure")
	assert.Empty(t, names)
}

// TestReadDirNames_ManyEntries_MultiBufferAccumulation exercises the
// cross-iteration accumulation readDirNames does across more than one
// unix.ReadDirent call: every other test's directory is small enough to
// fit in a single 8KiB getdents buffer, so this is the only test that
// actually forces readDirNames' for-loop around more than one read.
// ~1000 entries with ~40-char names comfortably exceeds one buffer's
// worth of dirents.
func TestReadDirNames_ManyEntries_MultiBufferAccumulation(t *testing.T) {
	dir := t.TempDir()
	const count = 1000
	want := make(map[string]struct{}, count)
	for i := 0; i < count; i++ {
		// 40 chars: "entry-" (6) + a zero-padded index (34) -- distinct
		// across all 1000 entries, well within NAME_MAX.
		name := fmt.Sprintf("entry-%034d", i)
		require.Len(t, name, 40)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o644))
		want[name] = struct{}{}
	}

	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()

	names, err := readDirNames(fd)
	require.NoError(t, err)
	require.Len(t, names, count, "every entry across every buffer fill must be accumulated, none dropped or duplicated")

	got := make(map[string]struct{}, len(names))
	for _, n := range names {
		got[n] = struct{}{}
	}
	assert.Equal(t, want, got, "the returned set must be exactly the created entries")
}

// TestDeleteSharedDir_RemovesOnlyTheNamedLeaf: DeleteSharedDir removes
// exactly the named shared dir and its content, leaving shared-dirs/ itself
// and any sibling shared dir alone.
func TestDeleteSharedDir_RemovesOnlyTheNamedLeaf(t *testing.T) {
	hostBase := t.TempDir()
	sharedDirs := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs")
	target := filepath.Join(sharedDirs, "artifacts")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "nested"), 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(target, "file.txt"), []byte("hi"), 0o644))

	sibling := filepath.Join(sharedDirs, "scratchpad")
	require.NoError(t, os.MkdirAll(sibling, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "keep.txt"), []byte("keep"), 0o644))

	require.NoError(t, DeleteSharedDir(hostBase, "projects", "pid-1", "artifacts"))

	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err), "the named shared dir must be fully removed")

	_, err = os.Stat(sharedDirs)
	assert.NoError(t, err, "shared-dirs/ itself must survive")

	info, err := os.Stat(filepath.Join(sibling, "keep.txt"))
	require.NoError(t, err, "a sibling shared dir must survive untouched")
	assert.False(t, info.IsDir())
}

// TestDeleteSharedDir_NothingToDelete_IsANoOp mirrors
// the DeleteTree tests for the single-leaf variant.
func TestDeleteSharedDir_NothingToDelete_IsANoOp(t *testing.T) {
	hostBase := t.TempDir()

	t.Run("shared-dirs itself absent", func(t *testing.T) {
		assert.NoError(t, DeleteSharedDir(hostBase, "projects", "pid-1", "artifacts"))
	})

	t.Run("shared-dirs present, named leaf absent", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects", "pid-1", "shared-dirs"), 0o755))
		assert.NoError(t, DeleteSharedDir(hostBase, "projects", "pid-1", "artifacts"))
	})
}

// TestDeleteSharedDir_InvalidInputs_Refused mirrors the project-ID/name
// validation DeleteTree already requires.
func TestDeleteSharedDir_InvalidInputs_Refused(t *testing.T) {
	hostBase := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))

	err := DeleteSharedDir(hostBase, "projects", "../victim", "artifacts")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid project ID")

	err = DeleteSharedDir(hostBase, "projects", "pid-1", "../victim")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid shared dir name")
}

// TestDeleteSharedDir_MissingHostBase_IsAnError: a missing host base (e.g.
// an unmounted export) must be an error, not the nil "nothing to do"
// result.
func TestDeleteSharedDir_MissingHostBase_IsAnError(t *testing.T) {
	err := DeleteSharedDir(filepath.Join(t.TempDir(), "does-not-exist"), "projects", "pid-1", "artifacts")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

// TestDeleteSharedDir_SymlinkedSharedDirsComponent_Refused: a symlinked
// shared-dirs/ component -- e.g. planted by an agent with write access to
// the project directory -- must be refused outright, never traversed into
// an arbitrary victim tree.
func TestDeleteSharedDir_SymlinkedSharedDirsComponent_Refused(t *testing.T) {
	hostBase := t.TempDir()
	pidDir := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(pidDir, "shared-dirs")))

	err := DeleteSharedDir(hostBase, "projects", "pid-1", "artifacts")
	require.Error(t, err, "a symlinked shared-dirs component must be refused, not traversed")

	info, statErr := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, statErr, "the victim must be untouched")
	assert.False(t, info.IsDir())
}

// TestDeleteSharedDir_SymlinkedPidComponent_Refused: DeleteSharedDir walks
// subPathRoot/projectID/shared-dirs one component at a time via the same
// openExistingDirPathNoFollow used for
// subPathRoot itself, so a symlinked <pid> (projectID) component must be
// refused exactly like a symlinked shared-dirs component already is
// (TestDeleteSharedDir_SymlinkedSharedDirsComponent_Refused above) --
// mirroring DeleteTree's own
// the DeleteTree tests. Without a dedicated
// test, a regression that special-cased just the <pid> hop to follow
// symlinks (while leaving every other hop NOFOLLOW) would pass every other
// committed DeleteSharedDir test.
func TestDeleteSharedDir_SymlinkedPidComponent_Refused(t *testing.T) {
	hostBase := t.TempDir()
	subRoot := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))

	victim := t.TempDir()
	victimLeaf := filepath.Join(victim, "shared-dirs", "artifacts")
	require.NoError(t, os.MkdirAll(victimLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(victimLeaf, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(subRoot, "pid-1")))

	err := DeleteSharedDir(hostBase, "projects", "pid-1", "artifacts")
	require.Error(t, err, "a symlinked pid component must be refused, not traversed")

	got, statErr := os.ReadFile(filepath.Join(victimLeaf, "secret.txt"))
	require.NoError(t, statErr, "the victim must be untouched")
	assert.Equal(t, "victim data", string(got))

	target, readErr := os.Readlink(filepath.Join(subRoot, "pid-1"))
	require.NoError(t, readErr, "the symlink component itself must not have been removed either")
	assert.Equal(t, victim, target)
}

// TestDeleteSharedDir_SymlinkedSubPathRootIntermediateComponent_Refused: a
// multi-component subPathRoot with a symlinked INTERMEDIATE component (not
// the final one) must be refused the
// same way DeleteTree's own
// the DeleteTree tests
// already requires, since DeleteSharedDir walks the whole
// subPathRoot/projectID/shared-dirs chain through the same
// openExistingDirPathNoFollow helper.
func TestDeleteSharedDir_SymlinkedSubPathRootIntermediateComponent_Refused(t *testing.T) {
	hostBase := t.TempDir()

	victim := t.TempDir()
	victimLeaf := filepath.Join(victim, "b", "pid-1", "shared-dirs", "artifacts")
	require.NoError(t, os.MkdirAll(victimLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(victimLeaf, "secret.txt"), []byte("victim data"), 0o644))

	// hostBase/a is a symlink to victim, so hostBase/a/b would resolve to
	// victim/b if the intermediate "a" component were ever followed.
	require.NoError(t, os.Symlink(victim, filepath.Join(hostBase, "a")))

	err := DeleteSharedDir(hostBase, filepath.Join("a", "b"), "pid-1", "artifacts")
	require.Error(t, err, "a symlinked intermediate subpath_root component must be refused, not traversed")

	got, statErr := os.ReadFile(filepath.Join(victimLeaf, "secret.txt"))
	require.NoError(t, statErr, "the victim must be completely untouched")
	assert.Equal(t, "victim data", string(got))
}

// TestDeleteSharedDir_SymlinkedLeaf_Refused is the leaf-level half: the
// named shared dir itself being a symlink must be refused too, not unlinked
// as a plain directory would be, and not traversed into its target either.
func TestDeleteSharedDir_SymlinkedLeaf_Refused(t *testing.T) {
	hostBase := t.TempDir()
	sharedDirs := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs")
	require.NoError(t, os.MkdirAll(sharedDirs, 0o755))

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(sharedDirs, "artifacts")))

	err := DeleteSharedDir(hostBase, "projects", "pid-1", "artifacts")
	require.Error(t, err, "a symlinked shared dir leaf must be refused, not traversed or unlinked as a directory")

	info, statErr := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, statErr, "the victim must be untouched")
	assert.False(t, info.IsDir())

	target, readErr := os.Readlink(filepath.Join(sharedDirs, "artifacts"))
	require.NoError(t, readErr, "the symlink itself must still be exactly what it was")
	assert.Equal(t, victim, target)
}

// TestDeleteSharedDir_SymlinkInsideLeaf_UnlinkedNotTraversed confirms
// content-level symlink safety carries over from DeleteTree's own
// walk: a symlink planted as leaf CONTENT is unlinked directly, never
// followed.
func TestDeleteSharedDir_SymlinkInsideLeaf_UnlinkedNotTraversed(t *testing.T) {
	hostBase := t.TempDir()
	leaf := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs", "artifacts")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(leaf, "escape")))

	require.NoError(t, DeleteSharedDir(hostBase, "projects", "pid-1", "artifacts"))

	_, err := os.Stat(leaf)
	assert.True(t, os.IsNotExist(err), "the shared dir must be fully removed (its symlink is unlinked, not traversed)")

	info, statErr := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, statErr, "the victim must be untouched")
	assert.False(t, info.IsDir())
}

// TestDeleteProjectRoot_RemovesWholeTree: every entry under the project dir
// (workspace, provisioning state, configuration, shared dirs, a symlink
// pointing outside) is removed, the symlink is unlinked and never followed,
// the project dir itself is gone, and sibling projects are untouched.
func TestDeleteProjectRoot_RemovesWholeTree(t *testing.T) {
	hostBase := t.TempDir()
	root := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workspace", ".git", "objects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workspace", "README.md"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "provision"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "provision", ".lock"), nil, 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".scion"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "shared-dirs", "scratch"), 0o2775))
	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("victim"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(root, "workspace", "out")))
	sibling := filepath.Join(hostBase, "projects", "pid-2", "workspace")
	require.NoError(t, os.MkdirAll(sibling, 0o755))

	require.NoError(t, DeleteProjectRoot(hostBase, "projects", "pid-1"))

	_, err := os.Lstat(root)
	assert.True(t, os.IsNotExist(err), "the project dir is removed")
	_, err = os.Stat(filepath.Join(victim, "keep.txt"))
	assert.NoError(t, err, "a symlink target outside the tree is untouched")
	assert.DirExists(t, sibling, "a sibling project is untouched")
	assert.DirExists(t, filepath.Join(hostBase, "projects"), "the subpath root stays")

	// Repeated or concurrent calls: nothing to do.
	require.NoError(t, DeleteProjectRoot(hostBase, "projects", "pid-1"))
	require.NoError(t, DeleteProjectRoot(hostBase, "absent-root", "pid-1"))
}

// TestDeleteProjectRoot_SymlinkedComponentsRefused: a symlinked project dir
// or subpath root component is refused, not traversed, and left as is.
func TestDeleteProjectRoot_SymlinkedComponentsRefused(t *testing.T) {
	hostBase := t.TempDir()
	subRoot := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))
	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(subRoot, "pid-1")))

	require.Error(t, DeleteProjectRoot(hostBase, "projects", "pid-1"), "a symlinked project dir is refused")
	_, err := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, err, "the victim is untouched")
	target, err := os.Readlink(filepath.Join(subRoot, "pid-1"))
	require.NoError(t, err)
	assert.Equal(t, victim, target, "the symlink itself is left as is")

	other := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(victim, "pid-3"), 0o755))
	require.NoError(t, os.Symlink(victim, filepath.Join(other, "projects")))
	require.Error(t, DeleteProjectRoot(other, "projects", "pid-3"), "a symlinked subpath root is refused")
	assert.DirExists(t, filepath.Join(victim, "pid-3"))

	require.Error(t, DeleteProjectRoot(hostBase, "projects", "../pid-1"), "an invalid project ID is refused")
}

// TestDeleteTree_RemovesOnlyTheLeaf: a multi-component path is removed with
// everything under it; its parents and siblings stay; a symlink inside is
// unlinked, never followed; a repeated call is nothing to do.
func TestDeleteTree_RemovesOnlyTheLeaf(t *testing.T) {
	hostBase := t.TempDir()
	leaf := filepath.Join(hostBase, "projects", "pid-1", "agents", "dev")
	require.NoError(t, os.MkdirAll(filepath.Join(leaf, "workspace", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "workspace", "sub", "f"), []byte("x"), 0o644))
	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("victim"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(leaf, "out")))
	sibling := filepath.Join(hostBase, "projects", "pid-1", "agents", "other")
	require.NoError(t, os.MkdirAll(sibling, 0o755))

	existed, err := DeleteTree(hostBase, "projects/pid-1/agents/dev")
	require.NoError(t, err)
	assert.True(t, existed)
	assert.NoDirExists(t, leaf)
	assert.DirExists(t, sibling)
	assert.FileExists(t, filepath.Join(victim, "keep.txt"))

	existed, err = DeleteTree(hostBase, "projects/pid-1/agents/dev")
	require.NoError(t, err)
	assert.False(t, existed, "a missing leaf is nothing to do")
	existed, err = DeleteTree(hostBase, "absent/pid-1")
	require.NoError(t, err)
	assert.False(t, existed, "a missing parent is nothing to do")
}

// TestDeleteTree_SymlinkAtAnyComponentRefused: a symlink at the leaf or at
// any parent component is refused and its target left untouched.
func TestDeleteTree_SymlinkAtAnyComponentRefused(t *testing.T) {
	for _, linkAt := range []string{"projects", "projects/pid-1", "projects/pid-1/agents", "projects/pid-1/agents/dev"} {
		t.Run(linkAt, func(t *testing.T) {
			hostBase := t.TempDir()
			victim := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(victim, "pid-1", "agents", "dev", "agents", "dev"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("v"), 0o644))
			link := filepath.Join(hostBase, linkAt)
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
			require.NoError(t, os.Symlink(victim, link))

			_, err := DeleteTree(hostBase, "projects/pid-1/agents/dev")
			require.Error(t, err)
			assert.FileExists(t, filepath.Join(victim, "keep.txt"))
			assert.DirExists(t, filepath.Join(victim, "pid-1", "agents", "dev"))
		})
	}
}

// TestDeleteTree_InvalidInputs: an unclean or absolute path and a missing
// host base are errors, and nothing is removed.
func TestDeleteTree_InvalidInputs(t *testing.T) {
	hostBase := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "keep"), 0o755))
	for _, rel := range []string{"", ".", "..", "../keep", "a/../keep", "/keep", "a//b", "a/./b"} {
		_, err := DeleteTree(hostBase, rel)
		assert.Error(t, err, "path %q", rel)
	}
	assert.DirExists(t, filepath.Join(hostBase, "keep"))
	_, err := DeleteTree(filepath.Join(hostBase, "absent"), "keep")
	assert.Error(t, err, "a missing host base is an error")
}

// TestRemoveDirIfEmpty: an empty directory is removed; a non-empty one is
// kept with everything in it; an absent one is nothing to do; a symlinked
// leaf or parent is refused.
func TestRemoveDirIfEmpty(t *testing.T) {
	hostBase := t.TempDir()
	empty := filepath.Join(hostBase, "p", "agents", "a")
	full := filepath.Join(hostBase, "p", "agents", "b")
	require.NoError(t, os.MkdirAll(empty, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(full, "workspace"), 0o755))

	existed, notEmpty, err := RemoveDirIfEmpty(hostBase, "p/agents/a")
	require.NoError(t, err)
	assert.True(t, existed)
	assert.False(t, notEmpty)
	assert.NoDirExists(t, empty)

	existed, notEmpty, err = RemoveDirIfEmpty(hostBase, "p/agents/b")
	require.NoError(t, err)
	assert.True(t, existed)
	assert.True(t, notEmpty)
	assert.DirExists(t, filepath.Join(full, "workspace"))

	existed, _, err = RemoveDirIfEmpty(hostBase, "p/agents/a")
	require.NoError(t, err)
	assert.False(t, existed)

	victim := t.TempDir()
	require.NoError(t, os.Symlink(victim, filepath.Join(hostBase, "p", "agents", "c")))
	_, _, err = RemoveDirIfEmpty(hostBase, "p/agents/c")
	assert.ErrorIs(t, err, ErrTreeRefused)
	assert.DirExists(t, victim)
	_, _, err = RemoveDirIfEmpty(hostBase, "../x")
	assert.ErrorIs(t, err, ErrTreeRefused)
}

// The tree file helpers write, read, list and remove a file below a host
// base without following a symlink at any component, and refuse a
// non-regular file or a symlinked component.
func TestTreeFileHelpers(t *testing.T) {
	hostBase := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects", "pid-1"), 0o755))

	require.NoError(t, WriteFileInTree(hostBase, "projects/pid-1/provision", "marker", []byte("v1\n"), true))
	require.NoError(t, WriteFileInTree(hostBase, "projects/pid-1/provision", "marker", []byte("v2\n"), true))
	data, ok, err := ReadFileInTree(hostBase, "projects/pid-1/provision", "marker", 64)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "v2\n", string(data))
	names, ok, err := ListDirInTree(hostBase, "projects")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, []string{"pid-1"}, names)

	_, ok, err = ReadFileInTree(hostBase, "projects/absent/provision", "marker", 64)
	require.NoError(t, err)
	assert.False(t, ok, "an absent component reads as absent")
	_, ok, err = ListDirInTree(hostBase, "absent")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Error(t, WriteFileInTree(hostBase, "projects/absent/provision", "marker", nil, true), "only the last component is created")
	_, _, err = ReadFileInTree(hostBase, "projects/pid-1/provision", "marker", 1)
	assert.ErrorIs(t, err, ErrTreeRefused, "an oversized file is refused")

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "marker"), []byte("victim"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(hostBase, "projects", "pid-2")))
	assert.ErrorIs(t, WriteFileInTree(hostBase, "projects/pid-2", "marker", []byte("x"), false), ErrTreeRefused)
	_, _, err = ReadFileInTree(hostBase, "projects/pid-2", "marker", 64)
	assert.ErrorIs(t, err, ErrTreeRefused)
	assert.ErrorIs(t, RemoveFileInTree(hostBase, "projects/pid-2", "marker"), ErrTreeRefused)
	got, err := os.ReadFile(filepath.Join(victim, "marker"))
	require.NoError(t, err)
	assert.Equal(t, "victim", string(got), "a symlinked component is never followed")

	require.NoError(t, os.Symlink(filepath.Join(victim, "marker"), filepath.Join(hostBase, "projects", "pid-1", "provision", "link")))
	_, _, err = ReadFileInTree(hostBase, "projects/pid-1/provision", "link", 64)
	assert.ErrorIs(t, err, ErrTreeRefused, "a symlinked file is refused")
	assert.ErrorIs(t, WriteFileInTree(hostBase, "projects/pid-1/provision", "link", []byte("x"), false), ErrTreeRefused)
	require.NoError(t, RemoveFileInTree(hostBase, "projects/pid-1/provision", "link"), "a symlink is unlinked")
	got, err = os.ReadFile(filepath.Join(victim, "marker"))
	require.NoError(t, err)
	assert.Equal(t, "victim", string(got))

	require.NoError(t, RemoveFileInTree(hostBase, "projects/pid-1/provision", "marker"))
	require.NoError(t, RemoveFileInTree(hostBase, "projects/pid-1/provision", "marker"), "removing again is nothing to do")
	for _, bad := range []string{"", ".", "..", "a/b"} {
		assert.ErrorIs(t, WriteFileInTree(hostBase, "projects/pid-1/provision", bad, nil, false), ErrTreeRefused, "name %q", bad)
	}
}
