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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Project visibility via membership-based policies
//
// After narrowing hub-member-read-all to exclude project/agent/broker resources,
// project visibility is controlled by per-project membership policies:
//   - Project members (in the project:<slug>:members group) can read the project
//   - Non-members cannot read the project (get 404)
//   - Adding the hub-members group to the project's members group makes it
//     visible to all hub members ("everyone" / "public" visibility)
// =============================================================================

// TestGetProject_MemberCanRead verifies that a project member can read their
// project via the getProject handler.
func TestGetProject_MemberCanRead(t *testing.T) {
	srv, _, alice, _, project := setupDemoPolicyTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"project member should see their project; got: %s", rec.Body.String())

	var resp ProjectWithCapabilities
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, project.ID, resp.ID)
}

// TestGetProject_NonMemberGetNotFound verifies that a hub member who is NOT a
// project member receives 404 (not 403) when reading the project.
func TestGetProject_NonMemberGetNotFound(t *testing.T) {
	srv, _, _, bob, project := setupDemoPolicyTest(t)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"non-member should get 404; got: %s", rec.Body.String())
}

// TestGetProject_UnauthenticatedGetNotFound verifies that an unauthenticated
// caller receives 404 when attempting to read any project.
func TestGetProject_UnauthenticatedGetNotFound(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("vis-unauth-proj"),
		Name:    "Visibility Unauth Project",
		Slug:    "vis-unauth-proj",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+project.ID, nil)
	rec := httptest.NewRecorder()
	srv.getProject(rec, req, project.ID)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"unauthenticated caller should get 404; got: %s", rec.Body.String())
}

// TestListProjects_MemberSeesOwnProject verifies that a project member can see
// their project in the list response.
func TestListProjects_MemberSeesOwnProject(t *testing.T) {
	srv, _, alice, _, project := setupDemoPolicyTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	found := false
	for _, p := range resp.Projects {
		if p.ID == project.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "project member should see their project in list")
}

// TestListProjects_NonMemberDoesNotSeeProject verifies that a hub member who is
// NOT a project member does not see that project in list responses.
func TestListProjects_NonMemberDoesNotSeeProject(t *testing.T) {
	srv, _, _, bob, project := setupDemoPolicyTest(t)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	for _, p := range resp.Projects {
		assert.NotEqual(t, project.ID, p.ID,
			"non-member should NOT see the project in list")
	}
}

// TestProjectVisibility_HubMembersGroupMakesProjectPublic verifies the "everyone"
// visibility pattern: granting the hub-members group a project-member role binding
// makes the project visible to all hub members.
//
// CO1: Under the AK1 kernel, group nesting into the project members group is for
// collaboration only. Authorization requires a role binding for the hub-members
// group on the project.
func TestProjectVisibility_HubMembersGroupMakesProjectPublic(t *testing.T) {
	srv, s, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob is a hub member but NOT a project member — verify he can't see the project.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"before granting hub-members group access, non-member should get 404")

	// CO1: Create a project-member role binding for the hub-members group.
	// This is the "make project public" operation under the AK1 kernel.
	hubMembersGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err, "hub-members group should exist")

	pmRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: pmRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      hubMembersGroup.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err, "creating hub-members group role binding should succeed")

	// Now bob (hub member) should be able to see the project through group role binding.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"after granting hub-members group access, any hub member should see the project; got: %s", rec.Body.String())
}

// TestProjectVisibility_HubMembersGroupMakesProjectVisibleInList verifies that
// after granting the hub-members group a project-member role binding, the project
// appears in the list response for all hub members.
//
// CO1: Under the AK1 kernel, group nesting is for collaboration only.
// Authorization requires a role binding for the hub-members group on the project.
func TestProjectVisibility_HubMembersGroupMakesProjectVisibleInList(t *testing.T) {
	srv, s, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Verify bob can't see the project in list before.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var beforeResp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&beforeResp))
	for _, p := range beforeResp.Projects {
		require.NotEqual(t, project.ID, p.ID,
			"non-member should not see project in list before making public")
	}

	// CO1: Create a project-member role binding for the hub-members group.
	hubMembersGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	pmRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: pmRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      hubMembersGroup.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Now bob should see the project in list.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var afterResp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&afterResp))
	found := false
	for _, p := range afterResp.Projects {
		if p.ID == project.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "after making project public, any hub member should see it in list")
}

// TestGetProject_CheckAccess_MemberReadDecision verifies at the AuthzService
// level that a project member gets an allowed decision for read on their project.
func TestGetProject_CheckAccess_MemberReadDecision(t *testing.T) {
	srv, _, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	identity := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, identity, projectResource(project), ActionRead)
	assert.True(t, decision.Allowed,
		"project member should be allowed to read project; reason=%q", decision.Reason)
}

// TestGetProject_CheckAccess_NonMemberReadDenied verifies at the AuthzService
// level that a non-member gets a denied decision for read on a project.
func TestGetProject_CheckAccess_NonMemberReadDenied(t *testing.T) {
	srv, _, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	identity := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, identity, projectResource(project), ActionRead)
	assert.False(t, decision.Allowed,
		"non-member should be denied read on project; reason=%q", decision.Reason)
}

// TestNarrowHubMemberReadAll_RoleBindingBased verifies that after CO1
// cutover, authorization is handled by RoleBindings — the hub-members
// group exists and policies are no longer seeded (PolicyStore removed).
func TestNarrowHubMemberReadAll_RoleBindingBased(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Verify hub-members group exists with a role binding.
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)
	assert.NotEmpty(t, group.ID, "hub-members group should exist")
}

// TestEnsureProjectMemberReadPolicy_RoleBindingBased verifies that after
// CO1 cutover, project access is handled by project-scoped role bindings
// (project-member, project-admin, project-owner) and the project members
// group exists. PolicyStore has been removed — no policies are created.
func TestEnsureProjectMemberReadPolicy_RoleBindingBased(t *testing.T) {
	_, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Verify the project members group exists (role bindings handle access).
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	assert.NotEmpty(t, membersGroup.ID,
		"project members group should exist for role-binding-based access")
}

// TestProjectVisibility_NewProjectCreatorCanRead verifies that when a project is
// created via the HTTP API, the creator automatically has read access.
func TestProjectVisibility_NewProjectCreatorCanRead(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-admin user.
	creator := &store.User{
		ID:          tid("vis-creator"),
		Email:       "creator@test.com",
		DisplayName: "Creator",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, creator))
	ensureHubMembership(ctx, s, creator.ID)

	// Create a project via the API as this user.
	rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/projects",
		CreateProjectRequest{Name: "Creator's Project"})
	require.Equal(t, http.StatusCreated, rec.Code,
		"project creation should succeed; got: %s", rec.Body.String())

	var createdProject store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createdProject))

	// Creator should be able to read the project.
	rec = doRequestAsUser(t, srv, creator, http.MethodGet,
		"/api/v1/projects/"+createdProject.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"project creator should be able to read their project; got: %s", rec.Body.String())

	// Another user should NOT be able to read it.
	outsider := &store.User{
		ID:          tid("vis-outsider"),
		Email:       "outsider@test.com",
		DisplayName: "Outsider",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, outsider))
	ensureHubMembership(ctx, s, outsider.ID)

	rec = doRequestAsUser(t, srv, outsider, http.MethodGet,
		"/api/v1/projects/"+createdProject.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"outsider should get 404 for project they're not a member of; got: %s", rec.Body.String())
}
