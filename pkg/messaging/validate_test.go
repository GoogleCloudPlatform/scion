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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------- helpers ----------

// validTextMessage returns a Message that passes all validation checks.
// Tests mutate a copy to break exactly one rule at a time.
func validTextMessage() *Message {
	intent := IntentInform
	return &Message{
		ID:             "msg-1",
		ConversationID: "conv-1",
		From:           "user:alice",
		Kind:           KindText,
		Intent:         &intent,
		Body:           "hello",
		CreatedAt:      time.Now().UTC(),
	}
}

// validEventMessage returns a valid event message.
func validEventMessage() *Message {
	return &Message{
		ID:             "msg-2",
		ConversationID: "conv-1",
		From:           "agent:builder",
		Kind:           KindEvent,
		Event:          &EventBody{Type: EventAgentStateChanged, Status: "COMPLETED"},
		CreatedAt:      time.Now().UTC(),
	}
}

// validAddressees returns a valid addressee list.
func validAddressees(msgID string) []Addressee {
	return []Addressee{
		{
			MessageID:     msgID,
			PrincipalKind: "agent",
			PrincipalID:   "builder",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
	}
}

// ---------- ValidateMessage tests ----------

func TestValidateMessage_NilMessage(t *testing.T) {
	if err := ValidateMessage(nil); err == nil {
		t.Fatal("expected error for nil message")
	}
}

func TestValidateMessage_ValidTextMessage(t *testing.T) {
	msg := validTextMessage()
	if err := ValidateMessage(msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMessage_ValidEventMessage(t *testing.T) {
	msg := validEventMessage()
	if err := ValidateMessage(msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMessage_MissingID(t *testing.T) {
	msg := validTextMessage()
	msg.ID = ""
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Fatalf("error should mention id, got: %v", err)
	}
}

func TestValidateMessage_MissingConversationID(t *testing.T) {
	msg := validTextMessage()
	msg.ConversationID = ""
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing conversation_id")
	}
	if !strings.Contains(err.Error(), "conversation_id") {
		t.Fatalf("error should mention conversation_id, got: %v", err)
	}
}

func TestValidateMessage_MissingFrom(t *testing.T) {
	msg := validTextMessage()
	msg.From = ""
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for missing from")
	}
	if !strings.Contains(err.Error(), "from") {
		t.Fatalf("error should mention from, got: %v", err)
	}
}

func TestValidateMessage_InvalidFrom(t *testing.T) {
	msg := validTextMessage()
	msg.From = "badref"
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for malformed from")
	}
}

func TestValidateMessage_InvalidKind(t *testing.T) {
	msg := validTextMessage()
	msg.Kind = "bogus"
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for invalid kind")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Fatalf("error should mention kind, got: %v", err)
	}
}

func TestValidateMessage_TextWithoutIntent(t *testing.T) {
	msg := validTextMessage()
	msg.Intent = nil
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for text message without intent")
	}
	if !strings.Contains(err.Error(), "intent") {
		t.Fatalf("error should mention intent, got: %v", err)
	}
}

func TestValidateMessage_TextWithInvalidIntent(t *testing.T) {
	msg := validTextMessage()
	bad := TextIntent("bogus")
	msg.Intent = &bad
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for invalid intent")
	}
}

func TestValidateMessage_TextWithEventBody(t *testing.T) {
	msg := validTextMessage()
	msg.Event = &EventBody{Type: EventAgentStateChanged}
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for text message with event body")
	}
	if !strings.Contains(err.Error(), "event body") {
		t.Fatalf("error should mention event body, got: %v", err)
	}
}

func TestValidateMessage_EventWithoutEventBody(t *testing.T) {
	msg := validEventMessage()
	msg.Event = nil
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for event message without event body")
	}
}

func TestValidateMessage_EventWithInvalidType(t *testing.T) {
	msg := validEventMessage()
	msg.Event.Type = "bogus.type"
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for event message with invalid event type")
	}
}

func TestValidateMessage_EventWithIntent(t *testing.T) {
	msg := validEventMessage()
	intent := IntentInform
	msg.Intent = &intent
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for event message with intent set")
	}
	if !strings.Contains(err.Error(), "intent") {
		t.Fatalf("error should mention intent, got: %v", err)
	}
}

func TestValidateMessage_BodyOverCharLimit(t *testing.T) {
	msg := validTextMessage()
	msg.Body = strings.Repeat("x", messages.MaxMessageLength+1)
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for body over character limit")
	}
	if !strings.Contains(err.Error(), "character limit") {
		t.Fatalf("error should mention character limit, got: %v", err)
	}
}

func TestValidateMessage_BodyOverByteLimit(t *testing.T) {
	msg := validTextMessage()
	// Use multi-byte characters so char count is under MaxMessageLength
	// but byte count exceeds MaxMsgSize.
	msg.Body = strings.Repeat("🎉", messages.MaxMsgSize/4+1)
	if len([]rune(msg.Body)) <= messages.MaxMessageLength && len(msg.Body) > messages.MaxMsgSize {
		err := ValidateMessage(msg)
		if err == nil {
			t.Fatal("expected error for body over byte limit")
		}
	}
}

