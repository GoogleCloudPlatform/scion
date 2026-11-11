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

// Session generation in hub user JWTs: every hub JWT carries the user's
// session generation (claim sg) and the hub refuses a token whose claim
// differs from the users row in either direction. A token without the claim
// reads as 0. Each test names, in its comment, the check whose removal makes
// it fail.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authctx"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// sessionGenFixture is a SQLite-backed hub with one active member user.
type sessionGenFixture struct {
	srv  *Server
	s    store.Store
	user *store.User
}

func newSessionGenFixture(t *testing.T) *sessionGenFixture {
	t.Helper()
	srv, s := testServer(t)
	id := tid("sessgen-user")
	u := &store.User{
		ID:          id,
		Email:       id + "@example.com",
		DisplayName: "Session Gen User",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return &sessionGenFixture{srv: srv, s: s, user: u}
}

// mintPair mints an access and refresh token pair for the fixture user with
// the given session generation and client type.
func (f *sessionGenFixture) mintPair(t *testing.T, gen int64, ct ClientType) (access, refresh string) {
	t.Helper()
	u := f.user
	access, refresh, _, err := f.srv.userTokenService.MintTokenPair(u.ID, u.Email, u.DisplayName, u.Role, gen, ct)
	require.NoError(t, err)
	return access, refresh
}

// sessionGenClaimlessClaims mirrors UserTokenClaims without the sg claim:
// the shape of a token minted before the claim existed.
type sessionGenClaimlessClaims struct {
	jwt.Claims
	UserID      string        `json:"uid"`
	Email       string        `json:"email"`
	DisplayName string        `json:"name,omitempty"`
	Role        string        `json:"role"`
	TokenType   UserTokenType `json:"type"`
	ClientType  ClientType    `json:"client"`
}

// mintClaimless signs a token for the fixture user that has no sg claim.
func (f *sessionGenFixture) mintClaimless(t *testing.T, tt UserTokenType, ct ClientType) string {
	t.Helper()
	now := time.Now()
	u := f.user
	tok, err := jwt.Signed(f.srv.userTokenService.signer).Claims(sessionGenClaimlessClaims{
		Claims: jwt.Claims{
			Issuer:    UserTokenIssuer,
			Subject:   u.ID,
			Audience:  jwt.Audience{UserTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(now.Add(time.Hour)),
			NotBefore: jwt.NewNumericDate(now),
			ID:        generateUserTokenID(),
		},
		UserID: u.ID, Email: u.Email, DisplayName: u.DisplayName, Role: u.Role,
		TokenType: tt, ClientType: ct,
	}).Serialize()
	require.NoError(t, err)
	claims, err := f.srv.userTokenService.ValidateUserToken(tok)
	require.NoError(t, err)
	require.Zero(t, claims.SessionGeneration, "a token without the sg claim decodes to 0")
	return tok
}

// revoke calls the admin revoke-sessions endpoint for the fixture user and
// returns the user's new session generation.
func (f *sessionGenFixture) revoke(t *testing.T) int64 {
	t.Helper()
	before := f.generation(t)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/users/"+f.user.ID+"/revoke-sessions", map[string]interface{}{})
	require.Equal(t, http.StatusOK, rec.Code, "revoke-sessions: %s", rec.Body.String())
	after := f.generation(t)
	require.Equal(t, before+1, after, "revoke-sessions increments the session generation")
	return after
}

func (f *sessionGenFixture) generation(t *testing.T) int64 {
	t.Helper()
	u, err := f.s.GetUser(context.Background(), f.user.ID)
	require.NoError(t, err)
	return u.SessionGeneration
}

// me calls GET /api/v1/auth/me with token as the bearer credential.
func (f *sessionGenFixture) me(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithToken(t, f.srv, token, http.MethodGet, "/api/v1/auth/me", nil)
}

// requireRefusedAsRevoked asserts the bearer arm's session generation refusal.
func requireRefusedAsRevoked(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, rec.Code, "refused as revoked: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "session revoked")
}

// assertDeniedAfterRevoke checks that token is accepted on an ordinary API
// route before a revoke and refused after it.
func (f *sessionGenFixture) assertDeniedAfterRevoke(t *testing.T, token string) {
	t.Helper()
	rec := f.me(t, token)
	require.Equal(t, http.StatusOK, rec.Code, "accepted before the revoke: %s", rec.Body.String())
	f.revoke(t)
	requireRefusedAsRevoked(t, f.me(t, token))
}

// Fails if the session generation check is removed from the JWT bearer arm
// (UnifiedAuthMiddleware): a CLI access token minted before the revoke would
// still be accepted.
func TestSessionGen_BearerArm_CLIAccessToken_DeniedAfterRevoke(t *testing.T) {
	f := newSessionGenFixture(t)
	access, _ := f.mintPair(t, 0, ClientTypeCLI)
	claims, err := f.srv.userTokenService.ValidateUserToken(access)
	require.NoError(t, err)
	require.Equal(t, TokenTypeCLI, claims.TokenType)
	f.assertDeniedAfterRevoke(t, access)
}

// Fails if the session generation check is removed from the JWT bearer arm:
// a web access token minted before the revoke would still be accepted.
func TestSessionGen_BearerArm_WebAccessToken_DeniedAfterRevoke(t *testing.T) {
	f := newSessionGenFixture(t)
	access, _ := f.mintPair(t, 0, ClientTypeWeb)
	claims, err := f.srv.userTokenService.ValidateUserToken(access)
	require.NoError(t, err)
	require.Equal(t, TokenTypeAccess, claims.TokenType)
	f.assertDeniedAfterRevoke(t, access)
}

// Fails if the session generation check is removed from the JWT bearer arm:
// a refresh token presented as a bearer credential, minted before the
// revoke, would still be accepted.
func TestSessionGen_BearerArm_RefreshTokenAsBearer_DeniedAfterRevoke(t *testing.T) {
	f := newSessionGenFixture(t)
	_, refresh := f.mintPair(t, 0, ClientTypeCLI)
	claims, err := f.srv.userTokenService.ValidateUserToken(refresh)
	require.NoError(t, err)
	require.Equal(t, TokenTypeRefresh, claims.TokenType)
	f.assertDeniedAfterRevoke(t, refresh)
}

// Fails if the session generation check is removed from the JWT bearer arm:
// an API access token of the GE-exchange shape (MintAccessTokenWithTTL,
// client type api), minted before the revoke, would still be accepted.
func TestSessionGen_BearerArm_GEExchangeToken_DeniedAfterRevoke(t *testing.T) {
	f := newSessionGenFixture(t)
	u := f.user
	tok, _, err := f.srv.userTokenService.MintAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, u.Role, 0, ClientTypeAPI, 10*time.Minute)
	require.NoError(t, err)
	f.assertDeniedAfterRevoke(t, tok)
}

// sessionGenOAuthTokens obtains a token pair for the fixture user through
// the real POST /api/v1/auth/token issuance path (mocked Google exchange).
func (f *sessionGenFixture) sessionGenOAuthTokens(t *testing.T, clientType string) AuthTokenResponse {
	t.Helper()
	f.srv.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{Google: OAuthProviderConfig{ClientID: "test-web-id", ClientSecret: "test-web-secret"}},
		CLI: OAuthClientConfig{Google: OAuthProviderConfig{ClientID: "test-cli-id", ClientSecret: "test-cli-secret"}},
	}, nil)
	f.srv.oauthService.httpClient = &http.Client{Transport: &mockOAuthTransport{
		tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
		userinfoJSON: `{"id":"sub-` + f.user.ID + `","email":"` + f.user.Email + `","verified_email":true,"name":"Session Gen User"}`,
	}}
	rec := doRequestNoAuth(t, f.srv, http.MethodPost, "/api/v1/auth/token", AuthTokenRequest{
		Provider:    "google",
		Code:        "test-code",
		RedirectURI: "http://localhost:8080/callback",
		GrantType:   "authorization_code",
		ClientType:  clientType,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp AuthTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.AccessToken)
	require.NotEmpty(t, resp.RefreshToken)
	return resp
}

