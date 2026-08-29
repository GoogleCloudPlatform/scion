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
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// brokerMockDispatcher records dispatched messages for test assertions.
type brokerMockDispatcher struct {
	mu       sync.Mutex
	messages []brokerDispatchedMsg
}

type brokerDispatchedMsg struct {
	agentSlug  string
	msg        string
	interrupt  bool
	structured *messages.StructuredMessage
}

func (d *brokerMockDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, _ bool) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentRestart(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messages = append(d.messages, brokerDispatchedMsg{
		agentSlug:  agent.Slug,
		msg:        message,
		interrupt:  interrupt,
		structured: structuredMsg,
	})
	return nil
}
func (d *brokerMockDispatcher) DispatchCheckAgentPrompt(ctx context.Context, agent *store.Agent) (bool, error) {
	return false, nil
}
func (d *brokerMockDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *brokerMockDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *brokerMockDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *brokerMockDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) error {
	return nil
}

func (d *brokerMockDispatcher) getMessages() []brokerDispatchedMsg {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]brokerDispatchedMsg, len(d.messages))
	copy(result, d.messages)
	return result
}

func newBrokerTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	return s
}

// setupBrokerTestProject creates a project and a runtime broker, returns the project ID.
func setupBrokerTestProject(t *testing.T, s store.Store) string {
	t.Helper()
	ctx := context.Background()

	// Create a runtime broker for agent FK constraints
	rb := &store.RuntimeBroker{
		ID:       tid("broker-1"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := s.CreateRuntimeBroker(ctx, rb); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	project := &store.Project{
		ID:         api.NewUUID(),
		Name:       "test-project",
		Slug:       "test-project",
		Visibility: store.VisibilityPrivate,
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	return project.ID
}

// setupBrokerTestAgent creates a running agent and returns it.
func setupBrokerTestAgent(t *testing.T, s store.Store, projectID, slug, phase string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:              api.NewUUID(),
		Name:            slug,
		Slug:            slug,
		ProjectID:       projectID,
		Phase:           phase,
		RuntimeBrokerID: tid("broker-1"),
		Visibility:      store.VisibilityPrivate,
	}
	if err := s.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}
	return agent
}

func TestMessageBrokerProxy_DirectMessage(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "test-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "test-agent")

	msg := messages.NewInstruction("user:alice", "agent:test-agent", "hello agent")
	if err := proxy.PublishMessage(context.Background(), projectID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != "test-agent" {
		t.Errorf("expected agent slug 'test-agent', got %q", dispatched[0].agentSlug)
	}
	if dispatched[0].msg != "hello agent" {
		t.Errorf("expected message 'hello agent', got %q", dispatched[0].msg)
	}
}

func TestMessageBrokerProxy_InterruptPrefix(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "test-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = b.Close() })

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "test-agent")

	msg := messages.NewInstruction("user:alice", "agent:test-agent", "!restart now")
	if err := proxy.PublishMessage(context.Background(), projectID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
	}
	if dispatched[0].msg != "restart now" {
		t.Errorf("expected message 'restart now' (! stripped), got %q", dispatched[0].msg)
	}
	if !dispatched[0].interrupt {
		t.Error("expected interrupt=true for !-prefixed message")
	}
	if !dispatched[0].structured.Urgent {
		t.Error("expected structured message Urgent=true for !-prefixed message")
	}
}

func TestMessageBrokerProxy_InterruptPrefixNotStrippedWithoutBang(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "test-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = b.Close() })

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "test-agent")

	msg := messages.NewInstruction("user:alice", "agent:test-agent", "hello agent")
	if err := proxy.PublishMessage(context.Background(), projectID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
	}
	if dispatched[0].msg != "hello agent" {
		t.Errorf("expected message 'hello agent' unchanged, got %q", dispatched[0].msg)
	}
	if dispatched[0].interrupt {
		t.Error("expected interrupt=false for non-!-prefixed message")
	}
}

