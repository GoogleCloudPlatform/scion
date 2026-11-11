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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// recordingDispatcher is a mock AgentDispatcher that records DispatchAgentMessage calls.
type recordingDispatcher struct {
	mu        sync.Mutex
	calls     []dispatchCall
	returnErr error
}

type dispatchCall struct {
	Agent             *store.Agent
	Message           string
	Interrupt         bool
	StructuredMessage *messages.StructuredMessage
	// MessageID is the hub message ID carried on the dispatch context (#1820).
	MessageID string
}

// notificationTestEnv holds all components for a notification test.
type notificationTestEnv struct {
	store      store.Store
	pub        *ChannelEventPublisher
	dispatcher *recordingDispatcher
	nd         *NotificationDispatcher
	project    *store.Project
	watched    *store.Agent // the agent being watched
	subscriber *store.Agent // the agent receiving notifications
	sub        *store.NotificationSubscription
	// quiesceBroker, when set (startRealBrokerProxy), drains and closes the
	// broker bus so every asynchronous deliverToUser has finished.
	quiesceBroker func()
}

// setupNotificationTest creates an in-memory SQLite store, event publisher,
// recording dispatcher, project, watched agent, subscriber agent, and subscription.
func setupNotificationTest(t *testing.T) *notificationTestEnv {
	t.Helper()

	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)

	dispatcher := &recordingDispatcher{}

	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "Notification Test Project",
		Slug: "notif-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     tid("broker-1"),
		Name:   "Test Broker",
		Slug:   "test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	watched := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "watched-agent",
		Name:            "Watched Agent",
		Template:        "claude",
		ProjectID:       project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: tid("broker-1"),
	}
	require.NoError(t, s.CreateAgent(ctx, watched))

	subscriber := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "subscriber-agent",
		Name:            "Subscriber Agent",
		Template:        "claude",
		ProjectID:       project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: tid("broker-1"),
	}
	require.NoError(t, s.CreateAgent(ctx, subscriber))

	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           watched.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      subscriber.Slug,
		ProjectID:         project.ID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedAt:         time.Now().Add(-time.Minute), // Predate agent creation so the stale event filter doesn't skip test events
		CreatedBy:         "test",
	}
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))

	nd := NewNotificationDispatcher(s, pub, func() AgentDispatcher { return dispatcher }, slog.Default())

	return &notificationTestEnv{
		store:      s,
		pub:        pub,
		dispatcher: dispatcher,
		nd:         nd,
		project:    project,
		watched:    watched,
		subscriber: subscriber,
		sub:        sub,
	}
}

func (d *recordingDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, dispatchCall{Agent: agent, Message: message, Interrupt: interrupt, StructuredMessage: structuredMsg, MessageID: dispatchMessageIDFromContext(ctx)})
	return d.returnErr
}

func (d *recordingDispatcher) getCalls() []dispatchCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]dispatchCall, len(d.calls))
	copy(result, d.calls)
	return result
}

// Implement remaining AgentDispatcher methods as no-ops.
func (d *recordingDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *recordingDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}

func (d *recordingDispatcher) DispatchAgentReprovision(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error { return nil }
func (d *recordingDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *recordingDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *recordingDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *recordingDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *recordingDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *recordingDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

// startRealBrokerProxy wires a started MessageBrokerProxy on an in-process
// bus into env.nd and returns the bus wrapper recording every publish.
func (env *notificationTestEnv) startRealBrokerProxy(t *testing.T) *capturingBus {
	t.Helper()
	inner := eventbus.NewInProcessEventBus(slog.Default())
	bus := &capturingBus{EventBus: inner}
	env.startBrokerProxyOn(t, bus, env.pub, func() { _ = inner.Close() })
	return bus
}

// startBrokerProxyOn starts a MessageBrokerProxy on bus, publishing its SSE
// and receiving lifecycle events on events, and wires it into env.nd.
// closeBus must drain and close the bus; it becomes env.quiesceBroker.
func (env *notificationTestEnv) startBrokerProxyOn(t *testing.T, bus eventbus.EventBus, events *ChannelEventPublisher, closeBus func()) *MessageBrokerProxy {
	t.Helper()
	t.Cleanup(closeBus)
	proxy := NewMessageBrokerProxy(bus, env.store, events, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	env.nd.SetBrokerProxy(proxy)
	env.quiesceBroker = closeBus
	return proxy
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

func (env *notificationTestEnv) countUserMessageSSE(t *testing.T, userID string) *userMessageSSECounter {
	t.Helper()
	return countUserMessageSSE(t, env.pub, userID)
}

func (env *notificationTestEnv) listUserRows(t *testing.T, userID string) []store.Message {
	t.Helper()
	res, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		RecipientID: userID,
		ProjectID:   env.project.ID,
	}, store.ListOptions{})
	require.NoError(t, err)
	return res.Items
}

// settledUserRows waits for the first inbox row for userID, then quiesces
// both writers before counting, so a late duplicate cannot be missed:
// nd.Stop waits for the in-flight storeAndDispatch (the notifier's own
// write and its publish), and closing the broker bus drains every queued
// deliverToUser. No settle sleep is involved.
func (env *notificationTestEnv) settledUserRows(t *testing.T, userID string) []store.Message {
	t.Helper()
	require.Eventually(t, func() bool { return len(env.listUserRows(t, userID)) > 0 }, 5*time.Second, 10*time.Millisecond,
		"the notification must be persisted to the user's inbox")
	env.nd.Stop()
	if env.quiesceBroker != nil {
		env.quiesceBroker()
	}
	return env.listUserRows(t, userID)
}

// publishStatus publishes an agent status event via the event publisher.
func (env *notificationTestEnv) publishStatus(activity string) {
	env.pub.PublishAgentStatus(context.Background(), &store.Agent{
		ID:        env.watched.ID,
		Slug:      env.watched.Slug,
		ProjectID: env.project.ID,
		Phase:     string(state.PhaseRunning),
		Activity:  activity,
	})
}

// publishStatusWithPhase publishes an agent status event with a specific phase and activity.
func (env *notificationTestEnv) publishStatusWithPhase(phase, activity string) {
	env.pub.PublishAgentStatus(context.Background(), &store.Agent{
		ID:        env.watched.ID,
		Slug:      env.watched.Slug,
		ProjectID: env.project.ID,
		Phase:     phase,
		Activity:  activity,
	})
}
