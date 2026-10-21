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

// A start refused before anything ran, after a stop was queued for the
// agent's offline broker (ptone/scion#5070), puts the stopped intent back:
// the agent keeps its queued stop pending (container status stop_queued,
// run intent stopped), so the settle, the intent backstop and the next
// lifecycle action all treat it as a queued stop again.

package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusedStartFixture is a claim fixture whose agent's stop is queued for its
// offline broker (intent stopped at intentAt, container status stop_queued
// with the notice, a pending stop row), with a status-event recorder.
type refusedStartFixture struct {
	*reconcileFixture
	disp     *claimTestDispatcher
	agent    *store.Agent
	intentAt time.Time
	events   *trackingEventPublisher
}

// newRefusedStartFixture builds the fixture; withSlot records the agent's
// reservation, as a queued stop holds it.
func newRefusedStartFixture(t *testing.T, withSlot bool) *refusedStartFixture {
	t.Helper()
	f, d, a := newClaimFixture(t)
	if withSlot {
		seedSlot(t, f.s, f.brokerID, a.ID)
	}
	at := queueStop(t, f, a, "")
	ev := &trackingEventPublisher{}
	f.srv.events = ev
	return &refusedStartFixture{reconcileFixture: f, disp: d, agent: getAgent(t, f.s, a.ID), intentAt: at, events: ev}
}

// start sends a lifecycle start of the fixture's agent and returns its code.
func (q *refusedStartFixture) start(t *testing.T) int {
	t.Helper()
	code, _ := lifecycle(t, q.reconcileFixture, q.agent.ID, "start")
	return code
}

// lastPublished returns the last status event published for the agent, or
// nil.
func (q *refusedStartFixture) lastPublished() *store.Agent {
	var last *store.Agent
	for _, a := range q.events.publishedAgents() {
		if a.ID == q.agent.ID {
			last = a
		}
	}
	return last
}

// A start refused before anything ran puts the stopped intent back, so the
// queued stop is still pending: intent stopped, container status
// stop_queued, phase stopped, the claim released, the reservation the queued
// stop holds unchanged, and the refusal shown and published. A flat Runtime
// Broker refusal shows its own message (the handler records the same one).
func TestRefusedStartAfterQueuedStop_RestoresStoppedIntent(t *testing.T) {
	flatRefusal := &RuntimeTargetRefusal{Code: "runtime_target_mismatch", Status: http.StatusConflict, Message: "the Runtime Broker serves another runtime target"}
	cases := []struct {
		name     string
		withSlot bool
		// fill takes the broker's only slot before the start.
		fill    bool
		err     error
		message string
	}{
		{name: "request not sent", withSlot: true, err: fmt.Errorf("x: %w", errStartRequestNotSent), message: "Start failed:"},
		{name: "flat runtime target refusal", withSlot: true, err: flatRefusal, message: flatRefusal.Message},
		{name: "broker capacity", fill: true, message: "Start failed:"},
		{name: "broker draining", withSlot: true, err: drainingErr("5"), message: "Start failed:"},
		{name: "runtime unavailable", withSlot: true, err: runtimeUnavailableErr(), message: "Start failed:"},
		{name: "run not begun", withSlot: true, err: fmt.Errorf("%w: %w", errStartRunNotBegun, errors.New("disk full")), message: "Start failed:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := newRefusedStartFixture(t, tc.withSlot)
			if tc.fill {
				setBrokerAgentCeiling(t, q.s, 1)
				filler := q.addAgent("rs-filler", "running", "working")
				seedSlot(t, q.s, q.brokerID, filler.ID)
			}
			q.disp.start = func(ctx context.Context, a *store.Agent) error {
				if tc.err == nil {
					t.Fatal("dispatched past the capacity refusal")
				}
				return tc.err
			}
			def := brokerLimitDefID(t, q.s)
			heldBefore := activeReservation(t, q.s, def, q.agent.ID)

			code := q.start(t)
			require.GreaterOrEqual(t, code, 400, "the start is refused")

			got := getAgent(t, q.s, q.agent.ID)
			assert.Equal(t, store.RunIntentStopped, got.RunIntent, "a definitely refused start after a queued stop restores the stopped intent")
			assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "the queued stop is still pending")
			assert.Equal(t, "stopped", got.Phase)
			assert.True(t, store.QueuedStopStillIntended(got), "the queued-stop paths apply again")
			assert.Empty(t, got.StartClaimID, "the claim is released")
			assert.Contains(t, got.Message, tc.message, "the refusal is shown")
			heldAfter := activeReservation(t, q.s, def, q.agent.ID)
			if heldBefore == nil {
				assert.Nil(t, heldAfter, "no reservation is created")
			} else {
				require.NotNil(t, heldAfter, "the queued stop keeps its reservation")
				assert.Equal(t, heldBefore.ID, heldAfter.ID)
			}
			pub := q.lastPublished()
			require.NotNil(t, pub, "the restored state is published")
			assert.Equal(t, store.RunIntentStopped, pub.RunIntent)
			assert.Contains(t, pub.Message, tc.message, "the published row carries the message the agent keeps")
			assert.Equal(t, got.Message, pub.Message, "the published row carries the message the agent keeps")
		})
	}
}

