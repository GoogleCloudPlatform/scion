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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/gorilla/securecookie"
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
	paths := []string{"/", "/projects", "/agents", "/projects/abc123", "/settings", "/not-a-real-page"}
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
	// Create a temporary directory with a test asset under assets/ subdirectory
	// to match the Vite build output structure (dist/client/assets/main.js).
	tmpDir := t.TempDir()
	assetsDir := filepath.Join(tmpDir, "assets")
	if err := os.MkdirAll(assetsDir, 0755); err != nil {
		t.Fatalf("failed to create assets dir: %v", err)
	}
	testContent := "console.log('test');"
	if err := os.WriteFile(filepath.Join(assetsDir, "main.js"), []byte(testContent), 0644); err != nil {
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
	assetsDir := filepath.Join(tmpDir, "assets")
	if err := os.MkdirAll(assetsDir, 0755); err != nil {
		t.Fatalf("failed to create assets dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(assetsDir, "chunk-abc12345.js"), []byte("// chunk"), 0644); err != nil {
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
	// Force the no-assets state regardless of whether web.AssetsEmbedded is true.
	// Without this, the embedded FS would be used and the test would only pass
	// if the embedded dist/client/ directory happens to lack the requested file.
	ws.assets = nil
	ws.assetsDisk = ""

	req := httptest.NewRequest("GET", "/assets/main.js", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404 when no assets available, got %d", resp.StatusCode)
	}
}

func TestSPAHandler_NoAssets_ServesErrorPage(t *testing.T) {
	ws := newDevAuthWebServer(t)
	ws.assets = nil
	ws.assetsDisk = ""

	handler := ws.Handler()

	paths := []string{"/", "/projects", "/agents", "/settings"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			resp := rec.Result()
			body, _ := io.ReadAll(resp.Body)
			html := string(body)

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
			assert.Contains(t, html, "Web UI Not Available")
			assert.Contains(t, html, "not built from source")
			assert.Contains(t, html, "Hub API")
			assert.NotContains(t, html, "scion-app",
				"should not render SPA shell when no assets are available")
			assert.NotContains(t, html, "main.js",
				"should not reference main.js when no assets are available")
		})
	}
}

func TestSPAHandler_NoAssets_HealthzStillWorks(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})
	ws.assets = nil
	ws.assetsDisk = ""

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var result CompositeHealthResponse
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "healthy", result.Status)
}

func TestSPAHandler_NoAssets_APIStillWorks(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})
	ws.assets = nil
	ws.assetsDisk = ""

	mockHub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	ws.MountHubAPI(mockHub, func(ctx context.Context) error { return nil })

	req := httptest.NewRequest("GET", "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "ok", result["status"])
}

func TestSPAHandler_WithAssets_ServesNormalShell(t *testing.T) {
	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) {
		cfg.AssetsDir = t.TempDir()
	})

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, html, "scion-app")
	assert.Contains(t, html, "main.js")
	assert.NotContains(t, html, "Web UI Not Available")
}

func TestRootLevelStaticFile_Disk(t *testing.T) {
	// Root-level public files (e.g. /scion-notification-icon.png) should be
	// served as static assets rather than falling through to the SPA shell.
	tmpDir := t.TempDir()
	iconContent := "fake-png-data"
	if err := os.WriteFile(filepath.Join(tmpDir, "scion-notification-icon.png"), []byte(iconContent), 0644); err != nil {
		t.Fatalf("failed to write test icon: %v", err)
	}

	ws := newTestWebServer(t, WebServerConfig{
		AssetsDir: tmpDir,
	})

	req := httptest.NewRequest("GET", "/scion-notification-icon.png", nil)
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 for root-level static file, got %d", resp.StatusCode)
	}
	if string(body) != iconContent {
		t.Errorf("expected icon content %q, got %q", iconContent, string(body))
	}
	// Should NOT be text/html (that would mean SPA handler served it)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") {
		t.Errorf("root-level static file should not be served as HTML, got Content-Type %q", ct)
	}
}

