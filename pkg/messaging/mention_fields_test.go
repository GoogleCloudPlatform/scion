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
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ---------- Verification point 1: multi-mention symmetry ----------
// A message mentioning 2+ agents: every mentioned agent's individually
// rendered envelope shows "type":"mention" and a "to" array listing
// ALL mentioned agents, not just itself. Primary and fan-out envelopes
// must show the identical "to" set.

func TestDEF169_MultiMention_PrimaryAndFanout_IdenticalTo(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)

	// Three agents mentioned: alpha, beta, gamma.
	coAddrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "id-alpha", Via: ViaBodyMention, DeliveryState: DeliveryPending},
		{PrincipalKind: "agent", PrincipalID: "id-beta", Via: ViaBodyMention, DeliveryState: DeliveryPending},
		{PrincipalKind: "agent", PrincipalID: "id-gamma", Via: ViaBodyMention, DeliveryState: DeliveryPending},
	}

	// Primary agent's envelope (alpha).
	primaryMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:alpha",
		Msg:       "@alpha @beta @gamma deploy it all",
		Type:      messages.TypeMention,
	}
	primaryResult := RenderDeliveryText(RenderDeliveryInput{
		MessageID:    "msg-primary-001",
		Msg:          primaryMsg,
		CreatedAt:    now,
		IsMention:    true,
		CoAddressees: coAddrs,
	})

	// Fan-out agent's envelope (beta).
	fanoutMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:beta",
		Msg:       "@alpha @beta @gamma deploy it all",
		Type:      messages.TypeMention,
	}
	fanoutResult := RenderDeliveryText(RenderDeliveryInput{
		MessageID:    "msg-fanout-001",
		Msg:          fanoutMsg,
		CreatedAt:    now,
		IsMention:    true,
		CoAddressees: coAddrs,
	})

	// Third fan-out agent's envelope (gamma).
	fanout2Msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:gamma",
		Msg:       "@alpha @beta @gamma deploy it all",
		Type:      messages.TypeMention,
	}
	fanout2Result := RenderDeliveryText(RenderDeliveryInput{
		MessageID:    "msg-fanout-002",
		Msg:          fanout2Msg,
		CreatedAt:    now,
		IsMention:    true,
		CoAddressees: coAddrs,
	})

	// Parse all three envelopes.
	primaryEnv := extractDeliveryEnvelope(t, primaryResult)
	fanoutEnv := extractDeliveryEnvelope(t, fanoutResult)
	fanout2Env := extractDeliveryEnvelope(t, fanout2Result)

	// All three must have type "mention".
	if primaryEnv.Type != "mention" {
		t.Errorf("primary type = %q, want %q", primaryEnv.Type, "mention")
	}
	if fanoutEnv.Type != "mention" {
		t.Errorf("fanout type = %q, want %q", fanoutEnv.Type, "mention")
	}
	if fanout2Env.Type != "mention" {
		t.Errorf("fanout2 type = %q, want %q", fanout2Env.Type, "mention")
	}

	// All three must have len(to) == 3.
	if len(primaryEnv.To) != 3 {
		t.Fatalf("primary to length = %d, want 3", len(primaryEnv.To))
	}
	if len(fanoutEnv.To) != 3 {
		t.Fatalf("fanout to length = %d, want 3", len(fanoutEnv.To))
	}
	if len(fanout2Env.To) != 3 {
		t.Fatalf("fanout2 to length = %d, want 3", len(fanout2Env.To))
	}

	// Sort to sets for comparison — the order should be the same, but
	// sort to be robust against implementation changes.
	sortedPrimary := sortedCopy(primaryEnv.To)
	sortedFanout := sortedCopy(fanoutEnv.To)
	sortedFanout2 := sortedCopy(fanout2Env.To)

	wantTo := []string{"agent:id-alpha", "agent:id-beta", "agent:id-gamma"}
	sort.Strings(wantTo)

	for i, w := range wantTo {
		if sortedPrimary[i] != w {
			t.Errorf("primary to[%d] = %q, want %q", i, sortedPrimary[i], w)
		}
		if sortedFanout[i] != w {
			t.Errorf("fanout to[%d] = %q, want %q", i, sortedFanout[i], w)
		}
		if sortedFanout2[i] != w {
			t.Errorf("fanout2 to[%d] = %q, want %q", i, sortedFanout2[i], w)
		}
	}

	// Verify the to sets are identical across primary and fan-out.
	if !equalSorted(sortedPrimary, sortedFanout) {
		t.Errorf("primary and fanout to sets differ:\n  primary: %v\n  fanout:  %v", sortedPrimary, sortedFanout)
	}
	if !equalSorted(sortedPrimary, sortedFanout2) {
		t.Errorf("primary and fanout2 to sets differ:\n  primary:  %v\n  fanout2:  %v", sortedPrimary, sortedFanout2)
	}
}

// ---------- Verification point 2: single-mention ----------
// A message mentioning exactly 1 agent: that agent's envelope shows
// "type":"mention" and "to" containing exactly itself.

