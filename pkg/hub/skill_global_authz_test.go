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
	"net/http"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Phase 1 vertical slice: skill.create_global authorization
// Design doc §3.1, §7 Phase 1, §8
// ============================================================================

// TestGlobalSkillCreate_GrantedUserAllowed verifies that a user with the
// global-catalog-author role can create a global skill (200).
func TestGlobalSkillCreate_GrantedUserAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupSkillAuthzTest(t)
	ctx := context.Background()

	// Grant alice the global-catalog-author role (system-scoped).
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err, "global-catalog-author role should have been seeded")

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      alice.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "global-authored-skill",
		Scope: "global",
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"global-catalog-author should be able to create a global skill; got: %s", rec.Body.String())
}

// TestGlobalSkillCreate_UngrantedMemberDenied verifies that a hub member
// WITHOUT the global-catalog-author role is denied (403).
func TestGlobalSkillCreate_UngrantedMemberDenied(t *testing.T) {
	srv, _, alice, _, _ := setupSkillAuthzTest(t)

	// Alice is a hub member but has no global-catalog-author binding.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "should-be-denied",
		Scope: "global",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub member without global-catalog-author should be denied; got: %s", rec.Body.String())
}

// TestGlobalSkillCreate_HubAdminStillAllowed verifies that hub-admin can
// still create a global skill after the permission split — the regression
// that matters most (design §5).
func TestGlobalSkillCreate_HubAdminStillAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	admin := createNamedTestUser(t, s, "gca-admin", store.UserRoleMember)
	ensureHubMembership(ctx, s, admin.ID)

	// Assign hub-admin role.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role should have been seeded")

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "admin-global-skill",
		Scope: "global",
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"hub-admin should still be able to create global skills after permission split; got: %s", rec.Body.String())
}

// TestGlobalSkillCreate_CatalogAuthorDeniedOnProjectSkills verifies that a
// user with global-catalog-author can NOT create a project skill — even in a
// project they are otherwise a member of. This is the whole point of the
// design; the permission set contains only global IDs, and they must not
// grant project-level authority. (Design §3.1, Alternative C rejection.)
func TestGlobalSkillCreate_CatalogAuthorDeniedOnProjectSkills(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	// Grant alice global-catalog-author but remove any project-scoped skill.create.
	// She is already a hub member and project owner (from setupSkillAuthzTest).
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      alice.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create a new user with ONLY global-catalog-author and no project role.
	charlie := createNamedTestUser(t, s, "gca-charlie", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      charlie.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Charlie tries to create a project skill — should be denied because
	// global-catalog-author only holds skill.create_global, not skill.create.
	rec := doRequestAsUser(t, srv, charlie, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:    "project-skill-denied",
		Scope:   "project",
		ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"global-catalog-author should NOT grant project skill create; got: %s", rec.Body.String())
}

// TestGlobalCatalogAuthor_ExactPermissionSet verifies that the
// global-catalog-author role contains EXACTLY the expected permission IDs.
// A future careless addition to this role would silently grant hub-wide
// authority because it is bound at system scope. (Design §8.)
func TestGlobalCatalogAuthor_ExactPermissionSet(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err, "global-catalog-author role should have been seeded")

	expected := []string{
		"skill.create_global",
	}
	sort.Strings(expected)

	actual := make([]string, len(rd.Permissions))
	copy(actual, rd.Permissions)
	sort.Strings(actual)

	assert.Equal(t, expected, actual,
		"global-catalog-author must contain EXACTLY these permissions and nothing else")
}

// TestGlobalSkillCreate_HubAdminHasCreateGlobal verifies that hub-admin's
// seeded permission set includes skill.create_global.
func TestGlobalSkillCreate_HubAdminHasCreateGlobal(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role should have been seeded")

	found := false
	for _, p := range rd.Permissions {
		if p == "skill.create_global" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"hub-admin should include skill.create_global to avoid regression; has: %v", rd.Permissions)
}

// TestGlobalSkillCreate_SuperAdminHasCreateGlobal verifies that super-admin
// gets skill.create_global automatically through allPermissionIDs().
func TestGlobalSkillCreate_SuperAdminHasCreateGlobal(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "super-admin role should have been seeded")

	found := false
	for _, p := range rd.Permissions {
		if p == "skill.create_global" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"super-admin should include skill.create_global via allPermissionIDs(); has %d perms", len(rd.Permissions))
}

