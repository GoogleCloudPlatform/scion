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

// This file covers design t1-async-create-v11.md §6 test case H-2 (the
// launch reaper). Timings use stored timestamps set relative to the store
// clock (design §3.3), not a faked Go clock, per the design's own testing
// note; a short real KeepaliveInterval/ReaperInterval keeps the suite fast.
package entadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/launchreaperstate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testReaperParams keeps the staleness/arming windows small (milliseconds)
// so tests don't need to sleep for real minutes.
var testReaperParams = store.ReaperParams{
	KeepaliveInterval: 20 * time.Millisecond,
	ReaperInterval:    20 * time.Millisecond,
}

// setAgentLaunchDeadline backdates a row's launch_deadline directly through
// the ent client (white-box test setup: production code only ever sets it via
// BeginLaunch's now+timeout).
func setAgentLaunchDeadline(t *testing.T, ctx context.Context, s *AgentStore, agentID string, deadline time.Time) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetLaunchDeadline(deadline).Save(ctx)
	require.NoError(t, err)
}

func setAgentLastReportAt(t *testing.T, ctx context.Context, s *AgentStore, agentID string, at time.Time) {
	t.Helper()
	uid, err := parseUUID(agentID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetLaunchLastReportAt(at).Save(ctx)
	require.NoError(t, err)
}

// setLaunchReaperOkAt backdates the singleton launch_reaper_state row's ok_at
// directly through the ent client (white-box test setup), simulating "no
// replica has completed a tick in a while" — a store outage or a stuck lock
// holder — without a real wait. The row must already exist (created by an
// earlier tick). The next tick's arm check compares this against its own
// storeNow, so backdating it far enough back makes that tick re-arm
// (disarm), exactly as it would after a real outage.
func setLaunchReaperOkAt(t *testing.T, ctx context.Context, s *AgentStore, at time.Time) {
	t.Helper()
	_, err := s.client.LaunchReaperState.UpdateOneID(launchReaperStateID).SetOkAt(at).Save(ctx)
	require.NoError(t, err)
}

// setLaunchReaperArmedSince backdates the singleton launch_reaper_state row's
// armed_since directly through the ent client (white-box test setup), so
// arming-threshold tests don't need to sleep for real wall-clock time. The
// row must already exist (created by an earlier tick).
func setLaunchReaperArmedSince(t *testing.T, ctx context.Context, s *AgentStore, at time.Time) {
	t.Helper()
	_, err := s.client.LaunchReaperState.UpdateOneID(launchReaperStateID).SetArmedSince(at).Save(ctx)
	require.NoError(t, err)
}

// readStoreNow reads the store clock exactly the way RunLaunchReaperTick does
// (storeNow in launch_store.go: "SELECT now()" on Postgres, time.Now() on
// SQLite), so a test can anchor a backdated timestamp to the same clock a
// later tick's DisarmedFor computation will compare it against. Using the
// test process's own time.Now() instead would be vulnerable to clock skew
// between the test host and a remote Postgres server. On SQLite, storeNow
// never touches the database, so this skips the connection checkout and
// transaction entirely and returns time.Now() directly, avoiding pointless
// work and any pool-exhaustion or deadlock risk on single-connection SQLite
// setups.
func readStoreNow(t *testing.T, ctx context.Context, s *AgentStore) time.Time {
	t.Helper()
	isPG := s.dialect(ctx) == dialect.Postgres
	if !isPG {
		return time.Now()
	}
	db := s.sqlDB()
	require.NotNil(t, db)
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	now, err := storeNow(ctx, tx, isPG)
	require.NoError(t, err)
	return now
}

func TestReaper_FreshStateIsDisarmed_DeadlineRuleStillApplies(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-fresh")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	// Force the deadline into the past so this tick's deadline selection
	// (never gated by arming) picks it up on the very first tick.
	setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.False(t, result.Armed, "a brand-new launch_reaper_state row must start disarmed")
	assert.Equal(t, time.Duration(0), result.DisarmedFor, "armed_since is set to this same tick's storeNow, so DisarmedFor must be 0 on the very first tick")
	require.Len(t, result.Reaped, 1)
	assert.Equal(t, a.ID, result.Reaped[0].ID)
	assert.Equal(t, store.LaunchErrorLaunchTimeout, result.Reaped[0].LaunchError)
	assert.Equal(t, store.LaunchEndReasonTimedOut, result.Reaped[0].LaunchEndReason)
	assert.Equal(t, "error", result.Reaped[0].Phase)

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, launchID, after.LaunchID)
	assert.Equal(t, store.LaunchStateEnded, after.LaunchState)
}

