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

package main

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseTargetURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantErr  bool
		wantHost string
	}{
		{
			name:     "valid http url",
			input:    "http://10.128.0.10:8080",
			wantErr:  false,
			wantHost: "10.128.0.10:8080",
		},
		{
			name:     "valid https url",
			input:    "https://example.internal:8443",
			wantErr:  false,
			wantHost: "example.internal:8443",
		},
		{
			name:     "missing scheme adds http",
			input:    "10.128.0.10:8080",
			wantErr:  false,
			wantHost: "10.128.0.10:8080",
		},
		{
			name:    "empty string fails",
			input:   "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := parseTargetURL(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tc.input, err)
			}
			if u.Host != tc.wantHost {
				t.Errorf("host = %q, want %q", u.Host, tc.wantHost)
			}
		})
	}
}

func TestHealthCheck(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:9999")
	handler := newMux(target)

	req := httptest.NewRequest(http.MethodGet, "/proxy-healthz", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode json body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body status = %q, want 'ok'", body["status"])
	}
	if body["service"] != "cloudrun-iap-proxy" {
		t.Errorf("body service = %q, want 'cloudrun-iap-proxy'", body["service"])
	}
}

func TestProxyForwardingAndIAPHeaders(t *testing.T) {
	type receivedReq struct {
		Host     string
		Path     string
		Query    string
		JWT      string
		Email    string
		UserID   string
		BodyText string
	}

	var received receivedReq
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		received = receivedReq{
			Host:     r.Host,
			Path:     r.URL.Path,
			Query:    r.URL.RawQuery,
			JWT:      r.Header.Get("X-Goog-IAP-JWT-Assertion"),
			Email:    r.Header.Get("X-Goog-Authenticated-User-Email"),
			UserID:   r.Header.Get("X-Goog-Authenticated-User-Id"),
			BodyText: string(bodyBytes),
		}
		w.Header().Set("X-Backend-Reply", "pong")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend-response"))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("failed to parse backend url: %v", err)
	}

	handler := newMux(backendURL)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents?filter=active", strings.NewReader("sample-payload"))
	req.Header.Set("X-Goog-IAP-JWT-Assertion", "test-iap-jwt-token")
	req.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:developer@example.com")
	req.Header.Set("X-Goog-Authenticated-User-Id", "accounts.google.com:123456789")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if rr.Header().Get("X-Backend-Reply") != "pong" {
		t.Errorf("expected X-Backend-Reply header 'pong', got %q", rr.Header().Get("X-Backend-Reply"))
	}
	if rr.Body.String() != "backend-response" {
		t.Errorf("body = %q, want 'backend-response'", rr.Body.String())
	}

	if received.Host != backendURL.Host {
		t.Errorf("forwarded Host = %q, want %q", received.Host, backendURL.Host)
	}
	if received.Path != "/api/v1/agents" {
		t.Errorf("forwarded Path = %q, want '/api/v1/agents'", received.Path)
	}
	if received.Query != "filter=active" {
		t.Errorf("forwarded Query = %q, want 'filter=active'", received.Query)
	}
	if received.JWT != "test-iap-jwt-token" {
		t.Errorf("forwarded JWT = %q, want 'test-iap-jwt-token'", received.JWT)
	}
	if received.Email != "accounts.google.com:developer@example.com" {
		t.Errorf("forwarded Email = %q, want 'accounts.google.com:developer@example.com'", received.Email)
	}
	if received.UserID != "accounts.google.com:123456789" {
		t.Errorf("forwarded UserID = %q, want 'accounts.google.com:123456789'", received.UserID)
	}
	if received.BodyText != "sample-payload" {
		t.Errorf("forwarded BodyText = %q, want 'sample-payload'", received.BodyText)
	}
}

func TestProxyErrorReturns502(t *testing.T) {
	// Point to unreachable local port
	target, _ := url.Parse("http://127.0.0.1:54321")
	handler := newMux(target)

	req := httptest.NewRequest(http.MethodGet, "/some-route", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway, got %d", rr.Code)
	}
}

// No client-supplied forwarding header reaches the backend: the proxy sets
// X-Forwarded-Proto to the scheme of this connection, X-Forwarded-For to the
// immediate peer and X-Forwarded-Host to the served host, and drops
// Forwarded, whatever the client sent.
func TestProxyReplacesClientForwardingHeaders(t *testing.T) {
	for name, tc := range map[string]struct {
		tls  bool
		want string
	}{
		"plain http connection (Cloud Run)": {want: "http"},
		"proxy terminated TLS":              {tls: true, want: "https"},
	} {
		t.Run(name, func(t *testing.T) {
			var gotProto, gotForwarded, gotFor, gotHost []string
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotProto = r.Header.Values("X-Forwarded-Proto")
				gotForwarded = r.Header.Values("Forwarded")
				gotFor = r.Header.Values("X-Forwarded-For")
				gotHost = r.Header.Values("X-Forwarded-Host")
				w.WriteHeader(http.StatusOK)
			}))
			defer backend.Close()
			backendURL, err := url.Parse(backend.URL)
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/brokers/join", nil)
			req.TLS = nil
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			// What a client might send to claim TLS it did not use, or to
			// contradict the proxy.
			req.Header.Add("X-Forwarded-Proto", "https")
			req.Header.Add("X-Forwarded-Proto", "ftp")
			req.Header.Set("Forwarded", "for=198.51.100.7;proto=https")
			req.Header.Set("X-Forwarded-For", "198.51.100.7")
			req.Header.Set("X-Forwarded-Host", "attacker.example")
			// A client naming the proxy's own headers as hop-by-hop must not
			// be able to strip them.
			req.Header.Set("Connection", "X-Forwarded-Proto, X-Forwarded-Host, X-Forwarded-For")
			req.Host = "hub.example.com"
			req.RemoteAddr = "203.0.113.9:4321"
			rr := httptest.NewRecorder()
			newMux(backendURL).ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if len(gotProto) != 1 || gotProto[0] != tc.want {
				t.Errorf("backend X-Forwarded-Proto = %q, want exactly [%q]", gotProto, tc.want)
			}
			if len(gotForwarded) != 0 {
				t.Errorf("backend Forwarded = %q, want none (client value dropped)", gotForwarded)
			}
			if len(gotFor) != 1 || gotFor[0] != "203.0.113.9" {
				t.Errorf("backend X-Forwarded-For = %q, want exactly the immediate peer [203.0.113.9]", gotFor)
			}
			if len(gotHost) != 1 || gotHost[0] != "hub.example.com" {
				t.Errorf("backend X-Forwarded-Host = %q, want exactly the served host [hub.example.com]", gotHost)
			}
		})
	}
}
