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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mixedRealization serves notes from the shared claim; gocache is local.
func mixedRealization() *SharedDirRealization {
	return &SharedDirRealization{
		Backend:     "nfs",
		PVClaimName: "scion-shared",
		SubPaths:    map[string]string{"notes": "projects/pid-1/shared-dirs/notes"},
		LocalDirs:   map[string]bool{"gocache": true},
	}
}

func TestSharedDirRealization_Serves(t *testing.T) {
	var nilR *SharedDirRealization
	assert.False(t, nilR.Serves("notes"))
	assert.False(t, (&SharedDirRealization{Backend: "local"}).Serves("notes"))
	r := mixedRealization()
	assert.True(t, r.Serves("notes"))
	assert.False(t, r.Serves("gocache"))
	assert.True(t, r.Serves("unresolved"), "a dir not named local is served from nfs, so a missing subPath fails closed")
}

// Per-dir backends: notes is mounted from the shared claim by subPath and
// gocache from its own per-dir PVC.
func TestBuildPod_SharedDirStoragePerDir_Mixed(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := RunConfig{
		Name:             "test-agent",
		Image:            "test-image",
		UnixUsername:     "scion",
		Labels:           map[string]string{"scion.project": "myproject"},
		SharedDirs:       []api.SharedDir{{Name: "gocache"}, {Name: "notes"}},
		SharedDirStorage: mixedRealization(),
	}
	pod, err := rt.buildPod("default", cfg)
	require.NoError(t, err)

	cacheVol := findVolume(pod, "shared-dir-0")
	require.NotNil(t, cacheVol)
	require.NotNil(t, cacheVol.PersistentVolumeClaim)
	assert.Equal(t, "scion-shared-myproject-gocache", cacheVol.PersistentVolumeClaim.ClaimName)
	cacheMount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-0")
	require.NotNil(t, cacheMount)
	assert.Empty(t, cacheMount.SubPath)
	assert.Equal(t, "/scion-volumes/gocache", cacheMount.MountPath)

	notesVol := findVolume(pod, "shared-dir-1")
	require.NotNil(t, notesVol)
	require.NotNil(t, notesVol.PersistentVolumeClaim)
	assert.Equal(t, "scion-shared", notesVol.PersistentVolumeClaim.ClaimName)
	notesMount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-1")
	require.NotNil(t, notesMount)
	assert.Equal(t, "projects/pid-1/shared-dirs/notes", notesMount.SubPath)
}

// With an nfs workspace, a local dir is mounted from the workspace claim,
// as without shared_dir_storage, and gets an init container mount at its
// own volume index; the nfs dir gets none.
func TestBuildPod_SharedDirStoragePerDir_MixedWithNFSWorkspace(t *testing.T) {
	rt := newNFSTestK8sRuntime()
	cfg := RunConfig{
		Name:                 "test-agent",
		Image:                "test-image",
		UnixUsername:         "scion",
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		Labels:               map[string]string{"scion.project": "myproject"},
		SharedDirs:           []api.SharedDir{{Name: "notes"}, {Name: "gocache"}},
		SharedDirStorage:     mixedRealization(),
	}
	pod, err := rt.buildPod("default", cfg)
	require.NoError(t, err)

	notesVol := findVolume(pod, "shared-dir-0")
	require.NotNil(t, notesVol)
	assert.Equal(t, "scion-shared", notesVol.PersistentVolumeClaim.ClaimName)
	cacheVol := findVolume(pod, "shared-dir-1")
	require.NotNil(t, cacheVol)
	assert.Equal(t, "scion-workspaces", cacheVol.PersistentVolumeClaim.ClaimName)
	cacheMount := findVolumeMount(&pod.Spec.Containers[0], "shared-dir-1")
	require.NotNil(t, cacheMount)
	wantSubPath, err := nfsSharedDirSubPath(cfg.NFSSubPath, "gocache")
	require.NoError(t, err)
	assert.Equal(t, wantSubPath, cacheMount.SubPath)

	mounts, err := nfsSharedDirInitMounts(cfg)
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	assert.Equal(t, "gocache", mounts[0].Name)
	assert.Equal(t, "shared-dir-1", mounts[0].Mount.Name, "the init mount keeps the pod volume index")
	assert.Equal(t, wantSubPath, mounts[0].Mount.SubPath)
}

// Every dir on nfs: no init container mounts, exactly as before.
func TestNFSSharedDirInitMounts_AllServedIsNil(t *testing.T) {
	cfg := RunConfig{
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/proj-123/workspace",
		SharedDirs:           []api.SharedDir{{Name: "notes"}},
		SharedDirStorage:     &SharedDirRealization{Backend: "nfs", PVClaimName: "scion-shared", SubPaths: map[string]string{"notes": "x"}},
	}
	mounts, err := nfsSharedDirInitMounts(cfg)
	require.NoError(t, err)
	assert.Nil(t, mounts)
}

// Only the local dir gets a per-dir PVC.
func TestCreateSharedDirPVCs_SharedDirStoragePerDir_OnlyLocalDirs(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	cfg := RunConfig{
		Name:             "test-agent",
		Labels:           map[string]string{"scion.project": "myproject"},
		SharedDirs:       []api.SharedDir{{Name: "notes"}, {Name: "gocache"}},
		SharedDirStorage: mixedRealization(),
	}
	require.NoError(t, rt.createSharedDirPVCs(t.Context(), "default", cfg))
	pvcs, err := clientset.CoreV1().PersistentVolumeClaims("default").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pvcs.Items, 1)
	assert.Equal(t, "scion-shared-myproject-gocache", pvcs.Items[0].Name)
}
