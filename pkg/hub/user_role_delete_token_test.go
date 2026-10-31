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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// urdMember creates an active member with hub membership and no direct
// role binding, as a member is represented outside tests.
func urdMember(t *testing.T, s store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: id, Email: id + "@test.com", DisplayName: name,
		Role: store.UserRoleMember, Status: store.UserStatusActive}))
	ensureHubMembership(ctx, s, id)
	return id
}

// urdViewer creates an active viewer represented canonically: the
// unconditional hub-viewer binding and no hub membership.
func urdViewer(t *testing.T, s store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: id, Email: id + "@test.com", DisplayName: name,
		Role: store.UserRoleViewer, Status: store.UserStatusActive}))
	require.NoError(t, ensureHubViewerBindingTx(ctx, s, id, "test"))
	return id
}

// urdSystemBinding binds userID to a role definition at system scope,
// valid until expiresAt when it is non-nil.
func urdSystemBinding(t *testing.T, s store.Store, userID, roleDefinitionID string, expiresAt *time.Time) {
	t.Helper()
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleDefinitionID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test", ExpiresAt: expiresAt,
	})
	require.NoError(t, err)
}

// urdCustomSystemRole creates a custom system-scope role definition and
// returns its ID.
func urdCustomSystemRole(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	now := time.Now()
	_, err := s.CreateRoleDefinition(context.Background(), &store.RoleDefinition{ID: id, Name: id,
		Description: "custom system role", ScopeType: store.RoleScopeSystem, Permissions: []string{"agent.read"},
		CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	return id
}

// urdRoleDefinitionID returns the ID of a built-in system role definition.
func urdRoleDefinitionID(t *testing.T, s store.Store, name string) string {
	t.Helper()
	rd, err := s.GetRoleDefinitionByName(context.Background(), name, store.RoleScopeSystem)
	require.NoError(t, err)
	require.NotNil(t, rd)
	return rd.ID
}

// urdHoldsBinding reports whether userID holds a system-scope binding of
// the named role definition.
func urdHoldsBinding(t *testing.T, s store.Store, userID, roleDefinitionID string) bool {
	t.Helper()
	bindings, err := s.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == roleDefinitionID {
			return true
		}
	}
	return false
}

// urdUserExists reports whether the user record exists.
func urdUserExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetUser(context.Background(), id)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		return false
	}
	return true
}

// requireSystemBindingRefusal asserts the token delete refusal for a user
// holding a direct system-scope role binding.
func requireSystemBindingRefusal(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	code, details := errorBody(t, rec)
	assert.Equal(t, ErrCodeForbidden, code, label)
	assert.Equal(t, userDeleteSystemBindingReason, details["reason"], "%s: details.reason", label)
}

// TestUserRoleToken_PromotionToAdminDenied pins the role field of PATCH
// /api/v1/users/{id} for a token: a hub token with user:promote never
// makes a user admin, from member or viewer, because CanDelegate refuses
// the super-admin binding for every token; nothing is written. A token
// without user:promote gets the fixed refusal, user:promote cannot be
// minted on a project boundary, and a project-boundary credential carrying
// it is refused. A session promotes the same user.
func TestUserRoleToken_PromotionToAdminDenied(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "urd-promo-super", store.SystemRoleSuperAdmin)
	superAdminRD := urdRoleDefinitionID(t, s, store.SystemRoleSuperAdmin)
	member := urdMember(t, s, "urd-promo-member")
	viewer := urdViewer(t, s, "urd-promo-viewer")

	promote := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:promote")
	for _, target := range []string{member, viewer} {
		before := upUser(t, s, target).Role
		rec := doRequestWithToken(t, srv, promote, http.MethodPatch, "/api/v1/users/"+target, map[string]string{"role": store.UserRoleAdmin})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s to admin: %s", before, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "insufficient authority to modify super-admin role binding")
		assert.Equal(t, before, upUser(t, s, target).Role, "%s: role kept", before)
		assert.False(t, urdHoldsBinding(t, s, target, superAdminRD), "%s: no super-admin binding", before)
	}

	// Without user:promote: the fixed refusal for every id.
	read := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:read")
	requireUpTokenRefusal(t, doRequestWithToken(t, srv, read, http.MethodPatch, "/api/v1/users/"+member,
		map[string]string{"role": store.UserRoleViewer}), "user:read only")
	assert.Equal(t, store.UserRoleMember, upUser(t, s, member).Role)

	// user:promote is hub-only.
	igMintRefused(t, srv, admin, projectBoundary(igProject(t, s, "urd-promo-project")), "user:promote")
	projectCredential := upProjectCredential(t, srv, s, admin, "urd-promo-project-sel", "user:promote")
	rec := doRequestAsIdentity(t, srv, projectCredential, http.MethodPatch, "/api/v1/users/"+member, map[string]string{"role": store.UserRoleViewer})
	assert.Equal(t, http.StatusForbidden, rec.Code, "project credential: %s", rec.Body.String())
	assert.Equal(t, store.UserRoleMember, upUser(t, s, member).Role)

	// A session promotes.
	rec = doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodPatch, "/api/v1/users/"+member, map[string]string{"role": store.UserRoleAdmin})
	require.Equal(t, http.StatusOK, rec.Code, "session: %s", rec.Body.String())
	assert.True(t, urdHoldsBinding(t, s, member, superAdminRD))
}

