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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Byte-compatibility pins for project governance refusals (ptone/scion#2695).
//
// RemoveMember (members DELETE and the generic role-binding DELETE),
// TransferOwnership and project deletion refuse requests both before the
// transaction and inside it, after re-evaluating authority under the project
// lock. AddMember and UpdateMemberRole share the in-transaction refusal
// channel. These tests pin the exact HTTP response body (status, code,
// message and details) of each refusal, so that moving the in-transaction
// refusals from "governance:STATUS:REASON" strings to the typed
// governanceDenialError cannot silently change what clients receive. The
// bodies were captured from the code before that refactor.
//
// In-transaction refusals are reached through three seams:
//   - mmrAuthoritySwapStore (the swap-store seam) commits a concurrent change
//     (revocation, demotion, deletion) between the pre-transaction checks and
//     the lock. Most in-transaction pins use it.
//   - pinNoDirectOwnerStore (the owner-hiding seam) presents a different
//     project-owner role-definition ID to the transaction. The five
//     in-transaction "only direct project owners" branches cannot be reached
//     with valid data, so their pins (*DirectOwnerUnderLock) need this seam.
//   - pinFailingPrincipalListStore (the failing-store seam) makes the
//     in-transaction authority lookup fail, for the deletion 500 pin.
// =============================================================================

// pinRefusal asserts the exact status and body bytes of a refusal.
func pinRefusal(t *testing.T, gotStatus int, gotBody string, wantStatus int, wantBody string) {
	t.Helper()
	require.Equal(t, wantStatus, gotStatus, gotBody)
	require.Equal(t, wantBody, gotBody, "governance refusal body must stay byte-compatible")
}

// pinSwapMembershipStore installs a swap seam on the membership service
// store; swap runs once, right before the service opens its transaction.
func pinSwapMembershipStore(t *testing.T, f *mmrFixture, swap func(s store.Store)) *mmrAuthoritySwapStore {
	t.Helper()
	realStore := f.srv.membershipService.store
	sw := &mmrAuthoritySwapStore{Store: realStore}
	sw.swap = func() { swap(realStore) }
	f.srv.membershipService.store = sw
	t.Cleanup(func() { f.srv.membershipService.store = realStore })
	return sw
}

// pinSwapDeletionStore is pinSwapMembershipStore for the deletion service.
func pinSwapDeletionStore(t *testing.T, f *mmrFixture, swap func(s store.Store)) *mmrAuthoritySwapStore {
	t.Helper()
	realStore := f.srv.deletionService.store
	sw := &mmrAuthoritySwapStore{Store: realStore}
	sw.swap = func() { swap(realStore) }
	f.srv.deletionService.store = sw
	t.Cleanup(func() { f.srv.deletionService.store = realStore })
	return sw
}

// pinBindingID returns the user's single direct binding in the project.
func pinBindingID(t *testing.T, s store.Store, userID, projectID string) string {
	t.Helper()
	bindings := mmrBindingsFor(t, s, "user", userID, projectID)
	require.Len(t, bindings, 1)
	return bindings[0].ID
}

// pinDeleteDirectBindings deletes every direct project binding of the user.
func pinDeleteDirectBindings(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	for _, b := range mmrBindingsFor(t, s, "user", userID, projectID) {
		require.NoError(t, s.DeleteRoleBinding(context.Background(), b.ID))
	}
}

// pinSetDirectRole replaces the user's direct project bindings with one
// binding of rd.
func pinSetDirectRole(t *testing.T, s store.Store, userID, projectID string, rd *store.RoleDefinition) {
	t.Helper()
	pinDeleteDirectBindings(t, s, userID, projectID)
	grpBind(t, s, "user", userID, rd.ID, projectID)
}

// pinNoDirectOwnerStore resolves the project-owner role definition to a
// different ID on the transactional store only. Inside the transaction the
// actor's effective role stays "project-owner" (it is resolved through the
// binding's own role definition ID) while isActorDirectOwnerFromStore no
// longer recognises the binding as a direct owner binding. With valid data
// the two never disagree (group-derived ownership is rejected by the store
// and ignored by projectEffectiveRoleFromStore), so this seam is the only way
// to reach the in-transaction "only direct project owners" refusals.
type pinNoDirectOwnerStore struct {
	store.Store
}

type pinNoDirectOwnerTx struct {
	store.Store
}

func (s *pinNoDirectOwnerStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&pinNoDirectOwnerTx{Store: tx})
	})
}

func (tx *pinNoDirectOwnerTx) GetRoleDefinitionByName(ctx context.Context, name, scopeType string) (*store.RoleDefinition, error) {
	rd, err := tx.Store.GetRoleDefinitionByName(ctx, name, scopeType)
	if err != nil || name != store.ProjectRoleOwner {
		return rd, err
	}
	hidden := *rd
	hidden.ID = "pin-hidden-" + rd.ID
	return &hidden, nil
}

