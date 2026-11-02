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

//go:build !no_sqlite

package hub

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gitConfigContent = "[core]\n\tbare = false\n"

// plantGitWorkspace gives workspacePath a .git directory with a config and a
// hooks directory, and returns the config path.
func plantGitWorkspace(t *testing.T, workspacePath string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, ".git", "hooks"), 0o755))
	cfg := filepath.Join(workspacePath, ".git", "config")
	require.NoError(t, os.WriteFile(cfg, []byte(gitConfigContent), 0o644))
	return cfg
}

func assertGitConfigUnchanged(t *testing.T, cfg string) {
	t.Helper()
	got, err := os.ReadFile(cfg)
	require.NoError(t, err)
	assert.Equal(t, gitConfigContent, string(got), ".git/config was changed")
}

// TestProjectWorkspaceWrite_SymlinkIntoDotGitRefused covers a write or an
// upload that reaches .git through a symlink stored in the workspace.
func TestProjectWorkspaceWrite_SymlinkIntoDotGitRefused(t *testing.T) {
	cases := []struct {
		name   string
		links  map[string]string // link path (relative) -> target
		path   string            // request path
		gone   string            // workspace-relative path that must not exist afterwards
		reason string            // expected refusal reason in the body, "" = not checked
	}{
		{
			name:  "dir_link_to_dotgit",
			links: map[string]string{"gitlink": ".git"},
			path:  "gitlink/config",
		},
		{
			name:  "dir_link_to_dotgit_new_file",
			links: map[string]string{"gitlink": ".git"},
			path:  "gitlink/hooks/pre-commit",
			gone:  ".git/hooks/pre-commit",
		},
		{
			name:  "nested_dir_link_to_hooks",
			links: map[string]string{"sub/hooks": "../.git/hooks"},
			path:  "sub/hooks/pre-commit",
			gone:  ".git/hooks/pre-commit",
		},
		{
			name:  "top_dir_link_to_hooks",
			links: map[string]string{"hookslink": ".git/hooks"},
			path:  "hookslink/post-checkout",
			gone:  ".git/hooks/post-checkout",
		},
		{
			name:   "final_file_link_to_config",
			links:  map[string]string{"cfg": ".git/config"},
			path:   "cfg",
			reason: "target is not a regular file",
		},
		{
			name:   "final_dangling_link_into_dotgit",
			links:  map[string]string{"newhook": ".git/hooks/pre-push"},
			path:   "newhook",
			gone:   ".git/hooks/pre-push",
			reason: "target is not a regular file",
		},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodPut, http.MethodPost} {
			t.Run(tc.name+"_"+method, func(t *testing.T) {
				srv, _ := testServer(t)
				project, workspacePath := createTestHubManagedProject(t, srv, "WS Symlink DotGit")
				cfg := plantGitWorkspace(t, workspacePath)
				for link, target := range tc.links {
					abs := filepath.Join(workspacePath, filepath.FromSlash(link))
					require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
					require.NoError(t, os.Symlink(target, abs))
				}

				filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
				var code int
				var body string
				if method == http.MethodPut {
					rec := doRequest(t, srv, http.MethodPut, filesURL+"/"+tc.path,
						ProjectWorkspaceWriteRequest{Content: "planted"})
					code, body = rec.Code, rec.Body.String()
				} else {
					rec := doMultipartRequest(t, srv, http.MethodPost, filesURL,
						map[string][]byte{tc.path: []byte("planted")})
					code, body = rec.Code, rec.Body.String()
				}
				assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
				assertGitConfigUnchanged(t, cfg)
				if tc.reason != "" {
					assert.Contains(t, body, tc.reason, "refused for another reason than the symlink target")
				}
				if tc.gone != "" {
					assertNoEntry(t, workspacePath, tc.gone)
				}
				for link := range tc.links {
					fi, err := os.Lstat(filepath.Join(workspacePath, filepath.FromSlash(link)))
					require.NoError(t, err)
					assert.NotZero(t, fi.Mode()&os.ModeSymlink, "%s is no longer a symlink", link)
				}
			})
		}
	}
}

