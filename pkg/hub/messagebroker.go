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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// brokerCallbackTimeout bounds how long a broker subscription callback may
// spend on persistence and dispatch. Subscription callbacks run asynchronously
// from the publisher and must NOT use the publisher's context (typically an
// HTTP request context) because it may already be canceled by the time the
// callback fires.
const brokerCallbackTimeout = 30 * time.Second

// MessageBrokerProxy bridges the message broker with the Hub's agent lifecycle
// and dispatch infrastructure. It:
//   - Subscribes to broker topics on behalf of agents (agents don't have direct broker access)
//   - Dispatches received messages to agents via the existing DispatchAgentMessage path
//   - Manages subscriptions based on agent lifecycle events (created/deleted)
//   - Handles broadcast fan-out from a single broker publish to individual agent deliveries
type MessageBrokerProxy struct {
	bus           eventbus.EventBus
	store         store.Store
	events        EventPublisher
	getDispatcher func() AgentDispatcher
	log           *slog.Logger
	messageLog    *slog.Logger
	chatNotifier  *ChatNotifier // W6: DM notification trigger for agent replies (nil-safe)
	// webChatStore is used for the two things that need the store-assigned
	// message ID: stamping the DM watermark and linking the message's
	// attachments. Neither can happen in the web channel spoke, because the ID
	// does not exist until deliverToUser runs. Nil-safe.
	webChatStore WebChatStore
	// writeDenyEnabled returns whether the G2 write-deny switch is on.
	// When nil or returning false, conversation resolution failures are non-fatal
	// (B10 contract). When returning true, they deny the write (G2 contract).
	writeDenyEnabled func() bool

	// messageAuthorizer, when non-nil, is called by deliverToAgent to
	// reauthorize cross-project messages at delivery/retry time (Phase 2, D5).
	// The callback receives the sender identity, target agent, and returns a
	// MessageDecision. A denied message is NOT persisted to recipient-visible
	// history. Nil means no reauthorization (legacy same-project behavior).
	messageAuthorizer func(ctx context.Context, senderID string, targetAgent *store.Agent) *MessageDecision

	mu                  sync.Mutex
	subscriptions       map[string][]eventbus.Subscription // projectID -> active subscriptions
	pluginSubscriptions map[string]eventbus.Subscription   // pattern -> plugin-initiated subscription
	subscribedTopics    map[string]bool                    // dedup guard for project-level subscriptions
	runningSeen         map[string]bool                    // agent IDs whose running status already ensured subscriptions
	stopped             bool                               // set by Stop; no subscription is registered afterwards
	// userSubMu is held across the whole subscribeProjectUserMessages call,
	// so a caller that is told "subscribed" knows Subscribe has returned
	// (ptone/scion#1906).
	userSubMu sync.Mutex
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewMessageBrokerProxy creates a new MessageBrokerProxy.
func NewMessageBrokerProxy(
	b eventbus.EventBus,
	s store.Store,
	events EventPublisher,
	getDispatcher func() AgentDispatcher,
	log *slog.Logger,
) *MessageBrokerProxy {
	return &MessageBrokerProxy{
		bus:                 b,
		store:               s,
		events:              events,
		getDispatcher:       getDispatcher,
		log:                 log,
		subscriptions:       make(map[string][]eventbus.Subscription),
		pluginSubscriptions: make(map[string]eventbus.Subscription),
		subscribedTopics:    make(map[string]bool),
		runningSeen:         make(map[string]bool),
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
		p.mu.Lock()
		p.stopped = true
		p.mu.Unlock()
		close(p.stopCh)
		p.wg.Wait()

		// Unsubscribe all broker subscriptions
		p.mu.Lock()
		for projectID, subs := range p.subscriptions {
			for _, sub := range subs {
				_ = sub.Unsubscribe()
			}
			delete(p.subscriptions, projectID)
		}
		for pattern, sub := range p.pluginSubscriptions {
			_ = sub.Unsubscribe()
			delete(p.pluginSubscriptions, pattern)
		}
		p.subscribedTopics = make(map[string]bool)
		p.runningSeen = make(map[string]bool)
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

	// With FanOutBroker, all plugin spokes receive Publish() calls directly
	// for every message. Re-publishing via p.bus.Publish() here would
	// loop back through InProcessBroker's own subscribers, creating a
	// feedback storm. The subscription is tracked for accounting only.
	sub, err := p.bus.Subscribe(pattern, func(_ context.Context, _ string, _ *messages.StructuredMessage) {})
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
	topic := eventbus.TopicAgentMessages(projectID, recipientSlug(msg.Recipient))
	return p.bus.Publish(ctx, topic, msg)
}

// PublishBroadcast publishes a broadcast message to the project or global broadcast topic.
func (p *MessageBrokerProxy) PublishBroadcast(ctx context.Context, projectID string, msg *messages.StructuredMessage) error {
	if projectID == "" {
		return p.bus.Publish(ctx, eventbus.TopicGlobalBroadcast(), msg)
	}
	return p.bus.Publish(ctx, eventbus.TopicProjectBroadcast(projectID), msg)
}

// PublishUserMessage publishes a message to the user-targeted broker topic.
// Local delivery (DB persistence + SSE) is handled by the InProcessBroker
// subscription in subscribeProjectUserMessages — do not call deliverToUser()
// here to avoid double-delivery.
func (p *MessageBrokerProxy) PublishUserMessage(ctx context.Context, projectID, userID string, msg *messages.StructuredMessage) error {
	topic := eventbus.TopicUserMessages(projectID, userID)
	return p.bus.Publish(ctx, topic, msg)
}

// PublishToGroup fans out a message to a parsed set of recipients, delegating
// to PublishMessage for agents and PublishUserMessage for users.
func (p *MessageBrokerProxy) PublishToGroup(ctx context.Context, projectID string, recipients []messages.GroupRecipient, msg *messages.StructuredMessage) map[string]error {
	errs := make(map[string]error, len(recipients))
	for _, r := range recipients {
		recipMsg := *msg
		recipMsg.Recipient = r.String()

		switch r.Kind {
		case messages.RecipientAgent:
			recipMsg.Recipient = "agent:" + r.Name
			if err := p.PublishMessage(ctx, projectID, &recipMsg); err != nil {
				errs[r.String()] = err
			}
		case messages.RecipientUser:
			recipMsg.Recipient = "user:" + r.Name
			if err := p.PublishUserMessage(ctx, projectID, r.Name, &recipMsg); err != nil {
				errs[r.String()] = err
			}
		}
	}
	return errs
}

// EnsureProjectSubscriptions sets up broker subscriptions for all running agents
// in the specified project. Called when a project becomes active or a broker reconnects.
func (p *MessageBrokerProxy) EnsureProjectSubscriptions(ctx context.Context, projectID string) error {
	result, err := p.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: projectID,
		Phase:     "running",
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
		// Subscriptions are per-agent and persist through status changes, but
		// they are only created at startup (for agents already running) and on
		// agent.created. An agent that was not running when the hub started
		// and is later started or resumed never gets them, so its outbound
		// replies to users (published on the project user-message topic) find
		// no subscriber and are silently dropped. Ensure them the first time
		// each agent reports running.
		if status.Phase == "running" && status.ProjectID != "" && status.AgentID != "" {
			p.ensureSubscriptionsForRunningAgent(status.ProjectID, status.AgentID)
		}

	case containsSuffix(evt.Subject, ".agent.deleted"):
		var deleted AgentDeletedEvent
		if err := json.Unmarshal(evt.Data, &deleted); err != nil {
			p.log.Error("Failed to unmarshal agent deleted event", "error", err)
			return
		}
		p.mu.Lock()
		delete(p.runningSeen, deleted.AgentID)
		p.mu.Unlock()
		// Agent subscriptions are cleaned up when the project's subscriptions
		// are rebuilt. Individual cleanup is handled by the broker's
		// Unsubscribe mechanism if needed.
		p.log.Debug("Agent deleted, broker subscriptions will be cleaned on next project rebuild",
			"agent_id", deleted.AgentID, "project_id", deleted.ProjectID)
	}
}

// ensureSubscriptionsForRunningAgent subscribes a running agent's topic and
// its project's broadcast and user-message topics, once per agent ID per
// proxy lifetime. The agent is read back first so that a status event that
// is already stale (the agent stopped again) does not mark it as handled.
// The subscribe helpers are idempotent; the runningSeen guard only avoids a
// store read on every later status event of the same agent.
func (p *MessageBrokerProxy) ensureSubscriptionsForRunningAgent(projectID, agentID string) {
	p.mu.Lock()
	seen := p.runningSeen[agentID]
	p.mu.Unlock()
	if seen {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), brokerCallbackTimeout)
	defer cancel()
	agent, err := p.store.GetAgent(ctx, agentID)
	if err != nil {
		p.log.Error("Failed to read running agent for broker subscriptions",
			"project_id", projectID, "agent_id", agentID, "error", err)
		return
	}
	if agent.Phase != "running" || agent.ProjectID != projectID {
		return
	}

	p.subscribeAgent(projectID, agent.Slug)
	p.subscribeProjectBroadcast(projectID)
	p.subscribeProjectUserMessages(projectID)

	p.mu.Lock()
	p.runningSeen[agentID] = true
	p.mu.Unlock()
}

