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

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// These tests cover listing the human user who posts through a chat bridge
// (broker inbound, routed and legacy) as a participant of the group
// conversation the post lands in, mirroring the native group path.

// countParticipants returns the number of active participant rows for the
// given principal in a conversation.
func countParticipants(t *testing.T, s store.Store, conversationID, kind, principalID string) int {
	t.Helper()
	parts, err := s.ListParticipants(context.Background(), conversationID)
	require.NoError(t, err)
	n := 0
	for _, p := range parts {
		if p.PrincipalKind == kind && p.PrincipalID == principalID && p.LeftAt == nil {
			n++
		}
	}
	return n
}

// listConversationIDsAsUser calls GET /api/v1/conversations as the given user
// and returns the listed conversation IDs.
func listConversationIDsAsUser(t *testing.T, srv *Server, user *store.User) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(),
		NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")))
	rr := httptest.NewRecorder()
	srv.handleListConversations(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "list body: %s", rr.Body.String())
	var result conversationListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result))
	ids := make([]string, 0, len(result.Conversations))
	for _, c := range result.Conversations {
		ids = append(ids, c.ID)
	}
	return ids
}

// latestConversationID returns the conversation ID of the most recent
// persisted message for the agent.
func latestConversationID(t *testing.T, s store.Store, agentID string) string {
	t.Helper()
	msgs, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agentID}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items, "the message must be persisted")
	require.NotEmpty(t, msgs.Items[0].ConversationID)
	return msgs.Items[0].ConversationID
}

func routedThreadPost(env routedTestEnv, threadID, text string) routedInboundRequest {
	return routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			Sender:    "user:" + env.user.Email,
			Msg:       text,
			Type:      messages.TypeInstruction,
			ThreadID:  threadID,
		},
	}
}

func TestBrokerInboundRouted_GroupPost_ListsUserAsParticipant(t *testing.T) {
	env := setupRoutedTestEnv(t)

	rec := env.doRoutedRequest(t, routedThreadPost(env, "group-user-thread-1", "hello @beta"))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	convID := latestConversationID(t, env.store, env.agent1.ID)
	conv, err := env.store.GetConversation(context.Background(), convID)
	require.NoError(t, err)
	require.Equal(t, "group", conv.Kind)

	require.Equal(t, 1, countParticipants(t, env.store, convID, "user", env.user.ID),
		"the posting user must be listed once, even with two recipients")
	require.Equal(t, 1, countParticipants(t, env.store, convID, "agent", env.agent1.ID))
	require.Equal(t, 1, countParticipants(t, env.store, convID, "agent", env.agent2.ID))

	require.Contains(t, listConversationIDsAsUser(t, env.srv, env.user), convID,
		"the group conversation must appear in the posting user's conversation list")
}

func TestBrokerInboundRouted_GroupRepeatPost_DoesNotDuplicateUser(t *testing.T) {
	env := setupRoutedTestEnv(t)

	for _, text := range []string{"first", "second"} {
		rec := env.doRoutedRequest(t, routedThreadPost(env, "group-user-thread-2", text))
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	}

	convID := latestConversationID(t, env.store, env.agent1.ID)
	require.Equal(t, 1, countParticipants(t, env.store, convID, "user", env.user.ID))
}

func TestBrokerInboundRouted_GroupParticipantFailure_StillDelivers(t *testing.T) {
	env := setupRoutedTestEnv(t)
	env.srv.store = &failingParticipantStore{Store: env.store}

	rec := env.doRoutedRequest(t, routedThreadPost(env, "group-user-thread-3", "hello"))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.True(t, resp.Delivered)
	require.Len(t, resp.Results, 1)
	require.Equal(t, "delivered", resp.Results[0].Status)
}

func TestBrokerInboundRouted_DirectConversation_ParticipantsUnchanged(t *testing.T) {
	env := setupRoutedTestEnv(t)

	req := routedThreadPost(env, "", "hello direct")
	rec := env.doRoutedRequest(t, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	convID := latestConversationID(t, env.store, env.agent1.ID)
	conv, err := env.store.GetConversation(context.Background(), convID)
	require.NoError(t, err)
	require.Equal(t, "direct", conv.Kind)

	parts, err := env.store.ListParticipants(context.Background(), convID)
	require.NoError(t, err)
	require.Len(t, parts, 2, "a direct conversation keeps exactly its two principals")
	require.Equal(t, 1, countParticipants(t, env.store, convID, "user", env.user.ID))
	require.Equal(t, 1, countParticipants(t, env.store, convID, "agent", env.agent1.ID))
}

func legacyThreadPost(f def135Fixture, threadID, text string) *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "discord",
		Sender:    f.senderRef,
		Recipient: "agent:" + f.agent.Slug,
		Msg:       text,
		Type:      messages.TypeInstruction,
		ThreadID:  threadID,
	}
}

func TestBrokerInbound_GroupPost_ListsUserAndAgentAsParticipants(t *testing.T) {
	f := setupDEF135(t)

	for _, text := range []string{"first", "second"} {
		rec := f.sendBrokerInbound(t, legacyThreadPost(f, "legacy-group-thread-1", text), "", "", "")
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	}

	convID := latestConversationID(t, f.store, f.agent.ID)
	conv, err := f.store.GetConversation(context.Background(), convID)
	require.NoError(t, err)
	require.Equal(t, "group", conv.Kind)

	require.Equal(t, 1, countParticipants(t, f.store, convID, "user", f.user.ID),
		"the posting user must be listed exactly once across repeat posts")
	require.Equal(t, 1, countParticipants(t, f.store, convID, "agent", f.agent.ID),
		"the delivered-to agent must be listed exactly once across repeat posts")

	require.Contains(t, listConversationIDsAsUser(t, f.srv, f.user), convID,
		"the group conversation must appear in the posting user's conversation list")
}

func TestBrokerInbound_GroupPost_DeferredAgent_ListsOnlyUser(t *testing.T) {
	f := setupDEF135(t)
	ctx := context.Background()

	f.agent.ReincarnationState = store.ReincarnationStatePending
	require.NoError(t, f.store.UpdateAgent(ctx, f.agent))

	rec := f.sendBrokerInbound(t, legacyThreadPost(f, "legacy-group-thread-2", "while reincarnating"), "", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, false, resp["delivered"], "sanity: the message is deferred, not delivered")

	convID := latestConversationID(t, f.store, f.agent.ID)
	require.Equal(t, 1, countParticipants(t, f.store, convID, "user", f.user.ID))
	require.Equal(t, 0, countParticipants(t, f.store, convID, "agent", f.agent.ID),
		"an agent whose message was deferred must not be listed as a participant")
}

func TestBrokerInbound_GroupParticipantFailure_StillDelivers(t *testing.T) {
	f := setupDEF135(t)
	f.srv.store = &failingParticipantStore{Store: f.store}

	rec := f.sendBrokerInbound(t, legacyThreadPost(f, "legacy-group-thread-3", "hello"), "", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, true, resp["delivered"])
	require.Len(t, f.dispatcher.getCalls(), 1)
}
