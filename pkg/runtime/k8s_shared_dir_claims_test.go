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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const claimTestProjectID = "550e8400-e29b-41d4-a716-446655440000"

func claim(ns, name string, labels map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, UID: types.UID("uid-" + name)}}
}

func claimLabels(dir, projectName, projectID string) map[string]string {
	l := map[string]string{}
	if dir != "" {
		l[sharedDirClaimLabel] = dir
	}
	if projectName != "" {
		l[projectkeys.LabelProject] = projectName
	}
	if projectID != "" {
		l[projectkeys.LabelProjectID] = projectID
	}
	return l
}

func claimRuntime(t *testing.T, objs ...k8sruntime.Object) (*KubernetesRuntime, *k8sfake.Clientset) {
	t.Helper()
	cs := k8sfake.NewClientset(objs...)
	r := NewKubernetesRuntime(k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), cs))
	r.DefaultNamespace = "agents"
	return r, cs
}

func claimNames(t *testing.T, cs *k8sfake.Clientset, ns string) []string {
	t.Helper()
	list, err := cs.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	var out []string
	for _, c := range list.Items {
		out = append(out, c.Name)
	}
	return out
}

// Only claims carrying all three conditions are deleted: the project-id
// label equal to the project, a shared-dir label, and the
// scion-shared-<...>-<dir> name. Claims without the project-id label are
// kept, and those of the project's name are listed; the layout's shared
// claim, another project's claims, name-only matches and claims in other
// namespaces are kept.
func TestReleaseProjectSharedDirClaims_Selector(t *testing.T) {
	r, cs := claimRuntime(t,
		claim("agents", "scion-shared-web-scratchpad", claimLabels("scratchpad", "web", claimTestProjectID)),    // deleted
		claim("agents", "scion-shared-web-data", claimLabels("data", "web", claimTestProjectID)),                // deleted
		claim("agents", "scion-shared-web-cache", claimLabels("cache", "web", "")),                              // no project-id: listed
		claim("agents", "scion-shared-web-notes", claimLabels("notes", "", "")),                                 // name only (no project-id): listed
		claim("agents", "scion-shared-web-other", claimLabels("", "web", claimTestProjectID)),                   // no shared-dir label: kept
		claim("agents", "web-scratchpad", claimLabels("scratchpad", "web", claimTestProjectID)),                 // wrong name shape: kept
		claim("agents", "scion-shared-web-mismatch", claimLabels("scratchpad", "web", claimTestProjectID)),      // name does not end in the dir: kept
		claim("agents", "scion-shared-other-scratchpad", claimLabels("scratchpad", "other", "another-project")), // another project: kept
		claim("agents", "hub-nfs", claimLabels("", "", claimTestProjectID)),                                     // layout claim: kept
		claim("agents", "scion-shared-web-layout", claimLabels("layout", "web", claimTestProjectID)),            // named in keep: kept
		claim("elsewhere", "scion-shared-web-scratchpad", claimLabels("scratchpad", "web", claimTestProjectID)), // other namespace: kept
	)

	res, err := r.ReleaseProjectSharedDirClaims(context.Background(), claimTestProjectID, "web", []string{"scion-shared-web-layout"})
	require.NoError(t, err)
	assert.Equal(t, "agents", res.Namespace)
	assert.Equal(t, []string{"scion-shared-web-data", "scion-shared-web-scratchpad"}, res.Deleted)
	assert.Equal(t, []string{"scion-shared-web-cache", "scion-shared-web-notes"}, res.Unlabelled)
	assert.Empty(t, res.Failed)

	assert.ElementsMatch(t, []string{
		"scion-shared-web-cache", "scion-shared-web-notes", "scion-shared-web-other", "web-scratchpad",
		"scion-shared-web-mismatch", "scion-shared-other-scratchpad", "hub-nfs", "scion-shared-web-layout",
	}, claimNames(t, cs, "agents"))
	assert.Equal(t, []string{"scion-shared-web-scratchpad"}, claimNames(t, cs, "elsewhere"))
}

// Each condition is required on its own.
func TestReleaseProjectSharedDirClaims_EachConditionRequired(t *testing.T) {
	cases := map[string]*corev1.PersistentVolumeClaim{
		"no project-id label":   claim("agents", "scion-shared-web-a", claimLabels("a", "web", "")),
		"other project-id":      claim("agents", "scion-shared-web-a", claimLabels("a", "web", "x")),
		"no shared-dir label":   claim("agents", "scion-shared-web-a", claimLabels("", "web", claimTestProjectID)),
		"invalid shared-dir":    claim("agents", "scion-shared-web-A_", claimLabels("A_", "web", claimTestProjectID)),
		"no scion-shared- name": claim("agents", "web-a", claimLabels("a", "web", claimTestProjectID)),
		"empty middle":          claim("agents", "scion-shared--a", claimLabels("a", "web", claimTestProjectID)),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, cs := claimRuntime(t, c)
			res, err := r.ReleaseProjectSharedDirClaims(context.Background(), claimTestProjectID, "web", nil)
			require.NoError(t, err)
			assert.Empty(t, res.Deleted)
			assert.Equal(t, []string{c.Name}, claimNames(t, cs, "agents"))
		})
	}
}

