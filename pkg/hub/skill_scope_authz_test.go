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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1901: enforce the user/project skill read boundary.
//
// Before the fix, the built-in hub-member (and hub-viewer) role carried
// skill.read/skill.list at SYSTEM scope (seed.go hubMemberPermissionIDs /
// hubViewerPermissionIDs). The kernel's scope containment check
// (scopeApplies in authz_kernel.go) treats system scope as "applies to
// everything", so that grant let ANY hub member read ANY other user's
// user-scoped skill, and ANY other project's project-scoped skill —
// including detail, versions, files, list, resolve and signed download-URL
// issuance, since every one of those handlers gates on the same
// skillResource(skill) + CheckAccess(ActionRead) call (see skill_handlers.go,
// skill_file_handlers.go, skill_dispatch_resolve.go).
//
// The fix (filterHubWideSkillGrants, authz_skill_scope.go) removes that
// system-scoped grant from consideration when the target skill's own scope
// is "user" or "project" — unless the caller holds an elevated role
// (hub-admin, super-admin), which the ptone/scion#1901 ruling says must
// keep full visibility. This file is the table-driven regression suite the
// ruling asks for: (owner, other member, admin, project member) × (read,
// versions, list, resolve, download) for user- and project-scoped skills,
// plus a hub-scoped control and an agent-scope check.
// ============================================================================

// setupSkillScopeTest builds on setupSkillAuthzTest, adding carol: a hub
// member who is neither the resource owner nor a member of alice's project.
// Carol is the principal the pre-fix bug affected — unlike bob (not a hub
// member at all), carol's denial can only come from the scope boundary
// itself, not from missing hub membership.
func setupSkillScopeTest(t *testing.T) (srv *Server, s store.Store, alice, carol *store.User, project *store.Project) {
	t.Helper()
	srv, s, alice, _, project = setupSkillAuthzTest(t)
	carol = createNamedTestUser(t, s, "skillscope-carol", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, carol.ID)
	return srv, s, alice, carol, project
}

// createSkillScopeAdmin creates a user with an explicit hub-admin role
// binding. hub-admin (not just User.Role=="admin") is what actually grants
// authority under the AK1 kernel; see TestGlobalSkillCreate_HubAdminStillAllowed
// for the same pattern.
func createSkillScopeAdmin(t *testing.T, s store.Store, namePrefix string) *store.User {
	t.Helper()
	admin := createNamedTestUser(t, s, namePrefix, store.UserRoleMember)
	rd, err := s.GetRoleDefinitionByName(context.Background(), store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "hub-admin role should have been seeded")
	_, err = s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return admin
}

func createPublishedSkillVersion(t *testing.T, s store.Store, skillID string) *store.SkillVersion {
	t.Helper()
	sv := &store.SkillVersion{
		ID:      api.NewUUID(),
		SkillID: skillID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusPublished,
		Created: time.Now(),
	}
	require.NoError(t, s.CreateSkillVersion(context.Background(), sv))
	return sv
}

// ----------------------------------------------------------------------
// User-scoped skills: readable only by the owning user and hub admins.
// ----------------------------------------------------------------------

func TestSkillScope_UserScoped_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-user-skill", store.SkillScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "owner should read their own user-scoped skill; got: %s", rec.Body.String())
}

// TestSkillScope_UserScoped_OtherHubMemberDenied is the INFO-1 regression
// test: carol is a hub member (so the old hub-member-read-all-equivalent
// grant applied to her) but is not alice's user-scoped skill's owner. She
// must be denied, the same way a non-member is denied — 404, not 403, so
// existence is not leaked (matches TestSkillAuthz_GetSkill_NonMemberDenied's
// convention).
func TestSkillScope_UserScoped_OtherHubMemberDenied(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-private-user-skill", store.SkillScopeUser, alice.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a hub member who is not the owner must not read another user's user-scoped skill; got: %s", rec.Body.String())
}

func TestSkillScope_UserScoped_HubAdminAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-user-skill-admin", store.SkillScopeUser, alice.ID, alice.ID)
	admin := createSkillScopeAdmin(t, s, "skillscope-admin-user")

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"hub admins must retain read access to user-scoped skills per the #1901 ruling; got: %s", rec.Body.String())
}

