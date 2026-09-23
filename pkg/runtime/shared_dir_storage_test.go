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
	"fmt"
	"os"
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
// there must be no dynamic scion-shared-* PVC. It exercises the export-root
// guard (ValidateNotExportRoot) inside NFSSharedDirsToVolumeMounts.
//
// Note (round 1 review finding #10): this test calls NewNFSBackend(...).Resolve
// and NFSSharedDirsToVolumeMounts directly — it does not call the actual
// production caller, pkg/agent.resolveSharedDirs, which package boundaries
// prevent (pkg/runtime cannot import pkg/agent). The production wiring
// itself is pinned by the pkg/agent run.go-level tests
// (TestStartPropagatesSharedDirStorageToKubernetesRunConfig and siblings)
// and by TestResolveSharedDirs_NFS_HostBasePresent_MkdirsSharedDirs, which
// calls resolveSharedDirs end to end from the agent side.
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

	// --- Docker/Podman/Apple side: NFSSharedDirsToVolumeMounts (AC6: exercised here; the actual production caller is pkg/agent.resolveSharedDirs) ---
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

	// AC3: shared_dir_storage=nfs with no workspace_storage leaves the
	// workspace on its default EmptyDir — no workspace PVC of any kind
	// appears just because shared dirs are NFS-backed (round 1 review
	// finding #4/#6).
	wsVol := findVolume(pod, "workspace")
	require.NotNil(t, wsVol, "expected a workspace volume")
	assert.NotNil(t, wsVol.EmptyDir, "workspace volume should be EmptyDir (AC3)")
	assert.Nil(t, wsVol.PersistentVolumeClaim, "workspace volume must not be a PVC (AC3)")
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

// TestBuildPod_SharedDirStorageNFS_PerDirDetails is round 1 review finding
// #5/T5: the earlier tests only ever exercised one dir with
// ReadOnly=false/InWorkspace=false. §3.2.4 explicitly requires "honours
// ReadOnly", and per-index volume naming must not collide across multiple
// dirs. This table asserts, per dir: volume name, ClaimName, PVC.ReadOnly,
// mount.ReadOnly, SubPath, and MountPath (including the InWorkspace target).
func TestBuildPod_SharedDirStorageNFS_PerDirDetails(t *testing.T) {
	rt := newNFSTestK8sRuntime()
	cfg := RunConfig{
		Name:               "test-agent",
		Image:              "test-image",
		UnixUsername:       "scion",
		ContainerWorkspace: "/workspace",
		Labels:             map[string]string{"scion.grove": "myproject"},
		SharedDirs: []api.SharedDir{
			{Name: "scratchpad", ReadOnly: true},
			{Name: "b", InWorkspace: true},
		},
		SharedDirStorage: &SharedDirRealization{
			Backend:     "nfs",
			PVClaimName: "scion-shared",
			SubPaths: map[string]string{
				"scratchpad": "projects/pid-1/shared-dirs/scratchpad",
				"b":          "projects/pid-1/shared-dirs/b",
			},
		},
	}

	pod, err := rt.buildPod("default", cfg)
	require.NoError(t, err)

	tests := []struct {
		dirIndex     int
		wantVolName  string
		wantSubPath  string
		wantMount    string
		wantReadOnly bool
	}{
		{0, "shared-dir-0", "projects/pid-1/shared-dirs/scratchpad", "/scion-volumes/scratchpad", true},
		{1, "shared-dir-1", "projects/pid-1/shared-dirs/b", "/workspace/.scion-volumes/b", false},
	}

	for _, tc := range tests {
		t.Run(tc.wantVolName, func(t *testing.T) {
			vol := findVolume(pod, tc.wantVolName)
			require.NotNil(t, vol, "volume %s not found", tc.wantVolName)
			require.NotNil(t, vol.PersistentVolumeClaim)
			assert.Equal(t, "scion-shared", vol.PersistentVolumeClaim.ClaimName)
			assert.Equal(t, tc.wantReadOnly, vol.PersistentVolumeClaim.ReadOnly, "PVC source ReadOnly")

			mount := findVolumeMount(&pod.Spec.Containers[0], tc.wantVolName)
			require.NotNil(t, mount, "mount %s not found", tc.wantVolName)
			assert.Equal(t, tc.wantSubPath, mount.SubPath)
			assert.Equal(t, tc.wantMount, mount.MountPath)
			assert.Equal(t, tc.wantReadOnly, mount.ReadOnly, "VolumeMount ReadOnly")
		})
	}

	// Distinct volume names — a regression to a shared name would produce a
	// pod the API server rejects for duplicate volume names.
	assert.NotEqual(t, findVolume(pod, "shared-dir-0").Name, findVolume(pod, "shared-dir-1").Name)
}

// TestBuildCommonRunArgs_SharedDirStorageNFS_DoesNotAffectHostUID is part of
// AC3: with shared_dir_storage=nfs set but WorkspaceBackendName left at ""
// (the case whenever workspace_storage is unset), Docker/Podman/Apple must
// still advertise the broker's own host UID/GID — the UID/GID branch in
// buildCommonRunArgs keys only on WorkspaceBackendName, which
// shared_dir_storage never touches.
func TestBuildCommonRunArgs_SharedDirStorageNFS_DoesNotAffectHostUID(t *testing.T) {
	cfg := minimalRunConfig()
	cfg.SharedDirStorage = &SharedDirRealization{
		Backend:     "nfs",
		PVClaimName: "scion-shared",
		SubPaths:    map[string]string{"scratchpad": "projects/pid-1/shared-dirs/scratchpad"},
	}
	// WorkspaceBackendName stays "" (zero value) — shared_dir_storage does
	// not set it.

	args, err := buildCommonRunArgs(cfg)
	require.NoError(t, err)

	wantUID := fmt.Sprintf("SCION_HOST_UID=%d", os.Getuid())
	wantGID := fmt.Sprintf("SCION_HOST_GID=%d", os.Getgid())
	assertEnvInArgs(t, args, wantUID, "shared_dir_storage nfs must not affect host UID (AC3)")
	assertEnvInArgs(t, args, wantGID, "shared_dir_storage nfs must not affect host GID (AC3)")
}
