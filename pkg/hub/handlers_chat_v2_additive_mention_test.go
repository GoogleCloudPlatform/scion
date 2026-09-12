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

// Additive mention routing tests: verifies the additive model where the
// primary agent (thread default, DM implicit, or first-mentioned) always
// gets type:"message" and secondaries get type:"mention", with a shared
// "to" field listing all engaged agents.

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// additiveEnvelope is a minimal struct for decoding the rendered envelope.
type additiveEnvelope struct {
	Type string   `json:"type"`
	To   []string `json:"to,omitempty"`
	From string   `json:"from"`
	Msg  string   `json:"msg"`
}

// parseAdditiveEnvelope extracts and decodes the JSON envelope from DeliveryText.
func parseAdditiveEnvelope(t *testing.T, deliveryText string) additiveEnvelope {
	t.Helper()
	jsonStr := extractDEF169JSON(t, deliveryText) // reuse the existing helper
	var env additiveEnvelope
	if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
		t.Fatalf("failed to unmarshal envelope JSON: %v\nJSON: %s", err, jsonStr)
	}
	return env
}

// collectEnvelopes extracts agent slug -> envelope from dispatched messages.
func collectEnvelopes(t *testing.T, dispatched []brokerDispatchedMsg) map[string]additiveEnvelope {
	t.Helper()
	envelopes := make(map[string]additiveEnvelope)
	for _, d := range dispatched {
		if d.structured == nil {
			t.Errorf("dispatch to %q has nil structured message", d.agentSlug)
			continue
		}
		if d.structured.DeliveryText == "" {
			t.Fatalf("dispatch to %q has empty DeliveryText — writeDenyEnabled() path not reached", d.agentSlug)
		}
		envelopes[d.agentSlug] = parseAdditiveEnvelope(t, d.structured.DeliveryText)
	}
	return envelopes
}

// sortedTo returns a sorted copy of the "to" array.
func sortedTo(to []string) []string {
	c := make([]string, len(to))
	copy(c, to)
	sort.Strings(c)
	return c
}

// assertToEqual checks that two "to" arrays contain the same elements.
func assertToEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	gs := sortedTo(got)
	ws := sortedTo(want)
	if len(gs) != len(ws) {
		t.Errorf("%s: to length = %d, want %d; got %v, want %v", label, len(gs), len(ws), gs, ws)
		return
	}
	for i := range ws {
		if gs[i] != ws[i] {
			t.Errorf("%s: to[%d] = %q, want %q", label, i, gs[i], ws[i])
		}
	}
}

// Case 1: Default + mention
// Topic has default agent-a. Message mentions agent-b.
// agent-a dispatched with type:"message", to:["agent:agent-a","agent:agent-b"]
// agent-b dispatched with type:"mention", to:["agent:agent-a","agent:agent-b"]
func TestAdditiveMention_DefaultPlusMention(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("add-default-a"),
		ProjectID: proj.ID,
		Name:      "Default Agent A",
		Slug:      "default-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("add-mention-b"),
		ProjectID: proj.ID,
		Name:      "Mentioned Agent B",
		Slug:      "mention-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("add-topic-default-mention")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "additive-default-mention",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agentA.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "please help @mention-b"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatch calls, got %d", len(dispatched))
	}

	envelopes := collectEnvelopes(t, dispatched)

	// agent-a (default, primary) must get type:"message"
	envA, ok := envelopes["default-a"]
	if !ok {
		t.Fatal("no envelope found for default-a")
	}
	if envA.Type != "message" {
		t.Errorf("default-a (primary): type = %q, want %q", envA.Type, "message")
	}

	// agent-b (mentioned, secondary) must get type:"mention"
	envB, ok := envelopes["mention-b"]
	if !ok {
		t.Fatal("no envelope found for mention-b")
	}
	if envB.Type != "mention" {
		t.Errorf("mention-b (secondary): type = %q, want %q", envB.Type, "mention")
	}

	// Both must have to:["agent:default-a","agent:mention-b"]
	wantTo := []string{"agent:default-a", "agent:mention-b"}
	assertToEqual(t, "default-a", envA.To, wantTo)
	assertToEqual(t, "mention-b", envB.To, wantTo)

	t.Logf("Case 1 — default-a envelope: %+v", envA)
	t.Logf("Case 1 — mention-b envelope: %+v", envB)
}

