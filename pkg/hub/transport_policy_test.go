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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func mustTrustedNets(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	nets, err := config.ParseTrustedProxies(entries)
	if err != nil {
		t.Fatal(err)
	}
	return nets
}

// TestConfidentialArrival_RelayAdmitSyntheticPeer: the synthetic peer of
// a relay admission (conduitRelayAdmitRemoteAddr) is never a confidential
// arrival. The predicate's own cases are in pkg/config
// (TestConfidentialArrival there).
func TestConfidentialArrival_RelayAdmitSyntheticPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/join", nil)
	r.RemoteAddr = conduitRelayAdmitRemoteAddr
	r.TLS = nil
	if config.ConfidentialArrival(r, mustTrustedNets(t, "10.20.0.0/16")) {
		t.Error("the relay admit synthetic peer is a confidential arrival")
	}
}

// The hub never sends a request to a broker's HTTP endpoint that is plain
// http on a non-loopback host; nothing is sent and the request is reported
// as not sent.
func TestBrokerHTTPTransport_RefusesPlaintextNonLoopbackEndpoint(t *testing.T) {
	tr := newBrokerHTTPTransport(false, nil)
	for _, endpoint := range []string{"http://192.0.2.10:9800/api/v1/agents", "http://broker.example.test:9800/api/v1/agents"} {
		resp, err := tr.doRequest(context.Background(), "b1", http.MethodPost, endpoint, []byte(`{"agentToken":"x"}`))
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, config.ErrPlaintextNonLoopback) {
			t.Errorf("doRequest(%s) error = %v, want config.ErrPlaintextNonLoopback", endpoint, err)
		}
		if !errors.Is(err, errStartRequestNotSent) {
			t.Errorf("doRequest(%s) error = %v, want errStartRequestNotSent", endpoint, err)
		}
	}
}

// A loopback http broker endpoint still receives the request.
func TestBrokerHTTPTransport_LoopbackEndpointReached(t *testing.T) {
	var hits atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer broker.Close()
	tr := newBrokerHTTPTransport(false, nil)
	resp, err := tr.doRequest(context.Background(), "b1", http.MethodGet, broker.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("doRequest to a loopback broker: %v", err)
	}
	_ = resp.Body.Close()
	if hits.Load() != 1 {
		t.Errorf("broker received %d requests, want 1", hits.Load())
	}
}

// Keystrokes to a broker go through the same transport rule as every other
// broker request: none to a plain http non-loopback endpoint, reported as
// not dispatched.
func TestBrokerHTTPTransport_ExecuteKeysRefusesPlaintextNonLoopbackEndpoint(t *testing.T) {
	tr := newBrokerHTTPTransport(false, nil)
	_, err := tr.ExecuteKeys(context.Background(), "b1", "http://192.0.2.10:9800", "agent-1", agentkeys.BrokerRequest{ProjectID: "p1"})
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Errorf("ExecuteKeys error = %v, want agentkeys.ErrNotDispatched", err)
	}
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Errorf("ExecuteKeys error = %v, want config.ErrPlaintextNonLoopback", err)
	}
}

// The route()-driven HTTP fallback (no control channel here, no live
// affinity owner) to a stored non-loopback http endpoint fails closed, for
// lifecycle dispatch and for keys.
func TestHybridBrokerClient_RouteHTTPRefusesPlaintextNonLoopbackEndpoint(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, NewHTTPRuntimeBrokerClient(), nil, false)
	c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
	const endpoint = "http://192.0.2.10:9800"
	if got := c.route(context.Background(), "b-remote-replica", endpoint); got != routeHTTP {
		t.Fatalf("route = %v, want routeHTTP (the fallback under test)", got)
	}
	_, err := c.StartAgent(context.Background(), "b-remote-replica", endpoint, "agent-1", "project-1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Errorf("StartAgent error = %v, want config.ErrPlaintextNonLoopback", err)
	}
	_, err = c.ExecuteKeys(context.Background(), "b-remote-replica", endpoint, "agent-1", agentkeys.BrokerRequest{ProjectID: "project-1"})
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Errorf("ExecuteKeys error = %v, want config.ErrPlaintextNonLoopback", err)
	}
}

