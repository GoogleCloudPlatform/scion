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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesRuntime_List(t *testing.T) {
	// Create a fake clientset
	clientset := k8sfake.NewClientset()

	// Create a pod that mimics what we expect
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
			UID:       "test-agent-uid-1234",
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
					Name:  agentContainerName,
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

	if agents[0].Kubernetes == nil {
		t.Fatal("expected a non-nil Kubernetes block")
	}
	if agents[0].Kubernetes.UID != string(pod.UID) {
		t.Errorf("expected Kubernetes.UID %q, got %q", pod.UID, agents[0].Kubernetes.UID)
	}
}

func TestKubernetesRuntime_List_SelectorUsesProjectLabels(t *testing.T) {
	clientset := k8sfake.NewClientset()

	var capturedSelector string
	clientset.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if listAction, ok := action.(k8stesting.ListActionImpl); ok {
			capturedSelector = listAction.GetListOptions().LabelSelector
		}
		return false, nil, nil
	})

	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	_, err := r.List(context.Background(), map[string]string{
		projectkeys.LabelProject:   "myproject",
		projectkeys.LabelProjectID: "proj-123",
	})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	wantParts := []string{"scion.project=myproject", "scion.project_id=proj-123"}
	gotParts := strings.Split(capturedSelector, ",")
	sort.Strings(gotParts)
	sort.Strings(wantParts)
	if strings.Join(gotParts, ",") != strings.Join(wantParts, ",") {
		t.Errorf("selector = %q, want (in any order) %q", capturedSelector, strings.Join(wantParts, ","))
	}
	if strings.Contains(capturedSelector, "grove") {
		t.Errorf("selector %q must not reference grove labels", capturedSelector)
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

	// Verify structured ExitCode and ExitReason fields (Phase 1 of #1257).
	if got["completed-agent"].ExitCode == nil {
		t.Error("completed-agent ExitCode should be set for terminated pod")
	} else if *got["completed-agent"].ExitCode != 0 {
		t.Errorf("completed-agent ExitCode = %d, want 0", *got["completed-agent"].ExitCode)
	}
	if got["completed-agent"].ExitReason != "" {
		t.Errorf("completed-agent ExitReason = %q, want empty (clean exit)", got["completed-agent"].ExitReason)
	}

	if got["failed-agent"].ExitCode == nil {
		t.Error("failed-agent ExitCode should be set for terminated pod")
	} else if *got["failed-agent"].ExitCode != 1 {
		t.Errorf("failed-agent ExitCode = %d, want 1", *got["failed-agent"].ExitCode)
	}
	if got["failed-agent"].ExitReason != "crashed" {
		t.Errorf("failed-agent ExitReason = %q, want %q", got["failed-agent"].ExitReason, "crashed")
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

// TestKubernetesRuntime_BuildPod_DedupesEnv proves that SCION_AGENT_NAME,
// GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION each appear exactly once in
// the built pod spec's container env, even though they are legitimately
// contributed by two different sources (harness env / config.Env for
// SCION_AGENT_NAME; config.Env / resolved auth for the GCP vars). It also
// asserts every env name in the pod is unique, and that the three vars keep
// the value Kubernetes would already select today for duplicate env names:
// the last entry appended wins.
func TestKubernetesRuntime_BuildPod_DedupesEnv(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Harness:      &MockHarness{Env: map[string]string{"SCION_AGENT_NAME": "agent-from-harness"}},
		Env: []string{
			"SCION_AGENT_NAME=agent-from-config-env",
			"GOOGLE_CLOUD_PROJECT=project-from-config-env",
			"GOOGLE_CLOUD_REGION=region-from-config-env",
		},
		ResolvedAuth: &api.ResolvedAuth{
			Method: "vertex-ai",
			EnvVars: map[string]string{
				"GOOGLE_CLOUD_PROJECT": "project-from-resolved-auth",
				"GOOGLE_CLOUD_REGION":  "region-from-resolved-auth",
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	envVars := pod.Spec.Containers[0].Env

	counts := make(map[string]int, len(envVars))
	values := make(map[string]string, len(envVars))
	for _, e := range envVars {
		counts[e.Name]++
		values[e.Name] = e.Value
	}

	for name, count := range counts {
		if count > 1 {
			t.Errorf("env var %q appears %d times in pod spec, want 1", name, count)
		}
	}

	// The two sources for these vars disagree on purpose; the effective
	// value must match what Kubernetes already picks today for duplicate
	// env names — the last one appended.
	wantEffective := map[string]string{
		"SCION_AGENT_NAME":     "agent-from-config-env",
		"GOOGLE_CLOUD_PROJECT": "project-from-resolved-auth",
		"GOOGLE_CLOUD_REGION":  "region-from-resolved-auth",
	}
	for name, want := range wantEffective {
		if got := values[name]; got != want {
			t.Errorf("effective value of %s = %q, want %q", name, got, want)
		}
	}
}

// TestDedupeEnvVars pins the exact semantics of dedupeEnvVars: a duplicated
// name keeps only its last occurrence, in that occurrence's original
// position (not hoisted to the first), unless an entry from its first
// through its last occurrence contains the substring "$(NAME)", in which
// case all its occurrences are kept; the cases below also pin the bounds of
// that exception. Non-duplicated entries keep their relative order
// untouched.
func TestDedupeEnvVars(t *testing.T) {
	secretRef := &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "s"},
			Key:                  "k",
		},
	}

	tests := []struct {
		name  string
		input []corev1.EnvVar
		want  []corev1.EnvVar
	}{
		{
			name:  "nil input returns nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "empty input returns empty",
			input: []corev1.EnvVar{},
			want:  []corev1.EnvVar{},
		},
		{
			name: "no duplicates preserves order",
			input: []corev1.EnvVar{
				{Name: "A", Value: "1"},
				{Name: "B", Value: "2"},
				{Name: "C", Value: "3"},
			},
			want: []corev1.EnvVar{
				{Name: "A", Value: "1"},
				{Name: "B", Value: "2"},
				{Name: "C", Value: "3"},
			},
		},
		{
			name: "duplicate survivor keeps its last position, not the first",
			input: []corev1.EnvVar{
				{Name: "FOO", Value: "first"},
				{Name: "BAR", Value: "unrelated"},
				{Name: "FOO", Value: "second"},
			},
			want: []corev1.EnvVar{
				{Name: "BAR", Value: "unrelated"},
				{Name: "FOO", Value: "second"},
			},
		},
		{
			name: "three occurrences keeps only the last",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "X", Value: "b"},
				{Name: "X", Value: "c"},
			},
			want: []corev1.EnvVar{
				{Name: "X", Value: "c"},
			},
		},
		{
			name: "later Value entry replaces earlier ValueFrom entry",
			input: []corev1.EnvVar{
				{Name: "SECRET", ValueFrom: secretRef},
				{Name: "SECRET", Value: "plain"},
			},
			want: []corev1.EnvVar{
				{Name: "SECRET", Value: "plain"},
			},
		},
		{
			name: "later ValueFrom entry replaces earlier Value entry",
			input: []corev1.EnvVar{
				{Name: "SECRET", Value: "plain"},
				{Name: "SECRET", ValueFrom: secretRef},
			},
			want: []corev1.EnvVar{
				{Name: "SECRET", ValueFrom: secretRef},
			},
		},
		{
			// The survivor references its own earlier value via $(X), e.g.
			// NODE_OPTIONS=$(NODE_OPTIONS) --foo appended after an earlier
			// NODE_OPTIONS. Dropping the earlier occurrence would leave
			// $(X) unresolved (kubelet does not fall back to a later
			// occurrence), which changes behavior. Both must be kept.
			name: "self-reference in survivor keeps both occurrences",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "X", Value: "$(X)b"},
			},
			want: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "X", Value: "$(X)b"},
			},
		},
		{
			// An entry between X's two occurrences references $(X). That
			// entry needs the earlier X value, so the earlier occurrence
			// must be kept even though X itself would otherwise dedupe.
			name: "intermediate reference to duplicated name keeps all occurrences",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "$(X)"},
				{Name: "X", Value: "b"},
			},
			want: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "$(X)"},
				{Name: "X", Value: "b"},
			},
		},
		{
			// An escaped $$(X) is not a kubelet reference (it expands to
			// the literal "$(X)"), but the substring check still counts
			// it. That's intentionally conservative: it only keeps
			// duplicates it didn't need to, never changes expansion.
			name: "escaped reference is conservatively treated as a reference",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "$$(X)"},
				{Name: "X", Value: "b"},
			},
			want: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "$$(X)"},
				{Name: "X", Value: "b"},
			},
		},
		{
			// A reference to a *different* name (Z, never duplicated) must
			// not stop X from collapsing.
			name: "reference to unrelated name does not block collapse",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "$(Z)"},
				{Name: "X", Value: "b"},
			},
			want: []corev1.EnvVar{
				{Name: "Y", Value: "$(Z)"},
				{Name: "X", Value: "b"},
			},
		},
		{
			// $(XY) is not the substring "$(X)": a name-boundary-aware
			// match must not treat this as a reference to X.
			name: "reference to a longer name sharing a prefix does not block collapse",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "$(XY)"},
				{Name: "X", Value: "b"},
			},
			want: []corev1.EnvVar{
				{Name: "Y", Value: "$(XY)"},
				{Name: "X", Value: "b"},
			},
		},
		{
			// A reference to a longer name ending in X: $(YX) is not the
			// substring "$(X)" either — a suffix match would wrongly treat
			// it as one.
			name: "reference to a longer name ending in X does not block collapse",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "$(YX)"},
				{Name: "X", Value: "b"},
			},
			want: []corev1.EnvVar{
				{Name: "Y", Value: "$(YX)"},
				{Name: "X", Value: "b"},
			},
		},
		{
			// Text without the leading $ is not a reference.
			name: "text without the leading dollar sign is not a reference",
			input: []corev1.EnvVar{
				{Name: "X", Value: "a"},
				{Name: "Y", Value: "(X)"},
				{Name: "X", Value: "b"},
			},
			want: []corev1.EnvVar{
				{Name: "Y", Value: "(X)"},
				{Name: "X", Value: "b"},
			},
		},
		{
			// A reference positioned after X's last occurrence resolves
			// against the survivor either way; it must not force X's
			// earlier occurrence to be kept (the scan must stop at last).
			name: "reference after the last occurrence does not block collapse",
			input: []corev1.EnvVar{
				{Name: "X", Value: "1"},
				{Name: "X", Value: "2"},
				{Name: "Y", Value: "$(X)"},
			},
			want: []corev1.EnvVar{
				{Name: "X", Value: "2"},
				{Name: "Y", Value: "$(X)"},
			},
		},
		{
			// A reference positioned before X's first occurrence resolves
			// via service-link fallback or stays literal either way; it
			// must not force an occurrence to be kept (the scan must not
			// start before first).
			name: "reference before the first occurrence does not block collapse",
			input: []corev1.EnvVar{
				{Name: "Y", Value: "$(X)"},
				{Name: "X", Value: "a"},
				{Name: "X", Value: "b"},
			},
			want: []corev1.EnvVar{
				{Name: "Y", Value: "$(X)"},
				{Name: "X", Value: "b"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupeEnvVars(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("dedupeEnvVars(%+v) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDefaultKubernetesNamespace(t *testing.T) {
	t.Run("env overrides default", func(t *testing.T) {
		t.Setenv("POD_NAMESPACE", "scion")
		t.Setenv("SCION_K8S_NAMESPACE", "")
		if got := defaultKubernetesNamespace(); got != "scion" {
			t.Fatalf("defaultKubernetesNamespace() = %q, want %q", got, "scion")
		}
	})

	t.Run("serviceaccount file used when env missing", func(t *testing.T) {
		t.Setenv("POD_NAMESPACE", "")
		t.Setenv("SCION_K8S_NAMESPACE", "")

		tmpDir := t.TempDir()
		nsFile := filepath.Join(tmpDir, "namespace")
		if err := os.WriteFile(nsFile, []byte("scion-from-file\n"), 0644); err != nil {
			t.Fatalf("failed to write temp namespace file: %v", err)
		}

		prev := serviceAccountNamespacePath
		serviceAccountNamespacePath = nsFile
		defer func() { serviceAccountNamespacePath = prev }()

		if got := defaultKubernetesNamespace(); got != "scion-from-file" {
			t.Fatalf("defaultKubernetesNamespace() = %q, want %q", got, "scion-from-file")
		}
	})

	t.Run("default fallback", func(t *testing.T) {
		t.Setenv("POD_NAMESPACE", "")
		t.Setenv("SCION_K8S_NAMESPACE", "")

		prev := serviceAccountNamespacePath
		serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing")
		defer func() { serviceAccountNamespacePath = prev }()

		if got := defaultKubernetesNamespace(); got != "default" {
			t.Fatalf("defaultKubernetesNamespace() = %q, want %q", got, "default")
		}
	})
}

func TestNewKubernetesRuntime_UsesDetectedNamespace(t *testing.T) {
	clientset := k8sfake.NewClientset()
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)

	t.Setenv("POD_NAMESPACE", "scion")
	t.Setenv("SCION_K8S_NAMESPACE", "")

	r := NewKubernetesRuntime(client)
	if r.DefaultNamespace != "scion" {
		t.Fatalf("DefaultNamespace = %q, want %q", r.DefaultNamespace, "scion")
	}
}
