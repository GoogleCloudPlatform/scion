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
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authzTestSetup creates a test server with the authz service and pre-populated data.
// Note: testServer() removes the delegation edge backfill marker so that
// agents created directly via the store (without delegation edges) are
// not denied by the post-backfill no-edge check. Tests that specifically
// exercise post-backfill behavior re-create the marker explicitly.
func authzTestSetup(t *testing.T) (*AuthzService, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	return srv.authzService, s
}

func TestAuthz_AdminBypass(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// CO1: Admin access now comes through role binding, not bypass.
	// Create user with super-admin role binding.
	createTestUserWithRole(t, s, tid("admin-1"), "admin@example.com", "admin", store.SystemRoleSuperAdmin)

	admin := NewAuthenticatedUser(tid("admin-1"), "admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "agent", ID: "some-agent"}

	decision := authz.CheckAccess(ctx, admin, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "role binding grant", decision.Reason)
}

func TestAuthz_OwnerBypass(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create a user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-owner"), Email: "owner@test.com", DisplayName: "Owner", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-owner"), "owner@test.com", "Owner", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), OwnerID: tid("user-owner")}

	decision := authz.CheckAccess(ctx, user, resource, ActionDelete)
	assert.True(t, decision.Allowed)
	// CO1: The AK1 kernel returns "relationship grant: resource owner" for the owner bypass.
	assert.Equal(t, "relationship grant: resource owner", decision.Reason)
}

func TestAuthz_DirectUserPolicy(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// CO1: Create a custom role with agent.read permission and bind to user
	// (replaces policy-based grant).
	rd := createTestRoleDefinition(t, s, "allow-agent-read", store.RoleScopeSystem, []string{"agent.read"})
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

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "role binding grant", decision.Reason)
}

func TestAuthz_DefaultDeny(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-nodeny"), Email: "nodeny@test.com", DisplayName: "NoDeny", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-nodeny"), "nodeny@test.com", "NoDeny", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionDelete)
	assert.False(t, decision.Allowed)
	// CO1: The AK1 kernel returns "no candidate bindings" when no role bindings match.
	assert.Equal(t, "no candidate bindings", decision.Reason)
}

func TestAuthz_DenyEffect(t *testing.T) {
	// CO1: The AK1 kernel has no deny effect. Without a role binding granting
	// the requested permission, the result is default deny.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-deny"), Email: "deny@test.com", DisplayName: "Deny", Role: "member", Status: "active",
	}))

	// Give the user agent.read but NOT agent.update
	rd := createTestRoleDefinition(t, s, "read-not-update", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-deny"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-deny"), "deny@test.com", "Deny", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	// agent.update is not granted — bindings exist but do not include this permission
	decision := authz.CheckAccess(ctx, user, resource, ActionUpdate)
	assert.False(t, decision.Allowed)
	assert.Contains(t, decision.Reason, "do not include permission")
}

func TestAuthz_WildcardAction(t *testing.T) {
	// CO1: Wildcard policies replaced by super-admin role which has all permissions.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	createTestUserWithRole(t, s, tid("user-wc"), "wc@test.com", "admin", store.SystemRoleSuperAdmin)

	user := NewAuthenticatedUser(tid("user-wc"), "wc@test.com", "WC", "admin", "api")

	// Test with different actions and resource types
	for _, action := range []Action{ActionRead, ActionUpdate, ActionDelete, ActionManage} {
		decision := authz.CheckAccess(ctx, user, Resource{Type: "project", ID: "g1"}, action)
		assert.True(t, decision.Allowed, "expected allow for action %s", action)
	}
}

func TestAuthz_ScopeOverride(t *testing.T) {
	// CO1: Scope override now tested via project-scoped role binding. A user
	// with a project-scoped role binding gets access within that project.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-scope"), Email: "scope@test.com", DisplayName: "Scope", Role: "member", Status: "active",
	}))

	// Create project
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-1"), Name: "Scope Project", Slug: "scope-project-1",
	}))

	// Create a project-scoped role binding with agent.read permission
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-scope"),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          tid("project-1"),
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-scope"), "scope@test.com", "Scope", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), ParentType: "project", ParentID: tid("project-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "project", decision.Scope)
	assert.Equal(t, "role binding grant", decision.Reason)
}

