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

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the idle suspend commit and recovery protocol (ptone/scion#8204).

// commitFaultStore fails the idle-suspend commit write (IfIdleSuspendCommit)
// while failures > 0.
type commitFaultStore struct {
	store.Store
	failures atomic.Int32
	attempts atomic.Int32
	// ambiguous makes the first commit write take effect and still return an
	// error, as a connection lost after the transaction committed would.
	ambiguous atomic.Bool
}

func (c *commitFaultStore) UpdateAgentStatus(ctx context.Context, id string, upd store.AgentStatusUpdate) error {
	if upd.IfIdleSuspendCommit != nil {
		c.attempts.Add(1)
		if c.failures.Add(-1) >= 0 {
			return errors.New("injected commit write failure")
		}
		if c.ambiguous.CompareAndSwap(true, false) {
			if err := c.Store.UpdateAgentStatus(ctx, id, upd); err != nil {
				return err
			}
			return errors.New("injected connection loss after commit")
		}
	}
	return c.Store.UpdateAgentStatus(ctx, id, upd)
}

func fastIdleCommitRetry(t *testing.T) {
	t.Helper()
	b, bo, bm := idleSuspendCommitBudget, idleSuspendCommitBackoff, idleSuspendCommitBackoffMax
	idleSuspendCommitBudget, idleSuspendCommitBackoff, idleSuspendCommitBackoffMax = 400*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { idleSuspendCommitBudget, idleSuspendCommitBackoff, idleSuspendCommitBackoffMax = b, bo, bm })
}

func expireIdleClaim(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	_, err := s.(*entadapter.CompositeStore).DB().ExecContext(context.Background(),
		"UPDATE agents SET idle_suspend_claim_until = ? WHERE id = ?", time.Now().UTC().Add(-time.Second), agentID)
	require.NoError(t, err)
}

func setPhaseForTest(t *testing.T, s store.Store, agentID, phase string) {
	t.Helper()
	_, err := s.(*entadapter.CompositeStore).DB().ExecContext(context.Background(),
		"UPDATE agents SET phase = ? WHERE id = ?", phase, agentID)
	require.NoError(t, err)
}

// idleRecoveryFixture is a server with the idle sweep on, a candidate agent,
// and a dispatcher whose stop lands (the row reports stopped).
type idleRecoveryFixture struct {
	srv      *Server
	raw      store.Store
	faulty   *commitFaultStore
	agent    *store.Agent
	stopErr  error
	disp     *stopMutatingDispatcher
	stopHits atomic.Int32
}

func newIdleRecoveryFixture(t *testing.T, name string, stopLands bool) *idleRecoveryFixture {
	t.Helper()
	srv, raw := testServer(t)
	f := &idleRecoveryFixture{srv: srv, raw: raw}
	f.faulty, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *commitFaultStore {
		return &commitFaultStore{Store: inner}
	})
	f.disp = &stopMutatingDispatcher{onStop: func(_ context.Context, a *store.Agent) {
		f.stopHits.Add(1)
		if stopLands {
			setPhaseForTest(t, raw, a.ID, string(state.PhaseStopped))
		}
	}}
	srv.SetDispatcher(f.disp)
	enableIdleSweepForTest(t, srv, 15*time.Minute)
	now := time.Now().UTC()
	f.agent = newIdleCandidateBrokerAgent(t, raw, name, string(state.ActivityIdle), ptrTime(now.Add(-20*time.Minute)), now.Add(-15*time.Second))
	return f
}

func (f *idleRecoveryFixture) suspendOnce(t *testing.T) {
	t.Helper()
	f.srv.autoSuspendAgents(context.Background(), []autoSuspendItem{{agent: *f.agent, reason: state.SuspendReasonIdle}})
}

func (f *idleRecoveryFixture) get(t *testing.T) *store.Agent {
	t.Helper()
	a, err := f.raw.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	return a
}

func (f *idleRecoveryFixture) sweep() {
	f.srv.agentIdleSuspendHandler()(context.Background())
	f.srv.waitAutoSuspendIdle()
}

