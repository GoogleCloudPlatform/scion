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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upUser returns the stored user record.
func upUser(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// upTarget creates an active member with hub membership and returns its ID.
func upTarget(t *testing.T, s store.Store, name string) string {
	t.Helper()
	return hubConfigTokenUser(t, s, name, store.SystemRoleHubMember)
}

// upProjectCredential returns a project-boundary token identity of the
// super-admin adminID carrying selectors, for a project the super-admin
// owns. Hub-only selectors cannot be minted on a project boundary, so the
// identity is built directly, as the token middleware would build it.
func upProjectCredential(t *testing.T, srv *Server, s store.Store, adminID, name string, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	project := igProject(t, s, name)
	igProjectBinding(t, s, adminID, project, store.ProjectRoleOwner)
	for _, sel := range selectors {
		igMintRefused(t, srv, adminID, projectBoundary(project), sel)
	}
	adminUser := NewAuthenticatedUser(adminID, adminID+"@test.com", adminID, "admin", "api")
	return NewScopedUserIdentityWithBoundaryAndDecoration(adminUser, projectBoundary(project),
		selectors, tid(name+"-cred"), bearerCeiling(t, selectors...), nil)
}

// upTokenRefusal returns the response body of the PATCH /api/v1/users/{id}
// refusal for a token that may use none of user.update, user.suspend and
// user.promote.
func upTokenRefusal() string {
	rec := httptest.NewRecorder()
	writeUserPatchTokenRefusal(rec)
	return rec.Body.String()
}

// requireUpTokenRefusal asserts that rec is that refusal: 403, code
// forbidden, the refusal message and no session-only reason.
func requireUpTokenRefusal(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	assert.Equal(t, upTokenRefusal(), rec.Body.String(), label)
	assert.Contains(t, rec.Body.String(), userPatchTokenRefusalMessage, label)
	reason, _ := sessionOnlyDetailsOf(rec)
	assert.Empty(t, reason, "%s: no session-only reason: %s", label, rec.Body.String())
}

// TestUserPatchToken_CrossUserProfileRequiresUpdateSelector pins the
// cross-user profile rule of PATCH /api/v1/users/{id} for tokens: a hub
// token with user:update changes another user's display name and
// preferences; a token without it is refused, including for a PATCH that
// names no field; user:update cannot be minted on a project boundary, and
// a project-boundary credential carrying it is refused; a holder without
// user.update cannot mint it; a token identity whose request carries a
// session credential kind is refused with the session-only reason. A
// session updates another user's profile as it does without these rules.
func TestUserPatchToken_CrossUserProfileRequiresUpdateSelector(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "up-profile-super", store.SystemRoleSuperAdmin)
	target := upTarget(t, s, "up-profile-target")
	path := "/api/v1/users/" + target

	update := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:update")
	rec := doRequestWithToken(t, srv, update, http.MethodPatch, path, map[string]interface{}{
		"displayName": "Renamed By Token", "preferences": map[string]string{"theme": "dark"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := upUser(t, s, target)
	assert.Equal(t, "Renamed By Token", got.DisplayName)
	require.NotNil(t, got.Preferences)
	assert.Equal(t, "dark", got.Preferences.Theme)
	assert.Contains(t, rec.Body.String(), `"theme":"dark"`, "a token with user:update sees the preferences it wrote")

	rec = doRequestWithToken(t, srv, update, http.MethodPatch, path, map[string]interface{}{})
	assert.Equal(t, http.StatusOK, rec.Code, "an empty PATCH with user:update: %s", rec.Body.String())

	for _, sel := range []string{"user:suspend", "user:read"} {
		other := mintHubConfigToken(t, srv, admin, hubBoundary(), sel)
		rec = doRequestWithToken(t, srv, other, http.MethodPatch, path, map[string]interface{}{"displayName": "Not Allowed"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s without user:update: %s", sel, rec.Body.String())
		rec = doRequestWithToken(t, srv, other, http.MethodPatch, path, map[string]interface{}{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "an empty PATCH with %s: %s", sel, rec.Body.String())
	}
	assert.Equal(t, "Renamed By Token", upUser(t, s, target).DisplayName, "refused PATCHes write nothing")

	projectCredential := upProjectCredential(t, srv, s, admin, "up-profile-project", "user:update")
	rec = doRequestAsIdentity(t, srv, projectCredential, http.MethodPatch, path, map[string]interface{}{"displayName": "Project Bound"})
	requireUpTokenRefusal(t, rec, "project boundary")
	assert.Equal(t, "Renamed By Token", upUser(t, s, target).DisplayName)

	member := hubConfigTokenUser(t, s, "up-profile-member", store.SystemRoleHubMember)
	igMintRefused(t, srv, member, hubBoundary(), "user:update")

	hubCredential := NewScopedUserIdentityWithBoundaryAndDecoration(
		NewAuthenticatedUser(admin, admin+"@test.com", admin, "admin", "api"), hubBoundary(),
		[]string{"user:update"}, tid("up-profile-hub-cred"), bearerCeiling(t, "user:update"), nil)
	rec = igRequestWithCredential(srv.handleUserByID, hubCredential, CredentialKindInteractive, http.MethodPatch, path, `{"displayName":"Wrong Kind"}`)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "token identity with a session credential kind")
	assert.Equal(t, "Renamed By Token", upUser(t, s, target).DisplayName)

	rec = doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodPatch, path, map[string]interface{}{"displayName": "Renamed By Session"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "Renamed By Session", upUser(t, s, target).DisplayName)
}

// TestUserPatchToken_StatusRequiresSuspendSelector pins the status rule of
// PATCH /api/v1/users/{id} for tokens: a hub token with user:suspend
// suspends and reactivates another user and the change is audited; a
// token without it is refused; user:suspend cannot be minted on a project
// boundary, and a project-boundary credential carrying it is refused; a
// holder without user.suspend cannot mint it. Activating an invited user
// is refused for a token as for a session.
func TestUserPatchToken_StatusRequiresSuspendSelector(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := hubConfigTokenUser(t, s, "up-status-super", store.SystemRoleSuperAdmin)
	target := upTarget(t, s, "up-status-target")
	path := "/api/v1/users/" + target

	withoutSuspend := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:update")
	rec := doRequestWithToken(t, srv, withoutSuspend, http.MethodPatch, path, map[string]string{"status": store.UserStatusSuspended})
	assert.Equal(t, http.StatusForbidden, rec.Code, "user:update without user:suspend: %s", rec.Body.String())
	assert.Equal(t, store.UserStatusActive, upUser(t, s, target).Status)

	projectCredential := upProjectCredential(t, srv, s, admin, "up-status-project", "user:suspend")
	rec = doRequestAsIdentity(t, srv, projectCredential, http.MethodPatch, path, map[string]string{"status": store.UserStatusSuspended})
	requireUpTokenRefusal(t, rec, "project boundary")
	assert.Equal(t, store.UserStatusActive, upUser(t, s, target).Status)

	member := hubConfigTokenUser(t, s, "up-status-member", store.SystemRoleHubMember)
	igMintRefused(t, srv, member, hubBoundary(), "user:suspend")

	rec = doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodPatch, path, map[string]interface{}{"preferences": map[string]string{"theme": "dark"}})
	require.Equal(t, http.StatusOK, rec.Code, "session sets a preference: %s", rec.Body.String())

	suspend := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:suspend")
	rec = doRequestWithToken(t, srv, suspend, http.MethodPatch, path, map[string]string{"status": store.UserStatusSuspended})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.UserStatusSuspended, upUser(t, s, target).Status)
	var suspended map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &suspended))
	assert.NotContains(t, suspended, "preferences", "a token with user:suspend only gets no preferences: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"theme":"dark"`)

	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "user_suspend", TargetType: "user", TargetID: target})
	require.NoError(t, err)
	require.Len(t, audits, 1, "the token suspension is audited")
	assert.Equal(t, admin, audits[0].ActorPrincipalID)
	assert.Equal(t, string(CredentialKindUAT), audits[0].ActorCredentialType)
	assert.Equal(t, string(BoundaryKindHub), audits[0].CredentialBoundaryKind)

	rec = doRequestWithToken(t, srv, suspend, http.MethodPatch, path, map[string]string{"status": store.UserStatusActive})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.UserStatusActive, upUser(t, s, target).Status)

	invited := tid("up-status-invited")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: invited, Email: invited + "@test.com", DisplayName: "Invited", Role: "member", Status: store.UserStatusInvited}))
	rec = doRequestWithToken(t, srv, suspend, http.MethodPatch, "/api/v1/users/"+invited, map[string]string{"status": store.UserStatusActive})
	assert.Equal(t, http.StatusConflict, rec.Code, "activating an invited user: %s", rec.Body.String())
	assert.Equal(t, store.UserStatusInvited, upUser(t, s, invited).Status)

	rec = doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodPatch, path, map[string]string{"status": store.UserStatusSuspended})
	require.Equal(t, http.StatusOK, rec.Code, "session: %s", rec.Body.String())
	assert.Equal(t, store.UserStatusSuspended, upUser(t, s, target).Status)
}

