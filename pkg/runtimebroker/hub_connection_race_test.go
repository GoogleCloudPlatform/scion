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
	"encoding/json"
	"log/slog"
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
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// TestHubConnection_ReinitializeConcurrentReaders overlaps
// HubConnection.Reinitialize (the writer) with each goroutine that reads the
// fields it rewrites. Run under -race: before the fix Reinitialize wrote
// Credentials, BrokerID, SecretKey and the client fields without hc.mu, and
// these readers read them without it (ptone/scion#4344).
func TestHubConnection_ReinitializeConcurrentReaders(t *testing.T) {
	// Keep transport auth off so Reinitialize does not try to build an ADC
	// token source from a leaked environment.
	t.Setenv(transportauth.EnvTransportMode, "")
	t.Setenv(transportauth.EnvTransportAudience, "")

	const name = "hub-one"
	creds := makeTestCreds(name, "broker-1", "http://hub1.example.com")

	newServer := func(t *testing.T) (*Server, *HubConnection) {
		t.Helper()
		credDir := filepath.Join(t.TempDir(), "hub-credentials")
		if err := os.MkdirAll(credDir, 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(creds)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(credDir, name+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}

		cfg := DefaultServerConfig()
		cfg.BrokerID = "broker-1"
		cfg.BrokerName = "test-host"
		cfg.HeartbeatEnabled = false
		cfg.ControlChannelEnabled = false
		cfg.BrokerAuthEnabled = false
		srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
		srv.multiCredStore = brokercredentials.NewMultiStore(credDir)

		conn, err := srv.createHubConnection(name, creds)
		if err != nil {
			t.Fatal(err)
		}
		srv.hubMu.Lock()
		srv.hubConnections[name] = conn
		srv.hubMu.Unlock()
		return srv, conn
	}

	connHeaderRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Scion-Hub-Connection", name)
		return r
	}

	readers := []struct {
		name string
		read func(srv *Server, conn *HubConnection)
	}{
		{"handleHubConnections", func(srv *Server, _ *HubConnection) {
			srv.handleHubConnections(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/hub-connections", nil))
		}},
		{"preResolvedHubEndpoint", func(_ *Server, conn *HubConnection) {
			_ = preResolvedHubEndpoint(conn, "http://advertised.example.com")
		}},
		{"resolveHubEndpointFromRequest", func(srv *Server, _ *HubConnection) {
			_ = srv.resolveHubEndpointFromRequest(connHeaderRequest())
		}},
		{"checkAndReloadCredentials", func(srv *Server, _ *HubConnection) {
			// Force a rescan. The file holds the same credentials the
			// writer installs, so the reload compares Credentials (and then
			// rebuilds the auth middleware from SecretKey) without spawning
			// a Reinitialize of its own. Only this goroutine touches
			// credLastScan.
			srv.credLastScan = time.Time{}
			_ = srv.checkAndReloadCredentials(context.Background())
		}},
		{"buildAuthMiddleware", func(srv *Server, _ *HubConnection) {
			srv.buildAuthMiddleware()
		}},
		{"authKeyCount", func(srv *Server, _ *HubConnection) {
			_ = srv.authKeyCount()
		}},
		{"logHubConnections", func(srv *Server, _ *HubConnection) {
			srv.logHubConnections()
		}},
		{"forceHeartbeatAll", func(srv *Server, _ *HubConnection) {
			srv.forceHeartbeatAll("test", "agent-1")
		}},
	}

	for _, rd := range readers {
		t.Run(rd.name, func(t *testing.T) {
			srv, conn := newServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-done:
						return
					default:
					}
					rd.read(srv, conn)
				}
			}()

			for i := 0; i < 20; i++ {
				if err := conn.Reinitialize(ctx, srv, creds); err != nil {
					close(done)
					wg.Wait()
					t.Fatalf("Reinitialize: %v", err)
				}
			}
			close(done)
			wg.Wait()
			conn.Stop()

			snap := conn.snapshot()
			if snap.BrokerID != creds.BrokerID || snap.HubEndpoint != creds.HubEndpoint || snap.Credentials != creds {
				t.Errorf("unexpected connection state after Reinitialize: %+v", snap)
			}
		})
	}
}

// TestControlChannelClient_WaitForConnectedConcurrentSessionID overlaps the
// handshake write of sessionID in waitForConnected with SessionID(), which
// reads it under c.mu. Run under -race: before the fix the write was not
// under c.mu (ptone/scion#4345).
func TestControlChannelClient_WaitForConnectedConcurrentSessionID(t *testing.T) {
	brokerConn, hubConn, cleanup := newWSPair(t)
	defer cleanup()

	client := NewControlChannelClient(ControlChannelConfig{
		HubEndpoint: "https://hub.example.com",
		BrokerID:    "test-broker",
	}, nil, nil, "", slog.Default())
	client.conn = brokerConn

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = client.SessionID()
		}
	}()

	if err := hubConn.WriteJSON(wsprotocol.ConnectedMessage{
		Type:      wsprotocol.TypeConnected,
		BrokerID:  "test-broker",
		SessionID: "session-1",
	}); err != nil {
		close(done)
		wg.Wait()
		t.Fatalf("write connected: %v", err)
	}

	err := client.waitForConnected()
	// Keep reading for a moment after the write so the race detector sees
	// reads on both sides of it.
	time.Sleep(10 * time.Millisecond)
	close(done)
	wg.Wait()
	if err != nil {
		t.Fatalf("waitForConnected: %v", err)
	}
	if got := client.SessionID(); got != "session-1" {
		t.Errorf("SessionID() = %q, want %q", got, "session-1")
	}
}