func TestMessageBrokerProxy_InterruptPrefixEdgeCases(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantMsg       string
		wantInterrupt bool
		wantUrgent    bool
	}{
		{"bare bang", "!", "interrupt", true, true},
		{"bang with trailing spaces", "!   ", "interrupt", true, true},
		{"leading whitespace before bang", "  !restart", "restart", true, true},
		{"whitespace between bang and content", "!  restart", "restart", true, true},
		{"leading and inner whitespace", "  !  restart now  ", "restart now", true, true},
		{"normal message no prefix", "hello", "hello", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newBrokerTestStore(t)
			projectID := setupBrokerTestProject(t, s)
			setupBrokerTestAgent(t, s, projectID, "test-agent", "running")

			events := NewChannelEventPublisher()
			defer events.Close()

			bus := eventbus.NewInProcessEventBus(slog.Default())
			t.Cleanup(func() { _ = bus.Close() })

			dispatcher := &brokerMockDispatcher{}

			proxy := NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
			proxy.Start()
			defer proxy.Stop()

			proxy.subscribeAgent(projectID, "test-agent")

			msg := messages.NewInstruction("user:alice", "agent:test-agent", tt.input)
			if err := proxy.PublishMessage(context.Background(), projectID, msg); err != nil {
				t.Fatal(err)
			}

			time.Sleep(100 * time.Millisecond)

			dispatched := dispatcher.getMessages()
			if len(dispatched) != 1 {
				t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
			}
			if dispatched[0].msg != tt.wantMsg {
				t.Errorf("msg = %q, want %q", dispatched[0].msg, tt.wantMsg)
			}
			if dispatched[0].interrupt != tt.wantInterrupt {
				t.Errorf("interrupt = %v, want %v", dispatched[0].interrupt, tt.wantInterrupt)
			}
			if dispatched[0].structured.Urgent != tt.wantUrgent {
				t.Errorf("Urgent = %v, want %v", dispatched[0].structured.Urgent, tt.wantUrgent)
			}
		})
	}
}

func TestMessageBrokerProxy_InterruptPrefixPersistence(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	agent := setupBrokerTestAgent(t, s, projectID, "persist-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = b.Close() })

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "persist-agent")

	msg := messages.NewInstruction("user:alice", "agent:persist-agent", "!urgent task")
	msg.SenderID = "user-alice-id"
	msg.RecipientID = agent.ID
	if err := proxy.PublishMessage(context.Background(), projectID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify the persisted message has the stripped content and urgent flag
	ctx := context.Background()
	result, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list messages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(result.Items))
	}
	if result.Items[0].Msg != "urgent task" {
		t.Errorf("expected persisted msg 'urgent task', got %q", result.Items[0].Msg)
	}
	if !result.Items[0].Urgent {
		t.Error("expected persisted message Urgent=true")
	}
}

func TestMessageBrokerProxy_ProjectBroadcast(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "agent-a", "running")
	setupBrokerTestAgent(t, s, projectID, "agent-b", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeProjectBroadcast(projectID)

	msg := messages.NewInstruction("user:alice", "project:test-project", "hello everyone")
	msg.Broadcasted = true
	if err := proxy.PublishBroadcast(context.Background(), projectID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched messages (fan-out), got %d", len(dispatched))
	}

	slugs := map[string]bool{}
	for _, d := range dispatched {
		slugs[d.agentSlug] = true
	}
	if !slugs["agent-a"] || !slugs["agent-b"] {
		t.Errorf("expected both agent-a and agent-b to receive broadcast, got %v", slugs)
	}
}

func TestMessageBrokerProxy_BroadcastSkipsSender(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	senderAgent := setupBrokerTestAgent(t, s, projectID, "sender-agent", "running")
	setupBrokerTestAgent(t, s, projectID, tid("other-agent"), "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeProjectBroadcast(projectID)

	msg := messages.NewInstruction("agent:sender-agent", "project:test-project", "any updates?")
	msg.Broadcasted = true
	// R3: production now always sets SenderID from auth (B5 override at
	// handleProjectBroadcast:1261-1270). The fan-out self-skip relies on
	// SenderID, not the display-label Sender field.
	msg.SenderID = senderAgent.ID
	_ = proxy.PublishBroadcast(context.Background(), projectID, msg)

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 message (sender excluded), got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != tid("other-agent") {
		t.Errorf("expected message delivered to 'other-agent', got %q", dispatched[0].agentSlug)
	}
}

func TestMessageBrokerProxy_EnsureProjectSubscriptions(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "running-agent", "running")
	setupBrokerTestAgent(t, s, projectID, "stopped-agent", "stopped")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	if err := proxy.EnsureProjectSubscriptions(context.Background(), projectID); err != nil {
		t.Fatal(err)
	}

	msg := messages.NewInstruction("user:alice", "agent:running-agent", "hello")
	_ = proxy.PublishMessage(context.Background(), projectID, msg)

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
	}
}

