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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	policytroubleshooteriam "cloud.google.com/go/policytroubleshooter/iam/apiv3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/api/option"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hostlimits"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubexport"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubtracing"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/reapermetrics"
	scionplugin "github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	cloudrunruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secretmigration"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
	gcputil "github.com/GoogleCloudPlatform/scion/pkg/util/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/web"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/cobra"
)

// isRootWorkingDir reports whether wd is the filesystem root. A daemon
// supervisor started without an explicit working directory (for example a
// systemd unit with no WorkingDirectory=) leaves its child process here,
// which is rarely a sane base for cwd-derived path resolution. On Windows
// os.Getwd always returns a volume-qualified path (C:\ or \\server\share\),
// never a bare separator, so this never matches there and the fallback
// below is a no-op on that platform.
func isRootWorkingDir(wd string) bool {
	return wd == string(filepath.Separator)
}

// chdirHomeIfAtFilesystemRoot changes the process's working directory to the
// user's home directory when (and only when) the current working directory
// is the filesystem root ("/"). It is called after logging, config loading,
// and the global-directory setup above it in runServerStart have already
// run, so it does not affect any of those -- only the cwd-dependent
// resolution later in runServerStart (from the FindProjectRoot check right
// below its call site onward) and in code called after it. This is
// therefore not a substitute for --global, which sets the project path
// before those earlier steps run; it only catches the narrower case of a
// daemon supervisor that left the process at "/" (for example a systemd
// unit with no WorkingDirectory=) for whatever runs afterward.
//
// It is a best-effort fallback, not a hard requirement: a failure to read
// $HOME or to chdir into it is logged, not fatal, so it can never turn a
// previously-working invocation into a startup failure.
func chdirHomeIfAtFilesystemRoot() {
	wd, err := os.Getwd()
	if err != nil || !isRootWorkingDir(wd) {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		log.Printf("Working directory was %s; could not determine home directory: %v", wd, err)
		return
	}
	if err := os.Chdir(home); err != nil {
		log.Printf("Working directory was %s; failed to change to home directory %s: %v", wd, home, err)
		return
	}
	log.Printf("Working directory was %s; changed to home directory %s", wd, home)
}

// pinProcessUTC is util.PinProcessUTC; a var so the cmd test binary can
// disable it (see TestMain in main_test.go). Running the real pin inside a
// test would race goroutines leaked by earlier tests (both read and write
// time.Local) and would silently switch every later test in the binary to
// UTC regardless of TZ, masking real timezone bugs. Placement of every call
// to this seam is enforced by the AST test in pin_process_utc_test.go, not
// by this comment.
var pinProcessUTC = util.PinProcessUTC

// Seams for tests that drive runServerStart up to a failed hub
// initialization. Production uses the real functions.
var (
	initServerLoggingFn = initServerLogging
	initHubServerFn     = initHubServer
)

func runServerStart(cmd *cobra.Command, args []string) error {
	// Pin the process to UTC before anything else runs (log timestamps, cron
	// parsing, ent's Default(time.Now), etc. all read time.Local).
	pinProcessUTC()

	// 1. Initialize logging
	logCleanups, requestLogger, messageLogger, artifactLogger, err := initServerLoggingFn(cmd)
	if err != nil {
		return err
	}
	// The log cleanups (OTel, Cloud Logging flush) run once: deferred here,
	// or earlier on the graceful shutdown's early exit (awaitServerExit),
	// which ends the process without returning (ptone/scion#5054).
	flushLogs := onceCleanups(logCleanups)
	defer flushLogs()

	// 2. Load & reconcile config
	cfg, err := loadAndReconcileConfig(cmd)
	if err != nil {
		return err
	}
	if err := validateServerRole(serverRole); err != nil {
		return err
	}
	if serverRole == serverRoleRelay {
		// A separate conduit relay: no hub, Runtime Broker or web.
		if err := validateRelayRoleFlags(cmd.Flags()); err != nil {
			return err
		}
		applyServerLogLevelSetting(cfg.LogLevel)
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		return runRelayRole(ctx, cfg)
	}
	// Server secrets are now in memory; remove them from the process env
	// before anything below can start a child process. Two earlier
	// spawns remain: on macOS the container runtime probe during logging
	// setup, and the legacy settings migration's git ls-files during
	// config load (pkg/config gitTracked), which runs with these names
	// filtered out of its env.
	scrubServerSecretEnv()
	// Apply server.log_level (settings or SCION_SERVER_LOGLEVEL) at setting
	// precedence. The level filter and the cloud, request and message
	// handlers built by initServerLogging follow the shared level state, so
	// this takes effect for every later record.
	applyServerLogLevelSetting(cfg.LogLevel)
	if enableHub {
		warnRetiredServerSettings(slog.Default(), serverConfigPath)
		if err := validateHubWorkspaceStorage(cfg); err != nil {
			return err
		}
	}

	// 3. Resolve admin mode settings
	adminMode := cfg.AdminMode
	if os.Getenv("SCION_SERVER_ADMIN_MODE") != "" {
		adminMode = parseBoolEnv("SCION_SERVER_ADMIN_MODE")
	}
	maintenanceMessage := cfg.MaintenanceMessage
	if v := os.Getenv("SCION_SERVER_MAINTENANCE_MESSAGE"); v != "" {
		maintenanceMessage = v
	}

	// 4. Ensure global directory exists
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}
	if _, err := os.Stat(globalDir); os.IsNotExist(err) {
		log.Println("Initializing global scion directory...")
		initOpts := config.InitMachineOpts{SkipRuntimeCheck: hostedMode}
		if err := config.InitGlobal(harness.EmbedOnlyHarnesses(), initOpts); err != nil {
			return fmt.Errorf("failed to initialize global config: %w", err)
		}
	} else if !hostedMode {
		// In workstation mode, refresh the default template and harness-configs
		// from the binary's embeds. Hosted mode bootstraps directly into the Hub
		// via BootstrapBundledResources, bypassing local ~/.scion materialization.
		if err := config.UpdateDefaultTemplates(true, harness.EmbedOnlyHarnesses()); err != nil {
			log.Printf("Warning: failed to refresh default templates: %v", err)
		}
	} else {
		// In hosted mode, materialize any missing harness configs from the
		// binary's embedded catalog. This ensures newly added harness configs
		// from binary updates are available on disk without a full InitGlobal.
		// Force=false refreshes bundle-owned files (config.yaml and the
		// provisioner scripts) but preserves other operator files.
		if err := config.MaterializeBundledHarnessConfigs(globalDir, config.MaterializeOptions{Force: false}); err != nil {
			log.Printf("Warning: failed to materialize missing harness configs: %v", err)
		}
	}

	// When --global is set, change to the home directory so the server
	// operates from the global project context regardless of where it was launched.
	if globalMode {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		if err := os.Chdir(home); err != nil {
			return fmt.Errorf("failed to change to home directory: %w", err)
		}
		log.Printf("Global mode: changed working directory to %s", home)
	} else {
		chdirHomeIfAtFilesystemRoot()
	}

	// Warn if running from within a project directory instead of the global (~/.scion) context.
	if projectDir, ok := config.FindProjectRoot(); ok {
		if projectDir != globalDir {
			parentDir := filepath.Dir(projectDir)
			fmt.Fprint(os.Stderr, util.ColorFor(os.Stderr, fmt.Sprintf("\n%s%s WARNING: Server is running from a project directory context (%s)%s\n",
				util.Bold, util.Yellow, parentDir, util.Reset)))
			fmt.Fprint(os.Stderr, util.ColorFor(os.Stderr, fmt.Sprintf("%s%s          Startup settings come from ~/.scion, but templates and some settings read while the server runs may still come from this project.%s\n",
				util.Bold, util.Yellow, util.Reset)))
			fmt.Fprint(os.Stderr, util.ColorFor(os.Stderr, fmt.Sprintf("%s%s          For machine-wide operation, run the server from outside any project directory.%s\n\n",
				util.Bold, util.Yellow, util.Reset)))
		}
	}

	// 4b. Workstation mode only: without an image registry the co-located
	// broker cannot pull agent images. Rather than exiting, start the hub
	// and web without the broker so the onboarding wizard (which sets the
	// registry) is reachable. This runs before the hub is created so the
	// broker is never marked as expected (ExpectEmbeddedBroker), which
	// would leave /healthz degraded. Hosted mode keeps failing fast at
	// broker start (step 13). The broker cannot be started in place later,
	// so the fix is a restart.
	//
	// A registry saved through the wizard lives in the hub database, which
	// is not open yet. When the env and file check fails and the hub is
	// enabled, the decision is deferred until the store is open (step 8)
	// and completes there, before the hub is created (step 11)
	// (ptone/scion#3916).
	registryDecisionDeferred := false
	registryFromHubDB := false
	// The env and file check reads the global settings only, like the
	// readiness check before a daemon start (server_start_ready.go), never
	// a project found from the working directory (ptone/scion#5362).
	requireServerImageRegistry := func() error { return requireImageRegistryForBrokerAt(globalDir) }
	if !hostedMode {
		disableBroker, err := workstationBrokerRegistryDegrade(cfg.RuntimeBroker.Enabled, enableHub || enableWeb, requireServerImageRegistry)
		if err != nil {
			return err
		}
		switch {
		case disableBroker && enableHub:
			// Off until step 8b decides, so the checks before it (the
			// port check included) treat the broker as not started, as
			// they did before the deferral; 8b re-checks its port if it
			// turns the broker back on.
			registryDecisionDeferred = true
			cfg.RuntimeBroker.Enabled = false
		case disableBroker:
			disableWorkstationBrokerForRegistry(cfg)
		}
	}

	// 5. Check if at least one server is enabled
	if !enableHub && !cfg.RuntimeBroker.Enabled && !enableWeb {
		return fmt.Errorf("no server components enabled; use --enable-hub, --enable-runtime-broker, or --enable-web")
	}

	validateHostedBasic(cfg)
	if err := validateHostedHAPreflight(cfg); err != nil {
		return err
	}
	if err := validateServerPreflight(cfg); err != nil {
		return err
	}
	if err := validateBrokerStateDir(cfg); err != nil {
		return err
	}

	// 6. Check ports
	if err := checkServerPorts(cfg); err != nil {
		return err
	}

	// Log server mode
	if hostedMode {
		log.Println("Server mode: hosted")
	} else {
		log.Printf("Server mode: workstation (binding to %s)", cfg.Hub.Host)
	}
	if enableDebug {
		slog.Debug("Debug logging enabled")
		logOAuthDebug(cfg)
	}

	// 7. Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One graceful shutdown for the whole process (ptone/scion#5054):
	// components register pre-cancel steps on it (shutdownSeq.addStep)
	// instead of subscribing to signals themselves. The signal handler
	// lives until this function returns (signalsCtx), so a second signal
	// can force the shutdown during the teardown. The returned done
	// channel is for tests only: do not wait on it here (architect ruling
	// R11b).
	shutdownSeq := newServerShutdownSequence(cancel, shutdownClock)
	signalsCtx, stopSignals := context.WithCancel(context.Background())
	defer stopSignals()
	_ = installServerShutdownSignals(signalsCtx, signal.Notify, shutdownSeq)
	// The whole-process shutdown budget (shutdown_grace when set, else
	// none: each phase keeps its own bound) and its phase deadlines; see
	// cmd/server_shutdown.go for the order.
	shutdownPlan := serverShutdownPlan{q: shutdownSeq, budget: serverShutdownBudgetFor(cfg)}
	// With the budget set, the process exits within the grace whatever
	// close is still running.
	shutdownPlan.armExitWatchdog(signalsCtx.Done())
	// The hub front waits for the co-located broker before it closes its
	// listener (hub.ShutdownHooks.BeforeHTTPDrain).
	brokerStop := newColocatedBrokerStop()
	frontShutdownHooks := hub.ShutdownHooks{
		Context:          shutdownPlan.teardownContext,
		HTTPDrainContext: shutdownPlan.httpDrainContext,
		BeforeHTTPDrain:  brokerStop.beforeHTTPDrain(shutdownClock, brokerStopWaitFallback, shutdownPlan.brokerContext),
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	// Server boot hook: warn about legacy environment variables scion no
	// longer reads, before project discovery and storage init. This is the
	// server-boot half of the legacy-migration hook points;
	// the CLI equivalent is Execute()/rootCmd.PersistentPreRunE in
	// cmd/root.go (skipped for the "start" command under "server" or
	// "runtime-broker" — this hook reports instead), and the broker
	// equivalent is pkg/runtimebroker/server.go:(*Server).Start. This hook
	// and the broker's share config's process-wide sync.Once
	// (WarnRemovedLegacyEnvOnce), so a combined `--enable-hub
	// --enable-runtime-broker` process reports once, not twice; the CLI
	// hook is skipped for this command and plays no part in that dedup.
	// Unconditional (not gated on enableHub): a
	// `--enable-web`-only process still boots via this same command and
	// must still report, since the CLI hook is skipped for the whole
	// `server start` invocation regardless of which components it enables.
	// On-disk layout migration is expected to run at this same hook point.
	config.WarnRemovedLegacyEnvOnce(os.Getenv, config.NewSlogReporter())
	// Legacy settings and marker key migration (as projects load) reports
	// through slog here too. This is already the default, but set it
	// explicitly so all three boot hooks (CLI, hub, broker) are visible at
	// their call sites.
	config.SetProjectMigrationReporter(config.NewSlogReporter())
	// Migrate the global ~/.scion layout before project discovery and
	// storage init scan it (step 8 below and beyond).
	config.MigrateLegacyGlobalLayoutOnce(globalDir, config.NewSlogReporter())

	// 8. Initialize store
	if err := validateTestModeRequiresHub(enableHub); err != nil {
		return err
	}
	if err := validateTestModeHubIDPinned(cfg); err != nil {
		return err
	}
	var s store.Store
	var entClient *ent.Client
	if enableHub {
		s, entClient, err = initStore(ctx, cfg)
		if err != nil {
			return err
		}
		if closer, ok := s.(io.Closer); ok {
			defer func() { _ = closer.Close() }()
		}
	}

	// 8b. Complete the deferred workstation registry decision (step 4b):
	// a saved hub value, or a hub file-tier value (SCION_SERVER_* /
	// SCION_SEED_*), keeps the broker; otherwise it is not started.
	if registryDecisionDeferred {
		settingStore, _ := s.(store.HubSettingStore)
		active := colocatedActiveProfile()
		if managedImageRegistryConfigured(ctx, settingStore, active) || bootstrapImageRegistryConfigured(serverConfigPath, active) {
			registryFromHubDB = true
			cfg.RuntimeBroker.Enabled = true
			if err := checkBrokerPort(cfg); err != nil {
				return err
			}
		} else {
			disableWorkstationBrokerForRegistry(cfg)
		}
	}

	// Load the global settings early so both Hub and Broker can use the
	// legacy hub.endpoint fallback.
	brokerSettings, brokerDefaultProfile := loadServerSettings()
	if brokerSettings.Hub == nil {
		brokerSettings.Hub = &config.HubClientConfig{}
	}

	// 9. Initialize dev auth
	var devAuthToken string
	if cfg.Auth.Enabled {
		devAuthToken, err = initDevAuth(cfg, globalDir)
		if err != nil {
			return err
		}
		if hostedMode {
			log.Println("WARNING: Development authentication enabled - not for production use")
		}
	}

	// 10. Resolve hub endpoint: the operator endpoint (environment, flags,
	// settings.yaml and fallbacks), then the endpoint in effect, which a
	// server.hub.public_url saved in the settings database can replace
	// unless an operator setting pins it (ptone/scion#3905). Resolved before
	// the hub is constructed so every derivation sees the same value.
	operatorEndpoint, operatorEndpointSrc := resolveServerHubEndpoint(cfg, brokerSettings)
	hubEndpoints, err := resolveHubEndpoints(ctx, s, operatorHubEndpoint{URL: operatorEndpoint, Source: operatorEndpointSrc})
	if err != nil {
		return err
	}
	hubEndpoint, hubEndpointSrc := effectiveHubEndpoint(hubEndpoints, operatorEndpointSrc)

	// Parse admin emails
	adminEmailList := parseAdminEmails(cfg)

	// Initialize secret backend before plugin manager so that boot-time
	// inline-secret migration can write to the backend before
	// ResolvePluginConfig strips inline secrets.
	var secretBackend secret.SecretBackend
	if enableHub && s != nil {
		hubID := cfg.Hub.ResolveHubID()
		var sbErr error
		secretBackend, sbErr = secret.NewBackend(ctx, cfg.Secrets.Backend, s, secret.GCPBackendConfig{
			ProjectID:            cfg.Secrets.GCPProjectID,
			CredentialsJSON:      cfg.Secrets.GCPCredentials,
			ReplicationLocations: cfg.Secrets.GCPReplicationLocations,
		}, hubID, resolveSessionSecret())
		if sbErr != nil {
			log.Printf("Warning: failed to initialize secret backend: %v", sbErr)
		}
		if gcpBackend, ok := secretBackend.(*secret.GCPBackend); ok {
			gcpBackend.SetHubName(cfg.Hub.ResolveHubName())
		}
	}

	// 10b. Initialize plugin manager
	pluginMgr := initPluginManager(ctx, secretBackend, s)
	defer pluginMgr.Shutdown()
	// From the shutdown signal on, plugin process exits are expected: the
	// service manager's SIGTERM can reach the plugins before Shutdown
	// above runs (ptone/scion#4869).
	stopPluginShutdownMark := context.AfterFunc(ctx, pluginMgr.BeginShutdown)
	defer stopPluginShutdownMark()
	// The graceful shutdown's mark comes before cancel (after the conduit
	// drain), so plugin exits are expected from the mark on
	// (ptone/scion#5054); the AfterFunc above covers the other returns.
	shutdownSeq.addStep(shutdownPhaseMark, pluginMgr.BeginShutdown)

	// 11. Start Hub
	var hubSrv *hub.Server
	var hubDBRec dbmetrics.Recorder
	if enableHub {

		var hubInitErr error
		hubSrv, hubInitErr = initHubServerFn(ctx, cfg, s, entClient, hubEndpoints, devAuthToken, adminEmailList, adminMode, maintenanceMessage, requestLogger, messageLogger, artifactLogger, globalDir, pluginMgr, secretBackend)
		if hubInitErr != nil {
			// Return rather than log.Fatalf so the deferred cleanups (store
			// close, plugin shutdown, log flush including Cloud Logging) run.
			// Startup stays fail-closed: nothing is serving yet. The exit
			// status is still 1 (exitCodeFor).
			log.Printf("Hub server failed to start: %v", hubInitErr)
			return fmt.Errorf("hub server failed to start: %w", hubInitErr)
		}

		// Flush telemetry providers before the store closes on exit.
		exit := &hubExitSequence{}
		defer exit.run()

		// Close the decision-log audit writer on EVERY return from here on
		// (architect ruling R5), including the startup-failure returns in
		// steps 11-13 that can happen after the Hub API (step 11) or the
		// web server (step 12) is already serving. Bounded by the writer's
		// own drain timeout; idempotent with the step-16 close in
		// awaitServerExit and with Server.Shutdown's close. Defers run LIFO:
		// this runs before exit.run (registered just above, so telemetry
		// flushes after the writer's final counts) and before the log
		// cleanups deferred at step 1.
		defer func() {
			closeCtx, cancelClose := context.WithTimeout(context.Background(), asyncwrite.DefaultDrainTimeout)
			defer cancelClose()
			_ = hubSrv.CloseAuditWriter(closeCtx)
		}()

		// /healthz cloud_logging: present only when initServerLogging
		// configured a Cloud Logging handler (P2).
		wireCloudLoggingHealth(hubSrv, serverCloudWriter())

		// The co-located broker registers (startRuntimeBroker, step 13)
		// only after the Hub API is serving. Mark it as expected now, under
		// the same condition startRuntimeBroker registers it, so gates that
		// depend on the embedded broker ID (hub-default GCP passthrough)
		// wait for registration instead of misclassifying the broker.
		if colocatedBrokerRegisters(cfg, s) {
			hubSrv.ExpectEmbeddedBroker()
		}

		// Metrics and traces share one resolved identity. initHubServerFn
		// has applied the operational settings, so the hub name is the
		// applied one (ptone/scion#4903).
		telemetryID := newHubTelemetryIdentity(hubSrv)

		// Decide the hub's own exporters once, from the observability
		// switches and the export project (ptone/scion#4914). Each
		// exporter logs one line saying whether it runs and why.
		obs := hubObservabilityExport(cfg, hubSrv)

		// Wire hub OTel tracing export to Cloud Trace.
		if obs.Tracing.Enabled {
			tp, tpErr := hubtracing.NewTracerProvider(ctx, obs.Tracing.Project.ID, telemetryID.tracingOptions()...)
			if tpErr != nil {
				obs.Tracing = obs.Tracing.Failed(tpErr)
			} else {
				exit.addFlush(tp.Shutdown)
			}
		}
		log.Print(obs.Tracing.LogLine())

		// Wire hub OTel metrics export to Cloud Monitoring.
		if obs.Metrics.Enabled {
			mp, mpErr := hubmetrics.NewMeterProvider(ctx, obs.Metrics.Project.ID, telemetryID.metricsOptions()...)
			if mpErr != nil {
				obs.Metrics = obs.Metrics.Failed(mpErr)
			} else {
				exit.addFlush(mp.Shutdown)

				hubDBRec = wireHubCoreMetrics(hubSrv, mp)

				if hubSrv.GetBrokerAuthService() != nil {
					otelMetrics, otelAuthErr := hub.NewOTelMetricsRecorder(mp)
					if otelAuthErr != nil {
						log.Printf("WARNING: hub auth metrics OTel export disabled: %v", otelAuthErr)
					} else {
						hubSrv.SetMetrics(otelMetrics)
					}
				}

				otelGCP, otelGCPErr := hub.NewOTelGCPTokenMetrics(mp)
				if otelGCPErr != nil {
					log.Printf("WARNING: hub GCP token metrics OTel export disabled: %v", otelGCPErr)
				} else {
					hubSrv.SetGCPTokenMetrics(otelGCP)
				}

				otelExtBearer, otelExtBearerErr := hub.NewOTelExternalBearerMetrics(mp, hubSrv.ExternalBearerSnapshotMetrics())
				if otelExtBearerErr != nil {
					log.Printf("WARNING: hub external-bearer metrics OTel export disabled: %v", otelExtBearerErr)
				} else {
					hubSrv.SetExternalBearerMetrics(otelExtBearer)
					hubSrv.SetGoogleValidatorCacheMetrics(otelExtBearer)
					hubSrv.SetGEExchangeMetrics(otelExtBearer)
				}
			}
		}
		log.Print(obs.Metrics.LogLine())
		hubSrv.SetObservabilityExportStatus(obs)

		// Wire command bus for cross-node dispatch (B2-4).
		cmdBus, err := newCommandBus(ctx, cfg, hubSrv)
		if err != nil {
			return err
		}
		hubSrv.SetCommandBus(cmdBus)

		// Conduit relay (hub.conduit): after operational settings are
		// loaded (initHubServer) and before the background services start,
		// which register the registry singleton only when a relay runs.
		// Mark the hub shutting down first when the shutdown begins
		// (/readyz 503, new work refused, streams ended).
		shutdownSeq.addStep(shutdownPhaseMark, hubSrv.BeginShutdown)
		hubSrv.SetShutdownHooks(frontShutdownHooks)

		if err := startConduit(ctx, shutdownSeq, cfg, hubSrv, hubEndpoints.For(hub.HubEndpointForConduitCheck), &wg, errCh); err != nil {
			return err
		}

		if !enableWeb {
			// Hub runs its own HTTP server (standalone mode).
			eventPub, err := newEventPublisher(ctx, cfg, hubDBRec)
			if err != nil {
				return err
			}
			hubSrv.SetEventPublisher(eventPub)
			startSettingsPropagation(ctx, hubSrv, eventPub)

			log.Printf("Starting Hub API server on %s:%d", cfg.Hub.Host, cfg.Hub.Port)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := hubSrv.Start(ctx); err != nil {
					errCh <- fmt.Errorf("hub server error: %w", err)
				}
			}()
		} else {
			// Combined mode: Hub API is mounted on the Web server.
			// Background services (scheduler, notification dispatcher) are
			// started after initWebServer sets the ChannelEventPublisher.
			// The web server runs the hub's background teardown after
			// its own HTTP drain (WebServer.Shutdown, mounted by
			// MountHubAPI); the audit writer closes after every server
			// has drained (awaitServerExit).
			log.Printf("Hub API will be mounted on Web server (port %d)", webPort)
		}
	}

	// 12. Start Web
	var webSrv *hub.WebServer
	if enableWeb {
		webSrv, err = initWebServer(ctx, cfg, hubSrv, devAuthToken, adminMode, maintenanceMessage, requestLogger, hubDBRec)
		if err != nil {
			return err
		}
		shutdownSeq.addStep(shutdownPhaseMark, webSrv.BeginShutdown)
		webSrv.SetShutdownHooks(frontShutdownHooks)

		// In combined mode, start Hub background services now that the
		// ChannelEventPublisher has been wired by initWebServer.
		if enableHub && hubSrv != nil {
			hubSrv.StartBackgroundServices(ctx)
		}

		if !web.AssetsEmbedded && webAssetsDir == "" {
			slog.Warn("This binary was built without web assets. The web UI will not be available. Run 'make all' (or 'make web && make build') to rebuild with web assets included, or use --web-assets-dir.")
		} else if web.AssetsEmbedded && webAssetsDir == "" {
			sub, err := fs.Sub(web.ClientAssets, "dist/client")
			if err != nil {
				slog.Error("Failed to create sub-filesystem from embedded assets. The web UI will not be available. Run 'make web && make build' to rebuild with web assets included, or use --web-assets-dir.", "error", err)
			} else if _, err := fs.Stat(sub, "assets/main.js"); err != nil {
				slog.Warn("Embedded web assets are incomplete (main.js missing). The web UI will not be available. Run 'make all' (or 'make web && make build') to rebuild with web assets included, or use --web-assets-dir.")
			}
		}
		log.Printf("Starting Web Frontend on %s:%d", cfg.Hub.Host, webPort)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := webSrv.Start(ctx); err != nil {
				errCh <- fmt.Errorf("web server error: %w", err)
			}
		}()
	}

	// 13. Start Broker
	// On a Cloud Run service host, warn once that async launches need CPU
	// allocated outside requests (ptone/scion#4763). No-op elsewhere.
	cloudrunruntime.WarnCPUAllocationIfNeeded(slog.Default(), os.Getenv, cfg.RuntimeBroker.Enabled)
	if cfg.RuntimeBroker.Enabled {
		if !registryFromHubDB {
			if err := requireServerImageRegistry(); err != nil {
				return err
			}
		}
		// The broker's goroutines join brokerWG, so the hub front can
		// wait for them (brokerStop) before closing its listener; the
		// server WaitGroup waits for brokerWG.
		var brokerWG sync.WaitGroup
		brokerStop.markStarted()
		brokerErr := startRuntimeBroker(ctx, cmd, cfg, hubSrv, webSrv, s, hubEndpoint, hubEndpointSrc, devAuthToken, brokerSettings, brokerDefaultProfile, globalDir, requestLogger, messageLogger, &brokerWG, errCh, shutdownPlan.brokerContext)
		wg.Add(1)
		go func() {
			defer wg.Done()
			brokerWG.Wait()
			brokerStop.markStopped()
		}()
		if brokerErr != nil {
			return brokerErr
		}
	}
	// Phase 2 item 5: one, centralized call site for the shared_dir_storage
	// startup summary, gated by sharedDirStorageStartupLogWanted so a
	// hub-only process, a broker-only
	// process, and a combined hub+broker process all log it exactly once
	// (logSharedDirStorageStartupOnce's own sync.Once is what makes it
	// "exactly once", not this condition).
	if sharedDirStorageStartupLogWanted(cfg.RuntimeBroker.Enabled, enableHub) {
		logSharedDirStorageStartupOnce()
	}

	// 13b. Re-check image status for all active harness configs now that
	// the local image checker has been registered by startRuntimeBroker.
	if enableHub && hubSrv != nil {
		go hubSrv.RecheckAllImageStatuses(ctx)
	}

	// 14. Set up dispatcher, message broker, and notification dispatcher.
	// This must happen after the ChannelEventPublisher is set (step 11/12)
	// so that StartMessageBroker and StartNotificationDispatcher can
	// create their proxies with the real event publisher.
	if enableHub && hubSrv != nil {
		dispatcher := hubSrv.CreateAuthenticatedDispatcher()
		hubSrv.SetDispatcher(dispatcher)
		log.Printf("Agent dispatcher configured (HTTP-based)")

		warnShadowedBrokerEnv(ctx, dispatcher)

		// Initialize web chat store and attachment store. These only need a
		// DB handle, not the message broker, so they live outside the broker
		// gate to work on single-node instances without broker plugins.
		var webStore hub.WebChatStore
		if dbProvider, ok := s.(interface{ DB() *sql.DB }); ok {
			if rawDB := dbProvider.DB(); rawDB != nil {
				// Artifact store: creates the artifact_* tables (outside
				// the Ent migration graph, design D3) on every start,
				// whether or not the hub.artifacts experiment is on.
				as := artifacts.NewStore(rawDB, cfg.Database.Driver)
				if err := as.Init(ctx); err != nil {
					log.Printf("Warning: failed to initialize artifact store: %v", err)
				} else {
					hubSrv.SetArtifactStore(as)
					log.Printf("Artifact store initialized")
				}

				ws := hub.NewWebChatStore(rawDB, cfg.Database.Driver)
				if err := ws.Init(); err != nil {
					log.Printf("Warning: failed to initialize webchat store: %v", err)
				} else {
					webStore = ws
					hubSrv.SetWebChatStore(webStore)
					log.Printf("Web chat store initialized")

					// W7: Initialize local-disk attachment store.
					globalDir, err := config.GetGlobalDir()
					if err != nil {
						log.Printf("Warning: could not determine global dir, attachments disabled: %v", err)
					} else {
						attachDir := filepath.Join(globalDir, "attachments")
						attachStore, err := hub.NewLocalDiskAttachmentStore(attachDir)
						if err != nil {
							log.Printf("Warning: failed to initialize attachment store: %v", err)
						} else {
							hubSrv.SetAttachmentStore(attachStore)
							log.Printf("Attachment store initialized: dir=%s", attachDir)
						}
					}
				}
			}
		}

		// Initialize message broker from versioned settings.
		// Uses FanOutBroker to support multiple simultaneous broker plugins.
		if vs, err := loadServerVersionedSettings(); err == nil && vs.Server != nil {
			// Auto-enable message broker when broker plugins are configured
			// but message_broker.enabled was not explicitly set (#472).
			if vs.Server.Plugins != nil && len(vs.Server.Plugins.Broker) > 0 {
				if vs.Server.MessageBroker == nil {
					vs.Server.MessageBroker = &config.V1MessageBrokerConfig{}
				}
				if !vs.Server.MessageBroker.Enabled {
					vs.Server.MessageBroker.Enabled = true
					log.Printf("Auto-enabled message broker for configured broker plugin(s)")
				}
			}

			// Reconcile: ensure every configured broker plugin is listed in
			// message_broker.types (in-memory AND on disk). Covers both empty
			// and partially-populated type lists.
			if vs.Server.Plugins != nil && vs.Server.MessageBroker != nil && len(vs.Server.Plugins.Broker) > 0 {
				typesSet := make(map[string]bool, len(vs.Server.MessageBroker.Types))
				for _, t := range vs.Server.MessageBroker.Types {
					typesSet[t] = true
				}
				for name := range vs.Server.Plugins.Broker {
					if !typesSet[name] {
						vs.Server.MessageBroker.Types = append(vs.Server.MessageBroker.Types, name)
						if err := config.AddPluginToMessageBrokerTypes(name); err != nil {
							log.Printf("Warning: failed to persist %s to message_broker.types: %v", name, err)
						} else {
							log.Printf("NOTICE: auto-added %q to message_broker.types (was configured but missing)", name)
						}
					}
				}
			}

			// Warn on plugin-not-in-types
			if vs.Server.Plugins != nil && vs.Server.MessageBroker != nil &&
				vs.Server.MessageBroker.Enabled && len(vs.Server.MessageBroker.Types) > 0 {
				typesSet := make(map[string]bool, len(vs.Server.MessageBroker.Types))
				for _, t := range vs.Server.MessageBroker.Types {
					typesSet[t] = true
				}
				for name := range vs.Server.Plugins.Broker {
					if !typesSet[name] {
						log.Printf("WARNING: Broker plugin %q is loaded but not listed in message_broker.types — it will NOT participate in message routing", name)
					}
				}
			}

			if vs.Server.MessageBroker != nil && vs.Server.MessageBroker.Enabled {
				var namedBuses []eventbus.NamedEventBus

				// InProcessEventBus is always present for local pub/sub routing.
				inproc := eventbus.NewInProcessEventBus(logging.Subsystem("hub.eventbus.inprocess"))
				namedBuses = append(namedBuses, eventbus.NamedEventBus{Name: "inprocess", Bus: inproc})

				// Resolve the list of plugin broker types.
				brokerTypes := vs.Server.MessageBroker.Types
				if len(brokerTypes) == 0 && vs.Server.MessageBroker.Type != "" && vs.Server.MessageBroker.Type != "inprocess" {
					brokerTypes = []string{vs.Server.MessageBroker.Type}
				}

				for _, bt := range brokerTypes {
					if !pluginMgr.HasPlugin(scionplugin.PluginTypeBroker, bt) {
						log.Printf("Warning: broker plugin %q not loaded, skipping", bt)
						continue
					}
					b, pluginErr := pluginMgr.GetBroker(bt)
					if pluginErr != nil {
						log.Printf("Warning: failed to get broker plugin %q: %v", bt, pluginErr)
						continue
					}

					// Inject hub credentials into hub-managed, non-HA broker plugins.
					// Self-managed plugins handle their own credential lifecycle;
					// HA integrations pull credentials from env/Secret Manager.
					if !pluginMgr.IsSelfManaged(scionplugin.PluginTypeBroker, bt) &&
						pluginMgr.GetDeploymentMode(scionplugin.PluginTypeBroker, bt) != scionplugin.DeploymentModeHA &&
						hubSrv != nil && s != nil {
						// Use the same deterministic UUIDv5 as the α migration so the
						// broker entity created here matches the migrated ID.
						pluginBrokerNS := uuid.MustParse("5c104390-a1d0-5e9a-9b1e-5c104390a1d0")
						legacyID := "plugin-broker-" + bt
						brokerID := uuid.NewSHA1(pluginBrokerNS, []byte(legacyID)).String()
						if authSvc := hubSrv.GetBrokerAuthService(); authSvc != nil {
							// Ensure the runtime broker entity exists (required by
							// the broker_secrets foreign key constraint).
							if _, err := s.GetRuntimeBroker(ctx, brokerID); err != nil {
								pluginBroker := &store.RuntimeBroker{
									ID:              brokerID,
									Name:            "plugin-" + bt,
									Slug:            api.Slugify("plugin-" + bt),
									Version:         "0.1.0",
									Status:          store.BrokerStatusOnline,
									ConnectionState: "embedded",
									Labels:          map[string]string{"scion.io/plugin": bt},
									Created:         time.Now(),
									Updated:         time.Now(),
								}
								if createErr := s.CreateRuntimeBroker(ctx, pluginBroker); createErr != nil {
									log.Printf("Warning: failed to register broker entity for plugin %q: %v", bt, createErr)
								}
							}
							// The hub provisions this record in-process: the test
							// admin tier admits it by this id, never by a stored
							// attribute.
							hubSrv.RegisterInProcessPluginBroker(brokerID)
							secretKey, secretErr := authSvc.GenerateAndStoreSecret(ctx, brokerID)
							if secretErr != nil {
								log.Printf("Warning: failed to generate secret for broker plugin %q: %v", bt, secretErr)
							} else {
								// hub_url (public, for links people open) and
								// hub_api_url (loopback, for the plugin's own
								// API calls; this plugin runs on the hub's host).
								hubCreds := hubSrv.HubManagedPluginURLCreds()
								hubCreds["hmac_key"] = secretKey
								hubCreds["broker_id"] = brokerID
								hubCreds["plugin_name"] = bt
								// Inject project slug map so hub-managed plugins can resolve
								// human-readable project names without user-level API access.
								if projects, listErr := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500}); listErr == nil {
									slugMap := make(map[string]string, len(projects.Items))
									for _, p := range projects.Items {
										if p.Slug != "" {
											slugMap[p.ID] = p.Slug
										} else {
											slugMap[p.ID] = p.Name
										}
									}
									if jsonBytes, jsonErr := json.Marshal(slugMap); jsonErr == nil {
										hubCreds["project_slug_map"] = string(jsonBytes)
									}
								}
								// Inject transport auth config so plugins can authenticate
								// with IAP-protected hub endpoints.
								if cfg.Auth.Transport != nil && cfg.Auth.Transport.Mode != "" {
									hubCreds["transport_mode"] = cfg.Auth.Transport.Mode
								}
								if cfg.Auth.Transport != nil && cfg.Auth.Transport.OIDCAudience != "" {
									hubCreds["transport_audience"] = cfg.Auth.Transport.OIDCAudience
								}
								if cfg.Database.Driver != "" && cfg.Database.Driver != "sqlite" {
									hubCreds["database_driver"] = cfg.Database.Driver
									hubCreds["database_url"] = cfg.Database.URL
								}
								// Inject chat integration secrets from the secret backend.
								// Pass the plugin's merged config so secrets are only injected
								// when not already set by file or inline config.
								brokerCfg := pluginMgr.GetPluginConfig(scionplugin.PluginTypeBroker, bt)
								injectPluginSecrets(ctx, secretBackend, bt, brokerCfg, hubCreds)
								if cfgErr := pluginMgr.ConfigureBroker(bt, hubCreds); cfgErr != nil {
									log.Printf("Warning: failed to inject hub credentials into broker plugin %q: %v", bt, cfgErr)
								} else {
									log.Printf("Injected hub credentials into broker plugin %q (broker_id=%s)", bt, brokerID)
								}
							}
						}
					}

					observer := isObserverBroker(pluginMgr, bt)
					channelID := pluginChannelID(pluginMgr, bt)
					namedBuses = append(namedBuses, eventbus.NamedEventBus{
						Name: bt, Bus: b, Observer: observer, ChannelID: channelID,
					})
					log.Printf("Message broker spoke added: name=%s channel_id=%s observer=%v", bt, channelID, observer)
				}

				// Register the native web channel spoke if web chat store is
				// available. The spoke follows the same contract as every plugin
				// adapter: Subscribe discards the handler (F7/F8, #944), Publish
				// does real work (webchat_* state). Observer: true so a
				// state-write failure degrades the thread rail rather than
				// failing the user's message.
				if webStore != nil {
					webSpoke := hub.NewWebChannelBus(logging.Subsystem("hub.eventbus.web"), webStore)
					namedBuses = append(namedBuses, eventbus.NamedEventBus{
						Name:      "web",
						ChannelID: "web",
						Bus:       webSpoke,
						Observer:  true,
					})
					log.Printf("Message broker spoke added: name=web channel_id=web observer=true")
				}

				fanout := eventbus.NewFanOutEventBus(namedBuses, logging.Subsystem("hub.eventbus.fanout"))
				hubSrv.StartMessageBroker(fanout)
				log.Printf("Message broker started: fan-out with %d spoke(s)", len(namedBuses))

				// Wire the broker proxy as the host callbacks target for broker plugins.
				if proxy := hubSrv.GetMessageBrokerProxy(); proxy != nil {
					pluginMgr.SetBrokerHostCallbacks(proxy)
				}
			}
		} else if err != nil {
			log.Printf("Warning: failed to load settings; message broker is not started: %v", err)
		}

		hubSrv.StartNotificationDispatcher()
		hubSrv.InitPresenceManager()
	}

	// 14a. Test admin tier broker check: once the co-located broker (step
	// 13) and the hub's in-process plugin broker records (step 14) are
	// known, no other runtime broker may be registered and active.
	// Registration and broker authentication of any other broker are
	// refused from hub construction on, so this closes the startup window.
	if enableHub && hubSrv != nil {
		if err := hubSrv.VerifyTestAdminBrokerSet(ctx); err != nil {
			return err
		}
	}

	// 15. Print startup banner
	if !hostedMode {
		logWorkstationReadyBanner(cfg, globalDir, enableWeb, webPort, devAuthToken)
	}

	// 16. Wait for either an error or context cancellation
	var closeAudit func(context.Context) error
	if hubSrv != nil {
		closeAudit = hubSrv.CloseAuditWriter
	}
	return awaitServerExit(ctx, errCh, cancel, wg.Wait, closeAudit, shutdownPlan.exitSignal(signalsCtx.Done()), shutdownPlan.exitCloseContext, flushLogs)
}

