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

package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

func makeTestJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]interface{}{"exp": exp.Unix(), "iss": "test"})
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return fmt.Sprintf("%s.%s.%s", header, payloadB64, sig)
}

// iapMiddleware simulates Google IAP: rejects requests without the expected
// Authorization: Bearer token, returning an HTML login page (mimicking IAP).
func iapMiddleware(expectedToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+expectedToken {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusFound)
			fmt.Fprint(w, "<html><body>Sign in with Google</body></html>")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestWithTransportAuth_CRUDThroughFakeIAP verifies hubclient CRUD operations
// succeed through a fake IAP layer when WithTransportAuth is configured.
func TestWithTransportAuth_CRUDThroughFakeIAP(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	src := transportauth.NewInjectedSource()
	src.SetToken(token, time.Now().Add(1*time.Hour))

	handler := http.NewServeMux()
	handler.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok", Version: "1.0.0"})
	})
	handler.HandleFunc("/api/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"agents":     []Agent{{ID: "a1", Slug: "test", Name: "Test", Status: "running"}},
			"totalCount": 1,
		})
	})
	handler.HandleFunc("/api/v1/projects/proj-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Project{ID: "proj-1", Name: "My Project"})
	})

	server := httptest.NewServer(iapMiddleware(token, handler))
	defer server.Close()

	c, err := New(server.URL, WithTransportAuth(src, transportauth.HeaderAuthorization))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Health check
	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health through IAP failed: %v", err)
	}
	if health.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", health.Status)
	}

	// List agents
	resp, err := c.Agents().List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List agents through IAP failed: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(resp.Agents))
	}

	// Get project
	proj, err := c.Projects().Get(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("Get project through IAP failed: %v", err)
	}
	if proj.Name != "My Project" {
		t.Errorf("expected 'My Project', got %q", proj.Name)
	}
}

// TestWithTransportAuth_WithAgentToken verifies that transport auth composes
// with app-layer agent token auth (different headers, no collision).
func TestWithTransportAuth_WithAgentToken(t *testing.T) {
	transportToken := makeTestJWT(time.Now().Add(1 * time.Hour))
	src := transportauth.NewInjectedSource()
	src.SetToken(transportToken, time.Now().Add(1*time.Hour))

	var receivedAuth, receivedAgentToken string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedAgentToken = r.Header.Get("X-Scion-Agent-Token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	})

	server := httptest.NewServer(iapMiddleware(transportToken, handler))
	defer server.Close()

	c, err := New(server.URL,
		WithAgentToken("my-agent-jwt"),
		WithTransportAuth(src, transportauth.HeaderAuthorization),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}

	if receivedAuth != "Bearer "+transportToken {
		t.Errorf("expected transport token in Authorization, got %q", receivedAuth)
	}
	if receivedAgentToken != "my-agent-jwt" {
		t.Errorf("expected agent token in X-Scion-Agent-Token, got %q", receivedAgentToken)
	}
}

// TestAutoDetection_FromEnv verifies that hubclient.New() auto-detects
// transport auth from SCION_TRANSPORT_TOKEN when no explicit
// WithTransportAuth is provided.
func TestAutoDetection_FromEnv(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	t.Setenv("SCION_TRANSPORT_TOKEN", token)

	var receivedAuth string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	})

	server := httptest.NewServer(iapMiddleware(token, handler))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health with auto-detected transport auth failed: %v", err)
	}

	if receivedAuth != "Bearer "+token {
		t.Errorf("auto-detection should inject transport token, got Authorization: %q", receivedAuth)
	}
}

// TestAutoDetection_OptOut verifies that WithTransportAuth(nil, ...) suppresses
// auto-detection, even when SCION_TRANSPORT_TOKEN is set.
func TestAutoDetection_OptOut(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	t.Setenv("SCION_TRANSPORT_TOKEN", token)

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	}))
	defer server.Close()

	c, err := New(server.URL, WithTransportAuth(nil, transportauth.HeaderAuthorization))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}

	if receivedAuth != "" {
		t.Errorf("opt-out should prevent transport auth injection, got Authorization: %q", receivedAuth)
	}
}

// TestNoAuthHeader_WhenEnvUnset is the critical non-IAP regression test.
// Verifies that when no transport env vars are set, hubclient requests
// do NOT include an Authorization header from transport auth.
func TestNoAuthHeader_WhenEnvUnset(t *testing.T) {
	// Ensure transport env vars are cleared
	for _, key := range []string{
		"SCION_TRANSPORT_TOKEN",
		"SCION_TRANSPORT_AUDIENCE",
		"SCION_TRANSPORT_TOKEN_EXPIRY",
		"SCION_TRANSPORT_MODE",
		"SCION_HUB_OIDC_AUDIENCE",
	} {
		_ = os.Unsetenv(key)
	}

	orig := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = func() bool { return false }
	defer func() { transportauth.IsOnGCEFunc = orig }()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}

	if receivedAuth != "" {
		t.Errorf("no transport auth should be injected when env is unset, got Authorization: %q", receivedAuth)
	}
}

// TestWithTransportAuth_FailedIAP_ReturnsHTMLError verifies that without
// transport auth, an IAP-fronted server returns an HTML error (the
// original #488 failure mode).
func TestWithTransportAuth_FailedIAP_ReturnsHTMLError(t *testing.T) {
	token := makeTestJWT(time.Now().Add(1 * time.Hour))
	handler := http.NewServeMux()
	handler.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{Status: "ok"})
	})

	server := httptest.NewServer(iapMiddleware(token, handler))
	defer server.Close()

	// Ensure no transport env vars
	for _, key := range []string{"SCION_TRANSPORT_TOKEN", "SCION_TRANSPORT_AUDIENCE", "SCION_HUB_OIDC_AUDIENCE"} {
		_ = os.Unsetenv(key)
	}
	orig := transportauth.IsOnGCEFunc
	transportauth.IsOnGCEFunc = func() bool { return false }
	defer func() { transportauth.IsOnGCEFunc = orig }()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Without transport auth, IAP returns HTML → JSON decode fails
	_, err = c.Health(context.Background())
	if err == nil {
		t.Error("expected error when IAP blocks request without transport token")
	}
}
