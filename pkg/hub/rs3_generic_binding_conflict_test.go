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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS3: Generic Role-Binding POST Create-Only Semantics
//
// The generic POST /api/v1/admin/role-bindings endpoint is create-only for
// built-in project membership roles. When a principal already has a built-in
// membership binding in the same project, the endpoint must return HTTP 409
// with a truthful conflict message and make no mutation. Explicit membership
// endpoints (POST /projects/:id/members, PATCH) retain replacement semantics.
//
// These tests verify the RS3 contract and check for regressions against the
// RS2 custom-role preservation hotfix.
// =============================================================================

// ---------------------------------------------------------------------------
// RS3.1: Generic POST with existing built-in + custom bindings → 409, zero mutation
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_ExistingBuiltIn_Conflict verifies that the generic
// role-binding endpoint returns 409 when a principal already has a different
// built-in membership, and does not mutate any existing binding (built-in or
// custom).
func TestRS3_GenericPost_ExistingBuiltIn_Conflict(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-gc-proj")
	ownerID := tid("rs3-gc-owner")
	targetID := tid("rs3-gc-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-gc-target@test.com",
		DisplayName: "RS3 Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add target as project-member via the explicit membership endpoint.
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

	// Snapshot the built-in binding.
	builtInBindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, builtInBindings, 1, "should have exactly 1 built-in binding")
	builtInSnapshot := snapshotBinding(t, s, builtInBindings[0])

	// Create two distinct custom project-scoped role bindings.
	customViewerDef := createCustomRoleDef(t, s, "rs3-gc-custom-viewer", []string{"project.read"})
	customEditorDef := createCustomRoleDef(t, s, "rs3-gc-custom-editor", []string{"project.read", "project.write"})
	customBinding1 := snapshotBinding(t, s, createCustomBinding(t, s, customViewerDef.ID, targetID, projectID))
	customBinding2 := snapshotBinding(t, s, createCustomBinding(t, s, customEditorDef.ID, targetID, projectID))

	// Verify: 3 bindings (1 built-in + 2 custom).
	bindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, bindings, 3, "expected 3 bindings before generic POST")

	// --- THE TEST: generic POST with project-admin (different built-in) ---
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})

	// Must be 409 Conflict.
	require.Equal(t, http.StatusConflict, rec.Code,
		"generic POST with different built-in must return 409, got %d: %s", rec.Code, rec.Body.String())

	// Response body should name the existing role.
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, store.ProjectRoleMember,
		"conflict message should name the existing built-in role")

	// --- ZERO MUTATION: all 3 records must be preserved field-for-field ---
	bindingsAfter := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, bindingsAfter, 3,
		"expected 3 bindings after rejected generic POST (zero mutation)")

	// Built-in binding unchanged.
	assertBindingPreserved(t, s, builtInSnapshot)

	// Custom bindings unchanged.
	assertBindingPreserved(t, s, customBinding1)
	assertBindingPreserved(t, s, customBinding2)

	// Verify the built-in is still project-member (not replaced).
	for _, b := range bindingsAfter {
		rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, rdErr)
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			assert.Equal(t, store.ProjectRoleMember, rd.Name,
				"built-in binding must still be project-member after rejected generic POST")
		}
	}
}

// ---------------------------------------------------------------------------
// RS3.2: Explicit membership update from member to admin succeeds +
//        preserves both custom bindings
// ---------------------------------------------------------------------------

