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
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestCloudRunList_MapsInstanceConditionToPhase: List maps each Instance's
// soft-delete time, reconciling flag and terminal condition state to an
// agent phase (ptone/scion#5270, ptone/scion#5295), in that order of
// precedence, and keeps the raw condition state as the container status,
// with ",reconciling" appended while the instance is reconciling. A
// succeeded terminal condition is "running"; a failed one is "stopped" when
// it carries no reason and there is no Retry condition, else "error".
func TestCloudRunList_MapsInstanceConditionToPhase(t *testing.T) {
	cond := func(s runpb.Condition_State) *runpb.Condition {
		return &runpb.Condition{Type: "Running", State: s}
	}
	deleted := timestamppb.Now()

	tests := []struct {
		name       string
		inst       *runpb.Instance
		wantPhase  state.Phase
		wantStatus string
	}{
		{
			name:       "no terminal condition",
			inst:       &runpb.Instance{},
			wantPhase:  state.PhaseProvisioning,
			wantStatus: "unknown",
		},
		{
			name:       "unspecified",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_STATE_UNSPECIFIED)},
			wantPhase:  state.PhaseProvisioning,
			wantStatus: "STATE_UNSPECIFIED",
		},
		{
			name:       "pending",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_PENDING)},
			wantPhase:  state.PhaseProvisioning,
			wantStatus: "CONDITION_PENDING",
		},
		{
			name:       "reconciling state",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_RECONCILING)},
			wantPhase:  state.PhaseProvisioning,
			wantStatus: "CONDITION_RECONCILING",
		},
		{
			name:       "succeeded is running",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED)},
			wantPhase:  state.PhaseRunning,
			wantStatus: "CONDITION_SUCCEEDED",
		},
		{
			name:       "failed without reason or Retry is stopped",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_FAILED)},
			wantPhase:  state.PhaseStopped,
			wantStatus: "CONDITION_FAILED",
		},
		{
			name: "failed with a Retry condition is error",
			inst: &runpb.Instance{
				TerminalCondition: cond(runpb.Condition_CONDITION_FAILED),
				Conditions:        []*runpb.Condition{{Type: "Retry", State: runpb.Condition_CONDITION_SUCCEEDED}},
			},
			wantPhase:  state.PhaseError,
			wantStatus: "CONDITION_FAILED",
		},
		{
			name:       "succeeded while reconciling",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED), Reconciling: true},
			wantPhase:  state.PhaseProvisioning,
			wantStatus: "CONDITION_SUCCEEDED,reconciling",
		},
		{
			name:       "failed while reconciling",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_FAILED), Reconciling: true},
			wantPhase:  state.PhaseProvisioning,
			wantStatus: "CONDITION_FAILED,reconciling",
		},
		{
			name:       "soft-deleted succeeded instance is stopped",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED), DeleteTime: deleted},
			wantPhase:  state.PhaseStopped,
			wantStatus: "CONDITION_SUCCEEDED",
		},
		{
			name:       "soft-deleted reconciling instance is stopped",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED), Reconciling: true, DeleteTime: deleted},
			wantPhase:  state.PhaseStopped,
			wantStatus: "CONDITION_SUCCEEDED,reconciling",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertCloudRunListPhase(t, tc.inst, tc.wantPhase, tc.wantStatus)
		})
	}
}

// assertCloudRunListPhase lists inst through a fake client and checks the
// phase and container status List reports for it.
func assertCloudRunListPhase(t *testing.T, inst *runpb.Instance, wantPhase state.Phase, wantStatus string) {
	t.Helper()
	inst.Name = "projects/test-project/locations/us-central1/instances/agent-a-0000000000"
	inst.Labels = map[string]string{"scion_name": "a"}
	fake := &fakeInstancesClient{listResult: []*runpb.Instance{inst}}
	rt := newFakeCloudRunRuntime(t, fake)

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("List returned %d agents, want 1", len(agents))
	}
	if got := agents[0].Phase; got != string(wantPhase) {
		t.Errorf("Phase = %q, want %q", got, wantPhase)
	}
	if got := agents[0].ContainerStatus; got != wantStatus {
		t.Errorf("ContainerStatus = %q, want %q", got, wantStatus)
	}
}

// Recorded Instance shapes (ptone/scion#5295). Each builder returns a fresh
// Instance with the fields a live Cloud Run Admin v2 GetInstance returned
// in that state: generation, reconciling, delete_time, the terminal
// condition (type, state, message; every reason field was unset) and the
// conditions with their states and reasons. Fields List does not read
// (times, etag, uid, container statuses) are left out.

func recordedCondition(typ string, s runpb.Condition_State, msg string) *runpb.Condition {
	return &runpb.Condition{Type: typ, State: s, Message: msg}
}

