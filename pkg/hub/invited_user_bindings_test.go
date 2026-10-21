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

// Invited users' memberships and role bindings take effect at first
// sign-in: decide's account-status gate, token refresh and web sign-in,
// for users created by invite, allow list or POST /api/v1/users.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// invitedBindingsFixture is an invited user carol holding, recorded through
// the admin API, a project-member membership on project, a custom
// project-scoped role (project.update) on project, and a custom
// system-scoped role (project.update) that reaches other.
type invitedBindingsFixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	other   *store.Project
	carol   *store.User
}

func newInvitedBindingsFixture(t *testing.T) *invitedBindingsFixture {
	t.Helper()
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	other := newTestProject(t, s, "project-other", "Other Project")
	carol := newInvitedUser(t, s, "user-carol", "carol@test.com")

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, carol.Email),
		map[string]interface{}{"roleDefinitionIds": []string{memberRD.ID}})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())

	projRole := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name: "invited-project-updater", ScopeType: store.RoleScopeProject, Permissions: []string{"project.update"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: projRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: carol.Email,
		ScopeType: store.RoleScopeProject, ScopeID: project.ID,
	})
	sysRole := createRoleViaAPI(t, srv, createRoleDefinitionRequest{
		Name: "invited-system-updater", ScopeType: store.RoleScopeSystem, Permissions: []string{"project.update"},
	})
	createBindingViaAPI(t, srv, createRoleBindingRequest{
		RoleDefinitionID: sysRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: carol.Email,
		ScopeType: store.RoleScopeSystem,
	})
	require.Len(t, allBindingsFor(t, s, carol.ID), 3, "all three bindings are recorded on the invited row")

	return &invitedBindingsFixture{srv: srv, s: s, project: project, other: other, carol: carol}
}

// checks are the three decisions the recorded bindings would admit once
// carol is active: read (membership), update (custom project role) and
// update on a project carol holds no project binding on (custom system
// role).
func (f *invitedBindingsFixture) checks() []struct {
	name string
	res  Resource
	perm string
} {
	return []struct {
		name string
		res  Resource
		perm string
	}{
		{"membership read", projectResource(f.project), "project.read"},
		{"custom project role", projectResource(f.project), "project.update"},
		{"custom system role", projectResource(f.other), "project.update"},
	}
}

func (f *invitedBindingsFixture) decide(t *testing.T, u *store.User, res Resource, perm string) Decision {
	t.Helper()
	action, ok := registryActionFor(perm)
	require.True(t, ok, perm)
	return decidePerm(f.srv.authzService, NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)), res, action, perm, true)
}

func TestInvitedUser_PreRecordedBindings_InertUntilSignIn(t *testing.T) {
	f := newInvitedBindingsFixture(t)
	ctx := context.Background()

	for _, c := range f.checks() {
		d := f.decide(t, f.carol, c.res, c.perm)
		assert.False(t, d.Allowed, "%s: denied before first sign-in", c.name)
		assert.Equal(t, principalNotActiveReason, d.Reason, c.name)
		assert.Empty(t, d.DenyCause, c.name)
		require.NotNil(t, d.Provenance, c.name)
		assert.Equal(t, []string{principalNotActiveReason}, d.Provenance.DenyReasons, c.name)
		assert.Empty(t, d.Provenance.Grants, c.name)
	}
	member, _, err := f.srv.authzService.ProjectMembershipEvidence(ctx,
		principalContextForIdentity(NewAuthenticatedUser(f.carol.ID, f.carol.Email, "", f.carol.Role, string(ClientTypeWeb))), f.project.ID)
	assert.False(t, member)
	assert.ErrorIs(t, err, ErrProjectAccessDenied)

	// First sign-in activates the same row.
	signedIn, err := f.srv.provisionUser(ctx, &ExternalUserInfo{Email: f.carol.Email, DisplayName: "Carol"})
	require.NoError(t, err)
	require.Equal(t, f.carol.ID, signedIn.ID, "activation keeps the user ID")
	require.Equal(t, store.UserStatusActive, signedIn.Status)

	for _, c := range f.checks() {
		d := f.decide(t, signedIn, c.res, c.perm)
		assert.True(t, d.Allowed, "%s: allowed after first sign-in: %s", c.name, d.Reason)
	}
	member, _, err = f.srv.authzService.ProjectMembershipEvidence(ctx,
		principalContextForIdentity(NewAuthenticatedUser(signedIn.ID, signedIn.Email, "", signedIn.Role, string(ClientTypeWeb))), f.project.ID)
	require.NoError(t, err)
	assert.True(t, member)
}

