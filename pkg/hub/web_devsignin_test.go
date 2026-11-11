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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// devSignInEnv is a dev-auth web server with a token service and a mock hub
// that records the Authorization and agent token headers it receives.
type devSignInEnv struct {
	ws      *WebServer
	handler http.Handler
	logs    *streamLogCapture

	mu        sync.Mutex
	hubCalls  int
	hubAuth   string
	hubAgent  string
	hubCookie string
}

func newDevSignInEnv(t *testing.T, overrides ...func(*WebServerConfig)) *devSignInEnv {
	t.Helper()
	ws := newDevAuthWebServer(t, overrides...)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	e := &devSignInEnv{ws: ws, logs: &streamLogCapture{}}
	ws.log = slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.hubCalls++
		e.hubAuth = r.Header.Get("Authorization")
		e.hubAgent = r.Header.Get("X-Scion-Agent-Token")
		e.hubCookie = r.Header.Get("Cookie")
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}), func(context.Context) error { return nil })
	e.handler = ws.Handler()
	return e
}

func (e *devSignInEnv) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// mint asks for a sign-in code with the given Authorization header value.
func (e *devSignInEnv) mint(t *testing.T, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, devLaunchPath, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return e.serve(req)
}

// mintCode mints a code with the dev token and returns the launch answer.
func (e *devSignInEnv) mintCode(t *testing.T) devLaunchResponse {
	t.Helper()
	rec := e.mint(t, "Bearer "+e.ws.config.DevAuthToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var resp devLaunchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func (e *devSignInEnv) signIn(path string) *httptest.ResponseRecorder {
	return e.serve(httptest.NewRequest(http.MethodGet, path, nil))
}

func (e *devSignInEnv) lastHub() (calls int, auth, agent, cookie string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hubCalls, e.hubAuth, e.hubAgent, e.hubCookie
}

func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == webSessionName {
			return c
		}
	}
	return nil
}

func withCookies(req *http.Request, cookies ...*http.Cookie) *http.Request {
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}

// A request with no session and no sign-in code gets no session: a page
// goes to the login page, /auth/me answers 401, and /api/v1 reaches the hub
// with no Authorization header. No session cookie is set on any of them,
// whatever Host the request names.
func TestDevAuth_NoSessionNoCodeGetsNoSession(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "host.docker.internal:8080"} {
		t.Run(host, func(t *testing.T) {
			e := newDevSignInEnv(t)

			page := httptest.NewRequest(http.MethodGet, "/projects", nil)
			page.Host = host
			page.Header.Set("Accept", "text/html")
			rec := e.serve(page)
			assert.Equal(t, http.StatusFound, rec.Code)
			assert.Equal(t, "/auth/login", rec.Header().Get("Location"), "the normal login path")
			assert.NotContains(t, rec.Body.String(), "__SCION_DATA__")
			assert.Nil(t, sessionCookieOf(rec), "no session cookie for a page")

			me := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
			me.Host = host
			rec = e.serve(me)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.NotContains(t, rec.Body.String(), DevUserID)
			assert.Nil(t, sessionCookieOf(rec), "no session cookie for /auth/me")

			api := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
			api.Host = host
			rec = e.serve(api)
			calls, auth, _, _ := e.lastHub()
			require.Equal(t, 1, calls)
			assert.Empty(t, auth, "no Authorization forwarded to the hub API")
			assert.Nil(t, sessionCookieOf(rec), "no session cookie for /api/v1")
			assert.Empty(t, rec.Result().Cookies())
		})
	}
}

