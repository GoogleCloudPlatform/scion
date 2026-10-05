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
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2769 PR1: the last-owner rule counts only usable
// owners (direct user project-owner binding, active window, user exists and
// is active) and is checked on the post-state; it also never drops the last
// owner binding of any kind (I2, ptone/scion#2554).

// usableOwnerCount returns the number of distinct principals on projectID
// that hold a usable owner binding. It goes through usableOwnerPrincipals,
// the same path projectHasUsableOwner (and so enforcement) uses.
func usableOwnerCount(t *testing.T, s store.Store, projectID string) int {
	t.Helper()
	ids, err := usableOwnerPrincipals(context.Background(), s, projectID, time.Now(), "", false)
	require.NoError(t, err)
	return len(ids)
}

// requireLastOwner409 asserts a 409 last_owner refusal.
func requireLastOwner409(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeLastOwner+`"`)
}

// ownerBindingIDs returns the IDs of every project-owner binding on the
// project (any principal, any window).
func ownerBindingIDs(t *testing.T, s store.Store, projectID string) []string {
	t.Helper()
	ctx := context.Background()
	ownerRDID, err := projectOwnerRoleDefinitionID(ctx, s)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	var ids []string
	for _, b := range bindings {
		if b.RoleDefinitionID == ownerRDID {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// createOwnerBindingRB is createOwnerBinding that returns the binding.
func createOwnerBindingRB(t *testing.T, s store.Store, principalID, projectID string, notBefore, expiresAt *time.Time) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	ownerRDID, err := projectOwnerRoleDefinitionID(ctx, s)
	require.NoError(t, err)
	b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRDID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		NotBefore:        notBefore,
		ExpiresAt:        expiresAt,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return b
}

// createGhostOwnerBinding creates a project-owner binding whose user row no
// longer exists: the user is created, bound, then deleted directly in the
// store (role_bindings.principal_id has no foreign key), as a user delete
// before the ptone/scion#2598 cascade could leave behind.
func createGhostOwnerBinding(t *testing.T, s store.Store, userID, projectID string) *store.RoleBinding {
	t.Helper()
	newUserWithStatus(t, s, userID, store.UserStatusActive)
	b := createOwnerBindingRB(t, s, userID, projectID, nil, nil)
	require.NoError(t, s.DeleteUser(context.Background(), userID))
	_, err := s.GetUser(context.Background(), userID)
	require.ErrorIs(t, err, store.ErrNotFound)
	return b
}

// newUserWithStatus creates a hub-member user with the given status.
func newUserWithStatus(t *testing.T, s store.Store, id, status string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{ID: id, Email: id + "@test.com", DisplayName: id,
		Role: store.UserRoleMember, Status: status, Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, u))
	ensureHubMembership(ctx, s, id)
	return u
}

// usableOwnerFixture is a project with one active owner (the actor) and,
// per case, a second owner binding that is not usable.
type usableOwnerFixture struct {
	srv          *Server
	s            store.Store
	owner        *store.User
	projectID    string
	ownerBinding *store.RoleBinding
	memberRD     *store.RoleDefinition
}

func setupUsableOwnerFixture(t *testing.T) *usableOwnerFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	ownerID := tid(t.Name() + "-owner")
	projectID := tid(t.Name() + "-project")
	createRS1Project(t, s, projectID, ownerID)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	var ownerBinding *store.RoleBinding
	for _, b := range projectBindingsFor(t, s, projectID, ownerID) {
		ownerBinding = b
	}
	require.NotNil(t, ownerBinding)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	return &usableOwnerFixture{srv: srv, s: s, owner: owner, projectID: projectID, ownerBinding: ownerBinding, memberRD: memberRD}
}