// serverShutdown is the run-once graceful shutdown that
// installServerShutdownSignals publishes once the process is subscribed to
// SIGINT and SIGTERM (runServerStart step 7). Nil until then. The foreground
// broker's SIGTERM handler calls it directly (see handleBrokerSIGTERM).
var serverShutdown atomic.Pointer[func(os.Signal)]

// installServerShutdownSignals is step 7 of runServerStart, the process's
// one SIGINT/SIGTERM handler (ptone/scion#5054): the first signal (Ctrl+C,
// systemd, Kubernetes, container stop) begins q, the single graceful
// shutdown, which logs "Received signal ...", runs q's pre-cancel steps
// (mark shutting down, then the conduit drain) and cancels the server
// context. No other component subscribes to these signals, so nothing
// cancels before the steps have run. A second signal from outside the
// process on this subscription forces the shutdown (q.force): cancel at
// once and stop waiting for the servers, so an interactive Ctrl+C never
// waits out a drain. Neither the foreground broker's direct call
// (serverShutdown) nor a SIGINT the process sent itself (interruptSelf)
// counts toward that. Orchestrators that escalate to
// SIGKILL after their grace period still terminate the process
// immediately. notify is signal.Notify in production and injectable in
// tests.
//
// ctx is the handler's lifetime: runServerStart passes one that ends when
// it returns (not the server context, which the shutdown itself cancels),
// so a second signal is handled for the whole teardown. A signal arriving
// after ctx ends is swallowed by the subscription, which is never stopped,
// unless the foreground broker's SIGTERM handler calls the published
// shutdown, which then does nothing more than a no-op begin.
// The returned done channel is closed as the goroutine's last action; it
// exists for tests, and production must not wait on it.
//
// The run-once shutdown is published in serverShutdown strictly after
// notify returns, so a caller that finds it unpublished knows the server
// may not be subscribed yet.
func installServerShutdownSignals(ctx context.Context, notify func(chan<- os.Signal, ...os.Signal), q *serverShutdownSequence) (done <-chan struct{}) {
	shutdown := func(sig os.Signal) { q.begin(sig) }

	sigCh := make(chan os.Signal, 1)
	notify(sigCh, os.Interrupt, syscall.SIGTERM)

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		// received counts the signals from outside the process seen on
		// this subscription. The first begins the shutdown (or finds it
		// already begun by the foreground broker's direct call for the
		// same SIGTERM); only a second one forces it. A SIGINT the
		// process sent itself is never counted.
		received := 0
		for {
			select {
			case sig := <-sigCh:
				self := sig == os.Interrupt && consumeSelfInterrupt()
				if !self {
					received++
				}
				if q.claim() {
					// The steps run off this goroutine, so a second
					// signal can force the shutdown while they run.
					go q.run(sig)
					continue
				}
				if received >= 2 {
					q.force(sig)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	serverShutdown.Store(&shutdown)
	return exited
}

// awaitServerExit is step 16 of runServerStart. On cancellation it calls
// wait (production: the server WaitGroup's Wait; each server's Start
// returns only after its shutdown and HTTP drain), then closes the hub
// audit writer, so records
// emitted by draining requests are written; this runs before
// runServerStart's deferred log cleanups. On a server error it cancels and
// closes the writer without waiting: requests still draining then have
// their audit records counted as closed. closeAudit may be nil (no hub);
// the close is bounded by the writer's own drain timeout.
//
// exit (may be nil) ends the wait early (ptone/scion#5054): it is closed
// when a second signal forces the shutdown, or at the teardown deadline
// of the shutdown budget (server.hub.conduit.shutdown_grace). The servers
// are then still running, so the store and the other deferred closes must
// not run under them: awaitServerExit closes the audit writer, then
// flushes the logs (flushLogs, may be nil), both within exitClose's bound,
// and ends the process (exitProcess) with exitCodeUnfinishedShutdown.
func awaitServerExit(ctx context.Context, errCh <-chan error, cancel context.CancelFunc, wait func(), closeAudit func(context.Context) error, exit <-chan struct{}, exitClose func() (context.Context, context.CancelFunc), flushLogs func()) error {
	select {
	case err := <-errCh:
		cancel()
		if closeAudit != nil {
			_ = closeAudit(context.Background())
		}
		return err
	case <-ctx.Done():
		waited := make(chan struct{})
		go func() {
			wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-exit:
			log.Printf("Exiting before the servers stopped (shutdown forced or its budget used up)")
			closeCtx, cancelClose := exitClose()
			flushed := make(chan struct{})
			go func() {
				defer close(flushed)
				if closeAudit != nil {
					_ = closeAudit(closeCtx)
				}
				if flushLogs != nil {
					flushLogs()
				}
			}()
			select {
			case <-flushed:
			case <-closeCtx.Done():
			}
			cancelClose()
			exitProcess(exitCodeUnfinishedShutdown)
			return nil // reached only when exitProcess is a test fake
		}
		if closeAudit != nil {
			_ = closeAudit(context.Background())
		}
		return nil
	}
}

// brokerEnvShadowWarner is the single method warnShadowedBrokerEnv needs. It
// exists so the boot wiring can be exercised without standing up a hub.Server.
type brokerEnvShadowWarner interface {
	WarnOutrankedBrokerEnvKeys(context.Context) error
}

// warnShadowedBrokerEnv runs the one-shot boot check for runtime_broker-scoped
// env vars that a higher-precedence scope now overrides. It is the operator-
// facing half of demoting runtime_broker to the weakest env storage scope: the
// hub cannot tell a deliberately pinned broker value from an accidental one, so
// it must not migrate them, and naming them once at boot is the only warning
// available.
//
// It runs SYNCHRONOUSLY, and that is the point. Behind a goroutine it would
// reproduce the exact defect it exists to report: if ctx were cancelled between
// here and the point the queries ran, no warning would ever be emitted and the
// diff would still look done. A handful of small SELECTs on a path that runs
// once per process is not worth that trade.
//
// The failure branch says DID NOT RUN rather than "failed", because from the
// operator's side those are one event: no list of shadowed keys was produced.
// The absence of warnings below must not be read as "nothing is shadowed".
func warnShadowedBrokerEnv(ctx context.Context, w brokerEnvShadowWarner) {
	if w == nil {
		return
	}
	if err := w.WarnOutrankedBrokerEnvKeys(ctx); err != nil {
		slog.Warn("Broker env shadow check DID NOT RUN. Treat the absence of shadowed-key warnings as unknown, not as none.",
			"error", err)
	}
}

// initServerLogging initializes all logging subsystems and returns cleanup functions.
func initServerLogging(cmd *cobra.Command) (cleanups []func(), requestLogger, messageLogger, artifactLogger *slog.Logger, err error) {
	useGCP := parseBoolEnv("SCION_LOG_GCP")
	if cloudrunenv.IsService() {
		useGCP = true
	}
	if !hostedMode && os.Getenv("SCION_LOG_GCP") == "" {
		useGCP = false
	}

	component := "scion-server"
	if enableHub && !enableRuntimeBroker {
		component = "scion-hub"
	} else if !enableHub && enableRuntimeBroker {
		component = "scion-broker"
	}

	hubName := resolveHubNameFromEnv()
	hubID := resolveHubIDFromEnv()

	// Initialize OTel logging
	ctx := context.Background()
	logProvider, logCleanup, otelErr := logging.InitOTelLogging(ctx, logging.OTelConfig{})
	if otelErr != nil {
		log.Printf("Warning: failed to initialize OTel logging: %v", otelErr)
	}
	if logCleanup != nil {
		cleanups = append(cleanups, logCleanup)
	}

	// Initialize direct Cloud Logging with circuit breaker protection.
	// If Cloud Logging becomes unavailable (e.g. during a metadata
	// service outage), the circuit breaker opens and the hub falls back to
	// local-only logging automatically.
	// Check env var first (SCION_CLOUD_LOGGING), then fall back to settings.
	// When the env var is set (even to "false"), it takes precedence over
	// the settings value. Only consult settings when the env var is unset.
	var cloudLoggingEnabled bool
	if os.Getenv(logging.EnvCloudLogging) != "" {
		cloudLoggingEnabled = logging.IsCloudLoggingEnabled()
	} else {
		// The global settings, whether or not --global is given.
		if vs, err := loadServerVersionedSettings(); err == nil && vs.Telemetry != nil && vs.Telemetry.Cloud != nil && vs.Telemetry.Cloud.CloudLogging != nil {
			cloudLoggingEnabled = *vs.Telemetry.Cloud.CloudLogging
		}
	}
	var cloudHandler slog.Handler
	if cloudLoggingEnabled {
		logLevel := logging.ResolveLogLeveler(enableDebug)
		logCfg := logging.CloudLoggingConfig{
			Component: component,
			HubName:   hubName,
			HubID:     hubID,
		}
		ch, cloudLogCleanup, cloudErr := logging.NewCloudHandler(ctx, logCfg, logLevel)
		if cloudErr != nil {
			log.Printf("Warning: failed to initialize Cloud Logging: %v", cloudErr)
		} else {
			// Wrap with resilient handler for circuit breaker protection.
			resilientHandler, resilientCleanup := logging.NewResilientCloudHandler(
				ch, logging.ResilientCloudHandlerConfig{},
			)
			cloudHandler = resilientHandler
			cleanups = append(cleanups, cloudLogCleanup)
			cleanups = append(cleanups, resilientCleanup)
			log.Printf("Cloud Logging enabled with circuit breaker (logId=%s, project=%s)", logging.FormatLogID(), logging.FormatProjectID())
		}
	}

	logging.SetupWithOTel(component, hubName, enableDebug, useGCP, logProvider, cloudHandler)

	// Initialize request logger
	reqLogCfg := logging.RequestLoggerConfig{
		FilePath:   os.Getenv(logging.EnvRequestLogPath),
		Component:  component,
		HubName:    hubName,
		HubID:      hubID,
		UseGCP:     useGCP,
		Foreground: serverStartForeground,
		Level:      logging.ResolveLogLeveler(enableDebug),
	}
	if ch, ok := cloudHandler.(*logging.ResilientCloudHandler); ok && ch != nil {
		reqLogCfg.CloudClient = ch.Client()
		reqLogCfg.CircuitOpen = ch.CircuitOpen
		reqLogCfg.ProjectID = logging.FormatProjectID()
	}
	requestLogger, reqLogCleanup, reqErr := logging.NewRequestLogger(reqLogCfg)
	if reqErr != nil {
		slog.Warn("Failed to initialize request logger", "error", reqErr)
		requestLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if reqLogCleanup != nil {
		cleanups = append(cleanups, reqLogCleanup)
	}

	// Initialize message logger
	msgLogCfg := logging.MessageLoggerConfig{
		Component: component,
		HubName:   hubName,
		HubID:     hubID,
		UseGCP:    useGCP,
		Level:     logging.ResolveLogLeveler(enableDebug),
	}
	if ch, ok := cloudHandler.(*logging.ResilientCloudHandler); ok && ch != nil {
		msgLogCfg.CloudClient = ch.Client()
		msgLogCfg.CircuitOpen = ch.CircuitOpen
	}
	messageLogger, msgLogCleanup, msgErr := logging.NewMessageLogger(msgLogCfg)
	if msgErr != nil {
		slog.Warn("Failed to initialize message logger", "error", msgErr)
		messageLogger = nil
	}
	if msgLogCleanup != nil {
		cleanups = append(cleanups, msgLogCleanup)
	}

	// Initialize the artifact decision logger (hub only; the broker serves
	// no artifacts).
	if enableHub {
		artLogCfg := logging.ArtifactLoggerConfig{
			Component: component,
			HubName:   hubName,
			HubID:     hubID,
			UseGCP:    useGCP,
			Level:     logging.ResolveLogLeveler(enableDebug),
		}
		if ch, ok := cloudHandler.(*logging.ResilientCloudHandler); ok && ch != nil {
			artLogCfg.CloudClient = ch.Client()
			artLogCfg.CircuitOpen = ch.CircuitOpen
		}
		var artLogCleanup func()
		var artErr error
		artifactLogger, artLogCleanup, artErr = logging.NewArtifactLogger(artLogCfg)
		if artErr != nil {
			slog.Warn("Failed to initialize artifact logger", "error", artErr)
			artifactLogger = nil
		}
		if artLogCleanup != nil {
			cleanups = append(cleanups, artLogCleanup)
		}
	}

	return cleanups, requestLogger, messageLogger, artifactLogger, nil
}

// applyServerLogLevelSetting applies the server.log_level setting to the
// shared level state and logs the resolved level and its source. Precedence
// is the --debug flag, then SCION_LOG_LEVEL (or SCION_DEBUG), then
// server.log_level, then the default (info).
func applyServerLogLevelSetting(level string) {
	logging.ApplyLogLevelSetting("server.log_level", level)
	logging.LogResolvedLevel(slog.Default())
}

// applyServerListenHosts applies the listen-host rules for the hub/web
// listener (cfg.Hub.Host) and the runtime broker listener
// (cfg.RuntimeBroker.Host). hostSet and host are the --host flag state.
//
//   - --host sets only the hub and web listen address, in every mode.
//   - Workstation mode: the hub defaults to loopback when --host is not set.
//     The co-located broker stays on loopback unless the broker host is set
//     explicitly (server.broker.host); --host does not move it.
//   - Hosted mode: a standalone broker (broker enabled, hub disabled) defaults
//     to loopback when --host is not set, so that `scion broker register` can
//     reach it locally before HMAC keys exist. Otherwise the broker keeps its
//     configured host.
func applyServerListenHosts(cfg *config.GlobalConfig, hosted, hostSet bool, host string, hubEnabled bool) {
	if !hosted {
		if !hostSet {
			cfg.Hub.Host = "127.0.0.1"
		}
		if !brokerHostExplicit(cfg) {
			cfg.RuntimeBroker.Host = "127.0.0.1"
		}
	} else if !hostSet && cfg.RuntimeBroker.Enabled && !hubEnabled {
		cfg.RuntimeBroker.Host = "127.0.0.1"
	}
	if hostSet {
		cfg.Hub.Host = host
	}
}

// brokerHostExplicit reports whether the loaded settings set the broker host
// to something other than the built-in default. The loaded config does not
// record which keys came from a file or the environment, so a value equal to
// the default (config.DefaultBrokerHost) cannot be told apart from an unset
// one and counts as not explicit.
func brokerHostExplicit(cfg *config.GlobalConfig) bool {
	h := cfg.RuntimeBroker.Host
	return h != "" && h != config.DefaultBrokerHost
}

// validateHubWorkspaceStorage fails hub startup when server.workspace_storage
// cannot be used: an unknown backend, nfs without shares, a volume backend
// without volume_name, or an invalid subpath_root. Without it the hub would
// quietly fall back to ephemeral local project paths and only readiness
// would notice. A broker-only process never calls this: it only logs
// startup warnings (brokerNFSConfig, brokerWorkspaceStorageWarning).
func validateHubWorkspaceStorage(cfg *config.GlobalConfig) error {
	if cfg == nil {
		return nil
	}
	// Defaults are applied during config load; applying them again is
	// idempotent and keeps this check independent of the load path.
	cfg.WorkspaceStorage.ApplyWorkspaceStorageDefaults()
	if err := cfg.WorkspaceStorage.ValidateWorkspaceStorage(); err != nil {
		// The wrapped error already names the key (for example
		// server.workspace_storage.nfs.uid), so do not repeat it here.
		return fmt.Errorf("invalid workspace storage settings: %w", err)
	}
	return nil
}

// warnRetiredServerSettings logs one warning for each retired server setting
// still set in the server config or environment. The loader ignores them, so
// without this a hub would silently change behaviour after an upgrade
// (ptone/scion#4991).
func warnRetiredServerSettings(logger *slog.Logger, configPath string) {
	for _, w := range config.RetiredServerSettingWarnings(configPath) {
		logger.Warn(w)
	}
}

// loadAndReconcileConfig loads the server configuration file and reconciles
// it with command-line flags and workstation defaults.
func loadAndReconcileConfig(cmd *cobra.Command) (*config.GlobalConfig, error) {
	// An unparseable SCION_SERVER_* / SCION_SEED_* value fails startup with
	// an error naming the variable, rather than being ignored or falling
	// back to a default (ptone/scion#3911).
	if err := config.ValidateServerEnv(os.Environ(), opsettings.IsLayer1Key); err != nil {
		return nil, err
	}
	// Legacy spellings of a setting still work for one more release; name
	// each one set, and any that another spelling overrides. Once per start.
	config.WarnDeprecatedServerEnvAliases(slog.Default(), os.Environ(), opsettings.IsLayer1Key)
	cfg, err := config.LoadGlobalConfig(serverConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	// Reject an unknown server.mode (settings.yaml or SCION_SERVER_MODE)
	// rather than silently treating it as workstation.
	if err := config.ValidateServerMode(cfg.Mode); err != nil {
		return nil, err
	}

	// Check if hosted mode is set in config
	if !cmd.Flags().Changed("hosted") && !cmd.Flags().Changed("production") {
		if cfg.Mode == "hosted" || cfg.Mode == "production" {
			hostedMode = true
		}
	}

	if err := validateDebugEndpoints(hostedMode, enableDebugEndpoints); err != nil {
		return nil, err
	}
	// Test mode: the flag-only checks (removed flags, G3, G5) run before
	// anything else is reconciled; the full gate runs at hub construction.
	if err := validateTestModeFlagsEarly(cmd); err != nil {
		return nil, err
	}

	// Apply workstation defaults
	if !hostedMode {
		applyWorkstationDefaults(cmd)
		cfg.RuntimeBroker.Enabled = enableRuntimeBroker
		cfg.Auth.Enabled = enableDevAuth
		if !cmd.Flags().Changed("storage-bucket") {
			cfg.Storage.Provider = "local"
		}
		cfg.Secrets.Backend = "local"

		// Auto-detect gcloud ADC credentials for GCP clients.
		if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
			if home, err := os.UserHomeDir(); err == nil {
				adcPath := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
				if _, err := os.Stat(adcPath); err == nil {
					if err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adcPath); err != nil {
						log.Printf("GCP: failed to set GOOGLE_APPLICATION_CREDENTIALS: %v", err)
					} else {
						log.Printf("GCP: auto-detected gcloud ADC credentials at %s", adcPath)
					}
				}
			}
		}
	}

	// Override with command-line flags
	if cmd.Flags().Changed("port") {
		cfg.Hub.Port = hubPort
	}
	if cmd.Flags().Changed("db") {
		cfg.Database.URL = dbURL
	}
	if cmd.Flags().Changed("enable-runtime-broker") {
		cfg.RuntimeBroker.Enabled = enableRuntimeBroker
	}
	if cmd.Flags().Changed("runtime-broker-port") {
		cfg.RuntimeBroker.Port = runtimeBrokerPort
	}
	if cmd.Flags().Changed("dev-auth") {
		cfg.Auth.Enabled = enableDevAuth
	}
	if cmd.Flags().Changed("storage-bucket") {
		cfg.Storage.Bucket = storageBucket
	}
	if cmd.Flags().Changed("storage-dir") {
		cfg.Storage.LocalPath = storageDir
	}
	applyConduitFlagOverrides(cmd, cfg)

	applyServerListenHosts(cfg, hostedMode, cmd.Flags().Changed("host"), hubHost, enableHub)

	// Fallback to legacy environment variable
	if cfg.Storage.Bucket == "" && hostedMode {
		if val := os.Getenv("SCION_HUB_STORAGE_BUCKET"); val != "" {
			cfg.Storage.Bucket = val
			if cfg.Storage.Provider == "local" || cfg.Storage.Provider == "" {
				cfg.Storage.Provider = "gcs"
			}
		}
	}

	// Update local variables from cfg
	storageBucket = cfg.Storage.Bucket
	storageDir = cfg.Storage.LocalPath
	if storageBucket != "" && (cfg.Storage.Provider == "local" || cfg.Storage.Provider == "") {
		cfg.Storage.Provider = "gcs"
	}

	return cfg, nil
}

func hostedHAGuardsRequired(cfg *config.GlobalConfig) bool {
	return hostedMode && enableHub && cfg != nil && isHADeployment(cfg)
}

// isHADeployment returns true when the configuration indicates a multi-instance
// (Cloud Run / HA) deployment rather than a single-instance GCE VM.
func isHADeployment(cfg *config.GlobalConfig) bool {
	if cloudrunenv.IsService() {
		return true
	}
	if strings.EqualFold(cfg.Database.Driver, "postgres") {
		return true
	}
	if strings.EqualFold(cfg.Storage.Provider, "gcs") && cfg.Auth.Mode == "proxy" {
		return true
	}
	return false
}

// validateDebugEndpoints refuses --enable-debug-endpoints in hosted mode.
// Diagnostic endpoints are for local development only.
func validateDebugEndpoints(hosted, enabled bool) error {
	if hosted && enabled {
		return fmt.Errorf("--enable-debug-endpoints is not allowed in hosted mode; diagnostic endpoints are for local development only")
	}
	return nil
}

// validateHostedBasic runs lightweight checks that apply to all --hosted
// deployments (both single-instance VMs and Cloud Run HA). The session
// secret backs hub JWT signing and web sessions only, so a broker-only
// process (runtime-broker start) does not warn about it (#3605).
func validateHostedBasic(cfg *config.GlobalConfig) {
	if !hostedMode || cfg == nil || (!enableHub && !enableWeb) {
		return
	}
	if strings.TrimSpace(resolveSessionSecret()) == "" {
		log.Println("Warning: no session secret set; sessions will not survive restarts. Set --session-secret or SCION_SERVER_SESSION_SECRET for durable sessions.")
	}
}

// validateHostedHAPreflight fails startup closed when a hosted multi-instance
// deployment is configured in a way that is not HA-safe.  It has two parts:
// universal HA consistency checks that apply to every multi-instance
// deployment regardless of auth mode, and IAP checks that apply only when IAP
// is the auth frontend (auth.mode=proxy).
func validateHostedHAPreflight(cfg *config.GlobalConfig) error {
	if !hostedHAGuardsRequired(cfg) {
		return nil
	}

	// --- Universal HA consistency checks (all auth modes) ---

	// Require an explicitly configured hub_id so all instances share the same
	// GCS prefix, secret scopes, and DB lookups. Without this, each Cloud Run
	// instance derives a different hubId from its hostname, causing divergent
	// state (audit finding C1).
	if cfg.Hub.IsHubIDUnconfigured() {
		return fmt.Errorf("hosted HA deployment requires an explicit server.hub.hub_id; without it each instance derives a different ID from its hostname, causing divergent GCS prefixes and secret scopes")
	}

	if !strings.EqualFold(cfg.Database.Driver, "postgres") {
		return fmt.Errorf("hosted HA deployment requires server.database.driver=postgres; got %q (single-instance VM deployments do not require Postgres)", cfg.Database.Driver)
	}
	if strings.TrimSpace(cfg.Database.URL) == "" {
		return fmt.Errorf("hosted HA deployment requires server.database.url for Postgres")
	}

	if !strings.EqualFold(cfg.Storage.Provider, "gcs") || strings.TrimSpace(cfg.Storage.Bucket) == "" {
		return fmt.Errorf("hosted HA deployment requires server.storage.provider=gcs and server.storage.bucket; local storage is not HA-safe")
	}

	if strings.TrimSpace(resolveSessionSecret()) == "" {
		return fmt.Errorf("hosted HA deployment requires a durable session/signing secret; set --session-secret or SCION_SERVER_SESSION_SECRET")
	}

	// --- IAP-specific validation (proxy mode only) ---
	// When auth.mode is not "proxy", the hub handles authentication directly
	// (OAuth/OIDC). Cross-replica session consistency is provided by the
	// shared session secret (checked above) and stateless encrypted cookies.
	// No IAP infrastructure config is needed, so IAP is not required for HA.
	if cfg.Auth.Mode == "proxy" {
		if cfg.Auth.Proxy == nil || cfg.Auth.Proxy.Provider != "iap" {
			provider := ""
			if cfg.Auth.Proxy != nil {
				provider = cfg.Auth.Proxy.Provider
			}
			return fmt.Errorf("hosted HA deployment requires server.auth.proxy.provider=iap; got %q", provider)
		}
		if cfg.Auth.Proxy.IAP == nil || strings.TrimSpace(cfg.Auth.Proxy.IAP.Audience) == "" {
			return fmt.Errorf("hosted HA deployment requires server.auth.proxy.iap.audience")
		}
		proxyAudience := strings.TrimRight(strings.TrimSpace(cfg.Auth.Proxy.IAP.Audience), "/")
		if !isSupportedIAPAudience(proxyAudience) {
			return fmt.Errorf("hosted HA deployment requires a supported IAP audience: Cloud Run (/projects/<number>/locations/<region>/services/<service>) or GCLB (/projects/<number>/global/backendServices/<id>); got %q", proxyAudience)
		}
		// Normalize the audience in-place so downstream consumers (IAP JWT
		// validation, endpoint derivation) see the trimmed value.  Without this
		// a trailing slash would pass preflight but cause a runtime audience
		// mismatch in IAP token verification.
		cfg.Auth.Proxy.IAP.Audience = proxyAudience
		// The GKE + GCLB bootstrap flow deliberately allows a placeholder audience
		// on the first deploy, because the backend-service ID only exists once the
		// ingress has reconciled.  Preflight stays non-fatal for that case, but an
		// operator who never completes the follow-up upgrade would otherwise get a
		// hub that looks healthy and 401s every real request.
		if isLikelyPlaceholderAudience(proxyAudience) {
			log.Printf("WARNING: IAP audience %q looks like a bootstrap placeholder. "+
				"IAP token validation will FAIL on real requests until a valid "+
				"backend-service audience is configured. After your ingress "+
				"reconciles, update auth.proxy.iap.audience with the real "+
				"backend-service ID and restart.", proxyAudience)
		}

		if cfg.Auth.Transport == nil {
			return fmt.Errorf("hosted HA deployment requires server.auth.transport; do not use server.transport")
		}
		if cfg.Auth.Transport.Mode != "iap" {
			return fmt.Errorf("hosted HA deployment requires server.auth.transport.mode=iap; got %q", cfg.Auth.Transport.Mode)
		}
		transportAudience := strings.TrimRight(strings.TrimSpace(cfg.Auth.Transport.OIDCAudience), "/")
		if transportAudience == "" {
			return fmt.Errorf("hosted HA deployment requires server.auth.transport.oidc_audience")
		}
		// Normalize in-place so downstream consumers (token minting,
		// validation) see the trimmed value — matching the proxy audience
		// normalization above.
		cfg.Auth.Transport.OIDCAudience = transportAudience
		// Note: transport.oidc_audience and proxy.iap.audience are intentionally
		// allowed to differ. proxy.iap.audience is the IAP audience resource path
		// (Cloud Run or GCLB) used for validating incoming IAP-signed JWTs, while
		// transport.oidc_audience is the audience minted into OIDC tokens for
		// dispatched agents (typically the IAP OAuth client ID). IAP requires
		// the OAuth client ID format for token validation, not the resource path.
		if strings.TrimSpace(cfg.Auth.Transport.PlatformAuthSA) == "" {
			return fmt.Errorf("hosted HA deployment requires server.auth.transport.platform_auth_sa")
		}
	}

	return nil
}

// validateServerPreflight validates settings that matter only when this
// process runs the Hub, normalizing them in place on success. A broker-only
// process (enableHub == false) never installs a dispatcher and never reads
// these settings, so it skips the check rather than refusing to start over a
// value it will never use.
func validateServerPreflight(cfg *config.GlobalConfig) error {
	if !enableHub || cfg == nil {
		return nil
	}
	normalized, err := config.ValidateAgentEndpoint(cfg.Hub.AgentEndpoint)
	if err != nil {
		return err
	}
	cfg.Hub.AgentEndpoint = normalized
	if err := cfg.Hub.Conduit.Validate(); err != nil {
		return err
	}
	warnConduitTargetStreamDeadlines(cfg.Hub.Conduit)
	if err := cfg.Hub.PortProxy.Validate(); err != nil {
		return err
	}
	if err := cfg.Relay.Validate(); err != nil {
		return err
	}
	if err := validateConduitPeerSecret(cfg.Hub.Conduit, false, sessionSecretValue()); err != nil {
		return err
	}
	if _, err := hub.ParseAgentRunScope(cfg.Auth.AgentRunScope, cfg.Auth.AgentRunScopeLegacyUntil); err != nil {
		return err
	}
	return nil
}

// portProxyResponseHeaderTimeoutSetting returns the configured bound on
// the wait for an agent port's response headers (default 60s).
// validateServerPreflight has already rejected a malformed or
// out-of-range value.
func portProxyResponseHeaderTimeoutSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.PortProxy.ResponseHeaderTimeoutDuration()
	if err != nil {
		return config.PortProxyDefaultResponseHeaderTimeout
	}
	return d
}

// agentRunScopeSetting returns the parsed agent run-scope setting.
// validateServerPreflight has already rejected an invalid value, so an
// error here falls back to the default (off).
func agentRunScopeSetting(cfg *config.GlobalConfig) hub.AgentRunScope {
	s, err := hub.ParseAgentRunScope(cfg.Auth.AgentRunScope, cfg.Auth.AgentRunScopeLegacyUntil)
	if err != nil {
		return hub.AgentRunScope{}
	}
	return s
}

// isSupportedIAPAudience returns true when audience is a recognised IAP
// audience path.  Two formats are accepted:
//
//   - Cloud Run:  /projects/<number>/locations/<region>/services/<service>
//   - GCLB:       /projects/<number>/global/backendServices/<id>
//
// Everything else is rejected (fail-closed).
func isSupportedIAPAudience(audience string) bool {
	parts := strings.Split(strings.TrimSpace(audience), "/")
	// Cloud Run format: 7 parts ["", "projects", <n>, "locations", <r>, "services", <s>]
	if len(parts) == 7 &&
		parts[0] == "" &&
		parts[1] == "projects" && parts[2] != "" &&
		parts[3] == "locations" && parts[4] != "" &&
		parts[5] == "services" && parts[6] != "" {
		return true
	}
	// GCLB backend-service format: 6 parts ["", "projects", <n>, "global", "backendServices", <id>]
	if len(parts) == 6 &&
		parts[0] == "" &&
		parts[1] == "projects" && parts[2] != "" &&
		parts[3] == "global" &&
		parts[4] == "backendServices" && parts[5] != "" {
		return true
	}
	return false
}

// isLikelyPlaceholderAudience returns true when a GCLB audience looks
// synthetic — e.g. all-zero project numbers, backend-service ID "0", or other
// patterns that indicate a bootstrap placeholder rather than a real
// backend-service audience.
//
// Only the GCLB format is inspected.  Cloud Run audiences are knowable before
// the first deploy, so they never need a placeholder.
func isLikelyPlaceholderAudience(audience string) bool {
	parts := strings.Split(strings.TrimSpace(audience), "/")
	if len(parts) != 6 || parts[4] != "backendServices" {
		return false
	}
	projectNumber := parts[2]
	backendServiceID := parts[5]

	// Backend-service ID "0" (or any all-zeros string) is the canonical placeholder.
	if backendServiceID != "" && strings.Trim(backendServiceID, "0") == "" {
		return true
	}
	// An all-zeros project number (any length) is synthetic.
	if projectNumber != "" && strings.Trim(projectNumber, "0") == "" {
		return true
	}
	// Well-known dummy project numbers.
	if projectNumber == "123456789" {
		return true
	}
	return false
}

// checkServerPorts checks that required server ports are available.
func checkServerPorts(cfg *config.GlobalConfig) error {
	if enableHub && !enableWeb {
		status := checkPort(cfg.Hub.Host, cfg.Hub.Port)
		if status.inUse {
			if status.isScionServer {
				return fmt.Errorf("a scion server is already running on port %d\nUse 'scion server status' to check or 'scion server stop' to stop it", cfg.Hub.Port)
			}
			return fmt.Errorf("hub port %d is already in use by another process", cfg.Hub.Port)
		}
	}
	if cfg.RuntimeBroker.Enabled {
		if err := checkBrokerPort(cfg); err != nil {
			return err
		}
	}
	if enableWeb {
		webHost := cfg.Hub.Host
		if webHost == "" {
			webHost = "0.0.0.0"
		}
		status := checkPort(webHost, webPort)
		if status.inUse {
			if status.isScionServer {
				return fmt.Errorf("a scion server is already running on port %d\nUse 'scion server status' to check or 'scion server stop' to stop it", webPort)
			}
			return fmt.Errorf("web frontend port %d is already in use by another process", webPort)
		}
	}
	return nil
}

// checkBrokerPort checks that the runtime broker's port is free.
func checkBrokerPort(cfg *config.GlobalConfig) error {
	status := checkPort(cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)
	if status.inUse {
		if status.isScionServer {
			return fmt.Errorf("a scion server is already running on port %d\nUse 'scion server status' to check or 'scion server stop' to stop it", cfg.RuntimeBroker.Port)
		}
		return fmt.Errorf("runtime broker port %d is already in use by another process", cfg.RuntimeBroker.Port)
	}
	return nil
}

// initStore initializes the database store. The provided context is used for
// schema migration and the initial health-check ping so that a Ctrl+C during
// startup cancels those operations gracefully.
func initStore(ctx context.Context, cfg *config.GlobalConfig) (store.Store, *ent.Client, error) {
	connMaxLifetime, err := cfg.Database.ConnMaxLifetimeDuration()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database pool config: %w", err)
	}
	connMaxIdleTime, err := cfg.Database.ConnMaxIdleTimeDuration()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid database pool config: %w", err)
	}

	// The connection pool config is shared across backends. For SQLite,
	// MaxOpenConns is forced to 1 by applyDatabasePoolDefaults to serialize
	// writes; for Postgres it carries the larger pool sizing (default 10/5/30m
	// lifetime, 5m idle) since Postgres handles concurrent connections natively.
	pool := entc.PoolConfig{
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: connMaxLifetime,
		ConnMaxIdleTime: connMaxIdleTime,
	}

	var entClient *ent.Client
	switch cfg.Database.Driver {
	case "sqlite":
		// Migration α: upgrade a legacy raw-SQL hub.db (the former
		// pkg/store/sqlite schema) to the consolidated Ent schema before opening
		// it. Detection is conservative and the whole step is a no-op for an
		// already-Ent file, so it is safe to run on every boot.
		if err := maybeMigrateLegacySQLite(ctx, cfg.Database.URL); err != nil {
			return nil, nil, err
		}

		// All Hub state lives in a single Ent-backed SQLite database.
		// Guard against a double "file:" prefix when the operator already
		// supplies "file:/path/hub.db" in their config.
		sqliteDSN := cfg.Database.URL
		if !strings.HasPrefix(sqliteDSN, "file:") {
			sqliteDSN = "file:" + sqliteDSN
		}
		if !strings.Contains(sqliteDSN, "?") {
			sqliteDSN += "?cache=shared"
		} else if !strings.Contains(sqliteDSN, "cache=") {
			sqliteDSN += "&cache=shared"
		}
		entClient, err = entc.OpenSQLite(sqliteDSN, pool)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open database: %w", err)
		}
	case "postgres":
		// Postgres uses the pgx stdlib driver. The URL is a standard
		// connection string (e.g. "postgres://user:pass@host:5432/db?sslmode=require").
		entClient, err = entc.OpenPostgres(cfg.Database.URL, pool)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open database: %w", err)
		}
	default:
		return nil, nil, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}

	s := entadapter.NewCompositeStore(entClient)

	// Repair SQLite tables whose timestamps the driver cannot scan BEFORE
	// migrateStore: Migrate reads tables through ent and fails, fatally, on
	// such rows, so this cannot move into runBootDataMigrations. It uses raw
	// SQL only, snapshots the database before writing, and is a no-op on
	// Postgres and on a store with nothing to repair.
	snaps := &startupSnapshots{tsRepair: repairUnreadableTimestamps(ctx, s)}

	// On Postgres, refuse to migrate when the search_path would build a
	// fresh table set that shadows existing hub tables in a later schema,
	// unless server.database.allow_shadowed_schema is set. Only hub startup
	// enables this check.
	s.EnableSchemaShadowGuard(cfg.Database.AllowShadowedSchema)

	// On SQLite, snapshot the database when the migration (after
	// PreMigrate) is about to apply declared ON DELETE actions to orphaned
	// rows. A snapshot that is needed but fails stops startup before the
	// reconcile changes any rows. The snapshot's path is recorded so the
	// prune below keeps it.
	s.SetBeforeFKReconcile(func(ctx context.Context, db *sql.DB, groups []entc.FKRepairGroup) error {
		return snapshotBeforeFKReconcileRecord(ctx, db, groups, snaps.recordFK)
	})

	// Migrate runs Ent's schema migration and seeds built-in maintenance
	// operations (parity with the former raw-SQL store). After it succeeds,
	// the repair snapshots unused for 7 days are removed; see
	// migrateThenPrune.
	if err := migrateThenPrune(ctx, s, func(ctx context.Context) error { return migrateStore(ctx, s) }, snaps); err != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	runBootDataMigrations(ctx, s)

	if err := s.Ping(ctx); err != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("database ping failed: %w", err)
	}

	return s, entClient, nil
}

