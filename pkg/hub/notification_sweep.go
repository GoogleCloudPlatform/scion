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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// notificationDispatchSweepHandler returns a recurring handler that queries for
// undispatched agent notifications and re-attempts delivery. Registered as a
// RecurringSingleton guarded by LockNotificationDispatchSweep. This is the
// backstop for notifications that could not be dispatched because the
// subscriber agent had no RuntimeBrokerID or no dispatcher was available.
func (s *Server) notificationDispatchSweepHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		if s.notificationDispatcher == nil {
			return
		}

		notifs, err := s.store.GetUndispatchedAgentNotifications(ctx, "")
		if err != nil {
			s.agentLifecycleLog.Error("notification-dispatch-sweep: query failed", "error", err)
			return
		}
		if len(notifs) == 0 {
			return
		}

		s.agentLifecycleLog.Info("notification-dispatch-sweep: found undispatched",
			"count", len(notifs))

		for i := range notifs {
			s.notificationDispatcher.RetryDispatch(ctx, &notifs[i])
		}
	}
}

// drainUndispatchedNotifications delivers undispatched notifications for agents
// on the given broker. Called as a goroutine from markBrokerOnline for sub-second
// delivery when a broker first connects.
func (s *Server) drainUndispatchedNotifications(ctx context.Context, brokerID string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if s.notificationDispatcher == nil {
		return
	}

	notifs, err := s.store.GetUndispatchedAgentNotifications(ctx, brokerID)
	if err != nil {
		s.agentLifecycleLog.Error("broker-connect notification drain: query failed",
			"brokerID", brokerID, "error", err)
		return
	}
	if len(notifs) == 0 {
		return
	}

	s.agentLifecycleLog.Info("broker-connect notification drain: delivering",
		"brokerID", brokerID, "count", len(notifs))

	for i := range notifs {
		s.notificationDispatcher.RetryDispatch(ctx, &notifs[i])
	}
}

// RetryDispatch re-attempts delivery of an existing undispatched notification.
// It is the shared code path for both the periodic sweep backstop and the
// broker-connect fast-path hook.
//
// Claim-first semantics: ClaimNotificationForDispatch (CAS on dispatched=false)
// is called before delivery to prevent double-dispatch when the sweep and hook
// race on the same notification. If the claim fails (another caller already
// claimed it), this is a no-op. If delivery cannot proceed (no dispatcher, no
// broker), the claim is reverted so a future sweep can retry.
func (nd *NotificationDispatcher) RetryDispatch(ctx context.Context, notif *store.Notification) {
	// 1. Claim: atomically mark dispatched (CAS: WHERE dispatched = false).
	claimed, err := nd.store.ClaimNotificationForDispatch(ctx, notif.ID)
	if err != nil {
		nd.log.Error("retry: claim failed", "id", notif.ID, "error", err)
		return
	}
	if !claimed {
		return // another caller (sweep or hook) already claimed this one
	}

	// 2. Look up subscription context.
	sub, err := nd.store.GetNotificationSubscription(ctx, notif.SubscriptionID)
	if err != nil {
		nd.log.Warn("retry: subscription not found, leaving claimed",
			"subscriptionID", notif.SubscriptionID, "notificationID", notif.ID, "error", err)
		return
	}

	// 3. Look up subscriber agent.
	subscriber, err := nd.store.GetAgentBySlug(ctx, sub.ProjectID, sub.SubscriberID)
	if err != nil {
		nd.log.Warn("retry: subscriber agent not found",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID, "error", err)
		// Revert claim so a future sweep can retry (agent may be temporarily
		// unreachable during a restart).
		if err2 := nd.store.UnmarkNotificationDispatched(ctx, notif.ID); err2 != nil {
			nd.log.Error("retry: failed to revert claim", "notificationID", notif.ID, "error", err2)
		}
		return
	}

	// 4. Check preconditions for delivery.
	dispatcher := nd.getDispatcher()
	if dispatcher == nil {
		nd.log.Warn("retry: no dispatcher available, reverting claim",
			"notificationID", notif.ID)
		if err2 := nd.store.UnmarkNotificationDispatched(ctx, notif.ID); err2 != nil {
			nd.log.Error("retry: failed to revert claim", "notificationID", notif.ID, "error", err2)
		}
		return
	}

	if subscriber.RuntimeBrokerID == "" {
		nd.log.Debug("retry: subscriber has no broker, reverting claim",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID)
		if err2 := nd.store.UnmarkNotificationDispatched(ctx, notif.ID); err2 != nil {
			nd.log.Error("retry: failed to revert claim", "notificationID", notif.ID, "error", err2)
		}
		return
	}

	// 5. Resolve watched agent for the structured message.
	watchedAgent, err := nd.store.GetAgent(ctx, notif.AgentID)
	if err != nil {
		nd.log.Warn("retry: watched agent not found, leaving claimed",
			"agentID", notif.AgentID, "notificationID", notif.ID, "error", err)
		// Agent may have been deleted — leave claimed (no point retrying).
		return
	}

	// 6. Build and deliver.
	msgType := notificationMessageType(notif.Status)
	structuredMsg := messages.NewNotification(
		"agent:"+watchedAgent.Slug,
		"agent:"+subscriber.Slug,
		notif.Message,
		msgType,
	)
	structuredMsg.SenderID = watchedAgent.ID
	structuredMsg.RecipientID = subscriber.ID
	structuredMsg.Status = strings.ToUpper(notif.Status)

	retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer retryCancel()

	if err := dispatchWithBrokerRetry(retryCtx, dispatcher, subscriber, notif.Message, false, structuredMsg); err != nil {
		nd.log.Error("retry: delivery failed, leaving claimed",
			"notificationID", notif.ID, "subscriberID", sub.SubscriberID, "error", err)
		// Leave claimed (dispatched=true). The delivery attempt was made; the
		// current contract is best-effort (same as the primary dispatch path).
	} else {
		nd.log.Info("retry: notification delivered",
			"notificationID", notif.ID, "subscriberID", sub.SubscriberID,
			"brokerID", subscriber.RuntimeBrokerID)
		// Log to dedicated message audit log if available.
		if nd.messageLog != nil {
			logAttrs := []any{
				"agent_id", subscriber.ID,
				"agent_name", subscriber.Name,
				"project_id", subscriber.ProjectID,
				"notification_id", notif.ID,
			}
			logAttrs = append(logAttrs, structuredMsg.LogAttrs()...)
			nd.messageLog.Debug("retry: notification message dispatched", logAttrs...)
		}
	}
}