// An invited user's request is answered exactly like that of a signed-in
// user with no access to the project.
func TestInvitedUser_PreRecordedBindings_SameResponseAsNonMember(t *testing.T) {
	f := newInvitedBindingsFixture(t)
	dave := &store.User{ID: tid("user-dave"), Email: "dave@test.com", DisplayName: "dave",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, f.s.CreateUser(context.Background(), dave))

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/projects/" + f.project.ID},
		{http.MethodPatch, "/api/v1/projects/" + f.other.ID},
	} {
		invited := doRequestAsUser(t, f.srv, f.carol, req.method, req.path, map[string]interface{}{})
		nonMember := doRequestAsUser(t, f.srv, dave, req.method, req.path, map[string]interface{}{})
		assert.NotEqual(t, http.StatusOK, invited.Code, "%s %s", req.method, req.path)
		assert.Equal(t, nonMember.Code, invited.Code, "%s %s", req.method, req.path)
		assert.JSONEq(t, nonMember.Body.String(), invited.Body.String(), "%s %s", req.method, req.path)
	}
}

// One predicate covers every account that is not active: suspended and
// missing local users are denied with the same reason as invited ones,
// whatever bindings they hold.
func TestDecide_AccountStatusGate_NotActiveStatuses(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	bind := func(userID string) {
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
			ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: "test",
		})
		require.NoError(t, err)
	}
	newUser := func(name, status string) *store.User {
		u := &store.User{ID: tid(name), Email: name + "@test.com", DisplayName: name,
			Role: store.UserRoleMember, Status: status, Created: time.Now()}
		require.NoError(t, s.CreateUser(ctx, u))
		bind(u.ID)
		return u
	}

	active := newUser("gate-active", store.UserStatusActive)
	suspended := newUser("gate-suspended", store.UserStatusSuspended)
	invited := newUser("gate-invited", store.UserStatusInvited)
	// No users row (the store does not accept a binding without one).
	missing := &store.User{ID: tid("gate-missing"), Email: "gate-missing@test.com", Role: store.UserRoleMember}

	read := func(u *store.User) Decision {
		return decidePerm(srv.authzService, NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)),
			projectResource(project), ActionRead, "project.read", false)
	}

	d := read(active)
	require.True(t, d.Allowed, "active member is allowed: %s", d.Reason)
	for name, u := range map[string]*store.User{"suspended": suspended, "invited": invited, "missing": missing} {
		d := read(u)
		assert.False(t, d.Allowed, name)
		assert.Equal(t, principalNotActiveReason, d.Reason, name)
		assert.Empty(t, d.DenyCause, name)
	}
}

// getUserCountingStore counts GetUser calls and fails them while fail is
// set.
type getUserCountingStore struct {
	store.Store
	calls atomic.Int32
	fail  atomic.Bool
}

var errGetUserFault = errors.New("injected user lookup fault")

func (s *getUserCountingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return nil, errGetUserFault
	}
	return s.Store.GetUser(ctx, id)
}

