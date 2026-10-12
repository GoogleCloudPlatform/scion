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

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// End-to-end regression: a GitHub web login, through the real callback
// handler, must reach the same outcome as the existing Google callback
// tests. Unlike Google's single userinfo call, GitHub's login always makes
// two calls after the token exchange (GET /user, then GET /user/emails —
// see getGitHubUserInfo), so the mock transport here answers three
// endpoints instead of two.
// ---------------------------------------------------------------------------

// mockGitHubOAuthTransport answers the GitHub token exchange, GET /user, and
// GET /user/emails endpoints for a full web-login round trip.
type mockGitHubOAuthTransport struct {
	tokenJSON  string
	userJSON   string
	emailsJSON string
}

func (t *mockGitHubOAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	switch {
	case strings.Contains(req.URL.String(), "github.com/login/oauth/access_token"):
		body = t.tokenJSON
	case req.URL.String() == githubEmailURL:
		body = t.emailsJSON
	case req.URL.String() == githubUserURL:
		body = t.userJSON
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found"))}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// TestOAuthCallback_GitHub_VerifiedPrimaryEmail_Provisions is the full
// GitHub web-login regression: callback -> getGitHubUserInfo (always
// fetching /user/emails) -> WebServer.handleOAuthCallback's own find-or-create
// -> session, with a normal primary-verified email.
func TestOAuthCallback_GitHub_VerifiedPrimaryEmail_Provisions(t *testing.T) {
	const secret = "test-session-secret-for-github-oauth-1234567890"
	const email = "github-user@example.com"

	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: secret,
		BaseURL:       "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
		},
	}, nil)
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockGitHubOAuthTransport{
			tokenJSON: `{"access_token":"mock-token","token_type":"bearer","scope":"read:user,user:email"}`,
			userJSON:  `{"id":42,"login":"octocat","name":"GitHub User","email":"` + email + `","avatar_url":"https://example.com/a.png"}`,
			emailsJSON: `[
				{"email":"` + email + `","primary":true,"verified":true}
			]`,
		},
	}

	st := newProxyAuthStoreWithRoles()
	ws.store = st

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/github", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	oauthState := "test-state-github"
	sess.Values[sessKeyOAuthState] = oauthState
	require.NoError(t, sess.Save(reqSetup, recSetup))
	cookies := recSetup.Result().Cookies()
	require.NotEmpty(t, cookies)

	callbackURL := "/auth/callback/github?code=test-code&state=" + oauthState
	reqCallback := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	for _, c := range cookies {
		reqCallback.AddCookie(c)
	}
	recCallback := httptest.NewRecorder()

	ws.Handler().ServeHTTP(recCallback, reqCallback)

	resp := recCallback.Result()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/", resp.Header.Get("Location"), "GitHub OAuth callback must redirect to '/' on success")

	user, err := st.GetUserByEmail(context.Background(), email)
	require.NoError(t, err)
	assert.Equal(t, email, user.Email)
}

// TestOAuthCallback_GitHub_ProfileEmailUnverified_Rejected is the same
// full callback path with a profile email that GitHub's own email list
// marks unverified and no other verified address: the login must fail
// before any session is established, and no user record is created.
func TestOAuthCallback_GitHub_ProfileEmailUnverified_Rejected(t *testing.T) {
	const secret = "test-session-secret-for-github-oauth-unverified-1234"
	const email = "github-user@example.com"

	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: secret,
		BaseURL:       "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
		},
	}, nil)
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockGitHubOAuthTransport{
			tokenJSON: `{"access_token":"mock-token","token_type":"bearer","scope":"read:user,user:email"}`,
			userJSON:  `{"id":42,"login":"octocat","name":"GitHub User","email":"` + email + `","avatar_url":""}`,
			emailsJSON: `[
				{"email":"` + email + `","primary":true,"verified":false}
			]`,
		},
	}

	st := newProxyAuthStoreWithRoles()
	ws.store = st

	reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/github", nil)
	recSetup := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
	require.NoError(t, err)
	oauthState := "test-state-github-unverified"
	sess.Values[sessKeyOAuthState] = oauthState
	require.NoError(t, sess.Save(reqSetup, recSetup))
	cookies := recSetup.Result().Cookies()
	require.NotEmpty(t, cookies)

	callbackURL := "/auth/callback/github?code=test-code&state=" + oauthState
	reqCallback := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	for _, c := range cookies {
		reqCallback.AddCookie(c)
	}
	recCallback := httptest.NewRecorder()

	ws.Handler().ServeHTTP(recCallback, reqCallback)

	resp := recCallback.Result()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	assert.NotEqual(t, "/", resp.Header.Get("Location"), "an unverified-email GitHub login must not redirect to the success location")
	assert.Equal(t, "/login?error=exchange_failed", resp.Header.Get("Location"))

	if _, err := st.GetUserByEmail(context.Background(), email); err == nil {
		t.Error("expected no user record to be created for an unverified GitHub email")
	}
}
