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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test 1: CLI-shaped outbound request through real HTTP handler
// ---------------------------------------------------------------------------

// TestContract_DM_CLIRequestThroughHandler verifies that a CLI-shaped
// outbound message (Recipient, Msg, Type:"instruction") posts successfully
// through the real outbound handler and produces a persisted message with a
// non-empty ConversationID.
func TestContract_DM_CLIRequestThroughHandler(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Set a recording dispatcher so the handler doesn't fail with 503.
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Construct CLI-shaped request: Recipient, Msg, Type:"instruction".
	// See cmd/message.go:653 for the CLI fields.
	reqBody, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "CLI test message",
		Type:      "instruction",
	})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)

	// Assertions: HTTP 200.
	require.Equal(t, http.StatusOK, rr.Code, "expected 200 OK; body: %s", rr.Body.String())

	// Message persisted with non-empty ConversationID.
	// Query by agent to find the message we just created.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items, "at least one message must be persisted")

	// Find the message we just sent (most recent).
	var msg *store.Message
	for i := range msgs.Items {
		if msgs.Items[i].Msg == "CLI test message" {
			msg = &msgs.Items[i]
			break
		}
	}
	require.NotNil(t, msg, "message must be found")
	require.NotEmpty(t, msg.ConversationID, "message must have ConversationID set")

	// Verify dispatcher was called (or not, for user messages).
	dispatcher.mu.Lock()
	callCount := len(dispatcher.calls)
	dispatcher.mu.Unlock()
	require.Equal(t, 0, callCount, "user messages do not dispatch to agents")

	// DM conversation created with correct key format (dm:...).
	conv, err := s.GetConversation(ctx, msg.ConversationID)
	require.NoError(t, err)
	require.Equal(t, "direct", conv.Kind)
	require.Contains(t, conv.ExternalRef, "dm:", "DM key must start with dm:")
	require.Contains(t, conv.ExternalRef, user.ID, "DM key must contain user ID")
	require.Contains(t, conv.ExternalRef, agent.ID, "DM key must contain agent ID")
}

// ---------------------------------------------------------------------------
// Test 2: Agent-to-agent DM via CLI request (DEF-164 branch)
// ---------------------------------------------------------------------------

// TestContract_AgentDM_DEF164_CLIRequestThroughHandler verifies that an
// agent-to-agent DM request (DEF-164 code path at handlers_agent_messaging.go:580+)
// succeeds through the real handler.
func TestContract_AgentDM_DEF164_CLIRequestThroughHandler(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project with two agents.
	project := &store.Project{
		ID:   tid("contract-agent-dm-project"),
		Name: "contract-agent-dm-project",
		Slug: "contract-agent-dm-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a broker so agents can have RuntimeBrokerID set.
	brokerID := tid("contract-broker")
	broker := &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "contract-broker",
		Slug:   "contract-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agentA := &store.Agent{
		ID:              tid("contract-agent-a"),
		Name:            "agent-a",
		Slug:            "agent-a",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	agentB := &store.Agent{
		ID:              tid("contract-agent-b"),
		Name:            "agent-b",
		Slug:            "agent-b",
		ProjectID:       project.ID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
		RuntimeBrokerID: brokerID,
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create a DM conversation between the two agents.
	dmKey, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)

	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Set a recording dispatcher.
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Send agent-to-agent message from A to B using ConversationRef.
	// This exercises the DEF-164 branch at handlers_agent_messaging.go:580+.
	reqBody, _ := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		Msg:             "Agent DM test",
		Type:            "instruction",
	})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agentA.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentA.ID},
		ProjectID: project.ID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentA.ID)

	// Assertions: HTTP 200 (not 400 rejection).
	require.Equal(t, http.StatusOK, rr.Code, "agent-to-agent DM must succeed; body: %s", rr.Body.String())

	// Message persisted with agent recipient fields.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agentA.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items, "at least one message must be persisted")

	// Find the message we just sent.
	var msg *store.Message
	for i := range msgs.Items {
		if msgs.Items[i].Msg == "Agent DM test" {
			msg = &msgs.Items[i]
			break
		}
	}
	require.NotNil(t, msg, "message must be found")
	require.NotEmpty(t, msg.ConversationID, "message must have ConversationID")

	// DM conversation key contains both agent IDs (dm:agent:...).
	convLoaded, err := s.GetConversation(ctx, msg.ConversationID)
	require.NoError(t, err)
	require.Equal(t, "direct", convLoaded.Kind)
	require.Contains(t, convLoaded.ExternalRef, "dm:agent:", "DM key must contain dm:agent:")

	// Dispatcher called (agent messages dispatch).
	require.Eventually(t, func() bool {
		dispatcher.mu.Lock()
		defer dispatcher.mu.Unlock()
		return len(dispatcher.calls) == 1
	}, 2*time.Second, 100*time.Millisecond, "agent DM should dispatch to recipient agent")
	dispatcher.mu.Lock()
	require.Equal(t, agentB.ID, dispatcher.calls[0].Agent.ID)
	dispatcher.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Test 3: Reply via ConversationRef (conv:<uuid> format)
