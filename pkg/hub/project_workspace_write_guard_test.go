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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testWorkspaceBodyLimit lowers workspaceBodyLimit so the oversized-body
// tests stay small.
const testWorkspaceBodyLimit = 4096

// assertNoEntry fails when rel exists under the workspace.
func assertNoEntry(t *testing.T, workspacePath, rel string) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(workspacePath, filepath.FromSlash(rel)))
	assert.True(t, os.IsNotExist(err), "%s exists (err %v)", rel, err)
}

func TestProjectWorkspaceWrite_DotGitPathRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write DotGit")

	for _, p := range []string{".git/config", ".GIT/config", "a/.git/b"} {
		t.Run(p, func(t *testing.T) {
			body := ProjectWorkspaceWriteRequest{Content: "[core]\n"}
			rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/%s", project.ID, p), body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), ".git component")
			assertNoEntry(t, workspacePath, p)
			// Nothing is created on the way either.
			assertNoEntry(t, workspacePath, strings.SplitN(p, "/", 2)[0])
		})
	}
}

func TestProjectWorkspaceUpload_DotGitPathRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Upload DotGit")

	// A refused name refuses the whole upload: the allowed file in the same
	// request is not written either.
	files := map[string][]byte{
		".Git/hooks/pre-commit": []byte("#!/bin/sh\n"),
		"ok.txt":                []byte("fine"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), ".git component")
	assertNoEntry(t, workspacePath, ".Git")
	assertNoEntry(t, workspacePath, "ok.txt")
}

func TestProjectWorkspaceDelete_DotGitPathRefused(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete DotGit")

	cfg := filepath.Join(workspacePath, ".git", "config")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg), 0o755))
	require.NoError(t, os.WriteFile(cfg, []byte("[core]\n"), 0o644))

	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/.git/config", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	_, err := os.Stat(cfg)
	assert.NoError(t, err, ".git/config was removed")
}

func TestProjectWorkspaceWrite_OversizedBodyRefused(t *testing.T) {
	srv, _ := testServer(t)
	srv.workspaceBodyLimitForTest = testWorkspaceBodyLimit
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write Oversized")

	body := ProjectWorkspaceWriteRequest{Content: strings.Repeat("a", 2*testWorkspaceBodyLimit)}
	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/big.txt", project.ID), body)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body: %s", rec.Body.String())
	assertNoEntry(t, workspacePath, "big.txt")

	// A body under the limit is still written.
	small := ProjectWorkspaceWriteRequest{Content: "small"}
	rec = doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/docs/small.txt", project.ID), small)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	content, err := os.ReadFile(filepath.Join(workspacePath, "docs", "small.txt"))
	require.NoError(t, err)
	assert.Equal(t, "small", string(content))
}

func TestProjectWorkspaceUpload_OversizedBodyRefused(t *testing.T) {
	srv, _ := testServer(t)
	srv.workspaceBodyLimitForTest = testWorkspaceBodyLimit
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Upload Oversized")

	files := map[string][]byte{
		"big.bin": []byte(strings.Repeat("b", 2*testWorkspaceBodyLimit)),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "body: %s", rec.Body.String())
	assertNoEntry(t, workspacePath, "big.bin")

	// An upload under the limit is still written.
	rec = doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID),
		map[string][]byte{"small.txt": []byte("small")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	content, err := os.ReadFile(filepath.Join(workspacePath, "small.txt"))
	require.NoError(t, err)
	assert.Equal(t, "small", string(content))
}

func TestValidateWorkspaceWritePath(t *testing.T) {
	refused := []string{".git", ".git/config", ".GIT/config", "a/.git/b", "a/.Git", ".git./x", `a\.git\config`, "../x", ""}
	for _, p := range refused {
		assert.Error(t, validateWorkspaceWritePath(p), "path %q", p)
	}
	allowed := []string{"README.md", ".gitignore", ".github/workflows/ci.yml", "a/git/b", "a/x.git/b", "docs/.gitkeep"}
	for _, p := range allowed {
		assert.NoError(t, validateWorkspaceWritePath(p), "path %q", p)
	}
}
