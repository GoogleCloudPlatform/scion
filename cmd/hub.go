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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
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

The Hub provides centralized coordination for projects, agents, and templates
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

// hubProjectsCmd lists projects on the Hub
var hubProjectsCmd = &cobra.Command{
	Use:     "projects [project-name]",
	Aliases: []string{"project"},
	Short:   "List projects on the Hub",
	Long: `List projects registered on the Hub that you have access to.

If a project name is provided, shows detailed information for that project.

Examples:
  # List all projects
  scion hub projects

  # Show info for a specific project
  scion hub project my-project`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubProjects,
}

// hubProjectsInfoCmd shows detailed information about a project
var hubProjectsInfoCmd = &cobra.Command{
	Use:   "info [project-name]",
	Short: "Show detailed information about a project",
	Long: `Show detailed information about a project on the Hub.

Displays project metadata including creation date, broker providers,
and agent count.

If no project name is provided, the current project is used.

Examples:
  # Show info for the current project
  scion hub projects info

  # Show info for a project by name
  scion hub projects info my-project

  # Output as JSON
  scion hub projects info my-project --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubProjectsInfo,
}

// hubProjectsDeleteCmd deletes a project from the Hub
var hubProjectsDeleteCmd = &cobra.Command{
	Use:   "delete [project-name]",
	Short: "Delete a project from the Hub",
	Long: `Delete a project from the Hub.

This will remove the project and all associated broker provider relationships.
All agents within the project will be stopped and deleted.

If no project name is provided, the current project is used.

Examples:
  # Delete the current project (with confirmation)
  scion hub projects delete

  # Delete a project by name (with confirmation)
  scion hub projects delete my-project

  # Delete without confirmation
  scion hub projects delete my-project -y`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubProjectsDelete,
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
capabilities, available profiles, and projects it provides for.

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

This will remove the broker registration and all associated project provider relationships.

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

// hubLinkCmd links the current project to the Hub
var hubLinkCmd = &cobra.Command{
	Use:   "link",
	Short: "Link this project to the Hub",
	Long: `Link the current project to the Hub.

This command associates your local project with the Hub, enabling:
- Centralized agent coordination across multiple brokers
- Agent state synchronization
- Remote management via the Hub UI or API

The project will be created on the Hub if it doesn't exist, or linked
to an existing project with a matching name or git remote.

Examples:
  # Link the current project
  scion hub link

  # Link the global project
  scion hub link --global`,
	RunE: runHubLink,
}

// hubUnlinkCmd unlinks the current project from the Hub
var hubUnlinkCmd = &cobra.Command{
	Use:   "unlink",
	Short: "Unlink this project from the Hub",
	Long: `Unlink the current project from the Hub locally.

This command disables Hub integration for the project without removing
the project or its agents from the Hub. Other brokers can still manage
the project through the Hub.

Use 'scion hub link' to re-link the project later.

Examples:
  # Unlink the current project
  scion hub unlink

  # Unlink the global project
  scion hub unlink --global`,
	RunE: runHubUnlink,
}

var (
	hubProjectCreateSlug       string
	hubProjectCreateName       string
	hubProjectCreateBranch     string
	hubProjectCreateVisibility string
)

// hubProjectCreateCmd creates a project on the Hub from a git URL
var hubProjectCreateCmd = &cobra.Command{
	Use:   "create <git-url>",
	Short: "Create a project on the Hub from a git repository URL",
	Long: `Creates a new project on the Hub anchored to a remote git repository.
The project can be used to start agents without a local checkout of the repository.

Multiple projects can reference the same git URL. When the URL already has
projects on the Hub, the existing projects are shown and the new project receives
a serial-numbered slug (e.g., acme-widgets-1, acme-widgets-2).

Examples:
  # Create from HTTPS URL
  scion hub projects create https://github.com/acme/widgets.git

  # Create from SSH URL
  scion hub projects create git@github.com:acme/widgets.git

  # Create with a specific branch
  scion hub projects create https://github.com/acme/widgets.git --branch release/v2

  # Create with a custom slug
  scion hub projects create https://github.com/acme/widgets.git --slug widgets`,
	Args: cobra.ExactArgs(1),
	RunE: runHubProjectCreate,
}

func init() {
	rootCmd.AddCommand(hubCmd)
	hubCmd.AddCommand(hubStatusCmd)
	hubCmd.AddCommand(hubProjectsCmd)
	hubCmd.AddCommand(hubBrokersCmd)
	hubCmd.AddCommand(hubEnableCmd)
	hubCmd.AddCommand(hubDisableCmd)
	hubCmd.AddCommand(hubLinkCmd)
	hubCmd.AddCommand(hubUnlinkCmd)

	// Project subcommands
	hubProjectsCmd.AddCommand(hubProjectsInfoCmd)
	hubProjectsCmd.AddCommand(hubProjectsDeleteCmd)
	hubProjectsCmd.AddCommand(hubProjectCreateCmd)

	// Hidden aliases for 'groves' for backward compatibility
	hubGrovesCmd := &cobra.Command{
		Use:    "groves",
		Aliases: []string{"grove"},
		Hidden: true,
		Short:  "Alias for 'projects'",
		RunE:   runHubProjects,
		Args:   cobra.MaximumNArgs(1),
	}
	hubCmd.AddCommand(hubGrovesCmd)

	// Add the same subcommands to the hidden alias
	hubGrovesInfoCmd := *hubProjectsInfoCmd
	hubGrovesDeleteCmd := *hubProjectsDeleteCmd
	hubGrovesCreateCmd := *hubProjectCreateCmd
	hubGrovesCmd.AddCommand(&hubGrovesInfoCmd, &hubGrovesDeleteCmd, &hubGrovesCreateCmd)

	// Broker subcommands
	hubBrokersCmd.AddCommand(hubBrokersInfoCmd)
	hubBrokersCmd.AddCommand(hubBrokersDeleteCmd)

	// Common flags
	hubStatusCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubProjectsCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubBrokersCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")

	// Project subcommand flags
	hubProjectsInfoCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubProjectsDeleteCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Skip confirmation prompt")
	hubProjectsDeleteCmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Non-interactive mode: implies --yes, errors on ambiguous prompts")
	// Project create flags
	hubProjectCreateCmd.Flags().StringVar(&hubProjectCreateSlug, "slug", "", "Override the auto-derived slug")
	hubProjectCreateCmd.Flags().StringVar(&hubProjectCreateName, "name", "", "Human-friendly display name (defaults to repo name)")
	hubProjectCreateCmd.Flags().StringVar(&hubProjectCreateBranch, "branch", "", "Base branch for the project (defaults to detected default branch, or main)")
	hubProjectCreateCmd.Flags().StringVar(&hubProjectCreateVisibility, "visibility", "", "Project visibility: private, team, or public (default: private)")
	hubProjectCreateCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")

	// Also link flags to the hidden alias subcommands so they work too
	hubGrovesInfoCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubGrovesDeleteCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Skip confirmation prompt")
	hubGrovesDeleteCmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Non-interactive mode: implies --yes, errors on ambiguous prompts")
	hubGrovesCreateCmd.Flags().StringVar(&hubProjectCreateSlug, "slug", "", "Override the auto-derived slug")
	hubGrovesCreateCmd.Flags().StringVar(&hubProjectCreateName, "name", "", "Human-friendly display name (defaults to repo name)")
	hubGrovesCreateCmd.Flags().StringVar(&hubProjectCreateBranch, "branch", "", "Base branch for the project (defaults to detected default branch, or main)")
	hubGrovesCreateCmd.Flags().StringVar(&hubProjectCreateVisibility, "visibility", "", "Project visibility: private, team, or public (default: private)")
	hubGrovesCreateCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")

	// Broker subcommand flags
	hubBrokersInfoCmd.Flags().BoolVar(&hubOutputJSON, "json", false, "Output in JSON format")
	hubBrokersDeleteCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Skip confirmation prompt")
	hubBrokersDeleteCmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Non-interactive mode: implies --yes, errors on ambiguous prompts")
}

