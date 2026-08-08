/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

var (
	identityTokenAudience string
	identityTokenFormat   string
)

var identityTokenCmd = &cobra.Command{
	Use:   "identity-token",
	Short: "Request an OIDC identity token for external system authentication",
	Long: `Requests a short-lived RS256-signed identity token from the Hub.
The token can be used to authenticate to external systems that support
OIDC/JWT verification (Vault, GCP WIF, AWS IRSA, A2A bridges, etc.).

Examples:
  # Get a raw token for pipeline use
  sciontool identity-token --audience=https://vault.example.com

  # Get structured JSON output
  sciontool identity-token --audience=https://vault.example.com --format=json

  # Use in a script
  export TOKEN=$(sciontool identity-token --audience=https://vault.example.com)`,
	RunE: runIdentityToken,
}

func init() {
	rootCmd.AddCommand(identityTokenCmd)

	identityTokenCmd.Flags().StringVar(&identityTokenAudience, "audience", "",
		"Target audience for the token (required)")
	identityTokenCmd.Flags().StringVar(&identityTokenFormat, "format", "token",
		"Output format: \"token\" (raw JWT) or \"json\"")
}

func runIdentityToken(cmd *cobra.Command, args []string) error {
	if identityTokenAudience == "" {
		return fmt.Errorf("--audience is required")
	}

	if identityTokenFormat != "token" && identityTokenFormat != "json" {
		return fmt.Errorf("--format must be \"token\" or \"json\", got %q", identityTokenFormat)
	}

	hubClient := hub.NewClient()
	if hubClient == nil || !hubClient.IsConfigured() {
		return fmt.Errorf("hub client not configured (is SCION_HUB_ENDPOINT set?)")
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := hubClient.RequestIdentityToken(ctx, identityTokenAudience)
	if err != nil {
		return err
	}

	switch identityTokenFormat {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "")
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("failed to encode response: %w", err)
		}
	default:
		// Raw token output — no trailing newline for clean pipeline use.
		_, _ = fmt.Fprint(cmd.OutOrStdout(), resp.Token)
	}

	return nil
}
