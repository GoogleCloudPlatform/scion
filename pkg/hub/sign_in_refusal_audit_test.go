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

// A refused sign-in on the web OAuth callback, the proxy sign-in path and
// the identity resolver's new-user branch records one login_denied audit
// entry with the same reason as provisionUser (ptone/scion#4537). Allowed
// sign-ins record none, and the allow/deny outcome is the same as before
// the audit write.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signInAllowedAuditCases are sign-ins every path admits for a new email.
var signInAllowedAuditCases = []struct {
	name    string
	mode    string
	domains []string
	admins  []string
	email   string
}{
	{"open without domains", "open", nil, nil, "user@other.example"},
	{"domain_restricted inside domain", "domain_restricted", []string{"allowed.example"}, nil, "user@allowed.example"},
	{"invite_only admin", "invite_only", nil, []string{"boss@other.example"}, "boss@other.example"},
}

func requireOneDenialEvent(t *testing.T, rec *inviteAuditRecorder, email, wantReason string) {
	t.Helper()
	require.Len(t, rec.events, 1, "a refused sign-in writes exactly one audit entry")
	e := rec.events[0]
	assert.Equal(t, InviteAuditLoginDenied, e.EventType)
	assert.Equal(t, email, e.Email)
	assert.False(t, e.Success)
	assert.Equal(t, wantReason, e.FailReason)
}

func newAuditOAuthWebServer(t *testing.T, st store.Store, srv *Server) *WebServer {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: "test-session-secret-for-refusal-audit-1234567890",
		BaseURL:       "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"},
		},
	}, nil)
	ws.store = st
	ws.SetAccessSettingsProvider(srv)
	return ws
}

func newAuditProxyWebServer(t *testing.T, st store.Store, srv *Server) (*WebServer, func(email string) int) {
	t.Helper()
	mockAuth := &mockProxyAuthenticator{}
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode:           "proxy",
		ProxyAuthenticator: mockAuth,
	})
	ws.SetStore(st)
	ws.SetAccessSettingsProvider(srv)
	login := func(email string) int {
		mockAuth.user = &ProxyUserInfo{Subject: "sub-" + email, Email: email}
		req := httptest.NewRequest(http.MethodGet, "/projects", nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	return ws, login
}

// newAuditResolver wires a resolver the way server.go wires it.
func newAuditResolver(st store.Store, srv *Server) *GoogleIdentityResolver {
	resolver := NewGoogleIdentityResolver(st, newMemExtIDStore(), srv.isUserAuthorized, nil, nil)
	resolver.SetSignInDenial(srv.signInDenial)
	resolver.SetSignInPolicyDeps(srv.signInPolicyDeps())
	return resolver
}

func resolveNewUser(resolver *GoogleIdentityResolver, email string) (*store.User, error) {
	return resolver.Resolve(context.Background(), &ValidatedGoogleIdentity{
		Subject:       "sub-" + email,
		Email:         email,
		EmailVerified: true,
		Issuer:        googleIssuerHTTPS,
	}, ResolvePolicy{})
}

func TestOAuthCallback_RefusalAudit(t *testing.T) {
	for _, tc := range signInDenialAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLiveModeStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec
			ws := newAuditOAuthWebServer(t, st, srv)

			wantCode := signInDeniedDomain
			if tc.wantInvite {
				wantCode = signInDeniedInviteOnly
			}
			assert.Equal(t, "/login?error="+wantCode, oauthCallbackLogin(t, ws, tc.email))
			_, err := st.GetUserByEmail(context.Background(), tc.email)
			require.ErrorIs(t, err, store.ErrNotFound)
			requireOneDenialEvent(t, rec, tc.email, tc.wantReason)
		})
	}
}

func TestOAuthCallback_AllowedNoRefusalAudit(t *testing.T) {
	for _, tc := range signInAllowedAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLiveModeStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			srv.config.AdminEmails = tc.admins
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec
			ws := newAuditOAuthWebServer(t, st, srv)

			assert.Equal(t, "/", oauthCallbackLogin(t, ws, tc.email))
			_, err := st.GetUserByEmail(context.Background(), tc.email)
			require.NoError(t, err)
			assert.Empty(t, rec.events)
		})
	}
}

func TestProxyAuth_RefusalAudit(t *testing.T) {
	for _, tc := range signInDenialAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLiveModeStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec
			_, login := newAuditProxyWebServer(t, st, srv)

			assert.Equal(t, http.StatusForbidden, login(tc.email))
			_, err := st.GetUserByEmail(context.Background(), tc.email)
			require.ErrorIs(t, err, store.ErrNotFound)
			requireOneDenialEvent(t, rec, tc.email, tc.wantReason)
		})
	}
}

func TestProxyAuth_AllowedNoRefusalAudit(t *testing.T) {
	for _, tc := range signInAllowedAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newLiveModeStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			srv.config.AdminEmails = tc.admins
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec
			_, login := newAuditProxyWebServer(t, st, srv)

			assert.Equal(t, http.StatusOK, login(tc.email))
			_, err := st.GetUserByEmail(context.Background(), tc.email)
			require.NoError(t, err)
			assert.Empty(t, rec.events)
		})
	}
}

func TestGoogleIdentityResolver_NewUserRefusalAudit(t *testing.T) {
	for _, tc := range signInDenialAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newInviteFlowStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec

			_, err := resolveNewUser(newAuditResolver(st, srv), tc.email)
			require.ErrorIs(t, err, ErrAccessDenied)
			_, err = st.GetUserByEmail(context.Background(), tc.email)
			require.Error(t, err)
			requireOneDenialEvent(t, rec, tc.email, tc.wantReason)
		})
	}
}

func TestGoogleIdentityResolver_NewUserAllowedNoRefusalAudit(t *testing.T) {
	for _, tc := range signInAllowedAuditCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newInviteFlowStore()
			srv := newLiveModeServer(st, tc.mode)
			srv.config.AuthorizedDomains = tc.domains
			srv.config.AdminEmails = tc.admins
			rec := &inviteAuditRecorder{}
			srv.auditLogger = rec

			user, err := resolveNewUser(newAuditResolver(st, srv), tc.email)
			require.NoError(t, err)
			require.NotNil(t, user)
			assert.Equal(t, tc.email, user.Email)
			assert.Empty(t, rec.events)
		})
	}
}

// The web server reads the audit logger from its access settings provider,
// which cmd/server_foreground.go sets to the hub Server.
func TestWebServer_AuditLoggerFromHubServer(t *testing.T) {
	srv := &Server{auditLogger: NewLogAuditLogger("[Hub Audit]", false)}
	ws := newTestWebServer(t, WebServerConfig{})
	assert.Nil(t, ws.auditLogger(), "no provider means no audit logger")

	ws.SetAccessSettingsProvider(srv)
	got := ws.auditLogger()
	require.NotNil(t, got)
	assert.Same(t, srv.GetAuditLogger(), got)
}
