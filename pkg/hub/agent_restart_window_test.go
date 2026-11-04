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

package hub

// Restart-window tests: the run check across a restart, through the real
// broker heartbeat handler and the agent status endpoint. A restart's start
// leg records the new run with store.SetAgentRunID before it calls the
// broker (HTTPAgentDispatcher.beginRun makes exactly that call), so
// restartMinted models "restart accepted and the new run recorded" with the
// same store call. The fixture (haltFixture) is a running kubernetes agent
// of run "run-1".

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// restartMinted records run "run-2" on the fixture's agent, the write a
// restart's beginRun makes before the broker call.
func (f haltFixture) restartMinted(t *testing.T) {
	t.Helper()
	prior, err := f.s.SetAgentRunID(context.Background(), f.agent.ID, "run-2", nil)
	require.NoError(t, err)
	require.Equal(t, "run-1", prior)
}

// restartClaimed takes a restart's start claim on the fixture's agent, as a
// restart does before its stop leg, and checks that the claim is later than
// the recorded run "run-1" (the restart has not recorded its new run).
func (f haltFixture) restartClaimed(t *testing.T) {
	t.Helper()
	_, err := f.s.ClaimAgentStart(context.Background(), f.agent.ID, "test-owner", store.StartClaimRestart, "", time.Minute)
	require.NoError(t, err)
	a := f.get(t)
	require.NotNil(t, a.StartClaimAt)
	require.NotNil(t, a.RunIDUpdatedAt)
	require.True(t, a.RunIDUpdatedAt.Before(*a.StartClaimAt), "the run was recorded before the restart claim")
}

func TestRestartWindow_HeartbeatOldRunIgnoredAfterRestart(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	before := f.get(t)
	code := 143
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", ExitCode: &code,
		ContainerStatus: "exited (143)", Message: "old run", RunID: "run-1"})
	got := f.get(t)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.ContainerStatus, got.ContainerStatus)
	assert.Equal(t, before.Message, got.Message)
	assert.Empty(t, got.ExitReason)
}

func TestRestartWindow_SelfReportOldRunIgnoredAfterRestart(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	before := f.get(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "stopped", "exitReason": "oom_killed"}))
	got := f.get(t)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Empty(t, got.ExitReason)
}

func TestRestartWindow_HeartbeatNewRunApplied(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "running (new)", Message: "new run", RunID: "run-2"})
	got := f.get(t)
	assert.Equal(t, "running (new)", got.ContainerStatus)
	assert.Equal(t, "new run", got.Message)
}

func TestRestartWindow_SelfReportNewRunApplied(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-2", map[string]any{"phase": "running", "activity": "idle"}))
	assert.Equal(t, "idle", f.get(t).Activity)
}

// TestRestartWindow_CharacterizesRunlessHeartbeatAccepted documents CURRENT
// behaviour; it is not an endorsement. The store's run check skips a report
// that names no run, so after a restart has recorded the new run, a
// heartbeat entry without a run ID (a pod without the scion.run_id label,
// or an older broker) still changes the agent once the restart has recorded
// its new run. Run-less self-reports from an agent token are dropped
// (TestRestartWindow_RunlessSelfReportIgnoredAfterRestart); run-less
// heartbeats after a restart are a deferred follow-up that needs a broker
// capability (ptone/scion#1956 item 5b).
func TestRestartWindow_CharacterizesRunlessHeartbeatAccepted(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "exited (1)", Message: "run-less report"})
	got := f.get(t)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, "exited (1)", got.ContainerStatus, "current behaviour: a run-less heartbeat applies")
	assert.Equal(t, "run-less report", got.Message, "current behaviour: a run-less heartbeat applies")
}

// TestRestartWindow_RunlessSelfReportIgnoredAfterRestart: an agent token
// without a run claim (presentedAgentTokenRunID "") belongs to no run of a
// row whose run was recorded by a dispatch, so after a restart its report
// no longer changes the agent's status fields (gap (1), the self-report
// variant; store.RunlessAgentTokenReport).
func TestRestartWindow_RunlessSelfReportIgnoredAfterRestart(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	before := f.get(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "", map[string]any{"phase": "stopped", "exitReason": "oom_killed", "activity": "idle"}))
	got := f.get(t)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Empty(t, got.ExitReason)
}

// TestRestartWindow_OldRunReportAppliesWithoutRestartClaim: with no
// restart in flight the row's run is the live one, so its reports apply.
// The restart-window rule needs a restart claim
// (TestRestartWindow_OldPodStoppedDuringRestartIgnored).
func TestRestartWindow_OldRunReportAppliesWithoutRestartClaim(t *testing.T) {
	f := newHaltFixture(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "exited (143)", Message: "old pod", RunID: "run-1"})
	got := f.get(t)
	assert.Equal(t, "run-1", got.RunID)
	assert.Equal(t, "exited (143)", got.ContainerStatus, "no restart in flight: the run's report applies")
	assert.Equal(t, "old pod", got.Message, "no restart in flight: the run's report applies")
}

