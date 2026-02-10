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
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/brokercredentials"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/credentials"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/hubsync"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/ptone/scion-agent/pkg/version"
	"github.com/spf13/cobra"
)

var (
	hubOutputJSON bool
)

// hubCmd represents the hub command
var hubCmd = &cobra.Command{
	Use:   "hub",
	Short: "Interact with the Scion Hub",
	Long: `Commands for interacting with a remote Scion Hub.

The Hub provides centralized coordination for groves, agents, and templates
across multiple runtime brokers.

Configure the Hub endpoint via:
  - SCION_HUB_ENDPOINT environment variable
  - hub.endpoint in settings.yaml
  - --hub flag on any command`,
}

// hubStatusCmd shows Hub connection status
var hubStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Hub connection status",
	Long:  `Show the current Hub connection status and configuration.`,
	RunE:  runHubStatus,
}

// hubGrovesCmd lists groves on the Hub
var hubGrovesCmd = &cobra.Command{
	Use:     "groves [grove-name]",
	Aliases: []string{"grove"},
	Short:   "List groves on the Hub",
	Long: `List groves registered on the Hub that you have access to.

If a grove name is provided, shows detailed information for that grove.

Examples:
  # List all groves
  scion hub groves

  # Show info for a specific grove
  scion hub grove my-project`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubGroves,
}

// hubGrovesInfoCmd shows detailed information about a grove
var hubGrovesInfoCmd = &cobra.Command{
	Use:   "info [grove-name]",
	Short: "Show detailed information about a grove",
	Long: `Show detailed information about a grove on the Hub.

Displays grove metadata including creation date, broker providers,
and agent count.

If no grove name is provided, the current grove is used.

Examples:
  # Show info for the current grove
  scion hub groves info

  # Show info for a grove by name
  scion hub groves info my-project

  # Output as JSON
  scion hub groves info my-project --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubGrovesInfo,
}

// hubGrovesDeleteCmd deletes a grove from the Hub
var hubGrovesDeleteCmd = &cobra.Command{
	Use:   "delete [grove-name]",
	Short: "Delete a grove from the Hub",
	Long: `Delete a grove from the Hub.

This will remove the grove and all associated broker provider relationships.
Agents within the grove will also be deleted unless --preserve-agents is set.

If no grove name is provided, the current grove is used.

Examples:
  # Delete the current grove (with confirmation)
  scion hub groves delete

  # Delete a grove by name (with confirmation)
  scion hub groves delete my-project

  # Delete without confirmation
  scion hub groves delete my-project -y

  # Delete grove but preserve agents
  scion hub groves delete my-project --preserve-agents`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubGrovesDelete,
}

// hubBrokersCmd lists runtime brokers on the Hub
var hubBrokersCmd = &cobra.Command{
	Use:     "brokers",
	Aliases: []string{"broker"},
	Short:   "List runtime brokers on the Hub",
	Long:    `List runtime brokers registered on the Hub.`,
	RunE:    runHubBrokers,
}

// hubBrokersInfoCmd shows detailed information about a broker
var hubBrokersInfoCmd = &cobra.Command{
	Use:   "info [broker-name]",
	Short: "Show detailed information about a broker",
	Long: `Show detailed information about a runtime broker on the Hub.

Displays broker metadata including name, status, version, last heartbeat,
capabilities, available profiles, and groves it provides for.

If no broker name is provided, the current host's broker is used (if registered).

Examples:
  # Show info for the current host's broker
  scion hub brokers info

  # Show info for a broker by name
  scion hub brokers info my-broker

  # Output as JSON
  scion hub brokers info my-broker --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubBrokersInfo,
}

// hubBrokersDeleteCmd deletes a broker from the Hub
var hubBrokersDeleteCmd = &cobra.Command{
	Use:   "delete [broker-name]",
	Short: "Delete a broker from the Hub",
	Long: `Delete a runtime broker from the Hub.

This will remove the broker registration and all associated grove provider relationships.

Examples:
  # Delete a broker by name (with confirmation)
  scion hub brokers delete my-broker

  # Delete without confirmation
  scion hub brokers delete my-broker -y`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubBrokersDelete,
}

// hubEnableCmd enables Hub integration
var hubEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable Hub integration",
	Long: `Enable Hub integration for agent operations.

When enabled, agent operations (create, start, delete) will be routed through
the Hub API instead of being performed locally. This allows centralized
coordination of agents across multiple runtime brokers.

The Hub endpoint must be configured before enabling:
  - SCION_HUB_ENDPOINT environment variable
  - hub.endpoint in settings.yaml
  - --hub flag on any command`,
	RunE: runHubEnable,
}

// hubDisableCmd disables Hub integration
var hubDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable Hub integration",
	Long: `Disable Hub integration for agent operations.

When disabled, agent operations are performed locally on this broker.
The Hub configuration is preserved and can be re-enabled later.`,
	RunE: runHubDisable,
}

// hubLinkCmd links the current grove to the Hub
var hubLinkCmd = &cobra.Command{
	Use:   "link",
	Short: "Link this grove to the Hub",
	Long: `Link the current grove (project) to the Hub.

This command associates your local grove with the Hub, enabling:
- Centralized agent coordination across multiple brokers
- Agent state synchronization
- Remote management via the Hub UI or API

The grove will be created on the Hub if it doesn't exist, or linked
to an existing grove with a matching name or git remote.

Examples:
  # Link the current project grove
  scion hub link

  # Link the global grove
  scion hub link --global`,
	RunE: runHubLink,
}

// hubUnlinkCmd unlinks the current grove from the Hub
var hubUnlinkCmd = &cobra.Command{
	Use:   "unlink",
	Short: "Unlink this grove from the Hub",
	Long: `Unlink the current grove from the Hub locally.

This command disables Hub integration for the grove without removing
the grove or its agents from the Hub. Other brokers can still manage
the grove through the Hub.

Use 'scion hub link' to re-link the grove later.

Examples:
  # Unlink the current project grove
  scion hub unlink

  # Unlink the global grove
  scion hub unlink --global`,
	RunE: runHubUnlink,
}

