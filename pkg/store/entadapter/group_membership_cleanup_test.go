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

package entadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	entgo "entgo.io/ent"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/groupmembership"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2769 item 4: group memberships left behind with both
// user_id and agent_id NULL (ON DELETE SET NULL) when their principal is
// deleted.

// orphanedMembershipCount counts membership rows whose user and agent are
// both NULL, read directly so the count does not go through the filters
// under test.
func orphanedMembershipCount(t *testing.T, cs *CompositeStore) int {
	t.Helper()
	n, err := cs.client.GroupMembership.Query().
		Where(groupmembership.UserIDIsNil(), groupmembership.AgentIDIsNil()).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

// membershipRowCount counts every membership row of a group, orphans included.
func membershipRowCount(t *testing.T, cs *CompositeStore, groupID string) int {
	t.Helper()
	n, err := cs.client.GroupMembership.Query().
		Where(groupmembership.GroupIDEQ(uuid.MustParse(groupID))).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

// newCleanupGroup creates an explicit group.
func newCleanupGroup(t *testing.T, cs *CompositeStore, slug string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, cs.CreateGroup(context.Background(), &store.Group{
		ID: id, Name: slug, Slug: slug, GroupType: store.GroupTypeExplicit,
	}))
	return id
}

func addCleanupMember(t *testing.T, cs *CompositeStore, groupID, memberType, memberID, role string) {
	t.Helper()
	require.NoError(t, cs.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: groupID, MemberType: memberType, MemberID: memberID, Role: role,
	}))
}

func newCleanupUser(t *testing.T, cs *CompositeStore, email string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, cs.CreateUser(context.Background(), &store.User{
		ID: id, Email: email, DisplayName: email, Role: store.UserRoleMember, Status: store.UserStatusActive,
	}))
	return id
}

// agentGroupFixture is a project with two agents, both members of one
// explicit group that also has a user owner (a row that must survive).
type agentGroupFixture struct {
	cs        *CompositeStore
	projectID string
	group     string
	owner     string
	a, b      *store.Agent
}

func newAgentGroupFixture(t *testing.T) agentGroupFixture {
	t.Helper()
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	f := agentGroupFixture{cs: cs, projectID: uuid.NewString()}
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: f.projectID, Name: "gm", Slug: "gm-" + f.projectID[:8]}))
	f.a = makeAgent(f.projectID, "gm-a")
	f.b = makeAgent(f.projectID, "gm-b")
	require.NoError(t, cs.CreateAgent(ctx, f.a))
	require.NoError(t, cs.CreateAgent(ctx, f.b))
	f.owner = newCleanupUser(t, cs, "gm-owner@example.com")
	f.group = newCleanupGroup(t, cs, "gm-group")
	addCleanupMember(t, cs, f.group, store.GroupMemberTypeUser, f.owner, store.GroupMemberRoleOwner)
	addCleanupMember(t, cs, f.group, store.GroupMemberTypeAgent, f.a.ID, store.GroupMemberRoleMember)
	addCleanupMember(t, cs, f.group, store.GroupMemberTypeAgent, f.b.ID, store.GroupMemberRoleMember)
	return f
}

// TestCountGroupMembersByRole_IgnoresDeletedOwner: one of two group owners
// is deleted (the user row only, as every pre-fix delete path did), so the
// owner count is 1, not 2.
func TestCountGroupMembersByRole_IgnoresDeletedOwner(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	alice := newCleanupUser(t, cs, "alice@example.com")
	bob := newCleanupUser(t, cs, "bob@example.com")
	g := newCleanupGroup(t, cs, "two-owners")
	addCleanupMember(t, cs, g, store.GroupMemberTypeUser, alice, store.GroupMemberRoleOwner)
	addCleanupMember(t, cs, g, store.GroupMemberTypeUser, bob, store.GroupMemberRoleOwner)

	require.NoError(t, cs.DeleteUser(ctx, bob))
	require.Equal(t, 1, orphanedMembershipCount(t, cs), "precondition: ON DELETE SET NULL left an orphan row")

	count, err := cs.CountGroupMembersByRole(ctx, g, store.GroupMemberRoleOwner)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the deleted owner's orphaned row must not be counted")
}

