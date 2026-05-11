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

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/broker"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MessageBrokerProxy bridges the message broker with the Hub's agent lifecycle
// and dispatch infrastructure. It:
//   - Subscribes to broker topics on behalf of agents (agents don't have direct broker access)
//   - Dispatches received messages to agents via the existing DispatchAgentMessage path
//   - Manages subscriptions based on agent lifecycle events (created/deleted)
//   - Handles broadcast fan-out from a single broker publish to individual agent deliveries
type MessageBrokerProxy struct {
	broker        broker.MessageBroker
	store         store.Store
	events        *ChannelEventPublisher
	getDispatcher func() AgentDispatcher
	log           *slog.Logger
	messageLog    *slog.Logger

	mu                  sync.Mutex
	subscriptions       map[string][]broker.Subscription // projectID -> active subscriptions
	pluginSubscriptions map[string]broker.Subscription   // pattern -> plugin-initiated subscription
	subscribedTopics    map[string]bool                  // dedup guard for project-level subscriptions
	stopCh              chan struct{}
	stopOnce            sync.Once
	wg                  sync.WaitGroup
}

// NewMessageBrokerProxy creates a new MessageBrokerProxy.
func NewMessageBrokerProxy(
	b broker.MessageBroker,
	s store.Store,
	events *ChannelEventPublisher,
	getDispatcher func() AgentDispatcher,
	log *slog.Logger,
) *MessageBrokerProxy {
	return &MessageBrokerProxy{
		broker:              b,
		store:               s,
		events:              events,
		getDispatcher:       getDispatcher,
		log:                 log,
		subscriptions:       make(map[string][]broker.Subscription),
		pluginSubscriptions: make(map[string]broker.Subscription),
		subscribedTopics:    make(map[string]bool),
		stopCh:              make(chan struct{}),
	}
}

// Start subscribes to agent lifecycle events and sets up broker subscriptions
// for existing running agents.
func (p *MessageBrokerProxy) Start() {
	// Listen for agent lifecycle events to manage broker subscriptions dynamically
	ch, unsubscribe := p.events.Subscribe(
		"project.>.agent.created",
		"project.>.agent.status",
		"project.>.agent.deleted",
	)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer unsubscribe()
		for {
			select {
			case evt, ok := <-ch:
				if !ok {
					return
				}
				p.handleLifecycleEvent(evt)
			case <-p.stopCh:
				return
			}
		}
	}()

	// Subscribe to global broadcasts
	p.subscribeGlobalBroadcast()

	// Bootstrap subscriptions for projects that already have running agents.
	// Without this, messages published before the next agent.created lifecycle
	// event would be silently dropped by the broker.
	p.bootstrapExistingProjects()

	p.log.Info("Message broker proxy started")
}

// bootstrapExistingProjects sets up broker subscriptions for all projects that
// already have running agents at startup time.
func (p *MessageBrokerProxy) bootstrapExistingProjects() {
	ctx := context.Background()
	result, err := p.store.ListAgents(ctx, store.AgentFilter{
		Phase: "running",
	}, store.ListOptions{})
	if err != nil {
		p.log.Error("Failed to list running agents for bootstrap", "error", err)
		return
	}

	projects := make(map[string]bool)
	for _, agent := range result.Items {
		if !projects[agent.ProjectID] {
			projects[agent.ProjectID] = true
			if err := p.EnsureProjectSubscriptions(ctx, agent.ProjectID); err != nil {
				p.log.Error("Failed to bootstrap project subscriptions",
					"project_id", agent.ProjectID, "error", err)
			}
		}
	}

	if len(projects) > 0 {
		p.log.Info("Bootstrapped broker subscriptions for existing projects", "count", len(projects))
	}
}

