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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
// Fail-closed: ws.store == nil
// ---------------------------------------------------------------------------

func TestSuspendedUserMiddleware_NilStore_FailsClosed_Browser(t *testing.T) {
	// Need a session but no store — the middleware must fail closed.
	// Use dev auth which auto-creates a session without needing a store
	// for the auth step, but the suspended-user middleware will see nil store.
	wsWithDevAuth := newDevAuthWebServer(t, func(cfg *WebServerConfig) {})
	// Don't set a store on the dev auth server.
	handlerNoStore := wsWithDevAuth.Handler()

	req := httptest.NewRequest("GET", "/projects", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	handlerNoStore.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"nil store must fail closed with 500, not pass through")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html",
		"browser request should get HTML error")
}

func TestSuspendedUserMiddleware_NilStore_FailsClosed_JSON(t *testing.T) {
	wsWithDevAuth := newDevAuthWebServer(t, func(cfg *WebServerConfig) {})
	handlerNoStore := wsWithDevAuth.Handler()

	req := httptest.NewRequest("GET", "/events?sub=project.123.>", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handlerNoStore.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"nil store must fail closed with 500")
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json",
		"non-browser request should get JSON error")

	var resp map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	errObj, _ := resp["error"].(map[string]interface{})
	assert.Equal(t, "internal_error", errObj["code"])
}

// ---------------------------------------------------------------------------
// Proxy existing-session: SSE/non-browser must get JSON, not HTML (R-1 fix)
// ---------------------------------------------------------------------------

func TestProxyAuth_SuspendedUser_SSE_ReturnsJSON(t *testing.T) {
	// Verifies that proxy mode's existing-session branch returns JSON
	// for SSE requests from suspended users, not HTML.
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

	// Suspend the user.
	created, err := st.GetUserByEmail(context.Background(), "proxy-user@example.com")
	require.NoError(t, err)
	created.Status = "suspended"
	require.NoError(t, st.UpdateUser(context.Background(), created))

	// SSE request with session cookie must get JSON denial.
	req2 := httptest.NewRequest("GET", "/events?sub=project.123.>", nil)
	req2.Header.Set("Accept", "text/event-stream")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusForbidden, rec2.Code)
	assert.Contains(t, rec2.Header().Get("Content-Type"), "application/json",
		"SSE request from suspended user must get JSON, not HTML")

	var resp map[string]interface{}
	err = json.Unmarshal(rec2.Body.Bytes(), &resp)
	require.NoError(t, err)
	errObj, _ := resp["error"].(map[string]interface{})
	assert.Equal(t, "user_suspended", errObj["code"])
}

func TestProxyAuth_NewLogin_SuspendedUser_SSE_ReturnsJSON(t *testing.T) {
	// Verifies that proxy mode's new-login branch returns JSON for
	// non-browser requests from suspended users.
	mockAuth := &mockProxyAuthenticator{
		user: &ProxyUserInfo{
			Subject:     "12345",
			Email:       "proxy-user@example.com",
			Domain:      "example.com",
			DisplayName: "Proxy User",
		},
	}

	st := newProxyAuthStore()
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

	// Non-browser request (no session) — suspended user.
	req := httptest.NewRequest("GET", "/events?sub=project.123.>", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json",
		"non-browser suspended new-login must get JSON denial")

	var resp map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	errObj, _ := resp["error"].(map[string]interface{})
	assert.Equal(t, "user_suspended", errObj["code"])
}

// ---------------------------------------------------------------------------
// Proxy existing-session: uses user ID lookup (R2 correction #2)
// ---------------------------------------------------------------------------

