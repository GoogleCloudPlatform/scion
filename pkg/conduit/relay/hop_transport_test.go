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

package relay_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// countingDialer counts every connection attempt an HTTP client or a
// WebSocket dialer makes, and makes none.
type countingDialer struct{ n atomic.Int32 }

func (d *countingDialer) dial(context.Context, string, string) (net.Conn, error) {
	d.n.Add(1)
	return nil, errors.New("countingDialer: no connection")
}

func testPeerAuth(t *testing.T, self string) relay.PeerAuth {
	t.Helper()
	a, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: []byte("0123456789abcdef0123456789abcdef"), SelfID: self})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (d *countingDialer) peerClient(t *testing.T) *relay.PeerClient {
	return &relay.PeerClient{
		HTTP: &http.Client{Transport: &http.Transport{DialContext: d.dial, DialTLSContext: d.dial}},
		WS:   &websocket.Dialer{NetDialContext: d.dial, NetDialTLSContext: d.dial},
		Auth: testPeerAuth(t, "relay-b"),
	}
}

// plaintextPeerEndpoints are relay internal endpoints the hop rule
// refuses: plaintext to a host that is not a literal loopback address.
var plaintextPeerEndpoints = []string{
	"http://192.0.2.10:9810",
	"http://relay-a.internal:9810",
	"http://0.0.0.0:9810",
	"ws://192.0.2.10:9810",
}

// assertHopRefusal checks that err is the hop rule's permanent refusal:
// it wraps config.ErrPlaintextNonLoopback and is neither
// ErrOwnerUnreachable (a router would exclude the relay as dead and
// re-resolve) nor ErrStaleRoute.
func assertHopRefusal(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, config.ErrPlaintextNonLoopback) {
		t.Fatalf("%s = %v, want config.ErrPlaintextNonLoopback", what, err)
	}
	if errors.Is(err, relay.ErrOwnerUnreachable) || errors.Is(err, relay.ErrStaleRoute) {
		t.Fatalf("%s = %v: the refusal must not mark the peer unreachable or stale", what, err)
	}
}

// TestPeerClientRefusesPlaintextNonLoopbackHop: every PeerClient call
// (self probe, drain, session RPC, stream hop) to a non-loopback
// plaintext endpoint is refused before any connection is attempted.
func TestPeerClientRefusesPlaintextNonLoopbackHop(t *testing.T) {
	for _, ep := range plaintextPeerEndpoints {
		t.Run(ep, func(t *testing.T) {
			var d countingDialer
			pc := d.peerClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := pc.Probe(ctx, ep, "relay-a", 1)
			assertHopRefusal(t, "Probe", err)

			_, err = pc.Drain(ctx, ep, "relay-a", 1, "relay-b", time.Second)
			var de *relay.DrainError
			if !errors.As(err, &de) || de.Cause != relay.DrainFailedRefused {
				t.Fatalf("Drain = %v, want a DrainError with cause %q", err, relay.DrainFailedRefused)
			}
			assertHopRefusal(t, "Drain", err)

			rec := registry.SessionRecord{SessionID: "s1", RelayInstanceID: "relay-a", RelayGeneration: 1}
			rs := relay.NewRemoteSession(pc, ep, rec, registry.Want{ProjectID: "p1"})
			_, err = rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "GET", Path: "/x"})
			assertHopRefusal(t, "Call", err)

			_, err = rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
			assertHopRefusal(t, "OpenStream", err)

			if n := d.n.Load(); n != 0 {
				t.Fatalf("%d connection attempts to %s; want none", n, ep)
			}
		})
	}
}

// TestHubClientRefusesPlaintextNonLoopbackHub: the relay's calls to the
// hub internal API are refused before any connection when the hub URL is
// plaintext to a non-loopback host, and the refusal is not
// ErrHubUnavailable (which callers treat as transient).
func TestHubClientRefusesPlaintextNonLoopbackHub(t *testing.T) {
	for _, base := range []string{"http://192.0.2.20:8080", "http://hub.internal:8080/"} {
		var d countingDialer
		hc := &relay.HubClient{
			BaseURL: base,
			HTTP:    &http.Client{Transport: &http.Transport{DialContext: d.dial, DialTLSContext: d.dial}},
			Auth:    testPeerAuth(t, "relay-a"),
		}
		err := hc.Call(context.Background(), "/internal/v1/hub/admit", map[string]string{}, nil)
		if !errors.Is(err, config.ErrPlaintextNonLoopback) || errors.Is(err, relay.ErrHubUnavailable) {
			t.Fatalf("HubClient.Call(%s) = %v, want a permanent ErrPlaintextNonLoopback refusal", base, err)
		}
		if n := d.n.Load(); n != 0 {
			t.Fatalf("%d connection attempts to %s; want none", n, base)
		}
	}
}

