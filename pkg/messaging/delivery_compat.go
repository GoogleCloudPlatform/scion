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

package messaging

import (
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// FormatLegacyAsNewDelivery takes a legacy StructuredMessage and formats it
// using the new delivery envelope format. This is the transition path: the
// internal representation is still old, but the agent-facing output is new.
//
// convInfo may be nil if conversation context is not available (e.g., messages
// that predate the conversation model). In that case, a minimal conversation
// stub is synthesized from the legacy fields.
func FormatLegacyAsNewDelivery(
	msg *messages.StructuredMessage,
	convInfo *ConversationInfo,
) string {
	if msg == nil {
		return ""
	}

	// Use the legacy Plain/Raw flags as delivery options.
	opts := DeliveryOptions{
		Plain: msg.Plain,
		Raw:   msg.Raw,
	}

	// Short-circuit: plain/raw messages return raw text only.
	if opts.Plain || opts.Raw {
		return msg.Msg
	}

	// Convert legacy message to new types via Phase 6 mapper.
	newMsg, addrs, err := MapLegacyEnvelope(msg)
	if err != nil {
		// If conversion fails, fall back to raw text.
		return msg.Msg
	}

	// If no conversation context is provided, synthesize a minimal stub.
	var conv ConversationInfo
	if convInfo != nil {
		conv = *convInfo
	} else {
		conv = synthesizeConversationInfo(msg)
	}

	return FormatNewDelivery(newMsg, addrs, conv, opts)
}

// synthesizeConversationInfo creates a minimal ConversationInfo from legacy
// message fields when no conversation context is available.
func synthesizeConversationInfo(msg *messages.StructuredMessage) ConversationInfo {
	conv := ConversationInfo{
		Surface: "native",
	}

	// Use channel as conversation ID if available, otherwise use thread_id.
	if msg.Channel != "" {
		conv.ID = msg.Channel
		if msg.ThreadID != "" {
			conv.ID = msg.Channel + "/" + msg.ThreadID
		}
	} else if msg.ThreadID != "" {
		conv.ID = msg.ThreadID
	}

	// Determine kind from whether the message is broadcast or has multiple recipients.
	if msg.Broadcasted || msg.Recipients != "" {
		conv.Kind = "group"
	} else {
		conv.Kind = "direct"
	}

	return conv
}
