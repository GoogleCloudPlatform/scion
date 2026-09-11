//go:build !no_sqlite

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

package hub

// DEF-169: integration tests for mention routing surfaced in type and to.
//
// These tests exercise the REAL handler path (handleConversationSend →
// sendAgentRouted) with a mock dispatcher, then decode the rendered
// DeliveryText on the dispatched StructuredMessage to verify the envelope
// contains type:"mention" and the correct "to" list.
//
// This closes the gap between "the rendering function works when called
// correctly" (tested in pkg/messaging/mention_fields_test.go) and "the
// handler calls it correctly" (tested here).

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/v2"
)

// def169Envelope is a minimal struct matching the DeliveryEnvelope shape,
// used to decode DeliveryText without importing the full delivery package
// type into pkg/hub's test package.
type def169Envelope struct {
	Type string   `json:"type"`
	To   []string `json:"to,omitempty"`
	From string   `json:"from"`
	Msg  string   `json:"msg"`
}

// parseDEF169Envelope extracts and decodes the JSON envelope from a rendered
// DeliveryText string (between the BEGIN/END SCION MESSAGE delimiters).
func parseDEF169Envelope(t *testing.T, deliveryText string) def169Envelope {
	t.Helper()
	jsonStr := extractDEF169JSON(t, deliveryText)
	var env def169Envelope
	if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
		t.Fatalf("failed to unmarshal envelope JSON: %v\nJSON: %s", err, jsonStr)
	}
	return env
}

// extractDEF169JSON pulls the raw JSON from between delimiters.
func extractDEF169JSON(t *testing.T, deliveryText string) string {
	t.Helper()
	const begin = "---BEGIN SCION MESSAGE---"
	const end = "---END SCION MESSAGE---"

	startIdx := 0
	for i := 0; i+len(begin) <= len(deliveryText); i++ {
		if deliveryText[i:i+len(begin)] == begin {
			startIdx = i + len(begin) + 1 // skip newline after delimiter
			break
		}
	}
	endIdx := len(deliveryText)
	for i := len(deliveryText) - len(end); i >= 0; i-- {
		if deliveryText[i:i+len(end)] == end {
			endIdx = i - 1 // trim newline before end delimiter
			break
		}
	}
	if startIdx == 0 || endIdx <= startIdx {
		t.Fatalf("could not find delimiters in DeliveryText:\n%s", deliveryText)
	}
	return deliveryText[startIdx:endIdx]
}

// enableEnvelopeSwitch creates OperationalSettings with defaults (envelope
// switch is ON by compiled default when the messaging section is absent)
// and attaches them to the server so writeDenyEnabled() returns true.
func enableEnvelopeSwitch(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	ops := NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	srv.SetOperationalSettings(ops)
}

