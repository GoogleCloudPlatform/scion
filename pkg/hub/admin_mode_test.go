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
	"bytes"
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

// --- MaintenanceState unit tests ---

func TestMaintenanceState_Defaults(t *testing.T) {
	ms := NewMaintenanceState(false, "")
	if ms.IsEnabled() {
		t.Error("expected disabled by default")
	}
	if ms.Message() != defaultMaintenanceMessage {
		t.Errorf("expected default message, got %q", ms.Message())
	}
}

func TestMaintenanceState_SetEnabled(t *testing.T) {
	ms := NewMaintenanceState(false, "")
	ms.SetEnabled(true)
	if !ms.IsEnabled() {
		t.Error("expected enabled after SetEnabled(true)")
	}
	ms.SetEnabled(false)
	if ms.IsEnabled() {
		t.Error("expected disabled after SetEnabled(false)")
	}
}

func TestMaintenanceState_SetMessage(t *testing.T) {
	ms := NewMaintenanceState(false, "")
	ms.SetMessage("custom msg")
	if ms.Message() != "custom msg" {
		t.Errorf("expected 'custom msg', got %q", ms.Message())
	}
}

func TestMaintenanceState_Set(t *testing.T) {
	ms := NewMaintenanceState(false, "")
	ms.Set(true, "both updated")
	if !ms.IsEnabled() {
		t.Error("expected enabled")
	}
	if ms.Message() != "both updated" {
		t.Errorf("expected 'both updated', got %q", ms.Message())
	}
}

// --- Hub API middleware tests ---

func TestAdminModeMiddleware_Disabled(t *testing.T) {
	// When maintenance is disabled, requests pass through.
	state := NewMaintenanceState(false, "")
	mw := adminModeMiddleware(state)(passthrough)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 when disabled, got %d", rr.Code)
	}
}

func TestAdminModeMiddleware_AdminUser(t *testing.T) {
	state := NewMaintenanceState(true, "")
	mw := adminModeMiddleware(state)(passthrough)

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
	state := NewMaintenanceState(true, "")
	mw := adminModeMiddleware(state)(passthrough)

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
	state := NewMaintenanceState(true, "")
	mw := adminModeMiddleware(state)(passthrough)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unauthenticated should get 503, got %d", rr.Code)
	}
}

func TestAdminModeMiddleware_AgentIdentity(t *testing.T) {
	state := NewMaintenanceState(true, "")
	mw := adminModeMiddleware(state)(passthrough)

	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:  jwt.Claims{Subject: "agent-1"},
		ProjectID: "project-1",
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
	state := NewMaintenanceState(true, "")
	mw := adminModeMiddleware(state)(passthrough)

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
	state := NewMaintenanceState(true, customMsg)
	mw := adminModeMiddleware(state)(passthrough)

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

func TestAdminModeMiddleware_RuntimeToggle(t *testing.T) {
	// Start with maintenance off, toggle on mid-flight.
	state := NewMaintenanceState(false, "")
	mw := adminModeMiddleware(state)(passthrough)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	rr := httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 when disabled, got %d", rr.Code)
	}

	// Enable maintenance mode at runtime.
	state.SetEnabled(true)

	rr = httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after enabling, got %d", rr.Code)
	}

	// Disable again.
	state.SetEnabled(false)

	rr = httptest.NewRecorder()
	mw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after disabling, got %d", rr.Code)
	}
}

// --- Web server middleware tests ---

func newTestWebServerWithMaintenance(enabled bool, message string) *WebServer {
	ws := &WebServer{
		config:      WebServerConfig{},
		mux:         http.NewServeMux(),
		maintenance: NewMaintenanceState(enabled, message),
	}
	return ws
}

func TestAdminModeWebMiddleware_AdminPassesThrough(t *testing.T) {
	ws := newTestWebServerWithMaintenance(true, "")
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
	ws := newTestWebServerWithMaintenance(true, "")
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
	ws := newTestWebServerWithMaintenance(true, "")
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
	ws := newTestWebServerWithMaintenance(true, "")
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
	ws := newTestWebServerWithMaintenance(true, "")
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
	ws := newTestWebServerWithMaintenance(true, customMsg)
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
	ws := newTestWebServerWithMaintenance(true, "")
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
	ws := newTestWebServerWithMaintenance(true, "")
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

func TestAdminModeWebMiddleware_NilState(t *testing.T) {
	// When no MaintenanceState is set, everything passes through.
	ws := &WebServer{config: WebServerConfig{}, mux: http.NewServeMux()}
	ws.mux.HandleFunc("/", passthrough)

	handler := ws.adminModeWebMiddleware(ws.mux)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("nil maintenance state should pass through, got %d", rr.Code)
	}
}

// --- HTML page tests ---

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

// --- API handler tests ---

func TestHandleAdminMaintenance_Get(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(false, "test message"),
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var body map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&body)
	if body["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", body["enabled"])
	}
	if body["message"] != "test message" {
		t.Errorf("expected message='test message', got %v", body["message"])
	}
}

func TestHandleAdminMaintenance_Put(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	payload := `{"enabled": true, "message": "going down"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/maintenance", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var body map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&body)
	if body["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", body["enabled"])
	}
	if body["message"] != "going down" {
		t.Errorf("expected message='going down', got %v", body["message"])
	}

	// Verify state was actually updated.
	if !srv.maintenance.IsEnabled() {
		t.Error("maintenance should be enabled after PUT")
	}
}

func TestHandleAdminMaintenance_NonAdmin(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	user := NewAuthenticatedUser("u2", "user@example.com", "User", "member", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), user))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin should get 403, got %d", rr.Code)
	}
}

func TestHandleAdminMaintenance_Unauthenticated(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance", nil)
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated should get 403, got %d", rr.Code)
	}
}

func TestHandleAdminMaintenance_MethodNotAllowed(t *testing.T) {
	srv := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}

	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/maintenance", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenance(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// setWebSessionUser is a test helper to set the web session user in context.
func setWebSessionUser(ctx context.Context, user *webSessionUser) context.Context {
	return context.WithValue(ctx, webUserContextKey{}, user)
}
