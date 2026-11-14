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
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

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
		"skill.delete_global",
		"skill.update_global",
		"template.create_global",
		"template.delete_global",
		"template.update_global",
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
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
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

	// Verify the revision marker was updated to the code-declared
	// revision (4 when skill.create_global was added; later revisions add
	// further permissions).
	codeRevision := 0
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleHubAdmin {
			codeRevision = role.Revision
		}
	}
	require.GreaterOrEqual(t, codeRevision, 4)
	marker := getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleHubAdmin)
	assert.Equal(t, codeRevision, marker.Revision,
		"hub-admin revision marker should match the code revision after upgrade")
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

// TestSkillCreateGlobal_PermissionRegistered pins skill.create_global in the
// permissions registry: its selector is skill:create_global, it stays out of
// the skill:manage alias, and a token may carry it only on a hub boundary.
func TestSkillCreateGlobal_PermissionRegistered(t *testing.T) {
	found := false
	for _, p := range permissions.Registry {
		if p.ID == "skill.create_global" {
			found = true
			assert.Equal(t, permissions.ResourceSkill, p.Resource)
			assert.Equal(t, permissions.ActionCreateGlobal, p.Action)
			assert.Equal(t, "skill:create_global", p.UATScope)
			assert.True(t, p.ExcludeFromManageAlias, "skill.create_global must stay out of the skill:manage alias")
			break
		}
	}
	assert.True(t, found, "skill.create_global should exist in the permissions registry")
	assert.NotContains(t, permissions.UATManageScopesFor(permissions.ResourceSkill), "skill:create_global")
	boundaries, ok := permissions.SelectorAllowedBoundaries("skill.create_global")
	assert.True(t, ok)
	assert.Equal(t, []permissions.BoundaryKind{permissions.BoundaryKindHub}, boundaries)
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

// TestCatalogWriteAction_MapsCorrectly verifies the catalogWriteAction helper
// across catalog resource kinds, scopes, and actions.
func TestCatalogWriteAction_MapsCorrectly(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		scope  string
		action Action
		want   Action
	}{
		{"skill/global/create", permissions.ResourceSkill, store.SkillScopeGlobal, ActionCreate, ActionCreateGlobal},
		{"skill/global/update", permissions.ResourceSkill, store.SkillScopeGlobal, ActionUpdate, ActionUpdateGlobal},
		{"skill/global/delete", permissions.ResourceSkill, store.SkillScopeGlobal, ActionDelete, ActionDeleteGlobal},
		{"skill/global/read unchanged", permissions.ResourceSkill, store.SkillScopeGlobal, ActionRead, ActionRead},
		{"skill/core/create interim", permissions.ResourceSkill, store.SkillScopeCore, ActionCreate, ActionCreateGlobal},
		{"skill/core/update interim plain", permissions.ResourceSkill, store.SkillScopeCore, ActionUpdate, ActionUpdate},
		{"skill/core/delete interim plain", permissions.ResourceSkill, store.SkillScopeCore, ActionDelete, ActionDelete},
		{"skill/project/create unchanged", permissions.ResourceSkill, store.SkillScopeProject, ActionCreate, ActionCreate},
		{"skill/project/update unchanged", permissions.ResourceSkill, store.SkillScopeProject, ActionUpdate, ActionUpdate},
		{"skill/project/delete unchanged", permissions.ResourceSkill, store.SkillScopeProject, ActionDelete, ActionDelete},
		{"skill/user/create unchanged", permissions.ResourceSkill, store.SkillScopeUser, ActionCreate, ActionCreate},
		{"skill/user/update unchanged", permissions.ResourceSkill, store.SkillScopeUser, ActionUpdate, ActionUpdate},
		{"skill/user/delete unchanged", permissions.ResourceSkill, store.SkillScopeUser, ActionDelete, ActionDelete},
		{"template/global/create", permissions.ResourceTemplate, store.TemplateScopeGlobal, ActionCreate, ActionCreateGlobal},
		{"template/global/update", permissions.ResourceTemplate, store.TemplateScopeGlobal, ActionUpdate, ActionUpdateGlobal},
		{"template/global/delete", permissions.ResourceTemplate, store.TemplateScopeGlobal, ActionDelete, ActionDeleteGlobal},
		{"template/global/read unchanged", permissions.ResourceTemplate, store.TemplateScopeGlobal, ActionRead, ActionRead},
		{"template/project/create unchanged", permissions.ResourceTemplate, store.TemplateScopeProject, ActionCreate, ActionCreate},
		{"template/project/update unchanged", permissions.ResourceTemplate, store.TemplateScopeProject, ActionUpdate, ActionUpdate},
		{"template/project/delete unchanged", permissions.ResourceTemplate, store.TemplateScopeProject, ActionDelete, ActionDelete},
		{"template/user/create unchanged", permissions.ResourceTemplate, store.TemplateScopeUser, ActionCreate, ActionCreate},
		{"template/user/update unchanged", permissions.ResourceTemplate, store.TemplateScopeUser, ActionUpdate, ActionUpdate},
		{"template/user/delete unchanged", permissions.ResourceTemplate, store.TemplateScopeUser, ActionDelete, ActionDelete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := catalogWriteAction(tt.kind, tt.scope, tt.action)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// Phase 2 Slice 1: skill.update_global & skill.delete_global authorization
// Design doc §3.1–§3.5, §7 Phase 1, §8
// ============================================================================

func bindGlobalCatalogAuthor(t *testing.T, s store.Store, userID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err)
	created, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return created
}

// TestGlobalSkillUpdate_CatalogAuthorAllowed verifies that a user with
// global-catalog-author can update, publish versions for, upload/finalize files
// on, and deprecate versions of a global skill created by a DIFFERENT user
// (proving role-governed, not owner-governed, authority).
func TestGlobalSkillUpdate_CatalogAuthorAllowed(t *testing.T) {
	srv, s, alice, bob, _ := setupSkillAuthzTest(t)
	srv.SetStorage(newMockStorage("gca-update-storage"))
	ensureHubMembership(context.Background(), s, bob.ID)

	// Alice creates the global skill; Bob is a fellow global-catalog-author who
	// did NOT create it.
	bindGlobalCatalogAuthor(t, s, alice.ID)
	bindGlobalCatalogAuthor(t, s, bob.ID)

	createRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "shared-global-skill",
		Scope: store.SkillScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateSkillResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Skill)
	created := createResp.Skill
	require.Equal(t, alice.ID, created.OwnerID)

	// Bob updates metadata via PATCH.
	patchRec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/skills/"+created.ID, UpdateSkillRequest{
		Description: "maintained by bob",
	})
	assert.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

	// Bob creates a draft version (publishSkillVersion).
	pubRec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/skills/"+created.ID+"/versions", PublishVersionRequest{
		Version: "1.0.0",
	})
	require.Equal(t, http.StatusCreated, pubRec.Code, pubRec.Body.String())
	var pubResp PublishVersionResponse
	require.NoError(t, json.Unmarshal(pubRec.Body.Bytes(), &pubResp))
	require.NotNil(t, pubResp.Version)

	// Bob sees the draft version on version-addressed reads (skillVersionsAllStatuses).
	verRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills/"+created.ID+"/versions/"+pubResp.Version.ID, nil)
	assert.Equal(t, http.StatusOK, verRec.Code, verRec.Body.String())

	// Bob requests upload URLs.
	upRec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/skills/"+created.ID+"/upload", map[string]interface{}{
		"version": "1.0.0",
		"files":   []FileUploadRequest{{Path: "SKILL.md", Size: 12}},
	})
	assert.Equal(t, http.StatusOK, upRec.Code, upRec.Body.String())
}

