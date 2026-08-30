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
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// NotificationDispatcher listens for agent status events, matches them against
// notification subscriptions, stores notification records, and dispatches
// messages to subscriber agents.
type NotificationDispatcher struct {
	store            store.Store
	events           EventPublisher
	getDispatcher    func() AgentDispatcher // lazy getter; dispatcher may be set after startup
	log              *slog.Logger
	messageLog       *slog.Logger        // dedicated message audit logger (nil = disabled)
	channelRegistry  *ChannelRegistry    // external notification channels (nil = disabled)
	brokerProxy      *MessageBrokerProxy // broker plugin proxy (nil = no broker, use ChannelRegistry)
	writeDenyEnabled func() bool         // G2 write-deny switch callback (nil = OFF)
	stopCh           chan struct{}
	stopOnce         sync.Once
	wg               sync.WaitGroup
}

// NewNotificationDispatcher creates a new NotificationDispatcher.
// The getDispatcher function is called at dispatch time to resolve the current
// AgentDispatcher, allowing the dispatcher to be set up after the notification
// system starts (e.g. in combined hub+web mode).
func NewNotificationDispatcher(s store.Store, events EventPublisher, getDispatcher func() AgentDispatcher, log *slog.Logger) *NotificationDispatcher {
	return &NotificationDispatcher{
		store:         s,
		events:        events,
		getDispatcher: getDispatcher,
		log:           log,
		stopCh:        make(chan struct{}),
	}
}

// SetBrokerProxy sets the message broker proxy for routing user notifications.
// When set, user-targeted notifications are published through the broker so
// the broker plugin can render them (e.g., as rich chat cards). The
// ChannelRegistry becomes a fallback for deployments without a broker plugin.
func (nd *NotificationDispatcher) SetBrokerProxy(p *MessageBrokerProxy) {
	nd.brokerProxy = p
}

// Start subscribes to agent status and deletion events and spawns goroutines to process them.
func (nd *NotificationDispatcher) Start() {
	statusCh, unsubStatus := nd.events.Subscribe("project.>.agent.status")
	deletedCh, unsubDeleted := nd.events.Subscribe("project.>.agent.deleted")

	nd.wg.Add(1)
	go func() {
		defer nd.wg.Done()
		defer unsubStatus()
		defer unsubDeleted()
		for {
			select {
			case evt, ok := <-statusCh:
				if !ok {
					return
				}
				nd.handleEvent(evt)
			case evt, ok := <-deletedCh:
				if !ok {
					return
				}
				nd.handleDeletedEvent(evt)
			case <-nd.stopCh:
				return
			}
		}
	}()

	nd.log.Info("Notification dispatcher started")
}

// Stop signals the dispatcher goroutine to exit and waits for it to finish.
// It is safe to call multiple times.
func (nd *NotificationDispatcher) Stop() {
	nd.stopOnce.Do(func() {
		close(nd.stopCh)
		nd.wg.Wait()
		nd.log.Info("Notification dispatcher stopped")
	})
}

// handleEvent processes a single agent status event.
func (nd *NotificationDispatcher) handleEvent(evt Event) {
	var statusEvt AgentStatusEvent
	if err := json.Unmarshal(evt.Data, &statusEvt); err != nil {
		nd.log.Error("Failed to unmarshal agent status event", "error", err)
		return
	}

	// Skip events with no agent ID — can happen for system events fired during
	// project creation before any agents exist.
	if statusEvt.AgentID == "" {
		return
	}

	ctx, span := tracer.Start(context.Background(), "hub.notification.evaluate")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.event.type", evt.Subject),
		attribute.String("scion.agent.id", statusEvt.AgentID),
	)

	// Collect subscriptions from both scopes: agent-scoped first (more specific),
	// then project-scoped.
	agentSubs, err := nd.store.GetNotificationSubscriptions(ctx, statusEvt.AgentID)
	if err != nil {
		nd.log.Error("Failed to get agent notification subscriptions",
			"agent_id", statusEvt.AgentID, "error", err)
		return
	}

	projectSubs, err := nd.store.GetNotificationSubscriptionsByProjectScope(ctx, statusEvt.ProjectID)
	if err != nil {
		nd.log.Error("Failed to get project notification subscriptions",
			"project_id", statusEvt.ProjectID, "error", err)
		// Continue with agent-scoped only
		projectSubs = nil
	}

	allSubs := append(agentSubs, projectSubs...)
	if len(allSubs) == 0 {
		return
	}

	// Use activity for matching (notifications trigger on activity changes).
	// Fall back to phase when activity is empty (e.g. phase "error" has no activity).
	matchStatus := statusEvt.Activity
	if matchStatus == "" {
		matchStatus = statusEvt.Phase
	}

	// Deduplicate: one notification per (subscriber_type, subscriber_id).
	// Agent-scoped subscriptions are checked first since they are more specific.
	seen := make(map[string]bool)
	for i := range allSubs {
		sub := &allSubs[i]

		// Dedup across overlapping scopes
		dedupeKey := sub.SubscriberType + ":" + sub.SubscriberID
		if seen[dedupeKey] {
			continue
		}

		if !sub.MatchesActivity(matchStatus) {
			continue
		}

		// Dedup: check if the last notification for this subscription already has this status
		lastStatus, err := nd.store.GetLastNotificationStatus(ctx, sub.ID)
		if err != nil {
			nd.log.Error("Failed to get last notification status",
				"subscriptionID", sub.ID, "error", err)
			continue
		}
		if strings.EqualFold(lastStatus, matchStatus) {
			seen[dedupeKey] = true
			continue
		}

		seen[dedupeKey] = true
		nd.storeAndDispatch(ctx, sub, statusEvt)
	}
}

