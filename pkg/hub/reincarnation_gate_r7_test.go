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

// Tests for the p2a-u1 review round-2 fixes (design agent-reincarnate §3.7,
// Amendment A25.7):
//   - R1 widened: a pkg/hub test for every remaining wired call site the
//     review found untested (m3, m4, m7, m8, m9). (m1, m2, m6 were already
//     covered by reincarnation_gate_r6_test.go; m5 and m12 are covered by
//     pkg/messaging's derive_key_participants_test.go, which can exercise
//     the sink directly without HTTP plumbing.)
//   - R2: the handleAgentMessage ownership check must verify the
//     authenticated principal's KIND, not just its ID, closing the phantom
//     "user:<agent-uuid>" participant row an agent sender could otherwise
//     write.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// R2: ownership check must verify KIND, not just ID
// ---------------------------------------------------------------------------

// TestHandleAgentMessage_A257_R2_AgentSenderPhantomUserSlotDenied is the
// brief's required repro: agent A sends dm:agent:Z:user:<A-uuid> (A's own
// agent ID placed in the "user" slot). Before R2, the ownership check
// compared only IDs, so this passed (the ID matches A's own authenticated
// ID) even though A is an agent, not a user — and the new A25.6 F1/F3
// registration would have written a phantom "user:<agent-uuid>" participant
// row naming a principal kind that was never actually a party. R2 requires
// the authenticated principal be an actual user for this check to succeed at
// all, so the request is denied and no conversation is ever created.
func TestHandleAgentMessage_A257_R2_AgentSenderPhantomUserSlotDenied(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	phantomThreadID := "dm:agent:" + target.ID + ":user:" + sender.ID

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "agent:" + sender.Slug, Recipient: "agent:" + target.Slug,
			Msg: "hello", Type: messages.TypeInstruction,
			ThreadID: phantomThreadID,
			Channel:  "web",
		},
	}
	req := reincarnateRequest(t, target.ID, agentIdentityFor(sender.ID, sender.ProjectID), body)
	rec := httptest.NewRecorder()
	srv.handleAgentMessage(rec, req, target.ID)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, dispatcher.calls, "a denied ownership check must not dispatch")

	conv, convErr := s.GetConversationByExternalRef(ctx, "native", phantomThreadID)
	assert.True(t, convErr != nil || conv == nil,
		"no conversation may be created for a phantom-kind key denied by the ownership check")
}

// TestHandleAgentMessage_A257_R2_UserSenderStillAllowed is the negative
// control: a genuine user sender using the same ThreadID shape (naming
// itself in the "user" slot) must be unaffected by the R2 tightening.
func TestHandleAgentMessage_A257_R2_UserSenderStillAllowed(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseRunning))
	srv.SetDispatcher(&brokerMockDispatcher{})

	threadID := "dm:agent:" + agentID + ":user:" + DevUserID
	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
			ThreadID: threadID,
			Channel:  "web",
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/message", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// R1 widened: m3 — outbound Rules 2/3 (resolveOutboundRouting)
// ---------------------------------------------------------------------------

// TestHandleAgentOutboundMessage_A257_R1_M3_RegistersBothParticipants covers
// handlers_agent_messaging.go's resolveOutboundRouting Rules 2/3 branch —
// named explicitly in the A25.6 contract — via the real HTTP handler an
// agent uses to message a human (no ThreadID, no conversation_ref: derives a
// direct DM from the sender/recipient principal pair).
func TestHandleAgentOutboundMessage_A257_R1_M3_RegistersBothParticipants(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("m3-p"), Slug: "m3-p", Name: "m3-p"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{ID: tid("m3-b"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	agent := &store.Agent{ID: tid("m3-agent"), Slug: "m3-agent", Name: "m3-agent", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, agent))
	user := &store.User{ID: tid("m3-user"), Email: "m3-user@test.com", DisplayName: "M3 User"}
	require.NoError(t, s.CreateUser(ctx, user))

	body := map[string]interface{}{
		"recipient": "user:" + user.Email,
		"msg":       "hello human",
		"type":      "input-needed",
	}
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/outbound-message", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), agentIdentityFor(agent.ID, agent.ProjectID)))

	rec := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.MessageID)

	msg, err := s.GetMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	require.NotEmpty(t, msg.ConversationID, "the outbound send must resolve a conversation")

	assertBothParticipants(t, s, msg.ConversationID, "agent", agent.ID, "user", user.ID)
}

// ---------------------------------------------------------------------------
// R1 widened: m4 — chat v2 sendAgentRouted primary (dm: route)
// ---------------------------------------------------------------------------

