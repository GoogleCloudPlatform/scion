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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalledRaceDispatcher models a broker whose container becomes unmessageable
// once DispatchAgentStop begins and becomes messageable again after
// DispatchAgentStart(continue=true) runs.
type stalledRaceDispatcher struct {
	wakeTrackingDispatcher

	mu              sync.Mutex
	stopEntered     chan struct{}
	stopEnteredOnce sync.Once
	releaseStop     chan struct{}
	stopErr         error
	stopPanic       bool
	containerDown   atomic.Bool
	msgErrOnce      error
	msgCalls        atomic.Int32
	onStopEntered   func()
}

func newStalledRaceDispatcher() *stalledRaceDispatcher {
	return &stalledRaceDispatcher{
		stopEntered: make(chan struct{}),
		releaseStop: make(chan struct{}),
	}
}

func (d *stalledRaceDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	d.containerDown.Store(true)
	if d.onStopEntered != nil {
		d.onStopEntered()
	}
	d.stopEnteredOnce.Do(func() { close(d.stopEntered) })
	select {
	case <-d.releaseStop:
	case <-ctx.Done():
		return ctx.Err()
	}
	if d.stopPanic {
		panic("simulated stop dispatcher panic")
	}
	return d.stopErr
}

func (d *stalledRaceDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, prompt string, cont bool) error {
	if err := d.wakeTrackingDispatcher.DispatchAgentStart(ctx, agent, prompt, cont); err != nil {
		return err
	}
	d.containerDown.Store(false)
	return nil
}

// brokerAgentNotFoundErr returns the real *brokerStatusError (HTTP 404 with
// error.code == "agent_not_found") that pkg/runtimebroker returns synchronously
// when DispatchAgentMessage targets a stopped or missing container.
func brokerAgentNotFoundErr() error {
	return &brokerStatusError{
		StatusCode: http.StatusNotFound,
		Body:       `{"error":{"code":"agent_not_found"}}`,
	}
}

func (d *stalledRaceDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.msgCalls.Add(1)
	d.mu.Lock()
	errOnce := d.msgErrOnce
	d.msgErrOnce = nil
	d.mu.Unlock()
	if errOnce != nil {
		return errOnce
	}
	if d.containerDown.Load() {
		return brokerAgentNotFoundErr()
	}
	return d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

// setupStalledDMFixtures creates a sender and a stalled running target agent
// on an online broker with RunID and RunIntentRunning set, ready for
// autoSuspendAgent(..., SuspendReasonStalled).
func setupStalledDMFixtures(t *testing.T, idleSuspendExp bool) (*Server, store.Store, *store.Agent, *store.Agent, *stalledRaceDispatcher) {
	t.Helper()
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
	setIdleSuspendExperiment(t, srv, idleSuspendExp)

	ctx := context.Background()
	_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
	require.NoError(t, err)
	db := s.(*entadapter.CompositeStore).DB()
	_, err = db.ExecContext(ctx,
		"UPDATE agents SET run_id = ?, activity = ?, last_seen = ? WHERE id = ?",
		"run-stalled-"+target.ID, string(state.ActivityStalled), time.Now().UTC(), target.ID,
	)
	require.NoError(t, err)
	fresh, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	*target = *fresh

	disp := newStalledRaceDispatcher()
	srv.SetDispatcher(disp)
	awReadyOnStart(t, s, target.ID)
	t.Cleanup(srv.waitAutoWakeIdle)
	return srv, s, sender, target, disp
}

// TestStalledAutoSuspend_DM_MidStopRunningPhase_WakesAndDelivers verifies
// Window A: a DM arrives while stalled auto-suspend is mid-DispatchAgentStop
// and the DB row still reports phase=running. On main before the fix, the DM
// skips wake and fails with "container ... is not running".
func TestStalledAutoSuspend_DM_MidStopRunningPhase_WakesAndDelivers(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

	// Start stalled auto-suspend in the background; it blocks in DispatchAgentStop.
	suspendDone := make(chan bool, 1)
	go func() {
		suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
	}()

	select {
	case <-disp.stopEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for DispatchAgentStop")
	}

	// Confirm the DB row still says phase=running while the container is down.
	mid, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseRunning), mid.Phase)

	// Release stop shortly after the DM begins so the DM interleaves with it.
	go func() {
		time.Sleep(350 * time.Millisecond)
		close(disp.releaseStop)
	}()

	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, mid, "hello during stalled suspend"))
	require.Nil(t, dmErr, "DM during stalled auto-suspend must settle, wake, and deliver")
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)

	require.True(t, <-suspendDone, "auto-suspend should have recorded suspended")
	assert.Len(t, disp.getStartCalls(), 1, "agent should be resumed exactly once")
	assert.Equal(t, int32(1), disp.msgCalls.Load(), "message should be dispatched once after resume")
	assert.Equal(t, string(state.PhaseRunning), awAgentPhase(t, s, target.ID))

	msg, err := s.GetMessage(context.Background(), res.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)
}

// TestStalledAutoSuspend_DM_StoppingPhase_WakesAndDelivers verifies Window B:
// a broker heartbeat sets phase=stopping during stalled auto-suspend before
// recordStopStatus writes suspended(stalled). On main before the fix, stopping
// is refused with 400/409 "not yet running (phase: stopping)".
func TestStalledAutoSuspend_DM_StoppingPhase_WakesAndDelivers(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

	suspendDone := make(chan bool, 1)
	go func() {
		suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
	}()

	select {
	case <-disp.stopEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for DispatchAgentStop")
	}

	// Simulate broker heartbeat reporting phase=stopping before suspend commits.
	require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseStopping),
	}))
	stoppingRow, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseStopping), stoppingRow.Phase)

	go func() {
		time.Sleep(350 * time.Millisecond)
		close(disp.releaseStop)
	}()

	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, stoppingRow, "hello while stopping"))
	require.Nil(t, dmErr, "DM while stopping for stalled auto-suspend must settle, wake, and deliver")
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)

	require.True(t, <-suspendDone)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.Equal(t, int32(1), disp.msgCalls.Load())
}

// TestStalledAutoSuspend_UserStopInStoppingPhase_FailsFastWithoutPolling
// verifies L-1: a plain user/creator stop stuck in phase=stopping with
// run_intent=stopped (no local hub auto-suspend tracker and no idle-suspend
// claim) is never settle-waited and fails immediately without polling.
func TestStalledAutoSuspend_UserStopInStoppingPhase_FailsFastWithoutPolling(t *testing.T) {
	for _, tc := range []struct {
		name           string
		idleSuspendExp bool
		wake           bool
	}{
		{name: "experiment on no wake", idleSuspendExp: true, wake: false},
		{name: "experiment off with wake", idleSuspendExp: false, wake: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, sender, target, disp := setupStalledDMFixtures(t, tc.idleSuspendExp)
			_, err := s.SetRunIntent(context.Background(), target.ID, store.RunIntentStopped)
			require.NoError(t, err)
			require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
				Phase: string(state.PhaseStopping),
			}))
			stoppingRow, err := s.GetAgent(context.Background(), target.ID)
			require.NoError(t, err)
			require.False(t, srv.isHubAutoSuspending(stoppingRow))

			input := awAgentDMInput(sender, stoppingRow, "dm to user-stopping agent")
			if tc.wake {
				input.Wake = true
				input.SenderIdentity = wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)
			}

			start := time.Now()
			res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
			elapsed := time.Since(start)

			assert.Nil(t, res)
			require.NotNil(t, dmErr)
			if tc.wake {
				assert.Equal(t, http.StatusBadRequest, dmErr.HTTPStatus)
			} else {
				assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
				assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
			}
			assert.Less(t, elapsed, 2*time.Second, "user stop in stopping phase must fail fast without settle-waiting")
			assert.Empty(t, disp.getStartCalls())
			assert.Equal(t, int32(0), disp.msgCalls.Load())
		})
	}
}

type markDeliveryCountingStore struct {
	store.Store
	fault *storeFaultSwitch
	calls atomic.Int32
}

func (w *markDeliveryCountingStore) MarkAgentDelivery(ctx context.Context, agentID string) (*store.Agent, bool, error) {
	if w.fault.Active() {
		w.calls.Add(1)
	}
	return w.Store.MarkAgentDelivery(ctx, agentID)
}

// TestWaitForIdleSuspendToSettle_BoundedExponentialBackoff verifies L-1:
// cross-replica idle-suspend settle polling uses bounded exponential backoff
// rather than flat 10Hz polling.
func TestWaitForIdleSuspendToSettle_BoundedExponentialBackoff(t *testing.T) {
	srv, s, counter, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *markDeliveryCountingStore {
		return &markDeliveryCountingStore{Store: inner, fault: f}
	})
	setIdleSuspendExperiment(t, srv, true)

	ctx := context.Background()
	project := &store.Project{ID: tid("backoff-proj"), Name: "Backoff", Slug: "backoff-proj"}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID:        tid("backoff-agent"),
		ProjectID: project.ID,
		Name:      "backoff-agent",
		Slug:      "backoff-agent",
		Phase:     string(state.PhaseRunning),
		RunID:     "run-backoff",
		RunIntent: store.RunIntentRunning,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	claimUntil := time.Now().UTC().Add(30 * time.Second)
	db := s.(*entadapter.CompositeStore).DB()
	_, err := db.ExecContext(ctx,
		"UPDATE agents SET idle_suspend_claim_until = ? WHERE id = ?",
		claimUntil, agent.ID,
	)
	require.NoError(t, err)
	claimed, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.True(t, claimed.IdleSuspendClaimActive)

	fault.Arm()
	shortCtx, cancel := context.WithTimeout(ctx, 850*time.Millisecond)
	defer cancel()
	err = srv.waitForIdleSuspendToSettle(shortCtx, claimed)
	require.ErrorIs(t, err, errIdleSuspendInProgressNotSettled)
	// With exponential backoff (100ms, 200ms, 400ms, 800ms), 850ms produces at
	// most 4-5 polls (initial + 3-4 ticks), vs 9-10 polls at flat 10Hz.
	calls := counter.calls.Load()
	assert.GreaterOrEqual(t, calls, int32(2), "must poll while claim is active")
	assert.LessOrEqual(t, calls, int32(5), "exponential backoff must avoid 10Hz store polling")
}