// TestHopTLSConfigsNeverSkipVerification: the hop TLS configurations
// verify peers, and no relay internal hop source sets InsecureSkipVerify
// (a pinned invariant of the hop transport policy).
func TestHopTLSConfigsNeverSkipVerification(t *testing.T) {
	c, err := relay.NewHopClientTLS("", "scion-relay.internal")
	if err != nil {
		t.Fatal(err)
	}
	for name, skip := range map[string]bool{
		"client":           c.InsecureSkipVerify,
		"http transport":   relay.NewHopTransport(c).TLSClientConfig.InsecureSkipVerify,
		"websocket dialer": relay.NewHopWSDialer(c).TLSClientConfig.InsecureSkipVerify,
	} {
		if skip {
			t.Errorf("%s: InsecureSkipVerify is set", name)
		}
	}

	for f, src := range hopSources(t) {
		if strings.Contains(src, "InsecureSkipVerify") {
			t.Errorf("%s mentions InsecureSkipVerify: the relay internal hop never skips TLS verification", f)
		}
	}
}

// hopSources reads the non-test sources of the relay internal hop: the
// relay packages, the hub's conduit files and the server's conduit and
// relay commands. It fails unless the globs still match them.
func hopSources(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	out := map[string]string{}
	for _, pattern := range []string{
		"pkg/conduit/*/*.go", "pkg/conduit/*/*/*.go", "pkg/conduit/*.go",
		"pkg/hub/conduit*.go", "cmd/server_conduit*.go", "cmd/server_relay*.go",
	} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			out[f] = string(b)
		}
	}
	// The scan must have covered the hop sources, or it proves nothing.
	if len(out) < 20 {
		t.Fatalf("scanned %d source files; the globs no longer match the relay hop sources", len(out))
	}
	for _, want := range []string{"hoptls.go", "conduit_relay.go", "server_conduit.go"} {
		found := false
		for f := range out {
			found = found || filepath.Base(f) == want
		}
		if !found {
			t.Fatalf("the hop source scan no longer covers %s", want)
		}
	}
	return out
}

// TestHopSourcesBuildHTTPClientsThroughTheHelper: no relay internal hop
// source builds an HTTP client inline or uses the default client, directly
// or through the package helpers; every hop client comes from
// NewHopHTTPClientWithTimeout (hoptls.go), which follows no redirect. A
// call site that goes back to &http.Client{...}, new(http.Client),
// http.DefaultClient or http.Get/Post/Head/PostForm fails here.
func TestHopSourcesBuildHTTPClientsThroughTheHelper(t *testing.T) {
	for f, src := range hopSources(t) {
		if filepath.Base(f) == "hoptls.go" && strings.Contains(f, filepath.Join("conduit", "relay")) {
			continue
		}
		for _, banned := range []string{"http.Client{", "new(http.Client)", "http.DefaultClient",
			"http.Get(", "http.Post(", "http.Head(", "http.PostForm("} {
			if strings.Contains(src, banned) {
				t.Errorf("%s uses %s: build relay hop HTTP clients with relay.NewHopHTTPClientWithTimeout", f, banned)
			}
		}
	}
}

// redirectingPeer answers every request with status, redirecting to
// location, and counts the requests it received.
func redirectingPeer(t *testing.T, status int, location string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, location, status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// recordingHopClient is a hop client (NewHopHTTPClient) whose dials are
// recorded and made for real.
func recordingHopClient(t *testing.T) (*http.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var addrs []string
	c := relay.NewHopHTTPClient(nil)
	c.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		addrs = append(addrs, addr)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), addrs...)
	}
}