// addUnusableCoOwner adds a second owner binding of the given kind:
// "suspended" or "invited" (a real user in that status) or "deleted" (a
// ghost binding whose user row does not exist).
func (f *usableOwnerFixture) addUnusableCoOwner(t *testing.T, kind string) *store.RoleBinding {
	t.Helper()
	id := tid(t.Name() + "-co-" + kind)
	switch kind {
	case store.UserStatusSuspended, store.UserStatusInvited:
		newUserWithStatus(t, f.s, id, kind)
		return createOwnerBindingRB(t, f.s, id, f.projectID, nil, nil)
	case "deleted":
		return createGhostOwnerBinding(t, f.s, id, f.projectID)
	default:
		t.Fatalf("unknown co-owner kind %q", kind)
		return nil
	}
}

// demotionPaths are the four members-API ways the active owner can drop
// their own owner binding.
var demotionPaths = []struct {
	name string
	do   func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder
}{
	{"RemoveMember", func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder {
		return doRequestAsUser(t, f.srv, f.owner, http.MethodDelete,
			"/api/v1/projects/"+f.projectID+"/members/"+f.ownerBinding.ID, nil)
	}},
	{"UpdateMemberRole", func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder {
		return doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/projects/"+f.projectID+"/members/"+f.ownerBinding.ID,
			updateProjectMemberRequest{RoleDefinitionID: f.memberRD.ID})
	}},
	{"AddMemberReplace", func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder {
		return doRequestAsUser(t, f.srv, f.owner, http.MethodPost,
			"/api/v1/projects/"+f.projectID+"/members",
			addProjectMemberRequest{RoleDefinitionID: f.memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.owner.ID})
	}},
	{"SetMemberRolesPUT", func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder {
		return putMemberRoles(t, f.srv, f.owner, f.projectID, "user", f.owner.ID, []string{f.memberRD.ID}, nil)
	}},
}

// Tests 1 and 2: with the only other owner suspended, deleted (ghost
// binding) or invited, the last usable owner cannot remove or demote
// themselves through any members path.
//
// Revert proof: restoring the binding count (countActiveDirectProjectOwners,
// which counts the other owner's binding) makes every case succeed.
func TestLastOwner_UsableOwner_OtherOwnerUnusableDenied(t *testing.T) {
	for _, kind := range []string{store.UserStatusSuspended, "deleted", store.UserStatusInvited} {
		for _, path := range demotionPaths {
			t.Run(kind+"/"+path.name, func(t *testing.T) {
				f := setupUsableOwnerFixture(t)
				f.addUnusableCoOwner(t, kind)
				before := ownerBindingIDs(t, f.s, f.projectID)

				rec := path.do(t, f)
				requireLastOwner409(t, rec)

				assert.ElementsMatch(t, before, ownerBindingIDs(t, f.s, f.projectID), "denied request must change nothing")
				assert.Equal(t, 1, usableOwnerCount(t, f.s, f.projectID))
			})
		}
	}
}

// Test 3: one real owner plus an expired owner binding; removing the
// expired binding succeeds.
//
// Revert proof: restoring the pre-state guard (count<=1 of active bindings
// before the delete) returns 409.
func TestLastOwner_RemoveExpiredOwnerBindingAllowed(t *testing.T) {
	f := setupUsableOwnerFixture(t)
	other := newUserWithStatus(t, f.s, tid(t.Name()+"-expired"), store.UserStatusActive)
	past := time.Now().Add(-time.Hour)
	expired := createOwnerBindingRB(t, f.s, other.ID, f.projectID, nil, &past)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete,
		"/api/v1/projects/"+f.projectID+"/members/"+expired.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{f.ownerBinding.ID}, ownerBindingIDs(t, f.s, f.projectID))
}

// Test 4: one real owner plus a ghost owner binding; removing the ghost
// succeeds. (This case also passed before ptone/scion#2769: the old count
// included the ghost. The discriminating case is
// TestLastOwner_NoUsableOwner_RemoveGhostKeepsExpired.)
//
// Revert proof: making userIsActive turn ErrNotFound into an error (instead
// of "not usable") fails this test with 500 ("look up user …: not found").
func TestLastOwner_RemoveGhostOwnerBindingAllowed(t *testing.T) {
	f := setupUsableOwnerFixture(t)
	ghost := f.addUnusableCoOwner(t, "deleted")

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete,
		"/api/v1/projects/"+f.projectID+"/members/"+ghost.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{f.ownerBinding.ID}, ownerBindingIDs(t, f.s, f.projectID))
	assert.Equal(t, 1, usableOwnerCount(t, f.s, f.projectID))
}

