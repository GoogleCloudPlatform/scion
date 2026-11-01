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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
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

// The OAuth callback redirects to the session's returnTo only when it is a
// path on this server; any other value ends at /.
func TestOAuthCallback_RedirectsToLocalReturnToOnly(t *testing.T) {
	const secret = "test-session-secret-for-github-oauth-returnto-12345"
	const email = "github-returnto@example.com"

	for returnTo, want := range map[string]string{
		"":                           "/",
		"/projects/p1/agents/a1":     "/projects/p1/agents/a1",
		"/projects/p1?tab=agents":    "/projects/p1?tab=agents",
		"https://evil.example/x":     "/",
		"//evil.example/x":           "/",
		"/\\evil.example/x":          "/",
		"\\\\evil.example/x":         "/",
		"/\t/evil.example/x":         "/",
		"evil.example/x":             "/",
		"/%2F%2Fevil.example/x":      "/%2F%2Fevil.example/x",
		"%2F%2Fevil.example/x":       "/",
		"/%5Cevil.example":           "/%5Cevil.example",
		"/ok%0d%0aSet-Cookie:%20x=1": "/ok%0d%0aSet-Cookie:%20x=1",
		"/ok\r\nSet-Cookie: x=1":     "/",
		"/a/..//evil.example":        "/evil.example",
		"//user@evil.example/x":      "/",
		" //evil.example/x":          "/",
		"/\x7f/evil.example/x":       "/",
		// net/http.Redirect escapes non-ASCII bytes in lowercase hex.
		"/\uFF0Fevil.example": "/%ef%bc%8fevil.example",
		"/\u2215evil.example": "/%e2%88%95evil.example",
		"/\u2044evil.example": "/%e2%81%84evil.example",
		"/\uFF3Cevil.example": "/%ef%bc%bcevil.example",
	} {
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
				tokenJSON:  `{"access_token":"mock-token","token_type":"bearer","scope":"read:user,user:email"}`,
				userJSON:   `{"id":43,"login":"octocat2","name":"GitHub User","email":"` + email + `","avatar_url":"https://example.com/a.png"}`,
				emailsJSON: `[{"email":"` + email + `","primary":true,"verified":true}]`,
			},
		}
		ws.store = newProxyAuthStoreWithRoles()

		reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/github", nil)
		recSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyOAuthState] = "test-state-returnto"
		if returnTo != "" {
			sess.Values[sessKeyReturnTo] = returnTo
		}
		require.NoError(t, sess.Save(reqSetup, recSetup))

		reqCallback := httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=test-code&state=test-state-returnto", nil)
		for _, c := range recSetup.Result().Cookies() {
			reqCallback.AddCookie(c)
		}
		recCallback := httptest.NewRecorder()
		ws.Handler().ServeHTTP(recCallback, reqCallback)

		require.Equal(t, http.StatusFound, recCallback.Code, returnTo)
		assert.Equal(t, want, recCallback.Header().Get("Location"), returnTo)
	}
}

// A signed-out page deep link with a query lands on that path and query
// after the OAuth callback.
func TestOAuthCallback_PageDeepLinkReturnsWithQuery(t *testing.T) {
	const email = "github-deeplink@example.com"
	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: "test-session-secret-for-github-oauth-deeplink-1234",
		BaseURL:       "http://localhost:8080",
	})
	configureWebGitHubOAuth(ws)
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockGitHubOAuthTransport{
			tokenJSON:  `{"access_token":"mock-token","token_type":"bearer","scope":"read:user,user:email"}`,
			userJSON:   `{"id":44,"login":"octocat3","name":"GitHub User","email":"` + email + `","avatar_url":"https://example.com/a.png"}`,
			emailsJSON: `[{"email":"` + email + `","primary":true,"verified":true}]`,
		},
	}
	ws.store = newProxyAuthStoreWithRoles()
	handler := ws.Handler()

	page := httptest.NewRequest(http.MethodGet, "/projects/p1?tab=agents", nil)
	page.Header.Set("Accept", "text/html")
	recPage := httptest.NewRecorder()
	handler.ServeHTTP(recPage, page)
	require.Equal(t, http.StatusFound, recPage.Code)
	require.Equal(t, "/auth/login", recPage.Header().Get("Location"))
	pageCookie := sessionCookieOf(recPage)
	require.NotNil(t, pageCookie)

	// The provider round trip sets the OAuth state in the same session.
	reqState := withCookies(httptest.NewRequest(http.MethodGet, "/auth/login/github", nil), pageCookie)
	recState := httptest.NewRecorder()
	sess, err := ws.sessionStore.Get(reqState, webSessionName)
	require.NoError(t, err)
	require.Equal(t, "/projects/p1?tab=agents", sess.Values[sessKeyReturnTo])
	sess.Values[sessKeyOAuthState] = "test-state-deeplink"
	require.NoError(t, sess.Save(reqState, recState))
	stateCookie := sessionCookieOf(recState)
	require.NotNil(t, stateCookie)

	callback := withCookies(httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=test-code&state=test-state-deeplink", nil), stateCookie)
	recCallback := httptest.NewRecorder()
	handler.ServeHTTP(recCallback, callback)
	require.Equal(t, http.StatusFound, recCallback.Code, recCallback.Body.String())
	assert.Equal(t, "/projects/p1?tab=agents", recCallback.Header().Get("Location"))
}