func TestIdleSuspendRecovery_CommitWriteFailure(t *testing.T) {
	t.Run("persistent failure keeps the claim and the next sweep records suspended(idle)", func(t *testing.T) {
		fastIdleCommitRetry(t)
		f := newIdleRecoveryFixture(t, "rec-persist", true)
		f.faulty.failures.Store(1 << 20)

		f.suspendOnce(t)

		got := f.get(t)
		assert.Greater(t, f.faulty.attempts.Load(), int32(1), "the commit write is retried")
		assert.Equal(t, string(state.PhaseStopped), got.Phase)
		assert.Empty(t, got.SuspendReason)
		assert.Equal(t, store.RunIntentStopped, got.RunIntent, "the intent is never reverted after the stop landed")
		require.NotNil(t, got.IdleSuspendClaimUntil, "the claim is kept so recovery can settle it")

		// While the lease is active the sweep leaves the row to the worker.
		f.faulty.failures.Store(0)
		f.sweep()
		assert.Equal(t, string(state.PhaseStopped), f.get(t).Phase)

		expireIdleClaim(t, f.raw, f.agent.ID)
		f.sweep()
		got = f.get(t)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
		assert.Nil(t, got.IdleSuspendClaimUntil)
		assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	})

	t.Run("transient failures are retried inside the worker", func(t *testing.T) {
		fastIdleCommitRetry(t)
		f := newIdleRecoveryFixture(t, "rec-transient", true)
		f.faulty.failures.Store(2)

		f.suspendOnce(t)

		got := f.get(t)
		assert.Equal(t, int32(3), f.faulty.attempts.Load())
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
		assert.Nil(t, got.IdleSuspendClaimUntil)
	})
}

func TestIdleSuspendRecovery_CrashBetweenStopAndCommit(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetDispatcher(&runIntentDispatcher{})
	enableIdleSweepForTest(t, srv, 15*time.Minute)
	now := time.Now().UTC()
	a := newIdleCandidateBrokerAgent(t, s, "rec-crash", string(state.ActivityIdle), ptrTime(now.Add(-20*time.Minute)), now.Add(-15*time.Second))

	// The worker claimed and dispatched the stop, then the hub went away: only
	// the claim and the container's own stopped report remain.
	cond := store.IdleSuspendCondition{RunID: a.RunID, Activity: a.Activity, ActivitySince: *a.ActivitySince,
		IdleSuspendAfter: 15 * time.Minute, HeartbeatRecency: 2 * time.Minute, ClaimTTL: time.Minute}
	_, _, _, err := s.ClaimIdleSuspend(ctx, a.ID, cond)
	require.NoError(t, err)
	setPhaseForTest(t, s, a.ID, string(state.PhaseStopped))

	srv.agentIdleSuspendHandler()(ctx)
	srv.waitAutoSuspendIdle()
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "an active lease is not recovered")

	expireIdleClaim(t, s, a.ID)
	srv.agentIdleSuspendHandler()(ctx)
	srv.waitAutoSuspendIdle()
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
	assert.Nil(t, got.IdleSuspendClaimUntil)
}

