/*
Copyright 2025 The Scion Authors.
*/

// Package agentstatus is the agent status update behind
// `sciontool status <type> <message>`: the local agent state write (sticky
// for waiting_for_input), the event log line and the report to the Hub.
// The scion CLI runs the same path after sending a choice prompt from an
// agent (the ask_user side effect), so both binaries share one
// implementation.
package agentstatus

import (
	"context"
	"fmt"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Status types.
const (
	AskUser        = "ask_user"
	Blocked        = "blocked"
	TaskCompleted  = "task_completed"
	LimitsExceeded = "limits_exceeded"
)

// Definition describes one status type.
type Definition struct {
	Activity       state.Activity
	DefaultMessage string
	eventPrefix    string
	outputPrefix   string
	useTaskSummary bool
}

var definitions = map[string]Definition{
	AskUser: {
		Activity:       state.ActivityWaitingForInput,
		DefaultMessage: "Input requested",
		eventPrefix:    "Agent requested input",
		outputPrefix:   "Agent asked",
	},
	Blocked: {
		Activity:       state.ActivityBlocked,
		DefaultMessage: "Agent is blocked",
		eventPrefix:    "Agent blocked",
		outputPrefix:   "Agent blocked",
	},
	TaskCompleted: {
		Activity:       state.ActivityCompleted,
		DefaultMessage: "Task completed",
		eventPrefix:    "Agent completed task",
		outputPrefix:   "Agent completed",
		useTaskSummary: true,
	},
	LimitsExceeded: {
		Activity:       state.ActivityLimitsExceeded,
		DefaultMessage: "Agent limits exceeded",
		eventPrefix:    "Agent limits exceeded",
		outputPrefix:   "Agent limits exceeded",
	},
}

// Lookup returns the definition of a status type.
func Lookup(statusType string) (Definition, bool) {
	d, ok := definitions[statusType]
	return d, ok
}

// Run performs the status update: the local activity (handlers keep
// waiting_for_input sticky through the next tool-end hook), the event log
// line, and the Hub report when a Hub is configured. Failures are logged,
// never returned: a status update never fails its caller.
func Run(definition Definition, message string) {
	if message == "" {
		message = definition.DefaultMessage
	}
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()

	if err := statusHandler.UpdateActivity(definition.Activity, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	logMessage := fmt.Sprintf("%s: %s", definition.eventPrefix, message)
	if err := loggingHandler.LogEvent(string(definition.Activity), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hubClient.UpdateStatus(ctx, definition.HubUpdate(message)); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	log.Info("%s: %s", definition.outputPrefix, message)
}

// RunAskUser performs the ask_user status update with message, exactly as
// `sciontool status ask_user "<message>"` does.
func RunAskUser(message string) {
	Run(definitions[AskUser], message)
}

// HubUpdate is the status report sent to the Hub for message.
func (definition Definition) HubUpdate(message string) hub.StatusUpdate {
	agentState := state.AgentState{Phase: state.PhaseRunning, Activity: definition.Activity}
	update := hub.StatusUpdate{
		Activity: definition.Activity,
		Status:   agentState.DisplayStatus(),
	}
	if definition.useTaskSummary {
		update.TaskSummary = message
	} else {
		update.Message = message
	}
	return update
}
