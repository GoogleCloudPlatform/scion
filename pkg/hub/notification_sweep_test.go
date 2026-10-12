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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupRetryTest creates a test environment with a pre-existing undispatched
// notification. The notification is backdated beyond the 60s grace period so
// the sweep will pick it up.
func setupRetryTest(t *testing.T) *notificationTestEnv {
	t.Helper()
	env := setupNotificationTest(t)

	// Create a notification record manually (simulating a previous dispatch
	// that failed because subscriber had no broker).
	ctx := context.Background()
	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: env.sub.ID,
		AgentID:        env.watched.ID,
		ProjectID:      env.project.ID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   env.subscriber.Slug,
		Status:         "COMPLETED",
		Message:        "watched-agent has reached a state of COMPLETED",
		Dispatched:     false,
		CreatedAt:      time.Now().Add(-2 * time.Minute), // beyond 60s grace period
	}
	require.NoError(t, env.store.CreateNotification(ctx, notif))

	// Stash for test assertions
	env.sub.AgentID = env.watched.ID // ensure AgentID is available
	return env
}

func TestRetryDispatch_ClaimAndDeliver(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	// Fetch the undispatched notification
	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.False(t, notifs[0].Dispatched)

	// Retry dispatch
	env.nd.RetryDispatch(ctx, &notifs[0])

	// Verify dispatch was called
	calls := env.dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, env.subscriber.ID, calls[0].Agent.ID)
	assert.Contains(t, calls[0].Message, "watched-agent has reached a state of COMPLETED")

	// Verify notification is now marked dispatched
	allNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	// Find our notification (there may be others from setup)
	var found bool
	for _, n := range allNotifs {
		if n.ID == notifs[0].ID {
			assert.True(t, n.Dispatched, "notification should be marked dispatched after retry")
			found = true
			break
		}
	}
	assert.True(t, found, "notification should still exist")
}

func TestRetryDispatch_AlreadyClaimed(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	// Claim the notification first (simulating another caller)
	claimed, err := env.store.ClaimNotificationForDispatch(ctx, notifs[0].ID)
	require.NoError(t, err)
	assert.True(t, claimed)

	// Now retry — should be a no-op
	env.nd.RetryDispatch(ctx, &notifs[0])

	// No dispatch call should have been made
	assert.Empty(t, env.dispatcher.getCalls())
}

func TestRetryDispatch_NoBroker_Reverts(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	// Fetch the notification while subscriber still has a broker (simulates
	// the race where the sweep query ran before the broker disconnected).
	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	// Now remove subscriber's broker (simulates broker disconnect between
	// query time and dispatch time).
	env.subscriber.RuntimeBrokerID = ""
	require.NoError(t, env.store.UpdateAgent(ctx, env.subscriber))

	// Retry — should claim, then revert because no broker
	env.nd.RetryDispatch(ctx, &notifs[0])

	// No dispatch call
	assert.Empty(t, env.dispatcher.getCalls())

	// Notification should be back to undispatched
	allNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	for _, n := range allNotifs {
		if n.ID == notifs[0].ID {
			assert.False(t, n.Dispatched, "notification should be reverted to undispatched")
		}
	}
}

func TestRetryDispatch_NoDispatcher_Reverts(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	// Create a dispatcher that returns nil
	ndNoDispatcher := NewNotificationDispatcher(
		env.store, env.pub,
		func() AgentDispatcher { return nil },
		slog.Default(),
	)

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	// Retry — should claim, then revert because no dispatcher
	ndNoDispatcher.RetryDispatch(ctx, &notifs[0])

	// No dispatch call
	assert.Empty(t, env.dispatcher.getCalls())

	// Notification should be back to undispatched
	allNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	for _, n := range allNotifs {
		if n.ID == notifs[0].ID {
			assert.False(t, n.Dispatched, "notification should be reverted to undispatched")
		}
	}
}

func TestRetryDispatch_SubscriberDeleted(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	// Delete the subscriber agent
	require.NoError(t, env.store.DeleteAgent(ctx, env.subscriber.ID))

	// Retry — should claim, then find subscriber permanently deleted (ErrNotFound).
	// The notification should remain claimed (dispatched=true) so the sweep
	// does not retry it every 5 minutes forever.
	env.nd.RetryDispatch(ctx, &notifs[0])

	// No dispatch call
	assert.Empty(t, env.dispatcher.getCalls())

	// Notification should remain claimed (dispatched=true) — subscriber is gone
	allNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	for _, n := range allNotifs {
		if n.ID == notifs[0].ID {
			assert.True(t, n.Dispatched, "notification should stay claimed when subscriber is permanently deleted")
		}
	}
}