// TestUserRoleToken_MemberViewerChange pins that a hub token with
// user:promote changes member to viewer and viewer to member, with the
// role's canonical representation: the unconditional hub-viewer binding
// for viewer, hub membership for member.
func TestUserRoleToken_MemberViewerChange(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "urd-mv-super", store.SystemRoleSuperAdmin)
	target := urdMember(t, s, "urd-mv-target")
	path := "/api/v1/users/" + target
	promote := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:promote")

	rec := doRequestWithToken(t, srv, promote, http.MethodPatch, path, map[string]string{"role": store.UserRoleViewer})
	require.Equal(t, http.StatusOK, rec.Code, "member to viewer: %s", rec.Body.String())
	assert.Equal(t, store.UserRoleViewer, upUser(t, s, target).Role)
	_, bindings, unconditional, err := hubViewerBindingsForUser(context.Background(), s, target)
	require.NoError(t, err)
	assert.Len(t, bindings, 1)
	assert.True(t, unconditional, "the canonical hub-viewer binding")

	rec = doRequestWithToken(t, srv, promote, http.MethodPatch, path, map[string]string{"role": store.UserRoleMember})
	require.Equal(t, http.StatusOK, rec.Code, "viewer to member: %s", rec.Body.String())
	assert.Equal(t, store.UserRoleMember, upUser(t, s, target).Role)
	_, bindings, _, err = hubViewerBindingsForUser(context.Background(), s, target)
	require.NoError(t, err)
	assert.Empty(t, bindings, "no hub-viewer binding for a member")
}

// TestUserRoleToken_LastSuperAdminProtected pins that a token removes no
// super-admin authority: a hub token with user:promote and user:delete
// cannot demote or delete a super-admin (CanDelegate, and the token delete
// rule for a direct system-scope binding, system_role_binding_held), and
// the holder's own role is session-only. Every refusal writes nothing. The
// last super-admin guard applies to sessions.
func TestUserRoleToken_LastSuperAdminProtected(t *testing.T) {
	srv, s := testServer(t)
	holder := hubConfigTokenUser(t, s, "urd-last-holder", store.SystemRoleSuperAdmin)
	other := hubConfigTokenUser(t, s, "urd-last-other", store.SystemRoleSuperAdmin)
	superAdminRD := urdRoleDefinitionID(t, s, store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, holder, hubBoundary(), "user:promote", "user:delete")

	for _, role := range []string{store.UserRoleMember, store.UserRoleViewer} {
		rec := doRequestWithToken(t, srv, key, http.MethodPatch, "/api/v1/users/"+other, map[string]string{"role": role})
		assert.Equal(t, http.StatusForbidden, rec.Code, "demote to %s: %s", role, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "insufficient authority to modify super-admin role binding")
	}
	rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+other, nil)
	requireSystemBindingRefusal(t, rec, "delete a super-admin")
	rec = doRequestWithToken(t, srv, key, http.MethodPatch, "/api/v1/users/"+holder, map[string]string{"role": store.UserRoleMember})
	requireSessionOnlyRefusal(t, rec, authzop.ReasonInteractiveState, "own role")

	for _, id := range []string{holder, other} {
		assert.Equal(t, store.UserRoleAdmin, upUser(t, s, id).Role)
		assert.True(t, urdHoldsBinding(t, s, id, superAdminRD))
	}

	// A session demotes the other super-admin; the guards still refuse
	// the session's own demotion.
	rec = doRequestAsUser(t, srv, upUser(t, s, holder), http.MethodPatch, "/api/v1/users/"+other, map[string]string{"role": store.UserRoleMember})
	require.Equal(t, http.StatusOK, rec.Code, "session demotes another super-admin: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, upUser(t, s, holder), http.MethodPatch, "/api/v1/users/"+holder, map[string]string{"role": store.UserRoleMember})
	assert.Equal(t, http.StatusConflict, rec.Code, "session self-demotion: %s", rec.Body.String())
	assert.True(t, urdHoldsBinding(t, s, holder, superAdminRD))
}

