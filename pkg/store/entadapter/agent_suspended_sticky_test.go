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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// seedSuspendedAgent creates a running agent on runID with the given activity
// (and optional stalledFrom), then suspends it with reason as the hub's
// suspend paths do.
func seedSuspendedAgent(
	t *testing.T,
	ctx context.Context,
	s *AgentStore,
	projectID, slug, runID, activity, stalledFrom string,
	reason state.SuspendReason,
) *store.Agent {
	t.Helper()
	a := makeAgent(projectID, slug)
	a.Phase = string(state.PhaseRunning)
	a.Activity = activity
	a.ToolName = "bash"
	a.Runtime = "kubernetes"
	require.NoError(t, s.CreateAgent(ctx, a))

	now := time.Now().UTC()
	intentStopped := string(store.RunIntentStopped)
	upd := s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
		SetRunID(runID).
		SetRunIDUpdatedAt(now).
		SetRunIntent(intentStopped).
		SetRunIntentAt(now)
	if stalledFrom != "" {
		upd.SetStalledFromActivity(stalledFrom)
	}
	require.NoError(t, upd.Exec(ctx))

	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Phase:           string(state.PhaseSuspended),
		ContainerStatus: "stopped",
		SuspendReason:   string(reason),
		IfRunID:         runID,
	}))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	return got
}

