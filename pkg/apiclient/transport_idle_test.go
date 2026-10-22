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

package apiclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

type idleTestRoundTripper struct{}

func (idleTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	panic("RoundTrip must not be called during idle connection cleanup")
}

type idleCloseSpy struct {
	idleTestRoundTripper
	calls int
}

func (s *idleCloseSpy) CloseIdleConnections() { s.calls++ }

func TestAuthenticatedHTTPClient_CloseIdleConnectionsForwarding(t *testing.T) {
	// Keep these cases sequential: the fallback replaces DefaultTransport.
	for _, source := range []string{"configured", "default"} {
		for _, supported := range []bool{true, false} {
			name := "unsupported"
			if supported {
				name = "supported"
			}
			t.Run(source+"/"+name, func(t *testing.T) {
				spy := &idleCloseSpy{}
				var base http.RoundTripper = idleTestRoundTripper{}
				if supported {
					base = spy
				}
				if source == "default" {
					previous := http.DefaultTransport
					http.DefaultTransport = base
					t.Cleanup(func() { http.DefaultTransport = previous })
					base = nil
				}
				client := NewTransport("https://hub.invalid",
					WithHTTPClient(&http.Client{Transport: base})).AuthenticatedHTTPClient()
				client.CloseIdleConnections()
				client.CloseIdleConnections()
				want := 0
				if supported {
					want = 2
				}
				if spy.calls != want {
					t.Errorf("forwarded cleanup calls = %d, want %d", spy.calls, want)
				}
			})
		}
	}
}

func TestAuthenticatedHTTPClient_CloseIdleConnectionsPool(t *testing.T) {
	for _, closeIdle := range []bool{false, true} {
		name := "keep_alive"
		if closeIdle {
			name = "close_idle"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" || r.UserAgent() != "idle-test" {
					t.Error("authentication or user agent missing")
				}
				_, _ = io.WriteString(w, "content")
			}))
			defer server.Close()
			base := &http.Transport{}
			defer base.CloseIdleConnections()
			client := NewTransport(server.URL,
				WithHTTPClient(&http.Client{Transport: base, Timeout: 5 * time.Second}),
				WithAuth(&BearerAuth{Token: "test-token"}),
				WithUserAgent("idle-test")).AuthenticatedHTTPClient()
			request := func() bool {
				t.Helper()
				var gotConn, reused atomic.Bool
				ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) {
						reused.Store(info.Reused)
						gotConn.Store(true)
					},
				})
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || string(body) != "content" {
					t.Fatalf("response = %q, error = %v", body, err)
				}
				if !gotConn.Load() {
					t.Fatal("no connection trace")
				}
				return reused.Load()
			}
			if request() {
				t.Fatal("first request unexpectedly reused a connection")
			}
			if closeIdle {
				client.CloseIdleConnections()
			}
			if reused := request(); reused != !closeIdle {
				t.Errorf("second request reused connection = %t, want %t", reused, !closeIdle)
			}
		})
	}
}

func TestAuthenticatedHTTPClient_CloseIdleConnectionsKeepsActive(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "start")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "finish")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	base := &http.Transport{}
	defer base.CloseIdleConnections()
	client := NewTransport(server.URL,
		WithHTTPClient(&http.Client{Transport: base, Timeout: 5 * time.Second})).AuthenticatedHTTPClient()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	client.CloseIdleConnections()
	close(release)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "startfinish" {
		t.Fatalf("active response = %q, error = %v", body, err)
	}
}