// A valid code gives exactly one session: the link sets an HttpOnly,
// SameSite=Strict session cookie and redirects; the session then reaches
// the hub API as the dev user. The same code cannot be used again.
func TestDevSignIn_ValidCodeOnceThenRefused(t *testing.T) {
	e := newDevSignInEnv(t)
	launch := e.mintCode(t)
	require.NotEmpty(t, launch.Code)
	assert.Equal(t, devSignInPath+"?code="+url.QueryEscape(launch.Code), launch.SignInPath)
	assert.WithinDuration(t, time.Now().Add(devSignInCodeTTL), launch.ExpiresAt, 5*time.Second)

	rec := e.signIn(launch.SignInPath)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	assert.Equal(t, "/", rec.Header().Get("Location"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	sess := sessionCookieOf(rec)
	require.NotNil(t, sess, "the link sets the session cookie")
	assert.True(t, sess.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, sess.SameSite)
	assert.Equal(t, "/", sess.Path)

	rec = e.serve(withCookies(httptest.NewRequest(http.MethodGet, "/auth/me", nil), sess))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var me webSessionUser
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, DevUserID, me.UserID)
	assert.Equal(t, "admin", me.Role)

	e.serve(withCookies(httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil), sess))
	_, auth, _, _ := e.lastHub()
	require.True(t, strings.HasPrefix(auth, "Bearer "), "the session reaches the hub API with its token")
	claims, err := e.ws.userTokenSvc.ValidateUserToken(strings.TrimPrefix(auth, "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, DevUserID, claims.UserID)

	again := e.signIn(launch.SignInPath)
	assert.Equal(t, http.StatusUnauthorized, again.Code)
	assert.Contains(t, again.Body.String(), "already used or it has expired")
	assert.Nil(t, sessionCookieOf(again), "a used code sets no cookie")
}

// An expired code is refused and sets no cookie, and it is gone afterwards.
func TestDevSignIn_ExpiredCodeRefused(t *testing.T) {
	e := newDevSignInEnv(t)
	now := time.Now()
	e.ws.devSignIn.now = func() time.Time { return now }
	launch := e.mintCode(t)

	now = now.Add(devSignInCodeTTL)
	rec := e.signIn(launch.SignInPath)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Nil(t, sessionCookieOf(rec))

	now = now.Add(-time.Minute) // even back inside the window: removed on use
	rec = e.signIn(launch.SignInPath)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Nil(t, sessionCookieOf(rec))
}

// A missing, unknown or tampered code is refused with no cookie.
func TestDevSignIn_UnknownCodesRefused(t *testing.T) {
	e := newDevSignInEnv(t)
	launch := e.mintCode(t)
	for _, path := range []string{
		devSignInPath,
		devSignInPath + "?code=",
		devSignInPath + "?code=not-a-code",
		devSignInPath + "?code=" + url.QueryEscape(launch.Code+"x"),
	} {
		rec := e.signIn(path)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
		assert.Nil(t, sessionCookieOf(rec), path)
	}
	// The real code still works once.
	assert.Equal(t, http.StatusSeeOther, e.signIn(launch.SignInPath).Code)
}

// A navigation that arrives without the SameSite=Strict dev cookie (a link
// followed from another site) is sent to the login path with no session
// cookie, so the browser keeps its dev session: the original cookie still
// works afterwards. The same holds for a browser request to an agent port
// proxy route, which redirects to login from the hub API side.
func TestDevSignIn_CrossSiteNavigationKeepsSession(t *testing.T) {
	e := newDevSignInEnv(t)
	sess := sessionCookieOf(e.signIn(e.mintCode(t).SignInPath))
	require.NotNil(t, sess)

	for _, path := range []string{"/projects/p1/agents/a1", "/api/v1/agents/a1/ports/8080/proxy/"} {
		nav := httptest.NewRequest(http.MethodGet, path, nil)
		nav.Header.Set("Accept", "text/html")
		nav.Header.Set("Sec-Fetch-Site", "cross-site")
		nav.Header.Set("Sec-Fetch-Mode", "navigate")
		rec := e.serve(nav)
		assert.Equal(t, http.StatusFound, rec.Code, path)
		assert.Equal(t, "/auth/login", rec.Header().Get("Location"), path)
		assert.Nil(t, sessionCookieOf(rec), "%s: no session cookie that would replace the dev session", path)
	}

	rec := e.serve(withCookies(httptest.NewRequest(http.MethodGet, "/auth/me", nil), sess))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var me webSessionUser
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, DevUserID, me.UserID)
}

// Without dev auth the login redirect still keeps the return path in the
// session cookie.
func TestLoginRedirect_KeepsReturnPathWithoutDevAuth(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})
	page := httptest.NewRequest(http.MethodGet, "/projects", nil)
	page.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, page)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.NotNil(t, sessionCookieOf(rec), "the return path cookie is kept outside dev sign-in")
}