// A start whose outcome is unknown may have reached the runtime: its
// running intent and unconfirmed claim are kept.
func TestRefusedStartAfterQueuedStop_AmbiguousKeepsRunningIntent(t *testing.T) {
	q := newRefusedStartFixture(t, true)
	q.disp.start = func(ctx context.Context, a *store.Agent) error {
		return errors.New("connection reset by peer")
	}

	require.GreaterOrEqual(t, q.start(t), 500)

	got := getAgent(t, q.s, q.agent.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "an ambiguous start keeps the running intent")
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "an ambiguous start keeps the running intent")
	assert.NotContains(t, got.Message, "Start failed:")
}

// intentAfterReleaseStore writes a newer running intent for agentID right
// after the start claim is released (as a newer start would), once.
type intentAfterReleaseStore struct {
	store.Store
	t       *testing.T
	agentID string
	done    bool
}

func (w *intentAfterReleaseStore) ReleaseAgentStart(ctx context.Context, agentID, claimID, owner string) (bool, error) {
	held, err := w.Store.ReleaseAgentStart(ctx, agentID, claimID, owner)
	if agentID == w.agentID && !w.done {
		w.done = true
		_, serr := w.Store.SetRunIntent(ctx, agentID, store.RunIntentRunning)
		require.NoError(w.t, serr)
	}
	return held, err
}

// A newer intent written after the claim release wins: the revert is a
// compare-and-set on the refused start's intent time, so nothing is
// reverted and no message is written.
func TestRefusedStartAfterQueuedStop_NewerIntentKept(t *testing.T) {
	q := newRefusedStartFixture(t, true)
	w := &intentAfterReleaseStore{Store: q.s, t: t, agentID: q.agent.ID}
	q.srv.store = w
	q.srv.quotaService.store = w
	q.disp.start = func(ctx context.Context, a *store.Agent) error {
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}

	require.GreaterOrEqual(t, q.start(t), 400)

	require.True(t, w.done, "setup: the newer intent was written")
	got := getAgent(t, q.s, q.agent.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "the newer intent is kept")
	assert.NotContains(t, got.Message, "Start failed:", "no message for a revert that lost")
}

// A stale pin is refused before the start claim: no intent is written, so
// nothing needs restoring, and the queued stop still applies on the drain.
func TestRefusedStartAfterQueuedStop_StalePinRefusalWritesNothing(t *testing.T) {
	q := newRefusedStartFixture(t, true)
	stale := *q.agent
	stale.PinnedRuntimeTargetID = "target-1" // pinned, but the pin is incomplete

	err := q.srv.startAgentCore(context.Background(), &stale, StartOpts{Kind: store.StartClaimUser})
	var pc *PlacementConflictError
	require.ErrorAs(t, err, &pc, "the stale pin is refused")

	got := getAgent(t, q.s, q.agent.ID)
	assert.True(t, got.RunIntentMatches(store.RunIntentStopped, q.intentAt), "the queued stop's intent is untouched")
	assert.Empty(t, got.StartClaimID)

	q.srv.drainBrokerDispatch(context.Background(), q.brokerID, func(op string) bool { return op == "stop" })
	assert.Equal(t, int32(1), q.disp.stops.Load(), "the queued stop is applied on the drain")
	assert.Equal(t, "stopped", getAgent(t, q.s, q.agent.ID).ContainerStatus)
}

