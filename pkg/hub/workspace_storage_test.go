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
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

func TestWorkspaceWriteBlocked_GKESharedVolumeOnCloudRun(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  "workspace-vol",
			PVClaimName: "scion-workspaces",
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

// setVolumeMountBase points the platform volume mount base at dir for the
// duration of the test, so a test can create the mount root that a real
// deployment gets from Cloud Run or a Kubernetes pod spec. Tests in this
// package do not run in parallel, so mutating the package-level seam is safe.
func setVolumeMountBase(t *testing.T, dir string) {
	t.Helper()
	prev := volumeMountBase
	volumeMountBase = dir
	t.Cleanup(func() { volumeMountBase = prev })
}

// This test pins the literal default mount root: a deployment gets
// /mnt/<volume_name> and the chart must mount the PVC there.
func TestServerHubManagedProjectPath_GKESharedVolume(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  "workspace-vol",
			PVClaimName: "scion-workspaces",
		},
	}

	path, err := srv.hubManagedProjectPath("my-project")
	require.NoError(t, err)

	expected := filepath.Join("/mnt", "workspace-vol", "projects", "hub-projects", "my-project")
	assert.Equal(t, expected, path)
}

func TestServerHubManagedProjectPath_GKESharedVolumeCustomSubPath(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  "workspace-vol",
			SubPathRoot: "custom-root",
		},
	}

	path, err := srv.hubManagedProjectPath("my-project")
	require.NoError(t, err)

	expected := filepath.Join("/mnt", "workspace-vol", "custom-root", "hub-projects", "my-project")
	assert.Equal(t, expected, path)
}

// Content on the volume wins over content in the legacy local path. This is
// the case that discriminates: without the gke branch the local path is
// returned and the project is served from ephemeral disk.
func TestServerHubManagedProjectPath_GKESharedVolumePrefersVolumeWhenBothExist(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)

	slug := "gke-both-project"

	localDir := filepath.Join(tmpHome, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(localDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "local.txt"), []byte("local"), 0644))

	volumeDir := filepath.Join(mountBase, "workspace-vol", "projects", "hub-projects", slug)
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "volume.txt"), []byte("volume"), 0644))

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  "workspace-vol",
			PVClaimName: "scion-workspaces",
		},
	}

	path, err := srv.hubManagedProjectPath(slug)
	require.NoError(t, err)
	assert.Equal(t, volumeDir, path)
}

// A project with no content on the volume yet resolves to the volume, not to
// the local path — new projects are created on durable storage.
func TestServerHubManagedProjectPath_GKESharedVolumeEmptyVolumeStillWins(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "workspace-vol"},
	}

	path, err := srv.hubManagedProjectPath("new-project")
	require.NoError(t, err)

	expected := filepath.Join(mountBase, "workspace-vol", "projects", "hub-projects", "new-project")
	assert.Equal(t, expected, path)
}

func TestServerHubManagedProjectPath_GKESharedVolumeFallbackToLocal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	// The volume is mounted and empty: a deployment that predates it still has
	// its content on the local path.
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	slug := "gke-fallback-project"

	localDir := filepath.Join(tmpHome, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(localDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("data"), 0644))

	srv, _ := testServer(t)
	logs := captureProjectsLog(t, srv)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName: "workspace-vol",
		},
	}

	path, err := srv.hubManagedProjectPath(slug)
	require.NoError(t, err)
	assert.Equal(t, localDir, path)

	// The warning is the only thing that distinguishes this fallback from the
	// unfixed code, which returns the same local path without ever consulting
	// the volume — so asserting on it is what makes this test discriminate,
	// and it is also the only coverage the warning has.
	assert.Equal(t, 1, countWarningsForSlug(logs, slug))
	assert.Contains(t, logs.String(), filepath.Join(mountBase, "workspace-vol"))

	// Repeated resolutions must not repeat the warning: this runs on the
	// WebDAV, clone and cache paths.
	for range 3 {
		_, err := srv.hubManagedProjectPath(slug)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, countEphemeralWarnings(logs))

	// A second project must still get its own warning. Suppression is per
	// project, not per process: with a single shared key the first project to
	// resolve would silence every other one, and every assertion above would
	// still pass. This is the assertion that separates the two.
	otherSlug := "gke-fallback-project-2"
	otherLocalDir := filepath.Join(tmpHome, ".scion", "projects", otherSlug)
	require.NoError(t, os.MkdirAll(otherLocalDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(otherLocalDir, "existing.txt"), []byte("data"), 0644))

	for range 3 {
		otherPath, err := srv.hubManagedProjectPath(otherSlug)
		require.NoError(t, err)
		assert.Equal(t, otherLocalDir, otherPath)
	}
	assert.Equal(t, 2, countEphemeralWarnings(logs))
	assert.Equal(t, 1, countWarningsForSlug(logs, slug))
	assert.Equal(t, 1, countWarningsForSlug(logs, otherSlug))
}