// TestSeedReconcile_GlobalCatalogAuthorAppearsOnUpgrade simulates a database
// seeded by the previous code (without global-catalog-author) and verifies
// that the reconcile path creates the role and updates hub-admin.
func TestSeedReconcile_GlobalCatalogAuthorAppearsOnUpgrade(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// testServer() already runs the seeder, so global-catalog-author exists.
	// Verify it was created.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err, "global-catalog-author should exist after seeding")
	assert.Equal(t, store.SystemRoleGlobalCatalogAuthor, rd.Name)

	// Verify hub-admin was reconciled with the new permission.
	hubAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	hasCreateGlobal := false
	for _, p := range hubAdmin.Permissions {
		if p == "skill.create_global" {
			hasCreateGlobal = true
			break
		}
	}
	assert.True(t, hasCreateGlobal,
		"hub-admin should have skill.create_global after reconciliation")

	// Now simulate re-seeding (as if on a subsequent restart). The role should
	// still be present and unchanged.
	for _, role := range BuiltInRoles() {
		reconcileBuiltInRole(ctx, s, role)
	}

	rd2, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err, "global-catalog-author should still exist after re-seeding")
	assert.Equal(t, rd.ID, rd2.ID, "role ID should be stable across re-seeds")
}

// TestSeedReconcile_OperatorOverrideRespected verifies that when an operator
// has bumped the stored revision of global-catalog-author higher than the
// code's revision, the seeder does not downgrade it.
func TestSeedReconcile_OperatorOverrideRespected(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Simulate operator override by recording a higher revision marker.
	overrideMarker := builtInRoleMarker{Revision: 99, PermHash: "operator-hash"}
	recordBuiltInRoleMarker(ctx, s, store.SystemRoleGlobalCatalogAuthor, overrideMarker)

	// Run reconcile again.
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleGlobalCatalogAuthor {
			reconcileBuiltInRole(ctx, s, role)
		}
	}

	// Verify stored marker is still the operator's.
	applied := getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleGlobalCatalogAuthor)
	assert.Equal(t, 99, applied.Revision,
		"reconciler should not downgrade operator-overridden revision")
}

// TestSkillCreateGlobal_PermissionRegistered verifies that skill.create_global
// exists in the permissions registry.
func TestSkillCreateGlobal_PermissionRegistered(t *testing.T) {
	found := false
	for _, p := range permissions.Registry {
		if p.ID == "skill.create_global" {
			found = true
			assert.Equal(t, permissions.ResourceSkill, p.Resource)
			assert.Equal(t, permissions.ActionCreateGlobal, p.Action)
			assert.Equal(t, "skill:create_global", p.UATScope)
			break
		}
	}
	assert.True(t, found, "skill.create_global should exist in the permissions registry")
}

// TestGlobalWriteAction_MapsCorrectly verifies the globalWriteAction helper.
func TestGlobalWriteAction_MapsCorrectly(t *testing.T) {
	tests := []struct {
		name   string
		scope  string
		action Action
		want   Action
	}{
		{"global/create", store.SkillScopeGlobal, ActionCreate, ActionCreateGlobal},
		{"core/create", store.SkillScopeCore, ActionCreate, ActionCreateGlobal},
		{"project/create unchanged", store.SkillScopeProject, ActionCreate, ActionCreate},
		{"user/create unchanged", store.SkillScopeUser, ActionCreate, ActionCreate},
		{"global/update not yet mapped", store.SkillScopeGlobal, ActionUpdate, ActionUpdate},
		{"global/delete not yet mapped", store.SkillScopeGlobal, ActionDelete, ActionDelete},
		{"global/read unchanged", store.SkillScopeGlobal, ActionRead, ActionRead},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := globalWriteAction(tt.scope, tt.action)
			assert.Equal(t, tt.want, got)
		})
	}
}

// createNamedTestUser creates a user with a given name prefix and role.
func createNamedTestUser(t *testing.T, s store.Store, namePrefix, role string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid(namePrefix),
		Email:       namePrefix + "@test.com",
		DisplayName: namePrefix,
		Role:        role,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}