// The next lifecycle action after the refused start works as for any queued
// stop: a start runs and replaces the queued status, a stop records a newer
// stop intent.
func TestRefusedStartAfterQueuedStop_NextActionUnaffected(t *testing.T) {
	refuse := func(q *refusedStartFixture) {
		q.disp.start = func(ctx context.Context, a *store.Agent) error {
			return fmt.Errorf("x: %w", errStartRequestNotSent)
		}
		require.GreaterOrEqual(t, q.start(t), 400)
		require.Equal(t, store.RunIntentStopped, getAgent(t, q.s, q.agent.ID).RunIntent, "setup: the refused start restored the stopped intent")
	}
	t.Run("start", func(t *testing.T) {
		q := newRefusedStartFixture(t, true)
		refuse(q)
		q.disp.start = nil
		require.Equal(t, http.StatusOK, q.start(t))
		got := getAgent(t, q.s, q.agent.ID)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent)
		assert.Equal(t, "running", got.ContainerStatus, "the start replaces the queued status")
		assert.Empty(t, got.Message, "the start clears the refusal")
	})
	t.Run("stop", func(t *testing.T) {
		q := newRefusedStartFixture(t, true)
		refuse(q)
		refused := getAgent(t, q.s, q.agent.ID)
		code, _ := lifecycle(t, q.reconcileFixture, q.agent.ID, "stop")
		require.Less(t, code, 300)
		got := getAgent(t, q.s, q.agent.ID)
		assert.Equal(t, store.RunIntentStopped, got.RunIntent)
		require.NotNil(t, got.RunIntentAt)
		assert.True(t, got.RunIntentAt.After(*refused.RunIntentAt), "the stop records a newer intent")
	})
}

// After the refusal, a fresh complete inventory that no longer lists the
// container confirms the queued stop: the reservation is released and the
// container status recorded stopped. The refusal message is kept.
func TestRefusedStartAfterQueuedStop_SettleConfirmsOnInventory(t *testing.T) {
	q := newRefusedStartFixture(t, true)
	q.disp.start = func(ctx context.Context, a *store.Agent) error {
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}
	require.GreaterOrEqual(t, q.start(t), 400)

	q.heartbeat(completeInventory())

	got := getAgent(t, q.s, q.agent.ID)
	assert.Equal(t, "stopped", got.ContainerStatus, "the settle confirms the queued stop")
	assert.False(t, reserved(t, q.reconcileFixture, q.agent.ID), "the settle releases the slot")
	assert.Contains(t, got.Message, "Start failed:")
}

// pinCheckingStopDispatcher records, at each stop, the intent the agent row
// holds and its claim kind.
type pinCheckingStopDispatcher struct {
	*claimTestDispatcher
	t        *testing.T
	s        store.Store
	agentID  string
	intentAt time.Time
}

func (d *pinCheckingStopDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	if a.ID == d.agentID {
		cur, err := d.s.GetAgent(ctx, a.ID)
		require.NoError(d.t, err)
		assert.Equal(d.t, store.StartClaimStop, cur.StartClaimKind, "the backstop stops under a stop claim")
		assert.True(d.t, cur.RunIntentMatches(store.RunIntentStopped, d.intentAt), "pinned to the restored intent")
	}
	return d.claimTestDispatcher.DispatchAgentStop(ctx, a)
}

