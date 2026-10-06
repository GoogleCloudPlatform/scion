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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	slashpath "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	// broker register flags
	brokerForceRegister bool
	brokerAutoProvide   bool
	brokerHubName       string // --name flag for hub connection name

	// broker deregister flags
	brokerDeregisterBrokerOnly bool
	brokerDeregisterPurgeLocal bool
	brokerDeregisterName       string // --name flag for deregister

	// broker start flags
	brokerStartForeground  bool
	brokerStartPort        int
	brokerStartAutoProvide bool
	brokerStartDebug       bool

	// broker restart flags
	brokerRestartPort        int
	brokerRestartAutoProvide bool
	brokerRestartDebug       bool

	// broker provide/withdraw flags
	brokerProjectID   string
	brokerBrokerID    string // --broker flag for remote broker operations
	brokerMakeDefault bool   // --make-default flag to set broker as project default
	brokerHubFlag     string // --hub flag to target a specific hub connection
	brokerProvidePath string // --path flag: explicit local project path for provide

	// broker register transport flags
	brokerTransportMode     string
	brokerTransportAudience string

	// broker hubs flags
	brokerHubsJSON bool

	// --port for register, deregister, status, stop and hubs. Read through
	// resolveBrokerPort, never directly: when the flag is not set, the port
	// saved by the last 'runtime-broker start' is used.
	brokerPort int
)

// brokerCmd represents the runtime-broker command group.
// The command was renamed from "broker" to "runtime-broker" to disambiguate
// from the Message Broker plugin system. The old "broker" name is kept as a
// deprecated alias for backward compatibility.
var brokerCmd = &cobra.Command{
	Use:     "runtime-broker",
	Aliases: []string{"broker"},
	Short:   "Manage the Runtime Broker",
	Long: `Commands for managing this host as a Runtime Broker.

A Runtime Broker is a compute node that executes agents on behalf of the Hub.
Brokers register with the Hub and can be added as providers for projects.

Note: This command was previously named "broker". The old name still works
but is deprecated. Please use "scion runtime-broker" instead.

Commands:
  status       Show broker status (server, registration, projects)
  start        Start the broker server (as daemon by default)
  stop         Stop the broker daemon
  restart      Stop and restart the broker daemon
  register     Register this host as a Runtime Broker with the Hub
  deregister   Remove this broker from the Hub
  hubs         List hub connections
  provide      Add this broker as a provider for a project
  withdraw     Remove this broker as a provider from a project`,
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
- The broker server must be running (scion runtime-broker start)
- The Hub endpoint must be configured
- You must be authenticated with the Hub

This command will:
1. Verify the local broker server is running
2. Create a broker registration on the Hub
3. Complete the two-phase join process
4. Save broker credentials for future authentication

Examples:
  # Register this host as a broker
  scion runtime-broker register

  # Force re-registration even if already registered
  scion runtime-broker register --force

  # Register with auto-provide enabled
  scion runtime-broker register --auto-provide

  # Register a broker on a non-default port. Not needed when the broker
  # was started with 'scion runtime-broker start --port 19800': the port
  # used by the last start is the default.
  scion runtime-broker register --port 19800`,
	RunE: runBrokerRegister,
}

// brokerDeregisterCmd removes this broker from the Hub
var brokerDeregisterCmd = &cobra.Command{
	Use:   "deregister",
	Short: "Remove this broker from the Hub",
	Long: `Remove this broker from the Hub.

This command will:
1. Remove this broker from all projects it provides for
2. Clear the stored broker token

Use --broker-only to only remove the broker record without affecting project providers.

Local state: deregister removes this hub's credentials file, its
hub_connections entry and (when no connections remain) the empty
hub-credentials/ directory. Broker-local state stays: the broker daemon log
(broker.log), the broker's state directory (runtime-broker-state/<broker-id>)
and the broker template cache (cache/templates). Pass --purge-local to remove
them as well once the last hub connection is gone and the broker is stopped;
it also works on a host that is no longer registered. Settings files, the
hub-id file (it belongs to a local hub) and other hubs' credentials are never
removed.`,
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
  scion runtime-broker start

  # Start broker in foreground
  scion runtime-broker start --foreground

  # Start broker on custom port
  scion runtime-broker start --port 9801

  # Start broker with auto-provide enabled
  scion runtime-broker start --auto-provide`,
	RunE: runBrokerStart,
}

// brokerProvideCmd adds this broker as a provider for a project
var brokerProvideCmd = &cobra.Command{
	Use:   "provide",
	Short: "Add a broker as a provider for a project",
	Long: `Add a broker as a provider for a project.

When a broker is a provider for a project, it can execute agents
for that project. The Hub will dispatch agent operations to the
broker when agents are created in the project.

If --project is not specified, uses the current local project.
If --broker is not specified, uses the local broker registration.

Use --make-default to set the broker as the default for the project. If the
project already has a different default broker, you will be prompted to confirm
the change.

Examples:
  # Add local broker as provider for current project
  scion runtime-broker provide

  # Add local broker as provider for a specific project. No local path is
  # sent: an existing provider path for this broker is kept (a stored
  # global-directory path is cleared), otherwise the broker uses its
  # hub-managed project directory.
  scion runtime-broker provide --project <project-id>

  # Add local broker as provider for a project linked to a local checkout
  scion runtime-broker provide --project <project-id> --path /path/to/checkout

  # Add a remote broker as provider for a project (admin only)
  scion runtime-broker provide --broker <broker-id> --project <project-id>

  # Add broker as provider and set as default
  scion runtime-broker provide --make-default`,
	RunE: runBrokerProvide,
}

// brokerWithdrawCmd removes this broker as a provider from a project
var brokerWithdrawCmd = &cobra.Command{
	Use:   "withdraw",
	Short: "Remove a broker as a provider from a project",
	Long: `Remove a broker as a provider from a project.

After withdrawing, the broker will no longer receive agent dispatch
requests for the project. Existing agents on the broker will continue
running but cannot be managed through the Hub until the broker is
re-added as a provider.

If --project is not specified, uses the current local project.
If --broker is not specified, uses the local broker registration.

Examples:
  # Remove local broker as provider from current project
  scion runtime-broker withdraw

  # Remove local broker as provider from a specific project
  scion runtime-broker withdraw --project <project-id>

  # Remove a remote broker as provider from a project (admin only)
  scion runtime-broker withdraw --broker <broker-id> --project <project-id>`,
	RunE: runBrokerWithdraw,
}

// brokerHubsCmd lists all hub connections
var brokerHubsCmd = &cobra.Command{
	Use:   "hubs",
	Short: "List hub connections",
	Long: `List all hub connections for this Runtime Broker.

Each connection represents a registration with a different Hub.
Credentials are stored in ~/.scion/hub-credentials/.

Examples:
  # List all hub connections
  scion runtime-broker hubs

  # List hub connections in JSON format
  scion runtime-broker hubs --json`,
	RunE: runBrokerHubs,
}

// brokerStatusCmd shows the current broker status
var brokerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Runtime Broker status",
	Long: `Show the current status of a Runtime Broker.

This command displays:
- Whether the broker server is running (daemon or foreground)
- Hub registration status
- Projects this broker provides for
- Connection status to the Hub

If --broker is not specified, shows the local broker status.
If --broker is specified, queries the Hub for the remote broker's status.

Examples:
  # Show local broker status
  scion runtime-broker status

  # Show local broker status in JSON format
  scion runtime-broker status --json

  # Show status of a remote broker
  scion runtime-broker status --broker <broker-id>`,
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
  scion runtime-broker stop`,
	RunE: runBrokerStop,
}

// brokerRestartCmd restarts the broker daemon
var brokerRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Runtime Broker daemon",
	Long: `Restart the Runtime Broker daemon.

This command stops the currently running broker daemon and starts a new one
using the current scion binary. This is useful after installing a new version
of scion to pick up the updated binary.

If the broker is not running as a daemon, this command will return an error.

The new daemon keeps the --port, --auto-provide and --debug values the
running daemon was started with, unless you pass them again.