func TestRetryDispatch_WatchedAgentDeleted(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	// Delete the watched agent (the one that triggered the notification)
	require.NoError(t, env.store.DeleteAgent(ctx, env.watched.ID))

	// Retry — should claim, look up subscriber (still exists), but fail to
	// resolve the watched agent. Notification remains claimed (dispatched=true)
	// because the event source is gone and there is no point retrying.
	env.nd.RetryDispatch(ctx, &notifs[0])

	// No dispatch call
	assert.Empty(t, env.dispatcher.getCalls())

	// Notification should remain claimed
	allNotifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	for _, n := range allNotifs {
		if n.ID == notifs[0].ID {
			assert.True(t, n.Dispatched, "notification should stay claimed when watched agent is deleted")
		}
	}
}

func TestSweepHandler_QueriesAndRetries(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	// Simulate server with just the notification dispatcher
	srv := &Server{
		store:                  env.store,
		agentLifecycleLog:      slog.Default(),
		notificationDispatcher: env.nd,
	}

	// Run the sweep handler
	handler := srv.notificationDispatchSweepHandler()
	handler(ctx)

	// Verify dispatch was called
	calls := env.dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, env.subscriber.ID, calls[0].Agent.ID)

	// Verify notification is now dispatched
	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, notifs, "no undispatched notifications should remain")
}

func TestSweepHandler_EmptyResultNoOp(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	// No undispatched notifications exist — just the test subscription
	srv := &Server{
		store:                  env.store,
		agentLifecycleLog:      slog.Default(),
		notificationDispatcher: env.nd,
	}

	handler := srv.notificationDispatchSweepHandler()
	handler(ctx)

	// No dispatch calls
	assert.Empty(t, env.dispatcher.getCalls())
}

func TestDrainUndispatchedNotifications_FiltersByBroker(t *testing.T) {
	env := setupRetryTest(t)
	ctx := context.Background()

	// Create a second agent with a different broker, with an undispatched notification
	broker2 := &store.RuntimeBroker{
		ID:     tid("broker-2"),
		Name:   "Other Broker",
		Slug:   "other-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, env.store.CreateRuntimeBroker(ctx, broker2))

	otherSubscriber := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "other-subscriber",
		Name:            "Other Subscriber",
		Template:        "claude",
		ProjectID:       env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: tid("broker-2"),
	}
	require.NoError(t, env.store.CreateAgent(ctx, otherSubscriber))

	otherSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      otherSubscriber.Slug,
		ProjectID:         env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, otherSub))

	otherNotif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: otherSub.ID,
		AgentID:        env.watched.ID,
		ProjectID:      env.project.ID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   otherSubscriber.Slug,
		Status:         "COMPLETED",
		Message:        "watched-agent has reached a state of COMPLETED",
		Dispatched:     false,
		CreatedAt:      time.Now().Add(-2 * time.Minute),
	}
	require.NoError(t, env.store.CreateNotification(ctx, otherNotif))

	// Drain for broker-1 only
	srv := &Server{
		store:                  env.store,
		agentLifecycleLog:      slog.Default(),
		notificationDispatcher: env.nd,
	}
	srv.drainUndispatchedNotifications(ctx, tid("broker-1"))

	// Should only deliver the notification for the subscriber on broker-1
	calls := env.dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, env.subscriber.ID, calls[0].Agent.ID)

	// The notification for broker-2's subscriber should still be undispatched
	remaining, err := env.store.GetUndispatchedAgentNotifications(ctx, tid("broker-2"))
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, otherSubscriber.Slug, remaining[0].SubscriberID)
}

