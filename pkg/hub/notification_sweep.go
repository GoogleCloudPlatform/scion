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
	"errors"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// notificationDispatchSweepHandler returns a recurring handler that queries for
// undispatched agent notifications and re-attempts delivery. Registered as a
// RecurringSingleton guarded by LockNotificationDispatchSweep. This is the
// backstop for notifications that could not be dispatched because the
// subscriber agent had no RuntimeBrokerID, no dispatcher was available, or the
// delivery attempt failed.
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

		// Notifications are processed sequentially under the handler's 30s
		// timeout. This is acceptable because: (1) the batch is capped at 100,
		// (2) each attempt is fast when the broker is reachable (CAS + one
		// RPC), (3) retryBatch skips a broker for the rest of the pass once it
		// is found unreachable, and (4) the sweep is a backstop — the
		// broker-connect hook handles the latency-sensitive path. If
		// sequential processing becomes a bottleneck under sustained load,
		// consider bounded concurrency.
		s.notificationDispatcher.retryBatch(ctx, notifs)
	}
}

// notificationOrphanGCHandler returns a recurring handler that deletes
// acknowledged notifications whose agent and subscription are both gone,
// such as DELETED notifications persisted after an agent's hard delete.
// Unacknowledged ones stay until acknowledged. Registered as a
// RecurringSingleton guarded by LockNotificationOrphanGC.
func (s *Server) notificationOrphanGCHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		purged, err := s.store.PurgeOrphanedNotifications(ctx)
		if err != nil {
			s.agentLifecycleLog.Error("notification-orphan-gc: purge failed", "error", err)
			return
		}
		if purged > 0 {
			s.agentLifecycleLog.Info("notification-orphan-gc: purged notifications",
				"count", purged)
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

	s.notificationDispatcher.retryBatch(ctx, notifs)
}

// retryDispatchAttemptTimeout caps one RetryDispatch delivery attempt. It is
// a variable so tests can shorten it.
var retryDispatchAttemptTimeout = 10 * time.Second

// revertClaimTimeout bounds the store write that releases a retry claim.
const revertClaimTimeout = 5 * time.Second

// notificationRetryMaxAge is how old a notification may be and still have a
// failed delivery reverted for another retry. Past it, RetryDispatch gives up
// and leaves the notification claimed. There is no per-notification attempt
// counter: the 5-minute sweep cadence is the backoff, so this bounds a
// notification to about notificationRetryMaxAge / 5m sweep attempts, plus one
// per broker-connect drain.
const notificationRetryMaxAge = 24 * time.Hour

// retryOutcome is the result of one retryDispatch call.
type retryOutcome int

const (
	// retryNotAttempted: no delivery was attempted (the claim was lost or
	// failed, or a lookup or precondition stopped it first).
	retryNotAttempted retryOutcome = iota
	// retryDelivered: the notification was delivered.
	retryDelivered
	// retryFailed: delivery failed with an error that does not show the
	// broker is unreachable.
	retryFailed
	// retryBrokerUnreachable: delivery was deferred until the attempt
	// timed out, or the attempt hit its deadline: the subscriber's broker
	// cannot be reached right now.
	retryBrokerUnreachable
)