// authInfo describes the authentication method being used
type authInfo struct {
	Method      string // Human-readable description
	MethodType  string // Short type: "oauth", "bearer", "apikey", "devauth", "none"
	Source      string // Where the credentials came from
	IsDevAuth   bool   // Whether dev-auth is being used
	HasOAuth    bool   // Whether OAuth credentials are present
	OAuthCreds  *credentials.HubCredentials
	TokenExpiry *time.Time // Expiry time extracted from agent JWT, nil if unknown
}

// parseJWTExpiry extracts the expiry time from a JWT without verifying the signature.
// Returns nil if the token cannot be parsed or has no expiry claim.
func parseJWTExpiry(tokenString string) *time.Time {
	parts := strings.SplitN(tokenString, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == nil {
		return nil
	}
	t := time.Unix(int64(*claims.Exp), 0)
	return &t
}

// printTokenExpiry prints the token expiry in a human-friendly format.
func printTokenExpiry(expiry time.Time) {
	now := time.Now()
	if now.After(expiry) {
		ago := now.Sub(expiry).Truncate(time.Minute)
		fmt.Printf("Expires:    %s (EXPIRED %s ago)\n", expiry.Format("2006-01-02 15:04:05 MST"), ago)
	} else {
		remaining := expiry.Sub(now).Truncate(time.Minute)
		fmt.Printf("Expires:    %s (in %s)\n", expiry.Format("2006-01-02 15:04:05 MST"), remaining)
	}
}

func isLocalhostEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// readAgentTokenFile reads the canonical agent token from ~/.scion/scion-token.
// Returns empty string if the file doesn't exist (e.g. not running in a container).
func readAgentTokenFile() string {
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".scion", "scion-token"))
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(string(data))
	return token
}

