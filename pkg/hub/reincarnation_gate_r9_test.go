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

// Tests for the p2a-u5 review round-6 fix (design agent-reincarnate §3.7,
// Amendment A25.11, R1 — the FINAL review round): chat v2's sendHumanToHuman
// registered both principals of a URL-supplied "dm:" key
// (WithThreadParticipants, added by A25.6's 6122f1bc), but the authorization
// gate (isDMParticipant, handleConversationSend) only proves the CALLER's
// own slot. The other slot is a caller-chosen path segment that was never
// resolved, so an authenticated human could write "user:<agent-uuid>",
// "user:<nonexistent>" or "agent:<nonexistent>" participant rows — the same
// phantom-row class A25.7 R2 and A25.8 R1 closed on the agent paths, via a
// URL path segment instead of a JSON payload field. Folds in the reviewer's
// TestRevU5_ChatV2PhantomShapes (reviews/p2a-u5-repro_test.go.txt).
//
// Fix (A25.11 R1, the reviewer's option 2, narrower): sendHumanToHuman now
// registers participants only when the peer (non-caller) slot resolves in
// the store with its matching kind. authorizeDMPeer (authorizeChatSend)
// also checks that slot before anything is persisted: a send whose peer
// does not resolve with its kind is refused with 403, a store error on the
// lookup with 503, and no participant rows are written.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendChatV2AndSnapshot posts to the chat v2 send endpoint for the given key
// and returns the response plus the resulting conversation (nil if none was
// created) and its participant rows (nil if the conversation doesn't exist).
func sendChatV2AndSnapshot(t *testing.T, srv *Server, s store.Store, key string) (code int, parts []store.ConversationParticipant) {
	t.Helper()
	ctx := context.Background()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+key+"/messages", map[string]string{"content": "x"})
	conv, err := s.GetConversationByExternalRef(ctx, "native", key)
	if err != nil || conv == nil {
		return rec.Code, nil
	}
	got, err := s.ListParticipants(ctx, conv.ID)
	require.NoError(t, err)
	return rec.Code, got
}

// TestChatV2_A2511_R1_UserUser_AgentUUIDAsPeer_NoPhantomRow is the reviewer's
// first shape: dm:user:<self>:user:<agent Z's UUID>. The peer slot names a
// real principal, but of the WRONG kind (an agent's UUID in a "user" slot) —
// it must not resolve, and no "user:<agentUUID>" row may be written.
func TestChatV2_A2511_R1_UserUser_AgentUUIDAsPeer_NoPhantomRow(t *testing.T) {
	srv, s, _, proj, _ := setupSendTest(t)
	ctx := context.Background()

	agentZ := &store.Agent{ID: tid("a2511-agent-z"), ProjectID: proj.ID, Name: "Z", Slug: "a2511-z", Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateAgent(ctx, agentZ))

	key, err := messages.DMConversationKey("user", DevUserID, "user", agentZ.ID)
	require.NoError(t, err)

	code, parts := sendChatV2AndSnapshot(t, srv, s, key)
	require.Equal(t, http.StatusForbidden, code, "authorizeDMPeer refuses a peer that is not a user")
	assert.Empty(t, parts, "no participant rows may be written when the peer slot names an agent, not a user")

	// Also confirm agent Z gains no listing from this: it must not appear in
	// GetConversationsForPrincipal for "agent".
	convs, err := s.GetConversationsForPrincipal(ctx, "agent", agentZ.ID)
	require.NoError(t, err)
	assert.Empty(t, convs, "agent Z must not be able to list a conversation it was never made a participant of")
}

// TestChatV2_A2511_R1_UserUser_GhostUUIDAsPeer_NoPhantomRow is the second
// shape: dm:user:<ghost>:user:<self>, where <ghost> matches no user at all.
func TestChatV2_A2511_R1_UserUser_GhostUUIDAsPeer_NoPhantomRow(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)

	ghostID := tid("a2511-ghost-user")
	key, err := messages.DMConversationKey("user", ghostID, "user", DevUserID)
	require.NoError(t, err)

	code, parts := sendChatV2AndSnapshot(t, srv, s, key)
	require.Equal(t, http.StatusForbidden, code)
	assert.Empty(t, parts, "no participant rows may be written when the peer slot resolves to nothing")
}

// TestChatV2_A2511_R1_AgentUser_GhostAgentAsPeer_NoPhantomRow is the third
// shape: dm:agent:<ghost-agent>:user:<self> — the peer is an agent slot
// naming an agent that doesn't exist. authorizeDMPeer refuses it before
// routing, so nothing is persisted.
func TestChatV2_A2511_R1_AgentUser_GhostAgentAsPeer_NoPhantomRow(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)

	key := "dm:agent:" + tid("a2511-ghost-agent") + ":user:" + DevUserID

	code, parts := sendChatV2AndSnapshot(t, srv, s, key)
	require.Equal(t, http.StatusForbidden, code)
	assert.Empty(t, parts, "no participant rows may be written when the peer slot names a nonexistent agent")
}

