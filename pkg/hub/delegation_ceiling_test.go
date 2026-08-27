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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Delegation ceiling test helpers ---

func setupDelegationCeilingTest(t *testing.T) (*AuthzService, store.Store) {
	t.Helper()
	return authzTestSetup(t)
}

// createDCUser creates a user and gives them a project-scoped role binding.
// The user is created with role="member" (NOT "admin") so that delegation
// ceiling tests exercise the actual ceiling logic rather than being
// short-circuited by the super-admin bypass in checkUserHoldsPermission.
func createDCUser(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()

	// Create user if not exists
	if _, err := s.GetUser(ctx, userID); err != nil {
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: email, DisplayName: email, Role: "member", Status: "active",
		}))
	}

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err, "role definition %q not found", roleName)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}

// assertNotSystemAdmin verifies that the test user is NOT a system admin,
// ensuring delegation ceiling tests exercise the actual ceiling logic.
func assertNotSystemAdmin(t *testing.T, authz *AuthzService, ctx context.Context, userID string) {
	t.Helper()
	require.False(t, authz.IsSystemAdmin(ctx, userID),
		"test user %s must NOT be a system admin — ceiling tests would be vacuous", userID)
}

func createDCProject(t *testing.T, s store.Store, projectID, slug string) {
	t.Helper()
	ctx := context.Background()
	_ = s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Slug: slug,
		Name: slug,
	})
}

func createDCAgent(t *testing.T, s store.Store, agentID, projectID, ownerID string, role AgentRole) {
	t.Helper()
	ctx := context.Background()

	appliedConfig := &store.AgentAppliedConfig{
		AgentRole: string(role),
	}

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:            agentID,
		Slug:          "slug-" + agentID[:8],
		Name:          "name-" + agentID[:8],
		ProjectID:     projectID,
		Phase:         "running",
		CreatedBy:     ownerID,
		OwnerID:       ownerID,
		AppliedConfig: appliedConfig,
		Ancestry:      []string{ownerID},
	}))
}

func createDCEdge(t *testing.T, s store.Store, delegatorType, delegatorID, delegateType, delegateID, scopeType, scopeID, role string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  delegateType,
		DelegateID:    delegateID,
		ScopeType:     scopeType,
		ScopeID:       scopeID,
		Role:          role,
		Active:        true,
	}))
}

func dcAgentIdentity(agentID, projectID string, role AgentRole) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    ScopesForRole(role),
	}}
}

// --- Test: User->Agent chain (depth 1) ---