// TestRS3_ExplicitUpdate_MemberToAdmin_PreservesCustom verifies that the
// explicit membership update endpoint (POST /projects/:id/members) can change
// the built-in role and preserves all custom bindings.
func TestRS3_ExplicitUpdate_MemberToAdmin_PreservesCustom(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-eu-proj")
	ownerID := tid("rs3-eu-owner")
	targetID := tid("rs3-eu-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-eu-target@test.com",
		DisplayName: "RS3 Explicit Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add target as project-member via explicit endpoint.
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

	// Add two custom bindings.
	customViewerDef := createCustomRoleDef(t, s, "rs3-eu-custom-viewer", []string{"project.read"})
	customEditorDef := createCustomRoleDef(t, s, "rs3-eu-custom-editor", []string{"project.read", "project.write"})
	customBinding1 := snapshotBinding(t, s, createCustomBinding(t, s, customViewerDef.ID, targetID, projectID))
	customBinding2 := snapshotBinding(t, s, createCustomBinding(t, s, customEditorDef.ID, targetID, projectID))

	// Change membership from member to admin via explicit endpoint (not generic).
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
	require.Equal(t, http.StatusOK, rec.Code,
		"explicit membership update should succeed: %s", rec.Body.String())

	// Verify: 3 bindings remain (1 new built-in admin + 2 custom).
	bindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, bindings, 3, "expected 3 bindings after explicit update")

	// Built-in is now project-admin.
	var builtInCount int
	for _, b := range bindings {
		rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, rdErr)
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			builtInCount++
			assert.Equal(t, store.ProjectRoleAdmin, rd.Name,
				"built-in binding should be project-admin after explicit update")
		}
	}
	assert.Equal(t, 1, builtInCount, "exactly one built-in binding should exist")

	// Custom bindings preserved.
	assertBindingPreserved(t, s, customBinding1)
	assertBindingPreserved(t, s, customBinding2)
}

// ---------------------------------------------------------------------------
// RS3.3: Concurrent generic POST of different built-in roles — at most one
//        succeeds; the loser conflicts and does not replace the winner
// ---------------------------------------------------------------------------

// TestRS3_ConcurrentGenericPost_AtMostOneSucceeds verifies the race-safety
// of the create-only constraint under concurrent generic POST requests.
func TestRS3_ConcurrentGenericPost_AtMostOneSucceeds(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-race-proj")
	ownerID := tid("rs3-race-owner")
	targetID := tid("rs3-race-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-race-target@test.com",
		DisplayName: "RS3 Race Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	ownerUser := &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}

	const concurrency = 10
	type result struct {
		code int
		body string
	}

	results := make([]result, concurrency)
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Alternate between member and admin requests.
			var rd *store.RoleDefinition
			if idx%2 == 0 {
				rd = memberRD
			} else {
				rd = adminRD
			}
			rec := doRequestAsUser(t, srv, ownerUser,
				http.MethodPost, "/api/v1/admin/role-bindings",
				createRoleBindingRequest{
					RoleDefinitionID: rd.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      targetID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          projectID,
				})
			results[idx] = result{code: rec.Code, body: rec.Body.String()}
		}(i)
	}
	wg.Wait()

	// Count outcomes.
	var creates, conflicts, others int
	for _, r := range results {
		switch r.code {
		case http.StatusCreated:
			creates++
		case http.StatusConflict:
			conflicts++
		default:
			others++
			t.Logf("unexpected status %d: %s", r.code, r.body)
		}
	}

	// At most one create should succeed.
	assert.LessOrEqual(t, creates, 1,
		"at most one concurrent generic POST should succeed (got %d creates)", creates)
	// At least (concurrency - 1) should conflict.
	assert.GreaterOrEqual(t, conflicts, concurrency-1,
		"at least %d requests should conflict (got %d)", concurrency-1, conflicts)
	assert.Equal(t, 0, others, "no unexpected status codes")

	// Verify exactly one built-in membership exists.
	bindings := listProjectBindings(t, s, targetID, projectID)
	var builtInCount int
	for _, b := range bindings {
		rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, rdErr)
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			builtInCount++
		}
	}
	assert.Equal(t, 1, builtInCount,
		"exactly one built-in membership binding should exist after concurrent race")
}

// ---------------------------------------------------------------------------
// RS3.4: Exact duplicate built-in via generic POST → 409
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_ExactDuplicateBuiltIn_Conflict verifies that posting
// the exact same built-in role through the generic endpoint returns 409
// (not false 201 success).
func TestRS3_GenericPost_ExactDuplicateBuiltIn_Conflict(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-dup-proj")
	ownerID := tid("rs3-dup-owner")
	targetID := tid("rs3-dup-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-dup-target@test.com",
		DisplayName: "RS3 Dup Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add target as project-member via explicit endpoint.
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

	// Snapshot the existing binding.
	existingBindings := listProjectBindings(t, s, targetID, projectID)
	require.Len(t, existingBindings, 1)
	snapshot := snapshotBinding(t, s, existingBindings[0])

	// POST exact same built-in role via generic endpoint.
	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})

	// Must be 409, not false 201.
	assert.Equal(t, http.StatusConflict, rec.Code,
		"exact duplicate built-in via generic POST should return 409, got %d: %s",
		rec.Code, rec.Body.String())

	// Binding unchanged.
	assertBindingPreserved(t, s, snapshot)
}

