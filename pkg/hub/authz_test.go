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
	authz, _ := authzTestSetup(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "agent", ID: "some-agent"}

	decision := authz.CheckAccess(ctx, admin, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "admin bypass", decision.Reason)
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
	assert.Equal(t, "resource owner", decision.Reason)
}

func TestAuthz_DirectUserPolicy(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-1"), Email: "user1@test.com", DisplayName: "User 1", Role: "member", Status: "active",
	}))

	// Create policy allowing read
	policy := &store.Policy{
		ID: tid("policy-1"), Name: "Allow Read", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))

	// Bind to user
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-1"), PrincipalType: "user", PrincipalID: tid("user-1"),
	}))

	user := NewAuthenticatedUser(tid("user-1"), "user1@test.com", "User 1", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, tid("policy-1"), decision.PolicyID)
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
	assert.Equal(t, "default deny", decision.Reason)
}

func TestAuthz_DenyEffect(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-deny"), Email: "deny@test.com", DisplayName: "Deny", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: tid("policy-deny"), Name: "Deny Write", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"update"}, Effect: "deny",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-deny"), PrincipalType: "user", PrincipalID: tid("user-deny"),
	}))

	user := NewAuthenticatedUser(tid("user-deny"), "deny@test.com", "Deny", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionUpdate)
	assert.False(t, decision.Allowed)
	assert.Equal(t, tid("policy-deny"), decision.PolicyID)
}

func TestAuthz_WildcardAction(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-wc"), Email: "wc@test.com", DisplayName: "WC", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: tid("policy-wc"), Name: "Allow All", ScopeType: "hub",
		ResourceType: "*", Actions: []string{"*"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-wc"), PrincipalType: "user", PrincipalID: tid("user-wc"),
	}))

	user := NewAuthenticatedUser(tid("user-wc"), "wc@test.com", "WC", "member", "api")

	// Test with different actions and resource types
	for _, action := range []Action{ActionRead, ActionUpdate, ActionDelete, ActionManage} {
		decision := authz.CheckAccess(ctx, user, Resource{Type: "project", ID: "g1"}, action)
		assert.True(t, decision.Allowed, "expected allow for action %s", action)
	}
}

func TestAuthz_ScopeOverride(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-scope"), Email: "scope@test.com", DisplayName: "Scope", Role: "member", Status: "active",
	}))

	// Hub-level deny
	hubPolicy := &store.Policy{
		ID: tid("policy-hub-deny"), Name: "Hub Deny", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "deny", Priority: 0,
	}
	require.NoError(t, s.CreatePolicy(ctx, hubPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-hub-deny"), PrincipalType: "user", PrincipalID: tid("user-scope"),
	}))

	// Project-level allow (more specific scope overrides)
	projectPolicy := &store.Policy{
		ID: tid("policy-project-allow"), Name: "Project Allow", ScopeType: "project",
		ScopeID:      tid("project-1"),
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow", Priority: 0,
	}
	require.NoError(t, s.CreatePolicy(ctx, projectPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-project-allow"), PrincipalType: "user", PrincipalID: tid("user-scope"),
	}))

	user := NewAuthenticatedUser(tid("user-scope"), "scope@test.com", "Scope", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1"), ParentType: "project", ParentID: tid("project-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "project", decision.Scope)
	assert.Equal(t, tid("policy-project-allow"), decision.PolicyID)
}

func TestAuthz_PriorityWithinScope(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-prio"), Email: "prio@test.com", DisplayName: "Prio", Role: "member", Status: "active",
	}))

	// Low priority allow
	p1 := &store.Policy{
		ID: tid("policy-low"), Name: "Low Priority Allow", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow", Priority: 0,
	}
	// High priority deny (should override)
	p2 := &store.Policy{
		ID: tid("policy-high"), Name: "High Priority Deny", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "deny", Priority: 10,
	}
	require.NoError(t, s.CreatePolicy(ctx, p1))
	require.NoError(t, s.CreatePolicy(ctx, p2))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-low"), PrincipalType: "user", PrincipalID: tid("user-prio"),
	}))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-high"), PrincipalType: "user", PrincipalID: tid("user-prio"),
	}))

	user := NewAuthenticatedUser(tid("user-prio"), "prio@test.com", "Prio", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.False(t, decision.Allowed)
	assert.Equal(t, tid("policy-high"), decision.PolicyID)
}

