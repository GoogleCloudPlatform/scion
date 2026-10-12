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

// Sign-in tests for provisioned records (design §5.6, §16.2 slice
// validation, §16.4 sign-in equivalence). They provision through the
// real handler (and, for the end-to-end slice, through the hubclient call
// the CLI makes) against the SQLite-backed hub, then sign in on each path.
// They assert only today's behaviour: a provisioned record is treated
// exactly as an invite-created record.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGoogleIssuer = "https://accounts.google.com"

// signInOutcome is what a sign-in path did with an email.
type signInOutcome struct {
	Admitted    bool
	ErrText     string
	Status      string
	Role        string
	HubMember   bool
	BindingCnt  int
	ExternalIDs int
	DisplayName string
}

// signInPaths drives one sign-in per call on each path, against srv and
// its store.
type signInPaths struct {
	t     *testing.T
	srv   *Server
	s     store.Store
	oauth *WebServer
	proxy *WebServer
	auth  *mockProxyAuthenticator
}

func newSignInPaths(t *testing.T, srv *Server, s store.Store) *signInPaths {
	t.Helper()
	oauth := newTestWebServer(t, WebServerConfig{
		SessionSecret: "test-session-secret-for-provision-oauth-1234567890",
		BaseURL:       "http://localhost:8080",
	})
	oauth.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{ClientID: "test-client-id", ClientSecret: "test-client-secret"},
		},
	}, nil)
	oauth.SetStore(s)
	oauth.SetAccessSettingsProvider(srv)

	auth := &mockProxyAuthenticator{}
	proxy := newTestWebServer(t, WebServerConfig{AuthMode: "proxy", ProxyAuthenticator: auth})
	proxy.SetStore(s)
	proxy.SetAccessSettingsProvider(srv)
	return &signInPaths{t: t, srv: srv, s: s, oauth: oauth, proxy: proxy, auth: auth}
}

// outcome reads the account state of email after a sign-in attempt.
func (p *signInPaths) outcome(email string, admitted bool, errText string) signInOutcome {
	p.t.Helper()
	ctx := context.Background()
	o := signInOutcome{Admitted: admitted, ErrText: errText}
	u, err := p.s.GetUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return o
	}
	require.NoError(p.t, err)
	o.Status, o.Role, o.DisplayName = u.Status, u.Role, u.DisplayName
	group, err := p.s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(p.t, err)
	_, err = p.s.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, u.ID)
	o.HubMember = err == nil
	bindings, err := p.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
	require.NoError(p.t, err)
	o.BindingCnt = len(bindings)
	ids, err := p.s.GetExternalIdentitiesByUserID(ctx, u.ID)
	require.NoError(p.t, err)
	o.ExternalIDs = len(ids)
	return o
}

// apiLogin is the shared decision point of POST /api/v1/auth/login, the
// token and CLI/device token exchanges and the proxy user provisioner.
func (p *signInPaths) apiLogin(email string) signInOutcome {
	p.t.Helper()
	_, err := p.srv.provisionUser(context.Background(), &ExternalUserInfo{Email: email, DisplayName: "Provider Name"})
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return p.outcome(email, err == nil, errText)
}

// webOAuth drives one complete web OAuth callback.
func (p *signInPaths) webOAuth(email string) signInOutcome {
	p.t.Helper()
	loc := oauthCallbackLogin(p.t, p.oauth, email)
	return p.outcome(email, loc == "/", loc)
}

// webProxy sends one proxy-asserted request with no session.
func (p *signInPaths) webProxy(email string) signInOutcome {
	p.t.Helper()
	p.auth.user = &ProxyUserInfo{Subject: "sub-" + email, Email: email, DisplayName: "Provider Name", Domain: "gmail.com"}
	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	p.proxy.Handler().ServeHTTP(rec, req)
	return p.outcome(email, rec.Code == http.StatusOK, http.StatusText(rec.Code))
}

// googleResolver is the shared resolver behind the GE exchange endpoint
// and the external-bearer path.
func (p *signInPaths) googleResolver(email string) signInOutcome {
	p.t.Helper()
	_, err := p.srv.authConfig.GoogleResolver.Resolve(context.Background(), &ValidatedGoogleIdentity{
		Subject: "sub-" + email, Email: email, EmailVerified: true, Issuer: testGoogleIssuer, DisplayName: "Provider Name",
	}, ResolvePolicy{})
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return p.outcome(email, err == nil, errText)
}

func (p *signInPaths) all() map[string]func(string) signInOutcome {
	return map[string]func(string) signInOutcome{
		"api login":       p.apiLogin,
		"web oauth":       p.webOAuth,
		"web proxy":       p.webProxy,
		"google resolver": p.googleResolver,
	}
}

func setAccessConfig(srv *Server, mode string, domains []string) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.config.UserAccessMode = mode
	srv.config.AuthorizedDomains = domains
}

