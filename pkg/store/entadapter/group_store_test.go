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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestGroupStore(t *testing.T) *GroupStore {
	t.Helper()
	client, _, err := entc.OpenSQLite("file:" + t.Name() + "?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))

	// Create a test user for membership tests
	_, err = client.User.Create().
		SetID(testUserUID).
		SetEmail("test@example.com").
		SetDisplayName("Test User").
		Save(context.Background())
	require.NoError(t, err)

	// Create a grove (required FK for agent)
	grove, err := client.Grove.Create().
		SetID(testGroveUID).
		SetName("test-grove").
		SetSlug("test-grove").
		Save(context.Background())
	require.NoError(t, err)

	// Create a test agent for membership tests
	_, err = client.Agent.Create().
		SetID(testAgentUID).
		SetName("test-agent").
		SetSlug("test-agent").
		SetGrove(grove).
		Save(context.Background())
	require.NoError(t, err)

	return NewGroupStore(client)
}

var (
	testUserUID  = uuid.MustParse("10000000-0000-0000-0000-000000000001")
	testAgentUID = uuid.MustParse("20000000-0000-0000-0000-000000000001")
	testGroveUID = uuid.MustParse("30000000-0000-0000-0000-000000000001")
)

func TestCreateGroup(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:          uuid.New().String(),
		Name:        "Engineering",
		Slug:        "engineering",
		Description: "Engineering team",
	}

	err := gs.CreateGroup(ctx, g)
	require.NoError(t, err)
	assert.False(t, g.Created.IsZero())
	assert.False(t, g.Updated.IsZero())
	assert.Equal(t, store.GroupTypeExplicit, g.GroupType)
}

func TestCreateGroupDuplicate(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Engineering",
		Slug: "engineering",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	g2 := &store.Group{
		ID:   uuid.New().String(),
		Name: "Engineering 2",
		Slug: "engineering", // same slug
	}
	err := gs.CreateGroup(ctx, g2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestGetGroup(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	id := uuid.New().String()
	g := &store.Group{
		ID:          id,
		Name:        "Platform",
		Slug:        "platform",
		Description: "Platform team",
		Labels:      map[string]string{"dept": "eng"},
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	got, err := gs.GetGroup(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "Platform", got.Name)
	assert.Equal(t, "platform", got.Slug)
	assert.Equal(t, "Platform team", got.Description)
	assert.Equal(t, "eng", got.Labels["dept"])
	assert.Equal(t, store.GroupTypeExplicit, got.GroupType)
}

func TestGetGroupNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	_, err := gs.GetGroup(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestGetGroupBySlug(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Ops Team",
		Slug: "ops-team",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	got, err := gs.GetGroupBySlug(ctx, "ops-team")
	require.NoError(t, err)
	assert.Equal(t, g.ID, got.ID)
	assert.Equal(t, "Ops Team", got.Name)
}

func TestGetGroupBySlugNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	_, err := gs.GetGroupBySlug(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateGroup(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Old Name",
		Slug: "old-name",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	g.Name = "New Name"
	g.Description = "Updated"
	err := gs.UpdateGroup(ctx, g)
	require.NoError(t, err)

	got, err := gs.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, "New Name", got.Name)
	assert.Equal(t, "Updated", got.Description)
}

func TestUpdateGroupNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Ghost",
		Slug: "ghost",
	}
	err := gs.UpdateGroup(ctx, g)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteGroup(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Delete Me",
		Slug: "delete-me",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	err := gs.DeleteGroup(ctx, g.ID)
	require.NoError(t, err)

	_, err = gs.GetGroup(ctx, g.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteGroupNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	err := gs.DeleteGroup(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestListGroups(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		g := &store.Group{
			ID:   uuid.New().String(),
			Name: "Group " + string(rune('A'+i)),
			Slug: "group-" + string(rune('a'+i)),
		}
		require.NoError(t, gs.CreateGroup(ctx, g))
	}

	result, err := gs.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)
	assert.Len(t, result.Items, 3)
}

func TestListGroupsWithGroupTypeFilter(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g1 := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Explicit Group",
		Slug:      "explicit-group",
		GroupType: store.GroupTypeExplicit,
	}
	require.NoError(t, gs.CreateGroup(ctx, g1))

	// For grove_agents, we create directly to bypass the API guard
	g2 := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Grove Group",
		Slug:      "grove-group",
		GroupType: store.GroupTypeGroveAgents,
	}
	require.NoError(t, gs.CreateGroup(ctx, g2))

	result, err := gs.ListGroups(ctx, store.GroupFilter{GroupType: store.GroupTypeExplicit}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, store.GroupTypeExplicit, result.Items[0].GroupType)

	result, err = gs.ListGroups(ctx, store.GroupFilter{GroupType: store.GroupTypeGroveAgents}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, store.GroupTypeGroveAgents, result.Items[0].GroupType)
}

func TestListGroupsWithLimit(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		g := &store.Group{
			ID:   uuid.New().String(),
			Name: "Group " + string(rune('A'+i)),
			Slug: "group-" + string(rune('a'+i)),
		}
		require.NoError(t, gs.CreateGroup(ctx, g))
	}

	result, err := gs.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, 5, result.TotalCount)
	assert.Len(t, result.Items, 2)
}

