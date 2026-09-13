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
	"testing"
)

// TestFormatForDelivery_ByteIdentity is the Phase 9a safety net (AC-9-1).
//
// It asserts that FormatForDelivery output is byte-identical to a pinned
// golden string for a representative spread of inputs. Later phases that
// introduce a new delivery envelope must NOT weaken this test — the
// byte-identity guarantee protects the switch-off path.
//
// Inputs covered: plain, raw, urgent, broadcast, threaded, with attachments,
// with metadata, group-set with recipients, system message.
func TestFormatForDelivery_ByteIdentity(t *testing.T) {
	tests := []struct {
		name   string
		msg    *StructuredMessage
		golden string
	}{
		{
			name: "plain",
			msg: &StructuredMessage{
				Version:   Version,
				Timestamp: "2026-08-01T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:dev",
				Msg:       "just raw text",
				Type:      TypeInstruction,
				Plain:     true,
			},
			golden: "just raw text",
		},
		{
			name: "raw",
			msg: &StructuredMessage{
				Version:   Version,
				Timestamp: "2026-08-01T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:dev",
				Msg:       "Escape",
				Type:      TypeInstruction,
				Raw:       true,
			},
			golden: "Escape",
		},
		{
			name: "urgent",
			msg: &StructuredMessage{
				Version:   Version,
				Timestamp: "2026-08-01T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:dev",
				Msg:       "fix this now",
				Type:      TypeInstruction,
				Urgent:    true,
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"msg\": \"fix this now\",\n" +
				"  \"type\": \"instruction\",\n" +
				"  \"urgent\": true\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "broadcast",
			msg: &StructuredMessage{
				Version:     Version,
				Timestamp:   "2026-08-01T10:00:00Z",
				Sender:      "user:alice",
				Recipient:   "agent:dev",
				Msg:         "attention all",
				Type:        TypeInstruction,
				Broadcasted: true,
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"msg\": \"attention all\",\n" +
				"  \"type\": \"instruction\",\n" +
				"  \"broadcasted\": true\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "threaded",
			msg: &StructuredMessage{
				Version:   Version,
				Timestamp: "2026-08-01T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:dev",
				Msg:       "thread reply",
				Type:      TypeInstruction,
				Channel:   "web",
				ThreadID:  "abc-123",
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"msg\": \"thread reply\",\n" +
				"  \"type\": \"instruction\",\n" +
				"  \"channel\": \"web\",\n" +
				"  \"thread_id\": \"abc-123\"\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "with_attachments",
			msg: &StructuredMessage{
				Version:     Version,
				Timestamp:   "2026-08-01T10:00:00Z",
				Sender:      "user:alice",
				Recipient:   "agent:dev",
				Msg:         "review these files",
				Type:        TypeInstruction,
				Attachments: []string{"src/auth.go", "src/middleware.go"},
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"msg\": \"review these files\",\n" +
				"  \"type\": \"instruction\",\n" +
				"  \"attachments\": [\n" +
				"    \"src/auth.go\",\n" +
				"    \"src/middleware.go\"\n" +
				"  ]\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "with_metadata",
			msg: &StructuredMessage{
				Version:   Version,
				Timestamp: "2026-08-01T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:dev",
				Msg:       "from a mention",
				Type:      TypeMention,
				Metadata: map[string]string{
					"mention_source":   "agent:primary",
					"mention_position": "body",
				},
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"msg\": \"from a mention\",\n" +
				"  \"type\": \"mention\",\n" +
				"  \"metadata\": {\n" +
				"    \"mention_position\": \"body\",\n" +
				"    \"mention_source\": \"agent:primary\"\n" +
				"  }\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "group_set_with_recipients",
			msg: &StructuredMessage{
				Version:    Version,
				Timestamp:  "2026-08-01T10:00:00Z",
				Sender:     "user:alice",
				Recipient:  "agent:coder",
				Recipients: "set[user:alice,agent:coder,agent:reviewer]",
				Msg:        "review this",
				Type:       TypeGroupSet,
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"recipients\": \"set[user:alice,agent:coder,agent:reviewer]\",\n" +
				"  \"msg\": \"review this\",\n" +
				"  \"type\": \"group-set\"\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "system_message",
			msg:  NewSystemMessage("system", "agent:dev", "Port 8080 has been auto-exposed", SystemCategoryPortForward),
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"DYNAMIC\",\n" +
				"  \"sender\": \"system\",\n" +
				"  \"msg\": \"Port 8080 has been auto-exposed\",\n" +
				"  \"type\": \"system\",\n" +
				"  \"metadata\": {\n" +
				"    \"system_category\": \"port-forward\"\n" +
				"  }\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "basic_instruction",
			msg: &StructuredMessage{
				Version:   Version,
				Timestamp: "2026-08-01T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:dev",
				Msg:       "implement auth",
				Type:      TypeInstruction,
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"msg\": \"implement auth\",\n" +
				"  \"type\": \"instruction\"\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "metadata_filtering",
			msg: &StructuredMessage{
				Version:   Version,
				Timestamp: "2026-08-01T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:dev",
				Msg:       "hello",
				Type:      TypeInstruction,
				Metadata: map[string]string{
					"system_category":  "port-forward",
					"telegram_chat_id": "secret-should-be-filtered",
				},
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:alice\",\n" +
				"  \"msg\": \"hello\",\n" +
				"  \"type\": \"instruction\",\n" +
				"  \"metadata\": {\n" +
				"    \"system_category\": \"port-forward\"\n" +
				"  }\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
		{
			name: "urgent_broadcast_threaded_with_attachments",
			msg: &StructuredMessage{
				Version:     Version,
				Timestamp:   "2026-08-01T10:00:00Z",
				Sender:      "user:admin",
				Recipient:   "agent:lead",
				Msg:         "full featured message",
				Type:        TypeInstruction,
				Urgent:      true,
				Broadcasted: true,
				Channel:     "web",
				ThreadID:    "thread-xyz",
				Attachments: []string{"README.md"},
			},
			golden: "You are receiving a message from the orchestration system:\n\n" +
				"---BEGIN SCION MESSAGE---\n" +
				"{\n" +
				"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
				"  \"sender\": \"user:admin\",\n" +
				"  \"msg\": \"full featured message\",\n" +
				"  \"type\": \"instruction\",\n" +
				"  \"urgent\": true,\n" +
				"  \"broadcasted\": true,\n" +
				"  \"attachments\": [\n" +
				"    \"README.md\"\n" +
				"  ],\n" +
				"  \"channel\": \"web\",\n" +
				"  \"thread_id\": \"thread-xyz\"\n" +
				"}\n" +
				"---END SCION MESSAGE---",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The system_message case uses NewSystemMessage which sets a
			// dynamic timestamp. Override it for deterministic comparison.
			if tt.name == "system_message" {
				tt.msg.Timestamp = "2026-08-01T10:00:00Z"
				// Recompute golden with the fixed timestamp.
				tt.golden = "You are receiving a message from the orchestration system:\n\n" +
					"---BEGIN SCION MESSAGE---\n" +
					"{\n" +
					"  \"timestamp\": \"2026-08-01T10:00:00Z\",\n" +
					"  \"sender\": \"system\",\n" +
					"  \"msg\": \"Port 8080 has been auto-exposed\",\n" +
					"  \"type\": \"system\",\n" +
					"  \"metadata\": {\n" +
					"    \"system_category\": \"port-forward\"\n" +
					"  }\n" +
					"}\n" +
					"---END SCION MESSAGE---"
			}

			got := FormatForDelivery(tt.msg)
			if got != tt.golden {
				t.Errorf("byte-identity mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", tt.name, tt.golden, got)
			}
		})
	}
}
