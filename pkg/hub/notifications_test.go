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
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/broker"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/stretchr/testify/assert"
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
}

func (d *recordingDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, dispatchCall{Agent: agent, Message: message, Interrupt: interrupt, StructuredMessage: structuredMsg})
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
func (d *recordingDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error { return nil }
func (d *recordingDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *recordingDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *recordingDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *recordingDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *recordingDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *recordingDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *recordingDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
}

// recordingBroker is a mock MessageBroker that records Publish calls.
type recordingBroker struct {
	mu        sync.Mutex
	publishes []brokerPublish
}

type brokerPublish struct {
	topic string
	msg   *messages.StructuredMessage
}

func (b *recordingBroker) Publish(_ context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishes = append(b.publishes, brokerPublish{topic: topic, msg: msg})
	return nil
}

func (b *recordingBroker) Subscribe(_ string, _ broker.MessageHandler) (broker.Subscription, error) {
	return &noopSubscription{}, nil
}

func (b *recordingBroker) Close() error { return nil }

func (b *recordingBroker) getPublishes() []brokerPublish {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]brokerPublish, len(b.publishes))
	copy(result, b.publishes)
	return result
}

type noopSubscription struct{}

func (s *noopSubscription) Unsubscribe() error { return nil }

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
}

// setupNotificationTest creates an in-memory SQLite store, event publisher,
// recording dispatcher, project, watched agent, subscriber agent, and subscription.
func setupNotificationTest(t *testing.T) *notificationTestEnv {
	t.Helper()

	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })

	pub := NewChannelEventPublisher()
	t.Cleanup(func() { pub.Close() })

	dispatcher := &recordingDispatcher{}

	ctx := context.Background()

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "Notification Test Project",
		Slug:       "notif-test-project",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-1",
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
		ProjectID:         project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, watched))

	subscriber := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            "subscriber-agent",
		Name:            "Subscriber Agent",
		Template:        "claude",
		ProjectID:         project.ID,
		Phase:           string(state.PhaseRunning),
		RuntimeBrokerID: "broker-1",
		Visibility:      store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, subscriber))

	sub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           watched.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      subscriber.Slug,
		ProjectID:           project.ID,
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
		project:      project,
		watched:    watched,
		subscriber: subscriber,
		sub:        sub,
	}
}

// publishStatus publishes an agent status event via the event publisher.
func (env *notificationTestEnv) publishStatus(activity string) {
	env.pub.PublishAgentStatus(context.Background(), &store.Agent{
		ID:       env.watched.ID,
		Slug:     env.watched.Slug,
		ProjectID:  env.project.ID,
		Phase:    string(state.PhaseRunning),
		Activity: activity,
	})
}

// publishStatusWithPhase publishes an agent status event with a specific phase and activity.
func (env *notificationTestEnv) publishStatusWithPhase(phase, activity string) {
	env.pub.PublishAgentStatus(context.Background(), &store.Agent{
		ID:       env.watched.ID,
		Slug:     env.watched.Slug,
		ProjectID:  env.project.ID,
		Phase:    phase,
		Activity: activity,
	})
}

func TestNotificationDispatcher_HappyPath(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	assert.Equal(t, env.subscriber.ID, calls[0].Agent.ID)
	assert.Contains(t, calls[0].Message, "watched-agent has reached a state of COMPLETED")
	assert.False(t, calls[0].Interrupt)

	// Verify structured message was produced
	sm := calls[0].StructuredMessage
	require.NotNil(t, sm, "structured message should be set")
	assert.Equal(t, "agent:watched-agent", sm.Sender)
	assert.Equal(t, "agent:subscriber-agent", sm.Recipient)
	assert.Equal(t, messages.TypeStateChange, sm.Type)
	assert.Contains(t, sm.Msg, "watched-agent has reached a state of COMPLETED")

	// Verify notification was stored
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, "COMPLETED", notifs[0].Status)
	assert.True(t, notifs[0].Dispatched)
}

