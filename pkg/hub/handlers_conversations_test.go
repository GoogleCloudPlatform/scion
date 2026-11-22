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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// projectReaderAgentContext is an agent caller that can read its project
// (project-read scope and a project role), as the conversation list and
// add-participant require of their callers.
func projectReaderAgentContext(t *testing.T, s store.Store, agentID, projectID string) context.Context {
	t.Helper()
	grantAgentProjectAccess(t, s, agentID, projectID)
	return agentContextWithScopes(agentID, projectID, []AgentTokenScope{ScopeProjectRead})
}

// ---- Tests ----

func TestListConversations_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	req = req.WithContext(projectReaderAgentContext(t, s, agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Conversations, 1)
	require.Equal(t, conv.ID, result.Conversations[0].ID)
	require.Equal(t, "Test Conversation", result.Conversations[0].DisplayName)
	// Participants are deliberately omitted from list responses (R-2: N+1 fix).
	// Use GET /conversations/{id} for participant details.
	require.Empty(t, result.Conversations[0].Participants)
}

func TestListConversations_WithFilters(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	// Create a second conversation with different surface.
	now := time.Now().UTC()
	conv2 := &store.Conversation{
		ID:             api.NewUUID(),
		ProjectID:      &project.ID,
		Kind:           "group",
		Surface:        "discord",
		DisplayName:    "Discord Thread",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(context.Background(), conv2))
	addConvParticipant(t, s, conv2.ID, "agent", agent.ID)

	readerCtx := projectReaderAgentContext(t, s, agent.ID, project.ID)

	// Filter by surface=native — should only return the first conversation.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?surface=native", nil)
	req = req.WithContext(readerCtx)
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Conversations, 1)
	require.Equal(t, conv.ID, result.Conversations[0].ID)

	// Filter by surface=discord — should only return the second conversation.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/conversations?surface=discord", nil)
	req = req.WithContext(readerCtx)
	rr = httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Conversations, 1)
	require.Equal(t, conv2.ID, result.Conversations[0].ID)
}

func TestListConversations_WithLimit(t *testing.T) {
	srv, s := testServer(t)
	project, agent, _ := setupConvTestData(t, s)

	// Create 3 more conversations (4 total with the one from setup).
	for i := 0; i < 3; i++ {
		now := time.Now().UTC()
		c := &store.Conversation{
			ID:             api.NewUUID(),
			ProjectID:      &project.ID,
			Kind:           "group",
			Surface:        "native",
			DisplayName:    "Conv " + string(rune('A'+i)),
			DriftState:     "active",
			LastActivityAt: now,
			CreatedAt:      now,
		}
		require.NoError(t, s.CreateConversation(context.Background(), c))
		addConvParticipant(t, s, c.ID, "agent", agent.ID)
	}
	// Add agent to the first conv from setup too.
	conv := &store.Conversation{} // dummy; we already know setup created one
	_ = conv
	// We need the original conv ID. Re-read from store.
	convs, err := s.GetConversationsForPrincipal(context.Background(), "agent", agent.ID)
	require.NoError(t, err)
	require.Len(t, convs, 3) // agent was only added to 3 new ones

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?limit=2", nil)
	req = req.WithContext(projectReaderAgentContext(t, s, agent.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Conversations, 2)
}

func TestListConversations_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusForbidden, rr.Code)
}

func TestListConversations_MethodNotAllowed(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/conversations", nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestGetConversation_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, conv.ID, result.ID)
	require.Equal(t, "Test Conversation", result.DisplayName)
	require.Len(t, result.Participants, 1)
	require.Equal(t, "agent", result.Participants[0].PrincipalKind)
	require.Equal(t, agent.ID, result.Participants[0].PrincipalID)
}

func TestGetConversation_NotParticipant(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	// Don't add the agent as a participant.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code)
}

func TestGetConversation_NotFound(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/nonexistent-id", nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, "nonexistent-id")

	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestConvListMessages_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create a test message in the conversation.
	recipientID := api.NewUUID()
	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      project.ID,
		AgentID:        agent.ID,
		Sender:         "agent:" + agent.Name,
		SenderID:       agent.ID,
		Recipient:      "user:test@example.com",
		RecipientID:    recipientID,
		Msg:            "Hello from conversation",
		Type:           "instruction",
		ConversationID: conv.ID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages", nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleConvListMessages(rr, req, conv.ID)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var result store.ListResult[store.Message]
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Items, 1)
	require.Equal(t, "Hello from conversation", result.Items[0].Msg)
}

func TestConvListMessages_NotParticipant(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	// Don't add the agent as a participant.

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages", nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleConvListMessages(rr, req, conv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code)
}

func TestGetConversationMessage_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      project.ID,
		AgentID:        agent.ID,
		Sender:         "agent:" + agent.Name,
		SenderID:       agent.ID,
		Recipient:      "user:test@example.com",
		RecipientID:    api.NewUUID(),
		Msg:            "A specific message",
		Type:           "instruction",
		ConversationID: conv.ID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages/"+msg.ID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleConversationRoutes(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var result store.Message
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, msg.ID, result.ID)
	require.Equal(t, msg.ConversationID, result.ConversationID)
	require.Equal(t, msg.Msg, result.Msg)
}

