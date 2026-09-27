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
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// --- EnsureLeaf, direct tests ---

// TestEnsureLeaf_NewLeaf_GetsModeAndGoldenACL: a leaf EnsureLeaf itself
// creates gets 0o2775 plus the golden default ACL, and every intermediate it
// creates along the way gets 0o2755.
func TestEnsureLeaf_NewLeaf_GetsModeAndGoldenACL(t *testing.T) {
	hostBase := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "artifacts")

	fd, existed, err := EnsureLeaf(hostBase, rel)
	require.NoError(t, err)
	defer func() { _ = CloseFd(fd) }()
	assert.False(t, existed)

	var st unix.Stat_t
	require.NoError(t, unix.Fstat(fd, &st))
	assert.Equal(t, os.FileMode(0o2775), os.FileMode(st.Mode&0o7777), "leaf mode")

	for _, rel := range []string{"projects", filepath.Join("projects", "pid-1"), filepath.Join("projects", "pid-1", "shared-dirs")} {
		fi, err := os.Stat(filepath.Join(hostBase, rel))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755)|os.ModeSetgid|os.ModeDir, fi.Mode(), rel)
	}

	if !aclSupported(t, hostBase, fd) {
		return
	}
	buf := make([]byte, 256)
	n, xerr := unix.Fgetxattr(fd, xattrPosixACLDefault, buf)
	require.NoError(t, xerr)
	assert.Equal(t, encodeMinimalPosixACL(7, 7, 5), buf[:n], "golden default ACL")
}

// TestEnsureLeaf_PreExistingLeaf_Untouched: a leaf that already exists is
// reported as such (existed=true) and its
// mode is left exactly as it was found -- EnsureLeaf never re-chmods or
// re-ACLs a leaf it did not itself create.
func TestEnsureLeaf_PreExistingLeaf_Untouched(t *testing.T) {
	hostBase := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "artifacts")
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, rel), 0o700))

	fd, existed, err := EnsureLeaf(hostBase, rel)
	require.NoError(t, err)
	defer func() { _ = CloseFd(fd) }()
	assert.True(t, existed)

	var st unix.Stat_t
	require.NoError(t, unix.Fstat(fd, &st))
	assert.Equal(t, os.FileMode(0o700), os.FileMode(st.Mode&0o7777), "a pre-existing leaf's mode must be untouched")
}

// TestEnsureLeaf_SymlinkedIntermediate_Refused: a symlinked intermediate
// component is refused, and nothing past it is created.
func TestEnsureLeaf_SymlinkedIntermediate_Refused(t *testing.T) {
	hostBase := t.TempDir()
	victim := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))
	require.NoError(t, os.Symlink(victim, filepath.Join(hostBase, "projects", "pid-1")))

	_, _, err := EnsureLeaf(hostBase, filepath.Join("projects", "pid-1", "shared-dirs", "artifacts"))
	require.Error(t, err)

	entries, rdErr := os.ReadDir(victim)
	require.NoError(t, rdErr)
	assert.Empty(t, entries, "nothing must be created through the symlinked intermediate")
}

// TestEnsureLeaf_ChmodFailure_RemovesLeaf_KeepsIntermediates_RetrySucceeds:
// EnsureLeaf's rollback for the chmod step: an injected EINVAL/EPERM removes
// the leaf it just created (never the intermediates above it), and a retry
// with the real implementation restored succeeds cleanly.
func TestEnsureLeaf_ChmodFailure_RemovesLeaf_KeepsIntermediates_RetrySucceeds(t *testing.T) {
	hostBase := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "artifacts")

	old := chmodLeaf
	chmodLeaf = func(fd int, mode uint32) error { return unix.EINVAL }
	t.Cleanup(func() { chmodLeaf = old })

	_, _, err := EnsureLeaf(hostBase, rel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chmod shared dir")

	_, statErr := os.Stat(filepath.Join(hostBase, rel))
	assert.True(t, os.IsNotExist(statErr), "the leaf must be removed after a chmod failure")
	for _, rel := range []string{"projects", filepath.Join("projects", "pid-1"), filepath.Join("projects", "pid-1", "shared-dirs")} {
		_, statErr := os.Stat(filepath.Join(hostBase, rel))
		assert.NoError(t, statErr, "intermediate %q must survive a leaf finalization failure", rel)
	}

	chmodLeaf = old
	fd, existed, err := EnsureLeaf(hostBase, rel)
	require.NoError(t, err, "a retry after the transient failure is gone must succeed")
	defer func() { _ = CloseFd(fd) }()
	assert.False(t, existed, "the removed leaf must be recreated, not seen as already existing")
}

// TestEnsureLeaf_ACLFailure_RemovesLeaf_KeepsIntermediates_RetrySucceeds:
// EnsureLeaf's rollback for the ACL step, mirroring the chmod-failure test
// above.
func TestEnsureLeaf_ACLFailure_RemovesLeaf_KeepsIntermediates_RetrySucceeds(t *testing.T) {
	hostBase := t.TempDir()
	rel := filepath.Join("projects", "pid-1", "shared-dirs", "artifacts")

	old := setLeafDefaultACL
	setLeafDefaultACL = func(fd int) error { return unix.EPERM }
	t.Cleanup(func() { setLeafDefaultACL = old })

	_, _, err := EnsureLeaf(hostBase, rel)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set default ACL")

	_, statErr := os.Stat(filepath.Join(hostBase, rel))
	assert.True(t, os.IsNotExist(statErr), "the leaf must be removed after an ACL failure")
	for _, rel := range []string{"projects", filepath.Join("projects", "pid-1"), filepath.Join("projects", "pid-1", "shared-dirs")} {
		_, statErr := os.Stat(filepath.Join(hostBase, rel))
		assert.NoError(t, statErr, "intermediate %q must survive a leaf finalization failure", rel)
	}

	setLeafDefaultACL = old
	fd, existed, err := EnsureLeaf(hostBase, rel)
	require.NoError(t, err, "a retry after the transient failure is gone must succeed")
	defer func() { _ = CloseFd(fd) }()
	assert.False(t, existed, "the removed leaf must be recreated, not seen as already existing")
}