// Fails if an issuance path mints with a session generation other than the
// users row's (for example 0): a token issued after the revoke would then be
// refused. Each token type is obtained through the real OAuth token
// endpoint, after the revoke.
func TestSessionGen_BearerArm_TokenIssuedAfterRevoke_Accepted(t *testing.T) {
	cases := []struct {
		name       string
		clientType string
		pick       func(AuthTokenResponse) string
		wantType   UserTokenType
	}{
		{"cli_access", "cli", func(r AuthTokenResponse) string { return r.AccessToken }, TokenTypeCLI},
		{"web_access", "web", func(r AuthTokenResponse) string { return r.AccessToken }, TokenTypeAccess},
		{"refresh", "cli", func(r AuthTokenResponse) string { return r.RefreshToken }, TokenTypeRefresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionGenFixture(t)
			gen := f.revoke(t)
			tok := tc.pick(f.sessionGenOAuthTokens(t, tc.clientType))
			claims, err := f.srv.userTokenService.ValidateUserToken(tok)
			require.NoError(t, err)
			require.Equal(t, tc.wantType, claims.TokenType)
			assert.Equal(t, gen, claims.SessionGeneration, "issued with the users row's generation")
			rec := f.me(t, tok)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// Fails if the bearer arm refuses a never-revoked user's tokens (generation
// 0 on both sides), for each token type.
func TestSessionGen_BearerArm_NeverRevoked_Accepted(t *testing.T) {
	f := newSessionGenFixture(t)
	require.Zero(t, f.generation(t))
	cliAccess, cliRefresh := f.mintPair(t, 0, ClientTypeCLI)
	webAccess, _ := f.mintPair(t, 0, ClientTypeWeb)
	for name, tok := range map[string]string{"cli_access": cliAccess, "web_access": webAccess, "refresh": cliRefresh} {
		t.Run(name, func(t *testing.T) {
			rec := f.me(t, tok)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// Fails if the session generation check is removed from the JWT bearer arm,
// or if a missing sg claim stops reading as 0: a token without the claim
// must be refused for a user whose sessions were ever revoked.
func TestSessionGen_BearerArm_ClaimlessToken_EverRevokedUser_Denied(t *testing.T) {
	f := newSessionGenFixture(t)
	f.revoke(t)
	for name, tok := range map[string]string{
		"cli_access": f.mintClaimless(t, TokenTypeCLI, ClientTypeCLI),
		"web_access": f.mintClaimless(t, TokenTypeAccess, ClientTypeWeb),
		"refresh":    f.mintClaimless(t, TokenTypeRefresh, ClientTypeCLI),
	} {
		t.Run(name, func(t *testing.T) {
			requireRefusedAsRevoked(t, f.me(t, tok))
		})
	}
}

// Fails if a token without the sg claim is refused for a user whose sessions
// were never revoked (a missing claim must read as 0, equal to the row).
func TestSessionGen_BearerArm_ClaimlessToken_NeverRevokedUser_Accepted(t *testing.T) {
	f := newSessionGenFixture(t)
	require.Zero(t, f.generation(t))
	for name, tok := range map[string]string{
		"cli_access": f.mintClaimless(t, TokenTypeCLI, ClientTypeCLI),
		"web_access": f.mintClaimless(t, TokenTypeAccess, ClientTypeWeb),
		"refresh":    f.mintClaimless(t, TokenTypeRefresh, ClientTypeCLI),
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.me(t, tok)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// Fails if the session generation check is removed from the JWT bearer arm,
// or if the comparison refuses only when the row is ahead of the claim
// (row > claim) instead of on any inequality: a token whose claim is ahead
// of the row must be refused.
func TestSessionGen_BearerArm_ClaimAheadOfRow_Denied(t *testing.T) {
	f := newSessionGenFixture(t)
	t.Run("row_0_claim_1", func(t *testing.T) {
		access, _ := f.mintPair(t, 1, ClientTypeCLI)
		requireRefusedAsRevoked(t, f.me(t, access))
	})
	t.Run("row_2_claim_3", func(t *testing.T) {
		f.revoke(t)
		row := f.revoke(t)
		require.Equal(t, int64(2), row)
		access, _ := f.mintPair(t, row+1, ClientTypeWeb)
		requireRefusedAsRevoked(t, f.me(t, access))
		current, _ := f.mintPair(t, row, ClientTypeWeb)
		rec := f.me(t, current)
		assert.Equal(t, http.StatusOK, rec.Code, "a token at the row's generation is accepted: %s", rec.Body.String())
	})
}

// authctxProbe runs the hub's own auth configuration in front of a handler
// that records the session generation the request context carries.
type authctxProbe struct {
	reached bool
	gen     int64
	ok      bool
}

func (p *authctxProbe) serve(t *testing.T, cfg AuthConfig, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	h := UnifiedAuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.reached = true
		p.gen, p.ok = authctx.SessionGeneration(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionGenProbeRequest(bearer string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

// Fails if the JWT bearer arm stops recording the users row's session
// generation in the request context (authctx.WithSessionGeneration is
// removed or given another value).
func TestSessionGen_BearerArm_SetsAuthctxRowValue(t *testing.T) {
	f := newSessionGenFixture(t)
	t.Run("generation_0", func(t *testing.T) {
		access, _ := f.mintPair(t, 0, ClientTypeCLI)
		var p authctxProbe
		rec := p.serve(t, f.srv.authConfig, sessionGenProbeRequest(access))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, p.reached)
		assert.True(t, p.ok, "a hub-JWT request carries a session generation")
		assert.Equal(t, int64(0), p.gen)
	})
	t.Run("generation_2", func(t *testing.T) {
		f.revoke(t)
		row := f.revoke(t)
		access, _ := f.mintPair(t, row, ClientTypeWeb)
		var p authctxProbe
		rec := p.serve(t, f.srv.authConfig, sessionGenProbeRequest(access))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, p.reached)
		assert.True(t, p.ok)
		assert.Equal(t, row, p.gen, "the value is the users row's generation")
	})
}

// Fails if a session generation is recorded for a request that was not
// authenticated by the hub-JWT arm: user access tokens (UAT), the dev token
// and proxy-authenticated requests must leave it unset.
func TestSessionGen_BearerArm_UAT_DevToken_Proxy_NoAuthctx(t *testing.T) {
	f := newSessionGenFixture(t)

	t.Run("uat", func(t *testing.T) {
		key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.user.ID), CreateTokenParams{
			UserID: f.user.ID, Name: "sessgen", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"inbox:read"},
		})
		require.NoError(t, err)
		var p authctxProbe
		rec := p.serve(t, f.srv.authConfig, sessionGenProbeRequest(key))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, p.reached)
		assert.False(t, p.ok, "a UAT request carries no session generation")
	})

	t.Run("dev_token", func(t *testing.T) {
		var p authctxProbe
		rec := p.serve(t, f.srv.authConfig, sessionGenProbeRequest(testDevToken))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, p.reached)
		assert.False(t, p.ok, "a dev-token request carries no session generation")
	})

	t.Run("proxy", func(t *testing.T) {
		cfg := f.srv.authConfig
		cfg.ProxyAuthenticator = &mockProxyAuthenticator{user: &ProxyUserInfo{Email: f.user.Email, Subject: "proxy-sub"}}
		cfg.ProxyUserProvisioner = func(_ context.Context, info *ProxyUserInfo) (UserIdentity, error) {
			return NewAuthenticatedUser(f.user.ID, info.Email, f.user.DisplayName, f.user.Role, "web"), nil
		}
		var p authctxProbe
		rec := p.serve(t, cfg, sessionGenProbeRequest(""))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, p.reached)
		assert.False(t, p.ok, "a proxy-authenticated request carries no session generation")
	})
}

// Fails if hub.New stops wiring its store argument into the auth
// middleware's UserStore (server.go, AuthConfig{UserStore: s}). Without it
// the JWT bearer arm skips the users-row block, including the session
// generation check; see the comment at that skip in auth.go.
func TestNew_WiresAuthUserStoreFromStore(t *testing.T) {
	srv, s := testServer(t)
	require.NotNil(t, srv.authConfig.UserStore, "the auth middleware has a user store")
	assert.True(t, any(srv.authConfig.UserStore) == any(s), "the auth middleware's user store is the store passed to New")
}

// postRefresh calls POST /api/v1/auth/refresh with refreshToken.
func (f *sessionGenFixture) postRefresh(t *testing.T, refreshToken string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestNoAuth(t, f.srv, http.MethodPost, "/api/v1/auth/refresh", AuthRefreshRequest{RefreshToken: refreshToken})
}

func requireRefreshRefused(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, rec.Code, "refresh refused: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid refresh token")
	assert.NotContains(t, rec.Body.String(), "accessToken", "no token pair is returned")
}

// Fails if the session generation check is removed from POST
// /api/v1/auth/refresh: a CLI or web refresh token minted before the revoke
// would be exchanged for a new pair. The refusal is asserted at the refresh
// itself, not only at the new pair's first use.
func TestSessionGen_AuthRefresh_BehindRow_Denied(t *testing.T) {
	for _, ct := range []ClientType{ClientTypeCLI, ClientTypeWeb} {
		t.Run(string(ct), func(t *testing.T) {
			f := newSessionGenFixture(t)
			_, refresh := f.mintPair(t, 0, ct)
			f.revoke(t)
			requireRefreshRefused(t, f.postRefresh(t, refresh))
		})
	}
}

// Fails if the session generation check is removed from POST
// /api/v1/auth/refresh, or if it refuses only when the row is ahead: a
// refresh token whose claim is ahead of the row must be refused.
func TestSessionGen_AuthRefresh_ClaimAhead_Denied(t *testing.T) {
	f := newSessionGenFixture(t)
	_, refresh := f.mintPair(t, 1, ClientTypeCLI)
	requireRefreshRefused(t, f.postRefresh(t, refresh))
}

// Fails if the session generation check is removed from POST
// /api/v1/auth/refresh, or if a missing sg claim stops reading as 0: a
// refresh token without the claim is refused for an ever-revoked user.
func TestSessionGen_AuthRefresh_Claimless_EverRevoked_Denied(t *testing.T) {
	f := newSessionGenFixture(t)
	f.revoke(t)
	requireRefreshRefused(t, f.postRefresh(t, f.mintClaimless(t, TokenTypeRefresh, ClientTypeCLI)))
}

// Fails if POST /api/v1/auth/refresh refuses a refresh token at the row's
// generation, or if the new pair stops carrying the presented token's
// session generation. Also covers a never-revoked user with a token
// without the claim.
func TestSessionGen_AuthRefresh_NeverRevoked_OK_CarriesSameSG(t *testing.T) {
	check := func(t *testing.T, f *sessionGenFixture, refresh string, want int64) {
		t.Helper()
		rec := f.postRefresh(t, refresh)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AuthRefreshResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for name, tok := range map[string]string{"access": resp.AccessToken, "refresh": resp.RefreshToken} {
			claims, err := f.srv.userTokenService.ValidateUserToken(tok)
			require.NoError(t, err, name)
			assert.Equal(t, want, claims.SessionGeneration, "new %s token carries the presented generation", name)
		}
		assert.Equal(t, http.StatusOK, f.me(t, resp.AccessToken).Code, "the new access token is accepted")
	}
	t.Run("never_revoked", func(t *testing.T) {
		f := newSessionGenFixture(t)
		_, refresh := f.mintPair(t, 0, ClientTypeCLI)
		check(t, f, refresh, 0)
	})
	t.Run("never_revoked_claimless", func(t *testing.T) {
		f := newSessionGenFixture(t)
		check(t, f, f.mintClaimless(t, TokenTypeRefresh, ClientTypeWeb), 0)
	})
	t.Run("current_generation", func(t *testing.T) {
		f := newSessionGenFixture(t)
		f.revoke(t)
		row := f.revoke(t)
		_, refresh := f.mintPair(t, row, ClientTypeWeb)
		check(t, f, refresh, row)
	})
}

// Fails if POST /api/v1/auth/refresh mints a pair when the users row cannot
// be read (for example by restoring the former fall-back-and-mint path):
// with the generation unknown it must answer 503 and mint nothing.
func TestSessionGen_AuthRefresh_StoreFault_FailsClosed(t *testing.T) {
	f := newSessionGenFixture(t)
	installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *failingUserLookupStore {
		return &failingUserLookupStore{Store: inner, err: errors.New("database is locked")}
	})
	_, refresh := f.mintPair(t, 0, ClientTypeCLI)
	rec := f.postRefresh(t, refresh)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "a lookup fault fails closed: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "store_error")
	assert.NotContains(t, rec.Body.String(), "accessToken")
}

// postValidate calls POST /api/v1/auth/validate for token.
func (f *sessionGenFixture) postValidate(t *testing.T, token string) AuthValidateResponse {
	t.Helper()
	rec := doRequestNoAuth(t, f.srv, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: token})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp AuthValidateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

// Fails if the session generation check is removed from POST
// /api/v1/auth/validate: a token minted before the revoke, or one whose
// claim is ahead of the row, would be reported valid.
func TestSessionGen_AuthValidate_RevokedToken_Invalid(t *testing.T) {
	f := newSessionGenFixture(t)
	access, _ := f.mintPair(t, 0, ClientTypeCLI)
	require.True(t, f.postValidate(t, access).Valid, "valid before the revoke")
	row := f.revoke(t)

	resp := f.postValidate(t, access)
	assert.False(t, resp.Valid, "a token behind the row is not valid")
	assert.Nil(t, resp.User)
	ahead, _ := f.mintPair(t, row+1, ClientTypeWeb)
	assert.False(t, f.postValidate(t, ahead).Valid, "a token ahead of the row is not valid")
	current, _ := f.mintPair(t, row, ClientTypeWeb)
	assert.True(t, f.postValidate(t, current).Valid, "a token at the row's generation is valid")
}

// newWeb builds a web server over the fixture's store and token service,
// with the fixture hub mounted as its API.
func (f *sessionGenFixture) newWeb(t *testing.T, mutate func(*WebServerConfig)) *WebServer {
	t.Helper()
	cfg := WebServerConfig{BaseURL: "http://localhost:8080"}
	if mutate != nil {
		mutate(&cfg)
	}
	ws := newTestWebServer(t, cfg)
	ws.SetStore(f.s)
	ws.SetAccessSettingsProvider(f.srv)
	ws.SetUserTokenService(f.srv.userTokenService)
	ws.MountHubAPI(f.srv.Handler(), func(context.Context) error { return nil })
	return ws
}

// webSessionCookies returns cookies for a web session holding the fixture
// user's identity plus vals.
func (f *sessionGenFixture) webSessionCookies(t *testing.T, ws *WebServer, vals map[string]interface{}) []*http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)
	sess.Values[sessKeyUserID] = f.user.ID
	sess.Values[sessKeyUserEmail] = f.user.Email
	sess.Values[sessKeyUserName] = f.user.DisplayName
	sess.Values[sessKeyUserRole] = f.user.Role
	for k, v := range vals {
		sess.Values[k] = v
	}
	require.NoError(t, sess.Save(req, rec))
	cookies := rec.Result().Cookies()
	require.NotEmpty(t, cookies)
	return cookies
}

