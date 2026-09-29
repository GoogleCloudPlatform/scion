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
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	tokenOutputJSON bool
)

// hubTokenCmd is the parent command for user access token operations.
var hubTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage user access tokens",
	Long: `Manage user access tokens for CI/CD and automation.

User access tokens (UATs) are scoped, revocable bearer tokens for
non-interactive authentication. Each token is scoped to a single project
and carries a set of action permissions.

Examples:
  # Create a token for CI that can create and monitor agents
  scion hub token create \
    --project my-project \
    --name "github-actions" \
    --scopes agent:create,agent:read \
    --expires 90d

  # List your tokens
  scion hub token list

  # Revoke a token
  scion hub token revoke <token-id>

  # Delete a token permanently
  scion hub token delete <token-id>

  # Use the token in CI
  export SCION_HUB_TOKEN=scion_pat_...
  scion hub agent dispatch --project my-project --template default --task "Run tests"`,
}

var hubTokenCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new access token",
	Long: fmt.Sprintf(`Create a new user access token scoped to a project.

The token value is displayed only once on creation. Store it securely.

Available scopes:
%s

Expiry can be specified as a duration (e.g., 30d, 90d, 1y) or an
RFC 3339 date (e.g., 2026-12-31T00:00:00Z). Default: 90 days.
Maximum: 1 year.

Examples:
  scion hub token create --project my-project --name ci-token --scopes agent:create,agent:read
  scion hub token create --project my-project --name deploy --scopes agent:manage --expires 30d`, permissions.UATScopeHelp()),
	Args: cobra.NoArgs,
	RunE: runTokenCreate,
}

var hubTokenListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your access tokens",
	Long: `List all user access tokens for the authenticated user.

Examples:
  scion hub token list
  scion hub token list --json
  scion hub token list --project my-project`,
	Args: cobra.NoArgs,
	RunE: runTokenList,
}

var hubTokenRevokeCmd = &cobra.Command{
	Use:   "revoke TOKEN-ID",
	Short: "Revoke an access token",
	Long: `Revoke a user access token. The token will no longer be accepted
for authentication but will still appear in listings as revoked.

Examples:
  scion hub token revoke abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runTokenRevoke,
}

var hubTokenDeleteCmd = &cobra.Command{
	Use:   "delete TOKEN-ID",
	Short: "Delete an access token permanently",
	Long: `Permanently delete a user access token. This cannot be undone.

Examples:
  scion hub token delete abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runTokenDelete,
}

var (
	tokenCreateName    string
	tokenCreateProject string
	tokenCreateScopes  []string
	tokenCreateExpires string
	tokenCreatePurpose string
	tokenCreateLabels  []string
	tokenListProject   string
)

func init() {
	hubCmd.AddCommand(hubTokenCmd)
	hubTokenCmd.AddCommand(hubTokenCreateCmd)
	hubTokenCmd.AddCommand(hubTokenListCmd)
	hubTokenCmd.AddCommand(hubTokenRevokeCmd)
	hubTokenCmd.AddCommand(hubTokenDeleteCmd)

	hubTokenCreateCmd.Flags().StringVar(&tokenCreateName, "name", "", "Token name/label (required)")
	hubTokenCreateCmd.Flags().StringVar(&tokenCreateProject, "project", "", "Project name or ID to scope the token to (required)")

	hubTokenCreateCmd.Flags().StringArrayVar(&tokenCreateScopes, "scopes", nil, "Scope to grant (required, repeatable; also accepts a comma-separated list)")
	hubTokenCreateCmd.Flags().StringVar(&tokenCreateExpires, "expires", "", "Expiry duration (e.g., 30d, 90d, 1y) or RFC 3339 date (default: 90d)")
	// E.1 descriptive credential metadata: optional, bounded, immutable
	// after issuance (there is no update command). Requires a hub connection,
	// like the rest of `scion hub token`.
	hubTokenCreateCmd.Flags().StringVar(&tokenCreatePurpose, "purpose", "", "Optional bounded description of what this token is for")
	hubTokenCreateCmd.Flags().StringArrayVar(&tokenCreateLabels, "label", nil, "Optional bounded label as key=value (repeatable)")

	_ = hubTokenCreateCmd.MarkFlagRequired("name")
	_ = hubTokenCreateCmd.MarkFlagRequired("project")
	_ = hubTokenCreateCmd.MarkFlagRequired("scopes")

	hubTokenListCmd.Flags().BoolVar(&tokenOutputJSON, "json", false, "Output in JSON format")
	hubTokenListCmd.Flags().StringVar(&tokenListProject, "project", "", "Filter tokens by project name or ID")
}

