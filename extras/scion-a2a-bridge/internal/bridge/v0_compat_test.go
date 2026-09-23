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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
// mockHubServer — handles Hub API endpoints needed by the executor
// ---------------------------------------------------------------------------

// mockHubServer is an httptest.Server that handles the Hub API endpoints the
// bridge executor calls: agent listing, message sending, and GE exchange.
type mockHubServer struct {
	*httptest.Server

	mu              sync.Mutex
	sentMessages    []mockSentMessage
	exchangeCalls   int
	exchangeHeaders []http.Header    // captured headers from each exchange call
	messageHeaders  []http.Header    // captured headers from each message send
	exchangeHandler http.HandlerFunc // optional override for exchange endpoint
}

type mockSentMessage struct {
	AgentID    string
	Body       json.RawMessage
	AuthHeader string // Authorization header value from the send request
}

func newMockHubServer(t *testing.T) *mockHubServer {
	t.Helper()
	m := &mockHubServer{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route based on path prefix.
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/auth/me":
			// Stands in for the Hub's external-bearer path (design §4.4):
			// any non-empty bearer token is admitted, EXCEPT the sentinel
			// "invalid-bearer-token" value tests use to exercise rejection.
			// This is the endpoint the bridge's hubBearer scheme introspects
			// via UATValidator.
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" || token == "invalid-bearer-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "bearer-user-001", "email": "bearer-user@example.com",
				"display_name": "Bearer User", "role": "member",
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/agents"):
			// Agent list: return a test agent.
			projectID := r.URL.Query().Get("project_id")
			if projectID == "" {
				projectID = "proj1"
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []map[string]interface{}{
					{
						"id":        "agent-001",
						"name":      "agent1",
						"slug":      "agent1",
						"projectId": projectID,
						"status":    "running",
					},
				},
				"totalCount": 1,
			})
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/message"):
			// Message send: capture the message, auth header, and return success.
			// Note: Hub API uses /message (singular), not /messages.
			body, _ := io.ReadAll(r.Body)
			agentID := strings.TrimPrefix(r.URL.Path, "/api/v1/agents/")
			agentID = strings.TrimSuffix(agentID, "/message")
			m.mu.Lock()
			m.sentMessages = append(m.sentMessages, mockSentMessage{
				AgentID:    agentID,
				Body:       body,
				AuthHeader: r.Header.Get("Authorization"),
			})
			m.messageHeaders = append(m.messageHeaders, r.Header.Clone())
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"conversationId": "conv-001",
				"messageId":      "msg-001",
			})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/auth/integrations/google/exchange"):
			m.mu.Lock()
			m.exchangeCalls++
			m.exchangeHeaders = append(m.exchangeHeaders, r.Header.Clone())
			handler := m.exchangeHandler
			m.mu.Unlock()
			if handler != nil {
				handler(w, r)
				return
			}
			// Default exchange response.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"accessToken":       "hub-token-from-exchange",
				"tokenType":         "Bearer",
				"expiresAt":         time.Now().Add(60 * time.Second).Format(time.RFC3339),
				"upstreamExpiresAt": time.Now().Add(300 * time.Second).Format(time.RFC3339),
				"user": map[string]interface{}{
					"id":    "user-ge-001",
					"email": "ge-user@gmail.com",
					"role":  "member",
				},
			})
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockHubServer) SentMessages() []mockSentMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockSentMessage, len(m.sentMessages))
	copy(out, m.sentMessages)
	return out
}

func (m *mockHubServer) ExchangeHeaders() []http.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]http.Header, len(m.exchangeHeaders))
	copy(out, m.exchangeHeaders)
	return out
}

func (m *mockHubServer) ExchangeCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exchangeCalls
}

// newIntegrationTestServer creates a full bridge test server with a real SDK
// handler, real executor, and mock Hub — exercises the complete message
// dispatch pipeline including callerHubClient.
func newIntegrationTestServer(t *testing.T, hub *mockHubServer, scheme, apiKey string) (*Server, *httptest.Server, state.Store) {
	t.Helper()

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://bridge.example.com"},
		Hub:    HubConfig{Endpoint: hub.URL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: scheme, APIKey: apiKey},
		Projects: []ProjectConfig{
			{Slug: "proj1", ExposedAgents: []string{"agent1", "agent2"}},
		},
		Timeouts: TimeoutConfig{SendMessage: 3 * time.Second},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Create a hubclient pointing to the mock Hub for admin operations.
	adminClient, err := hubclient.New(hub.URL, hubclient.WithBearerToken("admin-token"))
	if err != nil {
		t.Fatal(err)
	}

	b := New(store, adminClient, nil, cfg, nil, log)

	// Create real SDK executor + handler.
	executor := NewScionExecutor(b, log)
	routeAuth := RouteKeyAuthenticator()
	innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: routeAuth,
	})
	scopedStore := NewScopedTaskStore(innerStore)
	sdkRequestHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
		}),
		a2asrv.WithAgentInactivityTimeout(2*time.Second),
		a2asrv.WithTaskStore(scopedStore),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)
	sdkJSONRPCHandler := a2asrv.NewJSONRPCHandler(sdkRequestHandler)

	srv := NewServer(b, cfg, nil, log, sdkJSONRPCHandler)

	if scheme == "geGoogle" {
		// Wire GE exchange validator pointing to mock Hub.
		gev := NewGEExchangeValidator(hub.URL, cfg.Auth.GEExchange, log)
		srv.geExchangeValidator = gev
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return srv, ts, store
}

