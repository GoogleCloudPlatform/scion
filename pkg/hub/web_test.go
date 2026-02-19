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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptone/scion-agent/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockWebStore is a minimal mock of store.Store for web tests.
// Only methods actually called in tests are implemented; others panic.
type mockWebStore struct {
	store.Store // embed interface to satisfy all method signatures (will panic if called)
}

func newTestWebServer(t *testing.T, cfg WebServerConfig) *WebServer {
	t.Helper()
	return NewWebServer(cfg)
}

// newDevAuthWebServer creates a web server with dev-auth enabled for testing
// authenticated routes without requiring OAuth.
func newDevAuthWebServer(t *testing.T, overrides ...func(*WebServerConfig)) *WebServer {
	t.Helper()
	cfg := WebServerConfig{
		DevAuthToken: "test-dev-token-12345",
	}
	for _, fn := range overrides {
		fn(&cfg)
	}
	return NewWebServer(cfg)
}

func TestSPAShellHandler(t *testing.T) {
	// Use dev-auth so the SPA handler is accessible
	ws := newDevAuthWebServer(t)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	html := string(body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected Content-Type text/html, got %q", ct)
	}

	// Verify expected SPA shell elements
	checks := map[string]string{
		"__SCION_DATA__":  "hydration data script",
		"scion-app":       "root custom element",
		"main.js":         "client entry point script",
		"--scion-primary": "critical CSS variables",
		"scion-theme":     "theme detection script",
		shoelaceVersion:   "Shoelace CDN version",
	}
	for needle, desc := range checks {
		if !strings.Contains(html, needle) {
			t.Errorf("SPA shell missing %s (expected %q in HTML)", desc, needle)
		}
	}
}

func TestSPACatchAll(t *testing.T) {
	// Use dev-auth so all routes are accessible
	ws := newDevAuthWebServer(t)

	// Various SPA routes should all return the SPA shell
	paths := []string{"/", "/groves", "/agents", "/groves/abc123", "/settings", "/not-a-real-page"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			rec := httptest.NewRecorder()

			ws.Handler().ServeHTTP(rec, req)

			resp := rec.Result()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected status 200 for %s, got %d", path, resp.StatusCode)
			}

			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "scion-app") {
				t.Errorf("expected SPA shell HTML for %s", path)
			}
		})
	}
}

func TestStaticAssetHandler_Disk(t *testing.T) {
	// Create a temporary directory with a test asset
	tmpDir := t.TempDir()
	testContent := "console.log('test');"
	if err := os.WriteFile(filepath.Join(tmpDir, "main.js"), []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to write test asset: %v", err)
	}

	ws := newTestWebServer(t, WebServerConfig{
		AssetsDir: tmpDir,
	})

	req := httptest.NewRequest("GET", "/assets/main.js", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if string(body) != testContent {
		t.Errorf("expected %q, got %q", testContent, string(body))
	}

	// Non-hashed asset should get no-cache
	cc := resp.Header.Get("Cache-Control")
	if cc != "no-cache" {
		t.Errorf("expected Cache-Control no-cache for non-hashed asset, got %q", cc)
	}
}

func TestStaticAssetHandler_HashedCaching(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "chunk-abc12345.js"), []byte("// chunk"), 0644); err != nil {
		t.Fatalf("failed to write test asset: %v", err)
	}

	ws := newTestWebServer(t, WebServerConfig{
		AssetsDir: tmpDir,
	})

	req := httptest.NewRequest("GET", "/assets/chunk-abc12345.js", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	cc := resp.Header.Get("Cache-Control")
	if cc != "public, max-age=86400" {
		t.Errorf("expected Cache-Control for hashed asset, got %q", cc)
	}
}

