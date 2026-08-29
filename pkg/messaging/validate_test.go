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
	if !strings.Contains(err.Error(), "across project boundaries") {
		t.Fatalf("error should mention project boundaries, got: %v", err)
	}
	// AC-33: error MUST name the rejected addressee IDs.
	if !strings.Contains(err.Error(), "agent-2") {
		t.Fatalf("error should name the rejected addressee, got: %v", err)
	}
	// AC-33: project IDs must NOT be disclosed in the error.
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

// ---------- DEF-40: sentinel-collision tests ----------

func TestValidateCrossProjectAddressees_EmptyProjectFirst(t *testing.T) {
	// Empty-project agent first, then normal agent → must reject (order-independent).
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-empty": {ID: "agent-empty", ProjectID: ""},
		"agent-p1":    {ID: "agent-p1", ProjectID: "project-a"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-empty"},
		{PrincipalKind: "agent", PrincipalID: "agent-p1"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	if err == nil {
		t.Fatal("expected error: empty-project agent alongside placed agent must be rejected")
	}
	// AC-33: error names rejected addressees but never project IDs.
	if !strings.Contains(err.Error(), "agent-empty") {
		t.Fatalf("error should name the rejected addressee, got: %v", err)
	}
	if strings.Contains(err.Error(), "project-a") {
		t.Fatalf("error must not disclose project IDs, got: %v", err)
	}
}

func TestValidateCrossProjectAddressees_EmptyProjectSecond(t *testing.T) {
	// Normal agent first, then empty-project agent → must also reject.
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-p1":    {ID: "agent-p1", ProjectID: "project-a"},
		"agent-empty": {ID: "agent-empty", ProjectID: ""},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-p1"},
		{PrincipalKind: "agent", PrincipalID: "agent-empty"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	if err == nil {
		t.Fatal("expected error: empty-project agent alongside placed agent must be rejected regardless of order")
	}
	if !strings.Contains(err.Error(), "agent-empty") {
		t.Fatalf("error should name the rejected addressee, got: %v", err)
	}
	if strings.Contains(err.Error(), "project-a") {
		t.Fatalf("error must not disclose project IDs, got: %v", err)
	}
}

func TestValidateCrossProjectAddressees_ZeroUUIDFirst(t *testing.T) {
	// Zero-UUID agent first, then normal agent → must reject.
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-zero": {ID: "agent-zero", ProjectID: "00000000-0000-0000-0000-000000000000"},
		"agent-p1":   {ID: "agent-p1", ProjectID: "project-a"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-zero"},
		{PrincipalKind: "agent", PrincipalID: "agent-p1"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	if err == nil {
		t.Fatal("expected error: zero-UUID agent alongside placed agent must be rejected")
	}
	if !strings.Contains(err.Error(), "agent-zero") {
		t.Fatalf("error should name the rejected addressee, got: %v", err)
	}
	if strings.Contains(err.Error(), "project-a") {
		t.Fatalf("error must not disclose project IDs, got: %v", err)
	}
}

func TestValidateCrossProjectAddressees_ZeroUUIDSecond(t *testing.T) {
	// Normal agent first, then zero-UUID agent → must also reject.
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-p1":   {ID: "agent-p1", ProjectID: "project-a"},
		"agent-zero": {ID: "agent-zero", ProjectID: "00000000-0000-0000-0000-000000000000"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-p1"},
		{PrincipalKind: "agent", PrincipalID: "agent-zero"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	if err == nil {
		t.Fatal("expected error: zero-UUID agent alongside placed agent must be rejected regardless of order")
	}
	if !strings.Contains(err.Error(), "agent-zero") {
		t.Fatalf("error should name the rejected addressee, got: %v", err)
	}
	if strings.Contains(err.Error(), "project-a") {
		t.Fatalf("error must not disclose project IDs, got: %v", err)
	}
}

func TestValidateCrossProjectAddressees_SingleEmptyProjectAgent(t *testing.T) {
	// A single unplaceable agent alone has no cross-project issue → must pass.
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-empty": {ID: "agent-empty", ProjectID: ""},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-empty"},
	}
	if err := ValidateCrossProjectAddressees(context.Background(), s, addrs); err != nil {
		t.Fatalf("single unplaceable agent should pass: %v", err)
	}
}

func TestValidateCrossProjectAddressees_SingleZeroUUIDAgent(t *testing.T) {
	// A single zero-UUID agent alone has no cross-project issue → must pass.
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-zero": {ID: "agent-zero", ProjectID: "00000000-0000-0000-0000-000000000000"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-zero"},
	}
	if err := ValidateCrossProjectAddressees(context.Background(), s, addrs); err != nil {
		t.Fatalf("single zero-UUID agent should pass: %v", err)
	}
}

func TestValidateCrossProjectAddressees_MultipleUnplaceableAgents(t *testing.T) {
	// Multiple unplaceable agents → cannot verify same-project → must reject.
	s := &mockAgentStore{agents: map[string]*store.Agent{
		"agent-empty": {ID: "agent-empty", ProjectID: ""},
		"agent-zero":  {ID: "agent-zero", ProjectID: "00000000-0000-0000-0000-000000000000"},
	}}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "agent-empty"},
		{PrincipalKind: "agent", PrincipalID: "agent-zero"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	if err == nil {
		t.Fatal("expected error: multiple unplaceable agents must be rejected")
	}
	if !strings.Contains(err.Error(), "agent-empty") || !strings.Contains(err.Error(), "agent-zero") {
		t.Fatalf("error should name all rejected addressees, got: %v", err)
	}
}

// ---------- Nil agent guard test (DEF-40 adjacent) ----------

// nilAgentStore is a pathological store that returns (nil, nil) — no agent, no
// error. This models a buggy or incomplete AgentProjectLookup implementation.
type nilAgentStore struct{}

func (s *nilAgentStore) GetAgent(_ context.Context, _ string) (*store.Agent, error) {
	return nil, nil // pathological: no agent, no error
}

func TestValidateCrossProjectAddressees_NilAgentDenied(t *testing.T) {
	// A store that returns (nil, nil) must produce an error, not a panic.
	s := &nilAgentStore{}
	addrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "ghost-agent"},
	}
	err := ValidateCrossProjectAddressees(context.Background(), s, addrs)
	if err == nil {
		t.Fatal("nil agent with nil error must be denied, not silently passed")
	}
	if !strings.Contains(err.Error(), "ghost-agent") {
		t.Fatalf("error should name the agent, got: %v", err)
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

// ---------- ValidateAttributed (DEF-41 — function correctness) ----------

// TestValidateAttributed_RejectsEmptyConversationID proves that
// ValidateAttributed rejects an empty ConversationID.
//
// This tests the function body, not production reachability. While B10
// holds, every production call site guards ValidateAttributed behind
// `if convResult != nil`, and every non-nil convResult carries a
// uuid.UUID ConversationID that is never empty. The check is therefore
// structural pre-placement: it cannot fire today, and it becomes
// load-bearing at Tranche G when derivation failure becomes fatal and
// the nil guard is removed.
//
// See the commit message for the proof-by-enumeration that no
// production path can deliver "" to ValidateAttributed under B10.
func TestValidateAttributed_RejectsEmptyConversationID(t *testing.T) {
	err := ValidateAttributed("")
	if err == nil {
		t.Fatal("ValidateAttributed must reject an empty conversation_id")
	}
	if !strings.Contains(err.Error(), "conversation_id") {
		t.Fatalf("error should mention conversation_id, got: %v", err)
	}
}

// TestValidateAttributed_AcceptsNonEmptyConversationID confirms that
// ValidateAttributed passes when a real ConversationID is present.
func TestValidateAttributed_AcceptsNonEmptyConversationID(t *testing.T) {
	err := ValidateAttributed("conv-12345")
	if err != nil {
		t.Fatalf("ValidateAttributed should accept a non-empty conversation_id, got: %v", err)
	}
}

// TestValidateAttributed_CheckIsLoadBearing proves that the ValidateAttributed
// check is load-bearing per Rule 10. If the function body were replaced with
// `return nil`, this test would fail because an empty ConversationID would
// incorrectly pass. The function is correctly implemented; it is the
// production call sites that are currently inert (see above).
func TestValidateAttributed_CheckIsLoadBearing(t *testing.T) {
	err := ValidateAttributed("")
	// If the check is removed, err would be nil and this assertion would fail.
	if err == nil {
		t.Fatal("RULE 10 VIOLATION: ValidateAttributed check was removed or " +
			"bypassed — an empty conversation_id after attribution must be rejected")
	}
}

// ---------- validateMessageContent (internal, shared core) ----------

// TestValidateMessageContent_SkipsConversationIDCheck confirms that the shared
// core validator (used by ValidateLegacyMessage) does not check ConversationID.
// ConversationID is checked by ValidateAttributed (after attribution).
func TestValidateMessageContent_SkipsConversationIDCheck(t *testing.T) {
	msg := validTextMessage()
	msg.ConversationID = "" // intentionally empty
	err := validateMessageContent(msg)
	if err != nil {
		t.Fatalf("validateMessageContent must not check ConversationID "+
			"(that is ValidateAttributed's job), got: %v", err)
	}
}
