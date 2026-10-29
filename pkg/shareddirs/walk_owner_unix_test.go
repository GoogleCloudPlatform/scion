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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// chownCall is one fchownDir call: the directory's inode and the owner.
type chownCall struct {
	ino      uint64
	uid, gid int
}

// recordChowns makes the walk see euid as its effective uid and records
// every fchownDir call instead of making it, so the owner rule is tested
// without root. It returns the recorded calls.
func recordChowns(t *testing.T, euid int) *[]chownCall {
	t.Helper()
	oldEuid, oldChown := geteuid, fchownDir
	t.Cleanup(func() { geteuid, fchownDir = oldEuid, oldChown })
	calls := &[]chownCall{}
	geteuid = func() int { return euid }
	fchownDir = func(fd, uid, gid int) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		*calls = append(*calls, chownCall{ino: uint64(st.Ino), uid: uid, gid: gid})
		return nil
	}
	return calls
}

// inodeOf returns the inode of path (not following a final symlink).
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	var st unix.Stat_t
	require.NoError(t, unix.Lstat(path, &st))
	return uint64(st.Ino)
}

var testOwner = Owner{UID: 1000, GID: 1001}

// A root creator with an owner gives every directory it creates (each
// intermediate and the leaf) that owner, on the walk's descriptor, and
// the leaf keeps mode 2775.
func TestEnsureLeafOwned_RootCreatorChownsCreatedDirs(t *testing.T) {
	base := t.TempDir()
	calls := recordChowns(t, 0)
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad")

	fd, existed, err := EnsureLeafOwned(base, rel, testOwner)
	require.NoError(t, err)
	t.Cleanup(func() { _ = CloseFd(fd) })
	assert.False(t, existed)

	var want []chownCall
	for _, p := range []string{"projects", "projects/pid-1", "projects/pid-1/shared-dirs", rel} {
		want = append(want, chownCall{ino: inodeOf(t, filepath.Join(base, p)), uid: 1000, gid: 1001})
	}
	assert.Equal(t, want, *calls, "every created directory, and only those, gets the owner")

	info, err := os.Stat(filepath.Join(base, rel))
	require.NoError(t, err)
	assert.Equal(t, os.ModeDir|os.ModeSetgid|0o775, info.Mode(), "the leaf keeps setgid 2775")
}

// Directories that already existed are never changed; only what the call
// creates gets the owner.
func TestEnsureLeafOwned_PreExistingDirsUnchanged(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "projects", "pid-1"), 0o755))
	calls := recordChowns(t, 0)
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad")

	fd, _, err := EnsureLeafOwned(base, rel, testOwner)
	require.NoError(t, err)
	require.NoError(t, CloseFd(fd))
	assert.Equal(t, []chownCall{
		{ino: inodeOf(t, filepath.Join(base, "projects", "pid-1", "shared-dirs")), uid: 1000, gid: 1001},
		{ino: inodeOf(t, filepath.Join(base, rel)), uid: 1000, gid: 1001},
	}, *calls)

	*calls = nil
	fd, existed, err := EnsureLeafOwned(base, rel, testOwner)
	require.NoError(t, err)
	require.NoError(t, CloseFd(fd))
	assert.True(t, existed)
	assert.Empty(t, *calls, "an existing leaf is left as it is")
}

// A creator that is not root, or an unset owner, chowns nothing.
func TestEnsureLeafOwned_NonRootOrNoOwnerUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		euid  int
		owner Owner
	}{
		{name: "non-root creator", euid: 4242, owner: testOwner},
		{name: "root, no owner", euid: 0, owner: Owner{}},
		{name: "root, uid only", euid: 0, owner: Owner{UID: 1000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			calls := recordChowns(t, tc.euid)
			fd, _, err := EnsureLeafOwned(base, filepath.Join("projects", "pid-1", "shared-dirs", "a"), tc.owner)
			require.NoError(t, err)
			require.NoError(t, CloseFd(fd))
			assert.Empty(t, *calls)
		})
	}
}

