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

func TestFormatLegacyAsNewDelivery_NilConvInfo_SynthesizesStub(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		SenderID:  "user:alice",
		Recipient: "agent:builder",
		Msg:       "Hello",
		Type:      messages.TypeInstruction,
		Channel:   "general",
		ThreadID:  "thread-42",
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	env := extractEnvelope(t, result)

	// Synthesized conversation ID should include channel and thread.
	if env.Conversation.ID != "general/thread-42" {
		t.Errorf("conversation.id = %q, want %q", env.Conversation.ID, "general/thread-42")
	}
	if env.Conversation.Kind != "direct" {
		t.Errorf("conversation.kind = %q, want %q", env.Conversation.Kind, "direct")
	}
	if env.Conversation.Surface != "native" {
		t.Errorf("conversation.surface = %q, want %q", env.Conversation.Surface, "native")
	}
}

func TestFormatLegacyAsNewDelivery_NilConvInfo_ChannelOnly(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: "2026-08-27T10:00:00Z",
		Sender:    "user:alice",
		SenderID:  "user:alice",
		Recipient: "agent:builder",
		Msg:       "Hello",
		Type:      messages.TypeInstruction,
		Channel:   "general",
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	env := extractEnvelope(t, result)

	if env.Conversation.ID != "general" {
		t.Errorf("conversation.id = %q, want %q", env.Conversation.ID, "general")
	}
}

func TestFormatLegacyAsNewDelivery_NilConvInfo_Broadcasted(t *testing.T) {
	old := &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   "2026-08-27T10:00:00Z",
		Sender:      "user:alice",
		SenderID:    "user:alice",
		Recipient:   "agent:builder",
		Msg:         "Hello everyone",
		Type:        messages.TypeInstruction,
		Broadcasted: true,
	}

	result := FormatLegacyAsNewDelivery(old, nil)

	env := extractEnvelope(t, result)

	if env.Conversation.Kind != "group" {
		t.Errorf("conversation.kind = %q, want %q for broadcasted message", env.Conversation.Kind, "group")
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
// contains conversation.id, kind, and intent/event.
func TestFormatLegacyAsNewDelivery_RoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		old        *messages.StructuredMessage
		wantKind   MessageKind
		wantIntent *TextIntent
		wantEvent  bool
	}{
		{
			name: "instruction -> text/request",
			old: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "user:alice",
				Recipient: "agent:builder",
				Msg:       "Do the thing",
				Type:      messages.TypeInstruction,
				Channel:   "dev",
			},
			wantKind:   KindText,
			wantIntent: intentPtr(IntentRequest),
		},
		{
			name: "state-change -> event",
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
			wantKind:  KindEvent,
			wantEvent: true,
		},
		{
			name: "chat -> text/inform",
			old: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: "2026-08-27T10:00:00Z",
				Sender:    "user:bob",
				Recipient: "user:alice",
				Msg:       "Hey there",
				Type:      messages.TypeChat,
				Channel:   "general",
			},
			wantKind:   KindText,
			wantIntent: intentPtr(IntentInform),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatLegacyAsNewDelivery(tt.old, nil)

			jsonStr := extractJSON(t, result)

			// Must be valid JSON.
			var raw map[string]any
			if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, jsonStr)
			}

			// Must have conversation.id.
			convRaw, ok := raw["conversation"].(map[string]any)
			if !ok {
				t.Fatal("missing or invalid conversation object")
			}
			if _, ok := convRaw["id"]; !ok {
				t.Error("missing conversation.id")
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