func TestDelegationCeiling_UserAgentChain(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-1")
	userID := tid("dc-user-1")
	agentID := tid("dc-agent-1")

	createDCProject(t, s, projectID, "dc-test-project-1")
	createDCUser(t, s, userID, "user1@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Verify user is NOT a system admin (acceptance criterion)
	assertNotSystemAdmin(t, authz, ctx, userID)

	// Create delegation edge: user -> agent
	createDCEdge(t, s, store.DelegationPrincipalUser, userID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	// Agent should be able to exercise permissions
	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed, "agent should be allowed when user holds permission")

	// Now remove the user's role binding (user loses permission)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	// Agent should now be denied — fresh context for no caching
	decision = authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.False(t, decision.Allowed, "agent should be denied when user loses permission")
	assert.Contains(t, decision.Reason, "delegator", "denial reason should mention delegator")
}

// --- Test: Agent->Agent chain (depth > 1) ---

func TestDelegationCeiling_AgentAgentChain(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-2")
	userID := tid("dc-user-2")
	agentAID := tid("dc-agent-2a")
	agentBID := tid("dc-agent-2b")

	createDCProject(t, s, projectID, "dc-test-project-2")
	createDCUser(t, s, userID, "user2@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentAID, projectID, userID, AgentRoleFull)
	createDCAgent(t, s, agentBID, projectID, agentAID, AgentRoleBaseline)

	// Verify user is NOT a system admin (acceptance criterion)
	assertNotSystemAdmin(t, authz, ctx, userID)

	// User -> Agent A -> Agent B
	createDCEdge(t, s, store.DelegationPrincipalUser, userID, store.DelegationPrincipalAgent, agentAID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
	createDCEdge(t, s, store.DelegationPrincipalAgent, agentAID, store.DelegationPrincipalAgent, agentBID,
		store.RoleScopeProject, projectID, string(AgentRoleBaseline))

	// Both agents should be allowed
	agentA := dcAgentIdentity(agentAID, projectID, AgentRoleFull)
	agentB := dcAgentIdentity(agentBID, projectID, AgentRoleBaseline)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	decisionA := authz.CheckAccess(ctx, agentA, resource, ActionRead)
	assert.True(t, decisionA.Allowed, "agent A should be allowed")

	decisionB := authz.CheckAccess(ctx, agentB, resource, ActionRead)
	assert.True(t, decisionB.Allowed, "agent B should be allowed")

	// Remove user's permission
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	// Both agents should now be denied
	decisionA = authz.CheckAccess(ctx, agentA, resource, ActionRead)
	assert.False(t, decisionA.Allowed, "agent A should be denied when user loses permission")

	decisionB = authz.CheckAccess(ctx, agentB, resource, ActionRead)
	assert.False(t, decisionB.Allowed, "agent B should be denied when user loses permission (transitive)")
}

// --- Test: Pre-migration agents without delegation edges (pre-backfill) ---

func TestDelegationCeiling_NoEdgePreBackfill(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	// authzTestSetup already removes the backfill marker.
	// Verify it's absent.
	_, err := s.GetHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	require.Error(t, err, "backfill marker should be absent for pre-backfill test")

	projectID := tid("dc-proj-3")
	agentID := tid("dc-agent-3")

	createDCProject(t, s, projectID, "dc-test-project-3")
	createDCAgent(t, s, agentID, projectID, tid("dc-owner-3"), AgentRoleFull)

	// No delegation edge created — this is a pre-migration agent
	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Pre-backfill: allow temporarily until backfill completes
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed, "pre-backfill agent without delegation edge should be allowed temporarily")
}

// --- Test: New agents with edges have ceiling enforced ---

func TestDelegationCeiling_NewAgentWithEdge(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-4")
	userID := tid("dc-user-4")
	agentID := tid("dc-agent-4")

	createDCProject(t, s, projectID, "dc-test-project-4")
	createDCUser(t, s, userID, "user4@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Create delegation edge
	createDCEdge(t, s, store.DelegationPrincipalUser, userID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Should be allowed when user has permission
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed, "new agent with delegation edge should be allowed")
}

// --- Test: Fail-closed on store errors for minting operations ---

func TestDelegationCeiling_FailClosedMinting(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-5")
	userID := tid("dc-user-5")
	agentID := tid("dc-agent-5")

	createDCProject(t, s, projectID, "dc-test-project-5")
	createDCUser(t, s, userID, "user5@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Create delegation edge to a non-existent user (simulating lookup failure when
	// checking user's permissions)
	createDCEdge(t, s, store.DelegationPrincipalUser, tid("dc-nonexistent-user"), store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Minting operation should fail closed when delegator can't be resolved
	decision := authz.CheckAccess(ctx, agent, resource, ActionCreate)
	assert.False(t, decision.Allowed, "minting operation should fail closed on delegation ceiling error")
}

// --- Test: isMintingOperation ---

func TestIsMintingOperation(t *testing.T) {
	assert.True(t, isMintingOperation(ActionCreate), "ActionCreate is minting")
	assert.True(t, isMintingOperation(ActionManage), "ActionManage is minting")
	assert.True(t, isMintingOperation(ActionAddMember), "ActionAddMember is minting")
	assert.True(t, isMintingOperation(ActionMint), "ActionMint is minting")
	assert.True(t, isMintingOperation(ActionAssign), "ActionAssign is minting")
	assert.True(t, isMintingOperation(ActionRegister), "ActionRegister is minting")
	assert.False(t, isMintingOperation(ActionRead), "ActionRead is not minting")
	assert.False(t, isMintingOperation(ActionList), "ActionList is not minting")
}

// --- Test: Request-scoped caching ---

func TestDelegationCeiling_RequestScopedCache(t *testing.T) {
	ctx := context.Background()

	// Without cache
	cache := getDelegationCeilingCache(ctx)
	assert.Nil(t, cache, "no cache in plain context")

	// With cache
	ctx = contextWithDelegationCeilingCache(ctx)
	cache = getDelegationCeilingCache(ctx)
	require.NotNil(t, cache, "cache should be attached to context")

	// Cache stores and retrieves edges
	edges := []*store.DelegationEdge{{ID: "e1", DelegatorID: "d1"}}
	cache.edges["agent:a1"] = edges
	retrieved := cache.edges["agent:a1"]
	assert.Len(t, retrieved, 1, "cached edges should be retrievable")
	assert.Equal(t, "e1", retrieved[0].ID)
}

// --- Test: AncestryIsHubAttested ---

func TestAncestryIsHubAttested(t *testing.T) {
	// Local agent: hub attested
	localAgent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: "local-agent"},
	}}
	assert.True(t, AncestryIsHubAttested(localAgent), "local agent ancestry is hub-attested")

	// Federated agent: NOT hub attested
	federatedAgent := NewFederatedAgentIdentity(
		"https://remote.example.com", "remote-agent-1", "proj-1",
		"test-agent", "root-user-1", []string{"root-user-1"}, nil,
	)
	assert.False(t, AncestryIsHubAttested(federatedAgent), "federated agent ancestry is NOT hub-attested")

	// Federated user: NOT hub attested (1G-5 fix — tests FederatedIdentity interface)
	federatedUser := NewFederatedUserIdentity(
		"https://idp.example.com", "sub-1", "user@ext.com", "Ext User", "member", nil,
	)
	assert.False(t, AncestryIsHubAttested(federatedUser), "federated user ancestry is NOT hub-attested")

	// Federated service: NOT hub attested (1G-5 fix — tests FederatedIdentity interface)
	federatedService := NewFederatedServiceIdentity(
		"https://accounts.google.com", "sa-sub-1", "sa@project.iam.gserviceaccount.com", nil,
	)
	assert.False(t, AncestryIsHubAttested(federatedService), "federated service ancestry is NOT hub-attested")

	// Local user: hub attested (ancestry not relevant but should not panic)
	user := NewAuthenticatedUser("user-1", "user@test.com", "User", "member", "api")
	assert.True(t, AncestryIsHubAttested(user), "user ancestry is hub-attested")

	// Nil identity: NOT hub attested (fail closed)
	assert.False(t, AncestryIsHubAttested(nil), "nil identity is NOT hub-attested (fail closed)")
}

// --- Test: Federated agent with no edge denied (absent edge = no authority) ---

func TestDelegationCeiling_FederatedAgentNoEdgeDenied(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-fed-1")
	createDCProject(t, s, projectID, "dc-test-fed-project-1")

	// Create a federated agent identity — no store-recorded delegation edge
	federatedAgent := NewFederatedAgentIdentity(
		"https://remote.example.com", "federated-agent-1", projectID,
		"test-agent", "remote-user-1", []string{"remote-user-1"}, ScopesForRole(AgentRoleFull),
	)

	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Federated agent with no delegation edge should be DENIED.
	// Absent edge = no authority (floor), NOT unlimited authority.
	decision := authz.CheckAccess(ctx, federatedAgent, resource, ActionRead)
	assert.False(t, decision.Allowed, "federated agent with no delegation edge should be denied")
}

// --- Test: Federated ancestry does not match DelegatedFrom for local principals ---

func TestDelegationCeiling_FederatedAncestryNotUsedForDelegation(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-fed-2")
	localUserID := tid("dc-local-user-fed-2")

	createDCProject(t, s, projectID, "dc-test-fed-project-2")
	createDCUser(t, s, localUserID, "local@test.com", projectID, store.ProjectRoleOwner)

	// Create a policy with DelegatedFrom condition matching the local user
	require.NoError(t, s.CreatePolicy(ctx, &store.Policy{
		ID:           tid("dc-pol-fed-2"),
		Name:         "delegated-test-policy",
		Effect:       "allow",
		ScopeType:    "project",
		ScopeID:      projectID,
		ResourceType: "agent",
		Actions:      []string{"read"},
		Conditions: &store.PolicyConditions{
			DelegatedFrom: &store.DelegatedFromCondition{
				PrincipalID: localUserID,
			},
		},
	}))

	// Federated agent claiming this local user as ancestor
	federatedAgent := NewFederatedAgentIdentity(
		"https://remote.example.com", "fed-agent-2", projectID,
		"test-agent", localUserID, []string{localUserID}, ScopesForRole(AgentRoleFull),
	)

	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Federated agent should NOT be allowed via ancestry fallback (fix 3)
	decision := authz.CheckAccess(ctx, federatedAgent, resource, ActionRead)
	assert.False(t, decision.Allowed,
		"federated agent naming a valid local principal in ancestry should be DENIED at delegation fallback")

	// Local agent with the same ancestry SHOULD be allowed (no regression)
	localAgentID := tid("dc-local-agent-fed-2")
	localAgent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: localAgentID},
		ProjectID: projectID,
		Scopes:    ScopesForRole(AgentRoleFull),
		Ancestry:  []string{localUserID},
	}}
	// We need an agent record for the local agent to pass ancestry checks
	createDCAgent(t, s, localAgentID, projectID, localUserID, AgentRoleFull)
	// Create a delegation edge for the local agent (post-backfill, edges are required)
	createDCEdge(t, s, store.DelegationPrincipalUser, localUserID, store.DelegationPrincipalAgent, localAgentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	decision = authz.CheckAccess(ctx, localAgent, resource, ActionRead)
	assert.True(t, decision.Allowed,
		"local agent with the same ancestry should still be ALLOWED (no regression)")
}

