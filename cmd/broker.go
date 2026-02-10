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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/brokercredentials"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/daemon"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/hubsync"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/ptone/scion-agent/pkg/version"
	"github.com/spf13/cobra"
)

var (
	// broker register flags
	brokerForceRegister bool
	brokerAutoProvide   bool

	// broker deregister flags
	brokerDeregisterBrokerOnly bool

	// broker start flags
	brokerStartForeground  bool
	brokerStartPort        int
	brokerStartAutoProvide bool
	brokerStartDebug       bool

	// broker provide/withdraw flags
	brokerGroveID   string
	brokerBrokerID  string // --broker flag for remote broker operations
)

// brokerCmd represents the broker command group
var brokerCmd = &cobra.Command{
	Use:   "broker",
	Short: "Manage the Runtime Broker",
	Long: `Commands for managing this host as a Runtime Broker.

A Runtime Broker is a compute node that executes agents on behalf of the Hub.
Brokers register with the Hub and can be added as providers for groves.

Commands:
  status       Show broker status (server, registration, groves)
  start        Start the broker server (as daemon by default)
  stop         Stop the broker daemon
  register     Register this host as a Runtime Broker with the Hub
  deregister   Remove this broker from the Hub
  provide      Add this broker as a provider for a grove
  withdraw     Remove this broker as a provider from a grove`,
}

// brokerRegisterCmd registers this broker with the Hub
var brokerRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register this host as a Runtime Broker with the Hub",
	Long: `Register this host as a Runtime Broker with the Hub.

This command registers your machine as a compute node that can execute
agents on behalf of the Hub. Once registered, the Hub can dispatch
agent operations to this broker.

Prerequisites:
- The broker server must be running (scion broker start)
- The Hub endpoint must be configured
- You must be authenticated with the Hub

This command will:
1. Verify the local broker server is running
2. Create a broker registration on the Hub
3. Complete the two-phase join process
4. Save broker credentials for future authentication

Examples:
  # Register this host as a broker
  scion broker register

  # Force re-registration even if already registered
  scion broker register --force

  # Register with auto-provide enabled
  scion broker register --auto-provide`,
	RunE: runBrokerRegister,
}

// brokerDeregisterCmd removes this broker from the Hub
var brokerDeregisterCmd = &cobra.Command{
	Use:   "deregister",
	Short: "Remove this broker from the Hub",
	Long: `Remove this broker from the Hub.

This command will:
1. Remove this broker from all groves it provides for
2. Clear the stored broker token

Use --broker-only to only remove the broker record without affecting grove providers.`,
	RunE: runBrokerDeregister,
}

// brokerStartCmd starts the broker server
var brokerStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Runtime Broker server",
	Long: `Start the Runtime Broker server.

By default, the broker starts as a background daemon. Use --foreground
to run in the current terminal session.

The broker server provides an API for agent lifecycle management and
communicates with the Hub for coordination.

Examples:
  # Start broker as daemon (background)
  scion broker start

  # Start broker in foreground
  scion broker start --foreground

  # Start broker on custom port
  scion broker start --port 9801

  # Start broker with auto-provide enabled
  scion broker start --auto-provide`,
	RunE: runBrokerStart,
}

// brokerProvideCmd adds this broker as a provider for a grove
var brokerProvideCmd = &cobra.Command{
	Use:   "provide",
	Short: "Add a broker as a provider for a grove",
	Long: `Add a broker as a provider for a grove.

When a broker is a provider for a grove, it can execute agents
for that grove. The Hub will dispatch agent operations to the
broker when agents are created in the grove.

If --grove is not specified, uses the current local grove.
If --broker is not specified, uses the local broker registration.

Examples:
  # Add local broker as provider for current grove
  scion broker provide

  # Add local broker as provider for a specific grove
  scion broker provide --grove <grove-id>

  # Add a remote broker as provider for a grove (admin only)
  scion broker provide --broker <broker-id> --grove <grove-id>`,
	RunE: runBrokerProvide,
}

// brokerWithdrawCmd removes this broker as a provider from a grove
var brokerWithdrawCmd = &cobra.Command{
	Use:   "withdraw",
	Short: "Remove a broker as a provider from a grove",
	Long: `Remove a broker as a provider from a grove.

After withdrawing, the broker will no longer receive agent dispatch
requests for the grove. Existing agents on the broker will continue
running but cannot be managed through the Hub until the broker is
re-added as a provider.

If --grove is not specified, uses the current local grove.
If --broker is not specified, uses the local broker registration.

Examples:
  # Remove local broker as provider from current grove
  scion broker withdraw

  # Remove local broker as provider from a specific grove
  scion broker withdraw --grove <grove-id>

  # Remove a remote broker as provider from a grove (admin only)
  scion broker withdraw --broker <broker-id> --grove <grove-id>`,
	RunE: runBrokerWithdraw,
}

