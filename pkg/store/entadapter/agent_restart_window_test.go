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

// Restart-window store tests: the run check (UpdateAgentStatus, the
// ReportRunID filter) across a restart. A restart's start leg records the
// new run with SetAgentRunID before it calls the broker (the hub's
// HTTPAgentDispatcher.beginRun makes exactly that store call), so these
// tests model "restart accepted and minted" with the same call.

package entadapter

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restartWindowAgent is a running kubernetes agent of run "run-1" whose
// restart has recorded the new run "run-2".
func restartWindowAgent(t *testing.T, ctx context.Context, slug string) (*AgentStore, string) {
	t.Helper()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, slug, defaultHaltRow())
	prior, err := s.SetAgentRunID(ctx, id, "run-2", nil)
	require.NoError(t, err)
	require.Equal(t, "run-1", prior)
	return s, id
}

// restartWindowHeartbeat is a broker heartbeat of run runID that would move
// the row's container status and message if it applied.
func restartWindowHeartbeat(runID, containerStatus, message string) store.AgentStatusUpdate {
	return store.AgentStatusUpdate{Phase: "running", ContainerStatus: containerStatus, Message: message, Heartbeat: true,
		ReportSource: store.StatusReportSourceRuntime, ReportRunID: runID}
}

func TestAgentRestartWindow_OldRunDroppedAfterSetRunID(t *testing.T) {
	ctx := context.Background()
	s, id := restartWindowAgent(t, ctx, "rw-old")
	before, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	// newHaltAgent writes last-seen an hour in the past, so the advance
	// below is checked against the write time, not at clock resolution.
	require.True(t, before.LastSeen.Before(time.Now().Add(-30*time.Minute)), "fixture last-seen is in the past")

	apply(t, ctx, s, id, restartWindowHeartbeat("run-1", "exited (143)", "old run"))
	oldSelf := selfReport("error", "")
	oldSelf.ReportRunID = "run-1"
	oldSelf.ExitReason = "oom_killed"
	apply(t, ctx, s, id, oldSelf)

	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.ContainerStatus, got.ContainerStatus)
	assert.Equal(t, before.Message, got.Message)
	assert.Equal(t, before.ExitReason, got.ExitReason)
	assert.WithinDuration(t, time.Now(), got.LastSeen, time.Minute, "last-seen still advances to the report time")
}

func TestAgentRestartWindow_NewRunApplied(t *testing.T) {
	ctx := context.Background()
	s, id := restartWindowAgent(t, ctx, "rw-new")

	apply(t, ctx, s, id, restartWindowHeartbeat("run-2", "running (new)", "new run"))
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "running (new)", got.ContainerStatus)
	assert.Equal(t, "new run", got.Message)

	newSelf := selfReport("running", "idle")
	newSelf.ReportRunID = "run-2"
	apply(t, ctx, s, id, newSelf)
	got, err = s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity)
}

// TestAgentRestartWindow_CharacterizesRunlessReportAccepted documents
// CURRENT behaviour; it is not an endorsement. The run check skips a
// report that names no run (ReportRunID ""), so after a restart has
// recorded the new run, a run-less report (a pod without the run label,
// or an older broker) still changes the row. Run-less self-reports from an
// agent token are dropped (TestAgentRestartWindow_RunlessAgentTokenDropped
// WhenMinted); run-less heartbeats after a restart are a deferred follow-up
// (ptone/scion#1956 item 5b).
func TestAgentRestartWindow_CharacterizesRunlessReportAccepted(t *testing.T) {
	ctx := context.Background()
	s, id := restartWindowAgent(t, ctx, "rw-runless")

	apply(t, ctx, s, id, restartWindowHeartbeat("", "exited (1)", "run-less report"))
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, "exited (1)", got.ContainerStatus, "current behaviour: a run-less report applies")
	assert.Equal(t, "run-less report", got.Message, "current behaviour: a run-less report applies")
}

// setStartClaim writes a held start claim of kind onto the row directly,
// taken at at.
func setStartClaim(t *testing.T, ctx context.Context, s *AgentStore, id string, kind store.StartClaimKind, at time.Time) {
	t.Helper()
	uid, err := parseUUID(id)
	require.NoError(t, err)
	require.NoError(t, s.client.Agent.UpdateOneID(uid).
		SetStartClaimID("claim-"+string(kind)).SetStartClaimKind(string(kind)).SetStartClaimAt(at).Exec(ctx))
}

// preMintAgent is a running kubernetes agent of run "run-1" (recorded an
// hour ago) whose restart claim was taken now: a restart that has not yet
// recorded its new run.
func preMintAgent(t *testing.T, ctx context.Context, slug string) (*AgentStore, string) {
	t.Helper()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, slug, defaultHaltRow())
	setStartClaim(t, ctx, s, id, store.StartClaimRestart, time.Now())
	return s, id
}

// stoppedFlicker is the old pod's terminal report during a restart's stop
// leg: phase stopped, a preemption exit, a message and a container status.
func stoppedFlicker(runID string) store.AgentStatusUpdate {
	return store.AgentStatusUpdate{Phase: "stopped", ExitReason: "preempted", ExitCode: intp(143),
		Message: "old pod stopped", ContainerStatus: "exited (143)", Heartbeat: true,
		ReportSource: store.StatusReportSourceRuntime, ReportRunID: runID}
}