func TestRootLevelStaticFile_NonexistentFallsToSPA(t *testing.T) {
	// A root-level path with a file extension that doesn't match a real file
	// should fall through to the SPA shell (not serve a 404 from the static handler).
	tmpDir := t.TempDir()

	ws := newDevAuthWebServer(t, func(cfg *WebServerConfig) {
		cfg.AssetsDir = tmpDir
	})

	req := httptest.NewRequest("GET", "/nonexistent.png", nil)
	req.Header.Set("Authorization", "Bearer test-dev-token-12345")
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("non-existent root file should fall through to SPA shell (text/html), got Content-Type %q", ct)
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
		"X-XSS-Protection":       "1; mode=block",
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

	// Parse the composite response
	var result CompositeHealthResponse
	require.NoError(t, json.Unmarshal(body, &result))

	// Top-level backward-compatible fields
	assert.Equal(t, "healthy", result.Status)
	assert.NotEmpty(t, result.ScionVersion)
	assert.NotEmpty(t, result.Version)
	assert.NotEmpty(t, result.Uptime)

	// Web sub-object
	assert.NotNil(t, result.Web)
	webMap, ok := result.Web.(map[string]interface{})
	require.True(t, ok, "web should be a JSON object, got %T", result.Web)
	assert.Equal(t, "ok", webMap["status"])

	// No hub/broker in standalone mode
	assert.Nil(t, result.Hub)
	assert.Nil(t, result.Broker)
}

func TestWebHealthz_CompositeMode(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	// Register mock hub health provider
	ws.SetHubHealthProvider(func(ctx context.Context) interface{} {
		return &HealthResponse{
			Status:       "healthy",
			Version:      "0.1.0",
			ScionVersion: "abc1234",
			Uptime:       "5m0s",
			Checks:       map[string]string{"database": "healthy"},
			Stats:        &HealthStats{ConnectedBrokers: 1, ActiveAgents: 2, Projects: 3},
		}
	})

	// Register mock broker health provider
	type brokerHealth struct {
		Status  string            `json:"status"`
		Version string            `json:"version"`
		Uptime  string            `json:"uptime"`
		Checks  map[string]string `json:"checks,omitempty"`
	}
	ws.SetBrokerHealthProvider(func(ctx context.Context) interface{} {
		return &brokerHealth{
			Status:  "healthy",
			Version: "0.1.0",
			Uptime:  "5m0s",
			Checks:  map[string]string{"docker": "available"},
		}
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &result))

	// Top-level fields from hub health
	assert.Equal(t, "healthy", result["status"])
	assert.Equal(t, "0.1.0", result["version"])
	assert.Equal(t, "abc1234", result["scionVersion"])
	assert.Equal(t, "5m0s", result["uptime"])

	// Web sub-object
	webObj, ok := result["web"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "ok", webObj["status"])

	// Hub sub-object
	hubObj, ok := result["hub"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "healthy", hubObj["status"])
	hubChecks, _ := hubObj["checks"].(map[string]interface{})
	assert.Equal(t, "healthy", hubChecks["database"])
	hubStats, _ := hubObj["stats"].(map[string]interface{})
	assert.Equal(t, float64(1), hubStats["connectedBrokers"])

	// Broker sub-object
	brokerObj, ok := result["broker"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "healthy", brokerObj["status"])
	brokerChecks, _ := brokerObj["checks"].(map[string]interface{})
	assert.Equal(t, "available", brokerChecks["docker"])
}