// TestProjectWorkspaceWrite_DotGitAliasRefused covers a directory that is the
// workspace's .git without a .git name on the way: .git is a symlink to an
// in-tree directory, and the request names that directory. Only the
// comparison of the opened handle with .git catches it.
func TestProjectWorkspaceWrite_DotGitAliasRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS DotGit Alias")
	gitReal := filepath.Join(workspacePath, "gitdir-real")
	require.NoError(t, os.MkdirAll(filepath.Join(gitReal, "hooks"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitReal, "config"), []byte(gitConfigContent), 0o644))
	require.NoError(t, os.Symlink("gitdir-real", filepath.Join(workspacePath, ".git")))

	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
	rec := doRequest(t, srv, http.MethodPut, filesURL+"/gitdir-real/config",
		ProjectWorkspaceWriteRequest{Content: "planted"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "write through the .git alias, body: %s", rec.Body.String())
	assertGitConfigUnchanged(t, filepath.Join(gitReal, "config"))

	rec = doMultipartRequest(t, srv, http.MethodPost, filesURL,
		map[string][]byte{"gitdir-real/hooks/pre-commit": []byte("planted")})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "upload through the .git alias, body: %s", rec.Body.String())
	assertNoEntry(t, workspacePath, "gitdir-real/hooks/pre-commit")
}

// TestProjectWorkspaceWrite_SymlinkToNormalDirAllowed is the control: a write
// or an upload beneath a symlink to an ordinary in-tree directory still lands
// in that directory.
func TestProjectWorkspaceWrite_SymlinkToNormalDirAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Symlink Control")
	plantGitWorkspace(t, workspacePath)
	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "docs", "real"), 0o755))
	require.NoError(t, os.Symlink("docs/real", filepath.Join(workspacePath, "linked")))
	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "sub"), 0o755))
	require.NoError(t, os.Symlink("../docs/real", filepath.Join(workspacePath, "sub", "up")))

	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
	rec := doRequest(t, srv, http.MethodPut, filesURL+"/linked/new/put.txt",
		ProjectWorkspaceWriteRequest{Content: "put"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rec = doMultipartRequest(t, srv, http.MethodPost, filesURL,
		map[string][]byte{"sub/up/post.txt": []byte("post")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	got, err := os.ReadFile(filepath.Join(workspacePath, "docs", "real", "new", "put.txt"))
	require.NoError(t, err)
	assert.Equal(t, "put", string(got))
	got, err = os.ReadFile(filepath.Join(workspacePath, "docs", "real", "post.txt"))
	require.NoError(t, err)
	assert.Equal(t, "post", string(got))
}

// TestProjectWorkspaceDelete_SymlinkToDotGitRemovesLinkOnly confirms DELETE
// of a symlink into .git removes the link and leaves .git alone.
func TestProjectWorkspaceDelete_SymlinkToDotGitRemovesLinkOnly(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete Symlink DotGit")
	cfg := plantGitWorkspace(t, workspacePath)
	require.NoError(t, os.Symlink(".git", filepath.Join(workspacePath, "gitlink")))
	require.NoError(t, os.Symlink(".git/config", filepath.Join(workspacePath, "cfg")))

	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)
	for _, link := range []string{"gitlink", "cfg"} {
		rec := doRequest(t, srv, http.MethodDelete, filesURL+"/"+link, nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
		assertNoEntry(t, workspacePath, link)
	}
	assertGitConfigUnchanged(t, cfg)
	fi, err := os.Lstat(filepath.Join(workspacePath, ".git", "hooks"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), ".git/hooks was removed")
}

// TestProjectWorkspaceDelete_ThroughLinkIntoDotGitRefused covers a DELETE
// that reaches .git through a directory symlink on the way, or through the
// .git alias: each is refused and .git keeps its content.
func TestProjectWorkspaceDelete_ThroughLinkIntoDotGitRefused(t *testing.T) {
	cases := []struct {
		name  string
		alias bool   // .git is a symlink to gitdir-real
		path  string // request path
		keep  string // workspace-relative file that must survive
	}{
		{name: "dir_link_config", path: "g/config", keep: ".git/config"},
		{name: "dir_link_hook", path: "g/hooks/pre-commit", keep: ".git/hooks/pre-commit"},
		{name: "alias_config", alias: true, path: "gitdir-real/config", keep: "gitdir-real/config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete Through Link")
			gitDir := ".git"
			if tc.alias {
				gitDir = "gitdir-real"
				require.NoError(t, os.Symlink("gitdir-real", filepath.Join(workspacePath, ".git")))
			}
			require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, gitDir, "hooks"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(workspacePath, gitDir, "config"), []byte(gitConfigContent), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(workspacePath, gitDir, "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755))
			if !tc.alias {
				require.NoError(t, os.Symlink(".git", filepath.Join(workspacePath, "g")))
			}

			rec := doRequest(t, srv, http.MethodDelete,
				fmt.Sprintf("/api/v1/projects/%s/workspace/files/%s", project.ID, tc.path), nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "delete through a link into .git, body: %s", rec.Body.String())
			_, err := os.Stat(filepath.Join(workspacePath, filepath.FromSlash(tc.keep)))
			assert.NoError(t, err, "the delete removed %s", tc.keep)
		})
	}
}

// TestProjectWorkspaceDelete_UnderNormalDirLinkAllowed is the control: a
// DELETE beneath a symlink to an ordinary in-tree directory removes the file
// in that directory.
func TestProjectWorkspaceDelete_UnderNormalDirLinkAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete Control")
	plantGitWorkspace(t, workspacePath)
	realDir := filepath.Join(workspacePath, "docs", "real")
	require.NoError(t, os.MkdirAll(realDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "gone.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "stays.txt"), []byte("y"), 0o644))
	require.NoError(t, os.Symlink("docs/real", filepath.Join(workspacePath, "linked")))

	rec := doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files/linked/gone.txt", project.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	assertNoEntry(t, workspacePath, "docs/real/gone.txt")
	_, err := os.Stat(filepath.Join(realDir, "stays.txt"))
	assert.NoError(t, err)
}

// TestProjectWorkspaceDelete_TrailingSlash pins the answers to a DELETE path
// with a trailing slash: an existing directory or file is refused and kept,
// and a missing name is not found.
func TestProjectWorkspaceDelete_TrailingSlash(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete Trailing Slash")
	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "d"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "f"), []byte("f"), 0o644))
	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)

	for _, tc := range []struct {
		name string
		path string
		want int
		text string // expected in the body; never a .git claim
		keep string
	}{
		{name: "empty_dir", path: "d/", want: http.StatusBadRequest, text: "File not accessible", keep: "d"},
		{name: "file", path: "f/", want: http.StatusBadRequest, text: "path is not accessible", keep: "f"},
		{name: "missing", path: "x/", want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodDelete, filesURL+"/"+tc.path, nil)
			assert.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			if tc.text != "" {
				assert.Contains(t, rec.Body.String(), tc.text)
			}
			assert.NotContains(t, rec.Body.String(), ".git")
			if tc.keep != "" {
				_, err := os.Lstat(filepath.Join(workspacePath, tc.keep))
				assert.NoError(t, err, "%s was removed", tc.keep)
			}
		})
	}
}

