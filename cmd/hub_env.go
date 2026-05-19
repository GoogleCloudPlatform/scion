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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// scopeInferSentinel is used as NoOptDefVal for --project and --broker flags,
// allowing bare flag usage (e.g. --project) to infer scope from settings.
const scopeInferSentinel = "\x00"

var (
	envProjectScope string
	envBrokerScope  string
	envScope        string
	envOutputJSON   bool
	envAlways       bool
	envAsNeeded     bool
	envSecret       bool
)

// hubEnvCmd is the parent command for environment variable operations
var hubEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage environment variables",
	Long: `Manage environment variables stored in the Hub.

Environment variables can be scoped to:
  - Hub: Available to all agents across the entire hub (admin-only writes)
  - User (default): Available to all your agents
  - Project: Available to agents in a specific project
  - Broker: Available to agents running on a specific broker

Variables are resolved hierarchically when an agent starts:
  hub -> user -> project -> broker -> agent config

Examples:
  # Set a user-scoped variable (two formats)
  scion hub env set API_URL=https://api.example.com
  scion hub env set API_URL https://api.example.com

  # Set a project-scoped variable (infer project from current directory)
  scion hub env set --project API_URL=https://api.example.com

  # Set a project-scoped variable for a specific project (by name, slug, or ID)
  scion hub env set --project=my-project API_URL=https://api.example.com

  # List all user variables
  scion hub env get

  # Get a specific variable
  scion hub env get API_URL

  # Delete a variable
  scion hub env clear API_URL`,
}

// hubEnvSetCmd sets an environment variable
var hubEnvSetCmd = &cobra.Command{
	Use:   "set KEY=VALUE | KEY VALUE",
	Short: "Set an environment variable",
	Long: `Set an environment variable in the Hub.

By default, variables are scoped to the current user. Use --project or --broker
to set variables at different scopes.

The value can be provided as a single argument in KEY=VALUE format, or as
two separate arguments.

Examples:
  scion hub env set API_URL=https://api.example.com
  scion hub env set API_URL https://api.example.com
  scion hub env set --project LOG_LEVEL=debug
  scion hub env set --host DATABASE_HOST localhost`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runEnvSet,
}

// hubEnvGetCmd gets environment variables
var hubEnvGetCmd = &cobra.Command{
	Use:   "get [KEY]",
	Short: "Get environment variables",
	Long: `Get environment variables from the Hub.

Without a key, lists all variables for the scope.
With a key, returns the specific variable.

Examples:
  scion hub env get                    # List all user variables
  scion hub env get API_URL            # Get specific variable
  scion hub env get --project          # List project variables
  scion hub env get --project API_URL  # Get project variable`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnvGet,
}

// hubEnvListCmd lists environment variables
var hubEnvListCmd = &cobra.Command{
	Use:   "list",
	Short: "List environment variables",
	Long: `List all environment variables for a scope from the Hub.

By default, lists user-scoped variables. Use --project or --broker
to list variables at different scopes.

Examples:
  scion hub env list                      # List all user variables
  scion hub env list --project            # List current project variables
  scion hub env list --project=my-project # List variables for a specific project
  scion hub env list --json               # Output as JSON`,
	Args: cobra.NoArgs,
	RunE: runEnvList,
}

// hubEnvClearCmd clears an environment variable
var hubEnvClearCmd = &cobra.Command{
	Use:   "clear KEY",
	Short: "Clear an environment variable",
	Long: `Remove an environment variable from the Hub.

Examples:
  scion hub env clear API_URL
  scion hub env clear --project API_URL
  scion hub env clear --broker API_URL`,
	Args: cobra.ExactArgs(1),
	RunE: runEnvClear,
}

func init() {
	hubCmd.AddCommand(hubEnvCmd)
	hubEnvCmd.AddCommand(hubEnvSetCmd)
	hubEnvCmd.AddCommand(hubEnvGetCmd)
	hubEnvCmd.AddCommand(hubEnvListCmd)
	hubEnvCmd.AddCommand(hubEnvClearCmd)

	// Add scope flags to all subcommands.
	// --scope selects the scope level (hub, user). --project/--broker select their
	// respective scopes and support both bare usage (infer from settings) and
	// explicit name/ID via --project=<name|id>.
	for _, cmd := range []*cobra.Command{hubEnvSetCmd, hubEnvGetCmd, hubEnvListCmd, hubEnvClearCmd} {
		cmd.Flags().StringVar(&envScope, "scope", "", "Scope level: hub, user (default: user)")
		cmd.Flags().StringVar(&envProjectScope, "project", "", "Project scope (bare flag infers current project, or use --project=<name|id>)")
		cmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel

		cmd.Flags().StringVar(&envProjectScope, "grove", "", "Deprecated alias for --project")
		cmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
		_ = cmd.Flags().MarkDeprecated("grove", "use --project instead")
		_ = cmd.Flags().MarkHidden("grove")

		cmd.Flags().StringVar(&envBrokerScope, "broker", "", "Broker scope (bare flag infers current broker, or use --broker=<name|id>)")
		cmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel
	}

	hubEnvGetCmd.Flags().BoolVar(&envOutputJSON, "json", false, "Output in JSON format")
	hubEnvListCmd.Flags().BoolVar(&envOutputJSON, "json", false, "Output in JSON format")

	// Injection mode and secret flags for set command
	hubEnvSetCmd.Flags().BoolVar(&envAlways, "always", false, "Always inject this variable at its scope")
	hubEnvSetCmd.Flags().BoolVar(&envAsNeeded, "as-needed", false, "Only inject when requested by a template (default)")
	hubEnvSetCmd.Flags().BoolVar(&envSecret, "secret", false, "Treat as a secret (encrypted, value never returned)")
}