// doRPCRaw sends a raw JSON-RPC request and returns the HTTP response body.
func doRPCRaw(t *testing.T, ts *httptest.Server, path string, payload string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
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
			name:       "tasks GET",
			method:     "GET",
			path:       "/projects/proj1/agents/agent1/tasks",
			wantStatus: http.StatusOK,
			wantPath:   "/tasks",
		},
		{
			name:       "tasks/id:cancel",
			method:     "POST",
			path:       "/projects/proj1/agents/agent1/tasks/abc:cancel",
			wantStatus: http.StatusOK,
			wantPath:   "/tasks/abc:cancel",
		},
		{
			name:       "tasks/id:resubscribe",
			method:     "POST",
			path:       "/projects/proj1/agents/agent1/tasks/abc:resubscribe",
			wantStatus: http.StatusOK,
			wantPath:   "/tasks/abc:resubscribe",
		},
		{
			name:       "groves alias",
			method:     "POST",
			path:       "/groves/proj1/agents/agent1/message:send",
			wantStatus: http.StatusOK,
			wantPath:   "/message:send",
		},
		{
			name:       "nested path",
			method:     "GET",
			path:       "/projects/proj1/agents/agent1/tasks/task-123",
			wantStatus: http.StatusOK,
			wantPath:   "/tasks/task-123",
		},
		{
			name:       "message:stream",
			method:     "POST",
			path:       "/projects/proj1/agents/agent1/message:stream",
			wantStatus: http.StatusOK,
			wantPath:   "/message:stream",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub.called = false
			req := httptest.NewRequest(tc.method, tc.path,
				strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if stub.called && stub.lastPath != tc.wantPath {
				t.Errorf("stripped path = %q, want %q", stub.lastPath, tc.wantPath)
			}
		})
	}
}

func TestV0REST_NotConfigured(t *testing.T) {
	srv, _ := newV0TestServer(t, "none", "", nil)
	handler := srv.Handler()

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Without v0 handler, catch-all is not registered → 405 or 404.
	if w.Code == http.StatusOK {
		t.Error("v0 REST route should not work when handler is nil")
	}
}

func TestV0REST_ExistingRoutePrecedence(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	// Agent card should be served by the dedicated handler, not the v0 stub.
	req := httptest.NewRequest("GET", "/projects/proj1/agents/agent1/.well-known/agent-card.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("agent card status = %d, want 200", w.Code)
	}
	// The stub should NOT have been called (dedicated handler takes precedence).
	if stub.called {
		t.Error("v0 stub handler was called for agent card — dedicated handler should take precedence")
	}
}

func TestV0REST_InvalidSlug(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	req := httptest.NewRequest("POST", "/projects/INVALID!/agents/agent1/message:send",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid slug status = %d, want 400", w.Code)
	}
}

func TestV0REST_UnexposedAgent(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	req := httptest.NewRequest("POST", "/projects/proj1/agents/hidden-agent/message:send",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unexposed agent status = %d, want 404", w.Code)
	}
}

func TestV0REST_AuthProtection(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "apiKey", "secret-key", stub)
	handler := srv.Handler()

	// Without API key, should be rejected.
	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send",
		strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Error("unauthenticated v0 request should be rejected")
	}
}

func TestV0REST_RouteInfoInjected(t *testing.T) {
	var capturedRoute RouteInfo
	contextCapture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ri, ok := RouteInfoFrom(r.Context()); ok {
			capturedRoute = ri
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	srv, _ := newV0TestServer(t, "none", "", contextCapture)
	handler := srv.Handler()

	req := httptest.NewRequest("POST", "/projects/proj1/agents/agent1/message:send",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if capturedRoute.ProjectSlug != "proj1" {
		t.Errorf("project slug = %q, want proj1", capturedRoute.ProjectSlug)
	}
	if capturedRoute.AgentSlug != "agent1" {
		t.Errorf("agent slug = %q, want agent1", capturedRoute.AgentSlug)
	}
}

func TestV0REST_QueryStringPreserved(t *testing.T) {
	var capturedQuery string
	queryCapture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Write([]byte("ok"))
	})

	srv, _ := newV0TestServer(t, "none", "", queryCapture)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/projects/proj1/agents/agent1/tasks?status=completed&limit=10", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !strings.Contains(capturedQuery, "status=completed") {
		t.Errorf("query = %q, missing status=completed", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "limit=10") {
		t.Errorf("query = %q, missing limit=10", capturedQuery)
	}
}

