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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS2: Custom Role Preservation on Membership Change
//
// These tests verify that built-in membership mutations (add, replace, remove,
// transfer) never delete, rewrite, or hide additive custom project-scoped role
// bindings. This is the regression suite for the defect where
// findExistingDirectBindingsFromStore returned ALL project-scoped bindings,
// causing replaceBindingTx to delete custom bindings as collateral damage.
// =============================================================================

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// createCustomRoleDef creates a non-built-in project-scoped role definition.
func createCustomRoleDef(t *testing.T, s store.Store, name string, perms []string) *store.RoleDefinition {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        name,
		ScopeType:   store.RoleScopeProject,
		Permissions: perms,
		System:      false,
	})
	require.NoError(t, err, "failed to create custom role definition %q", name)
	return rd
}

// createCustomBinding creates a custom project-scoped role binding directly
// in the store (bypassing the membership service, which only handles built-in
// roles).
func createCustomBinding(t *testing.T, s store.Store, roleDefID, principalID, projectID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: roleDefID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test-custom",
	})
	require.NoError(t, err, "failed to create custom role binding")
	return rb
}

// listProjectBindings returns all project-scoped bindings for a principal in a
// project.
func listProjectBindings(t *testing.T, s store.Store, principalID, projectID string) []*store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	all, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, principalID)
	require.NoError(t, err)
	var result []*store.RoleBinding
	for _, b := range all {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			result = append(result, b)
		}
	}
	return result
}

// assertBindingPreserved verifies that a binding exists unchanged: same ID,
// same role definition, same metadata fields. Uses a fresh read from the
// store as the "before" snapshot to avoid SQLite timestamp-precision
// differences between the in-memory object returned by Create and a
// subsequent Get.
func assertBindingPreserved(t *testing.T, s store.Store, original *store.RoleBinding) {
	t.Helper()
	ctx := context.Background()
	got, err := s.GetRoleBinding(ctx, original.ID)
	require.NoError(t, err, "custom binding %s must still exist", original.ID)
	assert.Equal(t, original.ID, got.ID, "binding ID must be unchanged")
	assert.Equal(t, original.RoleDefinitionID, got.RoleDefinitionID, "role definition must be unchanged")
	assert.Equal(t, original.PrincipalType, got.PrincipalType, "principal type must be unchanged")
	assert.Equal(t, original.PrincipalID, got.PrincipalID, "principal ID must be unchanged")
	assert.Equal(t, original.ScopeType, got.ScopeType, "scope type must be unchanged")
	assert.Equal(t, original.ScopeID, got.ScopeID, "scope ID must be unchanged")
	assert.Equal(t, original.CreatedBy, got.CreatedBy, "created_by must be unchanged")
	if original.NotBefore != nil {
		require.NotNil(t, got.NotBefore, "not_before must be preserved")
		assert.True(t, original.NotBefore.Equal(*got.NotBefore), "not_before must be unchanged")
	} else {
		assert.Nil(t, got.NotBefore, "not_before must remain nil")
	}
	if original.ExpiresAt != nil {
		require.NotNil(t, got.ExpiresAt, "expires_at must be preserved")
		assert.True(t, original.ExpiresAt.Equal(*got.ExpiresAt), "expires_at must be unchanged")
	} else {
		assert.Nil(t, got.ExpiresAt, "expires_at must remain nil")
	}
}

// snapshotBinding reads a binding from the store to get a consistent
// timestamp-precision snapshot. Use this immediately after CreateRoleBinding
// to get the "before" reference for assertBindingPreserved.
func snapshotBinding(t *testing.T, s store.Store, rb *store.RoleBinding) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	got, err := s.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err, "failed to snapshot binding %s", rb.ID)
	return got
}

// ---------------------------------------------------------------------------
// RS2.1: Membership change preserves custom bindings
// ---------------------------------------------------------------------------

