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
	"time"
)

func TestFormatNewDelivery_TextRequest(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-001",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Please deploy the service",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{
		{
			MessageID:     "msg-001",
			PrincipalKind: "agent",
			PrincipalID:   "deployer",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
	}
	conv := &ConversationInfo{
		ID:      "conv-123",
		Kind:    "direct",
		Surface: "native",
	}

	result := FormatNewDelivery(msg, addrs, conv, DeliveryOptions{})

	// Parse the JSON out of the delimiters.
	env := extractEnvelope(t, result)

	if env.Conversation == nil {
		t.Fatal("conversation is nil, want non-nil")
	}
	if env.Conversation.ID != "conv-123" {
		t.Errorf("conversation.id = %q, want %q", env.Conversation.ID, "conv-123")
	}
	if env.Conversation.Kind != "direct" {
		t.Errorf("conversation.kind = %q, want %q", env.Conversation.Kind, "direct")
	}
	if env.From != "user:alice" {
		t.Errorf("from = %q, want %q", env.From, "user:alice")
	}
	if len(env.To) != 1 || env.To[0] != "agent:deployer" {
		t.Errorf("to = %v, want [agent:deployer]", env.To)
	}
	if env.Kind != KindText {
		t.Errorf("kind = %q, want %q", env.Kind, KindText)
	}
	if env.Intent == nil || *env.Intent != IntentRequest {
		t.Errorf("intent = %v, want request", env.Intent)
	}
	if env.Msg != "Please deploy the service" {
		t.Errorf("msg = %q, want %q", env.Msg, "Please deploy the service")
	}
}

func TestFormatNewDelivery_TextInform_NoTo(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:        "msg-002",
		From:      PrincipalRef("agent:builder"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Build completed successfully",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{
		ID:      "conv-456",
		Kind:    "group",
		Surface: "native",
	}

	// No addressees for informational messages.
	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	env := extractEnvelope(t, result)

	if len(env.To) != 0 {
		t.Errorf("to = %v, want empty (informational message)", env.To)
	}
	if env.Kind != KindText {
		t.Errorf("kind = %q, want %q", env.Kind, KindText)
	}
	if env.Intent == nil || *env.Intent != IntentInform {
		t.Errorf("intent = %v, want inform", env.Intent)
	}
}

// TestFormatNewDelivery_EventWithStatus is the critical test for AC-10:
// status on lifecycle events must be a structured field in the delivery JSON.
// If the Status field were removed from EventBody, this test MUST fail.
func TestFormatNewDelivery_EventWithStatus(t *testing.T) {
	msg := &Message{
		ID:   "msg-003",
		From: PrincipalRef("system:lifecycle"),
		Kind: KindEvent,
		Event: &EventBody{
			Type:    EventAgentStateChanged,
			Subject: "agent:builder",
			Status:  "COMPLETED",
		},
		Body:      "Agent builder has completed",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{
		ID:      "conv-789",
		Kind:    "direct",
		Surface: "native",
	}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	env := extractEnvelope(t, result)

	if env.Kind != KindEvent {
		t.Fatalf("kind = %q, want %q", env.Kind, KindEvent)
	}
	if env.Event == nil {
		t.Fatal("event is nil, want non-nil EventBody")
	}
	if env.Event.Type != EventAgentStateChanged {
		t.Errorf("event.type = %q, want %q", env.Event.Type, EventAgentStateChanged)
	}
	if env.Event.Status != "COMPLETED" {
		t.Errorf("event.status = %q, want %q", env.Event.Status, "COMPLETED")
	}
	if env.Event.Subject != "agent:builder" {
		t.Errorf("event.subject = %q, want %q", env.Event.Subject, "agent:builder")
	}

	// Also verify via raw JSON that the "status" key exists inside "event".
	jsonStr := extractJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		t.Fatalf("failed to unmarshal raw JSON: %v", err)
	}
	eventMap, ok := raw["event"].(map[string]any)
	if !ok {
		t.Fatal("event field is not a JSON object")
	}
	if status, ok := eventMap["status"]; !ok || status != "COMPLETED" {
		t.Errorf("raw event.status = %v, want %q", status, "COMPLETED")
	}
}

func TestFormatNewDelivery_VisibilityDelivered(t *testing.T) {
	intent := IntentInform
	msg := &Message{
		ID:         "msg-004",
		From:       PrincipalRef("agent:builder"),
		Kind:       KindText,
		Intent:     &intent,
		Body:       "Verbose output",
		Visibility: VisibilityVerbose,
		CreatedAt:  time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{
		ID:      "conv-100",
		Kind:    "direct",
		Surface: "native",
	}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	env := extractEnvelope(t, result)

	if env.Visibility != VisibilityVerbose {
		t.Errorf("visibility = %q, want %q", env.Visibility, VisibilityVerbose)
	}
}

func TestFormatNewDelivery_NoMetadata(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-005",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{
		ID:      "conv-200",
		Kind:    "direct",
		Surface: "native",
	}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	jsonStr := extractJSON(t, result)
	if strings.Contains(jsonStr, `"metadata"`) {
		t.Error("output contains 'metadata' field, want none")
	}
}

func TestFormatNewDelivery_NoBroadcasted(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-006",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Hello",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{
		ID:      "conv-200",
		Kind:    "group",
		Surface: "native",
	}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	jsonStr := extractJSON(t, result)
	if strings.Contains(jsonStr, `"broadcasted"`) {
		t.Error("output contains 'broadcasted' field, want none")
	}
}

func TestFormatNewDelivery_PlainReturnsRawText(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-007",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "raw text content",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	result := FormatNewDelivery(msg, nil, nil, DeliveryOptions{Plain: true})

	if result != "raw text content" {
		t.Errorf("plain delivery = %q, want %q", result, "raw text content")
	}
}

func TestFormatNewDelivery_RawReturnsRawText(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-008",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "keystroke content",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	result := FormatNewDelivery(msg, nil, nil, DeliveryOptions{Raw: true})

	if result != "keystroke content" {
		t.Errorf("raw delivery = %q, want %q", result, "keystroke content")
	}
}

func TestFormatNewDelivery_Delimiters(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-009",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Test",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{ID: "conv-500", Kind: "direct", Surface: "native"}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	expectedPrefix := "You are receiving a message from the orchestration system:\n\n---BEGIN SCION MESSAGE---\n"
	if !strings.HasPrefix(result, expectedPrefix) {
		t.Errorf("result does not start with expected delimiter prefix.\nGot prefix: %q", result[:min(len(result), len(expectedPrefix)+10)])
	}
	if !strings.HasSuffix(result, "\n---END SCION MESSAGE---") {
		t.Errorf("result does not end with expected delimiter suffix.\nGot suffix: %q", result[max(0, len(result)-30):])
	}
}

func TestFormatNewDelivery_Attachments(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:     "msg-010",
		From:   PrincipalRef("user:alice"),
		Kind:   KindText,
		Intent: &intent,
		Body:   "See attached",
		Attachments: []AttachmentRef{
			{Path: "/tmp/file1.txt", Name: "file1"},
			{Path: "/tmp/file2.txt"},
		},
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{ID: "conv-600", Kind: "direct", Surface: "native"}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	env := extractEnvelope(t, result)
	if len(env.Attachments) != 2 {
		t.Fatalf("attachments length = %d, want 2", len(env.Attachments))
	}
	if env.Attachments[0] != "/tmp/file1.txt" {
		t.Errorf("attachments[0] = %q, want %q", env.Attachments[0], "/tmp/file1.txt")
	}
}

func TestFormatNewDelivery_ReplyTo(t *testing.T) {
	intent := IntentRequest
	replyTo := "msg-000"
	msg := &Message{
		ID:        "msg-011",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Replying to your question",
		ReplyToID: &replyTo,
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{ID: "conv-700", Kind: "direct", Surface: "native"}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	env := extractEnvelope(t, result)
	if env.ReplyTo == nil || *env.ReplyTo != "msg-000" {
		t.Errorf("reply_to = %v, want %q", env.ReplyTo, "msg-000")
	}
}

func TestFormatNewDelivery_MultipleAddressees(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-013",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Deploy all services",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{
		{MessageID: "msg-013", PrincipalKind: "agent", PrincipalID: "deployer", Via: ViaExplicit, DeliveryState: DeliveryPending},
		{MessageID: "msg-013", PrincipalKind: "agent", PrincipalID: "tester", Via: ViaBodyMention, DeliveryState: DeliveryPending},
	}
	conv := &ConversationInfo{ID: "conv-900", Kind: "group", Surface: "native"}

	result := FormatNewDelivery(msg, addrs, conv, DeliveryOptions{})

	env := extractEnvelope(t, result)
	if len(env.To) != 2 {
		t.Fatalf("to length = %d, want 2", len(env.To))
	}
	if env.To[0] != "agent:deployer" {
		t.Errorf("to[0] = %q, want %q", env.To[0], "agent:deployer")
	}
	if env.To[1] != "agent:tester" {
		t.Errorf("to[1] = %q, want %q", env.To[1], "agent:tester")
	}
}

// TestFormatNewDelivery_Urgent (AC-9-10a) verifies that an urgent message
// produces "urgent": true in the delivered envelope. This pins the urgent
// semantics on the new envelope so drift between the new renderer and the
// legacy renderer (pkg/messages/format.go) is caught.
func TestFormatNewDelivery_Urgent(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-015",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Urgent request",
		Urgent:    true,
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{ID: "conv-1000", Kind: "direct", Surface: "native"}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	env := extractEnvelope(t, result)
	if !env.Urgent {
		t.Error("urgent = false, want true")
	}

	// Also verify via raw JSON that "urgent": true appears.
	jsonStr := extractJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	urgentVal, ok := raw["urgent"]
	if !ok {
		t.Fatal("missing 'urgent' key in JSON")
	}
	if urgentVal != true {
		t.Errorf("urgent = %v, want true", urgentVal)
	}
}

// TestFormatNewDelivery_NotUrgent_OmitsKey verifies that a non-urgent message
// does not include "urgent" in the JSON (omitempty).
func TestFormatNewDelivery_NotUrgent_OmitsKey(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-016",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Normal request",
		Urgent:    false,
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{ID: "conv-1001", Kind: "direct", Surface: "native"}

	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{})

	jsonStr := extractJSON(t, result)
	if strings.Contains(jsonStr, `"urgent"`) {
		t.Error("JSON contains 'urgent' key for non-urgent message; want omitted")
	}
}

// TestFormatNewDelivery_NilConversation_OmitsKey (DEF-102, AC-9-4) verifies
// that when no conversation context is available, the "conversation" key is
// absent from the JSON envelope (not fabricated), and the message body is
// still delivered.
func TestFormatNewDelivery_NilConversation_OmitsKey(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-014",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "Message without conversation context",
		CreatedAt: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC),
	}

	result := FormatNewDelivery(msg, nil, nil, DeliveryOptions{})

	// The message body must still be delivered.
	if !strings.Contains(result, "Message without conversation context") {
		t.Error("body not delivered when conversation is nil")
	}
	if !strings.Contains(result, beginDelimiter) {
		t.Error("missing begin delimiter — message not wrapped")
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

	// The structured envelope should still parse (with nil Conversation).
	env := extractEnvelope(t, result)
	if env.Conversation != nil {
		t.Errorf("conversation = %+v, want nil", env.Conversation)
	}
	if env.Msg != "Message without conversation context" {
		t.Errorf("msg = %q, want %q", env.Msg, "Message without conversation context")
	}
}

// ---------- Helpers ----------

// extractJSON pulls the JSON content from between the delimiters.
func extractJSON(t *testing.T, result string) string {
	t.Helper()
	start := strings.Index(result, beginDelimiter)
	if start < 0 {
		t.Fatalf("missing begin delimiter in result")
	}
	start += len(beginDelimiter) + 1 // skip newline after delimiter
	end := strings.Index(result, endDelimiter)
	if end < 0 {
		t.Fatalf("missing end delimiter in result")
	}
	return result[start : end-1] // trim trailing newline before end delimiter
}

// extractEnvelope parses the delivery envelope from the formatted result string.
func extractEnvelope(t *testing.T, result string) DeliveryEnvelope {
	t.Helper()
	jsonStr := extractJSON(t, result)
	var env DeliveryEnvelope
	if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
		t.Fatalf("failed to unmarshal delivery envelope: %v\nJSON: %s", err, jsonStr)
	}
	return env
}
