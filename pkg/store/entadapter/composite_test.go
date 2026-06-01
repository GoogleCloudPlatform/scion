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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestCompositeStore creates a CompositeStore backed by a single in-memory
// Ent database, matching the production single-database layout.
func newTestCompositeStore(t *testing.T) *CompositeStore {
	t.Helper()

	entClient := enttest.NewClient(t)

	cs := NewCompositeStore(entClient)
	t.Cleanup(func() { cs.Close() })

	return cs
}

func TestCompositeStore_AddGroupMember_User(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create a user. With a single Ent-backed database the user, group, and
	// membership all live in the same store and the FK resolves natively.
	userID := uuid.New().String()
	err := cs.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "test@example.com",
		DisplayName: "Test User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	})
	require.NoError(t, err)

	// Create a group.
	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Test Group",
		Slug:      "test-group",
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)

	// Add the user as a member.
	err = cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	})
	require.NoError(t, err, "AddGroupMember should succeed for an existing user")

	// Verify the membership was created.
	membership, err := cs.GetGroupMembership(ctx, groupID, store.GroupMemberTypeUser, userID)
	require.NoError(t, err)
	assert.Equal(t, userID, membership.MemberID)

	// Verify the user appears in effective groups.
	groups, err := cs.GetEffectiveGroups(ctx, userID)
	require.NoError(t, err)
	assert.Contains(t, groups, groupID)
}

func TestCompositeStore_AddGroupMember_Agent(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create a project.
	projectID := uuid.New().String()
	err := cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Test Project",
		Slug:    "test-project",
		Created: time.Now(),
		Updated: time.Now(),
	})
	require.NoError(t, err)

	// Create an agent referencing the project.
	agentID := uuid.New().String()
	err = cs.CreateAgent(ctx, &store.Agent{
		ID:           agentID,
		Name:         "Test Agent",
		Slug:         "test-agent",
		ProjectID:    projectID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	})
	require.NoError(t, err)

	// Create a group.
	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Test Agent Group",
		Slug:      "test-agent-group",
		GroupType: store.GroupTypeExplicit,
	})
	require.NoError(t, err)

	// Add the agent as a member.
	err = cs.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   agentID,
		Role:       store.GroupMemberRoleMember,
	})
	require.NoError(t, err, "AddGroupMember should succeed for an existing agent")

	// Verify membership.
	membership, err := cs.GetGroupMembership(ctx, groupID, store.GroupMemberTypeAgent, agentID)
	require.NoError(t, err)
	assert.Equal(t, agentID, membership.MemberID)
}

func TestCompositeStore_AddGroupMember_Idempotent(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	userID := uuid.New().String()
	err := cs.CreateUser(ctx, &store.User{
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

	// First add should succeed.
	member := &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	}
	err = cs.AddGroupMember(ctx, member)
	require.NoError(t, err)

	// Second add of same membership should return ErrAlreadyExists.
	err = cs.AddGroupMember(ctx, member)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

// TestCompositeStore_CreateGroup_WithProjectID verifies that creating a group
// referencing a project succeeds when the project lives in the same Ent store.
func TestCompositeStore_CreateGroup_WithProjectID(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	err := cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Project",
		Slug:    "project",
		Created: time.Now(),
		Updated: time.Now(),
	})
	require.NoError(t, err)

	groupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Project Agents",
		Slug:      "project:project:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
	})
	require.NoError(t, err, "CreateGroup should succeed for an existing project")

	group, err := cs.GetGroup(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, projectID, group.ProjectID)
	assert.Equal(t, "project:project:agents", group.Slug)
}

// TestCompositeStore_CreateGroup_MultipleGroupsPerProject verifies that multiple
// groups (agents + members) can reference the same project. The project_id FK must
// NOT have a unique constraint.
func TestCompositeStore_CreateGroup_MultipleGroupsPerProject(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	err := cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Multi-Group Project",
		Slug:    "multi-group-project",
		Created: time.Now(),
		Updated: time.Now(),
	})
	require.NoError(t, err)

	// Create agents group.
	agentsGroupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        agentsGroupID,
		Name:      "Multi-Group Project Agents",
		Slug:      "project:multi-group-project:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: projectID,
	})
	require.NoError(t, err, "agents group creation should succeed")

	// Create members group for the same project — this must NOT fail.
	membersGroupID := uuid.New().String()
	err = cs.CreateGroup(ctx, &store.Group{
		ID:        membersGroupID,
		Name:      "Multi-Group Project Members",
		Slug:      "project:multi-group-project:members",
		GroupType: store.GroupTypeExplicit,
		ProjectID: projectID,
	})
	require.NoError(t, err, "members group creation should succeed for same project")

	// Verify both groups exist with the correct project ID.
	agents, err := cs.GetGroup(ctx, agentsGroupID)
	require.NoError(t, err)
	assert.Equal(t, projectID, agents.ProjectID)

	members, err := cs.GetGroup(ctx, membersGroupID)
	require.NoError(t, err)
	assert.Equal(t, projectID, members.ProjectID)
}
