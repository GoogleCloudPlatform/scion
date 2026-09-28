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

// Leading-mention override routing tests: verifies that a message starting
// with @agent-name dispatches ONLY to the mentioned agents (override),
// while non-leading mentions remain additive (prepend default/DM agent).

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Test 1: Scenario C — leading sole mention overrides default
// Topic default = agent-a, message = "@agent-b please help" (leading, sole).
// Only agent-b dispatched; agent-a (default) is NOT dispatched.
func TestLeadingMention_SoleMentionOverridesDefault(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("lead-default-a"),
		ProjectID: proj.ID,
		Name:      "Default Agent A",
		Slug:      "lead-default-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("lead-mention-b"),
		ProjectID: proj.ID,
		Name:      "Mentioned Agent B",
		Slug:      "lead-mention-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("lead-topic-sole")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "leading-sole-mention",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agentA.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "@lead-mention-b please help"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		slugs := make([]string, len(dispatched))
		for i, d := range dispatched {
			slugs[i] = d.agentSlug
		}
		t.Fatalf("expected 1 dispatch (only agent-b), got %d: %v", len(dispatched), slugs)
	}

	d := dispatched[0]
	if d.agentSlug != "lead-mention-b" {
		t.Fatalf("dispatched to %q, want %q", d.agentSlug, "lead-mention-b")
	}
	if d.structured == nil || d.structured.DeliveryText == "" {
		t.Fatal("dispatch has nil structured or empty DeliveryText")
	}

	env := parseAdditiveEnvelope(t, d.structured.DeliveryText)
	if env.Type != "message" {
		t.Errorf("type = %q, want %q (sole leading mention is primary)", env.Type, "message")
	}

	// Single recipient: no "to" field.
	rawJSON := extractDEF169JSON(t, d.structured.DeliveryText)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("unmarshal raw JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for single leading-mention override")
	}

	t.Logf("Test 1 (scenario C) — lead-mention-b envelope: %+v", env)
}

// Test 2: Scenario D — leading multi-mention, no default dispatched
// Topic default = agent-a, message = "@agent-b hello @agent-c" (leading + body).
// agent-b and agent-c dispatched; agent-a is NOT dispatched.
func TestLeadingMention_MultiMentionNoDefault(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("lead-multi-a"),
		ProjectID: proj.ID,
		Name:      "Default Agent A",
		Slug:      "lead-multi-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("lead-multi-b"),
		ProjectID: proj.ID,
		Name:      "Lead Mention B",
		Slug:      "lead-multi-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentC := &store.Agent{
		ID:        tid("lead-multi-c"),
		ProjectID: proj.ID,
		Name:      "Body Mention C",
		Slug:      "lead-multi-c",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB, agentC} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("lead-topic-multi")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "leading-multi-mention",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agentA.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "@lead-multi-b hello @lead-multi-c"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		slugs := make([]string, len(dispatched))
		for i, d := range dispatched {
			slugs[i] = d.agentSlug
		}
		t.Fatalf("expected 2 dispatches (agent-b, agent-c), got %d: %v", len(dispatched), slugs)
	}

	// Verify agent-a is NOT dispatched.
	for _, d := range dispatched {
		if d.agentSlug == "lead-multi-a" {
			t.Fatal("default agent lead-multi-a was dispatched; leading mention should override")
		}
	}

	envelopes := collectEnvelopes(t, dispatched)

	envB, ok := envelopes["lead-multi-b"]
	if !ok {
		t.Fatal("no envelope found for lead-multi-b")
	}
	if envB.Type != "message" {
		t.Errorf("lead-multi-b (leading, primary): type = %q, want %q", envB.Type, "message")
	}

	envC, ok := envelopes["lead-multi-c"]
	if !ok {
		t.Fatal("no envelope found for lead-multi-c")
	}
	if envC.Type != "mention" {
		t.Errorf("lead-multi-c (secondary): type = %q, want %q", envC.Type, "mention")
	}

	wantTo := []string{"agent:lead-multi-b", "agent:lead-multi-c"}
	assertToEqual(t, "lead-multi-b", envB.To, wantTo)
	assertToEqual(t, "lead-multi-c", envC.To, wantTo)

	t.Logf("Test 2 (scenario D) — lead-multi-b envelope: %+v", envB)
	t.Logf("Test 2 (scenario D) — lead-multi-c envelope: %+v", envC)
}