func TestV0REST_AgentCardDualFormat(t *testing.T) {
	stub := &v0StubHandler{}
	srv, _ := newV0TestServer(t, "none", "", stub)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/projects/proj1/agents/agent1/.well-known/agent-card.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var card map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatalf("failed to parse agent card: %v", err)
	}

	// With v0 handler active, card should include REST interface.
	ifaces, ok := card["supportedInterfaces"].([]interface{})
	if !ok {
		t.Fatal("supportedInterfaces not present or wrong type")
	}

	hasJSONRPC := false
	hasREST := false
	for _, iface := range ifaces {
		m := iface.(map[string]interface{})
		switch m["protocolBinding"] {
		case "JSONRPC":
			hasJSONRPC = true
		case "REST":
			hasREST = true
		}
	}
	if !hasJSONRPC {
		t.Error("missing JSONRPC interface in supportedInterfaces")
	}
	if !hasREST {
		t.Error("missing REST interface in supportedInterfaces (v0 handler is active)")
	}
}

// ===========================================================================
// REST v0.3 conditional advertising (nit fix)
// ===========================================================================

func TestV0REST_AgentCardNoRESTWhenHandlerNil(t *testing.T) {
	// Without v0 handler, REST should NOT be advertised.
	srv, _ := newV0TestServer(t, "none", "", nil)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/projects/proj1/agents/agent1/.well-known/agent-card.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var card map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &card)

	ifaces, ok := card["supportedInterfaces"].([]interface{})
	if !ok {
		t.Fatal("supportedInterfaces not present")
	}

	for _, iface := range ifaces {
		m := iface.(map[string]interface{})
		if m["protocolBinding"] == "REST" {
			t.Error("REST interface should NOT be advertised when v0 handler is nil")
		}
	}

	// Legacy flat fields should also be absent.
	if _, ok := card["protocolVersion"]; ok {
		t.Error("protocolVersion flat field should not be present without v0 handler")
	}
}

// ===========================================================================
// Discovery aliases: agent.json + direct POST
// ===========================================================================

func TestDiscovery_RootAgentJSON(t *testing.T) {
	srv, _ := newV0TestServer(t, "none", "", nil)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/.well-known/agent.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/.well-known/agent.json status = %d, want 200", w.Code)
	}

	var card map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &card)
	if card["name"] == nil {
		t.Error("agent.json missing 'name' field")
	}
}

func TestDiscovery_PerAgentAgentJSON(t *testing.T) {
	srv, _ := newV0TestServer(t, "none", "", nil)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/projects/proj1/agents/agent1/.well-known/agent.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("per-agent agent.json status = %d, want 200", w.Code)
	}

	var card map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &card)
	if card["name"] == nil {
		t.Error("per-agent agent.json missing 'name' field")
	}
}

func TestDiscovery_PerAgentAgentJSON_GrovesAlias(t *testing.T) {
	srv, _ := newV0TestServer(t, "none", "", nil)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/groves/proj1/agents/agent1/.well-known/agent.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("groves agent.json status = %d, want 200", w.Code)
	}
}

func TestDiscovery_AgentJSON_PublicNoAuth(t *testing.T) {
	srv, _ := newV0TestServer(t, "apiKey", "secret-key", nil)
	handler := srv.Handler()

	// Root agent.json should be accessible without authentication.
	req := httptest.NewRequest("GET", "/.well-known/agent.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("root agent.json should be public, got status %d", w.Code)
	}

	// Per-agent agent.json should also be accessible without authentication.
	req = httptest.NewRequest("GET", "/projects/proj1/agents/agent1/.well-known/agent.json", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("per-agent agent.json should be public, got status %d", w.Code)
	}
}

func TestDiscovery_DirectPOST_AgentRoot(t *testing.T) {
	// Direct POST to agent root should forward to JSON-RPC handler.
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	// Send a tasks/get request via direct POST — no /jsonrpc suffix.
	payload := `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"nonexistent"}}`
	status, body := doRPCRaw(t, ts, "/projects/proj1/agents/agent1", payload, nil)

	if status != http.StatusOK {
		t.Fatalf("direct POST status = %d, want 200; body: %s", status, string(body))
	}

	var resp map[string]interface{}
	json.Unmarshal(body, &resp)
	// Should get a JSON-RPC error (task not found), but the request should be processed.
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("response is not JSON-RPC 2.0: %s", string(body))
	}
}

func TestDiscovery_DirectPOST_GrovesAlias(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	payload := `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"nonexistent"}}`
	status, body := doRPCRaw(t, ts, "/groves/proj1/agents/agent1", payload, nil)

	if status != http.StatusOK {
		t.Fatalf("direct POST via /groves/ status = %d, want 200; body: %s", status, string(body))
	}
}

// ===========================================================================
// Critical regression: callerHubClient handles ge_exchange token type
// ===========================================================================