// newNoOwnerProject creates a project with no owner bindings and a hub
// admin to act on it at the service level (see removeAsService).
func newNoOwnerProject(t *testing.T) (*Server, store.Store, string, *store.User) {
	t.Helper()
	srv, s := testServer(t)
	projectID := tid(t.Name() + "-project")
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{
		ID: projectID, Name: "No Usable Owner", Slug: tid("no-usable-owner"),
	}))
	hubAdmin := createHubAdminUser(t, s, tid(t.Name()+"-hub-admin"), "ha-"+projectID[:8]+"@test.com")
	return srv, s, projectID, hubAdmin
}

// L3(b) of review r1: a project whose only owner bindings are a ghost and an
// expired binding (no usable owner). Removing the ghost succeeds and the
// expired binding remains, because one owner binding of any kind is left.
//
// Revert proof: restoring the old pre-state count in RemoveMember (active
// user owner bindings before the delete <= 1 gives 409) returns 409 here,
// since the old count was 1 (the ghost; the expired binding is not active).
func TestLastOwner_NoUsableOwner_RemoveGhostKeepsExpired(t *testing.T) {
	srv, s, projectID, hubAdmin := newNoOwnerProject(t)
	ghost := createGhostOwnerBinding(t, s, tid(t.Name()+"-ghost"), projectID)
	expiredUser := newUserWithStatus(t, s, tid(t.Name()+"-expired"), store.UserStatusActive)
	past := time.Now().Add(-time.Hour)
	expired := createOwnerBindingRB(t, s, expiredUser.ID, projectID, nil, &past)

	_, denial := removeAsService(srv, hubAdmin, projectID, ghost.ID)
	require.Nil(t, denial, "removing the ghost binding must succeed: %+v", denial)
	assert.Equal(t, []string{expired.ID}, ownerBindingIDs(t, s, projectID))
}

// N3 of review r1: I2 counts owner bindings of any principal. A group
// owner binding that is the project's last owner binding cannot be removed.
// The store refuses new non-user owner bindings (ErrDirectUserOnly), so the
// binding is inserted with raw SQL, as a legacy row would exist.
func TestLastOwner_LastGroupOwnerBindingKept(t *testing.T) {
	srv, s, projectID, hubAdmin := newNoOwnerProject(t)
	ctx := context.Background()
	ownerRDID, err := projectOwnerRoleDefinitionID(ctx, s)
	require.NoError(t, err)
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store must expose DB()")
	groupBindingID := uuid.New().String()
	_, err = dbProvider.DB().ExecContext(ctx, `INSERT INTO role_bindings `+
		`(id, role_definition_id, principal_type, principal_id, scope_type, scope_id, membership_kind, created_by, created) `+
		`VALUES (?, ?, 'group', ?, 'project', ?, 'builtin', 'test', datetime('now'))`,
		groupBindingID, ownerRDID, uuid.New().String(), projectID)
	require.NoError(t, err)
	require.Equal(t, []string{groupBindingID}, ownerBindingIDs(t, s, projectID))

	_, denial := removeAsService(srv, hubAdmin, projectID, groupBindingID)
	requireLastOwnerDecision(t, denial)
	assert.Equal(t, []string{groupBindingID}, ownerBindingIDs(t, s, projectID))
}