func TestGetConversationMessage_DMAuth(t *testing.T) {
	srv, s := testServer(t)
	project, agentA, _ := setupConvTestData(t, s)
	agentB := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "get-message-dm-agent-b",
		Slug:      "get-message-dm-agent-b",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(context.Background(), agentB))

	conv := setupDMConversation(t, s, agentA.ID, agentB.ID)
	require.NoError(t, s.RemoveParticipant(context.Background(), conv.ID, "agent", agentA.ID))

	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      project.ID,
		AgentID:        agentB.ID,
		Sender:         "agent:" + agentB.Name,
		SenderID:       agentB.ID,
		Recipient:      "agent:" + agentA.Name,
		RecipientID:    agentA.ID,
		Msg:            "DM message after leaving",
		Type:           "instruction",
		ConversationID: conv.ID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages/"+msg.ID, nil)
	req = req.WithContext(agentContext(agentA.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr, req, conv.ID, msg.ID)

	require.Equal(t, http.StatusOK, rr.Code,
		"canonical DM participant should retain message access after leaving; body: %s", rr.Body.String())
}

func TestGetConversationMessage_NotParticipant(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)

	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      project.ID,
		AgentID:        agent.ID,
		Sender:         "agent:" + agent.Name,
		SenderID:       agent.ID,
		Recipient:      "user:test@example.com",
		RecipientID:    api.NewUUID(),
		Msg:            "Private message",
		Type:           "instruction",
		ConversationID: conv.ID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages/"+msg.ID, nil)
	req = req.WithContext(agentContext(agent.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr, req, conv.ID, msg.ID)

	require.Equal(t, http.StatusForbidden, rr.Code)
}

func TestGetConversationMessage_WrongConversation(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	otherConv := &store.Conversation{
		ID:             api.NewUUID(),
		ProjectID:      &project.ID,
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Other Conversation",
		DriftState:     "active",
		LastActivityAt: time.Now().UTC(),
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateConversation(context.Background(), otherConv))
	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      project.ID,
		AgentID:        agent.ID,
		Sender:         "agent:" + agent.Name,
		SenderID:       agent.ID,
		Recipient:      "user:test@example.com",
		RecipientID:    api.NewUUID(),
		Msg:            "Message from another conversation",
		Type:           "instruction",
		ConversationID: otherConv.ID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages/"+msg.ID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr, req, conv.ID, msg.ID)

	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestGetConversationMessage_NotFound(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	messageID := api.NewUUID()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages/"+messageID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr, req, conv.ID, messageID)

	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestGetConversationMessage_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/conversation-id/messages/message-id", nil)
	rr := httptest.NewRecorder()

	srv.handleGetConversationMessage(rr, req, "conversation-id", "message-id")

	require.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestConvListMessages_WithPagination(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create multiple messages.
	recipientID := api.NewUUID()
	for i := 0; i < 5; i++ {
		msg := &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      project.ID,
			AgentID:        agent.ID,
			Sender:         "agent:" + agent.Name,
			SenderID:       agent.ID,
			Recipient:      "user:test@example.com",
			RecipientID:    recipientID,
			Msg:            "Message " + string(rune('A'+i)),
			Type:           "instruction",
			ConversationID: conv.ID,
			CreatedAt:      time.Now().UTC(),
		}
		require.NoError(t, s.CreateMessage(context.Background(), msg))
	}

	// Request with limit=2.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages?limit=2", nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleConvListMessages(rr, req, conv.ID)

	require.Equal(t, http.StatusOK, rr.Code)
	var result store.ListResult[store.Message]
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Items, 2)
}

func TestCreateConversation_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	wireSharedWebChatStore(t, srv, s)
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := createConversationRequest{
		DisplayName: "New Discussion",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, "New Discussion", result.DisplayName)
	require.Equal(t, "group", result.Kind)
	require.Equal(t, "native", result.Surface)
	require.NotEmpty(t, result.ID)
	// Caller should be auto-added as participant.
	require.Len(t, result.Participants, 1)
	require.Equal(t, "agent", result.Participants[0].PrincipalKind)
	require.Equal(t, agent.ID, result.Participants[0].PrincipalID)
}

func TestCreateConversation_MissingName(t *testing.T) {
	srv, s := testServer(t)
	_, agent, _ := setupConvTestData(t, s)

	body := createConversationRequest{Kind: "group"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, ""))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestCreateConversation_DefaultsToGroup(t *testing.T) {
	srv, s := testServer(t)
	wireSharedWebChatStore(t, srv, s)
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// projectId comes from the agent's own token (§3.2 step 2), not the body —
	// this test is about kind defaulting, not project resolution.
	body := createConversationRequest{DisplayName: "No Kind Specified"}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, "group", result.Kind)
}

func TestSetDefaultAgent_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create a second agent to set as the default (must exist in the store per N-1 validation).
	newAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "new-default-agent",
		Slug:      "new-default-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(context.Background(), newAgent))

	body := setDefaultAgentRequest{AgentID: newAgent.ID}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify the default agent was set.
	ctx := context.Background()
	updated, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.DefaultAgentID)
	require.Equal(t, newAgent.ID, *updated.DefaultAgentID)
}

// TestSetDefaultAgent_NoProjectAccess is review round 1 finding #8: renamed
// from TestSetDefaultAgent_NotParticipant. It still passes, but for a
// different reason since Phase 3 — the gate is project membership
// (authorizeGroupConversationAccess), not the participant table, so a
// caller with no role binding on the conversation's project is denied
// regardless of whether it is a participant.
func TestSetDefaultAgent_NoProjectAccess(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	// Don't grant the agent project access (and don't add it as a
	// participant either — that table no longer gates this endpoint).

	body := setDefaultAgentRequest{AgentID: api.NewUUID()}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code)
}