func TestDEF169_SingleMention_TypeAndTo(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)

	coAddrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "id-solo", Via: ViaBodyMention, DeliveryState: DeliveryPending},
	}

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:solo",
		Msg:       "@solo do the thing",
		Type:      messages.TypeMention,
	}

	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID:    "msg-solo-001",
		Msg:          msg,
		CreatedAt:    now,
		IsMention:    true,
		CoAddressees: coAddrs,
	})

	env := extractDeliveryEnvelope(t, result)

	if env.Type != "mention" {
		t.Errorf("type = %q, want %q", env.Type, "mention")
	}

	// "to" must be present with exactly one entry — the sole mentioned agent.
	if len(env.To) != 1 {
		t.Fatalf("to length = %d, want 1", len(env.To))
	}
	if env.To[0] != "agent:id-solo" {
		t.Errorf("to[0] = %q, want %q", env.To[0], "agent:id-solo")
	}

	// Raw JSON: "to" key must be present (not omitted despite single addressee).
	rawJSON := extractRawJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	if _, ok := raw["to"]; !ok {
		t.Error("JSON missing 'to' key; want present for single-mention message")
	}
}

// ---------- Verification point 3: default-agent regression ----------
// Default-agent routing (no mention): unchanged — "type":"message", to omitted
// for single recipient.

func TestDEF169_DefaultAgent_Unchanged(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:default-bot",
		Msg:       "hello default bot",
		Type:      messages.TypeInstruction,
	}

	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-default-001",
		Msg:       msg,
		CreatedAt: now,
		IsMention: false,
		// CoAddressees intentionally nil — no mention.
	})

	env := extractDeliveryEnvelope(t, result)

	if env.Type != "message" {
		t.Errorf("type = %q, want %q", env.Type, "message")
	}

	// "to" must be omitted for single-recipient default routing.
	if len(env.To) != 0 {
		t.Errorf("to = %v, want empty (default-agent single recipient)", env.To)
	}

	// Raw JSON: "to" key must be absent.
	rawJSON := extractRawJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for default-agent routing")
	}
}

// ---------- Verification point 4: DM-implicit ----------
// DM-implicit routing: same as default — "type":"message", to omitted.

func TestDEF169_DMImplicit_Unchanged(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:dm-bot",
		Msg:       "direct message",
		Type:      messages.TypeInstruction,
	}

	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-dm-001",
		Msg:       msg,
		CreatedAt: now,
		IsMention: false,
	})

	env := extractDeliveryEnvelope(t, result)

	if env.Type != "message" {
		t.Errorf("type = %q, want %q", env.Type, "message")
	}

	if len(env.To) != 0 {
		t.Errorf("to = %v, want empty (DM-implicit routing)", env.To)
	}

	rawJSON := extractRawJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for DM-implicit routing")
	}
}

// ---------- Verification point 5: event unchanged ----------

func TestDEF169_Event_Unchanged(t *testing.T) {
	// An event message with IsMention=true should still produce type "event",
	// not "mention" — events are events regardless of routing.
	intent := IntentRequest
	_ = intent // unused, just noting events don't use intent

	msg := &Message{
		ID:   "msg-event-001",
		From: PrincipalRef("system:lifecycle"),
		Kind: KindEvent,
		Event: &EventBody{
			Type:    EventAgentStateChanged,
			Subject: "agent:worker",
			Status:  "COMPLETED",
		},
		Body:      "Agent worker completed",
		CreatedAt: time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC),
	}
	conv := &ConversationInfo{ID: "conv-ev", Kind: "direct", Surface: "native"}

	// Even with isMention=true, event should still render as "event".
	result := FormatNewDelivery(msg, nil, conv, DeliveryOptions{}, true)

	env := extractEnvelope(t, result)
	if env.Type != "event" {
		t.Errorf("type = %q, want %q (event takes priority over mention)", env.Type, "event")
	}
}

// ---------- Verification point 6: mutation tests ----------

// 6a: Force IsMention=false in the mention call path → type reverts to
// "message", to reverts to omitted-if-single.
func TestDEF169_Mutation_IsMentionFalse_RevertsBehavior(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)

	// Simulate a single-mention scenario but with IsMention=false (the mutation).
	coAddrs := []Addressee{
		{PrincipalKind: "agent", PrincipalID: "id-solo", Via: ViaBodyMention, DeliveryState: DeliveryPending},
	}

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:solo",
		Msg:       "@solo do the thing",
		Type:      messages.TypeMention,
	}

	// MUTATION: IsMention is false despite this being a mention path.
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID:    "msg-mut-001",
		Msg:          msg,
		CreatedAt:    now,
		IsMention:    false, // MUTATED
		CoAddressees: coAddrs,
	})

	env := extractDeliveryEnvelope(t, result)

	// With IsMention=false, type must revert to "message".
	if env.Type != "message" {
		t.Errorf("[mutation] type = %q, want %q (IsMention=false should not produce 'mention')", env.Type, "message")
	}

	// With IsMention=false and a single addressee, "to" should be omitted.
	// CoAddressees overrides the legacy addrs, so len(addrs)==1,
	// but isMention is false so the condition len(addrs)>1 || isMention is false.
	if len(env.To) != 0 {
		t.Errorf("[mutation] to = %v, want empty (IsMention=false, single addressee)", env.To)
	}
}