// TestStalledAutoSuspend_DM_StaleRunningSnapshotAfterStop_WakesOnce verifies
// the stale-snapshot path through markAgentForDispatch: the caller read
// phase=running before stalled auto-suspend finished, and by the time
// markAgentForDispatch runs the store row is suspended(stalled). The hub wakes
// once and delivers once.
func TestStalledAutoSuspend_DM_StaleRunningSnapshotAfterStop_WakesOnce(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

	// Capture a stale running snapshot as a caller would read before suspend.
	staleRunning := *target

	// Complete the stalled auto-suspend so the store row is suspended(stalled).
	close(disp.releaseStop)
	require.True(t, srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled))
	require.Equal(t, string(state.PhaseSuspended), awAgentPhase(t, s, target.ID))

	// ExecuteAgentDM with staleRunning enters dispatchMarkedAgentMessage ->
	// markAgentForDispatch sees suspended(stalled) -> wakes once and delivers once.
	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, &staleRunning, "hello after stalled suspend"))
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.Equal(t, int32(1), disp.msgCalls.Load())
}

// TestStalledAutoSuspend_DM_BrokerNotRunningError_ReChecksAndWakesOnce tests
// the synchronous broker error recovery path when DispatchAgentMessage itself
// returns the real broker 404 (*brokerStatusError agent_not_found) or the
// raw container-not-running string fallback (e.g. on a replica where
// MarkAgentDelivery ran just before the stop).
func TestStalledAutoSuspend_DM_BrokerNotRunningError_ReChecksAndWakesOnce(t *testing.T) {
	t.Run("broker 404 agent_not_found wakes once and delivers", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: disp,
			onFirstMsg: func() {
				awSuspendFor(t, s, target, state.SuspendReasonStalled)
			},
			firstErr: brokerAgentNotFoundErr(),
		}
		srv.SetDispatcher(transitioningDisp)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "hello racing broker"))
		require.Nil(t, dmErr, "broker 404 agent_not_found when target just stalled-suspended must wake and retry once")
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		assert.Len(t, disp.getStartCalls(), 1)
		assert.Equal(t, int32(2), transitioningDisp.calls.Load(), "one failed attempt + one retry after wake")

		msg, err := s.GetMessage(context.Background(), res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)
	})

	t.Run("raw container-not-running string fallback wakes once and delivers", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: disp,
			onFirstMsg: func() {
				awSuspendFor(t, s, target, state.SuspendReasonStalled)
			},
			firstErr: fmt.Errorf("Error response from daemon: container %s is not running", target.ID),
		}
		srv.SetDispatcher(transitioningDisp)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "hello raw container err"))
		require.Nil(t, dmErr, "raw container-not-running fallback when target just stalled-suspended must wake and retry once")
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		assert.Len(t, disp.getStartCalls(), 1)
		assert.Equal(t, int32(2), transitioningDisp.calls.Load())
	})
}

type stalledTransitionOnFirstMsgDispatcher struct {
	*stalledRaceDispatcher
	onFirstMsg func()
	firstErr   error
	calls      atomic.Int32
}

func (d *stalledTransitionOnFirstMsgDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if d.calls.Add(1) == 1 {
		if d.onFirstMsg != nil {
			d.onFirstMsg()
		}
		return d.firstErr
	}
	return d.stalledRaceDispatcher.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

// TestStalledAutoSuspend_DM_AuthzAndExclusions verifies Condition 2:
// authorization never widens; user suspends, user stops, unauthorized --wake,
// experiment-off without --wake, holds, and deletions are never woken.
func TestStalledAutoSuspend_DM_AuthzAndExclusions(t *testing.T) {
	t.Run("user suspend in stopping phase fails fast without auto-waking", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		_, err := s.SetRunIntent(context.Background(), target.ID, store.RunIntentStopped)
		require.NoError(t, err)
		require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseStopping),
		}))
		stopping, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, stopping, "do not wake user suspend"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls(), "user-suspended agent must never auto-wake")
		assert.Equal(t, int32(0), disp.msgCalls.Load())
	})

	t.Run("user suspend settling after tracker finishes does not auto-wake", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		mid, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)
		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		go func() {
			time.Sleep(350 * time.Millisecond)
			_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
				Phase:         string(state.PhaseSuspended),
				SuspendReason: string(state.SuspendReasonUser),
			})
			end()
		}()

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, mid, "settled to user suspend"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls(), "agent settled to SuspendReasonUser must never auto-wake")
		assert.Equal(t, int32(0), disp.msgCalls.Load())
	})

	t.Run("unauthorized --wake during stalled auto-suspend returns 403 without waking", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		defer func() {
			close(disp.releaseStop)
			<-suspendDone
		}()

		mid, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		input := awAgentDMInput(sender, mid, "unauthorized wake")
		input.Wake = true // sender lacks ScopeAgentLifecycle
		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeForbidden, dmErr.Code)
		assert.Equal(t, http.StatusForbidden, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls())
		assert.Equal(t, int32(0), disp.msgCalls.Load())
	})

	t.Run("authorized --wake during stalled auto-suspend with experiment OFF settles and wakes", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		mid, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		input := awAgentDMInput(sender, mid, "authorized explicit wake")
		input.SenderIdentity = wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)
		input.Wake = true
		go func() {
			time.Sleep(300 * time.Millisecond)
			close(disp.releaseStop)
		}()
		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		require.True(t, <-suspendDone)
		assert.Len(t, disp.getStartCalls(), 1)
		assert.Equal(t, int32(1), disp.msgCalls.Load())
	})

	t.Run("no --wake during stalled auto-suspend with experiment OFF refuses without waking", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		defer func() {
			close(disp.releaseStop)
			<-suspendDone
		}()

		mid, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, mid, "no wake experiment off"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Contains(t, dmErr.Message, "suspending")
		assert.Empty(t, disp.getStartCalls())
		assert.Equal(t, int32(0), disp.msgCalls.Load())
	})

	t.Run("held target during stalled auto-suspend is never woken", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		_, err := s.CreateAgentHolds(context.Background(), []*store.AgentHold{{
			AgentID:           target.ID,
			ProjectID:         target.ProjectID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   DevUserID,
			Trigger:           store.MembershipLossTriggerMemberRemove,
		}})
		require.NoError(t, err)

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		defer func() {
			close(disp.releaseStop)
			<-suspendDone
		}()

		mid, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, mid, "held target"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Empty(t, disp.getStartCalls())
		assert.Equal(t, int32(0), disp.msgCalls.Load())
	})

	t.Run("deleting target is never classified as auto-suspending or woken", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		defer end()

		db := s.(*entadapter.CompositeStore).DB()
		_, err := db.ExecContext(context.Background(),
			"UPDATE agents SET phase = ?, run_intent = ?, deletion_state = ?, deletion_lease_at = ? WHERE id = ?",
			string(state.PhaseStopping), string(store.RunIntentStopped), string(store.DeletionStateDeleting), time.Now().UTC().Add(time.Minute), target.ID,
		)
		require.NoError(t, err)
		deletingRow, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)
		assert.False(t, srv.isHubAutoSuspending(deletingRow), "deleting row must never be classified as auto-suspending")
		assert.Equal(t, autoWakeNone, srv.autoWakeModeOf(deletingRow), "deleting row must never enter auto-wake")

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, deletingRow, "deleting target"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Empty(t, disp.getStartCalls())
		assert.Equal(t, int32(0), disp.msgCalls.Load())
	})
}

// TestStalledAutoSuspend_DM_DeliveryTruthAndNoDoubleDelivery verifies Condition 3.
func TestStalledAutoSuspend_DM_DeliveryTruthAndNoDoubleDelivery(t *testing.T) {
	t.Run("broker accepted message is never retried", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "accepted once"))
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		assert.Equal(t, int32(1), disp.msgCalls.Load())
		assert.Empty(t, disp.getStartCalls())

		msg, err := s.GetMessage(context.Background(), res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)
	})

	t.Run("non-container-stopped broker error does not wake or retry", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: disp,
			onFirstMsg: func() {
				awSuspendFor(t, s, target, state.SuspendReasonStalled)
			},
			firstErr: errors.New("broker HTTP 500 internal error"),
		}
		srv.SetDispatcher(transitioningDisp)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "500 error"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, int32(1), transitioningDisp.calls.Load(), "ambiguous/500 broker error must never retry")
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("bounded settle timeout fails honestly with 503", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		orig := idleSuspendSettleTimeoutOverride
		idleSuspendSettleTimeoutOverride = 150 * time.Millisecond
		t.Cleanup(func() { idleSuspendSettleTimeoutOverride = orig })

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		defer func() {
			close(disp.releaseStop)
			<-suspendDone
		}()

		mid, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, mid, "timeout during suspend"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
		assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
		assert.Contains(t, dmErr.Message, "suspending")
		assert.Empty(t, disp.getStartCalls())
		assert.Equal(t, int32(0), disp.msgCalls.Load())
	})
}

// TestStalledAutoSuspend_InFlightCleanupOnErrorAndPanic verifies Condition 4:
// no tracker leak if DispatchAgentStop fails or panics — a subsequent DM
// delivers cleanly to the still-running agent.
func TestStalledAutoSuspend_InFlightCleanupOnErrorAndPanic(t *testing.T) {
	t.Run("stop error clears in-flight state", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		disp.stopErr = errors.New("broker refused stop")
		close(disp.releaseStop)

		ok := srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		assert.False(t, ok)
		assert.False(t, srv.autoSuspending.isActive(target.ID))
		disp.containerDown.Store(false)

		fresh, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, fresh, "after stop error"))
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
	})

	t.Run("stop panic clears in-flight state", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		disp.stopPanic = true
		close(disp.releaseStop)

		assert.Panics(t, func() {
			srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		})
		assert.False(t, srv.autoSuspending.isActive(target.ID))
		disp.containerDown.Store(false)

		fresh, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, fresh, "after stop panic"))
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
	})
}

