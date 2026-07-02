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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"context"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// jsonRPCRequest is a test helper for constructing JSON-RPC requests.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// jsonRPCResponse is a test helper for parsing JSON-RPC responses.
type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *jsonRPCErr `json:"error,omitempty"`
}

type jsonRPCErr struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func newTestServer(t *testing.T) (*Server, *httptest.Server, *state.Store) {
	t.Helper()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://a2a.test.example.com",
			Provider: ProviderConfig{
				Organization: "Test Org",
				URL:          "https://test.example.com",
			},
		},
		Auth: AuthConfig{
			Scheme: "apiKey",
			APIKey: "test-api-key",
		},
		Projects: []ProjectConfig{
			{
				Slug:          "test-grove",
				ExposedAgents: []string{"test-agent"},
			},
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)

	// Create a minimal SDK executor and handler for testing.
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
		a2asrv.WithTaskStore(scopedStore),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)
	sdkJSONRPCHandler := a2asrv.NewJSONRPCHandler(sdkRequestHandler)

	srv := NewServer(b, cfg, nil, log, sdkJSONRPCHandler)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return srv, ts, store
}

func doRPC(t *testing.T, ts *httptest.Server, path string, method string, params interface{}, apiKey string) *jsonRPCResponse {
	t.Helper()

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  paramsJSON,
	}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("X-API-Key", apiKey)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var rpcResp jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return &rpcResp
}

func TestHealthEndpoint(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

func TestWellKnownAgentCard(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET /.well-known/agent-card.json: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc == "" {
		t.Error("expected Cache-Control header")
	}

	var card map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&card)

	if card["name"] != "scion-a2a-bridge" {
		t.Errorf("name = %q, want %q", card["name"], "scion-a2a-bridge")
	}
	if card["url"] != "https://a2a.test.example.com" {
		t.Errorf("url = %q, want external URL", card["url"])
	}

	provider, ok := card["provider"].(map[string]interface{})
	if !ok {
		t.Fatal("expected provider object in card")
	}
	if provider["organization"] != "Test Org" {
		t.Errorf("provider.organization = %q, want %q", provider["organization"], "Test Org")
	}

	caps, ok := card["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatal("expected capabilities object in card")
	}
	if caps["streaming"] != true {
		t.Errorf("capabilities.streaming = %v, want true", caps["streaming"])
	}
	if caps["pushNotifications"] != true {
		t.Errorf("capabilities.pushNotifications = %v, want true", caps["pushNotifications"])
	}
}

func TestPerAgentCard(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/projects/test-grove/agents/test-agent/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var card map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&card)

	if card["name"] != "test-agent" {
		t.Errorf("name = %q, want %q", card["name"], "test-agent")
	}

	expectedURL := "https://a2a.test.example.com/projects/test-grove/agents/test-agent"
	if card["url"] != expectedURL {
		t.Errorf("url = %q, want %q", card["url"], expectedURL)
	}

	caps, ok := card["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatal("expected capabilities object in per-agent card")
	}
	if caps["streaming"] != true {
		t.Errorf("capabilities.streaming = %v, want true", caps["streaming"])
	}
	if caps["pushNotifications"] != true {
		t.Errorf("capabilities.pushNotifications = %v, want true", caps["pushNotifications"])
	}
}

func TestPerAgentCardNotExposed(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/projects/test-grove/agents/hidden-agent/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for non-exposed agent", resp.StatusCode)
	}
}

func TestPerAgentCardUnknownProject(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/projects/unknown-grove/agents/test-agent/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown project", resp.StatusCode)
	}
}

func TestAuthMiddleware(t *testing.T) {
	_, ts, _ := newTestServer(t)

	// Agent cards are public — no auth required.
	resp, err := http.Get(ts.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("agent card without auth: status = %d, want 200", resp.StatusCode)
	}

	// JSON-RPC without auth should be rejected.
	rpcReq, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tasks/get", Params: json.RawMessage(`{"id":"x"}`)})
	httpReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/projects/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(rpcReq))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("RPC without auth: status = %d, want 401", resp.StatusCode)
	}

	// With correct API key should succeed.
	httpReq, _ = http.NewRequest(http.MethodPost, ts.URL+"/projects/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(rpcReq))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", "test-api-key")

	resp, err = http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("RPC with valid auth: status = %d, want 200", resp.StatusCode)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	_, ts, _ := newTestServer(t)

	// The SDK handler will return TaskNotFound via its own error handling.
	rpcResp := doRPC(t, ts, "/projects/test-grove/agents/test-agent/jsonrpc",
		"tasks/get", map[string]interface{}{"id": "nonexistent-task"}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for nonexistent task")
	}
	// SDK uses standard A2A error codes.
	if rpcResp.Error.Code >= 0 {
		t.Errorf("expected negative error code, got %d", rpcResp.Error.Code)
	}
}

func TestUnknownMethod(t *testing.T) {
	_, ts, _ := newTestServer(t)

	rpcResp := doRPC(t, ts, "/projects/test-grove/agents/test-agent/jsonrpc",
		"unknown/method", map[string]string{}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	// -32601 is method not found in JSON-RPC spec.
	if rpcResp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", rpcResp.Error.Code)
	}
}