// The main broker client follows a redirect only on the same host and only
// to a target the transport rule accepts: a broker response cannot make the
// hub resend a signed body to another host or to a plaintext network host.
func TestBrokerHTTPTransport_RedirectPolicy(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	// The same server under another host name: a different host to the client.
	otherByName := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
		case "/final":
			w.WriteHeader(http.StatusOK)
		case "/cross-host":
			http.Redirect(w, r, otherByName+"/x", http.StatusTemporaryRedirect)
		case "/plaintext-network":
			http.Redirect(w, r, "http://192.0.2.10:9800/x", http.StatusTemporaryRedirect)
		}
	}))
	defer broker.Close()
	tr := newBrokerHTTPTransport(false, nil)

	resp, err := tr.doRequest(context.Background(), "b1", http.MethodPost, broker.URL+"/same", []byte(`{}`))
	if err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("same-host redirect status = %d, want 200", resp.StatusCode)
	}

	resp, err = tr.doRequest(context.Background(), "b1", http.MethodPost, broker.URL+"/cross-host", []byte(`{"agentToken":"x"}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Errorf("cross-host redirect error = %v, want a refusal", err)
	}
	if otherHits.Load() != 0 {
		t.Errorf("the other host received %d requests, want none", otherHits.Load())
	}

	resp, err = tr.doRequest(context.Background(), "b1", http.MethodPost, broker.URL+"/plaintext-network", []byte(`{}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("redirect to a plaintext network host was followed, want a refusal")
	}
}

// The redirect policy never follows a same-host redirect that downgrades the
// scheme from https, on a network host (where the transport rule also
// refuses plain http) and on a loopback host (where it would accept it); an
// https same-host redirect and a plain-http loopback same-host redirect are
// followed.
func TestCheckBrokerRedirect_RefusesSchemeDowngrade(t *testing.T) {
	hop := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodPost, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, tc := range []struct {
		from, to string
		ok       bool
	}{
		{"https://hub.example.com:9800/a", "http://hub.example.com:9800/b", false},
		{"https://127.0.0.1:9800/a", "http://127.0.0.1:9800/b", false},
		{"https://[::1]:9800/a", "http://[::1]:9800/b", false},
		{"https://hub.example.com:9800/a", "https://hub.example.com:9800/b", true},
		{"http://127.0.0.1:9800/a", "http://127.0.0.1:9800/b", true},
	} {
		err := checkBrokerRedirect(hop(tc.to), []*http.Request{hop(tc.from)})
		if tc.ok && err != nil {
			t.Errorf("%s -> %s: %v, want followed", tc.from, tc.to, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "downgrades https")) {
			t.Errorf("%s -> %s: error = %v, want a downgrade refusal", tc.from, tc.to, err)
		}
	}
	// A later hop is compared with the original request, so https -> https ->
	// http is refused too.
	err := checkBrokerRedirect(hop("http://127.0.0.1:9800/c"), []*http.Request{hop("https://127.0.0.1:9800/a"), hop("https://127.0.0.1:9800/b")})
	if err == nil || !strings.Contains(err.Error(), "downgrades https") {
		t.Errorf("two-hop downgrade: error = %v, want a refusal", err)
	}
}

// End to end through the hub's broker client: a TLS broker that answers with
// a same-host redirect to http is not followed.
func TestBrokerHTTPTransport_RedirectDowngradeRefused(t *testing.T) {
	broker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			http.Redirect(w, r, "http://"+r.Host+"/plain", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer broker.Close()
	// The hub's broker client wraps http.DefaultTransport at construction; make
	// it trust the test broker's certificate for this test.
	orig := http.DefaultTransport
	base, ok := orig.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	tr := base.Clone()
	tr.TLSClientConfig = broker.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = orig })

	bt := newBrokerHTTPTransport(false, nil)
	resp, err := bt.doRequest(context.Background(), "b1", http.MethodPost, broker.URL+"/ok", []byte(`{}`))
	if err != nil {
		t.Fatalf("control: plain TLS request: %v", err)
	}
	_ = resp.Body.Close()
	resp, err = bt.doRequest(context.Background(), "b1", http.MethodPost, broker.URL+"/down", []byte(`{"agentToken":"x"}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "downgrades https") {
		t.Errorf("downgrade redirect error = %v, want a refusal", err)
	}
}
