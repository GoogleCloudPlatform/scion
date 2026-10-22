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
// upstream_unreachable). Once the relay stops, the ingress route answers
// 503 relay_not_admitting (before Admit, so the ingress re-picks), while
// the rest of the internal API keeps its 503 not_serving.
func TestInternalIngressRoute(t *testing.T) {
	w := relaytest.NewWorld(t)
	s := startServer(t, testConfig(t, w, newFakeExperiment(expAnswer{on: true})))
	hub, err := relay.NewHMACPeerAuthFromPeerSecret(relay.HMACPeerAuthConfig{Secret: []byte(testPeerSecret), SelfID: "hub-a"})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(t *testing.T, instance string, sign bool) (int, string) {
		t.Helper()
		status, _, body := dialPath(t, s, hub, relayrole.ConduitPath, instance, sign)
		return status, body
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

	s.Relay.Kill()
	status, h, body := dialPath(t, s, hub, relayrole.ConduitPath, "hub-a", true)
	if status != http.StatusServiceUnavailable || !strings.Contains(body, relay.ReasonRelayNotAdmitting) || h.Get(relay.HeaderRefusalReason) != "" {
		t.Fatalf("stopped relay, ingress route: answer %d %v %s, want 503 %s with no refusal reason header", status, h, body, relay.ReasonRelayNotAdmitting)
	}
	status, h, body = dialPath(t, s, hub, "/internal-api-call", "", true)
	if status != http.StatusServiceUnavailable || h.Get(relay.HeaderRefusalReason) != relay.ReasonNotServing {
		t.Fatalf("stopped relay, internal API: answer %d %v %s, want 503 with reason header %s", status, h, body, relay.ReasonNotServing)
	}
}

// dialPath sends a conduit upgrade to path on the internal listener,
// naming instance in the ingress instance header (none when empty) and
// signed by hub as relay-peer for relay-0 when sign is set.
func dialPath(t *testing.T, s *Server, hub relay.PeerAuth, path, instance string, sign bool) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+s.InternalAddr.String()+path, nil)
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
	return resp.StatusCode, resp.Header, string(b)
}