// TestGetGroupMembers_SkipsOrphanedRows: listing a group whose members were
// deleted returns no blank member rows.
func TestGetGroupMembers_SkipsOrphanedRows(t *testing.T) {
	ctx := context.Background()
	f := newAgentGroupFixture(t)
	gone := newCleanupUser(t, f.cs, "gone@example.com")
	addCleanupMember(t, f.cs, f.group, store.GroupMemberTypeUser, gone, store.GroupMemberRoleMember)

	require.NoError(t, f.cs.DeleteUser(ctx, gone))
	require.Equal(t, 1, orphanedMembershipCount(t, f.cs), "precondition: orphan row present")

	members, err := f.cs.GetGroupMembers(ctx, f.group)
	require.NoError(t, err)
	ids := make([]string, 0, len(members))
	for _, m := range members {
		assert.NotEmpty(t, m.MemberType, "no blank member type")
		assert.NotEmpty(t, m.MemberID, "no blank member ID")
		ids = append(ids, m.MemberID)
	}
	assert.ElementsMatch(t, []string{f.owner, f.a.ID, f.b.ID}, ids)
}

// TestCompositeDeleteAgent_RemovesGroupMemberships covers hard-delete path 1:
// CompositeStore.DeleteAgent.
func TestCompositeDeleteAgent_RemovesGroupMemberships(t *testing.T) {
	f := newAgentGroupFixture(t)
	require.NoError(t, f.cs.DeleteAgent(context.Background(), f.a.ID))

	assert.Zero(t, orphanedMembershipCount(t, f.cs), "no NULL row left behind")
	assert.Equal(t, 2, membershipRowCount(t, f.cs, f.group), "owner and the other agent remain")
}

// TestFinalizeAgentDeletionHard_RemovesGroupMemberships covers hard-delete
// path 2: the delete engine's finalize-hard, where the membership delete now
// runs before tx.Agent.Delete() in the same transaction.
func TestFinalizeAgentDeletionHard_RemovesGroupMemberships(t *testing.T) {
	ctx := context.Background()
	f := newAgentGroupFixture(t)
	seedDeletion(t, f.cs.AgentStore, f.a.ID, store.DeletionStateFinalizing, time.Now().Add(time.Minute), "")
	claim := int64(1)
	pred := store.DeletionPredicate{Claim: &claim, States: []string{store.DeletionStateFinalizing}, DeletedAtNull: true}

	n, err := f.cs.FinalizeAgentDeletion(ctx, f.a.ID, pred, store.DeletionFinalizeHard, store.DeletionFields{}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Zero(t, orphanedMembershipCount(t, f.cs), "no NULL row left behind")
	assert.Equal(t, 2, membershipRowCount(t, f.cs, f.group), "owner and the other agent remain")
}

// TestFinalizeAgentDeletionHard_HookFailureKeepsGroupMemberships: the
// membership delete is part of the finalize transaction, so a failing hook
// rolls it back together with the agent delete.
func TestFinalizeAgentDeletionHard_HookFailureKeepsGroupMemberships(t *testing.T) {
	ctx := context.Background()
	f := newAgentGroupFixture(t)
	seedDeletion(t, f.cs.AgentStore, f.a.ID, store.DeletionStateFinalizing, time.Now().Add(time.Minute), "")
	claim := int64(1)
	pred := store.DeletionPredicate{Claim: &claim, States: []string{store.DeletionStateFinalizing}, DeletedAtNull: true}
	boom := errors.New("hook failed")

	_, err := f.cs.FinalizeAgentDeletion(ctx, f.a.ID, pred, store.DeletionFinalizeHard, store.DeletionFields{},
		func(context.Context, store.Store, *store.Agent, store.DeletionFinalizeMode) error { return boom })
	require.ErrorIs(t, err, boom)

	_, err = f.cs.GetAgent(ctx, f.a.ID)
	require.NoError(t, err, "agent survives the rolled-back finalize")
	assert.Equal(t, 3, membershipRowCount(t, f.cs, f.group), "memberships survive the rolled-back finalize")
}

// TestPurgeDeletedAgents_RemovesGroupMemberships covers hard-delete path 3:
// PurgeDeletedAgents (per batch, before the delete). An agent restored inside
// the purge window keeps its membership along with its row.
func TestPurgeDeletedAgents_RemovesGroupMemberships(t *testing.T) {
	ctx := context.Background()
	f := newAgentGroupFixture(t)
	for _, a := range []*store.Agent{f.a, f.b} {
		a.DeletedAt = time.Now().Add(-48 * time.Hour)
		require.NoError(t, f.cs.UpdateAgent(ctx, a))
	}
	restoredUID := uuid.MustParse(f.b.ID)
	purgeDeletedAgentsTestHook = func(tx *ent.Tx, _ []uuid.UUID) {
		require.NoError(t, tx.Agent.UpdateOneID(restoredUID).ClearDeletedAt().Exec(ctx))
	}
	t.Cleanup(func() { purgeDeletedAgentsTestHook = nil })

	purged, err := f.cs.PurgeDeletedAgents(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, purged)

	assert.Zero(t, orphanedMembershipCount(t, f.cs), "no NULL row left behind")
	_, err = f.cs.GetGroupMembership(ctx, f.group, store.GroupMemberTypeAgent, f.b.ID)
	assert.NoError(t, err, "the agent restored mid-purge keeps its membership")
	assert.Equal(t, 2, membershipRowCount(t, f.cs, f.group), "owner and the restored agent remain")
}

// TestCompositeDeleteProject_RemovesGroupMemberships covers hard-delete path
// 4: the project bulk delete of its agents.
func TestCompositeDeleteProject_RemovesGroupMemberships(t *testing.T) {
	f := newAgentGroupFixture(t)
	require.NoError(t, f.cs.DeleteProject(context.Background(), f.projectID))

	assert.Zero(t, orphanedMembershipCount(t, f.cs), "no NULL row left behind")
	assert.Equal(t, 1, membershipRowCount(t, f.cs, f.group), "only the user owner remains")
}

// failAgentDeletes makes every agent delete on cs's client (and on any
// transaction opened from it, which shares the client's hooks) fail with the
// returned error.
func failAgentDeletes(cs *CompositeStore) error {
	boom := errors.New("agent delete failed")
	cs.client.Agent.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if m.Op().Is(entgo.OpDeleteOne | entgo.OpDelete) {
				return nil, boom
			}
			return next.Mutate(ctx, m)
		})
	})
	return boom
}