// assertStatusKept checks that got still carries before's status fields,
// with the container status of the dropped report applied.
func assertStatusKept(t *testing.T, before, got *store.Agent, containerStatus string) {
	t.Helper()
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.ExitReason, got.ExitReason)
	assert.Equal(t, before.Message, got.Message)
	assert.Equal(t, containerStatus, got.ContainerStatus, "the rest of the report still applies")
	assert.True(t, got.LastSeen.After(before.LastSeen), "last-seen still advances")
}

func TestAgentRestartWindow_RestartClaimPreMintDropsCurrentRunStatus(t *testing.T) {
	ctx := context.Background()
	s, id := preMintAgent(t, ctx, "rw-flicker")
	before, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	res := apply(t, ctx, s, id, stoppedFlicker("run-1"))
	assert.False(t, res.Halted, "a dropped report never halts")
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assertStatusKept(t, before, got, "exited (143)")
}

func TestAgentRestartWindow_RestartClaimPreMintDropsRunlessReports(t *testing.T) {
	ctx := context.Background()
	s, id := preMintAgent(t, ctx, "rw-flicker-runless")
	before, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	// A run-less heartbeat cannot be the new run before it is recorded.
	apply(t, ctx, s, id, stoppedFlicker(""))
	self := selfReport("stopped", "")
	self.ReportRunID = ""
	self.ReportAgentToken = true
	apply(t, ctx, s, id, self)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assertStatusKept(t, before, got, "exited (143)")
}

func TestAgentRestartWindow_RestartClaimAfterMintNewRunApplies(t *testing.T) {
	ctx := context.Background()
	s, id := preMintAgent(t, ctx, "rw-aftermint")
	// The restart's start leg records the new run after the claim was taken.
	_, err := s.SetAgentRunID(ctx, id, "run-2", nil)
	require.NoError(t, err)
	newRun := restartWindowHeartbeat("run-2", "running (new)", "new run")
	newRun.Activity = "idle"
	apply(t, ctx, s, id, newRun)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity, "the new run's report applies while the claim is still held")
	assert.Equal(t, "new run", got.Message)
	assert.Equal(t, "running (new)", got.ContainerStatus)
}

func TestAgentRestartWindow_NonRestartClaimDoesNotDrop(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []store.StartClaimKind{store.StartClaimUser, store.StartClaimWake, store.StartClaimCreate} {
		t.Run(string(kind), func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			id := newHaltAgent(t, ctx, s, projectID, "rw-claim-"+string(kind), defaultHaltRow())
			setStartClaim(t, ctx, s, id, kind, time.Now())
			apply(t, ctx, s, id, restartWindowHeartbeat("run-1", "exited (1)", "applies"))
			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, "applies", got.Message)
		})
	}
}

// TestAgentRestartWindow_NoRunRowRestartClaimDoesNotDrop: on a row with no
// run (one not dispatched since run IDs) the restart window rule does not
// apply. There a failed restart records no stopped state of its own, so the
// dying container's report is the only record of the stop.
func TestAgentRestartWindow_NoRunRowRestartClaimDoesNotDrop(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	row := defaultHaltRow()
	row.runID = ""
	id := newHaltAgent(t, ctx, s, projectID, "rw-norun", row)
	setStartClaim(t, ctx, s, id, store.StartClaimRestart, time.Now())
	report := stoppedFlicker("")
	report.ExitReason, report.ExitCode = "", nil
	apply(t, ctx, s, id, report)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
	assert.Equal(t, "old pod stopped", got.Message)
}

func TestAgentRestartWindow_RestartHubInternalWritesApply(t *testing.T) {
	ctx := context.Background()
	s, id := preMintAgent(t, ctx, "rw-internal")
	// A hub write (no ReportSource), such as a failed restart recording
	// stopped, is not a report: the restart window rule leaves it alone.
	zero := 0
	require.NoError(t, s.UpdateAgentStatus(ctx, id, store.AgentStatusUpdate{
		Phase: "stopped", ContainerStatus: "stopped", ExitCode: &zero}))
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
}

func TestAgentRestartWindow_RunlessAgentTokenDroppedWhenMinted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, "rw-token", defaultHaltRow())
	before, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	self := selfReport("error", "")
	self.ReportRunID = ""
	self.ReportAgentToken = true
	self.ExitReason = "oom_killed"
	self.Message = "run-less agent"
	apply(t, ctx, s, id, self)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.ExitReason, got.ExitReason)
	assert.Equal(t, before.Message, got.Message)
}

func TestAgentRestartWindow_RunlessNonAgentReportApplies(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, "rw-user", defaultHaltRow())
	// A user or service with update access sends no run and no agent token.
	update := selfReport("running", "idle")
	update.ReportRunID = ""
	update.ReportAgentToken = false
	apply(t, ctx, s, id, update)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity)
}

