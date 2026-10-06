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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newNamespacedPodManager returns a manager over a Kubernetes runtime that
// lists all namespaces with "default" as its default namespace, where every
// request is Forbidden, and an agent pod in scion-agents.
func newNamespacedPodManager(t *testing.T) (Manager, *k8sfake.Clientset, *[]string) {
	t.Helper()
	cs := k8sfake.NewClientset()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "my-agent", Namespace: "scion-agents",
		Labels: map[string]string{"scion.name": "my-agent", "scion.agent": "true"},
	}}
	if _, err := cs.CoreV1().Pods("scion-agents").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var defaultRequests []string
	cs.PrependReactor("*", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetNamespace() != "default" {
			return false, nil, nil
		}
		defaultRequests = append(defaultRequests, action.GetVerb()+" "+action.GetResource().Resource)
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: action.GetResource().Resource}, "", nil)
	})
	rt := runtime.NewKubernetesRuntime(k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), cs))
	rt.DefaultNamespace = "default"
	rt.ListAllNamespaces = true
	return NewManager(rt), cs, &defaultRequests
}

func TestManagerStopAndDelete_KubernetesPodAddressedByNamespace(t *testing.T) {
	for _, op := range []string{"stop", "delete"} {
		t.Run(op, func(t *testing.T) {
			mgr, cs, defaultRequests := newNamespacedPodManager(t)
			ctx := context.Background()
			var err error
			if op == "stop" {
				err = mgr.Stop(ctx, "my-agent", "", "")
			} else {
				_, err = mgr.Delete(ctx, "my-agent", false, "", false)
			}
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			if _, err := cs.CoreV1().Pods("scion-agents").Get(ctx, "my-agent", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
				t.Fatalf("pod still present after %s (err=%v)", op, err)
			}
			if len(*defaultRequests) != 0 {
				t.Fatalf("requests sent to the default namespace: %v", *defaultRequests)
			}
		})
	}
}