func TestAuthz_ConditionLabels(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-labels"), Email: "labels@test.com", DisplayName: "Labels", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: tid("policy-labels"), Name: "Label Condition", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
		Conditions: &store.PolicyConditions{
			Labels: map[string]string{"env": "production", "team": "backend"},
		},
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-labels"), PrincipalType: "user", PrincipalID: tid("user-labels"),
	}))

	user := NewAuthenticatedUser(tid("user-labels"), "labels@test.com", "Labels", "member", "api")

	// Matching labels
	resourceMatch := Resource{
		Type:   "agent",
		ID:     tid("agent-1"),
		Labels: map[string]string{"env": "production", "team": "backend"},
	}
	decision := authz.CheckAccess(ctx, user, resourceMatch, ActionRead)
	assert.True(t, decision.Allowed)

	// Non-matching labels
	resourceNoMatch := Resource{
		Type:   "agent",
		ID:     tid("agent-2"),
		Labels: map[string]string{"env": "staging"},
	}
	decision = authz.CheckAccess(ctx, user, resourceNoMatch, ActionRead)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "default deny", decision.Reason)
}

func TestAuthz_TimeConditions(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-time"), Email: "time@test.com", DisplayName: "Time", Role: "member", Status: "active",
	}))

	past := time.Now().Add(-time.Hour)
	policy := &store.Policy{
		ID: tid("policy-expired"), Name: "Expired Policy", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
		Conditions: &store.PolicyConditions{
			ValidUntil: &past,
		},
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-expired"), PrincipalType: "user", PrincipalID: tid("user-time"),
	}))

	user := NewAuthenticatedUser(tid("user-time"), "time@test.com", "Time", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "default deny", decision.Reason)
}

func TestAuthz_AgentDirectPolicy(t *testing.T) {
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

	// Create and bind policy to agent
	policy := &store.Policy{
		ID: tid("policy-agent"), Name: "Agent Allow", ScopeType: "hub",
		ResourceType: "project", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-agent"), PrincipalType: "agent", PrincipalID: tid("agent-direct"),
	}))

	agent := &evaluateAgentIdentity{id: tid("agent-direct"), projectID: tid("project-agent-1")}
	resource := Resource{Type: "project", ID: tid("project-agent-1")}

	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, tid("policy-agent"), decision.PolicyID)
}

func TestAuthz_ActionMismatch(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-act"), Email: "act@test.com", DisplayName: "Act", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: tid("policy-read-only"), Name: "Read Only", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-read-only"), PrincipalType: "user", PrincipalID: tid("user-act"),
	}))

	user := NewAuthenticatedUser(tid("user-act"), "act@test.com", "Act", "member", "api")
	resource := Resource{Type: "agent", ID: tid("agent-1")}

	// Read should succeed
	decision := authz.CheckAccess(ctx, user, resource, ActionRead)
	assert.True(t, decision.Allowed)

	// Delete should fail
	decision = authz.CheckAccess(ctx, user, resource, ActionDelete)
	assert.False(t, decision.Allowed)
}

func TestAuthz_ResourceTypeMismatch(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("user-rt"), Email: "rt@test.com", DisplayName: "RT", Role: "member", Status: "active",
	}))

	policy := &store.Policy{
		ID: tid("policy-agent-only"), Name: "Agent Only", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: tid("policy-agent-only"), PrincipalType: "user", PrincipalID: tid("user-rt"),
	}))

	user := NewAuthenticatedUser(tid("user-rt"), "rt@test.com", "RT", "member", "api")

	// Agent resource should match
	decision := authz.CheckAccess(ctx, user, Resource{Type: "agent", ID: "a1"}, ActionRead)
	assert.True(t, decision.Allowed)

	// Project resource should not match
	decision = authz.CheckAccess(ctx, user, Resource{Type: "project", ID: "g1"}, ActionRead)
	assert.False(t, decision.Allowed)
}

func TestEvaluatePolicies_NoMatch(t *testing.T) {
	authz := NewAuthzService(nil, slog.Default())

	decision := authz.evaluatePolicies(nil, Resource{Type: "agent"}, ActionRead)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "default deny", decision.Reason)
}

func TestMatchesAction(t *testing.T) {
	tests := []struct {
		name     string
		actions  []string
		action   Action
		expected bool
	}{
		{"exact match", []string{"read"}, ActionRead, true},
		{"wildcard", []string{"*"}, ActionDelete, true},
		{"no match", []string{"read", "update"}, ActionDelete, false},
		{"one of many", []string{"read", "update", "delete"}, ActionDelete, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := store.Policy{Actions: tt.actions}
			assert.Equal(t, tt.expected, matchesAction(policy, tt.action))
		})
	}
}

