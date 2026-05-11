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
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	// broker register flags
	brokerForceRegister bool
	brokerAutoProvide   bool
	brokerHubName       string // --name flag for hub connection name

	// broker deregister flags
	brokerDeregisterBrokerOnly bool
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
	brokerProjectID     string
	brokerBrokerID    string // --broker flag for remote broker operations
	brokerMakeDefault bool   // --make-default flag to set broker as project default
	brokerHubFlag     string // --hub flag to target a specific hub connection

	// broker hubs flags
	brokerHubsJSON bool
)

// brokerCmd represents the broker command group
var brokerCmd = &cobra.Command{
	Use:   "broker",
	Short: "Manage the Runtime Broker",
	Long: `Commands for managing this host as a Runtime Broker.

A Runtime Broker is a compute node that executes agents on behalf of the Hub.
Brokers register with the Hub and can be added as providers for projects.

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
1. Remove this broker from all projects it provides for
2. Clear the stored broker token

Use --broker-only to only remove the broker record without affecting project providers.`,
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
  scion broker provide

  # Add local broker as provider for a specific project
  scion broker provide --project <project-id>

  # Add a remote broker as provider for a project (admin only)
  scion broker provide --broker <broker-id> --project <project-id>

  # Add broker as provider and set as default
  scion broker provide --make-default`,
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
  scion broker withdraw

  # Remove local broker as provider from a specific project
  scion broker withdraw --project <project-id>

  # Remove a remote broker as provider from a project (admin only)
  scion broker withdraw --broker <broker-id> --project <project-id>`,
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
  scion broker hubs

  # List hub connections in JSON format
  scion broker hubs --json`,
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

// brokerRestartCmd restarts the broker daemon
var brokerRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Runtime Broker daemon",
	Long: `Restart the Runtime Broker daemon.

This command stops the currently running broker daemon and starts a new one
using the current scion binary. This is useful after installing a new version
of scion to pick up the updated binary.

If the broker is not running as a daemon, this command will return an error.

Examples:
  # Restart the broker daemon
  scion broker restart

  # Restart with a different port
  scion broker restart --port 9801

  # Restart with auto-provide enabled
  scion broker restart --auto-provide`,
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
	brokerRestartCmd.Flags().IntVar(&brokerRestartPort, "port", DefaultBrokerPort, "Runtime Broker API port")
	brokerRestartCmd.Flags().BoolVar(&brokerRestartAutoProvide, "auto-provide", false, "Automatically add as provider for new projects")
	brokerRestartCmd.Flags().BoolVar(&brokerRestartDebug, "debug", false, "Enable debug logging (verbose output)")

	// Status flags
	brokerStatusCmd.Flags().BoolVar(&brokerStatusJSON, "json", false, "Output in JSON format")
	brokerStatusCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker ID to query (for remote broker status)")

	// Register flags
	brokerRegisterCmd.Flags().BoolVar(&brokerForceRegister, "force", false, "Force re-registration even if already registered")
	brokerRegisterCmd.Flags().BoolVar(&brokerAutoProvide, "auto-provide", false, "Automatically add as provider for new projects")
	brokerRegisterCmd.Flags().StringVar(&brokerHubName, "name", "", "Name for this hub connection (derived from endpoint if not specified)")

	// Deregister flags
	brokerDeregisterCmd.Flags().BoolVar(&brokerDeregisterBrokerOnly, "broker-only", false, "Only remove broker record, not project providers")
	brokerDeregisterCmd.Flags().StringVar(&brokerDeregisterName, "name", "", "Name of the hub connection to deregister")

	// Hubs flags
	brokerHubsCmd.Flags().BoolVar(&brokerHubsJSON, "json", false, "Output in JSON format")

	// Start flags
	brokerStartCmd.Flags().BoolVar(&brokerStartForeground, "foreground", false, "Run in foreground instead of as daemon")
	brokerStartCmd.Flags().IntVar(&brokerStartPort, "port", DefaultBrokerPort, "Runtime Broker API port")
	brokerStartCmd.Flags().BoolVar(&brokerStartAutoProvide, "auto-provide", false, "Automatically add as provider for new projects")
	brokerStartCmd.Flags().BoolVar(&brokerStartDebug, "debug", false, "Enable debug logging (verbose output)")

	// Provide/withdraw flags
	brokerProvideCmd.Flags().StringVar(&brokerProjectID, "project", "", "Project name or ID to add as provider for")
	brokerProvideCmd.Flags().StringVar(&brokerProjectID, "grove", "", "Deprecated alias for --project")
	_ = brokerProvideCmd.Flags().MarkDeprecated("grove", "use --project instead")
	_ = brokerProvideCmd.Flags().MarkHidden("grove")

	brokerProvideCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker name or ID to use (for remote broker operations)")
	brokerProvideCmd.Flags().BoolVar(&brokerMakeDefault, "make-default", false, "Set this broker as the default for the project")
	brokerProvideCmd.Flags().StringVar(&brokerHubFlag, "hub", "", "Hub connection name (from 'scion broker hubs')")

	brokerWithdrawCmd.Flags().StringVar(&brokerProjectID, "project", "", "Project name or ID to remove as provider from")
	brokerWithdrawCmd.Flags().StringVar(&brokerProjectID, "grove", "", "Deprecated alias for --project")
	_ = brokerWithdrawCmd.Flags().MarkDeprecated("grove", "use --project instead")
	_ = brokerWithdrawCmd.Flags().MarkHidden("grove")

	brokerWithdrawCmd.Flags().StringVar(&brokerBrokerID, "broker", "", "Broker name or ID to use (for remote broker operations)")
	brokerWithdrawCmd.Flags().StringVar(&brokerHubFlag, "hub", "", "Hub connection name (from 'scion broker hubs')")
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
		return fmt.Errorf("Hub endpoint not configured.\n\nConfigure the Hub endpoint via:\n  - SCION_HUB_ENDPOINT environment variable\n  - hub.endpoint in settings.yaml\n  - --hub flag on any command\n\nExample: scion config set hub.endpoint https://hub.scion.dev --global")
	}

	// Step 1: Check if local broker server is running
	health, err := checkLocalBrokerServer(DefaultBrokerPort)
	if err != nil {
		return fmt.Errorf("broker server not running on port %d.\n\nStart it with: scion broker start\n\nError: %w", DefaultBrokerPort, err)
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
		return fmt.Errorf("Hub at %s is not responding: %w", endpoint, err)
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

	if !projectLinked && !settings.IsHubEnabled() {
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

	// Initialize MultiStore with auto-migration from legacy single-file store
	multiStore := brokercredentials.NewMultiStore("")
	legacyStore := brokercredentials.NewStore("")
	if legacyStore.Exists() {
		if err := multiStore.MigrateFromLegacy(legacyStore.Path()); err != nil {
			fmt.Printf("Warning: failed to migrate legacy credentials: %v\n", err)
		} else {
			fmt.Printf("Migrated legacy credentials to %s\n", multiStore.Dir())
		}
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
			BrokerID: stableBrokerID,
			Name:     brokerName,
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

		if createResp.Reregistered {
			fmt.Printf("Found existing broker registration for '%s' (ID: %s), re-registering...\n", brokerName, createResp.BrokerID)
		} else {
			fmt.Printf("Broker created (ID: %s), completing join...\n", createResp.BrokerID)
		}

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

		// Save credentials to MultiStore
		newCreds := &brokercredentials.BrokerCredentials{
			Name:         hubName,
			BrokerID:     brokerID,
			SecretKey:    joinResp.SecretKey,
			HubEndpoint:  endpoint,
			AuthMode:     brokercredentials.AuthModeHMAC,
			RegisteredAt: time.Now(),
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

	// If project is linked, offer to add this broker as a provider
	if projectID != "" && settings.IsHubEnabled() {
		if hubsync.ShowProjectProviderPrompt(projectName, autoConfirm) {
			req := &hubclient.RegisterProjectRequest{
				ID:       projectID,
				Name:     projectName,
				Path:     resolvedPath,
				BrokerID: brokerID,
			}
			if !isGlobal {
				req.GitRemote = util.NormalizeGitRemote(util.GetGitRemote())
			}

			resp, err := client.Projects().Register(ctx, req)
			if err != nil {
				fmt.Printf("Warning: failed to add broker to project: %v\n", err)
			} else {
				fmt.Printf("Broker added as provider to project '%s'\n", resp.Project.Name)
			}
		}
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
	// Initialize MultiStore with auto-migration from legacy
	multiStore := brokercredentials.NewMultiStore("")
	legacyStore := brokercredentials.NewStore("")
	if legacyStore.Exists() {
		if err := multiStore.MigrateFromLegacy(legacyStore.Path()); err != nil {
			fmt.Printf("Warning: failed to migrate legacy credentials: %v\n", err)
		}
	}

	// Determine which connection to deregister
	var creds *brokercredentials.BrokerCredentials
	var hubName string

	if brokerDeregisterName != "" {
		// Specific connection requested
		hubName = brokerDeregisterName
		loaded, err := multiStore.Load(hubName)
		if err != nil {
			return fmt.Errorf("hub connection '%s' not found.\n\nUse 'scion broker hubs' to list connections.", hubName)
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
				return fmt.Errorf("no broker registration found.\n\nThis host is not registered as a Runtime Broker with the Hub.")
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
		return fmt.Errorf("no broker registration found.\n\nThis host is not registered as a Runtime Broker with the Hub.")
	}

	// Load settings for Hub client
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
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

	// Remove hub_connections entry from global settings
	globalDir, globalErr := config.GetGlobalDir()
	if globalErr == nil && hubName != "" {
		if err := config.DeleteHubConnection(globalDir, hubName, false); err != nil {
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

	return nil
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

func runBrokerStart(cmd *cobra.Command, args []string) error {
	// Get global directory for daemon files
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Check if the broker is managed by the combined server
	if managed, pid := isServerDaemonManagingBroker(globalDir); managed {
		return fmt.Errorf("the runtime broker is managed by the combined server process (PID: %d).\n\nUse 'scion server start/stop/restart' to manage the server, or 'scion broker status' to check broker status.", pid)
	}

	// Foreground mode - just run the server command directly
	if brokerStartForeground {
		// Build args for server start (just the flags, no command names)
		// Use --production to avoid workstation defaults (we only want the broker)
		serverArgs := []string{"--production", "--enable-runtime-broker"}
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
	// Use --foreground so the child process runs directly (daemon.Start handles backgrounding)
	// Use --production to avoid workstation defaults (we only want the broker)
	daemonArgs := []string{"server", "start", "--foreground", "--production", "--enable-runtime-broker"}
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

	// Check if the broker is managed by the combined server
	if managed, pid := isServerDaemonManagingBroker(globalDir); managed {
		return fmt.Errorf("the runtime broker is managed by the combined server process (PID: %d).\n\nUse 'scion server stop' to stop the server, or 'scion broker status' to check broker status.", pid)
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

func runBrokerRestart(cmd *cobra.Command, args []string) error {
	// Get global directory for daemon files
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Check if the broker is managed by the combined server
	if managed, pid := isServerDaemonManagingBroker(globalDir); managed {
		return fmt.Errorf("the runtime broker is managed by the combined server process (PID: %d).\n\nUse 'scion server restart' to restart the server, or 'scion broker status' to check broker status.", pid)
	}

	// Check if daemon is running
	running, pid, _ := daemon.Status(globalDir)
	if !running {
		// Check if server is running on the port (might be foreground)
		health, err := checkLocalBrokerServer(DefaultBrokerPort)
		if err == nil {
			return fmt.Errorf("broker server is running (status: %s) but not as a daemon.\n\nIf running in foreground, use Ctrl+C to stop it and then 'scion broker start' to restart.", health.Status)
		}
		return fmt.Errorf("broker daemon is not running.\n\nUse 'scion broker start' to start it.")
	}

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

	// Build args for the daemon process
	// Use --foreground so the child process runs directly (daemon.Start handles backgrounding)
	// Use --production to avoid workstation defaults (we only want the broker)
	daemonArgs := []string{"server", "start", "--foreground", "--production", "--enable-runtime-broker"}
	if brokerRestartPort != DefaultBrokerPort {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--runtime-broker-port=%d", brokerRestartPort))
	}
	if brokerRestartAutoProvide {
		daemonArgs = append(daemonArgs, "--auto-provide")
	}
	if brokerRestartDebug {
		daemonArgs = append(daemonArgs, "--debug")
	}

	// Start new daemon
	fmt.Printf("Starting broker with new binary...\n")
	if err := daemon.Start(executable, daemonArgs, globalDir); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Verify it started
	time.Sleep(500 * time.Millisecond)
	running, pid, _ = daemon.Status(globalDir)
	if !running {
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPath(globalDir))
	}

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
			return fmt.Errorf("no broker registration found.\n\nRegister with: scion broker register\nOr specify a broker with --broker <name-or-id>")
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
		// Try to resolve local project path when using --hub flag with --project
		if localProjectPath == "" {
			if rp, _, err := config.ResolveProjectPath(projectPath); err == nil {
				localProjectPath = rp
			}
		}
	} else {
		resolvedPath, _, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path: %w", err)
		}
		if localProjectPath == "" {
			localProjectPath = resolvedPath
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

	// Handle --make-default flag
	if brokerMakeDefault {
		currentDefault := resp.Project.DefaultRuntimeBrokerID

		if currentDefault == brokerID {
			// Already the default, nothing to do
			fmt.Printf("Broker '%s' is already the default for project '%s'\n", brokerName, resp.Project.Name)
		} else if currentDefault == "" {
			// No default set - the server should have auto-set it during provide,
			// but set it explicitly to be sure
			_, err := client.Projects().Update(ctx, resp.Project.ID, &hubclient.UpdateProjectRequest{
				DefaultRuntimeBrokerID: brokerID,
			})
			if err != nil {
				return fmt.Errorf("failed to set default broker: %w", err)
			}
			fmt.Printf("Broker '%s' set as default for project '%s'\n", brokerName, resp.Project.Name)
		} else {
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
			return fmt.Errorf("no broker registration found.\n\nRegister with: scion broker register\nOr specify a broker with --broker <name-or-id>")
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
	health, err := checkLocalBrokerServer(DefaultBrokerPort)
	if err == nil {
		status.ServerRunning = true
		status.ServerPort = DefaultBrokerPort
		status.ServerStatus = health.Status
		status.ServerVersion = health.Version
	}

	// Load hub connections from MultiStore
	multiStore := brokercredentials.NewMultiStore("")

	// Auto-migrate legacy credentials if they exist
	legacyStore := brokercredentials.NewStore("")
	if legacyStore.Exists() {
		if err := multiStore.MigrateFromLegacy(legacyStore.Path()); err != nil {
			util.Debugf("Warning: failed to migrate legacy credentials: %v", err)
		}
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
						if err == nil && projectsResp != nil {
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
		// Try to get live status from running broker server
		liveStatus := queryBrokerHubConnections(DefaultBrokerPort)
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
			connStatus := "unknown"
			if s, ok := liveStatusMap[conn.Name]; ok {
				connStatus = s
			}
			fmt.Printf("  %s\n", conn.Name)
			fmt.Printf("    Hub:         %s\n", conn.HubEndpoint)
			fmt.Printf("    Auth:        %s\n", conn.AuthMode)
			fmt.Printf("    Status:      %s\n", connStatus)
			if !conn.RegisteredAt.IsZero() {
				fmt.Printf("    Registered:  %s\n", conn.RegisteredAt.Format("2006-01-02"))
			}
		}
		if status.HubConnected {
			fmt.Printf("\n  Connected:   yes (%s)\n", status.HubStatus)
			if status.BrokerStatus != "" {
				fmt.Printf("  Status:      %s\n", status.BrokerStatus)
			}
			if !status.LastHeartbeat.IsZero() {
				fmt.Printf("  Last seen:   %s\n", formatRelativeTime(status.LastHeartbeat))
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
				fmt.Printf("  Last seen:   %s\n", formatRelativeTime(status.LastHeartbeat))
			}
		} else {
			fmt.Printf("  Connected:   no (Hub unreachable)\n")
		}
	} else {
		fmt.Println("Hub Registration")
		fmt.Println("----------------")
		fmt.Printf("  Registered:  no\n")
		fmt.Printf("\n  Run 'scion broker register' to register with the Hub.\n")
	}
	fmt.Println()

	// Projects
	if len(status.Projects) > 0 {
		fmt.Println("Projects (Provider)")
		fmt.Println("-----------------")
		for _, g := range status.Projects {
			fmt.Printf("  - %s (ID: %s)\n", g.Name, g.ID)
		}
	} else if status.Registered {
		fmt.Println("Projects (Provider)")
		fmt.Println("-----------------")
		fmt.Printf("  (none)\n")
		fmt.Printf("\n  Run 'scion broker provide' to add this broker as a provider for a project.\n")
	}

	return nil
	}


func runBrokerHubs(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if brokerHubsJSON {
		outputFormat = "json"
	}

	// Initialize MultiStore with auto-migration from legacy
	multiStore := brokercredentials.NewMultiStore("")
	legacyStore := brokercredentials.NewStore("")
	if legacyStore.Exists() {
		if err := multiStore.MigrateFromLegacy(legacyStore.Path()); err != nil {
			fmt.Printf("Warning: failed to migrate legacy credentials: %v\n", err)
		}
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
		fmt.Println("\nRun 'scion broker register' to register with a Hub.")
		return nil
	}

	// Try to get live status from running broker server
	liveStatus := queryBrokerHubConnections(DefaultBrokerPort)

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
	fmt.Fprintf(w, "  NAME\tHUB ENDPOINT\tAUTH\tSTATUS\tREGISTERED\n")
	for _, c := range allCreds {
		regDate := ""
		if !c.RegisteredAt.IsZero() {
			regDate = c.RegisteredAt.Format("2006-01-02")
		}
		authMode := string(c.AuthMode)
		if authMode == "" {
			authMode = "hmac"
		}
		status := "unknown"
		if s, ok := liveStatusMap[c.Name]; ok {
			status = s
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", c.Name, c.HubEndpoint, authMode, status, regDate)
	}
	w.Flush()

	fmt.Printf("\nCredentials directory: %s\n", multiStore.Dir())

	return nil
}

// runRemoteBrokerStatus fetches and displays status for a remote broker from the Hub
func runRemoteBrokerStatus(brokerID string) error {
	// Load settings for Hub client
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
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

	// Get projects this broker provides for
	projectsResp, err := client.RuntimeBrokers().ListProjects(ctx, brokerID)
	if err == nil && projectsResp != nil {
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
		fmt.Printf("  Last seen:   %s\n", formatRelativeTime(status.LastHeartbeat))
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

// getHubClientForConnection creates a hub client using credentials from a named hub connection.
// It loads the credential file from the MultiStore and constructs a hub client
// authenticated with the connection's HMAC credentials.
func getHubClientForConnection(name string) (hubclient.Client, error) {
	multiStore := brokercredentials.NewMultiStore("")
	creds, err := multiStore.Load(name)
	if err != nil {
		return nil, fmt.Errorf("hub connection '%s' not found.\n\nUse 'scion broker hubs' to list available connections.", name)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result BrokerHubConnectionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	return &result
}