func TestIdleSuspendRecovery_StopDispatchOutcomes(t *testing.T) {
	cc := func(sentinel error, msg string) error {
		return fmt.Errorf("control channel request failed: %w", &brokerTransportError{msg: msg, sentinel: sentinel})
	}
	ownerTimeout := fmt.Errorf("dispatch stop: %w", cc(errBrokerResponseTimeout, "request timeout after 2m0s"))
	status := func(code int) error { return &brokerStatusError{StatusCode: code} }

	// Everything but the allowlist is an unknown outcome: the claim is kept and
	// the intent is not reverted.
	unknown := map[string]error{
		"http deadline":           fmt.Errorf("dispatch: %w", context.DeadlineExceeded),
		"context canceled":        fmt.Errorf("dispatch: %w", context.Canceled),
		"http transport timeout":  fmt.Errorf("failed to send request: %w", &netTimeoutErr{}),
		"connection reset":        fmt.Errorf("failed to send request: %w", errors.New("read tcp: connection reset by peer")),
		"control channel timeout": cc(errBrokerResponseTimeout, "request timeout after 2m0s"),
		"control channel lost":    cc(errBrokerConnectionLost, "connection closed"),
		"cross-node wait expired": fmt.Errorf("stop: %w", ErrDispatchFailed),
		"cross-node flattened owner timeout": dispatchFailureError(&store.BrokerDispatch{
			Op: "stop", Error: ownerTimeout.Error(), Result: dispatchFailureResult(ownerTimeout)}),
		"error phase during stop": fmt.Errorf("agent entered error phase during %s", "stop"),
		"broker 500":              status(http.StatusInternalServerError),
		"broker 404":              status(http.StatusNotFound),
		"broker 409":              status(http.StatusConflict),
		"pre-send endpoint error": errors.New("broker endpoint lookup failed"),
	}
	// The allowlist (stopDefinitelyRefused): the stop provably did nothing.
	definite := map[string]error{
		"request not sent":     fmt.Errorf("failed to sign request: %w (%w)", errors.New("sign"), errStartRequestNotSent),
		"broker not connected": fmt.Errorf("stop: %w", errStartBrokerNotConnected),
		"dial failure":         fmt.Errorf("failed to send request: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}),
		"run mismatch":         fmt.Errorf("%w (requested run r1)", ErrStopRunNotFound),
		"broker 400":           status(http.StatusBadRequest),
		"broker 401":           status(http.StatusUnauthorized),
		"broker 403":           status(http.StatusForbidden),
	}

	for name, stopErr := range unknown {
		t.Run("unknown/"+name, func(t *testing.T) {
			assert.False(t, stopDefinitelyRefused(stopErr))
			f := newIdleRecoveryFixture(t, "rec-unknown-"+tidSlugSafe(name), false)
			f.disp.runIntentDispatcher.stopErr = stopErr

			f.suspendOnce(t)

			got := f.get(t)
			assert.Equal(t, store.RunIntentStopped, got.RunIntent, "an unknown outcome does not revert the intent")
			require.NotNil(t, got.IdleSuspendClaimUntil)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)

			// The row still runs after the lease: recovery does not touch it.
			expireIdleClaim(t, f.raw, f.agent.ID)
			f.sweep()
			assert.Equal(t, string(state.PhaseRunning), f.get(t).Phase)

			// The stop did land after all: the container reports stopped.
			setPhaseForTest(t, f.raw, f.agent.ID, string(state.PhaseStopped))
			f.sweep()
			got = f.get(t)
			assert.Equal(t, string(state.PhaseSuspended), got.Phase)
			assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
			assert.Nil(t, got.IdleSuspendClaimUntil)
		})
	}

	for name, stopErr := range definite {
		t.Run("definite/"+name, func(t *testing.T) {
			assert.True(t, stopDefinitelyRefused(stopErr))
			f := newIdleRecoveryFixture(t, "rec-refused-"+tidSlugSafe(name), false)
			f.disp.runIntentDispatcher.stopErr = stopErr

			f.suspendOnce(t)

			got := f.get(t)
			assert.Equal(t, store.RunIntentRunning, got.RunIntent)
			assert.Nil(t, got.IdleSuspendClaimUntil)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
		})
	}
}

// TestIdleSuspendRecovery_UnknownStopThatNeverLandedConverges: the stop error
// is unknown and the container keeps running with intent=stopped. The
// intent-stop backstop (a system writer that preserves the claim) stops it,
// the sweep records suspended(idle), and a DM then auto-wakes the agent.
func TestIdleSuspendRecovery_UnknownStopThatNeverLandedConverges(t *testing.T) {
	ctx := context.Background()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	disp := &stopLandingWakeDispatcher{}
	srv.SetDispatcher(disp)
	t.Cleanup(srv.waitAutoWakeIdle)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	target, cond := claimIdleForTest(t, s, target, "run-never-landed")
	_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
	require.NoError(t, err)
	expireIdleClaim(t, s, target.ID)
	running, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseRunning), running.Phase)
	require.NotNil(t, running.RunIntentAt)

	// The backstop's stop (here: the broker stops the container and it reports
	// stopped) runs under a stop claim and keeps the idle stamp.
	disp.onStop = func(a *store.Agent) { setPhaseForTest(t, s, a.ID, string(state.PhaseStopped)) }
	srv.stopForStoppedIntent(ctx, *running)
	stopped, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseStopped), stopped.Phase)
	require.NotNil(t, stopped.IdleSuspendClaimUntil, "the backstop preserves the idle claim")

	srv.agentIdleSuspendHandler()(ctx)
	suspended, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseSuspended), suspended.Phase)
	require.Equal(t, string(state.SuspendReasonIdle), suspended.SuspendReason)

	awReadyOnStart(t, s, target.ID)
	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, suspended, "wakes after the backstop"))
	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	require.Len(t, disp.getStartCalls(), 1)
	require.Len(t, disp.getMessageCalls(), 1)
}