func TestMatchesResource(t *testing.T) {
	tests := []struct {
		name     string
		policy   store.Policy
		resource Resource
		expected bool
	}{
		{
			"wildcard type",
			store.Policy{ResourceType: "*", ScopeType: "hub"},
			Resource{Type: "agent"},
			true,
		},
		{
			"matching type",
			store.Policy{ResourceType: "agent", ScopeType: "hub"},
			Resource{Type: "agent"},
			true,
		},
		{
			"mismatched type",
			store.Policy{ResourceType: "project", ScopeType: "hub"},
			Resource{Type: "agent"},
			false,
		},
		{
			"specific resource ID match",
			store.Policy{ResourceType: "agent", ResourceID: "a1", ScopeType: "hub"},
			Resource{Type: "agent", ID: "a1"},
			true,
		},
		{
			"specific resource ID mismatch",
			store.Policy{ResourceType: "agent", ResourceID: "a1", ScopeType: "hub"},
			Resource{Type: "agent", ID: "a2"},
			false,
		},
		{
			"project scope matching",
			store.Policy{ResourceType: "agent", ScopeType: "project", ScopeID: tid("project-1")},
			Resource{Type: "agent", ParentType: "project", ParentID: tid("project-1")},
			true,
		},
		{
			"project scope mismatch",
			store.Policy{ResourceType: "agent", ScopeType: "project", ScopeID: tid("project-1")},
			Resource{Type: "agent", ParentType: "project", ParentID: tid("project-2")},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, matchesResource(tt.policy, tt.resource))
		})
	}
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
	assert.Equal(t, "resource owner", decision.Reason)
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
	assert.Equal(t, "default deny", decision.Reason)
}

func TestAuthz_BrokerDispatch_AdminAllowed(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "api")
	resource := Resource{Type: "broker", ID: tid("broker-1"), OwnerID: "someone-else"}

	decision := authz.CheckAccess(ctx, admin, resource, ActionDispatch)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "admin bypass", decision.Reason)
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
	assert.Equal(t, "ancestor access", decision.Reason)
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

	agent := &evaluateAgentIdentity{id: tid("agent-parent"), projectID: tid("project-ancestry-1")}

	// Grandchild agent with parent in ancestry
	resource := Resource{
		Type:     "agent",
		ID:       tid("agent-grandchild"),
		Ancestry: []string{tid("user-root"), tid("agent-parent"), tid("agent-child")},
	}

	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "ancestor access", decision.Reason)
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
	assert.Equal(t, "default deny", decision.Reason)
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

	// Create a project-scoped role binding using the hub-admin role definition.
	// IsHubAdmin must reject non-system-scoped bindings.
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
	require.NoError(t, err)

	result := authz.IsHubAdmin(ctx, userID)
	assert.False(t, result, "should return false for project-scoped hub-admin binding (must be system-scoped)")
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
		MemberID:  tid("grp-perm-user"),
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
		MemberID:  tid("grp-proj-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Get the project-owner role definition
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	// Bind project-owner to the group, scoped to this project
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
	assert.True(t, result, "user should be project owner via group membership")
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
		MemberID:  tid("grp-sysadmin-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Bind super-admin role to the group
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      tid("grp-sysadmin-group"),
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	result := authz.IsSystemAdmin(ctx, tid("grp-sysadmin-user"))
	assert.True(t, result, "user should be system admin via group membership")
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
		MemberID:  tid("grp-hubadmin-user"),
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
		MemberID:  tid("child-group"),
		MemberType: store.GroupMemberTypeGroup,
		Role:       store.GroupMemberRoleMember,
	}))

	// Add user to child group
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("child-group"),
		MemberID:  tid("nested-grp-user"),
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
		MemberID:  tid("merge-user"),
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

	// Bind the project-owner role to the group BEFORE the user joins
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
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

	// Verify user is NOT project owner yet (no group membership)
	result := authz.isProjectOwnerOrAdmin(ctx, tid("realtime-user"), tid("realtime-proj"))
	assert.False(t, result, "user should NOT be project owner before joining the group")

	// NOW add the user to the group
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    tid("realtime-group"),
		MemberID:  tid("realtime-user"),
		MemberType: store.GroupMemberTypeUser,
		Role:       store.GroupMemberRoleMember,
	}))

	// Verify user IS project owner now (real-time — no restart needed)
	result = authz.isProjectOwnerOrAdmin(ctx, tid("realtime-user"), tid("realtime-proj"))
	assert.True(t, result, "user should IMMEDIATELY be project owner after joining group (real-time expansion)")
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