// getAuthInfo determines what authentication method will be used for a given endpoint.
// Note: hub.token and hub.apiKey are deprecated and no longer checked.
func getAuthInfo(settings *config.Settings, endpoint string) authInfo {
	info := authInfo{
		Method:     "none",
		MethodType: "none",
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

	// Check for agent auth token from canonical token file or bootstrap env var.
	// When the endpoint is localhost and dev auth is available, prefer dev auth
	// over a non-dev agent token — the scion-token may be stale from a previous
	// remote hub connection while the dev-token was written by the running local server.
	if token := readAgentTokenFile(); token != "" {
		if apiclient.IsDevToken(token) {
			info.Method = "Agent token (dev)"
			info.MethodType = "agent_token"
			info.Source = "scion-token file"
			info.IsDevAuth = true
			info.TokenExpiry = parseJWTExpiry(token)
			return info
		}
		if isLocalhostEndpoint(endpoint) {
			if devToken, devSource := apiclient.ResolveDevTokenWithSource(); devToken != "" {
				util.Debugf("Skipping non-dev agent token from scion-token file; using dev auth (%s) for localhost endpoint", devSource)
				info.Method = "Dev auth"
				info.MethodType = "devauth"
				info.Source = devSource
				info.IsDevAuth = true
				return info
			}
		}
		info.Method = "Agent token"
		info.MethodType = "agent_token"
		info.Source = "scion-token file"
		info.TokenExpiry = parseJWTExpiry(token)
		return info
	}
	if token := os.Getenv("SCION_AUTH_TOKEN"); token != "" {
		if apiclient.IsDevToken(token) {
			info.Method = "Agent token (dev)"
			info.MethodType = "agent_token"
			info.Source = "SCION_AUTH_TOKEN env"
			info.IsDevAuth = true
		} else {
			info.Method = "Agent token"
			info.MethodType = "agent_token"
			info.Source = "SCION_AUTH_TOKEN env"
		}
		info.TokenExpiry = parseJWTExpiry(token)
		return info
	}

	// Check for legacy agent-mode token
	if token := os.Getenv("SCION_HUB_TOKEN"); token != "" {
		info.Method = "Bearer token"
		info.MethodType = "bearer"
		info.Source = "SCION_HUB_TOKEN env"
		return info
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

	// Add authentication - check in priority order.
	// Note: hub.token and hub.apiKey are deprecated and no longer used for auth.
	// Auth priority: OAuth credentials > scion-token file > SCION_AUTH_TOKEN env > SCION_HUB_TOKEN env > auto dev auth.
	// Exception: for localhost endpoints, dev auth takes priority over non-dev agent tokens
	// to avoid stale scion-token files from previous remote hub connections.
	authConfigured := false

	// Check for OAuth credentials from scion hub auth login
	if accessToken := credentials.GetAccessToken(endpoint); accessToken != "" {
		opts = append(opts, hubclient.WithBearerToken(accessToken))
		authConfigured = true
	}

	// Check for agent auth token from canonical token file, then bootstrap env var
	if !authConfigured {
		if token := readAgentTokenFile(); token != "" {
			if !apiclient.IsDevToken(token) && isLocalhostEndpoint(endpoint) {
				if devToken := apiclient.ResolveDevToken(); devToken != "" {
					opts = append(opts, hubclient.WithBearerToken(devToken))
					authConfigured = true
				}
			}
			if !authConfigured {
				opts = append(opts, hubclient.WithAgentToken(token))
				authConfigured = true
			}
		} else if token := os.Getenv("SCION_AUTH_TOKEN"); token != "" {
			opts = append(opts, hubclient.WithAgentToken(token))
			authConfigured = true
		}
	}

	// Check for legacy agent-mode token (running inside a container)
	if !authConfigured {
		if token := os.Getenv("SCION_HUB_TOKEN"); token != "" {
			opts = append(opts, hubclient.WithBearerToken(token))
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

// hubEnabledScope describes where the hub.enabled setting comes from.
type hubEnabledScope struct {
	// Enabled is the effective value of hub.enabled after merging.
	Enabled bool
	// Scope is "project", "global", or "default" indicating where the value originates.
	Scope string
	// Inherited is true when a project-scoped invocation uses a global setting.
	Inherited bool
}

// getHubEnabledScope determines where the hub.enabled setting comes from.
// When operating at project scope, it checks whether the project has its own
// hub.enabled setting or is inheriting from the global settings.
func getHubEnabledScope(resolvedPath string, isGlobal bool, mergedSettings *config.Settings) hubEnabledScope {
	result := hubEnabledScope{
		Enabled: mergedSettings.IsHubEnabled(),
	}

	if isGlobal {
		result.Scope = "global"
		return result
	}

	// Check if enabled implicitly via credentials — this takes priority
	// over any hub.enabled setting in config files.
	if mergedSettings.Hub != nil && mergedSettings.Hub.Endpoint != "" &&
		(mergedSettings.Hub.Token != "" || mergedSettings.Hub.APIKey != "") {
		result.Scope = "implicit"
		return result
	}

	// Check if the project itself has hub.enabled set
	projectSettings, err := config.LoadSettingsFromDir(resolvedPath)
	if err == nil && projectSettings.Hub != nil && projectSettings.Hub.Enabled != nil {
		result.Scope = "project"
		return result
	}

	// Project doesn't have its own setting — check if global has one
	globalDir, _ := config.GetGlobalDir()
	if globalDir != "" {
		globalSettings, err := config.LoadSettingsFromDir(globalDir)
		if err == nil && globalSettings.Hub != nil && globalSettings.Hub.Enabled != nil {
			result.Scope = "global"
			result.Inherited = true
			return result
		}
	}

	// Neither project nor global has it set — default (false)
	result.Scope = "default"
	return result
}

// hubEndpointScope describes where the hub.endpoint setting comes from.
type hubEndpointScope struct {
	// Endpoint is the resolved value.
	Endpoint string
	// Source is "flag", "project", "global", "env", or "none".
	Source string
	// Inherited is true when a project-scoped invocation uses a global or env setting.
	Inherited bool
}

// getHubEndpointScope determines where the hub endpoint comes from.
func getHubEndpointScope(resolvedPath string, isGlobal bool, settings *config.Settings) hubEndpointScope {
	// --hub flag takes top priority
	if hubEndpoint != "" {
		return hubEndpointScope{Endpoint: hubEndpoint, Source: "flag"}
	}

	if !isGlobal {
		// Check if project has its own endpoint
		projectSettings, err := config.LoadSettingsFromDir(resolvedPath)
		if err == nil && projectSettings.Hub != nil && projectSettings.Hub.Endpoint != "" {
			return hubEndpointScope{Endpoint: projectSettings.Hub.Endpoint, Source: "project"}
		}
	}

	// Check global settings
	globalDir, _ := config.GetGlobalDir()
	if globalDir != "" {
		globalSettings, _ := config.LoadSettingsFromDir(globalDir)
		if globalSettings != nil && globalSettings.Hub != nil && globalSettings.Hub.Endpoint != "" {
			return hubEndpointScope{
				Endpoint:  globalSettings.Hub.Endpoint,
				Source:    "global",
				Inherited: !isGlobal,
			}
		}
	}

	// Check env var
	if ep := os.Getenv("SCION_HUB_ENDPOINT"); ep != "" {
		return hubEndpointScope{Endpoint: ep, Source: "env", Inherited: !isGlobal}
	}

	return hubEndpointScope{Source: "none"}
}

func runHubStatus(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if hubOutputJSON {
		outputFormat = "json"
	}

	// Resolve project path to find project settings
	resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	endpoint := GetHubEndpoint(settings)

	hubEnabled := settings.IsHubEnabled()

	// Determine scope of hub settings
	enabledScope := getHubEnabledScope(resolvedPath, isGlobal, settings)
	endpointScope := getHubEndpointScope(resolvedPath, isGlobal, settings)

	// Get authentication info
	authInfo := getAuthInfo(settings, endpoint)

	if isJSONOutput() {
		status := map[string]interface{}{
			"enabled":           hubEnabled,
			"enabledScope":      enabledScope.Scope,
			"enabledInherited":  enabledScope.Inherited,
			"cliOverride":       noHub,
			"endpoint":          endpoint,
			"endpointSource":    endpointScope.Source,
			"endpointInherited": endpointScope.Inherited,
			"configured":        settings.IsHubConfigured(),
			"projectId":         settings.ProjectID,
			"scionVersionLocal": version.Short(),
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

		// Try to connect and get health, then verify auth
		if endpoint != "" && !noHub {
			client, err := getHubClient(settings)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if health, err := client.Health(ctx); err == nil {
					status["connected"] = true
					status["hubVersion"] = health.Version
					status["hubStatus"] = health.Status
					status["scionVersionServer"] = health.ScionVersion

					// Verify auth against server
					jsonAuthVerified := false
					if authInfo.MethodType != "none" {
						meCtx, meCancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer meCancel()
						if meUser, meErr := client.Auth().Me(meCtx); meErr == nil {
							status["authVerified"] = true
							jsonAuthVerified = true
							status["authUser"] = map[string]string{
								"id":          meUser.ID,
								"email":       meUser.Email,
								"displayName": meUser.DisplayName,
								"role":        meUser.Role,
							}
						} else {
							status["authVerified"] = false
						}
					}

					// Add OAuth expiration if applicable
					if authInfo.HasOAuth && authInfo.OAuthCreds != nil && !authInfo.OAuthCreds.ExpiresAt.IsZero() {
						status["authExpires"] = authInfo.OAuthCreds.ExpiresAt.Format(time.RFC3339)
					}

					// Add project context to JSON output
					projectContext := getProjectContextJSON(client, resolvedPath, isGlobal, settings, jsonAuthVerified)
					status["projectContext"] = projectContext
				} else {
					status["connected"] = false
					status["error"] = err.Error()
				}
			}
		}

		return outputJSON(status)
	}

	// Determine scope label for display
	scopeLabel := "project"
	if isGlobal {
		scopeLabel = "global"
	}

	// Text output
	fmt.Println("Hub Integration Status")
	fmt.Println("======================")
	fmt.Printf("Scope:      %s\n", scopeLabel)
	fmt.Printf("Enabled:    %v\n", hubEnabled)
	if noHub {
		fmt.Printf("            (overridden by --no-hub flag)\n")
	}
	if enabledScope.Scope == "implicit" {
		fmt.Printf("            (implicit via credentials)\n")
	} else if enabledScope.Inherited {
		fmt.Printf("            (inherited from global settings)\n")
	}
	fmt.Printf("Endpoint:   %s\n", valueOrNone(endpoint))
	if endpointScope.Inherited {
		fmt.Printf("            (inherited from %s)\n", endpointScope.Source)
	} else if endpointScope.Source == "flag" {
		fmt.Printf("            (from --hub flag)\n")
	}
	fmt.Printf("Configured: %v\n", settings.IsHubConfigured())

	// Show project_id from top-level setting (where it's now stored)
	fmt.Printf("Project ID: %s\n", valueOrNone(settings.ProjectID))
	if settings.Hub != nil {
		fmt.Printf("Broker ID:  %s\n", valueOrNone(settings.Hub.BrokerID))
	}

	// Create hub client early so we can use it for auth verification and health checks
	var client hubclient.Client
	var health *hubclient.HealthResponse
	var clientErr error
	authVerified := false

	if endpoint != "" && !noHub {
		client, clientErr = getHubClient(settings)
		if clientErr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			health, _ = client.Health(ctx)
		}
	}

	// Authentication status section
	fmt.Println()
	fmt.Println("Authentication")
	fmt.Println("--------------")
	if authInfo.MethodType == "none" {
		fmt.Println("Status:     Not authenticated")
		fmt.Println("            Run 'scion hub auth login' to authenticate.")
	} else if client != nil {
		// Verify auth against the server by calling an authenticated endpoint
		meCtx, meCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer meCancel()
		meUser, meErr := client.Auth().Me(meCtx)
		if meErr != nil {
			fmt.Println("Status:     Not authenticated")
			fmt.Printf("Method:     %s (configured but not accepted by server)\n", authInfo.Method)
			fmt.Println("            Run 'scion hub auth login' to authenticate.")
		} else {
			authVerified = true
			fmt.Printf("Method:     %s\n", authInfo.Method)
			if authInfo.IsDevAuth {
				fmt.Println("            (development mode - not for production use)")
			}
			if meUser != nil {
				if meUser.DisplayName != "" || meUser.Email != "" {
					fmt.Printf("User:       %s (%s)\n", meUser.DisplayName, meUser.Email)
				}
				if meUser.Role != "" {
					fmt.Printf("Role:       %s\n", meUser.Role)
				}
			}
			if authInfo.HasOAuth && authInfo.OAuthCreds != nil && !authInfo.OAuthCreds.ExpiresAt.IsZero() {
				if time.Now().After(authInfo.OAuthCreds.ExpiresAt) {
					fmt.Printf("Expires:    %s (EXPIRED)\n", authInfo.OAuthCreds.ExpiresAt.Format(time.RFC3339))
				} else {
					fmt.Printf("Expires:    %s\n", authInfo.OAuthCreds.ExpiresAt.Format(time.RFC3339))
				}
			}
			if authInfo.TokenExpiry != nil {
				printTokenExpiry(*authInfo.TokenExpiry)
			}
		}
	} else {
		// Can't reach server to verify - show local auth info only
		fmt.Printf("Method:     %s (not verified - server unreachable)\n", authInfo.Method)
		if authInfo.IsDevAuth {
			fmt.Println("            (development mode - not for production use)")
		}
		if authInfo.TokenExpiry != nil {
			printTokenExpiry(*authInfo.TokenExpiry)
		}
	}

	// Hub Server section
	if endpoint != "" && !noHub {
		if clientErr != nil {
			fmt.Printf("\nConnection: failed (%s)\n", clientErr)
			return nil
		}

		fmt.Println()
		fmt.Println("Hub Server")
		fmt.Println("----------")
		if health == nil {
			fmt.Printf("Connection: failed\n")
		} else {
			fmt.Printf("Connection: ok\n")
			fmt.Printf("Hub Version: %s\n", health.Version)
			fmt.Printf("Hub Status:  %s\n", health.Status)
			fmt.Printf("Scion Version (Server): %s\n", valueOrNone(health.ScionVersion))
			fmt.Printf("Scion Version (Local):  %s\n", version.Short())

			// Show project context if we're in a project
			printProjectContext(client, resolvedPath, isGlobal, settings, authVerified)
		}
	}

	return nil
}

// printProjectContext prints information about the current project's registration and available brokers.
func printProjectContext(client hubclient.Client, projectPath string, isGlobal bool, settings *config.Settings, authVerified bool) {
	// Determine project name from path
	projectName := config.GetProjectName(projectPath)
	if isGlobal {
		projectName = "global"
	}

	fmt.Println()
	fmt.Println("Project Context")
	fmt.Println("---------------")
	fmt.Printf("Project:      %s\n", projectName)
	if isGlobal {
		fmt.Printf("Type:       user global\n")
	} else {
		fmt.Printf("Type:       project\n")
	}

	// If not authenticated, we can't query the Hub for project info
	if !authVerified {
		fmt.Printf("Linked:     unknown (not authenticated)\n")
		fmt.Println()
		fmt.Println("Authenticate with 'scion hub auth login' to view project status.")
		return
	}

	// Get git remote for this project (if not global)
	var gitRemote string
	if !isGlobal {
		gitRemote = util.GetGitRemoteDir(filepath.Dir(projectPath))
		if gitRemote != "" {
			fmt.Printf("Git Remote: %s\n", gitRemote)
		}
	}

	// If hub integration is disabled locally (e.g. after unlink), don't query the Hub
	if !settings.IsHubEnabled() {
		fmt.Printf("Linked: no (unlinked locally)\n")
		fmt.Println()
		fmt.Println("Run 'scion hub link' to re-link this project with the Hub.")
		return
	}

	// If project has not been explicitly linked via 'hub link', don't report as linked
	if !settings.IsHubLinked() {
		fmt.Printf("Linked: no\n")
		fmt.Println()
		fmt.Println("Run 'scion hub link' to link this project with the Hub.")
		return
	}

	// Check if project is linked to the Hub
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var linkedProject *hubclient.Project

	// First try hub.projectId (explicit link), then fall back to project_id
	hubProjectID:= settings.GetHubProjectID()
	if hubProjectID != "" {
		project, err := client.Projects().Get(ctx, hubProjectID)
		if err == nil {
			linkedProject = project
		}
	}
	if linkedProject == nil && settings.ProjectID != "" {
		project, err := client.Projects().Get(ctx, settings.ProjectID)
		if err == nil {
			linkedProject = project
		}
	}

	// If not found by ID and we have a git remote, try by git remote
	if linkedProject == nil && gitRemote != "" {
		resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			GitRemote: util.NormalizeGitRemote(gitRemote),
		})
		if err == nil && len(resp.Projects) > 0 {
			linkedProject = &resp.Projects[0]
		}
	}

	// If still not found and global, try by name
	if linkedProject == nil && isGlobal {
		resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			Name: "global",
		})
		if err == nil && len(resp.Projects) > 0 {
			linkedProject = &resp.Projects[0]
		}
	}

	if linkedProject == nil {
		fmt.Printf("Linked: no (project not found on Hub)\n")
		fmt.Println()
		fmt.Println("Run 'scion hub link' to re-link this project with the Hub.")
		return
	}

	fmt.Printf("Linked: yes\n")
	fmt.Printf("Hub Project:  %s (ID: %s)\n", linkedProject.Name, linkedProject.ID)

	// Get runtime brokers for this project
	brokersResp, err := client.RuntimeBrokers().List(ctx, &hubclient.ListBrokersOptions{
		ProjectID: linkedProject.ID,
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

// getProjectContextJSON returns project context information for JSON output.
func getProjectContextJSON(client hubclient.Client, projectPath string, isGlobal bool, settings *config.Settings, authVerified bool) map[string]interface{} {
	result := make(map[string]interface{})

	// Determine project name from path
	projectName := config.GetProjectName(projectPath)
	if isGlobal {
		projectName = "global"
	}

	result["name"] = projectName
	result["isGlobal"] = isGlobal
	if isGlobal {
		result["type"] = "user global"
	} else {
		result["type"] = "project"
	}

	// If not authenticated, we can't query the Hub for project info
	if !authVerified {
		result["linked"] = "unknown"
		result["reason"] = "not authenticated"
		return result
	}

	// Get git remote for this project (if not global)
	var gitRemote string
	if !isGlobal {
		gitRemote = util.GetGitRemoteDir(filepath.Dir(projectPath))
		if gitRemote != "" {
			result["gitRemote"] = gitRemote
		}
	}

	// If hub integration is disabled locally (e.g. after unlink), report as not linked
	if !settings.IsHubEnabled() {
		result["linked"] = false
		result["unlinkedLocally"] = true
		return result
	}

	// If project has not been explicitly linked via 'hub link', report as not linked
	if !settings.IsHubLinked() {
		result["linked"] = false
		return result
	}

	// Check if project is linked to the Hub
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var linkedProject *hubclient.Project

	// First try hub.projectId (explicit link), then fall back to project_id
	hubProjectID:= settings.GetHubProjectID()
	if hubProjectID != "" {
		project, err := client.Projects().Get(ctx, hubProjectID)
		if err == nil {
			linkedProject = project
		}
	}
	if linkedProject == nil && settings.ProjectID != "" {
		project, err := client.Projects().Get(ctx, settings.ProjectID)
		if err == nil {
			linkedProject = project
		}
	}

	// If not found by ID and we have a git remote, try by git remote
	if linkedProject == nil && gitRemote != "" {
		resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			GitRemote: util.NormalizeGitRemote(gitRemote),
		})
		if err == nil && len(resp.Projects) > 0 {
			linkedProject = &resp.Projects[0]
		}
	}

	// If still not found and global, try by name
	if linkedProject == nil && isGlobal {
		resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			Name: "global",
		})
		if err == nil && len(resp.Projects) > 0 {
			linkedProject = &resp.Projects[0]
		}
	}

	if linkedProject == nil {
		result["linked"] = false
		return result
	}

	result["linked"] = true
	result["hubProjectId"] = linkedProject.ID
	result["hubProjectName"] = linkedProject.Name

	// Get runtime brokers for this project
	brokersResp, err := client.RuntimeBrokers().List(ctx, &hubclient.ListBrokersOptions{
		ProjectID: linkedProject.ID,
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

func runHubProjects(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if hubOutputJSON {
		outputFormat = "json"
	}

	// If a project name is provided, show info for that project
	if len(args) == 1 {
		return runHubProjectsInfo(cmd, args)
	}

	// Resolve project path to find project settings
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

	resp, err := client.Projects().List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list projects: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(resp.Projects)
	}

	if len(resp.Projects) == 0 {
		fmt.Println("No projects found")
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
	for _, g := range resp.Projects {
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

func runHubProjectsInfo(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if hubOutputJSON {
		outputFormat = "json"
	}

	// Resolve project path to find project settings
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

	// Determine project name from args or current project
	var projectName string
	if len(args) > 0 {
		projectName = args[0]
	} else {
		// Use current project name
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

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find the project by name
	project, err := findProjectByName(ctx, client, projectName)
	if err != nil {
		return err
	}

	// Get providers for this project
	providersResp, err := client.Projects().ListProviders(ctx, project.ID)
	if err != nil {
		// Non-fatal: we can still show project info without providers
		util.Debugf("Failed to get providers: %v", err)
	}

	if isJSONOutput() {
		output := map[string]interface{}{
			"id":         project.ID,
			"name":       project.Name,
			"slug":       project.Slug,
			"gitRemote":  project.GitRemote,
			"visibility": project.Visibility,
			"agentCount": project.AgentCount,
			"created":    project.Created,
			"updated":    project.Updated,
			"createdBy":  project.CreatedBy,
			"ownerId":    project.OwnerID, // TODO: resolve to user display name when available
		}
		if project.DefaultRuntimeBrokerID != "" {
			output["defaultRuntimeBrokerId"] = project.DefaultRuntimeBrokerID
		}
		if len(project.Labels) > 0 {
			output["labels"] = project.Labels
		}
		if providersResp != nil && len(providersResp.Providers) > 0 {
			output["providers"] = providersResp.Providers
		}
		return outputJSON(output)
	}

	// Text output
	fmt.Println("Project Information")
	fmt.Println("===================")
	fmt.Printf("ID:          %s\n", project.ID)
	fmt.Printf("Name:        %s\n", project.Name)
	fmt.Printf("Slug:        %s\n", project.Slug)
	if project.GitRemote != "" {
		fmt.Printf("Git Remote:  %s\n", project.GitRemote)
	}
	fmt.Printf("Visibility:  %s\n", valueOrDefault(project.Visibility, "private"))
	fmt.Printf("Agents:      %d\n", project.AgentCount)
	fmt.Printf("Created:     %s\n", project.Created.Format(time.RFC3339))
	if !project.Updated.IsZero() && project.Updated != project.Created {
		fmt.Printf("Updated:     %s\n", project.Updated.Format(time.RFC3339))
	}
	// TODO: Resolve owner ID to display name when user lookup is available
	if project.OwnerID != "" {
		fmt.Printf("Owner:       %s (TODO: resolve to display name)\n", project.OwnerID)
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
			if p.BrokerID == project.DefaultRuntimeBrokerID {
				defaultIndicator = " (default)"
			}
			if p.LocalPath != "" {
				fmt.Printf("  - %s %s%s\n    Path: %s\n", p.BrokerName, statusIndicator, defaultIndicator, p.LocalPath)
			} else {
				fmt.Printf("  - %s %s%s\n", p.BrokerName, statusIndicator, defaultIndicator)
			}
		}
	} else {
		fmt.Println()
		fmt.Println("Broker Providers: none")
	}

	return nil
}

func runHubProjectsDelete(cmd *cobra.Command, args []string) error {
	// Resolve project path to find project settings
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

	// Determine project name from args or current project
	var projectName string
	if len(args) > 0 {
		projectName = args[0]
	} else {
		// Use current project name
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

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find the project by name
	project, err := findProjectByName(ctx, client, projectName)
	if err != nil {
		return err
	}

	// Get providers for display in confirmation
	providersResp, err := client.Projects().ListProviders(ctx, project.ID)
	if err != nil {
		util.Debugf("Failed to get providers: %v", err)
	}

	// Show confirmation prompt
	if !hubsync.ShowProjectDeletePrompt(project.Name, project.AgentCount, providersResp, autoConfirm) {
		return fmt.Errorf("deletion cancelled")
	}

	// Delete the project (always cascade-deletes all agents)
	if err := client.Projects().Delete(ctx, project.ID); err != nil {
		return fmt.Errorf("failed to delete project: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "hub projects delete",
			Message: fmt.Sprintf("Project '%s' deleted successfully.", project.Name),
			Details: map[string]interface{}{
				"projectId":    project.ID,
				"projectName":  project.Name,
				"agentCount": project.AgentCount,
			},
		})
	}

	fmt.Printf("Project '%s' deleted successfully.\n", project.Name)
	if project.AgentCount > 0 {
		fmt.Printf("Deleted %d agent(s).\n", project.AgentCount)
	}
	if providersResp != nil && len(providersResp.Providers) > 0 {
		fmt.Printf("Removed %d broker provider association(s).\n", len(providersResp.Providers))
	}

	return nil
}

func runHubProjectCreate(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if hubOutputJSON {
		outputFormat = "json"
	}

	gitURL := args[0]

	// Validate URL format
	if !util.IsGitURL(gitURL) {
		return fmt.Errorf("invalid git URL: %s\n\nAccepted formats:\n  https://github.com/org/repo.git\n  git@github.com:org/repo.git\n  ssh://git@github.com/org/repo", gitURL)
	}

	normalized := util.NormalizeGitRemote(gitURL)

	// Display name
	org, repo := util.ExtractOrgRepo(gitURL)
	displayName := hubProjectCreateName
	if displayName == "" {
		displayName = repo
	}

	// Derive slug: prefer explicit --slug, then --name, then org-repo
	slug := hubProjectCreateSlug
	if slug == "" {
		slugBase := displayName
		if hubProjectCreateName == "" {
			slugBase = org + "-" + repo
		}
		if hubProjectCreateBranch != "" {
			slugBase += "-" + hubProjectCreateBranch
		}
		slug = api.Slugify(slugBase)
	}

	// Detect default branch
	defaultBranch := hubProjectCreateBranch
	if defaultBranch == "" {
		cloneURL := util.ToHTTPSCloneURL(gitURL)
		defaultBranch = detectDefaultBranch(cloneURL)
		if defaultBranch == "" {
			defaultBranch = "main"
		}
	}

	// Load settings and get Hub client
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

	// Check for existing projects with the same git remote.
	existing, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
		GitRemote: normalized,
	})
	if err != nil {
		return fmt.Errorf("failed to check existing projects: %w", err)
	}

	if len(existing.Projects) > 0 {
		// Build matches for the prompt and compute next serial slug.
		matches := make([]hubsync.ProjectMatch, len(existing.Projects))
		for i, g := range existing.Projects {
			matches[i] = hubsync.ProjectMatch{
				ID:        g.ID,
				Name:      g.Name,
				Slug:      g.Slug,
				GitRemote: g.GitRemote,
			}
		}
		nextSlug := hubsync.NextSlugFromMatches(slug, matches)
		if nextSlug != "" {
			slug = nextSlug
		}

		if !isJSONOutput() {
			fmt.Printf("\nThis git remote already has %d project(s) on the Hub:\n\n", len(existing.Projects))
			for _, g := range existing.Projects {
				fmt.Printf("  - %s (slug: %s, ID: %s)\n", g.Name, g.Slug, g.ID)
			}
			fmt.Printf("\nA new project will be created as '%s' (slug: %s).\n", displayName, slug)

			if !autoConfirm {
				if nonInteractive {
					return fmt.Errorf("cannot create duplicate project in non-interactive mode without --yes")
				}
				if !hubsync.ConfirmAction("Continue?", true, autoConfirm) {
					fmt.Println("Cancelled.")
					return nil
				}
			}
		}
	}

	// Validate custom slug uniqueness. When --slug is provided explicitly,
	// check if it's already taken before sending to the server.
	if hubProjectCreateSlug != "" {
		slugCheck, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			Slug: hubProjectCreateSlug,
		})
		if err != nil {
			return fmt.Errorf("failed to validate slug: %w", err)
		}
		if len(slugCheck.Projects) > 0 {
			return fmt.Errorf("slug %q is already in use by project %q (ID: %s)", hubProjectCreateSlug, slugCheck.Projects[0].Name, slugCheck.Projects[0].ID)
		}
	}

	// Create project on the hub (server assigns ID)
	project, err := client.Projects().Create(ctx, &hubclient.CreateProjectRequest{
		Name:       displayName,
		Slug:       slug,
		GitRemote:  normalized,
		Visibility: hubProjectCreateVisibility,
		Labels: map[string]string{
			"scion.dev/default-branch": defaultBranch,
			"scion.dev/clone-url":      util.ToHTTPSCloneURL(gitURL),
			"scion.dev/source-url":     gitURL,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create project: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"id":        project.ID,
			"slug":      project.Slug,
			"name":      project.Name,
			"gitRemote": project.GitRemote,
			"branch":    defaultBranch,
		})
	}

	fmt.Printf("Project created:\n")
	fmt.Printf("  ID:     %s\n", project.ID)
	fmt.Printf("  Slug:   %s\n", project.Slug)
	fmt.Printf("  Remote: %s\n", project.GitRemote)
	fmt.Printf("  Branch: %s\n", defaultBranch)
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  1. Set git credentials:\n")
	fmt.Printf("     scion hub secret set GITHUB_TOKEN --project %s <your-pat>\n\n", project.Slug)
	fmt.Printf("  2. Start an agent:\n")
	fmt.Printf("     scion start my-agent --project %s \"your task\"\n", project.Slug)

	return nil
}

