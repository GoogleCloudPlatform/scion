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

package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

const skipTestHubEndpoint = "http://localhost:9810"

// captureConduitLog sends slog output to a buffer that is safe for the
// relay's background goroutines, for the rest of the test.
func captureConduitLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// countInternalListeners counts the internal listeners opened for the rest
// of the test; the real listener still opens.
func countInternalListeners(t *testing.T) *int {
	t.Helper()
	n := new(int)
	prev := serveConduitInternalListenerFn
	serveConduitInternalListenerFn = func(ctx context.Context, listen string, tlsConfig *tls.Config, h http.Handler, wg *sync.WaitGroup, errCh chan<- error) (net.Addr, func(), error) {
		*n++
		return prev(ctx, listen, tlsConfig, h, wg, errCh)
	}
	t.Cleanup(func() { serveConduitInternalListenerFn = prev })
	return n
}

// recordStartFailures collects the keys startConduit records on the hub
// for the rest of the test (the real record still runs).
func recordStartFailures(t *testing.T) *[]string {
	t.Helper()
	got := &[]string{}
	prev := conduitRecordStartFailure
	conduitRecordStartFailure = func(hubSrv *hub.Server, setting string) {
		*got = append(*got, setting)
		prev(hubSrv, setting)
	}
	t.Cleanup(func() { conduitRecordStartFailure = prev })
	return got
}

// stubHubRelayStart replaces the hub's in-process relay start for the rest
// of the test: it records the options it is given and starts nothing (the
// cmd test hub has no conduit registry database).
func stubHubRelayStart(t *testing.T) *[]hub.ConduitRelayOptions {
	t.Helper()
	got := &[]hub.ConduitRelayOptions{}
	prev := conduitStartHubRelay
	conduitStartHubRelay = func(_ context.Context, _ *hub.Server, opts hub.ConduitRelayOptions) error {
		*got = append(*got, opts)
		return nil
	}
	t.Cleanup(func() { conduitStartHubRelay = prev })
	return got
}