func recordedRetry(reason runpb.Condition_CommonReason, msg string) *runpb.Condition {
	return &runpb.Condition{
		Type:     "Retry",
		State:    runpb.Condition_CONDITION_SUCCEEDED,
		Reasons:  &runpb.Condition_Reason{Reason: reason},
		Message:  msg,
		Severity: runpb.Condition_INFO,
	}
}

// recordedReadyConditions: the ContainerReady and ResourcesAvailable
// conditions, in state s with the recorded messages.
func recordedReadyConditions(s runpb.Condition_State, containerMsg, resourcesMsg string) []*runpb.Condition {
	return []*runpb.Condition{
		recordedCondition("ContainerReady", s, containerMsg),
		recordedCondition("ResourcesAvailable", s, resourcesMsg),
	}
}

// recordedCreating: just after CreateInstance.
func recordedCreating() *runpb.Instance {
	return &runpb.Instance{
		Generation:        1,
		Reconciling:       true,
		TerminalCondition: &runpb.Condition{Type: "Running", State: runpb.Condition_CONDITION_PENDING},
	}
}

// recordedRunning: started and serving.
func recordedRunning() *runpb.Instance {
	return &runpb.Instance{
		Generation:         1,
		ObservedGeneration: 1,
		TerminalCondition:  recordedCondition("Running", runpb.Condition_CONDITION_SUCCEEDED, "Started instance in 2.54s."),
		Conditions: append([]*runpb.Condition{
			recordedRetry(runpb.Condition_WAITING_FOR_OPERATION, "System will retry after 01:00 from lastTransitionTime for polling interval"),
		}, recordedReadyConditions(runpb.Condition_CONDITION_SUCCEEDED, "Imported container image in 2.66s.", "Provisioned imported containers.")...),
	}
}

// recordedStopping: just after StopInstance.
func recordedStopping() *runpb.Instance {
	return &runpb.Instance{
		Generation:         2,
		ObservedGeneration: 2,
		Reconciling:        true,
		TerminalCondition:  recordedCondition("Running", runpb.Condition_CONDITION_RECONCILING, "Waiting for instance to be stopped."),
		Conditions: append([]*runpb.Condition{
			recordedRetry(runpb.Condition_WAITING_FOR_OPERATION, "System will retry after 00:05 from lastTransitionTime for polling interval"),
		}, recordedReadyConditions(runpb.Condition_CONDITION_SUCCEEDED, "Imported container image in 2.66s.", "Provisioned imported containers.")...),
	}
}

// recordedStopped: after StopInstance completed. The Retry condition is gone.
func recordedStopped() *runpb.Instance {
	return &runpb.Instance{
		Generation:         2,
		ObservedGeneration: 2,
		TerminalCondition:  recordedCondition("Running", runpb.Condition_CONDITION_FAILED, "Instance stopped."),
		Conditions:         recordedReadyConditions(runpb.Condition_CONDITION_SUCCEEDED, "Imported container image in 2.66s.", "Provisioned imported containers."),
	}
}

// recordedRestarting: just after StartInstance on a stopped instance.
func recordedRestarting() *runpb.Instance {
	return &runpb.Instance{
		Generation:         3,
		ObservedGeneration: 3,
		Reconciling:        true,
		TerminalCondition:  recordedCondition("Running", runpb.Condition_CONDITION_RECONCILING, "Restarting stopped instance."),
		Conditions: append(recordedReadyConditions(runpb.Condition_CONDITION_RECONCILING, "Restarting stopped instance.", "Restarting stopped instance."),
			recordedRetry(runpb.Condition_IMMEDIATE_RETRY, "System will retry after 00:00 from lastTransitionTime for attempt 0.")),
	}
}

// recordedRestarted: started again after StartInstance.
func recordedRestarted() *runpb.Instance {
	return &runpb.Instance{
		Generation:         3,
		ObservedGeneration: 3,
		TerminalCondition:  recordedCondition("Running", runpb.Condition_CONDITION_SUCCEEDED, "Started instance in 3.47s."),
		Conditions: append(recordedReadyConditions(runpb.Condition_CONDITION_SUCCEEDED, "Imported container image in 1.39s.", "Provisioned imported containers."),
			recordedRetry(runpb.Condition_WAITING_FOR_OPERATION, "System will retry after 00:05 from lastTransitionTime for polling interval")),
	}
}

// recordedExitedZero: the container exited 0 on its own. Apart from the
// message and generation, the same shape as recordedStopped.
func recordedExitedZero() *runpb.Instance {
	return &runpb.Instance{
		Generation:         1,
		ObservedGeneration: 1,
		TerminalCondition:  recordedCondition("Running", runpb.Condition_CONDITION_FAILED, "Instance completed successfully in 50.25s."),
		Conditions:         recordedReadyConditions(runpb.Condition_CONDITION_SUCCEEDED, "Imported container image in 1.02s.", "Provisioned imported containers."),
	}
}

