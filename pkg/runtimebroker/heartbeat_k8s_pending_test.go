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

package runtimebroker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// TestHeartbeatService_KubernetesPendingPodReportsStarting
// (ptone/scion#5365): Start writes phase "running" to agent-info.json before
// the pod exists. With the real Kubernetes runtime and agent manager, the
// broker heartbeat reports an agent whose pod is still Pending (waiting for
// a node) as starting, so the hub does not record it as running and end its
// launch as observed running. An agent whose pod is Running is still
// reported as running.
func TestHeartbeatService_KubernetesPendingPodReportsStarting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	projectPath := filepath.Join(t.TempDir(), ".scion")

	cs := k8sfake.NewClientset()
	addAgent := func(name string, podPhase corev1.PodPhase, agentState corev1.ContainerState) {
		t.Helper()
		agentDir := filepath.Join(projectPath, "agents", name)
		agentHome := filepath.Join(agentDir, "home")
		if err := os.MkdirAll(agentHome, 0755); err != nil {
			t.Fatal(err)
		}
		infoData, err := json.Marshal(api.AgentInfo{Name: name, Phase: string(state.PhaseRunning), Runtime: "kubernetes"})
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
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					"scion.name":               name,
					"scion.agent":              "true",
					projectkeys.LabelProjectID: "project-1",
				},
				Annotations: map[string]string{projectkeys.LabelProjectPath: projectPath},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "test:latest"}}},
			Status: corev1.PodStatus{
				Phase:             podPhase,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: agentState}},
			},
		}
		if _, err := cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	addAgent("pending-agent", corev1.PodPending, corev1.ContainerState{})
	addAgent("running-agent", corev1.PodRunning, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

	rt := runtime.NewKubernetesRuntime(k8s.NewTestClient(dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme()), cs))
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, agent.NewManager(rt), nil, slog.Default())
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat failed: %v", err)
	}

	calls := client.getHeartbeatCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 heartbeat call, got %d", len(calls))
	}
	got := map[string][]hubclient.AgentHeartbeat{}
	for _, p := range calls[0].Heartbeat.Projects {
		if p.ProjectID != "project-1" {
			continue
		}
		for _, a := range p.Agents {
			got[a.Slug] = append(got[a.Slug], a)
		}
	}
	want := map[string]struct{ phase, containerStatus string }{
		"pending-agent": {string(state.PhaseStarting), "Pending"},
		"running-agent": {string(state.PhaseRunning), "Running"},
	}
	for slug, w := range want {
		if len(got[slug]) != 1 {
			t.Errorf("%s reported %d times in project-1, want 1: %+v", slug, len(got[slug]), calls[0].Heartbeat.Projects)
			continue
		}
		hb := got[slug][0]
		if hb.Phase != w.phase {
			t.Errorf("%s: heartbeat Phase = %q, want %q", slug, hb.Phase, w.phase)
		}
		if hb.ContainerStatus != w.containerStatus {
			t.Errorf("%s: heartbeat ContainerStatus = %q, want %q", slug, hb.ContainerStatus, w.containerStatus)
		}
	}
}
