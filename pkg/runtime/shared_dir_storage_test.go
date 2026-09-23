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

package runtime

import (
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSharedDirStorage_DockerAndK8s_SameLayout is test (a) from design
// deploy-config-explore §7: it inverts the repro in findings/code-surface.md
// ("Reproduction"). With one shared_dir_storage nfs settings block, the
// Docker bind-mount source and the K8s PVC subPath must resolve to the same
// projects/<pid>/shared-dirs/scratchpad path under the same base (AC2), and
// there must be no dynamic scion-shared-* PVC. This is the production caller
// AC6 requires for NFSSharedDirsToVolumeMounts, and it exercises the
// export-root guard (ValidateNotExportRoot) inside it.
func TestSharedDirStorage_DockerAndK8s_SameLayout(t *testing.T) {
	sdCfg := &config.V1SharedDirStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "/srv",
			Shares: []config.V1NFSShare{
				{ID: "scion-shared", Server: "10.128.15.241", Export: "/srv/scion-shared", PVName: "scion-shared"},
			},
		},
	}
	projectID := "pid-abc123"
	dirs := []api.SharedDir{{Name: "scratchpad"}}

	// --- Resolve, exactly as pkg/agent.resolveSharedDirs does for both runtimes ---
	res, err := NewNFSBackend(sdCfg.NFS).Resolve(ResolveInput{
		ProjectID:      projectID,
		SharedDirNames: []string{"scratchpad"},
	})
	require.NoError(t, err)

	wantHostBase := filepath.Join("/srv", "scion-shared")
	wantRel := filepath.Join("projects", projectID, "shared-dirs", "scratchpad")
	assert.Equal(t, wantHostBase, res.HostBase)
	assert.Equal(t, wantRel, res.SharedDirs["scratchpad"].ServerRelativePath)

	// --- Docker/Podman/Apple side: NFSSharedDirsToVolumeMounts (production caller, AC6) ---
	mounts, err := NFSSharedDirsToVolumeMounts(res, dirs, "/workspace")
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	wantDockerSource := filepath.Join(wantHostBase, wantRel)
	assert.Equal(t, wantDockerSource, mounts[0].Source, "docker bind source")
	assert.Equal(t, "/scion-volumes/scratchpad", mounts[0].Target)
	assert.False(t, mounts[0].ReadOnly)

	// --- K8s side: buildPod with the SharedDirRealization run.go would populate ---
	rt := newNFSTestK8sRuntime()
	runConfig := RunConfig{
		Name:         "test-agent",
		Image:        "test-image",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.grove": "myproject",
		},
		SharedDirs: dirs,
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: sdCfg.NFS.Shares[0].PVName,
			SubPaths: map[string]string{
				"scratchpad": res.SharedDirs["scratchpad"].ServerRelativePath,
			},
		},
	}

	pod, err := rt.buildPod("default", runConfig)
	require.NoError(t, err)

	sdVol := findVolume(pod, "shared-dir-0")
	require.NotNil(t, sdVol, "expected shared-dir-0 volume")
	require.NotNil(t, sdVol.PersistentVolumeClaim)
	assert.Equal(t, "scion-shared", sdVol.PersistentVolumeClaim.ClaimName)

	sdMount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-0")
	require.NotNil(t, sdMount, "expected shared-dir-0 mount")
	assert.Equal(t, wantRel, sdMount.SubPath, "k8s subPath")
	assert.Equal(t, "/scion-volumes/scratchpad", sdMount.MountPath)

	// Same relative path under the same base for both runtimes (AC2, the
	// success criterion of test (a)).
	assert.Equal(t, wantDockerSource, filepath.Join(wantHostBase, sdMount.SubPath),
		"docker source and k8s (base+subPath) must resolve to the same path")

	// No dynamic scion-shared-<project>-<dir> PVCs are created (AC2).
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			assert.Equal(t, "scion-shared", v.PersistentVolumeClaim.ClaimName,
				"only the static shared PVC claim should appear, no dynamic per-dir PVCs")
		}
	}
}

// TestCreateSharedDirPVCs_SharedDirStorageNFS_SkipsPVCCreation is part of
// AC2 ("no scion-shared-* dynamic PVCs"): createSharedDirPVCs must early
// return when server.shared_dir_storage.backend is "nfs" (design §3.2.4).
func TestCreateSharedDirPVCs_SharedDirStorageNFS_SkipsPVCCreation(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := t.Context()

	cfg := RunConfig{
		Name:  "test-agent",
		Image: "test:latest",
		Labels: map[string]string{
			"scion.grove": "myproject",
		},
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: "scion-shared",
			SubPaths:    map[string]string{"build-cache": "projects/pid/shared-dirs/build-cache"},
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
		},
	}

	err := rt.createSharedDirPVCs(ctx, "default", cfg)
	require.NoError(t, err)

	pvcList, err := clientset.CoreV1().PersistentVolumeClaims("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pvcList.Items, "no dynamic PVCs expected when shared_dir_storage backend is nfs")
}