// webDo sends a request through the web server with cookies.
func webDo(ws *WebServer, method, path string, cookies []*http.Cookie, browser bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if browser {
		req.Header.Set("Accept", "text/html")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	return rec
}

// webSessionAfter decodes the web session the response's cookies carry.
// It reports false when the response sets no session cookie.
func webSessionAfter(t *testing.T, ws *WebServer, rec *httptest.ResponseRecorder) (map[interface{}]interface{}, bool) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == webSessionName {
			found = true
			req.AddCookie(c)
		}
	}
	if !found {
		return nil, false
	}
	sess, err := ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)
	return sess.Values, true
}

// expiredAccessToken mints an already-expired access token for the fixture
// user at generation gen.
func (f *sessionGenFixture) expiredAccessToken(t *testing.T, gen int64) string {
	t.Helper()
	u := f.user
	tok, _, err := f.srv.userTokenService.MintAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, u.Role, gen, ClientTypeWeb, -time.Hour)
	require.NoError(t, err)
	return tok
}

// Fails if the session generation check is removed from the web BFF refresh
// (refreshHubTokens), on the branch that refreshes an invalid (expired)
// access token: the session's pre-revoke refresh token would be rotated
// into a new pair and stored, instead of the session being cleared.
func TestSessionGen_BFFRefresh_ExpiredAccess_BehindRow_ClearsSession(t *testing.T) {
	f := newSessionGenFixture(t)
	ws := f.newWeb(t, nil)
	_, refresh := f.mintPair(t, 0, ClientTypeWeb)
	cookies := f.webSessionCookies(t, ws, map[string]interface{}{
		sessKeyHubAccessToken:    f.expiredAccessToken(t, 0),
		sessKeyHubRefreshToken:   refresh,
		sessKeySessionGeneration: int64(0),
	})
	f.revoke(t)

	rec := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assertSessionCookieCleared(t, rec)
	if vals, ok := webSessionAfter(t, ws, rec); ok {
		assert.Empty(t, vals[sessKeyHubAccessToken], "no new access token is stored")
		assert.Empty(t, vals[sessKeyHubRefreshToken], "no new refresh token is stored")
	}

	// A browser request on the same branch gets the session-expired answer.
	rec = webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, true)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "session_expired")
	assertSessionCookieCleared(t, rec)
}