// TestRS2_MembershipChange_PreservesCustomBindings verifies the core defect
// fix: changing a built-in membership role (project-member -> project-admin)
// must preserve all custom project-scoped role bindings field-for-field.
func TestRS2_MembershipChange_PreservesCustomBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-chg-proj")
	ownerID := tid("rs2-chg-owner")
	targetID := tid("rs2-chg-target")

	createRS1Project(t, s, projectID, ownerID)

	// Create the target user.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-target@test.com",
		DisplayName: "RS2 Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add target as project-member via HTTP.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "initial add: %s", rec.Body.String())

	// Create two distinct custom project-scoped role bindings directly in store.
	customViewer := createCustomRoleDef(t, s, "rs2-custom-viewer", []string{"project.read"})
	customEditor := createCustomRoleDef(t, s, "rs2-custom-editor", []string{"project.read", "project.write"})

	customBinding1 := snapshotBinding(t, s, createCustomBinding(t, s, customViewer.ID, targetID, projectID))
	customBinding2 := snapshotBinding(t, s, createCustomBinding(t, s, customEditor.ID, targetID, projectID))

	// Verify we have 3 bindings: 1 built-in + 2 custom.
	bindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, bindings, 3, "expected 3 bindings before membership change")

	// Change membership from project-member to project-admin via HTTP POST.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusOK, rec.Code, "membership change: %s", rec.Body.String())

	// Verify: exactly 3 bindings remain (1 new built-in + 2 custom).
	bindings = listProjectBindings(t, s, targetID, projectID)
	require.Len(t, bindings, 3,
		"expected 3 bindings after membership change (1 built-in + 2 custom), got %d", len(bindings))

	// The built-in binding should now be project-admin.
	var builtInCount int
	for _, b := range bindings {
		rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, rdErr)
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			builtInCount++
			assert.Equal(t, store.ProjectRoleAdmin, rd.Name,
				"built-in binding should be project-admin after change")
		}
	}
	assert.Equal(t, 1, builtInCount, "exactly one built-in binding should exist")

	// Custom bindings must be preserved field-for-field.
	assertBindingPreserved(t, s, customBinding1)
	assertBindingPreserved(t, s, customBinding2)
}

// ---------------------------------------------------------------------------
// RS2.2: Membership removal preserves custom bindings
// ---------------------------------------------------------------------------

// TestRS2_MembershipRemoval_PreservesCustomBindings verifies that removing a
// built-in membership binding via DELETE leaves all custom bindings intact.
func TestRS2_MembershipRemoval_PreservesCustomBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-rm-proj")
	ownerID := tid("rs2-rm-owner")
	targetID := tid("rs2-rm-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-rm-target@test.com",
		DisplayName: "RS2 RM Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add target as project-member.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "initial add: %s", rec.Body.String())

	// Parse the binding ID from the response.
	var memberInfo projectMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&memberInfo))
	memberBindingID := memberInfo.RoleBinding.ID

	// Create custom bindings.
	customViewer := createCustomRoleDef(t, s, "rs2-rm-custom-viewer", []string{"project.read"})
	customEditor := createCustomRoleDef(t, s, "rs2-rm-custom-editor", []string{"project.read", "project.write"})
	customBinding1 := snapshotBinding(t, s, createCustomBinding(t, s, customViewer.ID, targetID, projectID))
	customBinding2 := snapshotBinding(t, s, createCustomBinding(t, s, customEditor.ID, targetID, projectID))

	// Remove the built-in membership via DELETE.
	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodDelete, "/api/v1/projects/"+projectID+"/members/"+memberBindingID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code,
		"membership removal: %s", rec.Body.String())

	// Verify: the built-in binding is gone, but custom bindings survive.
	bindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, bindings, 2,
		"expected 2 custom bindings after removing built-in membership, got %d", len(bindings))

	assertBindingPreserved(t, s, customBinding1)
	assertBindingPreserved(t, s, customBinding2)
}

// ---------------------------------------------------------------------------
// RS2.3: Membership add where custom bindings already exist
// ---------------------------------------------------------------------------

