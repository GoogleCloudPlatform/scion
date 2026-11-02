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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inviteTokenFixture is a hub admin (live user.invite, hub.allow_list.read
// and hub.allow_list.update authority) who owns a project, a hub member who
// holds none of them, and a project the admin's project tokens bind to.
type inviteTokenFixture struct {
	srv     *Server
	s       store.Store
	admin   *store.User
	member  *store.User
	project string
}

func newInviteTokenFixture(t *testing.T, prefix string) *inviteTokenFixture {
	t.Helper()
	srv, s := testServer(t)
	return newInviteTokenFixtureOn(t, prefix, srv, s)
}

// newInviteTokenFixtureOn builds the fixture on srv and its store s.
func newInviteTokenFixtureOn(t *testing.T, prefix string, srv *Server, s store.Store) *inviteTokenFixture {
	t.Helper()
	ctx := context.Background()
	f := &inviteTokenFixture{srv: srv, s: s, project: tid(prefix + "-project")}
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.project, Name: prefix, Slug: prefix + "-project"}))
	adminID := hubConfigTokenUser(t, s, prefix+"-admin", store.SystemRoleHubAdmin)
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", f.project, store.ProjectRoleOwner)
	memberID := hubConfigTokenUser(t, s, prefix+"-member", store.SystemRoleHubMember)
	var err error
	f.admin, err = s.GetUser(ctx, adminID)
	require.NoError(t, err)
	f.member, err = s.GetUser(ctx, memberID)
	require.NoError(t, err)
	return f
}

// itfUniqueName returns prefix with a random suffix, for records a test
// creates more than once.
func itfUniqueName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// mint mints a real token for the hub admin.
func (f *inviteTokenFixture) mint(t *testing.T, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	return mintHubConfigToken(t, f.srv, f.admin.ID, boundary, scopes...)
}

// mintFails reports whether minting the selectors for userID on boundary
// is refused.
func (f *inviteTokenFixture) mintFails(userID string, boundary TokenBoundary, scopes ...string) bool {
	_, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "itf-" + tid("probe"), Boundary: boundary, Scopes: scopes,
	})
	return err != nil
}

// formerAdminToken creates a second hub admin and mints a real hub token
// with scopes for that admin. The returned func removes the admin's hub
// admin role binding through the store, so the token's holder lacks
// the authority the token was minted with.
func (f *inviteTokenFixture) formerAdminToken(t *testing.T, name string, scopes ...string) (string, func()) {
	t.Helper()
	id := hubConfigTokenUser(t, f.s, name, store.SystemRoleHubAdmin)
	key := mintHubConfigToken(t, f.srv, id, hubBoundary(), scopes...)
	return key, func() {
		t.Helper()
		f.removeHubAdminBinding(t, id)
	}
}

// removeHubAdminBinding removes user id's hub admin role binding through
// the store, and requires exactly one to be removed.
func (f *inviteTokenFixture) removeHubAdminBinding(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	rd, err := f.s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, id)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID {
			require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.Equal(t, 1, removed, "the holder's hub admin role binding is removed")
}

// itfInviteCodeExpiry is the expiry of the invitation codes the tests store.
var itfInviteCodeExpiry = time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)

// inviteCode stores a fresh invitation code created by the hub admin.
func (f *inviteTokenFixture) inviteCode(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, f.s.CreateInviteCode(context.Background(), &store.InviteCode{
		ID: id, CodeHash: id + "-hash", CodePrefix: "itf", MaxUses: 1,
		ExpiresAt: itfInviteCodeExpiry, CreatedBy: f.admin.ID,
	}))
	return id
}