func TestWebHealthz_DegradedHub(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	// Register a degraded hub health provider
	ws.SetHubHealthProvider(func(ctx context.Context) interface{} {
		return &HealthResponse{
			Status:       "degraded",
			Version:      "0.1.0",
			ScionVersion: "abc1234",
			Uptime:       "1m0s",
			Checks:       map[string]string{"database": "unhealthy"},
		}
	})

	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &result))

	// Top-level status should be degraded because hub is degraded
	assert.Equal(t, "degraded", result["status"])

	// Hub sub-object should show degraded
	hubObj, ok := result["hub"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "degraded", hubObj["status"])
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
		{"chunk-ab.js", false},      // hash too short
		{"chunk-ABCDEF12.js", true}, // uppercase hex
		{".js", false},              // no name
		{"no-extension", false},     // no extension
		{"name-ghijk.js", false},    // non-hex chars
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
	assetsDir := filepath.Join(tmpDir, "assets")
	require.NoError(t, os.MkdirAll(assetsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, "test.js"), []byte("//js"), 0644))

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

	req := httptest.NewRequest("GET", "/projects", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()

	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	location := resp.Header.Get("Location")
	assert.Equal(t, "/auth/login", location)
}

func TestSessionMiddleware_ProtectedAPI(t *testing.T) {
	// Unauthenticated non-browser request to a protected route should get 401 JSON
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/events", nil)
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

func TestMountHubAPI_RoutesToHub(t *testing.T) {
	// Mount a mock Hub handler on the WebServer and verify that
	// /api/v1/* requests are routed to it.
	ws := newDevAuthWebServer(t)

	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"source": "hub", "path": r.URL.Path})
	})
	ws.MountHubAPI(mockHandler, func(ctx context.Context) error { return nil })

	handler := ws.Handler()

	// /api/v1/projects should reach the Hub handler
	req := httptest.NewRequest("GET", "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "hub", result["source"])
	assert.Equal(t, "/api/v1/projects", result["path"])

	// /api/v1/agents should also reach the Hub handler
	req2 := httptest.NewRequest("GET", "/api/v1/agents", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Result().StatusCode)
}

func TestSessionToBearerMiddleware(t *testing.T) {
	// Verify that a session with a Hub JWT has the token injected
	// as an Authorization header when routed to the Hub handler.
	ws := newDevAuthWebServer(t)

	// Set up user token service for JWT generation
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)

	var capturedAuthHeader string
	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	ws.MountHubAPI(mockHandler, func(ctx context.Context) error { return nil })

	handler := ws.Handler()

	// First request: auto-login via dev-auth (establishes session with JWT)
	req1 := httptest.NewRequest("GET", "/api/v1/projects", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusOK, rec1.Result().StatusCode)
	assert.True(t, strings.HasPrefix(capturedAuthHeader, "Bearer "),
		"session-to-bearer should inject Authorization header, got %q", capturedAuthHeader)
}

