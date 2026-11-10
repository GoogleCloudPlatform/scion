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
	calls := &[]chownCall{}
	restore := SetOwnerHooksForTest(func() int { return euid }, func(fd, uid, gid int) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		*calls = append(*calls, chownCall{ino: uint64(st.Ino), uid: uid, gid: gid})
		return nil
	})
	t.Cleanup(restore)
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

// A root creator with an owner gives the LEAF it creates that owner, on the
// walk's descriptor; the intermediates it creates are not given the owner,
// and the leaf keeps mode 2775.
func TestEnsureLeafOwned_RootCreatorChownsCreatedLeafOnly(t *testing.T) {
	base := t.TempDir()
	calls := recordChowns(t, 0)
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad")

	fd, existed, err := EnsureLeafOwned(base, rel, testOwner)
	require.NoError(t, err)
	t.Cleanup(func() { _ = CloseFd(fd) })
	assert.False(t, existed)

	assert.Equal(t, []chownCall{{ino: inodeOf(t, filepath.Join(base, rel)), uid: 1000, gid: 1001}}, *calls,
		"only the created leaf gets the owner; created intermediates are not nfs-owned")

	info, err := os.Stat(filepath.Join(base, rel))
	require.NoError(t, err)
	assert.Equal(t, os.ModeDir|os.ModeSetgid|0o775, info.Mode(), "the leaf keeps setgid 2775")
	for _, p := range []string{"projects", "projects/pid-1", "projects/pid-1/shared-dirs"} {
		info, err := os.Stat(filepath.Join(base, p))
		require.NoError(t, err)
		assert.Equal(t, os.ModeDir|os.ModeSetgid|0o755, info.Mode(), "intermediate %s keeps 2755", p)
	}
}

// Directories that already existed are never changed.
func TestEnsureLeafOwned_PreExistingLeafUnchanged(t *testing.T) {
	base := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(filepath.Join(base, rel), 0o755))
	calls := recordChowns(t, 0)

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
// chmod does, and keeps the intermediates.
func TestEnsureLeafOwned_ChownFailureRemovesLeaf(t *testing.T) {
	base := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "a")
	n := 0
	t.Cleanup(SetOwnerHooksForTest(func() int { return 0 }, func(fd, uid, gid int) error {
		n++
		return unix.EPERM
	}))

	_, _, err := EnsureLeafOwned(base, rel, testOwner)
	require.Error(t, err)
	assert.True(t, errors.Is(err, unix.EPERM), "err = %v", err)
	assert.Equal(t, 1, n, "only the leaf is chowned")
	assert.NoDirExists(t, filepath.Join(base, rel), "the leaf this call created is removed")
	assert.DirExists(t, filepath.Join(base, "projects", "pid-1", "shared-dirs"), "intermediates are kept")
}

// When a concurrent creator wins the mkdirat race for the leaf, this call
// did not create it and never chowns it.
func TestEnsureLeafOwned_ConcurrentCreatorWinsNotChowned(t *testing.T) {
	base := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "raced")
	require.NoError(t, os.MkdirAll(filepath.Join(base, filepath.Dir(rel)), 0o755))
	calls := recordChowns(t, 0)
	oldMkdirat := mkdirat
	t.Cleanup(func() { mkdirat = oldMkdirat })
	mkdirat = func(dirfd int, path string, mode uint32) error {
		if path == "raced" {
			// The other creator's mkdirat lands first; ours then sees EEXIST.
			if err := oldMkdirat(dirfd, path, mode); err != nil {
				return err
			}
			return unix.EEXIST
		}
		return oldMkdirat(dirfd, path, mode)
	}

	fd, existed, err := EnsureLeafOwned(base, rel, testOwner)
	require.NoError(t, err)
	require.NoError(t, CloseFd(fd))
	assert.False(t, existed, "the first open saw no directory")
	assert.Empty(t, *calls, "a directory a concurrent creator made is not chowned")
}

// An existing intermediate without other-execute is left unchanged (it is
// warned about, never chmod'ed).
func TestEnsureLeafOwned_UntraversableIntermediateUnchanged(t *testing.T) {
	base := t.TempDir()
	mid := filepath.Join(base, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(mid, 0o755))
	require.NoError(t, os.Chmod(mid, 0o750))
	recordChowns(t, 0)

	fd, _, err := EnsureLeafOwned(base, filepath.Join("projects", "pid-1", "shared-dirs", "a"), testOwner)
	require.NoError(t, err)
	require.NoError(t, CloseFd(fd))
	info, err := os.Stat(mid)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm(), "the existing intermediate is not chmod'ed")
}

// With real root (no recorder), the created leaf really gets the owner's
// uid and gid with setgid 2775, and the created intermediates keep root's.
// It needs root: CI runs unprivileged and skips it; the recorder tests
// above cover the rule there. Run it as root on a Linux host (for example:
// sudo go test -run TestEnsureLeafOwned_RealRoot ./pkg/shareddirs/).
func TestEnsureLeafOwned_RealRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (chown to another uid); covered without root by TestEnsureLeafOwned_RootCreatorChownsCreatedLeafOnly")
	}
	base := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "scratchpad")
	fd, _, err := EnsureLeafOwned(base, rel, testOwner)
	require.NoError(t, err)
	require.NoError(t, CloseFd(fd))
	var st unix.Stat_t
	require.NoError(t, unix.Lstat(filepath.Join(base, rel), &st))
	assert.Equal(t, uint32(1000), st.Uid)
	assert.Equal(t, uint32(1001), st.Gid)
	for _, p := range []string{"projects", "projects/pid-1", "projects/pid-1/shared-dirs"} {
		require.NoError(t, unix.Lstat(filepath.Join(base, p), &st))
		assert.Equal(t, uint32(0), st.Uid, "intermediate %s keeps root's owner", p)
	}
	info, err := os.Stat(filepath.Join(base, rel))
	require.NoError(t, err)
	assert.Equal(t, os.ModeDir|os.ModeSetgid|0o775, info.Mode())
}
