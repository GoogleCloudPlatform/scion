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

func TestProjectMembers_Add_RejectsDirectUserOnlyRoleForGroup(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create a group to assign the role to.
	testGroup := &store.Group{
		ID:        tid("pm-test-group"),
		Name:      "Test Group",
		Slug:      "pm-test-group",
		GroupType: store.GroupTypeExplicit,
		CreatedBy: owner.ID,
	}
	require.NoError(t, st.CreateGroup(ctx, testGroup))

	// Try to assign project-owner to the group — should return 400.
	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: ownerRoleDef.ID,
			PrincipalType:    "group",
			PrincipalID:      testGroup.ID,
		})
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"R2: assigning project-owner to a group should return 400, not 500")
	assert.Contains(t, rec.Body.String(), "direct users")
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
	assert.Equal(t, http.StatusConflict, rec.Code, "should get 409 last_owner")

	// Verify the error code.
	var errResp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	if errObj, ok := errResp["error"].(map[string]interface{}); ok {
		assert.Equal(t, "last_owner", errObj["code"])
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
	assert.Equal(t, http.StatusConflict, rec.Code, "should get 409 last_owner")

	var errResp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	if errObj, ok := errResp["error"].(map[string]interface{}); ok {
		assert.Equal(t, "last_owner", errObj["code"])
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

// ===========================================================================
// C0 Exit-Gate Regression Tests
//
// These tests prove the C0 containment exit gate:
//   - A project-admin binding never classifies a project as Mine.
//   - A project admin cannot add another admin through any membership API path.
//   - A project admin cannot promote an owner.
//   - A project admin cannot demote/remove an admin.
//   - A project admin cannot remove an owner.
//   - Denial codes are stable product-level, not raw evaluator output.
//   - Direct API calls behave the same as the UI.
// ===========================================================================

// ---------------------------------------------------------------------------
// C0: Project admin cannot manage membership (owner-only containment)
// ---------------------------------------------------------------------------

func TestC0_AdminCannotAddMember(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create an admin user.
	admin := &store.User{
		ID:          tid("c0-admin"),
		Email:       "admin@test.com",
		DisplayName: "Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	ensureHubMembership(ctx, st, admin.ID)

	// Give admin a project-admin binding.
	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// Create a target user to add.
	target := &store.User{
		ID:          tid("c0-target"),
		Email:       "target@test.com",
		DisplayName: "Target",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Admin tries to add a member — should be denied.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0: project-admin must not be able to add members")

	// Verify stable denial code.
	var errResp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	errObj, ok := errResp["error"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, errObj["code"],
		"C0: denial code must be stable product-level, not raw evaluator output")
}

func TestC0_AdminCannotAddAdmin(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create an admin user.
	admin := &store.User{
		ID:          tid("c0-admin2"),
		Email:       "admin2@test.com",
		DisplayName: "Admin2",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	ensureHubMembership(ctx, st, admin.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// Another target user.
	target := &store.User{
		ID:          tid("c0-target2"),
		Email:       "target2@test.com",
		DisplayName: "Target2",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	// Admin tries to add another admin — must be denied.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0 exit gate: project-admin must not add another admin")
}

func TestC0_AdminCannotPromoteOwner(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create admin and a regular member.
	admin := &store.User{
		ID:          tid("c0-admin3"),
		Email:       "admin3@test.com",
		DisplayName: "Admin3",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	ensureHubMembership(ctx, st, admin.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	target := &store.User{
		ID:          tid("c0-target3"),
		Email:       "target3@test.com",
		DisplayName: "Target3",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	// Admin tries to add someone as owner — must be denied.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: ownerRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0 exit gate: project-admin must not promote someone to owner")
}

func TestC0_AdminCannotRemoveOwner(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Add a second owner so the last-owner guard doesn't fire first.
	secondOwner := &store.User{
		ID:          tid("c0-owner2"),
		Email:       "owner2@test.com",
		DisplayName: "Owner2",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, secondOwner))
	ensureHubMembership(ctx, st, secondOwner.ID)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, secondOwner.ID))

	// Create an admin.
	admin := &store.User{
		ID:          tid("c0-admin4"),
		Email:       "admin4@test.com",
		DisplayName: "Admin4",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	ensureHubMembership(ctx, st, admin.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// Find an owner binding to remove.
	bindings, err := st.ListRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID)
	require.NoError(t, err)

	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	var ownerBindingID string
	for _, b := range bindings {
		if b.PrincipalID == secondOwner.ID && b.RoleDefinitionID == ownerRoleDef.ID {
			ownerBindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, ownerBindingID)

	// Admin tries to remove an owner — must be denied.
	rec := doRequestAsUser(t, srv, admin, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+ownerBindingID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0 exit gate: project-admin must not remove an owner")
}

func TestC0_AdminCannotDemoteAdmin(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create two admins.
	admin1 := &store.User{
		ID:          tid("c0-admin5"),
		Email:       "admin5@test.com",
		DisplayName: "Admin5",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin1))
	ensureHubMembership(ctx, st, admin1.ID)

	admin2 := &store.User{
		ID:          tid("c0-admin6"),
		Email:       "admin6@test.com",
		DisplayName: "Admin6",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin2))
	ensureHubMembership(ctx, st, admin2.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin1.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	admin2Binding, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin2.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Admin1 tries to demote admin2 to member — must be denied.
	rec := doRequestAsUser(t, srv, admin1, http.MethodPatch,
		"/api/v1/projects/"+project.ID+"/members/"+admin2Binding.ID,
		updateProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0 exit gate: project-admin must not demote another admin")
}

func TestC0_AdminCannotRemoveAdmin(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create two admins.
	admin1 := &store.User{
		ID:          tid("c0-admin7"),
		Email:       "admin7@test.com",
		DisplayName: "Admin7",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin1))
	ensureHubMembership(ctx, st, admin1.ID)

	admin2 := &store.User{
		ID:          tid("c0-admin8"),
		Email:       "admin8@test.com",
		DisplayName: "Admin8",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin2))
	ensureHubMembership(ctx, st, admin2.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin1.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	admin2Binding, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin2.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// Admin1 tries to remove admin2 — must be denied.
	rec := doRequestAsUser(t, srv, admin1, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+admin2Binding.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0 exit gate: project-admin must not remove another admin")
}

// ---------------------------------------------------------------------------
// C0: Stable denial codes (not raw evaluator output)
// ---------------------------------------------------------------------------

func TestC0_DenialCodesAreStable(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create an admin.
	admin := &store.User{
		ID:          tid("c0-admin9"),
		Email:       "admin9@test.com",
		DisplayName: "Admin9",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	ensureHubMembership(ctx, st, admin.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	target := &store.User{
		ID:          tid("c0-target-dc"),
		Email:       "targetdc@test.com",
		DisplayName: "TargetDC",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	// Admin tries to add a member — denied with ROLE_ASSIGNMENT_FORBIDDEN.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	var errResp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	errObj, ok := errResp["error"].(map[string]interface{})
	require.True(t, ok, "response must have error object")

	// Verify the code is the stable product-level code.
	assert.Equal(t, ErrCodeRoleAssignmentForbidden, errObj["code"],
		"denial must use stable code, not raw evaluator output")

	// Verify the message does NOT contain internal permission names.
	msg, _ := errObj["message"].(string)
	assert.NotContains(t, msg, "agent.delete",
		"denial message must not leak internal permission names")
	assert.NotContains(t, msg, "actor lacks permission",
		"denial message must not leak raw evaluator reason")
}

// ---------------------------------------------------------------------------
// C0: Owner can still manage membership (positive case)
// ---------------------------------------------------------------------------

func TestC0_OwnerCanStillAddMember(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	target := &store.User{
		ID:          tid("c0-target-own"),
		Email:       "target-own@test.com",
		DisplayName: "TargetOwn",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Owner can add a member — positive case for containment sanity.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"C0: project-owner must still be able to add members")
}

func TestC0_OwnerCanAddAdmin(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	target := &store.User{
		ID:          tid("c0-target-oa"),
		Email:       "target-oa@test.com",
		DisplayName: "TargetOA",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	// Owner can add an admin.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"C0: project-owner must be able to add admins")
}

func TestC0_OwnerCanAddOwner(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	target := &store.User{
		ID:          tid("c0-target-oo"),
		Email:       "target-oo@test.com",
		DisplayName: "TargetOO",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	// Owner can add another owner.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: ownerRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"C0: project-owner must be able to add another owner")
}

// ---------------------------------------------------------------------------
// C0: Cross-project containment
// ---------------------------------------------------------------------------

func TestC0_CrossProjectDenied(t *testing.T) {
	srv, st, _, _, _ := setupProjectMembersTest(t)
	ctx := context.Background()

	// Create a second project with its own owner.
	otherOwner := &store.User{
		ID:          tid("c0-other-owner"),
		Email:       "other-owner@test.com",
		DisplayName: "OtherOwner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, otherOwner))
	ensureHubMembership(ctx, st, otherOwner.ID)

	otherProject := &store.Project{
		ID:        tid("c0-other-proj"),
		Name:      "Other Project",
		Slug:      "c0-other-project",
		OwnerID:   otherOwner.ID,
		CreatedBy: otherOwner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, otherProject))
	srv.createProjectMembersGroup(ctx, otherProject)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, otherProject.ID, otherOwner.ID))

	// The first project's owner tries to manage members in the other project.
	// They should be denied (they're not an owner there).
	target := &store.User{
		ID:          tid("c0-target-cp"),
		Email:       "targetcp@test.com",
		DisplayName: "TargetCP",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// First project's owner (from setupProjectMembersTest) tries to add
	// a member to the OTHER project — should be denied.
	owner := &store.User{ID: tid("pm-owner"), Email: "owner@test.com", Role: store.UserRoleMember}
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		"/api/v1/projects/"+otherProject.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0: project owner of one project must not manage members of another project")
}
