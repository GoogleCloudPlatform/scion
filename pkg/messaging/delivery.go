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
	"encoding/json"
)

const (
	beginDelimiter = "---BEGIN SCION MESSAGE---"
	endDelimiter   = "---END SCION MESSAGE---"
	deliveryIntro  = "You are receiving a message from the orchestration system:"
)

// ConversationInfo is the conversation context delivered to agents.
type ConversationInfo struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`           // "direct" or "group"
	Surface string `json:"surface"`        // "native", "discord", etc.
	Name    string `json:"name,omitempty"` // human-readable
}

// DeliveryEnvelope is the new agent-facing message format.
// It replaces the old deliveryMessage struct in pkg/messages/format.go.
type DeliveryEnvelope struct {
	Timestamp    string            `json:"timestamp"`
	Conversation *ConversationInfo `json:"conversation,omitempty"`
	From         string            `json:"from"`            // PrincipalRef
	To           []string          `json:"to,omitempty"`    // addressee PrincipalRefs
	Type         string            `json:"type"`            // "message" | "event"
	Event        *EventBody        `json:"event,omitempty"` // Type == "event"
	Msg          string            `json:"msg"`
	Urgent       bool              `json:"urgent,omitempty"`
	Attachments  []string          `json:"attachments,omitempty"`
	ReplyTo      *string           `json:"reply_to,omitempty"` // msg ID
}

// DeliveryOptions captures transport-level options that are not part of the
// message envelope itself (per design section 2.8).
type DeliveryOptions struct {
	Plain bool // deliver raw text only, no JSON wrapper
	Raw   bool // keystroke injection — raw text only
}

// FormatNewDelivery formats a new-style Message with its Addressees and
// conversation context into the delivery envelope for an agent.
// convInfo may be nil when no conversation context is available; the
// "conversation" key is omitted from the envelope rather than fabricated.
// If the message has plain/raw delivery options, only the raw msg text is returned.
//
// isMention, when true, sets the envelope's type to "mention" (instead of
// "message") and forces the "to" field to be present even for a single
// addressee. Both are set explicitly by the routing call site that knows
// whether the message was @-mention-routed.
func FormatNewDelivery(
	msg *Message,
	addrs []Addressee,
	convInfo *ConversationInfo,
	opts DeliveryOptions,
	isMention bool,
) string {
	if opts.Plain || opts.Raw {
		return msg.Body
	}

	env := DeliveryEnvelope{
		Timestamp:    msg.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Conversation: convInfo,
		From:         string(msg.From),
		Type:         typeString(msg.Kind, isMention),
		Event:        msg.Event,
		Msg:          msg.Body,
		Urgent:       msg.Urgent,
		ReplyTo:      msg.ReplyToID,
	}

	// Build addressee principal refs for the "to" field.
	// For non-mention single-recipient (direct) messages the recipient is
	// implicit — omit "to" to reduce envelope noise. Multi-recipient (group)
	// messages still list every addressee so agents know who else received.
	// Mention-routed messages always include "to" regardless of count — a
	// single mentioned agent sees "to" naming itself, confirming no one else
	// was mentioned.
	if len(addrs) > 1 || isMention {
		for _, a := range addrs {
			env.To = append(env.To, a.PrincipalKind+":"+a.PrincipalID)
		}
	}

	// Map attachments to plain paths.
	for _, a := range msg.Attachments {
		env.Attachments = append(env.Attachments, a.Path)
	}

	jsonBytes, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		// Fallback to plain text if JSON marshaling fails.
		return msg.Body
	}

	return deliveryIntro + "\n\n" + beginDelimiter + "\n" + string(jsonBytes) + "\n" + endDelimiter
}

// typeString maps internal MessageKind and the explicit mention flag to the
// three-value wire type: "event", "mention", or "message".
// KindEvent → "event"; isMention → "mention"; everything else → "message".
func typeString(k MessageKind, isMention bool) string {
	if k == KindEvent {
		return "event"
	}
	if isMention {
		return "mention"
	}
	return "message"
}
