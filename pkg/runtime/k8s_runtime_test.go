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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

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

// TestChownRecursiveArgs covers the shared helper both in-pod chown call
// sites use: an empty owner must be refused outright rather than build a
// command with no real target user, and a non-empty owner must produce the
// expected `chown -R owner:owner path` argv -- as separate argv elements,
// not a shell string, so a path or owner containing shell metacharacters is
// never given a shell to be interpreted by.
func TestChownRecursiveArgs(t *testing.T) {
	tests := []struct {
		name     string
		owner    string
		path     string
		wantArgs []string
		wantOK   bool
	}{
		{name: "empty owner, home path", owner: "", path: "/home", wantArgs: nil, wantOK: false},
		{name: "empty owner, workspace path", owner: "", path: "/workspace", wantArgs: nil, wantOK: false},
		{name: "non-empty owner", owner: "scion", path: "/workspace", wantArgs: []string{"chown", "-R", "scion:scion", "/workspace"}, wantOK: true},
		{name: "path with shell metacharacters is passed through literally", owner: "scion", path: "/workspace; rm -rf /", wantArgs: []string{"chown", "-R", "scion:scion", "/workspace; rm -rf /"}, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotArgs, gotOK := chownRecursiveArgs(tt.owner, tt.path)
			if !slices.Equal(gotArgs, tt.wantArgs) || gotOK != tt.wantOK {
				t.Errorf("chownRecursiveArgs(%q, %q) = (%v, %v), want (%v, %v)", tt.owner, tt.path, gotArgs, gotOK, tt.wantArgs, tt.wantOK)
			}
		})
	}
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

// TestRun_RejectsUnsafeWorkspaceSource is the fail-closed regression test for
// the Run() call site: a workspace source that is not an allowed workspace
// path must be refused before any pod is created, not just logged.
func TestRun_RejectsUnsafeWorkspaceSource(t *testing.T) {
	clientset := k8sfake.NewClientset()
	// Guard-regression safety net, not something this test expects to hit:
	// if the workspace-source rejection below were ever removed, Run()
	// would proceed to actually create a pod and call waitForPodReady,
	// which polls Get in a loop that only stops on an error or its own
	// internal 10-minute timeout -- against a fake clientset with no real
	// controller ever advancing the pod to Ready, that would otherwise run
	// until ctx's deadline, or the full 600s go test package timeout if ctx
	// had none. Failing every Get on pods here, combined with the short
	// ctx deadline below, turns a reintroduced regression into a fast
	// assertion failure instead of a multi-minute hang.
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fmt.Errorf("test: no real pod backend behind the fake clientset")
	})
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	config := RunConfig{
		Name:         "test-agent-unsafe-workspace",
		Image:        "test-image",
		UnixUsername: "scion",
		Workspace:    "/",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.Run(ctx, config)
	if err == nil {
		t.Fatal("expected Run() to fail for a workspace source of '/'")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from workspace source validation, got: %v", err)
	}

	pods, listErr := clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("failed to list pods: %v", listErr)
	}
	if len(pods.Items) != 0 {
		t.Errorf("expected no pods created (fail closed), found %d", len(pods.Items))
	}
}

// TestRun_RejectsUnsafeHomeDir is TestRun_RejectsUnsafeWorkspaceSource's
// counterpart for config.HomeDir: a home directory that is not an allowed
// agent-home path must be refused before any pod is created, the same way,
// independent of the workspace check. Without this, an unacceptable home
// path would reach the pod on this first Run and only be caught afterward,
// on the next Sync.
func TestRun_RejectsUnsafeHomeDir(t *testing.T) {
	clientset := k8sfake.NewClientset()
	// Same guard-regression safety net as TestRun_RejectsUnsafeWorkspaceSource.
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, fmt.Errorf("test: no real pod backend behind the fake clientset")
	})
	scheme := k8sruntime.NewScheme()
	fc := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(fc, clientset)
	r := NewKubernetesRuntime(client)

	config := RunConfig{
		Name:         "test-agent-unsafe-homedir",
		Image:        "test-image",
		UnixUsername: "scion",
		HomeDir:      "/",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.Run(ctx, config)
	if err == nil {
		t.Fatal("expected Run() to fail for a home directory of '/'")
	}
	if !strings.Contains(err.Error(), "is not an allowed agent home path") {
		t.Errorf("expected the rejection to come from agent home source validation, got: %v", err)
	}

	pods, listErr := clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("failed to list pods: %v", listErr)
	}
	if len(pods.Items) != 0 {
		t.Errorf("expected no pods created (fail closed), found %d", len(pods.Items))
	}
}

