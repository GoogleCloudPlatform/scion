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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPolicyDeterministicOrdering verifies that policies inserted in different
// orders produce identical authorization decisions.
func TestPolicyDeterministicOrdering(t *testing.T) {
	// Test multiple insertion orders
	orders := [][]string{
		{"policy-1", "policy-2", "policy-3"},
		{"policy-3", "policy-1", "policy-2"},
		{"policy-2", "policy-3", "policy-1"},
	}

	var expectedDecision *Decision
	for i, order := range orders {
		authz, s := authzTestSetup(t)
		ctx := context.Background()

		// Create user and group
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
		}))

		require.NoError(t, s.CreateGroup(ctx, &store.Group{
			ID: tid("group-1"), Slug: "test-group", Name: "Test Group",
		}))

		// Create policies in this order
		policies := map[string]*store.Policy{
			"policy-1": {
				ID: tid("policy-1"), Name: "Hub Allow", ScopeType: "hub",
				ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
				Priority: 10, PolicyKind: store.PolicyKindExplicit,
			},
			"policy-2": {
				ID: tid("policy-2"), Name: "Hub Deny", ScopeType: "hub",
				ResourceType: "agent", Actions: []string{"read"}, Effect: "deny",
				Priority: 20, PolicyKind: store.PolicyKindExplicit,
			},
			"policy-3": {
				ID: tid("policy-3"), Name: "Hub Allow High", ScopeType: "hub",
				ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
				Priority: 30, PolicyKind: store.PolicyKindExplicit,
			},
		}

		// Insert in the specified order
		for _, policyID := range order {
			policy := policies[policyID]
			require.NoError(t, s.CreatePolicy(ctx, policy))
			require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
				PolicyID: policy.ID, PrincipalType: "group", PrincipalID: tid("group-1"),
			}))
		}

		// Add user to group
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    tid("group-1"),
			MemberID:   tid("user-1"),
			MemberType: store.GroupMemberTypeUser,
			Role:       store.GroupMemberRoleMember,
		}))

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
			assert.Equal(t, expectedDecision.PolicyID, decision.PolicyID,
				"Order %d: matched policy should be deterministic", i)
		}
	}

	// The highest priority (30) should win
	require.NotNil(t, expectedDecision)
	assert.True(t, expectedDecision.Allowed, "highest priority policy should win")
	assert.Equal(t, tid("policy-3"), expectedDecision.PolicyID)
}

// TestPolicyPriorityPrecedence verifies that higher priority wins at the same scope.
func TestPolicyPriorityPrecedence(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// Create two policies at same scope, different priorities
	lowPriorityPolicy := &store.Policy{
		ID: tid("policy-low"), Name: "Low Priority Allow", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
		Priority: 10, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, lowPriorityPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-low"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	highPriorityPolicy := &store.Policy{
		ID: tid("policy-high"), Name: "High Priority Deny", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "deny",
		Priority: 100, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, highPriorityPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-high"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)

	// Higher priority policy should win
	assert.False(t, decision.Allowed, "higher priority deny should win")
	assert.Equal(t, tid("policy-high"), decision.PolicyID)
}

// TestPolicyKindPrecedence verifies that explicit wins over default at same scope and priority.
func TestPolicyKindPrecedence(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// Create default policy (deny)
	defaultPolicy := &store.Policy{
		ID: tid("policy-default"), Name: "Default Deny", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "deny",
		Priority: 10, PolicyKind: store.PolicyKindDefault,
	}
	require.NoError(t, s.CreatePolicy(ctx, defaultPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-default"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	// Create explicit policy at same priority (allow)
	explicitPolicy := &store.Policy{
		ID: tid("policy-explicit"), Name: "Explicit Allow", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
		Priority: 10, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, explicitPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-explicit"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)

	// Explicit policy should win over default at same priority
	assert.True(t, decision.Allowed, "explicit policy should win over default")
	assert.Equal(t, tid("policy-explicit"), decision.PolicyID)
}

// TestPolicyLocalOverride verifies that project-scoped policies override hub-scoped policies.
func TestPolicyLocalOverride(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-1"), Slug: "test-project", Name: "Test Project",
	}))

	// Create hub-scoped deny policy
	hubPolicy := &store.Policy{
		ID: tid("policy-hub"), Name: "Hub Deny", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"delete"}, Effect: "deny",
		Priority: 100, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, hubPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-hub"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	// Create project-scoped allow policy (lower priority)
	projectPolicy := &store.Policy{
		ID: tid("policy-project"), Name: "Project Allow", ScopeType: "project",
		ScopeID: tid("project-1"), ResourceType: "agent", Actions: []string{"delete"}, Effect: "allow",
		Priority: 10, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, projectPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-project"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), ParentType: "project", ParentID: tid("project-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionDelete)

	// Project-scoped policy should override hub-scoped policy (local override)
	assert.True(t, decision.Allowed, "project policy should override hub policy")
	assert.Equal(t, tid("policy-project"), decision.PolicyID)
	assert.Equal(t, "project", decision.Scope)
}

// TestPolicyResourceOverride verifies that resource-scoped policies override project and hub policies.
func TestPolicyResourceOverride(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-1"), Slug: "test-project", Name: "Test Project",
	}))

	// Create hub-scoped allow policy
	hubPolicy := &store.Policy{
		ID: tid("policy-hub"), Name: "Hub Allow", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"delete"}, Effect: "allow",
		Priority: 100, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, hubPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-hub"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	// Create project-scoped allow policy
	projectPolicy := &store.Policy{
		ID: tid("policy-project"), Name: "Project Allow", ScopeType: "project",
		ScopeID: tid("project-1"), ResourceType: "agent", Actions: []string{"delete"}, Effect: "allow",
		Priority: 50, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, projectPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-project"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	// Create resource-scoped deny policy (lowest priority)
	resourcePolicy := &store.Policy{
		ID: tid("policy-resource"), Name: "Resource Deny", ScopeType: "resource",
		ResourceType: "agent", ResourceID: tid("agent-1"), Actions: []string{"delete"}, Effect: "deny",
		Priority: 1, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, resourcePolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-resource"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), ParentType: "project", ParentID: tid("project-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionDelete)

	// Resource-scoped policy should override both project and hub policies
	assert.False(t, decision.Allowed, "resource policy should override broader scopes")
	assert.Equal(t, tid("policy-resource"), decision.PolicyID)
	assert.Equal(t, "resource", decision.Scope)
}

// TestPolicyStableTiebreaker verifies that creation time and ID provide stable ordering.
func TestPolicyStableTiebreaker(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// Create first policy
	policy1 := &store.Policy{
		ID: tid("policy-1"), Name: "First Policy", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "deny",
		Priority: 10, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, policy1))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-1"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	// Wait to ensure different creation timestamp
	time.Sleep(10 * time.Millisecond)

	// Create second policy with same scope, priority, and kind
	policy2 := &store.Policy{
		ID: tid("policy-2"), Name: "Second Policy", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
		Priority: 10, PolicyKind: store.PolicyKindExplicit,
	}
	require.NoError(t, s.CreatePolicy(ctx, policy2))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-2"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)

	// Later-created policy should win (policy-2)
	assert.True(t, decision.Allowed, "later-created policy should win")
	assert.Equal(t, tid("policy-2"), decision.PolicyID)
}
