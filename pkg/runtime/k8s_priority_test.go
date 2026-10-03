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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// --- buildPod: priorityClassName ---

func TestBuildPod_PriorityClassName_Unset(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if pod.Spec.PriorityClassName != "" {
		t.Errorf("expected empty PriorityClassName, got %q", pod.Spec.PriorityClassName)
	}
}

func TestBuildPod_PriorityClassName_FromTemplate(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			PriorityClassName: "scion-agent-priority",
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if pod.Spec.PriorityClassName != "scion-agent-priority" {
		t.Errorf("expected PriorityClassName 'scion-agent-priority', got %q", pod.Spec.PriorityClassName)
	}
}

func TestBuildPod_PriorityClassName_RuntimeDefault(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.PriorityClassName = "runtime-default-priority"

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if pod.Spec.PriorityClassName != "runtime-default-priority" {
		t.Errorf("expected PriorityClassName 'runtime-default-priority', got %q", pod.Spec.PriorityClassName)
	}
}

func TestBuildPod_PriorityClassName_TemplateOverridesRuntimeDefault(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.PriorityClassName = "runtime-default-priority"

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			PriorityClassName: "explicit-priority",
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}
	if pod.Spec.PriorityClassName != "explicit-priority" {
		t.Errorf("expected the explicit template value to win, got %q", pod.Spec.PriorityClassName)
	}
}

func TestBuildPod_PriorityClassName_Invalid(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	tests := []string{
		"Invalid_Name",  // uppercase and underscore not allowed
		"-leading-dash", // must start/end alphanumeric
		"trailing-dash-",
		"has a space",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			config := RunConfig{
				Name:         "test-agent",
				Image:        "test:latest",
				UnixUsername: "scion",
				Kubernetes: &api.KubernetesConfig{
					PriorityClassName: name,
				},
			}
			_, err := rt.buildPod("default", config)
			if err == nil {
				t.Errorf("expected error for invalid priorityClassName %q, got nil", name)
				return
			}
			if !strings.Contains(err.Error(), "kubernetes.priorityClassName") {
				t.Errorf("expected error to name the template/agent-config source (kubernetes.priorityClassName), got: %v", err)
			}
		})
	}
}

func TestBuildPod_PriorityClassName_InvalidRuntimeDefault_NamesRuntimeSource(t *testing.T) {
	// When the bad value came from the runtime-level default rather than
	// the template/agent config, the error must name that source instead of
	// always blaming kubernetes.priorityClassName.
	rt, _, _ := newTestK8sRuntime()
	rt.PriorityClassName = "Invalid_Runtime_Default"

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	_, err := rt.buildPod("default", config)
	if err == nil {
		t.Fatal("expected error for invalid runtime-default priorityClassName, got nil")
	}
	if !strings.Contains(err.Error(), "runtimes.<name>.priority_class_name") {
		t.Errorf("expected error to name the runtime-default source (runtimes.<name>.priority_class_name), got: %v", err)
	}
	if strings.Contains(err.Error(), "kubernetes.priorityClassName") {
		t.Errorf("error must not blame the template/agent-config source when the value came from the runtime default, got: %v", err)
	}
}

// --- List(): preemption/eviction status mapping ---

func newPodForDisruptionTest(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"scion.name": name},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: "test:latest"}}},
	}
}

func listSingleAgent(t *testing.T, pod *corev1.Pod) api.AgentInfo {
	t.Helper()
	clientset := k8sfake.NewClientset()
	_, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)
	rt := NewKubernetesRuntime(client)

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	return agents[0]
}

func TestList_ExitReason_EvictedPodStatusReason(t *testing.T) {
	pod := newPodForDisruptionTest("agent-evicted", corev1.PodFailed)
	pod.Status.Reason = "Evicted"
	pod.Status.Message = "The node was low on resource: memory."

	info := listSingleAgent(t, pod)
	if info.ExitReason != string(state.ExitReasonEvicted) {
		t.Errorf("expected ExitReason %q, got %q", state.ExitReasonEvicted, info.ExitReason)
	}
	if info.Phase != string(state.PhaseError) {
		t.Errorf("expected Phase %q, got %q", state.PhaseError, info.Phase)
	}
}

