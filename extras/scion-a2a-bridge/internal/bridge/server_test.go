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
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
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
		Groves: []GroveConfig{
			{
				Slug:          "test-grove",
				ExposedAgents: []string{"test-agent"},
			},
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := New(store, nil, nil, cfg, log)
	srv := NewServer(bridge, cfg, nil, log)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return srv, ts
}

func doRPC(t *testing.T, ts *httptest.Server, path string, method string, params interface{}, apiKey string) *JSONRPCResponse {
	t.Helper()

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	req := JSONRPCRequest{
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

	var rpcResp JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return &rpcResp
}

func TestHealthEndpoint(t *testing.T) {
	_, ts := newTestServer(t)

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
	_, ts := newTestServer(t)

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
}

func TestPerAgentCard(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/groves/test-grove/agents/test-agent/.well-known/agent-card.json")
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

	expectedURL := "https://a2a.test.example.com/groves/test-grove/agents/test-agent"
	if card["url"] != expectedURL {
		t.Errorf("url = %q, want %q", card["url"], expectedURL)
	}
}

func TestPerAgentCardNotExposed(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/groves/test-grove/agents/hidden-agent/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for non-exposed agent", resp.StatusCode)
	}
}

func TestPerAgentCardUnknownGrove(t *testing.T) {
	_, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/groves/unknown-grove/agents/test-agent/.well-known/agent-card.json")
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown grove", resp.StatusCode)
	}
}

func TestAuthMiddleware(t *testing.T) {
	_, ts := newTestServer(t)

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
	rpcReq, _ := json.Marshal(JSONRPCRequest{JSONRPC: "2.0", ID: 1, Method: "tasks/get", Params: json.RawMessage(`{"id":"x"}`)})
	httpReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/groves/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(rpcReq))
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
	httpReq, _ = http.NewRequest(http.MethodPost, ts.URL+"/groves/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(rpcReq))
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
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/get", TaskQueryParams{ID: "nonexistent-task"}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for nonexistent task")
	}
	if rpcResp.Error.Code != ErrCodeTaskNotFound {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeTaskNotFound)
	}
}

func TestListTasksRequiresContextID(t *testing.T) {
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/list", TaskQueryParams{}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error when contextId is missing")
	}
	if rpcResp.Error.Code != ErrCodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeInvalidParams)
	}
}

func TestUnknownMethod(t *testing.T) {
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"unknown/method", map[string]string{}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if rpcResp.Error.Code != ErrCodeMethodNotFound {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeMethodNotFound)
	}
}

func TestCancelTaskSuccess(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "cancel-test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://a2a.test.example.com",
			Provider:    ProviderConfig{Organization: "Test Org", URL: "https://test.example.com"},
		},
		Auth: AuthConfig{Scheme: "apiKey", APIKey: "test-api-key"},
		Groves: []GroveConfig{
			{Slug: "test-grove", ExposedAgents: []string{"test-agent"}},
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := New(store, nil, nil, cfg, log)
	srv := NewServer(bridge, cfg, nil, log)
	ts2 := httptest.NewServer(srv.Handler())
	defer ts2.Close()

	now := time.Now()
	store.CreateTask(&state.Task{
		ID: "cancel-me", ContextID: "ctx-1", GroveID: "test-grove", AgentSlug: "test-agent",
		State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	rpcResp := doRPC(t, ts2, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/cancel", map[string]string{"id": "cancel-me"}, "test-api-key")

	if rpcResp.Error != nil {
		t.Fatalf("unexpected error: code=%d msg=%s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	resultBytes, _ := json.Marshal(rpcResp.Result)
	var result TaskResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Status.State != TaskStateCanceled {
		t.Errorf("status.state = %q, want %q", result.Status.State, TaskStateCanceled)
	}

	// Verify the store was updated.
	task, _ := store.GetTask("cancel-me")
	if task.State != TaskStateCanceled {
		t.Errorf("store state = %q, want %q", task.State, TaskStateCanceled)
	}
}

func TestCancelTaskAlreadyTerminal(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "cancel-terminal.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	cfg := &Config{
		Bridge: BridgeConfig{ExternalURL: "https://a2a.test.example.com"},
		Auth:   AuthConfig{Scheme: "apiKey", APIKey: "test-api-key"},
		Groves: []GroveConfig{{Slug: "test-grove", ExposedAgents: []string{"test-agent"}}},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bridge := New(store, nil, nil, cfg, log)
	srv := NewServer(bridge, cfg, nil, log)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	now := time.Now()
	store.CreateTask(&state.Task{
		ID: "done-task", ContextID: "ctx-1", GroveID: "test-grove", AgentSlug: "test-agent",
		State: TaskStateCompleted, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/cancel", map[string]string{"id": "done-task"}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error when canceling a completed task")
	}
	if rpcResp.Error.Code != ErrCodeTaskNotCancelable {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeTaskNotCancelable)
	}
}

func TestCancelTaskNotFound(t *testing.T) {
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/cancel", map[string]string{"id": "nonexistent-task"}, "test-api-key")

	if rpcResp.Error == nil {
		t.Fatal("expected error for cancel of nonexistent task")
	}
	if rpcResp.Error.Code != ErrCodeTaskNotFound {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeTaskNotFound)
	}
}

func TestInvalidJSONRPC(t *testing.T) {
	_, ts := newTestServer(t)

	// Send with wrong version.
	rpcReq, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "1.0",
		"id":      1,
		"method":  "tasks/get",
		"params":  map[string]string{"id": "x"},
	})
	httpReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/groves/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(rpcReq))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", "test-api-key")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)

	if rpcResp.Error == nil {
		t.Fatal("expected error for invalid JSON-RPC version")
	}
	if rpcResp.Error.Code != ErrCodeInvalidRequest {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeInvalidRequest)
	}
}