func TestMessageBrokerProxy_DeliverToAgentPersistence(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	agent := setupBrokerTestAgent(t, s, projectID, "persist-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "persist-agent")

	msg := messages.NewInstruction("user:alice", "agent:persist-agent", "persist this")
	msg.SenderID = "user-alice-id"
	msg.RecipientID = agent.ID
	if err := proxy.PublishMessage(context.Background(), projectID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify message was dispatched
	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
	}

	// Verify message was persisted to store
	ctx := context.Background()
	result, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list messages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(result.Items))
	}
	if result.Items[0].Msg != "persist this" {
		t.Errorf("expected msg 'persist this', got %q", result.Items[0].Msg)
	}
	if result.Items[0].AgentID != agent.ID {
		t.Errorf("expected agentID %q, got %q", agent.ID, result.Items[0].AgentID)
	}
}

func TestMessageBrokerProxy_UserMessageDelivery(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "sending-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	// Subscribe to user messages for this project (as EnsureProjectSubscriptions would do)
	proxy.subscribeProjectUserMessages(projectID)

	// Subscribe to SSE user.message events to verify delivery
	sseEvents, unsub := events.Subscribe("user.user-bob-id.message", "project.*.user.message")
	defer unsub()

	userID := "user-bob-id"
	msg := messages.NewInstruction("agent:sending-agent", "user:bob", "question for you")
	msg.SenderID = "agent-uuid-123"
	msg.RecipientID = userID

	if err := proxy.PublishUserMessage(context.Background(), projectID, userID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify message was persisted to store
	ctx := context.Background()
	result, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: userID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list messages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted user message, got %d", len(result.Items))
	}
	if result.Items[0].Msg != "question for you" {
		t.Errorf("expected msg 'question for you', got %q", result.Items[0].Msg)
	}
	if result.Items[0].RecipientID != userID {
		t.Errorf("expected recipientID %q, got %q", userID, result.Items[0].RecipientID)
	}

	// Verify SSE event was published
	select {
	case evt := <-sseEvents:
		if evt.Subject != "user."+userID+".message" && !containsSuffix(evt.Subject, ".user.message") {
			t.Errorf("unexpected SSE event subject: %q", evt.Subject)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("expected SSE user.message event, got none")
	}
}

func TestMessageBrokerProxy_EnsureProjectSubscriptionsIncludesUserMessages(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "some-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	// EnsureProjectSubscriptions should also set up user message subscriptions
	if err := proxy.EnsureProjectSubscriptions(context.Background(), projectID); err != nil {
		t.Fatal(err)
	}

	userID := "user-carol-id"
	msg := messages.NewInstruction("agent:some-agent", "user:carol", "auto-subscribed?")
	msg.RecipientID = userID

	if err := proxy.PublishUserMessage(context.Background(), projectID, userID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify message was persisted via the auto-subscribed user topic
	ctx := context.Background()
	result, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: userID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list messages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted user message after EnsureProjectSubscriptions, got %d", len(result.Items))
	}
}

func TestMessageBrokerProxy_PluginSubscription(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "agent-x", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	// Plugin requests a subscription for the project
	pattern := eventbus.TopicAgentMessages(projectID, "*")
	if err := proxy.RequestSubscription(pattern); err != nil {
		t.Fatalf("RequestSubscription failed: %v", err)
	}

	// Verify the subscription was tracked
	proxy.mu.Lock()
	_, exists := proxy.pluginSubscriptions[pattern]
	proxy.mu.Unlock()
	if !exists {
		t.Fatal("expected plugin subscription to be tracked")
	}
}

func TestMessageBrokerProxy_PluginSubscriptionDedup(t *testing.T) {
	s := newBrokerTestStore(t)
	_ = setupBrokerTestProject(t, s)

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	pattern := "scion.project.test.>"
	if err := proxy.RequestSubscription(pattern); err != nil {
		t.Fatalf("first RequestSubscription failed: %v", err)
	}

	// Second request for the same pattern should be a no-op
	if err := proxy.RequestSubscription(pattern); err != nil {
		t.Fatalf("duplicate RequestSubscription failed: %v", err)
	}

	proxy.mu.Lock()
	count := len(proxy.pluginSubscriptions)
	proxy.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 plugin subscription, got %d", count)
	}
}