// migrateStore runs the schema migration and seed data. On Postgres it is
// serialized across Hub replicas by the store.LockSchemaMigration advisory
// lock; see CompositeStore.MigrateWithSchemaLock, which owns the locked path
// so its Postgres integration test exercises the same code
// (ptone/scion#1078).
func migrateStore(ctx context.Context, s *entadapter.CompositeStore) error {
	return s.MigrateWithSchemaLock(ctx)
}

// runWithAdvisoryLock runs fn under a TryAdvisoryLock if the store implements
// AdvisoryLocker. On non-Postgres backends (SQLite) or when the store is nil,
// fn runs unconditionally — single-writer backends don't need coordination.
// This is the generic helper for boot-time migrations that must not race
// across replicas (audit finding C6).
//
// When the lock is held by another replica, fn is silently skipped. This is
// safe because all guarded boot migrations are idempotent: each one checks
// whether its work has already been done (e.g. MigrateStorageOnFirstBoot
// checks for existing namespaced objects, BootstrapBundledResources uses
// per-resource OverwritePolicy checks, secretmigration.MigratePluginSecrets
// checks for existing secret values)
// and no-ops if so. The winning replica does the work; the others skip it
// here and will see the completed state on their next access. Exception:
// the workstation resource bootstrap imports per-replica disk content; see
// bootstrapWorkstationResources.
func runWithAdvisoryLock(ctx context.Context, s store.Store, key store.AdvisoryLockKey, label string, fn func()) {
	if s == nil {
		fn()
		return
	}
	locker, ok := s.(store.AdvisoryLocker)
	if !ok {
		fn()
		return
	}
	acquired, release, err := locker.TryAdvisoryLock(ctx, key)
	if err != nil {
		slog.Error("Failed to acquire advisory lock", "label", label, "error", err)
		return
	}
	if !acquired {
		// release is safe to call even when not acquired (per AdvisoryLocker
		// contract), but we call it here explicitly before returning.
		_ = release()
		slog.Info("Advisory lock held by another replica, skipping", "label", label)
		return
	}
	defer func() { _ = release() }()
	fn()
}

// workstationResourceBootstrapper is the subset of *hub.Server used by the
// workstation (non-hosted) resource bootstrap; an interface so the lock
// routing can be unit-tested without a full hub.
type workstationResourceBootstrapper interface {
	BootstrapTemplatesFromDir(ctx context.Context, dir string) error
	BootstrapHarnessConfigsFromDir(ctx context.Context, dir string) error
}

// bootstrapWorkstationResources imports templates and harness-configs from
// the local ~/.scion directories into the hub (non-hosted mode). Both imports
// run under one advisory lock (ptone/scion#1079): ResourceStore.Bootstrap is
// GetBySlug-then-Create with no ErrAlreadyExists recovery, so two replicas
// sharing a Postgres store would race. The key is shared with the hosted
// bundled-resource bootstrap; the two branches are mutually exclusive.
//
// Caveats:
//   - In non-hosted mode each replica imports its OWN ~/.scion, so when the
//     lock is held the loser skips and the winner's disk content defines the
//     hub records.
//   - A lock-acquire error (Postgres only, e.g. pool exhaustion) skips the
//     import for this boot, logged at ERROR by runWithAdvisoryLock. Before
//     the lock the import always ran; this matches the hosted branch.
//   - On SQLite the lock is a no-op and both imports always run.
func bootstrapWorkstationResources(ctx context.Context, s store.Store, b workstationResourceBootstrapper, globalDir string) {
	runWithAdvisoryLock(ctx, s, store.LockBundledResources, "workstation resource bootstrap", func() {
		globalTemplatesDir := filepath.Join(globalDir, "templates")
		if err := b.BootstrapTemplatesFromDir(ctx, globalTemplatesDir); err != nil {
			log.Printf("Warning: template bootstrap failed: %v", err)
		}
		globalHarnessConfigsDir := filepath.Join(globalDir, "harness-configs")
		if err := b.BootstrapHarnessConfigsFromDir(ctx, globalHarnessConfigsDir); err != nil {
			log.Printf("Warning: harness config bootstrap failed: %v", err)
		}
	})
}

