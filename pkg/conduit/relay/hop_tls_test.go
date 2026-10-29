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

package relay_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const hopServerName = "scion-relay.internal"

// hopTLS builds the TLS a world's nodes use from operator files: the
// listener serves files' certificate, and the clients trust caFile and
// verify serverName.
func hopTLS(t *testing.T, files relaytest.HopTLSFiles, caFile, serverName string) *relaytest.HopTLS {
	t.Helper()
	srv, err := relay.NewHopServerTLS(files.CertFile, files.KeyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := relay.NewHopClientTLS(caFile, serverName)
	if err != nil {
		t.Fatal(err)
	}
	return &relaytest.HopTLS{Server: srv, Client: cli}
}

// newTLSPair is newPair with TLS on the internal hop: both relays serve
// the same operator certificate (SAN hopServerName) and verify each other
// against the operator CA and that one server name.
func newTLSPair(t *testing.T, cfg conduit.Config) (*pair, relaytest.HopTLSFiles) {
	t.Helper()
	files := relaytest.NewHopTLSFiles(t, hopServerName)
	w := relaytest.NewWorld(t)
	w.HopTLS = hopTLS(t, files, files.CAFile, hopServerName)
	// StartNode runs the required self-check, a signed probe of the node's
	// own https endpoint: it passes only if TLS verification succeeds.
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", agentPrincipal("L1", 1))
	target, _ := a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
	rec := w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session
	return &pair{w: w, a: a, b: b, target: target, rec: rec,
		want: registry.Want{ProjectID: project, Incarnation: "L1"}}, files
}

// TestHopTLSEndToEnd: with TLS on the internal hop, relay B reaches a
// target held by relay A over https and wss: an RPC and a stream (data
// spanning frames and windows, half-close) round-trip, and the endpoints
// both relays registered are https.
func TestHopTLSEndToEnd(t *testing.T) {
	p, _ := newTLSPair(t, echoConfig())
	for _, n := range []*relaytest.Node{p.a, p.b} {
		if ep := n.Relay.InternalEndpoint(); !strings.HasPrefix(ep, "https://") {
			t.Fatalf("%s registered %q, want an https endpoint", n.Relay.InstanceID(), ep)
		}
	}
	rs := p.remote()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "exec", Body: []byte("over tls")})
	if err != nil || resp.GetStatus() != 200 || string(resp.GetBody()) != "echo:over tls" {
		t.Fatalf("Call over TLS = %v, %v", resp, err)
	}
	st, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatalf("OpenStream over TLS: %v", err)
	}
	payload := bytes.Repeat([]byte("tls-hop-payload!"), 3*conduit.MaxDataFrame/16)
	go func() {
		_, _ = st.Write(payload)
		_ = st.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err := readAllWithin(t, st, "echoed payload over TLS")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo over TLS: %d bytes, err %v; want %d bytes", len(got), err, len(payload))
	}
	_ = st.Close()
}

// countingEcho is echoConfig that counts the RPCs and streams the target
// received.
func countingEcho(rpcs, streams *atomic.Int32) conduit.Config {
	c := echoConfig()
	rh, sh := c.RPCHandler, c.StreamHandler
	c.RPCHandler = conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		rpcs.Add(1)
		return rh.HandleRPC(ctx, req)
	})
	c.StreamHandler = conduit.StreamHandlerFunc(func(ctx context.Context, open *conduitv1.StreamOpen, ps conduit.PendingStream) error {
		streams.Add(1)
		return sh.HandleStreamOpen(ctx, open, ps)
	})
	return c
}

// TestHopTLSRefusesUnverifiedPeer: a hop client refuses a relay whose
// certificate it cannot verify, before anything reaches the target:
// a certificate from another CA, and a certificate that does not name
// the configured server name.
func TestHopTLSRefusesUnverifiedPeer(t *testing.T) {
	var rpcs, streams atomic.Int32
	p, files := newTLSPair(t, countingEcho(&rpcs, &streams))
	other := relaytest.NewHopTLSFiles(t, hopServerName)
	for _, tc := range []struct {
		name, caFile, serverName string
		want                     func(error) bool
	}{
		{name: "certificate from another CA", caFile: other.CAFile, serverName: hopServerName, want: func(err error) bool {
			var ua x509.UnknownAuthorityError
			return errors.As(err, &ua)
		}},
		{name: "server name mismatch", caFile: files.CAFile, serverName: "other-relay.internal", want: func(err error) bool {
			var he x509.HostnameError
			return errors.As(err, &he)
		}},
		{name: "system roots for a private CA", caFile: "", serverName: hopServerName, want: func(err error) bool {
			var ve *tls.CertificateVerificationError
			return errors.As(err, &ve)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, err := relay.NewHopClientTLS(tc.caFile, tc.serverName)
			if err != nil {
				t.Fatal(err)
			}
			pc := &relay.PeerClient{HTTP: relay.NewHopHTTPClient(cli), WS: relay.NewHopWSDialer(cli), Auth: p.w.PeerAuth("relay-b")}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err = pc.Probe(ctx, p.a.Internal.URL, "relay-a", p.a.Relay.Generation())
			if err == nil || !tc.want(err) {
				t.Fatalf("Probe = %v, want a certificate verification failure", err)
			}
			rs := relay.NewRemoteSession(pc, p.a.Internal.URL, p.rec, p.want)
			if _, err := rs.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "exec"}); err == nil {
				t.Fatal("Call to an unverified relay succeeded")
			}
			if _, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY}); err == nil {
				t.Fatal("OpenStream to an unverified relay succeeded")
			}
		})
	}
	if r, s := rpcs.Load(), streams.Load(); r != 0 || s != 0 {
		t.Fatalf("the target received %d RPCs and %d streams through unverified hops; want none", r, s)
	}
}

