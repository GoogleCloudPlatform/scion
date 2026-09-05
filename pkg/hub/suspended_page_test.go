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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Suspended-user middleware — OAuth / session auth tests
// ---------------------------------------------------------------------------

func TestSuspendedUserMiddleware_ActiveUser_PassesThrough(t *testing.T) {
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "active@example.com",
		Role:   "member",
		Status: "active",
	})

	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) {
		cfg.DevAuthToken = "" // disable dev-auth; we'll set session manually
	})
	ws.SetStore(st)
	handler := ws.Handler()

	// Establish session for the active user.
	cookies := loginSession(t, ws, "user-1", "active@example.com", "member")

	req := httptest.NewRequest("GET", "/projects", nil)
	req.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Active user should see the SPA shell.
	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "__SCION_DATA__", "active user should get SPA shell with prefetch data")
	assert.Contains(t, body, "main.js", "active user should get SPA entry script")
}

func TestSuspendedUserMiddleware_SuspendedUser_ServesPage(t *testing.T) {
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "suspended@example.com",
		Role:   "member",
		Status: "active",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	handler := ws.Handler()

	// Establish session while user is active.
	cookies := loginSession(t, ws, "user-1", "suspended@example.com", "member")

	// Admin suspends the user.
	u, _ := st.GetUser(context.Background(), "user-1")
	u.Status = "suspended"
	st.UpdateUser(context.Background(), u)

	// Next browser navigation should see the suspended page.
	req := httptest.NewRequest("GET", "/projects", nil)
	req.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Account Suspended")
	assert.Contains(t, body, "suspended@example.com")
	assert.Contains(t, body, "/auth/logout", "suspended page must have sign-out link")
	assert.NotContains(t, body, "__SCION_DATA__", "suspended page must not contain SPA prefetch data")
	assert.NotContains(t, body, `src="/assets/main.js"`, "suspended page must not load SPA entry script")
}

func TestSuspendedUserMiddleware_DeletedUser_ClearsSession(t *testing.T) {
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "deleted@example.com",
		Role:   "admin",
		Status: "active",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	handler := ws.Handler()

	// Establish session while user exists.
	cookies := loginSession(t, ws, "user-1", "deleted@example.com", "admin")

	// Delete user from store.
	delete(st.users, "user-1")

	// Next browser navigation should redirect to login.
	req := httptest.NewRequest("GET", "/projects", nil)
	req.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/login", rec.Header().Get("Location"))

	// Session cookie should be cleared (MaxAge = -1).
	var sessionCleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == webSessionName && c.MaxAge < 0 {
			sessionCleared = true
			break
		}
	}
	assert.True(t, sessionCleared, "session cookie should be invalidated for deleted user")
}

func TestSuspendedUserMiddleware_APIRoutes_Unaffected(t *testing.T) {
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "suspended@example.com",
		Role:   "member",
		Status: "suspended",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	handler := ws.Handler()

	// Establish session.
	cookies := loginSession(t, ws, "user-1", "suspended@example.com", "member")

	// API routes should pass through the suspended middleware unchanged
	// (the Hub's UnifiedAuth handles them with structured JSON denial).
	req := httptest.NewRequest("GET", "/api/v1/projects", nil)
	req.Header.Set("Accept", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// The middleware skips /api/v1/ routes, so they reach the mux.
	// Without a mounted hub handler, the mux returns 404 for API routes.
	// The key assertion is that the middleware did NOT return 403/HTML.
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"API routes should not be intercepted by suspended user middleware")
}

func TestSuspendedUserMiddleware_PublicRoutes_Accessible(t *testing.T) {
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "suspended@example.com",
		Role:   "member",
		Status: "suspended",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	handler := ws.Handler()

	// Establish session.
	cookies := loginSession(t, ws, "user-1", "suspended@example.com", "member")

	// Public routes must remain accessible even for suspended users.
	publicPaths := []string{"/healthz", "/login", "/auth/logout", "/auth/providers"}
	for _, path := range publicPaths {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "text/html")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.NotEqual(t, http.StatusForbidden, rec.Code,
			"public route %s should not return 403 for suspended user", path)
	}
}