// ---------------------------------------------------------------------------
// RS3.5: Distinct custom bindings via generic POST still coexist
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_DistinctCustom_Coexist verifies that the create-only
// constraint does not affect custom role bindings — multiple distinct custom
// bindings can still be created through the generic endpoint.
func TestRS3_GenericPost_DistinctCustom_Coexist(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-cust-proj")
	ownerID := tid("rs3-cust-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Create custom role definitions with permissions the owner holds
	// (to pass CanDelegate).
	customViewerDef := createCustomRoleDef(t, s, "rs3-cust-viewer", []string{"project.read"})
	customEditorDef := createCustomRoleDef(t, s, "rs3-cust-editor", []string{"project.read", "project.write"})

	// Look up a user to be the target — use a hub-admin so CanDelegate passes.
	adminUserID := tid("rs3-cust-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminUserID, Email: "rs3-cust-admin@test.com",
		DisplayName: "RS3 Admin", Role: "admin", Status: "active",
	}))
	ensureHubMembership(ctx, s, adminUserID)

	adminUser := &store.User{
		ID: adminUserID, Email: "rs3-cust-admin@test.com",
		DisplayName: "RS3 Admin", Role: "admin",
	}

	// Ensure the admin has hub-admin binding for CanDelegate.
	hubAdminDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUserID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create first custom binding via generic endpoint.
	rec := doRequestAsUser(t, srv, adminUser,
		http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: customViewerDef.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      ownerID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	require.Equal(t, http.StatusCreated, rec.Code,
		"first custom binding should succeed: %s", rec.Body.String())

	// Create second custom binding via generic endpoint.
	rec = doRequestAsUser(t, srv, adminUser,
		http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: customEditorDef.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      ownerID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	require.Equal(t, http.StatusCreated, rec.Code,
		"second custom binding should succeed: %s", rec.Body.String())

	// The owner should now have the built-in owner + 2 custom bindings.
	bindings := listProjectBindings(t, s, ownerID, projectID)
	assert.GreaterOrEqual(t, len(bindings), 3,
		"should have at least 3 bindings (1 built-in + 2 custom)")
}

// ---------------------------------------------------------------------------
// RS3.6: Last-owner guard via generic POST
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_LastOwnerGuard verifies that the last-owner guard
// correctly prevents demotion of the sole project owner, even through the
// generic endpoint, without mutating any existing bindings.
func TestRS3_GenericPost_LastOwnerGuard(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-lo-proj")
	ownerID := tid("rs3-lo-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Attempt to demote the sole owner by posting project-member via generic.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ownerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})

	// Should be 409 (create-only conflict — owner already has built-in role).
	assert.Equal(t, http.StatusConflict, rec.Code,
		"generic POST for sole owner with different built-in should return 409, got %d: %s",
		rec.Code, rec.Body.String())

	// Verify the owner still has project-owner.
	bindings := listProjectBindings(t, s, ownerID, projectID)
	var ownerRoleFound bool
	for _, b := range bindings {
		rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		require.NoError(t, rdErr)
		if rd.Name == store.ProjectRoleOwner {
			ownerRoleFound = true
		}
	}
	assert.True(t, ownerRoleFound, "owner must still have project-owner role")
}

// ---------------------------------------------------------------------------
// RS3.7: Delegation check still enforced on generic POST for built-in
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_DelegationCheck verifies that the CanDelegate check
// is enforced before the create-only path is reached — an actor without
// sufficient authority is denied.
func TestRS3_GenericPost_DelegationCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-del-proj")
	ownerID := tid("rs3-del-owner")
	memberID := tid("rs3-del-member")
	targetID := tid("rs3-del-target")

	createRS1Project(t, s, projectID, ownerID)

	// Create a project-member (not admin/owner).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "rs3-del-member@test.com",
		DisplayName: "RS3 Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, memberID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Add memberID as project-member via explicit endpoint.
	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      memberID,
		})
	require.Equal(t, http.StatusCreated, rec.Code)

	// Create target user.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-del-target@test.com",
		DisplayName: "RS3 Del Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Member tries to add target as admin via generic endpoint — should be denied
	// by governance (members cannot manage membership at all).
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec = doRequestAsUser(t, srv, &store.User{
		ID: memberID, Email: "rs3-del-member@test.com",
		DisplayName: "RS3 Member", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"member should be denied creating admin binding via generic POST, got %d: %s",
		rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS3.8: Credential-kind enforcement on generic POST
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_CredentialKind verifies that credential-kind
// enforcement is applied to the generic POST path (the membership service
// checks R2-2 credential kind before any mutation).
func TestRS3_GenericPost_CredentialKind(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-cred-proj")
	ownerID := tid("rs3-cred-owner")
	targetID := tid("rs3-cred-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-cred-target@test.com",
		DisplayName: "RS3 Cred Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create a member binding for the target.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Normal request from owner should succeed.
	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})

	// Should succeed (201) since this is the first built-in binding.
	assert.Equal(t, http.StatusCreated, rec.Code,
		"first built-in binding via generic POST should succeed: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS3.9: Audit record is NOT created on conflict (no mutation = no audit)
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_Conflict_NoAudit verifies that when the generic POST
// returns 409, no audit record is created (since no mutation occurred).
func TestRS3_GenericPost_Conflict_NoAudit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-aud-proj")
	ownerID := tid("rs3-aud-owner")
	targetID := tid("rs3-aud-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-aud-target@test.com",
		DisplayName: "RS3 Audit Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add target as project-member via explicit endpoint.
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

	// Count audit records before the conflicting request.
	auditsBefore, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 1000})
	require.NoError(t, err)

	// Attempt generic POST with different built-in.
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	require.Equal(t, http.StatusConflict, rec.Code)

	// Audit count should NOT have increased.
	auditsAfter, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 1000})
	require.NoError(t, err)
	assert.Equal(t, len(auditsBefore), len(auditsAfter),
		"no audit record should be created for a rejected generic POST conflict")
}