// detectDefaultBranch probes a git remote to detect its default branch.
// Returns the branch name or empty string on failure.
func detectDefaultBranch(cloneURL string) string {
	cmd := exec.Command("git", "ls-remote", "--symref", cloneURL, "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseDefaultBranch(string(output))
}

// parseDefaultBranch extracts the default branch name from `git ls-remote --symref` output.
// The expected format is: "ref: refs/heads/<branch>\tHEAD"
// Returns the branch name or empty string if not found.
func parseDefaultBranch(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") && strings.Contains(line, "\tHEAD") {
			branch := strings.TrimPrefix(line, "ref: refs/heads/")
			branch = strings.TrimSuffix(branch, "\tHEAD")
			return strings.TrimSpace(branch)
		}
	}
	return ""
}

// findProjectByName finds a project by name (case-insensitive) and returns it.
// Returns an error if not found or multiple matches are found.
func findProjectByName(ctx context.Context, client hubclient.Client, name string) (*hubclient.Project, error) {
	resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
		Name: name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search for project: %w", err)
	}

	if len(resp.Projects) == 0 {
		return nil, fmt.Errorf("project '%s' not found", name)
	}

	if len(resp.Projects) > 1 {
		fmt.Printf("Multiple projects found with name '%s':\n", name)
		for _, g := range resp.Projects {
			fmt.Printf("  - %s (ID: %s)\n", g.Name, g.ID)
		}
		return nil, fmt.Errorf("ambiguous project name - please use the project ID instead")
	}

	return &resp.Projects[0], nil
}