func TestList_ExitReason_DisruptionTargetReasons(t *testing.T) {
	tests := []struct {
		name           string
		reason         string
		wantExitReason string
	}{
		{"preemption-by-scheduler", "PreemptionByScheduler", string(state.ExitReasonPreempted)},
		{"termination-by-kubelet", "TerminationByKubelet", string(state.ExitReasonEvicted)},
		{"eviction-by-eviction-api", "EvictionByEvictionAPI", string(state.ExitReasonEvicted)},
		{"future-unknown-reason", "DeletionByTaintManager", string(state.ExitReasonEvicted)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPodForDisruptionTest("agent-"+tt.name, corev1.PodFailed)
			pod.Status.Conditions = []corev1.PodCondition{
				{
					Type:   corev1.DisruptionTarget,
					Status: corev1.ConditionTrue,
					Reason: tt.reason,
				},
			}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{
					Name: agentContainerName,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 137,
							Reason:   "Error",
						},
					},
				},
			}

			info := listSingleAgent(t, pod)
			if info.ExitReason != tt.wantExitReason {
				t.Errorf("expected ExitReason %q, got %q", tt.wantExitReason, info.ExitReason)
			}
		})
	}
}

func TestList_ExitReason_DisruptionTargetOnRunningPod_NotYetTerminal(t *testing.T) {
	// A DisruptionTarget condition can appear while the pod is still running
	// out its grace period. The pod has not stopped yet, so List must not
	// report preempted/evicted until the pod actually reaches a terminal
	// phase.
	pod := newPodForDisruptionTest("agent-still-running", corev1.PodRunning)
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.DisruptionTarget,
			Status: corev1.ConditionTrue,
			Reason: "PreemptionByScheduler",
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != "" {
		t.Errorf("expected no ExitReason while pod is still running, got %q", info.ExitReason)
	}
}

func TestList_ExitReason_CommittedDisruption_RunningWithDeletionTimestamp(t *testing.T) {
	// Scheduler preemption and the eviction API delete the pod object
	// outright once termination completes, often before any heartbeat
	// observes a terminal phase (List() polls, it does not watch). Once the
	// pod has a deletionTimestamp and a live DisruptionTarget condition, it
	// is already committed to that termination, so List must report the
	// reason ahead of the pod actually stopping — without claiming the
	// agent has stopped.
	now := metav1.Now()
	pod := newPodForDisruptionTest("agent-committed-disruption", corev1.PodRunning)
	pod.DeletionTimestamp = &now
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.DisruptionTarget,
			Status: corev1.ConditionTrue,
			Reason: "PreemptionByScheduler",
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != string(state.ExitReasonPreempted) {
		t.Errorf("expected ExitReason %q, got %q", state.ExitReasonPreempted, info.ExitReason)
	}
	if info.Phase != "" {
		t.Errorf("expected no Phase (agent has not stopped), got %q", info.Phase)
	}
}

func TestList_ExitReason_DeletionTimestampWithoutDisruptionTarget_Ignored(t *testing.T) {
	// A deletionTimestamp alone (an ordinary scion rm / stop delete) must
	// not be mistaken for a disruption — only a live DisruptionTarget
	// condition makes it one.
	now := metav1.Now()
	pod := newPodForDisruptionTest("agent-ordinary-delete", corev1.PodRunning)
	pod.DeletionTimestamp = &now

	info := listSingleAgent(t, pod)
	if info.ExitReason != "" {
		t.Errorf("expected no ExitReason for an ordinary delete, got %q", info.ExitReason)
	}
}

func TestList_ExitReason_NormalCrashUnaffected(t *testing.T) {
	// A plain non-zero exit with no disruption signal must still report
	// "crashed" — the new mapping must not change existing crash reporting.
	pod := newPodForDisruptionTest("agent-crashed", corev1.PodFailed)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: agentContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Reason:   "Error",
				},
			},
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != string(state.ExitReasonCrashed) {
		t.Errorf("expected ExitReason %q, got %q", state.ExitReasonCrashed, info.ExitReason)
	}
}

