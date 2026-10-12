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
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1901 uat finding F7: three read surfaces (version-by-id,
// single-resolve, and direct file read) shared the skillResource+CheckAccess
// gate that the rest of skill_scope_authz_test.go pins per-surface, but had
// no test of their own. A mutation exercise confirmed all three are
// correctly denied today but would regress silently: killing the ScopeKind
// on any of them left every existing test green.
//
// Also here: a project-scoped GET .../versions test — the existing versions
// coverage (TestSkillScope_UserScoped_VersionsDeniedForOtherMember) only
// exercises the user-scope arm.
// ============================================================================

// TestSkillScope_UserScoped_VersionByIDDeniedForOtherMember covers uat F7 /
// mutation M6: GET /api/v1/skills/{id}/versions/{versionId}.
func TestSkillScope_UserScoped_VersionByIDDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-user-skill-version-by-id", store.SkillScopeUser, alice.ID, alice.ID)
	sv := createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, fmt.Sprintf("/api/v1/skills/%s/versions/%s", skill.ID, sv.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a hub member who is not the owner must not read a version of another user's user-scoped skill; got: %s", rec.Body.String())
}

// TestSkillScope_ProjectScoped_VersionByIDDeniedForOtherHubMember is the
// project-scope arm of the same surface.
func TestSkillScope_ProjectScoped_VersionByIDDeniedForOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-version-by-id", store.SkillScopeProject, project.ID, alice.ID)
	sv := createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, fmt.Sprintf("/api/v1/skills/%s/versions/%s", skill.ID, sv.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a non-member must not read a version of another project's project-scoped skill; got: %s", rec.Body.String())
}

// TestSkillScope_ProjectScoped_VersionsListDeniedForOtherHubMember is the
// project-scope arm of GET /api/v1/skills/{id}/versions — previously only
// covered for user scope.
func TestSkillScope_ProjectScoped_VersionsListDeniedForOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-versions-list", store.SkillScopeProject, project.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/versions", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"versions of another project's project-scoped skill must not be listable; got: %s", rec.Body.String())
}

// TestSkillScope_UserScoped_ResolveSingleDeniedForOtherMember covers uat F7 /
// mutation M8: GET /api/v1/skills/{id}/resolve. This is the single-skill
// resolve surface, distinct from the batch POST /api/v1/skills/resolve
// already covered by TestSkillScope_UserScoped_ResolveDeniedForOtherMember.
func TestSkillScope_UserScoped_ResolveSingleDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-user-skill-resolve-single", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/resolve?version=1.0.0", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"single-skill resolve of another user's user-scoped skill must not be allowed; got: %s", rec.Body.String())
}

// TestSkillScope_ProjectScoped_ResolveSingleDeniedForOtherHubMember is the
// project-scope arm of the same surface.
func TestSkillScope_ProjectScoped_ResolveSingleDeniedForOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-resolve-single", store.SkillScopeProject, project.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/resolve?version=1.0.0", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"single-skill resolve of another project's project-scoped skill must not be allowed; got: %s", rec.Body.String())
}

// TestSkillScope_UserScoped_FileReadDeniedForOtherMember covers uat F7 /
// mutation M10: GET /api/v1/skills/{id}/files/{path}. This is the most
// sensitive surface — it streams skill content — and previously had no
// dedicated regression test even though it is correctly denied today
// (skill_file_handlers.go authorizeSkillFileRead). No real file/storage is
// needed: an unauthorized caller is denied before the file lookup runs.
func TestSkillScope_UserScoped_FileReadDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-user-skill-file-read", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/files/SKILL.md?version=1.0.0", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"file read of another user's user-scoped skill must not be allowed; got: %s", rec.Body.String())
}

// TestSkillScope_ProjectScoped_FileReadDeniedForOtherHubMember is the
// project-scope arm of the same surface.
func TestSkillScope_ProjectScoped_FileReadDeniedForOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-file-read", store.SkillScopeProject, project.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/files/SKILL.md?version=1.0.0", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"file read of another project's project-scoped skill must not be allowed; got: %s", rec.Body.String())
}