func TestCancelTaskNotFound(t *testing.T) {
	_, ts, _ := newTestServer(t)

	rpcResp := doRPC(t, ts, "/projects/test-grove/agents/test-agent/jsonrpc",
		"tasks/cancel", map[string]string{"id": "nonexistent-task"}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for cancel of nonexistent task")
	}
}

func TestInvalidJSONRPC(t *testing.T) {
	_, ts, _ := newTestServer(t)

	// Send with wrong version.
	rpcReq, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0",
		"id":      1,
		"method":  "tasks/get",
		"params":  map[string]string{"id": "x"},
	})
	httpReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/projects/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(rpcReq))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", "test-api-key")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rpcResp jsonRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)

	if rpcResp.Error == nil {
		t.Fatal("expected error for invalid JSON-RPC version")
	}
}

func TestMalformedJSON(t *testing.T) {
	_, ts, _ := newTestServer(t)

	httpReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/projects/test-grove/agents/test-agent/jsonrpc",
		bytes.NewReader([]byte(`{not valid json`)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", "test-api-key")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rpcResp jsonRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)

	if rpcResp.Error == nil {
		t.Fatal("expected parse error")
	}
	// -32700 is parse error in JSON-RPC spec.
	if rpcResp.Error.Code != -32700 {
		t.Errorf("error code = %d, want -32700", rpcResp.Error.Code)
	}
}

func TestJSONRPCDeniesNonExposedAgent(t *testing.T) {
	_, ts, _ := newTestServer(t)

	methods := []string{
		"message/send",
		"tasks/get",
		"tasks/cancel",
	}

	for _, method := range methods {
		t.Run("hidden-agent/"+method, func(t *testing.T) {
			rpcResp := doRPC(t, ts, "/projects/test-grove/agents/hidden-agent/jsonrpc",
				method, map[string]string{"id": "x"}, "test-api-key")

			if rpcResp.Error == nil {
				t.Fatalf("expected error for non-exposed agent on %s", method)
			}
			if rpcResp.Error.Message != "agent not found" {
				t.Errorf("error message = %q, want %q", rpcResp.Error.Message, "agent not found")
			}
		})

		t.Run("unknown-project/"+method, func(t *testing.T) {
			rpcResp := doRPC(t, ts, "/projects/unknown-grove/agents/test-agent/jsonrpc",
				method, map[string]string{"id": "x"}, "test-api-key")

			if rpcResp.Error == nil {
				t.Fatalf("expected error for unknown project on %s", method)
			}
		})
	}
}

func TestLegacyGrovePath(t *testing.T) {
	_, ts, _ := newTestServer(t)

	// Test legacy .well-known path (public access)
	resp, err := http.Get(ts.URL + "/groves/test-grove/agents/test-agent/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET legacy agent card: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Test legacy JSON-RPC path (requires auth)
	rpcReq, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tasks/get", Params: json.RawMessage(`{"id":"x"}`)})
	httpReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/groves/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(rpcReq))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", "test-api-key")

	resp, err = http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should be 200 OK (the actual RPC might fail with "task not found" but the route should be authorized)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("legacy RPC: status = %d, want 200", resp.StatusCode)
	}
}

func TestAuthorizeTaskReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	s, err := state.New(filepath.Join(dir, "auth-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://a2a.test.example.com"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(s, nil, nil, cfg, nil, log)

	now := time.Now()
	s.CreateTask(&state.Task{
		ID: "owned-task", ContextID: "ctx-1", ProjectID: "grove-a", AgentSlug: "agent-x",
		State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Task not found returns (nil, nil).
	task, err := b.AuthorizeTask("nonexistent", "grove-a", "agent-x")
	if task != nil || err != nil {
		t.Errorf("AuthorizeTask(nonexistent) = (%v, %v), want (nil, nil)", task, err)
	}

	// Task exists but wrong project returns (nil, nil) — no existence leak.
	task, err = b.AuthorizeTask("owned-task", "grove-b", "agent-x")
	if task != nil || err != nil {
		t.Errorf("AuthorizeTask(wrong project) = (%v, %v), want (nil, nil)", task, err)
	}

	// Task exists but wrong agent returns (nil, nil).
	task, err = b.AuthorizeTask("owned-task", "grove-a", "agent-y")
	if task != nil || err != nil {
		t.Errorf("AuthorizeTask(wrong agent) = (%v, %v), want (nil, nil)", task, err)
	}

	// Correct project and agent returns the task.
	task, err = b.AuthorizeTask("owned-task", "grove-a", "agent-x")
	if err != nil {
		t.Fatalf("AuthorizeTask(correct owner) error: %v", err)
	}
	if task == nil || task.ID != "owned-task" {
		t.Errorf("AuthorizeTask(correct owner) = %v, want task with ID %q", task, "owned-task")
	}
}

func TestRouteInfoContext(t *testing.T) {
	ctx := WithRouteInfo(context.Background(), RouteInfo{ProjectSlug: "proj", AgentSlug: "agt"})
	info, ok := RouteInfoFrom(ctx)
	if !ok {
		t.Fatal("expected route info in context")
	}
	if info.ProjectSlug != "proj" || info.AgentSlug != "agt" {
		t.Errorf("RouteInfo = %+v, want {proj, agt}", info)
	}
}

func TestRouteInfoContextMissing(t *testing.T) {
	_, ok := RouteInfoFrom(context.Background())
	if ok {
		t.Fatal("expected no route info in empty context")
	}
}
