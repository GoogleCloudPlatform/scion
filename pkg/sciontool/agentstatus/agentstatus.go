/*
Copyright 2025 The Scion Authors.
*/

// Package agentstatus is the agent status update behind
// `sciontool status <type> <message>`: the local agent state write (sticky
// for waiting_for_input), the event log line and the report to the Hub.
// The scion CLI runs the same path after sending a choice prompt from an
// agent (the ask_user side effect), so both binaries share one
// implementation. The local update is unix-only (agentstatus_unix.go); the
// Windows build of the scion CLI links agentstatus_other.go, which refuses
// it.
package agentstatus

import (
	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
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