// ---------------------------------------------------------------------------

// TestContract_ConvRefReply_CLIRequestThroughHandler verifies that a reply
// using ConversationRef (the conv:<uuid> format from sendMessageViaConversation
// in the CLI) posts to the SAME conversation.
func TestContract_ConvRefReply_CLIRequestThroughHandler(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create an existing DM conversation.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Seed an initial message in this conversation.
	initialMsg := &store.Message{
		ID:             tid("initial-msg"),
		ConversationID: conv.ID,
		ProjectID:      project.ID,
		Msg:            "Initial message",
		Sender:         "user:" + user.Email,
		SenderID:       user.ID,
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID,
		AgentID:        agent.ID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(ctx, initialMsg))

	// Set a recording dispatcher.
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Send a reply using ConversationRef.
	reqBody, _ := json.Marshal(OutboundMessageRequest{
		ConversationRef: "conv:" + conv.ID,
		Msg:             "Reply via conv ref",
		Type:            "instruction",
	})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.ID+"/outbound-message",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)

	// Assertions: HTTP 200.
	require.Equal(t, http.StatusOK, rr.Code, "conv ref reply must succeed; body: %s", rr.Body.String())

	// Message persisted in SAME conversation as the original.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items, "at least one message must be persisted")

	// Find the reply message (not the initial message).
	var msg *store.Message
	for i := range msgs.Items {
		if msgs.Items[i].Msg == "Reply via conv ref" {
			msg = &msgs.Items[i]
			break
		}
	}
	require.NotNil(t, msg, "reply message must be found")
	require.Equal(t, conv.ID, msg.ConversationID, "reply must be in same conversation")
}

// ---------------------------------------------------------------------------
// Test 4: Backfilled data visible through read handler
// ---------------------------------------------------------------------------

// TestContract_BackfilledData_VisibleThroughReadHandler is the critical
// cross-boundary test. It seeds messages WITHOUT conversation_id (pre-upgrade),
// runs the production BackfillService, and verifies the backfilled messages
// are visible via the read handler.
func TestContract_BackfilledData_VisibleThroughReadHandler(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Enable the read switch.
	enableReadSwitch(t, srv)

	// Create DM conversation manually.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Seed messages WITHOUT conversation_id (pre-upgrade pattern).
	msg1 := &store.Message{
		ID:             tid("backfill-msg-1"),
		ConversationID: "", // Empty = pre-upgrade
		ProjectID:      project.ID,
		Msg:            "Pre-upgrade message 1",
		Sender:         "user:" + user.Email,
		SenderID:       user.ID,
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID,
		AgentID:        agent.ID,
		CreatedAt:      time.Now().UTC().Add(-10 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, msg1))

	msg2 := &store.Message{
		ID:             tid("backfill-msg-2"),
		ConversationID: "", // Empty = pre-upgrade
		ProjectID:      project.ID,
		Msg:            "Pre-upgrade message 2",
		Sender:         "agent:" + agent.Slug,
		SenderID:       agent.ID,
		Recipient:      "user:" + user.Email,
		RecipientID:    user.ID,
		AgentID:        agent.ID,
		CreatedAt:      time.Now().UTC().Add(-5 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, msg2))

	// Run production BackfillService.
	backfillSvc := messaging.NewBackfillService(s, s, nil)
	result, err := backfillSvc.Run(ctx, messaging.BackfillConfig{
		ProjectID: project.ID,
	})
	require.NoError(t, err, "BackfillService.Run must succeed")
	require.NotNil(t, result)

	// Verify messages now have ConversationID set.
	reloaded1, err := s.GetMessage(ctx, msg1.ID)
	require.NoError(t, err)
	require.NotEmpty(t, reloaded1.ConversationID, "backfill must set conversation_id on msg1")

	reloaded2, err := s.GetMessage(ctx, msg2.ID)
	require.NoError(t, err)
	require.NotEmpty(t, reloaded2.ConversationID, "backfill must set conversation_id on msg2")

	// Both messages should be in the same conversation.
	require.Equal(t, conv.ID, reloaded1.ConversationID, "msg1 must be in DM conversation")
	require.Equal(t, conv.ID, reloaded2.ConversationID, "msg2 must be in DM conversation")

	// Verify backfill results show success.
	require.Greater(t, result.Attributed, 0, "backfill should have attributed messages")
	require.Empty(t, result.Errors, "backfill should complete without errors")
}