// A gke-shared-volume config without a volume name has no mount point to build
// a path from, so it is treated as unset and falls through to the local path.
// Such a deployment fails its readiness check, so it never serves traffic.
func TestServerHubManagedProjectPath_GKESharedVolumeMissingVolumeName(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)

	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{PVClaimName: "scion-workspaces"},
	}

	path, err := srv.hubManagedProjectPath("my-project")
	require.NoError(t, err)

	expected := filepath.Join(tmpHome, ".scion", "projects", "my-project")
	assert.Equal(t, expected, path)
}

// TestWorkspaceMountRoot covers the single resolver both the readiness check
// and the hub-managed project path derive the mount location from.
func TestWorkspaceMountRoot(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.V1WorkspaceStorageConfig
		want string
	}{
		{name: "nil config", cfg: nil, want: ""},
		{name: "local backend", cfg: &config.V1WorkspaceStorageConfig{Backend: "local"}, want: ""},
		{
			name: "nfs joins mount root and first share",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend: "nfs",
				NFS: &config.V1NFSConfig{
					MountRoot: "/mnt/nfs",
					Shares:    []config.V1NFSShare{{ID: "share1"}, {ID: "share2"}},
				},
			},
			want: "/mnt/nfs/share1",
		},
		{
			name: "nfs without shares",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend: "nfs",
				NFS:     &config.V1NFSConfig{MountRoot: "/mnt/nfs"},
			},
			want: "",
		},
		{
			name: "cloudrun-volume mounts under /mnt",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend:        "cloudrun-volume",
				CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "scion-workspaces"},
			},
			want: "/mnt/scion-workspaces",
		},
		{
			name: "gke-shared-volume mounts under /mnt",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend: "gke-shared-volume",
				GKESharedVolume: &config.V1GKESharedVolumeConfig{
					VolumeName:  "scion-workspaces",
					PVClaimName: "scion-workspaces-pvc",
					SubPathRoot: "projects",
				},
			},
			want: "/mnt/scion-workspaces",
		},
		{
			name: "gke-shared-volume without a volume name",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend:         "gke-shared-volume",
				GKESharedVolume: &config.V1GKESharedVolumeConfig{PVClaimName: "scion-workspaces-pvc"},
			},
			want: "",
		},
		{
			name: "unknown backend",
			cfg:  &config.V1WorkspaceStorageConfig{Backend: "some-future-backend"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, workspaceMountRoot(tt.cfg))
		})
	}
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