func TestAddGroupMemberUser(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test Group",
		Slug: "test-group",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	member := &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}

	err := gs.AddGroupMember(ctx, member)
	require.NoError(t, err)
	assert.False(t, member.AddedAt.IsZero())
}

func TestAddGroupMemberAgent(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test Group",
		Slug: "test-group-agent",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	member := &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   testAgentUID.String(),
		Role:       store.GroupMemberRoleMember,
	}

	err := gs.AddGroupMember(ctx, member)
	require.NoError(t, err)
	assert.False(t, member.AddedAt.IsZero())

	// Verify we can get the membership back
	members, err := gs.GetGroupMembers(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, store.GroupMemberTypeAgent, members[0].MemberType)
	assert.Equal(t, testAgentUID.String(), members[0].MemberID)
}

func TestAddGroupMemberDuplicate(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test Group",
		Slug: "test-group-dup",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	member := &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}

	require.NoError(t, gs.AddGroupMember(ctx, member))

	err := gs.AddGroupMember(ctx, member)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestAddGroupMemberGroupNesting(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	parent := &store.Group{
		ID:   uuid.New().String(),
		Name: "Parent",
		Slug: "parent",
	}
	child := &store.Group{
		ID:   uuid.New().String(),
		Name: "Child",
		Slug: "child",
	}
	require.NoError(t, gs.CreateGroup(ctx, parent))
	require.NoError(t, gs.CreateGroup(ctx, child))

	member := &store.GroupMember{
		GroupID:    parent.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   child.ID,
		Role:       store.GroupMemberRoleMember,
	}

	err := gs.AddGroupMember(ctx, member)
	require.NoError(t, err)

	// Verify child shows up in members
	members, err := gs.GetGroupMembers(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, store.GroupMemberTypeGroup, members[0].MemberType)
	assert.Equal(t, child.ID, members[0].MemberID)
}

