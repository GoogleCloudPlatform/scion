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

package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for copyDirExcludingGit and checkRegularTree; shared fixtures are in
// remote_templates_nofollow_test.go.

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

// plantLinkedRepo builds parent/repo with a template at templates/foo that
// contains regular files, a .git dir, and absolute and relative file and
// directory links (to the outside dir and inside the tree).
func plantLinkedRepo(t *testing.T, parent, outside string) string {
	t.Helper()
	repo := filepath.Join(parent, "repo")
	tpl := filepath.Join(repo, "templates", "foo")
	writeTestFile(t, filepath.Join(repo, "README.md"), "# repo\n", 0o644)
	writeTestFile(t, filepath.Join(tpl, "scion-agent.yaml"), "schema_version: \"1\"\n", 0o644)
	writeTestFile(t, filepath.Join(tpl, "home", ".bashrc"), "export FOO=1\n", 0o644)
	writeTestFile(t, filepath.Join(tpl, "bin", "run.sh"), "#!/bin/sh\n", 0o755)
	writeTestFile(t, filepath.Join(tpl, ".git", "config"), "[core]\n", 0o644)
	writeTestFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)

	links := map[string]string{
		"abs-file":      filepath.Join(outside, "secret.txt"),
		"abs-dir":       outside,
		"rel-file":      "../../../outside/secret.txt",
		"rel-dir":       "../../../outside",
		"inside-file":   "scion-agent.yaml",
		"inside-dir":    "home",
		"home/rel-deep": "../../../../outside/secret.txt",
	}
	for name, target := range links {
		require.NoError(t, os.Symlink(target, filepath.Join(tpl, name)))
	}
	// A top-level link to the outside directory, reachable with an empty
	// sub-path.
	require.NoError(t, os.Symlink(outside, filepath.Join(repo, "top-abs-dir")))
	require.NoError(t, os.Symlink("../outside/secret.txt", filepath.Join(repo, "top-rel-file")))
	return repo
}

var linkedRepoTemplateFiles = map[string]string{
	"scion-agent.yaml": "schema_version: \"1\"\n",
	"home/.bashrc":     "export FOO=1\n",
	"bin/run.sh":       "#!/bin/sh\n",
}

func TestCopyDirExcludingGit_NeverFollowsLinks(t *testing.T) {
	skipWithoutSymlinks(t)
	parent, outside := outsideFixture(t)
	repo := plantLinkedRepo(t, parent, outside)

	t.Run("sub-path", func(t *testing.T) {
		dst := filepath.Join(parent, "dst-sub")
		require.NoError(t, os.MkdirAll(dst, 0o755))
		require.NoError(t, copyDirExcludingGit(repo, "templates/foo/", dst))
		assert.Equal(t, linkedRepoTemplateFiles, readTree(t, dst))
		assertRegularOnly(t, dst)
		info, err := os.Stat(filepath.Join(dst, "bin", "run.sh"))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode().Perm()&0o100, "executable bit is kept")
		assertOutsideUntouched(t, outside)
	})

	t.Run("whole repo", func(t *testing.T) {
		dst := filepath.Join(parent, "dst-all")
		require.NoError(t, os.MkdirAll(dst, 0o755))
		require.NoError(t, copyDirExcludingGit(repo, "", dst))
		want := map[string]string{"README.md": "# repo\n"}
		for k, v := range linkedRepoTemplateFiles {
			want["templates/foo/"+k] = v
		}
		assert.Equal(t, want, readTree(t, dst))
		assertRegularOnly(t, dst)
		assertOutsideUntouched(t, outside)
	})
}

func TestCopyDirExcludingGit_RefusesLinkedOrEscapingSubPath(t *testing.T) {
	skipWithoutSymlinks(t)
	parent, outside := outsideFixture(t)
	repo := plantLinkedRepo(t, parent, outside)
	// An in-tree relative directory link and a chain of links.
	require.NoError(t, os.Symlink("templates", filepath.Join(repo, "alias")))
	require.NoError(t, os.Symlink("alias/foo", filepath.Join(repo, "chain")))

	tests := []struct {
		name, sub, wantErr string
	}{
		{"absolute dir link", "top-abs-dir", "symbolic link"},
		{"below absolute dir link", "templates/foo/abs-dir", "symbolic link"},
		{"relative dir link", "templates/foo/rel-dir", "symbolic link"},
		{"in-tree dir link", "alias/foo", "symbolic link"},
		{"chained links", "chain", "symbolic link"},
		{"file link", "top-rel-file", "symbolic link"},
		{"dot-dot", "../outside", "not inside the repository"},
		{"git dir", ".git", "inside a .git directory"},
		{"nested git dir", "templates/foo/.git", "inside a .git directory"},
		{"dot-dot after a dir", "templates/../../outside", "not inside the repository"},
		{"absolute path", outside, "not inside the repository"},
		{"regular file", "README.md", "not a directory"},
		{"missing", "templates/missing", "path templates/missing not found in repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := t.TempDir()
			err := copyDirExcludingGit(repo, tt.sub, dst)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Empty(t, readTree(t, dst), "nothing is copied on error")
			assertOutsideUntouched(t, outside)
		})
	}
}

func TestCheckRegularTree(t *testing.T) {
	skipWithoutSymlinks(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a", "b.txt"), "b\n", 0o644)
	require.NoError(t, checkRegularTree(dir))

	require.NoError(t, os.Symlink("b.txt", filepath.Join(dir, "a", "link")))
	err := checkRegularTree(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a/link")
}

// TestProductionGitSparseCheckoutForTest pins that the accessor returns the
// production sparse-checkout runner, even while a stub is installed.
func TestProductionGitSparseCheckoutForTest(t *testing.T) {
	stub := GitSparseCheckoutFunc(func(context.Context, *GitHubURLParts, string, string) error { return nil })
	t.Cleanup(SetGitSparseCheckoutForTest(stub))
	assert.Equal(t, reflect.ValueOf(execSparseGitCheckout).Pointer(), reflect.ValueOf(ProductionGitSparseCheckoutForTest()).Pointer())
}
