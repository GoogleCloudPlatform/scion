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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testPlatformAuthSA is a realistic-shaped configured transport service
// account email, used consistently across this file's test cases.
const testPlatformAuthSA = "transport-sa@example.iam.gserviceaccount.com"

// TestNew_WiresPlatformAuthSAToBothGuards builds a Server through New (not by
// poking srv.platformAuthSA / resolver.SetPlatformAuthSA / authConfig fields
// directly, as the other tests in this file do) and asserts that
// ServerConfig.PlatformAuthSA reaches every guard it is supposed to wire:
// Server.provisionUser, the GoogleIdentityResolver installed at
// srv.authConfig.GoogleResolver, and UnifiedAuthMiddleware's tokenTypeUser
// arm (via srv.authConfig.PlatformAuthSA, exercised through the full HTTP
// handler). Every other test here sets a field or calls a setter directly,
// so none of them would fail if server.go stopped copying cfg.PlatformAuthSA
// into srv.platformAuthSA, stopped calling googleResolver.SetPlatformAuthSA,
// or stopped copying srv.platformAuthSA into srv.authConfig.PlatformAuthSA —
// this test is the one that would. (Kept its original name, covering only
// two guards, for continuity with existing review references, even though it
// now covers three.)
func TestNew_WiresPlatformAuthSAToBothGuards(t *testing.T) {
	ctx := context.Background()

	s, err := newTestStore(":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.PlatformAuthSA = testPlatformAuthSA
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() {
		_ = srv.Shutdown(ctx)
		_ = s.Close()
	})

	// (a) Server.provisionUser must deny the configured identity.
	if _, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: testPlatformAuthSA}); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("provisionUser: expected ErrAccessDenied, got %v (cfg.PlatformAuthSA not reaching srv.platformAuthSA?)", err)
	}

	// (b) The shared GoogleIdentityResolver, reached exactly as production
	// code reaches it (srv.authConfig.GoogleResolver), must also deny it.
	resolver := srv.authConfig.GoogleResolver
	if resolver == nil {
		t.Fatal("srv.authConfig.GoogleResolver is nil")
	}
	identity := &ValidatedGoogleIdentity{
		Subject:          "111122223333",
		Email:            testPlatformAuthSA,
		EmailVerified:    true,
		Issuer:           googleIssuerHTTPS,
		IsServiceAccount: true,
		UpstreamExpiry:   time.Now().Add(time.Hour),
	}
	if _, err := resolver.Resolve(ctx, identity, ResolvePolicy{PreAuthorized: true}); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("GoogleResolver.Resolve: expected ErrAccessDenied, got %v (cfg.PlatformAuthSA not reaching googleResolver.SetPlatformAuthSA?)", err)
	}

	// (c) UnifiedAuthMiddleware's tokenTypeUser arm, reached through the full
	// HTTP handler, must also deny a Hub-issued user JWT for the configured
	// identity — proving cfg.PlatformAuthSA reached srv.authConfig.PlatformAuthSA.
	pre := &store.User{
		ID:      generateID(),
		Email:   testPlatformAuthSA,
		Role:    "member",
		Status:  "active",
		Created: time.Now(),
	}
	if err := s.CreateUser(ctx, pre); err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		pre.ID, pre.Email, pre.DisplayName, pre.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("tokenTypeUser arm: expected 401, got %d: %s (cfg.PlatformAuthSA not reaching srv.authConfig.PlatformAuthSA?)", rec.Code, rec.Body.String())
	}
}

// fakeProxyAuthenticator is a minimal ProxyAuthenticator test double that
// returns a fixed verified identity (or error) regardless of the request.
type fakeProxyAuthenticator struct {
	info *ProxyUserInfo
	err  error
}

func (f *fakeProxyAuthenticator) Authenticate(_ *http.Request) (*ProxyUserInfo, error) {
	return f.info, f.err
}

