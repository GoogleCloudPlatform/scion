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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupSkillAuthzTest creates a test server with two users and a project.
// Alice is a hub member and project owner. Bob is NOT a hub member, so
// the seeded hub-member-read-all policy does not grant him read access.
func setupSkillAuthzTest(t *testing.T) (srv *Server, s store.Store, alice, bob *store.User, project *store.Project) {
	t.Helper()

	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{
		ID:          tid("skill-alice"),
		Email:       "skill-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob = &store.User{
		ID:          tid("skill-bob"),
		Email:       "skill-bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	ensureHubMembership(ctx, s, alice.ID)
	// Bob is intentionally NOT added to hub-members, so default-deny applies.

	project = &store.Project{
		ID:        tid("skill-project"),
		Name:      "Skill Project",
		Slug:      "skill-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	return srv, s, alice, bob, project
}

// createTestSkill is a helper that inserts a skill directly into the store.
func createTestSkill(t *testing.T, s store.Store, name, scope, scopeID, ownerID string) *store.Skill {
	t.Helper()
	skill := &store.Skill{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        api.Slugify(name),
		Scope:       scope,
		ScopeID:     scopeID,
		OwnerID:     ownerID,
		Status:      "active",
		Visibility:  store.VisibilityPrivate,
		StoragePath: fmt.Sprintf("skills/%s/%s", scope, api.Slugify(name)),
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateSkill(context.Background(), skill))
	return skill
}

// ============================================================================
// H1: getSkill ActionRead tests
// ============================================================================

func TestSkillAuthz_GetSkill_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "alice-skill", store.SkillScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "owner should be able to read own skill")
}

func TestSkillAuthz_GetSkill_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "alice-private", store.SkillScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"non-member should get 404 (not 200) to avoid leaking existence; got: %s", rec.Body.String())
}

func TestSkillAuthz_GetSkill_HubMemberAllowed(t *testing.T) {
	srv, s, alice, _, _ := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "global-skill", store.SkillScopeGlobal, "", alice.ID)

	// Hub members have read access via hub-member-read-all policy.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills/"+skill.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"hub member should be able to read global skill; got: %s", rec.Body.String())
}

// ============================================================================
// H1: listSkillVersions / getSkillVersion ActionRead tests
// ============================================================================

func TestSkillAuthz_ListSkillVersions_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "versioned-skill", store.SkillScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills/"+skill.ID+"/versions", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"non-member should not be able to list versions; got: %s", rec.Body.String())
}

func TestSkillAuthz_GetSkillVersion_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "ver-check-skill", store.SkillScopeProject, project.ID, alice.ID)

	sv := &store.SkillVersion{
		ID:      api.NewUUID(),
		SkillID: skill.ID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusPublished,
		Created: time.Now(),
	}
	require.NoError(t, s.CreateSkillVersion(context.Background(), sv))

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills/"+skill.ID+"/versions/"+sv.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"non-member should not be able to get version; got: %s", rec.Body.String())
}

// ============================================================================
// H1: listSkills ActionRead filtering tests
// ============================================================================

func TestSkillAuthz_ListSkills_FiltersUnreadable(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)

	createTestSkill(t, s, "visible-to-alice", store.SkillScopeProject, project.ID, alice.ID)
	createTestSkill(t, s, "also-visible", store.SkillScopeProject, project.ID, alice.ID)

	// Alice (project member) should see skills.
	recAlice := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/skills?scope=project&scopeId="+project.ID, nil)
	assert.Equal(t, http.StatusOK, recAlice.Code)

	var aliceResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recAlice.Body).Decode(&aliceResp))

	// Bob (non-member) should have skills filtered out.
	recBob := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/skills?scope=project&scopeId="+project.ID, nil)
	assert.Equal(t, http.StatusOK, recBob.Code)

	var bobResp ListSkillsResponse
	require.NoError(t, json.NewDecoder(recBob.Body).Decode(&bobResp))

	assert.Greater(t, len(aliceResp.Skills), 0, "alice should see project skills")
	assert.Less(t, len(bobResp.Skills), len(aliceResp.Skills),
		"bob should see fewer skills than alice")
}

// ============================================================================
// H1: handleSkillsResolve ActionRead tests
// ============================================================================

func TestSkillAuthz_Resolve_ForbiddenSkill(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	skill := createTestSkill(t, s, "secret-skill", store.SkillScopeProject, project.ID, alice.ID)

	// Create a published version so resolve can find it.
	sv := &store.SkillVersion{
		ID:      api.NewUUID(),
		SkillID: skill.ID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusPublished,
		Created: time.Now(),
	}
	require.NoError(t, s.CreateSkillVersion(context.Background(), sv))

	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills:    []ResolveSkillRef{{URI: "skill://project/secret-skill"}},
		ProjectID: project.ID,
	})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.Empty(t, resp.Resolved, "forbidden skill should not be in resolved list")
	require.NotEmpty(t, resp.Errors, "forbidden skill should produce an error")
	assert.Equal(t, "forbidden", resp.Errors[0].Code)
}

// ============================================================================
// H2: createSkill user scope tests
// ============================================================================

func TestSkillAuthz_CreateSkill_UserScope_EnforcesScopeID(t *testing.T) {
	srv, _, alice, _, _ := setupSkillAuthzTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:    "my-user-skill",
		Scope:   "user",
		ScopeID: "arbitrary-id-that-should-be-ignored",
	})
	// Should succeed, but scopeId should be the authenticated user's ID.
	assert.Equal(t, http.StatusCreated, rec.Code, "user scope create should succeed; got: %s", rec.Body.String())

	var resp CreateSkillResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, alice.ID, resp.Skill.ScopeID,
		"scopeId should be the authenticated user's ID, not the client-supplied value")
}

func TestSkillAuthz_CreateSkill_UserScope_UnauthenticatedRejected(t *testing.T) {
	srv, _, _, _, _ := setupSkillAuthzTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/skills", CreateSkillRequest{
		Name:  "anon-skill",
		Scope: "user",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated user-scope create should be rejected; got: %s", rec.Body.String())
}

// ============================================================================
// L1: Batch resolve item cap
// ============================================================================

func TestSkillAuthz_Resolve_TooManyItems(t *testing.T) {
	srv, _, _, _, _ := setupSkillAuthzTest(t)

	skills := make([]ResolveSkillRef, 51)
	for i := range skills {
		skills[i] = ResolveSkillRef{URI: fmt.Sprintf("skill://global/skill-%d", i)}
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills: skills,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"batch resolve with >50 items should return 400; got: %s", rec.Body.String())
}