func TestValidateMessage_TooManyAttachments(t *testing.T) {
	msg := validTextMessage()
	for i := 0; i < messages.MaxAttachments+1; i++ {
		msg.Attachments = append(msg.Attachments, AttachmentRef{Path: fmt.Sprintf("/tmp/file%d", i)})
	}
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for too many attachments")
	}
	if !strings.Contains(err.Error(), "attachments") {
		t.Fatalf("error should mention attachments, got: %v", err)
	}
}

func TestValidateMessage_InvalidVisibility(t *testing.T) {
	msg := validTextMessage()
	msg.Visibility = "secret"
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for invalid visibility")
	}
}

func TestValidateMessage_EmptyVisibilityAllowed(t *testing.T) {
	msg := validTextMessage()
	msg.Visibility = ""
	if err := ValidateMessage(msg); err != nil {
		t.Fatalf("empty visibility should be allowed: %v", err)
	}
}

func TestValidateMessage_ReplyToIDEmptyString(t *testing.T) {
	msg := validTextMessage()
	empty := ""
	msg.ReplyToID = &empty
	err := ValidateMessage(msg)
	if err == nil {
		t.Fatal("expected error for empty reply_to_id")
	}
	if !strings.Contains(err.Error(), "reply_to_id") {
		t.Fatalf("error should mention reply_to_id, got: %v", err)
	}
}

func TestValidateMessage_ReplyToIDValidString(t *testing.T) {
	msg := validTextMessage()
	id := "parent-msg-1"
	msg.ReplyToID = &id
	if err := ValidateMessage(msg); err != nil {
		t.Fatalf("valid reply_to_id should pass: %v", err)
	}
}

func TestValidateMessage_ReplyToIDNil(t *testing.T) {
	msg := validTextMessage()
	msg.ReplyToID = nil
	if err := ValidateMessage(msg); err != nil {
		t.Fatalf("nil reply_to_id should pass: %v", err)
	}
}

// ---------- ValidateAddressees tests ----------

func TestValidateAddressees_Valid(t *testing.T) {
	msg := validTextMessage()
	addrs := validAddressees(msg.ID)
	if err := ValidateAddressees(addrs, msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAddressees_InvalidPrincipalKind(t *testing.T) {
	msg := validTextMessage()
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "system",
			PrincipalID:   "scheduler",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
	}
	err := ValidateAddressees(addrs, msg)
	if err == nil {
		t.Fatal("expected error for invalid principal_kind")
	}
	if !strings.Contains(err.Error(), "principal_kind") {
		t.Fatalf("error should mention principal_kind, got: %v", err)
	}
}

func TestValidateAddressees_EmptyPrincipalID(t *testing.T) {
	msg := validTextMessage()
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
	}
	err := ValidateAddressees(addrs, msg)
	if err == nil {
		t.Fatal("expected error for empty principal_id")
	}
}

func TestValidateAddressees_InvalidVia(t *testing.T) {
	msg := validTextMessage()
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "builder",
			Via:           "telepathy",
			DeliveryState: DeliveryPending,
		},
	}
	err := ValidateAddressees(addrs, msg)
	if err == nil {
		t.Fatal("expected error for invalid via")
	}
}

func TestValidateAddressees_InvalidDeliveryState(t *testing.T) {
	msg := validTextMessage()
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "builder",
			Via:           ViaExplicit,
			DeliveryState: "exploded",
		},
	}
	err := ValidateAddressees(addrs, msg)
	if err == nil {
		t.Fatal("expected error for invalid delivery state")
	}
}

func TestValidateAddressees_DuplicateAddressees(t *testing.T) {
	msg := validTextMessage()
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "builder",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "builder",
			Via:           ViaBodyMention,
			DeliveryState: DeliveryPending,
		},
	}
	err := ValidateAddressees(addrs, msg)
	if err == nil {
		t.Fatal("expected error for duplicate addressees")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error should mention duplicate, got: %v", err)
	}
}

func TestValidateAddressees_SameIDDifferentKind(t *testing.T) {
	msg := validTextMessage()
	// Same PrincipalID but different PrincipalKind should be OK.
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "builder",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
		{
			MessageID:     msg.ID,
			PrincipalKind: "user",
			PrincipalID:   "builder",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
	}
	if err := ValidateAddressees(addrs, msg); err != nil {
		t.Fatalf("same ID with different kind should be allowed: %v", err)
	}
}

func TestValidateAddressees_EmptyList(t *testing.T) {
	msg := validTextMessage()
	if err := ValidateAddressees(nil, msg); err != nil {
		t.Fatalf("empty addressee list should be valid: %v", err)
	}
}

// ---------- ValidateCrossProjectAddressees tests (AC-33) ----------