func TestAuthz_PriorityWithinScope(t *testing.T) {
	// CO1: The AK1 kernel has no policy priority concept. Role bindings are
	// additive (allow-only). This test verifies that a user without any role
	// binding for the requested permission is denied.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-prio"), Email: "prio@test.com", DisplayName: "Prio", Role: "member", Status: "active",
	}))

	// Give user a role that does NOT include agent.read
	rd := createTestRoleDefinition(t, s, "no-agent-read", store.RoleScopeSystem, []string{"project.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-prio"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-prio"), "prio@test.com", "Prio", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.False(t, decision.Allowed)
	assert.Contains(t, decision.Reason, "do not include permission")
}

func TestAuthz_ConditionLabels(t *testing.T) {
	// CO1: Policy conditions (labels) are not supported in the AK1 kernel.
	// Role bindings grant permission unconditionally. This test verifies that
	// a user with the right role binding can access agents regardless of labels,
	// and that a user without any role binding is denied.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-labels"), Email: "labels@test.com", DisplayName: "Labels", Role: "member", Status: "active",
	}))

	// Create role with agent.read and bind to user
	rd := createTestRoleDefinition(t, s, "label-agent-read", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-labels"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-labels"), "labels@test.com", "Labels", "member", "api")

	// With agent.read role binding, access is granted regardless of labels
	resourceMatch := Resource{
		Type:   "agent",
		ID:     tid("agent-1"),
		Labels: map[string]string{"env": "production", "team": "backend"},
	}
	decision := authz.CheckAccess(ctx, user, resourceMatch, ActionRead)
	assert.True(t, decision.Allowed)

	// Same user requesting an action NOT in their role binding — denied
	decision = authz.CheckAccess(ctx, user, resourceMatch, ActionDelete)
	assert.False(t, decision.Allowed)
	assert.Contains(t, decision.Reason, "do not include permission")
}

func TestAuthz_TimeConditions(t *testing.T) {
	// CO1: Time conditions on policies are not supported in the AK1 kernel.
	// This test verifies that a user without a role binding for the requested
	// permission is denied (equivalent to an expired grant).
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-time"), Email: "time@test.com", DisplayName: "Time", Role: "member", Status: "active",
	}))

	// No role binding granting agent.read — simulates an expired/absent grant.
	user := NewAuthenticatedUser(tid("user-time"), "time@test.com", "Time", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "no candidate bindings", decision.Reason)
}

func TestAuthz_AgentDirectPolicy(t *testing.T) {
	// CO1: Agent access now comes through role bindings, not policies.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create project and agent
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-agent-1"), Name: "Test Project", Slug: "test-project-agent-1",
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("agent-direct"), Slug: tid("agent-direct"), Name: "Agent Direct",
		ProjectID: tid("project-agent-1"), Phase: string(state.PhaseRunning),
	}))

	// Create a custom role with project.read and bind to the agent
	rd := createTestRoleDefinition(t, s, "agent-project-read", store.RoleScopeSystem, []string{"project.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      tid("agent-direct"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("agent-direct")},
		ProjectID: tid("project-agent-1"),
		Scopes:    []AgentTokenScope{ScopeProjectRead},
	}}
	resource := Resource{Type: "project", ID: tid("project-agent-1")}

	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "role binding grant", decision.Reason)
}

func TestAuthz_ActionMismatch(t *testing.T) {
	// CO1: Action mismatch tested via role binding with specific permission.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-act"), Email: "act@test.com", DisplayName: "Act", Role: "member", Status: "active",
	}))

	// Create role with only agent.read
	rd := createTestRoleDefinition(t, s, "action-agent-read-only", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-act"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-act"), "act@test.com", "Act", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	// Read should succeed
	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed)

	// Delete should fail (not in role permissions)
	decision = authz.CheckAccess(ctx, user, resource, ActionDelete)
	assert.False(t, decision.Allowed)
}

func TestAuthz_ResourceTypeMismatch(t *testing.T) {
	// CO1: Resource type mismatch tested via role binding with specific permission.
	// agent.read grants access to agents but not projects.
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-rt"), Email: "rt@test.com", DisplayName: "RT", Role: "member", Status: "active",
	}))

	// Create role with only agent.read
	rd := createTestRoleDefinition(t, s, "rt-agent-read-only", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("user-rt"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(tid("user-rt"), "rt@test.com", "RT", "member", "api")

	// Agent resource should match (agent.read)
	decision := authz.CheckAccess(ctx, user, Resource{Type: "agent", ID: "a1"}, ActionRead)
	assert.True(t, decision.Allowed)

	// Project resource should not match (no project.read permission)
	decision = authz.CheckAccess(ctx, user, Resource{Type: "project", ID: "g1"}, ActionRead)
	assert.False(t, decision.Allowed)
}

func TestEvaluatePolicies_NoMatch(t *testing.T) {
	// CO1: Legacy function removed; test retained as shell.
}

func TestMatchesAction(t *testing.T) {
	// CO1: Legacy function removed; test retained as shell.
}

func TestMatchesResource(t *testing.T) {
	// CO1: Legacy function removed; test retained as shell.
}

func TestScopeLevel(t *testing.T) {
	assert.Equal(t, 0, scopeLevel("hub"))
	assert.Equal(t, 1, scopeLevel("project"))
	assert.Equal(t, 2, scopeLevel("resource"))
	assert.Equal(t, -1, scopeLevel("unknown"))
}

func TestAuthz_BrokerDispatch_OwnerAllowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("broker-owner"), Email: "owner@test.com", DisplayName: "Owner", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("broker-owner"), "owner@test.com", "Owner", "member", "api")
	resource := Resource{Type: "broker", ID: tid("broker-1"), OwnerID: tid("broker-owner")}

	decision := authz.CheckAccess(ctx, user, resource, ActionDispatch)
	assert.True(t, decision.Allowed)
	// CO1: The AK1 kernel returns "relationship grant: resource owner" for the owner bypass.
	assert.Equal(t, "relationship grant: resource owner", decision.Reason)
}