type netTimeoutErr struct{}

func (*netTimeoutErr) Error() string   { return "i/o timeout" }
func (*netTimeoutErr) Timeout() bool   { return true }
func (*netTimeoutErr) Temporary() bool { return true }

func TestIdleSuspendRecovery_UserStopInWindowIsNeverConverted(t *testing.T) {
	fastIdleCommitRetry(t)
	f := newIdleRecoveryFixture(t, "rec-userstop", true)
	f.faulty.failures.Store(1 << 20)
	f.suspendOnce(t)
	require.NotNil(t, f.get(t).IdleSuspendClaimUntil)

	// A user stop is a hub-authored lifecycle write: it clears the claim.
	require.NoError(t, f.raw.UpdateAgentStatus(context.Background(), f.agent.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseStopped), ContainerStatus: "stopped",
	}))
	got := f.get(t)
	require.Nil(t, got.IdleSuspendClaimUntil)

	f.faulty.failures.Store(0)
	f.sweep()
	got = f.get(t)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Empty(t, got.SuspendReason)
}

// strandIdleClaim leaves f's agent stopped with an expired claim: the commit
// write failed for good and the lease ran out.
func strandIdleClaim(t *testing.T, f *idleRecoveryFixture) {
	t.Helper()
	fastIdleCommitRetry(t)
	f.faulty.failures.Store(1 << 20)
	f.suspendOnce(t)
	expireIdleClaim(t, f.raw, f.agent.ID)
	f.faulty.failures.Store(0)
}

// TestIdleSuspendRecovery_Gates: recovery needs the experiment, and nothing
// else. With auto_suspend_idle turned off mid-claim it still finishes the
// in-flight suspend; with the experiment off it does nothing.
func TestIdleSuspendRecovery_Gates(t *testing.T) {
	t.Run("experiment off does nothing", func(t *testing.T) {
		f := newIdleRecoveryFixture(t, "rec-gate-exp", true)
		strandIdleClaim(t, f)
		setIdleSuspendExperiment(t, f.srv, false)

		f.srv.agentIdleSuspendHandler()(context.Background())
		f.srv.recoverIdleSuspendClaims(context.Background())

		got := f.get(t)
		assert.Equal(t, string(state.PhaseStopped), got.Phase)
		assert.Empty(t, got.SuspendReason)
		assert.NotNil(t, got.IdleSuspendClaimUntil)
	})

	t.Run("auto_suspend_idle off still settles the claim", func(t *testing.T) {
		f := newIdleRecoveryFixture(t, "rec-gate-setting", true)
		strandIdleClaim(t, f)
		f.srv.mu.Lock()
		f.srv.config.AutoSuspendIdle = false
		f.srv.mu.Unlock()

		f.srv.agentIdleSuspendHandler()(context.Background())

		got := f.get(t)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
		assert.Nil(t, got.IdleSuspendClaimUntil)
	})
}