// subscribeAgent creates a broker subscription for an individual agent's message topic.
func (p *MessageBrokerProxy) subscribeAgent(projectID, agentSlug string) {
	topic := eventbus.TopicAgentMessages(projectID, agentSlug)

	p.mu.Lock()
	if p.stopped || p.subscribedTopics[topic] {
		p.mu.Unlock()
		return
	}
	p.subscribedTopics[topic] = true
	p.mu.Unlock()

	sub, err := p.bus.Subscribe(topic, func(_ context.Context, t string, msg *messages.StructuredMessage) {
		ctx, cancel := context.WithTimeout(context.Background(), brokerCallbackTimeout)
		defer cancel()
		p.deliverToAgent(ctx, projectID, agentSlug, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for agent messages",
			"projectID", projectID, "agentSlug", agentSlug, "error", err)
		return
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		_ = sub.Unsubscribe()
		return
	}
	p.subscriptions[projectID] = append(p.subscriptions[projectID], sub)
	p.mu.Unlock()

	p.log.Debug("Subscribed to agent messages", "topic", topic, "agentSlug", agentSlug)
}

// subscribeProjectBroadcast creates a broker subscription for project-wide broadcasts
// that fans out to all running agents in the project.
func (p *MessageBrokerProxy) subscribeProjectBroadcast(projectID string) {
	topic := eventbus.TopicProjectBroadcast(projectID)

	p.mu.Lock()
	if p.stopped || p.subscribedTopics[topic] {
		p.mu.Unlock()
		return
	}
	p.subscribedTopics[topic] = true
	p.mu.Unlock()

	sub, err := p.bus.Subscribe(topic, func(_ context.Context, t string, msg *messages.StructuredMessage) {
		ctx, cancel := context.WithTimeout(context.Background(), brokerCallbackTimeout)
		defer cancel()
		p.fanOutToProject(ctx, projectID, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for project broadcast",
			"projectID", projectID, "error", err)
		return
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		_ = sub.Unsubscribe()
		return
	}
	p.subscriptions[projectID] = append(p.subscriptions[projectID], sub)
	p.mu.Unlock()

	p.log.Debug("Subscribed to project broadcast", "topic", topic)
}

// subscribeProjectUserMessages creates a broker subscription for all user-targeted
// messages in a project. When a message arrives, it is persisted to the message
// store and published as a user.message SSE event for connected browser clients.
// The subscription uses a wildcard to cover all users in the project.
//
// It reports whether the persisting subscription is in place when it
// returns (ptone/scion#1906): true only once Subscribe has returned
// successfully, so a caller that publishes next is guaranteed a subscriber.
// It returns false after Stop, when Subscribe fails (the topic is left
// unmarked so a later call retries), or when the bus has no inprocess spoke
// (handlers would never run). Callers that need the message persisted fall
// back to writing it themselves on false.
func (p *MessageBrokerProxy) subscribeProjectUserMessages(projectID string) bool {
	topic := eventbus.TopicAllUserMessages(projectID)

	if hs, ok := p.bus.(interface{ HasSpoke(string) bool }); ok && !hs.HasSpoke(eventbus.InProcessBusName) {
		return false
	}

	// Serialize callers across Subscribe: a concurrent caller (e.g. the
	// lifecycle goroutine and the notifier reacting to the same status
	// event) must not see the topic as subscribed before it is.
	p.userSubMu.Lock()
	defer p.userSubMu.Unlock()

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return false
	}
	if p.subscribedTopics[topic] {
		p.mu.Unlock()
		return true
	}
	p.mu.Unlock()

	sub, err := p.bus.Subscribe(topic, func(hctx context.Context, t string, msg *messages.StructuredMessage) {
		ctx, cancel := context.WithTimeout(context.Background(), brokerCallbackTimeout)
		defer cancel()
		// Carry only the notification ID (log correlation) from the
		// publisher's ctx; delivery keeps its own lifetime.
		ctx = withNotificationID(ctx, notificationIDFromContext(hctx))
		p.deliverToUser(ctx, projectID, t, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for project user messages",
			"projectID", projectID, "error", err)
		return false
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		_ = sub.Unsubscribe()
		return false
	}
	p.subscribedTopics[topic] = true
	p.subscriptions[projectID] = append(p.subscriptions[projectID], sub)
	p.mu.Unlock()

	p.log.Debug("Subscribed to project user messages", "topic", topic)
	return true
}

// notificationIDKey carries a notification ID from the notifier's publish
// into deliverToUser, in process only, so a lost inbox row can be logged
// against the notification it came from.
type notificationIDKey struct{}

func withNotificationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, notificationIDKey{}, id)
}

func notificationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(notificationIDKey{}).(string)
	return id
}

// canPersistUserDM reports whether deliverToUser can persist a DM addressed
// to recipientID. Under G2 write-deny a DM row needs a resolved conversation,
// and DM conversation keys only accept canonical UUIDs
// (messages.DMConversationKey), so a federated or otherwise non-canonical
// principal cannot be persisted there. Shared with the notifier's
// persistsViaInbox so the two cannot drift.
func canPersistUserDM(recipientID string, writeDeny bool) bool {
	if !writeDeny {
		return true
	}
	u, err := uuid.Parse(recipientID)
	return err == nil && u.String() == recipientID
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
		ProjectID:   projectID,
		Sender:      msg.Sender,
		SenderID:    msg.SenderID,
		Recipient:   msg.Recipient,
		RecipientID: msg.RecipientID,
		Msg:         msg.Msg,
		Type:        msg.Type,
		Urgent:      msg.Urgent,
		Broadcasted: msg.Broadcasted,
		AgentID:     agentID,
		Channel:     msg.Channel,
		ThreadID:    msg.ThreadID,
		// This delivery *is* the dispatch; Ent defaults dispatch_state to
		// "pending" if left unset (nc-promote-busy).
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now(),
	}
	// Phase 5 dual-write: resolve-or-create conversation for broker-delivered user messages.
	// Skip broadcasts — they are ephemeral and do not belong to a conversation.
	if !msg.Broadcasted {
		var convResult *messaging.ConversationResult

		// DEF-138 P-3: honour a pre-resolved ConversationID from the
		// upstream handler instead of re-deriving. The handler resolved
		// and stamped structuredMsg before publishing to the broker.
		// Re-deriving here produced the inbound/outbound conversation
		// split: the handler resolved a thread conversation, then the
		// broker re-derived a DM because the agent's reply carries no
		// ThreadID. Note: non-emptiness means "already resolved upstream"
		// — it does NOT mean "the caller asserted this". Provenance is
		// carried by ConversationAsserted (DEF-141).
		if msg.ConversationID != "" {
			storeMsg.ConversationID = msg.ConversationID
			// Build a minimal ConversationResult for divergence logging.
			// We do not re-fetch the conversation row — the handler
			// already looked it up (explicit path) or created it
			// (derivation path), and re-querying would add latency for
			// information we have.
			convResult = &messaging.ConversationResult{
				ConversationID: msg.ConversationID,
			}
		} else if msg.ThreadID != "" {
			var threadOpts []messaging.ThreadConversationOption
			if p.webChatStore != nil {
				threadOpts = append(threadOpts, messaging.WithTopicLookup(p.webChatStore))
			}
			if surface := messaging.ChannelToSurface(msg.Channel, p.log); surface != "native" {
				threadOpts = append(threadOpts, messaging.WithThreadSurface(surface))
			}
			// A25.6 F1/F3: msg.ThreadID may carry a dm: prefix, in which case
			// this resolves as kind=="direct"; register both principals so
			// the conversation is discoverable via `conversation list`.
			threadOpts = append(threadOpts, messaging.WithThreadParticipants(p.store))
			var convErr error
			convResult, convErr = messaging.ResolveOrCreateThreadConversation(ctx, p.store, p.log, msg.ThreadID, projectID, threadOpts...)
			if convErr != nil {
				if p.writeDenyEnabled != nil && p.writeDenyEnabled() {
					messaging.WriteDenialMetrics.Inc("mb.user.thread")
					p.log.Error("conversation resolution failed, message not persisted", "error", convErr)
					return
				}
				p.log.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
			}
		} else if msg.SenderID != "" && msg.RecipientID != "" {
			if !canPersistUserDM(msg.RecipientID, p.writeDenyEnabled != nil && p.writeDenyEnabled()) {
				messaging.WriteDenialMetrics.Inc("mb.user.dm")
				p.log.Error("DM recipient is not a canonical UUID under write-deny, message not persisted",
					"notification_id", notificationIDFromContext(ctx))
				return
			}
			senderKind, sOK := messages.PrincipalKindFromAddress(msg.Sender)
			recipientKind, rOK := messages.PrincipalKindFromAddress(msg.Recipient)
			if sOK && rOK {
				var convErr error
				convResult, convErr = messaging.ResolveOrCreateDMConversation(ctx, p.store, p.store, p.log, senderKind, msg.SenderID, recipientKind, msg.RecipientID)
				if convErr != nil {
					if p.writeDenyEnabled != nil && p.writeDenyEnabled() {
						messaging.WriteDenialMetrics.Inc("mb.user.dm")
						p.log.Error("conversation resolution failed, message not persisted", "error", convErr)
						return
					}
					p.log.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
				}
			} else {
				p.log.Warn("skipping DM conversation resolution: principal kind undetermined",
					"sender", msg.Sender, "sender_ok", sOK, "recipient", msg.Recipient, "recipient_ok", rOK)
			}
		}
		if convResult != nil && storeMsg.ConversationID == "" {
			storeMsg.ConversationID = convResult.ConversationID
		}
		// DEF-141: Classification is a three-way decision on provenance,
		// separated from the honouring block above. Honouring is gated on
		// non-emptiness (P-3, must not regress). Classification branches on
		// ConversationAsserted — never on ConversationID != "".
		switch {
		case msg.ConversationAsserted:
			// Caller named a conversation and the handler authorized it.
			messaging.LogExplicitRouting(p.log, storeMsg.ID, storeMsg.ConversationID)

		case msg.ConversationID != "":
			// Handler-derived and propagated. Deliberately NOT compared:
			// ComputeDivergenceMatch would take both sides from the same input
			// fields in the same request, so the verdict is tautological (DEF-139,
			// [^72]/[^73]). Counting it as a "match" would inflate the board with
			// confirmations that confirm nothing. CheckConversationConsistency
			// below is the independent check and runs on every path regardless.
			messaging.LogDerivedRouting(p.log, storeMsg.ID, storeMsg.ConversationID)

		default:
			// No pre-resolved conversation — compare old-model vs new-model routing.
			oldRouting := messaging.OldRoutingFromMessage(msg.SenderID, msg.RecipientID, msg.ThreadID)
			convID := ""
			actualRef := ""
			if convResult != nil {
				convID = convResult.ConversationID
				actualRef = convResult.ExternalRef
			}
			match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
			messaging.LogDivergence(p.log, messaging.DivergenceEntry{
				MessageID:  storeMsg.ID,
				OldRouting: oldRouting,
				NewRouting: messaging.NewRoutingStr(convID),
				Match:      match,
				Reason:     reason,
			})
		}
		// DEF-3: Independent consistency check against prior messages.
		if consistent := messaging.CheckConversationConsistency(ctx, p.store, storeMsg.ID, storeMsg.ConversationID, msg.ThreadID, msg.SenderID, msg.RecipientID, p.log); !consistent {
			p.log.Warn("DEF-3: conversation consistency mismatch (user message from broker)",
				"message_id", storeMsg.ID, "conversation_id", storeMsg.ConversationID)
		}
	}
	if err := p.store.CreateMessage(ctx, storeMsg); err != nil {
		p.log.Error("Failed to persist user message from broker", "topic", topic, "error", err)
		if notifID := notificationIDFromContext(ctx); notifID != "" {
			// ptone/scion#1906: this subscriber is the notification's only
			// persister, so the inbox row is lost; nothing retries.
			p.log.Warn("User notification inbox row lost: broker persist failed",
				"notification_id", notifID, "topic", topic, "error", err)
		}
		return
	}

	// W7: Link the sender's attachments, recorded before publish, to the message
	// row created here — the ID they need exists nowhere else. Done before the
	// SSE event so a client refetching on it already sees them.
	linkAttachmentRefs(ctx, p.webChatStore, storeMsg.ID, parseAttachmentRefs(msg.Metadata), p.log)
	delete(msg.Metadata, attachmentsMetadataKey) // strip internal transport key

	// Stamp the DM watermark with the store-assigned message ID. The web
	// channel spoke already registered the participant rows and bumped
	// last_activity_at, but it runs before the ID exists — without this the
	// unread indicator (last_message_id != last_read_message_id) never fires
	// for agent replies.
	if p.webChatStore != nil && storeMsg.ThreadID != "" {
		switch {
		case strings.HasPrefix(storeMsg.ThreadID, "dm:"):
			registerDMParticipants(ctx, p.webChatStore, storeMsg.ThreadID)
			if err := p.webChatStore.TouchDMActivity(ctx, storeMsg.ThreadID, storeMsg.ID); err != nil {
				p.log.Error("Failed to stamp DM watermark",
					"thread_id", storeMsg.ThreadID, "error", err)
			}
		case !strings.HasPrefix(storeMsg.ThreadID, "agent:"):
			// Space topic. Same reasoning as DMs: the topic's unread dot is
			// driven by last_message_id, and the spoke stamped only the
			// activity timestamp.
			if err := p.webChatStore.TouchTopicActivity(ctx, storeMsg.ThreadID, storeMsg.ID); err != nil {
				p.log.Error("Failed to stamp topic watermark",
					"thread_id", storeMsg.ThreadID, "error", err)
			}
		}
	}

	// Cross-channel DM fix: when a message arrives from a non-web channel
	// (e.g. Discord), its ThreadID is not a DM key, so TouchDMActivity above
	// is never called. Build the canonical DM key from the sender and
	// recipient and touch it explicitly so the web chat's unread indicator
	// tracks per-conversation, not per-channel.
	//
	// Guard: skip @mention fan-out copies. When a user is @mentioned in a
	// space thread, the routed copy also has SenderID + RecipientID set, but
	// it is NOT a DM — touching DM activity for it would create phantom
	// unread indicators for conversations that don't exist.
	if p.webChatStore != nil && storeMsg.SenderID != "" && storeMsg.RecipientID != "" &&
		!strings.HasPrefix(storeMsg.ThreadID, "dm:") &&
		storeMsg.Type != messages.TypeMention {
		senderKind, sOK := messages.PrincipalKindFromAddress(storeMsg.Sender)
		recipientKind, rOK := messages.PrincipalKindFromAddress(storeMsg.Recipient)
		if sOK && rOK {
			dmKey, err := messages.DMConversationKey(senderKind, storeMsg.SenderID, recipientKind, storeMsg.RecipientID)
			if err == nil {
				registerDMParticipants(ctx, p.webChatStore, dmKey)
				if touchErr := p.webChatStore.TouchDMActivity(ctx, dmKey, storeMsg.ID); touchErr != nil {
					p.log.Error("Failed to stamp cross-channel DM watermark",
						"dm_key", dmKey, "thread_id", storeMsg.ThreadID, "error", touchErr)
				}
			}
		}
	}

	// Publish SSE event so connected browser clients receive real-time inbox updates.
	p.events.PublishUserMessage(ctx, storeMsg, parseAttachmentRefs(msg.Metadata))

	// W6: DM notification for agent → human replies via broker path.
	if p.chatNotifier != nil && storeMsg.ThreadID != "" &&
		strings.HasPrefix(storeMsg.ThreadID, "dm:") &&
		storeMsg.RecipientID != "" && strings.HasPrefix(storeMsg.Sender, "agent:") {
		senderName := strings.TrimPrefix(storeMsg.Sender, "agent:")
		go p.chatNotifier.NotifyDMReceived(context.Background(), storeMsg.RecipientID, ChatMessageContext{
			SenderID:        storeMsg.SenderID,
			SenderName:      senderName,
			ConversationKey: storeMsg.ThreadID,
			Preview:         storeMsg.Msg,
			ProjectID:       projectID,
		})
	}

	// Log to dedicated message audit log
	if p.messageLog != nil {
		logAttrs := []any{
			"project_id", projectID,
			"topic", topic,
			"source", "broker",
		}
		if storeMsg.ConversationID != "" {
			msg.ConversationID = storeMsg.ConversationID
		}
		logAttrs = append(logAttrs, msg.LogAttrs()...)
		p.messageLog.Info("user message delivered via broker", logAttrs...)
	}
}

