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
// U7 / U12: the cross-project peer predicate, the row-stamp selection rule,
// and recipientCanReadConversation (ptone/scion#2257, design
// auto-offload-large-dm §4.3).
// ---------------------------------------------------------------------------

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// predicateSetup creates two projects, each with one agent, and a hub owner.
// Hub-level cross-project messaging starts disabled (compiled default).
func predicateSetup(t *testing.T) (srv *Server, s store.Store, projA, projB string, agentA, agentB *store.Agent) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("pred-owner"),
		Email:   "pred-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	projA = tid("pred-project-a")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projA, Name: "pred-a", Slug: "pred-a", OwnerID: owner.ID, CreatedBy: owner.ID,
	}))
	projB = tid("pred-project-b")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projB, Name: "pred-b", Slug: "pred-b", OwnerID: owner.ID, CreatedBy: owner.ID,
	}))

	agentA = &store.Agent{
		ID: tid("pred-agent-a"), Name: "agent-a", Slug: "agent-a",
		ProjectID: projA, Phase: "running", Ancestry: []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	agentB = &store.Agent{
		ID: tid("pred-agent-b"), Name: "agent-b", Slug: "agent-b",
		ProjectID: projB, Phase: "running", Ancestry: []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	return srv, s, projA, projB, agentA, agentB
}

func TestCrossProjectPeerAllowed_HumanPeerAlwaysAllowed(t *testing.T) {
	srv, _, projA, _, _, _ := predicateSetup(t)
	decision, reason := srv.crossProjectPeerAllowed(context.Background(), projA, "user", "someone", nil)
	assert.Equal(t, peerAllowed, decision)
	assert.Empty(t, reason)
}

func TestCrossProjectPeerAllowed_StampedProjectUsedWithNoLiveLookup(t *testing.T) {
	srv, _, projA, _, _, _ := predicateSetup(t)
	// A peer ID that does not exist in the store: if the stamp weren't
	// honoured, the live GetAgent fallback would return ErrNotFound and deny.
	nonExistentPeerID := tid("pred-nonexistent-peer")
	stamped := projA
	decision, reason := srv.crossProjectPeerAllowed(context.Background(), projA, "agent", nonExistentPeerID, &stamped)
	assert.Equal(t, peerAllowed, decision)
	assert.Empty(t, reason)
}

func TestCrossProjectPeerAllowed_NilStampSameProjectAllows(t *testing.T) {
	srv, _, projA, _, agentA, _ := predicateSetup(t)
	decision, _ := srv.crossProjectPeerAllowed(context.Background(), projA, "agent", agentA.ID, nil)
	assert.Equal(t, peerAllowed, decision)
}

func TestCrossProjectPeerAllowed_NilStampCrossProjectFollowsFlag(t *testing.T) {
	srv, s, projA, _, _, agentB := predicateSetup(t)
	ctx := context.Background()

	decision, reason := srv.crossProjectPeerAllowed(ctx, projA, "agent", agentB.ID, nil)
	assert.Equal(t, peerDenied403, decision)
	assert.Equal(t, "cross-project messaging is disabled", reason)

	enableCPM(t, srv, s)

	decision, reason = srv.crossProjectPeerAllowed(ctx, projA, "agent", agentB.ID, nil)
	assert.Equal(t, peerAllowed, decision)
	assert.Empty(t, reason)
}

func TestCrossProjectPeerAllowed_NilStampDeletedPeerIs403NeverA500(t *testing.T) {
	srv, s, projA, _, _, agentB := predicateSetup(t)
	ctx := context.Background()
	require.NoError(t, s.DeleteAgent(ctx, agentB.ID)) // hard delete

	decision, reason := srv.crossProjectPeerAllowed(ctx, projA, "agent", agentB.ID, nil)
	assert.Equal(t, peerDenied403, decision, "a deleted peer must never produce peerErr500")
	assert.Contains(t, reason, "peer agent not found")
}

// msg-attach-arch refinement (2026-09-30, ptone/scion#2282): with no stamp, a
// deleted peer's project can't be told apart from the caller's. When
// cross-project messaging is enabled, every possible peer project would be
// allowed anyway, so the missing project doesn't matter — allow. This keeps
// the gate exact under the current policy without needing the persisted
// stamp (ptone/scion#2282).
func TestCrossProjectPeerAllowed_NilStampDeletedPeerAllowsWhenCrossProjectEnabled(t *testing.T) {
	srv, s, projA, _, _, agentB := predicateSetup(t)
	ctx := context.Background()
	require.NoError(t, s.DeleteAgent(ctx, agentB.ID)) // hard delete
	enableCPM(t, srv, s)

	decision, reason := srv.crossProjectPeerAllowed(ctx, projA, "agent", agentB.ID, nil)
	assert.Equal(t, peerAllowed, decision)
	assert.Empty(t, reason)
}

func TestPeerProjectFromRow(t *testing.T) {
	senderProj := "proj-sender"
	recipProj := "proj-recipient"
	msg := &store.Message{
		Sender: "agent:sender-slug", SenderID: "sender-id", SenderProjectID: &senderProj,
		Recipient: "agent:recip-slug", RecipientID: "recip-id", RecipientProjectID: &recipProj,
	}

	// Key peer = sender.
	got := peerProjectFromRow(msg, "agent", "sender-id")
	require.NotNil(t, got)
	assert.Equal(t, senderProj, *got)

	// Key peer = recipient.
	got = peerProjectFromRow(msg, "agent", "recip-id")
	require.NotNil(t, got)
	assert.Equal(t, recipProj, *got)

	// Key peer is neither party -> nil (live lookup).
	got = peerProjectFromRow(msg, "agent", "someone-else")
	assert.Nil(t, got)

	// Matching ID but wrong kind: a user: party whose ID equals the peer
	// agent's ID must not be treated as that agent.
	msgUserSender := &store.Message{
		Sender: "user:sender-id", SenderID: "sender-id",
		Recipient: "agent:recip-slug", RecipientID: "recip-id", RecipientProjectID: &recipProj,
	}
	got = peerProjectFromRow(msgUserSender, "agent", "sender-id")
	assert.Nil(t, got)

	// Human peer: stamp unused regardless of row shape.
	got = peerProjectFromRow(msg, "user", "sender-id")
	assert.Nil(t, got)
}

func TestPeerProjectFromRow_NonPartyKeyPeerNeverFallsBackToWrongParty(t *testing.T) {
	// R4 #1 regression: in K(A<->B), a constructed row Z -> B (parties
	// exclude A) must never yield A's project via "the party that isn't the
	// caller". The key peer here is A, who is not a party to this row at all.
	recipProj := "proj-b-or-z-recipient"
	senderProj := "proj-z-sender"
	rowZtoB := &store.Message{
		Sender: "agent:z-slug", SenderID: "z-id", SenderProjectID: &senderProj,
		Recipient: "agent:b-slug", RecipientID: "b-id", RecipientProjectID: &recipProj,
	}
	got := peerProjectFromRow(rowZtoB, "agent", "a-id")
	assert.Nil(t, got, "key peer A is not a party to the Z->B row; must fall back to a live lookup, not silently borrow Z's or B's project")
}

func TestRecipientCanReadConversation_DirectSameProject(t *testing.T) {
	srv, s, projA, _, agentA, _ := predicateSetup(t)
	ctx := context.Background()

	target := &store.Agent{ID: tid("pred-target-same-project"), Name: "target", Slug: "target-same", ProjectID: projA, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, target))

	key, err := messages.DMConversationKey("agent", agentA.ID, "agent", target.ID)
	require.NoError(t, err)
	conv := &store.Conversation{Kind: "direct", ExternalRef: key}

	assert.True(t, srv.recipientCanReadConversation(ctx, conv, target))
}

