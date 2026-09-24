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

// TestDeleteProjectTree_RemovesSharedDirsAndEmptyPidDir is the happy path
// (ptone/scion#1802): a project with shared dirs containing real content —
// files and a nested subdirectory — is fully removed, and the now-empty pid
// dir is removed too, while sibling projects and subpath_root itself are
// untouched.
func TestDeleteProjectTree_RemovesSharedDirsAndEmptyPidDir(t *testing.T) {
	hostBase := t.TempDir()
	sharedDirs := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs")
	require.NoError(t, os.MkdirAll(filepath.Join(sharedDirs, "scratchpad", "nested"), 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(sharedDirs, "scratchpad", "file.txt"), []byte("hi"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sharedDirs, "scratchpad", "nested", "inner.txt"), []byte("hi"), 0o644))

	// A sibling project must survive untouched.
	siblingLeaf := filepath.Join(hostBase, "projects", "pid-2", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(siblingLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(siblingLeaf, "keep.txt"), []byte("keep"), 0o644))

	require.NoError(t, DeleteProjectTree(hostBase, "projects", "pid-1"))

	_, err := os.Stat(filepath.Join(hostBase, "projects", "pid-1"))
	assert.True(t, os.IsNotExist(err), "pid-1's directory should be gone entirely")

	_, err = os.Stat(filepath.Join(hostBase, "projects"))
	assert.NoError(t, err, "subpath_root itself must survive")

	info, err := os.Stat(filepath.Join(siblingLeaf, "keep.txt"))
	require.NoError(t, err, "sibling project's content must survive untouched")
	assert.False(t, info.IsDir())
}

// TestDeleteProjectTree_PidDirNotEmpty_Survives covers "the pid dir if it's
// then empty": if something other than shared-dirs lives under the pid dir,
// deletion must not force it away.
func TestDeleteProjectTree_PidDirNotEmpty_Survives(t *testing.T) {
	hostBase := t.TempDir()
	pidDir := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(filepath.Join(pidDir, "shared-dirs", "scratchpad"), 0o2775))
	require.NoError(t, os.MkdirAll(filepath.Join(pidDir, "something-else"), 0o755))

	require.NoError(t, DeleteProjectTree(hostBase, "projects", "pid-1"))

	_, err := os.Stat(filepath.Join(pidDir, "shared-dirs"))
	assert.True(t, os.IsNotExist(err), "shared-dirs must be fully removed")

	info, err := os.Stat(filepath.Join(pidDir, "something-else"))
	require.NoError(t, err, "the pid dir must survive since it isn't empty, and its other content is untouched")
	assert.True(t, info.IsDir())
}

// TestDeleteProjectTree_SymlinkInsideLeaf_UnlinkedNotTraversed is the
// required "symlinks inside the tree pointing at a victim or outside ⇒
// victim intact" test.
func TestDeleteProjectTree_SymlinkInsideLeaf_UnlinkedNotTraversed(t *testing.T) {
	hostBase := t.TempDir()
	leaf := filepath.Join(hostBase, "projects", "pid-1", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(leaf, "escape")))

	// A symlink to a sibling project's leaf, staying inside the export.
	siblingLeaf := filepath.Join(hostBase, "projects", "pid-2", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(siblingLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(siblingLeaf, "sibling.txt"), []byte("sibling data"), 0o644))
	require.NoError(t, os.Symlink(siblingLeaf, filepath.Join(leaf, "escape-in-base")))

	require.NoError(t, DeleteProjectTree(hostBase, "projects", "pid-1"))

	_, err := os.Stat(filepath.Join(hostBase, "projects", "pid-1"))
	assert.True(t, os.IsNotExist(err), "pid-1 must be fully removed (its symlinks are unlinked, not traversed)")

	// The symlink targets themselves must be completely untouched.
	info, err := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, err, "the outside victim must be untouched")
	assert.False(t, info.IsDir())

	info, err = os.Stat(filepath.Join(siblingLeaf, "sibling.txt"))
	require.NoError(t, err, "the in-base sibling victim must be untouched")
	assert.False(t, info.IsDir())
}