// TestIdleAutoSuspend_DM_MidStop_StillWakesAndDelivers verifies Condition 1:
// the idle auto-suspend case stays green alongside the stalled fix.
func TestIdleAutoSuspend_DM_MidStop_StillWakesAndDelivers(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
	enableIdleSweepForTest(t, srv, 15*time.Minute)

	pastGrace := time.Now().UTC().Add(-20 * time.Minute)
	db := s.(*entadapter.CompositeStore).DB()
	_, err := db.ExecContext(context.Background(),
		"UPDATE agents SET activity = ?, activity_since = ?, last_seen = ? WHERE id = ?",
		string(state.ActivityIdle), pastGrace, time.Now().UTC(), target.ID,
	)
	require.NoError(t, err)
	fresh, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	*target = *fresh

	suspendDone := make(chan bool, 1)
	go func() {
		suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonIdle)
	}()
	select {
	case <-disp.stopEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for idle DispatchAgentStop")
	}
	mid, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	go func() {
		time.Sleep(350 * time.Millisecond)
		close(disp.releaseStop)
	}()
	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, mid, "hello during idle suspend"))
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)

	require.True(t, <-suspendDone)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.Equal(t, int32(1), disp.msgCalls.Load())
}

// TestStalledAutoSuspend_GroupMemberNeverWakes verifies that group-message
// no-wake behaviour is unchanged during a stalled auto-suspend.
func TestStalledAutoSuspend_GroupMemberNeverWakes(t *testing.T) {
	srv, _, _, target, disp := setupStalledDMFixtures(t, true)

	suspendDone := make(chan bool, 1)
	go func() {
		suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
	}()
	select {
	case <-disp.stopEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for DispatchAgentStop")
	}
	defer func() {
		close(disp.releaseStop)
		<-suspendDone
	}()

	err := srv.groupMemberDeliverableError(target)
	require.NotNil(t, err)
	assert.Equal(t, ErrCodeAgentNotRunning, err.Code)
	assert.Contains(t, err.Message, "group messages do not wake agents")
	assert.Empty(t, disp.getStartCalls())
}

// TestStalledAutoSuspend_DM_PostAdmissionExplicitWake_AuthzEnforced verifies
// R1 / M-1: when a DM with --wake is admitted while the target is still running
// (experiment OFF) and the target suspends during dispatch, syncWakeForDM
// enforces wakeResumeDenial (403 without lifecycle permission, wake+deliver
// with permission) and refreshes the target from the store.
func TestStalledAutoSuspend_DM_PostAdmissionExplicitWake_AuthzEnforced(t *testing.T) {
	t.Run("unauthorized --wake after admission returns 403 and does not start", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF
		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: disp,
			onFirstMsg: func() {
				awSuspendFor(t, s, target, state.SuspendReasonStalled)
			},
			firstErr: brokerAgentNotFoundErr(),
		}
		srv.SetDispatcher(transitioningDisp)

		input := awAgentDMInput(sender, target, "post-admission unauthorized wake")
		input.Wake = true // sender lacks ScopeAgentLifecycle
		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeForbidden, dmErr.Code)
		assert.Equal(t, http.StatusForbidden, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls(), "unauthorized --wake must never resume target")
		assert.Equal(t, int32(1), transitioningDisp.calls.Load())

		msgs, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.NotEmpty(t, msgs.Items)
		assert.Equal(t, store.MessageDispatchFailed, msgs.Items[0].DispatchState)
	})

	t.Run("authorized --wake after admission wakes once and retries once", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF
		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: disp,
			onFirstMsg: func() {
				awSuspendFor(t, s, target, state.SuspendReasonStalled)
			},
			firstErr: brokerAgentNotFoundErr(),
		}
		srv.SetDispatcher(transitioningDisp)

		input := awAgentDMInput(sender, target, "post-admission authorized wake")
		input.Wake = true
		input.SenderIdentity = wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, AgentDMAccepted, res.Outcome)
		assert.Len(t, disp.getStartCalls(), 1)
		assert.Equal(t, int32(2), transitioningDisp.calls.Load())

		msg, err := s.GetMessage(context.Background(), res.MessageID)
		require.NoError(t, err)
		assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)
	})

	t.Run("syncWakeForDM re-reads stale running snapshot from store", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF
		staleRunning := *target
		awSuspendFor(t, s, target, state.SuspendReasonStalled)

		err := srv.syncWakeForDM(context.Background(), &staleRunning, true, wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle))
		require.NoError(t, err)
		assert.Len(t, disp.getStartCalls(), 1)
	})
}

// TestStalledAutoSuspend_ChatV2_PostAdmissionExplicitWake_AuthzEnforced
// verifies R1 / M-2: in chat v2 sendAgentRouted, when a send with wake=true is
// admitted while the primary is running (experiment OFF) and the primary
// suspends during dispatch, canExplicitWake enforces agentLifecycleAllowed.
func TestStalledAutoSuspend_ChatV2_PostAdmissionExplicitWake_AuthzEnforced(t *testing.T) {
	setupChatFixture := func(t *testing.T) (*Server, store.Store, *store.Project, string, *store.Agent, *stalledRaceDispatcher, *stalledTransitionOnFirstMsgDispatcher) {
		t.Helper()
		srv, s, wcs, proj, db := setupSendTest(t)
		setIdleSuspendExperiment(t, srv, false)
		ctx := t.Context()

		broker := &store.RuntimeBroker{
			ID: tid("chat-stalled-broker"), Name: "Chat Stalled Broker",
			Slug: "chat-stalled-broker", Status: store.BrokerStatusOnline,
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name,
			Status: store.BrokerStatusOnline,
		}))

		target := &store.Agent{
			ID: tid("chat-stalled-agent"), ProjectID: proj.ID, Name: "StalledTarget",
			Slug: "chat-stalled-agent", Phase: string(state.PhaseRunning),
			RuntimeBrokerID: broker.ID, OwnerID: DevUserID, CreatedBy: DevUserID,
			MessageMode: store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, target))
		awReadyOnStart(t, s, target.ID)

		topicID := tid("chat-stalled-topic")
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: topicID, ProjectID: proj.ID, Name: "chat-stalled-topic",
			CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: target.Slug,
		}))
		setTopicConversationID(t, db, s, topicID, proj.ID)

		baseDisp := newStalledRaceDispatcher()
		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: baseDisp,
			onFirstMsg: func() {
				awSuspendFor(t, s, target, state.SuspendReasonStalled)
			},
			firstErr: brokerAgentNotFoundErr(),
		}
		srv.SetDispatcher(transitioningDisp)
		return srv, s, proj, topicID, target, baseDisp, transitioningDisp
	}

	t.Run("user without lifecycle permission cannot wake mid-dispatch", func(t *testing.T) {
		srv, s, proj, topicID, target, baseDisp, transitioningDisp := setupChatFixture(t)
		ctx := t.Context()
		srv.seedProjectCreatorMembership(ctx, proj)
		member := &store.User{
			ID: tid("chat-stalled-member"), Email: "chat-stalled-member@example.com",
			DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, member))
		msgAuthzAddProjectMember(t, s, member.ID, proj.ID, proj.Slug, store.GroupMemberRoleMember)
		msgAuthzGrantAgentMessage(t, s, member.ID, proj.ID)
		require.False(t, srv.agentLifecycleAllowed(ctx, userIdentityFor(member), target))

		rec := doRequestAsUser(t, srv, member, http.MethodPost,
			"/api/v1/chat/conversations/"+topicID+"/messages",
			map[string]any{"content": "wake without perm", "wake": true},
		)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.MessageDispatchFailed, resp["dispatchState"])
		assert.Empty(t, baseDisp.getStartCalls(), "user without lifecycle permission must not wake target")
		assert.Equal(t, int32(1), transitioningDisp.calls.Load())
	})

	t.Run("user with lifecycle permission wakes once and delivers", func(t *testing.T) {
		srv, _, _, topicID, _, baseDisp, transitioningDisp := setupChatFixture(t)
		rec := doRequest(t, srv, http.MethodPost,
			"/api/v1/chat/conversations/"+topicID+"/messages",
			map[string]any{"content": "wake with perm", "wake": true},
		)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.MessageDispatchDispatched, resp["dispatchState"])
		assert.Len(t, baseDisp.getStartCalls(), 1)
		assert.Equal(t, int32(2), transitioningDisp.calls.Load())
	})
}

// TestStalledAutoSuspend_MarkerTrackerShortCircuit_SkipsBrokerBeforeWake
// verifies R1 / L-3: when the local autoSuspending tracker is active at
// markAgentForDispatch, dispatchWithBrokerRetry short-circuits before calling
// DispatchAgentMessage, settles and wakes, and calls the broker only once.
func TestStalledAutoSuspend_MarkerTrackerShortCircuit_SkipsBrokerBeforeWake(t *testing.T) {
	srv, s, _, target, disp := setupStalledDMFixtures(t, true)

	suspendDone := make(chan bool, 1)
	go func() {
		suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
	}()
	select {
	case <-disp.stopEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for DispatchAgentStop")
	}
	runningSnap, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseRunning), runningSnap.Phase)
	go func() {
		time.Sleep(350 * time.Millisecond)
		close(disp.releaseStop)
	}()

	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = dispatchWithBrokerRetry(
		dctx,
		srv.deliveryMarker(),
		nil,
		disp,
		runningSnap,
		"marker short-circuit",
		false,
		messages.NewNotification("user:dev", "agent:"+target.Slug, "marker short-circuit", messages.TypeInstruction),
		withSyncAutoWakeHook(func(_ context.Context, a *store.Agent) (context.Context, context.CancelFunc, error) {
			wctx, wcancel := dispatchContext(context.Background(), scheduledWakeBudget)
			defer wcancel()
			if werr := srv.syncWakeForDM(wctx, a, false, nil); werr != nil {
				return nil, nil, werr
			}
			nctx, ncancel := context.WithTimeout(context.Background(), 10*time.Second)
			return nctx, ncancel, nil
		}),
	)
	require.NoError(t, err)
	require.True(t, <-suspendDone)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.Equal(t, int32(1), disp.msgCalls.Load(), "tracker short-circuit must prevent any pre-wake broker attempt")
}