// handleDeletedEvent processes an agent deletion event.
// It fires DELETED notifications before the cascade delete removes subscriptions.
func (nd *NotificationDispatcher) handleDeletedEvent(evt Event) {
	var deletedEvt AgentDeletedEvent
	if err := json.Unmarshal(evt.Data, &deletedEvt); err != nil {
		nd.log.Error("Failed to unmarshal agent deleted event", "error", err)
		return
	}

	if deletedEvt.AgentID == "" {
		return
	}

	ctx := context.Background()

	// Collect subscriptions from both scopes
	agentSubs, err := nd.store.GetNotificationSubscriptions(ctx, deletedEvt.AgentID)
	if err != nil {
		nd.log.Error("Failed to get agent notification subscriptions for deleted event",
			"agent_id", deletedEvt.AgentID, "error", err)
		agentSubs = nil
	}

	projectSubs, err := nd.store.GetNotificationSubscriptionsByProjectScope(ctx, deletedEvt.ProjectID)
	if err != nil {
		nd.log.Error("Failed to get project notification subscriptions for deleted event",
			"projectID", deletedEvt.ProjectID, "error", err)
		projectSubs = nil
	}

	allSubs := append(agentSubs, projectSubs...)
	if len(allSubs) == 0 {
		return
	}

	// Deduplicate by subscriber and fire DELETED notifications
	seen := make(map[string]bool)
	for i := range allSubs {
		sub := &allSubs[i]

		dedupeKey := sub.SubscriberType + ":" + sub.SubscriberID
		if seen[dedupeKey] {
			continue
		}

		if !sub.MatchesActivity("DELETED") {
			continue
		}

		seen[dedupeKey] = true

		// Build a synthetic status event for storeAndDispatch
		statusEvt := AgentStatusEvent{
			AgentID:   deletedEvt.AgentID,
			ProjectID: deletedEvt.ProjectID,
			Phase:     "stopped",
			Activity:  "DELETED",
		}
		nd.storeAndDispatch(ctx, sub, statusEvt)
	}
}

