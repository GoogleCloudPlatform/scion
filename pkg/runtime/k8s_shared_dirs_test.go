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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestSharedDirPVCName(t *testing.T) {
	assert.Equal(t, "scion-shared-myproject-build-cache", sharedDirPVCName("myproject", "build-cache"))
	assert.Equal(t, "scion-shared-test-artifacts", sharedDirPVCName("test", "artifacts"))
}

func TestBuildPod_SharedDirs_DefaultMount(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.project": "myproject",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
			{Name: "artifacts", ReadOnly: true},
		},
	}

	pod, err := rt.buildPod("default", config)
	require.NoError(t, err)

	// Find shared dir volumes
	var sharedVolumes []corev1.Volume
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			sharedVolumes = append(sharedVolumes, v)
		}
	}
	require.Len(t, sharedVolumes, 2)

	// Verify PVC claim names
	assert.Equal(t, "scion-shared-myproject-build-cache", sharedVolumes[0].PersistentVolumeClaim.ClaimName)
	assert.False(t, sharedVolumes[0].PersistentVolumeClaim.ReadOnly)
	assert.Equal(t, "scion-shared-myproject-artifacts", sharedVolumes[1].PersistentVolumeClaim.ClaimName)
	assert.True(t, sharedVolumes[1].PersistentVolumeClaim.ReadOnly)

	// Verify mount paths
	var sharedMounts []corev1.VolumeMount
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/scion-volumes/build-cache" || m.MountPath == "/scion-volumes/artifacts" {
			sharedMounts = append(sharedMounts, m)
		}
	}
	require.Len(t, sharedMounts, 2)
	assert.Equal(t, "/scion-volumes/build-cache", sharedMounts[0].MountPath)
	assert.False(t, sharedMounts[0].ReadOnly)
	assert.Equal(t, "/scion-volumes/artifacts", sharedMounts[1].MountPath)
	assert.True(t, sharedMounts[1].ReadOnly)
}

func TestBuildPod_SharedDirs_InWorkspace(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.project": "myproject",
		},
		SharedDirs: []api.SharedDir{
			{Name: "workspace-cache", InWorkspace: true},
		},
	}

	pod, err := rt.buildPod("default", config)
	require.NoError(t, err)

	// Verify in-workspace mount path
	var found bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/workspace/.scion-volumes/workspace-cache" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected in-workspace mount at /workspace/.scion-volumes/workspace-cache")
}

func TestBuildPod_SharedDirs_SkipsLocalVolumesForSharedDirTargets(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.project": "myproject",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
		},
		// This volume would normally trigger a warning, but should be skipped
		// because its target matches a shared dir.
		Volumes: []api.VolumeMount{
			{Source: "/host/path/build-cache", Target: "/scion-volumes/build-cache"},
		},
	}

	pod, err := rt.buildPod("default", config)
	require.NoError(t, err)

	// The PVC volume should be present, but no duplicate volume for the local mount
	pvcCount := 0
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "scion-shared-myproject-build-cache" {
			pvcCount++
		}
	}
	assert.Equal(t, 1, pvcCount, "expected exactly one PVC volume for build-cache")
}

func TestBuildPod_NoSharedDirs(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.project": "myproject",
		},
	}

	pod, err := rt.buildPod("default", config)
	require.NoError(t, err)

	// No PVC volumes should be present
	for _, v := range pod.Spec.Volumes {
		assert.Nil(t, v.PersistentVolumeClaim, "no PVC volumes expected when no shared dirs")
	}
}

func TestCreateSharedDirPVCs(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	config := RunConfig{
		Name:  "test-agent",
		Image: "test:latest",
		Labels: map[string]string{
			"scion.project":    "myproject",
			"scion.project_id": "proj-1",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
			{Name: "artifacts", ReadOnly: true},
		},
	}

	err := rt.createSharedDirPVCs(ctx, "default", config)
	require.NoError(t, err)

	// Verify PVCs were created
	pvcList, err := clientset.CoreV1().PersistentVolumeClaims("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pvcList.Items, 2)

	pvcNames := make(map[string]bool)
	for _, pvc := range pvcList.Items {
		pvcNames[pvc.Name] = true
		// Verify the exact label set — nothing beyond the canonical project
		// labels and the shared-dir marker (no unexpected extra key survives).
		wantDir := "build-cache"
		if strings.Contains(pvc.Name, "artifacts") {
			wantDir = "artifacts"
		}
		assert.Equal(t, map[string]string{
			"scion.project":    "myproject",
			"scion.project_id": "proj-1",
			"scion.shared-dir": wantDir,
		}, pvc.Labels)
		// Verify access mode
		assert.Contains(t, pvc.Spec.AccessModes, corev1.ReadWriteMany)
		// Verify default size
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		assert.Equal(t, defaultSharedDirSize, storageReq.String())
	}

	assert.True(t, pvcNames["scion-shared-myproject-build-cache"])
	assert.True(t, pvcNames["scion-shared-myproject-artifacts"])
}

func TestCreateSharedDirPVCs_CustomStorageClassAndSize(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	config := RunConfig{
		Name:  "test-agent",
		Image: "test:latest",
		Labels: map[string]string{
			"scion.project": "myproject",
		},
		SharedDirs: []api.SharedDir{
			{Name: "data"},
		},
		Kubernetes: &api.KubernetesConfig{
			SharedDirStorageClass: "standard-rwx",
			SharedDirSize:         "50Gi",
		},
	}

	err := rt.createSharedDirPVCs(ctx, "default", config)
	require.NoError(t, err)

	pvc, err := clientset.CoreV1().PersistentVolumeClaims("default").Get(ctx, "scion-shared-myproject-data", metav1.GetOptions{})
	require.NoError(t, err)

	assert.Equal(t, "standard-rwx", *pvc.Spec.StorageClassName)
	storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, "50Gi", storageReq.String())
}

