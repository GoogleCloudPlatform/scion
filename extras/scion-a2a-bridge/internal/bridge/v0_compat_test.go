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

package bridge

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// v0StubHandler is a minimal HTTP handler that records the stripped path it
// receives, so tests can verify that the bridge correctly strips the
// per-agent prefix before delegating to the v0.3 REST handler.
type v0StubHandler struct {
	lastPath   string
	lastMethod string
	called     bool
}

func (h *v0StubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.lastPath = r.URL.Path
	h.lastMethod = r.Method
	h.called = true
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"path":   r.URL.Path,
		"method": r.Method,
	})
}

func newV0TestServer(t *testing.T, scheme, apiKey string, v0handler http.Handler) (*Server, *Config) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://bridge.example.com"},
		Hub:    HubConfig{Endpoint: "https://hub.example.com", User: "admin@test"},
		Auth:   AuthConfig{Scheme: scheme, APIKey: apiKey},
		Projects: []ProjectConfig{
			{Slug: "proj1", ExposedAgents: []string{"agent1", "agent2"}},
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)
	srv := NewServer(b, cfg, nil, log, testHandler())
	if v0handler != nil {
		srv.SetV0RESTHandler(v0handler)
	}
	return srv, cfg
}

// ---------------------------------------------------------------------------
// Route registration — v0.3 REST routes are registered when handler is set
// ---------------------------------------------------------------------------

func TestV0REST_RouteRegistration(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantPath   string // expected stripped path seen by v0 handler
	}{
		{
			name:       "message:send",
			method:     "POST",
			path:       "/projects/proj1/agents/agent1/message:send",
			wantStatus: http.StatusOK,
			wantPath:   "/message:send",
		},
		{
			name:       "message:stream",
			method:     "POST",
			path:       "/projects/proj1/agents/agent1/message:stream",
			wantStatus: http.StatusOK,
			wantPath:   "/message:stream",
		},
		{
			name:       "tasks list",
			method:     "GET",
			path:       "/projects/proj1/agents/agent1/tasks",
			wantStatus: http.StatusOK,
			wantPath:   "/tasks",
		},
		{
			name:       "tasks get by id",
			method:     "GET",
			path:       "/projects/proj1/agents/agent1/tasks/task-123",
			wantStatus: http.StatusOK,
			wantPath:   "/tasks/task-123",
		},
		{
			name:       "tasks cancel",
			method:     "POST",
			path:       "/projects/proj1/agents/agent1/tasks/task-123:cancel",
			wantStatus: http.StatusOK,
			wantPath:   "/tasks/task-123:cancel",
		},
		{
			name:       "extendedAgentCard",
			method:     "GET",
			path:       "/projects/proj1/agents/agent1/extendedAgentCard",
			wantStatus: http.StatusOK,
			wantPath:   "/extendedAgentCard",
		},
		{
			name:       "legacy grove path",
			method:     "POST",
			path:       "/groves/proj1/agents/agent1/message:send",
			wantStatus: http.StatusOK,
			wantPath:   "/message:send",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub.called = false
			stub.lastPath = ""
			stub.lastMethod = ""

			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if !stub.called {
				t.Fatal("v0 handler was not called")
			}
			if stub.lastPath != tt.wantPath {
				t.Errorf("stripped path = %q, want %q", stub.lastPath, tt.wantPath)
			}
			if stub.lastMethod != tt.method {
				t.Errorf("method = %q, want %q", stub.lastMethod, tt.method)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Existing routes take precedence over v0.3 catch-all
// ---------------------------------------------------------------------------

func TestV0REST_ExistingRoutePrecedence(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	// JSON-RPC route should NOT go to v0 handler.
	stub.called = false
	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if stub.called {
		t.Error("v0 handler should NOT be called for /jsonrpc — existing route should take precedence")
	}

	// Agent card route should NOT go to v0 handler.
	stub.called = false
	req = httptest.NewRequest("GET", "/projects/proj1/agents/agent1/.well-known/agent-card.json", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if stub.called {
		t.Error("v0 handler should NOT be called for agent card — existing route should take precedence")
	}
}

// ---------------------------------------------------------------------------
// Auth middleware protects v0.3 routes
// ---------------------------------------------------------------------------

func TestV0REST_AuthProtection(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "apiKey", "secret-key", stub)
	handler := srv.Handler()

	// Without API key: rejected.
	stub.called = false
	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without API key, got %d", w.Code)
	}
	if stub.called {
		t.Error("v0 handler should not be called when auth fails")
	}

	// With valid API key: accepted.
	stub.called = false
	req = httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send", nil)
	req.Header.Set("X-API-Key", "secret-key")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with valid API key, got %d", w.Code)
	}
	if !stub.called {
		t.Error("v0 handler should be called when auth succeeds")
	}
}

// ---------------------------------------------------------------------------
// Slug validation
// ---------------------------------------------------------------------------

func TestV0REST_InvalidSlug(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	// Invalid project slug.
	req := httptest.NewRequest("POST", "/projects/INVALID_SLUG!/agents/agent1/message:send", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if stub.called {
		t.Error("v0 handler should not be called for invalid slug")
	}
}

// ---------------------------------------------------------------------------
// Unexposed agent
// ---------------------------------------------------------------------------

func TestV0REST_UnexposedAgent(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	// agent3 is not in the exposed list.
	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent3/message:send", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unexposed agent, got %d", w.Code)
	}
	if stub.called {
		t.Error("v0 handler should not be called for unexposed agent")
	}
}

