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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// postOutboundRefOnly sends an outbound message with a conversation_ref and
// NO explicit recipient. This is the exact shape the CLI sends for conv:<uuid>,
// #<thread>, and @<agent> references (DEF-152).
func postOutboundRefOnly(t *testing.T, srv *Server, projectID, agentID, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg:             msg,
		ConversationRef: convRef,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// postOutboundNoAddressing sends an outbound message with NEITHER a recipient
// NOR a conversation_ref — this must be rejected.
func postOutboundNoAddressing(t *testing.T, srv *Server, projectID, agentID, msg string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg: msg,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// ---------------------------------------------------------------------------
// DEF-152 test 1: conv:<uuid> with NO recipient — the exact production shape.
// This is the test the suite was missing. The conversation is a direct DM
// between the sending agent and a user. The handler must resolve the ref,
// derive the addressee from the DM key, and deliver successfully.
// ---------------------------------------------------------------------------

func TestDEF152_ConvRef_NoRecipient_DirectDM(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a direct DM conversation between the sending agent and the user.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
		// ProjectID intentionally nil — DMs are global.
	})
	require.NoError(t, err)

	// Ensure participants (Resolve's post-resolution auth checks participant
	// membership for direct conversations via the DM key).
	_ = s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    agent.ID,
		Role:           "member",
	})
	_ = s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    user.ID,
		Role:           "member",
	})

	// Post with conversation_ref only — NO recipient.
	rr := postOutboundRefOnly(t, srv, project.ID, agent.ID,
		"hello via conv ref no recipient", "conv:"+conv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"conv:<uuid> with no recipient must succeed (DEF-152): %s", rr.Body.String())

	// Verify the response includes the derived recipient.
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp["recipient_id"],
		"response must include the derived recipient_id")
	require.Equal(t, user.ID, resp["recipient_id"],
		"derived recipient_id must be the user from the DM key")

	// Verify the persisted message has the correct conversation_id.
	msgID, ok := resp["message_id"].(string)
	require.True(t, ok && msgID != "")
	storedMsg, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	require.Equal(t, conv.ID, storedMsg.ConversationID,
		"persisted message must have the resolved conversation_id")
	require.Equal(t, user.ID, storedMsg.RecipientID,
		"persisted message must have the derived recipient_id")
}

// ---------------------------------------------------------------------------
// DEF-152 test 2: #<thread> with NO recipient — group conversation.
// Group conversations have no single addressee to derive. The handler must
// resolve the ref but refuse explicitly, instructing the caller to supply
// an explicit recipient alongside the conversation_ref.
// ---------------------------------------------------------------------------

func TestDEF152_ThreadRef_NoRecipient_GroupConv(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation the agent's project owns.
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d152-thread-norecip",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d152-thread-norecip",
	}
	_, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Post with conversation_ref only — NO recipient.
	rr := postOutboundRefOnly(t, srv, project.ID, agent.ID,
		"hello via thread ref no recipient", "#d152-thread-norecip")
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"#thread with no recipient for a group conversation must be refused (DEF-152): %s",
		rr.Body.String())
	assert.Contains(t, rr.Body.String(), "group conversations require an explicit recipient",
		"error must clearly explain that group conversations need an explicit recipient")
}

// ---------------------------------------------------------------------------
// DEF-152: #<thread> WITH a recipient still works — this is the existing path
// that all DEF-142 tests exercise. Verify backwards compatibility.
// ---------------------------------------------------------------------------

func TestDEF152_ThreadRef_WithRecipient_GroupConv(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d152-thread-withrecip",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d152-thread-withrecip",
	}
	_, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Use the existing helper which provides BOTH recipient and ref.
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello with recipient and thread ref", "#d152-thread-withrecip")
	require.Equal(t, http.StatusOK, rr.Code,
		"#thread with explicit recipient must still succeed")
}

// ---------------------------------------------------------------------------
// DEF-152 test 3 (negative): neither recipient nor conversation_ref → 400
// with the original error message unchanged.
// ---------------------------------------------------------------------------

func TestDEF152_NoRecipient_NoConvRef_Still400(t *testing.T) {
	srv, _, project, agent, _ := def138Setup(t)

	rr := postOutboundNoAddressing(t, srv, project.ID, agent.ID,
		"should be rejected")
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"no recipient and no conversation_ref must still be rejected")
	assert.Contains(t, rr.Body.String(), "recipient is required",
		"error message must be unchanged from the original guard")
}

// ---------------------------------------------------------------------------
// DEF-152 test 4 (negative): conv:<uuid> naming a conversation the sender
// is NOT a participant of → refused. The error must not disclose project IDs.
// ---------------------------------------------------------------------------

func TestDEF152_ConvRef_NoRecipient_NotParticipant(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	// Create a direct DM between two OTHER principals.
	otherAgentID := tid("d152-other-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:         otherAgentID,
		Name:       "d152-other-agent",
		Slug:       "d152-other-agent",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}))
	otherUserID := tid("d152-other-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          otherUserID,
		Email:       "d152-other@example.com",
		DisplayName: "Other User D152",
	}))

	dmKey, err := messages.DMConversationKey("agent", otherAgentID, "user", otherUserID)
	require.NoError(t, err)
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// The sending agent is NOT in this DM.
	rr := postOutboundRefOnly(t, srv, project.ID, agent.ID,
		"probing someone else's DM", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"conv ref to a conversation the sender is not a participant of must be refused")
	assert.Contains(t, rr.Body.String(), "could not be resolved",
		"error must use the collapsed generic message")
	assert.NotContains(t, rr.Body.String(), project.ID,
		"error must not disclose project IDs")
	assert.NotContains(t, rr.Body.String(), otherAgentID,
		"error must not disclose other participant IDs")
}

// ---------------------------------------------------------------------------
// DEF-152: verify that the existing postOutboundWithRef helper (from DEF-142
// tests) still works WITH a recipient — backwards compatibility.
// ---------------------------------------------------------------------------

func TestDEF152_ConvRef_WithRecipient_StillWorks(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d152-compat",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d152-compat",
	}
	_, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Use the existing helper which provides BOTH recipient and ref.
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"hello with recipient and ref", "#d152-compat")
	require.Equal(t, http.StatusOK, rr.Code,
		"providing both recipient and conversation_ref must still work")
}
