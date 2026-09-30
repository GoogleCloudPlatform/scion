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

// ---------------------------------------------------------------------------
// Impl review r1, finding 3: U7 route-level cases (handleGetConversationMessage,
// enforceCrossProjectReadGate, the listing predicate) and U12 (ExecuteAgentDM's
// GetConversation call discipline). These drive the real HTTP handlers, not
// just the pure predicate functions already covered in offload_predicate_test.go.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getAgentErrorStore wraps store.Store and makes GetAgent return a given
// non-ErrNotFound error for one specific agent ID, so the "any other lookup
// error -> 500" case (U7) can be exercised without a real database fault.
type getAgentErrorStore struct {
	store.Store
	failID string
	err    error
}

func (s *getAgentErrorStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.failID {
		return nil, s.err
	}
	return s.Store.GetAgent(ctx, id)
}

// countingGetConversationStore wraps store.Store and counts GetConversation
// calls, for U12 ("ExecuteAgentDM performs no GetConversation for a message
// that does not qualify").
type countingGetConversationStore struct {
	store.Store
	calls int
}

func (s *countingGetConversationStore) GetConversation(ctx context.Context, id string) (*store.Conversation, error) {
	s.calls++
	return s.Store.GetConversation(ctx, id)
}

// routeAuthzSetup creates two projects (P1, P2) each with one agent (A in
// P1, B in P2), a direct conversation keyed K(A,B), and a third agent Z in
// P2 (alongside B) — used to construct rows whose parties are NOT exactly
// the conversation's own key, to check that the peer is always taken from
// the key, never "the other row party" (design §4.3, r4 #1).
func routeAuthzSetup(t *testing.T) (srv *Server, s store.Store, conv *store.Conversation, agentA, agentB, agentZ *store.Agent) {
	t.Helper()
	srv, s, projA, projB, agentA, agentB := predicateSetup(t)
	ctx := context.Background()

	agentZ = &store.Agent{
		ID: tid("route-authz-z"), Name: "z", Slug: "z", ProjectID: projB, Phase: "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agentZ))
	_ = projA

	key, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv, err = s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: key, DriftState: "active",
	})
	require.NoError(t, err)
	return srv, s, conv, agentA, agentB, agentZ
}

func getConvMessage(srv *Server, callerID, callerProject, convID, msgID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages/"+msgID, nil)
	req = req.WithContext(agentContext(callerID, callerProject))
	rr := httptest.NewRecorder()
	srv.handleGetConversationMessage(rr, req, convID, msgID)
	return rr
}

// U7 route level, r4 #1: a Z -> B row inside K(A<->B), whose sender (Z) is
// not a party to the conversation's own key. The peer (from B's
// perspective) must be taken from the KEY (A), never from "the row party
// that isn't the caller" (which would wrongly be Z here). Z is in the same
// project as B; A is in a different project. If the implementation ever
// used Z's project instead of A's, this would wrongly ALLOW (same project);
// using A's project correctly denies (cross-project, flag off).
func TestRouteAuthz_U7_NonPartyKeyPeer_ZToBRow_FlagOff403ThroughLiveLookupOfA(t *testing.T) {
	srv, s, conv, _, agentB, agentZ := routeAuthzSetup(t)
	ctx := context.Background()

	msg := &store.Message{
		ID: tid("route-authz-z-to-b"), ProjectID: agentB.ProjectID,
		Sender: "agent:" + agentZ.Slug, SenderID: agentZ.ID,
		Recipient: "agent:" + agentB.Slug, RecipientID: agentB.ID,
		Msg: "hello", Type: messages.TypeInstruction, ConversationID: conv.ID,
	}
	require.NoError(t, s.CreateMessage(ctx, msg))

	rr := getConvMessage(srv, agentB.ID, agentB.ProjectID, conv.ID, msg.ID)
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
	assert.NotEqual(t, http.StatusOK, rr.Code, "must never allow via Z's (same-project) identity instead of the key peer A's")
}