// TestCheckWorkspaceStorageHealth_MountPathPerBackend covers mount path
// resolution for every backend.
//
// The Cloud Run and GKE volume backends resolve a path under /mnt, which a test
// cannot create, so those cases assert the distinction between "mount path not
// configured" (no path resolved at all — the gke-shared-volume bug) and "mount
// not available" (a path was resolved and stat'ed). The NFS backend, whose
// mount root is configurable, covers the healthy path above.
func TestCheckWorkspaceStorageHealth_MountPathPerBackend(t *testing.T) {
	srv, _ := testServer(t)

	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1"), 0755))

	tests := []struct {
		name string
		cfg  *config.V1WorkspaceStorageConfig
		want string // expected checks["workspace_storage"]; "" means no entry
	}{
		{
			name: "nil config records no check",
			cfg:  nil,
			want: "",
		},
		{
			name: "local backend records no check",
			cfg:  &config.V1WorkspaceStorageConfig{Backend: "local"},
			want: "",
		},
		{
			name: "empty backend records no check",
			cfg:  &config.V1WorkspaceStorageConfig{Backend: ""},
			want: "",
		},
		{
			name: "nfs with a mounted share is healthy",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend: "nfs",
				NFS: &config.V1NFSConfig{
					MountRoot: mountRoot,
					Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
				},
			},
			want: "healthy",
		},
		{
			name: "nfs with a missing share reports mount unavailable",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend: "nfs",
				NFS: &config.V1NFSConfig{
					MountRoot: mountRoot,
					Shares:    []config.V1NFSShare{{ID: "absent-share", Server: "10.0.0.2", Export: "/scion"}},
				},
			},
			want: "unhealthy: mount not available",
		},
		{
			name: "nfs without shares reports mount path not configured",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend: "nfs",
				NFS:     &config.V1NFSConfig{MountRoot: mountRoot},
			},
			want: "unhealthy: mount path not configured",
		},
		{
			name: "cloudrun-volume with a volume name resolves a mount path",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend:        "cloudrun-volume",
				CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "scion-absent-cloudrun-vol"},
			},
			want: "unhealthy: mount not available",
		},
		{
			name: "cloudrun-volume without a volume name reports mount path not configured",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend:        "cloudrun-volume",
				CloudRunVolume: &config.V1CloudRunVolumeConfig{},
			},
			want: "unhealthy: mount path not configured",
		},
		{
			name: "gke-shared-volume with a volume name resolves a mount path",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend: "gke-shared-volume",
				GKESharedVolume: &config.V1GKESharedVolumeConfig{
					VolumeName:  "scion-absent-gke-vol",
					PVClaimName: "scion-workspaces",
					SubPathRoot: "projects",
				},
			},
			want: "unhealthy: mount not available",
		},
		{
			// The mount path keys off VolumeName, not PVClaimName.
			name: "gke-shared-volume without a volume name reports mount path not configured",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend:         "gke-shared-volume",
				GKESharedVolume: &config.V1GKESharedVolumeConfig{PVClaimName: "scion-workspaces"},
			},
			want: "unhealthy: mount path not configured",
		},
		{
			name: "gke-shared-volume without a config block reports mount path not configured",
			cfg:  &config.V1WorkspaceStorageConfig{Backend: "gke-shared-volume"},
			want: "unhealthy: mount path not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv.config.WorkspaceStorageConfig = tt.cfg

			checks := make(map[string]string)
			srv.checkWorkspaceStorageHealth(checks)

			if tt.want == "" {
				assert.NotContains(t, checks, "workspace_storage")
				return
			}
			assert.Equal(t, tt.want, checks["workspace_storage"])
		})
	}
}

// ephemeralWarnMessage is the message logged by warnEphemeralProjectPath.
const ephemeralWarnMessage = "hub-managed project served from ephemeral local path"

// countEphemeralWarnings returns how many ephemeral-path warnings were logged,
// and countWarningsForSlug how many of those name the given slug.
//
// Parsed per line and per attribute rather than matched as a substring: a
// substring match for `slug=<name> ` depends on slog attribute ORDER, so
// reordering the attrs in warnEphemeralProjectPath would silently make these
// assertions count zero and pass. An assertion whose whole job is to not pass
// vacuously must not be the thing that quietly stops matching.
func countEphemeralWarnings(logs *bytes.Buffer) int {
	return countWarningsForSlug(logs, "")
}

func countWarningsForSlug(logs *bytes.Buffer, slug string) int {
	n := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, ephemeralWarnMessage) {
			continue
		}
		if slug == "" {
			n++
			continue
		}
		for _, field := range strings.Fields(line) {
			if key, value, found := strings.Cut(field, "="); found && key == "slug" {
				if strings.Trim(value, `"`) == slug {
					n++
				}
				break
			}
		}
	}
	return n
}

