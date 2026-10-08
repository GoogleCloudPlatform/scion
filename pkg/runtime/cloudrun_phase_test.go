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
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cloud Run List Phase (ptone/scion#3738): only an instance being deleted
// gets a phase. No state reads as running: the Instances API does not tell
// a stopped instance from a running one, and the broker heartbeat writes
// any non-empty phase onto the hub's agent.

func TestCloudRunInstancePhase(t *testing.T) {
	cond := func(s runpb.Condition_State) *runpb.Condition { return &runpb.Condition{State: s} }
	deleted := timestamppb.Now()
	cases := []struct {
		name string
		inst *runpb.Instance
		want string
	}{
		{"nil instance", nil, ""},
		{"no terminal condition", &runpb.Instance{}, ""},
		{"unspecified", &runpb.Instance{TerminalCondition: cond(runpb.Condition_STATE_UNSPECIFIED)}, ""},
		{"unknown enum value", &runpb.Instance{TerminalCondition: cond(runpb.Condition_State(99))}, ""},
		{"succeeded", &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED)}, ""},
		{"failed", &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_FAILED)}, ""},
		{"pending", &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_PENDING)}, ""},
		{"reconciling condition", &runpb.Instance{TerminalCondition: cond(runpb.Condition_CONDITION_RECONCILING)}, ""},
		{"reconciling flag", &runpb.Instance{Reconciling: true, TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED)}, ""},
		{"deleting", &runpb.Instance{DeleteTime: deleted}, "stopping"},
		{"deleting after success", &runpb.Instance{DeleteTime: deleted, TerminalCondition: cond(runpb.Condition_CONDITION_SUCCEEDED)}, "stopping"},
		{"deleting while reconciling", &runpb.Instance{DeleteTime: deleted, Reconciling: true}, "stopping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloudRunInstancePhase(tc.inst); got != tc.want {
				t.Errorf("cloudRunInstancePhase = %q, want %q", got, tc.want)
			}
		})
	}
}

// List carries the phase: a deleting instance reads as stopping, and a
// serving (SUCCEEDED) instance does not read as running, so Start's
// pre-clean (pkg/agent/run.go, a.Phase == "running") still deletes and
// recreates it exactly as before.
func TestCloudRunList_SetsPhase(t *testing.T) {
	const base = "projects/test-project/locations/us-central1/instances/"
	fake := &fakeInstancesClient{listResult: []*runpb.Instance{
		{
			Name:              base + "agent-serving-0000000000",
			Labels:            map[string]string{"agent_id": "serving"},
			TerminalCondition: &runpb.Condition{State: runpb.Condition_CONDITION_SUCCEEDED},
		},
		{
			Name:              base + "agent-deleting-1111111111",
			Labels:            map[string]string{"agent_id": "deleting"},
			TerminalCondition: &runpb.Condition{State: runpb.Condition_CONDITION_SUCCEEDED},
			DeleteTime:        timestamppb.Now(),
		},
	}}
	rt := newFakeCloudRunRuntime(t, fake)
	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]string{}
	for _, a := range agents {
		got[a.ID] = a.Phase
	}
	if p := got["serving"]; p != "" {
		t.Errorf("serving instance phase = %q, want \"\" (not running: pre-clean unchanged)", p)
	}
	if p := got["deleting"]; p != "stopping" {
		t.Errorf("deleting instance phase = %q, want stopping", p)
	}
}
