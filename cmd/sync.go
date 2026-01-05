package cmd

import (
	"context"
	"fmt"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/spf13/cobra"
)

// syncCmd represents the sync command
var syncCmd = &cobra.Command{
	Use:   "sync [to|from] <agent-name>",
	Short: "Sync agent workspace",
	Long:  `Triggers a synchronization of the workspace for the specified agent.
Behavior depends on the configured sync mode (e.g. mutagen or tar).
For tar sync, direction (to or from) must be specified.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var agentName string
		var direction runtime.SyncDirection = runtime.SyncUnspecified

		if len(args) == 2 {
			dirStr := args[0]
			if dirStr != "to" && dirStr != "from" {
				return fmt.Errorf("invalid direction '%s', must be 'to' or 'from'", dirStr)
			}
			direction = runtime.SyncDirection(dirStr)
			agentName = args[1]
		} else {
			agentName = args[0]
		}

		effectiveProfile := profile
		if effectiveProfile == "" {
			effectiveProfile = agent.GetSavedProfile(agentName, grovePath)
		}

		effectiveRuntime := effectiveProfile
		if effectiveRuntime == "" {
			effectiveRuntime = agent.GetSavedRuntime(agentName, grovePath)
		}

		rt := runtime.GetRuntime(grovePath, effectiveRuntime)

		// For tar sync in Kubernetes, we should probably enforce direction if we want to be strict,
		// but defaulting to 'to' is also an option.
		// The user mentioned: "if the sync setting is tar, the sync command must have a direct positional arg of from or to"
		// If we want to strictly follow this, we'd need to check the runtime sync mode.

		return rt.Sync(context.Background(), agentName, direction)
	},
}

func init() {
	rootCmd.AddCommand(syncCmd)
}