// TestChatV2_A257_R1_M4_SendAgentRouted_PrimaryRegistersDMParticipants is
// report-7-gteam-2a case (e), confirmed live in the UAT: a chat v2 1:1 DM
// send (POST /chat/conversations/dm:agent:<A>:user:<U>/messages) resolved
// through sendAgentRouted's primary conversation resolution.
func TestChatV2_A257_R1_M4_SendAgentRouted_PrimaryRegistersDMParticipants(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:        tid("m4-agent"),
		ProjectID: proj.ID,
		Name:      "M4 Bot",
		Slug:      "m4-bot",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	dmKey := "dm:agent:" + agent.ID + ":user:" + DevUserID
	// Pre-create the conversation WITHOUT participants — mirroring the exact
	// UAT defect: the row exists, but nothing has ever registered rows for
	// it. The send under test must self-heal this on resolve.
	setDMConversationID(t, s, dmKey, proj.ID)

	body := map[string]string{"content": "hi there"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	conv, err := s.GetConversationByExternalRef(ctx, "native", dmKey)
	require.NoError(t, err)
	assertBothParticipants(t, s, conv.ID, "agent", agent.ID, "user", DevUserID)
}

// ---------------------------------------------------------------------------
// R1 widened: m8 — chat v2 sendHumanToHuman (user-to-user dm: route)
// ---------------------------------------------------------------------------

// TestChatV2_A257_R1_M8_SendHumanToHuman_RegistersDMParticipants covers the
// sendHumanToHuman branch: a user-to-user DM route, which never touches an
// agent at all (F1's contract is not agent-specific — "both principals",
// whatever kind they are).
func TestChatV2_A257_R1_M8_SendHumanToHuman_RegistersDMParticipants(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	ctx := context.Background()

	peerID := tid("m8-peer-user")
	dmKey, err := messages.DMConversationKey("user", DevUserID, "user", peerID)
	require.NoError(t, err)
	setDMConversationID(t, s, dmKey, proj.ID)

	body := map[string]string{"content": "hey there"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+dmKey+"/messages", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	conv, err := s.GetConversationByExternalRef(ctx, "native", dmKey)
	require.NoError(t, err)
	assertBothParticipants(t, s, conv.ID, "user", DevUserID, "user", peerID)
}

// ---------------------------------------------------------------------------
// R1 widened: m7 / messagebroker.go:831 — deliverToUser / deliverToAgent
// ---------------------------------------------------------------------------

// TestDeliverToUser_A257_R1_M7_ThreadIDDM_RegistersBothParticipants covers
// messagebroker.go's deliverToUser thread branch (:503): a broker-delivered
// message (agent -> user reply) whose ThreadID happens to carry a dm:
// prefix resolves as kind=="direct".
func TestDeliverToUser_A257_R1_M7_ThreadIDDM_RegistersBothParticipants(t *testing.T) {
	s := newBrokerTestStore(t)
	ctx := context.Background()
	projectID := setupBrokerTestProject(t, s)
	agent := setupBrokerTestAgent(t, s, projectID, "m7-agent", "running")
	user := &store.User{ID: tid("m7-user"), Email: "m7-user@test.com", DisplayName: "M7 User"}
	require.NoError(t, s.CreateUser(ctx, user))

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	msg := messages.NewInstruction("agent:"+agent.Slug, "user:"+user.Email, "hello from agent")
	msg.SenderID = agent.ID
	msg.RecipientID = user.ID
	msg.ThreadID = dmKey

	proxy.deliverToUser(ctx, projectID, "m7-topic", msg)

	conv, err := s.GetConversationByExternalRef(ctx, "native", dmKey)
	require.NoError(t, err)
	assertBothParticipants(t, s, conv.ID, "agent", agent.ID, "user", user.ID)
}

// TestDeliverToAgent_A257_R1_MB831_ThreadIDDM_RegistersBothParticipants
// covers messagebroker.go's deliverToAgent thread branch (:831): a
// broker-delivered message (user -> agent) whose ThreadID carries a dm:
// prefix.
func TestDeliverToAgent_A257_R1_MB831_ThreadIDDM_RegistersBothParticipants(t *testing.T) {
	s := newBrokerTestStore(t)
	ctx := context.Background()
	projectID := setupBrokerTestProject(t, s)
	agent := setupBrokerTestAgent(t, s, projectID, "mb831-agent", "running")
	user := &store.User{ID: tid("mb831-user"), Email: "mb831-user@test.com", DisplayName: "MB831 User"}
	require.NoError(t, s.CreateUser(ctx, user))

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	msg := messages.NewInstruction("user:"+user.Email, "agent:"+agent.Slug, "hello agent")
	msg.SenderID = user.ID
	msg.RecipientID = agent.ID
	msg.ThreadID = dmKey

	proxy.deliverToAgent(ctx, projectID, agent.Slug, msg)

	conv, err := s.GetConversationByExternalRef(ctx, "native", dmKey)
	require.NoError(t, err)
	assertBothParticipants(t, s, conv.ID, "agent", agent.ID, "user", user.ID)
}

// ---------------------------------------------------------------------------
// R1 widened: m9 — broker-inbound resolvePhase5Conversation
// ---------------------------------------------------------------------------

// TestResolvePhase5Conversation_A257_R1_M9_ThreadIDDM_RegistersBothParticipants
// covers handlers_broker_inbound.go's resolvePhase5Conversation thread
// branch directly (the function is a private Server method, callable
// in-package without standing up a full broker-inbound HTTP request).
func TestResolvePhase5Conversation_A257_R1_M9_ThreadIDDM_RegistersBothParticipants(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("m9-p"), Slug: "m9-p", Name: "m9-p"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{ID: tid("m9-b"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	agent := &store.Agent{ID: tid("m9-agent"), Slug: "m9-agent", Name: "m9-agent", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, agent))
	user := &store.User{ID: tid("m9-user"), Email: "m9-user@test.com", DisplayName: "M9 User"}
	require.NoError(t, s.CreateUser(ctx, user))

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	convResult, err := srv.resolvePhase5Conversation(ctx, dmKey, project.ID, user.ID, agent.ID, "")
	require.NoError(t, err)
	require.NotNil(t, convResult)

	assertBothParticipants(t, s, convResult.ConversationID, "agent", agent.ID, "user", user.ID)
}
