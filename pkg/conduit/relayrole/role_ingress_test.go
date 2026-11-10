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
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
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

// TestRoleAnswersAfterAdmitAreNotRepickable pins what a hub ingress relies
// on (design §3.9 "Relay selection"): no answer the relay gives after its
// admission call to the hub carries the refusal reason header, and none is
// a 503 relay_not_admitting, even when the hub's answer carries either. So
// an ingress never re-picks a dial the hub may have admitted.
func TestRoleAnswersAfterAdmitAreNotRepickable(t *testing.T) {
	notAdmitting := map[string]any{"error": map[string]any{
		"code": "unavailable", "message": "unavailable", "details": map[string]string{"reason": relay.ReasonRelayNotAdmitting},
	}}
	reasonHeader := http.Header{http.CanonicalHeaderKey(relay.HeaderRefusalReason): {relay.ReasonNotServing}}
	for _, tc := range []struct {
		name   string
		answer *hubAnswer // nil: the hub is unreachable
	}{
		{name: "hub 503 with the reason header and body", answer: &hubAnswer{status: http.StatusServiceUnavailable, header: reasonHeader, body: notAdmitting}},
		{name: "hub 401 with the reason header", answer: &hubAnswer{status: http.StatusUnauthorized, header: reasonHeader, body: notAdmitting}},
		{name: "hub 409 with the reason header", answer: &hubAnswer{status: http.StatusConflict, header: reasonHeader, body: notAdmitting}},
		{name: "hub 429 with the reason header", answer: &hubAnswer{status: http.StatusTooManyRequests, header: reasonHeader, body: notAdmitting}},
		{name: "hub unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoleFixture(t)
			if tc.answer == nil {
				f.hub.srv.Close()
			} else {
				a := *tc.answer
				f.hub.setAnswer(func(string, []byte) hubAnswer { return a })
			}
			req, err := http.NewRequest(http.MethodGet, f.front.URL+ConduitPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if tc.answer != nil && f.hub.count("admit") != 1 {
				t.Fatalf("admit calls %d, want 1", f.hub.count("admit"))
			}
			if got := resp.Header.Get(relay.HeaderRefusalReason); got != "" {
				t.Fatalf("answer %d carries %s %q after the admission call", resp.StatusCode, relay.HeaderRefusalReason, got)
			}
			if resp.StatusCode == http.StatusServiceUnavailable && strings.Contains(string(body), relay.ReasonRelayNotAdmitting) {
				t.Fatalf("answer 503 %s after the admission call", body)
			}
			if resp.StatusCode == http.StatusSwitchingProtocols {
				t.Fatalf("the dial was admitted")
			}
		})
	}
}