// Stop signals the proxy to shut down and waits for goroutines to finish.
func (p *MessageBrokerProxy) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.wg.Wait()

		// Unsubscribe all broker subscriptions
		p.mu.Lock()
		for projectID, subs := range p.subscriptions {
			for _, sub := range subs {
				sub.Unsubscribe()
			}
			delete(p.subscriptions, projectID)
		}
		for pattern, sub := range p.pluginSubscriptions {
			sub.Unsubscribe()
			delete(p.pluginSubscriptions, pattern)
		}
		p.subscribedTopics = make(map[string]bool)
		p.mu.Unlock()

		p.log.Info("Message broker proxy stopped")
	})
}

// RequestSubscription handles a plugin's request to subscribe to a topic
// pattern. Messages matching the pattern are routed to the plugin via the
// broker's Publish method. Plugin-initiated subscriptions coexist with
// proxy-managed subscriptions; duplicate patterns are no-ops.
func (p *MessageBrokerProxy) RequestSubscription(pattern string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.pluginSubscriptions[pattern]; exists {
		p.log.Debug("Plugin subscription already exists", "pattern", pattern)
		return nil
	}

	sub, err := p.broker.Subscribe(pattern, func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		// Route the message to the plugin for external delivery.
		if pubErr := p.broker.Publish(ctx, topic, msg); pubErr != nil {
			p.log.Error("Failed to deliver plugin-requested message", "pattern", pattern, "error", pubErr)
		}
	})
	if err != nil {
		return fmt.Errorf("failed to subscribe for plugin pattern %q: %w", pattern, err)
	}

	p.pluginSubscriptions[pattern] = sub
	p.log.Info("Plugin-initiated subscription created", "pattern", pattern)
	return nil
}

// CancelSubscription handles a plugin's request to cancel a previously
// requested subscription.
func (p *MessageBrokerProxy) CancelSubscription(pattern string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	sub, exists := p.pluginSubscriptions[pattern]
	if !exists {
		return nil
	}

	if err := sub.Unsubscribe(); err != nil {
		return fmt.Errorf("failed to unsubscribe plugin pattern %q: %w", pattern, err)
	}

	delete(p.pluginSubscriptions, pattern)
	p.log.Info("Plugin-initiated subscription cancelled", "pattern", pattern)
	return nil
}

// PublishMessage publishes a message to the appropriate broker topic based on
// the message's recipient. This is the entry point for Hub handlers to route
// messages through the broker instead of direct dispatch.
func (p *MessageBrokerProxy) PublishMessage(ctx context.Context, projectID string, msg *messages.StructuredMessage) error {
	topic := broker.TopicAgentMessages(projectID, recipientSlug(msg.Recipient))
	return p.broker.Publish(ctx, topic, msg)
}

// PublishBroadcast publishes a broadcast message to the project or global broadcast topic.
func (p *MessageBrokerProxy) PublishBroadcast(ctx context.Context, projectID string, msg *messages.StructuredMessage) error {
	if projectID == "" {
		return p.broker.Publish(ctx, broker.TopicGlobalBroadcast(), msg)
	}
	return p.broker.Publish(ctx, broker.TopicProjectBroadcast(projectID), msg)
}

// PublishUserMessage publishes a message to the user-targeted broker topic and
// handles local delivery (DB persistence + SSE) directly. The external broker
// receives the message for chat-app delivery while the hub persists and
// publishes the SSE event so the web UI is updated immediately.
func (p *MessageBrokerProxy) PublishUserMessage(ctx context.Context, projectID, userID string, msg *messages.StructuredMessage) error {
	topic := broker.TopicUserMessages(projectID, userID)

	// Deliver locally: persist to message store and publish SSE event.
	// This is necessary because the BrokerPluginAdapter does not invoke
	// local subscription handlers — it only forwards via RPC.
	p.deliverToUser(ctx, projectID, topic, msg)

	// Publish to external broker for chat-app / external delivery.
	return p.broker.Publish(ctx, topic, msg)
}

