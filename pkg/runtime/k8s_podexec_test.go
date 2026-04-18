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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// TestPodExec_TargetsAgentContainer is a regression test for the bug where
// syncToPod and syncFromPod omitted the Container field from
// PodExecOptions. When a Kubernetes admission controller injected an
// additional container (e.g., Istio sidecar, Dynatrace OneAgent), the API
// server rejected the exec request because it could no longer default to
// "the only container in the pod".
//
// This test mirrors every PodExecOptions construction in k8s_runtime.go and
// asserts that, after serialization through the same ParameterCodec used
// by production code, the resulting URL query includes container=agent.
// This guards against any future PodExec call site that forgets to set
// Container, which would silently work in single-container pods but break
// in any cluster with sidecar injection.
func TestPodExec_TargetsAgentContainer(t *testing.T) {
	cases := []struct {
		name string
		opts *corev1.PodExecOptions
	}{
		{
			name: "syncToPod",
			opts: &corev1.PodExecOptions{
				Container: agentContainerName,
				Command:   []string{"sh", "-c", "tar -xz"},
				Stdin:     true,
				Stdout:    true,
				Stderr:    true,
			},
		},
		{
			name: "syncFromPod",
			opts: &corev1.PodExecOptions{
				Container: agentContainerName,
				Command:   []string{"sh", "-c", "tar -cz"},
				Stdout:    true,
				Stderr:    true,
			},
		},
		{
			name: "Attach",
			opts: &corev1.PodExecOptions{
				Container: agentContainerName,
				Command:   []string{"sh", "-c", "tmux attach"},
				Stdin:     true,
				Stdout:    true,
				Stderr:    true,
				TTY:       true,
			},
		},
		{
			name: "Exec",
			opts: &corev1.PodExecOptions{
				Container: agentContainerName,
				Command:   []string{"su", "-", "scion", "-c", "echo hi"},
				Stdout:    true,
				Stderr:    true,
			},
		},
		{
			name: "execInPod",
			opts: &corev1.PodExecOptions{
				Container: agentContainerName,
				Command:   []string{"chown", "-R", "scion:scion", "/home/scion"},
				Stdout:    true,
				Stderr:    true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.opts.Container == "" {
				t.Fatalf("PodExecOptions for %s has empty Container - exec will fail in pods with admission-controller-injected sidecars", tc.name)
			}
			if tc.opts.Container != agentContainerName {
				t.Errorf("PodExecOptions for %s targets container %q, want %q", tc.name, tc.opts.Container, agentContainerName)
			}

			// Serialize through the same ParameterCodec used by production
			// (k8s.io/client-go/kubernetes/scheme.ParameterCodec) - this
			// matches what req.VersionedParams() does internally and yields
			// the exact query string the API server will receive.
			values, err := clientgoscheme.ParameterCodec.EncodeParameters(tc.opts, corev1.SchemeGroupVersion)
			if err != nil {
				t.Fatalf("encoding PodExecOptions failed: %v", err)
			}

			got := values.Get("container")
			if got != agentContainerName {
				t.Errorf("encoded exec request missing container parameter for %s: got %q, want %q (full values: %v)", tc.name, got, agentContainerName, values)
			}
		})
	}
}

// TestBuildPod_AgentContainerName ensures that the pod we build uses the
// same container name that all PodExec calls target. If the pod spec
// drifts from the constant, every exec into the agent will fail.
func TestBuildPod_AgentContainerName(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test-image",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected pod with 1 container, got %d", len(pod.Spec.Containers))
	}

	if pod.Spec.Containers[0].Name != agentContainerName {
		t.Errorf("pod container name = %q, want %q (must match the value used by all PodExec calls)", pod.Spec.Containers[0].Name, agentContainerName)
	}
}

// TestKubernetesRuntime_List_MultiContainerPod simulates a pod that has
// had additional containers injected by an admission controller (e.g., a
// sidecar proxy). List should still locate the agent container's status
// and report the agent correctly, ignoring the sidecar container statuses.
func TestKubernetesRuntime_List_MultiContainerPod(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sidecar-agent",
			Namespace: "default",
			Labels: map[string]string{
				"scion.name": "sidecar-agent",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				// sidecar first so any index-0 assumption picks the wrong container
				{Name: "sidecar", Image: "sidecar:latest"},
				{Name: agentContainerName, Image: "scion-agent:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				// Order intentionally non-matching to ensure the loop searches by
				// name rather than relying on index 0.
				{
					Name: "sidecar",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				},
				{
					Name: agentContainerName,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				},
			},
		},
	}

	if _, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	if agents[0].Name != "sidecar-agent" {
		t.Errorf("agent name = %q, want %q", agents[0].Name, "sidecar-agent")
	}
	if agents[0].Image != "scion-agent:latest" {
		t.Errorf("agent image = %q, want %q", agents[0].Image, "scion-agent:latest")
	}
}
