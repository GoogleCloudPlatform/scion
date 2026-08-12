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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Phase 0: Safety Gate Tests
// ============================================================================

func TestWorkspaceWriteBlocked_LocalBackendOnCloudRun(t *testing.T) {
	srv, _ := testServer(t)

	// Simulate Cloud Run environment
	t.Setenv("K_SERVICE", "hub-service")

	// No workspace storage config → local backend → blocked
	assert.True(t, srv.workspaceWriteBlocked())
}

func TestWorkspaceWriteBlocked_LocalBackendExplicit(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "local",
	}
	assert.True(t, srv.workspaceWriteBlocked())
}

func TestWorkspaceWriteBlocked_NFSBackendOnCloudRun(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "/mnt/nfs",
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}
	assert.False(t, srv.workspaceWriteBlocked())
}

func TestWorkspaceWriteBlocked_CloudRunVolumeOnCloudRun(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{
			VolumeName: "workspace-vol",
		},
	}
	assert.False(t, srv.workspaceWriteBlocked())
}

func TestWorkspaceWriteBlocked_NotOnCloudRun(t *testing.T) {
	srv, _ := testServer(t)

	// K_SERVICE not set → not on Cloud Run → writes allowed
	t.Setenv("K_SERVICE", "")
	assert.False(t, srv.workspaceWriteBlocked())
}

func TestSafetyGate_Write503OnCloudRunLocalBackend(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	project, _ := createTestHubManagedProject(t, srv, "Gate Write Test")

	// PUT (write) should return 503
	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files/test.txt", project.ID),
		ProjectWorkspaceWriteRequest{Content: "hello"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, "workspace_writes_unavailable", errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "Workspace writes are not available")
}

func TestSafetyGate_Upload503OnCloudRunLocalBackend(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	project, _ := createTestHubManagedProject(t, srv, "Gate Upload Test")

	// POST (upload) should return 503
	rec := doMultipartRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID),
		map[string][]byte{"test.txt": []byte("hello")})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestSafetyGate_Delete503OnCloudRunLocalBackend(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	project, workspacePath := createTestHubManagedProject(t, srv, "Gate Delete Test")

	// Create a file to delete
	require.NoError(t, os.WriteFile(filepath.Join(workspacePath, "deleteme.txt"), []byte("bye"), 0644))

	// DELETE should return 503
	rec := doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files/deleteme.txt", project.ID), nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestSafetyGate_ReadAllowedOnCloudRunLocalBackend(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	project, _ := createTestHubManagedProject(t, srv, "Gate Read Test")

	// GET (list) should still work
	rec := doRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestSafetyGate_WritesAllowedWithNFS(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	// Configure NFS backend — writes should be allowed
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "/mnt/nfs",
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	project, _ := createTestHubManagedProject(t, srv, "Gate NFS Test")

	// PUT should not return 503 (it may fail for other reasons since NFS isn't actually mounted)
	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files/test.txt", project.ID),
		ProjectWorkspaceWriteRequest{Content: "hello"})
	assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code)
}

// ============================================================================
// Phase 1: NFS Path Integration Tests
// ============================================================================

func TestServerHubManagedProjectPath_LocalBackend(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	srv, _ := testServer(t)
	// Default: no workspace storage config → local path
	path, err := srv.hubManagedProjectPath("my-project")
	require.NoError(t, err)

	expected := filepath.Join(tmpHome, ".scion", "projects", "my-project")
	assert.Equal(t, expected, path)
}

func TestServerHubManagedProjectPath_NFSBackend(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "/mnt/nfs",
			Shares:    []config.V1NFSShare{{ID: "ws-share", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	path, err := srv.hubManagedProjectPath("my-project")
	require.NoError(t, err)

	expected := filepath.Join("/mnt/nfs", "ws-share", "hub-projects", "my-project")
	assert.Equal(t, expected, path)
}

func TestServerHubManagedProjectPath_CloudRunVolume(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{
			VolumeName: "workspace-vol",
		},
	}

	path, err := srv.hubManagedProjectPath("my-project")
	require.NoError(t, err)

	expected := filepath.Join("/mnt", "workspace-vol", "projects", "hub-projects", "my-project")
	assert.Equal(t, expected, path)
}

func TestServerHubManagedProjectPath_CloudRunVolumeCustomSubPath(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{
			VolumeName:  "workspace-vol",
			SubPathRoot: "custom-root",
		},
	}

	path, err := srv.hubManagedProjectPath("my-project")
	require.NoError(t, err)

	expected := filepath.Join("/mnt", "workspace-vol", "custom-root", "hub-projects", "my-project")
	assert.Equal(t, expected, path)
}