// brokerStatusCmd shows the current broker status
var brokerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Runtime Broker status",
	Long: `Show the current status of a Runtime Broker.

This command displays:
- Whether the broker server is running (daemon or foreground)
- Hub registration status
- Groves this broker provides for
- Connection status to the Hub

If --broker is not specified, shows the local broker status.
If --broker is specified, queries the Hub for the remote broker's status.

Examples:
  # Show local broker status
  scion broker status

  # Show local broker status in JSON format
  scion broker status --json

  # Show status of a remote broker
  scion broker status --broker <broker-id>`,
	RunE: runBrokerStatus,
}

// brokerStopCmd stops the broker daemon
var brokerStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Runtime Broker daemon",
	Long: `Stop the Runtime Broker daemon.

This command stops the broker server if it's running as a daemon.
If the broker is running in foreground mode, use Ctrl+C to stop it.

Examples:
  # Stop the broker daemon
  scion broker stop`,
	RunE: runBrokerStop,
}

var brokerStatusJSON bool

func init() {
	rootCmd.AddCommand(brokerCmd)
	brokerCmd.AddCommand(brokerRegisterCmd)
	brokerCmd.AddCommand(brokerDeregisterCmd)
	brokerCmd.AddCommand(brokerStartCmd)
	brokerCmd.AddCommand(brokerProvideCmd)
	brokerCmd.AddCommand(brokerWithdrawCmd)
	brokerCmd.AddCommand(brokerStatusCmd)
	brokerCmd.AddCommand(brokerStopCmd)

	// Status flags
	brokerStatusCmd.Flags().BoolVar(&brokerStatusJSON, "json", false, "Output in JSON format")
	brokerStatusCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker ID to query (for remote broker status)")

	// Register flags
	brokerRegisterCmd.Flags().BoolVar(&brokerForceRegister, "force", false, "Force re-registration even if already registered")
	brokerRegisterCmd.Flags().BoolVar(&brokerAutoProvide, "auto-provide", false, "Automatically add as provider for new groves")

	// Deregister flags
	brokerDeregisterCmd.Flags().BoolVar(&brokerDeregisterBrokerOnly, "broker-only", false, "Only remove broker record, not grove providers")

	// Start flags
	brokerStartCmd.Flags().BoolVar(&brokerStartForeground, "foreground", false, "Run in foreground instead of as daemon")
	brokerStartCmd.Flags().IntVar(&brokerStartPort, "port", DefaultBrokerPort, "Runtime Broker API port")
	brokerStartCmd.Flags().BoolVar(&brokerStartAutoProvide, "auto-provide", false, "Automatically add as provider for new groves")
	brokerStartCmd.Flags().BoolVar(&brokerStartDebug, "debug", false, "Enable debug logging (verbose output)")

	// Provide/withdraw flags
	brokerProvideCmd.Flags().StringVar(&brokerGroveID, "grove", "", "Grove name or ID to add as provider for")
	brokerProvideCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker name or ID to use (for remote broker operations)")
	brokerWithdrawCmd.Flags().StringVar(&brokerGroveID, "grove", "", "Grove name or ID to remove as provider from")
	brokerWithdrawCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker name or ID to use (for remote broker operations)")
}