func TestNotificationDispatcher_NonMatchingStatus(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("running")

	// Give time for event to be processed
	time.Sleep(200 * time.Millisecond)

	assert.Empty(t, env.dispatcher.getCalls())

	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Empty(t, notifs)
}

func TestNotificationDispatcher_Dedup(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	// Publish same status again
	env.publishStatus("completed")

	// Wait and verify no additional dispatch
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, env.dispatcher.getCalls(), 1)

	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
}

func TestNotificationDispatcher_DifferentStatuses(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	env.publishStatus("waiting_for_input")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 2
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	assert.Contains(t, calls[0].Message, "COMPLETED")
	assert.Contains(t, calls[1].Message, "WAITING_FOR_INPUT")
}

func TestNotificationDispatcher_NoSubscriptions(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	// Publish status for an agent with no subscriptions
	env.pub.PublishAgentStatus(context.Background(), &store.Agent{
		ID:       api.NewUUID(), // different agent
		ProjectID:  env.project.ID,
		Phase:    string(state.PhaseRunning),
		Activity: "completed",
	})

	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, env.dispatcher.getCalls())
}

func TestNotificationDispatcher_SubscriberAgentNotFound(t *testing.T) {
	env := setupNotificationTest(t)

	// Delete the subscriber agent
	require.NoError(t, env.store.DeleteAgent(context.Background(), env.subscriber.ID))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// No dispatch call since subscriber not found
	assert.Empty(t, env.dispatcher.getCalls())

	// Notification should still be stored
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.False(t, notifs[0].Dispatched) // not dispatched since subscriber was not found
}

func TestNotificationDispatcher_SubscriberNoBroker(t *testing.T) {
	env := setupNotificationTest(t)

	// Update subscriber to have no broker
	env.subscriber.RuntimeBrokerID = ""
	require.NoError(t, env.store.UpdateAgent(context.Background(), env.subscriber))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// No DispatchAgentMessage call
	assert.Empty(t, env.dispatcher.getCalls())

	// Notification should be stored and marked dispatched (best-effort)
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.True(t, notifs[0].Dispatched)
}

func TestNotificationDispatcher_DispatchFailure(t *testing.T) {
	env := setupNotificationTest(t)
	env.dispatcher.returnErr = fmt.Errorf("broker unavailable")

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	// Even on dispatch failure, notification is stored and marked dispatched (best-effort)
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.True(t, notifs[0].Dispatched)
}

func TestNotificationDispatcher_UserSubscriber(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace the agent subscription with a user subscription
	require.NoError(t, env.store.DeleteNotificationSubscription(context.Background(), env.sub.ID))
	userSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-123",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute), // Predate agent creation so stale filter doesn't skip
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(context.Background(), userSub))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// No dispatch call for user subscribers
	assert.Empty(t, env.dispatcher.getCalls())

	// But notification should be stored
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeUser, "user-123", false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "COMPLETED", notifs[0].Status)

	// Inbox message should also be created (no broker → direct persistence)
	msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		RecipientID: "user-123",
		ProjectID:     env.project.ID,
	}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, "agent:watched-agent", msgs.Items[0].Sender)
	assert.Equal(t, "user:user-123", msgs.Items[0].Recipient)
	assert.Equal(t, messages.TypeStateChange, msgs.Items[0].Type)
}

func TestNotificationDispatcher_UserSubscriberInboxWithBroker(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace the agent subscription with a user subscription
	require.NoError(t, env.store.DeleteNotificationSubscription(context.Background(), env.sub.ID))
	userSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-broker-inbox",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(context.Background(), userSub))

	// Set up a broker proxy — notifications should NOT be routed through the
	// broker (only explicit scion-message commands use the broker path).
	rb := &recordingBroker{}
	proxy := NewMessageBrokerProxy(rb, env.store, env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	env.nd.SetBrokerProxy(proxy)

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Broker should NOT receive the notification (only scion message does).
	assert.Empty(t, rb.getPublishes(), "broker should not receive notification publishes")

	// Inbox message should be created directly for the web UI.
	msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		RecipientID: "user-broker-inbox",
		ProjectID:     env.project.ID,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, msgs.Items, 1, "inbox message should be created directly even when broker is present")
}

