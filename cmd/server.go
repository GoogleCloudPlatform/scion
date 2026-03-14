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
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/broker"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

// GlobalGroveName is the special name for the default grove when hub and runtime-broker run together
const GlobalGroveName = "global"

var (
	serverConfigPath    string
	hubPort             int
	hubHost             string
	enableHub           bool
	enableRuntimeBroker bool
	runtimeBrokerPort   int
	dbURL               string
	enableDevAuth       bool
	enableDebug         bool
	storageBucket       string
	storageDir          string

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
	enableWeb        bool
	webPort          int
	webAssetsDir     string
	webSessionSecret string
	webBaseURL       string

	// Server daemon flags
	serverStartForeground bool

	// Production mode flag
	productionMode bool
)

const (
	// serverDaemonComponent is the component name used for server daemon PID/log files.
	serverDaemonComponent = "server"
)

// serverCmd represents the server command
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage the Scion server components",
	Long: `Commands for managing the Scion server components.

By default, the server runs in workstation mode: all components are enabled,
dev-auth is on, and the server binds to 127.0.0.1 (loopback only). This is
the zero-configuration path for single-user, local development.

For production deployments, use --production to require explicit component
selection and bind to 0.0.0.0 by default.

The server provides:
- Hub API: Central registry for groves, agents, and templates (standalone: port 9810)
- Runtime Broker API: Agent lifecycle management on compute nodes (port 9800)
- Web Frontend: Browser-based UI (port 8080)

In combined mode, the Hub API is mounted on the web server's port (default 8080)
and the standalone Hub listener is not started.`,
}

// serverStartCmd represents the server start command
var serverStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Scion server components",
	Long: `Start the Scion server.

By default, the server runs in workstation mode: all components (Hub, Broker,
Web) are enabled, dev-auth is on, auto-provide is enabled, and the server
binds to 127.0.0.1 (loopback only). Just run 'scion server start' to get a
fully functional local server with no flags needed.

The server starts as a background daemon by default. Use --foreground to run
in the current terminal session (useful for systemd/launchd integration).

For production deployments, use --production to switch to explicit mode where
no components are enabled by default and the server binds to 0.0.0.0.

Explicit flags always override workstation defaults. For example,
'scion server start --host 0.0.0.0' uses workstation mode but binds to
all interfaces.

Configuration can be provided via:
- Config file (--config flag or ~/.scion/server.yaml)
- Environment variables (SCION_SERVER_* prefix)
- Command-line flags

Examples:
  # Start in workstation mode (all components, dev-auth, loopback)
  scion server start

  # Start in foreground (for systemd/launchd)
  scion server start --foreground

  # Workstation mode but expose on all interfaces
  scion server start --host 0.0.0.0

  # Production mode with explicit components
  scion server start --production --enable-hub --enable-runtime-broker --enable-web

  # Production mode, Hub with Web Frontend only
  scion server start --production --enable-hub --enable-web`,
	RunE: runServerStartOrDaemon,
}

// serverStopCmd stops the server daemon
var serverStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Scion server daemon",
	Long: `Stop the Scion server daemon.

This command stops the server if it's running as a daemon.
If the server is running in foreground mode, use Ctrl+C to stop it.

Examples:
  # Stop the server daemon
  scion server stop`,
	RunE: runServerStop,
}

// serverRestartCmd restarts the server daemon
var serverRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Scion server daemon",
	Long: `Restart the Scion server daemon.

This command stops the currently running server daemon and starts a new one
using the current scion binary. This is useful after installing a new version
of scion to pick up the updated binary.

If the server is not running as a daemon, this command will return an error.

Examples:
  # Restart the server daemon
  scion server restart`,
	RunE: runServerRestart,
}

// serverStatusCmd shows the current server status
var serverStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Scion server status",
	Long: `Show the current status of the Scion server.

This command displays:
- Whether the server is running (daemon or foreground)
- Daemon PID and log file location
- Component health status (Hub, Runtime Broker, Web)

Examples:
  # Show server status
  scion server status

  # Show server status in JSON format
  scion server status --json`,
	RunE: runServerStatus,
}

var serverStatusJSON bool

// serverInstallCmd generates a service file for the current platform
var serverInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Generate a system service file for Scion server",
	Long: `Generate a systemd (Linux) or launchd (macOS) service file for running
the Scion server as a managed system service.

The generated file uses --foreground mode so the service manager handles
lifecycle, logging, and restart. Workstation mode defaults apply unless
--production is specified.

On Linux, generates a systemd unit file.
On macOS, generates a launchd plist file.

Examples:
  # Generate a service file (prints to stdout)
  scion server install

  # Install directly on Linux (systemd user service)
  scion server install > ~/.config/systemd/user/scion-server.service
  systemctl --user daemon-reload
  systemctl --user enable --now scion-server

  # Install directly on macOS (launchd user agent)
  scion server install > ~/Library/LaunchAgents/io.scion.server.plist
  launchctl load ~/Library/LaunchAgents/io.scion.server.plist`,
	RunE: runServerInstall,
}