func runBrokerRegister(cmd *cobra.Command, args []string) error {
	// Resolve grove path to find project settings (needed for Hub endpoint config)
	gp := grovePath
	if gp == "" && globalMode {
		gp = "global"
	}

	resolvedPath, isGlobal, err := config.ResolveGrovePath(gp)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return fmt.Errorf("Hub endpoint not configured.\n\nConfigure the Hub endpoint via:\n  - SCION_HUB_ENDPOINT environment variable\n  - hub.endpoint in settings.yaml\n  - --hub flag on any command\n\nExample: scion config set hub.endpoint https://hub.scion.dev --global")
	}

	// Step 1: Check if local broker server is running
	health, err := checkLocalBrokerServer(DefaultBrokerPort)
	if err != nil {
		return fmt.Errorf("broker server not running on port %d.\n\nStart it with: scion broker start\n\nError: %w", DefaultBrokerPort, err)
	}
	fmt.Printf("Broker server is running (status: %s, version: %s)\n", health.Status, health.Version)

	// Step 2: Check if grove is linked to Hub
	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Check Hub connectivity
	if _, err := client.Health(ctx); err != nil {
		return fmt.Errorf("Hub at %s is not responding: %w", endpoint, err)
	}

	// Get grove name for display
	var groveName string
	if isGlobal {
		groveName = "global"
	} else {
		gitRemote := util.GetGitRemote()
		if gitRemote != "" {
			groveName = util.ExtractRepoName(gitRemote)
		} else {
			groveName = filepath.Base(filepath.Dir(resolvedPath))
		}
	}

	// Check if grove is linked
	groveID := settings.GroveID
	groveLinked := false
	if groveID != "" {
		groveLinked, _ = isGroveLinked(ctx, client, groveID)
	}

	if !groveLinked && !settings.IsHubEnabled() {
		// Grove not linked - offer to link first
		if hubsync.ShowLinkBeforeRegisterPrompt(groveName, autoConfirm) {
			// Run the link flow
			if err := runHubLink(cmd, args); err != nil {
				return fmt.Errorf("failed to link grove: %w", err)
			}
			// Reload settings after linking
			settings, err = config.LoadSettings(resolvedPath)
			if err != nil {
				return fmt.Errorf("failed to reload settings: %w", err)
			}
			groveID = settings.GroveID
		}
	}

	// Step 3: Show broker registration confirmation
	if !hubsync.ShowBrokerRegistrationPrompt(endpoint, autoConfirm) {
		return fmt.Errorf("registration cancelled")
	}

	// Get hostname for broker name
	brokerName, err := os.Hostname()
	if err != nil {
		brokerName = "local-host"
	}

	// ==== TWO-PHASE BROKER REGISTRATION ====
	credStore := brokercredentials.NewStore("")
	existingCreds, credErr := credStore.Load()

	var brokerID string
	var needsJoin bool

	// Check if we already have valid credentials
	if credErr == nil && existingCreds != nil && existingCreds.BrokerID != "" && !brokerForceRegister {
		brokerID = existingCreds.BrokerID
		fmt.Printf("Using existing broker credentials (brokerId: %s)\n", brokerID)

		// Verify the broker still exists on the hub
		_, err := client.RuntimeBrokers().Get(ctx, brokerID)
		if err != nil {
			fmt.Printf("Warning: existing broker not found on Hub, will re-register\n")
			brokerID = ""
			needsJoin = true
		}
	} else {
		needsJoin = true
	}

	// Phase 1 & 2: Create broker and complete join if needed
	if needsJoin || brokerID == "" {
		fmt.Printf("Registering broker with Hub...\n")

		// Phase 1: Create broker registration
		createReq := &hubclient.CreateBrokerRequest{
			Name: brokerName,
			Capabilities: []string{
				"sync",
				"attach",
			},
			AutoProvide: brokerAutoProvide,
		}

		createResp, err := client.RuntimeBrokers().Create(ctx, createReq)
		if err != nil {
			return fmt.Errorf("failed to create broker registration: %w", err)
		}

		fmt.Printf("Broker created (ID: %s), completing join...\n", createResp.BrokerID)

		// Build profiles from settings to send to Hub
		profiles := buildBrokerProfiles(settings)

		// Phase 2: Complete broker join with join token
		joinReq := &hubclient.JoinBrokerRequest{
			BrokerID:  createResp.BrokerID,
			JoinToken: createResp.JoinToken,
			Hostname:  brokerName,
			Version:   version.Version,
			Capabilities: []string{
				"sync",
				"attach",
			},
			Profiles: profiles,
		}

		joinResp, err := client.RuntimeBrokers().Join(ctx, joinReq)
		if err != nil {
			return fmt.Errorf("failed to complete broker join: %w", err)
		}

		brokerID = joinResp.BrokerID

		// Save credentials
		if err := credStore.SaveFromJoinResponse(brokerID, joinResp.SecretKey, endpoint); err != nil {
			fmt.Printf("Warning: failed to save broker credentials: %v\n", err)
		} else {
			fmt.Printf("Broker credentials saved to %s\n", credStore.Path())
		}
	}

	// Save broker ID to global settings
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		fmt.Printf("Warning: failed to get global directory: %v\n", err)
	} else {
		if endpoint != "" {
			if err := config.UpdateSetting(globalDir, "hub.endpoint", endpoint, true); err != nil {
				fmt.Printf("Warning: failed to save hub endpoint to global settings: %v\n", err)
			}
		}
		if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
			fmt.Printf("Warning: failed to save broker ID: %v\n", err)
		}
	}

	// If grove is linked, offer to add this broker as a provider
	if groveID != "" && settings.IsHubEnabled() {
		if hubsync.ShowGroveProviderPrompt(groveName, autoConfirm) {
			req := &hubclient.RegisterGroveRequest{
				ID:       groveID,
				Name:     groveName,
				Path:     resolvedPath,
				BrokerID: brokerID,
			}
			if !isGlobal {
				req.GitRemote = util.NormalizeGitRemote(util.GetGitRemote())
			}

			resp, err := client.Groves().Register(ctx, req)
			if err != nil {
				fmt.Printf("Warning: failed to add broker to grove: %v\n", err)
			} else {
				fmt.Printf("Broker added as provider to grove '%s'\n", resp.Grove.Name)
			}
		}
	}

	fmt.Println()
	fmt.Printf("Broker '%s' registered successfully (ID: %s)\n", brokerName, brokerID)
	if brokerAutoProvide {
		fmt.Println("Auto-provide is enabled - broker will be added to new groves automatically.")
	}
	fmt.Println("\nThe broker server will automatically connect to the Hub.")
	fmt.Println("Use 'scion hub status' to check the connection status.")

	return nil
}

