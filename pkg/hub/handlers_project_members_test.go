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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupProjectMembersTest creates a test server with two users and a project
// where "owner" has a project-owner role binding. "other" is a hub member
// with no project role.
func setupProjectMembersTest(t *testing.T) (srv *Server, st store.Store, owner *store.User, other *store.User, project *store.Project) {
	t.Helper()
	srv, st = testServer(t)
	ctx := context.Background()

	owner = &store.User{
		ID:          tid("pm-owner"),
		Email:       "owner@test.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, owner))

	other = &store.User{
		ID:          tid("pm-other"),
		Email:       "other@test.com",
		DisplayName: "Other",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, other))

	// Ensure both are hub members (get system-scoped bindings).
	ensureHubMembership(ctx, st, owner.ID)
	ensureHubMembership(ctx, st, other.ID)

	// Create project.
	project = &store.Project{
		ID:        tid("pm-project"),
		Name:      "Members Test Project",
		Slug:      "pm-test-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, project))

	// Create project-owner role binding for "owner" (simulates project creation).
	srv.createProjectMembersGroup(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, owner.ID))

	return srv, st, owner, other, project
}

// ---------------------------------------------------------------------------
// GET /api/v1/projects/{id}/members
// ---------------------------------------------------------------------------

func TestProjectMembers_List_ReturnsProjectScopedBindings(t *testing.T) {
	srv, _, owner, _, project := setupProjectMembersTest(t)

	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp listProjectMembersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// Should contain at least the owner binding.
	assert.GreaterOrEqual(t, resp.TotalCount, 1)

	// Find the owner binding and verify enrichment.
	found := false
	for _, m := range resp.Items {
		if m.PrincipalID == owner.ID && m.RoleName == store.ProjectRoleOwner {
			found = true
			assert.Equal(t, "direct", m.Source)
			assert.Equal(t, store.ProjectRoleOwner, m.RoleName, "roleName should be enriched")
			assert.NotEmpty(t, m.PrincipalDisplayName)
			assert.Equal(t, store.RoleScopeProject, m.ScopeType)
			assert.Equal(t, project.ID, m.ScopeID)
		}
	}
	assert.True(t, found, "owner binding should be in results with enriched roleName")
}

func TestProjectMembers_List_NonMemberDenied(t *testing.T) {
	srv, _, _, other, project := setupProjectMembersTest(t)

	rec := doRequestAsUser(t, srv, other, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/members", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-project-member should be denied access to project members list")
}

// ---------------------------------------------------------------------------
// POST /api/v1/projects/{id}/members
// ---------------------------------------------------------------------------

func TestProjectMembers_Add_OwnerCanAddMember(t *testing.T) {
	srv, st, owner, other, project := setupProjectMembersTest(t)

	// Look up the project-member role.
	memberRoleDef, err := st.GetRoleDefinitionByName(context.Background(), store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      other.ID,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var info projectMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
	assert.Equal(t, store.ProjectRoleMember, info.RoleName)
	assert.Equal(t, "direct", info.Source)
	assert.Equal(t, other.ID, info.PrincipalID)
}

func TestProjectMembers_Add_NonMemberDenied(t *testing.T) {
	srv, st, _, other, project := setupProjectMembersTest(t)

	memberRoleDef, err := st.GetRoleDefinitionByName(context.Background(), store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, other, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      other.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-project-member should not be able to add members")
}

func TestProjectMembers_Add_EscalationPrevented(t *testing.T) {
	srv, st, owner, other, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Give "other" a project-admin role binding.
	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      other.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// project-admin tries to mint a project-owner — should be denied.
	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	// Create a third user to add.
	third := &store.User{
		ID:          tid("pm-third"),
		Email:       "third@test.com",
		DisplayName: "Third",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, third))
	ensureHubMembership(ctx, st, third.ID)

	rec := doRequestAsUser(t, srv, other, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: ownerRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      third.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project-admin should not be able to mint project-owner (escalation)")
}

func TestProjectMembers_Add_RejectsNonProjectRole(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)

	// Try to assign a system-scoped role.
	systemRoles, _ := st.ListRoleDefinitions(context.Background())
	var systemRoleID string
	for _, rd := range systemRoles {
		if rd.ScopeType == store.RoleScopeSystem && rd.Name == store.SystemRoleHubMember {
			systemRoleID = rd.ID
			break
		}
	}
	require.NotEmpty(t, systemRoleID)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: systemRoleID,
			PrincipalType:    "user",
			PrincipalID:      owner.ID,
		})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"should reject non-project-scoped role")
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/projects/{id}/members/{bindingID}
// ---------------------------------------------------------------------------