// --- Test: ResolveEffectiveRole no longer uses userCeiling ---

func TestResolveEffectiveRole_NoUserCeiling(t *testing.T) {
	// With the hardcoded ceiling removed, the role should be min(requested, projectMax)
	// regardless of the user's hub role.
	assert.Equal(t, AgentRoleFull, ResolveEffectiveRole(AgentRoleFull, "member", AgentRoleFull))
	assert.Equal(t, AgentRoleFull, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleFull))

	// Project max caps the role
	assert.Equal(t, AgentRoleBaseline, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleBaseline))
	assert.Equal(t, AgentRoleReadOnly, ResolveEffectiveRole(AgentRoleFull, "admin", AgentRoleReadOnly))

	// Requested role is respected
	assert.Equal(t, AgentRoleBaseline, ResolveEffectiveRole(AgentRoleBaseline, "admin", AgentRoleFull))
}

// --- Test: AgentRoleGrandfathered marker works correctly in store ---

func TestAgentRoleGrandfathered_StoreRoundTrip(t *testing.T) {
	_, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-backfill-proj")
	agentID1 := tid("dc-backfill-agent-1")
	agentID2 := tid("dc-backfill-agent-2")

	createDCProject(t, s, projectID, "backfill-test-project")

	// Create agent with grandfathered marker set (simulating post-backfill state)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        agentID1,
		Slug:      "backfill-test-1",
		Name:      "backfill-test-1",
		ProjectID: projectID,
		Phase:     "running",
		CreatedBy: tid("dc-backfill-user"),
		OwnerID:   tid("dc-backfill-user"),
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:              "full",
			AgentRoleGrandfathered: true,
		},
	}))

	// Create agent with explicit role and no marker
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        agentID2,
		Slug:      "backfill-test-2",
		Name:      "backfill-test-2",
		ProjectID: projectID,
		Phase:     "running",
		CreatedBy: tid("dc-backfill-user"),
		OwnerID:   tid("dc-backfill-user"),
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}))

	// Verify grandfathered marker survives store round-trip
	agent1, err := s.GetAgent(ctx, agentID1)
	require.NoError(t, err)
	assert.Equal(t, "full", agent1.AppliedConfig.AgentRole, "role should be preserved")
	assert.True(t, agent1.AppliedConfig.AgentRoleGrandfathered, "grandfathered marker should survive store round-trip")

	// Verify non-grandfathered agent has no marker
	agent2, err := s.GetAgent(ctx, agentID2)
	require.NoError(t, err)
	assert.Equal(t, "baseline", agent2.AppliedConfig.AgentRole, "explicit role should be preserved")
	assert.False(t, agent2.AppliedConfig.AgentRoleGrandfathered, "non-grandfathered agent should have no marker")
}