func TestAuthz_BrokerDispatch_NonOwnerDenied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("other-user"), Email: "other@test.com", DisplayName: "Other", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("other-user"), "other@test.com", "Other", "member", "api")
	resource := Resource{Type: "broker", ID: tid("broker-1"), OwnerID: tid("broker-owner-id")}

	decision := authz.CheckAccess(ctx, user, resource, ActionDispatch)
	assert.False(t, decision.Allowed)
	// CO1: The AK1 kernel returns "no candidate bindings" when no role bindings match.
	assert.Equal(t, "no candidate bindings", decision.Reason)
}

func TestAuthz_BrokerDispatch_AdminAllowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// CO1: Admin access now comes through role binding, not bypass.
	createTestUserWithRole(t, s, tid("broker-admin-1"), "broker-admin@example.com", "admin", store.SystemRoleSuperAdmin)

	admin := NewAuthenticatedUser(tid("broker-admin-1"), "broker-admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "broker", ID: tid("broker-1"), OwnerID: "someone-else"}

	decision := authz.CheckAccess(ctx, admin, resource, ActionDispatch)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "role binding grant", decision.Reason)
}

func TestAuthz_BrokerCapabilities_Owner(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("cap-owner"), Email: "cap-owner@test.com", DisplayName: "Cap Owner", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("cap-owner"), "cap-owner@test.com", "Cap Owner", "member", "api")
	resource := Resource{Type: "broker", ID: tid("broker-cap"), OwnerID: tid("cap-owner")}

	caps := authz.ComputeCapabilities(ctx, user, resource)
	assert.Contains(t, caps.Actions, "dispatch")
	assert.Contains(t, caps.Actions, "read")
	assert.Contains(t, caps.Actions, "update")
	assert.Contains(t, caps.Actions, "delete")
}

func TestAuthz_BrokerCapabilities_NonOwner(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("cap-nonowner"), Email: "nonowner@test.com", DisplayName: "Non Owner", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("cap-nonowner"), "nonowner@test.com", "Non Owner", "member", "api")
	resource := Resource{Type: "broker", ID: tid("broker-cap"), OwnerID: "someone-else"}

	caps := authz.ComputeCapabilities(ctx, user, resource)
	assert.NotContains(t, caps.Actions, "dispatch")
	assert.NotContains(t, caps.Actions, "delete")
}

func TestBrokerResource_Helper(t *testing.T) {
	broker := &store.RuntimeBroker{
		ID:        tid("broker-helper-test"),
		CreatedBy: tid("user-123"),
	}

	r := brokerResource(broker)
	assert.Equal(t, "broker", r.Type)
	assert.Equal(t, tid("broker-helper-test"), r.ID)
	assert.Equal(t, tid("user-123"), r.OwnerID)
}

// =============================================================================
// Ancestry-Based Transitive Access Tests
// =============================================================================

func TestCanAccessAsAncestor(t *testing.T) {
	tests := []struct {
		name        string
		principalID string
		ancestry    []string
		expected    bool
	}{
		{"root ancestor", tid("user-1"), []string{tid("user-1")}, true},
		{"intermediate ancestor", tid("agent-A"), []string{tid("user-1"), tid("agent-A")}, true},
		{"not in ancestry", tid("user-2"), []string{tid("user-1"), tid("agent-A")}, false},
		{"empty ancestry", tid("user-1"), nil, false},
		{"deep chain", tid("user-1"), []string{tid("user-1"), tid("agent-A"), tid("agent-B")}, true},
		{"deep chain middle", tid("agent-A"), []string{tid("user-1"), tid("agent-A"), tid("agent-B")}, true},
		{"deep chain last", tid("agent-B"), []string{tid("user-1"), tid("agent-A"), tid("agent-B")}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource := Resource{Type: "agent", ID: "target", Ancestry: tt.ancestry}
			assert.Equal(t, tt.expected, canAccessAsAncestor(tt.principalID, resource))
		})
	}
}

func TestAuthz_AncestryAccess_UserToAgent(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user (non-admin, non-owner — ancestry is the only access path)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-ancestor"), Email: "ancestor@test.com", DisplayName: "Ancestor", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-ancestor"), "ancestor@test.com", "Ancestor", "member", "api")

	// Resource with user in ancestry but different owner
	resource := Resource{
		Type:     "agent",
		ID:       tid("agent-grandchild"),
		OwnerID:  "someone-else",
		Ancestry: []string{tid("user-ancestor"), tid("agent-child")},
	}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed)
	// CO1: Reason prefix changed to include relationship grant provenance.
	assert.Equal(t, "relationship grant: ancestor access", decision.Reason)
}