func (f *fakeProxyAuthenticator) Name() string { return "fake" }

// TestProvisionUser_PlatformAuthSA_Denied covers the configured transport
// service account arriving as a verified identity with no other credential:
// provisionUser must deny it and must not create a user row for it.
func TestProvisionUser_PlatformAuthSA_Denied(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA

	_, err := srv.provisionUser(ctx, &ExternalUserInfo{
		Email:       testPlatformAuthSA,
		DisplayName: "Transport SA",
	})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied, got %v", err)
	}

	if _, lookupErr := s.GetUserByEmail(ctx, testPlatformAuthSA); !errors.Is(lookupErr, store.ErrNotFound) {
		t.Fatalf("expected no user row for the configured service account, lookup returned err=%v", lookupErr)
	}
}

// TestProvisionUser_PlatformAuthSA_ExistingRow_Denied covers a pre-existing
// user row with the configured service account's email: provisionUser's
// existing-user branch must also deny it, not just the create branch.
func TestProvisionUser_PlatformAuthSA_ExistingRow_Denied(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA

	pre := &store.User{
		ID:      generateID(),
		Email:   testPlatformAuthSA,
		Role:    "member",
		Status:  "active",
		Created: time.Now(),
	}
	if err := s.CreateUser(ctx, pre); err != nil {
		t.Fatalf("failed to seed pre-existing user row: %v", err)
	}

	_, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: testPlatformAuthSA})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied for an existing row, got %v", err)
	}
}

// TestProvisionUser_PlatformAuthSA_Unconfigured_Inert covers a hub that has
// not configured a transport service account (the default / non-transport
// deployment): the check must be inert and normal provisioning unaffected.
func TestProvisionUser_PlatformAuthSA_Unconfigured_Inert(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	// srv.platformAuthSA intentionally left at its zero value.

	info := &ExternalUserInfo{Email: "someone@example.com", DisplayName: "Someone"}
	user, err := srv.provisionUser(ctx, info)
	if err != nil {
		t.Fatalf("unexpected error with no platform auth SA configured: %v", err)
	}
	if user.Email != info.Email {
		t.Errorf("expected provisioned email %q, got %q", info.Email, user.Email)
	}
	if _, lookupErr := s.GetUserByEmail(ctx, info.Email); lookupErr != nil {
		t.Fatalf("expected the user row to exist: %v", lookupErr)
	}
}

// TestProvisionUser_NormalUser_UnaffectedByPlatformAuthSA is a regression
// check that configuring a platform auth SA does not change provisioning for
// any other identity.
func TestProvisionUser_NormalUser_UnaffectedByPlatformAuthSA(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA

	user, err := srv.provisionUser(ctx, &ExternalUserInfo{
		Email:       "person@example.com",
		DisplayName: "Person",
	})
	if err != nil {
		t.Fatalf("unexpected error for an unrelated identity: %v", err)
	}
	if user.Email != "person@example.com" {
		t.Errorf("expected email person@example.com, got %q", user.Email)
	}
}

// TestUnifiedAuthMiddleware_PlatformAuthSA_ProxyIdentity_Denied exercises the
// guard at the HTTP layer: a verified proxy identity matching the configured
// transport service account, with no other credential on the request, must
// be rejected (403) and must not create a user row.
func TestUnifiedAuthMiddleware_PlatformAuthSA_ProxyIdentity_Denied(t *testing.T) {
	srv, s := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA

	cfg := AuthConfig{
		Mode:                 "production",
		ProxyAuthenticator:   &fakeProxyAuthenticator{info: &ProxyUserInfo{Email: testPlatformAuthSA}},
		ProxyUserProvisioner: MakeProxyUserProvisioner(srv),
	}
	middleware := UnifiedAuthMiddleware(cfg)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached for the configured service account identity")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	if _, err := s.GetUserByEmail(context.Background(), testPlatformAuthSA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no user row created, lookup returned err=%v", err)
	}
}

