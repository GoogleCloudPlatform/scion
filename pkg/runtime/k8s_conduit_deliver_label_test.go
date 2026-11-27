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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestKubernetesRuntime_ListReturnsConduitDeliverLabel verifies that the
// scion.conduit_deliver pod label survives the Kubernetes List round trip
// into api.AgentInfo.Labels.
func TestKubernetesRuntime_ListReturnsConduitDeliverLabel(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	rt.DefaultNamespace = "default"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conduit-agent",
			Namespace: "default",
			Labels: map[string]string{
				"scion.agent":              "true",
				"scion.name":               "conduit-agent",
				projectkeys.LabelProjectID: "proj-1",
				"scion.conduit_deliver":    "true",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: agentContainerName, Image: "test:latest"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  agentContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	if _, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("List returned %d agents, want 1", len(agents))
	}
	if got := agents[0].Labels["scion.conduit_deliver"]; got != "true" {
		t.Fatalf("Labels[scion.conduit_deliver] = %q, want %q (labels=%v)", got, "true", agents[0].Labels)
	}
}

// TestKnownConduitDeliverRuntimes_MatchRuntimeNames guards against drift
// between config.KnownConduitDeliverRuntimes and the Runtime.Name() values of
// the concrete runtime implementations.
func TestKnownConduitDeliverRuntimes_MatchRuntimeNames(t *testing.T) {
	want := []string{
		(&CloudRunRuntime{}).Name(),
		(&KubernetesRuntime{}).Name(),
		(&DockerRuntime{}).Name(),
		(&PodmanRuntime{}).Name(),
		(&AppleContainerRuntime{}).Name(),
		(&SubstrateRuntime{}).Name(),
		(&CloudRunSandboxRuntime{}).Name(),
	}
	got := config.KnownConduitDeliverRuntimes()
	if !slices.Equal(got, want) {
		t.Fatalf("config.KnownConduitDeliverRuntimes() = %v, want %v", got, want)
	}
}