// A HEAD request (a link prefetcher or unfurler) does not use up the code.
func TestDevSignIn_HeadDoesNotUseCode(t *testing.T) {
	e := newDevSignInEnv(t)
	launch := e.mintCode(t)
	rec := e.serve(httptest.NewRequest(http.MethodHead, launch.SignInPath, nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Nil(t, sessionCookieOf(rec))
	assert.Equal(t, http.StatusSeeOther, e.signIn(launch.SignInPath).Code, "the code still works after a HEAD")
}

// An existing session cookie keeps working without a new code, on pages,
// /auth/me and the hub API.
func TestDevSignIn_ExistingSessionStillWorks(t *testing.T) {
	e := newDevSignInEnv(t)
	sess := sessionCookieOf(e.signIn(e.mintCode(t).SignInPath))
	require.NotNil(t, sess)

	for i := 0; i < 3; i++ {
		page := withCookies(httptest.NewRequest(http.MethodGet, "/projects", nil), sess)
		page.Header.Set("Accept", "text/html")
		rec := e.serve(page)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "__SCION_DATA__")

		rec = e.serve(withCookies(httptest.NewRequest(http.MethodGet, "/auth/me", nil), sess))
		assert.Equal(t, http.StatusOK, rec.Code)

		e.serve(withCookies(httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil), sess))
		_, auth, _, _ := e.lastHub()
		assert.True(t, strings.HasPrefix(auth, "Bearer "))
	}
}

// Requests that carry their own credential pass through the web listener
// unchanged and get no session: an agent token header reaches the hub API
// with no Authorization added, and a Bearer token reaches it as sent.
func TestDevAuth_CredentialRequestsPassWithoutSession(t *testing.T) {
	e := newDevSignInEnv(t)

	agentReq := httptest.NewRequest(http.MethodGet, "/api/v1/agents/a1", nil)
	agentReq.Header.Set("X-Scion-Agent-Token", "agent-token-value")
	rec := e.serve(agentReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls, auth, agent, cookie := e.lastHub()
	assert.Equal(t, 1, calls)
	assert.Equal(t, "agent-token-value", agent)
	assert.Empty(t, auth, "no Authorization added to an agent token request")
	assert.Empty(t, cookie)
	assert.Empty(t, rec.Result().Cookies(), "no cookie set for an agent token request")

	bearerReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	bearerReq.Header.Set("Authorization", "Bearer some-hub-token")
	rec = e.serve(bearerReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls, auth, _, _ = e.lastHub()
	assert.Equal(t, 2, calls)
	assert.Equal(t, "Bearer some-hub-token", auth, "the Bearer token is passed through as sent")
	assert.Empty(t, rec.Result().Cookies(), "no cookie set for a Bearer request")

	devReq := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	devReq.Header.Set("Authorization", "Bearer "+e.ws.config.DevAuthToken)
	rec = e.serve(devReq)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Result().Cookies(), "no cookie set for a dev token request")
}

// Minting a code needs the dev token as a Bearer, on POST.
func TestDevLaunch_RequiresDevToken(t *testing.T) {
	e := newDevSignInEnv(t)
	for name, authz := range map[string]string{
		"none":         "",
		"wrong token":  "Bearer not-the-dev-token",
		"not bearer":   "Basic " + e.ws.config.DevAuthToken,
		"token prefix": "Bearer " + e.ws.config.DevAuthToken[:len(e.ws.config.DevAuthToken)-1],
	} {
		rec := e.mint(t, authz)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.NotContains(t, rec.Body.String(), "signInPath", name)
		assert.Empty(t, rec.Result().Cookies(), name)
	}

	get := httptest.NewRequest(http.MethodGet, devLaunchPath, nil)
	get.Header.Set("Authorization", "Bearer "+e.ws.config.DevAuthToken)
	assert.Equal(t, http.StatusMethodNotAllowed, e.serve(get).Code)

	// A session cookie is not a substitute for the dev token.
	sess := sessionCookieOf(e.signIn(e.mintCode(t).SignInPath))
	require.NotNil(t, sess)
	rec := e.serve(withCookies(httptest.NewRequest(http.MethodPost, devLaunchPath, nil), sess))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// Pending codes are bounded; used and expired codes free their place.
func TestDevLaunch_PendingCodesBounded(t *testing.T) {
	e := newDevSignInEnv(t)
	now := time.Now()
	e.ws.devSignIn.now = func() time.Time { return now }
	var last devLaunchResponse
	for i := 0; i < devSignInMaxPending; i++ {
		last = e.mintCode(t)
	}
	rec := e.mint(t, "Bearer "+e.ws.config.DevAuthToken)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)

	require.Equal(t, http.StatusSeeOther, e.signIn(last.SignInPath).Code)
	e.mintCode(t)

	now = now.Add(devSignInCodeTTL)
	for i := 0; i < devSignInMaxPending; i++ {
		e.mintCode(t)
	}
}

// Without dev auth neither route exists and nothing signs a browser in.
func TestDevSignIn_NotServedWithoutDevAuth(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})
	assert.Nil(t, ws.devSignIn)
	h := ws.Handler()

	mint := httptest.NewRequest(http.MethodPost, devLaunchPath, nil)
	mint.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mint)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, devSignInPath+"?code=x", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Result().Cookies())

	page := httptest.NewRequest(http.MethodGet, "/projects", nil)
	page.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, page)
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/auth/login", rec.Header().Get("Location"))

	// Session cookies keep the store's defaults outside dev auth.
	cookies := loginSession(t, ws, "user-1", "u@example.com", "member")
	require.NotEmpty(t, cookies)
	assert.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
}

