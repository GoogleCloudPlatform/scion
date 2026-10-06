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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3471: the resume a direct-message wake runs no longer follows
// the sender's request. A sender that disconnects or gives up mid-resume
// must not cancel the broker launch, nor leave the agent half-started; the
// dispatch is bounded by syncDispatchTimeout and, when the caller has one,
// by its own deadline (the earlier wins). These tests are not parallel:
// some shorten the package-level timeout.

type dmWakeCtxKey struct{}

func TestDetachLaunchKeepDeadline(t *testing.T) {
	t.Run("no deadline", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.WithValue(context.Background(), dmWakeCtxKey{}, "v"))
		ctx, done := detachLaunchKeepDeadline(parent)
		defer done()
		cancel()
		require.Error(t, parent.Err())
		assert.NoError(t, ctx.Err(), "the parent's cancellation does not reach the launch")
		_, has := ctx.Deadline()
		assert.False(t, has, "no deadline is invented")
		assert.Equal(t, "v", ctx.Value(dmWakeCtxKey{}), "values are kept")
	})
	t.Run("deadline kept", func(t *testing.T) {
		want := time.Now().Add(time.Hour)
		parent, cancel := context.WithDeadline(context.WithValue(context.Background(), dmWakeCtxKey{}, "v"), want)
		defer cancel()
		ctx, done := detachLaunchKeepDeadline(parent)
		defer done()
		cancel() // the sender leaves before its deadline
		require.Error(t, parent.Err())
		assert.NoError(t, ctx.Err(), "the parent's cancellation does not reach the launch")
		got, has := ctx.Deadline()
		require.True(t, has, "the caller's deadline is kept")
		assert.True(t, got.Equal(want), "deadline %v, want %v", got, want)
		assert.Equal(t, "v", ctx.Value(dmWakeCtxKey{}), "values are kept")
	})
	t.Run("cancel func releases", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		ctx, done := detachLaunchKeepDeadline(parent)
		done()
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
	})
}

// wakeProbeDispatcher records the ctx the wake's start dispatch ran on. It
// cancels the sender's request inside the dispatch, as a client that gives
// up mid-resume does.
type wakeProbeDispatcher struct {
	quotaLifecycleDispatcher
	s            store.Store
	cancelSender context.CancelFunc
	// block waits until the dispatch ctx is done (capped at 5s) and fails
	// with how it ended.
	block bool
	// fail fails a live dispatch, as a real broker failure.
	fail bool
	// events records the status events the server publishes.
	events *dmWakePhaseRecorder

	mu          sync.Mutex
	ctxErr      error
	hadDeadline bool
	deadline    time.Time
	ready       sync.WaitGroup
}

func (d *wakeProbeDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCount.Add(1)
	dl, has := ctx.Deadline()
	if d.cancelSender != nil {
		d.cancelSender()
	}
	var err error
	if d.block {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(5 * time.Second):
			err = errProbeNeverDone
		}
	} else {
		err = ctx.Err()
	}
	d.mu.Lock()
	d.ctxErr, d.hadDeadline, d.deadline = err, has, dl
	d.mu.Unlock()
	if err != nil {
		// A dispatch ctx that ended aborts the broker request.
		return err
	}
	if d.fail {
		return errors.New("simulated broker start failure")
	}
	// The new container's first status is the readiness signal. Post it
	// once the wake's starting write cleared the old generation's message.
	bg := context.Background()
	if err := d.s.UpdateAgentStatus(bg, agent.ID, store.AgentStatusUpdate{Message: "old generation"}); err != nil {
		return err
	}
	d.ready.Add(1)
	go func() {
		defer d.ready.Done()
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			if got, gerr := d.s.GetAgent(bg, agent.ID); gerr == nil && got.Message == "" {
				_ = d.s.UpdateAgentStatus(bg, agent.ID, store.AgentStatusUpdate{Activity: string(state.ActivityWorking)})
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	agent.Phase = string(state.PhaseRunning)
	agent.ContainerStatus = "running"
	return nil
}

// result reports whether the dispatch ctx had a deadline, which one, and
// how the ctx ended once the probe acted (nil: live).
func (d *wakeProbeDispatcher) result() (bool, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hadDeadline, d.deadline, d.ctxErr
}

// dmWakePhaseRecorder records the phase of each agent status event at the
// time it is published (the published agent is mutated afterwards).
type dmWakePhaseRecorder struct {
	noopEventPublisher
	mu     sync.Mutex
	phases []string
}

func (r *dmWakePhaseRecorder) PublishAgentStatus(_ context.Context, agent *store.Agent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases = append(r.phases, agent.Phase)
}

func (r *dmWakePhaseRecorder) published() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.phases...)
}