func TestRemoveGroupMemberUser(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test Group",
		Slug: "test-group-rm",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	member := &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}
	require.NoError(t, gs.AddGroupMember(ctx, member))

	err := gs.RemoveGroupMember(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	require.NoError(t, err)

	_, err = gs.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRemoveGroupMemberAgent(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test Group",
		Slug: "test-group-rm-agent",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	member := &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   testAgentUID.String(),
		Role:       store.GroupMemberRoleMember,
	}
	require.NoError(t, gs.AddGroupMember(ctx, member))

	err := gs.RemoveGroupMember(ctx, g.ID, store.GroupMemberTypeAgent, testAgentUID.String())
	require.NoError(t, err)

	_, err = gs.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeAgent, testAgentUID.String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRemoveGroupMemberNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test Group",
		Slug: "test-group-rm-nf",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	err := gs.RemoveGroupMember(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestGetGroupMembers(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Mixed Group",
		Slug: "mixed-group",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	child := &store.Group{
		ID:   uuid.New().String(),
		Name: "Child Group",
		Slug: "child-group",
	}
	require.NoError(t, gs.CreateGroup(ctx, child))

	// Add user member
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	// Add agent member
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   testAgentUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	// Add group member
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   child.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	members, err := gs.GetGroupMembers(ctx, g.ID)
	require.NoError(t, err)
	assert.Len(t, members, 3)

	// Count by type
	typeCounts := map[string]int{}
	for _, m := range members {
		typeCounts[m.MemberType]++
	}
	assert.Equal(t, 1, typeCounts[store.GroupMemberTypeUser])
	assert.Equal(t, 1, typeCounts[store.GroupMemberTypeAgent])
	assert.Equal(t, 1, typeCounts[store.GroupMemberTypeGroup])
}

func TestGetUserGroups(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g1 := &store.Group{
		ID:   uuid.New().String(),
		Name: "Group 1",
		Slug: "group-1",
	}
	g2 := &store.Group{
		ID:   uuid.New().String(),
		Name: "Group 2",
		Slug: "group-2",
	}
	require.NoError(t, gs.CreateGroup(ctx, g1))
	require.NoError(t, gs.CreateGroup(ctx, g2))

	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g1.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g2.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleAdmin,
	}))

	groups, err := gs.GetUserGroups(ctx, testUserUID.String())
	require.NoError(t, err)
	assert.Len(t, groups, 2)
}

func TestGetGroupMembershipUser(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test",
		Slug: "test-membership",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleAdmin,
	}))

	m, err := gs.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleAdmin, m.Role)
	assert.Equal(t, store.GroupMemberTypeUser, m.MemberType)
	assert.Equal(t, testUserUID.String(), m.MemberID)
}

func TestGetGroupMembershipGroup(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	parent := &store.Group{
		ID:   uuid.New().String(),
		Name: "Parent",
		Slug: "parent-gm",
	}
	child := &store.Group{
		ID:   uuid.New().String(),
		Name: "Child",
		Slug: "child-gm",
	}
	require.NoError(t, gs.CreateGroup(ctx, parent))
	require.NoError(t, gs.CreateGroup(ctx, child))

	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    parent.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   child.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	m, err := gs.GetGroupMembership(ctx, parent.ID, store.GroupMemberTypeGroup, child.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberTypeGroup, m.MemberType)
	assert.Equal(t, child.ID, m.MemberID)
}

func TestGetGroupMembershipNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Test",
		Slug: "test-gm-nf",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	_, err := gs.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestWouldCreateCycleSelf(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Self",
		Slug: "self-cycle",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	wouldCycle, err := gs.WouldCreateCycle(ctx, g.ID, g.ID)
	require.NoError(t, err)
	assert.True(t, wouldCycle)
}

func TestWouldCreateCycleDirect(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	a := &store.Group{
		ID:   uuid.New().String(),
		Name: "A",
		Slug: "cycle-a",
	}
	b := &store.Group{
		ID:   uuid.New().String(),
		Name: "B",
		Slug: "cycle-b",
	}
	require.NoError(t, gs.CreateGroup(ctx, a))
	require.NoError(t, gs.CreateGroup(ctx, b))

	// A contains B
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    a.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   b.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Would B containing A create a cycle?
	wouldCycle, err := gs.WouldCreateCycle(ctx, b.ID, a.ID)
	require.NoError(t, err)
	assert.True(t, wouldCycle)
}