func TestSessionToBearerMiddleware_NoToken(t *testing.T) {
	// Without a session, requests should pass through without an Authorization header.
	ws := newTestWebServer(t, WebServerConfig{})

	var capturedAuthHeader string
	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	ws.MountHubAPI(mockHandler, func(ctx context.Context) error { return nil })

	handler := ws.Handler()

	req := httptest.NewRequest("GET", "/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Result().StatusCode)
	assert.Empty(t, capturedAuthHeader, "no session = no Authorization header")
}

func TestDevAuthMiddleware_GeneratesHubTokens(t *testing.T) {
	// When userTokenSvc is available, dev-auth should generate Hub JWTs in the session.
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)

	ws := newDevAuthWebServer(t)
	ws.SetUserTokenService(tokenSvc)

	handler := ws.Handler()

	// Trigger dev auto-login
	req := httptest.NewRequest("GET", "/auth/me", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Result().StatusCode)

	// Verify the session cookie contains Hub tokens by making a second request
	// to an /api/v1/ route and checking the Authorization header is injected.
	var capturedAuth string
	mockHub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	ws.MountHubAPI(mockHub, func(ctx context.Context) error { return nil })

	req2 := httptest.NewRequest("GET", "/api/v1/test", nil)
	for _, c := range rec.Result().Cookies() {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.True(t, strings.HasPrefix(capturedAuth, "Bearer "),
		"dev-auth should generate Hub JWT, got Authorization: %q", capturedAuth)
}

func TestSessionToBearerMiddleware_SigningKeyRotation(t *testing.T) {
	// Simulate signing key rotation: establish a session with tokens signed
	// by key A, then rotate the server to key B. API requests should get a
	// 401 with code "session_expired" and the session should be cleared.
	oldSvc, err := NewUserTokenService(UserTokenConfig{
		SigningKey: []byte("old-signing-key-0123456789abcdef"),
	})
	require.NoError(t, err)

	ws := newDevAuthWebServer(t)
	ws.SetUserTokenService(oldSvc)

	var capturedAuthHeader string
	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	ws.MountHubAPI(mockHandler, func(ctx context.Context) error { return nil })

	handler := ws.Handler()

	// Step 1: Establish a session with tokens signed by the old key.
	req1 := httptest.NewRequest("GET", "/api/v1/projects", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Result().StatusCode)
	require.True(t, strings.HasPrefix(capturedAuthHeader, "Bearer "),
		"initial request should have a valid Bearer token")

	sessionCookies := rec1.Result().Cookies()

	// Step 2: Rotate the signing key.
	newSvc, err := NewUserTokenService(UserTokenConfig{
		SigningKey: []byte("new-signing-key-0123456789abcdef"),
	})
	require.NoError(t, err)
	ws.SetUserTokenService(newSvc)

	// Step 3: Make an API request with the old session cookie.
	capturedAuthHeader = ""
	req2 := httptest.NewRequest("GET", "/api/v1/projects", nil)
	req2.Header.Set("Accept", "text/html,application/xhtml+xml") // browser request
	for _, c := range sessionCookies {
		req2.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	// The middleware should detect the invalid token and return 401.
	assert.Equal(t, http.StatusUnauthorized, rec2.Result().StatusCode,
		"rotated signing key should result in 401")

	// Verify the response body contains session_expired error code.
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	err = json.NewDecoder(rec2.Body).Decode(&errResp)
	require.NoError(t, err)
	assert.Equal(t, "session_expired", errResp.Error.Code,
		"error code should be session_expired")

	// Step 4: Verify the session cookie was cleared (MaxAge=-1).
	// In a non-dev environment, the next page load would redirect to login
	// since the session user info was removed.
	var sessionCleared bool
	for _, c := range rec2.Result().Cookies() {
		if c.Name == "scion_sess" && c.MaxAge < 0 {
			sessionCleared = true
		}
	}
	assert.True(t, sessionCleared,
		"session cookie should be cleared (MaxAge=-1) after signing key rotation")
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
	assert.Equal(t, DevUserID, user1.UserID)
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
	assert.Equal(t, DevUserID, user2.UserID)
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
	assert.Equal(t, DevUserID, user.UserID)
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
		{"/scion-notification-icon.png", true},
		{"/robots.txt", true},
		{"/api/v1/projects", true},
		{"/api/v1/agents", true},
		{"/api/v1/auth/login", true},
		{"/", false},
		{"/projects", false},
		{"/agents", false},
		{"/settings", false},
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

func TestSessionStore_NoMaxLengthLimit(t *testing.T) {
	// The FilesystemStore stores data on disk, not in cookies, so the default
	// securecookie 4096-byte limit must be removed. JWT tokens in the session
	// regularly exceed that limit after gob+base64 encoding.
	ws := newTestWebServer(t, WebServerConfig{})
	for _, codec := range ws.sessionStore.Codecs {
		if sc, ok := codec.(*securecookie.SecureCookie); ok {
			// Encode a large value — if MaxLength were still 4096 this would fail.
			large := make(map[interface{}]interface{})
			large["token"] = string(make([]byte, 8000))
			_, err := securecookie.EncodeMulti("test", large, sc)
			assert.NoError(t, err, "session store should allow values larger than 4096 bytes")
		}
	}
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

	// SetEventPublisher
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	assert.Equal(t, pub, ws.events)
}

// --- SSE Endpoint Tests ---

func TestSSEHandler_RequiresSubParam(t *testing.T) {
	ws := newDevAuthWebServer(t)
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	t.Cleanup(func() { pub.Close() })

	req := httptest.NewRequest("GET", "/events", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "at least one sub parameter required")
}

func TestSSEHandler_InvalidSubject(t *testing.T) {
	ws := newDevAuthWebServer(t)
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	t.Cleanup(func() { pub.Close() })

	req := httptest.NewRequest("GET", "/events?sub=foo..bar", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "empty token")
}

func TestSSEHandler_NoPublisher(t *testing.T) {
	ws := newDevAuthWebServer(t)
	// Don't set publisher — events field remains nil

	req := httptest.NewRequest("GET", "/events?sub=project.test.>", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "event streaming not configured")
}

func TestSSEHandler_Headers(t *testing.T) {
	ws := newDevAuthWebServer(t)
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	t.Cleanup(func() { pub.Close() })

	// Use a test server so we get a real connection that supports streaming
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	// Make a request that will establish the SSE connection
	resp, err := http.Get(ts.URL + "/events?sub=project.test.>")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
}

func TestSSEHandler_EventDelivery(t *testing.T) {
	ws := newDevAuthWebServer(t)
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	t.Cleanup(func() { pub.Close() })

	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	// Start SSE connection in background
	resp, err := http.Get(ts.URL + "/events?sub=project.test123.>")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Publish events in a loop until the subscriber is ready.
	// The SSE handler goroutine may not have called Subscribe yet when
	// http.Get returns (it returns as soon as headers are flushed).
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pub.publish("project.test123.agent.status", AgentStatusEvent{
					AgentID: "agent-1",
					ProjectID: "test123",
					Phase:   "running",
				})
			case <-stop:
				return
			}
		}
	}()

	// Read SSE frames until we get the event (skip heartbeats).
	var frame string
	buf := make([]byte, 4096)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			close(stop)
			t.Fatal("timed out waiting for SSE event")
		default:
		}
		n, err := resp.Body.Read(buf)
		require.NoError(t, err)
		chunk := string(buf[:n])
		if strings.Contains(chunk, "event: update") {
			frame = chunk
			break
		}
	}
	close(stop)

	// Verify SSE frame format: event type is "update", subject is wrapped in data
	assert.Contains(t, frame, "event: update\n")
	assert.Contains(t, frame, "data: ")
	assert.Contains(t, frame, `"subject":"project.test123.agent.status"`)
	assert.Contains(t, frame, `"agentId":"agent-1"`)
	assert.Contains(t, frame, `"phase":"running"`)
}

