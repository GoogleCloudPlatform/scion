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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func grantTestEnableExperiment(t *testing.T, srv *Server, st store.Store, enabled bool) {
	t.Helper()
	val := "false"
	if enabled {
		val = "true"
	}
	doc := json.RawMessage(`{"overrides":{"` + experiments.CustomDefaultUserRole + `":` + val + `}}`)
	_, err := st.UpsertHubSetting(context.Background(), "experiments", doc, "test", -1, "managed")
	require.NoError(t, err)
	ops := srv.GetOperationalSettings()
	if ops == nil {
		ops = NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
		srv.SetOperationalSettings(ops)
	}
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
}

func grantTestSeedRole(t *testing.T, st store.Store, name, scopeType string, perms []string) *store.RoleDefinition {
	t.Helper()
	rd, err := st.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		Name:        name,
		Description: "test role " + name,
		ScopeType:   scopeType,
		Permissions: perms,
		System:      false,
	})
	require.NoError(t, err)
	return rd
}

func grantTestIsHubMember(t *testing.T, st store.Store, userID string) bool {
	t.Helper()
	ctx := context.Background()
	grp, err := st.GetGroupBySlug(ctx, hubMembersSlug)
	require.NoError(t, err)
	members, err := st.GetGroupMembers(ctx, grp.ID)
	require.NoError(t, err)
	for _, m := range members {
		if m.MemberType == store.GroupMemberTypeUser && m.MemberID == userID {
			return true
		}
	}
	return false
}

func grantTestSystemBindings(t *testing.T, st store.Store, userID string) []*store.RoleBinding {
	t.Helper()
	bindings, err := st.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	var out []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem {
			out = append(out, b)
		}
	}
	return out
}

func grantTestAssertCustomDefaultOnly(t *testing.T, st store.Store, userID, wantRoleDefID string) {
	t.Helper()
	assert.False(t, grantTestIsHubMember(t, st, userID), "user %s must not be in hub-members", userID)
	bindings := grantTestSystemBindings(t, st, userID)
	require.Len(t, bindings, 1, "expected exactly 1 system-scope binding for user %s", userID)
	assert.Equal(t, wantRoleDefID, bindings[0].RoleDefinitionID)
	assert.Equal(t, store.SystemDefaultRoleCreatedBy, bindings[0].CreatedBy)
	assert.Nil(t, bindings[0].NotBefore)
	assert.Nil(t, bindings[0].ExpiresAt)
}

func grantTestAssertBuiltinHubMember(t *testing.T, st store.Store, userID string) {
	t.Helper()
	assert.True(t, grantTestIsHubMember(t, st, userID), "user %s must be in hub-members", userID)
	for _, b := range grantTestSystemBindings(t, st, userID) {
		assert.NotEqual(t, store.SystemDefaultRoleCreatedBy, b.CreatedBy,
			"built-in member must not hold SystemDefaultRoleCreatedBy binding")
	}
}

func grantTestDriveOAuthLogin(t *testing.T, srv *Server, st store.Store, sub, email, displayName string) *store.User {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{BaseURL: "http://localhost:8080"})
	ws.SetStore(st)
	ws.SetAccessSettingsProvider(srv)
	ws.SetDemotionSafe(&srv.demotionSafe)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.SetOAuthService(NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{ClientID: "cid", ClientSecret: "csec"},
		},
	}, nil))
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockOAuthTransport{
			tokenJSON:    `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`,
			userinfoJSON: `{"id":"` + sub + `","email":"` + email + `","verified_email":true,"name":"` + displayName + `"}`,
		},
	}

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	oauthState := "st-" + sub
	sess.Values[sessKeyOAuthState] = oauthState
	require.NoError(t, sess.Save(reqSetup, recSetup))

	reqCb := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=ok&state="+oauthState, nil)
	for _, c := range recSetup.Result().Cookies() {
		reqCb.AddCookie(c)
	}
	recCb := httptest.NewRecorder()
	ws.Handler().ServeHTTP(recCb, reqCb)
	require.Equal(t, http.StatusFound, recCb.Code)
	require.Equal(t, "/", recCb.Header().Get("Location"))

	u, err := st.GetUserByEmail(context.Background(), email)
	require.NoError(t, err)
	return u
}

