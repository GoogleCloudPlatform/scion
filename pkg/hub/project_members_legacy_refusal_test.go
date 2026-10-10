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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Byte-compatibility pins for the legacy single-binding members endpoints
// (ptone/scion#2600).
//
// POST /api/v1/projects/{id}/members and PATCH …/members/{bindingID} build
// their "actor has no project role" and CanDelegate refusals through the
// shared constructors in project_membership_set.go (noProjectRoleDecision,
// canDelegateRefusal / canDelegateRefusalFor), which the multi-role PUT and
// assignable-roles also use. These tests pin the exact legacy HTTP response
// body — status, code, message and details — so that sharing the
// constructors cannot silently change what the legacy endpoints return. The
// bodies were captured from the code before the refactor.
// =============================================================================

// legacyMemberCanDelegateReason is the CanDelegate refusal reason the
// hub-override actor (custom project.read+project.manage role plus system
// hub-admin) gets when delegating a built-in role it does not hold all
// permissions of. CanDelegate names the first permission of the target
// role's list (seed.go) the actor lacks; the built-in project roles list
// artifact.create first.
const legacyMemberCanDelegateReason = "actor lacks permission for delegation: artifact.create"

func legacyMembersPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/members"
}

// assertLegacyBody asserts the exact response status and body bytes.
func assertLegacyBody(t *testing.T, gotStatus int, gotBody string, wantStatus int, wantBody string) {
	t.Helper()
	assert.Equal(t, wantStatus, gotStatus, gotBody)
	assert.Equal(t, wantBody, gotBody, "legacy refusal body must stay byte-compatible")
}

func TestAddMember_LegacyPOST_NoProjectRoleRefusalBody(t *testing.T) {
	f := setupMMRFixture(t)
	actor, _ := asgManagerActor(t, f, "noprojectrole", false)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")

	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, legacyMembersPath(f.projectID), map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID, "principalType": "user", "principalId": target.ID,
	})
	assertLegacyBody(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor has no project role"}}`+"\n")
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", target.ID, f.projectID))
}

func TestAddMember_LegacyPOST_CanDelegateRefusalBody(t *testing.T) {
	f := setupMMRFixture(t)
	actor, _ := asgHubOverrideActor(t, f)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")

	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, legacyMembersPath(f.projectID), map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID, "principalType": "user", "principalId": target.ID,
	})
	assertLegacyBody(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"target_role_protected","message":"actor cannot delegate the requested role: `+legacyMemberCanDelegateReason+`"}}`+"\n")
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", target.ID, f.projectID))
}

// TestAddMember_LegacyRoleBindingsPOST_CanDelegateRefusalBody pins the same
// refusal reached through the generic role-binding POST, which routes
// built-in project roles to AddMember and attaches its own 403 details.
func TestAddMember_LegacyRoleBindingsPOST_CanDelegateRefusalBody(t *testing.T) {
	f := setupMMRFixture(t)
	actor, _ := asgHubOverrideActor(t, f)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")

	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/admin/role-bindings", map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID, "principalType": "user", "principalId": target.ID,
		"scopeType": store.RoleScopeProject, "scopeId": f.projectID,
	})
	assertLegacyBody(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"target_role_protected","message":"actor cannot delegate the requested role: `+legacyMemberCanDelegateReason+`","details":{"denied_action":"create","resource_type":"role_binding"}}}`+"\n")
	assert.Empty(t, mmrBindingsFor(t, f.store, "user", target.ID, f.projectID))
}

func legacyMemberBindingID(t *testing.T, f *mmrFixture) string {
	t.Helper()
	bindings := mmrBindingsFor(t, f.store, "user", f.member.ID, f.projectID)
	require.Len(t, bindings, 1)
	return bindings[0].ID
}

func TestUpdateMember_LegacyPATCH_NoProjectRoleRefusalBody(t *testing.T) {
	f := setupMMRFixture(t)
	actor, _ := asgManagerActor(t, f, "noprojectrole", false)
	bindingID := legacyMemberBindingID(t, f)

	rec := doRequestAsUser(t, f.srv, actor, http.MethodPatch, legacyMembersPath(f.projectID)+"/"+bindingID, map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID,
	})
	assertLegacyBody(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor has no project role"}}`+"\n")
	assert.Equal(t, bindingID, legacyMemberBindingID(t, f), "the member binding is unchanged")
}