var (
	hubGrovesDeletePreserveAgents bool
)

func init() {
	rootCmd.AddCommand(hubCmd)
	hubCmd.AddCommand(hubStatusCmd)
	hubCmd.AddCommand(hubGrovesCmd)
	hubCmd.AddCommand(hubBrokersCmd)
	hubCmd.AddCommand(hubEnableCmd)
	hubCmd.AddCommand(hubDisableCmd)
	hubCmd.AddCommand(hubLinkCmd)
	hubCmd.AddCommand(hubUnlinkCmd)

	// Grove subcommands
	hubGrovesCmd.AddCommand(hubGrovesInfoCmd)
	hubGrovesCmd.AddCommand(hubGrovesDeleteCmd)

	// Broker subcommands
	hubBrokersCmd.AddCommand(hubBrokersInfoCmd)
	hubBrokersCmd.AddCommand(hubBrokersDeleteCmd)

	// Common flags
	hubStatusCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubGrovesCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubBrokersCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")

	// Grove subcommand flags
	hubGrovesInfoCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubGrovesDeleteCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Skip confirmation prompt")
	hubGrovesDeleteCmd.Flags().BoolVar(&hubGrovesDeletePreserveAgents, "preserve-agents", false, "Preserve agents when deleting grove")

	// Broker subcommand flags
	hubBrokersInfoCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubBrokersDeleteCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Skip confirmation prompt")
}

// authInfo describes the authentication method being used
type authInfo struct {
	Method      string // Human-readable description
	MethodType  string // Short type: "oauth", "bearer", "apikey", "devauth", "none"
	Source      string // Where the credentials came from
	IsDevAuth   bool   // Whether dev-auth is being used
	HasOAuth    bool   // Whether OAuth credentials are present
	OAuthCreds  *credentials.HubCredentials
}

// getAuthInfo determines what authentication method will be used for a given endpoint
func getAuthInfo(settings *config.Settings, endpoint string) authInfo {
	info := authInfo{
		Method:     "none",
		MethodType: "none",
	}

	// Check settings-based auth first
	if settings.Hub != nil {
		if settings.Hub.Token != "" {
			info.Method = "Bearer token"
			info.MethodType = "bearer"
			info.Source = "settings"
			return info
		}
		if settings.Hub.APIKey != "" {
			info.Method = "API key"
			info.MethodType = "apikey"
			info.Source = "settings"
			return info
		}
	}

	// Check for OAuth credentials from scion hub auth login
	if endpoint != "" {
		if creds, err := credentials.Load(endpoint); err == nil && creds.AccessToken != "" {
			info.Method = "OAuth"
			info.MethodType = "oauth"
			info.Source = "scion hub auth login"
			info.HasOAuth = true
			info.OAuthCreds = creds
			return info
		}
	}

	// Check for dev auth
	token, source := apiclient.ResolveDevTokenWithSource()
	if token != "" {
		info.Method = "Dev auth"
		info.MethodType = "devauth"
		info.Source = source
		info.IsDevAuth = true
		return info
	}

	return info
}

func getHubClient(settings *config.Settings) (hubclient.Client, error) {
	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return nil, fmt.Errorf("Hub endpoint not configured. Set SCION_HUB_ENDPOINT or use --hub flag")
	}

	var opts []hubclient.Option

	// Get auth info for logging
	info := getAuthInfo(settings, endpoint)

	// Add authentication - check in priority order
	// Note: BrokerToken is intentionally NOT used here. BrokerTokens are for broker-level
	// operations (registration, heartbeats) and are NOT user authentication tokens.
	// For user operations (listing groves, agents, etc.), we use user tokens, API keys,
	// OAuth credentials, or dev auth.
	authConfigured := false
	if settings.Hub != nil {
		if settings.Hub.Token != "" {
			opts = append(opts, hubclient.WithBearerToken(settings.Hub.Token))
			authConfigured = true
		} else if settings.Hub.APIKey != "" {
			opts = append(opts, hubclient.WithAPIKey(settings.Hub.APIKey))
			authConfigured = true
		}
	}

	// Check for OAuth credentials from scion hub auth login
	if !authConfigured {
		if accessToken := credentials.GetAccessToken(endpoint); accessToken != "" {
			opts = append(opts, hubclient.WithBearerToken(accessToken))
			authConfigured = true
		}
	}

	// Fallback to auto dev auth if no explicit auth configured
	// This checks SCION_DEV_TOKEN env var and ~/.scion/dev-token file
	if !authConfigured {
		opts = append(opts, hubclient.WithAutoDevAuth())
	}

	util.Debugf("Hub client auth: %s (source: %s)", info.Method, info.Source)
	util.Debugf("Hub endpoint: %s", endpoint)

	opts = append(opts, hubclient.WithTimeout(30*time.Second))

	return hubclient.New(endpoint, opts...)
}

