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
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// publicDialPrincipals are the credential headers of each principal kind
// that dials the relay's public listener.
var publicDialPrincipals = map[string]http.Header{
	"user":   {"Authorization": {"Bearer user-token"}},
	"agent":  {"X-Scion-Agent-Token": {"agent-token"}},
	"broker": {"X-Scion-Broker-Id": {"broker-1"}, "X-Scion-Timestamp": {"1"}, "X-Scion-Nonce": {"n"}, "X-Scion-Signature": {"sig"}},
}

// publicDial sends a conduit dial to the public listener's handler as if
// it arrived from remote, with TLS or not, and returns the answer.
func publicDial(t *testing.T, s *Server, remote string, withTLS bool, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com"+relayrole.ConduitPath+"?v=1", nil)
	req.RemoteAddr = remote
	req.TLS = nil
	if withTLS {
		req.TLS = &tls.ConnectionState{}
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	for k, v := range headers {
		req.Header[http.CanonicalHeaderKey(k)] = v
	}
	rec := httptest.NewRecorder()
	s.public.Handler.ServeHTTP(rec, req)
	return rec
}

func refusedForTransport(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), ErrCodeTransportNotConfidential)
}

// reachedAdmission reports whether the dial got past the arrival check to
// the relay's admission: this test relay has no reachable hub, so
// admission answers 503 unavailable (upstream_unreachable).
func reachedAdmission(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), "upstream_unreachable")
}

// TestPublicListenerRefusesPlaintextArrival: a conduit dial on the relay's
// public listener that did not arrive over TLS, from this host, or from a
// trusted proxy that terminated TLS is refused with 403 before admission or
// upgrade, for every principal kind. TLS and loopback arrivals, and a
// trusted proxy's https arrival, reach admission. With the default (empty)
// trusted proxy list a forwarded protocol is never trusted.
func TestPublicListenerRefusesPlaintextArrival(t *testing.T) {
	w := relaytest.NewWorld(t)
	s := startServer(t, testConfig(t, w, newFakeExperiment(expAnswer{on: true})))
	if len(s.cfg.TrustedProxies) != 0 {
		t.Fatalf("default trusted proxies = %v, want none", s.cfg.TrustedProxies)
	}
	for kind, h := range publicDialPrincipals {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				remote  string
				tls     bool
				extra   http.Header
				refused bool
			}{
				{name: "plaintext from a network peer", remote: "192.0.2.10:40000", refused: true},
				{name: "plaintext from a network peer with a forged forwarded protocol", remote: "192.0.2.10:40000",
					extra: http.Header{"X-Forwarded-Proto": {"https"}}, refused: true},
				{name: "loopback peer relaying for another client", remote: "127.0.0.1:40000",
					extra: http.Header{"X-Forwarded-For": {"198.51.100.7"}}, refused: true},
				{name: "TLS", remote: "192.0.2.10:40000", tls: true},
				{name: "loopback", remote: "127.0.0.1:40000"},
			} {
				hdr := h.Clone()
				for k, v := range tc.extra {
					hdr[k] = v
				}
				rec := publicDial(t, s, tc.remote, tc.tls, hdr)
				if got := refusedForTransport(rec); got != tc.refused {
					t.Errorf("%s: refused = %v (HTTP %d %q), want %v", tc.name, got, rec.Code, rec.Body.String(), tc.refused)
				}
				if !tc.refused && !reachedAdmission(rec) {
					t.Errorf("%s: an admitted dial did not reach admission (HTTP %d %q)", tc.name, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// TestPublicListenerTrustsListedProxyOnly: with server.trusted_proxies set,
// a listed proxy's X-Forwarded-Proto: https dial reaches admission, while
// the same header from a peer outside the list, or http from a listed
// proxy, is refused.
func TestPublicListenerTrustsListedProxyOnly(t *testing.T) {
	trusted, err := config.ParseTrustedProxies([]string{"10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	cfg.TrustedProxies = trusted
	s := startServer(t, cfg)
	user := publicDialPrincipals["user"]
	for _, tc := range []struct {
		name    string
		remote  string
		proto   string
		refused bool
	}{
		{name: "listed proxy, https", remote: "10.20.3.4:40000", proto: "https"},
		{name: "listed proxy, http", remote: "10.20.3.4:40000", proto: "http", refused: true},
		{name: "unlisted peer, https", remote: "192.0.2.10:40000", proto: "https", refused: true},
	} {
		hdr := user.Clone()
		hdr.Set("X-Forwarded-Proto", tc.proto)
		rec := publicDial(t, s, tc.remote, false, hdr)
		if got := refusedForTransport(rec); got != tc.refused {
			t.Errorf("%s: refused = %v (HTTP %d %q), want %v", tc.name, got, rec.Code, rec.Body.String(), tc.refused)
		}
		if !tc.refused && !reachedAdmission(rec) {
			t.Errorf("%s: an admitted dial did not reach admission (HTTP %d %q)", tc.name, rec.Code, rec.Body.String())
		}
	}
}