func TestNotificationDispatcher_UserSubscriberInboxWaitingForInput(t *testing.T) {
	env := setupNotificationTest(t)

	// Set the watched agent's Message field so createInboxMessage uses it
	ctx := context.Background()
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Activity: "waiting_for_input",
		Message:  "What branch should I target?",
	}))

	// Replace the agent subscription with a user subscription
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))
	userSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-wfi",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"WAITING_FOR_INPUT"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, userSub))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("waiting_for_input")

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Inbox message should use the agent's raw Message field and input-needed type
	msgs, err := env.store.ListMessages(ctx, store.MessageFilter{
		RecipientID: "user-wfi",
		ProjectID:     env.project.ID,
	}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, "What branch should I target?", msgs.Items[0].Msg)
	assert.Equal(t, messages.TypeInputNeeded, msgs.Items[0].Type)
}

func TestNotificationDispatcher_Stop(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	env.nd.Stop()

	// Publish after stop — should not panic or process
	env.publishStatus("completed")

	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, env.dispatcher.getCalls())
}

func TestNotificationDispatcher_DoubleStop(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()

	// Calling Stop twice must not panic
	env.nd.Stop()
	env.nd.Stop()
}

func TestNotificationDispatcher_NilDispatcher(t *testing.T) {
	env := setupNotificationTest(t)
	// Replace with a nil-returning getter to simulate no dispatcher available
	env.nd.getDispatcher = func() AgentDispatcher { return nil }

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// No dispatch calls since dispatcher is nil
	assert.Empty(t, env.dispatcher.getCalls())

	// Notification should be stored and marked dispatched (best-effort)
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.True(t, notifs[0].Dispatched)
}

func TestNotificationDispatcher_CaseInsensitiveStatus(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	// Publish lowercase status — should match
	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	// Stored as uppercase
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, "COMPLETED", notifs[0].Status)
}

func TestFormatNotificationMessage(t *testing.T) {
	tests := []struct {
		name     string
		agent    *store.Agent
		status   string
		expected string
	}{
		{
			name:     "COMPLETED without summary",
			agent:    &store.Agent{Slug: "worker"},
			status:   "COMPLETED",
			expected: "worker has reached a state of COMPLETED",
		},
		{
			name:     "COMPLETED with summary",
			agent:    &store.Agent{Slug: "worker", TaskSummary: "Finished refactoring"},
			status:   "COMPLETED",
			expected: "worker has reached a state of COMPLETED: Finished refactoring",
		},
		{
			name:     "WAITING_FOR_INPUT without message",
			agent:    &store.Agent{Slug: "helper"},
			status:   "WAITING_FOR_INPUT",
			expected: "helper is WAITING_FOR_INPUT",
		},
		{
			name:     "WAITING_FOR_INPUT with message",
			agent:    &store.Agent{Slug: "helper", Message: "Need API key"},
			status:   "WAITING_FOR_INPUT",
			expected: "helper is WAITING_FOR_INPUT: Need API key",
		},
		{
			name:     "LIMITS_EXCEEDED without message",
			agent:    &store.Agent{Slug: "cruncher"},
			status:   "LIMITS_EXCEEDED",
			expected: "cruncher has reached a state of LIMITS_EXCEEDED",
		},
		{
			name:     "LIMITS_EXCEEDED with message",
			agent:    &store.Agent{Slug: "cruncher", Message: "Token limit reached"},
			status:   "LIMITS_EXCEEDED",
			expected: "cruncher has reached a state of LIMITS_EXCEEDED: Token limit reached",
		},
		{
			name:     "ERROR without message",
			agent:    &store.Agent{Slug: "bot"},
			status:   "ERROR",
			expected: "bot has reached a state of ERROR",
		},
		{
			name:     "ERROR with message",
			agent:    &store.Agent{Slug: "bot", Message: "Container OOM killed"},
			status:   "ERROR",
			expected: "bot has reached a state of ERROR: Container OOM killed",
		},
		{
			name:     "STALLED without context",
			agent:    &store.Agent{Slug: "bot"},
			status:   "STALLED",
			expected: "bot has STALLED",
		},
		{
			name:     "STALLED with prior activity",
			agent:    &store.Agent{Slug: "bot", StalledFromActivity: "thinking"},
			status:   "STALLED",
			expected: "bot has STALLED (was thinking)",
		},
		{
			name:     "STALLED with prior activity and message",
			agent:    &store.Agent{Slug: "bot", StalledFromActivity: "executing", Message: "Stuck on build"},
			status:   "STALLED",
			expected: "bot has STALLED (was executing): Stuck on build",
		},
		{
			name:     "Unknown status",
			agent:    &store.Agent{Slug: "bot"},
			status:   "SOMETHING_ELSE",
			expected: "bot has reached status: SOMETHING_ELSE",
		},
		{
			name:     "Case insensitive input",
			agent:    &store.Agent{Slug: "bot"},
			status:   "completed",
			expected: "bot has reached a state of COMPLETED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatNotificationMessage(tt.agent, tt.status)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNotificationDispatcher_MultipleSubscribers(t *testing.T) {
	env := setupNotificationTest(t)

	// Add a user subscription in addition to the existing agent subscription
	userSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-456",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(context.Background(), userSub))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Agent subscriber should get a dispatch
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	// Both notifications should be stored
	agentNotifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, agentNotifs, 1)

	userNotifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeUser, "user-456", false)
	require.NoError(t, err)
	assert.Len(t, userNotifs, 1)
}