func TestStaticAssetHandler_NoAssets(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/assets/main.js", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404 when no assets available, got %d", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()

	expectedHeaders := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"X-XSS-Protection":      "1; mode=block",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}

	for header, expected := range expectedHeaders {
		got := resp.Header.Get(header)
		if got != expected {
			t.Errorf("header %s: expected %q, got %q", header, expected, got)
		}
	}

	// Verify CSP is set and contains key directives
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Error("Content-Security-Policy header not set")
	} else {
		cspChecks := []string{
			"default-src 'self'",
			"script-src 'self'",
			"cdn.jsdelivr.net",
			"fonts.googleapis.com",
			"fonts.gstatic.com",
		}
		for _, check := range cspChecks {
			if !strings.Contains(csp, check) {
				t.Errorf("CSP missing %q", check)
			}
		}
	}

	// Verify Permissions-Policy is set
	pp := resp.Header.Get("Permissions-Policy")
	if pp == "" {
		t.Error("Permissions-Policy header not set")
	} else if !strings.Contains(pp, "camera=()") {
		t.Errorf("Permissions-Policy missing camera restriction: %q", pp)
	}
}

func TestWebHealthz(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{
		AssetsDir: "/tmp/test-assets",
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"status":"ok"`) {
		t.Errorf("expected status ok in response: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"component":"web"`) {
		t.Errorf("expected component web in response: %s", bodyStr)
	}
}

func TestIsHashedAsset(t *testing.T) {
	tests := []struct {
		path   string
		hashed bool
	}{
		{"chunk-abc12345.js", true},
		{"style-deadbeef.css", true},
		{"main.js", false},
		{"main.css", false},
		{"chunk-ab.js", false},       // hash too short
		{"chunk-ABCDEF12.js", true},   // uppercase hex
		{".js", false},               // no name
		{"no-extension", false},      // no extension
		{"name-ghijk.js", false},     // non-hex chars
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isHashedAsset(tt.path)
			if got != tt.hashed {
				t.Errorf("isHashedAsset(%q) = %v, want %v", tt.path, got, tt.hashed)
			}
		})
	}
}

// --- Session Management & Auth Tests ---

func TestSessionMiddleware_PublicRoutes(t *testing.T) {
	// Public routes should be accessible without authentication.
	// They should NOT redirect to /auth/login (the session auth redirect).
	ws := newTestWebServer(t, WebServerConfig{})

	publicPaths := []string{"/healthz", "/auth/me", "/auth/logout", "/auth/debug"}
	for _, path := range publicPaths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("Accept", "text/html") // simulate browser
			rec := httptest.NewRecorder()

			ws.Handler().ServeHTTP(rec, req)

			resp := rec.Result()
			location := resp.Header.Get("Location")
			// These routes should NOT redirect to /auth/login (session auth redirect)
			if resp.StatusCode == http.StatusFound {
				assert.NotEqual(t, "/auth/login", location,
					"public route %s should not redirect to /auth/login", path)
			}
		})
	}

	// /auth/login/ redirects to /login (SPA page), which is valid — it's the
	// handler's intended behavior, not a session-auth redirect.
	t.Run("/auth/login/", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/auth/login/", nil)
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)

		resp := rec.Result()
		assert.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Equal(t, "/login", resp.Header.Get("Location"),
			"/auth/login/ should redirect to /login (SPA), not /auth/login")
	})
}

func TestSessionMiddleware_AssetsPublic(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.js"), []byte("//js"), 0644))

	ws := newTestWebServer(t, WebServerConfig{AssetsDir: tmpDir})

	req := httptest.NewRequest("GET", "/assets/test.js", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Result().StatusCode,
		"/assets/ routes should be public")
}

func TestSessionMiddleware_ProtectedRedirect(t *testing.T) {
	// Unauthenticated browser request to a protected route should redirect
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/groves", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	location := resp.Header.Get("Location")
	assert.Equal(t, "/auth/login", location)
}

func TestSessionMiddleware_ProtectedAPI(t *testing.T) {
	// Unauthenticated API request (no Accept: text/html) should get 401 JSON
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/api/data", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "authentication required", result["error"])
}

func TestDevAuth_AutoLogin(t *testing.T) {
	ws := newDevAuthWebServer(t)

	// Request to a protected route should succeed with dev-auth
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"dev-auth should auto-login and serve the page")

	// A session cookie should be set
	cookies := resp.Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == webSessionName {
			sessionCookie = c
			break
		}
	}
	assert.NotNil(t, sessionCookie, "session cookie should be set")
}