// --- warnIfIntermediateUnsafe, captured-slog tests ---

// resetIntermediateUnsafeWarnOnce lets each test case observe its own
// warn-or-not-warn outcome instead of only the first test in the binary to
// call warnIfIntermediateUnsafe (the guard is process-wide by design in
// production, since it exists purely to prevent log spam over the life of
// one broker process).
func resetIntermediateUnsafeWarnOnce(t *testing.T) {
	t.Helper()
	intermediateUnsafeWarnOnce = sync.Once{}
}

func openDirFd(t *testing.T, path string) int {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// TestWarnIfIntermediateUnsafe_GroupOtherWritable_WarnsOnce: a pre-existing
// intermediate that is group/other-writable (0o777) warns
// exactly once and names the offending component, even across repeated
// calls (the sync.Once, not the caller, is what limits it to one).
func TestWarnIfIntermediateUnsafe_GroupOtherWritable_WarnsOnce(t *testing.T) {
	resetIntermediateUnsafeWarnOnce(t)
	records := captureSlog(t)

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o777))
	fd := openDirFd(t, dir)

	warnIfIntermediateUnsafe(fd, "unsafe-comp")
	warnIfIntermediateUnsafe(fd, "unsafe-comp")

	require.Len(t, *records, 1, "must warn exactly once regardless of call count")
	msg := (*records)[0].Message
	assert.Contains(t, msg, "group/other-writable")
	assertRecordHasAttr(t, (*records)[0], "component", "unsafe-comp")
}

// TestWarnIfIntermediateUnsafe_SelfOwnedSafeMode_NoWarn: a pre-existing
// intermediate at 0o755, owned by this process's effective uid, must not
// warn at all.
func TestWarnIfIntermediateUnsafe_SelfOwnedSafeMode_NoWarn(t *testing.T) {
	resetIntermediateUnsafeWarnOnce(t)
	records := captureSlog(t)

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o755))
	fd := openDirFd(t, dir)

	warnIfIntermediateUnsafe(fd, "safe-comp")

	assert.Empty(t, *records, "a safe-mode, self-owned intermediate must not warn")
}

// TestWarnIfIntermediateUnsafe_NotSelfOwned_WarnsEvenWithSafeMode catches a
// regression flipping the operator: warnIfIntermediateUnsafe's condition is
// "mode is safe AND owned by us" -- skip the warning only if BOTH hold. A
// test that only ever varies the mode half (as the two tests above do)
// can't distinguish that AND from an OR, since such a regression still
// passes both: with a safe mode and correct ownership, OR and AND
// agree the warning should be skipped just as much as AND alone. This test
// uses the geteuid seam to make ownership disagree while the mode stays
// safe (0o755), which OR would still skip (mode half is true) but AND must
// not.
func TestWarnIfIntermediateUnsafe_NotSelfOwned_WarnsEvenWithSafeMode(t *testing.T) {
	resetIntermediateUnsafeWarnOnce(t)
	records := captureSlog(t)

	oldEuid := geteuid
	geteuid = func() int { return unix.Geteuid() + 1 }
	t.Cleanup(func() { geteuid = oldEuid })

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o755))
	fd := openDirFd(t, dir)

	warnIfIntermediateUnsafe(fd, "not-self-owned")

	require.Len(t, *records, 1, "a safe mode does not excuse a mismatched owner")
	assertRecordHasAttr(t, (*records)[0], "component", "not-self-owned")
}

// TestEnsureLeaf_PreExistingUnsafeIntermediate_WarnsThroughRealWalk drives
// the warning through EnsureLeaf's own component walk, rather than calling
// warnIfIntermediateUnsafe directly: a pre-existing intermediate at 0o777
// (group/other-writable) must still trigger exactly the same warning when
// EnsureLeaf walks over it on the way to creating the leaf.
func TestEnsureLeaf_PreExistingUnsafeIntermediate_WarnsThroughRealWalk(t *testing.T) {
	resetIntermediateUnsafeWarnOnce(t)
	records := captureSlog(t)

	hostBase := t.TempDir()
	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))
	require.NoError(t, os.Chmod(projectsDir, 0o777))

	fd, existed, err := EnsureLeaf(hostBase, filepath.Join("projects", "pid-1", "shared-dirs", "artifacts"))
	require.NoError(t, err)
	defer func() { _ = CloseFd(fd) }()
	assert.False(t, existed)

	require.Len(t, *records, 1, "the pre-existing unsafe intermediate must produce exactly one warning")
	msg := (*records)[0].Message
	assert.Contains(t, msg, "group/other-writable")
	assertRecordHasAttr(t, (*records)[0], "component", "projects")
}

// assertRecordHasAttr fails the test unless rec carries an attribute with
// the given key whose value, formatted, equals want.
func assertRecordHasAttr(t *testing.T, rec slog.Record, key, want string) {
	t.Helper()
	found := false
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = true
			assert.Equal(t, want, a.Value.String())
			return false
		}
		return true
	})
	assert.True(t, found, "no attr with key %q on record %q", key, rec.Message)
}