func TestReaper_ArmsAfterFirstTick_ThenReapsStaleness(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-arm")

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)

	// Tick 1: no due rows, but it completes and writes ok_at, starting the
	// arming clock.
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.Empty(t, result.Reaped)

	// Not yet armed: staleness must not fire even if last_report_at looks
	// old, because 8x keepalive hasn't elapsed since arming.
	setAgentLastReportAt(t, ctx, s, a.ID, time.Now().Add(-time.Hour))
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Empty(t, result.Reaped, "staleness must not reap before the cluster has been armed for 8x keepalive")

	// Wait past the 8x keepalive arming window (8*20ms=160ms) and tick again.
	time.Sleep(300 * time.Millisecond)
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.True(t, result.Armed)
	assert.Equal(t, time.Duration(0), result.DisarmedFor, "DisarmedFor must be 0 once armed")
	require.Len(t, result.Reaped, 1)
	assert.Equal(t, store.LaunchErrorBrokerLost, result.Reaped[0].LaunchError)
	assert.Equal(t, store.LaunchEndReasonLost, result.Reaped[0].LaunchEndReason)
}

// TestReaper_DisarmedForReflectsElapsedSinceArmedSince proves DisarmedFor
// reports storeNow - armed_since while disarmed, not the time remaining
// until the cluster would re-arm. armed_since is backdated relative to the
// store's own clock (readStoreNow), not the test process's time.Now(), so
// clock skew between the test host and a remote Postgres server can't
// inflate the observed DisarmedFor. The assertion is a one-sided band rather
// than a tight InDelta: a lower bound (DisarmedFor must be at least elapsed)
// that query latency only ever pushes up, never down, and an upper bound
// chosen to stay clear of the wrong formula's value even under latency.
func TestReaper_DisarmedForReflectsElapsedSinceArmedSince(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	_ = createLaunchableAgent(t, ctx, s, projectID, "reaper-disarmedfor-elapsed")

	// First tick creates the row and arms it at this tick's storeNow.
	_, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)

	// Use a small elapsed (1x keepalive = 20ms), well under the 8x keepalive
	// arming threshold (160ms), so the two candidate formulas land far apart:
	// the correct formula (storeNow - armed_since) reports ~20ms plus
	// whatever latency elapses before the next tick's storeNow read, while
	// the wrong formula (8*KeepaliveInterval - elapsed) would report a fixed
	// ~140ms regardless of latency.
	elapsed := testReaperParams.KeepaliveInterval
	dbNow := readStoreNow(t, ctx, s)
	setLaunchReaperArmedSince(t, ctx, s, dbNow.Add(-elapsed))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.False(t, result.Armed, "setup: elapsed must stay under the 8x keepalive arming threshold")
	// Lower bound: DisarmedFor = storeNow - armed_since can only grow from
	// elapsed as latency between readStoreNow above and this tick's own
	// storeNow read adds on top of it; allow a couple of ms of slack for
	// timestamp quantization. The wrong formula does not satisfy this bound
	// (it would read ~140ms high, which does pass >= elapsed, but is caught
	// by the upper bound below instead).
	assert.GreaterOrEqual(t, result.DisarmedFor.Milliseconds(), elapsed.Milliseconds()-2,
		"DisarmedFor must be at least elapsed since armed_since (storeNow - armed_since), not time remaining until arming")
	// Upper bound: stay comfortably below the wrong formula's ~140ms
	// (8*20ms - 20ms) so ordinary query latency on the correct formula can't
	// cross into the wrong formula's range and make the two indistinguishable.
	assert.Less(t, result.DisarmedFor.Milliseconds(), int64(100),
		"DisarmedFor must stay far below the wrong formula's value (8*KeepaliveInterval-elapsed ~= 140ms); a value this high suggests the wrong formula is in effect")
}

// TestReaper_DisarmedForZeroWhenArmed proves DisarmedFor is 0 once the
// cluster is armed, using a backdated armed_since so the test does not need
// to sleep past the real 8x keepalive threshold.
func TestReaper_DisarmedForZeroWhenArmed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	_ = createLaunchableAgent(t, ctx, s, projectID, "reaper-disarmedfor-armed")

	_, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)

	setLaunchReaperArmedSince(t, ctx, s, time.Now().Add(-8*testReaperParams.KeepaliveInterval-time.Second))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.True(t, result.Armed, "setup: armed_since must be far enough in the past to arm")
	assert.Equal(t, time.Duration(0), result.DisarmedFor, "DisarmedFor must be 0 once armed, not time until it would have re-armed")
}

