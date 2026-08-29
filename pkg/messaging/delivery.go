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
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`                          // "direct" or "group"
	Surface      string   `json:"surface"`                       // "native", "discord", etc.
	Name         string   `json:"name,omitempty"`                // human-readable
	Participants []string `json:"participants,omitempty"`         // principal refs
}

// DeliveryEnvelope is the new agent-facing message format.
// It replaces the old deliveryMessage struct in pkg/messages/format.go.
type DeliveryEnvelope struct {
	Timestamp    string           `json:"timestamp"`
	Conversation ConversationInfo `json:"conversation"`
	From         string           `json:"from"`                   // PrincipalRef
	To           []string         `json:"to,omitempty"`           // addressee PrincipalRefs
	Kind         MessageKind      `json:"kind"`
	Intent       *TextIntent      `json:"intent,omitempty"`       // Kind == text
	Event        *EventBody       `json:"event,omitempty"`        // Kind == event
	Msg          string           `json:"msg"`
	Visibility   Visibility       `json:"visibility,omitempty"`
	Attachments  []string         `json:"attachments,omitempty"`
	ReplyTo      *string          `json:"reply_to,omitempty"`     // msg ID
}

// DeliveryOptions captures transport-level options that are not part of the
// message envelope itself (per design section 2.8).
type DeliveryOptions struct {
	Plain bool // deliver raw text only, no JSON wrapper
	Raw   bool // keystroke injection — raw text only
}

// FormatNewDelivery formats a new-style Message with its Addressees and
// conversation context into the delivery envelope for an agent.
// If the message has plain/raw delivery options, only the raw msg text is returned.
func FormatNewDelivery(
	msg *Message,
	addrs []Addressee,
	convInfo ConversationInfo,
	opts DeliveryOptions,
) string {
	if opts.Plain || opts.Raw {
		return msg.Body
	}

	env := DeliveryEnvelope{
		Timestamp:    msg.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Conversation: convInfo,
		From:         string(msg.From),
		Kind:         msg.Kind,
		Intent:       msg.Intent,
		Event:        msg.Event,
		Msg:          msg.Body,
		Visibility:   msg.Visibility,
		ReplyTo:      msg.ReplyToID,
	}

	// Build addressee principal refs for the "to" field.
	for _, a := range addrs {
		env.To = append(env.To, a.PrincipalKind+":"+a.PrincipalID)
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