func TestSkillScope_UserScoped_VersionsDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-user-skill-versions", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/versions", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"versions of another user's user-scoped skill must not be listable; got: %s", rec.Body.String())
}

func TestSkillScope_UserScoped_ListFiltersOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	createTestSkill(t, s, "alice-list-user-skill", store.SkillScopeUser, alice.ID, alice.ID)

	recAlice := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills?scope=user&scopeId="+alice.ID, nil)
	require.Equal(t, http.StatusOK, recAlice.Code)
	var aliceResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recAlice.Body).Decode(&aliceResp))
	assert.NotEmpty(t, aliceResp.Skills, "alice should see her own user-scoped skill")

	recCarol := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills?scope=user&scopeId="+alice.ID, nil)
	require.Equal(t, http.StatusOK, recCarol.Code)
	var carolResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recCarol.Body).Decode(&carolResp))
	assert.Empty(t, carolResp.Skills, "a hub member must not see another user's user-scoped skills in a list")
}

func TestSkillScope_UserScoped_ResolveDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-resolve-user-skill", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: "skill://scion/user/" + alice.ID + "/alice-resolve-user-skill"}},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Resolved, "another user's user-scoped skill must not resolve")
	require.NotEmpty(t, resp.Errors, "resolve of another user's skill should error")
	assert.Equal(t, "forbidden", resp.Errors[0].Code)
}

func TestSkillScope_UserScoped_ResolveAllowedForOwner(t *testing.T) {
	srv, s, alice, _, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-resolve-own-user-skill", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: "skill://scion/user/" + alice.ID + "/alice-resolve-own-user-skill"}},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Errors)
	require.NotEmpty(t, resp.Resolved, "owner must still resolve their own user-scoped skill")
}

// TestSkillScope_UserScoped_DownloadDeniedForOtherMember covers download-URL
// issuance: GET /api/v1/skills/{id}/download is where signed file URLs are
// minted (skill_handlers.go handleSkillDownload), gated by the same
// skillResource+CheckAccess(ActionRead) call as everything else here.
func TestSkillScope_UserScoped_DownloadDeniedForOtherMember(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-download-user-skill", store.SkillScopeUser, alice.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/download?version=1.0.0", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"download-URL issuance for another user's user-scoped skill must be denied; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------
// Project-scoped skills: readable only by project members (and admins).
// ----------------------------------------------------------------------

func TestSkillScope_ProjectScoped_OtherHubMemberDenied(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-carol", store.SkillScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a hub member who is not a project member must not read the project's skill; got: %s", rec.Body.String())
}

func TestSkillScope_ProjectScoped_ProjectMemberAllowed(t *testing.T) {
	srv, s, alice, _, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-member", store.SkillScopeProject, project.ID, alice.ID)

	dave := createNamedTestUser(t, s, "skillscope-dave", store.UserRoleMember)
	ensureHubMembership(context.Background(), s, dave.ID)
	createTestUserWithProjectRole(t, s, dave.ID, dave.Email, project.ID, store.ProjectRoleMember)

	rec := doRequestAsUser(t, srv, dave, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"a project member must still read the project's skill after the scope fix; got: %s", rec.Body.String())
}

func TestSkillScope_ProjectScoped_HubAdminAllowed(t *testing.T) {
	srv, s, alice, _, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-admin", store.SkillScopeProject, project.ID, alice.ID)
	admin := createSkillScopeAdmin(t, s, "skillscope-admin-project")

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"hub admins must retain read access to project-scoped skills; got: %s", rec.Body.String())
}

func TestSkillScope_ProjectScoped_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-skill-owner", store.SkillScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the project owner should read the project's own skill; got: %s", rec.Body.String())
}