// storeAndDispatch creates a notification record and dispatches it to the subscriber.
func (nd *NotificationDispatcher) storeAndDispatch(ctx context.Context, sub *store.NotificationSubscription, evt AgentStatusEvent) {
	ctx, span := tracer.Start(ctx, "hub.notification.dispatch")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.subscription.id", sub.ID),
		attribute.String("scion.agent.id", evt.AgentID),
	)

	agent, err := nd.store.GetAgent(ctx, evt.AgentID)
	if err != nil {
		nd.log.Error("Failed to get agent for notification",
			"agent_id", evt.AgentID, "error", err)
		return
	}

	// Skip stale status events that predate this subscription. This prevents
	// retroactive notifications when a new project-scoped subscription is created
	// and existing agents' statuses are re-reported.
	if !sub.CreatedAt.IsZero() {
		activityTime := agent.LastActivityEvent
		if activityTime.IsZero() {
			activityTime = agent.Updated
		}
		if !activityTime.IsZero() && activityTime.Before(sub.CreatedAt) {
			nd.log.Debug("Skipping notification for stale event predating subscription",
				"subscriptionID", sub.ID, "agent_id", evt.AgentID,
				"activityTime", activityTime, "subscriptionCreatedAt", sub.CreatedAt)
			return
		}
	}

	// Use activity for matching/display; fall back to phase when activity is empty.
	effectiveStatus := evt.Activity
	if effectiveStatus == "" {
		effectiveStatus = evt.Phase
	}

	message := formatNotificationMessage(agent, effectiveStatus)

	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: sub.ID,
		AgentID:        evt.AgentID,
		ProjectID:      sub.ProjectID,
		SubscriberType: sub.SubscriberType,
		SubscriberID:   sub.SubscriberID,
		Status:         strings.ToUpper(effectiveStatus),
		Message:        message,
		CreatedAt:      time.Now(),
	}

	if err := nd.store.CreateNotification(ctx, notif); err != nil {
		nd.log.Error("Failed to create notification",
			"subscriptionID", sub.ID, "agent_id", evt.AgentID, "error", err)
		return
	}

	nd.log.Info("Notification created",
		"notificationID", notif.ID, "agent_id", evt.AgentID, "subscriber", sub.SubscriberType+":"+sub.SubscriberID, "status", notif.Status)

	switch sub.SubscriberType {
	case store.SubscriberTypeAgent:
		nd.dispatchToAgent(ctx, sub, notif, agent.ID, agent.Slug)
	case store.SubscriberTypeUser:
		nd.events.PublishNotification(ctx, notif)
		nd.log.Info("Notification dispatched to user via SSE",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID)

		// Persist an inbox message for the web UI.
		nd.createInboxMessage(ctx, sub, notif, agent)

		// Route through the broker so external integrations (Telegram,
		// Discord) receive state-change messages as rich cards.
		if nd.brokerProxy != nil {
			nd.dispatchToBroker(ctx, sub, notif, agent.ID, agent.Slug)
		}

		// Channel registry is a fallback for deployments without a broker.
		nd.dispatchToChannels(ctx, sub, notif, agent.ID, agent.Slug)
	default:
		nd.log.Warn("Unknown subscriber type", "type", sub.SubscriberType)
	}
}

// dispatchToAgent sends a notification message to a subscriber agent as a
// structured message. The sender is the watched agent (agent:<slug>), and
// the type is state-change or input-needed based on the notification status.
func (nd *NotificationDispatcher) dispatchToAgent(ctx context.Context, sub *store.NotificationSubscription, notif *store.Notification, watchedAgentID, watchedSlug string) {
	subscriber, err := nd.store.GetAgentBySlug(ctx, sub.ProjectID, sub.SubscriberID)
	if err != nil {
		nd.log.Warn("Subscriber agent not found, skipping dispatch",
			"subscriberID", sub.SubscriberID, "projectID", sub.ProjectID, "error", err)
		return
	}

	dispatcher := nd.getDispatcher()
	if dispatcher == nil {
		nd.log.Error("No dispatcher available; notification NOT dispatched",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID)
		// Do NOT mark as dispatched — the notification was never delivered.
		// Leaving it undelivered preserves the record and makes the failure
		// explicit; a future retry mechanism can sweep for undelivered
		// notifications and redeliver them once a dispatcher is available.
		return
	}

	if subscriber.RuntimeBrokerID == "" {
		nd.log.Error("Subscriber agent has no runtime broker; notification NOT dispatched",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID)
		// Do NOT mark as dispatched — the notification was never delivered.
		// Leaving it undelivered preserves the record and makes the failure
		// explicit; a future retry mechanism can sweep for undelivered
		// notifications and redeliver them once a broker is assigned.
		return
	}

	// Build structured message for the notification
	msgType := notificationMessageType(notif.Status)
	structuredMsg := messages.NewNotification(
		"agent:"+watchedSlug,
		"agent:"+subscriber.Slug,
		notif.Message,
		msgType,
	)
	structuredMsg.SenderID = watchedAgentID
	structuredMsg.RecipientID = subscriber.ID
	structuredMsg.Status = strings.ToUpper(notif.Status)

	retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer retryCancel()

	if err := dispatchWithBrokerRetry(retryCtx, dispatcher, subscriber, notif.Message, false, structuredMsg); err != nil {
		nd.log.Error("Failed to dispatch notification to agent",
			"subscriberID", sub.SubscriberID, "error", err)
	} else {
		nd.log.Info("Notification dispatched to agent",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID, "brokerID", subscriber.RuntimeBrokerID)
		// Log to dedicated message audit log
		if nd.messageLog != nil {
			logAttrs := []any{
				"agent_id", subscriber.ID,
				"agent_name", subscriber.Name,
				"project_id", subscriber.ProjectID,
				"notification_id", notif.ID,
			}
			logAttrs = append(logAttrs, structuredMsg.LogAttrs()...)
			nd.messageLog.Debug("notification message dispatched", logAttrs...)
		}
	}

	// Mark dispatched regardless of success (best-effort)
	if err := nd.store.MarkNotificationDispatched(ctx, notif.ID); err != nil {
		nd.log.Error("Failed to mark notification dispatched", "notificationID", notif.ID, "error", err)
	}
}