// TestHopClientsFollowNoRedirect: a peer, hub internal API or loopback
// terminator that answers 3xx does not move a hop request elsewhere: the
// redirect target, here plaintext to a non-loopback host, is never
// dialed, for the self probe, drain, session RPC and hub call, with 307
// and 308 (which would re-send the body) as well as 302.
func TestHopClientsFollowNoRedirect(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			peer, hits := redirectingPeer(t, status, "http://192.0.2.10:9/elsewhere")
			hc, dialed := recordingHopClient(t)
			pc := &relay.PeerClient{HTTP: hc, Auth: testPeerAuth(t, "relay-b")}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if _, err := pc.Probe(ctx, peer.URL, "relay-a", 1); err == nil {
				t.Error("Probe through a redirect succeeded")
			}
			if _, err := pc.Drain(ctx, peer.URL, "relay-a", 1, "relay-b", time.Second); err == nil {
				t.Error("Drain through a redirect succeeded")
			}
			rec := registry.SessionRecord{SessionID: "s1", RelayInstanceID: "relay-a", RelayGeneration: 1}
			rs := relay.NewRemoteSession(pc, peer.URL, rec, registry.Want{ProjectID: "p1"})
			if _, err := rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "POST", Body: []byte("secret body")}); err == nil {
				t.Error("Call through a redirect succeeded")
			}
			hub := &relay.HubClient{BaseURL: peer.URL, HTTP: hc, Auth: testPeerAuth(t, "relay-a")}
			if err := hub.Call(ctx, "/internal/v1/hub/admit", map[string]string{"k": "v"}, nil); err == nil {
				t.Error("HubClient.Call through a redirect succeeded")
			}

			if n := hits.Load(); n != 4 {
				t.Errorf("the peer received %d requests, want 4 (one per call)", n)
			}
			for _, a := range dialed() {
				if a == "192.0.2.10:9" {
					t.Fatalf("a hop client dialed the redirect target %s", a)
				}
			}
		})
	}
}

// TestHopDefaultClientsFollowNoRedirect: the clients PeerClient and
// HubClient use when none is set follow no redirect either.
func TestHopDefaultClientsFollowNoRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	peer, hits := redirectingPeer(t, http.StatusTemporaryRedirect, target.URL+"/x")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pc := &relay.PeerClient{Auth: testPeerAuth(t, "relay-b")}
	_, _ = pc.Probe(ctx, peer.URL, "relay-a", 1)
	hub := &relay.HubClient{BaseURL: peer.URL, Auth: testPeerAuth(t, "relay-a")}
	_ = hub.Call(ctx, "/internal/v1/hub/admit", map[string]string{"k": "v"}, nil)
	// The peer must have answered both calls, or the test proves nothing.
	if n := hits.Load(); n != 2 {
		t.Fatalf("the redirecting peer received %d requests, want 2", n)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target received %d requests through the default hop clients; want none", n)
	}
}

// TestHopInjectedClientsFollowNoRedirect: a client injected into
// PeerClient or HubClient, here one that follows redirects by default, is
// used without redirects, and the caller's client is not modified.
func TestHopInjectedClientsFollowNoRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	peer, hits := redirectingPeer(t, http.StatusPermanentRedirect, target.URL+"/x")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	injected := &http.Client{}
	pc := &relay.PeerClient{HTTP: injected, Auth: testPeerAuth(t, "relay-b")}
	_, _ = pc.Probe(ctx, peer.URL, "relay-a", 1)
	rec := registry.SessionRecord{SessionID: "s1", RelayInstanceID: "relay-a", RelayGeneration: 1}
	_, _ = relay.NewRemoteSession(pc, peer.URL, rec, registry.Want{ProjectID: "p1"}).Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Body: []byte("body")})
	hub := &relay.HubClient{BaseURL: peer.URL, HTTP: injected, Auth: testPeerAuth(t, "relay-a")}
	_ = hub.Call(ctx, "/internal/v1/hub/admit", map[string]string{"k": "v"}, nil)
	if n := hits.Load(); n != 3 {
		t.Fatalf("the peer received %d requests, want 3", n)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("the redirect target received %d requests through an injected client; want none", n)
	}
	if injected.CheckRedirect != nil {
		t.Fatal("the caller's client was modified")
	}
}
