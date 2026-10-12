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

// ---------------------------------------------------------------------------
// Messaging ingress inventory
//
// This file documents all messaging surfaces that accept or deliver messages,
// whether they authorize before writes or need delivery-time checks, and their
// cross-project implications. This inventory is a Phase 0 deliverable — it
// captures the current state as observed in the source at commit 1b0a25a.
//
// Each surface is annotated with:
//   [AUTH-BEFORE-WRITE]  Authorization runs before persistence/delivery
//   [AUTH-AT-DELIVERY]   Authorization must run at delivery time (deferred)
//   [BOTH]               Authorization at both creation/enqueue and delivery
//
// ---------------------------------------------------------------------------
//
// 1. Direct delivery — handleAgentMessage
//    File: handlers_agent_messaging.go:handleAgentMessage (resolveOutboundRouting)
//    Auth: [AUTH-BEFORE-WRITE]
//    How:  Calls authorizeAgentMessage with the authenticated sender identity
//          and the resolved target agent record. Authorization precedes message
//          persistence (CreateMessage) and dispatch (managedAgentMessage or
//          runtime broker delivery).
//    Cross-project: Currently denied by the ProjectID != check in
//          authorizeAgentToAgent. Sender/target IDs are derived server-side.
//
// 2. Set fan-out — handleGroupMessage
//    File: handlers_agent_messaging.go:handleGroupMessage
//    Auth: [AUTH-BEFORE-WRITE]
//    How:  Parses group[...] recipient list, resolves each recipient within the
//          sender's project, and calls authorizeAgentMessage per recipient.
//          Each recipient is an independent authorization decision.
//    Cross-project: Resolution is currently anchored to the sender's project
//          (GetAgentBySlug uses the sender's ProjectID). Cross-project
//          recipients would need per-recipient project resolution.
//
// 3. Mentions — processMentions / ValidateCrossProjectAddressees
//    File: handlers_agent_messaging.go:processMentions
//    File: pkg/messaging/validate.go:ValidateCrossProjectAddressees
//    Auth: [AUTH-BEFORE-WRITE]
//    How:  processMentions extracts @mentions from message text and resolves
//          them within the primary target's project. ValidateCrossProjectAddressees
//          rejects mentions that reference agents in different projects.
//    Cross-project: ValidateCrossProjectAddressees is a structural validator
//          requiring one project. For cross-project DM fan-out, this should be
//          replaced with contextual per-target authorization.
//
// 4. Broadcast — handleProjectBroadcast
//    File: handlers_agent_messaging.go:handleProjectBroadcast
//    Auth: [AUTH-BEFORE-WRITE]
//    How:  Agent caller must belong to the broadcast project. All project agents
//          in eligible modes receive the broadcast. Uses authorizeAgentMessage
//          per recipient.
//    Cross-project: Broadcast is inherently project-scoped. External agents
//          should not receive broadcasts unless a separate feature is designed.
//
// 5. Message Broker / Event Bus — messagebroker.go / deliverToAgent
//    File: pkg/hub/messagebroker.go
//    Auth: [AUTH-AT-DELIVERY]
//    How:  The Message Broker receives messages via its callback/event system.
//          deliverToAgent persists and dispatches without calling the central
//          authorizeAgentMessage evaluator itself. It relies on the upstream
//          sender having been authorized.
//    Cross-project: Delivery topics contain destination project and slug.
//          Message Broker callback authentication must carry originating
//          identity explicitly if a delivery is rechecked after queuing.
//          Reconstructing authority from a sender slug is unsafe with
//          identical slugs in different projects.
//    NOTE: This is a delivery-time gap — the broker should reauthorize on
//          deferred delivery/retry to ensure policy changes are respected.
//
// 6. Broker plugin ingress — handlers_broker_inbound.go
//    File: handlers_broker_inbound.go
//    Auth: [AUTH-BEFORE-WRITE]
//    How:  Resolves sender as a user and calls authorizeAgentMessage. A plugin's
//          text must not mint a trusted local-agent identity.
//    Cross-project: Plugin ingress authenticates as an integration/user, not as
//          a local agent. It cannot claim a local-agent sender or system-plane
//          exemption.
//
// 7. Scheduled messages — authorize_scheduled_message.go
//    File: authorize_scheduled_message.go, scheduler handlers
//    Auth: [BOTH]
//    How:  Authorization runs at event creation (authoring time) and again at
//          fire time. The target project must currently match the event's project.
//          Author kind/ID and credential restrictions are preserved.
//    Cross-project: Target project is currently bound to the event's project.
//          Cross-project scheduled messages would need separate target project
//          fields and re-authorization at fire/retry.
//    NOTE: Scheduled events are NOT system-plane — they are user/agent-authored
//          and must be authorized with isSystemPlane=false at both authoring and
//          fire time. The statement in docs/messaging-authorization.md that
//          scheduled events are system-plane is stale.
//
// 8. Native chat — handlers_chat_v2.go
//    File: handlers_chat_v2.go
//    Auth: [AUTH-BEFORE-WRITE]
//    How:  User-specific handlers for webchat. Uses separate topic-project
//          access checks, mentions handling, and DM-key history logic.
//    Cross-project: User-to-agent chat is currently scoped to the agent's
//          project. Cross-project DMs between users and agents would need
//          project-aware routing.
//
// 9. Non-HTTP callbacks
//    Auth: [AUTH-AT-DELIVERY] (varies)
//    How:  Some internal callbacks (e.g., Runtime Broker agent wake/interrupt,
//          state-change notifications) deliver messages without going through
//          HTTP handlers. These use system-plane or rely on the upstream
//          authorization context.
//    Cross-project: These callbacks should carry and verify the originating
//          authorization context when crossing project boundaries.
//
// ---------------------------------------------------------------------------
// Summary of authorization gaps requiring attention before cross-project:
//
// 1. Message Broker (deliverToAgent) does not reauthorize on deferred
//    delivery/retry — it trusts upstream authorization. For cross-project,
//    delivery-time reauthorization is needed to respect policy changes
//    (e.g., Hub disable, mode changes) between enqueue and dispatch.
//
// 2. Sender identity in broker callbacks is carried via slug/address, not
//    immutable (kind, ID) pairs. With identical slugs in different projects,
//    this is unsafe for cross-project messaging.
//
// 3. Scheduled message fire-time reauthorization exists but currently
//    constrains target to the event's project. Cross-project scheduling
//    needs separate target project fields.
//
// 4. ValidateCrossProjectAddressees is a blanket one-project validator.
//    Cross-project fan-out needs per-target authorization instead.
// ---------------------------------------------------------------------------

import "testing"

func TestMessagingIngressInventory_Documented(t *testing.T) {
	// This test serves as an executable documentation anchor for the messaging
	// ingress inventory above. The inventory is maintained as comments in this
	// file, and this test verifies the file compiles and runs as part of CI.
	//
	// Each ingress surface is annotated with its authorization timing:
	// - AUTH-BEFORE-WRITE: Authorization runs before persistence (safe for writes)
	// - AUTH-AT-DELIVERY: Authorization must run at delivery time (deferred check needed)
	// - BOTH: Authorization at both creation/enqueue and delivery
	//
	// Surfaces documented: 9 total
	// AUTH-BEFORE-WRITE: Direct delivery, Set fan-out, Mentions, Broadcast,
	//                    Broker plugin ingress, Native chat
	// AUTH-AT-DELIVERY:  Message Broker/Event Bus, Non-HTTP callbacks
	// BOTH:              Scheduled messages
	t.Log("Messaging ingress inventory documented in this file (9 surfaces)")
}