// N3 of review r1: removing or demoting an unusable owner binding is
// allowed through every members path while a usable owner remains.
func TestLastOwner_UnusableOwnerBindingChangeAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder
		want int
	}{
		{"PATCH expired owner to member", func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder {
			b := f.addExpiredCoOwner(t)
			return doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
				"/api/v1/projects/"+f.projectID+"/members/"+b.ID,
				updateProjectMemberRequest{RoleDefinitionID: f.memberRD.ID})
		}, http.StatusOK},
		{"PUT expired owner to member", func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder {
			b := f.addExpiredCoOwner(t)
			return putMemberRoles(t, f.srv, f.owner, f.projectID, "user", b.PrincipalID, []string{f.memberRD.ID}, nil)
		}, http.StatusOK},
		{"DELETE principal on a ghost owner", func(t *testing.T, f *usableOwnerFixture) *httptest.ResponseRecorder {
			b := f.addUnusableCoOwner(t, "deleted")
			return deleteMemberRoles(t, f.srv, f.owner, f.projectID, "user", b.PrincipalID)
		}, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupUsableOwnerFixture(t)
			rec := tc.do(t, f)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			assert.Equal(t, []string{f.ownerBinding.ID}, ownerBindingIDs(t, f.s, f.projectID))
		})
	}
}

// addExpiredCoOwner adds an expired owner binding held by an active user.
func (f *usableOwnerFixture) addExpiredCoOwner(t *testing.T) *store.RoleBinding {
	t.Helper()
	u := newUserWithStatus(t, f.s, tid(t.Name()+"-expired"), store.UserStatusActive)
	past := time.Now().Add(-time.Hour)
	return createOwnerBindingRB(t, f.s, u.ID, f.projectID, nil, &past)
}

// removeAsService calls RemoveMember on the server's membership service as
// actor. Hub-override actors (hub role_binding authority, no project role)
// are exercised at the service level: the HTTP entry gate needs
// project.manage, which hub-admin does not hold (see mmrServiceCtx).
func removeAsService(srv *Server, actor *store.User, projectID, bindingID string) (*MembershipResult, *MembershipDecision) {
	return srv.membershipService.RemoveMember(mmrServiceCtx(actor.ID, actor.Email), MembershipRequest{
		Op: MembershipOpRemove, ProjectID: projectID, Actor: mmrServiceIdentity(actor.ID, actor.Email), BindingID: bindingID,
	})
}

// requireLastOwnerDecision asserts a service-level 409 last_owner denial.
func requireLastOwnerDecision(t *testing.T, denial *MembershipDecision) {
	t.Helper()
	require.NotNil(t, denial, "expected a last_owner denial")
	assert.Equal(t, http.StatusConflict, denial.HTTPStatus, denial.Reason)
	assert.Equal(t, ErrCodeLastOwner, denial.DenialCode)
}

// Test 5: a project whose only owners are a suspended user and an expired
// binding. A hub admin can remove the expired binding (no usable owner is
// removed, one owner binding remains), but not the suspended owner's
// binding, the last owner binding of any kind (I2).
//
// Revert proof: deleting the binding-count (I2) check lets the last binding
// be removed (204).
func TestLastOwner_NoUsableOwner_KeepsLastOwnerBinding(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid(t.Name() + "-project")
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{
		ID: projectID, Name: "No Usable Owner", Slug: "no-usable-owner",
	}))
	suspended := newUserWithStatus(t, s, tid(t.Name()+"-suspended"), store.UserStatusSuspended)
	suspendedBinding := createOwnerBindingRB(t, s, suspended.ID, projectID, nil, nil)
	expiredUser := newUserWithStatus(t, s, tid(t.Name()+"-expired"), store.UserStatusActive)
	past := time.Now().Add(-time.Hour)
	expiredBinding := createOwnerBindingRB(t, s, expiredUser.ID, projectID, nil, &past)
	hubAdmin := createHubAdminUser(t, s, tid(t.Name()+"-hub-admin"), "ha-"+projectID[:8]+"@test.com")

	_, denial := removeAsService(srv, hubAdmin, projectID, expiredBinding.ID)
	require.Nil(t, denial, "removing the expired binding must succeed: %+v", denial)

	_, denial = removeAsService(srv, hubAdmin, projectID, suspendedBinding.ID)
	requireLastOwnerDecision(t, denial)
	assert.Equal(t, []string{suspendedBinding.ID}, ownerBindingIDs(t, s, projectID),
		"the last owner binding must remain (ptone/scion#2554)")
}

