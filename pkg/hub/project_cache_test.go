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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestLinkedProject creates a git-backed project with a provider broker,
// simulating a linked project whose workspace lives on a remote broker.
func createTestLinkedProject(t *testing.T, srv *Server, s store.Store, name, remote string) (*store.Project, string) {
	t.Helper()

	// Create project
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:      name,
		GitRemote: remote,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Create a provider broker record with a local path
	brokerLocalPath := t.TempDir()
	broker := &store.RuntimeBroker{
		ID:   "test-broker-remote",
		Name: "remote-broker",
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	require.NoError(t, s.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID: broker.ID,
		// LocalPath is set to simulate a linked project with workspace on broker
		LocalPath: brokerLocalPath,
	}))

	// Set up the hub-side cache directory
	cachePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(cachePath) })

	return &project, brokerLocalPath
}

// ============================================================================
// resolveProjectWebDAVPath Tests
// ============================================================================

func TestResolveProjectWebDAVPath_HubNativeProject(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubNativeProject(t, srv, "WebDAV HubNative")

	path, err := srv.resolveProjectWebDAVPath(context.Background(), project)
	require.NoError(t, err)
	assert.Equal(t, workspacePath, path)
}

func TestResolveProjectWebDAVPath_LinkedProject_CacheDir(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "WebDAV Linked", "https://github.com/org/linked-repo.git")

	// resolveProjectWebDAVPath should return the hub cache path for remote linked projects
	path, err := srv.resolveProjectWebDAVPath(context.Background(), project)
	require.NoError(t, err)

	expectedCache, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	assert.Equal(t, expectedCache, path)
}

func TestResolveProjectWebDAVPath_LinkedProject_EmbeddedBroker(t *testing.T) {
	srv, s := testServer(t)

	// Create a project and set up an embedded broker
	project := createTestGitProject(t, srv, "WebDAV Embedded", "https://github.com/org/embedded-repo.git")

	embeddedPath := t.TempDir()
	embeddedBrokerID := "test-embedded-broker"
	srv.SetEmbeddedBrokerID(embeddedBrokerID)

	broker := &store.RuntimeBroker{
		ID:   embeddedBrokerID,
		Name: "embedded-broker",
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	require.NoError(t, s.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID:   project.ID,
		BrokerID:  embeddedBrokerID,
		LocalPath: embeddedPath,
	}))

	// For embedded broker, should serve directly from local path
	path, err := srv.resolveProjectWebDAVPath(context.Background(), project)
	require.NoError(t, err)
	assert.Equal(t, embeddedPath, path)
}

// ============================================================================
// isLinkedProject Tests
// ============================================================================

func TestIsLinkedProject_HubNative(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubNativeProject(t, srv, "IsLinked HubNative")

	assert.False(t, srv.isLinkedProject(context.Background(), project))
}

func TestIsLinkedProject_RemoteBroker(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "IsLinked Remote", "https://github.com/org/remote.git")

	assert.True(t, srv.isLinkedProject(context.Background(), project))
}

func TestIsLinkedProject_EmbeddedBrokerOnly(t *testing.T) {
	srv, s := testServer(t)
	project := createTestGitProject(t, srv, "IsLinked Embedded", "https://github.com/org/emb.git")

	embeddedBrokerID := "embedded-only"
	srv.SetEmbeddedBrokerID(embeddedBrokerID)

	broker := &store.RuntimeBroker{ID: embeddedBrokerID, Name: "emb"}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	require.NoError(t, s.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID:   project.ID,
		BrokerID:  embeddedBrokerID,
		LocalPath: "/some/path",
	}))

	// Embedded broker with local path should NOT be considered "linked" (it's co-located)
	assert.False(t, srv.isLinkedProject(context.Background(), project))
}

// ============================================================================
// Cache Status Endpoint Tests
// ============================================================================

func TestProjectCacheStatus_NoCacheExists(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "Cache Status Empty", "https://github.com/org/cache-status.git")

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/cache/status", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectCacheStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, project.ID, resp.ProjectID)
	assert.False(t, resp.Cached)
}

func TestProjectCacheStatus_CacheExists(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "Cache Status With", "https://github.com/org/cache-with.git")

	// Create the cache directory with some files
	cachePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(cachePath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(cachePath, "cached.txt"), []byte("cached"), 0644))

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/cache/status", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectCacheStatusResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, project.ID, resp.ProjectID)
	assert.True(t, resp.Cached)
}

func TestProjectCacheStatus_MethodNotAllowed(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "Cache Status Method", "https://github.com/org/method.git")

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/cache/status", project.ID), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ============================================================================
// WebDAV Access for Linked Projects
// ============================================================================

func TestProjectWebDAV_LinkedProject_ServesFromCache(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "WebDAV Linked Serve", "https://github.com/org/dav-linked.git")

	// Populate the cache directory
	cachePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(cachePath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(cachePath, "hello.txt"), []byte("hello from cache"), 0644))

	// WebDAV PROPFIND should work for linked projects (serves from cache)
	rec := doRequestWithMethod(t, srv, "PROPFIND",
		fmt.Sprintf("/api/v1/projects/%s/dav/", project.ID))
	// WebDAV PROPFIND returns 207 Multi-Status on success
	assert.Equal(t, http.StatusMultiStatus, rec.Code, "body: %s", rec.Body.String())
}

func TestProjectWebDAV_LinkedProject_EmptyCache(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "WebDAV Linked Empty", "https://github.com/org/dav-empty.git")

	// WebDAV PROPFIND on empty cache should still work (creates the directory)
	rec := doRequestWithMethod(t, srv, "PROPFIND",
		fmt.Sprintf("/api/v1/projects/%s/dav/", project.ID))
	assert.Equal(t, http.StatusMultiStatus, rec.Code, "body: %s", rec.Body.String())
}

// ============================================================================
// Workspace File API for Linked Projects
// ============================================================================

func TestProjectWorkspaceList_LinkedProject(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "WS List Linked", "https://github.com/org/ws-list.git")

	// Populate the cache
	cachePath, err := hubNativeProjectPath(project.Slug)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(cachePath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(cachePath, "file1.txt"), []byte("one"), 0644))

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Equal(t, 1, resp.TotalCount)
	assert.Equal(t, "file1.txt", resp.Files[0].Path)
}

// ============================================================================
// Cache Refresh Endpoint Tests
// ============================================================================

func TestProjectCacheRefresh_HubNativeProject_Conflict(t *testing.T) {
	srv, _ := testServer(t)
	project, _ := createTestHubNativeProject(t, srv, "Cache Refresh HubNative")

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/cache/refresh", project.ID), nil)
	// Hub-native projects should return conflict (they don't need cache refresh)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestProjectCacheRefresh_MethodNotAllowed(t *testing.T) {
	srv, s := testServer(t)
	project, _ := createTestLinkedProject(t, srv, s, "Cache Refresh Method", "https://github.com/org/refresh-method.git")

	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/cache/refresh", project.ID), nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ============================================================================
// hasProjectCache Tests
// ============================================================================

func TestHasProjectCache(t *testing.T) {
	// Non-existent slug should return false
	assert.False(t, hasProjectCache("non-existent-slug-12345"))
}

// ============================================================================
// Helpers
// ============================================================================

// doRequestWithMethod performs a raw HTTP request with a custom method (e.g., PROPFIND).
func doRequestWithMethod(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	// WebDAV PROPFIND requires a Depth header
	if method == "PROPFIND" {
		req.Header.Set("Depth", "1")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