// TestDrainUndispatchedNotifications_PicksUpRecentNotifications verifies that
// the broker-connect hook (non-empty brokerID) does NOT apply the 60s grace
// period. This is the R1 fix: a notification created just seconds ago should
// be drained immediately when the broker connects.
func TestDrainUndispatchedNotifications_PicksUpRecentNotifications(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	// Create a very recent undispatched notification (within the 60s grace
	// period that the sweep would skip).
	recentNotif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: env.sub.ID,
		AgentID:        env.watched.ID,
		ProjectID:      env.project.ID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   env.subscriber.Slug,
		Status:         "COMPLETED",
		Message:        "watched-agent has reached a state of COMPLETED",
		Dispatched:     false,
		// CreatedAt defaults to time.Now() — well within the 60s grace period
	}
	require.NoError(t, env.store.CreateNotification(ctx, recentNotif))

	// Verify sweep mode does NOT see this notification (it's too recent).
	sweepNotifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, sweepNotifs, "sweep should not see notifications within grace period")

	// Verify broker-connect mode DOES see this notification.
	brokerNotifs, err := env.store.GetUndispatchedAgentNotifications(ctx, tid("broker-1"))
	require.NoError(t, err)
	require.Len(t, brokerNotifs, 1, "broker drain should see recent notifications")
	assert.Equal(t, env.subscriber.Slug, brokerNotifs[0].SubscriberID)

	// Drain should deliver it.
	srv := &Server{
		store:                  env.store,
		agentLifecycleLog:      slog.Default(),
		notificationDispatcher: env.nd,
	}
	srv.drainUndispatchedNotifications(ctx, tid("broker-1"))

	calls := env.dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, env.subscriber.ID, calls[0].Agent.ID)
	assert.Contains(t, calls[0].Message, "watched-agent has reached a state of COMPLETED")
}

// The orphan GC handler removes an acknowledged notification whose agent
// and subscription are gone and keeps an unacknowledged one.
func TestNotificationOrphanGCHandler(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: api.NewUUID(), Name: "orphan-gc", Slug: "orphan-gc"}
	require.NoError(t, s.CreateProject(ctx, project))

	create := func(acked bool) string {
		id := api.NewUUID()
		require.NoError(t, s.CreateNotification(ctx, &store.Notification{
			ID: id, SubscriptionID: api.NewUUID(), AgentID: api.NewUUID(), ProjectID: project.ID,
			SubscriberType: "user", SubscriberID: "user-1", Status: "DELETED", Message: "deleted",
		}))
		if acked {
			require.NoError(t, s.AcknowledgeNotification(ctx, id))
		}
		return id
	}
	acked, unacked := create(true), create(false)

	srv.notificationOrphanGCHandler()(ctx)

	_, err := s.GetNotification(ctx, acked)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetNotification(ctx, unacked)
	assert.NoError(t, err)
}

// notificationDispatched returns the dispatched flag of the notification
// with the given ID.
func notificationDispatched(t *testing.T, env *notificationTestEnv, id string) bool {
	t.Helper()
	n, err := env.store.GetNotification(context.Background(), id)
	require.NoError(t, err)
	return n.Dispatched
}

// setReturnErr sets the error DispatchAgentMessage returns, under the lock.
func (d *recordingDispatcher) setReturnErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.returnErr = err
}

// brokerDeferringDispatcher records calls like recordingDispatcher and
// returns ErrMessageDeferred for agents on the listed brokers, as the hub
// does when a broker cannot be routed to.
type brokerDeferringDispatcher struct {
	recordingDispatcher
	deferBrokers map[string]bool
}

func (d *brokerDeferringDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	err := d.recordingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
	if d.deferBrokers[agent.RuntimeBrokerID] {
		return ErrMessageDeferred
	}
	return err
}

// callsFor counts the recorded dispatch calls to the given agent.
func (d *brokerDeferringDispatcher) callsFor(agentID string) int {
	n := 0
	for _, c := range d.getCalls() {
		if c.Agent.ID == agentID {
			n++
		}
	}
	return n
}

// addSubscriberOnBroker creates an online broker and a subscriber agent on
// it, subscribed to env.watched, and returns the agent and subscription.
func addSubscriberOnBroker(t *testing.T, env *notificationTestEnv, name string) (*store.Agent, *store.NotificationSubscription) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:     tid(name + "-broker"),
		Name:   name + " broker",
		Slug:   name + "-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, env.store.CreateRuntimeBroker(ctx, broker))
	subscriber := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            name,
		Name:            name,
		Template:        "claude",
		ProjectID:       env.project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, env.store.CreateAgent(ctx, subscriber))
	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      subscriber.Slug,
		ProjectID:         env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Hour),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, sub))
	return subscriber, sub
}

