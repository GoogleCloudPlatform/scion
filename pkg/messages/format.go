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

// deliveryMessage is the subset of StructuredMessage fields delivered to the agent.
// The recipient and version fields are stripped to save tokens.
type deliveryMessage struct {
	Timestamp   string   `json:"timestamp"`
	Sender      string   `json:"sender"`
	Msg         string   `json:"msg"`
	Type        string   `json:"type"`
	Urgent      bool     `json:"urgent,omitempty"`
	Broadcasted bool     `json:"broadcasted,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
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
		Msg:         msg.Msg,
		Type:        msg.Type,
		Urgent:      msg.Urgent,
		Broadcasted: msg.Broadcasted,
		Attachments: msg.Attachments,
	}

	jsonBytes, err := json.MarshalIndent(dm, "", "  ")
	if err != nil {
		// Fallback to plain text if JSON marshaling fails
		return msg.Msg
	}

	return deliveryIntro + "\n\n" + beginDelimiter + "\n" + string(jsonBytes) + "\n" + endDelimiter
}
