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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// doMultipartRequest creates a multipart form request with file uploads.
// files is a map of field name (relative path) to file content.
func doMultipartRequest(t *testing.T, srv *Server, method, path string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	for fieldName, content := range files {
		part, err := writer.CreateFormFile(fieldName, fieldName)
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// createTestHubManagedProject creates a hub-managed project (no git remote) via the API
// and returns the project and its workspace path. Cleans up the workspace and any
// external project-config directory on test completion.
func createTestHubManagedProject(t *testing.T, srv *Server, name string) (*store.Project, string) {
	t.Helper()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: name})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	workspacePath, err := hubManagedProjectPath(project.Slug)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Clean up the external project-config directory created by initInRepoProject
		// (e.g. ~/.scion/project-configs/<slug>__<uuid>/).
		scionDir := filepath.Join(workspacePath, ".scion")
		if extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir); err == nil && extAgentsDir != "" {
			// extAgentsDir is ~/.scion/project-configs/<slug>__<uuid>/.scion/agents
			// Go up past "agents" and ".scion" to remove the <slug>__<uuid> parent dir
			_ = os.RemoveAll(filepath.Dir(filepath.Dir(extAgentsDir)))
		}
		_ = os.RemoveAll(workspacePath)
	})

	return &project, workspacePath
}

// resolveTestSharedDirPath resolves the project-configs shared dir path for a test
// hub-managed project. This matches the path that resolveHubProjectSharedDirPath uses
// in production: it reads the .scion marker to find the project-configs directory.
func resolveTestSharedDirPath(t *testing.T, workspacePath, dirName string) string {
	t.Helper()
	scionPath := filepath.Join(workspacePath, config.DotScion)
	projectDir, _, err := config.ResolveProjectPath(scionPath)
	require.NoError(t, err, "failed to resolve project path from marker at %s", scionPath)
	sdPath, err := config.GetSharedDirPath(projectDir, dirName)
	require.NoError(t, err)
	return sdPath
}

// createTestGitProject creates a git-backed project via the API.
func createTestGitProject(t *testing.T, srv *Server, name, remote string) *store.Project {
	t.Helper()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:      name,
		GitRemote: remote,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	return &project
}

// ============================================================================
// List Tests
// ============================================================================

func TestProjectWorkspaceList_EmptyWorkspace(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS List Empty")

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// Only .scion/settings.yaml from project init should be present
	for _, f := range resp.Files {
		assert.True(t, strings.HasPrefix(f.Path, ".scion/"), "unexpected non-.scion file: %s", f.Path)
	}
}

func TestProjectWorkspaceList_WithFiles(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS List Files")

	// Create some test files
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "hello.txt"), []byte("hello world"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "subdir", "nested.txt"), []byte("nested"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	paths := make(map[string]bool)
	for _, f := range resp.Files {
		paths[f.Path] = true
	}
	assert.True(t, paths["hello.txt"])
	assert.True(t, paths[filepath.Join("subdir", "nested.txt")])
}

func TestProjectWorkspaceList_IncludesScionDir(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS List Scion")

	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "visible.txt"), []byte("yes"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, ".scion", "extra.txt"), []byte("also visible"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	paths := make(map[string]bool)
	for _, f := range resp.Files {
		paths[f.Path] = true
	}
	assert.True(t, paths["visible.txt"])
	assert.True(t, paths[filepath.Join(".scion", "extra.txt")])
}

func TestProjectWorkspaceList_ProjectNotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/nonexistent/workspace/files", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectWorkspaceList_GitProjectRejected(t *testing.T) {
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Git Project", "github.com/test/ws-list")

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// ============================================================================
// Upload Tests
// ============================================================================

func TestProjectWorkspaceUpload_SingleFile(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Upload Single")

	files := map[string][]byte{
		"readme.txt": []byte("hello from upload"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceUploadResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Len(t, resp.Files, 1)
	assert.Equal(t, "readme.txt", resp.Files[0].Path)
	assert.Equal(t, int64(17), resp.Files[0].Size)

	// Verify file on disk
	content, err := os.ReadFile(filepath.Join(workspacePath, "readme.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello from upload", string(content))
}

func TestProjectWorkspaceUpload_MultipleFiles(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Upload Multi")

	files := map[string][]byte{
		"a.txt": []byte("file a"),
		"b.txt": []byte("file b"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceUploadResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Len(t, resp.Files, 2)

	// Verify both files on disk
	for name, expected := range files {
		content, err := os.ReadFile(filepath.Join(workspacePath, name))
		require.NoError(t, err)
		assert.Equal(t, string(expected), string(content))
	}
}

func TestProjectWorkspaceUpload_NestedPath(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Upload Nested")

	files := map[string][]byte{
		"src/main.go": []byte("package main"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify file on disk with parent directory created
	content, err := os.ReadFile(filepath.Join(workspacePath, "src", "main.go"))
	require.NoError(t, err)
	assert.Equal(t, "package main", string(content))
}

func TestProjectWorkspaceUpload_PathTraversalRejected(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Upload Traversal")

	files := map[string][]byte{
		"../escape.txt": []byte("bad"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectWorkspaceUpload_NoFilesRejected(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Upload Empty")

	// Send an empty multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectWorkspaceUpload_GitProjectRejected(t *testing.T) {
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Git Upload", "github.com/test/ws-upload")

	files := map[string][]byte{
		"test.txt": []byte("nope"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// ============================================================================
// Delete Tests
// ============================================================================

func TestProjectWorkspaceDelete_Success(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete OK")

	// Create a file to delete
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "doomed.txt"), []byte("bye"), 0644))

	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/doomed.txt", project.ID), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify file is gone
	_, err := os.Stat(filepath.Join(workspacePath, "doomed.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestProjectWorkspaceDelete_NotFound(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Delete NF")

	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/nonexistent.txt", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectWorkspaceDelete_CleansEmptyDirs(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Delete Clean")

	// Create a nested file
	nestedDir := filepath.Join(workspacePath, "deep", "nested")
	require.NoError(t, os.MkdirAll(nestedDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nestedDir, "file.txt"), []byte("data"), 0644))

	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/deep/nested/file.txt", project.ID), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify empty parent dirs were cleaned up
	_, err := os.Stat(filepath.Join(workspacePath, "deep", "nested"))
	assert.True(t, os.IsNotExist(err), "nested dir should be removed")
	_, err = os.Stat(filepath.Join(workspacePath, "deep"))
	assert.True(t, os.IsNotExist(err), "deep dir should be removed")
	// The workspace root should still exist
	_, err = os.Stat(workspacePath)
	assert.NoError(t, err, "workspace root should still exist")
}

// ============================================================================
// Download Tests
// ============================================================================

func TestProjectWorkspaceDownload_Success(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download OK")

	content := []byte("hello download")
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "readme.txt"), content, 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/readme.txt", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	assert.Equal(t, "hello download", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "readme.txt")
	assert.Equal(t, "14", rec.Header().Get("Content-Length"))
}

func TestProjectWorkspaceDownload_NestedFile(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download Nested")

	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "src"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "src", "main.go"), []byte("package main"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/src/main.go", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, "package main", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "main.go")
}

func TestProjectWorkspaceDownload_NotFound(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Download NF")

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/nonexistent.txt", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectWorkspaceDownload_InlineView(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download Inline")

	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "readme.txt"), []byte("inline content"), 0644))

	// Without ?view=true — should be attachment
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/readme.txt", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")

	// With ?view=true — should be inline
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/readme.txt?view=true", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "inline")
	assert.Equal(t, "inline content", rec.Body.String())
}

func TestProjectWorkspaceDownload_FormatJSON(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download JSON")

	content := "# Hello\n\nThis is markdown."
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "readme.md"), []byte(content), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/readme.md?format=json", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, "readme.md", resp["path"])
	assert.Equal(t, content, resp["content"])
	assert.Equal(t, "utf-8", resp["encoding"])
	assert.Equal(t, float64(len(content)), resp["size"])
}

func TestProjectWorkspaceDownload_FormatJSON_BinaryRejected(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download JSON Bin")

	// Write binary content (invalid UTF-8)
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "image.bin"), []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0xFF, 0xFE}, 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/image.bin?format=json", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "binary")
}

