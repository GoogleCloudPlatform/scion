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
	"strings"
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

// ---------------------------------------------------------------------------
// CPM-UAT-005: Conversation send dispatches to broker and uses canonical peer
// ---------------------------------------------------------------------------

// TestConversationSendDispatch verifies that handleCPMConversationSend:
//  1. Dispatches the message to the broker (not just persists it).
//  2. Derives the peer from the canonical ExternalRef, not participant rows.
//  3. Uses the agent slug format ("agent:<slug>") for sender, not UUID.
//  4. Sets DispatchState to dispatched.
//  5. A forged third participant does not redirect delivery.
func TestConversationSendDispatch(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Set up a mock dispatcher that records calls.
	spy := &brokerMockDispatcher{}
	srv.SetDispatcher(spy)

	// --- Setup: project with two agents ---
	proj := &store.Project{
		ID:   tid("cpm-send-proj"),
		Slug: "send-proj",
		Name: "Send Test Project",
	}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Sender agent (caller).
	senderAgent := &store.Agent{
		ID:          tid("cpm-send-sender"),
		Slug:        "send-sender",
		Name:        "Send Sender Agent",
		ProjectID:   proj.ID,
		Phase:       "running",
		MessageMode: store.MessageModeProject,
	}
	// Target agent (peer to receive the message).
	targetAgent := &store.Agent{
		ID:              tid("cpm-send-target"),
		Slug:            "send-target",
		Name:            "Send Target Agent",
		ProjectID:       proj.ID,
		Phase:           "running",
		MessageMode:     store.MessageModeProject,
		RuntimeBrokerID: "test-broker-001",
	}
	for _, a := range []*store.Agent{senderAgent, targetAgent} {
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}

	// Create the canonical DM conversation.
	dmKey, err := messages.DMConversationKey("agent", senderAgent.ID, "agent", targetAgent.ID)
	if err != nil {
		t.Fatalf("DMConversationKey: %v", err)
	}
	conv := &store.Conversation{
		ID:          tid("cpm-send-conv"),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
	}
	if err := s.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Build an agent identity for the sender.
	senderIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: senderAgent.ID},
		ProjectID: proj.ID,
	}}

	// --- Test 1: Message is dispatched to broker ---
	body := `{"msg":"CPM-UAT-005-dispatch-test","type":"instruction"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/messages",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), senderIdent))
	rr := httptest.NewRecorder()

	srv.handleCPMConversationSend(rr, req, conv.ID)

	if rr.Code != http.StatusOK {
		t.Fatalf("dispatch test: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Parse response.
	var resp conversationSendResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Status != "delivered" {
		t.Errorf("expected status 'delivered', got %q", resp.Status)
	}
	if resp.MessageID == "" {
		t.Error("expected non-empty messageId")
	}

	// Verify dispatcher was called with the target agent.
	spy.mu.Lock()
	dispatched := make([]brokerDispatchedMsg, len(spy.messages))
	copy(dispatched, spy.messages)
	spy.mu.Unlock()

	if len(dispatched) == 0 {
		t.Fatal("no dispatch calls recorded — message was stored but not dispatched (CPM-UAT-005 regression)")
	}
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", len(dispatched))
	}
	d := dispatched[0]
	if d.agentSlug != targetAgent.Slug {
		t.Errorf("dispatched to %q, expected %q", d.agentSlug, targetAgent.Slug)
	}
	if d.msg != "CPM-UAT-005-dispatch-test" {
		t.Errorf("dispatched msg %q, expected %q", d.msg, "CPM-UAT-005-dispatch-test")
	}

	// --- Test 2: Sender uses slug format, not UUID ---
	if d.structured == nil {
		t.Fatal("structured message is nil")
	}
	if d.structured.Sender != "agent:send-sender" {
		t.Errorf("sender = %q, expected 'agent:send-sender' (slug format)", d.structured.Sender)
	}
	if d.structured.Recipient != "agent:send-target" {
		t.Errorf("recipient = %q, expected 'agent:send-target'", d.structured.Recipient)
	}

	// --- Test 3: Verify persisted message has DispatchState set ---
	persistedMsg, err := s.GetMessage(ctx, resp.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if persistedMsg.DispatchState != store.MessageDispatchDispatched {
		t.Errorf("persisted DispatchState = %q, expected %q",
			persistedMsg.DispatchState, store.MessageDispatchDispatched)
	}
	// Also verify sender on persisted record.
	if persistedMsg.Sender != "agent:send-sender" {
		t.Errorf("persisted Sender = %q, expected 'agent:send-sender'", persistedMsg.Sender)
	}

	// --- Test 4: Canonical peer derivation from ExternalRef ---
	// Verify derivePeerFromExternalRef extracts the correct peer from the
	// immutable DM key. The store's DM participant guard (checkDMParticipantKey)
	// already prevents adding forged third-party participants to DM conversations
	// at the persistence layer — this test verifies the application-level
	// derivation also uses ExternalRef, not participant rows.
	peerKind, peerID := derivePeerFromExternalRef(dmKey, "agent", senderAgent.ID)
	if peerKind != "agent" || peerID != targetAgent.ID {
		t.Errorf("derivePeerFromExternalRef: got (%q, %q), expected ('agent', %q)",
			peerKind, peerID, targetAgent.ID)
	}
	// Reverse direction: target calling → sender is peer.
	peerKind2, peerID2 := derivePeerFromExternalRef(dmKey, "agent", targetAgent.ID)
	if peerKind2 != "agent" || peerID2 != senderAgent.ID {
		t.Errorf("derivePeerFromExternalRef reverse: got (%q, %q), expected ('agent', %q)",
			peerKind2, peerID2, senderAgent.ID)
	}
	// Non-participant → empty (fail closed).
	peerKind3, peerID3 := derivePeerFromExternalRef(dmKey, "agent", "not-in-key")
	if peerKind3 != "" || peerID3 != "" {
		t.Errorf("derivePeerFromExternalRef non-participant: got (%q, %q), expected empty",
			peerKind3, peerID3)
	}

	// --- Test 5: No dispatcher → status pending (not delivered) ---
	srv.SetDispatcher(nil)
	body3 := `{"msg":"CPM-UAT-005-no-dispatch-test","type":"instruction"}`
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/messages",
		strings.NewReader(body3))
	req3.Header.Set("Content-Type", "application/json")
	req3 = req3.WithContext(contextWithIdentity(req3.Context(), senderIdent))
	rr3 := httptest.NewRecorder()

	srv.handleCPMConversationSend(rr3, req3, conv.ID)

	if rr3.Code != http.StatusOK {
		t.Fatalf("no-dispatcher test: expected 200, got %d: %s", rr3.Code, rr3.Body.String())
	}
	var resp3 conversationSendResponse
	if err := json.Unmarshal(rr3.Body.Bytes(), &resp3); err != nil {
		t.Fatalf("unmarshal response3: %v", err)
	}
	if resp3.Status != "pending" {
		t.Errorf("no-dispatcher: expected status 'pending', got %q", resp3.Status)
	}
}
