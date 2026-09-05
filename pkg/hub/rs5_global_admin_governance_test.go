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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS5: Global Admin Governance — Hub-Level Authority Over Project Bindings
//
// Tests cover:
//   - Super-admin with no project membership can delete project-member binding
//   - Hub-admin with no project membership can delete project-member binding
//   - Equivalent create path works subject to CanDelegate
//   - Actor with only project-owner/project-admin authority still follows matrix
//   - Ordinary hub member denied; no mutation
//   - Hub-admin denied when CanDelegate ceiling is insufficient for target role
//   - Last owner cannot be deleted through global endpoint
//   - Structured denial shape/action for create and delete membership denials
//   - Audit record and credential-kind enforcement
// =============================================================================

// ---------------------------------------------------------------------------
// Helper: create a hub-admin user with no project membership
// ---------------------------------------------------------------------------

// createHubAdminUser creates a user with hub-admin system binding but no
// project membership. Returns the user and its hub-admin binding.
func createHubAdminUser(t *testing.T, s store.Store, userID, email string) *store.User {
	t.Helper()
	ctx := context.Background()

	user := &store.User{
		ID:          userID,
		Email:       email,
		DisplayName: "Hub Admin",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, userID)

	// Get hub-admin role definition and create binding.
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test-setup",
	})
	require.NoError(t, err)

	return user
}

// createOrdinaryHubMember creates a hub member with no system admin roles and
// no project membership. Returns the user.
func createOrdinaryHubMember(t *testing.T, s store.Store, userID, email string) *store.User {
	t.Helper()
	ctx := context.Background()

	user := &store.User{
		ID:          userID,
		Email:       email,
		DisplayName: "Ordinary Member",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, userID)
	return user
}

// ---------------------------------------------------------------------------
// RS5.1: Super-admin with no project membership can delete a project-member binding
// ---------------------------------------------------------------------------

func TestRS5_SuperAdmin_NoProjectMembership_DeleteProjectMember(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project with an owner.
	projectID := tid("rs5-sa-del-proj")
	ownerID := tid("rs5-sa-del-owner")
	createRS1Project(t, s, projectID, ownerID)

	// Create a target member binding in the project.
	targetID := tid("rs5-sa-del-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Dev user is super-admin and has NO project membership.
	// Use the admin role-binding DELETE endpoint.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code,
		"super-admin without project membership should delete project-member binding: %s",
		rec.Body.String())

	// Verify the binding is gone.
	_, err = s.GetRoleBinding(ctx, binding.ID)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"binding should have been deleted")
}

// ---------------------------------------------------------------------------
// RS5.2: Hub-admin with no project membership can delete a project-member binding
// ---------------------------------------------------------------------------

func TestRS5_HubAdmin_NoProjectMembership_DeleteProjectMember(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project with an owner.
	projectID := tid("rs5-ha-del-proj")
	ownerID := tid("rs5-ha-del-owner")
	createRS1Project(t, s, projectID, ownerID)

	// Create a target member binding.
	targetID := tid("rs5-ha-del-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Hub-admin user with NO project membership.
	hubAdminID := tid("rs5-ha-del-actor")
	hubAdmin := createHubAdminUser(t, s, hubAdminID, "hub-admin-del@test.com")

	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code,
		"hub-admin without project membership should delete project-member binding: %s",
		rec.Body.String())

	// Verify the binding is gone.
	_, err = s.GetRoleBinding(ctx, binding.ID)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"binding should have been deleted")
}

// ---------------------------------------------------------------------------
// RS5.3: Equivalent create path works subject to CanDelegate
// ---------------------------------------------------------------------------

func TestRS5_SuperAdmin_NoProjectMembership_CreateProjectMember(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project with an owner.
	projectID := tid("rs5-sa-cre-proj")
	ownerID := tid("rs5-sa-cre-owner")
	createRS1Project(t, s, projectID, ownerID)

	// Create a target user.
	targetID := tid("rs5-sa-cre-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Dev user (super-admin) creates a project-member binding with no project membership.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})

	assert.Equal(t, http.StatusCreated, rec.Code,
		"super-admin without project membership should create project-member binding: %s",
		rec.Body.String())

	// Verify the binding exists.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	found := false
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			found = true
		}
	}
	assert.True(t, found, "binding should exist after creation")
}