// A users-row lookup fault denies, tagged as a resolution error.
func TestDecide_AccountStatusGate_LookupFaultDenies(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	counting := &getUserCountingStore{Store: s}
	srv.authzService.store = counting
	ident := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, alice.Role, string(ClientTypeWeb))

	d := decidePerm(srv.authzService, ident, projectResource(project), ActionRead, "project.read", false)
	require.True(t, d.Allowed, "precondition: allowed without a fault: %s", d.Reason)

	counting.fail.Store(true)
	d = decidePerm(srv.authzService, ident, projectResource(project), ActionRead, "project.read", true)
	assert.False(t, d.Allowed)
	assert.Equal(t, principalStatusLookupReason, d.Reason)
	assert.Equal(t, DenyCauseResolutionError, d.DenyCause)
	assert.True(t, d.IsIndeterminate())
}

// With a request-local authz input memo, the gate reads the users row once
// for the whole phase; without one it reads it once per decision. A fault
// is not memoized.
func TestDecide_AccountStatusGate_UsesInputMemo(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	counting := &getUserCountingStore{Store: s}
	srv.authzService.store = counting
	ident := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, alice.Role, string(ClientTypeWeb))
	req := AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   projectResource(project),
		Action:     ActionRead,
		Permission: "project.read",
	}

	before := counting.calls.Load()
	for i := 0; i < 3; i++ {
		require.True(t, srv.authzService.Decide(context.Background(), req).Allowed)
	}
	assert.Equal(t, int32(3), counting.calls.Load()-before, "one lookup per decision without a memo")

	memoCtx := withAuthzInputMemo(context.Background())
	counting.fail.Store(true)
	assert.False(t, srv.authzService.Decide(memoCtx, req).Allowed, "fault denies")
	counting.fail.Store(false)
	before = counting.calls.Load()
	for i := 0; i < 3; i++ {
		require.True(t, srv.authzService.Decide(memoCtx, req).Allowed)
	}
	assert.Equal(t, int32(1), counting.calls.Load()-before, "one lookup for the phase with a memo; the fault was not stored")
}

// A refresh token issued before a user was removed does not renew tokens
// for a row later re-invited with the same email; the invited row is left
// as it was.
func TestAuthRefresh_ReinvitedEmail_Refused(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	old := &store.User{ID: tid("user-old-erin"), Email: "erin@test.com", DisplayName: "Erin",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, old))
	_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
		old.ID, old.Email, old.DisplayName, old.Role, ClientTypeWeb)
	require.NoError(t, err)
	require.NoError(t, s.DeleteUser(ctx, old.ID))

	erin := newInvitedUser(t, s, "user-erin", "erin@test.com")
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: erin.ID,
		ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	bindingsBefore := allBindingsFor(t, s, erin.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/refresh", AuthRefreshRequest{RefreshToken: refreshToken})
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotContains(t, body, "accessToken")
	assert.NotContains(t, body, "refreshToken")

	after, err := s.GetUser(ctx, erin.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusInvited, after.Status)
	assert.Equal(t, erin.Role, after.Role)
	assert.ElementsMatch(t, bindingsBefore, allBindingsFor(t, s, erin.ID))
}

// failUpdateUserStore fails every UpdateUser.
type failUpdateUserStore struct {
	*proxyAuthStore
}

func (s *failUpdateUserStore) UpdateUser(context.Context, *store.User) error {
	return errors.New("injected update fault")
}

// requireNoSignedInSession asserts that the response carries no session
// with a signed-in user or hub tokens.
func requireNoSignedInSession(t *testing.T, ws *WebServer, rec *httptest.ResponseRecorder, reqCookies []*http.Cookie) {
	t.Helper()
	probe := httptest.NewRequest(http.MethodGet, "/", nil)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		cookies = reqCookies
	}
	for _, c := range cookies {
		probe.AddCookie(c)
	}
	sess, err := ws.sessionStore.Get(probe, webSessionName)
	require.NoError(t, err)
	assert.Nil(t, sess.Values[sessKeyUserID], "no signed-in user in the session")
	assert.Nil(t, sess.Values[sessKeyHubAccessToken], "no access token in the session")
	assert.Nil(t, sess.Values[sessKeyHubRefreshToken], "no refresh token in the session")
}