// Test 3: Scenario B regression — non-leading mention is still additive
// Topic default = agent-a, message = "hey there @agent-b can you help" (non-leading).
// Both agent-a and agent-b dispatched (additive).
func TestLeadingMention_NonLeadingIsAdditive(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("lead-add-a"),
		ProjectID: proj.ID,
		Name:      "Default Agent A",
		Slug:      "lead-add-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("lead-add-b"),
		ProjectID: proj.ID,
		Name:      "Mentioned Agent B",
		Slug:      "lead-add-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("lead-topic-additive")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "leading-additive-regression",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agentA.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "hey there @lead-add-b can you help"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		slugs := make([]string, len(dispatched))
		for i, d := range dispatched {
			slugs[i] = d.agentSlug
		}
		t.Fatalf("expected 2 dispatches (additive: agent-a + agent-b), got %d: %v", len(dispatched), slugs)
	}

	envelopes := collectEnvelopes(t, dispatched)

	envA, ok := envelopes["lead-add-a"]
	if !ok {
		t.Fatal("no envelope found for lead-add-a (default, additive)")
	}
	if envA.Type != "message" {
		t.Errorf("lead-add-a (default, primary): type = %q, want %q", envA.Type, "message")
	}

	envB, ok := envelopes["lead-add-b"]
	if !ok {
		t.Fatal("no envelope found for lead-add-b")
	}
	if envB.Type != "mention" {
		t.Errorf("lead-add-b (secondary): type = %q, want %q", envB.Type, "mention")
	}

	wantTo := []string{"agent:lead-add-a", "agent:lead-add-b"}
	assertToEqual(t, "lead-add-a", envA.To, wantTo)
	assertToEqual(t, "lead-add-b", envB.To, wantTo)

	t.Logf("Test 3 (scenario B regression) — lead-add-a envelope: %+v", envA)
	t.Logf("Test 3 (scenario B regression) — lead-add-b envelope: %+v", envB)
}

// Test 4: Leading mention IS the default agent
// Topic default = agent-a, message = "@agent-a hello" (leading, names the default).
// Single dispatch to agent-a, type:"message", no "to".
func TestLeadingMention_LeadingIsDefault(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("lead-self-a"),
		ProjectID: proj.ID,
		Name:      "Default Agent A",
		Slug:      "lead-self-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agentA); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	topicID := tid("lead-topic-self")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "leading-is-default",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agentA.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "@lead-self-a hello"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 1 {
		slugs := make([]string, len(dispatched))
		for i, d := range dispatched {
			slugs[i] = d.agentSlug
		}
		t.Fatalf("expected 1 dispatch (agent-a only), got %d: %v", len(dispatched), slugs)
	}

	d := dispatched[0]
	if d.agentSlug != "lead-self-a" {
		t.Fatalf("dispatched to %q, want %q", d.agentSlug, "lead-self-a")
	}
	if d.structured == nil || d.structured.DeliveryText == "" {
		t.Fatal("dispatch has nil structured or empty DeliveryText")
	}

	env := parseAdditiveEnvelope(t, d.structured.DeliveryText)
	if env.Type != "message" {
		t.Errorf("type = %q, want %q", env.Type, "message")
	}

	// Single recipient: no "to" field.
	rawJSON := extractDEF169JSON(t, d.structured.DeliveryText)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("unmarshal raw JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for single leading-mention-is-default")
	}

	t.Logf("Test 4 (leading is default) — lead-self-a envelope: %+v", env)
}

// Test 5: Leading unresolved mention falls back to additive
// Topic default = agent-a, agent-b in project, NO "nonexistent" agent.
// Message = "@nonexistent hello @agent-b" (leading is unresolved).
// Additive: agent-a + agent-b dispatched.
func TestLeadingMention_UnresolvedFallsBackToAdditive(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("lead-unres-a"),
		ProjectID: proj.ID,
		Name:      "Default Agent A",
		Slug:      "lead-unres-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("lead-unres-b"),
		ProjectID: proj.ID,
		Name:      "Mentioned Agent B",
		Slug:      "lead-unres-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("lead-topic-unres")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "leading-unresolved",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agentA.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	// @nonexistent is not a known agent — it will be in mentionNames but not
	// in mentionedAgents (ResolveMentions returns not_found for it).
	// mentionedAgents[0] will be lead-unres-b, mentionNames[0] will be
	// "nonexistent", so EqualFold("lead-unres-b","nonexistent") == false
	// → isLeading = false → additive path.
	body := map[string]string{"content": "@nonexistent hello @lead-unres-b"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		slugs := make([]string, len(dispatched))
		for i, d := range dispatched {
			slugs[i] = d.agentSlug
		}
		t.Fatalf("expected 2 dispatches (additive fallback: agent-a + agent-b), got %d: %v", len(dispatched), slugs)
	}

	envelopes := collectEnvelopes(t, dispatched)

	envA, ok := envelopes["lead-unres-a"]
	if !ok {
		t.Fatal("no envelope found for lead-unres-a (default, additive fallback)")
	}
	if envA.Type != "message" {
		t.Errorf("lead-unres-a (default, primary): type = %q, want %q", envA.Type, "message")
	}

	envB, ok := envelopes["lead-unres-b"]
	if !ok {
		t.Fatal("no envelope found for lead-unres-b")
	}
	if envB.Type != "mention" {
		t.Errorf("lead-unres-b (secondary): type = %q, want %q", envB.Type, "mention")
	}

	wantTo := []string{"agent:lead-unres-a", "agent:lead-unres-b"}
	assertToEqual(t, "lead-unres-a", envA.To, wantTo)
	assertToEqual(t, "lead-unres-b", envB.To, wantTo)

	t.Logf("Test 5 (unresolved fallback) — lead-unres-a envelope: %+v", envA)
	t.Logf("Test 5 (unresolved fallback) — lead-unres-b envelope: %+v", envB)
}