var serverInstallProduction bool

// portStatus represents the result of checking a port.
type portStatus struct {
	inUse         bool
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

// runServerStartOrDaemon handles the server start command. By default it launches
// the server as a background daemon. When --foreground is set, it runs directly.
func runServerStartOrDaemon(cmd *cobra.Command, args []string) error {
	if serverStartForeground {
		return runServerStart(cmd, args)
	}

	// Daemon mode
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Check if already running
	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	if running {
		return fmt.Errorf("server is already running (PID: %d)\n\nUse 'scion server stop' to stop it, or check the log at %s",
			pid, daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	}

	// Check if production mode is set in config (settings.yaml server.mode)
	if !cmd.Flags().Changed("production") {
		if mode := config.LoadServerMode(); mode == "production" {
			productionMode = true
		}
	}

	// Apply workstation defaults when not in production mode.
	// Workstation mode enables all components, dev-auth, auto-provide,
	// and binds to loopback (127.0.0.1) for single-user security.
	if !productionMode {
		if !cmd.Flags().Changed("enable-hub") {
			enableHub = true
		}
		if !cmd.Flags().Changed("enable-runtime-broker") {
			enableRuntimeBroker = true
		}
		if !cmd.Flags().Changed("enable-web") {
			enableWeb = true
		}
		if !cmd.Flags().Changed("dev-auth") {
			enableDevAuth = true
		}
		if !cmd.Flags().Changed("auto-provide") {
			serverAutoProvide = true
		}
		if !cmd.Flags().Changed("host") {
			hubHost = "127.0.0.1"
		}
	}

	// Check if at least one component is enabled
	if !enableHub && !enableRuntimeBroker && !enableWeb {
		return fmt.Errorf("no server components enabled; use --enable-hub, --enable-runtime-broker, or --enable-web")
	}

	// Find the scion executable
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Build args for the daemon process — pass through all flags
	daemonArgs := []string{"server", "start", "--foreground"}
	if productionMode {
		daemonArgs = append(daemonArgs, "--production")
	}
	if enableHub {
		daemonArgs = append(daemonArgs, "--enable-hub")
	}
	if enableRuntimeBroker {
		daemonArgs = append(daemonArgs, "--enable-runtime-broker")
	}
	if enableWeb {
		daemonArgs = append(daemonArgs, "--enable-web")
	}
	if enableDevAuth {
		daemonArgs = append(daemonArgs, "--dev-auth")
	}
	if enableDebug {
		daemonArgs = append(daemonArgs, "--debug")
	}
	if serverAutoProvide {
		daemonArgs = append(daemonArgs, "--auto-provide")
	}
	daemonArgs = append(daemonArgs, fmt.Sprintf("--host=%s", hubHost))
	if cmd.Flags().Changed("port") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--port=%d", hubPort))
	}
	if cmd.Flags().Changed("runtime-broker-port") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--runtime-broker-port=%d", runtimeBrokerPort))
	}
	if cmd.Flags().Changed("web-port") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--web-port=%d", webPort))
	}
	if cmd.Flags().Changed("config") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--config=%s", serverConfigPath))
	}
	if cmd.Flags().Changed("db") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--db=%s", dbURL))
	}
	if cmd.Flags().Changed("storage-bucket") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--storage-bucket=%s", storageBucket))
	}
	if cmd.Flags().Changed("storage-dir") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--storage-dir=%s", storageDir))
	}
	if globalMode {
		daemonArgs = append(daemonArgs, "--global")
	}

	// Start daemon
	mode := "workstation"
	if productionMode {
		mode = "production"
	}
	fmt.Printf("Starting server as daemon (%s mode)...\n", mode)
	if err := daemon.StartComponent(serverDaemonComponent, executable, daemonArgs, globalDir); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Save the daemon args for restart
	if err := daemon.SaveArgs(serverDaemonComponent, globalDir, daemonArgs); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save daemon args: %v\n", err)
	}

	// Verify it started
	time.Sleep(500 * time.Millisecond)
	running, pid, err = daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !running {
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	}

	fmt.Printf("Server started (PID: %d)\n", pid)
	fmt.Printf("Log file: %s\n", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	fmt.Printf("PID file: %s\n", daemon.GetPIDPathComponent(serverDaemonComponent, globalDir))
	fmt.Println()

	// Print quickstart info for workstation mode
	if !productionMode {
		printWorkstationQuickstart(globalDir, hubHost, webPort, enableWeb, enableDevAuth)
	}

	fmt.Println("Use 'scion server stop' to stop the daemon.")
	fmt.Println("Use 'scion server status' to check status.")

	return nil
}

