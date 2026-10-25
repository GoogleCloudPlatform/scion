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
	"os"
	"path/filepath"
	"strings"
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

	root := filepath.Join("..", "..", "..")
	var files []string
	for _, pattern := range []string{
		"pkg/conduit/*/*.go", "pkg/conduit/*/*/*.go", "pkg/conduit/*.go",
		"pkg/hub/conduit*.go", "cmd/server_conduit*.go", "cmd/server_relay*.go",
	} {
		m, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if strings.Contains(string(b), "InsecureSkipVerify") {
			t.Errorf("%s mentions InsecureSkipVerify: the relay internal hop never skips TLS verification", f)
		}
	}
	// The scan must have covered the hop sources, or it proves nothing.
	if scanned < 20 {
		t.Fatalf("scanned %d source files; the globs no longer match the relay hop sources", scanned)
	}
}