// newDMWakeProbe returns a server (start claims on or off), a suspended
// agent on a broker with room for it, and a probe dispatcher.
func newDMWakeProbe(t *testing.T, name string, claims bool) (*Server, store.Store, *store.RuntimeBroker, *store.Agent, *wakeProbeDispatcher) {
	t.Helper()
	srv, s := testServer(t)
	srv.startClaimsOn = claims
	disp := &wakeProbeDispatcher{s: s, events: &dmWakePhaseRecorder{}}
	srv.SetDispatcher(disp)
	srv.SetEventPublisher(disp.events)
	setBrokerAgentCeiling(t, s, 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, name)
	a := newQuotaTestAgent(t, s, broker, project, name, state.PhaseSuspended)
	return srv, s, broker, a, disp
}

func dmWakeClaimModes(t *testing.T, fn func(t *testing.T, suffix string, claims bool)) {
	for _, tc := range []struct {
		name   string
		claims bool
	}{{"claims on", true}, {"claims off", false}} {
		t.Run(tc.name, func(t *testing.T) {
			suffix := "on"
			if !tc.claims {
				suffix = "off"
			}
			fn(t, suffix, tc.claims)
		})
	}
}

// A sender that gives up mid-resume does not cancel the launch: the dispatch
// ctx stays live, the agent becomes ready and ends running with its slot
// held, not half-started in starting.
func TestDMWake_SenderCancelDuringResume_LaunchSurvives(t *testing.T) {
	dmWakeClaimModes(t, func(t *testing.T, suffix string, claims bool) {
		srv, s, broker, a, disp := newDMWakeProbe(t, "dmwake-survive-"+suffix, claims)
		reqCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		disp.cancelSender = cancel

		res, dmErr := srv.wakeAgentForDM(reqCtx, a)
		disp.ready.Wait()

		require.Error(t, reqCtx.Err(), "the sender's request was cancelled")
		hadDeadline, _, ctxErr := disp.result()
		assert.NoError(t, ctxErr, "the dispatch ctx did not follow the sender")
		assert.True(t, hadDeadline, "the dispatch is bounded")
		require.Nil(t, dmErr)
		require.NotNil(t, res)
		assert.Equal(t, WakeResumed, res.Outcome)
		got, err := s.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase, "not left half-started")
		assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "the resumed agent holds its slot")
		assert.EqualValues(t, 1, disp.startCount.Load())
		assert.Contains(t, disp.events.published(), string(state.PhaseRunning),
			"the running status is published although the sender left")
	})
}

// claimSignalStore signals the first ClaimAgentStart call once it returns.
type claimSignalStore struct {
	store.Store
	once    sync.Once
	entered chan struct{}
}

func (c *claimSignalStore) ClaimAgentStart(ctx context.Context, agentID, owner string, kind store.StartClaimKind, target string, ttl time.Duration) (store.StartClaim, error) {
	claim, err := c.Store.ClaimAgentStart(ctx, agentID, owner, kind, target, ttl)
	c.once.Do(func() { close(c.entered) })
	return claim, err
}