// TestUserPatchToken_MixedRoleAndStatusAllOrNothing pins that a token PATCH
// naming role and status needs user:promote and user:suspend before any
// write: either alone writes nothing; both write both fields; and a role
// change CanDelegate refuses (to admin) writes nothing, the status
// included.
func TestUserPatchToken_MixedRoleAndStatusAllOrNothing(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "urd-mixed-super", store.SystemRoleSuperAdmin)
	target := urdMember(t, s, "urd-mixed-target")
	path := "/api/v1/users/" + target
	mixed := map[string]string{"role": store.UserRoleViewer, "status": store.UserStatusSuspended}

	for _, sel := range []string{"user:promote", "user:suspend"} {
		key := mintHubConfigToken(t, srv, admin, hubBoundary(), sel)
		rec := doRequestWithToken(t, srv, key, http.MethodPatch, path, mixed)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s alone: %s", sel, rec.Body.String())
		got := upUser(t, s, target)
		assert.Equal(t, store.UserRoleMember, got.Role, "%s alone writes no role", sel)
		assert.Equal(t, store.UserStatusActive, got.Status, "%s alone writes no status", sel)
	}

	both := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:promote", "user:suspend")
	rec := doRequestWithToken(t, srv, both, http.MethodPatch, path, map[string]string{"role": store.UserRoleAdmin, "status": store.UserStatusSuspended})
	assert.Equal(t, http.StatusForbidden, rec.Code, "admin with status: %s", rec.Body.String())
	got := upUser(t, s, target)
	assert.Equal(t, store.UserRoleMember, got.Role)
	assert.Equal(t, store.UserStatusActive, got.Status, "the refused role change writes no status")

	rec = doRequestWithToken(t, srv, both, http.MethodPatch, path, mixed)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = upUser(t, s, target)
	assert.Equal(t, store.UserRoleViewer, got.Role)
	assert.Equal(t, store.UserStatusSuspended, got.Status)
}

// TestUserDeleteToken_LastProjectOwnerProtected pins that a token delete
// keeps the last-project-owner guard: a hub token with user:delete gets
// 409 last_owner for the only owner of a project and writes nothing; once
// the project has another owner, the delete succeeds. The self-delete
// guard applies to a token too.
func TestUserDeleteToken_LastProjectOwnerProtected(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "urd-owner-super", store.SystemRoleSuperAdmin)
	target := urdMember(t, s, "urd-owner-target")
	project := igProject(t, s, "urd-owner-project")
	igProjectBinding(t, s, target, project, store.ProjectRoleOwner)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:delete")

	rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+target, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeLastOwner, code)
	assert.True(t, urdUserExists(t, s, target))

	rec = doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+admin, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, "self-delete: %s", rec.Body.String())
	assert.True(t, urdUserExists(t, s, admin))

	igProjectBinding(t, s, urdMember(t, s, "urd-owner-second"), project, store.ProjectRoleOwner)
	rec = doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+target, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, urdUserExists(t, s, target))
}

