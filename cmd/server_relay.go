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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubexport"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
)

// scion server --role relay: a separate conduit relay process (design §3.9
// "Separate relay"). This file only composes it: relayserver runs the
// relay, relaydb is its database access (the registry store, the
// hub.conduit reader and the revocation events), and relay builds its
// relay-peer authenticator. It never reads the session secret. A test in
// pkg/conduit/relayrole/relayserver checks the imports of this file.

// Server roles (--role).
const (
	serverRoleHub   = "hub"
	serverRoleRelay = "relay"
)

// serverRole is --role.
var serverRole = serverRoleHub

// validateServerRole checks --role.
func validateServerRole(role string) error {
	switch role {
	case serverRoleHub, serverRoleRelay:
		return nil
	}
	return fmt.Errorf("invalid --role %q: want hub or relay", role)
}

// validateRelayRoleConfig checks the configuration a separate relay needs.
// Errors name the setting, never a value.
func validateRelayRoleConfig(cfg *config.GlobalConfig) error {
	if err := validateConduitPeerSecret(cfg.Hub.Conduit, true, ""); err != nil {
		return err
	}
	if err := config.ValidatePeerSecret(cfg.Hub.Conduit.PeerSecretValue()); err != nil {
		return err
	}
	if err := cfg.Hub.Conduit.Validate(); err != nil {
		return err
	}
	if err := cfg.Relay.Validate(); err != nil {
		return err
	}
	switch {
	case cfg.Relay.HubInternalURL == "":
		return errors.New("server.relay.hub_internal_url is required on --role relay")
	case cfg.Relay.PublicURL == "":
		return errors.New("server.relay.public_url is required on --role relay")
	case cfg.Hub.Conduit.InternalListen == "":
		return errors.New("server.hub.conduit.internal_listen is required on --role relay")
	case relayOnCloudRun() && !cfg.Relay.Singleton:
		return errRelaySingletonRequired
	}
	return nil
}

// errRelaySingletonRequired refuses a relay on Cloud Run without the
// singleton assertion (design §3.9 "Cloud Run singleton"): Cloud Run
// instances are not individually addressable, so only a single instance
// may run, which the relay cannot verify itself.
var errRelaySingletonRequired = errors.New("on Cloud Run (K_SERVICE is set) a relay runs only as a non-HA singleton: set server.relay.singleton: true " +
	"and run the service with max_instances=1, min_instances=1 and cpu_idle=false")

// relayOnCloudRun reports whether the relay runs on Cloud Run or Knative
// serving (K_SERVICE is set). No metadata-server call is made.
var relayOnCloudRun = func() bool { return os.Getenv("K_SERVICE") != "" }

// logRelayStartup names the configuration files the relay loaded and its
// database driver. The relay reads only its own configuration; the
// connection string and its credentials are never logged.
func logRelayStartup(log *slog.Logger, cfg *config.GlobalConfig, configPath string) {
	log.Info("Conduit relay role starting", "config_files", config.ServerConfigFiles(configPath), "database_driver", cfg.Database.Driver)
}

// relayRoleExcludedFlags are the server components --role relay never runs.
var relayRoleExcludedFlags = []string{"enable-hub", "enable-runtime-broker", "enable-web"}

// validateRelayRoleFlags refuses --role relay together with a flag that
// enables another server component.
func validateRelayRoleFlags(flags *pflag.FlagSet) error {
	for _, name := range relayRoleExcludedFlags {
		if f := flags.Lookup(name); f != nil && f.Changed && f.Value.String() == "true" {
			return fmt.Errorf("--role relay runs only the conduit relay; remove --%s", name)
		}
	}
	// The relay never loads the server keyring (ptone/scion#1530 §2); its
	// peer secret contract is separate.
	for _, name := range []string{"keyring-file", "keyring-file-allow-world-readable"} {
		if f := flags.Lookup(name); f != nil && f.Changed {
			return fmt.Errorf("--role relay never reads a server keyring; remove --%s", name)
		}
	}
	return nil
}

