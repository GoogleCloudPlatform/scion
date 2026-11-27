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

package plugin

import "github.com/GoogleCloudPlatform/scion/pkg/messages"

// ConversationTarget holds the resolved conversation addressing extracted
// from an outbound StructuredMessage for broker plugin routing (Phase 11 / P5).
//
// Contract for broker plugins (P5):
//   - On outbound Publish(), the hub always attaches Conversation whenever
//     ConversationID resolves: external thread rows carry the surface-native
//     ExternalRef (with "thread:<project>:" stripped), ParentRef, DisplayName,
//     and Kind; native, direct, and untranslatable rows carry
//     {Surface: "native", Kind, DisplayName} with empty ExternalRef/ParentRef
//     so hub-internal keys never cross the wire. When ConversationID is empty
//     or lookup fails, the hub clears Conversation to nil.
//   - Call ResolveConversationTarget(msg) (and, for plugins that filter by
//     channel such as Slack/Discord/Teams, MatchesSurface(msg, "<surface>")).
//   - Spoke selection follows msg.Channel (the hub-computed delivery channel,
//     including reply affinity). Absent or native Conversations (DMs, native
//     threads, agent-to-agent observer copies) never filter out a message and
//     never supply an external target ref.
//   - When msg.Conversation is present, ThreadID is NEVER read: Kind and
//     DisplayName come from msg.Conversation, non-native Surface/ExternalRef/
//     ParentRef come from msg.Conversation, and an empty Surface falls back to
//     msg.Channel. Only when msg.Conversation is nil (an older pre-P5a hub)
//     does ResolveConversationTarget fall back to msg.Channel and msg.ThreadID.
//   - Direct messages (target.Kind == "direct") route by Recipient/RecipientID
//     without using ExternalRef or ThreadID.
//   - On inbound HTTP delivery (/api/v1/broker/inbound and
//     /api/v1/broker/inbound/routed), embed InboundConversation (surface,
//     external_ref, optional parent_ref, display_name, and legacy_ref) at the
//     top level of the request JSON ONLY when HubSupportsInboundConversation
//     returns true (hub passes hub_inbound_conversation="1" in Configure).
//     Plugins always set surface (even for 1:1 chats with empty external_ref)
//     and set legacy_ref to the exact pre-P4 thread_id value for read-only
//     continuity lookup.
//   - Exception: scion-chat-app delivers inbound messages through the
//     user-authenticated agent-message API (/api/v1/agents/{id}/message)
//     rather than /api/v1/broker/inbound and is therefore a documented
//     exception to inbound (surface, external_ref) resolution.
type ConversationTarget struct {
	ConversationID string
	Surface        string
	ExternalRef    string
	ParentRef      string
	DisplayName    string
	Kind           string
}

// ConfigKeyHubInboundConversation is the Configure map key injected by hubs
// that support Phase 11 inbound conversation key alignment (mapping top-level
// surface + external_ref onto canonical thread:<project>:<external_ref> keys).
// Broker plugins MUST populate top-level InboundConversation fields only when
// HubSupportsInboundConversation(config) returns true.
const ConfigKeyHubInboundConversation = "hub_inbound_conversation"

// HubSupportsInboundConversation reports whether a plugin Configure map
// advertises hub support for Phase 11 inbound conversation addressing.
func HubSupportsInboundConversation(config map[string]string) bool {
	if config == nil {
		return false
	}
	v := config[ConfigKeyHubInboundConversation]
	return v == "1" || v == "true"
}

// InboundConversation carries the top-level conversation resolution fields
// accepted by POST /api/v1/broker/inbound and POST /api/v1/broker/inbound/routed.
// Broker plugins should embed this struct in their inbound request payloads so
// the JSON field names stay uniform across plugins.
//
// Rules enforced by the hub:
//   - ExternalRef requires Surface to be non-empty.
//   - ParentRef requires ExternalRef to be non-empty.
//   - Surface must be a valid conversation surface (e.g. "slack", "discord",
//     "telegram", "teams", "gchat").
//   - LegacyRef carries the exact pre-P4 message.thread_id value used by the
//     hub's read-only Phase-5 continuity fallback until F1 retires legacy keys.
type InboundConversation struct {
	Surface     string `json:"surface,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`
	ParentRef   string `json:"parent_ref,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	LegacyRef   string `json:"legacy_ref,omitempty"`
}

// ResolveConversationTarget extracts conversation addressing from msg.
//
// When msg.Conversation is present (proto field 23), msg.ThreadID is never
// read: Kind and DisplayName are copied from msg.Conversation, non-native
// Surface/ExternalRef/ParentRef are copied from msg.Conversation, and an empty
// Surface falls back to msg.Channel (the hub-computed delivery channel).
// Native conversations (DMs, native webchat threads, agent-to-agent observer
// copies) never supply an external target ref, so hub-internal keys like
// "dm:..." or "thread:..." are not treated as surface addresses.
//
// Only when msg.Conversation is nil (an older pre-P5a hub) does
// ResolveConversationTarget fall back to msg.Channel and msg.ThreadID.
func ResolveConversationTarget(msg *messages.StructuredMessage) ConversationTarget {
	if msg == nil {
		return ConversationTarget{}
	}
	target := ConversationTarget{
		ConversationID: msg.ConversationID,
	}
	if msg.Conversation == nil {
		target.Surface = msg.Channel
		target.ExternalRef = msg.ThreadID
		return target
	}
	target.Kind = msg.Conversation.Kind
	target.DisplayName = msg.Conversation.DisplayName
	if msg.Conversation.Surface != "native" {
		target.Surface = msg.Conversation.Surface
		target.ExternalRef = msg.Conversation.ExternalRef
		target.ParentRef = msg.Conversation.ParentRef
	}
	if target.Surface == "" {
		target.Surface = msg.Channel
	}
	return target
}

// MatchesSurface reports whether msg is eligible for a broker plugin serving
// surface (e.g. "slack", "discord", "teams") that skips foreign channels.
//
// While legacy msg.Channel is populated (P4), spoke selection follows
// msg.Channel (the hub's per-message routing decision, including reply
// affinity): a non-empty msg.Channel must equal surface. When msg.Channel is
// empty, a non-native Conversation.Surface must equal surface, while an empty
// or "native" surface (untargeted broadcasts, agent-to-agent observer copies)
// returns true.
func MatchesSurface(msg *messages.StructuredMessage, surface string) bool {
	if msg == nil {
		return true
	}
	if msg.Channel != "" {
		return msg.Channel == surface
	}
	if msg.Conversation != nil && msg.Conversation.Surface != "" && msg.Conversation.Surface != "native" {
		return msg.Conversation.Surface == surface
	}
	return true
}
