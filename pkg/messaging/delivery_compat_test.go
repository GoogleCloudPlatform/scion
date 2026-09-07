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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestFormatLegacyAsNewDelivery_WithConvInfo(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		SenderID:  "user:alice",
		Recipient: "agent:builder",
		Msg:       "Build the project",
		Type:      messages.TypeInstruction,
	}
	conv := &ConversationInfo{
		ID:      "conv-legacy-1",
		Kind:    "direct",
		Surface: "native",
	}

	result := FormatLegacyAsNewDelivery(old, conv)

	// Should have delimiters.
	if !strings.Contains(result, beginDelimiter) {
		t.Error("result missing begin delimiter")
	}
	if !strings.Contains(result, endDelimiter) {
		t.Error("result missing end delimiter")
	}

	env := extractEnvelope(t, result)

	if env.Conversation == nil {
		t.Fatal("conversation is nil, want non-nil")
	}
	if env.Conversation.ID != "conv-legacy-1" {
		t.Errorf("conversation.id = %q, want %q", env.Conversation.ID, "conv-legacy-1")
	}
	if env.Kind != KindText {
		t.Errorf("kind = %q, want %q", env.Kind, KindText)
	}
	if env.Intent == nil || *env.Intent != IntentRequest {
		t.Errorf("intent = %v, want request", env.Intent)
	}
	if env.Msg != "Build the project" {
		t.Errorf("msg = %q, want %q", env.Msg, "Build the project")
	}
}

// TestFormatLegacyAsNewDelivery_NilConvInfo_OmitsConversation (DEF-102)
// replaces the three synthesize tests. When no conversation context is
// available, the "conversation" key must be absent from the JSON envelope
// and the message body must still be delivered.
func TestFormatLegacyAsNewDelivery_NilConvInfo_OmitsConversation(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		SenderID:  "user:alice",
		Recipient: "agent:builder",
		Msg:       "Hello without conversation",
		Type:      messages.TypeInstruction,
		Channel:   "general",
		ThreadID:  "thread-42",
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	// The message body must still be delivered.
	if !strings.Contains(result, "Hello without conversation") {
		t.Error("body not delivered when convInfo is nil")
	}
	if !strings.Contains(result, beginDelimiter) {
		t.Error("missing begin delimiter")
	}

	// The "conversation" key must be absent from the JSON.
	jsonStr := extractJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v\n%s", err, jsonStr)
	}
	if _, ok := raw["conversation"]; ok {
		t.Error("JSON contains 'conversation' key; want absent when convInfo is nil (DEF-102)")
	}

	// The structured envelope should still parse with nil Conversation.
	env := extractEnvelope(t, result)
	if env.Conversation != nil {
		t.Errorf("conversation = %+v, want nil", env.Conversation)
	}
	if env.Msg != "Hello without conversation" {
		t.Errorf("msg = %q, want %q", env.Msg, "Hello without conversation")
	}
}

func TestFormatLegacyAsNewDelivery_PlainMessage(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		Recipient: "agent:builder",
		Msg:       "plain text only",
		Type:      messages.TypeInstruction,
		Plain:     true,
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	if result != "plain text only" {
		t.Errorf("plain delivery = %q, want %q", result, "plain text only")
	}
}

func TestFormatLegacyAsNewDelivery_RawMessage(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		Recipient: "agent:builder",
		Msg:       "raw keystroke",
		Type:      messages.TypeInstruction,
		Raw:       true,
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	if result != "raw keystroke" {
		t.Errorf("raw delivery = %q, want %q", result, "raw keystroke")
	}
}

func TestFormatLegacyAsNewDelivery_NilMessage(t *testing.T) {
	result := FormatLegacyAsNewDelivery(nil, nil)
	if result != "" {
		t.Errorf("nil message delivery = %q, want empty string", result)
	}
}

// TestFormatLegacyAsNewDelivery_EventStatusDelivered is the critical test:
// a legacy state-change message with a status field must have that status
// appear as a structured field in the event body of the new delivery format.
func TestFormatLegacyAsNewDelivery_EventStatusDelivered(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "agent:builder",
		SenderID:  "agent:builder",
		Recipient: "agent:coordinator",
		Msg:       "Agent builder has completed",
		Type:      messages.TypeStateChange,
		Status:    "COMPLETED",
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	env := extractEnvelope(t, result)

	if env.Kind != KindEvent {
		t.Fatalf("kind = %q, want %q", env.Kind, KindEvent)
	}
	if env.Event == nil {
		t.Fatal("event is nil, want non-nil EventBody")
	}
	if env.Event.Status != "COMPLETED" {
		t.Errorf("event.status = %q, want %q", env.Event.Status, "COMPLETED")
	}
}