func TestDevAuth_SessionPersists(t *testing.T) {
	ws := newDevAuthWebServer(t)
	handler := ws.Handler()

	// First request: get the session cookie
	req1 := httptest.NewRequest("GET", "/auth/me", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	resp1 := rec1.Result()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// Parse response body
	body1, _ := io.ReadAll(resp1.Body)
	var user1 webSessionUser
	require.NoError(t, json.Unmarshal(body1, &user1))
	assert.Equal(t, "dev-user", user1.UserID)
	assert.Equal(t, "dev@localhost", user1.Email)
	assert.Equal(t, "Development User", user1.Name)

	// Second request with the session cookie should also work
	req2 := httptest.NewRequest("GET", "/auth/me", nil)
	for _, c := range resp1.Cookies() {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	resp2 := rec2.Result()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	body2, _ := io.ReadAll(resp2.Body)
	var user2 webSessionUser
	require.NoError(t, json.Unmarshal(body2, &user2))
	assert.Equal(t, "dev-user", user2.UserID)
}

func TestDevAuth_Disabled(t *testing.T) {
	// Without dev token, no auto-login should occur
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode,
		"without dev-auth, protected routes should redirect to login")
}

func TestAuthMe_Unauthenticated(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/auth/me", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "authentication required", result["error"])
}

func TestAuthMe_Authenticated(t *testing.T) {
	ws := newDevAuthWebServer(t)
	handler := ws.Handler()

	// First request auto-logs in
	req := httptest.NewRequest("GET", "/auth/me", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var user webSessionUser
	require.NoError(t, json.Unmarshal(body, &user))
	assert.Equal(t, "dev-user", user.UserID)
	assert.Equal(t, "dev@localhost", user.Email)
	assert.Equal(t, "Development User", user.Name)
}

func TestLogout_ClearsSession(t *testing.T) {
	ws := newDevAuthWebServer(t)
	handler := ws.Handler()

	// First: auto-login to get a session
	req1 := httptest.NewRequest("GET", "/auth/me", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	resp1 := rec1.Result()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	// POST /auth/logout with session cookies
	req2 := httptest.NewRequest("POST", "/auth/logout", nil)
	for _, c := range resp1.Cookies() {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	resp2 := rec2.Result()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	body, _ := io.ReadAll(resp2.Body)
	var result map[string]bool
	require.NoError(t, json.Unmarshal(body, &result))
	assert.True(t, result["success"])

	// The session cookie should be invalidated (MaxAge < 0)
	var found bool
	for _, c := range resp2.Cookies() {
		if c.Name == webSessionName {
			found = true
			assert.True(t, c.MaxAge < 0, "session cookie should have negative MaxAge to delete it")
		}
	}
	assert.True(t, found, "session cookie should be present in logout response")
}

func TestLogout_BrowserRedirect(t *testing.T) {
	ws := newDevAuthWebServer(t)
	handler := ws.Handler()

	// Browser logout should redirect to /login
	req := httptest.NewRequest("GET", "/auth/logout", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))
}

func TestOAuthLogin_UnknownProvider(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/auth/login/unknown", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOAuthLogin_NoOAuthService(t *testing.T) {
	// Without an OAuth service configured, login should return 503
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/auth/login/google", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestOAuthLogin_Redirect(t *testing.T) {
	// Create a web server with a mock OAuth service configured for Google
	ws := newTestWebServer(t, WebServerConfig{
		BaseURL: "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
		},
	})

	req := httptest.NewRequest("GET", "/auth/login/google", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.Contains(t, location, "accounts.google.com")
	assert.Contains(t, location, "test-client-id")
	assert.Contains(t, location, "redirect_uri=")
	assert.Contains(t, location, "state=")
}

func TestOAuthLogin_ProviderNotConfigured(t *testing.T) {
	// OAuth service exists but GitHub is not configured
	ws := newTestWebServer(t, WebServerConfig{
		BaseURL: "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			},
			// GitHub not configured
		},
	})

	req := httptest.NewRequest("GET", "/auth/login/github", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOAuthLogin_NoProvider_RedirectsToLoginPage(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/auth/login/", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/login", resp.Header.Get("Location"))
}

func TestOAuthCallback_StateMismatch(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{
		BaseURL: "http://localhost:8080",
	})
	ws.oauthService = NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "test-id",
				ClientSecret: "test-secret",
			},
		},
	})
	// Set a mock store so the handler doesn't short-circuit with 503
	ws.store = &mockWebStore{}

	// Request a callback with a state that doesn't match the session
	req := httptest.NewRequest("GET", "/auth/callback/google?code=test-code&state=bad-state", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	location := resp.Header.Get("Location")
	assert.Contains(t, location, "error=state_mismatch")
}

