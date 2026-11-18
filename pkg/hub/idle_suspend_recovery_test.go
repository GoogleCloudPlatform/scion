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
}

func (c *commitFaultStore) UpdateAgentStatus(ctx context.Context, id string, upd store.AgentStatusUpdate) error {
	if upd.IfIdleSuspendCommit != nil {
		c.attempts.Add(1)
		if c.failures.Add(-1) >= 0 {
			return errors.New("injected commit write failure")
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
	f.faulty = &commitFaultStore{Store: srv.store}
	srv.store = f.faulty
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
	t.Run("a timeout keeps the intent and the claim, and recovery settles it", func(t *testing.T) {
		for name, stopErr := range map[string]error{
			"deadline":  fmt.Errorf("dispatch: %w", context.DeadlineExceeded),
			"transport": fmt.Errorf("failed to send request: %w", &netTimeoutErr{}),
		} {
			t.Run(name, func(t *testing.T) {
				f := newIdleRecoveryFixture(t, "rec-timeout-"+name, false)
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
	})

	t.Run("a definite refusal reverts the intent and clears the claim", func(t *testing.T) {
		f := newIdleRecoveryFixture(t, "rec-refused", false)
		f.disp.runIntentDispatcher.stopErr = &brokerStatusError{StatusCode: http.StatusInternalServerError}

		f.suspendOnce(t)

		got := f.get(t)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent)
		assert.Nil(t, got.IdleSuspendClaimUntil)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
	})
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

func TestIdleSuspendRecovery_GatesOff(t *testing.T) {
	for name, off := range map[string]func(t *testing.T, srv *Server){
		"experiment off": func(t *testing.T, srv *Server) { setIdleSuspendExperiment(t, srv, false) },
		"setting off": func(_ *testing.T, srv *Server) {
			srv.mu.Lock()
			srv.config.AutoSuspendIdle = false
			srv.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			fastIdleCommitRetry(t)
			f := newIdleRecoveryFixture(t, "rec-gate-"+tidSlugSafe(name), true)
			f.faulty.failures.Store(1 << 20)
			f.suspendOnce(t)
			expireIdleClaim(t, f.raw, f.agent.ID)
			f.faulty.failures.Store(0)

			off(t, f.srv)
			f.srv.agentIdleSuspendHandler()(context.Background())
			f.srv.recoverIdleSuspendClaims(context.Background())

			got := f.get(t)
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
			assert.Empty(t, got.SuspendReason)
			assert.NotNil(t, got.IdleSuspendClaimUntil)
		})
	}
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
