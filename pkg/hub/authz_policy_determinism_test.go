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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPolicyDeterministicOrdering verifies that role bindings inserted in
// different orders produce identical authorization decisions. CO1: Policies
// replaced by role bindings — the AK1 kernel only evaluates role bindings.
func TestPolicyDeterministicOrdering(t *testing.T) {
	// Test multiple insertion orders of role bindings with different permissions.
	// All three bindings grant agent.read, so the decision should always be
	// "allowed" regardless of insertion order.
	orders := [][]string{
		{"binding-1", "binding-2", "binding-3"},
		{"binding-3", "binding-1", "binding-2"},
		{"binding-2", "binding-3", "binding-1"},
	}

	var expectedDecision *Decision
	for i, order := range orders {
		authz, s := authzTestSetup(t)
		ctx := context.Background()

		// Create user
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
		}))

		// Create three role definitions with agent.read permission
		roleDefs := map[string]*store.RoleDefinition{
			"binding-1": createTestRoleDefinition(t, s, "role-det-1", store.RoleScopeSystem, []string{"agent.read"}),
			"binding-2": createTestRoleDefinition(t, s, "role-det-2", store.RoleScopeSystem, []string{"agent.read", "agent.list"}),
			"binding-3": createTestRoleDefinition(t, s, "role-det-3", store.RoleScopeSystem, []string{"agent.read", "agent.update"}),
		}

		// Insert role bindings in the specified order
		for _, bindingID := range order {
			rd := roleDefs[bindingID]
			_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: rd.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      tid("user-1"),
				ScopeType:        store.RoleScopeSystem,
				CreatedBy:        "test",
			})
			require.NoError(t, err)
		}

		// Evaluate
		user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
		resource := Resource{Type: "agent", ID: tid("agent-1")}

		decision := authz.CheckAccess(ctx, user, resource, ActionRead)

		// First iteration: capture expected decision
		if i == 0 {
			expectedDecision = &decision
		} else {
			// Subsequent iterations: verify same decision
			assert.Equal(t, expectedDecision.Allowed, decision.Allowed,
				"Order %d: decision should be deterministic", i)
			assert.Equal(t, expectedDecision.Reason, decision.Reason,
				"Order %d: reason should be deterministic", i)
		}
	}

	// All role bindings grant agent.read, so the decision should be allowed.
	require.NotNil(t, expectedDecision)
	assert.True(t, expectedDecision.Allowed, "role binding granting agent.read should allow access")
	assert.Equal(t, "role binding grant", expectedDecision.Reason)
}

// TestPolicyPriorityPrecedence verifies that in the AK1 kernel, role bindings
// are additive (allow-only). A user with a role binding granting agent.read can
// read agents; without agent.delete permission, delete is denied.
// CO1: The AK1 kernel has no policy priority or deny effect. Role bindings
// only grant — they never deny.
func TestPolicyPriorityPrecedence(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// Create a role with only agent.read (no agent.delete)
	rd := createTestRoleDefinition(t, s, "prio-agent-read", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-1"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	// agent.read is granted
	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed, "role binding with agent.read should allow read")
	assert.Equal(t, "role binding grant", decision.Reason)

	// agent.delete is not granted — denied because bindings do not include the permission
	decision = authz.CheckAccess(ctx, user, resource, ActionDelete)
	assert.False(t, decision.Allowed, "without agent.delete permission, delete should be denied")
	assert.Contains(t, decision.Reason, "agent.delete")
}

// TestPolicyKindPrecedence verifies that multiple role bindings are additive.
// CO1: The AK1 kernel has no policy kind (explicit vs default) concept.
// Multiple role bindings combine additively — if any binding grants the
// requested permission, access is allowed.
func TestPolicyKindPrecedence(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// Create first role with agent.list only
	rd1 := createTestRoleDefinition(t, s, "kind-agent-list", store.RoleScopeSystem, []string{"agent.list"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd1.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-1"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create second role with agent.read
	rd2 := createTestRoleDefinition(t, s, "kind-agent-read", store.RoleScopeSystem, []string{"agent.read"})
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd2.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-1"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	// agent.read is granted by the second role binding (additive)
	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed, "additive role bindings should grant agent.read")
	assert.Equal(t, "role binding grant", decision.Reason)
}

// TestPolicyLocalOverride verifies that a project-scoped role binding grants
// access within its project. CO1: The AK1 kernel evaluates project-scoped
// bindings for resources within the project.
func TestPolicyLocalOverride(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-1"), Slug: "test-project", Name: "Test Project",
	}))

	// Create a project-scoped role binding granting agent.delete (project-owner has delete)
	createTestUserWithProjectRole(t, s, tid("user-1"), "user1@test.com", tid("project-1"), store.ProjectRoleOwner)

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), ParentType: "project", ParentID: tid("project-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionDelete)

	// Project-scoped role binding should grant access within the project
	assert.True(t, decision.Allowed, "project-scoped role binding should grant access")
	assert.Equal(t, "project", decision.Scope)
	assert.Equal(t, "role binding grant", decision.Reason)
}

// TestPolicyResourceOverride verifies that a user without a role binding for
// the requested permission on a resource is denied, even if they have
// bindings for other permissions. CO1: The AK1 kernel has no resource-scoped
// policies. Access is determined by system- and project-scoped role bindings.
func TestPolicyResourceOverride(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-1"), Slug: "test-project", Name: "Test Project",
	}))

	// Give the user project-member role (read/list only, no delete)
	createTestUserWithProjectRole(t, s, tid("user-1"), "user1@test.com", tid("project-1"), store.ProjectRoleMember)

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), ParentType: "project", ParentID: tid("project-1")}

	// Read should be allowed (project-member has agent.read)
	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed, "project member should be able to read agents")

	// Delete should be denied (project-member does not have agent.delete)
	decision = authz.CheckAccess(ctx, user, resource, ActionDelete)
	assert.False(t, decision.Allowed, "project member without agent.delete should be denied")
	assert.Contains(t, decision.Reason, "agent.delete")
}

// TestPolicyStableTiebreaker verifies that multiple role bindings granting the
// same permission produce a consistent allowed decision. CO1: The AK1 kernel
// has no tiebreaker concept — role bindings are additive and any matching
// binding grants access.
func TestPolicyStableTiebreaker(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// Create two role definitions both granting agent.read
	rd1 := createTestRoleDefinition(t, s, "tie-role-1", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd1.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-1"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	rd2 := createTestRoleDefinition(t, s, "tie-role-2", store.RoleScopeSystem, []string{"agent.read", "agent.update"})
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd2.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-1"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	// Both bindings grant agent.read — decision should be allowed
	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed, "multiple bindings granting agent.read should allow access")
	assert.Equal(t, "role binding grant", decision.Reason)

	// agent.update is also granted by the second binding
	decision = authz.CheckAccess(ctx, user, resource, ActionUpdate)
	assert.True(t, decision.Allowed, "second binding should also grant agent.update")
	assert.Equal(t, "role binding grant", decision.Reason)
}
