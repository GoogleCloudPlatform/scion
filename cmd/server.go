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
	"github.com/spf13/cobra"
)

// GlobalProjectName is the special name for the default project when hub and runtime-broker run together
const GlobalProjectName = "global"

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

	// Hosted mode flag (replaces former "production" mode)
	hostedMode bool
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
the zero-configuration path for single-user development.

For multi-user deployments, use --hosted to require explicit component
selection and bind to 0.0.0.0 by default.

The server provides:
- Hub API: Central registry for projects, agents, and templates (standalone: port 9810)
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
fully functional workstation server with no flags needed.

The server starts as a background daemon by default. Use --foreground to run
in the current terminal session (useful for systemd/launchd integration).

For multi-user deployments, use --hosted to switch to hosted mode where
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

  # Hosted mode with explicit components
  scion server start --hosted --enable-hub --enable-runtime-broker --enable-web

  # Hosted mode, Hub with Web Frontend only
  scion server start --hosted --enable-hub --enable-web`,
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
--hosted is specified.

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

var serverInstallHosted bool

func init() {
	rootCmd.AddCommand(serverCmd)
	serverCmd.AddCommand(serverStartCmd)
	serverCmd.AddCommand(serverStopCmd)
	serverCmd.AddCommand(serverRestartCmd)
	serverCmd.AddCommand(serverStatusCmd)
	serverCmd.AddCommand(serverInstallCmd)

	// Server start flags
	serverStartCmd.Flags().BoolVar(&serverStartForeground, "foreground", false, "Run in foreground instead of as daemon")
	serverStartCmd.Flags().BoolVar(&hostedMode, "hosted", false, "Hosted mode: no components enabled by default, binds to 0.0.0.0")
	serverStartCmd.Flags().BoolVar(&hostedMode, "production", false, "Deprecated: use --hosted instead")
	_ = serverStartCmd.Flags().MarkDeprecated("production", "use --hosted instead")
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
	serverStartCmd.Flags().BoolVar(&serverAutoProvide, "auto-provide", false, "Automatically add runtime broker as provider for new projects")

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
	serverInstallCmd.Flags().BoolVar(&serverInstallHosted, "hosted", false, "Generate service file for hosted mode")
	serverInstallCmd.Flags().BoolVar(&serverInstallHosted, "production", false, "Deprecated: use --hosted instead")
	_ = serverInstallCmd.Flags().MarkDeprecated("production", "use --hosted instead")
}
