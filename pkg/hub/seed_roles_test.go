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

// Tests for PG1: Curated built-in role definitions, deterministic
// reconciliation, and hub-member RoleBinding seeding.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Curated role permission tests
// =============================================================================

// TestBuiltInRoles_HubMemberExcludesCrossProjectPermissions verifies that the
// hub-member role does NOT contain project.list, project.read, agent.list, or
// agent.read at system scope. This is the CRITICAL security fix for the cross-
// project visibility vulnerability documented in the design doc.
func TestBuiltInRoles_HubMemberExcludesCrossProjectPermissions(t *testing.T) {
	memberPerms := hubMemberPermissionIDs()
	permSet := make(map[string]bool, len(memberPerms))
	for _, p := range memberPerms {
		permSet[p] = true
	}

	// These MUST NOT be present in the hub-member role
	crossProjectPerms := []string{
		"project.list",
		"project.read",
		"agent.list",
		"agent.read",
	}
	for _, p := range crossProjectPerms {
		assert.False(t, permSet[p],
			"hub-member role MUST NOT contain %s (cross-project visibility risk)", p)
	}
}

// TestBuiltInRoles_HubViewerExcludesCrossProjectPermissions verifies the same
// for hub-viewer.
func TestBuiltInRoles_HubViewerExcludesCrossProjectPermissions(t *testing.T) {
	viewerPerms := hubViewerPermissionIDs()
	permSet := make(map[string]bool, len(viewerPerms))
	for _, p := range viewerPerms {
		permSet[p] = true
	}

	crossProjectPerms := []string{
		"project.list",
		"project.read",
		"agent.list",
		"agent.read",
	}
	for _, p := range crossProjectPerms {
		assert.False(t, permSet[p],
			"hub-viewer role MUST NOT contain %s (cross-project visibility risk)", p)
	}
}

// TestBuiltInRoles_HubMemberContainsExpectedPermissions verifies that the
// curated hub-member role includes the permissions that replace the ~13 seeded
// policies (per-type read + project create).
func TestBuiltInRoles_HubMemberContainsExpectedPermissions(t *testing.T) {
	memberPerms := hubMemberPermissionIDs()
	permSet := make(map[string]bool, len(memberPerms))
	for _, p := range memberPerms {
		permSet[p] = true
	}

	// These MUST be present (replacing the old per-type read policies)
	expected := []string{
		"user.read", "user.list",
		"group.read", "group.list",
		"template.read", "template.list",
		"harness_config.read", "harness_config.list",
		"broker.read", "broker.list",
		"gcp_service_account.read", "gcp_service_account.list",
		// OBS-5: policy.read and policy.list removed — Policy API returns 410 Gone.
		"skill.read", "skill.list",
		"quota.read",
		"role.read",
		// S1: role_binding.read removed — project members use project-scoped endpoint.
		"hub.settings.read",
		// project.create (replacing hub-member-create-projects policy)
		"project.create",
	}
	for _, p := range expected {
		assert.True(t, permSet[p],
			"hub-member role MUST contain %s", p)
	}
}

// TestBuiltInRoles_AllPermissionsExistInRegistry verifies that every permission
// ID in every built-in role exists in the canonical permissions registry.
func TestBuiltInRoles_AllPermissionsExistInRegistry(t *testing.T) {
	registrySet := make(map[string]bool, len(permissions.Registry))
	for _, p := range permissions.Registry {
		registrySet[p.ID] = true
	}

	for _, role := range BuiltInRoles() {
		for _, perm := range role.Permissions {
			assert.True(t, registrySet[perm],
				"role %q contains permission %q which is not in the registry",
				role.Name, perm)
		}
	}
}

// TestBuiltInRoles_HubMemberAndViewerAreCurated verifies that hub-member and
// hub-viewer have fewer permissions than a naive "all read+list" derivation
// would produce, confirming they use curated lists rather than dynamic
// registry scanning.
func TestBuiltInRoles_HubMemberAndViewerAreCurated(t *testing.T) {
	hubMemberPerms := hubMemberPermissionIDs()
	hubViewerPerms := hubViewerPermissionIDs()

	// Count: hub-member should have a fixed, known count (not all read+list perms)
	allReadListPerms := permissionIDsByActions("read", "list")
	assert.Less(t, len(hubMemberPerms), len(allReadListPerms),
		"hub-member should have FEWER permissions than all read+list perms "+
			"(curated, not derived from action class)")
	assert.Less(t, len(hubViewerPerms), len(allReadListPerms),
		"hub-viewer should have FEWER permissions than all read+list perms")
}