// TestCompositeDeleteAgent_FailedDeleteKeepsGroupMemberships: DeleteAgent
// runs in one transaction, so when the agent row delete fails the membership
// delete that ran before it is rolled back too.
func TestCompositeDeleteAgent_FailedDeleteKeepsGroupMemberships(t *testing.T) {
	ctx := context.Background()
	f := newAgentGroupFixture(t)
	boom := failAgentDeletes(f.cs)

	require.ErrorIs(t, f.cs.DeleteAgent(ctx, f.a.ID), boom)

	_, err := f.cs.GetAgent(ctx, f.a.ID)
	require.NoError(t, err, "agent survives the failed delete")
	_, err = f.cs.GetGroupMembership(ctx, f.group, store.GroupMemberTypeAgent, f.a.ID)
	assert.NoError(t, err, "the agent keeps its membership")
	assert.Equal(t, 3, membershipRowCount(t, f.cs, f.group), "no membership was removed")
}

// TestCompositeDeleteProject_FailedDeleteKeepsGroupMemberships: DeleteProject
// runs in one transaction, so when the bulk agent delete fails the project's
// agents keep their memberships.
func TestCompositeDeleteProject_FailedDeleteKeepsGroupMemberships(t *testing.T) {
	ctx := context.Background()
	f := newAgentGroupFixture(t)
	boom := failAgentDeletes(f.cs)

	require.ErrorIs(t, f.cs.DeleteProject(ctx, f.projectID), boom)

	_, err := f.cs.GetProject(ctx, f.projectID)
	require.NoError(t, err, "project survives the failed delete")
	for _, a := range []*store.Agent{f.a, f.b} {
		_, err = f.cs.GetAgent(ctx, a.ID)
		require.NoError(t, err, "agent survives the failed delete")
		_, err = f.cs.GetGroupMembership(ctx, f.group, store.GroupMemberTypeAgent, a.ID)
		assert.NoError(t, err, "the agent keeps its membership")
	}
	assert.Equal(t, 3, membershipRowCount(t, f.cs, f.group), "no membership was removed")
}