func grantTestDriveProxyLogin(t *testing.T, srv *Server, st store.Store, sub, email, displayName string) *store.User {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode: "proxy",
		ProxyAuthenticator: &mockProxyAuthenticator{
			user: &ProxyUserInfo{Subject: sub, Email: email, DisplayName: displayName},
		},
	})
	ws.SetStore(st)
	ws.SetAccessSettingsProvider(srv)
	ws.SetDemotionSafe(&srv.demotionSafe)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{AccessTokenDuration: DefaultGETokenTTL})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusForbidden, rec.Code)
	require.NotEqual(t, http.StatusInternalServerError, rec.Code)

	u, err := st.GetUserByEmail(context.Background(), email)
	require.NoError(t, err)
	return u
}

func TestCustomDefaultUserRole_NewUserReplacementAndIdempotence(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	grantTestEnableExperiment(t, srv, st, true)

	roleA := grantTestSeedRole(t, st, "custom-a", store.RoleScopeSystem, []string{"user.read", "broker.read"})
	roleB := grantTestSeedRole(t, st, "custom-b", store.RoleScopeSystem, []string{"user.read", "quota.read"})
	additive := grantTestSeedRole(t, st, "custom-additive", store.RoleScopeSystem, []string{"broker.read"})

	// Existing standard member (in hub-members) and existing member with an
	// additive admin-granted custom system role must stay untouched (Decision 5).
	existingMember := &store.User{
		ID: generateID(), Email: "existing@example.com", DisplayName: "Existing",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, existingMember))
	require.NoError(t, ensureHubMembershipTx(ctx, st, existingMember.ID))

	additiveMember := &store.User{
		ID: generateID(), Email: "additive@example.com", DisplayName: "Additive",
		Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, additiveMember))
	require.NoError(t, ensureHubMembershipTx(ctx, st, additiveMember.ID))
	_, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: additive.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      additiveMember.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "admin-user-1",
	})
	require.NoError(t, err)

	srv.config.DefaultUserRole = roleA.Name
	srv.config.UserAccessMode = "open"

	// New user receives User.Role == "member", direct SystemDefaultRoleCreatedBy binding,
	// and is NOT added to hub-members.
	u1, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "new1@example.com", DisplayName: "New One"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, u1.Role)
	grantTestAssertCustomDefaultOnly(t, st, u1.ID, roleA.ID)

	// Subsequent login, syncHubRoleGrants, and backfillUserRoleBindings must NOT
	// re-add u1 to hub-members or duplicate the binding.
	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: "new1@example.com", DisplayName: "New One"})
	require.NoError(t, err)
	require.NoError(t, syncHubRoleGrants(ctx, st, u1.ID, store.UserRoleMember, store.SystemReconcileCreatedBy))
	require.NoError(t, backfillUserRoleBindings(ctx, st))
	grantTestAssertCustomDefaultOnly(t, st, u1.ID, roleA.ID)

	// Existing members logging in remain in hub-members with no SystemDefaultRoleCreatedBy binding.
	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: existingMember.Email, DisplayName: "Existing"})
	require.NoError(t, err)
	grantTestAssertBuiltinHubMember(t, st, existingMember.ID)

	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: additiveMember.Email, DisplayName: "Additive"})
	require.NoError(t, err)
	assert.True(t, grantTestIsHubMember(t, st, additiveMember.ID))

	// Changing default_user_role from roleA to roleB leaves u1 on roleA and grants roleB to u2.
	srv.config.DefaultUserRole = roleB.Name
	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: "new1@example.com", DisplayName: "New One"})
	require.NoError(t, err)
	grantTestAssertCustomDefaultOnly(t, st, u1.ID, roleA.ID)

	u2, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "new2@example.com", DisplayName: "New Two"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, u2.Role)
	grantTestAssertCustomDefaultOnly(t, st, u2.ID, roleB.ID)
}

