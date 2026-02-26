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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-jose/go-jose/v4/jwt"
)

// passthrough is a simple handler that writes 200 OK.
var passthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
})

// --- Hub API middleware tests ---

func TestAdminModeMiddleware_Disabled(t *testing.T) {
	// When admin mode is not applied, requests pass through.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	passthrough.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestAdminModeMiddleware_AdminUser(t *testing.T) {
	mw := adminModeMiddleware("")(passthrough)

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin user should pass through, got %d", rr.Code)
	}
}

func TestAdminModeMiddleware_NonAdminUser(t *testing.T) {
	mw := adminModeMiddleware("")(passthrough)

	user := NewAuthenticatedUser("u2", "user@example.com", "User", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), user))
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-admin user should get 503, got %d", rr.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if body["error"] != "system_maintenance" {
		t.Errorf("expected error=system_maintenance, got %q", body["error"])
	}
	if body["message"] != defaultMaintenanceMessage {
		t.Errorf("expected default message, got %q", body["message"])
	}
}

func TestAdminModeMiddleware_Unauthenticated(t *testing.T) {
	mw := adminModeMiddleware("")(passthrough)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unauthenticated should get 503, got %d", rr.Code)
	}
}

func TestAdminModeMiddleware_AgentIdentity(t *testing.T) {
	mw := adminModeMiddleware("")(passthrough)

	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:  jwt.Claims{Subject: "agent-1"},
		GroveID: "grove-1",
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), agent))
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("agent identity should pass through, got %d", rr.Code)
	}
}

func TestAdminModeMiddleware_BrokerIdentity(t *testing.T) {
	mw := adminModeMiddleware("")(passthrough)

	broker := NewBrokerIdentity("broker-1")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	ctx := contextWithIdentity(req.Context(), broker)
	ctx = contextWithBrokerIdentity(ctx, broker)
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("broker identity should pass through, got %d", rr.Code)
	}
}

func TestAdminModeMiddleware_CustomMessage(t *testing.T) {
	customMsg := "We are upgrading the system"
	mw := adminModeMiddleware(customMsg)(passthrough)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}
	if body["message"] != customMsg {
		t.Errorf("expected custom message %q, got %q", customMsg, body["message"])
	}
}

// --- Web server middleware tests ---

func TestAdminModeWebMiddleware_AdminPassesThrough(t *testing.T) {
	// When admin mode is on, admin users pass through to the inner handler.
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/dashboard", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	user := &webSessionUser{UserID: "admin1", Role: "admin"}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req = req.WithContext(setWebSessionUser(req.Context(), user))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin user should pass through, got %d", rr.Code)
	}
}

func TestAdminModeWebMiddleware_AuthRoutes(t *testing.T) {
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/auth/login/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ws.mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ws.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := ws.adminModeWebMiddleware(ws.mux)

	for _, path := range []string{"/auth/login/google", "/login", "/healthz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code == http.StatusServiceUnavailable {
			t.Errorf("path %s should not be blocked in admin mode, got 503", path)
		}
	}
}

func TestAdminModeWebMiddleware_NonAdminUser(t *testing.T) {
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	// Non-admin user in context
	user := &webSessionUser{UserID: "u1", Role: "member"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	ctx := req.Context()
	ctx = setWebSessionUser(ctx, user)
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-admin web user should get 503, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "Under Maintenance") {
		t.Error("expected maintenance page HTML")
	}
	if !strings.Contains(body, defaultMaintenanceMessage) {
		t.Errorf("expected default maintenance message in HTML body")
	}
}

func TestAdminModeWebMiddleware_AdminUser(t *testing.T) {
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	user := &webSessionUser{UserID: "u1", Role: "admin"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := setWebSessionUser(req.Context(), user)
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin web user should pass through, got %d", rr.Code)
	}
}

func TestAdminModeWebMiddleware_Unauthenticated(t *testing.T) {
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unauthenticated web request should get 503, got %d", rr.Code)
	}
}

func TestAdminModeWebMiddleware_CustomMessage(t *testing.T) {
	customMsg := "Back in 30 minutes"
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true, MaintenanceMessage: customMsg},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, customMsg) {
		t.Errorf("expected custom message %q in HTML body", customMsg)
	}
}

func TestAdminModeWebMiddleware_APIRoutesPassThrough(t *testing.T) {
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/api/v1/agents", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("API routes should pass through web admin mode middleware, got %d", rr.Code)
	}
}

func TestAdminModeWebMiddleware_StaticAssetsPassThrough(t *testing.T) {
	ws := &WebServer{
		config: WebServerConfig{AdminMode: true},
		mux:    http.NewServeMux(),
	}
	ws.mux.HandleFunc("/assets/main.js", passthrough)
	ws.mux.HandleFunc("/favicon.ico", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	for _, path := range []string{"/assets/main.js", "/favicon.ico"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("path %s should pass through in admin mode, got %d", path, rr.Code)
		}
	}
}

func TestMaintenancePageHTML_EscapesMessage(t *testing.T) {
	msg := `<script>alert("xss")</script>`
	html := maintenancePageHTML(msg)

	if strings.Contains(html, "<script>alert") {
		t.Error("message should be HTML-escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("expected HTML-escaped script tag")
	}
}

// setWebSessionUser is a test helper to set the web session user in context.
func setWebSessionUser(ctx context.Context, user *webSessionUser) context.Context {
	return context.WithValue(ctx, webUserContextKey{}, user)
}