func TestWouldCreateCycleTransitive(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	a := &store.Group{ID: uuid.New().String(), Name: "A", Slug: "trans-a"}
	b := &store.Group{ID: uuid.New().String(), Name: "B", Slug: "trans-b"}
	c := &store.Group{ID: uuid.New().String(), Name: "C", Slug: "trans-c"}
	require.NoError(t, gs.CreateGroup(ctx, a))
	require.NoError(t, gs.CreateGroup(ctx, b))
	require.NoError(t, gs.CreateGroup(ctx, c))

	// A contains B, B contains C
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID: a.ID, MemberType: store.GroupMemberTypeGroup, MemberID: b.ID, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID: b.ID, MemberType: store.GroupMemberTypeGroup, MemberID: c.ID, Role: store.GroupMemberRoleMember,
	}))

	// Would C containing A create a cycle?
	wouldCycle, err := gs.WouldCreateCycle(ctx, c.ID, a.ID)
	require.NoError(t, err)
	assert.True(t, wouldCycle)

	// A containing C should NOT create a cycle (C is already in A, but it's not circular)
	wouldCycle, err = gs.WouldCreateCycle(ctx, a.ID, c.ID)
	require.NoError(t, err)
	assert.False(t, wouldCycle) // C doesn't contain A anywhere
}

func TestWouldCreateCycleNoCycle(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	a := &store.Group{ID: uuid.New().String(), Name: "A", Slug: "nc-a"}
	b := &store.Group{ID: uuid.New().String(), Name: "B", Slug: "nc-b"}
	require.NoError(t, gs.CreateGroup(ctx, a))
	require.NoError(t, gs.CreateGroup(ctx, b))

	// Neither contains the other
	wouldCycle, err := gs.WouldCreateCycle(ctx, a.ID, b.ID)
	require.NoError(t, err)
	assert.False(t, wouldCycle)
}

func TestGroveGroupGuardAddMember(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Grove Group",
		Slug:      "grove-guard",
		GroupType: store.GroupTypeGroveAgents,
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	member := &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}
	err := gs.AddGroupMember(ctx, member)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestGroveGroupGuardRemoveMember(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Grove Group",
		Slug:      "grove-guard-rm",
		GroupType: store.GroupTypeGroveAgents,
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	err := gs.RemoveGroupMember(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestGetEffectiveGroups(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Create a group hierarchy: A contains B, B contains C
	a := &store.Group{ID: uuid.New().String(), Name: "A", Slug: "eff-a"}
	b := &store.Group{ID: uuid.New().String(), Name: "B", Slug: "eff-b"}
	c := &store.Group{ID: uuid.New().String(), Name: "C", Slug: "eff-c"}
	require.NoError(t, gs.CreateGroup(ctx, a))
	require.NoError(t, gs.CreateGroup(ctx, b))
	require.NoError(t, gs.CreateGroup(ctx, c))

	// B is a child of A (A contains B)
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID: a.ID, MemberType: store.GroupMemberTypeGroup, MemberID: b.ID, Role: store.GroupMemberRoleMember,
	}))
	// C is a child of B (B contains C)
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID: b.ID, MemberType: store.GroupMemberTypeGroup, MemberID: c.ID, Role: store.GroupMemberRoleMember,
	}))

	// User is member of C
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID: c.ID, MemberType: store.GroupMemberTypeUser, MemberID: testUserUID.String(), Role: store.GroupMemberRoleMember,
	}))

	effective, err := gs.GetEffectiveGroups(ctx, testUserUID.String())
	require.NoError(t, err)

	// User should be in C, and also in B and A through transitive parent_groups expansion
	assert.Len(t, effective, 3)

	found := make(map[string]bool)
	for _, gid := range effective {
		found[gid] = true
	}
	assert.True(t, found[a.ID], "expected group A")
	assert.True(t, found[b.ID], "expected group B")
	assert.True(t, found[c.ID], "expected group C")
}