func TestSetDefaultAgent_MissingAgentID(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := setDefaultAgentRequest{} // empty agent ID
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestConversationRoutes_MethodNotAllowed(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// GET on get conversation endpoint works.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)
	require.Equal(t, http.StatusOK, rr.Code)

	// DELETE on get conversation is not allowed.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr = httptest.NewRecorder()
	srv.handleGetConversation(rr, req, conv.ID)
	require.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// POST on messages endpoint is not allowed (read-only).
	req = httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/messages", nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr = httptest.NewRecorder()
	srv.handleConvListMessages(rr, req, conv.ID)
	require.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestListConversations_AsUser(t *testing.T) {
	srv, s := testServer(t)
	project, _, conv := setupConvTestData(t, s)

	// Create a user who can read the project and add them as participant.
	userID := api.NewUUID()
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: userID, Email: "testuser@example.com", DisplayName: "Test User", Role: "member", Status: "active",
	}))
	grantUserProjectAccess(t, s, userID, project.ID)
	addConvParticipant(t, s, conv.ID, "user", userID)

	// Use user identity directly
	userCtx := contextWithIdentity(context.Background(),
		NewAuthenticatedUser(userID, "testuser@example.com", "Test User", "user", "web"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	req = req.WithContext(userCtx)
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Conversations, 1)
	require.Equal(t, conv.ID, result.Conversations[0].ID)
}

// ---- Validation tests (N-1, N-2) ----

func TestSetDefaultAgent_AgentNotFound(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Use a non-existent agent ID.
	body := setDefaultAgentRequest{AgentID: api.NewUUID()}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	// Review round 1 finding #7: conv is project-scoped, so this now
	// resolves through validateDefaultAgent (DEF-31, the single source of
	// truth also used by the topic PATCH and thread-create writers), which
	// reports every resolution failure — not found, wrong project, or
	// soft-deleted — as one 400, not a 404.
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

// TestSetDefaultAgent_TooLongAgentID_MessageNamesAgentIdField is review
// round 4 finding #1: validateDefaultAgent now takes a field parameter
// instead of the handler string-editing its "defaultAgent"-worded message
// with strings.Replace. Pins both halves of that contract for the
// too-long-identifier case: 400, and a message containing "agentId" and
// "too long" (not the validator's internal "defaultAgent" wording).
func TestSetDefaultAgent_TooLongAgentID_MessageNamesAgentIdField(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := setDefaultAgentRequest{AgentID: strings.Repeat("a", 201)}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	require.Contains(t, errResp.Error.Message, "agentId")
	require.Contains(t, errResp.Error.Message, "too long")
}

// TestSetDefaultAgent_NotFound_MessageNamesAgentIdField is review round 4
// finding #1's second pinned case: a nonexistent agentId on a
// project-scoped group must produce a message naming "agentId", not
// validateDefaultAgent's internal "defaultAgent" wording.
func TestSetDefaultAgent_NotFound_MessageNamesAgentIdField(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := setDefaultAgentRequest{AgentID: api.NewUUID()}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	require.Contains(t, errResp.Error.Message, "agentId")
	require.NotContains(t, errResp.Error.Message, "defaultAgent")
}

// TestSetDefaultAgent_SoftDeletedAgentRejected is review round 1 finding #7:
// a soft-deleted agent must not be set as a group's default agent — it
// would drive web routing after ClearTopicDefaultAgent has already run for
// it, since ClearTopicDefaultAgent only fires on the deletion event itself.
func TestSetDefaultAgent_SoftDeletedAgentRejected(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	deletedAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "soft-deleted-agent",
		Slug:      "soft-deleted-agent",
		ProjectID: project.ID,
		Phase:     "terminated",
		DeletedAt: time.Now().UTC(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), deletedAgent))

	body := setDefaultAgentRequest{AgentID: deletedAgent.ID}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"a soft-deleted agent must be rejected; body: %s", rr.Body.String())
}

