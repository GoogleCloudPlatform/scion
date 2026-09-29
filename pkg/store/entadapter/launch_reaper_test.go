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

// setLaunchReaperArmedSince backdates the singleton launch_reaper_state row's
// armed_since directly through the ent client (white-box test setup), so
// arming-threshold tests don't need to sleep for real wall-clock time. The
// row must already exist (created by an earlier tick).
func setLaunchReaperArmedSince(t *testing.T, ctx context.Context, s *AgentStore, at time.Time) {
	t.Helper()
	_, err := s.client.LaunchReaperState.UpdateOneID(launchReaperStateID).SetArmedSince(at).Save(ctx)
	require.NoError(t, err)
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
// until the cluster would re-arm. armed_since is
// backdated directly rather than by sleeping, so the assertion is exact
// enough to distinguish the two formulas without being sensitive to
// scheduling jitter.
func TestReaper_DisarmedForReflectsElapsedSinceArmedSince(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	_ = createLaunchableAgent(t, ctx, s, projectID, "reaper-disarmedfor-elapsed")

	// First tick creates the row and arms it at this tick's storeNow.
	_, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)

	// Backdate armed_since to well under the 8x keepalive arming threshold
	// (8*20ms=160ms), so the next tick observes a still-disarmed cluster.
	elapsed := 3 * testReaperParams.KeepaliveInterval
	setLaunchReaperArmedSince(t, ctx, s, time.Now().Add(-elapsed))

	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.False(t, result.Armed, "setup: elapsed must stay under the 8x keepalive arming threshold")
	// The wrong formula (8*KeepaliveInterval - elapsed) would report ~100ms
	// here instead of ~60ms; a 40ms delta comfortably separates the two even
	// with scheduling jitter.
	assert.InDelta(t, elapsed.Milliseconds(), result.DisarmedFor.Milliseconds(), 20,
		"DisarmedFor must equal storeNow - armed_since while disarmed, not time remaining until arming")
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

func TestReaper_R10_4_TickLevelFailureDisarms(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestAgentStore(t)

	// Arm the cluster first: a single healthy tick is not enough — arming
	// requires 8x keepalive (160ms here) since armed_since. Without this
	// setup, Armed == false would hold on the failing tick below regardless
	// of whether the disarm write actually runs, making the assertion
	// vacuous.
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	time.Sleep(300 * time.Millisecond)
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.True(t, result.Armed, "setup: the cluster must be armed before this test exercises disarming it")

	withLaunchReaperFailureHook(t, func(point string) error {
		if point == "ok_at_write" {
			return errors.New("injected ok_at write failure")
		}
		return nil
	})
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickFailed, result.Outcome)
	launchReaperFailureHook = nil

	// If the best-effort disarm write ran, armed_since was just reset to
	// "now" (on a fresh connection/context), so the very next tick — run
	// immediately, well within 8x keepalive of that reset — observes the
	// cluster as freshly disarmed. Without the best-effort disarm write,
	// armed_since stays at its old, already-8x-keepalive-old value from the
	// arming step above, and this tick would still see Armed == true.
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.False(t, result.Armed, "a tick-level failure must disarm the cluster (best-effort disarm write)")
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

	// Arm the cluster first: a single tick is not enough time to be armed,
	// so a commit failure's disarm would be unobservable without this setup.
	result, err := s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.Equal(t, store.ReaperTickCompleted, result.Outcome)
	time.Sleep(300 * time.Millisecond)
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	require.True(t, result.Armed, "setup: the cluster must be armed before this test exercises disarming it")

	withLaunchReaperFailureHook(t, func(point string) error {
		if point == "commit" {
			return errors.New("injected commit failure")
		}
		return nil
	})
	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickFailed, result.Outcome)
	launchReaperFailureHook = nil

	result, err = s.RunLaunchReaperTick(ctx, testReaperParams)
	require.NoError(t, err)
	assert.Equal(t, store.ReaperTickCompleted, result.Outcome)
	assert.False(t, result.Armed, "a failed commit must disarm the cluster via the best-effort disarm write")
}