// A symlinked intermediate is refused before anything past it is created
// or chowned; the symlink's target is untouched.
func TestEnsureLeafOwned_SymlinkedIntermediateRefusedNotChowned(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "projects"), 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(base, "projects", "pid-1")))
	calls := recordChowns(t, 0)

	_, _, err := EnsureLeafOwned(base, filepath.Join("projects", "pid-1", "shared-dirs", "a"), testOwner)
	require.Error(t, err)
	assert.Empty(t, *calls, "nothing is chowned")
	entries, readErr := os.ReadDir(outside)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "nothing is created through the symlink")
}

// A failed chown of a leaf this call created removes the leaf, as a failed
// chmod does, and keeps the intermediates (chowned before it).
func TestEnsureLeafOwned_ChownFailureRemovesLeaf(t *testing.T) {
	base := t.TempDir()
	recordChowns(t, 0)
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "a")
	n := 0
	fchownDir = func(fd, uid, gid int) error {
		n++
		if n == 4 { // the three intermediates come first, then the leaf
			return unix.EPERM
		}
		return nil
	}

	_, _, err := EnsureLeafOwned(base, rel, testOwner)
	require.Error(t, err)
	assert.True(t, errors.Is(err, unix.EPERM), "err = %v", err)
	assert.NoDirExists(t, filepath.Join(base, rel), "the leaf this call created is removed")
	assert.DirExists(t, filepath.Join(base, "projects", "pid-1", "shared-dirs"), "intermediates are kept")
	assert.Equal(t, 4, n)
}

// EnsurePrivateDirOwned applies the same rule to a private directory such
// as a project's configuration directory, which keeps mode 2755.
func TestEnsurePrivateDirOwned_RootCreatorChownsCreatedDirs(t *testing.T) {
	base := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "projects"), 0o755))
	calls := recordChowns(t, 0)
	rel := filepath.Join("projects", "pid-1", ".scion")

	fd, existed, err := EnsurePrivateDirOwned(base, rel, testOwner)
	require.NoError(t, err)
	require.NoError(t, CloseFd(fd))
	assert.False(t, existed)
	assert.Equal(t, []chownCall{
		{ino: inodeOf(t, filepath.Join(base, "projects", "pid-1")), uid: 1000, gid: 1001},
		{ino: inodeOf(t, filepath.Join(base, rel)), uid: 1000, gid: 1001},
	}, *calls)
	info, err := os.Stat(filepath.Join(base, rel))
	require.NoError(t, err)
	assert.Equal(t, os.ModeDir|os.ModeSetgid|0o755, info.Mode())
}

// With real root (no recorder), the created leaf and intermediates really
// get the owner's uid and gid with setgid 2775. It needs root: CI runs
// unprivileged and skips it; the recorder tests above cover the rule there.
// Run it as root on a Linux host (for example: sudo go test -run
// TestEnsureLeafOwned_RealRoot ./pkg/shareddirs/).
func TestEnsureLeafOwned_RealRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (chown to another uid); covered without root by TestEnsureLeafOwned_RootCreatorChownsCreatedDirs")
	}
	base := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad")
	fd, _, err := EnsureLeafOwned(base, rel, testOwner)
	require.NoError(t, err)
	require.NoError(t, CloseFd(fd))
	for _, p := range []string{"projects", "projects/pid-1", "projects/pid-1/shared-dirs", rel} {
		var st unix.Stat_t
		require.NoError(t, unix.Lstat(filepath.Join(base, p), &st))
		assert.Equal(t, uint32(1000), st.Uid, p)
		assert.Equal(t, uint32(1001), st.Gid, p)
	}
	info, err := os.Stat(filepath.Join(base, rel))
	require.NoError(t, err)
	assert.Equal(t, os.ModeDir|os.ModeSetgid|0o775, info.Mode())
}
