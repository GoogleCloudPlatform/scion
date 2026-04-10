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
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/broker"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	scionplugin "github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
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
		if err := config.InitGlobal(harness.All()); err != nil {
			return fmt.Errorf("failed to initialize global config: %w", err)
		}
	} else if productionMode {
		// In production mode, refresh the default template and harness-configs
		// from the binary's embeds on every start. This ensures a binary upgrade
		// automatically propagates new defaults without manual re-init.
		// Only done in production to avoid overwriting local customizations
		// during development; admins should use non-default names for custom
		// templates.
		if err := config.UpdateDefaultTemplates(true, harness.All()); err != nil {
			log.Printf("Warning: failed to refresh default templates: %v", err)
		}
	}

	// When --global is set, change to the home directory so the server
	// operates from the global grove context regardless of where it was launched.
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

	// Warn if running from within a project grove instead of the global (~/.scion) grove.
	if projectDir, ok := config.FindProjectRoot(); ok {
		if projectDir != globalDir {
			parentDir := filepath.Dir(projectDir)
			fmt.Fprintf(os.Stderr, "\n%s%s WARNING: Server is running from a project grove context (%s)%s\n",
				util.Bold, util.Yellow, parentDir, util.Reset)
			fmt.Fprintf(os.Stderr, "%s%s          The runtime broker will use this grove's templates and settings.%s\n",
				util.Bold, util.Yellow, util.Reset)
			fmt.Fprintf(os.Stderr, "%s%s          For machine-wide operation, run the server from outside any project grove.%s\n\n",
				util.Bold, util.Yellow, util.Reset)
		}
	}

	// 5. Check if at least one server is enabled
	if !enableHub && !cfg.RuntimeBroker.Enabled && !enableWeb {
		return fmt.Errorf("no server components enabled; use --enable-hub, --enable-runtime-broker, or --enable-web")
	}

	// 6. Check ports
	if err := checkServerPorts(cfg); err != nil {
		return err
	}

	// Log server mode
	if productionMode {
		log.Println("Server mode: production")
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
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

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
		s, err = initStore(cfg)
		if err != nil {
			return err
		}
		if closer, ok := s.(io.Closer); ok {
			defer closer.Close()
		}
	}

	// Load settings early so both Hub and Broker can use grove-level hub.endpoint.
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
	}

	// 10. Resolve hub endpoint
	hubEndpoint := resolveHubEndpoint(cfg, brokerSettings)

	// Parse admin emails
	adminEmailList := parseAdminEmails(cfg)

	// 10b. Initialize plugin manager
	pluginMgr := initPluginManager()
	defer pluginMgr.Shutdown()
	harness.SetPluginManager(pluginMgr)

	// 11. Start Hub
	var hubSrv *hub.Server
	var secretBackend secret.SecretBackend
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

		if !enableWeb {
			// Hub runs its own HTTP server (standalone mode).
			eventPub := hub.NewChannelEventPublisher()
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
		webSrv = initWebServer(cfg, hubSrv, devAuthToken, adminEmailList, adminMode, maintenanceMessage, requestLogger)

		// In combined mode, start Hub background services now that the
		// ChannelEventPublisher has been wired by initWebServer.
		if enableHub && hubSrv != nil {
			hubSrv.StartBackgroundServices(ctx)
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

		// Initialize message broker from versioned settings
		if vs, err := config.LoadVersionedSettings(""); err == nil && vs.Server != nil && vs.Server.MessageBroker != nil && vs.Server.MessageBroker.Enabled {
			brokerType := vs.Server.MessageBroker.Type
			if brokerType == "" {
				brokerType = "inprocess"
			}
			switch brokerType {
			case "inprocess":
				b := broker.NewInProcessBroker(logging.Subsystem("hub.broker.inprocess"))
				hubSrv.StartMessageBroker(b)
				log.Printf("Message broker started: type=%s", brokerType)
			default:
				// Try loading as a plugin broker
				if pluginMgr.HasPlugin(scionplugin.PluginTypeBroker, brokerType) {
					b, pluginErr := pluginMgr.GetBroker(brokerType)
					if pluginErr != nil {
						log.Printf("Warning: failed to get broker plugin %q: %v", brokerType, pluginErr)
					} else {
						hubSrv.StartMessageBroker(b)
						log.Printf("Message broker started: type=%s (plugin)", brokerType)
					}
				} else {
					log.Printf("Warning: unknown message broker type %q (no plugin loaded), skipping", brokerType)
				}
			}

			// Wire the broker proxy as the host callbacks target for broker plugins.
			// This enables plugin-initiated subscriptions via the HostCallbacks
			// reverse channel (the proxy implements plugin.HostCallbacks).
			if proxy := hubSrv.GetMessageBrokerProxy(); proxy != nil {
				pluginMgr.SetBrokerHostCallbacks(proxy)
			}
		}

		hubSrv.StartNotificationDispatcher()
	}

	// 15. Print startup banner
	if !productionMode {
		log.Println("Scion server ready (workstation mode)")
		if enableWeb {
			displayHost := cfg.Hub.Host
			if displayHost == "0.0.0.0" || displayHost == "" {
				displayHost = "127.0.0.1"
			}
			log.Printf("Web UI: http://%s:%d", displayHost, webPort)
		}
		if devAuthToken != "" {
			log.Printf("Dev token: export SCION_DEV_TOKEN=%s", devAuthToken)
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
	if !productionMode && os.Getenv("SCION_LOG_GCP") == "" {
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

	// Initialize direct Cloud Logging
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
			cloudHandler = ch
			cleanups = append(cleanups, cloudLogCleanup)
			log.Printf("Cloud Logging enabled (logId=%s, project=%s)", logging.FormatLogID(), logging.FormatProjectID())
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
	if ch, ok := cloudHandler.(*logging.CloudHandler); ok && ch != nil {
		reqLogCfg.CloudClient = ch.Client()
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
	if ch, ok := cloudHandler.(*logging.CloudHandler); ok && ch != nil {
		msgLogCfg.CloudClient = ch.Client()
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

	// Check if production mode is set in config
	if !cmd.Flags().Changed("production") {
		if cfg.Mode == "production" {
			productionMode = true
		}
	}

	// Apply workstation defaults
	if !productionMode {
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

	// Fallback to legacy environment variable
	if cfg.Storage.Bucket == "" && productionMode {
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

// initStore initializes the database store.
func initStore(cfg *config.GlobalConfig) (store.Store, error) {
	switch cfg.Database.Driver {
	case "sqlite":
		sqliteStore, err := sqlite.New(cfg.Database.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to open database: %w", err)
		}

		if err := sqliteStore.Migrate(context.Background()); err != nil {
			sqliteStore.Close()
			return nil, fmt.Errorf("failed to run migrations: %w", err)
		}

		entDSN := cfg.Database.URL + "_ent"
		entClient, err := entc.OpenSQLite("file:" + entDSN + "?cache=shared")
		if err != nil {
			sqliteStore.Close()
			return nil, fmt.Errorf("failed to open ent database: %w", err)
		}
		if err := entc.AutoMigrate(context.Background(), entClient); err != nil {
			entClient.Close()
			sqliteStore.Close()
			return nil, fmt.Errorf("failed to run ent migrations: %w", err)
		}

		s := entadapter.NewCompositeStore(sqliteStore, entClient)

		if err := s.Ping(context.Background()); err != nil {
			sqliteStore.Close()
			return nil, fmt.Errorf("database ping failed: %w", err)
		}

		return s, nil
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}
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

	log.Println("WARNING: Development authentication enabled - not for production use")
	log.Printf("Dev token: %s", devAuthToken)
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
			log.Printf("Hub endpoint resolved from grove settings: %s", hubEndpoint)
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
		DevAuthToken:          devAuthToken,
		Debug:                 enableDebug,
		AuthorizedDomains:     cfg.Auth.AuthorizedDomains,
		AdminEmails:           adminEmailList,
		HubEndpoint:           hubEndpoint,
		SoftDeleteRetention:   cfg.Hub.SoftDeleteRetention,
		SoftDeleteRetainFiles: cfg.Hub.SoftDeleteRetainFiles,
		AdminMode:             adminMode,
		MaintenanceMessage:    maintenanceMessage,
		TelemetryDefault:      cfg.TelemetryEnabled,
		TelemetryConfig:       config.ConvertV1TelemetryToAPI(cfg.TelemetryConfig),
		BrokerAuthConfig:      hub.DefaultBrokerAuthConfig(),
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
	}

	hubSrv, err := hub.New(hubCfg, s)
	if err != nil {
		return nil, fmt.Errorf("hub server initialization failed: %w", err)
	}
	hubSrv.SetRequestLogger(requestLogger)
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
	initHubStorage(ctx, hubSrv, cfg, globalDir)

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
		hubSrv.SetGCPTokenGenerator(gcpGen)
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

	// Bootstrap local templates into Hub if database is empty
	globalTemplatesDir := filepath.Join(globalDir, "templates")
	if err := hubSrv.BootstrapTemplatesFromDir(ctx, globalTemplatesDir); err != nil {
		log.Printf("Warning: template bootstrap failed: %v", err)
	}

	log.Printf("Database: %s (%s)", cfg.Database.Driver, cfg.Database.URL)

	return hubSrv, nil
}

// initHubStorage initializes the storage backend for the Hub server.
func initHubStorage(ctx context.Context, hubSrv *hub.Server, cfg *config.GlobalConfig, globalDir string) {
	if storageBucket != "" {
		log.Printf("Initializing GCS storage with bucket: %s", storageBucket)
		storageCfg := storage.Config{
			Provider: storage.ProviderGCS,
			Bucket:   storageBucket,
		}
		stor, err := storage.New(ctx, storageCfg)
		if err != nil {
			log.Printf("Warning: failed to initialize GCS storage: %v", err)
			return
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
			return
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
			return
		}
		hubSrv.SetStorage(stor)
	}
}

// initWebServer creates and configures the Web server.
func initWebServer(cfg *config.GlobalConfig, hubSrv *hub.Server, devAuthToken string, adminEmailList []string, adminMode bool, maintenanceMessage string, requestLogger *slog.Logger) *hub.WebServer {
	webHost := cfg.Hub.Host
	if webHost == "" {
		webHost = "0.0.0.0"
	}

	// Allow env var overrides for session/OAuth config
	sessionSecret := webSessionSecret
	if sessionSecret == "" {
		sessionSecret = os.Getenv("SCION_SERVER_SESSION_SECRET")
	}
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

	webCfg := hub.WebServerConfig{
		Port:               webPort,
		Host:               webHost,
		AssetsDir:          webAssetsDir,
		Debug:              enableDebug,
		SessionSecret:      sessionSecret,
		BaseURL:            baseURL,
		DevAuthToken:       devAuthToken,
		AuthorizedDomains:  webAuthorizedDomains,
		AdminEmails:        webAdminEmails,
		AdminMode:          adminMode,
		MaintenanceMessage: maintenanceMessage,
	}
	webSrv := hub.NewWebServer(webCfg)
	webSrv.SetRequestLogger(requestLogger)

	// Create shared event publisher for real-time SSE
	eventPub := hub.NewChannelEventPublisher()
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

	return webSrv
}

// startRuntimeBroker initializes and starts the runtime broker server.
func startRuntimeBroker(ctx context.Context, cmd *cobra.Command, cfg *config.GlobalConfig, hubSrv *hub.Server, webSrv *hub.WebServer, s store.Store, hubEndpoint, devAuthToken string, brokerSettings *config.Settings, globalDir string, requestLogger, messageLogger *slog.Logger, wg *sync.WaitGroup, errCh chan error) error {
	rt := runtime.GetRuntime("", "")
	log.Printf("Runtime broker using runtime: %s", rt.Name())

	mgr := agent.NewManager(rt)
	settings := brokerSettings

	// Try loading versioned settings to get broker identity from server.broker
	versionedSettings, _, vsErr := config.LoadEffectiveSettings("")
	var vsBroker *config.V1BrokerConfig
	if vsErr == nil && versionedSettings != nil && versionedSettings.Server != nil {
		vsBroker = versionedSettings.Server.Broker
	}

	// Resolve broker ID
	brokerID := resolveBrokerID(cfg, settings, vsBroker, globalDir)

	// Resolve broker name
	brokerName := resolveBrokerName(cfg, settings, vsBroker)

	// Enrich logger with broker_id
	slog.SetDefault(slog.Default().With(slog.String(logging.AttrBrokerID, brokerID)))

	// Resolve hub endpoint for the runtime broker
	hubEndpointForRH := resolveHubEndpointForBroker(cfg, hubEndpoint, settings)

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

		effectiveID, regErr := registerGlobalGroveAndBroker(ctx, s, brokerID, brokerName, rhEndpoint, rt, serverAutoProvide, brokerSettings)
		if regErr != nil {
			log.Printf("Warning: failed to register global grove: %v", regErr)
		} else {
			colocatedBrokerRegistered = true
			if effectiveID != brokerID {
				log.Printf("Broker ID updated from %s to %s (name-based dedup)", brokerID, effectiveID)
				brokerID = effectiveID
				if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
					log.Printf("Warning: failed to persist deduplicated broker ID: %v", err)
				}
			}
			log.Printf("Registered global grove with runtime broker %s (endpoint: %s, autoProvide: %v)", brokerName, rhEndpoint, serverAutoProvide)
			hubSrv.SetEmbeddedBrokerID(brokerID)
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

	// Auto-compute ContainerHubEndpoint
	containerHubEndpoint := cfg.RuntimeBroker.ContainerHubEndpoint
	if containerHubEndpoint == "" && enableHub && hubEndpointForRH != "" && rt != nil {
		if computed := containerBridgeEndpoint(hubEndpointForRH, rt.Name()); computed != "" {
			containerHubEndpoint = computed
			log.Printf("Auto-computed ContainerHubEndpoint for %s runtime: %s", rt.Name(), containerHubEndpoint)
		}
	}

	// Create Runtime Broker server configuration
	rhCfg := runtimebroker.ServerConfig{
		Port:                 cfg.RuntimeBroker.Port,
		Host:                 cfg.RuntimeBroker.Host,
		ReadTimeout:          cfg.RuntimeBroker.ReadTimeout,
		WriteTimeout:         cfg.RuntimeBroker.WriteTimeout,
		HubEndpoint:          hubEndpointForRH,
		ContainerHubEndpoint: containerHubEndpoint,
		BrokerID:             brokerID,
		BrokerName:           brokerName,
		CORSEnabled:          cfg.RuntimeBroker.CORSEnabled,
		CORSAllowedOrigins:   cfg.RuntimeBroker.CORSAllowedOrigins,
		CORSAllowedMethods:   cfg.RuntimeBroker.CORSAllowedMethods,
		CORSAllowedHeaders:   cfg.RuntimeBroker.CORSAllowedHeaders,
		CORSMaxAge:           cfg.RuntimeBroker.CORSMaxAge,
		Debug:                enableDebug,

		HubEnabled:           hubEndpointForRH != "",
		HubToken:             devAuthToken,
		TemplateCacheDir:     templateCacheDir,
		TemplateCacheMaxSize: templateCacheMax,

		ControlChannelEnabled: hubEndpointForRH != "",
		HeartbeatEnabled:      hubEndpointForRH != "",

		InMemoryCredentials:  inMemoryCreds,
		BrokerAuthEnabled:    true,
		BrokerAuthStrictMode: true,
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
		log.Printf("Simulating remote broker: skipping automatic global grove registration")
	}

	return nil
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
		Broker:  make(map[string]scionplugin.PluginEntry),
		Harness: make(map[string]scionplugin.PluginEntry),
	}
	for name, entry := range vs.Server.Plugins.Broker {
		pluginsCfg.Broker[name] = scionplugin.PluginEntry{
			Path:        entry.Path,
			Config:      entry.Config,
			SelfManaged: entry.SelfManaged,
			Address:     entry.Address,
		}
	}
	for name, entry := range vs.Server.Plugins.Harness {
		pluginsCfg.Harness[name] = scionplugin.PluginEntry{
			Path:        entry.Path,
			Config:      entry.Config,
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

// resolveBrokerID determines the broker ID from various sources.
func resolveBrokerID(cfg *config.GlobalConfig, settings *config.Settings, vsBroker *config.V1BrokerConfig, globalDir string) string {
	var brokerID string
	if vsBroker != nil && vsBroker.BrokerID != "" {
		brokerID = vsBroker.BrokerID
	} else {
		brokerID = settings.Hub.BrokerID
	}
	if brokerID == "" {
		brokerID = cfg.RuntimeBroker.BrokerID
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

// resolveHubEndpointForBroker determines the Hub endpoint URL for the runtime broker.
func resolveHubEndpointForBroker(cfg *config.GlobalConfig, hubEndpoint string, settings *config.Settings) string {
	hubEndpointForRH := cfg.RuntimeBroker.HubEndpoint
	if hubEndpointForRH == "" && enableHub {
		if hubEndpoint != "" {
			// Use the already-resolved hub endpoint directly. It carries
			// the correct port (e.g. web port 8080 in combo mode) and the
			// broker is co-located so localhost is reachable.
			hubEndpointForRH = hubEndpoint
			if enableDebug {
				log.Printf("Co-located Hub: using endpoint %s for broker and agents", hubEndpointForRH)
			}
		} else {
			port := cfg.Hub.Port
			if enableWeb {
				port = webPort
			}
			hubEndpointForRH = fmt.Sprintf("http://localhost:%d", port)
			if enableDebug {
				log.Printf("Co-located Hub detected: using %s for heartbeat and template hydration", hubEndpointForRH)
			}
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
		mc.RepoPath = v
	}
	if v := os.Getenv("SCION_MAINTENANCE_BINARY_DEST"); v != "" {
		mc.BinaryDest = v
	}
	if v := os.Getenv("SCION_MAINTENANCE_SERVICE_NAME"); v != "" {
		mc.ServiceName = v
	}

	return mc
}
