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
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/brokercredentials"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/harness"
	"github.com/ptone/scion-agent/pkg/hub"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/ptone/scion-agent/pkg/runtimebroker"
	"github.com/ptone/scion-agent/pkg/secret"
	"github.com/ptone/scion-agent/pkg/storage"
	"github.com/ptone/scion-agent/pkg/ent/entc"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/store/entadapter"
	"github.com/ptone/scion-agent/pkg/store/sqlite"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/ptone/scion-agent/pkg/util/logging"
	"github.com/spf13/cobra"
)

// GlobalGroveName is the special name for the default grove when hub and runtime-broker run together
const GlobalGroveName = "global"

var (
	serverConfigPath  string
	hubPort           int
	hubHost           string
	enableHub         bool
	enableRuntimeBroker bool
	runtimeBrokerPort   int
	dbURL             string
	enableDevAuth     bool
	enableDebug       bool
	storageBucket     string
	storageDir        string

	// Template cache settings for Runtime Broker
	templateCacheDir string
	templateCacheMax int64

	// Testing flag to simulate remote broker behavior when running co-located
	simulateRemoteBroker bool

	// Auto-provide flag for runtime broker
	serverAutoProvide bool

	// Admin emails for bootstrapping - comma-separated list
	adminEmails string

	// Web frontend flags
	enableWeb       bool
	webPort         int
	webAssetsDir    string
	webSessionSecret string
	webBaseURL       string
)

// serverCmd represents the server command
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage the Scion server components",
	Long: `Commands for managing the Scion server components.

The server provides:
- Hub API: Central registry for groves, agents, and templates (standalone: port 9810)
- Runtime Broker API: Agent lifecycle management on compute nodes (port 9800)
- Web Frontend: Browser-based UI (--enable-web, port 8080)

In combined mode (--enable-hub --enable-web), the Hub API is mounted on the
web server's port (default 8080) and the standalone Hub listener is not started.`,
}

// serverStartCmd represents the server start command
var serverStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Scion server components",
	Long: `Start one or more Scion server components.

Server Components:
- Hub API (--enable-hub): Central coordination for groves, agents, templates
- Runtime Broker API (--enable-runtime-broker): Agent lifecycle on this compute node
- Web Frontend (--enable-web): Browser-based UI for managing agents and groves

Configuration can be provided via:
- Config file (--config flag or ~/.scion/server.yaml)
- Environment variables (SCION_SERVER_* prefix)
- Command-line flags

Examples:
  # Start Hub API only
  scion server start --enable-hub

  # Start Runtime Broker API only
  scion server start --enable-runtime-broker

  # Start both Hub and Runtime Broker
  scion server start --enable-hub --enable-runtime-broker

  # Start Runtime Broker with custom port
  scion server start --enable-runtime-broker --runtime-broker-port 9800

  # Start Hub with Web Frontend
  scion server start --enable-hub --enable-web

  # Start Web Frontend with custom assets directory (development)
  scion server start --enable-hub --enable-web --web-assets-dir ./web/dist/client`,
	RunE: runServerStart,
}

// portStatus represents the result of checking a port.
type portStatus struct {
	inUse        bool
	isScionServer bool
}

