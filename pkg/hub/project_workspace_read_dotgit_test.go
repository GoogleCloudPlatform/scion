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
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dotGitReadSecret = "Zq7mWx2Pk-not-a-real-token"

// plantDotGitReadTree gives workspacePath a .git directory whose config
// carries a credential, a nested gitfile, and control files beside them.
func plantDotGitReadTree(t *testing.T, workspacePath string) {
	t.Helper()
	files := map[string]string{
		".git/config":    "[remote \"origin\"]\n\turl = https://x-access-token:" + dotGitReadSecret + "@example.invalid/r.git\n",
		".git/HEAD":      "ref: refs/heads/main\n",
		"sub/.git":       "gitdir: ../.git/modules/sub\n",
		"sub/keep.txt":   "kept",
		".gitignore":     "*.tmp\n",
		"README.md":      "control",
		"docs/notes.txt": "notes",
	}
	for rel, content := range files {
		abs := filepath.Join(workspacePath, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	}
}

func hasDotGitComponent(p string) bool {
	for _, c := range strings.Split(p, "/") {
		if util.IsDotGitComponent(c) {
			return true
		}
	}
	return false
}

// TestProjectWorkspaceRead_DotGitRefused: GET of a path with a .git component
// is refused with the write error. The refusal is lexical and runs after
// authorization for every caller, so the test's caller (which may also
// write) stands in for a reader.
func TestProjectWorkspaceRead_DotGitRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Read DotGit")
	plantDotGitReadTree(t, workspacePath)
	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)

	for name, p := range map[string]string{
		"config":         ".git/config",
		"config_json":    ".git/config?format=json",
		"config_view":    ".git/config?view=true",
		"uppercase":      ".GIT/config",
		"nested_gitfile": "sub/.git",
	} {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodGet, filesURL+"/"+p, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), ".git component")
			assert.NotContains(t, rec.Body.String(), dotGitReadSecret)
		})
	}

	// Control files next to .git are still served.
	for p, want := range map[string]string{"README.md": "control", ".gitignore": "*.tmp\n", "sub/keep.txt": "kept"} {
		rec := doRequest(t, srv, http.MethodGet, filesURL+"/"+p, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s body: %s", p, rec.Body.String())
		assert.Equal(t, want, rec.Body.String(), "%s content", p)
	}
}

// TestProjectWorkspaceList_OmitsDotGit: the listing has no .git entry and
// still lists the control files.
func TestProjectWorkspaceList_OmitsDotGit(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS List DotGit")
	plantDotGitReadTree(t, workspacePath)

	for _, q := range []string{"", "?q=config", "?q=git"} {
		rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files%s", project.ID, q), nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.NotContains(t, rec.Body.String(), dotGitReadSecret)
		var resp ProjectWorkspaceListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for _, f := range resp.Files {
			assert.False(t, hasDotGitComponent(f.Path), "query %q lists %s", q, f.Path)
		}
		if q == "" {
			paths := map[string]bool{}
			for _, f := range resp.Files {
				paths[f.Path] = true
			}
			for _, p := range []string{"README.md", ".gitignore", "sub/keep.txt", "docs/notes.txt"} {
				assert.True(t, paths[p], "listing lacks %s", p)
			}
		}
	}
}

// TestProjectWorkspaceArchive_OmitsDotGit: the archive has no .git entry and
// still holds the control files.
func TestProjectWorkspaceArchive_OmitsDotGit(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Archive DotGit")
	plantDotGitReadTree(t, workspacePath)

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
	names := map[string]bool{}
	for _, f := range zr.File {
		assert.False(t, hasDotGitComponent(f.Name), "archive holds %s", f.Name)
		names[f.Name] = true
	}
	for _, p := range []string{"README.md", ".gitignore", "sub/keep.txt", "docs/notes.txt"} {
		assert.True(t, names[p], "archive lacks %s", p)
	}
}