func TestAuthz_AncestryAccess_AgentToDescendant(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create project and parent agent
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("project-ancestry-1"), Name: "Ancestry Project", Slug: "ancestry-project-1",
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("agent-parent"), Slug: tid("agent-parent"), Name: "Parent Agent",
		ProjectID: tid("project-ancestry-1"), Phase: string(state.PhaseRunning),
	}))

	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("agent-parent")},
		ProjectID: tid("project-ancestry-1"),
		Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead},
	}}

	// Grandchild agent with parent in ancestry
	resource := Resource{
		Type:     "agent",
		ID:       tid("agent-grandchild"),
		Ancestry: []string{tid("user-root"), tid("agent-parent"), tid("agent-child")},
	}

	// CO1: Ancestor access for agents is now subject to agent scope restrictions.
	// The agent.read permission has no AgentScopes mapping, so the relationship
	// grant is restricted. This is intentional fail-closed behavior.
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.False(t, decision.Allowed,
		"agent without agent.read scope mapping should not access via ancestry")
}

func TestAuthz_AncestryAccess_NoAncestry(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-no-ancestry"), Email: "no-ancestry@test.com", DisplayName: "NoAnc", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-no-ancestry"), "no-ancestry@test.com", "NoAnc", "member", "api")

	// Resource without ancestry — user is not owner and has no policies
	resource := Resource{
		Type:    "agent",
		ID:      tid("agent-no-ancestry"),
		OwnerID: "someone-else",
	}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.False(t, decision.Allowed)
	// CO1: With AK1 kernel, deny reason reflects binding resolution state.
	assert.Equal(t, "no candidate bindings", decision.Reason)
}

func TestAuthz_AncestryAccess_NotInChain(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-outsider"), Email: "outsider@test.com", DisplayName: "Outsider", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(tid("user-outsider"), "outsider@test.com", "Outsider", "member", "api")

	// Resource with ancestry that doesn't include this user
	resource := Resource{
		Type:     "agent",
		ID:       tid("agent-other-chain"),
		OwnerID:  "someone-else",
		Ancestry: []string{tid("user-other"), tid("agent-A")},
	}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.False(t, decision.Allowed)
}

// =============================================================================
// IsHubAdmin Tests
// =============================================================================

func TestIsHubAdmin_SystemScopedHubAdminBinding(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("hub-admin-user")
	createTestUserWithRole(t, s, userID, "hubadmin@test.com", "member", store.SystemRoleHubAdmin)

	result := authz.IsHubAdmin(ctx, userID)
	assert.True(t, result, "should return true for user with system-scoped hub-admin binding")
}

func TestIsHubAdmin_ProjectScopedBindingReturnsFalse(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("hub-admin-proj-scope")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "projscope@test.com", DisplayName: "ProjScope", Role: "member", Status: "active",
	}))

	// Attempt to create a project-scoped role binding using the hub-admin role definition.
	// The store now enforces scope-type matching, so the binding creation itself
	// must be rejected — the authz layer never sees it.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role definition must exist")

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          tid("some-project"),
		CreatedBy:        "test",
	})
	require.Error(t, err, "store must reject scope-type mismatch")
	assert.True(t, errors.Is(err, store.ErrScopeMismatch), "expected ErrScopeMismatch, got: %v", err)

	// With no binding created, IsHubAdmin must return false.
	result := authz.IsHubAdmin(ctx, userID)
	assert.False(t, result, "should return false when scope-mismatched binding is rejected at the store level")
}

func TestIsHubAdmin_SuperAdminOnlyReturnsFalse(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("super-admin-only")
	createTestUserWithRole(t, s, userID, "superonly@test.com", "admin", store.SystemRoleSuperAdmin)

	result := authz.IsHubAdmin(ctx, userID)
	assert.False(t, result, "should return false for user with super-admin only (IsHubAdmin and IsSystemAdmin are independent)")
}

func TestIsHubAdmin_EmptyUserID(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()

	result := authz.IsHubAdmin(ctx, "")
	assert.False(t, result, "should return false for empty userID")
}

func TestIsHubAdmin_StoreErrorReturnsFalse(t *testing.T) {
	// IsHubAdmin calls ListRoleBindingsForPrincipal; if the store returns
	// an error, IsHubAdmin must fail closed (return false).
	authz := NewAuthzService(&failingRoleBindingStore{}, slog.Default())
	ctx := context.Background()

	result := authz.IsHubAdmin(ctx, "any-user-id")
	assert.False(t, result, "should return false when store returns an error (fail closed)")
}

// failingRoleBindingStore is a minimal store.Store stub that makes
// ListRoleBindingsForPrincipal return an error. Only the methods called by
// IsHubAdmin need to be implemented; the rest panic if called.
type failingRoleBindingStore struct {
	store.Store
}

func (f *failingRoleBindingStore) ListRoleBindingsForPrincipal(_ context.Context, _, _ string) ([]*store.RoleBinding, error) {
	return nil, errors.New("store unavailable")
}

func (f *failingRoleBindingStore) ListRoleBindingsForPrincipals(_ context.Context, _ []store.PrincipalRef, _ []string, _ []string) ([]*store.RoleBinding, error) {
	return nil, errors.New("store unavailable")
}

