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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// setupConvTestData creates a project, agent, and conversation for testing.
func setupConvTestData(t *testing.T, s store.Store) (project *store.Project, agent *store.Agent, conv *store.Conversation) {
	t.Helper()
	ctx := context.Background()

	project = &store.Project{
		ID:   api.NewUUID(),
		Name: "conv-test-project",
		Slug: "conv-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent = &store.Agent{
		ID:         api.NewUUID(),
		Name:       "conv-test-agent",
		Slug:       "conv-test-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	now := time.Now().UTC()
	conv = &store.Conversation{
		ID:             api.NewUUID(),
		ProjectID:      &project.ID,
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "Test Conversation",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	return project, agent, conv
}

// addConvParticipant adds a participant to a conversation for testing.
func addConvParticipant(t *testing.T, s store.Store, convID, principalKind, principalID string) {
	t.Helper()
	p := &store.ConversationParticipant{
		ID:             api.NewUUID(),
		ConversationID: convID,
		PrincipalKind:  principalKind,
		PrincipalID:    principalID,
		Role:           "member",
		JoinedAt:       time.Now().UTC(),
	}
	require.NoError(t, s.AddParticipant(context.Background(), p))
}

// agentContext returns a context with an agent identity set.
func agentContext(agentID, projectID string) context.Context {
	return contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}})
}

// agentContextWithScopes returns a context with an agent identity that includes
// the given JWT scopes. Use this when calling endpoints that check authorization
// via s.authorize (e.g., project-level authz in handleCreateConversation).
func agentContextWithScopes(agentID, projectID string, scopes []AgentTokenScope) context.Context {
	return contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    scopes,
	}})
}

// convProjectID extracts the project ID from a conversation, returning empty if nil.
func convProjectID(c *store.Conversation) string {
	if c.ProjectID != nil {
		return *c.ProjectID
	}
	return ""
}

// grantAgentProjectAccess grants an agent a project-member role binding,
// giving it read access to the project. This is needed after the BOLA fix
// added an authorize check in handleCreateConversation.
func grantAgentProjectAccess(t *testing.T, s store.Store, agentID, projectID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err, "project-member role definition not found")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "agent",
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// wireSharedWebChatStore wires a WebChatStore onto srv that shares the test
// store's underlying SQLite DB. Group creation now routes through
// WebChatStore.CreateTopic (chat-thread-bridge), so every test that creates
// a group conversation via the HTTP handler needs one wired — otherwise the
// handler returns 503. Sharing the DB is load-bearing: CreateTopic's
// dual-write must land in the same "conversations" table that
// store.GetConversation reads back from.
func wireSharedWebChatStore(t *testing.T, srv *Server, s store.Store) WebChatStore {
	t.Helper()
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store does not expose DB()")
	rawDB := dbProvider.DB()
	require.NotNil(t, rawDB, "store DB() returned nil")

	wcs := NewWebChatStore(rawDB, "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)
	return wcs
}

// ---- Tests ----

func TestListConversations_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	_, agent, conv := setupConvTestData(t, s)
	addConvParticipant(t, s, conv.ID, "agent", agent.ID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
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

	// Filter by surface=native — should only return the first conversation.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?surface=native", nil)
	req = req.WithContext(agentContext(agent.ID, project.ID))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	require.Len(t, result.Conversations, 1)
	require.Equal(t, conv.ID, result.Conversations[0].ID)

	// Filter by surface=discord — should only return the second conversation.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/conversations?surface=discord", nil)
	req = req.WithContext(agentContext(agent.ID, project.ID))
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
	req = req.WithContext(agentContext(agent.ID, project.ID))
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
		ID:         api.NewUUID(),
		Name:       "get-message-dm-agent-b",
		Slug:       "get-message-dm-agent-b",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
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
		ID:         api.NewUUID(),
		Name:       "new-default-agent",
		Slug:       "new-default-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
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
	_, _, conv := setupConvTestData(t, s)

	// Create a user identity and add as participant.
	userID := api.NewUUID()
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
		ID:         api.NewUUID(),
		Name:       "soft-deleted-agent",
		Slug:       "soft-deleted-agent",
		ProjectID:  project.ID,
		Phase:      "terminated",
		DeletedAt:  time.Now().UTC(),
		Visibility: store.VisibilityPrivate,
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
		ProjectID: project.ID, Phase: "running", Visibility: store.VisibilityPrivate,
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
		ProjectID: project.ID, Phase: "running", Visibility: store.VisibilityPrivate,
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
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
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
		ID:         api.NewUUID(),
		Name:       "new-participant",
		Slug:       "new-participant",
		ProjectID:  *conv.ProjectID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), newAgent))

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   newAgent.ID,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
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

	require.Equal(t, http.StatusForbidden, rr.Code)
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
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
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
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
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
		ID:         api.NewUUID(),
		Name:       "cross-project-agent",
		Slug:       "cross-project-agent",
		ProjectID:  otherProject.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(context.Background(), crossProjectAgent))

	body := addParticipantRequest{
		PrincipalKind: "agent",
		PrincipalID:   crossProjectAgent.ID,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/"+conv.ID+"/participants", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
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
	req = req.WithContext(agentContext(agent.ID, convProjectID(conv)))
	rr := httptest.NewRecorder()
	srv.handleAddParticipant(rr, req, conv.ID)

	require.Equal(t, http.StatusNotFound, rr.Code)
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
		ID:         api.NewUUID(),
		Name:       "other-agent",
		Slug:       "other-agent",
		ProjectID:  otherProject.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
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
		ID:         api.NewUUID(),
		Name:       "participant-agent",
		Slug:       "participant-agent",
		ProjectID:  participantProject.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
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
		ID:         api.NewUUID(),
		Name:       "restricted-agent",
		Slug:       "restricted-agent",
		ProjectID:  restrictedProject.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
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
		ID:         api.NewUUID(),
		Name:       "cross-project-agent",
		Slug:       "cross-project-agent",
		ProjectID:  otherProject.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
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
