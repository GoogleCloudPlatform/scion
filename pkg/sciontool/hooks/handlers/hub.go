/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// HubHandler sends status updates to the Scion Hub.
type HubHandler struct {
	client *hub.Client
}

// NewHubHandler creates a new hub handler.
// Returns nil if the Hub client is not configured.
func NewHubHandler() *HubHandler {
	client := hub.NewClient()
	if client == nil || !client.IsConfigured() {
		return nil
	}
	return &HubHandler{
		client: client,
	}
}

// Handle processes an event and sends a status update to the Hub.
// It mirrors the sticky activity logic from StatusHandler: when the local activity
// is waiting_for_input or completed, non-new-work events won't overwrite it.
func (h *HubHandler) Handle(event *hooks.Event) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var err error
	switch event.Name {
	case hooks.EventSessionStart:
		// Session starting - report running phase with working activity (clears any sticky)
		log.Debug("Hub: Reporting running/working (session start)")
		err = h.client.ReportState(ctx, state.PhaseRunning, state.ActivityWorking, "Session started")

	case hooks.EventPromptSubmit, hooks.EventAgentStart:
		// New work events - always clear sticky status
		message := "Processing"
		if event.Data.Prompt != "" {
			message = truncateMessage(event.Data.Prompt, 100)
		}
		log.Debug("Hub: Reporting thinking")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityThinking}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityThinking,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventModelStart:
		// Model start - report thinking, but respect sticky activity
		if h.isLocalActivitySticky() {
			log.Debug("Hub: Skipping thinking (local activity is sticky)")
			return nil
		}
		message := "Processing"
		if event.Data.Prompt != "" {
			message = truncateMessage(event.Data.Prompt, 100)
		}
		log.Debug("Hub: Reporting thinking (model-start)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityThinking}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityThinking,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventToolStart:
		// Claude-specific: ExitPlanMode and AskUserQuestion mean waiting for user
		if event.Dialect == "claude" && (event.Data.ToolName == "ExitPlanMode" || event.Data.ToolName == "AskUserQuestion") {
			message := "Waiting for input"
			if event.Data.ToolName == "ExitPlanMode" {
				message = "Waiting for plan approval"
			}
			log.Debug("Hub: Reporting waiting_for_input (waiting: %s)", event.Data.ToolName)
			as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
			err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
				Activity: state.ActivityWaitingForInput,
				Status:   as.DisplayStatus(),
				Message:  message,
			})
			break
		}

		// Tool-start clears waiting_for_input (user has responded) but
		// preserves completed (tools may fire after task_completed as wrap-up).
		localActivity := readLocalActivity()
		if localActivity == string(state.ActivityCompleted) || localActivity == string(state.ActivityLimitsExceeded) {
			log.Debug("Hub: Skipping executing (completed is sticky, post-completion tool)")
			return nil
		}

		// Agent is executing a tool
		message := "Executing tool"
		if event.Data.ToolName != "" {
			message = "Executing: " + event.Data.ToolName
		}
		log.Debug("Hub: Reporting executing (tool: %s)", event.Data.ToolName)
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityExecuting}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityExecuting,
			ToolName: event.Data.ToolName,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventToolEnd, hooks.EventAgentEnd, hooks.EventModelEnd:
		// Forward assistant text (when the dialect extracted it — e.g.
		// Claude's Stop hook via transcript_path) to the hub message
		// store as an outbound agent→user reply. This is what makes
		// assistant responses show up in the Messages tab. Best-effort:
		// failure here must not break the status update flow below.
		if event.Name == hooks.EventAgentEnd && event.Data.AssistantText != "" {
			text := event.Data.AssistantText
			// Guard against very large assistant responses (e.g. full file
			// dumps) bloating the message store and slowing the Messages tab.
			const maxAssistantTextBytes = 64 * 1024
			if len(text) > maxAssistantTextBytes {
				text = text[:maxAssistantTextBytes] + "\n[truncated]"
			}

			msgCtx, msgCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer msgCancel()
			if msgErr := h.client.SendOutboundMessage(msgCtx, hub.OutboundMessage{
				Msg:  text,
				Type: "assistant-reply",
			}); msgErr != nil {
				log.Error("Hub: outbound assistant reply failed: %v", msgErr)
			} else {
				log.Debug("Hub: Forwarded assistant reply to message store (%d bytes)", len(text))
			}
		}

		// Check if local activity is sticky before sending working
		if h.isLocalActivitySticky() {
			log.Debug("Hub: Skipping working (local activity is sticky)")
			return nil
		}
		log.Debug("Hub: Reporting working (step completed)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWorking}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityWorking,
			Status:   as.DisplayStatus(),
			Message:  "Ready",
		})

	case hooks.EventNotification:
		// Agent is waiting for input
		message := "Waiting for input"
		if event.Data.Message != "" {
			message = truncateMessage(event.Data.Message, 100)
		}
		log.Debug("Hub: Reporting waiting_for_input (notification)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityWaitingForInput,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventResponseComplete:
		summary := "Task completed"
		if event.Data.Message != "" {
			summary = truncateMessage(event.Data.Message, 200)
		}
		log.Debug("Hub: Reporting task completed (response-complete)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityCompleted}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity:    state.ActivityCompleted,
			Status:      as.DisplayStatus(),
			TaskSummary: summary,
		})

	case hooks.EventSessionEnd:
		// Session ended
		log.Debug("Hub: Reporting stopped (session end)")
		as := state.AgentState{Phase: state.PhaseStopped}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Phase:   state.PhaseStopped,
			Status:  as.DisplayStatus(),
			Message: "Session ended",
		})

	default:
		// No status update for this event
		return nil
	}

	if err != nil {
		log.Error("Hub status update failed: %v", err)
		// Don't return error - we don't want Hub failures to break the hook chain
	} else {
		log.Debug("Hub status update sent successfully")
	}

	return nil
}

