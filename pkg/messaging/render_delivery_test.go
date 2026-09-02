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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------- RenderDeliveryText ----------

func TestRenderDeliveryText_NilMsg(t *testing.T) {
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-100",
	})
	if result != "" {
		t.Errorf("expected empty string for nil Msg, got %q", result)
	}
}

func TestRenderDeliveryText_PlainBypass(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "raw body text",
		Type:      messages.TypeInstruction,
		Plain:     true,
	}
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-101",
		Msg:       msg,
		CreatedAt: time.Now().UTC(),
	})
	if result != "raw body text" {
		t.Errorf("plain delivery = %q, want %q", result, "raw body text")
	}
}

func TestRenderDeliveryText_RawBypass(t *testing.T) {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "keystroke data",
		Type:      messages.TypeInstruction,
		Raw:       true,
	}
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-102",
		Msg:       msg,
		CreatedAt: time.Now().UTC(),
	})
	if result != "keystroke data" {
		t.Errorf("raw delivery = %q, want %q", result, "keystroke data")
	}
}

func TestRenderDeliveryText_UsesRealMessageID(t *testing.T) {
	// RenderDeliveryText sets msg.ID to the real persisted ID (not the
	// fabricated "legacy-..." from MapLegacyEnvelope). The current
	// DeliveryEnvelope struct does not serialise msg.ID to JSON yet —
	// that wire field arrives in Phase 11. This test verifies:
	//   1. The fabricated "legacy-" ID pattern does NOT leak into the output.
	//   2. The output is valid (non-empty, delimitered envelope).
	//   3. reply_to is omitted (Phase 9b hard constraint).
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "Hello agent",
		Type:      messages.TypeInstruction,
	}
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "real-persisted-id-001",
		Msg:       msg,
		CreatedAt: now,
	})

	// Must NOT contain the fabricated legacy ID pattern.
	if strings.Contains(result, "legacy-") {
		t.Error("envelope contains fabricated 'legacy-' identifier, violating the hard constraint")
	}

	// Must produce a valid envelope.
	env := extractDeliveryEnvelope(t, result)
	if env.Msg != "Hello agent" {
		t.Errorf("msg = %q, want %q", env.Msg, "Hello agent")
	}

	// reply_to must be absent.
	raw := extractRawJSON(t, result)
	if strings.Contains(raw, `"reply_to"`) {
		t.Error("envelope contains reply_to, but Phase 9b must always omit it")
	}
}

func TestRenderDeliveryText_ReplyToAlwaysOmitted(t *testing.T) {
	// Even if the StructuredMessage has a ThreadID, the render helper
	// must omit reply_to in Phase 9b (hard constraint: never fabricate).
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "Replying in thread",
		Type:      messages.TypeInstruction,
		ThreadID:  "thread-xyz",
	}
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-103",
		Msg:       msg,
		CreatedAt: now,
	})

	raw := extractRawJSON(t, result)
	if strings.Contains(raw, `"reply_to"`) {
		t.Error("envelope contains reply_to, but Phase 9b must always omit it")
	}
}

func TestRenderDeliveryText_WithConvResult(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "Direct message",
		Type:      messages.TypeInstruction,
	}
	conv := &ConversationResult{
		ConversationID: "conv-real-123",
		Kind:           "direct",
		Surface:        "native",
		DisplayName:    "alice ↔ bot",
	}
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID:  "msg-104",
		ConvResult: conv,
		Msg:        msg,
		CreatedAt:  now,
	})

	env := extractDeliveryEnvelope(t, result)
	if env.Conversation.ID != "conv-real-123" {
		t.Errorf("conversation.id = %q, want %q", env.Conversation.ID, "conv-real-123")
	}
	if env.Conversation.Kind != "direct" {
		t.Errorf("conversation.kind = %q, want %q", env.Conversation.Kind, "direct")
	}
	if env.Conversation.Surface != "native" {
		t.Errorf("conversation.surface = %q, want %q", env.Conversation.Surface, "native")
	}
	if env.Conversation.Name != "alice ↔ bot" {
		t.Errorf("conversation.name = %q, want %q", env.Conversation.Name, "alice ↔ bot")
	}
}

