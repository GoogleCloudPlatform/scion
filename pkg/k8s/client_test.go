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

package k8s

import (
	"context"
	"fmt"
	"testing"

	"github.com/ptone/scion-agent/pkg/k8s/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
)

func TestClient_ListSandboxClaims(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	gvr := schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxclaims"}
	
scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "extensions.agents.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "SandboxClaim",
	}, &v1alpha1.SandboxClaim{})
scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "extensions.agents.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "SandboxClaimList",
	}, &v1alpha1.SandboxClaimList{})

	fc := fake.NewSimpleDynamicClient(scheme)

	claim := &v1alpha1.SandboxClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
			Kind:       "SandboxClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
	}
	
unstructuredMap, _ := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(claim)
	u := &unstructured.Unstructured{Object: unstructuredMap}
	
	_, err := fc.Resource(gvr).Namespace("default").Create(context.Background(), u, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Raw List call to see what fake client returns
	rawList, err := fc.Resource(gvr).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Raw List failed: %v", err)
	}
	fmt.Printf("DEBUG: Raw List items length: %d\n", len(rawList.Items))

	client := NewTestClient(fc, &kubernetes.Clientset{})
	list, err := client.ListSandboxClaims(context.Background(), "default", "")
	if err != nil {
		t.Fatalf("ListSandboxClaims failed: %v", err)
	}

	if len(list.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(list.Items))
	}
}