// maybeMigrateLegacySQLite detects a legacy raw-SQL hub.db at path and, unless
// the operator opted out with --no-auto-migrate, upgrades it in-process to the
// consolidated Ent schema (after taking an automatic backup). It is a no-op when
// the file is already the Ent schema, empty, or absent. The provided context
// allows the migration to be cancelled (e.g. Ctrl+C during first boot).
func maybeMigrateLegacySQLite(ctx context.Context, path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	legacy, err := entc.IsLegacyRawSQLSchema(path)
	if err != nil {
		return fmt.Errorf("detecting database schema: %w", err)
	}
	if !legacy {
		return nil
	}

	if noAutoMigrate {
		// The operator opted out, but the file is a legacy schema the Ent store
		// cannot open. Fail loudly with guidance rather than crash later.
		return fmt.Errorf("detected a legacy raw-SQL hub database at %s but --no-auto-migrate is set; "+
			"remove the flag to upgrade it in place (a backup is taken automatically), "+
			"or point --db at an already-migrated database", path)
	}

	log.Printf("Detected legacy raw-SQL hub database at %s. Backing up and migrating to the Ent schema...", path)
	report, err := entc.MigrateAlphaSQLite(ctx, path, entc.AlphaOptions{
		Logf: func(format string, args ...any) { log.Printf(format, args...) },
	})
	if err != nil {
		return fmt.Errorf("migrating legacy database (original left untouched): %w", err)
	}
	if report.Skipped {
		return nil
	}
	log.Printf("Migration α complete: %d tables, %d rows migrated. Backup: %s",
		len(report.Tables), report.TotalRows(), report.BackupPath)
	return nil
}

// initDevAuth initializes dev authentication and returns the token.
func initDevAuth(cfg *config.GlobalConfig, globalDir string) (string, error) {
	devAuthCfg := apiclient.DevAuthConfig{
		Enabled:   cfg.Auth.Enabled,
		Token:     cfg.Auth.Token,
		TokenFile: devTokenFilePath(cfg.Auth.TokenFile, globalDir),
	}

	devAuthToken, err := apiclient.InitDevAuth(devAuthCfg, globalDir)
	if err != nil {
		return "", fmt.Errorf("failed to initialize dev auth: %w", err)
	}

	_ = os.Setenv("SCION_DEV_TOKEN", devAuthToken)
	_ = os.Setenv("SCION_AUTH_TOKEN", devAuthToken)

	if cfg.Auth.Token != "" {
		// A token set in the settings has no file, so there is no command
		// to print: say where it is set, never its value.
		log.Printf("Developer token: %s", devTokenStartupHint(cfg, globalDir))
		log.Printf("To authenticate CLI commands, export that same value as SCION_DEV_TOKEN.")
	} else {
		log.Printf("To authenticate CLI commands, run:")
		log.Printf("  %s", devTokenStartupHint(cfg, globalDir))
	}

	return devAuthToken, nil
}

// logWorkstationReadyBanner logs the workstation-mode startup banner. The
// dev token line shows its file path, not its value.
func logWorkstationReadyBanner(cfg *config.GlobalConfig, globalDir string, webEnabled bool, port int, devAuthToken string) {
	log.Println("Scion server ready (workstation mode)")
	if webEnabled {
		displayHost := cfg.Hub.Host
		if displayHost == "0.0.0.0" || displayHost == "" {
			displayHost = "127.0.0.1"
		}
		log.Printf("Web UI: http://%s:%d", displayHost, port)
	}
	if devAuthToken != "" {
		log.Printf("Developer token: %s", devTokenStartupHint(cfg, globalDir))
	}
}

// devTokenStartupHint returns what startup output shows for the dev token:
// the command that loads it from its file, so the output prints the dev
// token file path instead of its value. A token set directly in the server
// settings has no file, so the hint is a plain message naming where it is
// set (not a command).
func devTokenStartupHint(cfg *config.GlobalConfig, globalDir string) string {
	if cfg.Auth.Token != "" {
		return "set in the server settings (server.auth.dev_token, or the SCION_SERVER_AUTH_DEVTOKEN environment variable); the value is not printed"
	}
	return devTokenExportHint(devTokenFilePath(cfg.Auth.TokenFile, globalDir))
}

// devTokenFilePath returns the file the server keeps its dev token in: the
// configured token file, or <globalDir>/dev-token by default. A relative
// configured path is resolved against globalDir, so it names the same file
// however the server was started (foreground, background or --global) and
// whatever the working directory is.
func devTokenFilePath(configured, globalDir string) string {
	if configured == "" {
		return filepath.Join(globalDir, "dev-token")
	}
	if !filepath.IsAbs(configured) {
		return filepath.Join(globalDir, configured)
	}
	return configured
}

// devTokenExportHint returns the shell command that exports the dev token
// stored at path. The path is single-quoted, so it may contain spaces or
// shell metacharacters. The command substitution needs no outer double
// quotes (an assignment is not field-split), and leaving them out keeps the
// line free of characters the server's JSON log encoding would escape, so
// it can be copied from the log as is.
func devTokenExportHint(path string) string {
	return "export SCION_DEV_TOKEN=$(cat " + shellSingleQuote(path) + ")"
}

// shellSingleQuote quotes s for a POSIX shell: it is wrapped in single
// quotes, and each single quote inside it is written as an escaped quote
// between two quoted parts.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// loadServerSettings loads the settings the server's hub and broker use
// from the global settings directory only: the server never merges the
// settings of a project its working directory happens to be in. It falls
// back to empty settings (with a warning) when they fail to load. It also
// returns the default profile the broker reports on every heartbeat: the
// settings' active profile, or nil when they failed to load, so the
// heartbeat omits it and the hub keeps its value.
func loadServerSettings() (*config.Settings, *string) {
	var settings *config.Settings
	globalDir, err := config.GetGlobalDir()
	if err == nil {
		settings, err = config.LoadSettings(globalDir)
	}
	loaded := err == nil
	if err != nil {
		log.Printf("Warning: failed to load settings: %v", err)
		settings = &config.Settings{}
	}
	return settings, brokerHeartbeatDefaultProfile(settings, loaded)
}

// hubEndpointSource identifies which input resolveHubEndpointWithSource
// took the hub's public endpoint from. Callers that need to know whether
// the URL is really served by this host (rather than derived from IAP
// configuration) branch on it; the URL itself is the same either way.
type hubEndpointSource string

const (
	hubEndpointSourceNone           hubEndpointSource = ""
	hubEndpointSourceConfig         hubEndpointSource = "config"
	hubEndpointSourceBrokerSettings hubEndpointSource = "broker-settings"
	hubEndpointSourceBaseURLFlag    hubEndpointSource = "base-url-flag"
	hubEndpointSourceBaseURLEnv     hubEndpointSource = "base-url-env"
	hubEndpointSourceSettings       hubEndpointSource = "settings"
	// hubEndpointSourceIAPAudience means the URL was derived from the IAP
	// audience (a Cloud Run IAP front end). This host does not serve it,
	// so colocated agents must not be routed to it.
	hubEndpointSourceIAPAudience hubEndpointSource = "iap-audience"
	hubEndpointSourceLocalhost   hubEndpointSource = "localhost"
)

// operatorHubEndpoint is the hub's operator endpoint, as resolved by
// resolveHubEndpointWithSource, and where it came from. The settings
// database is never consulted for it (ptone/scion#3905).
type operatorHubEndpoint struct {
	URL    string
	Source hubEndpointSource
}

// sourceLabel names the source of the operator endpoint for logging and the
// admin settings view: "env", "flag" or "file" for an explicitly configured
// value, otherwise the fallback that produced it.
func (e operatorHubEndpoint) sourceLabel() string {
	switch e.Source {
	case hubEndpointSourceConfig:
		if os.Getenv(hubEndpointEnvVar) != "" {
			return "env"
		}
		return "file"
	case hubEndpointSourceBaseURLFlag:
		return "flag"
	case hubEndpointSourceBaseURLEnv:
		return "env"
	case hubEndpointSourceBrokerSettings, hubEndpointSourceSettings:
		return "settings"
	default:
		return string(e.Source)
	}
}

// hubEndpointEnvVar is the environment variable that sets
// server.hub.public_url (GlobalConfig hub.endpoint).
const hubEndpointEnvVar = "SCION_SERVER_HUB_ENDPOINT"

// hubEndpointPin returns the operator setting that pins the hub's public
// endpoint, or "" when none is set. Any of --base-url,
// SCION_SERVER_HUB_ENDPOINT and SCION_SERVER_BASE_URL pins it: the hub then
// uses its operator endpoint and ignores a server.hub.public_url saved in
// the settings database (ptone/scion#3905). With several set, it names the
// one resolveHubEndpointFrom takes the endpoint from (ptone/scion#5362).
func hubEndpointPin() string {
	switch {
	case webBaseURL != "":
		return "--base-url"
	case os.Getenv(hubEndpointEnvVar) != "":
		return hubEndpointEnvVar
	case os.Getenv("SCION_SERVER_BASE_URL") != "":
		return "SCION_SERVER_BASE_URL"
	}
	return ""
}

// resolveHubEndpoints reads the server.hub.public_url saved in the settings
// database (hub processes only) and resolves the hub's endpoints from it and
// the operator endpoint (hub.ResolveHubEndpoints), logging the result.
func resolveHubEndpoints(ctx context.Context, s store.Store, operator operatorHubEndpoint) (hub.HubEndpoints, error) {
	base := hub.HubEndpoints{
		Operator:       operator.URL,
		OperatorSource: operator.sourceLabel(),
		Pin:            hubEndpointPin(),
	}
	if !enableHub {
		base.Public, base.PublicSource = base.Operator, base.OperatorSource
		return base, nil
	}
	var dbValue string
	if st, ok := s.(store.HubSettingStore); ok {
		v, err := hub.StoredPublicURL(ctx, st)
		if err != nil {
			return hub.HubEndpoints{}, err
		}
		dbValue = v
	}
	return hub.ResolveHubEndpoints(base, dbValue, slog.Default()), nil
}

// effectiveHubEndpoint returns the hub endpoint in effect for the
// derivations made in this package (hub.HubEndpointForAgents decides), and
// the source to report for it. A value that differs from the operator
// endpoint came from the settings database and counts as explicitly
// configured.
func effectiveHubEndpoint(e hub.HubEndpoints, operatorSrc hubEndpointSource) (string, hubEndpointSource) {
	ep := e.For(hub.HubEndpointForAgents)
	if ep == e.Operator {
		return ep, operatorSrc
	}
	return ep, hubEndpointSourceConfig
}

// resolveHubEndpoint determines the Hub's public endpoint URL.
func resolveHubEndpoint(cfg *config.GlobalConfig, brokerSettings *config.Settings) string {
	endpoint, _ := resolveHubEndpointWithSource(cfg, brokerSettings)
	return endpoint
}

// resolveHubEndpointWithSource determines the Hub's public endpoint URL and
// reports which input it came from.
func resolveHubEndpointWithSource(cfg *config.GlobalConfig, brokerSettings *config.Settings) (string, hubEndpointSource) {
	return resolveHubEndpointFrom(cfg, currentServerEndpointMode(), brokerSettings.GetHubEndpoint(), log.Printf)
}

// serverEndpointMode is the part of the server's mode that decides how its
// hub endpoints resolve. currentServerEndpointMode fills it from the
// 'server start' flags; 'scion doctor' builds it without them.
type serverEndpointMode struct {
	hubEnabled    bool
	brokerEnabled bool
	webEnabled    bool
	hosted        bool
	webPort       int
	baseURLFlag   string // --base-url
	debug         bool
}

// currentServerEndpointMode returns the mode of the running 'server start'.
func currentServerEndpointMode() serverEndpointMode {
	return serverEndpointMode{
		hubEnabled:    enableHub,
		brokerEnabled: enableRuntimeBroker,
		webEnabled:    enableWeb,
		hosted:        hostedMode,
		webPort:       webPort,
		baseURLFlag:   webBaseURL,
		debug:         enableDebug,
	}
}

// hubListenPort is the port the hub is served on in mode m: the web port
// when the web frontend is enabled, else the hub's own port, and 0 when the
// hub is not enabled. See resolveHubListenPort.
func (m serverEndpointMode) hubListenPort(cfg *config.GlobalConfig) int {
	if !m.hubEnabled {
		return 0
	}
	if m.webEnabled {
		return m.webPort
	}
	return cfg.Hub.Port
}

// resolveHubEndpointFrom is resolveHubEndpointWithSource for an explicit
// mode. legacyEndpoint is the settings hub.endpoint (which koanf also fills
// from SCION_HUB_ENDPOINT); passing "" resolves as if that fallback were
// gone. logf receives the resolution log lines; nil discards them.
//
// With the hub enabled the order is flag, then environment, then file
// (ptone/scion#5362): --base-url, SCION_SERVER_HUB_ENDPOINT,
// SCION_SERVER_BASE_URL, server.hub.public_url from settings.yaml, then the
// fallbacks. cfg.Hub.Endpoint holds SCION_SERVER_HUB_ENDPOINT when it is set
// (applied over the file value when the config loads), so the environment
// check reads the variable and takes the value from cfg. A server that runs
// only the Runtime Broker takes cfg.Hub.Endpoint, then legacyEndpoint.
func resolveHubEndpointFrom(cfg *config.GlobalConfig, m serverEndpointMode, legacyEndpoint string, logf func(string, ...any)) (string, hubEndpointSource) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if !m.hubEnabled {
		if cfg.Hub.Endpoint != "" {
			return cfg.Hub.Endpoint, hubEndpointSourceConfig
		}
		hubEndpoint := legacyEndpoint
		if hubEndpoint != "" && m.debug {
			logf("Hub endpoint resolved from settings: %s", hubEndpoint)
		}
		if hubEndpoint == "" {
			return "", hubEndpointSourceNone
		}
		return hubEndpoint, hubEndpointSourceBrokerSettings
	}

	if m.baseURLFlag != "" {
		hubEndpoint := strings.TrimRight(m.baseURLFlag, "/")
		if m.debug {
			logf("Hub endpoint resolved from --base-url flag: %s", hubEndpoint)
		}
		return hubEndpoint, hubEndpointSourceBaseURLFlag
	}

	if os.Getenv(hubEndpointEnvVar) != "" && cfg.Hub.Endpoint != "" {
		return cfg.Hub.Endpoint, hubEndpointSourceConfig
	}

	if baseURL := os.Getenv("SCION_SERVER_BASE_URL"); baseURL != "" {
		hubEndpoint := strings.TrimRight(baseURL, "/")
		if m.debug {
			logf("Hub endpoint resolved from SCION_SERVER_BASE_URL: %s", hubEndpoint)
		}
		return hubEndpoint, hubEndpointSourceBaseURLEnv
	}

	if cfg.Hub.Endpoint != "" {
		return cfg.Hub.Endpoint, hubEndpointSourceConfig
	}

	// Check settings (e.g. SCION_HUB_ENDPOINT env var) before falling back
	// to localhost. On combo servers the settings-level endpoint is typically
	// the public URL and should be used for agent dispatch.
	if hubEndpoint := legacyEndpoint; hubEndpoint != "" {
		if m.debug {
			logf("Hub endpoint resolved from settings (SCION_HUB_ENDPOINT): %s", hubEndpoint)
		}
		return hubEndpoint, hubEndpointSourceSettings
	}

	// In hosted mode with IAP authentication, derive the Cloud Run URL from
	// the IAP audience. This prevents the localhost:8080 fallback which is
	// unreachable from GKE-dispatched agents. GCLB/GKE audiences carry no
	// routing information and cannot be converted to a URL, so they fall
	// through to the HA warning below.
	if m.hosted && cfg.Auth.Proxy != nil && cfg.Auth.Proxy.IAP != nil && cfg.Auth.Proxy.IAP.Audience != "" {
		if cloudRunURL := iapAudienceToCloudRunURL(cfg.Auth.Proxy.IAP.Audience); cloudRunURL != "" {
			logf("Hub endpoint derived from IAP audience: %s", cloudRunURL)
			return cloudRunURL, hubEndpointSourceIAPAudience
		}
	}

	hubEndpoint := fmt.Sprintf("http://localhost:%d", m.hubListenPort(cfg))
	// Any hosted multi-instance deployment reaching this fallback has no
	// usable public URL: dispatched agents would resolve the loopback address
	// inside their own container. This is not specific to IAP — a GCLB
	// audience cannot be converted to a URL, and a hub doing its own
	// OAuth/OIDC has no audience to derive one from at all.
	if m.hosted && m.hubEnabled && isHADeployment(cfg) {
		logf("Warning: hosted HA deployment has no explicit hub base URL; falling back to %s, which is unreachable from dispatched agents. Set SCION_SERVER_BASE_URL or server.hub.public_url.", hubEndpoint)
	}
	if m.debug {
		logf("Auto-computed hub endpoint for combo mode: %s", hubEndpoint)
	}
	return hubEndpoint, hubEndpointSourceLocalhost
}

// iapAudienceToCloudRunURL converts a Cloud Run native IAP audience path
// (/projects/<number>/locations/<region>/services/<service>) to the
// corresponding Cloud Run service URL (https://<service>-<number>.<region>.run.app).
//
// NOTE: This produces legacy-format Cloud Run URLs (<service>-<number>.<region>.run.app).
// Newer Cloud Run services use the format <service>-<hash>-<region>.a.run.app where
// the hash cannot be derived from the project number. For those services, set
// SCION_SERVER_BASE_URL explicitly instead of relying on this derivation.
func iapAudienceToCloudRunURL(audience string) string {
	// Expected format: /projects/<project-number>/locations/<region>/services/<service>
	parts := strings.Split(strings.TrimRight(strings.TrimSpace(audience), "/"), "/")
	if len(parts) != 7 || parts[0] != "" || parts[1] != "projects" || parts[3] != "locations" || parts[5] != "services" {
		return ""
	}
	projectNumber := parts[2]
	region := parts[4]
	service := parts[6]
	if projectNumber == "" || region == "" || service == "" {
		return ""
	}
	return fmt.Sprintf("https://%s-%s.%s.run.app", service, projectNumber, region)
}

// parseAdminEmails parses admin emails from the flag or config.
func parseAdminEmails(cfg *config.GlobalConfig) []string {
	adminEmailList := splitCommaList(adminEmails)
	if len(adminEmailList) == 0 && len(cfg.Hub.AdminEmails) > 0 {
		adminEmailList = cfg.Hub.AdminEmails
	}
	// D11-fix: normalize (TrimSpace + ToLower, drop empties) so matchers
	// compare against the same normalization the user store applies.
	adminEmailList = config.SanitizeEmailList(adminEmailList)
	if len(adminEmailList) > 0 {
		log.Printf("Admin emails configured: %v", adminEmailList)
	}
	return adminEmailList
}

// resolveHubIDFromEnv resolves the hub instance ID from environment variables,
// falling back to the environment-aware default (K_SERVICE on Cloud Run,
// persisted hostname-derived ID on workstations). This is used during early
// logging init before the full config is loaded.
func resolveHubIDFromEnv() string {
	return config.ResolveHubIDFromEnv()
}

// warnNonConformingHubName logs a warning when a configured hub_name does
// not match the settings schema pattern. It is not fatal: the name still
// loads, but the admin server-config API rejects such a value if an admin
// tries to set it (an unchanged echo is accepted). It reports whether it
// warned.
func warnNonConformingHubName(name string) bool {
	if name == "" || config.HubNameMatchesSchema(name) {
		return false
	}
	slog.Warn("server.hub.hub_name does not match the settings schema pattern; it is used as is, but cannot be set to this value through the admin server-config API",
		"hub_name", name, "pattern", config.HubNamePattern)
	return true
}

// resolveHubNameFromEnv resolves the hub display name from environment variables,
// falling back to os.Hostname(). This is used during early logging init before
// the full config is loaded. SCION_SERVER_HUB_HUBNAME (the standard koanf-derived
// name) takes precedence; SCION_HUB_NAME is accepted as a shorter fallback for
// simple setups.
func resolveHubNameFromEnv() string {
	if v := os.Getenv("SCION_SERVER_HUB_HUBNAME"); v != "" {
		return v
	}
	return config.ResolveHubNameOrDefault(os.Getenv("SCION_HUB_NAME"))
}

// resolveSessionSecret resolves the deployment-wide session secret from the
// --session-secret flag, falling back to the value scrubServerSecretEnv read
// at startup, then to the SCION_SERVER_SESSION_SECRET env var (then the
// deprecated SESSION_SECRET). The same value backs both the web session cookie
// store and the hub JWT signing keys so that all replicas behind the load
// balancer agree.
func resolveSessionSecret() string {
	secret := webSessionSecret
	if secret == "" {
		secret = capturedSessionSecret
	}
	if secret == "" {
		secret = sessionSecretFromEnv()
	}
	if secret == "" && hostedMode {
		slog.Warn("No session secret configured in hosted mode! Replicas will not be able to share sessions or agree on JWT signing keys, leading to login loops.")
	}
	return secret
}

// sessionSecretValue is the configured session secret, or "": the
// --session-secret flag, else the value scrubServerSecretEnv read at
// startup, else SCION_SERVER_SESSION_SECRET, then SESSION_SECRET. It only
// reads: no deprecation or hosted-mode warning (resolveSessionSecret adds
// the hosted-mode one).
func sessionSecretValue() string {
	if webSessionSecret != "" {
		return webSessionSecret
	}
	if capturedSessionSecret != "" {
		return capturedSessionSecret
	}
	if s := os.Getenv(sessionSecretEnv); s != "" {
		return s
	}
	return os.Getenv(legacySessionSecretEnv)
}

// hubObservabilityExport decides at startup whether the hub's own metrics
// and trace exporters run, and their export project (ptone/scion#4914).
// See decideHubObservabilityExport for the inputs.
func hubObservabilityExport(cfg *config.GlobalConfig, hubSrv *hub.Server) hubexport.Status {
	return decideHubObservabilityExport(cfg, hubSrv.GetOperationalSettings(), hubObservabilityInputs{
		getenv: os.Getenv,
		// Relies on the earlier Policy Troubleshooter probe in
		// initHubServer (ResolveGCPProjectID) for caching: the metadata
		// client caches OnGCE and a resolved project ID.
		metadata:   func() (string, error) { return hub.ResolveGCPProjectID("") },
		tracingEnv: parseBoolEnv(hubexport.TracingEnv),
	})
}

// hubObservabilityInputs are the process inputs of
// decideHubObservabilityExport, passed in so the selection is testable.
type hubObservabilityInputs struct {
	getenv     func(string) string
	metadata   func() (string, error)
	tracingEnv bool
}

// decideHubObservabilityExport selects the inputs of the export decision.
// The switches come from the Layer-1 snapshot, where a managed DB row wins
// over the settings file and the environment; a hub without the
// operational settings service (ops nil) reads them from the loaded
// config. The export project is server.hub.gcp_project_id, else the
// metadata-server project, resolved as for service-account minting
// (ResolveGCPProjectID, which skips the lookup off GCP).
func decideHubObservabilityExport(cfg *config.GlobalConfig, ops *hub.OperationalSettings, in hubObservabilityInputs) hubexport.Status {
	metrics, tracing := cfg.Hub.Observability.MetricsEnabled, cfg.Hub.Observability.TracingEnabled
	if ops != nil {
		snap := ops.Snapshot()
		metrics, tracing = snap.HubMetricsEnabled, snap.HubTracingEnabled
	}
	project := hubexport.ResolveProject(hubexport.ProjectInputs{
		Configured: cfg.Hub.GCPProjectID,
		EnvValue:   in.getenv(hubexport.ProjectIDEnv),
		Metadata:   in.metadata,
	})
	return hubexport.Status{
		Project: project,
		Metrics: hubexport.DecideMetrics(metrics, project),
		Tracing: hubexport.DecideTracing(tracing, in.tracingEnv, project),
	}
}

// parseBoolEnv reports whether the named environment variable is set to a
// truthy value, using util.LookupBoolEnv for the accepted spellings
// (whitespace-trimmed, case-insensitive strconv.ParseBool plus yes/y/on and
// no/n/off). Unset, empty, and unparseable values are false, but an
// unparseable non-empty value also logs a warning so a typo does not silently
// disable a feature the operator meant to turn on.
//
// The warning uses the stdlib logger because parseBoolEnv runs during
// initServerLogging, before the slog loggers are wired.
func parseBoolEnv(key string) bool {
	if v, ok := util.LookupBoolEnv(key); ok {
		return v
	}
	// ok is false for both unset/empty and garbage; warn only on garbage.
	if raw := os.Getenv(key); strings.TrimSpace(raw) != "" {
		log.Printf("WARNING: environment variable %s=%q is not a recognized boolean value; treating as false. "+
			"Accepted truthy values: true, 1, t, yes, y, on (case-insensitive, whitespace-trimmed).", key, raw)
	}
	return false
}

// newJWTProxyAuthenticator builds a hub.JWTProxyAuthenticator from
// auth.proxy.jwt config, shared by the hub and web server init paths so both
// authenticate identically (mirrors the IAP construction pattern).
//
// Validation happens eagerly here — at server startup — rather than being
// deferred to the first request, so a misconfigured algorithm, missing key
// source, or unreadable key file is reported immediately (load-bearing
// decision from the design doc).
func newJWTProxyAuthenticator(jwtCfg *config.JWTAuthConfig) (*hub.JWTProxyAuthenticator, error) {
	if jwtCfg == nil {
		return nil, fmt.Errorf("auth.proxy.jwt config required when provider=jwt")
	}
	if jwtCfg.Algorithm == "" {
		return nil, fmt.Errorf("auth.proxy.jwt.algorithm is required")
	}
	if !hub.IsAsymmetricJWTAlgorithm(jwtCfg.Algorithm) {
		return nil, fmt.Errorf("auth.proxy.jwt.algorithm %q is not a supported asymmetric algorithm (expected one of RS256, RS384, RS512, ES256, ES384, ES512, PS256, PS384, PS512)", jwtCfg.Algorithm)
	}

	claims := hub.JWTClaimMapping{}
	if jwtCfg.Claims != nil {
		claims = hub.JWTClaimMapping{
			Email:       jwtCfg.Claims.Email,
			Subject:     jwtCfg.Claims.Subject,
			DisplayName: jwtCfg.Claims.DisplayName,
			Domain:      jwtCfg.Claims.Domain,
		}
	}

	// Exactly one key source must be configured.
	switch {
	case jwtCfg.PublicKeyFile != "" && jwtCfg.JWKSURL == "" && jwtCfg.JWKSFile == "":
		keySource, err := hub.NewStaticJWTKeySource(jwtCfg.PublicKeyFile)
		if err != nil {
			return nil, fmt.Errorf("auth.proxy.jwt.publicKeyFile: %w", err)
		}
		return &hub.JWTProxyAuthenticator{
			Header:    jwtCfg.Header,
			Algorithm: jwtCfg.Algorithm,
			Issuer:    jwtCfg.Issuer,
			Audience:  jwtCfg.Audience,
			Claims:    claims,
			KeySource: keySource,
		}, nil
	case jwtCfg.JWKSURL != "" && jwtCfg.PublicKeyFile == "" && jwtCfg.JWKSFile == "":
		return &hub.JWTProxyAuthenticator{
			Header:    jwtCfg.Header,
			Algorithm: jwtCfg.Algorithm,
			Issuer:    jwtCfg.Issuer,
			Audience:  jwtCfg.Audience,
			Claims:    claims,
			KeySource: hub.NewJWKSURLKeySource(jwtCfg.JWKSURL),
		}, nil
	case jwtCfg.JWKSFile != "" && jwtCfg.PublicKeyFile == "" && jwtCfg.JWKSURL == "":
		keySource, err := hub.NewJWKSFileKeySource(jwtCfg.JWKSFile)
		if err != nil {
			return nil, fmt.Errorf("auth.proxy.jwt.jwksFile: %w", err)
		}
		return &hub.JWTProxyAuthenticator{
			Header:    jwtCfg.Header,
			Algorithm: jwtCfg.Algorithm,
			Issuer:    jwtCfg.Issuer,
			Audience:  jwtCfg.Audience,
			Claims:    claims,
			KeySource: keySource,
		}, nil
	default:
		return nil, fmt.Errorf("auth.proxy.jwt: exactly one key source must be configured (publicKeyFile, jwksURL, or jwksFile)")
	}
}