// flipStatusStore flips targetID's status after the first GetUser of
// targetID outside a transaction, simulating a suspend (or delete) that
// commits between TransferOwnership's pre-check and its transaction.
type flipStatusStore struct {
	store.Store
	targetID string
	delete   bool
	flipped  bool
}

func (f *flipStatusStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	u, err := f.Store.GetUser(ctx, id)
	if err == nil && id == f.targetID && !f.flipped {
		f.flipped = true
		if f.delete {
			if dErr := f.DeleteUser(ctx, id); dErr != nil {
				return nil, dErr
			}
		} else {
			cp := *u
			cp.Status = store.UserStatusSuspended
			if uErr := f.UpdateUser(ctx, &cp); uErr != nil {
				return nil, uErr
			}
		}
	}
	return u, err
}

// Test 6a: the new owner is re-checked inside the transaction. Suspended
// after the pre-check gives 400 principal_ineligible; deleted gives 400
// not_found. Nothing changes.
//
// Revert proof: without the in-tx GetUser re-check the transfer still does
// not commit, but it returns the wrong code: 409 last_owner for the
// suspended case (the post-state check blocks it) and 500 for the deleted
// case (creating the owner binding fails), instead of 400.
func TestTransferOwnership_NewOwnerChangedAfterPreCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delete   bool
		wantCode string
	}{
		{"suspended", false, ErrCodePrincipalIneligible},
		{"deleted", true, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupUsableOwnerFixture(t)
			target := newUserWithStatus(t, f.s, tid(t.Name()+"-target"), store.UserStatusActive)
			flip := &flipStatusStore{Store: f.s, targetID: target.ID, delete: tc.delete}
			svc := NewProjectMembershipService(flip, NewAuthzService(f.s, nil), nil)

			actor := mmrServiceIdentity(f.owner.ID, f.owner.Email)
			ctx := mmrServiceCtx(f.owner.ID, f.owner.Email)
			_, denial := svc.TransferOwnership(ctx, MembershipRequest{
				Op: MembershipOpTransfer, ProjectID: f.projectID, Actor: actor, NewOwnerID: target.ID,
			})
			require.True(t, flip.flipped, "precondition: the status flip was injected")
			require.NotNil(t, denial)
			assert.Equal(t, http.StatusBadRequest, denial.HTTPStatus, denial.Reason)
			assert.Equal(t, tc.wantCode, denial.DenialCode)
			assert.Equal(t, []string{f.ownerBinding.ID}, ownerBindingIDs(t, f.s, f.projectID), "nothing may change")
		})
	}
}

// Test 6b: a transfer whose post-state has no usable owner is 409
// last_owner, not 500. The new owner's existing binding is expired; the
// replacement owner binding keeps that window, so it is not usable.
//
// Revert proof: restoring the plain post-state error gives 500.
func TestTransferOwnership_PostStateNoUsableOwnerIs409(t *testing.T) {
	f := setupUsableOwnerFixture(t)
	ctx := context.Background()
	target := newUserWithStatus(t, f.s, tid(t.Name()+"-target"), store.UserStatusActive)
	past := time.Now().Add(-time.Hour)
	_, err := f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target.ID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, ExpiresAt: &past, CreatedBy: "test",
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/projects/"+f.projectID+"/transfer-ownership",
		transferOwnershipRequest{NewOwnerID: target.ID})
	requireLastOwner409(t, rec)
	assert.Equal(t, []string{f.ownerBinding.ID}, ownerBindingIDs(t, f.s, f.projectID), "the transfer must roll back")
}