func TestProjectWorkspaceDownload_FormatJSON_TooLarge(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download JSON Big")

	// Write a file larger than 1MB
	bigContent := make([]byte, maxEditableFileSize+1)
	for i := range bigContent {
		bigContent[i] = 'x'
	}
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "big.txt"), bigContent, 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/big.txt?format=json", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "too large")
}

func TestProjectWorkspaceDownload_FormatJSON_PreviewMode(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download JSON Preview")

	// Write a file slightly over 1MB (over edit limit, under preview limit)
	content := make([]byte, maxEditableFileSize+1)
	for i := range content {
		content[i] = 'A'
	}
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "large.txt"), content, 0644))

	// Without mode=preview, file over 1MB should be rejected
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/large.txt?format=json", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "too large")
	assert.Contains(t, rec.Body.String(), "editing")

	// With mode=preview, file over 1MB but under 100MB should succeed
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/large.txt?format=json&mode=preview", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "large.txt", resp["path"])
	assert.Equal(t, "utf-8", resp["encoding"])
}

func TestProjectWorkspaceDownload_FormatJSON_PreviewMode_TooLarge(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download JSON Preview Big")

	// Write a file over 100MB (over preview limit) — use a sparse approach
	// by creating the file and truncating to the desired size
	f, err := os.Create(filepath.Join(workspacePath, "huge.txt"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(int64(maxPreviewFileSize+1)))
	require.NoError(t, f.Close())

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/huge.txt?format=json&mode=preview", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "too large")
	assert.Contains(t, rec.Body.String(), "preview")
}

func TestProjectWorkspaceDownload_FormatJSON_PreviewMode_BinaryRejected(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Download JSON Preview Bin")

	// Write binary content (invalid UTF-8)
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "data.bin"), []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0xFF, 0xFE}, 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/data.bin?format=json&mode=preview", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "binary")
	assert.Contains(t, rec.Body.String(), "previewed")
}

// ============================================================================
// Archive Download Tests
// ============================================================================

func TestProjectWorkspaceArchive_Success(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Archive OK")

	// Create some test files
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "hello.txt"), []byte("hello world"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "subdir", "nested.txt"), []byte("nested"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body length: %d", rec.Body.Len())

	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), ".zip")

	// Verify the zip contents
	zipReader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)

	files := make(map[string]string)
	for _, f := range zipReader.File {
		rc, err := f.Open()
		require.NoError(t, err)
		content, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		files[f.Name] = string(content)
	}

	assert.Equal(t, "hello world", files["hello.txt"])
	assert.Equal(t, "nested", files[filepath.Join("subdir", "nested.txt")])
}

func TestProjectWorkspaceArchive_EmptyWorkspace(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Archive Empty")

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Should be a valid zip
	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	_, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)
}

func TestProjectWorkspaceArchive_GitProjectRejected(t *testing.T) {
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Git Archive", "github.com/test/ws-archive")

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestProjectWorkspaceArchive_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Archive Method")

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ============================================================================
// Write (PUT) Tests
// ============================================================================

func TestProjectWorkspaceWrite_CreateNewFile(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write New")

	body := ProjectWorkspaceWriteRequest{Content: "# New File\n\nHello!"}
	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/docs/readme.md", project.ID), body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceFile
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "docs/readme.md", resp.Path)
	assert.Equal(t, int64(len(body.Content)), resp.Size)

	// Verify on disk
	content, err := os.ReadFile(filepath.Join(workspacePath, "docs", "readme.md"))
	require.NoError(t, err)
	assert.Equal(t, body.Content, string(content))
}

func TestProjectWorkspaceWrite_OverwriteExisting(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write Overwrite")

	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "config.yaml"), []byte("old: true"), 0644))

	body := ProjectWorkspaceWriteRequest{Content: "new: true"}
	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/config.yaml", project.ID), body)
	require.Equal(t, http.StatusOK, rec.Code)

	content, err := os.ReadFile(filepath.Join(workspacePath, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "new: true", string(content))
}

func TestProjectWorkspaceWrite_ConflictDetection(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Write Conflict")

	filePath := filepath.Join(workspacePath, "data.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("original"), 0644))

	// Set expectedModTime to a time in the past
	pastTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	body := ProjectWorkspaceWriteRequest{
		Content:         "updated",
		ExpectedModTime: pastTime,
	}
	rec := doRequest(t, srv, http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files/data.txt", project.ID), body)
	assert.Equal(t, http.StatusConflict, rec.Code)

	// File should not have changed
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "original", string(content))
}

func TestProjectWorkspaceWrite_PathTraversalRejected(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Write Traversal")

	// Go's HTTP mux normalizes paths with "../" segments before they reach
	// the handler (returns 307 Redirect to the cleaned path). To test the
	// handler's own path validation, call handleProjectWorkspace directly
	// with a traversal path.
	body := ProjectWorkspaceWriteRequest{Content: "bad"}
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleProjectWorkspace(rec, req, project.ID, "../../../etc/passwd")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ============================================================================
// Auth Tests
// ============================================================================

func TestProjectWorkspace_RequiresAuth(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Auth")

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)},
		{http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)},
		{http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/test.txt", project.ID)},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			rec := doRequestNoAuth(t, srv, ep.method, ep.path, nil)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

// ============================================================================
// Method Not Allowed Tests
// ============================================================================

func TestProjectWorkspace_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Method")

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPut, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)},                 // PUT without filePath
		{http.MethodPatch, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)},               // PATCH not supported
		{http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files/some-file.txt", project.ID)},  // POST with filePath
		{http.MethodPatch, fmt.Sprintf("/api/v1/projects/%s/workspace/files/some-file.txt", project.ID)}, // PATCH with filePath
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := doRequest(t, srv, tt.method, tt.path, nil)
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		})
	}
}

// ============================================================================
// validateWorkspaceFilePath Unit Tests
// ============================================================================

func TestValidateWorkspaceFilePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
		errMsg  string
	}{
		{name: "valid simple", path: "file.txt", wantErr: false},
		{name: "valid nested", path: "src/main.go", wantErr: false},
		{name: "valid deeply nested", path: "a/b/c/d.txt", wantErr: false},
		{name: "valid dotfile", path: ".gitignore", wantErr: false},

		{name: "empty", path: "", wantErr: true, errMsg: "empty"},
		{name: "absolute unix", path: "/etc/passwd", wantErr: true, errMsg: "absolute"},
		{name: "traversal parent", path: "../escape.txt", wantErr: true, errMsg: "traversal"},
		{name: "traversal mid", path: "foo/../../escape.txt", wantErr: true, errMsg: "traversal"},
		{name: "scion root", path: ".scion", wantErr: false},
		{name: "scion file", path: ".scion/settings.yaml", wantErr: false},
		{name: "scion nested", path: ".scion/agents/test.yaml", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWorkspaceFilePath(tt.path)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ============================================================================
// Upload + List + Delete integration
// ============================================================================

func TestProjectWorkspace_UploadListDelete_Integration(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Integration")

	// Get baseline count (includes .scion files from project init)
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var baseResp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&baseResp))
	baseCount := baseResp.TotalCount

	// Upload files
	files := map[string][]byte{
		"main.py":        []byte("print('hello')"),
		"lib/helpers.py": []byte("def help(): pass"),
	}
	rec = doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	require.Equal(t, http.StatusOK, rec.Code, "upload body: %s", rec.Body.String())

	// List files
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var listResp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&listResp))
	assert.Equal(t, baseCount+2, listResp.TotalCount)

	// Delete one file
	rec = doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/main.py", project.ID), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// List again — should have one fewer
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	require.NoError(t, json.NewDecoder(rec.Body).Decode(&listResp))
	assert.Equal(t, baseCount+1, listResp.TotalCount)

	paths := make(map[string]bool)
	for _, f := range listResp.Files {
		paths[f.Path] = true
	}
	assert.True(t, paths[filepath.Join("lib", "helpers.py")])
	assert.False(t, paths["main.py"])
}

// ============================================================================
// Slug-format project ID Tests
// ============================================================================

func TestProjectWorkspace_SlugFormatProjectID(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "WS Slug Format")

	// Use {uuid}__{slug} format for project ID
	compositeID := project.ID + "__" + project.Slug
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", compositeID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// ============================================================================
// Shared Directory File Tests
// ============================================================================