func TestList_ExitReason_NormalStopUnaffected(t *testing.T) {
	// A clean exit (code 0) must still report no ExitReason at all.
	pod := newPodForDisruptionTest("agent-stopped", corev1.PodSucceeded)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: agentContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0,
					Reason:   "Completed",
				},
			},
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != "" {
		t.Errorf("expected no ExitReason for a clean stop, got %q", info.ExitReason)
	}
	if info.Phase != string(state.PhaseStopped) {
		t.Errorf("expected Phase %q, got %q", state.PhaseStopped, info.Phase)
	}
}

func TestList_ExitReason_GracefulPreemption_ExitZero(t *testing.T) {
	// Preemption and the eviction API delete the pod gracefully (SIGTERM),
	// so the agent container usually exits 0 — the pod reaches PodSucceeded,
	// not PodFailed. List must still report "preempted" here: a guard that
	// only checked for PhaseError (and not PhaseStopped) would miss this,
	// the exact symptom in #2528.
	pod := newPodForDisruptionTest("agent-graceful-preemption", corev1.PodSucceeded)
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.DisruptionTarget,
			Status: corev1.ConditionTrue,
			Reason: "PreemptionByScheduler",
		},
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: agentContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0,
					Reason:   "Completed",
				},
			},
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != string(state.ExitReasonPreempted) {
		t.Errorf("expected ExitReason %q, got %q", state.ExitReasonPreempted, info.ExitReason)
	}
	if info.Phase != string(state.PhaseStopped) {
		t.Errorf("expected Phase %q, got %q", state.PhaseStopped, info.Phase)
	}
}

func TestList_ExitReason_DisruptionTargetConditionFalse_Ignored(t *testing.T) {
	// A DisruptionTarget condition with Status=False means the disruption
	// was considered but is not in effect (e.g. it was later cleared); it
	// must not be treated as a live disruption signal.
	pod := newPodForDisruptionTest("agent-disruption-false", corev1.PodFailed)
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.DisruptionTarget,
			Status: corev1.ConditionFalse,
			Reason: "PreemptionByScheduler",
		},
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: agentContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Reason:   "Error",
				},
			},
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != string(state.ExitReasonCrashed) {
		t.Errorf("expected ExitReason %q (DisruptionTarget=False must be ignored), got %q", state.ExitReasonCrashed, info.ExitReason)
	}
}

func TestList_ExitReason_OtherConditionType_NotMistakenForDisruption(t *testing.T) {
	// A terminal pod can carry other conditions unrelated to disruption
	// (e.g. Ready=False with reason PodCompleted, which every pod that ran
	// to completion has). Only the DisruptionTarget condition type counts.
	pod := newPodForDisruptionTest("agent-other-condition", corev1.PodSucceeded)
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
			Reason: "PodCompleted",
		},
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: agentContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0,
					Reason:   "Completed",
				},
			},
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != "" {
		t.Errorf("expected no ExitReason for an ordinary stop with an unrelated condition, got %q", info.ExitReason)
	}
}

func TestList_ExitReason_OOMKilled_IsCrashedNotEvicted(t *testing.T) {
	// An OOM kill is a node-local kubelet action on the container, not a
	// disruption of the pod (no DisruptionTarget condition, no pod-level
	// Evicted reason). It must keep reading as an ordinary crash.
	pod := newPodForDisruptionTest("agent-oom", corev1.PodFailed)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			Name: agentContainerName,
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137,
					Reason:   "OOMKilled",
				},
			},
		},
	}

	info := listSingleAgent(t, pod)
	if info.ExitReason != string(state.ExitReasonCrashed) {
		t.Errorf("expected ExitReason %q for an OOM kill, got %q", state.ExitReasonCrashed, info.ExitReason)
	}
}