func TestSuspendedUserMiddleware_SSE_ReturnsJSON(t *testing.T) {
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "suspended@example.com",
		Role:   "member",
		Status: "suspended",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	handler := ws.Handler()

	// Establish session.
	cookies := loginSession(t, ws, "user-1", "suspended@example.com", "member")

	// SSE requests (Accept: text/event-stream) should get JSON denial,
	// not HTML.
	req := httptest.NewRequest("GET", "/events?sub=project.123.>", nil)
	req.Header.Set("Accept", "text/event-stream")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	var resp map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	errObj, _ := resp["error"].(map[string]interface{})
	assert.Equal(t, "user_suspended", errObj["code"])
}

// ---------------------------------------------------------------------------
// Suspended page content assertions — zero bootstrap fan-out
// ---------------------------------------------------------------------------

func TestSuspendedPage_NoSPABootstrap(t *testing.T) {
	// This is the "headless-browser equivalent" network-level test: a
	// suspended initial page load must produce zero protected API/SSE
	// bootstrap fan-out, verified by the absence of the SPA entry module
	// and __SCION_DATA__ in the response HTML.
	st := newProxyAuthStore()
	st.CreateUser(context.Background(), &store.User{
		ID:     "user-1",
		Email:  "suspended@example.com",
		Role:   "member",
		Status: "suspended",
	})

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetStore(st)
	handler := ws.Handler()

	cookies := loginSession(t, ws, "user-1", "suspended@example.com", "member")

	// Request the root page as a suspended user.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	// No SPA entry script — prevents all client-side JS bootstrap.
	assert.NotContains(t, html, `<script type="module"`,
		"suspended page must not contain module script tags")
	assert.NotContains(t, html, "main.js",
		"suspended page must not reference SPA entry point")

	// No __SCION_DATA__ — prevents prefetch data from being embedded.
	assert.NotContains(t, html, "__SCION_DATA__",
		"suspended page must not contain SSR prefetch data tag")

	// No scion-app shell element — prevents component initialization.
	assert.NotContains(t, html, "<scion-app",
		"suspended page must not contain SPA root element")

	// Does contain the expected suspended-account content.
	assert.Contains(t, html, "Account Suspended")
	assert.Contains(t, html, "suspended@example.com")
	assert.Contains(t, html, "/auth/logout")
}

// ---------------------------------------------------------------------------
// Proxy mode — suspended and deleted user with existing session
// ---------------------------------------------------------------------------

func TestProxyAuth_SuspendedUser_ServesPage_NotRaw403(t *testing.T) {
	// Verifies that proxy mode serves the proper suspended HTML page
	// instead of a raw http.Error 403 text response.
	mockAuth := &mockProxyAuthenticator{
		user: &ProxyUserInfo{
			Subject: "12345",
			Email:   "proxy-user@example.com",
			Domain:  "example.com",
		},
	}

	st := newProxyAuthStore()
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode:           "proxy",
		ProxyAuthenticator: mockAuth,
	})
	ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
	ws.SetStore(st)
	handler := ws.Handler()

	// First request: provisions the user and establishes the session.
	req1 := httptest.NewRequest("GET", "/projects", nil)
	req1.Header.Set("Accept", "text/html")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	require.NotEqual(t, http.StatusForbidden, rec1.Code)
	cookies := rec1.Result().Cookies()
	require.NotEmpty(t, cookies)

	// Suspend the user.
	created, err := st.GetUserByEmail(context.Background(), "proxy-user@example.com")
	require.NoError(t, err)
	created.Status = "suspended"
	require.NoError(t, st.UpdateUser(context.Background(), created))

	// Replay session cookie.
	req2 := httptest.NewRequest("GET", "/projects", nil)
	req2.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusForbidden, rec2.Code)
	body := rec2.Body.String()
	assert.Contains(t, body, "Account Suspended",
		"proxy mode should serve the suspended HTML page, not a raw text error")
	assert.Contains(t, body, "proxy-user@example.com")
	assert.Contains(t, body, "/auth/logout")
	assert.NotContains(t, body, "__SCION_DATA__")
}