func runServerStop(cmd *cobra.Command, args []string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !running {
		return fmt.Errorf("server daemon is not running")
	}

	fmt.Printf("Stopping server daemon (PID: %d)...\n", pid)

	if err := daemon.StopComponent(serverDaemonComponent, globalDir); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	// Verify it stopped
	time.Sleep(500 * time.Millisecond)
	running, _, _ = daemon.StatusComponent(serverDaemonComponent, globalDir)
	if running {
		return fmt.Errorf("daemon may still be running. Check with 'scion server status'")
	}

	fmt.Println("Server daemon stopped.")
	return nil
}

func runServerRestart(cmd *cobra.Command, args []string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !running {
		return fmt.Errorf("server daemon is not running.\n\nUse 'scion server start' to start it.")
	}

	// Stop the daemon
	fmt.Printf("Stopping server daemon (PID: %d)...\n", pid)
	if err := daemon.StopComponent(serverDaemonComponent, globalDir); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	// Wait for the process to exit
	if err := daemon.WaitForExitComponent(serverDaemonComponent, globalDir, 10*time.Second); err != nil {
		return fmt.Errorf("failed to stop server: %w", err)
	}
	fmt.Println("Server daemon stopped.")

	// Find the current scion executable
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Load saved args from previous start, or fall back to reconstructing from flags.
	daemonArgs, err := daemon.LoadArgs(serverDaemonComponent, globalDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load saved args: %v\n", err)
	}

	if daemonArgs == nil {
		// No saved args — reconstruct from current flags (legacy behavior).
		daemonArgs = []string{"server", "start", "--foreground"}
		if enableHub || enableRuntimeBroker || enableWeb {
			if enableHub {
				daemonArgs = append(daemonArgs, "--enable-hub")
			}
			if enableRuntimeBroker {
				daemonArgs = append(daemonArgs, "--enable-runtime-broker")
			}
			if enableWeb {
				daemonArgs = append(daemonArgs, "--enable-web")
			}
		}
		if enableDevAuth {
			daemonArgs = append(daemonArgs, "--dev-auth")
		}
		if enableDebug {
			daemonArgs = append(daemonArgs, "--debug")
		}
	}

	fmt.Println("Starting server with new binary...")
	if err := daemon.StartComponent(serverDaemonComponent, executable, daemonArgs, globalDir); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Verify it started
	time.Sleep(500 * time.Millisecond)
	running, pid, _ = daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !running {
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	}

	fmt.Printf("Server restarted (PID: %d)\n", pid)
	fmt.Printf("Log file: %s\n", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	fmt.Println()

	return nil
}

type serverStatusInfo struct {
	DaemonRunning bool   `json:"daemonRunning"`
	DaemonPID     int    `json:"daemonPid,omitempty"`
	LogFile       string `json:"logFile,omitempty"`
	PIDFile       string `json:"pidFile,omitempty"`
	HubRunning    bool   `json:"hubRunning,omitempty"`
	BrokerRunning bool   `json:"brokerRunning,omitempty"`
	WebRunning    bool   `json:"webRunning,omitempty"`
}

func runServerStatus(cmd *cobra.Command, args []string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	status := serverStatusInfo{}

	// Check daemon status
	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	status.DaemonRunning = running
	status.DaemonPID = pid
	if running {
		status.LogFile = daemon.GetLogPathComponent(serverDaemonComponent, globalDir)
		status.PIDFile = daemon.GetPIDPathComponent(serverDaemonComponent, globalDir)
	}

	// Probe health endpoints to check component status
	client := &http.Client{Timeout: 2 * time.Second}

	// Check web/hub on default web port (8080)
	if resp, err := client.Get("http://127.0.0.1:8080/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			status.WebRunning = true
			status.HubRunning = true // Hub is mounted on web when both are enabled
		}
	}

	// Check standalone hub on default hub port (9810) if not found on web port
	if !status.HubRunning {
		if resp, err := client.Get("http://127.0.0.1:9810/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				status.HubRunning = true
			}
		}
	}

	// Check broker on default broker port (9800)
	if resp, err := client.Get("http://127.0.0.1:9800/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			status.BrokerRunning = true
		}
	}

	if serverStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Human-readable output
	fmt.Println("Scion Server Status")
	if status.DaemonRunning {
		fmt.Printf("  Daemon:        running (PID: %d)\n", status.DaemonPID)
		fmt.Printf("  Log file:      %s\n", status.LogFile)
		fmt.Printf("  PID file:      %s\n", status.PIDFile)
	} else {
		fmt.Println("  Daemon:        not running")
	}
	fmt.Println()
	fmt.Println("Components:")
	if status.HubRunning {
		fmt.Println("  Hub API:         running")
	} else {
		fmt.Println("  Hub API:         not detected")
	}
	if status.BrokerRunning {
		fmt.Println("  Runtime Broker:  running")
	} else {
		fmt.Println("  Runtime Broker:  not detected")
	}
	if status.WebRunning {
		fmt.Println("  Web Frontend:    running")
	} else {
		fmt.Println("  Web Frontend:    not detected")
	}

	return nil
}