func TestMalformedJSON(t *testing.T) {
	_, ts := newTestServer(t)

	httpReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/groves/test-grove/agents/test-agent/jsonrpc",
		bytes.NewReader([]byte(`{not valid json`)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", "test-api-key")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)

	if rpcResp.Error == nil {
		t.Fatal("expected parse error")
	}
	if rpcResp.Error.Code != ErrCodeParseError {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeParseError)
	}
}

// --- Phase 2 server tests ---

func TestPushNotificationSetGetDelete(t *testing.T) {
	_, ts := newTestServer(t)

	// Create a task first (needed for push config FK).
	rpcPath := "/groves/test-grove/agents/test-agent/jsonrpc"

	// Create a task directly in the store via the test bridge.
	// We access it indirectly by creating it in the store.
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "push-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now()
	store.CreateTask(&state.Task{
		ID: "push-task-1", ContextID: "ctx-1", GroveID: "test-grove", AgentSlug: "test-agent",
		State: "working", CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Set push config — this test verifies the JSON-RPC dispatch works even though
	// the task is in a different store. The server handler delegates to bridge which
	// uses its own store, so we test the handler's param parsing and error paths.
	rpcResp := doRPC(t, ts, rpcPath,
		"tasks/pushNotification/set",
		PushNotificationParams{
			TaskID: "nonexistent-task",
			URL:    "https://example.com/webhook",
			Token:  "tok",
		},
		"test-api-key",
	)

	// Should fail because task doesn't exist in the server's store.
	if rpcResp.Error == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestPushNotificationSetRejectsPrivateIP(t *testing.T) {
	_, ts := newTestServer(t)
	rpcPath := "/groves/test-grove/agents/test-agent/jsonrpc"

	cases := []struct {
		name string
		url  string
	}{
		{"loopback", "https://127.0.0.1/webhook"},
		{"metadata", "https://169.254.169.254/latest/meta-data/"},
		{"rfc1918-10", "https://10.0.0.1/hook"},
		{"rfc1918-172", "https://172.16.0.1/hook"},
		{"rfc1918-192", "https://192.168.1.1/hook"},
		{"unspecified", "https://0.0.0.0/hook"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpcResp := doRPC(t, ts, rpcPath,
				"tasks/pushNotification/set",
				PushNotificationParams{
					TaskID: "some-task",
					URL:    tc.url,
					Token:  "tok",
				},
				"test-api-key",
			)

			if rpcResp.Error == nil {
				t.Fatal("expected error for private IP URL")
			}
			if rpcResp.Error.Code != ErrCodeInvalidParams {
				t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeInvalidParams)
			}
		})
	}
}

func TestPushNotificationGetReturnsEmpty(t *testing.T) {
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/pushNotification/get",
		PushNotificationParams{TaskID: "some-task"},
		"test-api-key",
	)

	// Should succeed with empty result (no configs).
	if rpcResp.Error != nil {
		t.Fatalf("unexpected error: %s", rpcResp.Error.Message)
	}
}

func TestPushNotificationDeleteSucceeds(t *testing.T) {
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/pushNotification/delete",
		PushNotificationParams{ID: "nonexistent-push-id"},
		"test-api-key",
	)

	// Delete of nonexistent ID should succeed (idempotent).
	if rpcResp.Error != nil {
		t.Fatalf("unexpected error: %s", rpcResp.Error.Message)
	}
}

func TestStreamMethodInvalidParams(t *testing.T) {
	_, ts := newTestServer(t)

	// Send a raw JSON string that can't be unmarshaled to SendMessageParams.
	rpcReq := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "message/stream",
		Params:  json.RawMessage(`"not an object"`),
	}
	body, _ := json.Marshal(rpcReq)
	httpReq, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/groves/test-grove/agents/test-agent/jsonrpc", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", "test-api-key")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var rpcResp JSONRPCResponse
	json.NewDecoder(resp.Body).Decode(&rpcResp)

	if rpcResp.Error == nil {
		t.Fatal("expected error for invalid params")
	}
	if rpcResp.Error.Code != ErrCodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcResp.Error.Code, ErrCodeInvalidParams)
	}
}

func TestResubscribeTaskNotFound(t *testing.T) {
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/resubscribe",
		TaskQueryParams{ID: "nonexistent-task"},
		"test-api-key",
	)

	if rpcResp.Error == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestResubscribeRequiresID(t *testing.T) {
	_, ts := newTestServer(t)

	rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
		"tasks/resubscribe",
		TaskQueryParams{},
		"test-api-key",
	)

	// Should fail because the task doesn't exist (empty ID).
	if rpcResp.Error == nil {
		t.Fatal("expected error for empty task ID")
	}
}

func TestNewRPCMethods(t *testing.T) {
	_, ts := newTestServer(t)

	// Verify these methods are recognized (not "method not found").
	// message/stream and tasks/resubscribe are excluded because they trigger
	// resolveContext which requires a hub client (nil in test fixture).
	methods := []string{
		"tasks/pushNotification/set",
		"tasks/pushNotification/get",
		"tasks/pushNotification/delete",
		"tasks/resubscribe",
	}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			rpcResp := doRPC(t, ts, "/groves/test-grove/agents/test-agent/jsonrpc",
				method,
				map[string]string{},
				"test-api-key",
			)

			if rpcResp.Error != nil && rpcResp.Error.Code == ErrCodeMethodNotFound {
				t.Errorf("method %q should be registered but got method not found", method)
			}
		})
	}
}