// TestRun_AcceptsLegitimateScionHomeWorkspaces is the positive
// acceptance-set counterpart to TestRun_RejectsUnsafeWorkspaceSource: Run()
// has no per-project root to pass to the shared validator (see the no-root
// comment at its call site), so it depends entirely on the validator's named
// ~/.scion allow list to still admit real workspaces. This does not assert
// Run() succeeds end to end (that needs a fully-ready fake pod, out of scope
// here) -- it asserts the failure, if any, is not the workspace-source
// rejection, proving validation did not refuse a legitimate path.
func TestRun_AcceptsLegitimateScionHomeWorkspaces(t *testing.T) {
	tests := []struct {
		name    string
		relPath []string
	}{
		{name: "global project workspace", relPath: []string{".scion", "workspace"}},
		{name: "hub-managed project workspace", relPath: []string{".scion", "projects", "my-project", "workspace"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)

			parts := append([]string{tmpHome}, tt.relPath...)
			workspace := filepath.Join(parts...)
			if err := os.MkdirAll(workspace, 0755); err != nil {
				t.Fatal(err)
			}

			clientset := k8sfake.NewClientset()
			scheme := k8sruntime.NewScheme()
			fc := fake.NewSimpleDynamicClient(scheme)
			client := k8s.NewTestClient(fc, clientset)
			r := NewKubernetesRuntime(client)

			config := RunConfig{
				Name:         "test-agent",
				Image:        "test-image",
				UnixUsername: "scion",
				Workspace:    workspace,
			}

			// A short-lived context: validation happens before pod creation
			// and is what this test cares about. Run() passes this same
			// context into waitForPodReady, which otherwise polls for up to
			// 10 minutes against a fake clientset that never reports a pod
			// as ready -- the deadline here cuts that wait short instead of
			// hitting the full 10-minute real timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			_, err := r.Run(ctx, config)
			if err != nil && strings.Contains(err.Error(), "is not an allowed workspace path") {
				t.Errorf("expected %q to be accepted by workspace source validation, got: %v", workspace, err)
			}
		})
	}
}

