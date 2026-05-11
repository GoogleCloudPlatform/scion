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
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// createTestHubNativeProject creates a hub-native project (no git remote) via the API
// and returns the project and its workspace path. Cleans up the workspace and any
// external project-config directory on test completion.
func createTestHubNativeProject(t *testing.T, srv *Server, name string) (*store.Project, string) {
	t.Helper()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: name})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Clean up the external project-config directory created by initInRepoProject
		// (e.g. ~/.scion/project-configs/<slug>__<uuid>/).
		scionDir := filepath.Join(workspacePath, ".scion")
		if extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir); err == nil && extAgentsDir != "" {
			// extAgentsDir is ~/.scion/project-configs/<slug>__<uuid>/.scion/agents
			// Go up past "agents" and ".scion" to remove the <slug>__<uuid> parent dir
			os.RemoveAll(filepath.Dir(filepath.Dir(extAgentsDir)))
		}
		os.RemoveAll(workspacePath)
	})

	return &project, workspacePath
}

// resolveTestSharedDirPath resolves the project-configs shared dir path for a test
// hub-native project. This matches the path that resolveHubProjectSharedDirPath uses
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
	project, _ := createTestHubNativeProject(t, srv, "WS List Empty")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS List Files")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS List Scion")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Upload Single")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Upload Multi")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Upload Nested")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Upload Traversal")

	files := map[string][]byte{
		"../escape.txt": []byte("bad"),
	}
	rec := doMultipartRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), files)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectWorkspaceUpload_NoFilesRejected(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubNativeProject(t, srv, "WS Upload Empty")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Delete OK")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Delete NF")

	rec := doRequest(t, srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/workspace/files/nonexistent.txt", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectWorkspaceDelete_CleansEmptyDirs(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Delete Clean")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Download OK")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Download Nested")

	require.NoError(t, os.MkdirAll(filepath.Join(workspacePath, "src"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "src", "main.go"), []byte("package main"), 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/src/main.go", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, "package main", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "main.go")
}

func TestProjectWorkspaceDownload_NotFound(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubNativeProject(t, srv, "WS Download NF")

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/nonexistent.txt", project.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProjectWorkspaceDownload_InlineView(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Download Inline")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Download JSON")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Download JSON Bin")

	// Write binary content (invalid UTF-8)
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "image.bin"), []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0xFF, 0xFE}, 0644))

	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/workspace/files/image.bin?format=json", project.ID), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "binary")
}

func TestProjectWorkspaceDownload_FormatJSON_TooLarge(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Download JSON Big")

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

// ============================================================================
// Archive Download Tests
// ============================================================================

func TestProjectWorkspaceArchive_Success(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Archive OK")

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
		rc.Close()
		files[f.Name] = string(content)
	}

	assert.Equal(t, "hello world", files["hello.txt"])
	assert.Equal(t, "nested", files[filepath.Join("subdir", "nested.txt")])
}

func TestProjectWorkspaceArchive_EmptyWorkspace(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubNativeProject(t, srv, "WS Archive Empty")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Archive Method")

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ============================================================================
// Write (PUT) Tests
// ============================================================================

func TestProjectWorkspaceWrite_CreateNewFile(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Write New")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Write Overwrite")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Write Conflict")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Write Traversal")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Auth")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Method")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Integration")

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
	project, _ := createTestHubNativeProject(t, srv, "WS Slug Format")

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