// TestStalledAutoSuspend_AbortedSuspendAfterMark_RetriesAndNeverLeaksSentinel
// verifies N4: if the local auto-suspend tracker is active at
// markAgentForDispatch and then aborts before syncWakeForDM runs (leaving the
// agent running), dispatchWithBrokerRetry retries once and delivers cleanly,
// and never surfaces the internal errNoWakeRecovery sentinel.
func TestStalledAutoSuspend_AbortedSuspendAfterMark_RetriesAndNeverLeaksSentinel(t *testing.T) {
	t.Run("aborted auto-suspend delivers once without waking", func(t *testing.T) {
		srv, _, _, target, disp := setupStalledDMFixtures(t, true)
		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		defer end()

		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := dispatchWithBrokerRetry(
			dctx,
			srv.deliveryMarker(),
			nil,
			disp,
			target,
			"aborted auto-suspend",
			false,
			messages.NewNotification("user:dev", "agent:"+target.Slug, "aborted auto-suspend", messages.TypeInstruction),
			withSyncAutoWakeHook(func(_ context.Context, a *store.Agent) (context.Context, context.CancelFunc, error) {
				// Simulate auto-suspend aborting right after markAgentForDispatch.
				end()
				return nil, nil, srv.syncWakeForDM(context.Background(), a, false, nil)
			}),
		)
		require.NoError(t, err)
		assert.Empty(t, disp.getStartCalls(), "aborted auto-suspend needs no wake")
		assert.Equal(t, int32(1), disp.msgCalls.Load(), "message delivered once after retry")
	})

	t.Run("sentinel text never surfaces even if tracker stays active on retry", func(t *testing.T) {
		srv, _, _, target, disp := setupStalledDMFixtures(t, true)
		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		defer end()

		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := dispatchWithBrokerRetry(
			dctx,
			srv.deliveryMarker(),
			nil,
			disp,
			target,
			"still suspending",
			false,
			messages.NewNotification("user:dev", "agent:"+target.Slug, "still suspending", messages.TypeInstruction),
			withSyncAutoWakeHook(func(_ context.Context, a *store.Agent) (context.Context, context.CancelFunc, error) {
				return nil, nil, errNoWakeRecovery
			}),
		)
		require.Error(t, err)
		assert.False(t, errors.Is(err, errNoWakeRecovery), "internal sentinel must never be returned")
		assert.NotContains(t, err.Error(), "no wake recovery applicable")
		assert.Contains(t, err.Error(), "suspended")
	})
}

// TestStalledAutoSuspend_WakeOrAwaitStart_HoldPlacedDuringSettle_BlocksStartingTarget
// verifies R1 (post-settle hold guard in wakeOrAwaitStart): if a hold is
// created during the settle window and the target settles into phase=starting
// (which would otherwise enter awaitStartInProgress without calling startGate),
// wakeOrAwaitStart's post-settle heldTargetRefusal rejects it before awaiting.
func TestStalledAutoSuspend_WakeOrAwaitStart_HoldPlacedDuringSettle_BlocksStartingTarget(t *testing.T) {
	srv, s, _, target, disp := setupStalledDMFixtures(t, true)
	end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)

	mid, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	go func() {
		time.Sleep(350 * time.Millisecond)
		_, _ = s.CreateAgentHolds(context.Background(), []*store.AgentHold{{
			AgentID:           target.ID,
			ProjectID:         target.ProjectID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   DevUserID,
			Trigger:           store.MembershipLossTriggerMemberRemove,
		}})
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase:         string(state.PhaseStarting),
			SuspendReason: string(state.SuspendReasonStalled),
		})
		end()
	}()
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dmErr, awaitErr := srv.wakeOrAwaitStart(wctx, mid)
	require.NotNil(t, dmErr, "post-settle hold must refuse even when target settled to starting")
	assert.Nil(t, awaitErr)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Contains(t, strings.ToLower(dmErr.Message), "hold")
	assert.Empty(t, disp.getStartCalls())
}

// TestStalledAutoSuspend_HandleAgentMessage_ExperimentOffNoWake_Refuses409
// verifies R1 (handleAgentMessage isHubAutoSuspending check): with the
// experiment OFF and no --wake, a user POST to /api/v1/agents/{id}/message
// while a stalled auto-suspend is in flight (DB row still running) is refused
// with 409 "suspended".
func TestStalledAutoSuspend_HandleAgentMessage_ExperimentOffNoWake_Refuses409(t *testing.T) {
	srv, s, _, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

	suspendDone := make(chan bool, 1)
	go func() {
		suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
	}()
	select {
	case <-disp.stopEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for DispatchAgentStop")
	}
	defer func() {
		close(disp.releaseStop)
		<-suspendDone
	}()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+target.ID+"/message", map[string]any{
		"message": "hello without wake",
	})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "suspending")
	assert.Empty(t, disp.getStartCalls())
	assert.Equal(t, int32(0), disp.msgCalls.Load())
	// Pre-persistence refusal must not persist a failed message row.
	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "pre-persistence refusal must not persist a message row")
}

// TestStalledAutoSuspend_ScheduledMessage_FreshReReadAnd404Preserved verifies
// R1 (deliverMessageAction syncAutoWake hook): a scheduled message hitting
// "container ... is not running" re-reads the fresh row and wakes once when
// stalled-suspended, and preserves broker 404 when the target is still running.
func TestStalledAutoSuspend_ScheduledMessage_FreshReReadAnd404Preserved(t *testing.T) {
	makeFireReq := func(t *testing.T, target *store.Agent, msg string) (hubfire.FireRequest, *messageAuthorized) {
		t.Helper()
		payload := MessageEventPayload{
			AgentID:   target.ID,
			AgentName: target.Slug,
			Message:   msg,
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		recorded := hubfire.RecordedAuthority{
			FireID:         tid("fire"),
			DefinitionKind: hubfire.DefinitionKindSchedule,
			DefinitionID:   tid("sched"),
		}
		req := hubfire.FireRequest{
			ProjectID: target.ProjectID,
			Payload:   string(raw),
			FireAt:    time.Now().UTC(),
			Authority: recorded,
		}
		targetCopy := *target
		ma := &messageAuthorized{
			payload: payload,
			agent:   &targetCopy,
			auth: FireAuthority{
				PrincipalKind: store.DelegationPrincipalUser,
				PrincipalID:   DevUserID,
				ProjectID:     target.ProjectID,
				Recorded:      recorded,
			},
		}
		return req, ma
	}

	t.Run("broker 404 re-reads fresh suspended row and wakes once", func(t *testing.T) {
		srv, s, _, target, disp := setupStalledDMFixtures(t, true)
		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: disp,
			onFirstMsg: func() {
				awSuspendFor(t, s, target, state.SuspendReasonStalled)
			},
			firstErr: brokerAgentNotFoundErr(),
		}
		srv.SetDispatcher(transitioningDisp)

		req, ma := makeFireReq(t, target, "scheduled ping")
		outcome, err := srv.deliverMessageAction(context.Background(), req, ma)
		require.NoError(t, err)
		assert.Equal(t, hubfire.FireStatusFired, outcome.Status)
		assert.Len(t, disp.getStartCalls(), 1)
		assert.Equal(t, int32(2), transitioningDisp.calls.Load())
	})

	t.Run("broker 404 on running target is preserved without sentinel leak", func(t *testing.T) {
		srv, _, _, target, disp := setupStalledDMFixtures(t, true)
		transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: disp,
			firstErr:              brokerAgentNotFoundErr(),
		}
		srv.SetDispatcher(transitioningDisp)

		req, ma := makeFireReq(t, target, "scheduled 404")
		_, err := srv.deliverMessageAction(context.Background(), req, ma)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no running container")
		assert.NotContains(t, err.Error(), "no wake recovery applicable")
		assert.NotContains(t, err.Error(), "auto-wake failed")
		assert.Empty(t, disp.getStartCalls())
		assert.Equal(t, int32(1), transitioningDisp.calls.Load())
	})
}

