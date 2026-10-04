//go:build linux

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

package homeprep

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// These tests run as the invoking user, who stands in for both the root
// init container and the agent uid: the home's owner and group are the
// test process's own.

func leafOpts(dir string) LeafOptions {
	return LeafOptions{AgentDir: dir, HomeName: HomeDirPrefix + testAgentID, UID: os.Getuid(), GID: os.Getgid()}
}

func modeOf(t *testing.T, p string) uint32 {
	t.Helper()
	var st unix.Stat_t
	require.NoError(t, unix.Lstat(p, &st))
	return st.Mode & 0o7777
}

func TestLeaf_CreateAndNoOp(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Leaf(leafOpts(dir)))
	home := filepath.Join(dir, HomeDirPrefix+testAgentID)
	assert.Equal(t, uint32(homeDirMode), modeOf(t, home))

	// Idempotent: an existing home with the expected owner, group and mode
	// is left alone, whether empty or not.
	require.NoError(t, Leaf(leafOpts(dir)))
	var st unix.Stat_t
	require.NoError(t, unix.Lstat(home, &st), "an existing empty home is kept")
	assert.Equal(t, uint32(homeDirMode), st.Mode&0o7777)
	assert.Equal(t, uint32(unix.S_IFDIR), st.Mode&unix.S_IFMT)
	assert.Equal(t, uint32(os.Getuid()), st.Uid)
	assert.Equal(t, uint32(os.Getgid()), st.Gid)
	require.NoError(t, os.WriteFile(filepath.Join(home, "f"), []byte("x"), 0o644))
	require.NoError(t, Leaf(leafOpts(dir)))
	_, err := os.Stat(filepath.Join(home, "f"))
	require.NoError(t, err)
}

func TestLeaf_MismatchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, HomeDirPrefix+testAgentID)
	require.NoError(t, os.Mkdir(home, 0o755))
	err := Leaf(leafOpts(dir))
	require.Error(t, err)
	var ce *ClassError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ErrClassLeafMatch, ce.Class)
	assert.Contains(t, err.Error(), "mode 0755")
	assert.Equal(t, uint32(0o755), modeOf(t, home), "a mismatched home is not repaired")

	o := leafOpts(dir)
	o.UID = os.Getuid() + 1
	require.NoError(t, unix.Chmod(home, homeDirMode))
	err = Leaf(o)
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ErrClassLeafMatch, ce.Class)
}

func TestLeaf_SymlinkOrFileAtHome(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	// The link target even looks like a correct home: it must still be
	// refused, never adopted.
	require.NoError(t, unix.Chmod(outside, homeDirMode))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, HomeDirPrefix+testAgentID)))
	before := modeOf(t, outside)
	err := Leaf(leafOpts(dir))
	var ce *ClassError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ErrClassLeafMatch, ce.Class)
	assert.Equal(t, before, modeOf(t, outside), "the link target is untouched")

	dir2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir2, HomeDirPrefix+testAgentID), nil, 0o644))
	require.ErrorAs(t, Leaf(leafOpts(dir2)), &ce)
	assert.Equal(t, ErrClassLeafMatch, ce.Class)
}

// An export that maps root to another user refuses the chown: the start
// fails with the export hint.
func TestLeaf_SquashedExport(t *testing.T) {
	old := fchownFn
	fchownFn = func(fd, uid, gid int) error { return unix.EPERM }
	t.Cleanup(func() { fchownFn = old })
	err := Leaf(leafOpts(t.TempDir()))
	var ce *ClassError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ErrClassLeafFailed, ce.Class)
	assert.Contains(t, err.Error(), "broker host mount")
}

// A failure after the leaf step's own mkdir removes the empty directory,
// so the next start creates it again.
func TestLeaf_FailureRemovesItsOwnDirectory(t *testing.T) {
	old := fchownFn
	fchownFn = func(fd, uid, gid int) error { return unix.EPERM }
	t.Cleanup(func() { fchownFn = old })
	dir := t.TempDir()
	require.Error(t, Leaf(leafOpts(dir)))
	_, err := os.Lstat(filepath.Join(dir, HomeDirPrefix+testAgentID))
	assert.True(t, os.IsNotExist(err), "the directory this call created is removed")

	fchownFn = old
	require.NoError(t, Leaf(leafOpts(dir)), "a later start succeeds")
}

// Step 0: a kubelet-created (root-owned) agent directory is normalised; an
// agent directory owned by anyone else is left alone.
func TestLeaf_AgentDirNormalisation(t *testing.T) {
	old := kubeletUID
	t.Cleanup(func() { kubeletUID = old })

	kubeletUID = uint32(os.Getuid())
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o755))
	require.NoError(t, Leaf(leafOpts(dir)))
	assert.Equal(t, uint32(agentDirMode), modeOf(t, dir))

	kubeletUID = uint32(os.Getuid()) + 1
	dir2 := t.TempDir()
	require.NoError(t, os.Chmod(dir2, 0o750))
	require.NoError(t, Leaf(leafOpts(dir2)))
	assert.Equal(t, uint32(0o750), modeOf(t, dir2), "an agent directory not created by the kubelet is left alone")
}

func TestLeaf_InvalidInput(t *testing.T) {
	dir := t.TempDir()
	for _, o := range []LeafOptions{
		{AgentDir: dir, HomeName: "", UID: 1, GID: 1},
		{AgentDir: dir, HomeName: "../x", UID: 1, GID: 1},
		{AgentDir: dir, HomeName: "a/b", UID: 1, GID: 1},
		{AgentDir: dir, HomeName: "h", UID: 0, GID: 1},
		{AgentDir: dir, HomeName: "h", UID: 1, GID: 0},
	} {
		assert.Error(t, Leaf(o), "%+v", o)
	}
	outside := t.TempDir()
	link := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.Symlink(outside, link))
	assert.Error(t, Leaf(leafOpts(link)), "a symbolic link as the agent directory is refused")
}

func TestStripACLs_NoACLIsFine(t *testing.T) {
	fd, err := unix.Open(t.TempDir(), unix.O_DIRECTORY|unix.O_RDONLY, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(fd) }()
	require.NoError(t, stripACLs(fd))
}