// mockAgentStore implements AgentProjectLookup for testing.
type mockAgentStore struct {
	agents map[string]*store.Agent
}

func (m *mockAgentStore) GetAgent(_ context.Context, id string) (*store.Agent, error) {
	a, ok := m.agents[id]
	if !ok {
		return nil, fmt.Errorf("agent %q not found", id)
	}
	return a, nil
}

func TestValidateCrossProjectAddressees_SameProject(t *testing.T) {
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-1": {ID: "agent-1", ProjectID: "project-a"},
		"agent-2": {ID: "agent-2", ProjectID: "project-a"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-1"},
		{PrincipalKind: "agent", PrincipalID: "agent-2"},
	}
	if err := ValidateCrossProjectAddressees(context.Background(), s, addrs); err != nil {
		t.Fatalf("agents in same project should be OK: %v", err)
	}
}

func TestValidateCrossProjectAddressees_SpanningProjects(t *testing.T) {
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-1": {ID: "agent-1", ProjectID: "project-a"},
		"agent-2": {ID: "agent-2", ProjectID: "project-b"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-1"},
		{PrincipalKind: "agent", PrincipalID: "agent-2"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	if err == nil {
		t.Fatal("expected error for agents spanning projects")
	}
	if !strings.Contains(err.Error(), "multiple projects") {
		t.Fatalf("error should mention multiple projects, got: %v", err)
	}
	// Project IDs must NOT be disclosed in the error (security audit M3).
	if strings.Contains(err.Error(), "project-a") || strings.Contains(err.Error(), "project-b") {
		t.Fatalf("error must not disclose project IDs, got: %v", err)
	}
}

func TestValidateCrossProjectAddressees_UserAddresseesExempt(t *testing.T) {
	// Only agent addressees are checked; users can span projects.
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-1": {ID: "agent-1", ProjectID: "project-a"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "user", PrincipalID: "alice"},
		{PrincipalKind: "agent", PrincipalID: "agent-1"},
		{PrincipalKind: "user", PrincipalID: "bob"},
	}
	if err := ValidateCrossProjectAddressees(context.Background(), s, addrs); err != nil {
		t.Fatalf("user addressees should be exempt from project check: %v", err)
	}
}

func TestValidateCrossProjectAddressees_SingleAgent(t *testing.T) {
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-1": {ID: "agent-1", ProjectID: "project-a"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-1"},
	}
	if err := ValidateCrossProjectAddressees(context.Background(), s, addrs); err != nil {
		t.Fatalf("single agent should pass: %v", err)
	}
}

func TestValidateCrossProjectAddressees_NoAgents(t *testing.T) {
	s := &mockAgentStore{agents: map[string]*store.Agent{}}
	addrs := []Addressee{
		{PrincipalKind: "user", PrincipalID: "alice"},
	}
	if err := ValidateCrossProjectAddressees(context.Background(), s, addrs); err != nil {
		t.Fatalf("no agent addressees should pass: %v", err)
	}
}

// ---------- ValidateMessageAddressees tests ----------

func TestValidateMessageAddressees_Valid(t *testing.T) {
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-1": {ID: "agent-1", ProjectID: "project-a"},
	}}
	msg := validTextMessage()
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "agent-1",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
	}
	if err := ValidateMessageAddressees(context.Background(), s, msg, addrs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMessageAddressees_CrossProjectRejected(t *testing.T) {
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-1": {ID: "agent-1", ProjectID: "project-a"},
		"agent-2": {ID: "agent-2", ProjectID: "project-b"},
	}}
	msg := validTextMessage()
	addrs := []Addressee{
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "agent-1",
			Via:           ViaExplicit,
			DeliveryState: DeliveryPending,
		},
		{
			MessageID:     msg.ID,
			PrincipalKind: "agent",
			PrincipalID:   "agent-2",
			Via:           ViaBodyMention,
			DeliveryState: DeliveryPending,
		},
	}
	err := ValidateMessageAddressees(context.Background(), s, msg, addrs)
	if err == nil {
		t.Fatal("expected error for cross-project addressees")
	}
	if !strings.Contains(err.Error(), "multiple projects") {
		t.Fatalf("error should mention multiple projects, got: %v", err)
	}
}

// TestValidateCrossProjectAddressees_CheckIsLoadBearing proves that the
// cross-project check is load-bearing per Rule 10. If the check were removed
// (e.g. the function body were replaced with `return nil`), this test would
// fail because spanning-projects would incorrectly pass.
func TestValidateCrossProjectAddressees_CheckIsLoadBearing(t *testing.T) {
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-1": {ID: "agent-1", ProjectID: "project-a"},
		"agent-2": {ID: "agent-2", ProjectID: "project-b"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-1"},
		{PrincipalKind: "agent", PrincipalID: "agent-2"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	// If the check is removed, err would be nil and this assertion would fail.
	if err == nil {
		t.Fatal("RULE 10 VIOLATION: cross-project check was removed or bypassed — " +
			"agents in different projects must be rejected")
	}
}