// After the refusal, a container the heartbeat still lists as running is
// stopped by the intent backstop, once, under a stop claim pinned to the
// restored intent; the settle then confirms the stop once it is gone.
func TestRefusedStartAfterQueuedStop_BackstopStopsRunningContainer(t *testing.T) {
	q := newRefusedStartFixture(t, true)
	q.disp.start = func(ctx context.Context, a *store.Agent) error {
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}
	require.GreaterOrEqual(t, q.start(t), 400)
	refused := getAgent(t, q.s, q.agent.ID)
	require.NotNil(t, refused.RunIntentAt)
	// The queued row drains as superseded (its intent time is the older
	// one), so no lifecycle row is pending for the agent.
	q.srv.drainBrokerDispatch(context.Background(), q.brokerID, func(op string) bool { return op == "stop" })
	require.Equal(t, 0, pendingStops(t, q.reconcileFixture))
	require.Equal(t, int32(0), q.disp.stops.Load(), "the superseded row stops nothing")
	pd := &pinCheckingStopDispatcher{claimTestDispatcher: q.disp, t: t, s: q.s, agentID: q.agent.ID, intentAt: *refused.RunIntentAt}
	q.srv.SetDispatcher(pd)

	q.heartbeat(completeInventory(), q.agent.Slug)
	require.Eventually(t, func() bool { return q.disp.stops.Load() == 1 }, 5*time.Second, 10*time.Millisecond, "the backstop stops the running container")
	require.Eventually(t, func() bool { return getAgent(t, q.s, q.agent.ID).StartClaimID == "" }, 5*time.Second, 10*time.Millisecond, "the backstop's claim is released")

	q.heartbeat(completeInventory())
	got := getAgent(t, q.s, q.agent.ID)
	assert.Equal(t, "stopped", got.ContainerStatus, "the settle confirms the stop")
	assert.False(t, reserved(t, q.reconcileFixture, q.agent.ID))
	assert.Equal(t, int32(1), q.disp.stops.Load())
}

// The drain runs while the start claim is held and records the queued row
// superseded; the start is then refused. With no row left to apply, the
// restored intent lets the backstop and the settle finish the stop.
func TestRefusedStartAfterQueuedStop_DrainDuringClaim(t *testing.T) {
	q := newRefusedStartFixture(t, true)
	q.disp.start = func(ctx context.Context, a *store.Agent) error {
		q.srv.drainBrokerDispatch(context.Background(), q.brokerID, func(op string) bool { return op == "stop" })
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}
	require.GreaterOrEqual(t, q.start(t), 400)
	require.Equal(t, 0, pendingStops(t, q.reconcileFixture), "the drain recorded the queued row superseded")
	require.Equal(t, int32(0), q.disp.stops.Load())
	require.Equal(t, store.RunIntentStopped, getAgent(t, q.s, q.agent.ID).RunIntent, "the refused start restored the stopped intent")

	q.heartbeat(completeInventory(), q.agent.Slug)
	require.Eventually(t, func() bool { return q.disp.stops.Load() == 1 }, 5*time.Second, 10*time.Millisecond, "the backstop stops the running container")
	require.Eventually(t, func() bool { return getAgent(t, q.s, q.agent.ID).StartClaimID == "" }, 5*time.Second, 10*time.Millisecond)

	q.heartbeat(completeInventory())
	assert.Equal(t, "stopped", getAgent(t, q.s, q.agent.ID).ContainerStatus, "the settle confirms the stop")
	assert.False(t, reserved(t, q.reconcileFixture, q.agent.ID))
}

// A restart of a stop-queued agent whose start leg is refused keeps the
// restart's running intent: the restart's own failure branch records the
// stopped state (its stop leg stopped the old container), so the revert is
// for starts only.
func TestRestartOfQueuedStopAgent_ConfirmedStartLegFailure_KeepsRunningIntent(t *testing.T) {
	q := newRefusedStartFixture(t, true)
	_, err := q.s.SetAgentRunID(context.Background(), q.agent.ID, "run-x", nil)
	require.NoError(t, err)
	sawQueued := false
	q.disp.start = func(ctx context.Context, a *store.Agent) error {
		sawQueued = a.ContainerStatus == containerStatusStopQueued
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}

	code, _ := lifecycle(t, q.reconcileFixture, q.agent.ID, "restart")
	require.GreaterOrEqual(t, code, 400)

	require.True(t, sawQueued, "setup: the start leg ran with the agent still reading stop_queued")
	got := getAgent(t, q.s, q.agent.ID)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "a restart keeps the running intent")
	assert.Equal(t, "stopped", got.ContainerStatus, "the restart records the stopped state")
}