// subscribeGlobalBroadcast creates a broker subscription for global broadcasts.
func (p *MessageBrokerProxy) subscribeGlobalBroadcast() {
	topic := eventbus.TopicGlobalBroadcast()

	_, err := p.bus.Subscribe(topic, func(_ context.Context, t string, msg *messages.StructuredMessage) {
		ctx, cancel := context.WithTimeout(context.Background(), brokerCallbackTimeout)
		defer cancel()
		p.fanOutGlobal(ctx, msg)
	})
	if err != nil {
		p.log.Error("Failed to subscribe for global broadcast", "error", err)
	}
}

// deliverToAgent dispatches a message to a specific agent via the existing
// DispatchAgentMessage path. ObserverOnly messages are skipped — they were
// already delivered directly and are only published for plugin observers.
//
// Raw forwarding note (ptone/scion#2192 inventory): this function and its
// siblings fanOutToProject/fanOutGlobal forward msg.Raw unchanged with no
// guard. That is intentional and safe here: after ptone/scion#2192, no Hub
// publisher places a raw message on this bus at all. Broadcast and group
// forms reject raw upstream before they would ever publish, the
// still-supported single-agent raw shape is dispatched directly through the
// dispatcher (never through this bus), and the agent-to-agent observer
// copies (agent_dm_operation.go, handlers_agent_messaging.go) are skipped
// entirely for raw. Inbound traffic from plugin adapters never reaches this
// bus either; it is delivered directly by handlers_broker_inbound.go /
// _routed.go, which is where the raw guard for that ingress path lives. Do
// not add a second guard here without first confirming a new Hub-originated
// publisher can put a raw message on this bus — that would be duplicating
// policy, not adding containment.
func (p *MessageBrokerProxy) deliverToAgent(ctx context.Context, projectID, agentSlug string, msg *messages.StructuredMessage) {
	if msg.ObserverOnly {
		return
	}

	// msg may be a pointer shared across broker event-bus subscribers, so it
	// is never mutated in place — a private copy is made once here (cheap:
	// struct fields only, no deep copy needed unless a field below is
	// reassigned) and both the "!" rewrite and the #2257 P2 metadata strip
	// (design auto-offload-large-dm §4.2 item 1) apply to that copy.
	copied := *msg
	msg = &copied
	msg.Metadata = messaging.StripReservedMetadata(msg.Metadata)

	// A leading "!" in the message body acts as an inline interrupt signal:
	// strip the prefix and promote to urgent so the harness is interrupted
	// before delivery — equivalent to --interrupt on the CLI.
	if trimmed := strings.TrimSpace(msg.Msg); strings.HasPrefix(trimmed, "!") {
		content := strings.TrimSpace(trimmed[1:])
		if content == "" {
			content = "interrupt"
		}
		msg.Msg = content
		msg.Urgent = true
	}

	dispatcher := p.getDispatcher()
	if dispatcher == nil {
		p.log.Warn("No dispatcher available, cannot deliver broker message",
			"agentSlug", agentSlug)
		return
	}

	// Validate agent existence BEFORE persisting to avoid orphan message rows.
	agent, err := p.store.GetAgentBySlug(ctx, projectID, agentSlug)
	if err != nil {
		p.log.Warn("Agent not found for broker message delivery",
			"agentSlug", agentSlug, "projectID", projectID, "error", err)
		if errors.Is(err, store.ErrNotFound) {
			p.publishDeliveryFailed(ctx, projectID, agentSlug, msg, err)
		}
		return
	}

	if agent.RuntimeBrokerID == "" {
		p.log.Warn("Agent has no runtime broker, skipping broker message delivery",
			"agentSlug", agentSlug)
		return
	}

	// Phase 2 D5: reauthorize cross-project messages at delivery/retry time.
	// Policy may have changed since the message was enqueued. A denied retry
	// must NOT publish denied content to recipients through history/SSE.
	if p.messageAuthorizer != nil && msg.SenderID != "" {
		decision := p.messageAuthorizer(ctx, msg.SenderID, agent)
		if decision != nil && !decision.Allowed {
			p.log.Warn("broker delivery denied at retry/delivery time",
				"agentSlug", agentSlug,
				"projectID", projectID,
				"sender_id", msg.SenderID,
				"denial_code", decision.Code,
				"reason", decision.Reason,
			)
			// Do NOT persist to recipient-visible history.
			return
		}
	}

	// Migration gate (design agent-reincarnate §3.7, Amendment A25 2a.2):
	// while the recipient is mid-`scion reincarnate`, the message is
	// persisted (so it appears in conversation history for the new
	// generation's catch-up) but never dispatched — the old container may
	// already be stopped and the new one may not be listening yet. This
	// check runs BEFORE the #1820 phase gate below: a migrating agent is
	// necessarily non-"running" for most of the migration, and without this
	// ordering the #1820 gate would silently drop the message instead of
	// deferring it. Checked with the same reincarnationInFlight predicate
	// the worker uses (reincarnate_worker.go) — non-terminal states only;
	// once the migration completes or fails, ordinary delivery resumes.
	//
	// O2 (p2a-r1 review, accepted in part): broadcasts are excluded.
	// Broadcast rows are persisted without a conversation (see below,
	// `!msg.Broadcasted` on the conversation-resolution block) by design —
	// they are ephemeral, project-wide fan-out, not addressed 1:1 — so a
	// deferred broadcast could never be found by the new generation's
	// `scion conversation catch-up`. A broadcast to a migrating agent keeps
	// the pre-existing #1820 rejection instead of a silently-unreachable
	// deferred row.
	deferred := reincarnationInFlight(agent) && !msg.Broadcasted

	// #1820: admission gate — mirror the phase check applied to direct
	// sends (handleAgentMessage for humans, ExecuteAgentDM for agents).
	// A non-running agent cannot receive terminal input; accepting the
	// message would persist a "dispatched" row that the broker then
	// silently drops. Reject before persistence and tell an agent sender.
	// Runs after reauthorization so a denied sender learns nothing about
	// the recipient's phase. Skipped for a migrating agent — the migration
	// gate above already decided this message is deferred, not dropped.
	if !deferred {
		if phaseErr := validateAgentDeliverable(agent); phaseErr != nil {
			p.log.Warn("Rejecting broker message to non-running agent",
				"agentSlug", agentSlug, "projectID", projectID, "phase", agent.Phase)
			p.publishDeliveryFailed(ctx, projectID, agentSlug, msg, errors.New(phaseErr.Message))
			return
		}
	}

	// Persist to message store before delivery attempt (no pending rows).
	// DispatchState reflects the migration gate above: "deferred" for a
	// migrating recipient (never handed to a dispatcher, see below),
	// "dispatched" otherwise (the pre-existing optimistic value; a later
	// dispatch failure below still CASes it to "failed" via MarkMessageFailed).
	initialDispatchState := store.MessageDispatchDispatched
	if deferred {
		initialDispatchState = store.MessageDispatchDeferred
	}
	storeMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     projectID,
		Sender:        msg.Sender,
		SenderID:      msg.SenderID,
		Recipient:     msg.Recipient,
		RecipientID:   msg.RecipientID,
		Msg:           msg.Msg,
		Type:          msg.Type,
		Urgent:        msg.Urgent,
		Broadcasted:   msg.Broadcasted,
		AgentID:       agent.ID,
		DispatchState: initialDispatchState,
		CreatedAt:     time.Now(),
	}
	// Phase 5 dual-write: resolve-or-create conversation for broker-delivered agent messages.
	// Skip broadcasts — they are ephemeral and do not belong to a conversation.
	// convResult is declared here (not inside the block) so Phase 9b(ii)
	// rendering can read it after persistence.
	var convResult *messaging.ConversationResult
	if !msg.Broadcasted {
		if msg.ThreadID != "" {
			var threadOpts []messaging.ThreadConversationOption
			if p.webChatStore != nil {
				threadOpts = append(threadOpts, messaging.WithTopicLookup(p.webChatStore))
			}
			if surface := messaging.ChannelToSurface(msg.Channel, p.log); surface != "native" {
				threadOpts = append(threadOpts, messaging.WithThreadSurface(surface))
			}
			// A25.6 F1/F3: msg.ThreadID may carry a dm: prefix, in which case
			// this resolves as kind=="direct"; register both principals so
			// the conversation is discoverable via `conversation list`.
			threadOpts = append(threadOpts, messaging.WithThreadParticipants(p.store))
			var convErr error
			convResult, convErr = messaging.ResolveOrCreateThreadConversation(ctx, p.store, p.log, msg.ThreadID, projectID, threadOpts...)
			if convErr != nil {
				if p.writeDenyEnabled != nil && p.writeDenyEnabled() {
					messaging.WriteDenialMetrics.Inc("mb.agent.thread")
					p.log.Error("conversation resolution failed, message not persisted", "error", convErr)
					return
				}
				p.log.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
			}
		} else if msg.SenderID != "" && agent.ID != "" {
			if senderKind, ok := messages.PrincipalKindFromAddress(msg.Sender); ok {
				var convErr error
				convResult, convErr = messaging.ResolveOrCreateDMConversation(ctx, p.store, p.store, p.log, senderKind, msg.SenderID, "agent", agent.ID)
				if convErr != nil {
					if p.writeDenyEnabled != nil && p.writeDenyEnabled() {
						messaging.WriteDenialMetrics.Inc("mb.agent.dm")
						p.log.Error("conversation resolution failed, message not persisted", "error", convErr)
						return
					}
					p.log.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
				}
			} else {
				p.log.Warn("skipping DM conversation resolution: sender kind undetermined",
					"sender", msg.Sender, "sender_id", msg.SenderID)
			}
		}
		if convResult != nil {
			storeMsg.ConversationID = convResult.ConversationID
		}
		// Always log divergence — even when convResult is nil, that is a divergence signal.
		oldRouting := messaging.OldRoutingFromMessage(msg.SenderID, agent.ID, msg.ThreadID)
		convID := ""
		actualRef := ""
		if convResult != nil {
			convID = convResult.ConversationID
			actualRef = convResult.ExternalRef
		}
		match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
		messaging.LogDivergence(p.log, messaging.DivergenceEntry{
			MessageID:  storeMsg.ID,
			OldRouting: oldRouting,
			NewRouting: messaging.NewRoutingStr(convID),
			Match:      match,
			Reason:     reason,
		})
		// DEF-3: Independent consistency check against prior messages.
		if consistent := messaging.CheckConversationConsistency(ctx, p.store, storeMsg.ID, convID, msg.ThreadID, msg.SenderID, agent.ID, p.log); !consistent {
			p.log.Warn("DEF-3: conversation consistency mismatch (agent message from broker)",
				"message_id", storeMsg.ID, "conversation_id", convID, "agent_id", agent.ID)
		}
	}
	if err := p.store.CreateMessage(ctx, storeMsg); err != nil {
		p.log.Error("Failed to persist broker message to store", "agentSlug", agentSlug, "error", err)
		return
	}

	// Phase 9b(ii): render the delivery envelope from the persisted message
	// row and conversation result when the envelope switch is ON. The broker
	// delivers DeliveryText verbatim; when empty, it falls back to
	// FormatForDelivery (legacy path).
	if p.writeDenyEnabled != nil && p.writeDenyEnabled() {
		msg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
			MessageID:  storeMsg.ID,
			ConvResult: convResult,
			Msg:        msg,
			CreatedAt:  storeMsg.CreatedAt,
		})
	}

	// Migration gate: the message is persisted above (visible on catch-up)
	// but must not be dispatched while the recipient is mid-migration — the
	// old container may already be gone and the new one may not exist yet.
	if deferred {
		if p.messageLog != nil {
			p.messageLog.Info("broker message deferred: recipient is reincarnating",
				"agent_id", agent.ID, "agent_name", agent.Name, "project_id", agent.ProjectID,
				"message_id", storeMsg.ID, "source", "broker")
		}
		// O2 (p2a-r1 review, accepted in part): tell an agent sender their
		// message was deferred, mirroring publishDeliveryFailed — §3.7's
		// "sender is told" holds on this path too, not just the two
		// synchronous (HTTP) paths.
		p.publishDeliveryDeferred(ctx, agentSlug, msg)
		return
	}

	// The 30s brokerCallbackTimeout is shared with pre-dispatch work above
	// (agent lookup, persistence), so retries get slightly less than 30s.
	if err := dispatchWithBrokerRetry(withDispatchMessageID(ctx, storeMsg.ID), dispatcher, agent, msg.Msg, msg.Urgent, msg); err != nil {
		p.log.Error("Failed to dispatch broker message to agent",
			"agentSlug", agentSlug, "error", err)
		if markErr := markMessageFailed(ctx, p.store, storeMsg.ID, err.Error()); markErr != nil {
			p.log.Error("Failed to mark broker message as failed", "id", storeMsg.ID, "error", markErr)
		}
		p.publishDeliveryFailed(ctx, projectID, agentSlug, msg, err)
		return
	}

	// Log to dedicated message audit log
	if p.messageLog != nil {
		logAttrs := []any{
			"agent_id", agent.ID,
			"agent_name", agent.Name,
			"project_id", agent.ProjectID,
			"source", "broker",
		}
		if storeMsg.ConversationID != "" {
			msg.ConversationID = storeMsg.ConversationID
		}
		logAttrs = append(logAttrs, msg.LogAttrs()...)
		p.messageLog.Info("broker message delivered", logAttrs...)
	}
}