// Fails if the session generation check is removed from the web BFF refresh
// (refreshHubTokens), on the proactive branch (access token still valid,
// stored expiry passed): the pre-revoke refresh token would be rotated into
// a new pair and stored, instead of the session being cleared.
func TestSessionGen_BFFRefresh_ProactiveExpiry_BehindRow_NoNewPair(t *testing.T) {
	f := newSessionGenFixture(t)
	ws := f.newWeb(t, nil)
	access, refresh := f.mintPair(t, 0, ClientTypeWeb)
	cookies := f.webSessionCookies(t, ws, map[string]interface{}{
		sessKeyHubAccessToken:    access,
		sessKeyHubRefreshToken:   refresh,
		sessKeyHubTokenExpiry:    time.Now().Add(-time.Minute).UnixMilli(),
		sessKeySessionGeneration: int64(0),
	})
	f.revoke(t)

	rec := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assertSessionCookieCleared(t, rec)
	if vals, ok := webSessionAfter(t, ws, rec); ok {
		assert.NotEqual(t, refresh, vals[sessKeyHubRefreshToken])
		assert.Empty(t, vals[sessKeyHubRefreshToken], "no new pair is stored")
	}
}

// Fails if the web BFF refresh refuses or stops rotating a never-revoked
// user's session, on either refresh branch.
func TestSessionGen_BFFRefresh_NeverRevoked_Rotates(t *testing.T) {
	cases := map[string]func(f *sessionGenFixture, t *testing.T) (access string, expiry int64){
		"expired_access": func(f *sessionGenFixture, t *testing.T) (string, int64) {
			return f.expiredAccessToken(t, 0), time.Now().Add(time.Hour).UnixMilli()
		},
		"proactive": func(f *sessionGenFixture, t *testing.T) (string, int64) {
			access, _ := f.mintPair(t, 0, ClientTypeWeb)
			return access, time.Now().Add(-time.Minute).UnixMilli()
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSessionGenFixture(t)
			ws := f.newWeb(t, nil)
			access, expiry := setup(f, t)
			_, refresh := f.mintPair(t, 0, ClientTypeWeb)
			cookies := f.webSessionCookies(t, ws, map[string]interface{}{
				sessKeyHubAccessToken:    access,
				sessKeyHubRefreshToken:   refresh,
				sessKeyHubTokenExpiry:    expiry,
				sessKeySessionGeneration: int64(0),
			})
			rec := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			vals, ok := webSessionAfter(t, ws, rec)
			require.True(t, ok, "the rotated pair is saved to the session")
			newRefresh, _ := vals[sessKeyHubRefreshToken].(string)
			require.NotEmpty(t, newRefresh)
			assert.NotEqual(t, refresh, newRefresh, "the refresh token was rotated")
			claims, err := f.srv.userTokenService.ValidateRefreshToken(newRefresh)
			require.NoError(t, err)
			assert.Equal(t, int64(0), claims.SessionGeneration)
		})
	}
}