func runBrokerDeregister(cmd *cobra.Command, args []string) error {
	// Check for existing broker credentials
	credStore := brokercredentials.NewStore("")
	creds, credErr := credStore.Load()

	// Also check global settings for broker ID
	globalDir, globalErr := config.GetGlobalDir()
	var brokerID string

	if credErr == nil && creds != nil && creds.BrokerID != "" {
		brokerID = creds.BrokerID
	} else if globalErr == nil {
		globalSettings, err := config.LoadSettings(globalDir)
		if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
			brokerID = globalSettings.Hub.BrokerID
		}
	}

	if brokerID == "" {
		return fmt.Errorf("no broker registration found.\n\nThis host is not registered as a Runtime Broker with the Hub.")
	}

	// Load settings for Hub client
	resolvedPath, _, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check local broker-server health (warning only)
	health, err := checkLocalBrokerServer(DefaultBrokerPort)
	if err != nil {
		fmt.Printf("Note: Broker server is not running (port %d)\n", DefaultBrokerPort)
	} else {
		fmt.Printf("Broker server is running (status: %s)\n", health.Status)
	}

	// Fetch list of groves this broker provides for
	var groveNames []string
	grovesResp, err := client.RuntimeBrokers().ListGroves(ctx, brokerID)
	if err != nil {
		util.Debugf("Warning: failed to list broker groves: %v", err)
	} else if grovesResp != nil {
		for _, g := range grovesResp.Groves {
			groveNames = append(groveNames, g.GroveName)
		}
	}

	// Show confirmation prompt with grove list
	if !hubsync.ShowBrokerDeregistrationPrompt(brokerID, groveNames, autoConfirm) {
		return fmt.Errorf("deregistration cancelled")
	}

	// Delete the broker from Hub
	if err := client.RuntimeBrokers().Delete(ctx, brokerID); err != nil {
		return fmt.Errorf("deregistration failed: %w", err)
	}

	// Clear local credentials
	if err := credStore.Delete(); err != nil {
		fmt.Printf("Warning: failed to delete local credentials: %v\n", err)
	}

	// Clear global settings
	if globalErr == nil {
		_ = config.UpdateSetting(globalDir, "hub.brokerToken", "", true)
		_ = config.UpdateSetting(globalDir, "hub.brokerId", "", true)
	}

	fmt.Println()
	fmt.Printf("Broker '%s' has been deregistered from the Hub.\n", brokerID)
	fmt.Println("Local broker credentials have been cleared.")
	if len(groveNames) > 0 {
		fmt.Printf("The broker has been removed from %d grove(s).\n", len(groveNames))
	}

	return nil
}

func runBrokerStart(cmd *cobra.Command, args []string) error {
	// Get global directory for daemon files
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Foreground mode - just run the server command directly
	if brokerStartForeground {
		// Build args for server start (just the flags, no command names)
		serverArgs := []string{"--enable-runtime-broker"}
		if brokerStartPort != DefaultBrokerPort {
			serverArgs = append(serverArgs, fmt.Sprintf("--runtime-broker-port=%d", brokerStartPort))
		}
		if brokerStartAutoProvide {
			serverArgs = append(serverArgs, "--auto-provide")
		}
		if brokerStartDebug {
			serverArgs = append(serverArgs, "--debug")
		}

		fmt.Printf("Starting broker in foreground on port %d...\n", brokerStartPort)
		fmt.Println("Press Ctrl+C to stop.")
		fmt.Println()

		// Parse the flags for serverStartCmd before calling RunE
		// (SetArgs only works with Execute, not RunE)
		if err := serverStartCmd.ParseFlags(serverArgs); err != nil {
			return fmt.Errorf("failed to parse server flags: %w", err)
		}
		return serverStartCmd.RunE(serverStartCmd, []string{})
	}

	// Daemon mode
	// Check if already running
	running, pid, _ := daemon.Status(globalDir)
	if running {
		return fmt.Errorf("broker is already running (PID: %d)\n\nUse 'kill %d' to stop it, or check the log at %s",
			pid, pid, daemon.GetLogPath(globalDir))
	}

	// Find the scion executable
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Build args for the daemon process
	daemonArgs := []string{"server", "start", "--enable-runtime-broker"}
	if brokerStartPort != DefaultBrokerPort {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--runtime-broker-port=%d", brokerStartPort))
	}
	if brokerStartAutoProvide {
		daemonArgs = append(daemonArgs, "--auto-provide")
	}
	if brokerStartDebug {
		daemonArgs = append(daemonArgs, "--debug")
	}

	// Start daemon
	fmt.Printf("Starting broker as daemon on port %d...\n", brokerStartPort)
	if err := daemon.Start(executable, daemonArgs, globalDir); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Verify it started
	time.Sleep(500 * time.Millisecond)
	running, pid, err = daemon.Status(globalDir)
	if !running {
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPath(globalDir))
	}

	fmt.Printf("Broker started (PID: %d)\n", pid)
	fmt.Printf("Log file: %s\n", daemon.GetLogPath(globalDir))
	fmt.Printf("PID file: %s\n", daemon.GetPIDPath(globalDir))
	fmt.Println()
	fmt.Println("Use 'scion broker stop' to stop the daemon.")
	fmt.Println()

	// Show broker status
	return runBrokerStatus(cmd, args)
}