func TestOAuthCallback_NoOAuthService(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/auth/callback/google?code=test&state=test", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestAuthDebug_DebugMode(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{
		Debug:   true,
		BaseURL: "http://localhost:8080",
	})

	req := httptest.NewRequest("GET", "/auth/debug", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var debug map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &debug))

	assert.Contains(t, debug, "sessionIsNew")
	assert.Contains(t, debug, "hasUser")
	assert.Contains(t, debug, "config")

	config := debug["config"].(map[string]interface{})
	assert.Equal(t, "http://localhost:8080", config["baseURL"])
	assert.Equal(t, false, config["devAuthEnabled"])
}

func TestAuthDebug_NotAvailableInProduction(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{
		Debug: false,
	})

	req := httptest.NewRequest("GET", "/auth/debug", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestIsPublicRoute(t *testing.T) {
	tests := []struct {
		path   string
		public bool
	}{
		{"/healthz", true},
		{"/assets/main.js", true},
		{"/assets/chunk-abc123.js", true},
		{"/auth/login/google", true},
		{"/auth/callback/google", true},
		{"/auth/me", true},
		{"/auth/logout", true},
		{"/auth/debug", true},
		{"/login", true},
		{"/favicon.ico", true},
		{"/", false},
		{"/groves", false},
		{"/agents", false},
		{"/settings", false},
		{"/api/data", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isPublicRoute(tt.path)
			assert.Equal(t, tt.public, got, "isPublicRoute(%q)", tt.path)
		})
	}
}

func TestIsBrowserRequest(t *testing.T) {
	tests := []struct {
		accept  string
		browser bool
	}{
		{"text/html", true},
		{"text/html, application/xhtml+xml", true},
		{"application/json", false},
		{"", false},
		{"*/*", false},
	}

	for _, tt := range tests {
		t.Run(tt.accept, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			assert.Equal(t, tt.browser, isBrowserRequest(req))
		})
	}
}

func TestSessionStore_CookieConfiguration(t *testing.T) {
	// HTTPS base URL should produce secure cookies
	ws := newTestWebServer(t, WebServerConfig{
		BaseURL: "https://scion.example.com",
	})
	assert.True(t, ws.sessionStore.Options.Secure,
		"HTTPS base URL should produce secure cookies")
	assert.True(t, ws.sessionStore.Options.HttpOnly,
		"cookies should always be HttpOnly")
	assert.Equal(t, http.SameSiteLaxMode, ws.sessionStore.Options.SameSite)

	// HTTP base URL should produce non-secure cookies
	ws2 := newTestWebServer(t, WebServerConfig{
		BaseURL: "http://localhost:8080",
	})
	assert.False(t, ws2.sessionStore.Options.Secure,
		"HTTP base URL should produce non-secure cookies")
}

func TestSetters(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	// Verify setters don't panic and fields are set
	oauthSvc := NewOAuthService(OAuthConfig{})
	ws.SetOAuthService(oauthSvc)
	assert.Equal(t, oauthSvc, ws.oauthService)

	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)
	assert.Equal(t, tokenSvc, ws.userTokenSvc)

	// SetStore with nil (should not panic)
	ws.SetStore(nil)
	assert.Nil(t, ws.store)
}
