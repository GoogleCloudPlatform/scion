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
	"fmt"
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
		if _, err := fakeStore.UpsertHubSetting(ctx, "messaging", val, "test", 0, "test"); err != nil {
			t.Fatalf("UpsertHubSetting: %v", err)
		}
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

	// --- Test 3: Hub OFF + human caller → allowed (human-agent DM preservation) ---
	// Human callers are never gated by Hub cross-project switch (design §7).
	// This test verifies a human-agent DM (user ↔ agentB in projB) remains
	// readable when the Hub is off. It does NOT test human observation of a
	// managed agent-agent DM — that is a different surface.
	clearHubSettings()
	humanIdent := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", "admin", "cli")
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

// ---------------------------------------------------------------------------
// CPM-UAT-004 follow-up: list, resolve, and route-path guard tests
// ---------------------------------------------------------------------------

// TestCrossProjectListGuard verifies that agent callers' conversation lists
// silently exclude cross-project DMs when the Hub CPM switch is disabled.
func TestCrossProjectListGuard(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// --- Setup: two projects, three agents ---
	projA := &store.Project{ID: tid("cpm-list-proj-a"), Slug: "list-proj-a", Name: "List Project A"}
	projB := &store.Project{ID: tid("cpm-list-proj-b"), Slug: "list-proj-b", Name: "List Project B"}
	for _, p := range []*store.Project{projA, projB} {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject %s: %v", p.Slug, err)
		}
	}

	agentA := &store.Agent{ID: tid("cpm-list-agent-a"), Slug: "list-agent-a", Name: "List Agent A", ProjectID: projA.ID, Phase: "running"}
	agentB := &store.Agent{ID: tid("cpm-list-agent-b"), Slug: "list-agent-b", Name: "List Agent B", ProjectID: projB.ID, Phase: "running"}
	agentA2 := &store.Agent{ID: tid("cpm-list-agent-a2"), Slug: "list-agent-a2", Name: "List Agent A2", ProjectID: projA.ID, Phase: "running"}
	for _, a := range []*store.Agent{agentA, agentB, agentA2} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}

	// Cross-project DM: agentA ↔ agentB.
	crossDMKey, _ := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	crossConv := &store.Conversation{
		ID: tid("cpm-list-cross-conv"), Kind: "direct", Surface: "native", ExternalRef: crossDMKey,
	}
	if err := s.CreateConversation(ctx, crossConv); err != nil {
		t.Fatalf("CreateConversation cross: %v", err)
	}
	// Add participant row so GetConversationsForPrincipal finds it.
	if err := s.AddParticipant(ctx, &store.ConversationParticipant{
		ID: tid("cpm-list-part-a-cross"), ConversationID: crossConv.ID,
		PrincipalKind: "agent", PrincipalID: agentA.ID, Role: "member", JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddParticipant cross: %v", err)
	}

	// Same-project DM: agentA ↔ agentA2.
	sameDMKey, _ := messages.DMConversationKey("agent", agentA.ID, "agent", agentA2.ID)
	sameConv := &store.Conversation{
		ID: tid("cpm-list-same-conv"), Kind: "direct", Surface: "native", ExternalRef: sameDMKey,
	}
	if err := s.CreateConversation(ctx, sameConv); err != nil {
		t.Fatalf("CreateConversation same: %v", err)
	}
	if err := s.AddParticipant(ctx, &store.ConversationParticipant{
		ID: tid("cpm-list-part-a-same"), ConversationID: sameConv.ID,
		PrincipalKind: "agent", PrincipalID: agentA.ID, Role: "member", JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AddParticipant same: %v", err)
	}

	agentAIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: agentA.ID}, ProjectID: projA.ID,
	}}

	// Helper to list conversations.
	listConvs := func() []conversationResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), agentAIdent))
		rr := httptest.NewRecorder()
		srv.handleListConversations(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("list: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var result conversationListResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatalf("unmarshal list: %v", err)
		}
		return result.Conversations
	}

	// Hub settings helpers.
	setHubCrossProject := func(enabled bool) {
		t.Helper()
		fakeStore := newFakeHubSettingStore()
		val, _ := json.Marshal(map[string]interface{}{"cross_project_messaging_enabled": enabled})
		if _, err := fakeStore.UpsertHubSetting(ctx, "messaging", val, "test", 0, "test"); err != nil {
			t.Fatalf("UpsertHubSetting: %v", err)
		}
		ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
		if _, err := ops.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		srv.SetOperationalSettings(ops)
	}
	clearHubSettings := func() {
		t.Helper()
		srv.operationalSettings.Store(nil)
	}

	// --- Test 1: Hub OFF → same-project visible, cross-project hidden ---
	clearHubSettings()
	convs := listConvs()
	foundCross, foundSame := false, false
	for _, c := range convs {
		if c.ID == crossConv.ID {
			foundCross = true
		}
		if c.ID == sameConv.ID {
			foundSame = true
		}
	}
	if foundCross {
		t.Error("Hub OFF: cross-project DM should NOT be in list")
	}
	if !foundSame {
		t.Error("Hub OFF: same-project DM should be in list")
	}

	// --- Test 2: Hub ON → both visible ---
	setHubCrossProject(true)
	convs = listConvs()
	foundCross, foundSame = false, false
	for _, c := range convs {
		if c.ID == crossConv.ID {
			foundCross = true
		}
		if c.ID == sameConv.ID {
			foundSame = true
		}
	}
	if !foundCross {
		t.Error("Hub ON: cross-project DM should be in list")
	}
	if !foundSame {
		t.Error("Hub ON: same-project DM should be in list")
	}
}