// Case 2: Multi-mention, no default
// Message mentions agent-a and agent-b, no default agent.
// agent-a (first-mentioned) dispatched with type:"message", to:["agent:agent-a","agent:agent-b"]
// agent-b dispatched with type:"mention", same to
func TestAdditiveMention_MultiMentionNoDefault(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("add-multi-a"),
		ProjectID: proj.ID,
		Name:      "Multi Agent A",
		Slug:      "multi-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("add-multi-b"),
		ProjectID: proj.ID,
		Name:      "Multi Agent B",
		Slug:      "multi-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("add-topic-multi-nodefault")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "additive-multi-nodefault",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "@multi-a deploy then @multi-b test"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatch calls, got %d", len(dispatched))
	}

	envelopes := collectEnvelopes(t, dispatched)

	envA, ok := envelopes["multi-a"]
	if !ok {
		t.Fatal("no envelope found for multi-a")
	}
	if envA.Type != "message" {
		t.Errorf("multi-a (primary, first-mentioned): type = %q, want %q", envA.Type, "message")
	}

	envB, ok := envelopes["multi-b"]
	if !ok {
		t.Fatal("no envelope found for multi-b")
	}
	if envB.Type != "mention" {
		t.Errorf("multi-b (secondary): type = %q, want %q", envB.Type, "mention")
	}

	wantTo := []string{"agent:multi-a", "agent:multi-b"}
	assertToEqual(t, "multi-a", envA.To, wantTo)
	assertToEqual(t, "multi-b", envB.To, wantTo)

	t.Logf("Case 2 — multi-a envelope: %+v", envA)
	t.Logf("Case 2 — multi-b envelope: %+v", envB)
}

// Case 3: Default agent also mentioned
// Topic default is agent-a. Message mentions @agent-a and @agent-b.
// agent-a dispatched ONCE (not twice), type:"message"
// agent-b dispatched with type:"mention"
// to = ["agent:agent-a","agent:agent-b"] for both
func TestAdditiveMention_DefaultAlsoMentioned(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("add-dedup-a"),
		ProjectID: proj.ID,
		Name:      "Dedup Agent A",
		Slug:      "dedup-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("add-dedup-b"),
		ProjectID: proj.ID,
		Name:      "Dedup Agent B",
		Slug:      "dedup-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	topicID := tid("add-topic-dedup")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "additive-dedup",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agentA.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	// Mention BOTH agents, including the default.
	body := map[string]string{"content": "@dedup-a and @dedup-b please coordinate"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+topicID+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	// agent-a must be dispatched ONCE (dedup), agent-b once = total 2
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatch calls (dedup), got %d", len(dispatched))
	}

	// Count per-slug dispatches to verify no double dispatch.
	slugCounts := make(map[string]int)
	for _, d := range dispatched {
		slugCounts[d.agentSlug]++
	}
	if slugCounts["dedup-a"] != 1 {
		t.Errorf("dedup-a dispatched %d times, want 1 (dedup failed)", slugCounts["dedup-a"])
	}
	if slugCounts["dedup-b"] != 1 {
		t.Errorf("dedup-b dispatched %d times, want 1", slugCounts["dedup-b"])
	}

	envelopes := collectEnvelopes(t, dispatched)

	envA := envelopes["dedup-a"]
	if envA.Type != "message" {
		t.Errorf("dedup-a (primary): type = %q, want %q", envA.Type, "message")
	}

	envB := envelopes["dedup-b"]
	if envB.Type != "mention" {
		t.Errorf("dedup-b (secondary): type = %q, want %q", envB.Type, "mention")
	}

	wantTo := []string{"agent:dedup-a", "agent:dedup-b"}
	assertToEqual(t, "dedup-a", envA.To, wantTo)
	assertToEqual(t, "dedup-b", envB.To, wantTo)

	t.Logf("Case 3 — dedup-a envelope: %+v", envA)
	t.Logf("Case 3 — dedup-b envelope: %+v", envB)
	t.Logf("Case 3 — slug dispatch counts: %v", slugCounts)
}