func TestGetEffectiveGroupsNoMemberships(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	effective, err := gs.GetEffectiveGroups(ctx, testUserUID.String())
	require.NoError(t, err)
	assert.Empty(t, effective)
}

func TestCompositeStoreDelegation(t *testing.T) {
	// Verify the CompositeStore properly delegates group operations
	client, _, err := entc.OpenSQLite("file:" + t.Name() + "?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))

	// We use nil as the base store since we're only testing group methods
	// and they should all go to the Ent adapter.
	composite := NewCompositeStore(nil, client)

	ctx := context.Background()
	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Composite Test",
		Slug: "composite-test",
	}

	err = composite.CreateGroup(ctx, g)
	require.NoError(t, err)

	got, err := composite.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, "Composite Test", got.Name)

	got, err = composite.GetGroupBySlug(ctx, "composite-test")
	require.NoError(t, err)
	assert.Equal(t, g.ID, got.ID)
}

func TestDeleteGroupCascadesMemberships(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Group With Members",
		Slug: "group-cascade",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	// Add a user member
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	// Delete group
	err := gs.DeleteGroup(ctx, g.ID)
	require.NoError(t, err)

	// Group should be gone
	_, err = gs.GetGroup(ctx, g.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Phase 3: Dynamic Grove Groups
// =============================================================================

func TestCreateGroveGroup(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Test Grove Agents",
		Slug:      "grove:test-grove:agents",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   testGroveUID.String(),
	}

	err := gs.CreateGroup(ctx, g)
	require.NoError(t, err)
	assert.False(t, g.Created.IsZero())
	assert.Equal(t, store.GroupTypeGroveAgents, g.GroupType)

	// Verify persisted correctly
	got, err := gs.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GroupTypeGroveAgents, got.GroupType)
	assert.Equal(t, testGroveUID.String(), got.GroveID)
}

func TestGetGroupByGroveID(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Test Grove Agents",
		Slug:      "grove:test-grove:agents",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   testGroveUID.String(),
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	got, err := gs.GetGroupByGroveID(ctx, testGroveUID.String())
	require.NoError(t, err)
	assert.Equal(t, g.ID, got.ID)
	assert.Equal(t, store.GroupTypeGroveAgents, got.GroupType)
	assert.Equal(t, testGroveUID.String(), got.GroveID)
}

func TestGetGroupByGroveIDNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	_, err := gs.GetGroupByGroveID(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestGetGroupMembersGroveGroup(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Create a grove_agents group linked to the test grove
	groveGroup := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Test Grove Agents",
		Slug:      "grove:test-grove:agents-members",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   testGroveUID.String(),
	}
	require.NoError(t, gs.CreateGroup(ctx, groveGroup))

	// The test setup already created an agent in the test grove (testAgentUID).
	// Create a second agent in the same grove to verify multiple agents are returned.
	agent2UID := uuid.MustParse("20000000-0000-0000-0000-000000000002")
	grove, err := gs.client.Grove.Get(ctx, testGroveUID)
	require.NoError(t, err)
	_, err = gs.client.Agent.Create().
		SetID(agent2UID).
		SetName("test-agent-2").
		SetSlug("test-agent-2").
		SetGrove(grove).
		Save(ctx)
	require.NoError(t, err)

	// Query-time resolution should return both agents
	members, err := gs.GetGroupMembers(ctx, groveGroup.ID)
	require.NoError(t, err)
	assert.Len(t, members, 2)

	memberIDs := make(map[string]bool)
	for _, m := range members {
		assert.Equal(t, store.GroupMemberTypeAgent, m.MemberType)
		assert.Equal(t, store.GroupMemberRoleMember, m.Role)
		assert.Equal(t, "system", m.AddedBy)
		memberIDs[m.MemberID] = true
	}
	assert.True(t, memberIDs[testAgentUID.String()])
	assert.True(t, memberIDs[agent2UID.String()])
}