// TestRS2_MembershipAdd_WithExistingCustomBindings verifies that adding a
// built-in membership where custom bindings already exist does not delete
// the custom bindings.
func TestRS2_MembershipAdd_WithExistingCustomBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-add-proj")
	ownerID := tid("rs2-add-owner")
	targetID := tid("rs2-add-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-add-target@test.com",
		DisplayName: "RS2 Add Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create custom bindings FIRST (before any built-in membership).
	customViewer := createCustomRoleDef(t, s, "rs2-add-custom-viewer", []string{"project.read"})
	customEditor := createCustomRoleDef(t, s, "rs2-add-custom-editor", []string{"project.read", "project.write"})
	customBinding1 := snapshotBinding(t, s, createCustomBinding(t, s, customViewer.ID, targetID, projectID))
	customBinding2 := snapshotBinding(t, s, createCustomBinding(t, s, customEditor.ID, targetID, projectID))

	// Now add built-in membership.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "add member: %s", rec.Body.String())

	// Verify: 3 bindings (1 built-in + 2 custom).
	bindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, bindings, 3,
		"expected 3 bindings after adding built-in membership, got %d", len(bindings))

	assertBindingPreserved(t, s, customBinding1)
	assertBindingPreserved(t, s, customBinding2)
}

// ---------------------------------------------------------------------------
// RS2.4: Transfer ownership preserves custom bindings for both participants
// ---------------------------------------------------------------------------

// TestRS2_TransferOwnership_PreservesCustomBindings verifies that ownership
// transfer preserves custom bindings for both the old owner and the new owner.
func TestRS2_TransferOwnership_PreservesCustomBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-xfr-proj")
	ownerID := tid("rs2-xfr-owner")
	newOwnerID := tid("rs2-xfr-newown")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: newOwnerID, Email: "rs2-newowner@test.com",
		DisplayName: "RS2 New Owner", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, newOwnerID)

	// Give new owner a project-member role first.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      newOwnerID,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "add new owner as member: %s", rec.Body.String())

	// Create custom bindings for BOTH old owner and new owner.
	customViewer := createCustomRoleDef(t, s, "rs2-xfr-custom-viewer", []string{"project.read"})
	customEditor := createCustomRoleDef(t, s, "rs2-xfr-custom-editor", []string{"project.read", "project.write"})

	// Old owner's custom bindings.
	ownerCustom1 := snapshotBinding(t, s, createCustomBinding(t, s, customViewer.ID, ownerID, projectID))
	ownerCustom2 := snapshotBinding(t, s, createCustomBinding(t, s, customEditor.ID, ownerID, projectID))

	// New owner's custom bindings.
	newOwnerCustom1 := snapshotBinding(t, s, createCustomBinding(t, s, customViewer.ID, newOwnerID, projectID))
	newOwnerCustom2 := snapshotBinding(t, s, createCustomBinding(t, s, customEditor.ID, newOwnerID, projectID))

	// Transfer ownership.
	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/transfer-ownership",
		transferOwnershipRequest{NewOwnerID: newOwnerID})
	require.Equal(t, http.StatusOK, rec.Code, "transfer: %s", rec.Body.String())

	// Verify old owner's custom bindings are preserved.
	assertBindingPreserved(t, s, ownerCustom1)
	assertBindingPreserved(t, s, ownerCustom2)

	// Verify new owner's custom bindings are preserved.
	assertBindingPreserved(t, s, newOwnerCustom1)
	assertBindingPreserved(t, s, newOwnerCustom2)

	// Old owner should have custom bindings + downgraded built-in (project-member).
	oldOwnerBindings := listProjectBindings(t, s, ownerID, projectID)
	require.Len(t, oldOwnerBindings, 3,
		"old owner should have 3 bindings (1 built-in member + 2 custom)")

	// New owner should have custom bindings + promoted built-in (project-owner).
	newOwnerBindings := listProjectBindings(t, s, newOwnerID, projectID)
	require.Len(t, newOwnerBindings, 3,
		"new owner should have 3 bindings (1 built-in owner + 2 custom)")

	// Verify the built-in roles changed correctly.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	var oldOwnerBuiltInRole, newOwnerBuiltInRole string
	for _, b := range oldOwnerBindings {
		rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, rdErr)
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			oldOwnerBuiltInRole = rd.Name
		}
	}
	for _, b := range newOwnerBindings {
		rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, rdErr)
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			newOwnerBuiltInRole = rd.Name
		}
	}
	assert.Equal(t, store.ProjectRoleMember, oldOwnerBuiltInRole,
		"old owner should be downgraded to project-member")
	assert.Equal(t, ownerRD.Name, newOwnerBuiltInRole,
		"new owner should be promoted to project-owner")
}

