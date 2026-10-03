//go:build !no_sqlite

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

package hub

// Tests for ptone/scion#1906: with a broker proxy configured, a user
// notification is persisted (and announced over SSE) exactly once. These use
// a real, started MessageBrokerProxy on an in-process bus, so the broker's
// deliverToUser subscriber actually runs.

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notificationSettle is how long to keep watching after the first row
// appears, so a late duplicate (the bus delivers asynchronously) is caught.
const notificationSettle = 300 * time.Millisecond

// startRealBrokerProxy wires a started MessageBrokerProxy on an in-process
// bus into env.nd and returns the bus wrapper recording every publish.
func (env *notificationTestEnv) startRealBrokerProxy(t *testing.T) *capturingBus {
	t.Helper()
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	bus := &capturingBus{EventBus: inner}
	proxy := NewMessageBrokerProxy(bus, env.store, env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	env.nd.SetBrokerProxy(proxy)
	return bus
}

// useUserSubscription replaces the default agent subscription with a user
// subscription for subscriberID on the given trigger.
func (env *notificationTestEnv) useUserSubscription(t *testing.T, subscriberID, trigger string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      subscriberID,
		ProjectID:         env.project.ID,
		TriggerActivities: []string{trigger},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}))
}

// userMessageSSECounter counts user.message SSE events for one user.
type userMessageSSECounter struct {
	mu   sync.Mutex
	evts []Event
}

func (env *notificationTestEnv) countUserMessageSSE(t *testing.T, userID string) *userMessageSSECounter {
	t.Helper()
	c := &userMessageSSECounter{}
	ch, unsub := env.pub.Subscribe("user." + userID + ".message")
	// unsub does not close ch, so stop the reader explicitly.
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case evt := <-ch:
				c.mu.Lock()
				c.evts = append(c.evts, evt)
				c.mu.Unlock()
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { unsub(); close(stop); <-done })
	return c
}

func (c *userMessageSSECounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.evts)
}

// settledUserRows waits for the first inbox row for userID, then for the
// settle window, and returns every row found.
func (env *notificationTestEnv) settledUserRows(t *testing.T, userID string) []store.Message {
	t.Helper()
	list := func() []store.Message {
		res, err := env.store.ListMessages(context.Background(), store.MessageFilter{
			RecipientID: userID,
			ProjectID:   env.project.ID,
		}, store.ListOptions{})
		require.NoError(t, err)
		return res.Items
	}
	require.Eventually(t, func() bool { return len(list()) > 0 }, 3*time.Second, 10*time.Millisecond,
		"the notification must be persisted to the user's inbox")
	time.Sleep(notificationSettle)
	return list()
}

func TestNotificationDispatcher_RealBrokerPersistsUserNotificationOnce(t *testing.T) {
	env := setupNotificationTest(t)
	userID := api.NewUUID()
	require.NoError(t, env.store.CreateUser(context.Background(), &store.User{ID: userID, Email: "once@example.com", DisplayName: "Once"}))
	env.useUserSubscription(t, userID, "COMPLETED")
	bus := env.startRealBrokerProxy(t)
	sse := env.countUserMessageSSE(t, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1, "exactly one inbox row per notification")
	assert.Equal(t, "agent:watched-agent", rows[0].Sender)
	assert.Equal(t, messages.TypeStateChange, rows[0].Type)
	assert.NotEmpty(t, rows[0].ConversationID, "the broker path resolves the DM conversation")
	assert.Equal(t, 1, sse.count(), "exactly one user.message SSE publish per notification")
	require.Len(t, bus.published(), 1, "external plugins still receive the notification")
}

func TestNotificationDispatcher_RealBrokerKeepsWaitingForInputBody(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Activity: "waiting_for_input",
		Message:  "What branch should I target?",
	}))
	userID := api.NewUUID()
	require.NoError(t, env.store.CreateUser(ctx, &store.User{ID: userID, Email: "wfi@example.com", DisplayName: "WFI"}))
	env.useUserSubscription(t, userID, "WAITING_FOR_INPUT")
	bus := env.startRealBrokerProxy(t)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("waiting_for_input")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1)
	assert.Equal(t, "What branch should I target?", rows[0].Msg,
		"the broker path must persist the agent's raw question, as the inbox path does")
	assert.Equal(t, messages.TypeInputNeeded, rows[0].Type)
	published := bus.published()
	require.Len(t, published, 1)
	assert.Equal(t, rows[0].Msg, published[0].Msg)
}

// A federated (non-UUID) subscriber under G2 write-deny: deliverToUser drops
// it, so the notifier keeps the G2-exempt inbox write — still exactly once.
func TestNotificationDispatcher_RealBrokerFederatedSubscriberWriteDenyOnce(t *testing.T) {
	env := setupNotificationTest(t)
	const userID = "fed-user@example.org"
	env.useUserSubscription(t, userID, "COMPLETED")
	bus := env.startRealBrokerProxy(t)
	writeDeny := func() bool { return true }
	env.nd.writeDenyEnabled = writeDeny
	env.nd.brokerProxy.writeDenyEnabled = writeDeny
	sse := env.countUserMessageSSE(t, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1, "exactly one inbox row for a federated subscriber")
	assert.Equal(t, 1, sse.count())
	require.Len(t, bus.published(), 1, "external plugins still receive the notification")
}

// If the publish cannot reach the broker's persisting subscriber at all, the
// notifier persists directly instead of losing the inbox row.
func TestNotificationDispatcher_BrokerPublishFailureFallsBackToInbox(t *testing.T) {
	env := setupNotificationTest(t)
	userID := api.NewUUID()
	env.useUserSubscription(t, userID, "COMPLETED")
	bus := env.startRealBrokerProxy(t)
	require.NoError(t, bus.EventBus.Close()) // every publish now fails
	sse := env.countUserMessageSSE(t, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, sse.count())
}