// With start claims on (production), the wake's claim is taken on the
// launch context: a sender that leaves while the wake waits for a queued
// stop's claim does not abandon the wake, which takes the claim once the
// stop releases it and resumes the agent.
func TestDMWake_SenderCancelWhileWaitingForClaim_ClaimTaken(t *testing.T) {
	srv, s, broker, a, disp := newDMWakeProbe(t, "dmwake-claimwait", true)
	ctx := context.Background()
	at, err := s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	stop, err := s.ClaimAgentStop(ctx, a.ID, "drain-hub", at, time.Minute)
	require.NoError(t, err)
	sig := &claimSignalStore{Store: s, entered: make(chan struct{})}
	srv.store = sig

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		res *WakeResult
		err *AgentDMError
	}
	done := make(chan outcome, 1)
	go func() {
		res, dmErr := srv.wakeAgentForDM(reqCtx, a)
		done <- outcome{res, dmErr}
	}()
	select {
	case <-sig.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the wake never tried to take its claim")
	}
	// The first attempt found the stop's claim held; the wake now waits for
	// it. The sender leaves, then the stop releases its claim.
	cancel()
	released, err := s.ReleaseAgentStart(ctx, a.ID, stop.ID, "drain-hub")
	require.NoError(t, err)
	require.True(t, released)

	var out outcome
	select {
	case out = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the wake did not finish")
	}
	disp.ready.Wait()
	require.Nil(t, out.err)
	require.NotNil(t, out.res)
	assert.Equal(t, WakeResumed, out.res.Outcome)
	assert.EqualValues(t, 1, disp.startCount.Load(), "the claim was taken and the resume dispatched")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.Empty(t, got.StartClaimID, "the wake's claim was released")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID))
	assert.Contains(t, disp.events.published(), string(state.PhaseRunning))
}

// A real broker failure after the sender left still rolls the start back:
// the agent returns to suspended and its slot is released.
func TestDMWake_SenderCancelThenBrokerFailure_RollsBack(t *testing.T) {
	dmWakeClaimModes(t, func(t *testing.T, suffix string, claims bool) {
		srv, s, broker, a, disp := newDMWakeProbe(t, "dmwake-rollback-"+suffix, claims)
		disp.fail = true
		reqCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		disp.cancelSender = cancel

		_, dmErr := srv.wakeAgentForDM(reqCtx, a)

		require.NotNil(t, dmErr)
		assert.Equal(t, ErrCodeRuntimeError, dmErr.Code)
		_, _, ctxErr := disp.result()
		assert.NoError(t, ctxErr, "the dispatch ctx did not follow the sender")
		got, err := s.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase, "the failed start was rolled back")
		assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID), "the slot was released")
	})
}

// The detached dispatch is still bounded by syncDispatchTimeout when the
// caller has no deadline (the user and agent direct messages).
func TestDMWake_DispatchHonoursSyncDispatchBound(t *testing.T) {
	dmWakeClaimModes(t, func(t *testing.T, suffix string, claims bool) {
		shortenSyncDispatchTimeout(t, 200*time.Millisecond)
		srv, s, broker, a, disp := newDMWakeProbe(t, "dmwake-bound-"+suffix, claims)
		disp.block = true
		reqCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		disp.cancelSender = cancel

		start := time.Now()
		_, dmErr := srv.wakeAgentForDM(reqCtx, a)

		require.NotNil(t, dmErr)
		hadDeadline, _, ctxErr := disp.result()
		assert.True(t, hadDeadline)
		assert.ErrorIs(t, ctxErr, context.DeadlineExceeded, "ended by its own bound, not the sender")
		assert.Less(t, time.Since(start), 4*time.Second)
		got, err := s.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase, "the timed-out start was rolled back")
		assert.EqualValues(t, 0, brokerReservationCount(t, s, broker.ID))
	})
}

// A caller deadline earlier than syncDispatchTimeout (the chat wake's
// resume budget) still bounds the dispatch: the earlier wins.
func TestDMWake_CallerDeadlineEarlierWins(t *testing.T) {
	dmWakeClaimModes(t, func(t *testing.T, suffix string, claims bool) {
		srv, _, _, a, disp := newDMWakeProbe(t, "dmwake-deadline-"+suffix, claims)
		disp.block = true
		want := time.Now().Add(2500 * time.Millisecond)
		reqCtx, cancel := context.WithDeadline(context.Background(), want)
		defer cancel()

		_, dmErr := srv.wakeAgentForDM(reqCtx, a)

		require.NotNil(t, dmErr)
		hadDeadline, got, ctxErr := disp.result()
		require.True(t, hadDeadline)
		assert.True(t, got.Equal(want), "dispatch deadline %v, want the caller's %v", got, want)
		assert.ErrorIs(t, ctxErr, context.DeadlineExceeded)
	})
}