// notificationMessageType returns the structured message type for a notification status.
func notificationMessageType(status string) string {
	if strings.EqualFold(status, "WAITING_FOR_INPUT") {
		return messages.TypeInputNeeded
	}
	return messages.TypeStateChange
}

// dispatchToChannels sends a notification to all configured external notification
// channels. This is fire-and-forget; errors are logged but do not affect the
// notification pipeline.
func (nd *NotificationDispatcher) dispatchToChannels(ctx context.Context, sub *store.NotificationSubscription, notif *store.Notification, watchedAgentID, watchedSlug string) {
	if nd.channelRegistry == nil || nd.channelRegistry.Len() == 0 {
		return
	}

	msgType := notificationMessageType(notif.Status)
	structuredMsg := messages.NewNotification(
		"agent:"+watchedSlug,
		"user:"+sub.SubscriberID,
		notif.Message,
		msgType,
	)
	structuredMsg.SenderID = watchedAgentID
	structuredMsg.RecipientID = sub.SubscriberID
	structuredMsg.Status = strings.ToUpper(notif.Status)

	nd.channelRegistry.Dispatch(ctx, structuredMsg)
}

// dispatchToBroker publishes a user notification through the message broker proxy
// so a broker plugin can render it (e.g., as a rich interactive card in a chat app).
// This is fire-and-forget; errors are logged but do not affect the notification pipeline.
func (nd *NotificationDispatcher) dispatchToBroker(ctx context.Context, sub *store.NotificationSubscription, notif *store.Notification, watchedAgentID, watchedSlug string) {
	msgType := notificationMessageType(notif.Status)
	structuredMsg := messages.NewNotification(
		"agent:"+watchedSlug,
		"user:"+sub.SubscriberID,
		notif.Message,
		msgType,
	)
	structuredMsg.SenderID = watchedAgentID
	structuredMsg.RecipientID = sub.SubscriberID
	structuredMsg.Status = strings.ToUpper(notif.Status)

	if err := nd.brokerProxy.PublishUserMessage(ctx, sub.ProjectID, sub.SubscriberID, structuredMsg); err != nil {
		nd.log.Error("Failed to dispatch notification through broker",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID, "error", err)
	} else {
		nd.log.Info("Notification dispatched to user via broker",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID)
	}
}