func TestGetEffectiveGroupsForAgent(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Create a grove_agents group linked to the test grove
	groveGroup := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Test Grove Agents",
		Slug:      "grove:test-grove:agents-eff",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   testGroveUID.String(),
	}
	require.NoError(t, gs.CreateGroup(ctx, groveGroup))

	// Create a parent group that contains the grove group
	parentGroup := &store.Group{
		ID:   uuid.New().String(),
		Name: "All Agents Parent",
		Slug: "all-agents-parent",
	}
	require.NoError(t, gs.CreateGroup(ctx, parentGroup))

	// Add grove group as child of parent
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    parentGroup.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   groveGroup.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create an explicit group and add the agent to it
	explicitGroup := &store.Group{
		ID:   uuid.New().String(),
		Name: "Explicit Group",
		Slug: "explicit-group-eff",
	}
	require.NoError(t, gs.CreateGroup(ctx, explicitGroup))
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    explicitGroup.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   testAgentUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	// Get effective groups for the agent
	effective, err := gs.GetEffectiveGroupsForAgent(ctx, testAgentUID.String())
	require.NoError(t, err)

	// Should include: grove group, parent group (transitive), explicit group
	found := make(map[string]bool)
	for _, gid := range effective {
		found[gid] = true
	}
	assert.True(t, found[groveGroup.ID], "expected grove group")
	assert.True(t, found[parentGroup.ID], "expected parent group (transitive)")
	assert.True(t, found[explicitGroup.ID], "expected explicit group")
	assert.Len(t, effective, 3)
}

func TestGetEffectiveGroupsForAgentNoGroups(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// No grove group or explicit memberships — should return empty
	effective, err := gs.GetEffectiveGroupsForAgent(ctx, testAgentUID.String())
	require.NoError(t, err)
	assert.Empty(t, effective)
}

func TestGroveGroupLifecycle(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// 1. Create a grove group (simulating what the handler does)
	groveGroup := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Test Grove Agents",
		Slug:      "grove:test-grove:agents-lc",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   testGroveUID.String(),
	}
	require.NoError(t, gs.CreateGroup(ctx, groveGroup))

	// 2. Verify it can be looked up by grove ID
	got, err := gs.GetGroupByGroveID(ctx, testGroveUID.String())
	require.NoError(t, err)
	assert.Equal(t, groveGroup.ID, got.ID)

	// 3. Verify agents show up as members
	members, err := gs.GetGroupMembers(ctx, groveGroup.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1) // testAgentUID is in the test grove
	assert.Equal(t, testAgentUID.String(), members[0].MemberID)

	// 4. Delete the grove group (simulating grove deletion)
	require.NoError(t, gs.DeleteGroup(ctx, groveGroup.ID))

	// 5. Verify it's gone
	_, err = gs.GetGroupByGroveID(ctx, testGroveUID.String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Phase 4: CheckDelegatedAccess and GetGroupsByIDs
// =============================================================================

func TestCheckDelegatedAccess_Enabled(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Enable delegation on the test agent and set creator
	_, err := gs.client.Agent.UpdateOneID(testAgentUID).
		SetDelegationEnabled(true).
		SetCreatorID(testUserUID).
		Save(ctx)
	require.NoError(t, err)

	// Check with matching DelegatedFrom condition
	conditions := &store.PolicyConditions{
		DelegatedFrom: &store.DelegatedFromCondition{
			PrincipalType: "user",
			PrincipalID:   testUserUID.String(),
		},
	}
	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), conditions)
	require.NoError(t, err)
	assert.True(t, result)
}