// TestRestartWindow_OldPodStoppedDuringRestartIgnored: while a restart has
// not recorded its new run, the old pod's terminal heartbeat (of the
// current run, or with no run) no longer flickers onto the agent: phase,
// activity, exit and message are kept, the container status still applies.
// Once the new run is recorded, its reports apply although the restart
// claim is still held (gap (2); store.RestartPreMint).
func TestRestartWindow_OldPodStoppedDuringRestartIgnored(t *testing.T) {
	f := newHaltFixture(t)
	f.restartClaimed(t)
	before := f.get(t)
	code := 143
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", ExitCode: &code,
		ContainerStatus: "exited (143)", Message: "old pod stopped", RunID: "run-1"})
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ContainerStatus: "exited (0)", Message: "run-less stop"})
	got := f.get(t)
	assert.Equal(t, before.Phase, got.Phase, "no stopped flicker during the restart")
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.Message, got.Message)
	assert.Empty(t, got.ExitReason)
	assert.Equal(t, "exited (0)", got.ContainerStatus, "the container status still applies")

	f.restartMinted(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "running (new)", Message: "new run", RunID: "run-2"})
	got = f.get(t)
	assert.Equal(t, "new run", got.Message, "the new run's report applies while the claim is held")
	assert.Equal(t, "running (new)", got.ContainerStatus)
}

func TestRestartWindow_OldPodSelfReportDuringRestartIgnored(t *testing.T) {
	f := newHaltFixture(t)
	f.restartClaimed(t)
	before := f.get(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "stopped", "exitReason": "oom_killed"}))
	require.Equal(t, http.StatusOK, f.selfReport(t, "", map[string]any{"phase": "stopped", "activity": "idle"}))
	got := f.get(t)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Empty(t, got.ExitReason)
}

// TestRestartWindow_UserStatusUpdateUnaffected: a user or service with
// update access sends no run either, but it is not an agent token, so the
// run-less rule leaves its update alone on a row with a minted run.
func TestRestartWindow_UserStatusUpdateUnaffected(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", map[string]any{"phase": "running", "activity": "idle"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "idle", f.get(t).Activity)
}

// TestRestartWindow_SelfReportMarksAgentTokenForStore pins the mark the
// store's check reads: updateAgentStatus passes ReportAgentToken=true to the
// store for the agent's own token and false for a user with update access.
// The handler's restart-window check runs first on the same snapshot, so
// the other tests here pass even without this mark; the store's check on
// the locked row (authoritative when the row changed after the handler's
// read) depends on it. The recorder is installed with installStoreFault; it
// ignores the switch, so it records from the start without Arm().
func TestRestartWindow_SelfReportMarksAgentTokenForStore(t *testing.T) {
	f := newHaltFixture(t)
	rec, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *recordingStatusStore {
		return &recordingStatusStore{Store: inner}
	})
	recorded := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.heartbeats)
	}

	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"heartbeat": true}))
	afterAgent := recorded()
	require.GreaterOrEqual(t, afterAgent, 1, "the agent's report reached the store through the recorder")
	assert.True(t, rec.lastHeartbeat(t).ReportAgentToken, "the agent's own token marks the report")

	userRec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", map[string]any{"heartbeat": true})
	require.Equal(t, http.StatusOK, userRec.Code, userRec.Body.String())
	require.Greater(t, recorded(), afterAgent, "the user's update reached the store through the recorder")
	assert.False(t, rec.lastHeartbeat(t).ReportAgentToken, "a user with update access is not an agent token")
}

// TestRestartWindow_OldRunHeartbeatTakesNoSlotDuringRestart: on a row whose
// stored phase is uncounted (stopped) during a restart's stop leg, the
// replaced run's "running" heartbeat does not re-reserve a broker slot, and
// the store keeps the phase stopped. A heartbeat never moves a terminal
// stored phase to a counted one (its running here is not applied), so the
// reported phase never reaches the reservation reconcile; the handler's
// restart-window drop does not decide this case.
func TestRestartWindow_OldRunHeartbeatTakesNoSlotDuringRestart(t *testing.T) {
	f := newHaltFixture(t)
	setBrokerAgentCeiling(t, f.s, 2)
	require.NoError(t, f.s.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{Phase: "stopped", ContainerStatus: "stopped"}))
	f.restartClaimed(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "running", RunID: "run-1"})
	assert.Equal(t, "stopped", f.get(t).Phase, "the store drops the old run's phase")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, f.agent.ID), "the dropped phase takes no broker slot")
	assert.Zero(t, brokerReservationCount(t, f.s, f.broker.ID))
}