// EnsureProjectSubscriptions sets up broker subscriptions for all running agents
// in the specified project. Called when a project becomes active or a broker reconnects.
func (p *MessageBrokerProxy) EnsureProjectSubscriptions(ctx context.Context, projectID string) error {
	result, err := p.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: projectID,
		Phase:   "running",
	}, store.ListOptions{})
	if err != nil {
		return err
	}

	for _, agent := range result.Items {
		p.subscribeAgent(projectID, agent.Slug)
	}

	// Also subscribe to project broadcast and user messages
	p.subscribeProjectBroadcast(projectID)
	p.subscribeProjectUserMessages(projectID)

	return nil
}

// handleLifecycleEvent processes agent lifecycle events to manage subscriptions.
func (p *MessageBrokerProxy) handleLifecycleEvent(evt Event) {
	switch {
	case containsSuffix(evt.Subject, ".agent.created"):
		var created AgentCreatedEvent
		if err := json.Unmarshal(evt.Data, &created); err != nil {
			p.log.Error("Failed to unmarshal agent created event", "error", err)
			return
		}
		p.subscribeAgent(created.ProjectID, created.Slug)
		p.subscribeProjectBroadcast(created.ProjectID)
		p.subscribeProjectUserMessages(created.ProjectID)

	case containsSuffix(evt.Subject, ".agent.status"):
		var status AgentStatusEvent
		if err := json.Unmarshal(evt.Data, &status); err != nil {
			p.log.Error("Failed to unmarshal agent status event", "error", err)
			return
		}
		// We don't need to take action on status events for the subscription
		// proxy — subscriptions are per-agent, not per-status. The agent's
		// subscription persists through status changes until it's deleted.

	case containsSuffix(evt.Subject, ".agent.deleted"):
		var deleted AgentDeletedEvent
		if err := json.Unmarshal(evt.Data, &deleted); err != nil {
			p.log.Error("Failed to unmarshal agent deleted event", "error", err)
			return
		}
		// Agent subscriptions are cleaned up when the project's subscriptions
		// are rebuilt. Individual cleanup is handled by the broker's
		// Unsubscribe mechanism if needed.
		p.log.Debug("Agent deleted, broker subscriptions will be cleaned on next project rebuild",
			"agent_id", deleted.AgentID, "project_id", deleted.ProjectID)
	}
}

