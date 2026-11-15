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
		{name: "uppercase IDLE normalized to idle", activity: "IDLE", want: "idle"},
		{name: "uppercase STALLED from COMPLETED restores completed", activity: "STALLED", stalledFrom: "COMPLETED", want: "completed"},
		{name: "stalled from working cleared", activity: "stalled", stalledFrom: "working", want: ""},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := makeAgent(projectID, fmt.Sprintf("susp-case-%d", i))
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

	// 2. Dispatch starting clears retained activity before launch -> rollback
	// restores suspended + retained activity without bumping LastActivityEvent;
	// ClearTerminalRemnants preserves a new activity posted mid-dispatch.
	a2 := makeAgent(projectID, "resume-via-starting")
	a2.Phase = string(state.PhaseRunning)
	a2.Activity = string(state.ActivityIdle)
	require.NoError(t, s.CreateAgent(ctx, a2))
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))
	beforeStart, err := s.GetAgent(ctx, a2.ID)
	require.NoError(t, err)
	lastActBefore := beforeStart.LastActivityEvent

	// beginStartDispatch marks starting with IfPhase=suspended, clearing retained idle before launch.
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	gotMidStart, err := s.GetAgent(ctx, a2.ID)
	require.NoError(t, err)
	assert.Empty(t, gotMidStart.Activity, "starting write from suspended clears retained activity before container launch")

	// Failed dispatch rollback restores suspended with retained idle intact and keeps LastActivityEvent.
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseSuspended),
		Activity: string(state.ActivityIdle),
		IfPhase:  string(state.PhaseStarting),
		Restore:  true,
	}))
	gotRollback, err := s.GetAgent(ctx, a2.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), gotRollback.Phase)
	assert.Equal(t, string(state.ActivityIdle), gotRollback.Activity)
	assert.True(t, gotRollback.LastActivityEvent.Equal(lastActBefore), "rollback restore must not bump LastActivityEvent")

	// Successful start: starting clears idle, new container reports working mid-dispatch,
	// and ClearTerminalRemnants preserves working while clearing remnants.
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Activity: string(state.ActivityWorking),
		Message:  "stale message",
	}))
	require.NoError(t, s.UpdateAgentStatus(ctx, a2.ID, store.AgentStatusUpdate{
		Phase:                 string(state.PhaseStarting),
		ClearTerminalRemnants: true,
		StartWrite:            true,
	}))
	gotWakeStart, err := s.GetAgent(ctx, a2.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.ActivityWorking), gotWakeStart.Activity, "ClearTerminalRemnants must preserve new activity posted mid-dispatch")
	assert.Empty(t, gotWakeStart.Message)

	// 3. Async BeginStartLaunch clears live/non-terminal activity, stall marker,
	// tool name, and activity_since on a row that still carries them, while
	// preserving terminal activities (crashed, limits_exceeded) on a start from
	// stopped so a failed async start keeps how the prior run stopped.
	a3 := makeAgent(projectID, "async-restart-clear")
	a3.Phase = string(state.PhaseRunning)
	a3.Activity = string(state.ActivityCompleted)
	a3.StalledFromActivity = string(state.ActivityWorking)
	a3.ToolName = "bash"
	require.NoError(t, s.CreateAgent(ctx, a3))
	require.NotNil(t, activitySinceOf(t, s, a3.ID))
	_, err = s.BeginStartLaunch(ctx, a3.ID, store.LaunchKindRestart, time.Minute, store.BeginLaunchOptions{
		PriorPhase: string(state.PhaseRunning),
	})
	require.NoError(t, err)
	got3, err := s.GetAgent(ctx, a3.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStarting), got3.Phase)
	assert.Empty(t, got3.Activity, "async restart launch must clear non-terminal prior activity")
	assert.Empty(t, got3.StalledFromActivity)
	assert.Empty(t, got3.ToolName)
	assert.Nil(t, got3.ActivitySince)

	a4a := makeAgent(projectID, "async-start-nonterminal-clear")
	a4a.Phase = string(state.PhaseStarting)
	a4a.Activity = string(state.ActivityIdle)
	a4a.ToolName = "bash"
	require.NoError(t, s.CreateAgent(ctx, a4a))
	_, err = s.BeginStartLaunch(ctx, a4a.ID, store.LaunchKindStart, time.Minute, store.BeginLaunchOptions{
		PriorPhase: string(state.PhaseSuspended),
	})
	require.NoError(t, err)
	got4a, err := s.GetAgent(ctx, a4a.ID)
	require.NoError(t, err)
	assert.Empty(t, got4a.Activity, "async start launch must clear non-terminal activity on starting row")
	assert.Empty(t, got4a.ToolName)

	for _, termAct := range []state.Activity{state.ActivityCrashed, state.ActivityLimitsExceeded} {
		a4b := makeAgent(projectID, "async-start-keep-"+string(termAct))
		a4b.Phase = string(state.PhaseStopped)
		a4b.Activity = string(termAct)
		a4b.ToolName = "bash"
		require.NoError(t, s.CreateAgent(ctx, a4b))
		require.NoError(t, s.UpdateAgentStatus(ctx, a4b.ID, store.AgentStatusUpdate{
			Phase:   string(state.PhaseStarting),
			IfPhase: string(state.PhaseStopped),
		}))
		launchID4b, err := s.BeginStartLaunch(ctx, a4b.ID, store.LaunchKindStart, time.Minute, store.BeginLaunchOptions{
			PriorPhase: string(state.PhaseStopped),
		})
		require.NoError(t, err)
		got4b, err := s.GetAgent(ctx, a4b.ID)
		require.NoError(t, err)
		assert.Equal(t, string(termAct), got4b.Activity, "BeginStartLaunch must preserve terminal activity %s", termAct)
		assert.Empty(t, got4b.ToolName)

		// If the async start fails, startLaunchFailureEnd restores stopped and keeps the terminal activity.
		_, _, err = s.ApplyLaunchReport(ctx, a4b.ID, a4b.RuntimeBrokerID, store.LaunchReport{
			LaunchID:   launchID4b,
			InstanceID: "i1",
			State:      store.LaunchReportStateFailed,
			ErrorCode:  "runtime_error",
		})
		require.NoError(t, err)
		failed4b, err := s.GetAgent(ctx, a4b.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), failed4b.Phase)
		assert.Equal(t, string(termAct), failed4b.Activity, "failed async start from stopped+%s must preserve terminal activity", termAct)
	}

	// 4. Case-variant re-report of the same activity does not move ActivitySince.
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