// pinHideDirectOwnership installs pinNoDirectOwnerStore on the membership
// service.
func pinHideDirectOwnership(t *testing.T, f *mmrFixture) {
	t.Helper()
	realStore := f.srv.membershipService.store
	f.srv.membershipService.store = &pinNoDirectOwnerStore{Store: realStore}
	t.Cleanup(func() { f.srv.membershipService.store = realStore })
}

// pinCoOwner creates a second direct project owner.
func pinCoOwner(t *testing.T, f *mmrFixture) *store.User {
	t.Helper()
	u := grpUser(t, f.store, t.Name()+"-co-owner", "Co Owner")
	grpBind(t, f.store, "user", u.ID, f.ownerRD.ID, f.projectID)
	return u
}

// pinHubAdmin creates a hub admin that holds no project role.
func pinHubAdmin(t *testing.T, f *mmrFixture) *store.User {
	t.Helper()
	u := grpUser(t, f.store, t.Name()+"-hubadmin", "Hub Admin")
	mmrSeedHubAdmin(t, f.store, u.ID)
	return u
}

func pinMemberPath(projectID, bindingID string) string {
	return legacyMembersPath(projectID) + "/" + bindingID
}

func pinTransferPath(projectID string) string {
	return "/api/v1/projects/" + projectID + "/transfer-ownership"
}

func pinProjectPath(projectID string) string {
	return "/api/v1/projects/" + projectID
}

// -----------------------------------------------------------------------------
// RemoveMember: members DELETE and the generic role-binding DELETE
// -----------------------------------------------------------------------------

func TestRemoveMemberRefusalBody_NoProjectRoleUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	actor := pinHubAdmin(t, f)
	bindingID := pinBindingID(t, f.store, f.member.ID, f.projectID)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) { mmrDeleteSystemHubAdminBindings(t, s, actor.ID) })

	rec := doRequestAsUser(t, f.srv, actor, http.MethodDelete, "/api/v1/admin/role-bindings/"+bindingID, nil)
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor has no project role (re-evaluated under lock)","details":{"denied_action":"delete","resource_type":"role_binding"}}}`+"\n")
}

func TestRemoveMemberRefusalBody_BindingNotFoundUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	bindingID := pinBindingID(t, f.store, f.member.ID, f.projectID)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) {
		require.NoError(t, s.DeleteRoleBinding(context.Background(), bindingID))
	})

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, pinMemberPath(f.projectID, bindingID), nil)
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusNotFound,
		`{"error":{"code":"not_found","message":"binding not found (re-fetched under lock)"}}`+"\n")
}

func TestRemoveMemberRefusalBody_ActorRoleUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	bindingID := pinBindingID(t, f.store, f.member.ID, f.projectID)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) { pinSetDirectRole(t, s, f.admin.ID, f.projectID, f.memberRD) })

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, pinMemberPath(f.projectID, bindingID), nil)
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor role \"project-member\" cannot remove target role \"project-member\" (re-evaluated under lock)"}}`+"\n")
}

func TestRemoveMemberRefusalBody_DirectOwnerUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	bindingID := pinBindingID(t, f.store, f.admin.ID, f.projectID)
	pinHideDirectOwnership(t, f)

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodDelete, pinMemberPath(f.projectID, bindingID), nil)
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"only direct project owners can manage admin and owner roles (re-evaluated under lock)"}}`+"\n")
}

func TestRemoveMemberRefusalBody_AdminCannotRemoveOwner(t *testing.T) {
	f := setupMMRFixture(t)
	bindingID := pinBindingID(t, f.store, f.owner.ID, f.projectID)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, pinMemberPath(f.projectID, bindingID), nil)
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"target_role_protected","message":"actor role \"project-admin\" cannot remove target role \"project-owner\""}}`+"\n")
}

func TestRemoveMemberRefusalBody_LastOwner(t *testing.T) {
	f := setupMMRFixture(t)
	bindingID := pinBindingID(t, f.store, f.owner.ID, f.projectID)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, pinMemberPath(f.projectID, bindingID), nil)
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusConflict,
		`{"error":{"code":"last_owner","message":"cannot remove or demote the last project owner — at least one active direct user owner must remain"}}`+"\n")
}

// -----------------------------------------------------------------------------
// TransferOwnership
// -----------------------------------------------------------------------------

func TestTransferOwnershipRefusalBody_NoLongerOwnerUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) { pinDeleteDirectBindings(t, s, f.owner.ID, f.projectID) })

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, pinTransferPath(f.projectID), map[string]interface{}{"newOwnerId": f.member.ID})
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor is no longer a direct project owner (re-evaluated under lock)"}}`+"\n")
}