func runTokenCreate(cmd *cobra.Command, args []string) error {
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Resolve project name/slug to ID
	project, err := resolveProjectByNameOrID(ctx, client, tokenCreateProject)
	if err != nil {
		return fmt.Errorf("failed to resolve project %q: %w", tokenCreateProject, err)
	}

	scopes := splitCommaList(tokenCreateScopes)
	if len(scopes) == 0 {
		return fmt.Errorf("--scopes must specify at least one scope")
	}

	var expiresAt *time.Time
	if tokenCreateExpires != "" {
		t, err := parseExpiry(tokenCreateExpires)
		if err != nil {
			return fmt.Errorf("invalid --expires value: %w", err)
		}
		expiresAt = &t
	}

	labels, err := parseLabelFlags(tokenCreateLabels)
	if err != nil {
		return err
	}

	req := &hubclient.CreateTokenRequest{
		Name:      tokenCreateName,
		ProjectID: project.ID,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
		Purpose:   tokenCreatePurpose,
		Labels:    labels,
	}

	resp, err := client.Tokens().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create token: %w", err)
	}

	if tokenOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	fmt.Printf("Created access token: %s\n", resp.AccessToken.Name)
	fmt.Printf("  ID:      %s\n", resp.AccessToken.ID)
	fmt.Printf("  Project:   %s (%s)\n", project.Name, project.ID)
	fmt.Printf("  Scopes:  %s\n", strings.Join(resp.AccessToken.Scopes, ", "))
	if resp.AccessToken.ExpiresAt != nil {
		fmt.Printf("  Expires: %s\n", resp.AccessToken.ExpiresAt.Format(time.RFC3339))
	}
	if resp.AccessToken.Purpose != "" {
		fmt.Printf("  Purpose: %s\n", resp.AccessToken.Purpose)
	}
	if len(resp.AccessToken.Labels) > 0 {
		fmt.Printf("  Labels:  %s\n", formatLabels(resp.AccessToken.Labels))
	}
	fmt.Println()
	fmt.Printf("Token: %s\n", resp.Token)
	fmt.Println()
	fmt.Println("This token will not be shown again. Store it securely.")

	return nil
}

func runTokenList(cmd *cobra.Command, args []string) error {
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Tokens().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list tokens: %w", err)
	}

	// Optionally filter by project
	var projectID string
	if tokenListProject != "" {
		project, err := resolveProjectByNameOrID(ctx, client, tokenListProject)
		if err != nil {
			return fmt.Errorf("failed to resolve project %q: %w", tokenListProject, err)
		}
		projectID = project.ID
	}

	items := resp.Items
	if projectID != "" {
		var filtered []hubclient.TokenInfo
		for _, t := range items {
			if t.ProjectID == projectID {
				filtered = append(filtered, t)
			}
		}
		items = filtered
	}

	if tokenOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]interface{}{"items": items})
	}

	if len(items) == 0 {
		fmt.Println("No access tokens found.")
		return nil
	}

	fmt.Printf("%-20s  %-36s  %-16s  %-10s  %-19s  %s\n", "NAME", "ID", "PREFIX", "STATUS", "EXPIRES", "SCOPES")
	fmt.Printf("%-20s  %-36s  %-16s  %-10s  %-19s  %s\n",
		"--------------------", "------------------------------------", "----------------", "----------", "-------------------", "------")
	for _, t := range items {
		status := "active"
		if t.Revoked {
			status = "revoked"
		} else if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
			status = "expired"
		}

		expires := "never"
		if t.ExpiresAt != nil {
			expires = t.ExpiresAt.Format("2006-01-02 15:04:05")
		}

		fmt.Printf("%-20s  %-36s  %-16s  %-10s  %-19s  %s\n",
			truncate(t.Name, 20),
			t.ID,
			t.Prefix,
			status,
			expires,
			strings.Join(t.Scopes, ","),
		)
	}

	return nil
}

func runTokenRevoke(cmd *cobra.Command, args []string) error {
	tokenID := args[0]

	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Tokens().Revoke(ctx, tokenID); err != nil {
		return fmt.Errorf("failed to revoke token: %w", err)
	}

	fmt.Printf("Token %s revoked.\n", tokenID)
	return nil
}

func runTokenDelete(cmd *cobra.Command, args []string) error {
	tokenID := args[0]

	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Tokens().Delete(ctx, tokenID); err != nil {
		return fmt.Errorf("failed to delete token: %w", err)
	}

	fmt.Printf("Token %s deleted.\n", tokenID)
	return nil
}

// parseLabelFlags parses repeated --label key=value flags into a map. It does
// not itself enforce the bounded label schema; the hub validates and rejects
// out-of-schema labels server-side (E.1), so the CLI reports the server's
// error rather than duplicating the rule set.
func parseLabelFlags(labels []string) (map[string]string, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(labels))
	for _, l := range labels {
		key, value, ok := strings.Cut(l, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --label %q: expected key=value", l)
		}
		result[key] = value
	}
	return result, nil
}

// formatLabels renders labels as a stable, comma-separated key=value list.
func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ", ")
}

// parseExpiry parses an expiry string as either a duration shorthand (30d, 90d, 1y)
// or an RFC 3339 timestamp.
func parseExpiry(s string) (time.Time, error) {
	// Try RFC 3339 first
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	// Try duration shorthand: Nd (days) or Ny (years)
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return time.Time{}, fmt.Errorf("expected a duration like '30d' or '1y', or an RFC 3339 date")
	}

	unit := s[len(s)-1]
	numStr := s[:len(s)-1]

	var n int
	if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("expected a positive number followed by 'd' (days) or 'y' (years)")
	}

	now := time.Now().UTC()
	switch unit {
	case 'd':
		return now.Add(time.Duration(n) * 24 * time.Hour), nil
	case 'y':
		return now.AddDate(n, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unknown duration unit %q: use 'd' (days) or 'y' (years)", string(unit))
	}
}