// TestGlobalSkillDelete_CatalogAuthorAllowed verifies that a user with
// global-catalog-author can delete a global skill created by a different user.
func TestGlobalSkillDelete_CatalogAuthorAllowed(t *testing.T) {
	srv, s, alice, bob, _ := setupSkillAuthzTest(t)

	bindGlobalCatalogAuthor(t, s, alice.ID)
	bindGlobalCatalogAuthor(t, s, bob.ID)

	createRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "shared-global-skill-del",
		Scope: store.SkillScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateSkillResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Skill)
	created := createResp.Skill

	delRec := doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/skills/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())
}

// TestGlobalSkillUpdate_UngrantedMemberDenied verifies that an ordinary hub
// member without global-catalog-author is denied update, version publish, and
// delete on a global skill (403).
func TestGlobalSkillUpdate_UngrantedMemberDenied(t *testing.T) {
	srv, s, alice, bob, _ := setupSkillAuthzTest(t)
	ensureHubMembership(context.Background(), s, bob.ID)

	bindGlobalCatalogAuthor(t, s, alice.ID)
	createRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "member-denied-global-skill",
		Scope: store.SkillScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateSkillResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Skill)
	created := createResp.Skill

	// Bob is a hub member without global-catalog-author.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"patch", http.MethodPatch, "/api/v1/skills/" + created.ID, UpdateSkillRequest{Description: "nope"}},
		{"publish-version", http.MethodPost, "/api/v1/skills/" + created.ID + "/versions", PublishVersionRequest{Version: "1.0.0"}},
		{"upload", http.MethodPost, "/api/v1/skills/" + created.ID + "/upload", map[string]interface{}{"version": "1.0.0", "files": []FileUploadRequest{{Path: "SKILL.md", Size: 5}}}},
		{"finalize", http.MethodPost, "/api/v1/skills/" + created.ID + "/finalize", FinalizeSkillVersionRequest{Version: "1.0.0"}},
		{"delete", http.MethodDelete, "/api/v1/skills/" + created.ID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, bob, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
}