// The sign-in link lands on a local path only.
func TestDevSignIn_NextIsLocalOnly(t *testing.T) {
	e := newDevSignInEnv(t)
	launch := e.mintCode(t)
	rec := e.signIn(launch.SignInPath + "&next=" + url.QueryEscape("/onboarding"))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/onboarding", rec.Header().Get("Location"))

	launch = e.mintCode(t)
	rec = e.signIn(launch.SignInPath + "&next=" + url.QueryEscape("https://evil.example/"))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/", rec.Header().Get("Location"))
}

func TestLocalRedirectPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "/",
		"/":                          "/",
		"/onboarding":                "/onboarding",
		"/projects/p1?tab=agents":    "/projects/p1?tab=agents",
		"//evil.example/":            "/",
		"/\\evil.example":            "/",
		"https://evil.example/":      "/",
		"evil.example":               "/",
		"javascript:alert(1)":        "/",
		"/ok\r\nSet-Cookie: x=1":     "/",
		devSignInPath + "?code=abc":  "/",
		devLaunchPath:                "/",
		"\\evil.example":             "/",
		"/\t/evil.example":           "/",
		"/ok\x00":                    "/",
		"https:/evil.example":        "/",
		"/%2F%2Fevil.example/x":      "/%2F%2Fevil.example/x",
		"%2F%2Fevil.example/x":       "/",
		"/%5Cevil.example":           "/%5Cevil.example",
		"/ok%0d%0aSet-Cookie:%20x=1": "/ok%0d%0aSet-Cookie:%20x=1",
		"//user@evil.example/":       "/",
		" /evil.example":             "/",
		" //evil.example":            "/",
		"/\x7f/evil.example":         "/",
		"/\uFF0Fevil.example":        "/\uFF0Fevil.example",
		"/\u2215evil.example":        "/\u2215evil.example",
		"/\u2044evil.example":        "/\u2044evil.example",
		"/\uFF3Cevil.example":        "/\uFF3Cevil.example",
	} {
		assert.Equal(t, want, localRedirectPath(in), in)
	}
}

// Signing in records the dev user's last login; using the session does
// not. A failed write does not stop the sign-in.
func TestDevSignIn_RecordsLastLogin(t *testing.T) {
	e := newDevSignInEnv(t)
	s := newFakeUserStore()
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: DevUserID, Email: "dev@localhost", Status: store.UserStatusActive, Role: "admin",
	}))
	e.ws.store = s

	before := time.Now()
	sess := sessionCookieOf(e.signIn(e.mintCode(t).SignInPath))
	require.NotNil(t, sess)
	user, err := s.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	assert.False(t, user.LastLogin.Before(before))
	assert.Equal(t, 1, s.lastLoginUpdateCalls)
	assert.Zero(t, s.updateCalls, "sign-in must not write the full user record")

	reuse := e.ws.devAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	reuse.ServeHTTP(rec, withCookies(httptest.NewRequest(http.MethodGet, "/", nil), sess))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, s.lastLoginUpdateCalls, "using a session is not a login")

	s.lastLoginUpdateErr = errors.New("last login write failed")
	rec = e.signIn(e.mintCode(t).SignInPath)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.NotNil(t, sessionCookieOf(rec))
}

