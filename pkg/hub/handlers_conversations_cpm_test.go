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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// CPM-UAT-004: Hub-off read guard for agent cross-project conversations
// ---------------------------------------------------------------------------

// TestCrossProjectConversationReadGate verifies that agent callers cannot
// read cross-project conversation messages when the Hub cross-project
// messaging switch is disabled (design §7).
func TestCrossProjectConversationReadGate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// --- Setup: two projects, two agents, one per project ---
	projA := &store.Project{
		ID:   tid("cpm-gate-proj-a"),
		Slug: "gate-proj-a",
		Name: "Gate Project A",
	}
	projB := &store.Project{
		ID:   tid("cpm-gate-proj-b"),
		Slug: "gate-proj-b",
		Name: "Gate Project B",
	}
	for _, p := range []*store.Project{projA, projB} {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject %s: %v", p.Slug, err)
		}
	}

	agentA := &store.Agent{
		ID:        tid("cpm-gate-agent-a"),
		Slug:      "gate-agent-a",
		Name:      "Gate Agent A",
		ProjectID: projA.ID,
		Phase:     "running",
	}
	agentB := &store.Agent{
		ID:        tid("cpm-gate-agent-b"),
		Slug:      "gate-agent-b",
		Name:      "Gate Agent B",
		ProjectID: projB.ID,
		Phase:     "running",
	}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}

	// Cross-project DM between agentA and agentB.
	crossDMKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	crossConv := &store.Conversation{
		ID:          tid("cpm-gate-cross-conv"),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: crossDMKey,
	}
	if err := s.CreateConversation(ctx, crossConv); err != nil {
		t.Fatalf("CreateConversation cross: %v", err)
	}

	// Same-project DM between agentA and another agent in the same project.
	agentA2 := &store.Agent{
		ID:        tid("cpm-gate-agent-a2"),
		Slug:      "gate-agent-a2",
		Name:      "Gate Agent A2",
		ProjectID: projA.ID,
		Phase:     "running",
	}
	if err := s.CreateAgent(ctx, agentA2); err != nil {
		t.Fatalf("CreateAgent %s: %v", agentA2.Slug, err)
	}
	sameDMKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentA2.ID)
	if err != nil {
		t.Fatalf("DMConversationKey same: %v", err)
	}
	sameConv := &store.Conversation{
		ID:          tid("cpm-gate-same-conv"),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: sameDMKey,
	}
	if err := s.CreateConversation(ctx, sameConv); err != nil {
		t.Fatalf("CreateConversation same: %v", err)
	}

	// Create a message in each conversation.
	crossMsg := &store.Message{
		ID:             tid("cpm-gate-cross-msg"),
		ProjectID:      projA.ID,
		ConversationID: crossConv.ID,
		Sender:         "agent:gate-agent-b",
		SenderID:       agentB.ID,
		Recipient:      "agent:gate-agent-a",
		RecipientID:    agentA.ID,
		Msg:            "cross-project message",
		Type:           "agent_message",
		DispatchState:  "dispatched",
		CreatedAt:      time.Now().Add(-1 * time.Minute),
	}
	sameMsg := &store.Message{
		ID:             tid("cpm-gate-same-msg"),
		ProjectID:      projA.ID,
		ConversationID: sameConv.ID,
		Sender:         "agent:gate-agent-a2",
		SenderID:       agentA2.ID,
		Recipient:      "agent:gate-agent-a",
		RecipientID:    agentA.ID,
		Msg:            "same-project message",
		Type:           "agent_message",
		DispatchState:  "dispatched",
		CreatedAt:      time.Now().Add(-1 * time.Minute),
	}
	for _, m := range []*store.Message{crossMsg, sameMsg} {
		if err := s.CreateMessage(ctx, m); err != nil {
			t.Fatalf("CreateMessage %s: %v", m.ID, err)
		}
	}

	// Agent identity for agentA (in projA).
	agentAIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentA.ID},
		ProjectID: projA.ID,
	}}

	// --- Helper: make a request as the agent ---
	agentRequest := func(method, convID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1/conversations/"+convID+"/messages", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), agentAIdent))
		rr := httptest.NewRecorder()
		srv.handleConvListMessages(rr, req, convID)
		return rr
	}

	// --- Helper: toggle Hub cross-project messaging ---
	setHubCrossProject := func(enabled bool) {
		t.Helper()
		fakeStore := newFakeHubSettingStore()
		val, _ := json.Marshal(map[string]interface{}{
			"cross_project_messaging_enabled": enabled,
		})
		fakeStore.UpsertHubSetting(ctx, "messaging", val, "test", 0, "test")
		ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
		if _, err := ops.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		srv.SetOperationalSettings(ops)
	}

	clearHubSettings := func() {
		t.Helper()
		// No operational settings = Hub off (fail-closed).
		srv.operationalSettings.Store(nil)
	}

	// --- Test 1: Hub OFF + agent caller → cross-project read denied ---
	clearHubSettings()
	rr := agentRequest(http.MethodGet, crossConv.ID)
	if rr.Code != http.StatusForbidden {
		t.Errorf("Hub OFF + agent + cross-project: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// --- Test 2: Hub ON + agent caller → cross-project read allowed ---
	setHubCrossProject(true)
	rr = agentRequest(http.MethodGet, crossConv.ID)
	if rr.Code != http.StatusOK {
		t.Errorf("Hub ON + agent + cross-project: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// --- Test 3: Hub OFF + human caller → allowed (authorized audit) ---
	clearHubSettings()
	humanIdent := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", "admin", "cli")
	// Use human DM key for a cross-project conv that the human can read.
	// For this test we re-use the cross-project conv and add a human participant via
	// a direct call to the handler — but authorizeDMRead would reject a human
	// not named in the DM key. Instead, create a human-agent DM where the
	// peer agent is in a different project.
	humanAgentDMKey, err := messages.DMConversationKey("agent", agentB.ID, "user", DevUserID)
	if err != nil {
		t.Fatalf("DMConversationKey human: %v", err)
	}
	humanCrossConv := &store.Conversation{
		ID:          tid("cpm-gate-human-cross-conv"),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: humanAgentDMKey,
	}
	if err := s.CreateConversation(ctx, humanCrossConv); err != nil {
		t.Fatalf("CreateConversation human-cross: %v", err)
	}
	humanReq := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+humanCrossConv.ID+"/messages", nil)
	humanReq = humanReq.WithContext(contextWithIdentity(humanReq.Context(), humanIdent))
	rr = httptest.NewRecorder()
	srv.handleConvListMessages(rr, humanReq, humanCrossConv.ID)
	if rr.Code != http.StatusOK {
		t.Errorf("Hub OFF + human + cross-project: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// --- Test 4: Hub OFF + agent caller → same-project read allowed ---
	clearHubSettings()
	rr = agentRequest(http.MethodGet, sameConv.ID)
	if rr.Code != http.StatusOK {
		t.Errorf("Hub OFF + agent + same-project: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