func runHubStatus(cmd *cobra.Command, args []string) error {
	// Resolve grove path to find project settings
	resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	endpoint := GetHubEndpoint(settings)

	hubEnabled := settings.IsHubEnabled()

	// Get authentication info
	authInfo := getAuthInfo(settings, endpoint)

	if hubOutputJSON {
		status := map[string]interface{}{
			"enabled":       hubEnabled,
			"cliOverride":   noHub,
			"endpoint":      endpoint,
			"configured":    settings.IsHubConfigured(),
			"groveId":       settings.GroveID,
			"scionVersion":  version.Short(),
		}
		if settings.Hub != nil {
			status["brokerId"] = settings.Hub.BrokerID
			status["hasToken"] = settings.Hub.Token != ""
			status["hasApiKey"] = settings.Hub.APIKey != ""
			status["hasBrokerToken"] = settings.Hub.BrokerToken != ""
		}

		// Add auth info to JSON output
		status["authMethod"] = authInfo.MethodType
		status["authSource"] = authInfo.Source
		status["isDevAuth"] = authInfo.IsDevAuth
		if authInfo.OAuthCreds != nil && authInfo.OAuthCreds.User != nil {
			status["authUser"] = map[string]string{
				"id":          authInfo.OAuthCreds.User.ID,
				"email":       authInfo.OAuthCreds.User.Email,
				"displayName": authInfo.OAuthCreds.User.DisplayName,
				"role":        authInfo.OAuthCreds.User.Role,
			}
			if !authInfo.OAuthCreds.ExpiresAt.IsZero() {
				status["authExpires"] = authInfo.OAuthCreds.ExpiresAt.Format(time.RFC3339)
			}
		}

		// Try to connect and get health
		if endpoint != "" && !noHub {
			client, err := getHubClient(settings)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if health, err := client.Health(ctx); err == nil {
					status["connected"] = true
					status["hubVersion"] = health.Version
					status["hubStatus"] = health.Status

					// Add grove context to JSON output
					groveContext := getGroveContextJSON(client, resolvedPath, isGlobal, settings)
					status["groveContext"] = groveContext
				} else {
					status["connected"] = false
					status["error"] = err.Error()
				}
			}
		}

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Text output
	fmt.Println("Hub Integration Status")
	fmt.Println("======================")
	fmt.Printf("Enabled:    %v\n", hubEnabled)
	if noHub {
		fmt.Printf("            (overridden by --no-hub flag)\n")
	}
	fmt.Printf("Endpoint:   %s\n", valueOrNone(endpoint))
	fmt.Printf("Configured: %v\n", settings.IsHubConfigured())

	// Show grove_id from top-level setting (where it's now stored)
	fmt.Printf("Grove ID:   %s\n", valueOrNone(settings.GroveID))
	if settings.Hub != nil {
		fmt.Printf("Broker ID:  %s\n", valueOrNone(settings.Hub.BrokerID))
	}

	// Authentication status section
	fmt.Println()
	fmt.Println("Authentication")
	fmt.Println("--------------")
	if authInfo.MethodType == "none" {
		fmt.Println("Method:     Not authenticated")
	} else {
		fmt.Printf("Method:     %s\n", authInfo.Method)
		if authInfo.IsDevAuth {
			fmt.Println("            (development mode - not for production use)")
		}
		if authInfo.HasOAuth && authInfo.OAuthCreds != nil {
			if authInfo.OAuthCreds.User != nil {
				fmt.Printf("User:       %s (%s)\n", authInfo.OAuthCreds.User.DisplayName, authInfo.OAuthCreds.User.Email)
				if authInfo.OAuthCreds.User.Role != "" {
					fmt.Printf("Role:       %s\n", authInfo.OAuthCreds.User.Role)
				}
			}
			if !authInfo.OAuthCreds.ExpiresAt.IsZero() {
				if time.Now().After(authInfo.OAuthCreds.ExpiresAt) {
					fmt.Printf("Expires:    %s (EXPIRED)\n", authInfo.OAuthCreds.ExpiresAt.Format(time.RFC3339))
				} else {
					fmt.Printf("Expires:    %s\n", authInfo.OAuthCreds.ExpiresAt.Format(time.RFC3339))
				}
			}
		}
	}

	// Try to connect
	if endpoint != "" && !noHub {
		client, err := getHubClient(settings)
		if err != nil {
			fmt.Printf("\nConnection: failed (%s)\n", err)
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		health, err := client.Health(ctx)
		if err != nil {
			fmt.Println()
			fmt.Println("Hub Server")
			fmt.Println("----------")
			fmt.Printf("Connection: failed (%s)\n", err)
		} else {
			fmt.Println()
			fmt.Println("Hub Server")
			fmt.Println("----------")
			fmt.Printf("Connection: ok\n")
			fmt.Printf("Hub Version: %s\n", health.Version)
			fmt.Printf("Hub Status:  %s\n", health.Status)
			fmt.Printf("Scion Version: %s\n", version.Short())

			// If OAuth, verify auth is actually working by calling /auth/me
			if authInfo.HasOAuth {
				meCtx, meCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer meCancel()
				if _, err := client.Auth().Me(meCtx); err != nil {
					fmt.Printf("\nAuth verification: failed (%s)\n", err)
					fmt.Println("Run 'scion hub auth login' to re-authenticate.")
				}
			}

			// Show grove context if we're in a grove
			printGroveContext(client, resolvedPath, isGlobal, settings)
		}
	}

	return nil
}

// printGroveContext prints information about the current grove's registration and available brokers.
func printGroveContext(client hubclient.Client, grovePath string, isGlobal bool, settings *config.Settings) {
	// Determine grove name from path
	groveName := filepath.Base(filepath.Dir(grovePath))
	if isGlobal {
		groveName = "global"
	}

	fmt.Println()
	fmt.Println("Grove Context")
	fmt.Println("-------------")
	fmt.Printf("Grove:      %s\n", groveName)
	if isGlobal {
		fmt.Printf("Type:       user global\n")
	} else {
		fmt.Printf("Type:       project\n")
	}

	// Get git remote for this grove (if not global)
	var gitRemote string
	if !isGlobal {
		gitRemote = util.GetGitRemoteDir(filepath.Dir(grovePath))
		if gitRemote != "" {
			fmt.Printf("Git Remote: %s\n", gitRemote)
		}
	}

	// Check if grove is linked to the Hub
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var linkedGrove *hubclient.Grove

	// First try to find by grove_id if we have one
	if settings.GroveID != "" {
		grove, err := client.Groves().Get(ctx, settings.GroveID)
		if err == nil {
			linkedGrove = grove
		}
	}

	// If not found by ID and we have a git remote, try by git remote
	if linkedGrove == nil && gitRemote != "" {
		resp, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{
			GitRemote: util.NormalizeGitRemote(gitRemote),
		})
		if err == nil && len(resp.Groves) > 0 {
			linkedGrove = &resp.Groves[0]
		}
	}

	// If still not found and global, try by name
	if linkedGrove == nil && isGlobal {
		resp, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{
			Name: "global",
		})
		if err == nil && len(resp.Groves) > 0 {
			linkedGrove = &resp.Groves[0]
		}
	}

	if linkedGrove == nil {
		fmt.Printf("Linked: no\n")
		fmt.Println()
		fmt.Println("Run 'scion hub link' to link this grove with the Hub.")
		return
	}

	fmt.Printf("Linked: yes\n")
	fmt.Printf("Hub Grove:  %s (ID: %s)\n", linkedGrove.Name, linkedGrove.ID)

	// Get runtime brokers for this grove
	brokersResp, err := client.RuntimeBrokers().List(ctx, &hubclient.ListBrokersOptions{
		GroveID: linkedGrove.ID,
	})
	if err != nil {
		fmt.Printf("Brokers:    (error fetching: %s)\n", err)
		return
	}

	if len(brokersResp.Brokers) == 0 {
		fmt.Printf("Brokers:    none\n")
		return
	}

	// Count only online brokers as "available"
	onlineCount := 0
	for _, broker := range brokersResp.Brokers {
		if broker.Status == "online" {
			onlineCount++
		}
	}

	fmt.Printf("Brokers:    %d available\n", onlineCount)
	for _, broker := range brokersResp.Brokers {
		statusIndicator := ""
		if broker.Status == "online" {
			statusIndicator = "[online]"
		} else {
			statusIndicator = fmt.Sprintf("[%s]", broker.Status)
		}
		fmt.Printf("  - %s %s\n", broker.Name, statusIndicator)
	}
}