// ---------------------------------------------------------------------------
// No v0 handler configured — routes not registered
// ---------------------------------------------------------------------------

func TestV0REST_NotConfigured(t *testing.T) {
	srv, _ := newV0TestServer(t, "none", "", nil) // nil v0 handler
	handler := srv.Handler()

	// Without v0 handler, /message:send should be 404 (no catch-all registered).
	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Go's ServeMux returns 405 Method Not Allowed or 404 for unregistered paths.
	// The exact status depends on whether any pattern partially matches.
	if w.Code == http.StatusOK {
		t.Errorf("expected non-200 when v0 handler is not configured, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// RouteInfo context injection
// ---------------------------------------------------------------------------

func TestV0REST_RouteInfoInjected(t *testing.T) {
	// Use a handler that checks for RouteInfo in the context.
	var capturedRouteInfo *RouteInfo
	routeCapture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ri, ok := RouteInfoFrom(r.Context())
		if ok {
			capturedRouteInfo = &ri
		}
		w.WriteHeader(http.StatusOK)
	})

	srv, _ := newV0TestServer(t, "none", "", routeCapture)
	handler := srv.Handler()

	capturedRouteInfo = nil
	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if capturedRouteInfo == nil {
		t.Fatal("RouteInfo not injected into context")
	}
	if capturedRouteInfo.ProjectSlug != "proj1" {
		t.Errorf("ProjectSlug = %q, want %q", capturedRouteInfo.ProjectSlug, "proj1")
	}
	if capturedRouteInfo.AgentSlug != "agent1" {
		t.Errorf("AgentSlug = %q, want %q", capturedRouteInfo.AgentSlug, "agent1")
	}
}

// ---------------------------------------------------------------------------
// Agent card includes v0.3 REST interface
// ---------------------------------------------------------------------------

func TestV0REST_AgentCardDualFormat(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/projects/proj1/agents/agent1/.well-known/agent-card.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("agent card status = %d, want 200", w.Code)
	}

	var card map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatalf("failed to parse agent card: %v", err)
	}

	// Check supportedInterfaces includes both v1.0 JSON-RPC and v0.3 REST.
	ifaces, ok := card["supportedInterfaces"].([]interface{})
	if !ok {
		t.Fatal("supportedInterfaces not found or wrong type")
	}
	if len(ifaces) < 2 {
		t.Fatalf("expected at least 2 interfaces, got %d", len(ifaces))
	}

	var foundV1JSONRPC, foundV03REST bool
	for _, iface := range ifaces {
		m := iface.(map[string]interface{})
		binding := m["protocolBinding"].(string)
		version := m["protocolVersion"].(string)
		if binding == "JSONRPC" && version == "1.0" {
			foundV1JSONRPC = true
		}
		if binding == "REST" && version == "0.3" {
			foundV03REST = true
			// v0.3 REST URL should be the agent base URL (no /jsonrpc suffix).
			url := m["url"].(string)
			if strings.HasSuffix(url, "/jsonrpc") {
				t.Errorf("v0.3 REST URL should not end with /jsonrpc, got %q", url)
			}
		}
	}
	if !foundV1JSONRPC {
		t.Error("missing v1.0 JSONRPC interface in agent card")
	}
	if !foundV03REST {
		t.Error("missing v0.3 REST interface in agent card")
	}

	// Check v0.3 compat flat fields.
	if pv, ok := card["protocolVersion"].(string); !ok || pv != "0.3" {
		t.Errorf("protocolVersion = %q, want %q", pv, "0.3")
	}
	if pt, ok := card["preferredTransport"].(string); !ok || pt != "REST" {
		t.Errorf("preferredTransport = %q, want %q", pt, "REST")
	}
}

// ---------------------------------------------------------------------------
// Query string preservation
// ---------------------------------------------------------------------------

func TestV0REST_QueryStringPreserved(t *testing.T) {
	var capturedQuery string
	queryCapture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	})

	srv, _ := newV0TestServer(t, "none", "", queryCapture)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/projects/proj1/agents/agent1/tasks?historyLength=5&contextId=ctx-1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if capturedQuery != "historyLength=5&contextId=ctx-1" {
		t.Errorf("query string = %q, want %q", capturedQuery, "historyLength=5&contextId=ctx-1")
	}
}
