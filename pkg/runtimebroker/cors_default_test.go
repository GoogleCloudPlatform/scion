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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Broker CORS policy: off by default, explicit opt-in per origin.

// newCORSServer returns a keyed, strict server whose CORS settings come
// from cors, and the key that signs requests for it. Change the auth mode in
// cors to test a permissive server.
func newCORSServer(t *testing.T, cors func(*ServerConfig)) (*Server, []byte) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = true
	cfg.BrokerAuthStrictMode = true
	cfg.StateDir = t.TempDir()
	cors(&cfg)
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	key := addLateComponentConn(t, srv, "hub-a")
	srv.buildAuthMiddleware()
	return srv, key
}

// fromGlobalDefaults applies the broker CORS settings of the built-in
// global config, as the server command maps them.
func fromGlobalDefaults(cfg *ServerConfig) {
	rb := config.DefaultGlobalConfig().RuntimeBroker
	cfg.CORSEnabled = rb.CORSEnabled
	cfg.CORSAllowedOrigins = rb.CORSAllowedOrigins
	cfg.CORSAllowedMethods = rb.CORSAllowedMethods
	cfg.CORSAllowedHeaders = rb.CORSAllowedHeaders
	cfg.CORSMaxAge = rb.CORSMaxAge
}

// corsAllowOrigin sends a cross-origin request from origin and returns the
// Access-Control-Allow-Origin header. Non-OPTIONS requests are signed.
func corsAllowOrigin(h http.Handler, method, origin string, key []byte) string {
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, "/api/v1/agents", strings.NewReader(`{"name":"a1"}`))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, "/api/v1/agents", nil)
	}
	req.Header.Set("Origin", origin)
	if method == http.MethodOptions {
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	} else {
		signRequest(req, "broker-1", key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Header().Get("Access-Control-Allow-Origin")
}

func TestCORSDefault_NoAllowOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		cors func(*ServerConfig)
	}{
		{"global config defaults", fromGlobalDefaults},
		{"server config defaults", func(*ServerConfig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := newCORSServer(t, tc.cors)
			h := srv.Handler()
			// Signed requests only: on a strict server an unsigned preflight
			// is refused by broker auth before CORS runs (see
			// TestCORSDefault_PreflightNoAllowOrigin).
			for _, method := range []string{http.MethodPost, http.MethodGet} {
				if got := corsAllowOrigin(h, method, "https://example.invalid", key); got != "" {
					t.Errorf("%s from a foreign origin: Access-Control-Allow-Origin = %q, want none", method, got)
				}
			}
		})
	}
}

// A preflight carries no signature, so it reaches the CORS policy only on a
// server whose broker auth admits unsigned requests (permissive mode).
// There the default must not allow a foreign origin either.
func TestCORSDefault_PreflightNoAllowOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		cors func(*ServerConfig)
	}{
		{"global config defaults", fromGlobalDefaults},
		{"server config defaults", func(*ServerConfig) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, key := newCORSServer(t, func(cfg *ServerConfig) {
				tc.cors(cfg)
				cfg.BrokerAuthStrictMode = false
			})
			h := srv.Handler()
			if got := corsAllowOrigin(h, http.MethodOptions, "https://example.invalid", key); got != "" {
				t.Errorf("preflight from a foreign origin: Access-Control-Allow-Origin = %q, want none", got)
			}
		})
	}
}

// Control for TestCORSDefault_PreflightNoAllowOrigin: on the same permissive
// server, a preflight from an opted-in origin does reach the CORS policy.
func TestCORSOptIn_PreflightListedOrigin(t *testing.T) {
	srv, key := newCORSServer(t, func(cfg *ServerConfig) {
		cfg.BrokerAuthStrictMode = false
		cfg.CORSEnabled = true
		cfg.CORSAllowedOrigins = []string{"https://ui.example.test"}
	})
	if got := corsAllowOrigin(srv.Handler(), http.MethodOptions, "https://ui.example.test", key); got != "https://ui.example.test" {
		t.Errorf("preflight from the listed origin: Access-Control-Allow-Origin = %q, want the origin", got)
	}
}

func TestCORSDefault_GlobalConfigOff(t *testing.T) {
	rb := config.DefaultGlobalConfig().RuntimeBroker
	if rb.CORSEnabled {
		t.Error("broker CORS enabled by default, want off")
	}
	if len(rb.CORSAllowedOrigins) != 0 {
		t.Errorf("broker CORS default origins = %q, want none", rb.CORSAllowedOrigins)
	}
	sc := DefaultServerConfig()
	if sc.CORSEnabled || len(sc.CORSAllowedOrigins) != 0 {
		t.Errorf("DefaultServerConfig CORS enabled=%v origins=%q, want off with none", sc.CORSEnabled, sc.CORSAllowedOrigins)
	}
}

func TestCORSOptIn_ListedOrigin(t *testing.T) {
	srv, key := newCORSServer(t, func(cfg *ServerConfig) {
		cfg.CORSEnabled = true
		cfg.CORSAllowedOrigins = []string{"https://ui.example.test"}
	})
	h := srv.Handler()
	if got := corsAllowOrigin(h, http.MethodPost, "https://ui.example.test", key); got != "https://ui.example.test" {
		t.Errorf("listed origin: Access-Control-Allow-Origin = %q, want the origin", got)
	}
	if got := corsAllowOrigin(h, http.MethodPost, "https://example.invalid", key); got != "" {
		t.Errorf("unlisted origin: Access-Control-Allow-Origin = %q, want none", got)
	}
}

func TestCORSOptIn_EnabledWithoutOriginsAllowsNone(t *testing.T) {
	srv, key := newCORSServer(t, func(cfg *ServerConfig) {
		fromGlobalDefaults(cfg)
		cfg.CORSEnabled = true
	})
	if got := corsAllowOrigin(srv.Handler(), http.MethodPost, "https://example.invalid", key); got != "" {
		t.Errorf("enabled with default origins: Access-Control-Allow-Origin = %q, want none", got)
	}
}

func TestCORSOptIn_ExplicitWildcard(t *testing.T) {
	srv, key := newCORSServer(t, func(cfg *ServerConfig) {
		cfg.CORSEnabled = true
		cfg.CORSAllowedOrigins = []string{"*"}
	})
	if got := corsAllowOrigin(srv.Handler(), http.MethodPost, "https://dev.example.test", key); got != "https://dev.example.test" {
		t.Errorf("explicit wildcard: Access-Control-Allow-Origin = %q, want the origin", got)
	}
}
