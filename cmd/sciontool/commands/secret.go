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

const maxSecretValueBytes = 64 * 1024

var (
	secretType         string
	secretTarget       string
	secretForce        bool
	secretScope        string
	secretAllowProgeny bool
	secretDeleteScope  string
)

type secretHubClient interface {
	IsConfigured() bool
	SetSecret(ctx context.Context, key, value, secretType, target, scope string, force bool, allowProgeny *bool) (*hub.SetSecretResponse, error)
	DeleteSecret(ctx context.Context, key, scope string) error
}

var newSecretHubClient = func() secretHubClient {
	c := hub.NewClient()
	if c == nil {
		return nil
	}
	return c
}

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
--force has no effect on it. Use --scope=agent to store a secret private to
the current agent (requires the hub.agent_secrets experiment).

If VALUE is "-", the secret value is read from stdin (up to 64 KiB) so it
stays out of process listings. For non-file types, a single trailing newline
is trimmed from stdin.

If VALUE starts with @, the remainder is treated as a file path. The file
contents are read and base64-encoded, and --type defaults to "file".

Examples:
  # Store a simple environment variable secret
  sciontool secret set MY_API_KEY "sk-abc123"

  # Store an agent-scoped secret from stdin (avoids argv exposure)
  printf '%s' "$TOKEN" | sciontool secret set MY_AGENT_TOKEN - --scope agent

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
		if err := runSecretSet(cmd, args); err != nil {
			log.Error("%v", err)
			os.Exit(1)
		}
	},
}

func runSecretSet(cmd *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}
	if strings.ContainsAny(key, "= \t\n") {
		return fmt.Errorf("key cannot contain spaces, tabs, newlines, or '='")
	}

	// Validate scope flag.
	if secretScope != "" && secretScope != "project" && secretScope != "user" && secretScope != "agent" {
		return fmt.Errorf("--scope must be \"project\", \"user\", or \"agent\"")
	}

	localType := secretType
	localTarget := secretTarget

	switch {
	case value == "-":
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxSecretValueBytes+1))
		if err != nil {
			return fmt.Errorf("failed to read secret value from stdin: %w", err)
		}
		if len(data) > maxSecretValueBytes {
			return fmt.Errorf("secret value from stdin exceeds 64KB limit")
		}
		if localType != "file" {
			s := string(data)
			if strings.HasSuffix(s, "\r\n") {
				s = strings.TrimSuffix(s, "\r\n")
			} else {
				s = strings.TrimSuffix(s, "\n")
			}
			data = []byte(s)
		}
		if len(data) == 0 {
			return fmt.Errorf("secret value from stdin is empty")
		}
		value = base64.StdEncoding.EncodeToString(data)

	case strings.HasPrefix(value, "@"):
		// Handle @file syntax: read file and base64-encode contents.
		filePath := value[1:]
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
		if info.Size() > maxSecretValueBytes {
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
		if localTarget == "" {
			absPath, err := filepath.Abs(filePath)
			if err != nil {
				return fmt.Errorf("failed to resolve absolute path for %s: %w", filePath, err)
			}
			home, err := os.UserHomeDir()
			if err == nil && strings.HasPrefix(absPath, home+"/") {
				localTarget = "~/" + absPath[len(home)+1:]
			} else {
				localTarget = absPath
			}
		}

	default:
		if len(value) > maxSecretValueBytes {
			return fmt.Errorf("secret value exceeds 64KB limit")
		}
		value = base64.StdEncoding.EncodeToString([]byte(value))
	}

	hubClient := newSecretHubClient()
	if hubClient == nil || !hubClient.IsConfigured() {
		return fmt.Errorf("hub client not configured. Is SCION_HUB_ENDPOINT set?")
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
		return err
	}

	reportSetSecretResult(cmd.OutOrStdout(), resp)
	return nil
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete KEY",
	Short: "Delete an agent-scoped secret via the Hub API",
	Long: `Delete an agent-scoped secret belonging to the current agent.

Only agent-scoped secrets (--scope agent) may be deleted by an agent, and
this requires the hub.agent_secrets experiment to be enabled on the Hub.

Examples:
  sciontool secret delete MY_AGENT_TOKEN --scope agent`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if err := runSecretDelete(cmd, args); err != nil {
			log.Error("%v", err)
			os.Exit(1)
		}
	},
}

func runSecretDelete(_ *cobra.Command, args []string) error {
	key := args[0]

	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}
	if strings.ContainsAny(key, "= \t\n") {
		return fmt.Errorf("key cannot contain spaces, tabs, newlines, or '='")
	}
	if secretDeleteScope != "agent" {
		return fmt.Errorf("--scope must be \"agent\"")
	}

	hubClient := newSecretHubClient()
	if hubClient == nil || !hubClient.IsConfigured() {
		return fmt.Errorf("hub client not configured. Is SCION_HUB_ENDPOINT set?")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := hubClient.DeleteSecret(ctx, key, secretDeleteScope); err != nil {
		return err
	}

	log.Info("Secret %q deleted (scope: %s)", key, secretDeleteScope)
	return nil
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
	secretCmd.AddCommand(secretDeleteCmd)
	// get and list moved to the scion CLI (ptone/scion#5013); set stays
	// for the harness capture and provisioning scripts.
	secretCmd.AddCommand(movedSubcommand("get", "secret get", "secret get --value"))
	secretCmd.AddCommand(movedSubcommand("list", "secret list", "secret list"))

	secretSetCmd.Flags().StringVar(&secretType, "type", "", "Secret type: environment (default), variable, file")
	secretSetCmd.Flags().StringVar(&secretTarget, "target", "", "Injection target path (defaults to key for env, required for file)")
	secretSetCmd.Flags().BoolVar(&secretForce, "force", false, "Overwrite existing secret")
	secretSetCmd.Flags().StringVar(&secretScope, "scope", "", "Secret scope: project (default), user, or agent")
	secretSetCmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "Allow progeny agents to inherit this secret (user scope only)")

	secretDeleteCmd.Flags().StringVar(&secretDeleteScope, "scope", "agent", "Secret scope: agent")
}
