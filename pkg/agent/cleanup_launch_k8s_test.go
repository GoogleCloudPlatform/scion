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

package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Every runtime that reports launch resource handles must also be able to
// delete them with an identity precondition, or CleanupLaunch skips them.
var (
	_ UIDPreconditionDeleter = (*runtime.KubernetesRuntime)(nil)
	_ UIDPreconditionDeleter = (*runtime.DockerRuntime)(nil)
	_ UIDPreconditionDeleter = (*runtime.PodmanRuntime)(nil)
	_ UIDPreconditionDeleter = (*runtime.AppleContainerRuntime)(nil)
)

// newUIDEnforcingK8sRuntime returns a real KubernetesRuntime over a fake
// clientset that, like the API server, rejects a delete whose UID
// precondition does not match the stored object.
func newUIDEnforcingK8sRuntime() (*runtime.KubernetesRuntime, *k8sfake.Clientset) {
	cs := k8sfake.NewClientset()
	cs.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		del := action.(k8stesting.DeleteAction)
		pre := del.GetDeleteOptions().Preconditions
		if pre == nil || pre.UID == nil {
			return false, nil, nil
		}
		existing, err := cs.Tracker().Get(del.GetResource(), del.GetNamespace(), del.GetName())
		if err != nil {
			return false, nil, nil
		}
		m, err := meta.Accessor(existing)
		if err != nil {
			return true, nil, err
		}
		if m.GetUID() != *pre.UID {
			return true, nil, k8serrors.NewConflict(schema.GroupResource{Resource: del.GetResource().Resource}, del.GetName(), fmt.Errorf("UID precondition failed"))
		}
		return false, nil, nil
	})
	rt := runtime.NewKubernetesRuntime(k8s.NewTestClient(dynfake.NewSimpleDynamicClient(k8sruntime.NewScheme()), cs))
	return rt, cs
}

func twentyKeySecret(name, uid string) *corev1.Secret {
	data := map[string][]byte{}
	for i := 0; i < 20; i++ {
		data[fmt.Sprintf("KEY_%02d", i)] = []byte("v")
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)},
		Data:       data,
	}
}

func TestCleanupLaunch_KubernetesRemovesEveryHandle(t *testing.T) {
	rt, cs := newUIDEnforcingK8sRuntime()
	ctx := context.Background()
	if _, err := cs.CoreV1().Secrets("ns").Create(ctx, twentyKeySecret("scion-agent-a1", "s-1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "ns", UID: "p-1"}}
	if _, err := cs.CoreV1().Pods("ns").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	mgr := &AgentManager{Runtime: rt}
	handles := []ResourceHandle{
		{Kind: api.ResourceKindSecret, Namespace: "ns", Name: "scion-agent-a1", UID: "s-1"},
		{Kind: api.ResourceKindPod, Namespace: "ns", Name: "a1", UID: "p-1"},
	}
	if err := mgr.CleanupLaunch(ctx, handles); err != nil {
		t.Fatalf("CleanupLaunch: %v", err)
	}
	if _, err := cs.CoreV1().Secrets("ns").Get(ctx, "scion-agent-a1", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("20-key secret not removed: %v", err)
	}
	if _, err := cs.CoreV1().Pods("ns").Get(ctx, "a1", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("pod not removed: %v", err)
	}
	// Repeating the cleanup (the objects are gone) is not an error.
	if err := mgr.CleanupLaunch(ctx, handles); err != nil {
		t.Fatalf("repeat CleanupLaunch: %v", err)
	}
}

// TestCleanupLaunch_KubernetesStaleHandlesKeepRecreatedSecret: a stale
// launch's cleanup, whose handles name objects a newer launch has since
// recreated under the same names, deletes nothing.
func TestCleanupLaunch_KubernetesStaleHandlesKeepRecreatedSecret(t *testing.T) {
	rt, cs := newUIDEnforcingK8sRuntime()
	ctx := context.Background()
	// The newer launch's objects (new UIDs) now hold the names.
	if _, err := cs.CoreV1().Secrets("ns").Create(ctx, twentyKeySecret("scion-agent-a1", "s-new"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "ns", UID: "p-new"}}
	if _, err := cs.CoreV1().Pods("ns").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	mgr := &AgentManager{Runtime: rt}
	stale := []ResourceHandle{
		{Kind: api.ResourceKindSecret, Namespace: "ns", Name: "scion-agent-a1", UID: "s-old"},
		{Kind: api.ResourceKindPod, Namespace: "ns", Name: "a1", UID: "p-old"},
	}
	if err := mgr.CleanupLaunch(ctx, stale); err != nil {
		t.Fatalf("CleanupLaunch: %v", err)
	}
	s, err := cs.CoreV1().Secrets("ns").Get(ctx, "scion-agent-a1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("recreated secret was deleted by a stale cleanup: %v", err)
	}
	if s.UID != "s-new" || len(s.Data) != 20 {
		t.Fatalf("secret changed: UID %q, %d keys", s.UID, len(s.Data))
	}
	if _, err := cs.CoreV1().Pods("ns").Get(ctx, "a1", metav1.GetOptions{}); err != nil {
		t.Fatalf("recreated pod was deleted by a stale cleanup: %v", err)
	}
}