// ---------------------------------------------------------------------------
// RS3.10: Rollback regression — explicit endpoints still work
// ---------------------------------------------------------------------------

// TestRS3_ExplicitEndpoints_Unaffected verifies that explicit membership
// endpoints (POST /projects/:id/members for add, PATCH for update) are NOT
// affected by the create-only constraint.
func TestRS3_ExplicitEndpoints_Unaffected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-rb-proj")
	ownerID := tid("rs3-rb-owner")
	targetID := tid("rs3-rb-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-rb-target@test.com",
		DisplayName: "RS3 Rollback Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Add as member via explicit endpoint.
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

	// Replace member → admin via explicit endpoint (should still work).
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
	assert.Equal(t, http.StatusOK, rec.Code,
		"explicit membership replacement should still succeed: %s", rec.Body.String())

	// Idempotent re-add via explicit endpoint (should still work).
	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: adminRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      targetID,
		})
	assert.True(t, rec.Code >= 200 && rec.Code < 300,
		"explicit idempotent re-add should succeed: %s", rec.Body.String())

	// Parse binding ID for PATCH test.
	var info projectMemberInfo
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&info))

	// PATCH to change role back to member.
	rec = doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPatch, "/api/v1/projects/"+projectID+"/members/"+info.RoleBinding.ID,
		updateProjectMemberRequest{RoleDefinitionID: memberRD.ID})
	assert.Equal(t, http.StatusOK, rec.Code,
		"explicit PATCH should still succeed: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS3.11: Generic POST create-only for new member succeeds
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_NewMember_Succeeds verifies that when no built-in
// membership exists, the generic POST successfully creates one.
func TestRS3_GenericPost_NewMember_Succeeds(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-new-proj")
	ownerID := tid("rs3-new-owner")
	targetID := tid("rs3-new-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-new-target@test.com",
		DisplayName: "RS3 New Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	// Create via generic endpoint — should succeed since no built-in exists.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"generic POST for new member should succeed: %s", rec.Body.String())

	// Verify binding was created.
	var rb store.RoleBinding
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&rb))
	assert.Equal(t, memberRD.ID, rb.RoleDefinitionID)
	assert.Equal(t, targetID, rb.PrincipalID)
}