// TestUserPatchToken_MixedFieldsAllOrNothing pins that a token PATCH
// naming several fields needs the permission of every field before any
// write: status with a profile field needs user:suspend and user:update,
// and missing either writes nothing; with both, every field is written;
// with a role field added, the token also needs user:promote, and without
// it nothing is written (TestUserPatchToken_MixedRoleAndStatusAllOrNothing
// pins the role field with user:promote).
func TestUserPatchToken_MixedFieldsAllOrNothing(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "up-mixed-super", store.SystemRoleSuperAdmin)
	target := upTarget(t, s, "up-mixed-target")
	path := "/api/v1/users/" + target
	original := upUser(t, s, target).DisplayName
	mixed := map[string]interface{}{"status": store.UserStatusSuspended, "displayName": "Mixed Rename"}

	for _, sel := range []string{"user:suspend", "user:update"} {
		key := mintHubConfigToken(t, srv, admin, hubBoundary(), sel)
		rec := doRequestWithToken(t, srv, key, http.MethodPatch, path, mixed)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s alone: %s", sel, rec.Body.String())
		got := upUser(t, s, target)
		assert.Equal(t, store.UserStatusActive, got.Status, "%s alone writes no status", sel)
		assert.Equal(t, original, got.DisplayName, "%s alone writes no profile", sel)
	}

	both := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:suspend", "user:update")
	withRole := map[string]interface{}{"status": store.UserStatusSuspended, "displayName": "Mixed Rename", "role": store.UserRoleViewer}
	rec := doRequestWithToken(t, srv, both, http.MethodPatch, path, withRole)
	assert.Equal(t, http.StatusForbidden, rec.Code, "mixed fields with role, no user:promote: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "requires user.promote permission")
	got := upUser(t, s, target)
	assert.Equal(t, store.UserStatusActive, got.Status)
	assert.Equal(t, original, got.DisplayName)
	assert.Equal(t, store.UserRoleMember, got.Role)

	rec = doRequestWithToken(t, srv, both, http.MethodPatch, path, mixed)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = upUser(t, s, target)
	assert.Equal(t, store.UserStatusSuspended, got.Status)
	assert.Equal(t, "Mixed Rename", got.DisplayName)
}