// TestBuiltInRoles_SuperAdminHasAllPermissions verifies that super-admin
// contains every permission in the registry (by design).
func TestBuiltInRoles_SuperAdminHasAllPermissions(t *testing.T) {
	var superAdminPerms []string
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleSuperAdmin {
			superAdminPerms = role.Permissions
			break
		}
	}
	require.NotNil(t, superAdminPerms, "super-admin role must exist")

	// Should include every registry permission
	registryIDs := allPermissionIDs()
	assert.Equal(t, len(registryIDs), len(superAdminPerms),
		"super-admin must have ALL registry permissions")
}

// TestBuiltInRoles_HubAdminConstraintPermissions verifies that hub-admin holds
// both access_constraint.read and access_constraint.admin, making access
// constraint management operator-complete without requiring super-admin.
func TestBuiltInRoles_HubAdminConstraintPermissions(t *testing.T) {
	adminPerms := hubAdminPermissionIDs()
	permSet := make(map[string]bool, len(adminPerms))
	for _, p := range adminPerms {
		permSet[p] = true
	}

	assert.True(t, permSet["access_constraint.read"],
		"hub-admin must include access_constraint.read")
	assert.True(t, permSet["access_constraint.admin"],
		"hub-admin must include access_constraint.admin for operator-complete access constraint management")
}

// TestBuiltInRoles_ProjectMemberPermissions verifies that the curated
// project-member role contains expected permissions and excludes permissions
// that should be reserved for higher roles or are meaningless in project scope.
func TestBuiltInRoles_ProjectMemberPermissions(t *testing.T) {
	memberPerms := projectMemberCuratedPermissionIDs()
	permSet := make(map[string]bool, len(memberPerms))
	for _, p := range memberPerms {
		permSet[p] = true
	}

	// Project members MUST have basic operational permissions.
	expected := []string{
		"agent.create",
		"agent.read",
		"agent.list",
		"agent.message",
		"project.read",
		"project.list",
	}
	for _, p := range expected {
		assert.True(t, permSet[p],
			"project-member role MUST contain %s", p)
	}

	// Project members MUST NOT have destructive, administrative, or
	// hub-level permissions.
	excluded := []string{
		"agent.delete",
		"agent.update",
		"agent.set_message_mode",
		"agent.stop_all", // R2: bulk stop is administrative
		"project.create", // R2: hub-level, meaningless in project scope
		"project.delete",
		"project.manage",
		"project.update",
		"skill.create", // R2: skill creation is admin/owner action
	}
	for _, p := range excluded {
		assert.False(t, permSet[p],
			"project-member role MUST NOT contain %s (reserved for admin/owner or hub-level)", p)
	}
}

// =============================================================================
// Deterministic reconciliation tests
// =============================================================================

// TestReconcileBuiltInRoles_CreatesNewRoles verifies that reconciliation
// creates role definitions that don't exist yet.
func TestReconcileBuiltInRoles_CreatesNewRoles(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// testServer's New() already calls reconcileBuiltInRoles, so all roles
	// should exist. Verify they do.
	for _, role := range BuiltInRoles() {
		rd, err := s.GetRoleDefinitionByName(ctx, role.Name, role.ScopeType)
		require.NoError(t, err, "role %s should exist after reconciliation", role.Name)
		assert.True(t, rd.System, "role %s should be marked as system", role.Name)
		assert.Equal(t, len(role.Permissions), len(rd.Permissions),
			"role %s should have the correct number of permissions", role.Name)
	}
}

// TestReconcileBuiltInRoles_Idempotent verifies that running reconciliation
// twice produces the same result (same code = same permissions).
func TestReconcileBuiltInRoles_Idempotent(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Record current permissions for each role
	beforePerms := make(map[string][]string)
	for _, role := range BuiltInRoles() {
		rd, err := s.GetRoleDefinitionByName(ctx, role.Name, role.ScopeType)
		require.NoError(t, err)
		beforePerms[role.Name] = rd.Permissions
	}

	// Run reconciliation again
	reconcileBuiltInRoles(ctx, s)

	// Permissions should be unchanged
	for _, role := range BuiltInRoles() {
		rd, err := s.GetRoleDefinitionByName(ctx, role.Name, role.ScopeType)
		require.NoError(t, err)
		assert.Equal(t, beforePerms[role.Name], rd.Permissions,
			"role %s permissions should be unchanged after idempotent reconciliation",
			role.Name)
	}
}