// TestDeleteProjectTree_SymlinkedPidComponent_Refused is the required "a
// symlinked pid/shared-dirs component ⇒ refused or unlinked, never
// traversed" test, for the pid component.
func TestDeleteProjectTree_SymlinkedPidComponent_Refused(t *testing.T) {
	hostBase := t.TempDir()
	subRoot := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(subRoot, "pid-1")))

	err := DeleteProjectTree(hostBase, "projects", "pid-1")
	require.Error(t, err, "a symlinked pid component must be refused, not traversed")

	info, statErr := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, statErr, "the victim must be untouched")
	assert.False(t, info.IsDir())

	// The symlink itself must still be exactly what it was.
	target, readErr := os.Readlink(filepath.Join(subRoot, "pid-1"))
	require.NoError(t, readErr, "the symlink component itself must not have been removed either")
	assert.Equal(t, victim, target)
}

// TestDeleteProjectTree_SymlinkedSharedDirsComponent_Refused is the
// shared-dirs half of the same requirement.
func TestDeleteProjectTree_SymlinkedSharedDirsComponent_Refused(t *testing.T) {
	hostBase := t.TempDir()
	pidDir := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(pidDir, "shared-dirs")))

	err := DeleteProjectTree(hostBase, "projects", "pid-1")
	require.Error(t, err, "a symlinked shared-dirs component must be refused, not traversed")

	info, statErr := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, statErr, "the victim must be untouched")
	assert.False(t, info.IsDir())
}

// TestDeleteProjectTree_NothingToDelete_IsANoOp covers the "no error when
// there's nothing under subpath_root/projectID at all" case, including
// project IDs that never had any shared dirs.
func TestDeleteProjectTree_NothingToDelete_IsANoOp(t *testing.T) {
	hostBase := t.TempDir()

	t.Run("subpath_root itself absent", func(t *testing.T) {
		assert.NoError(t, DeleteProjectTree(hostBase, "projects", "pid-1"))
	})

	t.Run("subpath_root present, project absent", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))
		assert.NoError(t, DeleteProjectTree(hostBase, "projects", "pid-never-existed"))
	})
}

// TestDeleteProjectTree_InvalidProjectID_Refused mirrors the same
// project-ID validation used at creation time: deletion must not accept a
// traversal-shaped project ID either.
func TestDeleteProjectTree_InvalidProjectID_Refused(t *testing.T) {
	hostBase := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))

	err := DeleteProjectTree(hostBase, "projects", "../victim")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid project ID")
}

// TestDeleteProjectTree_ConcurrentDeletion_Tolerated is the required
// "concurrent-deletion tolerance test": calling DeleteProjectTree
// concurrently (or twice in a row) for the same project must not error just
// because another call already removed part of the tree.
func TestDeleteProjectTree_ConcurrentDeletion_Tolerated(t *testing.T) {
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
			errs[i] = DeleteProjectTree(hostBase, "projects", "pid-1")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "concurrent call %d must not fail just because another call raced it", i)
	}

	_, statErr := os.Stat(filepath.Join(hostBase, "projects", "pid-1"))
	assert.True(t, os.IsNotExist(statErr), "pid-1 must be fully removed after the concurrent calls settle")
}

// TestDeleteProjectTree_HostBaseMissing_Refused covers "refuse and log when
// the base is missing".
func TestDeleteProjectTree_HostBaseMissing_Refused(t *testing.T) {
	hostBase := filepath.Join(t.TempDir(), "does-not-exist")
	err := DeleteProjectTree(hostBase, "projects", "pid-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

// TestDeleteProjectTree_MultiComponentSubPathRoot_Works is the legitimate
// half of the multi-component subpath_root behavior:
// config.validateSubPathRoot allows a multi-segment relative subpath_root
// (e.g. "a/b"), and deletion must walk it correctly, not just refuse it.
func TestDeleteProjectTree_MultiComponentSubPathRoot_Works(t *testing.T) {
	hostBase := t.TempDir()
	leaf := filepath.Join(hostBase, "a", "b", "pid-1", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "file.txt"), []byte("hi"), 0o644))

	require.NoError(t, DeleteProjectTree(hostBase, filepath.Join("a", "b"), "pid-1"))

	_, err := os.Stat(filepath.Join(hostBase, "a", "b", "pid-1"))
	assert.True(t, os.IsNotExist(err), "pid-1 must be fully removed")
	_, err = os.Stat(filepath.Join(hostBase, "a", "b"))
	assert.NoError(t, err, "subpath_root itself must survive")
}

