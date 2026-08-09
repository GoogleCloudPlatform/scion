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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// Flag variables for agent secret commands. Named with "agentSecret" prefix
// to avoid collisions with the hub secret command flags in hub_secret.go.
var (
	agentSecretType   string
	agentSecretTarget string
)

// agentSecretCmd is the top-level `scion secret` command group.
var agentSecretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage agent secrets",
	Long: `Commands for managing project-scoped secrets from within an agent container.

Secrets are stored securely in the Hub and scoped to the current agent's project.
Use these commands from within a running agent to manage secrets programmatically.

For admin-level secret management with scope options, use 'scion hub secret'.`,
}

// agentSecretGetCmd retrieves metadata for a project-scoped secret.
var agentSecretGetCmd = &cobra.Command{
	Use:   "get KEY",
	Short: "Get secret metadata",
	Long: `Get metadata for a project-scoped secret from the Hub.

Returns metadata such as the key name, type, scope, version, and timestamps.

Examples:
  # Get secret metadata
  scion secret get MY_API_KEY`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentSecretGet,
}

// agentSecretSetCmd stores a project-scoped secret.
var agentSecretSetCmd = &cobra.Command{
	Use:   "set KEY VALUE",
	Short: "Store a project-scoped secret",
	Long: `Store a project-scoped secret in the Hub from within an agent container.

The secret is scoped to the current agent's project. Subsequent agents in the
same project will receive this secret automatically.

If VALUE starts with @, the remainder is treated as a file path. The file
contents are read and base64-encoded, and --type defaults to "file".

Examples:
  # Store a simple environment variable secret
  scion secret set MY_API_KEY "sk-abc123"

  # Store a credential file
  scion secret set CLAUDE_AUTH @~/.claude/.credentials.json

  # Store a file secret with explicit target path
  scion secret set MY_CERT @/tmp/cert.pem --type file --target ~/certs/cert.pem

  # Update an existing secret (overwrites automatically)
  scion secret set MY_KEY "new-value"`,
	Args: cobra.ExactArgs(2),
	RunE: runAgentSecretSet,
}

// agentSecretListCmd lists metadata for all project-scoped secrets.
var agentSecretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List project-scoped secrets",
	Long: `List metadata for all project-scoped secrets from the Hub.

Only metadata (key, type, version, updated) is returned — secret values
are not included.

Examples:
  scion secret list`,
	Args: cobra.NoArgs,
	RunE: runAgentSecretList,
}

func init() {
	rootCmd.AddCommand(agentSecretCmd)
	agentSecretCmd.AddCommand(agentSecretGetCmd)
	agentSecretCmd.AddCommand(agentSecretSetCmd)
	agentSecretCmd.AddCommand(agentSecretListCmd)

	agentSecretSetCmd.Flags().StringVar(&agentSecretType, "type", "", "Secret type: environment (default), variable, file")
	agentSecretSetCmd.Flags().StringVar(&agentSecretTarget, "target", "", "Injection target path (defaults to key for env, required for file)")
}

