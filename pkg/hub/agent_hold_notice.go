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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Hub notices to held agents (ptone/scion#7145).
//
// A held agent (ptone/scion#3433) receives no notice the hub itself sends,
// as it receives no message from a user or an agent (ptone/scion#6859).
// Every hub notice to an agent checks the recipient's hold before it is
// dispatched, and a notice to a held agent is dropped: a notice about one
// moment (an artifact review, a message's delivery outcome) is not sent,
// and a stored agent notification is recorded as failed with reason
// subscriber_held. Nothing is kept back or delivered after the hold is
// lifted. No notice kind is exempt.
//
// When the hold cannot be read, nothing is delivered either. A one-off
// notice is dropped (it has no store to retry from); a stored agent
// notification stays pending, and the dispatch sweep checks it again.

// Hub notice kinds, for logs only.
const (
	heldNoticeArtifactReview   = "artifact_review"
	heldNoticeNotification     = "notification"
	heldNoticeDeliveryFailed   = "delivery_failed"
	heldNoticeDeliveryDeferred = "delivery_deferred"
	heldNoticeBroadcastFailed  = "broadcast_delivery_failed"
)

// agentHoldChecker is the store's active-hold check (store.AgentHoldStore).
type agentHoldChecker interface {
	HasActiveAgentHold(ctx context.Context, agentID string) (bool, error)
}

// noticeHold is the hold state of a hub notice's recipient.
type noticeHold int

const (
	// noticeNotHeld: the recipient has no active hold; the notice may go.
	noticeNotHeld noticeHold = iota
	// noticeHeld: the recipient has an active hold; the notice is dropped.
	noticeHeld
	// noticeHoldUnknown: the hold could not be read; nothing is delivered.
	noticeHoldUnknown
)

// noticeHoldOf returns the hold state of agent, the recipient of a hub
// notice of kind. A held recipient and an unreadable hold (a lookup fault,
// or no checker or agent) are each logged on log (slog.Default() when nil)
// with the agent ID and the notice kind, never the notice text, as an event
// line (logging.LogKindEvent): it is not a message record. They are logged
// at Warn, as the send paths log a refused send to a held agent
// (ptone/scion#6859).
func noticeHoldOf(ctx context.Context, holds agentHoldChecker, log *slog.Logger, agent *store.Agent, kind string) noticeHold {
	if log == nil {
		log = slog.Default()
	}
	if holds == nil || agent == nil {
		log.Warn("Hub notice withheld: the recipient's hold could not be checked",
			"notice_kind", kind, logging.AttrLogKind, logging.LogKindEvent)
		return noticeHoldUnknown
	}
	held, err := holds.HasActiveAgentHold(ctx, agent.ID)
	if err != nil {
		// The fault line also carries the store error (operators need it to
		// act on the fault). It is the hold lookup's error, never notice text.
		log.Warn("Hub notice withheld: recipient hold lookup failed",
			"agent_id", agent.ID, "notice_kind", kind, "error", err,
			logging.AttrLogKind, logging.LogKindEvent)
		return noticeHoldUnknown
	}
	if held {
		log.Warn("Hub notice withheld: recipient agent is held",
			"agent_id", agent.ID, "notice_kind", kind,
			logging.AttrLogKind, logging.LogKindEvent)
		return noticeHeld
	}
	return noticeNotHeld
}

// heldNoticeWithheld reports whether a one-off hub notice of kind must not
// be sent to agent: the agent is held, or its hold could not be read (the
// notice has no store to retry from, so a lookup fault drops it too). The
// logging is noticeHoldOf's.
func heldNoticeWithheld(ctx context.Context, holds agentHoldChecker, log *slog.Logger, agent *store.Agent, kind string) bool {
	return noticeHoldOf(ctx, holds, log, agent, kind) != noticeNotHeld
}

// noticeLogger returns primary when it is set, else fallback. Callers pass
// the dedicated message log and their own logger; the Server's sites use
// recordLog, which makes the same choice.
func noticeLogger(primary, fallback *slog.Logger) *slog.Logger {
	if primary != nil {
		return primary
	}
	return fallback
}

// dropHeldNotification records the stored notification id, addressed to a
// held subscriber, as failed with reason subscriber_held (ptone/scion#7145).
// It claims the notification first (the CAS the dispatch sweep uses), so a
// notification another caller holds is left to that caller; a claim error
// leaves it pending for the sweep to check again.
func (nd *NotificationDispatcher) dropHeldNotification(ctx context.Context, id string) {
	claimed, err := nd.store.ClaimNotificationForDispatch(ctx, id)
	if err != nil {
		nd.log.Error("held subscriber: notification claim failed; left pending",
			"notificationID", id, "error", err)
		return
	}
	if !claimed {
		return
	}
	nd.setOutcome(ctx, id, store.NotificationOutcomeFailed, store.NotificationReasonSubscriberHeld)
}
