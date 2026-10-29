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

// The confinement cases of the removed runtime.CleanupNFSProject tests that
// delete_project_root_confinement_unix_test.go does not already cover,
// ported to DeleteProjectRoot (the Hub's create-rollback removal; the Hub's
// delete removes through DeleteTree, see projectfs). Together the two files
// cover: the whole subpath_root symlinked, a symlinked mid-path component, a
// symlinked project entry, "." and ".." components, a non-canonical
// subpath_root and a missing tree.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const portProjectID = "proj-port-1"

// portKeep writes dir/workspace/keep.txt (creating dir) and returns its path.
func portKeep(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "workspace"), 0o755))
	p := filepath.Join(dir, "workspace", "keep.txt")
	require.NoError(t, os.WriteFile(p, []byte("keep"), 0o644))
	return p
}

// The WHOLE subpath_root is a symlink to another directory on the same
// share that holds a directory with the same project ID: refused, nothing
// behind the link is removed, and the link is kept.
func TestDeleteProjectRoot_SymlinkedWholeSubPathRootRefused(t *testing.T) {
	base := t.TempDir()
	keep := portKeep(t, filepath.Join(base, "other", portProjectID))
	link := filepath.Join(base, "projects")
	require.NoError(t, os.Symlink(filepath.Join(base, "other"), link))

	err := DeleteProjectRoot(base, "projects", portProjectID)

	require.Error(t, err, "a symlinked subpath_root must be refused")
	assert.FileExists(t, keep, "the directory the symlink points to must be untouched")
	fi, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the symlink itself must be left in place")
}

// A canonical multi-component subpath_root: only the named project goes; a
// sibling project and the subpath_root itself stay.
func TestDeleteProjectRoot_CanonicalMultiComponentRemovesOnlyProject(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tenants", "projects")
	portKeep(t, filepath.Join(root, portProjectID))
	other := portKeep(t, filepath.Join(root, "proj-port-2"))

	require.NoError(t, DeleteProjectRoot(base, "tenants/projects", portProjectID))

	assert.NoDirExists(t, filepath.Join(root, portProjectID))
	assert.FileExists(t, other, "a sibling project must be untouched")
	assert.DirExists(t, root, "the subpath_root itself must be kept")
}

// A non-canonical subpath_root (a leading "/", empty components) is walked
// relative to the host base, never outside it: a tree beside the host base
// that such a path might name is untouched.
func TestDeleteProjectRoot_NonCanonicalSubPathRootStaysUnderBase(t *testing.T) {
	for _, sub := range []string{"/projects", "projects//", "//projects"} {
		t.Run(sub, func(t *testing.T) {
			parent := t.TempDir()
			base := filepath.Join(parent, "base")
			require.NoError(t, os.MkdirAll(filepath.Join(base, "projects"), 0o755))
			beside := portKeep(t, filepath.Join(parent, "projects", portProjectID))

			err := DeleteProjectRoot(base, sub, portProjectID)

			assert.NoError(t, err, "nothing under the base to remove for %q", sub)
			assert.FileExists(t, beside, "a tree beside the host base must be untouched for %q", sub)
		})
	}
}

// A missing tree is nothing to remove: a missing project directory or a
// missing subpath_root returns nil and creates nothing.
func TestDeleteProjectRoot_MissingTreeIsNil(t *testing.T) {
	t.Run("project dir missing", func(t *testing.T) {
		base := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(base, "projects"), 0o755))
		require.NoError(t, DeleteProjectRoot(base, "projects", portProjectID))
		assert.NoDirExists(t, filepath.Join(base, "projects", portProjectID))
	})
	t.Run("subpath_root missing", func(t *testing.T) {
		base := t.TempDir()
		require.NoError(t, DeleteProjectRoot(base, "tenants/projects", portProjectID))
		assert.NoDirExists(t, filepath.Join(base, "tenants"), "nothing may be created")
	})
}