// TestGlobalSkillUpdate_OwnerWithoutRoleDenied pins design Q5: when a user
// creates a global skill while holding global-catalog-author (stamping
// OwnerID/CreatedBy) and their role binding is subsequently removed, they lose
// all write and draft-visibility authority on that global skill via both
// session and pre-minted hub UAT.
func TestGlobalSkillUpdate_OwnerWithoutRoleDenied(t *testing.T) {
	srv, s, alice, bob, _ := setupSkillAuthzTest(t)
	ctx := context.Background()
	ensureHubMembership(ctx, s, bob.ID)

	rb := bindGlobalCatalogAuthor(t, s, alice.ID)
	// Bob is a separate global-catalog-author who is NOT the creator/owner.
	bindGlobalCatalogAuthor(t, s, bob.ID)

	// Mint a hub UAT while Alice still holds global-catalog-author.
	aliceToken := mintHubConfigToken(t, srv, alice.ID, hubBoundary(),
		"skill:read", "skill:create_global", "skill:update_global", "skill:delete_global")

	createRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "former-author-global-skill",
		Scope: store.SkillScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateSkillResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Skill)
	created := createResp.Skill
	require.Equal(t, alice.ID, created.OwnerID)
	require.NotEqual(t, bob.ID, created.OwnerID)

	// Create a draft version before removing Alice's role binding.
	pubRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+created.ID+"/versions", PublishVersionRequest{
		Version: "0.1.0",
	})
	require.Equal(t, http.StatusCreated, pubRec.Code, pubRec.Body.String())
	var pubResp PublishVersionResponse
	require.NoError(t, json.Unmarshal(pubRec.Body.Bytes(), &pubResp))

	// Remove Alice's global-catalog-author binding.
	require.NoError(t, s.DeleteRoleBinding(ctx, rb.ID))

	// Non-creator author (Bob, holds global-catalog-author) CAN see draft
	// versions of the global skill in both get and list.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills/"+created.ID+"/versions/"+pubResp.Version.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "non-creator global-catalog-author must see draft version: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills/"+created.ID+"/versions", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bobList store.ListResult[store.SkillVersion]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bobList))
	require.Len(t, bobList.Items, 1, "non-creator global-catalog-author must list draft version")
	assert.Equal(t, pubResp.Version.ID, bobList.Items[0].ID)

	// Ex-author creator (Alice, OwnerID/CreatedBy stamped, role removed):
	// update, publish-version, and delete must all 403, and the draft version
	// is hidden in both get (404) and list (0 items).
	rec = doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/skills/"+created.ID, UpdateSkillRequest{Description: "denied"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "session patch after role removal: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/"+created.ID+"/versions", PublishVersionRequest{Version: "0.2.0"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "session publish after role removal: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/skills/"+created.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "session delete after role removal: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+created.ID+"/versions/"+pubResp.Version.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "draft version must not leak via OwnerID/CreatedBy after role removal: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+created.ID+"/versions", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var aliceList store.ListResult[store.SkillVersion]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &aliceList))
	assert.Empty(t, aliceList.Items, "draft version must not appear in list for ex-author creator")

	// Pre-minted hub UAT requests: live authority is gone, so update, publish,
	// and delete must also 403.
	rec = callWithKey(t, srv, aliceToken, http.MethodPatch, "/api/v1/skills/"+created.ID, UpdateSkillRequest{Description: "denied"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "token patch after role removal: %s", rec.Body.String())

	rec = callWithKey(t, srv, aliceToken, http.MethodPost, "/api/v1/skills/"+created.ID+"/versions", PublishVersionRequest{Version: "0.2.0"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "token publish after role removal: %s", rec.Body.String())

	rec = callWithKey(t, srv, aliceToken, http.MethodDelete, "/api/v1/skills/"+created.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "token delete after role removal: %s", rec.Body.String())
}

// TestGlobalSkillUpdate_HubAdminAllowed verifies that hub-admin retains full
// update and delete authority on global skills via skill.update_global and
// skill.delete_global.
func TestGlobalSkillUpdate_HubAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	author := createNamedTestUser(t, s, "gca-author-for-admin", store.UserRoleMember)
	ensureHubMembership(ctx, s, author.ID)
	bindGlobalCatalogAuthor(t, s, author.ID)

	admin := createNamedTestUser(t, s, "gca-hub-admin-rw", store.UserRoleMember)
	ensureHubMembership(ctx, s, admin.ID)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	createRec := doRequestAsUser(t, srv, author, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "admin-managed-global-skill",
		Scope: store.SkillScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var createResp CreateSkillResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Skill)
	created := createResp.Skill

	patchRec := doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/skills/"+created.ID, UpdateSkillRequest{
		Description: "updated by hub-admin",
	})
	assert.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

	delRec := doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/skills/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())
}

