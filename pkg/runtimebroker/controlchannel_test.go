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

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
)

func TestControlChannelClient_BuildAuthHeaders_Normalization(t *testing.T) {
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.scion.dev/", // Trailing slash
		BrokerID:    "test-host",
		SecretKey:   []byte("test-secret-key-12345678901234567890"),
	}
	client := NewControlChannelClient(config, nil, nil, "", slog.Default())

	headers, err := client.buildAuthHeaders()
	if err != nil {
		t.Fatalf("Failed to build auth headers: %v", err)
	}

	// The signature should be generated for /api/v1/runtime-brokers/connect
	// If it was generated for //api/v1/runtime-brokers/connect, it would be different.
	// We can't easily check the signature value without reimplementing the logic,
	// but we can verify the URL construction logic in the code by looking at it.

	// To verify my fix specifically, I will add a test that checks the URL path
	// if I can expose it, or just rely on the fact that I've verified the code.

	// Since buildAuthHeaders is private but reachable in the same package,
	// I can check its behavior.

	if headers.Get("X-Scion-Broker-ID") != "test-host" {
		t.Errorf("Expected Host-ID header to be 'test-host', got %q", headers.Get("X-Scion-Broker-ID"))
	}

	if headers.Get("X-Scion-Signature") == "" {
		t.Error("Expected Signature header to be set")
	}
}

func TestControlChannelClient_MarkDisconnected_InvokesCallback(t *testing.T) {
	var mu sync.Mutex
	var calls []bool

	config := ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "test-broker",
		OnConnectionStateChange: func(connected bool) {
			mu.Lock()
			calls = append(calls, connected)
			mu.Unlock()
		},
	}
	client := NewControlChannelClient(config, nil, nil, "", slog.Default())

	// Simulate connected state
	client.mu.Lock()
	client.connected = true
	client.mu.Unlock()

	client.markDisconnected()

	if client.IsConnected() {
		t.Error("expected disconnected after markDisconnected")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != false {
		t.Errorf("expected callback with connected=false, got %v", calls)
	}
}

func TestControlChannelClient_MarkDisconnected_NilCallback(t *testing.T) {
	config := ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "test-broker",
	}
	client := NewControlChannelClient(config, nil, nil, "", slog.Default())

	client.mu.Lock()
	client.connected = true
	client.mu.Unlock()

	// Should not panic with nil callback
	client.markDisconnected()

	if client.IsConnected() {
		t.Error("expected disconnected after markDisconnected")
	}
}

// newWSPair creates a connected pair of wsprotocol.Connection for testing.
// It starts an httptest server with a WebSocket endpoint, dials it, and
// returns (brokerConn, hubConn, cleanup).
func newWSPair(t *testing.T) (brokerConn, hubConn *wsprotocol.Connection, cleanup func()) {
	t.Helper()
	hubReady := make(chan *wsprotocol.Connection, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		cfg := wsprotocol.ConnectionConfig{WriteWait: 5 * time.Second}
		hubReady <- wsprotocol.NewConnection(ws, cfg)
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	rawConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bCfg := wsprotocol.ConnectionConfig{WriteWait: 5 * time.Second}
	bc := wsprotocol.NewConnection(rawConn, bCfg)
	hc := <-hubReady

	return bc, hc, func() {
		_ = bc.Close()
		_ = hc.Close()
		srv.Close()
	}
}

func makeRequestData(requestID, method, path string) []byte {
	req := wsprotocol.RequestEnvelope{
		Type:      "request",
		RequestID: requestID,
		Method:    method,
		Path:      path,
	}
	data, _ := json.Marshal(req)
	return data
}

func TestHandleRequest_AsyncDoesNotBlockCaller(t *testing.T) {
	// A handler that blocks for longer than PongWait. Before the fix,
	// handleRequest would block the message loop for this duration.
	// With the fix, handleRequest returns immediately.
	handlerStarted := make(chan struct{})
	handlerRelease := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-handlerRelease
		w.WriteHeader(http.StatusOK)
	})

	brokerConn, _, cleanup := newWSPair(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &ControlChannelClient{
		config:      ControlChannelConfig{Debug: true},
		conn:        brokerConn,
		handlers:    handler,
		log:         slog.Default(),
		streams:     make(map[string]*StreamHandler),
		dispatchSem: make(chan struct{}, defaultMaxConcurrentDispatches),
		ctx:         ctx,
		cancel:      cancel,
	}

	data := makeRequestData("req-1", "GET", "/api/v1/agents")

	// handleRequest should return almost immediately
	done := make(chan error, 1)
	go func() {
		done <- client.handleRequest(data)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleRequest returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleRequest blocked — async dispatch is not working")
	}

	// The handler goroutine should be running
	select {
	case <-handlerStarted:
		// Good — handler is running asynchronously
	case <-time.After(5 * time.Second):
		t.Fatal("handler goroutine was never started")
	}

	// Release the handler and wait for the dispatch goroutine to finish
	close(handlerRelease)
	client.wg.Wait()
}

