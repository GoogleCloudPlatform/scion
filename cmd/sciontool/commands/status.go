/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
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
  task_completed   Signal that the agent has completed its task
  limits_exceeded  Signal that the agent has exceeded its configured limits

Examples:
  # Signal waiting for user input
  sciontool status ask_user "What should I do next?"

  # Signal blocked waiting for a child agent
  sciontool status blocked "Waiting for agent deploy-frontend to complete"

  # Signal task completion
  sciontool status task_completed "Implemented feature X"

  # Signal limits exceeded
  sciontool status limits_exceeded "max_turns of 50 exceeded"`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		statusType := args[0]
		message := strings.Join(args[1:], " ")

		definition, ok := statusDefinitions[statusType]
		if !ok {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: unknown status type %q\n", statusType)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Valid types: ask_user, blocked, task_completed, limits_exceeded\n")
			cmd.Root().SetArgs([]string{"status", "--help"})
			_ = cmd.Root().Execute()
			return
		}
		if message == "" {
			message = definition.defaultMessage
		}
		runStatusUpdate(definition, message)
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

type statusDefinition struct {
	activity       state.Activity
	defaultMessage string
	eventPrefix    string
	outputPrefix   string
	useTaskSummary bool
}

var statusDefinitions = map[string]statusDefinition{
	"ask_user": {
		activity:       state.ActivityWaitingForInput,
		defaultMessage: "Input requested",
		eventPrefix:    "Agent requested input",
		outputPrefix:   "Agent asked",
	},
	"blocked": {
		activity:       state.ActivityBlocked,
		defaultMessage: "Agent is blocked",
		eventPrefix:    "Agent blocked",
		outputPrefix:   "Agent blocked",
	},
	"task_completed": {
		activity:       state.ActivityCompleted,
		defaultMessage: "Task completed",
		eventPrefix:    "Agent completed task",
		outputPrefix:   "Agent completed",
		useTaskSummary: true,
	},
	"limits_exceeded": {
		activity:       state.ActivityLimitsExceeded,
		defaultMessage: "Agent limits exceeded",
		eventPrefix:    "Agent limits exceeded",
		outputPrefix:   "Agent limits exceeded",
	},
}

func runStatusUpdate(definition statusDefinition, message string) {
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()

	if err := statusHandler.UpdateActivity(definition.activity, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	logMessage := fmt.Sprintf("%s: %s", definition.eventPrefix, message)
	if err := loggingHandler.LogEvent(string(definition.activity), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hubClient.UpdateStatus(ctx, definition.hubUpdate(message)); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	log.Info("%s: %s", definition.outputPrefix, message)
}

func (definition statusDefinition) hubUpdate(message string) hub.StatusUpdate {
	agentState := state.AgentState{Phase: state.PhaseRunning, Activity: definition.activity}
	update := hub.StatusUpdate{
		Activity: definition.activity,
		Status:   agentState.DisplayStatus(),
	}
	if definition.useTaskSummary {
		update.TaskSummary = message
	} else {
		update.Message = message
	}
	return update
}