// TestStalledAutoSuspend_ArtifactReviewAndTransfer_UserStoppingNotAutoWaking
// verifies L-2: a user-stopping agent (phase=stopping, run_intent=stopped, no
// hub auto-suspend in flight) is never reported as auto_waking by artifact
// review or queued for auto-wake by artifact transfer; whereas a real in-flight
// hub auto-suspend is truthfully reported as auto_waking and wakes on settle.
func TestStalledAutoSuspend_ArtifactReviewAndTransfer_UserStoppingNotAutoWaking(t *testing.T) {
	t.Run("user-stopping agent is not auto_waking for review or transfer", func(t *testing.T) {
		srv, s, _, target, disp := setupStalledDMFixtures(t, true)
		disp.containerDown.Store(true)
		ctx := contextWithIdentity(context.Background(), userIdentityFor(&store.User{
			ID: DevUserID, Email: "dev@example.com", Role: store.UserRoleAdmin, Status: "active",
		}))
		_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentStopped)
		require.NoError(t, err)
		require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseStopping),
		}))
		stoppingRow, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)

		assert.False(t, srv.shouldQueueAutoWake(stoppingRow))
		assert.False(t, srv.canAutoWakeActionAsker(ctx, stoppingRow))

		info := srv.notifyArtifactReviewWithDelivery(ctx, nil, artifacts.ReviewNotice{
			ArtifactID:   "art-user-stop",
			Seq:          1,
			ScopeKind:    artifacts.ScopeKindProject,
			ScopeRef:     target.ProjectID,
			OwnerKind:    artifacts.PrincipalKindAgent,
			OwnerRef:     target.ID,
			ReviewerKind: artifacts.PrincipalKindUser,
			ReviewerRef:  DevUserID,
		})
		if info != nil {
			assert.NotEqual(t, artifacts.ReviewDeliveryAutoWaking, info.Status)
		}

		delivered := srv.deliverTransferNoticeToAgent(ctx, "art-user-stop", target.ID, "ownership transferred")
		assert.False(t, delivered)
		srv.waitAutoWakeIdle()
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("in-flight hub auto-suspend truthfully reports auto_waking and wakes", func(t *testing.T) {
		srv, s, _, target, disp := setupStalledDMFixtures(t, true)
		ctx := contextWithIdentity(context.Background(), userIdentityFor(&store.User{
			ID: DevUserID, Email: "dev@example.com", Role: store.UserRoleAdmin, Status: "active",
		}))

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(ctx, disp, target, state.SuspendReasonStalled)
		}()
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(disp.releaseStop) }) }
		defer func() {
			release()
			<-suspendDone
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}

		mid, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		assert.True(t, srv.canAutoWakeActionAsker(ctx, mid))

		info := srv.notifyArtifactReviewWithDelivery(ctx, nil, artifacts.ReviewNotice{
			ArtifactID:   "art-hub-suspend",
			Seq:          1,
			ScopeKind:    artifacts.ScopeKindProject,
			ScopeRef:     target.ProjectID,
			OwnerKind:    artifacts.PrincipalKindAgent,
			OwnerRef:     target.ID,
			ReviewerKind: artifacts.PrincipalKindUser,
			ReviewerRef:  DevUserID,
		})
		require.NotNil(t, info)
		assert.Equal(t, artifacts.ReviewDeliveryAutoWaking, info.Status)

		release()
		srv.waitAutoWakeIdle()
		assert.Len(t, disp.getStartCalls(), 1)
	})
}

type holdInterceptStore struct {
	store.Store
	fault       *storeFaultSwitch
	onHoldCheck func(agentID string)
	fired       atomic.Bool
}

func (h *holdInterceptStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	if h.fault.Active() && h.onHoldCheck != nil && h.fired.CompareAndSwap(false, true) {
		h.onHoldCheck(agentID)
	}
	return h.Store.HasActiveAgentHold(ctx, agentID)
}

type countingRecordHandler struct {
	mu    sync.Mutex
	count int
	msgs  []string
}

func (h *countingRecordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingRecordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Message == "message handed to delivery" {
		h.count++
	}
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *countingRecordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingRecordHandler) WithGroup(string) slog.Handler      { return h }
func (h *countingRecordHandler) deliveryCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

type alwaysContainerNotRunningDispatcher struct {
	wakeTrackingDispatcher
	msgCalls atomic.Int32
}

func (d *alwaysContainerNotRunningDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	d.msgCalls.Add(1)
	return brokerAgentNotFoundErr()
}

func placeTestHold(t *testing.T, s store.Store, target *store.Agent) {
	t.Helper()
	_, err := s.CreateAgentHolds(context.Background(), []*store.AgentHold{{
		AgentID:           target.ID,
		ProjectID:         target.ProjectID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   DevUserID,
		Trigger:           store.MembershipLossTriggerMemberRemove,
	}})
	require.NoError(t, err)
}