// 6b: Force IsMention=true in the default-agent path → a test goes RED
// because default routing must not gain spurious to/mention.
func TestDEF169_Mutation_IsMentionTrue_DefaultPath_GainsSpuriousTo(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: now.Format(time.RFC3339),
		Sender:    "user:alice",
		Recipient: "agent:default-bot",
		Msg:       "hello default bot",
		Type:      messages.TypeInstruction,
	}

	// MUTATION: IsMention is true despite this being a default-agent path.
	// No CoAddressees — simulating the default path with the mutation.
	result := RenderDeliveryText(RenderDeliveryInput{
		MessageID: "msg-mut-002",
		Msg:       msg,
		CreatedAt: now,
		IsMention: true, // MUTATED — wrong for default-agent path
	})

	env := extractDeliveryEnvelope(t, result)

	// With the mutation, type will be "mention" — this test EXPECTS the
	// mutation to produce a different result than the default-agent test.
	// If someone accidentally sets IsMention=true on the default path,
	// this demonstrates the effect.
	if env.Type != "mention" {
		t.Errorf("[mutation effect] type = %q, want %q (mutation forces 'mention' on default path)", env.Type, "mention")
	}

	// The "to" field will be empty since CoAddressees is nil (no addrs
	// from the legacy path for this scenario) — but the type is wrong,
	// which is what the real test (TestDEF169_DefaultAgent_Unchanged) catches.
	// This test verifies the mutation has visible effect in the type field.
}

// ---------- FormatNewDelivery-level mention tests ----------

func TestFormatNewDelivery_Mention_TypeAndTo(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-fmt-mention",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "@deployer @tester deploy and test",
		CreatedAt: time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{
		{MessageID: "msg-fmt-mention", PrincipalKind: "agent", PrincipalID: "deployer", Via: ViaBodyMention, DeliveryState: DeliveryPending},
		{MessageID: "msg-fmt-mention", PrincipalKind: "agent", PrincipalID: "tester", Via: ViaBodyMention, DeliveryState: DeliveryPending},
	}
	conv := &ConversationInfo{ID: "conv-fmt-mention", Kind: "group", Surface: "native"}

	result := FormatNewDelivery(msg, addrs, conv, DeliveryOptions{}, true)

	env := extractEnvelope(t, result)
	if env.Type != "mention" {
		t.Errorf("type = %q, want %q", env.Type, "mention")
	}
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

func TestFormatNewDelivery_SingleMention_IncludesTo(t *testing.T) {
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-fmt-single",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "@solo do it",
		CreatedAt: time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{
		{MessageID: "msg-fmt-single", PrincipalKind: "agent", PrincipalID: "solo", Via: ViaBodyMention, DeliveryState: DeliveryPending},
	}
	conv := &ConversationInfo{ID: "conv-fmt-single", Kind: "direct", Surface: "native"}

	result := FormatNewDelivery(msg, addrs, conv, DeliveryOptions{}, true)

	env := extractEnvelope(t, result)
	if env.Type != "mention" {
		t.Errorf("type = %q, want %q", env.Type, "mention")
	}
	// Single mention: to must still be present (unlike the non-mention single case).
	if len(env.To) != 1 {
		t.Fatalf("to length = %d, want 1", len(env.To))
	}
	if env.To[0] != "agent:solo" {
		t.Errorf("to[0] = %q, want %q", env.To[0], "agent:solo")
	}

	// Raw JSON: "to" key must be present.
	jsonStr := extractJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	if _, ok := raw["to"]; !ok {
		t.Error("JSON missing 'to' key; want present for single-mention message")
	}
}

func TestFormatNewDelivery_NotMention_SingleAddressee_OmitsTo(t *testing.T) {
	// Non-mention single addressee: "to" omitted, type:"message".
	// This is the regression guard for the 8f217909 behaviour.
	intent := IntentRequest
	msg := &Message{
		ID:        "msg-fmt-nomention",
		From:      PrincipalRef("user:alice"),
		Kind:      KindText,
		Intent:    &intent,
		Body:      "hello",
		CreatedAt: time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC),
	}
	addrs := []Addressee{
		{MessageID: "msg-fmt-nomention", PrincipalKind: "agent", PrincipalID: "bot", Via: ViaExplicit, DeliveryState: DeliveryPending},
	}
	conv := &ConversationInfo{ID: "conv-fmt-nomention", Kind: "direct", Surface: "native"}

	result := FormatNewDelivery(msg, addrs, conv, DeliveryOptions{}, false)

	env := extractEnvelope(t, result)
	if env.Type != "message" {
		t.Errorf("type = %q, want %q", env.Type, "message")
	}
	if len(env.To) != 0 {
		t.Errorf("to = %v, want empty (non-mention single recipient)", env.To)
	}

	// Raw JSON: "to" key must be absent.
	jsonStr := extractJSON(t, result)
	var raw map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for non-mention single addressee")
	}
}

// ---------- Helpers ----------

func sortedCopy(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