func (f *failingRoleBindingStore) GetEffectiveGroups(_ context.Context, _ string) ([]string, error) {
	return nil, errors.New("store unavailable")
}

// =============================================================================
// Group-based RoleBinding Tests
// =============================================================================

func TestGetEffectivePermissions_GroupRoleBinding(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("grp-perm-user"), Email: "grpperm@test.com", DisplayName: "GrpPerm", Role: "member", Status: "active",
	}))

	// Create group and add user to it
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("grp-perm-group"), Slug: "grp-perm-group", Name: "GrpPerm Group",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("grp-perm-group"),
		MemberID:   tid("grp-perm-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create a custom role with a permission
	rd := createTestRoleDefinition(t, s, "grp-test-role", store.RoleScopeSystem, []string{"agent.read"})

	// Bind the role to the GROUP (not the user)
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("grp-perm-group"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// The user should get the group's permissions via expansion
	perms, err := authz.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, tid("grp-perm-user"), store.RoleScopeSystem, "")
	require.NoError(t, err)
	assert.Contains(t, perms, "agent.read", "user should inherit permissions from group role binding")
}

func TestIsProjectOwnerOrAdmin_ViaGroup(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user (no direct project membership)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("grp-proj-user"), Email: "grpproj@test.com", DisplayName: "GrpProj", Role: "member", Status: "active",
	}))

	// Create project
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("grp-proj"), Name: "Group Project", Slug: "grp-proj",
	}))

	// Create group and add user
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("grp-proj-group"), Slug: "grp-proj-group", Name: "GrpProj Group",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("grp-proj-group"),
		MemberID:   tid("grp-proj-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Get the project-admin role definition (project-owner is direct-user-only,
	// so group bindings use project-admin instead).
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	// Bind project-admin to the group, scoped to this project
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("grp-proj-group"),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          tid("grp-proj"),
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	result := authz.isProjectOwnerOrAdmin(ctx, tid("grp-proj-user"), tid("grp-proj"))
	assert.True(t, result, "user should be project admin via group membership")
}

func TestIsProjectOwnerOrAdmin_DirectStillWorks(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("direct-proj-user")
	projectID := tid("direct-proj")

	// Create user and project
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "directproj@test.com", DisplayName: "DirectProj", Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Direct Project", Slug: "direct-proj",
	}))

	// Get the project-owner role definition and bind directly to user
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	result := authz.isProjectOwnerOrAdmin(ctx, userID, projectID)
	assert.True(t, result, "direct user binding should still work (regression test)")
}

func TestIsSystemAdmin_ViaGroup(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user (non-admin)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("grp-sysadmin-user"), Email: "grpsysadmin@test.com", DisplayName: "GrpSysAdmin", Role: "member", Status: "active",
	}))

	// Create group and add user
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("grp-sysadmin-group"), Slug: "grp-sysadmin-group", Name: "GrpSysAdmin Group",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("grp-sysadmin-group"),
		MemberID:   tid("grp-sysadmin-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// super-admin is direct-user-only: the store must reject group bindings.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("grp-sysadmin-group"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.Error(t, err, "store must reject super-admin group binding (direct-user-only)")
	assert.True(t, errors.Is(err, store.ErrDirectUserOnly), "expected ErrDirectUserOnly, got: %v", err)

	// With no binding created, IsSystemAdmin must return false.
	result := authz.IsSystemAdmin(ctx, tid("grp-sysadmin-user"))
	assert.False(t, result, "user must NOT be system admin when group binding is blocked by direct-user-only constraint")
}