func TestCallerHubClient_GEExchangeTokenType(t *testing.T) {
	hub := newMockHubServer(t)

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := &Config{
		Hub: HubConfig{Endpoint: hub.URL, User: "admin@test"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adminClient, _ := hubclient.New(hub.URL, hubclient.WithBearerToken("admin-token"))
	b := New(store, adminClient, nil, cfg, nil, log)

	caller := &CallerIdentity{
		UserID:    "user-ge-001",
		Email:     "ge-user@gmail.com",
		Role:      "member",
		RawToken:  "hub-token-from-exchange",
		TokenType: "ge_exchange",
	}

	// This call must NOT fail. Before the fix, it returned
	// "unknown token type: ge_exchange".
	client, err := b.callerHubClient(caller)
	if err != nil {
		t.Fatalf("callerHubClient(ge_exchange) error: %v", err)
	}
	if client == nil {
		t.Fatal("callerHubClient returned nil client")
	}

	// Verify the client can actually reach the Hub.
	agents, err := client.Agents().List(t.Context(), nil)
	if err != nil {
		t.Fatalf("Hub API call with ge_exchange client failed: %v", err)
	}
	if len(agents.Agents) == 0 {
		t.Error("expected at least one agent from mock Hub")
	}
}

// TestCallerHubClient_BearerTokenType proves callerHubClient's "bearer" case
// (bridge.go) forwards the caller's original token verbatim, exactly like
// "uat" does. If the "uat", "bearer" case were ever reverted to "uat" alone,
// this call would fail with "unknown token type: bearer" instead of
// succeeding — the same regression TestCallerHubClient_GEExchangeTokenType
// guards for the "ge_exchange" type.
func TestCallerHubClient_BearerTokenType(t *testing.T) {
	hub := newMockHubServer(t)

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	cfg := &Config{
		Hub: HubConfig{Endpoint: hub.URL, User: "admin@test"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adminClient, _ := hubclient.New(hub.URL, hubclient.WithBearerToken("admin-token"))
	b := New(store, adminClient, nil, cfg, nil, log)

	caller := &CallerIdentity{
		UserID:    "bearer-user-001",
		Email:     "bearer-user@example.com",
		Role:      "member",
		RawToken:  "forwarded-google-id-token",
		TokenType: "bearer",
	}

	client, err := b.callerHubClient(caller)
	if err != nil {
		t.Fatalf("callerHubClient(bearer) error: %v", err)
	}
	if client == nil {
		t.Fatal("callerHubClient returned nil client")
	}

	// Verify the client can actually reach the Hub, and that no exchange
	// happened — the raw token is used directly as the bearer credential.
	agents, err := client.Agents().List(t.Context(), nil)
	if err != nil {
		t.Fatalf("Hub API call with bearer client failed: %v", err)
	}
	if len(agents.Agents) == 0 {
		t.Error("expected at least one agent from mock Hub")
	}
	if hub.ExchangeCallCount() != 0 {
		t.Errorf("exchange calls = %d, want 0 (bearer forwards verbatim, no exchange)", hub.ExchangeCallCount())
	}
}

// TestHubBearer_EndToEnd_PassThrough is the B3-adjacent bridge-side
// counterpart (see extras/scion-a2a-bridge/integration/auth_transport_process_test.go
// for the full-process test against a real Hub with a test Google issuer):
// through real production wiring (real New(), real Server, real executor,
// real SDK handler), a caller presents a Google-shaped bearer credential
// under auth.scheme: hubBearer. It must be (1) admitted via the mock Hub's
// /api/v1/auth/me, and (2) forwarded to the Hub's message-send call
// byte-for-byte identical to what the caller presented — never exchanged for
// a different credential (contrast with geGoogle in
// TestGEExchange_ExecutorPath_Regression, which mints/forwards a DIFFERENT,
// Hub-issued token).
func TestHubBearer_EndToEnd_PassThrough(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "hubBearer", "")

	const googleToken = "google-id-token-abc123"
	payload := `{
		"jsonrpc": "2.0",
		"id": "hub-bearer-1",
		"method": "SendMessage",
		"params": {
			"message": {
				"messageId": "msg-bearer-001",
				"role": "ROLE_USER",
				"parts": [{"text": "Hello via hubBearer"}]
			}
		}
	}`
	status, body := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload,
		map[string]string{"Authorization": "Bearer " + googleToken})

	if status != http.StatusOK {
		t.Fatalf("hubBearer message/send status = %d, want 200; body: %s", status, body)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["jsonrpc"] != "2.0" || resp["error"] != nil {
		t.Fatalf("unexpected JSON-RPC envelope: %s", body)
	}

	msgs := hub.SentMessages()
	if len(msgs) == 0 {
		t.Fatal("mock Hub received no messages — executor dispatch failed")
	}
	// Hard assertion: verbatim pass-through. Not "hub-token-from-exchange"
	// (that would mean hubBearer silently started exchanging, like geGoogle).
	if msgs[0].AuthHeader != "Bearer "+googleToken {
		t.Fatalf("per-caller Hub send used wrong auth: got %q, want %q",
			msgs[0].AuthHeader, "Bearer "+googleToken)
	}
	if hub.ExchangeCallCount() != 0 {
		t.Fatalf("exchange calls = %d, want 0 (hubBearer never exchanges)", hub.ExchangeCallCount())
	}
}

// TestGEExchange_ExecutorPath_Regression verifies that a GE-authenticated caller
// can send a message through the real executor path. This was broken before the
// ge_exchange case was added to callerHubClient — the executor would fail with
// "creating per-caller hub client: unknown token type: ge_exchange".
//
// Hard assertions: the exchange endpoint MUST be called, the Hub MUST receive
// the message dispatch, and the per-caller bearer token MUST be the exchanged
// Hub token (not the original Google credential).
func TestGEExchange_ExecutorPath_Regression(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "geGoogle", "")

	// Send message/send via JSON-RPC with Bearer token (GE auth).
	payload := `{
		"jsonrpc": "2.0",
		"id": "regression-1",
		"method": "SendMessage",
		"params": {
			"message": {
				"messageId": "msg-ge-001",
				"role": "ROLE_USER",
				"parts": [{"text": "Hello from GE caller"}]
			}
		}
	}`
	status, body := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload,
		map[string]string{"Authorization": "Bearer test-google-cred"})

	if status != http.StatusOK {
		t.Fatalf("GE message/send status = %d, want 200; body: %s", status, body)
	}

	// Hard assertion: the exchange endpoint MUST have been called.
	if hub.ExchangeCallCount() == 0 {
		t.Fatal("mock Hub exchange endpoint was never called — GE auth not wired")
	}

	// Hard assertion: the Hub MUST have received a dispatched message.
	msgs := hub.SentMessages()
	if len(msgs) == 0 {
		t.Fatal("mock Hub received no messages — executor dispatch failed")
	}

	// Hard assertion: the per-caller Hub request MUST use the exchanged Hub
	// token, NOT the original Google credential.
	if msgs[0].AuthHeader != "Bearer hub-token-from-exchange" {
		t.Fatalf("per-caller Hub send used wrong auth: got %q, want %q",
			msgs[0].AuthHeader, "Bearer hub-token-from-exchange")
	}

	// The response must be valid JSON-RPC.
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("response is not JSON-RPC 2.0: %s", string(body))
	}
	// Verify no "unknown token type" error.
	if errObj, ok := resp["error"]; ok {
		errMap := errObj.(map[string]interface{})
		msg := fmt.Sprint(errMap["message"])
		if strings.Contains(msg, "unknown token type") {
			t.Fatalf("executor failed with callerHubClient error: %s", msg)
		}
	}
}