func runBrokerStop(cmd *cobra.Command, args []string) error {
	// Get global directory for daemon files
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Check if daemon is running
	running, pid, _ := daemon.Status(globalDir)
	if !running {
		// Check if server is running on the port (might be foreground)
		health, err := checkLocalBrokerServer(DefaultBrokerPort)
		if err == nil {
			return fmt.Errorf("broker server is running (status: %s) but not as a daemon.\n\nIf running in foreground, use Ctrl+C to stop it.", health.Status)
		}
		return fmt.Errorf("broker daemon is not running")
	}

	fmt.Printf("Stopping broker daemon (PID: %d)...\n", pid)

	if err := daemon.Stop(globalDir); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	// Verify it stopped
	time.Sleep(500 * time.Millisecond)
	running, _, _ = daemon.Status(globalDir)
	if running {
		return fmt.Errorf("daemon may still be running. Check with 'scion broker status'")
	}

	fmt.Println("Broker daemon stopped.")
	return nil
}

func runBrokerProvide(cmd *cobra.Command, args []string) error {
	var brokerID string
	var brokerName string
	isRemoteBroker := brokerBrokerID != ""

	if isRemoteBroker {
		// Use the broker ID from --broker flag
		brokerID = brokerBrokerID
		// Broker name will be fetched from Hub below
	} else {
		// Get broker ID from local credentials
		credStore := brokercredentials.NewStore("")
		creds, credErr := credStore.Load()

		globalDir, globalErr := config.GetGlobalDir()

		if credErr == nil && creds != nil && creds.BrokerID != "" {
			brokerID = creds.BrokerID
		} else if globalErr == nil {
			globalSettings, err := config.LoadSettings(globalDir)
			if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
				brokerID = globalSettings.Hub.BrokerID
			}
		}

		if brokerID == "" {
			return fmt.Errorf("no broker registration found.\n\nRegister with: scion broker register\nOr specify a broker with --broker <name-or-id>")
		}

		// Get broker name for display
		brokerName, _ = os.Hostname()
		if brokerName == "" {
			brokerName = brokerID[:8]
		}
	}

	// Resolve grove ID
	var groveID string
	var groveName string

	if brokerGroveID != "" {
		groveID = brokerGroveID
		groveName = groveID // Will be updated after fetching
	} else {
		// Use current grove
		resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
		if err != nil {
			return fmt.Errorf("failed to resolve grove path: %w\n\nSpecify a grove with --grove <name-or-id>", err)
		}

		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		groveID = settings.GroveID
		if groveID == "" {
			return fmt.Errorf("current grove is not linked to the Hub.\n\nLink it first with: scion hub link\nOr specify a grove with --grove <name-or-id>")
		}

		// Get grove name for display
		if isGlobal {
			groveName = "global"
		} else {
			gitRemote := util.GetGitRemote()
			if gitRemote != "" {
				groveName = util.ExtractRepoName(gitRemote)
			} else {
				groveName = filepath.Base(filepath.Dir(resolvedPath))
			}
		}
	}

	// Load settings for Hub client
	resolvedPath, _, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// If we used --grove flag, resolve grove by name or ID
	if brokerGroveID != "" {
		grove, err := resolveGroveByNameOrID(ctx, client, brokerGroveID)
		if err != nil {
			return fmt.Errorf("failed to find grove '%s': %w", brokerGroveID, err)
		}
		groveID = grove.ID
		groveName = grove.Name
	}

	// If we used --broker flag, resolve broker by name or ID
	if isRemoteBroker {
		broker, err := resolveBrokerByNameOrID(ctx, client, brokerBrokerID)
		if err != nil {
			return fmt.Errorf("failed to find broker '%s': %w", brokerBrokerID, err)
		}
		brokerID = broker.ID
		brokerName = broker.Name
		if brokerName == "" {
			brokerName = brokerID[:8]
		}
	}

	// Show confirmation prompt
	if !hubsync.ShowProvidePrompt(groveName, brokerName, autoConfirm) {
		return fmt.Errorf("operation cancelled")
	}

	// Add broker as provider
	req := &hubclient.RegisterGroveRequest{
		ID:       groveID,
		Name:     groveName,
		BrokerID: brokerID,
	}

	resp, err := client.Groves().Register(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to add broker as provider: %w", err)
	}

	fmt.Println()
	fmt.Printf("Broker '%s' added as provider for grove '%s'\n", brokerName, resp.Grove.Name)

	return nil
}