func TestSkillScope_ProjectScoped_ListFiltersOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	createTestSkill(t, s, "alice-project-list-skill", store.SkillScopeProject, project.ID, alice.ID)

	recAlice := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, recAlice.Code)
	var aliceResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recAlice.Body).Decode(&aliceResp))
	assert.NotEmpty(t, aliceResp.Skills)

	recCarol := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, recCarol.Code)
	var carolResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recCarol.Body).Decode(&carolResp))
	assert.Empty(t, carolResp.Skills, "a non-member hub member must not see the project's skills in a list")
}

func TestSkillScope_ProjectScoped_ResolveDeniedForOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-resolve-skill", store.SkillScopeProject, project.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: "skill://project/alice-project-resolve-skill"}},
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Resolved)
	require.NotEmpty(t, resp.Errors)
	assert.Equal(t, "forbidden", resp.Errors[0].Code)
}

func TestSkillScope_ProjectScoped_DownloadDeniedForOtherHubMember(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "alice-project-download-skill", store.SkillScopeProject, project.ID, alice.ID)
	createPublishedSkillVersion(t, s, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/download?version=1.0.0", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"download-URL issuance for another project's skill must be denied; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------
// Hub-scoped (global/core) skills: still readable by every hub member.
// This is the control that proves the fix narrows, rather than removes,
// the hub-member/hub-viewer grant.
// ----------------------------------------------------------------------

func TestSkillScope_HubScoped_OtherHubMemberStillAllowed(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "global-catalog-skill", store.SkillScopeGlobal, "", alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"every hub member must still read hub-scoped (global) skills; got: %s", rec.Body.String())
}

func TestSkillScope_CoreScoped_OtherHubMemberStillAllowed(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	skill := createTestSkill(t, s, "core-catalog-skill", store.SkillScopeCore, "", alice.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"every hub member must still read hub-scoped (core) skills; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------
// Public visibility is out of scope for #1901 and must be unaffected: every
// CheckAccess(ActionRead) call site for skills is guarded by
// `skill.Visibility != store.VisibilityPublic` (getSkill, listSkillVersions,
// getSkillVersion, listSkills, handleSkillDownload, authorizeSkillFileRead,
// skill_dispatch_resolve.go's resolve gate), so a public skill's read
// authorization never reaches filterHubWideSkillGrants at all. These tests
// pin that: a public user- or project-scoped skill must remain readable,
// listable, resolvable and downloadable by a hub member who is neither the
// owner nor a project member, exactly as before this change.
// ----------------------------------------------------------------------

func makeSkillPublic(t *testing.T, s store.Store, skill *store.Skill) {
	t.Helper()
	skill.Visibility = store.VisibilityPublic
	require.NoError(t, s.UpdateSkill(context.Background(), skill))
}

// publishOneFileVersion runs the real two-phase publish flow (create draft
// version, upload, finalize) as owner so the skill has a real, downloadable
// file in local storage — handleSkillDownload needs actual storage objects
// to generate signed URLs, not just a SkillVersion row.
func publishOneFileVersion(t *testing.T, srv *Server, owner *store.User, skillID string) {
	t.Helper()
	content := []byte("---\nname: test\n---\n# Test")
	uploadReq := FileUploadRequest{Path: "SKILL.md", Size: int64(len(content))}
	manifest := []store.TemplateFile{{Path: "SKILL.md", Size: int64(len(content)), Hash: sha256Hex(content)}}

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/skills/"+skillID+"/versions",
		PublishVersionRequest{Version: "1.0.0", Files: []FileUploadRequest{uploadReq}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var pub PublishVersionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&pub))
	require.Len(t, pub.UploadURLs, 1)

	rec = doRawRequestAsUser(t, srv, owner, pub.UploadURLs[0].Method, pub.UploadURLs[0].URL, content)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/skills/"+skillID+"/finalize",
		FinalizeSkillVersionRequest{Version: "1.0.0", Manifest: &SkillManifest{Files: manifest}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestSkillScope_PublicUserScopedSkill_OtherHubMemberStillAllowed(t *testing.T) {
	srv, s, alice, carol, _ := setupSkillScopeTest(t)
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(stor)

	skill := createTestSkill(t, s, "alice-public-user-skill", store.SkillScopeUser, alice.ID, alice.ID)
	makeSkillPublic(t, s, skill)
	publishOneFileVersion(t, srv, alice, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"a public user-scoped skill must remain readable by any hub member; got: %s", rec.Body.String())

	recList := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills?scope=user&scopeId="+alice.ID, nil)
	require.Equal(t, http.StatusOK, recList.Code)
	var listResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recList.Body).Decode(&listResp))
	assert.NotEmpty(t, listResp.Skills, "a public user-scoped skill must still appear in another member's list")

	recResolve := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: "skill://scion/user/" + alice.ID + "/alice-public-user-skill"}},
	})
	require.Equal(t, http.StatusOK, recResolve.Code)
	var resolveResp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(recResolve.Body).Decode(&resolveResp))
	assert.Empty(t, resolveResp.Errors, "a public user-scoped skill must still resolve for another member")
	assert.NotEmpty(t, resolveResp.Resolved)

	recDownload := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/download?version=1.0.0", nil)
	assert.Equal(t, http.StatusOK, recDownload.Code,
		"a public user-scoped skill's download-URL issuance must still succeed for another member; got: %s", recDownload.Body.String())
}