// --- Test: AgentRoleGrandfathered survives JSON round-trip ---

func TestAgentRoleGrandfathered_JSONRoundTrip(t *testing.T) {
	cfg := &store.AgentAppliedConfig{
		AgentRole:              "full",
		AgentRoleGrandfathered: true,
		Task:                   "test task",
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var parsed store.AgentAppliedConfig
	require.NoError(t, json.Unmarshal(data, &parsed))

	assert.Equal(t, "full", parsed.AgentRole)
	assert.True(t, parsed.AgentRoleGrandfathered, "AgentRoleGrandfathered must survive JSON round-trip")
	assert.Equal(t, "test task", parsed.Task)
}

// --- Test: Delegation edge store operations ---

func TestDelegationEdgeStore_CRUD(t *testing.T) {
	_, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	// Create edge
	edge := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   tid("dc-edge-user-1"),
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    tid("dc-edge-agent-1"),
		ScopeType:     store.RoleScopeProject,
		ScopeID:       tid("dc-edge-proj-1"),
		Role:          "full",
		Active:        true,
	}
	require.NoError(t, s.CreateDelegationEdge(ctx, edge))
	assert.NotEmpty(t, edge.ID, "edge should have ID after creation")

	// Read by delegate
	edges, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, tid("dc-edge-agent-1"))
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, tid("dc-edge-user-1"), edges[0].DelegatorID)
	assert.True(t, edges[0].Active)

	// Read by delegator
	edges, err = s.GetDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, tid("dc-edge-user-1"))
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, tid("dc-edge-agent-1"), edges[0].DelegateID)

	// Deactivate
	require.NoError(t, s.DeactivateDelegationEdge(ctx, edge.ID))

	// Should not appear in active queries
	edges, err = s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, tid("dc-edge-agent-1"))
	require.NoError(t, err)
	assert.Len(t, edges, 0, "deactivated edge should not appear in active queries")
}

// ============================================================================
// Phase 1G fix verification tests
// ============================================================================

// --- Test 1: Grandfathered edge LOSES permission when delegator loses it ---

func TestDelegationCeiling_GrandfatheredEdgeLosesPermission(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-gf-lose")
	userID := tid("dc-user-gf-lose")
	agentID := tid("dc-agent-gf-lose")

	createDCProject(t, s, projectID, "dc-gf-lose-project")
	createDCUser(t, s, userID, "gflose@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Verify user is NOT a system admin (acceptance criterion)
	assertNotSystemAdmin(t, authz, ctx, userID)

	// Create a GRANDFATHERED edge (simulating backfill output)
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   userID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		Grandfathered: true, // backfill-created edge
	}))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Agent should be allowed while user has permission
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed, "agent with grandfathered edge should be allowed while delegator holds permission")

	// Remove the user's role binding (user loses permission)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	// Agent should now be DENIED — grandfathered edge does NOT bypass ceiling
	decision = authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.False(t, decision.Allowed, "agent with grandfathered edge must be denied when delegator loses permission (no bypass)")
}

// --- Test 2: Backfilled agent IS subject to the ceiling ---

func TestDelegationCeiling_BackfilledAgentSubjectToCeiling(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-bf-ceil")
	userID := tid("dc-user-bf-ceil")
	agentID := tid("dc-agent-bf-ceil")

	createDCProject(t, s, projectID, "dc-bf-ceil-project")
	createDCUser(t, s, userID, "bfceil@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Verify user is NOT a system admin (acceptance criterion)
	assertNotSystemAdmin(t, authz, ctx, userID)

	// Create edge with Grandfathered=true (as BackfillDelegationEdges does)
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   userID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		Grandfathered: true,
	}))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Allowed when user has permission
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed, "backfilled agent should be allowed when delegator holds permission")

	// Remove user permission
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}

	// Denied after user loses permission — backfilled agents are NOT exempt
	decision = authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.False(t, decision.Allowed, "backfilled agent must be subject to ceiling (NOT exempt)")
}

// --- Test 3: system/migration delegator: reads work, minting denied ---

func TestDelegationCeiling_SystemMigrationDelegator(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-sysmig")
	agentID := tid("dc-agent-sysmig")

	createDCProject(t, s, projectID, "dc-sysmig-project")
	createDCAgent(t, s, agentID, projectID, tid("dc-owner-sysmig"), AgentRoleFull)

	// Create edge with system/migration delegator (backfill for ambiguous provenance)
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   "system/migration",
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		Grandfathered: true,
	}))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// Read should work — ceiling frozen at agent's recorded role
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed, "agent with system/migration delegator should be allowed for reads")

	// Minting should be DENIED — cannot mint without a live delegator
	decision = authz.CheckAccess(ctx, agent, resource, ActionCreate)
	assert.False(t, decision.Allowed, "agent with system/migration delegator must be denied for minting")

	// Assign (minting) should be DENIED
	decision = authz.CheckAccess(ctx, agent, resource, ActionAssign)
	assert.False(t, decision.Allowed, "agent with system/migration delegator must be denied for assign")
}

// --- Test 4: ActionMint and ActionAssign fail closed on store error ---

