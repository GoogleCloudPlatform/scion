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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// withDisruptionStorage sets the pod's workspace volume and home storage
// annotation the way buildPod does for each backend.
func withDisruptionStorage(pod *corev1.Pod, pvcWorkspace, nfsHome bool) *corev1.Pod {
	ws := corev1.Volume{Name: "workspace"}
	if pvcWorkspace {
		ws.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "scion-nfs"}
	} else {
		ws.EmptyDir = &corev1.EmptyDirVolumeSource{}
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, ws)
	pod.Annotations = withHomeStorageAnnotation(pod.Annotations, nfsHome)
	return pod
}

func TestK8sDisruptionPhase_StorageCombinations(t *testing.T) {
	tests := []struct {
		name         string
		pvcWorkspace bool
		nfsHome      bool
		want         state.Phase
	}{
		{"pvc-workspace-nfs-home", true, true, state.PhaseStopped},
		{"pvc-workspace-local-home", true, false, state.PhaseStopped},
		{"emptydir-workspace-nfs-home", false, true, state.PhaseError},
		{"emptydir-workspace-local-home", false, false, state.PhaseError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := withDisruptionStorage(newPodForDisruptionTest("agent-"+tt.name, corev1.PodFailed), tt.pvcWorkspace, tt.nfsHome)
			if got := k8sDisruptionPhase(pod); got != tt.want {
				t.Errorf("k8sDisruptionPhase = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestK8sDisruptionPhase_NoWorkspaceVolumeIsError(t *testing.T) {
	pod := newPodForDisruptionTest("agent-no-ws", corev1.PodFailed)
	if got := k8sDisruptionPhase(pod); got != state.PhaseError {
		t.Errorf("k8sDisruptionPhase = %q, want %q", got, state.PhaseError)
	}
}

// TestList_DisruptionPhase covers the phase List reports for preempted and
// evicted pods, both once terminal and while committed to termination
// (deletionTimestamp + DisruptionTarget), for persistent and emptyDir
// workspaces (ptone/scion#2669).
func TestList_DisruptionPhase(t *testing.T) {
	type podState int
	const (
		terminal podState = iota
		committed
	)
	tests := []struct {
		name         string
		state        podState
		disruption   string // DisruptionTarget reason, or "Evicted" for the status reason
		pvcWorkspace bool
		wantPhase    state.Phase
		wantReason   state.ExitReason
	}{
		{"preempted-terminal-pvc", terminal, corev1.PodReasonPreemptionByScheduler, true, state.PhaseStopped, state.ExitReasonPreempted},
		{"preempted-terminal-emptydir", terminal, corev1.PodReasonPreemptionByScheduler, false, state.PhaseError, state.ExitReasonPreempted},
		{"preempted-committed-pvc", committed, corev1.PodReasonPreemptionByScheduler, true, state.PhaseStopped, state.ExitReasonPreempted},
		{"preempted-committed-emptydir", committed, corev1.PodReasonPreemptionByScheduler, false, state.PhaseError, state.ExitReasonPreempted},
		{"evicted-status-terminal-pvc", terminal, "Evicted", true, state.PhaseStopped, state.ExitReasonEvicted},
		{"evicted-status-terminal-emptydir", terminal, "Evicted", false, state.PhaseError, state.ExitReasonEvicted},
		{"eviction-api-committed-pvc", committed, "EvictionByEvictionAPI", true, state.PhaseStopped, state.ExitReasonEvicted},
		{"kubelet-terminal-emptydir", terminal, "TerminationByKubelet", false, state.PhaseError, state.ExitReasonEvicted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase := corev1.PodFailed
			if tt.state == committed {
				phase = corev1.PodRunning
			}
			pod := withDisruptionStorage(newPodForDisruptionTest("agent-"+tt.name, phase), tt.pvcWorkspace, false)
			if tt.state == committed {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				// The fake clientset keeps a deletionTimestamp only with a finalizer.
				pod.Finalizers = []string{"example.com/hold"}
			} else {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  agentContainerName,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143, Reason: "Error"}},
				}}
			}
			if tt.disruption == "Evicted" {
				pod.Status.Reason = "Evicted"
			} else {
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: tt.disruption}}
			}

			info := listSingleAgent(t, pod)
			if info.Phase != string(tt.wantPhase) {
				t.Errorf("Phase = %q, want %q", info.Phase, tt.wantPhase)
			}
			if info.ExitReason != string(tt.wantReason) {
				t.Errorf("ExitReason = %q, want %q", info.ExitReason, tt.wantReason)
			}
		})
	}
}

// A failed pod with no disruption signal keeps the existing mapping (error,
// crashed) even with a persistent workspace, and an ordinary delete of a
// running pod is not reported as stopped.
func TestList_DisruptionPhase_NoDisruptionUnchanged(t *testing.T) {
	crashed := withDisruptionStorage(newPodForDisruptionTest("agent-crash-pvc", corev1.PodFailed), true, true)
	crashed.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  agentContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
	}}
	info := listSingleAgent(t, crashed)
	if info.Phase != string(state.PhaseError) || info.ExitReason != string(state.ExitReasonCrashed) {
		t.Errorf("crashed pod: Phase=%q ExitReason=%q, want error/crashed", info.Phase, info.ExitReason)
	}

	deleting := withDisruptionStorage(newPodForDisruptionTest("agent-delete-pvc", corev1.PodRunning), true, true)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"example.com/hold"}
	info = listSingleAgent(t, deleting)
	if info.Phase != "" || info.ExitReason != "" {
		t.Errorf("ordinary delete: Phase=%q ExitReason=%q, want empty", info.Phase, info.ExitReason)
	}
}