// Fails if the web BFF's cookie-overflow mint (a session with no stored hub
// token) mints with the users row's generation instead of the session's
// own: a session established before the revoke would then reach the hub
// API with a token the bearer arm accepts.
func TestSessionGen_BFFOverflowMint_UsesCookieGeneration_DeniedAfterRevoke(t *testing.T) {
	f := newSessionGenFixture(t)
	ws := f.newWeb(t, nil)
	stale := f.webSessionCookies(t, ws, map[string]interface{}{sessKeySessionGeneration: int64(0)})
	rec := webDo(ws, http.MethodGet, "/api/v1/auth/me", stale, false)
	require.Equal(t, http.StatusOK, rec.Code, "accepted before the revoke: %s", rec.Body.String())

	row := f.revoke(t)
	rec = webDo(ws, http.MethodGet, "/api/v1/auth/me", stale, false)
	requireRefusedAsRevoked(t, rec)

	// Control: a session established at the current generation is served.
	current := f.webSessionCookies(t, ws, map[string]interface{}{sessKeySessionGeneration: row})
	rec = webDo(ws, http.MethodGet, "/api/v1/auth/me", current, false)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Fails if the proxy-mode role-change re-mint (proxyAuthMiddleware) mints
// with the users row's generation instead of the session's own.
func TestSessionGen_ProxyRoleRemint_UsesCookieGeneration(t *testing.T) {
	f := newSessionGenFixture(t)
	row := f.revoke(t)
	require.Equal(t, int64(1), row)
	ws := f.newWeb(t, func(c *WebServerConfig) {
		c.AuthMode = "proxy"
		c.ProxyAuthenticator = &mockProxyAuthenticator{}
	})
	// The session's role differs from the stored role, so the middleware
	// re-mints the session's hub tokens with the corrected role.
	cookies := f.webSessionCookies(t, ws, map[string]interface{}{
		sessKeyUserRole:          store.UserRoleAdmin,
		sessKeySessionGeneration: int64(0),
	})
	rec := webDo(ws, http.MethodGet, "/", cookies, true)
	vals, ok := webSessionAfter(t, ws, rec)
	require.True(t, ok, "the session is saved after the role change (status %d)", rec.Code)
	require.Equal(t, store.UserRoleMember, vals[sessKeyUserRole], "the role was corrected")
	access, _ := vals[sessKeyHubAccessToken].(string)
	require.NotEmpty(t, access, "hub tokens were re-minted")
	claims, err := f.srv.userTokenService.ValidateUserToken(access)
	require.NoError(t, err)
	assert.Equal(t, int64(0), claims.SessionGeneration, "the re-mint carries the session's generation, not the row's")
}

// newDevSessionGenWeb returns a dev-auth web server in front of the
// fixture's hub, with the dev user's row in the fixture store.
func (f *sessionGenFixture) newDevSessionGenWeb(t *testing.T) *WebServer {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.GetUser(ctx, DevUserID); errors.Is(err, store.ErrNotFound) {
		require.NoError(t, f.s.CreateUser(ctx, &store.User{
			ID:          DevUserID,
			Email:       "dev@localhost",
			DisplayName: "Development User",
			Role:        store.UserRoleAdmin,
			Status:      store.UserStatusActive,
			Created:     time.Now(),
		}))
	} else {
		require.NoError(t, err)
	}
	return f.newWeb(t, func(c *WebServerConfig) {
		c.Host = "127.0.0.1"
		c.DevAuthToken = "test-dev-token-12345"
	})
}

// devGeneration returns the dev user's current session generation.
func (f *sessionGenFixture) devGeneration(t *testing.T) int64 {
	t.Helper()
	u, err := f.s.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	return u.SessionGeneration
}

// devSessionCookies returns cookies for a dev web session recording
// generation gen and holding a hub token pair minted at gen.
func (f *sessionGenFixture) devSessionCookies(t *testing.T, ws *WebServer, gen int64) []*http.Cookie {
	t.Helper()
	access, refresh, _, err := f.srv.userTokenService.MintTokenPair(DevUserID, "dev@localhost", "Development User", store.UserRoleAdmin, gen, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)
	sess.Values[sessKeyUserID] = DevUserID
	sess.Values[sessKeyUserEmail] = "dev@localhost"
	sess.Values[sessKeyUserName] = "Development User"
	sess.Values[sessKeyUserRole] = store.UserRoleAdmin
	sess.Values[sessKeyHubAccessToken] = access
	sess.Values[sessKeyHubRefreshToken] = refresh
	sess.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Hour).UnixMilli()
	sess.Values[sessKeySessionGeneration] = gen
	require.NoError(t, sess.Save(req, rec))
	return rec.Result().Cookies()
}