func TestServerHubManagedProjectPath_NFSFallbackToLocal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	slug := "fallback-project"
	globalDir := filepath.Join(tmpHome, ".scion")

	// Create content in the local path only (simulating existing deployment)
	localDir := filepath.Join(globalDir, "projects", slug)
	require.NoError(t, os.MkdirAll(localDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("data"), 0644))

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: filepath.Join(tmpHome, "nfs-mount"),
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	// NFS path has no content, local path does → should fall back to local
	path, err := srv.hubManagedProjectPath(slug)
	require.NoError(t, err)
	assert.Equal(t, localDir, path)
}

func TestServerHubManagedProjectPath_NFSPrefersNFSWhenBothExist(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	slug := "both-exist"
	globalDir := filepath.Join(tmpHome, ".scion")

	// Create content in both local and NFS paths
	localDir := filepath.Join(globalDir, "projects", slug)
	require.NoError(t, os.MkdirAll(localDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local.txt"), []byte("local"), 0644))

	nfsBase := filepath.Join(tmpHome, "nfs-mount", "share1")
	nfsDir := filepath.Join(nfsBase, "hub-projects", slug)
	require.NoError(t, os.MkdirAll(nfsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nfsDir, "nfs.txt"), []byte("nfs"), 0644))

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: filepath.Join(tmpHome, "nfs-mount"),
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	// When both have content, NFS takes precedence
	path, err := srv.hubManagedProjectPath(slug)
	require.NoError(t, err)
	assert.Equal(t, nfsDir, path)
}

func TestServerHubManagedProjectPath_EmptySlugError(t *testing.T) {
	srv, _ := testServer(t)

	_, err := srv.hubManagedProjectPath("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slug must not be empty")
}

// ============================================================================
// Package-level hubManagedProjectPath backward compatibility
// ============================================================================

func TestPackageLevelHubManagedProjectPath_AlwaysLocal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	path, err := hubManagedProjectPath("test-slug")
	require.NoError(t, err)

	expected := filepath.Join(tmpHome, ".scion", "projects", "test-slug")
	assert.Equal(t, expected, path)
}

// ============================================================================
// Phase 3: Health Check Tests
// ============================================================================

func TestHealthCheck_NoWorkspaceStorage(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp HealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// No workspace_storage check when backend is local/unset
	_, hasCheck := resp.Checks["workspace_storage"]
	assert.False(t, hasCheck, "local backend should not have workspace_storage health check")
}