func TestNotificationDispatcher_PublisherClosed(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	// Close the publisher — goroutine should exit cleanly
	env.pub.Close()

	// Give time for goroutine to exit
	time.Sleep(200 * time.Millisecond)

	// No panic or deadlock — test passes if we get here
}

func TestNotificationDispatcher_CompletedWithTaskSummary(t *testing.T) {
	env := setupNotificationTest(t)

	// Update the watched agent with a task summary
	env.watched.TaskSummary = "Refactored auth module"
	require.NoError(t, env.store.UpdateAgent(context.Background(), env.watched))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	assert.Equal(t, "watched-agent has reached a state of COMPLETED: Refactored auth module", calls[0].Message)

	sm := calls[0].StructuredMessage
	require.NotNil(t, sm)
	assert.Equal(t, "agent:watched-agent", sm.Sender)
	assert.Equal(t, messages.TypeStateChange, sm.Type)
}

func TestNotificationDispatcher_AgentDispatchUsesStructuredMessage(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()

	// Dispatched message should have a structured message with proper sender/type
	sm := calls[0].StructuredMessage
	require.NotNil(t, sm)
	assert.Equal(t, "agent:watched-agent", sm.Sender)
	assert.Equal(t, "agent:subscriber-agent", sm.Recipient)
	assert.Equal(t, messages.TypeStateChange, sm.Type)
	assert.Equal(t, messages.Version, sm.Version)
	assert.NotEmpty(t, sm.Timestamp)

	// Plain message field should match the notification message (no prefix)
	assert.Equal(t, calls[0].Message, sm.Msg)

	// Stored notification message matches
	notifs, err := env.store.GetNotifications(context.Background(), store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, calls[0].Message, notifs[0].Message)
}

func TestNotificationDispatcher_WaitingForInputWithMessage(t *testing.T) {
	env := setupNotificationTest(t)

	// Update the watched agent with a message
	env.watched.Message = "Please approve the PR"
	require.NoError(t, env.store.UpdateAgent(context.Background(), env.watched))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("waiting_for_input")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	assert.Equal(t, "watched-agent is WAITING_FOR_INPUT: Please approve the PR", calls[0].Message)

	// Verify input-needed type is used for waiting_for_input status
	sm := calls[0].StructuredMessage
	require.NotNil(t, sm)
	assert.Equal(t, messages.TypeInputNeeded, sm.Type)
	assert.Equal(t, "agent:watched-agent", sm.Sender)
}