// TestUserPatchToken_SelfProfileSessionOnly pins that a token never
// changes its holder's own profile: a PATCH of the holder's own record
// naming a profile field, or no field, is refused as session-only
// (INTERACTIVE_STATE) even with user:update, and writes nothing. A
// session changes its own profile.
func TestUserPatchToken_SelfProfileSessionOnly(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "up-self-super", store.SystemRoleSuperAdmin)
	path := "/api/v1/users/" + admin
	original := upUser(t, s, admin).DisplayName

	key := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:update", "user:suspend")
	for _, body := range []map[string]interface{}{
		{"displayName": "Self Rename"},
		{"preferences": map[string]string{"theme": "dark"}},
		{},
		{"displayName": "Self Rename", "status": store.UserStatusActive},
	} {
		rec := doRequestWithToken(t, srv, key, http.MethodPatch, path, body)
		requireSessionOnlyRefusal(t, rec, authzop.ReasonInteractiveState, "own profile")
	}
	got := upUser(t, s, admin)
	assert.Equal(t, original, got.DisplayName)
	if got.Preferences != nil {
		assert.Empty(t, got.Preferences.Theme)
	}

	rec := doRequestAsUser(t, srv, got, http.MethodPatch, path, map[string]string{"displayName": "Self Rename"})
	require.Equal(t, http.StatusOK, rec.Code, "session: %s", rec.Body.String())
	assert.Equal(t, "Self Rename", upUser(t, s, admin).DisplayName)
}

