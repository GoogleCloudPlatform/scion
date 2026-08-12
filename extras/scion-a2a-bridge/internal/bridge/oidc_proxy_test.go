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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// fakeHubOIDC starts a fake hub server that serves OIDC discovery and JWKS.
func fakeHubOIDC(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]interface{}{
			"issuer":                                "https://hub.example.com",
			"jwks_uri":                              "https://hub.example.com/.well-known/jwks.json",
			"response_types_supported":              []string{"id_token"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		jwks := map[string]interface{}{
			"keys": []map[string]interface{}{
				{
					"kty": "RSA",
					"kid": "test-key-1",
					"use": "sig",
					"n":   "test-modulus",
					"e":   "AQAB",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		json.NewEncoder(w).Encode(jwks)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// newTestServerWithHub creates a test bridge server that proxies to a fake hub.
func newTestServerWithHub(t *testing.T, hubURL string) (*Server, *httptest.Server) {
	t.Helper()

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://bridge.example.com",
		},
		Hub: HubConfig{
			Endpoint: hubURL,
			User:     "test@example.com",
		},
		Auth: AuthConfig{
			Scheme: "none",
		},
		Projects: []ProjectConfig{
			{Slug: "test-project", ExposedAgents: []string{"test-agent"}},
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)

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

	return srv, ts
}

func TestOIDCDiscoveryProxy(t *testing.T) {
	hub := fakeHubOIDC(t)
	_, ts := newTestServerWithHub(t, hub.URL)

	resp, err := http.Get(ts.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET /.well-known/openid-configuration: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=300")
	}

	var doc map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Verify jwks_uri is rewritten to the bridge's endpoint.
	wantJWKSURI := "https://bridge.example.com/.well-known/jwks.json"
	if doc["jwks_uri"] != wantJWKSURI {
		t.Errorf("jwks_uri = %q, want %q", doc["jwks_uri"], wantJWKSURI)
	}

	// Verify the original issuer is preserved.
	if doc["issuer"] != "https://hub.example.com" {
		t.Errorf("issuer = %q, want %q", doc["issuer"], "https://hub.example.com")
	}
}

func TestJWKSProxy(t *testing.T) {
	hub := fakeHubOIDC(t)
	_, ts := newTestServerWithHub(t, hub.URL)

	resp, err := http.Get(ts.URL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("GET /.well-known/jwks.json: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=300")
	}

	var jwks map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	keys, ok := jwks["keys"].([]interface{})
	if !ok || len(keys) == 0 {
		t.Fatal("expected non-empty keys array in JWKS response")
	}

	key, ok := keys[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected keys[0] to be an object")
	}
	if key["kid"] != "test-key-1" {
		t.Errorf("kid = %q, want %q", key["kid"], "test-key-1")
	}
}

func TestOIDCProxyCaching(t *testing.T) {
	var fetchCount atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"keys": []map[string]string{{"kid": "k1"}},
		})
	}))
	t.Cleanup(hub.Close)

	_, ts := newTestServerWithHub(t, hub.URL)

	// First request should fetch from hub.
	resp, err := http.Get(ts.URL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", resp.StatusCode)
	}

	// Second request should be served from cache.
	resp, err = http.Get(ts.URL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second GET status = %d, want 200", resp.StatusCode)
	}

	if count := fetchCount.Load(); count != 1 {
		t.Errorf("hub was fetched %d times, want 1 (second should be cached)", count)
	}
}

func TestOIDCProxyHubDown(t *testing.T) {
	// Use a URL that can't be reached.
	_, ts := newTestServerWithHub(t, "http://127.0.0.1:1")

	resp, err := http.Get(ts.URL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when hub is unreachable", resp.StatusCode)
	}
}

func TestOIDCProxyHubReturnsError(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hub.Close)

	_, ts := newTestServerWithHub(t, hub.URL)

	resp, err := http.Get(ts.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when hub returns error", resp.StatusCode)
	}
}