// addSharedDirToProject adds a shared directory to a project via the API.
func addSharedDirToProject(t *testing.T, srv *Server, projectID, dirName string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/shared-dirs", projectID), map[string]interface{}{
		"name": dirName,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// setNFSSharedDirStorageGlobalSettings writes a global settings.yaml with
// server.shared_dir_storage: nfs, under a fresh HOME, and returns the
// resolved host base (<mount_root>/<share id>) so the caller can
// pre-populate/inspect the export directly. Must be called BEFORE
// testServer(t), since HOME must be set before any settings are read.
func setNFSSharedDirStorageGlobalSettings(t *testing.T) (hostBase string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	hostBase = filepath.Join(tmpHome, "nfs-export")
	require.NoError(t, os.MkdirAll(hostBase, 0o2775))

	settingsYAML := `schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: ` + tmpHome + `
      shares:
        - id: nfs-export
          pv_name: pv
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))
	return hostBase
}

// TestSharedDirFiles_NFSBackend_ListsExportLeaf is Phase 2 item 4 (design
// §3.2.5): when the hub's own global settings carry shared_dir_storage:
// nfs, the file browser must resolve and list the NFS export's leaf, not
// the local project-configs layout.
func TestSharedDirFiles_NFSBackend_ListsExportLeaf(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)

	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "NFS Browse Test", "github.com/test/nfs-browse-repo")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", "artifacts")
	require.NoError(t, os.MkdirAll(leaf, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(leaf, "note.txt"), []byte("from the export"), 0o644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, 1, resp.TotalCount)
	assert.Equal(t, "note.txt", resp.Files[0].Path)
}

// TestSharedDirFiles_NFSBackend_SymlinkEscapeRefused is the required
// "symlink escape is refused" test: a shared dir leaf that is a symlink
// pointing outside the export must not be browsable, and the symlink's
// target must be left untouched.
func TestSharedDirFiles_NFSBackend_SymlinkEscapeRefused(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)

	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "NFS Symlink Escape Test", "github.com/test/nfs-symlink-repo")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))

	leafParent := filepath.Join(hostBase, "projects", project.ID, "shared-dirs")
	require.NoError(t, os.MkdirAll(leafParent, 0o2775))
	require.NoError(t, os.Symlink(victim, filepath.Join(leafParent, "artifacts")))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID), nil)
	assert.NotEqual(t, http.StatusOK, rec.Code, "a symlinked leaf pointing outside the export must be refused, not browsed")

	info, err := os.Stat(filepath.Join(victim, "secret.txt"))
	require.NoError(t, err, "the victim must be untouched")
	assert.False(t, info.IsDir())
}

// TestSharedDirFiles_NFSBackend_LocalBackendExplicit_Unchanged is the
// required "local is unchanged" test, using an EXPLICIT backend: local
// (not just an absent block) to confirm resolveNFSSharedDirPath correctly
// reports "not applicable" for that value too, falling through to the
// pre-existing local-layout resolution byte-identically.
func TestSharedDirFiles_NFSBackend_LocalBackendExplicit_Unchanged(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: local\n"), 0644))

	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "Local Backend Explicit Test")
	addSharedDirToProject(t, srv, project.ID, "build-cache")

	filesURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/build-cache/files", project.ID)
	rec := doRequest(t, srv, http.MethodGet, filesURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 0, resp.TotalCount)

	// A GET must not bring the directory into existence: browsing is not a
	// provisioning operation (see TestSymlink_ReadDoesNotCreateSharedDir).
	sdPath := resolveTestSharedDirPath(t, workspacePath, "build-cache")
	_, err := os.Stat(sdPath)
	assert.True(t, os.IsNotExist(err), "a read must not create the shared dir")

	// A write, which does legitimately create it on first use, confirms
	// this really did resolve via the local layout the whole time, not
	// silently succeed some other way.
	rec = doRequest(t, srv, http.MethodPut, filesURL+"/note.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(sdPath, "note.txt"))
	require.NoError(t, err, "the write should have landed at the pre-existing project-configs layout")
	assert.Equal(t, "hello", string(got))
}

// --- os.Root confinement for every verb ---
//
// Every test below plants a symlink INSIDE an NFS-backed shared dir leaf
// pointing at a victim outside the export (or at another project's leaf,
// staying inside the export but outside the target leaf), then exercises
// one HTTP verb and asserts both that the operation is refused (not simply
// that it "fails silently") and that the victim is provably untouched.

// nfsSharedDirTestFixture sets up an NFS-backed project with one declared
// shared dir and a pre-created leaf, ready for a test to plant a symlink
// inside it.
type nfsSharedDirTestFixture struct {
	srv     *Server
	project *store.Project
	leaf    string // the shared dir's own leaf directory on the export
}

func setupNFSSharedDirTest(t *testing.T, projectName, dirName string) nfsSharedDirTestFixture {
	t.Helper()
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, projectName, "github.com/test/"+strings.ToLower(strings.ReplaceAll(projectName, " ", "-")))
	addSharedDirToProject(t, srv, project.ID, dirName)

	leaf := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", dirName)
	require.NoError(t, os.MkdirAll(leaf, 0o2775))

	return nfsSharedDirTestFixture{srv: srv, project: project, leaf: leaf}
}

func TestSharedDirFilesNFS_SymlinkedFile_DownloadRefused(t *testing.T) {
	f := setupNFSSharedDirTest(t, "Download Escape", "artifacts")

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim data"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(victim, "secret.txt"), filepath.Join(f.leaf, "escape.txt")))

	rec := doRequest(t, f.srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/escape.txt", f.project.ID), nil)
	assert.NotEqual(t, http.StatusOK, rec.Code, "downloading a symlink that escapes the leaf must be refused")
	assert.NotContains(t, rec.Body.String(), "victim data", "the victim's content must never appear in the response")
}

func TestSharedDirFilesNFS_SymlinkedFile_ArchiveExcludesIt(t *testing.T) {
	f := setupNFSSharedDirTest(t, "Archive Escape", "artifacts")

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "secret.txt"), []byte("victim archive data"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(victim, "secret.txt"), filepath.Join(f.leaf, "escape.txt")))
	require.NoError(t, os.WriteFile(filepath.Join(f.leaf, "real.txt"), []byte("real content"), 0o644))

	rec := doRequest(t, f.srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/archive", f.project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	require.NoError(t, err)

	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
		rc, err := zf.Open()
		require.NoError(t, err)
		content, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		assert.NotContains(t, string(content), "victim archive data",
			"the archive must never contain the victim's content, under any entry name")
	}
	assert.Contains(t, names, "real.txt", "the archive must still contain the leaf's real content")
}

func TestSharedDirFilesNFS_SymlinkedDir_PUTRefused(t *testing.T) {
	f := setupNFSSharedDirTest(t, "PUT Escape", "artifacts")

	victim := t.TempDir()
	require.NoError(t, os.Symlink(victim, filepath.Join(f.leaf, "escape")))

	rec := doRequest(t, f.srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/escape/authorized_keys", f.project.ID),
		ProjectWorkspaceWriteRequest{Content: "attacker-controlled-key"})
	assert.NotEqual(t, http.StatusOK, rec.Code, "a PUT through a symlinked directory escaping the leaf must be refused")

	_, statErr := os.Stat(filepath.Join(victim, "authorized_keys"))
	assert.True(t, os.IsNotExist(statErr), "the victim directory must not receive the written file")
}

func TestSharedDirFilesNFS_SymlinkedDir_UploadRefused(t *testing.T) {
	f := setupNFSSharedDirTest(t, "Upload Escape", "artifacts")

	victim := t.TempDir()
	require.NoError(t, os.Symlink(victim, filepath.Join(f.leaf, "escape")))

	rec := doMultipartRequest(t, f.srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", f.project.ID),
		map[string][]byte{"escape/authorized_keys": []byte("attacker-controlled-key")})
	assert.NotEqual(t, http.StatusOK, rec.Code, "an upload through a symlinked directory escaping the leaf must be refused")

	_, statErr := os.Stat(filepath.Join(victim, "authorized_keys"))
	assert.True(t, os.IsNotExist(statErr), "the victim directory must not receive the uploaded file")
}

func TestSharedDirFilesNFS_SymlinkedDir_DeleteRefused(t *testing.T) {
	f := setupNFSSharedDirTest(t, "Delete Escape", "artifacts")

	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "victim-file.txt"), []byte("do not delete me"), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(f.leaf, "escape")))

	rec := doRequest(t, f.srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/escape/victim-file.txt", f.project.ID), nil)
	assert.NotEqual(t, http.StatusNoContent, rec.Code, "a DELETE through a symlinked directory escaping the leaf must be refused")

	_, statErr := os.Stat(filepath.Join(victim, "victim-file.txt"))
	assert.NoError(t, statErr, "the victim's file must survive")
}

// TestSharedDirFilesNFS_MissingLeaf_SymlinkedPidComponent_Refused: the leaf
// doesn't exist yet, and the project's own <pid> directory (a structural
// component, not leaf content) is a symlink. A write verb must not create
// anything through it, at either the redirected location or the real
// expected path.
func TestSharedDirFilesNFS_MissingLeaf_SymlinkedPidComponent_Refused(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Mkdir Escape", "github.com/test/mkdir-escape")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	victim := t.TempDir()
	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))
	require.NoError(t, os.Symlink(victim, filepath.Join(projectsDir, project.ID)))

	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/note.txt", project.ID),
		ProjectWorkspaceWriteRequest{Content: "should never land anywhere"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"a write behind a symlinked pid component must be refused: EnsureLeaf's refusal is not an "+
			"fs.ErrNotExist-compatible error, so it surfaces as a real failure, not a quiet 404")

	_, statErr := os.Stat(filepath.Join(victim, "shared-dirs"))
	assert.True(t, os.IsNotExist(statErr), "nothing must be created through the symlinked pid component")
	// The symlink itself must be untouched -- never replaced, never
	// traversed to create a real directory over it.
	target, readErr := os.Readlink(filepath.Join(projectsDir, project.ID))
	require.NoError(t, readErr)
	assert.Equal(t, victim, target)
}

// TestSharedDirFilesNFS_MissingLeaf_CreatedWithHardenedModesAndACL: when a
// write verb legitimately creates a missing leaf (no symlink involved), it
// must get the same leaf modes/ACL the broker applies -- never a plain
// 0755 MkdirAll.
func TestSharedDirFilesNFS_MissingLeaf_CreatedWithHardenedModesAndACL(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Mkdir Symlinked Leaf", "github.com/test/mkdir-symlinked-leaf")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	// No leaf pre-created this time -- the handler must create the full
	// chain itself.
	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/note.txt", project.ID),
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	pidDir := filepath.Join(hostBase, "projects", project.ID)
	sharedDirsDir := filepath.Join(pidDir, "shared-dirs")
	leaf := filepath.Join(sharedDirsDir, "artifacts")

	for _, dir := range []string{filepath.Join(hostBase, "projects"), pidDir, sharedDirsDir} {
		info, err := os.Stat(dir)
		require.NoError(t, err, "intermediate dir %q should have been created", dir)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "intermediate dir %q permission bits", dir)
		assert.NotZero(t, info.Mode()&os.ModeSetgid, "intermediate dir %q must be setgid", dir)
	}

	info, err := os.Stat(leaf)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "leaf permission bits")
	assert.NotZero(t, info.Mode()&os.ModeSetgid, "leaf must be setgid")

	content, err := os.ReadFile(filepath.Join(leaf, "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))

	// The golden default ACL lands on the leaf itself...
	leafFd, err := unix.Open(leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	defer func() { _ = unix.Close(leafFd) }()

	buf := make([]byte, 256)
	n, xerr := unix.Fgetxattr(leafFd, "system.posix_acl_default", buf)
	if errors.Is(xerr, unix.ENOTSUP) || errors.Is(xerr, unix.EOPNOTSUPP) {
		t.Skip("filesystem does not support POSIX ACLs")
	}
	require.NoError(t, xerr, "read back system.posix_acl_default")
	wantACL := []byte{
		0x02, 0x00, 0x00, 0x00, // acl_ea_header, version 2
		0x01, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_USER_OBJ, rwx, undefined id
		0x04, 0x00, 0x07, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_GROUP_OBJ, rwx, undefined id
		0x20, 0x00, 0x05, 0x00, 0xff, 0xff, 0xff, 0xff, // ACL_OTHER, r-x, undefined id
	}
	assert.Equal(t, wantACL, buf[:n], "leaf's default ACL bytes")

	// ...and never on an intermediate: EnsureLeaf's modes/ACL hardening only
	// ever finalizes the leaf it creates.
	for _, dir := range []string{filepath.Join(hostBase, "projects"), pidDir, sharedDirsDir} {
		dirFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		require.NoError(t, err)
		_, xerr := unix.Fgetxattr(dirFd, "system.posix_acl_default", buf)
		_ = unix.Close(dirFd)
		assert.True(t, errors.Is(xerr, unix.ENODATA), "intermediate %q must have no default ACL, got %v", dir, xerr)
	}

	// A follow-up write to the same, now-existing leaf must not re-finalize
	// it: EnsureLeaf's alreadyExisted=true path skips the chmod/ACL step
	// entirely. Mark the leaf with a mode EnsureLeaf would never itself
	// produce, then confirm a second write leaves it exactly as marked.
	require.NoError(t, os.Chmod(leaf, 0o700))
	rec = doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/note2.txt", project.ID),
		ProjectWorkspaceWriteRequest{Content: "again"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	info, err = os.Stat(leaf)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"a follow-up write to an existing leaf must not re-chmod it (EnsureLeaf's existed=true skip path)")
}

// TestSharedDirFilesNFS_MissingLeaf_ListReturnsEmptyNotFoundNothingCreated:
// a GET list on a shared dir whose leaf doesn't exist yet must report an
// empty listing, not create the leaf or any part of its chain
// (openSharedDirRoot's createIfMissing=false for GET/DELETE). Uses the same
// hub-managed project fixture as the fail-closed tests below.
func TestSharedDirFilesNFS_MissingLeaf_ListReturnsEmptyNotFoundNothingCreated(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Missing Leaf List")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp SharedDirListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Files)

	_, statErr := os.Stat(filepath.Join(hostBase, "projects"))
	assert.True(t, os.IsNotExist(statErr), "listing a missing leaf must not create anything, not even the projects root")
}

// TestSharedDirFilesNFS_MissingLeaf_DownloadReturnsNotFoundNothingCreated:
// a GET on a specific file under a shared dir whose leaf doesn't exist yet
// must 404, not create anything.
func TestSharedDirFilesNFS_MissingLeaf_DownloadReturnsNotFoundNothingCreated(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Missing Leaf Download")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/note.txt", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	_, statErr := os.Stat(filepath.Join(hostBase, "projects"))
	assert.True(t, os.IsNotExist(statErr), "downloading from a missing leaf must not create anything")
}

// TestSharedDirFilesNFS_MissingLeaf_DeleteReturnsNotFoundNothingCreated: a
// DELETE on a file under a shared dir whose leaf doesn't exist yet must
// 404, not create anything (there is nothing to delete, and delete never
// creates).
func TestSharedDirFilesNFS_MissingLeaf_DeleteReturnsNotFoundNothingCreated(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Missing Leaf Delete", "github.com/test/missing-leaf-delete")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	rec := doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/note.txt", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	_, statErr := os.Stat(filepath.Join(hostBase, "projects"))
	assert.True(t, os.IsNotExist(statErr), "deleting from a missing leaf must not create anything")
}

// TestSharedDirFilesNFS_MissingLeaf_ArchiveReturnsNotFoundNothingCreated: an
// archive request on a shared dir whose leaf doesn't exist yet must 404,
// the same as the other read verbs, and must not create the leaf or any
// part of its chain.
func TestSharedDirFilesNFS_MissingLeaf_ArchiveReturnsNotFoundNothingCreated(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Archive Missing Leaf", "github.com/test/archive-missing-leaf")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/archive", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	_, statErr := os.Stat(filepath.Join(hostBase, "projects"))
	assert.True(t, os.IsNotExist(statErr), "archiving a missing leaf must not create anything")
}

// TestSharedDirFilesNFS_PreexistingFileAtPidComponent_ErrorsNothingCreated:
// a pre-existing REGULAR FILE (not a directory) at the structural
// projects/<pid> component must produce an error, not silently replace or
// traverse it, and must not create anything past it (shared-dirs/ or the
// leaf itself).
func TestSharedDirFilesNFS_PreexistingFileAtPidComponent_ErrorsNothingCreated(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "File At Pid Component", "github.com/test/file-at-pid-component")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	require.NoError(t, os.MkdirAll(filepath.Join(hostBase, "projects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hostBase, "projects", project.ID), []byte("not a directory"), 0o644))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID), nil)
	assert.NotEqual(t, http.StatusOK, rec.Code, "a file where the pid directory should be must not be silently used")
	assert.NotEqual(t, http.StatusCreated, rec.Code)

	fi, statErr := os.Stat(filepath.Join(hostBase, "projects", project.ID))
	require.NoError(t, statErr, "the pre-existing file itself must be left in place")
	assert.False(t, fi.IsDir(), "the pre-existing file must not have been replaced by a directory")

	// The parent path component (projects/<pid>) is itself a file, so
	// os.Stat on anything under it fails with ENOTDIR, not ENOENT -- either
	// way, nothing usable was created there.
	_, statErr = os.Stat(filepath.Join(hostBase, "projects", project.ID, "shared-dirs"))
	assert.Error(t, statErr, "nothing must be created past the blocking file")
}

// TestSharedDirFilesNFS_InvalidNFSBlock_ConflictNeverFallsToLocal: an nfs
// block that fails V1SharedDirStorageConfig.Validate() (here, an empty
// mount_root) must report 409 Conflict, and must NEVER fall through to the
// local-layout resolution -- resolveNFSSharedDirPath's applicable=true
// short-circuits resolveSharedDirPath regardless of the error. A sanity
// request against the working local layout runs first, on a project whose
// local resolution would ALSO otherwise succeed, so a 409 afterward can only
// be attributed to the broken nfs block, not to some unrelated project
// setup that would 409 on its own.
func TestSharedDirFilesNFS_InvalidNFSBlock_ConflictNeverFallsToLocal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Invalid NFS Block")
	addSharedDirToProject(t, srv, project.ID, "artifacts")
	url := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID)

	// Sanity: with no settings.yaml at all, the local layout serves a 200.
	rec := doRequest(t, srv, http.MethodGet, url, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// backend: nfs but nfs.mount_root is empty -- fails Validate().
	settingsYAML := `schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      shares:
        - id: nfs-export
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))

	rec = doRequest(t, srv, http.MethodGet, url, nil)
	assert.Equal(t, http.StatusConflict, rec.Code,
		"an invalid nfs block must 409, never silently fall through to the local layout")
}