func TestAgentRestartWindow_RunlessAgentTokenAppliesWhenNotMinted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	row := defaultHaltRow()
	row.runID = ""
	id := newHaltAgent(t, ctx, s, projectID, "rw-token-norun", row)
	self := selfReport("running", "idle")
	self.ReportRunID = ""
	self.ReportAgentToken = true
	apply(t, ctx, s, id, self)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity, "a row with no minted run keeps today's behaviour")
}

// clockAheadRestart is a running agent of run "run-1" whose restart claim
// is stamped ahead of the hub clock by ahead, as a Postgres start claim is
// when the database clock runs ahead of the hub's.
func clockAheadRestart(t *testing.T, ctx context.Context, slug string, ahead time.Duration) (*AgentStore, string) {
	t.Helper()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, slug, defaultHaltRow())
	setStartClaim(t, ctx, s, id, store.StartClaimRestart, time.Now().Add(ahead))
	return s, id
}

// TestAgentRestartWindow_MintWithClaimClockAheadClosesWindow: the restart
// claim's time can come from the database clock and run_id_updated_at from
// the hub clock. With the database ahead, recording the new run must still
// end the window, so the new run's report applies (store.RunIDStamp).
func TestAgentRestartWindow_MintWithClaimClockAheadClosesWindow(t *testing.T) {
	ctx := context.Background()
	s, id := clockAheadRestart(t, ctx, "rw-skew-mint", 2*time.Second)
	_, err := s.SetAgentRunID(ctx, id, "run-2", nil)
	require.NoError(t, err)
	newRun := restartWindowHeartbeat("run-2", "running (new)", "new run")
	newRun.Activity = "idle"
	apply(t, ctx, s, id, newRun)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity, "the new run's report applies although the claim time is ahead")
	assert.Equal(t, "new run", got.Message)
}

// TestAgentRestartWindow_RevertWithClaimClockAheadClosesWindow: a start leg
// that reverts to the old run makes the old run current again, so the
// window ends for it too, whatever the clocks (the swap path of
// store.RunIDStamp).
func TestAgentRestartWindow_RevertWithClaimClockAheadClosesWindow(t *testing.T) {
	ctx := context.Background()
	s, id := clockAheadRestart(t, ctx, "rw-skew-revert", 2*time.Second)
	_, err := s.SetAgentRunID(ctx, id, "run-2", nil)
	require.NoError(t, err)
	reverted, err := s.RevertAgentRunID(ctx, id, "run-2", "run-1")
	require.NoError(t, err)
	require.True(t, reverted)
	oldRun := restartWindowHeartbeat("run-1", "running", "old run current again")
	oldRun.Activity = "idle"
	apply(t, ctx, s, id, oldRun)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity, "the reverted run's report applies")
	assert.Equal(t, "old run current again", got.Message)
}

// TestAgentRestartWindow_MintClaimClockBeyondCapUsesHubClock: a claim time
// further ahead than store.RestartClaimSkewCap is not honoured; the hub
// clock is used and the skew is logged, with no ids.
func TestAgentRestartWindow_MintClaimClockBeyondCapUsesHubClock(t *testing.T) {
	var logs bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })

	ctx := context.Background()
	s, id := clockAheadRestart(t, ctx, "rw-skew-cap", 10*time.Minute)
	before := time.Now()
	_, err := s.SetAgentRunID(ctx, id, "run-2", nil)
	require.NoError(t, err)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got.RunIDUpdatedAt)
	require.NotNil(t, got.StartClaimAt)
	assert.WithinDuration(t, before, *got.RunIDUpdatedAt, time.Minute, "the hub clock is used")
	assert.True(t, got.RunIDUpdatedAt.Before(*got.StartClaimAt), "the claim time beyond the cap is not honoured")
	assert.Equal(t, 1, strings.Count(logs.String(), "start claim time is ahead of the hub clock beyond the cap"), "one warning")
	assert.NotContains(t, logs.String(), id, "the warning names no ids")
}

// TestAgentRestartWindow_RevertClaimClockBeyondCapUsesHubClock: the swap
// path (RevertAgentRunID) applies the same cap as SetAgentRunID.
func TestAgentRestartWindow_RevertClaimClockBeyondCapUsesHubClock(t *testing.T) {
	var logs bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLog) })

	ctx := context.Background()
	s, id := clockAheadRestart(t, ctx, "rw-skew-cap-revert", 10*time.Minute)
	_, err := s.SetAgentRunID(ctx, id, "run-2", nil)
	require.NoError(t, err)
	before := time.Now()
	reverted, err := s.RevertAgentRunID(ctx, id, "run-2", "run-1")
	require.NoError(t, err)
	require.True(t, reverted)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, got.RunIDUpdatedAt)
	require.NotNil(t, got.StartClaimAt)
	assert.WithinDuration(t, before, *got.RunIDUpdatedAt, time.Minute, "the hub clock is used")
	assert.True(t, got.RunIDUpdatedAt.Before(*got.StartClaimAt), "the claim time beyond the cap is not honoured")
	assert.Equal(t, 2, strings.Count(logs.String(), "start claim time is ahead of the hub clock beyond the cap"), "one warning per run change")
}