// TestSetDefaultAgent_ProjectlessGroup_AgentNotFound_ReturnsValidationError is
// review round 3 finding #3: pin the round-2 #6 behavior for the *projectless*
// branch of handleSetDefaultAgent (conv.ProjectID == nil, a legacy group with
// no validateDefaultAgent to delegate to) — a nonexistent agentId must come
// back as 400 validation_error, not 404. Round-2 #6 fixed the status code;
// round-3 #4 then split this branch further so only store.ErrNotFound and
// soft-delete map to 400, with any other store error going to 500 instead.
// This test's mutation coverage is for the not-found case specifically:
// reverting the round-2 #6 fix (mapping ErrNotFound to NotFound(w, ...)
// instead of ValidationError) makes this fail, without touching the
// project-scoped tests above.
func TestSetDefaultAgent_ProjectlessGroup_AgentNotFound_ReturnsValidationError(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	now := time.Now().UTC()
	legacyConv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Legacy Projectless Group",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, legacyConv))

	project := &store.Project{ID: api.NewUUID(), Name: "phase3-legacy-def-agent-project", Slug: "phase3-legacy-def-agent-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	participant := &store.Agent{
		ID: api.NewUUID(), Name: "phase3-legacy-def-agent-participant", Slug: "phase3-legacy-def-agent-participant",
		ProjectID: project.ID, Phase: "running",
	}
	require.NoError(t, s.CreateAgent(ctx, participant))
	addConvParticipant(t, s, legacyConv.ID, "agent", participant.ID)

	body := setDefaultAgentRequest{AgentID: api.NewUUID()} // non-existent agent
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+legacyConv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(participant.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, legacyConv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"a nonexistent agentId on a projectless group must be a validation error, not a 404; body: %s", rr.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	require.Equal(t, ErrCodeValidationError, errResp.Error.Code)
}

// nilAgentStore wraps a real store and forces GetAgent to return (nil, nil)
// for one specific ID. No real store.Store implementation does this — a
// successful lookup always returns a non-nil agent — but the handler must
// not rely on that invariant when err == nil.
type nilAgentStore struct {
	store.Store
	nilForID string
}

func (s *nilAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.nilForID {
		return nil, nil
	}
	return s.Store.GetAgent(ctx, id)
}

// TestSetDefaultAgent_ProjectlessGroup_NilAgentNoPanic is the upstream
// review fix for GoogleCloudPlatform/scion#1864 (gemini-code-assist,
// medium): handleSetDefaultAgent's projectless branch dereferenced
// agent.DeletedAt right after a nil-error GetAgent call, assuming err == nil
// implies agent != nil — a nil, nil result would panic instead of reporting
// not-found. Forces exactly that response via nilAgentStore and asserts a
// clean 400 validation_error, not a panic.
func TestSetDefaultAgent_ProjectlessGroup_NilAgentNoPanic(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	now := time.Now().UTC()
	legacyConv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Legacy Projectless Group Nil Agent",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, legacyConv))

	project := &store.Project{ID: api.NewUUID(), Name: "phase3-legacy-nil-agent-project", Slug: "phase3-legacy-nil-agent-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	participant := &store.Agent{
		ID: api.NewUUID(), Name: "phase3-legacy-nil-agent-participant", Slug: "phase3-legacy-nil-agent-participant",
		ProjectID: project.ID, Phase: "running",
	}
	require.NoError(t, s.CreateAgent(ctx, participant))
	addConvParticipant(t, s, legacyConv.ID, "agent", participant.ID)

	nilForID := api.NewUUID()
	srv.store = &nilAgentStore{Store: s, nilForID: nilForID}

	body := setDefaultAgentRequest{AgentID: nilForID}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+legacyConv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(participant.ID, project.ID))
	rr := httptest.NewRecorder()

	require.NotPanics(t, func() {
		srv.handleSetDefaultAgent(rr, req, legacyConv.ID)
	})

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"a nil agent from GetAgent must be reported as not-found, not panic; body: %s", rr.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	require.Equal(t, ErrCodeValidationError, errResp.Error.Code)
}

func TestCreateConversation_ProjectNotFound(t *testing.T) {
	srv, s := testServer(t)
	_, agent, _ := setupConvTestData(t, s)

	body := createConversationRequest{
		DisplayName: "Test Conv",
		ProjectID:   api.NewUUID(), // non-existent project
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, ""))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusNotFound, rr.Code)
}

// ---- Mux-level integration tests (R-3) ----
// These tests send requests through srv.mux.ServeHTTP to verify the HTTP
// routing layer, not just individual handler functions. The R-1 routing bug
// (POST /api/v1/conversations unreachable) was missed because the original
// tests called handlers directly.

func TestMux_CreateConversation(t *testing.T) {
	srv, s := testServer(t)
	wireSharedWebChatStore(t, srv, s)
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	body := createConversationRequest{
		DisplayName: "Mux Test Conv",
		ProjectID:   project.ID,
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, "Mux Test Conv", result.DisplayName)
	require.Equal(t, "group", result.Kind)
	require.NotEmpty(t, result.ID)
	require.Len(t, result.Participants, 1)
}

func TestMux_ListConversations(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	req = req.WithContext(projectReaderAgentContext(t, s, agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Conversations, 1)
	require.Equal(t, conv.ID, result.Conversations[0].ID)
}

func TestMux_GetConversation(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID, nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, conv.ID, result.ID)
	require.Equal(t, "Test Conversation", result.DisplayName)
	// GET detail endpoint should include participants.
	require.Len(t, result.Participants, 1)
}

func TestMux_ListMessages(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create a test message.
	msg := &store.Message{
		ID:             api.NewUUID(),
		ProjectID:      project.ID,
		AgentID:        agent.ID,
		Sender:         "agent:" + agent.Name,
		SenderID:       agent.ID,
		Recipient:      "user:test@example.com",
		RecipientID:    api.NewUUID(),
		Msg:            "Mux routed message",
		Type:           "instruction",
		ConversationID: conv.ID,
		CreatedAt:      time.Now().UTC(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+conv.ID+"/messages", nil)
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var result store.ListResult[store.Message]
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Items, 1)
	require.Equal(t, "Mux routed message", result.Items[0].Msg)
}

func TestMux_SetDefaultAgent(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// The agent ID must reference an existing agent (N-1 validation).
	body := setDefaultAgentRequest{AgentID: agent.ID}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify the default agent was set.
	updated, err := s.GetConversation(context.Background(), conv.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.DefaultAgentID)
	require.Equal(t, agent.ID, *updated.DefaultAgentID)
}

// ---- Participant management tests ----

func TestCreateConversation_DefaultsToAgentProject(t *testing.T) {
	srv, s := testServer(t)
	wireSharedWebChatStore(t, srv, s)
	project, agent, _ := setupConvTestData(t, s)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create conversation without specifying projectId.
	body := createConversationRequest{
		DisplayName: "No Project Specified",
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result conversationResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.NotNil(t, result.ProjectID, "conversation should have project ID set")
	require.Equal(t, project.ID, *result.ProjectID, "project ID should default to agent's project")
}

func TestAddParticipant_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	// Create another agent to add.
	newAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "new-participant",
		Slug:      "new-participant",
		ProjectID: *conv.ProjectID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(context.Background(), newAgent))

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   newAgent.ID,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, convProjectID(conv), []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

	var result store.ConversationParticipant
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, "agent", result.PrincipalKind)
	require.Equal(t, newAgent.ID, result.PrincipalID)
	require.Equal(t, "member", result.Role)
}