// valueOrDefault returns value if non-empty, otherwise returns the default.
func valueOrDefault(value, defaultVal string) string {
	if value == "" {
		return defaultVal
	}
	return value
}

func runHubBrokers(cmd *cobra.Command, args []string) error {
	// Bridge --json flag to global --format
	if hubOutputJSON {
		outputFormat = "json"
	}

	// Resolve project path to find project settings
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

	resp, err := client.RuntimeBrokers().List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to list brokers: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(resp.Brokers)
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
	// Bridge --json flag to global --format
	if hubOutputJSON {
		outputFormat = "json"
	}

	// Resolve project path to find project settings
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
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

	if isJSONOutput() {
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
		if len(broker.Projects) > 0 {
			output["projects"] = broker.Projects
		}
		if len(broker.Labels) > 0 {
			output["labels"] = broker.Labels
		}
		if len(broker.Annotations) > 0 {
			output["annotations"] = broker.Annotations
		}
		return outputJSON(output)
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

	// Show projects
	if len(broker.Projects) > 0 {
		fmt.Println()
		fmt.Println("Projects")
		fmt.Println("--------")
		for _, g := range broker.Projects {
			fmt.Printf("  - %s (%d agents)\n", g.ProjectName, g.AgentCount)
		}
	} else {
		fmt.Println()
		fmt.Println("Projects: none")
	}

	return nil
}

func runHubBrokersDelete(cmd *cobra.Command, args []string) error {
	// Broker name is required for delete
	if len(args) == 0 {
		return fmt.Errorf("broker name or ID is required.\n\nUsage: scion hub brokers delete <broker-name>")
	}

	brokerNameOrID := args[0]

	// Resolve project path to find project settings
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

	// Find the broker by name or ID
	broker, err := resolveBrokerByNameOrID(ctx, client, brokerNameOrID)
	if err != nil {
		return err
	}

	// Extract project names for the confirmation prompt
	projectNames := make([]string, len(broker.Projects))
	for i, g := range broker.Projects {
		projectNames[i] = g.ProjectName
	}

	// Show confirmation prompt
	if !hubsync.ShowBrokerDeletePrompt(broker.Name, projectNames, autoConfirm) {
		return fmt.Errorf("deletion cancelled")
	}

	// Delete the broker
	if err := client.RuntimeBrokers().Delete(ctx, broker.ID); err != nil {
		return fmt.Errorf("failed to delete broker: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "hub brokers delete",
			Message: fmt.Sprintf("Broker '%s' deleted successfully.", broker.Name),
			Details: map[string]interface{}{
				"brokerId":        broker.ID,
				"brokerName":      broker.Name,
				"projectsRemoved": len(broker.Projects),
			},
		})
	}

	fmt.Printf("Broker '%s' deleted successfully.\n", broker.Name)
	if len(broker.Projects) > 0 {
		fmt.Printf("Removed from %d project(s).\n", len(broker.Projects))
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
	// Resolve project path
	resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
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

	scopeLabel := "global"
	if !isGlobal {
		scopeLabel = "project"
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "hub enable",
			Message: fmt.Sprintf("Hub integration enabled at %s scope.", scopeLabel),
			Details: map[string]interface{}{
				"endpoint":   endpoint,
				"hubStatus":  health.Status,
				"hubVersion": health.Version,
				"scope":      scopeLabel,
			},
		})
	}

	fmt.Printf("Hub integration enabled (%s scope).\n", scopeLabel)
	fmt.Printf("Endpoint: %s\n", endpoint)
	fmt.Printf("Hub Status: %s (version %s)\n", health.Status, health.Version)
	fmt.Println("\nAgent operations (create, start, delete) will now be routed through the Hub.")
	fmt.Println("Use 'scion hub disable' to switch back to local-only mode.")

	return nil
}