// TestSharedDirArchiveNFS_InvalidNFSBlock_ConflictBodyIsFixedMessage: the
// archive endpoint's twin of the files-endpoint fixed-message requirement --
// a resolution failure must report the fixed, detail-free message, never the
// raw settings-parse error or a resolved host path.
func TestSharedDirArchiveNFS_InvalidNFSBlock_ConflictBodyIsFixedMessage(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Archive Invalid NFS Block")
	addSharedDirToProject(t, srv, project.ID, "artifacts")
	filesURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID)
	archiveURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/archive", project.ID)

	// Sanity: with no settings.yaml at all, the local layout resolves fine
	// (a write creates the leaf archive then reads back).
	rec := doRequest(t, srv, http.MethodPut, filesURL+"/note.txt", ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	rec = doRequest(t, srv, http.MethodGet, archiveURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// backend: nfs but nfs.mount_root is empty -- fails Validate().
	settingsYAML := `schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      shares:
        - id: nfs-export
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))

	rec = doRequest(t, srv, http.MethodGet, archiveURL, nil)
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), sharedDirStorageUnavailableMessage,
		"the archive endpoint must return the fixed message")
	assert.NotContains(t, rec.Body.String(), "mount_root",
		"the archive endpoint must not echo the raw settings-validation error")
}

// TestSharedDirFilesNFS_EmptyBackendStringWithNFSBlock_UsesLocal: backend:
// "" (the zero value, distinct from the block being entirely absent) with a
// populated nfs sub-block still configured underneath it must resolve via
// the pre-existing local layout, exactly like an absent shared_dir_storage
// block -- Validate() treats "" the same as "local", and
// resolveNFSSharedDirPath's sdCfg.Backend != "nfs" check must agree.
func TestSharedDirFilesNFS_EmptyBackendStringWithNFSBlock_UsesLocal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalScionDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	hostBase := filepath.Join(tmpHome, "nfs-export")
	require.NoError(t, os.MkdirAll(hostBase, 0o2775))

	settingsYAML := `schema_version: "1"
server:
  shared_dir_storage:
    backend: ""
    nfs:
      mount_root: ` + tmpHome + `
      shares:
        - id: nfs-export
          pv_name: pv
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settingsYAML), 0644))

	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "Empty Backend With NFS Block")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	filesURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID)
	rec := doRequest(t, srv, http.MethodGet, filesURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// The NFS export must never have been touched -- resolution went through
	// the local layout entirely.
	_, statErr := os.Stat(filepath.Join(hostBase, "projects"))
	assert.True(t, os.IsNotExist(statErr), "backend: \"\" must resolve locally, never touching the nfs export")

	// A read must not create the local shared dir either; a write, which
	// does create it on first use, confirms this resolved via the local
	// layout the whole time.
	sdPath := resolveTestSharedDirPath(t, workspacePath, "artifacts")
	_, err := os.Stat(sdPath)
	assert.True(t, os.IsNotExist(err), "a read must not create the shared dir")

	rec = doRequest(t, srv, http.MethodPut, filesURL+"/note.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(sdPath, "note.txt"))
	require.NoError(t, err, "the write should have landed at the pre-existing project-configs layout")
	assert.Equal(t, "hello", string(got))

	_, statErr = os.Stat(filepath.Join(hostBase, "projects"))
	assert.True(t, os.IsNotExist(statErr), "backend: \"\" must resolve locally even for a write, never touching the nfs export")
}

// TestSharedDirFiles_UnreadableGlobalSettings_FailsClosed_NoLocalFallback: a
// global settings file that fails to load, or loads via the legacy
// pre-schema_version format, must not silently serve the local layout if
// its raw bytes mention shared_dir_storage at all. A sanity request against
// the working local layout runs first, so a fail-closed result afterward
// can only be attributed to the broken settings, not to some unrelated
// setup problem.
func TestSharedDirFiles_UnreadableGlobalSettings_FailsClosed_NoLocalFallback(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	g := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(g, 0o755))

	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Broken Settings Fail Closed")
	addSharedDirToProject(t, srv, project.ID, "artifacts")
	url := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID)

	// Sanity: with no settings.yaml at all, the local layout serves a 200.
	rec := doRequest(t, srv, http.MethodGet, url, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// A v1-tagged file that mentions shared_dir_storage but fails to parse.
	require.NoError(t, os.WriteFile(filepath.Join(g, "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs: [unterminated\n"), 0o644))
	buf := captureAuditLogs(t)
	rec = doRequest(t, srv, http.MethodGet, url, nil)
	assert.NotEqual(t, http.StatusOK, rec.Code, "an unreadable settings file mentioning shared_dir_storage must fail closed, not serve the local layout")
	assert.Contains(t, rec.Body.String(), sharedDirStorageUnavailableMessage,
		"the client-facing error must be the fixed message, not the raw parse error or a host path")
	assertLogContains(t, buf, "failed to resolve shared directory path")

	// A legacy-format file (no schema_version) that still mentions the key.
	require.NoError(t, os.WriteFile(filepath.Join(g, "settings.yaml"),
		[]byte("server:\n  shared_dir_storage:\n    backend: nfs\n"), 0o644))
	rec = doRequest(t, srv, http.MethodGet, url, nil)
	assert.NotEqual(t, http.StatusOK, rec.Code, "a legacy-format file mentioning shared_dir_storage must also fail closed")
}

