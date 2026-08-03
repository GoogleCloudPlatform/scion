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

package messages

import (
	"encoding/json"
)

const (
	beginDelimiter = "---BEGIN SCION MESSAGE---"
	endDelimiter   = "---END SCION MESSAGE---"
	deliveryIntro  = "You are receiving a message from the orchestration system:"
)

// deliveryMetadataAllowlist defines the metadata keys that are forwarded to
// agents during delivery. Platform-specific keys (e.g. telegram_chat_id) are
// excluded to avoid leaking implementation details.
//
// "channel" and "thread_id" duplicate first-class fields on deliveryMessage
// but are included here for completeness — callers may set them as metadata
// instead of (or in addition to) the top-level fields.
var deliveryMetadataAllowlist = map[string]bool{
	"mention_source":   true,
	"mention_position": true,
	"channel":          true,
	"thread_id":        true,
	"system_category":  true,
}

// deliveryMessage is the subset of StructuredMessage fields delivered to the agent.
// The recipient and version fields are stripped to save tokens.
type deliveryMessage struct {
	Timestamp   string            `json:"timestamp"`
	Sender      string            `json:"sender"`
	Recipients  string            `json:"recipients,omitempty"`
	Msg         string            `json:"msg"`
	Type        string            `json:"type"`
	Urgent      bool              `json:"urgent,omitempty"`
	Broadcasted bool              `json:"broadcasted,omitempty"`
	Attachments []string          `json:"attachments,omitempty"`
	Channel     string            `json:"channel,omitempty"`
	ThreadID    string            `json:"thread_id,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// FormatForDelivery formats a structured message for delivery to an agent via tmux.
// If the message has plain=true, only the raw msg text is returned.
// The recipient and version fields are stripped before delivery.
func FormatForDelivery(msg *StructuredMessage) string {
	if msg.Plain || msg.Raw {
		return msg.Msg
	}

	dm := deliveryMessage{
		Timestamp:   msg.Timestamp,
		Sender:      msg.Sender,
		Recipients:  msg.Recipients,
		Msg:         msg.Msg,
		Type:        msg.Type,
		Urgent:      msg.Urgent,
		Broadcasted: msg.Broadcasted,
		Attachments: msg.Attachments,
		Channel:     msg.Channel,
		ThreadID:    msg.ThreadID,
		Metadata:    filterMetadata(msg.Metadata),
	}

	jsonBytes, err := json.MarshalIndent(dm, "", "  ")
	if err != nil {
		// Fallback to plain text if JSON marshaling fails
		return msg.Msg
	}

	return deliveryIntro + "\n\n" + beginDelimiter + "\n" + string(jsonBytes) + "\n" + endDelimiter
}

// filterMetadata returns a copy of m containing only the keys in
// deliveryMetadataAllowlist. Returns nil when no keys match.
func filterMetadata(m map[string]string) map[string]string {
	var filtered map[string]string
	for k, v := range m {
		if deliveryMetadataAllowlist[k] {
			if filtered == nil {
				filtered = make(map[string]string)
			}
			filtered[k] = v
		}
	}
	return filtered
}
