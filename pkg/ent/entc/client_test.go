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

package entc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/ent"
	"github.com/ptone/scion-agent/pkg/ent/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient creates an in-memory SQLite Ent client with auto-migration.
func newTestClient(t *testing.T) *ent.Client {
	t.Helper()
	client, err := OpenSQLite("file:" + t.Name() + "?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	require.NoError(t, AutoMigrate(context.Background(), client))
	return client
}

func TestOpenSQLite(t *testing.T) {
	client, err := OpenSQLite("file:TestOpenSQLite?mode=memory&cache=shared")
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, AutoMigrate(context.Background(), client))
}

func TestUserCRUD(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	// Create
	u, err := client.User.Create().
		SetEmail("alice@example.com").
		SetDisplayName("Alice").
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", u.Email)
	assert.Equal(t, "Alice", u.DisplayName)
	assert.Equal(t, user.RoleMember, u.Role)
	assert.Equal(t, user.StatusActive, u.Status)
	assert.NotEqual(t, uuid.Nil, u.ID)

	// Read
	fetched, err := client.User.Get(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, u.Email, fetched.Email)

	// Update
	updated, err := client.User.UpdateOneID(u.ID).
		SetDisplayName("Alice Updated").
		SetRole(user.RoleAdmin).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Alice Updated", updated.DisplayName)
	assert.Equal(t, user.RoleAdmin, updated.Role)

	// Delete
	err = client.User.DeleteOneID(u.ID).Exec(ctx)
	require.NoError(t, err)

	_, err = client.User.Get(ctx, u.ID)
	assert.True(t, ent.IsNotFound(err))
}

func TestUserEmailUnique(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	_, err := client.User.Create().
		SetEmail("dup@example.com").
		SetDisplayName("First").
		Save(ctx)
	require.NoError(t, err)

	_, err = client.User.Create().
		SetEmail("dup@example.com").
		SetDisplayName("Second").
		Save(ctx)
	assert.Error(t, err, "duplicate email should fail")
}

func TestGroveAndAgentEdge(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	// Create a grove
	g, err := client.Grove.Create().
		SetName("test-grove").
		SetSlug("test-grove").
		Save(ctx)
	require.NoError(t, err)

	// Create an agent linked to the grove
	a, err := client.Agent.Create().
		SetSlug("agent-1").
		SetName("Agent One").
		SetGrove(g).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, g.ID, a.GroveID)

	// Query agents through grove edge
	agents, err := client.Grove.QueryAgents(g).All(ctx)
	require.NoError(t, err)
	require.Len(t, agents, 1)
	assert.Equal(t, a.ID, agents[0].ID)
}

func TestAgentSlugGroveUnique(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	g, err := client.Grove.Create().
		SetName("grove").
		SetSlug("grove").
		Save(ctx)
	require.NoError(t, err)

	_, err = client.Agent.Create().
		SetSlug("dup-slug").
		SetName("Agent A").
		SetGrove(g).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.Agent.Create().
		SetSlug("dup-slug").
		SetName("Agent B").
		SetGrove(g).
		Save(ctx)
	assert.Error(t, err, "duplicate slug+grove_id should fail")
}

func TestGroupMembership(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	// Create a user and a group
	u, err := client.User.Create().
		SetEmail("bob@example.com").
		SetDisplayName("Bob").
		Save(ctx)
	require.NoError(t, err)

	grp, err := client.Group.Create().
		SetName("developers").
		SetSlug("developers").
		Save(ctx)
	require.NoError(t, err)

	// Create membership linking user to group
	m, err := client.GroupMembership.Create().
		SetGroup(grp).
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, m.ID)

	// Query memberships through group
	memberships, err := client.Group.QueryMemberships(grp).All(ctx)
	require.NoError(t, err)
	require.Len(t, memberships, 1)

	// Query user from membership
	memberUser, err := client.GroupMembership.QueryUser(m).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, u.ID, memberUser.ID)
}