func TestOIDCProxyNoAuthRequired(t *testing.T) {
	// Use a server with apiKey auth to verify the proxy endpoints bypass auth.
	hub := fakeHubOIDC(t)

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://bridge.example.com",
		},
		Hub: HubConfig{
			Endpoint: hub.URL,
			User:     "test@example.com",
		},
		Auth: AuthConfig{
			Scheme: "apiKey",
			APIKey: "secret-key",
		},
		Projects: []ProjectConfig{
			{Slug: "test-project", ExposedAgents: []string{"test-agent"}},
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)

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

	// Both OIDC proxy endpoints should be accessible without auth.
	for _, path := range []string{
		"/.well-known/openid-configuration",
		"/.well-known/jwks.json",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s without auth: status = %d, want 200", path, resp.StatusCode)
			}
		})
	}
}

func TestRewriteJWKSURI(t *testing.T) {
	input := `{
		"issuer": "https://hub.example.com",
		"jwks_uri": "https://hub.example.com/.well-known/jwks.json",
		"response_types_supported": ["id_token"]
	}`

	result, err := rewriteJWKSURI([]byte(input), "https://bridge.example.com")
	if err != nil {
		t.Fatalf("rewriteJWKSURI: %v", err)
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(result, &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	want := "https://bridge.example.com/.well-known/jwks.json"
	if doc["jwks_uri"] != want {
		t.Errorf("jwks_uri = %q, want %q", doc["jwks_uri"], want)
	}

	// Other fields should be preserved.
	if doc["issuer"] != "https://hub.example.com" {
		t.Errorf("issuer = %q, want preserved", doc["issuer"])
	}
}

func TestRewriteJWKSURITrailingSlash(t *testing.T) {
	input := `{"jwks_uri": "https://hub.example.com/.well-known/jwks.json"}`

	result, err := rewriteJWKSURI([]byte(input), "https://bridge.example.com/")
	if err != nil {
		t.Fatalf("rewriteJWKSURI: %v", err)
	}

	var doc map[string]interface{}
	json.Unmarshal(result, &doc)

	want := "https://bridge.example.com/.well-known/jwks.json"
	if doc["jwks_uri"] != want {
		t.Errorf("jwks_uri = %q, want %q (trailing slash should be trimmed)", doc["jwks_uri"], want)
	}
}

func TestOIDCProxyOversizedBody(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write more than oidcProxyMaxBody (1 MB) bytes.
		w.Write(make([]byte, 1<<20+1))
	}))
	t.Cleanup(hub.Close)

	_, ts := newTestServerWithHub(t, hub.URL)

	resp, err := http.Get(ts.URL + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when response exceeds max body size", resp.StatusCode)
	}
}

func TestOIDCProxySingleflight(t *testing.T) {
	var fetchCount atomic.Int32
	// Add a small delay to ensure requests overlap.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"keys": []map[string]string{{"kid": "k1"}},
		})
	}))
	t.Cleanup(hub.Close)

	_, ts := newTestServerWithHub(t, hub.URL)

	// Launch multiple concurrent requests.
	const n = 10
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := http.Get(ts.URL + "/.well-known/jwks.json")
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("status = %d, want 200", resp.StatusCode)
				return
			}
			errs <- nil
		}()
	}

	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}

	// With singleflight, only 1 request should reach the hub.
	if count := fetchCount.Load(); count != 1 {
		t.Errorf("hub was fetched %d times, want 1 (singleflight should deduplicate)", count)
	}
}

func TestOIDCProxyCacheTTL(t *testing.T) {
	cache := newOIDCProxyCache(50 * time.Millisecond)

	cache.set("key", []byte("value"))
	if got := cache.get("key"); string(got) != "value" {
		t.Errorf("cache.get = %q, want %q", got, "value")
	}

	// Wait for TTL to expire.
	time.Sleep(60 * time.Millisecond)

	if got := cache.get("key"); got != nil {
		t.Errorf("cache.get after TTL = %q, want nil", got)
	}
}