// TestFormatLegacyAsNewDelivery_VisibilityDelivered verifies that visibility,
// which was previously dropped by the old format, is now delivered.
func TestFormatLegacyAsNewDelivery_VisibilityDelivered(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:    messages.Version,
		Timestamp:  "2026-08-27T10:00:00Z",
		Sender:     "agent:builder",
		SenderID:   "agent:builder",
		Recipient:  "agent:coordinator",
		Msg:        "Verbose output here",
		Type:       messages.TypeAssistantReply,
		Visibility: messages.VisibilityVerbose,
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	env := extractEnvelope(t, result)

	if env.Visibility != VisibilityVerbose {
		t.Errorf("visibility = %q, want %q", env.Visibility, VisibilityVerbose)
	}
}

// TestFormatLegacyAsNewDelivery_NoMetadataInOutput verifies that the
// metadata allowlist is not used in the new format.
func TestFormatLegacyAsNewDelivery_NoMetadataInOutput(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		SenderID:  "user:alice",
		Recipient: "agent:builder",
		Msg:       "Hello",
		Type:      messages.TypeMention,
		Metadata: map[string]string{
			"mention_source":   "agent:coordinator",
			"mention_position": "body",
			"channel":          "general",
		},
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	jsonStr := extractJSON(t, result)
	if strings.Contains(jsonStr, `"metadata"`) {
		t.Error("output contains 'metadata' field, want none in new format")
	}
}

// TestFormatLegacyAsNewDelivery_RoundTrip verifies that a StructuredMessage
// round-trips through FormatLegacyAsNewDelivery producing parseable JSON that
// contains kind and intent/event. When convInfo is nil, conversation is absent.
func TestFormatLegacyAsNewDelivery_RoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		old        *messages.StructuredMessage
		conv       *ConversationInfo
		wantKind   MessageKind
		wantIntent *TextIntent
		wantEvent  bool
		wantConv   bool
	}{
		{
			name: "instruction with conv -> text/request",
			old: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:builder",
				Msg:       "Do the thing",
				Type:      messages.TypeInstruction,
				Channel:   "dev",
			},
			conv:       &ConversationInfo{ID: "conv-rt-1", Kind: "direct", Surface: "native"},
			wantKind:   KindText,
			wantIntent: intentPtr(IntentRequest),
			wantConv:   true,
		},
		{
			name: "state-change without conv -> event, no conversation key",
			old: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "agent:builder",
				Recipient: "agent:coordinator",
				Msg:       "State changed",
				Type:      messages.TypeStateChange,
				Status:    "RUNNING",
				Channel:   "dev",
			},
			conv:      nil,
			wantKind:  KindEvent,
			wantEvent: true,
			wantConv:  false,
		},
		{
			name: "chat with conv -> text/inform",
			old: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "user:bob",
				Recipient: "user:alice",
				Msg:       "Hey there",
				Type:      messages.TypeChat,
				Channel:   "general",
			},
			conv:       &ConversationInfo{ID: "conv-rt-3", Kind: "group", Surface: "native"},
			wantKind:   KindText,
			wantIntent: intentPtr(IntentInform),
			wantConv:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatLegacyAsNewDelivery(tt.old, tt.conv)

			jsonStr := extractJSON(t, result)

			// Must be valid JSON.
			var raw map[string]any
			if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, jsonStr)
			}

			// Check conversation presence/absence.
			_, hasConv := raw["conversation"]
			if tt.wantConv && !hasConv {
				t.Error("missing conversation object, want present")
			}
			if !tt.wantConv && hasConv {
				t.Error("has conversation object, want absent")
			}

			// Must have kind.
			kindRaw, ok := raw["kind"].(string)
			if !ok {
				t.Fatal("missing or invalid kind")
			}
			if MessageKind(kindRaw) != tt.wantKind {
				t.Errorf("kind = %q, want %q", kindRaw, tt.wantKind)
			}

			// Check intent or event.
			if tt.wantIntent != nil {
				intentRaw, ok := raw["intent"].(string)
				if !ok {
					t.Fatal("missing or invalid intent")
				}
				if TextIntent(intentRaw) != *tt.wantIntent {
					t.Errorf("intent = %q, want %q", intentRaw, *tt.wantIntent)
				}
			}
			if tt.wantEvent {
				if _, ok := raw["event"]; !ok {
					t.Error("missing event object")
				}
			}
		})
	}
}

// intentPtr is a helper to create a pointer to a TextIntent.
func intentPtr(i TextIntent) *TextIntent {
	return &i
}