// TestHopRelayRefusesPlaintextNonLoopbackEndpoint: a relay never
// registers a plaintext endpoint on a non-loopback host, which other
// relays and the hub ingress would dial as registered.
func TestHopRelayRefusesPlaintextNonLoopbackEndpoint(t *testing.T) {
	w := relaytest.NewWorld(t)
	for _, ep := range []string{"http://192.0.2.10:9810", "http://relay-a.internal:9810"} {
		_, err := w.NewNode("relay-x", func(c *relay.Config) { c.InternalEndpoint = ep })
		if !errors.Is(err, config.ErrPlaintextNonLoopback) {
			t.Fatalf("relay.New with InternalEndpoint %s = %v, want ErrPlaintextNonLoopback", ep, err)
		}
	}
	// Loopback plaintext stays allowed (every relaytest node uses it).
	if _, err := w.NewNode("relay-y", nil); err != nil {
		t.Fatalf("relay.New with a loopback http endpoint: %v", err)
	}
}

// TestHopServerTLSReloadsRewrittenCertificate: the internal listener picks up
// a certificate pair rewritten in place at the next check after
// HopCertReloadInterval, and keeps serving the current certificate when the
// rewritten pair does not load. It runs on every platform; the Kubernetes
// Secret-volume symlink swap is TestHopServerTLSReloadsRotatedCertificate.
func TestHopServerTLSReloadsRewrittenCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	pair := func() (certPEM, keyPEM []byte) {
		t.Helper()
		f := relaytest.NewHopTLSFiles(t, hopServerName)
		certPEM, err := os.ReadFile(f.CertFile)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM, err = os.ReadFile(f.KeyFile)
		if err != nil {
			t.Fatal(err)
		}
		return certPEM, keyPEM
	}
	write := func(certPEM, keyPEM []byte) {
		t.Helper()
		if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(pair())
	now := time.Now()
	clock := func() time.Time { return now }
	cfg, err := relay.NewHopServerTLSWithClockForTest(certPath, keyPath, clock)
	if err != nil {
		t.Fatal(err)
	}
	leaf := func() []byte {
		t.Helper()
		c, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}
		return c.Certificate[0]
	}
	first := leaf()

	write(pair())
	if !bytes.Equal(leaf(), first) {
		t.Fatal("the certificate changed before the reload interval")
	}
	now = now.Add(relay.HopCertReloadInterval)
	second := leaf()
	if bytes.Equal(second, first) {
		t.Fatal("the rewritten certificate was not picked up after the reload interval")
	}

	// A rewrite that does not load keeps the current certificate.
	write([]byte("not pem"), []byte("not pem"))
	now = now.Add(relay.HopCertReloadInterval)
	if !bytes.Equal(leaf(), second) {
		t.Fatal("a pair that does not load replaced the current certificate")
	}
}

// TestHopUpperCaseSchemeEndpoint: an endpoint whose scheme is upper case
// (accepted by the hop rule, as net/url lower-cases it) still opens stream
// hops, over wss for HTTPS:// and ws for HTTP://.
func TestHopUpperCaseSchemeEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	roundTrip := func(t *testing.T, rs *relay.RemoteSession) {
		t.Helper()
		st, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
		if err != nil {
			t.Fatalf("OpenStream: %v", err)
		}
		go func() {
			_, _ = st.Write([]byte("upper"))
			_ = st.(interface{ CloseWrite() error }).CloseWrite()
		}()
		got, err := readAllWithin(t, st, "echo")
		if err != nil || string(got) != "upper" {
			t.Fatalf("echo = %q, %v", got, err)
		}
		_ = st.Close()
	}
	t.Run("HTTPS", func(t *testing.T) {
		p, _ := newTLSPair(t, echoConfig())
		ep := "HTTPS://" + strings.TrimPrefix(p.a.Internal.URL, "https://")
		roundTrip(t, relay.NewRemoteSession(p.b.Peers, ep, p.rec, p.want))
	})
	t.Run("HTTP", func(t *testing.T) {
		p := newPair(t, echoConfig())
		ep := "HTTP://" + strings.TrimPrefix(p.a.Internal.URL, "http://")
		roundTrip(t, relay.NewRemoteSession(p.b.Peers, ep, p.rec, p.want))
	})
}

// TestHopRelayInjectedClientFollowsNoRedirect: a client given to relay.New
// (Config.HTTPClient) is used without redirects for its calls to other
// relays, and the caller's client is not modified.
func TestHopRelayInjectedClientFollowsNoRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	var peerHits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHits.Add(1)
		http.Redirect(w, r, target.URL+"/x", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(peer.Close)
	injected := &http.Client{}
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", func(c *relay.Config) { c.HTTPClient = injected; c.RequireSelfCheck = false })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := n.Relay.PeerClient().Probe(ctx, peer.URL, "relay-x", 1); err == nil {
		t.Fatal("Probe through a redirect succeeded")
	}
	// The peer must have answered the probe, or the test proves nothing.
	if got := peerHits.Load(); got != 1 {
		t.Fatalf("the redirecting peer received %d requests, want 1", got)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("the redirect target received %d requests through relay.New's client; want none", got)
	}
	if injected.CheckRedirect != nil {
		t.Fatal("the caller's client was modified")
	}
}