func TestProjectMembers_ChangeRole_Works(t *testing.T) {
	srv, st, owner, other, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Add "other" as project-member.
	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      other.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// Change to project-admin.
	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, owner, http.MethodPatch,
		"/api/v1/projects/"+project.ID+"/members/"+binding.ID,
		updateProjectMemberRequest{
			RoleDefinitionID: adminRoleDef.ID,
		})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var info projectMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
	assert.Equal(t, store.ProjectRoleAdmin, info.RoleName)
	assert.Equal(t, other.ID, info.PrincipalID)
}

func TestProjectMembers_ChangeRole_LastOwnerGuard(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Find the owner binding.
	bindings, err := st.ListRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID)
	require.NoError(t, err)

	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	var ownerBinding *store.RoleBinding
	for _, b := range bindings {
		if b.PrincipalID == owner.ID && b.RoleDefinitionID == ownerRoleDef.ID {
			ownerBinding = b
			break
		}
	}
	require.NotNil(t, ownerBinding, "owner binding should exist")

	// Try to change last owner to project-member — should be denied.
	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, owner, http.MethodPatch,
		"/api/v1/projects/"+project.ID+"/members/"+ownerBinding.ID,
		updateProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
		})
	assert.Equal(t, http.StatusConflict, rec.Code, "should get 409 LAST_OWNER")

	// Verify the error code.
	var errResp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	if errObj, ok := errResp["error"].(map[string]interface{}); ok {
		assert.Equal(t, "LAST_OWNER", errObj["code"])
	}
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/projects/{id}/members/{bindingID}
// ---------------------------------------------------------------------------

func TestProjectMembers_Remove_Works(t *testing.T) {
	srv, st, owner, other, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Add "other" as project-member.
	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      other.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+binding.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestProjectMembers_Remove_NonMemberDenied(t *testing.T) {
	srv, st, owner, other, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Add "other" as project-member first, then try to have "other" remove
	// the owner — "other" doesn't have project.manage.
	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      other.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// Find the owner binding.
	bindings, err := st.ListRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID)
	require.NoError(t, err)

	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	var ownerBindingID string
	for _, b := range bindings {
		if b.PrincipalID == owner.ID && b.RoleDefinitionID == ownerRoleDef.ID {
			ownerBindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, ownerBindingID)

	rec := doRequestAsUser(t, srv, other, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+ownerBindingID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project-member should not be able to remove other members")
}

func TestProjectMembers_Remove_LastOwnerGuard(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Find the owner binding.
	bindings, err := st.ListRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID)
	require.NoError(t, err)

	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	var ownerBindingID string
	for _, b := range bindings {
		if b.PrincipalID == owner.ID && b.RoleDefinitionID == ownerRoleDef.ID {
			ownerBindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, ownerBindingID)

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+ownerBindingID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "should get 409 LAST_OWNER")

	var errResp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	if errObj, ok := errResp["error"].(map[string]interface{}); ok {
		assert.Equal(t, "LAST_OWNER", errObj["code"])
	}
}

// ---------------------------------------------------------------------------
// S1 regression: hub member cannot enumerate all role bindings
// ---------------------------------------------------------------------------

func TestProjectMembers_S1_HubMemberCannotListAllBindings(t *testing.T) {
	srv, _, _, other, _ := setupProjectMembersTest(t)

	// "other" is a hub-member but has no project role.
	// After S1 fix, hub-member no longer has role_binding.read,
	// so GET /api/v1/admin/role-bindings should be denied.
	rec := doRequestAsUser(t, srv, other, http.MethodGet,
		"/api/v1/admin/role-bindings", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"S1: hub member should NOT be able to enumerate all role bindings hub-wide")
}