// TestUnifiedAuthMiddleware_AgentToken_SkipsUserProvisioning covers the
// legitimate agent-token path: a request carrying a valid agent token
// authenticates as the agent via Step 1 of UnifiedAuthMiddleware, and Step
// 1 returning early means the proxy identity / user-provisioning path is
// never reached for that request.
func TestUnifiedAuthMiddleware_AgentToken_SkipsUserProvisioning(t *testing.T) {
	srv, _ := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA

	agentTokenSvc, err := NewAgentTokenService(AgentTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create agent token service: %v", err)
	}
	agentToken, err := agentTokenSvc.GenerateAgentToken("agent-1", "project-1", []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	if err != nil {
		t.Fatalf("failed to generate agent token: %v", err)
	}

	provisionerCalled := false
	cfg := AuthConfig{
		Mode:               "production",
		AgentTokenSvc:      agentTokenSvc,
		ProxyAuthenticator: &fakeProxyAuthenticator{info: &ProxyUserInfo{Email: testPlatformAuthSA}},
		ProxyUserProvisioner: func(ctx context.Context, info *ProxyUserInfo) (UserIdentity, error) {
			provisionerCalled = true
			return MakeProxyUserProvisioner(srv)(ctx, info)
		},
	}
	middleware := UnifiedAuthMiddleware(cfg)

	var gotIdentity Identity
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = GetIdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("X-Scion-Agent-Token", agentToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the agent-tokened request to authenticate, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotIdentity == nil || gotIdentity.ID() != "agent-1" {
		t.Fatalf("expected agent identity agent-1, got %+v", gotIdentity)
	}
	if provisionerCalled {
		t.Error("expected the user-provisioning path not to be reached for an agent-tokened request")
	}
}

// TestAuthRefresh_PlatformAuthSA_Denied covers the stale-credential case: a
// refresh token minted for the configured service account, including an
// already-issued one, must not be renewable. See isReservedPlatformIdentity's
// invariant comment.
func TestAuthRefresh_PlatformAuthSA_Denied(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA

	pre := &store.User{
		ID:      generateID(),
		Email:   testPlatformAuthSA,
		Role:    "member",
		Status:  "active",
		Created: time.Now(),
	}
	if err := s.CreateUser(ctx, pre); err != nil {
		t.Fatalf("failed to seed pre-existing user row: %v", err)
	}

	_, refreshToken, _, err := srv.userTokenService.GenerateTokenPair(
		pre.ID, pre.Email, pre.DisplayName, pre.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/refresh",
		AuthRefreshRequest{RefreshToken: refreshToken})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestProxyAuthMiddleware_ExistingSession_PlatformAuthSA_ClearsSession covers
// the other stale-credential case: an existing session for a user row whose
// email is the configured service account must be cleared rather than
// re-verified and passed through or re-minted.
func TestProxyAuthMiddleware_ExistingSession_PlatformAuthSA_ClearsSession(t *testing.T) {
	pre := &store.User{
		ID:     "sa-user-1",
		Email:  testPlatformAuthSA,
		Role:   "member",
		Status: "active",
	}
	st := newProxyAuthStore()
	_ = st.CreateUser(context.Background(), pre)

	ws := newTestWebServer(t, WebServerConfig{
		AuthMode:           "proxy",
		ProxyAuthenticator: &fakeProxyAuthenticator{}, // no fresh assertion; session path only
		PlatformAuthSA:     testPlatformAuthSA,
	})
	ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
	ws.SetStore(st)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	ws.SetUserTokenService(tokenSvc)
	handler := ws.Handler()

	cookies := loginSession(t, ws, pre.ID, pre.Email, pre.Role)

	req := httptest.NewRequest("GET", "/projects", nil)
	req.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected the stale session to be cleared (302 to /login), got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/login" {
		t.Errorf("expected redirect to /login, got %q", got)
	}
}

// TestUnifiedAuthMiddleware_UserJWT_PlatformAuthSA_Denied covers the PRIMARY
// choke: a Hub-issued user JWT for the configured service account, presented
// as a Bearer token, must be rejected — including an already-issued,
// unexpired access token, regardless of which mint site it came from. See
// isReservedPlatformIdentity's invariant comment.
func TestUnifiedAuthMiddleware_UserJWT_PlatformAuthSA_Denied(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA
	srv.authConfig.PlatformAuthSA = testPlatformAuthSA

	pre := &store.User{
		ID:      generateID(),
		Email:   testPlatformAuthSA,
		Role:    "member",
		Status:  "active",
		Created: time.Now(),
	}
	if err := s.CreateUser(ctx, pre); err != nil {
		t.Fatalf("failed to seed pre-existing user row: %v", err)
	}

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		pre.ID, pre.Email, pre.DisplayName, pre.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUnifiedAuthMiddleware_UAT_PlatformAuthSA_Denied covers the tokenTypeUAT
// arm: a scion_pat_ personal access token minted under a user row whose
// email is the configured service account must be rejected.
func TestUnifiedAuthMiddleware_UAT_PlatformAuthSA_Denied(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA
	srv.authConfig.PlatformAuthSA = testPlatformAuthSA

	userID := generateID()
	if err := s.CreateUser(ctx, &store.User{
		ID:      userID,
		Email:   testPlatformAuthSA,
		Role:    "member",
		Status:  "active",
		Created: time.Now(),
	}); err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}
	ensureHubMembership(ctx, s, userID)

	projectID := generateID()
	if err := s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      "platform-auth-sa-uat-test",
		Slug:      "platform-auth-sa-uat-" + projectID[:8],
		CreatedBy: userID,
	}); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	if err != nil {
		t.Fatalf("failed to look up owner role definition: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	}); err != nil {
		t.Fatalf("failed to create owner role binding: %v", err)
	}

	mintCtx := contextWithCredentialContext(
		contextWithIdentity(context.Background(),
			NewAuthenticatedUser(userID, testPlatformAuthSA, "Transport SA", "member", string(ClientTypeAPI))),
		CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"},
	)
	key, _, err := srv.uatService.CreateToken(mintCtx, userID, "platform-auth-sa-pat", projectID, []string{"agent:read"}, nil)
	if err != nil {
		t.Fatalf("failed to mint PAT: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleAuthValidate_PlatformAuthSA_ReportsInvalid covers
// handleAuthValidate: a structurally valid, unexpired user JWT for the
// configured service account must be reported invalid, not valid.
func TestHandleAuthValidate_PlatformAuthSA_ReportsInvalid(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.platformAuthSA = testPlatformAuthSA

	pre := &store.User{
		ID:      generateID(),
		Email:   testPlatformAuthSA,
		Role:    "member",
		Status:  "active",
		Created: time.Now(),
	}
	if err := s.CreateUser(ctx, pre); err != nil {
		t.Fatalf("failed to seed pre-existing user row: %v", err)
	}

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		pre.ID, pre.Email, pre.DisplayName, pre.Role, ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: token})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp AuthValidateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Valid {
		t.Error("expected Valid=false for the configured service account")
	}
}

// TestSessionToBearerMiddleware_PlatformAuthSA_OverflowMint_ClearsSession
// covers sessionToBearerMiddleware's cookie-overflow-retry branch: a session
// carrying the configured service account's email, with no Hub access token
// stored (so the middleware would otherwise mint one per request), must be
// cleared instead, and the request must fall through with no Authorization
// header.
func TestSessionToBearerMiddleware_PlatformAuthSA_OverflowMint_ClearsSession(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{
		PlatformAuthSA: testPlatformAuthSA,
	})
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	ws.SetUserTokenService(tokenSvc)

	var sawRequest bool
	var capturedAuthHeader string
	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		capturedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	ws.MountHubAPI(mockHandler, func(ctx context.Context) error { return nil })
	handler := ws.Handler()

	reqSetup := httptest.NewRequest(http.MethodGet, "/", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	if err != nil {
		t.Fatalf("session store Get: %v", err)
	}
	sess.Values[sessKeyUserID] = "sa-user-1"
	sess.Values[sessKeyUserEmail] = testPlatformAuthSA
	sess.Values[sessKeyUserName] = "Transport SA"
	sess.Values[sessKeyUserRole] = "member"
	// Deliberately no sessKeyHubAccessToken/RefreshToken/Expiry: the
	// cookie-overflow-retry shape that would otherwise trigger a per-request mint.
	if err := sess.Save(reqSetup, recSetup); err != nil {
		t.Fatalf("session Save: %v", err)
	}
	cookies := recSetup.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("setup must produce a session cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !sawRequest {
		t.Fatal("expected the request to fall through to the next handler")
	}
	if capturedAuthHeader != "" {
		t.Errorf("expected no Authorization header for the configured service account, got %q", capturedAuthHeader)
	}
	assertSessionCookieCleared(t, rec)
}

// TestSessionToBearerMiddleware_PlatformAuthSA_Refresh_ClearsSession covers
// sessionToBearerMiddleware's refresh branch: a session carrying the
// configured service account's email, with an expired access token and a
// usable refresh token (so the middleware would otherwise call
// RefreshTokens), must be cleared instead, with no refresh performed and no
// Authorization header on the request.
func TestSessionToBearerMiddleware_PlatformAuthSA_Refresh_ClearsSession(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{
		PlatformAuthSA: testPlatformAuthSA,
	})
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("NewUserTokenService: %v", err)
	}
	ws.SetUserTokenService(tokenSvc)

	var sawRequest bool
	var capturedAuthHeader string
	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		capturedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	ws.MountHubAPI(mockHandler, func(ctx context.Context) error { return nil })
	handler := ws.Handler()

	expiredAccess, _, err := tokenSvc.GenerateAccessTokenWithTTL(
		"sa-user-1", testPlatformAuthSA, "Transport SA", "member", ClientTypeWeb, -time.Hour,
	)
	if err != nil {
		t.Fatalf("failed to generate expired access token: %v", err)
	}
	_, refreshToken, _, err := tokenSvc.GenerateTokenPair(
		"sa-user-1", testPlatformAuthSA, "Transport SA", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate refresh token: %v", err)
	}

	reqSetup := httptest.NewRequest(http.MethodGet, "/", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	if err != nil {
		t.Fatalf("session store Get: %v", err)
	}
	sess.Values[sessKeyUserID] = "sa-user-1"
	sess.Values[sessKeyUserEmail] = testPlatformAuthSA
	sess.Values[sessKeyUserName] = "Transport SA"
	sess.Values[sessKeyUserRole] = "member"
	sess.Values[sessKeyHubAccessToken] = expiredAccess
	sess.Values[sessKeyHubRefreshToken] = refreshToken
	if err := sess.Save(reqSetup, recSetup); err != nil {
		t.Fatalf("session Save: %v", err)
	}
	cookies := recSetup.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("setup must produce a session cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !sawRequest {
		t.Fatal("expected the request to fall through to the next handler")
	}
	if capturedAuthHeader != "" {
		t.Errorf("expected no Authorization header (and no refresh performed) for the configured service account, got %q", capturedAuthHeader)
	}
	assertSessionCookieCleared(t, rec)
}

// assertSessionCookieCleared asserts the response cleared the web session
// cookie (Set-Cookie with a negative Max-Age).
func assertSessionCookieCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == webSessionName {
			if c.MaxAge >= 0 {
				t.Errorf("expected the session cookie to be cleared (negative Max-Age), got %d", c.MaxAge)
			}
			return
		}
	}
	t.Error("expected a Set-Cookie clearing the session")
}