// TestProjectWorkspaceWrite_LinkIntoNestedDotGitRefused covers a nested
// repository's .git directory reached through a symlink: only the name check
// of the walk catches it (it is not the workspace root's .git).
func TestProjectWorkspaceWrite_LinkIntoNestedDotGitRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write Nested DotGit")
	plantGitWorkspace(t, workspacePath)
	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "sub", ".git", "hooks"), 0o755))
	require.NoError(t, os.Symlink("sub/.git", filepath.Join(workspacePath, "l")))
	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)

	rec := doRequest(t, srv, http.MethodPut, filesURL+"/l/hooks/pre-commit",
		ProjectWorkspaceWriteRequest{Content: "planted"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "write through a link into a nested .git, body: %s", rec.Body.String())
	rec = doMultipartRequest(t, srv, http.MethodPost, filesURL,
		map[string][]byte{"l/hooks/post-checkout": []byte("planted")})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "upload through a link into a nested .git, body: %s", rec.Body.String())
	assertNoEntry(t, workspacePath, "sub/.git/hooks/pre-commit")
	assertNoEntry(t, workspacePath, "sub/.git/hooks/post-checkout")
}

// TestOpenWorkspaceTarget_DanglingLinkCreatesNothing drives the open with a
// dangling link already in place, as if it had been swapped in after the
// Lstat: whether Lstat had found the name absent (O_EXCL) or present (no
// O_CREATE), the open fails and nothing is created at the link's target.
func TestOpenWorkspaceTarget_DanglingLinkCreatesNothing(t *testing.T) {
	for name, exists := range map[string]bool{"lstat_absent": false, "lstat_present": true} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
			require.NoError(t, os.Symlink(".git/index.lock", filepath.Join(dir, "swapped")))
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()

			f, err := openWorkspaceTarget(root, "swapped", exists)
			if f != nil {
				_ = f.Close()
			}
			assert.Error(t, err, "the open through a swapped-in dangling link succeeded")
			_, statErr := os.Lstat(filepath.Join(dir, ".git", "index.lock"))
			assert.True(t, os.IsNotExist(statErr), "the open created the link target (err %v)", statErr)
		})
	}
}