// getGroveContextJSON returns grove context information for JSON output.
func getGroveContextJSON(client hubclient.Client, grovePath string, isGlobal bool, settings *config.Settings) map[string]interface{} {
	result := make(map[string]interface{})

	// Determine grove name from path
	groveName := filepath.Base(filepath.Dir(grovePath))
	if isGlobal {
		groveName = "global"
	}

	result["name"] = groveName
	result["isGlobal"] = isGlobal
	if isGlobal {
		result["type"] = "user global"
	} else {
		result["type"] = "project"
	}

	// Get git remote for this grove (if not global)
	var gitRemote string
	if !isGlobal {
		gitRemote = util.GetGitRemoteDir(filepath.Dir(grovePath))
		if gitRemote != "" {
			result["gitRemote"] = gitRemote
		}
	}

	// Check if grove is linked to the Hub
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var linkedGrove *hubclient.Grove

	// First try to find by grove_id if we have one
	if settings.GroveID != "" {
		grove, err := client.Groves().Get(ctx, settings.GroveID)
		if err == nil {
			linkedGrove = grove
		}
	}

	// If not found by ID and we have a git remote, try by git remote
	if linkedGrove == nil && gitRemote != "" {
		resp, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{
			GitRemote: util.NormalizeGitRemote(gitRemote),
		})
		if err == nil && len(resp.Groves) > 0 {
			linkedGrove = &resp.Groves[0]
		}
	}

	// If still not found and global, try by name
	if linkedGrove == nil && isGlobal {
		resp, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{
			Name: "global",
		})
		if err == nil && len(resp.Groves) > 0 {
			linkedGrove = &resp.Groves[0]
		}
	}

	if linkedGrove == nil {
		result["linked"] = false
		return result
	}

	result["linked"] = true
	result["hubGroveId"] = linkedGrove.ID
	result["hubGroveName"] = linkedGrove.Name

	// Get runtime brokers for this grove
	brokersResp, err := client.RuntimeBrokers().List(ctx, &hubclient.ListBrokersOptions{
		GroveID: linkedGrove.ID,
	})
	if err != nil {
		result["brokersError"] = err.Error()
		return result
	}

	brokers := make([]map[string]interface{}, 0, len(brokersResp.Brokers))
	for _, broker := range brokersResp.Brokers {
		brokers = append(brokers, map[string]interface{}{
			"id":     broker.ID,
			"name":   broker.Name,
			"status": broker.Status,
		})
	}
	result["brokers"] = brokers

	return result
}

func runHubGroves(cmd *cobra.Command, args []string) error {
	// If a grove name is provided, show info for that grove
	if len(args) == 1 {
		return runHubGrovesInfo(cmd, args)
	}

	// Resolve grove path to find project settings
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

	resp, err := client.Groves().List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list groves: %w", err)
	}

	if hubOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.Groves)
	}

	if len(resp.Groves) == 0 {
		fmt.Println("No groves found")
		return nil
	}

	// Fetch brokers to map IDs to names for the "Default Broker" column
	brokerNames := make(map[string]string)
	brokersResp, err := client.RuntimeBrokers().List(ctx, nil)
	if err == nil {
		for _, b := range brokersResp.Brokers {
			brokerNames[b.ID] = b.Name
		}
	}

	fmt.Printf("%-36s  %-20s  %-10s  %-20s  %s\n", "ID", "NAME", "AGENTS", "DEFAULT BROKER", "GIT REMOTE")
	fmt.Printf("%-36s  %-20s  %-10s  %-20s  %s\n", "------------------------------------", "--------------------", "----------", "--------------------", "----------")
	for _, g := range resp.Groves {
		gitRemote := g.GitRemote
		if len(gitRemote) > 40 {
			gitRemote = gitRemote[:37] + "..."
		}
		brokerDisplay := g.DefaultRuntimeBrokerID
		if name, ok := brokerNames[g.DefaultRuntimeBrokerID]; ok {
			brokerDisplay = name
		}
		fmt.Printf("%-36s  %-20s  %-10d  %-20s  %s\n", g.ID, truncate(g.Name, 20), g.AgentCount, truncate(brokerDisplay, 20), gitRemote)
	}

	return nil
}

