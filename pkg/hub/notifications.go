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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// NotificationDispatcher listens for agent status events, matches them against
// notification subscriptions, stores notification records, and dispatches
// messages to subscriber agents.
type NotificationDispatcher struct {
	store           store.Store
	events          *ChannelEventPublisher
	getDispatcher   func() AgentDispatcher // lazy getter; dispatcher may be set after startup
	log             *slog.Logger
	messageLog      *slog.Logger        // dedicated message audit logger (nil = disabled)
	channelRegistry *ChannelRegistry    // external notification channels (nil = disabled)
	brokerProxy     *MessageBrokerProxy // broker plugin proxy (nil = no broker, use ChannelRegistry)
	stopCh          chan struct{}
	stopOnce        sync.Once
	wg              sync.WaitGroup
}

// NewNotificationDispatcher creates a new NotificationDispatcher.
// The getDispatcher function is called at dispatch time to resolve the current
// AgentDispatcher, allowing the dispatcher to be set up after the notification
// system starts (e.g. in combined hub+web mode).
func NewNotificationDispatcher(s store.Store, events *ChannelEventPublisher, getDispatcher func() AgentDispatcher, log *slog.Logger) *NotificationDispatcher {
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

	ctx := context.Background()

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
			AgentID:  deletedEvt.AgentID,
			ProjectID:  deletedEvt.ProjectID,
			Phase:    "stopped",
			Activity: "DELETED",
		}
		nd.storeAndDispatch(ctx, sub, statusEvt)
	}
}

// storeAndDispatch creates a notification record and dispatches it to the subscriber.
func (nd *NotificationDispatcher) storeAndDispatch(ctx context.Context, sub *store.NotificationSubscription, evt AgentStatusEvent) {
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
		ProjectID:        sub.ProjectID,
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

		// Persist an inbox message for the web UI and fall back to the
		// channel registry for external delivery. Notifications are NOT
		// routed through the broker plugin so that only explicit
		// "scion message" commands produce chat messages.
		nd.createInboxMessage(ctx, sub, notif, agent)
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
		nd.log.Warn("No dispatcher available, skipping notification dispatch",
			"subscriberID", sub.SubscriberID)
		// Mark dispatched anyway (best-effort)
		if err := nd.store.MarkNotificationDispatched(ctx, notif.ID); err != nil {
			nd.log.Error("Failed to mark notification dispatched", "notificationID", notif.ID, "error", err)
		}
		return
	}

	if subscriber.RuntimeBrokerID == "" {
		nd.log.Warn("Subscriber agent has no runtime broker, skipping dispatch",
			"subscriberID", sub.SubscriberID)
		if err := nd.store.MarkNotificationDispatched(ctx, notif.ID); err != nil {
			nd.log.Error("Failed to mark notification dispatched", "notificationID", notif.ID, "error", err)
		}
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

	if err := dispatcher.DispatchAgentMessage(ctx, subscriber, notif.Message, false, structuredMsg); err != nil {
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
		ProjectID:     notif.ProjectID,
		Sender:      "agent:" + agent.Slug,
		SenderID:    agent.ID,
		Recipient:   "user:" + sub.SubscriberID,
		RecipientID: sub.SubscriberID,
		Msg:         msgBody,
		Type:        msgType,
		AgentID:     agent.ID,
		CreatedAt:   time.Now(),
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
	default:
		return fmt.Sprintf("%s has reached status: %s", agent.Slug, upper)
	}
}