func TestDelegationCeiling_MintAndAssignFailClosed(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-mint-fc")
	agentID := tid("dc-agent-mint-fc")

	createDCProject(t, s, projectID, "dc-mint-failclosed")
	createDCAgent(t, s, agentID, projectID, tid("dc-owner-mint-fc"), AgentRoleFull)

	// Create edge to non-existent user (causes ErrNotFound → orphaned delegation)
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   tid("dc-gone-user-mint-fc"),
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
	}))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// ActionMint should fail closed
	decision := authz.CheckAccess(ctx, agent, resource, ActionMint)
	assert.False(t, decision.Allowed, "ActionMint must fail closed when delegator not found")

	// ActionAssign should fail closed
	decision = authz.CheckAccess(ctx, agent, resource, ActionAssign)
	assert.False(t, decision.Allowed, "ActionAssign must fail closed when delegator not found")

	// ActionCreate should fail closed
	decision = authz.CheckAccess(ctx, agent, resource, ActionCreate)
	assert.False(t, decision.Allowed, "ActionCreate must fail closed when delegator not found")
}

// --- Test 5: Post-backfill, no-edge agent is DENIED (not grandfathered) ---

func TestDelegationCeiling_PostBackfillNoEdgeDenied(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-postbf")
	agentID := tid("dc-agent-postbf")

	createDCProject(t, s, projectID, "dc-postbf-project")
	createDCAgent(t, s, agentID, projectID, tid("dc-owner-postbf"), AgentRoleFull)

	// Re-create the backfill marker (authzTestSetup deletes it).
	_, err := s.UpsertHubSetting(ctx, "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err, "should be able to set backfill marker")

	// No delegation edge — post-backfill, this should be DENIED
	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.False(t, decision.Allowed, "post-backfill agent with no edge must be denied (no grandfathering)")
}

// --- Test 8e: Duplicate active edges → fail closed for minting ---

func TestDelegationCeiling_DuplicateEdgesFailClosed(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-dup")
	userID1 := tid("dc-user-dup-1")
	userID2 := tid("dc-user-dup-2")
	agentID := tid("dc-agent-dup")

	createDCProject(t, s, projectID, "dc-dup-project")
	// User1 has limited permission (member, not owner)
	createDCUser(t, s, userID1, "dup1@test.com", projectID, store.ProjectRoleMember)
	// User2 DOES have permission
	createDCUser(t, s, userID2, "dup2@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID1, AgentRoleFull)

	// Manually create TWO active edges for the same delegate+scope
	// (bypassing the unique constraint for testing — we insert them
	// with different scope_ids to avoid the constraint, then the
	// ceiling code counts active edges per principal)
	// Actually, we need to test the code path, so let's use different
	// scope types. But the real test is that the code detects > 1 active
	// edge and fails closed.
	//
	// For this test, we create two edges with the same delegate but
	// different scope_ids so we can exercise the "for _, edge := range edges"
	// loop with both. But the duplicate detection counts ALL active edges
	// returned for that delegate, regardless of scope.
	//
	// Actually — the unique constraint prevents exact duplicates at the DB
	// level. The test verifies the application-level safety net (8e):
	// if somehow two active edges exist, minting is denied.
	// We insert edges for different projects to bypass the unique constraint.
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   userID1, // lacks permission
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
	}))
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   userID2, // has permission
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeSystem, // different scope to bypass unique constraint
		ScopeID:       "",
		Role:          string(AgentRoleFull),
		Active:        true,
	}))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// With two active edges, minting should fail closed (invariant violation)
	decision := authz.CheckAccess(ctx, agent, resource, ActionCreate)
	assert.False(t, decision.Allowed, "minting must fail closed when multiple active edges exist (invariant violation)")
}

// ============================================================================
// Phase 1G Round 2 fix verification tests
// ============================================================================

// --- R2-1: Create → revoke → create → revoke must not fail ---

func TestDelegationEdge_CreateRevokeCreateRevoke(t *testing.T) {
	_, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	delegatorID := tid("dc-user-revoke-cycle")
	agentID := tid("dc-agent-revoke-cycle")
	projectID := tid("dc-proj-revoke-cycle")

	// Create first edge
	edge1 := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          "full",
		Active:        true,
	}
	require.NoError(t, s.CreateDelegationEdge(ctx, edge1), "first edge creation should succeed")

	// Revoke first edge
	require.NoError(t, s.DeactivateDelegationEdge(ctx, edge1.ID), "first revocation should succeed")

	// Create second edge (same delegate + scope)
	edge2 := &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          "full",
		Active:        true,
	}
	require.NoError(t, s.CreateDelegationEdge(ctx, edge2), "second edge creation should succeed")

	// Revoke second edge — this MUST NOT fail with a unique violation.
	// Before the partial index fix, the second revocation would conflict
	// with edge1's inactive row on the unique index.
	require.NoError(t, s.DeactivateDelegationEdge(ctx, edge2.ID), "second revocation must not fail (R2-1)")

	// Verify both edges are now inactive
	edges, err := s.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	assert.Len(t, edges, 0, "no active edges should remain after two revocations")
}

// --- R2-2: isReadOnlyOperation tests ---