func (f *inviteTokenFixture) userExists(t *testing.T, email string) bool {
	t.Helper()
	_, err := f.s.GetUserByEmail(context.Background(), email)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

func (f *inviteTokenFixture) inviteCodeCount(t *testing.T) int {
	t.Helper()
	res, err := f.s.ListInviteCodes(context.Background(), store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return res.TotalCount
}

// itfRouteCase is one request: prep returns the path and body for a
// fresh target, and wrote reports whether the request changed anything
// (nil for a read).
type itfRouteCase struct {
	name   string
	method string
	want   int
	prep   func(t *testing.T) (path string, body interface{}, wrote func() bool)
}

func (f *inviteTokenFixture) inviteRoutes() []itfRouteCase {
	return []itfRouteCase{
		{"invite", http.MethodPost, http.StatusCreated, func(t *testing.T) (string, interface{}, func() bool) {
			email := itfUniqueName("itf-one") + "@example.com"
			return "/api/v1/admin/users/invite", UserInviteRequest{Email: email}, func() bool { return f.userExists(t, email) }
		}},
		{"invite bulk", http.MethodPost, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			email := itfUniqueName("itf-bulk") + "@example.com"
			return "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{{Email: email}}},
				func() bool { return f.userExists(t, email) }
		}},
		{"invites list", http.MethodGet, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			return "/api/v1/admin/invites", nil, nil
		}},
		{"invite code create", http.MethodPost, http.StatusCreated, func(t *testing.T) (string, interface{}, func() bool) {
			before := f.inviteCodeCount(t)
			return "/api/v1/admin/invites", InviteCreateRequest{ExpiresIn: "1h", MaxUses: 1},
				func() bool { return f.inviteCodeCount(t) != before }
		}},
		{"invite stats", http.MethodGet, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			return "/api/v1/admin/invites/stats", nil, nil
		}},
		{"invite code get", http.MethodGet, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			return "/api/v1/admin/invites/" + f.inviteCode(t), nil, nil
		}},
		{"invite code revoke", http.MethodPost, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			id := f.inviteCode(t)
			return "/api/v1/admin/invites/" + id + "/revoke", nil, func() bool {
				inv, err := f.s.GetInviteCode(context.Background(), id)
				require.NoError(t, err)
				return inv.Revoked
			}
		}},
		{"invite code delete", http.MethodDelete, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			id := f.inviteCode(t)
			return "/api/v1/admin/invites/" + id, nil, func() bool {
				_, err := f.s.GetInviteCode(context.Background(), id)
				if errors.Is(err, store.ErrNotFound) {
					return true
				}
				require.NoError(t, err)
				return false
			}
		}},
	}
}

// itfExpectRefused sends the route with a fresh target and requires 403 with
// nothing written.
func itfExpectRefused(t *testing.T, rc itfRouteCase, send func(path string, body interface{}) int, who string) {
	t.Helper()
	path, body, wrote := rc.prep(t)
	assert.Equal(t, http.StatusForbidden, send(path, body), "%s: %s %s", who, rc.method, path)
	if wrote != nil {
		assert.False(t, wrote(), "%s: a refused %s %s must write nothing", who, rc.method, path)
	}
}

// itfExpectAdmitted sends the route with a fresh target and requires the
// route's status and, for a write, its effect.
func itfExpectAdmitted(t *testing.T, rc itfRouteCase, send func(path string, body interface{}) int, who string) {
	t.Helper()
	path, body, wrote := rc.prep(t)
	assert.Equal(t, rc.want, send(path, body), "%s: %s %s", who, rc.method, path)
	if wrote != nil {
		assert.True(t, wrote(), "%s: an admitted %s %s must take effect", who, rc.method, path)
	}
}

func (f *inviteTokenFixture) withToken(t *testing.T, key, method string) func(string, interface{}) int {
	return func(path string, body interface{}) int {
		return doRequestWithToken(t, f.srv, key, method, path, body).Code
	}
}

func (f *inviteTokenFixture) asUser(t *testing.T, u *store.User, method string) func(string, interface{}) int {
	return func(path string, body interface{}) int {
		return doRequestAsUser(t, f.srv, u, method, path, body).Code
	}
}

