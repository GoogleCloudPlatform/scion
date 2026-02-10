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
	"strings"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// MockHarness for testing command generation
type MockHarness struct{}

func (m *MockHarness) Name() string { return "mock" }
func (m *MockHarness) GetCommand(task string, resume bool, args []string) []string {
	return []string{"/bin/echo", "hello"}
}
func (m *MockHarness) GetVolumes(username string, auth api.AuthConfig) []api.VolumeMount { return nil }
func (m *MockHarness) GetEnv(agentName, homeDir, username string, auth api.AuthConfig) map[string]string {
	return nil
}
func (m *MockHarness) PropagateFiles(homeDir, username string, auth api.AuthConfig) error { return nil }
func (m *MockHarness) DiscoverAuth(agentHome string) api.AuthConfig { return api.AuthConfig{} }
func (m *MockHarness) DefaultConfigDir() string { return ".mock" }
func (m *MockHarness) HasSystemPrompt(agentHome string) bool { return false }
func (m *MockHarness) Provision(ctx context.Context, agentName, agentHome, agentWorkspace string) error { return nil }
func (m *MockHarness) SeedTemplateDir(templateDir string, force bool) error { return nil }
func (m *MockHarness) GetEmbedDir() string { return "mock" }
func (m *MockHarness) GetInterruptKey() string { return "C-c" }

func TestKubernetesRuntime_Run_Tmux(t *testing.T) {
	// Setup
	clientset := k8sfake.NewSimpleClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	useTmux := true
	config := RunConfig{
		Name:    "tmux-agent",
		Image:   "test-image",
		UseTmux: useTmux,
		Harness: &MockHarness{},
	}

	// Run in background because it waits for Pod Ready
	errChan := make(chan error)
	go func() {
		_, err := r.Run(context.Background(), config)
		errChan <- err
	}()

	// Wait for Pod to be created
	var pod *corev1.Pod
	var err error
	for i := 0; i < 10; i++ {
		pod, err = clientset.CoreV1().Pods("default").Get(context.Background(), "tmux-agent", metav1.GetOptions{})
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if pod == nil {
		t.Fatal("Pod was not created within timeout")
	}

	// Assertions
	// 1. Check Label
	if pod.Labels["scion.tmux"] != "true" {
		t.Errorf("expected scion.tmux label to be true, got %s", pod.Labels["scion.tmux"])
	}

	// 2. Check Command
	// Expected: tmux new-session -s scion "/bin/echo hello"
	// Note: quoting might vary depending on implementation, but we expect tmux to be the entrypoint
	if len(pod.Spec.Containers) == 0 {
		t.Fatal("Pod has no containers")
	}
	cmd := pod.Spec.Containers[0].Command
	if len(cmd) < 4 {
		t.Fatalf("Command too short: %v", cmd)
	}
	if cmd[0] != "tmux" || cmd[1] != "new-session" {
		t.Errorf("Expected command to start with tmux new-session, got %v", cmd)
	}
	// Check if the wrapped command contains our harness command
	joinedCmd := cmd[len(cmd)-1]
	if !strings.Contains(joinedCmd, "/bin/echo") || !strings.Contains(joinedCmd, "hello") {
		t.Errorf("Wrapped command does not contain harness command. Got: %s", joinedCmd)
	}

	// Update Pod to Running to let Run finish
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: "agent",
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{},
			},
		},
	}
	_, err = clientset.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	// Wait for Run to return
	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("Run failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run timed out waiting for pod ready")
	}
}