// U7 route level, r4 #1: an A -> Z row inside K(A<->B): the sender (A) IS
// the key peer, so peerProjectFromRow's sender branch is the one designed to
// fire here, using A's own SenderProjectID stamp directly with no live
// lookup needed.
//
// r2 finding 3: in this test, that design intent is not what actually runs.
// The ent-backed test store never persists SenderProjectID/RecipientProjectID
// (ptone/scion#2282), so s.CreateMessage followed by the handler's
// s.store.GetMessage round-trip silently drops the stamp set above — the
// message peerProjectFromRow sees has a nil SenderProjectID, so
// enforceCrossProjectReadGate actually falls through to the live lookup of
// A's current project, not the stamp branch. The assertion (403, flag off)
// still holds either way, so this test remains valid coverage of the
// outcome, but its name and comment must not claim the stamp path was
// exercised. See TestOffload_I10_StampSurvivesDeletion_WhenStorePersistsIt
// for the in-memory-store variant that does exercise the stamp branch.
func TestRouteAuthz_U7_NonPartyKeyPeer_AToZRow_FlagOff403ThroughLiveLookup(t *testing.T) {
	srv, s, conv, agentA, agentB, agentZ := routeAuthzSetup(t)
	ctx := context.Background()

	senderProj := agentA.ProjectID
	msg := &store.Message{
		ID: tid("route-authz-a-to-z"), ProjectID: agentZ.ProjectID,
		Sender: "agent:" + agentA.Slug, SenderID: agentA.ID, SenderProjectID: &senderProj,
		Recipient: "agent:" + agentZ.Slug, RecipientID: agentZ.ID,
		Msg: "hello", Type: messages.TypeInstruction, ConversationID: conv.ID,
	}
	require.NoError(t, s.CreateMessage(ctx, msg))

	rr := getConvMessage(srv, agentB.ID, agentB.ProjectID, conv.ID, msg.ID)
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
}

// U7 route level: a message that exists but is not in the requested
// conversation must 404 — the reordered handler loads the message before
// the cross-project gate, but the conversation-membership check still runs.
func TestRouteAuthz_U7_MessageNotInConversation_404(t *testing.T) {
	srv, s, conv, agentA, agentB, _ := routeAuthzSetup(t)
	ctx := context.Background()

	otherConv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "direct", Surface: "native",
		ExternalRef: mustDMKey(t, "agent", agentA.ID, "user", tid("route-authz-other-user")),
		DriftState:  "active",
	})
	require.NoError(t, err)

	msg := &store.Message{
		ID: tid("route-authz-wrong-conv"), ProjectID: agentB.ProjectID,
		Sender: "agent:" + agentA.Slug, SenderID: agentA.ID,
		Recipient: "agent:" + agentB.Slug, RecipientID: agentB.ID,
		Msg: "hello", Type: messages.TypeInstruction, ConversationID: otherConv.ID,
	}
	require.NoError(t, s.CreateMessage(ctx, msg))

	rr := getConvMessage(srv, agentB.ID, agentB.ProjectID, conv.ID, msg.ID)
	assert.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())
}

// U7 route level: a caller who is not named in the conversation's DM key
// must be denied (403) by authorizeDMRead, before the message row is ever
// loaded — proven here with a message ID that does not exist, so a 404
// (from a failed GetMessage) would indicate the row was loaded first.
func TestRouteAuthz_U7_NonPartyCaller_403BeforeRowLoaded(t *testing.T) {
	srv, _, conv, _, _, agentZ := routeAuthzSetup(t)

	rr := getConvMessage(srv, agentZ.ID, agentZ.ProjectID, conv.ID, "00000000-0000-0000-0000-000000000000")
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
}

// U7: any GetAgent error other than ErrNotFound is a 500, both at the
// predicate level (already covered in offload_predicate_test.go indirectly)
// and at the route level through handleGetConversationMessage, with no
// stamp available (forcing the live lookup).
func TestRouteAuthz_U7_OtherLookupError_500(t *testing.T) {
	srv, s, conv, agentA, agentB, _ := routeAuthzSetup(t)
	ctx := context.Background()

	msg := &store.Message{
		ID: tid("route-authz-lookup-error"), ProjectID: agentB.ProjectID,
		Sender: "agent:" + agentA.Slug, SenderID: agentA.ID, // no SenderProjectID stamp
		Recipient: "agent:" + agentB.Slug, RecipientID: agentB.ID,
		Msg: "hello", Type: messages.TypeInstruction, ConversationID: conv.ID,
	}
	require.NoError(t, s.CreateMessage(ctx, msg))

	srv.store = &getAgentErrorStore{Store: s, failID: agentA.ID, err: errors.New("injected transient db failure")}

	rr := getConvMessage(srv, agentB.ID, agentB.ProjectID, conv.ID, msg.ID)
	assert.Equal(t, http.StatusInternalServerError, rr.Code, "body: %s", rr.Body.String())
}

