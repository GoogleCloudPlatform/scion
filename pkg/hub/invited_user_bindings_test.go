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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
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

// An invited user's pre-recorded bindings give its requests nothing: a hub
// user token whose account is still invited is refused at authentication,
// before any binding is read, whereas a signed-in user with no access to
// the project gets the not-found answer.
func TestInvitedUser_PreRecordedBindings_TokenRefused(t *testing.T) {
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
		assert.Equal(t, http.StatusUnauthorized, invited.Code, "%s %s: %s", req.method, req.path, invited.Body.String())
		assert.JSONEq(t, `{"error":{"code":"unauthorized","message":"invalid access token"}}`, invited.Body.String(), "%s %s", req.method, req.path)
		assert.NotEqual(t, http.StatusOK, nonMember.Code, "%s %s", req.method, req.path)
		assert.NotEqual(t, http.StatusUnauthorized, nonMember.Code, "%s %s", req.method, req.path)
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

// Built-in system hub roles and hub-members membership cannot be assigned to
// an invited user before first sign-in, whether directly, via group nesting,
// or through GovernanceService.ReplaceRoleBinding.
func TestInvitedUser_BuiltInHubRolesAndHubMembersRefused(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol-hubguard", "carol-hubguard@test.com")
	active := &store.User{
		ID: tid("user-active-hubguard"), Email: "active-hubguard@test.com",
		DisplayName: "Active", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, active))

	hubMembers, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	builtInHubRoles := []string{
		store.SystemRoleHubAdmin,
		store.SystemRoleHubMember,
		store.SystemRoleHubViewer,
		store.SystemRoleGlobalCatalogAuthor,
	}

	t.Run("direct role-bindings by ID and email", func(t *testing.T) {
		for _, roleName := range builtInHubRoles {
			rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
			require.NoError(t, err)
			for _, principal := range []string{carol.ID, carol.Email} {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
					RoleDefinitionID: rd.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      principal,
					ScopeType:        store.RoleScopeSystem,
				})
				assert.Equal(t, http.StatusConflict, rec.Code, "%s (%s): %s", roleName, principal, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "hub roles are assigned when the user first signs in")
			}
		}
		assert.Empty(t, allBindingsFor(t, s, carol.ID))
	})

	t.Run("hub-members group add by ID and slug", func(t *testing.T) {
		for _, groupRef := range []string{hubMembers.ID, "hub-members"} {
			for _, memberRef := range []string{carol.ID, carol.Email} {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+groupRef+"/members", AddGroupMemberRequest{
					MemberType: store.GroupMemberTypeUser,
					MemberID:   memberRef,
					Role:       store.GroupMemberRoleMember,
				})
				assert.Equal(t, http.StatusConflict, rec.Code, "group=%s member=%s: %s", groupRef, memberRef, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "hub roles are assigned when the user first signs in")
			}
		}
		_, err := s.GetGroupMembership(ctx, hubMembers.ID, store.GroupMemberTypeUser, carol.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("group-mediated hub role bindings and nested groups", func(t *testing.T) {
		// Explicit group containing carol (via a nested child group) is
		// allowed for non-hub-role grouping, but cannot receive a built-in
		// hub role or be nested under hub-members.
		parentGroup := &store.Group{ID: tid("grp-inv-parent"), Name: "Invited Parent", Slug: "invited-parent", GroupType: store.GroupTypeExplicit}
		childGroup := &store.Group{ID: tid("grp-inv-child"), Name: "Invited Child", Slug: "invited-child", GroupType: store.GroupTypeExplicit}
		require.NoError(t, s.CreateGroup(ctx, parentGroup))
		require.NoError(t, s.CreateGroup(ctx, childGroup))

		// Adding carol to an ordinary explicit group succeeds.
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+childGroup.ID+"/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeUser,
			MemberID:   carol.ID,
			Role:       store.GroupMemberRoleMember,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+parentGroup.ID+"/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   childGroup.ID,
			Role:       store.GroupMemberRoleMember,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		// Binding a built-in system hub role to parentGroup (which transitively contains carol) is refused.
		viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
		require.NoError(t, err)
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: viewerRD.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      parentGroup.ID,
			ScopeType:        store.RoleScopeSystem,
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "hub roles are assigned when the user first signs in")

		// Nesting parentGroup into hub-members is refused.
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/hub-members/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   parentGroup.ID,
			Role:       store.GroupMemberRoleMember,
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "hub roles are assigned when the user first signs in")

		// Adding carol to a group that already holds a built-in system hub role (or is under hub-members) is refused.
		roleBearingGroup := &store.Group{ID: tid("grp-hub-viewer-holders"), Name: "Viewer Holders", Slug: "viewer-holders", GroupType: store.GroupTypeExplicit}
		require.NoError(t, s.CreateGroup(ctx, roleBearingGroup))
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: viewerRD.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      roleBearingGroup.ID,
			ScopeType:        store.RoleScopeSystem,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		// Active user can still join roleBearingGroup.
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+roleBearingGroup.ID+"/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeUser,
			MemberID:   active.ID,
			Role:       store.GroupMemberRoleMember,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		// Invited user cannot join roleBearingGroup directly or via group nesting.
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+roleBearingGroup.ID+"/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeUser,
			MemberID:   carol.ID,
			Role:       store.GroupMemberRoleMember,
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "hub roles are assigned when the user first signs in")

		rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+roleBearingGroup.ID+"/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   parentGroup.ID,
			Role:       store.GroupMemberRoleMember,
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "hub roles are assigned when the user first signs in")

		// Group created under hub-members also refuses invited members.
		subHubMembers := &store.Group{
			ID: tid("grp-sub-hub-members"), Name: "Sub Hub Members", Slug: "sub-hub-members",
			GroupType: store.GroupTypeExplicit, ParentID: hubMembers.ID,
		}
		require.NoError(t, s.CreateGroup(ctx, subHubMembers))
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+subHubMembers.ID+"/members", AddGroupMemberRequest{
			MemberType: store.GroupMemberTypeUser,
			MemberID:   carol.ID,
			Role:       store.GroupMemberRoleMember,
		})
		assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "hub roles are assigned when the user first signs in")
	})

	t.Run("ReplaceRoleBinding refuses built-in hub role for invited user", func(t *testing.T) {
		customSys, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			Name: "invited-replace-custom-sys", ScopeType: store.RoleScopeSystem, Permissions: []string{"project.read"},
		})
		require.NoError(t, err)
		existing, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: customSys.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      carol.ID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        "test",
		})
		require.NoError(t, err)

		viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
		require.NoError(t, err)

		_, err = srv.governanceService.ReplaceRoleBinding(ctx, existing.ID, &store.RoleBinding{
			RoleDefinitionID: viewerRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      carol.ID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        "test",
		}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hub roles are assigned when the user first signs in")

		// Existing custom binding remains intact.
		stillThere, err := s.GetRoleBinding(ctx, existing.ID)
		require.NoError(t, err)
		assert.Equal(t, customSys.ID, stillThere.RoleDefinitionID)
	})
}