func TestCheckDelegatedAccess_Disabled(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Creator is set but delegation is disabled (default)
	_, err := gs.client.Agent.UpdateOneID(testAgentUID).
		SetCreatorID(testUserUID).
		Save(ctx)
	require.NoError(t, err)

	conditions := &store.PolicyConditions{
		DelegatedFrom: &store.DelegatedFromCondition{
			PrincipalType: "user",
			PrincipalID:   testUserUID.String(),
		},
	}
	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), conditions)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestCheckDelegatedAccess_SuspendedCreator(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Suspend the creator
	_, err := gs.client.User.UpdateOneID(testUserUID).
		SetStatus("suspended").
		Save(ctx)
	require.NoError(t, err)

	// Enable delegation
	_, err = gs.client.Agent.UpdateOneID(testAgentUID).
		SetDelegationEnabled(true).
		SetCreatorID(testUserUID).
		Save(ctx)
	require.NoError(t, err)

	conditions := &store.PolicyConditions{
		DelegatedFrom: &store.DelegatedFromCondition{
			PrincipalType: "user",
			PrincipalID:   testUserUID.String(),
		},
	}
	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), conditions)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestCheckDelegatedAccess_NoCreator(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Enable delegation but don't set a creator
	_, err := gs.client.Agent.UpdateOneID(testAgentUID).
		SetDelegationEnabled(true).
		Save(ctx)
	require.NoError(t, err)

	conditions := &store.PolicyConditions{
		DelegatedFrom: &store.DelegatedFromCondition{
			PrincipalType: "user",
			PrincipalID:   testUserUID.String(),
		},
	}
	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), conditions)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestCheckDelegatedAccess_GroupCondition(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Create a group and add the user to it
	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Platform Team",
		Slug: "platform-team-deleg",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	// Enable delegation and set creator
	_, err := gs.client.Agent.UpdateOneID(testAgentUID).
		SetDelegationEnabled(true).
		SetCreatorID(testUserUID).
		Save(ctx)
	require.NoError(t, err)

	// Check with DelegatedFromGroup matching the creator's group
	conditions := &store.PolicyConditions{
		DelegatedFromGroup: g.ID,
	}
	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), conditions)
	require.NoError(t, err)
	assert.True(t, result)
}

func TestCheckDelegatedAccess_GroupCondition_NotMember(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Create a group but DON'T add the user to it
	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Other Team",
		Slug: "other-team-deleg",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	// Enable delegation and set creator
	_, err := gs.client.Agent.UpdateOneID(testAgentUID).
		SetDelegationEnabled(true).
		SetCreatorID(testUserUID).
		Save(ctx)
	require.NoError(t, err)

	// Check with DelegatedFromGroup — creator is NOT a member
	conditions := &store.PolicyConditions{
		DelegatedFromGroup: g.ID,
	}
	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), conditions)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestCheckDelegatedAccess_NilConditions(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), nil)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestCheckDelegatedAccess_NoDelegationConditions(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Conditions exist but no delegation fields
	conditions := &store.PolicyConditions{
		Labels: map[string]string{"env": "prod"},
	}
	result, err := gs.CheckDelegatedAccess(ctx, testAgentUID.String(), conditions)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestGetGroupsByIDs(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g1 := &store.Group{ID: uuid.New().String(), Name: "Group 1", Slug: "gbi-1"}
	g2 := &store.Group{ID: uuid.New().String(), Name: "Group 2", Slug: "gbi-2"}
	require.NoError(t, gs.CreateGroup(ctx, g1))
	require.NoError(t, gs.CreateGroup(ctx, g2))

	groups, err := gs.GetGroupsByIDs(ctx, []string{g1.ID, g2.ID})
	require.NoError(t, err)
	assert.Len(t, groups, 2)

	names := map[string]bool{}
	for _, g := range groups {
		names[g.Name] = true
	}
	assert.True(t, names["Group 1"])
	assert.True(t, names["Group 2"])
}

func TestGetGroupsByIDs_Empty(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	groups, err := gs.GetGroupsByIDs(ctx, []string{})
	require.NoError(t, err)
	assert.Nil(t, groups)
}

func TestGetGroupsByIDs_MissingIDs(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g1 := &store.Group{ID: uuid.New().String(), Name: "Exists", Slug: "gbi-exists"}
	require.NoError(t, gs.CreateGroup(ctx, g1))

	// One valid, one missing
	groups, err := gs.GetGroupsByIDs(ctx, []string{g1.ID, uuid.New().String()})
	require.NoError(t, err)
	assert.Len(t, groups, 1)
	assert.Equal(t, "Exists", groups[0].Name)
}

func TestUpdateGroupMemberRole(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Role Update",
		Slug: "role-update",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	// Add a user as member
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	// Verify initial role
	m, err := gs.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleMember, m.Role)

	// Promote to owner
	err = gs.UpdateGroupMemberRole(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String(), store.GroupMemberRoleOwner)
	require.NoError(t, err)

	// Verify updated role
	m, err = gs.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String())
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleOwner, m.Role)
}