func TestHealthCheck_NFSHealthy(t *testing.T) {
	srv, _ := testServer(t)

	// Use a temp dir as the "NFS mount" so it actually exists
	nfsMount := t.TempDir()
	shareDir := filepath.Join(nfsMount, "test-share")
	require.NoError(t, os.MkdirAll(shareDir, 0755))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: nfsMount,
			Shares:    []config.V1NFSShare{{ID: "test-share", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	rec := doRequest(t, srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp HealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "healthy", resp.Checks["workspace_storage"])
}

func TestHealthCheck_NFSUnhealthy(t *testing.T) {
	srv, _ := testServer(t)

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "/nonexistent/nfs/mount",
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	rec := doRequest(t, srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp HealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Contains(t, resp.Checks["workspace_storage"], "unhealthy")
	assert.Equal(t, "degraded", resp.Status)
}

func TestReadiness_NFSUnavailable(t *testing.T) {
	srv, _ := testServer(t)

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "/nonexistent/nfs/mount",
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "not_ready", resp["status"])
	assert.Contains(t, resp["reason"], "workspace storage")
}

func TestReadiness_NFSAvailable(t *testing.T) {
	srv, _ := testServer(t)

	nfsMount := t.TempDir()
	shareDir := filepath.Join(nfsMount, "test-share")
	require.NoError(t, os.MkdirAll(shareDir, 0755))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: nfsMount,
			Shares:    []config.V1NFSShare{{ID: "test-share", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ============================================================================
// Integration Test: Write → Verify → Simulated Restart
// ============================================================================

func TestWorkspaceStorage_WriteVerifySurvivesRestart(t *testing.T) {
	// Simulate durable storage with a temp directory that represents
	// an NFS mount. Writes should persist across "restarts" (new Server instances
	// pointing at the same storage).
	nfsMount := t.TempDir()
	shareDir := filepath.Join(nfsMount, "test-share")
	require.NoError(t, os.MkdirAll(shareDir, 0755))

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	wsCfg := &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: nfsMount,
			Shares:    []config.V1NFSShare{{ID: "test-share", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	// Verify path selection
	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = wsCfg

	slug := "restart-test"
	path, err := srv.hubManagedProjectPath(slug)
	require.NoError(t, err)

	// Write a file to the "NFS" path
	require.NoError(t, os.MkdirAll(path, 0755))
	testContent := []byte("this data should survive a restart")
	require.NoError(t, os.WriteFile(filepath.Join(path, "persistent.txt"), testContent, 0644))

	// "Restart" — create a new server instance with the same config
	srv2, _ := testServer(t)
	srv2.config.WorkspaceStorageConfig = wsCfg

	// Verify the file is still accessible from the new instance
	path2, err := srv2.hubManagedProjectPath(slug)
	require.NoError(t, err)
	assert.Equal(t, path, path2, "path should be the same across restarts")

	data, err := os.ReadFile(filepath.Join(path2, "persistent.txt"))
	require.NoError(t, err)
	assert.Equal(t, testContent, data, "file content should survive simulated restart")
}

// ============================================================================
// WebDAV Safety Gate Tests
// ============================================================================

func TestWebDAVSafetyGate_WriteMethodsBlocked(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	project, _ := createTestHubManagedProject(t, srv, "WebDAV Gate Test")

	blockedMethods := []string{"PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH"}
	for _, method := range blockedMethods {
		t.Run(method, func(t *testing.T) {
			rec := doRequest(t, srv, method,
				fmt.Sprintf("/api/v1/projects/%s/dav/test.txt", project.ID), nil)
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
				"WebDAV %s should be blocked on Cloud Run with local backend", method)
		})
	}
}

// ============================================================================
// Phase 2: WebDAV Lock Store Tests
// ============================================================================

func TestWebDAVLockStore_PerProject(t *testing.T) {
	srv, _ := testServer(t)

	// Create two projects
	p1, _ := createTestHubManagedProject(t, srv, "Lock Project 1")
	p2, _ := createTestHubManagedProject(t, srv, "Lock Project 2")

	// Send PROPFIND requests to trigger lock store creation
	doRequest(t, srv, "PROPFIND", fmt.Sprintf("/api/v1/projects/%s/dav/", p1.ID), nil)
	doRequest(t, srv, "PROPFIND", fmt.Sprintf("/api/v1/projects/%s/dav/", p2.ID), nil)

	// Both should now have lock stores, and they should be different instances
	ls1, ok1 := srv.webdavLocks.Load(p1.ID)
	ls2, ok2 := srv.webdavLocks.Load(p2.ID)
	assert.True(t, ok1, "project 1 should have a lock store")
	assert.True(t, ok2, "project 2 should have a lock store")
	assert.NotNil(t, ls1, "project 1 lock store should not be nil")
	assert.NotNil(t, ls2, "project 2 lock store should not be nil")
	// Different projects should have independent lock stores
	assert.True(t, ls1 != ls2, "different projects should have different lock stores")
}

func TestWebDAVLockStore_SameProjectSharesLocks(t *testing.T) {
	srv, _ := testServer(t)

	project, _ := createTestHubManagedProject(t, srv, "Lock Shared Project")

	// First PROPFIND triggers lock store creation
	doRequest(t, srv, "PROPFIND", fmt.Sprintf("/api/v1/projects/%s/dav/", project.ID), nil)
	ls1, ok := srv.webdavLocks.Load(project.ID)
	require.True(t, ok)

	// Second request to same project should reuse the same lock store
	doRequest(t, srv, "PROPFIND", fmt.Sprintf("/api/v1/projects/%s/dav/", project.ID), nil)
	ls2, ok := srv.webdavLocks.Load(project.ID)
	require.True(t, ok)

	assert.Same(t, ls1, ls2, "same project should reuse the same lock store across requests")
}

func TestWebDAVSafetyGate_ReadMethodsAllowed(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	project, _ := createTestHubManagedProject(t, srv, "WebDAV Read Gate")

	// PROPFIND (directory listing) should still work
	rec := doRequest(t, srv, "PROPFIND",
		fmt.Sprintf("/api/v1/projects/%s/dav/", project.ID), nil)
	// PROPFIND returns 207 Multi-Status on success
	assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code,
		"WebDAV PROPFIND should not be blocked on Cloud Run")
}