// TestBuildPod_SharedDirStorageNFS_EmptyPVClaimName_FailsClosed is test (c)
// / AC4: an empty pv_name must produce an error naming pv_name rather than
// silently falling back to an unclaimed/EmptyDir volume.
func TestBuildPod_SharedDirStorageNFS_EmptyPVClaimName_FailsClosed(t *testing.T) {
	rt := newNFSTestK8sRuntime()
	cfg := RunConfig{
		Name:         "test-agent",
		Image:        "test-image",
		UnixUsername: "scion",
		Labels:       map[string]string{"scion.grove": "myproject"},
		SharedDirs:   []api.SharedDir{{Name: "scratchpad"}},
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: "", // misconfigured: pv_name missing
			SubPaths:    map[string]string{"scratchpad": "projects/pid/shared-dirs/scratchpad"},
		},
	}

	_, err := rt.buildPod("default", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pv_name", "error should name the missing field (design G5/AC4)")
}

// TestBuildPod_SharedDirStorageNFS_MissingSubPath_FailsClosed covers the
// companion fail-closed case: a shared dir with no resolved subPath (e.g. a
// broker-local shared_dirs override naming a dir the hub never resolved)
// must error rather than mount an unscoped path.
func TestBuildPod_SharedDirStorageNFS_MissingSubPath_FailsClosed(t *testing.T) {
	rt := newNFSTestK8sRuntime()
	cfg := RunConfig{
		Name:         "test-agent",
		Image:        "test-image",
		UnixUsername: "scion",
		Labels:       map[string]string{"scion.grove": "myproject"},
		SharedDirs:   []api.SharedDir{{Name: "scratchpad"}},
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: "scion-shared",
			SubPaths:    map[string]string{}, // "scratchpad" never resolved
		},
	}

	_, err := rt.buildPod("default", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scratchpad")
	assert.Contains(t, err.Error(), "subPath")
}

// TestBuildPod_SharedDirStorageNFS_PrecedesWorkspaceStorageNFS is test (d):
// shared_dir_storage nfs must take precedence over the existing
// workspace_storage:nfs shared-dir branch when both are set (design §3.2.4).
func TestBuildPod_SharedDirStorageNFS_PrecedesWorkspaceStorageNFS(t *testing.T) {
	rt := newNFSTestK8sRuntime()
	cfg := RunConfig{
		Name:                 "test-agent",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		Labels:               map[string]string{"scion.grove": "myproject"},
		SharedDirs:           []api.SharedDir{{Name: "scratchpad"}},
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: "scion-shared",
			SubPaths:    map[string]string{"scratchpad": "projects/pid-999/shared-dirs/scratchpad"},
		},
	}

	pod, err := rt.buildPod("default", cfg)
	require.NoError(t, err)

	sdVol := findVolume(pod, "shared-dir-0")
	require.NotNil(t, sdVol)
	require.NotNil(t, sdVol.PersistentVolumeClaim)
	assert.Equal(t, "scion-shared", sdVol.PersistentVolumeClaim.ClaimName,
		"shared_dir_storage nfs must win over workspace_storage:nfs for shared dirs")

	sdMount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-0")
	require.NotNil(t, sdMount)
	assert.Equal(t, "projects/pid-999/shared-dirs/scratchpad", sdMount.SubPath)

	// createSharedDirPVCs must also skip PVC creation under this precedence.
	rt2, clientset, _ := newTestK8sRuntime()
	err = rt2.createSharedDirPVCs(t.Context(), "default", cfg)
	require.NoError(t, err)
	pvcList, err := clientset.CoreV1().PersistentVolumeClaims("default").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pvcList.Items)
}

// TestBuildPod_SharedDirStorageNFS_Unset_UnaffectedLocalBehavior is test (b)
// at the K8s layer / AC1: when RunConfig.SharedDirStorage is nil, buildPod's
// shared-dir handling is unchanged (local per-dir PVCs).
func TestBuildPod_SharedDirStorageNFS_Unset_UnaffectedLocalBehavior(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := RunConfig{
		Name:         "test-agent",
		Image:        "test-image",
		UnixUsername: "scion",
		Labels:       map[string]string{"scion.grove": "myproject"},
		SharedDirs:   []api.SharedDir{{Name: "build-cache"}},
	}

	pod, err := rt.buildPod("default", cfg)
	require.NoError(t, err)

	sdVol := findVolume(pod, "shared-dir-0")
	require.NotNil(t, sdVol)
	require.NotNil(t, sdVol.PersistentVolumeClaim)
	assert.Equal(t, "scion-shared-myproject-build-cache", sdVol.PersistentVolumeClaim.ClaimName)
	assert.Empty(t, findVolumeMount(&pod.Spec.Containers[0], "shared-dir-0").SubPath,
		"local per-dir PVCs are mounted without a subPath")
}