// requireDevSessionEnded asserts that rec ended the dev session: every
// session cookie it sets is cleared (negative Max-Age) and none holds a
// user or a hub token.
func requireDevSessionEnded(t *testing.T, ws *WebServer, rec *httptest.ResponseRecorder) {
	t.Helper()
	assertSessionCookieCleared(t, rec)
	for _, c := range rec.Result().Cookies() {
		if c.Name != webSessionName {
			continue
		}
		assert.Negative(t, c.MaxAge, "every session cookie set is cleared")
		if c.Value == "" {
			continue
		}
		sess, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), c), webSessionName)
		require.NoError(t, err)
		assert.NotContains(t, sess.Values, sessKeyHubAccessToken, "no hub access token is minted")
		assert.NotContains(t, sess.Values, sessKeyHubRefreshToken, "no hub refresh token is minted")
		assert.NotContains(t, sess.Values, sessKeyUserID, "the session holds no user")
	}
}

// Fails if a dev web session whose session generation differs from the dev
// user's row (in either direction) survives: devAuthMiddleware must clear
// it, mint no hub token, and continue with no user, so the page goes to the
// login path and the hub API refuses the request.
func TestSessionGen_DevAuthWeb_GenerationMismatchEndsSession(t *testing.T) {
	cases := map[string]func(f *sessionGenFixture, t *testing.T) int64{
		// The session was established before a revocation.
		"row_ahead": func(f *sessionGenFixture, t *testing.T) int64 {
			gen := f.devGeneration(t)
			require.NoError(t, f.s.IncrementSessionGeneration(context.Background(), DevUserID))
			return gen
		},
		// The session records a generation the row does not have yet.
		"session_ahead": func(f *sessionGenFixture, t *testing.T) int64 {
			return f.devGeneration(t) + 1
		},
	}
	for name, sessionGen := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSessionGenFixture(t)
			ws := f.newDevSessionGenWeb(t)
			cookies := f.devSessionCookies(t, ws, sessionGen(f, t))

			page := webDo(ws, http.MethodGet, "/", cookies, true)
			assert.Equal(t, http.StatusFound, page.Code, "the page takes the login path: %s", page.Body.String())
			assert.NotContains(t, page.Body.String(), "__SCION_DATA__", "the page is not served to the session")
			requireDevSessionEnded(t, ws, page)

			api := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
			assert.Equal(t, http.StatusUnauthorized, api.Code, "the hub API refuses the request: %s", api.Body.String())
			requireDevSessionEnded(t, ws, api)

			me := webDo(ws, http.MethodGet, "/auth/me", cookies, false)
			assert.Equal(t, http.StatusUnauthorized, me.Code, "the web layer has no user for the session")
		})
	}
}

// Fails if a dev web session at the dev user's current session generation
// is ended or has its hub tokens replaced, including a session from a
// sign-in link after a revocation, whose tokens must carry the row's
// generation (populateDevSession).
func TestSessionGen_DevAuthWeb_MatchingGenerationUnchanged(t *testing.T) {
	f := newSessionGenFixture(t)
	ws := f.newDevSessionGenWeb(t)
	require.NoError(t, f.s.IncrementSessionGeneration(context.Background(), DevUserID))
	gen := f.devGeneration(t)
	require.NotZero(t, gen)

	cookies := devSignIn(t, ws)
	signedIn, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), cookies...), webSessionName)
	require.NoError(t, err, "the sign-in link set a session")
	vals := signedIn.Values
	assert.Equal(t, gen, vals[sessKeySessionGeneration], "the session records the row's generation")
	signedInAccess, _ := vals[sessKeyHubAccessToken].(string)
	require.NotEmpty(t, signedInAccess)
	claims, err := f.srv.userTokenService.ValidateUserToken(signedInAccess)
	require.NoError(t, err)
	assert.Equal(t, gen, claims.SessionGeneration, "the sign-in tokens carry the row's generation")

	for i := 0; i < 2; i++ {
		page := webDo(ws, http.MethodGet, "/", cookies, true)
		assert.Equal(t, http.StatusOK, page.Code, page.Body.String())
		assert.Nil(t, sessionCookieOf(page), "the session is left as it is")

		api := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
		assert.Equal(t, http.StatusOK, api.Code, api.Body.String())
		if after, ok := webSessionAfter(t, ws, api); ok {
			assert.Equal(t, signedInAccess, after[sessKeyHubAccessToken], "the hub token is not replaced")
		}
	}
}

// Fails if a dev web session of a dev user whose sessions were never
// revoked (generation 0) is ended or re-minted.
func TestSessionGen_DevAuthWeb_NeverRevokedUnaffected(t *testing.T) {
	f := newSessionGenFixture(t)
	ws := f.newDevSessionGenWeb(t)
	require.Equal(t, int64(0), f.devGeneration(t))

	signedIn := devSignIn(t, ws)
	legacy := f.devSessionCookies(t, ws, 0)
	for name, cookies := range map[string][]*http.Cookie{"sign_in": signedIn, "generation_zero": legacy} {
		t.Run(name, func(t *testing.T) {
			page := webDo(ws, http.MethodGet, "/", cookies, true)
			assert.Equal(t, http.StatusOK, page.Code, page.Body.String())
			assert.Nil(t, sessionCookieOf(page), "the session is left as it is")
			api := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
			assert.Equal(t, http.StatusOK, api.Code, api.Body.String())
		})
	}
}