func TestCustomDefaultUserRole_AllProvisioningPaths(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	grantTestEnableExperiment(t, srv, st, true)

	customRole := grantTestSeedRole(t, st, "custom-all-paths", store.RoleScopeSystem, []string{"user.read", "broker.read"})
	srv.config.DefaultUserRole = customRole.Name
	srv.config.UserAccessMode = "open"

	resolver := NewGoogleIdentityResolver(
		st,
		st,
		srv.isUserAuthorized,
		func(ctx context.Context, email string) string {
			return srv.getUserRole(ctx, email, "", "")
		},
		slog.Default(),
	)
	resolver.SetSignInPolicyDeps(srv.signInPolicyDeps())

	seedInvited := func(email string) *store.User {
		u := &store.User{
			ID: generateID(), Email: email, DisplayName: "Invited",
			Role: store.UserRoleMember, Status: store.UserStatusInvited, Created: time.Now(),
		}
		require.NoError(t, st.CreateUser(ctx, u))
		return u
	}

	// 1. provisionUser: new + invited activation
	uProvNew, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "prov-new@example.com", DisplayName: "Prov New"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, uProvNew.Role)
	grantTestAssertCustomDefaultOnly(t, st, uProvNew.ID, customRole.ID)

	invProv := seedInvited("prov-inv@example.com")
	uProvInv, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: invProv.Email, DisplayName: "Prov Inv"})
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, uProvInv.Status)
	assert.Equal(t, store.UserRoleMember, uProvInv.Role)
	grantTestAssertCustomDefaultOnly(t, st, uProvInv.ID, customRole.ID)

	// 2. WebServer.handleOAuthCallback: new + invited activation + subsequent login
	uOAuthNew := grantTestDriveOAuthLogin(t, srv, st, "sub-oauth-new", "oauth-new@example.com", "OAuth New")
	assert.Equal(t, store.UserRoleMember, uOAuthNew.Role)
	grantTestAssertCustomDefaultOnly(t, st, uOAuthNew.ID, customRole.ID)
	_ = grantTestDriveOAuthLogin(t, srv, st, "sub-oauth-new", "oauth-new@example.com", "OAuth New")
	grantTestAssertCustomDefaultOnly(t, st, uOAuthNew.ID, customRole.ID)

	invOAuth := seedInvited("oauth-inv@example.com")
	uOAuthInv := grantTestDriveOAuthLogin(t, srv, st, "sub-oauth-inv", invOAuth.Email, "OAuth Inv")
	assert.Equal(t, store.UserStatusActive, uOAuthInv.Status)
	grantTestAssertCustomDefaultOnly(t, st, uOAuthInv.ID, customRole.ID)

	// 3. WebServer.proxyAuthMiddleware: new + invited activation + subsequent login
	uProxyNew := grantTestDriveProxyLogin(t, srv, st, "sub-proxy-new", "proxy-new@example.com", "Proxy New")
	assert.Equal(t, store.UserRoleMember, uProxyNew.Role)
	grantTestAssertCustomDefaultOnly(t, st, uProxyNew.ID, customRole.ID)
	_ = grantTestDriveProxyLogin(t, srv, st, "sub-proxy-new", "proxy-new@example.com", "Proxy New")
	grantTestAssertCustomDefaultOnly(t, st, uProxyNew.ID, customRole.ID)

	invProxy := seedInvited("proxy-inv@example.com")
	uProxyInv := grantTestDriveProxyLogin(t, srv, st, "sub-proxy-inv", invProxy.Email, "Proxy Inv")
	assert.Equal(t, store.UserStatusActive, uProxyInv.Status)
	grantTestAssertCustomDefaultOnly(t, st, uProxyInv.ID, customRole.ID)

	// 4. GoogleIdentityResolver.Resolve: new (provisionNewUser) + invited activation + bound re-resolve
	uResNew, err := resolver.Resolve(ctx, &ValidatedGoogleIdentity{
		Subject: "sub-res-new", Email: "res-new@gmail.com", EmailVerified: true,
		DisplayName: "Res New", Issuer: googleCanonicalIssuer, UpstreamExpiry: time.Now().Add(time.Hour),
	}, ResolvePolicy{})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, uResNew.Role)
	grantTestAssertCustomDefaultOnly(t, st, uResNew.ID, customRole.ID)

	// Subsequent resolution via bound identity keeps custom default binding and never adds hub-members.
	_, err = resolver.Resolve(ctx, &ValidatedGoogleIdentity{
		Subject: "sub-res-new", Email: "res-new@gmail.com", EmailVerified: true,
		DisplayName: "Res New", Issuer: googleCanonicalIssuer, UpstreamExpiry: time.Now().Add(time.Hour),
	}, ResolvePolicy{})
	require.NoError(t, err)
	grantTestAssertCustomDefaultOnly(t, st, uResNew.ID, customRole.ID)

	invRes := seedInvited("res-inv@gmail.com")
	uResInv, err := resolver.Resolve(ctx, &ValidatedGoogleIdentity{
		Subject: "sub-res-inv", Email: invRes.Email, EmailVerified: true,
		DisplayName: "Res Inv", Issuer: googleCanonicalIssuer, UpstreamExpiry: time.Now().Add(time.Hour),
	}, ResolvePolicy{})
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusActive, uResInv.Status)
	grantTestAssertCustomDefaultOnly(t, st, uResInv.ID, customRole.ID)
}