func TestReaper_WindDownReap(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-winddown")

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)

	// Simulate the Hub having written "stopped" while the launch is still
	// active (the wind-down window: the broker has not reported yet).
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).SetPhase("stopped").Save(ctx)
	require.NoError(t, err)

	// Arm the cluster first.
	_, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)

	setAgentLastReportAt(t, ctx, s, a.ID, time.Now().Add(-time.Hour))
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Len(t, result.Reaped, 1)
	assert.Equal(t, "stopped", result.Reaped[0].Phase, "wind-down reap leaves the phase alone")
	assert.Equal(t, store.LaunchErrorLaunchStopped, result.Reaped[0].LaunchError)
	assert.Equal(t, store.LaunchEndReasonLost, result.Reaped[0].LaunchEndReason)
	assert.Equal(t, store.LaunchStateEnded, result.Reaped[0].LaunchState)
}

func TestReaper_NeverTouchesRunningAgent(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-running")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))

	// A racing status write reaches "running" before the tick runs, which
	// ends the launch as running_observed (design §3.3). The reaper must not
	// select it even though its (stale) deadline is in the past, because the
	// in-flight predicate excludes phase=running and launch_state is no
	// longer active.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Empty(t, result.Reaped)

	after, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", after.Phase)
	assert.Equal(t, store.LaunchEndReasonRunningObserved, after.LaunchEndReason)
	assert.Equal(t, launchID, after.LaunchID)
}

func TestReaper_LateFailedAfterReapRefinesInsteadOfCompleting(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-late-failed")

	launchID, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Len(t, result.Reaped, 1)

	// The broker's failed report arrives after the reap.
	answer, updated, err := s.ApplyLaunchReport(ctx, a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "instance-1", State: store.LaunchReportStateFailed,
		Step: "pod_create", Message: "quota exceeded", ErrorCode: "image_pull_failed",
	})
	require.NoError(t, err)
	assert.Equal(t, store.LaunchReportResultApplied, answer.Result, "a late failed on a reaped launch must refine, not 200 completed")
	assert.Equal(t, "image_pull_failed", updated.LaunchError)
	assert.Contains(t, updated.Message, "pod_create")
}

func TestReaper_ReleasesReservationOnDeadlineReap(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-quota")
	a.RuntimeBrokerID = "broker-1"
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{}))

	quota := NewQuotaStore(s.client)
	ld, err := quota.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name: store.LimitMaxAgentsPerBroker, ResourceType: "agent", Unit: "count", DefaultValue: 1,
	})
	require.NoError(t, err)
	_, err = quota.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: ld.ID, SubjectID: "broker-1", ScopeType: "broker", ScopeID: "broker-1",
		ResourceID: a.ID, Reserved: 1,
	})
	require.NoError(t, err)

	has, err := quota.HasActiveReservation(ctx, ld.ID, a.ID)
	require.NoError(t, err)
	require.True(t, has)

	_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Len(t, result.Reaped, 1)

	has, err = quota.HasActiveReservation(ctx, ld.ID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "the reaper must release the max_agents_per_broker reservation through its own transaction")
}

// TestReaper_DeletedRowIsNoOp covers a candidate row that vanishes between
// selection and reap (here, simulated by deleting it outright rather than
// racing a real concurrent delete, which is not deterministic in this
// harness): reapRow treats a vanished row as a benign skip, not a row error
// or a tick failure, and the other due row still reaps. The genuine
// poison-row case — a row whose reap WRITE fails — is covered separately by
// TestReaper_R10_5_RowErrorDoesNotDisarm, which asserts RowErrors and that
// the row is left for a later tick.
func TestReaper_DeletedRowIsNoOp(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	good := createLaunchableAgent(t, ctx, s, projectID, "reaper-good")
	vanished := createLaunchableAgent(t, ctx, s, projectID, "reaper-vanished")

	_, err := s.BeginLaunch(ctx, good.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, good.ID, time.Now().Add(-time.Second))

	_, err = s.BeginLaunch(ctx, vanished.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, vanished.ID, time.Now().Add(-time.Second))
	uid, err := parseUUID(vanished.ID)
	require.NoError(t, err)
	require.NoError(t, s.client.Agent.DeleteOneID(uid).Exec(ctx))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.Zero(t, result.RowErrors, "a vanished row is a benign skip, not a row error")
	require.Len(t, result.Reaped, 1)
	assert.Equal(t, good.ID, result.Reaped[0].ID)
}

