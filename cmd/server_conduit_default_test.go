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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conduitHubServerWithExperimentsRaw creates a hub.Server backed by an
// OperationalSettings instance whose "experiments" row is raw (empty = absent).
func conduitHubServerWithExperimentsRaw(t *testing.T, sc hub.ServerConfig, raw string) *hub.Server {
	t.Helper()
	ctx := context.Background()
	st := newTestStore(t)
	srv, err := hub.New(sc, st)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	if raw != "" {
		_, err = st.UpsertHubSetting(ctx, "experiments", json.RawMessage(raw), "test", -1, "seeded")
		require.NoError(t, err)
	}
	ops := hub.NewOperationalSettings(st, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	return srv
}

// captureConduitSlog redirects slog.Default() to a text handler backed by a
// buffer for the duration of the test and returns the buffer.
func captureConduitSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestConduitDeploymentDefault covers Phase 5 (ptone/scion#9491) stage 1:
//   - (a) non-HA with nothing set → on;
//   - (b) hosted HA with nothing set → off, reason logged, hub starts;
//   - (c) hosted HA with explicit on → A-i..A-iv checks remain fatal;
//   - (d) explicit off → off everywhere;
//   - (e) malformed overrides document → off.
func TestConduitDeploymentDefault(t *testing.T) {
	t.Cleanup(resetServerFlags)
	const hubEndpoint = "https://hub.example.com"
	const signingSecret = "conduit-test-signing-secret-0123456789"

	t.Run("a_non_HA_nothing_set_is_on", func(t *testing.T) {
		resetServerFlags()
		enableHub = true
		t.Setenv("K_SERVICE", "")
		logBuf := captureConduitSlog(t)

		prevStart := startConduitRelayFn
		t.Cleanup(func() { startConduitRelayFn = prevStart })
		var relayStarted bool
		var gotRequireHA bool
		startConduitRelayFn = func(_ context.Context, _ *config.GlobalConfig, _ *hub.Server, _ string, _ *sync.WaitGroup, _ chan<- error, requireHA bool) error {
			relayStarted = true
			gotRequireHA = requireHA
			return nil
		}

		cfg := &config.GlobalConfig{}
		srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{}, "")
		configureConduitDeploymentDefault(cfg, srv, hubEndpoint, false)

		assert.True(t, srv.ConduitEnabled(), "compiled default true applies outside hosted HA")
		assert.Empty(t, srv.ConduitDeploymentDefaultReason())
		assert.NotContains(t, logBuf.String(), "conduit_default_off_reason")

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		t.Cleanup(func() { cancel(); wg.Wait() })
		require.NoError(t, startConduit(ctx, newTestShutdown(cancel), cfg, srv, hubEndpoint, &wg, make(chan error, 1)))
		assert.True(t, relayStarted, "relay must start outside hosted HA when nothing is set")
		assert.False(t, gotRequireHA)
	})

	t.Run("b_hosted_HA_nothing_set_is_off_with_reason_logged", func(t *testing.T) {
		tests := []struct {
			name       string
			sc         hub.ServerConfig
			kService   string
			driver     string
			advertise  string
			busStarted bool
			wantReason string
		}{
			{
				name:       "no_at_rest_key",
				sc:         hub.ServerConfig{},
				driver:     "postgres",
				busStarted: true,
				wantReason: conduitDefaultOffNoAtRestKey,
			},
			{
				name:       "cloudrun_inprocess_relay",
				sc:         hub.ServerConfig{SharedSigningSecret: signingSecret},
				kService:   "scion-hub",
				driver:     "postgres",
				busStarted: true,
				wantReason: conduitDefaultOffCloudRunInProcessRelay,
			},
			{
				name:       "advertise_host",
				sc:         hub.ServerConfig{SharedSigningSecret: signingSecret},
				driver:     "postgres",
				advertise:  "https://hub.example.com:9810",
				busStarted: true,
				wantReason: conduitDefaultOffAdvertiseHost,
			},
			{
				name:       "command_bus",
				sc:         hub.ServerConfig{SharedSigningSecret: signingSecret},
				driver:     "postgres",
				busStarted: false,
				wantReason: conduitDefaultOffCommandBus,
			},
			{
				name:       "hosted_ha_staged",
				sc:         hub.ServerConfig{SharedSigningSecret: signingSecret},
				driver:     "postgres",
				busStarted: true,
				wantReason: conduitDefaultOffHostedHAStaged,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				resetServerFlags()
				hostedMode, enableHub = true, true
				t.Setenv("K_SERVICE", tt.kService)
				logBuf := captureConduitSlog(t)

				cfg := &config.GlobalConfig{}
				cfg.Database.Driver = tt.driver
				cfg.Hub.Conduit.InternalAdvertise = tt.advertise
				srv := conduitHubServerWithExperimentsRaw(t, tt.sc, "")

				configureConduitDeploymentDefault(cfg, srv, hubEndpoint, tt.busStarted)

				assert.False(t, srv.ConduitEnabled(), "hosted HA stage 1 must default hub.conduit off")
				assert.Equal(t, tt.wantReason, srv.ConduitDeploymentDefaultReason())
				assert.Contains(t, logBuf.String(), "level=WARN")
				assert.Contains(t, logBuf.String(), "conduit_default_off_reason="+tt.wantReason)

				ctx, cancel := context.WithCancel(context.Background())
				var wg sync.WaitGroup
				t.Cleanup(func() { cancel(); wg.Wait() })
				require.NoError(t, startConduit(ctx, newTestShutdown(cancel), cfg, srv, hubEndpoint, &wg, make(chan error, 1)),
					"hub must start cleanly with conduit off")
			})
		}
	})

	t.Run("b_hosted_HA_nothing_set_command_bus_failure_falls_back_to_noop", func(t *testing.T) {
		resetServerFlags()
		hostedMode, enableHub = true, true
		t.Setenv("K_SERVICE", "")
		logBuf := captureConduitSlog(t)
		captureStdLog(t)

		orig := startPostgresCommandBus
		t.Cleanup(func() { startPostgresCommandBus = orig })
		startPostgresCommandBus = func(context.Context, string, func(string) bool, func(context.Context, string), *slog.Logger) (hub.CommandBus, error) {
			return nil, errors.New("listen refused")
		}

		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "postgres"
		srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{SharedSigningSecret: signingSecret}, "")

		bus, err := newCommandBus(context.Background(), cfg, srv)
		require.NoError(t, err, "without explicit override, command bus failure must not be fatal")
		assert.Equal(t, hub.NoopCommandBus{}, bus)

		_, isNoop := bus.(hub.NoopCommandBus)
		configureConduitDeploymentDefault(cfg, srv, hubEndpoint, !isNoop)
		assert.False(t, srv.ConduitEnabled())
		assert.Contains(t, logBuf.String(), "conduit_default_off_reason="+conduitDefaultOffCommandBus)
	})

	t.Run("c_hosted_HA_explicit_on_keeps_A_i_to_A_iv_fatal", func(t *testing.T) {
		t.Run("A_i_no_at_rest_key", func(t *testing.T) {
			resetServerFlags()
			hostedMode, enableHub = true, true
			t.Setenv("K_SERVICE", "")
			logBuf := captureConduitSlog(t)

			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = "postgres"
			srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{}, `{"overrides":{"hub.conduit":true}}`)
			configureConduitDeploymentDefault(cfg, srv, hubEndpoint, true)

			assert.True(t, srv.ConduitEnabled(), "explicit override beats deployment default")
			assert.NotContains(t, logBuf.String(), "conduit_default_off_reason")

			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			t.Cleanup(func() { cancel(); wg.Wait() })
			err := startConduit(ctx, newTestShutdown(cancel), cfg, srv, hubEndpoint, &wg, make(chan error, 1))
			require.Error(t, err)
			assert.ErrorIs(t, err, hub.ErrConduitNoAtRestKey)
		})

		t.Run("A_ii_cloudrun_inprocess_relay", func(t *testing.T) {
			resetServerFlags()
			hostedMode, enableHub = true, true
			t.Setenv("K_SERVICE", "scion-hub")
			t.Setenv("SCION_SERVER_SESSION_SECRET", signingSecret)

			cfg := &config.GlobalConfig{}
			cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
			srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{SharedSigningSecret: signingSecret}, `{"overrides":{"hub.conduit":true}}`)
			configureConduitDeploymentDefault(cfg, srv, hubEndpoint, true)

			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			t.Cleanup(func() { cancel(); wg.Wait() })
			err := startConduit(ctx, newTestShutdown(cancel), cfg, srv, hubEndpoint, &wg, make(chan error, 1))
			require.Error(t, err)
			assert.ErrorIs(t, err, errConduitRelayOnCloudRun)
		})

		t.Run("A_iii_advertise_host", func(t *testing.T) {
			resetServerFlags()
			hostedMode, enableHub = true, true
			t.Setenv("K_SERVICE", "")
			t.Setenv("SCION_SERVER_SESSION_SECRET", signingSecret)

			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = "postgres"
			cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
			cfg.Hub.Conduit.InternalAdvertise = "https://hub.example.com:9810"
			srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{SharedSigningSecret: signingSecret}, `{"overrides":{"hub.conduit":true}}`)
			configureConduitDeploymentDefault(cfg, srv, hubEndpoint, true)

			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			t.Cleanup(func() { cancel(); wg.Wait() })
			err := startConduit(ctx, newTestShutdown(cancel), cfg, srv, hubEndpoint, &wg, make(chan error, 1))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "internal_advertise")
		})

		t.Run("A_iv_command_bus", func(t *testing.T) {
			resetServerFlags()
			hostedMode, enableHub = true, true
			t.Setenv("K_SERVICE", "")

			orig := startPostgresCommandBus
			t.Cleanup(func() { startPostgresCommandBus = orig })
			busErr := errors.New("listen refused")
			startPostgresCommandBus = func(context.Context, string, func(string) bool, func(context.Context, string), *slog.Logger) (hub.CommandBus, error) {
				return nil, busErr
			}

			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = "postgres"
			srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{SharedSigningSecret: signingSecret}, `{"overrides":{"hub.conduit":true}}`)

			_, err := newCommandBus(context.Background(), cfg, srv)
			require.Error(t, err)
			assert.ErrorIs(t, err, busErr)
		})
	})

	t.Run("d_explicit_off_is_off_everywhere", func(t *testing.T) {
		for _, hosted := range []bool{false, true} {
			name := "non_HA"
			if hosted {
				name = "hosted_HA"
			}
			t.Run(name, func(t *testing.T) {
				resetServerFlags()
				hostedMode, enableHub = hosted, true
				t.Setenv("K_SERVICE", "")
				logBuf := captureConduitSlog(t)

				cfg := &config.GlobalConfig{}
				if hosted {
					cfg.Database.Driver = "postgres"
				}
				srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{}, `{"overrides":{"hub.conduit":false}}`)
				configureConduitDeploymentDefault(cfg, srv, hubEndpoint, true)

				assert.False(t, srv.ConduitEnabled())
				assert.NotContains(t, logBuf.String(), "conduit_default_off_reason",
					"explicit off must not log deployment-default-off warning")

				ctx, cancel := context.WithCancel(context.Background())
				var wg sync.WaitGroup
				t.Cleanup(func() { cancel(); wg.Wait() })
				require.NoError(t, startConduit(ctx, newTestShutdown(cancel), cfg, srv, hubEndpoint, &wg, make(chan error, 1)))
			})
		}
	})

	t.Run("e_malformed_overrides_document_is_off", func(t *testing.T) {
		for _, hosted := range []bool{false, true} {
			name := "non_HA"
			if hosted {
				name = "hosted_HA"
			}
			t.Run(name, func(t *testing.T) {
				resetServerFlags()
				hostedMode, enableHub = hosted, true
				t.Setenv("K_SERVICE", "")
				logBuf := captureConduitSlog(t)

				cfg := &config.GlobalConfig{}
				if hosted {
					cfg.Database.Driver = "postgres"
				}
				// Valid JSON so SQLite UpsertHubSetting accepts it, but schema-invalid
				// (non-boolean override value), which OperationalSettings marks Malformed.
				srv := conduitHubServerWithExperimentsRaw(t, hub.ServerConfig{}, `{"overrides":{"hub.conduit":"nope"}}`)
				configureConduitDeploymentDefault(cfg, srv, hubEndpoint, true)

				assert.False(t, srv.ConduitEnabled(), "malformed experiments document must resolve hub.conduit off")
				assert.NotContains(t, logBuf.String(), "conduit_default_off_reason",
					"malformed document must not log deployment-default-off warning")

				ctx, cancel := context.WithCancel(context.Background())
				var wg sync.WaitGroup
				t.Cleanup(func() { cancel(); wg.Wait() })
				require.NoError(t, startConduit(ctx, newTestShutdown(cancel), cfg, srv, hubEndpoint, &wg, make(chan error, 1)))
			})
		}
	})
}