// TestReconcileBuiltInRoles_UpdatesOnRevisionBump verifies that when the code
// revision is higher than the stored revision, permissions are updated.
func TestReconcileBuiltInRoles_UpdatesOnRevisionBump(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Manually set the hub-member role to a stale permission set
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	stalePerms := []string{"user.read"} // intentionally incomplete
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, stalePerms))

	// Verify it's stale
	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Equal(t, stalePerms, rd.Permissions)

	// Reset the stored revision to 0 to simulate a pre-existing deployment
	revJSON, _ := json.Marshal(0)
	_, _ = s.UpsertHubSetting(ctx, builtInRoleRevisionKey(store.SystemRoleHubMember),
		revJSON, "system", -1, "seeded")

	// Run reconciliation — it should detect the lower revision and update
	reconcileBuiltInRoles(ctx, s)

	// Permissions should now match the code-declared set
	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	expectedPerms := hubMemberPermissionIDs()
	assert.Equal(t, len(expectedPerms), len(rd.Permissions),
		"hub-member permissions should be updated to match code-declared set")
}

// TestReconcileBuiltInRoles_DoesNotDowngrade verifies that reconciliation does
// not downgrade permissions when the stored revision is already at or above the
// code revision.
func TestReconcileBuiltInRoles_DoesNotDowngrade(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	originalPerms := rd.Permissions

	// Set a higher revision than the code declares
	revJSON, _ := json.Marshal(999)
	_, _ = s.UpsertHubSetting(ctx, builtInRoleRevisionKey(store.SystemRoleHubMember),
		revJSON, "system", -1, "seeded")

	// Manually add an extra permission
	extraPerms := append([]string{}, originalPerms...)
	extraPerms = append(extraPerms, "agent.read")
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, extraPerms))

	// Run reconciliation — it should NOT touch the permissions
	reconcileBuiltInRoles(ctx, s)

	rd, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Equal(t, len(extraPerms), len(rd.Permissions),
		"permissions should not be modified when stored revision >= code revision")
}

// TestReconcileBuiltInRoles_RevisionTracking verifies that revision tracking
// via hub settings works correctly.
func TestReconcileBuiltInRoles_RevisionTracking(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// After initial reconciliation, marker should be stored with revision and hash.
	for _, role := range BuiltInRoles() {
		marker := getAppliedBuiltInRoleMarker(ctx, s, role.Name)
		assert.Equal(t, role.Revision, marker.Revision,
			"role %s should have revision %d after reconciliation", role.Name, role.Revision)
		assert.Equal(t, permListHash(role.Permissions), marker.PermHash,
			"role %s should have matching perm hash after reconciliation", role.Name)
	}
}

// TestReconcileBuiltInRole_PermHashChangeTriggersUpdate verifies that when
// the permission list changes at the same revision, reconciliation still
// fires and updates the role definition. This is the R-6 fix: dynamic
// permission lists (super-admin, agent roles) must converge even when the
// code revision is unchanged.
func TestReconcileBuiltInRole_PermHashChangeTriggersUpdate(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Pick the hub-member role for a controlled test.
	roleName := store.SystemRoleHubMember

	// Fetch the current role definition.
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	originalPerms := rd.Permissions

	// Simulate a prior reconciliation that recorded the SAME revision but a
	// DIFFERENT permission hash (as if the permission list had been different
	// in a previous code version).
	staleMarker := builtInRoleMarker{
		Revision: 1,
		PermHash: "stale_hash_does_not_match",
	}
	recordBuiltInRoleMarker(ctx, s, roleName, staleMarker)

	// Strip a permission from the DB to verify reconciliation restores it.
	require.Greater(t, len(originalPerms), 1, "precondition: role must have >1 permission")
	strippedPerms := originalPerms[:len(originalPerms)-1]
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, strippedPerms))

	// Verify precondition: permission is missing.
	rd2, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	require.Equal(t, len(strippedPerms), len(rd2.Permissions),
		"precondition: permission should be stripped")

	// Run reconciliation — should trigger because permHash doesn't match.
	reconcileBuiltInRoles(ctx, s)

	// Verify permissions were restored.
	rd3, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.ElementsMatch(t, hubMemberPermissionIDs(), rd3.Permissions,
		"reconciliation should restore the full permission list when hash changes")

	// Verify the marker was updated with the correct hash.
	marker := getAppliedBuiltInRoleMarker(ctx, s, roleName)
	assert.Equal(t, permListHash(hubMemberPermissionIDs()), marker.PermHash,
		"marker should have the updated perm hash")
}