func TestMessageBrokerProxy_PluginSubscriptionCancel(t *testing.T) {
	s := newBrokerTestStore(t)
	_ = setupBrokerTestProject(t, s)

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	pattern := "scion.project.test.>"
	if err := proxy.RequestSubscription(pattern); err != nil {
		t.Fatalf("RequestSubscription failed: %v", err)
	}

	if err := proxy.CancelSubscription(pattern); err != nil {
		t.Fatalf("CancelSubscription failed: %v", err)
	}

	proxy.mu.Lock()
	_, exists := proxy.pluginSubscriptions[pattern]
	proxy.mu.Unlock()
	if exists {
		t.Fatal("expected plugin subscription to be removed after cancel")
	}

	// Cancelling a non-existent pattern should be a no-op
	if err := proxy.CancelSubscription("nonexistent.>"); err != nil {
		t.Fatalf("CancelSubscription of nonexistent pattern should not error: %v", err)
	}
}

func TestMessageBrokerProxy_PluginSubscriptionCleanupOnStop(t *testing.T) {
	s := newBrokerTestStore(t)
	_ = setupBrokerTestProject(t, s)

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()

	if err := proxy.RequestSubscription("scion.project.a.>"); err != nil {
		t.Fatal(err)
	}
	if err := proxy.RequestSubscription("scion.project.b.>"); err != nil {
		t.Fatal(err)
	}

	proxy.Stop()

	proxy.mu.Lock()
	count := len(proxy.pluginSubscriptions)
	proxy.mu.Unlock()
	if count != 0 {
		t.Fatalf("expected 0 plugin subscriptions after stop, got %d", count)
	}
}

func TestMessageBrokerProxy_StartBootstrapsExistingProjects(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "pre-existing-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	// Subscribe to SSE events before Start() so we can verify delivery
	sseEvents, unsub := events.Subscribe("user.user-dave-id.message", "project.*.user.message")
	defer unsub()

	// Start() should bootstrap subscriptions for the pre-existing project
	proxy.Start()
	defer proxy.Stop()

	// Publish a user message — should be received because Start() bootstrapped
	// the project's user message subscription
	userID := "user-dave-id"
	msg := messages.NewInstruction("agent:pre-existing-agent", "user:dave", "bootstrap test")
	msg.SenderID = "agent-uuid"
	msg.RecipientID = userID

	if err := proxy.PublishUserMessage(context.Background(), projectID, userID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Verify message was persisted
	result, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: userID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list messages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message from bootstrapped subscription, got %d", len(result.Items))
	}
	if result.Items[0].Msg != "bootstrap test" {
		t.Errorf("expected msg 'bootstrap test', got %q", result.Items[0].Msg)
	}

	// Verify SSE event was published
	select {
	case <-sseEvents:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Error("expected SSE user.message event from bootstrapped subscription, got none")
	}
}

func TestMessageBrokerProxy_ProjectSubscriptionDedup(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "dedup-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	// Call EnsureProjectSubscriptions twice — second call should be a no-op
	if err := proxy.EnsureProjectSubscriptions(context.Background(), projectID); err != nil {
		t.Fatal(err)
	}
	if err := proxy.EnsureProjectSubscriptions(context.Background(), projectID); err != nil {
		t.Fatal(err)
	}

	// Publish a user message — should be received exactly once
	userID := "user-dedup-id"
	msg := messages.NewInstruction("agent:dedup-agent", "user:dedup", "dedup test")
	msg.RecipientID = userID

	if err := proxy.PublishUserMessage(context.Background(), projectID, userID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	result, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: userID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list messages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected exactly 1 persisted message, got %d", len(result.Items))
	}
}