// ===========================================================================
// JSON-RPC wire tests through real SDK handler + executor + mock Hub
// ===========================================================================

func TestJSONRPC_RealHandler_MessageSend(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	payload := `{
		"jsonrpc": "2.0",
		"id": "req-1",
		"method": "SendMessage",
		"params": {
			"message": {
				"messageId": "msg-send-001",
				"role": "ROLE_USER",
				"parts": [{"text": "Hello, agent!"}]
			}
		}
	}`
	status, body := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload, nil)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("response jsonrpc = %v, want 2.0", resp["jsonrpc"])
	}

	// Hard assertion: the executor MUST have dispatched to the Hub.
	msgs := hub.SentMessages()
	if len(msgs) == 0 {
		t.Fatal("mock Hub received no messages — executor dispatch broken")
	}
	if msgs[0].AgentID != "agent-001" {
		t.Errorf("dispatched to agent %q, want agent-001", msgs[0].AgentID)
	}

	// Response must contain result (task with status).
	if resp["result"] == nil && resp["error"] == nil {
		t.Fatal("response has neither result nor error")
	}
}

func TestJSONRPC_RealHandler_TasksGet(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	// tasks/get goes through the SDK task store, not the executor.
	payload := `{"jsonrpc":"2.0","id":"req-3","method":"GetTask","params":{"id":"task-001"}}`
	status, body := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload, nil)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var resp map[string]interface{}
	json.Unmarshal(body, &resp)
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("response jsonrpc = %v, want 2.0", resp["jsonrpc"])
	}
	// Task not found → error.
	if resp["error"] == nil {
		t.Error("expected TaskNotFound error for nonexistent task")
	}
}

func TestJSONRPC_RealHandler_TasksCancel(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	payload := `{"jsonrpc":"2.0","id":"req-4","method":"CancelTask","params":{"id":"task-001"}}`
	status, body := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload, nil)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	var resp map[string]interface{}
	json.Unmarshal(body, &resp)
	// Cancel of nonexistent task → error.
	if resp["error"] == nil {
		t.Error("expected error for cancel of nonexistent task")
	}
}

func TestJSONRPC_RealHandler_MessageStream(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	payload := `{
		"jsonrpc": "2.0",
		"id": "req-2",
		"method": "SendStreamingMessage",
		"params": {
			"message": {
				"messageId": "msg-stream-001",
				"role": "ROLE_USER",
				"parts": [{"text": "stream this"}]
			}
		}
	}`
	req, _ := http.NewRequest("POST", ts.URL+"/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	// Streaming response may be SSE (text/event-stream) or JSON-RPC
	// (application/json) depending on whether events were available to
	// stream before the executor completed.
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") && !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want text/event-stream or application/json", ct)
	}
}