// isLocalActivitySticky reads the local agent-info.json (written by StatusHandler
// which runs before HubHandler) and returns true if the activity is sticky.
func (h *HubHandler) isLocalActivitySticky() bool {
	activity := readLocalActivity()
	return isStickyActivity(activity)
}

// readLocalActivity reads the current activity from the local agent-info.json file.
func readLocalActivity() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}
	statusPath := filepath.Join(home, "agent-info.json")

	data, err := os.ReadFile(statusPath)
	if err != nil {
		return ""
	}

	var info map[string]interface{}
	if err := json.Unmarshal(data, &info); err != nil {
		return ""
	}

	activity, _ := info["activity"].(string)
	return activity
}

// ReportWaitingForInput sends a waiting-for-input status to the Hub.
func (h *HubHandler) ReportWaitingForInput(message string) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting waiting_for_input (ask_user: %s)", truncateMessage(message, 50))
	as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		Activity: state.ActivityWaitingForInput,
		Status:   as.DisplayStatus(),
		Message:  message,
	})
}

// ReportTaskCompleted sends a task-completed status to the Hub.
func (h *HubHandler) ReportTaskCompleted(taskSummary string) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting task completed: %s", truncateMessage(taskSummary, 50))
	as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityCompleted}
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		Activity:    state.ActivityCompleted,
		Status:      as.DisplayStatus(),
		TaskSummary: taskSummary,
	})
}

// ReportLimitsExceeded sends a limits-exceeded status to the Hub.
func (h *HubHandler) ReportLimitsExceeded(message string) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting limits_exceeded: %s", truncateMessage(message, 50))
	as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityLimitsExceeded}
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		Activity: state.ActivityLimitsExceeded,
		Status:   as.DisplayStatus(),
		Message:  message,
	})
}

// ReportCounts sends current turn and model call counts to the Hub.
func (h *HubHandler) ReportCounts(turnCount, modelCallCount int) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting counts (turns=%d, model_calls=%d)", turnCount, modelCallCount)
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		CurrentTurns:      &turnCount,
		CurrentModelCalls: &modelCallCount,
	})
}

// truncateMessage truncates a message to the specified length.
func truncateMessage(msg string, maxLen int) string {
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen-3] + "..."
}
