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

package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// This file covers design §6 test case H-4: the `launch` object contract
// (§3.2).

func TestComputeAgentLaunch_AbsentWhenNoLaunch(t *testing.T) {
	a := &Agent{ID: "a1", Phase: "created"}
	if got := ComputeAgentLaunch(a, time.Now()); got != nil {
		t.Fatalf("expected nil launch when LaunchID is empty, got %+v", got)
	}
}

func TestComputeAgentLaunch_ActiveHasDeadlineAndRemaining(t *testing.T) {
	now := time.Now()
	a := &Agent{
		ID: "a1", Phase: "provisioning",
		LaunchID: "L1", LaunchState: LaunchStateActive, LaunchKind: LaunchKindCreate,
		LaunchStep: "cloning", LaunchDeadline: now.Add(90 * time.Second),
	}
	got := ComputeAgentLaunch(a, now)
	if got == nil {
		t.Fatal("expected a non-nil launch")
	}
	if got.ID != "L1" || got.State != "active" || !got.Active || got.Kind != "create" || got.Step != "cloning" {
		t.Fatalf("unexpected launch fields: %+v", got)
	}
	if got.Deadline == nil || !got.Deadline.Equal(a.LaunchDeadline) {
		t.Fatalf("expected deadline %v, got %v", a.LaunchDeadline, got.Deadline)
	}
	if got.RemainingSeconds == nil || *got.RemainingSeconds != 90 {
		t.Fatalf("expected remainingSeconds 90, got %v", got.RemainingSeconds)
	}
}

// TestComputeAgentLaunch_CeilingNotRounding proves
// design §3.2's max(0, ceil(deadline-now)) formula is not a rounded value. A
// 0.4s remainder must round UP to 1, not down to 0.
func TestComputeAgentLaunch_CeilingNotRounding(t *testing.T) {
	now := time.Now()
	a := &Agent{
		ID: "a1", Phase: "provisioning",
		LaunchID: "L1", LaunchState: LaunchStateActive,
		LaunchDeadline: now.Add(400 * time.Millisecond),
	}
	got := ComputeAgentLaunch(a, now)
	if got.RemainingSeconds == nil || *got.RemainingSeconds != 1 {
		t.Fatalf("expected ceil(0.4s) == 1, got %v", got.RemainingSeconds)
	}
}

func TestComputeAgentLaunch_RemainingSecondsClampedAtZeroPastDeadline(t *testing.T) {
	now := time.Now()
	a := &Agent{
		ID: "a1", Phase: "provisioning",
		LaunchID: "L1", LaunchState: LaunchStateActive, LaunchKind: LaunchKindCreate,
		LaunchDeadline: now.Add(-30 * time.Second), // past D; the reaper hasn't ticked yet
	}
	got := ComputeAgentLaunch(a, now)
	if got == nil {
		t.Fatal("expected a non-nil launch")
	}
	if got.RemainingSeconds == nil || *got.RemainingSeconds != 0 {
		t.Fatalf("expected remainingSeconds clamped to 0, got %v", got.RemainingSeconds)
	}
	if !got.Active {
		t.Fatal("a launch past its deadline but not yet reaped is still active")
	}
}

func TestComputeAgentLaunch_DeadlineAndRemainingAbsentWhenEnded(t *testing.T) {
	now := time.Now()
	a := &Agent{
		ID: "a1", Phase: "running",
		LaunchID: "L1", LaunchState: LaunchStateEnded, LaunchEndReason: LaunchEndReasonSucceeded,
		LaunchKind: LaunchKindCreate, LaunchDeadline: now.Add(time.Hour), // stale, must be ignored once ended
	}
	got := ComputeAgentLaunch(a, now)
	if got == nil {
		t.Fatal("expected a non-nil launch (ended launches are still reported, just not active)")
	}
	if got.Active {
		t.Fatal("an ended launch must not be active")
	}
	if got.Deadline != nil {
		t.Fatalf("deadline must be absent once ended, got %v", got.Deadline)
	}
	if got.RemainingSeconds != nil {
		t.Fatalf("remainingSeconds must be absent once ended, got %v", got.RemainingSeconds)
	}
	if got.EndReason != LaunchEndReasonSucceeded {
		t.Fatalf("expected end reason %q, got %q", LaunchEndReasonSucceeded, got.EndReason)
	}
}

func TestComputeAgentLaunch_NotActiveWhenDeleted(t *testing.T) {
	now := time.Now()
	a := &Agent{
		ID: "a1", Phase: "provisioning",
		LaunchID: "L1", LaunchState: LaunchStateActive, DeletedAt: now,
	}
	got := ComputeAgentLaunch(a, now)
	if got.Active {
		t.Fatal("a deleted agent's launch must not report Active")
	}
}

// Agent is the hub's REST agent shape: provisionedOnly is always sent,
// even when false, so the web's partial seed merge clears a previously
// merged true (ptone/scion#2929).
func TestAgent_ProvisionedOnlyFalseIsExplicit(t *testing.T) {
	data, err := json.Marshal(Agent{ID: "a1", Phase: "running"})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if !strings.Contains(string(data), `"provisionedOnly":false`) {
		t.Errorf("expected explicit provisionedOnly false, got %s", data)
	}
}