// TestUserDeleteToken_SystemBindingHolderRefused pins the token delete rule
// for direct system-scope role bindings: a hub token with user:delete
// deletes a member and a canonical viewer (the unconditional hub-viewer
// binding of a viewer is the representation of the role), and is refused
// with 403 details.reason system_role_binding_held, writing nothing, for a
// user who holds any other direct system-scope binding: a custom system
// role, a time-limited hub-viewer binding, or hub-admin; with force=true
// as without it. A token without user:delete is refused before the rule,
// and a session deletes a refused user.
func TestUserDeleteToken_SystemBindingHolderRefused(t *testing.T) {
	srv, s := testServer(t)
	enableUserDeleteForce(t, srv)
	admin := hubConfigTokenUser(t, s, "urd-sys-super", store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:delete")
	later := time.Now().Add(24 * time.Hour)

	customRole := urdCustomSystemRole(t, s, "urd-sys-custom-role")
	withCustom := urdMember(t, s, "urd-sys-custom")
	urdSystemBinding(t, s, withCustom, customRole, nil)
	viewerWithCustom := urdViewer(t, s, "urd-sys-viewer-custom")
	urdSystemBinding(t, s, viewerWithCustom, customRole, nil)
	timedViewer := tid("urd-sys-timed-viewer")
	require.NoError(t, s.CreateUser(context.Background(), &store.User{ID: timedViewer, Email: timedViewer + "@test.com",
		DisplayName: "timed viewer", Role: store.UserRoleViewer, Status: store.UserStatusActive}))
	urdSystemBinding(t, s, timedViewer, urdRoleDefinitionID(t, s, store.SystemRoleHubViewer), &later)
	hubAdmin := urdMember(t, s, "urd-sys-hub-admin")
	urdSystemBinding(t, s, hubAdmin, urdRoleDefinitionID(t, s, store.SystemRoleHubAdmin), nil)

	for _, tc := range []struct{ label, id string }{
		{"custom system role", withCustom},
		{"viewer with a custom system role", viewerWithCustom},
		{"time-limited hub-viewer binding", timedViewer},
		{"hub-admin", hubAdmin},
	} {
		for _, suffix := range []string{"", "?force=true"} {
			rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+tc.id+suffix, nil)
			requireSystemBindingRefusal(t, rec, tc.label+suffix)
			assert.True(t, urdUserExists(t, s, tc.id), "%s%s: user kept", tc.label, suffix)
		}
	}

	read := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:read")
	rec := doRequestWithToken(t, srv, read, http.MethodDelete, "/api/v1/users/"+withCustom, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "requires user.delete permission")

	for _, id := range []string{urdMember(t, s, "urd-sys-member"), urdViewer(t, s, "urd-sys-viewer")} {
		rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+id, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, "%s: %s", id, rec.Body.String())
		assert.False(t, urdUserExists(t, s, id))
	}

	rec = doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodDelete, "/api/v1/users/"+withCustom, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "session: %s", rec.Body.String())
}

// TestUserDeleteToken_SystemBindingRecheckedInTransaction pins the token
// delete rule as judged on the bindings the cascade deletes
// (tokenDeleteBindingsCheck): a direct system-scope binding other than the
// canonical hub-viewer binding of a viewer returns errBindingStateDrift,
// and a member or canonical viewer passes.
func TestUserDeleteToken_SystemBindingRecheckedInTransaction(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	member := urdMember(t, s, "urd-drift-member")
	viewer := urdViewer(t, s, "urd-drift-viewer")
	customRole := urdCustomSystemRole(t, s, "urd-drift-role")
	listed := func(id string) []*store.RoleBinding {
		bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, id)
		require.NoError(t, err)
		return bindings
	}

	for _, id := range []string{member, viewer} {
		require.NoError(t, tokenDeleteBindingsCheck(ctx, s, id, listed(id)), id)
	}
	for _, id := range []string{member, viewer} {
		urdSystemBinding(t, s, id, customRole, nil)
		require.ErrorIs(t, tokenDeleteBindingsCheck(ctx, s, id, listed(id)), errBindingStateDrift, id)
	}
}

// urdHiddenBindingsStore hides the direct system-scope role bindings of
// one user from reads outside a transaction, and serves a saved copy of
// that user's record to GetUser outside a transaction when user is set.
// Inside WithTx every read goes straight to the store. It makes a
// binding, or a deletion, appear between a handler's pre-check reads and
// its transaction. It is installed with installStoreFault right after the
// server is built and stays transparent until hide arms it; stop ends it.
type urdHiddenBindingsStore struct {
	store.Store
	fault  *storeFaultSwitch
	done   *storeFaultSwitch
	userID string
	user   *store.User
}

