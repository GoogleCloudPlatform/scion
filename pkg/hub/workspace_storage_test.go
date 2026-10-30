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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend: "local",
	}
	assert.True(t, srv.workspaceWriteBlocked())
}

func TestWorkspaceWriteBlocked_NFSBackendOnCloudRun(t *testing.T) {
	srv, _ := testServer(t)
	t.Setenv("K_SERVICE", "hub-service")

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  "workspace-vol",
			PVClaimName: "scion-workspaces",
		},
	}
	assert.False(t, srv.workspaceWriteBlocked())
}

// A volume backend without a volume name has no mount point, so project
// paths fall back to ephemeral local storage: writes must be blocked, the same
// way for both volume backends (ptone/scion#1073).
func TestWorkspaceWriteBlocked_VolumeBackendMissingVolumeName(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.V1WorkspaceStorageConfig
	}{
		{
			name: "cloudrun-volume",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend:        "cloudrun-volume",
				CloudRunVolume: &config.V1CloudRunVolumeConfig{SubPathRoot: "projects"},
			},
		},
		{
			name: "cloudrun-volume nil block",
			cfg:  &config.V1WorkspaceStorageConfig{Backend: "cloudrun-volume"},
		},
		{
			name: "gke-shared-volume",
			cfg: &config.V1WorkspaceStorageConfig{
				Backend:         "gke-shared-volume",
				GKESharedVolume: &config.V1GKESharedVolumeConfig{PVClaimName: "scion-workspaces"},
			},
		},
		{
			name: "gke-shared-volume nil block",
			cfg:  &config.V1WorkspaceStorageConfig{Backend: "gke-shared-volume"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := testServer(t)
			t.Setenv("K_SERVICE", "hub-service")
			srv.config.ProjectStorageConfig = projectStorageFromWorkspace(tt.cfg)
			assert.True(t, srv.workspaceWriteBlocked())
		})
	}
}

func TestWorkspaceWriteBlocked_NotOnCloudRun(t *testing.T) {
	srv, _ := testServer(t)

	// K_SERVICE not set → not on Cloud Run → writes allowed
	t.Setenv("K_SERVICE", "")
	assert.False(t, srv.workspaceWriteBlocked())
}

// ============================================================================
// Cloud Run Instance (Tier 0 ephemeral) Tests
// ============================================================================

func TestIsCloudRunInstance_Set(t *testing.T) {
	t.Setenv("CLOUD_RUN_INSTANCE", "my-instance")
	assert.True(t, isCloudRunInstance())
}

func TestIsCloudRunInstance_Unset(t *testing.T) {
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	assert.False(t, isCloudRunInstance())
}

func TestWorkspaceWriteBlocked_CloudRunInstance(t *testing.T) {
	srv, _ := testServer(t)

	// Cloud Run Instance: CLOUD_RUN_INSTANCE is set, K_SERVICE is NOT set.
	// Writes should be explicitly permitted (ephemeral storage, Tier 0).
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "my-instance")

	assert.False(t, srv.workspaceWriteBlocked())
}

func TestWorkspaceWriteBlocked_CloudRunInstanceEvenWithLocalBackend(t *testing.T) {
	srv, _ := testServer(t)

	// Even with an explicit "local" backend config, CRI should permit writes.
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "my-instance")

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend: "local",
	}
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

	// Configure NFS backend (mounted) — writes are allowed and land in the
	// uuid tree agents mount.
	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755))
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(mountRoot))

	project, _ := createTestHubManagedProject(t, srv, "Gate NFS Test")

	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files/test.txt", project.ID),
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(mountRoot, "share1", "projects", project.ID, "workspace", "test.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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
	// Critical check: /readyz 503s on it, so /healthz reports unhealthy too.
	assert.Equal(t, "unhealthy", resp.Status)
}

func TestReadiness_NFSUnavailable(t *testing.T) {
	srv, _ := testServer(t)

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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
			srv.config.ProjectStorageConfig = projectStorageFromWorkspace(tt.cfg)

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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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

	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
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
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(wsCfg)

	project := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000aa", Slug: "restart-test"}
	res, err := srv.projectWorkspaceResolved(project)
	require.NoError(t, err)
	path := res.HostPath
	assert.Equal(t, filepath.Join(shareDir, "projects", project.ID, "workspace"), path, "the uuid tree agents mount")

	// Write a file to the "NFS" path
	require.NoError(t, os.MkdirAll(path, 0755))
	testContent := []byte("this data should survive a restart")
	require.NoError(t, os.WriteFile(filepath.Join(path, "persistent.txt"), testContent, 0644))

	// "Restart" — create a new server instance with the same config
	srv2, _ := testServer(t)
	srv2.config.ProjectStorageConfig = projectStorageFromWorkspace(wsCfg)

	// Verify the file is still accessible from the new instance
	res2, err := srv2.projectWorkspaceResolved(project)
	require.NoError(t, err)
	path2 := res2.HostPath
	assert.Equal(t, path, path2, "path should be the same across restarts")

	data, err := os.ReadFile(filepath.Join(path2, "persistent.txt"))
	require.NoError(t, err)
	assert.Equal(t, testContent, data, "file content should survive simulated restart")
}
