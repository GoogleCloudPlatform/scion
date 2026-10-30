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

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Broker request policy while no hub keys are loaded: with broker auth
// enabled, only the health endpoints are served.

// newNoKeysServer returns a server with broker auth enabled (as every
// production constructor sets it) and no hub keys.
func newNoKeysServer(t *testing.T) *Server {
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
	return New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
}

type noKeysRequest struct {
	name    string
	method  string
	path    string
	body    string
	origin  string
	upgrade bool // send WebSocket upgrade headers
}

// noKeysRefusedRequests are the API calls the broker must refuse while no
// hub keys are loaded.
var noKeysRefusedRequests = []noKeysRequest{
	{name: "agent create", method: http.MethodPost, path: "/api/v1/agents", body: `{"name":"a1"}`},
	{name: "agent list", method: http.MethodGet, path: "/api/v1/agents"},
	{name: "agent stop", method: http.MethodPost, path: "/api/v1/agents/a1/stop"},
	{name: "agent attach websocket upgrade", method: http.MethodGet, path: "/api/v1/agents/a1/attach", upgrade: true},
	{name: "agent delete", method: http.MethodDelete, path: "/api/v1/agents/a1"},
	{name: "workspace apply", method: http.MethodPost, path: "/api/v1/workspace/apply", body: `{}`},
	{name: "image pull", method: http.MethodPost, path: "/api/v1/images/pull", body: `{}`},
	{name: "info", method: http.MethodGet, path: "/api/v1/info"},
	{name: "hub connections", method: http.MethodGet, path: "/api/v1/hub-connections"},
	{name: "cross-origin preflight", method: http.MethodOptions, path: "/api/v1/agents", origin: "https://example.invalid"},
	{name: "cross-origin create", method: http.MethodPost, path: "/api/v1/agents", body: `{"name":"a1"}`, origin: "https://example.invalid"},
}

func serveNoKeys(h http.Handler, rq noKeysRequest, prefix string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(rq.method, prefix+rq.path, strings.NewReader(rq.body))
	if rq.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if rq.upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	if rq.origin != "" {
		req.Header.Set("Origin", rq.origin)
		if rq.method == http.MethodOptions {
			req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func assertNoKeysPolicy(t *testing.T, h http.Handler, prefix string) {
	t.Helper()
	for _, rq := range noKeysRefusedRequests {
		t.Run(rq.name, func(t *testing.T) {
			rr := serveNoKeys(h, rq, prefix)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s: status %d, want %d (body %q)", rq.method, rq.path, rr.Code, http.StatusServiceUnavailable, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "broker_auth_unavailable") {
				t.Errorf("body %q, want error code broker_auth_unavailable", rr.Body.String())
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("Access-Control-Allow-Origin = %q, want none", got)
			}
		})
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run("health "+path, func(t *testing.T) {
			rr := serveNoKeys(h, noKeysRequest{method: http.MethodGet, path: path}, prefix)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s: status %d, want 200 (body %q)", path, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestNoKeys_OnlyHealthServed(t *testing.T) {
	srv := newNoKeysServer(t)
	if srv.loadBrokerMiddleware() != nil {
		t.Fatal("precondition: no auth middleware expected without keys")
	}
	assertNoKeysPolicy(t, srv.Handler(), "")
}

func TestNoKeys_OnlyHealthServed_Prefixed(t *testing.T) {
	srv := newNoKeysServer(t)
	assertNoKeysPolicy(t, srv.PrefixedHandler("/instances/broker-1"), "/instances/broker-1")
}

// A credential that fails to load must not leave the API served without
// auth: the broker serves health only.
func TestNoKeys_CredentialLoadFailureRefusesAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://127.0.0.1:1"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = true
	cfg.BrokerAuthStrictMode = true
	cfg.StateDir = t.TempDir()
	cfg.TemplateCacheDir = t.TempDir()
	cfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{
		Name:        "local",
		BrokerID:    "broker-1",
		SecretKey:   "%%% not base64 %%%",
		HubEndpoint: cfg.HubEndpoint,
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	if n := srv.authKeyCount(); n != 0 {
		t.Fatalf("precondition: %d auth keys loaded, want 0 after a failed credential load", n)
	}
	assertNoKeysPolicy(t, srv.Handler(), "")
}

// Once a keyed connection is added, signed requests pass and unsigned ones
// get the auth error, without rebuilding the handler.
func TestNoKeys_GateLiftsWhenKeysLoad(t *testing.T) {
	srv := newNoKeysServer(t)
	h := srv.Handler()

	if got := lateComponentStatus(h, nil); got != http.StatusServiceUnavailable {
		t.Fatalf("before keys: unsigned status %d, want %d", got, http.StatusServiceUnavailable)
	}

	key := addLateComponentConn(t, srv, "hub-a")
	srv.buildAuthMiddleware()

	if got := lateComponentStatus(h, key); got != http.StatusOK {
		t.Errorf("after keys: signed status %d, want 200", got)
	}
	if got := lateComponentStatus(h, nil); got != http.StatusUnauthorized {
		t.Errorf("after keys: unsigned status %d, want 401", got)
	}
}

// With broker auth disabled (only settable in code) the gate does not
// apply.
func TestNoKeys_AuthDisabledServesAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = false
	cfg.StateDir = t.TempDir()
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	if got := lateComponentStatus(srv.Handler(), nil); got != http.StatusOK {
		t.Errorf("auth disabled: status %d, want 200", got)
	}
}