// TestInviteToken_RequiresInviteSelectorAndHubBoundary pins every
// invitation route (single and bulk invite; invitation code list, create,
// stats, read, revoke and delete) for user access tokens: the route checks
// user.invite on the hub instance target, so a token is admitted only with
// user:invite, on the hub boundary, and with the holder's live user.invite
// authority, checked at each request: a token is refused once its holder
// loses user.invite. Refused requests write nothing. Sessions are decided
// on the same permission: a hub admin's session is admitted and a hub
// member's is refused.
func TestInviteToken_RequiresInviteSelectorAndHubBoundary(t *testing.T) {
	f := newInviteTokenFixture(t, "itok")

	// The selector is hub-only and needs live user.invite authority.
	assert.True(t, f.mintFails(f.admin.ID, projectBoundary(f.project), "user:invite"), "user:invite must not mint on a project boundary")
	assert.True(t, f.mintFails(f.member.ID, hubBoundary(), "user:invite"), "a holder without user.invite must not mint user:invite")

	admitted := f.mint(t, hubBoundary(), "user:invite")
	// A hub token of the same holder with another hub selector it holds.
	unrelated := f.mint(t, hubBoundary(), "hub_allow_list:read")
	projectToken := f.mint(t, projectBoundary(f.project), "project:read")
	// A stored project-boundary row carrying the hub-only selector, as a
	// legacy token would: refused at request time for the boundary.
	legacy := uatpInsertLegacyToken(t, f.s, f.admin.ID, f.project, []string{"user:invite"})
	// A token minted while its holder held user.invite, used after the
	// holder lost it: refused at request time.
	former, demote := f.formerAdminToken(t, "itok-former", "user:invite")
	require.Equal(t, http.StatusOK, doRequestWithToken(t, f.srv, former, http.MethodGet, "/api/v1/admin/invites", nil).Code,
		"the token is admitted while its holder holds user.invite")
	demote()

	for _, rc := range f.inviteRoutes() {
		t.Run(rc.name, func(t *testing.T) {
			itfExpectRefused(t, rc, f.withToken(t, former, rc.method), "hub token whose holder lost user.invite")
			itfExpectRefused(t, rc, f.withToken(t, unrelated, rc.method), "hub token without user:invite")
			itfExpectRefused(t, rc, f.withToken(t, projectToken, rc.method), "project token")
			itfExpectRefused(t, rc, f.withToken(t, legacy, rc.method), "project-boundary row with user:invite")
			itfExpectRefused(t, rc, f.asUser(t, f.member, rc.method), "hub member session")
			itfExpectAdmitted(t, rc, f.withToken(t, admitted, rc.method), "hub token with user:invite")
			itfExpectAdmitted(t, rc, f.asUser(t, f.admin, rc.method), "hub admin session")
		})
	}
}

func (f *inviteTokenFixture) allowListRoutes() (reads, writes []itfRouteCase) {
	reads = []itfRouteCase{
		{"allow-list get", http.MethodGet, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			return "/api/v1/admin/allow-list", nil, nil
		}},
		{"allow-list domains", http.MethodGet, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			return "/api/v1/admin/allow-list/domains", nil, nil
		}},
	}
	writes = []itfRouteCase{
		{"allow-list add", http.MethodPost, http.StatusCreated, func(t *testing.T) (string, interface{}, func() bool) {
			email := itfUniqueName("itf-add") + "@example.com"
			return "/api/v1/admin/allow-list", AllowListAddRequest{Email: email}, func() bool { return f.userExists(t, email) }
		}},
		{"allow-list import", http.MethodPost, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			email := itfUniqueName("itf-import") + "@example.com"
			return "/api/v1/admin/allow-list/import", AllowListBulkAddRequest{Emails: []AllowListAddRequest{{Email: email}}},
				func() bool { return f.userExists(t, email) }
		}},
		{"allow-list delete", http.MethodDelete, http.StatusOK, func(t *testing.T) (string, interface{}, func() bool) {
			email := itfUniqueName("itf-del") + "@example.com"
			newInvitedUser(t, f.s, email, email)
			return "/api/v1/admin/allow-list/" + email, nil, func() bool { return !f.userExists(t, email) }
		}},
	}
	return reads, writes
}

