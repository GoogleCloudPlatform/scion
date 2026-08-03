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
	"strings"
	"testing"
)

func TestFormatForDelivery_Plain(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "just raw text",
		Type:      TypeInstruction,
		Plain:     true,
	}

	result := FormatForDelivery(msg)
	if result != "just raw text" {
		t.Errorf("plain mode should return raw msg, got %q", result)
	}
}

func TestFormatForDelivery_Structured(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "implement auth",
		Type:      TypeInstruction,
		Urgent:    true,
	}

	result := FormatForDelivery(msg)

	// Should have the intro
	if !strings.Contains(result, deliveryIntro) {
		t.Error("missing delivery intro")
	}

	// Should have delimiters
	if !strings.Contains(result, beginDelimiter) {
		t.Error("missing begin delimiter")
	}
	if !strings.Contains(result, endDelimiter) {
		t.Error("missing end delimiter")
	}

	// Should contain key fields
	if !strings.Contains(result, `"sender": "user:alice"`) {
		t.Error("missing sender in output")
	}
	if !strings.Contains(result, `"msg": "implement auth"`) {
		t.Error("missing msg in output")
	}
	if !strings.Contains(result, `"urgent": true`) {
		t.Error("missing urgent in output")
	}

	// Should NOT contain recipient or version (stripped)
	if strings.Contains(result, `"recipient"`) {
		t.Error("recipient should be stripped from delivery")
	}
	if strings.Contains(result, `"version"`) {
		t.Error("version should be stripped from delivery")
	}
}

func TestFormatForDelivery_StripsRecipient(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "agent:lead",
		Recipient: "agent:worker",
		Msg:       "check the schema",
		Type:      TypeInstruction,
	}

	result := FormatForDelivery(msg)
	if strings.Contains(result, "agent:worker") {
		t.Error("recipient identity should not appear in delivery output")
	}
}

func TestFormatForDelivery_EmptyMsg(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "",
		Type:      TypeInstruction,
		Plain:     true,
	}

	result := FormatForDelivery(msg)
	if result != "" {
		t.Errorf("empty plain message should return empty string, got %q", result)
	}
}

func TestFormatForDelivery_Raw(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "Escape",
		Type:      TypeInstruction,
		Raw:       true,
	}

	result := FormatForDelivery(msg)
	if result != "Escape" {
		t.Errorf("raw mode should return raw msg, got %q", result)
	}
}

func TestFormatForDelivery_WithRecipients(t *testing.T) {
	msg := &StructuredMessage{
		Version:    Version,
		Timestamp:  "2026-05-15T14:00:00Z",
		Sender:     "user:alice",
		Recipient:  "agent:coder",
		Recipients: "set[user:alice,agent:coder,agent:reviewer]",
		Msg:        "review this",
		Type:       TypeGroupSet,
	}

	result := FormatForDelivery(msg)
	if !strings.Contains(result, `"recipients": "set[user:alice,agent:coder,agent:reviewer]"`) {
		t.Error("missing recipients in delivery output")
	}
	if !strings.Contains(result, `"type": "group-set"`) {
		t.Error("missing group-set type in delivery output")
	}
}

func TestFormatForDelivery_OmitsEmptyRecipients(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-05-15T14:00:00Z",
		Sender:    "user:alice",
		Recipient: "agent:coder",
		Msg:       "single message",
		Type:      TypeInstruction,
	}

	result := FormatForDelivery(msg)
	if strings.Contains(result, "recipients") {
		t.Error("recipients should be omitted when empty")
	}
}

func TestFormatForDelivery_MentionMetadata(t *testing.T) {
	msg := NewMention("user:alice", "agent:observer", "check the design", "agent:primary-dev")

	result := FormatForDelivery(msg)

	// Should contain mention_source in the JSON output
	if !strings.Contains(result, `"mention_source": "agent:primary-dev"`) {
		t.Error("missing mention_source in delivery output")
	}

	// Should contain mention_position in the JSON output
	if !strings.Contains(result, `"mention_position": "body"`) {
		t.Error("missing mention_position in delivery output")
	}

	// Should still have the correct type
	if !strings.Contains(result, `"type": "mention"`) {
		t.Error("missing mention type in delivery output")
	}
}