func TestMessageBrokerProxy_PublishToGroup(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "group-agent-a", "running")
	setupBrokerTestAgent(t, s, projectID, "group-agent-b", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "group-agent-a")
	proxy.subscribeAgent(projectID, "group-agent-b")

	msg := messages.NewInstruction("user:alice", "", "hello group")

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "group-agent-a"},
		{Kind: messages.RecipientAgent, Name: "group-agent-b"},
	}

	errs := proxy.PublishToGroup(context.Background(), projectID, recipients, msg)
	for k, err := range errs {
		if err != nil {
			t.Errorf("PublishToGroup error for %s: %v", k, err)
		}
	}

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatched messages from PublishToGroup, got %d", len(dispatched))
	}

	slugs := map[string]bool{}
	for _, d := range dispatched {
		slugs[d.agentSlug] = true
		if d.msg != "hello group" {
			t.Errorf("expected msg 'hello group', got %q", d.msg)
		}
	}
	if !slugs["group-agent-a"] || !slugs["group-agent-b"] {
		t.Errorf("expected both group-agent-a and group-agent-b to receive messages, got %v", slugs)
	}
}

func TestRecipientSlug(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"agent:code-reviewer", "code-reviewer"},
		{"user:alice", "alice"},
		{"no-prefix", "no-prefix"},
	}
	for _, tt := range tests {
		got := recipientSlug(tt.input)
		if got != tt.expected {
			t.Errorf("recipientSlug(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestContainsSuffix(t *testing.T) {
	tests := []struct {
		subject string
		suffix  string
		match   bool
	}{
		{"project.g1.agent.created", ".agent.created", true},
		{"project.g1.agent.status", ".agent.status", true},
		{"project.g1.agent.deleted", ".agent.deleted", true},
		{"project.g1.agent.status", ".agent.created", false},
		{"short", ".agent.created", false},
	}
	for _, tt := range tests {
		got := containsSuffix(tt.subject, tt.suffix)
		if got != tt.match {
			t.Errorf("containsSuffix(%q, %q) = %v, want %v", tt.subject, tt.suffix, got, tt.match)
		}
	}
}

// An agent's attachments are recorded before the message is published; the
// broker path is where they gain the message ID that ties them to a thread.
func TestMessageBrokerProxy_UserMessageLinksAttachments(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = db.Close() }()
	wcs := NewWebChatStore(db, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	meta := AttachmentMeta{
		ID:         api.NewUUID(),
		ProjectID:  projectID,
		Filename:   "shot.png",
		MimeType:   "image/png",
		Size:       12,
		UploadedBy: "agent-uuid-123",
		CreatedAt:  time.Now().UTC(),
	}
	if err := wcs.CreateAttachment(ctx, meta); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	encoded, ok := attachmentRefsMetadata([]AttachmentRef{{
		ID: meta.ID, Name: meta.Filename, MimeType: meta.MimeType, Size: meta.Size,
	}})
	if !ok {
		t.Fatal("expected refs to encode")
	}

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	proxy.webChatStore = wcs

	msg := messages.NewInstruction("agent:sending-agent", "user:bob", "here is the screenshot")
	msg.SenderID = "agent-uuid-123"
	msg.RecipientID = "user-bob-id"
	msg.Metadata = map[string]string{attachmentsMetadataKey: encoded}

	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg)

	result, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: "user-bob-id"}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(result.Items))
	}

	linked, err := wcs.GetAttachmentsByMessage(ctx, result.Items[0].ID)
	if err != nil {
		t.Fatalf("GetAttachmentsByMessage: %v", err)
	}
	if len(linked) != 1 || linked[0].ID != meta.ID {
		t.Fatalf("expected the attachment to be linked to the delivered message, got %+v", linked)
	}
}