func runHubDisable(cmd *cobra.Command, args []string) error {
	// Resolve project path
	resolvedPath, isGlobal, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	scopeLabel := "global"
	if !isGlobal {
		scopeLabel = "project"
	}

	enabledScope := getHubEnabledScope(resolvedPath, isGlobal, settings)

	if !settings.IsHubEnabled() {
		msg := "Hub integration is already disabled."
		if enabledScope.Scope == "default" && !isGlobal {
			msg = "Hub integration is not enabled at the project scope (and no global setting found)."
		}
		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "hub disable",
				Message: msg,
				Details: map[string]interface{}{
					"scope": scopeLabel,
				},
			})
		}
		fmt.Println(msg)
		return nil
	}

	// Warn if hub is enabled globally but user is disabling at project scope
	if enabledScope.Inherited {
		// The setting is inherited from global — disabling at project scope
		// will write an explicit hub.enabled=false at the project level
		if isJSONOutput() {
			// Continue to disable at project scope
		} else {
			fmt.Printf("Note: Hub is currently enabled via global settings.\n")
			fmt.Printf("      This will disable it for this project only.\n\n")
		}
	}

	// Save the disabled setting
	if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
		return fmt.Errorf("failed to save setting: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "hub disable",
			Message: fmt.Sprintf("Hub integration disabled at %s scope.", scopeLabel),
			Details: map[string]interface{}{
				"scope":            scopeLabel,
				"wasInheritedFrom": enabledScope.Scope,
			},
		})
	}

	fmt.Printf("Hub integration disabled (%s scope).\n", scopeLabel)
	fmt.Println("Agent operations will now be performed locally.")
	fmt.Println("\nHub configuration is preserved. Use 'scion hub enable' to re-enable.")

	return nil
}