// createUndispatchedNotification stores an undispatched notification for
// sub, created age ago, and returns its ID.
func createUndispatchedNotification(t *testing.T, env *notificationTestEnv, sub *store.NotificationSubscription, age time.Duration) string {
	t.Helper()
	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        env.watched.ID,
		ProjectID:      env.project.ID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   sub.SubscriberID,
		Status:         "COMPLETED",
		Message:        "watched-agent has reached a state of COMPLETED",
		CreatedAt:      time.Now().Add(-age),
	}
	require.NoError(t, env.store.CreateNotification(context.Background(), notif))
	return notif.ID
}

// setRetryAttemptTimeout overrides retryDispatchAttemptTimeout for one test.
func setRetryAttemptTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := retryDispatchAttemptTimeout
	retryDispatchAttemptTimeout = d
	t.Cleanup(func() { retryDispatchAttemptTimeout = orig })
}

// A failed retry delivery releases its claim so the next sweep retries it
// (ptone/scion#2743).
func TestRetryDispatch_DeliveryFailure_RevertsClaim(t *testing.T) {
	env := setupRetryTest(t)
	env.dispatcher.setReturnErr(errors.New("broker refused"))
	ctx := context.Background()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	env.nd.RetryDispatch(ctx, &notifs[0])

	require.Len(t, env.dispatcher.getCalls(), 1)
	assert.False(t, notificationDispatched(t, env, notifs[0].ID),
		"a failed retry delivery must release its claim")

	again, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, again, 1, "the next sweep must see the notification again")
	assert.Equal(t, notifs[0].ID, again[0].ID)
}

// A notification older than notificationRetryMaxAge whose delivery fails
// stays claimed, so retries are bounded.
func TestRetryDispatch_DeliveryFailure_PastMaxAge_StaysClaimed(t *testing.T) {
	env := setupNotificationTest(t)
	env.dispatcher.setReturnErr(errors.New("broker refused"))
	ctx := context.Background()

	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: env.sub.ID,
		AgentID:        env.watched.ID,
		ProjectID:      env.project.ID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   env.subscriber.Slug,
		Status:         "COMPLETED",
		Message:        "watched-agent has reached a state of COMPLETED",
		CreatedAt:      time.Now().Add(-notificationRetryMaxAge - time.Hour),
	}
	require.NoError(t, env.store.CreateNotification(ctx, notif))

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	env.nd.RetryDispatch(ctx, &notifs[0])

	require.Len(t, env.dispatcher.getCalls(), 1)
	assert.True(t, notificationDispatched(t, env, notif.ID),
		"a failed delivery past the max retry age must stay claimed")
}

// A delivery that keeps deferring (broker not reachable) is cut off by the
// per-attempt timeout instead of using the caller's whole budget, and its
// claim is released.
func TestRetryDispatch_DeferredDelivery_AttemptTimeoutRevertsClaim(t *testing.T) {
	setRetryAttemptTimeout(t, 200*time.Millisecond)

	env := setupRetryTest(t)
	env.dispatcher.setReturnErr(ErrMessageDeferred)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	start := time.Now()
	env.nd.RetryDispatch(ctx, &notifs[0])
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second,
		"the attempt must stop at retryDispatchAttemptTimeout, not the caller's deadline")
	require.NotEmpty(t, env.dispatcher.getCalls())
	assert.False(t, notificationDispatched(t, env, notifs[0].ID),
		"a timed-out retry delivery must release its claim")
}

// End to end through the sweep handler: a sweep whose delivery fails leaves
// the notification undispatched, and the next sweep delivers it.
func TestSweepHandler_FailedDeliveryRetriedNextSweep(t *testing.T) {
	env := setupRetryTest(t)
	env.dispatcher.setReturnErr(errors.New("broker refused"))
	ctx := context.Background()

	srv := &Server{
		store:                  env.store,
		agentLifecycleLog:      slog.Default(),
		notificationDispatcher: env.nd,
	}
	handler := srv.notificationDispatchSweepHandler()

	handler(ctx)
	require.Len(t, env.dispatcher.getCalls(), 1)
	pending, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, pending, 1, "a failed delivery must stay undispatched")

	env.dispatcher.setReturnErr(nil)

	handler(ctx)
	require.Len(t, env.dispatcher.getCalls(), 2)
	pending, err = env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, pending, "the next sweep must deliver it")
}

