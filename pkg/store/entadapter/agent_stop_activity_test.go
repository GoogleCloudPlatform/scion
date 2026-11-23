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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestUpdateAgentStatus_StopRetainsCompletedOrTerminalActivity(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	cases := []struct {
		name        string
		activity    string
		stalledFrom string
		want        string
	}{
		{name: "completed retained", activity: "completed", want: "completed"},
		{name: "uppercase COMPLETED normalized", activity: "COMPLETED", want: "completed"},
		{name: "crashed retained", activity: "crashed", want: "crashed"},
		{name: "limits_exceeded retained", activity: "limits_exceeded", want: "limits_exceeded"},
		{name: "working cleared", activity: "working", want: ""},
		{name: "idle cleared", activity: "idle", want: ""},
		{name: "blocked cleared", activity: "blocked", want: ""},
		{name: "harness_error cleared", activity: "harness_error", want: ""},
		{name: "stalled from completed restores completed", activity: "stalled", stalledFrom: "completed", want: "completed"},
		{name: "stalled from working cleared", activity: "stalled", stalledFrom: "working", want: ""},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := makeAgent(projectID, fmt.Sprintf("stop-act-%d", i))
			a.Phase = string(state.PhaseRunning)
			a.Activity = tc.activity
			a.StalledFromActivity = tc.stalledFrom
			a.TaskSummary = "finished work"
			ts := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
			a.LastActivityEvent = ts
			require.NoError(t, s.CreateAgent(ctx, a))

			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase: string(state.PhaseStopped),
			}))

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
			assert.Equal(t, tc.want, got.Activity)
			assert.Empty(t, got.StalledFromActivity)
			assert.Equal(t, "finished work", got.TaskSummary)
			assert.Equal(t, ts.Unix(), got.LastActivityEvent.Unix())
		})
	}
}

func TestUpdateAgentStatus_LeavingStoppedCompletedClearsAndRollbackRestores(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	a := makeAgent(projectID, "stop-completed-cycle")
	a.Phase = string(state.PhaseStopped)
	a.Activity = string(state.ActivityCompleted)
	a.TaskSummary = "PR #42 merged"
	a.LastActivityEvent = ts
	require.NoError(t, s.CreateAgent(ctx, a))

	// Transitioning stopped (completed) -> starting clears retained activity
	// while keeping TaskSummary and LastActivityEvent intact.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseStopped),
	}))
	starting, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStarting), starting.Phase)
	assert.Empty(t, starting.Activity)
	assert.Equal(t, "PR #42 merged", starting.TaskSummary)
	assert.Equal(t, ts.Unix(), starting.LastActivityEvent.Unix())

	// Failed-start rollback restores PhaseStopped + ActivityCompleted without
	// bumping LastActivityEvent.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseStopped),
		Activity: string(state.ActivityCompleted),
		IfPhase:  string(state.PhaseStarting),
		Restore:  true,
	}))
	restored, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), restored.Phase)
	assert.Equal(t, string(state.ActivityCompleted), restored.Activity)
	assert.Equal(t, ts.Unix(), restored.LastActivityEvent.Unix())
}

func TestUpdateAgentStatus_HaltRetainsCompletedActivity(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	cases := []struct {
		name        string
		phase       string
		activity    string
		stalledFrom string
		hbPhase     string
		hbExitCode  *int
		want        string
	}{
		{name: "running completed retained on halt", phase: "running", activity: "completed", hbPhase: "stopped", hbExitCode: intp(143), want: "completed"},
		{name: "running uppercase COMPLETED normalized on halt", phase: "running", activity: "COMPLETED", hbPhase: "stopped", hbExitCode: intp(143), want: "completed"},
		{name: "stalled from completed retained on halt", phase: "running", activity: "stalled", stalledFrom: "completed", hbPhase: "stopped", hbExitCode: intp(143), want: "completed"},
		{name: "stopped completed upgraded to halt retains completed", phase: "stopped", activity: "completed", hbPhase: "", hbExitCode: nil, want: "completed"},
		{name: "running working cleared on halt", phase: "running", activity: "working", hbPhase: "stopped", hbExitCode: intp(143), want: ""},
		{name: "running crashed cleared on halt", phase: "running", activity: "crashed", hbPhase: "stopped", hbExitCode: intp(137), want: ""},
		{name: "stopped limits_exceeded cleared on halt upgrade", phase: "stopped", activity: "limits_exceeded", hbPhase: "", hbExitCode: nil, want: ""},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := defaultHaltRow()
			r.phase = tc.phase
			r.activity = tc.activity
			id := newHaltAgent(t, ctx, s, projectID, fmt.Sprintf("halt-act-%d", i), r)
			if tc.stalledFrom != "" {
				uid, err := parseUUID(id)
				require.NoError(t, err)
				_, err = s.client.Agent.UpdateOneID(uid).SetStalledFromActivity(tc.stalledFrom).Save(ctx)
				require.NoError(t, err)
			}

			res := apply(t, ctx, s, id, heartbeat(tc.hbPhase, "preempted", tc.hbExitCode))
			assert.True(t, res.Halted, "expected halt transition")

			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseHalted), got.Phase)
			assert.Equal(t, tc.want, got.Activity)
			assert.Empty(t, got.StalledFromActivity)
			assert.Equal(t, "preempted", got.ExitReason)
		})
	}
}

func TestUpdateAgentStatus_LeavingHaltedCompletedClearsAndRollbackRestores(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	r := defaultHaltRow()
	r.phase = string(state.PhaseHalted)
	r.activity = string(state.ActivityCompleted)
	r.exitReason = "preempted"
	id := newHaltAgent(t, ctx, s, projectID, "halt-completed-cycle", r)
	uid, err := parseUUID(id)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).
		SetTaskSummary("PR #99 merged").
		SetLastActivityEvent(ts).
		Save(ctx)
	require.NoError(t, err)

	// Transitioning halted (completed) -> starting clears retained activity
	// while keeping TaskSummary and LastActivityEvent intact.
	require.NoError(t, s.UpdateAgentStatus(ctx, id, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseHalted),
	}))
	starting, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStarting), starting.Phase)
	assert.Empty(t, starting.Activity)
	assert.Equal(t, "PR #99 merged", starting.TaskSummary)
	assert.Equal(t, ts.Unix(), starting.LastActivityEvent.Unix())

	// Failed-start rollback restores PhaseHalted + ActivityCompleted without
	// bumping LastActivityEvent.
	require.NoError(t, s.UpdateAgentStatus(ctx, id, store.AgentStatusUpdate{
		Phase:    string(state.PhaseHalted),
		Activity: string(state.ActivityCompleted),
		IfPhase:  string(state.PhaseStarting),
		Restore:  true,
	}))
	restored, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseHalted), restored.Phase)
	assert.Equal(t, string(state.ActivityCompleted), restored.Activity)
	assert.Equal(t, ts.Unix(), restored.LastActivityEvent.Unix())
}
