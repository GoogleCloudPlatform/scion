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

package relayrole

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Arrival of a dial (design §3.9 "Ingress"): the public listener always
// admits as direct and never trusts the ingress headers; the ingress leg
// admits as ingress only when the instance header names the authenticated
// relay peer.

// upgradeTo sends a WebSocket upgrade to url with extra headers and
// returns the status (101 once admitted).
func upgradeTo(t *testing.T, url string, extra http.Header) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	for k, v := range extra {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestRolePublicDialIgnoresIngressHeaders: on the public listener the
// ingress headers are ignored and counted, even when the instance header
// names a real hub id, and the dial is admitted as direct.
func TestRolePublicDialIgnoresIngressHeaders(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", nil)
	status := upgradeTo(t, f.front.URL+ConduitPath, http.Header{
		http.CanonicalHeaderKey(relay.HeaderIngressInstance):    {"hub-a"},
		http.CanonicalHeaderKey(relay.HeaderIngressLifetimeCap): {"600"},
	})
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", status)
	}
	var req relay.HubAdmitRequest
	f.hub.last("admit", &req)
	if req.Arrival != relay.ArrivalDirect || req.IngressInstance != "" {
		t.Fatalf("admit arrival %q instance %q, want direct and none", req.Arrival, req.IngressInstance)
	}
	if n := f.role.IngressHeaderUntrustedCount(); n != 1 {
		t.Fatalf("untrusted ingress count %d, want 1", n)
	}
}

// TestRoleIngressNeedsTheAuthenticatedPeer: on the ingress leg the dial is
// an ingress arrival only when the instance header equals the
// authenticated peer; a missing or other instance is refused with 403 and
// counted, and the hub is never asked to admit it.
func TestRoleIngressNeedsTheAuthenticatedPeer(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", nil)
	const peer = "hub-a"
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.role.ServeIngress(w, r, peer)
	}))
	t.Cleanup(ingress.Close)

	for name, instance := range map[string]string{"missing": "", "another hub": "hub-b"} {
		t.Run(name, func(t *testing.T) {
			before, untrusted := f.hub.count("admit"), f.role.IngressHeaderUntrustedCount()
			extra := http.Header{}
			if instance != "" {
				extra.Set(relay.HeaderIngressInstance, instance)
			}
			if status := upgradeTo(t, ingress.URL+ConduitPath, extra); status != http.StatusForbidden {
				t.Fatalf("status %d, want 403", status)
			}
			if f.hub.count("admit") != before {
				t.Fatal("the hub was asked to admit")
			}
			if f.role.IngressHeaderUntrustedCount() != untrusted+1 {
				t.Fatal("not counted as untrusted")
			}
		})
	}

	t.Run("the authenticated peer", func(t *testing.T) {
		if status := upgradeTo(t, ingress.URL+ConduitPath, http.Header{http.CanonicalHeaderKey(relay.HeaderIngressInstance): {peer}}); status != http.StatusSwitchingProtocols {
			t.Fatalf("status %d, want 101", status)
		}
		var req relay.HubAdmitRequest
		f.hub.last("admit", &req)
		if req.Arrival != relay.ArrivalIngress || req.IngressInstance != peer {
			t.Fatalf("admit arrival %q instance %q, want ingress and %q", req.Arrival, req.IngressInstance, peer)
		}
	})
}
