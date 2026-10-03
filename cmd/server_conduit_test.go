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
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetConduitFlags() {
	conduitInternalListen, conduitInternalAdvertise = "", ""
	conduitGrantKeyActivation, conduitReconnectWindow = "", ""
	conduitTCPAllowedPorts = nil
}

// conduitFlagCommand returns a command with the conduit flags, parsed
// from args.
func conduitFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	resetConduitFlags()
	t.Cleanup(resetConduitFlags)
	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	registerConduitServerFlags(c.Flags())
	c.SetArgs(args)
	require.NoError(t, c.Execute())
	return c
}

func TestConduitAdvertiseEndpoint(t *testing.T) {
	tcp := func(s string) net.Addr {
		a, err := net.ResolveTCPAddr("tcp", s)
		require.NoError(t, err)
		return a
	}
	tests := []struct {
		name      string
		advertise string
		bound     net.Addr
		podIP     string
		want      string
	}{
		{name: "configured wins", advertise: "https://relay.internal:9443", bound: tcp("10.0.0.5:9810"), podIP: "10.1.1.1", want: "https://relay.internal:9443"},
		{name: "specific listen host", bound: tcp("10.0.0.5:9810"), podIP: "10.1.1.1", want: "http://10.0.0.5:9810"},
		{name: "wildcard uses pod IP", bound: tcp("0.0.0.0:9810"), podIP: "10.1.1.1", want: "http://10.1.1.1:9810"},
		{name: "IPv6 wildcard uses pod IP", bound: tcp("[::]:9810"), podIP: "fd00::5", want: "http://[fd00::5]:9810"},
		{name: "wildcard without pod IP is unaddressable", bound: tcp("0.0.0.0:9810")},
		{name: "no listener", podIP: "10.1.1.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, conduitAdvertiseEndpoint(tt.advertise, tt.bound, tt.podIP))
		})
	}
}

func TestApplyConduitFlagOverrides(t *testing.T) {
	configured := config.HubConduitConfig{
		InternalListen: ":9000", InternalAdvertise: "http://a:9000", GrantKeyActivation: "20m",
		ReconnectWindow: "3s", TCPAllowedPorts: []int{22}, PeerAuth: "hmac",
	}
	tests := []struct {
		name string
		args []string
		want config.HubConduitConfig
	}{
		{name: "no flags keep the config", want: configured},
		{
			name: "every flag overrides",
			args: []string{"--internal-listen=:9810", "--internal-advertise=http://b:9810", "--conduit-grant-key-activation=30m",
				"--conduit-reconnect-window=0s", "--conduit-tcp-allowed-ports=3000,8080"},
			want: config.HubConduitConfig{
				InternalListen: ":9810", InternalAdvertise: "http://b:9810", GrantKeyActivation: "30m",
				ReconnectWindow: "0s", TCPAllowedPorts: []int{3000, 8080}, PeerAuth: "hmac",
			},
		},
		{
			name: "an explicit empty value clears the setting",
			args: []string{"--internal-advertise="},
			want: func() config.HubConduitConfig { c := configured; c.InternalAdvertise = ""; return c }(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := conduitFlagCommand(t, tt.args...)
			cfg := &config.GlobalConfig{}
			cfg.Hub.Conduit = configured
			cfg.Hub.Conduit.TCPAllowedPorts = append([]int(nil), configured.TCPAllowedPorts...)
			applyConduitFlagOverrides(c, cfg)
			assert.Equal(t, tt.want, cfg.Hub.Conduit)
		})
	}
}

func TestAppendConduitDaemonArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "nothing set", want: []string{"server", "start"}},
		{
			name: "explicit flags forwarded",
			args: []string{"--internal-listen=:9810", "--conduit-reconnect-window=2s", "--conduit-tcp-allowed-ports=22", "--conduit-tcp-allowed-ports=3000"},
			want: []string{"server", "start", "--internal-listen=:9810", "--conduit-reconnect-window=2s", "--conduit-tcp-allowed-ports=22,3000"},
		},
		{
			name: "every flag",
			args: []string{"--internal-advertise=http://b:9810", "--conduit-grant-key-activation=30m"},
			want: []string{"server", "start", "--internal-advertise=http://b:9810", "--conduit-grant-key-activation=30m"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := conduitFlagCommand(t, tt.args...)
			assert.Equal(t, tt.want, appendConduitDaemonArgs(c, []string{"server", "start"}))
		})
	}
}

