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
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSharedDirPVCName(t *testing.T) {
	assert.Equal(t, "scion-shared-mygrove-build-cache", sharedDirPVCName("mygrove", "build-cache"))
	assert.Equal(t, "scion-shared-test-artifacts", sharedDirPVCName("test", "artifacts"))
}

func TestBuildPod_SharedDirs_DefaultMount(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.grove": "mygrove",
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
	assert.Equal(t, "scion-shared-mygrove-build-cache", sharedVolumes[0].PersistentVolumeClaim.ClaimName)
	assert.False(t, sharedVolumes[0].PersistentVolumeClaim.ReadOnly)
	assert.Equal(t, "scion-shared-mygrove-artifacts", sharedVolumes[1].PersistentVolumeClaim.ClaimName)
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
			"scion.grove": "mygrove",
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
			"scion.grove": "mygrove",
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
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "scion-shared-mygrove-build-cache" {
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
			"scion.grove": "mygrove",
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
			"scion.grove": "mygrove",
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
		// Verify labels
		assert.Equal(t, "mygrove", pvc.Labels["scion.grove"])
		assert.NotEmpty(t, pvc.Labels["scion.shared-dir"])
		// Verify access mode
		assert.Contains(t, pvc.Spec.AccessModes, corev1.ReadWriteMany)
		// Verify default size
		storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		assert.Equal(t, defaultSharedDirSize, storageReq.String())
	}

	assert.True(t, pvcNames["scion-shared-mygrove-build-cache"])
	assert.True(t, pvcNames["scion-shared-mygrove-artifacts"])
}

func TestCreateSharedDirPVCs_CustomStorageClassAndSize(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	config := RunConfig{
		Name:  "test-agent",
		Image: "test:latest",
		Labels: map[string]string{
			"scion.grove": "mygrove",
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

	pvc, err := clientset.CoreV1().PersistentVolumeClaims("default").Get(ctx, "scion-shared-mygrove-data", metav1.GetOptions{})
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
			Name:      "scion-shared-mygrove-build-cache",
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
			"scion.grove": "mygrove",
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
			"scion.grove": "mygrove",
		},
	}

	err := rt.createSharedDirPVCs(ctx, "default", config)
	require.NoError(t, err)
}

func TestCreateSharedDirPVCs_MissingGroveLabel(t *testing.T) {
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
	assert.Contains(t, err.Error(), "missing scion.project or scion.grove label")
}

func TestCleanupSharedDirPVCs(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	// Create PVCs with grove labels
	for _, name := range []string{"scion-shared-mygrove-cache", "scion-shared-mygrove-data"} {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					"scion.grove":      "mygrove",
					"scion.shared-dir": "test",
				},
			},
		}
		_, err := clientset.CoreV1().PersistentVolumeClaims("default").Create(ctx, pvc, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	rt.cleanupSharedDirPVCs(ctx, "default", "mygrove")

	pvcList, err := clientset.CoreV1().PersistentVolumeClaims("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pvcList.Items)
}