// TestAllowListReadToken_DoesNotAdmitWrites pins the allow-list routes for
// user access tokens. The route guard checks hub.allow_list.read for every
// method, and add, import and delete also check hub.allow_list.update:
//   - hub_allow_list:read admits the list and domain reads and no write;
//   - hub_allow_list:update alone admits nothing, because the guard needs
//     the read selector;
//   - both selectors admit the writes;
//   - both selectors are hub-only and need the holder's live authority,
//     checked at each request; project tokens, project-boundary rows and
//     tokens whose holder lost the permissions are refused.
//
// Sessions follow the same two permissions: a session holding only
// hub.allow_list.read reads the list and is refused every write, and one
// holding both writes.
func TestAllowListReadToken_DoesNotAdmitWrites(t *testing.T) {
	f := newInviteTokenFixture(t, "altok")
	ctx := context.Background()

	for _, sel := range []string{"hub_allow_list:read", "hub_allow_list:update"} {
		assert.True(t, f.mintFails(f.admin.ID, projectBoundary(f.project), sel), "%s must not mint on a project boundary", sel)
		assert.True(t, f.mintFails(f.member.ID, hubBoundary(), sel), "a holder without the permission must not mint %s", sel)
	}

	readOnly := f.mint(t, hubBoundary(), "hub_allow_list:read")
	updateOnly := f.mint(t, hubBoundary(), "hub_allow_list:update")
	both := f.mint(t, hubBoundary(), "hub_allow_list:read", "hub_allow_list:update")
	unrelated := f.mint(t, hubBoundary(), "user:invite")
	projectToken := f.mint(t, projectBoundary(f.project), "project:read")
	legacy := uatpInsertLegacyToken(t, f.s, f.admin.ID, f.project, []string{"hub_allow_list:read", "hub_allow_list:update"})
	// A token minted while its holder held both permissions, used after the
	// holder lost them: refused at request time.
	former, demote := f.formerAdminToken(t, "altok-former", "hub_allow_list:read", "hub_allow_list:update")
	require.Equal(t, http.StatusOK, doRequestWithToken(t, f.srv, former, http.MethodGet, "/api/v1/admin/allow-list", nil).Code,
		"the token is admitted while its holder holds hub.allow_list.read")
	demote()

	readSessionID := tid("altok-read-session")
	systemRoleUserWithPermissions(t, f.s, readSessionID, []string{"hub.allow_list.read"})
	ensureHubMembership(ctx, f.s, readSessionID)
	readSession, err := f.s.GetUser(ctx, readSessionID)
	require.NoError(t, err)
	writeSessionID := tid("altok-write-session")
	systemRoleUserWithPermissions(t, f.s, writeSessionID, []string{"hub.allow_list.read", "hub.allow_list.update"})
	ensureHubMembership(ctx, f.s, writeSessionID)
	writeSession, err := f.s.GetUser(ctx, writeSessionID)
	require.NoError(t, err)

	reads, writes := f.allowListRoutes()
	for _, rc := range reads {
		t.Run(rc.name, func(t *testing.T) {
			itfExpectRefused(t, rc, f.withToken(t, former, rc.method), "hub token whose holder lost the allow-list permissions")
			itfExpectRefused(t, rc, f.withToken(t, updateOnly, rc.method), "hub token with hub_allow_list:update only")
			itfExpectRefused(t, rc, f.withToken(t, unrelated, rc.method), "hub token without hub_allow_list:read")
			itfExpectRefused(t, rc, f.withToken(t, projectToken, rc.method), "project token")
			itfExpectRefused(t, rc, f.withToken(t, legacy, rc.method), "project-boundary row with both selectors")
			itfExpectRefused(t, rc, f.asUser(t, f.member, rc.method), "hub member session")
			itfExpectAdmitted(t, rc, f.withToken(t, readOnly, rc.method), "hub token with hub_allow_list:read")
			itfExpectAdmitted(t, rc, f.withToken(t, both, rc.method), "hub token with both selectors")
			itfExpectAdmitted(t, rc, f.asUser(t, readSession, rc.method), "session with hub.allow_list.read")
		})
	}
	for _, rc := range writes {
		t.Run(rc.name, func(t *testing.T) {
			itfExpectRefused(t, rc, f.withToken(t, former, rc.method), "hub token whose holder lost the allow-list permissions")
			itfExpectRefused(t, rc, f.withToken(t, readOnly, rc.method), "hub token with hub_allow_list:read only")
			itfExpectRefused(t, rc, f.withToken(t, updateOnly, rc.method), "hub token with hub_allow_list:update only")
			itfExpectRefused(t, rc, f.withToken(t, unrelated, rc.method), "hub token without the allow-list selectors")
			itfExpectRefused(t, rc, f.withToken(t, projectToken, rc.method), "project token")
			itfExpectRefused(t, rc, f.withToken(t, legacy, rc.method), "project-boundary row with both selectors")
			itfExpectRefused(t, rc, f.asUser(t, readSession, rc.method), "session with hub.allow_list.read only")
			itfExpectAdmitted(t, rc, f.withToken(t, both, rc.method), "hub token with both selectors")
			itfExpectAdmitted(t, rc, f.asUser(t, writeSession, rc.method), "session with both permissions")
		})
	}
}