// TestStalledAutoSuspend_ExplicitWake_PostSettleAuthzAndHold verifies that
// every explicit --wake caller re-checks lifecycle authorization, holds, and
// deletion on the freshly settled row after waitForHubAutoSuspendToSettle
// inside wakeAgentForDMAuthorized, refusing with 0 starts and truthful row
// state if any check now fails.
func TestStalledAutoSuspend_ExplicitWake_PostSettleAuthzAndHold(t *testing.T) {
	t.Run("ExecuteAgentDM unauthorized --wake TOCTOU across settle returns 403 and 0 starts", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		suspendDone := make(chan bool, 1)
		input := awAgentDMInput(sender, target, "explicit wake TOCTOU")
		input.Wake = true // sender lacks ScopeAgentLifecycle
		input.BeforeWake = func() {
			go func() {
				suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
			}()
			select {
			case <-disp.stopEntered:
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting for DispatchAgentStop")
				return
			}
			go func() {
				time.Sleep(150 * time.Millisecond)
				close(disp.releaseStop)
			}()
		}

		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		<-suspendDone
		assert.Nil(t, res)
		require.NotNil(t, dmErr, "unauthorized --wake must be refused after settle")
		assert.Equal(t, ErrCodeForbidden, dmErr.Code)
		assert.Equal(t, http.StatusForbidden, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls(), "unauthorized --wake must never resume target")
		msgs, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items, "pre-persistence refusal must not persist a message row")
	})

	t.Run("ExecuteAgentDM hold placed during settle refuses with 409 and 0 starts", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			placeTestHold(t, s, target)
			close(disp.releaseStop)
		}()

		input := awAgentDMInput(sender, target, "explicit wake hold during settle")
		input.Wake = true
		input.SenderIdentity = wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		<-suspendDone
		assert.Nil(t, res)
		require.NotNil(t, dmErr, "hold placed during settle must refuse explicit wake")
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Contains(t, strings.ToLower(dmErr.Message), "hold")
		assert.Empty(t, disp.getStartCalls())
		msgs, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items)
	})

	t.Run("ExecuteAgentDM soft-delete during settle refuses with 409 and 0 starts", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		db := s.(*entadapter.CompositeStore).DB()
		go func() {
			time.Sleep(150 * time.Millisecond)
			_, _ = db.ExecContext(context.Background(),
				"UPDATE agents SET deleted_at = ?, phase = ? WHERE id = ?",
				time.Now().UTC(), string(state.PhaseRunning), target.ID,
			)
			end()
		}()

		input := awAgentDMInput(sender, target, "explicit wake delete during settle")
		input.Wake = true
		input.SenderIdentity = wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		assert.Nil(t, res)
		require.NotNil(t, dmErr, "soft-delete during settle must refuse explicit wake")
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("ExecuteAgentDM deletion in progress during settle refuses with 409 even when phase stays running", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		db := s.(*entadapter.CompositeStore).DB()
		go func() {
			time.Sleep(150 * time.Millisecond)
			_, _ = db.ExecContext(context.Background(),
				"UPDATE agents SET deletion_state = ?, deletion_lease_at = ?, phase = ? WHERE id = ?",
				string(store.DeletionStateDeleting), time.Now().UTC().Add(time.Minute), string(state.PhaseRunning), target.ID,
			)
			end()
		}()

		input := awAgentDMInput(sender, target, "explicit wake deleting during settle")
		input.Wake = true
		input.SenderIdentity = wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)
		res, dmErr := srv.ExecuteAgentDM(context.Background(), input)
		assert.Nil(t, res)
		require.NotNil(t, dmErr, "deletion in progress during settle must refuse explicit wake even when phase stays running")
		assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("handleAgentMessage unauthorized --wake TOCTOU across settle returns 403 and 0 starts", func(t *testing.T) {
		srv, s, hookStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *holdInterceptStore {
			return &holdInterceptStore{Store: inner, fault: f}
		})
		setIdleSuspendExperiment(t, srv, false)
		ctx := context.Background()

		proj := &store.Project{ID: tid("hm-toctou-proj"), Name: "HM TOCTOU", Slug: "hm-toctou-proj"}
		require.NoError(t, s.CreateProject(ctx, proj))
		srv.seedProjectCreatorMembership(ctx, proj)

		broker := &store.RuntimeBroker{ID: tid("hm-toctou-broker"), Name: "HM Broker", Slug: "hm-toctou-broker", Status: store.BrokerStatusOnline}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
		}))

		target := &store.Agent{
			ID: tid("hm-toctou-target"), ProjectID: proj.ID, Name: "HMTarget", Slug: "hm-toctou-target",
			Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID,
			OwnerID: DevUserID, CreatedBy: DevUserID, MessageMode: store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, target))
		_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
		require.NoError(t, err)
		db := s.(*entadapter.CompositeStore).DB()
		_, err = db.ExecContext(ctx, "UPDATE agents SET run_id = ?, activity = ?, last_seen = ? WHERE id = ?",
			"run-hm-"+target.ID, string(state.ActivityStalled), time.Now().UTC(), target.ID)
		require.NoError(t, err)
		fresh, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		*target = *fresh

		disp := newStalledRaceDispatcher()
		srv.SetDispatcher(disp)
		awReadyOnStart(t, s, target.ID)

		member := &store.User{
			ID: tid("hm-toctou-member"), Email: "hm-toctou-member@example.com",
			DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, member))
		msgAuthzAddProjectMember(t, s, member.ID, proj.ID, proj.Slug, store.GroupMemberRoleMember)
		msgAuthzGrantAgentMessage(t, s, member.ID, proj.ID)
		require.False(t, srv.agentLifecycleAllowed(ctx, userIdentityFor(member), target))

		suspendDone := make(chan bool, 1)
		hookStore.onHoldCheck = func(_ string) {
			// Fires at handleAgentMessage's heldTargetRefusal, strictly after
			// wakeResumeDenial (which passed while running) and before wakeAgentForDM.
			go func() {
				suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
			}()
			select {
			case <-disp.stopEntered:
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting for DispatchAgentStop")
				return
			}
			go func() {
				time.Sleep(150 * time.Millisecond)
				close(disp.releaseStop)
			}()
		}
		fault.Arm()

		rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/agents/"+target.ID+"/message", map[string]any{
			"message": "user explicit wake TOCTOU",
			"wake":    true,
		})
		<-suspendDone
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Empty(t, disp.getStartCalls(), "unauthorized user --wake must never resume target")
		msgs, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: target.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items)
	})

	t.Run("handleAgentMessage hold placed during settle returns 409 and 0 starts", func(t *testing.T) {
		srv, s, _, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			placeTestHold(t, s, target)
			close(disp.releaseStop)
		}()

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+target.ID+"/message", map[string]any{
			"message": "user wake hold during settle",
			"wake":    true,
		})
		<-suspendDone
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, strings.ToLower(rec.Body.String()), "hold")
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("syncWakeForDM hold placed during settle refuses and 0 starts", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			placeTestHold(t, s, target)
			close(disp.releaseStop)
		}()

		err := srv.syncWakeForDM(context.Background(), target, true, wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle))
		<-suspendDone
		require.Error(t, err)
		var refused *errAutoWakeRefused
		require.True(t, errors.As(err, &refused))
		require.NotNil(t, refused.dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, refused.dmErr.Code)
		assert.Contains(t, strings.ToLower(refused.dmErr.Message), "hold")
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("chat v2 pre-persist and mid-dispatch explicit wake hold during settle refuses and 0 starts", func(t *testing.T) {
		setupChat := func(t *testing.T) (*Server, store.Store, string, *store.Agent, *stalledRaceDispatcher) {
			t.Helper()
			srv, s, wcs, proj, db := setupSendTest(t)
			setIdleSuspendExperiment(t, srv, false)
			ctx := t.Context()

			broker := &store.RuntimeBroker{ID: tid("cv2-hold-broker"), Name: "CV2 Hold Broker", Slug: "cv2-hold-broker", Status: store.BrokerStatusOnline}
			require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
			}))
			target := &store.Agent{
				ID: tid("cv2-hold-target"), ProjectID: proj.ID, Name: "CV2HoldTarget", Slug: "cv2-hold-target",
				Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID,
				OwnerID: DevUserID, CreatedBy: DevUserID, MessageMode: store.MessageModeProject,
			}
			require.NoError(t, s.CreateAgent(ctx, target))
			_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
			require.NoError(t, err)
			storeDB := s.(*entadapter.CompositeStore).DB()
			_, err = storeDB.ExecContext(ctx, "UPDATE agents SET run_id = ?, activity = ?, last_seen = ? WHERE id = ?",
				"run-cv2-"+target.ID, string(state.ActivityStalled), time.Now().UTC(), target.ID)
			require.NoError(t, err)
			fresh, err := s.GetAgent(ctx, target.ID)
			require.NoError(t, err)
			*target = *fresh
			awReadyOnStart(t, s, target.ID)

			topicID := tid("cv2-hold-topic")
			require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
				ID: topicID, ProjectID: proj.ID, Name: "cv2-hold-topic",
				CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: target.Slug,
			}))
			setTopicConversationID(t, db, s, topicID, proj.ID)

			disp := newStalledRaceDispatcher()
			srv.SetDispatcher(disp)
			return srv, s, topicID, target, disp
		}

		t.Run("pre-persist explicit wake", func(t *testing.T) {
			srv, s, topicID, target, disp := setupChat(t)
			suspendDone := make(chan bool, 1)
			go func() {
				suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
			}()
			select {
			case <-disp.stopEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for DispatchAgentStop")
			}
			go func() {
				time.Sleep(150 * time.Millisecond)
				placeTestHold(t, s, target)
				close(disp.releaseStop)
			}()

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", map[string]any{
				"content": "chat pre-persist hold during settle",
				"wake":    true,
			})
			<-suspendDone
			assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, strings.ToLower(rec.Body.String()), "hold")
			assert.Empty(t, disp.getStartCalls())
		})

		t.Run("mid-dispatch sync wake hook", func(t *testing.T) {
			srv, s, topicID, target, disp := setupChat(t)
			suspendDone := make(chan bool, 1)
			transitioning := &stalledTransitionOnFirstMsgDispatcher{
				stalledRaceDispatcher: disp,
				onFirstMsg: func() {
					go func() {
						suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
					}()
					select {
					case <-disp.stopEntered:
					case <-time.After(5 * time.Second):
						t.Error("timed out waiting for DispatchAgentStop")
						return
					}
					go func() {
						time.Sleep(150 * time.Millisecond)
						placeTestHold(t, s, target)
						close(disp.releaseStop)
					}()
				},
				firstErr: brokerAgentNotFoundErr(),
			}
			srv.SetDispatcher(transitioning)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", map[string]any{
				"content": "chat mid-dispatch hold during settle",
				"wake":    true,
			})
			<-suspendDone
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, store.MessageDispatchFailed, resp["dispatchState"])
			assert.Empty(t, disp.getStartCalls())
		})
	})

	t.Run("artifacts review explicit wake hold during settle refuses and 0 starts", func(t *testing.T) {
		srv, s, _, target, disp := setupStalledDMFixtures(t, false) // experiment OFF
		awSuspendFor(t, s, target, state.SuspendReasonStalled)
		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		go func() {
			time.Sleep(150 * time.Millisecond)
			placeTestHold(t, s, target)
			end()
		}()

		ctx := contextWithIdentity(context.Background(), userIdentityFor(&store.User{
			ID: DevUserID, Email: "dev@example.com", Role: store.UserRoleAdmin, Status: "active",
		}))
		info := srv.notifyArtifactReviewWithDelivery(ctx, nil, artifacts.ReviewNotice{
			ArtifactID:   "art-explicit-hold-settle",
			Seq:          1,
			ScopeKind:    artifacts.ScopeKindProject,
			ScopeRef:     target.ProjectID,
			OwnerKind:    artifacts.PrincipalKindAgent,
			OwnerRef:     target.ID,
			ReviewerKind: artifacts.PrincipalKindUser,
			ReviewerRef:  DevUserID,
			Wake:         true,
		})
		require.NotNil(t, info)
		assert.Contains(t, []string{artifacts.ReviewDeliveryWakeFailed, artifacts.ReviewDeliveryWakeRefused}, info.Status)
		assert.Contains(t, strings.ToLower(info.Message), "hold")
		assert.Empty(t, disp.getStartCalls())
	})

	t.Run("chat v2 pre-persist explicit wake permission revoked during settle refuses with 403 and 0 starts", func(t *testing.T) {
		srv, s, wcs, proj, db := setupSendTest(t)
		setIdleSuspendExperiment(t, srv, false)
		ctx := t.Context()
		srv.seedProjectCreatorMembership(ctx, proj)

		broker := &store.RuntimeBroker{ID: tid("cv2-perm-pre-broker"), Name: "CV2 Perm Pre Broker", Slug: "cv2-perm-pre-broker", Status: store.BrokerStatusOnline}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
		}))

		member := &store.User{
			ID: tid("cv2-perm-pre-member"), Email: "cv2-perm-pre-member@example.com",
			DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, member))
		msgAuthzAddProjectMember(t, s, member.ID, proj.ID, proj.Slug, store.GroupMemberRoleMember)
		msgAuthzGrantAgentMessage(t, s, member.ID, proj.ID)

		target := &store.Agent{
			ID: tid("cv2-perm-pre-target"), ProjectID: proj.ID, Name: "CV2PermPreTarget", Slug: "cv2-perm-pre-target",
			Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID,
			OwnerID: member.ID, CreatedBy: member.ID, MessageMode: store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, target))
		_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
		require.NoError(t, err)
		storeDB := s.(*entadapter.CompositeStore).DB()
		_, err = storeDB.ExecContext(ctx, "UPDATE agents SET run_id = ?, activity = ?, last_seen = ? WHERE id = ?",
			"run-cv2-pre-"+target.ID, string(state.ActivityStalled), time.Now().UTC(), target.ID)
		require.NoError(t, err)
		fresh, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		*target = *fresh
		awReadyOnStart(t, s, target.ID)
		require.True(t, srv.agentLifecycleAllowed(ctx, userIdentityFor(member), target), "member must have lifecycle permission before settle")

		topicID := tid("cv2-perm-pre-topic")
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: topicID, ProjectID: proj.ID, Name: "cv2-perm-pre-topic",
			CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: target.Slug,
		}))
		setTopicConversationID(t, db, s, topicID, proj.ID)

		disp := newStalledRaceDispatcher()
		srv.SetDispatcher(disp)

		suspendDone := make(chan bool, 1)
		go func() {
			suspendDone <- srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled)
		}()
		select {
		case <-disp.stopEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for DispatchAgentStop")
		}

		// BeforeWake runs strictly after the pre-persist agentLifecycleAllowed
		// check passes and right before wakeAgentForDMAuthorized settles.
		resp, sendErr := srv.sendAgentRouted(
			ctx, "topic:"+topicID, proj.ID, userIdentityFor(member),
			"chat pre-persist perm revoked during settle", member.DisplayName,
			[]*store.Agent{target}, nil, nil, nil, time.Now().UTC(), "", nil,
			chatSendOptions{
				Wake: true,
				BeforeWake: func(int) {
					_, uerr := storeDB.ExecContext(context.Background(),
						"UPDATE agents SET owner_id = ?, created_by = ? WHERE id = ?",
						DevUserID, DevUserID, target.ID)
					if uerr != nil {
						t.Errorf("failed to reassign target ownership: %v", uerr)
					}
					go func() {
						time.Sleep(100 * time.Millisecond)
						close(disp.releaseStop)
					}()
				},
			},
		)
		<-suspendDone
		assert.Nil(t, resp)
		require.NotNil(t, sendErr, "revoked lifecycle permission during settle must refuse pre-persist chat wake")
		assert.Equal(t, http.StatusForbidden, sendErr.Status)
		assert.Equal(t, ErrCodeForbidden, sendErr.Code)
		assert.Empty(t, disp.getStartCalls(), "must not start agent when post-settle explicitWakeAuthorize denies")
	})

	t.Run("chat v2 mid-dispatch explicit wake permission revoked during settle refuses with 0 starts", func(t *testing.T) {
		srv, s, wcs, proj, db := setupSendTest(t)
		setIdleSuspendExperiment(t, srv, false)
		ctx := t.Context()
		srv.seedProjectCreatorMembership(ctx, proj)

		broker := &store.RuntimeBroker{ID: tid("cv2-perm-mid-broker"), Name: "CV2 Perm Mid Broker", Slug: "cv2-perm-mid-broker", Status: store.BrokerStatusOnline}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
		}))

		member := &store.User{
			ID: tid("cv2-perm-mid-member"), Email: "cv2-perm-mid-member@example.com",
			DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, member))
		msgAuthzAddProjectMember(t, s, member.ID, proj.ID, proj.Slug, store.GroupMemberRoleMember)
		msgAuthzGrantAgentMessage(t, s, member.ID, proj.ID)

		target := &store.Agent{
			ID: tid("cv2-perm-mid-target"), ProjectID: proj.ID, Name: "CV2PermMidTarget", Slug: "cv2-perm-mid-target",
			Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID,
			OwnerID: member.ID, CreatedBy: member.ID, MessageMode: store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, target))
		_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
		require.NoError(t, err)
		storeDB := s.(*entadapter.CompositeStore).DB()
		_, err = storeDB.ExecContext(ctx, "UPDATE agents SET run_id = ?, activity = ?, last_seen = ? WHERE id = ?",
			"run-cv2-mid-"+target.ID, string(state.ActivityStalled), time.Now().UTC(), target.ID)
		require.NoError(t, err)
		fresh, err := s.GetAgent(ctx, target.ID)
		require.NoError(t, err)
		*target = *fresh
		awReadyOnStart(t, s, target.ID)
		require.True(t, srv.agentLifecycleAllowed(ctx, userIdentityFor(member), target))

		topicID := tid("cv2-perm-mid-topic")
		require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
			ID: topicID, ProjectID: proj.ID, Name: "cv2-perm-mid-topic",
			CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: target.Slug,
		}))
		setTopicConversationID(t, db, s, topicID, proj.ID)

		baseDisp := newStalledRaceDispatcher()
		suspendDone := make(chan bool, 1)
		transitioning := &stalledTransitionOnFirstMsgDispatcher{
			stalledRaceDispatcher: baseDisp,
			onFirstMsg: func() {
				go func() {
					suspendDone <- srv.autoSuspendAgent(context.Background(), baseDisp, target, state.SuspendReasonStalled)
				}()
				select {
				case <-baseDisp.stopEntered:
				case <-time.After(5 * time.Second):
					t.Error("timed out waiting for DispatchAgentStop")
				}
			},
			firstErr: brokerAgentNotFoundErr(),
		}
		srv.SetDispatcher(transitioning)

		// Mid-dispatch hook checks canExplicitWake (passes while member still
		// owns target), then invokes BeforeWake before wakeAgentForDMAuthorized.
		resp, sendErr := srv.sendAgentRouted(
			ctx, "topic:"+topicID, proj.ID, userIdentityFor(member),
			"chat mid-dispatch perm revoked during settle", member.DisplayName,
			[]*store.Agent{target}, nil, nil, nil, time.Now().UTC(), "", nil,
			chatSendOptions{
				Wake: true,
				BeforeWake: func(int) {
					_, uerr := storeDB.ExecContext(context.Background(),
						"UPDATE agents SET owner_id = ?, created_by = ? WHERE id = ?",
						DevUserID, DevUserID, target.ID)
					if uerr != nil {
						t.Errorf("failed to reassign target ownership: %v", uerr)
					}
					go func() {
						time.Sleep(100 * time.Millisecond)
						close(baseDisp.releaseStop)
					}()
				},
			},
		)
		<-suspendDone
		require.Nil(t, sendErr)
		require.NotNil(t, resp)
		assert.Equal(t, store.MessageDispatchFailed, resp.DispatchState)
		assert.Empty(t, baseDisp.getStartCalls(), "must not start agent when mid-dispatch post-settle explicitWakeAuthorize denies")
	})

	t.Run("artifacts review explicit wake permission revoked during settle refuses with 0 starts", func(t *testing.T) {
		srv, s, hookStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *holdInterceptStore {
			return &holdInterceptStore{Store: inner, fault: f}
		})
		setIdleSuspendExperiment(t, srv, false)
		ctx := context.Background()

		proj := &store.Project{ID: tid("art-perm-proj"), Name: "Art Perm", Slug: "art-perm-proj"}
		require.NoError(t, s.CreateProject(ctx, proj))
		srv.seedProjectCreatorMembership(ctx, proj)

		broker := &store.RuntimeBroker{ID: tid("art-perm-broker"), Name: "Art Perm Broker", Slug: "art-perm-broker", Status: store.BrokerStatusOnline}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
		}))

		member := &store.User{
			ID: tid("art-perm-member"), Email: "art-perm-member@example.com",
			DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, member))
		msgAuthzAddProjectMember(t, s, member.ID, proj.ID, proj.Slug, store.GroupMemberRoleMember)
		msgAuthzGrantAgentMessage(t, s, member.ID, proj.ID)

		target := &store.Agent{
			ID: tid("art-perm-target"), ProjectID: proj.ID, Name: "ArtPermTarget", Slug: "art-perm-target",
			Phase: string(state.PhaseSuspended), SuspendReason: string(state.SuspendReasonStalled),
			RuntimeBrokerID: broker.ID, OwnerID: member.ID, CreatedBy: member.ID,
			MessageMode: store.MessageModeProject,
		}
		require.NoError(t, s.CreateAgent(ctx, target))
		awReadyOnStart(t, s, target.ID)
		require.True(t, srv.agentLifecycleAllowed(ctx, userIdentityFor(member), target))

		disp := newStalledRaceDispatcher()
		srv.SetDispatcher(disp)

		db := s.(*entadapter.CompositeStore).DB()
		end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
		// Fires at agentHeldOrUnknown strictly after agentLifecycleAllowed passes
		// and right before wakeAgentForDMAuthorized settles.
		hookStore.onHoldCheck = func(_ string) {
			_, uerr := db.ExecContext(context.Background(),
				"UPDATE agents SET owner_id = ?, created_by = ? WHERE id = ?",
				DevUserID, DevUserID, target.ID)
			if uerr != nil {
				t.Errorf("failed to reassign target ownership: %v", uerr)
			}
			go func() {
				time.Sleep(100 * time.Millisecond)
				end()
			}()
		}
		fault.Arm()

		reqCtx := contextWithIdentity(ctx, userIdentityFor(member))
		info := srv.notifyArtifactReviewWithDelivery(reqCtx, nil, artifacts.ReviewNotice{
			ArtifactID:   "art-explicit-perm-revoked-settle",
			Seq:          1,
			ScopeKind:    artifacts.ScopeKindProject,
			ScopeRef:     target.ProjectID,
			OwnerKind:    artifacts.PrincipalKindAgent,
			OwnerRef:     target.ID,
			ReviewerKind: artifacts.PrincipalKindUser,
			ReviewerRef:  member.ID,
			Wake:         true,
		})
		require.NotNil(t, info)
		assert.Equal(t, artifacts.ReviewDeliveryWakeRefused, info.Status)
		assert.Empty(t, disp.getStartCalls(), "must not start agent when post-settle explicitWakeAuthorize denies")
	})
}