func TestAddParticipant_NotParticipant(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	// Don't add the caller as a participant.

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   api.NewUUID(),
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	// A non-participant gets the same answer as for an unknown conversation.
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "Conversation not found")
}

func TestAddParticipant_AlreadyExists(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	// Try to add the same agent again.
	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   agent.ID,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, convProjectID(conv), []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	require.Equal(t, http.StatusConflict, rr.Code)
}

func TestAddParticipant_InvalidPrincipalKind(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	body := addParticipantRequest{
		PrincipalKind: "robot",
		PrincipalID:   api.NewUUID(),
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(projectReaderAgentContext(t, s, agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestAddParticipant_CrossProjectAgent(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	// Create an agent in a DIFFERENT project.
	otherProject := &store.Project{
		ID:   api.NewUUID(),
		Name: "other-project",
		Slug: "other-project",
	}
	require.NoError(t, s.CreateProject(context.Background(), otherProject))

	crossProjectAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "cross-project-agent",
		Slug:      "cross-project-agent",
		ProjectID: otherProject.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(context.Background(), crossProjectAgent))

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   crossProjectAgent.ID,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, convProjectID(conv), []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	// An agent of another project gets the same answer as an unknown agent.
	require.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), "agent not found")
}

func TestAddParticipant_AgentNotFound(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   api.NewUUID(), // non-existent agent
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, convProjectID(conv), []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	require.Equal(t, http.StatusNotFound, rr.Code)
}

// TestAddParticipant_SoftDeletedAgentRefused: adding a soft-deleted agent
// of the conversation's own project is refused like an unknown agent, and
// no participant row is written.
func TestAddParticipant_SoftDeletedAgentRefused(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	deletedAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "soft-deleted-participant",
		Slug:      "soft-deleted-participant",
		ProjectID: *conv.ProjectID,
		Phase:     "terminated",
		DeletedAt: time.Now().UTC(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), deletedAgent))

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   deletedAgent.ID,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, convProjectID(conv), []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	require.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), "agent not found")

	isParticipant, err := isConversationParticipant(context.Background(), s, conv.ID, "agent", deletedAgent.ID)
	require.NoError(t, err)
	require.False(t, isParticipant, "a soft-deleted agent must not become a participant")
}