// TestUserPatchToken_SelfStatusGuardStillApplies pins that the self-status
// guard applies to tokens: a hub token with user:suspend changing its
// holder's own status gets 409 and writes nothing; without user:suspend
// the status check refuses it first with 403. A session gets the same 409.
func TestUserPatchToken_SelfStatusGuardStillApplies(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "up-selfstatus-super", store.SystemRoleSuperAdmin)
	path := "/api/v1/users/" + admin

	suspend := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:suspend")
	rec := doRequestWithToken(t, srv, suspend, http.MethodPatch, path, map[string]string{"status": store.UserStatusSuspended})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot change your own status")
	assert.Equal(t, store.UserStatusActive, upUser(t, s, admin).Status)

	update := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:update")
	rec = doRequestWithToken(t, srv, update, http.MethodPatch, path, map[string]string{"status": store.UserStatusSuspended})
	assert.Equal(t, http.StatusForbidden, rec.Code, "without user:suspend: %s", rec.Body.String())
	assert.Equal(t, store.UserStatusActive, upUser(t, s, admin).Status)

	rec = doRequestAsUser(t, srv, upUser(t, s, admin), http.MethodPatch, path, map[string]string{"status": store.UserStatusSuspended})
	assert.Equal(t, http.StatusConflict, rec.Code, "session: %s", rec.Body.String())
	assert.Equal(t, store.UserStatusActive, upUser(t, s, admin).Status)
}

// TestUserPatchToken_RefusalSameForEveryUserID pins that a PATCH of
// another user by a token that may use neither user.update nor
// user.suspend gets one refusal whether or not the user exists, for every
// body: a super-admin's hub token with no user selector, a hub token with
// user:read only, project-boundary tokens with or without user selectors,
// and a hub token carrying user:update and user:suspend whose holder
// holds neither permission. A hub token with user:update gets 404
// for an unknown user, and one with user:suspend gets 404 for a status
// change of an unknown user.
func TestUserPatchToken_RefusalSameForEveryUserID(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "up-anyid-super", store.SystemRoleSuperAdmin)
	target := upTarget(t, s, "up-anyid-target")
	unknown := tid("up-anyid-unknown")
	_, err := s.GetUser(context.Background(), unknown)
	require.Error(t, err, "the unknown user does not exist")
	original := upUser(t, s, target)

	bodies := []map[string]interface{}{
		{"displayName": "Not Allowed"},
		{"status": store.UserStatusSuspended},
		{"preferences": map[string]string{"theme": "dark"}},
		{"role": store.UserRoleAdmin},
		{},
		{"status": "not-a-status"},
	}
	type caller struct {
		label string
		do    func(path string, body map[string]interface{}) *httptest.ResponseRecorder
	}
	viaToken := func(key string) func(string, map[string]interface{}) *httptest.ResponseRecorder {
		return func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
			return doRequestWithToken(t, srv, key, http.MethodPatch, path, body)
		}
	}
	viaIdentity := func(id *ScopedUserIdentity) func(string, map[string]interface{}) *httptest.ResponseRecorder {
		return func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
			return doRequestAsIdentity(t, srv, id, http.MethodPatch, path, body)
		}
	}
	callers := []caller{
		{"super-admin's hub token with no user selector", viaToken(mintHubConfigToken(t, srv, admin, hubBoundary(), "project:read"))},
		{"hub token with user:update and user:suspend, holder without either", viaToken(upHolderWithoutAuthority(t, srv, s, "up-anyid-holder"))},
		{"hub token with user:read only", viaToken(mintHubConfigToken(t, srv, admin, hubBoundary(), "user:read"))},
		{"project-boundary token with user:update and user:suspend", viaIdentity(upProjectCredential(t, srv, s, admin, "up-anyid-project-sel", "user:update", "user:suspend"))},
		{"project-boundary token with no user selector", viaIdentity(NewScopedUserIdentityWithBoundaryAndDecoration(
			NewAuthenticatedUser(admin, admin+"@test.com", admin, "admin", "api"), projectBoundary(igProject(t, s, "up-anyid-project")),
			[]string{"project:read"}, tid("up-anyid-project-cred"), bearerCeiling(t, "project:read"), nil))},
	}
	for _, c := range callers {
		for _, body := range bodies {
			label, _ := json.Marshal(body)
			existing := c.do("/api/v1/users/"+target, body)
			missing := c.do("/api/v1/users/"+unknown, body)
			requireUpTokenRefusal(t, existing, c.label+" existing user "+string(label))
			assert.Equal(t, existing.Code, missing.Code, "%s %s: status", c.label, label)
			assert.Equal(t, existing.Body.String(), missing.Body.String(), "%s %s: body", c.label, label)
		}
	}
	got := upUser(t, s, target)
	assert.Equal(t, original.DisplayName, got.DisplayName, "refused PATCHes write nothing")
	assert.Equal(t, original.Status, got.Status)
	assert.Equal(t, original.Role, got.Role)

	update := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:update")
	rec := doRequestWithToken(t, srv, update, http.MethodPatch, "/api/v1/users/"+unknown, map[string]interface{}{"displayName": "Nobody"})
	assert.Equal(t, http.StatusNotFound, rec.Code, "user:update, unknown user: %s", rec.Body.String())
	suspend := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:suspend")
	rec = doRequestWithToken(t, srv, suspend, http.MethodPatch, "/api/v1/users/"+unknown, map[string]interface{}{"status": store.UserStatusSuspended})
	assert.Equal(t, http.StatusNotFound, rec.Code, "user:suspend, unknown user: %s", rec.Body.String())
}