// ---------------------------------------------------------------------------
// RS2.5: Regression — second built-in via generic API still conflicts
// ---------------------------------------------------------------------------

// TestRS2_SecondBuiltIn_StillConflicts verifies that creating a second
// different built-in membership role through the generic role binding API
// correctly returns a conflict error.
func TestRS2_SecondBuiltIn_StillConflicts(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-conf-proj")
	ownerID := tid("rs2-conf-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Owner already has a project-owner binding. Attempt to create a second
	// built-in membership (project-member) through the store directly.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ownerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.Error(t, err, "second built-in membership must be rejected")
	assert.ErrorIs(t, err, store.ErrBuiltInMembershipConflict,
		"expected ErrBuiltInMembershipConflict, got: %v", err)
}

// ---------------------------------------------------------------------------
// RS2.6: Regression — exact duplicate custom binding still conflicts
// ---------------------------------------------------------------------------

// TestRS2_ExactDuplicateCustom_StillConflicts verifies that creating an
// exact duplicate custom binding (same role, same principal, same scope)
// is still correctly rejected.
func TestRS2_ExactDuplicateCustom_StillConflicts(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-dup-proj")
	ownerID := tid("rs2-dup-owner")
	targetID := tid("rs2-dup-target")

	createRS1Project(t, s, projectID, ownerID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-dup@test.com",
		DisplayName: "RS2 Dup Target", Role: "member", Status: "active",
	}))

	customViewer := createCustomRoleDef(t, s, "rs2-dup-custom-viewer", []string{"project.read"})
	createCustomBinding(t, s, customViewer.ID, targetID, projectID)

	// Exact duplicate must fail.
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customViewer.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.Error(t, err, "exact duplicate custom binding must be rejected")
	assert.ErrorIs(t, err, store.ErrAlreadyExists,
		"expected ErrAlreadyExists, got: %v", err)
}

// ---------------------------------------------------------------------------
// RS2.7: Regression — distinct custom bindings still coexist
// ---------------------------------------------------------------------------

// TestRS2_DistinctCustomBindings_Coexist verifies that multiple distinct
// custom project-scoped role bindings can coexist for the same principal.
func TestRS2_DistinctCustomBindings_Coexist(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-coex-proj")
	ownerID := tid("rs2-coex-owner")
	targetID := tid("rs2-coex-target")

	createRS1Project(t, s, projectID, ownerID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-coex@test.com",
		DisplayName: "RS2 Coex Target", Role: "member", Status: "active",
	}))

	customViewer := createCustomRoleDef(t, s, "rs2-coex-viewer", []string{"project.read"})
	customEditor := createCustomRoleDef(t, s, "rs2-coex-editor", []string{"project.read", "project.write"})
	customAdmin := createCustomRoleDef(t, s, "rs2-coex-custom-admin", []string{"project.read", "project.write", "project.manage"})

	cb1 := createCustomBinding(t, s, customViewer.ID, targetID, projectID)
	cb2 := createCustomBinding(t, s, customEditor.ID, targetID, projectID)
	cb3 := createCustomBinding(t, s, customAdmin.ID, targetID, projectID)

	bindings := listProjectBindings(t, s, targetID, projectID)
	// Only custom bindings (no built-in membership added yet).
	assert.Len(t, bindings, 3, "three distinct custom bindings must coexist")
	assert.NotEmpty(t, cb1.ID)
	assert.NotEmpty(t, cb2.ID)
	assert.NotEmpty(t, cb3.ID)
}

// ---------------------------------------------------------------------------
// RS2.8: Regression — last-owner guard still works with custom bindings
// ---------------------------------------------------------------------------

