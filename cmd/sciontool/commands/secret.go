/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

var secretDeleteScope string

type secretHubClient interface {
	IsConfigured() bool
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

To read, list, or store secrets (including on_demand secrets, which are never
injected into the container at start), use 'scion secret get --value KEY',
'scion secret list', and 'scion secret set'.`,
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

func init() {
	rootCmd.AddCommand(secretCmd)
	secretCmd.AddCommand(secretDeleteCmd)
	// get, list, and set moved to the scion CLI (ptone/scion#5013, ptone/scion#9297).
	secretCmd.AddCommand(movedSubcommand("get", "secret get", "secret get --value"))
	secretCmd.AddCommand(movedSubcommand("list", "secret list", "secret list"))
	secretCmd.AddCommand(movedSubcommand("set", "secret set", "secret set"))

	secretDeleteCmd.Flags().StringVar(&secretDeleteScope, "scope", "agent", "Secret scope: agent")
}