// resolveEnvScope determines the scope and scopeID based on flags.
// When --grove or --broker is used bare (no value), scopeID is inferred from settings.
// When a value is provided, it is returned as-is and may need further resolution
// (name/slug to UUID) via resolveScopeID.
func resolveEnvScope(cmd *cobra.Command, settings *config.Settings) (scope, scopeID string, err error) {
	scopeSet := cmd.Flags().Changed("scope")
	projectSet := cmd.Flags().Changed("project")
	groveSet := cmd.Flags().Changed("grove")
	brokerSet := cmd.Flags().Changed("broker")

	// Enforce mutual exclusivity
	setCount := 0
	if scopeSet {
		setCount++
	}
	if projectSet || groveSet {
		setCount++
	}
	if brokerSet {
		setCount++
	}
	if setCount > 1 {
		return "", "", fmt.Errorf("cannot specify more than one of --scope, --project, and --broker")
	}

	if scopeSet {
		switch envScope {
		case "hub":
			return "hub", "", nil
		case "user", "":
			return "user", "", nil
		default:
			return "", "", fmt.Errorf("invalid --scope value %q: must be 'hub' or 'user'", envScope)
		}
	}

	if projectSet || groveSet {
		scope = "project"
		projectVal := envProjectScope
		if projectVal == scopeInferSentinel {
			projectVal = ""
		}
		if projectVal != "" {
			// Explicit value — may be a name, slug, or UUID (resolved later)
			scopeID = projectVal
		} else {
			// Infer from settings
			if settings.Hub != nil && settings.Hub.ProjectID != "" {
				scopeID = settings.Hub.ProjectID
			} else {
				return "", "", fmt.Errorf("cannot infer project ID: not linked with Hub. Use 'scion hub link' first or provide explicit project ID")
			}
		}
		return scope, scopeID, nil
	}

	if brokerSet {
		scope = "runtime_broker"
		brokerVal := envBrokerScope
		if brokerVal == scopeInferSentinel {
			brokerVal = ""
		}
		if brokerVal != "" {
			// Explicit value — may be a name or UUID (resolved later)
			scopeID = brokerVal
		} else {
			// Infer from settings
			if settings.Hub != nil && settings.Hub.BrokerID != "" {
				scopeID = settings.Hub.BrokerID
			} else {
				return "", "", fmt.Errorf("cannot infer broker ID: not linked with Hub. Use 'scion hub link' first or provide explicit broker ID")
			}
		}
		return scope, scopeID, nil
	}

	// Default to user scope
	return "user", "", nil
}

// resolveScopeID resolves a scope ID that may be a human-friendly name or slug
// into a UUID by querying the hub API. If the scopeID is already a valid UUID
// or is empty, it is returned unchanged.
func resolveScopeID(ctx context.Context, client hubclient.Client, scope, scopeID string) (string, error) {
	if scopeID == "" {
		return scopeID, nil
	}
	// Already a UUID — no resolution needed
	if _, err := uuid.Parse(scopeID); err == nil {
		return scopeID, nil
	}
	switch scope {
	case "project":
		project, err := resolveProjectByNameOrID(ctx, client, scopeID)
		if err != nil {
			return "", err
		}
		return project.ID, nil
	case "runtime_broker":
		broker, err := resolveBrokerByNameOrID(ctx, client, scopeID)
		if err != nil {
			return "", err
		}
		return broker.ID, nil
	}
	return scopeID, nil
}