// TestGlobalSkillUpdate_PlainSkillUpdateDeniedOnGlobal verifies that a custom
// system role carrying plain skill.update and skill.delete (without the
// *_global twins) is denied on a global skill (403) while still authorizing
// writes on a project skill.
func TestGlobalSkillUpdate_PlainSkillUpdateDeniedOnGlobal(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	// Seed a global skill and a project skill.
	now := time.Now()
	globalSkill := &store.Skill{
		ID: tid("plain-role-global-skill"), Name: "plain-global", Slug: "plain-global",
		Scope: store.SkillScopeGlobal, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, globalSkill))
	projectSkill := &store.Skill{
		ID: tid("plain-role-project-skill"), Name: "plain-project", Slug: "plain-project",
		Scope: store.SkillScopeProject, ScopeID: project.ID, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, projectSkill))

	// Give Charlie project-member in project (no skill.update/delete) plus a
	// custom system role with plain skill.update and skill.delete only.
	charlie := createNamedTestUser(t, s, "plain-skill-sys-user", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	createTestUserWithProjectRole(t, s, charlie.ID, charlie.Email, project.ID, store.ProjectRoleMember)

	customRole := &store.RoleDefinition{
		ID:          tid("custom-plain-skill-rw"),
		Name:        "custom-plain-skill-rw",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"skill.update", "skill.delete"},
	}
	_, err := s.CreateRoleDefinition(ctx, customRole)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      charlie.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        alice.ID,
	})
	require.NoError(t, err)

	// Denied on global skill (requires skill.update_global / skill.delete_global).
	rec := doRequestAsUser(t, srv, charlie, http.MethodPatch, "/api/v1/skills/"+globalSkill.ID, UpdateSkillRequest{Description: "nope"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, charlie, http.MethodDelete, "/api/v1/skills/"+globalSkill.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Allowed on project skill (checks plain skill.update / skill.delete).
	rec = doRequestAsUser(t, srv, charlie, http.MethodPatch, "/api/v1/skills/"+projectSkill.ID, UpdateSkillRequest{Description: "ok"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, charlie, http.MethodDelete, "/api/v1/skills/"+projectSkill.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestGlobalCatalogAuthor_CannotWriteProjectOrUserSkills verifies that
// global-catalog-author's system-scoped *_global twins never authorize create,
// update, or delete on a project-scoped skill (member or non-member) or on
// another user's user-scoped skill.
func TestGlobalCatalogAuthor_CannotWriteProjectOrUserSkills(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	now := time.Now()

	// Seed a project skill in project (Alice owns project) and a user skill owned by Alice.
	projSkill := &store.Skill{
		ID: tid("gca-iso-proj-skill"), Name: "iso-proj-skill", Slug: "iso-proj-skill",
		Scope: store.SkillScopeProject, ScopeID: project.ID, OwnerID: alice.ID, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, projSkill))

	aliceUserSkill := &store.Skill{
		ID: tid("gca-iso-user-skill"), Name: "iso-user-skill", Slug: "iso-user-skill",
		Scope: store.SkillScopeUser, ScopeID: alice.ID, OwnerID: alice.ID, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, aliceUserSkill))

	// Bob is a non-member of project with global-catalog-author.
	bindGlobalCatalogAuthor(t, s, bob.ID)

	// Charlie is a project-member of project with global-catalog-author.
	charlie := createNamedTestUser(t, s, "gca-iso-charlie", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	createTestUserWithProjectRole(t, s, charlie.ID, charlie.Email, project.ID, store.ProjectRoleMember)
	bindGlobalCatalogAuthor(t, s, charlie.ID)

	for _, u := range []*store.User{bob, charlie} {
		rec := doRequestAsUser(t, srv, u, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
			Name: "denied-proj-create", Scope: store.SkillScopeProject, ScopeID: project.ID,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, "user %s create project skill: %s", u.ID, rec.Body.String())

		rec = doRequestAsUser(t, srv, u, http.MethodPatch, "/api/v1/skills/"+projSkill.ID, UpdateSkillRequest{Description: "nope"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "user %s update project skill: %s", u.ID, rec.Body.String())

		rec = doRequestAsUser(t, srv, u, http.MethodDelete, "/api/v1/skills/"+projSkill.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "user %s delete project skill: %s", u.ID, rec.Body.String())

		rec = doRequestAsUser(t, srv, u, http.MethodPatch, "/api/v1/skills/"+aliceUserSkill.ID, UpdateSkillRequest{Description: "nope"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "user %s update other user's skill: %s", u.ID, rec.Body.String())

		rec = doRequestAsUser(t, srv, u, http.MethodDelete, "/api/v1/skills/"+aliceUserSkill.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "user %s delete other user's skill: %s", u.ID, rec.Body.String())
	}
}

// TestGlobalSkill_HubAdminAndSuperAdminHaveTwins verifies that hub-admin and
// super-admin both carry skill.update_global and skill.delete_global.
func TestGlobalSkill_HubAdminAndSuperAdminHaveTwins(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	for _, roleName := range []string{store.SystemRoleHubAdmin, store.SystemRoleSuperAdmin} {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
		require.NoError(t, err)
		assert.Contains(t, rd.Permissions, "skill.create_global", "%s missing skill.create_global", roleName)
		assert.Contains(t, rd.Permissions, "skill.update_global", "%s missing skill.update_global", roleName)
		assert.Contains(t, rd.Permissions, "skill.delete_global", "%s missing skill.delete_global", roleName)
	}
}

// TestSeedReconcile_GlobalCatalogAuthorR2AppliesOnUpgrade verifies that
// reconciling from global-catalog-author revision 1 (create_global only) to
// the current revision adds skill.update_global, skill.delete_global, and the
// template.*_global twins.
func TestSeedReconcile_GlobalCatalogAuthorR2AppliesOnUpgrade(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()

	r1 := BuiltInRole{
		Name:        store.SystemRoleGlobalCatalogAuthor,
		Description: "Creates global (hub-scoped) skills",
		ScopeType:   store.RoleScopeSystem,
		Revision:    1,
		Permissions: []string{"skill.create_global"},
	}
	reconcileBuiltInRole(ctx, s, r1)

	before, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err)
	require.Equal(t, []string{"skill.create_global"}, before.Permissions)
	require.Equal(t, 1, getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleGlobalCatalogAuthor).Revision)

	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleGlobalCatalogAuthor {
			reconcileBuiltInRole(ctx, s, role)
		}
	}

	after, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"skill.create_global", "skill.update_global", "skill.delete_global",
		"template.create_global", "template.update_global", "template.delete_global",
	}, after.Permissions)
	assert.Equal(t, 3, getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleGlobalCatalogAuthor).Revision)
}

