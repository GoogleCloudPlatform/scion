/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

var (
	secretType         string
	secretTarget       string
	secretForce        bool
	secretScope        string
	secretAllowProgeny bool
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage agent secrets",
	Long: `Commands for managing secrets from within an agent container.

To read a secret value, use 'scion secret get --value'; to list secrets, use
'scion secret list'.`,
}

var secretSetCmd = &cobra.Command{
	Use:   "set KEY VALUE",
	Short: "Store a secret via the Hub API",
	Long: `Store a secret in the Hub from within an agent container.

By default, the secret is scoped to the current agent's project. Subsequent
agents in the same project will receive this secret automatically. Use
--scope=user to save a personal credential for your agents: a user-scope
secret waits for your approval in Scion before any agent receives it, and
--force has no effect on it.

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
  sciontool secret set MY_KEY "new-value" --force

  # Store a user-scoped (personal) secret
  sciontool secret set MY_TOKEN "tok-123" --scope user`,
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

		// Validate scope flag.
		if secretScope != "" && secretScope != "project" && secretScope != "user" {
			log.Error("--scope must be \"project\" or \"user\"")
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

		// Resolve allowProgeny. User-scoped captured secrets should always
		// enable progeny so spawned children inherit the credential. If the
		// caller didn't set --allow-progeny explicitly and the scope is user,
		// default to true; otherwise honour the flag value.
		var allowProgeny *bool
		if cmd.Flags().Changed("allow-progeny") {
			allowProgeny = &secretAllowProgeny
		} else if secretScope == "user" {
			t := true
			allowProgeny = &t
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := hubClient.SetSecret(ctx, key, value, localType, localTarget, secretScope, secretForce, allowProgeny)
		if err != nil {
			log.Error("%v", err)
			os.Exit(1)
		}

		reportSetSecretResult(cmd.OutOrStdout(), resp)
	},
}

// reportSetSecretResult reports a successful save. A pending result is
// written to w directly, whatever the log level, because callers such as
// capture-auth read it to tell the user that the secret waits for
// approval in Scion.
func reportSetSecretResult(w io.Writer, resp *hub.SetSecretResponse) {
	if resp.Pending() {
		msg := fmt.Sprintf("Secret %q saved, pending approval in Scion (scope: %s)", resp.Key, resp.Scope)
		if resp.ReplacesExisting {
			msg += "; it replaces your existing secret once approved"
		}
		_, _ = fmt.Fprintln(w, msg)
		return
	}
	log.Info("Secret %q stored (scope: %s)", resp.Key, resp.Scope)
}

func init() {
	rootCmd.AddCommand(secretCmd)
	secretCmd.AddCommand(secretSetCmd)
	// get and list moved to the scion CLI (ptone/scion#5013); set stays
	// for the harness capture and provisioning scripts.
	secretCmd.AddCommand(movedSubcommand("get", "secret get", "secret get --value"))
	secretCmd.AddCommand(movedSubcommand("list", "secret list", "secret list"))

	secretSetCmd.Flags().StringVar(&secretType, "type", "", "Secret type: environment (default), variable, file")
	secretSetCmd.Flags().StringVar(&secretTarget, "target", "", "Injection target path (defaults to key for env, required for file)")
	secretSetCmd.Flags().BoolVar(&secretForce, "force", false, "Overwrite existing secret")
	secretSetCmd.Flags().StringVar(&secretScope, "scope", "", "Secret scope: project (default) or user")
	secretSetCmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "Allow progeny agents to inherit this secret (user scope only)")
}
