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
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestAgentOperationID(t *testing.T) {
	tests := []struct {
		name string
		in   api.AgentInfo
		want string
	}{
		{
			name: "docker container ID is unchanged",
			in:   api.AgentInfo{ContainerID: "3f2a9c1d0b7e", Runtime: "docker"},
			want: "3f2a9c1d0b7e",
		},
		{
			name: "docker container name with odd characters is unchanged",
			in:   api.AgentInfo{ContainerID: " scion-agent_1 ", Runtime: "docker"},
			want: " scion-agent_1 ",
		},
		{
			name: "kubernetes pod is qualified with its namespace",
			in: api.AgentInfo{ContainerID: "my-agent", Runtime: "kubernetes",
				Kubernetes: &api.AgentK8sMetadata{Namespace: "scion-agents", PodName: "my-agent"}},
			want: "scion-agents/my-agent",
		},
		{
			name: "kubernetes entry without a namespace stays bare",
			in: api.AgentInfo{ContainerID: "my-agent", Runtime: "kubernetes",
				Kubernetes: &api.AgentK8sMetadata{PodName: "my-agent"}},
			want: "my-agent",
		},
		{
			name: "already qualified ID is not qualified twice",
			in: api.AgentInfo{ContainerID: "ns-a/my-agent",
				Kubernetes: &api.AgentK8sMetadata{Namespace: "ns-b"}},
			want: "ns-a/my-agent",
		},
		{
			name: "empty container ID stays empty",
			in:   api.AgentInfo{Kubernetes: &api.AgentK8sMetadata{Namespace: "scion-agents"}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AgentOperationID(tt.in); got != tt.want {
				t.Errorf("AgentOperationID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestKubernetesDelete_OperationIDUsesPodNamespace covers a runtime whose
// default namespace is "default" while the agent pod runs in a profile
// namespace, and every request in "default" is Forbidden. Deleting by the
// entry's operation ID removes the pod in its own namespace and sends no
// request to "default".
func TestKubernetesDelete_OperationIDUsesPodNamespace(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "my-agent",
			Namespace:   "scion-agents",
			Labels:      map[string]string{"scion.name": "my-agent", "scion.agent": "true"},
			Annotations: map[string]string{"scion.namespace": "scion-agents"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: "img"}}},
	}
	if _, err := clientset.CoreV1().Pods("scion-agents").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	var defaultNSRequests []string
	clientset.PrependReactor("*", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetNamespace() != "default" {
			return false, nil, nil
		}
		defaultNSRequests = append(defaultNSRequests, action.GetVerb()+" "+action.GetResource().Resource)
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: action.GetResource().Resource}, "", nil)
	})

	r := NewKubernetesRuntime(k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), clientset))
	r.DefaultNamespace = "default"
	r.ListAllNamespaces = true

	agents, err := r.List(ctx, map[string]string{"scion.name": "my-agent"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("List returned %d entries, want 1", len(agents))
	}
	if agents[0].ContainerID != "my-agent" {
		t.Fatalf("List ContainerID = %q, want the bare pod name", agents[0].ContainerID)
	}

	id := AgentOperationID(agents[0])
	if err := r.Stop(ctx, id); err != nil {
		t.Fatalf("Stop(%q): %v", id, err)
	}
	if err := r.Delete(ctx, RunRef{ID: id}); err != nil {
		t.Fatalf("Delete(%q): %v", id, err)
	}
	if _, err := clientset.CoreV1().Pods("scion-agents").Get(ctx, "my-agent", metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("pod still present after delete (err=%v)", err)
	}
	if len(defaultNSRequests) != 0 {
		t.Fatalf("requests sent to the default namespace: %v", defaultNSRequests)
	}
}