func TestRenderDeliveryText_NilConvResult_OmitsConversation(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "Broadcast message",
		Type:      messages.TypeInstruction,
	}
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID:  "msg-105",
		ConvResult: nil,
		Msg:        msg,
		CreatedAt:  now,
	})

	env := extractDeliveryEnvelope(t, result)
	// When ConvResult is nil, conversation should have zero-value fields (honest absence).
	if env.Conversation.ID != "" {
		t.Errorf("conversation.id = %q, want empty (nil ConvResult)", env.Conversation.ID)
	}
}

func TestRenderDeliveryText_UsesPersistedTimestamp(t *testing.T) {
	// The envelope timestamp should come from the persisted row's
	// CreatedAt, not the StructuredMessage's Timestamp.
	msgTime := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	persistTime := time.Date(2026, 9, 1, 14, 30, 0, 0, time.UTC)

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: msgTime.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "Timestamped message",
		Type:      messages.TypeInstruction,
	}
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-106",
		Msg:       msg,
		CreatedAt: persistTime,
	})

	env := extractDeliveryEnvelope(t, result)
	want := "2026-09-01T14:30:00Z"
	if env.Timestamp != want {
		t.Errorf("timestamp = %q, want %q (from persisted CreatedAt)", env.Timestamp, want)
	}
}

// ---------- RenderDeliveryTextWithLookup ----------

// mockConversationGetter implements ConversationGetter for testing.
type mockConversationGetter struct {
	conversations map[string]*store.Conversation
	lookupErr     error
}

func (m *mockConversationGetter) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	conv, ok := m.conversations[id]
	if !ok {
		return nil, fmt.Errorf("conversation %q not found", id)
	}
	return conv, nil
}

func TestRenderDeliveryTextWithLookup_UsesExistingConvResult(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      now.Format(time.RFC3339),
		Sender:         "user:alice",
		Recipient:      "agent:bot",
		Msg:            "Test",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-should-not-lookup",
	}
	conv := &ConversationResult{
		ConversationID: "conv-from-result",
		Kind:           "group",
		Surface:        "slack",
	}
	cg := &mockConversationGetter{
		conversations: map[string]*store.Conversation{
			"conv-should-not-lookup": {ID: "conv-should-not-lookup", Kind: "direct", Surface: "native"},
		},
	}

	result := RenderDeliveryTextWithLookup(context.Background(), cg, slog.Default(), RenderDeliveryInput{
		MessageID:  "msg-200",
		ConvResult: conv,
		Msg:        msg,
		CreatedAt:  now,
	})

	env := extractDeliveryEnvelope(t, result)
	// Should use the provided ConvResult, not the looked-up one.
	if env.Conversation.ID != "conv-from-result" {
		t.Errorf("conversation.id = %q, want %q (should use existing ConvResult)", env.Conversation.ID, "conv-from-result")
	}
	if env.Conversation.Kind != "group" {
		t.Errorf("conversation.kind = %q, want %q", env.Conversation.Kind, "group")
	}
}

func TestRenderDeliveryTextWithLookup_FallbackLookup(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      now.Format(time.RFC3339),
		Sender:         "user:alice",
		Recipient:      "agent:bot",
		Msg:            "Test",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-lookup-target",
	}
	cg := &mockConversationGetter{
		conversations: map[string]*store.Conversation{
			"conv-lookup-target": {
				ID:          "conv-lookup-target",
				Kind:        "direct",
				Surface:     "discord",
				DisplayName: "looked-up-name",
			},
		},
	}

	result := RenderDeliveryTextWithLookup(context.Background(), cg, slog.Default(), RenderDeliveryInput{
		MessageID:  "msg-201",
		ConvResult: nil, // no ConvResult — should trigger lookup
		Msg:        msg,
		CreatedAt:  now,
	})

	env := extractDeliveryEnvelope(t, result)
	if env.Conversation.ID != "conv-lookup-target" {
		t.Errorf("conversation.id = %q, want %q", env.Conversation.ID, "conv-lookup-target")
	}
	if env.Conversation.Surface != "discord" {
		t.Errorf("conversation.surface = %q, want %q", env.Conversation.Surface, "discord")
	}
}