// TestSkillScope_GroupDerivedHubMember_DeniedOnUserScopedSkill pins uat C4:
// a hub-member grant obtained transitively through group membership (not a
// direct role binding) must be filtered the same as a direct one.
func TestSkillScope_GroupDerivedHubMember_DeniedOnUserScopedSkill(t *testing.T) {
	srv, s, alice, _, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-user-skill-group-derived", store.SkillScopeUser, alice.ID, alice.ID)

	vgm := createNamedTestUser(t, s, "skillscope-groupderived", store.UserRoleMember)
	// ensureHubMembership adds the user to the hub-members group, which
	// carries the hub-member role binding — the exact "group-derived system
	// hub-member" shape uat C4 exercises live.
	ensureHubMembership(context.Background(), s, vgm.ID)

	rec := doRequestAsUser(t, srv, vgm, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a group-derived hub-member grant must not read another user's user-scoped skill; got: %s", rec.Body.String())
}

// TestCanUseProjectGitHubToken_NonMemberDenied is the regression test for
// uat finding F3: canUseProjectGitHubToken built a Resource{Type: "skill",
// ParentType: "project", ...} with no ScopeKind, so filterHubWideSkillGrants
// never narrowed the curated hub-member system-scoped skill.read grant for
// it — any hub member (not just project members) could make the Hub mint
// the target project's GitHub App installation token for gh:// skill
// resolution.
//
// canUseProjectGitHubToken itself is unexported and reads identity from
// context, so this drives it through its one production call site
// (handleSkillsResolve) instead of fabricating a context: a gh:// ref with
// ProjectID set short-circuits to a "forbidden" ResolveSkillError before any
// real GitHub call is attempted when the caller fails this check, and to a
// different error path (no GitHub App configured in tests) when it passes —
// so the presence/absence of "forbidden" is exactly this check's outcome.
func TestCanUseProjectGitHubToken_NonMemberDenied(t *testing.T) {
	srv, _, _, carol, project := setupSkillScopeTest(t)

	rec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: "gh://acme/private-repo/SKILL.md@main"}},
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.Errors, "a non-member's gh:// resolve must error")
	assert.Equal(t, "forbidden", resp.Errors[0].Code,
		"a hub member who is not a project member must not be able to have the Hub mint that project's GitHub token; got: %+v", resp.Errors[0])
}

// TestCanUseProjectGitHubToken_MemberAllowed is the positive control for the
// same fix: an actual project member must clear canUseProjectGitHubToken
// (no "forbidden" error), even though resolution then fails downstream
// since no GitHub App is configured in this test environment.
func TestCanUseProjectGitHubToken_MemberAllowed(t *testing.T) {
	srv, _, alice, _, project := setupSkillScopeTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: "gh://acme/private-repo/SKILL.md@main"}},
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.Errors, "resolution must still fail (no GitHub App configured in tests)")
	assert.NotEqual(t, "forbidden", resp.Errors[0].Code,
		"the project owner must clear the GitHub-token authorization check; got: %+v", resp.Errors[0])
}

// TestFilterHubWideSkillGrants_FailsClosedOnUnrecognizedScope is the store/
// authz-kernel-level regression for uat finding F4: filterHubWideSkillGrants
// must filter the curated hub-member/hub-viewer grant for any ScopeKind that
// is not explicitly "global" or "core" — including an empty string (the
// exact shape of an ad hoc Resource literal that forgot to set it, as F3
// was). Before the fix, only "user" and "project" were filtered, so an
// empty ScopeKind fell through unfiltered.
func TestFilterHubWideSkillGrants_FailsClosedOnUnrecognizedScope(t *testing.T) {
	hubMemberBinding := CandidateBinding{
		BindingID: "b1", RoleDefinitionID: "hub-member-rd",
		PrincipalType: "group", PrincipalID: "hub-members", ScopeType: ScopeTypeSystem,
	}
	roleDefs := map[string]*RolePermissions{
		"hub-member-rd": NewRolePermissions("hub-member-rd", store.SystemRoleHubMember, store.RoleScopeSystem, []string{"skill.read", "skill.list"}),
	}

	for _, scope := range []string{"", "user", "project", "team"} {
		t.Run("scope="+scope, func(t *testing.T) {
			out := filterHubWideSkillGrants([]CandidateBinding{hubMemberBinding}, roleDefs, scope)
			assert.Empty(t, out, "an ad hoc or non-hub scope %q must filter the curated hub-member grant", scope)
		})
	}

	for _, scope := range []string{store.SkillScopeGlobal, store.SkillScopeCore} {
		t.Run("scope="+scope, func(t *testing.T) {
			out := filterHubWideSkillGrants([]CandidateBinding{hubMemberBinding}, roleDefs, scope)
			require.Len(t, out, 1, "an explicit hub scope %q must keep the curated hub-member grant", scope)
		})
	}
}