// TestSkillGlobalTwins_PermissionRegistered pins skill.update_global and
// skill.delete_global in the permissions registry, applicability tables, and
// selector registry, and verifies ActionUpdateGlobal/ActionDeleteGlobal match
// across pkg/hub and pkg/hub/permissions.
func TestSkillGlobalTwins_PermissionRegistered(t *testing.T) {
	require.Equal(t, string(ActionUpdateGlobal), permissions.ActionUpdateGlobal)
	require.Equal(t, string(ActionDeleteGlobal), permissions.ActionDeleteGlobal)

	manageScopes := permissions.UATManageScopesFor(permissions.ResourceSkill)
	for _, tc := range []struct {
		id     string
		action string
		scope  string
	}{
		{"skill.update_global", permissions.ActionUpdateGlobal, "skill:update_global"},
		{"skill.delete_global", permissions.ActionDeleteGlobal, "skill:delete_global"},
	} {
		var found *permissions.Permission
		for i := range permissions.Registry {
			if permissions.Registry[i].ID == tc.id {
				found = &permissions.Registry[i]
				break
			}
		}
		require.NotNil(t, found, "%s must exist in permissions.Registry", tc.id)
		assert.Equal(t, permissions.ResourceSkill, found.Resource)
		assert.Equal(t, tc.action, found.Action)
		assert.Equal(t, tc.scope, found.UATScope)
		assert.True(t, found.ExcludeFromManageAlias, "%s must stay out of skill:manage", tc.id)
		assert.NotContains(t, manageScopes, tc.scope)

		applies, reviewed := permissions.AppliesToExistingProjectTarget(tc.id)
		assert.True(t, reviewed)
		assert.False(t, applies, "%s must not apply to project targets", tc.id)

		boundaries, ok := permissions.SelectorAllowedBoundaries(tc.id)
		assert.True(t, ok)
		assert.Equal(t, []permissions.BoundaryKind{permissions.BoundaryKindHub}, boundaries)

		assert.Equal(t, []permissions.TargetClassKind{permissions.TargetClassKindGlobalCatalog}, permissions.SupportedTargetClassesFor(tc.id))
		collClasses, collReviewed := permissions.CollectionTargetClassesFor(tc.id)
		assert.True(t, collReviewed)
		assert.Empty(t, collClasses, "%s targets an existing skill record, never a collection", tc.id)

		// CapabilityNone must not hide the twin from live UAT selector
		// resolution, UATScopeOptions picker listings, or custom-role validation.
		sel, selOK := permissions.ResolveSelector(tc.scope)
		require.True(t, selOK, "ResolveSelector(%q) must succeed", tc.scope)
		assert.Equal(t, []string{tc.id}, sel.PermissionIDs)
		assert.Equal(t, []permissions.BoundaryKind{permissions.BoundaryKindHub}, sel.AllowedBoundaries)
		require.NoError(t, validatePermissionIDs([]string{tc.id}))
	}

	pickerScopes := make([]string, 0)
	for _, opt := range permissions.UATScopeOptions(false) {
		pickerScopes = append(pickerScopes, opt.UATScope)
	}
	assert.Contains(t, pickerScopes, "skill:update_global")
	assert.Contains(t, pickerScopes, "skill:delete_global")
}