// The code never appears in the web server's logs, for a used or a refused
// link.
func TestDevSignIn_CodeNotLogged(t *testing.T) {
	e := newDevSignInEnv(t)
	launch := e.mintCode(t)
	require.Equal(t, http.StatusSeeOther, e.signIn(launch.SignInPath).Code)
	require.Equal(t, http.StatusUnauthorized, e.signIn(launch.SignInPath).Code)

	e.logs.mu.Lock()
	logged := e.logs.buf.String()
	e.logs.mu.Unlock()
	assert.NotEmpty(t, logged, "the refusal is logged")
	assert.NotContains(t, logged, launch.Code)
}

// /auth/providers tells the login page that this server signs in from a
// dev sign-in link.
func TestAuthProviders_DevSignInFlag(t *testing.T) {
	read := func(ws *WebServer) map[string]any {
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body
	}
	assert.Equal(t, true, read(newDevAuthWebServer(t))["devSignIn"])
	_, present := read(newTestWebServer(t, WebServerConfig{}))["devSignIn"]
	assert.False(t, present)
}

// A failed last-login write is logged and does not stop the sign-in: the
// link still sets the dev session, the row's last login stays unset, and
// the full user record is never written.
func TestDevSignIn_LastLoginWriteFailure(t *testing.T) {
	e := newDevSignInEnv(t)
	s := newFakeUserStore()
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: DevUserID, Email: "dev@localhost", Status: store.UserStatusActive, Role: "admin",
	}))
	s.lastLoginUpdateErr = errors.New("last login write failed")
	e.ws.store = s

	rec := e.signIn(e.mintCode(t).SignInPath)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.NotNil(t, sessionCookieOf(rec), "sign-in succeeds despite the write failure")

	user, err := s.GetUser(context.Background(), DevUserID)
	require.NoError(t, err)
	assert.True(t, user.LastLogin.IsZero())
	assert.Equal(t, 1, s.lastLoginUpdateCalls)
	assert.Zero(t, s.updateCalls, "sign-in must not write the full user record")

	e.logs.mu.Lock()
	logged := e.logs.buf.String()
	e.logs.mu.Unlock()
	assert.Contains(t, logged, "Failed to update dev-auth last login", "the write failure is logged")
}

// configureWebGitHubOAuth gives ws a GitHub OAuth provider for web sign-in.
func configureWebGitHubOAuth(ws *WebServer) {
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			GitHub: OAuthProviderConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
		},
	}, nil)
}

// With dev sign-in and an OAuth provider both configured, a signed-out
// deep link goes to the sign-in page with the requested path as returnTo,
// and no session cookie is set. An existing dev session is served.
func TestDevAuthWithOAuth_SignedOutDeepLinkCarriesReturnPath(t *testing.T) {
	e := newDevSignInEnv(t)
	configureWebGitHubOAuth(e.ws)
	sess := sessionCookieOf(e.signIn(e.mintCode(t).SignInPath))
	require.NotNil(t, sess)

	for path, want := range map[string]string{
		"/projects/p1/agents/a1":                    "/login?returnTo=%2Fprojects%2Fp1%2Fagents%2Fa1",
		"/projects/p1?tab=agents":                   "/login?returnTo=%2Fprojects%2Fp1%3Ftab%3Dagents",
		"/api/v1/agents/a1/ports/8080/proxy/?tab=x": "/login?returnTo=%2Fapi%2Fv1%2Fagents%2Fa1%2Fports%2F8080%2Fproxy%2F%3Ftab%3Dx",
	} {
		nav := httptest.NewRequest(http.MethodGet, path, nil)
		nav.Header.Set("Accept", "text/html")
		rec := e.serve(nav)
		assert.Equal(t, http.StatusFound, rec.Code, path)
		assert.Equal(t, want, rec.Header().Get("Location"), path)
		assert.Empty(t, rec.Result().Cookies(), "%s: no cookie that would replace the dev session", path)
	}

	page := withCookies(httptest.NewRequest(http.MethodGet, "/projects", nil), sess)
	page.Header.Set("Accept", "text/html")
	rec := e.serve(page)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "__SCION_DATA__")

	rec = e.serve(withCookies(httptest.NewRequest(http.MethodGet, "/auth/me", nil), sess))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var me webSessionUser
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	assert.Equal(t, DevUserID, me.UserID)
}