func TestNotificationMessageType(t *testing.T) {
	assert.Equal(t, messages.TypeInputNeeded, notificationMessageType("WAITING_FOR_INPUT"))
	assert.Equal(t, messages.TypeInputNeeded, notificationMessageType("waiting_for_input"))
	assert.Equal(t, messages.TypeStateChange, notificationMessageType("COMPLETED"))
	assert.Equal(t, messages.TypeStateChange, notificationMessageType("ERROR"))
	assert.Equal(t, messages.TypeStateChange, notificationMessageType("STALLED"))
	assert.Equal(t, messages.TypeStateChange, notificationMessageType("LIMITS_EXCEEDED"))
}

func TestNotificationDispatcher_StalledActivity(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace subscription to include STALLED
	ctx := context.Background()
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))
	stalledSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      env.subscriber.Slug,
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT", "STALLED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, stalledSub))

	// Set stalled context on the watched agent
	env.watched.StalledFromActivity = "thinking"
	require.NoError(t, env.store.UpdateAgent(ctx, env.watched))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("stalled")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	assert.Contains(t, calls[0].Message, "watched-agent has STALLED (was thinking)")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, "STALLED", notifs[0].Status)
}

func TestNotificationDispatcher_ErrorPhase(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace subscription to include ERROR
	ctx := context.Background()
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))
	errorSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      env.subscriber.Slug,
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED", "ERROR"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, errorSub))

	env.nd.Start()
	defer env.nd.Stop()

	// Publish with phase=error and no activity (typical for infrastructure errors)
	env.publishStatusWithPhase(string(state.PhaseError), "")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	assert.Contains(t, calls[0].Message, "watched-agent has reached a state of ERROR")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, "ERROR", notifs[0].Status)
}

func TestNotificationDispatcher_BrokerNotUsedForUserNotification(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace the agent subscription with a user subscription
	require.NoError(t, env.store.DeleteNotificationSubscription(context.Background(), env.sub.ID))
	userSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-broker",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(context.Background(), userSub))

	// Set up a recording broker and wire it as the broker proxy
	rb := &recordingBroker{}
	proxy := NewMessageBrokerProxy(rb, env.store, env.pub, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	env.nd.SetBrokerProxy(proxy)

	// Also set up a recording channel — should receive the notification since
	// the broker path is no longer used for notifications.
	ch := &recordingChannel{name: "test-channel"}
	env.nd.channelRegistry = &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs:  []ChannelConfig{{Type: "test-channel"}},
		log:      slog.Default(),
	}

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Broker should NOT receive the notification (only scion message uses broker).
	assert.Empty(t, rb.getPublishes(), "broker should not receive notification publishes")

	// Inbox message should be created directly for the web UI.
	msgs, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		RecipientID: "user-broker",
		ProjectID:     env.project.ID,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, msgs.Items, 1, "inbox message should be created directly")

	// Channel registry should be called as a fallback for external delivery.
	assert.Len(t, ch.getDeliveries(), 1, "channel registry should receive the notification")
}

