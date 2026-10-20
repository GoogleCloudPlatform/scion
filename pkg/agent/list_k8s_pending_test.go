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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// TestListKubernetesPendingPodIsStarting (ptone/scion#5365): before
// ptone/scion#5368, Start wrote phase "running" to agent-info.json before the
// pod existed; on Kubernetes it now writes "starting" until Run returns.
// Through the real Kubernetes runtime, Manager.List (which the broker
// heartbeat uses) lists an agent whose pod is still Pending as starting,
// whatever agent-info.json holds; a Running pod lists agent-info.json's
// phase, so "running" once Run has returned and "starting" while the startup
// gate is still closed.
func TestListKubernetesPendingPodIsStarting(t *testing.T) {
	tests := []struct {
		name         string
		podPhase     corev1.PodPhase
		agentState   corev1.ContainerState
		infoPhase    string
		infoActivity string
		wantPhase    string
		wantActivity string
	}{
		{
			name:         "pending pod awaiting capacity",
			podPhase:     corev1.PodPending,
			infoPhase:    string(state.PhaseRunning),
			infoActivity: string(state.ActivityThinking),
			wantPhase:    string(state.PhaseStarting),
		},
		{
			name:         "running pod",
			podPhase:     corev1.PodRunning,
			agentState:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			infoPhase:    string(state.PhaseRunning),
			infoActivity: string(state.ActivityThinking),
			wantPhase:    string(state.PhaseRunning),
			wantActivity: string(state.ActivityThinking),
		},
		{
			// ptone/scion#5368: Start records starting until Run returns;
			// a Running pod whose startup gate is still closed lists no
			// runtime phase, so agent-info.json's starting is used.
			name:       "running pod, startup gate closed",
			podPhase:   corev1.PodRunning,
			agentState: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			infoPhase:  string(state.PhaseStarting),
			wantPhase:  string(state.PhaseStarting),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			projectPath := filepath.Join(t.TempDir(), ".scion")
			agentName := "k8s-pending-agent"
			agentDir := filepath.Join(projectPath, "agents", agentName)
			agentHome := filepath.Join(agentDir, "home")
			if err := os.MkdirAll(agentHome, 0755); err != nil {
				t.Fatal(err)
			}
			infoData, err := json.Marshal(api.AgentInfo{
				Name:     agentName,
				Phase:    tc.infoPhase,
				Activity: tc.infoActivity,
				Runtime:  "kubernetes",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        agentName,
					Namespace:   "default",
					Labels:      map[string]string{"scion.name": agentName, "scion.agent": "true"},
					Annotations: map[string]string{projectkeys.LabelProjectPath: projectPath},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "test:latest"}}},
				Status: corev1.PodStatus{
					Phase:             tc.podPhase,
					ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: tc.agentState}},
				},
			}
			cs := k8sfake.NewClientset()
			if _, err := cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			rt := runtime.NewKubernetesRuntime(k8s.NewTestClient(fake.NewSimpleDynamicClient(k8sruntime.NewScheme()), cs))

			agents, err := NewManager(rt).List(context.Background(), map[string]string{"scion.name": agentName})
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}
			if len(agents) != 1 {
				t.Fatalf("listed %d agents, want 1: %+v", len(agents), agents)
			}
			if agents[0].ProjectPath != projectPath {
				t.Fatalf("ProjectPath = %q, want %q (agent-info.json not merged)", agents[0].ProjectPath, projectPath)
			}
			if agents[0].Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", agents[0].Phase, tc.wantPhase)
			}
			if agents[0].Activity != tc.wantActivity {
				t.Errorf("Activity = %q, want %q", agents[0].Activity, tc.wantActivity)
			}
		})
	}
}