func TestCustomDefaultUserRole_AdminDemotion(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	grantTestEnableExperiment(t, srv, st, true)

	customRole := grantTestSeedRole(t, st, "custom-demote", store.RoleScopeSystem, []string{"user.read"})
	srv.config.DefaultUserRole = customRole.Name
	srv.config.AdminEmails = []string{"keep-admin@example.com"}
	srv.config.UserAccessMode = "open"
	srv.demotionSafe.Store(true)

	keepAdmin := &store.User{
		ID: generateID(), Email: "keep-admin@example.com", DisplayName: "Keep",
		Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, keepAdmin))
	srv.ensureSuperAdminBinding(ctx, keepAdmin.ID)

	seedDemotableAdmin := func(name, email string) *store.User {
		u := &store.User{
			ID: generateID(), Email: email, DisplayName: name,
			Role: store.UserRoleAdmin, Status: store.UserStatusActive, Created: time.Now(),
		}
		require.NoError(t, st.CreateUser(ctx, u))
		srv.ensureSuperAdminBinding(ctx, u.ID)
		// Also place in hub-members to verify demotion strips hub-members too.
		require.NoError(t, ensureHubMembershipTx(ctx, st, u.ID))
		return u
	}

	// 1. Login-time demotion via provisionUser.
	demoteLogin := seedDemotableAdmin("demote-login", "demote-login@example.com")
	u, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: demoteLogin.Email, DisplayName: "Demote Login"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, u.Role)
	grantTestAssertCustomDefaultOnly(t, st, demoteLogin.ID, customRole.ID)

	// 2. Token refresh demotion.
	demoteRefresh := seedDemotableAdmin("demote-refresh", "demote-refresh@example.com")
	_, refreshTok, _, err := srv.userTokenService.MintTokenPair(
		demoteRefresh.ID, demoteRefresh.Email, demoteRefresh.DisplayName, store.UserRoleAdmin, 0, ClientTypeCLI,
	)
	require.NoError(t, err)
	body, _ := json.Marshal(map[string]string{"refreshToken": refreshTok})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleAuthRefresh(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	grantTestAssertCustomDefaultOnly(t, st, demoteRefresh.ID, customRole.ID)

	// 3. Web OAuth and proxy-auth demotion.
	demoteOAuth := seedDemotableAdmin("demote-oauth", "demote-oauth@example.com")
	_ = grantTestDriveOAuthLogin(t, srv, st, "sub-demote-oauth", demoteOAuth.Email, "Demote OAuth")
	grantTestAssertCustomDefaultOnly(t, st, demoteOAuth.ID, customRole.ID)

	demoteProxy := seedDemotableAdmin("demote-proxy", "demote-proxy@example.com")
	_ = grantTestDriveProxyLogin(t, srv, st, "sub-demote-proxy", demoteProxy.Email, "Demote Proxy")
	grantTestAssertCustomDefaultOnly(t, st, demoteProxy.ID, customRole.ID)

	// 4. Startup ReconcileSuperAdminBindings with DB-configured access + experiments rows.
	demoteStartup := seedDemotableAdmin("demote-startup", "demote-startup@example.com")
	_, err = st.UpsertHubSetting(ctx, "access", json.RawMessage(`{"default_user_role":"`+customRole.Name+`"}`), "test", -1, "managed")
	require.NoError(t, err)
	// Simulate New() before OperationalSettings is attached: srvWithNilOps has no ops attached.
	srvWithNilOps := &Server{}
	startupRole, startupCustomOn := startupDefaultUserRoleConfig(ctx, st, srvWithNilOps, "member")
	assert.Equal(t, customRole.Name, startupRole)
	assert.True(t, startupCustomOn)

	safe, err := reconcileSuperAdminBindings(ctx, st, []string{keepAdmin.Email}, startupRole, startupCustomOn)
	require.NoError(t, err)
	assert.True(t, safe)
	gotStartup, err := st.GetUser(ctx, demoteStartup.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, gotStartup.Role)
	grantTestAssertCustomDefaultOnly(t, st, demoteStartup.ID, customRole.ID)

	// 5. Re-promotion to admin deletes SystemDefaultRoleCreatedBy binding, and a
	// second demotion under a new default role cleanly grants the new role without accumulation.
	roleB := grantTestSeedRole(t, st, "custom-demote-b", store.RoleScopeSystem, []string{"quota.read"})
	require.NoError(t, syncHubRoleGrants(ctx, st, demoteStartup.ID, store.UserRoleAdmin, store.SystemReconcileCreatedBy))
	assert.Empty(t, grantTestSystemBindings(t, st, demoteStartup.ID))
	require.NoError(t, applyDefaultUserRoleGrants(ctx, st, demoteStartup.ID, store.UserRoleMember, roleB.Name, true))
	grantTestAssertCustomDefaultOnly(t, st, demoteStartup.ID, roleB.ID)
}

