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
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// TestCredentialReload_TakesEffectAfterReinitialize changes a connection's
// credential file and checks that, once the connection has been
// reinitialized with it, the new credential is in effect and the old one
// no longer is. The
// reinitialize is held in Stop until the reload has returned, which is
// the usual order in a running broker (Stop waits for the control
// channel and conduit to close).
func TestCredentialReload_TakesEffectAfterReinitialize(t *testing.T) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	hub, _ := newLifecycleTestHub(t)
	const name = "hub-reload"
	oldCreds := makeTestCreds(name, "broker-1", hub.URL)
	newCreds := *oldCreds
	newCreds.SecretKey = makeTestCreds(name+"-rotated", "broker-1", hub.URL).SecretKey

	credDir := filepath.Join(t.TempDir(), "hub-credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCreds := func(c *brokercredentials.BrokerCredentials) {
		t.Helper()
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(credDir, name+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCreds(oldCreds)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = true
	cfg.BrokerAuthEnabled = true
	cfg.BrokerAuthStrictMode = true
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)

	conn, err := srv.createHubConnection(name, oldCreds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections[name] = conn
	srv.hubMu.Unlock()
	srv.buildAuthMiddleware()
	mw := srv.brokerAuthMiddleware
	if mw == nil {
		t.Fatal("expected the credential set to be in use")
	}

	// Hold the conduit dialer goroutine at its end until released, so the
	// reinitialize started by the reload stays in Stop until then.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	conn.conduitExitHook = func() { <-release }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := conn.Start(ctx, srv); err != nil {
		t.Fatal(err)
	}
	defer conn.Stop()
	// Registered after conn.Stop so it runs first: Stop waits for the held
	// conduit dialer, so a failure before the release below must not hang.
	defer releaseAll()

	srv.credLastScan = time.Time{}
	writeCreds(&newCreds)
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	releaseAll()

	newKey := decodeTestKey(t, newCreds.SecretKey)
	oldKey := decodeTestKey(t, oldCreds.SecretKey)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	status := func(key []byte) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		signRequest(req, "broker-1", key)
		rr := httptest.NewRecorder()
		mw.Middleware(ok).ServeHTTP(rr, req)
		return rr.Code
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		reinitialized := string(conn.snapshot().SecretKey) == string(newKey) && conn.GetStatus() == ConnectionStatusConnected
		if reinitialized && status(newKey) == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the credential change: reinitialized=%v, new credential got %d, want %d",
				reinitialized, status(newKey), http.StatusOK)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := status(oldKey); got != http.StatusUnauthorized {
		t.Errorf("old credential got %d, want %d", got, http.StatusUnauthorized)
	}
}

func decodeTestKey(t *testing.T, encoded string) []byte {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestCredentialReload_InEffectMatchesConnectionAfterFailedStep changes a
// connection's credential file to a set whose reinitialize fails after the
// new credential is stored on the connection (a transport mode without an
// audience). What is in effect must still match what the connection holds.
func TestCredentialReload_InEffectMatchesConnectionAfterFailedStep(t *testing.T) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	const name = "hub-partial"
	oldCreds := makeTestCreds(name, "broker-1", "http://hub1.example.com")
	newCreds := *oldCreds
	newCreds.SecretKey = makeTestCreds(name+"-rotated", "broker-1", "http://hub1.example.com").SecretKey
	newCreds.TransportMode = "iap"

	credDir := filepath.Join(t.TempDir(), "hub-credentials")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCreds := func(c *brokercredentials.BrokerCredentials) {
		t.Helper()
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(credDir, name+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCreds(oldCreds)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = true
	cfg.BrokerAuthStrictMode = true
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)

	conn, err := srv.createHubConnection(name, oldCreds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections[name] = conn
	srv.hubMu.Unlock()
	srv.buildAuthMiddleware()
	mw := srv.brokerAuthMiddleware
	if mw == nil {
		t.Fatal("expected the credential set to be in use")
	}

	srv.credLastScan = time.Time{}
	writeCreds(&newCreds)
	if err := srv.checkAndReloadCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}

	newKey := decodeTestKey(t, newCreds.SecretKey)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	status := func(key []byte) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		signRequest(req, "broker-1", key)
		rr := httptest.NewRecorder()
		mw.Middleware(ok).ServeHTTP(rr, req)
		return rr.Code
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		failed := conn.GetStatus() == ConnectionStatusError
		holdsNew := string(conn.snapshot().SecretKey) == string(newKey)
		if failed && holdsNew && status(newKey) == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a failed reinitialize: failed=%v connection holds new credential=%v, new credential got %d, want %d",
				failed, holdsNew, status(newKey), http.StatusOK)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