// A signed-out page request is returned to after sign-in with its query:
// sessionAuthMiddleware stores the request URI (path and query) as the
// session's returnTo, and on a dev sign-in server without a session cookie
// carries it in /login?returnTo=. Both go through localRedirectPath, so a
// request URI that is not a plain path on this server ends at /.
func TestSessionAuthMiddleware_ReturnPathKeepsQuery(t *testing.T) {
	cases := map[string]string{
		"/projects/p1":                               "/projects/p1",
		"/projects/p1?tab=agents":                    "/projects/p1?tab=agents",
		"/projects/p1/agents/a1?tab=logs&q=a%2Fb":    "/projects/p1/agents/a1?tab=logs&q=a%2Fb",
		"/projects/p1?next=https://evil.example/x":   "/projects/p1?next=https://evil.example/x",
		"/projects/p1?x=%0d%0aSet-Cookie:%20a=1":     "/projects/p1?x=%0d%0aSet-Cookie:%20a=1",
		"http://evil.example/projects/p1?tab=agents": "/projects/p1?tab=agents",
		"/%2F%2Fevil.example/x?tab=a":                "/%2F%2Fevil.example/x?tab=a",
		"/projects/%2F%2Fevil.example?tab=a":         "/projects/%2F%2Fevil.example?tab=a",
		"/projects/%5Cevil.example?tab=a":            "/projects/%5Cevil.example?tab=a",
		"/ok%0d%0aSet-Cookie:%20a=1":                 "/ok%0d%0aSet-Cookie:%20a=1",
		"//evil.example/x?tab=a":                     "/",
		"/projects/p1?next=\\evil.example":           "/",
		"/projects/p1?next=/\\evil.example":          "/",
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("%s: protected handler reached without a session", r.URL.RequestURI())
	})

	t.Run("session returnTo", func(t *testing.T) {
		ws := newTestWebServer(t, WebServerConfig{SessionSecret: "test-session-secret-for-return-path-query-1234"})
		h := ws.sessionAuthMiddleware(next)
		for target, want := range cases {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Accept", "text/html")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusFound, rec.Code, target)
			assert.Equal(t, "/auth/login", rec.Header().Get("Location"), target)

			cookie := sessionCookieOf(rec)
			require.NotNil(t, cookie, target)
			sess, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), cookie), webSessionName)
			require.NoError(t, err, target)
			assert.Equal(t, want, sess.Values[sessKeyReturnTo], target)
		}
	})

	t.Run("dev sign-in login returnTo", func(t *testing.T) {
		ws := newDevAuthWebServer(t)
		configureWebGitHubOAuth(ws)
		h := ws.sessionAuthMiddleware(next)
		for target, want := range cases {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Accept", "text/html")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			wantLocation := "/login"
			if want != "/" {
				wantLocation = "/login?" + url.Values{"returnTo": {want}}.Encode()
			}
			require.Equal(t, http.StatusFound, rec.Code, target)
			assert.Equal(t, wantLocation, rec.Header().Get("Location"), target)
			assert.Nil(t, sessionCookieOf(rec), target)
		}
	})
}

// The returnTo carried in the redirect is a path on this server only; an
// off-site, absolute or sign-in path is never echoed. Without a web OAuth
// provider, or in proxy mode, the redirect is /auth/login.
func TestNoSessionLoginLocation(t *testing.T) {
	ws := newDevAuthWebServer(t)
	configureWebGitHubOAuth(ws)
	for in, want := range map[string]string{
		"/projects/p1":              "/login?returnTo=%2Fprojects%2Fp1",
		"/projects/p1?tab=agents":   "/login?returnTo=%2Fprojects%2Fp1%3Ftab%3Dagents",
		"/":                         "/login",
		"":                          "/login",
		"//evil.example/x":          "/login",
		"/\\evil.example":           "/login",
		"https://evil.example/x":    "/login",
		"evil.example":              "/login",
		"/ok\r\nSet-Cookie: x=1":    "/login",
		devSignInPath + "?code=abc": "/login",
	} {
		assert.Equal(t, want, ws.noSessionLoginLocation(in), in)
	}

	assert.Equal(t, "/auth/login", newDevAuthWebServer(t).noSessionLoginLocation("/projects/p1"), "no OAuth provider")

	proxy := newDevAuthWebServer(t, func(c *WebServerConfig) { c.AuthMode = "proxy" })
	configureWebGitHubOAuth(proxy)
	assert.Equal(t, "/auth/login", proxy.noSessionLoginLocation("/projects/p1"), "proxy mode")
}