// The error code from the provider reaches the sign-in page as one
// query value.
func TestOAuthCallback_ProviderErrorIsOneQueryValue(t *testing.T) {
	for errParam, want := range map[string]string{
		"access_denied": "/login?error=access_denied",
		"access_denied&returnTo=https://evil.example/x": "/login?error=access_denied%26returnTo%3Dhttps%3A%2F%2Fevil.example%2Fx",
	} {
		ws := newTestWebServer(t, WebServerConfig{
			SessionSecret: "test-session-secret-for-github-oauth-error-123456",
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
		ws.store = newProxyAuthStoreWithRoles()

		reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/github", nil)
		recSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyOAuthState] = "test-state-error"
		require.NoError(t, sess.Save(reqSetup, recSetup))

		reqCallback := httptest.NewRequest(http.MethodGet, "/auth/callback/github?state=test-state-error&error="+url.QueryEscape(errParam), nil)
		for _, c := range recSetup.Result().Cookies() {
			reqCallback.AddCookie(c)
		}
		recCallback := httptest.NewRecorder()
		ws.Handler().ServeHTTP(recCallback, reqCallback)

		require.Equal(t, http.StatusFound, recCallback.Code, errParam)
		assert.Equal(t, want, recCallback.Header().Get("Location"), errParam)
	}
}

// returnPathEnv is a web server with GitHub web sign-in whose log is
// captured, for the post-sign-in return path tests.
type returnPathEnv struct {
	ws      *WebServer
	handler http.Handler
	logs    *strings.Builder
}

func newReturnPathEnv(t *testing.T) *returnPathEnv {
	t.Helper()
	const email = "github-return-path@example.com"
	ws := newTestWebServer(t, WebServerConfig{
		SessionSecret: "test-session-secret-for-github-oauth-return-path-1",
		BaseURL:       "http://localhost:8080",
	})
	logs := &strings.Builder{}
	ws.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// The web component logs every request path at debug level; pin info so
	// the captured log holds only what these tests produce, whatever ran
	// before them.
	require.True(t, loglevel.Apply(loglevel.Spec{Default: slog.LevelInfo}, loglevel.SourceFlag))
	t.Cleanup(func() { loglevel.Reset(false) })
	configureWebGitHubOAuth(ws)
	ws.oauthService.httpClient = &http.Client{
		Transport: &mockGitHubOAuthTransport{
			tokenJSON:  `{"access_token":"mock-token","token_type":"bearer","scope":"read:user,user:email"}`,
			userJSON:   `{"id":45,"login":"octocat4","name":"GitHub User","email":"` + email + `","avatar_url":"https://example.com/a.png"}`,
			emailsJSON: `[{"email":"` + email + `","primary":true,"verified":true}]`,
		},
	}
	ws.store = newProxyAuthStoreWithRoles()
	return &returnPathEnv{ws: ws, handler: ws.Handler(), logs: logs}
}

// returnTo reads the stored return path from the session cookie in rec.
func (e *returnPathEnv) returnTo(t *testing.T, rec *httptest.ResponseRecorder) (string, bool) {
	t.Helper()
	c := sessionCookieOf(rec)
	require.NotNil(t, c, "a session cookie is set")
	sess, err := e.ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), c), webSessionName)
	require.NoError(t, err)
	v, ok := sess.Values[sessKeyReturnTo].(string)
	return v, ok
}