// TestRun_AcceptsRealAgentHomes is TestRun_AcceptsLegitimateScionHomeWorkspaces's
// counterpart for config.HomeDir: the three real shapes
// config.GetAgentHomePath produces under ~/.scion, plus a plain in-repo
// project's own agent home (outside ~/.scion entirely, admitted through the
// fixed floors alone). Without this, TestRun_RejectsUnsafeHomeDir alone
// would not catch a validation root pinned too narrowly to admit only '/'
// while refusing every real home.
func TestRun_AcceptsRealAgentHomes(t *testing.T) {
	tests := []struct {
		name    string
		relPath []string
	}{
		{name: "global project agent home", relPath: []string{".scion", "agents", "a", "home"}},
		{name: "hub-managed project agent home", relPath: []string{".scion", "projects", "p", ".scion", "agents", "a", "home"}},
		{name: "externalized git project agent home", relPath: []string{".scion", "project-configs", "d__1", ".scion", "agents", "a", "home"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)

			parts := append([]string{tmpHome}, tt.relPath...)
			homeDir := filepath.Join(parts...)
			if err := os.MkdirAll(homeDir, 0755); err != nil {
				t.Fatal(err)
			}

			clientset := k8sfake.NewClientset()
			scheme := k8sruntime.NewScheme()
			fc := fake.NewSimpleDynamicClient(scheme)
			client := k8s.NewTestClient(fc, clientset)
			r := NewKubernetesRuntime(client)

			config := RunConfig{
				Name:         "test-agent",
				Image:        "test-image",
				UnixUsername: "scion",
				HomeDir:      homeDir,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			_, err := r.Run(ctx, config)
			if err != nil && (strings.Contains(err.Error(), "is not an allowed agent home path") || strings.Contains(err.Error(), "outside the permitted agent home root")) {
				t.Errorf("expected %q to be accepted by agent home source validation, got: %v", homeDir, err)
			}
		})
	}

	t.Run("in-repo project agent home", func(t *testing.T) {
		tmpDir := t.TempDir()
		homeDir := filepath.Join(tmpDir, "repo", ".scion", "agents", "a", "home")
		if err := os.MkdirAll(homeDir, 0755); err != nil {
			t.Fatal(err)
		}

		clientset := k8sfake.NewClientset()
		scheme := k8sruntime.NewScheme()
		fc := fake.NewSimpleDynamicClient(scheme)
		client := k8s.NewTestClient(fc, clientset)
		r := NewKubernetesRuntime(client)

		config := RunConfig{
			Name:         "test-agent",
			Image:        "test-image",
			UnixUsername: "scion",
			HomeDir:      homeDir,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		_, err := r.Run(ctx, config)
		if err != nil && (strings.Contains(err.Error(), "is not an allowed agent home path") || strings.Contains(err.Error(), "outside the permitted agent home root")) {
			t.Errorf("expected %q to be accepted by agent home source validation, got: %v", homeDir, err)
		}
	})
}

// newFakeAgentPodWithWorkspaceAnnotation builds a fake pod for Sync() tests:
// List() finds it via the scion.name label, and Sync() reads the workspace
// path straight from the scion.workspace annotation — exactly the "value
// read from a persisted config" shape this test is exercising, not a
// freshly-computed one.
func newFakeAgentPodWithWorkspaceAnnotation(name, workspacePath string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"scion.name": name,
			},
			Annotations: map[string]string{
				"scion.workspace": workspacePath,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: agentContainerName, Image: "test-image"},
			},
		},
	}
}

// newFakeAgentPodWithHomeDirAnnotation is newFakeAgentPodWithWorkspaceAnnotation
// plus the scion.homedir/scion.username annotations Sync() reads for the
// home-directory sync, with workspacePath fixed to a plain, outside-~/.scion
// value the rootless validator accepts unconditionally, so a test using this
// exercises only the homeDir check.
func newFakeAgentPodWithHomeDirAnnotation(name, homeDirPath string) *corev1.Pod {
	pod := newFakeAgentPodWithWorkspaceAnnotation(name, "/some/workspace")
	pod.Annotations["scion.homedir"] = homeDirPath
	pod.Annotations["scion.username"] = "scion"
	return pod
}