// TestSharedDirConfigDelete_UnreadableSettings_LogsAndSkipsCleanup_NoLocalTouch:
// when the global settings fail closed the same way, the config-level
// DELETE must still remove the DB record (cleanup is best-effort and must
// never block the caller), must log why cleanup was skipped, and must never
// fall back to touching the local project-configs layout it can no longer
// be sure is even the right one.
func TestSharedDirConfigDelete_UnreadableSettings_LogsAndSkipsCleanup_NoLocalTouch(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	g := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(g, 0o755))

	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "Delete Broken Settings")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	// Populate the local shared dir while settings are still unset (local
	// layout), so there is something concrete to prove untouched later.
	filesURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID)
	rec := doRequest(t, srv, http.MethodPut, filesURL+"/note.txt", ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	sdPath := resolveTestSharedDirPath(t, workspacePath, "artifacts")
	localFile := filepath.Join(sdPath, "note.txt")
	_, err := os.Stat(localFile)
	require.NoError(t, err, "sanity: the local file must exist before settings break")

	require.NoError(t, os.WriteFile(filepath.Join(g, "settings.yaml"),
		[]byte("schema_version: \"1\"\nserver:\n  shared_dir_storage:\n    backend: nfs\n    nfs: [unterminated\n"), 0o644))

	buf := captureAuditLogs(t)
	rec = doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts", project.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	assertLogContains(t, buf, "could not resolve shared directory host path for cleanup")

	_, statErr := os.Stat(localFile)
	assert.NoError(t, statErr, "the local shared dir file must be untouched when cleanup fails closed, not silently removed")
}