// signIn completes a sign-in from the session cookie in rec and returns
// the post-sign-in redirect location.
func (e *returnPathEnv) signIn(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	c := sessionCookieOf(rec)
	require.NotNil(t, c, "a session cookie is set")
	reqState := withCookies(httptest.NewRequest(http.MethodGet, "/auth/login/github", nil), c)
	recState := httptest.NewRecorder()
	sess, err := e.ws.sessionStore.Get(reqState, webSessionName)
	require.NoError(t, err)
	sess.Values[sessKeyOAuthState] = "test-state-return-path"
	require.NoError(t, sess.Save(reqState, recState))

	callback := withCookies(httptest.NewRequest(http.MethodGet, "/auth/callback/github?code=test-code&state=test-state-return-path", nil), sessionCookieOf(recState))
	recCallback := httptest.NewRecorder()
	e.handler.ServeHTTP(recCallback, callback)
	require.Equal(t, http.StatusFound, recCallback.Code, recCallback.Body.String())
	return recCallback.Header().Get("Location")
}

// assertLogsHideValues checks that the captured log holds none of the
// given values (query content, cookie values).
func (e *returnPathEnv) assertLogsHideValues(t *testing.T, values ...string) {
	t.Helper()
	for _, v := range values {
		assert.NotContains(t, e.logs.String(), v)
	}
}

const (
	// overflowQueryMarker is a made-up query value; repeated, it makes the
	// return path too large for the session cookie.
	overflowQueryMarker    = "zq7marker"
	saveReturnToTrimmedLog = "Sign-in return path stored without its query; the session could not be saved with the full path"
	saveReturnToDroppedLog = "Sign-in return path dropped; the session could not be saved with it, so sign-in returns to /"
)

// assertOneSaveLog checks that the captured log holds exactly one return
// path line, at WARN, with want as its message and pathLen as path_len.
func (e *returnPathEnv) assertOneSaveLog(t *testing.T, want string, pathLen int) {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(l, "Sign-in return path") {
			lines = append(lines, l)
		}
	}
	require.Len(t, lines, 1, e.logs.String())
	assert.Contains(t, lines[0], "level=WARN")
	assert.Contains(t, lines[0], want)
	assert.Contains(t, lines[0], "path_len="+strconv.Itoa(pathLen))
	assert.NotContains(t, e.logs.String(), "level=ERROR")
}

// assertSaveLogHidesValues checks that at least one return path log line
// exists and that the return path log lines hold none of the given values.
func (e *returnPathEnv) assertSaveLogHidesValues(t *testing.T, values ...string) {
	t.Helper()
	n := 0
	for _, l := range strings.Split(e.logs.String(), "\n") {
		if !strings.Contains(l, "Sign-in return path") {
			continue
		}
		n++
		for _, v := range values {
			assert.NotContains(t, l, v)
		}
	}
	require.GreaterOrEqual(t, n, 1, "no return path log line to check")
}

func overflowQuery() string {
	return "filter=" + strings.Repeat(overflowQueryMarker, 600)
}

// A signed-out page deep link returns to its path and query after
// sign-in when the session holds it, and to its path without the query
// when the query makes the session cookie too large. The save error is
// logged with the path length only.
func TestOAuthCallback_PageDeepLinkReturnPathFitsSession(t *testing.T) {
	for _, tc := range []struct {
		name, target, want string
		logged             bool
	}{
		{name: "normal query", target: "/projects/p1?tab=agents", want: "/projects/p1?tab=agents"},
		{name: "overflowing query", target: "/projects/p1?" + overflowQuery(), want: "/projects/p1", logged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newReturnPathEnv(t)
			page := httptest.NewRequest(http.MethodGet, tc.target, nil)
			page.Header.Set("Accept", "text/html")
			recPage := httptest.NewRecorder()
			e.handler.ServeHTTP(recPage, page)
			require.Equal(t, http.StatusFound, recPage.Code)
			require.Equal(t, "/auth/login", recPage.Header().Get("Location"))
			require.Len(t, recPage.Result().Cookies(), 1, "one session cookie is set")

			got, ok := e.returnTo(t, recPage)
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want, e.signIn(t, recPage))

			if !tc.logged {
				assert.NotContains(t, e.logs.String(), "Sign-in return path")
				return
			}
			e.assertOneSaveLog(t, saveReturnToTrimmedLog, len(tc.target))
			e.assertLogsHideValues(t, overflowQueryMarker, "filter=", sessionCookieOf(recPage).Value)
		})
	}
}

