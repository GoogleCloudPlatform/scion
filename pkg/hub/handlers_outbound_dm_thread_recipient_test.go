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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// This file covers two direct-conversation consistency checks that apply on
// both ways a caller can assert an existing direct conversation — a resolved
// conversation_ref and a raw conversation_id:
//
//  1. a dm:-prefixed thread_id must match the conversation's own DM key
//     (ptone/scion#2211);
//  2. an explicit recipient must match the DM key's non-sender participant
//     by ID, and by kind when the recipient carries one, not merely appear
//     somewhere in the key (ptone/scion#2212).
//
// Every scenario below is exercised on both paths using postOutboundRequest
// (conv_id_recipient_test.go), so the two can't silently drift apart.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ptone/scion#2211: dm: thread_id must match the asserted conversation's own key.
// ---------------------------------------------------------------------------

// TestDirectConversation_ThreadIDMismatch_NoRecipient_Rejected_ConvRef covers
// the case a pre-existing check (the S1 thread_id/recipient ownership check)
// cannot catch: no explicit recipient is supplied at all, so that check never
// runs, yet the caller's dm: thread_id still names a different conversation
// than the one just asserted by conversation_ref.
func TestDirectConversation_ThreadIDMismatch_NoRecipient_Rejected_ConvRef(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)

	otherUser := &store.User{
		ID:          tid("thread-mismatch-other-user"),
		Email:       "thread-mismatch-other@example.com",
		DisplayName: "Other Thread Mismatch User",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	// The asserted conversation is a DM between the agent and otherUser.
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "user", otherUser.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	// thread_id names a DIFFERENT DM — agent and `user` — not the DM being
	// asserted. No recipient is supplied, so S5 derives the addressee from
	// the conversation itself (otherUser); the thread_id mismatch must still
	// be caught.
	mismatchedThreadID := mustDMKey(t, "agent", agent.ID, "user", user.ID)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Msg:             "no recipient, mismatched thread_id",
		ConversationRef: "conv:" + dmConv.ID,
		ThreadID:        mismatchedThreadID,
		Channel:         "web",
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"conversation_ref: a dm: thread_id that does not match the conversation's key must be rejected: %s",
		rr.Body.String())
	assert.Contains(t, rr.Body.String(), "does not match the direct conversation's key",
		"error must name the mismatch")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

// TestDirectConversation_ThreadIDMismatch_Rejected_RawConvID is the raw
// conversation_id counterpart. Asserting a conversation_id with no recipient
// at all is already rejected earlier (the pre-existing "recipient is
// required" guard applies whenever conversation_ref is empty, regardless of
// conversation_id) — that is unrelated to ptone/scion#2211/ptone/scion#2212 and out of scope here.
//
// To isolate the new ptone/scion#2211 thread_id check from the pre-existing S1
// thread_id/recipient ownership check (handlers_agent_messaging.go, the
// "DM thread_id does not match the sender and recipient" check), the
// recipient supplied is otherUser — the participant the thread_id's OWN key
// names — so that older check passes. The asserted conversation_id, however,
// names a different direct conversation (agent/user), so only the new
// conversation-vs-thread_id check can reject this request.
func TestDirectConversation_ThreadIDMismatch_Rejected_RawConvID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()
	setupWebChannelBroker(t, srv, s, project)

	otherUser := &store.User{
		ID:          tid("thread-mismatch-raw-other-user"),
		Email:       "thread-mismatch-raw-other@example.com",
		DisplayName: "Other Thread Mismatch Raw User",
	}
	require.NoError(t, s.CreateUser(ctx, otherUser))

	// The asserted conversation is a DM between the agent and `user`.
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "user", user.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	// thread_id names a DIFFERENT DM — agent and otherUser — and the
	// recipient supplied matches THAT key (satisfying the older S1 check),
	// not the asserted conversation.
	mismatchedThreadID := mustDMKey(t, "agent", agent.ID, "user", otherUser.ID)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:      "user:" + otherUser.Email,
		RecipientID:    otherUser.ID,
		Msg:            "recipient matches thread_id, not the asserted conversation",
		ConversationID: dmConv.ID,
		ThreadID:       mismatchedThreadID,
		Channel:        "web",
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"raw conversation_id: a dm: thread_id that does not match the conversation's key must be rejected: %s",
		rr.Body.String())
	assert.Contains(t, rr.Body.String(), "does not match the direct conversation's key",
		"error must name the mismatch")

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