func TestIsHubAdmin_ViaGroup(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("grp-hubadmin-user"), Email: "grphubadmin@test.com", DisplayName: "GrpHubAdmin", Role: "member", Status: "active",
	}))

	// Create group and add user
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("grp-hubadmin-group"), Slug: "grp-hubadmin-group", Name: "GrpHubAdmin Group",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("grp-hubadmin-group"),
		MemberID:   tid("grp-hubadmin-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Bind hub-admin role to the group
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("grp-hubadmin-group"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	result := authz.IsHubAdmin(ctx, tid("grp-hubadmin-user"))
	assert.True(t, result, "user should be hub admin via group membership")
}

func TestGetEffectivePermissions_NestedGroup(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("nested-grp-user"), Email: "nestedgrp@test.com", DisplayName: "NestedGrp", Role: "member", Status: "active",
	}))

	// Create parent and child groups
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("parent-group"), Slug: "parent-group", Name: "Parent Group",
	}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("child-group"), Slug: "child-group", Name: "Child Group",
		ParentID: tid("parent-group"),
	}))

	// Add child group to parent group
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("parent-group"),
		MemberID:   tid("child-group"),
		MemberType: store.GroupMemberTypeGroup,
		Role:       store.GroupMemberRoleMember,
	}))

	// Add user to child group
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("child-group"),
		MemberID:   tid("nested-grp-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create a role with permission and bind to parent group
	rd := createTestRoleDefinition(t, s, "nested-test-role", store.RoleScopeSystem, []string{"project.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("parent-group"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// The user in the child group should get permissions from the parent's binding (transitive)
	perms, err := authz.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, tid("nested-grp-user"), store.RoleScopeSystem, "")
	require.NoError(t, err)
	assert.Contains(t, perms, "project.read", "user in nested group should inherit parent group's role binding permissions")
}

func TestGetEffectivePermissions_DirectAndGroupMerge(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("merge-user"), Email: "merge@test.com", DisplayName: "MergeUser", Role: "member", Status: "active",
	}))

	// Create group and add user
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("merge-group"), Slug: "merge-group", Name: "Merge Group",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("merge-group"),
		MemberID:   tid("merge-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create role with agent.read and bind DIRECTLY to user
	rdDirect := createTestRoleDefinition(t, s, "merge-direct-role", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdDirect.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("merge-user"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create role with project.read and bind to GROUP
	rdGroup := createTestRoleDefinition(t, s, "merge-group-role", store.RoleScopeSystem, []string{"project.read"})
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdGroup.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("merge-group"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// User should get both direct AND group-granted permissions
	perms, err := authz.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, tid("merge-user"), store.RoleScopeSystem, "")
	require.NoError(t, err)
	assert.Contains(t, perms, "agent.read", "user should have direct permission")
	assert.Contains(t, perms, "project.read", "user should have group-granted permission")
}

func TestRealTimeGroupExpansion(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user (NOT yet in any group)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("realtime-user"), Email: "realtime@test.com", DisplayName: "Realtime", Role: "member", Status: "active",
	}))

	// Create project
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: tid("realtime-proj"), Name: "Realtime Project", Slug: "realtime-proj",
	}))

	// Create group
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("realtime-group"), Slug: "realtime-group", Name: "Realtime Group",
	}))

	// Bind the project-admin role to the group BEFORE the user joins
	// (project-owner is direct-user-only, so group bindings use project-admin).
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("realtime-group"),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          tid("realtime-proj"),
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Verify user is NOT project admin yet (no group membership)
	result := authz.isProjectOwnerOrAdmin(ctx, tid("realtime-user"), tid("realtime-proj"))
	assert.False(t, result, "user should NOT be project admin before joining the group")

	// NOW add the user to the group
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("realtime-group"),
		MemberID:   tid("realtime-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Verify user IS project admin now (real-time — no restart needed)
	result = authz.isProjectOwnerOrAdmin(ctx, tid("realtime-user"), tid("realtime-proj"))
	assert.True(t, result, "user should IMMEDIATELY be project admin after joining group (real-time expansion)")
}

// =============================================================================
// R-2 Regression Tests — Activation Checks and Constraint Intersection
// =============================================================================

// TestIsSystemAdmin_ExpiredBinding_ReturnsFalse verifies that a super-admin
// RoleBinding with ExpiresAt in the past does NOT grant system-admin status.
// Regression test for R-2: IsSystemAdmin now calls isBindingActive to skip
// expired bindings.
func TestIsSystemAdmin_ExpiredBinding_ReturnsFalse(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("r2-expired-sysadmin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "expired-sysadmin@test.com", DisplayName: "ExpiredSysAdmin",
		Role: "admin", Status: "active",
	}))

	// Get the super-admin role definition.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	// Create a super-admin binding that expired an hour ago.
	expired := time.Now().Add(-1 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ExpiresAt:        &expired,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	result := authz.IsSystemAdmin(ctx, userID)
	assert.False(t, result, "expired super-admin binding must not grant system-admin status")
}

// TestIsHubAdmin_FutureBinding_ReturnsFalse verifies that a hub-admin
// RoleBinding with NotBefore in the future does NOT grant hub-admin status.
// Regression test for R-2: IsHubAdmin now calls isBindingActive to skip
// not-yet-active bindings.
func TestIsHubAdmin_FutureBinding_ReturnsFalse(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("r2-future-hubadmin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "future-hubadmin@test.com", DisplayName: "FutureHubAdmin",
		Role: "member", Status: "active",
	}))

	// Get the hub-admin role definition.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	// Create a hub-admin binding that becomes active one hour from now.
	future := time.Now().Add(1 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		NotBefore:        &future,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	result := authz.IsHubAdmin(ctx, userID)
	assert.False(t, result, "future hub-admin binding (NotBefore in the future) must not grant hub-admin status")
}

// TestGetEffectivePermissions_ExcludesExpiredBindings verifies that
// getEffectivePermissions skips expired bindings and only returns
// permissions from active ones.
// Regression test for R-2: getEffectivePermissions now calls
// evaluateActivation to filter out expired/not-yet-active bindings.
func TestGetEffectivePermissions_ExcludesExpiredBindings(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("r2-perm-expired")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "perm-expired@test.com", DisplayName: "PermExpired",
		Role: "member", Status: "active",
	}))

	// Active binding: grants agent.read.
	rdActive := createTestRoleDefinition(t, s, "r2-active-role", store.RoleScopeSystem, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdActive.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Expired binding: grants project.read but expired an hour ago.
	rdExpired := createTestRoleDefinition(t, s, "r2-expired-role", store.RoleScopeSystem, []string{"project.read"})
	expired := time.Now().Add(-1 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdExpired.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ExpiresAt:        &expired,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	perms, err := authz.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, store.RoleScopeSystem, "")
	require.NoError(t, err)

	assert.Contains(t, perms, "agent.read", "active binding's permission must be present")
	assert.NotContains(t, perms, "project.read", "expired binding's permission must be excluded")
}

