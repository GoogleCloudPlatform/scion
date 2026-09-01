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
		"role_binding.read",
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
// access_constraint.read but does NOT hold access_constraint.admin.
// Constraint administration requires explicit super-admin authority.
func TestBuiltInRoles_HubAdminConstraintPermissions(t *testing.T) {
	adminPerms := hubAdminPermissionIDs()
	permSet := make(map[string]bool, len(adminPerms))
	for _, p := range adminPerms {
		permSet[p] = true
	}

	assert.True(t, permSet["access_constraint.read"],
		"hub-admin must include access_constraint.read")
	assert.False(t, permSet["access_constraint.admin"],
		"hub-admin must NOT include access_constraint.admin — constraint admin requires super-admin")
}

// TestBuiltInRoles_ProjectMemberPermissions verifies that the curated
// project-member role contains expected permissions and excludes permissions
// that should be reserved for higher roles.
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
		"agent.stop_all",
		"project.read",
		"project.list",
	}
	for _, p := range expected {
		assert.True(t, permSet[p],
			"project-member role MUST contain %s", p)
	}

	// Project members MUST NOT have destructive or administrative permissions.
	excluded := []string{
		"agent.delete",
		"agent.update",
		"agent.set_message_mode",
		"project.delete",
		"project.manage",
		"project.update",
	}
	for _, p := range excluded {
		assert.False(t, permSet[p],
			"project-member role MUST NOT contain %s (reserved for admin/owner)", p)
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

	// After reconciliation, marker should now have the hash.
	updatedMarker := getAppliedBuiltInRoleMarker(ctx, s, roleName)
	assert.Equal(t, 1, updatedMarker.Revision)
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