func TestOAuthCallback_InvitedUser_ActivationWriteFails_NoSession(t *testing.T) {
	const secret = "test-session-secret-for-activation-fault-12345678"
	invitedEmail := "activation-fault@example.com"

	ws := newTestWebServer(t, WebServerConfig{SessionSecret: secret, BaseURL: "http://localhost:8080"})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"}},
	}, nil)
	ws.oauthService.httpClient = &http.Client{Transport: &mockOAuthTransport{
		tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
		userinfoJSON: `{"id":"fault-id","email":"` + invitedEmail + `","verified_email":true,"name":"Fault"}`,
	}}
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)

	inner := newProxyAuthStoreWithRoles()
	require.NoError(t, inner.CreateUser(context.Background(), &store.User{
		ID: "u-activation-fault", Email: invitedEmail, Role: "member", Status: store.UserStatusInvited, Created: time.Now(),
	}))
	ws.store = &failUpdateUserStore{proxyAuthStore: inner}
	ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{"other@example.com"}})

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	sess.Values[sessKeyOAuthState] = "state-activation-fault"
	require.NoError(t, sess.Save(reqSetup, recSetup))
	cookies := recSetup.Result().Cookies()

	reqCallback := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=test-code&state=state-activation-fault", nil)
	for _, c := range cookies {
		reqCallback.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, reqCallback)

	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/login?error=user_create_failed", rec.Header().Get("Location"))
	requireNoSignedInSession(t, ws, rec, cookies)
}

func TestProxyAuthMiddleware_InvitedUser_ActivationWriteFails_NoSession(t *testing.T) {
	mockAuth := &mockProxyAuthenticator{user: &ProxyUserInfo{
		Subject: "sa-activation-fault", Email: "proxy-activation-fault@example.com",
		DisplayName: "Proxy Fault", Domain: "example.com",
	}}
	inner := newProxyAuthStoreWithRoles()
	require.NoError(t, inner.CreateUser(context.Background(), &store.User{
		ID: "u-proxy-activation-fault", Email: "proxy-activation-fault@example.com",
		Role: "member", Status: store.UserStatusInvited, Created: time.Now(),
	}))

	ws := newTestWebServer(t, WebServerConfig{AuthMode: "proxy", ProxyAuthenticator: mockAuth})
	ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{"other@example.com"}})
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.SetStore(&failUpdateUserStore{proxyAuthStore: inner})

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	requireNoSignedInSession(t, ws, rec, nil)
}