func TestProxyAuth_ExistingSession_LookupByUserID(t *testing.T) {
	// Verifies that the proxy existing-session path uses GetUser (by ID)
	// not GetUserByEmail, so email changes don't resolve the wrong account.
	mockAuth := &mockProxyAuthenticator{
		user: &ProxyUserInfo{
			Subject: "12345",
			Email:   "original@example.com",
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

	// Change the user's email in the store (simulates email update).
	created, err := st.GetUserByEmail(context.Background(), "original@example.com")
	require.NoError(t, err)
	created.Email = "changed@example.com"
	require.NoError(t, st.UpdateUser(context.Background(), created))

	// Replay session cookie — should still resolve the same user by ID.
	req2 := httptest.NewRequest("GET", "/projects", nil)
	req2.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	// Should succeed (not 500 or 403).
	assert.Equal(t, http.StatusOK, rec2.Code,
		"user with changed email should still be found by ID")
}

// ---------------------------------------------------------------------------
// Proxy existing-session: fail closed on nil store and transient errors
// ---------------------------------------------------------------------------

func TestProxyAuth_ExistingSession_NilStore_FailsClosed(t *testing.T) {
	mockAuth := &mockProxyAuthenticator{
		user: &ProxyUserInfo{
			Subject: "12345",
			Email:   "user@example.com",
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

	// First request: provisions user and establishes session.
	handler := ws.Handler()
	req1 := httptest.NewRequest("GET", "/projects", nil)
	req1.Header.Set("Accept", "text/html")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	cookies := rec1.Result().Cookies()
	require.NotEmpty(t, cookies)

	// Remove the store to simulate nil store on session replay.
	ws.store = nil
	handler2 := ws.Handler()

	req2 := httptest.NewRequest("GET", "/projects", nil)
	req2.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler2.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusInternalServerError, rec2.Code,
		"nil store must fail closed, not pass through")
}

func TestProxyAuth_ExistingSession_TransientError_FailsClosed(t *testing.T) {
	mockAuth := &mockProxyAuthenticator{
		user: &ProxyUserInfo{
			Subject: "12345",
			Email:   "user@example.com",
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

	// First request: provisions user and establishes session.
	handler := ws.Handler()
	req1 := httptest.NewRequest("GET", "/projects", nil)
	req1.Header.Set("Accept", "text/html")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	cookies := rec1.Result().Cookies()
	require.NotEmpty(t, cookies)

	// Get the user ID so we can inject a transient error store.
	created, err := st.GetUserByEmail(context.Background(), "user@example.com")
	require.NoError(t, err)

	// Replace with a transient-error store.
	errStore := &transientErrorStore{userID: created.ID}
	ws.store = errStore
	handler2 := ws.Handler()

	req2 := httptest.NewRequest("GET", "/projects", nil)
	req2.Header.Set("Accept", "text/html")
	for _, c := range cookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler2.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusInternalServerError, rec2.Code,
		"transient error must fail closed")
}

// ---------------------------------------------------------------------------
// Adversarial response-classification tests (R2 correction #5)
// ---------------------------------------------------------------------------

func TestSuspendedResponse_Classification(t *testing.T) {
	// Comprehensive adversarial test for response classification across
	// both proxy existing-session and first-login paths. Verifies that
	// browser navigations get HTML and all other request types get JSON.

	tests := []struct {
		name           string
		accept         string
		method         string
		path           string
		wantJSON       bool // true = expect JSON, false = expect HTML
		wantStatus     int
		skipSuspension bool // true = expect passthrough (public/API routes)
	}{
		// Browser navigation — HTML
		{
			name:       "Accept text/html",
			accept:     "text/html",
			path:       "/projects",
			wantJSON:   false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "Accept text/html with q-values",
			accept:     "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			path:       "/projects",
			wantJSON:   false,
			wantStatus: http.StatusForbidden,
		},
		// Non-browser — JSON
		{
			name:       "Accept application/json",
			accept:     "application/json",
			path:       "/projects",
			wantJSON:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "Accept text/event-stream (EventSource)",
			accept:     "text/event-stream",
			path:       "/events?sub=project.123.>",
			wantJSON:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "No Accept header (programmatic fetch)",
			accept:     "",
			path:       "/projects",
			wantJSON:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "Accept */* (fetch default)",
			accept:     "*/*",
			path:       "/projects",
			wantJSON:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "Accept application/json, text/plain",
			accept:     "application/json, text/plain, */*",
			path:       "/projects",
			wantJSON:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "Mixed with html lower q than json",
			accept:     "application/json;q=1.0, text/html;q=0.5",
			path:       "/projects",
			wantJSON:   false, // isBrowserRequest checks Contains("text/html")
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "HEAD method with HTML Accept",
			accept:     "text/html",
			method:     "HEAD",
			path:       "/projects",
			wantJSON:   false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "HEAD method with JSON Accept",
			accept:     "application/json",
			method:     "HEAD",
			path:       "/projects",
			wantJSON:   true,
			wantStatus: http.StatusForbidden,
		},
		// Public/API routes — should NOT be intercepted
		{
			name:           "API path with HTML Accept",
			accept:         "text/html",
			path:           "/api/v1/projects",
			skipSuspension: true,
		},
		{
			name:           "Login path",
			accept:         "text/html",
			path:           "/login",
			skipSuspension: true,
		},
		{
			name:           "Logout path",
			accept:         "text/html",
			path:           "/auth/logout",
			skipSuspension: true,
		},
		{
			name:           "Health path",
			accept:         "text/html",
			path:           "/healthz",
			skipSuspension: true,
		},
		{
			name:           "Static assets",
			accept:         "text/html",
			path:           "/assets/main.js",
			skipSuspension: true,
		},
	}

	// Test against OAuth/session middleware path.
	t.Run("OAuth/session path", func(t *testing.T) {
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

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				method := tc.method
				if method == "" {
					method = "GET"
				}
				req := httptest.NewRequest(method, tc.path, nil)
				if tc.accept != "" {
					req.Header.Set("Accept", tc.accept)
				}
				for _, c := range cookies {
					req.AddCookie(c)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if tc.skipSuspension {
					assert.NotEqual(t, http.StatusForbidden, rec.Code,
						"route %s must not be intercepted by suspended middleware", tc.path)
					return
				}

				assert.Equal(t, tc.wantStatus, rec.Code)
				if tc.wantJSON {
					assert.Contains(t, rec.Header().Get("Content-Type"), "application/json",
						"expected JSON response")
					var resp map[string]interface{}
					err := json.Unmarshal(rec.Body.Bytes(), &resp)
					require.NoError(t, err, "response must be valid JSON")
					errObj, _ := resp["error"].(map[string]interface{})
					assert.Equal(t, "user_suspended", errObj["code"])
				} else {
					assert.Contains(t, rec.Header().Get("Content-Type"), "text/html",
						"expected HTML response")
					assert.Contains(t, rec.Body.String(), "Account Suspended")
				}
			})
		}
	})

	// Test against proxy existing-session path.
	t.Run("Proxy existing-session path", func(t *testing.T) {
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

		// Provision the user.
		req1 := httptest.NewRequest("GET", "/projects", nil)
		req1.Header.Set("Accept", "text/html")
		rec1 := httptest.NewRecorder()
		handler.ServeHTTP(rec1, req1)
		cookies := rec1.Result().Cookies()
		require.NotEmpty(t, cookies)

		// Suspend the user.
		created, err := st.GetUserByEmail(context.Background(), "proxy-user@example.com")
		require.NoError(t, err)
		created.Status = "suspended"
		require.NoError(t, st.UpdateUser(context.Background(), created))

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				method := tc.method
				if method == "" {
					method = "GET"
				}
				req := httptest.NewRequest(method, tc.path, nil)
				if tc.accept != "" {
					req.Header.Set("Accept", tc.accept)
				}
				for _, c := range cookies {
					req.AddCookie(c)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				if tc.skipSuspension {
					assert.NotEqual(t, http.StatusForbidden, rec.Code,
						"route %s must not be intercepted", tc.path)
					return
				}

				assert.Equal(t, tc.wantStatus, rec.Code)
				if tc.wantJSON {
					assert.Contains(t, rec.Header().Get("Content-Type"), "application/json",
						"expected JSON response")
					var resp map[string]interface{}
					err := json.Unmarshal(rec.Body.Bytes(), &resp)
					require.NoError(t, err, "response must be valid JSON")
					errObj, _ := resp["error"].(map[string]interface{})
					assert.Equal(t, "user_suspended", errObj["code"])
				} else {
					assert.Contains(t, rec.Header().Get("Content-Type"), "text/html",
						"expected HTML response")
					assert.Contains(t, rec.Body.String(), "Account Suspended")
				}
			})
		}
	})

	// Test against proxy first-login path.
	t.Run("Proxy first-login path", func(t *testing.T) {
		st := newProxyAuthStore()
		st.CreateUser(context.Background(), &store.User{
			ID:          "pre-existing",
			Email:       "proxy-new@example.com",
			DisplayName: "Proxy User",
			Role:        "member",
			Status:      "suspended",
			Created:     time.Now(),
		})

		for _, tc := range tests {
			if tc.skipSuspension {
				continue // first-login doesn't reach these paths the same way
			}
			t.Run(tc.name, func(t *testing.T) {
				mockAuth := &mockProxyAuthenticator{
					user: &ProxyUserInfo{
						Subject:     "12345",
						Email:       "proxy-new@example.com",
						Domain:      "example.com",
						DisplayName: "Proxy User",
					},
				}
				ws := newTestWebServer(t, WebServerConfig{
					AuthMode:           "proxy",
					ProxyAuthenticator: mockAuth,
				})
				ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
				ws.SetStore(st)
				handler := ws.Handler()

				method := tc.method
				if method == "" {
					method = "GET"
				}
				req := httptest.NewRequest(method, tc.path, nil)
				if tc.accept != "" {
					req.Header.Set("Accept", tc.accept)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)

				assert.Equal(t, tc.wantStatus, rec.Code)
				if tc.wantJSON {
					assert.Contains(t, rec.Header().Get("Content-Type"), "application/json",
						"expected JSON response")
				} else {
					assert.Contains(t, rec.Header().Get("Content-Type"), "text/html",
						"expected HTML response")
					assert.Contains(t, rec.Body.String(), "Account Suspended")
				}
			})
		}
	})
}

