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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestUpdateAgentStatus_SuspendRetainsOrClearsActivity(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	cases := []struct {
		name        string
		activity    string
		stalledFrom string
		want        string
	}{
		{name: "completed retained", activity: "completed", want: "completed"},
		{name: "idle retained", activity: "idle", want: "idle"},
		{name: "working cleared", activity: "working", want: ""},
		{name: "thinking cleared", activity: "thinking", want: ""},
		{name: "executing cleared", activity: "executing", want: ""},
		{name: "blocked cleared", activity: "blocked", want: ""},
		{name: "waiting_for_input cleared", activity: "waiting_for_input", want: ""},
		{name: "initializing cleared", activity: "initializing", want: ""},
		{name: "offline cleared", activity: "offline", want: ""},
		{name: "crashed cleared", activity: "crashed", want: ""},
		{name: "stalled from completed restores completed", activity: "stalled", stalledFrom: "completed", want: "completed"},
		{name: "stalled from idle restores idle", activity: "stalled", stalledFrom: "idle", want: "idle"},
		{name: "stalled from working cleared", activity: "stalled", stalledFrom: "working", want: ""},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := makeAgent(projectID, "susp-case-"+string(rune('a'+i)))
			a.Phase = string(state.PhaseRunning)
			a.Activity = tc.activity
			a.ToolName = "bash"
			require.NoError(t, s.CreateAgent(ctx, a))
			if tc.stalledFrom != "" {
				row, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				row.StalledFromActivity = tc.stalledFrom
				require.NoError(t, s.UpdateAgent(ctx, row))
			}

			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:           string(state.PhaseSuspended),
				ContainerStatus: "stopped",
			}))

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, tc.want, got.Activity)
			assert.Empty(t, got.StalledFromActivity)
			assert.Empty(t, got.ToolName)
			assert.Nil(t, got.ActivitySince, "suspended phase must clear activity_since")
			require.NoError(t, (state.AgentState{
				Phase:    state.Phase(got.Phase),
				Activity: state.Activity(got.Activity),
			}).Validate())
		})
	}
}

func TestUpdateAgentStatus_ResumeAndClearActivity(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	// 1. Direct suspended -> running clears retained activity.
	a1 := makeAgent(projectID, "resume-direct")
	a1.Phase = string(state.PhaseRunning)
	a1.Activity = string(state.ActivityCompleted)
	require.NoError(t, s.CreateAgent(ctx, a1))
	require.NoError(t, s.UpdateAgentStatus(ctx, a1.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))

	require.NoError(t, s.UpdateAgentStatus(ctx, a1.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
	got1, err := s.GetAgent(ctx, a1.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got1.Phase)
	assert.Empty(t, got1.Activity)
	assert.Nil(t, got1.ActivitySince)

	// 2. Dispatch starting -> rollback restores suspended + retained activity;
	// ClearTerminalRemnants (start settle / DM wake readiness gate) clears it.
	a2 := makeAgent(projectID, "resume-via-starting")
	a2.Phase = string(state.PhaseRunning)
	a2.Activity = string(state.ActivityIdle)
	require.NoError(t, s.CreateAgent(ctx, a2))
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))

	// beginStartDispatch marks starting with IfPhase=suspended.
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	// Failed dispatch rollback restores suspended with retained idle intact.
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseSuspended),
		IfPhase: string(state.PhaseStarting),
		Restore: true,
	}))
	gotRollback, err := s.GetAgent(ctx, a2.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), gotRollback.Phase)
	assert.Equal(t, string(state.ActivityIdle), gotRollback.Activity)

	// Successful start: starting, then ClearTerminalRemnants clears retained activity.
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:                 string(state.PhaseStarting),
		ClearTerminalRemnants: true,
		StartWrite:            true,
	}))
	gotWakeStart, err := s.GetAgent(ctx, a2.ID)
	require.NoError(t, err)
	assert.Empty(t, gotWakeStart.Activity, "DM wake starting write must clear retained activity before readiness polling")

	// 3. Explicit ClearActivity flag clears activity without touching phase.
	a3 := makeAgent(projectID, "explicit-clear")
	a3.Phase = string(state.PhaseRunning)
	a3.Activity = string(state.ActivityCompleted)
	a3.ToolName = "edit"
	require.NoError(t, s.CreateAgent(ctx, a3))
	require.NoError(t, s.UpdateAgentStatus(ctx, a3.ID, store.AgentStatusUpdate{ClearActivity: true}))
	got3, err := s.GetAgent(ctx, a3.ID)
	require.NoError(t, err)
	assert.Empty(t, got3.Activity)
	assert.Empty(t, got3.ToolName)
	assert.Nil(t, got3.ActivitySince)

	// 4. Async BeginStartLaunch clears prior retained activity on the starting row.
	a4 := makeAgent(projectID, "async-start-clear")
	a4.Phase = string(state.PhaseRunning)
	a4.Activity = string(state.ActivityCompleted)
	require.NoError(t, s.CreateAgent(ctx, a4))
	require.NoError(t, s.UpdateAgentStatus(ctx, a4.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))
	require.NoError(t, s.UpdateAgentStatus(ctx, a4.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	_, err = s.BeginStartLaunch(ctx, a4.ID, store.LaunchKindStart, time.Minute, store.BeginLaunchOptions{
		PriorPhase: string(state.PhaseSuspended),
	})
	require.NoError(t, err)
	got4, err := s.GetAgent(ctx, a4.ID)
	require.NoError(t, err)
	assert.Empty(t, got4.Activity, "async start launch must clear retained activity from suspended prior")

	// 5. Case-variant re-report of the same activity does not move ActivitySince.
	a5 := makeAgent(projectID, "case-stable-since")
	a5.Phase = string(state.PhaseRunning)
	a5.Activity = "completed"
	require.NoError(t, s.CreateAgent(ctx, a5))
	initial5 := backdateActivitySince(t, s, a5.ID)
	require.NoError(t, s.UpdateAgentStatus(ctx, a5.ID, store.AgentStatusUpdate{Activity: "COMPLETED"}))
	after5, err := s.GetAgent(ctx, a5.ID)
	require.NoError(t, err)
	require.NotNil(t, after5.ActivitySince)
	assert.True(t, after5.ActivitySince.Equal(initial5), "case-only re-report must keep ActivitySince stable: %v != %v", *after5.ActivitySince, initial5)
}