// recordedDeleting: just after DeleteInstance on a running instance.
func recordedDeleting() *runpb.Instance {
	return &runpb.Instance{
		Generation:         4,
		ObservedGeneration: 4,
		Reconciling:        true,
		DeleteTime:         timestamppb.New(time.Date(2026, 10, 22, 4, 46, 26, 926436000, time.UTC)),
		ExpireTime:         timestamppb.New(time.Date(2026, 11, 21, 4, 46, 26, 926436000, time.UTC)),
		TerminalCondition:  recordedCondition("Running", runpb.Condition_CONDITION_RECONCILING, "Waiting for instance to be stopped."),
		Conditions: append(recordedReadyConditions(runpb.Condition_CONDITION_SUCCEEDED, "Imported container image in 1.39s.", "Provisioned imported containers."),
			recordedRetry(runpb.Condition_WAITING_FOR_OPERATION, "System will retry after 00:05 from lastTransitionTime for polling interval")),
	}
}

// TestCloudRunList_MapsRecordedInstanceShapesToPhase: List maps each shape
// recorded from a live instance to its phase (ptone/scion#5295). A running
// instance is "running", not "provisioning". A stopped instance and one
// whose container exited 0 cannot be told apart, so both are "stopped".
func TestCloudRunList_MapsRecordedInstanceShapesToPhase(t *testing.T) {
	tests := []struct {
		name       string
		inst       func() *runpb.Instance
		wantPhase  state.Phase
		wantStatus string
	}{
		{"creating", recordedCreating, state.PhaseProvisioning, "CONDITION_PENDING,reconciling"},
		{"running", recordedRunning, state.PhaseRunning, "CONDITION_SUCCEEDED"},
		{"stopping", recordedStopping, state.PhaseProvisioning, "CONDITION_RECONCILING,reconciling"},
		{"stopped", recordedStopped, state.PhaseStopped, "CONDITION_FAILED"},
		{"restarting", recordedRestarting, state.PhaseProvisioning, "CONDITION_RECONCILING,reconciling"},
		{"restarted", recordedRestarted, state.PhaseRunning, "CONDITION_SUCCEEDED"},
		{"exited zero", recordedExitedZero, state.PhaseStopped, "CONDITION_FAILED"},
		{"deleting", recordedDeleting, state.PhaseStopped, "CONDITION_RECONCILING,reconciling"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertCloudRunListPhase(t, tc.inst(), tc.wantPhase, tc.wantStatus)
		})
	}
}

// TestCloudRunList_FailedInstanceWithFailureSignalIsError: a failed
// terminal condition is "stopped" only in the recorded clean-end shape. A
// genuine failure was not recorded, so these cases change one field of the
// recorded stopped shape at a time: a Retry condition or any kind of
// terminal reason keeps the instance in "error". A different message alone
// does not, since the message is free text and not read.
func TestCloudRunList_FailedInstanceWithFailureSignalIsError(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*runpb.Instance)
		wantPhase state.Phase
	}{
		{
			name: "Retry condition present",
			mutate: func(inst *runpb.Instance) {
				inst.Conditions = append(inst.Conditions, recordedRetry(runpb.Condition_IMMEDIATE_RETRY, "System will retry after 00:10 from lastTransitionTime for attempt 1."))
			},
			wantPhase: state.PhaseError,
		},
		{
			name: "terminal common reason",
			mutate: func(inst *runpb.Instance) {
				inst.TerminalCondition.Reasons = &runpb.Condition_Reason{Reason: runpb.Condition_CONTAINER_MISSING}
			},
			wantPhase: state.PhaseError,
		},
		{
			name: "terminal execution reason",
			mutate: func(inst *runpb.Instance) {
				inst.TerminalCondition.Reasons = &runpb.Condition_ExecutionReason_{ExecutionReason: runpb.Condition_NON_ZERO_EXIT_CODE}
			},
			wantPhase: state.PhaseError,
		},
		{
			name: "terminal revision reason",
			mutate: func(inst *runpb.Instance) {
				inst.TerminalCondition.Reasons = &runpb.Condition_RevisionReason_{RevisionReason: runpb.Condition_HEALTH_CHECK_CONTAINER_ERROR}
			},
			wantPhase: state.PhaseError,
		},
		{
			name: "other message only",
			mutate: func(inst *runpb.Instance) {
				inst.TerminalCondition.Message = "Container exited with an unexpected status."
			},
			wantPhase: state.PhaseStopped,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := recordedStopped()
			tc.mutate(inst)
			assertCloudRunListPhase(t, inst, tc.wantPhase, "CONDITION_FAILED")
		})
	}
}