// TestUpdateAgentStatus_SuspendedStickyAgainstReports covers ptone/scion#8256:
// once the locked row is suspended, phase changes and incoming activities from
// agent reports (sciontool status/hook POSTs) and runtime reports (broker
// heartbeats) for that run are ignored, so ClearSuspendReasonHook never clears
// suspend_reason and retained suspend activities are preserved.
func TestUpdateAgentStatus_SuspendedStickyAgainstReports(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	reasonCases := []struct {
		name         string
		reason       state.SuspendReason
		activity     string
		stalledFrom  string
		wantActivity string
	}{
		{
			name:         "user with retained activity",
			reason:       state.SuspendReasonUser,
			activity:     string(state.ActivityCompleted),
			wantActivity: string(state.ActivityCompleted),
		},
		{
			name:         "user without retained activity",
			reason:       state.SuspendReasonUser,
			activity:     string(state.ActivityBlocked),
			wantActivity: "",
		},
		{
			name:         "stalled with retained activity",
			reason:       state.SuspendReasonStalled,
			activity:     string(state.ActivityStalled),
			stalledFrom:  string(state.ActivityIdle),
			wantActivity: string(state.ActivityIdle),
		},
		{
			name:         "stalled without retained activity",
			reason:       state.SuspendReasonStalled,
			activity:     string(state.ActivityStalled),
			stalledFrom:  string(state.ActivityWorking),
			wantActivity: "",
		},
		{
			name:         "idle with retained activity",
			reason:       state.SuspendReasonIdle,
			activity:     string(state.ActivityHarnessError),
			wantActivity: string(state.ActivityHarnessError),
		},
		{
			name:         "idle without retained activity",
			reason:       state.SuspendReasonIdle,
			activity:     string(state.ActivityWorking),
			wantActivity: "",
		},
	}

	reportSources := []struct {
		name  string
		build func(runID string) []store.AgentStatusUpdate
	}{
		{
			name: "agent_reports",
			build: func(runID string) []store.AgentStatusUpdate {
				turns := 4
				calls := 9
				return []store.AgentStatusUpdate{
					{
						Phase:            string(state.PhaseStopping),
						Message:          "Agent shutting down",
						ReportSource:     store.StatusReportSourceAgent,
						ReportRunID:      runID,
						ReportAgentToken: true,
					},
					{
						Phase:             string(state.PhaseStopped),
						Message:           "Session ended",
						ExitReason:        string(state.ExitReasonSignaled),
						ExitCode:          intp(0),
						TaskSummary:       "finished slice",
						CurrentTurns:      &turns,
						CurrentModelCalls: &calls,
						ReportSource:      store.StatusReportSourceAgent,
						ReportRunID:       runID,
						ReportAgentToken:  true,
					},
					{
						Phase:            string(state.PhaseError),
						Activity:         string(state.ActivityCrashed),
						ExitCode:         intp(1),
						ExitReason:       "crashed",
						ReportSource:     store.StatusReportSourceAgent,
						ReportRunID:      runID,
						ReportAgentToken: true,
					},
					{
						Phase:            string(state.PhaseStopped),
						Activity:         string(state.ActivityLimitsExceeded),
						ExitCode:         intp(10),
						ExitReason:       "limits_exceeded",
						ReportSource:     store.StatusReportSourceAgent,
						ReportRunID:      runID,
						ReportAgentToken: true,
					},
					{
						Phase:            string(state.PhaseRunning),
						Activity:         string(state.ActivityWorking),
						ToolName:         "bash",
						ReportSource:     store.StatusReportSourceAgent,
						ReportRunID:      runID,
						ReportAgentToken: true,
					},
				}
			},
		},
		{
			name: "runtime_reports",
			build: func(runID string) []store.AgentStatusUpdate {
				return []store.AgentStatusUpdate{
					{
						Phase:           string(state.PhaseStopping),
						ContainerStatus: "stopping",
						Heartbeat:       true,
						ReportSource:    store.StatusReportSourceRuntime,
						ReportRunID:     runID,
					},
					{
						Phase:           string(state.PhaseStopped),
						ContainerStatus: "exited (0)",
						ExitCode:        intp(0),
						Message:         "container stopped",
						Heartbeat:       true,
						ReportSource:    store.StatusReportSourceRuntime,
						ReportRunID:     runID,
					},
					{
						Phase:           string(state.PhaseError),
						Activity:        string(state.ActivityCrashed),
						ContainerStatus: "exited (137)",
						ExitCode:        intp(137),
						ExitReason:      string(state.ExitReasonOOMKilled),
						Heartbeat:       true,
						ReportSource:    store.StatusReportSourceRuntime,
						ReportRunID:     runID,
					},
					{
						Phase:           string(state.PhaseRunning),
						Activity:        string(state.ActivityThinking),
						ContainerStatus: "Up 5 minutes",
						Heartbeat:       true,
						ReportSource:    store.StatusReportSourceRuntime,
						ReportRunID:     runID,
					},
				}
			},
		},
	}

	for i, rc := range reasonCases {
		for _, src := range reportSources {
			t.Run(rc.name+"/"+src.name, func(t *testing.T) {
				const runID = "run-susp-1"
				slug := fmt.Sprintf("susp-sticky-%d-%s", i, src.name)
				before := seedSuspendedAgent(t, ctx, s, projectID, slug, runID, rc.activity, rc.stalledFrom, rc.reason)
				require.Equal(t, string(state.PhaseSuspended), before.Phase)
				require.Equal(t, string(rc.reason), before.SuspendReason)
				require.Equal(t, rc.wantActivity, before.Activity)

				for step, su := range src.build(runID) {
					var res store.AgentStatusResult
					su.Result = &res
					time.Sleep(2 * time.Millisecond)
					require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, su), "step %d", step)
					assert.False(t, res.Halted, "step %d: suspended row must never halt", step)
					assert.False(t, res.Unstalled, "step %d: suspended row must not un-stall", step)

					got, err := s.GetAgent(ctx, before.ID)
					require.NoError(t, err)
					assert.Equal(t, string(state.PhaseSuspended), got.Phase, "step %d", step)
					assert.Equal(t, string(rc.reason), got.SuspendReason, "step %d", step)
					assert.Equal(t, rc.wantActivity, got.Activity, "step %d", step)
					assert.Empty(t, got.StalledFromActivity, "step %d", step)
					assert.Empty(t, got.ToolName, "step %d", step)
					assert.Nil(t, got.ActivitySince, "step %d", step)
					assert.Nil(t, got.ExitCode, "step %d", step)
					assert.Empty(t, got.ExitReason, "step %d", step)
					assert.True(t, got.LastActivityEvent.Equal(before.LastActivityEvent), "step %d", step)
					assert.True(t, got.LastSeen.After(before.LastSeen) || before.LastSeen.IsZero(), "step %d", step)
					require.NoError(t, (state.AgentState{
						Phase:    state.Phase(got.Phase),
						Activity: state.Activity(got.Activity),
					}).Validate(), "step %d", step)
				}

				final, err := s.GetAgent(ctx, before.ID)
				require.NoError(t, err)
				if src.name == "agent_reports" {
					assert.Equal(t, "finished slice", final.TaskSummary, "harmless telemetry still applies")
					assert.Equal(t, 4, final.CurrentTurns)
					assert.Equal(t, 9, final.CurrentModelCalls)
					assert.Equal(t, "Session ended", final.Message)
				} else {
					assert.Equal(t, "exited (137)", final.ContainerStatus,
						"exited container status applies while trailing Up status was held")
					assert.Equal(t, "container stopped", final.Message)
				}
			})
		}
	}

	// Even if a superseded start claim is still recorded on the row when the
	// suspension status commits (released in defer after recordStopStatus), a
	// running/starting agent report must not move the row out of suspended.
	t.Run("superseded start claim held does not un-suspend on agent report", func(t *testing.T) {
		const runID = "run-claim-1"
		before := seedSuspendedAgent(t, ctx, s, projectID, "susp-claim-held", runID,
			string(state.ActivityCompleted), "", state.SuspendReasonIdle)
		require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(before.ID)).
			SetStartClaimID("superseded-claim").Exec(ctx))

		for _, p := range []state.Phase{state.PhaseStarting, state.PhaseRunning} {
			require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
				Phase:            string(p),
				Activity:         string(state.ActivityWorking),
				ReportSource:     store.StatusReportSourceAgent,
				ReportRunID:      runID,
				ReportAgentToken: true,
			}))
			got, err := s.GetAgent(ctx, before.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
			assert.Equal(t, string(state.ActivityCompleted), got.Activity)
		}
	})
}