// A deep link whose path alone is too large for the session cookie
// stores no return path: the save error is logged, the session is saved
// without a return path, and sign-in lands on /.
func TestOAuthCallback_OverflowingDeepLinkPathReturnsToRoot(t *testing.T) {
	e := newReturnPathEnv(t)
	target := "/projects/" + strings.Repeat(overflowQueryMarker, 600) + "?" + overflowQuery()
	page := httptest.NewRequest(http.MethodGet, target, nil)
	page.Header.Set("Accept", "text/html")
	recPage := httptest.NewRecorder()
	e.handler.ServeHTTP(recPage, page)
	require.Equal(t, http.StatusFound, recPage.Code)
	assert.Equal(t, "/auth/login", recPage.Header().Get("Location"))
	require.Len(t, recPage.Result().Cookies(), 1, "the session is saved without a return path")
	_, ok := e.returnTo(t, recPage)
	assert.False(t, ok, "no return path is stored")
	assert.Equal(t, "/", e.signIn(t, recPage))

	e.assertOneSaveLog(t, saveReturnToDroppedLog, len(target))
	e.assertSaveLogHidesValues(t, overflowQueryMarker, "filter=")
	e.assertLogsHideValues(t, "filter=")
}

// A session that holds a return path from an earlier signed-out request,
// followed by a deep link too large for the session cookie, lands on /
// after sign-in, not on the earlier path.
func TestOAuthCallback_OverflowingDeepLinkClearsEarlierReturnPath(t *testing.T) {
	e := newReturnPathEnv(t)
	first := httptest.NewRequest(http.MethodGet, "/projects/earlier-page", nil)
	first.Header.Set("Accept", "text/html")
	recFirst := httptest.NewRecorder()
	e.handler.ServeHTTP(recFirst, first)
	earlier, ok := e.returnTo(t, recFirst)
	require.True(t, ok)
	require.Equal(t, "/projects/earlier-page", earlier)

	target := "/projects/" + strings.Repeat(overflowQueryMarker, 600)
	page := withCookies(httptest.NewRequest(http.MethodGet, target, nil), sessionCookieOf(recFirst))
	page.Header.Set("Accept", "text/html")
	recPage := httptest.NewRecorder()
	e.handler.ServeHTTP(recPage, page)
	require.Equal(t, http.StatusFound, recPage.Code)
	require.Len(t, recPage.Result().Cookies(), 1, "the session is saved without a return path")
	_, ok = e.returnTo(t, recPage)
	assert.False(t, ok, "the earlier return path is cleared")
	assert.Equal(t, "/", e.signIn(t, recPage))
	e.assertOneSaveLog(t, saveReturnToDroppedLog, len(target))
}

// A browser request to a proxy route with an overflowing query stores the
// route path without its query as the return path.
func TestSessionToBearerMiddleware_ProxyRedirectOverflowingQuery(t *testing.T) {
	e := newReturnPathEnv(t)
	e.ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the Hub handler is not reached for a redirected request")
	}), func(ctx context.Context) error { return nil })
	handler := e.ws.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent123/ports/8080/proxy/index.html?"+overflowQuery(), nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/auth/login", rec.Header().Get("Location"))

	got, ok := e.returnTo(t, rec)
	require.True(t, ok)
	assert.Equal(t, "/api/v1/agents/agent123/ports/8080/proxy/index.html", got)
	e.assertOneSaveLog(t, saveReturnToTrimmedLog, len(req.URL.RequestURI()))
	e.assertLogsHideValues(t, overflowQueryMarker, "filter=", sessionCookieOf(rec).Value)
}