// upHolderWithoutAuthority mints a hub token carrying user:update and
// user:suspend for a new super-admin, then removes the holder's role
// bindings, and returns the token. The holder keeps hub membership, which
// grants neither permission.
func upHolderWithoutAuthority(t *testing.T, srv *Server, s store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	holder := hubConfigTokenUser(t, s, name, store.SystemRoleSuperAdmin)
	key := mintHubConfigToken(t, srv, holder, hubBoundary(), "user:update", "user:suspend")
	_, err := s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, holder)
	require.NoError(t, err)
	require.Equal(t, 0, superAdminBindingCount(t, s, holder))
	u := upUser(t, s, holder)
	u.Role = store.UserRoleMember
	require.NoError(t, s.UpdateUser(ctx, u))
	return key
}

// upAuditShape returns the decision audit records of one request with the
// fields that identify the request (record ID, time, correlation ID) and
// the path id (resource ID, route) cleared, after asserting that every
// user record names the path id.
func upAuditShape(t *testing.T, records []*store.DecisionAuditRecord, id, label string) []store.DecisionAuditRecord {
	t.Helper()
	out := make([]store.DecisionAuditRecord, 0, len(records))
	for _, r := range records {
		c := *r
		if c.ResourceType == "user" {
			assert.Equal(t, id, c.ResourceID, "%s: the record names the path id", label)
		}
		c.ID, c.Timestamp, c.CorrelationID = "", time.Time{}, ""
		c.ResourceID = strings.ReplaceAll(c.ResourceID, id, "{id}")
		c.Route = strings.ReplaceAll(c.Route, id, "{id}")
		out = append(out, c)
	}
	return out
}

// TestUserPatchToken_RefusalDecisionAudited pins that the refusal of a
// PATCH of another user by a token that may use none of user.update,
// user.suspend and user.promote comes from authorization decisions on the
// hub-level user record that are audited as denies, one for each of
// user.update, user.suspend and user.promote, and that the records have
// the same shape whether or not the user exists: for a super-admin's token
// with no user selector and for a token with user:update and user:suspend
// whose holder holds neither permission.
func TestUserPatchToken_RefusalDecisionAudited(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "up-audit-super", store.SystemRoleSuperAdmin)
	target := upTarget(t, s, "up-audit-target")
	unknown := tid("up-audit-unknown")
	_, err := s.GetUser(context.Background(), unknown)
	require.Error(t, err, "the unknown user does not exist")

	emitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	for _, c := range []struct {
		label string
		key   string
	}{
		{"super-admin's hub token with no user selector", mintHubConfigToken(t, srv, admin, hubBoundary(), "project:read")},
		{"hub token with user:update and user:suspend, holder without either", upHolderWithoutAuthority(t, srv, s, "up-audit-holder")},
	} {
		body := map[string]interface{}{"displayName": "Not Allowed"}
		var shapes [2][]store.DecisionAuditRecord
		var bodies [2]string
		for i, id := range []string{target, unknown} {
			emitter.records = nil
			rec := doRequestWithToken(t, srv, c.key, http.MethodPatch, "/api/v1/users/"+id, body)
			requireUpTokenRefusal(t, rec, c.label+" "+id)
			bodies[i] = rec.Body.String()

			var userDenies []string
			for _, r := range emitter.records {
				if r.ResourceType == "user" {
					assert.Equal(t, "deny", r.Result, "%s %s: %s", c.label, id, r.Permission)
					userDenies = append(userDenies, r.PermissionID)
				}
			}
			assert.Equal(t, []string{"user.update", "user.suspend", "user.promote"}, userDenies, "%s %s: audited decisions", c.label, id)
			shapes[i] = upAuditShape(t, emitter.records, id, c.label)
		}
		assert.Equal(t, bodies[0], bodies[1], "%s: body", c.label)
		assert.Equal(t, shapes[0], shapes[1], "%s: audit record shape", c.label)
	}
}