func TestSkillScope_PublicProjectScopedSkill_OtherHubMemberStillAllowed(t *testing.T) {
	srv, s, alice, carol, project := setupSkillScopeTest(t)
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(stor)

	skill := createTestSkill(t, s, "alice-public-project-skill", store.SkillScopeProject, project.ID, alice.ID)
	makeSkillPublic(t, s, skill)
	publishOneFileVersion(t, srv, alice, skill.ID)

	rec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"a public project-scoped skill must remain readable by a non-member hub member; got: %s", rec.Body.String())

	recList := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills?scope=project&scopeId="+project.ID, nil)
	require.Equal(t, http.StatusOK, recList.Code)
	var listResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recList.Body).Decode(&listResp))
	assert.NotEmpty(t, listResp.Skills, "a public project-scoped skill must still appear in a non-member's list")

	recResolve := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: "skill://project/alice-public-project-skill"}},
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusOK, recResolve.Code)
	var resolveResp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(recResolve.Body).Decode(&resolveResp))
	assert.Empty(t, resolveResp.Errors, "a public project-scoped skill must still resolve for a non-member")
	assert.NotEmpty(t, resolveResp.Resolved)

	recDownload := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/skills/"+skill.ID+"/download?version=1.0.0", nil)
	assert.Equal(t, http.StatusOK, recDownload.Code,
		"a public project-scoped skill's download-URL issuance must still succeed for a non-member; got: %s", recDownload.Body.String())
}

// ----------------------------------------------------------------------
// Agent tokens: an agent reads with its creator's scope, not beyond it.
// An agent's own principal closure does not include the hub-members group
// (GetEffectiveGroupsForAgent only returns explicit memberships and the
// implicit project_agents group), so an agent with no project-scoped grant
// for the target project or skill must be denied exactly like an outside
// user would be — the scope fix must not accidentally widen agent access,
// and an agent must not gain more than its own project's scope grants it.
// ----------------------------------------------------------------------

func TestSkillScope_Agent_OutsideProjectDeniedOnUserScopedSkill(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	owner := createNamedTestUser(t, s, "skillscope-agent-owner", store.UserRoleMember)
	ensureHubMembership(ctx, s, owner.ID)
	skill := createTestSkill(t, s, "agent-outside-user-skill", store.SkillScopeUser, owner.ID, owner.ID)

	otherProject := &store.Project{ID: tid("skillscope-agent-project"), Name: "Agent Project", Slug: "skillscope-agent-project"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	agent := &store.Agent{
		ID: tid("skillscope-agent"), Slug: tid("skillscope-agent"), Name: "Scope Agent",
		ProjectID: otherProject.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	identity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: otherProject.ID,
	}}

	decision := authz.CheckAccess(ctx, identity, skillResource(skill), ActionRead)
	assert.False(t, decision.Allowed,
		"an agent outside the owner's scope must not read the owner's user-scoped skill")
}