// subscribeAgent creates a broker subscription for an individual agent's message topic.
func (p *MessageBrokerProxy) subscribeAgent(projectID, agentSlug string) {
	topic := broker.TopicAgentMessages(projectID, agentSlug)

	p.mu.Lock()
	if p.subscribedTopics[topic] {
		p.mu.Unlock()
		return
	}
	p.subscribedTopics[topic] = true
	p.mu.Unlock()

	sub, err := p.broker.Subscribe(topic, func(ctx context.Context, t string, msg *messages.StructuredMessage) {
		p.deliverToAgent(ctx, projectID, agentSlug, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for agent messages",
			"projectID", projectID, "agentSlug", agentSlug, "error", err)
		return
	}

	p.mu.Lock()
	p.subscriptions[projectID] = append(p.subscriptions[projectID], sub)
	p.mu.Unlock()

	p.log.Debug("Subscribed to agent messages", "topic", topic, "agentSlug", agentSlug)
}

// subscribeProjectBroadcast creates a broker subscription for project-wide broadcasts
// that fans out to all running agents in the project.
func (p *MessageBrokerProxy) subscribeProjectBroadcast(projectID string) {
	topic := broker.TopicProjectBroadcast(projectID)

	p.mu.Lock()
	if p.subscribedTopics[topic] {
		p.mu.Unlock()
		return
	}
	p.subscribedTopics[topic] = true
	p.mu.Unlock()

	sub, err := p.broker.Subscribe(topic, func(ctx context.Context, t string, msg *messages.StructuredMessage) {
		p.fanOutToProject(ctx, projectID, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for project broadcast",
			"projectID", projectID, "error", err)
		return
	}

	p.mu.Lock()
	p.subscriptions[projectID] = append(p.subscriptions[projectID], sub)
	p.mu.Unlock()

	p.log.Debug("Subscribed to project broadcast", "topic", topic)
}

// subscribeProjectUserMessages creates a broker subscription for all user-targeted
// messages in a project. When a message arrives, it is persisted to the message
// store and published as a user.message SSE event for connected browser clients.
// The subscription uses a wildcard to cover all users in the project.
func (p *MessageBrokerProxy) subscribeProjectUserMessages(projectID string) {
	topic := broker.TopicAllUserMessages(projectID)

	p.mu.Lock()
	if p.subscribedTopics[topic] {
		p.mu.Unlock()
		return
	}
	p.subscribedTopics[topic] = true
	p.mu.Unlock()

	sub, err := p.broker.Subscribe(topic, func(ctx context.Context, t string, msg *messages.StructuredMessage) {
		p.deliverToUser(ctx, projectID, t, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for project user messages",
			"projectID", projectID, "error", err)
		return
	}

	p.mu.Lock()
	p.subscriptions[projectID] = append(p.subscriptions[projectID], sub)
	p.mu.Unlock()

	p.log.Debug("Subscribed to project user messages", "topic", topic)
}

// deliverToUser handles a broker message addressed to a human user by persisting
// it to the message store and publishing a user.message SSE event.
func (p *MessageBrokerProxy) deliverToUser(ctx context.Context, projectID, topic string, msg *messages.StructuredMessage) {
	// Persist to message store (write-through; non-fatal if store fails).
	// AgentID is the sender's agent ID when an agent sends to a user.
	agentID := ""
	if strings.HasPrefix(msg.Sender, "agent:") {
		agentID = msg.SenderID
	}

	storeMsg := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:     projectID,
		Sender:      msg.Sender,
		SenderID:    msg.SenderID,
		Recipient:   msg.Recipient,
		RecipientID: msg.RecipientID,
		Msg:         msg.Msg,
		Type:        msg.Type,
		Urgent:      msg.Urgent,
		Broadcasted: msg.Broadcasted,
		AgentID:     agentID,
		CreatedAt:   time.Now(),
	}
	if err := p.store.CreateMessage(ctx, storeMsg); err != nil {
		p.log.Error("Failed to persist user message from broker", "topic", topic, "error", err)
	}

	// Publish SSE event so connected browser clients receive real-time inbox updates.
	p.events.PublishUserMessage(ctx, storeMsg)

	// Log to dedicated message audit log
	if p.messageLog != nil {
		logAttrs := []any{
			"project_id", projectID,
			"topic", topic,
			"source", "broker",
		}
		logAttrs = append(logAttrs, msg.LogAttrs()...)
		p.messageLog.Info("user message delivered via broker", logAttrs...)
	}
}

// subscribeGlobalBroadcast creates a broker subscription for global broadcasts.
func (p *MessageBrokerProxy) subscribeGlobalBroadcast() {
	topic := broker.TopicGlobalBroadcast()

	_, err := p.broker.Subscribe(topic, func(ctx context.Context, t string, msg *messages.StructuredMessage) {
		p.fanOutGlobal(ctx, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for global broadcast", "error", err)
	}
}

// deliverToAgent dispatches a message to a specific agent via the existing
// DispatchAgentMessage path.
func (p *MessageBrokerProxy) deliverToAgent(ctx context.Context, projectID, agentSlug string, msg *messages.StructuredMessage) {
	dispatcher := p.getDispatcher()
	if dispatcher == nil {
		p.log.Warn("No dispatcher available, cannot deliver broker message",
			"agentSlug", agentSlug)
		return
	}

	agent, err := p.store.GetAgentBySlug(ctx, projectID, agentSlug)
	if err != nil {
		p.log.Error("Failed to find agent for broker message delivery",
			"agentSlug", agentSlug, "projectID", projectID, "error", err)
		return
	}

	if agent.RuntimeBrokerID == "" {
		p.log.Warn("Agent has no runtime broker, skipping broker message delivery",
			"agentSlug", agentSlug)
		return
	}

	if err := dispatcher.DispatchAgentMessage(ctx, agent, msg.Msg, msg.Urgent, msg); err != nil {
		p.log.Error("Failed to dispatch broker message to agent",
			"agentSlug", agentSlug, "error", err)
		return
	}

	// Persist to message store (write-through; non-fatal if store fails).
	storeMsg := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:     projectID,
		Sender:      msg.Sender,
		SenderID:    msg.SenderID,
		Recipient:   msg.Recipient,
		RecipientID: msg.RecipientID,
		Msg:         msg.Msg,
		Type:        msg.Type,
		Urgent:      msg.Urgent,
		Broadcasted: msg.Broadcasted,
		AgentID:     agent.ID,
		CreatedAt:   time.Now(),
	}
	if err := p.store.CreateMessage(ctx, storeMsg); err != nil {
		p.log.Error("Failed to persist broker message to store", "agentSlug", agentSlug, "error", err)
	}

	// Log to dedicated message audit log
	if p.messageLog != nil {
		logAttrs := []any{
			"agent_id", agent.ID,
			"agent_name", agent.Name,
			"project_id", agent.ProjectID,
			"source", "broker",
		}
		logAttrs = append(logAttrs, msg.LogAttrs()...)
		p.messageLog.Info("broker message delivered", logAttrs...)
	}
}

// fanOutToProject dispatches a broadcast message to all running agents in a project.
func (p *MessageBrokerProxy) fanOutToProject(ctx context.Context, projectID string, msg *messages.StructuredMessage) {
	result, err := p.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: projectID,
		Phase:   "running",
	}, store.ListOptions{})
	if err != nil {
		p.log.Error("Failed to list agents for project broadcast fan-out",
			"projectID", projectID, "error", err)
		return
	}

	p.log.Debug("Broadcasting to project agents", "project_id", projectID, "count", len(result.Items))

	for _, agent := range result.Items {
		// Skip the sender if it's an agent in this project
		if msg.Sender == "agent:"+agent.Slug {
			continue
		}
		agentMsg := *msg // copy to set per-agent recipient
		agentMsg.Recipient = "agent:" + agent.Slug
		agentMsg.RecipientID = agent.ID
		p.deliverToAgent(ctx, projectID, agent.Slug, &agentMsg)
	}
}

// fanOutGlobal dispatches a global broadcast to all running agents across all projects.
func (p *MessageBrokerProxy) fanOutGlobal(ctx context.Context, msg *messages.StructuredMessage) {
	result, err := p.store.ListAgents(ctx, store.AgentFilter{
		Phase: "running",
	}, store.ListOptions{})
	if err != nil {
		p.log.Error("Failed to list agents for global broadcast fan-out", "error", err)
		return
	}

	p.log.Debug("Global broadcast to all agents", "count", len(result.Items))

	for _, agent := range result.Items {
		if msg.Sender == "agent:"+agent.Slug {
			continue
		}
		agentMsg := *msg
		agentMsg.Recipient = "agent:" + agent.Slug
		agentMsg.RecipientID = agent.ID
		p.deliverToAgent(ctx, agent.ProjectID, agent.Slug, &agentMsg)
	}
}

// recipientSlug extracts the slug from a recipient identity string.
// e.g. "agent:code-reviewer" -> "code-reviewer"
func recipientSlug(recipient string) string {
	for i, c := range recipient {
		if c == ':' {
			return recipient[i+1:]
		}
	}
	return recipient
}

// containsSuffix checks if a dot-separated subject string ends with the given suffix.
func containsSuffix(subject, suffix string) bool {
	return len(subject) >= len(suffix) && subject[len(subject)-len(suffix):] == suffix
}