// Fails if a non-dev (OAuth) web session on a dev-auth server keeps
// working after its user's session generation moves past the session's:
// devAuthMiddleware loads such a session itself (sessionAuthMiddleware's
// check never runs for it), so it must end it, minting nothing. A session
// at the row's generation is left as it is.
func TestSessionGen_DevAuthWeb_NonDevSessionFollowsGeneration(t *testing.T) {
	f := newSessionGenFixture(t)
	ws := f.newDevSessionGenWeb(t)
	access, refresh := f.mintPair(t, 0, ClientTypeWeb)
	stale := f.webSessionCookies(t, ws, map[string]interface{}{
		sessKeyHubAccessToken:    access,
		sessKeyHubRefreshToken:   refresh,
		sessKeyHubTokenExpiry:    time.Now().Add(time.Hour).UnixMilli(),
		sessKeySessionGeneration: int64(0),
	})
	api := webDo(ws, http.MethodGet, "/api/v1/auth/me", stale, false)
	require.Equal(t, http.StatusOK, api.Code, "accepted before the revoke: %s", api.Body.String())

	gen := f.revoke(t)
	page := webDo(ws, http.MethodGet, "/", stale, true)
	assert.Equal(t, http.StatusFound, page.Code, "the page takes the login path: %s", page.Body.String())
	requireDevSessionEnded(t, ws, page)
	api = webDo(ws, http.MethodGet, "/api/v1/auth/me", stale, false)
	assert.Equal(t, http.StatusUnauthorized, api.Code, "the hub API refuses the request: %s", api.Body.String())
	requireDevSessionEnded(t, ws, api)

	// Control: a session at the row's generation is served and kept.
	access, refresh = f.mintPair(t, gen, ClientTypeWeb)
	current := f.webSessionCookies(t, ws, map[string]interface{}{
		sessKeyHubAccessToken:    access,
		sessKeyHubRefreshToken:   refresh,
		sessKeyHubTokenExpiry:    time.Now().Add(time.Hour).UnixMilli(),
		sessKeySessionGeneration: gen,
	})
	page = webDo(ws, http.MethodGet, "/", current, true)
	assert.Equal(t, http.StatusOK, page.Code, page.Body.String())
	assert.Nil(t, sessionCookieOf(page), "the session is left as it is")
	api = webDo(ws, http.MethodGet, "/api/v1/auth/me", current, false)
	assert.Equal(t, http.StatusOK, api.Code, api.Body.String())
	if after, ok := webSessionAfter(t, ws, api); ok {
		assert.Equal(t, access, after[sessKeyHubAccessToken], "the hub token is not replaced")
	}
}

// Fails if a dev sign-in mints its hub tokens with anything but the dev
// user's row generation, or with a non-zero generation when the row cannot
// be read.
func TestSessionGen_DevSignIn_MintsWithRowGeneration(t *testing.T) {
	for name, tc := range map[string]struct {
		row  *store.User
		want int64
	}{
		"row_read":       {row: &store.User{ID: DevUserID, Email: "dev@localhost", Role: "admin", Status: store.UserStatusActive, SessionGeneration: 4}, want: 4},
		"row_unreadable": {row: nil, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			ws := newDevAuthWebServer(t)
			svc, err := NewUserTokenService(UserTokenConfig{})
			require.NoError(t, err)
			ws.SetUserTokenService(svc)
			devStore := ws.store.(*proxyAuthStore)
			delete(devStore.users, DevUserID)
			if tc.row != nil {
				devStore.users[DevUserID] = tc.row
			}

			sess, err := ws.sessionStore.New(httptest.NewRequest(http.MethodGet, devSignInPath, nil), webSessionName)
			require.NoError(t, err)
			ws.populateDevSession(context.Background(), sess)
			for _, key := range []string{sessKeyHubAccessToken, sessKeyHubRefreshToken} {
				tok, _ := sess.Values[key].(string)
				require.NotEmpty(t, tok, key)
				claims, err := svc.ValidateUserToken(tok)
				require.NoError(t, err, key)
				assert.Equal(t, tc.want, claims.SessionGeneration, key)
			}
		})
	}
}

// userRowFaultStore fails GetUser for one user id with a non-ErrNotFound
// error while its switch is armed, and counts the reads it failed.
type userRowFaultStore struct {
	store.Store
	uid    string
	fault  *storeFaultSwitch
	failed atomic.Int64
}

func (s *userRowFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.uid && s.fault.Active() {
		s.failed.Add(1)
		return nil, errors.New("database is locked")
	}
	return s.Store.GetUser(ctx, id)
}

// requireDevSessionKept asserts that rec neither cleared the session nor
// replaced its hub tokens: no session cookie it sets has a negative
// Max-Age, and any session it saves still holds the user and the original
// access and refresh tokens.
func requireDevSessionKept(t *testing.T, ws *WebServer, rec *httptest.ResponseRecorder, access, refresh string) {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name != webSessionName {
			continue
		}
		assert.GreaterOrEqual(t, c.MaxAge, 0, "the session is not cleared")
		sess, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), c), webSessionName)
		require.NoError(t, err)
		assert.Equal(t, DevUserID, sess.Values[sessKeyUserID], "the session keeps its user")
		assert.Equal(t, access, sess.Values[sessKeyHubAccessToken], "no hub access token is minted")
		assert.Equal(t, refresh, sess.Values[sessKeyHubRefreshToken], "no hub refresh token is minted")
	}
}