func TestSSEHandler_SubjectValidation(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		valid   bool
	}{
		{"simple subject", "project.abc.status", true},
		{"wildcard star", "project.*.status", true},
		{"wildcard gt", "project.abc.>", true},
		{"single token", "project", true},
		{"with hyphens", "project.my-project.status", true},
		{"with underscores", "project.my_project.status", true},
		{"empty", "", false},
		{"empty token", "project..status", false},
		{"gt not last", "project.>.status", false},
		{"star mixed", "project.foo*bar.status", false},
		{"invalid char space", "project.foo bar", false},
		{"invalid char slash", "project/bar", false},
		{"too long", strings.Repeat("a", 257), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateSSESubjects([]string{tt.subject})
			if tt.valid {
				assert.Empty(t, result, "expected valid subject %q", tt.subject)
			} else {
				assert.NotEmpty(t, result, "expected invalid subject %q", tt.subject)
			}
		})
	}
}

func TestSSEHandler_RequiresAuth(t *testing.T) {
	// Without dev-auth, the SSE endpoint should require authentication
	ws := newTestWebServer(t, WebServerConfig{})
	pub := NewChannelEventPublisher()
	ws.SetEventPublisher(pub)
	t.Cleanup(func() { pub.Close() })

	// API-style request (no Accept: text/html) should get 401
	req := httptest.NewRequest("GET", "/events?sub=project.test.>", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestLoginPageRendersLoginComponent(t *testing.T) {
	// /login is a public route so no dev-auth needed
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/login", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, html, "scion-login-page")
	assert.NotContains(t, html, "<scion-app>")
}

func TestNonLoginPageRendersAppComponent(t *testing.T) {
	ws := newDevAuthWebServer(t)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, html, "<scion-app></scion-app>")
	assert.NotContains(t, html, "<scion-login-page")
}

func TestLoginPageNoOAuthAttributes(t *testing.T) {
	// After the provider-detection refactor, the login page template no longer
	// injects OAuth attributes — the component fetches them via /auth/providers.
	ws := newTestWebServer(t, WebServerConfig{})

	oauthSvc := NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "test-google-id",
				ClientSecret: "test-google-secret",
			},
		},
	})
	ws.SetOAuthService(oauthSvc)

	req := httptest.NewRequest("GET", "/login", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, html, "<scion-login-page></scion-login-page>")
	assert.NotContains(t, html, "googleEnabled")
	assert.NotContains(t, html, "githubEnabled")
}