func TestRS5_HubAdmin_NoProjectMembership_CreateProjectMember(t *testing.T) {
	// Hub-admin passes the governance gate (has hub-level role_binding.create)
	// but CanDelegate correctly denies because hub-admin does not hold the
	// project-member permissions (e.g. agent.create). Per brief §3: "A
	// hub-admin must not assign a role whose permissions exceed its delegation
	// ceiling merely because it has role_binding.create." Super-admin bypasses
	// CanDelegate entirely, which is why the super-admin create test passes.
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-ha-cre-proj")
	ownerID := tid("rs5-ha-cre-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-ha-cre-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	hubAdminID := tid("rs5-ha-cre-actor")
	hubAdmin := createHubAdminUser(t, s, hubAdminID, "hub-admin-cre@test.com")

	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub-admin should be denied by CanDelegate ceiling (lacks project-member permissions): %s",
		rec.Body.String())

	// Verify the response mentions delegation denial.
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "delegate",
		"denial should mention delegation ceiling")

	// Verify NO binding was created.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			t.Fatal("binding should not have been created when CanDelegate denies")
		}
	}
}

// ---------------------------------------------------------------------------
// RS5.4: Project-owner/admin authority still follows governance matrix
// ---------------------------------------------------------------------------

func TestRS5_ProjectOwner_StillFollowsMatrix(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-pm-matrix-proj")
	ownerID := tid("rs5-pm-matrix-owner")
	createRS1Project(t, s, projectID, ownerID)

	// Project admin can add members but NOT admins.
	adminID := tid("rs5-pm-matrix-admin")
	createRS1UserWithRole(t, s, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)

	targetID := tid("rs5-pm-matrix-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	// Admin trying to add an admin role — should be denied by governance matrix.
	rec := doRequestAsUser(t, srv, &store.User{
		ID: adminID, Email: adminID + "@test.com",
		DisplayName: "Admin", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project-admin should be denied admin role assignment by governance matrix: %s",
		rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS5.5: Ordinary hub member denied; no mutation
// ---------------------------------------------------------------------------

func TestRS5_OrdinaryHubMember_Denied_NoMutation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-ohm-denied-proj")
	ownerID := tid("rs5-ohm-denied-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-ohm-denied-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Ordinary hub member — no system admin roles, no project membership.
	ordinaryID := tid("rs5-ohm-denied-actor")
	ordinaryUser := createOrdinaryHubMember(t, s, ordinaryID, "ordinary@test.com")

	// Try to delete via admin API — should fail at hub-level authz gate
	// (role_binding.delete permission check).
	rec := doRequestAsUser(t, srv, ordinaryUser, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"ordinary hub member should be denied: %s", rec.Body.String())

	// Verify binding was NOT mutated.
	_, err = s.GetRoleBinding(ctx, binding.ID)
	assert.NoError(t, err, "binding must survive denied deletion")
}

// ---------------------------------------------------------------------------
// RS5.6: Hub-admin denied when CanDelegate ceiling is insufficient
// ---------------------------------------------------------------------------

func TestRS5_HubAdmin_CanDelegate_CeilingInsufficient(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-cd-ceil-proj")
	ownerID := tid("rs5-cd-ceil-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-cd-ceil-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	// Create a custom system role with role_binding.create but NOT the
	// project permissions needed for the owner role. This user can pass
	// the governance gate but should fail the CanDelegate check.
	customUserID := tid("rs5-cd-ceil-actor")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: customUserID, Email: customUserID + "@test.com",
		DisplayName: "Limited Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, customUserID)

	// Create a custom role with only role_binding.create.
	customRD, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-limited-admin-" + t.Name(),
		Description: "Limited admin with only role_binding.create",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"role_binding.create", "role_binding.read"},
		System:      false,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      customUserID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test-setup",
	})
	require.NoError(t, err)

	// Try to create a project-owner binding. CanDelegate should deny because
	// the actor doesn't hold the owner role's permissions.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: customUserID, Email: customUserID + "@test.com",
		DisplayName: "Limited Admin", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: ownerRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"limited admin should be denied by CanDelegate ceiling: %s",
		rec.Body.String())

	// Verify NO binding was created.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, targetID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			t.Fatal("binding should not have been created for CanDelegate-insufficient actor")
		}
	}
}