// startRelayForSkipTest runs startConduitRelay on a fresh hub with
// hub.conduit on and no shared signing secret.
func startRelayForSkipTest(t *testing.T, cfg *config.GlobalConfig, requireHA bool) (*hub.Server, error) {
	t.Helper()
	srv := conduitHubServer(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	return srv, startConduitRelay(ctx, cfg, srv, skipTestHubEndpoint, &wg, make(chan error, 1), requireHA)
}

// noPeerCredentialCase is a hub config that today cannot build relay-peer
// auth on a host with no service account: sessionSecret is the session
// secret set for the case ("" = none).
type noPeerCredentialCase struct {
	peerAuth      string
	sessionSecret string
	wantKey       string // the setting a refusal names
}

func noPeerCredentialCases() map[string]noPeerCredentialCase {
	return map[string]noPeerCredentialCase{
		// The session secret is set, so the oidc allow-list is what fails.
		"oidc with no service account": {
			peerAuth: config.ConduitPeerAuthOIDC, sessionSecret: "conduit-test-signing-secret-0123456789",
			wantKey: "server.hub.conduit.peer_service_accounts",
		},
		"hmac with no session secret": {
			peerAuth: config.ConduitPeerAuthHMAC,
			wantKey:  "--session-secret (SCION_SERVER_SESSION_SECRET)",
		},
	}
}

// config returns the case's hub config and sets its session secret.
func (c noPeerCredentialCase) config(t *testing.T) *config.GlobalConfig {
	t.Helper()
	t.Setenv("SCION_SERVER_SESSION_SECRET", c.sessionSecret)
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit.PeerAuth = c.peerAuth
	return cfg
}

func resetForSkipTest(t *testing.T) {
	t.Helper()
	resetServerFlags()
	enableHub = true
	oldCaptured := capturedSessionSecret
	capturedSessionSecret = ""
	t.Cleanup(func() { capturedSessionSecret = oldCaptured })
	// Not on GCE, whatever the test host: the node's own service account
	// must not fill the oidc allow-list.
	oldOnGCE := conduitOnGCE
	conduitOnGCE = func() bool { return false }
	t.Cleanup(func() { conduitOnGCE = oldOnGCE })
	t.Setenv("K_SERVICE", "")
	t.Setenv("SCION_SERVER_SESSION_SECRET", "")
	t.Setenv("SESSION_SECRET", "")
}

// TestStartConduitRelay_NoInternalListenSkipsPeerAuth: a workstation or
// single-node hub (not hosted HA, no internal listener) starts its relay
// without relay-peer credentials: the relay is started with relay.NoPeerAuth,
// no internal listener opens, the log says the internal relay API is not
// served, and the relay runs with peer_auth=none. (A running relay with
// NoPeerAuth is covered in pkg/hub by TestSkipPath_DialerAdmittedWithoutSecret.)
func TestStartConduitRelay_NoInternalListenSkipsPeerAuth(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for name, tc := range noPeerCredentialCases() {
		t.Run(name, func(t *testing.T) {
			resetForSkipTest(t)
			logs := captureConduitLog(t)
			listeners := countInternalListeners(t)
			starts := stubHubRelayStart(t)
			_, err := startRelayForSkipTest(t, tc.config(t), false)
			require.NoError(t, err)
			require.Len(t, *starts, 1, "the relay is started")
			assert.IsType(t, relay.NoPeerAuth{}, (*starts)[0].PeerAuth)
			assert.Empty(t, (*starts)[0].InternalEndpoint, "no internal endpoint")
			assert.Zero(t, *listeners, "no internal listener, so the internal relay API is not served")
			out := logs.String()
			assert.Contains(t, out, "internal relay API not served on this node")
			assert.Contains(t, out, "peer_auth=none")
			assert.NotContains(t, out, "peer auth disabled")
		})
	}
}

// TestStartConduitRelay_InternalListenStillNeedsPeerAuth: with an internal
// listener the same configs still refuse to start, naming the setting, and
// no listener is opened.
func TestStartConduitRelay_InternalListenStillNeedsPeerAuth(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for name, tc := range noPeerCredentialCases() {
		t.Run(name, func(t *testing.T) {
			resetForSkipTest(t)
			listeners := countInternalListeners(t)
			starts := stubHubRelayStart(t)
			cfg := tc.config(t)
			cfg.Hub.Conduit.InternalListen = "127.0.0.1:0"
			_, err := startRelayForSkipTest(t, cfg, false)
			require.Error(t, err)
			var se *relay.SettingError
			require.True(t, errors.As(err, &se), "%v", err)
			assert.Equal(t, tc.wantKey, se.Key)
			assert.Empty(t, *starts, "refused before any relay start")
			assert.Zero(t, *listeners, "peer auth is checked before the listener opens")
		})
	}
}

// TestStartConduitRelay_SkipGuards: the advertise-without-listen refusal
// runs before the skip, and hosted HA never takes the skip.
func TestStartConduitRelay_SkipGuards(t *testing.T) {
	t.Cleanup(resetServerFlags)
	t.Run("advertise without listen is refused before the skip", func(t *testing.T) {
		resetForSkipTest(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthOIDC
		cfg.Hub.Conduit.InternalAdvertise = "http://10.0.0.5:9810"
		srv, err := startRelayForSkipTest(t, cfg, false)
		require.Error(t, err)
		var se *relay.SettingError
		require.True(t, errors.As(err, &se), "%v", err)
		assert.Equal(t, "server.hub.conduit.internal_advertise", se.Key)
		assert.Nil(t, srv.ConduitRelayFatal(), "no relay is running")
	})
	t.Run("hosted HA with no listener still builds relay-peer auth", func(t *testing.T) {
		resetForSkipTest(t)
		hostedMode = true
		// The ring is shared, so the ring check passes and relay-peer auth
		// is what fails, as before.
		srv := conduitHubServerWith(t, true, hub.ServerConfig{SharedSigningSecret: "conduit-test-signing-secret-0123456789"})
		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "postgres"
		cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		t.Cleanup(func() { cancel(); wg.Wait() })
		err := startConduitRelay(ctx, cfg, srv, skipTestHubEndpoint, &wg, make(chan error, 1), true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no shared signing secret")
	})
}

// TestConduitInternalRelayAPIServed pins the skip predicate: only a node
// outside hosted HA with no internal listener skips relay-peer auth.
func TestConduitInternalRelayAPIServed(t *testing.T) {
	cfg := func(listen string) *config.GlobalConfig {
		c := &config.GlobalConfig{}
		c.Hub.Conduit.InternalListen = listen
		return c
	}
	assert.False(t, conduitInternalRelayAPIServed(cfg(""), false))
	assert.True(t, conduitInternalRelayAPIServed(cfg("127.0.0.1:9811"), false))
	assert.True(t, conduitInternalRelayAPIServed(cfg(""), true))
	assert.True(t, conduitInternalRelayAPIServed(cfg("127.0.0.1:9811"), true))
}

// TestStartConduit_FailureLogNamesTheKey: outside hosted HA a relay that
// fails on one setting logs that setting's key and records it for the
// relay-not-running response; the log carries no configured value.
func TestStartConduit_FailureLogNamesTheKey(t *testing.T) {
	t.Cleanup(resetServerFlags)
	resetForSkipTest(t)
	logs := captureConduitLog(t)
	recorded := recordStartFailures(t)
	const account = "relay-peer@p.iam.gserviceaccount.com"
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit.PeerAuth = "mtls" // an unknown mode
	cfg.Hub.Conduit.PeerServiceAccounts = []string{account}
	cfg.Hub.Conduit.InternalListen = "127.0.0.1:0"
	srv := conduitHubServer(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	require.NoError(t, startConduit(ctx, newTestShutdown(cancel), cfg, srv, skipTestHubEndpoint, &wg, make(chan error, 1)))
	assert.Nil(t, srv.ConduitRelayFatal(), "no relay is running")
	out := logs.String()
	assert.Contains(t, out, "setting=server.hub.conduit.peer_auth")
	assert.NotContains(t, out, "mtls", "the configured value is not logged")
	assert.NotContains(t, out, account)
	assert.Equal(t, []string{"server.hub.conduit.peer_auth"}, *recorded, "the key is recorded for the relay-not-running response")
}

// TestStartConduit_NonSettingFailureRecordsNoKey: a relay start that fails
// for a reason other than one setting (here the internal listener cannot
// open) records the failure with no key. The listen address fails to parse
// (port out of range), so the failure is local: no name lookup is made.
func TestStartConduit_NonSettingFailureRecordsNoKey(t *testing.T) {
	t.Cleanup(resetServerFlags)
	resetForSkipTest(t)
	t.Setenv("SCION_SERVER_SESSION_SECRET", "conduit-test-signing-secret-0123456789")
	logs := captureConduitLog(t)
	recorded := recordStartFailures(t)
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
	cfg.Hub.Conduit.InternalListen = "127.0.0.1:99999" // port out of range: a local parse error
	srv := conduitHubServer(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	require.NoError(t, startConduit(ctx, newTestShutdown(cancel), cfg, srv, skipTestHubEndpoint, &wg, make(chan error, 1)))
	assert.Nil(t, srv.ConduitRelayFatal(), "no relay is running")
	assert.Equal(t, []string{""}, *recorded, "a failure not caused by one setting records no key")
	out := logs.String()
	assert.Contains(t, out, "invalid port", "the listener failed on the local port parse")
	assert.NotContains(t, out, "lookup", "no name lookup was made")
}