// urdInstallHidden installs a disarmed urdHiddenBindingsStore on srv. Call
// it right after building srv, before any setup through srv.
func urdInstallHidden(t *testing.T, srv *Server) *urdHiddenBindingsStore {
	t.Helper()
	w, _ := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *urdHiddenBindingsStore {
		return &urdHiddenBindingsStore{Store: inner, fault: f, done: &storeFaultSwitch{}}
	})
	return w
}

// hide sets the user to hide (and the saved record to serve when user is
// set), then arms the wrapper. The fields are written before Arm, and the
// methods read them only after Active, so the switch orders them.
func (h *urdHiddenBindingsStore) hide(userID string, user *store.User) {
	h.userID, h.user = userID, user
	h.fault.Arm()
}

// stop makes the wrapper transparent again.
func (h *urdHiddenBindingsStore) stop() { h.done.Arm() }

func (h *urdHiddenBindingsStore) active() bool { return h.fault.Active() && !h.done.Active() }

func (h *urdHiddenBindingsStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	bindings, err := h.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
	if err != nil || !h.active() || principalType != store.RoleBindingPrincipalUser || principalID != h.userID {
		return bindings, err
	}
	var out []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType != store.RoleScopeSystem {
			out = append(out, b)
		}
	}
	return out, nil
}

func (h *urdHiddenBindingsStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if h.active() && h.user != nil && id == h.userID {
		u := *h.user
		return &u, nil
	}
	return h.Store.GetUser(ctx, id)
}

// requireBindingStateDrift asserts the 409 conflict with details.reason
// binding_state_drift.
func requireBindingStateDrift(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, "%s: %s", label, rec.Body.String())
	code, details := errorBody(t, rec)
	assert.Equal(t, ErrCodeConflict, code, label)
	assert.Equal(t, bindingStateDriftReason, details["reason"], "%s: details.reason", label)
}

// TestUserDeleteToken_BindingAddedAfterPreCheckRefused pins the
// in-transaction token rule through the handlers: when a system-scope
// binding is invisible to the pre-check and present in the transaction, a
// token's plain delete and a token's forced delete both answer 409 conflict
// with details.reason binding_state_drift, and the user, the binding and
// the user's agents are kept, with no deletion recorded.
func TestUserDeleteToken_BindingAddedAfterPreCheckRefused(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		srv, s := testServer(t)
		hidden := urdInstallHidden(t, srv)
		admin := hubConfigTokenUser(t, s, "urd-late-super", store.SystemRoleSuperAdmin)
		target := urdMember(t, s, "urd-late-target")
		customRole := urdCustomSystemRole(t, s, "urd-late-role")
		urdSystemBinding(t, s, target, customRole, nil)
		key := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:delete")
		hidden.hide(target, nil)

		rec := doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+target, nil)
		requireBindingStateDrift(t, rec, "plain delete")
		assert.True(t, urdUserExists(t, s, target))
		assert.True(t, urdHoldsBinding(t, s, target, customRole))
	})
	t.Run("force", func(t *testing.T) {
		f := newForceDelFixture(t, time.Hour)
		// The fixture's setup writes through the raw store only, so the
		// wrapper is installed before anything reads the server's store in
		// the background.
		hidden := urdInstallHidden(t, f.srv)
		a := f.agent(t, "urd-late-a", nil)
		admin := hubConfigTokenUser(t, f.s, "urd-late-force-super", store.SystemRoleSuperAdmin)
		igProjectBinding(t, f.s, admin, f.project.ID, store.ProjectRoleOwner)
		customRole := urdCustomSystemRole(t, f.s, "urd-late-force-role")
		urdSystemBinding(t, f.s, f.dave.ID, customRole, nil)
		key := mintHubConfigToken(t, f.srv, admin, hubBoundary(), "user:delete", "agent:delete")
		hidden.hide(f.dave.ID, nil)

		rec := doRequestWithToken(t, f.srv, key, http.MethodDelete, "/api/v1/users/"+f.dave.ID+"?force=true", nil)
		requireBindingStateDrift(t, rec, "forced delete")
		u, err := f.s.GetUser(context.Background(), f.dave.ID)
		require.NoError(t, err)
		assert.Equal(t, store.UserStatusActive, u.Status)
		assert.True(t, urdHoldsBinding(t, f.s, f.dave.ID, customRole))
		_, err = f.s.GetUserDeletionByUser(context.Background(), f.dave.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "no deletion recorded")
		assert.True(t, mustGetAgent(t, f.s, a.ID).DeletedAt.IsZero(), "the agent is kept")
	})
}