// TestDEF169_Integration_MultiMention_EnvelopeTypeAndTo verifies that when
// a message mentioning 2 agents goes through the real handler, BOTH agents'
// dispatched DeliveryText contains type:"mention" and a "to" array listing
// ALL mentioned agents — not just the recipient.
func TestDEF169_Integration_MultiMention_EnvelopeTypeAndTo(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Create two agents.
	agentA := &store.Agent{
		ID:        tid("def169-agent-a"),
		ProjectID: proj.ID,
		Name:      "Agent Alpha",
		Slug:      "agent-alpha",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("def169-agent-b"),
		ProjectID: proj.ID,
		Name:      "Agent Beta",
		Slug:      "agent-beta",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	// Topic with no default_agent — routing is purely mention-driven.
	topicID := tid("def169-topic-multi")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "def169-multi-mention",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	// Send message mentioning both agents.
	body := map[string]string{"content": "@agent-alpha @agent-beta deploy and test"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Collect dispatched messages.
	dispatched := dispatcher.getMessages()
	if len(dispatched) < 2 {
		t.Fatalf("expected at least 2 dispatch calls, got %d", len(dispatched))
	}

	// Build a map of slug → envelope.
	envelopes := make(map[string]def169Envelope)
	for _, d := range dispatched {
		if d.structured == nil {
			t.Errorf("dispatch to %q has nil structured message", d.agentSlug)
			continue
		}
		if d.structured.DeliveryText == "" {
			t.Fatalf("dispatch to %q has empty DeliveryText — writeDenyEnabled() path not reached", d.agentSlug)
		}
		envelopes[d.agentSlug] = parseDEF169Envelope(t, d.structured.DeliveryText)
	}

	// Both agents must have type "mention".
	for _, slug := range []string{"agent-alpha", "agent-beta"} {
		env, ok := envelopes[slug]
		if !ok {
			t.Errorf("no envelope found for %s", slug)
			continue
		}
		if env.Type != "mention" {
			t.Errorf("%s: type = %q, want %q", slug, env.Type, "mention")
		}
	}

	// Both agents must see the same "to" list containing both agents' IDs.
	wantTo := []string{"agent:" + agentA.ID, "agent:" + agentB.ID}
	sort.Strings(wantTo)

	for _, slug := range []string{"agent-alpha", "agent-beta"} {
		env := envelopes[slug]
		gotTo := make([]string, len(env.To))
		copy(gotTo, env.To)
		sort.Strings(gotTo)

		if len(gotTo) != len(wantTo) {
			t.Errorf("%s: to length = %d, want %d; got %v", slug, len(gotTo), len(wantTo), gotTo)
			continue
		}
		for i, w := range wantTo {
			if gotTo[i] != w {
				t.Errorf("%s: to[%d] = %q, want %q", slug, i, gotTo[i], w)
			}
		}
	}

	// Primary and fan-out envelopes must have IDENTICAL to sets.
	envA := envelopes["agent-alpha"]
	envB := envelopes["agent-beta"]
	toA := def169SortedStrings(envA.To)
	toB := def169SortedStrings(envB.To)
	if len(toA) != len(toB) {
		t.Errorf("primary and fanout to sets have different lengths: %v vs %v", toA, toB)
	} else {
		for i := range toA {
			if toA[i] != toB[i] {
				t.Errorf("primary and fanout to sets differ at [%d]: %q vs %q", i, toA[i], toB[i])
			}
		}
	}
}

// TestDEF169_Integration_SingleMention_EnvelopeTypeAndTo verifies that when
// a message mentioning exactly 1 agent goes through the real handler, the
// dispatched DeliveryText contains type:"mention" and "to" naming itself.
func TestDEF169_Integration_SingleMention_EnvelopeTypeAndTo(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := &store.Agent{
		ID:        tid("def169-solo"),
		ProjectID: proj.ID,
		Name:      "Solo Agent",
		Slug:      "solo-agent",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	topicID := tid("def169-topic-single")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "def169-single-mention",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "@solo-agent do the thing"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", len(dispatched))
	}

	d := dispatched[0]
	if d.structured == nil || d.structured.DeliveryText == "" {
		t.Fatal("dispatch has nil structured or empty DeliveryText")
	}

	env := parseDEF169Envelope(t, d.structured.DeliveryText)
	if env.Type != "mention" {
		t.Errorf("type = %q, want %q", env.Type, "mention")
	}
	if len(env.To) != 1 {
		t.Fatalf("to length = %d, want 1", len(env.To))
	}
	wantTo := "agent:" + agent.ID
	if env.To[0] != wantTo {
		t.Errorf("to[0] = %q, want %q", env.To[0], wantTo)
	}
}

// TestDEF169_Integration_DefaultAgent_NoMentionType verifies that
// default-agent routing (no mention) still produces type:"message" and
// omits "to" for a single recipient — the 8f217909 behavior unchanged.
func TestDEF169_Integration_DefaultAgent_NoMentionType(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := &store.Agent{
		ID:        tid("def169-default"),
		ProjectID: proj.ID,
		Name:      "Default Agent",
		Slug:      "default-agent",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	topicID := tid("def169-topic-default")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "def169-default-agent",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agent.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	// Message WITHOUT any @mention — routed via default_agent.
	body := map[string]string{"content": "hello default agent, no mention here"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", len(dispatched))
	}

	d := dispatched[0]
	if d.structured == nil || d.structured.DeliveryText == "" {
		t.Fatal("dispatch has nil structured or empty DeliveryText")
	}

	env := parseDEF169Envelope(t, d.structured.DeliveryText)
	if env.Type != "message" {
		t.Errorf("type = %q, want %q (default-agent routing, not mention)", env.Type, "message")
	}
	if len(env.To) != 0 {
		t.Errorf("to = %v, want empty (single-recipient default-agent routing)", env.To)
	}

	// Also verify via raw JSON that "to" key is absent.
	rawJSON := extractDEF169JSON(t, d.structured.DeliveryText)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("unmarshal raw JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for default-agent routing")
	}
}

func def169SortedStrings(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}