func runHubLink(cmd *cobra.Command, args []string) error {
	// Resolve project path
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

	// Show confirmation prompt
	if !hubsync.ShowProjectLinkPrompt(projectName, endpoint, autoConfirm) {
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

	// Ensure project_id exists
	projectID := settings.ProjectID
	if projectID == "" {
		projectID = config.GenerateProjectIDForDir(filepath.Dir(resolvedPath))
		if err := config.UpdateSetting(resolvedPath, "project_id", projectID, isGlobal); err != nil {
			return fmt.Errorf("failed to save project_id: %w", err)
		}
	}

	// Check if project already exists on Hub.
	// Read this project's own settings in isolation (without global merge) to
	// determine whether hub.projectId was explicitly set for THIS project, rather
	// than inherited from global settings via the koanf merge chain.
	localSettings, _ := config.LoadSettingsFromDir(resolvedPath)
	localHubProjectID:= ""
	if localSettings != nil {
		localHubProjectID = localSettings.GetHubProjectID()
	}
	hubLookupID := localHubProjectID
	if hubLookupID == "" {
		hubLookupID = projectID
	}
	hubProject, err := getLinkedProject(ctx, client, hubLookupID)
	if err != nil {
		util.Debugf("Error checking project link status: %v", err)
	}

	if hubProject != nil && hubProject.Name == projectName {
		// Already linked — still call register so the server can backfill
		// the membership group if it was created before group support.
		if _, err := registerProjectOnHub(ctx, client, hubLookupID, projectName, resolvedPath, isGlobal); err != nil {
			util.Debugf("Failed to register during re-link (non-fatal): %v", err)
		}
		fmt.Printf("Project '%s' is already linked to the Hub (ID: %s)\n", projectName, projectID)
	} else {
		if hubProject != nil && localHubProjectID != "" {
			// This project's own hub.projectId points to a different project on the
			// Hub — stale link. In V1 settings, hub.projectId and project_id share
			// a single field, so a previous link may have overwritten the local
			// project_id with the stale hub project ID. Regenerate from the marker
			// file or directory to get the true local identity before
			// re-registering.
			fmt.Printf("Warning: local project '%s' was linked to hub project '%s' (ID: %s). Re-linking.\n",
				projectName, hubProject.Name, hubLookupID)

			// Clear the stale hub project ID
			if err := config.UpdateSetting(resolvedPath, "hub.projectId", "", isGlobal); err != nil {
				util.Debugf("Failed to clear stale hub.projectId: %v", err)
			}

			// Regenerate the local project ID from the marker file or directory
			if markerID, err := config.ReadProjectID(resolvedPath); err == nil && markerID != "" {
				projectID = markerID
			} else {
				projectID = config.GenerateProjectIDForDir(filepath.Dir(resolvedPath))
			}
			if err := config.UpdateSetting(resolvedPath, "project_id", projectID, isGlobal); err != nil {
				return fmt.Errorf("failed to save project_id: %w", err)
			}
		} else if hubProject != nil {
			// The lookup ID (either from a deterministic project_id collision or
			// an inherited global hub.projectId) matched a different project on the
			// Hub. This is not a genuine link for THIS project — ignore the match
			// and proceed to register or link by name.
			util.Debugf("Project ID %s matched hub project '%s' but is not an explicit link for local project '%s'; ignoring",
				hubLookupID, hubProject.Name, projectName)
			hubProject = nil
		}
		// Check for existing projects with the same name
		resp, err := client.Projects().List(ctx, &hubclient.ListProjectsOptions{
			Name: projectName,
		})
		if err != nil {
			return fmt.Errorf("failed to search for matching projects: %w", err)
		}

		if len(resp.Projects) > 0 {
			// Found matching projects - ask user what to do
			matches := make([]hubsync.ProjectMatch, len(resp.Projects))
			for i, g := range resp.Projects {
				matches[i] = hubsync.ProjectMatch{
					ID:        g.ID,
					Name:      g.Name,
					Slug:      g.Slug,
					GitRemote: g.GitRemote,
				}
			}

			baseSlug := api.Slugify(projectName)
			nextSlug := hubsync.NextSlugFromMatches(baseSlug, matches)
			choice, selectedID := hubsync.ShowMatchingProjectsPrompt(projectName, matches, nextSlug, autoConfirm)
			switch choice {
			case hubsync.ProjectChoiceCancel:
				return fmt.Errorf("linking cancelled")
			case hubsync.ProjectChoiceLink:
				// Register with the selected project's ID so the hub creates
				// the membership group (and adds this user as owner) if it
				// doesn't already exist.
				if _, err := registerProjectOnHub(ctx, client, selectedID, projectName, resolvedPath, isGlobal); err != nil {
					util.Debugf("Failed to register during link (non-fatal): %v", err)
				}
				// Store the hub project ID separately — don't overwrite the
				// deterministic local project_id (which drives config-dir paths).
				if err := config.UpdateSetting(resolvedPath, "hub.projectId", selectedID, isGlobal); err != nil {
					return fmt.Errorf("failed to save hub project ID: %w", err)
				}
				hubLookupID = selectedID
				fmt.Printf("Linked to existing project (ID: %s)\n", selectedID)
			case hubsync.ProjectChoiceRegisterNew:
				// Register as a new project on the Hub using the local project_id.
				hubProjectID, err := registerProjectOnHub(ctx, client, projectID, projectName, resolvedPath, isGlobal)
				if err != nil {
					return err
				}
				// Store the hub project ID if it differs from the local one
				if hubProjectID != "" && hubProjectID != projectID {
					if err := config.UpdateSetting(resolvedPath, "hub.projectId", hubProjectID, isGlobal); err != nil {
						return fmt.Errorf("failed to save hub project ID: %w", err)
					}
				}
				hubLookupID = hubProjectID
			}
		} else {
			// No matching projects - create new one
			hubProjectID, err := registerProjectOnHub(ctx, client, projectID, projectName, resolvedPath, isGlobal)
			if err != nil {
				return err
			}
			// Store the hub project ID if it differs from the local one
			if hubProjectID != "" && hubProjectID != projectID {
				if err := config.UpdateSetting(resolvedPath, "hub.projectId", hubProjectID, isGlobal); err != nil {
					return fmt.Errorf("failed to save hub project ID: %w", err)
				}
			}
			hubLookupID = hubProjectID
		}
	}

	// Use the hub project ID for all hub API calls from here on
	effectiveHubProjectID:= hubLookupID
	if effectiveHubProjectID == "" {
		effectiveHubProjectID = projectID
	}

	// If this host is a registered broker, add it as a provider for this project
	localBrokerID, localBrokerName := getLocalBrokerInfo(settings)
	if localBrokerID != "" {
		addReq := &hubclient.AddProviderRequest{
			BrokerID:  localBrokerID,
			LocalPath: resolvedPath,
		}
		if _, err := client.Projects().AddProvider(ctx, effectiveHubProjectID, addReq); err != nil {
			util.Debugf("Failed to add broker as provider during link: %v", err)
		} else {
			util.Debugf("Registered local broker %s as provider for project %s", localBrokerName, effectiveHubProjectID)
		}
	}

	// Enable Hub integration and mark as linked for this project
	if err := config.UpdateSetting(resolvedPath, "hub.enabled", "true", isGlobal); err != nil {
		return fmt.Errorf("failed to enable hub: %w", err)
	}
	if err := config.UpdateSetting(resolvedPath, "hub.linked", "true", isGlobal); err != nil {
		return fmt.Errorf("failed to save linked state: %w", err)
	}

	// Save endpoint if provided via flag
	if hubEndpoint != "" {
		if err := config.UpdateSetting(resolvedPath, "hub.endpoint", hubEndpoint, isGlobal); err != nil {
			return fmt.Errorf("failed to save endpoint: %w", err)
		}
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "hub link",
			Message: fmt.Sprintf("Project '%s' is now linked to the Hub.", projectName),
			Details: map[string]interface{}{
				"project":      projectName,
				"projectId":    projectID,
				"hubProjectId": effectiveHubProjectID,
				"endpoint":   endpoint,
			},
		})
	}

	fmt.Println()
	fmt.Printf("Project '%s' is now linked to the Hub.\n", projectName)

	// Offer to sync agents
	if hubsync.ShowSyncAfterLinkPrompt(autoConfirm) {
		// Create HubContext for sync
		hubCtx := &hubsync.HubContext{
			Client:    client,
			Endpoint:  endpoint,
			Settings:  settings,
			ProjectID:   effectiveHubProjectID,
			ProjectPath: resolvedPath,
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

	// Offer to sync project templates to Hub
	offerTemplateSyncOnLink(resolvedPath, endpoint, projectID)

	// Display available brokers for this project
	listBrokersForProject(ctx, client, projectID)

	return nil
}

// offerTemplateSyncOnLink detects local project templates and prompts
// the user to sync them to the Hub during project linking.
func offerTemplateSyncOnLink(projectPath, endpoint, projectID string) {
	// List project-scoped templates
	_, projectTemplates, err := config.ListTemplatesGrouped()
	if err != nil || len(projectTemplates) == 0 {
		return
	}

	if !util.IsTerminal() {
		fmt.Printf("\nSkipping template sync (non-interactive mode).\n")
		fmt.Println("Run 'scion templates sync --all' to upload project templates.")
		return
	}

	// Show discovered templates
	fmt.Printf("\nFound %d project template(s) not yet synced to Hub:\n", len(projectTemplates))
	for _, t := range projectTemplates {
		fmt.Printf("  - %s\n", t.Name)
	}

	if !hubsync.ConfirmAction("Sync these templates to the Hub?", true, autoConfirm) {
		fmt.Println("Skipping template sync.")
		fmt.Println("Run 'scion templates sync --all' to upload project templates later.")
		return
	}

	// Create a HubContext for syncing
	settings, err := config.LoadSettings(projectPath)
	if err != nil {
		fmt.Printf("Warning: failed to load settings for template sync: %v\n", err)
		return
	}

	client, err := getHubClient(settings)
	if err != nil {
		fmt.Printf("Warning: failed to create Hub client for template sync: %v\n", err)
		return
	}

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  endpoint,
		ProjectPath: projectPath,
		Settings:  settings,
	}

	fmt.Println("\nSyncing project templates to Hub...")
	var synced int
	for _, tpl := range projectTemplates {
		harnessType, err := detectHarnessType(tpl)
		if err != nil {
			fmt.Printf("  %s: skipped (failed to detect harness: %v)\n", tpl.Name, err)
			continue
		}

		// Use force=false — don't overwrite existing Hub templates
		err = syncTemplateToHub(hubCtx, tpl.Name, tpl.Path, "project", harnessType)
		if err != nil {
			fmt.Printf("  %s: failed: %v\n", tpl.Name, err)
			continue
		}
		synced++
	}
	fmt.Printf("%d template(s) synced to project scope.\n", synced)
}