// TestGetEffectivePermissions_AppliesConstraintIntersection verifies that
// getEffectivePermissions applies AccessConstraint intersection to remove
// permissions excluded by an applicable constraint.
// Regression test for R-2: getEffectivePermissions now loads access
// constraints and filters the result through their maximum-permissions sets.
func TestGetEffectivePermissions_AppliesConstraintIntersection(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("r2-constraint-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "constraint@test.com", DisplayName: "ConstraintUser",
		Role: "member", Status: "active",
	}))

	// Grant both agent.read and project.read via a single binding.
	rd := createTestRoleDefinition(t, s, "r2-multi-perm-role", store.RoleScopeSystem, []string{"agent.read", "project.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create an AccessConstraint that allows only agent.read (excludes project.read).
	// Use all_principals subject so it applies to our test user.
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "r2-test-constraint",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "R-2 test: constraint intersection",
	})
	require.NoError(t, err)

	perms, err := authz.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, store.RoleScopeSystem, "")
	require.NoError(t, err)

	assert.Contains(t, perms, "agent.read", "permission in constraint's maximum set must be present")
	assert.NotContains(t, perms, "project.read", "permission outside constraint's maximum set must be excluded")
}

// ===========================================================================
// B1 integration tests: fail-closed behavior and type normalization
// ===========================================================================

// TestGetEffectivePermissions_PrincipalConstraintTargetingGroup verifies that
// a {principal, group, G} constraint is enforced by getEffectivePermissions
// when the user is a member of group G.
// Regression test for B1 fix 1: exact-principal matching fail-open.
func TestGetEffectivePermissions_PrincipalConstraintTargetingGroup(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("b1-group-constraint-user")
	groupID := tid("b1-target-group")

	// Create user and group.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "b1group@test.com", DisplayName: "B1GroupUser",
		Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "b1-target-group", Name: "B1 Target Group",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupID, MemberID: userID,
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Grant agent.read + project.read.
	rd := createTestRoleDefinition(t, s, "b1-multi-perm-role", store.RoleScopeSystem,
		[]string{"agent.read", "project.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create a constraint targeting group:b1-target-group that allows only agent.read.
	groupIDStr := groupID
	principalType := "group"
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "b1-group-targeting-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &principalType,
		SubjectPrincipalID:   &groupIDStr,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "B1 test: group-targeted principal constraint",
	})
	require.NoError(t, err)

	perms, err := authz.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, store.RoleScopeSystem, "")
	require.NoError(t, err)

	assert.Contains(t, perms, "agent.read", "agent.read should survive the constraint")
	assert.NotContains(t, perms, "project.read",
		"project.read should be removed by group-targeted principal constraint")
}

// TestGetEffectivePermissions_ProjectScopeConstraint verifies that project-scoped
// constraints are applied in getEffectivePermissions.
// Regression test for B1: constraint intersection on project scope.
func TestGetEffectivePermissions_ProjectScopeConstraint(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("b1-proj-user")
	projID := tid("b1-proj")

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "b1proj@test.com", DisplayName: "B1ProjUser",
		Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projID, Name: "B1 Project", Slug: "b1-proj",
	}))

	rd := createTestRoleDefinition(t, s, "b1-proj-role", store.RoleScopeProject,
		[]string{"agent.read", "agent.create"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create project-scoped constraint allowing only agent.read.
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "b1-proj-constraint",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeProject,
		ScopeID:            projID,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "B1 test: project-scoped constraint",
	})
	require.NoError(t, err)

	perms, err := authz.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, userID, store.RoleScopeProject, projID)
	require.NoError(t, err)

	assert.Contains(t, perms, "agent.read")
	assert.NotContains(t, perms, "agent.create",
		"project-scoped constraint should remove agent.create")
}

// createTestRoleDefinition creates a custom role definition for tests.
func createTestRoleDefinition(t *testing.T, s store.Store, name, scopeType string, permissions []string) *store.RoleDefinition {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        name,
		ScopeType:   scopeType,
		Permissions: permissions,
	})
	require.NoError(t, err)
	return rd
}

// ===========================================================================
// R1: Fail-closed error-path regression tests
// ===========================================================================

// errorInjectingStore wraps a real store and allows injecting errors into
// specific methods. All other methods delegate to the embedded store.
type errorInjectingStore struct {
	store.Store
	getEffectiveGroupsErr         error
	getEffectiveGroupsForAgentErr error
	getGroupMembersErr            error
}