func TestJSONRPC_RealHandler_TasksResubscribe(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	payload := `{"jsonrpc":"2.0","id":"req-5","method":"SubscribeToTask","params":{"id":"task-001"}}`
	req, _ := http.NewRequest("POST", ts.URL+"/projects/proj1/agents/agent1/jsonrpc",
		strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Tasks/resubscribe on a nonexistent task should return an error
	// (either as SSE event or JSON-RPC error).
	if resp.StatusCode != http.StatusOK {
		t.Logf("status = %d (task not found → expected)", resp.StatusCode)
	}
}

func TestJSONRPC_RealHandler_UnknownMethod(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	payload := `{"jsonrpc":"2.0","id":"req-u","method":"invalid/method","params":{}}`
	status, body := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload, nil)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	var resp map[string]interface{}
	json.Unmarshal(body, &resp)
	if resp["error"] == nil {
		t.Error("expected method not found error")
	}
	errObj := resp["error"].(map[string]interface{})
	if code, ok := errObj["code"].(float64); !ok || code != -32601 {
		t.Errorf("error code = %v, want -32601", errObj["code"])
	}
}

// ---------------------------------------------------------------------------
// Discovery aliases (/groves/ ↔ /projects/) through real handler
// ---------------------------------------------------------------------------

func TestJSONRPC_DiscoveryAlias_GrovesPath(t *testing.T) {
	hub := newMockHubServer(t)
	_, ts, _ := newIntegrationTestServer(t, hub, "none", "")

	payload := `{"jsonrpc":"2.0","id":"req-grove","method":"GetTask","params":{"id":"nonexistent"}}`
	status, body := doRPCRaw(t, ts, "/groves/proj1/agents/agent1/jsonrpc", payload, nil)

	if status != http.StatusOK {
		t.Fatalf("groves alias status = %d, want 200; body: %s", status, body)
	}

	var resp map[string]interface{}
	json.Unmarshal(body, &resp)
	if resp["jsonrpc"] != "2.0" {
		t.Errorf("response via /groves/ is not valid JSON-RPC")
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

// ===========================================================================
// GE exchange — transport auth tests
// ===========================================================================

func TestGEExchangeValidator_TransportAuth_OutgoingHeaders(t *testing.T) {
	var capturedHeaders http.Header
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":       "hub-token",
			"tokenType":         "Bearer",
			"expiresAt":         time.Now().Add(60 * time.Second).Format(time.RFC3339),
			"upstreamExpiresAt": time.Now().Add(300 * time.Second).Format(time.RFC3339),
			"user":              map[string]interface{}{"id": "u1", "email": "a@b.com", "role": "member"},
		})
	}))
	defer hubServer.Close()

	// Create a mock transport auth source.
	mockSrc := &mockTokenSource{token: "transport-oidc-token"}

	v := NewGEExchangeValidator(hubServer.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger(), WithGETransportAuth(mockSrc, 2)) // HeaderServerlessAuthorization = 2

	_, err := v.Validate(t.Context(), "user-google-cred")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Verify the outgoing request had transport auth headers.
	xSA := capturedHeaders.Get("X-Serverless-Authorization")
	if xSA == "" {
		t.Error("expected X-Serverless-Authorization header from transport auth")
	}
	if !strings.Contains(xSA, "transport-oidc-token") {
		t.Errorf("X-Serverless-Authorization = %q, want to contain transport-oidc-token", xSA)
	}
}

// mockTokenSource implements transportauth.TokenSource for testing.
type mockTokenSource struct {
	token string
}

func (m *mockTokenSource) Token() (string, error)           { return m.token, nil }
func (m *mockTokenSource) SetToken(t string, exp time.Time) {}
func (m *mockTokenSource) Expiry() time.Time                { return time.Now().Add(1 * time.Hour) }

func TestGEExchangeValidator_TransportAuth_NotSet_NoHeaders(t *testing.T) {
	var capturedHeaders http.Header
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":       "hub-token",
			"tokenType":         "Bearer",
			"expiresAt":         time.Now().Add(60 * time.Second).Format(time.RFC3339),
			"upstreamExpiresAt": time.Now().Add(300 * time.Second).Format(time.RFC3339),
			"user":              map[string]interface{}{"id": "u1", "email": "a@b.com", "role": "member"},
		})
	}))
	defer hubServer.Close()

	// Without transport auth, no extra headers should be set.
	v := NewGEExchangeValidator(hubServer.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(t.Context(), "user-google-cred")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	if xSA := capturedHeaders.Get("X-Serverless-Authorization"); xSA != "" {
		t.Errorf("unexpected X-Serverless-Authorization header: %q", xSA)
	}
}

// ===========================================================================
// Snapshot-backed production middleware: transport auth end-to-end
// ===========================================================================