// TestCrossProjectResolveGuard verifies that handleConversationResolve denies
// agent callers from resolving cross-project conv: references when Hub CPM is off.
func TestCrossProjectResolveGuard(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projA := &store.Project{ID: tid("cpm-resolve-proj-a"), Slug: "resolve-proj-a", Name: "Resolve A"}
	projB := &store.Project{ID: tid("cpm-resolve-proj-b"), Slug: "resolve-proj-b", Name: "Resolve B"}
	for _, p := range []*store.Project{projA, projB} {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject %s: %v", p.Slug, err)
		}
	}

	agentA := &store.Agent{ID: tid("cpm-resolve-agent-a"), Slug: "resolve-agent-a", Name: "Resolve A", ProjectID: projA.ID, Phase: "running"}
	agentB := &store.Agent{ID: tid("cpm-resolve-agent-b"), Slug: "resolve-agent-b", Name: "Resolve B", ProjectID: projB.ID, Phase: "running"}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}

	crossDMKey, _ := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	crossConv := &store.Conversation{
		ID: tid("cpm-resolve-cross-conv"), Kind: "direct", Surface: "native", ExternalRef: crossDMKey,
	}
	if err := s.CreateConversation(ctx, crossConv); err != nil {
		t.Fatalf("CreateConversation cross: %v", err)
	}

	agentAIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: agentA.ID}, ProjectID: projA.ID,
	}}

	setHubCrossProject := func(enabled bool) {
		t.Helper()
		fakeStore := newFakeHubSettingStore()
		val, _ := json.Marshal(map[string]interface{}{"cross_project_messaging_enabled": enabled})
		if _, err := fakeStore.UpsertHubSetting(ctx, "messaging", val, "test", 0, "test"); err != nil {
			t.Fatalf("UpsertHubSetting: %v", err)
		}
		ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
		if _, err := ops.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		srv.SetOperationalSettings(ops)
	}
	clearHubSettings := func() {
		t.Helper()
		srv.operationalSettings.Store(nil)
	}

	resolveConv := func() *httptest.ResponseRecorder {
		t.Helper()
		ref := fmt.Sprintf("conv:%s", crossConv.ID)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/resolve?reference="+ref, nil)
		req = req.WithContext(contextWithIdentity(req.Context(), agentAIdent))
		rr := httptest.NewRecorder()
		srv.handleConversationResolve(rr, req)
		return rr
	}

	// --- Test 1: Hub OFF → resolve denied (403) ---
	clearHubSettings()
	rr := resolveConv()
	if rr.Code != http.StatusForbidden {
		t.Errorf("Hub OFF resolve: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// --- Test 2: Hub ON → resolve allowed ---
	setHubCrossProject(true)
	rr = resolveConv()
	if rr.Code != http.StatusOK {
		t.Errorf("Hub ON resolve: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var result conversationResolveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal resolve: %v", err)
	}
	if !result.Exists {
		t.Error("Hub ON resolve: expected exists=true")
	}
}

// TestCrossProjectRoutePathGuard verifies that the Hub-off guard fires on the
// actual routed handler paths (handleConversationRoutes), not just when calling
// handler functions directly.
func TestCrossProjectRoutePathGuard(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projA := &store.Project{ID: tid("cpm-route-proj-a"), Slug: "route-proj-a", Name: "Route A"}
	projB := &store.Project{ID: tid("cpm-route-proj-b"), Slug: "route-proj-b", Name: "Route B"}
	for _, p := range []*store.Project{projA, projB} {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject %s: %v", p.Slug, err)
		}
	}

	agentA := &store.Agent{ID: tid("cpm-route-agent-a"), Slug: "route-agent-a", Name: "Route A", ProjectID: projA.ID, Phase: "running"}
	agentB := &store.Agent{ID: tid("cpm-route-agent-b"), Slug: "route-agent-b", Name: "Route B", ProjectID: projB.ID, Phase: "running"}
	for _, a := range []*store.Agent{agentA, agentB} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}

	crossDMKey, _ := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	crossConv := &store.Conversation{
		ID: tid("cpm-route-cross-conv"), Kind: "direct", Surface: "native", ExternalRef: crossDMKey,
	}
	if err := s.CreateConversation(ctx, crossConv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Create a message for the single-message test.
	crossMsg := &store.Message{
		ID:             tid("cpm-route-cross-msg"),
		ProjectID:      projA.ID,
		ConversationID: crossConv.ID,
		Sender:         "agent:route-agent-b",
		SenderID:       agentB.ID,
		Recipient:      "agent:route-agent-a",
		RecipientID:    agentA.ID,
		Msg:            "routed cross-project message",
		Type:           "agent_message",
		DispatchState:  "dispatched",
		CreatedAt:      time.Now().Add(-1 * time.Minute),
	}
	if err := s.CreateMessage(ctx, crossMsg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	agentAIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: agentA.ID}, ProjectID: projA.ID,
	}}

	// Clear Hub settings → Hub OFF.
	srv.operationalSettings.Store(nil)

	// --- Test 1: GET /api/v1/conversations/{id} via handleConversationRoutes → 403 ---
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+crossConv.ID, nil)
	req1 = req1.WithContext(contextWithIdentity(req1.Context(), agentAIdent))
	rr1 := httptest.NewRecorder()
	srv.handleConversationRoutes(rr1, req1)
	if rr1.Code != http.StatusForbidden {
		t.Errorf("route GET conversation detail: expected 403, got %d: %s", rr1.Code, rr1.Body.String())
	}

	// --- Test 2: GET /api/v1/conversations/{id}/messages via handleConversationRoutes → 403 ---
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+crossConv.ID+"/messages", nil)
	req2 = req2.WithContext(contextWithIdentity(req2.Context(), agentAIdent))
	rr2 := httptest.NewRecorder()
	srv.handleConversationRoutes(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Errorf("route GET conversation messages: expected 403, got %d: %s", rr2.Code, rr2.Body.String())
	}

	// --- Test 3: GET /api/v1/conversations/{id}/messages/{msgID} via handleConversationRoutes → 403 ---
	req3 := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/conversations/%s/messages/%s", crossConv.ID, crossMsg.ID), nil)
	req3 = req3.WithContext(contextWithIdentity(req3.Context(), agentAIdent))
	rr3 := httptest.NewRecorder()
	srv.handleConversationRoutes(rr3, req3)
	if rr3.Code != http.StatusForbidden {
		t.Errorf("route GET single message: expected 403, got %d: %s", rr3.Code, rr3.Body.String())
	}
}