func TestAddParticipant_ResolveAgentBySlugAndName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, callerAgent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", callerAgent.ID)
	callerCtx := agentContextWithScopes(callerAgent.ID, project.ID, []AgentTokenScope{ScopeProjectRead})

	postJoin := func(principal string) *httptest.ResponseRecorder {
		bodyBytes, _ := json.Marshal(addParticipantRequest{
			PrincipalKind: "agent",
			PrincipalID:   principal,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(callerCtx)
		rr := httptest.NewRecorder()
		srv.handleAddParticipant(rr, req, conv.ID)
		return rr
	}

	// Agent resolved by slug.
	slugAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Slug Agent",
		Slug:      "slug-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, slugAgent))

	rr := postJoin("slug-agent")
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	var result store.ConversationParticipant
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, slugAgent.ID, result.PrincipalID)

	// Agent resolved by @slug (leading @ stripped).
	atAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "At Agent",
		Slug:      "at-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, atAgent))

	rr = postJoin("@at-agent")
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, atAgent.ID, result.PrincipalID)

	// Agent resolved by display name (case-insensitive, differs from slug).
	namedAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Release Coordinator",
		Slug:      "rel-coord",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, namedAgent))

	rr = postJoin("release coordinator")
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, namedAgent.ID, result.PrincipalID)

	// Ambiguous agent display name (two live agents share Name with different slugs).
	dup1 := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Shared Worker",
		Slug:      "shared-worker-1",
		ProjectID: project.ID,
		Phase:     "running",
	}
	dup2 := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Shared Worker",
		Slug:      "shared-worker-2",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, dup1))
	require.NoError(t, s.CreateAgent(ctx, dup2))

	rr = postJoin("Shared Worker")
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), "ambiguous")

	// Cross-project agent by slug is indistinguishable from an unknown agent.
	otherProject := &store.Project{
		ID:   api.NewUUID(),
		Name: "other-project-resolve",
		Slug: "other-project-resolve",
	}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	otherAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Secret Agent",
		Slug:      "secret-agent",
		ProjectID: otherProject.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, otherAgent))

	rrOther := postJoin("secret-agent")
	rrUnknown := postJoin("does-not-exist")
	require.Equal(t, http.StatusNotFound, rrOther.Code, "body: %s", rrOther.Body.String())
	require.Equal(t, http.StatusNotFound, rrUnknown.Code, "body: %s", rrUnknown.Body.String())
	require.Equal(t, rrUnknown.Body.String(), rrOther.Body.String(),
		"cross-project agent name must be indistinguishable from unknown agent")

	// Soft-deleted agent by slug is refused as unknown.
	deletedAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Gone Agent",
		Slug:      "gone-agent",
		ProjectID: project.ID,
		Phase:     "terminated",
		DeletedAt: time.Now().UTC(),
	}
	require.NoError(t, s.CreateAgent(ctx, deletedAgent))

	rrDeleted := postJoin("gone-agent")
	require.Equal(t, http.StatusNotFound, rrDeleted.Code, "body: %s", rrDeleted.Body.String())
	require.Equal(t, rrUnknown.Body.String(), rrDeleted.Body.String())

	// Cross-project agent ID with no local match is indistinguishable from unknown.
	rrOtherID := postJoin(otherAgent.ID)
	require.Equal(t, http.StatusNotFound, rrOtherID.Code, "body: %s", rrOtherID.Body.String())
	require.Equal(t, rrUnknown.Body.String(), rrOtherID.Body.String(),
		"cross-project agent ID must be indistinguishable from unknown agent")

	// Cross-project and soft-deleted agents sharing a Name with a single live
	// local agent must not trigger 409 ambiguous_reference.
	liveShared := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Secret Agent",
		Slug:      "local-secret-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	deletedShared := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "Secret Agent",
		Slug:      "deleted-secret-agent",
		ProjectID: project.ID,
		Phase:     "terminated",
		DeletedAt: time.Now().UTC(),
	}
	require.NoError(t, s.CreateAgent(ctx, liveShared))
	require.NoError(t, s.CreateAgent(ctx, deletedShared))
	rrLiveShared := postJoin("Secret Agent")
	require.Equal(t, http.StatusCreated, rrLiveShared.Code, "body: %s", rrLiveShared.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrLiveShared.Body.Bytes(), &result))
	require.Equal(t, liveShared.ID, result.PrincipalID)

	// Existence-oracle regression: a local agent whose Name matches another
	// project's agent ID (with a different slug) must resolve identically to a
	// local agent whose Name is an unknown UUID.
	decoyOther := &store.Agent{
		ID:        api.NewUUID(),
		Name:      otherAgent.ID,
		Slug:      "decoy-other-agent-id",
		ProjectID: project.ID,
		Phase:     "running",
	}
	unknownAgentID := api.NewUUID()
	decoyUnknown := &store.Agent{
		ID:        api.NewUUID(),
		Name:      unknownAgentID,
		Slug:      "decoy-unknown-agent-id",
		ProjectID: project.ID,
		Phase:     "running",
	}
	decoyDeleted := &store.Agent{
		ID:        api.NewUUID(),
		Name:      deletedAgent.ID,
		Slug:      "decoy-deleted-agent-id",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, decoyOther))
	require.NoError(t, s.CreateAgent(ctx, decoyUnknown))
	require.NoError(t, s.CreateAgent(ctx, decoyDeleted))

	rrDecoyOther := postJoin(otherAgent.ID)
	rrDecoyUnknown := postJoin(unknownAgentID)
	rrDecoyDeleted := postJoin(deletedAgent.ID)
	require.Equal(t, http.StatusCreated, rrDecoyOther.Code,
		"local agent named after cross-project agent ID must resolve (no oracle); body: %s", rrDecoyOther.Body.String())
	require.Equal(t, http.StatusCreated, rrDecoyUnknown.Code, "body: %s", rrDecoyUnknown.Body.String())
	require.Equal(t, http.StatusCreated, rrDecoyDeleted.Code,
		"local agent named after soft-deleted agent ID must resolve (no oracle); body: %s", rrDecoyDeleted.Body.String())

	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrDecoyOther.Body.Bytes(), &result))
	require.Equal(t, decoyOther.ID, result.PrincipalID)
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrDecoyUnknown.Body.Bytes(), &result))
	require.Equal(t, decoyUnknown.ID, result.PrincipalID)
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrDecoyDeleted.Body.Bytes(), &result))
	require.Equal(t, decoyDeleted.ID, result.PrincipalID)
}