func TestRecipientCanReadConversation_TargetNotInKey(t *testing.T) {
	srv, s, projA, _, agentA, _ := predicateSetup(t)
	ctx := context.Background()

	other := &store.Agent{ID: tid("pred-other-agent"), Name: "other", Slug: "other", ProjectID: projA, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, other))
	target := &store.Agent{ID: tid("pred-uninvolved-target"), Name: "uninvolved", Slug: "uninvolved", ProjectID: projA, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, target))

	key, err := messages.DMConversationKey("agent", agentA.ID, "agent", other.ID)
	require.NoError(t, err)
	conv := &store.Conversation{Kind: "direct", ExternalRef: key}

	assert.False(t, srv.recipientCanReadConversation(ctx, conv, target))
}

func TestRecipientCanReadConversation_CrossProjectFollowsFlag(t *testing.T) {
	srv, s, _, projB, agentA, agentB := predicateSetup(t)
	ctx := context.Background()
	_ = projB

	key, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)
	conv := &store.Conversation{Kind: "direct", ExternalRef: key}

	assert.False(t, srv.recipientCanReadConversation(ctx, conv, agentB), "flag off -> false")

	enableCPM(t, srv, s)
	assert.True(t, srv.recipientCanReadConversation(ctx, conv, agentB), "flag on -> true")
}

func TestRecipientCanReadConversation_GroupInTargetProject(t *testing.T) {
	srv, _, projA, _, _, _ := predicateSetup(t)
	ctx := context.Background()
	target := &store.Agent{ID: tid("pred-group-target"), ProjectID: projA}
	conv := &store.Conversation{Kind: "group", ProjectID: &projA}
	assert.True(t, srv.recipientCanReadConversation(ctx, conv, target))
}

func TestRecipientCanReadConversation_GroupInOtherProject(t *testing.T) {
	srv, _, projA, projB, _, _ := predicateSetup(t)
	ctx := context.Background()
	target := &store.Agent{ID: tid("pred-group-target-2"), ProjectID: projA}
	conv := &store.Conversation{Kind: "group", ProjectID: &projB}
	assert.False(t, srv.recipientCanReadConversation(ctx, conv, target))
}

func TestRecipientCanReadConversation_LegacyProjectlessGroupFailsSafe(t *testing.T) {
	srv, _, projA, _, _, _ := predicateSetup(t)
	ctx := context.Background()
	target := &store.Agent{ID: tid("pred-legacy-group-target"), ProjectID: projA}
	conv := &store.Conversation{Kind: "group", ProjectID: nil}
	assert.False(t, srv.recipientCanReadConversation(ctx, conv, target))
}

func TestRecipientCanReadConversation_MissingConversationOrTarget(t *testing.T) {
	srv, _, projA, _, _, _ := predicateSetup(t)
	ctx := context.Background()
	target := &store.Agent{ID: tid("pred-missing-conv-target"), ProjectID: projA}
	assert.False(t, srv.recipientCanReadConversation(ctx, nil, target))
	assert.False(t, srv.recipientCanReadConversation(ctx, &store.Conversation{Kind: "direct"}, nil))
}