// TestDirectConversation_ThreadIDMatchesDMKey_Accepted_ConvRef and its raw
// counterpart below confirm a dm: thread_id that matches the asserted
// conversation's own key is accepted, exactly as before ptone/scion#2211.
func TestDirectConversation_ThreadIDMatchesDMKey_Accepted_ConvRef(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	setupWebChannelBroker(t, srv, s, project)

	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	dmConv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Msg:             "matching thread_id",
		ConversationRef: "conv:" + dmConv.ID,
		ThreadID:        dmKey,
		Channel:         "web",
	})
	require.Equal(t, http.StatusOK, rr.Code,
		"conversation_ref: a dm: thread_id matching the conversation's key must be accepted: %s",
		rr.Body.String())
}

func TestDirectConversation_ThreadIDMatchesDMKey_Accepted_RawConvID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	setupWebChannelBroker(t, srv, s, project)

	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	dmConv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:      "user:" + user.Email,
		Msg:            "matching thread_id",
		ConversationID: dmConv.ID,
		ThreadID:       dmKey,
		Channel:        "web",
	})
	require.Equal(t, http.StatusOK, rr.Code,
		"raw conversation_id: a dm: thread_id matching the conversation's key must be accepted: %s",
		rr.Body.String())
}

// TestDirectConversation_EmptyThreadID_StillBackfilled_ConvRef and its raw
// counterpart confirm an empty thread_id is still backfilled to the
// conversation's DM key, unaffected by the new ptone/scion#2211 check (a no-op on empty
// thread_id).
func TestDirectConversation_EmptyThreadID_StillBackfilled_ConvRef(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"empty thread_id", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"conversation_ref: an empty thread_id must still be accepted and backfilled: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items)
	assert.Equal(t, dmKey, msgs.Items[len(msgs.Items)-1].ThreadID,
		"empty thread_id must still be backfilled to the DM key")
}

func TestDirectConversation_EmptyThreadID_StillBackfilled_RawConvID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	dmKey := mustDMKey(t, "agent", agent.ID, "user", user.ID)
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email,
		"empty thread_id", dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"raw conversation_id: an empty thread_id must still be accepted and backfilled: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, msgs.Items)
	assert.Equal(t, dmKey, msgs.Items[len(msgs.Items)-1].ThreadID,
		"empty thread_id must still be backfilled to the DM key")
}

// ---------------------------------------------------------------------------
// ptone/scion#2212: an explicit recipient must match the non-sender participant by
// ID, and by kind when the recipient carries one.
// ---------------------------------------------------------------------------