// dialWelcome opens a conduit session at url as an agent with the extra
// headers and returns its Welcome.
func dialWelcome(t *testing.T, url string, extra http.Header) *conduitv1.Welcome {
	t.Helper()
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(url, "http") + ConduitPath,
		Header: func(context.Context) (http.Header, error) {
			h := http.Header{"Authorization": {"Bearer dialer-credential"}}
			for k, v := range extra {
				h[k] = v
			}
			return h, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, wel, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, relaytest.AgentHello(testAgentID, "L1", "", "pty"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return wel
}

// TestRoleIngressLifetimeCap (design §3.9 "Source of the cap", v2.21.53):
// on a trusted ingress leg a valid cap header lowers the session's
// lifetime cap and never raises it; an absent or invalid one leaves the
// relay's cap and is counted (ingress_cap_invalid). The header is not
// read on an untrusted leg or on a direct dial.
func TestRoleIngressLifetimeCap(t *testing.T) {
	const relayCap = 3500 * time.Second
	f := newRoleFixtureWith(t, func(c *relay.Config) { c.LifetimeCap = relayCap })
	f.admitAs(registry.PrincipalAgent, "tok-1", nil)
	const peer = "hub-a"
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.role.ServeIngress(w, r, peer)
	}))
	t.Cleanup(ingress.Close)
	relayHint := uint32((relayCap - relay.LifetimeGoAwayLead) / time.Second)
	instanceKey, capKey := http.CanonicalHeaderKey(relay.HeaderIngressInstance), http.CanonicalHeaderKey(relay.HeaderIngressLifetimeCap)

	for _, tc := range []struct {
		name     string
		header   []string // nil: no cap header
		wantHint uint32
		invalid  bool
	}{
		{name: "lower cap", header: []string{"600"}, wantHint: 540},
		{name: "the 90s floor", header: []string{"90"}, wantHint: 30},
		{name: "above the relay cap never extends", header: []string{"7200"}, wantHint: relayHint},
		{name: "the 24h ceiling, above the relay cap", header: []string{"86400"}, wantHint: relayHint},
		{name: "absent", wantHint: relayHint, invalid: true},
		{name: "empty", header: []string{""}, wantHint: relayHint, invalid: true},
		{name: "not a number", header: []string{"abc"}, wantHint: relayHint, invalid: true},
		{name: "signed", header: []string{"+600"}, wantHint: relayHint, invalid: true},
		{name: "negative", header: []string{"-5"}, wantHint: relayHint, invalid: true},
		{name: "fraction", header: []string{"90.5"}, wantHint: relayHint, invalid: true},
		{name: "below 90s", header: []string{"89"}, wantHint: relayHint, invalid: true},
		{name: "above 24h", header: []string{"86401"}, wantHint: relayHint, invalid: true},
		{name: "too many digits", header: []string{"000600"}, wantHint: relayHint, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.role.IngressCapInvalidCount()
			extra := http.Header{instanceKey: {peer}}
			if tc.header != nil {
				extra[capKey] = tc.header
			}
			wel := dialWelcome(t, ingress.URL, extra)
			if got := wel.GetLifetimeHintS(); got != tc.wantHint {
				t.Fatalf("Welcome.lifetime_hint_s = %d, want %d", got, tc.wantHint)
			}
			want := before
			if tc.invalid {
				want++
			}
			if got := f.role.IngressCapInvalidCount(); got != want {
				t.Fatalf("ingress_cap_invalid count = %d, want %d", got, want)
			}
		})
	}

	t.Run("untrusted instance: refused, the cap is not read", func(t *testing.T) {
		before := f.role.IngressCapInvalidCount()
		if status := upgradeTo(t, ingress.URL+ConduitPath, http.Header{instanceKey: {"hub-b"}, capKey: {"abc"}}); status != http.StatusForbidden {
			t.Fatalf("status %d, want 403", status)
		}
		if f.role.IngressCapInvalidCount() != before {
			t.Fatal("the cap header was read on an untrusted leg")
		}
	})

	t.Run("direct dial: the header is ignored", func(t *testing.T) {
		before, untrusted := f.role.IngressCapInvalidCount(), f.role.IngressHeaderUntrustedCount()
		wel := dialWelcome(t, f.front.URL, http.Header{capKey: {"600"}})
		if got := wel.GetLifetimeHintS(); got != relayHint {
			t.Fatalf("Welcome.lifetime_hint_s = %d, want the relay's %d", got, relayHint)
		}
		if f.role.IngressCapInvalidCount() != before {
			t.Fatal("the cap header was read on a direct dial")
		}
		if f.role.IngressHeaderUntrustedCount() != untrusted+1 {
			t.Fatal("the header on a direct dial was not counted as untrusted")
		}
	})
}

// TestRoleIngressSessionsCarryTheInstance (design §3.9 "Drain through the
// proxy"): a session admitted over a trusted ingress leg carries the
// ingress instance, so a drain for that instance reaches it; a direct
// session carries none.
func TestRoleIngressSessionsCarryTheInstance(t *testing.T) {
	f := newRoleFixture(t)
	f.admitAs(registry.PrincipalAgent, "tok-1", nil)
	const peer = "hub-a"
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.role.ServeIngress(w, r, peer)
	}))
	t.Cleanup(ingress.Close)

	// The Welcome is sent while a session is still pending; a drain only
	// sees registered sessions, so wait until the relay has registered each.
	registered := func(wel *conduitv1.Welcome) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, _, ok := f.node.Relay.Local(ctx, wel.GetSessionId()); !ok {
			t.Fatalf("session %s was not registered on the relay", wel.GetSessionId())
		}
	}
	registered(dialWelcome(t, f.front.URL, nil))
	if n := f.node.Relay.DrainIngress(peer, 0); n != 0 {
		t.Fatalf("drain matched %d direct sessions, want 0", n)
	}
	registered(dialWelcome(t, ingress.URL, http.Header{http.CanonicalHeaderKey(relay.HeaderIngressInstance): {peer}}))
	if n := f.node.Relay.DrainIngress("hub-b", 0); n != 0 {
		t.Fatalf("a drain for another hub matched %d sessions, want 0", n)
	}
	if n := f.node.Relay.DrainIngress(peer, 0); n != 1 {
		t.Fatalf("drain matched %d sessions, want the 1 ingress session", n)
	}
}
