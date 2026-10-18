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

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestCloudRunList_MapsInstanceConditionToPhase: List maps each Instance's
// soft-delete time, reconciling flag and Ready condition state to an agent
// phase (ptone/scion#5270), in that order of precedence, and keeps the raw
// condition state as the container status, with ",reconciling" appended
// while the instance is reconciling. A succeeded Ready condition, which may
// be a serving or a stopped instance, is "provisioning", never "running".
func TestCloudRunList_MapsInstanceConditionToPhase(t *testing.T) {
	cond := func(s runpb.Condition_State) *runpb.Condition {
		return &runpb.Condition{Type: "Ready", State: s}
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
			name:       "succeeded is provisioning, not running",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED)},
			wantPhase:  state.PhaseProvisioning,
			wantStatus: "CONDITION_SUCCEEDED",
		},
		{
			name:       "failed is error",
			inst:       &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_FAILED)},
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
			tc.inst.Name = "projects/test-project/locations/us-central1/instances/agent-a-0000000000"
			tc.inst.Labels = map[string]string{"scion_name": "a"}
			fake := &fakeInstancesClient{listResult: []*runpb.Instance{tc.inst}}
			rt := newFakeCloudRunRuntime(t, fake)

			agents, err := rt.List(context.Background(), nil)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(agents) != 1 {
				t.Fatalf("List returned %d agents, want 1", len(agents))
			}
			if got := agents[0].Phase; got != string(tc.wantPhase) {
				t.Errorf("Phase = %q, want %q", got, tc.wantPhase)
			}
			if got := agents[0].ContainerStatus; got != tc.wantStatus {
				t.Errorf("ContainerStatus = %q, want %q", got, tc.wantStatus)
			}
		})
	}
}
