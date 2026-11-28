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
		{name: "harness_error retained", activity: "harness_error", want: "harness_error"},
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
		{name: "stalled from harness_error restores harness_error", activity: "stalled", stalledFrom: "harness_error", want: "harness_error"},
		{name: "uppercase IDLE normalized to idle", activity: "IDLE", want: "idle"},
		{name: "uppercase HARNESS_ERROR normalized to harness_error", activity: "HARNESS_ERROR", want: "harness_error"},
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

func TestUpdateAgentStatus_SuspendReasonAndRetainedActivityTogether(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	cases := []struct {
		name         string
		activity     string
		stalledFrom  string
		reason       state.SuspendReason
		wantActivity string
	}{
		{
			name:         "idle policy on completed writes reason and retains completed",
			activity:     string(state.ActivityCompleted),
			reason:       state.SuspendReasonIdle,
			wantActivity: string(state.ActivityCompleted),
		},
		{
			name:         "idle policy on idle writes reason and retains idle",
			activity:     string(state.ActivityIdle),
			reason:       state.SuspendReasonIdle,
			wantActivity: string(state.ActivityIdle),
		},
		{
			name:         "stalled policy on stalled-from-completed writes stalled reason and restores completed",
			activity:     string(state.ActivityStalled),
			stalledFrom:  string(state.ActivityCompleted),
			reason:       state.SuspendReasonStalled,
			wantActivity: string(state.ActivityCompleted),
		},
		{
			name:         "stalled policy on stalled-from-idle writes stalled reason and restores idle",
			activity:     string(state.ActivityStalled),
			stalledFrom:  string(state.ActivityIdle),
			reason:       state.SuspendReasonStalled,
			wantActivity: string(state.ActivityIdle),
		},
		{
			name:         "user suspend on harness_error writes user reason and retains harness_error",
			activity:     string(state.ActivityHarnessError),
			reason:       state.SuspendReasonUser,
			wantActivity: string(state.ActivityHarnessError),
		},
		{
			name:         "user suspend on blocked writes user reason and clears activity",
			activity:     string(state.ActivityBlocked),
			reason:       state.SuspendReasonUser,
			wantActivity: "",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := makeAgent(projectID, fmt.Sprintf("susp-reason-act-%d", i))
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

			// Single UpdateAgentStatus write sets suspend_reason and computes retained activity atomically.
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:           string(state.PhaseSuspended),
				ContainerStatus: "stopped",
				SuspendReason:   string(tc.reason),
			}))

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(tc.reason), got.SuspendReason)
			assert.Equal(t, tc.wantActivity, got.Activity)
			assert.Empty(t, got.StalledFromActivity)
			assert.Empty(t, got.ToolName)
			assert.Nil(t, got.ActivitySince)

			// A repeat suspended write with no explicit reason or activity preserves both.
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase: string(state.PhaseSuspended),
			}))
			gotRepeat, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(tc.reason), gotRepeat.SuspendReason)
			assert.Equal(t, tc.wantActivity, gotRepeat.Activity)

			// Starting clears retained activity while ClearSuspendReasonHook keeps suspend_reason;
			// failed-resume rollback restores retained activity and keeps suspend_reason.
			lastActBefore := gotRepeat.LastActivityEvent
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:   string(state.PhaseStarting),
				IfPhase: string(state.PhaseSuspended),
			}))
			gotStarting, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Empty(t, gotStarting.Activity)
			assert.Equal(t, string(tc.reason), gotStarting.SuspendReason)

			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase:    string(state.PhaseSuspended),
				Activity: tc.wantActivity,
				IfPhase:  string(state.PhaseStarting),
				Restore:  true,
			}))
			gotRolledBack, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), gotRolledBack.Phase)
			assert.Equal(t, string(tc.reason), gotRolledBack.SuspendReason)
			assert.Equal(t, tc.wantActivity, gotRolledBack.Activity)
			assert.True(t, gotRolledBack.LastActivityEvent.Equal(lastActBefore))

			// Resuming to running clears both retained activity and suspend_reason.
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
				Phase: string(state.PhaseRunning),
			}))
			gotResumed, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), gotResumed.Phase)
			assert.Empty(t, gotResumed.Activity)
			assert.Empty(t, gotResumed.SuspendReason)
		})
	}
}