type failInvitedGuardLookupStore struct {
	store.Store
	fault        *storeFaultSwitch
	targetUserID string
	inTxOnly     bool
	inTx         bool
}

func (f *failInvitedGuardLookupStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if f.fault.Active() && id == f.targetUserID && (!f.inTxOnly || f.inTx) {
		return nil, errors.New("injected user lookup fault")
	}
	return f.Store.GetUser(ctx, id)
}

func (f *failInvitedGuardLookupStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !f.fault.Active() {
		return f.Store.WithTx(ctx, fn)
	}
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failInvitedGuardLookupStore{
			Store: tx, fault: f.fault, targetUserID: f.targetUserID, inTxOnly: f.inTxOnly, inTx: true,
		})
	})
}

func TestInvitedUser_HubRoleGuard_FailsClosedOnStoreFault(t *testing.T) {
	srv, s, wrapper, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *failInvitedGuardLookupStore {
		return &failInvitedGuardLookupStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	carol := newInvitedUser(t, s, "user-carol-guardfault", "carol-guardfault@test.com")
	wrapper.targetUserID = carol.ID

	viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	hubMembers, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	// Fault only inside WithTx so createRoleBinding's pre-tx test-identity
	// check passes and rejectInvitedHubRoleBinding's transactional GetUser
	// fails closed.
	wrapper.inTxOnly = true
	fault.Arm()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: viewerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      carol.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	// Group add: both pre-tx and in-tx guard lookups fail closed -> 500, no membership added.
	wrapper.inTxOnly = false
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/hub-members/members", AddGroupMemberRequest{
		MemberType: store.GroupMemberTypeUser,
		MemberID:   carol.Email,
		Role:       store.GroupMemberRoleMember,
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	assert.Empty(t, allBindingsFor(t, s, carol.ID))
	_, err = s.GetGroupMembership(ctx, hubMembers.ID, store.GroupMemberTypeUser, carol.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

type nilGetGroupStore struct {
	store.Store
}

func (nilGetGroupStore) GetGroup(context.Context, string) (*store.Group, error) {
	return nil, nil
}

func TestGroupClosureCarriesBuiltInHubRole_NilGroupFailsClosed(t *testing.T) {
	carries, err := groupClosureCarriesBuiltInHubRole(context.Background(), nilGetGroupStore{}, "group-1")
	require.Error(t, err)
	assert.False(t, carries)
}

type getUserCountStore struct {
	store.Store
	targetID string
	count    atomic.Int64
}

func (c *getUserCountStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == c.targetID {
		c.count.Add(1)
	}
	return c.Store.GetUser(ctx, id)
}

// Grouped project member responses expose principalStatus for user principals
// ("invited" before first sign-in, "active" after) using a single memoized
// GetUser per user principal, and omit principalStatus for non-user principals.
func TestProjectMembers_PrincipalStatus(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	counter, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *getUserCountStore {
		return &getUserCountStore{Store: inner}
	})
	ctx := context.Background()

	carol := newInvitedUser(t, s, "user-carol-status", "carol-status@test.com")
	counter.targetID = carol.ID

	grp := &store.Group{ID: tid("grp-status-check"), Name: "Status Group", Slug: "status-group", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, grp))
	agent := &store.Agent{
		ID: tid("agent-status-check"), Slug: "agent-status-check", Name: "agent-status-check",
		ProjectID: project.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	customRD := createCustomRoleDef(t, s, "status-custom-role", []string{"project.update"})

	// PUT on invited user returns principalStatus: "invited".
	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, carol.Email),
		map[string]interface{}{"roleDefinitionIds": []string{memberRD.ID, customRD.ID}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var putBody map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &putBody))
	assert.Equal(t, store.UserStatusInvited, putBody["principalStatus"])

	// Add group and agent members too.
	for _, p := range []struct{ pType, pID string }{
		{store.RoleBindingPrincipalGroup, grp.ID},
		{store.RoleBindingPrincipalAgent, agent.ID},
	} {
		rec := doRequest(t, srv, http.MethodPut,
			fmt.Sprintf("/api/v1/projects/%s/members/principals/%s/%s", project.ID, p.pType, p.pID),
			map[string]interface{}{"roleDefinitionIds": []string{memberRD.ID}})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	counter.count.Store(0)
	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/members?groupBy=principal", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(1), counter.count.Load(), "memoized user lookup runs once for carol across multiple bindings")

	var listBody struct {
		Items []map[string]interface{} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listBody))
	byPrincipal := map[string]map[string]interface{}{}
	for _, item := range listBody.Items {
		byPrincipal[fmt.Sprintf("%s:%s", item["principalType"], item["principalId"])] = item
	}

	assert.Equal(t, store.UserStatusActive, byPrincipal["user:"+alice.ID]["principalStatus"])
	assert.Equal(t, store.UserStatusInvited, byPrincipal["user:"+carol.ID]["principalStatus"])
	_, grpHasStatus := byPrincipal["group:"+grp.ID]["principalStatus"]
	assert.False(t, grpHasStatus, "group principal omits principalStatus")
	_, agentHasStatus := byPrincipal["agent:"+agent.ID]["principalStatus"]
	assert.False(t, agentHasStatus, "agent principal omits principalStatus")

	// After carol signs in, grouped GET reports principalStatus: "active".
	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: carol.Email, DisplayName: "Carol"})
	require.NoError(t, err)

	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/members?groupBy=principal", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listBody))
	for _, item := range listBody.Items {
		if item["principalType"] == "user" && item["principalId"] == carol.ID {
			assert.Equal(t, store.UserStatusActive, item["principalStatus"])
		}
	}
}