// captureProjectsLog redirects the projects subsystem logger into a buffer the
// test can assert on.
func captureProjectsLog(t *testing.T, srv *Server) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := srv.projectsLog
	srv.projectsLog = slog.New(slog.NewTextHandler(buf, nil))
	t.Cleanup(func() { srv.projectsLog = prev })
	return buf
}

// setContainerRootPath overrides the reference path isMountedVolume compares
// device IDs against. Pointing it at a temp directory makes anything created
// under that directory look like it lives on the container root filesystem.
func setContainerRootPath(t *testing.T, dir string) {
	t.Helper()
	prev := containerRootPath
	containerRootPath = dir
	t.Cleanup(func() { containerRootPath = prev })
}

// distinctDeviceDir returns a fresh directory on a filesystem other than the
// one containerRootPath lives on — a stand-in for a real volume mount — or
// skips the test when the sandbox has no second filesystem to offer.
func distinctDeviceDir(t *testing.T) string {
	t.Helper()

	rootFI, err := os.Stat(containerRootPath)
	require.NoError(t, err)
	rootDev, ok := deviceID(rootFI)
	if !ok {
		t.Skip("device IDs unavailable on this platform")
	}

	for _, candidate := range []string{"/dev/shm", "/run", "/tmp", os.TempDir()} {
		fi, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		dev, ok := deviceID(fi)
		if !ok || dev == rootDev {
			continue
		}
		dir, err := os.MkdirTemp(candidate, "scion-mount-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}

	t.Skip("no filesystem distinct from the container root is available")
	return ""
}

// TestCheckWorkspaceStorageHealth_GKEOverlayDirIsUnhealthy is the regression
// test for the self-latching readiness check.
//
// Nothing forces a Kubernetes pod spec to mount the PVC at /mnt/<volume_name>.
// When it does not, the hub — running as root — creates that directory itself
// on the container overlay the first time a project is written. Readiness must
// keep reporting unhealthy in that state; if it flips to healthy, the pod
// serves every project tree from ephemeral disk and the failure is silent.
func TestCheckWorkspaceStorageHealth_GKEOverlayDirIsUnhealthy(t *testing.T) {
	srv, _ := testServer(t)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	// Same filesystem as the "container root", i.e. an overlay directory.
	setContainerRootPath(t, mountBase)
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  "workspace-vol",
			PVClaimName: "scion-workspaces",
		},
	}

	checks := make(map[string]string)
	srv.checkWorkspaceStorageHealth(checks)
	assert.Equal(t, "unhealthy: mount path is not a mounted volume", checks["workspace_storage"])
}

// The same condition must keep the pod out of the load balancer, not merely
// annotate /healthz.
func TestReadiness_GKEOverlayDirNotReady(t *testing.T) {
	srv, _ := testServer(t)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	setContainerRootPath(t, mountBase)
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "workspace-vol"},
	}

	rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "not_ready", resp["status"])
	assert.Contains(t, resp["reason"], "workspace storage")
}

// A correctly mounted volume — a filesystem of its own — is healthy.
func TestCheckWorkspaceStorageHealth_GKEMountedVolumeIsHealthy(t *testing.T) {
	srv, _ := testServer(t)

	mountDir := distinctDeviceDir(t)
	setVolumeMountBase(t, filepath.Dir(mountDir))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  filepath.Base(mountDir),
			PVClaimName: "scion-workspaces",
		},
	}

	checks := make(map[string]string)
	srv.checkWorkspaceStorageHealth(checks)
	assert.Equal(t, "healthy", checks["workspace_storage"])
}