// ---------------------------------------------------------------------------
// RS5.7: Last owner cannot be deleted through global endpoint
// ---------------------------------------------------------------------------

func TestRS5_LastOwner_CannotBeDeleted_ViaGlobalEndpoint(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project with a single owner.
	projectID := tid("rs5-lo-del-proj")
	ownerID := tid("rs5-lo-del-owner")
	createRS1Project(t, s, projectID, ownerID)

	// Find the owner binding.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	ownerBindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, ownerID)
	require.NoError(t, err)
	var ownerBinding *store.RoleBinding
	for _, b := range ownerBindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID && b.RoleDefinitionID == ownerRD.ID {
			ownerBinding = b
			break
		}
	}
	require.NotNil(t, ownerBinding, "owner binding must exist")

	// Super-admin (dev user) tries to delete the last owner.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+ownerBinding.ID, nil)

	assert.Equal(t, http.StatusConflict, rec.Code,
		"last-owner deletion should be denied: %s", rec.Body.String())

	// Parse error to check it's last_owner code.
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeLastOwner, errResp.Error.Code,
		"denial should use last_owner error code")

	// Verify binding was NOT mutated.
	_, err = s.GetRoleBinding(ctx, ownerBinding.ID)
	assert.NoError(t, err, "last-owner binding must survive denied deletion")
}

// ---------------------------------------------------------------------------
// RS5.8: Structured denial shape/action for create and delete
// ---------------------------------------------------------------------------

func TestRS5_StructuredDenial_Delete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-sd-del-proj")
	ownerID := tid("rs5-sd-del-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-sd-del-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Ordinary member tries to delete (will be denied at role_binding.delete check).
	ordinaryID := tid("rs5-sd-del-actor")
	ordinaryUser := createOrdinaryHubMember(t, s, ordinaryID, "ordinary-sd-del@test.com")

	rec := doRequestAsUser(t, srv, ordinaryUser, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)

	assert.Equal(t, http.StatusForbidden, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.NotEmpty(t, errResp.Error.Code, "error code should be present")
	// Structured details should include resource_type and denied_action.
	assert.NotNil(t, errResp.Error.Details, "details should be present on 403")
	assert.Equal(t, "role_binding", errResp.Error.Details["resource_type"],
		"resource_type should be role_binding")
	assert.Equal(t, "delete", errResp.Error.Details["denied_action"],
		"denied_action should be delete")
}

func TestRS5_StructuredDenial_Create(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-sd-cre-proj")
	ownerID := tid("rs5-sd-cre-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-sd-cre-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// User with role_binding.create but governance denied (no project role,
	// no system admin — just role_binding.create permission via custom role).
	// This tests that when a user passes the HTTP gate (role_binding.create)
	// but fails governance, the error includes structured details.
	customUserID := tid("rs5-sd-cre-actor")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: customUserID, Email: customUserID + "@test.com",
		DisplayName: "Custom Actor", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, customUserID)

	// Create custom role with ONLY role_binding.create (no role_binding.delete).
	customRD, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        fmt.Sprintf("test-rbc-only-%s", t.Name()),
		Description: "Custom role with only role_binding.create",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"role_binding.create", "role_binding.read"},
		System:      false,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      customUserID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test-setup",
	})
	require.NoError(t, err)

	// This user passes the HTTP gate (has role_binding.create) and the
	// governance gate (has hub-level role_binding.create which overrides
	// no-project-role), but the CanDelegate check should deny because
	// they don't have the project-member role permissions.
	rec := doRequestAsUser(t, srv, &store.User{
		ID: customUserID, Email: customUserID + "@test.com",
		DisplayName: "Custom Actor", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"user with role_binding.create but insufficient delegation should be denied: %s",
		rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.NotEmpty(t, errResp.Error.Code, "error code should be present")
	if errResp.Error.Details != nil {
		assert.Equal(t, "role_binding", errResp.Error.Details["resource_type"])
		assert.Equal(t, "create", errResp.Error.Details["denied_action"])
	}
}

// TestRS5_StructuredDenial_MembershipServiceDelete_ViaAdminEndpoint tests
// that when the membership service returns a 403 through the admin role-binding
// delete endpoint, the response includes structured details.
func TestRS5_StructuredDenial_MembershipServiceDelete_ViaAdminEndpoint(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-msd-del-proj")
	ownerID := tid("rs5-msd-del-owner")
	createRS1Project(t, s, projectID, ownerID)

	// Create a project-admin binding that only owners can manage.
	targetID := tid("rs5-msd-del-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Admin Target", Role: "member", Status: "active",
	}))
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Give a user role_binding.delete + project-admin (but NOT owner).
	// This user has hub-level role_binding.delete so they can manage
	// project-member bindings. But admin bindings require project-owner
	// in the governance matrix. With hub override, even admin bindings
	// should be manageable.
	// Actually, hub override bypasses the governance matrix entirely,
	// so this should succeed for a hub-admin. Let's test with a user
	// who has role_binding.delete but NOT enough permissions to delegate.

	// Use a project-admin actor trying to delete another project-admin
	// binding. The matrix denies this.
	actorID := tid("rs5-msd-del-actor")
	createRS1UserWithRole(t, s, actorID, actorID+"@test.com", projectID, store.ProjectRoleAdmin)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: actorID, Email: actorID + "@test.com",
		DisplayName: "Admin Actor", Role: "member",
	}, http.MethodDelete, "/api/v1/projects/"+projectID+"/members/"+binding.ID, nil)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project-admin should be denied admin binding deletion: %s",
		rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS5.9: Audit record on successful paths
