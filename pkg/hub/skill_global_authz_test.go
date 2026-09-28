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
//
// Charlie has project-member (which grants skill.read/list but NOT skill.create)
// plus global-catalog-author (system-scoped, only skill.create_global). The
// denial must come from scope isolation — skill.create_global in a system-scoped
// binding does not satisfy the project-scoped skill.create check — not from
// Charlie lacking project membership entirely.
func TestGlobalSkillCreate_CatalogAuthorDeniedOnProjectSkills(t *testing.T) {
	srv, s, _, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	// Create Charlie with hub membership + project-member (has skill.read/list,
	// NOT skill.create) + global-catalog-author (system-scoped).
	charlie := createNamedTestUser(t, s, "gca-charlie", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)

	// Give Charlie project-member in the project. project-member has skill.read
	// and skill.list but NOT skill.create (confirmed in projectMemberCuratedPermissionIDs).
	createTestUserWithProjectRole(t, s, charlie.ID, charlie.Email, project.ID, store.ProjectRoleMember)

	// Also grant global-catalog-author (system-scoped).
	gcaRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: gcaRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      charlie.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Charlie tries to create a project skill — should be denied.
	// The denial is because skill.create_global (from global-catalog-author,
	// system-scoped) does NOT satisfy the project-scoped skill.create check.
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

// TestSeedReconcile_GlobalCatalogAuthorAppearsOnUpgrade exercises the genuine
// revision 3 → 4 upgrade path. It constructs a raw store with hub-admin at
// revision 3 (no skill.create_global, no global-catalog-author), then runs
// the new reconciler and asserts both that the role appears and that hub-admin
// gains the permission.
func TestSeedReconcile_GlobalCatalogAuthorAppearsOnUpgrade(t *testing.T) {
	// Create a raw store — no testServer, so no automatic seeding.
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.Migrate(context.Background()))
	ctx := context.Background()

	// ── Simulate the pre-upgrade state (revision 3 hub-admin, no global-catalog-author) ──
	// Seed hub-admin at revision 3 with a permission set that does NOT include
	// skill.create_global. This is the exact set from the previous code.
	rev3Perms := hubAdminPermissionIDs()
	// Remove skill.create_global to get the revision-3 set.
	var rev3PermsFiltered []string
	for _, p := range rev3Perms {
		if p != "skill.create_global" {
			rev3PermsFiltered = append(rev3PermsFiltered, p)
		}
	}
	rev3Role := BuiltInRole{
		Name:        store.SystemRoleHubAdmin,
		Description: "Hub administrator with scopeable admin permissions",
		ScopeType:   store.RoleScopeSystem,
		Revision:    3,
		Permissions: rev3PermsFiltered,
	}
	reconcileBuiltInRole(ctx, s, rev3Role)

	// Verify pre-upgrade state.
	hubAdminBefore, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin should exist after initial seed")
	for _, p := range hubAdminBefore.Permissions {
		require.NotEqual(t, "skill.create_global", p,
			"hub-admin should not have skill.create_global before upgrade")
	}
	_, err = s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.ErrorIs(t, err, store.ErrNotFound,
		"global-catalog-author should not exist before upgrade")
	markerBefore := getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleHubAdmin)
	require.Equal(t, 3, markerBefore.Revision,
		"hub-admin revision marker should be 3 before upgrade")

	// ── Run the new reconciler (simulates server restart with new code) ──
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleHubAdmin || role.Name == store.SystemRoleGlobalCatalogAuthor {
			reconcileBuiltInRole(ctx, s, role)
		}
	}

	// ── Assert post-upgrade state ──
	// global-catalog-author should now exist.
	newGCA, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err, "global-catalog-author should have been created by reconciler")
	assert.Equal(t, store.SystemRoleGlobalCatalogAuthor, newGCA.Name)

	// hub-admin should now include skill.create_global (revision 3 → 4).
	hubAdminAfter, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	hasCreateGlobal := false
	for _, p := range hubAdminAfter.Permissions {
		if p == "skill.create_global" {
			hasCreateGlobal = true
			break
		}
	}
	assert.True(t, hasCreateGlobal,
		"hub-admin should have skill.create_global after revision 3 → 4 upgrade")

	// Verify the revision marker was updated.
	marker := getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleHubAdmin)
	assert.Equal(t, 4, marker.Revision, "hub-admin revision marker should be 4 after upgrade")
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
// exists in the permissions registry with the correct fields.
func TestSkillCreateGlobal_PermissionRegistered(t *testing.T) {
	found := false
	for _, p := range permissions.Registry {
		if p.ID == "skill.create_global" {
			found = true
			assert.Equal(t, permissions.ResourceSkill, p.Resource)
			assert.Equal(t, permissions.ActionCreateGlobal, p.Action)
			// UATScope intentionally empty: enforceUATConstraints denies every
			// UAT on hub-level resources (authz.go:~1322), so a UAT scope for
			// this permission would be a surface we advertise and cannot honour.
			// Adding it back is gated on open question Q1. See design §3.2.
			assert.Empty(t, p.UATScope,
				"skill.create_global must not have a UATScope until Q1 resolves — "+
					"it would silently widen skill:manage alias expansion and break PAT minting")
			break
		}
	}
	assert.True(t, found, "skill.create_global should exist in the permissions registry")
}

// TestActionCreateGlobal_ConstantsAgree guards the duplicated ActionCreateGlobal
// constant across pkg/hub and pkg/hub/permissions. The packages cannot import
// each other (circular dependency), so duplication is structurally necessary.
// This assertion catches drift at the constant level.
func TestActionCreateGlobal_ConstantsAgree(t *testing.T) {
	if string(ActionCreateGlobal) != permissions.ActionCreateGlobal {
		t.Fatalf("ActionCreateGlobal constants diverged: hub=%q, permissions=%q",
			ActionCreateGlobal, permissions.ActionCreateGlobal)
	}
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
