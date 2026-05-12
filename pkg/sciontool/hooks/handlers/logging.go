/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"fmt"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// LoggingHandler logs hook events to a file.
type LoggingHandler struct {
}

// NewLoggingHandler creates a new logging handler.
func NewLoggingHandler() *LoggingHandler {
	return &LoggingHandler{}
}

// Handle logs an event to the log file.
func (h *LoggingHandler) Handle(event *hooks.Event) error {
	tag := h.eventToTag(event)
	message := h.formatLogMessage(event)

	return h.LogEvent(tag, message)
}

// LogEvent writes a log entry to the agent log file.
func (h *LoggingHandler) LogEvent(tag string, message string) error {
	log.TaggedInfo(tag, "%s", message)
	return nil
}

// eventToTag maps normalized events to display tags for logging.
func (h *LoggingHandler) eventToTag(event *hooks.Event) string {
	switch event.Name {
	case hooks.EventSessionStart:
		return string(state.PhaseStarting)
	case hooks.EventPromptSubmit, hooks.EventAgentStart:
		return string(state.ActivityThinking)
	case hooks.EventModelStart:
		return string(state.ActivityThinking)
	case hooks.EventModelEnd:
		return string(state.ActivityWorking)
	case hooks.EventToolStart:
		return string(state.ActivityExecuting)
	case hooks.EventToolEnd, hooks.EventAgentEnd:
		return string(state.ActivityWorking)
	case hooks.EventNotification:
		return string(state.ActivityWaitingForInput)
	case hooks.EventResponseComplete:
		return string(state.ActivityCompleted)
	case hooks.EventSessionEnd:
		return string(state.PhaseStopped)
	case hooks.EventPreStart:
		return string(state.PhaseStarting)
	case hooks.EventPostStart:
		return string(state.ActivityWorking)
	case hooks.EventPreStop:
		return string(state.PhaseStopping)
	default:
		return string(state.ActivityWorking)
	}
}

// formatLogMessage creates a human-readable log message for an event.
func (h *LoggingHandler) formatLogMessage(event *hooks.Event) string {
	switch event.Name {
	case hooks.EventSessionStart:
		if event.Data.Source != "" {
			return fmt.Sprintf("Session started (source: %s)", event.Data.Source)
		}
		return "Session started"

	case hooks.EventSessionEnd:
		if event.Data.Reason != "" {
			return fmt.Sprintf("Session ended (reason: %s)", event.Data.Reason)
		}
		return "Session ended"

	case hooks.EventPromptSubmit:
		if event.Data.Prompt != "" {
			prompt := event.Data.Prompt
			if len(prompt) > 100 {
				prompt = prompt[:100] + "..."
			}
			return fmt.Sprintf("User prompt: %s", prompt)
		}
		return "User prompt submitted"

	case hooks.EventAgentStart:
		return "Agent turn started"

	case hooks.EventAgentEnd:
		return "Agent turn completed"

	case hooks.EventToolStart:
		if event.Data.ToolName != "" {
			return fmt.Sprintf("Running tool: %s", event.Data.ToolName)
		}
		return "Tool execution started"

	case hooks.EventToolEnd:
		if event.Data.ToolName != "" {
			return fmt.Sprintf("Tool %s completed", event.Data.ToolName)
		}
		return "Tool execution completed"

	case hooks.EventModelStart:
		return "LLM call started"

	case hooks.EventModelEnd:
		return "LLM call completed"

	case hooks.EventNotification:
		if event.Data.Message != "" {
			return fmt.Sprintf("Notification: %s", event.Data.Message)
		}
		return "Notification received"

	case hooks.EventResponseComplete:
		if event.Data.Message != "" {
			return fmt.Sprintf("Agent completed task: %s", event.Data.Message)
		}
		return "Agent completed task"

	case hooks.EventPreStart:
		return "Container initializing"

	case hooks.EventPostStart:
		return "Container ready"

	case hooks.EventPreStop:
		return "Container shutting down (received termination signal)"

	default:
		return fmt.Sprintf("Event: %s", event.RawName)
	}
}