func TestSharedDirFiles_ListEmpty(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubNativeProject(t, srv, "SD List Empty")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "SD Upload List")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "SD Download")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "SD Delete")

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
	project, _ := createTestHubNativeProject(t, srv, "SD Undeclared")

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
		ID:       "embedded-broker-001",
		Name:     "local-broker",
		Slug:     "local-broker",
		Endpoint: "http://localhost:9090",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	srv.SetEmbeddedBrokerID(broker.ID)

	// Add as provider WITHOUT LocalPath (simulates auto-link / shared-workspace)
	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		// LocalPath intentionally empty — fallback resolves via hub workspace marker
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	// Initialize a hub workspace so the .scion marker exists for path resolution.
	// This simulates a shared-workspace project that was cloned by the hub.
	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	scionDir := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, config.InitProject(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}))

	t.Cleanup(func() {
		// Clean up the external project-config directory via marker resolution
		if resolved, rErr := config.ResolveProjectMarker(scionDir); rErr == nil {
			// resolved is ~/.scion/project-configs/<slug>__<uuid>/.scion/
			os.RemoveAll(filepath.Dir(resolved))
		}
		os.RemoveAll(workspacePath)
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
		ID:       "embedded-broker-002",
		Name:     "local-broker",
		Slug:     "local-broker-2",
		Endpoint: "http://localhost:9090",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, embeddedBroker))
	srv.SetEmbeddedBrokerID(embeddedBroker.ID)

	// Create a second (remote) broker
	remoteBroker := &store.RuntimeBroker{
		ID:       "remote-broker-001",
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
	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	scionDir := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, config.InitProject(scionDir, nil, config.InitProjectOpts{SkipRuntimeCheck: true}))

	t.Cleanup(func() {
		if resolved, rErr := config.ResolveProjectMarker(scionDir); rErr == nil {
			os.RemoveAll(filepath.Dir(resolved))
		}
		os.RemoveAll(workspacePath)
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

	workspacePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)

	t.Cleanup(func() {
		scionDir := filepath.Join(workspacePath, ".scion")
		if extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir); err == nil && extAgentsDir != "" {
			os.RemoveAll(filepath.Dir(filepath.Dir(extAgentsDir)))
		}
		os.RemoveAll(workspacePath)
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

	// Create a regular hub-native project (not shared-workspace)
	project, _ := createTestHubNativeProject(t, srv, "Pull NonShared")

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/workspace/pull", project.ID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "pull should be rejected for non-shared-workspace projects")
}

func TestProjectWorkspacePull_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	// Create shared-workspace project directly in the store to avoid clone attempt
	project := store.Project{
		ID:        "pull-method-test-id",
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

func TestWalkDirSearcher_NonExistentRoot(t *testing.T) {
	result, err := defaultFileSearcher.Search("/nonexistent/path/that/does/not/exist", "", 500)
	require.NoError(t, err)
	assert.Empty(t, result.Files)
	assert.Equal(t, 0, result.TotalCount)
	assert.False(t, result.HasMore)
}

func TestWalkDirSearcher_NoQuery(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("aaa"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "beta.go"), []byte("bb"), 0644))

	result, err := defaultFileSearcher.Search(root, "", 500)
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

	result, err := defaultFileSearcher.Search(root, `\.go$`, 500)
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
	result, err := defaultFileSearcher.Search(root, "[", 500)
	require.NoError(t, err, "invalid regex must not cause an error")
	assert.Equal(t, 0, result.TotalCount, "no filenames contain '['")

	// Verify that a valid regex matches correctly (different from fuzzy in-order logic).
	// "sev" as regex requires the LITERAL substring "sev" — "server.go" does not have it.
	// "sev" as fuzzy would match "server.go" (s..e..v in order).
	// Since "sev" IS a valid regex, the regex path runs and finds 0 matches.
	result2, err2 := defaultFileSearcher.Search(root, "sev", 500)
	require.NoError(t, err2)
	assert.Equal(t, 0, result2.TotalCount, "literal regex 'sev' is not a substring of 'server.go'")
}

func TestWalkDirSearcher_LimitEnforced(t *testing.T) {
	root := t.TempDir()
	for i := range 10 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("file%02d.txt", i)), []byte("x"), 0644))
	}

	result, err := defaultFileSearcher.Search(root, "", 3)
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

	result, err := defaultFileSearcher.Search(root, "", 500)
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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Search Query")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS Limit Param")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "WS HasMore False")

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
	project, workspacePath := createTestHubNativeProject(t, srv, "SD Search Query")
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