// TestGlobalSkillCapabilities_CatalogAuthorVsMember verifies that
// _capabilities.actions on a global skill record reports ["read", "update",
// "delete"] (evaluated from skill.update_global / skill.delete_global without
// leaking "*_global" action names) for a global-catalog-author session and hub
// UAT, reports only ["read"] for an ordinary hub member, and does NOT grant
// update/delete capabilities on a project skill to a global-catalog-author.
func TestGlobalSkillCapabilities_CatalogAuthorVsMember(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	now := time.Now()

	globalSkill := &store.Skill{
		ID: tid("caps-global-skill"), Name: "caps-global", Slug: "caps-global",
		Scope: store.SkillScopeGlobal, OwnerID: bob.ID, CreatedBy: bob.ID, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, globalSkill))

	projSkill := &store.Skill{
		ID: tid("caps-project-skill"), Name: "caps-project", Slug: "caps-project",
		Scope: store.SkillScopeProject, ScopeID: project.ID, OwnerID: alice.ID, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, projSkill))

	// Give Charlie project-member + global-catalog-author.
	charlie := createNamedTestUser(t, s, "caps-charlie", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	createTestUserWithProjectRole(t, s, charlie.ID, charlie.Email, project.ID, store.ProjectRoleMember)
	bindGlobalCatalogAuthor(t, s, charlie.ID)

	type skillWithCaps struct {
		ID           string        `json:"id"`
		Capabilities *Capabilities `json:"_capabilities"`
	}
	getCaps := func(rec interface{ Bytes() []byte }) []string {
		var out skillWithCaps
		require.NoError(t, json.Unmarshal(rec.Bytes(), &out))
		require.NotNil(t, out.Capabilities)
		return out.Capabilities.Actions
	}

	// Bob is even stamped as OwnerID on globalSkill, yet holds only hub-member -> ["read"] only.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills/"+globalSkill.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"read"}, getCaps(rec.Body))

	// Charlie holds global-catalog-author -> ["read", "update", "delete"] on globalSkill...
	rec = doRequestAsUser(t, srv, charlie, http.MethodGet, "/api/v1/skills/"+globalSkill.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.ElementsMatch(t, []string{"read", "update", "delete"}, getCaps(rec.Body))

	// ...and only ["read"] on projSkill (where Charlie is project-member).
	rec = doRequestAsUser(t, srv, charlie, http.MethodGet, "/api/v1/skills/"+projSkill.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	charlieProjCaps := getCaps(rec.Body)
	assert.Equal(t, []string{"read"}, charlieProjCaps)
	assert.NotContains(t, charlieProjCaps, "update_global")
	assert.NotContains(t, charlieProjCaps, "delete_global")

	// Alice is project-owner on projSkill AND holds both global-catalog-author
	// and hub-admin at system scope: projSkill must emit only ["read",
	// "update", "delete"] and never emit "update_global" or "delete_global".
	bindGlobalCatalogAuthor(t, s, alice.ID)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      alice.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+projSkill.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	aliceProjCaps := getCaps(rec.Body)
	assert.ElementsMatch(t, []string{"read", "update", "delete"}, aliceProjCaps)
	assert.NotContains(t, aliceProjCaps, "update_global")
	assert.NotContains(t, aliceProjCaps, "delete_global")

	// Scoped hub UAT with skill:read + skill:update_global -> ["read", "update"] (no delete).
	charlieToken := mintHubConfigToken(t, srv, charlie.ID, hubBoundary(), "skill:read", "skill:update_global")
	rec = callWithKey(t, srv, charlieToken, http.MethodGet, "/api/v1/skills/"+globalSkill.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.ElementsMatch(t, []string{"read", "update"}, getCaps(rec.Body))
}

// TestExplainAPI_GlobalSkillWriteUsesTwin pins that explain without an
// explicit permission resolves update/delete on a global skill to the
// *_global twin, matching enforcement: a custom system role holding only
// plain skill.update is denied, a global-catalog-author is allowed.
func TestExplainAPI_GlobalSkillWriteUsesTwin(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	now := time.Now()

	globalSkill := &store.Skill{
		ID: tid("explain-global-skill"), Name: "explain-global", Slug: "explain-global",
		Scope: store.SkillScopeGlobal, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, globalSkill))
	projectSkill := &store.Skill{
		ID: tid("explain-project-skill"), Name: "explain-project", Slug: "explain-project",
		Scope: store.SkillScopeProject, ScopeID: project.ID, Status: "active", Created: now, Updated: now,
	}
	require.NoError(t, s.CreateSkill(ctx, projectSkill))

	charlie := createNamedTestUser(t, s, "explain-plain-skill-user", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	customRole := &store.RoleDefinition{
		ID:          tid("explain-plain-skill-rw"),
		Name:        "explain-plain-skill-rw",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"skill.update", "skill.delete"},
	}
	_, err := s.CreateRoleDefinition(ctx, customRole)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      charlie.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        alice.ID,
	})
	require.NoError(t, err)

	ensureHubMembership(ctx, s, bob.ID)
	bindGlobalCatalogAuthor(t, s, bob.ID)

	explain := func(t *testing.T, u *store.User, skillID, action string) explainResponse {
		t.Helper()
		body, err := json.Marshal(map[string]interface{}{
			"resource": map[string]interface{}{"type": "skill", "id": skillID},
			"action":   action,
		})
		require.NoError(t, err)
		identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "api")
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", body, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	for _, action := range []string{"update", "delete"} {
		t.Run(action, func(t *testing.T) {
			denied := explain(t, charlie, globalSkill.ID, action)
			assert.False(t, denied.Allowed, "plain skill.%s must not allow a global skill write", action)
			require.NotNil(t, denied.Provenance)
			assert.Equal(t, "skill."+action+"_global", denied.Provenance.Permission)

			allowed := explain(t, bob, globalSkill.ID, action)
			assert.True(t, allowed.Allowed, "global-catalog-author must be allowed to %s a global skill", action)
			require.NotNil(t, allowed.Provenance)
			assert.Equal(t, "skill."+action+"_global", allowed.Provenance.Permission)

			// Project skills keep the plain permission.
			plain := explain(t, charlie, projectSkill.ID, action)
			require.NotNil(t, plain.Provenance)
			assert.Equal(t, "skill."+action, plain.Provenance.Permission)
		})
	}
}
