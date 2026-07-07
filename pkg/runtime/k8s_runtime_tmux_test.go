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
	"embed"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// MockHarness for testing command generation
type MockHarness struct{}

func (m *MockHarness) Name() string { return "mock" }
func (m *MockHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	return api.HarnessAdvancedCapabilities{Harness: "mock"}
}
func (m *MockHarness) GetCommand(task string, resume bool, args []string) []string {
	return []string{"/bin/echo", "hello"}
}
func (m *MockHarness) GetEnv(agentName, homeDir, username string) map[string]string {
	return nil
}
func (m *MockHarness) DefaultConfigDir() string              { return ".mock" }
func (m *MockHarness) SkillsDir() string                     { return ".mock/skills" }
func (m *MockHarness) HasSystemPrompt(agentHome string) bool { return false }
func (m *MockHarness) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	return nil
}
func (m *MockHarness) GetInterruptKey() string                                        { return "C-c" }
func (m *MockHarness) GetInterruptSequence() []string                                 { return nil }
func (m *MockHarness) GetHarnessEmbedsFS() (embed.FS, string)                         { return embed.FS{}, "" }
func (m *MockHarness) InjectAgentInstructions(agentHome string, content []byte) error { return nil }
func (m *MockHarness) InjectSystemPrompt(agentHome string, content []byte) error      { return nil }
func (m *MockHarness) GetTelemetryEnv() map[string]string                             { return nil }
func (m *MockHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	return &api.ResolvedAuth{Method: "mock"}, nil
}

func TestKubernetesRuntime_Run_Tmux(t *testing.T) {
	// Setup
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	config := RunConfig{
		Name:    "tmux-agent",
		Image:   "test-image",
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
	// Check Command — the pod uses a startup gate pattern:
	//   Command: ["sh", "-c", "<gate loop that waits for /tmp/.scion-home-ready>"]
	// The real startup command (sciontool init → tmux) is in the SCION_START_CMD env var.
	if len(pod.Spec.Containers) == 0 {
		t.Fatal("Pod has no containers")
	}
	cmd := pod.Spec.Containers[0].Command
	if len(cmd) != 3 {
		t.Fatalf("Expected gate command [sh -c <script>], got %v", cmd)
	}
	if cmd[0] != "sh" || cmd[1] != "-c" {
		t.Errorf("Expected gate command to start with 'sh -c', got %v", cmd[:2])
	}
	gateScript := cmd[2]
	if !strings.Contains(gateScript, "/tmp/.scion-home-ready") {
		t.Errorf("Gate script should poll for /tmp/.scion-home-ready, got: %s", gateScript)
	}
	if !strings.Contains(gateScript, "sciontool init") {
		t.Errorf("Gate script should exec sciontool init, got: %s", gateScript)
	}
	if !strings.Contains(gateScript, "SCION_START_CMD") {
		t.Errorf("Gate script should reference SCION_START_CMD env var, got: %s", gateScript)
	}

	// Check that the real tmux command is in the SCION_START_CMD env var
	var startCmd string
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "SCION_START_CMD" {
			startCmd = env.Value
			break
		}
	}
	if startCmd == "" {
		t.Fatal("SCION_START_CMD env var not set")
	}
	if !strings.Contains(startCmd, "tmux new-session -d -s scion -n agent") {
		t.Errorf("Expected tmux new-session with agent window, got: %s", startCmd)
	}
	if !strings.Contains(startCmd, "new-window -t scion -n shell") {
		t.Errorf("Expected shell window creation, got: %s", startCmd)
	}
	if !strings.Contains(startCmd, "/bin/echo") || !strings.Contains(startCmd, "hello") {
		t.Errorf("Wrapped command does not contain harness command. Got: %s", startCmd)
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

	// Wait for Run to return. The fake K8s clientset doesn't support exec,
	// so the startup gate signal will fail — that's expected in tests. We
	// only care about the pod spec assertions above.
	select {
	case err := <-errChan:
		if err != nil && !strings.Contains(err.Error(), "startup gate") {
			t.Errorf("Run failed with unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run timed out waiting for pod ready")
	}
}