// The returnTo the sign-in page passes on, to /auth/login/ and to a
// provider, is stored without its query when the query makes the session
// cookie too large; the provider sign-in proceeds with its OAuth state.
func TestOAuthLogin_ReturnToFitsSession(t *testing.T) {
	for _, tc := range []struct {
		name, returnTo, want string
	}{
		{name: "normal query", returnTo: "/projects/p1?tab=agents", want: "/projects/p1?tab=agents"},
		{name: "overflowing query", returnTo: "/projects/p1?" + overflowQuery(), want: "/projects/p1"},
		{name: "overflowing path", returnTo: "/projects/" + strings.Repeat(overflowQueryMarker, 600)},
	} {
		t.Run(tc.name+"/login page", func(t *testing.T) {
			e := newReturnPathEnv(t)
			rec := httptest.NewRecorder()
			// The handler serves /auth/login/ (the mux redirects /auth/login there).
			e.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login/?"+url.Values{"returnTo": {tc.returnTo}}.Encode(), nil))
			require.Equal(t, http.StatusFound, rec.Code)
			assert.Equal(t, "/login", rec.Header().Get("Location"))
			if tc.want == "" {
				require.Len(t, rec.Result().Cookies(), 1, "the session is saved without a return path")
				_, ok := e.returnTo(t, rec)
				assert.False(t, ok, "no return path is stored")
				e.assertOneSaveLog(t, saveReturnToDroppedLog, len(tc.returnTo))
			} else {
				got, ok := e.returnTo(t, rec)
				require.True(t, ok)
				assert.Equal(t, tc.want, got)
			}
			e.assertLogsHideValues(t, overflowQueryMarker, "filter=")
		})
		t.Run(tc.name+"/provider", func(t *testing.T) {
			e := newReturnPathEnv(t)
			rec := httptest.NewRecorder()
			e.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login/github?"+url.Values{"returnTo": {tc.returnTo}}.Encode(), nil))
			require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
			assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "https://github.com/"), rec.Header().Get("Location"))
			require.Len(t, rec.Result().Cookies(), 1, "one session cookie is set")

			sess, err := e.ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), sessionCookieOf(rec)), webSessionName)
			require.NoError(t, err)
			assert.NotEmpty(t, sess.Values[sessKeyOAuthState], "the OAuth state is saved")
			got, ok := sess.Values[sessKeyReturnTo].(string)
			if tc.want == "" {
				assert.False(t, ok, "no return path is stored")
				e.assertOneSaveLog(t, saveReturnToDroppedLog, len(tc.returnTo))
			} else {
				require.True(t, ok)
				assert.Equal(t, tc.want, got)
			}
			e.assertLogsHideValues(t, overflowQueryMarker, "filter=", sessionCookieOf(rec).Value)
		})
	}
}

// saveReturnTo checks the path without its query with localRedirectPath
// before storing it: a value that is not a path on this server is dropped,
// even when a caller passes one, and sign-in returns to /.
func TestSaveReturnTo_RevalidatesShortenedPath(t *testing.T) {
	for _, next := range []string{
		"//evil.example/x?" + overflowQuery(),
		"/\\evil.example/x?" + overflowQuery(),
		"/?" + overflowQuery(),
	} {
		e := newReturnPathEnv(t)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		sess, err := e.ws.sessionStore.New(req, webSessionName)
		require.NoError(t, err)

		require.NoError(t, e.ws.saveReturnTo(rec, req, sess, next))
		_, stored := sess.Values[sessKeyReturnTo]
		assert.False(t, stored, "no return path is stored")
		require.Len(t, rec.Result().Cookies(), 1, "the session is saved without a return path")
		_, ok := e.returnTo(t, rec)
		assert.False(t, ok, "the saved session holds no return path")
		e.assertOneSaveLog(t, saveReturnToDroppedLog, len(next))
		e.assertLogsHideValues(t, overflowQueryMarker, "evil.example")
	}
}

// A session that cannot be saved even without the return path returns the
// error, sets no cookie, and logs one Warn line only.
func TestSaveReturnTo_UnsavableSessionLogsOnce(t *testing.T) {
	e := newReturnPathEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	sess, err := e.ws.sessionStore.New(req, webSessionName)
	require.NoError(t, err)
	// A made-up value too large for the cookie on its own.
	sess.Values["test-padding"] = strings.Repeat(overflowQueryMarker, 600)

	next := "/projects/p1?tab=agents"
	require.Error(t, e.ws.saveReturnTo(rec, req, sess, next))
	_, stored := sess.Values[sessKeyReturnTo]
	assert.False(t, stored, "no return path is stored")
	assert.Empty(t, rec.Result().Cookies(), "no session is saved")
	e.assertOneSaveLog(t, saveReturnToDroppedLog, len(next))
	e.assertLogsHideValues(t, overflowQueryMarker, "tab=agents")
}