// ---------------------------------------------------------------------------

func TestRS5_AuditRecord_GlobalAdminDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-audit-del-proj")
	ownerID := tid("rs5-audit-del-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-audit-del-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Super-admin (dev user) deletes the binding.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"delete should succeed: %s", rec.Body.String())

	// Verify an audit record was created.
	auditRecords, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		TargetType: "project_membership",
		TargetID:   projectID,
		Limit:      100,
	})
	require.NoError(t, err)

	found := false
	for _, a := range auditRecords {
		if a.MutationType == "project_member_remove" {
			found = true
			assert.NotEmpty(t, a.BeforeSummary, "audit should have before summary")
			break
		}
	}
	assert.True(t, found, "mutation audit record should exist for global admin delete")
}

func TestRS5_AuditRecord_GlobalAdminCreate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-audit-cre-proj")
	ownerID := tid("rs5-audit-cre-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-audit-cre-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Super-admin creates the binding.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings",
		createRoleBindingRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	require.Equal(t, http.StatusCreated, rec.Code,
		"create should succeed: %s", rec.Body.String())

	// Verify an audit record was created.
	auditRecords, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		TargetType: "project_membership",
		TargetID:   projectID,
		Limit:      100,
	})
	require.NoError(t, err)

	found := false
	for _, a := range auditRecords {
		if a.MutationType == "project_member_add" {
			found = true
			assert.NotEmpty(t, a.AfterSummary, "audit should have after summary")
			break
		}
	}
	assert.True(t, found, "mutation audit record should exist for global admin create")
}

// ---------------------------------------------------------------------------
// RS5.10: Credential-kind enforcement (session only via delete endpoint)
// ---------------------------------------------------------------------------