func TestIsReadOnlyOperation(t *testing.T) {
	// Read-only operations (fail-open on store errors)
	assert.True(t, isReadOnlyOperation(ActionRead), "ActionRead is read-only")
	assert.True(t, isReadOnlyOperation(ActionList), "ActionList is read-only")
	assert.True(t, isReadOnlyOperation(ActionVerify), "ActionVerify is read-only")

	// Non-read-only operations (fail-closed on store errors)
	assert.False(t, isReadOnlyOperation(ActionCreate), "ActionCreate is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionDelete), "ActionDelete is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionUpdate), "ActionUpdate is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionStop), "ActionStop is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionStopAll), "ActionStopAll is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionAttach), "ActionAttach is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionPortAccess), "ActionPortAccess is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionDispatch), "ActionDispatch is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionRemoveMember), "ActionRemoveMember is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionManage), "ActionManage is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionMint), "ActionMint is NOT read-only")
	assert.False(t, isReadOnlyOperation(ActionAssign), "ActionAssign is NOT read-only")
}

// --- R2-2: Unknown/future action fails closed ---

func TestIsReadOnlyOperation_UnknownActionFailsClosed(t *testing.T) {
	// A future action that hasn't been classified must default to
	// fail-closed (NOT read-only). This verifies the inverted default.
	futureAction := Action("hypothetical_future_action")
	assert.False(t, isReadOnlyOperation(futureAction),
		"unknown/future action must default to fail-closed (not read-only)")
}

// --- R2-2: Non-read, non-minting action fails closed on store error ---

func TestDelegationCeiling_DeleteFailsClosedOnOrphanedDelegation(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-del-fc")
	agentID := tid("dc-agent-del-fc")

	createDCProject(t, s, projectID, "dc-del-failclosed")
	createDCAgent(t, s, agentID, projectID, tid("dc-owner-del-fc"), AgentRoleFull)

	// Create edge with system/migration delegator (orphaned)
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   "system/migration",
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		Grandfathered: true,
	}))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := Resource{Type: "agent", ParentType: "project", ParentID: projectID}

	// ActionDelete is non-minting but also non-read-only.
	// It must fail closed for orphaned delegations.
	decision := authz.CheckAccess(ctx, agent, resource, ActionDelete)
	assert.False(t, decision.Allowed,
		"ActionDelete must fail closed on orphaned delegation (non-read-only, non-minting)")

	// ActionStop likewise
	decision = authz.CheckAccess(ctx, agent, resource, ActionStop)
	assert.False(t, decision.Allowed,
		"ActionStop must fail closed on orphaned delegation (non-read-only)")

	// ActionRead should still work (it IS read-only)
	decision = authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed,
		"ActionRead should still be allowed on orphaned delegation")
}

// --- R2-3: Unmapped permission in handleOrphanedDelegation must deny ---

func TestDelegationCeiling_UnmappedPermissionOrphanedDeny(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-unmap")
	agentID := tid("dc-agent-unmap")

	createDCProject(t, s, projectID, "dc-unmap-project")
	createDCAgent(t, s, agentID, projectID, tid("dc-owner-unmap"), AgentRoleFull)

	// Create edge with system/migration delegator (orphaned)
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   "system/migration",
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agentID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		Grandfathered: true,
	}))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)

	// Use a resource type that produces a permission ID not in the registry
	// (e.g., "imaginary_resource.read" won't be in the permissions registry).
	unmappedResource := Resource{Type: "imaginary_resource", ParentType: "project", ParentID: projectID}

	// Even though this is a "read" action, the resource type is unknown,
	// so the permission will not be in the registry. For orphaned delegations,
	// genuinely unmapped permissions must deny.
	decision := authz.CheckAccess(ctx, agent, unmappedResource, ActionRead)
	// The resolvePermissionID will construct "imaginary_resource.read" which
	// won't be in the registry, so permissionToAgentScope returns "".
	// The isKnownRead check should find it's NOT in the registry and deny.
	assert.False(t, decision.Allowed,
		"unmapped permission in orphaned delegation must deny (R2-3)")
}

// --- R2-4: backfillCompleted caching and behavior ---

func TestBackfillCompleted_PreBackfillReturnsFalse(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	// authzTestSetup removes the backfill marker, so this is pre-backfill.
	_, err := s.GetHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	require.Error(t, err, "marker should be absent for pre-backfill test")

	// backfillCompleted should return false (pre-backfill, ErrNotFound)
	assert.False(t, authz.backfillCompleted(ctx),
		"should be false when marker not found (ErrNotFound)")

	// R3-2: false is NOT cached — re-query returns false again
	assert.False(t, authz.backfillCompleted(ctx),
		"false result should NOT be cached — re-query should also return false")
}

// --- R3-2: Monotonic backfill latch catches up after marker appears ---

