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

package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubexport"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
)

const relayRoleTestPeerSecret = "peer-secret-for-relay-role-tests-0123456789"

// relayRoleTestConfig is a complete --role relay configuration.
func relayRoleTestConfig() *config.GlobalConfig {
	cfg := &config.GlobalConfig{}
	cfg.Hub.Host, cfg.Hub.Port = "127.0.0.1", 18443
	cfg.Hub.Conduit.PeerSecret = relayRoleTestPeerSecret
	cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
	cfg.Hub.Conduit.InstanceID = "relay-7"
	cfg.Hub.Conduit.InternalListen = "127.0.0.1:0"
	cfg.Hub.Conduit.InternalAdvertise = "https://relay-7.internal:9811"
	cfg.Relay.HubInternalURL = "https://hub-internal.example:9811"
	cfg.Relay.PublicURL = "https://relay-7.example.com"
	cfg.Database.Driver, cfg.Database.URL = "sqlite", "relay.db"
	return cfg
}

// TestServerRoleFlag: --role accepts hub and relay only.
func TestServerRoleFlag(t *testing.T) {
	assert.NoError(t, validateServerRole("hub"))
	assert.NoError(t, validateServerRole("relay"))
	assert.ErrorContains(t, validateServerRole("broker"), "invalid --role")
}

// TestValidateRelayRoleConfig: --role relay requires peer_secret, the hub
// internal URL, its public URL and the internal listener; errors name the
// setting, never a value.
func TestValidateRelayRoleConfig(t *testing.T) {
	prevCR := relayOnCloudRun
	t.Cleanup(func() { relayOnCloudRun = prevCR })
	relayOnCloudRun = func() bool { return false }
	require.NoError(t, validateRelayRoleConfig(relayRoleTestConfig()))
	for name, tc := range map[string]struct {
		mod     func(*config.GlobalConfig)
		wantErr string
	}{
		"no peer secret":       {func(c *config.GlobalConfig) { c.Hub.Conduit.PeerSecret = "" }, "server.hub.conduit.peer_secret is required on --role relay"},
		"weak peer secret":     {func(c *config.GlobalConfig) { c.Hub.Conduit.PeerSecret = "short" }, "peer_secret"},
		"no hub internal url":  {func(c *config.GlobalConfig) { c.Relay.HubInternalURL = "" }, "server.relay.hub_internal_url is required"},
		"no public url":        {func(c *config.GlobalConfig) { c.Relay.PublicURL = "" }, "server.relay.public_url is required"},
		"no internal listener": {func(c *config.GlobalConfig) { c.Hub.Conduit.InternalListen = "" }, "server.hub.conduit.internal_listen is required"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := relayRoleTestConfig()
			tc.mod(cfg)
			err := validateRelayRoleConfig(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), relayRoleTestPeerSecret)
		})
	}

	// A peer_secret that is only a newline, loaded from settings.yaml or
	// from either env spelling, is unset: --role relay and external relay
	// mode both refuse to start, naming the key.
	for _, source := range []string{"file", "SCION_SERVER_HUB_CONDUIT_PEERSECRET", "SCION_SERVER_HUB_CONDUIT_PEER_SECRET"} {
		t.Run("newline-only peer secret from "+source, func(t *testing.T) {
			// No inherited server setting may reach the load.
			for _, kv := range os.Environ() {
				if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SCION_SERVER_") {
					t.Setenv(k, v) // registers the restore
					require.NoError(t, os.Unsetenv(k))
				}
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".scion", "settings.yaml")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			settings := "schema_version: \"1\"\n"
			if source == "file" {
				settings += "server:\n  hub:\n    conduit:\n      peer_secret: \"\\n\"\n"
			} else {
				t.Setenv(source, "\n")
			}
			require.NoError(t, os.WriteFile(path, []byte(settings), 0o600))
			loaded, err := config.LoadGlobalConfig(path)
			require.NoError(t, err)

			cfg := relayRoleTestConfig()
			cfg.Hub.Conduit.PeerSecret = loaded.Hub.Conduit.PeerSecret
			assert.ErrorContains(t, validateRelayRoleConfig(cfg), "server.hub.conduit.peer_secret is required on --role relay")

			external := loaded.Hub.Conduit
			external.RelayMode = config.ConduitRelayModeExternal
			assert.ErrorContains(t, validateConduitPeerSecret(external, false, ""), "server.hub.conduit.peer_secret is required when server.hub.conduit.relay_mode is external")
		})
	}
}