func runServerStart(cmd *cobra.Command, args []string) error {
	// Initialize logging
	useGCP := os.Getenv("SCION_LOG_GCP") == "true"
	if os.Getenv("K_SERVICE") != "" {
		// Auto-enable GCP logging on Cloud Run
		useGCP = true
	}
	// Disable GCP logging in workstation mode unless explicitly enabled
	if !productionMode && os.Getenv("SCION_LOG_GCP") == "" {
		useGCP = false
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

	// Initialize dedicated request logger
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
	requestLogger, reqLogCleanup, err := logging.NewRequestLogger(reqLogCfg)
	if err != nil {
		slog.Warn("Failed to initialize request logger", "error", err)
		requestLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if reqLogCleanup != nil {
		defer reqLogCleanup()
	}

	// Initialize dedicated message logger for message audit trail
	msgLogCfg := logging.MessageLoggerConfig{
		Component: component,
		UseGCP:    useGCP,
		Level:     logging.ResolveLogLevel(enableDebug),
	}
	if ch, ok := cloudHandler.(*logging.CloudHandler); ok && ch != nil {
		msgLogCfg.CloudClient = ch.Client()
	}
	messageLogger, msgLogCleanup, err := logging.NewMessageLogger(msgLogCfg)
	if err != nil {
		slog.Warn("Failed to initialize message logger", "error", err)
		messageLogger = nil
	}
	if msgLogCleanup != nil {
		defer msgLogCleanup()
	}

	// Load configuration
	cfg, err := config.LoadGlobalConfig(serverConfigPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Check if production mode is set in config (settings.yaml server.mode).
	// The config value is only consulted if --production was not explicitly passed.
	if !cmd.Flags().Changed("production") {
		if cfg.Mode == "production" {
			productionMode = true
		}
	}

	// Apply workstation defaults when not in production mode.
	// These are applied before explicit flag overrides so flags always win.
	if !productionMode {
		if !cmd.Flags().Changed("enable-hub") {
			enableHub = true
		}
		if !cmd.Flags().Changed("enable-runtime-broker") {
			enableRuntimeBroker = true
			cfg.RuntimeBroker.Enabled = true
		}
		if !cmd.Flags().Changed("enable-web") {
			enableWeb = true
		}
		if !cmd.Flags().Changed("dev-auth") {
			enableDevAuth = true
			cfg.Auth.Enabled = true
		}
		if !cmd.Flags().Changed("auto-provide") {
			serverAutoProvide = true
		}
		if !cmd.Flags().Changed("host") {
			cfg.Hub.Host = "127.0.0.1"
			cfg.RuntimeBroker.Host = "127.0.0.1"
		}
		// Force local backends unless explicitly overridden
		if !cmd.Flags().Changed("storage-bucket") {
			cfg.Storage.Provider = "local"
		}
		cfg.Secrets.Backend = "local"
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

	// Fallback to legacy environment variable if not set elsewhere (production mode only)
	if cfg.Storage.Bucket == "" && productionMode {
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

	// Resolve admin mode settings from config and env vars.
	// SCION_SERVER_ADMIN_MODE maps to admin.mode due to underscore splitting,
	// so we read these env vars directly (consistent with SESSION_SECRET, BASE_URL, etc.).
	adminMode := cfg.AdminMode
	if v := os.Getenv("SCION_SERVER_ADMIN_MODE"); v != "" {
		adminMode = v == "true" || v == "1" || v == "yes"
	}
	maintenanceMessage := cfg.MaintenanceMessage
	if v := os.Getenv("SCION_SERVER_MAINTENANCE_MESSAGE"); v != "" {
		maintenanceMessage = v
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

	// Log server mode
	if productionMode {
		log.Println("Server mode: production")
	} else {
		log.Printf("Server mode: workstation (binding to %s)", cfg.Hub.Host)
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
		// - SCION_AUTH_TOKEN: the broker handler can pass this to
		//   agent containers as a fallback when no JWT agent token is provided.
		os.Setenv("SCION_DEV_TOKEN", devAuthToken)
		os.Setenv("SCION_AUTH_TOKEN", devAuthToken)

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
		// Priority when hub is enabled (combo mode):
		//   1. Explicit server config (hub.endpoint / public_url)
		//   2. SCION_SERVER_BASE_URL env var
		//   3. Auto-compute from localhost:port
		// When hub is NOT enabled (standalone broker):
		//   1. Explicit server config
		//   2. Grove settings (hub.endpoint)
		// In combo mode, settings are NOT used because they may point to a
		// different hub entirely (e.g. a remote demo hub) while the local
		// hub is running on localhost.
		hubEndpoint := cfg.Hub.Endpoint
		if hubEndpoint == "" && enableHub {
			// Prefer SCION_SERVER_BASE_URL if set — this is the public URL of
			// the combined server (e.g. "https://hub.demo.scion-ai.dev") and
			// is reachable from remote brokers and agents inside containers.
			if baseURL := os.Getenv("SCION_SERVER_BASE_URL"); baseURL != "" {
				hubEndpoint = strings.TrimRight(baseURL, "/")
				if enableDebug {
					log.Printf("Hub endpoint resolved from SCION_SERVER_BASE_URL: %s", hubEndpoint)
				}
			} else {
				port := cfg.Hub.Port
				if enableWeb {
					port = webPort
				}
				hubEndpoint = fmt.Sprintf("http://localhost:%d", port)
				if enableDebug {
					log.Printf("Auto-computed hub endpoint for combo mode: %s", hubEndpoint)
				}
			}
		} else if hubEndpoint == "" {
			// Not in combo mode — fall back to grove settings.
			hubEndpoint = brokerSettings.GetHubEndpoint()
			if hubEndpoint != "" && enableDebug {
				log.Printf("Hub endpoint resolved from grove settings: %s", hubEndpoint)
			}
		}

		// Create Hub server configuration
		hubCfg := hub.ServerConfig{
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
			BrokerAuthConfig:      hub.DefaultBrokerAuthConfig(), // Enable broker HMAC authentication
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
				log.Printf("Warning: unknown message broker type %q, skipping", brokerType)
			}
		}

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

		// Bootstrap local templates into Hub if database is empty
		globalTemplatesDir := filepath.Join(globalDir, "templates")
		if err := hubSrv.BootstrapTemplatesFromDir(ctx, globalTemplatesDir); err != nil {
			log.Printf("Warning: template bootstrap failed: %v", err)
		}

		log.Printf("Database: %s (%s)", cfg.Database.Driver, cfg.Database.URL)

		if !enableWeb {
			// Hub runs its own HTTP server (standalone mode).
			// Create event publisher for notification dispatcher and event-driven features.
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
			// Start background services (scheduler, notifications) that
			// normally start inside hubSrv.Start(). In combined mode
			// Start() is not called since the Web server owns the listener.
			hubSrv.StartBackgroundServices(ctx)
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
			Port:               webPort,
			Host:               webHost,
			AssetsDir:          webAssetsDir,
			Debug:              enableDebug,
			SessionSecret:      webSessionSecret,
			BaseURL:            webBaseURL,
			DevAuthToken:       devAuthToken,
			AuthorizedDomains:  webAuthorizedDomains,
			AdminEmails:        webAdminEmails,
			AdminMode:          adminMode,
			MaintenanceMessage: maintenanceMessage,
		}
		webSrv = hub.NewWebServer(webCfg)
		webSrv.SetRequestLogger(requestLogger)

		// Create shared event publisher for real-time SSE
		eventPub := hub.NewChannelEventPublisher()
		webSrv.SetEventPublisher(eventPub)

		// Wire Hub services into WebServer if Hub is enabled
		if hubSrv != nil {
			hubSrv.SetEventPublisher(eventPub) // Hub publishes events
			webSrv.SetOAuthService(hubSrv.GetOAuthService())
			webSrv.SetStore(hubSrv.GetStore())
			webSrv.SetUserTokenService(hubSrv.GetUserTokenService())

			// Share runtime maintenance state between Hub and Web servers
			webSrv.SetMaintenanceState(hubSrv.GetMaintenanceState())

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

		// Enrich the default logger with broker_id so all logs from this
		// broker include the label (promoted to Cloud Logging labels by CloudHandler).
		slog.SetDefault(slog.Default().With(slog.String(logging.AttrBrokerID, brokerID)))

		// Get hub endpoint for the co-located runtime broker.
		// When hub and web are both enabled, the Hub API is mounted on the
		// web server's mux, so the broker MUST use webPort regardless of
		// what settings.Hub.Endpoint says (it may contain a stale standalone port).
		hubEndpointForRH := cfg.RuntimeBroker.HubEndpoint
		if hubEndpointForRH == "" && enableHub {
			// Co-located hub: if a public endpoint was resolved (e.g. from
			// SCION_SERVER_BASE_URL or explicit hub.endpoint config), use it
			// so that agents inside containers can reach the hub directly
			// without a bridge address. This is the production path.
			// Otherwise, compute the localhost endpoint with the correct port
			// for workstation/dev mode (the bridge override will handle
			// container reachability).
			if hubEndpoint != "" && !isLocalhostURL(hubEndpoint) {
				hubEndpointForRH = hubEndpoint
				if enableDebug {
					log.Printf("Co-located Hub: using public endpoint %s for broker and agents", hubEndpointForRH)
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
			effectiveID, regErr := registerGlobalGroveAndBroker(ctx, s, brokerID, brokerName, rhEndpoint, rt, serverAutoProvide, brokerSettings)
			if regErr != nil {
				log.Printf("Warning: failed to register global grove: %v", regErr)
			} else {
				colocatedBrokerRegistered = true
				// If name-based dedup found an existing broker with a different ID,
				// update brokerID and persist so future restarts use the correct ID.
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

		// Auto-compute ContainerHubEndpoint for combo mode when the hub
		// endpoint is localhost and the runtime is a container engine that
		// needs a bridge address to reach the host network.
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

			// Hub integration for template hydration
			HubEnabled:           hubEndpointForRH != "",
			HubToken:             devAuthToken, // Use dev token if available
			TemplateCacheDir:     templateCacheDir,
			TemplateCacheMaxSize: templateCacheMax,

			// Control channel - always enabled when Hub is configured because
			// PTY proxying requires the WebSocket control channel to route
			// terminal I/O between clients and brokers.
			// Heartbeat - enabled whenever a hub endpoint is configured.
			// The co-located "local" connection skips HTTP heartbeat via its
			// IsColocated flag (internal DB loop handles it instead).
			// Remote file-based connections always use HTTP heartbeat.
			ControlChannelEnabled: hubEndpointForRH != "",
			HeartbeatEnabled:      hubEndpointForRH != "",

			// In-memory credentials for co-located mode (allows control channel without file-based creds)
			InMemoryCredentials:  inMemoryCreds,
			BrokerAuthEnabled:    true,
			BrokerAuthStrictMode: true,
		}

		// Create Runtime Broker server
		rhSrv := runtimebroker.New(rhCfg, mgr, rt)
		rhSrv.SetRequestLogger(requestLogger)
		if messageLogger != nil {
			rhSrv.SetMessageLogger(messageLogger)
		}

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

		// Ensure notification dispatcher is started. In standalone hub mode,
		// Start() already started it (this is a no-op). In combined web+hub
		// mode, Start() is never called so this is the primary startup path.
		hubSrv.StartNotificationDispatcher()
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

	// Print startup banner for foreground workstation mode
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
// Returns the effective broker ID, which may differ from the input if an
// existing broker was found by name (deduplication).
func registerGlobalGroveAndBroker(ctx context.Context, s store.Store, brokerID, brokerName, endpoint string, rt runtime.Runtime, autoProvide bool, settings *config.Settings) (string, error) {
	// Check if global grove already exists
	globalGrove, err := s.GetGroveBySlug(ctx, GlobalGroveName)
	if err != nil && err != store.ErrNotFound {
		return brokerID, fmt.Errorf("failed to check for global grove: %w", err)
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
			return brokerID, fmt.Errorf("failed to create global grove: %w", err)
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
		return brokerID, fmt.Errorf("failed to check for runtime broker: %w", err)
	}

	// If not found by ID, try to find an existing broker with the same name
	// to prevent duplicate registrations when the broker ID changes (e.g.,
	// settings file recreated, format migration, or database reset).
	if broker == nil && brokerName != "" {
		existingByName, nameErr := s.GetRuntimeBrokerByName(ctx, brokerName)
		if nameErr != nil && nameErr != store.ErrNotFound {
			return brokerID, fmt.Errorf("failed to check for runtime broker by name: %w", nameErr)
		}
		if existingByName != nil {
			log.Printf("Found existing broker by name %q (ID: %s), reusing instead of creating duplicate", brokerName, existingByName.ID)
			broker = existingByName
			brokerID = existingByName.ID
		}
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
			return brokerID, fmt.Errorf("failed to create runtime broker: %w", err)
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
			return brokerID, fmt.Errorf("failed to update runtime broker: %w", err)
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
			return brokerID, fmt.Errorf("failed to add grove provider: %w", err)
		}
		// Update provider status
		if err := s.UpdateProviderStatus(ctx, globalGrove.ID, brokerID, store.BrokerStatusOnline); err != nil {
			log.Printf("Warning: failed to update provider status: %v", err)
		}
	}

	return brokerID, nil
}

// agentDispatcherAdapter adapts the agent.Manager to the hub.AgentDispatcher interface.
// This enables the Hub to dispatch agent creation to a co-located runtime broker.
type agentDispatcherAdapter struct {
	manager  agent.Manager
	store    store.Store
	brokerID string // The ID of this runtime broker
}

// newAgentDispatcherAdapter creates a new dispatcher adapter.
func newAgentDispatcherAdapter(mgr agent.Manager, s store.Store, brokerID string) *agentDispatcherAdapter {
	return &agentDispatcherAdapter{
		manager:  mgr,
		store:    s,
		brokerID: brokerID,
	}
}

// DispatchAgentCreate implements hub.AgentDispatcher.
// It starts the agent on the runtime broker and updates the hub store with runtime info.
func (d *agentDispatcherAdapter) DispatchAgentCreate(ctx context.Context, hubAgent *store.Agent) error {
	grovePath := d.resolveGrovePath(ctx, hubAgent.GroveID)
	opts := d.buildStartOptions(hubAgent, grovePath, false)

	// Ensure grove ID label is present for tracking
	if hubAgent.Labels == nil {
		hubAgent.Labels = make(map[string]string)
	}
	hubAgent.Labels["scion.grove"] = hubAgent.GroveID

	// Start the agent on the runtime broker
	agentInfo, err := d.manager.Start(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to start agent: %w", err)
	}

	// Update the hub agent record with runtime information
	hubAgent.Phase = string(state.PhaseRunning)
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
	grovePath := d.resolveGrovePath(ctx, hubAgent.GroveID)
	opts := d.buildStartOptions(hubAgent, grovePath, true)

	// Ensure grove ID label is present for tracking
	if hubAgent.Labels == nil {
		hubAgent.Labels = make(map[string]string)
	}
	hubAgent.Labels["scion.grove"] = hubAgent.GroveID
	if task != "" {
		opts.Task = task
	}

	// Start the agent on the runtime broker
	agentInfo, err := d.manager.Start(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to start agent: %w", err)
	}

	// Update the hub agent record with runtime information
	hubAgent.Phase = string(state.PhaseRunning)
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

// DispatchAgentStop implements hub.AgentDispatcher.
// It stops a running agent on the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentStop(ctx context.Context, hubAgent *store.Agent) error {
	if err := d.manager.Stop(ctx, hubAgent.Name); err != nil {
		return fmt.Errorf("failed to stop agent: %w", err)
	}

	// Update the hub agent record
	hubAgent.Phase = string(state.PhaseStopped)
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

	grovePath := d.resolveGrovePath(ctx, hubAgent.GroveID)
	opts := d.buildStartOptions(hubAgent, grovePath, true)

	// Ensure grove ID label is present for tracking
	if hubAgent.Labels == nil {
		hubAgent.Labels = make(map[string]string)
	}
	hubAgent.Labels["scion.grove"] = hubAgent.GroveID

	agentInfo, err := d.manager.Start(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to restart agent: %w", err)
	}

	hubAgent.Phase = string(state.PhaseRunning)
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

func (d *agentDispatcherAdapter) buildStartOptions(hubAgent *store.Agent, grovePath string, resume bool) api.StartOptions {
	// Build StartOptions from the hub agent record
	env := make(map[string]string)
	if hubAgent.AppliedConfig != nil && hubAgent.AppliedConfig.Env != nil {
		env = hubAgent.AppliedConfig.Env
	}

	opts := api.StartOptions{
		Name:      hubAgent.Name,
		Template:  hubAgent.Template,
		Image:     hubAgent.Image,
		Env:       env,
		Detached:  &hubAgent.Detached,
		GrovePath: grovePath,
		Resume:    resume,
	}

	if hubAgent.AppliedConfig != nil {
		opts.HarnessConfig = hubAgent.AppliedConfig.HarnessConfig
		if hubAgent.AppliedConfig.Task != "" {
			opts.Task = hubAgent.AppliedConfig.Task
		}
	}
	return opts
}
func (d *agentDispatcherAdapter) resolveGrovePath(ctx context.Context, groveID string) string {
	if groveID == "" || d.brokerID == "" {
		return ""
	}
	provider, err := d.store.GetGroveProvider(ctx, groveID, d.brokerID)
	if err != nil {
		log.Printf("Warning: failed to get grove provider for path lookup: %v", err)
		return ""
	}
	return provider.LocalPath
}

// DispatchAgentDelete implements hub.AgentDispatcher.
// It removes an agent from the runtime broker.
func (d *agentDispatcherAdapter) DispatchAgentDelete(ctx context.Context, hubAgent *store.Agent, deleteFiles, removeBranch, _ bool, _ time.Time) error {
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

	// For hub-native groves the provider LocalPath is typically empty.
	// Resolve from the grove slug so file cleanup can find the agent
	// directory at ~/.scion/groves/<slug>/.scion/agents/<name>.
	if grovePath == "" && hubAgent.GroveID != "" && deleteFiles {
		grove, err := d.store.GetGrove(ctx, hubAgent.GroveID)
		if err == nil && grove.GitRemote == "" && grove.Slug != "" {
			if globalDir, gErr := config.GetGlobalDir(); gErr == nil {
				candidate := filepath.Join(globalDir, "groves", grove.Slug)
				if _, sErr := os.Stat(candidate); sErr == nil {
					grovePath = candidate
				}
			}
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
func (d *agentDispatcherAdapter) DispatchAgentMessage(ctx context.Context, hubAgent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	// When a structured message is provided, format it for delivery
	deliveryText := message
	if structuredMsg != nil {
		deliveryText = messages.FormatForDelivery(structuredMsg)
	}
	if err := d.manager.Message(ctx, hubAgent.Name, deliveryText, interrupt); err != nil {
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

func runServerInstall(cmd *cobra.Command, args []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Resolve to absolute path
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	switch goos := goruntime.GOOS; goos {
	case "linux":
		return generateSystemdUnit(executable, serverInstallProduction)
	case "darwin":
		return generateLaunchdPlist(executable, serverInstallProduction)
	default:
		return fmt.Errorf("unsupported platform %q; only linux (systemd) and darwin (launchd) are supported", goos)
	}
}

func generateSystemdUnit(executable string, production bool) error {
	args := "server start --foreground"
	if production {
		args = "server start --foreground --production"
	}

	description := "Scion Workstation Server"
	if production {
		description = "Scion Server (Production)"
	}

	unit := fmt.Sprintf(`[Unit]
Description=%s
After=network.target docker.service

[Service]
Type=simple
ExecStart=%s %s
ExecStop=%s server stop
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, description, executable, args, executable)

	fmt.Print(unit)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To install as a systemd user service:")
	fmt.Fprintln(os.Stderr, "  mkdir -p ~/.config/systemd/user")
	fmt.Fprintln(os.Stderr, "  scion server install > ~/.config/systemd/user/scion-server.service")
	fmt.Fprintln(os.Stderr, "  systemctl --user daemon-reload")
	fmt.Fprintln(os.Stderr, "  systemctl --user enable --now scion-server")
	return nil
}

func generateLaunchdPlist(executable string, production bool) error {
	args := []string{executable, "server", "start", "--foreground"}
	if production {
		args = append(args, "--production")
	}

	// Build ProgramArguments XML entries
	var argEntries string
	for _, arg := range args {
		argEntries += fmt.Sprintf("        <string>%s</string>\n", arg)
	}

	label := "io.scion.server"
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/scion-server.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/scion-server.log</string>
</dict>
</plist>
`, label, argEntries)

	fmt.Print(plist)

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To install as a launchd user agent:")
	fmt.Fprintln(os.Stderr, "  scion server install > ~/Library/LaunchAgents/io.scion.server.plist")
	fmt.Fprintln(os.Stderr, "  launchctl load ~/Library/LaunchAgents/io.scion.server.plist")
	return nil
}

// printWorkstationQuickstart prints the first-run quickstart information
// including the dev token and web UI URL after a workstation-mode daemon starts.
func printWorkstationQuickstart(globalDir string, host string, wPort int, webEnabled, devAuth bool) {
	if webEnabled {
		displayHost := host
		if displayHost == "0.0.0.0" || displayHost == "" {
			displayHost = "127.0.0.1"
		}
		fmt.Printf("Web UI:  http://%s:%d\n", displayHost, wPort)
	}

	if devAuth {
		// Read the dev token from the token file (written by the daemon child process)
		tokenFile := filepath.Join(globalDir, "dev-token")
		if data, err := os.ReadFile(tokenFile); err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				fmt.Println()
				fmt.Println("Dev token (for CLI authentication):")
				fmt.Printf("  export SCION_DEV_TOKEN=%s\n", token)
			}
		}
	}
	fmt.Println()
}

func init() {
	rootCmd.AddCommand(serverCmd)
	serverCmd.AddCommand(serverStartCmd)
	serverCmd.AddCommand(serverStopCmd)
	serverCmd.AddCommand(serverRestartCmd)
	serverCmd.AddCommand(serverStatusCmd)
	serverCmd.AddCommand(serverInstallCmd)

	// Server start flags
	serverStartCmd.Flags().BoolVar(&serverStartForeground, "foreground", false, "Run in foreground instead of as daemon")
	serverStartCmd.Flags().BoolVar(&productionMode, "production", false, "Production mode: no components enabled by default, binds to 0.0.0.0")
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

	// Status flags
	serverStatusCmd.Flags().BoolVar(&serverStatusJSON, "json", false, "Output in JSON format")

	// Install flags
	serverInstallCmd.Flags().BoolVar(&serverInstallProduction, "production", false, "Generate service file for production mode")
}

// isLocalhostURL returns true if the given URL refers to a loopback address.
func isLocalhostURL(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// containerBridgeEndpoint returns a container-accessible URL that replaces
// localhost in hubEndpoint with the appropriate bridge hostname for the given
// runtime. Returns "" if the endpoint is not localhost or the runtime does not
// need a bridge address (e.g. kubernetes).
func containerBridgeEndpoint(hubEndpoint, runtimeName string) string {
	var bridgeHost string
	switch runtimeName {
	case "podman":
		bridgeHost = "host.containers.internal"
	case "docker":
		bridgeHost = "host.docker.internal"
	default:
		return ""
	}
	u, err := url.Parse(hubEndpoint)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return ""
	}
	u.Host = net.JoinHostPort(bridgeHost, u.Port())
	return u.String()
}