func TestTransferOwnershipRefusalBody_NotDirectOwner(t *testing.T) {
	f := setupMMRFixture(t)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodPost, pinTransferPath(f.projectID), map[string]interface{}{"newOwnerId": f.member.ID})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"only direct project owners can transfer ownership"}}`+"\n")
}

func TestTransferOwnershipRefusalBody_SelfTransfer(t *testing.T) {
	f := setupMMRFixture(t)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, pinTransferPath(f.projectID), map[string]interface{}{"newOwnerId": f.owner.ID})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusConflict,
		`{"error":{"code":"conflict","message":"cannot transfer ownership to yourself"}}`+"\n")
}

func TestTransferOwnershipRefusalBody_UnknownTarget(t *testing.T) {
	f := setupMMRFixture(t)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, pinTransferPath(f.projectID), map[string]interface{}{"newOwnerId": tid(t.Name() + "-nobody")})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusBadRequest,
		`{"error":{"code":"not_found","message":"target user not found"}}`+"\n")
}

// -----------------------------------------------------------------------------
// Project deletion
// -----------------------------------------------------------------------------

func TestDeleteProjectRefusalBody_NotDirectOwnerUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	sw := pinSwapDeletionStore(t, f, func(s store.Store) { pinDeleteDirectBindings(t, s, f.owner.ID, f.projectID) })

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, pinProjectPath(f.projectID), nil)
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"forbidden","message":"actor is not a direct project owner (re-evaluated under lock)"}}`+"\n")
	_, err := f.store.GetProject(context.Background(), f.projectID)
	require.NoError(t, err, "the project is not deleted")
}

// pinFailingPrincipalListStore makes ListRoleBindingsForPrincipal fail on
// the transactional store only, so the deletion service's under-lock
// authority lookup fails.
type pinFailingPrincipalListStore struct {
	store.Store
}

type pinFailingPrincipalListTx struct {
	store.Store
}

func (s *pinFailingPrincipalListStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&pinFailingPrincipalListTx{Store: tx})
	})
}

func (tx *pinFailingPrincipalListTx) ListRoleBindingsForPrincipal(context.Context, string, string) ([]*store.RoleBinding, error) {
	return nil, errors.New("injected principal binding lookup failure")
}

func TestDeleteProjectRefusalBody_AuthorityLookupFailedUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	realStore := f.srv.deletionService.store
	f.srv.deletionService.store = &pinFailingPrincipalListStore{Store: realStore}
	t.Cleanup(func() { f.srv.deletionService.store = realStore })

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, pinProjectPath(f.projectID), nil)
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusInternalServerError,
		`{"error":{"code":"forbidden","message":"authority lookup failed under lock"}}`+"\n")
}

func TestDeleteProjectRefusalBody_AdminIsNotDirectOwner(t *testing.T) {
	f := setupMMRFixture(t)

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, pinProjectPath(f.projectID), nil)
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"forbidden","message":"insufficient permissions for project deletion"}}`+"\n")
}

func TestDeleteProjectRefusalBody_HubAdmin(t *testing.T) {
	f := setupMMRFixture(t)
	actor := pinHubAdmin(t, f)

	rec := doRequestAsUser(t, f.srv, actor, http.MethodDelete, pinProjectPath(f.projectID), nil)
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"forbidden","message":"insufficient permissions for project deletion"}}`+"\n")
}

// -----------------------------------------------------------------------------
// AddMember and UpdateMemberRole share the in-transaction refusal channel
// -----------------------------------------------------------------------------

func TestAddMemberRefusalBody_ActorRoleUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")
	sw := pinSwapMembershipStore(t, f, func(s store.Store) { pinSetDirectRole(t, s, coOwner.ID, f.projectID, f.adminRD) })

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodPost, legacyMembersPath(f.projectID), map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID, "principalType": "user", "principalId": target.ID,
	})
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor role \"project-admin\" cannot add target role \"project-admin\" (re-evaluated under lock)"}}`+"\n")
}

func TestAddMemberRefusalBody_DirectOwnerUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")
	pinHideDirectOwnership(t, f)

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodPost, legacyMembersPath(f.projectID), map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID, "principalType": "user", "principalId": target.ID,
	})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"only direct project owners can manage admin and owner roles (re-evaluated under lock)"}}`+"\n")
}

