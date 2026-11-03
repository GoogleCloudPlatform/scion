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

// keepFile writes dir/keep.txt (creating dir) and returns its path.
func keepFile(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, "keep.txt")
	require.NoError(t, os.WriteFile(p, []byte("keep"), 0o644))
	return p
}

// A subpath_root whose intermediate component is a symlink (here to another
// directory on the same share) is refused, and nothing is removed on either
// side of the link.
func TestDeleteProjectRoot_SymlinkedSubPathRootComponentRefused(t *testing.T) {
	hostBase := t.TempDir()
	behind := keepFile(t, filepath.Join(hostBase, "elsewhere", "projects", "pid-1"))
	require.NoError(t, os.Symlink(filepath.Join(hostBase, "elsewhere"), filepath.Join(hostBase, "tenants")))

	err := DeleteProjectRoot(hostBase, "tenants/projects", "pid-1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnconfinedPath), "err = %v", err)
	assert.FileExists(t, behind, "the tree behind the symlinked component is untouched")
	fi, err := os.Lstat(filepath.Join(hostBase, "tenants"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the link is left in place")
}

// A <projectID> entry that is a symlink to a directory with content is
// refused: the link is kept and the directory it names is untouched.
func TestDeleteProjectRoot_SymlinkedProjectDirRefused(t *testing.T) {
	hostBase := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))
	victim := t.TempDir()
	kept := keepFile(t, filepath.Join(victim, "workspace"))
	link := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.Symlink(victim, link))

	err := DeleteProjectRoot(hostBase, "projects", "pid-1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnconfinedPath), "err = %v", err)
	assert.FileExists(t, kept, "the link target's content is untouched")
	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, victim, target, "the link is left in place")
}

// A subpath_root with a "." or ".." component passed directly (not through
// the config validator) is refused before anything is opened, and nothing
// is removed: neither a tree outside the host base nor the canonical tree
// the "." form would name.
func TestDeleteProjectRoot_DotComponentsRefused(t *testing.T) {
	parent := t.TempDir()
	hostBase := filepath.Join(parent, "base")
	canonical := keepFile(t, filepath.Join(hostBase, "projects", "pid-1"))
	outside := keepFile(t, filepath.Join(parent, "outside", "pid-1"))

	for _, sub := range []string{"../outside", "projects/../../outside", "projects/.", "./projects", "."} {
		t.Run(sub, func(t *testing.T) {
			err := DeleteProjectRoot(hostBase, sub, "pid-1")
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrUnconfinedPath), "err = %v", err)
			assert.FileExists(t, outside, "a tree outside the host base is untouched")
			assert.FileExists(t, canonical, "the canonical tree is untouched")
		})
	}
}

// A symlink inside the project dir that points outside the share is
// unlinked, never followed: the project dir goes, and the link targets (a
// directory and a file) survive with their content.
func TestDeleteProjectRoot_InnerSymlinkUnlinkedTargetKept(t *testing.T) {
	hostBase := t.TempDir()
	root := filepath.Join(hostBase, "projects", "pid-1")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workspace"), 0o755))
	outsideDir := t.TempDir()
	outsideKeep := keepFile(t, outsideDir)
	outsideFile := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("file"), 0o644))
	require.NoError(t, os.Symlink(outsideDir, filepath.Join(root, "dir-link")))
	require.NoError(t, os.Symlink(outsideFile, filepath.Join(root, "workspace", "file-link")))

	require.NoError(t, DeleteProjectRoot(hostBase, "projects", "pid-1"))

	_, err := os.Lstat(root)
	assert.True(t, os.IsNotExist(err), "the project dir is removed (lstat: %v)", err)
	assert.FileExists(t, outsideKeep, "the directory link's target keeps its content")
	got, err := os.ReadFile(outsideFile)
	require.NoError(t, err, "the file link's target survives")
	assert.Equal(t, "file", string(got))
}

// The single-component open refuses "." and ".." itself, without relying on
// its callers' checks: neither opens a directory.
func TestOpenExistingDirNoFollow_RefusesDotComponents(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	require.NoError(t, os.MkdirAll(base, 0o755))
	baseFd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(baseFd) })

	for _, comp := range []string{".", ".."} {
		fd, existed, err := openExistingDirNoFollow(baseFd, comp)
		if err == nil && fd >= 0 {
			_ = unix.Close(fd)
		}
		require.Error(t, err, "comp %q", comp)
		assert.True(t, errors.Is(err, ErrUnconfinedPath), "comp %q: err = %v", comp, err)
		assert.False(t, existed, "comp %q", comp)
		assert.Equal(t, -1, fd, "comp %q", comp)
	}
}