func TestUpdateMember_LegacyPATCH_CanDelegateRefusalBody(t *testing.T) {
	f := setupMMRFixture(t)
	actor, _ := asgHubOverrideActor(t, f)
	bindingID := legacyMemberBindingID(t, f)

	rec := doRequestAsUser(t, f.srv, actor, http.MethodPatch, legacyMembersPath(f.projectID)+"/"+bindingID, map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID,
	})
	assertLegacyBody(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"target_role_protected","message":"actor cannot delegate the new role: `+legacyMemberCanDelegateReason+`"}}`+"\n")
	assert.Equal(t, bindingID, legacyMemberBindingID(t, f), "the member binding is unchanged")
}

// TestUpdateMember_LegacyPATCH_NoProjectRoleUnderLockRefusal pins the
// in-transaction "actor has no project role (re-evaluated under lock)"
// refusal of UpdateMemberRole. The actor is a hub admin with no project
// role demoting a co-owner (a decrease, so no CanDelegate); between the
// pre-transaction checks and the lock, mmrAuthoritySwapStore revokes the
// actor's hub-admin binding. The legacy handler renders only status, code
// and message, so the decision's DenialCode, Reason, HTTPStatus and an empty
// Details are the whole legacy body.
func TestUpdateMember_LegacyPATCH_NoProjectRoleUnderLockRefusal(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	hubAdminID := tid(t.Name() + "-hubadmin")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: hubAdminID, Email: hubAdminID + "@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, f.store, hubAdminID)
	mmrSeedHubAdmin(t, f.store, hubAdminID)

	coOwnerID := tid(t.Name() + "-co-owner")
	createRS1UserWithRole(t, f.store, coOwnerID, coOwnerID+"@test.com", f.projectID, store.ProjectRoleOwner)
	before := mmrBindingsFor(t, f.store, "user", coOwnerID, f.projectID)
	require.Len(t, before, 1)

	realStore := f.srv.membershipService.store
	sw := &mmrAuthoritySwapStore{Store: realStore}
	sw.swap = func() { mmrDeleteSystemHubAdminBindings(t, realStore, hubAdminID) }
	f.srv.membershipService.store = sw
	defer func() { f.srv.membershipService.store = realStore }()

	_, decision := f.srv.membershipService.UpdateMemberRole(mmrServiceCtx(hubAdminID, hubAdminID+"@test.com"), MembershipRequest{
		Op:           MembershipOpUpdate,
		ProjectID:    f.projectID,
		Actor:        mmrServiceIdentity(hubAdminID, hubAdminID+"@test.com"),
		BindingID:    before[0].ID,
		NewRoleDefID: f.memberRD.ID,
	})
	require.True(t, sw.didSwap, "the swap seam must have run: %+v", decision)
	require.NotNil(t, decision)
	assert.Equal(t, MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodeRoleAssignmentForbidden,
		Reason:     "actor has no project role (re-evaluated under lock)",
		HTTPStatus: http.StatusForbidden,
	}, *decision)
	assert.Equal(t, roleDefIDs(before), roleDefIDs(mmrBindingsFor(t, realStore, "user", coOwnerID, f.projectID)), "the co-owner binding is unchanged")
}

// mmrDeleteSystemSuperAdminBindings revokes the user's system-scope
// super-admin binding directly in the store, modelling an already-committed
// concurrent revocation.
func mmrDeleteSystemSuperAdminBindings(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	superAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	deleted := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == superAdmin.ID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
			deleted++
		}
	}
	require.Equal(t, 1, deleted, "expected exactly one system super-admin binding to revoke")
}

// TestAddMember_LegacyPOST_NoProjectRoleUnderLockRefusal pins the
// in-transaction "actor has no project role (re-evaluated under lock)"
// refusal of AddMember (cleanup review r1 C1-1), the twin of
// TestUpdateMember_LegacyPATCH_NoProjectRoleUnderLockRefusal. The actor is
// a non-member super-admin: it passes the pre-transaction governance check
// through the hub override and CanDelegate for project-member (super-admin
// can delegate any authority), so the request reaches the lock. Between the
// pre-transaction checks and the lock, mmrAuthoritySwapStore revokes the
// actor's system super-admin binding, so the in-transaction hub-authority
// revalidation fails and AddMember must refuse with the exact under-lock
// decision before anything is written. A hub-admin actor cannot be used
// here: CanDelegate refuses it pre-transaction (it lacks the role's permissions).
func TestAddMember_LegacyPOST_NoProjectRoleUnderLockRefusal(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	superID := tid(t.Name() + "-super")
	createTestUserWithRole(t, f.store, superID, superID+"@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, f.store, superID)
	require.Empty(t, mmrBindingsFor(t, f.store, "user", superID, f.projectID), "the super-admin holds no project role")

	target := grpUser(t, f.store, t.Name()+"-target", "Target")

	realStore := f.srv.membershipService.store
	sw := &mmrAuthoritySwapStore{Store: realStore}
	sw.swap = func() { mmrDeleteSystemSuperAdminBindings(t, realStore, superID) }
	f.srv.membershipService.store = sw
	defer func() { f.srv.membershipService.store = realStore }()

	_, decision := f.srv.membershipService.AddMember(mmrServiceCtx(superID, superID+"@test.com"), MembershipRequest{
		Op:            MembershipOpAdd,
		ProjectID:     f.projectID,
		Actor:         mmrServiceIdentity(superID, superID+"@test.com"),
		PrincipalType: "user",
		PrincipalID:   target.ID,
		RoleDefID:     f.memberRD.ID,
	})
	require.True(t, sw.didSwap, "the swap seam must have run: %+v", decision)
	require.NotNil(t, decision)
	assert.Equal(t, MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodeRoleAssignmentForbidden,
		Reason:     "actor has no project role (re-evaluated under lock)",
		HTTPStatus: http.StatusForbidden,
	}, *decision)
	assert.Empty(t, mmrBindingsFor(t, realStore, "user", target.ID, f.projectID), "no binding was created")
}