func runBrokerWithdraw(cmd *cobra.Command, args []string) error {
	var brokerID string
	var brokerName string
	isRemoteBroker := brokerBrokerID != ""

	if isRemoteBroker {
		// Use the broker ID from --broker flag
		brokerID = brokerBrokerID
		// Broker name will be fetched from Hub below
	} else {
		// Get broker ID from local credentials
		credStore := brokercredentials.NewStore("")
		creds, credErr := credStore.Load()

		globalDir, globalErr := config.GetGlobalDir()

		if credErr == nil && creds != nil && creds.BrokerID != "" {
			brokerID = creds.BrokerID
		} else if globalErr == nil {
			globalSettings, err := config.LoadSettings(globalDir)
			if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
				brokerID = globalSettings.Hub.BrokerID
			}
		}

		if brokerID == "" {
			return fmt.Errorf("no broker registration found.\n\nRegister with: scion broker register\nOr specify a broker with --broker <name-or-id>")
		}

		// Get broker name for display
		brokerName, _ = os.Hostname()
		if brokerName == "" {
			brokerName = brokerID[:8]
		}
	}

	// Resolve grove ID
	var groveID string
	var groveName string

	if brokerGroveID != "" {
		groveID = brokerGroveID
		groveName = groveID // Will be updated after fetching
	} else {
		// Use current grove
		resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
		if err != nil {
			return fmt.Errorf("failed to resolve grove path: %w\n\nSpecify a grove with --grove <name-or-id>", err)
		}

		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		groveID = settings.GroveID
		if groveID == "" {
			return fmt.Errorf("current grove is not linked to the Hub.\n\nSpecify a grove with --grove <name-or-id>")
		}

		// Get grove name for display
		if isGlobal {
			groveName = "global"
		} else {
			gitRemote := util.GetGitRemote()
			if gitRemote != "" {
				groveName = util.ExtractRepoName(gitRemote)
			} else {
				groveName = filepath.Base(filepath.Dir(resolvedPath))
			}
		}
	}

	// Load settings for Hub client
	resolvedPath, _, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// If we used --grove flag, resolve grove by name or ID
	if brokerGroveID != "" {
		grove, err := resolveGroveByNameOrID(ctx, client, brokerGroveID)
		if err != nil {
			return fmt.Errorf("failed to find grove '%s': %w", brokerGroveID, err)
		}
		groveID = grove.ID
		groveName = grove.Name
	}

	// If we used --broker flag, resolve broker by name or ID
	if isRemoteBroker {
		broker, err := resolveBrokerByNameOrID(ctx, client, brokerBrokerID)
		if err != nil {
			return fmt.Errorf("failed to find broker '%s': %w", brokerBrokerID, err)
		}
		brokerID = broker.ID
		brokerName = broker.Name
		if brokerName == "" {
			brokerName = brokerID[:8]
		}
	}

	// Show confirmation prompt
	if !hubsync.ShowWithdrawPrompt(groveName, brokerName, autoConfirm) {
		return fmt.Errorf("operation cancelled")
	}

	// Remove broker as provider
	if err := client.Groves().RemoveProvider(ctx, groveID, brokerID); err != nil {
		return fmt.Errorf("failed to remove broker as provider: %w", err)
	}

	fmt.Println()
	fmt.Printf("Broker '%s' removed as provider from grove '%s'\n", brokerName, groveName)

	return nil
}