// checkPort checks if a port is already bound and if it's a scion server.
func checkPort(host string, port int) portStatus {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return portStatus{inUse: false}
	}

	// Port is in use - check if it's a scion server by hitting the health endpoint
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		return portStatus{inUse: true, isScionServer: false}
	}
	defer resp.Body.Close()

	// Check if the response looks like a scion health response
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Uptime  string `json:"uptime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return portStatus{inUse: true, isScionServer: false}
	}

	// If we got valid health response fields, it's a scion server
	if health.Status != "" && health.Uptime != "" {
		return portStatus{inUse: true, isScionServer: true}
	}

	return portStatus{inUse: true, isScionServer: false}
}

func runServerStart(cmd *cobra.Command, args []string) error {
	// Initialize logging
	useGCP := os.Getenv("SCION_LOG_GCP") == "true"
	if os.Getenv("K_SERVICE") != "" {
		// Auto-enable GCP logging on Cloud Run
		useGCP = true
	}

	// Determine component name based on flags
	component := "scion-server"
	if enableHub && !enableRuntimeBroker {
		component = "scion-hub"
	} else if !enableHub && enableRuntimeBroker {
		component = "scion-broker"
	}

	// Initialize OTel logging if configured
	ctx := context.Background()
	logProvider, logCleanup, err := logging.InitOTelLogging(ctx, logging.OTelConfig{})
	if err != nil {
		log.Printf("Warning: failed to initialize OTel logging: %v", err)
	}
	if logCleanup != nil {
		defer logCleanup()
	}

	// Initialize direct Cloud Logging if enabled
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
			defer cloudLogCleanup()
			log.Printf("Cloud Logging enabled (logId=%s, project=%s)", logging.FormatLogID(), logging.FormatProjectID())
		}
	}

	// Setup logging with optional OTel bridge and Cloud Logging handler
	logging.SetupWithOTel(component, enableDebug, useGCP, logProvider, cloudHandler)

	// Load configuration
	cfg, err := config.LoadGlobalConfig(serverConfigPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Override with command-line flags if specified
	if cmd.Flags().Changed("port") {
		cfg.Hub.Port = hubPort
	}
	if cmd.Flags().Changed("host") {
		cfg.Hub.Host = hubHost
	}
	if cmd.Flags().Changed("db") {
		cfg.Database.URL = dbURL
	}
	if cmd.Flags().Changed("enable-hub") {
		// If explicitly set, use the flag value
		// (enableHub is the variable, it's already set by cobra)
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

	// Handle storage configuration
	if cmd.Flags().Changed("storage-bucket") {
		cfg.Storage.Bucket = storageBucket
	}
	if cmd.Flags().Changed("storage-dir") {
		cfg.Storage.LocalPath = storageDir
	}

	// Fallback to legacy environment variable if not set elsewhere
	if cfg.Storage.Bucket == "" {
		if val := os.Getenv("SCION_HUB_STORAGE_BUCKET"); val != "" {
			cfg.Storage.Bucket = val
			if cfg.Storage.Provider == "local" || cfg.Storage.Provider == "" {
				cfg.Storage.Provider = "gcs"
			}
		}
	}

	// Update local variables from cfg for backward compatibility in initialization logic
	storageBucket = cfg.Storage.Bucket
	storageDir = cfg.Storage.LocalPath
	if storageBucket != "" && (cfg.Storage.Provider == "local" || cfg.Storage.Provider == "") {
		cfg.Storage.Provider = "gcs"
	}

	// Ensure global directory exists and settings are initialized.
	// This is required for persisting the runtime broker identity.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}
	if _, err := os.Stat(globalDir); os.IsNotExist(err) {
		log.Println("Initializing global scion directory...")
		if err := config.InitGlobal(harness.All()); err != nil {
			return fmt.Errorf("failed to initialize global config: %w", err)
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
	// The server loads templates and settings from the active grove context, so running
	// inside a project grove may pick up project-specific (possibly legacy) configuration.
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

	// Check if at least one server is enabled
	if !enableHub && !cfg.RuntimeBroker.Enabled && !enableWeb {
		return fmt.Errorf("no server components enabled; use --enable-hub, --enable-runtime-broker, or --enable-web")
	}

	// Check if server ports are already in use
	if enableHub && !enableWeb {
		// Only check Hub port when running standalone (not mounted on web server).
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

	// Log debug mode status
	if enableDebug {
		log.Println("Debug logging enabled")
		// Log OAuth configuration for debugging
		logOAuthDebug(cfg)
	}

	// Setup graceful shutdown
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

	// Initialize store (needed for Hub and for global grove registration)
	var s store.Store
	if enableHub {
		switch cfg.Database.Driver {
		case "sqlite":
			sqliteStore, err := sqlite.New(cfg.Database.URL)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer sqliteStore.Close()

			// Run legacy migrations
			if err := sqliteStore.Migrate(context.Background()); err != nil {
				return fmt.Errorf("failed to run migrations: %w", err)
			}

			// Create Ent client for group operations (uses a separate
			// in-process database so Ent-managed tables don't conflict
			// with the legacy SQLite schema).
			entDSN := cfg.Database.URL + "_ent"
			entClient, err := entc.OpenSQLite("file:" + entDSN + "?cache=shared")
			if err != nil {
				return fmt.Errorf("failed to open ent database: %w", err)
			}
			if err := entc.AutoMigrate(context.Background(), entClient); err != nil {
				entClient.Close()
				return fmt.Errorf("failed to run ent migrations: %w", err)
			}

			// Wrap the SQLite store with the Ent-backed CompositeStore
			// so that all group operations use the Ent ORM.
			s = entadapter.NewCompositeStore(sqliteStore, entClient)
		default:
			return fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
		}

		// Verify database connectivity
		if err := s.Ping(context.Background()); err != nil {
			return fmt.Errorf("database ping failed: %w", err)
		}
	}

	// Variables to track runtime broker info for co-located registration
	var brokerID string
	var brokerName string
	var rt runtime.Runtime
	var brokerSettings *config.Settings
	var hubSrv *hub.Server
	var mgr agent.Manager
	var colocatedBrokerRegistered bool

	// Load settings early so both Hub and Broker can use grove-level hub.endpoint.
	// This resolves the grove settings hierarchy (global → project → env vars).
	{
		var err error
		brokerSettings, err = config.LoadSettings("")
		if err != nil {
			log.Printf("Warning: failed to load settings: %v", err)
			brokerSettings = &config.Settings{}
		}
		if brokerSettings.Hub == nil {
			brokerSettings.Hub = &config.HubClientConfig{}
		}
	}

	// Initialize dev auth if enabled
	var devAuthToken string
	if cfg.Auth.Enabled {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			return fmt.Errorf("failed to get global directory: %w", err)
		}

		devAuthCfg := apiclient.DevAuthConfig{
			Enabled:   cfg.Auth.Enabled,
			Token:     cfg.Auth.Token,
			TokenFile: cfg.Auth.TokenFile,
		}

		devAuthToken, err = apiclient.InitDevAuth(devAuthCfg, globalDir)
		if err != nil {
			return fmt.Errorf("failed to initialize dev auth: %w", err)
		}

		// Set dev token env vars in the server's own environment so that:
		// - SCION_DEV_TOKEN: co-located broker components using WithAutoDevAuth()
		//   automatically pick up the token for hub client authentication.
		// - SCION_SERVER_AUTH_DEV_TOKEN: the broker handler can pass this to
		//   agent containers as a fallback when no JWT agent token is provided.
		os.Setenv("SCION_DEV_TOKEN", devAuthToken)
		os.Setenv("SCION_SERVER_AUTH_DEV_TOKEN", devAuthToken)

		log.Println("WARNING: Development authentication enabled - not for production use")
		log.Printf("Dev token: %s", devAuthToken)
		log.Printf("To authenticate CLI commands, run:")
		log.Printf("  export SCION_DEV_TOKEN=%s", devAuthToken)
	}

	// Start Hub API if enabled
	if enableHub {
		// Parse admin emails from flag or config
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

		// Resolve the Hub's public endpoint URL.
		// Priority: server config (hub.endpoint / public_url) > grove settings (hub.endpoint)
		hubEndpoint := cfg.Hub.Endpoint
		if hubEndpoint == "" {
			hubEndpoint = brokerSettings.GetHubEndpoint()
			if hubEndpoint != "" && enableDebug {
				log.Printf("Hub endpoint resolved from grove settings: %s", hubEndpoint)
			}
		}

		// Auto-compute hub endpoint when running in combo mode (hub enabled)
		// and no explicit endpoint was configured. This ensures the Hub
		// dispatcher always has a proper endpoint to send to brokers/agents.
		if hubEndpoint == "" && enableHub {
			port := cfg.Hub.Port
			if enableWeb {
				port = webPort
			}
			hubEndpoint = fmt.Sprintf("http://localhost:%d", port)
			if enableDebug {
				log.Printf("Auto-computed hub endpoint for dispatcher: %s", hubEndpoint)
			}
		}

		// Create Hub server configuration
		hubCfg := hub.ServerConfig{
			Port:               cfg.Hub.Port,
			Host:               cfg.Hub.Host,
			ReadTimeout:        cfg.Hub.ReadTimeout,
			WriteTimeout:       cfg.Hub.WriteTimeout,
			CORSEnabled:        cfg.Hub.CORSEnabled,
			CORSAllowedOrigins: cfg.Hub.CORSAllowedOrigins,
			CORSAllowedMethods: cfg.Hub.CORSAllowedMethods,
			CORSAllowedHeaders: cfg.Hub.CORSAllowedHeaders,
			CORSMaxAge:         cfg.Hub.CORSMaxAge,
			DevAuthToken:       devAuthToken,
			Debug:              enableDebug,
			AuthorizedDomains:  cfg.Auth.AuthorizedDomains,
			AdminEmails:        adminEmailList,
			HubEndpoint:        hubEndpoint,
			BrokerAuthConfig:     hub.DefaultBrokerAuthConfig(), // Enable broker HMAC authentication
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
		}

		// Create Hub server
		hubSrv = hub.New(hubCfg, s)

		// Initialize storage if configured
		if storageBucket != "" {
			log.Printf("Initializing GCS storage with bucket: %s", storageBucket)
			storageCfg := storage.Config{
				Provider: storage.ProviderGCS,
				Bucket:   storageBucket,
			}
			stor, err := storage.New(ctx, storageCfg)
			if err != nil {
				return fmt.Errorf("failed to initialize GCS storage: %w", err)
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
				return fmt.Errorf("failed to initialize local storage: %w", err)
			}
			hubSrv.SetStorage(stor)
			log.Printf("Local storage configured: %s", storageDir)
		} else {
			// Auto-initialize local storage as fallback for development/local use
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
				return fmt.Errorf("failed to initialize local storage fallback: %w", err)
			}
			hubSrv.SetStorage(stor)
		}

		// Initialize secret backend
		secretBackend, err := secret.NewBackend(ctx, cfg.Secrets.Backend, s, secret.GCPBackendConfig{
			ProjectID:       cfg.Secrets.GCPProjectID,
			CredentialsJSON: cfg.Secrets.GCPCredentials,
		})
		if err != nil {
			return fmt.Errorf("failed to initialize secret backend: %w", err)
		}
		hubSrv.SetSecretBackend(secretBackend)
		log.Printf("Secret backend configured: %s", cfg.Secrets.Backend)

		log.Printf("Database: %s (%s)", cfg.Database.Driver, cfg.Database.URL)

		if !enableWeb {
			// Hub runs its own HTTP server (standalone mode).
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
			// Only need to clean up Hub resources on shutdown.
			log.Printf("Hub API will be mounted on Web server (port %d)", webPort)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-ctx.Done()
				hubSrv.CleanupResources(context.Background())
			}()
		}
	}

	// Start Web Frontend if enabled
	var webSrv *hub.WebServer
	if enableWeb {
		webHost := cfg.Hub.Host
		if webHost == "" {
			webHost = "0.0.0.0"
		}

		// Allow env var overrides for session/OAuth config
		if webSessionSecret == "" {
			webSessionSecret = os.Getenv("SCION_SERVER_SESSION_SECRET")
		}
		if webBaseURL == "" {
			webBaseURL = os.Getenv("SCION_SERVER_BASE_URL")
		}
		if webBaseURL == "" {
			webBaseURL = fmt.Sprintf("http://localhost:%d", webPort)
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
			Port:              webPort,
			Host:              webHost,
			AssetsDir:         webAssetsDir,
			Debug:             enableDebug,
			SessionSecret:     webSessionSecret,
			BaseURL:           webBaseURL,
			DevAuthToken:      devAuthToken,
			AuthorizedDomains: webAuthorizedDomains,
			AdminEmails:       webAdminEmails,
		}
		webSrv = hub.NewWebServer(webCfg)

		// Create shared event publisher for real-time SSE
		eventPub := hub.NewChannelEventPublisher()
		webSrv.SetEventPublisher(eventPub)

		// Wire Hub services into WebServer if Hub is enabled
		if hubSrv != nil {
			hubSrv.SetEventPublisher(eventPub) // Hub publishes events
			webSrv.SetOAuthService(hubSrv.GetOAuthService())
			webSrv.SetStore(hubSrv.GetStore())
			webSrv.SetUserTokenService(hubSrv.GetUserTokenService())

			// Mount Hub API on Web server — single port serves both.
			webSrv.MountHubAPI(hubSrv.Handler(), hubSrv.CleanupResources)

			// Register Hub health provider for composite /healthz
			localHubSrv := hubSrv // capture for closure
			webSrv.SetHubHealthProvider(func(ctx context.Context) interface{} {
				return localHubSrv.GetHealthInfo(ctx)
			})
		}

		log.Printf("Starting Web Frontend on %s:%d", webCfg.Host, webCfg.Port)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := webSrv.Start(ctx); err != nil {
				errCh <- fmt.Errorf("web server error: %w", err)
			}
		}()
	}

	// Start Runtime Broker API if enabled
	if cfg.RuntimeBroker.Enabled {
		// Initialize runtime (auto-detect based on environment)
		rt = runtime.GetRuntime("", "")
		log.Printf("Runtime broker using runtime: %s", rt.Name())

		// Create agent manager
		mgr = agent.NewManager(rt)

		// Settings were already loaded above for hub endpoint resolution.
		// Reuse them here for broker identity and configuration.
		settings := brokerSettings

		// Try loading versioned settings to get broker identity from server.broker
		versionedSettings, _, vsErr := config.LoadEffectiveSettings("")
		var vsBroker *config.V1BrokerConfig
		if vsErr == nil && versionedSettings != nil && versionedSettings.Server != nil {
			vsBroker = versionedSettings.Server.Broker
		}

		// Get broker ID: versioned server.broker > legacy settings.Hub > server.yaml config > generate new
		if vsBroker != nil && vsBroker.BrokerID != "" {
			brokerID = vsBroker.BrokerID
		} else {
			brokerID = settings.Hub.BrokerID
		}
		if brokerID == "" {
			brokerID = cfg.RuntimeBroker.BrokerID
		}
		if brokerID == "" {
			// Generate new UUID and persist it
			brokerID = api.NewUUID()
			if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
				log.Printf("Warning: failed to persist broker ID to settings: %v", err)
			} else {
				log.Printf("Generated and persisted new broker ID: %s", brokerID)
			}
		}

		// Get host nickname: versioned server.broker > legacy settings.Hub > server.yaml config > hostname
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

		// Get hub endpoint for the co-located runtime broker.
		// When hub and web are both enabled, the Hub API is mounted on the
		// web server's mux, so the broker MUST use webPort regardless of
		// what settings.Hub.Endpoint says (it may contain a stale standalone port).
		hubEndpointForRH := cfg.RuntimeBroker.HubEndpoint
		if hubEndpointForRH == "" && enableHub {
			// Co-located hub: compute the correct local endpoint.
			port := cfg.Hub.Port
			if enableWeb {
				port = webPort
			}
			hubEndpointForRH = fmt.Sprintf("http://localhost:%d", port)
			if enableDebug {
				log.Printf("Co-located Hub detected: using %s for heartbeat and template hydration", hubEndpointForRH)
			}
		} else if hubEndpointForRH == "" && settings.Hub != nil {
			// Remote hub: fall back to the persisted endpoint.
			hubEndpointForRH = settings.Hub.Endpoint
		}

		// In combined hub/broker mode, default auto-provide to true unless
		// explicitly overridden by the --auto-provide flag or settings.
		if enableHub && !cmd.Flags().Changed("auto-provide") {
			if vsBroker != nil && vsBroker.AutoProvide != nil {
				serverAutoProvide = *vsBroker.AutoProvide
			} else {
				serverAutoProvide = true
			}
		}

		// For co-located mode, register the broker record first, then generate
		// in-memory credentials so the RuntimeBroker can establish a control
		// channel to the Hub. The broker record must exist before the secret
		// because broker_secrets has a FK constraint on runtime_brokers(id).
		var inMemoryCreds *brokercredentials.BrokerCredentials
		if enableHub && !simulateRemoteBroker && s != nil {
			// Build RuntimeBroker endpoint for registration
			rhEndpoint := fmt.Sprintf("http://%s:%d", cfg.RuntimeBroker.Host, cfg.RuntimeBroker.Port)
			if cfg.RuntimeBroker.Host == "0.0.0.0" {
				rhEndpoint = fmt.Sprintf("http://localhost:%d", cfg.RuntimeBroker.Port)
			}

			// Register global grove and runtime broker record first (required for FK constraint)
			if err := registerGlobalGroveAndBroker(ctx, s, brokerID, brokerName, rhEndpoint, rt, serverAutoProvide, brokerSettings); err != nil {
				log.Printf("Warning: failed to register global grove: %v", err)
			} else {
				colocatedBrokerRegistered = true
				log.Printf("Registered global grove with runtime broker %s (endpoint: %s, autoProvide: %v)", brokerName, rhEndpoint, serverAutoProvide)
			}

			// Generate a 32-byte random secret key
			secretKeyBytes := make([]byte, 32)
			if _, err := rand.Read(secretKeyBytes); err != nil {
				log.Printf("Warning: failed to generate secret key for co-located mode: %v", err)
			} else {
				// Store the secret in Hub's database for HMAC validation
				brokerSecret := &store.BrokerSecret{
					BrokerID:  brokerID,
					SecretKey: secretKeyBytes,
					Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
					CreatedAt: time.Now(),
					Status:    store.BrokerSecretStatusActive,
				}
				// Delete any stale secret first (idempotent: ignore ErrNotFound on fresh DB)
				if err := s.DeleteBrokerSecret(ctx, brokerID); err != nil && err != store.ErrNotFound {
					log.Printf("Warning: failed to delete old broker secret: %v", err)
				}
				if err := s.CreateBrokerSecret(ctx, brokerSecret); err != nil {
					log.Printf("Warning: failed to create broker secret for co-located mode: %v", err)
				} else {
					log.Printf("Created broker secret for co-located control channel")
				}

				// Create in-memory credentials for RuntimeBroker
				inMemoryCreds = &brokercredentials.BrokerCredentials{
					BrokerID:     brokerID,
					SecretKey:    base64.StdEncoding.EncodeToString(secretKeyBytes),
					HubEndpoint:  hubEndpointForRH,
					RegisteredAt: time.Now(),
				}
			}
		}

		// Create Runtime Broker server configuration
		rhCfg := runtimebroker.ServerConfig{
			Port:               cfg.RuntimeBroker.Port,
			Host:               cfg.RuntimeBroker.Host,
			ReadTimeout:        cfg.RuntimeBroker.ReadTimeout,
			WriteTimeout:       cfg.RuntimeBroker.WriteTimeout,
			HubEndpoint:        hubEndpointForRH,
			BrokerID:           brokerID,
			BrokerName:         brokerName,
			CORSEnabled:        cfg.RuntimeBroker.CORSEnabled,
			CORSAllowedOrigins: cfg.RuntimeBroker.CORSAllowedOrigins,
			CORSAllowedMethods: cfg.RuntimeBroker.CORSAllowedMethods,
			CORSAllowedHeaders: cfg.RuntimeBroker.CORSAllowedHeaders,
			CORSMaxAge:         cfg.RuntimeBroker.CORSMaxAge,
			Debug:              enableDebug,

			// Hub integration for template hydration
			HubEnabled:           hubEndpointForRH != "",
			HubToken:             devAuthToken, // Use dev token if available
			TemplateCacheDir:     templateCacheDir,
			TemplateCacheMaxSize: templateCacheMax,

			// Control channel - always enabled when Hub is configured because
			// PTY proxying requires the WebSocket control channel to route
			// terminal I/O between clients and brokers.
			// Heartbeat - disabled for co-located operation since we use
			// the internal database heartbeat loop instead.
			ControlChannelEnabled: hubEndpointForRH != "",
			HeartbeatEnabled:      hubEndpointForRH != "" && (simulateRemoteBroker || !enableHub),

			// In-memory credentials for co-located mode (allows control channel without file-based creds)
			InMemoryCredentials: inMemoryCreds,
		}

		// Create Runtime Broker server
		rhSrv := runtimebroker.New(rhCfg, mgr, rt)

		// Register Broker health provider for composite web /healthz
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
	}

	// Set up the HTTP dispatcher for Hub to dispatch agents to RuntimeBrokers.
	// This uses the same code path whether RuntimeBroker is co-located or remote.
	if enableHub && hubSrv != nil {
		dispatcher := hubSrv.CreateAuthenticatedDispatcher()
		hubSrv.SetDispatcher(dispatcher)
		log.Printf("Agent dispatcher configured (HTTP-based)")
	}

	// Start internal heartbeat loop for co-located operation.
	// Registration was already done above before broker secret creation.
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

	// Wait for either an error or context cancellation
	select {
	case err := <-errCh:
		cancel() // Stop other servers
		return err
	case <-ctx.Done():
		// Wait for all servers to shutdown
		wg.Wait()
		return nil
	}
}

// registerGlobalGroveAndBroker creates the global grove and registers this
// runtime broker as a provider. This enables automatic agent handoff.
func registerGlobalGroveAndBroker(ctx context.Context, s store.Store, brokerID, brokerName, endpoint string, rt runtime.Runtime, autoProvide bool, settings *config.Settings) error {
	// Check if global grove already exists
	globalGrove, err := s.GetGroveBySlug(ctx, GlobalGroveName)
	if err != nil && err != store.ErrNotFound {
		return fmt.Errorf("failed to check for global grove: %w", err)
	}

	// Create global grove if it doesn't exist (without DefaultRuntimeBrokerID yet)
	groveNeedsDefaultBroker := false
	if globalGrove == nil {
		globalGrove = &store.Grove{
			ID:         api.NewUUID(),
			Name:       "Global",
			Slug:       GlobalGroveName,
			Visibility: store.VisibilityPrivate,
			Labels: map[string]string{
				"scion.io/system": "true",
				"scion.io/global": "true",
			},
		}

		if err := s.CreateGrove(ctx, globalGrove); err != nil {
			return fmt.Errorf("failed to create global grove: %w", err)
		}
		groveNeedsDefaultBroker = true
	} else if globalGrove.DefaultRuntimeBrokerID == "" {
		groveNeedsDefaultBroker = true
	}

	// Create or update the runtime broker record (must happen before setting as default)
	runtimeType := "docker"
	if rt != nil {
		runtimeType = rt.Name()
	}

	// Build profiles from settings, falling back to a default profile if none defined
	profiles := buildStoreBrokerProfiles(settings, runtimeType)

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	if err != nil && err != store.ErrNotFound {
		return fmt.Errorf("failed to check for runtime broker: %w", err)
	}

	if broker == nil {
		broker = &store.RuntimeBroker{
			ID:              brokerID,
			Name:            brokerName,
			Slug:            api.Slugify(brokerName),
			Version:         "0.1.0",
			Status:          store.BrokerStatusOnline,
			ConnectionState: "connected",
			Endpoint:        endpoint,
			AutoProvide:     autoProvide,
			Capabilities: &store.BrokerCapabilities{
				WebPTY: false,
				Sync:   true,
				Attach: true,
			},
			Profiles: profiles,
		}

		if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
			return fmt.Errorf("failed to create runtime broker: %w", err)
		}
	} else {
		// Update existing broker status, endpoint, auto-provide setting, and profiles
		broker.Status = store.BrokerStatusOnline
		broker.ConnectionState = "connected"
		broker.Endpoint = endpoint
		broker.AutoProvide = autoProvide
		broker.LastHeartbeat = time.Now()
		// Update profiles from settings (may have changed)
		broker.Profiles = profiles
		if err := s.UpdateRuntimeBroker(ctx, broker); err != nil {
			return fmt.Errorf("failed to update runtime broker: %w", err)
		}
	}

	// Now that the runtime broker exists, set it as the default for the grove
	if groveNeedsDefaultBroker {
		globalGrove.DefaultRuntimeBrokerID = brokerID
		if err := s.UpdateGrove(ctx, globalGrove); err != nil {
			log.Printf("Warning: failed to set default runtime broker for global grove: %v", err)
		}
	}

	// Get the global grove path (~/.scion)
	globalPath, err := config.GetGlobalDir()
	if err != nil {
		log.Printf("Warning: failed to get global grove path: %v", err)
		globalPath = "" // Will work but agents may not find the right path
	}

	// Add runtime broker as provider to global grove
	provider := &store.GroveProvider{
		GroveID:    globalGrove.ID,
		BrokerID:   brokerID,
		BrokerName: brokerName,
		LocalPath:  globalPath, // ~/.scion for the global grove
		Status:     store.BrokerStatusOnline,
		LastSeen:   time.Now(),
	}

	if err := s.AddGroveProvider(ctx, provider); err != nil {
		// Ignore duplicate provider errors
		if err != store.ErrAlreadyExists {
			return fmt.Errorf("failed to add grove provider: %w", err)
		}
		// Update provider status
		if err := s.UpdateProviderStatus(ctx, globalGrove.ID, brokerID, store.BrokerStatusOnline); err != nil {
			log.Printf("Warning: failed to update provider status: %v", err)
		}
	}

	return nil
}

// agentDispatcherAdapter adapts the agent.Manager to the hub.AgentDispatcher interface.
// This enables the Hub to dispatch agent creation to a co-located runtime broker.
type agentDispatcherAdapter struct {
	manager agent.Manager
	store   store.Store
	brokerID  string // The ID of this runtime broker
}

// newAgentDispatcherAdapter creates a new dispatcher adapter.
func newAgentDispatcherAdapter(mgr agent.Manager, s store.Store, brokerID string) *agentDispatcherAdapter {
	return &agentDispatcherAdapter{
		manager: mgr,
		store:   s,
		brokerID:  brokerID,
	}
}

// DispatchAgentCreate implements hub.AgentDispatcher.
// It starts the agent on the runtime broker and updates the hub store with runtime info.
func (d *agentDispatcherAdapter) DispatchAgentCreate(ctx context.Context, hubAgent *store.Agent) error {
	// Look up the local path for this grove on this runtime broker
	var grovePath string
	if hubAgent.GroveID != "" && d.brokerID != "" {
		provider, err := d.store.GetGroveProvider(ctx, hubAgent.GroveID, d.brokerID)
		if err != nil {
			log.Printf("Warning: failed to get grove provider for path lookup: %v", err)
		} else if provider.LocalPath != "" {
			grovePath = provider.LocalPath
		}
	}

	// Build StartOptions from the hub agent record
	env := make(map[string]string)
	if hubAgent.AppliedConfig != nil && hubAgent.AppliedConfig.Env != nil {
		env = hubAgent.AppliedConfig.Env
	}

	// Add grove ID label for tracking
	if hubAgent.Labels == nil {
		hubAgent.Labels = make(map[string]string)
	}
	hubAgent.Labels["scion.grove"] = hubAgent.GroveID

	opts := api.StartOptions{
		Name:      hubAgent.Name,
		Template:  hubAgent.Template,
		Image:     hubAgent.Image,
		Env:       env,
		Detached:  &hubAgent.Detached,
		GrovePath: grovePath, // Pass the local filesystem path for this grove
	}

	if hubAgent.AppliedConfig != nil {
		opts.Template = hubAgent.AppliedConfig.Harness
		// Pass the task through to the runtime broker
		if hubAgent.AppliedConfig.Task != "" {
			opts.Task = hubAgent.AppliedConfig.Task
		}
	}

	// Start the agent on the runtime broker
	agentInfo, err := d.manager.Start(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to start agent: %w", err)
	}

	// Update the hub agent record with runtime information
	hubAgent.Status = store.AgentStatusRunning
	hubAgent.ContainerStatus = agentInfo.ContainerStatus
	if agentInfo.ID != "" {
		hubAgent.RuntimeState = "container:" + agentInfo.ID
	}
	hubAgent.LastSeen = time.Now()

	if err := d.store.UpdateAgent(ctx, hubAgent); err != nil {
		log.Printf("Warning: failed to update agent with runtime info: %v", err)
	}

	return nil
}

// DispatchAgentStart implements hub.AgentDispatcher.
// For co-located runtime brokers, this resumes a stopped agent.
func (d *agentDispatcherAdapter) DispatchAgentStart(ctx context.Context, hubAgent *store.Agent, task string) error {
	// For now, starting an existing agent is not fully supported in the manager
	// The manager's Start method creates new agents, not resumes existing ones
	// TODO: Implement proper agent resume functionality in the manager
	log.Printf("DispatchAgentStart called for agent %s (not fully implemented)", hubAgent.Name)
	return nil
}

// DispatchAgentStop implements hub.AgentDispatcher.
// It stops a running agent on the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentStop(ctx context.Context, hubAgent *store.Agent) error {
	if err := d.manager.Stop(ctx, hubAgent.Name); err != nil {
		return fmt.Errorf("failed to stop agent: %w", err)
	}

	// Update the hub agent record
	hubAgent.Status = store.AgentStatusStopped
	hubAgent.LastSeen = time.Now()

	if err := d.store.UpdateAgent(ctx, hubAgent); err != nil {
		log.Printf("Warning: failed to update agent status: %v", err)
	}

	return nil
}

// DispatchAgentRestart implements hub.AgentDispatcher.
// It restarts an agent on the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentRestart(ctx context.Context, hubAgent *store.Agent) error {
	// Stop then start
	if err := d.manager.Stop(ctx, hubAgent.Name); err != nil {
		log.Printf("Warning: failed to stop agent during restart: %v", err)
	}

	// TODO: Implement proper restart with start after stop
	// For now, just update status
	hubAgent.Status = store.AgentStatusRunning
	hubAgent.LastSeen = time.Now()

	if err := d.store.UpdateAgent(ctx, hubAgent); err != nil {
		log.Printf("Warning: failed to update agent status: %v", err)
	}

	return nil
}

// DispatchAgentDelete implements hub.AgentDispatcher.
// It removes an agent from the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentDelete(ctx context.Context, hubAgent *store.Agent, deleteFiles, removeBranch bool) error {
	// Look up the local path for this grove on this runtime broker
	var grovePath string
	if hubAgent.GroveID != "" && d.brokerID != "" {
		provider, err := d.store.GetGroveProvider(ctx, hubAgent.GroveID, d.brokerID)
		if err != nil {
			log.Printf("Warning: failed to get grove provider for path lookup: %v", err)
		} else if provider.LocalPath != "" {
			grovePath = provider.LocalPath
		}
	}

	// Stop the agent first (ignore error if already stopped)
	_ = d.manager.Stop(ctx, hubAgent.Name)

	// Delete the agent
	_, err := d.manager.Delete(ctx, hubAgent.Name, deleteFiles, grovePath, removeBranch)
	if err != nil {
		return fmt.Errorf("failed to delete agent: %w", err)
	}

	return nil
}

// buildStoreBrokerProfiles builds store.BrokerProfile objects from settings.Profiles.
// If no profiles are defined in settings, returns a default profile with the detected runtime type.
func buildStoreBrokerProfiles(settings *config.Settings, defaultRuntimeType string) []store.BrokerProfile {
	// If no settings or no profiles defined, return a default profile
	if settings == nil || len(settings.Profiles) == 0 {
		return []store.BrokerProfile{
			{Name: "default", Type: defaultRuntimeType, Available: true},
		}
	}

	var profiles []store.BrokerProfile
	for name, profileCfg := range settings.Profiles {
		// Determine runtime type from the profile's runtime reference
		runtimeType := profileCfg.Runtime
		if runtimeType == "" {
			runtimeType = defaultRuntimeType
		}

		// Look up runtime config to get additional info (context, namespace for K8s)
		var context, namespace string
		if settings.Runtimes != nil {
			if rtCfg, ok := settings.Runtimes[profileCfg.Runtime]; ok {
				context = rtCfg.Context
				namespace = rtCfg.Namespace
			}
		}

		profiles = append(profiles, store.BrokerProfile{
			Name:      name,
			Type:      runtimeType,
			Available: true,
			Context:   context,
			Namespace: namespace,
		})
	}

	return profiles
}

// DispatchAgentMessage implements hub.AgentDispatcher.
// It sends a message to an agent on the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentMessage(ctx context.Context, hubAgent *store.Agent, message string, interrupt bool) error {
	if err := d.manager.Message(ctx, hubAgent.Name, message, interrupt); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	return nil
}

// logOAuthDebug logs OAuth configuration details for debugging.
// Secrets are redacted to only show whether they are set.
func logOAuthDebug(cfg *config.GlobalConfig) {
	log.Println("[Debug] OAuth Configuration:")
	log.Printf("[Debug]   CLI Google ClientID: %s", redactForDebug(cfg.OAuth.CLI.Google.ClientID))
	log.Printf("[Debug]   CLI Google ClientSecret: %s", redactForDebug(cfg.OAuth.CLI.Google.ClientSecret))
	log.Printf("[Debug]   CLI GitHub ClientID: %s", redactForDebug(cfg.OAuth.CLI.GitHub.ClientID))
	log.Printf("[Debug]   CLI GitHub ClientSecret: %s", redactForDebug(cfg.OAuth.CLI.GitHub.ClientSecret))
	log.Printf("[Debug]   Web Google ClientID: %s", redactForDebug(cfg.OAuth.Web.Google.ClientID))
	log.Printf("[Debug]   Web Google ClientSecret: %s", redactForDebug(cfg.OAuth.Web.Google.ClientSecret))
	log.Printf("[Debug]   Web GitHub ClientID: %s", redactForDebug(cfg.OAuth.Web.GitHub.ClientID))
	log.Printf("[Debug]   Web GitHub ClientSecret: %s", redactForDebug(cfg.OAuth.Web.GitHub.ClientSecret))
	log.Printf("[Debug]   Device Google ClientID: %s", redactForDebug(cfg.OAuth.Device.Google.ClientID))
	log.Printf("[Debug]   Device Google ClientSecret: %s", redactForDebug(cfg.OAuth.Device.Google.ClientSecret))
	log.Printf("[Debug]   Device GitHub ClientID: %s", redactForDebug(cfg.OAuth.Device.GitHub.ClientID))
	log.Printf("[Debug]   Device GitHub ClientSecret: %s", redactForDebug(cfg.OAuth.Device.GitHub.ClientSecret))
}

// redactForDebug returns a redacted version of a secret for debug logging.
func redactForDebug(value string) string {
	if value == "" {
		return "(not set)"
	}
	if len(value) <= 8 {
		return "(set, " + fmt.Sprintf("%d", len(value)) + " chars)"
	}
	return value[:4] + "..." + value[len(value)-4:] + " (" + fmt.Sprintf("%d", len(value)) + " chars)"
}

func init() {
	rootCmd.AddCommand(serverCmd)
	serverCmd.AddCommand(serverStartCmd)

	// Server start flags
	serverStartCmd.Flags().StringVarP(&serverConfigPath, "config", "c", "", "Path to server configuration file")

	// Hub API flags
	serverStartCmd.Flags().BoolVar(&enableHub, "enable-hub", false, "Enable the Hub API")
	serverStartCmd.Flags().IntVar(&hubPort, "port", 9810, "Hub API port (standalone mode only; ignored when --enable-web is set, use --web-port instead)")
	serverStartCmd.Flags().StringVar(&hubHost, "host", "0.0.0.0", "Hub API host to bind")
	serverStartCmd.Flags().StringVar(&dbURL, "db", "", "Database URL/path")

	// Runtime Broker API flags
	serverStartCmd.Flags().BoolVar(&enableRuntimeBroker, "enable-runtime-broker", false, "Enable the Runtime Broker API")
	serverStartCmd.Flags().IntVar(&runtimeBrokerPort, "runtime-broker-port", 9800, "Runtime Broker API port")

	// Auth flags
	serverStartCmd.Flags().BoolVar(&enableDevAuth, "dev-auth", false, "Enable development authentication (auto-generates token)")

	// Debug flags
	serverStartCmd.Flags().BoolVar(&enableDebug, "debug", false, "Enable debug logging (verbose output)")

	// Storage flags
	serverStartCmd.Flags().StringVar(&storageBucket, "storage-bucket", "", "GCS bucket name for template storage")
	serverStartCmd.Flags().StringVar(&storageDir, "storage-dir", "", "Local directory for template storage (alternative to GCS)")

	// Template cache flags (for Runtime Broker)
	serverStartCmd.Flags().StringVar(&templateCacheDir, "template-cache-dir", "", "Directory for caching templates from Hub (default: ~/.scion/cache/templates)")
	serverStartCmd.Flags().Int64Var(&templateCacheMax, "template-cache-max", 100*1024*1024, "Maximum template cache size in bytes (default: 100MB)")

	// Testing flags
	serverStartCmd.Flags().BoolVar(&simulateRemoteBroker, "simulate-remote-broker", false, "Skip co-located optimizations to test full remote broker code path")

	// Runtime Broker auto-provide flag
	serverStartCmd.Flags().BoolVar(&serverAutoProvide, "auto-provide", false, "Automatically add runtime broker as provider for new groves")

	// Web Frontend flags
	serverStartCmd.Flags().BoolVar(&enableWeb, "enable-web", false, "Enable the web frontend")
	serverStartCmd.Flags().IntVar(&webPort, "web-port", 8080, "Web frontend port")
	serverStartCmd.Flags().StringVar(&webAssetsDir, "web-assets-dir", "", "Path to client assets directory (overrides embedded)")
	serverStartCmd.Flags().StringVar(&webSessionSecret, "session-secret", "", "Session cookie signing secret (auto-generated if empty)")
	serverStartCmd.Flags().StringVar(&webBaseURL, "base-url", "", "Public base URL for OAuth redirects (e.g., https://scion.example.com)")

	// Admin bootstrap flags
	serverStartCmd.Flags().StringVar(&adminEmails, "admin-emails", "", "Comma-separated list of email addresses to auto-promote to admin role")
}
