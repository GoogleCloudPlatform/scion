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
	"bufio"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// TestSSEThroughFullMiddlewareStack verifies that SSE streaming works
// correctly when both RequestLogMiddleware (outermost) and InstrumentHandler
// (inner) wrap the handler. This is the middleware stack used in production:
//
//	RequestLogMiddleware -> InstrumentHandler -> handler
//
// The test checks:
//  1. SSE events stream through without being buffered or dropped.
//  2. Flush() works through the full middleware chain.
//  3. The correct Content-Type (text/event-stream) is preserved.
func TestSSEThroughFullMiddlewareStack(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("http.Flusher not available through middleware chain")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Write multiple SSE events to verify streaming works.
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			flusher.Flush()
		}
	})

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	// Build the same middleware stack as production:
	// RequestLogMiddleware -> InstrumentHandler -> handler
	handler := InstrumentHandler(inner, m)
	handler = logging.RequestLogMiddleware(
		slog.Default(), "scion-a2a-bridge-test", BridgePathPatterns(), 0,
	)(handler)

	req := httptest.NewRequest(http.MethodPost, "/projects/my-proj/agents/my-agent/jsonrpc", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body := rec.Body.String()
	for i := 0; i < 3; i++ {
		want := fmt.Sprintf("data: event-%d\n\n", i)
		if !strings.Contains(body, want) {
			t.Errorf("body missing SSE event %d: %q", i, body)
		}
	}
}

// TestSSEFlushThroughLiveServer verifies SSE streaming via a real TCP
// connection (httptest.Server), proving Flush() propagates through
// the middleware chain to the actual network socket.
func TestSSEFlushThroughLiveServer(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("http.Flusher not available in live server")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: live-%d\n\n", i)
			flusher.Flush()
		}
	})

	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	handler := InstrumentHandler(inner, m)
	handler = logging.RequestLogMiddleware(
		slog.Default(), "scion-a2a-bridge-test", BridgePathPatterns(), 0,
	)(handler)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/projects/test-proj/agents/test-agent/jsonrpc")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Read SSE events from the streamed response.
	scanner := bufio.NewScanner(resp.Body)
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("got %d SSE events, want 3: %v", len(events), events)
	}
	for i, ev := range events {
		want := fmt.Sprintf("data: live-%d", i)
		if ev != want {
			t.Errorf("event[%d] = %q, want %q", i, ev, want)
		}
	}
}

// TestBridgePathPatternsEnrichContext verifies that RequestLogMiddleware
// with BridgePathPatterns correctly extracts project and agent IDs from
// bridge URL paths and stores them in the request context.
func TestBridgePathPatternsEnrichContext(t *testing.T) {
	tests := []struct {
		path      string
		wantProj  string
		wantAgent string
	}{
		{"/projects/my-proj/agents/my-agent/jsonrpc", "my-proj", "my-agent"},
		{"/projects/p1/agents/a1/.well-known/agent-card.json", "p1", "a1"},
		{"/groves/old-grove/agents/old-agent/jsonrpc", "old-grove", "old-agent"},
		{"/healthz", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			var gotProj, gotAgent string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				meta := logging.RequestMetaFromContext(r.Context())
				if meta != nil {
					gotProj = meta.ProjectID
					gotAgent = meta.AgentID
				}
				w.WriteHeader(http.StatusOK)
			})

			handler := logging.RequestLogMiddleware(
				slog.Default(), "test", BridgePathPatterns(), 0,
			)(inner)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if gotProj != tt.wantProj || gotAgent != tt.wantAgent {
				t.Errorf("path %q: project=%q agent=%q, want project=%q agent=%q",
					tt.path, gotProj, gotAgent, tt.wantProj, tt.wantAgent)
			}
		})
	}
}