// registerProjectOnHub registers a new project on the Hub.
func registerProjectOnHub(ctx context.Context, client hubclient.Client, projectID, projectName, projectPath string, isGlobal bool) (string, error) {
	var gitRemote string
	if !isGlobal {
		gitRemote = util.GetGitRemote()
	}

	req := &hubclient.RegisterProjectRequest{
		ID:        projectID,
		Name:      projectName,
		GitRemote: util.NormalizeGitRemote(gitRemote),
		Path:      projectPath,
	}

	resp, err := client.Projects().Register(ctx, req)
	if err != nil {
		return "", fmt.Errorf("failed to register project: %w", err)
	}

	if resp.Created {
		fmt.Printf("Created new project: %s (ID: %s)\n", resp.Project.Name, resp.Project.ID)
	} else {
		fmt.Printf("Linked to existing project: %s (ID: %s)\n", resp.Project.Name, resp.Project.ID)
	}

	return resp.Project.ID, nil
}

func runHubUnlink(cmd *cobra.Command, args []string) error {
	// Resolve project path
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

	// Check if project is currently linked
	if !settings.IsHubEnabled() {
		fmt.Println("This project is not linked to the Hub.")
		return nil
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

	// Show confirmation prompt
	if !hubsync.ShowProjectUnlinkPrompt(projectName, autoConfirm) {
		return fmt.Errorf("unlinking cancelled")
	}

	// Disable Hub integration, clear linked state and hub project ID
	if err := config.UpdateSetting(resolvedPath, "hub.enabled", "false", isGlobal); err != nil {
		return fmt.Errorf("failed to disable hub: %w", err)
	}
	if err := config.UpdateSetting(resolvedPath, "hub.linked", "false", isGlobal); err != nil {
		util.Debugf("Failed to clear hub.linked: %v", err)
	}
	if err := config.UpdateSetting(resolvedPath, "hub.projectId", "", isGlobal); err != nil {
		util.Debugf("Failed to clear hub.projectId: %v", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "hub unlink",
			Message: fmt.Sprintf("Project '%s' has been unlinked from the Hub.", projectName),
			Details: map[string]interface{}{
				"project": projectName,
			},
		})
	}

	fmt.Println()
	fmt.Printf("Project '%s' has been unlinked from the Hub.\n", projectName)
	fmt.Println("The project and its agents remain on the Hub for other brokers.")
	fmt.Println("Use 'scion hub link' to re-link this local project to the hub's.")
	fmt.Printf("Use \"scion hub projects delete '%s'\" to remove project from hub entirely.", projectName)

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

// isProjectLinked checks if the project exists on the Hub.
func isProjectLinked(ctx context.Context, client hubclient.Client, projectID string) (bool, error) {
	project, err := getLinkedProject(ctx, client, projectID)
	return project != nil, err
}

// getLinkedProject returns the hub project for the given ID, or nil if not found.
func getLinkedProject(ctx context.Context, client hubclient.Client, projectID string) (*hubclient.Project, error) {
	if projectID == "" {
		return nil, nil
	}

	project, err := client.Projects().Get(ctx, projectID)
	if err != nil {
		errStr := err.Error()
		if containsIgnoreCase(errStr, "404") || containsIgnoreCase(errStr, "not found") {
			return nil, nil
		}
		return nil, err
	}

	return project, nil
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

// listBrokersForProject fetches and displays available runtime brokers for a project.
func listBrokersForProject(ctx context.Context, client hubclient.Client, projectID string) {
	resp, err := client.RuntimeBrokers().List(ctx, &hubclient.ListBrokersOptions{
		ProjectID: projectID,
	})
	if err != nil {
		util.Debugf("Failed to list brokers for project: %v", err)
		return
	}

	if len(resp.Brokers) == 0 {
		fmt.Println()
		fmt.Println("Warning: This project has no active runtime brokers.")
		fmt.Println("Register one with 'scion broker register'")
		return
	}

	fmt.Println()
	fmt.Println("Runtime brokers available for this project:")
	for _, b := range resp.Brokers {
		status := b.Status
		if status == "" {
			status = "unknown"
		}
		fmt.Printf("  - %s (%s)\n", b.Name, status)
	}
}