func TestProxyAuth_DeletedUser_ClearsSession(t *testing.T) {
	// Verifies that proxy mode clears the session and redirects for
	// deleted users instead of returning a raw http.Error 403.
	mockAuth := &mockProxyAuthenticator{
		user: &ProxyUserInfo{
			Subject: "12345",
			Email:   "proxy-user@example.com",
			Domain:  "example.com",
		},
	}

	st := newProxyAuthStore()
	ws := newTestWebServer(t, WebServerConfig{
		AuthMode:           "proxy",
		ProxyAuthenticator: mockAuth,
	})
	ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
	ws.SetStore(st)
	handler := ws.Handler()

	// First request: provisions user.
	req1 := httptest.NewRequest("GET", "/projects", nil)
	req1.Header.Set("Accept", "text/html")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	cookies := rec1.Result().Cookies()
	require.NotEmpty(t, cookies)

	// Delete user from store.
	created, err := st.GetUserByEmail(context.Background(), "proxy-user@example.com")
	require.NoError(t, err)
	delete(st.users, created.ID)

	// Replay session cookie.
	req2 := httptest.NewRequest("GET", "/projects", nil)
	req2.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusFound, rec2.Code,
		"deleted user should be redirected to login")
	assert.Equal(t, "/login", rec2.Header().Get("Location"))
}

func TestProxyAuth_NewLogin_SuspendedUser_ServesPage(t *testing.T) {
	// Verifies that proxy mode serves the suspended page when a known
	// suspended user attempts to login through proxy auth (no existing session).
	mockAuth := &mockProxyAuthenticator{
		user: &ProxyUserInfo{
			Subject:     "12345",
			Email:       "proxy-user@example.com",
			Domain:      "example.com",
			DisplayName: "Proxy User",
		},
	}

	st := newProxyAuthStore()
	// Pre-create the user as suspended.
	st.CreateUser(context.Background(), &store.User{
		ID:          "pre-existing",
		Email:       "proxy-user@example.com",
		DisplayName: "Proxy User",
		Role:        "member",
		Status:      "suspended",
		Created:     time.Now(),
	})

	ws := newTestWebServer(t, WebServerConfig{
		AuthMode:           "proxy",
		ProxyAuthenticator: mockAuth,
	})
	ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
	ws.SetStore(st)
	handler := ws.Handler()

	// First request (no session) — proxy provisions but user is suspended.
	req := httptest.NewRequest("GET", "/projects", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Account Suspended")
	assert.Contains(t, body, "proxy-user@example.com")
}

// ---------------------------------------------------------------------------
// HTML sanitization
// ---------------------------------------------------------------------------

func TestSuspendedPage_EmailEscaped(t *testing.T) {
	html := suspendedPageHTML("<script>alert('xss')</script>@example.com")
	assert.NotContains(t, html, "<script>alert")
	assert.Contains(t, html, "&lt;script&gt;")
}

// ---------------------------------------------------------------------------
// Helper: loginSession creates a session cookie for a test user.
// ---------------------------------------------------------------------------

func loginSession(t *testing.T, ws *WebServer, userID, email, role string) []*http.Cookie {
	t.Helper()

	// Create a synthetic request and set up the session.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	session, err := ws.sessionStore.Get(req, webSessionName)
	require.NoError(t, err)

	session.Values[sessKeyUserID] = userID
	session.Values[sessKeyUserEmail] = email
	session.Values[sessKeyUserName] = "Test User"
	session.Values[sessKeyUserAvatar] = ""
	session.Values[sessKeyUserRole] = role

	require.NoError(t, session.Save(req, rec))

	return rec.Result().Cookies()
}