func TestNotificationDispatcher_FallbackToChannelWhenNoBroker(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace the agent subscription with a user subscription
	require.NoError(t, env.store.DeleteNotificationSubscription(context.Background(), env.sub.ID))
	userSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-fallback",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(context.Background(), userSub))

	// No broker proxy set — should fall back to channel registry
	ch := &recordingChannel{name: "test-channel"}
	env.nd.channelRegistry = &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs:  []ChannelConfig{{Type: "test-channel"}},
		log:      slog.Default(),
	}

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for channel to receive the notification
	require.Eventually(t, func() bool {
		return len(ch.getDeliveries()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	deliveries := ch.getDeliveries()
	assert.Equal(t, "agent:watched-agent", deliveries[0].Sender)
	assert.Equal(t, "user:user-fallback", deliveries[0].Recipient)
}

func TestNotificationDispatcher_ChannelDispatchOnUserNotification(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace the agent subscription with a user subscription
	require.NoError(t, env.store.DeleteNotificationSubscription(context.Background(), env.sub.ID))
	userSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "user-123",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(context.Background(), userSub))

	// Set up a recording channel via the registry
	ch := &recordingChannel{name: "test-channel"}
	env.nd.channelRegistry = &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs:  []ChannelConfig{{Type: "test-channel"}},
		log:      slog.Default(),
	}

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for processing
	require.Eventually(t, func() bool {
		return len(ch.getDeliveries()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	deliveries := ch.getDeliveries()
	assert.Equal(t, "agent:watched-agent", deliveries[0].Sender)
	assert.Equal(t, "user:user-123", deliveries[0].Recipient)
	assert.Equal(t, messages.TypeStateChange, deliveries[0].Type)
	assert.Contains(t, deliveries[0].Msg, "watched-agent has reached a state of COMPLETED")
}

func TestNotificationDispatcher_NoChannelDispatchForAgentSubscriber(t *testing.T) {
	env := setupNotificationTest(t)

	// Set up a recording channel — should NOT receive anything for agent subscribers
	ch := &recordingChannel{name: "test-channel"}
	env.nd.channelRegistry = &ChannelRegistry{
		channels: []NotificationChannel{ch},
		configs:  []ChannelConfig{{Type: "test-channel"}},
		log:      slog.Default(),
	}

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for agent dispatch
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	// Channel should not have been called for agent subscriber
	assert.Empty(t, ch.getDeliveries())
}

func TestNotificationDispatcher_ErrorPhaseNotMatchedWithoutSubscription(t *testing.T) {
	env := setupNotificationTest(t)

	// Default subscription does not include ERROR
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatusWithPhase(string(state.PhaseError), "")

	// Give time for event to be processed
	time.Sleep(200 * time.Millisecond)

	// Should not trigger since default sub only has COMPLETED and WAITING_FOR_INPUT
	assert.Empty(t, env.dispatcher.getCalls())
}

func TestNotificationDispatcher_ProjectScopedSubscription(t *testing.T) {
	env := setupNotificationTest(t)

	// Delete the agent-scoped subscription
	ctx := context.Background()
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))

	// Create a project-scoped user subscription
	projectSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "project-watcher",
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, projectSub))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	// Wait for notification to be stored
	require.Eventually(t, func() bool {
		notifs, _ := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "project-watcher", false)
		return len(notifs) == 1
	}, 2*time.Second, 50*time.Millisecond)

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeUser, "project-watcher", false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, "COMPLETED", notifs[0].Status)
}

func TestNotificationDispatcher_DeletedTrigger(t *testing.T) {
	env := setupNotificationTest(t)

	// Replace subscription to include DELETED
	ctx := context.Background()
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))
	deletedSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      env.subscriber.Slug,
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED", "DELETED"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, deletedSub))

	env.nd.Start()
	defer env.nd.Stop()

	// Publish an agent deleted event
	env.pub.PublishAgentDeleted(ctx, env.watched.ID, env.project.ID)

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	assert.Contains(t, calls[0].Message, "watched-agent has been DELETED")

	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
	assert.Equal(t, "DELETED", notifs[0].Status)
}

func TestNotificationDispatcher_DeletedNotMatchedWithoutSubscription(t *testing.T) {
	env := setupNotificationTest(t)

	// Default subscription does not include DELETED
	env.nd.Start()
	defer env.nd.Stop()

	env.pub.PublishAgentDeleted(context.Background(), env.watched.ID, env.project.ID)

	// Give time for event to be processed
	time.Sleep(200 * time.Millisecond)

	// Should not trigger since default sub only has COMPLETED and WAITING_FOR_INPUT
	assert.Empty(t, env.dispatcher.getCalls())
}

func TestFormatNotificationMessage_Deleted(t *testing.T) {
	agent := &store.Agent{Slug: "worker"}
	result := formatNotificationMessage(agent, "DELETED")
	assert.Equal(t, "worker has been DELETED", result)
}