// TestDEF20_NativeTopicUsesTopicConversation verifies that when a message is
// sent through the MessageBrokerProxy with a ThreadID matching a native webchat
// topic, the persisted message carries the topic's pre-existing conversation_id
// rather than a newly minted one. This is the production-path integration test
// for DEF-20.
func TestDEF20_NativeTopicUsesTopicConversation(t *testing.T) {
	// 1. Create test store and webchat store sharing the same *sql.DB.
	// The Ent migrations create the conversations table; sharing the DB lets
	// the webchat store's CreateTopic dual-write into it.
	s := newBrokerTestStore(t)
	cs, ok := s.(*entadapter.CompositeStore)
	if !ok {
		t.Fatal("expected *entadapter.CompositeStore from newBrokerTestStore")
	}
	rawDB := cs.DB()
	if rawDB == nil {
		t.Fatal("CompositeStore.DB() returned nil")
	}

	wcs := NewWebChatStore(rawDB, "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("webchat store Init: %v", err)
	}

	// 2. Set up project and agent.
	ctx := context.Background()
	projectID := setupBrokerTestProject(t, s)
	agent := setupBrokerTestAgent(t, s, projectID, "test-agent", "running")

	// 3. Create a topic with a known conversation_id. CreateTopic atomically
	// inserts both the topic row and a conversations row.
	topicID := "topic-" + api.NewUUID()[:8]
	convID := api.NewUUID()
	topic := WebChatTopic{
		ID:             topicID,
		ProjectID:      projectID,
		Name:           "test-topic",
		ConversationID: convID,
		CreatedBy:      "user-1",
		CreatedAt:      time.Now().UTC(),
	}
	if err := wcs.CreateTopic(ctx, topic); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// 4. Create proxy with webchat store wired in.
	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.webChatStore = wcs
	proxy.Start()
	defer proxy.Stop()
	proxy.subscribeAgent(projectID, "test-agent")

	// 5. Send a message with ThreadID = topicID. The proxy's deliverToAgent
	// should resolve the topic's conversation_id via WithTopicLookup.
	msg := messages.NewInstruction("user:alice", "agent:test-agent", "hello via topic")
	msg.SenderID = tid("user-alice")
	msg.RecipientID = agent.ID
	msg.ThreadID = topicID
	if err := proxy.PublishMessage(ctx, projectID, msg); err != nil {
		t.Fatal(err)
	}

	// 6. Wait for async delivery.
	time.Sleep(200 * time.Millisecond)

	// 7. Verify the dispatched message reached the agent.
	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
	}
	if dispatched[0].msg != "hello via topic" {
		t.Errorf("expected dispatched msg 'hello via topic', got %q", dispatched[0].msg)
	}

	// 8. Verify the persisted message has the topic's conversation_id.
	result, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(result.Items))
	}
	if result.Items[0].ConversationID != convID {
		t.Errorf("expected conversation_id %q (from topic), got %q",
			convID, result.Items[0].ConversationID)
	}
}

// logSpy is a slog.Handler that captures log records for test assertions.
// It passes all records through to a wrapped handler and records them.
type logSpy struct {
	slog.Handler
	mu      sync.Mutex
	records []slog.Record
}

func newLogSpy(inner slog.Handler) *logSpy {
	return &logSpy{Handler: inner}
}

func (s *logSpy) Handle(ctx context.Context, r slog.Record) error {
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
	return s.Handler.Handle(ctx, r)
}

// containsMessage returns true if any captured record contains the substring.
func (s *logSpy) containsMessage(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if strings.Contains(r.Message, substr) {
			return true
		}
	}
	return false
}

// TestEmptySenderID_DeliverToUser_SkipsDMResolution verifies that an empty
// SenderID prevents DM conversation resolution in the deliverToUser path.
//
// The DM key IS the ACL: a conversation resolved with an empty sender ID
// produces a key that names the wrong participant set. The SenderID != ""
// conjunct at messagebroker.go deliverToUser ensures this cannot happen.
//
// The test asserts TWO things:
//  1. No conversation_id is stamped on the persisted message.
//  2. ResolveOrCreateDMConversation is never CALLED — the messagebroker guard
//     prevents the resolution path from being entered at all, rather than
//     relying on the downstream function's own empty-ID check.
//
// POSITIVE CONTROL: remove the msg.SenderID != "" conjunct from deliverToUser's
// else-if (the DM resolution branch), re-run this test, and verify it FAILS.
// The downstream function logs "skipping conversation resolution: missing
// sender or recipient ID" — that log appearing means the guard was bypassed.
func TestEmptySenderID_DeliverToUser_SkipsDMResolution(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	ctx := context.Background()

	events := NewChannelEventPublisher()
	defer events.Close()

	spy := newLogSpy(slog.Default().Handler())
	log := slog.New(spy)

	b := eventbus.NewInProcessEventBus(log)
	defer func() { _ = b.Close() }()

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, log)

	// Message has a valid RecipientID but empty SenderID — the guard must
	// prevent DM resolution despite having a non-empty recipient.
	msg := messages.NewInstruction("agent:some-agent", "user:bob", "should not resolve DM")
	msg.SenderID = "" // deliberately empty
	msg.RecipientID = api.NewUUID()

	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg)

	// Assert 1: no conversation_id on the persisted message.
	result, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: msg.RecipientID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(result.Items))
	}
	if result.Items[0].ConversationID != "" {
		t.Fatalf("SECURITY: empty SenderID must not resolve a DM conversation, "+
			"but got conversation_id %q — the SenderID guard is broken",
			result.Items[0].ConversationID)
	}

	// Assert 2: the resolution function was never entered. If it was called,
	// it logs "skipping conversation resolution: missing sender or recipient ID".
	// That log appearing means the messagebroker guard was bypassed and we are
	// relying on defense-in-depth rather than the gate itself.
	if spy.containsMessage("skipping conversation resolution: missing sender or recipient ID") {
		t.Fatal("SECURITY: ResolveOrCreateDMConversation was called with empty SenderID — " +
			"the messagebroker guard (msg.SenderID != \"\") was bypassed. " +
			"The downstream function rejected it, but the guard should have prevented the call entirely.")
	}
}