// retryBatch runs retryDispatch over notifs in order (oldest first) within
// ctx's budget, shared by the sweep and the broker-connect drain.
//
// It starts no attempt when less than retryDispatchAttemptTimeout is left
// before ctx's deadline: such an attempt could only be cut short by the
// parent deadline, and the rest of the batch is left for the next pass.
// Once a broker is found unreachable, later notifications for subscribers
// on that broker are skipped (without being claimed) for the rest of the
// pass, so a dead broker costs at most one attempt per pass instead of
// blocking newer, deliverable notifications behind it.
func (nd *NotificationDispatcher) retryBatch(ctx context.Context, notifs []store.Notification) {
	skipBrokers := map[string]bool{}
	for i := range notifs {
		if ctx.Err() != nil {
			return
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < retryDispatchAttemptTimeout {
			nd.log.Debug("retry: budget below one attempt, leaving the rest for the next pass",
				"remaining", len(notifs)-i)
			return
		}
		if len(skipBrokers) > 0 && skipBrokers[nd.subscriberBrokerID(ctx, &notifs[i])] {
			continue
		}
		if outcome, brokerID := nd.retryDispatch(ctx, &notifs[i]); outcome == retryBrokerUnreachable {
			skipBrokers[brokerID] = true
		}
	}
}

// subscriberBrokerID returns the RuntimeBrokerID of notif's subscriber agent,
// or "" if it cannot be read. It is a read only (no claim); a lookup failure
// returns "" so the caller does not skip, and retryDispatch handles the error.
func (nd *NotificationDispatcher) subscriberBrokerID(ctx context.Context, notif *store.Notification) string {
	subscriber, err := nd.store.GetAgentBySlug(ctx, notif.ProjectID, notif.SubscriberID)
	if err != nil || subscriber == nil {
		return ""
	}
	return subscriber.RuntimeBrokerID
}

// revertClaim releases a retry claim (dispatched back to false) so a future
// sweep can retry the notification. It runs on a context the caller's
// deadline cannot cancel: a delivery attempt can end because that deadline
// passed, and a revert on the expired context would fail and leave the
// notification claimed, undelivered and invisible to the sweep.
func (nd *NotificationDispatcher) revertClaim(ctx context.Context, notificationID string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revertClaimTimeout)
	defer cancel()
	if err := nd.store.UnmarkNotificationDispatched(rctx, notificationID); err != nil {
		nd.log.Error("retry: failed to revert claim", "notificationID", notificationID, "error", err)
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
// broker) or the delivery attempt fails, the claim is reverted so a future
// sweep can retry; a failed delivery older than notificationRetryMaxAge stays
// claimed instead.
//
// Delivery is at-least-once: any delivery error is retried, including an
// ambiguous one (a transport error after the request was sent, or the attempt
// timeout cancelling an in-flight request), so a subscriber can occasionally
// receive the same notification twice. That is a deliberate choice for
// state-change notifications, where a duplicate is cheaper than a lost one.
func (nd *NotificationDispatcher) RetryDispatch(ctx context.Context, notif *store.Notification) {
	nd.retryDispatch(ctx, notif)
}

// retryDispatch implements RetryDispatch and reports its outcome. brokerID is
// the subscriber's RuntimeBrokerID when a delivery was attempted, else "".
func (nd *NotificationDispatcher) retryDispatch(ctx context.Context, notif *store.Notification) (outcome retryOutcome, brokerID string) {
	// 1. Claim: atomically mark dispatched (CAS: WHERE dispatched = false).
	claimed, err := nd.store.ClaimNotificationForDispatch(ctx, notif.ID)
	if err != nil {
		nd.log.Error("retry: claim failed", "id", notif.ID, "error", err)
		return retryNotAttempted, ""
	}
	if !claimed {
		return retryNotAttempted, "" // another caller (sweep or hook) already claimed this one
	}

	// 2. Look up subscription context.
	sub, err := nd.store.GetNotificationSubscription(ctx, notif.SubscriptionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Subscription permanently deleted — leave claimed so the sweep
			// does not retry this notification every 5 minutes forever.
			nd.log.Warn("retry: subscription deleted, leaving claimed",
				"subscriptionID", notif.SubscriptionID, "notificationID", notif.ID)
		} else {
			// Transient error (DB timeout, connection issue) — revert claim
			// so a future sweep can retry once the store is reachable again.
			nd.log.Warn("retry: subscription lookup failed, reverting claim",
				"subscriptionID", notif.SubscriptionID, "notificationID", notif.ID, "error", err)
			nd.revertClaim(ctx, notif.ID)
		}
		return retryNotAttempted, ""
	}
	if sub == nil {
		nd.log.Error("retry: subscription lookup returned nil with no error", "subscriptionID", notif.SubscriptionID)
		nd.revertClaim(ctx, notif.ID)
		return retryNotAttempted, ""
	}

	// 3. Look up subscriber agent.
	subscriber, err := nd.store.GetAgentBySlug(ctx, sub.ProjectID, sub.SubscriberID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Subscriber is permanently deleted — leave claimed so the sweep
			// does not retry this notification every 5 minutes forever.
			nd.log.Warn("retry: subscriber agent deleted, leaving claimed",
				"subscriberID", sub.SubscriberID, "notificationID", notif.ID)
		} else {
			// Transient error (DB timeout, connection issue) — revert claim
			// so a future sweep can retry once the store is reachable again.
			nd.log.Warn("retry: subscriber lookup failed, reverting claim",
				"subscriberID", sub.SubscriberID, "notificationID", notif.ID, "error", err)
			nd.revertClaim(ctx, notif.ID)
		}
		return retryNotAttempted, ""
	}
	if subscriber == nil {
		nd.log.Error("retry: subscriber lookup returned nil with no error", "subscriberID", sub.SubscriberID)
		nd.revertClaim(ctx, notif.ID)
		return retryNotAttempted, ""
	}

	// 4. Check preconditions for delivery.
	dispatcher := nd.getDispatcher()
	if dispatcher == nil {
		nd.log.Warn("retry: no dispatcher available, reverting claim",
			"notificationID", notif.ID)
		nd.revertClaim(ctx, notif.ID)
		return retryNotAttempted, ""
	}

	if subscriber.RuntimeBrokerID == "" {
		nd.log.Debug("retry: subscriber has no broker, reverting claim",
			"subscriberID", sub.SubscriberID, "notificationID", notif.ID)
		nd.revertClaim(ctx, notif.ID)
		return retryNotAttempted, ""
	}

	// 5. Resolve watched agent for the structured message.
	watchedAgent, err := nd.store.GetAgent(ctx, notif.AgentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Watched agent permanently deleted — leave claimed (no point retrying).
			nd.log.Warn("retry: watched agent deleted, leaving claimed",
				"agentID", notif.AgentID, "notificationID", notif.ID)
		} else {
			// Transient error — revert claim so a future sweep can retry.
			nd.log.Warn("retry: watched agent lookup failed, reverting claim",
				"agentID", notif.AgentID, "notificationID", notif.ID, "error", err)
			nd.revertClaim(ctx, notif.ID)
		}
		return retryNotAttempted, ""
	}
	if watchedAgent == nil {
		nd.log.Error("retry: watched agent lookup returned nil with no error", "agentID", notif.AgentID)
		nd.revertClaim(ctx, notif.ID)
		return retryNotAttempted, ""
	}

	// 6. Build and deliver.
	// N.B. notif.Status is already uppercase (set by storeAndDispatch).
	msgType := notificationMessageType(notif.Status)
	structuredMsg := messages.NewNotification(
		"agent:"+watchedAgent.Slug,
		"agent:"+subscriber.Slug,
		notif.Message,
		msgType,
	)
	structuredMsg.SenderID = watchedAgent.ID
	structuredMsg.RecipientID = subscriber.ID
	structuredMsg.Status = notif.Status

	// Cap this attempt so one unreachable broker (dispatchWithBrokerRetry
	// keeps retrying ErrMessageDeferred until ctx ends) cannot use up the
	// whole sweep/drain budget shared by the rest of the batch.
	attemptCtx, cancel := context.WithTimeout(ctx, retryDispatchAttemptTimeout)
	defer cancel()
	brokerID = subscriber.RuntimeBrokerID
	if err := dispatchWithBrokerRetry(attemptCtx, nd.messageLog, dispatcher, subscriber, notif.Message, false, structuredMsg); err != nil {
		outcome = retryFailed
		if errors.Is(err, ErrBrokerTimeout) || errors.Is(err, ErrMessageDeferred) || errors.Is(err, context.DeadlineExceeded) {
			outcome = retryBrokerUnreachable
		}
		// The store always sets CreatedAt; a zero value never gives up.
		if !notif.CreatedAt.IsZero() && time.Since(notif.CreatedAt) > notificationRetryMaxAge {
			// Give up: leave claimed so the sweep stops retrying it.
			nd.log.Error("retry: delivery failed past max retry age, giving up",
				"notificationID", notif.ID, "subscriberID", sub.SubscriberID,
				"createdAt", notif.CreatedAt, "error", err)
			return outcome, brokerID
		}
		nd.log.Warn("retry: delivery failed, reverting claim",
			"notificationID", notif.ID, "subscriberID", sub.SubscriberID, "error", err)
		nd.revertClaim(ctx, notif.ID)
		return outcome, brokerID
	}

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
			// The record was written before dispatch; this outcome
			// line carries no record fields or content.
			logging.AttrLogKind, logging.LogKindEvent,
		}
		nd.messageLog.Debug("retry: notification message dispatched", logAttrs...)
	}
	return retryDelivered, brokerID
}