func TestBackfillCompleted_MonotonicLatch(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	// First call: marker absent, returns false
	assert.False(t, authz.backfillCompleted(ctx),
		"should be false before marker exists")

	// Simulate backfill completing: insert marker
	_, err := s.UpsertHubSetting(ctx, "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err)

	// Second call: marker now exists, should latch to true
	assert.True(t, authz.backfillCompleted(ctx),
		"should latch to true once marker appears (R3-2 monotonic latch)")

	// Third call: still true (fast path)
	assert.True(t, authz.backfillCompleted(ctx),
		"once latched, should remain true permanently")
}

func TestBackfillCompleted_PostBackfillReturnsTrue(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	// Set the backfill marker BEFORE the first call to backfillCompleted
	_, err := s.UpsertHubSetting(ctx, "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err)

	// backfillCompleted should return true (post-backfill)
	assert.True(t, authz.backfillCompleted(ctx),
		"should be true when marker exists")
}

// --- R2-5: Cross-scope authority leak ---

func TestDelegationCeiling_CrossScopeAuthorityLeak(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectP1 := tid("dc-proj-cross-p1")
	projectP2 := tid("dc-proj-cross-p2")
	userID := tid("dc-user-cross")
	agentID := tid("dc-agent-cross")

	createDCProject(t, s, projectP1, "dc-cross-project-p1")
	createDCProject(t, s, projectP2, "dc-cross-project-p2")
	createDCUser(t, s, userID, "cross@test.com", projectP1, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectP1, userID, AgentRoleFull)

	// Create a delegation edge ONLY in P1
	createDCEdge(t, s, store.DelegationPrincipalUser, userID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectP1, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectP1, AgentRoleFull)

	// Request in P1 should succeed
	resourceP1 := Resource{Type: "agent", ParentType: "project", ParentID: projectP1}
	decisionP1 := authz.CheckAccess(ctx, agent, resourceP1, ActionRead)
	assert.True(t, decisionP1.Allowed, "agent with edge in P1 should be allowed for P1 requests")

	// Request in P2 should be DENIED — the agent has no edge in P2.
	// Before the cross-scope fix, the P1 edge would satisfy the ceiling
	// check for a P2 request.
	resourceP2 := Resource{Type: "agent", ParentType: "project", ParentID: projectP2}
	decisionP2 := authz.CheckAccess(ctx, agent, resourceP2, ActionRead)
	assert.False(t, decisionP2.Allowed,
		"agent with edge in P1 must be DENIED for P2 requests (cross-scope authority leak fix, R2-5)")
}

// --- R2-5: Scope filtering function ---

func TestFilterEdgesByScope(t *testing.T) {
	edges := []*store.DelegationEdge{
		{ID: "e1", ScopeType: store.RoleScopeProject, ScopeID: "proj-1", Active: true},
		{ID: "e2", ScopeType: store.RoleScopeProject, ScopeID: "proj-2", Active: true},
		{ID: "e3", ScopeType: store.RoleScopeSystem, ScopeID: "", Active: true},
	}

	// Filter for project proj-1
	filtered1 := filterEdgesByScope(edges, store.RoleScopeProject, "proj-1")
	require.Len(t, filtered1, 1, "should match only proj-1 edge")
	assert.Equal(t, "e1", filtered1[0].ID)

	// Filter for project proj-2
	filtered2 := filterEdgesByScope(edges, store.RoleScopeProject, "proj-2")
	require.Len(t, filtered2, 1, "should match only proj-2 edge")
	assert.Equal(t, "e2", filtered2[0].ID)

	// Filter for system scope
	filtered3 := filterEdgesByScope(edges, store.RoleScopeSystem, "")
	require.Len(t, filtered3, 1, "should match only system-scoped edge")
	assert.Equal(t, "e3", filtered3[0].ID)

	// Filter for unknown project — no match
	filtered4 := filterEdgesByScope(edges, store.RoleScopeProject, "proj-unknown")
	assert.Len(t, filtered4, 0, "should match no edges for unknown project")
}

// ============================================================================
// Phase 1G Round 3 fix verification tests
// ============================================================================

// --- R3-1: Resource with no ParentType must NOT be denied ---
// Production handler paths commonly build Resource{Type: "project", ID: projectID}
// with no ParentType. Previously this mapped to system scope and denied.

func TestDelegationCeiling_ResourceWithNoParentType(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-noparent")
	userID := tid("dc-user-noparent")
	agentID := tid("dc-agent-noparent")

	createDCProject(t, s, projectID, "dc-noparent-project")
	createDCUser(t, s, userID, "noparent@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Verify user is NOT a system admin
	assertNotSystemAdmin(t, authz, ctx, userID)

	// Create delegation edge
	createDCEdge(t, s, store.DelegationPrincipalUser, userID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)

	// Production-shape Resource: Type="project", ID=projectID, NO ParentType.
	// This is the most common form in handler code paths.
	resource := Resource{Type: "project", ID: projectID}

	// Must NOT be denied — scope is derived from principal, not resource.
	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed,
		"resource with no ParentType must NOT be denied (R3-1: scope derived from principal)")
}

// --- R3-1: Cross-project request: agent with edge in P1, request targets P2 ---

func TestDelegationCeiling_CrossProjectDenied(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectP1 := tid("dc-proj-xproj-p1")
	projectP2 := tid("dc-proj-xproj-p2")
	userID := tid("dc-user-xproj")
	agentID := tid("dc-agent-xproj")

	createDCProject(t, s, projectP1, "dc-xproj-p1")
	createDCProject(t, s, projectP2, "dc-xproj-p2")
	createDCUser(t, s, userID, "xproj@test.com", projectP1, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectP1, userID, AgentRoleFull)

	// Edge ONLY in P1
	createDCEdge(t, s, store.DelegationPrincipalUser, userID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectP1, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectP1, AgentRoleFull)

	// Same-project request succeeds
	resourceP1 := Resource{Type: "project", ID: projectP1}
	decisionP1 := authz.CheckAccess(ctx, agent, resourceP1, ActionRead)
	assert.True(t, decisionP1.Allowed, "same-project request should succeed")

	// Cross-project request: resource IS project P2
	resourceP2 := Resource{Type: "project", ID: projectP2}
	decisionP2 := authz.CheckAccess(ctx, agent, resourceP2, ActionRead)
	assert.False(t, decisionP2.Allowed,
		"cross-project request must be DENIED (R3-1: agent has no edge in P2)")
}

// --- R3-1: Production-shape project_settings Resource without ParentType ---
// Exercises the delegation ceiling with a project-type resource that has
// no ParentType. This simulates handler paths like project_settings_handlers.go
// where Resource{Type: "project", ID: project.ID, OwnerID: ...} is used.
// The project-scoped read baseline lets the upstream access check pass,
// and the ceiling must derive scope from the agent's identity.

func TestDelegationCeiling_ProjectSettingsNoParentType(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectID := tid("dc-proj-pstnopt")
	userID := tid("dc-user-pstnopt")
	agentID := tid("dc-agent-pstnopt")

	createDCProject(t, s, projectID, "dc-pstnopt-project")
	createDCUser(t, s, userID, "pstnopt@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, userID, AgentRoleFull)

	// Verify user is NOT a system admin
	assertNotSystemAdmin(t, authz, ctx, userID)

	// Create delegation edge
	createDCEdge(t, s, store.DelegationPrincipalUser, userID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)

	// Production-shape: Resource{Type: "project", ID: projectID, OwnerID: ...}
	// as seen in project_settings_handlers.go. No ParentType set.
	resource := Resource{Type: "project", ID: projectID, OwnerID: userID}

	decision := authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed,
		"project settings resource with no ParentType must NOT be denied (R3-1: scope from principal)")
}

// --- R3-1: resourceProjectScope tests ---

func TestResourceProjectScope(t *testing.T) {
	// Resource IS a project
	assert.Equal(t, "proj-1", resourceProjectScope(Resource{Type: "project", ID: "proj-1"}))

	// Resource has project parent
	assert.Equal(t, "proj-2", resourceProjectScope(Resource{Type: "agent", ParentType: "project", ParentID: "proj-2"}))

	// Resource with no project info → empty
	assert.Equal(t, "", resourceProjectScope(Resource{Type: "agent", ID: "a1"}))

	// System-scoped resource → empty
	assert.Equal(t, "", resourceProjectScope(Resource{Type: "policy", ParentType: "system"}))
}

// ============================================================================
// Phase 1G Pre-Gate Cleanup tests
// ============================================================================

// --- Cross-project chain denial: parent in Q, child in P → DENIED ---
// This pins a security property that is currently emergent from scope filtering:
// an agent cannot confer authority in a project where it holds none. A parent
// agent in project Q with a delegation edge in Q cannot satisfy the ceiling for
// a child agent operating in project P, even when the child's edge correctly
// points to the parent. The parent's edge is in Q, which does not match a
// request scoped to P.

func TestDelegationCeiling_CrossProjectChainDenied(t *testing.T) {
	authz, s := setupDelegationCeilingTest(t)
	ctx := context.Background()

	projectP := tid("dc-proj-xchain-p")
	projectQ := tid("dc-proj-xchain-q")
	userID := tid("dc-user-xchain")
	parentAgentID := tid("dc-agent-xchain-parent")
	childAgentID := tid("dc-agent-xchain-child")

	// Create both projects.
	createDCProject(t, s, projectP, "dc-xchain-project-p")
	createDCProject(t, s, projectQ, "dc-xchain-project-q")

	// User has permissions in BOTH projects.
	createDCUser(t, s, userID, "xchain@test.com", projectP, store.ProjectRoleOwner)
	createDCUser(t, s, userID, "xchain@test.com", projectQ, store.ProjectRoleOwner)

	// Parent agent lives in project Q.
	createDCAgent(t, s, parentAgentID, projectQ, userID, AgentRoleFull)
	// Child agent lives in project P.
	createDCAgent(t, s, childAgentID, projectP, parentAgentID, AgentRoleBaseline)

	// Verify user is NOT a system admin (acceptance criterion).
	assertNotSystemAdmin(t, authz, ctx, userID)

	// Delegation chain: user → parentAgent (in Q), parentAgent → childAgent (in P).
	// The parent's edge is scoped to project Q.
	createDCEdge(t, s, store.DelegationPrincipalUser, userID,
		store.DelegationPrincipalAgent, parentAgentID,
		store.RoleScopeProject, projectQ, string(AgentRoleFull))
	// The child's edge points to the parent, scoped to project P.
	createDCEdge(t, s, store.DelegationPrincipalAgent, parentAgentID,
		store.DelegationPrincipalAgent, childAgentID,
		store.RoleScopeProject, projectP, string(AgentRoleBaseline))

	// The child agent requests access to a resource in project P.
	childIdentity := dcAgentIdentity(childAgentID, projectP, AgentRoleBaseline)
	resourceP := Resource{Type: "agent", ParentType: "project", ParentID: projectP}

	// The child must be DENIED: walking the chain reaches the parent, but the
	// parent's own project is Q, not P. The ceiling check for the parent in
	// scope (project, P) fails because the parent holds no authority in P.
	// This is the cross-project chain denial security property.
	decision := authz.CheckAccess(ctx, childIdentity, resourceP, ActionRead)
	assert.False(t, decision.Allowed,
		"child agent in project P must be DENIED when parent's edge is in project Q (cross-project chain denial)")

	// Sanity check: the parent agent operating in its OWN project (Q)
	// should still be allowed — its edge is in Q and the request is in Q.
	parentIdentity := dcAgentIdentity(parentAgentID, projectQ, AgentRoleFull)
	resourceQ := Resource{Type: "agent", ParentType: "project", ParentID: projectQ}

	decisionQ := authz.CheckAccess(ctx, parentIdentity, resourceQ, ActionRead)
	assert.True(t, decisionQ.Allowed,
		"parent agent operating in its own project Q should be allowed (sanity check)")
}
