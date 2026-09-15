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
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
		Visibility:      store.VisibilityPrivate,
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