func TestUpdateAgentStatus_SuspendedHubTransitionsStillWork(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	t.Run("resume via starting, failed-start restore, then running", func(t *testing.T) {
		const runID = "run-resume-1"
		before := seedSuspendedAgent(t, ctx, s, projectID, "susp-hub-resume", runID,
			string(state.ActivityCompleted), "", state.SuspendReasonStalled)

		// Late shutdown report arrives first: stays suspended.
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
			Phase:        string(state.PhaseStopped),
			ReportSource: store.StatusReportSourceAgent,
			ReportRunID:  runID,
		}))

		// Hub beginStartDispatch: suspended -> starting keeps suspend_reason and clears retained activity.
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
			Phase:   string(state.PhaseStarting),
			IfPhase: string(state.PhaseSuspended),
		}))
		mid, err := s.GetAgent(ctx, before.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStarting), mid.Phase)
		assert.Equal(t, string(state.SuspendReasonStalled), mid.SuspendReason)
		assert.Empty(t, mid.Activity)

		// Hub failed-start rollback: starting -> suspended restores retained activity and keeps reason.
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseSuspended),
			Activity: string(state.ActivityCompleted),
			IfPhase:  string(state.PhaseStarting),
			Restore:  true,
		}))
		restored, err := s.GetAgent(ctx, before.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), restored.Phase)
		assert.Equal(t, string(state.SuspendReasonStalled), restored.SuspendReason)
		assert.Equal(t, string(state.ActivityCompleted), restored.Activity)

		// Successful resume: suspended -> starting -> running clears suspend_reason and activity.
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
			Phase:   string(state.PhaseStarting),
			IfPhase: string(state.PhaseSuspended),
		}))
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
			Phase:                 string(state.PhaseRunning),
			ClearExit:             true,
			ClearTerminalRemnants: true,
			StartWrite:            true,
		}))
		running, err := s.GetAgent(ctx, before.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), running.Phase)
		assert.Empty(t, running.SuspendReason)
		assert.Empty(t, running.Activity)
	})

	t.Run("user conversion of suspend reason preserves suspended and retained activity", func(t *testing.T) {
		before := seedSuspendedAgent(t, ctx, s, projectID, "susp-hub-convert", "run-conv-1",
			string(state.ActivityIdle), "", state.SuspendReasonIdle)

		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
			Phase:         string(state.PhaseSuspended),
			SuspendReason: string(state.SuspendReasonUser),
			IfPhase:       string(state.PhaseSuspended),
		}))
		got, err := s.GetAgent(ctx, before.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)
		assert.Equal(t, string(state.ActivityIdle), got.Activity)
	})

	t.Run("explicit hub stop of a suspended agent moves to stopped and clears reason", func(t *testing.T) {
		const runID = "run-stop-1"
		before := seedSuspendedAgent(t, ctx, s, projectID, "susp-hub-stop", runID,
			string(state.ActivityCompleted), "", state.SuspendReasonUser)

		zero := 0
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
			Phase:           string(state.PhaseStopped),
			ContainerStatus: "stopped",
			ExitCode:        &zero,
			IfRunID:         runID,
		}))
		got, err := s.GetAgent(ctx, before.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase)
		assert.Empty(t, got.SuspendReason)
		assert.Empty(t, got.Activity)
	})

	t.Run("delete of a suspended agent succeeds", func(t *testing.T) {
		before := seedSuspendedAgent(t, ctx, s, projectID, "susp-hub-del", "run-del-1",
			string(state.ActivityIdle), "", state.SuspendReasonIdle)
		require.NoError(t, s.DeleteAgent(ctx, before.ID))
		_, err := s.GetAgent(ctx, before.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestUpdateAgentStatus_SuspendedOtherRunReportUnchanged(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	before := seedSuspendedAgent(t, ctx, s, projectID, "susp-other-run", "run-current",
		string(state.ActivityCompleted), "", state.SuspendReasonIdle)
	require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, store.AgentStatusUpdate{
		Message:     "kept message",
		TaskSummary: "kept summary",
	}))
	before, err := s.GetAgent(ctx, before.ID)
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)
	for _, su := range []store.AgentStatusUpdate{
		{
			Phase:            string(state.PhaseStopped),
			Activity:         string(state.ActivityCrashed),
			Message:          "other run agent report",
			TaskSummary:      "other summary",
			ReportSource:     store.StatusReportSourceAgent,
			ReportRunID:      "run-older",
			ReportAgentToken: true,
		},
		{
			Phase:           string(state.PhaseStopped),
			ContainerStatus: "exited (1)",
			Message:         "other run heartbeat",
			Heartbeat:       true,
			ReportSource:    store.StatusReportSourceRuntime,
			ReportRunID:     "run-older",
		},
	} {
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, su))
	}

	got, err := s.GetAgent(ctx, before.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
	assert.Equal(t, string(state.ActivityCompleted), got.Activity)
	assert.Equal(t, "stopped", got.ContainerStatus, "other-run report must not update ContainerStatus")
	assert.Equal(t, "kept message", got.Message, "other-run report must not update Message")
	assert.Equal(t, "kept summary", got.TaskSummary, "other-run report must not update TaskSummary")
	assert.True(t, got.LastSeen.After(before.LastSeen) || before.LastSeen.IsZero(), "LastSeen still advances")
}
