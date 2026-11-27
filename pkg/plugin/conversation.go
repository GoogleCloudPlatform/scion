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
// from an outbound StructuredMessage for broker plugin routing (Phase 11).
//
// Contract for broker plugins (P4 dual-population):
//   - On outbound Publish(), the hub translates stored thread keys
//     ("thread:<project>:<ref>") on external surfaces into surface-native
//     Conversation.ExternalRef values (<ref>), and omits Conversation (nil)
//     for native conversations, direct (dm:*) conversations, and any ref that
//     cannot be safely translated. ConversationID is always preserved.
//   - Call ResolveConversationTarget(msg) (and, for plugins that filter by
//     channel such as Slack/Discord/Teams, MatchesSurface(msg, "<surface>")).
//   - While legacy msg.Channel is populated (P4), spoke selection follows
//     msg.Channel (the hub's per-message routing decision, including reply
//     affinity). Absent or native Conversations (DMs, native threads,
//     agent-to-agent observer copies) never filter out a message and never
//     supply an external target ref.
//   - When msg.Conversation is present with a non-native surface, Surface,
//     ExternalRef, ParentRef and DisplayName come from it; when absent or
//     native, Surface falls back to msg.Channel and ExternalRef falls back to
//     msg.ThreadID.
//   - On inbound HTTP delivery (/api/v1/broker/inbound and
//     /api/v1/broker/inbound/routed), embed InboundConversation (surface,
//     external_ref, optional parent_ref and display_name) at the top level of
//     the request JSON ONLY when HubSupportsInboundConversation(config) is true
//     (hub passes hub_inbound_conversation="1" in Configure). Against older
//     hubs where the flag is absent, plugins omit those fields so inbound falls
//     through to legacy Phase 5 resolution without creating raw-keyed rows.
//   - When (surface, external_ref) is sent to a P4+ hub, the hub maps it onto
//     the canonical thread:<project>:<ref> key (with a read-only fallback to
//     pre-P4 Phase 5 keys when no row exists under the new key) so inbound and
//     outbound threads share one conversation. 1:1 surface DMs without a thread
//     omit external_ref so Phase 5 resolves the shared direct dm: conversation.
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
type InboundConversation struct {
	Surface     string `json:"surface,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`
	ParentRef   string `json:"parent_ref,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// ResolveConversationTarget extracts conversation addressing from msg,
// preferring msg.Conversation (proto field 23) when it carries a non-native
// surface and falling back field-by-field to legacy msg.Channel and
// msg.ThreadID when msg.Conversation is absent or has Surface=="native".
//
// Native conversations (DMs, native webchat threads, agent-to-agent observer
// copies) never supply an external target ref, so hub-internal keys like
// "dm:..." or "thread:..." are not treated as surface addresses.
func ResolveConversationTarget(msg *messages.StructuredMessage) ConversationTarget {
	if msg == nil {
		return ConversationTarget{}
	}
	target := ConversationTarget{
		ConversationID: msg.ConversationID,
	}
	if msg.Conversation != nil && msg.Conversation.Surface != "native" {
		target.Surface = msg.Conversation.Surface
		target.ExternalRef = msg.Conversation.ExternalRef
		target.ParentRef = msg.Conversation.ParentRef
		target.DisplayName = msg.Conversation.DisplayName
	}
	if target.Surface == "" {
		target.Surface = msg.Channel
	}
	if target.ExternalRef == "" {
		target.ExternalRef = msg.ThreadID
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