// When devAuthMiddleware cannot read the user row of an existing session
// (a lookup fault), the session is kept and nothing is minted; the hub API
// call it forwards still fails closed in the bearer arm, which cannot read
// the row either. The session is at a non-zero generation, so a failed
// read that is treated as generation 0 also ends it.
//
// Fails if a row-read failure in devAuthMiddleware clears the session (for
// example by dropping the err == nil guard, or by comparing against a zero
// row), if it mints hub tokens (for example by restoring a re-mint
// fallback), or if the bearer arm admits a token whose user row cannot be
// read (accountLookupFault in UnifiedAuthMiddleware).
func TestSessionGen_DevAuthWeb_RowReadFault_KeepsSessionNoMint(t *testing.T) {
	f := newSessionGenFixture(t)
	faulty := &userRowFaultStore{Store: f.s, uid: DevUserID, fault: &storeFaultSwitch{}}
	// The bearer arm's user store is captured when the hub handler is
	// built, so it is replaced before the web server mounts the hub.
	f.srv.authConfig.UserStore = faulty
	ws := f.newDevSessionGenWeb(t)
	ws.SetStore(faulty)

	require.NoError(t, f.s.IncrementSessionGeneration(context.Background(), DevUserID))
	gen := f.devGeneration(t)
	require.NotZero(t, gen)
	cookies := f.devSessionCookies(t, ws, gen)
	stored, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), cookies...), webSessionName)
	require.NoError(t, err)
	access, _ := stored.Values[sessKeyHubAccessToken].(string)
	refresh, _ := stored.Values[sessKeyHubRefreshToken].(string)
	require.NotEmpty(t, access)
	require.NotEmpty(t, refresh)

	// Warm the test identity classifier's kind cache with one successful
	// request. Cold, the classifier reads the row first and answers 500
	// before the generation check runs.
	warm := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
	require.Equal(t, http.StatusOK, warm.Code, warm.Body.String())
	require.Equal(t, tiKindHuman, ws.testIdentityKindCached(DevUserID))

	faulty.fault.Arm()

	page := webDo(ws, http.MethodGet, "/", cookies, true)
	require.NotZero(t, faulty.failed.Load(), "the page request read the row and the read failed")
	requireDevSessionKept(t, ws, page, access, refresh)

	before := faulty.failed.Load()
	api := webDo(ws, http.MethodGet, "/api/v1/auth/me", cookies, false)
	assert.Greater(t, faulty.failed.Load(), before, "the API request read the row and the read failed")
	assert.Equal(t, http.StatusServiceUnavailable, api.Code, "the bearer arm fails closed: %s", api.Body.String())
	assert.Contains(t, api.Body.String(), "store_error")
	requireDevSessionKept(t, ws, api, access, refresh)
}

// Fails if the web cookie check refuses only when the users row is ahead of
// the cookie (restoring ">" in sessionAuthMiddleware): a cookie whose
// generation is ahead of the row must also end the session.
func TestSessionGen_CookieCheck_AnyInequality(t *testing.T) {
	f := newSessionGenFixture(t)
	ws := f.newWeb(t, nil)

	current := f.webSessionCookies(t, ws, map[string]interface{}{sessKeySessionGeneration: int64(0)})
	rec := webDo(ws, http.MethodGet, "/", current, true)
	require.NotEqual(t, "/auth/login", rec.Header().Get("Location"), "a session at the row's generation is served (status %d)", rec.Code)

	ahead := f.webSessionCookies(t, ws, map[string]interface{}{sessKeySessionGeneration: int64(1)})
	rec = webDo(ws, http.MethodGet, "/", ahead, true)
	assert.Equal(t, http.StatusFound, rec.Code, "a cookie ahead of the row ends the session")
	assert.Equal(t, "/auth/login", rec.Header().Get("Location"))
	assertSessionCookieCleared(t, rec)

	f.revoke(t)
	rec = webDo(ws, http.MethodGet, "/", current, true)
	assert.Equal(t, http.StatusFound, rec.Code, "a session behind the row ends too")
	assert.Equal(t, "/auth/login", rec.Header().Get("Location"))
}

// mockGoogleForFixtureUser is a mocked Google token and userinfo exchange
// that resolves to the fixture user.
func (f *sessionGenFixture) mockGoogleForFixtureUser() *http.Client {
	return &http.Client{Transport: &mockOAuthTransport{
		tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
		userinfoJSON: `{"id":"sub-` + f.user.ID + `","email":"` + f.user.Email + `","verified_email":true,"name":"Session Gen User"}`,
	}}
}

// Fails if the CLI code-exchange issuance (POST /api/v1/auth/cli/token) or
// the web BFF OAuth callback mints with a session generation other than
// the users row's (for example 0): their tokens, issued after a revoke,
// would then be refused. Complements TokenIssuedAfterRevoke_Accepted,
// which covers POST /api/v1/auth/token.
func TestSessionGen_IssuedAfterRevoke_CLIFlowAndOAuthCallback_Accepted(t *testing.T) {
	t.Run("cli_token_exchange", func(t *testing.T) {
		f := newSessionGenFixture(t)
		gen := f.revoke(t)
		f.srv.oauthService = NewOAuthService(OAuthConfig{
			CLI: OAuthClientConfig{Google: OAuthProviderConfig{ClientID: "test-cli-id", ClientSecret: "test-cli-secret"}},
		}, nil)
		f.srv.oauthService.httpClient = f.mockGoogleForFixtureUser()
		rec := doRequestNoAuth(t, f.srv, http.MethodPost, "/api/v1/auth/cli/token", CLIAuthTokenRequest{
			Code: "test-code", CallbackURL: "http://localhost:18271/callback", Provider: "google",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp CLIAuthTokenResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for name, tok := range map[string]string{"access": resp.AccessToken, "refresh": resp.RefreshToken} {
			claims, err := f.srv.userTokenService.ValidateUserToken(tok)
			require.NoError(t, err, name)
			assert.Equal(t, gen, claims.SessionGeneration, "%s token carries the users row's generation", name)
			assert.Equal(t, http.StatusOK, f.me(t, tok).Code, "%s token is accepted", name)
		}
	})

	t.Run("web_oauth_callback", func(t *testing.T) {
		f := newSessionGenFixture(t)
		gen := f.revoke(t)
		ws := f.newWeb(t, nil)
		ws.SetOAuthService(NewOAuthService(OAuthConfig{
			Web: OAuthClientConfig{Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"}},
		}, nil))
		ws.oauthService.httpClient = f.mockGoogleForFixtureUser()

		reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
		recSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyOAuthState] = "state-sessgen"
		require.NoError(t, sess.Save(reqSetup, recSetup))

		rec := webDo(ws, http.MethodGet, "/auth/callback/google?code=test-code&state=state-sessgen", recSetup.Result().Cookies(), true)
		require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Header().Get("Location"), "error", "the callback signed the user in")
		vals, ok := webSessionAfter(t, ws, rec)
		require.True(t, ok, "the callback saved a session")
		assert.Equal(t, gen, vals[sessKeySessionGeneration], "the session records the users row's generation")
		for _, key := range []string{sessKeyHubAccessToken, sessKeyHubRefreshToken} {
			tok, _ := vals[key].(string)
			require.NotEmpty(t, tok, key)
			claims, err := f.srv.userTokenService.ValidateUserToken(tok)
			require.NoError(t, err, key)
			assert.Equal(t, gen, claims.SessionGeneration, "%s carries the users row's generation", key)
			assert.Equal(t, http.StatusOK, f.me(t, tok).Code, "%s is accepted", key)
		}
	})
}