// relayDatabaseOptions is the relay's database connection input.
func relayDatabaseOptions(cfg *config.GlobalConfig) relaydb.Options {
	return relaydb.Options{
		Driver:        cfg.Database.Driver,
		DSN:           cfg.Database.URL,
		MaxOpenConns:  cfg.Database.MaxOpenConns,
		MaxIdleConns:  cfg.Database.MaxIdleConns,
		EventSubjects: append(append([]string(nil), relayserver.RevocationSubjects...), relayserver.SettingsUpdatedSubject),
	}
}

// openRelayDB opens the relay's database access (a var for tests).
var openRelayDB = relaydb.Open

// relayServerConfig builds the relay's configuration from cfg. It reads
// the relay and conduit settings and builds the relay-peer authenticator
// from server.hub.conduit.peer_secret; there is no session secret input.
func relayServerConfig(ctx context.Context, cfg *config.GlobalConfig, db *relaydb.DB) (relayserver.Config, error) {
	if err := validateRelayRoleConfig(cfg); err != nil {
		return relayserver.Config{}, err
	}
	id := relay.InstanceID(cfg.Hub.Conduit.InstanceID)
	onGCP := metadata.OnGCE()
	opts := relay.ConduitPeerAuthOptions{
		Mode:            cfg.Hub.Conduit.PeerAuthMode(),
		OnGCP:           onGCP,
		SelfID:          id,
		PeerSecret:      cfg.Hub.Conduit.PeerSecretValue(),
		Audience:        cfg.Hub.Conduit.PeerAudience,
		ServiceAccounts: cfg.Hub.Conduit.PeerServiceAccounts,
	}
	if onGCP {
		if email, err := metadata.EmailWithContext(ctx, "default"); err == nil {
			opts.OwnServiceAccount = email
		}
	}
	auth, mode, err := relay.NewConduitPeerAuth(opts)
	if err != nil {
		return relayserver.Config{}, err
	}
	slog.Info("Conduit relay-peer authentication", "mode", mode)
	podIP := os.Getenv("POD_IP")
	hopServerTLS, hopClientTLS, err := conduitHopTLS(cfg.Hub.Conduit)
	if err != nil {
		return relayserver.Config{}, err
	}
	// The public listener trusts a forwarded protocol only from the
	// operator's server.trusted_proxies; empty (the default) trusts none.
	trusted, err := config.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return relayserver.Config{}, err
	}
	return relayserver.Config{
		InstanceID:     id,
		PublicURL:      cfg.Relay.PublicURL,
		HubInternalURL: cfg.Relay.HubInternalURL,
		PublicListen:   net.JoinHostPort(cfg.Hub.Host, strconv.Itoa(cfg.Hub.Port)),
		InternalListen: cfg.Hub.Conduit.InternalListen,
		Advertise: func(bound net.Addr) string {
			return conduitAdvertiseEndpoint(cfg.Hub.Conduit.InternalAdvertise, bound, podIP, hopServerTLS != nil)
		},
		InternalTLS:      hopServerTLS,
		HopTLS:           hopClientTLS,
		PeerAuth:         auth,
		TrustedProxies:   trusted,
		Registry:         db.Registry,
		Experiment:       db.Settings,
		Events:           db.Events(),
		StartEvents:      db.StartEvents,
		RecheckInterval:  conduitAuthzRecheckIntervalSetting(cfg),
		ReconnectWindow:  conduitReconnectWindowSetting(cfg),
		LifetimeCap:      conduitLifetimeCapSetting(cfg),
		RequireSelfCheck: hostedMode && isHADeployment(cfg),
		Singleton:        cfg.Relay.Singleton,
	}, nil
}

// runRelayRole runs scion server --role relay until a signal or a fatal
// relay error.
func runRelayRole(ctx context.Context, cfg *config.GlobalConfig) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The config is loaded: remove the server secret env names (unread)
	// so nothing the relay starts inherits them.
	unsetServerSecretEnv()
	if err := validateRelayRoleConfig(cfg); err != nil {
		return err
	}
	logRelayStartup(slog.Default(), cfg, serverConfigPath)
	db, err := openRelayDB(ctx, relayDatabaseOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	rcfg, err := relayServerConfig(ctx, cfg, db)
	if err != nil {
		return err
	}
	mp, flushMetrics := relayMetrics(ctx, cfg, rcfg.InstanceID, relayObservabilityInputs(cfg))
	if mp != nil {
		rcfg.MeterProvider = mp
	}
	srv, err := relayserver.Start(ctx, rcfg)
	if err != nil {
		_ = flushMetrics(context.Background())
		return err
	}
	runErr := srv.Wait(ctx)
	grace, capped := conduitShutdownDrainSetting(cfg)
	sctx, cancel := context.WithTimeout(context.Background(), relayShutdownTimeout(grace, capped))
	defer cancel()
	_ = srv.Shutdown(sctx, capped, relay.ShutdownDrainCap(grace))
	// After the drain, within the same shutdown deadline: flushing metrics
	// never extends the SIGTERM handling.
	_ = flushMetrics(sctx)
	return runErr
}