func runBrokerStatus(cmd *cobra.Command, args []string) error {
	// If --broker flag is provided, show remote broker status
	if brokerBrokerID != "" {
		return runRemoteBrokerStatus(brokerBrokerID)
	}

	// Get global directory for daemon files
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Collect status information
	status := brokerStatusInfo{}

	// Check daemon status
	running, pid, _ := daemon.Status(globalDir)
	status.DaemonRunning = running
	status.DaemonPID = pid
	if running {
		status.LogFile = daemon.GetLogPath(globalDir)
		status.PIDFile = daemon.GetPIDPath(globalDir)
	}

	// Check if broker server is responding (could be foreground or daemon)
	health, err := checkLocalBrokerServer(DefaultBrokerPort)
	if err == nil {
		status.ServerRunning = true
		status.ServerPort = DefaultBrokerPort
		status.ServerStatus = health.Status
		status.ServerVersion = health.Version
	}

	// Get broker credentials
	credStore := brokercredentials.NewStore("")
	creds, credErr := credStore.Load()

	if credErr == nil && creds != nil && creds.BrokerID != "" {
		status.BrokerID = creds.BrokerID
		status.HubEndpoint = creds.HubEndpoint
		status.Registered = true
		status.CredentialsPath = credStore.Path()
	} else {
		// Check global settings for broker ID
		globalSettings, err := config.LoadSettings(globalDir)
		if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
			status.BrokerID = globalSettings.Hub.BrokerID
			status.HubEndpoint = globalSettings.Hub.Endpoint
			status.Registered = true
		}
	}

	// Get broker name
	status.Hostname, _ = os.Hostname()

	// If registered, try to get Hub status and grove list
	if status.Registered && status.HubEndpoint != "" {
		resolvedPath, _, _ := config.ResolveGrovePath(grovePath)
		settings, err := config.LoadSettings(resolvedPath)
		if err == nil {
			client, err := getHubClient(settings)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				// Check Hub connectivity
				hubHealth, err := client.Health(ctx)
				if err == nil {
					status.HubConnected = true
					status.HubStatus = hubHealth.Status
					status.HubVersion = hubHealth.Version
				}

				// Get broker info from Hub
				if status.BrokerID != "" {
					broker, err := client.RuntimeBrokers().Get(ctx, status.BrokerID)
					if err == nil {
						status.BrokerName = broker.Name
						status.BrokerStatus = broker.Status
						status.LastHeartbeat = broker.LastHeartbeat
					} else if apiclient.IsNotFoundError(err) {
						// Broker ID exists locally but not on Hub - not actually registered
						status.Registered = false
					}

					// Get groves this broker provides for (only if still registered)
					if status.Registered {
						grovesResp, err := client.RuntimeBrokers().ListGroves(ctx, status.BrokerID)
						if err == nil && grovesResp != nil {
							for _, g := range grovesResp.Groves {
								status.Groves = append(status.Groves, brokerGroveStatus{
									ID:   g.GroveID,
									Name: g.GroveName,
								})
							}
						}
					}
				}
			}
		}
	}

	// Output
	if brokerStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Text output
	fmt.Println("Runtime Broker Status")
	fmt.Println("=====================")
	fmt.Println()

	// Server status
	fmt.Println("Server")
	fmt.Println("------")
	if status.ServerRunning {
		fmt.Printf("  Running:     yes (port %d)\n", status.ServerPort)
		fmt.Printf("  Status:      %s\n", status.ServerStatus)
		fmt.Printf("  Version:     %s\n", status.ServerVersion)
	} else {
		fmt.Printf("  Running:     no\n")
	}

	if status.DaemonRunning {
		fmt.Printf("  Daemon PID:  %d\n", status.DaemonPID)
		fmt.Printf("  Log file:    %s\n", status.LogFile)
	} else if status.ServerRunning {
		fmt.Printf("  Mode:        foreground (or external)\n")
	}
	fmt.Println()

	// Registration status
	fmt.Println("Hub Registration")
	fmt.Println("----------------")
	if status.Registered {
		fmt.Printf("  Registered:  yes\n")
		fmt.Printf("  Broker ID:   %s\n", status.BrokerID)
		if status.BrokerName != "" {
			fmt.Printf("  Broker Name: %s\n", status.BrokerName)
		}
		fmt.Printf("  Hub:         %s\n", status.HubEndpoint)
		if status.HubConnected {
			fmt.Printf("  Connected:   yes (%s)\n", status.HubStatus)
			if status.BrokerStatus != "" {
				fmt.Printf("  Status:      %s\n", status.BrokerStatus)
			}
			if !status.LastHeartbeat.IsZero() {
				fmt.Printf("  Last seen:   %s\n", formatRelativeTime(status.LastHeartbeat))
			}
		} else {
			fmt.Printf("  Connected:   no (Hub unreachable)\n")
		}
	} else {
		fmt.Printf("  Registered:  no\n")
		fmt.Printf("\n  Run 'scion broker register' to register with the Hub.\n")
	}
	fmt.Println()

	// Groves
	if len(status.Groves) > 0 {
		fmt.Println("Groves (Provider)")
		fmt.Println("-----------------")
		for _, g := range status.Groves {
			fmt.Printf("  - %s (ID: %s)\n", g.Name, g.ID)
		}
	} else if status.Registered {
		fmt.Println("Groves (Provider)")
		fmt.Println("-----------------")
		fmt.Printf("  (none)\n")
		fmt.Printf("\n  Run 'scion broker provide' to add this broker as a provider for a grove.\n")
	}

	return nil
}

// runRemoteBrokerStatus fetches and displays status for a remote broker from the Hub
func runRemoteBrokerStatus(brokerID string) error {
	// Load settings for Hub client
	resolvedPath, _, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fetch broker info from Hub
	broker, err := client.RuntimeBrokers().Get(ctx, brokerID)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("broker '%s' not found on Hub", brokerID)
		}
		return fmt.Errorf("failed to fetch broker: %w", err)
	}

	// Collect status information
	status := brokerStatusInfo{
		Registered:    true,
		BrokerID:      broker.ID,
		BrokerName:    broker.Name,
		BrokerStatus:  broker.Status,
		LastHeartbeat: broker.LastHeartbeat,
		HubEndpoint:   GetHubEndpoint(settings),
		HubConnected:  true, // We just connected successfully
	}

	// Get groves this broker provides for
	grovesResp, err := client.RuntimeBrokers().ListGroves(ctx, brokerID)
	if err == nil && grovesResp != nil {
		for _, g := range grovesResp.Groves {
			status.Groves = append(status.Groves, brokerGroveStatus{
				ID:   g.GroveID,
				Name: g.GroveName,
			})
		}
	}

	// Output
	if brokerStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Text output
	fmt.Printf("Runtime Broker Status (Remote: %s)\n", brokerID)
	fmt.Println("======================================")
	fmt.Println()

	// Registration status
	fmt.Println("Broker Info")
	fmt.Println("-----------")
	fmt.Printf("  Broker ID:   %s\n", status.BrokerID)
	if status.BrokerName != "" {
		fmt.Printf("  Name:        %s\n", status.BrokerName)
	}
	fmt.Printf("  Status:      %s\n", status.BrokerStatus)
	if !status.LastHeartbeat.IsZero() {
		fmt.Printf("  Last seen:   %s\n", formatRelativeTime(status.LastHeartbeat))
	}
	fmt.Printf("  Hub:         %s\n", status.HubEndpoint)
	fmt.Println()

	// Groves
	if len(status.Groves) > 0 {
		fmt.Println("Groves (Provider)")
		fmt.Println("-----------------")
		for _, g := range status.Groves {
			fmt.Printf("  - %s (ID: %s)\n", g.Name, g.ID)
		}
	} else {
		fmt.Println("Groves (Provider)")
		fmt.Println("-----------------")
		fmt.Printf("  (none)\n")
	}

	return nil
}