func TestRenderDeliveryTextWithLookup_LookupFails_OmitsConversation(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:        messages.Version,
		Timestamp:      now.Format(time.RFC3339),
		Sender:         "user:alice",
		Recipient:      "agent:bot",
		Msg:            "Test",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-missing",
	}
	cg := &mockConversationGetter{
		lookupErr: fmt.Errorf("database error"),
	}

	result := RenderDeliveryTextWithLookup(context.Background(), cg, slog.Default(), RenderDeliveryInput{
		MessageID:  "msg-202",
		ConvResult: nil,
		Msg:        msg,
		CreatedAt:  now,
	})

	// Should still produce a valid envelope (honest absence).
	env := extractDeliveryEnvelope(t, result)
	if env.Conversation.ID != "" {
		t.Errorf("conversation.id = %q, want empty (lookup failure)", env.Conversation.ID)
	}
	// But the message should still be rendered.
	if env.Msg != "Test" {
		t.Errorf("msg = %q, want %q", env.Msg, "Test")
	}
}

func TestRenderDeliveryTextWithLookup_EmptyConversationID_NoLookup(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:bot",
		Msg:       "No conversation",
		Type:      messages.TypeInstruction,
		// ConversationID empty — no lookup should be attempted.
	}
	cg := &mockConversationGetter{
		lookupErr: fmt.Errorf("should not be called"),
	}

	result := RenderDeliveryTextWithLookup(context.Background(), cg, slog.Default(), RenderDeliveryInput{
		MessageID:  "msg-203",
		ConvResult: nil,
		Msg:        msg,
		CreatedAt:  now,
	})

	env := extractDeliveryEnvelope(t, result)
	if env.Conversation.ID != "" {
		t.Errorf("conversation.id = %q, want empty", env.Conversation.ID)
	}
	if env.Msg != "No conversation" {
		t.Errorf("msg = %q, want %q", env.Msg, "No conversation")
	}
}

func TestRenderDeliveryTextWithLookup_NilMsg(t *testing.T) {
	cg := &mockConversationGetter{}
	result := RenderDeliveryTextWithLookup(context.Background(), cg, slog.Default(), RenderDeliveryInput{
		MessageID: "msg-204",
		Msg:       nil,
	})
	if result != "" {
		t.Errorf("expected empty for nil Msg, got %q", result)
	}
}

// ---------- Helpers (render_delivery_test only) ----------

// extractDeliveryEnvelope parses a DeliveryEnvelope from the render output.
// Reuses the same begin/end delimiter logic as delivery_test.go.
func extractDeliveryEnvelope(t *testing.T, result string) DeliveryEnvelope {
	t.Helper()
	raw := extractRawJSON(t, result)
	var env DeliveryEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("failed to unmarshal delivery envelope: %v\nJSON: %s", err, raw)
	}
	return env
}

// extractRawJSON pulls the JSON content from between the begin/end delimiters.
func extractRawJSON(t *testing.T, result string) string {
	t.Helper()
	start := strings.Index(result, beginDelimiter)
	if start < 0 {
		t.Fatalf("missing begin delimiter in result:\n%s", result)
	}
	start += len(beginDelimiter) + 1 // skip newline after delimiter
	end := strings.Index(result, endDelimiter)
	if end < 0 {
		t.Fatalf("missing end delimiter in result:\n%s", result)
	}
	return result[start : end-1] // trim trailing newline before end delimiter
}