func runHubGrovesInfo(cmd *cobra.Command, args []string) error {
	// Resolve grove path to find project settings
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

	// Determine grove name from args or current grove
	var groveName string
	if len(args) > 0 {
		groveName = args[0]
	} else {
		// Use current grove name
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

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find the grove by name
	grove, err := findGroveByName(ctx, client, groveName)
	if err != nil {
		return err
	}

	// Get providers for this grove
	providersResp, err := client.Groves().ListProviders(ctx, grove.ID)
	if err != nil {
		// Non-fatal: we can still show grove info without providers
		util.Debugf("Failed to get providers: %v", err)
	}

	if hubOutputJSON {
		output := map[string]interface{}{
			"id":         grove.ID,
			"name":       grove.Name,
			"slug":       grove.Slug,
			"gitRemote":  grove.GitRemote,
			"visibility": grove.Visibility,
			"agentCount": grove.AgentCount,
			"created":    grove.Created,
			"updated":    grove.Updated,
			"createdBy":  grove.CreatedBy,
			"ownerId":    grove.OwnerID, // TODO: resolve to user display name when available
		}
		if grove.DefaultRuntimeBrokerID != "" {
			output["defaultRuntimeBrokerId"] = grove.DefaultRuntimeBrokerID
		}
		if len(grove.Labels) > 0 {
			output["labels"] = grove.Labels
		}
		if providersResp != nil && len(providersResp.Providers) > 0 {
			output["providers"] = providersResp.Providers
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	// Text output
	fmt.Println("Grove Information")
	fmt.Println("=================")
	fmt.Printf("ID:          %s\n", grove.ID)
	fmt.Printf("Name:        %s\n", grove.Name)
	fmt.Printf("Slug:        %s\n", grove.Slug)
	if grove.GitRemote != "" {
		fmt.Printf("Git Remote:  %s\n", grove.GitRemote)
	}
	fmt.Printf("Visibility:  %s\n", valueOrDefault(grove.Visibility, "private"))
	fmt.Printf("Agents:      %d\n", grove.AgentCount)
	fmt.Printf("Created:     %s\n", grove.Created.Format(time.RFC3339))
	if !grove.Updated.IsZero() && grove.Updated != grove.Created {
		fmt.Printf("Updated:     %s\n", grove.Updated.Format(time.RFC3339))
	}
	// TODO: Resolve owner ID to display name when user lookup is available
	if grove.OwnerID != "" {
		fmt.Printf("Owner:       %s (TODO: resolve to display name)\n", grove.OwnerID)
	}

	// Show providers
	if providersResp != nil && len(providersResp.Providers) > 0 {
		fmt.Println()
		fmt.Println("Broker Providers")
		fmt.Println("----------------")
		for _, p := range providersResp.Providers {
			statusIndicator := ""
			if p.Status == "online" {
				statusIndicator = "[online]"
			} else {
				statusIndicator = fmt.Sprintf("[%s]", p.Status)
			}
			defaultIndicator := ""
			if p.BrokerID == grove.DefaultRuntimeBrokerID {
				defaultIndicator = " (default)"
			}
			fmt.Printf("  - %s %s%s\n", p.BrokerName, statusIndicator, defaultIndicator)
		}
	} else {
		fmt.Println()
		fmt.Println("Broker Providers: none")
	}

	return nil
}

func runHubGrovesDelete(cmd *cobra.Command, args []string) error {
	// Resolve grove path to find project settings
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

	// Determine grove name from args or current grove
	var groveName string
	if len(args) > 0 {
		groveName = args[0]
	} else {
		// Use current grove name
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

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find the grove by name
	grove, err := findGroveByName(ctx, client, groveName)
	if err != nil {
		return err
	}

	// Get providers for display in confirmation
	providersResp, err := client.Groves().ListProviders(ctx, grove.ID)
	if err != nil {
		util.Debugf("Failed to get providers: %v", err)
	}

	// Show confirmation prompt
	if !hubsync.ShowGroveDeletePrompt(grove.Name, grove.AgentCount, providersResp, autoConfirm) {
		return fmt.Errorf("deletion cancelled")
	}

	// Delete the grove
	deleteAgents := !hubGrovesDeletePreserveAgents
	if err := client.Groves().Delete(ctx, grove.ID, deleteAgents); err != nil {
		return fmt.Errorf("failed to delete grove: %w", err)
	}

	fmt.Printf("Grove '%s' deleted successfully.\n", grove.Name)
	if deleteAgents {
		fmt.Printf("Deleted %d agent(s).\n", grove.AgentCount)
	}
	if providersResp != nil && len(providersResp.Providers) > 0 {
		fmt.Printf("Removed %d broker provider association(s).\n", len(providersResp.Providers))
	}

	return nil
}

// findGroveByName finds a grove by name (case-insensitive) and returns it.
// Returns an error if not found or multiple matches are found.
func findGroveByName(ctx context.Context, client hubclient.Client, name string) (*hubclient.Grove, error) {
	resp, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{
		Name: name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for grove: %w", err)
	}

	if len(resp.Groves) == 0 {
		return nil, fmt.Errorf("grove '%s' not found", name)
	}

	if len(resp.Groves) > 1 {
		fmt.Printf("Multiple groves found with name '%s':\n", name)
		for _, g := range resp.Groves {
			fmt.Printf("  - %s (ID: %s)\n", g.Name, g.ID)
		}
		return nil, fmt.Errorf("ambiguous grove name - please use the grove ID instead")
	}

	return &resp.Groves[0], nil
}

// valueOrDefault returns value if non-empty, otherwise returns the default.
func valueOrDefault(value, defaultVal string) string {
	if value == "" {
		return defaultVal
	}
	return value
}

func runHubBrokers(cmd *cobra.Command, args []string) error {
	// Resolve grove path to find project settings
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

	resp, err := client.RuntimeBrokers().List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list brokers: %w", err)
	}

	if hubOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.Brokers)
	}

	if len(resp.Brokers) == 0 {
		fmt.Println("No runtime brokers found")
		return nil
	}

	fmt.Printf("%-36s  %-20s  %-10s  %-12s  %s\n", "ID", "NAME", "STATUS", "AUTO-PROVIDE", "LAST SEEN")
	fmt.Printf("%-36s  %-20s  %-10s  %-12s  %s\n", "------------------------------------", "--------------------", "----------", "------------", "---------------")
	for _, h := range resp.Brokers {
		lastSeen := "-"
		if !h.LastHeartbeat.IsZero() {
			lastSeen = formatRelativeTime(h.LastHeartbeat)
		}
		autoProvide := "no"
		if h.AutoProvide {
			autoProvide = "yes"
		}
		fmt.Printf("%-36s  %-20s  %-10s  %-12s  %s\n", h.ID, truncate(h.Name, 20), h.Status, autoProvide, lastSeen)
	}

	return nil
}

func runHubBrokersInfo(cmd *cobra.Command, args []string) error {
	// Resolve grove path to find project settings
	resolvedPath, _, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	// Determine broker ID from args or current host's broker
	var brokerNameOrID string
	if len(args) > 0 {
		brokerNameOrID = args[0]
	} else {
		// Try to get the current host's broker ID
		brokerNameOrID = getCurrentHostBrokerID(settings)
		if brokerNameOrID == "" {
			return fmt.Errorf("no broker name provided and this host is not registered as a broker.\n\nUsage: scion hub brokers info [broker-name]")
		}
	}

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find the broker by name or ID
	broker, err := resolveBrokerByNameOrID(ctx, client, brokerNameOrID)
	if err != nil {
		return err
	}

	if hubOutputJSON {
		output := map[string]interface{}{
			"id":              broker.ID,
			"name":            broker.Name,
			"slug":            broker.Slug,
			"version":         broker.Version,
			"status":          broker.Status,
			"connectionState": broker.ConnectionState,
			"autoProvide":     broker.AutoProvide,
			"created":         broker.Created,
			"updated":         broker.Updated,
		}
		if !broker.LastHeartbeat.IsZero() {
			output["lastHeartbeat"] = broker.LastHeartbeat
		}
		if broker.Endpoint != "" {
			output["endpoint"] = broker.Endpoint
		}
		if broker.CreatedBy != "" {
			output["createdBy"] = broker.CreatedBy
		}
		if broker.Capabilities != nil {
			output["capabilities"] = broker.Capabilities
		}
		if len(broker.Profiles) > 0 {
			output["profiles"] = broker.Profiles
		}
		if len(broker.Groves) > 0 {
			output["groves"] = broker.Groves
		}
		if len(broker.Labels) > 0 {
			output["labels"] = broker.Labels
		}
		if len(broker.Annotations) > 0 {
			output["annotations"] = broker.Annotations
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	// Text output
	fmt.Println("Broker Information")
	fmt.Println("==================")
	fmt.Printf("ID:          %s\n", broker.ID)
	fmt.Printf("Name:        %s\n", broker.Name)
	if broker.Slug != "" && broker.Slug != broker.Name {
		fmt.Printf("Slug:        %s\n", broker.Slug)
	}
	fmt.Printf("Status:      %s\n", valueOrDefault(broker.Status, "unknown"))
	if broker.ConnectionState != "" {
		fmt.Printf("Connection:  %s\n", broker.ConnectionState)
	}
	if broker.Version != "" {
		fmt.Printf("Version:     %s\n", broker.Version)
	}
	if !broker.LastHeartbeat.IsZero() {
		fmt.Printf("Last Seen:   %s (%s)\n", formatRelativeTime(broker.LastHeartbeat), broker.LastHeartbeat.Format(time.RFC3339))
	}
	if broker.Endpoint != "" {
		fmt.Printf("Endpoint:    %s\n", broker.Endpoint)
	}
	fmt.Printf("Auto-Provide: %v\n", broker.AutoProvide)
	fmt.Printf("Created:     %s\n", broker.Created.Format(time.RFC3339))
	if !broker.Updated.IsZero() && broker.Updated != broker.Created {
		fmt.Printf("Updated:     %s\n", broker.Updated.Format(time.RFC3339))
	}

	// Show capabilities
	if broker.Capabilities != nil {
		fmt.Println()
		fmt.Println("Capabilities")
		fmt.Println("------------")
		fmt.Printf("Web PTY:     %v\n", broker.Capabilities.WebPTY)
		fmt.Printf("Sync:        %v\n", broker.Capabilities.Sync)
		fmt.Printf("Attach:      %v\n", broker.Capabilities.Attach)
	}

	// Show profiles
	if len(broker.Profiles) > 0 {
		fmt.Println()
		fmt.Println("Profiles")
		fmt.Println("--------")
		for _, p := range broker.Profiles {
			availStr := "available"
			if !p.Available {
				availStr = "unavailable"
			}
			if p.Context != "" || p.Namespace != "" {
				details := ""
				if p.Context != "" {
					details = fmt.Sprintf("context: %s", p.Context)
				}
				if p.Namespace != "" {
					if details != "" {
						details += ", "
					}
					details += fmt.Sprintf("namespace: %s", p.Namespace)
				}
				fmt.Printf("  - %s (%s) [%s] %s\n", p.Name, p.Type, availStr, details)
			} else {
				fmt.Printf("  - %s (%s) [%s]\n", p.Name, p.Type, availStr)
			}
		}
	} else {
		fmt.Println()
		fmt.Println("Profiles: none")
	}

	// Show groves
	if len(broker.Groves) > 0 {
		fmt.Println()
		fmt.Println("Groves")
		fmt.Println("------")
		for _, g := range broker.Groves {
			fmt.Printf("  - %s (%d agents)\n", g.GroveName, g.AgentCount)
		}
	} else {
		fmt.Println()
		fmt.Println("Groves: none")
	}

	return nil
}

func runHubBrokersDelete(cmd *cobra.Command, args []string) error {
	// Broker name is required for delete
	if len(args) == 0 {
		return fmt.Errorf("broker name or ID is required.\n\nUsage: scion hub brokers delete <broker-name>")
	}

	brokerNameOrID := args[0]

	// Resolve grove path to find project settings
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

	// Find the broker by name or ID
	broker, err := resolveBrokerByNameOrID(ctx, client, brokerNameOrID)
	if err != nil {
		return err
	}

	// Extract grove names for the confirmation prompt
	groveNames := make([]string, len(broker.Groves))
	for i, g := range broker.Groves {
		groveNames[i] = g.GroveName
	}

	// Show confirmation prompt
	if !hubsync.ShowBrokerDeletePrompt(broker.Name, groveNames, autoConfirm) {
		return fmt.Errorf("deletion cancelled")
	}

	// Delete the broker
	if err := client.RuntimeBrokers().Delete(ctx, broker.ID); err != nil {
		return fmt.Errorf("failed to delete broker: %w", err)
	}

	fmt.Printf("Broker '%s' deleted successfully.\n", broker.Name)
	if len(broker.Groves) > 0 {
		fmt.Printf("Removed from %d grove(s).\n", len(broker.Groves))
	}

	return nil
}

// getCurrentHostBrokerID returns the broker ID for the current host, if registered.
// Checks broker credentials first, then falls back to global settings.
func getCurrentHostBrokerID(settings *config.Settings) string {
	// Check broker credentials first
	credStore := brokercredentials.NewStore("")
	creds, err := credStore.Load()
	if err == nil && creds != nil && creds.BrokerID != "" {
		return creds.BrokerID
	}

	// Check global settings
	globalDir, err := config.GetGlobalDir()
	if err == nil {
		globalSettings, err := config.LoadSettings(globalDir)
		if err == nil && globalSettings.Hub != nil && globalSettings.Hub.BrokerID != "" {
			return globalSettings.Hub.BrokerID
		}
	}

	// Check current settings
	if settings.Hub != nil && settings.Hub.BrokerID != "" {
		return settings.Hub.BrokerID
	}

	return ""
}

func valueOrNone(s string) string {
	if s == "" {
		return "(not configured)"
	}
	return s
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func formatRelativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func runHubEnable(cmd *cobra.Command, args []string) error {
	// Resolve grove path
	resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
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

	// Try to connect and verify Hub is healthy before enabling
	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	health, err := client.Health(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Hub at %s: %w\n\nVerify the Hub endpoint is correct and the Hub is running.", endpoint, err)
	}

	// Save the enabled setting
	if err := config.UpdateSetting(resolvedPath, "hub.enabled", "true", isGlobal); err != nil {
		return fmt.Errorf("failed to save setting: %w", err)
	}

	// If the endpoint was provided via --hub flag, persist it to settings
	if hubEndpoint != "" {
		if err := config.UpdateSetting(resolvedPath, "hub.endpoint", hubEndpoint, isGlobal); err != nil {
			return fmt.Errorf("failed to save endpoint: %w", err)
		}
	}

	fmt.Printf("Hub integration enabled.\n")
	fmt.Printf("Endpoint: %s\n", endpoint)
	fmt.Printf("Hub Status: %s (version %s)\n", health.Status, health.Version)
	fmt.Println("\nAgent operations (create, start, delete) will now be routed through the Hub.")
	fmt.Println("Use 'scion hub disable' to switch back to local-only mode.")

	return nil
}

func runHubDisable(cmd *cobra.Command, args []string) error {
	// Resolve grove path
	resolvedPath, isGlobal, err := config.ResolveGrovePath(grovePath)
	if err != nil {
		return fmt.Errorf("failed to resolve grove path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	if !settings.IsHubEnabled() {
		fmt.Println("Hub integration is already disabled.")
		return nil
	}

	// Save the disabled setting
	if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
		return fmt.Errorf("failed to save setting: %w", err)
	}

	fmt.Println("Hub integration disabled.")
	fmt.Println("Agent operations will now be performed locally.")
	fmt.Println("\nHub configuration is preserved. Use 'scion hub enable' to re-enable.")

	return nil
}

func runHubLink(cmd *cobra.Command, args []string) error {
	// Resolve grove path
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

	// Show confirmation prompt
	if !hubsync.ShowGroveLinkPrompt(groveName, endpoint, autoConfirm) {
		return fmt.Errorf("linking cancelled")
	}

	// Verify authentication before proceeding
	authInfo := getAuthInfo(settings, endpoint)
	if authInfo.MethodType == "none" {
		return fmt.Errorf("not authenticated to Hub at %s\n\nPlease log in first:\n  scion hub auth login", endpoint)
	}

	// Create Hub client
	client, err := getHubClient(settings)
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}

	// Check Hub connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := client.Health(ctx); err != nil {
		return fmt.Errorf("Hub at %s is not responding: %w", endpoint, err)
	}

	// Ensure grove_id exists
	groveID := settings.GroveID
	if groveID == "" {
		groveID = config.GenerateGroveIDForDir(filepath.Dir(resolvedPath))
		if err := config.UpdateSetting(resolvedPath, "grove_id", groveID, isGlobal); err != nil {
			return fmt.Errorf("failed to save grove_id: %w", err)
		}
	}

	// Check if grove already exists on Hub
	linked, err := isGroveLinked(ctx, client, groveID)
	if err != nil {
		util.Debugf("Error checking grove link status: %v", err)
	}

	if linked {
		fmt.Printf("Grove '%s' is already linked to the Hub (ID: %s)\n", groveName, groveID)
	} else {
		// Check for existing groves with the same name
		resp, err := client.Groves().List(ctx, &hubclient.ListGrovesOptions{
			Name: groveName,
		})
		if err != nil {
			return fmt.Errorf("failed to search for matching groves: %w", err)
		}

		if len(resp.Groves) > 0 {
			// Found matching groves - ask user what to do
			matches := make([]hubsync.GroveMatch, len(resp.Groves))
			for i, g := range resp.Groves {
				matches[i] = hubsync.GroveMatch{
					ID:        g.ID,
					Name:      g.Name,
					GitRemote: g.GitRemote,
				}
			}

			choice, selectedID := hubsync.ShowMatchingGrovesPrompt(groveName, matches, autoConfirm)
			switch choice {
			case hubsync.GroveChoiceCancel:
				return fmt.Errorf("linking cancelled")
			case hubsync.GroveChoiceLink:
				// Update local grove_id to the selected grove
				if err := config.UpdateSetting(resolvedPath, "grove_id", selectedID, isGlobal); err != nil {
					return fmt.Errorf("failed to update local grove_id: %w", err)
				}
				groveID = selectedID
				fmt.Printf("Linked to existing grove (ID: %s)\n", groveID)
			case hubsync.GroveChoiceRegisterNew:
				// Generate a new grove ID
				groveID = config.GenerateGroveIDForDir(filepath.Dir(resolvedPath))
				if err := config.UpdateSetting(resolvedPath, "grove_id", groveID, isGlobal); err != nil {
					return fmt.Errorf("failed to update local grove_id: %w", err)
				}
				// Register as new grove
				if err := registerGroveOnHub(ctx, client, groveID, groveName, resolvedPath, isGlobal); err != nil {
					return err
				}
			}
		} else {
			// No matching groves - create new one
			if err := registerGroveOnHub(ctx, client, groveID, groveName, resolvedPath, isGlobal); err != nil {
				return err
			}
		}
	}

	// Enable Hub integration for this grove
	if err := config.UpdateSetting(resolvedPath, "hub.enabled", "true", isGlobal); err != nil {
		return fmt.Errorf("failed to enable hub: %w", err)
	}

	// Save endpoint if provided via flag
	if hubEndpoint != "" {
		if err := config.UpdateSetting(resolvedPath, "hub.endpoint", hubEndpoint, isGlobal); err != nil {
			return fmt.Errorf("failed to save endpoint: %w", err)
		}
	}

	fmt.Println()
	fmt.Printf("Grove '%s' is now linked to the Hub.\n", groveName)

	// Offer to sync agents
	if hubsync.ShowSyncAfterLinkPrompt(autoConfirm) {
		// Create HubContext for sync
		hubCtx := &hubsync.HubContext{
			Client:    client,
			Endpoint:  endpoint,
			Settings:  settings,
			GroveID:   groveID,
			GrovePath: resolvedPath,
			IsGlobal:  isGlobal,
		}

		syncResult, err := hubsync.CompareAgents(ctx, hubCtx)
		if err != nil {
			fmt.Printf("Warning: failed to compare agents: %v\n", err)
		} else if !syncResult.IsInSync() {
			if hubsync.ShowSyncPlan(syncResult, autoConfirm) {
				if err := hubsync.ExecuteSync(ctx, hubCtx, syncResult, autoConfirm); err != nil {
					fmt.Printf("Warning: failed to sync agents: %v\n", err)
				}
			}
		} else {
			fmt.Println("Agents are already in sync.")
		}
	}

	// Display available brokers for this grove
	listBrokersForGrove(ctx, client, groveID)

	return nil
}

// registerGroveOnHub registers a new grove on the Hub.
func registerGroveOnHub(ctx context.Context, client hubclient.Client, groveID, groveName, grovePath string, isGlobal bool) error {
	var gitRemote string
	if !isGlobal {
		gitRemote = util.GetGitRemote()
	}

	req := &hubclient.RegisterGroveRequest{
		ID:        groveID,
		Name:      groveName,
		GitRemote: util.NormalizeGitRemote(gitRemote),
		Path:      grovePath,
	}

	resp, err := client.Groves().Register(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to register grove: %w", err)
	}

	if resp.Created {
		fmt.Printf("Created new grove: %s (ID: %s)\n", resp.Grove.Name, resp.Grove.ID)
	} else {
		fmt.Printf("Linked to existing grove: %s (ID: %s)\n", resp.Grove.Name, resp.Grove.ID)
	}

	return nil
}

func runHubUnlink(cmd *cobra.Command, args []string) error {
	// Resolve grove path
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

	// Check if grove is currently linked
	if !settings.IsHubEnabled() {
		fmt.Println("This grove is not linked to the Hub.")
		return nil
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

	// Show confirmation prompt
	if !hubsync.ShowGroveUnlinkPrompt(groveName, autoConfirm) {
		return fmt.Errorf("unlinking cancelled")
	}

	// Disable Hub integration for this grove
	if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
		return fmt.Errorf("failed to disable hub: %w", err)
	}

	fmt.Println()
	fmt.Printf("Grove '%s' has been unlinked from the Hub.\n", groveName)
	fmt.Println("The grove and its agents remain on the Hub for other brokers.")
	fmt.Println("Use 'scion hub link' to re-link this grove.")

	return nil
}

// DefaultBrokerPort is the default port for the local broker server.
const DefaultBrokerPort = 9800

// BrokerHealthResponse represents the response from the broker /healthz endpoint.
type BrokerHealthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Mode    string            `json:"mode"`
	Uptime  string            `json:"uptime"`
	Checks  map[string]string `json:"checks"`
}

// checkLocalBrokerServer checks if the local broker server is running and healthy.
// Returns the health response if healthy, or an error if not accessible.
func checkLocalBrokerServer(port int) (*BrokerHealthResponse, error) {
	if port <= 0 {
		port = DefaultBrokerPort
	}

	url := fmt.Sprintf("http://localhost:%d/healthz", port)

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("broker server not responding: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("broker server returned status %d", resp.StatusCode)
	}

	var health BrokerHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("failed to parse health response: %w", err)
	}

	return &health, nil
}