func TestAddMemberRefusalBody_ExistingRoleProtectedUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")
	grpBind(t, f.store, "user", target.ID, f.memberRD.ID, f.projectID)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) { pinSetDirectRole(t, s, target.ID, f.projectID, f.adminRD) })

	rec := doRequestAsUser(t, f.srv, f.admin, http.MethodPost, legacyMembersPath(f.projectID), map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID, "principalType": "user", "principalId": target.ID,
	})
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"target role is protected: actor role \"project-admin\" cannot add target role \"project-admin\""}}`+"\n")
}

func TestAddMemberRefusalBody_ExistingRoleDirectOwnerUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	target := grpUser(t, f.store, t.Name()+"-target", "Target")
	grpBind(t, f.store, "user", target.ID, f.adminRD.ID, f.projectID)
	pinHideDirectOwnership(t, f)

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodPost, legacyMembersPath(f.projectID), map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID, "principalType": "user", "principalId": target.ID,
	})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"only direct project owners can manage admin and owner roles"}}`+"\n")
}

func TestAddMemberRefusalBody_CreateOnlyDuplicate(t *testing.T) {
	f := setupMMRFixture(t)
	actor := grpUser(t, f.store, t.Name()+"-super", "Super")
	pinGrantSuperAdmin(t, f.store, actor.ID)

	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/admin/role-bindings", map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID, "principalType": "user", "principalId": f.member.ID,
		"scopeType": store.RoleScopeProject, "scopeId": f.projectID,
	})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusConflict,
		`{"error":{"code":"conflict","message":"principal already has built-in membership role \"project-member\" in this project"}}`+"\n")
}

func TestAddMemberRefusalBody_CreateOnlyDifferentRole(t *testing.T) {
	f := setupMMRFixture(t)
	actor := grpUser(t, f.store, t.Name()+"-super", "Super")
	pinGrantSuperAdmin(t, f.store, actor.ID)

	rec := doRequestAsUser(t, f.srv, actor, http.MethodPost, "/api/v1/admin/role-bindings", map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID, "principalType": "user", "principalId": f.member.ID,
		"scopeType": store.RoleScopeProject, "scopeId": f.projectID,
	})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusConflict,
		`{"error":{"code":"conflict","message":"principal already has built-in membership role \"project-member\" in this project; use the project membership endpoint to change roles"}}`+"\n")
}

// pinGrantSuperAdmin grants an existing user system super-admin.
func pinGrantSuperAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	saRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: saRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
}

func TestUpdateMemberRefusalBody_BindingNotFoundUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	bindingID := pinBindingID(t, f.store, f.member.ID, f.projectID)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) {
		require.NoError(t, s.DeleteRoleBinding(context.Background(), bindingID))
	})

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, pinMemberPath(f.projectID, bindingID), map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID,
	})
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusNotFound,
		`{"error":{"code":"not_found","message":"binding not found (re-fetched under lock)"}}`+"\n")
}

func TestUpdateMemberRefusalBody_OldRoleUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	bindingID := pinBindingID(t, f.store, f.admin.ID, f.projectID)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) { pinSetDirectRole(t, s, coOwner.ID, f.projectID, f.adminRD) })

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodPatch, pinMemberPath(f.projectID, bindingID), map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID,
	})
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor role \"project-admin\" cannot update target role \"project-admin\" (re-evaluated under lock)"}}`+"\n")
}

func TestUpdateMemberRefusalBody_NewRoleUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	bindingID := pinBindingID(t, f.store, f.member.ID, f.projectID)
	sw := pinSwapMembershipStore(t, f, func(s store.Store) { pinSetDirectRole(t, s, coOwner.ID, f.projectID, f.adminRD) })

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodPatch, pinMemberPath(f.projectID, bindingID), map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID,
	})
	require.True(t, sw.didSwap, rec.Body.String())
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"actor role \"project-admin\" cannot update target role \"project-admin\" (re-evaluated under lock)"}}`+"\n")
}

func TestUpdateMemberRefusalBody_OldRoleDirectOwnerUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	bindingID := pinBindingID(t, f.store, f.admin.ID, f.projectID)
	pinHideDirectOwnership(t, f)

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodPatch, pinMemberPath(f.projectID, bindingID), map[string]interface{}{
		"roleDefinitionId": f.memberRD.ID,
	})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"only direct project owners can manage admin and owner roles (re-evaluated under lock)"}}`+"\n")
}

func TestUpdateMemberRefusalBody_NewRoleDirectOwnerUnderLock(t *testing.T) {
	f := setupMMRFixture(t)
	coOwner := pinCoOwner(t, f)
	bindingID := pinBindingID(t, f.store, f.member.ID, f.projectID)
	pinHideDirectOwnership(t, f)

	rec := doRequestAsUser(t, f.srv, coOwner, http.MethodPatch, pinMemberPath(f.projectID, bindingID), map[string]interface{}{
		"roleDefinitionId": f.adminRD.ID,
	})
	pinRefusal(t, rec.Code, rec.Body.String(), http.StatusForbidden,
		`{"error":{"code":"role_assignment_forbidden","message":"only direct project owners can manage admin and owner roles (re-evaluated under lock)"}}`+"\n")
}
