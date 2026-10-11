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

// Tests for the p2a UAT-fix round (report-7-gteam-2a, Amendment A25.6),
// findings F1 and F3: the hub's 1:1 (no-ThreadID) DM resolution path
// (handleAgentMessage -> messaging.DeriveConversationKey ->
// messaging.ResolveOrCreateConversationByKey) never registered
// conversation_participants rows, so `conversation list` could not
// discover a deferred conversation even though the message itself was
// correctly persisted. Fixed by teaching the shared sink
// (ResolveOrCreateConversationByKey) to register both principals named in
// a kind=="direct" dm: external_ref, via a new WithParticipants option
// (G2 non-fatal semantics, mirroring ResolveOrCreateDMConversation).
//
// Per A25.6: cover user->agent and agent->agent no-thread DMs, each both
// non-gated (recipient running normally) and deferred (recipient
// reincarnating), asserting both principals are registered participants
// and that the conversation is discoverable via GetConversationsForPrincipal
// for the recipient agent (the store-level equivalent of `conversation
// list`, cited directly in report-7-gteam-2a F1's root-cause section).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertBothParticipants asserts that conversationID has exactly the two
// named principals as active participants (order-independent), and that
// GetConversationsForPrincipal for the recipient agent returns it — the
// concrete regression report-7-gteam-2a F1 observed: `conversation list`
// returning [] for a conversation the recipient was a genuine party to.
func assertBothParticipants(t *testing.T, s store.Store, conversationID string, aKind, aID, bKind, bID string) {
	t.Helper()
	ctx := context.Background()

	parts, err := s.ListParticipants(ctx, conversationID)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, p := range parts {
		got[p.PrincipalKind+":"+p.PrincipalID] = true
	}
	assert.True(t, got[aKind+":"+aID], "expected participant %s:%s, got %v", aKind, aID, got)
	assert.True(t, got[bKind+":"+bID], "expected participant %s:%s, got %v", bKind, bID, got)

	// Discoverability: whichever side is an agent must be able to find the
	// conversation via the principal-keyed listing query (F1's root cause
	// citation of GetConversationsForPrincipal).
	for _, side := range []struct{ kind, id string }{{aKind, aID}, {bKind, bID}} {
		if side.kind != "agent" {
			continue
		}
		convs, err := s.GetConversationsForPrincipal(ctx, "agent", side.id)
		require.NoError(t, err)
		found := false
		for _, c := range convs {
			if c.ID == conversationID {
				found = true
				break
			}
		}
		assert.True(t, found, "GetConversationsForPrincipal(agent, %s) must list conversation %s", side.id, conversationID)
	}
}

// ---------------------------------------------------------------------------
// F1: user -> agent, no-thread DM
// ---------------------------------------------------------------------------

func TestHandleAgentMessage_A256_F1_UserToAgent_NotGated_RegistersBothParticipants(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseRunning))
	srv.SetDispatcher(&brokerMockDispatcher{})

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/message", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	msg, err := s.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	require.NotEmpty(t, msg.ConversationID, "the send must resolve a conversation")

	assertBothParticipants(t, s, msg.ConversationID, "user", DevUserID, "agent", agentID)
}

func TestHandleAgentMessage_A256_F1_UserToAgent_Deferred_RegistersBothParticipants(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseStopping))

	agent, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	agent.ReincarnationState = store.ReincarnationStateStopping
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	srv.SetDispatcher(&brokerMockDispatcher{})

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello during migration", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/message", body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, "deferred", resp.Status)

	msg, err := s.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	require.Equal(t, store.MessageDispatchDeferred, msg.DispatchState)
	require.NotEmpty(t, msg.ConversationID, "a deferred send must still resolve a conversation")

	assertBothParticipants(t, s, msg.ConversationID, "user", DevUserID, "agent", agentID)
}

// ---------------------------------------------------------------------------
// F3: agent -> agent, no-thread DM
// ---------------------------------------------------------------------------

func TestHandleAgentMessage_A256_F3_AgentToAgent_NotGated_RegistersBothParticipants(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()
	_ = dispatcher

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "agent:" + sender.Slug, Recipient: "agent:" + target.Slug,
			Msg: "hello from a peer agent", Type: messages.TypeInstruction,
		},
	}
	req := reincarnateRequest(t, target.ID, agentIdentityFor(sender.ID, sender.ProjectID), body)
	rec := httptest.NewRecorder()
	srv.handleAgentMessage(rec, req, target.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	msg, err := s.GetMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	require.NotEmpty(t, msg.ConversationID, "the send must resolve a conversation")

	assertBothParticipants(t, s, msg.ConversationID, "agent", sender.ID, "agent", target.ID)
}

func TestHandleAgentMessage_A256_F3_AgentToAgent_Deferred_RegistersBothParticipants(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	target.ReincarnationState = store.ReincarnationStateStarting
	target.Phase = string(state.PhaseStarting)
	require.NoError(t, s.UpdateAgent(ctx, target))

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "agent:" + sender.Slug, Recipient: "agent:" + target.Slug,
			Msg: "hello from a peer agent, mid-migration", Type: messages.TypeInstruction,
		},
	}
	req := reincarnateRequest(t, target.ID, agentIdentityFor(sender.ID, sender.ProjectID), body)
	rec := httptest.NewRecorder()
	srv.handleAgentMessage(rec, req, target.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Equal(t, "deferred", resp.Status)
	assert.Empty(t, dispatcher.calls, "no dispatch call may be made to a migrating target")

	msg, err := s.GetMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	require.Equal(t, store.MessageDispatchDeferred, msg.DispatchState)
	require.NotEmpty(t, msg.ConversationID, "a deferred send must still resolve a conversation")

	assertBothParticipants(t, s, msg.ConversationID, "agent", sender.ID, "agent", target.ID)
}