func TestUpdateAgentStatus_ResumeToRunningRefreshesLastActivityEvent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	now := time.Now()
	staleAct := now.Add(-6 * time.Hour)
	cutoffs := store.StallCutoffs{
		Activity: now.Add(-5 * time.Minute),
		Thinking: now.Add(-20 * time.Minute),
	}
	heartbeatRecency := now.Add(-2 * time.Minute)

	backdateLastActivity := func(t *testing.T, id string, ts time.Time) {
		t.Helper()
		row, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		row.LastActivityEvent = ts
		require.NoError(t, s.UpdateAgent(ctx, row))
	}

	// 1. Suspended -> starting -> running (sync wake/resume) refreshes
	// LastActivityEvent on entering running, not on starting (preserving
	// failed-start rollback), and prevents an immediate stall false positive.
	aSync := makeAgent(projectID, "resume-sync-anchor")
	aSync.Phase = string(state.PhaseRunning)
	aSync.Activity = string(state.ActivityIdle)
	require.NoError(t, s.CreateAgent(ctx, aSync))
	require.NoError(t, s.UpdateAgentStatus(ctx, aSync.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseSuspended),
	}))
	backdateLastActivity(t, aSync.ID, staleAct)

	// Entering starting must not bump LastActivityEvent.
	require.NoError(t, s.UpdateAgentStatus(ctx, aSync.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	gotStarting, err := s.GetAgent(ctx, aSync.ID)
	require.NoError(t, err)
	assert.True(t, gotStarting.LastActivityEvent.Equal(staleAct), "starting must not bump LastActivityEvent")

	// Failed-start rollback restores suspended with pre-suspension LastActivityEvent.
	require.NoError(t, s.UpdateAgentStatus(ctx, aSync.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseSuspended),
		Activity: string(state.ActivityIdle),
		IfPhase:  string(state.PhaseStarting),
		Restore:  true,
	}))
	gotRolledBack, err := s.GetAgent(ctx, aSync.ID)
	require.NoError(t, err)
	assert.True(t, gotRolledBack.LastActivityEvent.Equal(staleAct), "rollback must preserve pre-suspension LastActivityEvent")

	// Retry: suspended -> starting -> running refreshes LastActivityEvent.
	require.NoError(t, s.UpdateAgentStatus(ctx, aSync.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	beforeRunning := time.Now().Add(-time.Second)
	require.NoError(t, s.UpdateAgentStatus(ctx, aSync.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseRunning),
	}))
	gotSyncRunning, err := s.GetAgent(ctx, aSync.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotSyncRunning.Phase)
	assert.Empty(t, gotSyncRunning.Activity)
	assert.False(t, gotSyncRunning.LastActivityEvent.Before(beforeRunning),
		"entering running via starting must refresh LastActivityEvent (got %v, want >= %v)", gotSyncRunning.LastActivityEvent, beforeRunning)

	// 2. Direct suspended -> running refreshes LastActivityEvent.
	aDirect := makeAgent(projectID, "resume-direct-anchor")
	aDirect.Phase = string(state.PhaseSuspended)
	aDirect.Activity = string(state.ActivityCompleted)
	aDirect.LastActivityEvent = staleAct
	require.NoError(t, s.CreateAgent(ctx, aDirect))

	beforeDirect := time.Now().Add(-time.Second)
	require.NoError(t, s.UpdateAgentStatus(ctx, aDirect.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseRunning),
	}))
	gotDirect, err := s.GetAgent(ctx, aDirect.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotDirect.Phase)
	assert.Empty(t, gotDirect.Activity)
	assert.False(t, gotDirect.LastActivityEvent.Before(beforeDirect),
		"direct suspended -> running must refresh LastActivityEvent (got %v, want >= %v)", gotDirect.LastActivityEvent, beforeDirect)

	// 3. Async launch path: suspended -> starting -> BeginStartLaunch ->
	// ApplyLaunchReport(Succeeded) refreshes LastActivityEvent on entering
	// running, not on BeginStartLaunch.
	aAsync := makeAgent(projectID, "resume-async-anchor")
	aAsync.Phase = string(state.PhaseSuspended)
	aAsync.Activity = string(state.ActivityIdle)
	aAsync.LastActivityEvent = staleAct
	require.NoError(t, s.CreateAgent(ctx, aAsync))

	require.NoError(t, s.UpdateAgentStatus(ctx, aAsync.ID, store.AgentStatusUpdate{
		Phase:   string(state.PhaseStarting),
		IfPhase: string(state.PhaseSuspended),
	}))
	launchID, err := s.BeginStartLaunch(ctx, aAsync.ID, store.LaunchKindStart, time.Minute, store.BeginLaunchOptions{
		PriorPhase: string(state.PhaseSuspended),
	})
	require.NoError(t, err)
	gotPreReport, err := s.GetAgent(ctx, aAsync.ID)
	require.NoError(t, err)
	assert.True(t, gotPreReport.LastActivityEvent.Equal(staleAct), "BeginStartLaunch must not bump LastActivityEvent")

	beforeAsyncRunning := time.Now().Add(-time.Second)
	ans, _, err := s.ApplyLaunchReport(ctx, aAsync.ID, aAsync.RuntimeBrokerID, store.LaunchReport{
		LaunchID:   launchID,
		InstanceID: "inst-async-anchor",
		State:      store.LaunchReportStateSucceeded,
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, ans.Result)
	gotAsyncRunning, err := s.GetAgent(ctx, aAsync.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotAsyncRunning.Phase)
	assert.Empty(t, gotAsyncRunning.Activity)
	assert.False(t, gotAsyncRunning.LastActivityEvent.Before(beforeAsyncRunning),
		"ApplyLaunchReport(Succeeded) entering running must refresh LastActivityEvent (got %v, want >= %v)", gotAsyncRunning.LastActivityEvent, beforeAsyncRunning)

	// Broker heartbeat with empty activity bumps LastSeen; must not trigger stall.
	require.NoError(t, s.UpdateAgentStatus(ctx, aAsync.ID, store.AgentStatusUpdate{
		Phase:        string(state.PhaseRunning),
		ReportSource: store.StatusReportSourceRuntime,
	}))

	// 4. Guard against over-fix: a genuinely stalled running agent receiving a
	// routine running -> running heartbeat without activity keeps its old
	// LastActivityEvent and IS marked stalled by MarkStalledAgents.
	aStalled := makeAgent(projectID, "genuinely-stalled")
	aStalled.Phase = string(state.PhaseRunning)
	aStalled.Activity = string(state.ActivityExecuting)
	aStalled.LastActivityEvent = now.Add(-30 * time.Minute)
	aStalled.LastSeen = now
	require.NoError(t, s.CreateAgent(ctx, aStalled))

	require.NoError(t, s.UpdateAgentStatus(ctx, aStalled.ID, store.AgentStatusUpdate{
		Phase:        string(state.PhaseRunning),
		ReportSource: store.StatusReportSourceRuntime,
	}))
	gotStillStale, err := s.GetAgent(ctx, aStalled.ID)
	require.NoError(t, err)
	assert.True(t, gotStillStale.LastActivityEvent.Equal(aStalled.LastActivityEvent),
		"routine running -> running heartbeat without activity must not refresh LastActivityEvent")

	// 5. Guard against over-fix: a resumed agent that genuinely hangs before
	// posting its first activity (Activity == "") and ages past StalledThreshold
	// IS still caught by MarkStalledAgents.
	aHungStartup := makeAgent(projectID, "hung-after-resume")
	aHungStartup.Phase = string(state.PhaseRunning)
	aHungStartup.Activity = ""
	aHungStartup.LastActivityEvent = now.Add(-10 * time.Minute)
	aHungStartup.LastSeen = now
	require.NoError(t, s.CreateAgent(ctx, aHungStartup))

	// Run stall sweep: none of the freshly resumed agents should be marked
	// stalled, while both genuinely stalled agents must be.
	stalled, err := s.MarkStalledAgents(ctx, cutoffs, heartbeatRecency)
	require.NoError(t, err)
	stalledIDs := make([]string, 0, len(stalled))
	for _, a := range stalled {
		stalledIDs = append(stalledIDs, a.ID)
	}
	assert.ElementsMatch(t, []string{aStalled.ID, aHungStartup.ID}, stalledIDs,
		"immediate stall sweep must skip freshly resumed agents and catch genuinely stalled ones")

	for _, freshID := range []string{aSync.ID, aDirect.ID, aAsync.ID} {
		got, err := s.GetAgent(ctx, freshID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.Empty(t, got.Activity, "freshly resumed agent %s must not be marked stalled", got.Slug)
	}
}
