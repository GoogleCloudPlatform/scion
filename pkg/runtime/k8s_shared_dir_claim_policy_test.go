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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSharedDirClaimPolicy_CreateSpec(t *testing.T) {
	class, err := sharedDirClaimPolicy{Class: "standard-rwx"}.createSpec()
	require.NoError(t, err)
	require.NotNil(t, class)
	assert.Equal(t, "standard-rwx", *class)

	class, err = sharedDirClaimPolicy{}.createSpec()
	assert.ErrorIs(t, err, errSharedDirClaimUnresolved)
	assert.Nil(t, class)

	assert.Equal(t, sharedDirClaimPolicy{}, sharedDirClaimPolicyFor(nil))
	assert.Equal(t, sharedDirClaimPolicy{Class: "c"}, sharedDirClaimPolicyFor(&api.KubernetesConfig{SharedDirStorageClass: "c"}))
}

// claimWrites lists the create, update, patch and delete calls made on
// claims.
func claimWrites(clientset *k8sfake.Clientset) []string {
	var out []string
	for _, a := range clientset.Actions() {
		if a.GetResource().Resource != "persistentvolumeclaims" {
			continue
		}
		switch a.GetVerb() {
		case "get", "list", "watch":
		default:
			out = append(out, a.GetVerb())
		}
	}
	return out
}

func sharedDirClaimConfig(class string) RunConfig {
	cfg := RunConfig{
		Name:       "test-agent",
		Labels:     map[string]string{"scion.project": "myproject", "scion.project_id": "proj-1"},
		SharedDirs: []api.SharedDir{{Name: "build-cache"}},
	}
	if class != "" {
		cfg.Kubernetes = &api.KubernetesConfig{SharedDirStorageClass: class}
	}
	return cfg
}

// A claim that must be created with no storage class set refuses the
// start, creates nothing, and names the settings that fix it.
func TestCreateSharedDirPVCs_NoClassNoClaimRefused(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()

	err := rt.createSharedDirPVCs(context.Background(), "default", sharedDirClaimConfig(""))
	require.Error(t, err)
	assert.ErrorIs(t, err, errSharedDirClaimUnresolved)
	msg := err.Error()
	assert.Contains(t, msg, `shared dir "build-cache"`)
	assert.Contains(t, msg, "claim default/scion-shared-myproject-build-cache does not exist")
	assert.Contains(t, msg, "no claim was created")
	assert.Contains(t, msg, "configure server.project_storage backend nfs")
	assert.Contains(t, msg, "set shared_dir_storage_class to a ReadWriteMany class")
	assert.Contains(t, msg, "set shared_dir_storage_class to that class, or configure server.project_storage backend nfs")
	assert.Equal(t, ".", msg[len(msg)-1:], "the message ends with a period")

	assert.Empty(t, claimWrites(clientset))
	pvcs, err := clientset.CoreV1().PersistentVolumeClaims("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pvcs.Items)
}

// The refusal applies only when a new claim would be created: existing
// claims, bound or not, are reused unchanged with no storage class set.
func TestCreateSharedDirPVCs_ExistingClaimReusedWithoutPolicy(t *testing.T) {
	for _, phase := range []corev1.PersistentVolumeClaimPhase{corev1.ClaimBound, corev1.ClaimPending} {
		t.Run(string(phase), func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			existing := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: "scion-shared-myproject-build-cache", Namespace: "default",
					Labels: map[string]string{"scion.project": "myproject", "scion.project_id": "proj-1", "scion.shared-dir": "build-cache"},
				},
				Status: corev1.PersistentVolumeClaimStatus{Phase: phase},
			}
			require.NoError(t, clientset.Tracker().Add(existing))

			require.NoError(t, rt.createSharedDirPVCs(context.Background(), "default", sharedDirClaimConfig("")))
			assert.Empty(t, claimWrites(clientset))
			got, err := clientset.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), existing.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, existing.Labels, got.Labels)
			assert.Nil(t, got.Spec.StorageClassName)
		})
	}
}

func TestEnsureProjectRWXClaim_OtherProjectRefused(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	require.NoError(t, clientset.Tracker().Add(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scion-shared-myproject-build-cache", Namespace: "default",
			Labels: map[string]string{"scion.project": "myproject", "scion.project_id": "proj-other"},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}))

	err := rt.createSharedDirPVCs(context.Background(), "default", sharedDirClaimConfig("standard-rwx"))
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "claim default/scion-shared-myproject-build-cache is labelled for project proj-other (scion.project_id)")
	assert.Contains(t, msg, "not this project proj-1")
	assert.Contains(t, msg, "Delete or rename that claim")
	assert.Empty(t, claimWrites(clientset))
}

// A start without a project ID cannot compare, and reuses the claim.
func TestEnsureProjectRWXClaim_NoStartProjectIDReuses(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	require.NoError(t, clientset.Tracker().Add(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scion-shared-myproject-build-cache", Namespace: "default",
			Labels: map[string]string{"scion.project_id": "proj-other"},
		},
	}))
	cfg := sharedDirClaimConfig("")
	delete(cfg.Labels, "scion.project_id")
	require.NoError(t, rt.createSharedDirPVCs(context.Background(), "default", cfg))
	assert.Empty(t, claimWrites(clientset))
}

func TestEnsureProjectRWXClaim_UnlabelledReusedWithWarning(t *testing.T) {
	var buf bytes.Buffer
	setRuntimeLogHandler(t, slog.NewTextHandler(&buf, nil))
	rt, clientset, _ := newTestK8sRuntime()
	require.NoError(t, clientset.Tracker().Add(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "scion-shared-myproject-build-cache", Namespace: "default"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}))

	require.NoError(t, rt.createSharedDirPVCs(context.Background(), "default", sharedDirClaimConfig("")))
	assert.Empty(t, claimWrites(clientset))
	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "without a project ID label")
	assert.Contains(t, out, "pvc=scion-shared-myproject-build-cache")
}

// With a storage class set, a missing claim is created exactly as before.
func TestCreateSharedDirPVCs_ClassSetCreatesAsToday(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	require.NoError(t, rt.createSharedDirPVCs(context.Background(), "default", sharedDirClaimConfig("standard-rwx")))

	assert.Equal(t, []string{"create"}, claimWrites(clientset))
	pvc, err := clientset.CoreV1().PersistentVolumeClaims("default").Get(context.Background(), "scion-shared-myproject-build-cache", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"scion.project":    "myproject",
		"scion.project_id": "proj-1",
		"scion.shared-dir": "build-cache",
	}, pvc.Labels)
	require.NotNil(t, pvc.Spec.StorageClassName)
	assert.Equal(t, "standard-rwx", *pvc.Spec.StorageClassName)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, pvc.Spec.AccessModes)
	assert.Empty(t, pvc.Spec.VolumeName)
	storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, defaultSharedDirSize, storageReq.String())
}

// A claim read error other than NotFound is returned; it is not taken to
// mean that the claim must be created.
func TestEnsureProjectRWXClaim_GetErrorReturned(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	clientset.PrependReactor("get", "persistentvolumeclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewServiceUnavailable("try later")
	})

	err := rt.createSharedDirPVCs(context.Background(), "default", sharedDirClaimConfig("standard-rwx"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking shared-dir claim default/scion-shared-myproject-build-cache")
	assert.True(t, k8serrors.IsServiceUnavailable(errors.Unwrap(err)))
	assert.Empty(t, claimWrites(clientset))
}