// TestDirectConversation_RecipientIsSender_Rejected_ConvRef and its raw
// counterpart cover a recipient equal to the sender's own ID — previously
// accepted because the old check only asked "is this ID in the key
// somewhere", and the sender's own ID always is. The DM here is agent-to-
// agent (both sides share the "agent" kind) specifically so that the
// recipient's kind ("agent") matches the non-sender participant's kind too —
// isolating the "own ID" case from a kind mismatch, which is covered
// separately below.
func TestDirectConversation_RecipientIsSender_Rejected_ConvRef(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	targetAgent := &store.Agent{
		ID:        tid("recip-is-sender-target-agent"),
		Name:      "recip-is-sender-target-agent",
		Slug:      "recip-is-sender-target-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "agent", targetAgent.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:       "agent:" + agent.Slug,
		RecipientID:     agent.ID, // the sender's own ID, same kind as targetAgent
		Msg:             "recipient is sender",
		ConversationRef: "conv:" + dmConv.ID,
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"conversation_ref: a recipient equal to the sender's own ID must be rejected: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

func TestDirectConversation_RecipientIsSender_Rejected_RawConvID(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	targetAgent := &store.Agent{
		ID:        tid("recip-is-sender-raw-target-agent"),
		Name:      "recip-is-sender-raw-target-agent",
		Slug:      "recip-is-sender-raw-target-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "agent", targetAgent.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:      "agent:" + agent.Slug,
		RecipientID:    agent.ID, // the sender's own ID, same kind as targetAgent
		Msg:            "recipient is sender",
		ConversationID: dmConv.ID,
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"raw conversation_id: a recipient equal to the sender's own ID must be rejected: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

// TestDirectConversation_RecipientRightIDWrongKind_Rejected_ConvRef and its
// raw counterpart cover a recipient whose ID matches the non-sender
// participant but whose kind does not — previously accepted because the old
// check compared ID only.
func TestDirectConversation_RecipientRightIDWrongKind_Rejected_ConvRef(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// DM key's non-sender participant is (user, user.ID).
	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "user", user.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:       "agent:not-a-real-agent", // right ID, wrong kind
		RecipientID:     user.ID,
		Msg:             "right id wrong kind",
		ConversationRef: "conv:" + dmConv.ID,
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"conversation_ref: the right ID under the wrong kind must be rejected: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

func TestDirectConversation_RecipientRightIDWrongKind_Rejected_RawConvID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "user", user.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:      "agent:not-a-real-agent", // right ID, wrong kind
		RecipientID:    user.ID,
		Msg:            "right id wrong kind",
		ConversationID: dmConv.ID,
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"raw conversation_id: the right ID under the wrong kind must be rejected: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

// TestDirectConversation_CorrectNonSenderRecipient_Accepted_ConvRef and its
// raw counterpart confirm the correct non-sender recipient (by ID, and by
// kind when the recipient carries one) is still accepted.
func TestDirectConversation_CorrectNonSenderRecipient_Accepted_ConvRef(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "user", user.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:       "user:" + user.Email,
		RecipientID:     user.ID,
		Msg:             "correct non-sender recipient",
		ConversationRef: "conv:" + dmConv.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code,
		"conversation_ref: the correct non-sender recipient must be accepted: %s",
		rr.Body.String())
}

func TestDirectConversation_CorrectNonSenderRecipient_Accepted_RawConvID(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "user", user.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:      "user:" + user.Email,
		RecipientID:    user.ID,
		Msg:            "correct non-sender recipient",
		ConversationID: dmConv.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code,
		"raw conversation_id: the correct non-sender recipient must be accepted: %s",
		rr.Body.String())
}

// TestDirectConversation_KindlessRecipientID_PeerAgent_Accepted_ConvRef and
// its raw counterpart cover a recipient supplied as recipient_id only (no
// Recipient string, so it carries no kind) on an agent-agent DM. The
// non-sender participant is the peer agent; a kindless recipient_id naming
// that peer must still be accepted — it cannot be "the wrong kind" when it
// carries none. Dispatch itself then fails for an unrelated reason (no
// runtime broker configured for the fixture agent), which pins that the
// recipient check let the request through rather than rejecting it.
func TestDirectConversation_KindlessRecipientID_PeerAgent_Accepted_ConvRef(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	peerAgent := &store.Agent{
		ID:        tid("kindless-recip-peer-agent"),
		Name:      "kindless-recip-peer-agent",
		Slug:      "kindless-recip-peer-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, peerAgent))

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "agent", peerAgent.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		RecipientID:     peerAgent.ID, // no Recipient string supplied — no kind
		Msg:             "kindless recipient_id naming the peer",
		ConversationRef: "conv:" + dmConv.ID,
	})
	require.NotEqual(t, http.StatusBadRequest, rr.Code,
		"conversation_ref: a kindless recipient_id naming the non-sender peer must not be rejected as a mismatch: %s",
		rr.Body.String())
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code,
		"the request must fail only at dispatch (no runtime broker), not at the recipient check: %s",
		rr.Body.String())
	assert.Equal(t, ErrCodeDeliveryFailed, decodeErrorCode(t, rr.Body.Bytes()))
	assert.Contains(t, rr.Body.String(), "no_runtime_broker")
}

func TestDirectConversation_KindlessRecipientID_PeerAgent_Accepted_RawConvID(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	peerAgent := &store.Agent{
		ID:        tid("kindless-recip-raw-peer-agent"),
		Name:      "kindless-recip-raw-peer-agent",
		Slug:      "kindless-recip-raw-peer-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, peerAgent))

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "agent", peerAgent.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		RecipientID:    peerAgent.ID, // no Recipient string supplied — no kind
		Msg:            "kindless recipient_id naming the peer",
		ConversationID: dmConv.ID,
	})
	require.NotEqual(t, http.StatusBadRequest, rr.Code,
		"raw conversation_id: a kindless recipient_id naming the non-sender peer must not be rejected as a mismatch: %s",
		rr.Body.String())
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code,
		"the request must fail only at dispatch (no runtime broker), not at the recipient check: %s",
		rr.Body.String())
	assert.Equal(t, ErrCodeDeliveryFailed, decodeErrorCode(t, rr.Body.Bytes()))
	assert.Contains(t, rr.Body.String(), "no_runtime_broker")
}

