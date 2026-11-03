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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// transportTestCreds returns broker credentials for the hub at endpoint.
func transportTestCreds(name, endpoint string) *brokercredentials.BrokerCredentials {
	return &brokercredentials.BrokerCredentials{
		Name:        name,
		BrokerID:    "rb-transport",
		SecretKey:   base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		HubEndpoint: endpoint,
		AuthMode:    brokercredentials.AuthModeHMAC,
	}
}

// A broker builds no hub connection for a hub it would reach unencrypted
// over the network: the broker's credentials and the agent secrets on the
// control channel and conduit would cross it in the clear.
func TestCreateHubConnection_RefusesPlaintextNonLoopbackHub(t *testing.T) {
	s := &Server{}
	for _, endpoint := range []string{"http://192.0.2.10:8080", "http://hub.example.test", "ws://hub.example.test"} {
		conn, err := s.createHubConnection("remote", transportTestCreds("remote", endpoint))
		if !errors.Is(err, config.ErrPlaintextNonLoopback) {
			t.Errorf("createHubConnection(%s) error = %v, want config.ErrPlaintextNonLoopback", endpoint, err)
		}
		if conn != nil {
			t.Errorf("createHubConnection(%s) returned a connection", endpoint)
		}
	}
	// The server's hub endpoint is checked when the credentials name none.
	s.config.HubEndpoint = "http://192.0.2.10:8080"
	if _, err := s.createHubConnection("remote", transportTestCreds("remote", "")); !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Errorf("createHubConnection with a non-loopback server hub endpoint: error = %v, want a refusal", err)
	}
}

// A loopback http hub and an https hub are accepted.
func TestCreateHubConnection_AcceptsLoopbackAndHTTPS(t *testing.T) {
	s := &Server{}
	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080", "https://hub.example.test"} {
		if _, err := s.createHubConnection("hub", transportTestCreds("hub", endpoint)); err != nil {
			t.Errorf("createHubConnection(%s) = %v, want nil", endpoint, err)
		}
	}
}

// The config-derived connection (bearer token or dev auth) follows the
// same rule, so the configured bearer token never crosses the network in
// the clear.
func TestCreateHubConnectionFromConfig_RefusesPlaintextNonLoopbackHub(t *testing.T) {
	s := &Server{config: ServerConfig{HubEndpoint: "http://192.0.2.10:8080", HubToken: "made-up-hub-token"}}
	if _, err := s.createHubConnectionFromConfig(); !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Fatalf("createHubConnectionFromConfig error = %v, want config.ErrPlaintextNonLoopback", err)
	}
	s.config.HubEndpoint = "http://127.0.0.1:8080"
	if _, err := s.createHubConnectionFromConfig(); err != nil {
		t.Fatalf("loopback hub: %v", err)
	}
}

// Credentials that move a connection to a plaintext network hub are refused
// on reinitialize before any request: the hub at the old (loopback) address
// sees nothing and the connection reports an error.
func TestHubConnectionReinitialize_RefusesPlaintextNonLoopbackHub(t *testing.T) {
	var requests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	cfg := DefaultServerConfig()
	cfg.HeartbeatEnabled = true
	cfg.ControlChannelEnabled = true
	srv := New(cfg, &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
	conn, err := srv.createHubConnection("hub", transportTestCreds("hub", hub.URL))
	if err != nil {
		t.Fatal(err)
	}
	before := requests.Load()

	err = conn.reinitialize(context.Background(), srv, transportTestCreds("hub", "http://192.0.2.10:8080"))
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Fatalf("reinitialize error = %v, want config.ErrPlaintextNonLoopback", err)
	}
	if got := conn.GetStatus(); got != ConnectionStatusError {
		t.Errorf("status = %v, want %v", got, ConnectionStatusError)
	}
	conn.mu.RLock()
	cc, hb := conn.ControlChannel, conn.Heartbeat
	conn.mu.RUnlock()
	if cc != nil || hb != nil {
		t.Errorf("control channel %v / heartbeat %v started for a refused hub", cc, hb)
	}
	if got := requests.Load(); got != before {
		t.Errorf("hub received %d requests after the refusal, want none", got-before)
	}
}

// HubClientForCredentials (the flat-instance activation validation) builds
// no client for credentials naming a plain http hub on a network host, and
// still builds one for a loopback or https hub.
func TestHubClientForCredentials_RefusesPlaintextNonLoopbackHub(t *testing.T) {
	client, err := HubClientForCredentials(transportTestCreds("old", "http://192.0.2.10:8080"))
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Fatalf("HubClientForCredentials error = %v, want config.ErrPlaintextNonLoopback", err)
	}
	if client != nil {
		t.Error("a client was returned for a refused hub")
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080", "https://hub.example.test"} {
		if _, err := HubClientForCredentials(transportTestCreds("ok", endpoint)); err != nil {
			t.Errorf("HubClientForCredentials(%s) = %v, want nil", endpoint, err)
		}
	}
}