func runAgentSecretGet(cmd *cobra.Command, args []string) error {
	key := args[0]

	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("secret commands require Hub mode (use 'scion hub enable' first)")
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	opts := &hubclient.SecretScopeOptions{
		Scope:   "project",
		ScopeID: projectID,
	}

	secret, err := hubCtx.Client.Secrets().Get(ctx, key, opts)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to get secret: %w", err))
	}

	if isJSONOutput() {
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

func runAgentSecretSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	// Validate key.
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}
	if strings.ContainsAny(key, "= \t\n") {
		return fmt.Errorf("key cannot contain spaces, tabs, newlines, or '='")
	}

	localType := agentSecretType
	localTarget := agentSecretTarget

	// Handle @file syntax: read file and base64-encode contents.
	if strings.HasPrefix(value, "@") {
		filePath := value[1:]
		if filePath == "" {
			return fmt.Errorf("empty file path: VALUE starting with @ must be followed by a file path (e.g., @/path/to/file)")
		}
		// Expand ~ in source file path for reading.
		if filePath == "~" || strings.HasPrefix(filePath, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to expand home directory: %w", err)
			}
			filePath = filepath.Join(home, strings.TrimPrefix(filePath[1:], "/"))
		}
		info, err := os.Stat(filePath)
		if err != nil {
			return fmt.Errorf("failed to stat file %s: %w", filePath, err)
		}
		if info.IsDir() {
			return fmt.Errorf("cannot read secret from directory: %s (expected a file)", filePath)
		}
		if info.Size() > 64*1024 {
			return fmt.Errorf("file exceeds 64KB limit (%d bytes)", info.Size())
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", filePath, err)
		}
		value = base64.StdEncoding.EncodeToString(data)
		if localType == "" {
			localType = "file"
		}
		// Auto-set target from source file path if not explicitly provided.
		if localTarget == "" {
			absPath, err := filepath.Abs(filePath)
			if err != nil {
				return fmt.Errorf("failed to resolve absolute path for %s: %w", filePath, err)
			}
			// Convert paths under the user's home directory to ~/ so they
			// map to the container user's home directory at projection time.
			home, err := os.UserHomeDir()
			if err == nil && strings.HasPrefix(absPath, home+"/") {
				localTarget = "~/" + absPath[len(home)+1:]
			} else {
				localTarget = absPath
			}
		}
	}

	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("secret commands require Hub mode (use 'scion hub enable' first)")
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	typeLabel := localType
	if typeLabel == "" {
		typeLabel = "environment"
	}

	// Use the agent-scoped endpoint when running as an agent. The generic
	// /api/v1/secrets endpoint forbids project-scope writes from agent JWTs,
	// so agents must use /api/v1/agents/{id}/secrets/{key} instead.
	agentID := os.Getenv("SCION_AGENT_ID")
	if agentID != "" {
		// Base64-encode non-file values before sending. The hub expects
		// base64-encoded values by default; file values are already
		// base64-encoded by the @file handling above.
		agentValue := value
		if !strings.HasPrefix(args[1], "@") {
			agentValue = base64.StdEncoding.EncodeToString([]byte(value))
		}
		req := &hubclient.AgentSetSecretRequest{
			Value:  agentValue,
			Type:   localType,
			Target: localTarget,
			Force:  true, // scion secret set always overwrites
		}

		resp, err := hubCtx.Client.Secrets().AgentSet(ctx, agentID, key, req)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to set secret: %w", err))
		}

		if resp.Created {
			fmt.Printf("Created secret %s (scope: project, type: %s)\n", key, typeLabel)
		} else {
			fmt.Printf("Updated secret %s (scope: project, type: %s)\n", key, typeLabel)
		}
		return nil
	}

	// Fall back to the user-scoped endpoint for non-agent contexts.
	// Base64-encode non-file values; file values are already encoded above.
	userValue := value
	if !strings.HasPrefix(args[1], "@") {
		userValue = base64.StdEncoding.EncodeToString([]byte(value))
	}
	req := &hubclient.SetSecretRequest{
		Value:   userValue,
		Scope:   "project",
		ScopeID: projectID,
		Type:    localType,
		Target:  localTarget,
	}

	resp, err := hubCtx.Client.Secrets().Set(ctx, key, req)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to set secret: %w", err))
	}

	if resp.Created {
		fmt.Printf("Created secret %s (scope: project, type: %s)\n", key, typeLabel)
	} else {
		version := 0
		if resp.Secret != nil {
			version = resp.Secret.Version
		}
		fmt.Printf("Updated secret %s (scope: project, type: %s, version: %d)\n", key, typeLabel, version)
	}

	return nil
}

func runAgentSecretList(cmd *cobra.Command, _ []string) error {
	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("secret commands require Hub mode (use 'scion hub enable' first)")
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListSecretOptions{
		Scope:   "project",
		ScopeID: projectID,
	}

	resp, err := hubCtx.Client.Secrets().List(ctx, opts)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list secrets: %w", err))
	}

	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if len(resp.Secrets) == 0 {
		fmt.Println("No secrets found (scope: project)")
		return nil
	}

	fmt.Println("Secrets (scope: project):")
	fmt.Printf("%-30s  %-12s  %-8s  %s\n", "KEY", "TYPE", "VERSION", "UPDATED")
	fmt.Printf("%-30s  %-12s  %-8s  %s\n", "------------------------------", "------------", "--------", "-------------------")
	for _, s := range resp.Secrets {
		typeLabel := s.SecretType
		if typeLabel == "" {
			typeLabel = "environment"
		}
		fmt.Printf("%-30s  %-12s  v%-7d  %s\n", truncate(s.Key, 30), typeLabel, s.Version, s.Updated.Format("2006-01-02 15:04:05"))
	}

	return nil
}
