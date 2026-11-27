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

package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func makeFakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return hdr + "." + payload + ".sig"
}

func TestFrontDoor_PoliciesAndHeaderAllowlist(t *testing.T) {
	var expOn atomic.Bool
	expOn.Store(true)

	var mu sync.Mutex
	var lastHubHeaders http.Header
	var lastHubRemoteAddr string

	fakeHub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastHubHeaders = r.Header.Clone()
		lastHubRemoteAddr = r.RemoteAddr
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          "user-123",
				"email":       "alice@example.com",
				"displayName": "Alice",
				"role":        "member",
				"status":      "active",
			})
		case strings.HasSuffix(r.URL.Path, "/message"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message_id":      "msg-1",
				"conversation_id": "conv-1",
				"status":          "delivered",
				"agent":           "echo-t",
			})
		case strings.HasSuffix(r.URL.Path, "/watch"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"events":     []any{},
				"nextCursor": "cur-1",
				"hasMore":    false,
				"truncated":  false,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	backend := NewBackend(BackendOptions{
		HubBaseURL: "http://in-process",
		Transport:  &InProcessTransport{Handler: fakeHub},
		Version:    "test-1.0",
		ExperimentChecker: func(_ context.Context, _ Credential, name string) (bool, error) {
			if name != experiments.MCPServer {
				return false, nil
			}
			return expOn.Load(), nil
		},
		DisableTransportAuth: true,
	})

	srv, err := New(Options{
		AllowedOrigins: []string{"https://allowed.example.com"},
		Version:        "test-1.0",
		Backend:        backend,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	postMCP := func(headers map[string]string, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-client","version":"0.1"}}}`

	// 1. Experiment off -> 404.
	expOn.Store(false)
	resp := postMCP(map[string]string{"Authorization": "Bearer scion_pat_valid"}, initBody)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("experiment off: got status %d, want 404", resp.StatusCode)
	}
	expOn.Store(true)

	// 2. Credential refusals -> 401 + WWW-Authenticate: Bearer realm="scion-mcp".
	agentJWT := makeFakeJWT(t, map[string]any{
		"iss":        "scion-hub",
		"aud":        []string{"scion-hub-api"},
		"sub":        "agent-1",
		"project_id": "proj-1",
		"scopes":     []string{"agent:status:update"},
	})
	credCases := []struct {
		name     string
		headers  map[string]string
		wantCode string
	}{
		{"no credential", nil, "unauthorized"},
		{"both headers", map[string]string{
			"Authorization":       "Bearer scion_pat_abc",
			HeaderFederationToken: "fed-jwt",
		}, "unauthorized"},
		{"non-bearer auth", map[string]string{
			"Authorization": "Basic dXNlcjpwYXNz",
		}, "unauthorized"},
		{"delegated agent token", map[string]string{
			"Authorization": "Bearer scion_adt_secret123",
		}, "unsupported_credential"},
		{"agent JWT", map[string]string{
			"Authorization": "Bearer " + agentJWT,
		}, "unsupported_credential"},
	}
	for _, tc := range credCases {
		t.Run(tc.name, func(t *testing.T) {
			r := postMCP(tc.headers, initBody)
			defer r.Body.Close()
			if r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("got status %d, want 401", r.StatusCode)
			}
			if got := r.Header.Get("WWW-Authenticate"); got != wwwAuthenticateHeader {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, wwwAuthenticateHeader)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["error"] != tc.wantCode {
				t.Fatalf("error code = %q, want %q", body["error"], tc.wantCode)
			}
		})
	}

	// 3. User JWT is NOT misclassified as an agent JWT.
	userJWT := makeFakeJWT(t, map[string]any{
		"iss":  "scion-hub",
		"sub":  "user-123",
		"type": "access",
	})
	if isAgentJWT(userJWT) {
		t.Fatal("user JWT was misclassified as an agent JWT")
	}

	// 4. Origin allowlist -> bad Origin gets 403, allowed Origin gets 200, no Origin gets 200.
	resp = postMCP(map[string]string{
		"Authorization": "Bearer scion_pat_valid",
		"Origin":        "https://evil.example.com",
	}, initBody)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad origin: got status %d, want 403", resp.StatusCode)
	}

	resp = postMCP(map[string]string{
		"Authorization": "Bearer scion_pat_valid",
		"Origin":        "https://allowed.example.com",
	}, initBody)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed origin: got status %d, want 200", resp.StatusCode)
	}

	// 5. Header allowlist (§10 AC4): Cookie and X-Goog-Iap-Jwt-Assertion never reach the Hub.
	callBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{"project":"proj-1","agent":"echo-t","text":"hello"}}}`
	resp = postMCP(map[string]string{
		"Authorization":            "Bearer scion_pat_valid",
		"Cookie":                   "session=secret-cookie",
		"X-Goog-Iap-Jwt-Assertion": "eyJ.iap.sig",
		"X-Custom-Untrusted":       "should-not-forward",
		"User-Agent":               "claude-code/1.2",
	}, callBody)
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tool call: got status %d (%s), want 200", resp.StatusCode, string(bodyBytes))
	}

	mu.Lock()
	gotHeaders := lastHubHeaders
	gotRemote := lastHubRemoteAddr
	mu.Unlock()

	if gotHeaders.Get("Cookie") != "" {
		t.Fatalf("Cookie leaked to Hub request: %q", gotHeaders.Get("Cookie"))
	}
	if gotHeaders.Get("X-Goog-Iap-Jwt-Assertion") != "" {
		t.Fatalf("X-Goog-Iap-Jwt-Assertion leaked to Hub request: %q", gotHeaders.Get("X-Goog-Iap-Jwt-Assertion"))
	}
	if gotHeaders.Get("X-Custom-Untrusted") != "" {
		t.Fatalf("X-Custom-Untrusted leaked to Hub request: %q", gotHeaders.Get("X-Custom-Untrusted"))
	}
	if got := gotHeaders.Get(HeaderScionClient); got != ClientHeaderValue {
		t.Fatalf("X-Scion-Client = %q, want %q", got, ClientHeaderValue)
	}
	if got := gotHeaders.Get("User-Agent"); !strings.HasPrefix(got, "scion-mcp/test-1.0 (") {
		t.Fatalf("User-Agent = %q, want prefix %q", got, "scion-mcp/test-1.0 (")
	}
	if gotRemote == "" {
		t.Fatal("expected RemoteAddr to be propagated to in-process Hub request")
	}

	// 6. Audit log line contains §4.5 fields.
	logText := logBuf.String()
	for _, field := range []string{
		`"msg":"mcp tool call"`,
		`"principal_kind":"user"`,
		`"principal_id":"user-123"`,
		`"credential_class":"bearer"`,
		`"tool":"send_message"`,
		`"conversation_id":"conv-1"`,
		`"result":"ok"`,
		`"hub_status":200`,
		`"duration":`,
	} {
		if !strings.Contains(logText, field) {
			t.Errorf("audit log missing %s; got:\n%s", field, logText)
		}
	}

	// 7. Healthz and Readyz.
	hz, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = hz.Body.Close()
	if hz.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", hz.StatusCode)
	}

	rz, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	_ = rz.Body.Close()
	if rz.StatusCode != http.StatusOK {
		t.Fatalf("/readyz status = %d, want 200", rz.StatusCode)
	}

	// 8. BeginShutdown -> /mcp and /readyz return 503, /healthz stays 200.
	srv.BeginShutdown()

	resp = postMCP(map[string]string{"Authorization": "Bearer scion_pat_valid"}, initBody)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after BeginShutdown /mcp status = %d, want 503", resp.StatusCode)
	}

	rz2, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz after shutdown: %v", err)
	}
	_ = rz2.Body.Close()
	if rz2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after BeginShutdown /readyz status = %d, want 503", rz2.StatusCode)
	}

	hz2, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz after shutdown: %v", err)
	}
	_ = hz2.Body.Close()
	if hz2.StatusCode != http.StatusOK {
		t.Fatalf("after BeginShutdown /healthz status = %d, want 200", hz2.StatusCode)
	}
}

func TestServer_ShutdownNoGoroutineLeak(t *testing.T) {
	fakeHub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "email": "u1@example.com"})
	})
	backend := NewBackend(BackendOptions{
		HubBaseURL: "http://in-process",
		Transport:  &InProcessTransport{Handler: fakeHub},
		ExperimentChecker: func(_ context.Context, _ Credential, _ string) (bool, error) {
			return true, nil
		},
		DisableTransportAuth: true,
	})
	srv, err := New(Options{Backend: backend})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// Settle baseline goroutine count.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, ln)
	}()

	// Exercise one MCP initialize + tools/list via go-sdk client.
	cl := mcp.NewClient(&mcp.Implementation{Name: "leak-test", Version: "0.1"}, nil)
	hc := &http.Client{
		Transport: &staticHeaderTransport{
			base:    &http.Transport{DisableKeepAlives: true},
			headers: map[string]string{"Authorization": "Bearer scion_pat_test"},
		},
	}
	session, err := cl.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + ln.Addr().String() + "/mcp",
		HTTPClient: hc,
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("Connect: %v", err)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		cancel()
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools.Tools))
	}
	_ = session.Close()

	// Cancel context and verify clean shutdown.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not exit within 5s after context cancellation")
	}

	// Wait briefly for goroutines to unwind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutine leak after shutdown: before=%d after=%d", before, after)
	}
}

type staticHeaderTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *staticHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
}