// Pre-recorded bindings do not change the hub role computed at first
// sign-in or the hub role grants, and survive activation unchanged.
func TestInvitedUser_PreRecordedBindings_DoNotAlterActivationRole(t *testing.T) {
	for _, defaultRole := range []string{store.UserRoleMember, store.UserRoleViewer} {
		t.Run(defaultRole, func(t *testing.T) {
			srv, s := newLoginGrantServer(t, defaultRole, []string{"boss@example.com"})
			ctx := context.Background()
			project := newTestProject(t, s, "project-activation-"+defaultRole, "Activation "+defaultRole)

			createLoginGrantUser(t, s, "inv-plain-"+defaultRole, "plain-"+defaultRole+"@example.com", store.UserRoleMember, store.UserStatusInvited)
			bound := createLoginGrantUser(t, s, "inv-bound-"+defaultRole, "bound-"+defaultRole+"@example.com", store.UserRoleMember, store.UserStatusInvited)

			memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
			require.NoError(t, err)
			_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: bound.ID,
				ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: "test",
			})
			require.NoError(t, err)
			custom := createCustomRoleDef(t, s, "activation-custom-"+defaultRole, []string{"project.update"})
			createCustomBinding(t, s, custom.ID, bound.ID, project.ID)
			recorded := allBindingsFor(t, s, bound.ID)
			require.Len(t, recorded, 2)

			plainUser, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "plain-" + defaultRole + "@example.com"})
			require.NoError(t, err)
			boundUser, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "bound-" + defaultRole + "@example.com"})
			require.NoError(t, err)

			assert.Equal(t, defaultRole, boundUser.Role)
			assert.Equal(t, plainUser.Role, boundUser.Role)
			assert.Equal(t, plainUser.Status, boundUser.Status)
			assert.Equal(t, observeHubRoleGrants(t, s, plainUser.ID), observeHubRoleGrants(t, s, boundUser.ID))
			if defaultRole == store.UserRoleViewer {
				assertViewerGrants(t, srv, s, "bound-"+defaultRole+"@example.com")
			} else {
				assertMemberGrants(t, srv, s, "bound-"+defaultRole+"@example.com")
			}

			// The pre-recorded bindings are still there, unchanged; the only
			// additions are the hub role grants.
			after := map[string]*store.RoleBinding{}
			for _, b := range allBindingsFor(t, s, boundUser.ID) {
				after[b.ID] = b
			}
			for _, b := range recorded {
				got, ok := after[b.ID]
				require.True(t, ok, "binding %s survives activation", b.ID)
				assert.Equal(t, b.RoleDefinitionID, got.RoleDefinitionID)
				assert.Equal(t, b.ScopeType, got.ScopeType)
				assert.Equal(t, b.ScopeID, got.ScopeID)
			}
		})
	}
}

// The super-admin role still cannot be bound to an invited user through
// the role-binding API.
func TestInvitedUser_SuperAdminBindingStillRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol-sa", "carol-sa@test.com")
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: carol.ID,
		ScopeType: store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, allBindingsFor(t, s, carol.ID))
}

// failDeleteUserTxStore fails DeleteUser inside WithTx while its switch is
// armed.
type failDeleteUserTxStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *failDeleteUserTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !f.fault.Active() {
		return f.Store.WithTx(ctx, fn)
	}
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failDeleteUserInTx{Store: tx})
	})
}

type failDeleteUserInTx struct {
	store.Store
}

func (f *failDeleteUserInTx) DeleteUser(context.Context, string) error {
	return errors.New("injected delete fault")
}

// seedInvitedUserGrants gives an invited user a project membership, a
// custom project binding, a custom system binding and a group membership.
func seedInvitedUserGrants(t *testing.T, s store.Store, project *store.Project, userID string) {
	t.Helper()
	ctx := context.Background()
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: project.ID, CreatedBy: "test",
	})
	require.NoError(t, err)
	custom := createCustomRoleDef(t, s, "invited-delete-custom", []string{"project.update"})
	createCustomBinding(t, s, custom.ID, userID, project.ID)
	sys, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "invited-delete-system", ScopeType: store.RoleScopeSystem, Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: sys.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	group := &store.Group{ID: tid("group-invited-delete"), Name: "Invited Delete", Slug: "invited-delete", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, group))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: group.ID, MemberID: userID, MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember,
	}))
	require.Len(t, allBindingsFor(t, s, userID), 3)
	groups, err := s.GetUserGroups(ctx, userID)
	require.NoError(t, err)
	require.Len(t, groups, 1)
}