// TestIdleSuspendRecovery_SettingOffThenDMAutoWakes: a claim left in flight
// when auto_suspend_idle is turned off is settled by recovery, and a DM then
// auto-wakes the agent and delivers.
func TestIdleSuspendRecovery_SettingOffThenDMAutoWakes(t *testing.T) {
	ctx := context.Background()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	t.Cleanup(srv.waitAutoWakeIdle)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	target, cond := claimIdleForTest(t, s, target, "run-setting-off")
	_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
	require.NoError(t, err)
	setPhaseForTest(t, s, target.ID, string(state.PhaseStopped))
	expireIdleClaim(t, s, target.ID)
	srv.mu.Lock()
	srv.config.AutoSuspendIdle = false
	srv.mu.Unlock()

	srv.agentIdleSuspendHandler()(ctx)

	suspended, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseSuspended), suspended.Phase)
	require.Equal(t, string(state.SuspendReasonIdle), suspended.SuspendReason)

	awReadyOnStart(t, s, target.ID)
	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, suspended, "wakes after setting off"))
	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	require.Len(t, disp.getStartCalls(), 1)
	require.Len(t, disp.getMessageCalls(), 1)
}

// TestIdleSuspendRecovery_DeliveryWaitRecoversOnDemand: a delivery that finds
// an expired claim on a row that already reports stopped records the
// suspension itself instead of waiting for the next sweep tick.
func TestIdleSuspendRecovery_DeliveryWaitRecoversOnDemand(t *testing.T) {
	ctx := context.Background()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	t.Cleanup(srv.waitAutoWakeIdle)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	target, cond := claimIdleForTest(t, s, target, "run-on-demand")
	_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
	require.NoError(t, err)
	setPhaseForTest(t, s, target.ID, string(state.PhaseStopped))
	expireIdleClaim(t, s, target.ID)
	claimed, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.True(t, claimed.IdleSuspendClaimActive)

	awReadyOnStart(t, s, target.ID)
	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, claimed, "no sweep tick needed"))
	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	require.Len(t, disp.getStartCalls(), 1)
	msgs := disp.getMessageCalls()
	require.Len(t, msgs, 1)
	assert.Equal(t, "no sweep tick needed", msgs[0].Message)
}

// TestIdleSuspendRecovery_AmbiguousCommitWithMembershipHold: a commit that took
// effect but returned an error is recorded on retry even when a membership
// hold made the stored reason NULL, so the post-record steps run (the claim
// is cleared) and the held agent never carries suspend_reason=idle.
func TestIdleSuspendRecovery_AmbiguousCommitWithMembershipHold(t *testing.T) {
	ctx := context.Background()
	f := newIdleRecoveryFixture(t, "rec-ambig-hold", true)
	_, err := f.raw.(*entadapter.CompositeStore).CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID: f.agent.ID, ProjectID: f.agent.ProjectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: "11111111-1111-1111-1111-111111111111",
		Trigger: store.MembershipLossTriggerMemberRemove,
	}})
	require.NoError(t, err)
	fastIdleCommitRetry(t)
	f.faulty.ambiguous.Store(true)

	f.suspendOnce(t)

	got := f.get(t)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	assert.Empty(t, got.SuspendReason)
	assert.Nil(t, got.IdleSuspendClaimUntil, "recorded: the post-record steps ran and cleared the claim")
}