// createInboxMessage persists an inbox Message for a user notification so
// that it appears in the user's message feed alongside agent conversations.
// This is the non-broker path; when a broker is present, the broker's
// deliverToUser callback handles message persistence instead.
func (nd *NotificationDispatcher) createInboxMessage(ctx context.Context, sub *store.NotificationSubscription, notif *store.Notification, agent *store.Agent) {
	msgType := notificationMessageType(notif.Status)

	// Use the agent's current message (the raw question/status text) for
	// actionable notifications; fall back to the formatted notification message.
	msgBody := notif.Message
	if agent.Message != "" && strings.EqualFold(notif.Status, "WAITING_FOR_INPUT") {
		msgBody = agent.Message
	}

	storeMsg := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   notif.ProjectID,
		Sender:      "agent:" + agent.Slug,
		SenderID:    agent.ID,
		Recipient:   "user:" + sub.SubscriberID,
		RecipientID: sub.SubscriberID,
		Msg:         msgBody,
		Type:        msgType,
		AgentID:     agent.ID,
		CreatedAt:   time.Now(),
	}

	// Phase 5 dual-write: resolve-or-create DM conversation for inbox notification messages.
	//
	// G2 EXCEPTION — federated subscriber skip stays non-fatal.
	// SubscriberID may be a slug or federated identity rather than a UUID;
	// DMConversationKey requires valid UUIDs for both parties. Denying here
	// means federated users stop receiving notifications entirely. The
	// federated population is counted by the G1 attribution report and blocks
	// the flip at the OPERATOR level; it must not deny per-request.
	if _, parseErr := uuid.Parse(sub.SubscriberID); parseErr != nil {
		nd.log.Warn("skipping DM conversation resolution for inbox message: subscriber ID not a UUID (federated subscriber — G2 exempt)",
			"subscriber_id", sub.SubscriberID, "notification_id", notif.ID)
	} else {
		convResult, convErr := messaging.ResolveOrCreateDMConversation(ctx, nd.store, nd.store, nd.log,
			"agent", agent.ID, "user", sub.SubscriberID)
		if convErr != nil {
			if nd.writeDenyEnabled != nil && nd.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc("notif.inbox")
				nd.log.Error("conversation resolution failed for inbox notification",
					"notification_id", notif.ID, "subscriber_id", sub.SubscriberID, "error", convErr)
				return
			}
			nd.log.Warn("conversation resolution failed for inbox notification (write-deny OFF, continuing)",
				"notification_id", notif.ID, "subscriber_id", sub.SubscriberID, "error", convErr)
		} else {
			storeMsg.ConversationID = convResult.ConversationID
		}
	}

	if err := nd.store.CreateMessage(ctx, storeMsg); err != nil {
		nd.log.Error("Failed to persist inbox message for notification",
			"notificationID", notif.ID, "subscriberID", sub.SubscriberID, "error", err)
		return
	}

	nd.events.PublishUserMessage(ctx, storeMsg)
	nd.log.Debug("Inbox message created for notification",
		"notificationID", notif.ID, "messageID", storeMsg.ID, "subscriberID", sub.SubscriberID)
}

// ---------------------------------------------------------------------------
// Chat notification triggers (W6): human mention + DM received
// ---------------------------------------------------------------------------

// ChatNotificationStatus constants for chat-specific notification triggers.
const (
	ChatNotificationMention    = "MENTION"
	ChatNotificationDMReceived = "DM_RECEIVED"
)

// PresenceChecker reports whether a user has active presence (i.e. is
// currently viewing the chat UI). When a user is actively present, DM
// notifications are suppressed to avoid interrupting them.
//
// The server satisfies this with its PresenceManager (see
// serverPresenceChecker). A nil PresenceChecker (or the NoOpPresenceChecker)
// treats every user as absent, which means DM notifications always fire —
// the conservative default.
type PresenceChecker interface {
	// IsUserActive returns true if the user is actively present
	// (heartbeat within the presence window).
	IsUserActive(userID string) bool
}

// NoOpPresenceChecker always returns false (user is not active).
// Used as the default when no presence source is wired in.
type NoOpPresenceChecker struct{}

// IsUserActive always returns false — the user is assumed absent.
func (NoOpPresenceChecker) IsUserActive(_ string) bool { return false }

// ChatNotifier creates notifications for chat events (human mentions,
// DM received) using the existing notification pipeline (SSE publish +
// store). It is wired into the Server at startup and called from the
// send path in handlers_chat_v2.go.
type ChatNotifier struct {
	store        store.Store
	events       EventPublisher
	webChatStore WebChatStore
	presence     PresenceChecker
	log          *slog.Logger
}

// NewChatNotifier creates a ChatNotifier. If presence is nil, a
// NoOpPresenceChecker is used (DM notifications always fire).
func NewChatNotifier(s store.Store, events EventPublisher, wcs WebChatStore, presence PresenceChecker, log *slog.Logger) *ChatNotifier {
	if presence == nil {
		presence = NoOpPresenceChecker{}
	}
	return &ChatNotifier{
		store:        s,
		events:       events,
		webChatStore: wcs,
		presence:     presence,
		log:          log,
	}
}