// Removing an invited user before sign-in removes the row, its bindings and
// its group memberships together.
func TestDeleteUser_InvitedUser_CascadesBindingsAndGroups(t *testing.T) {
	t.Run("removed", func(t *testing.T) {
		srv, s, _, _, project := setupDemoPolicyTest(t)
		ctx := context.Background()
		carol := newInvitedUser(t, s, "user-carol-del", "carol-del@test.com")
		seedInvitedUserGrants(t, s, project, carol.ID)

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+carol.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

		_, err := s.GetUser(ctx, carol.ID)
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.Empty(t, allBindingsFor(t, s, carol.ID))
		groups, err := s.GetUserGroups(ctx, carol.ID)
		require.NoError(t, err)
		assert.Empty(t, groups)
	})

	t.Run("rolled back together", func(t *testing.T) {
		srv, s, _, _, project, _, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *failDeleteUserTxStore {
			return &failDeleteUserTxStore{Store: inner, fault: f}
		})
		ctx := context.Background()
		carol := newInvitedUser(t, s, "user-carol-del", "carol-del@test.com")
		seedInvitedUserGrants(t, s, project, carol.ID)

		fault.Arm()
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+carol.ID, nil)
		require.NotEqual(t, http.StatusNoContent, rec.Code, rec.Body.String())

		_, err := s.GetUser(ctx, carol.ID)
		require.NoError(t, err, "the row is kept")
		assert.Len(t, allBindingsFor(t, s, carol.ID), 3, "bindings are kept")
		groups, err := s.GetUserGroups(ctx, carol.ID)
		require.NoError(t, err)
		assert.Len(t, groups, 1, "group membership is kept")
	})
}

// A user created through POST /api/v1/users is the same invited record:
// memberships and role bindings recorded for it take effect at first
// sign-in, on the same user ID.
func TestProvisionedUser_PreRecordedBindings_InertUntilSignIn(t *testing.T) {
	f := newProvisionFixture(t)
	ctx := context.Background()
	project := newTestProject(t, f.s, "project-provisioned", "Provisioned Project")
	other := newTestProject(t, f.s, "project-provisioned-other", "Provisioned Other")

	rec := provisionAs(t, f.srv, f.superAdmin, map[string]interface{}{"email": "Provisioned.Person@Example.com"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeProvisionResponse(t, rec)
	require.Equal(t, store.UserStatusInvited, resp.User.Status)
	provisioned, err := f.s.GetUser(ctx, resp.User.ID)
	require.NoError(t, err)

	memberRD, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, provisioned.Email),
		map[string]interface{}{"roleDefinitionIds": []string{memberRD.ID}})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
	projRole := createCustomRoleDef(t, f.s, "provisioned-project-updater", []string{"project.update"})
	sysRole, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "provisioned-system-updater", ScopeType: store.RoleScopeSystem, Permissions: []string{"project.update"},
	})
	require.NoError(t, err)
	for _, req := range []createRoleBindingRequest{
		{RoleDefinitionID: projRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: provisioned.Email,
			ScopeType: store.RoleScopeProject, ScopeID: project.ID},
		{RoleDefinitionID: sysRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: provisioned.Email,
			ScopeType: store.RoleScopeSystem},
	} {
		rec := doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, "/api/v1/admin/role-bindings", req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	require.Len(t, allBindingsFor(t, f.s, provisioned.ID), 3)

	checks := []struct {
		res  Resource
		perm string
	}{
		{projectResource(project), "project.read"},
		{projectResource(project), "project.update"},
		{projectResource(other), "project.update"},
	}
	decide := func(u *store.User, res Resource, perm string) Decision {
		action, ok := registryActionFor(perm)
		require.True(t, ok)
		return decidePerm(f.srv.authzService, NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb)), res, action, perm, false)
	}
	for _, c := range checks {
		d := decide(provisioned, c.res, c.perm)
		assert.False(t, d.Allowed, "%s on %s denied before first sign-in", c.perm, c.res.ID)
		assert.Equal(t, principalNotActiveReason, d.Reason)
	}

	signedIn, err := f.srv.provisionUser(ctx, &ExternalUserInfo{Email: provisioned.Email})
	require.NoError(t, err)
	require.Equal(t, provisioned.ID, signedIn.ID, "activation keeps the user ID")
	require.Equal(t, store.UserStatusActive, signedIn.Status)
	for _, c := range checks {
		d := decide(signedIn, c.res, c.perm)
		assert.True(t, d.Allowed, "%s on %s allowed after first sign-in: %s", c.perm, c.res.ID, d.Reason)
	}
}
