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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
		return fmt.Errorf("Error response from daemon: container %s is not running", agent.ID)
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
		time.Sleep(100 * time.Millisecond)
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
		time.Sleep(100 * time.Millisecond)
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

// TestStalledAutoSuspend_DM_StoppingPhaseWithoutLocalTracker_WakesAndDelivers
// verifies the cross-replica stopping window: phase=stopping with
// run_intent=stopped and no local autoSuspending entry still waits for the
// stop to settle to suspended(stalled), then wakes and delivers once.
func TestStalledAutoSuspend_DM_StoppingPhaseWithoutLocalTracker_WakesAndDelivers(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

	_, err := s.SetRunIntent(context.Background(), target.ID, store.RunIntentStopped)
	require.NoError(t, err)
	require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
		Phase: string(state.PhaseStopping),
	}))
	stoppingRow, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	require.False(t, srv.autoSuspending.isActive(target.ID))

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase:         string(state.PhaseSuspended),
			SuspendReason: string(state.SuspendReasonStalled),
		})
	}()

	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, stoppingRow, "hello cross-replica stopping"))
	require.Nil(t, dmErr, "DM to stopping agent with stopped intent must settle, wake, and deliver")
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.Equal(t, int32(1), disp.msgCalls.Load())
}

// TestStalledAutoSuspend_DM_PostStopBrokerContainerNotRunning_WakesAndRetriesOnce
// verifies Window C: the caller read phase=running before stalled auto-suspend
// finished, and DispatchAgentMessage fails synchronously with "container ...
// is not running" (e.g. cross-replica race). The hub re-reads, sees
// suspended(stalled), wakes once, and retries delivery once.
func TestStalledAutoSuspend_DM_PostStopBrokerContainerNotRunning_WakesAndRetriesOnce(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

	// Capture a stale running snapshot as a caller would read before suspend.
	staleRunning := *target

	// Complete the stalled auto-suspend so the store row is suspended(stalled).
	close(disp.releaseStop)
	require.True(t, srv.autoSuspendAgent(context.Background(), disp, target, state.SuspendReasonStalled))
	require.Equal(t, string(state.PhaseSuspended), awAgentPhase(t, s, target.ID))

	// Even with lifecycle.idle_suspend on, ExecuteAgentDM with staleRunning
	// enters dispatchMarkedAgentMessage -> MarkAgentDelivery sees suspended(stalled)
	// -> wakes once and delivers once.
	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, &staleRunning, "hello after stalled suspend"))
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.Equal(t, int32(1), disp.msgCalls.Load())
}

// TestStalledAutoSuspend_DM_BrokerNotRunningError_ReChecksAndWakesOnce tests
// the synchronous broker error recovery path when DispatchAgentMessage itself
// returns "container ... is not running" (e.g. on a replica where
// MarkAgentDelivery ran just before the stop).
func TestStalledAutoSuspend_DM_BrokerNotRunningError_ReChecksAndWakesOnce(t *testing.T) {
	srv, s, sender, target, disp := setupStalledDMFixtures(t, true)

	// First DispatchAgentMessage returns Docker's "container ... is not running"
	// AND simulates the store having transitioned to suspended(stalled).
	disp.msgErrOnce = fmt.Errorf("Error response from daemon: container %s is not running", target.ID)
	// Transition target to suspended(stalled) as soon as MarkAgentDelivery passes
	// by using a hook on DispatchAgentMessage? Even simpler: test via
	// dispatchWithBrokerRetry directly or set up a dispatcher that transitions
	// the DB row on first attempt.
	transitioningDisp := &stalledTransitionOnFirstMsgDispatcher{
		stalledRaceDispatcher: disp,
		onFirstMsg: func() {
			awSuspendFor(t, s, target, state.SuspendReasonStalled)
		},
		firstErr: fmt.Errorf("Error response from daemon: container %s is not running", target.ID),
	}
	srv.SetDispatcher(transitioningDisp)

	res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, target, "hello racing broker"))
	require.Nil(t, dmErr, "broker container-not-running error when target just stalled-suspended must wake and retry once")
	require.NotNil(t, res)
	assert.Equal(t, AgentDMAccepted, res.Outcome)
	assert.Len(t, disp.getStartCalls(), 1)
	assert.Equal(t, int32(2), transitioningDisp.calls.Load(), "one failed attempt + one retry after wake")

	msg, err := s.GetMessage(context.Background(), res.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)
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
	t.Run("user suspend settling from stopping does not auto-wake", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		_, err := s.SetRunIntent(context.Background(), target.ID, store.RunIntentStopped)
		require.NoError(t, err)
		require.NoError(t, s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseStopping),
		}))
		stopping, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		go func() {
			time.Sleep(80 * time.Millisecond)
			_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
				Phase:         string(state.PhaseSuspended),
				SuspendReason: string(state.SuspendReasonUser),
			})
		}()

		res, dmErr := srv.ExecuteAgentDM(context.Background(), awAgentDMInput(sender, stopping, "do not wake user suspend"))
		assert.Nil(t, res)
		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
		assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
		assert.Empty(t, disp.getStartCalls(), "user-suspended agent must never auto-wake")
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
		go func() {
			time.Sleep(100 * time.Millisecond)
			close(disp.releaseStop)
		}()

		mid, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)

		input := awAgentDMInput(sender, mid, "authorized explicit wake")
		input.SenderIdentity = wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle)
		input.Wake = true
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

	t.Run("deleting target in stopping phase is never woken", func(t *testing.T) {
		srv, s, sender, target, disp := setupStalledDMFixtures(t, true)
		db := s.(*entadapter.CompositeStore).DB()
		_, err := db.ExecContext(context.Background(),
			"UPDATE agents SET phase = ?, run_intent = ?, deletion_state = ?, deletion_lease_at = ? WHERE id = ?",
			string(state.PhaseStopping), string(store.RunIntentStopped), string(store.DeletionStateDeleting), time.Now().UTC().Add(time.Minute), target.ID,
		)
		require.NoError(t, err)
		deletingRow, err := s.GetAgent(context.Background(), target.ID)
		require.NoError(t, err)
		assert.False(t, srv.isStoppingForSuspend(deletingRow), "deleting row must never be classified as stopping-for-suspend")
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
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(disp.releaseStop)
	}()

	mid, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
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