// A chart may mount the PVC one level up — at the mount base itself, with a
// subPath — so that <base>/<volume_name> is a directory inside the volume
// rather than the mount point. Writes there still land in the volume, so this
// must read as healthy. Comparing against the container root rather than the
// path's parent is what makes that work.
func TestCheckWorkspaceStorageHealth_GKEVolumeMountedAtBaseIsHealthy(t *testing.T) {
	srv, _ := testServer(t)

	mountDir := distinctDeviceDir(t)
	setVolumeMountBase(t, mountDir)
	require.NoError(t, os.MkdirAll(filepath.Join(mountDir, "workspace-vol"), 0755))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "workspace-vol"},
	}

	checks := make(map[string]string)
	srv.checkWorkspaceStorageHealth(checks)
	assert.Equal(t, "healthy", checks["workspace_storage"])
}

// statlessFileInfo is an os.FileInfo whose Sys() carries no unix stat, which is
// what a filesystem implementation outside package os hands back.
type statlessFileInfo struct{}

func (statlessFileInfo) Name() string       { return "workspace-vol" }
func (statlessFileInfo) Size() int64        { return 0 }
func (statlessFileInfo) Mode() os.FileMode  { return os.ModeDir | 0755 }
func (statlessFileInfo) ModTime() time.Time { return time.Time{} }
func (statlessFileInfo) IsDir() bool        { return true }
func (statlessFileInfo) Sys() any           { return nil }

// The three branches where the device comparison cannot be made are
// unreachable on every platform this repo ships, which is exactly why they need
// tests: nothing else will ever exercise them. They must fail open — refusing
// readiness on a check that cannot be enforced would take a working pod out of
// service — and they must say so, which is the second return.
func TestIsMountedVolume_FailsOpenWhenUndeterminable(t *testing.T) {
	dir := t.TempDir()
	realFI, err := os.Stat(dir)
	require.NoError(t, err)

	t.Run("file info carries no unix stat", func(t *testing.T) {
		mounted, determinable := isMountedVolume(statlessFileInfo{}, dir)
		assert.True(t, mounted)
		assert.False(t, determinable)
	})

	t.Run("container root cannot be stat'ed", func(t *testing.T) {
		mounted, determinable := isMountedVolume(realFI, filepath.Join(dir, "absent-root"))
		assert.True(t, mounted)
		assert.False(t, determinable)
	})

	t.Run("a determinable comparison reports so", func(t *testing.T) {
		mounted, determinable := isMountedVolume(realFI, dir)
		assert.False(t, mounted, "a directory compared against itself shares its device")
		assert.True(t, determinable)
	})
}

// Failing open leaves the pod ready — deliberately — but it also reinstates the
// failure mode the mount check exists to catch, so it must not be reported as
// an ordinary "healthy". The distinct entry marks overall health degraded while
// leaving readiness alone.
func TestCheckWorkspaceStorageHealth_GKEUnverifiableMountStaysReady(t *testing.T) {
	srv, _ := testServer(t)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	setContainerRootPath(t, filepath.Join(mountBase, "absent-root"))
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "workspace-vol"},
	}

	checks := make(map[string]string)
	srv.checkWorkspaceStorageHealth(checks)
	assert.Equal(t, "healthy", checks["workspace_storage"])
	assert.Contains(t, checks["workspace_storage_mount_verification"], "unavailable")

	rec := doRequest(t, srv, http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "an unenforceable check must not take the pod out of service")

	rec = doRequest(t, srv, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp HealthResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "degraded", resp.Status, "the operator needs to see that the mount was never verified")
}

// The mount requirement is confined to the GKE backend: Cloud Run mounts the
// volume itself, and changing what its deployments report is out of scope.
func TestCheckWorkspaceStorageHealth_CloudRunPlainDirIsHealthy(t *testing.T) {
	srv, _ := testServer(t)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	setContainerRootPath(t, mountBase)
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:        "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "workspace-vol"},
	}

	checks := make(map[string]string)
	srv.checkWorkspaceStorageHealth(checks)
	assert.Equal(t, "healthy", checks["workspace_storage"])
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