// TestReconcileBuiltInRole_LegacyIntegerMarkerTriggersReconciliation verifies
// that a legacy integer-only revision marker (pre-R-6) triggers reconciliation
// because it has an empty PermHash, which never matches.
func TestReconcileBuiltInRole_LegacyIntegerMarkerTriggersReconciliation(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	roleName := store.SystemRoleHubMember

	// Write a legacy integer marker (simulating a pre-R-6 deployment).
	revJSON, _ := json.Marshal(1)
	_, err := s.UpsertHubSetting(ctx, builtInRoleRevisionKey(roleName),
		revJSON, "system", -1, "seeded")
	require.NoError(t, err)

	// Verify the legacy marker parses correctly.
	marker := getAppliedBuiltInRoleMarker(ctx, s, roleName)
	assert.Equal(t, 1, marker.Revision)
	assert.Empty(t, marker.PermHash, "legacy marker should have empty PermHash")

	// Run reconciliation — should trigger because PermHash is empty.
	reconcileBuiltInRoles(ctx, s)

	// After reconciliation, marker should now have the hash and the
	// current revision (hub-member is at revision 2 after S1 fix).
	updatedMarker := getAppliedBuiltInRoleMarker(ctx, s, roleName)
	assert.Equal(t, 2, updatedMarker.Revision)
	assert.NotEmpty(t, updatedMarker.PermHash, "marker should have PermHash after reconciliation")
	assert.Equal(t, permListHash(hubMemberPermissionIDs()), updatedMarker.PermHash)
}

// =============================================================================
// Hub-member RoleBinding seeding tests
// =============================================================================

// TestSeedDefaultGroupsAndBindings_CreatesHubMemberBinding verifies that
// startup seeding creates a system-scoped RoleBinding of the hub-member role
// to the hub-members group.
func TestSeedDefaultGroupsAndBindings_CreatesHubMemberBinding(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// The hub-members group should exist
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	// There should be a RoleBinding for the hub-members group
	bindings, err := s.ListRoleBindingsForPrincipal(ctx,
		store.RoleBindingPrincipalGroup, group.ID)
	require.NoError(t, err)

	// Find the hub-member binding
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	var found bool
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID && b.ScopeType == store.RoleScopeSystem {
			found = true
			assert.Equal(t, store.RoleBindingPrincipalGroup, b.PrincipalType)
			assert.Equal(t, group.ID, b.PrincipalID)
			assert.Equal(t, store.SystemReconcileCreatedBy, b.CreatedBy)
			break
		}
	}
	assert.True(t, found,
		"hub-members group should have a system-scoped hub-member role binding")
}

// TestSeedDefaultGroupsAndBindings_Idempotent verifies that seeding the hub-member
// binding twice does not create duplicates.
func TestSeedDefaultGroupsAndBindings_Idempotent(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	// Count bindings before
	bindingsBefore, err := s.ListRoleBindingsForPrincipal(ctx,
		store.RoleBindingPrincipalGroup, group.ID)
	require.NoError(t, err)

	// Seed again
	seedDefaultGroupsAndBindings(ctx, s)

	// Count bindings after — should be the same
	bindingsAfter, err := s.ListRoleBindingsForPrincipal(ctx,
		store.RoleBindingPrincipalGroup, group.ID)
	require.NoError(t, err)

	assert.Equal(t, len(bindingsBefore), len(bindingsAfter),
		"seeding again should not create duplicate bindings")
}

// =============================================================================
// Cross-project visibility regression tests (with reconciled roles)
// =============================================================================