func TestCustomDefaultUserRole_AdminOverride(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	grantTestEnableExperiment(t, srv, st, true)

	customRole := grantTestSeedRole(t, st, "custom-override", store.RoleScopeSystem, []string{"user.read"})
	srv.config.DefaultUserRole = customRole.Name
	srv.config.UserAccessMode = "open"

	u1, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "override1@example.com", DisplayName: "Override 1"})
	require.NoError(t, err)
	grantTestAssertCustomDefaultOnly(t, st, u1.ID, customRole.ID)

	u2, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "override2@example.com", DisplayName: "Override 2"})
	require.NoError(t, err)
	grantTestAssertCustomDefaultOnly(t, st, u2.ID, customRole.ID)

	// Admin PATCH to "member" replaces SystemDefaultRoleCreatedBy binding with hub-members.
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+u1.ID, map[string]string{"role": store.UserRoleMember})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	grantTestAssertBuiltinHubMember(t, st, u1.ID)

	// Subsequent login keeps u1 in hub-members.
	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: u1.Email, DisplayName: "Override 1"})
	require.NoError(t, err)
	grantTestAssertBuiltinHubMember(t, st, u1.ID)

	// Admin PATCH to "viewer" removes SystemDefaultRoleCreatedBy binding and grants hub-viewer.
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+u2.ID, map[string]string{"role": store.UserRoleViewer})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, grantTestIsHubMember(t, st, u2.ID))
	hubViewerRD, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings := grantTestSystemBindings(t, st, u2.ID)
	require.Len(t, bindings, 1)
	assert.Equal(t, hubViewerRD.ID, bindings[0].RoleDefinitionID)
}