// Test 7: the user-delete guard uses the same rule.
//
// Revert proof: restoring countActiveDirectProjectOwners in the guard makes
// the suspended and ghost co-owner cases succeed (204).
func TestDeleteUser_UsableOwnerRule(t *testing.T) {
	t.Run("sole usable owner with suspended co-owner denied", func(t *testing.T) {
		srv, s, alice, bob, project := setupDemoPolicyTest(t)
		createOwnerBinding(t, s, bob.ID, project.ID, nil, nil)
		setUserStatus(t, s, bob.ID, store.UserStatusSuspended)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
		requireLastOwnerDenial(t, rec, project)
		_, err := s.GetUser(context.Background(), alice.ID)
		require.NoError(t, err)
		assert.Len(t, ownerBindingIDs(t, s, project.ID), 2)
	})
	t.Run("sole usable owner with ghost co-owner denied", func(t *testing.T) {
		srv, s, alice, _, project := setupDemoPolicyTest(t)
		createGhostOwnerBinding(t, s, tid("ghost-owner"), project.ID)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
		requireLastOwnerDenial(t, rec, project)
		_, err := s.GetUser(context.Background(), alice.ID)
		require.NoError(t, err)
		assert.Len(t, ownerBindingIDs(t, s, project.ID), 2)
	})
	t.Run("suspended co-owner with a real owner allowed", func(t *testing.T) {
		srv, s, alice, bob, project := setupDemoPolicyTest(t)
		createOwnerBinding(t, s, bob.ID, project.ID, nil, nil)
		setUserStatus(t, s, bob.ID, store.UserStatusSuspended)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+bob.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		requireSingleOwnerBinding(t, s, project.ID, alice.ID)
	})
	t.Run("suspended co-owner whose only other owner is suspended allowed", func(t *testing.T) {
		srv, s, alice, bob, project := setupDemoPolicyTest(t)
		createOwnerBinding(t, s, bob.ID, project.ID, nil, nil)
		setUserStatus(t, s, bob.ID, store.UserStatusSuspended)
		setUserStatus(t, s, alice.ID, store.UserStatusSuspended)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+bob.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		requireSingleOwnerBinding(t, s, project.ID, alice.ID)
	})
}

// faultingGetUserStore makes GetUser(failID) fail with a non-NotFound error
// inside transactions only, so pre-transaction checks pass.
type faultingGetUserStore struct {
	store.Store
	failID string
	inTx   bool
}

func (f *faultingGetUserStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&faultingGetUserStore{Store: tx, failID: f.failID, inTx: true})
	})
}

var errInjectedGetUser = errors.New("injected GetUser fault")

func (f *faultingGetUserStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if f.inTx && id == f.failID {
		return nil, errInjectedGetUser
	}
	return f.Store.GetUser(ctx, id)
}

// Test 8: a non-NotFound GetUser error in the predicate is 500 and changes
// nothing, for the members API and for the delete guard (D1).
//
// Revert proof: treating the lookup error as "not usable" turns the
// members case into 409 and the delete case into 409.
func TestLastOwner_GetUserFaultIs500(t *testing.T) {
	t.Run("members", func(t *testing.T) {
		f := setupUsableOwnerFixture(t)
		co := newUserWithStatus(t, f.s, tid(t.Name()+"-co"), store.UserStatusActive)
		createOwnerBindingRB(t, f.s, co.ID, f.projectID, nil, nil)
		before := ownerBindingIDs(t, f.s, f.projectID)

		faulty := &faultingGetUserStore{Store: f.s, failID: co.ID}
		svc := NewProjectMembershipService(faulty, NewAuthzService(f.s, nil), nil)
		actor := mmrServiceIdentity(f.owner.ID, f.owner.Email)
		ctx := mmrServiceCtx(f.owner.ID, f.owner.Email)
		_, denial := svc.RemoveMember(ctx, MembershipRequest{
			Op: MembershipOpRemove, ProjectID: f.projectID, Actor: actor, BindingID: f.ownerBinding.ID,
		})
		require.NotNil(t, denial)
		assert.Equal(t, http.StatusInternalServerError, denial.HTTPStatus, denial.Reason)
		assert.Contains(t, denial.Reason, errInjectedGetUser.Error())
		assert.ElementsMatch(t, before, ownerBindingIDs(t, f.s, f.projectID))
	})
	t.Run("delete guard", func(t *testing.T) {
		srv, s, alice, bob, project := setupDemoPolicyTest(t)
		createOwnerBinding(t, s, bob.ID, project.ID, nil, nil)

		srv.store = &faultingGetUserStore{Store: s, failID: bob.ID}
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+alice.ID, nil)
		srv.store = s
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		_, err := s.GetUser(context.Background(), alice.ID)
		require.NoError(t, err, "a failed delete must keep the user")
		assert.Len(t, ownerBindingIDs(t, s, project.ID), 2)
	})
}