// TestIdleSuspendRecovery_DMNotDispatchedWhileSuspendUnsettled covers the
// delivery helper: a surviving claim with run_intent=stopped is an idle
// suspend in progress even after the lease expired. A DM is not dispatched
// into a container whose stop may have landed; once recovery records
// suspended(idle) it auto-wakes the agent and delivers.
func TestIdleSuspendRecovery_DMNotDispatchedWhileSuspendUnsettled(t *testing.T) {
	ctx := context.Background()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	t.Cleanup(srv.waitAutoWakeIdle)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	target, cond := claimIdleForTest(t, s, target, "run-dm-unsettled")
	_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
	require.NoError(t, err)
	expireIdleClaim(t, s, target.ID)

	claimed, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.False(t, claimed.IdleSuspendClaimLeaseActive)
	require.True(t, claimed.IdleSuspendClaimActive, "an expired claim with a stopped intent is still in progress")
	require.Equal(t, string(state.PhaseRunning), claimed.Phase)

	prev := idleSuspendSettleTimeoutOverride
	idleSuspendSettleTimeoutOverride = 50 * time.Millisecond
	t.Cleanup(func() { idleSuspendSettleTimeoutOverride = prev })

	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, claimed, "must not reach the container"))
	assert.Nil(t, res)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "is suspending")
	assert.Empty(t, disp.getMessageCalls(), "nothing is dispatched to the container")
	assert.Empty(t, disp.getStartCalls())

	// The stop lands, recovery records suspended(idle), and the next DM wakes
	// the agent and delivers.
	setPhaseForTest(t, s, target.ID, string(state.PhaseStopped))
	srv.recoverIdleSuspendClaims(ctx)
	suspended, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseSuspended), suspended.Phase)
	require.Equal(t, string(state.SuspendReasonIdle), suspended.SuspendReason)

	awReadyOnStart(t, s, target.ID)
	res, dmErr = srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, suspended, "delivered after recovery"))
	require.Nil(t, dmErr)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	require.Len(t, disp.getStartCalls(), 1)
	msgs := disp.getMessageCalls()
	require.Len(t, msgs, 1)
	assert.Equal(t, "delivered after recovery", msgs[0].Message)
}

// stopLandingWakeDispatcher is a wakeTrackingDispatcher whose stop runs onStop.
type stopLandingWakeDispatcher struct {
	wakeTrackingDispatcher
	onStop func(a *store.Agent)
}

func (d *stopLandingWakeDispatcher) DispatchAgentStop(_ context.Context, a *store.Agent) error {
	if d.onStop != nil {
		d.onStop(a)
	}
	return nil
}

// TestIdleSuspendRecovery_StopAllWinsOverExpiredInProgressRow: a user stop-all
// after the lease expired clears the claim of a row whose idle suspend never
// recorded, so recovery does not convert it to suspended(idle).
func TestIdleSuspendRecovery_StopAllWinsOverExpiredInProgressRow(t *testing.T) {
	ctx := context.Background()
	f := newIdleRecoveryFixture(t, "rec-stopall", true)
	strandIdleClaim(t, f)
	require.True(t, f.get(t).IdleSuspendClaimActive)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.agent.ProjectID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := f.get(t)
	assert.Nil(t, got.IdleSuspendClaimUntil, "stop-all clears the claim of an in-progress row")

	f.srv.recoverIdleSuspendClaims(ctx)
	got = f.get(t)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Empty(t, got.SuspendReason)
}

// TestIdleSuspendRecovery_ExperimentOffIsInert: with lifecycle.idle_suspend off
// a stranded stamp is no claim at all (no claim checks): an explicit --wake
// resumes and delivers instead of waiting out the lease and failing.
func TestIdleSuspendRecovery_ExperimentOffIsInert(t *testing.T) {
	ctx := context.Background()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	t.Cleanup(srv.waitAutoWakeIdle)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	target, cond := claimIdleForTest(t, s, target, "run-exp-off")
	_, _, _, err := s.ClaimIdleSuspend(ctx, target.ID, cond)
	require.NoError(t, err)
	setPhaseForTest(t, s, target.ID, string(state.PhaseStopped))
	expireIdleClaim(t, s, target.ID)
	setIdleSuspendExperiment(t, srv, false)

	stranded, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.False(t, stranded.IdleSuspendClaimActive, "no in-progress claim with the experiment off")
	assert.NotNil(t, stranded.IdleSuspendClaimUntil)

	prev := idleSuspendSettleTimeoutOverride
	idleSuspendSettleTimeoutOverride = 100 * time.Millisecond
	t.Cleanup(func() { idleSuspendSettleTimeoutOverride = prev })

	awReadyOnStart(t, s, target.ID)
	in := awAgentDMInput(sender, stranded, "explicit wake with the experiment off")
	in.Wake = true
	res, dmErr := srv.ExecuteAgentDM(ctx, in)
	if dmErr != nil {
		assert.NotEqual(t, http.StatusServiceUnavailable, dmErr.HTTPStatus, "no suspending wait: %s", dmErr.Message)
	} else {
		assert.Equal(t, AgentDMAccepted, res.Outcome)
	}
}