// TestReaper_PostgresRowLock_SkippedAndRetriedNextTick is PG-only: FOR
// UPDATE SKIP LOCKED has no SQLite equivalent (single writer), so this can
// only be exercised against a real Postgres backend.
func TestReaper_PostgresRowLock_SkippedAndRetriedNextTick(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	locked := createLaunchableAgent(t, ctx, s, projectID, "reaper-pg-lock")
	other := createLaunchableAgent(t, ctx, s, projectID, "reaper-pg-other")

	_, err := s.BeginLaunch(ctx, locked.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, locked.ID, time.Now().Add(-time.Second))

	_, err = s.BeginLaunch(ctx, other.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, other.ID, time.Now().Add(-time.Second))

	// Hold a row lock on the first agent on a separate connection/transaction
	// for longer than one tick. The second, unlocked due row must still be
	// reaped in the same tick.
	tx, err := s.client.Tx(ctx)
	require.NoError(t, err)
	uid, err := parseUUID(locked.ID)
	require.NoError(t, err)
	_, err = tx.Agent.Query().Where(agent.IDEQ(uid)).ForUpdate().Only(ctx)
	require.NoError(t, err)

	rsBefore, err := s.client.LaunchReaperState.Query().Where(launchreaperstate.IDEQ(launchReaperStateID)).Only(ctx)
	okAtBefore := (*time.Time)(nil)
	if err == nil {
		okAtBefore = rsBefore.OkAt
	}

	start := time.Now()
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome, "the tick completes even though one due row is locked elsewhere")
	assert.Less(t, elapsed, 5*time.Second, "the tick must stay well within its 10s budget while a row is locked")
	require.Len(t, result.Reaped, 1, "the unlocked due row must still be reaped in the same tick")
	assert.Equal(t, other.ID, result.Reaped[0].ID)

	rsAfter, err := s.client.LaunchReaperState.Query().Where(launchreaperstate.IDEQ(launchReaperStateID)).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, rsAfter.OkAt)
	if okAtBefore != nil {
		assert.True(t, rsAfter.OkAt.After(*okAtBefore), "ok_at must advance even though one row was skipped")
	}

	require.NoError(t, tx.Rollback())

	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Len(t, result.Reaped, 1, "the locked row is reaped on the next tick once the lock is released")
	assert.Equal(t, locked.ID, result.Reaped[0].ID)
}

// --- design §6 H-2: reaper tick-level failure handling ---------------------
//
// These use launchReaperFailureHook to inject a failure at a named tick-level
// statement deterministically, since SQLite gives no way to force a genuine
// lock, connection or savepoint failure from Go, and this sandbox has no
// Postgres server to exercise the real statements against (see the CI job
// for that). The hook fires at the same call sites the real statements
// execute from, so it exercises the same branch structure the real driver
// errors would.

func withLaunchReaperFailureHook(t *testing.T, fn func(point string) error) {
	t.Helper()
	prev := launchReaperFailureHook
	launchReaperFailureHook = fn
	t.Cleanup(func() { launchReaperFailureHook = prev })
}

// disarmObserveParams is used by the "a failed tick disarms" tests
// (R10_4, R10_5). It makes the arming threshold (8x keepalive = 8 minutes)
// and the stale-ok_at re-disarm window (ReaperInterval + 5s margin) far
// longer than any CI scheduling delay. With testReaperParams' 160ms
// threshold, the tick that checks "now disarmed" could itself see the
// cluster re-armed: under CPU load, more than 160ms can pass between the
// best-effort disarm's storeNow and that tick's storeNow (ptone/scion#3002).
var disarmObserveParams = store.ReaperParams{
	KeepaliveInterval: time.Minute,
	ReaperInterval:    time.Minute,
}

