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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCompositeStore creates a CompositeStore with a real SQLite base store
// and a separate Ent client, simulating the production dual-database layout.
func newTestCompositeStore(t *testing.T) *CompositeStore {
	t.Helper()

	// Create the base SQLite store (main database)
	base, err := sqlite.New(":memory:")
	require.NoError(t, err)
	require.NoError(t, base.Migrate(context.Background()))

	// Create a separate Ent-managed database (permissions database)
	entClient, _, err := entc.OpenSQLite("file:" + t.Name() + "?mode=memory&cache=shared")
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), entClient))

	cs := NewCompositeStore(base, entClient)
	t.Cleanup(func() { cs.Close() })

	return cs
}

func TestCompositeStore_AddGroupMember_UserShadowRecord(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create a user in the base store only (simulating normal user creation)
	userID := uuid.New().String()
	err := cs.Store.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "test@example.com",
		DisplayName: "Test User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	})
	require.NoError(t, err)

	// Create a group in Ent
	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Test Group",
		Slug:      "test-group",
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)

	// Add the user as a member — this should succeed because the CompositeStore
	// creates a shadow user record in the Ent database before adding the membership.
	err = cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	})
	require.NoError(t, err, "AddGroupMember should succeed for user that exists only in base store")

	// Verify the membership was created
	membership, err := cs.GetGroupMembership(ctx, groupID, store.GroupMemberTypeUser, userID)
	require.NoError(t, err)
	assert.Equal(t, userID, membership.MemberID)

	// Verify the user appears in effective groups
	groups, err := cs.GetEffectiveGroups(ctx, userID)
	require.NoError(t, err)
	assert.Contains(t, groups, groupID)
}

func TestCompositeStore_AddGroupMember_UserAlreadyInEnt(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	userID := uuid.New().String()
	userUID, _ := uuid.Parse(userID)

	// Create user in both base store and Ent
	err := cs.Store.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "already@example.com",
		DisplayName: "Already Here",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	})
	require.NoError(t, err)

	_, err = cs.client.User.Create().
		SetID(userUID).
		SetEmail("already@example.com").
		SetDisplayName("Already Here").
		Save(ctx)
	require.NoError(t, err)

	// Create a group
	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Test Group 2",
		Slug:      "test-group-2",
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)

	// Should work without issues (no duplicate creation)
	err = cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	})
	require.NoError(t, err)
}

func TestCompositeStore_AddGroupMember_AgentShadowRecord(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create a grove in the base store
	groveID := uuid.New().String()
	err := cs.Store.CreateGrove(ctx, &store.Grove{
		ID:      groveID,
		Name:    "Test Grove",
		Slug:    "test-grove",
		Created: time.Now(),
		Updated: time.Now(),
	})
	require.NoError(t, err)

	// Create an agent in the base store only
	agentID := uuid.New().String()
	err = cs.Store.CreateAgent(ctx, &store.Agent{
		ID:           agentID,
		Name:         "Test Agent",
		Slug:         "test-agent",
		GroveID:      groveID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	})
	require.NoError(t, err)

	// Create a group
	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Test Agent Group",
		Slug:      "test-agent-group",
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)

	// Add the agent as a member — should create shadow agent and grove records
	err = cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   agentID,
		Role:       store.GroupMemberRoleMember,
	})
	require.NoError(t, err, "AddGroupMember should succeed for agent that exists only in base store")

	// Verify membership
	membership, err := cs.GetGroupMembership(ctx, groupID, store.GroupMemberTypeAgent, agentID)
	require.NoError(t, err)
	assert.Equal(t, agentID, membership.MemberID)
}

func TestCompositeStore_AddGroupMember_Idempotent(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	userID := uuid.New().String()
	err := cs.Store.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "idempotent@example.com",
		DisplayName: "Idempotent User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	})
	require.NoError(t, err)

	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Idempotent Group",
		Slug:      "idempotent-group",
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)

	// First add should succeed
	member := &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	}
	err = cs.AddGroupMember(ctx, member)
	require.NoError(t, err)

	// Second add of same membership should return ErrAlreadyExists
	err = cs.AddGroupMember(ctx, member)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

// TestCompositeStore_CreateGroup_WithGroveID tests that creating a group with a
// grove ID succeeds even though the grove only exists in the base (SQLite) store.
// The CompositeStore should create a shadow grove record in the Ent database to
// satisfy the foreign key constraint.
func TestCompositeStore_CreateGroup_WithGroveID(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create a grove in the base store only (not in Ent)
	groveID := uuid.New().String()
	err := cs.Store.CreateGrove(ctx, &store.Grove{
		ID:      groveID,
		Name:    "Shadow Grove",
		Slug:    "shadow-grove",
		Created: time.Now(),
		Updated: time.Now(),
	})
	require.NoError(t, err)

	// Create a group with grove_id — this should succeed because the
	// CompositeStore creates a shadow grove record in Ent before creating
	// the group.
	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Shadow Grove Agents",
		Slug:      "grove:shadow-grove:agents",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   groveID,
	})
	require.NoError(t, err, "CreateGroup should succeed for grove that exists only in base store")

	// Verify the group was created with the correct grove ID
	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, groveID, group.GroveID)
	assert.Equal(t, "grove:shadow-grove:agents", group.Slug)
}

// TestCompositeStore_CreateGroup_MultipleGroupsPerGrove verifies that multiple
// groups (agents + members) can reference the same grove. The grove_id FK must
// NOT have a unique constraint.
func TestCompositeStore_CreateGroup_MultipleGroupsPerGrove(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	groveID := uuid.New().String()
	err := cs.Store.CreateGrove(ctx, &store.Grove{
		ID:      groveID,
		Name:    "Multi-Group Grove",
		Slug:    "multi-group-grove",
		Created: time.Now(),
		Updated: time.Now(),
	})
	require.NoError(t, err)

	// Create agents group
	agentsGroupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        agentsGroupID,
		Name:      "Multi-Group Grove Agents",
		Slug:      "grove:multi-group-grove:agents",
		GroupType: store.GroupTypeGroveAgents,
		GroveID:   groveID,
	})
	require.NoError(t, err, "agents group creation should succeed")

	// Create members group for the same grove — this must NOT fail
	membersGroupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        membersGroupID,
		Name:      "Multi-Group Grove Members",
		Slug:      "grove:multi-group-grove:members",
		GroupType: store.GroupTypeExplicit,
		GroveID:   groveID,
	})
	require.NoError(t, err, "members group creation should succeed for same grove")

	// Verify both groups exist with the correct grove ID
	agents, err := cs.GetGroup(ctx, agentsGroupID)
	require.NoError(t, err)
	assert.Equal(t, groveID, agents.GroveID)

	members, err := cs.GetGroup(ctx, membersGroupID)
	require.NoError(t, err)
	assert.Equal(t, groveID, members.GroveID)
}