func TestGroupMembershipUniqueConstraint(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	u, err := client.User.Create().
		SetEmail("carol@example.com").
		SetDisplayName("Carol").
		Save(ctx)
	require.NoError(t, err)

	grp, err := client.Group.Create().
		SetName("team").
		SetSlug("team").
		Save(ctx)
	require.NoError(t, err)

	_, err = client.GroupMembership.Create().
		SetGroup(grp).
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.GroupMembership.Create().
		SetGroup(grp).
		SetUser(u).
		Save(ctx)
	assert.Error(t, err, "duplicate group+user membership should fail")
}

func TestPolicyBindingEdges(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	// Create a policy
	pol, err := client.AccessPolicy.Create().
		SetName("read-all").
		SetScopeType("hub").
		SetResourceType("*").
		SetActions([]string{"read"}).
		SetEffect("allow").
		Save(ctx)
	require.NoError(t, err)

	// Create a user
	u, err := client.User.Create().
		SetEmail("dan@example.com").
		SetDisplayName("Dan").
		Save(ctx)
	require.NoError(t, err)

	// Create a policy binding
	pb, err := client.PolicyBinding.Create().
		SetPrincipalType("user").
		SetPolicy(pol).
		SetUser(u).
		Save(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, pb.ID)

	// Query bindings through policy
	bindings, err := client.AccessPolicy.QueryBindings(pol).All(ctx)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	assert.Equal(t, pb.ID, bindings[0].ID)
}

func TestGroupSelfReferentialEdge(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	// Create parent and child groups
	parent, err := client.Group.Create().
		SetName("engineering").
		SetSlug("engineering").
		Save(ctx)
	require.NoError(t, err)

	child, err := client.Group.Create().
		SetName("frontend").
		SetSlug("frontend").
		AddParentGroups(parent).
		Save(ctx)
	require.NoError(t, err)

	// Query children from parent
	children, err := client.Group.QueryChildGroups(parent).All(ctx)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, child.ID, children[0].ID)

	// Query parents from child
	parents, err := client.Group.QueryParentGroups(child).All(ctx)
	require.NoError(t, err)
	require.Len(t, parents, 1)
	assert.Equal(t, parent.ID, parents[0].ID)
}

func TestGroupGroveEdge(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	// Create a grove
	gv, err := client.Grove.Create().
		SetName("my-grove").
		SetSlug("my-grove").
		Save(ctx)
	require.NoError(t, err)

	// Create a grove_agents group linked to the grove
	grp, err := client.Group.Create().
		SetName("my-grove-agents").
		SetSlug("my-grove-agents").
		SetGroupType("grove_agents").
		SetGrove(gv).
		Save(ctx)
	require.NoError(t, err)
	assert.NotNil(t, grp.GroveID)

	// Query groups from grove
	groups, err := client.Grove.QueryGroups(gv).All(ctx)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, grp.ID, groups[0].ID)
}

func TestAgentOwnerAndCreatorEdges(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	creator, err := client.User.Create().
		SetEmail("creator@example.com").
		SetDisplayName("Creator").
		Save(ctx)
	require.NoError(t, err)

	owner, err := client.User.Create().
		SetEmail("owner@example.com").
		SetDisplayName("Owner").
		Save(ctx)
	require.NoError(t, err)

	gv, err := client.Grove.Create().
		SetName("gv").
		SetSlug("gv").
		Save(ctx)
	require.NoError(t, err)

	a, err := client.Agent.Create().
		SetSlug("owned-agent").
		SetName("Owned Agent").
		SetGrove(gv).
		SetCreator(creator).
		SetOwner(owner).
		SetDelegationEnabled(true).
		Save(ctx)
	require.NoError(t, err)
	assert.True(t, a.DelegationEnabled)

	// Verify edges
	createdAgents, err := client.User.QueryCreatedAgents(creator).All(ctx)
	require.NoError(t, err)
	require.Len(t, createdAgents, 1)
	assert.Equal(t, a.ID, createdAgents[0].ID)

	ownedAgents, err := client.User.QueryOwnedAgents(owner).All(ctx)
	require.NoError(t, err)
	require.Len(t, ownedAgents, 1)
	assert.Equal(t, a.ID, ownedAgents[0].ID)
}
