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

package projectfs

// The confinement cases of the removed runtime.CleanupNFSProject tests,
// ported to the Hub's removal path (Layout.RemoveProjectTree, which removes
// through shareddirs.DeleteTree): the Hub is the one deleter of a shared
// project tree, so these guard the only removal path that remains.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// portKeepTree creates dir/workspace/keep.txt and returns the file's path.
func portKeepTree(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workspace"), 0o755))
	keep := filepath.Join(dir, "workspace", "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))
	return keep
}

// assertRefusedNotRetried checks a single refused, non-retryable root.
func assertRefusedNotRetried(t *testing.T, ds []Deletion, err error) {
	t.Helper()
	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeFailed, ds[0].Outcome)
	assert.ErrorIs(t, ds[0].Err, ErrRefused)
	assert.False(t, Retryable(ds[0]), "a refusal is not retried: retrying cannot help")
}

// The whole subpath root is a symlink to another directory on the same
// share that holds a directory with the same project ID: the removal is
// refused, the tree behind the link is untouched and the link is kept.
func TestRemoveProjectTree_SymlinkedSubPathRootOnShareRefused(t *testing.T) {
	hostBase := t.TempDir()
	keep := portKeepTree(t, filepath.Join(hostBase, "other", testPID))
	link := filepath.Join(hostBase, "projects")
	require.NoError(t, os.Symlink(filepath.Join(hostBase, "other"), link))

	ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)

	assertRefusedNotRetried(t, ds, err)
	assert.FileExists(t, keep, "the directory the symlink points to must be untouched")
	fi, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the symlink itself must be left in place")
}

// A symlink in the middle of a multi-component subpath root is refused the
// same way, even when it points inside the share.
func TestRemoveProjectTree_SymlinkedSubPathRootComponentRefused(t *testing.T) {
	hostBase := t.TempDir()
	keep := portKeepTree(t, filepath.Join(hostBase, "elsewhere", "projects", testPID))
	require.NoError(t, os.Symlink(filepath.Join(hostBase, "elsewhere"), filepath.Join(hostBase, "tenants")))
	l := Layout{Backend: BackendNFS, HostBase: hostBase, SubPathRoot: "tenants/projects"}

	ds, err := l.RemoveProjectTree(testPID)

	assertRefusedNotRetried(t, ds, err)
	assert.FileExists(t, keep, "the directory behind the symlinked component must be untouched")
}

// The project entry itself is a symlink: refused; the link is kept and the
// directory it names keeps its content.
func TestRemoveProjectTree_SymlinkedProjectEntryRefused(t *testing.T) {
	hostBase := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))
	keep := portKeepTree(t, filepath.Join(hostBase, "projects", testOtherPID))
	link := filepath.Join(hostBase, "projects", testPID)
	require.NoError(t, os.Symlink(filepath.Join(hostBase, "projects", testOtherPID), link))

	ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)

	assertRefusedNotRetried(t, ds, err)
	assert.FileExists(t, keep)
	fi, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the symlink itself must be left in place")
}

// The canonical layout with a multi-component subpath root is still
// removed, and only the named project: a sibling project and the subpath
// root stay.
func TestRemoveProjectTree_CanonicalMultiComponentSubPathRootRemoved(t *testing.T) {
	hostBase := t.TempDir()
	root := filepath.Join(hostBase, "tenants", "projects")
	portKeepTree(t, filepath.Join(root, testPID))
	other := portKeepTree(t, filepath.Join(root, testOtherPID))
	l := Layout{Backend: BackendNFS, HostBase: hostBase, SubPathRoot: "tenants/projects"}

	ds, err := l.RemoveProjectTree(testPID)

	require.NoError(t, err)
	require.Len(t, ds, 1)
	assert.Equal(t, OutcomeDeleted, ds[0].Outcome)
	assert.NoDirExists(t, filepath.Join(root, testPID))
	assert.FileExists(t, other, "a sibling project must be untouched")
	assert.DirExists(t, root, "the subpath root itself must be kept")
}

// A subpath root with a "." or ".." component, or a non-canonical form (an
// absolute path, an empty component), is refused before anything is
// opened: nothing is removed, neither the canonical tree such a form might
// name nor a tree beside the host base.
func TestRemoveProjectTree_DotAndNonCanonicalSubPathRootRefused(t *testing.T) {
	for _, sub := range []string{"..", "projects/..", "./projects", "tenants/../projects", "/projects", "projects//x", "projects/"} {
		t.Run(sub, func(t *testing.T) {
			parent := t.TempDir()
			hostBase := filepath.Join(parent, "base")
			canonical := portKeepTree(t, filepath.Join(hostBase, "projects", testPID))
			beside := portKeepTree(t, filepath.Join(parent, testPID))
			l := Layout{Backend: BackendNFS, HostBase: hostBase, SubPathRoot: sub}

			ds, err := l.RemoveProjectTree(testPID)

			assert.Error(t, err, "subpath_root %q must be refused", sub)
			assert.Empty(t, ds, "nothing may be attempted for subpath_root %q", sub)
			assert.FileExists(t, canonical)
			assert.FileExists(t, beside)
		})
	}
}

// A host base that is not absolute (a non-canonical layout) is refused as
// an unusable share, and nothing is removed.
func TestRemoveProjectTree_RelativeHostBaseRefused(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	keep := portKeepTree(t, filepath.Join(cwd, "rel", "projects", testPID))
	l := Layout{Backend: BackendNFS, HostBase: "rel", SubPathRoot: "projects"}

	ds, err := l.RemoveProjectTree(testPID)

	assertRefusedNotRetried(t, ds, err)
	assert.FileExists(t, keep)
}

// A missing project tree is OutcomeAbsent (idempotent, nothing to remove).
// A missing subpath root is the share-not-mounted case: a refused failure
// (logged; the Hub's health reports the tree as degraded), never a removal.
func TestRemoveProjectTree_MissingTree(t *testing.T) {
	t.Run("project tree missing", func(t *testing.T) {
		hostBase := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))

		ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)

		require.NoError(t, err)
		require.Len(t, ds, 1)
		assert.Equal(t, OutcomeAbsent, ds[0].Outcome)
		assert.NoError(t, ds[0].Err)
	})
	t.Run("subpath root missing", func(t *testing.T) {
		hostBase := t.TempDir()

		ds, err := delNFSLayout(hostBase).RemoveProjectTree(testPID)

		assertRefusedNotRetried(t, ds, err)
		assert.ErrorIs(t, ds[0].Err, ErrNoProjectTree)
	})
}
