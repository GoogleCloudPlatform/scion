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

//go:build !no_sqlite

package relayserver

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole"
)

// TestInternalIngressRoute: the internal listener serves proxied conduit
// dials on GET /api/v1/conduit after the relay-peer checks. An unsigned
// dial gets 401; a signed dial naming another instance than the signing
// peer gets 403; a signed dial naming the signing peer reaches admission
// (here the hub is unreachable, so admission answers 503
// upstream_unreachable).
func TestInternalIngressRoute(t *testing.T) {
	w := relaytest.NewWorld(t)
	s := startServer(t, testConfig(t, w, newFakeExperiment(expAnswer{on: true})))
	hub, err := relay.NewHMACPeerAuthFromPeerSecret(relay.HMACPeerAuthConfig{Secret: []byte(testPeerSecret), SelfID: "hub-a"})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(t *testing.T, instance string, sign bool) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+s.InternalAddr.String()+relayrole.ConduitPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if instance != "" {
			req.Header.Set(relay.HeaderIngressInstance, instance)
		}
		if sign {
			relay.SetPeerTarget(req, "relay-0", s.Relay.Generation())
			if err := hub.Sign(req); err != nil {
				t.Fatal(err)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if status, _ := dial(t, "hub-a", false); status != http.StatusUnauthorized {
		t.Fatalf("unsigned dial: status %d, want 401", status)
	}
	if status, _ := dial(t, "hub-b", true); status != http.StatusForbidden {
		t.Fatalf("another instance: status %d, want 403", status)
	}
	if status, body := dial(t, "hub-a", true); status != http.StatusServiceUnavailable || !strings.Contains(body, relay.ReasonUpstreamUnreachable) {
		t.Fatalf("the signing peer: answer %d %s, want admission's 503 %s", status, body, relay.ReasonUpstreamUnreachable)
	}
}