// TestUserDeleteToken_ConcurrentlyDeletedUserNotFound pins that a user who
// is gone by the time the delete transaction runs answers 404 on both
// delete paths: for a token's and a session's plain delete, and for a
// forced delete.
func TestUserDeleteToken_ConcurrentlyDeletedUserNotFound(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		srv, s := testServer(t)
		hidden := map[bool]*urdHiddenBindingsStore{true: urdInstallHidden(t, srv), false: urdInstallHidden(t, srv)}
		admin := hubConfigTokenUser(t, s, "urd-gone-super", store.SystemRoleSuperAdmin)
		key := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:delete")
		for _, viaToken := range []bool{true, false} {
			target := urdMember(t, s, fmt.Sprintf("urd-gone-target-%t", viaToken))
			saved := upUser(t, s, target)
			require.NoError(t, s.DeleteUser(context.Background(), target))
			hidden[viaToken].hide(target, saved)
			var rec *httptest.ResponseRecorder
			if viaToken {
				rec = doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/users/"+target, nil)
			} else {
				rec = doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodDelete, "/api/v1/users/"+target, nil)
			}
			assert.Equal(t, http.StatusNotFound, rec.Code, "token=%t: %s", viaToken, rec.Body.String())
			hidden[viaToken].stop()
		}
	})
	t.Run("force", func(t *testing.T) {
		f := newForceDelFixture(t, time.Hour)
		// The fixture's setup writes through the raw store only.
		hidden := urdInstallHidden(t, f.srv)
		f.agent(t, "urd-gone-a", nil)
		saved := upUser(t, f.s, f.dave.ID)
		require.NoError(t, f.s.DeleteUser(context.Background(), f.dave.ID))
		hidden.hide(f.dave.ID, saved)
		rec := f.forceDelete(t)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})
}

// TestUserDeleteToken_ForcedCompletionKeepsSystemBinding pins the
// completion of a forced deletion that a user access token requested: while
// the user holds a direct system-scope role binding the token's delete rule
// refuses, the completion fails with errTokenDeletionSystemBindingHeld and
// keeps the user and the binding; once the binding is gone it completes.
// A deletion a session requested completes with the binding.
func TestUserDeleteToken_ForcedCompletionKeepsSystemBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	customRole := urdCustomSystemRole(t, s, "urd-complete-role")
	deletion := func(userID, credentialKind string) *store.UserDeletion {
		d := &store.UserDeletion{UserID: userID, RequestedByKind: userDeletionRequesterKind, RequestedByID: DevUserID,
			CredentialKind: credentialKind, RequestedAt: time.Now()}
		require.NoError(t, s.CreateUserDeletion(ctx, d))
		return d
	}

	byToken := urdMember(t, s, "urd-complete-token")
	urdSystemBinding(t, s, byToken, customRole, nil)
	d := deletion(byToken, userDeletionTokenCredentialKind)
	_, err := srv.completeUserDeletion(ctx, d)
	require.ErrorIs(t, err, errTokenDeletionSystemBindingHeld)
	assert.True(t, urdUserExists(t, s, byToken))
	assert.True(t, urdHoldsBinding(t, s, byToken, customRole))

	_, err = s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, byToken)
	require.NoError(t, err)
	completed, err := srv.completeUserDeletion(ctx, d)
	require.NoError(t, err)
	assert.True(t, completed)
	assert.False(t, urdUserExists(t, s, byToken))

	bySession := urdMember(t, s, "urd-complete-session")
	urdSystemBinding(t, s, bySession, customRole, nil)
	completed, err = srv.completeUserDeletion(ctx, deletion(bySession, userDeletionCredentialKind))
	require.NoError(t, err)
	assert.True(t, completed)
	assert.False(t, urdUserExists(t, s, bySession))
}