func TestAddParticipant_ResolveUserByEmailAndHandle(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project, callerAgent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", callerAgent.ID)
	callerCtx := agentContextWithScopes(callerAgent.ID, project.ID, []AgentTokenScope{ScopeProjectRead})

	addProjectMember := func(email, displayName string) *store.User {
		u := &store.User{
			ID:          api.NewUUID(),
			Email:       email,
			DisplayName: displayName,
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now().UTC(),
		}
		createTestUserWithProjectRole(t, s, u.ID, u.Email, project.ID, store.ProjectRoleMember)
		// Update display name so handle resolution can match it.
		stored, err := s.GetUser(ctx, u.ID)
		require.NoError(t, err)
		stored.DisplayName = displayName
		require.NoError(t, s.UpdateUser(ctx, stored))
		return stored
	}

	postJoin := func(principal string) *httptest.ResponseRecorder {
		bodyBytes, _ := json.Marshal(addParticipantRequest{
			PrincipalKind: "user",
			PrincipalID:   principal,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(callerCtx)
		rr := httptest.NewRecorder()
		srv.handleAddParticipant(rr, req, conv.ID)
		return rr
	}

	// 1. User by email (case-insensitive).
	uEmail := addProjectMember("alice@example.com", "Alice Example")
	rr := postJoin("ALICE@EXAMPLE.COM")
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	var result store.ConversationParticipant
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, "user", result.PrincipalKind)
	require.Equal(t, uEmail.ID, result.PrincipalID)

	// 2. User by @email (leading @ stripped).
	uAtEmail := addProjectMember("carol@example.com", "Carol Example")
	rr = postJoin("@carol@example.com")
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, uAtEmail.ID, result.PrincipalID)

	// 3. User by hyphenated display-name slug.
	uSlug := addProjectMember("bob.jones@example.com", "Bob Jones")
	rr = postJoin("bob-jones")
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, uSlug.ID, result.PrincipalID)

	// 4. User by email local part.
	uLocal := addProjectMember("diana@example.com", "Different Name")
	rr = postJoin("diana")
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, uLocal.ID, result.PrincipalID)

	// 5. User by raw ID.
	uID := addProjectMember("erin@example.com", "Erin Raw")
	rr = postJoin(uID.ID)
	require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Equal(t, uID.ID, result.PrincipalID)

	// 6. Ambiguous handle (two project members with same email local part).
	_ = addProjectMember("alex@corp.example", "Alex Corp")
	_ = addProjectMember("alex@partner.example", "Alex Partner")
	rr = postJoin("alex")
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), "ambiguous")

	// 7. Unknown user vs outsider (exists in hub, not in project):
	// both must produce the exact same error so existence is not leaked.
	outsider := &store.User{
		ID:          api.NewUUID(),
		Email:       "outsider@example.com",
		DisplayName: "Outsider Person",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, outsider))

	rrUnknownEmail := postJoin("nobody@example.com")
	rrOutsiderEmail := postJoin(outsider.Email)
	rrOutsiderHandle := postJoin("outsider-person")
	rrOutsiderID := postJoin(outsider.ID)

	require.Equal(t, http.StatusBadRequest, rrUnknownEmail.Code, "body: %s", rrUnknownEmail.Body.String())
	require.Equal(t, rrUnknownEmail.Code, rrOutsiderEmail.Code)
	require.Equal(t, rrUnknownEmail.Body.String(), rrOutsiderEmail.Body.String(),
		"outsider email must be indistinguishable from unknown email")
	require.Equal(t, rrUnknownEmail.Code, rrOutsiderHandle.Code)
	require.Equal(t, rrUnknownEmail.Body.String(), rrOutsiderHandle.Body.String(),
		"outsider handle must be indistinguishable from unknown user")
	require.Equal(t, rrUnknownEmail.Code, rrOutsiderID.Code)
	require.Equal(t, rrUnknownEmail.Body.String(), rrOutsiderID.Body.String(),
		"outsider ID must be indistinguishable from unknown user")

	// 8. Existence-oracle regression: a project member setting DisplayName to
	// an outsider's email or UUID must behave identically to setting it to an
	// unregistered email or unknown UUID (no short-circuit before handle fallback).
	decoyVictimEmail := addProjectMember("decoy-email-1@example.com", outsider.Email)
	decoyUnknownEmail := addProjectMember("decoy-email-2@example.com", "never-registered@example.com")

	rrProbeVictimEmail := postJoin(outsider.Email)
	rrProbeUnknownEmail := postJoin("never-registered@example.com")
	require.Equal(t, http.StatusCreated, rrProbeVictimEmail.Code,
		"displayName matching outsider email must resolve project member (no oracle); body: %s", rrProbeVictimEmail.Body.String())
	require.Equal(t, http.StatusCreated, rrProbeUnknownEmail.Code, "body: %s", rrProbeUnknownEmail.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrProbeVictimEmail.Body.Bytes(), &result))
	require.Equal(t, decoyVictimEmail.ID, result.PrincipalID)
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrProbeUnknownEmail.Body.Bytes(), &result))
	require.Equal(t, decoyUnknownEmail.ID, result.PrincipalID)

	// Re-probing when already a participant must also match (409 already_exists).
	rrProbeVictimEmailDup := postJoin(outsider.Email)
	rrProbeUnknownEmailDup := postJoin("never-registered@example.com")
	require.Equal(t, http.StatusConflict, rrProbeVictimEmailDup.Code, "body: %s", rrProbeVictimEmailDup.Body.String())
	require.Equal(t, rrProbeUnknownEmailDup.Code, rrProbeVictimEmailDup.Code)
	require.Equal(t, rrProbeUnknownEmailDup.Body.String(), rrProbeVictimEmailDup.Body.String())

	unknownUserID := api.NewUUID()
	decoyVictimUUID := addProjectMember("decoy-uuid-1@example.com", outsider.ID)
	decoyUnknownUUID := addProjectMember("decoy-uuid-2@example.com", unknownUserID)

	rrProbeVictimUUID := postJoin(outsider.ID)
	rrProbeUnknownUUID := postJoin(unknownUserID)
	require.Equal(t, http.StatusCreated, rrProbeVictimUUID.Code,
		"displayName matching outsider UUID must resolve project member (no oracle); body: %s", rrProbeVictimUUID.Body.String())
	require.Equal(t, http.StatusCreated, rrProbeUnknownUUID.Code, "body: %s", rrProbeUnknownUUID.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrProbeVictimUUID.Body.Bytes(), &result))
	require.Equal(t, decoyVictimUUID.ID, result.PrincipalID)
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrProbeUnknownUUID.Body.Bytes(), &result))
	require.Equal(t, decoyUnknownUUID.ID, result.PrincipalID)

	// 9. Suspended project member: refused identically to an unknown user by
	// ID, email, display-name slug, and email local part, and does not cause
	// ambiguity with an active project member sharing a handle.
	suspendedUser := addProjectMember("suspended@example.com", "Suspended Member")
	suspendedUser.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, suspendedUser))

	for _, ref := range []string{suspendedUser.ID, suspendedUser.Email, "suspended-member", "suspended"} {
		rrSusp := postJoin(ref)
		require.Equal(t, rrUnknownEmail.Code, rrSusp.Code, "suspended ref %q body: %s", ref, rrSusp.Body.String())
		require.Equal(t, rrUnknownEmail.Body.String(), rrSusp.Body.String(),
			"suspended project member ref %q must be indistinguishable from unknown user", ref)
	}

	activeShared := addProjectMember("sharedlocal@active.example", "Active Shared")
	suspendedShared := addProjectMember("sharedlocal@suspended.example", "Suspended Shared")
	suspendedShared.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, suspendedShared))

	rrShared := postJoin("sharedlocal")
	require.Equal(t, http.StatusCreated, rrShared.Code,
		"suspended member must not cause 409 ambiguity with active member; body: %s", rrShared.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrShared.Body.Bytes(), &result))
	require.Equal(t, activeShared.ID, result.PrincipalID)

	// 10. Group-derived project member resolves by handle (display-name slug).
	groupUser := &store.User{
		ID:          api.NewUUID(),
		Email:       "grace@example.com",
		DisplayName: "Grace Hopper",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, groupUser))
	memberGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Join Resolve Group",
		Slug:      "join-resolve-group",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   uEmail.ID,
		Created:   time.Now().UTC(),
		Updated:   time.Now().UTC(),
	}
	require.NoError(t, s.CreateGroup(ctx, memberGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    memberGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   groupUser.ID,
		Role:       store.GroupMemberRoleMember,
		AddedAt:    time.Now().UTC(),
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      memberGroup.ID,
		RoleDefinitionID: memberRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedAt:        time.Now().UTC(),
	})
	require.NoError(t, err)

	rrGroupHandle := postJoin("grace-hopper")
	require.Equal(t, http.StatusCreated, rrGroupHandle.Code,
		"group-derived project member should resolve by handle; body: %s", rrGroupHandle.Body.String())
	result = store.ConversationParticipant{}
	require.NoError(t, json.Unmarshal(rrGroupHandle.Body.Bytes(), &result))
	require.Equal(t, groupUser.ID, result.PrincipalID)
}