// TestPG1_CrossProjectVisibilityFixed verifies that after PG1 curates the
// hub-member role, the cross-project visibility vulnerability is closed.
// This is the counterpart to TestGolden_CrossProjectVisibilityRegression.
func TestPG1_CrossProjectVisibilityFixed(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// The hub-member role should NOT contain project.list or agent.list
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	permSet := make(map[string]bool, len(rd.Permissions))
	for _, p := range rd.Permissions {
		permSet[p] = true
	}

	assert.False(t, permSet["project.list"],
		"hub-member role in store MUST NOT contain project.list")
	assert.False(t, permSet["project.read"],
		"hub-member role in store MUST NOT contain project.read")
	assert.False(t, permSet["agent.list"],
		"hub-member role in store MUST NOT contain agent.list")
	assert.False(t, permSet["agent.read"],
		"hub-member role in store MUST NOT contain agent.read")
}

// TestPG1_HubMemberRoleBindingDecision verifies that the role binding for the
// hub-members group grants the curated permissions but NOT cross-project ones.
func TestPG1_HubMemberRoleBindingDecision(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	// Create a hub member user with a role binding
	userID := tid("pg1-member-test")
	createTestUserWithRole(t, s, userID, "pg1-member@test.com", "member", store.SystemRoleHubMember)

	user := NewAuthenticatedUser(userID, "pg1-member@test.com", "PG1 Member", "member", "api")

	// Should be able to read users (directory access via hub-member role)
	userReadReq := AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "user", ID: "some-user"},
		Action:     ActionRead,
		Permission: "user.read",
	}
	decision := authz.Decide(ctx, userReadReq)
	assert.True(t, decision.Allowed,
		"hub-member should be able to read users via role binding")

	// Should NOT be able to list all projects (cross-project visibility fix)
	projectListReq := AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "project", ID: "hub"},
		Action:     ActionList,
		Permission: "project.list",
	}
	decision = authz.Decide(ctx, projectListReq)
	assert.False(t, decision.Allowed,
		"hub-member should NOT be able to list all projects (PG1 fix)")

	// Should NOT be able to list all agents
	agentListReq := AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "agent", ID: "hub"},
		Action:     ActionList,
		Permission: "agent.list",
	}
	decision = authz.Decide(ctx, agentListReq)
	assert.False(t, decision.Allowed,
		"hub-member should NOT be able to list all agents (PG1 fix)")
}

// =============================================================================
// R2: Project-role permission cleanup tests
// =============================================================================

// agentSelfPermissions returns the set of agent-self credential permissions
// that should NOT appear in any project-scoped role intended for human users.
// These permissions are for agent identities acting on their own behalf.
func agentSelfPermissions() []string {
	return []string{
		"agent.status_update",
		"agent.log_append",
		"agent.token_refresh",
		"agent.identity_token",
		"agent.port_forward",
		"agent.notify",
	}
}

// hubLevelProjectPermissions returns permissions that target hub-level
// operations and are meaningless in a project-scoped role binding.
func hubLevelProjectPermissions() []string {
	return []string{
		"project.create",   // creates new projects at hub scope
		"project.clone",    // enforced against Resource{ID: "hub"}
		"project.register", // registers/creates projects from external sources
	}
}

// TestR2_ProjectOwnerExcludesAgentSelfPermissions verifies that project-owner
// does NOT contain agent-self credential permissions (status_update, log_append,
// token_refresh, identity_token, port_forward, notify). These belong to agent
// identities, not human project administrators.
func TestR2_ProjectOwnerExcludesAgentSelfPermissions(t *testing.T) {
	ownerPerms := projectOwnerPermissionIDs()
	permSet := make(map[string]bool, len(ownerPerms))
	for _, p := range ownerPerms {
		permSet[p] = true
	}

	for _, p := range agentSelfPermissions() {
		assert.False(t, permSet[p],
			"project-owner MUST NOT contain agent-self permission %s", p)
	}
}

// TestR2_ProjectAdminExcludesAgentSelfPermissions verifies the same for
// project-admin.
func TestR2_ProjectAdminExcludesAgentSelfPermissions(t *testing.T) {
	adminPerms := projectAdminPermissionIDs()
	permSet := make(map[string]bool, len(adminPerms))
	for _, p := range adminPerms {
		permSet[p] = true
	}

	for _, p := range agentSelfPermissions() {
		assert.False(t, permSet[p],
			"project-admin MUST NOT contain agent-self permission %s", p)
	}
}

