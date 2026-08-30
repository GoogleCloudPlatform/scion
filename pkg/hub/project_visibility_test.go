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
// visibility pattern: adding the hub-members group to a project's members group
// makes the project visible to all hub members via transitive group expansion.
func TestProjectVisibility_HubMembersGroupMakesProjectPublic(t *testing.T) {
	srv, s, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob is a hub member but NOT a project member — verify he can't see the project.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"before adding hub-members group, non-member should get 404")

	// Add the hub-members group to the project's members group (nested group).
	// This is the "make project public" operation.
	hubMembersGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err, "hub-members group should exist")

	projectMembersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err, "project members group should exist")

	err = s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    projectMembersGroup.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   hubMembersGroup.ID,
		Role:       store.GroupMemberRoleMember,
	})
	require.NoError(t, err, "adding hub-members group to project members should succeed")

	// Now bob (hub member) should be able to see the project through transitive membership.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"after adding hub-members group, any hub member should see the project; got: %s", rec.Body.String())
}

// TestProjectVisibility_HubMembersGroupMakesProjectVisibleInList verifies that
// after adding hub-members to the project members group, the project appears in
// the list response for all hub members.
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

	// Make the project public by adding hub-members to project members.
	hubMembersGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)
	projectMembersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    projectMembersGroup.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   hubMembersGroup.ID,
		Role:       store.GroupMemberRoleMember,
	}))

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

// TestNarrowHubMemberReadAll_DeletesWildcardPolicy verifies that the
// narrowHubMemberReadAll function deletes a wildcard hub-member-read-all policy
// and that per-type policies are seeded for directory resources only.
func TestNarrowHubMemberReadAll_DeletesWildcardPolicy(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// After testServer/seedDefaultPoliciesAndGroups, the wildcard policy should
	// be gone and per-type policies should exist.
	wildcardPolicies, err := s.ListPolicies(ctx, store.PolicyFilter{
		Name:      "hub-member-read-all",
		ScopeType: "hub",
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, wildcardPolicies.Items,
		"wildcard hub-member-read-all should be deleted after narrowing")

	// Per-type policies for globally readable resources should exist.
	for _, rt := range []string{"user", "group", "template", "harness_config", "broker", "runtime_broker", "gcp_service_account", "policy", "skill", "quota", "role", "role_binding", "hub"} {
		policies, err := s.ListPolicies(ctx, store.PolicyFilter{
			Name:      "hub-member-read-" + rt,
			ScopeType: "hub",
		}, store.ListOptions{Limit: 1})
		require.NoError(t, err)
		require.NotEmpty(t, policies.Items,
			"hub-member-read-%s policy should be seeded", rt)
		assert.Equal(t, rt, policies.Items[0].ResourceType,
			"policy resource type should be %s", rt)
		assert.Equal(t, []string{"read", "list"}, policies.Items[0].Actions)
	}

	// No per-type policy should exist for project or agent
	// (those are now gated per-project by membership policies).
	for _, rt := range []string{"project", "agent"} {
		policies, err := s.ListPolicies(ctx, store.PolicyFilter{
			Name:      "hub-member-read-" + rt,
			ScopeType: "hub",
		}, store.ListOptions{Limit: 1})
		require.NoError(t, err)
		assert.Empty(t, policies.Items,
			"hub-member-read-%s should NOT exist as a hub-scoped policy", rt)
	}
}

// TestEnsureProjectMemberReadPolicy_CreatesReadPolicies verifies that
// ensureProjectMemberReadPolicy creates read+list policies for project and agent
// resource types bound to the project's members group.
func TestEnsureProjectMemberReadPolicy_CreatesReadPolicies(t *testing.T) {
	_, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	for _, rt := range []string{"project", "agent"} {
		policyName := "project:" + project.Slug + ":member-read-" + rt
		policies, err := s.ListPolicies(ctx, store.PolicyFilter{
			Name: policyName,
		}, store.ListOptions{Limit: 1})
		require.NoError(t, err)
		require.NotEmpty(t, policies.Items,
			"member-read-%s policy should exist for project %s", rt, project.Slug)

		policy := policies.Items[0]
		assert.Equal(t, "project", policy.ScopeType)
		assert.Equal(t, project.ID, policy.ScopeID)
		assert.Equal(t, rt, policy.ResourceType)
		assert.Equal(t, []string{"read", "list"}, policy.Actions)
		assert.Equal(t, "allow", policy.Effect)

		// Verify binding to members group
		membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
		require.NoError(t, err)
		bindings, err := s.GetPolicyBindings(ctx, policy.ID)
		require.NoError(t, err)
		found := false
		for _, b := range bindings {
			if b.PrincipalType == "group" && b.PrincipalID == membersGroup.ID {
				found = true
				break
			}
		}
		assert.True(t, found,
			"member-read-%s policy should be bound to members group", rt)
	}
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