// TestSuspendedResponse_Never200HTML verifies the hard invariant: no protected
// API or non-public route may EVER return a 200 with suspended HTML content.
func TestSuspendedResponse_Never200HTML(t *testing.T) {
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

	protectedPaths := []string{"/", "/projects", "/agents", "/skills", "/events?sub=project.123.>"}
	for _, path := range protectedPaths {
		for _, accept := range []string{"text/html", "application/json", "text/event-stream", "*/*", ""} {
			req := httptest.NewRequest("GET", path, nil)
			if accept != "" {
				req.Header.Set("Accept", accept)
			}
			for _, c := range cookies {
				req.AddCookie(c)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "Account Suspended") {
				t.Errorf("INVARIANT VIOLATION: path=%s accept=%q returned 200 with suspended HTML",
					path, accept)
			}
		}
	}
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
// transientErrorStore simulates a store that returns transient errors for
// GetUser lookups.
// ---------------------------------------------------------------------------

type transientErrorStore struct {
	store.Store
	userID string
}

func (s *transientErrorStore) GetUser(_ context.Context, id string) (*store.User, error) {
	if id == s.userID {
		return nil, fmt.Errorf("connection refused (simulated transient error)")
	}
	return nil, store.ErrNotFound
}

func (s *transientErrorStore) GetUserByEmail(_ context.Context, _ string) (*store.User, error) {
	return nil, fmt.Errorf("connection refused (simulated transient error)")
}

func (s *transientErrorStore) GetGroupBySlug(_ context.Context, _ string) (*store.Group, error) {
	return nil, store.ErrNotFound
}

func (s *transientErrorStore) AddGroupMember(_ context.Context, _ *store.GroupMember) error {
	return nil
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