// assertLogContains reports whether any captured JSON log line's "msg"
// field contains substr.
func assertLogContains(t *testing.T, buf *bytes.Buffer, substr string) {
	t.Helper()
	for _, rec := range auditRecords(t, buf) {
		if msg, _ := rec["msg"].(string); strings.Contains(msg, substr) {
			return
		}
	}
	t.Errorf("no captured log line contains %q; log: %s", substr, buf.String())
}

// TestSharedDirFilesNFS_EnsureLeafFailure_PostPutRefused_NothingWritten:
// when ensureNFSSharedDirLeaf (shareddirs.EnsureLeaf) fails finalizing a
// leaf it just created, that error must reach the HTTP caller as a real
// failure (500 -- see below for why it's specifically 500, not just "any
// non-2xx"), and nothing must be left behind on the export -- not a
// half-finalized leaf, not an uploaded/written file. EnsureLeaf's own
// rollback already removes the leaf it created on a finalization failure.
//
// The exact status matters here, not just "non-2xx": EnsureLeaf's rollback
// means a *masked* ensureNFSSharedDirLeaf error (one that openSharedDirRoot
// ignored and treated as "proceed anyway") would still hit
// shareddirs.OpenAnchoredRoot next, which independently finds the
// just-rolled-back leaf missing and returns its OWN fs.ErrNotExist-compatible
// error -- which the handler maps to 404, not 500. A test that only checked
// "not 2xx" could not tell "the real error propagated" (500) apart from "the
// real error was swallowed, but a byproduct of its own rollback was caught
// by a second, independent check" (404) -- both are safe, but only the
// first is the specific failure mode this test pins.
func TestSharedDirFilesNFS_EnsureLeafFailure_PostPutRefused_NothingWritten(t *testing.T) {
	t.Run("PUT", func(t *testing.T) {
		hostBase := setNFSSharedDirStorageGlobalSettings(t)
		srv, _ := testServer(t)
		project := createTestGitProject(t, srv, "EnsureLeaf Failure PUT", "github.com/test/ensureleaf-failure-put")
		addSharedDirToProject(t, srv, project.ID, "artifacts")

		restore := shareddirs.SetLeafFinalizationHooksForTest(
			func(fd int, mode uint32) error { return unix.EINVAL },
			nil,
		)
		t.Cleanup(restore)

		rec := doRequest(t, srv, http.MethodPut,
			fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/note.txt", project.ID),
			ProjectWorkspaceWriteRequest{Content: "hello"})
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"the EnsureLeaf failure itself must propagate as a real error, not be masked into a 404 by a downstream check")

		_, statErr := os.Stat(filepath.Join(hostBase, "projects", project.ID, "shared-dirs", "artifacts", "note.txt"))
		assert.True(t, os.IsNotExist(statErr), "nothing must be written when the leaf can't be finalized")
	})

	t.Run("POST_upload", func(t *testing.T) {
		hostBase := setNFSSharedDirStorageGlobalSettings(t)
		srv, _ := testServer(t)
		project := createTestGitProject(t, srv, "EnsureLeaf Failure POST", "github.com/test/ensureleaf-failure-post")
		addSharedDirToProject(t, srv, project.ID, "artifacts")

		restore := shareddirs.SetLeafFinalizationHooksForTest(
			func(fd int, mode uint32) error { return unix.EINVAL },
			nil,
		)
		t.Cleanup(restore)

		rec := doMultipartRequest(t, srv, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID),
			map[string][]byte{"note.txt": []byte("hello")})
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"the EnsureLeaf failure itself must propagate as a real error, not be masked into a 404 by a downstream check")

		_, statErr := os.Stat(filepath.Join(hostBase, "projects", project.ID, "shared-dirs", "artifacts", "note.txt"))
		assert.True(t, os.IsNotExist(statErr), "nothing must be written when the leaf can't be finalized")
	})
}

// TestResolveNFSSharedDirPath_TraversalInputs_Refused: a direct, non-HTTP
// call into resolveNFSSharedDirPath with a traversal-shaped dirName or
// projectID must be refused, mirroring the creation-time resolver's own
// coverage (TestResolveSharedDirs_NFS_PoC_TraversalNamesAndProjectID in
// pkg/agent) at the hub's own resolver. dirName and projectID are checked by
// two SEPARATE validators (api.ValidateSharedDirs, shareddirs.ValidProjectID),
// so each case below breaks only ONE of the two inputs at a time -- a single
// case with both broken can't tell "the dirName check caught it" apart from
// "the projectID check caught it", since either alone already refuses.
func TestResolveNFSSharedDirPath_TraversalInputs_Refused(t *testing.T) {
	setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)

	t.Run("valid dirName, traversal-shaped projectID", func(t *testing.T) {
		resolution, applicable, err := srv.resolveNFSSharedDirPath("artifacts", "../y")
		assert.True(t, applicable, "nfs is configured, so this must never report not-applicable")
		require.Error(t, err)
		assert.Nil(t, resolution)
	})

	t.Run("traversal-shaped dirName, valid projectID", func(t *testing.T) {
		resolution, applicable, err := srv.resolveNFSSharedDirPath("../x", "pid-1")
		assert.True(t, applicable, "nfs is configured, so this must never report not-applicable")
		require.Error(t, err)
		assert.Nil(t, resolution)
	})
}

// TestSharedDirFilesNFS_ConcurrentSymlinkSwap_NeverWritesVictim: a goroutine
// repeatedly replaces a leaf entry between a plain directory and a symlink
// to a victim while PUT requests race it. The victim must never receive a
// write, regardless of timing.
func TestSharedDirFilesNFS_ConcurrentSymlinkSwap_NeverWritesVictim(t *testing.T) {
	f := setupNFSSharedDirTest(t, "Swap Race", "artifacts")

	victim := t.TempDir()
	real := filepath.Join(f.leaf, "real-target")
	require.NoError(t, os.MkdirAll(real, 0o775))
	linkPath := filepath.Join(f.leaf, "sub")

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = os.Remove(linkPath)
			_ = os.Symlink(victim, linkPath)
			_ = os.Remove(linkPath)
			_ = os.Mkdir(linkPath, 0o775)
		}
	}()

	for i := 0; i < 200; i++ {
		rec := doRequest(t, f.srv, http.MethodPut,
			fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files/sub/race-%d.txt", f.project.ID, i),
			ProjectWorkspaceWriteRequest{Content: "race"})
		_ = rec // either outcome (200 into the real dir, or refused mid-swap) is acceptable
	}
	stop.Store(true)
	<-done
	_ = os.Remove(linkPath)

	entries, err := os.ReadDir(victim)
	require.NoError(t, err)
	assert.Empty(t, entries, "the victim directory must never receive a file, regardless of swap timing")
}