// TestSync_RejectsPersistedWorkspacePath is the fail-closed regression test
// for the Sync() call site: a workspace path read from an already-persisted
// pod annotation (as opposed to one freshly computed this call) must still
// be rejected. The assertion is on the specific validation error message,
// not just "err != nil" — Sync() would also return a non-nil error if
// rejection were skipped and the sync attempt itself failed (there is no
// real API server behind the fake clientset), so asserting the exact
// validation wording is what proves the sync was never attempted, not just
// that it failed for some other reason.
//
// Positive acceptance-set coverage for Sync() is not exercised end to end
// here: Sync() has no per-project root available (only the pod's
// annotations/labels are in scope), so it depends entirely on the
// validator's named ~/.scion allow list to still admit a real, persisted
// workspace path. That path can't be driven through the fake clientset --
// syncToPod issues its pod-exec request through
// r.Client.Clientset.CoreV1().RESTClient(), and the fake clientset's
// RESTClient() panics on that call (a nil-config dereference deep inside
// client-go) rather than returning an error, a limitation of the fake
// clientset's exec subresource, and not something to work around by
// reaching into client-go internals from a test. The
// acceptance-set proof for this call site is
// TestValidateWorkspaceSource_RootlessAcceptsScionProjectsSubtree
// (workspace_source_guard_test.go), which directly covers the same
// ~/.scion/projects/<slug>/... shape Sync() passes to the validator with no
// root, the same way this test proves the rejection path without needing
// the sync call to actually run.
func TestSync_RejectsPersistedWorkspacePath(t *testing.T) {
	for _, tt := range []struct {
		name      string
		direction SyncDirection
		bad       string
	}{
		{name: "SyncTo with root", direction: SyncTo, bad: "/"},
		{name: "SyncFrom with root", direction: SyncFrom, bad: "/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset()
			pod := newFakeAgentPodWithWorkspaceAnnotation("test-agent", tt.bad)
			if _, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create pod: %v", err)
			}

			scheme := k8sruntime.NewScheme()
			fc := fake.NewSimpleDynamicClient(scheme)
			client := k8s.NewTestClient(fc, clientset)
			r := NewKubernetesRuntime(client)

			err := r.Sync(context.Background(), "test-agent", tt.direction)
			if err == nil {
				t.Fatal("expected Sync to fail for a persisted workspace path of '/'")
			}
			if !strings.Contains(err.Error(), "is not an allowed workspace path") {
				t.Errorf("expected the rejection to come from workspace source validation, got: %v", err)
			}
		})
	}
}

// TestSync_RejectsPersistedHomeDirPath is TestSync_RejectsPersistedWorkspacePath's
// counterpart for the scion.homedir annotation: a home directory path read
// from an already-persisted pod annotation must still be rejected, the same
// way, independent of the workspace path check. This call site validates
// through ValidateAgentHomeSource, not ValidateWorkspaceSource: the error
// wording below ("agent home path", not "workspace path") is itself part of
// what proves the right validator ran -- before that fix, this call site
// used ValidateWorkspaceSource, which would have refused every real agent
// home under ~/.scion as well, just with the other function's wording.
//
// Positive acceptance-set coverage for Sync() is not exercised end to end
// here, for the same reason given in TestSync_RejectsPersistedWorkspacePath's
// own comment (the fake clientset's RESTClient() panics on the pod-exec call
// a successful validation would reach). The acceptance-set proof for this
// call site is TestValidateAgentHomeSource_AcceptsRealAgentHomeShapes
// (workspace_source_guard_test.go), which directly covers all three real
// home shapes this call site's homeDir value can take.
func TestSync_RejectsPersistedHomeDirPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	for _, tt := range []struct {
		name      string
		direction SyncDirection
		bad       string
	}{
		{name: "SyncTo with root", direction: SyncTo, bad: "/"},
		{name: "SyncFrom with root", direction: SyncFrom, bad: "/"},
		{name: "SyncTo with $HOME", direction: SyncTo, bad: home},
		{name: "SyncFrom with $HOME", direction: SyncFrom, bad: home},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clientset := k8sfake.NewClientset()
			pod := newFakeAgentPodWithHomeDirAnnotation("test-agent", tt.bad)
			if _, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("failed to create pod: %v", err)
			}

			scheme := k8sruntime.NewScheme()
			fc := fake.NewSimpleDynamicClient(scheme)
			client := k8s.NewTestClient(fc, clientset)
			r := NewKubernetesRuntime(client)

			err := r.Sync(context.Background(), "test-agent", tt.direction)
			if err == nil {
				t.Fatalf("expected Sync to fail for a persisted home directory path of %q", tt.bad)
			}
			if !strings.Contains(err.Error(), "is not an allowed agent home path") {
				t.Errorf("expected the rejection to come from agent home source validation, got: %v", err)
			}
		})
	}
}
