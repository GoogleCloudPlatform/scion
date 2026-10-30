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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)

	conn, err := srv.createHubConnection(name, oldCreds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections[name] = conn
	srv.hubMu.Unlock()
	srv.buildAuthMiddleware()
	mw := srv.loadBrokerMiddleware()
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
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)

	conn, err := srv.createHubConnection(name, oldCreds)
	if err != nil {
		t.Fatal(err)
	}
	srv.hubMu.Lock()
	srv.hubConnections[name] = conn
	srv.hubMu.Unlock()
	srv.buildAuthMiddleware()
	mw := srv.loadBrokerMiddleware()
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

// TestCredentialReload_RetriesFailedReinitialize changes a connection's
// credential file while the reinitialize cannot succeed (a transport mode
// without an audience in the environment), then fixes the environment and
// checks that a later watcher tick applies the same set although that
// connection's file has not changed again: either no file changed, or
// only another connection's file did.
func TestCredentialReload_RetriesFailedReinitialize(t *testing.T) {
	for _, tc := range []struct {
		name         string
		otherChanged bool
	}{
		{"no file changed", false},
		{"another connection's file changed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRetriesFailedReinitialize(t, tc.otherChanged)
		})
	}
}

func testRetriesFailedReinitialize(t *testing.T, otherChanged bool) {
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	const name = "hub-retry"
	oldCreds := makeTestCreds(name, "broker-1", "http://hub1.example.com")
	newCreds := *oldCreds
	newCreds.SecretKey = makeTestCreds(name+"-rotated", "broker-1", "http://hub1.example.com").SecretKey

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
		if err := os.WriteFile(filepath.Join(credDir, c.Name+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCreds(oldCreds)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "broker-1"
	cfg.BrokerName = "test-host"
	cfg.HeartbeatEnabled = false
	cfg.ControlChannelEnabled = false
	cfg.BrokerAuthEnabled = false
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	srv.multiCredStore = brokercredentials.NewMultiStore(credDir)
	// Close every connection at the end, including one the watcher added,
	// so no Start or reinitialize queued by a tick starts anything after
	// the test.
	t.Cleanup(func() {
		for _, c := range srv.hubConnectionList() {
			c.close()
		}
	})

	conn, err := srv.createHubConnection(name, oldCreds)
	if err != nil {
		t.Fatal(err)
	}
	// The retry is backed off; a fake clock moves it forward without
	// sleeping.
	clock := &fakeReinitClock{now: time.Now()}
	conn.now = clock.Now
	srv.hubMu.Lock()
	srv.hubConnections[name] = conn
	srv.hubMu.Unlock()

	waitForStatus := func(want ConnectionStatus) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for conn.GetStatus() != want {
			if time.Now().After(deadline) {
				t.Fatalf("connection status = %q, want %q", conn.GetStatus(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
		// The reinitialize holds the lifecycle lock until it has
		// returned; wait for that.
		conn.lifecycleMu.Lock()
		got := conn.GetStatus()
		conn.lifecycleMu.Unlock()
		if got != want {
			t.Fatalf("connection status = %q after the reinitialize returned, want %q", got, want)
		}
	}

	// The reinitialize for the new set fails.
	t.Setenv(transportauth.EnvTransportMode, "iap")
	srv.credLastScan = time.Time{}
	writeCreds(&newCreds)
	ctx := context.Background()
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	waitForStatus(ConnectionStatusError)

	// Fix the environment. This connection's file does not change, but the
	// next tick, one watcher interval later, must retry the same set.
	t.Setenv(transportauth.EnvTransportMode, "")
	clock.Advance(credentialWatchInterval)
	if otherChanged {
		// Another connection's file changes, so the scan reports a change
		// in which this connection's set compares unchanged. Resetting the
		// last scan time makes the change visible whatever the file
		// system's timestamp granularity.
		writeCreds(makeTestCreds("hub-other", "broker-1", "http://hub2.example.com"))
		srv.credLastScan = time.Time{}
	}
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	waitForStatus(ConnectionStatusConnected)

	snap := conn.snapshot()
	if snap.Credentials == nil || snap.Credentials.SecretKey != newCreds.SecretKey {
		t.Errorf("connection does not hold the new credential set after the retry")
	}
	if got := conn.latestCredentials(); got == nil || got.SecretKey != newCreds.SecretKey {
		t.Errorf("latestCredentials does not return the new credential set after the retry")
	}

	// Once the retry has succeeded, a further tick does not request a
	// retry (reinitGen unchanged) and no failure is recorded. A request
	// would bump reinitGen synchronously inside checkAndReloadCredentials.
	conn.mu.RLock()
	genBefore := conn.reinitGen
	conn.mu.RUnlock()
	if otherChanged {
		// Make the further tick take the changed-files path again.
		writeCreds(makeTestCreds("hub-other", "broker-1", "http://hub2.example.com"))
		srv.credLastScan = time.Time{}
	}
	if err := srv.checkAndReloadCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	conn.mu.RLock()
	genAfter, failed := conn.reinitGen, conn.reinitFailed
	conn.mu.RUnlock()
	if genAfter != genBefore || failed {
		t.Errorf("after a successful retry, a further tick changed reinitGen %d -> %d (reinitFailed=%v), want no retry requested",
			genBefore, genAfter, failed)
	}
}