// fanOutToProject dispatches a broadcast message to all running agents in a project.
func (p *MessageBrokerProxy) fanOutToProject(ctx context.Context, projectID string, msg *messages.StructuredMessage) {
	result, err := p.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: projectID,
		Phase:     "running",
	}, store.ListOptions{})
	if err != nil {
		p.log.Error("Failed to list agents for project broadcast fan-out",
			"projectID", projectID, "error", err)
		return
	}

	p.log.Debug("Broadcasting to project agents", "project_id", projectID, "count", len(result.Items))

	// R3b: warn when a broadcast carries an agent-prefixed Sender but no
	// SenderID. Without SenderID the self-skip cannot fire and the sender
	// will silently receive its own broadcast. Do not guess from the slug —
	// that is the bug B5/R1 removed. Just make it loud so it is caught in
	// logs rather than silently regressing.
	// The Broadcasted flag is not checked: these functions are only reached
	// from broadcast subscriptions, and omitting the flag is exactly the
	// class of publisher error R3b guards against.
	if strings.HasPrefix(msg.Sender, "agent:") && msg.SenderID == "" {
		p.log.Warn("Broadcast has agent Sender but empty SenderID — self-skip not possible",
			"sender", msg.Sender, "projectID", projectID)
	}

	for _, agent := range result.Items {
		// B5/R1: skip the sender by ID, not by the display-label Sender field.
		// Sender is a display label that may be in UUID form after the B5
		// auth-derivation override; SenderID is the canonical identity.
		if msg.SenderID != "" && msg.SenderID == agent.ID {
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

	// R3b: same warning as fanOutToProject — see comment there.
	if strings.HasPrefix(msg.Sender, "agent:") && msg.SenderID == "" {
		p.log.Warn("Global broadcast has agent Sender but empty SenderID — self-skip not possible",
			"sender", msg.Sender)
	}

	for _, agent := range result.Items {
		// B5/R1: skip the sender by ID, not by the display-label Sender field.
		if msg.SenderID != "" && msg.SenderID == agent.ID {
			continue
		}
		agentMsg := *msg
		agentMsg.Recipient = "agent:" + agent.Slug
		agentMsg.RecipientID = agent.ID
		p.deliverToAgent(ctx, agent.ProjectID, agent.Slug, &agentMsg)
	}
}

// ListChannels returns the named bus channels when using a FanOutEventBus,
// or nil for single-bus configurations. Used by the message-channels API.
func (p *MessageBrokerProxy) ListChannels() []eventbus.BusChannel {
	if fb, ok := p.bus.(*eventbus.FanOutEventBus); ok {
		return fb.BusChannels()
	}
	return nil
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

// publishDeliveryFailed publishes a DELIVERY_FAILED notification event when
// a broker message cannot be delivered to an agent. If the sender is an agent,
// the notification is dispatched to the sender so it learns about the failure.
// When deliveryErr is a non-ErrNotFound error, the message includes the actual
// error; otherwise it reports the agent as not found.
func (p *MessageBrokerProxy) publishDeliveryFailed(ctx context.Context, projectID, agentSlug string, msg *messages.StructuredMessage, deliveryErr error) {
	if !strings.HasPrefix(msg.Sender, "agent:") || msg.SenderID == "" {
		return
	}
	// ptone/scion#1838: this notice is a post-dispatch finalization. Callers
	// often hold a dispatch ctx that has already expired (broker timeout) or
	// been cancelled, so detach from it with a bounded timeout rather than
	// silently dropping the sender's DELIVERY_FAILED. The notice dispatches
	// through the runtime broker, so it gets deliveryNoticeTimeout rather
	// than the 5s row-CAS budget.
	ctx, cancel := detachedContext(ctx, deliveryNoticeTimeout)
	defer cancel()
	senderAgent, err := p.store.GetAgent(ctx, msg.SenderID)
	if err != nil {
		p.log.Warn("Could not resolve sender agent for DELIVERY_FAILED notification",
			"senderID", msg.SenderID, "error", err)
		return
	}

	var failMsg string
	if deliveryErr != nil && !errors.Is(deliveryErr, store.ErrNotFound) {
		// ptone/scion#1841: deliveryErr can carry a raw broker response
		// body; sanitize here so every DELIVERY_FAILED notice is covered
		// whichever path produced it.
		failMsg = fmt.Sprintf("Message delivery failed to agent %q: %s", agentSlug, sanitizeFailureReason(deliveryErr.Error()))
	} else {
		failMsg = fmt.Sprintf("Message delivery failed: agent %q not found in project", agentSlug)
	}
	structuredMsg := newDeliveryNotice(msg.Sender, senderAgent.ID, failMsg, "DELIVERY_FAILED", messages.SystemCategoryDeliveryFailed)

	dispatcher := p.getDispatcher()
	if dispatcher == nil {
		return
	}
	if err := dispatcher.DispatchAgentMessage(ctx, senderAgent, failMsg, false, structuredMsg); err != nil {
		p.log.Warn("Failed to dispatch DELIVERY_FAILED notification",
			"senderID", msg.SenderID, "error", err)
	}
}

// newDeliveryNotice builds the system notice sent to an agent sender about the
// fate of its message (DELIVERY_FAILED, DELIVERY_DEFERRED). It goes through
// messages.NewSystemMessage so every notice carries Version and an RFC3339 UTC
// Timestamp (ptone/scion#2100); new notice sites should use it rather than a
// StructuredMessage literal so they cannot drift.
func newDeliveryNotice(recipient, recipientID, text, status, category string) *messages.StructuredMessage {
	notice := messages.NewSystemMessage("system", recipient, text, category)
	notice.RecipientID = recipientID
	notice.Status = status
	return notice
}

// publishDeliveryDeferred tells an agent sender that their message to
// agentSlug was deferred by the migration gate (design agent-reincarnate
// §3.7, O2 p2a-r1 review): the recipient is mid-`scion reincarnate`, so the
// message was persisted for catch-up but not dispatched. Mirrors
// publishDeliveryFailed structurally, with a distinct status so a sender
// cannot mistake this for a failure — the message is saved, not dropped.
// No-op for non-agent senders and for a nil dispatcher, same as
// publishDeliveryFailed; a sender that cannot be notified this way still
// has the persisted row available on its own next catch-up.
func (p *MessageBrokerProxy) publishDeliveryDeferred(ctx context.Context, agentSlug string, msg *messages.StructuredMessage) {
	if !strings.HasPrefix(msg.Sender, "agent:") || msg.SenderID == "" {
		return
	}
	senderAgent, err := p.store.GetAgent(ctx, msg.SenderID)
	if err != nil {
		p.log.Warn("Could not resolve sender agent for DELIVERY_DEFERRED notification",
			"senderID", msg.SenderID, "error", err)
		return
	}

	deferredMsg := fmt.Sprintf("agent %q is reincarnating; message saved to history and will be seen on catch-up", agentSlug)
	structuredMsg := newDeliveryNotice(msg.Sender, senderAgent.ID, deferredMsg, "DELIVERY_DEFERRED", messages.SystemCategoryDeliveryDeferred)

	dispatcher := p.getDispatcher()
	if dispatcher == nil {
		return
	}
	if err := dispatcher.DispatchAgentMessage(ctx, senderAgent, deferredMsg, false, structuredMsg); err != nil {
		p.log.Warn("Failed to dispatch DELIVERY_DEFERRED notification",
			"senderID", msg.SenderID, "error", err)
	}
}

// containsSuffix checks if a dot-separated subject string ends with the given suffix.
func containsSuffix(subject, suffix string) bool {
	return len(subject) >= len(suffix) && subject[len(subject)-len(suffix):] == suffix
}