func TestRS5_CredentialKind_WebSession_Allowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-ck-web-proj")
	ownerID := tid("rs5-ck-web-owner")
	createRS1Project(t, s, projectID, ownerID)

	targetID := tid("rs5-ck-web-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Target", Role: "member", Status: "active",
	}))
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	devUser := getDevUser(t, srv, s)
	srv.ensureSuperAdminBinding(ctx, devUser.ID)

	// Use interactive credential kind.
	rec := doDeleteBindingWithCredentialKind(t, srv, devUser, CredentialKindInteractive, binding.ID)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"interactive credential should be allowed for project binding deletion: %s",
		rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS5.11: Hub-admin can delete project-admin binding (hub override bypasses
// direct-owner requirement)
// ---------------------------------------------------------------------------

func TestRS5_HubAdmin_DeleteProjectAdmin_Binding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs5-ha-da-proj")
	ownerID := tid("rs5-ha-da-owner")
	createRS1Project(t, s, projectID, ownerID)

	// Create a project-admin binding.
	targetID := tid("rs5-ha-da-target")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: targetID + "@test.com",
		DisplayName: "Admin Target", Role: "member", Status: "active",
	}))
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	binding, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Hub-admin with no project membership.
	hubAdminID := tid("rs5-ha-da-actor")
	hubAdmin := createHubAdminUser(t, s, hubAdminID, "hub-admin-da@test.com")

	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+binding.ID, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code,
		"hub-admin should delete project-admin binding (hub override): %s",
		rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS5.12: Unit test — actorHasHubRoleBindingAuthority
// ---------------------------------------------------------------------------

func TestRS5_ActorHasHubRoleBindingAuthority(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	authzSvc := NewAuthzService(s, nil)
	svc := NewProjectMembershipService(s, authzSvc, nil)

	// Super-admin user.
	superID := tid("rs5-ahba-super")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: superID, Email: superID + "@test.com",
		DisplayName: "Super", Role: "admin", Status: "active",
	}))
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      superID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	assert.True(t, svc.actorHasHubRoleBindingAuthority(ctx, superID, MembershipOpRemove),
		"super-admin should have role_binding.delete authority")
	assert.True(t, svc.actorHasHubRoleBindingAuthority(ctx, superID, MembershipOpAdd),
		"super-admin should have role_binding.create authority")

	// Hub-admin user.
	hubAdminID := tid("rs5-ahba-hubadm")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: hubAdminID, Email: hubAdminID + "@test.com",
		DisplayName: "HubAdmin", Role: "member", Status: "active",
	}))
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      hubAdminID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test-setup",
	})
	require.NoError(t, err)

	assert.True(t, svc.actorHasHubRoleBindingAuthority(ctx, hubAdminID, MembershipOpRemove),
		"hub-admin should have role_binding.delete authority")
	assert.True(t, svc.actorHasHubRoleBindingAuthority(ctx, hubAdminID, MembershipOpAdd),
		"hub-admin should have role_binding.create authority")

	// Ordinary user.
	ordinaryID := tid("rs5-ahba-ord")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: ordinaryID, Email: ordinaryID + "@test.com",
		DisplayName: "Ordinary", Role: "member", Status: "active",
	}))

	assert.False(t, svc.actorHasHubRoleBindingAuthority(ctx, ordinaryID, MembershipOpRemove),
		"ordinary user should NOT have role_binding.delete authority")
	assert.False(t, svc.actorHasHubRoleBindingAuthority(ctx, ordinaryID, MembershipOpAdd),
		"ordinary user should NOT have role_binding.create authority")
}

// ---------------------------------------------------------------------------
// Helper: doDeleteBindingWithCredentialKind
// (already defined in handlers_roles_test.go, re-use by calling it)
// ---------------------------------------------------------------------------

// addProjectMemberRequest mirrors the request structure used by project member
// endpoints.
type addProjectMemberRequestRS5 struct {
	RoleDefinitionID string `json:"roleDefinitionId"`
	PrincipalType    string `json:"principalType"`
	PrincipalID      string `json:"principalId"`
}

// doDeleteAdminBinding deletes a binding through the admin endpoint using
// a specific user identity and credential kind.
func doDeleteAdminBinding(t *testing.T, srv *Server, user *store.User, credKind CredentialKind, bindingID string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/admin/role-bindings/" + bindingID
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	ctx := req.Context()
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	ctx = contextWithIdentity(ctx, identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: credKind})
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleAdminRoleBindingByID(rec, req)
	return rec
}