// TestDirectConversation_KindlessRecipientID_Sender_Rejected_ConvRef and its
// raw counterpart cover a recipient supplied as recipient_id only equal to
// the sender's own ID. A kindless recipient still carries an ID, and that ID
// must still be compared against the non-sender participant — omitting the
// kind must not also disable the ID check.
func TestDirectConversation_KindlessRecipientID_Sender_Rejected_ConvRef(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	peerAgent := &store.Agent{
		ID:        tid("kindless-recip-sender-peer-agent"),
		Name:      "kindless-recip-sender-peer-agent",
		Slug:      "kindless-recip-sender-peer-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, peerAgent))

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "agent", peerAgent.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		RecipientID:     agent.ID, // the sender's own ID, no Recipient string
		Msg:             "kindless recipient_id naming the sender",
		ConversationRef: "conv:" + dmConv.ID,
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"conversation_ref: a kindless recipient_id equal to the sender's own ID must still be rejected: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

func TestDirectConversation_KindlessRecipientID_Sender_Rejected_RawConvID(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	peerAgent := &store.Agent{
		ID:        tid("kindless-recip-sender-raw-peer-agent"),
		Name:      "kindless-recip-sender-raw-peer-agent",
		Slug:      "kindless-recip-sender-raw-peer-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, peerAgent))

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: mustDMKey(t, "agent", agent.ID, "agent", peerAgent.ID),
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		RecipientID:    agent.ID, // the sender's own ID, no Recipient string
		Msg:            "kindless recipient_id naming the sender",
		ConversationID: dmConv.ID,
	})
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"raw conversation_id: a kindless recipient_id equal to the sender's own ID must still be rejected: %s",
		rr.Body.String())

	msgs, err := s.ListMessages(ctx, store.MessageFilter{SenderID: agent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "a rejected send must not persist a message")
}

// ---------------------------------------------------------------------------
// Group (non-direct) conversations are unaffected by both checks.
// ---------------------------------------------------------------------------

// TestGroupConvRef_RecipientSupplied_Unaffected is the conversation_ref
// counterpart of TestGroupRawConversationID_RecipientSupplied_Unaffected
// (handlers_outbound_conv_id_recipient_test.go): a caller-supplied recipient
// on a group conv-ref is discarded (pre-existing DEF-161 group behaviour),
// not rejected by the new direct-conversation checks.
func TestGroupConvRef_RecipientSupplied_Unaffected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: "thread:" + project.ID + ":dm-thread-recipient-kind-group-topic",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postOutboundRequest(t, srv, project.ID, agent.ID, OutboundMessageRequest{
		Recipient:       "user:" + user.Email,
		RecipientID:     user.ID,
		Msg:             "group message with recipient",
		ConversationRef: "conv:" + created.ID,
	})
	require.Equal(t, http.StatusOK, rr.Code,
		"conversation_ref for a group conversation must be unaffected by the direct-conversation checks: %s",
		rr.Body.String())
}

// mustDMKey builds a DM key or fails the test.
func mustDMKey(t *testing.T, kindA, idA, kindB, idB string) string {
	t.Helper()
	key, err := messages.DMConversationKey(kindA, idA, kindB, idB)
	require.NoError(t, err)
	return key
}
