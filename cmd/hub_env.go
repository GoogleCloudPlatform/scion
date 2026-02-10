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

	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	envGroveScope string
	envBrokerScope  string
	envOutputJSON bool
)

// hubEnvCmd is the parent command for environment variable operations
var hubEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage environment variables",
	Long: `Manage environment variables stored in the Hub.

Environment variables can be scoped to:
  - User (default): Available to all your agents
  - Grove: Available to agents in a specific grove
  - Broker: Available to agents running on a specific broker

Variables are resolved hierarchically when an agent starts:
  user -> grove -> broker -> agent config

Examples:
  # Set a user-scoped variable (two formats)
  scion hub env set API_URL=https://api.example.com
  scion hub env set API_URL https://api.example.com

  # Set a grove-scoped variable (infer grove from current directory)
  scion hub env set --grove API_URL=https://api.example.com

  # Set a grove-scoped variable with explicit grove ID
  scion hub env set --grove=abc123 API_URL=https://api.example.com

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

By default, variables are scoped to the current user. Use --grove or --broker
to set variables at different scopes.

The value can be provided as a single argument in KEY=VALUE format, or as
two separate arguments.

Examples:
  scion hub env set API_URL=https://api.example.com
  scion hub env set API_URL https://api.example.com
  scion hub env set --grove LOG_LEVEL=debug
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
  scion hub env get --grove            # List grove variables
  scion hub env get --grove API_URL    # Get grove variable`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEnvGet,
}

// hubEnvClearCmd clears an environment variable
var hubEnvClearCmd = &cobra.Command{
	Use:   "clear KEY",
	Short: "Clear an environment variable",
	Long: `Remove an environment variable from the Hub.

Examples:
  scion hub env clear API_URL
  scion hub env clear --grove API_URL
  scion hub env clear --broker API_URL`,
	Args: cobra.ExactArgs(1),
	RunE: runEnvClear,
}

func init() {
	hubCmd.AddCommand(hubEnvCmd)
	hubEnvCmd.AddCommand(hubEnvSetCmd)
	hubEnvCmd.AddCommand(hubEnvGetCmd)
	hubEnvCmd.AddCommand(hubEnvClearCmd)

	// Add scope flags to all subcommands
	for _, cmd := range []*cobra.Command{hubEnvSetCmd, hubEnvGetCmd, hubEnvClearCmd} {
		cmd.Flags().StringVar(&envGroveScope, "grove", "", "Grove scope (use flag without value to infer from current directory, or provide grove ID)")
		cmd.Flags().StringVar(&envBrokerScope, "broker", "", "Broker scope (use flag without value to use current broker, or provide broker ID)")
	}

	hubEnvGetCmd.Flags().BoolVar(&envOutputJSON, "json", false, "Output in JSON format")
}

// resolveEnvScope determines the scope and scopeID based on flags
func resolveEnvScope(cmd *cobra.Command, settings *config.Settings) (scope, scopeID string, err error) {
	groveSet := cmd.Flags().Changed("grove")
	brokerSet := cmd.Flags().Changed("broker")

	if groveSet && brokerSet {
		return "", "", fmt.Errorf("cannot specify both --grove and --broker")
	}

	if groveSet {
		scope = "grove"
		if envGroveScope != "" {
			scopeID = envGroveScope
		} else {
			// Infer from settings
			if settings.Hub != nil && settings.Hub.GroveID != "" {
				scopeID = settings.Hub.GroveID
			} else {
				return "", "", fmt.Errorf("cannot infer grove ID: not linked with Hub. Use 'scion hub link' first or provide explicit grove ID")
			}
		}
		return scope, scopeID, nil
	}

	if brokerSet {
		scope = "runtime_broker"
		if envBrokerScope != "" {
			scopeID = envBrokerScope
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

	scope, scopeID, err := resolveEnvScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := &hubclient.SetEnvRequest{
		Value:   value,
		Scope:   scope,
		ScopeID: scopeID,
	}

	resp, err := client.Env().Set(ctx, key, req)
	if err != nil {
		return fmt.Errorf("failed to set environment variable: %w", err)
	}

	if resp.Created {
		fmt.Printf("Created %s=%s (scope: %s)\n", key, value, scope)
	} else {
		fmt.Printf("Updated %s=%s (scope: %s)\n", key, value, scope)
	}

	return nil
}

func runEnvGet(cmd *cobra.Command, args []string) error {
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

	scope, scopeID, err := resolveEnvScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
			fmt.Printf("%s=****** (sensitive, scope: %s)\n", envVar.Key, envVar.Scope)
		} else {
			fmt.Printf("%s=%s\n", envVar.Key, envVar.Value)
		}
		return nil
	}

	// List all variables for scope
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
			fmt.Printf("  %s=****** (sensitive)\n", v.Key)
		} else {
			fmt.Printf("  %s=%s\n", v.Key, v.Value)
		}
	}

	return nil
}

func runEnvClear(cmd *cobra.Command, args []string) error {
	key := args[0]

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

	scope, scopeID, err := resolveEnvScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