// armLaunchReaperForDisarmTest arms the cluster without relying on real
// sleeps: one tick creates the arming row, then armed_since is backdated an
// hour on the store clock, well past disarmObserveParams' 8-minute arming
// threshold. It returns the backdated armed_since. A later tick using
// disarmObserveParams then reports Armed == false only if something reset
// armed_since to a recent value, such as the best-effort disarm write.
func armLaunchReaperForDisarmTest(t *testing.T, ctx context.Context, s *AgentStore) time.Time {
	t.Helper()
	result, err := s.RunLaunchReaperTick(ctx, disarmObserveParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	armedSince := readStoreNow(t, ctx, s).Add(-time.Hour)
	setLaunchReaperArmedSince(t, ctx, s, armedSince)
	result, err = s.RunLaunchReaperTick(ctx, disarmObserveParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	require.True(t, result.Armed, "setup: the cluster must be armed before this test exercises disarming it")
	return armedSince
}

// requireDisarmedAfterFailedTick checks that the failed tick reset
// armed_since: first by reading the persisted row, then through a normal
// tick. Both checks are deterministic. Without the best-effort disarm
// write, armed_since stays an hour old and both fail.
func requireDisarmedAfterFailedTick(t *testing.T, ctx context.Context, s *AgentStore, armedBefore time.Time, msg string) {
	t.Helper()
	rs, err := s.client.LaunchReaperState.Get(ctx, launchReaperStateID)
	require.NoError(t, err)
	require.NotNil(t, rs.ArmedSince)
	assert.True(t, rs.ArmedSince.After(armedBefore.Add(30*time.Minute)), "%s: armed_since must be reset to the disarm write's storeNow (was %v, now %v)", msg, armedBefore, *rs.ArmedSince)

	result, err := s.RunLaunchReaperTick(ctx, disarmObserveParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.False(t, result.Armed, msg)
}

func TestReaper_R10_4_TickLevelFailureDisarms(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestAgentStore(t)

	// Arm the cluster first. Otherwise Armed == false would hold after the
	// failing tick whether or not the disarm write runs, so the check
	// would prove nothing.
	armedBefore := armLaunchReaperForDisarmTest(t, ctx, s)

	withLaunchReaperFailureHook(t, func(point string) error {
		if point == "ok_at_write" {
			return errors.New("injected ok_at write failure")
		}
		return nil
	})
	result, err := s.RunLaunchReaperTick(ctx, disarmObserveParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickFailed, result.Outcome)
	launchReaperFailureHook = nil

	requireDisarmedAfterFailedTick(t, ctx, s, armedBefore, "a tick-level failure must disarm the cluster (best-effort disarm write)")
}

// TestReaper_R10_5_LockAndSetLocalErrorsMapToUnavailable is PG-only: the
// lock and SET LOCAL injection points sit inside `if isPG`, right beside the
// real pg_try_advisory_xact_lock/SET LOCAL statements' own error checks, so
// they exercise the exact branches those statements' real errors would take
// — but that also means they cannot fire at all on SQLite, which never
// takes the isPG branch. Each sub-test is independent (a fresh store, one
// injection point at a time).
func TestReaper_R10_5_LockAndSetLocalErrorsMapToUnavailable(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	for _, point := range []string{"lock", "set_local_lock_timeout", "set_local_idle_timeout"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			s, _ := newTestAgentStore(t)
			injectPoint := point
			withLaunchReaperFailureHook(t, func(p string) error {
				if p == injectPoint {
					return errors.New("injected")
				}
				return nil
			})
			result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
			require.NoError(t, err)
			assert.Equal(t, store.ReaperTickUnavailable, result.Outcome, "a lock/SET LOCAL failure must map to unavailable, not failed")
		})
	}
}

func TestReaper_R10_5_SavepointErrorMapsToFailed(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-savepoint-fail")
	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))

	withLaunchReaperFailureHook(t, func(point string) error {
		if point == "savepoint" {
			return errors.New("injected savepoint failure")
		}
		return nil
	})
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickFailed, result.Outcome, "a SAVEPOINT/ROLLBACK TO SAVEPOINT/RELEASE failure must map to failed, not unavailable")
}

func TestReaper_R10_5_RowErrorDoesNotDisarm(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	// Arm the cluster first (a tick with nothing due).
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	time.Sleep(300 * time.Millisecond)

	good := createLaunchableAgent(t, ctx, s, projectID, "reaper-row-good")
	_, err = s.BeginLaunch(ctx, good.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, good.ID, time.Now().Add(-time.Second))

	poison := createLaunchableAgent(t, ctx, s, projectID, "reaper-row-poison")
	_, err = s.BeginLaunch(ctx, poison.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, poison.ID, time.Now().Add(-time.Second))

	// A genuine per-row write failure (rolled back to its own savepoint, not
	// a tick-level failure) on the poison row only: the good row still
	// reaps, and the tick still completes and advances ok_at.
	withLaunchReaperFailureHook(t, func(point string) error {
		if point == "row_apply:"+poison.ID {
			return errors.New("injected row apply failure")
		}
		return nil
	})

	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.True(t, result.Armed, "a per-row failure must not disarm the cluster")
	assert.Equal(t, 1, result.RowErrors)
	require.Len(t, result.Reaped, 1)
	assert.Equal(t, good.ID, result.Reaped[0].ID)

	poisonAfter, err := s.GetAgent(ctx, poison.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateActive, poisonAfter.LaunchState, "the poison row is left for a later tick")
}

// --- design §6 H-2 (PG-only): not_acquired / pool-exhausted unavailable ---

