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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseStreamHandler writes n SSE events spaced by gap.
func sseStreamHandler(n int, gap time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < n; i++ {
			time.Sleep(gap)
			if _, err := fmt.Fprintf(w, "data: event-%d\n\n", i); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
}

// readSSEEvents counts "data:" lines until the stream ends.
func readSSEEvents(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	count := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			count++
		}
	}
	return count
}

// TestSSEWriteDeadlineMiddleware_KeepsStreamAlive verifies that an SSE stream
// outlives the server's fixed WriteTimeout when the middleware is installed,
// and (as a control) that it is cut short without it.
func TestSSEWriteDeadlineMiddleware_KeepsStreamAlive(t *testing.T) {
	const (
		events       = 6
		gap          = 100 * time.Millisecond // total ~600ms
		writeTimeout = 250 * time.Millisecond
	)

	t.Run("without middleware the stream is cut short", func(t *testing.T) {
		srv := httptest.NewUnstartedServer(sseStreamHandler(events, gap))
		srv.Config.WriteTimeout = writeTimeout
		srv.Start()
		defer srv.Close()

		if got := readSSEEvents(t, srv.URL); got >= events {
			t.Fatalf("got %d events, expected the WriteTimeout to truncate the stream", got)
		}
	})

	t.Run("with middleware the stream completes", func(t *testing.T) {
		// The middleware installs a rolling per-write deadline; use a short one
		// so the test stays fast while still exceeding the server WriteTimeout.
		srv := httptest.NewUnstartedServer(SSEWriteDeadlineMiddleware(sseStreamHandler(events, gap)))
		srv.Config.WriteTimeout = writeTimeout
		srv.Start()
		defer srv.Close()

		if got := readSSEEvents(t, srv.URL); got != events {
			t.Fatalf("got %d events, want %d", got, events)
		}
	})
}

// TestSSEWriteDeadlineMiddleware_NonSSEUnaffected verifies that ordinary JSON
// responses still honour the server's WriteTimeout (the middleware must not
// disable write deadlines globally).
func TestSSEWriteDeadlineMiddleware_NonSSEUnaffected(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 6; i++ {
			time.Sleep(100 * time.Millisecond)
			if _, err := fmt.Fprintf(w, `{"chunk":%d}`+"\n", i); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})

	srv := httptest.NewUnstartedServer(SSEWriteDeadlineMiddleware(slow))
	srv.Config.WriteTimeout = 250 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	count := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), `{"chunk"`) {
			count++
		}
	}
	if count >= 6 {
		t.Fatalf("non-SSE response returned %d chunks; WriteTimeout should have truncated it", count)
	}
}