// ChatMessageContext describes the chat message that triggered a notification.
// It travels with the notification onto the SSE event, where the client uses
// it to title the browser notification, tag it per conversation, route a click
// to the right conversation, and suppress notifications for its own messages.
//
// It replaces what was a growing list of positional string parameters; at
// eight strings a transposed pair compiles cleanly and misroutes silently.
type ChatMessageContext struct {
	// SenderID is the UUID of the user who sent the message.
	SenderID string
	// SenderName is the sender's display name, falling back to their email.
	SenderName string
	// ConversationKey is the topic UUID, or the dm:<...> key for a DM.
	ConversationKey string
	// ConversationName is the human-readable thread name. Empty for DMs.
	ConversationName string
	// Preview is the raw message text; it is truncated before use.
	Preview string
	// ProjectID is the project UUID; empty for user-to-user DMs.
	ProjectID string
}

// NotifyMention creates a notification for a human user who was @mentioned
// in a thread or DM. It respects the muted flag on the conversation.
func (cn *ChatNotifier) NotifyMention(ctx context.Context, mentionedUserID string, msg ChatMessageContext) {
	if cn == nil || cn.webChatStore == nil {
		return
	}
	senderName, conversationKey := msg.SenderName, msg.ConversationKey

	// Respect muted flag.
	muted, err := cn.webChatStore.IsConversationMuted(ctx, mentionedUserID, conversationKey)
	if err != nil {
		cn.log.Error("Failed to check muted state for mention notification",
			"userID", mentionedUserID, "conversationKey", conversationKey, "error", err)
		return
	}
	if muted {
		cn.log.Debug("Skipping mention notification — conversation muted",
			"userID", mentionedUserID, "conversationKey", conversationKey)
		return
	}

	message := formatChatNotification(ChatNotificationMention, senderName, msg.ConversationName, msg.Preview)

	notif := cn.buildChatNotification(mentionedUserID, ChatNotificationMention, message, msg.ProjectID)

	if err := cn.store.CreateNotification(ctx, notif); err != nil {
		cn.log.Error("Failed to create mention notification",
			"userID", mentionedUserID, "error", err)
		return
	}

	cn.events.PublishChatNotification(ctx, notif, msg)
	cn.log.Info("Mention notification created",
		"notificationID", notif.ID, "mentionedUser", mentionedUserID,
		"sender", senderName, "conversationKey", conversationKey)
}

// NotifyDMReceived creates a notification for a user who received a DM.
// Notifications are skipped when:
//   - the conversation is muted
//   - the recipient has active presence (W5 integration)
func (cn *ChatNotifier) NotifyDMReceived(ctx context.Context, recipientUserID string, msg ChatMessageContext) {
	if cn == nil || cn.webChatStore == nil {
		return
	}
	senderName, conversationKey := msg.SenderName, msg.ConversationKey

	// Respect muted flag.
	muted, err := cn.webChatStore.IsConversationMuted(ctx, recipientUserID, conversationKey)
	if err != nil {
		cn.log.Error("Failed to check muted state for DM notification",
			"userID", recipientUserID, "conversationKey", conversationKey, "error", err)
		return
	}
	if muted {
		cn.log.Debug("Skipping DM notification — conversation muted",
			"userID", recipientUserID, "conversationKey", conversationKey)
		return
	}

	// Skip if recipient has active presence (they're already viewing chat).
	if cn.presence.IsUserActive(recipientUserID) {
		cn.log.Debug("Skipping DM notification — user has active presence",
			"userID", recipientUserID, "conversationKey", conversationKey)
		return
	}

	// F2: resolve agent display name. After the B5 auth-derivation
	// override, the broker path sets SenderName to the raw UUID (the
	// agent ID). Resolve it to the human-readable Name (preferred) or
	// Slug (fallback) for the notification text. The guard ensures we
	// only look up when SenderName IS the UUID — other callers
	// (handlers_agent_messaging.go:353, handlers_chat_v2.go:1293)
	// already pass a proper label and must not be clobbered. This also
	// avoids a pointless GetAgent call on the user-to-user DM path.
	// On lookup failure, fall back to the current label — display
	// resolution must never drop a notification.
	if msg.SenderID != "" && msg.SenderName == msg.SenderID {
		if agent, err := cn.store.GetAgent(ctx, msg.SenderID); err == nil {
			if agent.Name != "" {
				senderName = agent.Name
			} else if agent.Slug != "" {
				senderName = agent.Slug
			}
		}
	}

	message := formatChatNotification(ChatNotificationDMReceived, senderName, "", msg.Preview)

	notif := cn.buildChatNotification(recipientUserID, ChatNotificationDMReceived, message, msg.ProjectID)

	if err := cn.store.CreateNotification(ctx, notif); err != nil {
		cn.log.Error("Failed to create DM notification",
			"userID", recipientUserID, "error", err)
		return
	}

	// A DM has no thread name; make sure a stale one cannot ride along.
	// Also propagate the resolved slug (if any) so the SSE event payload
	// carries the human-readable name, not the raw UUID.
	dmMsg := msg
	dmMsg.SenderName = senderName
	dmMsg.ConversationName = ""
	cn.events.PublishChatNotification(ctx, notif, dmMsg)
	cn.log.Info("DM notification created",
		"notificationID", notif.ID, "recipient", recipientUserID,
		"sender", senderName, "conversationKey", conversationKey)
}