func TestFormatForDelivery_NoMetadataRegression(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-03-07T14:30:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "do the thing",
		Type:      TypeInstruction,
	}

	result := FormatForDelivery(msg)

	// Messages without metadata should not include a metadata field
	if strings.Contains(result, `"metadata"`) {
		t.Error("metadata should be omitted when empty/nil")
	}

	// Should still format correctly
	if !strings.Contains(result, `"sender": "user:alice"`) {
		t.Error("missing sender in output")
	}
}

func TestFormatForDelivery_MetadataFiltersNonAllowlisted(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-06-01T10:00:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "hello from telegram",
		Type:      TypeMention,
		Metadata: map[string]string{
			"mention_source":   "agent:primary",
			"mention_position": "body",
			"telegram_chat_id": "123456789",
			"platform_token":   "secret-token",
		},
	}

	result := FormatForDelivery(msg)

	// Allowlisted keys should be present
	if !strings.Contains(result, `"mention_source": "agent:primary"`) {
		t.Error("missing allowlisted mention_source in delivery output")
	}
	if !strings.Contains(result, `"mention_position": "body"`) {
		t.Error("missing allowlisted mention_position in delivery output")
	}

	// Non-allowlisted keys must be excluded
	if strings.Contains(result, "telegram_chat_id") {
		t.Error("non-allowlisted telegram_chat_id should be filtered from delivery output")
	}
	if strings.Contains(result, "platform_token") {
		t.Error("non-allowlisted platform_token should be filtered from delivery output")
	}
}

func TestFormatForDelivery_AllMetadataFiltered(t *testing.T) {
	msg := &StructuredMessage{
		Version:   Version,
		Timestamp: "2026-06-01T10:00:00Z",
		Sender:    "user:alice",
		Recipient: "agent:dev",
		Msg:       "platform-only metadata",
		Type:      TypeInstruction,
		Metadata: map[string]string{
			"telegram_chat_id": "123456789",
			"internal_flag":    "true",
		},
	}

	result := FormatForDelivery(msg)

	// When all metadata keys are non-allowlisted, metadata should be omitted entirely
	if strings.Contains(result, `"metadata"`) {
		t.Error("metadata should be omitted when all keys are filtered out")
	}
}

func TestFormatForDelivery_SystemMessage(t *testing.T) {
	msg := NewSystemMessage("system", "agent:dev", "Port 8080 has been auto-exposed", SystemCategoryPortForward)

	result := FormatForDelivery(msg)

	// Should have the intro and delimiters
	if !strings.Contains(result, deliveryIntro) {
		t.Error("missing delivery intro")
	}
	if !strings.Contains(result, beginDelimiter) {
		t.Error("missing begin delimiter")
	}

	// Should contain system type
	if !strings.Contains(result, `"type": "system"`) {
		t.Error("missing system type in output")
	}

	// Should contain system_category in metadata (it's in the allowlist)
	if !strings.Contains(result, `"system_category": "port-forward"`) {
		t.Error("missing system_category in delivery output")
	}

	// Should contain the message body
	if !strings.Contains(result, "Port 8080 has been auto-exposed") {
		t.Error("missing message body in output")
	}
}

func TestFormatForDelivery_WithAttachments(t *testing.T) {
	msg := &StructuredMessage{
		Version:     Version,
		Timestamp:   "2026-03-07T14:30:00Z",
		Sender:      "user:alice",
		Recipient:   "agent:dev",
		Msg:         "review these",
		Type:        TypeInstruction,
		Attachments: []string{"src/auth.go", "src/middleware.go"},
	}

	result := FormatForDelivery(msg)
	if !strings.Contains(result, "src/auth.go") {
		t.Error("missing attachment in output")
	}
}