// newPGTestAgentStore opens a fresh, migrated Postgres schema with a custom
// connection pool size and seeds the shared test project. Skips (via
// enttest.NewSchemaURL) unless built with -tags integration and
// SCION_TEST_POSTGRES_URL is set.
func newPGTestAgentStore(t *testing.T, maxOpenConns int) (*AgentStore, string) {
	t.Helper()
	url := enttest.NewSchemaURL(t)
	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: maxOpenConns, MaxIdleConns: maxOpenConns})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	_, err = client.Project.Create().
		SetID(agentTestProjectUID).
		SetName("test-project").
		SetSlug("test-project").
		Save(context.Background())
	require.NoError(t, err)

	return NewAgentStore(client), agentTestProjectUID.String()
}

// TestReaper_R10_4_NotAcquiredLeavesArmingRowUnchanged holds the transaction-
// scoped advisory lock open on a separate connection, then asserts a
// concurrent tick reports not_acquired and does not touch the arming row.
func TestReaper_R10_4_NotAcquiredLeavesArmingRowUnchanged(t *testing.T) {
	s, _ := newPGTestAgentStore(t, 2) // one for the holder, one for the tick
	ctx := context.Background()

	// A first tick creates and populates the arming row.
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	rs1, err := s.client.LaunchReaperState.Get(ctx, launchReaperStateID)
	require.NoError(t, err)
	require.NotNil(t, rs1.OkAt)
	okAtBefore := *rs1.OkAt

	// Hold the lock on a separate connection/transaction.
	db := s.sqlDB()
	require.NotNil(t, db)
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var acquired bool
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", int64(store.LockAgentLaunchDeadline)).Scan(&acquired))
	require.True(t, acquired, "setup: the holder must acquire the lock first")

	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickNotAcquired, result.Outcome)

	rs2, err := s.client.LaunchReaperState.Get(ctx, launchReaperStateID)
	require.NoError(t, err)
	require.NotNil(t, rs2.OkAt)
	assert.True(t, okAtBefore.Equal(*rs2.OkAt), "not_acquired must leave the arming row unchanged")
}

// TestReaper_R10_4_PoolExhaustedIsUnavailable holds the only pooled
// connection open (MaxOpenConns=1), so the tick's own connection checkout
// times out and the tick reports unavailable.
func TestReaper_R10_4_PoolExhaustedIsUnavailable(t *testing.T) {
	s, _ := newPGTestAgentStore(t, 1)
	ctx := context.Background()

	// A first tick (before the pool's only connection is pinned below)
	// creates and populates the arming row.
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	rs1, err := s.client.LaunchReaperState.Get(ctx, launchReaperStateID)
	require.NoError(t, err)
	require.NotNil(t, rs1.OkAt)
	okAtBefore := *rs1.OkAt

	db := s.sqlDB()
	require.NotNil(t, db)
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)

	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	// Release the pool's only connection before doing anything else on
	// s.client below — otherwise the verification read would itself block
	// forever waiting for a connection from the same exhausted, MaxOpenConns=1
	// pool.
	_ = tx.Rollback()
	_ = conn.Close()
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickUnavailable, result.Outcome, "a tick that cannot even check out a connection must report unavailable")

	rs2, err := s.client.LaunchReaperState.Get(ctx, launchReaperStateID)
	require.NoError(t, err)
	require.NotNil(t, rs2.OkAt)
	assert.True(t, okAtBefore.Equal(*rs2.OkAt), "unavailable must leave the arming row unchanged")
}

// TestReaper_R10_2_ReleasesReservationOnPGWithSingleConnection is the
// Postgres counterpart to TestReaper_ReleasesReservationOnDeadlineReap: with
// MaxOpenConns=1, the only way this tick can complete at all is if
// releaseBrokerQuotaTx reuses the tick's own
// transaction/connection rather than opening a second one on s.client,
// which would deadlock against the exhausted pool.
func TestReaper_R10_2_ReleasesReservationOnPGWithSingleConnection(t *testing.T) {
	s, projectID := newPGTestAgentStore(t, 1)
	ctx := context.Background()
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-quota-pg-single-conn")
	a.RuntimeBrokerID = "broker-1"
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{}))

	quota := NewQuotaStore(s.client)
	ld, err := quota.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name: store.LimitMaxAgentsPerBroker, ResourceType: "agent", Unit: "count", DefaultValue: 1,
	})
	require.NoError(t, err)
	_, err = quota.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: ld.ID, SubjectID: "broker-1", ScopeType: "broker", ScopeID: "broker-1",
		ResourceID: a.ID, Reserved: 1,
	})
	require.NoError(t, err)

	_, err = s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	setAgentLaunchDeadline(t, ctx, s, a.ID, time.Now().Add(-time.Second))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome, "the tick must complete within budget on a single-connection pool")
	require.Len(t, result.Reaped, 1)

	has, err := quota.HasActiveReservation(ctx, ld.ID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "the reaper must release the reservation through its own transaction, not a second connection")
}