// relayNewMeterProvider builds the metrics exporter (a test seam).
var relayNewMeterProvider = hubmetrics.NewMeterProvider

// relayMetricsInputs are the process inputs of the relay's export
// decision (passed in so the decision is testable).
type relayMetricsInputs struct {
	getenv   func(string) string
	metadata func() (string, error)
}

// relayObservabilityInputs returns the relay's export inputs. The
// metadata-server project lookup is offered only when metrics are
// explicitly on, so with the switch unset or off the decision makes no
// network call.
func relayObservabilityInputs(cfg *config.GlobalConfig) relayMetricsInputs {
	in := relayMetricsInputs{getenv: os.Getenv}
	if on := cfg.Hub.Observability.MetricsEnabled; on != nil && *on {
		in.metadata = func() (string, error) {
			if !metadata.OnGCE() {
				return "", errors.New("not on GCP")
			}
			return metadata.ProjectIDWithContext(context.Background())
		}
	}
	return in
}

// relayMetricsDecision is the hub's metrics export decision
// (hubexport.ResolveProject and hubexport.DecideMetrics over
// server.hub.observability.metrics_enabled and the export project), read
// from the relay's own settings. The relay has no operational settings
// service, so the switch comes from the loaded config, as for a hub
// without one.
func relayMetricsDecision(cfg *config.GlobalConfig, in relayMetricsInputs) hubexport.Decision {
	project := hubexport.ResolveProject(hubexport.ProjectInputs{
		Configured: cfg.Hub.GCPProjectID,
		EnvValue:   in.getenv(hubexport.ProjectIDEnv),
		Metadata:   in.metadata,
	})
	return hubexport.DecideMetrics(cfg.Hub.Observability.MetricsEnabled, project)
}

// relayMetrics builds the relay process's metrics export exactly like the
// hub's (design §3.9, v2.21.55): the same observability switch
// (server.hub.observability.metrics_enabled in the relay's own settings)
// and export project, the same exporter helper, with service name
// scion-relay and the relay instance id. With metrics off, or no project,
// it returns a nil provider and a no-op flush: no exporter and no network.
// The returned flush shuts the provider down within the caller's context.
func relayMetrics(ctx context.Context, cfg *config.GlobalConfig, instanceID string, in relayMetricsInputs) (metric.MeterProvider, func(context.Context) error) {
	noop := func(context.Context) error { return nil }
	status := relayMetricsDecision(cfg, in)
	if !status.Enabled {
		slog.Info("Conduit relay metrics export", "status", status.LogLine())
		return nil, noop
	}
	mp, err := relayNewMeterProvider(ctx, status.Project.ID,
		hubmetrics.WithServiceName("scion-relay"), hubmetrics.WithInstanceID(instanceID))
	if err != nil {
		slog.Warn("Conduit relay metrics export disabled", "error", err)
		return nil, noop
	}
	slog.Info("Conduit relay metrics export", "status", status.LogLine())
	return mp, mp.Shutdown
}

// relayShutdownPad is how long Shutdown may run past a capped drain: the
// drain cap ends the drain, and the pad leaves the HTTP servers time to
// close after it.
const relayShutdownPad = 10 * time.Second

// relayShutdownTimeout is the deadline for the relay's Shutdown: 30 s by
// default, or the configured grace plus relayShutdownPad when the drain is
// capped, so Shutdown is not cut off before the drain cap ends the drain.
func relayShutdownTimeout(grace time.Duration, capped bool) time.Duration {
	if capped {
		return grace + relayShutdownPad
	}
	return 30 * time.Second
}
