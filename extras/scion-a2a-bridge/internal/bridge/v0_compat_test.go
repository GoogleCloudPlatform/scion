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

// ===========================================================================
// Required #8: GE JSON-RPC wire compatibility proof.
//
// These tests exercise actual A2A operation payloads through production routes
// to verify wire-level compatibility: JSON-RPC envelope parsing, v0.3 REST
// body forwarding, discovery aliases, and multi-turn cursor context.
// ===========================================================================

// ---------------------------------------------------------------------------
// JSON-RPC v1.0 — message/send, message/stream, tasks/get, tasks/cancel,
// tasks/resubscribe through /jsonrpc endpoint
// ---------------------------------------------------------------------------

func TestJSONRPC_WireFormat_MessageSend(t *testing.T) {
	var capturedBody json.RawMessage
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		// Echo a valid JSON-RPC response.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"req-1","result":{"id":"task-001","contextId":"ctx-001","status":{"state":"completed"},"artifacts":[{"parts":[{"type":"text","text":"hello"}]}]}}`))
	})

	srv, _ := newV0TestServer(t, "none", "", nil)
	// Override the SDK handler for JSON-RPC testing.
	srv.SetSDKHandler(sdkHandler)
	handler := srv.Handler()

	// A2A v1.0 message/send JSON-RPC envelope.
	payload := `{
		"jsonrpc": "2.0",
		"id": "req-1",
		"method": "message/send",
		"params": {
			"message": {
				"role": "user",
				"parts": [{"type": "text", "text": "Hello, agent!"}]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// Verify the JSON-RPC envelope was forwarded to the SDK handler.
	var envelope map[string]interface{}
	if err := json.Unmarshal(capturedBody, &envelope); err != nil {
		t.Fatalf("SDK handler received invalid JSON: %v", err)
	}
	if envelope["method"] != "message/send" {
		t.Errorf("method = %v, want message/send", envelope["method"])
	}
	if envelope["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", envelope["jsonrpc"])
	}

	// Verify the response is valid JSON-RPC.
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is invalid JSON: %v", err)
	}
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("response jsonrpc = %v, want 2.0", resp["jsonrpc"])
	}
	result := resp["result"].(map[string]interface{})
	if result["contextId"] != "ctx-001" {
		t.Errorf("contextId = %v, want ctx-001", result["contextId"])
	}
}

func TestJSONRPC_WireFormat_MessageStream(t *testing.T) {
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope map[string]interface{}
		json.Unmarshal(body, &envelope)
		if envelope["method"] != "message/stream" {
			t.Errorf("method = %v, want message/stream", envelope["method"])
		}
		// Simulate SSE streaming response.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"req-2\",\"result\":{\"id\":\"task-002\",\"status\":{\"state\":\"working\"}}}\n\n"))
	})

	srv, _ := newV0TestServer(t, "none", "", nil)
	srv.SetSDKHandler(sdkHandler)
	handler := srv.Handler()

	payload := `{
		"jsonrpc": "2.0",
		"id": "req-2",
		"method": "message/stream",
		"params": {
			"message": {
				"role": "user",
				"parts": [{"type": "text", "text": "stream this"}]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestJSONRPC_WireFormat_TasksGet(t *testing.T) {
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope map[string]interface{}
		json.Unmarshal(body, &envelope)
		if envelope["method"] != "tasks/get" {
			t.Errorf("method = %v, want tasks/get", envelope["method"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"req-3","result":{"id":"task-001","status":{"state":"completed"}}}`))
	})

	srv, _ := newV0TestServer(t, "none", "", nil)
	srv.SetSDKHandler(sdkHandler)
	handler := srv.Handler()

	payload := `{
		"jsonrpc": "2.0",
		"id": "req-3",
		"method": "tasks/get",
		"params": {"id": "task-001"}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestJSONRPC_WireFormat_TasksCancel(t *testing.T) {
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope map[string]interface{}
		json.Unmarshal(body, &envelope)
		if envelope["method"] != "tasks/cancel" {
			t.Errorf("method = %v, want tasks/cancel", envelope["method"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"req-4","result":{"id":"task-001","status":{"state":"canceled"}}}`))
	})

	srv, _ := newV0TestServer(t, "none", "", nil)
	srv.SetSDKHandler(sdkHandler)
	handler := srv.Handler()

	payload := `{
		"jsonrpc": "2.0",
		"id": "req-4",
		"method": "tasks/cancel",
		"params": {"id": "task-001"}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestJSONRPC_WireFormat_TasksResubscribe(t *testing.T) {
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope map[string]interface{}
		json.Unmarshal(body, &envelope)
		if envelope["method"] != "tasks/resubscribe" {
			t.Errorf("method = %v, want tasks/resubscribe", envelope["method"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"req-5\"}\n\n"))
	})

	srv, _ := newV0TestServer(t, "none", "", nil)
	srv.SetSDKHandler(sdkHandler)
	handler := srv.Handler()

	payload := `{
		"jsonrpc": "2.0",
		"id": "req-5",
		"method": "tasks/resubscribe",
		"params": {"id": "task-001"}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC — discovery aliases (/groves/ ↔ /projects/)
// ---------------------------------------------------------------------------

func TestJSONRPC_DiscoveryAlias_GrovesPath(t *testing.T) {
	var capturedBody json.RawMessage
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"req-grove","result":{}}`))
	})

	srv, _ := newV0TestServer(t, "none", "", nil)
	srv.SetSDKHandler(sdkHandler)
	handler := srv.Handler()

	payload := `{"jsonrpc":"2.0","id":"req-grove","method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"via grove"}]}}}`

	// Use /groves/ path instead of /projects/.
	req := httptest.NewRequest("POST", "/groves/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var envelope map[string]interface{}
	if err := json.Unmarshal(capturedBody, &envelope); err != nil {
		t.Fatalf("SDK handler received invalid JSON via /groves/: %v", err)
	}
	if envelope["method"] != "message/send" {
		t.Errorf("method = %v, want message/send (via /groves/ alias)", envelope["method"])
	}
}

func TestJSONRPC_DiscoveryAlias_AgentCard(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	// Agent card via /groves/ should work.
	req := httptest.NewRequest("GET", "/groves/proj1/agents/agent1/.well-known/agent-card.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("agent card via /groves/ status = %d, want 200", w.Code)
	}

	var card map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatalf("failed to parse agent card: %v", err)
	}
	if card["name"] == nil {
		t.Error("agent card missing 'name' field")
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC — multi-turn cursor (contextId tracking)
// ---------------------------------------------------------------------------

func TestJSONRPC_MultiTurnCursor_ContextIdPreserved(t *testing.T) {
	var capturedContextID string
	sdkHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope map[string]interface{}
		json.Unmarshal(body, &envelope)

		params := envelope["params"].(map[string]interface{})
		if cid, ok := params["contextId"]; ok {
			capturedContextID = cid.(string)
		}

		// Return a task with the same contextId.
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      envelope["id"],
			"result": map[string]interface{}{
				"id":        "task-mt-001",
				"contextId": capturedContextID,
				"status":    map[string]string{"state": "completed"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	srv, _ := newV0TestServer(t, "none", "", nil)
	srv.SetSDKHandler(sdkHandler)
	handler := srv.Handler()

	// First message — establishes context.
	payload1 := `{
		"jsonrpc": "2.0",
		"id": "req-mt-1",
		"method": "message/send",
		"params": {
			"message": {
				"role": "user",
				"parts": [{"type": "text", "text": "first turn"}]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload1))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first turn status = %d, want 200", w.Code)
	}

	// Second message — references the contextId from first turn.
	payload2 := `{
		"jsonrpc": "2.0",
		"id": "req-mt-2",
		"method": "message/send",
		"params": {
			"contextId": "ctx-mt-001",
			"message": {
				"role": "user",
				"parts": [{"type": "text", "text": "second turn"}]
			}
		}
	}`

	req2 := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second turn status = %d, want 200", w2.Code)
	}

	// Verify the contextId was forwarded to the SDK handler.
	if capturedContextID != "ctx-mt-001" {
		t.Errorf("contextId = %q, want ctx-mt-001", capturedContextID)
	}

	// Verify the response preserves the contextId.
	var resp map[string]interface{}
	json.Unmarshal(w2.Body.Bytes(), &resp)
	result := resp["result"].(map[string]interface{})
	if result["contextId"] != "ctx-mt-001" {
		t.Errorf("response contextId = %v, want ctx-mt-001", result["contextId"])
	}
}

// ---------------------------------------------------------------------------
// v0.3 REST — actual A2A operation payloads forwarded to handler
// ---------------------------------------------------------------------------

func TestV0REST_WireFormat_MessageSend(t *testing.T) {
	var capturedBody json.RawMessage
	var capturedPath string
	var capturedMethod string
	bodyCapture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"task-rest-001","contextId":"ctx-rest-001","status":{"state":"completed"}}`))
	})

	srv, _ := newV0TestServer(t, "none", "", bodyCapture)
	handler := srv.Handler()

	// v0.3 REST message:send with actual A2A payload.
	payload := `{
		"message": {
			"role": "user",
			"parts": [{"type": "text", "text": "v0.3 REST message"}]
		}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if capturedPath != "/message:send" {
		t.Errorf("stripped path = %q, want /message:send", capturedPath)
	}
	if capturedMethod != "POST" {
		t.Errorf("method = %q, want POST", capturedMethod)
	}

	// Verify the A2A payload was forwarded intact.
	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("handler received invalid JSON: %v", err)
	}
	msg := body["message"].(map[string]interface{})
	if msg["role"] != "user" {
		t.Errorf("message.role = %v, want user", msg["role"])
	}
}

func TestV0REST_WireFormat_MultiTurnContextId(t *testing.T) {
	var capturedContextID string
	contextCapture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		json.Unmarshal(body, &payload)
		if cid, ok := payload["contextId"]; ok {
			capturedContextID = cid.(string)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"task-mt","contextId":"` + capturedContextID + `","status":{"state":"completed"}}`))
	})

	srv, _ := newV0TestServer(t, "none", "", contextCapture)
	handler := srv.Handler()

	payload := `{
		"contextId": "ctx-multi-turn-42",
		"message": {
			"role": "user",
			"parts": [{"type": "text", "text": "follow-up turn"}]
		}
	}`

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if capturedContextID != "ctx-multi-turn-42" {
		t.Errorf("contextId = %q, want ctx-multi-turn-42", capturedContextID)
	}
}

func TestV0REST_WireFormat_DirectPOST(t *testing.T) {
	var capturedPath string
	directCapture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"task-direct","status":{"state":"completed"}}`))
	})

	srv, _ := newV0TestServer(t, "none", "", directCapture)
	handler := srv.Handler()

	// Direct POST to a custom sub-path — verifies catch-all routing.
	payload := `{"id":"task-direct"}`
	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/tasks/task-123:cancel",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if capturedPath != "/tasks/task-123:cancel" {
		t.Errorf("stripped path = %q, want /tasks/task-123:cancel", capturedPath)
	}
}