func TestLeaveConversation_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/leave", nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleLeaveConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusNoContent, rr.Code)

	// Verify the agent is no longer a participant.
	isParticipant, err := isConversationParticipant(context.Background(), s, conv.ID, "agent", agent.ID)
	require.NoError(t, err)
	require.False(t, isParticipant, "agent should no longer be a participant")
}

func TestLeaveConversation_NotParticipant(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	// Don't add the agent as a participant.

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/leave", nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleLeaveConversation(rr, req, conv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code)
}

// ---- Security fix tests ----

func TestHandleCreateConversation_ProjectAuthorizationDenied(t *testing.T) {
	srv, s := testServer(t)
	project, _, _ := setupConvTestData(t, s)

	// Create a separate agent in a different project — it has NO access to `project`.
	otherProject := &store.Project{
		ID:   api.NewUUID(),
		Name: "other-project",
		Slug: "other-project",
	}
	require.NoError(t, s.CreateProject(context.Background(), otherProject))

	otherAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "other-agent",
		Slug:      "other-agent",
		ProjectID: otherProject.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(context.Background(), otherAgent))

	body := createConversationRequest{
		DisplayName: "Unauthorized Conv",
		ProjectID:   project.ID, // target project the agent has no access to
		Kind:        "group",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(otherAgent.ID, otherProject.ID))
	rr := httptest.NewRecorder()
	srv.handleCreateConversation(rr, req)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"agent without project access should be denied; body: %s", rr.Body.String())
}

func TestHandleSetDefaultAgent_AgentProjectUnauthorized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a global conversation (no project).
	now := time.Now().UTC()
	globalConv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Global Conversation",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, globalConv))

	// Create a participant agent in its own project.
	participantProject := &store.Project{
		ID:   api.NewUUID(),
		Name: "participant-project",
		Slug: "participant-project",
	}
	require.NoError(t, s.CreateProject(ctx, participantProject))

	participantAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "participant-agent",
		Slug:      "participant-agent",
		ProjectID: participantProject.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, participantAgent))
	addConvParticipant(t, s, globalConv.ID, "agent", participantAgent.ID)

	// Create an agent in a project the participant has NO access to.
	restrictedProject := &store.Project{
		ID:   api.NewUUID(),
		Name: "restricted-project",
		Slug: "restricted-project",
	}
	require.NoError(t, s.CreateProject(ctx, restrictedProject))

	restrictedAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "restricted-agent",
		Slug:      "restricted-agent",
		ProjectID: restrictedProject.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, restrictedAgent))

	// Try to set the restricted agent as default — should be denied (403).
	body := setDefaultAgentRequest{AgentID: restrictedAgent.ID}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+globalConv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(participantAgent.ID, participantProject.ID))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, globalConv.ID)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"participant without access to the agent's project should be denied; body: %s", rr.Body.String())
}

func TestHandleSetDefaultAgent_CrossProjectDenied(t *testing.T) {
	srv, s := testServer(t)
	project, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)
	grantAgentProjectAccess(t, s, agent.ID, project.ID)

	// Create an agent in a DIFFERENT project.
	otherProject := &store.Project{
		ID:   api.NewUUID(),
		Name: "other-project",
		Slug: "other-project",
	}
	require.NoError(t, s.CreateProject(context.Background(), otherProject))

	crossAgent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "cross-project-agent",
		Slug:      "cross-project-agent",
		ProjectID: otherProject.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(context.Background(), crossAgent))

	body := setDefaultAgentRequest{AgentID: crossAgent.ID}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/conversations/"+conv.ID+"/default-agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContextWithScopes(agent.ID, project.ID, []AgentTokenScope{ScopeProjectRead}))
	rr := httptest.NewRecorder()
	srv.handleSetDefaultAgent(rr, req, conv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"setting a cross-project agent should be rejected; body: %s", rr.Body.String())
}