// initHubServer creates and configures the Hub server.
// buildHubServerConfig builds the static portion of hub.ServerConfig from
// GlobalConfig and the caller's already-resolved values. It is a pure,
// side-effect-free copy — no I/O, no error return — so a config-copying
// regression (e.g. dropping a field like AgentEndpoint) shows up in a direct
// unit test instead of requiring a running Hub to notice the setting is
// silently ignored.
func buildHubServerConfig(cfg *config.GlobalConfig, hubEndpoint, devAuthToken string, adminEmailList []string, adminMode bool, maintenanceMessage string, secretBackend secret.SecretBackend) hub.ServerConfig {
	return hub.ServerConfig{
		HubID:                        cfg.Hub.ResolveHubID(),
		HubIDPinned:                  hubIDPinned(cfg),
		HubName:                      cfg.Hub.ResolveHubName(),
		DisableLegacyStorageFallback: cfg.Hub.DisableLegacyStorageFallback,
		Port:                         cfg.Hub.Port,
		Host:                         cfg.Hub.Host,
		ReadTimeout:                  cfg.Hub.ReadTimeout,
		WriteTimeout:                 cfg.Hub.WriteTimeout,
		CORSEnabled:                  cfg.Hub.CORSEnabled,
		CORSAllowedOrigins:           cfg.Hub.CORSAllowedOrigins,
		CORSAllowedMethods:           cfg.Hub.CORSAllowedMethods,
		CORSAllowedHeaders:           cfg.Hub.CORSAllowedHeaders,
		CORSMaxAge:                   cfg.Hub.CORSMaxAge,
		AuthMode:                     cfg.Auth.Mode,
		DevAuthToken:                 devAuthToken,
		Debug:                        enableDebug,
		AuthorizedDomains:            cfg.Auth.AuthorizedDomains,
		AdminEmails:                  adminEmailList,
		UserAccessMode:               cfg.Auth.UserAccessMode,
		DefaultUserRole:              cfg.Auth.DefaultUserRole,
		HubEndpoint:                  hubEndpoint,
		AgentEndpoint:                cfg.Hub.AgentEndpoint,
		SlowRequestThreshold:         cfg.SlowRequestThreshold,
		StalledThreshold:             cfg.Hub.StalledThreshold,
		MissingAgentGrace:            cfg.Hub.MissingAgentGrace,
		SoftDeleteRetention:          cfg.Hub.SoftDeleteRetention,
		SoftDeleteRetainFiles:        cfg.Hub.SoftDeleteRetainFiles,
		PerfTrace:                    cfg.Hub.PerfTrace,
		MembershipSweepReportOnly:    cfg.Hub.MembershipSweepReportOnly,
		LaunchTimeout:                cfg.Hub.LaunchTimeout,
		LaunchKeepaliveSeconds:       cfg.Hub.LaunchKeepaliveSeconds,
		ConduitTCPAllowedPorts:       append([]int(nil), cfg.Hub.Conduit.TCPAllowedPorts...),
		ConduitGrantKeyActivation:    conduitGrantKeyActivationSetting(cfg),
		ConduitAuthzRecheckInterval:  conduitAuthzRecheckIntervalSetting(cfg),
		ConduitUserStreamAuthzMax:    conduitUserStreamAuthzMaxSetting(cfg),
		ConduitProxySessionMaxAge:    conduitProxySessionMaxAgeSetting(cfg),
		ConduitTargetStreamDeadlines: cfg.Hub.Conduit.TargetStreamDeadlinesValue(),
		AgentRunScope:                agentRunScopeSetting(cfg),
		AdminMode:                    adminMode,
		MaintenanceMessage:           maintenanceMessage,
		SchedulerIntervalSeconds:     cfg.Scheduler.IntervalSeconds,
		SchedulerMaxConcurrency:      cfg.Scheduler.MaxConcurrency, // *int: nil = use default, *0 = unlimited
		Workstation:                  !hostedMode,
		ConfigPath:                   serverConfigPath,
		StartClaim: hub.StartClaimSettings{
			LeaseTTL:              cfg.Hub.StartClaimLeaseTTL,
			MaxDuration:           cfg.Hub.StartMaxDuration,
			UnconfirmedHold:       cfg.Hub.StartUnconfirmedHold,
			CreateUnconfirmedHold: cfg.Hub.StartCreateUnconfirmedHold,
		},
		DevUserConfig: hub.DevUserConfig{
			Username:    cfg.Auth.Username,
			DisplayName: cfg.Auth.DisplayName,
			Email:       cfg.Auth.Email,
		},
		TelemetryDefault: cfg.TelemetryEnabled,
		TelemetryConfig:  config.ConvertV1TelemetryToAPI(cfg.TelemetryConfig),
		BrokerAuthConfig: hub.DefaultBrokerAuthConfig(),
		GitHubAppConfig: hub.GitHubAppServerConfig{
			AppID:           cfg.GitHubApp.AppID,
			PrivateKeyPath:  cfg.GitHubApp.PrivateKeyPath,
			PrivateKey:      cfg.GitHubApp.PrivateKey,
			WebhookSecret:   cfg.GitHubApp.WebhookSecret,
			APIBaseURL:      cfg.GitHubApp.APIBaseURL,
			WebhooksEnabled: cfg.GitHubApp.WebhooksEnabled,
			InstallationURL: cfg.GitHubApp.InstallationURL,
		},
		OAuthConfig: hub.OAuthConfig{
			Web: hub.OAuthClientConfig{
				Google: hub.OAuthProviderConfig{
					ClientID:     cfg.OAuth.Web.Google.ClientID,
					ClientSecret: cfg.OAuth.Web.Google.ClientSecret,
				},
				GitHub: hub.OAuthProviderConfig{
					ClientID:     cfg.OAuth.Web.GitHub.ClientID,
					ClientSecret: cfg.OAuth.Web.GitHub.ClientSecret,
				},
			},
			CLI: hub.OAuthClientConfig{
				Google: hub.OAuthProviderConfig{
					ClientID:     cfg.OAuth.CLI.Google.ClientID,
					ClientSecret: cfg.OAuth.CLI.Google.ClientSecret,
				},
				GitHub: hub.OAuthProviderConfig{
					ClientID:     cfg.OAuth.CLI.GitHub.ClientID,
					ClientSecret: cfg.OAuth.CLI.GitHub.ClientSecret,
				},
			},
			Device: hub.OAuthClientConfig{
				Google: hub.OAuthProviderConfig{
					ClientID:     cfg.OAuth.Device.Google.ClientID,
					ClientSecret: cfg.OAuth.Device.Google.ClientSecret,
				},
				GitHub: hub.OAuthProviderConfig{
					ClientID:     cfg.OAuth.Device.GitHub.ClientID,
					ClientSecret: cfg.OAuth.Device.GitHub.ClientSecret,
				},
			},
		},
		MaintenanceConfig:       resolveMaintenanceConfig(cfg),
		NodeImageRegistry:       config.ImageRegistryFromEnv(),
		SecretBackend:           secretBackend,
		GCPIAMCheckMode:         cfg.Hub.GCPIAMCheckMode,
		GCPIAMDenyUnknownPolicy: cfg.Hub.GCPIAMDenyUnknownPolicy,
		GCPProjectID:            cfg.Hub.GCPProjectID,
		// Startup value for a hub without OperationalSettings; with them,
		// ApplySnapshot replaces it from the endpoints section.
		MonitoringDashboardURL: config.MonitoringDashboardURLOrEmpty(cfg.Hub.MonitoringDashboardURL),
		// Derive the agent/user JWT signing keys from the same shared session
		// secret the web cookie store uses, so every replica behind the load
		// balancer agrees on the signing key regardless of its host-derived
		// HubID. Without this, a JWT minted by one replica fails validation on
		// another (cross-replica "session_expired" login loop).
		SharedSigningSecret: resolveSessionSecret(),
		// The web session cookie key comes from the same secret
		// (initWebServer); its source label feeds the test admin gate (G7).
		WebSessionKeySource: webSessionKeySource(resolveSessionSecret()),
		// When SCION_REQUIRE_STABLE_SIGNING_KEY is truthy, the hub refuses to
		// start rather than silently mint a new signing key it cannot resolve
		// (which would invalidate every live token after, e.g., a redeploy onto a
		// new host that changed the HubID). Operators enabling this must supply a
		// session secret or pre-provision the signing keys.
		RequireStableSigningKey: parseBoolEnv("SCION_REQUIRE_STABLE_SIGNING_KEY"),
		OIDCLogin:               cfg.OIDCLogin,
		OIDCConfig:              cfg.OIDC,
		Federation:              cfg.Federation,
		WorkspaceStorageConfig:  cfg.WorkspaceStorage,
		// nil (no server.native_chat section) means enabled — chat is default-on.
		NativeChatEnabled: cfg.NativeChat.EnabledSetting(),
		// server.hub.port_proxy.response_header_timeout (validated at startup).
		PortProxyResponseHeaderTimeout: portProxyResponseHeaderTimeoutSetting(cfg),
	}
}

// resolveTransportAudience returns the effective OIDC audience for
// auth.transport: the explicitly configured audience if set, otherwise the
// public hub endpoint when mode is cloudrun_invoker. hubEndpoint must be the
// resolved public endpoint (never server.hub.agent_endpoint): Cloud Run's own
// IAM check validates minted tokens against the Hub's public URL, and the
// agent-only override must not change what that check expects.
func resolveTransportAudience(oidcAudience, mode, hubEndpoint string) string {
	if oidcAudience != "" {
		return oidcAudience
	}
	if mode == "cloudrun_invoker" {
		return hubEndpoint
	}
	return ""
}

// wireHubCoreMetrics wires the Hub's db, broker-dispatch and launch-reaper
// metrics recorders to mp, returning the db recorder for callers that need
// to pass it along separately (event publisher / web server construction).
// Extracted from runServerStart's OTel-metrics block above so it can be
// exercised directly in a test with a ManualReader-backed MeterProvider (see
// server_foreground_metrics_test.go), rather than only indirectly through
// the whole runServerStart path.
func wireHubCoreMetrics(hubSrv *hub.Server, mp metric.MeterProvider) dbmetrics.Recorder {
	var hubDBRec dbmetrics.Recorder

	dbRec, dbErr := dbmetrics.New(mp)
	if dbErr != nil {
		log.Printf("WARNING: hub db metrics disabled: %v", dbErr)
	} else {
		hubDBRec = dbRec
		hubSrv.SetDBMetrics(dbRec)
	}

	dispRec, dispErr := dispatchmetrics.New(mp)
	if dispErr != nil {
		log.Printf("WARNING: hub dispatch metrics disabled: %v", dispErr)
	} else {
		hubSrv.SetDispatchMetrics(dispRec)
	}

	reaperRec, reaperErr := reapermetrics.New(mp)
	if reaperErr != nil {
		log.Printf("WARNING: hub launch reaper metrics disabled: %v", reaperErr)
	} else {
		hubSrv.SetReaperMetrics(reaperRec)
	}

	if authzRec, err := hub.NewOTelConduitStreamAuthzMetrics(mp); err != nil {
		log.Printf("WARNING: hub conduit stream authz metrics disabled: %v", err)
	} else {
		hubSrv.SetConduitStreamAuthzMetrics(authzRec)
	}
	if refusedRec, err := hub.NewOTelConduitOpenRefusedMetrics(mp); err != nil {
		log.Printf("WARNING: hub conduit stream open refusal metrics disabled: %v", err)
	} else {
		hubSrv.SetConduitOpenRefusedMetrics(refusedRec)
	}

	if err := hubSrv.RegisterConduitRelayReadinessGauge(mp); err != nil {
		log.Printf("WARNING: hub conduit relay readiness gauge disabled: %v", err)
	}
	if err := hubSrv.RegisterBrokerPresenceMetrics(mp); err != nil {
		log.Printf("WARNING: hub broker presence metrics disabled: %v", err)
	}

	if refreshRec, err := hub.NewOTelConduitAgentRefreshMetrics(mp); err != nil {
		log.Printf("WARNING: hub conduit agent refresh metrics disabled: %v", err)
	} else {
		hubSrv.SetConduitAgentRefreshMetrics(refreshRec)
	}

	if brokerRPCRec, err := hub.NewOTelConduitBrokerRPCMetrics(mp); err != nil {
		log.Printf("WARNING: hub conduit broker rpc metrics disabled: %v", err)
	} else {
		hubSrv.SetConduitBrokerRPCMetrics(brokerRPCRec)
	}

	if brokerIncRec, err := hub.NewOTelConduitBrokerIncarnationMetrics(mp); err != nil {
		log.Printf("WARNING: hub conduit broker incarnation metrics disabled: %v", err)
	} else {
		hubSrv.SetConduitBrokerIncarnationMetrics(brokerIncRec)
	}

	if conflictRec, err := hub.NewOTelQuotaScopeConflictMetrics(mp); err != nil {
		log.Printf("WARNING: hub quota scope conflict metrics disabled: %v", err)
	} else {
		hubSrv.SetQuotaScopeConflictMetrics(conflictRec)
	}

	if capacityRec, err := hub.NewOTelQuotaCapacityMetrics(mp); err != nil {
		log.Printf("WARNING: hub quota capacity metrics disabled: %v", err)
	} else {
		hubSrv.SetQuotaCapacityMetrics(capacityRec)
	}

	if portProxyRec, err := hub.NewOTelPortProxyMetrics(mp); err != nil {
		log.Printf("WARNING: hub port proxy metrics disabled: %v", err)
	} else {
		hubSrv.SetPortProxyMetrics(portProxyRec)
	}

	runScopeRec, runScopeErr := hub.NewOTelAgentRunScopeMetrics(mp)
	if runScopeErr != nil {
		log.Printf("WARNING: hub agent run-scope metrics disabled: %v", runScopeErr)
	} else {
		hubSrv.SetAgentRunScopeMetrics(runScopeRec)
	}

	// Decision logging (remaining-audit P1): the audit writer's
	// scion.logging.* series and the decision-log disposition counter.
	// The same instruments carry writer=cloud (P2): the Cloud Logging
	// counters are process atomics read at collection, so counts recorded
	// before this MeterProvider existed are exported too. Without a Cloud
	// handler they stay zero and export no series.
	if writeRec, err := logging.NewWriteMetrics(mp); err != nil {
		log.Printf("WARNING: hub audit log writer metrics disabled: %v", err)
	} else {
		hubSrv.SetAuditWriterMetrics(writeRec)
		writeRec.ObserveCloud(serverCloudWriter())
	}
	if decisionRec, err := hub.NewOTelDecisionAuditMetrics(mp); err != nil {
		log.Printf("WARNING: hub decision audit metrics disabled: %v", err)
	} else {
		hubSrv.SetDecisionAuditMetrics(decisionRec)
	}

	return hubDBRec
}

// serverCloudWriter returns the writer=cloud counters the server exports
// and reports on /healthz: logging.CloudWriter() in production. A var so
// cmd tests can substitute standalone counters instead of the process-wide
// ones.
var serverCloudWriter = logging.CloudWriter

// wireCloudLoggingHealth injects the cloud_logging /healthz provider when a
// Cloud Logging handler is configured (its circuit breaker registered with
// cs). Without one the key stays absent. The key is non-critical: it can
// only degrade /healthz.
func wireCloudLoggingHealth(hubSrv *hub.Server, cs *logging.CloudWriteStats) {
	if hubSrv == nil || cs == nil || !cs.Configured() {
		return
	}
	hubSrv.SetCloudLoggingHealth(func() string { return cs.HealthStatus(time.Now()) })
}

func initHubServer(ctx context.Context, cfg *config.GlobalConfig, s store.Store, entClient *ent.Client, endpoints hub.HubEndpoints, devAuthToken string, adminEmailList []string, adminMode bool, maintenanceMessage string, requestLogger, messageLogger, artifactLogger *slog.Logger, globalDir string, pluginMgr *scionplugin.Manager, secretBackend secret.SecretBackend) (*hub.Server, error) {
	hubCfg := buildHubServerConfig(cfg, endpoints.Operator, devAuthToken, adminEmailList, adminMode, maintenanceMessage, secretBackend)
	hubCfg.HubEndpoints = &endpoints
	hubCfg.PluginHubAPIURL = pluginHubAPIURLFrom(cfg, currentServerEndpointMode())
	warnNonConformingHubName(cfg.Hub.HubName)

	// In hosted mode every replica must share the same session secret for
	// cookies and JWT signing keys to work across the load balancer. Running
	// without one means each replica generates its own ephemeral key, which
	// breaks session persistence and causes login loops.
	if hostedMode && hubCfg.SharedSigningSecret == "" {
		log.Println("WARNING: hosted mode is enabled but no session secret is configured. " +
			"Set --session-secret or SCION_SERVER_SESSION_SECRET to avoid cross-replica session failures.")
	}

	// Derive TelemetryProjectID using a priority chain:
	// 1. Explicit telemetry.cloud.gcp_project_id setting (highest priority)
	// 2. Derived from scion-telemetry-gcp-credentials SA key JSON
	// 3. Existing GCPProjectID fallback (SA minting project, handled in server.go)
	if pid := telemetryGCPProjectFromSettings(cfg); pid != "" {
		hubCfg.TelemetryProjectID = pid
		log.Printf("Telemetry project ID from settings: %s", pid)
	}
	if hubCfg.TelemetryProjectID == "" && secretBackend != nil {
		if pid := telemetryGCPProjectFromSecret(ctx, secretBackend, cfg.Hub.ResolveHubID()); pid != "" {
			hubCfg.TelemetryProjectID = pid
			log.Printf("Telemetry project ID derived from SA credentials: %s", pid)
		}
	}

	// Construct proxy authenticator when auth mode is "proxy"
	if cfg.Auth.Mode == "proxy" && cfg.Auth.Proxy != nil {
		switch cfg.Auth.Proxy.Provider {
		case "iap":
			if cfg.Auth.Proxy.IAP == nil || cfg.Auth.Proxy.IAP.Audience == "" {
				return nil, fmt.Errorf("auth.proxy.iap.audience is required when auth.mode=proxy and provider=iap")
			}
			hubCfg.ProxyAuth = &hub.IAPAuthenticator{
				Audience: cfg.Auth.Proxy.IAP.Audience,
				Issuer:   cfg.Auth.Proxy.IAP.Issuer,
				JWKSURL:  cfg.Auth.Proxy.IAP.JWKSURL,
			}
			log.Printf("Proxy auth configured: provider=iap, audience=%s", cfg.Auth.Proxy.IAP.Audience)
		case "jwt":
			jwtAuth, err := newJWTProxyAuthenticator(cfg.Auth.Proxy.JWT)
			if err != nil {
				return nil, err
			}
			hubCfg.ProxyAuth = jwtAuth
			log.Printf("Proxy auth configured: provider=jwt, algorithm=%s", cfg.Auth.Proxy.JWT.Algorithm)
		case "header":
			// TODO: HeaderProxyAuthenticator (refactor of extractProxyUser)
			log.Printf("Proxy auth configured: provider=header (legacy IP-trust mode)")
		default:
			return nil, fmt.Errorf("unsupported auth.proxy.provider: %q", cfg.Auth.Proxy.Provider)
		}
	}

	// Proxy driver principals (auth.proxy.driverPrincipals, test identities
	// phase WS2): exact emails, proxy auth mode only. A non-empty list on a
	// hub that is not in proxy mode refuses startup; hub.New re-checks it.
	proxyDrivers, driversErr := config.ValidateProxyDriverConfig(cfg.Auth.Mode, cfg.Auth.Proxy.ProxyDriverPrincipals())
	if driversErr != nil {
		return nil, driversErr
	}
	hubCfg.ProxyDriverPrincipals = proxyDrivers
	if len(proxyDrivers) > 0 {
		log.Printf("Proxy driver principals configured: %d (test identity web session drivers; they never get a hub account)", len(proxyDrivers))
	}

	// Construct transport token minter when auth.transport is configured
	if cfg.Auth.Transport != nil && cfg.Auth.Transport.Mode != "" && cfg.Auth.Transport.Mode != "none" {
		if cfg.Auth.Transport.PlatformAuthSA == "" {
			return nil, fmt.Errorf("auth.transport.platformAuthSA is required when auth.transport.mode=%q", cfg.Auth.Transport.Mode)
		}
		audience := resolveTransportAudience(cfg.Auth.Transport.OIDCAudience, cfg.Auth.Transport.Mode, endpoints.For(hub.HubEndpointForTransportAudience))
		if audience == "" {
			return nil, fmt.Errorf("auth.transport.oidcAudience is required when auth.transport.mode=%q", cfg.Auth.Transport.Mode)
		}
		hubCfg.TransportMode = cfg.Auth.Transport.Mode
		hubCfg.TransportAudience = audience
		hubCfg.TransportMinter = hub.NewGCPTransportMinter(cfg.Auth.Transport.PlatformAuthSA, "")
		hubCfg.PlatformAuthSA = cfg.Auth.Transport.PlatformAuthSA
		log.Printf("Transport auth configured: mode=%s, audience=%s, sa=%s",
			cfg.Auth.Transport.Mode, audience, cfg.Auth.Transport.PlatformAuthSA)
	}

	// Hub-issued test identities (--test-mode, off by default): every
	// precondition of the requested level is checked here and any unmet one
	// refuses startup with a named error. hub.New re-checks the result and
	// verifies the signing key sources (G7) before capturing the immutable
	// gate, and writes the resolved level through the test admin audit
	// sink.
	testMode, err := resolveTestMode(testModeInputsFor(ctx, cfg, hubCfg, s != nil, entClient))
	if err != nil {
		return nil, err
	}
	hubCfg.EnableTestIdentities = testMode.EnableTestIdentities
	hubCfg.TestAdminGate = testMode.TestAdminGate
	hubCfg.TestMode = testMode.TestMode
	hubCfg.TestAdminWebSessions = testMode.TestAdminWebSessions
	if testMode.EnableTestIdentities {
		slog.Warn("Test identity issuance is ENABLED (--test-mode). Holders of test_identity.issue can create short-lived synthetic users on this hub. Use on test hubs only; MUST NOT be enabled in production!",
			"test_mode", string(testMode.TestMode.Level), "preconditions", testMode.TestMode.Preconditions)
	}
	// Workload issuer allowlist (server.hub.test_identities.workload_issuers,
	// phase X2): startup config only. A malformed list or an admin-tier
	// entry refuses startup; hub.New re-checks it with the issuer audience
	// cross-checks.
	if issuers := cfg.Hub.TestIdentityWorkloadIssuers(); len(issuers) > 0 {
		if err := config.ValidateTestIdentityWorkloadIssuers(issuers); err != nil {
			return nil, err
		}
		hubCfg.TestIdentityWorkloadIssuers = issuers
		log.Printf("Test identity workload issuers configured: %d (accepted only with --test-mode=member or above)", len(issuers))
	}

	// Test identity web sessions (WS1): the hub-local key from the test
	// infrastructure marker. A missing or unusable key only leaves web
	// sessions unavailable; it never stops startup.
	hubCfg.TestIdentityWebSessionKey = loadTestIdentityWebSessionKey(ctx, entClient, hubCfg.HubID, hubCfg.EnableTestIdentities)

	hubSrv, err := hub.New(hubCfg, s)
	if err != nil {
		return nil, fmt.Errorf("hub server initialization failed: %w", err)
	}
	hubSrv.SetRequestLogger(requestLogger)
	hubSrv.SetPluginManager(pluginMgr)
	hubSrv.SetIntegrationHA(cfg.Database.Driver, entClient, cfg.Database.URL)
	if messageLogger != nil {
		hubSrv.SetMessageLogger(messageLogger)
	}
	if artifactLogger != nil {
		hubSrv.SetArtifactLogger(artifactLogger)
	}

	// Load notification channels from versioned settings. With operational
	// settings (any DB driver), ApplySnapshot rebuilds the registry from the
	// notifications section at startup and on every change.
	if vs, err := loadServerVersionedSettings(); err == nil && vs.Server != nil && len(vs.Server.NotificationChannels) > 0 {
		registry := hub.NewChannelRegistryFromSettings(vs.Server.NotificationChannels)
		hubSrv.SetChannelRegistry(registry)
		log.Printf("Notification channels configured: %d channel(s) registered", registry.Len())
	} else if err != nil {
		log.Printf("Warning: failed to load settings; notification channels are not configured: %v", err)
	}

	// NOTE: Message broker initialization is deferred to the main startup
	// flow (after the event publisher is set) so that StartMessageBroker
	// can create its proxy with the ChannelEventPublisher.

	// Initialize storage
	if err := initHubStorage(ctx, hubSrv, cfg, globalDir); err != nil {
		return nil, err
	}

	// Hub ID was already resolved and set during initHubServer via ServerConfig.HubID
	hubID := hubSrv.HubID()
	log.Printf("Hub instance ID: %s", hubID)
	if hubID != "" && storageBucket != "" {
		slog.Info("storage: using hub-scoped GCS paths", "hub_id", hubID, "prefix", "hubs/"+hubID+"/")
	}
	if storageBucket != "" && cfg.Hub.IsHubIDUnconfigured() {
		slog.Warn("storage: hub_id was auto-generated from hostname; "+
			"set an explicit hub_id in settings for multi-hub deployments",
			"hub_id", hubID,
			"storage_bucket", storageBucket)
	}

	// Secret backend was initialized before hub.New() and passed via
	// ServerConfig.SecretBackend so that signing keys are loaded from it
	// during server creation. Log the configured backend here.
	if secretBackend != nil {
		log.Printf("Secret backend configured: %s", cfg.Secrets.Backend)
	}

	// Initialize GCP token generator for agent identity impersonation.
	// This uses Application Default Credentials; on GCE/Cloud Run the Hub's
	// own SA is auto-detected. Non-fatal if GCP is not available.
	gcpGen, gcpErr := hub.NewIAMTokenGenerator(ctx, "")
	if gcpErr != nil {
		log.Printf("GCP token generator not available (agent GCP identity disabled): %v", gcpErr)
	} else {
		hubSrv.SetGCPTokenGenerator(hub.NewCachedGCPTokenGenerator(gcpGen))
		saEmail := gcpGen.ServiceAccountEmail()
		if saEmail != "" {
			log.Printf("GCP token generator configured (hub SA: %s)", saEmail)
		} else {
			log.Printf("GCP token generator configured (hub SA: unknown - not running on GCE)")
		}
	}

	// Initialize GCP IAM admin client for minting service accounts.
	// Non-fatal if GCP is not available — minting will be disabled.
	gcpAdmin, adminErr := hub.NewIAMAdminClient(ctx)
	if adminErr != nil {
		log.Printf("GCP IAM admin not available (service account minting disabled): %v", adminErr)
	} else {
		// Resolve project ID for minting
		if projectID, err := hub.ResolveGCPProjectID(cfg.Hub.GCPProjectID); err != nil {
			log.Printf("GCP project ID not available (service account minting disabled): %v", err)
		} else {
			hubSrv.SetGCPServiceAccountAdmin(gcpAdmin)
			hubSrv.SetGCPProjectID(projectID)
			log.Printf("GCP service account minting configured (project: %s)", projectID)
		}
	}

	// Initialize Policy Troubleshooter checker for actAs checks.
	// Non-fatal if the PT API is not available — the existing disabled checker
	// remains in place and denies when mode is "enforce" (fail-closed by
	// construction). Requires a GCP project ID for quota/billing context and
	// the GCP token generator for diagnostic messages.
	ptProjectID, ptProjErr := hub.ResolveGCPProjectID(cfg.Hub.GCPProjectID)
	if ptProjErr != nil {
		log.Printf("Policy Troubleshooter: no GCP project ID available (actAs check will be unavailable): %v", ptProjErr)
	} else {
		ptClient, ptErr := policytroubleshooteriam.NewPolicyTroubleshooterClient(ctx,
			option.WithQuotaProject(ptProjectID),
		)
		if ptErr != nil {
			log.Printf("Policy Troubleshooter not available (actAs check will be unavailable; ensure Policy Troubleshooter API is enabled and Hub SA has serviceUsage.serviceUsageConsumer role): %v", ptErr)
		} else {
			hubSAEmail := ""
			if gcpErr == nil {
				hubSAEmail = gcpGen.ServiceAccountEmail()
			}
			checker := hub.NewPolicyTroubleshooterChecker(ptClient, hubSAEmail, hubSrv.DenyUnknownFailOpen())
			// Follow reloads of the deny-unknown fallback policy.
			checker.SetDenyUnknownPolicySource(hubSrv.DenyUnknownFailOpen)
			// Report real API calls, not cached results, to the admin
			// diagnostic for the assignment check.
			checker.SetCallObserver(hubSrv.NoteSAAssignCheckCall)
			cached := hub.NewCachedCallerPermissionChecker(checker,
				60*time.Second, // allowTTL
				10*time.Second, // denyTTL
			)
			hubSrv.SetSAAssignChecker(cached)
			hubSrv.SetHookIdentityChecker(cached)
			// Registration and verification warn when the hub's identity
			// cannot read IAM policy in an account's GCP project other
			// than this one; the assignment check would fail there.
			hubSrv.SetSAProjectAccessProbe(hub.NewPTProjectAccessProbe(ptClient), ptProjectID)
			// Both surfaces share one checker instance and one cache.
			log.Printf("Policy Troubleshooter checker configured for actAs checks (quota project: %s)", ptProjectID)
		}
	}

	if hostedMode {
		// Hosted mode: bootstrap bundled resources directly into the Hub from
		// the binary's embedded catalog. This removes the dependency on local
		// ~/.scion directories and ensures every replica converges on the same
		// DB + storage state. Advisory lock prevents concurrent replicas from
		// racing this bootstrap (audit finding C6).
		runWithAdvisoryLock(ctx, s, store.LockBundledResources, "bundled resource bootstrap", func() {
			if err := hubSrv.BootstrapBundledResources(ctx, hub.BootstrapOptions{
				RepairStorage:   true,
				OverwritePolicy: hub.OverwriteBuiltinManaged,
			}); err != nil {
				log.Printf("Warning: bundled resource bootstrap failed: %v", err)
			}
		})
	} else {
		// Workstation mode: import from local ~/.scion directories. These were
		// refreshed from embeds earlier in the startup sequence.
		bootstrapWorkstationResources(ctx, s, hubSrv, globalDir)
	}

	// On first boot with hub-namespaced paths, copy legacy GCS objects to
	// the hub-scoped prefix so that subsequent sync/dispatch uses the
	// namespaced paths. Runs synchronously before SyncAll* calls.
	// Advisory lock prevents concurrent replicas from racing (audit finding C6).
	runWithAdvisoryLock(ctx, s, store.LockStorageMigration, "storage migration", func() {
		hubSrv.MigrateStorageOnFirstBoot(ctx)
	})

	// Reconcile resource DB manifest hashes against actual GCS content.
	// In multi-hub mode a peer hub may have uploaded newer files; this ensures
	// the local DB reflects reality before any dispatch is attempted.
	go hubSrv.SyncAllHarnessConfigsFromStorage(ctx)
	go hubSrv.SyncAllTemplatesFromStorage(ctx)

	log.Printf("Database: %s (%s)", cfg.Database.Driver, config.RedactDatabaseURL(cfg.Database.Driver, cfg.Database.URL))

	// Warn once (outside the settings retry loop below) about SCION_SERVER_*
	// and SCION_SEED_* names that no loader maps to a setting, so a
	// misspelled override is not silently ignored (ptone/scion#1081). Names
	// only, never values.
	config.WarnUnmatchedSettingsEnv(slog.Default(), os.Environ(), opsettings.IsLayer1Key)

	// --- Settings-DB Phase 3: OperationalSettings wiring (§3.9) ---
	// Driver-agnostic: initOperationalSettings handles both postgres (advisory
	// locking) and SQLite (single-writer) via the existing AdvisoryLocker branch.
	//
	// Fail-closed: if settings cannot be loaded the hub must not serve ordinary
	// traffic, because a DB-persisted admin_mode=true would be invisible to this
	// process. Retry with exponential backoff to tolerate transient DB issues,
	// then fail the hub startup if all attempts are exhausted.
	// NOTE: this call and its error-return chain (up to runServerStart) is what makes
	// settings init fail-closed. Do NOT revert to the old log-and-continue
	// pattern — that would let the server accept traffic without authoritative
	// settings, silently bypassing a DB-persisted admin_mode=true.
	// See TestInitOperationalSettings_FailClosed for the regression test.
	if err := initOperationalSettingsWithRetry(ctx, cfg, hubSrv, s, globalDir); err != nil {
		return nil, fmt.Errorf("operational settings init failed after retries (driver=%s): %w",
			cfg.Database.Driver, err)
	}
	// Test admin tier G8, second half: no federation in the stored
	// operational settings either.
	if err := hubSrv.VerifyTestAdminFederationOff(); err != nil {
		return nil, err
	}

	// ptone/scion#1316 fault 3: confirm the hub's own agent_defaults resolve
	// to registered resources now that bundled resources are seeded and
	// operational settings are loaded, so a misconfigured default is a
	// startup warning rather than a silent 502 at the first agent create.
	hubSrv.ValidateStartupDefaults(ctx)

	return hubSrv, nil
}