// TestR2_ProjectScopedRolesExcludeHubLevelPermissions verifies that all three
// project-scoped built-in roles exclude hub-level operations (project.create,
// project.clone, project.register). These are enforced at hub scope and are
// meaningless/misleading in a role bound to one existing project.
func TestR2_ProjectScopedRolesExcludeHubLevelPermissions(t *testing.T) {
	roles := map[string][]string{
		"project-owner":  projectOwnerPermissionIDs(),
		"project-admin":  projectAdminPermissionIDs(),
		"project-member": projectMemberCuratedPermissionIDs(),
	}

	for roleName, perms := range roles {
		permSet := make(map[string]bool, len(perms))
		for _, p := range perms {
			permSet[p] = true
		}
		for _, p := range hubLevelProjectPermissions() {
			assert.False(t, permSet[p],
				"%s MUST NOT contain hub-level permission %s", roleName, p)
		}
	}
}

// TestR2_ProjectOwnerRetainsHumanAgentManagement verifies that project-owner
// still contains legitimate human control-plane permissions over agent
// resources.
func TestR2_ProjectOwnerRetainsHumanAgentManagement(t *testing.T) {
	ownerPerms := projectOwnerPermissionIDs()
	permSet := make(map[string]bool, len(ownerPerms))
	for _, p := range ownerPerms {
		permSet[p] = true
	}

	// Human agent-management permissions that MUST remain.
	humanAgentPerms := []string{
		"agent.attach",
		"agent.create",
		"agent.delete",
		"agent.list",
		"agent.message",
		"agent.port_access",
		"agent.read",
		"agent.set_message_mode",
		"agent.stop_all",
		"agent.update",
	}
	for _, p := range humanAgentPerms {
		assert.True(t, permSet[p],
			"project-owner MUST retain human agent-management permission %s", p)
	}
}

// TestR2_ProjectMemberExcludesStopAllAndSkillCreate verifies the explicit
// user-requested removals from project-member.
func TestR2_ProjectMemberExcludesStopAllAndSkillCreate(t *testing.T) {
	memberPerms := projectMemberCuratedPermissionIDs()
	permSet := make(map[string]bool, len(memberPerms))
	for _, p := range memberPerms {
		permSet[p] = true
	}

	assert.False(t, permSet["agent.stop_all"],
		"project-member MUST NOT contain agent.stop_all (bulk stop is admin-level)")
	assert.False(t, permSet["skill.create"],
		"project-member MUST NOT contain skill.create (skill creation is admin/owner)")
}

// TestR2_ProjectRoleExactPermissionSets verifies the exact permission sets for
// each project-scoped role after R2 cleanup. This is a regression guard: any
// permission addition or removal must be deliberate and reflected here.
func TestR2_ProjectRoleExactPermissionSets(t *testing.T) {
	tests := []struct {
		name  string
		perms []string
		want  []string
	}{
		{
			name:  "project-owner",
			perms: projectOwnerPermissionIDs(),
			want: []string{
				"agent.attach", "agent.create", "agent.delete", "agent.list",
				"agent.message", "agent.port_access", "agent.read",
				"agent.set_message_mode", "agent.stop_all", "agent.update",
				"harness_config.create", "harness_config.delete",
				"harness_config.list", "harness_config.read", "harness_config.update",
				"project.delete", "project.list", "project.manage",
				"project.read", "project.secret_read", "project.update",
				"scheduled_event.create", "scheduled_event.delete",
				"scheduled_event.list", "scheduled_event.read", "scheduled_event.update",
				"skill.create", "skill.delete", "skill.list", "skill.read",
				"skill.register", "skill.update",
				"template.create", "template.delete", "template.list",
				"template.read", "template.update",
			},
		},
		{
			name:  "project-admin",
			perms: projectAdminPermissionIDs(),
			want: []string{
				"agent.attach", "agent.create", "agent.list",
				"agent.message", "agent.port_access", "agent.read",
				"agent.stop_all", "agent.update",
				"harness_config.create",
				"harness_config.list", "harness_config.read", "harness_config.update",
				"project.list", "project.manage",
				"project.read", "project.secret_read", "project.update",
				"scheduled_event.create",
				"scheduled_event.list", "scheduled_event.read", "scheduled_event.update",
				"skill.create", "skill.list", "skill.read",
				"skill.register", "skill.update",
				"template.create", "template.list",
				"template.read", "template.update",
			},
		},
		{
			name:  "project-member",
			perms: projectMemberCuratedPermissionIDs(),
			want: []string{
				"agent.create", "agent.list", "agent.message", "agent.read",
				"harness_config.create", "harness_config.list", "harness_config.read",
				"project.list", "project.read",
				"scheduled_event.create", "scheduled_event.list", "scheduled_event.read",
				"skill.list", "skill.read",
				"template.create", "template.list", "template.read",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, tt.perms,
				"%s exact permission set mismatch", tt.name)
		})
	}
}