// An invited project owner does not count as a usable owner until first sign-in:
// removing the sole active owner fails with 409 last_owner while removing the
// invited owner succeeds, and once the invited owner signs in, removing the
// original owner succeeds.
func TestInvitedOwner_NotUsableUntilSignIn(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()
	carol := newInvitedUser(t, s, "user-carol-owner", "carol-owner@test.com")

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	// Assign carol as an invited owner.
	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, carol.Email),
		map[string]interface{}{"roleDefinitionIds": []string{ownerRD.ID}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Removing alice (the only active owner) fails with 409 last_owner.
	rec = doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, alice.ID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "last_owner")

	// Removing carol (invited owner) succeeds.
	rec = doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, carol.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Re-add carol as owner, activate via first sign-in, then remove alice -> succeeds.
	rec = doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, carol.Email),
		map[string]interface{}{"roleDefinitionIds": []string{ownerRD.ID}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	signedIn, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: carol.Email, DisplayName: "Carol Owner"})
	require.NoError(t, err)
	require.Equal(t, carol.ID, signedIn.ID)
	require.Equal(t, store.UserStatusActive, signedIn.Status)

	rec = doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/members/principals/user/%s", project.ID, alice.ID), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

type invitedGuardLockRecorder struct {
	mu                 sync.Mutex
	txLockCalls        int
	outsideTxLockCalls int
	readBeforeTxLock   bool
	failLock           bool
}