// TestChatV2_A2511_R1_AgentUser_UserUUIDInAgentSlot_NoPhantomRow is the
// p2a-u6 reviewer's repro (design.md A25.12 R1): dm:agent:<real USER's
// UUID>:user:<self> — the agent slot names a real principal, but of the
// WRONG kind (a user's UUID in an "agent" slot). It must not resolve via
// GetUser, and no "agent:<user-UUID>" row may be written. This is the
// mirror image of UserUser_AgentUUIDAsPeer above and is the test that
// kills mutation kind_userForAgent (accepting GetUser for an agent slot),
// which survived every other test in the p2a-u5/u6 suites because none of
// them used a real, store-backed user's UUID in the agent slot specifically
// (GhostAgentAsPeer's ID resolves to neither a user nor an agent, so it
// cannot tell GetAgent from GetUser apart).
func TestChatV2_A2511_R1_AgentUser_UserUUIDInAgentSlot_NoPhantomRow(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)
	ctx := context.Background()
	u := &store.User{ID: tid("a2511-user-in-agent-slot"), Email: "a2511-uias@test.com", DisplayName: "U"}
	require.NoError(t, s.CreateUser(ctx, u))
	key := "dm:agent:" + u.ID + ":user:" + DevUserID
	code, parts := sendChatV2AndSnapshot(t, srv, s, key)
	require.Equal(t, http.StatusForbidden, code)
	assert.Empty(t, parts)
}

// ---------------------------------------------------------------------------
// A25.12 O1: the store-error path (a lookup failure that is NOT "not
// found") must be treated as unresolved, exactly like a genuine ghost ID —
// never as "resolved" (which would let a store hiccup mint a
// caller-uncontrolled phantom row).
// ---------------------------------------------------------------------------

// getUserErrStore wraps a real store and makes GetUser return a non-NotFound
// error for one specific ID, simulating a transient store failure during
// peer resolution — distinct from a ghost ID, which returns store.ErrNotFound
// and is already covered by the shapes above.
type getUserErrStore struct {
	store.Store
	failID string
}

func (s *getUserErrStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.failID {
		return nil, errors.New("injected store error")
	}
	return s.Store.GetUser(ctx, id)
}

// getAgentErrStore is getUserErrStore's mirror for the agent-slot lookup.
type getAgentErrStore struct {
	store.Store
	failID string
}

func (s *getAgentErrStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.failID {
		return nil, errors.New("injected store error")
	}
	return s.Store.GetAgent(ctx, id)
}

// TestChatV2_A2511_O1_UserSlot_StoreError_NoPhantomRow pins the fail-closed
// 503 for a store error on the user-slot peer lookup: the send is refused
// and no participant row is written.
func TestChatV2_A2511_O1_UserSlot_StoreError_NoPhantomRow(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)

	peerID := tid("a2511-o1-user-peer")
	srv.store = &getUserErrStore{Store: s, failID: peerID}

	key, err := messages.DMConversationKey("user", DevUserID, "user", peerID)
	require.NoError(t, err)

	code, parts := sendChatV2AndSnapshot(t, srv, s, key)
	require.Equal(t, http.StatusServiceUnavailable, code, "authorizeDMPeer fails closed on a store error")
	assert.Empty(t, parts, "a store error on the peer lookup must not write a participant row")
}

// TestChatV2_A2511_O1_AgentSlot_StoreError_NoPhantomRow pins the same
// fail-closed 503 for a store error on the agent-slot (GetAgent) peer
// lookup (A25.12 O1).
func TestChatV2_A2511_O1_AgentSlot_StoreError_NoPhantomRow(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)

	agentID := tid("a2511-o1-agent-peer")
	srv.store = &getAgentErrStore{Store: s, failID: agentID}

	key := "dm:agent:" + agentID + ":user:" + DevUserID

	code, parts := sendChatV2AndSnapshot(t, srv, s, key)
	require.Equal(t, http.StatusServiceUnavailable, code, "authorizeDMPeer fails closed on a store error")
	assert.Empty(t, parts, "a store error on the peer lookup must not write a participant row")
}

// TestChatV2_A2511_R1_UserUser_RealPeer_BothRowsRegistered is the positive
// control: when the peer DOES resolve (a genuine, store-created user), both
// participant rows are still registered exactly as A25.6/A25.7 intended.
func TestChatV2_A2511_R1_UserUser_RealPeer_BothRowsRegistered(t *testing.T) {
	srv, s, _, _, _ := setupSendTest(t)
	ctx := context.Background()

	peer := &store.User{ID: tid("a2511-real-peer"), Email: "a2511-real-peer@test.com", DisplayName: "A2511 Peer"}
	require.NoError(t, s.CreateUser(ctx, peer))

	key, err := messages.DMConversationKey("user", DevUserID, "user", peer.ID)
	require.NoError(t, err)

	code, parts := sendChatV2AndSnapshot(t, srv, s, key)
	require.Equal(t, http.StatusCreated, code)
	require.Len(t, parts, 2, "a resolved real peer must still get both participant rows registered")

	ids := map[string]bool{}
	for _, p := range parts {
		assert.Equal(t, "user", p.PrincipalKind)
		ids[p.PrincipalID] = true
	}
	assert.True(t, ids[DevUserID])
	assert.True(t, ids[peer.ID])
}