func TestCreateSharedDirPVCs_ReusesExisting(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	// Pre-create a PVC
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scion-shared-myproject-build-cache",
			Namespace: "default",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
	}
	_, err := clientset.CoreV1().PersistentVolumeClaims("default").Create(ctx, existingPVC, metav1.CreateOptions{})
	require.NoError(t, err)

	config := RunConfig{
		Name:  "test-agent",
		Image: "test:latest",
		Labels: map[string]string{
			"scion.project": "myproject",
		},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
		},
	}

	// Should not error, should reuse the existing PVC
	err = rt.createSharedDirPVCs(ctx, "default", config)
	require.NoError(t, err)

	// Verify still only one PVC
	pvcList, err := clientset.CoreV1().PersistentVolumeClaims("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, pvcList.Items, 1)
}

func TestCreateSharedDirPVCs_NoSharedDirs(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	ctx := context.Background()

	config := RunConfig{
		Name:  "test-agent",
		Image: "test:latest",
		Labels: map[string]string{
			"scion.project": "myproject",
		},
	}

	err := rt.createSharedDirPVCs(ctx, "default", config)
	require.NoError(t, err)
}

func TestCreateSharedDirPVCs_MissingProjectLabel(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	ctx := context.Background()

	config := RunConfig{
		Name:   "test-agent",
		Image:  "test:latest",
		Labels: map[string]string{},
		SharedDirs: []api.SharedDir{
			{Name: "build-cache"},
		},
	}

	err := rt.createSharedDirPVCs(ctx, "default", config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scion.project label")
}

// TestCreateSharedDirPVCs_SettingsResolvedClassAndSize feeds createSharedDirPVCs
// the Kubernetes block the start path builds from settings defaults
// (config.ApplySharedDirDefaults): the template's explicit size wins, the
// settings class fills the gap, and an unset size keeps the 10Gi default
// (ptone/scion#2634).
func TestCreateSharedDirPVCs_SettingsResolvedClassAndSize(t *testing.T) {
	tests := []struct {
		name      string
		base      *api.KubernetesConfig
		class     string
		size      string
		wantClass string
		wantSize  string
	}{
		{"settings class and size, no template block", nil, "standard-rwx", "1Ti", "standard-rwx", "1Ti"},
		{"template size wins, settings class fills", &api.KubernetesConfig{SharedDirSize: "20Gi"}, "standard-rwx", "1Ti", "standard-rwx", "20Gi"},
		{"template class wins", &api.KubernetesConfig{SharedDirStorageClass: "tpl-rwx"}, "standard-rwx", "", "tpl-rwx", defaultSharedDirSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			ctx := context.Background()
			cfg := RunConfig{
				Name:       "test-agent",
				Image:      "test:latest",
				Labels:     map[string]string{"scion.project": "myproject"},
				SharedDirs: []api.SharedDir{{Name: "data"}},
				Kubernetes: config.ApplySharedDirDefaults(tt.base, tt.class, tt.size),
			}
			require.NoError(t, rt.createSharedDirPVCs(ctx, "default", cfg))

			pvc, err := clientset.CoreV1().PersistentVolumeClaims("default").Get(ctx, "scion-shared-myproject-data", metav1.GetOptions{})
			require.NoError(t, err)
			require.NotNil(t, pvc.Spec.StorageClassName)
			assert.Equal(t, tt.wantClass, *pvc.Spec.StorageClassName)
			storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			assert.Equal(t, tt.wantSize, storageReq.String())
		})
	}
}