// TestR2_ProjectRoleRevisionsBumped verifies that all project-scoped roles
// have their revision bumped to 2 after the R2 permission cleanup.
func TestR2_ProjectRoleRevisionsBumped(t *testing.T) {
	for _, role := range BuiltInRoles() {
		if role.ScopeType != store.RoleScopeProject {
			continue
		}
		assert.Equal(t, 2, role.Revision,
			"project-scoped role %s should be at revision 2 after R2 cleanup", role.Name)
	}
}

// TestR2_ReconciliationConvergesProjectRoles verifies that startup
// reconciliation updates existing project-scoped role definitions to the
// corrected R2 permission sets.
func TestR2_ReconciliationConvergesProjectRoles(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectRoles := []struct {
		name     string
		permFunc func() []string
	}{
		{store.ProjectRoleOwner, projectOwnerPermissionIDs},
		{store.ProjectRoleAdmin, projectAdminPermissionIDs},
		{store.ProjectRoleMember, projectMemberCuratedPermissionIDs},
	}

	for _, pr := range projectRoles {
		rd, err := s.GetRoleDefinitionByName(ctx, pr.name, store.RoleScopeProject)
		require.NoError(t, err, "role %s should exist after reconciliation", pr.name)

		expectedPerms := pr.permFunc()
		assert.ElementsMatch(t, expectedPerms, rd.Permissions,
			"role %s in store should match code-declared R2 permission set", pr.name)

		// Verify revision marker was recorded
		marker := getAppliedBuiltInRoleMarker(ctx, s, pr.name)
		assert.Equal(t, 2, marker.Revision,
			"role %s revision marker should be 2", pr.name)
		assert.Equal(t, permListHash(expectedPerms), marker.PermHash,
			"role %s perm hash should match", pr.name)
	}
}

// TestR2_ReconciliationUpdatesStaleProjectRoles verifies that running
// reconciliation against a store that has R1 (pre-cleanup) permission sets
// converges them to the R2 sets.
func TestR2_ReconciliationUpdatesStaleProjectRoles(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Simulate a pre-R2 project-member with the old permissions (including
	// agent.stop_all, skill.create, project.create).
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	stalePerms := []string{
		"agent.create", "agent.list", "agent.message", "agent.read",
		"agent.stop_all", // should be removed by R2
		"harness_config.create", "harness_config.list", "harness_config.read",
		"project.create", // should be removed by R2
		"project.list", "project.read",
		"scheduled_event.create", "scheduled_event.list", "scheduled_event.read",
		"skill.create", // should be removed by R2
		"skill.list", "skill.read",
		"template.create", "template.list", "template.read",
	}
	require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, stalePerms))

	// Reset the marker to revision 1 (pre-R2)
	staleMarker := builtInRoleMarker{Revision: 1, PermHash: permListHash(stalePerms)}
	recordBuiltInRoleMarker(ctx, s, store.ProjectRoleMember, staleMarker)

	// Run reconciliation
	reconcileBuiltInRoles(ctx, s)

	// Verify convergence
	rd, err = s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	expectedPerms := projectMemberCuratedPermissionIDs()
	assert.ElementsMatch(t, expectedPerms, rd.Permissions,
		"project-member should converge to R2 permission set after reconciliation")

	// Verify the removed permissions are gone
	permSet := make(map[string]bool, len(rd.Permissions))
	for _, p := range rd.Permissions {
		permSet[p] = true
	}
	assert.False(t, permSet["agent.stop_all"], "agent.stop_all should be removed after convergence")
	assert.False(t, permSet["skill.create"], "skill.create should be removed after convergence")
	assert.False(t, permSet["project.create"], "project.create should be removed after convergence")
}