// TestUserDeleteToken_ForceUsesTokenAgentDelete pins a token's forced
// delete: the agent.delete check on each of the user's agents is decided
// on the token, so a hub token with user:delete and no agent:delete gets
// 403 user_delete_force_forbidden and writes nothing, and one that also
// carries agent:delete starts the forced deletion (202).
func TestUserDeleteToken_ForceUsesTokenAgentDelete(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	f.agent(t, "urd-force-a", nil)
	admin := hubConfigTokenUser(t, f.s, "urd-force-super", store.SystemRoleSuperAdmin)
	igProjectBinding(t, f.s, admin, f.project.ID, store.ProjectRoleOwner)
	path := "/api/v1/users/" + f.dave.ID + "?force=true"

	deleteOnly := mintHubConfigToken(t, f.srv, admin, hubBoundary(), "user:delete")
	rec := doRequestWithToken(t, f.srv, deleteOnly, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeUserDeleteForceForbidden, code)
	_, err := f.s.GetUserDeletionByUser(context.Background(), f.dave.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "nothing written")

	withAgentDelete := mintHubConfigToken(t, f.srv, admin, hubBoundary(), "user:delete", "agent:delete")
	rec = doRequestWithToken(t, f.srv, withAgentDelete, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, f.dave.ID, decodeDeletion(t, rec).UserID)
}