// TestRS2_LastOwnerGuard_WithCustomBindings verifies that the last-owner
// guard still works correctly when custom bindings exist alongside the
// built-in ownership binding.
func TestRS2_LastOwnerGuard_WithCustomBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-lo-proj")
	ownerID := tid("rs2-lo-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Add custom bindings for the owner.
	customViewer := createCustomRoleDef(t, s, "rs2-lo-custom-viewer", []string{"project.read"})
	createCustomBinding(t, s, customViewer.ID, ownerID, projectID)

	// Attempt to demote the sole owner to member (should be denied).
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      ownerID,
		})
	// Last-owner guard should prevent this with 409 Conflict.
	assert.Equal(t, http.StatusConflict, rec.Code,
		"last-owner demotion should be denied with 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS2.9: HTTP endpoint exercise — POST membership replacement
// ---------------------------------------------------------------------------

// TestRS2_HTTPPost_MembershipReplacement_PreservesCustom exercises the
// full HTTP POST endpoint for membership replacement and verifies
// custom bindings are preserved in the response path.
func TestRS2_HTTPPost_MembershipReplacement_PreservesCustom(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-http-proj")
	ownerID := tid("rs2-http-owner")
	targetID := tid("rs2-http-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-http-target@test.com",
		DisplayName: "RS2 HTTP Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add as member.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	// Add custom bindings.
	customRole := createCustomRoleDef(t, s, "rs2-http-custom", []string{"project.read"})
	customBinding := snapshotBinding(t, s, createCustomBinding(t, s, customRole.ID, targetID, projectID))

	// Replace member -> admin via HTTP POST.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusOK, rec.Code, "replacement: %s", rec.Body.String())

	// Response should show the new admin binding.
	var info projectMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
	assert.Equal(t, store.ProjectRoleAdmin, info.RoleName,
		"response should show admin role")

	// Custom binding must still exist.
	assertBindingPreserved(t, s, customBinding)
}

// ---------------------------------------------------------------------------
// RS2.10: PATCH update role — single binding path (not affected, but verify)
// ---------------------------------------------------------------------------

// TestRS2_PATCHUpdateRole_PreservesCustomBindings verifies that the PATCH
// single-binding update path also does not affect custom bindings (it should
// not, since it operates on a single binding by ID).
func TestRS2_PATCHUpdateRole_PreservesCustomBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-patch-proj")
	ownerID := tid("rs2-patch-owner")
	targetID := tid("rs2-patch-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-patch-target@test.com",
		DisplayName: "RS2 Patch Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add as member.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	var info projectMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))
	bindingID := info.RoleBinding.ID

	// Add custom bindings.
	customRole := createCustomRoleDef(t, s, "rs2-patch-custom", []string{"project.read"})
	customBinding := snapshotBinding(t, s, createCustomBinding(t, s, customRole.ID, targetID, projectID))

	// PATCH to change the built-in binding's role.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPatch, "/api/v1/projects/"+projectID+"/members/"+bindingID,
		updateProjectMemberRequest{RoleDefinitionID: adminRD.ID})

	// PATCH should succeed with 200 OK.
	require.Equal(t, http.StatusOK, rec.Code,
		"PATCH should succeed with 200 OK, got %d: %s", rec.Code, rec.Body.String())

	// Custom binding must still exist.
	assertBindingPreserved(t, s, customBinding)
}

// ---------------------------------------------------------------------------
// RS2.11: Idempotent re-add with custom bindings
// ---------------------------------------------------------------------------

// TestRS2_IdempotentReadd_PreservesCustomBindings verifies that
// re-adding the same built-in role (idempotent case) preserves custom
// bindings.
func TestRS2_IdempotentReadd_PreservesCustomBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs2-idem-proj")
	ownerID := tid("rs2-idem-owner")
	targetID := tid("rs2-idem-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs2-idem-target@test.com",
		DisplayName: "RS2 Idem Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add as member.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	// Add custom bindings.
	customRole := createCustomRoleDef(t, s, "rs2-idem-custom", []string{"project.read"})
	customBinding := snapshotBinding(t, s, createCustomBinding(t, s, customRole.ID, targetID, projectID))

	// Re-add same role (idempotent case).
	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	// Idempotent re-add should succeed (200 OK, not create new).
	require.True(t, rec.Code >= 200 && rec.Code < 300,
		"idempotent re-add should succeed, got %d: %s", rec.Code, rec.Body.String())

	// Custom binding must still exist.
	assertBindingPreserved(t, s, customBinding)

	// Total bindings should be 2 (1 built-in + 1 custom).
	bindings := listProjectBindings(t, s, targetID, projectID)
	assert.Len(t, bindings, 2,
		"expected 2 bindings after idempotent re-add (1 built-in + 1 custom)")
}