func runEnvSet(cmd *cobra.Command, args []string) error {
	var key, value string

	if len(args) == 1 {
		// Single argument: expect KEY=VALUE format
		parts := strings.SplitN(args[0], "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid format: expected KEY=VALUE or KEY VALUE")
		}
		key = parts[0]
		value = parts[1]
	} else {
		// Two arguments: KEY VALUE
		key = args[0]
		value = args[1]
	}

	// Validate key
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}
	if strings.ContainsAny(key, "= \t\n") {
		return fmt.Errorf("key cannot contain spaces, tabs, newlines, or '='")
	}

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

	scope, scopeID, err := resolveEnvScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

	// Validate --always and --as-needed are mutually exclusive
	if envAlways && envAsNeeded {
		return fmt.Errorf("--always and --as-needed are mutually exclusive")
	}

	// Determine injection mode
	injectionMode := ""
	if envAlways {
		injectionMode = "always"
	} else if envAsNeeded {
		injectionMode = "as_needed"
	}
	// If neither is set, leave empty to let the server default to "as_needed"

	// When --secret flag is set, redirect to the Secret API instead of Env API
	if envSecret {
		secretReq := &hubclient.SetSecretRequest{
			Value:         value,
			Scope:         scope,
			ScopeID:       scopeID,
			InjectionMode: injectionMode,
			Type:          "environment",
			Target:        key,
		}

		secretResp, err := client.Secrets().Set(ctx, key, secretReq)
		if err != nil {
			return fmt.Errorf("failed to set secret: %w", err)
		}

		action := "Updated"
		if secretResp.Created {
			action = "Created"
		}

		// Build annotation string
		annotations := " (secret)"
		if secretResp.Secret != nil {
			if secretResp.Secret.InjectionMode == "always" {
				annotations += " (always)"
			} else {
				annotations += " (as-needed)"
			}
		}

		fmt.Printf("%s %s=******** (scope: %s)%s\n", action, key, scope, annotations)
		return nil
	}

	req := &hubclient.SetEnvRequest{
		Value:         value,
		Scope:         scope,
		ScopeID:       scopeID,
		InjectionMode: injectionMode,
		Secret:        envSecret,
	}

	resp, err := client.Env().Set(ctx, key, req)
	if err != nil {
		return fmt.Errorf("failed to set environment variable: %w", err)
	}

	displayValue := value
	if resp.EnvVar != nil && resp.EnvVar.Sensitive {
		displayValue = "********"
	}

	action := "Updated"
	if resp.Created {
		action = "Created"
	}

	// Build annotation string
	annotations := ""
	if resp.EnvVar != nil {
		if resp.EnvVar.InjectionMode == "always" {
			annotations += " (always)"
		} else {
			annotations += " (as-needed)"
		}
	}

	fmt.Printf("%s %s=%s (scope: %s)%s\n", action, key, displayValue, scope, annotations)

	return nil
}

func runEnvGet(cmd *cobra.Command, args []string) error {
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

	scope, scopeID, err := resolveEnvScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

	// If key is provided, get specific variable
	if len(args) == 1 {
		key := args[0]
		opts := &hubclient.EnvScopeOptions{
			Scope:   scope,
			ScopeID: scopeID,
		}

		envVar, err := client.Env().Get(ctx, key, opts)
		if err != nil {
			return fmt.Errorf("failed to get environment variable: %w", err)
		}

		if envOutputJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(envVar)
		}

		if envVar.Sensitive {
			fmt.Printf("%s=****** (sensitive, scope: %s)%s\n", envVar.Key, envVar.Scope, formatEnvAnnotations(envVar))
		} else {
			fmt.Printf("%s=%s%s\n", envVar.Key, envVar.Value, formatEnvAnnotations(envVar))
		}
		return nil
	}

	// No key provided, delegate to list
	return runEnvList(cmd, nil)
}

func runEnvList(cmd *cobra.Command, _ []string) error {
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

	scope, scopeID, err := resolveEnvScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

	opts := &hubclient.ListEnvOptions{
		Scope:   scope,
		ScopeID: scopeID,
	}

	resp, err := client.Env().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list environment variables: %w", err)
	}

	if envOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if len(resp.EnvVars) == 0 {
		fmt.Printf("No environment variables found (scope: %s)\n", scope)
		return nil
	}

	fmt.Printf("Environment variables (scope: %s):\n", scope)
	for _, v := range resp.EnvVars {
		if v.Sensitive {
			fmt.Printf("  %s=****** (sensitive)%s\n", v.Key, formatEnvAnnotations(&v))
		} else {
			fmt.Printf("  %s=%s%s\n", v.Key, v.Value, formatEnvAnnotations(&v))
		}
	}

	return nil
}

// formatEnvAnnotations builds an annotation string for injection mode and secret status.
func formatEnvAnnotations(v *hubclient.EnvVar) string {
	var parts []string
	if v.InjectionMode == "always" {
		parts = append(parts, "always")
	} else if v.InjectionMode == "as_needed" {
		parts = append(parts, "as-needed")
	}
	if v.Secret {
		parts = append(parts, "secret")
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func runEnvClear(cmd *cobra.Command, args []string) error {
	key := args[0]

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

	scope, scopeID, err := resolveEnvScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

	opts := &hubclient.EnvScopeOptions{
		Scope:   scope,
		ScopeID: scopeID,
	}

	if err := client.Env().Delete(ctx, key, opts); err != nil {
		return fmt.Errorf("failed to delete environment variable: %w", err)
	}

	fmt.Printf("Deleted %s (scope: %s)\n", key, scope)
	return nil
}
