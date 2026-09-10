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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// !no_sqlite dependency: this file depends on def138Setup (handlers_outbound_
// def138_test.go), postOutboundRefOnly (handlers_outbound_def152_test.go),
// postOutboundWithRef (handlers_outbound_def142_test.go), and
// postConvRefNoRecipient (handlers_outbound_def158_test.go). All of those
// helpers are in files tagged !no_sqlite because they call testServer/testStore
// which open an in-process sqlite database. Removing the tag from this file
// would make these 8+ tests visible to `make test-fast` (-tags no_sqlite)
// where the helpers do not compile. The tag is load-bearing.

// ---------------------------------------------------------------------------
// DEF-160 AC-1: conv:<group-uuid> with NO recipient delivers 200.
// ---------------------------------------------------------------------------

func TestDEF160_AC1_GroupConvRef_NoRecipient_Delivers200(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d160-ac1-topic",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postOutboundRefOnly(t, srv, project.ID, agent.ID,
		"AC-1 group message", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"AC-1: conv:<group-uuid> with no recipient must succeed: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// DEF-160 AC-3: stored row has non-empty Channel, ThreadID, and
// recipientID == threadKey.
// ---------------------------------------------------------------------------

func TestDEF160_AC3_StoredRow_ChannelThreadIDRecipient(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d160-ac3-topic",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postOutboundRefOnly(t, srv, project.ID, agent.ID,
		"AC-3 row shape", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code)

	result, err := s.ListMessages(ctx, store.MessageFilter{ConversationID: created.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1, "exactly one message should be stored")

	msg := result.Items[0]
	assert.Equal(t, "d160-ac3-topic", msg.RecipientID,
		"AC-3: recipientID must be the topic key")
	assert.Equal(t, "thread:d160-ac3-topic", msg.Recipient,
		"AC-3: recipient must be thread:<key>")
	assert.NotEmpty(t, msg.Channel,
		"AC-3: Channel must be non-empty")
	assert.Equal(t, "web", msg.Channel,
		"AC-3: Channel must be 'web' for native surface")
	assert.Equal(t, "d160-ac3-topic", msg.ThreadID,
		"AC-3: ThreadID must be the topic key")
}

// ---------------------------------------------------------------------------
// DEF-160 AC-5: a supplied user recipient on a group conv-ref is discarded;
// the row still carries recipientID == threadKey.
// ---------------------------------------------------------------------------

func TestDEF160_AC5_SuppliedRecipient_Discarded(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d160-ac5-topic",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Send WITH an explicit user recipient alongside the conv-ref.
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"AC-5 discard recipient", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"AC-5: group conv-ref with explicit recipient must succeed: %s", rr.Body.String())

	// The stored row must have recipientID == threadKey, not the user.
	result, err := s.ListMessages(ctx, store.MessageFilter{ConversationID: created.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)

	msg := result.Items[0]
	assert.Equal(t, "d160-ac5-topic", msg.RecipientID,
		"AC-5: recipientID must be the topic key, not the supplied user")
	assert.Equal(t, "thread:d160-ac5-topic", msg.Recipient,
		"AC-5: recipient must be thread:<key>, not user:<email>")
	assert.NotEqual(t, user.ID, msg.RecipientID,
		"AC-5: recipientID must NOT be the supplied user ID")
}

// ---------------------------------------------------------------------------
// DEF-160 AC-7: an unparseable group external_ref is refused, not repaired.
// ---------------------------------------------------------------------------

func TestDEF160_AC7_UnparseableExternalRef_Refused(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	// Create a group conversation with a malformed external_ref
	// (missing the third component).
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:malformed",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"should fail", "conv:"+created.ID)
	require.Equal(t, http.StatusInternalServerError, rr.Code,
		"AC-7: unparseable external_ref must be refused: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "unparseable external_ref",
		"AC-7: error message must describe the issue")
}

// ---------------------------------------------------------------------------
// DEF-160 AC-7 positive pair: a well-formed external_ref succeeds.
// ---------------------------------------------------------------------------

func TestDEF160_AC7_WellFormedExternalRef_Succeeds(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d160-ac7-good",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postConvRefNoRecipient(t, srv, project.ID, agent.ID,
		"should succeed", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"AC-7 positive: well-formed external_ref must succeed: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// DEF-160 #thread syntax: group reply via #topic-name (not conv:uuid).
// ---------------------------------------------------------------------------

func TestDEF160_ThreadRef_GroupReply_Delivers200(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d160-threadref",
		ProjectID:   &project.ID,
		DriftState:  "active",
		DisplayName: "d160-threadref",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// Use #topic-name syntax.
	rr := postOutboundRefOnly(t, srv, project.ID, agent.ID,
		"thread ref message", "#d160-threadref")
	require.Equal(t, http.StatusOK, rr.Code,
		"#topic with no recipient must succeed: %s", rr.Body.String())

	result, err := s.ListMessages(ctx, store.MessageFilter{ConversationID: created.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "d160-threadref", result.Items[0].RecipientID,
		"recipientID must be the topic key via #ref")
}

// ---------------------------------------------------------------------------
// DEF-160 response body check: the response must carry the derived
// recipient so the caller can see what was used.
// ---------------------------------------------------------------------------

func TestDEF160_ResponseBody_DerivedRecipient(t *testing.T) {
	srv, s, project, agent, _ := def138Setup(t)
	ctx := context.Background()

	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + project.ID + ":d160-resp",
		ProjectID:   &project.ID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	rr := postOutboundRefOnly(t, srv, project.ID, agent.ID,
		"response check", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "thread:d160-resp", resp["recipient"],
		"response recipient must be thread:<key>")
	assert.Equal(t, "d160-resp", resp["recipient_id"],
		"response recipient_id must be the topic key")
}

// ---------------------------------------------------------------------------
// DEF-161 AC-6: recipient-side validation for direct conv-ref.
//
// This is the RECIPIENT-SIDE counterpart of the SENDER-SIDE test in
// TestDEF142_AC3_NotFound_vs_NotParticipant_ByteIdentical (handlers_
// outbound_def142_test.go:336). That test covers: the authenticated
// sender is NOT in the DM key → rejected at Resolve. This test covers:
// the sender IS in the DM key (Resolve passes) but the supplied
// recipient is NOT in the DM key → rejected at DEF-161.
//
// Together the two tests assert both halves of a single invariant:
// no direct message may name a principal — sender or recipient — who
// is not in the DM key.
// ---------------------------------------------------------------------------

func TestDEF161_AC6_DirectConvRef_RecipientNotInDMKey_Rejected(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// Create a direct DM between the sending agent and a DIFFERENT user.
	// The test's `user` (from def138Setup) is NOT in this DM key.
	otherUser := &store.User{
		ID:          tid("d161-ac6-other-user"),
		Email:       "d161-other@example.com",
		DisplayName: "Other D161 User",
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

	// The sender (agent) IS in the DM key → Resolve and DEF-138 auth pass.
	// The supplied recipient (user) is NOT in the DM key → DEF-161 rejects.
	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"should be rejected", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusBadRequest, rr.Code,
		"DEF-161 AC-6: recipient not in DM key must be rejected 400: %s",
		rr.Body.String())
	assert.Contains(t, rr.Body.String(), "does not match",
		"error must state the mismatch")
}

// ---------------------------------------------------------------------------
// DEF-161 AC-6 positive pair: recipient IS in the DM key → succeeds.
// ---------------------------------------------------------------------------

func TestDEF161_AC6_DirectConvRef_RecipientInDMKey_Succeeds(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	// DM between agent and user — user IS the recipient.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	dmConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundWithRef(t, srv, project.ID, agent.ID, user.Email,
		"should succeed", "conv:"+dmConv.ID)
	require.Equal(t, http.StatusOK, rr.Code,
		"DEF-161 positive: recipient in DM key must succeed: %s",
		rr.Body.String())
}