// TestAllowListDeleteToken_LastProjectOwnerProtected pins that removing an
// invited user through the allow list by token keeps the last-project-owner
// guard: while the invited user is a project's only owner the request gets
// 409 last_owner and the user and the owner binding stay; once the project
// has another owner the request removes the user and the user's bindings. A
// session gets the same 409.
func TestAllowListDeleteToken_LastProjectOwnerProtected(t *testing.T) {
	f := newInviteTokenFixture(t, "aldel")
	ctx := context.Background()
	carol := newInvitedUser(t, f.s, "aldel-carol", tid("aldel-carol")+"@example.com")
	project := &store.Project{ID: tid("aldel-carol-project"), Name: "Carol Project", Slug: "aldel-carol-project",
		CreatedBy: carol.ID, OwnerID: carol.ID}
	require.NoError(t, f.s.CreateProject(ctx, project))
	f.srv.seedProjectCreatorMembership(ctx, project)
	requireSingleOwnerBinding(t, f.s, project.ID, carol.ID)

	key := f.mint(t, hubBoundary(), "hub_allow_list:read", "hub_allow_list:update")
	path := "/api/v1/admin/allow-list/" + carol.Email

	requireLastOwnerDenial(t, doRequestWithToken(t, f.srv, key, http.MethodDelete, path, nil), project)
	_, err := f.s.GetUser(ctx, carol.ID)
	require.NoError(t, err, "a refused delete keeps the invited user")
	requireSingleOwnerBinding(t, f.s, project.ID, carol.ID)

	requireLastOwnerDenial(t, doRequestAsUser(t, f.srv, f.admin, http.MethodDelete, path, nil), project)
	_, err = f.s.GetUser(ctx, carol.ID)
	require.NoError(t, err, "a refused delete keeps the invited user")
	requireSingleOwnerBinding(t, f.s, project.ID, carol.ID)

	createOwnerBinding(t, f.s, f.admin.ID, project.ID, nil, nil)
	rec := doRequestWithToken(t, f.srv, key, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, err = f.s.GetUser(ctx, carol.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "the invited user is removed")
	assert.Empty(t, allBindingsFor(t, f.s, carol.ID), "the invited user's bindings are removed")
	requireSingleOwnerBinding(t, f.s, project.ID, f.admin.ID)
}

// TestInvitationToken_CreatesNoRoleBindingOrGroupMembership pins that a
// record a token creates through an invitation route carries no authority:
// single invite and allow-list add and import each create a user with
// status invited and the placeholder role member, with no role binding and
// no group membership beyond those of an invited user created directly in
// the store, even when the request body names a role, a project or a
// group. Bulk invite refuses a body that names a role with 422 and creates
// nothing. User provisioning (POST /api/v1/users) refuses such a body too,
// so its case sends only the fields it accepts and gets the same record.
// An invitation code a token creates is attributed to the token's holder
// and names no role. The hub runs without dev authentication, because a
// hub with dev authentication refuses user provisioning.
func TestInvitationToken_CreatesNoRoleBindingOrGroupMembership(t *testing.T) {
	srv, st := testServerNoDevAuth(t)
	f := newInviteTokenFixtureOn(t, "inauth", srv, st)
	ctx := context.Background()
	key := f.mint(t, hubBoundary(), "user:invite", "hub_allow_list:read", "hub_allow_list:update")

	baseline := newInvitedUser(t, f.s, "inauth-baseline", tid("inauth-baseline")+"@example.com")
	baseGroups, err := f.s.GetEffectiveGroups(ctx, baseline.ID)
	require.NoError(t, err)

	extra := func(body map[string]interface{}) map[string]interface{} {
		body["role"] = store.UserRoleAdmin
		body["projectId"] = f.project
		body["roleDefinitionId"] = store.SystemRoleSuperAdmin
		body["groupId"] = "hub-members"
		return body
	}
	cases := []struct {
		name   string
		path   string
		body   func(email string) interface{}
		status int
	}{
		{"invite", "/api/v1/admin/users/invite", func(e string) interface{} {
			return extra(map[string]interface{}{"email": e})
		}, http.StatusCreated},
		// Bulk invite decodes strictly (ptone/scion#4952): the role field
		// is refused with 422 and nothing is created.
		{"invite bulk", "/api/v1/admin/users/invite/bulk", func(e string) interface{} {
			return extra(map[string]interface{}{"emails": []interface{}{extra(map[string]interface{}{"email": e})}})
		}, http.StatusUnprocessableEntity},
		{"allow-list add", "/api/v1/admin/allow-list", func(e string) interface{} {
			return extra(map[string]interface{}{"email": e})
		}, http.StatusCreated},
		{"allow-list import", "/api/v1/admin/allow-list/import", func(e string) interface{} {
			return extra(map[string]interface{}{"emails": []interface{}{extra(map[string]interface{}{"email": e})}})
		}, http.StatusOK},
		{"provision", provisionPath, func(e string) interface{} {
			return map[string]interface{}{"email": e, "displayName": "Provisioned", "note": "by token"}
		}, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email := itfUniqueName("inauth") + "@example.com"
			rec := doRequestWithToken(t, f.srv, key, http.MethodPost, tc.path, tc.body(email))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.status == http.StatusUnprocessableEntity {
				_, err := f.s.GetUserByEmail(ctx, email)
				assert.ErrorIs(t, err, store.ErrNotFound, "a refused request creates no record")
				return
			}
			u, err := f.s.GetUserByEmail(ctx, email)
			require.NoError(t, err)
			assert.Equal(t, store.UserStatusInvited, u.Status)
			assert.Equal(t, store.UserRoleMember, u.Role)
			require.NotNil(t, u.InvitedBy)
			assert.Equal(t, f.admin.ID, *u.InvitedBy, "the record names the token's holder")
			assert.Empty(t, allBindingsFor(t, f.s, u.ID), "an invited record carries no role binding")
			groups, err := f.s.GetEffectiveGroups(ctx, u.ID)
			require.NoError(t, err)
			assert.ElementsMatch(t, baseGroups, groups, "an invited record gets no group membership from the request")
		})
	}

	t.Run("invite code", func(t *testing.T) {
		rec := doRequestWithToken(t, f.srv, key, http.MethodPost, "/api/v1/admin/invites",
			extra(map[string]interface{}{"expiresIn": "1h", "maxUses": 1}))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp InviteCreateResponse
		decodeJSONBody(t, rec, &resp)
		require.NotNil(t, resp.Invite)
		stored, err := f.s.GetInviteCode(ctx, resp.Invite.ID)
		require.NoError(t, err)
		assert.Equal(t, f.admin.ID, stored.CreatedBy, "the code names the token's holder")
		assert.Nil(t, stored.IssuanceID)
	})
}