// TestReaper_R10_5_CommitFailureDisarms proves a failed commit disarms the
// cluster via the fresh-context best-effort disarm write.
func TestReaper_R10_5_CommitFailureDisarms(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestAgentStore(t)

	// Arm the cluster first: a commit failure's disarm would be
	// unobservable without this setup.
	armedBefore := armLaunchReaperForDisarmTest(t, ctx, s)

	withLaunchReaperFailureHook(t, func(point string) error {
		if point == "commit" {
			return errors.New("injected commit failure")
		}
		return nil
	})
	result, err := s.RunLaunchReaperTick(ctx, disarmObserveParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickFailed, result.Outcome)
	launchReaperFailureHook = nil

	requireDisarmedAfterFailedTick(t, ctx, s, armedBefore, "a failed commit must disarm the cluster via the best-effort disarm write")
}

// --- design §6 H-2: extended arming scenarios (store outage, two replicas) --
//
// These two cases simulate a real outage's duration by backdating the arming
// row's stored timestamps (setLaunchReaperOkAt / setLaunchReaperArmedSince)
// rather than sleeping in real time for the production durations (180s,
// 120s) — the same technique setAgentLaunchDeadline already uses for
// launch_deadline, and consistent with this file's clock-basis note above:
// timings are stored timestamps relative to the store clock, not a faked Go
// clock. testReaperParams' short KeepaliveInterval/ReaperInterval (20ms) make
// the actual arming/staleness thresholds (8x keepalive = 160ms) fast in real
// time; what is backdated is only the *history* (ok_at, armed_since) a real
// 180s outage or a real 60s stuck lock holder would have produced.

// TestReaper_H2_OutageRecovery_ResumingAndSilentBroker covers the design's
// "180s store outage" case: after a store outage longer than the staleness
// window, a launch whose broker resumes keepalives within seconds is never
// marked lost, and a launch whose broker stays silent is marked lost only
// once the cluster has been armed again for a full staleness window — not at
// the moment of recovery, even though its last report is already far older
// than that window.
func TestReaper_H2_OutageRecovery_ResumingAndSilentBroker(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	resuming := createLaunchableAgent(t, ctx, s, projectID, "reaper-outage-resuming")
	silent := createLaunchableAgent(t, ctx, s, projectID, "reaper-outage-silent")

	_, err := s.BeginLaunch(ctx, resuming.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	_, err = s.BeginLaunch(ctx, silent.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)

	// Steady state before the outage: armed, both agents' last report already
	// older than the staleness window would normally require — but not yet
	// stale relative to armed_since, since arming just happened.
	preOutage := time.Now().Add(-time.Hour)
	setAgentLastReportAt(t, ctx, s, resuming.ID, preOutage)
	setAgentLastReportAt(t, ctx, s, silent.ID, preOutage)

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams) // creates the arming row
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)

	// Simulate the outage: no tick has completed for far longer than
	// reaperInterval + the arming margin, as a real 180s outage would leave
	// ok_at.
	setLaunchReaperOkAt(t, ctx, s, time.Now().Add(-time.Hour))

	// Recovery tick: the arm check sees a stale ok_at and re-disarms
	// (armed_since = this tick's storeNow), so this very tick must not reap
	// either agent for staleness, no matter how long their last report has
	// been silent.
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.False(t, result.Armed, "the recovery tick itself must be disarmed")
	assert.Equal(t, time.Duration(0), result.DisarmedFor, "armed_since was just reset to this tick's storeNow")
	assert.Empty(t, result.Reaped, "neither agent may be marked lost on the recovery tick itself")

	// The resuming broker starts reporting again right after recovery.
	setAgentLastReportAt(t, ctx, s, resuming.ID, time.Now())

	// A tick partway through the post-recovery staleness window (well under
	// 8x keepalive = 160ms) must still not reap the silent agent: it is not
	// "earlier" relief from the outage, it is the normal arming gate.
	time.Sleep(60 * time.Millisecond)
	setAgentLastReportAt(t, ctx, s, resuming.ID, time.Now()) // the resumed broker keeps reporting on schedule
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.False(t, result.Armed, "still within the post-recovery arming window")
	assert.Empty(t, result.Reaped, "the silent agent must not be reaped before a full staleness window has elapsed since recovery")

	// Past the full post-recovery staleness window: the cluster is armed
	// again, and only the silent agent (last report still from before the
	// outage) is reaped. The resuming agent's fresh reports keep it safe.
	time.Sleep(150 * time.Millisecond)
	setAgentLastReportAt(t, ctx, s, resuming.ID, time.Now()) // still on schedule right up to this tick
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.True(t, result.Armed, "the cluster must be armed again a full staleness window after recovery")
	require.Len(t, result.Reaped, 1, "only the silent agent should be reaped")
	assert.Equal(t, silent.ID, result.Reaped[0].ID)
	assert.Equal(t, store.LaunchErrorBrokerLost, result.Reaped[0].LaunchError)
	assert.Equal(t, store.LaunchEndReasonLost, result.Reaped[0].LaunchEndReason)

	after, err := s.GetAgent(ctx, resuming.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LaunchStateActive, after.LaunchState, "a launch whose broker resumed keepalives after the outage must never be marked lost")
}

