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
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func newLateComponentServer(t *testing.T) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = true
	cfg.BrokerAuthStrictMode = true
	return New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
}

// addLateComponentConn adds a connection named name and returns its key.
func addLateComponentConn(t *testing.T, srv *Server, name string) []byte {
	t.Helper()
	creds := makeTestCreds(name, "broker-1", "http://127.0.0.1:1")
	conn, err := srv.createHubConnection(name, creds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections[name] = conn
	srv.hubMu.Unlock()
	key, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func removeLateComponentConn(srv *Server, name string) {
	srv.hubMu.Lock()
	delete(srv.hubConnections, name)
	srv.hubMu.Unlock()
}

// lateComponentInPlace reports whether the server has the component set up.
func lateComponentInPlace(srv *Server) bool {
	srv.authMiddlewareMu.Lock()
	defer srv.authMiddlewareMu.Unlock()
	return srv.brokerAuthMiddleware != nil
}

// lateComponentStatus sends GET /api/v1/agents to h, passed through
// signRequest with key unless key is nil, and returns the status code.
func lateComponentStatus(h http.Handler, key []byte) int {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	if key != nil {
		signRequest(req, "broker-1", key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestLateComponent_TakesEffectWithoutRestart builds the handler before
// the component exists, as Start does, then sets the component up and
// checks that the handler already built uses it.
func TestLateComponent_TakesEffectWithoutRestart(t *testing.T) {
	srv := newLateComponentServer(t)
	h := srv.Handler()
	if lateComponentInPlace(srv) {
		t.Fatal("precondition: component already set up when the handler was built")
	}
	if got := lateComponentStatus(h, nil); got != http.StatusOK {
		t.Fatalf("before setup: got %d, want %d", got, http.StatusOK)
	}

	key := addLateComponentConn(t, srv, "hub-late")
	srv.buildAuthMiddleware()
	if !lateComponentInPlace(srv) {
		t.Fatal("precondition: component not set up")
	}

	if got := lateComponentStatus(h, nil); got != http.StatusUnauthorized {
		t.Errorf("after setup, plain request: got %d, want %d", got, http.StatusUnauthorized)
	}
	if got := lateComponentStatus(h, key); got != http.StatusOK {
		t.Errorf("after setup, signed request: got %d, want %d", got, http.StatusOK)
	}
}

// TestReplacedComponent_TakesEffectWithoutRestart builds the handler while
// one component is in place, removes it and sets up a new one, and checks
// that the handler already built uses the new one.
func TestReplacedComponent_TakesEffectWithoutRestart(t *testing.T) {
	srv := newLateComponentServer(t)
	oldKey := addLateComponentConn(t, srv, "hub-old")
	srv.buildAuthMiddleware()
	h := srv.Handler()
	if got := lateComponentStatus(h, oldKey); got != http.StatusOK {
		t.Fatalf("before replacement, old key: got %d, want %d", got, http.StatusOK)
	}

	removeLateComponentConn(srv, "hub-old")
	srv.buildAuthMiddleware()
	if lateComponentInPlace(srv) {
		t.Fatal("precondition: component still in place after removal")
	}
	newKey := addLateComponentConn(t, srv, "hub-new")
	srv.buildAuthMiddleware()

	if got := lateComponentStatus(h, newKey); got != http.StatusOK {
		t.Errorf("after replacement, new key: got %d, want %d", got, http.StatusOK)
	}
	if got := lateComponentStatus(h, oldKey); got != http.StatusUnauthorized {
		t.Errorf("after replacement, old key: got %d, want %d", got, http.StatusUnauthorized)
	}
}