// TestR2_ProjectOwnerDelegationCeiling verifies that a project-owner can
// delegate (assign) the project-admin and project-member roles, and that the
// delegation ceiling correctly reflects the R2 cleanup — removed permissions
// cannot be delegated.
func TestR2_ProjectOwnerDelegationCeiling(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("r2-delegation-project")
	userID := tid("r2-delegation-owner")
	createDelegateTestProject(t, s, projectID, "r2-test", userID)
	createTestUserWithProjectRole(t, s, userID, "owner@test.com", projectID, store.ProjectRoleOwner)

	user := NewAuthenticatedUser(userID, "owner@test.com", "Owner", "member", "api")

	// Owner should be able to delegate project-admin
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:             GrantTypeRoleBinding,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		RoleDefinitionID: adminRD.ID,
	})
	assert.True(t, decision.Allowed,
		"project-owner should be able to delegate project-admin (subset of owner perms)")

	// Owner should be able to delegate project-member
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	decision = authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:             GrantTypeRoleBinding,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		RoleDefinitionID: memberRD.ID,
	})
	assert.True(t, decision.Allowed,
		"project-owner should be able to delegate project-member")
}

// TestR2_ProjectAdminCannotDelegateRemovedPermissions verifies that a
// project-admin cannot delegate a custom role containing permissions that were
// removed from project-admin in R2 (e.g. agent-self permissions).
func TestR2_ProjectAdminCannotDelegateRemovedPermissions(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("r2-admin-ceiling-project")
	userID := tid("r2-admin-ceiling-user")
	createDelegateTestProject(t, s, projectID, "r2-admin-test", userID)
	createTestUserWithProjectRole(t, s, userID, "admin@test.com", projectID, store.ProjectRoleAdmin)

	user := NewAuthenticatedUser(userID, "admin@test.com", "Admin", "member", "api")

	// A custom role containing agent-self permissions should fail delegation
	// because project-admin no longer holds those permissions.
	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		ScopeType:       store.RoleScopeProject,
		ScopeID:         projectID,
		RolePermissions: []string{"agent.status_update", "agent.log_append"},
	})
	assert.False(t, decision.Allowed,
		"project-admin should NOT be able to delegate agent-self permissions (removed in R2)")
}

// TestR2_EffectiveGrantsProjectMember verifies that a project-member user
// gets the correct effective grants after R2 cleanup — specifically, that
// removed permissions do NOT produce positive authorization decisions.
func TestR2_EffectiveGrantsProjectMember(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("r2-grants-project")
	userID := tid("r2-grants-member")
	createDelegateTestProject(t, s, projectID, "r2-grants-test", userID)
	createTestUserWithProjectRole(t, s, userID, "member@test.com", projectID, store.ProjectRoleMember)

	user := NewAuthenticatedUser(userID, "member@test.com", "Member", "member", "api")

	// For project-scoped checks, projectIDForResource derives the project ID:
	//  - Resource{Type: "project", ID: projectID} → projectID
	//  - Resource{Type: "agent", ParentType: "project", ParentID: projectID} → projectID
	projectResource := Resource{Type: "project", ID: projectID}
	agentResource := Resource{Type: "agent", ID: "some-agent", ParentType: "project", ParentID: projectID}

	// Permissions that SHOULD be granted
	allowedPerms := []struct {
		resource   Resource
		action     Action
		permission string
	}{
		{agentResource, ActionCreate, "agent.create"},
		{agentResource, ActionRead, "agent.read"},
		{agentResource, ActionList, "agent.list"},
		{projectResource, ActionRead, "project.read"},
	}
	for _, tc := range allowedPerms {
		decision := authz.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(user),
			Credential: credentialContextForIdentity(user),
			Resource:   tc.resource,
			Action:     tc.action,
			Permission: tc.permission,
		})
		assert.True(t, decision.Allowed,
			"project-member should have %s", tc.permission)
	}

	// Permissions that should be DENIED after R2 cleanup
	deniedPerms := []struct {
		resource   Resource
		action     Action
		permission string
	}{
		{agentResource, ActionStopAll, "agent.stop_all"},
		{Resource{Type: "skill", ID: "some-skill", ParentType: "project", ParentID: projectID}, ActionCreate, "skill.create"},
		{projectResource, ActionCreate, "project.create"},
	}
	for _, tc := range deniedPerms {
		decision := authz.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(user),
			Credential: credentialContextForIdentity(user),
			Resource:   tc.resource,
			Action:     tc.action,
			Permission: tc.permission,
		})
		assert.False(t, decision.Allowed,
			"project-member should NOT have %s after R2 cleanup", tc.permission)
	}
}