// ---------------------------------------------------------------------------
// RS3.12: Exact duplicate custom via generic POST still conflicts
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_ExactDuplicateCustom_Conflict verifies that exact
// duplicate custom bindings still conflict through the generic endpoint
// (unchanged from pre-RS3 behavior).
func TestRS3_GenericPost_ExactDuplicateCustom_Conflict(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-dupc-proj")
	ownerID := tid("rs3-dupc-owner")

	createRS1Project(t, s, projectID, ownerID)

	// Create a hub-admin user for CanDelegate.
	adminUserID := tid("rs3-dupc-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminUserID, Email: "rs3-dupc-admin@test.com",
		DisplayName: "RS3 DupC Admin", Role: "admin", Status: "active",
	}))
	ensureHubMembership(ctx, s, adminUserID)

	hubAdminDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      adminUserID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	adminUser := &store.User{
		ID: adminUserID, Email: "rs3-dupc-admin@test.com",
		DisplayName: "RS3 DupC Admin", Role: "admin",
	}

	customDef := createCustomRoleDef(t, s, "rs3-dupc-custom", []string{"project.read"})

	// First create — succeeds.
	rec := doRequestAsUser(t, srv, adminUser,
		http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: customDef.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      ownerID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	require.Equal(t, http.StatusCreated, rec.Code,
		"first custom binding should succeed: %s", rec.Body.String())

	// Exact duplicate — should conflict.
	rec = doRequestAsUser(t, srv, adminUser,
		http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: customDef.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      ownerID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
	assert.Equal(t, http.StatusConflict, rec.Code,
		"exact duplicate custom via generic POST should return 409: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// RS3.13: Generic POST with lifecycle fields on new member
// ---------------------------------------------------------------------------

// TestRS3_GenericPost_LifecycleFields verifies that lifecycle fields
// (NotBefore, ExpiresAt) are correctly passed through to the created
// binding when using the generic endpoint.
func TestRS3_GenericPost_LifecycleFields(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-lf-proj")
	ownerID := tid("rs3-lf-owner")
	targetID := tid("rs3-lf-target")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-lf-target@test.com",
		DisplayName: "RS3 LF Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	notBefore := time.Now().Add(1 * time.Hour).Truncate(time.Second)
	expiresAt := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	rec := doRequestAsUser(t, srv, &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      targetID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        &notBefore,
		ExpiresAt:        &expiresAt,
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"generic POST with lifecycle fields should succeed: %s", rec.Body.String())

	var rb store.RoleBinding
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&rb))
	require.NotNil(t, rb.NotBefore, "NotBefore should be set")
	require.NotNil(t, rb.ExpiresAt, "ExpiresAt should be set")
}

// ---------------------------------------------------------------------------
// RS3.14: Concurrent generic POST of same built-in role — exactly one
//         201, rest 409
// ---------------------------------------------------------------------------

// TestRS3_ConcurrentGenericPost_SameRole verifies that concurrent creates
// of the same built-in role are correctly serialized — exactly one succeeds.
func TestRS3_ConcurrentGenericPost_SameRole(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs3-csame-proj")
	ownerID := tid("rs3-csame-own")
	targetID := tid("rs3-csame-tgt")

	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: targetID, Email: "rs3-csame@test.com",
		DisplayName: "RS3 CSame Target", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, targetID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	ownerUser := &store.User{
		ID: ownerID, Email: ownerID + "@test.com",
		DisplayName: "Owner", Role: "member",
	}

	const concurrency = 8
	codes := make([]int, concurrency)
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := doRequestAsUser(t, srv, ownerUser,
				http.MethodPost, "/api/v1/admin/role-bindings",
				createRoleBindingRequest{
					RoleDefinitionID: memberRD.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      targetID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          projectID,
				})
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	var creates, conflicts int
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			creates++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status code: %d", code)
		}
	}

	assert.Equal(t, 1, creates,
		"exactly one concurrent create of same built-in should succeed")
	assert.Equal(t, concurrency-1, conflicts,
		"rest should conflict")

	// Verify exactly one binding.
	bindings := listProjectBindings(t, s, targetID, projectID)
	builtInCount := 0
	for _, b := range bindings {
		rd, _ := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if store.IsBuiltInProjectMembershipRole(rd.Name) {
			builtInCount++
		}
	}
	assert.Equal(t, 1, builtInCount, "exactly one built-in binding should exist")
}