// ---------------------------------------------------------------------------
// Test 5: Mixed channels backfill then read
// ---------------------------------------------------------------------------

// TestContract_MixedChannels_BackfillThenRead verifies that messages with
// Channel:"" and Channel:"web" in the same DM thread are both attributed to
// the same conversation (no surface_conflict) and readable via the history
// endpoint.
func TestContract_MixedChannels_BackfillThenRead(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Enable the read switch.
	enableReadSwitch(t, srv)

	// Create DM conversation manually.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Seed messages with Channel:"" and Channel:"web" (pre-upgrade pattern).
	msgNoChannel := &store.Message{
		ID:             tid("mixed-channel-msg-1"),
		ConversationID: "", // Empty = pre-upgrade
		ProjectID:      project.ID,
		Channel:        "", // No channel
		Msg:            "Message without channel",
		Sender:         "user:" + user.Email,
		SenderID:       user.ID,
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID,
		AgentID:        agent.ID,
		CreatedAt:      time.Now().UTC().Add(-10 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, msgNoChannel))

	msgWebChannel := &store.Message{
		ID:             tid("mixed-channel-msg-2"),
		ConversationID: "", // Empty = pre-upgrade
		ProjectID:      project.ID,
		Channel:        "web", // Web channel
		Msg:            "Message with web channel",
		Sender:         "agent:" + agent.Slug,
		SenderID:       agent.ID,
		Recipient:      "user:" + user.Email,
		RecipientID:    user.ID,
		AgentID:        agent.ID,
		CreatedAt:      time.Now().UTC().Add(-5 * time.Minute),
	}
	require.NoError(t, s.CreateMessage(ctx, msgWebChannel))

	// Run production BackfillService.
	backfillSvc := messaging.NewBackfillService(s, s, nil)
	result, err := backfillSvc.Run(ctx, messaging.BackfillConfig{
		ProjectID: project.ID,
	})
	require.NoError(t, err, "BackfillService.Run must succeed")
	require.NotNil(t, result)

	// Verify both messages have same ConversationID (no surface conflict).
	reloaded1, err := s.GetMessage(ctx, msgNoChannel.ID)
	require.NoError(t, err)
	require.NotEmpty(t, reloaded1.ConversationID)

	reloaded2, err := s.GetMessage(ctx, msgWebChannel.ID)
	require.NoError(t, err)
	require.NotEmpty(t, reloaded2.ConversationID)

	require.Equal(t, reloaded1.ConversationID, reloaded2.ConversationID,
		"both messages must be in same conversation despite channel difference")
	require.Equal(t, conv.ID, reloaded1.ConversationID, "messages must be in DM conversation")

	// Verify backfill results show no surface conflict.
	require.Greater(t, result.Attributed, 0, "backfill should have attributed messages")
	require.Empty(t, result.Errors, "backfill should complete without errors")
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// contractChatHistoryResponse matches the structure returned by handleConversationHistory.
type contractChatHistoryResponse struct {
	Messages []struct {
		ID        string `json:"id"`
		Msg       string `json:"msg"`
		Sender    string `json:"sender"`
		Timestamp string `json:"timestamp"`
	} `json:"messages"`
}

// readConversationHistoryAsUserContract calls GET /api/v1/chat/conversations/{key}/messages
// authenticated as the given user.
func readConversationHistoryAsUserContract(t *testing.T, srv *Server, user *store.User, key string) (int, contractChatHistoryResponse) {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/chat/conversations/"+key+"/messages", nil)
	var resp contractChatHistoryResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, resp
}