// A failed deletion is reported and does not stop the others; a list
// failure is the error; a missing project ID is refused.
func TestReleaseProjectSharedDirClaims_Failures(t *testing.T) {
	r, cs := claimRuntime(t,
		claim("agents", "scion-shared-web-a", claimLabels("a", "web", claimTestProjectID)),
		claim("agents", "scion-shared-web-b", claimLabels("b", "web", claimTestProjectID)),
	)
	cs.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() == "scion-shared-web-a" {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	res, err := r.ReleaseProjectSharedDirClaims(context.Background(), claimTestProjectID, "web", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"scion-shared-web-b"}, res.Deleted)
	require.Contains(t, res.Failed, "scion-shared-web-a")

	r2, cs2 := claimRuntime(t)
	cs2.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("unavailable")
	})
	_, err = r2.ReleaseProjectSharedDirClaims(context.Background(), claimTestProjectID, "web", nil)
	assert.Error(t, err)

	_, err = r2.ReleaseProjectSharedDirClaims(context.Background(), "", "web", nil)
	assert.Error(t, err)
}

func podUsing(ns, name string, claims ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	for _, c := range claims {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: c,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c}}})
	}
	return p
}

// A claim an existing pod references (in any phase) is kept and listed as
// in use, never released; a claim no pod references is released.
func TestReleaseProjectSharedDirClaims_InUseKept(t *testing.T) {
	r, cs := claimRuntime(t,
		claim("agents", "scion-shared-web-a", claimLabels("a", "web", claimTestProjectID)),
		claim("agents", "scion-shared-web-b", claimLabels("b", "web", claimTestProjectID)),
		podUsing("agents", "agent-pod", "scion-shared-web-a"),
		podUsing("elsewhere", "other-ns-pod", "scion-shared-web-b"), // another namespace: no effect
	)
	res, err := r.ReleaseProjectSharedDirClaims(context.Background(), claimTestProjectID, "web", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"scion-shared-web-a"}, res.InUse)
	assert.Equal(t, []string{"scion-shared-web-b"}, res.Deleted)
	assert.Equal(t, []string{"scion-shared-web-a"}, claimNames(t, cs, "agents"))

	r2, cs2 := claimRuntime(t, claim("agents", "scion-shared-web-a", claimLabels("a", "web", claimTestProjectID)))
	cs2.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	_, err = r2.ReleaseProjectSharedDirClaims(context.Background(), claimTestProjectID, "web", nil)
	assert.Error(t, err, "without the pod list nothing is released")
	assert.Equal(t, []string{"scion-shared-web-a"}, claimNames(t, cs2, "agents"))
}

// Every claim deletion carries the claim's UID as a precondition, so a
// claim re-created meanwhile under the same name is not deleted.
func TestReleaseProjectSharedDirClaims_UIDPrecondition(t *testing.T) {
	c := claim("agents", "scion-shared-web-a", claimLabels("a", "web", claimTestProjectID))
	r, cs := claimRuntime(t, c)
	var got *metav1.Preconditions
	cs.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		got = a.(k8stesting.DeleteAction).GetDeleteOptions().Preconditions
		return false, nil, nil
	})
	_, err := r.ReleaseProjectSharedDirClaims(context.Background(), claimTestProjectID, "web", nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.UID)
	assert.Equal(t, c.UID, *got.UID)
}

// A claim of the same name labelled for another project is never reused
// for this project's shared dir.
func TestEnsureProjectRWXClaim_RefusesAnotherProjectsClaim(t *testing.T) {
	r, _ := claimRuntime(t, claim("agents", "scion-shared-web-a", claimLabels("a", "web", "another-project")))
	err := r.ensureProjectRWXClaim(context.Background(), "agents", "web", claimTestProjectID, "a", sharedDirClaimPolicy{}, resource.MustParse("1Gi"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is labelled for project another-project")

	r2, _ := claimRuntime(t, claim("agents", "scion-shared-web-a", claimLabels("a", "web", claimTestProjectID)))
	assert.NoError(t, r2.ensureProjectRWXClaim(context.Background(), "agents", "web", claimTestProjectID, "a", sharedDirClaimPolicy{}, resource.MustParse("1Gi")), "its own claim is reused")
	r3, _ := claimRuntime(t, claim("agents", "scion-shared-web-a", claimLabels("a", "web", "")))
	assert.NoError(t, r3.ensureProjectRWXClaim(context.Background(), "agents", "web", claimTestProjectID, "a", sharedDirClaimPolicy{}, resource.MustParse("1Gi")), "a claim without the label is reused as before")
}