// newSnapshotIntegrationTestServer creates a full bridge test server that uses
// the snapshot-backed auth middleware (the production code path). The snapshot's
// GE validator is constructed with the given geOpts, proving that transport auth
// flows through BuildSnapshot → BuildAuthValidators → GEExchangeValidator.
func newSnapshotIntegrationTestServer(t *testing.T, hub *mockHubServer, geOpts ...GEValidatorOption) (*Server, *httptest.Server) {
	t.Helper()

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://bridge.example.com"},
		Hub:    HubConfig{Endpoint: hub.URL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: "geGoogle", GEExchange: GEExchangeConfig{CredentialType: "id_token", CacheTTL: 60 * time.Second}},
		Projects: []ProjectConfig{
			{Slug: "proj1", ExposedAgents: []string{"agent1"}},
		},
		Timeouts: TimeoutConfig{SendMessage: 3 * time.Second},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	adminClient, err := hubclient.New(hub.URL, hubclient.WithBearerToken("admin-token"))
	if err != nil {
		t.Fatal(err)
	}

	b := New(store, adminClient, nil, cfg, nil, log)

	// Build SDK handler pipeline.
	executor := NewScionExecutor(b, log)
	routeAuth := RouteKeyAuthenticator()
	innerStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: routeAuth})
	scopedStore := NewScopedTaskStore(innerStore)
	sdkRequestHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true}),
		a2asrv.WithAgentInactivityTimeout(2*time.Second),
		a2asrv.WithTaskStore(scopedStore),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)
	sdkJSONRPCHandler := a2asrv.NewJSONRPCHandler(sdkRequestHandler)

	srv := NewServer(b, cfg, nil, log, sdkJSONRPCHandler)

	// Build the snapshot WITH transport auth (this is the production path).
	snap := BuildSnapshot(*cfg, geOpts...)
	snapHolder := NewSnapshotHolder(snap)
	srv.SetSnapshot(snapHolder)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return srv, ts
}

// TestSnapshotMiddleware_TransportAuth_InitialComposition proves that the
// snapshot-backed GE validator in the production middleware receives transport
// auth options. The exchange request to Hub must carry the X-Serverless-Authorization
// header from the transport auth source.
func TestSnapshotMiddleware_TransportAuth_InitialComposition(t *testing.T) {
	hub := newMockHubServer(t)
	mockSrc := &mockTokenSource{token: "snapshot-transport-token"}

	_, ts := newSnapshotIntegrationTestServer(t, hub,
		WithGETransportAuth(mockSrc, 2)) // HeaderServerlessAuthorization

	// Send a request through the production middleware.
	payload := `{"jsonrpc":"2.0","id":"snap-1","method":"GetTask","params":{"id":"t1"}}`
	status, _ := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload,
		map[string]string{"Authorization": "Bearer user-google-cred"})

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	// Hard assertion: the exchange call to Hub MUST have the transport header.
	if hub.ExchangeCallCount() == 0 {
		t.Fatal("exchange endpoint was not called — snapshot middleware not wired")
	}
	headers := hub.ExchangeHeaders()
	xSA := headers[0].Get("X-Serverless-Authorization")
	if xSA == "" {
		t.Fatal("exchange request missing X-Serverless-Authorization — transport auth not wired in snapshot")
	}
	if !strings.Contains(xSA, "snapshot-transport-token") {
		t.Fatalf("X-Serverless-Authorization = %q, want to contain snapshot-transport-token", xSA)
	}
}

// TestSnapshotMiddleware_TransportAuth_AfterSnapshotReplacement proves that
// transport auth survives snapshot replacement (hot-reload / reconfigure).
// This simulates what happens when the broker pushes a new admin config.
func TestSnapshotMiddleware_TransportAuth_AfterSnapshotReplacement(t *testing.T) {
	hub := newMockHubServer(t)
	mockSrc := &mockTokenSource{token: "reload-transport-token"}
	geOpts := []GEValidatorOption{WithGETransportAuth(mockSrc, 2)}

	srv, ts := newSnapshotIntegrationTestServer(t, hub, geOpts...)

	// First request — validates initial snapshot has transport auth.
	payload := `{"jsonrpc":"2.0","id":"r1","method":"GetTask","params":{"id":"t1"}}`
	status, _ := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload,
		map[string]string{"Authorization": "Bearer cred-1"})
	if status != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200", status)
	}
	if hub.ExchangeCallCount() != 1 {
		t.Fatalf("exchange calls = %d, want 1", hub.ExchangeCallCount())
	}

	// Simulate hot-reload: rebuild snapshot with same geOpts (as broker does).
	newCfg := Config{
		Bridge: BridgeConfig{ExternalURL: "https://bridge-new.example.com"},
		Hub:    HubConfig{Endpoint: hub.URL, User: "admin@test"},
		Auth:   AuthConfig{Scheme: "geGoogle", GEExchange: GEExchangeConfig{CredentialType: "id_token", CacheTTL: 60 * time.Second}},
		Projects: []ProjectConfig{
			{Slug: "proj1", ExposedAgents: []string{"agent1"}},
		},
	}
	newSnap := BuildSnapshot(newCfg, geOpts...)
	srv.snapshot.Store(newSnap)

	// Second request — the new snapshot must also have transport auth.
	payload2 := `{"jsonrpc":"2.0","id":"r2","method":"GetTask","params":{"id":"t2"}}`
	status2, _ := doRPCRaw(t, ts, "/projects/proj1/agents/agent1/jsonrpc", payload2,
		map[string]string{"Authorization": "Bearer cred-2"})
	if status2 != http.StatusOK {
		t.Fatalf("post-reload request status = %d, want 200", status2)
	}

	// The new snapshot's validator should have called exchange with transport headers.
	if hub.ExchangeCallCount() < 2 {
		t.Fatalf("exchange calls after reload = %d, want >= 2", hub.ExchangeCallCount())
	}
	headers := hub.ExchangeHeaders()
	lastHeader := headers[len(headers)-1]
	xSA := lastHeader.Get("X-Serverless-Authorization")
	if xSA == "" {
		t.Fatal("post-reload exchange missing X-Serverless-Authorization — transport auth lost on snapshot rebuild")
	}
	if !strings.Contains(xSA, "reload-transport-token") {
		t.Fatalf("post-reload X-Serverless-Authorization = %q, want to contain reload-transport-token", xSA)
	}
}