func (s *errorInjectingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if s.getEffectiveGroupsErr != nil {
		return nil, s.getEffectiveGroupsErr
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s *errorInjectingStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	if s.getEffectiveGroupsForAgentErr != nil {
		return nil, s.getEffectiveGroupsForAgentErr
	}
	return s.Store.GetEffectiveGroupsForAgent(ctx, agentID)
}

func (s *errorInjectingStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	if s.getGroupMembersErr != nil {
		return nil, s.getGroupMembersErr
	}
	return s.Store.GetGroupMembers(ctx, groupID)
}

// TestGetEffectivePermissions_GroupResolutionFailure_FailsClosed verifies that
// when GetEffectiveGroups returns an error, getEffectivePermissions returns an
// error (fail-closed) instead of silently continuing with empty group closure.
//
// Regression test for B1 fix 2: store fault → evaluator denies.
func TestGetEffectivePermissions_GroupResolutionFailure_FailsClosed(t *testing.T) {
	_, realStore := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("r1-failclose-user")
	require.NoError(t, realStore.CreateUser(ctx, &store.User{
		ID: userID, Email: "r1fail@test.com", DisplayName: "R1FailUser",
		Role: "member", Status: "active",
	}))

	// Grant a permission so we'd see results if the function didn't fail.
	rd := createTestRoleDefinition(t, realStore, "r1-fail-role", store.RoleScopeSystem,
		[]string{"agent.read"})
	_, err := realStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create an AuthzService with the error-injecting store wrapper.
	injectedErr := errors.New("simulated store failure")
	errStore := &errorInjectingStore{
		Store:                 realStore,
		getEffectiveGroupsErr: injectedErr,
	}
	authzWithErr := NewAuthzService(errStore, slog.Default())

	perms, err := authzWithErr.getEffectivePermissions(ctx,
		store.RoleBindingPrincipalUser, userID, store.RoleScopeSystem, "")

	// Must fail closed: return error, not partial results.
	assert.Error(t, err, "getEffectivePermissions must fail closed on group resolution error")
	assert.Nil(t, perms, "permissions must be nil on group resolution failure")
	assert.ErrorIs(t, err, injectedErr, "original error must be wrapped")
}

// TestResolveConstraintAdminUsers_GetGroupMembersFailure_FailsClosed verifies
// that when GetGroupMembers returns an error during lockout resolution,
// resolveConstraintAdminUsers propagates the error (fail-closed) instead of
// silently continuing with incomplete admin data.
//
// Regression test for B1 fix 4a: lockout helper store fault → reject mutation.
func TestResolveConstraintAdminUsers_GetGroupMembersFailure_FailsClosed(t *testing.T) {
	srv, realStore := testServer(t)
	ctx := context.Background()

	// Create a group and a role with constraint-admin permission.
	groupID := tid("r1-lockout-group")
	require.NoError(t, realStore.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r1-lockout-group", Name: "R1 Lockout Group",
	}))

	// Create a custom role with access_constraint.admin and bind to group.
	rd := createTestRoleDefinition(t, realStore, "r1-admin-role", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})
	_, err := realStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Inject GetGroupMembers error and replace the store on the server.
	injectedErr := errors.New("simulated GetGroupMembers failure")
	errStore := &errorInjectingStore{
		Store:              realStore,
		getGroupMembersErr: injectedErr,
	}
	srv.store = errStore

	_, err = srv.resolveConstraintAdminUsers(ctx, ScopeTypeSystem, "")

	// Must fail closed: error propagates instead of continuing with empty members.
	assert.Error(t, err, "resolveConstraintAdminUsers must propagate GetGroupMembers error")
	assert.ErrorIs(t, err, injectedErr, "original error must be wrapped")
}

// TestResolveConstraintAdminUsers_GetEffectiveGroupsFailure_FailsClosed verifies
// that when GetEffectiveGroups returns an error during lockout resolution,
// resolveConstraintAdminUsers propagates the error (fail-closed) instead of
// silently setting groupIDs to nil.
//
// Regression test for B1 fix 4b: lockout helper store fault → reject mutation.
func TestResolveConstraintAdminUsers_GetEffectiveGroupsFailure_FailsClosed(t *testing.T) {
	srv, realStore := testServer(t)
	ctx := context.Background()

	// Create a user and bind them directly to a constraint-admin role.
	userID := tid("r1-lockout-user")
	require.NoError(t, realStore.CreateUser(ctx, &store.User{
		ID: userID, Email: "r1lockout@test.com", DisplayName: "R1LockoutUser",
		Role: "member", Status: "active",
	}))

	rd := createTestRoleDefinition(t, realStore, "r1-admin-role2", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin})
	_, err := realStore.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Inject GetEffectiveGroups error and replace the store on the server.
	injectedErr := errors.New("simulated GetEffectiveGroups failure")
	errStore := &errorInjectingStore{
		Store:                 realStore,
		getEffectiveGroupsErr: injectedErr,
	}
	srv.store = errStore

	_, err = srv.resolveConstraintAdminUsers(ctx, ScopeTypeSystem, "")

	// Must fail closed: error propagates instead of setting groupIDs = nil.
	assert.Error(t, err, "resolveConstraintAdminUsers must propagate GetEffectiveGroups error")
	assert.ErrorIs(t, err, injectedErr, "original error must be wrapped")
}