func TestSharedDirFiles_ListEmpty(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "SD List Empty")

	addSharedDirToProject(t, srv, project.ID, "build-cache")

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/build-cache/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 0, resp.TotalCount)
	assert.Empty(t, resp.Files)
}

func TestSharedDirFiles_UploadAndList(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "SD Upload List")

	addSharedDirToProject(t, srv, project.ID, "artifacts")

	// Upload a file
	files := map[string][]byte{
		"output.log": []byte("build log content"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID), files)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify file on disk — shared dirs live under project-configs, not the workspace
	sdPath := resolveTestSharedDirPath(t, workspacePath, "artifacts")
	content, err := os.ReadFile(filepath.Join(sdPath, "output.log"))
	require.NoError(t, err)
	assert.Equal(t, "build log content", string(content))

	// List files
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 1, resp.TotalCount)
	assert.Equal(t, "output.log", resp.Files[0].Path)
}

func TestSharedDirFiles_Download(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "SD Download")

	addSharedDirToProject(t, srv, project.ID, "data")

	// Create a file directly at the project-configs shared dir path
	sharedDirPath := resolveTestSharedDirPath(t, workspacePath, "data")
	require.NoError(t, os.MkdirAll(sharedDirPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sharedDirPath, "result.txt"), []byte("result data"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/data/files/result.txt", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "result data", rec.Body.String())
}

func TestSharedDirFiles_Delete(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "SD Delete")

	addSharedDirToProject(t, srv, project.ID, "temp")

	// Create a file at the project-configs shared dir path
	sharedDirPath := resolveTestSharedDirPath(t, workspacePath, "temp")
	require.NoError(t, os.MkdirAll(sharedDirPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sharedDirPath, "old.txt"), []byte("old"), 0644))

	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/temp/files/old.txt", project.ID), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify file is gone
	_, err := os.Stat(filepath.Join(sharedDirPath, "old.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestSharedDirFiles_UndeclaredDirRejected(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "SD Undeclared")

	// Try to access files in a shared dir that hasn't been declared
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/nonexistent/files", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSharedDirFiles_GitProjectNoLocalBroker(t *testing.T) {
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "SD Git Project", "github.com/test/sd-files")

	addSharedDirToProject(t, srv, project.ID, "cache")

	// Without a co-located broker, shared dir browsing should return 409
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/cache/files", project.ID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "co-located runtime broker")
}

func TestSharedDirFiles_GitProjectWithEmbeddedBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := createTestGitProject(t, srv, "SD Git Embedded", "github.com/test/sd-embedded")
	addSharedDirToProject(t, srv, project.ID, "build-cache")

	// Create a broker and set it as the embedded broker
	broker := &store.RuntimeBroker{
		ID:       tid("embedded-broker-001"),
		Name:     "local-broker",
		Slug:     "local-broker",
		Endpoint: "http://localhost:9090",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	srv.SetEmbeddedBrokerID(broker.ID)

	// Add as provider WITHOUT LocalPath (simulates auto-link / shared-workspace)
	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		// LocalPath intentionally empty — fallback resolves via hub workspace marker
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	// Initialize a hub workspace so the .scion marker exists for path resolution.
	// This simulates a shared-workspace project that was cloned by the hub.
	workspacePath, err := hubManagedProjectPath(project.Slug)
	require.NoError(t, err)
	scionDir := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, config.InitProject(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}))

	t.Cleanup(func() {
		// Clean up the external project-config directory via marker resolution
		if resolved, rErr := config.ResolveProjectMarker(scionDir); rErr == nil {
			// resolved is ~/.scion/project-configs/<slug>__<uuid>/.scion/
			_ = os.RemoveAll(filepath.Dir(resolved))
		}
		_ = os.RemoveAll(workspacePath)
	})

	// Should now work via marker-based path resolution
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/build-cache/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp SharedDirListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 0, resp.TotalCount)
	assert.Equal(t, 1, resp.ProviderCount)
}

func TestSharedDirFiles_GitProjectMultipleProviders(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := createTestGitProject(t, srv, "SD Git Multi", "github.com/test/sd-multi")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	// Create embedded broker
	embeddedBroker := &store.RuntimeBroker{
		ID:       tid("embedded-broker-002"),
		Name:     "local-broker",
		Slug:     "local-broker-2",
		Endpoint: "http://localhost:9090",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, embeddedBroker))
	srv.SetEmbeddedBrokerID(embeddedBroker.ID)

	// Create a second (remote) broker
	remoteBroker := &store.RuntimeBroker{
		ID:       tid("remote-broker-001"),
		Name:     "remote-broker",
		Slug:     "remote-broker",
		Endpoint: "http://remote:9090",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, remoteBroker))

	// Add both as providers
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: embeddedBroker.ID, BrokerName: embeddedBroker.Name,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: remoteBroker.ID, BrokerName: remoteBroker.Name,
	}))

	// Initialize a hub workspace so the .scion marker exists for path resolution
	workspacePath, err := hubManagedProjectPath(project.Slug)
	require.NoError(t, err)
	scionDir := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, config.InitProject(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}))

	t.Cleanup(func() {
		if resolved, rErr := config.ResolveProjectMarker(scionDir); rErr == nil {
			_ = os.RemoveAll(filepath.Dir(resolved))
		}
		_ = os.RemoveAll(workspacePath)
	})

	// Request should succeed and report providerCount=2
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp SharedDirListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.ProviderCount)
}

// =============================================================================
// Shared Workspace (Git-Workspace Hybrid) Tests
// =============================================================================

// createTestSharedWorkspaceProject creates a shared-workspace git project via the API.
// It uses a local git repo as the clone source so that tests don't require network
// access or a GITHUB_TOKEN.
func createTestSharedWorkspaceProject(t *testing.T, srv *Server, name, remote string) (*store.Project, string) {
	t.Helper()

	// Create a local git repo to serve as the clone source
	sourceDir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = sourceDir
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:          name,
		GitRemote:     remote,
		WorkspaceMode: "shared",
		Labels: map[string]string{
			"scion.dev/clone-url":      sourceDir,
			"scion.dev/default-branch": "master",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	workspacePath, err := hubManagedProjectPath(project.Slug)
	require.NoError(t, err)

	t.Cleanup(func() {
		scionDir := filepath.Join(workspacePath, ".scion")
		if extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir); err == nil && extAgentsDir != "" {
			_ = os.RemoveAll(filepath.Dir(filepath.Dir(extAgentsDir)))
		}
		_ = os.RemoveAll(workspacePath)
	})

	return &project, workspacePath
}

func TestProjectWorkspaceList_SharedWorkspaceAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestSharedWorkspaceProject(t, srv, "Shared List", "github.com/test/shared-list")

	// Create a test file in the workspace
	require.NoError(t, os.MkdirAll(workspacePath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "hello.txt"), []byte("hello"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	assert.Equal(t, http.StatusOK, rec.Code, "shared-workspace project should allow file listing")

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.GreaterOrEqual(t, resp.TotalCount, 1, "should list at least the created file")
}

func TestProjectWorkspaceUpload_SharedWorkspaceAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestSharedWorkspaceProject(t, srv, "Shared Upload", "github.com/test/shared-upload")

	files := map[string][]byte{
		"test.txt": []byte("shared workspace upload"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	assert.Equal(t, http.StatusOK, rec.Code, "shared-workspace project should allow file upload")
}

func TestProjectWorkspaceArchive_SharedWorkspaceAllowed(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestSharedWorkspaceProject(t, srv, "Shared Archive", "github.com/test/shared-archive")

	// Create a test file
	require.NoError(t, os.MkdirAll(workspacePath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "file.txt"), []byte("archive me"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID), nil)
	assert.Equal(t, http.StatusOK, rec.Code, "shared-workspace project should allow workspace archive")
}

func TestProjectWorkspacePull_RequiresSharedWorkspace(t *testing.T) {
	srv, _ := testServer(t)

	// Create a regular hub-managed project (not shared-workspace)
	project, _ := createTestHubManagedProject(t, srv, "Pull NonShared")

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/pull", project.ID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "pull should be rejected for non-shared-workspace projects")
}

func TestProjectWorkspacePull_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	// Create shared-workspace project directly in the store to avoid clone attempt
	project := store.Project{
		ID:        tid("pull-method-test-id"),
		Name:      "Pull Method Test",
		Slug:      "pull-method-test",
		GitRemote: "github.com/test/pull-method",
		Labels: map[string]string{
			"scion.dev/workspace-mode": "shared",
		},
	}
	ctx := context.Background()
	err := srv.store.CreateProject(ctx, &project)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/pull", project.ID), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "GET should not be allowed for pull")
}

// ============================================================================
// FileSearcher / fuzzyMatch Unit Tests
// ============================================================================

func TestFuzzyMatch(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    bool
	}{
		// Basic in-order character matching
		{"abc", "a/b/c.go", true},
		{"abc", "xaxbxcx", true},
		{"abc", "cba", false},
		{"abc", "ab", false},

		// Case-insensitive
		{"ABC", "a/b/c.go", true},
		{"abc", "A/B/C.GO", true},

		// Empty pattern matches everything
		{"", "anything", true},
		{"", "", true},

		// Exact match
		{"foo", "foo", true},

		// Pattern longer than string
		{"toolong", "too", false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("pattern=%q input=%q", tt.pattern, tt.input), func(t *testing.T) {
			fn := fuzzyMatch(tt.pattern)
			assert.Equal(t, tt.want, fn(tt.input))
		})
	}
}