// brokerStatusInfo holds the status information for JSON output
type brokerStatusInfo struct {
	// Server status
	ServerRunning bool   `json:"serverRunning"`
	ServerPort    int    `json:"serverPort,omitempty"`
	ServerStatus  string `json:"serverStatus,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`

	// Daemon status
	DaemonRunning bool   `json:"daemonRunning"`
	DaemonPID     int    `json:"daemonPid,omitempty"`
	LogFile       string `json:"logFile,omitempty"`
	PIDFile       string `json:"pidFile,omitempty"`

	// Registration status
	Registered      bool   `json:"registered"`
	BrokerID        string `json:"brokerId,omitempty"`
	BrokerName      string `json:"brokerName,omitempty"`
	BrokerStatus    string `json:"brokerStatus,omitempty"`
	Hostname        string `json:"hostname,omitempty"`
	CredentialsPath string `json:"credentialsPath,omitempty"`

	// Hub connection
	HubEndpoint   string    `json:"hubEndpoint,omitempty"`
	HubConnected  bool      `json:"hubConnected"`
	HubStatus     string    `json:"hubStatus,omitempty"`
	HubVersion    string    `json:"hubVersion,omitempty"`
	LastHeartbeat time.Time `json:"lastHeartbeat,omitempty"`

	// Groves
	Groves []brokerGroveStatus `json:"groves,omitempty"`
}

// brokerGroveStatus holds grove info for status output
type brokerGroveStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// resolveGroveByNameOrID resolves a grove identifier (name or ID) to a grove.
// It first attempts to fetch by ID, and if that fails with a 404, tries to find by name.
// Returns the grove if found, or an error if not found or multiple matches.
func resolveGroveByNameOrID(ctx context.Context, client hubclient.Client, nameOrID string) (*hubclient.Grove, error) {
	// First try to fetch by ID directly
	grove, err := client.Groves().Get(ctx, nameOrID)
	if err == nil {
		return grove, nil
	}

	// If not a 404, return the error
	if !apiclient.IsNotFoundError(err) {
		return nil, err
	}

	// ID not found, try to find by name
	resp, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{
		Name: nameOrID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for grove by name: %w", err)
	}

	switch len(resp.Groves) {
	case 0:
		return nil, fmt.Errorf("grove '%s' not found", nameOrID)
	case 1:
		return &resp.Groves[0], nil
	default:
		return nil, fmt.Errorf("multiple groves found with name '%s' - please use the grove ID instead", nameOrID)
	}
}

// resolveBrokerByNameOrID resolves a broker identifier (name or ID) to a broker.
// It first attempts to fetch by ID, and if that fails with a 404, tries to find by name.
// Returns the broker if found, or an error if not found or multiple matches.
func resolveBrokerByNameOrID(ctx context.Context, client hubclient.Client, nameOrID string) (*hubclient.RuntimeBroker, error) {
	// First try to fetch by ID directly
	broker, err := client.RuntimeBrokers().Get(ctx, nameOrID)
	if err == nil {
		return broker, nil
	}

	// If not a 404, return the error
	if !apiclient.IsNotFoundError(err) {
		return nil, err
	}

	// ID not found, try to find by name
	resp, err := client.RuntimeBrokers().List(ctx, &hubclient.ListBrokersOptions{
		Name: nameOrID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for broker by name: %w", err)
	}

	switch len(resp.Brokers) {
	case 0:
		return nil, fmt.Errorf("broker '%s' not found", nameOrID)
	case 1:
		return &resp.Brokers[0], nil
	default:
		return nil, fmt.Errorf("multiple brokers found with name '%s' - please use the broker ID instead", nameOrID)
	}
}

// buildBrokerProfiles builds BrokerProfile objects from settings.Profiles.
// It converts the user-defined profiles in settings.yaml to the format expected by the Hub.
func buildBrokerProfiles(settings *config.Settings) []hubclient.BrokerProfile {
	if settings == nil || len(settings.Profiles) == 0 {
		return nil
	}

	var profiles []hubclient.BrokerProfile
	for name, profileCfg := range settings.Profiles {
		// Determine runtime type from the profile's runtime reference
		runtimeType := profileCfg.Runtime
		if runtimeType == "" {
			runtimeType = "docker" // default
		}

		// Look up runtime config to get additional info (context, namespace for K8s)
		var context, namespace string
		if settings.Runtimes != nil {
			if rtCfg, ok := settings.Runtimes[profileCfg.Runtime]; ok {
				context = rtCfg.Context
				namespace = rtCfg.Namespace
			}
		}

		profiles = append(profiles, hubclient.BrokerProfile{
			Name:      name,
			Type:      runtimeType,
			Available: true,
			Context:   context,
			Namespace: namespace,
		})
	}

	return profiles
}
