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
// Every hub notice to an agent asks heldNoticeWithheld before it is
// dispatched. A notice about one moment (an artifact review, a message's
// delivery outcome) is dropped; an agent notification is parked, as for a
// suspended subscriber, and ends at notificationRetryMaxAge like any other
// pending notification. No notice kind is exempt.

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

// heldNoticeWithheld reports whether a hub notice of kind must not be sent
// to agent: the agent has an active hold, or its hold could not be read (a
// lookup fault withholds the notice). Each withheld notice is logged on log
// (slog.Default() when nil) with the agent ID and the notice kind, never
// the notice text, as an event line (logging.LogKindEvent): it is not a
// message record.
func heldNoticeWithheld(ctx context.Context, holds agentHoldChecker, log *slog.Logger, agent *store.Agent, kind string) bool {
	if log == nil {
		log = slog.Default()
	}
	if holds == nil || agent == nil {
		log.Warn("Hub notice withheld: the recipient's hold could not be checked",
			"notice_kind", kind, logging.AttrLogKind, logging.LogKindEvent)
		return true
	}
	held, err := holds.HasActiveAgentHold(ctx, agent.ID)
	if err != nil {
		log.Warn("Hub notice withheld: recipient hold lookup failed",
			"agent_id", agent.ID, "notice_kind", kind, "error", err,
			logging.AttrLogKind, logging.LogKindEvent)
		return true
	}
	if held {
		log.Warn("Hub notice withheld: recipient agent is held",
			"agent_id", agent.ID, "notice_kind", kind,
			logging.AttrLogKind, logging.LogKindEvent)
		return true
	}
	return false
}

// noticeLogger returns primary when it is set, else fallback.
func noticeLogger(primary, fallback *slog.Logger) *slog.Logger {
	if primary != nil {
		return primary
	}
	return fallback
}