Examples:
  # Restart the broker daemon
  scion runtime-broker restart

  # Restart with a different port
  scion runtime-broker restart --port 9801

  # Restart with auto-provide enabled
  scion runtime-broker restart --auto-provide`,
	RunE: runBrokerRestart,
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
	brokerCmd.AddCommand(brokerRestartCmd)
	brokerCmd.AddCommand(brokerHubsCmd)

	// Restart flags
	brokerRestartCmd.Flags().IntVar(&brokerRestartPort, "port", 0, "Runtime Broker API port; when not set, the port the running daemon was started with, else server.broker.port from settings, else 9800")
	brokerRestartCmd.Flags().BoolVar(&brokerRestartAutoProvide, "auto-provide", false, "Automatically add as provider for new projects; when not set, as the running daemon was started")
	brokerRestartCmd.Flags().BoolVar(&brokerRestartDebug, "debug", false, "Enable debug logging (verbose output); when not set, as the running daemon was started")

	// Status flags
	brokerStatusCmd.Flags().BoolVar(&brokerStatusJSON, "json", false, "Output in JSON format")
	brokerStatusCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker ID to query (for remote broker status)")

	// Local broker port for the subcommands that talk to a running broker.
	for _, c := range []*cobra.Command{brokerRegisterCmd, brokerDeregisterCmd, brokerStatusCmd, brokerStopCmd, brokerHubsCmd} {
		c.Flags().IntVar(&brokerPort, "port", 0, brokerPortFlagUsage)
	}

	// Register flags
	brokerRegisterCmd.Flags().BoolVar(&brokerForceRegister, "force", false, "Force re-registration even if already registered")
	brokerRegisterCmd.Flags().BoolVar(&brokerAutoProvide, "auto-provide", false, "Automatically add as provider for new projects")
	brokerRegisterCmd.Flags().StringVar(&brokerHubName, "name", "", "Name for this hub connection (derived from endpoint if not specified)")
	brokerRegisterCmd.Flags().StringVar(&brokerTransportMode, "transport-mode", "", "Transport auth mode: 'iap' or 'cloudrun_invoker' (overrides SCION_TRANSPORT_MODE)")
	brokerRegisterCmd.Flags().StringVar(&brokerTransportAudience, "transport-audience", "", "Transport auth OIDC audience (overrides SCION_TRANSPORT_AUDIENCE)")

	// Deregister flags
	brokerDeregisterCmd.Flags().BoolVar(&brokerDeregisterBrokerOnly, "broker-only", false, "Only remove broker record, not project providers")
	brokerDeregisterCmd.Flags().StringVar(&brokerDeregisterName, "name", "", "Name of the hub connection to deregister")
	brokerDeregisterCmd.Flags().BoolVar(&brokerDeregisterPurgeLocal, "purge-local", false, "After deregistering the last hub connection, also remove broker-local state (broker.log, runtime-broker-state/<broker-id>, cache/templates); never settings, the hub-id file or other hubs' credentials. Skipped while the broker is running")

	// Hubs flags
	brokerHubsCmd.Flags().BoolVar(&brokerHubsJSON, "json", false, "Output in JSON format")

	// Start flags
	brokerStartCmd.Flags().BoolVar(&brokerStartForeground, "foreground", false, "Run in foreground instead of as daemon")
	brokerStartCmd.Flags().IntVar(&brokerStartPort, "port", 0, "Runtime Broker API port; when not set, server.broker.port from settings, else 9800")
	brokerStartCmd.Flags().BoolVar(&brokerStartAutoProvide, "auto-provide", false, "Automatically add as provider for new projects")
	brokerStartCmd.Flags().BoolVar(&brokerStartDebug, "debug", false, "Enable debug logging (verbose output)")

	// Provide/withdraw flags
	brokerProvideCmd.Flags().StringVar(&brokerProjectID, "project", "", "Project name or ID to add as provider for")

	brokerProvideCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker name or ID to use (for remote broker operations)")
	brokerProvideCmd.Flags().BoolVar(&brokerMakeDefault, "make-default", false, "Set this broker as the default for the project")
	brokerProvideCmd.Flags().StringVar(&brokerHubFlag, "hub", "", "Hub connection name (from 'scion runtime-broker hubs')")
	brokerProvideCmd.Flags().StringVar(&brokerProvidePath, "path", "", "Project path to register for this broker, resolved on this host (default with --project: none sent; an existing provider path is kept unless it is the global directory, otherwise the broker uses its hub-managed project directory). With --broker naming another host's broker, give the absolute path to the project root (the directory containing .scion) on that host; it is sent as given")

	brokerWithdrawCmd.Flags().StringVar(&brokerProjectID, "project", "", "Project name or ID to remove as provider from")

	brokerWithdrawCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker name or ID to use (for remote broker operations)")
	brokerWithdrawCmd.Flags().StringVar(&brokerHubFlag, "hub", "", "Hub connection name (from 'scion runtime-broker hubs')")
}

func runBrokerRegister(cmd *cobra.Command, args []string) error {
	// Resolve project path to find project settings (needed for Hub endpoint config)
	gp := projectPath
	if gp == "" && globalMode {
		gp = "global"
	}

	resolvedPath, isGlobal, err := config.ResolveProjectPath(gp)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return fmt.Errorf("hub endpoint not configured; configure via SCION_HUB_ENDPOINT, hub.endpoint in settings.yaml, or --hub flag")
	}

	// Step 1: Check if local broker server is running
	port := resolveBrokerPort(cmd)
	health, err := checkLocalBrokerServer(port)
	if err != nil {
		return fmt.Errorf("broker server not running on port %d.\n\nStart it with: scion runtime-broker start (or pass --port if it runs on another port)\n\nError: %w", port, err)
	}
	fmt.Printf("Broker server is running (status: %s, version: %s)\n", health.Status, health.Version)

	// Step 2: Check if project is linked to Hub
	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Check Hub connectivity
	if _, err := client.Health(ctx); err != nil {
		return fmt.Errorf("hub at %s is not responding: %w", endpoint, err)
	}

	// Get project name for display
	var projectName string
	if isGlobal {
		projectName = "global"
	} else {
		gitRemote := util.GetGitRemote()
		if gitRemote != "" {
			projectName = util.ExtractRepoName(gitRemote)
		} else {
			projectName = config.GetProjectName(resolvedPath)
		}
	}

	// Check if project is linked — prefer hub.projectId over project_id
	projectID := settings.GetHubProjectID()
	if projectID == "" {
		projectID = settings.ProjectID
	}
	projectLinked := false
	if projectID != "" {
		projectLinked, _ = isProjectLinked(ctx, client, projectID)
	}

	// The global directory is a local pseudo-project: never offer to link
	// it, which would create a hub project named "global" (ptone/scion#3534).
	if !projectLinked && !settings.IsHubEnabled() && !isGlobal {
		// Project not linked - offer to link first
		if hubsync.ShowLinkBeforeRegisterPrompt(projectName, autoConfirm) {
			// Run the link flow
			if err := runHubLink(cmd, args); err != nil {
				return fmt.Errorf("failed to link project: %w", err)
			}
			// Reload settings after linking
			settings, err = config.LoadSettings(resolvedPath)
			if err != nil {
				return fmt.Errorf("failed to reload settings: %w", err)
			}
			projectID = settings.GetHubProjectID()
			if projectID == "" {
				projectID = settings.ProjectID
			}
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

	// Get global directory early (needed for stable broker ID)
	globalDir, globalDirErr := config.GetGlobalDir()

	// Initialize MultiStore with auto-migration from legacy single-file store.
	multiStore, migrated, migrationErr := initializeBrokerCredentialStore()
	if migrationErr != nil {
		fmt.Printf("Warning: failed to migrate legacy credentials: %v\n", migrationErr)
	} else if migrated {
		fmt.Printf("Migrated legacy credentials to %s\n", multiStore.Dir())
	}

	// Determine hub connection name
	hubName := brokerHubName
	if hubName == "" {
		hubName = brokercredentials.DeriveHubName(endpoint)
	}
	if hubName == "" {
		hubName = "default"
	}

	// Get or generate a stable broker UUID
	var stableBrokerID string
	if globalDirErr == nil {
		globalSettings, gsErr := config.LoadSettings(globalDir)
		if gsErr == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
			stableBrokerID = globalSettings.Hub.BrokerID
		}
	}
	if stableBrokerID == "" {
		stableBrokerID = uuid.New().String()
	}

	// Check for existing credentials for this hub connection
	existingCreds, credErr := multiStore.Load(hubName)

	var brokerID string
	var needsJoin bool

	if credErr == nil && existingCreds != nil && existingCreds.BrokerID != "" && !brokerForceRegister {
		brokerID = existingCreds.BrokerID
		fmt.Printf("Using existing broker credentials for '%s' (brokerId: %s)\n", hubName, brokerID)

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
			BrokerID:     stableBrokerID,
			Name:         brokerName,
			Capabilities: brokerRegistrationCapabilities(),
			AutoProvide:  brokerAutoProvide,
			Labels: map[string]string{
				"scion.io/broker-role": "remote",
			},
		}

		createResp, err := client.RuntimeBrokers().Create(ctx, createReq)
		if err != nil {
			return fmt.Errorf("failed to create broker registration: %w", err)
		}

		if createResp.Reregistered {
			fmt.Printf("Found existing broker registration for '%s' (ID: %s), re-registering...\n", brokerName, createResp.BrokerID)
		} else {
			fmt.Printf("Broker created (ID: %s), completing join...\n", createResp.BrokerID)
		}

		// Build profiles from settings to send to Hub
		profiles := buildBrokerProfiles(settings)

		// Phase 2: Complete broker join with join token
		joinReq := &hubclient.JoinBrokerRequest{
			BrokerID:         createResp.BrokerID,
			JoinToken:        createResp.JoinToken,
			Hostname:         brokerName,
			Version:          version.Version,
			Capabilities:     brokerRegistrationCapabilities(),
			Profiles:         profiles,
			WorkspaceStorage: loadBrokerRegistrationWorkspaceStorage(),
			DefaultProfile:   brokerRegistrationDefaultProfile(settings),
		}

		joinResp, err := client.RuntimeBrokers().Join(ctx, joinReq)
		if err != nil {
			return fmt.Errorf("failed to complete broker join: %w", err)
		}

		brokerID = joinResp.BrokerID

		// Resolve transport config from flags, then env
		transportMode := brokerTransportMode
		if transportMode == "" {
			transportMode = os.Getenv(transportauth.EnvTransportMode)
		}
		transportAudience := brokerTransportAudience
		if transportAudience == "" {
			transportAudience = os.Getenv(transportauth.EnvTransportAudience)
		}

		// Save credentials to MultiStore
		newCreds := &brokercredentials.BrokerCredentials{
			Name:              hubName,
			BrokerID:          brokerID,
			SecretKey:         joinResp.SecretKey,
			HubEndpoint:       endpoint,
			AuthMode:          brokercredentials.AuthModeHMAC,
			RegisteredAt:      time.Now(),
			TransportMode:     transportMode,
			TransportAudience: transportAudience,
		}
		if err := multiStore.Save(newCreds); err != nil {
			fmt.Printf("Warning: failed to save broker credentials: %v\n", err)
		} else {
			fmt.Printf("Broker credentials saved to %s\n", multiStore.Dir())
		}
	}

	// Save broker ID to global settings
	if globalDirErr != nil {
		fmt.Printf("Warning: failed to get global directory: %v\n", globalDirErr)
	} else {
		if endpoint != "" {
			if err := config.UpdateSetting(globalDir, "hub.endpoint", endpoint, true); err != nil {
				fmt.Printf("Warning: failed to save hub endpoint to global settings: %v\n", err)
			}
		}
		if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
			fmt.Printf("Warning: failed to save broker ID: %v\n", err)
		}
		// Write hub_connections entry for this registration
		if err := config.UpdateSetting(globalDir, "hub_connections."+hubName+".endpoint", endpoint, true); err != nil {
			fmt.Printf("Warning: failed to save hub connection to settings: %v\n", err)
		}
	}

	// If project is linked, offer to add this broker as a provider. The
	// global directory is a local pseudo-project, never a hub project, so a
	// --global register (or one resolved to the global directory) skips this
	// step instead of offering, and creating, a hub project named "global"
	// (ptone/scion#3534).
	if shouldOfferProjectProvider(projectID, settings.IsHubEnabled(), isGlobal) {
		if hubsync.ShowProjectProviderPrompt(projectName, autoConfirm) {
			req := &hubclient.RegisterProjectRequest{
				ID:        projectID,
				Name:      projectName,
				Path:      resolvedPath,
				BrokerID:  brokerID,
				GitRemote: util.NormalizeGitRemote(util.GetGitRemote()),
			}

			resp, err := client.Projects().Register(ctx, req)
			if err != nil {
				fmt.Printf("Warning: failed to add broker to project: %v\n", err)
			} else {
				fmt.Printf("Broker added as provider to project '%s'\n", resp.Project.Name)
			}
		}
	}

	if isGlobal {
		fmt.Println("Skipped project linking: the global directory is a local pseudo-project, not a hub project.")
		fmt.Println("Use 'scion runtime-broker provide --project <name>' to add this broker as a provider for a hub project.")
	}

	fmt.Println()
	fmt.Printf("Broker '%s' registered successfully (ID: %s)\n", brokerName, brokerID)
	if brokerAutoProvide {
		fmt.Println("Auto-provide is enabled - broker will be added to new projects automatically.")
	}
	fmt.Println("\nThe broker server will automatically connect to the Hub.")
	fmt.Println("Use 'scion hub status' to check the connection status.")

	return nil
}

func runBrokerDeregister(cmd *cobra.Command, args []string) error {
	// Initialize MultiStore with auto-migration from legacy.
	multiStore, _, migrationErr := initializeBrokerCredentialStore()
	if migrationErr != nil {
		fmt.Printf("Warning: failed to migrate legacy credentials: %v\n", migrationErr)
	}

	// Determine which connection to deregister
	var creds *brokercredentials.BrokerCredentials
	var hubName string

	if brokerDeregisterName != "" {
		// Specific connection requested
		hubName = brokerDeregisterName
		loaded, err := multiStore.Load(hubName)
		if err != nil {
			return fmt.Errorf("hub connection '%s' not found, use 'scion runtime-broker hubs' to list connections", hubName)
		}
		creds = loaded
	} else {
		// No name specified - require exactly one connection
		all, err := multiStore.List()
		if err != nil {
			return fmt.Errorf("failed to list hub connections: %w", err)
		}
		switch len(all) {
		case 0:
			// Fall back to global settings
			globalDir, globalErr := config.GetGlobalDir()
			if globalErr == nil {
				globalSettings, err := config.LoadSettings(globalDir)
				if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
					creds = &brokercredentials.BrokerCredentials{
						BrokerID:    globalSettings.Hub.BrokerID,
						HubEndpoint: globalSettings.Hub.Endpoint,
					}
				}
			}
			if creds == nil {
				if brokerDeregisterPurgeLocal {
					return purgeLocalBrokerStateOnly(cmd, "")
				}
				return fmt.Errorf("no broker registration found, this host is not registered as a Runtime Broker with the Hub")
			}
		case 1:
			creds = &all[0]
			hubName = creds.Name
		default:
			fmt.Println("Multiple hub connections found. Specify which to deregister with --name:")
			for _, c := range all {
				fmt.Printf("  - %s (%s)\n", c.Name, c.HubEndpoint)
			}
			return fmt.Errorf("use --name to specify which hub connection to deregister")
		}
	}

	brokerID := creds.BrokerID
	if brokerID == "" {
		if brokerDeregisterPurgeLocal {
			return purgeLocalBrokerStateOnly(cmd, "")
		}
		return fmt.Errorf("no broker registration found, this host is not registered as a runtime broker with the hub")
	}

	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check local broker-server health (warning only)
	port := resolveBrokerPort(cmd)
	health, err := checkLocalBrokerServer(port)
	brokerRunning := err == nil
	if err != nil {
		fmt.Printf("Note: Broker server is not running (port %d)\n", port)
	} else {
		fmt.Printf("Broker server is running (status: %s)\n", health.Status)
	}

	// Fetch list of projects this broker provides for
	var projectNames []string
	projectsResp, err := client.RuntimeBrokers().ListProjects(ctx, brokerID)
	if err != nil {
		util.Debugf("Warning: failed to list broker projects: %v", err)
	} else if projectsResp != nil {
		for _, g := range projectsResp.Projects {
			projectNames = append(projectNames, g.ProjectName)
		}
	}

	// Show confirmation prompt with project list
	if !hubsync.ShowBrokerDeregistrationPrompt(brokerID, projectNames, autoConfirm) {
		return fmt.Errorf("deregistration cancelled")
	}

	// Delete the broker from Hub
	if err := client.RuntimeBrokers().Delete(ctx, brokerID); err != nil {
		return fmt.Errorf("deregistration failed: %w", err)
	}

	// Clear local credentials
	if hubName != "" {
		if err := multiStore.Delete(hubName); err != nil {
			fmt.Printf("Warning: failed to delete local credentials: %v\n", err)
		}
	}

	// Remove hub_connections entry from global settings, leaving every other
	// key in the file as it is.
	globalDir, globalErr := config.GetGlobalDir()
	if globalErr == nil && hubName != "" {
		if err := removeHubConnectionSetting(globalDir, hubName); err != nil {
			fmt.Printf("Warning: failed to remove hub connection from settings: %v\n", err)
		}
	}

	// Only clear global settings if no connections remain
	remaining, _ := multiStore.List()
	if globalErr == nil && len(remaining) == 0 {
		_ = config.UpdateSetting(globalDir, "hub.brokerToken", "", true)
		_ = config.UpdateSetting(globalDir, "hub.brokerId", "", true)
	}

	fmt.Println()
	fmt.Printf("Broker '%s' has been deregistered from the Hub.\n", brokerID)
	fmt.Println("Local broker credentials have been cleared.")
	if len(projectNames) > 0 {
		fmt.Printf("The broker has been removed from %d project(s).\n", len(projectNames))
	}

	// Local broker state: remove the credentials directory once empty, and
	// with --purge-local the broker's log, state and caches (ptone/scion#3538).
	if globalErr == nil {
		if daemonRunning, _, _ := daemon.Status(globalDir); daemonRunning {
			brokerRunning = true
		}
		return cleanupAfterDeregister(os.Stdout, multiStore.Dir(), len(remaining), brokerRunning,
			brokerDeregisterPurgeLocal, brokerLocalStatePaths(globalDir, brokerScionHome(globalDir), brokerID))
	}
	return nil
}

// brokerScionHome is the directory the runtime broker keeps its state and
// template cache under by default: ~/.scion (see pkg/runtimebroker), falling
// back to globalDir when the home directory is unknown.
func brokerScionHome(globalDir string) string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".scion")
	}
	return globalDir
}

// purgeLocalBrokerStateOnly runs 'deregister --purge-local' on a host with
// no registration left (for example after an earlier deregister): it removes
// broker-local state only. brokerID names the state directory to remove, if
// known; otherwise the one recorded in global settings is used.
func purgeLocalBrokerStateOnly(cmd *cobra.Command, brokerID string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}
	_, healthErr := checkLocalBrokerServer(resolveBrokerPort(cmd))
	running := healthErr == nil
	if daemonRunning, _, _ := daemon.Status(globalDir); daemonRunning {
		running = true
	}
	if brokerID == "" {
		brokerID = getLocalBrokerID()
	}
	credsDir := ""
	if ms, _, msErr := initializeBrokerCredentialStore(); msErr == nil {
		credsDir = ms.Dir()
	}
	fmt.Println("No broker registration found; removing local broker state only.")
	return cleanupAfterDeregister(os.Stdout, credsDir, 0, running, true,
		brokerLocalStatePaths(globalDir, brokerScionHome(globalDir), brokerID))
}

// isServerDaemonManagingBroker checks if the combined server daemon is running
// and the broker health endpoint is responding, indicating the broker is managed
// as part of the combined server process.
func isServerDaemonManagingBroker(globalDir string) (running bool, pid int) {
	serverRunning, serverPID, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !serverRunning {
		return false, 0
	}
	// Confirm broker is actually responding on its health endpoint
	_, err := checkLocalBrokerServer(DefaultBrokerPort)
	if err != nil {
		return false, 0
	}
	return true, serverPID
}

// buildBrokerForegroundArgs constructs the `server start` args used when
// `runtime-broker start --foreground` runs the server command directly,
// in-process, via serverStartCmd.RunE (no re-exec, no daemon).
//
// --foreground MUST be included here: runServerStartOrDaemon (serverStartCmd's
// RunE) decides whether to daemonize based on the --foreground flag being set
// on serverStartCmd itself, not on the fact that the caller is already inside
// runBrokerStart's foreground branch. Dropping it caused serverStartCmd.RunE
// to spawn a background daemon child instead of running inline: fatal under a
// systemd Type=simple unit, whose ExecStart is expected to stay in the
// foreground, since the parent then exits 0 immediately and systemd's cgroup
// cleanup reaps the now-orphaned daemon child.
func buildBrokerForegroundArgs(port int, autoProvide, debug bool) []string {
	// Use --hosted to avoid workstation defaults (we only want the broker).
	args := []string{"--foreground", "--hosted", "--enable-runtime-broker"}
	if port != DefaultBrokerPort {
		args = append(args, fmt.Sprintf("--runtime-broker-port=%d", port))
	}
	if autoProvide {
		args = append(args, "--auto-provide")
	}
	if debug {
		args = append(args, "--debug")
	}
	return args
}

// buildBrokerDaemonArgs constructs the `server start --foreground` argv used
// to (re-)launch the broker as a background daemon: the re-exec'd child itself
// runs with --foreground, and daemon.Start is what backgrounds that child.
// Shared by runBrokerStart's daemon path and runBrokerRestart. Built on top of
// buildBrokerForegroundArgs (with the "server", "start" command name prefixed)
// so the flag list can't drift between the two paths the way --foreground once
// did.
func buildBrokerDaemonArgs(port int, autoProvide, debug bool) []string {
	return append([]string{"server", "start"}, buildBrokerForegroundArgs(port, autoProvide, debug)...)
}

func runBrokerStart(cmd *cobra.Command, args []string) error {
	// Get global directory for daemon files
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Check if the broker is managed by the combined server
	if managed, pid := isServerDaemonManagingBroker(globalDir); managed {
		return fmt.Errorf("the runtime broker is managed by the combined server process (PID: %d), use 'scion server start/stop/restart' to manage the server, or 'scion runtime-broker status' to check broker status", pid)
	}

	// Foreground mode - just run the server command directly
	// The port the server will listen on: --port, else the settings port
	// (server.broker.port), else DefaultBrokerPort.
	port := resolveBrokerStartPort(cmd)

	if brokerStartForeground {
		serverArgs := pinBrokerPort(buildBrokerForegroundArgs(port, brokerStartAutoProvide, brokerStartDebug), port)
		// Saved so the other subcommands find the port while this broker
		// runs, and removed when it exits (also on SIGTERM, e.g. systemctl
		// stop). Skipped when another broker owns the record: a broker
		// daemon, or a broker already answering on this port (this run
		// will fail to bind and must not remove that broker's record).
		if recordArgs, ok := claimForegroundBrokerRecord(globalDir, port); ok {
			saveBrokerArgs(globalDir, recordArgs)
			stopSignals := removeBrokerRecordOnSIGTERM(globalDir, recordArgs)
			defer func() {
				stopSignals()
				removeBrokerArgsIfOwned(globalDir, recordArgs)
			}()
		}

		fmt.Printf("Starting broker in foreground on port %d...\n", port)
		fmt.Println("Press Ctrl+C to stop.")
		fmt.Println()

		return runForegroundBrokerServer(serverArgs)
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
	daemonArgs := pinBrokerPort(buildBrokerDaemonArgs(port, brokerStartAutoProvide, brokerStartDebug), port)

	// Start daemon
	fmt.Printf("Starting broker as daemon on port %d...\n", port)
	// Nothing is saved until the daemon is up, so the failure paths leave
	// any existing record (for example a live foreground broker's) alone.
	if err := startBrokerDaemon(executable, daemonArgs, globalDir); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Verify it started
	time.Sleep(brokerStartVerifyDelay)
	running, pid, _ = daemon.Status(globalDir)
	if !running {
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPath(globalDir))
	}
	// Saved so the other subcommands find the port and restart keeps
	// --port, --auto-provide and --debug.
	saveBrokerArgs(globalDir, daemonArgs)

	fmt.Printf("Broker started (PID: %d)\n", pid)
	fmt.Printf("Log file: %s\n", daemon.GetLogPath(globalDir))
	fmt.Printf("PID file: %s\n", daemon.GetPIDPath(globalDir))
	fmt.Println()
	fmt.Println("Use 'scion runtime-broker stop' to stop the daemon.")
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

	// Check if the broker is managed by the combined server
	if managed, pid := isServerDaemonManagingBroker(globalDir); managed {
		return fmt.Errorf("the runtime broker is managed by the combined server process (PID: %d), use 'scion server stop' to stop the server, or 'scion runtime-broker status' to check broker status", pid)
	}

	// Check if daemon is running
	running, pid, _ := daemon.Status(globalDir)
	if !running {
		// Check if server is running on the port (might be foreground)
		health, err := checkLocalBrokerServer(resolveBrokerPort(cmd))
		if err == nil {
			return fmt.Errorf("broker server is running (status: %s) but not as a daemon, if running in foreground use Ctrl+C to stop it", health.Status)
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
		return fmt.Errorf("daemon may still be running, check with 'scion runtime-broker status'")
	}

	removeBrokerArgs(globalDir)
	fmt.Println("Broker daemon stopped.")
	return nil
}

func runBrokerRestart(cmd *cobra.Command, args []string) error {
	// Get global directory for daemon files
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Check if the broker is managed by the combined server
	if managed, pid := isServerDaemonManagingBroker(globalDir); managed {
		return fmt.Errorf("the runtime broker is managed by the combined server process (PID: %d), use 'scion server restart' to restart the server or 'scion runtime-broker status' to check broker status", pid)
	}

	// Check if daemon is running
	running, pid, _ := daemon.Status(globalDir)
	if !running {
		// Check if server is running on the port (might be foreground)
		health, err := checkLocalBrokerServer(resolveBrokerPort(cmd))
		if err == nil {
			return fmt.Errorf("broker server is running (status: %s) but not as a daemon, if running in foreground use Ctrl+C to stop it and then 'scion runtime-broker start' to restart", health.Status)
		}
		return fmt.Errorf("broker daemon is not running, use 'scion runtime-broker start' to start it")
	}

	// Flags given to restart win; anything else keeps the value the running
	// daemon was started with.
	saved := loadBrokerArgs(globalDir)
	port, autoProvide, debug := resolveBrokerRestartOptions(cmd, saved)
	daemonArgs := pinBrokerPort(buildBrokerDaemonArgs(port, autoProvide, debug), port)

	// Stop the daemon
	fmt.Printf("Stopping broker daemon (PID: %d)...\n", pid)
	if err := daemon.Stop(globalDir); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	// Wait for the process to exit
	if err := daemon.WaitForExit(globalDir, 10*time.Second); err != nil {
		return fmt.Errorf("failed to stop broker: %w", err)
	}
	fmt.Println("Broker daemon stopped.")

	// Find the current scion executable
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Start new daemon
	fmt.Printf("Starting broker with new binary on port %d...\n", port)
	if err := daemon.Start(executable, daemonArgs, globalDir); err != nil {
		removeBrokerArgs(globalDir)
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Verify it started
	time.Sleep(500 * time.Millisecond)
	running, pid, _ = daemon.Status(globalDir)
	if !running {
		removeBrokerArgs(globalDir)
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPath(globalDir))
	}
	saveBrokerArgs(globalDir, daemonArgs)

	fmt.Printf("Broker restarted (PID: %d)\n", pid)
	fmt.Printf("Log file: %s\n", daemon.GetLogPath(globalDir))
	fmt.Println()

	// Show broker status
	return runBrokerStatus(cmd, args)
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
		// Get broker ID from local credentials (try MultiStore first)
		brokerID = getLocalBrokerID()

		if brokerID == "" {
			return fmt.Errorf("no broker registration found.\n\nRegister with: scion runtime-broker register\nOr specify a broker with --broker <name-or-id>")
		}

		// Get broker name for display
		brokerName, _ = os.Hostname()
		if brokerName == "" {
			brokerName = brokerID[:8]
		}
	}

	// Resolve project ID
	var projectID string
	var projectName string
	var localProjectPath string // Local path to the project's .scion directory on this broker
	var projectSlug string      // Hub slug of the target project when known ("global" for the global project)

	if brokerProjectID != "" {
		projectID = brokerProjectID
		projectName = projectID // Will be updated after fetching
	} else {
		// Use current project
		resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path: %w\n\nSpecify a project with --project <name-or-id>", err)
		}
		localProjectPath = resolvedPath

		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		projectID = settings.GetHubProjectID()
		if projectID == "" {
			projectID = settings.ProjectID
		}
		if projectID == "" {
			return fmt.Errorf("current project is not linked to the Hub.\n\nLink it first with: scion hub link\nOr specify a project with --project <name-or-id>")
		}

		// Get project name for display
		if isGlobal {
			projectName = "global"
			projectSlug = "global"
		} else {
			gitRemote := util.GetGitRemote()
			if gitRemote != "" {
				projectName = util.ExtractRepoName(gitRemote)
			} else {
				projectName = config.GetProjectName(resolvedPath)
			}
		}
	}

	// Load Hub client: use --hub flag if specified, otherwise fall back to project settings
	var client hubclient.Client
	if brokerHubFlag != "" {
		var err error
		client, err = getHubClientForConnection(brokerHubFlag)
		if err != nil {
			return err
		}
	} else {
		resolvedPath, _, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path: %w", err)
		}
		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		client, err = getHubClient(settings)
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// If we used --project flag, resolve project by name or ID
	if brokerProjectID != "" {
		project, err := resolveProjectByNameOrID(ctx, client, brokerProjectID)
		if err != nil {
			return fmt.Errorf("failed to find project '%s': %w", brokerProjectID, err)
		}
		projectID = project.ID
		projectName = project.Name
		projectSlug = project.Slug
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
		// The current project's path names a directory on this host only,
		// so it is never registered for another host's broker
		// (ptone/scion#2839); the broker then uses its hub-managed project
		// directory. An explicit --path below is still sent. A --broker
		// naming this host's own broker keeps the path.
		if brokerID != getLocalBrokerID() {
			localProjectPath = ""
		}
	}

	// An explicit --path replaces the current-project path. With --project
	// and no --path, no path is sent: the current directory need not belong
	// to the named project.
	if brokerProvidePath != "" {
		remote := isRemoteBroker && brokerID != getLocalBrokerID()
		explicitPath, err := resolveProvidePath(brokerProvidePath, projectName, projectSlug, remote)
		if err != nil {
			return err
		}
		localProjectPath = explicitPath
	}

	// Show confirmation prompt
	if !hubsync.ShowProvidePrompt(projectName, brokerName, autoConfirm) {
		return fmt.Errorf("operation cancelled")
	}

	// Add broker as provider
	req := &hubclient.RegisterProjectRequest{
		ID:       projectID,
		Name:     projectName,
		BrokerID: brokerID,
		Path:     localProjectPath,
	}

	resp, err := client.Projects().Register(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to add broker as provider: %w", err)
	}

	fmt.Println()
	fmt.Printf("Broker '%s' added as provider for project '%s'\n", brokerName, resp.Project.Name)
	if localProjectPath != "" {
		fmt.Println(providePathSummary(localProjectPath, brokerName, isRemoteBroker && brokerID != getLocalBrokerID()))
	} else {
		fmt.Println("No local path sent; an existing provider path for this broker is kept (a stored global-directory path is cleared), otherwise the broker uses its hub-managed project directory.")
	}

	// Handle --make-default flag
	if brokerMakeDefault {
		currentDefault := resp.Project.DefaultRuntimeBrokerID

		switch currentDefault {
		case brokerID:
			// Already the default, nothing to do
			fmt.Printf("Broker '%s' is already the default for project '%s'\n", brokerName, resp.Project.Name)
		case "":
			// No default set - the server should have auto-set it during provide,
			// but set it explicitly to be sure
			_, err := client.Projects().Update(ctx, resp.Project.ID, &hubclient.UpdateProjectRequest{
				DefaultRuntimeBrokerID: brokerID,
			})
			if err != nil {
				return fmt.Errorf("failed to set default broker: %w", err)
			}
			fmt.Printf("Broker '%s' set as default for project '%s'\n", brokerName, resp.Project.Name)
		default:
			// Different default already set - resolve its name and confirm
			currentDefaultName := currentDefault[:8] // fallback to truncated ID
			currentBroker, err := client.RuntimeBrokers().Get(ctx, currentDefault)
			if err == nil && currentBroker.Name != "" {
				currentDefaultName = currentBroker.Name
			}

			if !hubsync.ShowChangeDefaultBrokerPrompt(resp.Project.Name, currentDefaultName, brokerName, autoConfirm) {
				fmt.Println("Default broker not changed.")
			} else {
				_, err := client.Projects().Update(ctx, resp.Project.ID, &hubclient.UpdateProjectRequest{
					DefaultRuntimeBrokerID: brokerID,
				})
				if err != nil {
					return fmt.Errorf("failed to update default broker: %w", err)
				}
				fmt.Printf("Default broker for project '%s' changed from '%s' to '%s'\n", resp.Project.Name, currentDefaultName, brokerName)
			}
		}
	}

	return nil
}

// resolveProvidePath resolves an explicit provide --path to the project
// path registered with the hub. It refuses the global scion directory unless
// the target project is the global project (hub slug "global"): registering
// that directory for any other project makes the broker treat its global
// directory as that project.
//
// For a remote broker (one that is not this host's broker) the path is the
// absolute project root (the directory containing .scion) on the broker's
// host, so it is not resolved or checked against this host's filesystem: it
// is sent as given, cleaned with slash-only path rules (separators are not
// translated). A path naming the .scion directory itself is
// refused with a hint to pass its parent (ptone/scion#3157).
func resolveProvidePath(path, projectName, projectSlug string, remote bool) (string, error) {
	if remote {
		// The path is in the remote broker's form, so it is checked and
		// cleaned with the slash-only path package, never this host's
		// filepath rules, and its separators are not translated.
		if !slashpath.IsAbs(path) {
			return "", fmt.Errorf("--path %q must be an absolute path on the broker's host when --broker names a remote broker", path)
		}
		cleaned := slashpath.Clean(path)
		if slashpath.Base(cleaned) == config.DotScion {
			return "", fmt.Errorf("--path %q names a .scion directory; for a remote broker pass the project root that contains it: %s", path, slashpath.Dir(cleaned))
		}
		return cleaned, nil
	}
	resolved, isGlobal, err := config.ResolveProjectPath(path)
	if err != nil {
		return "", fmt.Errorf("failed to resolve --path %q: %w", path, err)
	}
	if (isGlobal || config.IsGlobalProjectDir(resolved)) && projectSlug != "global" {
		return "", fmt.Errorf("--path %q is the global scion directory and cannot be registered for project %q", path, projectName)
	}
	return resolved, nil
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
		// Get broker ID from local credentials (try MultiStore first)
		brokerID = getLocalBrokerID()

		if brokerID == "" {
			return fmt.Errorf("no broker registration found.\n\nRegister with: scion runtime-broker register\nOr specify a broker with --broker <name-or-id>")
		}

		// Get broker name for display
		brokerName, _ = os.Hostname()
		if brokerName == "" {
			brokerName = brokerID[:8]
		}
	}

	// Resolve project ID
	var projectID string
	var projectName string

	if brokerProjectID != "" {
		projectID = brokerProjectID
		projectName = projectID // Will be updated after fetching
	} else {
		// Use current project
		resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path: %w\n\nSpecify a project with --project <name-or-id>", err)
		}

		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		projectID = settings.GetHubProjectID()
		if projectID == "" {
			projectID = settings.ProjectID
		}
		if projectID == "" {
			return fmt.Errorf("current project is not linked to the Hub.\n\nSpecify a project with --project <name-or-id>")
		}

		// Get project name for display
		if isGlobal {
			projectName = "global"
		} else {
			gitRemote := util.GetGitRemote()
			if gitRemote != "" {
				projectName = util.ExtractRepoName(gitRemote)
			} else {
				projectName = config.GetProjectName(resolvedPath)
			}
		}
	}

	// Load Hub client: use --hub flag if specified, otherwise fall back to project settings
	var client hubclient.Client
	if brokerHubFlag != "" {
		var err error
		client, err = getHubClientForConnection(brokerHubFlag)
		if err != nil {
			return err
		}
	} else {
		resolvedPath, _, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path: %w", err)
		}

		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		client, err = getHubClient(settings)
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// If we used --project flag, resolve project by name or ID
	if brokerProjectID != "" {
		project, err := resolveProjectByNameOrID(ctx, client, brokerProjectID)
		if err != nil {
			return fmt.Errorf("failed to find project '%s': %w", brokerProjectID, err)
		}
		projectID = project.ID
		projectName = project.Name
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
	if !hubsync.ShowWithdrawPrompt(projectName, brokerName, autoConfirm) {
		return fmt.Errorf("operation cancelled")
	}

	// Remove broker as provider
	if err := client.Projects().RemoveProvider(ctx, projectID, brokerID); err != nil {
		return fmt.Errorf("failed to remove broker as provider: %w", err)
	}

	fmt.Println()
	fmt.Printf("Broker '%s' removed as provider from project '%s'\n", brokerName, projectName)

	return nil
}

func runBrokerStatus(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if brokerStatusJSON {
		outputFormat = "json"
	}

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

	// Check daemon status - first check for standalone broker daemon
	running, pid, _ := daemon.Status(globalDir)
	status.DaemonRunning = running
	status.DaemonPID = pid
	if running {
		status.LogFile = daemon.GetLogPath(globalDir)
		status.PIDFile = daemon.GetPIDPath(globalDir)
	} else {
		// Check if the combined server daemon is managing the broker
		serverRunning, serverPID, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
		if serverRunning {
			status.DaemonRunning = true
			status.DaemonPID = serverPID
			status.ManagedByServer = true
			status.LogFile = daemon.GetLogPathComponent(serverDaemonComponent, globalDir)
			status.PIDFile = daemon.GetPIDPathComponent(serverDaemonComponent, globalDir)
		}
	}

	// Check if broker server is responding (could be foreground or daemon)
	port := resolveBrokerPort(cmd)
	health, err := checkLocalBrokerServer(port)
	if err != nil && status.DaemonRunning {
		// A daemon that is up but not serving yet is still starting (status
		// runs right after start and restart): wait briefly for it.
		pollUntil(func() bool {
			health, err = checkLocalBrokerServer(port)
			return err == nil
		}, brokerStatusPollTimeout, brokerStatusPollInterval)
	}
	brokerUptime := ""
	if err == nil {
		status.ServerRunning = true
		status.ServerPort = port
		status.ServerStatus = health.Status
		status.ServerVersion = health.Version
		brokerUptime = health.Uptime
	}

	// Load hub connections from MultiStore, migrating legacy credentials first.
	multiStore, _, migrationErr := initializeBrokerCredentialStore()
	if migrationErr != nil {
		util.Debugf("Warning: failed to migrate legacy credentials: %v", migrationErr)
	}

	allCreds, _ := multiStore.List()

	// Get broker ID from first connection or global settings
	if len(allCreds) > 0 {
		status.BrokerID = allCreds[0].BrokerID
		status.HubEndpoint = allCreds[0].HubEndpoint
		status.Registered = true
		status.CredentialsPath = multiStore.Dir()
	} else {
		// Check global settings for broker ID (may be pre-populated from init or from registration)
		globalSettings, err := config.LoadSettings(globalDir)
		if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
			status.BrokerID = globalSettings.Hub.BrokerID
			status.HubEndpoint = globalSettings.Hub.Endpoint
			status.Registered = true
		}
	}

	// Build hub connection statuses
	for _, c := range allCreds {
		status.HubConnections = append(status.HubConnections, brokerHubConnectionStatus{
			Name:         c.Name,
			HubEndpoint:  c.HubEndpoint,
			AuthMode:     string(c.AuthMode),
			RegisteredAt: c.RegisteredAt,
			BrokerID:     c.BrokerID,
		})
	}

	// Get broker name
	status.Hostname, _ = os.Hostname()

	// If registered, try to get Hub status and project list
	if status.Registered && status.HubEndpoint != "" {
		resolvedPath, _, _ := config.ResolveProjectPath(projectPath)
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

					// Get projects this broker provides for (only if still registered)
					if status.Registered {
						projectsResp, err := client.RuntimeBrokers().ListProjects(ctx, status.BrokerID)
						if err != nil {
							// Record the failure instead of silently treating
							// it the same as a confirmed-empty provider list.
							status.ProjectsError = err.Error()
						} else if projectsResp != nil {
							for _, g := range projectsResp.Projects {
								status.Projects = append(status.Projects, brokerProjectStatus{
									ID:   g.ProjectID,
									Name: g.ProjectName,
								})
							}
						}
					}
				}
			}
		}
	}

	// Output
	if isJSONOutput() {
		return outputJSON(status)
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
		if status.ManagedByServer {
			fmt.Printf("  Mode:        combined server (use 'scion server' to manage)\n")
		}
		fmt.Printf("  Log file:    %s\n", status.LogFile)
	} else if status.ServerRunning {
		fmt.Printf("  Mode:        foreground (or external)\n")
	}
	fmt.Println()

	// Hub Connections
	if len(status.HubConnections) > 0 {
		// Try to get live status from running broker server. Right after a
		// (re)start the broker has not connected yet, so wait briefly
		// (bounded) for its first heartbeat instead of printing "unknown".
		liveStatus := queryBrokerHubConnections(port)
		if status.ServerRunning && brokerRecentlyStarted(brokerUptime) {
			names := make([]string, 0, len(status.HubConnections))
			for _, conn := range status.HubConnections {
				names = append(names, conn.Name)
			}
			liveStatus = pollBrokerHubConnections(func() *BrokerHubConnectionsResponse {
				return queryBrokerHubConnections(port)
			}, names, brokerStatusPollTimeout, brokerStatusPollInterval)
		}
		liveStatusMap := make(map[string]string)
		if liveStatus != nil {
			for _, conn := range liveStatus.Connections {
				liveStatusMap[conn.Name] = conn.Status
			}
		}

		fmt.Println("Hub Connections")
		fmt.Println("---------------")
		if status.BrokerID != "" {
			fmt.Printf("  Broker ID:   %s\n", status.BrokerID)
		}
		if liveStatus != nil {
			fmt.Printf("  Mode:        %s\n", liveStatus.Mode)
		}
		fmt.Println()
		for _, conn := range status.HubConnections {
			connStatus := brokerHubConnectionDisplayStatus(liveStatusMap, conn.Name, status.ServerRunning)
			fmt.Printf("  %s\n", conn.Name)
			fmt.Printf("    Hub:         %s\n", conn.HubEndpoint)
			fmt.Printf("    Auth:        %s\n", conn.AuthMode)
			fmt.Printf("    Status:      %s\n", connStatus)
			if !conn.RegisteredAt.IsZero() {
				fmt.Printf("    Registered:  %s\n", clitime.Format(conn.RegisteredAt, clitime.Date))
			}
		}
		if status.HubConnected {
			fmt.Printf("\n  Connected:   yes (%s)\n", status.HubStatus)
			if status.BrokerStatus != "" {
				fmt.Printf("  Status:      %s\n", status.BrokerStatus)
			}
			if !status.LastHeartbeat.IsZero() {
				fmt.Printf("  Last seen:   %s\n", clitime.Ago(status.LastHeartbeat))
			}
		} else if status.Registered {
			fmt.Printf("\n  Connected:   no (Hub unreachable)\n")
		}
	} else if status.Registered {
		// Legacy single-connection display
		fmt.Println("Hub Registration")
		fmt.Println("----------------")
		if status.BrokerID != "" {
			fmt.Printf("  Broker ID:   %s\n", status.BrokerID)
		}
		fmt.Printf("  Registered:  yes\n")
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
				fmt.Printf("  Last seen:   %s\n", clitime.Ago(status.LastHeartbeat))
			}
		} else {
			fmt.Printf("  Connected:   no (Hub unreachable)\n")
		}
	} else {
		fmt.Println("Hub Registration")
		fmt.Println("----------------")
		fmt.Printf("  Registered:  no\n")
		fmt.Printf("\n  Run 'scion runtime-broker register' to register with the Hub.\n")
	}
	fmt.Println()

	// Projects
	if len(status.Projects) > 0 {
		fmt.Println("Projects (Provider)")
		fmt.Println("-----------------")
		for _, g := range status.Projects {
			fmt.Printf("  - %s (ID: %s)\n", g.Name, g.ID)
		}
	} else if status.ProjectsError != "" {
		// Distinguish a failed lookup from a confirmed-empty list: printing
		// "(none)" here would tell the operator to re-provide a project that
		// may already be provisioned correctly.
		fmt.Println("Projects (Provider)")
		fmt.Println("-----------------")
		fmt.Printf("  (unknown - failed to fetch provider list: %s)\n", status.ProjectsError)
	} else if status.Registered {
		fmt.Println("Projects (Provider)")
		fmt.Println("-----------------")
		fmt.Printf("  (none)\n")
		fmt.Printf("\n  Run 'scion runtime-broker provide' to add this broker as a provider for a project.\n")
	}

	return nil
}

func runBrokerHubs(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if brokerHubsJSON {
		outputFormat = "json"
	}

	// Initialize MultiStore with auto-migration from legacy.
	multiStore, _, migrationErr := initializeBrokerCredentialStore()
	if migrationErr != nil {
		fmt.Printf("Warning: failed to migrate legacy credentials: %v\n", migrationErr)
	}

	allCreds, err := multiStore.List()
	if err != nil {
		return fmt.Errorf("failed to list hub connections: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(allCreds)
	}

	if len(allCreds) == 0 {
		fmt.Println("No hub connections found.")
		fmt.Println("\nRun 'scion runtime-broker register' to register with a Hub.")
		return nil
	}

	// Try to get live status from running broker server
	liveStatus := queryBrokerHubConnections(resolveBrokerPort(cmd))

	// Build a lookup map for live status by connection name
	liveStatusMap := make(map[string]string)
	var mode string
	if liveStatus != nil {
		mode = liveStatus.Mode
		for _, conn := range liveStatus.Connections {
			liveStatusMap[conn.Name] = conn.Status
		}
	}

	fmt.Println("Hub Connections")
	fmt.Println("===============")
	if mode != "" {
		fmt.Printf("Mode: %s\n", mode)
	}
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  NAME\tHUB ENDPOINT\tAUTH\tSTATUS\tREGISTERED\n")
	for _, c := range allCreds {
		regDate := ""
		if !c.RegisteredAt.IsZero() {
			regDate = clitime.Format(c.RegisteredAt, clitime.Date)
		}
		authMode := string(c.AuthMode)
		if authMode == "" {
			authMode = "hmac"
		}
		status := "unknown"
		if s, ok := liveStatusMap[c.Name]; ok {
			status = s
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", c.Name, c.HubEndpoint, authMode, status, regDate)
	}
	_ = w.Flush()

	fmt.Printf("\nCredentials directory: %s\n", multiStore.Dir())

	return nil
}

// runRemoteBrokerStatus fetches and displays status for a remote broker from the Hub
func runRemoteBrokerStatus(brokerID string) error {
	settings, client, err := loadHubClient()
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

	// Get projects this broker provides for
	projectsResp, err := client.RuntimeBrokers().ListProjects(ctx, brokerID)
	if err != nil {
		status.ProjectsError = err.Error()
	} else if projectsResp != nil {
		for _, g := range projectsResp.Projects {
			status.Projects = append(status.Projects, brokerProjectStatus{
				ID:   g.ProjectID,
				Name: g.ProjectName,
			})
		}
	}

	// Output
	if isJSONOutput() {
		return outputJSON(status)
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
		fmt.Printf("  Last seen:   %s\n", clitime.Ago(status.LastHeartbeat))
	}
	fmt.Printf("  Hub:         %s\n", status.HubEndpoint)
	fmt.Println()

	// Projects
	if len(status.Projects) > 0 {
		fmt.Println("Projects (Provider)")
		fmt.Println("-----------------")
		for _, g := range status.Projects {
			fmt.Printf("  - %s (ID: %s)\n", g.Name, g.ID)
		}
	} else if status.ProjectsError != "" {
		fmt.Println("Projects (Provider)")
		fmt.Println("-----------------")
		fmt.Printf("  (unknown - failed to fetch provider list: %s)\n", status.ProjectsError)
	} else {
		fmt.Println("Projects (Provider)")
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
	DaemonRunning   bool   `json:"daemonRunning"`
	DaemonPID       int    `json:"daemonPid,omitempty"`
	ManagedByServer bool   `json:"managedByServer,omitempty"`
	LogFile         string `json:"logFile,omitempty"`
	PIDFile         string `json:"pidFile,omitempty"`

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

	// Hub connections
	HubConnections []brokerHubConnectionStatus `json:"hubConnections,omitempty"`

	// Projects
	Projects []brokerProjectStatus `json:"projects,omitempty"`
	// ProjectsError records why the provider list could not be fetched, so a
	// failed lookup (e.g. a transient Hub error) is never displayed the same
	// way as a confirmed-empty list.
	ProjectsError string `json:"projectsError,omitempty"`
}

// brokerHubConnectionStatus holds status for a single hub connection.
type brokerHubConnectionStatus struct {
	Name         string    `json:"name"`
	HubEndpoint  string    `json:"hubEndpoint"`
	AuthMode     string    `json:"authMode"`
	RegisteredAt time.Time `json:"registeredAt,omitempty"`
	BrokerID     string    `json:"brokerId"`
}

// brokerProjectStatus holds project info for status output
type brokerProjectStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// resolveProjectByNameOrID resolves a project identifier (name, slug, or ID) to a project.
// It tries multiple strategies in order: direct ID lookup, slug, then name.
// Returns the project if found, or an error if not found or multiple matches.
func resolveProjectByNameOrID(ctx context.Context, client hubclient.Client, nameOrID string) (*hubclient.Project, error) {
	// First try to fetch by ID directly
	project, err := client.Projects().Get(ctx, nameOrID)
	if err == nil {
		return project, nil
	}

	// If not a 404, return the error
	if !apiclient.IsNotFoundError(err) {
		return nil, err
	}

	// Try by slug
	resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
		Slug: nameOrID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for project: %w", err)
	}
	if len(resp.Projects) == 1 {
		return &resp.Projects[0], nil
	}

	// Try by name
	resp, err = client.Projects().List(ctx, &hubclient.ListProjectsOptions{
		Name: nameOrID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for project by name: %w", err)
	}

	switch len(resp.Projects) {
	case 0:
		return nil, fmt.Errorf("project '%s' not found", nameOrID)
	case 1:
		return &resp.Projects[0], nil
	default:
		return nil, fmt.Errorf("multiple projects found with name '%s' - please use the project ID instead", nameOrID)
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

// getLocalBrokerID resolves the local broker ID from MultiStore or global settings.
// It tries MultiStore first, then falls back to the global settings file.
func getLocalBrokerID() string {
	// Try MultiStore first
	multiStore := brokercredentials.NewMultiStore("")
	allCreds, err := multiStore.List()
	if err == nil && len(allCreds) == 1 {
		return allCreds[0].BrokerID
	}
	if err == nil && len(allCreds) > 1 {
		// Multiple connections - use the first one's broker ID
		// (they should all share the same stable ID)
		return allCreds[0].BrokerID
	}

	// Fall back to legacy single-file store
	legacyStore := brokercredentials.NewStore("")
	creds, credErr := legacyStore.Load()
	if credErr == nil && creds != nil && creds.BrokerID != "" {
		return creds.BrokerID
	}

	// Fall back to global settings
	globalDir, globalErr := config.GetGlobalDir()
	if globalErr == nil {
		globalSettings, err := config.LoadSettings(globalDir)
		if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
			return globalSettings.Hub.BrokerID
		}
	}

	return ""
}

// brokerRegistrationCapabilities builds the capability-name list this
// command reports at registration/join time. There is no live runtime
// instance available here to ask (this command only sends metadata to the
// Hub; it does not start the broker daemon or construct a runtime), so
// "attach" is always included — the same missing-capability-implies-
// supported default used everywhere else this feature answers the
// question, applied because there is nothing here to say otherwise. The
// same holds for "emptyPerAgentWorkspace" (false only for Cloud Run). A
// runtime that actually opts out reports it once the broker itself runs
// and registers/heartbeats with a live instance in hand (see
// buildStoreBrokerProfiles and HeartbeatService.buildHeartbeat).
// "agentMove" is a property of this broker binary (localOnly delete and the
// moved-workspace check), like "reprovision". So is
// "reprovisionEmptyPerAgent" (in-place empty-per-agent reprovision,
// miller79/scion#167); the hub checks runtime suitability separately.
func brokerRegistrationCapabilities() []string {
	return []string{"sync", "attach", "reprovision", "emptyPerAgentWorkspace", "agentMove", "reprovisionEmptyPerAgent"}
}

// buildBrokerProfiles builds BrokerProfile objects from settings.Profiles.
// It converts the user-defined profiles in settings.yaml to the format expected by the Hub.
func buildBrokerProfiles(settings *config.Settings) []hubclient.BrokerProfile {
	if settings == nil || len(settings.Profiles) == 0 {
		return nil
	}

	// GSA-to-KSA mappings are read from the broker's global settings only,
	// the same source the broker resolves them from at dispatch
	// (resolveKubernetesAssignIdentity). If they cannot be read, profiles
	// are reported without them (MappingsReported=false: unknown).
	mappingVS, mappingErr := loadBrokerMappingSettings()
	if mappingErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read global settings for kubernetes_service_account_mappings; the Hub will treat this broker's mappings as unknown: %v\n", mappingErr)
	}

	var profiles []hubclient.BrokerProfile
	for name, profileCfg := range settings.Profiles {
		// Determine runtime type from the profile's runtime reference
		runtimeType := profileCfg.Runtime
		if runtimeType == "" {
			runtimeType = "docker" // default
		}
		mappings, reported := brokerProfileSAMappings(mappingVS, mappingErr, name)

		// Look up runtime config to get additional info (context, namespace for K8s)
		var context, namespace string
		if settings.Runtimes != nil {
			if rtCfg, ok := settings.Runtimes[profileCfg.Runtime]; ok {
				context = rtCfg.Context
				namespace = rtCfg.Namespace
			}
		}

		profiles = append(profiles, hubclient.BrokerProfile{
			Name:                   name,
			Type:                   runtimeType,
			Available:              true,
			Context:                context,
			Namespace:              namespace,
			ServiceAccountMappings: mappings,
			MappingsReported:       reported,
		})
	}

	return profiles
}

// loadBrokerMappingSettings loads the broker's global settings (plus the
// DB-backed overlay, when one is installed) for reporting
// kubernetes_service_account_mappings at join. A variable so tests can
// substitute settings.
var loadBrokerMappingSettings = func() (*config.VersionedSettings, error) {
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	return vs, err
}

// brokerProfileSAMappings returns the GSAs profileName maps to a KSA (its
// own mapping plus its runtime entry's) and whether they are reported. Only
// Kubernetes profiles report (by resolved runtime type, so a custom entry
// key with type kubernetes counts). Not reported when the settings could not
// be loaded or do not have the profile: the Hub then treats the profile's
// mappings as unknown instead of empty.
func brokerProfileSAMappings(vs *config.VersionedSettings, loadErr error, profileName string) ([]hubclient.BrokerProfileSAMapping, bool) {
	if loadErr != nil {
		return nil, false
	}
	gsas, isKubernetes, known := vs.ProfileKubernetesSAMappings(profileName)
	if !known || !isKubernetes {
		return nil, false
	}
	var out []hubclient.BrokerProfileSAMapping
	for _, gsa := range gsas {
		out = append(out, hubclient.BrokerProfileSAMapping{GSA: gsa})
	}
	return out, true
}

// brokerRegistrationDefaultProfile returns the broker's default (active)
// profile name to report at join, or nil when settings could not be loaded
// (the hub then keeps what it has).
func brokerRegistrationDefaultProfile(settings *config.Settings) *string {
	if settings == nil {
		return nil
	}
	name := settings.ActiveProfile
	return &name
}

// brokerHeartbeatDefaultProfile returns the default profile the running
// broker reports on every heartbeat: settings' active profile, or nil when
// settings failed to load, so the heartbeat omits it and the hub keeps what
// it has (the same rule as join).
func brokerHeartbeatDefaultProfile(settings *config.Settings, loaded bool) *string {
	if !loaded {
		return nil
	}
	return brokerRegistrationDefaultProfile(settings)
}

// getHubClientForConnection creates a hub client using credentials from a named hub connection.
// It loads the credential file from the MultiStore and constructs a hub client
// authenticated with the connection's HMAC credentials.
func getHubClientForConnection(name string) (hubclient.Client, error) {
	multiStore := brokercredentials.NewMultiStore("")
	creds, err := multiStore.Load(name)
	if err != nil {
		return nil, fmt.Errorf("hub connection '%s' not found\n\nUse 'scion runtime-broker hubs' to list available connections", name)
	}

	if creds.HubEndpoint == "" {
		return nil, fmt.Errorf("hub connection '%s' has no endpoint configured", name)
	}

	var opts []hubclient.Option
	switch creds.AuthMode {
	case brokercredentials.AuthModeDevAuth:
		opts = append(opts, hubclient.WithAutoDevAuth())
	default:
		if creds.SecretKey != "" {
			secretKey, err := base64.StdEncoding.DecodeString(creds.SecretKey)
			if err != nil {
				return nil, fmt.Errorf("failed to decode secret key for connection '%s': %w", name, err)
			}
			opts = append(opts, hubclient.WithHMACAuth(creds.BrokerID, secretKey))
		} else {
			opts = append(opts, hubclient.WithAutoDevAuth())
		}
	}

	// Transport auth for IAP-protected hubs
	src, mode, err := transportauth.ResolveBrokerTransport(creds.TransportMode, creds.TransportAudience, adcsource.New)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve transport auth: %w", err)
	}
	if src != nil {
		opts = append(opts, hubclient.WithTransportAuth(src, mode))
	}

	client, err := hubclient.New(creds.HubEndpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create hub client for connection '%s': %w", name, err)
	}

	return client, nil
}

// BrokerHubConnectionsResponse mirrors runtimebroker.HubConnectionStatusResponse
// for CLI use without importing the runtimebroker package.
type BrokerHubConnectionsResponse struct {
	Connections []BrokerHubConnectionInfo `json:"connections"`
	Mode        string                    `json:"mode"`
}

// BrokerHubConnectionInfo mirrors runtimebroker.HubConnectionInfo for CLI use.
type BrokerHubConnectionInfo struct {
	Name              string `json:"name"`
	HubEndpoint       string `json:"hubEndpoint"`
	BrokerID          string `json:"brokerId"`
	AuthMode          string `json:"authMode,omitempty"`
	Status            string `json:"status"`
	IsColocated       bool   `json:"isColocated,omitempty"`
	HasHeartbeat      bool   `json:"hasHeartbeat"`
	HasControlChannel bool   `json:"hasControlChannel"`
}

// queryBrokerHubConnections queries the local broker server for live hub connection status.
// Returns nil if the server is not running or the endpoint is not available.
func queryBrokerHubConnections(port int) *BrokerHubConnectionsResponse {
	if port <= 0 {
		port = DefaultBrokerPort
	}

	url := fmt.Sprintf("http://localhost:%d/api/v1/hub-connections", port)

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result BrokerHubConnectionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	return &result
}

// brokerDaemonComponent is the daemon component name of the standalone
// broker (broker.pid, broker.log, broker-args.json).
const brokerDaemonComponent = "broker"

const brokerPortFlagUsage = "Local Runtime Broker API port; when not set, the port of the broker started by 'runtime-broker start', else server.broker.port from settings, else 9800"

// saveBrokerArgs records the args a running broker was launched with, so
// later subcommands can find its port and restart can keep the original
// flags. It is written once the broker is up and removed when it stops. A
// failure only loses that convenience, so it is a warning.
func saveBrokerArgs(globalDir string, args []string) {
	if err := daemon.SaveArgs(brokerDaemonComponent, globalDir, args); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save broker launch args: %v\n", err)
	}
}

// removeBrokerArgs removes the saved launch args once the broker they
// describe is gone, so they cannot point later subcommands at a stale port.
func removeBrokerArgs(globalDir string) {
	if err := daemon.RemoveArgs(brokerDaemonComponent, globalDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove saved broker launch args: %v\n", err)
	}
}

// Seams for tests: launching the daemon, running the foreground server and
// interrupting this process.
var (
	startBrokerDaemon      = daemon.Start
	brokerStartVerifyDelay = 500 * time.Millisecond

	// runForegroundBrokerServer runs 'server start' in-process with args.
	runForegroundBrokerServer = func(args []string) error {
		// Parse the flags for serverStartCmd before calling RunE
		// (SetArgs only works with Execute, not RunE)
		if err := serverStartCmd.ParseFlags(args); err != nil {
			return fmt.Errorf("failed to parse server flags: %w", err)
		}
		return serverStartCmd.RunE(serverStartCmd, []string{})
	}

	// interruptSelf asks this process to shut down the way Ctrl+C does.
	interruptSelf = func() {
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(os.Interrupt)
		}
	}
)

// claimForegroundBrokerRecord returns the record a foreground broker on
// port should save, and false when another broker owns the record: a broker
// daemon is running, or a broker already answers on port.
func claimForegroundBrokerRecord(globalDir string, port int) ([]string, bool) {
	if running, _, _ := daemon.Status(globalDir); running {
		return nil, false
	}
	if _, err := checkLocalBrokerServer(port); err == nil {
		return nil, false
	}
	return pinBrokerPort(buildBrokerDaemonArgs(port, brokerStartAutoProvide, brokerStartDebug), port), true
}

// removeBrokerArgsIfOwned removes the saved launch args only if they are
// still the args this process saved, so a broker that has since saved its
// own record keeps it.
func removeBrokerArgsIfOwned(globalDir string, owned []string) {
	saved, err := daemon.LoadArgs(brokerDaemonComponent, globalDir)
	if err != nil || saved == nil || !slices.Equal(saved, owned) {
		return
	}
	removeBrokerArgs(globalDir)
}

// removeBrokerRecordOnSIGTERM makes SIGTERM (systemd's stop signal) remove
// this foreground broker's record and then shut the server down like Ctrl+C
// (the server traps only os.Interrupt, so SIGTERM would otherwise kill the
// process before deferred cleanup runs). The returned func stops handling.
func removeBrokerRecordOnSIGTERM(globalDir string, owned []string) (stop func()) {
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			removeBrokerArgsIfOwned(globalDir, owned)
			interruptSelf()
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
		})
	}
}

// loadBrokerArgs returns the saved broker launch args, or nil when there
// are none. An unreadable file is reported on stderr and treated as none.
func loadBrokerArgs(globalDir string) []string {
	saved, err := daemon.LoadArgs(brokerDaemonComponent, globalDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: ignoring saved broker launch args %s: %v; using the settings or default port (pass --port to override)\n",
			filepath.Join(globalDir, daemon.ArgsFileName(brokerDaemonComponent)), err)
		return nil
	}
	return saved
}

// settingsBrokerPort returns the port 'server start' gives the runtime
// broker when no --runtime-broker-port is passed: server.broker.port from
// the global settings (or its SCION_SERVER_ environment override), else
// DefaultBrokerPort. It reads the configuration the same way the server
// does (config.LoadGlobalConfig with no --config path).
var settingsBrokerPort = func() int {
	cfg, err := config.LoadGlobalConfig("")
	if err != nil || cfg == nil || cfg.RuntimeBroker.Port <= 0 {
		return DefaultBrokerPort
	}
	return cfg.RuntimeBroker.Port
}

// resolveBrokerStartPort returns the port 'runtime-broker start' runs the
// broker on: --port when given, else the settings port.
func resolveBrokerStartPort(cmd *cobra.Command) int {
	if cmd.Flags().Changed("port") && brokerStartPort > 0 {
		return brokerStartPort
	}
	return settingsBrokerPort()
}

// pinBrokerPort makes the launch args name port explicitly when the
// builders left it out (it is the default) but the settings would move the
// server to another port, for example 'start --port 9800' with
// server.broker.port set.
func pinBrokerPort(args []string, port int) []string {
	if port == DefaultBrokerPort && settingsBrokerPort() != DefaultBrokerPort {
		return append(args, fmt.Sprintf("--runtime-broker-port=%d", port))
	}
	return args
}

// resolveBrokerPort returns the local broker port for cmd: its --port flag
// when set, else the port saved by the running broker's start, else the
// settings port (server.broker.port), else DefaultBrokerPort. cmd may be
// any command (runBrokerStatus is also called from start and restart); only
// an explicitly set "port" flag is used.
func resolveBrokerPort(cmd *cobra.Command) int {
	if cmd != nil {
		if f := cmd.Flags().Lookup("port"); f != nil && f.Changed {
			if p, err := strconv.Atoi(f.Value.String()); err == nil && p > 0 {
				return p
			}
		}
	}
	if globalDir, err := config.GetGlobalDir(); err == nil {
		if saved := loadBrokerArgs(globalDir); saved != nil {
			return brokerPortFromArgs(saved)
		}
	}
	return settingsBrokerPort()
}

// brokerPortFromArgs returns the --runtime-broker-port in saved broker
// launch args, or DefaultBrokerPort when they don't set one (the start
// builders omit it for the default port).
func brokerPortFromArgs(args []string) int {
	for i, a := range args {
		var v string
		switch {
		case strings.HasPrefix(a, "--runtime-broker-port="):
			v = strings.TrimPrefix(a, "--runtime-broker-port=")
		case a == "--runtime-broker-port" && i+1 < len(args):
			v = args[i+1]
		default:
			continue
		}
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return DefaultBrokerPort
}

// argsContain reports whether flag appears in args.
func argsContain(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// resolveBrokerRestartOptions returns the port, auto-provide and debug
// options restart relaunches the daemon with: each flag given to restart
// wins; otherwise the value from the saved launch args of the running
// daemon; otherwise the flag default (for the port, the settings port).
func resolveBrokerRestartOptions(cmd *cobra.Command, saved []string) (port int, autoProvide, debug bool) {
	flags := cmd.Flags()
	port, autoProvide, debug = brokerRestartPort, brokerRestartAutoProvide, brokerRestartDebug
	if saved != nil && !flags.Changed("port") {
		port = brokerPortFromArgs(saved)
	}
	if port <= 0 {
		// No saved args (for example a daemon started by an older binary):
		// the port the server takes from settings, else the default.
		port = settingsBrokerPort()
	}
	if saved == nil {
		return port, autoProvide, debug
	}
	if !flags.Changed("auto-provide") {
		autoProvide = argsContain(saved, "--auto-provide")
	}
	if !flags.Changed("debug") {
		debug = argsContain(saved, "--debug")
	}
	return port, autoProvide, debug
}

// removeHubConnectionSetting removes hub_connections.<name> from the
// settings file in dir and leaves every other setting as it is.
//
//   - No file, or no entry for name: the file is not touched.
//   - Versioned YAML file: the entry is deleted in place
//     (config.PrepareSettingsPathEdits), so comments and every other key,
//     including v1 keys such as image_registry, default_harness_config and
//     server.broker, survive. An emptied hub_connections map is removed too.
//   - Unversioned (legacy-format) file: config.DeleteHubConnection, the
//     writer for that schema, as before. An in-place edit would stamp
//     schema_version without migrating the legacy keys (harnesses, ...), so
//     they would stop being read; the legacy file is migrated by the next
//     settings write instead, as it always was.
//   - Versioned JSON file: refused with a request to edit it by hand.
//     config.DeleteHubConnection would rewrite it through the legacy struct
//     and drop every v1 key, and the in-place editor does not handle JSON.
func removeHubConnectionSetting(dir, name string) error {
	// Held from the read to the write, so the entry check and the
	// last-entry decision below use the file as it is written back.
	// (config.LockSettingsFile serialises writers in this process only.)
	unlock := config.LockSettingsFile()
	defer unlock()

	path := config.GetSettingsPath(dir)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	isJSON := filepath.Ext(path) == ".json"

	var raw map[string]interface{}
	if isJSON {
		err = util.UnmarshalJSONC(data, &raw)
	} else {
		err = yaml.Unmarshal(data, &raw)
	}
	if err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
	}
	conns, _ := raw["hub_connections"].(map[string]interface{})
	if _, ok := conns[name]; !ok {
		// Nothing to remove: don't touch the file.
		return nil
	}

	_, versioned := raw["schema_version"]
	if !versioned && !isJSON {
		// DetectSettingsFormat also recognises a v1 file that lacks
		// schema_version by its v1-only runtime keys.
		version, _ := config.DetectSettingsFormat(data)
		versioned = version != ""
	}
	if !versioned {
		return config.DeleteHubConnection(dir, name, false)
	}
	if isJSON {
		return fmt.Errorf("%s is JSON and cannot be edited in place; remove hub_connections.%s from it by hand", path, name)
	}

	edit := config.SettingsPathEdit{Path: []string{"hub_connections", name}, Delete: true}
	if len(conns) == 1 {
		// The last entry: drop the map rather than leave hub_connections: {}.
		edit.Path = []string{"hub_connections"}
	}
	edits := []config.SettingsPathEdit{edit}
	staged, err := config.PrepareSettingsPathEdits(dir, edits)
	if err != nil {
		return fmt.Errorf("%w; remove hub_connections.%s from %s by hand", err, name, path)
	}
	return staged.Commit()
}
