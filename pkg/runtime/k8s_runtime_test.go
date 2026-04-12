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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestKubernetesRuntime_List(t *testing.T) {
	// Create a fake clientset
	clientset := k8sfake.NewClientset()

	// Create a pod that mimics what we expect
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
			Labels: map[string]string{
				"scion.name":     "test-agent",
				"scion.template": "test-template",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Image: "test-image",
				},
			},
		},
	}

	_, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	// Create a generic scheme for dynamic client
	scheme := k8sruntime.NewScheme()

	fc := fake.NewSimpleDynamicClient(scheme)

	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	agents, err := r.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(agents))
		return
	}

	if agents[0].ContainerID != "test-agent" {
		t.Errorf("expected ContainerID test-agent, got %s", agents[0].ContainerID)
	}

	if agents[0].ContainerStatus != "Running" {
		t.Errorf("expected container status Running, got %s", agents[0].ContainerStatus)
	}

	if agents[0].Image != "test-image" {
		t.Errorf("expected image test-image, got %s", agents[0].Image)
	}
}

func TestKubernetesRuntime_List_TerminalPhases(t *testing.T) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "completed-agent",
				Namespace: "default",
				Labels: map[string]string{
					"scion.name": "completed-agent",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "agent",
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								Reason:   "Completed",
								ExitCode: 0,
							},
						},
					},
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Image: "test-image"}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "failed-agent",
				Namespace: "default",
				Labels: map[string]string{
					"scion.name": "failed-agent",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "agent",
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								Reason:   "Error",
								ExitCode: 1,
							},
						},
					},
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Image: "test-image"}},
			},
		},
	}

	for _, pod := range pods {
		if _, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create pod %q: %v", pod.Name, err)
		}
	}

	agents, err := r.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	got := map[string]api.AgentInfo{}
	for _, agent := range agents {
		got[agent.Name] = agent
	}

	if got["completed-agent"].Phase != "stopped" {
		t.Errorf("completed-agent phase = %q, want %q", got["completed-agent"].Phase, "stopped")
	}
	if got["completed-agent"].ContainerStatus != "Succeeded (Completed)" {
		t.Errorf("completed-agent container status = %q, want %q", got["completed-agent"].ContainerStatus, "Succeeded (Completed)")
	}
	if got["failed-agent"].Phase != "error" {
		t.Errorf("failed-agent phase = %q, want %q", got["failed-agent"].Phase, "error")
	}
	if got["failed-agent"].ContainerStatus != "Failed (Error)" {
		t.Errorf("failed-agent container status = %q, want %q", got["failed-agent"].ContainerStatus, "Failed (Error)")
	}
}

func TestKubernetesRuntime_BuildPod_Env(t *testing.T) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test-image",
		UnixUsername: "scion",
	}

	pod, _ := r.buildPod("default", config)

	foundUID := false
	foundGID := false
	foundHome := false
	foundUser := false
	foundLogname := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "SCION_HOST_UID" {
			foundUID = true
		}
		if env.Name == "SCION_HOST_GID" {
			foundGID = true
		}
		if env.Name == "HOME" && env.Value == "/home/scion" {
			foundHome = true
		}
		if env.Name == "USER" && env.Value == "scion" {
			foundUser = true
		}
		if env.Name == "LOGNAME" && env.Value == "scion" {
			foundLogname = true
		}
	}

	if !foundUID {
		t.Errorf("SCION_HOST_UID not found in pod env")
	}
	if !foundGID {
		t.Errorf("SCION_HOST_GID not found in pod env")
	}
	if !foundHome {
		t.Errorf("HOME not found in pod env")
	}
	if !foundUser {
		t.Errorf("USER not found in pod env")
	}
	if !foundLogname {
		t.Errorf("LOGNAME not found in pod env")
	}
}

func TestSelectLogContainer(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "single container",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "agent"}},
				},
			},
			want: "agent",
		},
		{
			name: "prefers agent container in multi-container pod",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "sync-helper"},
						{Name: "agent"},
					},
				},
			},
			want: "agent",
		},
		{
			name: "falls back to first container",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main"},
						{Name: "sidecar"},
					},
				},
			},
			want: "main",
		},
		{
			name: "empty pod",
			pod:  &corev1.Pod{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectLogContainer(tt.pod); got != tt.want {
				t.Fatalf("selectLogContainer() = %q, want %q", got, tt.want)
			}
		})
	}
}