// buildChatNotification creates a store.Notification for chat events.
func (cn *ChatNotifier) buildChatNotification(
	subscriberID, status, message, projectID string,
) *store.Notification {
	nilUUID := uuid.Nil.String()
	effectiveProjectID := projectID
	if effectiveProjectID == "" {
		effectiveProjectID = nilUUID
	}
	return &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: nilUUID,
		AgentID:        nilUUID,
		ProjectID:      effectiveProjectID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   subscriberID,
		Status:         status,
		Message:        message,
		CreatedAt:      time.Now(),
	}
}

// maxChatPreview bounds the message text carried in a notification, in runes.
const maxChatPreview = 100

// truncateChatPreview bounds a message preview for notification display.
// Rune-based so a multi-byte character is never split down the middle.
//
// Shared by the formatted message and the structured event payload: the two
// must show the same amount of the message, or the tray row and the browser
// popup disagree about where the text stops.
func truncateChatPreview(messagePreview string) string {
	runes := []rune(messagePreview)
	if len(runes) > maxChatPreview {
		return string(runes[:maxChatPreview]) + "…"
	}
	return messagePreview
}

// formatChatNotification formats a notification message for chat triggers.
func formatChatNotification(trigger, senderName, conversationName, messagePreview string) string {
	preview := truncateChatPreview(messagePreview)

	switch trigger {
	case ChatNotificationMention:
		if conversationName != "" {
			return fmt.Sprintf("@%s mentioned you in #%s: %s", senderName, conversationName, preview)
		}
		return fmt.Sprintf("@%s mentioned you: %s", senderName, preview)
	case ChatNotificationDMReceived:
		return fmt.Sprintf("%s sent you a message: %s", senderName, preview)
	default:
		return fmt.Sprintf("Chat notification from %s: %s", senderName, preview)
	}
}

// formatNotificationMessage formats a notification message based on agent state and status.
func formatNotificationMessage(agent *store.Agent, status string) string {
	upper := strings.ToUpper(status)
	switch upper {
	case "COMPLETED":
		msg := fmt.Sprintf("%s has reached a state of COMPLETED", agent.Slug)
		if agent.TaskSummary != "" {
			msg += ": " + agent.TaskSummary
		}
		return msg
	case "WAITING_FOR_INPUT":
		msg := fmt.Sprintf("%s is WAITING_FOR_INPUT", agent.Slug)
		if agent.Message != "" {
			msg += ": " + agent.Message
		}
		return msg
	case "LIMITS_EXCEEDED":
		msg := fmt.Sprintf("%s has reached a state of LIMITS_EXCEEDED", agent.Slug)
		if agent.Message != "" {
			msg += ": " + agent.Message
		}
		return msg
	case "STALLED":
		msg := fmt.Sprintf("%s has STALLED", agent.Slug)
		if agent.StalledFromActivity != "" {
			msg += " (was " + agent.StalledFromActivity + ")"
		}
		if agent.Message != "" {
			msg += ": " + agent.Message
		}
		return msg
	case "ERROR":
		msg := fmt.Sprintf("%s has reached a state of ERROR", agent.Slug)
		if agent.Message != "" {
			msg += ": " + agent.Message
		}
		return msg
	case "DELETED":
		return fmt.Sprintf("%s has been DELETED", agent.Slug)
	case "DELIVERY_FAILED":
		msg := fmt.Sprintf("Message delivery to %s failed", agent.Slug)
		if agent.Message != "" {
			msg += ": " + agent.Message
		}
		return msg
	default:
		return fmt.Sprintf("%s has reached status: %s", agent.Slug, upper)
	}
}