func TestUpdateGroupMemberRoleNotFound(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Role NF",
		Slug: "role-nf",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	err := gs.UpdateGroupMemberRole(ctx, g.ID, store.GroupMemberTypeUser, testUserUID.String(), store.GroupMemberRoleOwner)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateGroupMemberRoleAgent(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Agent Role",
		Slug: "agent-role-update",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   testAgentUID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	err := gs.UpdateGroupMemberRole(ctx, g.ID, store.GroupMemberTypeAgent, testAgentUID.String(), store.GroupMemberRoleAdmin)
	require.NoError(t, err)

	m, err := gs.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeAgent, testAgentUID.String())
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleAdmin, m.Role)
}

func TestCountGroupMembersByRole(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	// Create a second user for the test
	testUser2UID := uuid.MustParse("10000000-0000-0000-0000-000000000002")
	_, err := gs.client.User.Create().
		SetID(testUser2UID).
		SetEmail("test2@example.com").
		SetDisplayName("Test User 2").
		Save(ctx)
	require.NoError(t, err)

	g := &store.Group{
		ID:   uuid.New().String(),
		Name: "Count Roles",
		Slug: "count-roles",
	}
	require.NoError(t, gs.CreateGroup(ctx, g))

	// No owners initially
	count, err := gs.CountGroupMembersByRole(ctx, g.ID, store.GroupMemberRoleOwner)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// Add one owner
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUserUID.String(),
		Role:       store.GroupMemberRoleOwner,
	}))

	count, err = gs.CountGroupMembersByRole(ctx, g.ID, store.GroupMemberRoleOwner)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Add a member (not owner)
	require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    g.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   testUser2UID.String(),
		Role:       store.GroupMemberRoleMember,
	}))

	// Owner count should still be 1
	count, err = gs.CountGroupMembersByRole(ctx, g.ID, store.GroupMemberRoleOwner)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Member count should be 1
	count, err = gs.CountGroupMembersByRole(ctx, g.ID, store.GroupMemberRoleMember)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestListGroupsWithGroveIDFilter(t *testing.T) {
	gs := newTestGroupStore(t)
	ctx := context.Background()

	groveID1 := testGroveUID.String()
	groveID2 := uuid.New().String()

	// Create a second grove for the filter test
	_, err := gs.client.Grove.Create().
		SetID(uuid.MustParse(groveID2)).
		SetName("grove-2").
		SetSlug("grove-2").
		Save(ctx)
	require.NoError(t, err)

	g1 := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Grove 1 Agents",
		Slug:      "grove:grove-1:agents",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   groveID1,
	}
	g2 := &store.Group{
		ID:        uuid.New().String(),
		Name:      "Grove 2 Agents",
		Slug:      "grove:grove-2:agents",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   groveID2,
	}
	require.NoError(t, gs.CreateGroup(ctx, g1))
	require.NoError(t, gs.CreateGroup(ctx, g2))

	result, err := gs.ListGroups(ctx, store.GroupFilter{GroveID: groveID1}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, g1.ID, result.Items[0].ID)
}