// TestDeleteProjectTree_SymlinkedSubPathRootIntermediateComponent_Refused:
// openat(2)'s O_NOFOLLOW only refuses a
// symlink at the FINAL component of a path string, so a naive
// single-openat-call walk of a multi-component subpath_root like "a/b"
// would silently follow a symlinked "a" and operate inside whatever it
// points at. Component-by-component walking (openExistingDirPathNoFollow)
// must refuse this instead.
func TestDeleteProjectTree_SymlinkedSubPathRootIntermediateComponent_Refused(t *testing.T) {
	hostBase := t.TempDir()

	victim := t.TempDir()
	victimLeaf := filepath.Join(victim, "b", "pid-1", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(victimLeaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(victimLeaf, "secret.txt"), []byte("victim data"), 0o644))

	// hostBase/a is a symlink to victim, so hostBase/a/b would resolve to
	// victim/b if the intermediate "a" component were ever followed.
	require.NoError(t, os.Symlink(victim, filepath.Join(hostBase, "a")))

	err := DeleteProjectTree(hostBase, filepath.Join("a", "b"), "pid-1")
	require.Error(t, err, "a symlinked intermediate subpath_root component must be refused, not traversed")

	info, statErr := os.Stat(filepath.Join(victimLeaf, "secret.txt"))
	require.NoError(t, statErr, "the victim must be completely untouched")
	assert.False(t, info.IsDir())
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

// TestDeleteProjectTree_DepthCapEntryPoint_StartsAtZero pins the depth
// DeleteProjectTree itself passes to removeTreeContents for shared-dirs/'s
// own contents: it must be 0, exactly like a fresh top-level call, not some
// other value that would make the cap fire one level early or late. A chain
// exactly `cap` levels deep under shared-dirs/ must be fully removable
// end to end through DeleteProjectTree; a chain one level deeper must be
// reported and left in place.
func TestDeleteProjectTree_DepthCapEntryPoint_StartsAtZero(t *testing.T) {
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

	require.NoError(t, DeleteProjectTree(hostBase, "projects", "pid-at-cap"))
	_, statErr := os.Stat(filepath.Join(hostBase, "projects", "pid-at-cap"))
	assert.True(t, os.IsNotExist(statErr), "a chain exactly at the cap, entered through DeleteProjectTree, must be fully removed")

	// One level past the cap: the deepest directory must be reported and
	// left in place, so the project dir itself survives (not empty).
	overCap := filepath.Join(hostBase, "projects", "pid-over-cap", "shared-dirs")
	deepOverCap := overCap
	for i := 1; i <= 5; i++ {
		deepOverCap = filepath.Join(deepOverCap, fmt.Sprintf("level%d", i))
	}
	require.NoError(t, os.MkdirAll(deepOverCap, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deepOverCap, "leaf.txt"), []byte("x"), 0o644))

	err := DeleteProjectTree(hostBase, "projects", "pid-over-cap")
	require.Error(t, err, "a chain one level past the cap, entered through DeleteProjectTree, must be reported")

	_, statErr = os.Stat(deepOverCap)
	assert.NoError(t, statErr, "the over-cap directory must be left in place, untouched")
	_, statErr = os.Stat(filepath.Join(hostBase, "projects", "pid-over-cap"))
	assert.NoError(t, statErr, "the project dir must survive: shared-dirs/ under it is not empty")
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
// TestDeleteProjectTree_NothingToDelete_IsANoOp for the single-leaf variant.
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
// validation DeleteProjectTree already requires.
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
// mirroring DeleteProjectTree's own
// TestDeleteProjectTree_SymlinkedPidComponent_Refused. Without a dedicated
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
// same way DeleteProjectTree's own
// TestDeleteProjectTree_SymlinkedSubPathRootIntermediateComponent_Refused
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
// content-level symlink safety carries over from DeleteProjectTree's own
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
