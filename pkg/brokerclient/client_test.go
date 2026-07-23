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

package brokerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// TestBrokerHealth_HappyPath verifies that Health() succeeds when /healthz
// returns 200 application/json with a valid HealthResponse body.
func TestBrokerHealth_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("expected path /healthz, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runtimebroker.HealthResponse{Status: "ok"})
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if health.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", health.Status)
	}
}

// TestBrokerHealth_GFEInterception verifies that when /healthz returns 200
// with Content-Type: text/plain (proxy interception), Health() returns nil
// and an error that mentions "reverse proxy" or "GFE".
func TestBrokerHealth_GFEInterception(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	health, err := client.Health(context.Background())
	if err == nil {
		t.Fatalf("expected error for proxy-intercepted response, got nil (health=%v)", health)
	}
	msg := err.Error()
	if !strings.Contains(msg, "reverse proxy") && !strings.Contains(msg, "GFE") {
		t.Errorf("expected error to mention 'reverse proxy' or 'GFE', got: %v", err)
	}
}

// TestBrokerHealth_ServerError verifies that a non-2xx response (503) from
// /healthz is surfaced as an error by Health().
func TestBrokerHealth_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	health, err := client.Health(context.Background())
	if err == nil {
		t.Fatalf("expected error for 503 response, got nil (health=%v)", health)
	}
}

// TestBrokerHealth_NonJSONErrorPage verifies that a non-2xx response with a
// non-JSON Content-Type (e.g. a 502 HTML error page from a load balancer) is
// surfaced as a real status error, NOT misdiagnosed as proxy interception.
// This guards the 2xx guard added to the proxy-interception check.
func TestBrokerHealth_NonJSONErrorPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway) // 502 from a load balancer
		_, _ = w.Write([]byte(`<html><body>Bad Gateway</body></html>`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	health, err := client.Health(context.Background())
	if err == nil {
		t.Fatalf("expected error for 502 response, got nil (health=%v)", health)
	}
	// Must NOT produce a proxy-hint diagnosis — the real status error should surface.
	msg := err.Error()
	if strings.Contains(msg, "reverse proxy") || strings.Contains(msg, "GFE") {
		t.Errorf("502 from load balancer should not produce a proxy-hint diagnosis, got: %v", err)
	}
}