// The ephemeral-path warning is suppressed per slug for the life of the
// process, so the suppression has to be dropped when the slug goes away.
// Otherwise a slug that is deleted and recreated — or reused by a different
// project — silently inherits the old suppression and never warns, on a
// deployment that is still writing to ephemeral storage.
//
// Configured against gke-shared-volume on purpose, and asserted immediately
// after the delete returns. deleteProject resolves the slug itself on its way
// to removing the directory; under any other backend that resolution never
// reaches the warning, and an eviction placed before it would pass. Totals at
// the end of the test cannot tell the two placements apart, because a
// misplaced eviction is followed by a warn that re-records the slug and
// suppresses the later one — same count, opposite behavior.
func TestWarnEphemeralProjectPath_SuppressionClearedOnProjectDelete(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	srv, _ := testServer(t)
	logs := captureProjectsLog(t, srv)

	// Created before the backend is configured, so its content is on the local
	// path — the legacy deployment this fallback exists for.
	project, localDir := createTestHubManagedProject(t, srv, "Ephemeral Warn Delete")
	seedLocal := func() {
		require.NoError(t, os.MkdirAll(localDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("data"), 0644))
	}
	seedLocal()

	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "workspace-vol"},
	}

	path, err := srv.hubManagedProjectPath(project.Slug)
	require.NoError(t, err)
	require.Equal(t, localDir, path)
	require.Equal(t, 1, countWarningsForSlug(logs, project.Slug))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	// The delete resolves the slug once more to find the directory to remove.
	// That resolution must still be suppressed, which is only true if the
	// eviction runs after it.
	assert.Equal(t, 1, countWarningsForSlug(logs, project.Slug),
		"the delete's own path resolution must not re-warn, and must not re-record the slug")

	// The slug is now free and taken by a new project with local content.
	seedLocal()
	_, err = srv.hubManagedProjectPath(project.Slug)
	require.NoError(t, err)
	assert.Equal(t, 2, countWarningsForSlug(logs, project.Slug),
		"deleting the project should clear its warning suppression")
}

// A renamed project keeps its state under the new slug; the old slug is
// unreachable, so its suppression entry is dead weight that would also
// mis-suppress the old slug if it were later reused by another project.
//
// Configured against gke-shared-volume on purpose. migrateProjectSlug resolves
// the old slug itself, and under any other backend that resolution never
// reaches the warning — so the cleanup could be placed before it, be dead
// wrong, and still pass. The assertion immediately after the migration is what
// pins the ordering: the migration's own resolution must find the suppression
// still in place.
func TestWarnEphemeralProjectPath_SuppressionClearedOnSlugMigration(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mountBase := t.TempDir()
	setVolumeMountBase(t, mountBase)
	require.NoError(t, os.MkdirAll(filepath.Join(mountBase, "workspace-vol"), 0755))

	srv, _ := testServer(t)
	logs := captureProjectsLog(t, srv)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "workspace-vol"},
	}

	oldSlug := "rename-ephemeral"
	newSlug := oldSlug + "-renamed"
	localDir := filepath.Join(tmpHome, ".scion", "projects", oldSlug)
	seedLocal := func() {
		require.NoError(t, os.MkdirAll(localDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(localDir, "existing.txt"), []byte("data"), 0644))
	}
	seedLocal()

	path, err := srv.hubManagedProjectPath(oldSlug)
	require.NoError(t, err)
	require.Equal(t, localDir, path)
	require.Equal(t, 1, countWarningsForSlug(logs, oldSlug))

	srv.migrateProjectSlug(t.Context(), &store.Project{
		ID:   "rename-ephemeral-project",
		Name: "Ephemeral Warn Rename",
		Slug: newSlug,
	}, oldSlug)

	// The migration resolves the old slug on its way to renaming the directory.
	// That resolution must still be suppressed, which is only true if the
	// suppression is dropped after it rather than before.
	assert.Equal(t, 1, countWarningsForSlug(logs, oldSlug),
		"the migration's own path resolution must not re-warn, and must not re-record the slug")

	// The slug is now free and taken by another project with local content.
	seedLocal()
	_, err = srv.hubManagedProjectPath(oldSlug)
	require.NoError(t, err)
	assert.Equal(t, 2, countWarningsForSlug(logs, oldSlug),
		"migrating away from a slug should clear its warning suppression")
}