// TestRestartWindow_DroppedSelfReportAnsweredNotApplied: a self-report the
// restart window drops entirely is answered applied=false with its reason,
// as Guards 0b, 0c and 0f answer; one that still carries other fields is
// applied in part.
func TestRestartWindow_DroppedSelfReportAnsweredNotApplied(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	post := func(body map[string]any) statusUpdateResult {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", bytes.NewReader(raw))
		req = req.WithContext(agentClaimsCtx(req.Context(), f.agent, "", ScopeAgentStatusUpdate))
		rec := httptest.NewRecorder()
		f.srv.updateAgentStatus(rec, req, f.agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var res statusUpdateResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
		return res
	}
	assert.Equal(t, statusUpdateResult{Applied: false, Reason: statusUpdateReasonRestartWindow},
		post(map[string]any{"phase": "stopped", "activity": "idle"}), "nothing left to persist")
	assert.Equal(t, statusUpdateResult{Applied: true}, post(map[string]any{"phase": "stopped", "heartbeat": true}),
		"the heartbeat still applies")
}

// postSelfReportResult posts body to the fixture agent's status endpoint
// with an agent token for runID and returns the decoded answer.
func (f haltFixture) postSelfReportResult(t *testing.T, runID string, body map[string]any) statusUpdateResult {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/status", bytes.NewReader(raw))
	req = req.WithContext(agentClaimsCtx(req.Context(), f.agent, runID, ScopeAgentStatusUpdate))
	rec := httptest.NewRecorder()
	f.srv.updateAgentStatus(rec, req, f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res statusUpdateResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	return res
}

// TestRestartWindow_NotAppliedAnswerPaths: the restart_window answer also
// comes from the pre-mint rule (a token for the current run while a
// restart has not recorded its new run), and the guards that own the
// agent's status keep their own reasons when they also apply: Guard 0f
// (an active start launch) and Guard 0b (a reincarnation in flight) are
// checked first.
func TestRestartWindow_NotAppliedAnswerPaths(t *testing.T) {
	t.Run("pre-mint", func(t *testing.T) {
		f := newHaltFixture(t)
		f.restartClaimed(t)
		assert.Equal(t, statusUpdateResult{Applied: false, Reason: statusUpdateReasonRestartWindow},
			f.postSelfReportResult(t, "run-1", map[string]any{"phase": "stopped"}))
		assert.Equal(t, "running", f.get(t).Phase)
	})
	t.Run("start launch first", func(t *testing.T) {
		f := newHaltFixture(t)
		beginTestStartLaunch(t, f.s, f.get(t), store.LaunchKindStart, "stopped", false)
		assert.Equal(t, statusUpdateResult{Applied: false, Reason: statusUpdateReasonStartLaunchOtherRun},
			f.postSelfReportResult(t, "", map[string]any{"phase": "stopped"}), "Guard 0f answers before the restart window")
	})
	t.Run("reincarnation first", func(t *testing.T) {
		f := newHaltFixture(t)
		f.restartClaimed(t)
		a := f.get(t)
		a.ReincarnationState = store.ReincarnationStateStopping
		require.NoError(t, f.s.UpdateAgent(context.Background(), a))
		assert.Equal(t, statusUpdateResult{Applied: false, Reason: statusUpdateReasonReincarnationInFlight},
			f.postSelfReportResult(t, "run-1", map[string]any{"phase": "stopped"}), "Guard 0b answers before the restart window")
	})
}

// replacedRunRestartLaunch begins the asynchronous launch of an earlier
// restart, bound to run-1 and taken by no claim, the way the launch whose
// reports a later restart's window sees. The row moves to starting first,
// the phase a launch begins on.
func replacedRunRestartLaunch(t *testing.T, f haltFixture) string {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.s.UpdateAgentStatus(ctx, f.agent.ID, store.AgentStatusUpdate{Phase: "starting"}))
	launchID, err := f.s.BeginStartLaunch(ctx, f.agent.ID, store.LaunchKindRestart, 5*time.Minute,
		store.BeginLaunchOptions{PriorPhase: "running", RunID: "run-1"})
	require.NoError(t, err)
	return launchID
}

// TestRestartWindow_LateAsyncLaunchReportOfReplacedRunMovesNothing: a
// restart has taken its claim but has not recorded its new run, and the row
// still carries the ended launch of the run being replaced. A late launch
// report of that launch keeps the phase, the run and its change time (so
// the window stays open) and the restart claim; a report of an earlier,
// superseded launch is refused; and the replaced run's heartbeat is still
// dropped.
func TestRestartWindow_LateAsyncLaunchReportOfReplacedRunMovesNothing(t *testing.T) {
	ctx := context.Background()
	succeeded := func(launchID string) AgentLaunchReport {
		return AgentLaunchReport{LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
			Agent: &RemoteAgentInfo{RunID: "run-1"}}
	}
	runningObserved := func(t *testing.T, f haltFixture) {
		t.Helper()
		require.NoError(t, f.s.UpdateAgentStatus(ctx, f.agent.ID, store.AgentStatusUpdate{Phase: "running", ContainerStatus: "running",
			ReportSource: store.StatusReportSourceRuntime, ReportRunID: "run-1"}))
		require.Equal(t, store.LaunchEndReasonRunningObserved, f.get(t).LaunchEndReason, "setup: the running report ends the launch")
		// No live activity: the shape in which a failed report for the run
		// could end the launch failed.
		a := f.get(t)
		a.Activity = ""
		require.NoError(t, f.s.UpdateAgent(ctx, a))
		require.Empty(t, f.get(t).Activity)
	}
	assertWindowKept := func(t *testing.T, f haltFixture, before *store.Agent) {
		t.Helper()
		got := f.get(t)
		assert.Equal(t, before.Phase, got.Phase, "the phase is kept")
		assert.Equal(t, "run-1", got.RunID)
		require.NotNil(t, got.RunIDUpdatedAt)
		assert.True(t, got.RunIDUpdatedAt.Equal(*before.RunIDUpdatedAt), "the run change time is kept, so the window stays open")
		require.NotEmpty(t, before.StartClaimID, "the restart claim is still held")
		assert.Equal(t, before.StartClaimID, got.StartClaimID, "the restart claim is still held")
		f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ContainerStatus: "exited (0)", RunID: "run-1"})
		assert.Equal(t, before.Phase, f.get(t).Phase, "the replaced run's heartbeat is still dropped")
	}

	t.Run("launch ended succeeded", func(t *testing.T) {
		f := newHaltFixture(t)
		launchID := replacedRunRestartLaunch(t, f)
		rec := postLaunchReport(t, f.srv, f.broker.ID, f.agent.ID, f.broker.ID, succeeded(launchID))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, store.LaunchEndReasonSucceeded, f.get(t).LaunchEndReason)
		f.restartClaimed(t)
		before := f.get(t)
		require.Equal(t, launchID, before.LaunchID, "the row still carries the replaced run's launch")

		rec = postLaunchReport(t, f.srv, f.broker.ID, f.agent.ID, f.broker.ID, succeeded(launchID))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, store.LaunchReportResultCompleted, launchReportResult(t, rec.Body.Bytes()))
		assertWindowKept(t, f, before)
	})

	t.Run("launch ended running_observed, late succeeded", func(t *testing.T) {
		f := newHaltFixture(t)
		launchID := replacedRunRestartLaunch(t, f)
		runningObserved(t, f)
		f.restartClaimed(t)
		before := f.get(t)
		require.Equal(t, launchID, before.LaunchID, "the row still carries the replaced run's launch")

		rec := postLaunchReport(t, f.srv, f.broker.ID, f.agent.ID, f.broker.ID, succeeded(launchID))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, store.LaunchEndReasonSucceeded, f.get(t).LaunchEndReason, "the late report settles its own launch")
		assertWindowKept(t, f, before)
	})

	t.Run("launch ended running_observed, late failed for the run", func(t *testing.T) {
		f := newHaltFixture(t)
		launchID := replacedRunRestartLaunch(t, f)
		runningObserved(t, f)
		f.restartClaimed(t)
		before := f.get(t)

		rec := postLaunchReport(t, f.srv, f.broker.ID, f.agent.ID, f.broker.ID, AgentLaunchReport{LaunchID: launchID, InstanceID: "i1",
			State: store.LaunchReportStateFailed, RunID: "run-1", ErrorCode: "boom"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, store.LaunchReportResultCompleted, launchReportResult(t, rec.Body.Bytes()))
		assertWindowKept(t, f, before)
	})

	t.Run("superseded launch refused", func(t *testing.T) {
		f := newHaltFixture(t)
		earlier := replacedRunRestartLaunch(t, f)
		rec := postLaunchReport(t, f.srv, f.broker.ID, f.agent.ID, f.broker.ID, succeeded(earlier))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		latest := replacedRunRestartLaunch(t, f)
		require.NotEqual(t, earlier, latest)
		rec = postLaunchReport(t, f.srv, f.broker.ID, f.agent.ID, f.broker.ID, succeeded(latest))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		f.restartClaimed(t)
		before := f.get(t)

		rec = postLaunchReport(t, f.srv, f.broker.ID, f.agent.ID, f.broker.ID, succeeded(earlier))
		assert.Equal(t, http.StatusConflict, rec.Code, "a report of a superseded launch is refused: %s", rec.Body.String())
		assertWindowKept(t, f, before)
	})
}
