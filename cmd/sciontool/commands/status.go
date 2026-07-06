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

		switch statusType {
		case "ask_user":
			if message == "" {
				message = "Input requested"
			}
			runStatusAskUser(message)
		case "blocked":
			if message == "" {
				message = "Agent is blocked"
			}
			runStatusBlocked(message)
		case "task_completed":
			if message == "" {
				message = "Task completed"
			}
			runStatusTaskCompleted(message)
		case "limits_exceeded":
			if message == "" {
				message = "Agent limits exceeded"
			}
			runStatusLimitsExceeded(message)
		default:
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: unknown status type %q\n", statusType)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Valid types: ask_user, blocked, task_completed, limits_exceeded\n")
			cmd.Root().SetArgs([]string{"status", "--help"})
			_ = cmd.Root().Execute()
		}
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

// runStatusAskUser updates status to waiting for input.
func runStatusAskUser(message string) {
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()

	// Update activity to waiting_for_input (sticky)
	if err := statusHandler.UpdateActivity(state.ActivityWaitingForInput, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	// Log the event
	logMessage := fmt.Sprintf("Agent requested input: %s", message)
	if err := loggingHandler.LogEvent(string(state.ActivityWaitingForInput), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	// Report to Hub if configured. The status update triggers the notification
	// system, which handles both the notification tray and inbox message creation.
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
		if err := hubClient.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityWaitingForInput,
			Status:   as.DisplayStatus(),
			Message:  message,
		}); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	log.Info("Agent asked: %s", message)
}

// runStatusBlocked updates status to blocked (agent is intentionally waiting).
func runStatusBlocked(message string) {
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()

	// Update activity to blocked (sticky)
	if err := statusHandler.UpdateActivity(state.ActivityBlocked, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	// Log the event
	logMessage := fmt.Sprintf("Agent blocked: %s", message)
	if err := loggingHandler.LogEvent(string(state.ActivityBlocked), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	// Report to Hub if configured
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityBlocked}
		if err := hubClient.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityBlocked,
			Status:   as.DisplayStatus(),
			Message:  message,
		}); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	log.Info("Agent blocked: %s", message)
}

// runStatusLimitsExceeded updates status to limits exceeded.
func runStatusLimitsExceeded(message string) {
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()

	// Update activity to limits_exceeded (sticky)
	if err := statusHandler.UpdateActivity(state.ActivityLimitsExceeded, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	// Log the event
	logMessage := fmt.Sprintf("Agent limits exceeded: %s", message)
	if err := loggingHandler.LogEvent(string(state.ActivityLimitsExceeded), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	// Report to Hub if configured
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityLimitsExceeded}
		if err := hubClient.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityLimitsExceeded,
			Status:   as.DisplayStatus(),
			Message:  message,
		}); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	log.Info("Agent limits exceeded: %s", message)
}

// runStatusTaskCompleted updates status to completed.
func runStatusTaskCompleted(message string) {
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()

	// Update activity to completed (sticky)
	if err := statusHandler.UpdateActivity(state.ActivityCompleted, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	// Log the event
	logMessage := fmt.Sprintf("Agent completed task: %s", message)
	if err := loggingHandler.LogEvent(string(state.ActivityCompleted), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	// Report to Hub if in hosted mode
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityCompleted}
		if err := hubClient.UpdateStatus(ctx, hub.StatusUpdate{
			Activity:    state.ActivityCompleted,
			Status:      as.DisplayStatus(),
			TaskSummary: message,
		}); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	log.Info("Agent completed: %s", message)
}