// TestConduitDeploymentDefault_NotResolvedBeforeSetter verifies (f): nothing
// in the pre-setter startup sequence (hub.New, initOperationalSettings,
// newCommandBus) resolves hub.conduit before SetConduitDeploymentDefault runs,
// and the guard panics if anything does resolve it early.
func TestConduitDeploymentDefault_NotResolvedBeforeSetter(t *testing.T) {
	t.Cleanup(resetServerFlags)
	captureStdLog(t)
	captureConduitSlog(t)

	// Prove the guard itself fires on an early resolution.
	t.Run("guard panics on early resolution", func(t *testing.T) {
		st := newTestStore(t)
		srv, err := hub.New(hub.ServerConfig{}, st)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		srv.SetConduitEarlyResolveGuardForTest(true)
		assert.PanicsWithValue(t, "hub.conduit resolved before SetConduitDeploymentDefault", func() {
			_ = srv.ConduitEnabled()
		})
	})

	// Run the pre-setter startup sequence in hosted HA with the guard armed.
	t.Run("pre-setter startup sequence does not resolve hub.conduit", func(t *testing.T) {
		resetServerFlags()
		hostedMode, enableHub = true, true
		t.Setenv("K_SERVICE", "")

		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o600))

		orig := startPostgresCommandBus
		t.Cleanup(func() { startPostgresCommandBus = orig })
		startPostgresCommandBus = func(context.Context, string, func(string) bool, func(context.Context, string), *slog.Logger) (hub.CommandBus, error) {
			return nil, errors.New("listen refused")
		}

		ctx := context.Background()
		st := newTestStore(t)
		srv, err := hub.New(hub.ServerConfig{}, st)
		require.NoError(t, err)
		t.Cleanup(func() { _ = srv.Shutdown(ctx) })
		srv.SetConduitEarlyResolveGuardForTest(true)

		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "postgres"

		require.NoError(t, initOperationalSettings(ctx, cfg, srv, st, globalDir))
		bus, err := newCommandBus(ctx, cfg, srv)
		require.NoError(t, err)
		srv.SetCommandBus(bus)

		// Now run the setter; subsequent resolution must succeed without panicking.
		_, isNoop := bus.(hub.NoopCommandBus)
		configureConduitDeploymentDefault(cfg, srv, "https://hub.example.com", !isNoop)
		assert.NotPanics(t, func() {
			assert.False(t, srv.ConduitEnabled())
		})
		_, err = st.GetHubSetting(ctx, "experiments")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}