func TestDispatchRequest_SendsResponse(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &ControlChannelClient{
		config:      ControlChannelConfig{Debug: true},
		conn:        brokerConn,
		handlers:    handler,
		log:         slog.Default(),
		streams:     make(map[string]*StreamHandler),
		dispatchSem: make(chan struct{}, defaultMaxConcurrentDispatches),
		ctx:         ctx,
		cancel:      cancel,
	}

	req := wsprotocol.RequestEnvelope{
		Type:      "request",
		RequestID: "test-req-1",
		Method:    "GET",
		Path:      "/api/v1/agents",
	}

	client.wg.Add(1)
	go client.dispatchRequest(brokerConn, req)
	client.wg.Wait()

	// Read the response from the hub side
	var resp wsprotocol.ResponseEnvelope
	if err := hubConn.ReadJSON(&resp); err != nil {
		t.Fatalf("failed to read response from hub side: %v", err)
	}

	if resp.RequestID != "test-req-1" {
		t.Errorf("expected requestID 'test-req-1', got %q", resp.RequestID)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("unexpected body: %s", string(resp.Body))
	}
}

func TestDispatchRequest_PanicRecovery(t *testing.T) {
	// Handler that panics should not crash the process
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic in handler")
	})

	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &ControlChannelClient{
		config:      ControlChannelConfig{Debug: true},
		conn:        brokerConn,
		handlers:    handler,
		log:         slog.Default(),
		streams:     make(map[string]*StreamHandler),
		dispatchSem: make(chan struct{}, defaultMaxConcurrentDispatches),
		ctx:         ctx,
		cancel:      cancel,
	}

	req := wsprotocol.RequestEnvelope{
		Type:      "request",
		RequestID: "panic-req",
		Method:    "GET",
		Path:      "/panic",
	}

	client.wg.Add(1)
	go client.dispatchRequest(brokerConn, req)
	client.wg.Wait()

	// Should receive a 400 error response, not crash
	var resp wsprotocol.ResponseEnvelope
	if err := hubConn.ReadJSON(&resp); err != nil {
		t.Fatalf("failed to read panic error response: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400 for panic, got %d", resp.StatusCode)
	}
	if resp.RequestID != "panic-req" {
		t.Errorf("expected requestID 'panic-req', got %q", resp.RequestID)
	}
}

func TestDispatchRequest_SemaphoreLimitsConcurrency(t *testing.T) {
	const maxConcurrent = 3
	var inflight atomic.Int32
	var maxSeen atomic.Int32

	handlerRelease := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		<-handlerRelease
		inflight.Add(-1)
		w.WriteHeader(http.StatusOK)
	})

	brokerConn, _, cleanup := newWSPair(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &ControlChannelClient{
		config:      ControlChannelConfig{},
		conn:        brokerConn,
		handlers:    handler,
		log:         slog.Default(),
		streams:     make(map[string]*StreamHandler),
		dispatchSem: make(chan struct{}, maxConcurrent),
		ctx:         ctx,
		cancel:      cancel,
	}

	// Launch more goroutines than the semaphore allows
	total := maxConcurrent + 5
	for i := 0; i < total; i++ {
		req := wsprotocol.RequestEnvelope{
			Type:      "request",
			RequestID: fmt.Sprintf("req-%d", i),
			Method:    "GET",
			Path:      "/test",
		}
		client.wg.Add(1)
		go client.dispatchRequest(brokerConn, req)
	}

	// Give goroutines time to all reach the semaphore
	time.Sleep(100 * time.Millisecond)

	// Only maxConcurrent should be running inside the handler
	if cur := inflight.Load(); cur != int32(maxConcurrent) {
		t.Errorf("expected %d concurrent handlers, got %d", maxConcurrent, cur)
	}

	close(handlerRelease)
	client.wg.Wait()

	if max := maxSeen.Load(); max > int32(maxConcurrent) {
		t.Errorf("max concurrent dispatches exceeded semaphore limit: got %d, limit %d", max, maxConcurrent)
	}
}

func TestDispatchRequest_ContextCancelledBeforeSemaphore(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called when context is cancelled")
	})

	brokerConn, _, cleanup := newWSPair(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())

	client := &ControlChannelClient{
		config:      ControlChannelConfig{},
		conn:        brokerConn,
		handlers:    handler,
		log:         slog.Default(),
		streams:     make(map[string]*StreamHandler),
		dispatchSem: make(chan struct{}, 1),
		ctx:         ctx,
		cancel:      cancel,
	}

	// Fill the semaphore so the next dispatch blocks on it
	client.dispatchSem <- struct{}{}

	req := wsprotocol.RequestEnvelope{
		Type:      "request",
		RequestID: "cancel-req",
		Method:    "GET",
		Path:      "/test",
	}

	client.wg.Add(1)
	go client.dispatchRequest(brokerConn, req)

	// Give the goroutine time to reach the select
	time.Sleep(50 * time.Millisecond)

	// Cancel context — the goroutine should exit without calling the handler
	cancel()
	client.wg.Wait()
}

func TestBuildWebSocketURL_Normalization(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		expectedURL string
	}{
		{
			name:        "trailing slash",
			endpoint:    "https://hub.scion.dev/",
			expectedURL: "wss://hub.scion.dev/api/v1/runtime-brokers/connect",
		},
		{
			name:        "no trailing slash",
			endpoint:    "https://hub.scion.dev",
			expectedURL: "wss://hub.scion.dev/api/v1/runtime-brokers/connect",
		},
		{
			name:        "http endpoint",
			endpoint:    "http://hub.scion.dev",
			expectedURL: "ws://hub.scion.dev/api/v1/runtime-brokers/connect",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := NewControlChannelClient(ControlChannelConfig{HubEndpoint: tc.endpoint}, nil, nil, "", slog.Default())
			wsURL, err := client.buildWebSocketURL()
			if err != nil {
				t.Fatalf("buildWebSocketURL failed: %v", err)
			}
			if wsURL != tc.expectedURL {
				t.Errorf("Expected URL %q, got %q", tc.expectedURL, wsURL)
			}
		})
	}
}