// settingsInitMaxRetries is the number of retry attempts for operational
// settings initialization before the hub gives up and refuses to start.
const settingsInitMaxRetries = 3

// initOperationalSettingsWithRetry wraps initOperationalSettings with
// exponential backoff. If all attempts fail, it returns the last error
// so the caller can abort hub startup (fail-closed). This ensures a
// DB-persisted admin_mode=true is never bypassed due to a transient
// boot-time failure loading settings.
func initOperationalSettingsWithRetry(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, s store.Store, globalDir string) error {
	var lastErr error
	for attempt := 0; attempt <= settingsInitMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 2 * time.Second
			slog.Warn("Retrying operational settings init",
				"attempt", attempt+1,
				"backoff", backoff,
				"prev_error", lastErr,
			)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("context cancelled during settings init retry: %w", ctx.Err())
			}
		}
		lastErr = initOperationalSettings(ctx, cfg, hubSrv, s, globalDir)
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// initOperationalSettings sets up the OperationalSettings service (settings-db
// §3.9). It is driver-agnostic: postgres uses advisory locking, SQLite uses the
// single-writer no-op path. It:
//  1. Acquires advisory lock "hub_settings_seed"
//  2. If no _meta row exists, seeds sections from settings.yaml (file values only)
//  3. Releases the lock
//  4. Calls Refresh to load sections
//  5. Retires stored runtime-profile timezone values (hub.RetireProfileTimezones)
//  6. Applies the snapshot
//  7. Logs any env-overridden Layer-1 keys as a WARN
func initOperationalSettings(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, s store.Store, globalDir string) error {
	// The runtime-profile timezone was removed. Scan the settings file for
	// leftover profiles.<name>.timezone keys (a raw map walk; the struct
	// field is gone) so the seed material can drop them and the retirement
	// step below can report or migrate them.
	tzScan, err := config.ScanSettingsFileProfileTimezones(globalDir)
	if err != nil {
		slog.Warn("Could not scan the settings file for removed runtime-profile timezones", "file", tzScan.Path, "error", err)
	}

	settingStore, ok := s.(store.HubSettingStore)
	if !ok {
		log.Println("WARNING: store does not implement HubSettingStore; skipping operational settings init")
		_ = hub.RetireProfileTimezones(ctx, nil, hub.ProfileTimezoneRetireInput{File: tzScan}, slog.Default())
		return nil
	}

	// Build koanf instances.
	envKoanf := config.LoadEnvKoanf()
	// Seed material never carries the removed key, so the every-boot sync
	// cannot write it back into a seeded profiles row.
	bootstrapKoanf := config.LoadServerBootstrapKoanf(serverConfigPath, tzScan.ProfileTimezones)

	// Log deprecation warnings for SCION_SERVER_* env vars that overlap
	// Layer-1 settings (these should use SCION_SEED_* instead).
	opsettings.LogDeprecatedServerEnv(envKoanf, slog.Default())

	// --- Every-boot re-sync under advisory lock ---
	locker, hasLocker := s.(store.AdvisoryLocker)
	if hasLocker {
		acquired, release, err := locker.TryAdvisoryLock(ctx, store.LockHubSettingsSeed)
		if err != nil {
			return fmt.Errorf("acquiring hub_settings_seed advisory lock: %w", err)
		}
		if acquired {
			syncErr := syncHubSettings(ctx, settingStore, bootstrapKoanf)
			if rerr := release(); rerr != nil {
				slog.Error("Failed to release hub_settings_seed advisory lock", "error", rerr)
			}
			if syncErr != nil {
				return fmt.Errorf("syncing hub settings: %w", syncErr)
			}
		} else {
			log.Println("Hub settings seed lock held by another replica; skipping sync")
		}
	} else {
		if err := syncHubSettings(ctx, settingStore, bootstrapKoanf); err != nil {
			return fmt.Errorf("syncing hub settings: %w", err)
		}
	}

	// --- Create OperationalSettings, Refresh, and apply ---
	ops := hub.NewOperationalSettings(settingStore, bootstrapKoanf, envKoanf)
	// The admin view shows a public URL from the layers startup's endpoint
	// resolution reads, not a seed value (ptone/scion#4826).
	ops.SetUnseededBootstrap(config.LoadUnseededBootstrapKoanfWithConfigPath(serverConfigPath))
	// It attributes a superseded key that only the coded defaults set to
	// them, not to settings.yaml (ptone/scion#4983).
	ops.SetAboveSeedBootstrap(config.LoadAboveSeedBootstrapKoanfWithConfigPath(serverConfigPath))

	changed, err := ops.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("initial operational settings refresh: %w", err)
	}
	if len(changed) > 0 {
		log.Printf("Operational settings loaded from DB: %v", changed)
	}

	// Retire stored runtime-profile timezone values once settings are
	// loaded and seeded, before the snapshot is applied and before the
	// dispatcher is wired. Non-fatal: a failure leaves the values in place
	// (nothing reads them) and the next start retries.
	if err := hub.RetireProfileTimezones(ctx, ops, hub.ProfileTimezoneRetireInput{
		DBTier: hubSrv.IsPostgres(),
		File:   tzScan,
	}, slog.Default()); err != nil {
		slog.Error("Retiring runtime-profile timezones failed; will retry at the next start", "error", err)
	}

	snap := ops.Snapshot()
	hub.ApplySnapshot(hubSrv, snap)
	hub.ApplyMaintenanceFromSnapshot(hubSrv, snap)

	hubSrv.SetOperationalSettings(ops)

	// One-time image_registry warnings (ptone/scion#3916): a maintenance
	// registry that no longer applies to agent images, and settings-file
	// values a saved hub value now beats.
	hubSrv.WarnMaintenanceImageRegistryDrift()
	hubSrv.WarnShadowedImageRegistries(flatInstanceImageRegistries())

	if envKeys := ops.EnvOverriddenKeys(); len(envKeys) > 0 {
		slog.Warn("Layer-1 settings overridden by SCION_SERVER_* env vars on this node — these values diverge from the shared DB",
			"keys", envKeys)
	}

	return nil
}

// startSettingsPropagation wires the event publisher into the OperationalSettings
// service and starts the cross-replica propagation loop (design §3.6, Phase 4).
// When OperationalSettings is nil (init failed) this is a no-op. It runs on
// every driver: on SQLite the publisher is an in-process ChannelEventPublisher
// (single replica), so propagation is effectively local self-apply plus the
// poll backstop.
func startSettingsPropagation(ctx context.Context, hubSrv *hub.Server, eventPub hub.EventPublisher) {
	ops := hubSrv.GetOperationalSettings()
	if ops == nil {
		return // OperationalSettings unavailable — no propagation possible
	}
	ops.SetEventPublisher(eventPub)
	ops.StartPropagation(ctx, hubSrv)
	slog.Info("Settings change propagation started (subscribe + poll backstop)")
}

// syncHubSettings performs every-boot re-sync of hub_settings from bootstrap
// material. For each registered section:
//   - If the row is absent or origin=="seeded": write the bootstrap doc
//     (skip-write-on-equality to avoid revision bumps and event churn),
//     except that an absent catalog section (runtimes, profiles,
//     harness_configs) with an empty bootstrap doc stays absent.
//   - If origin=="managed": skip — admin owns this section.
//
// Also runs BackfillOrigin for pre-origin rows and writes the _meta sentinel.
func syncHubSettings(ctx context.Context, s store.HubSettingStore, bootstrapKoanf *koanf.Koanf) error {
	// Backfill origin for rows created before the origin column existed.
	if err := s.BackfillOrigin(ctx); err != nil {
		return fmt.Errorf("backfilling origin: %w", err)
	}

	log.Println("Syncing hub_settings from bootstrap material...")

	for _, sec := range opsettings.Registry {
		if len(sec.KoanfPaths) == 0 {
			continue
		}

		bootstrapDoc, err := opsettings.ExtractSectionFromKoanf(bootstrapKoanf, sec.Name)
		if err != nil {
			slog.Warn("Failed to extract section for sync; skipping", "section", sec.Name, "error", err)
			continue
		}

		existing, err := s.GetHubSetting(ctx, sec.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("checking section %q: %w", sec.Name, err)
		}

		// The decision is opsettings.SyncedSection, shared with the rebuild
		// preflight. BackfillOrigin has already run, so the stored origin is
		// the effective one and UpdatedBy is not passed.
		var row *opsettings.SectionRow
		if existing != nil {
			row = &opsettings.SectionRow{Origin: existing.Origin, Value: existing.Value}
		}
		doc, present := opsettings.SyncedSection(sec.Name, row, bootstrapDoc)

		if existing == nil {
			// An empty catalog section is left absent: an empty stored
			// runtimes, profiles or harness_configs map means "not managed
			// in the hub", and absent says so without a row
			// (ptone/scion#5007).
			if !present {
				slog.Debug("Empty catalog section not seeded", "section", sec.Name)
				continue
			}
			// No row — create with bootstrap material.
			if _, err := s.UpsertHubSetting(ctx, sec.Name, doc, "seed", -1, "seeded"); err != nil {
				return fmt.Errorf("seeding section %q: %w", sec.Name, err)
			}
			log.Printf("  Seeded section: %s (new)", sec.Name)
			continue
		}

		// Write only when SyncedSection's document differs from the row, so
		// the write set comes from the shared rule alone: a managed row's
		// document is its own value and is never rewritten. Compare
		// semantically (not byte-for-byte) because Postgres jsonb
		// re-serializes values, changing whitespace and key ordering.
		if jsonEqual(existing.Value, doc) {
			if existing.Origin == opsettings.OriginManaged {
				slog.Debug("Skipping managed section", "section", sec.Name)
			} else {
				slog.Debug("Seeded section unchanged; skipping write", "section", sec.Name)
			}
			continue
		}

		if _, err := s.UpsertHubSetting(ctx, sec.Name, doc, "seed", -1, "seeded"); err != nil {
			return fmt.Errorf("re-syncing section %q: %w", sec.Name, err)
		}
		log.Printf("  Re-synced section: %s (content changed)", sec.Name)
	}

	// Write/update _meta sentinel.
	metaDoc, _ := json.Marshal(map[string]interface{}{
		"synced_at":    time.Now().UTC().Format(time.RFC3339),
		"seed_version": "2",
	})
	if _, err := s.UpsertHubSetting(ctx, "_meta", metaDoc, "seed", -1, "seeded"); err != nil {
		return fmt.Errorf("writing _meta sentinel: %w", err)
	}

	log.Println("Hub settings sync complete")
	return nil
}

// jsonEqual compares two JSON documents semantically, ignoring whitespace
// and key ordering differences (as Postgres jsonb re-serializes values).
// Both inputs must come through json.Unmarshal so numeric types are
// consistently float64. If either path changes to produce integer types
// (e.g. a custom decoder), semantically equal values could compare unequal.
func jsonEqual(a, b json.RawMessage) bool {
	var aVal, bVal interface{}
	if err := json.Unmarshal(a, &aVal); err != nil {
		return bytes.Equal(a, b)
	}
	if err := json.Unmarshal(b, &bVal); err != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(aVal, bVal)
}

// initHubStorage initializes the storage backend for the Hub server.
// It always ensures a storage backend is configured: if the explicitly
// configured backend (GCS or local) fails, it falls back to a local
// filesystem storage at ~/.scion/storage so that template bootstrap
// and other storage-dependent features still work.
func initHubStorage(ctx context.Context, hubSrv *hub.Server, cfg *config.GlobalConfig, globalDir string) error {
	if storageBucket != "" {
		log.Printf("Initializing GCS storage with bucket: %s", storageBucket)
		storageCfg := storage.Config{
			Provider: storage.ProviderGCS,
			Bucket:   storageBucket,
		}
		stor, err := storage.New(ctx, storageCfg)
		if err != nil {
			if hostedHAGuardsRequired(cfg) {
				return fmt.Errorf("failed to initialize required GCS storage: %w", err)
			}
			log.Printf("Warning: failed to initialize GCS storage, falling back to local: %v", err)
		} else {
			hubSrv.SetStorage(stor)
			log.Printf("GCS storage configured: gs://%s", storageBucket)
			return nil
		}
	} else if storageDir != "" {
		log.Printf("Initializing local storage at: %s", storageDir)
		storageCfg := storage.Config{
			Provider:  storage.ProviderLocal,
			LocalPath: storageDir,
		}
		stor, err := storage.New(ctx, storageCfg)
		if err != nil {
			log.Printf("Warning: failed to initialize local storage, falling back to default: %v", err)
		} else {
			hubSrv.SetStorage(stor)
			log.Printf("Local storage configured: %s", storageDir)
			return nil
		}
	}

	// Always set up a local filesystem fallback so that template bootstrap
	// and other storage-dependent features work even when no explicit
	// storage backend is configured (or the configured one failed).
	defaultStorageDir := filepath.Join(globalDir, "storage")
	if storageBucket == "" && storageDir == "" {
		log.Printf("WARNING: No storage backend configured. Using local filesystem storage at: %s", defaultStorageDir)
		log.Printf("  For production use, configure --storage-bucket (GCS) or --storage-dir (explicit local path)")
	}
	storageCfg := storage.Config{
		Provider:  storage.ProviderLocal,
		LocalPath: defaultStorageDir,
		Bucket:    "local",
	}
	stor, err := storage.New(ctx, storageCfg)
	if err != nil {
		log.Printf("Warning: failed to initialize local storage fallback: %v", err)
		return nil
	}
	hubSrv.SetStorage(stor)
	return nil
}

// newEventPublisher selects the event publisher backend based on the configured
// database driver. With Postgres it returns a PostgresEventPublisher
// (cross-replica LISTEN/NOTIFY); otherwise it returns the in-process
// ChannelEventPublisher. If the Postgres publisher cannot be started in hosted
// HA mode startup fails closed because in-process events cannot cross replicas.
func newEventPublisher(ctx context.Context, cfg *config.GlobalConfig, dbRec dbmetrics.Recorder) (hub.EventPublisher, error) {
	if strings.EqualFold(cfg.Database.Driver, "postgres") {
		if dbRec == nil {
			dbRec = dbmetrics.NewDisabled()
		}
		pub, err := hub.NewPostgresEventPublisher(ctx, cfg.Database.URL, dbRec, logging.Subsystem("hub.events"))
		if err != nil {
			if hostedHAGuardsRequired(cfg) {
				return nil, fmt.Errorf("failed to start required Postgres event publisher: %w", err)
			}
			log.Printf("WARNING: failed to start Postgres event publisher (%v); falling back to in-process events. Cross-replica SSE will not work.", err)
			return hub.NewChannelEventPublisher(), nil
		}
		log.Printf("Using Postgres LISTEN/NOTIFY event publisher")
		return pub, nil
	}
	return hub.NewChannelEventPublisher(), nil
}

// newCommandBus selects the command bus backend. With Postgres it returns a
// PostgresCommandBus (LISTEN/NOTIFY on scion_broker_cmd); otherwise it returns
// a no-op bus (single-process SQLite always owns all brokers locally).
//
// If the Postgres bus cannot start, the hub normally falls back to the
// no-op bus. In hosted HA with hub.conduit on that is a startup error
// instead (C8): cross-replica conduit routing and dispatch depend on the
// listener, so a replica without it must not serve.
func newCommandBus(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server) (hub.CommandBus, error) {
	if !strings.EqualFold(cfg.Database.Driver, "postgres") {
		return hub.NoopCommandBus{}, nil
	}
	ownsLocally := func(brokerID string) bool {
		mgr := hubSrv.GetControlChannelManager()
		if mgr == nil {
			return false
		}
		return mgr.IsConnected(brokerID)
	}
	bus, err := startPostgresCommandBus(ctx, cfg.Database.URL, ownsLocally, hubSrv.ReconcileBroker, logging.Subsystem("hub.commandbus"))
	if err != nil {
		if hubSrv.ConduitEnabled() && hostedHAGuardsRequired(cfg) {
			return nil, fmt.Errorf("postgres command bus startup failed (hosted HA with hub.conduit on): %w", err)
		}
		log.Printf("WARNING: failed to start Postgres command bus (%v); falling back to no-op. Cross-replica dispatch signals will not work.", err)
		return hub.NoopCommandBus{}, nil
	}
	log.Printf("Using Postgres command bus on channel scion_broker_cmd")
	return bus, nil
}

// startPostgresCommandBus starts the Postgres command bus (a test seam).
var startPostgresCommandBus = func(ctx context.Context, dsn string, ownsLocally func(string) bool, reconcile func(context.Context, string), logger *slog.Logger) (hub.CommandBus, error) {
	return hub.NewPostgresCommandBus(ctx, dsn, ownsLocally, reconcile, logger)
}