// TestConduitDaemonArgsRoundTrip: what the daemon parent forwards, the
// --foreground child parses back to the same settings.
func TestConduitDaemonArgsRoundTrip(t *testing.T) {
	parent := conduitFlagCommand(t, "--internal-listen=:9810", "--conduit-grant-key-activation=30m", "--conduit-tcp-allowed-ports=22,3000")
	parentCfg := &config.GlobalConfig{}
	applyConduitFlagOverrides(parent, parentCfg)

	child := conduitFlagCommand(t, appendConduitDaemonArgs(parent, nil)...)
	childCfg := &config.GlobalConfig{}
	applyConduitFlagOverrides(child, childCfg)
	assert.Equal(t, parentCfg.Hub.Conduit, childCfg.Hub.Conduit)
}

func TestValidateServerPreflight_Conduit(t *testing.T) {
	t.Cleanup(resetServerFlags)
	tests := []struct {
		name    string
		hub     bool
		conduit config.HubConduitConfig
		wantErr string
	}{
		{name: "valid", hub: true, conduit: config.HubConduitConfig{ReconnectWindow: "5s", GrantKeyActivation: "15m"}},
		{name: "bad reconnect window", hub: true, conduit: config.HubConduitConfig{ReconnectWindow: "1h"}, wantErr: "server.hub.conduit.reconnect_window"},
		{name: "bad activation", hub: true, conduit: config.HubConduitConfig{GrantKeyActivation: "10s"}, wantErr: "server.hub.conduit.grant_key_activation"},
		{name: "bad port", hub: true, conduit: config.HubConduitConfig{TCPAllowedPorts: []int{0}}, wantErr: "server.hub.conduit.tcp_allowed_ports"},
		{name: "broker-only process skips the check", conduit: config.HubConduitConfig{ReconnectWindow: "1h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetServerFlags()
			enableHub = tt.hub
			cfg := &config.GlobalConfig{}
			cfg.Hub.Conduit = tt.conduit
			err := validateServerPreflight(cfg)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// conduitHubServer returns a hub server with hub.conduit set to enabled
// and no at-rest encryption key.
func conduitHubServer(t *testing.T, enabled bool) *hub.Server {
	t.Helper()
	return conduitHubServerWith(t, enabled, hub.ServerConfig{})
}

// conduitHubServerWith is conduitHubServer with a server config.
func conduitHubServerWith(t *testing.T, enabled bool, sc hub.ServerConfig) *hub.Server {
	t.Helper()
	ctx := context.Background()
	st := newTestStore(t)
	srv, err := hub.New(sc, st)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	value, err := json.Marshal(map[string]any{"overrides": map[string]bool{"hub.conduit": enabled}})
	require.NoError(t, err)
	_, err = st.UpsertHubSetting(ctx, "experiments", value, "test", -1, "seeded")
	require.NoError(t, err)
	ops := hub.NewOperationalSettings(st, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.Equal(t, enabled, srv.ConduitEnabled())
	return srv
}

// TestStartConduit covers the C8 startup policy: with hub.conduit off
// nothing starts; in hosted HA a grant key ring without the shared at-rest
// key is a startup error (before any listener or peer auth); outside HA a
// failure is logged and the hub serves without a relay.
func TestStartConduit(t *testing.T) {
	t.Cleanup(resetServerFlags)
	run := func(t *testing.T, srv *hub.Server, cfg *config.GlobalConfig) error {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		t.Cleanup(func() { cancel(); wg.Wait() })
		return startConduit(ctx, cfg, srv, &wg, make(chan error, 1))
	}

	t.Run("nil server", func(t *testing.T) {
		assert.NoError(t, run(t, nil, &config.GlobalConfig{}))
	})

	t.Run("flag off is a no-op even in hosted HA", func(t *testing.T) {
		resetServerFlags()
		hostedMode, enableHub = true, true
		t.Setenv("K_SERVICE", "scion-hub")
		assert.NoError(t, run(t, conduitHubServer(t, false), &config.GlobalConfig{}))
	})

	t.Run("hosted HA without the at-rest key fails", func(t *testing.T) {
		resetServerFlags()
		hostedMode, enableHub = true, true
		t.Setenv("K_SERVICE", "scion-hub")
		srv := conduitHubServer(t, true)
		require.False(t, srv.ConduitGrantRingShared())
		cfg := &config.GlobalConfig{}
		// An unusable listen address shows the ring check runs first.
		cfg.Hub.Conduit.InternalListen = "256.0.0.1:1"
		err := run(t, srv, cfg)
		require.Error(t, err)
		assert.True(t, errors.Is(err, hub.ErrConduitNoAtRestKey), "got %v", err)
		assert.True(t, strings.Contains(err.Error(), "hosted HA"), "got %v", err)
	})

	t.Run("hosted HA without the shared secret for signing fails", func(t *testing.T) {
		resetServerFlags()
		hostedMode, enableHub = true, true
		t.Setenv("K_SERVICE", "scion-hub")
		t.Setenv("SCION_SERVER_SESSION_SECRET", "")
		t.Setenv("SESSION_SECRET", "")
		// The ring is shared, so the ring check passes and the peer-auth
		// requirement is what fails.
		srv := conduitHubServerWith(t, true, hub.ServerConfig{SharedSigningSecret: "conduit-test-signing-secret-0123456789"})
		require.True(t, srv.ConduitGrantRingShared())
		for _, mode := range []string{config.ConduitPeerAuthOIDC, config.ConduitPeerAuthHMAC} {
			cfg := &config.GlobalConfig{}
			cfg.Hub.Conduit.PeerAuth = mode
			cfg.Hub.Conduit.PeerServiceAccounts = []string{"hub@p.iam.gserviceaccount.com"}
			err := run(t, srv, cfg)
			require.Error(t, err, mode)
			assert.Contains(t, err.Error(), "no shared signing secret", mode)
			assert.Contains(t, err.Error(), "hosted HA", mode)
		}
	})

	t.Run("outside HA a failure is logged, not fatal", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		t.Setenv("SCION_SERVER_SESSION_SECRET", "conduit-test-signing-secret-0123456789")
		cfg := &config.GlobalConfig{}
		cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
		cfg.Hub.Conduit.InternalAdvertise = "http://10.0.0.5:9810" // without internal_listen
		srv := conduitHubServer(t, true)
		assert.NoError(t, run(t, srv, cfg))
		assert.Nil(t, srv.ConduitRelayFatal(), "no relay is running")
	})
}

// TestNewCommandBus_ConduitHA (C8): a Postgres command bus that cannot
// start is a startup error in hosted HA with hub.conduit on; otherwise the
// hub falls back to the no-op bus.
func TestNewCommandBus_ConduitHA(t *testing.T) {
	t.Cleanup(resetServerFlags)
	orig := startPostgresCommandBus
	t.Cleanup(func() { startPostgresCommandBus = orig })

	tests := []struct {
		name    string
		hosted  bool
		conduit bool
		busErr  error
		wantErr bool
		wantBus hub.CommandBus
	}{
		{name: "hosted HA, conduit on, bus fails", hosted: true, conduit: true, busErr: errors.New("listen refused"), wantErr: true},
		{name: "hosted HA, conduit off, bus fails", hosted: true, busErr: errors.New("listen refused"), wantBus: hub.NoopCommandBus{}},
		{name: "not hosted, conduit on, bus fails", conduit: true, busErr: errors.New("listen refused"), wantBus: hub.NoopCommandBus{}},
		{name: "hosted HA, conduit on, bus starts", hosted: true, conduit: true, wantBus: fakeCommandBus{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetServerFlags()
			hostedMode, enableHub = tt.hosted, true
			startPostgresCommandBus = func(context.Context, string, func(string) bool, func(context.Context, string), *slog.Logger) (hub.CommandBus, error) {
				if tt.busErr != nil {
					return nil, tt.busErr
				}
				return fakeCommandBus{}, nil
			}
			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = "postgres"
			bus, err := newCommandBus(context.Background(), cfg, conduitHubServer(t, tt.conduit))
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.busErr)
				assert.Contains(t, err.Error(), "hosted HA")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantBus, bus)
		})
	}
}

// fakeCommandBus stands in for a started Postgres bus.
type fakeCommandBus struct{ hub.NoopCommandBus }
