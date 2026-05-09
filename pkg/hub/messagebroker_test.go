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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/broker"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
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
func (d *brokerMockDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	return nil
}
func (d *brokerMockDispatcher) DispatchAgentRestart(ctx context.Context, agent *store.Agent) error {
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
	s, err := sqlite.New(":memory:")
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
		ID:       "broker-1",
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
		ProjectID:         projectID,
		Phase:           phase,
		RuntimeBrokerID: "broker-1",
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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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

func TestMessageBrokerProxy_ProjectBroadcast(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "agent-a", "running")
	setupBrokerTestAgent(t, s, projectID, "agent-b", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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
	setupBrokerTestAgent(t, s, projectID, "sender-agent", "running")
	setupBrokerTestAgent(t, s, projectID, "other-agent", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	proxy.subscribeProjectBroadcast(projectID)

	msg := messages.NewInstruction("agent:sender-agent", "project:test-project", "any updates?")
	msg.Broadcasted = true
	proxy.PublishBroadcast(context.Background(), projectID, msg)

	time.Sleep(100 * time.Millisecond)

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 message (sender excluded), got %d", len(dispatched))
	}
	if dispatched[0].agentSlug != "other-agent" {
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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	if err := proxy.EnsureProjectSubscriptions(context.Background(), projectID); err != nil {
		t.Fatal(err)
	}

	msg := messages.NewInstruction("user:alice", "agent:running-agent", "hello")
	proxy.PublishMessage(context.Background(), projectID, msg)

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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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
	if len(result.Items) != 2 {
		t.Fatalf("expected 2 persisted user messages (local + broker), got %d", len(result.Items))
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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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
	if len(result.Items) != 2 {
		t.Fatalf("expected 2 persisted user messages after EnsureProjectSubscriptions (local + broker), got %d", len(result.Items))
	}
}

func TestMessageBrokerProxy_PluginSubscription(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	setupBrokerTestAgent(t, s, projectID, "agent-x", "running")

	events := NewChannelEventPublisher()
	defer events.Close()

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

	dispatcher := &brokerMockDispatcher{}

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	proxy.Start()
	defer proxy.Stop()

	// Plugin requests a subscription for the project
	pattern := broker.TopicAgentMessages(projectID, "*")
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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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
	if len(result.Items) != 2 {
		t.Fatalf("expected 2 persisted messages from bootstrapped subscription (local + broker), got %d", len(result.Items))
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

	b := broker.NewInProcessBroker(slog.Default())
	defer b.Close()

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
	if len(result.Items) != 2 {
		t.Fatalf("expected exactly 2 persisted messages (local + broker), got %d", len(result.Items))
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