// TestStalledAutoSuspend_ContainerNotRunningRetry_NoDuplicateRecordLog verifies
// that a container-not-running wake retry does not log a duplicate
// "message handed to delivery" record.
func TestStalledAutoSuspend_ContainerNotRunningRetry_NoDuplicateRecordLog(t *testing.T) {
	srv, s, _, target, disp := setupStalledDMFixtures(t, true)
	transitioning := &stalledTransitionOnFirstMsgDispatcher{
		stalledRaceDispatcher: disp,
		onFirstMsg: func() {
			awSuspendFor(t, s, target, state.SuspendReasonStalled)
		},
		firstErr: brokerAgentNotFoundErr(),
	}
	srv.SetDispatcher(transitioning)

	recHandler := &countingRecordHandler{}
	recLog := slog.New(recHandler)
	msg := messages.NewInstruction("user:dev", "agent:"+target.Slug, "no duplicate log")

	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := dispatchWithBrokerRetry(
		dctx,
		srv.deliveryMarker(),
		recLog,
		transitioning,
		target,
		"no duplicate log",
		false,
		msg,
		withSyncAutoWakeHook(func(wctx context.Context, a *store.Agent) (context.Context, context.CancelFunc, error) {
			if err := srv.syncWakeForDM(wctx, a, false, nil); err != nil {
				return nil, nil, err
			}
			return nil, nil, nil
		}),
	)
	require.NoError(t, err)
	assert.Equal(t, int32(2), transitioning.calls.Load(), "expected initial failed attempt plus one retry")
	assert.Equal(t, 1, recHandler.deliveryCount(), "must log 'message handed to delivery' only once across retry")
}