// Test 9: a hub-override actor (hub role_binding authority, no project
// role) skips project governance but is still subject to the last-owner
// rule.
func TestLastOwner_HubOverrideActorStillSubject(t *testing.T) {
	f := setupUsableOwnerFixture(t)
	f.addUnusableCoOwner(t, store.UserStatusSuspended)
	hubAdmin := createHubAdminUser(t, f.s, tid(t.Name()+"-hub-admin"), "ha-"+f.projectID[:8]+"@test.com")

	_, denial := removeAsService(f.srv, hubAdmin, f.projectID, f.ownerBinding.ID)
	requireLastOwnerDecision(t, denial)

	_, denial = f.srv.membershipService.SetMemberRoles(mmrServiceCtx(hubAdmin.ID, hubAdmin.Email), SetMemberRolesRequest{
		ProjectID: f.projectID, Actor: mmrServiceIdentity(hubAdmin.ID, hubAdmin.Email),
		PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.owner.ID,
		DesiredRoleIDs: []string{f.memberRD.ID},
	})
	requireLastOwnerDecision(t, denial)
	assert.Equal(t, 1, usableOwnerCount(t, f.s, f.projectID))
}

// preTxFaultStore makes GetUser(failID) fail with a non-NotFound error
// outside transactions only (WithTx passes the real store through).
type preTxFaultStore struct {
	store.Store
	failID string
}

func (f *preTxFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == f.failID {
		return nil, errInjectedGetUser
	}
	return f.Store.GetUser(ctx, id)
}

// L6 and N3 of review r1: a non-NotFound GetUser fault on the new owner is
// 500 internal_error, not 400 not_found, both in the pre-transaction check
// and in the in-transaction re-check. Nothing changes.
//
// Revert proof (pre-tx): mapping every pre-check error to 400 not_found (the
// old code) fails the "pre-tx" case with 400.
func TestTransferOwnership_GetUserFaultIs500(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(s store.Store, id string) store.Store
	}{
		{"pre-tx", func(s store.Store, id string) store.Store { return &preTxFaultStore{Store: s, failID: id} }},
		{"in-tx", func(s store.Store, id string) store.Store { return &faultingGetUserStore{Store: s, failID: id} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupUsableOwnerFixture(t)
			target := newUserWithStatus(t, f.s, tid(t.Name()+"-target"), store.UserStatusActive)
			svc := NewProjectMembershipService(tc.wrap(f.s, target.ID), NewAuthzService(f.s, nil), nil)

			_, denial := svc.TransferOwnership(mmrServiceCtx(f.owner.ID, f.owner.Email), MembershipRequest{
				Op: MembershipOpTransfer, ProjectID: f.projectID,
				Actor: mmrServiceIdentity(f.owner.ID, f.owner.Email), NewOwnerID: target.ID,
			})
			require.NotNil(t, denial)
			assert.Equal(t, http.StatusInternalServerError, denial.HTTPStatus, denial.Reason)
			assert.Equal(t, "internal_error", denial.DenialCode)
			assert.Contains(t, denial.Reason, errInjectedGetUser.Error())
			assert.Equal(t, []string{f.ownerBinding.ID}, ownerBindingIDs(t, f.s, f.projectID), "nothing may change")
		})
	}
}