// initWebServer creates and configures the Web server. The provided context is
// threaded to the event publisher so that the Postgres LISTEN/NOTIFY goroutine
// is cancelled cleanly on shutdown, preventing connection leaks.
func initWebServer(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, devAuthToken string, adminMode bool, maintenanceMessage string, requestLogger *slog.Logger, dbRec dbmetrics.Recorder) (*hub.WebServer, error) {
	webHost := cfg.Hub.Host
	if webHost == "" {
		webHost = "0.0.0.0"
	}

	// Refuse to start when dev auth is enabled on a non-loopback interface.
	// Dev auth auto-logs in every request as admin — exposing it on a public
	// address would create an unauthenticated admin endpoint.
	if devAuthToken != "" && !hub.IsLoopbackHost(webHost) {
		return nil, fmt.Errorf(
			"dev auth cannot be enabled when the server is bound to a non-loopback address (%s). "+
				"Dev auth auto-logs in all requests as admin and must only be used on localhost. "+
				"Either bind to 127.0.0.1/::1/localhost (--host 127.0.0.1) or disable dev auth", webHost)
	}

	// Allow env var overrides for session/OAuth config
	sessionSecret := resolveSessionSecret()
	baseURL := webBaseURL
	if baseURL == "" {
		baseURL = os.Getenv("SCION_SERVER_BASE_URL")
	}
	if baseURL == "" {
		baseURL = fmt.Sprintf("http://localhost:%d", webPort)
	}

	// Construct proxy authenticator for the web server when auth mode is "proxy".
	// This mirrors the construction in initHubServer — both need the same authenticator.
	var webProxyAuth hub.ProxyAuthenticator
	if cfg.Auth.Mode == "proxy" {
		if cfg.Auth.Proxy == nil {
			return nil, fmt.Errorf("auth.mode=proxy requires auth.proxy configuration")
		}
		switch cfg.Auth.Proxy.Provider {
		case "iap":
			if cfg.Auth.Proxy.IAP == nil || cfg.Auth.Proxy.IAP.Audience == "" {
				return nil, fmt.Errorf("auth.proxy.iap.audience is required when auth.mode=proxy and provider=iap")
			}
			webProxyAuth = &hub.IAPAuthenticator{
				Audience: cfg.Auth.Proxy.IAP.Audience,
				Issuer:   cfg.Auth.Proxy.IAP.Issuer,
				JWKSURL:  cfg.Auth.Proxy.IAP.JWKSURL,
			}
		case "jwt":
			jwtAuth, err := newJWTProxyAuthenticator(cfg.Auth.Proxy.JWT)
			if err != nil {
				return nil, err
			}
			webProxyAuth = jwtAuth
		default:
			return nil, fmt.Errorf("unsupported auth.proxy.provider: %q", cfg.Auth.Proxy.Provider)
		}
	}

	// Same configured platform/transport auth service account that guards
	// Server.provisionUser and GoogleIdentityResolver.Resolve (see
	// initHubServer): the web proxy-auth path does its own find-or-create and
	// needs the identical check.
	//
	// cfg.Auth is GlobalConfig.Auth, a DevAuthConfig value (not a pointer),
	// so cfg.Auth itself is never nil here or anywhere else in this file
	// (see, e.g., the unguarded cfg.Auth.Enabled / cfg.Auth.Mode reads
	// throughout initHubServer and initWebServer). Only its nested pointer
	// fields — Transport and Proxy — can be nil, which is what the checks
	// here and elsewhere in this file guard against.
	var webPlatformAuthSA string
	if cfg.Auth.Transport != nil {
		webPlatformAuthSA = cfg.Auth.Transport.PlatformAuthSA
	}

	// The same proxy driver principals the hub gets (WS2), checked the same
	// way, so the web server never runs with a list the hub would refuse.
	webProxyDrivers, webDriversErr := config.ValidateProxyDriverConfig(cfg.Auth.Mode, cfg.Auth.Proxy.ProxyDriverPrincipals())
	if webDriversErr != nil {
		return nil, webDriversErr
	}

	webCfg := hub.WebServerConfig{
		Port:                 webPort,
		Host:                 webHost,
		AssetsDir:            webAssetsDir,
		SessionSecret:        sessionSecret,
		BaseURL:              baseURL,
		DevAuthToken:         devAuthToken,
		AuthMode:             cfg.Auth.Mode,
		AdminMode:            adminMode,
		MaintenanceMessage:   maintenanceMessage,
		EnableTestLogin:      enableTestLogin,
		EnableDebugEndpoints: enableDebugEndpoints,
		ProxyAuthenticator:   webProxyAuth,
		PlatformAuthSA:       webPlatformAuthSA,
		SlowRequestThreshold: cfg.SlowRequestThreshold,
		PerfTrace:            cfg.Hub.PerfTrace,

		ProxyDriverPrincipals:    webProxyDrivers,
		ProxyDriverTokenAudience: cfg.Auth.Proxy.ProxyDriverTokenAudience(),
	}
	if enableDebugEndpoints {
		slog.Warn("Diagnostic endpoints are enabled (--enable-debug-endpoints). Use for local development only.")
	}
	if enableTestLogin {
		slog.Warn("Test login endpoint is enabled (--enable-test-login). This allows bypass of authentication and MUST NOT be used in production!")
	}
	webSrv := hub.NewWebServer(webCfg)
	webSrv.SetRequestLogger(requestLogger)

	// Create shared event publisher for real-time SSE
	eventPub, err := newEventPublisher(ctx, cfg, dbRec)
	if err != nil {
		return nil, err
	}
	webSrv.SetEventPublisher(eventPub)

	// Wire Hub services into WebServer if Hub is enabled
	if hubSrv != nil {
		hubSrv.SetEventPublisher(eventPub)
		startSettingsPropagation(ctx, hubSrv, eventPub)
		webSrv.SetAccessSettingsProvider(hubSrv)
		webSrv.SetProfilingSettingsProvider(hubSrv)
		webSrv.SetOAuthService(hubSrv.GetOAuthService())
		webSrv.SetStore(hubSrv.GetStore())
		webSrv.SetUserTokenService(hubSrv.GetUserTokenService())
		webSrv.SetMaintenanceState(hubSrv.GetMaintenanceState())
		webSrv.SetDemotionSafe(hubSrv.GetDemotionSafe())
		webSrv.SetAuthzService(hubSrv.GetAuthzService())
		webSrv.SetTestIdentityWebSessions(hubSrv)
		webSrv.MountHubAPI(hubSrv.Handler(), hubSrv.CleanupBackgroundResources)

		localHubSrv := hubSrv
		webSrv.SetHubHealthProvider(func(ctx context.Context) interface{} {
			return localHubSrv.GetHealthInfo(ctx)
		})
	}

	return webSrv, nil
}

// logSharedDirStorageStartup is the pure half of the Phase 2 item 5 startup
// summary: given a resolved server.shared_dir_storage config, it warns
// about any set-but-ignored NFS fields and logs exactly one
// resolved-layout line, via logf so it's testable without a real logger. A
// nil sdCfg (server.shared_dir_storage absent) logs nothing.
func logSharedDirStorageStartup(sdCfg *config.V1SharedDirStorageConfig, logf func(format string, args ...interface{})) {
	if ignored := sdCfg.IgnoredNFSFields(); len(ignored) > 0 {
		logf("Warning: server.shared_dir_storage.nfs sets %s, which shared_dir_storage does not use (design deploy-config-explore §3.2.1)",
			strings.Join(ignored, ", "))
	}
	if summary := sdCfg.ResolvedLayoutSummary(); summary != "" {
		logf("server.shared_dir_storage resolved layout: %s", summary)
	}
}

var logSharedDirStorageStartupGuard sync.Once

// loadAndLogSharedDirStorageStartup is logSharedDirStorageStartupOnce's
// impure half, factored out so a test can call it directly -- as many times
// as it likes, with a captured logf -- without the once-per-process guard
// making every call after the first a no-op. It loads global settings the
// same env-free, global-only way the Start path does (config.LoadGlobalSettingsWithOverlay,
// never LoadEffectiveSettings, so this can never be influenced by a
// project's own settings.yaml) and forwards to logSharedDirStorageStartup.
//
// A settings-load error here never fails startup, but if the file that
// failed to load plausibly configured shared_dir_storage at all, that is
// worth one warning line at startup -- the broker's Start path
// and the hub's resolvers both fail closed on this same condition once a
// request/agent-start actually needs it, so this is the heads-up an
// operator sees before that happens.
func loadAndLogSharedDirStorageStartup(logf func(format string, args ...interface{})) {
	globalSettings, _, gErr := config.LoadGlobalSettingsWithOverlay()
	if gErr != nil {
		if config.GlobalSettingsMentions("shared_dir_storage") {
			logf("Warning: server.shared_dir_storage: global settings failed to load (%v); "+
				"shared_dir_storage will not take effect until this is fixed", gErr)
		}
		return
	}
	if globalSettings == nil {
		return
	}
	if globalSettings.Server != nil {
		logSharedDirStorageStartup(globalSettings.Server.SharedDirStorage, logf)
	}
	logSharedDirStorageOverridesStartup(globalSettings, logf)
	logHomeStorageStartup(globalSettings, logf)
}

// logHomeStorageStartup logs a warning for each invalid home storage value
// and each home_storage_backend "nfs" on a non-Kubernetes entry, and one
// line per profile whose resolved home storage is nfs. It checks
// configuration only; it never fails startup and never touches the export.
// A broker host mount needed by the broker leaf mode is checked when an
// agent using it starts, so a missing mount never affects startup or
// agents on other runtimes. The hub.k8s_nfs_home experiment is decided by
// the hub at each dispatch and is not shown here.
func logHomeStorageStartup(gs *config.VersionedSettings, logf func(format string, args ...interface{})) {
	if gs == nil {
		return
	}
	if gs.Server != nil {
		if err := gs.Server.HomeStorage.Validate(); err != nil {
			logf("Warning: %v", err)
		}
		for _, w := range config.HomeStorageWindowWarnings(gs.Server.HomeStorage) {
			logf("Warning: %s", w)
		}
	}
	for _, e := range config.ValidateHomeStorageOverrides(gs.Runtimes, gs.Profiles) {
		logf("Warning: %s", e.Error())
	}
	for _, w := range config.HomeStorageIgnoredWarnings(gs.Runtimes, gs.Profiles) {
		logf("Warning: %s", w)
	}
	names := make([]string, 0, len(gs.Profiles))
	for name := range gs.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := gs.ResolveHomeStorage(name)
		if r.Backend != config.HomeStorageBackendNFS {
			continue
		}
		rt := gs.Profiles[name].Runtime
		if entry, ok := gs.Runtimes[rt]; ok && !config.IsKubernetesRuntimeEntry(rt, entry) {
			continue
		}
		logf("home_storage for profile %s: backend=nfs (from %s), leaf=%s", name, r.BackendSource, r.Leaf)
	}
}

// logSharedDirStorageOverridesStartup logs one line per profile whose
// shared-dir storage backend comes from a profile or runtime entry
// shared_dir_storage_backend override, one per flat Runtime Broker
// instance whose backend comes from its runtime_target, and a warning per
// invalid override and per instance whose backend differs from the one
// plugins resolve (config.VersionedSettings.FlatInstanceSharedDirStorageReport).
// An instance that selects nfs without a complete nfs block is refused when
// the instances are loaded (config.LoadRuntimeBrokerInstances).
// It checks configuration only and never touches the filesystem: an nfs
// override whose export is not mounted on this host is reported when an
// agent using it starts, not here, so a missing mount never affects startup
// or agents that use the local backend. Overrides set through the hub's
// settings API after startup are not in this summary; they apply to the
// next agent start.
func logSharedDirStorageOverridesStartup(gs *config.VersionedSettings, logf func(format string, args ...interface{})) {
	if gs == nil {
		return
	}
	var global *config.V1SharedDirStorageConfig
	if gs.Server != nil {
		global = gs.Server.SharedDirStorage
	}
	for _, e := range config.ValidateSharedDirStorageBackends(gs.Runtimes, gs.Profiles, global) {
		logf("Warning: %s", e.Error())
	}
	names := make([]string, 0, len(gs.Profiles))
	for name := range gs.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg, source := gs.ResolveSharedDirStorage(name)
		if cfg == nil || source == config.SharedDirStorageGlobalSource {
			continue
		}
		line := fmt.Sprintf("shared_dir_storage for profile %s: backend=%s (from %s)", name, sharedDirBackendLabel(cfg), source)
		if summary := cfg.ResolvedLayoutSummary(); summary != "" {
			line = fmt.Sprintf("shared_dir_storage for profile %s: %s (from %s)", name, summary, source)
		}
		logf("%s", line)
	}
	// Per-dir entries: one line per profile and shared dir whose backend
	// comes from a shared_dir_storage_backends entry.
	for _, name := range names {
		dirSet := map[string]bool{}
		p := gs.Profiles[name]
		for dir := range p.SharedDirStorageBackends {
			dirSet[dir] = true
		}
		if rt, ok := gs.Runtimes[p.Runtime]; ok {
			for dir := range rt.SharedDirStorageBackends {
				dirSet[dir] = true
			}
		}
		dirs := make([]string, 0, len(dirSet))
		for dir := range dirSet {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			backend, source, perDir := gs.ResolveSharedDirStorageBackend(name, dir)
			if !perDir {
				continue
			}
			logf("shared_dir_storage for profile %s, shared dir %s: backend=%s (from %s)", name, dir, backend, source)
		}
	}
	// Flat Runtime Broker instances: their runtime_target overrides, and a
	// warning for each instance whose backend differs from the one plugins
	// resolve from the global settings.
	lines, warnings := gs.FlatInstanceSharedDirStorageReport()
	for _, w := range warnings {
		logf("Warning: %s", w)
	}
	for _, line := range lines {
		logf("%s", line)
	}
}

// sharedDirBackendLabel is the backend a resolved config selects, with
// nil and "" shown as local.
func sharedDirBackendLabel(cfg *config.V1SharedDirStorageConfig) string {
	if cfg != nil && cfg.Backend == "nfs" {
		return "nfs"
	}
	return "local"
}

// logSharedDirStorageStartupOnce calls loadAndLogSharedDirStorageStartup
// exactly once per process, regardless of how many of the broker and
// hub-only startup paths call it.
func logSharedDirStorageStartupOnce() {
	logSharedDirStorageStartupGuard.Do(func() {
		loadAndLogSharedDirStorageStartup(log.Printf)
	})
}

// sharedDirStorageStartupLogWanted reports whether this process should emit
// the shared_dir_storage startup summary (Phase 2 item 5). NFS shared dirs
// matter independently to a runtime broker (it creates and
// mounts them) and to the hub (file browser, project deletion), so either
// one being enabled is reason enough to want the summary logged -- a
// hub-only process, a broker-only process, and a combined hub+broker
// process must all log it exactly once (logSharedDirStorageStartupOnce's
// sync.Once handles the "exactly once" part regardless of how many of these
// end up true in the same process).
func sharedDirStorageStartupLogWanted(brokerEnabled, hubEnabled bool) bool {
	return brokerEnabled || hubEnabled
}

// startRuntimeBroker initializes and starts the runtime broker server.
// colocatedBrokerRegisters reports whether this process registers a
// co-located (embedded) runtime broker with its own Hub. It is the single
// condition shared by the early hubSrv.ExpectEmbeddedBroker() call and the
// registration in startRuntimeBroker: if the two drifted, an expected
// registration that never runs would stall hub-default passthrough creates
// for the full wait, or a registration that was not expected would reopen the
// startup window.
func colocatedBrokerRegisters(cfg *config.GlobalConfig, s store.Store) bool {
	return enableHub && cfg.RuntimeBroker.Enabled && !simulateRemoteBroker && s != nil
}

// refuseErrorRuntimeAtStartup returns a non-nil error when rt — the broker's
// own default runtime, as resolved once at startup by runtime.GetRuntime — is
// an *runtime.ErrorRuntime for a substrate default runtime that failed
// deterministic config validation, i.e. its error matches
// runtime.ErrSubstrateProfileInvalid. The scope is substrate only.
//
// GetRuntime never returns an error or nil: a construction or validation
// failure comes back as a Runtime whose every method returns the stored
// error. For a substrate profile, a config validation failure is a settings
// problem — a profile that fails ValidateOperatorOnlySubstrateProfile, or an
// operator-defined runtime block that fails NewSubstrateRuntime's config
// checks (required endpoints, substrate.Validate) — that a running broker
// can never recover from, and that also governs where the bootstrap payload
// and the actor's egress are sent. Refuse to start instead of coming up
// looking healthy; the operator fixes the settings and restarts.
//
// Every other *ErrorRuntime does not block startup: the broker starts
// degraded exactly as it always has, logging the "error" runtime name,
// because those failures can be transient or environmental. That includes a
// substrate runtime whose construct-time dependencies failed (building the
// Kubernetes client, or substrate.Dial's trust-bundle/CA load and API dial,
// e.g. on an API-server blip at boot) — refusing on those would turn a
// transient outage into a boot crash loop. It also includes, for example, a
// Kubernetes client that fails Verify at startup, or a missing container
// CLI. The broker starts degraded instead, but that degraded state is not
// self-healing: the default runtime is resolved once here and is not
// rebuilt until the broker process restarts. /healthz reports status
// "degraded" with checks["runtime"] = "unavailable" but still answers 200,
// so a liveness probe does not restart the broker; /readyz returns 503, and
// the Broker /healthz and /readyz uptime checks alert. Operators restart the
// broker to rebuild the runtime once the cause is fixed.
//
// Named profiles other than the default are unaffected: those are resolved
// lazily, per request, and this check only ever sees the one runtime
// GetRuntime(serverRuntimeProjectPath, "") resolves to at startup. A per-request profile whose
// construction fails is not memoized on the error, so it retries
// construction on the next request rather than staying degraded until a
// restart (see resolveManagerForOpts).
func refuseErrorRuntimeAtStartup(rt runtime.Runtime) error {
	er, ok := rt.(*runtime.ErrorRuntime)
	if !ok || !errors.Is(er.Err, runtime.ErrSubstrateProfileInvalid) {
		return nil
	}
	return fmt.Errorf("runtime broker: configured substrate runtime failed config validation: %w", er.Err)
}

// resolveBrokerDefaultRuntime resolves the broker's default runtime with
// getRuntime (runtime.GetRuntime in production), applies
// refuseErrorRuntimeAtStartup, and logs the runtime the broker will use. It
// is the first step of startRuntimeBroker; the returned runtime is the one
// the broker's manager is built on, so the refusal cannot be skipped without
// also losing the runtime itself.
func resolveBrokerDefaultRuntime(getRuntime func(projectPath, profileName string) runtime.Runtime, logf func(format string, args ...interface{})) (runtime.Runtime, error) {
	rt := getRuntime(serverRuntimeProjectPath, "")
	if err := refuseErrorRuntimeAtStartup(rt); err != nil {
		return nil, err
	}
	logf("Runtime broker using runtime: %s", rt.Name())
	return rt, nil
}

func startRuntimeBroker(ctx context.Context, cmd *cobra.Command, cfg *config.GlobalConfig, hubSrv *hub.Server, webSrv *hub.WebServer, s store.Store, hubEndpoint string, hubEndpointSrc hubEndpointSource, devAuthToken string, brokerSettings *config.Settings, brokerDefaultProfile *string, globalDir string, requestLogger, messageLogger *slog.Logger, wg *sync.WaitGroup, errCh chan error, shutdownContext func() (context.Context, context.CancelFunc)) error {
	// Flat Runtime Broker instances (server.broker.instances), strictly
	// loaded. A process with instances hosts them through the flat host
	// (co-located with the Hub, or remote with instance-scoped credentials
	// validated on every start) and never the legacy identity.
	flatInstances, err := loadServerRuntimeBrokerInstances(cfg, serverConfigPath)
	if err != nil {
		return err
	}
	if len(flatInstances) > 0 {
		if warning := flatInstancesStateDirWarning(cfg); warning != "" {
			log.Printf("WARNING: %s", warning)
		}
		autoProvide := serverAutoProvide
		if enableHub && !cmd.Flags().Changed("auto-provide") {
			autoProvide = true
			if vs, _, vsErr := loadServerEffectiveSettings(); vsErr == nil && vs != nil && vs.Server != nil && vs.Server.Broker != nil && vs.Server.Broker.AutoProvide != nil {
				autoProvide = *vs.Server.Broker.AutoProvide
			}
		}
		return startFlatRuntimeBrokerHost(ctx, flatHostParams{
			cfg: cfg, hubSrv: hubSrv, webSrv: webSrv, store: s, instances: flatInstances,
			hubEndpoint: hubEndpoint, hubEndpointSrc: hubEndpointSrc, devAuthToken: devAuthToken,
			settings: brokerSettings, globalDir: globalDir, autoProvide: autoProvide,
			requestLogger: requestLogger, messageLogger: messageLogger, wg: wg, errCh: errCh,
			shutdownContext: shutdownContext,
		})
	}
	warnUnhostedFlatIdentities(globalDir)
	rt, err := resolveBrokerDefaultRuntime(runtime.GetRuntime, log.Printf)
	if err != nil {
		return err
	}
	statelessCloudRunBroker := enableHub && !simulateRemoteBroker && rt != nil && rt.Name() == "cloudrun"

	mgr := agent.NewManager(rt)
	settings := brokerSettings

	// Try loading versioned settings to get broker identity from server.broker
	versionedSettings, _, vsErr := loadServerEffectiveSettings()
	var vsBroker *config.V1BrokerConfig
	if vsErr != nil {
		log.Printf("Warning: failed to load settings; broker identity from server.broker is not applied: %v", vsErr)
	} else if versionedSettings != nil && versionedSettings.Server != nil {
		vsBroker = versionedSettings.Server.Broker
	}

	// Resolve broker ID
	defaultBrokerID := ""
	if statelessCloudRunBroker {
		var deriveErr error
		defaultBrokerID, deriveErr = deriveCloudRunLogicalBrokerID(versionedSettings, rt)
		if deriveErr != nil {
			return fmt.Errorf("stateless Cloud Run broker requires a derivable broker ID: %w", deriveErr)
		}
	}
	brokerID, err := resolveBrokerID(ctx, cfg, settings, vsBroker, globalDir, defaultBrokerID, s)
	if err != nil {
		return err
	}
	// Resolve broker name
	brokerName := resolveBrokerName(cfg, settings, vsBroker)

	// If no explicit name was configured and this is a co-located broker,
	// use a stable human-readable name instead of the hostname fallback.
	if enableHub && !simulateRemoteBroker {
		if hostname, err := os.Hostname(); err == nil && brokerName == hostname {
			brokerName = "Hosted Broker"
		}
	}

	// Enrich logger with broker_id
	slog.SetDefault(slog.Default().With(slog.String(logging.AttrBrokerID, brokerID)))

	// Resolve hub endpoint for the runtime broker
	hubEndpointForRH := resolveHubEndpointForBroker(cfg, settings)

	// Auto-provide defaults
	if enableHub && !cmd.Flags().Changed("auto-provide") {
		if vsBroker != nil && vsBroker.AutoProvide != nil {
			serverAutoProvide = *vsBroker.AutoProvide
		} else {
			serverAutoProvide = true
		}
	}

	// Co-located registration and credential generation
	var inMemoryCreds *brokercredentials.BrokerCredentials
	var colocatedBrokerRegistered bool
	if colocatedBrokerRegisters(cfg, s) {
		rhEndpoint := fmt.Sprintf("http://%s:%d", cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)
		if cfg.RuntimeBroker.Host == "0.0.0.0" {
			rhEndpoint = fmt.Sprintf("http://localhost:%d", cfg.RuntimeBroker.Port)
		}

		effectiveID, regErr := registerGlobalProjectAndBrokerPresence(ctx, s, hubSrv.BrokerPresent, brokerID, brokerName, rhEndpoint, rt, serverAutoProvide, brokerSettings, loadBrokerRegistrationWorkspaceStorage(), loadBrokerProfileTypeSettings())
		if regErr != nil {
			// ERROR, not a warning: the co-located broker is how this process
			// runs agents. Losing it silently left the Hub reporting healthy
			// with zero brokers while every agent create failed 422 (no
			// runtime broker available). EmbeddedBrokerRegistrationFailed
			// also feeds /healthz (GetHealthInfo) and the admin health
			// summary (see Server.checkColocatedBrokerHealth) so this state
			// is visible there too, not just in the boot log. /readyz is
			// intentionally not affected — see checkColocatedBrokerHealth.
			// There is no retry, so this does not self-heal: it persists
			// until the broker configuration is fixed and the process is
			// restarted.
			// broker_id is already attached by the slog.SetDefault(...With(...))
			// call above; do not pass it again here (duplicate key in JSON output).
			slog.Error("Co-located broker registration failed; the Hub has no embedded broker and agent dispatch will fail until the broker configuration is fixed and the server is restarted",
				"error", regErr)
			hubSrv.EmbeddedBrokerRegistrationFailed(regErr)
		} else {
			colocatedBrokerRegistered = true
			if effectiveID != brokerID {
				log.Printf("Broker ID updated from %s to %s (name-based dedup)", brokerID, effectiveID)
				if err := adoptDedupedBrokerID(globalDir, effectiveID, log.Printf); err != nil {
					return err
				}
				brokerID = effectiveID
			}
			log.Printf("Registered global project with runtime broker %s (endpoint: %s, autoProvide: %v)", brokerName, rhEndpoint, serverAutoProvide)
			// Record the reservations of the agents the co-located broker
			// still runs (its row may have been re-created); idempotent.
			hubSrv.BackfillBrokerOnRegistration(ctx, brokerID)
			if statelessCloudRunBroker {
				hubSrv.SetStatelessEmbeddedBrokerID(brokerID)
			} else {
				hubSrv.SetEmbeddedBrokerID(brokerID)
			}
			hubSrv.SetLocalImageChecker(rt)
		}

		// Generate or retrieve credentials for co-located mode (idempotent).
		// Uses GenerateAndStoreSecret which returns the existing secret if one
		// is already stored, so multiple Cloud Run instances share the same key.
		if authSvc := hubSrv.GetBrokerAuthService(); authSvc != nil {
			secretKeyB64, secretErr := authSvc.GenerateAndStoreSecret(ctx, brokerID)
			if secretErr != nil {
				log.Printf("Warning: failed to generate/retrieve secret for co-located broker: %v", secretErr)
			} else {
				log.Printf("Broker secret ready for co-located control channel (idempotent)")
				inMemoryCreds = &brokercredentials.BrokerCredentials{
					BrokerID:     brokerID,
					SecretKey:    secretKeyB64,
					HubEndpoint:  hubEndpointForRH,
					RegisteredAt: time.Now(),
				}
			}
		} else {
			log.Printf("Warning: BrokerAuthService not available, skipping co-located broker credentials")
		}
	}

	// Auto-compute ContainerHubEndpoint (see computeContainerHubEndpoint).
	// We fall back to the legacy host.docker.internal (host networking) path
	// when the escape hatch SCION_FORCE_HOST_NETWORK is set or the Docker
	// daemon lacks host-gateway support.
	rtName := ""
	if rt != nil {
		rtName = rt.Name()
	}
	chRes := brokerContainerHubConfig(cfg, brokerContainerHubParams{
		RuntimeName:             rtName,
		BrokerHubEndpoint:       hubEndpointForRH,
		PublicHubEndpoint:       hubEndpoint,
		PublicHubEndpointSource: hubEndpointSrc,
		HostGatewayProbe:        func() bool { return runtime.DockerSupportsHostGateway(ctx, "") },
	}, log.Printf)
	containerHubEndpoint := chRes.Endpoint

	if rt != nil && rt.Name() == "container" && containerHubEndpoint != "" {
		exists, checkErr := runtime.AppleDNSRuleExists(ctx, runtime.AppleDNSHostname)
		if checkErr != nil {
			log.Printf("WARNING: could not check Apple Container DNS status: %v", checkErr)
		} else if exists {
			log.Printf("Apple Container DNS rule already configured: %s → %s", runtime.AppleDNSHostname, runtime.AppleDNSIP)
		} else {
			log.Printf("Apple Container runtime detected. To enable agent connectivity, run once:\n"+
				"  sudo container system dns create %s --localhost %s\n"+
				"  See: https://googlecloudplatform.github.io/scion/",
				runtime.AppleDNSHostname, runtime.AppleDNSIP)
		}
	}

	// NFS workspace storage: lets the broker check (and, with
	// server.workspace_storage.nfs.auto_mount, mount) the configured shares.
	// Nil when the backend is not nfs. Read from the broker's global
	// settings only, like shared_dir_storage: a project picked up from the
	// working directory does not decide what the broker mounts.
	var brokerNFS *config.V1NFSConfig
	var workspaceStorageBackend string
	if globalVS, _, gErr := config.LoadGlobalSettings(); gErr != nil {
		log.Printf("WARNING: NFS mount checks disabled: loading global settings: %v", gErr)
	} else {
		workspaceStorageBackend = brokerWorkspaceStorageBackend(globalVS)
		var nfsWarning string
		brokerNFS, nfsWarning = brokerNFSConfig(globalVS)
		if nfsWarning != "" {
			log.Printf("WARNING: %s", nfsWarning)
		}
		if warning := brokerWorkspaceStorageWarning(globalVS); warning != "" {
			log.Printf("WARNING: %s", warning)
		}
	}
	if warning := brokerInotifyWarning(hostedMode, rt != nil && brokerAgentsShareHostUser(nil, rt.Name()), hostlimits.DefaultReader()); warning != "" {
		log.Printf("WARNING: %s", warning)
	}

	// The broker's state directory: <server.broker.state_dir>/<broker-id>,
	// or the default when the setting is empty (validated at preflight).
	rhStateDir, err := brokerStateDir(cfg.RuntimeBroker.StateDir, brokerID)
	if err != nil {
		return err
	}

	// Create Runtime Broker server configuration
	rhCfg := runtimebroker.ServerConfig{
		Port:                          cfg.RuntimeBroker.Port,
		Host:                          cfg.RuntimeBroker.Host,
		ReadTimeout:                   cfg.RuntimeBroker.ReadTimeout,
		WriteTimeout:                  cfg.RuntimeBroker.WriteTimeout,
		HubEndpoint:                   hubEndpointForRH,
		BrokerID:                      brokerID,
		BrokerName:                    brokerName,
		CORSEnabled:                   cfg.RuntimeBroker.CORSEnabled,
		CORSAllowedOrigins:            cfg.RuntimeBroker.CORSAllowedOrigins,
		CORSAllowedMethods:            cfg.RuntimeBroker.CORSAllowedMethods,
		CORSAllowedHeaders:            cfg.RuntimeBroker.CORSAllowedHeaders,
		CORSMaxAge:                    cfg.RuntimeBroker.CORSMaxAge,
		AllowContainerScriptHarnesses: cfg.RuntimeBroker.AllowContainerScriptHarnesses,
		StateDir:                      rhStateDir,
		NFSConfig:                     brokerNFS,
		StorageBucket:                 brokerStorageBucket(cfg.Storage),
		WorkspaceStorageBackend:       workspaceStorageBackend,
		DefaultProfile:                brokerDefaultProfile,
		Debug:                         enableDebug,
		SlowRequestThreshold:          cfg.SlowRequestThreshold,

		HubEnabled:           hubEndpointForRH != "",
		HubToken:             devAuthToken,
		HostCredentials:      config.HostCredentialsEligible(hostedMode, devAuthToken != ""),
		WorkstationMode:      !hostedMode,
		TemplateCacheDir:     templateCacheDir,
		TemplateCacheMaxSize: templateCacheMax,

		ControlChannelEnabled: hubEndpointForRH != "" && !statelessCloudRunBroker,
		HeartbeatEnabled:      hubEndpointForRH != "" && !statelessCloudRunBroker,

		InMemoryCredentials:  inMemoryCreds,
		BrokerAuthEnabled:    true,
		BrokerAuthStrictMode: true,
	}
	chRes.applyTo(&rhCfg)

	// In co-located mode, hand the broker the Hub's storage backend so that a
	// local filesystem backend is read directly (zero-copy) instead of being
	// hydrated over HTTP. A non-local backend is left for cache-based hydration.
	if inMemoryCreds != nil && hubSrv != nil {
		rhCfg.ColocatedStorage = hubSrv.GetStorage()
	}

	// In co-located mode on any DB-backed Hub, install the global settings
	// overlay so the broker sees the Hub's stored runtimes, profiles,
	// harness_configs and image_registry (see installColocatedSettingsOverlay).
	// Ordering invariant: the broker's default runtime (rt, from
	// resolveBrokerDefaultRuntime) is resolved above, before the overlay
	// exists, so it comes from the settings file and startup never sees an
	// overlay-only active_profile error.
	if installColocatedSettingsOverlay(cfg, hubSrv) {
		// Built-in profile retention (ptone/scion#5113): enabled only for the
		// legacy co-located broker, whose ID is known here (registered above).
		// A failed registration leaves it disabled: the overlay is unchanged.
		enableColocatedProfileReferences(ctx, hubSrv)
	}

	rhSrv := runtimebroker.New(rhCfg, mgr, rt)
	// The broker phase of the process's graceful shutdown bounds the
	// broker's own (ptone/scion#5054).
	rhSrv.SetShutdownContext(shutdownContext)
	rhSrv.SetRequestLogger(requestLogger)
	if messageLogger != nil {
		rhSrv.SetMessageLogger(messageLogger)
	}

	// Wire runtime reload so reloadSettings can swap the broker's container
	// engine without a full server restart (fixes onboarding wizard flow).
	if hubSrv != nil && colocatedBrokerRegistered {
		hubSrv.SetRuntimeReloadFunc(func() bool {
			newRT := runtime.GetRuntime(serverRuntimeProjectPath, "")
			if newRT.Name() == rhSrv.RuntimeName() {
				return false
			}
			rhSrv.SwapRuntime(newRT)
			hubSrv.SetLocalImageChecker(newRT)
			return true
		})
	}

	if webSrv != nil {
		webSrv.SetBrokerHealthProvider(func(ctx context.Context) interface{} {
			return rhSrv.GetHealthInfo(ctx)
		})
	}

	log.Printf("Starting Runtime Broker API server on %s:%d",
		cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := rhSrv.Start(ctx); err != nil {
			errCh <- fmt.Errorf("runtime broker server error: %w", err)
		}
	}()

	// Start internal heartbeat loop for co-located operation.
	// Skip for stateless Cloud Run brokers — the Hub manages the
	// logical broker status directly and no container daemon is present.
	if colocatedBrokerRegistered && !statelessCloudRunBroker {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			prevOnline := rhSrv.IsControlChannelConnected()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					ccUp := rhSrv.IsControlChannelConnected()
					status := store.BrokerStatusOnline
					if !ccUp {
						status = store.BrokerStatusOffline
					}
					if ccUp != prevOnline {
						if ccUp {
							log.Printf("Co-located heartbeat: control channel restored, writing online for %s", brokerName)
						} else {
							log.Printf("Co-located heartbeat: control channel down, writing offline for %s", brokerName)
						}
						prevOnline = ccUp
					}
					if err := s.UpdateRuntimeBrokerHeartbeat(ctx, brokerID, status); err != nil {
						log.Printf("Warning: failed to update internal heartbeat for %s: %v", brokerName, err)
					}
				}
			}
		}()
	} else if simulateRemoteBroker && enableHub && cfg.RuntimeBroker.Enabled {
		log.Printf("Simulating remote broker: skipping automatic global project registration")
	}

	return nil
}