// TestEmptySenderID_DeliverToAgent_SkipsDMResolution verifies that an empty
// SenderID prevents DM conversation resolution in the deliverToAgent path.
//
// Same security property as the deliverToUser variant: the DM key names
// participants, and an empty sender ID would produce a malformed key that
// resolves to the wrong participant set.
//
// POSITIVE CONTROL: remove the msg.SenderID != "" conjunct from deliverToAgent's
// else-if (the DM resolution branch), re-run this test, and verify it FAILS.
// The downstream function logs "skipping conversation resolution: missing
// sender or recipient ID" — that log appearing means the guard was bypassed.
func TestEmptySenderID_DeliverToAgent_SkipsDMResolution(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	agent := setupBrokerTestAgent(t, s, projectID, "target-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	spy := newLogSpy(slog.Default().Handler())
	log := slog.New(spy)

	b := eventbus.NewInProcessEventBus(log)
	defer func() { _ = b.Close() }()

	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, log)
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeAgent(projectID, "target-agent")

	// Message has a valid agent target but empty SenderID — the guard must
	// prevent DM resolution despite having a known agent.ID.
	msg := messages.NewInstruction("user:alice", "agent:target-agent", "should not resolve DM")
	msg.SenderID = "" // deliberately empty
	msg.RecipientID = agent.ID

	if err := proxy.PublishMessage(context.Background(), projectID, msg); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Assert 1: the message was dispatched (delivery works regardless).
	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(dispatched))
	}

	// Assert 2: no conversation_id on the persisted message.
	result, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agent.ID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(result.Items))
	}
	if result.Items[0].ConversationID != "" {
		t.Fatalf("SECURITY: empty SenderID must not resolve a DM conversation, "+
			"but got conversation_id %q — the SenderID guard is broken",
			result.Items[0].ConversationID)
	}

	// Assert 3: the resolution function was never entered.
	if spy.containsMessage("skipping conversation resolution: missing sender or recipient ID") {
		t.Fatal("SECURITY: ResolveOrCreateDMConversation was called with empty SenderID — " +
			"the messagebroker guard (msg.SenderID != \"\") was bypassed. " +
			"The downstream function rejected it, but the guard should have prevented the call entirely.")
	}
}

// TestResolveDMConversation_BroadcastSkipped verifies that the helper returns
// nil and does not create a conversation when msg.Broadcasted is true. The
// helper itself does not check Broadcasted — the guard lives in the callers —
// so this test checks the callers' guards indirectly by exercising deliverToUser
// and verifying no conversation_id is set on the persisted message.
func TestResolveDMConversation_BroadcastSkipped(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	log := slog.Default()
	ctx := context.Background()

	events := NewChannelEventPublisher()
	defer events.Close()

	b := eventbus.NewInProcessEventBus(log)
	defer func() { _ = b.Close() }()

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return &brokerMockDispatcher{} }, log)

	msg := messages.NewInstruction("agent:sender-bot", "user:bob", "broadcast hello")
	msg.SenderID = api.NewUUID()
	msg.RecipientID = api.NewUUID()
	msg.Broadcasted = true

	proxy.deliverToUser(ctx, projectID, "project."+projectID+".user.message", msg)

	result, err := s.ListMessages(ctx, store.MessageFilter{RecipientID: msg.RecipientID}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 persisted message, got %d", len(result.Items))
	}
	if result.Items[0].ConversationID != "" {
		t.Fatalf("expected empty ConversationID for broadcast, got %q", result.Items[0].ConversationID)
	}
}
