/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/agentstatus"
)

// statusCmd represents the status command
var statusCmd = &cobra.Command{
	Use:   "status <status-type> <message>",
	Short: "Update agent status",
	Long: `The status command updates the agent's session status and logs the event.

This is used by agents to signal state changes to the scion orchestrator.

Status Types:
  ask_user         Signal that the agent is waiting for user input
  blocked          Signal that the agent is intentionally waiting (e.g. for a child agent or scheduled event)
  idle             Signal that the agent has nothing to do until new work arrives
  task_completed   Signal that the agent has completed its task
  limits_exceeded  Signal that the agent has exceeded its configured limits

Waiting on a specific event (a child agent, a schedule, your own background
job) is blocked; nothing to do until a new message or task arrives is idle;
finishing an assigned task is task_completed.

Examples:
  # Signal waiting for user input
  sciontool status ask_user "What should I do next?"

  # Signal blocked waiting for a child agent
  sciontool status blocked "Waiting for agent deploy-frontend to complete"

  # Signal idle with no work pending
  sciontool status idle "Answered the question; no task assigned"

  # Signal task completion
  sciontool status task_completed "Implemented feature X"

  # Signal limits exceeded
  sciontool status limits_exceeded "max_turns of 50 exceeded"`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		statusType := args[0]
		message := strings.Join(args[1:], " ")

		definition, ok := agentstatus.Lookup(statusType)
		if !ok {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: unknown status type %q\n", statusType)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Valid types: ask_user, blocked, idle, task_completed, limits_exceeded\n")
			cmd.Root().SetArgs([]string{"status", "--help"})
			_ = cmd.Root().Execute()
			return
		}
		agentstatus.Run(definition, message)
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