// Picking a provider on the sign-in page stores its returnTo for the OAuth
// callback, limited to a path on this server.
func TestOAuthLogin_ProviderStoresLocalReturnTo(t *testing.T) {
	ws := newDevAuthWebServer(t)
	configureWebGitHubOAuth(ws)
	h := ws.Handler()

	for returnTo, want := range map[string]string{
		"/projects/p1/agents/a1":  "/projects/p1/agents/a1",
		"https://evil.example/x":  "/",
		"//evil.example/x":        "/",
		devSignInPath + "?code=a": "/",
	} {
		req := httptest.NewRequest(http.MethodGet, "/auth/login/github?returnTo="+url.QueryEscape(returnTo), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusFound, rec.Code, returnTo)
		assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "https://github.com/login/oauth/authorize?"), rec.Header().Get("Location"))

		cookie := sessionCookieOf(rec)
		require.NotNil(t, cookie, returnTo)
		sess, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), cookie), webSessionName)
		require.NoError(t, err)
		assert.Equal(t, want, sess.Values[sessKeyReturnTo], returnTo)
	}
}

// Outside dev sign-in, an OAuth server stores the return path in the
// session cookie and redirects to /auth/login. Only a path on this server
// is stored; any other value is stored as /.
func TestLoginRedirect_OAuthWithoutDevAuthUsesSessionReturnPath(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})
	configureWebGitHubOAuth(ws)
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), func(context.Context) error { return nil })
	h := ws.Handler()

	for path, want := range map[string]string{
		"/projects/p1/agents/a1":                                  "/projects/p1/agents/a1",
		"/api/v1/agents/a1/ports/8080/proxy/?tab=x":               "/api/v1/agents/a1/ports/8080/proxy/?tab=x",
		"//evil.example/x":                                        "/",
		"/api/v1/agents/a1/ports/8080/proxy/?next=\\evil.example": "/",
	} {
		page := httptest.NewRequest(http.MethodGet, path, nil)
		page.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, page)
		assert.Equal(t, http.StatusFound, rec.Code, path)
		assert.Equal(t, "/auth/login", rec.Header().Get("Location"), path)

		cookie := sessionCookieOf(rec)
		require.NotNil(t, cookie, path)
		sess, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), cookie), webSessionName)
		require.NoError(t, err)
		assert.Equal(t, want, sess.Values[sessKeyReturnTo], path)
	}
}

// /auth/login?returnTo= reaches the handler at /auth/login/ with its query,
// which stores the return path only when it is a path on this server, and
// / otherwise.
func TestOAuthLogin_ProviderlessStoresLocalReturnTo(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})
	h := ws.Handler()

	for returnTo, want := range map[string]string{
		"/projects/p1/agents/a1":  "/projects/p1/agents/a1",
		"/projects/p1?tab=agents": "/projects/p1?tab=agents",
		"https://evil.example/x":  "/",
		"//evil.example/x":        "/",
		"/\\evil.example/x":       "/",
		"/\t/evil.example/x":      "/",
		"evil.example/x":          "/",
	} {
		query := "?returnTo=" + url.QueryEscape(returnTo)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login"+query, nil))
		require.Equal(t, http.StatusTemporaryRedirect, rec.Code, returnTo)
		assert.Equal(t, "/auth/login/"+query, rec.Header().Get("Location"), returnTo)

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login/"+query, nil))
		require.Equal(t, http.StatusFound, rec.Code, returnTo)
		assert.Equal(t, "/login", rec.Header().Get("Location"), returnTo)

		cookie := sessionCookieOf(rec)
		require.NotNil(t, cookie, returnTo)
		sess, err := ws.sessionStore.Get(withCookies(httptest.NewRequest(http.MethodGet, "/", nil), cookie), webSessionName)
		require.NoError(t, err)
		assert.Equal(t, want, sess.Values[sessKeyReturnTo], returnTo)
	}
}