// TestUserDeleteReadsToken_RequireDeleteSelectorAndHubBoundary pins GET
// /api/v1/users/{id}/delete-blockers and GET /api/v1/users/{id}/deletion
// for tokens: a hub token with user:delete reads both, and gets 404 for an
// unknown user; a token without user:delete, and a project-boundary
// credential carrying it, are refused.
func TestUserDeleteReadsToken_RequireDeleteSelectorAndHubBoundary(t *testing.T) {
	srv, s := testServer(t)
	enableUserDeleteForce(t, srv)
	admin := hubConfigTokenUser(t, s, "urd-reads-super", store.SystemRoleSuperAdmin)
	target := urdMember(t, s, "urd-reads-target")
	require.NoError(t, s.CreateUserDeletion(context.Background(), &store.UserDeletion{UserID: target,
		RequestedByKind: "user", RequestedByID: admin, CredentialKind: "session", RequestedAt: time.Now()}))
	unknown := tid("urd-reads-unknown")

	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:delete")
	read := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:read")
	projectCredential := upProjectCredential(t, srv, s, admin, "urd-reads-project", "user:delete")
	for _, sub := range []string{"/delete-blockers", "/deletion"} {
		path := "/api/v1/users/" + target + sub
		rec := doRequestWithToken(t, srv, key, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", sub, rec.Body.String())
		rec = doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/users/"+unknown+sub, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s unknown: %s", sub, rec.Body.String())
		rec = doRequestWithToken(t, srv, read, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s user:read: %s", sub, rec.Body.String())
		rec = doRequestAsIdentity(t, srv, projectCredential, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s project credential: %s", sub, rec.Body.String())
	}
}

// TestUserRoleToken_BindingDriftConflictForEveryCredential pins the role
// path's answer when a user's super-admin bindings changed after the
// authorization check: for a session and for a hub token with
// user:promote, a demotion whose super-admin binding is invisible to the
// pre-check and present in the transaction answers 409 conflict with
// details.reason binding_state_drift and writes nothing.
func TestUserRoleToken_BindingDriftConflictForEveryCredential(t *testing.T) {
	srv, s := testServer(t)
	hidden := map[bool]*urdHiddenBindingsStore{false: urdInstallHidden(t, srv), true: urdInstallHidden(t, srv)}
	holder := hubConfigTokenUser(t, s, "urd-rdrift-holder", store.SystemRoleSuperAdmin)
	superAdminRD := urdRoleDefinitionID(t, s, store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, holder, hubBoundary(), "user:promote")

	for _, viaToken := range []bool{false, true} {
		target := hubConfigTokenUser(t, s, fmt.Sprintf("urd-rdrift-target-%t", viaToken), store.SystemRoleSuperAdmin)
		path := "/api/v1/users/" + target
		body := map[string]string{"role": store.UserRoleMember}
		hidden[viaToken].hide(target, nil)
		var rec *httptest.ResponseRecorder
		if viaToken {
			rec = doRequestWithToken(t, srv, key, http.MethodPatch, path, body)
		} else {
			rec = doRequestAsUser(t, srv, upUser(t, s, holder), http.MethodPatch, path, body)
		}
		hidden[viaToken].stop()
		requireBindingStateDrift(t, rec, fmt.Sprintf("role drift, token=%t", viaToken))
		assert.Equal(t, store.UserRoleAdmin, upUser(t, s, target).Role, "token=%t: role kept", viaToken)
		assert.True(t, urdHoldsBinding(t, s, target, superAdminRD), "token=%t: binding kept", viaToken)
	}
}

// TestUserDeleteToken_ForceRecordsCredentialKind pins the credential kind a
// forced deletion records: uat for a hub token with user:delete and
// agent:delete, session for a session.
func TestUserDeleteToken_ForceRecordsCredentialKind(t *testing.T) {
	for _, viaToken := range []bool{true, false} {
		f := newForceDelFixture(t, time.Hour)
		f.agent(t, fmt.Sprintf("urd-kind-a-%t", viaToken), nil)
		var rec *httptest.ResponseRecorder
		want := userDeletionCredentialKind
		if viaToken {
			admin := hubConfigTokenUser(t, f.s, "urd-kind-super", store.SystemRoleSuperAdmin)
			igProjectBinding(t, f.s, admin, f.project.ID, store.ProjectRoleOwner)
			key := mintHubConfigToken(t, f.srv, admin, hubBoundary(), "user:delete", "agent:delete")
			rec = doRequestWithToken(t, f.srv, key, http.MethodDelete, "/api/v1/users/"+f.dave.ID+"?force=true", nil)
			want = userDeletionTokenCredentialKind
		} else {
			rec = f.forceDelete(t)
		}
		require.Equal(t, http.StatusAccepted, rec.Code, "token=%t: %s", viaToken, rec.Body.String())
		d, err := f.s.GetUserDeletionByUser(context.Background(), f.dave.ID)
		require.NoError(t, err)
		assert.Equal(t, want, d.CredentialKind, "token=%t", viaToken)
	}
}

// urdMissingOwnerRoleStore answers store.ErrNotFound for the project-owner
// role definition inside a transaction while its switch is armed and done
// is not; every other call goes to the store. It is installed with
// installStoreFault right after the server is built.
type urdMissingOwnerRoleStore struct {
	store.Store
	fault *storeFaultSwitch
	done  *storeFaultSwitch
	inTx  bool
}

func (m *urdMissingOwnerRoleStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !m.fault.Active() || m.done.Active() {
		return m.Store.WithTx(ctx, fn)
	}
	return m.Store.WithTx(ctx, func(tx store.Store) error {
		// No switches here: a nil switch reads as active, so a nested WithTx passes straight through.
		return fn(&urdMissingOwnerRoleStore{Store: tx, inTx: true})
	})
}

func (m *urdMissingOwnerRoleStore) GetRoleDefinitionByName(ctx context.Context, name, scopeType string) (*store.RoleDefinition, error) {
	if m.inTx && name == store.ProjectRoleOwner && scopeType == store.RoleScopeProject {
		return nil, store.ErrNotFound
	}
	return m.Store.GetRoleDefinitionByName(ctx, name, scopeType)
}

// TestUserDeleteToken_NotFoundOtherThanUserIsNotA404 pins that the plain
// delete answers 404 only for a missing user: a not-found from another read
// in the delete transaction (here the project-owner role definition) is a
// server error, and the user is kept.
func TestUserDeleteToken_NotFoundOtherThanUserIsNotA404(t *testing.T) {
	done := &storeFaultSwitch{}
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *urdMissingOwnerRoleStore {
		return &urdMissingOwnerRoleStore{Store: inner, fault: f, done: done}
	})
	admin := hubConfigTokenUser(t, s, "urd-nf-super", store.SystemRoleSuperAdmin)
	target := urdMember(t, s, "urd-nf-target")
	igProjectBinding(t, s, target, igProject(t, s, "urd-nf-project"), store.ProjectRoleMember)
	fault.Arm()

	rec := doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodDelete, "/api/v1/users/"+target, nil)
	done.Arm()
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project-owner role definition", "the 500 names the missing role definition")
	assert.True(t, urdUserExists(t, s, target))
}
