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

package hub

// These tests change user_access_mode at runtime (via ApplySnapshot, the
// same entry point the settings-propagation path uses) and then drive a
// fresh browser login on each login path. Each path must honour the new
// mode on the very next login, without rebuilding the Server or WebServer.
//
// Login paths covered:
//   - WebServer OAuth callback       (GET /auth/callback/{provider})
//   - WebServer proxy-auth middleware (IAP / proxy assertion, new session)
//   - Server.provisionUser — the shared decision point for
//     POST /api/v1/auth/login, /api/v1/auth/token, /api/v1/auth/cli/token,
//     /api/v1/auth/cli/device/token and the proxy user provisioner.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveModeStore extends proxyAuthStore with the invite lookup that
// checkUserAuthorized needs in invite_only mode.
type liveModeStore struct {
	*proxyAuthStore
}

func newLiveModeStore() *liveModeStore {
	return &liveModeStore{proxyAuthStore: newProxyAuthStoreWithRoles()}
}

func (s *liveModeStore) IsUserInvitedOrActive(_ context.Context, email string) (bool, error) {
	for _, u := range s.users {
		if strings.EqualFold(u.Email, email) &&
			(u.Status == store.UserStatusInvited || u.Status == store.UserStatusActive) {
			return true, nil
		}
	}
	return false, nil
}

// newLiveModeServer returns a Server whose access settings start in the given
// mode. It is used as the WebServer's AccessSettingsProvider, as in
// cmd/server_foreground.go.
func newLiveModeServer(st store.Store, mode string) *Server {
	srv := &Server{
		store:       st,
		auditLogger: &LogAuditLogger{},
		maintenance: NewMaintenanceState(false, ""),
	}
	srv.config.UserAccessMode = mode
	return srv
}

func setLiveAccessMode(t *testing.T, srv *Server, mode string) {
	t.Helper()
	ApplySnapshot(srv, Layer1Snapshot{UserAccessMode: mode})
	require.Equal(t, mode, srv.UserAccessMode())
}

// oauthCallbackLogin drives one complete OAuth callback for email on a fresh
// session and returns the redirect location.
func oauthCallbackLogin(t *testing.T, ws *WebServer, email string) string {
	t.Helper()
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockOAuthTransport{
			tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
			userinfoJSON: `{"id":"id-` + email + `","email":"` + email + `","verified_email":true,"name":"Live Mode User"}`,
		},
	}

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	const oauthState = "live-mode-state"
	sess.Values[sessKeyOAuthState] = oauthState
	require.NoError(t, sess.Save(reqSetup, recSetup))
	cookies := recSetup.Result().Cookies()
	require.NotEmpty(t, cookies)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=test-code&state="+oauthState, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	return resp.Header.Get("Location")
}

func TestOAuthCallback_HonoursRuntimeUserAccessModeChange(t *testing.T) {
	st := newLiveModeStore()
	srv := newLiveModeServer(st, "invite_only")

	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: "test-session-secret-for-live-mode-oauth-1234567890",
		BaseURL:       "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"},
		},
	}, nil)
	ws.store = st
	ws.SetAccessSettingsProvider(srv)

	const deniedLocation = "/login?error=unauthorized_domain"

	// invite_only at startup: an uninvited user is turned away.
	assert.Equal(t, deniedLocation, oauthCallbackLogin(t, ws, "first@other.example"))
	_, err := st.GetUserByEmail(context.Background(), "first@other.example")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Loosen to open at runtime: the same user now signs in.
	setLiveAccessMode(t, srv, "open")
	assert.Equal(t, "/", oauthCallbackLogin(t, ws, "first@other.example"),
		"OAuth callback must honour user_access_mode=open set after startup")
	_, err = st.GetUserByEmail(context.Background(), "first@other.example")
	require.NoError(t, err)

	// Tighten back to invite_only at runtime: a different uninvited user is
	// turned away on their next login.
	setLiveAccessMode(t, srv, "invite_only")
	assert.Equal(t, deniedLocation, oauthCallbackLogin(t, ws, "second@other.example"),
		"OAuth callback must honour user_access_mode=invite_only set after startup")
	_, err = st.GetUserByEmail(context.Background(), "second@other.example")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestProxyAuthMiddleware_HonoursRuntimeUserAccessModeChange(t *testing.T) {
	st := newLiveModeStore()
	srv := newLiveModeServer(st, "invite_only")

	mockAuth := &mockProxyAuthenticator{}
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode:           "proxy",
		ProxyAuthenticator: mockAuth,
	})
	ws.SetStore(st)
	ws.SetAccessSettingsProvider(srv)

	// proxyLogin sends one request with no session cookie, so the proxy
	// assertion is evaluated as a fresh login.
	proxyLogin := func(email string) int {
		mockAuth.user = &ProxyUserInfo{Subject: "sub-" + email, Email: email, Domain: "other.example"}
		req := httptest.NewRequest(http.MethodGet, "/projects", nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusForbidden, proxyLogin("first@other.example"))
	_, err := st.GetUserByEmail(context.Background(), "first@other.example")
	require.ErrorIs(t, err, store.ErrNotFound)

	setLiveAccessMode(t, srv, "open")
	assert.NotEqual(t, http.StatusForbidden, proxyLogin("first@other.example"),
		"proxy login must honour user_access_mode=open set after startup")
	_, err = st.GetUserByEmail(context.Background(), "first@other.example")
	require.NoError(t, err)

	setLiveAccessMode(t, srv, "invite_only")
	assert.Equal(t, http.StatusForbidden, proxyLogin("second@other.example"),
		"proxy login must honour user_access_mode=invite_only set after startup")
	_, err = st.GetUserByEmail(context.Background(), "second@other.example")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestProvisionUser_HonoursRuntimeUserAccessModeChange(t *testing.T) {
	st := newInviteFlowStore()
	srv := newLiveModeServer(st, "invite_only")
	ctx := context.Background()

	_, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "first@other.example"})
	require.ErrorIs(t, err, ErrAccessDenied)

	setLiveAccessMode(t, srv, "open")
	user, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "first@other.example"})
	require.NoError(t, err, "provisionUser must honour user_access_mode=open set after startup")
	require.NotNil(t, user)

	setLiveAccessMode(t, srv, "invite_only")
	_, err = srv.provisionUser(ctx, &ExternalUserInfo{Email: "second@other.example"})
	require.ErrorIs(t, err, ErrAccessDenied,
		"provisionUser must honour user_access_mode=invite_only set after startup")
}