// TestHandleProvisionUser_EndToEndSignIn is the §16.2 slice validation:
// provision through the hubclient call the CLI makes, then sign in under
// invite_only on the API login, web OAuth and web proxy paths, and confirm
// each user is active with the configured default role and its grants.
func TestHandleProvisionUser_EndToEndSignIn(t *testing.T) {
	ctx := context.Background()
	srv, s := testServerNoDevAuth(t)
	setAccessConfig(srv, "invite_only", nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// Provision as an interactive super-admin session on a hub without dev
	// auth; provisioning is refused in dev-auth mode.
	adminID := tid("e2e-super")
	createTestUserWithRole(t, s, adminID, "e2e-super@example.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	token, _, _, err := srv.userTokenService.GenerateTokenPair(adminID, "e2e-super@example.com", "E2E Super", store.UserRoleAdmin, ClientTypeWeb)
	require.NoError(t, err)
	client, err := hubclient.New(ts.URL, hubclient.WithBearerToken(token))
	require.NoError(t, err)
	paths := newSignInPaths(t, srv, s)

	cases := []struct {
		name  string
		email string
		login func(string) signInOutcome
	}{
		{"api login", "e2e-api@gmail.com", paths.apiLogin},
		{"web oauth", "e2e-oauth@gmail.com", paths.webOAuth},
		{"web proxy", "e2e-proxy@gmail.com", paths.webProxy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Without a record, invite_only turns the person away.
			denied := tc.login("uninvited-" + tc.email)
			assert.False(t, denied.Admitted, "an unprovisioned email is refused under invite_only")

			name := "Admin Chosen Name"
			resp, err := client.Users().Provision(ctx, &hubclient.ProvisionUserRequest{Email: tc.email, DisplayName: &name})
			require.NoError(t, err)
			require.True(t, resp.Created)
			assert.Equal(t, store.UserStatusInvited, resp.User.Status)

			got := tc.login(tc.email)
			require.True(t, got.Admitted, "provisioned user signs in: %s", got.ErrText)
			assert.Equal(t, store.UserStatusActive, got.Status)
			assert.Equal(t, srv.DefaultUserRole(), got.Role, "the configured default role applies at activation")
			assert.True(t, got.HubMember, "member grants are synced at activation")

			// Provider name wins at activation (OD-6 a).
			u, err := s.GetUserByEmail(ctx, tc.email)
			require.NoError(t, err)
			assert.NotEqual(t, name, u.DisplayName, "the provider name replaces the provisioned name at activation")
			assert.NotEmpty(t, u.DisplayName)
		})
	}

	t.Run("viewer default role", func(t *testing.T) {
		srv.mu.Lock()
		srv.config.DefaultUserRole = store.UserRoleViewer
		srv.mu.Unlock()
		defer func() {
			srv.mu.Lock()
			srv.config.DefaultUserRole = ""
			srv.mu.Unlock()
		}()
		_, err := client.Users().Provision(ctx, &hubclient.ProvisionUserRequest{Email: "e2e-viewer@gmail.com"})
		require.NoError(t, err)
		got := paths.apiLogin("e2e-viewer@gmail.com")
		require.True(t, got.Admitted, got.ErrText)
		assert.Equal(t, store.UserRoleViewer, got.Role)
		assert.False(t, got.HubMember)
		assert.Equal(t, 1, got.BindingCnt, "the hub-viewer binding is the only grant")
	})
}

// TestHandleProvisionUser_SignInEquivalence characterizes that a
// provisioned record and an invite-created record behave identically on
// every sign-in path, including the GE exchange / external-bearer
// resolver, under the same configuration. It asserts equality between the
// two records, not any new behaviour.
func TestHandleProvisionUser_SignInEquivalence(t *testing.T) {
	type config struct {
		name    string
		mode    string
		domains []string
	}
	configs := []config{
		{"invite_only", "invite_only", nil},
		{"invite_only, domains exclude the email", "invite_only", []string{"corp.example"}},
		{"domain_restricted without domains", "domain_restricted", nil},
		{"open", "open", nil},
	}
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			f := newProvisionFixture(t)
			paths := newSignInPaths(t, f.srv, f.s)
			for pathName, login := range paths.all() {
				prov := "prov-" + strings.ReplaceAll(pathName, " ", "-") + "@gmail.com"
				inv := "inv-" + strings.ReplaceAll(pathName, " ", "-") + "@gmail.com"

				// Records are created under invite_only with no domain
				// restriction, then the configuration under test applies.
				setAccessConfig(f.srv, "invite_only", nil)
				// The provisioned record also carries a display name, the one
				// stored field provision adds over invite.
				rec := provisionAs(t, f.srv, f.hubAdmin, map[string]interface{}{"email": prov, "note": "n", "displayName": "Admin Name"})
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{Email: inv, Note: "n"})
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

				setAccessConfig(f.srv, cfg.mode, cfg.domains)
				p := login(prov)
				i := login(inv)
				// Error texts name the email on some paths; compare the
				// outcome class instead.
				p.ErrText, i.ErrText = "", ""
				if p.Admitted {
					// Activation applies the provider name (OD-6 a), so the
					// two records end with the same display name.
					assert.NotEqual(t, "Admin Name", p.DisplayName, "%s: the provider name applies at activation", pathName)
				} else {
					// A refused sign-in leaves both records as created.
					assert.Equal(t, "Admin Name", p.DisplayName, pathName)
					assert.Equal(t, "", i.DisplayName, pathName)
					p.DisplayName, i.DisplayName = "", ""
				}
				assert.Equal(t, i, p, "%s: provisioned and invited records must behave identically", pathName)
				if cfg.mode == "invite_only" && cfg.domains == nil {
					assert.True(t, p.Admitted, "%s admits the provisioned record under invite_only", pathName)
				}
				if cfg.domains != nil || cfg.mode == "domain_restricted" {
					assert.False(t, p.Admitted, "%s refuses the provisioned record when policy excludes it", pathName)
					assert.Equal(t, store.UserStatusInvited, p.Status, "a refused sign-in leaves the record invited")
				}
			}
		})
	}
}