// TestValidateRelayRoleConfig_CloudRunSingleton (design §3.9 "Cloud Run
// singleton"): on Cloud Run (K_SERVICE set) a relay without
// server.relay.singleton: true fails, with an error naming the key and the
// three instance settings; with it, or off Cloud Run, the relay starts.
func TestValidateRelayRoleConfig_CloudRunSingleton(t *testing.T) {
	prevCR := relayOnCloudRun
	t.Cleanup(func() { relayOnCloudRun = prevCR })
	for _, tc := range []struct {
		name      string
		cloudRun  bool
		singleton bool
		wantErr   bool
	}{
		{name: "Cloud Run without the assertion", cloudRun: true, wantErr: true},
		{name: "Cloud Run singleton", cloudRun: true, singleton: true},
		{name: "singleton off Cloud Run", singleton: true},
		{name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			relayOnCloudRun = func() bool { return tc.cloudRun }
			cfg := relayRoleTestConfig()
			cfg.Relay.Singleton = tc.singleton
			err := validateRelayRoleConfig(cfg)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range []string{"server.relay.singleton: true", "max_instances=1", "min_instances=1", "cpu_idle=false", "K_SERVICE"} {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
	t.Setenv("K_SERVICE", "scion-relay")
	assert.True(t, prevCR(), "K_SERVICE set means Cloud Run")
	t.Setenv("K_SERVICE", "")
	assert.False(t, prevCR(), "an empty K_SERVICE is not Cloud Run")
}

// TestRunRelayRoleChecksConfigBeforeTheDatabase: an incomplete
// configuration fails before the database is opened.
func TestRunRelayRoleChecksConfigBeforeTheDatabase(t *testing.T) {
	prev := openRelayDB
	t.Cleanup(func() { openRelayDB = prev })
	openRelayDB = func(context.Context, relaydb.Options) (*relaydb.DB, error) {
		t.Fatal("the database was opened")
		return nil, nil
	}
	cfg := relayRoleTestConfig()
	cfg.Hub.Conduit.PeerSecret = ""
	err := runRelayRole(context.Background(), cfg)
	assert.ErrorContains(t, err, "peer_secret is required on --role relay")
}

// TestRunRelayRoleUnsetsServerSecretEnv: by the time the relay opens its
// database, none of the server secret env names is in the process env, the
// relay still has its peer secret in the config, and nothing kept a session
// secret in memory.
func TestRunRelayRoleUnsetsServerSecretEnv(t *testing.T) {
	isolateServerSecretEnv(t)
	for _, name := range serverSecretEnvNames {
		t.Setenv(name, "value-of-"+name)
	}
	prev := openRelayDB
	t.Cleanup(func() { openRelayDB = prev })
	errStop := errors.New("stop before the database")
	cfg := relayRoleTestConfig()
	var present []string
	var peerSecret string
	openRelayDB = func(context.Context, relaydb.Options) (*relaydb.DB, error) {
		for _, name := range serverSecretEnvNames {
			if _, ok := os.LookupEnv(name); ok {
				present = append(present, name)
			}
		}
		peerSecret = cfg.Hub.Conduit.PeerSecretValue()
		return nil, errStop
	}
	require.ErrorIs(t, runRelayRole(context.Background(), cfg), errStop)
	assert.Empty(t, present, "server secret env names still set when the relay opens its database")
	assert.Equal(t, relayRoleTestPeerSecret, peerSecret, "the relay lost its peer secret")
	assert.Empty(t, capturedSessionSecret, "the relay kept a session secret in memory")
}

// TestRelayServerConfigReadsRelaySettings: the relay's configuration comes
// from server.relay, server.hub and server.hub.conduit, its database access
// from the opened relay database, and its relay-peer authenticator from
// peer_secret.
func TestRelayServerConfigReadsRelaySettings(t *testing.T) {
	cfg := relayRoleTestConfig()
	db := &relaydb.DB{Settings: &relaydb.SettingsReader{}}
	rcfg, err := relayServerConfig(context.Background(), cfg, db)
	require.NoError(t, err)
	assert.Equal(t, "relay-7", rcfg.InstanceID)
	assert.Equal(t, "https://hub-internal.example:9811", rcfg.HubInternalURL)
	assert.Equal(t, "https://relay-7.example.com", rcfg.PublicURL)
	assert.False(t, rcfg.Singleton)
	assert.Equal(t, "127.0.0.1:18443", rcfg.PublicListen)
	assert.Equal(t, "127.0.0.1:0", rcfg.InternalListen)
	assert.Equal(t, "https://relay-7.internal:9811", rcfg.Advertise(nil), "server.hub.conduit.internal_advertise")
	assert.NotNil(t, rcfg.PeerAuth)
	assert.Same(t, db.Settings, rcfg.Experiment)
	assert.Nil(t, rcfg.Events, "no LISTEN source on SQLite")

	opts := relayDatabaseOptions(cfg)
	assert.Equal(t, "sqlite", opts.Driver)
	assert.Equal(t, "relay.db", opts.DSN)
}

// TestRelayRoleNeverReadsTheSessionSecret: the relay composition root
// names no session secret accessor, flag or environment spelling
// (resolveSessionSecret, sessionSecretValue, webSessionSecret,
// SCION_SERVER_SESSION_SECRET, SESSION_SECRET, ...) and passes none to the
// relay-peer authenticator.
func TestRelayRoleNeverReadsTheSessionSecret(t *testing.T) {
	src, err := os.ReadFile("server_relay.go")
	require.NoError(t, err)
	assert.Empty(t, relaySessionSecretMentions(string(src)))
}

// relaySessionSecretMentions returns the session secret mentions in src.
func relaySessionSecretMentions(src string) []string {
	return regexp.MustCompile(`(?i)session_?secret|SharedSecret|SharedSigningSecret`).FindAllString(src, -1)
}

// TestRelaySessionSecretCheckCatchesMentions: the check catches each kind
// of mention.
func TestRelaySessionSecretCheckCatchesMentions(t *testing.T) {
	for _, src := range []string{
		"_ = resolveSessionSecret()",
		"_ = sessionSecretValue()",
		"_ = webSessionSecret",
		`os.Getenv("SCION_SERVER_SESSION_SECRET")`,
		`os.Getenv("SESSION_SECRET")`,
		"opts.SharedSecret = s",
		"cfg.SharedSigningSecret",
	} {
		assert.NotEmpty(t, relaySessionSecretMentions(src), src)
	}
	assert.Empty(t, relaySessionSecretMentions("// it never reads the session secret"))
}

// TestRelayStartupLogNamesConfigNotDSN: the relay's startup log names the
// configuration file it loaded and its database driver, never the
// connection string or its credentials.
func TestRelayStartupLogNamesConfigNotDSN(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "server.yaml")
	require.NoError(t, os.WriteFile(file, []byte("hub:\n  port: 18443\n"), 0o600))
	t.Setenv("HOME", t.TempDir()) // no global settings
	cfg := relayRoleTestConfig()
	cfg.Database.Driver = "postgres"
	cfg.Database.URL = "postgres://relay_role:relay-db-password@db.internal:5432/scion?sslmode=require"
	var buf bytes.Buffer
	logRelayStartup(slog.New(slog.NewTextHandler(&buf, nil)), cfg, file)
	out := buf.String()
	assert.Contains(t, out, "server.yaml")
	assert.Contains(t, out, "database_driver=postgres")
	for _, leaked := range []string{"relay-db-password", "relay_role", "db.internal", "postgres://"} {
		assert.NotContains(t, out, leaked)
	}
}

// TestRelayRoleRefusesOtherComponents: --role relay refuses --enable-hub,
// --enable-runtime-broker and --enable-web.
func TestRelayRoleRefusesOtherComponents(t *testing.T) {
	newFlags := func() *pflag.FlagSet {
		f := pflag.NewFlagSet("server start", pflag.ContinueOnError)
		var hub, broker, web bool
		f.BoolVar(&hub, "enable-hub", false, "")
		f.BoolVar(&broker, "enable-runtime-broker", false, "")
		f.BoolVar(&web, "enable-web", false, "")
		return f
	}
	require.NoError(t, validateRelayRoleFlags(newFlags()))
	f := newFlags()
	require.NoError(t, f.Parse([]string{"--enable-hub=false"}))
	require.NoError(t, validateRelayRoleFlags(f), "an explicit false is allowed")
	for _, name := range relayRoleExcludedFlags {
		f := newFlags()
		require.NoError(t, f.Parse([]string{"--" + name}))
		err := validateRelayRoleFlags(f)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "--"+name)
	}
}

// A capped drain gets its grace plus the pad, so Shutdown is not cut off
// before the drain cap ends the drain; an uncapped one keeps 30 s.
func TestRelayShutdownTimeoutPadsPastTheDrainCap(t *testing.T) {
	assert.Equal(t, 70*time.Second, relayShutdownTimeout(60*time.Second, true))
	assert.Equal(t, relayShutdownPad, relayShutdownTimeout(0, true))
	assert.Equal(t, 30*time.Second, relayShutdownTimeout(0, false))
	assert.Equal(t, 30*time.Second, relayShutdownTimeout(60*time.Second, false))
}

// TestRelayMetrics_OffIsANoOp (design §3.9, v2.21.55): with metrics unset
// or off in the relay's settings and no export project, the relay builds
// no exporter and makes no network call: no metadata lookup is offered and
// the exporter constructor is never called; the flush is a no-op.
func TestRelayMetrics_OffIsANoOp(t *testing.T) {
	prev := relayNewMeterProvider
	t.Cleanup(func() { relayNewMeterProvider = prev })
	built := false
	relayNewMeterProvider = func(context.Context, string, ...hubmetrics.Option) (*sdkmetric.MeterProvider, error) {
		built = true
		return sdkmetric.NewMeterProvider(), nil
	}
	t.Setenv(hubexport.ProjectIDEnv, "")
	off := false
	for name, setting := range map[string]*bool{"unset": nil, "off": &off} {
		t.Run(name, func(t *testing.T) {
			cfg := relayRoleTestConfig()
			cfg.Hub.Observability.MetricsEnabled = setting
			in := relayObservabilityInputs(cfg)
			assert.Nil(t, in.metadata, "no metadata-server lookup with metrics not on")
			mp, flush := relayMetrics(context.Background(), cfg, "relay-7", in)
			assert.Nil(t, mp)
			assert.False(t, built, "no exporter is built")
			assert.NoError(t, flush(context.Background()))
		})
	}
}

// TestRelayMetrics_OnBuildsTheRelayProvider: with metrics on and an export
// project, the relay builds its provider with the hub's helper for that
// project, and the flush shuts it down.
func TestRelayMetrics_OnBuildsTheRelayProvider(t *testing.T) {
	prev := relayNewMeterProvider
	t.Cleanup(func() { relayNewMeterProvider = prev })
	var gotProject string
	relayNewMeterProvider = func(_ context.Context, project string, _ ...hubmetrics.Option) (*sdkmetric.MeterProvider, error) {
		gotProject = project
		return sdkmetric.NewMeterProvider(), nil
	}
	t.Setenv(hubexport.ProjectIDEnv, "")
	on := true
	cfg := relayRoleTestConfig()
	cfg.Hub.Observability.MetricsEnabled = &on
	cfg.Hub.GCPProjectID = "relay-metrics-project"
	mp, flush := relayMetrics(context.Background(), cfg, "relay-7", relayMetricsInputs{getenv: os.Getenv})
	require.NotNil(t, mp)
	assert.Equal(t, "relay-metrics-project", gotProject)
	assert.NoError(t, flush(context.Background()))
}

// TestRelayMetrics_UnsetWithAProjectExports: as for the hub (legacy rule),
// with the switch unset an operator-set export project turns relay metrics
// export on; only an explicit off keeps it off.
func TestRelayMetrics_UnsetWithAProjectExports(t *testing.T) {
	prev := relayNewMeterProvider
	t.Cleanup(func() { relayNewMeterProvider = prev })
	built := 0
	relayNewMeterProvider = func(context.Context, string, ...hubmetrics.Option) (*sdkmetric.MeterProvider, error) {
		built++
		return sdkmetric.NewMeterProvider(), nil
	}
	t.Setenv(hubexport.ProjectIDEnv, "")
	cfg := relayRoleTestConfig()
	cfg.Hub.GCPProjectID = "relay-metrics-project"
	mp, flush := relayMetrics(context.Background(), cfg, "relay-7", relayObservabilityInputs(cfg))
	require.NotNil(t, mp, "unset switch with a set project: export on")
	assert.NoError(t, flush(context.Background()))

	off := false
	cfg.Hub.Observability.MetricsEnabled = &off
	mp, _ = relayMetrics(context.Background(), cfg, "relay-7", relayObservabilityInputs(cfg))
	assert.Nil(t, mp, "explicitly off: no export even with a project")
	assert.Equal(t, 1, built)
}