func TestAuthProviders_NoOAuthService(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})

	req := httptest.NewRequest("GET", "/auth/providers", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var result map[string]bool
	require.NoError(t, json.Unmarshal(body, &result))
	assert.False(t, result["google"])
	assert.False(t, result["github"])
}

func TestAuthProviders_WithProviders(t *testing.T) {
	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetOAuthService(NewOAuthService(OAuthConfig{
		Web: OAuthClientConfig{
			Google: OAuthProviderConfig{
				ClientID:     "g-id",
				ClientSecret: "g-secret",
			},
		},
	}))

	req := httptest.NewRequest("GET", "/auth/providers", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)

	resp := rec.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	var result map[string]bool
	require.NoError(t, json.Unmarshal(body, &result))
	assert.True(t, result["google"])
	assert.False(t, result["github"])
}

// --- SSR Prefetch Tests ---

func TestSafeJSONForHTML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no special chars",
			input:    `{"key":"value"}`,
			expected: `{"key":"value"}`,
		},
		{
			name:     "script close tag",
			input:    `{"html":"</script>"}`,
			expected: `{"html":"<\/script>"}`,
		},
		{
			name:     "html comment",
			input:    `{"html":"<!-- comment -->"}`,
			expected: `{"html":"<\!-- comment -->"}`,
		},
		{
			name:     "multiple occurrences",
			input:    `</script></style><!--x-->`,
			expected: `<\/script><\/style><\!--x-->`,
		},
		{
			name:     "no false positives",
			input:    `{"path":"/api/v1/agents","count":42}`,
			expected: `{"path":"/api/v1/agents","count":42}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeJSONForHTML(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestResolveAPIPath(t *testing.T) {
	tests := []struct {
		urlPath  string
		expected string
	}{
		{"/agents", "/api/v1/agents"},
		{"/agents/", "/api/v1/agents"},
		{"/projects", "/api/v1/projects"},
		{"/projects/", "/api/v1/projects"},
		{"/agents/abc123", "/api/v1/agents/abc123"},
		{"/projects/my-project", "/api/v1/projects/my-project"},
		{"/", ""},
		{"/login", ""},
		{"/settings", ""},
		{"/admin/users", ""},
		{"/agents/abc/terminal", ""}, // too many segments
		{"/projects/abc/settings", ""}, // too many segments
	}

	for _, tt := range tests {
		t.Run(tt.urlPath, func(t *testing.T) {
			got := resolveAPIPath(tt.urlPath)
			assert.Equal(t, tt.expected, got, "resolveAPIPath(%q)", tt.urlPath)
		})
	}
}

func TestSPAShellHandler_ContainsInitialData(t *testing.T) {
	ws := newDevAuthWebServer(t)

	// Set up user token service so dev-auth generates Hub JWTs
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)

	// Mount a mock Hub handler that returns agent data with _capabilities
	mockHub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"agents": []map[string]interface{}{
				{
					"id":     "agent-1",
					"name":   "test-agent",
					"status": "running",
					"_capabilities": map[string]interface{}{
						"actions": []string{"start", "stop", "delete"},
					},
				},
			},
			"_capabilities": map[string]interface{}{
				"actions": []string{"create", "list"},
			},
		})
	})
	ws.MountHubAPI(mockHub, func(ctx context.Context) error { return nil })

	handler := ws.Handler()

	// Request the agents page
	req := httptest.NewRequest("GET", "/agents", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// The __SCION_DATA__ should contain agent data
	assert.Contains(t, html, `"agent-1"`)
	assert.Contains(t, html, `"test-agent"`)
	assert.Contains(t, html, `"_capabilities"`)
	assert.Contains(t, html, `"actions"`)

	// Verify it's valid JSON by extracting and parsing
	dataStart := strings.Index(html, `type="application/json">`) + len(`type="application/json">`)
	dataEnd := strings.Index(html[dataStart:], `</script>`)
	require.True(t, dataStart > 0 && dataEnd > 0, "should find __SCION_DATA__ boundaries")

	jsonData := html[dataStart : dataStart+dataEnd]
	var pageData map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(jsonData), &pageData), "initial data should be valid JSON")

	assert.Equal(t, "/agents", pageData["path"])
	assert.NotNil(t, pageData["data"], "data field should be present")
	assert.NotNil(t, pageData["user"], "user field should be present")
}

func TestSPAShellHandler_UserInInitialData(t *testing.T) {
	ws := newDevAuthWebServer(t)
	handler := ws.Handler()

	// Request the home page (no API prefetch for /)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Extract and parse __SCION_DATA__
	dataStart := strings.Index(html, `type="application/json">`) + len(`type="application/json">`)
	dataEnd := strings.Index(html[dataStart:], `</script>`)
	require.True(t, dataStart > 0 && dataEnd > 0)

	jsonData := html[dataStart : dataStart+dataEnd]
	var pageData map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(jsonData), &pageData))

	// User info should be present even without API prefetch
	userObj, ok := pageData["user"].(map[string]interface{})
	require.True(t, ok, "user should be a JSON object")
	assert.Equal(t, DevUserID, userObj["id"])
	assert.Equal(t, "dev@localhost", userObj["email"])
	assert.Equal(t, "Development User", userObj["name"])
	assert.Equal(t, "admin", userObj["role"])

	// No API data for the home page
	assert.Nil(t, pageData["data"])
}

func TestSPAShellHandler_NoHubMounted(t *testing.T) {
	ws := newDevAuthWebServer(t)
	// Do NOT mount a Hub handler
	handler := ws.Handler()

	// Request the agents page — should still render with user info
	req := httptest.NewRequest("GET", "/agents", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Extract and parse __SCION_DATA__
	dataStart := strings.Index(html, `type="application/json">`) + len(`type="application/json">`)
	dataEnd := strings.Index(html[dataStart:], `</script>`)
	require.True(t, dataStart > 0 && dataEnd > 0)

	jsonData := html[dataStart : dataStart+dataEnd]
	var pageData map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(jsonData), &pageData))

	// User should be present (dev-auth)
	assert.NotNil(t, pageData["user"])

	// No API data since Hub is not mounted
	assert.Nil(t, pageData["data"])
}

func TestSPAShellHandler_HubAPIError(t *testing.T) {
	ws := newDevAuthWebServer(t)

	// Set up user token service so dev-auth generates Hub JWTs
	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	ws.SetUserTokenService(tokenSvc)

	// Mount a Hub handler that returns 500
	mockHub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "database down"})
	})
	ws.MountHubAPI(mockHub, func(ctx context.Context) error { return nil })

	handler := ws.Handler()

	// Request agents page
	req := httptest.NewRequest("GET", "/agents", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	// Page should still render (200 OK)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Extract and parse __SCION_DATA__
	dataStart := strings.Index(html, `type="application/json">`) + len(`type="application/json">`)
	dataEnd := strings.Index(html[dataStart:], `</script>`)
	require.True(t, dataStart > 0 && dataEnd > 0)

	jsonData := html[dataStart : dataStart+dataEnd]
	var pageData map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(jsonData), &pageData))

	// User should still be present (graceful fallback)
	assert.NotNil(t, pageData["user"])

	// No API data because the Hub returned an error
	assert.Nil(t, pageData["data"])
}
