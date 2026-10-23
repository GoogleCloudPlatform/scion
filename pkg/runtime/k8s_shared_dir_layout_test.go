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

func TestSharedDirRealization_Shared(t *testing.T) {
	var nilR *SharedDirRealization
	assert.False(t, nilR.Shared())
	assert.False(t, (&SharedDirRealization{Backend: "local"}).Shared())
	assert.False(t, (&SharedDirRealization{}).Shared())
	for _, b := range []string{"nfs", "gke-shared-volume", "cloudrun-volume"} {
		assert.True(t, (&SharedDirRealization{Backend: b}).Shared(), b)
	}
}

// buildPod has one shared-dir branch for every shared backend (design
// fs-cleanup §P2): each dir is mounted from the share's claim by its
// resolved subPath, whether the share is the nfs workspace claim or a
// gke-shared-volume claim, and no per-dir claim is created.
func TestBuildPod_SharedDirLayout_OneSubPathBranch(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg   RunConfig
		claim string
	}{
		"nfs workspace claim": {
			cfg: RunConfig{
				WorkspaceBackendName: "nfs",
				NFSPVClaimName:       "scion-workspaces",
				NFSSubPath:           "projects/pid-1/workspace",
				SharedDirStorage:     sharedDirsOnClaim("nfs", "scion-workspaces", "projects/pid-1", "notes", "cache"),
			},
			claim: "scion-workspaces",
		},
		"gke-shared-volume": {
			cfg:   RunConfig{SharedDirStorage: sharedDirsOnClaim("gke-shared-volume", "filestore-claim", "projects/pid-1", "notes", "cache")},
			claim: "filestore-claim",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rt := newNFSTestK8sRuntime()
			cfg := tc.cfg
			cfg.Name, cfg.Image, cfg.UnixUsername = "test-agent", "test-image", "scion"
			cfg.Labels = map[string]string{"scion.project": "myproject"}
			cfg.SharedDirs = []api.SharedDir{{Name: "notes"}, {Name: "cache", ReadOnly: true, InWorkspace: true}}
			pod, err := rt.buildPod("default", cfg)
			require.NoError(t, err)

			for i, want := range []struct{ target, subPath string }{
				{"/scion-volumes/notes", "projects/pid-1/shared-dirs/notes"},
				{"/workspace/.scion-volumes/cache", "projects/pid-1/shared-dirs/cache"},
			} {
				volName := []string{"shared-dir-0", "shared-dir-1"}[i]
				vol := findVolume(pod, volName)
				require.NotNil(t, vol, volName)
				require.NotNil(t, vol.PersistentVolumeClaim, volName)
				assert.Equal(t, tc.claim, vol.PersistentVolumeClaim.ClaimName, volName)
				m := findVolumeMount(&pod.Spec.Containers[0], volName)
				require.NotNil(t, m, volName)
				assert.Equal(t, want.subPath, m.SubPath, volName)
				assert.Equal(t, want.target, m.MountPath, volName)
				assert.Equal(t, i == 1, m.ReadOnly, volName)
			}

			rt2, clientset, _ := newTestK8sRuntime()
			require.NoError(t, rt2.createSharedDirPVCs(t.Context(), "default", cfg))
			pvcs, err := clientset.CoreV1().PersistentVolumeClaims("default").List(t.Context(), metav1.ListOptions{})
			require.NoError(t, err)
			assert.Empty(t, pvcs.Items, "a shared backend creates no per-dir claim")
		})
	}
}

// A shared backend without a claim (for example cloudrun-volume, which
// Kubernetes cannot mount) fails closed instead of falling back to per-dir
// claims.
func TestBuildPod_SharedDirLayout_NoClaimFailsClosed(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	cfg := RunConfig{
		Name: "test-agent", Image: "test-image", UnixUsername: "scion",
		Labels:           map[string]string{"scion.project": "myproject"},
		SharedDirs:       []api.SharedDir{{Name: "notes"}},
		SharedDirStorage: sharedDirsOnClaim("cloudrun-volume", "", "projects/pid-1", "notes"),
	}
	_, err := rt.buildPod("default", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no claim")
}

// The workspace-provision init container mirrors the shared dirs that are
// on the workspace claim (F-111), at their pod volume index, and none on
// another claim (the legacy split shared-dir export).
func TestNFSSharedDirInitMounts_OnlyOnWorkspaceClaim(t *testing.T) {
	cfg := RunConfig{
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "scion-workspaces",
		NFSSubPath:           "projects/pid-1/workspace",
		SharedDirs:           []api.SharedDir{{Name: "notes"}, {Name: "cache"}},
		SharedDirStorage:     sharedDirsOnClaim("nfs", "scion-workspaces", "projects/pid-1", "notes", "cache"),
	}
	mounts, err := nfsSharedDirInitMounts(cfg)
	require.NoError(t, err)
	require.Len(t, mounts, 2)
	assert.Equal(t, "cache", mounts[1].Name)
	assert.Equal(t, "shared-dir-1", mounts[1].Mount.Name)
	assert.Equal(t, "projects/pid-1/shared-dirs/cache", mounts[1].Mount.SubPath)

	cfg.SharedDirStorage = sharedDirsOnClaim("nfs", "scion-shared", "projects/pid-1", "notes", "cache")
	mounts, err = nfsSharedDirInitMounts(cfg)
	require.NoError(t, err)
	assert.Nil(t, mounts)

	cfg.SharedDirStorage = nil
	mounts, err = nfsSharedDirInitMounts(cfg)
	require.NoError(t, err)
	assert.Nil(t, mounts, "local backend: no init mounts")
}