func TestUpdateNotificationSubscriptionTriggers(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	// Update triggers
	err := env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, []string{"COMPLETED", "DELETED"})
	require.NoError(t, err)

	// Verify update
	sub, err := env.store.GetNotificationSubscription(ctx, env.sub.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"COMPLETED", "DELETED"}, sub.TriggerActivities)
}

func TestUpdateNotificationSubscriptionTriggers_NotFound(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	err := env.store.UpdateNotificationSubscriptionTriggers(ctx, "nonexistent-id", []string{"COMPLETED"})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateNotificationSubscriptionTriggers_InvalidInput(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	err := env.store.UpdateNotificationSubscriptionTriggers(ctx, env.sub.ID, nil)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	err = env.store.UpdateNotificationSubscriptionTriggers(ctx, "", []string{"COMPLETED"})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestSubscriptionTemplates_CRUD(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	// Create
	tmpl := &store.SubscriptionTemplate{
		ID:                api.NewUUID(),
		Name:              "Critical Only",
		Scope:             store.SubscriptionScopeProject,
		TriggerActivities: []string{"ERROR", "LIMITS_EXCEEDED"},
		ProjectID:           env.project.ID,
		CreatedBy:         "test-user",
	}
	require.NoError(t, env.store.CreateSubscriptionTemplate(ctx, tmpl))

	// Get
	got, err := env.store.GetSubscriptionTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, "Critical Only", got.Name)
	assert.Equal(t, []string{"ERROR", "LIMITS_EXCEEDED"}, got.TriggerActivities)

	// List with project filter
	templates, err := env.store.ListSubscriptionTemplates(ctx, env.project.ID)
	require.NoError(t, err)
	assert.Len(t, templates, 1)
	assert.Equal(t, "Critical Only", templates[0].Name)

	// List without project filter (only global templates)
	globalTemplates, err := env.store.ListSubscriptionTemplates(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, globalTemplates)

	// Delete
	require.NoError(t, env.store.DeleteSubscriptionTemplate(ctx, tmpl.ID))
	_, err = env.store.GetSubscriptionTemplate(ctx, tmpl.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestSubscriptionTemplates_DuplicateName(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()

	tmpl := &store.SubscriptionTemplate{
		ID:                api.NewUUID(),
		Name:              "My Template",
		Scope:             store.SubscriptionScopeProject,
		TriggerActivities: []string{"COMPLETED"},
		ProjectID:           env.project.ID,
		CreatedBy:         "test-user",
	}
	require.NoError(t, env.store.CreateSubscriptionTemplate(ctx, tmpl))

	// Same name in same project should fail
	tmpl2 := &store.SubscriptionTemplate{
		ID:                api.NewUUID(),
		Name:              "My Template",
		Scope:             store.SubscriptionScopeProject,
		TriggerActivities: []string{"ERROR"},
		ProjectID:           env.project.ID,
		CreatedBy:         "test-user",
	}
	err := env.store.CreateSubscriptionTemplate(ctx, tmpl2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestNotificationDispatcher_DeduplicateAcrossScopes(t *testing.T) {
	env := setupNotificationTest(t)

	// Keep the existing agent-scoped subscription (subscriber-agent watches watched-agent).
	// Add a project-scoped subscription for the SAME subscriber.
	ctx := context.Background()
	projectSub := &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeProject,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      env.subscriber.Slug,
		ProjectID:           env.project.ID,
		TriggerActivities: []string{"COMPLETED", "WAITING_FOR_INPUT"},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, projectSub))

	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	// Wait a bit to ensure no second dispatch
	time.Sleep(200 * time.Millisecond)

	// Should receive exactly 1 dispatch (deduplicated), not 2
	assert.Len(t, env.dispatcher.getCalls(), 1)

	// Only 1 notification stored (from the agent-scoped subscription, which was checked first)
	notifs, err := env.store.GetNotifications(ctx, store.SubscriberTypeAgent, env.subscriber.Slug, false)
	require.NoError(t, err)
	assert.Len(t, notifs, 1)
}