func TestIntQueryParam(t *testing.T) {
	makeReq := func(query string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		return req
	}

	assert.Equal(t, 500, intQueryParam(makeReq(""), "limit", 500, 2000))
	assert.Equal(t, 100, intQueryParam(makeReq("limit=100"), "limit", 500, 2000))
	assert.Equal(t, 2000, intQueryParam(makeReq("limit=9999"), "limit", 500, 2000))
	assert.Equal(t, 500, intQueryParam(makeReq("limit=0"), "limit", 500, 2000))
	assert.Equal(t, 500, intQueryParam(makeReq("limit=-1"), "limit", 500, 2000))
	assert.Equal(t, 500, intQueryParam(makeReq("limit=bad"), "limit", 500, 2000))
}

// openTestRoot opens dir as an *os.Root for the searcher tests below. The
// searcher takes a Root rather than a path so that it cannot be walked out of
// the directory it was given; see openConfinedBase.
func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// A directory that does not exist has no Root to search. The searcher no
// longer absorbs that case; the handlers do, by answering with an empty
// listing before they ever reach the searcher — see
// TestProjectWorkspaceList_MissingWorkspaceDir.
func TestWalkDirSearcher_NonExistentRoot(t *testing.T) {
	_, err := os.OpenRoot("/nonexistent/path/that/does/not/exist")
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "want a not-exist error, got %v", err)
}

func TestWalkDirSearcher_NoQuery(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("aaa"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "beta.go"), []byte("bb"), 0644))

	result, err := defaultFileSearcher.Search(openTestRoot(t, root), "", 500)
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)
	assert.False(t, result.HasMore)
	assert.Equal(t, int64(5), result.TotalSize)

	paths := make(map[string]bool)
	for _, f := range result.Files {
		paths[f.Path] = true
	}
	assert.True(t, paths["alpha.txt"])
	assert.True(t, paths[filepath.Join("sub", "beta.go")])
}

func TestWalkDirSearcher_RegexQuery(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "foo.go"), []byte("go"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bar.ts"), []byte("ts"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "baz.go"), []byte("go"), 0644))

	result, err := defaultFileSearcher.Search(openTestRoot(t, root), `\.go$`, 500)
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)
	assert.False(t, result.HasMore)

	for _, f := range result.Files {
		assert.True(t, strings.HasSuffix(f.Path, ".go"), "unexpected file: %s", f.Path)
	}
}

func TestWalkDirSearcher_FuzzyFallback(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "server.go"), []byte("s"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "client.go"), []byte("c"), 0644))

	// "[" is an invalid regex — verify the search degrades gracefully (no error,
	// returns results via fuzzy fallback). Since "[" as a char doesn't appear in
	// these filenames, TotalCount is 0, which is the correct fuzzy result.
	result, err := defaultFileSearcher.Search(openTestRoot(t, root), "[", 500)
	require.NoError(t, err, "invalid regex must not cause an error")
	assert.Equal(t, 0, result.TotalCount, "no filenames contain '['")

	// Verify that a valid regex matches correctly (different from fuzzy in-order logic).
	// "sev" as regex requires the LITERAL substring "sev" — "server.go" does not have it.
	// "sev" as fuzzy would match "server.go" (s..e..v in order).
	// Since "sev" IS a valid regex, the regex path runs and finds 0 matches.
	result2, err2 := defaultFileSearcher.Search(openTestRoot(t, root), "sev", 500)
	require.NoError(t, err2)
	assert.Equal(t, 0, result2.TotalCount, "literal regex 'sev' is not a substring of 'server.go'")
}

func TestWalkDirSearcher_LimitEnforced(t *testing.T) {
	root := t.TempDir()
	for i := range 10 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("file%02d.txt", i)), []byte("x"), 0644))
	}

	result, err := defaultFileSearcher.Search(openTestRoot(t, root), "", 3)
	require.NoError(t, err)
	assert.Equal(t, 10, result.TotalCount)
	assert.Len(t, result.Files, 3)
	assert.True(t, result.HasMore)
}

func TestWalkDirSearcher_SortByModTimeDesc(t *testing.T) {
	root := t.TempDir()

	// Write files with explicit mod times to ensure deterministic ordering.
	base := time.Now()
	files := []struct {
		name    string
		content string
		age     time.Duration
	}{
		{"oldest.txt", "old", 3 * time.Hour},
		{"middle.txt", "mid", 2 * time.Hour},
		{"newest.txt", "new", 1 * time.Hour},
	}
	for _, f := range files {
		p := filepath.Join(root, f.name)
		require.NoError(t, os.WriteFile(p, []byte(f.content), 0644))
		mt := base.Add(-f.age)
		require.NoError(t, os.Chtimes(p, mt, mt))
	}

	result, err := defaultFileSearcher.Search(openTestRoot(t, root), "", 500)
	require.NoError(t, err)
	require.Len(t, result.Files, 3)
	assert.Equal(t, "newest.txt", result.Files[0].Path)
	assert.Equal(t, "middle.txt", result.Files[1].Path)
	assert.Equal(t, "oldest.txt", result.Files[2].Path)
}

// ============================================================================
// HTTP integration tests for search params
// ============================================================================

func TestProjectWorkspaceList_SearchQuery(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Search Query")

	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "main.go"), []byte("go"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "README.md"), []byte("md"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "config.yaml"), []byte("yaml"), 0644))

	// Search by regex
	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files?q=\\.go$", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	for _, f := range resp.Files {
		assert.True(t, strings.HasSuffix(f.Path, ".go") || strings.HasPrefix(f.Path, ".scion"),
			"unexpected file: %s", f.Path)
	}
}

func TestProjectWorkspaceList_LimitParam(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS Limit Param")

	for i := range 10 {
		require.NoError(t, os.WriteFile(filepath.Join(workspacePath, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0644))
	}

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files?limit=3", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.LessOrEqual(t, len(resp.Files), 3)
	assert.True(t, resp.HasMore)
	assert.Greater(t, resp.TotalCount, len(resp.Files))
}

func TestProjectWorkspaceList_HasMoreFalseWhenFewFiles(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS HasMore False")

	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "only.txt"), []byte("x"), 0644))

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.HasMore)
}

func TestSharedDirFiles_SearchQuery(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "SD Search Query")
	addSharedDirToProject(t, srv, project.ID, "cache")

	sharedDirPath := resolveTestSharedDirPath(t, workspacePath, "cache")
	require.NoError(t, os.MkdirAll(sharedDirPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(sharedDirPath, "build.log"), []byte("log"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(sharedDirPath, "data.bin"), []byte("bin"), 0644))

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/cache/files?q=\\.log$", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp SharedDirListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 1, resp.TotalCount)
	assert.Equal(t, "build.log", resp.Files[0].Path)
}

// Ensure the store's ErrNotFound is wired correctly for project lookups.

func init() {
	// Silence logs during tests.
	_ = time.Now
	_ = io.Discard
	_ = context.Background
}