// pluginChannelID returns the channel identifier reported by a broker plugin
// via GetInfo().ChannelID. Returns "" if the plugin does not report one, in
// which case the bus Name is used for channel routing.
func pluginChannelID(pluginMgr *scionplugin.Manager, name string) string {
	raw, err := pluginMgr.Get(scionplugin.PluginTypeBroker, name)
	if err != nil {
		return ""
	}
	type infoer interface {
		GetInfo() (*scionplugin.PluginInfo, error)
	}
	rpc, ok := raw.(infoer)
	if !ok {
		return ""
	}
	info, err := rpc.GetInfo()
	if err != nil || info == nil {
		return ""
	}
	return info.ChannelID
}

// isObserverBroker determines whether a broker plugin should be treated as an
// observer (fire-and-forget on publish errors). It checks the plugin's
// capabilities first, then falls back to a name-based heuristic.
func isObserverBroker(pluginMgr *scionplugin.Manager, name string) bool {
	raw, err := pluginMgr.Get(scionplugin.PluginTypeBroker, name)
	if err == nil {
		type infoer interface {
			GetInfo() (*scionplugin.PluginInfo, error)
		}
		if rpc, ok := raw.(infoer); ok {
			if info, infoErr := rpc.GetInfo(); infoErr == nil && info != nil {
				for _, cap := range info.Capabilities {
					if strings.EqualFold(cap, "observer") {
						return true
					}
				}
				return false
			}
		}
	}
	// Heuristic fallback: names containing "log" or "debug" are observers.
	lower := strings.ToLower(name)
	return strings.Contains(lower, "log") || strings.Contains(lower, "debug")
}

// initPluginManager creates and loads a plugin manager from versioned settings.
// The secretBackend parameter (may be nil) enables one-shot migration of inline
// secrets to the backend before ResolvePluginConfig strips them.
// The dataStore parameter (may be nil) is used to acquire an advisory lock
// around the inline secrets migration to prevent races across replicas.
func initPluginManager(ctx context.Context, secretBackend secret.SecretBackend, dataStore store.Store) *scionplugin.Manager {
	logger := logging.Subsystem("plugin")
	mgr := scionplugin.NewManager(logger)
	grpcbroker.SetADCSourceConstructor(adcsource.New)
	mgr.NewGRPCBrokerAdapter = grpcbroker.NewAdapterFromEntry

	vs, err := loadServerVersionedSettings()
	if err != nil {
		log.Printf("Warning: failed to load settings; plugins are not loaded: %v", err)
		return mgr
	}
	if vs == nil || vs.Server == nil || vs.Server.Plugins == nil {
		return mgr
	}

	pluginsDir, err := scionplugin.DefaultPluginsDir()
	if err != nil {
		log.Printf("Warning: failed to resolve plugins directory: %v", err)
		return mgr
	}

	// Convert V1PluginsConfig to plugin.PluginsConfig
	pluginsCfg := scionplugin.PluginsConfig{
		Broker: make(map[string]scionplugin.PluginEntry),
	}
	// Migrate plugin secrets under advisory lock to prevent concurrent replicas
	// from racing the one-shot migration (audit finding C6). The lock key stays
	// LockInlineSecretsMigration: it is a persisted name, and renaming it would
	// stop replicas on different versions from excluding each other.
	if secretBackend != nil {
		runWithAdvisoryLock(ctx, dataStore, store.LockInlineSecretsMigration, "plugin secrets migration", func() {
			for name, entry := range vs.Server.Plugins.Broker {
				secretmigration.MigratePluginSecrets(ctx, secretBackend, name, entry.Config, entry.ConfigFile)
			}
		})
	}

	for name, entry := range vs.Server.Plugins.Broker {
		mergedConfig, mergeErr := config.ResolvePluginConfig(entry.ConfigFile, entry.Config)
		if mergeErr != nil {
			log.Printf("Warning: failed to load config file for plugin %q: %v", name, mergeErr)
			mergedConfig = stripSecretKeys(entry.Config)
		}
		if secretBackend != nil {
			mergedConfig = injectPluginSecretsIntoConfig(ctx, secretBackend, name, mergedConfig)
		}
		if entry.ConfigFile != "" {
			if mergedConfig == nil {
				mergedConfig = make(map[string]string)
			}
			mergedConfig["config_file"] = entry.ConfigFile
		}
		pluginsCfg.Broker[name] = scionplugin.PluginEntry{
			Path:          entry.Path,
			Config:        mergedConfig,
			ConfigFile:    entry.ConfigFile,
			SelfManaged:   entry.SelfManaged,
			Mode:          entry.Mode,
			Address:       entry.Address,
			TLSCertFile:   entry.TLSCertFile,
			TLSKeyFile:    entry.TLSKeyFile,
			TLSCAFile:     entry.TLSCAFile,
			TLSSkipVerify: entry.TLSSkipVerify,
			AuthType:      entry.AuthType,
			AuthAudience:  entry.AuthAudience,
		}
	}

	if err := mgr.LoadAll(pluginsCfg, pluginsDir); err != nil {
		log.Printf("Warning: plugin loading encountered errors: %v", err)
	}

	loaded := mgr.ListPlugins()
	if len(loaded) > 0 {
		log.Printf("Loaded %d plugin(s): %v", len(loaded), loaded)
	}

	return mgr
}

var cloudRunLogicalBrokerNamespace = uuid.MustParse("c10f7a0a-6f03-5f9f-8d52-1d98b0fdb001")

// resolveBrokerID determines the broker ID from various sources.
// It checks (in order): versioned settings, legacy settings, global config,
// deterministic Cloud Run ID, DB recovery (single embedded broker), and
// finally generates a new UUID as a last resort.
func resolveBrokerID(ctx context.Context, cfg *config.GlobalConfig, settings *config.Settings, vsBroker *config.V1BrokerConfig, globalDir, defaultBrokerID string, s store.Store) (string, error) {
	var brokerID string
	if vsBroker != nil && vsBroker.BrokerID != "" {
		brokerID = vsBroker.BrokerID
	} else if settings != nil && settings.Hub != nil {
		brokerID = settings.Hub.BrokerID
	}
	if brokerID == "" {
		brokerID = cfg.RuntimeBroker.BrokerID
	}
	if brokerID == "" && defaultBrokerID != "" {
		log.Printf("Using deterministic logical broker ID: %s", defaultBrokerID)
		brokerID = defaultBrokerID
	}

	// Recover from DB: if exactly one embedded broker exists, reuse its ID.
	if brokerID == "" && s != nil {
		embedded, err := s.FindEmbeddedBroker(ctx)
		if err != nil {
			log.Printf("Warning: failed to query database for broker ID recovery: %v", err)
		} else if embedded != nil {
			if brokerID, err = recoverEmbeddedBrokerID(globalDir, embedded.ID, log.Printf); err != nil {
				return "", err
			}
		}
	}

	if brokerID == "" {
		brokerID = api.NewUUID()
		if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
			log.Printf("Warning: failed to persist broker ID to settings: %v", err)
		} else {
			log.Printf("Generated and persisted new broker ID: %s", brokerID)
		}
	}
	// Every source of the ID (configured, deterministic default, recovered,
	// generated) is checked before it is used.
	if err := brokercredentials.ValidateBrokerIDPathElement(brokerID); err != nil {
		return "", fmt.Errorf("broker ID: %w", err)
	}
	return brokerID, nil
}

func deriveCloudRunLogicalBrokerID(settings *config.VersionedSettings, rt runtime.Runtime) (string, error) {
	if settings == nil || rt == nil || rt.Name() != "cloudrun" {
		return "", fmt.Errorf("deriveCloudRunLogicalBrokerID requires cloudrun runtime (got settings=%v, rt=%v)", settings != nil, rt)
	}
	// Scan all profiles to find one backed by a cloudrun runtime with project+region.
	// This avoids requiring active_profile to point to the cloudrun profile, which
	// would make cloudrun the default dispatch profile — conflicting with k8s dispatch.
	// Sort profile names for deterministic iteration: if multiple cloudrun profiles
	// exist, the first alphabetically wins, producing a stable broker ID across restarts.
	profileNames := make([]string, 0, len(settings.Profiles))
	for profileName := range settings.Profiles {
		profileNames = append(profileNames, profileName)
	}
	sort.Strings(profileNames)
	for _, profileName := range profileNames {
		rtConfig, runtimeType, err := settings.ResolveRuntime(profileName)
		if err != nil {
			continue
		}
		projectID, location := resolveCloudRunProjectAndRegion(rtConfig, runtimeType)
		if projectID == "" || location == "" {
			continue
		}
		seed := fmt.Sprintf("cloudrun:%s:%s", projectID, location)
		return uuid.NewSHA1(cloudRunLogicalBrokerNamespace, []byte(seed)).String(), nil
	}
	// Fall back to resolving via active_profile for backward compatibility.
	rtConfig, runtimeType, err := settings.ResolveRuntime("")
	if err != nil {
		return "", fmt.Errorf("deriveCloudRunLogicalBrokerID: failed to resolve runtime: %w", err)
	}
	projectID, location := resolveCloudRunProjectAndRegion(rtConfig, runtimeType)
	if projectID == "" || location == "" {
		return "", fmt.Errorf("deriveCloudRunLogicalBrokerID: project (%q) and region (%q) must both be set in cloudrun runtime config (type: %q)", projectID, location, runtimeType)
	}
	seed := fmt.Sprintf("cloudrun:%s:%s", projectID, location)
	return uuid.NewSHA1(cloudRunLogicalBrokerNamespace, []byte(seed)).String(), nil
}

// resolveCloudRunProjectAndRegion extracts the GCP project ID and region from
// a V1RuntimeConfig for both "cloudrun" and "cloudrun-instances" runtime types.
// Returns empty strings when the runtime type is not a Cloud Run variant or
// when the required nested config is absent.
func resolveCloudRunProjectAndRegion(rtConfig config.V1RuntimeConfig, runtimeType string) (string, string) {
	switch runtimeType {
	case "cloudrun":
		if rtConfig.CloudRun == nil {
			return "", ""
		}
		return strings.TrimSpace(rtConfig.CloudRun.ProjectID), strings.TrimSpace(rtConfig.CloudRun.Location)
	case "cloudrun-instances":
		if rtConfig.CloudRunInstances == nil {
			return "", ""
		}
		return strings.TrimSpace(rtConfig.CloudRunInstances.ProjectID), strings.TrimSpace(rtConfig.CloudRunInstances.Region)
	default:
		return "", ""
	}
}

// resolveBrokerName determines the broker name from various sources.
func resolveBrokerName(cfg *config.GlobalConfig, settings *config.Settings, vsBroker *config.V1BrokerConfig) string {
	var brokerName string
	if vsBroker != nil && vsBroker.BrokerNickname != "" {
		brokerName = vsBroker.BrokerNickname
	} else if vsBroker != nil && vsBroker.BrokerName != "" {
		brokerName = vsBroker.BrokerName
	} else {
		brokerName = settings.Hub.BrokerNickname
	}
	if brokerName == "" {
		brokerName = cfg.RuntimeBroker.BrokerName
	}
	if brokerName == "" {
		if hostname, err := os.Hostname(); err == nil {
			brokerName = hostname
		} else {
			brokerName = "runtime-broker"
		}
	}
	return brokerName
}

// resolveHubListenPort returns the port the co-located hub HTTP server is
// listening on. In combined web+API mode (--enable-web) this is --web-port;
// in standalone hub mode it is --port. Returns 0 when the hub is not
// co-located (enableHub false).
//
// serverEndpointMode.hubListenPort is the single source of truth for the
// hub's listen port. Its callers: resolveBrokerHubEndpointFrom and the
// hub's localhost fallback in resolveHubEndpointFrom (which format it into a
// localhost URL), and, through this function, the broker config's
// HubListenPort (which cloudrunSandboxHubEndpoint uses to construct the
// link-local endpoint for sandboxes). Keeping the derivation there rather
// than duplicating it prevents one caller from drifting when the other is
// updated — a failure whose symptom is agents that start but never register.
func resolveHubListenPort(cfg *config.GlobalConfig) int {
	return currentServerEndpointMode().hubListenPort(cfg)
}

// resolveHubEndpointForBroker determines the Hub endpoint URL for the
// runtime broker's internal communication (heartbeat, control channel).
// In co-located mode (enableHub true), this always resolves to localhost
// so the broker never routes through the external public URL.
func resolveHubEndpointForBroker(cfg *config.GlobalConfig, settings *config.Settings) string {
	return resolveBrokerHubEndpointFrom(cfg, currentServerEndpointMode(), settings.GetHubEndpoint(), log.Printf)
}

// resolveBrokerHubEndpointFrom is resolveHubEndpointForBroker for an
// explicit mode. legacyEndpoint is the settings hub.endpoint; passing ""
// resolves as if that fallback were gone. logf may be nil.
func resolveBrokerHubEndpointFrom(cfg *config.GlobalConfig, m serverEndpointMode, legacyEndpoint string, logf func(string, ...any)) string {
	hubEndpointForRH := cfg.RuntimeBroker.HubEndpoint
	if hubEndpointForRH == "" && m.hubEnabled {
		hubEndpointForRH = fmt.Sprintf("http://localhost:%d", m.hubListenPort(cfg))
		if m.debug && logf != nil {
			logf("Co-located Hub detected: using %s for heartbeat and template hydration", hubEndpointForRH)
		}
	} else if hubEndpointForRH == "" {
		hubEndpointForRH = legacyEndpoint
	}
	return hubEndpointForRH
}

// pluginHubAPIURLFrom returns the URL hub-managed broker plugins use for
// their own API calls to the hub (hub.ServerConfig.PluginHubAPIURL): the
// hub's loopback URL, derived the same way as the co-located runtime
// broker's (resolveBrokerHubEndpointFrom). Such plugins run as child
// processes on the hub's host, so the call never leaves it and never meets a
// proxy in front of the public endpoint. "" when the listen port is unknown.
func pluginHubAPIURLFrom(cfg *config.GlobalConfig, m serverEndpointMode) string {
	port := m.hubListenPort(cfg)
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

// resolveMaintenanceConfig builds the maintenance config from versioned settings
// and environment variables.
func resolveMaintenanceConfig(cfg *config.GlobalConfig) hub.MaintenanceConfig {
	mc := hub.MaintenanceConfig{
		ServiceName:        "scion-hub",
		BinaryDest:         "/usr/local/bin/scion",
		CheckIntervalHours: 6,
		GitHubRepo:         "GoogleCloudPlatform/scion",
	}

	// Pull from versioned settings if available.
	if vs, err := loadServerVersionedSettings(); err == nil {
		// The maintenance build registry is resolved at build time
		// (hub.Server.maintenanceImageRegistry), not here.
		// Collect harness names from configured harness configs.
		for name := range vs.HarnessConfigs {
			mc.Harnesses = append(mc.Harnesses, name)
		}
		if mc.RepoPath == "" {
			mc.RepoPath = vs.WorkspacePath
		}

		// Read new binary auto-update fields from server.maintenance.
		if vs.Server != nil && vs.Server.Maintenance != nil {
			m := vs.Server.Maintenance
			if m.DeploymentTier != "" {
				mc.DeploymentTier = m.DeploymentTier
			}
			if m.ReleaseChannel != "" {
				mc.ReleaseChannel = m.ReleaseChannel
			}
			if m.UpdatePolicy != "" {
				mc.UpdatePolicy = m.UpdatePolicy
			}
			if m.CheckIntervalHours > 0 {
				mc.CheckIntervalHours = m.CheckIntervalHours
			}
			if m.GitHubRepo != "" {
				mc.GitHubRepo = m.GitHubRepo
			}
		}
	} else {
		log.Printf("Warning: failed to load settings; maintenance settings from the settings file are not applied: %v", err)
	}

	// Environment variable overrides.
	// SCION_MAINTENANCE_IMAGE_REGISTRY applies to maintenance builds only
	// (ptone/scion#3916).
	if v := os.Getenv(config.EnvMaintenanceImageRegistry); v != "" {
		mc.ImageRegistryOverride = v
	}
	if v := os.Getenv("SCION_MAINTENANCE_IMAGE_TAG"); v != "" {
		mc.ImageTag = v
	}
	if v := os.Getenv("SCION_MAINTENANCE_RUNTIME"); v != "" {
		mc.RuntimeBin = v
	}
	if v := os.Getenv("SCION_MAINTENANCE_REPO_PATH"); v != "" {
		// Support "path@branch" syntax: /home/scion/scion@feature-branch
		if path, branch, ok := strings.Cut(v, "@"); ok {
			mc.RepoPath = path
			mc.RepoBranch = branch
		} else {
			mc.RepoPath = v
		}
	}
	if v := os.Getenv("SCION_MAINTENANCE_REPO_BRANCH"); v != "" {
		mc.RepoBranch = v
	}
	if v := os.Getenv("SCION_MAINTENANCE_BINARY_DEST"); v != "" {
		mc.BinaryDest = v
	}
	if v := os.Getenv("SCION_MAINTENANCE_SERVICE_NAME"); v != "" {
		mc.ServiceName = v
	}

	// Apply deployment tier defaults if not explicitly configured.
	if mc.DeploymentTier == "" {
		if mc.RepoPath != "" {
			mc.DeploymentTier = "source"
		} else {
			mc.DeploymentTier = "binary"
		}
	}

	// Apply update policy default based on deployment tier.
	if mc.UpdatePolicy == "" {
		if mc.DeploymentTier == "binary" {
			mc.UpdatePolicy = "auto"
		} else {
			mc.UpdatePolicy = "disabled"
		}
	}

	// Enforce minimum check interval.
	if mc.CheckIntervalHours < 1 {
		mc.CheckIntervalHours = 1
	}

	return mc
}

// requireImageRegistryForBroker checks that an image registry is available from
// at least one source before starting the runtime broker. The broker needs a
// registry to pull agent container images; without one, image pulls fail with
// opaque 404 errors at dispatch time. This check fails fast at startup with an
// actionable error instead.
//
// Sources checked: the node env (SCION_IMAGE_REGISTRY) and image_registry in
// the settings file. SCION_MAINTENANCE_IMAGE_REGISTRY applies to maintenance
// builds only and is not a source. A workstation server also counts a saved
// hub value (deferred check in runServerForeground, step 8).
func requireImageRegistryForBroker() error {
	return requireImageRegistryForBrokerAt("")
}

// stripSecretKeys returns a copy of the config map with all secret config keys removed.
func stripSecretKeys(cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg))
	for k, v := range cfg {
		if !config.IsSecretConfigKey(k) {
			out[k] = v
		}
	}
	return out
}

// injectPluginSecrets loads chat integration secrets from the secret backend
// and injects them into the extra credentials map. Respects the fallback chain:
// if the plugin's merged config (file + inline) already has a value for a key,
// the secret backend is not consulted for that key.
func injectPluginSecrets(ctx context.Context, sb secret.SecretBackend, pluginName string, pluginConfig, creds map[string]string) {
	if sb == nil {
		return
	}

	mappings, ok := config.PluginSecretKeyMap[pluginName]
	if !ok {
		return
	}

	hubID := sb.HubID()
	for _, m := range mappings {
		if existing, ok := pluginConfig[m.ConfigKey]; ok && existing != "" {
			continue
		}
		sv, err := sb.Get(ctx, m.SecretKey, store.ScopeHub, hubID)
		if err != nil || sv == nil || sv.Value == "" {
			continue
		}
		creds[m.ConfigKey] = sv.Value
		slog.Info("Injected secret into broker plugin", "secret", m.SecretKey, "plugin", pluginName, "config_key", m.ConfigKey)
	}
}

// injectPluginSecretsIntoConfig loads secrets from the backend directly into
// the plugin's merged config map so they are available when LoadAll calls
// Configure. Without this, plugins that validate required keys (e.g.
// Telegram's bot_token) during Configure would fail because
// ResolvePluginConfig already stripped inline secrets.
func injectPluginSecretsIntoConfig(ctx context.Context, sb secret.SecretBackend, pluginName string, cfg map[string]string) map[string]string {
	if sb == nil {
		return cfg
	}

	mappings, ok := config.PluginSecretKeyMap[pluginName]
	if !ok {
		return cfg
	}

	hubID := sb.HubID()
	for _, m := range mappings {
		if cfg != nil {
			if existing, ok := cfg[m.ConfigKey]; ok && existing != "" {
				continue
			}
		}
		sv, err := sb.Get(ctx, m.SecretKey, store.ScopeHub, hubID)
		if err != nil || sv == nil || sv.Value == "" {
			continue
		}
		if cfg == nil {
			cfg = make(map[string]string)
		}
		cfg[m.ConfigKey] = sv.Value
	}
	return cfg
}

// telemetryGCPProjectFromSettings returns the explicit telemetry.cloud.gcp_project_id
// setting value, or "" if not configured.
func telemetryGCPProjectFromSettings(cfg *config.GlobalConfig) string {
	if cfg.TelemetryConfig == nil || cfg.TelemetryConfig.Cloud == nil {
		return ""
	}
	if cfg.TelemetryConfig.Cloud.GCPProjectID == nil {
		return ""
	}
	return *cfg.TelemetryConfig.Cloud.GCPProjectID
}

// telemetryGCPProjectFromSecret reads the scion-telemetry-gcp-credentials hub
// secret and extracts the project_id from the SA key JSON.
func telemetryGCPProjectFromSecret(ctx context.Context, sb secret.SecretBackend, hubID string) string {
	sw, err := sb.Get(ctx, "scion-telemetry-gcp-credentials", secret.ScopeHub, hubID)
	if err != nil || sw == nil || sw.Value == "" {
		return ""
	}
	return gcputil.ParseProjectID([]byte(sw.Value))
}

// hubExitSequence flushes telemetry providers in reverse registration order.
type hubExitSequence struct {
	flushes []func(context.Context) error
}

func (h *hubExitSequence) addFlush(f func(context.Context) error) {
	h.flushes = append(h.flushes, f)
}

func (h *hubExitSequence) run() {
	for i := len(h.flushes) - 1; i >= 0; i-- {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := h.flushes[i](ctx); err != nil {
			log.Printf("WARNING: hub OTel flush on exit: %v", err)
		}
		cancel()
	}
}
