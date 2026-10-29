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

package hub

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Conduit relay internal hop transport policy at the hub ingress: the hub
// never proxies a client's conduit dial (its credentials and its whole
// session) to a relay over plaintext to a non-loopback host, and with TLS
// it verifies the relay against the operator CA and server name.

// recordingDialer records every address the ingress transport dials and
// dials loopback addresses for real.
type recordingDialer struct {
	mu    sync.Mutex
	addrs []string
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func (d *recordingDialer) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// registerPlaintextRelay registers a relay row whose internal endpoint is
// plaintext to a non-loopback host, as a misconfigured relay, a stale row
// or a forged row would carry.
func (f *ingressFixture) registerPlaintextRelay(t *testing.T, id, endpoint string) {
	t.Helper()
	now := time.Now()
	_, err := f.st.RegisterRelay(context.Background(), registry.RelayInstance{InstanceID: id, InternalEndpoint: endpoint, StartedAt: now, LastSeen: now})
	require.NoError(t, err)
}

// TestConduitIngress_RefusesPlaintextNonLoopbackRelay: a relay whose
// registered endpoint is plaintext to a non-loopback host is never dialed:
// nothing is signed or sent to it and nothing is written to the client for
// it; another relay is picked, or, with none left, the client gets 503
// conduit_not_serving.
func TestConduitIngress_RefusesPlaintextNonLoopbackRelay(t *testing.T) {
	for _, ep := range []string{"http://192.0.2.10:9810", "http://relay-x.internal:9810"} {
		t.Run(ep, func(t *testing.T) {
			t.Run("another relay is picked", func(t *testing.T) {
				f := newIngressFixture(t, "relay-a")
				f.registerPlaintextRelay(t, "relay-x", ep)
				f.preferRelays("relay-x", "relay-a")
				var d recordingDialer
				f.rt.ingressTransport = newConduitIngressTransport(&http.Transport{DialContext: d.dial}, conduitIngressResponseHeaderTimeout)
				rec := f.dial(t, nil)
				require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
				assert.Equal(t, "reached relay-a", rec.Body.String())
				for _, a := range d.dialed() {
					assert.NotContains(t, []string{"192.0.2.10:9810", "relay-x.internal:9810"}, a, "the plaintext relay was dialed")
				}
			})
			t.Run("no relay left", func(t *testing.T) {
				f := newIngressFixture(t)
				f.registerPlaintextRelay(t, "relay-x", ep)
				var d recordingDialer
				f.rt.ingressTransport = newConduitIngressTransport(&http.Transport{DialContext: d.dial}, conduitIngressResponseHeaderTimeout)
				rec := f.dial(t, nil)
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), ptyReasonConduitNotServing)
				assert.Empty(t, d.dialed(), "nothing was dialed")
			})
		})
	}
}

// serveRelayTLS serves relay id behind a TLS listener with the operator
// certificate and re-registers its endpoint as https.
func (f *ingressFixture) serveRelayTLS(t *testing.T, id string, srvTLS *tls.Config) {
	t.Helper()
	fr := f.relays[id]
	h := fr.srv.Config.Handler
	fr.srv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(tls.NewListener(ln, srvTLS)) }()
	t.Cleanup(func() { _ = srv.Close() })
	now := time.Now()
	_, err = f.st.RegisterRelay(context.Background(), registry.RelayInstance{InstanceID: id, InternalEndpoint: "https://" + ln.Addr().String(), StartedAt: now, LastSeen: now})
	require.NoError(t, err)
}

// TestConduitIngress_TLSRelay: with TLS on the internal hop the ingress
// proxies the dial to the relay over TLS, verified against the operator CA
// and server name (the hop client TLS the hub is started with); a relay
// whose certificate does not verify receives nothing.
func TestConduitIngress_TLSRelay(t *testing.T) {
	const name = "scion-relay.internal"
	files := relaytest.NewHopTLSFiles(t, name)
	srvTLS, err := relay.NewHopServerTLS(files.CertFile, files.KeyFile, nil)
	require.NoError(t, err)

	t.Run("verified relay", func(t *testing.T) {
		cli, err := relay.NewHopClientTLS(files.CAFile, name)
		require.NoError(t, err)
		f := newIngressFixture(t, "relay-a")
		f.serveRelayTLS(t, "relay-a", srvTLS)
		f.rt.ingressTransport = newConduitIngressTransport(relay.NewHopTransport(cli), conduitIngressResponseHeaderTimeout)
		rec := f.dial(t, nil)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		assert.Equal(t, "reached relay-a", rec.Body.String())
		got, peer := f.relays["relay-a"].last(t)
		assert.Equal(t, "hub-a", peer, "the leg is signed as relay-peer by the hub")
		assert.Equal(t, "Bearer client-credential", got.Header.Get("Authorization"))
	})
	for _, tc := range []struct{ name, caFile, serverName string }{
		{name: "certificate from another CA", caFile: relaytest.NewHopTLSFiles(t, name).CAFile, serverName: name},
		{name: "server name mismatch", caFile: files.CAFile, serverName: "other-relay.internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, err := relay.NewHopClientTLS(tc.caFile, tc.serverName)
			require.NoError(t, err)
			f := newIngressFixture(t, "relay-a")
			f.serveRelayTLS(t, "relay-a", srvTLS)
			f.rt.ingressTransport = newConduitIngressTransport(relay.NewHopTransport(cli), conduitIngressResponseHeaderTimeout)
			rec := f.dial(t, nil)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			assert.Zero(t, f.relays["relay-a"].hits.Load(), "an unverified relay receives nothing")
		})
	}
}

// TestStartConduitExternal_HopTLSWiring: a hub started in external relay
// mode with HopTLS verifies relays with it on every hop client: the ingress
// proxy transport and the relay-peer HTTP client and stream dialer carry
// the operator CA and server name, and the HTTP client follows no
// redirect.
func TestStartConduitExternal_HopTLSWiring(t *testing.T) {
	const name = "scion-relay.internal"
	files := relaytest.NewHopTLSFiles(t, name)
	cli, err := relay.NewHopClientTLS(files.CAFile, name)
	require.NoError(t, err)
	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	opts := externalOptions(t, f)
	opts.HopTLS = cli
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.NoError(t, f.srv.StartConduitExternal(ctx, opts))
	rt := f.srv.conduitExternal.Load()
	require.NotNil(t, rt)

	tr, ok := rt.ingressTransport.(*http.Transport)
	require.True(t, ok, "ingress transport %T", rt.ingressTransport)
	require.NotNil(t, tr.TLSClientConfig, "the ingress transport verifies relays with the hop TLS")
	assert.Equal(t, name, tr.TLSClientConfig.ServerName)
	assert.True(t, tr.TLSClientConfig.RootCAs.Equal(cli.RootCAs), "the ingress transport trusts the operator CA")
	assert.False(t, tr.TLSClientConfig.InsecureSkipVerify)

	require.NotNil(t, rt.peers.WS, "the relay-peer stream dialer carries the hop TLS")
	assert.Equal(t, name, rt.peers.WS.TLSClientConfig.ServerName)
	require.NotNil(t, rt.peers.HTTP)
	ptr, ok := rt.peers.HTTP.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, name, ptr.TLSClientConfig.ServerName)
	require.NotNil(t, rt.peers.HTTP.CheckRedirect)
	assert.ErrorIs(t, rt.peers.HTTP.CheckRedirect(nil, nil), http.ErrUseLastResponse, "hop clients follow no redirect")
}
