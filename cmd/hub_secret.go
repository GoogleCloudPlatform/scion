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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	secretProjectScope string
	secretBrokerScope  string
	secretScope        string
	secretOutputJSON   bool
	secretType         string
	secretTarget       string
	secretAllowProgeny bool
)

// hubSecretCmd is the parent command for secret operations
var hubSecretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secrets",
	Long: `Manage secrets stored in the Hub.

Secrets are write-only values that can never be retrieved after creation.
They are injected into agents at runtime but never exposed via the API.

Secrets can be scoped to:
  - Hub: Available to all agents across the entire hub (admin-only writes)
  - User (default): Available to all your agents
  - Project: Available to agents in a specific project
  - Broker: Available to agents running on a specific broker

Secrets are resolved hierarchically when an agent starts:
  hub -> user -> project -> broker -> agent config

Examples:
  # Set a user-scoped secret
  scion hub secret set ANTHROPIC_API_KEY sk-...

  # Set a hub-scoped secret
  scion hub secret set --scope hub ANTHROPIC_API_KEY sk-...

  # Set a project-scoped secret (infer project from current directory)
  scion hub secret set --project DATABASE_PASSWORD mypassword

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

By default, secrets are scoped to the current user. Use --project or --broker
to set secrets at different scopes.

Secret types control how the value is projected into agent containers:
  - environment (default): Injected as an environment variable
  - variable: Written to ~/.scion/secrets.json for programmatic access
  - file: Written to the filesystem at the specified target path

For file secrets, prefix the value with @ to read from a file:
  scion hub secret set --type file --target /etc/ssl/cert.pem TLS_CERT @cert.pem

Examples:
  scion hub secret set API_KEY sk-abc123
  scion hub secret set --scope hub API_KEY sk-abc123
  scion hub secret set --project DATABASE_PASSWORD mypassword
  scion hub secret set --type variable CONFIG_JSON '{"key":"val"}'
  scion hub secret set --type file --target /home/scion/.ssh/id_rsa SSH_KEY @~/.ssh/id_rsa`,
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
  scion hub secret get --project          # List project secrets
  scion hub secret get --project API_KEY  # Get project secret metadata`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSecretGet,
}

// hubSecretListCmd lists secrets
var hubSecretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secrets",
	Long: `List all secrets for a scope from the Hub.

Secret values are never returned. This command only shows metadata
such as the key name, scope, type, version, and timestamps.

By default, lists user-scoped secrets. Use --project or --broker
to list secrets at different scopes.

Examples:
  scion hub secret list                    # List all user secrets
  scion hub secret list --project          # List project secrets
  scion hub secret list --json             # Output as JSON`,
	Args: cobra.NoArgs,
	RunE: runSecretList,
}

// hubSecretClearCmd clears a secret
var hubSecretClearCmd = &cobra.Command{
	Use:   "clear KEY",
	Short: "Clear a secret",
	Long: `Remove a secret from the Hub.

Examples:
  scion hub secret clear API_KEY
  scion hub secret clear --project API_KEY
  scion hub secret clear --broker API_KEY`,
	Args: cobra.ExactArgs(1),
	RunE: runSecretClear,
}

func init() {
	hubCmd.AddCommand(hubSecretCmd)
	hubSecretCmd.AddCommand(hubSecretSetCmd)
	hubSecretCmd.AddCommand(hubSecretGetCmd)
	hubSecretCmd.AddCommand(hubSecretListCmd)
	hubSecretCmd.AddCommand(hubSecretClearCmd)

	// Add scope flags to all subcommands.
	// --scope selects the scope level (hub, user). --project/--broker select their
	// respective scopes and support both bare usage (infer from settings) and
	// explicit name/ID via --project=<name|id>.
	for _, cmd := range []*cobra.Command{hubSecretSetCmd, hubSecretGetCmd, hubSecretListCmd, hubSecretClearCmd} {
		cmd.Flags().StringVar(&secretScope, "scope", "", "Scope level: hub, user (default: user)")
		cmd.Flags().StringVar(&secretProjectScope, "project", "", "Project scope (bare flag infers current project, or use --project=<name|id>)")
		cmd.Flags().Lookup("project").NoOptDefVal = scopeInferSentinel

		cmd.Flags().StringVar(&secretProjectScope, "grove", "", "Deprecated alias for --project")
		cmd.Flags().Lookup("grove").NoOptDefVal = scopeInferSentinel
		_ = cmd.Flags().MarkDeprecated("grove", "use --project instead")
		_ = cmd.Flags().MarkHidden("grove")

		cmd.Flags().StringVar(&secretBrokerScope, "broker", "", "Broker scope (bare flag infers current broker, or use --broker=<name|id>)")
		cmd.Flags().Lookup("broker").NoOptDefVal = scopeInferSentinel
	}

	hubSecretGetCmd.Flags().BoolVar(&secretOutputJSON, "json", false, "Output in JSON format")
	hubSecretListCmd.Flags().BoolVar(&secretOutputJSON, "json", false, "Output in JSON format")

	// Type and target flags for set command
	hubSecretSetCmd.Flags().StringVar(&secretType, "type", "", "Secret type: environment (default), variable, file")
	hubSecretSetCmd.Flags().StringVar(&secretTarget, "target", "", "Projection target (env var name, json key, or file path; defaults to KEY)")
	hubSecretSetCmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "Allow creator's progeny agents to access this secret (user scope only)")
}