// Case 4: Single mention, no default
// Message mentions only agent-a, no default.
// agent-a dispatched with type:"message", NO "to" field.
func TestAdditiveMention_SingleMentionNoDefault(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := &store.Agent{
		ID:        tid("add-single-a"),
		ProjectID: proj.ID,
		Name:      "Single Agent",
		Slug:      "single-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	topicID := tid("add-topic-single")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:        topicID,
		ProjectID: proj.ID,
		Name:      "additive-single",
		CreatedBy: "dev",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "@single-a do it"}
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

	env := parseAdditiveEnvelope(t, d.structured.DeliveryText)
	if env.Type != "message" {
		t.Errorf("type = %q, want %q (sole recipient is primary)", env.Type, "message")
	}
	if len(env.To) != 0 {
		t.Errorf("to = %v, want empty (single-recipient, no secondaries)", env.To)
	}

	// Verify via raw JSON that "to" key is absent.
	rawJSON := extractDEF169JSON(t, d.structured.DeliveryText)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("unmarshal raw JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for single-mention-no-default routing")
	}

	t.Logf("Case 4 — single-a envelope: %+v", env)
}

// Case 5: Default only, no mentions (regression)
// Unchanged from current behavior. type:"message", no "to", single dispatch.
func TestAdditiveMention_DefaultOnlyNoMention(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agent := &store.Agent{
		ID:        tid("add-defonly-a"),
		ProjectID: proj.ID,
		Name:      "Default Only Agent",
		Slug:      "defonly-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	topicID := tid("add-topic-defonly")
	if err := wcs.CreateTopic(ctx, WebChatTopic{
		ID:           topicID,
		ProjectID:    proj.ID,
		Name:         "additive-defonly",
		CreatedBy:    "dev",
		CreatedAt:    time.Now().UTC(),
		DefaultAgent: agent.Slug,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)

	body := map[string]string{"content": "hello default agent, no mention"}
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

	env := parseAdditiveEnvelope(t, d.structured.DeliveryText)
	if env.Type != "message" {
		t.Errorf("type = %q, want %q (default-agent, no mention)", env.Type, "message")
	}
	if len(env.To) != 0 {
		t.Errorf("to = %v, want empty (single-recipient default-agent)", env.To)
	}

	// Also verify "to" key is absent in JSON.
	rawJSON := extractDEF169JSON(t, d.structured.DeliveryText)
	var raw map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		t.Fatalf("unmarshal raw JSON: %v", err)
	}
	if _, ok := raw["to"]; ok {
		t.Error("JSON contains 'to' key; want absent for default-agent-only routing")
	}

	t.Logf("Case 5 — defonly-a envelope: %+v", env)
}

// Case 6: DM + mention
// In a DM with agent-a, mention @agent-b.
// agent-a (DM implicit) dispatched with type:"message", to:["agent:agent-a","agent:agent-b"]
// agent-b dispatched with type:"mention", same to
func TestAdditiveMention_DMPlusMention(t *testing.T) {
	srv, s, wcs, proj, _ := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()
	_ = wcs // not directly used but setupSendTest attaches it

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	agentA := &store.Agent{
		ID:        tid("add-dm-a"),
		ProjectID: proj.ID,
		Name:      "DM Agent A",
		Slug:      "dm-agent-a",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	agentB := &store.Agent{
		ID:        tid("add-dm-mention-b"),
		ProjectID: proj.ID,
		Name:      "DM Mention B",
		Slug:      "dm-mention-b",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent(%s): %v", a.Slug, err)
		}
	}

	// DM key: dm:agent:<agentA-UUID>:user:<DevUserID>
	dmKey := "dm:agent:" + agentA.ID + ":user:" + DevUserID
	setDMConversationID(t, s, dmKey, proj.ID)

	body := map[string]string{"content": "help with this @dm-mention-b"}
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+dmKey+"/messages", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	dispatched := dispatcher.getMessages()
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatch calls, got %d", len(dispatched))
	}

	envelopes := collectEnvelopes(t, dispatched)

	envA, ok := envelopes["dm-agent-a"]
	if !ok {
		t.Fatal("no envelope found for dm-agent-a")
	}
	if envA.Type != "message" {
		t.Errorf("dm-agent-a (DM implicit, primary): type = %q, want %q", envA.Type, "message")
	}

	envB, ok := envelopes["dm-mention-b"]
	if !ok {
		t.Fatal("no envelope found for dm-mention-b")
	}
	if envB.Type != "mention" {
		t.Errorf("dm-mention-b (secondary): type = %q, want %q", envB.Type, "mention")
	}

	wantTo := []string{"agent:dm-agent-a", "agent:dm-mention-b"}
	assertToEqual(t, "dm-agent-a", envA.To, wantTo)
	assertToEqual(t, "dm-mention-b", envB.To, wantTo)

	t.Logf("Case 6 — dm-agent-a envelope: %+v", envA)
	t.Logf("Case 6 — dm-mention-b envelope: %+v", envB)
}