// isGroveLinked checks if the grove is linked to the Hub (has hub.enabled=true and exists on the Hub).
func isGroveLinked(ctx context.Context, client hubclient.Client, groveID string) (bool, error) {
	if groveID == "" {
		return false, nil
	}

	_, err := client.Groves().Get(ctx, groveID)
	if err != nil {
		errStr := err.Error()
		if containsIgnoreCase(errStr, "404") || containsIgnoreCase(errStr, "not found") {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// containsIgnoreCase checks if s contains substr (case-insensitive).
func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			len(s) > len(substr) &&
				(s[:len(substr)] == substr ||
					containsIgnoreCaseSlow(s, substr)))
}

func containsIgnoreCaseSlow(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if equalFoldSlice(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func equalFoldSlice(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// listBrokersForGrove fetches and displays available runtime brokers for a grove.
func listBrokersForGrove(ctx context.Context, client hubclient.Client, groveID string) {
	resp, err := client.RuntimeBrokers().List(ctx, &hubclient.ListBrokersOptions{
		GroveID: groveID,
	})
	if err != nil {
		util.Debugf("Failed to list brokers for grove: %v", err)
		return
	}

	if len(resp.Brokers) == 0 {
		fmt.Println()
		fmt.Println("Warning: This grove has no active runtime brokers.")
		fmt.Println("Register one with 'scion broker register'")
		return
	}

	fmt.Println()
	fmt.Println("Runtime brokers available for this grove:")
	for _, b := range resp.Brokers {
		status := b.Status
		if status == "" {
			status = "unknown"
		}
		fmt.Printf("  - %s (%s)\n", b.Name, status)
	}
}
