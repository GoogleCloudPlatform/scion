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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// This file confirms that the DEF-161 direct-conversation recipient check
// (see handlers_outbound_def160_test.go's DEF-161 AC-6 tests, which cover
// the conversation_ref path) also runs when the caller asserts the same
// direct conversation with a raw conversation_id instead of a
// conversation_ref. Both are ways of naming the same conversation and must
// be validated the same way.
// ---------------------------------------------------------------------------

// postOutboundRequest sends a prepared OutboundMessageRequest for agentID.
// Shared by the raw conversation_id and conversation_ref with-thread cases
// below (they differ only in which fields of OutboundMessageRequest are
// set), so the two paths can't drift apart in how the request is built.
func postOutboundRequest(t *testing.T, srv *Server, projectID, agentID string, r OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(r)
	require.NoError(t, err)
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

// setupWebChannelBroker registers a "web" channel on srv so requests carrying
// Channel:"web" pass validateChannelRegistered. Mirrors the broker portion of
// def158BrokerSetup, without the WebChatStore/read-switch machinery this file
// doesn't need.
func setupWebChannelBroker(t *testing.T, srv *Server, s store.Store, project *store.Project) {
	t.Helper()
	inprocessBus := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocessBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)

	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	proxy.subscribeProjectUserMessages(project.ID)
}

// ---------------------------------------------------------------------------
// Raw conversation_id, mismatched recipient: rejected the same way as the
// conversation_ref path (see TestDEF161_AC6_DirectConvRef_RecipientNotInDMKey_Rejected).
// ---------------------------------------------------------------------------

func TestDirectRawConversationID_RecipientNotInDMKey_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a direct DM between the sending agent and a DIFFERENT user.
	// The test's `user` (from def138Setup) is NOT in this DM key.
	otherUser := &store.User{
		ID:          tid("convid-recip-other-user"),
		Email:       "convid-recip-other@example.com",
		DisplayName: "Other ConvID Recipient User",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", otherUser.ID)
	require.NoError(t, err)

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// The sender (agent) IS in the DM key, so the sender-side check (DEF-138)
	// passes. The supplied recipient (user) is NOT in the DM key, so the
	// recipient-side check must reject it — same as the conversation_ref path.
	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email,
		"should be rejected", dmConv.ID)
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"raw conversation_id: recipient not in DM key must be rejected 400: %s",
		rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, "remove the recipient",
		"error must name the remediation, matching the conversation_ref path")

	// The response must not enumerate the DM participants.
	assert.NotContains(t, body, agent.ID,
		"response must not contain sender participant ID")
	assert.NotContains(t, body, otherUser.ID,
		"response must not contain other participant ID")
	assert.NotContains(t, body, user.ID,
		"response must not contain the supplied recipient ID")

	// A rejection must leave no trace: no message row persisted or delivered
	// for the sending agent.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

// ---------------------------------------------------------------------------
// Raw conversation_id, recipient IS in the DM key: accepted.
// ---------------------------------------------------------------------------

func TestDirectRawConversationID_RecipientInDMKey_Accepted(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email,
		"should succeed", dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"raw conversation_id: recipient in DM key must succeed: %s",
		rr.Body.String())
}

// ---------------------------------------------------------------------------
// DM thread_id case: the message's thread_id names a thread that matches
// the supplied recipient (so the pre-existing thread_id/recipient ownership
// check passes), but the conversation being asserted (by raw conversation_id
// or by conversation_ref) is a DIFFERENT direct conversation whose DM key
// does not include that recipient. The dm: thread_id consistency check
// (ptone/scion#2211, checkDirectThreadIDMatchesDMKey) runs first and rejects
// the request before the recipient check; the test pins a 400 on both
// paths. See also TestDirectConversation_ThreadIDMismatch_Rejected_RawConvID
// (handlers_outbound_dm_thread_recipient_test.go), which isolates the
// thread_id check on its own.
// ---------------------------------------------------------------------------

func TestDirectConversation_ThreadID_RecipientMismatch_SameOnBothPaths(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)

	otherUser := &store.User{
		ID:          tid("convid-thread-other-user"),
		Email:       "convid-thread-other@example.com",
		DisplayName: "Other ConvID Thread User",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	// The conversation actually being asserted is a DM between the agent
	// and otherUser — it does not include `user`.
	convDMKey, err := messages.DMConversationKey("agent", agent.ID, "user", otherUser.ID)
	require.NoError(t, err)

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: convDMKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// The thread_id names a DM key between the agent and `user` — this
	// matches the supplied recipient, so the pre-existing thread_id/recipient
	// ownership check (S1) passes and does not mask the DEF-161 check.
	threadDMKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	// Raw conversation_id (naming the agent/otherUser DM) + thread_id (naming
	// the agent/user DM) + recipient user.Email.
	rawRR := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:      "user:" + user.Email,
		Msg:            "raw with thread",
		ConversationID: dmConv.ID,
		ThreadID:       threadDMKey,
		Channel:        "web",
	})
	require.Equal(t, http.StatusBadRequest, rawRR.Code,
		"raw conversation_id + thread_id: mismatched recipient must be rejected: %s",
		rawRR.Body.String())

	// conversation_ref (same conversation) + same thread_id + same recipient.
	refRR := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:       "user:" + user.Email,
		Msg:             "ref with thread",
		ConversationRef: "conv:" + dmConv.ID,
		ThreadID:        threadDMKey,
		Channel:         "web",
	})
	require.Equal(t, http.StatusBadRequest, refRR.Code,
		"conversation_ref + thread_id: mismatched recipient must be rejected: %s",
		refRR.Body.String())

	// Both paths must produce the same status and the same error body.
	assert.Equal(t, refRR.Code, rawRR.Code,
		"raw conversation_id and conversation_ref must reject identically when a thread_id is present")
	assert.Equal(t, refRR.Body.String(), rawRR.Body.String(),
		"raw conversation_id and conversation_ref must produce the same error body when a thread_id is present")

	// Both rejections must leave no trace: no message row persisted or
	// delivered for the sending agent.
	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message, on either path")
}

// ---------------------------------------------------------------------------
// Non-DM (group) conversations are unaffected: a raw conversation_id
// pointing at a group conversation, with a caller-supplied recipient, is
// accepted as supplied (200), not rejected — the new direct-conversation
// check must not apply to group conversations on the raw path.
// ---------------------------------------------------------------------------

func TestGroupRawConversationID_RecipientSupplied_Unaffected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: "thread:" + project.ID + ":convid-recip-group-topic",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email,
		"group message with recipient", created.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"raw conversation_id for a group conversation must be unaffected by the direct-conversation recipient check: %s",
		rr.Body.String())
}