// U7: enforceCrossProjectReadGate's plain conversation-level form (msg ==
// nil, used by handleGetConversation/handleConvListMessages/conversation
// resolution — no specific row to stamp from) must also return 403, never
// 500, for a deleted peer (design §8.6's documented behavior change).
func TestRouteAuthz_U7_ConversationLevelGate_DeletedPeer_403NeverA500(t *testing.T) {
	srv, s, conv, agentA, agentB, _ := routeAuthzSetup(t)
	ctx := context.Background()
	require.NoError(t, s.DeleteAgent(ctx, agentA.ID)) // hard delete

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(agentContext(agentB.ID, agentB.ProjectID))
	rr := httptest.NewRecorder()
	ok := srv.enforceCrossProjectReadGate(rr, req, conv, nil)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rr.Code, "body: %s", rr.Body.String())
}

// nit 4: the listing predicate (isCrossProjectReadAllowed) now follows the
// same deleted-peer/flag rule as the read gates (design §19 IF2), rather
// than "any lookup error excludes". Flag on -> visible; flag off -> hidden.
func TestRouteAuthz_Nit4_ListingPredicate_DeletedPeerFollowsFlag(t *testing.T) {
	srv, s, conv, agentA, agentB, _ := routeAuthzSetup(t)
	ctx := context.Background()
	require.NoError(t, s.DeleteAgent(ctx, agentA.ID))

	bIdent := cpmAgentIdentity(agentB.ID, agentB.ProjectID, nil)

	// Flag off (compiled default): hidden.
	assert.False(t, srv.isCrossProjectReadAllowed(ctx, conv, bIdent))

	// Flag on: visible.
	enableCPM(t, srv, s)
	assert.True(t, srv.isCrossProjectReadAllowed(ctx, conv, bIdent))
}

// mustDMKey is defined in handlers_outbound_dm_thread_recipient_test.go
// (added by an upstream commit merged into this branch); both files wrap
// messages.DMConversationKey identically, so this file reuses that one
// instead of redeclaring it.

// ---------------------------------------------------------------------------
// U12: ExecuteAgentDM performs no GetConversation for a message that does
// not qualify for offload — the small-message path pays nothing extra.
// ---------------------------------------------------------------------------

func TestU12_ExecuteAgentDM_NoGetConversationWhenNotQualifying(t *testing.T) {
	srv, s, _, sender, target, dmConvID, _, _ := paritySetup(t)
	enableOffload(t, srv, 4000, true)
	counting := &countingGetConversationStore{Store: s}
	srv.store = counting

	senderIdent := cpmAgentIdentity(sender.ID, sender.ProjectID, sender.Ancestry)
	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: senderIdent,
		TargetAgent:    target,
		Msg:            "short body, well under the 4000-rune threshold",
		Type:           messages.TypeInstruction,
		ConversationID: dmConvID,
		ProjectID:      sender.ProjectID,
	})
	require.Nil(t, dmErr)
	require.Equal(t, AgentDMAccepted, result.Outcome)
	assert.Equal(t, 0, counting.calls, "a non-qualifying message must never trigger ExecuteAgentDM's own GetConversation lookup")
}

func TestU12_ExecuteAgentDM_CallsGetConversationWhenQualifying(t *testing.T) {
	srv, s, _, sender, target, dmConvID, _, _ := paritySetup(t)
	enableOffload(t, srv, 4000, true)
	counting := &countingGetConversationStore{Store: s}
	srv.store = counting

	senderIdent := cpmAgentIdentity(sender.ID, sender.ProjectID, sender.Ancestry)
	body := ""
	for i := 0; i < 12000; i++ {
		body += "q"
	}
	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: senderIdent,
		TargetAgent:    target,
		Msg:            body,
		Type:           messages.TypeInstruction,
		ConversationID: dmConvID,
		ProjectID:      sender.ProjectID,
	})
	require.Nil(t, dmErr)
	require.Equal(t, AgentDMAccepted, result.Outcome)
	assert.Equal(t, 1, counting.calls, "a qualifying message must look the conversation up exactly once")
	_ = messaging.MetaBodyOffloaded // keep the messaging import meaningful if trimmed later
}
