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
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubmetrics"
	scionplugin "github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/web"
	"github.com/spf13/cobra"
)

func runServerStart(cmd *cobra.Command, args []string) error {
	// 1. Initialize logging
	logCleanups, requestLogger, messageLogger, err := initServerLogging(cmd)
	if err != nil {
		return err
	}
	for _, cleanup := range logCleanups {
		defer cleanup()
	}

	// 2. Load & reconcile config
	cfg, err := loadAndReconcileConfig(cmd)
	if err != nil {
		return err
	}

	// 3. Resolve admin mode settings
	adminMode := cfg.AdminMode
	if v := os.Getenv("SCION_SERVER_ADMIN_MODE"); v != "" {
		adminMode = v == "true" || v == "1" || v == "yes"
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
		if err := config.InitGlobal(harness.EmbedOnlyHarnesses()); err != nil {
			return fmt.Errorf("failed to initialize global config: %w", err)
		}
	} else if !hostedMode {
		// In workstation mode, refresh the default template and harness-configs
		// from the binary's embeds. Hosted mode bootstraps directly into the Hub
		// via BootstrapBundledResources, bypassing local ~/.scion materialization.
		if err := config.UpdateDefaultTemplates(true, harness.EmbedOnlyHarnesses()); err != nil {
			log.Printf("Warning: failed to refresh default templates: %v", err)
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
	}

	// Warn if running from within a project directory instead of the global (~/.scion) context.
	if projectDir, ok := config.FindProjectRoot(); ok {
		if projectDir != globalDir {
			parentDir := filepath.Dir(projectDir)
			fmt.Fprintf(os.Stderr, "\n%s%s WARNING: Server is running from a project directory context (%s)%s\n",
				util.Bold, util.Yellow, parentDir, util.Reset)
			fmt.Fprintf(os.Stderr, "%s%s          The runtime broker will use this project's templates and settings.%s\n",
				util.Bold, util.Yellow, util.Reset)
			fmt.Fprintf(os.Stderr, "%s%s          For machine-wide operation, run the server from outside any project directory.%s\n\n",
				util.Bold, util.Yellow, util.Reset)
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	// 8. Initialize store
	var s store.Store
	if enableHub {
		s, err = initStore(ctx, cfg)
		if err != nil {
			return err
		}
		if closer, ok := s.(io.Closer); ok {
			defer closer.Close()
		}
	}

	// Load settings early so both Hub and Broker can use project-level hub.endpoint.
	brokerSettings, err := config.LoadSettings("")
	if err != nil {
		log.Printf("Warning: failed to load settings: %v", err)
		brokerSettings = &config.Settings{}
	}
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

	// 10. Resolve hub endpoint
	hubEndpoint := resolveHubEndpoint(cfg, brokerSettings)

	// Parse admin emails
	adminEmailList := parseAdminEmails(cfg)

	// 10b. Initialize plugin manager
	pluginMgr := initPluginManager()
	defer pluginMgr.Shutdown()

	// 11. Start Hub
	var hubSrv *hub.Server
	var secretBackend secret.SecretBackend
	var hubDBRec dbmetrics.Recorder
	if enableHub {
		// Initialize secret backend early so signing keys can be loaded from it
		// during hub server creation. This prevents the previous bug where
		// ensureSigningKey always fell through to SQLite because the secret
		// backend was set too late (after hub.New()).
		hubID := cfg.Hub.ResolveHubID()
		var sbErr error
		secretBackend, sbErr = secret.NewBackend(ctx, cfg.Secrets.Backend, s, secret.GCPBackendConfig{
			ProjectID:       cfg.Secrets.GCPProjectID,
			CredentialsJSON: cfg.Secrets.GCPCredentials,
		}, hubID)
		if sbErr != nil {
			log.Printf("Warning: failed to initialize secret backend: %v", sbErr)
		}

		var hubInitErr error
		hubSrv, hubInitErr = initHubServer(ctx, cfg, s, hubEndpoint, devAuthToken, adminEmailList, adminMode, maintenanceMessage, requestLogger, messageLogger, globalDir, pluginMgr, secretBackend)
		if hubInitErr != nil {
			log.Fatalf("Hub server failed to start: %v", hubInitErr)
		}

		// Wire hub OTel metrics export to Cloud Monitoring.
		if cfg.Hub.GCPProjectID != "" {
			mp, mpErr := hubmetrics.NewMeterProvider(ctx, cfg.Hub.GCPProjectID,
				hubmetrics.WithHubID(hubSrv.HubID()),
			)
			if mpErr != nil {
				log.Printf("WARNING: hub metrics export disabled: %v", mpErr)
			} else {
				defer func() {
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = mp.Shutdown(shutdownCtx)
				}()

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

				log.Printf("Hub OTel metrics export enabled (project: %s)", cfg.Hub.GCPProjectID)
			}
		}

		// Wire command bus for cross-node dispatch (B2-4).
		cmdBus := newCommandBus(ctx, cfg, hubSrv)
		hubSrv.SetCommandBus(cmdBus)

		if !enableWeb {
			// Hub runs its own HTTP server (standalone mode).
			eventPub, err := newEventPublisher(ctx, cfg, hubDBRec)
			if err != nil {
				return err
			}
			hubSrv.SetEventPublisher(eventPub)

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
			log.Printf("Hub API will be mounted on Web server (port %d)", webPort)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-ctx.Done()
				hubSrv.CleanupResources(context.Background())
			}()
		}
	}

	// 12. Start Web
	var webSrv *hub.WebServer
	if enableWeb {
		webSrv, err = initWebServer(ctx, cfg, hubSrv, devAuthToken, adminEmailList, adminMode, maintenanceMessage, requestLogger, hubDBRec)
		if err != nil {
			return err
		}

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
	if cfg.RuntimeBroker.Enabled {
		if err := startRuntimeBroker(ctx, cmd, cfg, hubSrv, webSrv, s, hubEndpoint, devAuthToken, brokerSettings, globalDir, requestLogger, messageLogger, &wg, errCh); err != nil {
			return err
		}
	}

	// 14. Set up dispatcher, message broker, and notification dispatcher.
	// This must happen after the ChannelEventPublisher is set (step 11/12)
	// so that StartMessageBroker and StartNotificationDispatcher can
	// create their proxies with the real event publisher.
	if enableHub && hubSrv != nil {
		dispatcher := hubSrv.CreateAuthenticatedDispatcher()
		hubSrv.SetDispatcher(dispatcher)
		log.Printf("Agent dispatcher configured (HTTP-based)")

		// Initialize message broker from versioned settings.
		// Uses FanOutBroker to support multiple simultaneous broker plugins.
		if vs, err := config.LoadVersionedSettings(""); err == nil && vs.Server != nil && vs.Server.MessageBroker != nil && vs.Server.MessageBroker.Enabled {
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

				// Inject hub credentials into hub-managed broker plugins so they
				// can authenticate back to the Hub API. Self-managed plugins
				// handle their own credential lifecycle.
				if !pluginMgr.IsSelfManaged(scionplugin.PluginTypeBroker, bt) && hubSrv != nil && s != nil {
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
						secretKey, secretErr := authSvc.GenerateAndStoreSecret(ctx, brokerID)
						if secretErr != nil {
							log.Printf("Warning: failed to generate secret for broker plugin %q: %v", bt, secretErr)
						} else {
							hubCreds := map[string]string{
								"hub_url":     hubEndpoint,
								"hmac_key":    secretKey,
								"broker_id":   brokerID,
								"plugin_name": bt,
							}
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

			fanout := eventbus.NewFanOutEventBus(namedBuses, logging.Subsystem("hub.eventbus.fanout"))
			hubSrv.StartMessageBroker(fanout)
			log.Printf("Message broker started: fan-out with %d spoke(s)", len(namedBuses))

			// Wire the broker proxy as the host callbacks target for broker plugins.
			if proxy := hubSrv.GetMessageBrokerProxy(); proxy != nil {
				pluginMgr.SetBrokerHostCallbacks(proxy)
			}
		}

		hubSrv.StartNotificationDispatcher()
	}

	// 15. Print startup banner
	if !hostedMode {
		log.Println("Scion server ready (workstation mode)")
		if enableWeb {
			displayHost := cfg.Hub.Host
			if displayHost == "0.0.0.0" || displayHost == "" {
				displayHost = "127.0.0.1"
			}
			log.Printf("Web UI: http://%s:%d", displayHost, webPort)
		}
		if devAuthToken != "" {
			log.Printf("Developer token: export SCION_DEV_TOKEN=%s", devAuthToken)
		}
	}

	// 16. Wait for either an error or context cancellation
	select {
	case err := <-errCh:
		cancel()
		return err
	case <-ctx.Done():
		wg.Wait()
		return nil
	}
}

// initServerLogging initializes all logging subsystems and returns cleanup functions.
func initServerLogging(cmd *cobra.Command) (cleanups []func(), requestLogger *slog.Logger, messageLogger *slog.Logger, err error) {
	useGCP := os.Getenv("SCION_LOG_GCP") == "true"
	if os.Getenv("K_SERVICE") != "" {
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
	var cloudHandler slog.Handler
	if logging.IsCloudLoggingEnabled() {
		logLevel := logging.ResolveLogLevel(enableDebug)
		cfg := logging.CloudLoggingConfig{
			Component: component,
		}
		ch, cloudLogCleanup, cloudErr := logging.NewCloudHandler(ctx, cfg, logLevel)
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

	logging.SetupWithOTel(component, enableDebug, useGCP, logProvider, cloudHandler)

	// Initialize request logger
	reqLogCfg := logging.RequestLoggerConfig{
		FilePath:   os.Getenv(logging.EnvRequestLogPath),
		Component:  component,
		UseGCP:     useGCP,
		Foreground: serverStartForeground,
		Level:      logging.ResolveLogLevel(enableDebug),
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
		UseGCP:    useGCP,
		Level:     logging.ResolveLogLevel(enableDebug),
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

	return cleanups, requestLogger, messageLogger, nil
}

// loadAndReconcileConfig loads the server configuration file and reconciles
// it with command-line flags and workstation defaults.
func loadAndReconcileConfig(cmd *cobra.Command) (*config.GlobalConfig, error) {
	cfg, err := config.LoadGlobalConfig(serverConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	// Check if hosted mode is set in config
	if !cmd.Flags().Changed("hosted") && !cmd.Flags().Changed("production") {
		if cfg.Mode == "hosted" || cfg.Mode == "production" {
			hostedMode = true
		}
	}

	// Apply workstation defaults
	if !hostedMode {
		applyWorkstationDefaults(cmd)
		cfg.RuntimeBroker.Enabled = enableRuntimeBroker
		cfg.Auth.Enabled = enableDevAuth
		if !cmd.Flags().Changed("host") {
			cfg.Hub.Host = "127.0.0.1"
			cfg.RuntimeBroker.Host = "127.0.0.1"
		}
		if !cmd.Flags().Changed("storage-bucket") {
			cfg.Storage.Provider = "local"
		}
		cfg.Secrets.Backend = "local"
	}

	// Override with command-line flags
	if cmd.Flags().Changed("port") {
		cfg.Hub.Port = hubPort
	}
	if cmd.Flags().Changed("host") {
		cfg.Hub.Host = hubHost
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

	// Standalone broker in hosted mode: default to loopback when host
	// is not explicitly set. The broker needs to start on loopback so that
	// `scion broker register` can reach it locally before HMAC keys exist.
	if hostedMode && !cmd.Flags().Changed("host") && cfg.RuntimeBroker.Enabled && !enableHub {
		cfg.RuntimeBroker.Host = "127.0.0.1"
	}

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
	if os.Getenv("K_SERVICE") != "" {
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

// validateHostedBasic runs lightweight checks that apply to all --hosted
// deployments (both single-instance VMs and Cloud Run HA).
func validateHostedBasic(cfg *config.GlobalConfig) {
	if !hostedMode || cfg == nil {
		return
	}
	if strings.TrimSpace(resolveSessionSecret()) == "" {
		log.Println("Warning: no session secret set; sessions will not survive restarts. Set --session-secret or SCION_SERVER_SESSION_SECRET for durable sessions.")
	}
}

func validateHostedHAPreflight(cfg *config.GlobalConfig) error {
	if !hostedHAGuardsRequired(cfg) {
		return nil
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

	if cfg.Auth.Mode != "proxy" {
		return fmt.Errorf("hosted HA deployment requires server.auth.mode=proxy for Cloud Run IAP; got %q", cfg.Auth.Mode)
	}
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
	if !isCloudRunIAPAudience(proxyAudience) {
		return fmt.Errorf("hosted HA deployment requires a Cloud Run native IAP audience (/projects/<number>/locations/<region>/services/<service>); got %q", proxyAudience)
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
	if transportAudience != proxyAudience {
		return fmt.Errorf("hosted HA deployment requires server.auth.transport.oidc_audience to match server.auth.proxy.iap.audience")
	}
	if strings.TrimSpace(cfg.Auth.Transport.PlatformAuthSA) == "" {
		return fmt.Errorf("hosted HA deployment requires server.auth.transport.platform_auth_sa")
	}

	return nil
}

func isCloudRunIAPAudience(audience string) bool {
	parts := strings.Split(strings.TrimSpace(audience), "/")
	if len(parts) != 7 {
		return false
	}
	return parts[0] == "" &&
		parts[1] == "projects" && parts[2] != "" &&
		parts[3] == "locations" && parts[4] != "" &&
		parts[5] == "services" && parts[6] != ""
}

// checkServerPorts checks that required server ports are available.
func checkServerPorts(cfg *config.GlobalConfig) error {
	if enableHub && !enableWeb {
		status := checkPort(cfg.Hub.Host, cfg.Hub.Port)
		if status.inUse {
			if status.isScionServer {
				return fmt.Errorf("a scion server is already running on port %d\nUse 'scion server status' to check or 'scion server stop' to stop it", cfg.Hub.Port)
			}
			return fmt.Errorf("Hub port %d is already in use by another process", cfg.Hub.Port)
		}
	}
	if cfg.RuntimeBroker.Enabled {
		status := checkPort(cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)
		if status.inUse {
			if status.isScionServer {
				return fmt.Errorf("a scion server is already running on port %d\nUse 'scion server status' to check or 'scion server stop' to stop it", cfg.RuntimeBroker.Port)
			}
			return fmt.Errorf("Runtime Broker port %d is already in use by another process", cfg.RuntimeBroker.Port)
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
			return fmt.Errorf("Web Frontend port %d is already in use by another process", webPort)
		}
	}
	return nil
}

// initStore initializes the database store. The provided context is used for
// schema migration and the initial health-check ping so that a Ctrl+C during
// startup cancels those operations gracefully.
func initStore(ctx context.Context, cfg *config.GlobalConfig) (store.Store, error) {
	connMaxLifetime, err := cfg.Database.ConnMaxLifetimeDuration()
	if err != nil {
		return nil, fmt.Errorf("invalid database pool config: %w", err)
	}
	connMaxIdleTime, err := cfg.Database.ConnMaxIdleTimeDuration()
	if err != nil {
		return nil, fmt.Errorf("invalid database pool config: %w", err)
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
			return nil, err
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
			return nil, fmt.Errorf("failed to open database: %w", err)
		}
	case "postgres":
		// Postgres uses the pgx stdlib driver. The URL is a standard
		// connection string (e.g. "postgres://user:pass@host:5432/db?sslmode=require").
		entClient, err = entc.OpenPostgres(cfg.Database.URL, pool)
		if err != nil {
			return nil, fmt.Errorf("failed to open database: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}

	s := entadapter.NewCompositeStore(entClient)

	// Migrate runs Ent's schema migration and seeds built-in maintenance
	// operations (parity with the former raw-SQL store).
	if err := migrateStore(ctx, cfg, s); err != nil {
		s.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	if err := s.Ping(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	return s, nil
}

func migrateStore(ctx context.Context, cfg *config.GlobalConfig, s *entadapter.CompositeStore) error {
	if !strings.EqualFold(cfg.Database.Driver, "postgres") {
		return s.Migrate(ctx)
	}

	db := s.DB()
	if db == nil {
		return fmt.Errorf("postgres store does not expose a database connection")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring migration lock connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(store.LockSchemaMigration)); err != nil {
		return fmt.Errorf("acquiring migration advisory lock: %w", err)
	}
	locked := true
	defer func() {
		if locked {
			if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", int64(store.LockSchemaMigration)); err != nil {
				slog.Error("Failed to release migration advisory lock", "error", err)
			}
		}
	}()

	if err := s.Migrate(ctx); err != nil {
		return err
	}

	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", int64(store.LockSchemaMigration)); err != nil {
		return fmt.Errorf("releasing migration advisory lock: %w", err)
	}
	locked = false
	return nil
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
		TokenFile: cfg.Auth.TokenFile,
	}

	devAuthToken, err := apiclient.InitDevAuth(devAuthCfg, globalDir)
	if err != nil {
		return "", fmt.Errorf("failed to initialize dev auth: %w", err)
	}

	os.Setenv("SCION_DEV_TOKEN", devAuthToken)
	os.Setenv("SCION_AUTH_TOKEN", devAuthToken)

	log.Printf("Developer token: %s", devAuthToken)
	log.Printf("To authenticate CLI commands, run:")
	log.Printf("  export SCION_DEV_TOKEN=%s", devAuthToken)

	return devAuthToken, nil
}

// resolveHubEndpoint determines the Hub's public endpoint URL.
func resolveHubEndpoint(cfg *config.GlobalConfig, brokerSettings *config.Settings) string {
	if cfg.Hub.Endpoint != "" {
		return cfg.Hub.Endpoint
	}

	if !enableHub {
		hubEndpoint := brokerSettings.GetHubEndpoint()
		if hubEndpoint != "" && enableDebug {
			log.Printf("Hub endpoint resolved from project settings: %s", hubEndpoint)
		}
		return hubEndpoint
	}

	if webBaseURL != "" {
		hubEndpoint := strings.TrimRight(webBaseURL, "/")
		if enableDebug {
			log.Printf("Hub endpoint resolved from --base-url flag: %s", hubEndpoint)
		}
		return hubEndpoint
	}

	if baseURL := os.Getenv("SCION_SERVER_BASE_URL"); baseURL != "" {
		hubEndpoint := strings.TrimRight(baseURL, "/")
		if enableDebug {
			log.Printf("Hub endpoint resolved from SCION_SERVER_BASE_URL: %s", hubEndpoint)
		}
		return hubEndpoint
	}

	// Check settings (e.g. SCION_HUB_ENDPOINT env var) before falling back
	// to localhost. On combo servers the settings-level endpoint is typically
	// the public URL and should be used for agent dispatch.
	if hubEndpoint := brokerSettings.GetHubEndpoint(); hubEndpoint != "" {
		if enableDebug {
			log.Printf("Hub endpoint resolved from settings (SCION_HUB_ENDPOINT): %s", hubEndpoint)
		}
		return hubEndpoint
	}

	port := cfg.Hub.Port
	if enableWeb {
		port = webPort
	}
	hubEndpoint := fmt.Sprintf("http://localhost:%d", port)
	if enableDebug {
		log.Printf("Auto-computed hub endpoint for combo mode: %s", hubEndpoint)
	}
	return hubEndpoint
}

// parseAdminEmails parses admin emails from the flag or config.
func parseAdminEmails(cfg *config.GlobalConfig) []string {
	var adminEmailList []string
	if adminEmails != "" {
		for _, email := range strings.Split(adminEmails, ",") {
			email = strings.TrimSpace(email)
			if email != "" {
				adminEmailList = append(adminEmailList, email)
			}
		}
	} else if len(cfg.Hub.AdminEmails) > 0 {
		adminEmailList = cfg.Hub.AdminEmails
	}
	if len(adminEmailList) > 0 {
		log.Printf("Admin emails configured: %v", adminEmailList)
	}
	return adminEmailList
}

// resolveSessionSecret resolves the deployment-wide session secret from the
// --session-secret flag, falling back to the SCION_SERVER_SESSION_SECRET env
// var (then SESSION_SECRET for compatibility). The same value backs both the
// web session cookie store and the hub JWT signing keys so that all replicas
// behind the load balancer agree.
func resolveSessionSecret() string {
	secret := webSessionSecret
	if secret == "" {
		secret = os.Getenv("SCION_SERVER_SESSION_SECRET")
	}
	if secret == "" {
		secret = os.Getenv("SESSION_SECRET")
	}
	if secret == "" && hostedMode {
		slog.Warn("No session secret configured in hosted mode! Replicas will not be able to share sessions or agree on JWT signing keys, leading to login loops.")
	}
	return secret
}

// initHubServer creates and configures the Hub server.
func initHubServer(ctx context.Context, cfg *config.GlobalConfig, s store.Store, hubEndpoint, devAuthToken string, adminEmailList []string, adminMode bool, maintenanceMessage string, requestLogger, messageLogger *slog.Logger, globalDir string, pluginMgr *scionplugin.Manager, secretBackend secret.SecretBackend) (*hub.Server, error) {
	hubCfg := hub.ServerConfig{
		HubID:                 cfg.Hub.ResolveHubID(),
		Port:                  cfg.Hub.Port,
		Host:                  cfg.Hub.Host,
		ReadTimeout:           cfg.Hub.ReadTimeout,
		WriteTimeout:          cfg.Hub.WriteTimeout,
		CORSEnabled:           cfg.Hub.CORSEnabled,
		CORSAllowedOrigins:    cfg.Hub.CORSAllowedOrigins,
		CORSAllowedMethods:    cfg.Hub.CORSAllowedMethods,
		CORSAllowedHeaders:    cfg.Hub.CORSAllowedHeaders,
		CORSMaxAge:            cfg.Hub.CORSMaxAge,
		AuthMode:              cfg.Auth.Mode,
		DevAuthToken:          devAuthToken,
		Debug:                 enableDebug,
		AuthorizedDomains:     cfg.Auth.AuthorizedDomains,
		AdminEmails:           adminEmailList,
		UserAccessMode:        cfg.Auth.UserAccessMode,
		HubEndpoint:           hubEndpoint,
		SoftDeleteRetention:   cfg.Hub.SoftDeleteRetention,
		SoftDeleteRetainFiles: cfg.Hub.SoftDeleteRetainFiles,
		AdminMode:             adminMode,
		MaintenanceMessage:    maintenanceMessage,
		Workstation:           !hostedMode,
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
		MaintenanceConfig: resolveMaintenanceConfig(cfg),
		SecretBackend:     secretBackend,
		GCPProjectID:      cfg.Hub.GCPProjectID,
		// Derive the agent/user JWT signing keys from the same shared session
		// secret the web cookie store uses, so every replica behind the load
		// balancer agrees on the signing key regardless of its host-derived
		// HubID. Without this, a JWT minted by one replica fails validation on
		// another (cross-replica "session_expired" login loop).
		SharedSigningSecret: resolveSessionSecret(),
		// When SCION_REQUIRE_STABLE_SIGNING_KEY is truthy, the hub refuses to
		// start rather than silently mint a new signing key it cannot resolve
		// (which would invalidate every live token after, e.g., a redeploy onto a
		// new host that changed the HubID). Operators enabling this must supply a
		// session secret or pre-provision the signing keys.
		RequireStableSigningKey: os.Getenv("SCION_REQUIRE_STABLE_SIGNING_KEY") == "true",
	}

	// In hosted mode every replica must share the same session secret for
	// cookies and JWT signing keys to work across the load balancer. Running
	// without one means each replica generates its own ephemeral key, which
	// breaks session persistence and causes login loops.
	if hostedMode && hubCfg.SharedSigningSecret == "" {
		log.Println("WARNING: hosted mode is enabled but no session secret is configured. " +
			"Set --session-secret or SCION_SERVER_SESSION_SECRET to avoid cross-replica session failures.")
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
		case "header":
			// TODO: HeaderProxyAuthenticator (refactor of extractProxyUser)
			log.Printf("Proxy auth configured: provider=header (legacy IP-trust mode)")
		default:
			return nil, fmt.Errorf("unsupported auth.proxy.provider: %q", cfg.Auth.Proxy.Provider)
		}
	}

	// Construct transport token minter when auth.transport is configured
	if cfg.Auth.Transport != nil && cfg.Auth.Transport.Mode != "" && cfg.Auth.Transport.Mode != "none" {
		if cfg.Auth.Transport.PlatformAuthSA == "" {
			return nil, fmt.Errorf("auth.transport.platformAuthSA is required when auth.transport.mode=%q", cfg.Auth.Transport.Mode)
		}
		audience := cfg.Auth.Transport.OIDCAudience
		if audience == "" && cfg.Auth.Transport.Mode == "cloudrun_invoker" {
			// Derive audience from hub endpoint for Cloud Run invoker mode
			audience = hubEndpoint
		}
		if audience == "" {
			return nil, fmt.Errorf("auth.transport.oidcAudience is required when auth.transport.mode=%q", cfg.Auth.Transport.Mode)
		}
		hubCfg.TransportMode = cfg.Auth.Transport.Mode
		hubCfg.TransportAudience = audience
		hubCfg.TransportMinter = hub.NewGCPTransportMinter(cfg.Auth.Transport.PlatformAuthSA, "")
		log.Printf("Transport auth configured: mode=%s, audience=%s, sa=%s",
			cfg.Auth.Transport.Mode, audience, cfg.Auth.Transport.PlatformAuthSA)
	}

	hubSrv, err := hub.New(hubCfg, s)
	if err != nil {
		return nil, fmt.Errorf("hub server initialization failed: %w", err)
	}
	hubSrv.SetRequestLogger(requestLogger)
	hubSrv.SetPluginManager(pluginMgr)
	if messageLogger != nil {
		hubSrv.SetMessageLogger(messageLogger)
	}

	// Load notification channels from versioned settings
	if vs, err := config.LoadVersionedSettings(""); err == nil && vs.Server != nil && len(vs.Server.NotificationChannels) > 0 {
		channelConfigs := make([]hub.ChannelConfig, len(vs.Server.NotificationChannels))
		for i, c := range vs.Server.NotificationChannels {
			channelConfigs[i] = hub.ChannelConfig{
				Type:             c.Type,
				Params:           c.Params,
				FilterTypes:      c.FilterTypes,
				FilterUrgentOnly: c.FilterUrgentOnly,
			}
		}
		registry := hub.NewChannelRegistry(channelConfigs, logging.Subsystem("hub.notification-channels"))
		hubSrv.SetChannelRegistry(registry)
		log.Printf("Notification channels configured: %d channel(s) registered", registry.Len())
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

	if hostedMode {
		// Hosted mode: bootstrap bundled resources directly into the Hub from
		// the binary's embedded catalog. This removes the dependency on local
		// ~/.scion directories and ensures every replica converges on the same
		// DB + storage state.
		if err := hubSrv.BootstrapBundledResources(ctx, hub.BootstrapOptions{
			RepairStorage:   true,
			OverwritePolicy: hub.OverwriteBuiltinManaged,
		}); err != nil {
			log.Printf("Warning: bundled resource bootstrap failed: %v", err)
		}
	} else {
		// Workstation mode: import from local ~/.scion directories. These were
		// refreshed from embeds earlier in the startup sequence.
		globalTemplatesDir := filepath.Join(globalDir, "templates")
		if err := hubSrv.BootstrapTemplatesFromDir(ctx, globalTemplatesDir); err != nil {
			log.Printf("Warning: template bootstrap failed: %v", err)
		}
		globalHarnessConfigsDir := filepath.Join(globalDir, "harness-configs")
		if err := hubSrv.BootstrapHarnessConfigsFromDir(ctx, globalHarnessConfigsDir); err != nil {
			log.Printf("Warning: harness config bootstrap failed: %v", err)
		}
	}

	log.Printf("Database: %s (%s)", cfg.Database.Driver, cfg.Database.URL)

	return hubSrv, nil
}

// initHubStorage initializes the storage backend for the Hub server.
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
			log.Printf("Warning: failed to initialize GCS storage: %v", err)
			return nil
		}
		hubSrv.SetStorage(stor)
		log.Printf("GCS storage configured: gs://%s", storageBucket)
	} else if storageDir != "" {
		log.Printf("Initializing local storage at: %s", storageDir)
		storageCfg := storage.Config{
			Provider:  storage.ProviderLocal,
			LocalPath: storageDir,
		}
		stor, err := storage.New(ctx, storageCfg)
		if err != nil {
			log.Printf("Warning: failed to initialize local storage: %v", err)
			return nil
		}
		hubSrv.SetStorage(stor)
		log.Printf("Local storage configured: %s", storageDir)
	} else {
		defaultStorageDir := filepath.Join(globalDir, "storage")
		log.Printf("WARNING: No storage backend configured. Using local filesystem storage at: %s", defaultStorageDir)
		log.Printf("  For production use, configure --storage-bucket (GCS) or --storage-dir (explicit local path)")
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
	}
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
func newCommandBus(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server) hub.CommandBus {
	if !strings.EqualFold(cfg.Database.Driver, "postgres") {
		return hub.NoopCommandBus{}
	}
	ownsLocally := func(brokerID string) bool {
		mgr := hubSrv.GetControlChannelManager()
		if mgr == nil {
			return false
		}
		return mgr.IsConnected(brokerID)
	}
	bus, err := hub.NewPostgresCommandBus(ctx, cfg.Database.URL, ownsLocally, hubSrv.ReconcileBroker, logging.Subsystem("hub.commandbus"))
	if err != nil {
		log.Printf("WARNING: failed to start Postgres command bus (%v); falling back to no-op. Cross-replica dispatch signals will not work.", err)
		return hub.NoopCommandBus{}
	}
	log.Printf("Using Postgres command bus on channel scion_broker_cmd")
	return bus
}

// initWebServer creates and configures the Web server. The provided context is
// threaded to the event publisher so that the Postgres LISTEN/NOTIFY goroutine
// is cancelled cleanly on shutdown, preventing connection leaks.
func initWebServer(ctx context.Context, cfg *config.GlobalConfig, hubSrv *hub.Server, devAuthToken string, adminEmailList []string, adminMode bool, maintenanceMessage string, requestLogger *slog.Logger, dbRec dbmetrics.Recorder) (*hub.WebServer, error) {
	webHost := cfg.Hub.Host
	if webHost == "" {
		webHost = "0.0.0.0"
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

	// Resolve authorized domains and admin email list for the web server
	var webAuthorizedDomains []string
	var webAdminEmails []string
	if len(cfg.Auth.AuthorizedDomains) > 0 {
		webAuthorizedDomains = cfg.Auth.AuthorizedDomains
	}
	if adminEmails != "" {
		for _, email := range strings.Split(adminEmails, ",") {
			email = strings.TrimSpace(email)
			if email != "" {
				webAdminEmails = append(webAdminEmails, email)
			}
		}
	} else if len(cfg.Hub.AdminEmails) > 0 {
		webAdminEmails = cfg.Hub.AdminEmails
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
		default:
			return nil, fmt.Errorf("unsupported auth.proxy.provider: %q", cfg.Auth.Proxy.Provider)
		}
	}

	webCfg := hub.WebServerConfig{
		Port:               webPort,
		Host:               webHost,
		AssetsDir:          webAssetsDir,
		Debug:              enableDebug,
		SessionSecret:      sessionSecret,
		BaseURL:            baseURL,
		DevAuthToken:       devAuthToken,
		AuthMode:           cfg.Auth.Mode,
		AuthorizedDomains:  webAuthorizedDomains,
		AdminEmails:        webAdminEmails,
		UserAccessMode:     cfg.Auth.UserAccessMode,
		AdminMode:          adminMode,
		MaintenanceMessage: maintenanceMessage,
		EnableTestLogin:    enableTestLogin,
		ProxyAuthenticator: webProxyAuth,
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
		webSrv.SetOAuthService(hubSrv.GetOAuthService())
		webSrv.SetStore(hubSrv.GetStore())
		webSrv.SetUserTokenService(hubSrv.GetUserTokenService())
		webSrv.SetMaintenanceState(hubSrv.GetMaintenanceState())
		webSrv.MountHubAPI(hubSrv.Handler(), hubSrv.CleanupResources)

		localHubSrv := hubSrv
		webSrv.SetHubHealthProvider(func(ctx context.Context) interface{} {
			return localHubSrv.GetHealthInfo(ctx)
		})
	}

	return webSrv, nil
}

// startRuntimeBroker initializes and starts the runtime broker server.
func startRuntimeBroker(ctx context.Context, cmd *cobra.Command, cfg *config.GlobalConfig, hubSrv *hub.Server, webSrv *hub.WebServer, s store.Store, hubEndpoint, devAuthToken string, brokerSettings *config.Settings, globalDir string, requestLogger, messageLogger *slog.Logger, wg *sync.WaitGroup, errCh chan error) error {
	rt := runtime.GetRuntime("", "")
	log.Printf("Runtime broker using runtime: %s", rt.Name())
	statelessCloudRunBroker := enableHub && !simulateRemoteBroker && rt != nil && rt.Name() == "cloudrun"

	mgr := agent.NewManager(rt)
	settings := brokerSettings

	// Try loading versioned settings to get broker identity from server.broker
	versionedSettings, _, vsErr := config.LoadEffectiveSettings("")
	var vsBroker *config.V1BrokerConfig
	if vsErr == nil && versionedSettings != nil && versionedSettings.Server != nil {
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
	brokerID := resolveBrokerID(cfg, settings, vsBroker, globalDir, defaultBrokerID)

	// Resolve broker name
	brokerName := resolveBrokerName(cfg, settings, vsBroker)

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
	if enableHub && !simulateRemoteBroker && s != nil {
		rhEndpoint := fmt.Sprintf("http://%s:%d", cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)
		if cfg.RuntimeBroker.Host == "0.0.0.0" {
			rhEndpoint = fmt.Sprintf("http://localhost:%d", cfg.RuntimeBroker.Port)
		}

		effectiveID, regErr := registerGlobalProjectAndBroker(ctx, s, brokerID, brokerName, rhEndpoint, rt, serverAutoProvide, brokerSettings)
		if regErr != nil {
			log.Printf("Warning: failed to register global project: %v", regErr)
		} else {
			colocatedBrokerRegistered = true
			if effectiveID != brokerID {
				log.Printf("Broker ID updated from %s to %s (name-based dedup)", brokerID, effectiveID)
				brokerID = effectiveID
				if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
					log.Printf("Warning: failed to persist deduplicated broker ID: %v", err)
				}
			}
			log.Printf("Registered global project with runtime broker %s (endpoint: %s, autoProvide: %v)", brokerName, rhEndpoint, serverAutoProvide)
			if statelessCloudRunBroker {
				hubSrv.SetStatelessEmbeddedBrokerID(brokerID)
			} else {
				hubSrv.SetEmbeddedBrokerID(brokerID)
			}
		}

		// Generate credentials for co-located mode
		secretKeyBytes := make([]byte, 32)
		if _, err := rand.Read(secretKeyBytes); err != nil {
			log.Printf("Warning: failed to generate secret key for co-located mode: %v", err)
		} else {
			brokerSecret := &store.BrokerSecret{
				BrokerID:  brokerID,
				SecretKey: secretKeyBytes,
				Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
				CreatedAt: time.Now(),
				Status:    store.BrokerSecretStatusActive,
			}
			if err := s.DeleteBrokerSecret(ctx, brokerID); err != nil && err != store.ErrNotFound {
				log.Printf("Warning: failed to delete old broker secret: %v", err)
			}
			if err := s.CreateBrokerSecret(ctx, brokerSecret); err != nil {
				log.Printf("Warning: failed to create broker secret for co-located mode: %v", err)
			} else {
				log.Printf("Created broker secret for co-located control channel")
			}

			inMemoryCreds = &brokercredentials.BrokerCredentials{
				BrokerID:     brokerID,
				SecretKey:    base64.StdEncoding.EncodeToString(secretKeyBytes),
				HubEndpoint:  hubEndpointForRH,
				RegisteredAt: time.Now(),
			}
		}
	}

	// Auto-compute ContainerHubEndpoint.
	//
	// For colocated Docker agents we prefer to route them at the public domain
	// (served by Caddy) so each agent runs in its own network namespace under
	// bridge networking. This avoids the host-global metadata-server (:18380)
	// and telemetry (:4317) port collisions that --network=host causes for
	// concurrent agents. We fall back to the legacy host.docker.internal (host
	// networking) path when:
	//   - the escape hatch SCION_FORCE_HOST_NETWORK is set,
	//   - the Docker daemon lacks host-gateway support, or
	//   - no public domain is configured (can't reach Caddy without one).
	containerHubEndpoint := cfg.RuntimeBroker.ContainerHubEndpoint
	if containerHubEndpoint == "" && enableHub && hubEndpointForRH != "" && rt != nil {
		forceHost := os.Getenv(runtime.ForceHostNetworkEnvVar) != ""
		isDocker := rt.Name() == "docker"
		publicDomain := ""
		if hubEndpoint != "" && !isLocalhostURL(hubEndpoint) {
			publicDomain = strings.TrimRight(hubEndpoint, "/")
		}

		if isDocker && !forceHost && !runtime.DockerSupportsHostGateway(ctx, "") {
			log.Printf("WARNING: Docker daemon lacks host-gateway support; colocated agents will use host networking (re-introduces metadata-server port contention for concurrent agents). Upgrade Docker Engine to >= 20.10 to enable per-agent bridge networking.")
			forceHost = true
		}

		switch {
		case isDocker && !forceHost && publicDomain != "":
			// Route agents to the public domain so they reach the hub via Caddy
			// under bridge networking (colocatedExtraHosts maps the domain to
			// host-gateway). applyContainerBridgeOverride returns it wholesale.
			containerHubEndpoint = publicDomain
			log.Printf("Colocated %s agents routed via public domain %s (bridge networking)", rt.Name(), containerHubEndpoint)
		default:
			if computed := containerBridgeEndpoint(hubEndpointForRH, rt.Name()); computed != "" {
				containerHubEndpoint = computed
				if isDocker && !forceHost {
					// publicDomain == "" here: no domain configured to reach Caddy.
					log.Printf("WARNING: no public domain configured for colocated Docker agents; falling back to host networking. Set SCION_SERVER_BASE_URL=https://<domain> to enable per-agent bridge networking.")
				}
				log.Printf("Auto-computed ContainerHubEndpoint for %s runtime: %s", rt.Name(), containerHubEndpoint)
			}
		}
	}

	if rt != nil && rt.Name() == "container" && containerHubEndpoint != "" {
		created, dnsErr := runtime.EnsureAppleDNS(ctx, runtime.AppleDNSHostname, runtime.AppleDNSIP)
		if dnsErr != nil {
			log.Printf("WARNING: Failed to configure Apple Container DNS (%s → %s): %v\n"+
				"Agents may not reach the Hub. Run manually:\n"+
				"  sudo container system dns create %s --localhost %s",
				runtime.AppleDNSHostname, runtime.AppleDNSIP, dnsErr,
				runtime.AppleDNSHostname, runtime.AppleDNSIP)
		} else if created {
			log.Printf("Configured Apple Container DNS: %s → %s", runtime.AppleDNSHostname, runtime.AppleDNSIP)
		}
	}

	// Create Runtime Broker server configuration
	rhCfg := runtimebroker.ServerConfig{
		Port:                          cfg.RuntimeBroker.Port,
		Host:                          cfg.RuntimeBroker.Host,
		ReadTimeout:                   cfg.RuntimeBroker.ReadTimeout,
		WriteTimeout:                  cfg.RuntimeBroker.WriteTimeout,
		HubEndpoint:                   hubEndpointForRH,
		ContainerHubEndpoint:          containerHubEndpoint,
		BrokerID:                      brokerID,
		BrokerName:                    brokerName,
		CORSEnabled:                   cfg.RuntimeBroker.CORSEnabled,
		CORSAllowedOrigins:            cfg.RuntimeBroker.CORSAllowedOrigins,
		CORSAllowedMethods:            cfg.RuntimeBroker.CORSAllowedMethods,
		CORSAllowedHeaders:            cfg.RuntimeBroker.CORSAllowedHeaders,
		CORSMaxAge:                    cfg.RuntimeBroker.CORSMaxAge,
		AllowContainerScriptHarnesses: cfg.RuntimeBroker.AllowContainerScriptHarnesses,
		Debug:                         enableDebug,

		HubEnabled:           hubEndpointForRH != "",
		HubToken:             devAuthToken,
		TemplateCacheDir:     templateCacheDir,
		TemplateCacheMaxSize: templateCacheMax,

		ControlChannelEnabled: hubEndpointForRH != "" && !statelessCloudRunBroker,
		HeartbeatEnabled:      hubEndpointForRH != "" && !statelessCloudRunBroker,

		InMemoryCredentials:  inMemoryCreds,
		BrokerAuthEnabled:    true,
		BrokerAuthStrictMode: true,
	}

	// In co-located mode, hand the broker the Hub's storage backend so that a
	// local filesystem backend is read directly (zero-copy) instead of being
	// hydrated over HTTP. A non-local backend is left for cache-based hydration.
	if inMemoryCreds != nil && hubSrv != nil {
		rhCfg.ColocatedStorage = hubSrv.GetStorage()
	}

	rhSrv := runtimebroker.New(rhCfg, mgr, rt)
	rhSrv.SetRequestLogger(requestLogger)
	if messageLogger != nil {
		rhSrv.SetMessageLogger(messageLogger)
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

	// Start internal heartbeat loop for co-located operation
	if colocatedBrokerRegistered {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := s.UpdateRuntimeBrokerHeartbeat(ctx, brokerID, store.BrokerStatusOnline); err != nil {
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
func initPluginManager() *scionplugin.Manager {
	logger := logging.Subsystem("plugin")
	mgr := scionplugin.NewManager(logger)

	vs, err := config.LoadVersionedSettings("")
	if err != nil || vs == nil || vs.Server == nil || vs.Server.Plugins == nil {
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
	for name, entry := range vs.Server.Plugins.Broker {
		// Merge config_file contents with inline config (inline overrides file).
		mergedConfig, mergeErr := config.LoadPluginConfigFile(entry.ConfigFile, entry.Config)
		if mergeErr != nil {
			log.Printf("Warning: failed to load config file for plugin %q: %v", name, mergeErr)
			mergedConfig = entry.Config
		}
		pluginsCfg.Broker[name] = scionplugin.PluginEntry{
			Path:        entry.Path,
			Config:      mergedConfig,
			ConfigFile:  entry.ConfigFile,
			SelfManaged: entry.SelfManaged,
			Address:     entry.Address,
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
func resolveBrokerID(cfg *config.GlobalConfig, settings *config.Settings, vsBroker *config.V1BrokerConfig, globalDir, defaultBrokerID string) string {
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
		return defaultBrokerID
	}
	if brokerID == "" {
		brokerID = api.NewUUID()
		if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
			log.Printf("Warning: failed to persist broker ID to settings: %v", err)
		} else {
			log.Printf("Generated and persisted new broker ID: %s", brokerID)
		}
	}
	return brokerID
}

func deriveCloudRunLogicalBrokerID(settings *config.VersionedSettings, rt runtime.Runtime) (string, error) {
	if settings == nil || rt == nil || rt.Name() != "cloudrun" {
		return "", fmt.Errorf("deriveCloudRunLogicalBrokerID requires cloudrun runtime (got settings=%v, rt=%v)", settings != nil, rt)
	}
	rtConfig, runtimeType, err := settings.ResolveRuntime("")
	if err != nil {
		return "", fmt.Errorf("deriveCloudRunLogicalBrokerID: failed to resolve runtime: %w", err)
	}
	if runtimeType != "cloudrun" || rtConfig.CloudRun == nil {
		return "", fmt.Errorf("deriveCloudRunLogicalBrokerID: expected cloudrun runtime, got %q", runtimeType)
	}
	projectID := strings.TrimSpace(rtConfig.CloudRun.Project)
	location := strings.TrimSpace(rtConfig.CloudRun.Region)
	if projectID == "" || location == "" {
		return "", fmt.Errorf("deriveCloudRunLogicalBrokerID: project (%q) and region (%q) must both be set in cloudrun runtime config", projectID, location)
	}
	seed := fmt.Sprintf("cloudrun:%s:%s", projectID, location)
	return uuid.NewSHA1(cloudRunLogicalBrokerNamespace, []byte(seed)).String(), nil
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

// resolveHubEndpointForBroker determines the Hub endpoint URL for the
// runtime broker's internal communication (heartbeat, control channel).
// In co-located mode (enableHub true), this always resolves to localhost
// so the broker never routes through the external public URL.
func resolveHubEndpointForBroker(cfg *config.GlobalConfig, settings *config.Settings) string {
	hubEndpointForRH := cfg.RuntimeBroker.HubEndpoint
	if hubEndpointForRH == "" && enableHub {
		port := cfg.Hub.Port
		if enableWeb {
			port = webPort
		}
		hubEndpointForRH = fmt.Sprintf("http://localhost:%d", port)
		if enableDebug {
			log.Printf("Co-located Hub detected: using %s for heartbeat and template hydration", hubEndpointForRH)
		}
	} else if hubEndpointForRH == "" && settings.Hub != nil {
		hubEndpointForRH = settings.Hub.Endpoint
	}
	return hubEndpointForRH
}

// resolveMaintenanceConfig builds the maintenance config from versioned settings
// and environment variables.
func resolveMaintenanceConfig(cfg *config.GlobalConfig) hub.MaintenanceConfig {
	mc := hub.MaintenanceConfig{
		ServiceName: "scion-hub",
		BinaryDest:  "/usr/local/bin/scion",
	}

	// Pull from versioned settings if available.
	if vs, err := config.LoadVersionedSettings(""); err == nil {
		mc.ImageRegistry = vs.ResolveImageRegistry("")
		// Collect harness names from configured harness configs.
		for name := range vs.HarnessConfigs {
			mc.Harnesses = append(mc.Harnesses, name)
		}
		if mc.RepoPath == "" {
			mc.RepoPath = vs.WorkspacePath
		}
	}

	// Environment variable overrides.
	if v := os.Getenv("SCION_MAINTENANCE_IMAGE_REGISTRY"); v != "" {
		mc.ImageRegistry = v
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

	return mc
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