func TestCustomDefaultUserRole_RuntimeFailClosedAndFallback(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	grantTestEnableExperiment(t, srv, st, true)

	activeRole := grantTestSeedRole(t, st, "custom-guarded", store.RoleScopeSystem, []string{"user.read"})
	ineligibleOther := grantTestSeedRole(t, st, "other-with-skill", store.RoleScopeSystem, []string{"user.read", "skill.read"})
	srv.config.DefaultUserRole = activeRole.Name
	srv.config.UserAccessMode = "open"

	// 1. Update guard blocks renaming an ineligible role onto the active default_user_role name.
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+ineligibleOther.ID, updateRoleDefinitionRequest{
		Name:        activeRole.Name,
		Description: "rename bypass attempt",
		Permissions: []string{"user.read", "skill.read"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 2. Delete + recreate bypass (0 bindings allows deletion; createRoleDefinition has no guard):
	// runtime resolveDefaultUserRole rechecks eligibility and fails closed to built-in hub-member.
	require.NoError(t, st.DeleteRoleDefinition(ctx, activeRole.ID))
	recreatedIneligible := grantTestSeedRole(t, st, activeRole.Name, store.RoleScopeSystem, []string{"user.read", "skill.read"})

	uRecreate, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "recreate-fallback@example.com", DisplayName: "Recreate"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, uRecreate.Role)
	grantTestAssertBuiltinHubMember(t, st, uRecreate.ID)
	for _, b := range grantTestSystemBindings(t, st, uRecreate.ID) {
		assert.NotEqual(t, recreatedIneligible.ID, b.RoleDefinitionID, "must never grant ineligible recreated role")
	}

	// 3. Missing role fallback (Decision 3): deleted/renamed role falls back to hub-member.
	require.NoError(t, st.DeleteRoleDefinition(ctx, recreatedIneligible.ID))
	uMissing, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "missing-fallback@example.com", DisplayName: "Missing"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, uMissing.Role)
	grantTestAssertBuiltinHubMember(t, st, uMissing.ID)

	// 4. Experiment disabled: new users fall back to hub-member, while existing
	// custom-default users keep their binding and are NOT added to hub-members.
	eligible := grantTestSeedRole(t, st, "custom-exp-toggle", store.RoleScopeSystem, []string{"user.read"})
	srv.config.DefaultUserRole = eligible.Name
	uBeforeOff, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "before-off@example.com", DisplayName: "Before Off"})
	require.NoError(t, err)
	grantTestAssertCustomDefaultOnly(t, st, uBeforeOff.ID, eligible.ID)

	grantTestEnableExperiment(t, srv, st, false)
	uAfterOff, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "after-off@example.com", DisplayName: "After Off"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, uAfterOff.Role)
	grantTestAssertBuiltinHubMember(t, st, uAfterOff.ID)

	// Previously provisioned user keeps custom binding on subsequent login and backfill.
	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: uBeforeOff.Email, DisplayName: "Before Off"})
	require.NoError(t, err)
	require.NoError(t, backfillUserRoleBindings(ctx, st))
	grantTestAssertCustomDefaultOnly(t, st, uBeforeOff.ID, eligible.ID)
}

func TestCustomDefaultUserRole_ReservedRoleNames(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	grantTestEnableExperiment(t, srv, st, true)

	customMember := grantTestSeedRole(t, st, "member", store.RoleScopeSystem, []string{"user.read"})
	customViewer := grantTestSeedRole(t, st, "viewer", store.RoleScopeSystem, []string{"user.read"})
	customAdmin := grantTestSeedRole(t, st, "admin", store.RoleScopeSystem, []string{"user.read"})
	eligible := grantTestSeedRole(t, st, "custom-safe", store.RoleScopeSystem, []string{"user.read"})

	// Reserved names must never appear in eligible_default_roles.
	listed := listEligibleDefaultRoles(ctx, st)
	var names []string
	for _, r := range listed {
		names = append(names, r.Name)
	}
	assert.Contains(t, names, eligible.Name)
	assert.NotContains(t, names, customMember.Name)
	assert.NotContains(t, names, customViewer.Name)
	assert.NotContains(t, names, customAdmin.Name)

	// When default_user_role is unset (built-in "member"), updating the unrelated
	// custom role named "member" to add skill.read must NOT be blocked by the guard.
	srv.config.DefaultUserRole = ""
	srv.config.UserAccessMode = "open"
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/roles/"+customMember.ID, updateRoleDefinitionRequest{
		Name:        "member",
		Description: "unrelated custom role named member",
		Permissions: []string{"user.read", "skill.read"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Provisioning with default_user_role == "" or "member" grants built-in hub-members,
	// never a binding to the custom role named "member".
	uMember, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "res-member@example.com", DisplayName: "Member"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, uMember.Role)
	grantTestAssertBuiltinHubMember(t, st, uMember.ID)
	for _, b := range grantTestSystemBindings(t, st, uMember.ID) {
		assert.NotEqual(t, customMember.ID, b.RoleDefinitionID)
	}

	// Provisioning with default_user_role == "viewer" grants built-in hub-viewer,
	// never a binding to the custom role named "viewer".
	srv.config.DefaultUserRole = store.UserRoleViewer
	uViewer, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "res-viewer@example.com", DisplayName: "Viewer"})
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleViewer, uViewer.Role)
	assert.False(t, grantTestIsHubMember(t, st, uViewer.ID))
	hubViewerRD, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	viewerBindings := grantTestSystemBindings(t, st, uViewer.ID)
	require.Len(t, viewerBindings, 1)
	assert.Equal(t, hubViewerRD.ID, viewerBindings[0].RoleDefinitionID)
	assert.NotEqual(t, customViewer.ID, viewerBindings[0].RoleDefinitionID)
}
