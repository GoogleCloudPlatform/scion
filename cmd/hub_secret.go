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
	secretGroveScope string
	secretBrokerScope  string
	secretOutputJSON bool
)

// hubSecretCmd is the parent command for secret operations
var hubSecretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secrets",
	Long: `Manage secrets stored in the Hub.

Secrets are write-only values that can never be retrieved after creation.
They are injected into agents at runtime but never exposed via the API.

Secrets can be scoped to:
  - User (default): Available to all your agents
  - Grove: Available to agents in a specific grove
  - Broker: Available to agents running on a specific broker

Secrets are resolved hierarchically when an agent starts:
  user -> grove -> broker -> agent config

Examples:
  # Set a user-scoped secret
  scion hub secret set ANTHROPIC_API_KEY sk-...

  # Set a grove-scoped secret (infer grove from current directory)
  scion hub secret set --grove DATABASE_PASSWORD mypassword

  # List all user secrets (metadata only, no values)
  scion hub secret get

  # Get secret metadata
  scion hub secret get ANTHROPIC_API_KEY

  # Delete a secret
  scion hub secret clear ANTHROPIC_API_KEY`,
}

// hubSecretSetCmd sets a secret
var hubSecretSetCmd = &cobra.Command{
	Use:   "set KEY VALUE",
	Short: "Set a secret",
	Long: `Set a secret in the Hub.

The value is stored securely and can never be retrieved after creation.
Only metadata (key, scope, creation time) can be viewed.

By default, secrets are scoped to the current user. Use --grove or --broker
to set secrets at different scopes.

Examples:
  scion hub secret set API_KEY sk-abc123
  scion hub secret set --grove DATABASE_PASSWORD mypassword
  scion hub secret set --broker SSH_PRIVATE_KEY "$(cat ~/.ssh/id_rsa)"`,
	Args: cobra.ExactArgs(2),
	RunE: runSecretSet,
}

// hubSecretGetCmd gets secret metadata
var hubSecretGetCmd = &cobra.Command{
	Use:   "get [KEY]",
	Short: "Get secret metadata",
	Long: `Get secret metadata from the Hub.

Secret values are never returned. This command only shows metadata
such as the key name, scope, version, and timestamps.

Without a key, lists all secrets for the scope.
With a key, returns metadata for the specific secret.

Examples:
  scion hub secret get                    # List all user secrets
  scion hub secret get API_KEY            # Get specific secret metadata
  scion hub secret get --grove            # List grove secrets
  scion hub secret get --grove API_KEY    # Get grove secret metadata`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSecretGet,
}

// hubSecretClearCmd clears a secret
var hubSecretClearCmd = &cobra.Command{
	Use:   "clear KEY",
	Short: "Clear a secret",
	Long: `Remove a secret from the Hub.

Examples:
  scion hub secret clear API_KEY
  scion hub secret clear --grove API_KEY
  scion hub secret clear --broker API_KEY`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretClear,
}

func init() {
	hubCmd.AddCommand(hubSecretCmd)
	hubSecretCmd.AddCommand(hubSecretSetCmd)
	hubSecretCmd.AddCommand(hubSecretGetCmd)
	hubSecretCmd.AddCommand(hubSecretClearCmd)

	// Add scope flags to all subcommands
	for _, cmd := range []*cobra.Command{hubSecretSetCmd, hubSecretGetCmd, hubSecretClearCmd} {
		cmd.Flags().StringVar(&secretGroveScope, "grove", "", "Grove scope (use flag without value to infer from current directory, or provide grove ID)")
		cmd.Flags().StringVar(&secretBrokerScope, "broker", "", "Broker scope (use flag without value to use current broker, or provide broker ID)")
	}

	hubSecretGetCmd.Flags().BoolVar(&secretOutputJSON, "json", false, "Output in JSON format")
}

// resolveSecretScope determines the scope and scopeID based on flags
func resolveSecretScope(cmd *cobra.Command, settings *config.Settings) (scope, scopeID string, err error) {
	groveSet := cmd.Flags().Changed("grove")
	brokerSet := cmd.Flags().Changed("broker")

	if groveSet && brokerSet {
		return "", "", fmt.Errorf("cannot specify both --grove and --broker")
	}

	if groveSet {
		scope = "grove"
		if secretGroveScope != "" {
			scopeID = secretGroveScope
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
		if secretBrokerScope != "" {
			scopeID = secretBrokerScope
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

func runSecretSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

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

	scope, scopeID, err := resolveSecretScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := &hubclient.SetSecretRequest{
		Value:   value,
		Scope:   scope,
		ScopeID: scopeID,
	}

	resp, err := client.Secrets().Set(ctx, key, req)
	if err != nil {
		return fmt.Errorf("failed to set secret: %w", err)
	}

	if resp.Created {
		fmt.Printf("Created secret %s (scope: %s)\n", key, scope)
	} else {
		fmt.Printf("Updated secret %s (scope: %s, version: %d)\n", key, scope, resp.Secret.Version)
	}

	return nil
}

func runSecretGet(cmd *cobra.Command, args []string) error {
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

	scope, scopeID, err := resolveSecretScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// If key is provided, get specific secret metadata
	if len(args) == 1 {
		key := args[0]
		opts := &hubclient.SecretScopeOptions{
			Scope:   scope,
			ScopeID: scopeID,
		}

		secret, err := client.Secrets().Get(ctx, key, opts)
		if err != nil {
			return fmt.Errorf("failed to get secret: %w", err)
		}

		if secretOutputJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(secret)
		}

		fmt.Printf("Secret: %s\n", secret.Key)
		fmt.Printf("  Scope:   %s\n", secret.Scope)
		fmt.Printf("  Version: %d\n", secret.Version)
		fmt.Printf("  Created: %s\n", secret.Created.Format(time.RFC3339))
		fmt.Printf("  Updated: %s\n", secret.Updated.Format(time.RFC3339))
		if secret.Description != "" {
			fmt.Printf("  Description: %s\n", secret.Description)
		}
		return nil
	}

	// List all secrets for scope
	opts := &hubclient.ListSecretOptions{
		Scope:   scope,
		ScopeID: scopeID,
	}

	resp, err := client.Secrets().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	if secretOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if len(resp.Secrets) == 0 {
		fmt.Printf("No secrets found (scope: %s)\n", scope)
		return nil
	}

	fmt.Printf("Secrets (scope: %s):\n", scope)
	fmt.Printf("%-30s  %-8s  %s\n", "KEY", "VERSION", "UPDATED")
	fmt.Printf("%-30s  %-8s  %s\n", "------------------------------", "--------", "-------------------")
	for _, s := range resp.Secrets {
		fmt.Printf("%-30s  v%-7d  %s\n", truncate(s.Key, 30), s.Version, s.Updated.Format("2006-01-02 15:04:05"))
	}

	return nil
}

func runSecretClear(cmd *cobra.Command, args []string) error {
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

	scope, scopeID, err := resolveSecretScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	opts := &hubclient.SecretScopeOptions{
		Scope:   scope,
		ScopeID: scopeID,
	}

	if err := client.Secrets().Delete(ctx, key, opts); err != nil {
		return fmt.Errorf("failed to delete secret: %w", err)
	}

	fmt.Printf("Deleted secret %s (scope: %s)\n", key, scope)
	return nil
}