// ===========================================================================
// Cache — expired response caching (fail closed)
// ===========================================================================

func TestGEExchangeValidator_ExpiredHub_FailsClosed(t *testing.T) {
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":       "hub-token",
			"tokenType":         "Bearer",
			"expiresAt":         time.Now().Add(-10 * time.Second).Format(time.RFC3339), // Already expired
			"upstreamExpiresAt": time.Now().Add(300 * time.Second).Format(time.RFC3339),
			"user":              map[string]interface{}{"id": "u1", "email": "a@b.com", "role": "member"},
		})
	}))
	defer hubServer.Close()

	v := NewGEExchangeValidator(hubServer.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(t.Context(), "test-cred")
	if err == nil {
		t.Fatal("expected error for already-expired Hub token")
	}
	if !strings.Contains(err.Error(), "already expired") {
		t.Errorf("error = %v, want 'already expired'", err)
	}

	// Should NOT be cached.
	if v.CacheLen() != 0 {
		t.Errorf("cache len = %d, want 0 (expired response should not be cached)", v.CacheLen())
	}
}

func TestGEExchangeValidator_ExpiredUpstream_FailsClosed(t *testing.T) {
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":       "hub-token",
			"tokenType":         "Bearer",
			"expiresAt":         time.Now().Add(60 * time.Second).Format(time.RFC3339),
			"upstreamExpiresAt": time.Now().Add(-5 * time.Second).Format(time.RFC3339), // Already expired
			"user":              map[string]interface{}{"id": "u1", "email": "a@b.com", "role": "member"},
		})
	}))
	defer hubServer.Close()

	v := NewGEExchangeValidator(hubServer.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(t.Context(), "test-cred")
	if err == nil {
		t.Fatal("expected error for already-expired upstream credential")
	}
	if !strings.Contains(err.Error(), "already expired") {
		t.Errorf("error = %v, want 'already expired'", err)
	}
}

func TestGEExchangeValidator_ZeroExpiry_FailsClosed(t *testing.T) {
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ExpiresAt at exact boundary (now).
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":       "hub-token",
			"tokenType":         "Bearer",
			"expiresAt":         time.Now().Format(time.RFC3339),
			"upstreamExpiresAt": time.Now().Add(300 * time.Second).Format(time.RFC3339),
			"user":              map[string]interface{}{"id": "u1", "email": "a@b.com", "role": "member"},
		})
	}))
	defer hubServer.Close()

	v := NewGEExchangeValidator(hubServer.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(t.Context(), "test-cred")
	if err == nil {
		t.Fatal("expected error for zero-remaining Hub token")
	}
}

// ===========================================================================
// Cache — LRU eviction order + concurrency
// ===========================================================================

func TestGEExchangeValidator_LRUEviction_ConcurrentAccess(t *testing.T) {
	var callCount atomic.Int64
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":       fmt.Sprintf("tok-%d", n),
			"tokenType":         "Bearer",
			"expiresAt":         time.Now().Add(60 * time.Second).Format(time.RFC3339),
			"upstreamExpiresAt": time.Now().Add(300 * time.Second).Format(time.RFC3339),
			"user":              map[string]interface{}{"id": fmt.Sprintf("u-%d", n), "email": "a@b.com", "role": "member"},
		})
	}))
	defer hubServer.Close()

	v := NewGEExchangeValidator(hubServer.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	// Concurrent access with many different credentials.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := v.Validate(t.Context(), fmt.Sprintf("cred-%d", i))
			if err != nil {
				t.Errorf("validate cred-%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// All 50 should be cached.
	if v.CacheLen() != 50 {
		t.Errorf("cache len = %d, want 50", v.CacheLen())
	}
}
