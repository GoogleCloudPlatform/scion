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

// TestList_PendingPodPhase (ptone/scion#5365): List reports a Pending pod
// as starting, so agent.Manager.List does not fall back to agent-info.json's
// "running" (written by Start before the pod exists) and a broker heartbeat
// does not report a pod that is still waiting for a node as running. A
// Running or Unknown pod keeps the empty phase (agent-info.json decides), a
// terminated agent container keeps its stopped/error mapping, and a
// committed disruption still reports its own phase and reason.
func TestList_PendingPodPhase(t *testing.T) {
	agentState := func(st corev1.ContainerState) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{{Name: agentContainerName, State: st}}
	}
	unschedulable := []corev1.PodCondition{{
		Type:   corev1.PodScheduled,
		Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable,
	}}
	committedPreemption := []corev1.PodCondition{{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionTrue,
		Reason: "PreemptionByScheduler",
	}}

	tests := []struct {
		name                string
		podPhase            corev1.PodPhase
		conditions          []corev1.PodCondition
		containerStatuses   []corev1.ContainerStatus
		initStatuses        []corev1.ContainerStatus
		deleting            bool
		wantPhase           string
		wantContainerStatus string
		wantExitReason      string
	}{
		{
			name:                "pending unschedulable awaiting capacity",
			podPhase:            corev1.PodPending,
			conditions:          unschedulable,
			wantPhase:           string(state.PhaseStarting),
			wantContainerStatus: "Pending",
		},
		{
			name:     "pending container creating",
			podPhase: corev1.PodPending,
			containerStatuses: agentState(corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
			}),
			wantPhase:           string(state.PhaseStarting),
			wantContainerStatus: "Pending (ContainerCreating)",
		},
		{
			name:     "pending init container running",
			podPhase: corev1.PodPending,
			initStatuses: []corev1.ContainerStatus{{
				Name:  "init",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
			containerStatuses: agentState(corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
			}),
			wantPhase:           string(state.PhaseStarting),
			wantContainerStatus: "Pending (PodInitializing)",
		},
		{
			name:     "running pod keeps empty phase",
			podPhase: corev1.PodRunning,
			containerStatuses: agentState(corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{},
			}),
			wantPhase:           "",
			wantContainerStatus: "Running",
		},
		{
			name:                "unknown pod keeps empty phase",
			podPhase:            corev1.PodUnknown,
			wantPhase:           "",
			wantContainerStatus: "Unknown",
		},
		{
			name:     "pending pod with terminated agent container keeps error",
			podPhase: corev1.PodPending,
			containerStatuses: agentState(corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
			}),
			wantPhase:           string(state.PhaseError),
			wantContainerStatus: "Pending (Error)",
			wantExitReason:      string(state.ExitReasonCrashed),
		},
		{
			name:     "succeeded pod keeps stopped",
			podPhase: corev1.PodSucceeded,
			containerStatuses: agentState(corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
			}),
			wantPhase:           string(state.PhaseStopped),
			wantContainerStatus: "Succeeded (Completed)",
		},
		{
			name:     "failed pod keeps error",
			podPhase: corev1.PodFailed,
			containerStatuses: agentState(corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
			}),
			wantPhase:           string(state.PhaseError),
			wantContainerStatus: "Failed (Error)",
			wantExitReason:      string(state.ExitReasonCrashed),
		},
		{
			// emptyDir workspace: a committed disruption ends in error
			// (k8sDisruptionPhase), ahead of the pod stopping.
			name:                "pending pod committed to preemption keeps the disruption phase",
			podPhase:            corev1.PodPending,
			conditions:          committedPreemption,
			deleting:            true,
			wantPhase:           string(state.PhaseError),
			wantContainerStatus: "Pending",
			wantExitReason:      string(state.ExitReasonPreempted),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := newPodForDisruptionTest("agent-pending-phase", tc.podPhase)
			pod.Status.Conditions = tc.conditions
			pod.Status.ContainerStatuses = tc.containerStatuses
			pod.Status.InitContainerStatuses = tc.initStatuses
			if tc.deleting {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			}

			info := listSingleAgent(t, pod)
			if info.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", info.Phase, tc.wantPhase)
			}
			if info.ContainerStatus != tc.wantContainerStatus {
				t.Errorf("ContainerStatus = %q, want %q", info.ContainerStatus, tc.wantContainerStatus)
			}
			if info.ExitReason != tc.wantExitReason {
				t.Errorf("ExitReason = %q, want %q", info.ExitReason, tc.wantExitReason)
			}
		})
	}
}