// resolveSecretScope determines the scope and scopeID based on flags.
// When --project or --broker is used bare (no value), scopeID is inferred from settings.
// When a value is provided, it is returned as-is and may need further resolution
// (name/slug to UUID) via resolveScopeID.
func resolveSecretScope(cmd *cobra.Command, settings *config.Settings) (scope, scopeID string, err error) {
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
		switch secretScope {
		case "hub":
			return "hub", "", nil
		case "user", "":
			return "user", "", nil
		default:
			return "", "", fmt.Errorf("invalid --scope value %q: must be 'hub' or 'user'", secretScope)
		}
	}

	if projectSet || groveSet {
		scope = "project"
		projectVal := secretProjectScope
		if projectVal == scopeInferSentinel {
			projectVal = ""
		}
		if projectVal != "" {
			// Explicit value — may be a name, slug, or UUID (resolved later)
			scopeID = projectVal
		} else {
			// Infer from settings: try explicit hub link first, then local project ID
			if settings.Hub != nil && settings.Hub.ProjectID != "" {
				scopeID = settings.Hub.ProjectID
			} else if settings.ProjectID != "" {
				scopeID = settings.ProjectID
			} else {
				return "", "", fmt.Errorf("cannot infer project ID: not linked with Hub. Use 'scion hub link' first or provide explicit project ID")
			}
		}
		return scope, scopeID, nil
	}

	if brokerSet {
		scope = "runtime_broker"
		brokerVal := secretBrokerScope
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

	// Handle @filename prefix for file secrets: read file content and base64-encode
	if strings.HasPrefix(value, "@") {
		filePath := value[1:]
		// Expand ~ in source file path for reading
		if strings.HasPrefix(filePath, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to expand home directory: %w", err)
			}
			filePath = home + filePath[1:]
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", filePath, err)
		}
		value = base64.StdEncoding.EncodeToString(data)
		// Default to file type when using @file syntax
		if secretType == "" {
			secretType = "file"
		}
		// Auto-set target from source file path if not explicitly provided
		if secretTarget == "" {
			absPath, err := filepath.Abs(filePath)
			if err != nil {
				return fmt.Errorf("failed to resolve absolute path for %s: %w", filePath, err)
			}
			// Convert paths under the user's home directory to ~/ so they
			// map to the container user's home directory at projection time.
			home, err := os.UserHomeDir()
			if err == nil && strings.HasPrefix(absPath, home+"/") {
				secretTarget = "~/" + absPath[len(home)+1:]
			} else {
				secretTarget = absPath
			}
		}
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

	scope, scopeID, err := resolveSecretScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

	req := &hubclient.SetSecretRequest{
		Value:        value,
		Scope:        scope,
		ScopeID:      scopeID,
		Type:         secretType,
		Target:       secretTarget,
		AllowProgeny: secretAllowProgeny,
	}

	resp, err := client.Secrets().Set(ctx, key, req)
	if err != nil {
		return fmt.Errorf("failed to set secret: %w", err)
	}

	typeLabel := resp.Secret.SecretType
	if typeLabel == "" {
		typeLabel = "environment"
	}

	if resp.Created {
		fmt.Printf("Created secret %s (scope: %s, type: %s)\n", key, scope, typeLabel)
	} else {
		fmt.Printf("Updated secret %s (scope: %s, type: %s, version: %d)\n", key, scope, typeLabel, resp.Secret.Version)
	}

	return nil
}

func runSecretGet(cmd *cobra.Command, args []string) error {
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

	scope, scopeID, err := resolveSecretScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

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
		typeLabel := secret.SecretType
		if typeLabel == "" {
			typeLabel = "environment"
		}
		fmt.Printf("  Type:    %s\n", typeLabel)
		if secret.Target != "" && secret.Target != secret.Key {
			fmt.Printf("  Target:  %s\n", secret.Target)
		}
		if secret.SecretRef != "" {
			fmt.Printf("  Ref:     %s\n", secret.SecretRef)
		}
		fmt.Printf("  Version: %d\n", secret.Version)
		fmt.Printf("  Created: %s\n", secret.Created.Format(time.RFC3339))
		fmt.Printf("  Updated: %s\n", secret.Updated.Format(time.RFC3339))
		if secret.Description != "" {
			fmt.Printf("  Description: %s\n", secret.Description)
		}
		return nil
	}

	// No key provided, delegate to list
	return runSecretList(cmd, nil)
}

func runSecretList(cmd *cobra.Command, _ []string) error {
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

	scope, scopeID, err := resolveSecretScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

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
	fmt.Printf("%-30s  %-12s  %-8s  %-8s  %s\n", "KEY", "TYPE", "PROGENY", "VERSION", "UPDATED")
	fmt.Printf("%-30s  %-12s  %-8s  %-8s  %s\n", "------------------------------", "------------", "--------", "--------", "-------------------")
	for _, s := range resp.Secrets {
		typeLabel := s.SecretType
		if typeLabel == "" {
			typeLabel = "environment"
		}
		progenyLabel := "-"
		if s.AllowProgeny {
			progenyLabel = "\u2713"
		}
		fmt.Printf("%-30s  %-12s  %-8s  v%-7d  %s\n", truncate(s.Key, 30), typeLabel, progenyLabel, s.Version, s.Updated.Format("2006-01-02 15:04:05"))
	}

	return nil
}

func runSecretClear(cmd *cobra.Command, args []string) error {
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

	scope, scopeID, err := resolveSecretScope(cmd, settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		return err
	}

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