// TestReaper_H2_TwoReplicas_NotAcquiredDoesNotAdvanceLossClock covers the
// design's two-replica case: while one replica holds the transaction-scoped
// lock (simulating a stuck replica A), a concurrent tick (replica B) only
// ever sees not_acquired and never touches the arming row — so B's failed
// attempts cannot themselves cause a launch to be marked lost, or move up
// when one can be. Once the lock is released, recovery proceeds exactly as
// TestReaper_H2_OutageRecovery_ResumingAndSilentBroker's silent-broker case:
// disarmed on the first tick after release, reaped only a full staleness
// window later.
func TestReaper_H2_TwoReplicas_NotAcquiredDoesNotAdvanceLossClock(t *testing.T) {
	s, projectID := newPGTestAgentStore(t, 2) // one for the stuck holder, one for the ticks
	ctx := context.Background()
	a := createLaunchableAgent(t, ctx, s, projectID, "reaper-tworeplica")

	_, err := s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	staleReportAt := time.Now().Add(-time.Hour)
	setAgentLastReportAt(t, ctx, s, a.ID, staleReportAt)

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams) // creates the arming row
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)

	// Simulate replica A stuck holding the tick's lock for far longer than a
	// real 60s: back-date ok_at so the eventual recovery tick disarms, then
	// grab the real advisory lock on a separate connection and hold it open.
	setLaunchReaperOkAt(t, ctx, s, time.Now().Add(-time.Hour))

	db := s.sqlDB()
	require.NotNil(t, db)
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	holderReleased := false
	defer func() {
		if !holderReleased {
			_ = tx.Rollback()
		}
	}()
	var acquired bool
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", int64(store.LockAgentLaunchDeadline)).Scan(&acquired))
	require.True(t, acquired, "setup: replica A must acquire the lock first")

	rowBefore, err := s.client.LaunchReaperState.Get(ctx, launchReaperStateID)
	require.NoError(t, err)

	// Replica B tries repeatedly while A holds the lock: every attempt is
	// neutral (not_acquired, arming row untouched), so B's own failures never
	// advance or retreat the loss clock.
	for i := 0; i < 3; i++ {
		result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
		require.NoError(t, err)
		assert.Equal(t, store.ReaperTickNotAcquired, result.Outcome)
		assert.Empty(t, result.Reaped)
	}
	rowDuring, err := s.client.LaunchReaperState.Get(ctx, launchReaperStateID)
	require.NoError(t, err)
	require.NotNil(t, rowBefore.OkAt)
	require.NotNil(t, rowDuring.OkAt)
	assert.True(t, rowBefore.OkAt.Equal(*rowDuring.OkAt), "repeated not_acquired ticks must never touch ok_at")
	require.NotNil(t, rowBefore.ArmedSince)
	require.NotNil(t, rowDuring.ArmedSince)
	assert.True(t, rowBefore.ArmedSince.Equal(*rowDuring.ArmedSince), "repeated not_acquired ticks must never touch armed_since")

	// Replica A recovers (or replica B simply wins the lock next): release it.
	require.NoError(t, tx.Rollback())
	holderReleased = true

	// The first tick after the lock frees up is the recovery tick: disarmed
	// immediately, so it must not mark the long-silent agent lost yet.
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.False(t, result.Armed, "the recovery tick itself must be disarmed")
	assert.Empty(t, result.Reaped, "the agent must not be marked lost on the recovery tick itself, however long replica B's failed attempts lasted")

	// A full staleness window after recovery, the agent — whose last report
	// never changed throughout — is finally reaped.
	time.Sleep(200 * time.Millisecond)
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.True(t, result.Armed)
	require.Len(t, result.Reaped, 1)
	assert.Equal(t, a.ID, result.Reaped[0].ID)
	assert.Equal(t, store.LaunchErrorBrokerLost, result.Reaped[0].LaunchError)
}
