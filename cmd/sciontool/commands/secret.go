/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

var (
	secretType   string
	secretTarget string
	secretForce  bool
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage agent secrets",
	Long:  `Commands for managing secrets from within an agent container.`,
}

var secretSetCmd = &cobra.Command{
	Use:   "set KEY VALUE",
	Short: "Store a project-scoped secret via the Hub API",
	Long: `Store a project-scoped secret in the Hub from within an agent container.

The secret is scoped to the current agent's project. Subsequent agents in the
same project will receive this secret automatically.

If VALUE starts with @, the remainder is treated as a file path. The file
contents are read and base64-encoded, and --type defaults to "file".

Examples:
  # Store a simple environment variable secret
  sciontool secret set MY_API_KEY "sk-abc123"

  # Store a credential file
  sciontool secret set CLAUDE_AUTH @~/.claude/.credentials.json

  # Store a file secret with explicit target path
  sciontool secret set MY_CERT @/tmp/cert.pem --type file --target ~/certs/cert.pem

  # Overwrite an existing secret
  sciontool secret set MY_KEY "new-value" --force`,
	Args: cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		key := args[0]
		value := args[1]

		if key == "" {
			log.Error("key cannot be empty")
			os.Exit(1)
		}
		if strings.ContainsAny(key, "= \t\n") {
			log.Error("key cannot contain spaces, tabs, newlines, or '='")
			os.Exit(1)
		}

		localType := secretType
		localTarget := secretTarget

		// Handle @file syntax: read file and base64-encode contents.
		if strings.HasPrefix(value, "@") {
			filePath := value[1:]
			if filePath == "~" || strings.HasPrefix(filePath, "~/") {
				home, err := os.UserHomeDir()
				if err != nil {
					log.Error("Failed to expand home directory: %v", err)
					os.Exit(1)
				}
				filePath = filepath.Join(home, strings.TrimPrefix(filePath[1:], "/"))
			}
			info, err := os.Stat(filePath)
			if err != nil {
				log.Error("Failed to stat file %s: %v", filePath, err)
				os.Exit(1)
			}
			if info.Size() > 64*1024 {
				log.Error("File exceeds 64KB limit (%d bytes)", info.Size())
				os.Exit(1)
			}
			data, err := os.ReadFile(filePath)
			if err != nil {
				log.Error("Failed to read file %s: %v", filePath, err)
				os.Exit(1)
			}
			value = base64.StdEncoding.EncodeToString(data)
			if localType == "" {
				localType = "file"
			}
			if localTarget == "" {
				absPath, err := filepath.Abs(filePath)
				if err != nil {
					log.Error("Failed to resolve absolute path for %s: %v", filePath, err)
					os.Exit(1)
				}
				home, err := os.UserHomeDir()
				if err == nil && strings.HasPrefix(absPath, home+"/") {
					localTarget = "~/" + absPath[len(home)+1:]
				} else {
					localTarget = absPath
				}
			}
		} else {
			value = base64.StdEncoding.EncodeToString([]byte(value))
		}

		hubClient := hub.NewClient()
		if hubClient == nil || !hubClient.IsConfigured() {
			log.Error("Hub client not configured. Is SCION_HUB_ENDPOINT set?")
			os.Exit(1)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := hubClient.SetSecret(ctx, key, value, localType, localTarget, secretForce)
		if err != nil {
			log.Error("%v", err)
			os.Exit(1)
		}

		log.Info("Secret %q stored (scope: %s)", resp.Key, resp.Scope)
	},
}

var secretGetCmd = &cobra.Command{
	Use:   "get KEY",
	Short: "Retrieve a project-scoped secret via the Hub API",
	Long: `Retrieve a project-scoped secret from the Hub from within an agent container.

The decoded secret value is printed to stdout (suitable for piping).

Examples:
  # Retrieve a secret value
  sciontool secret get MY_API_KEY

  # Use in a script
  export API_KEY=$(sciontool secret get MY_API_KEY)`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		key := args[0]

		hubClient := hub.NewClient()
		if hubClient == nil || !hubClient.IsConfigured() {
			log.Error("Hub client not configured. Is SCION_HUB_ENDPOINT set?")
			os.Exit(1)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := hubClient.GetSecret(ctx, key)
		if err != nil {
			log.Error("%v", err)
			os.Exit(1)
		}

		// Decode the base64 value and write raw bytes to stdout.
		decoded, err := base64.StdEncoding.DecodeString(resp.Value)
		if err != nil {
			log.Error("Failed to decode secret value: %v", err)
			os.Exit(1)
		}

		if _, err := os.Stdout.Write(decoded); err != nil {
			log.Error("Failed to write secret value: %v", err)
			os.Exit(1)
		}
	},
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available project-scoped secrets via the Hub API",
	Long: `List metadata for all project-scoped secrets from within an agent container.

Only metadata (key, type, target) is returned — secret values are not included.

Examples:
  sciontool secret list`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		hubClient := hub.NewClient()
		if hubClient == nil || !hubClient.IsConfigured() {
			log.Error("Hub client not configured. Is SCION_HUB_ENDPOINT set?")
			os.Exit(1)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := hubClient.ListSecrets(ctx)
		if err != nil {
			log.Error("%v", err)
			os.Exit(1)
		}

		if len(resp.Secrets) == 0 {
			log.Info("No secrets found")
			return
		}

		// Print header and table using tabwriter for aligned columns.
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "KEY\tTYPE\tTARGET") //nolint:errcheck
		fmt.Fprintln(tw, "---\t----\t------") //nolint:errcheck
		for _, s := range resp.Secrets {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Key, s.Type, s.Target) //nolint:errcheck
		}
		if err := tw.Flush(); err != nil {
			log.Error("Failed to flush output: %v", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(secretCmd)
	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretGetCmd)
	secretCmd.AddCommand(secretListCmd)

	secretSetCmd.Flags().StringVar(&secretType, "type", "", "Secret type: environment (default), variable, file")
	secretSetCmd.Flags().StringVar(&secretTarget, "target", "", "Injection target path (defaults to key for env, required for file)")
	secretSetCmd.Flags().BoolVar(&secretForce, "force", false, "Overwrite existing secret")
}