// Reusing an existing claim whose storage class differs from the requested
// one logs a warning naming the claim, both classes and (when Pending) the
// phase; a matching class or no requested class logs nothing.
func TestCreateSharedDirPVCs_ReuseWarnsOnStorageClassMismatch(t *testing.T) {
	oldLog := runtimeLog
	defer func() { runtimeLog = oldLog }()

	strPtr := func(s string) *string { return &s }
	tests := []struct {
		name          string
		existingClass *string
		phase         corev1.PersistentVolumeClaimPhase
		requested     string
		wantWarn      bool
	}{
		{"differs, pending", strPtr("standard"), corev1.ClaimPending, "standard-rwx", true},
		{"differs, bound", strPtr("standard"), corev1.ClaimBound, "standard-rwx", true},
		{"existing unset", nil, corev1.ClaimBound, "standard-rwx", true},
		{"same class", strPtr("standard-rwx"), corev1.ClaimBound, "standard-rwx", false},
		{"no class requested", strPtr("standard"), corev1.ClaimBound, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			runtimeLog = slog.New(slog.NewTextHandler(&buf, nil))

			rt, clientset, _ := newTestK8sRuntime()
			ctx := context.Background()
			_, err := clientset.CoreV1().PersistentVolumeClaims("default").Create(ctx, &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "scion-shared-myproject-build-cache", Namespace: "default"},
				Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: tt.existingClass},
				Status:     corev1.PersistentVolumeClaimStatus{Phase: tt.phase},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			cfg := RunConfig{
				Labels:     map[string]string{"scion.project": "myproject"},
				SharedDirs: []api.SharedDir{{Name: "build-cache"}},
				Kubernetes: &api.KubernetesConfig{SharedDirStorageClass: tt.requested},
			}
			require.NoError(t, rt.createSharedDirPVCs(ctx, "default", cfg))

			out := buf.String()
			if !tt.wantWarn {
				assert.NotContains(t, out, "level=WARN")
				return
			}
			assert.Contains(t, out, "level=WARN")
			assert.Contains(t, out, "pvc=scion-shared-myproject-build-cache")
			assert.Contains(t, out, "requested_storage_class="+tt.requested)
			if tt.existingClass != nil {
				assert.Contains(t, out, "existing_storage_class="+*tt.existingClass)
			}
			if tt.phase == corev1.ClaimPending {
				assert.Contains(t, out, "phase=Pending")
			} else {
				assert.NotContains(t, out, "phase=")
			}
		})
	}
}

// SharedDirClaimExists looks up the PVC createSharedDirPVCs creates or
// reuses, by name, in the namespace Run would use; it never creates one.
func TestSharedDirClaimExists(t *testing.T) {
	ctx := context.Background()
	labels := map[string]string{"scion.project": "myproject", "scion.project_id": "proj-1"}

	t.Run("missing then created", func(t *testing.T) {
		rt, clientset, _ := newTestK8sRuntime()
		rt.DefaultNamespace = "default"
		exists, err := rt.SharedDirClaimExists(ctx, labels, "notes")
		require.NoError(t, err)
		assert.False(t, exists)
		pvcs, err := clientset.CoreV1().PersistentVolumeClaims("default").List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, pvcs.Items, "the lookup creates nothing")

		require.NoError(t, rt.createSharedDirPVCs(ctx, "default", RunConfig{Labels: labels, SharedDirs: []api.SharedDir{{Name: "notes"}}}))
		exists, err = rt.SharedDirClaimExists(ctx, labels, "notes")
		require.NoError(t, err)
		assert.True(t, exists)
		exists, err = rt.SharedDirClaimExists(ctx, labels, "other")
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("namespace label", func(t *testing.T) {
		rt, _, _ := newTestK8sRuntime()
		rt.DefaultNamespace = "default"
		require.NoError(t, rt.createSharedDirPVCs(ctx, "team-a", RunConfig{Labels: labels, SharedDirs: []api.SharedDir{{Name: "notes"}}}))
		exists, err := rt.SharedDirClaimExists(ctx, labels, "notes")
		require.NoError(t, err)
		assert.False(t, exists, "the claim is in another namespace")
		withNS := map[string]string{"scion.project": "myproject", "scion.namespace": "team-a"}
		exists, err = rt.SharedDirClaimExists(ctx, withNS, "notes")
		require.NoError(t, err)
		assert.True(t, exists)
	})

	t.Run("get error", func(t *testing.T) {
		rt, clientset, _ := newTestK8sRuntime()
		clientset.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, errors.New("forbidden")
		})
		_, err := rt.SharedDirClaimExists(ctx, labels, "notes")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scion-shared-myproject-notes")
	})

	t.Run("no project label", func(t *testing.T) {
		rt, _, _ := newTestK8sRuntime()
		_, err := rt.SharedDirClaimExists(ctx, map[string]string{}, "notes")
		require.Error(t, err)
	})
}