func (r *invitedGuardLockRecorder) reset(failLock bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.txLockCalls = 0
	r.outsideTxLockCalls = 0
	r.readBeforeTxLock = false
	r.failLock = failLock
}

func (r *invitedGuardLockRecorder) snapshot() (txCalls, outsideCalls int, readBefore bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.txLockCalls, r.outsideTxLockCalls, r.readBeforeTxLock
}

type recordingInvitedGuardLockStore struct {
	store.Store
	fault  *storeFaultSwitch
	rec    *invitedGuardLockRecorder
	inTx   bool
	locked bool
}

func (w *recordingInvitedGuardLockStore) LockInvitedHubRoleGuard(ctx context.Context) error {
	if w.fault.Active() {
		w.rec.mu.Lock()
		if w.inTx {
			w.rec.txLockCalls++
			w.locked = true
		} else {
			w.rec.outsideTxLockCalls++
		}
		fail := w.rec.failLock
		w.rec.mu.Unlock()
		if fail {
			return errors.New("injected LockInvitedHubRoleGuard fault")
		}
	}
	return w.Store.LockInvitedHubRoleGuard(ctx)
}

func (w *recordingInvitedGuardLockStore) noteGuardClosureRead() {
	if !w.fault.Active() || !w.inTx {
		return
	}
	w.rec.mu.Lock()
	if !w.locked {
		w.rec.readBeforeTxLock = true
	}
	w.rec.mu.Unlock()
}

func (w *recordingInvitedGuardLockStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	w.noteGuardClosureRead()
	return w.Store.GetGroupMembers(ctx, groupID)
}

func (w *recordingInvitedGuardLockStore) GetParentGroups(ctx context.Context, groupID string) ([]string, error) {
	w.noteGuardClosureRead()
	return w.Store.GetParentGroups(ctx, groupID)
}

func (w *recordingInvitedGuardLockStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	w.noteGuardClosureRead()
	return w.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (w *recordingInvitedGuardLockStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !w.fault.Active() {
		return w.Store.WithTx(ctx, fn)
	}
	return w.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&recordingInvitedGuardLockStore{
			Store: tx,
			fault: w.fault,
			rec:   w.rec,
			inTx:  true,
		})
	})
}

// TestInvitedUser_HubRoleGuard_LockDisciplineAndHotPaths verifies that
// LockInvitedHubRoleGuard is acquired inside WithTx before reading group
// membership or ancestor closures on the narrow group-mediated hub-role guard
// paths, fails closed when lock acquisition errors, and is NOT acquired on
// hot paths such as active-user group adds or non-group/non-built-in role
// bindings (ptone/scion#8081).
func TestInvitedUser_HubRoleGuard_LockDisciplineAndHotPaths(t *testing.T) {
	recState := &invitedGuardLockRecorder{}
	srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, sw *storeFaultSwitch) *recordingInvitedGuardLockStore {
		return &recordingInvitedGuardLockStore{Store: inner, fault: sw, rec: recState}
	})
	ctx := context.Background()

	active := &store.User{
		ID: tid("usr-lock-active"), Email: "lock-active@test.com", DisplayName: "Lock Active",
		Role: store.UserRoleMember, Status: store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, active))
	carol := newInvitedUser(t, s, "usr-lock-carol", "lock-carol@test.com")

	plainGroup := &store.Group{ID: tid("grp-lock-plain"), Name: "Plain", Slug: "lock-plain", GroupType: store.GroupTypeExplicit}
	childGroup := &store.Group{ID: tid("grp-lock-child"), Name: "Child", Slug: "lock-child", GroupType: store.GroupTypeExplicit}
	roleGroup := &store.Group{ID: tid("grp-lock-role"), Name: "Role", Slug: "lock-role", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, plainGroup))
	require.NoError(t, s.CreateGroup(ctx, childGroup))
	require.NoError(t, s.CreateGroup(ctx, roleGroup))

	viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	customSysRD, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "lock-custom-sys", ScopeType: store.RoleScopeSystem, Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	fault.Arm()

	// Hot path 1: adding an active user to a group takes zero advisory locks.
	recState.reset(false)
	httpRec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+plainGroup.ID+"/members", AddGroupMemberRequest{
		MemberType: store.GroupMemberTypeUser,
		MemberID:   active.ID,
		Role:       store.GroupMemberRoleMember,
	})
	require.Equal(t, http.StatusCreated, httpRec.Code, httpRec.Body.String())
	txCalls, outsideCalls, _ := recState.snapshot()
	assert.Equal(t, 0, txCalls, "active-user group add must not acquire LockInvitedHubRoleGuard")
	assert.Equal(t, 0, outsideCalls)

	// Hot path 2: binding a built-in system role directly to a user takes zero advisory locks.
	recState.reset(false)
	httpRec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: viewerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      active.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.Equal(t, http.StatusCreated, httpRec.Code, httpRec.Body.String())
	txCalls, outsideCalls, _ = recState.snapshot()
	assert.Equal(t, 0, txCalls, "direct user role binding must not acquire LockInvitedHubRoleGuard")
	assert.Equal(t, 0, outsideCalls)

	// Hot path 3: binding a custom system role to a group takes zero advisory locks.
	recState.reset(false)
	httpRec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: customSysRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      plainGroup.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.Equal(t, http.StatusCreated, httpRec.Code, httpRec.Body.String())
	txCalls, outsideCalls, _ = recState.snapshot()
	assert.Equal(t, 0, txCalls, "custom system role binding on group must not acquire LockInvitedHubRoleGuard")
	assert.Equal(t, 0, outsideCalls)

	// Guarded path 1: binding a built-in system hub role to a group locks inside WithTx before reading members.
	recState.reset(false)
	httpRec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: viewerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      roleGroup.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.Equal(t, http.StatusCreated, httpRec.Code, httpRec.Body.String())
	txCalls, outsideCalls, readBefore := recState.snapshot()
	assert.Equal(t, 1, txCalls)
	assert.Equal(t, 0, outsideCalls)
	assert.False(t, readBefore, "group member read ran before LockInvitedHubRoleGuard")

	// Guarded path 2: adding an invited user to a group locks inside WithTx before reading closure.
	recState.reset(false)
	httpRec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+childGroup.ID+"/members", AddGroupMemberRequest{
		MemberType: store.GroupMemberTypeUser,
		MemberID:   carol.ID,
		Role:       store.GroupMemberRoleMember,
	})
	require.Equal(t, http.StatusCreated, httpRec.Code, httpRec.Body.String())
	txCalls, outsideCalls, readBefore = recState.snapshot()
	assert.Equal(t, 1, txCalls)
	assert.Equal(t, 0, outsideCalls)
	assert.False(t, readBefore, "group closure read ran before LockInvitedHubRoleGuard")

	// Guarded path 3: adding a child group to a parent group locks inside WithTx before reading closure/members.
	recState.reset(false)
	httpRec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+plainGroup.ID+"/members", AddGroupMemberRequest{
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   childGroup.ID,
		Role:       store.GroupMemberRoleMember,
	})
	require.Equal(t, http.StatusCreated, httpRec.Code, httpRec.Body.String())
	txCalls, outsideCalls, readBefore = recState.snapshot()
	assert.Equal(t, 1, txCalls)
	assert.Equal(t, 0, outsideCalls)
	assert.False(t, readBefore, "group nesting read ran before LockInvitedHubRoleGuard")

	// Guarded path 4: GovernanceService.ReplaceRoleBinding on a group built-in hub role locks inside WithTx first.
	replaceGroup := &store.Group{ID: tid("grp-lock-replace"), Name: "Replace", Slug: "lock-replace", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, replaceGroup))
	existingRB, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customSysRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      replaceGroup.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	gs := &GovernanceService{store: wrapped, nowFunc: time.Now}
	recState.reset(false)
	_, err = gs.ReplaceRoleBinding(ctx, existingRB.ID, &store.RoleBinding{
		RoleDefinitionID: viewerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      replaceGroup.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}, "")
	require.NoError(t, err)
	txCalls, outsideCalls, readBefore = recState.snapshot()
	assert.Equal(t, 1, txCalls)
	assert.Equal(t, 0, outsideCalls)
	assert.False(t, readBefore, "ReplaceRoleBinding read group members before LockInvitedHubRoleGuard")

	// Fail-closed check: lock failure aborts both role-binding creation and invited group-member add.
	failGroup := &store.Group{ID: tid("grp-lock-fail"), Name: "Fail", Slug: "lock-fail", GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, failGroup))
	recState.reset(true)
	httpRec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: viewerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      failGroup.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusInternalServerError, httpRec.Code, httpRec.Body.String())
	rbs, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, failGroup.ID)
	require.NoError(t, err)
	assert.Empty(t, rbs)

	recState.reset(true)
	httpRec = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+failGroup.ID+"/members", AddGroupMemberRequest{
		MemberType: store.GroupMemberTypeUser,
		MemberID:   carol.ID,
		Role:       store.GroupMemberRoleMember,
	})
	assert.Equal(t, http.StatusInternalServerError, httpRec.Code, httpRec.Body.String())
	_, err = s.GetGroupMembership(ctx, failGroup.ID, store.GroupMemberTypeUser, carol.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func runInvitedHubRoleGuardConcurrentRaces(t *testing.T, srv *Server, s store.Store) {
	t.Helper()
	ctx := context.Background()

	carol := newInvitedUser(t, s, "usr-race-carol", "race-carol@test.com")
	viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	hubMembers, err := s.GetGroupBySlug(ctx, hubMembersSlug)
	require.NoError(t, err)

	assertNoTransitiveHubRole := func(step string) {
		t.Helper()
		effGroups, err := s.GetEffectiveGroups(ctx, carol.ID)
		require.NoError(t, err, step)
		for _, gid := range effGroups {
			require.NotEqual(t, hubMembers.ID, gid, "%s: invited user gained hub-members membership", step)
		}
		if len(effGroups) == 0 {
			return
		}
		refs := make([]store.PrincipalRef, 0, len(effGroups))
		for _, gid := range effGroups {
			refs = append(refs, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: gid})
		}
		rbs, err := s.ListRoleBindingsForPrincipals(ctx, refs, []string{store.RoleScopeSystem}, nil)
		require.NoError(t, err, step)
		for _, b := range rbs {
			rd, rdErr := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
			require.NoError(t, rdErr, step)
			require.False(t, isBuiltInSystemHubRoleDef(rd),
				"%s: invited user transitively gained built-in system hub role %s via group %s", step, rd.Name, b.PrincipalID)
		}
	}

	assertRacePair := func(step string, recA, recB *httptest.ResponseRecorder) {
		t.Helper()
		require.False(t, recA.Code == http.StatusCreated && recB.Code == http.StatusCreated,
			"%s: write skew allowed both mutations to commit (A=%d %s, B=%d %s)",
			step, recA.Code, recA.Body.String(), recB.Code, recB.Body.String())
		for label, rec := range map[string]*httptest.ResponseRecorder{"A": recA, "B": recB} {
			switch rec.Code {
			case http.StatusCreated:
			case http.StatusConflict:
				assert.Contains(t, rec.Body.String(), errHubRoleOnInvitedUser.Error(), "%s (%s)", step, label)
			default:
				t.Fatalf("%s (%s): unexpected status %d: %s", step, label, rec.Code, rec.Body.String())
			}
		}
		assert.True(t, recA.Code == http.StatusConflict || recB.Code == http.StatusConflict,
			"%s: expected at least one 409 Conflict", step)
	}

	const iterations = 8
	for iter := 0; iter < iterations; iter++ {
		// Shape 1: direct group role binding vs adding invited user to the same group.
		directGrp := &store.Group{
			ID:        tid(fmt.Sprintf("grp-race-direct-%d", iter)),
			Name:      fmt.Sprintf("Race Direct %d", iter),
			Slug:      fmt.Sprintf("race-direct-%d", iter),
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, directGrp))

		start := make(chan struct{})
		var wg sync.WaitGroup
		var recBind, recAdd *httptest.ResponseRecorder
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			recBind = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
				RoleDefinitionID: viewerRD.ID,
				PrincipalType:    store.RoleBindingPrincipalGroup,
				PrincipalID:      directGrp.ID,
				ScopeType:        store.RoleScopeSystem,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			recAdd = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+directGrp.ID+"/members", AddGroupMemberRequest{
				MemberType: store.GroupMemberTypeUser,
				MemberID:   carol.ID,
				Role:       store.GroupMemberRoleMember,
			})
		}()
		close(start)
		wg.Wait()
		assertRacePair(fmt.Sprintf("direct-%d", iter), recBind, recAdd)
		assertNoTransitiveHubRole(fmt.Sprintf("direct-%d", iter))

		// Shape 2: role binding on parent group P vs adding invited user to nested child group C.
		parentGrp := &store.Group{
			ID:        tid(fmt.Sprintf("grp-race-parent-%d", iter)),
			Name:      fmt.Sprintf("Race Parent %d", iter),
			Slug:      fmt.Sprintf("race-parent-%d", iter),
			GroupType: store.GroupTypeExplicit,
		}
		childGrp := &store.Group{
			ID:        tid(fmt.Sprintf("grp-race-child-%d", iter)),
			Name:      fmt.Sprintf("Race Child %d", iter),
			Slug:      fmt.Sprintf("race-child-%d", iter),
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, parentGrp))
		require.NoError(t, s.CreateGroup(ctx, childGrp))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    parentGrp.ID,
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   childGrp.ID,
			Role:       store.GroupMemberRoleMember,
		}))

		start = make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			recBind = doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
				RoleDefinitionID: viewerRD.ID,
				PrincipalType:    store.RoleBindingPrincipalGroup,
				PrincipalID:      parentGrp.ID,
				ScopeType:        store.RoleScopeSystem,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			recAdd = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+childGrp.ID+"/members", AddGroupMemberRequest{
				MemberType: store.GroupMemberTypeUser,
				MemberID:   carol.ID,
				Role:       store.GroupMemberRoleMember,
			})
		}()
		close(start)
		wg.Wait()
		assertRacePair(fmt.Sprintf("nested-bind-%d", iter), recBind, recAdd)
		assertNoTransitiveHubRole(fmt.Sprintf("nested-bind-%d", iter))

		// Shape 3: nesting child group C under role-bearing parent P vs adding invited user to C.
		roleParent := &store.Group{
			ID:        tid(fmt.Sprintf("grp-race-rparent-%d", iter)),
			Name:      fmt.Sprintf("Race Role Parent %d", iter),
			Slug:      fmt.Sprintf("race-rparent-%d", iter),
			GroupType: store.GroupTypeExplicit,
		}
		nestChild := &store.Group{
			ID:        tid(fmt.Sprintf("grp-race-nchild-%d", iter)),
			Name:      fmt.Sprintf("Race Nest Child %d", iter),
			Slug:      fmt.Sprintf("race-nchild-%d", iter),
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, roleParent))
		require.NoError(t, s.CreateGroup(ctx, nestChild))
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: viewerRD.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      roleParent.ID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        "test",
		})
		require.NoError(t, err)

		start = make(chan struct{})
		var recNest *httptest.ResponseRecorder
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			recNest = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+roleParent.ID+"/members", AddGroupMemberRequest{
				MemberType: store.GroupMemberTypeGroup,
				MemberID:   nestChild.ID,
				Role:       store.GroupMemberRoleMember,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			recAdd = doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+nestChild.ID+"/members", AddGroupMemberRequest{
				MemberType: store.GroupMemberTypeUser,
				MemberID:   carol.ID,
				Role:       store.GroupMemberRoleMember,
			})
		}()
		close(start)
		wg.Wait()
		assertRacePair(fmt.Sprintf("nested-edge-%d", iter), recNest, recAdd)
		assertNoTransitiveHubRole(fmt.Sprintf("nested-edge-%d", iter))
	}
}

func TestInvitedUser_HubRoleGuard_ConcurrentGroupBindAndAdd(t *testing.T) {
	srv, s := testServer(t)
	runInvitedHubRoleGuardConcurrentRaces(t, srv, s)
}

// TestInvitedUser_HubRoleGuard_ConcurrentGroupBindAndAdd_Postgres exercises the
// same concurrent group hub-role binding and invited-member/child-group races
// under PostgreSQL READ COMMITTED (wired into HUB_INTEGRATION_POSTGRES_TESTS),
// where LockInvitedHubRoleGuard is what prevents write skew (ptone/scion#8081).
func TestInvitedUser_HubRoleGuard_ConcurrentGroupBindAndAdd_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	srv, s := testServerWithStore(t, entadapter.NewCompositeStore(enttest.NewClient(t)))
	runInvitedHubRoleGuardConcurrentRaces(t, srv, s)
}