// A delivery attempt that outlives the caller's deadline still releases its
// claim: the revert does not run on the expired caller context.
func TestRetryDispatch_RevertSurvivesExpiredParentContext(t *testing.T) {
	setRetryAttemptTimeout(t, 5*time.Second)

	env := setupRetryTest(t)
	env.dispatcher.setReturnErr(ErrMessageDeferred)

	notifs, err := env.store.GetUndispatchedAgentNotifications(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	// The parent deadline ends the attempt (first backoff is 500ms), so the
	// revert runs after the parent context has expired.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	env.nd.RetryDispatch(ctx, &notifs[0])
	require.Error(t, ctx.Err(), "the parent context must have expired during the attempt")

	require.NotEmpty(t, env.dispatcher.getCalls())
	assert.False(t, notificationDispatched(t, env, notifs[0].ID),
		"the claim must be released even after the caller's deadline")
}

// The batch starts no attempt when less than one attempt timeout is left in
// the caller's budget; the notification stays undispatched (and unclaimed)
// for the next pass.
func TestSweepHandler_StopsWhenBudgetBelowAttemptTimeout(t *testing.T) {
	setRetryAttemptTimeout(t, 200*time.Millisecond)

	env := setupNotificationTest(t)
	_, subB := addSubscriberOnBroker(t, env, "subscriber-b")
	d := &brokerDeferringDispatcher{deferBrokers: map[string]bool{
		env.subscriber.RuntimeBrokerID: true,
		tid("subscriber-b-broker"):     true,
	}}
	nd := NewNotificationDispatcher(env.store, env.pub, func() AgentDispatcher { return d }, slog.Default())
	srv := &Server{store: env.store, agentLifecycleLog: slog.Default(), notificationDispatcher: nd}

	first := createUndispatchedNotification(t, env, env.sub, 3*time.Minute)
	second := createUndispatchedNotification(t, env, subB, 2*time.Minute)

	// 300ms budget: the first attempt (200ms) runs, then less than 200ms is
	// left, so the second (on another broker, not skipped) does not start.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	srv.notificationDispatchSweepHandler()(ctx)

	assert.Len(t, d.getCalls(), 1, "no attempt may start with less than one attempt timeout left")
	assert.False(t, notificationDispatched(t, env, first), "the attempted notification must be reverted")
	assert.False(t, notificationDispatched(t, env, second), "the unattempted notification must stay undispatched")
}

// A broker found unreachable is skipped for the rest of the pass, so older
// notifications for an offline broker do not block a newer one for an
// online broker (ptone/scion#2743).
func TestSweepHandler_SkipsUnreachableBrokerForRestOfPass(t *testing.T) {
	setRetryAttemptTimeout(t, 200*time.Millisecond)

	env := setupNotificationTest(t)
	subscriberB, subB := addSubscriberOnBroker(t, env, "subscriber-b")
	d := &brokerDeferringDispatcher{deferBrokers: map[string]bool{
		env.subscriber.RuntimeBrokerID: true, // broker A is offline
	}}
	nd := NewNotificationDispatcher(env.store, env.pub, func() AgentDispatcher { return d }, slog.Default())
	srv := &Server{store: env.store, agentLifecycleLog: slog.Default(), notificationDispatcher: nd}

	// Oldest first: two for broker A, then one for broker B.
	a1 := createUndispatchedNotification(t, env, env.sub, 4*time.Minute)
	a2 := createUndispatchedNotification(t, env, env.sub, 3*time.Minute)
	b1 := createUndispatchedNotification(t, env, subB, 2*time.Minute)

	srv.notificationDispatchSweepHandler()(context.Background())

	assert.Equal(t, 1, d.callsFor(env.subscriber.ID), "the offline broker must be attempted once per pass")
	assert.Equal(t, 1, d.callsFor(subscriberB.ID), "the online broker's notification must be attempted")
	assert.True(t, notificationDispatched(t, env, b1), "the online broker's notification must be delivered in the same pass")
	assert.False(t, notificationDispatched(t, env, a1), "the attempted offline notification must be reverted")
	assert.False(t, notificationDispatched(t, env, a2), "the skipped offline notification must stay undispatched")
}