// TestStalledAutoSuspend_ChatV2_MidDispatchExplicitWake_HeldTargetSkipsWake
// kills sec2 M3: removing !agentHeldOrUnknown from chat v2 canExplicitWake.
func TestStalledAutoSuspend_ChatV2_MidDispatchExplicitWake_HeldTargetSkipsWake(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	setIdleSuspendExperiment(t, srv, false)
	ctx := t.Context()

	broker := &store.RuntimeBroker{ID: tid("m3-broker"), Name: "M3 Broker", Slug: "m3-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
	target := &store.Agent{
		ID: tid("m3-target"), ProjectID: proj.ID, Name: "M3Target", Slug: "m3-target",
		Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID,
		OwnerID: DevUserID, CreatedBy: DevUserID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
	require.NoError(t, err)
	storeDB := s.(*entadapter.CompositeStore).DB()
	_, err = storeDB.ExecContext(ctx, "UPDATE agents SET run_id = ?, activity = ?, last_seen = ? WHERE id = ?",
		"run-m3-"+target.ID, string(state.ActivityStalled), time.Now().UTC(), target.ID)
	require.NoError(t, err)
	fresh, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	*target = *fresh
	awReadyOnStart(t, s, target.ID)

	topicID := tid("m3-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "m3-topic",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: target.Slug,
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	baseDisp := newStalledRaceDispatcher()
	endSuspend := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-endSuspend:
		default:
			close(endSuspend)
		}
	})
	var endTracker func()
	transitioning := &stalledTransitionOnFirstMsgDispatcher{
		stalledRaceDispatcher: baseDisp,
		onFirstMsg: func() {
			placeTestHold(t, s, target)
			endTracker = srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
			go func() {
				<-endSuspend
				endTracker()
			}()
		},
		firstErr: brokerAgentNotFoundErr(),
	}
	srv.SetDispatcher(transitioning)

	// Without !agentHeldOrUnknown in canExplicitWake, the hook would enter
	// wakeAgentForDM and block on the open autoSuspending tracker until timeout.
	done := make(chan int, 1)
	go func() {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages", map[string]any{
			"content": "held mid-dispatch must skip wake immediately",
			"wake":    true,
		})
		var resp map[string]any
		if json.Unmarshal(rec.Body.Bytes(), &resp) == nil && resp["dispatchState"] == store.MessageDispatchFailed {
			done <- rec.Code
			return
		}
		done <- -1
	}()

	select {
	case code := <-done:
		assert.Equal(t, http.StatusCreated, code)
	case <-time.After(2 * time.Second):
		close(endSuspend)
		t.Fatal("chat v2 mid-dispatch wake blocked on settle for an already-held target (canExplicitWake must reject held targets before entering wakeAgentForDM)")
	}
	assert.Empty(t, baseDisp.getStartCalls())
	assert.Equal(t, int32(1), transitioning.calls.Load())
}

// TestStalledAutoSuspend_SyncWakeForDM_HeldTargetFailsFastBeforeSettle kills
// sec2 M4: removing heldTargetRefusal at the top of syncWakeForDM.
func TestStalledAutoSuspend_SyncWakeForDM_HeldTargetFailsFastBeforeSettle(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, false) // experiment OFF
	placeTestHold(t, s, target)
	end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
	defer end()

	wctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := srv.syncWakeForDM(wctx, target, true, wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle))
	require.Error(t, err)
	var refused *errAutoWakeRefused
	require.True(t, errors.As(err, &refused))
	require.NotNil(t, refused.dmErr)
	assert.Equal(t, ErrCodeAgentNotRunning, refused.dmErr.Code)
	assert.Contains(t, strings.ToLower(refused.dmErr.Message), "hold")
	assert.Empty(t, disp.getStartCalls())
}

// TestStalledAutoSuspend_ContainerNotRunningRetry_WakesAtMostOnce kills sec2
// M11: removing !wokeOnce from the container-not-running retry branch.
func TestStalledAutoSuspend_ContainerNotRunningRetry_WakesAtMostOnce(t *testing.T) {
	srv, _, _, target, _ := setupStalledDMFixtures(t, true)
	alwaysFail := &alwaysContainerNotRunningDispatcher{}
	srv.SetDispatcher(alwaysFail)

	var wakeCalls atomic.Int32
	msg := messages.NewInstruction("user:dev", "agent:"+target.Slug, "wake at most once")
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := dispatchWithBrokerRetry(
		dctx,
		srv.deliveryMarker(),
		nil,
		alwaysFail,
		target,
		"wake at most once",
		false,
		msg,
		withSyncAutoWakeHook(func(_ context.Context, _ *store.Agent) (context.Context, context.CancelFunc, error) {
			if wakeCalls.Add(1) > 3 {
				cancel()
			}
			return nil, nil, nil
		}),
	)
	require.Error(t, err)
	assert.Equal(t, int32(1), wakeCalls.Load(), "container-not-running retry must invoke syncAutoWake at most once")
	assert.Equal(t, int32(2), alwaysFail.msgCalls.Load(), "expected initial attempt plus exactly one retry")
}

type getAgentFaultStore struct {
	store.Store
	fault      *storeFaultSwitch
	errToFail  error
	failNext   atomic.Bool
	failedOnce atomic.Bool
}

func (g *getAgentFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if g.fault.Active() && g.failNext.Load() && g.failedOnce.CompareAndSwap(false, true) {
		return nil, g.errToFail
	}
	return g.Store.GetAgent(ctx, id)
}

// TestStalledAutoSuspend_SettleReadError_UserFacingNoRawStoreText verifies
// O3 and security round-3 L2: a store read failure after the in-flight
// auto-suspend tracker finishes returns a user-facing 503 message about
// confirming suspend (not "was already starting") and never exposes raw
// store/driver error text in the DM response.
func TestStalledAutoSuspend_SettleReadError_UserFacingNoRawStoreText(t *testing.T) {
	srv, s, faultStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *getAgentFaultStore {
		return &getAgentFaultStore{
			Store:     inner,
			fault:     f,
			errToFail: errors.New("sqlite3: secret_internal_table_xyz constraint failed"),
		}
	})
	setIdleSuspendExperiment(t, srv, true)
	ctx := context.Background()

	proj := &store.Project{ID: tid("settle-err-proj"), Name: "Settle Err", Slug: "settle-err-proj"}
	require.NoError(t, s.CreateProject(ctx, proj))
	broker := &store.RuntimeBroker{ID: tid("settle-err-broker"), Name: "Settle Err Broker", Slug: "settle-err-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
	sender := &store.Agent{
		ID: tid("settle-err-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "settle-err-sender",
		Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID, MessageMode: store.MessageModeProject,
	}
	target := &store.Agent{
		ID: tid("settle-err-target"), ProjectID: proj.ID, Name: "Target", Slug: "settle-err-target",
		Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, sender))
	require.NoError(t, s.CreateAgent(ctx, target))
	disp := newStalledRaceDispatcher()
	srv.SetDispatcher(disp)

	end := srv.autoSuspending.begin(target.ID, state.SuspendReasonStalled)
	fault.Arm()
	go func() {
		time.Sleep(100 * time.Millisecond)
		faultStore.failNext.Store(true)
		end()
	}()

	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, target, "dm hitting settle read error"))
	assert.Nil(t, res)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "suspending")
	assert.NotContains(t, dmErr.Message, "was already starting")
	assert.NotContains(t, dmErr.Message, "sqlite3")
	assert.NotContains(t, dmErr.Message, "secret_internal_table_xyz")
	assert.Empty(t, disp.getStartCalls())
}

type swapIntentPauseStore struct {
	store.Store
	fault   *storeFaultSwitch
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *swapIntentPauseStore) SwapAutoSuspendRunIntent(ctx context.Context, id string, newIntent store.RunIntent, requireSuspendPolicyAuto bool) (store.RunIntent, time.Time, error) {
	if w.fault.Active() {
		w.once.Do(func() { close(w.entered) })
		select {
		case <-w.release:
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		}
		return "", time.Time{}, store.ErrAgentNeverSuspend
	}
	return w.Store.SwapAutoSuspendRunIntent(ctx, id, newIntent, requireSuspendPolicyAuto)
}

// TestStalledAutoSuspend_NeverSuspendPreconditionAbort_DoesNotRefuseConcurrentDM
// verifies O1: autoSuspendAgent registers the in-flight tracker only after the
// claim/intent swap succeeds, so a precondition abort (e.g. never-suspend)
// never transiently marks the running agent as suspending or refuses a
// concurrent DM.
func TestStalledAutoSuspend_NeverSuspendPreconditionAbort_DoesNotRefuseConcurrentDM(t *testing.T) {
	srv, s, pauseStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *swapIntentPauseStore {
		return &swapIntentPauseStore{
			Store:   inner,
			fault:   f,
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
	})
	setIdleSuspendExperiment(t, srv, false) // experiment OFF so any spurious tracker active refuses with 409
	ctx := context.Background()

	proj := &store.Project{ID: tid("ns-abort-proj"), Name: "NS Abort", Slug: "ns-abort-proj"}
	require.NoError(t, s.CreateProject(ctx, proj))
	broker := &store.RuntimeBroker{ID: tid("ns-abort-broker"), Name: "NS Abort Broker", Slug: "ns-abort-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: proj.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
	sender := &store.Agent{
		ID: tid("ns-abort-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "ns-abort-sender",
		Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID,
	}
	target := &store.Agent{
		ID: tid("ns-abort-target"), ProjectID: proj.ID, Name: "Target", Slug: "ns-abort-target",
		Phase: string(state.PhaseRunning), RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, sender))
	require.NoError(t, s.CreateAgent(ctx, target))
	_, err := s.SetRunIntent(ctx, target.ID, store.RunIntentRunning)
	require.NoError(t, err)
	db := s.(*entadapter.CompositeStore).DB()
	_, err = db.ExecContext(ctx, "UPDATE agents SET run_id = ?, activity = ?, last_seen = ? WHERE id = ?",
		"run-ns-"+target.ID, string(state.ActivityStalled), time.Now().UTC(), target.ID)
	require.NoError(t, err)
	fresh, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	*target = *fresh

	disp := newStalledRaceDispatcher()
	srv.SetDispatcher(disp)

	fault.Arm()
	suspendDone := make(chan bool, 1)
	go func() {
		suspendDone <- srv.autoSuspendAgent(ctx, disp, target, state.SuspendReasonStalled)
	}()

	select {
	case <-pauseStore.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SwapAutoSuspendRunIntent")
	}

	assert.False(t, srv.autoSuspending.isActive(target.ID), "tracker must not be active before SwapAutoSuspendRunIntent succeeds")

	res, dmErr := srv.ExecuteAgentDM(ctx, awAgentDMInput(sender, target, "dm during never-suspend precondition abort"))
	close(pauseStore.release)
	assert.False(t, <-suspendDone, "never-suspend precondition must abort auto-suspend")

	require.Nil(t, dmErr, "concurrent DM during precondition-aborted auto-suspend must deliver without refusal")
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	assert.Empty(t, disp.getStartCalls())
	assert.Equal(t, int32(1), disp.msgCalls.Load())
}
