/*
Copyright 2025 The Scion Authors.
*/

// Package handlers provides hook handler implementations.
package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// StatusHandler manages agent status in a JSON file.
type StatusHandler struct {
	// StatusPath is the path to the agent-info.json file.
	StatusPath string

	mu sync.Mutex
}

// NewStatusHandler creates a new status handler.
func NewStatusHandler() *StatusHandler {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}
	return &StatusHandler{
		StatusPath: filepath.Join(home, "agent-info.json"),
	}
}

// isStickyActivity returns true if the given activity is a "sticky" value that
// should resist being overwritten by normal event-driven updates.
func isStickyActivity(activity string) bool {
	return state.Activity(activity).IsSticky()
}

// eventResult holds the result of mapping an event to phase/activity.
type eventResult struct {
	phase    state.Phase
	activity state.Activity
	isPhase  bool // true if this is a phase-level change
}

// Handle processes an event and updates the agent status.
func (h *StatusHandler) Handle(event *hooks.Event) error {
	result := eventToPhaseActivity(event)
	if result == nil {
		return nil // Event doesn't trigger a state change
	}

	// Phase-level changes (pre-start, post-start, pre-stop, session-end)
	if result.isPhase {
		return h.UpdatePhase(result.phase, result.activity, "")
	}

	// New work events (prompt-submit, agent-start, session-start): always
	// update activity unconditionally — clears any sticky state.
	if isNewWorkEvent(event.Name) {
		return h.UpdateActivity(result.activity, "")
	}

	// Tool-start events require special handling for sticky states.
	if event.Name == hooks.EventToolStart {
		// Claude-specific: ExitPlanMode and AskUserQuestion set waiting_for_input (sticky).
		if event.Dialect == "claude" && (event.Data.ToolName == "ExitPlanMode" || event.Data.ToolName == "AskUserQuestion") {
			return h.UpdateActivity(state.ActivityWaitingForInput, "")
		}

		// Tool-start clears waiting_for_input (user has responded) but
		// preserves completed (tools may fire after task_completed as wrap-up).
		return h.updateActivityIfNotSticky(result.activity, event.Data.ToolName, true)
	}

	// Notification event: set waiting_for_input directly (sticky).
	if event.Name == hooks.EventNotification {
		return h.UpdateActivity(state.ActivityWaitingForInput, "")
	}

	// All other events (tool-end, agent-end, model-end, etc.): update activity
	// only if current activity is not sticky.
	return h.updateActivityIfNotSticky(result.activity, "", false)
}

// UpdateActivity writes the activity to the agent-info.json file atomically.
// Used for runtime activity changes (most events). Preserves the current phase.
func (h *StatusHandler) UpdateActivity(activity state.Activity, toolName string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

	// Update the activity field
	if activity == "" {
		delete(info, "activity")
	} else {
		info["activity"] = string(activity)
	}

	// Set or clear toolName
	if activity == state.ActivityExecuting && toolName != "" {
		info["toolName"] = toolName
	} else {
		delete(info, "toolName")
	}

	// Remove legacy fields if present
	delete(info, "status")
	delete(info, "sessionStatus")

	return h.writeAgentInfoLocked(info)
}

// UpdatePhase writes both phase and activity to the agent-info.json file atomically.
// Used for lifecycle phase changes (pre-start, post-start, pre-stop, session-end).
func (h *StatusHandler) UpdatePhase(phase state.Phase, activity state.Activity, toolName string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

	// Update phase
	info["phase"] = string(phase)

	// Update activity
	if activity == "" {
		delete(info, "activity")
	} else {
		info["activity"] = string(activity)
	}

	// Set or clear toolName
	if activity == state.ActivityExecuting && toolName != "" {
		info["toolName"] = toolName
	} else {
		delete(info, "toolName")
	}

	// Remove legacy fields if present
	delete(info, "status")
	delete(info, "sessionStatus")

	return h.writeAgentInfoLocked(info)
}

// SetMessage writes a message to the agent-info.json detail section.
// This is used to persist error messages so the broker heartbeat can
// relay them to the hub even after the container exits.
func (h *StatusHandler) SetMessage(message string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

	detail, _ := info["detail"].(map[string]interface{})
	if detail == nil {
		detail = make(map[string]interface{})
	}
	if message != "" {
		detail["message"] = message
	} else {
		delete(detail, "message")
	}
	if len(detail) > 0 {
		info["detail"] = detail
	} else {
		delete(info, "detail")
	}

	return h.writeAgentInfoLocked(info)
}

// updateActivityIfNotSticky updates the activity only if the current activity is not
// sticky. If clearWaiting is true, waiting_for_input is also cleared (treated
// as non-sticky for tool-start events where the user has responded).
func (h *StatusHandler) updateActivityIfNotSticky(activity state.Activity, toolName string, clearWaiting bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	info := h.readAgentInfoMap()

	currentActivity, _ := info["activity"].(string)

	if isStickyActivity(currentActivity) {
		if clearWaiting && currentActivity == string(state.ActivityWaitingForInput) {
			// waiting_for_input is cleared by tool-start (user has responded)
		} else {
			return nil // Activity is sticky, don't overwrite
		}
	}

	// Update activity
	info["activity"] = string(activity)

	// Set or clear toolName
	if activity == state.ActivityExecuting && toolName != "" {
		info["toolName"] = toolName
	} else {
		delete(info, "toolName")
	}

	// Remove legacy fields if present
	delete(info, "status")
	delete(info, "sessionStatus")

	return h.writeAgentInfoLocked(info)
}

// readAgentInfoMap reads agent-info.json into a generic map, preserving all fields.
// Caller must hold h.mu.
func (h *StatusHandler) readAgentInfoMap() map[string]interface{} {
	info := make(map[string]interface{})
	if data, err := os.ReadFile(h.StatusPath); err == nil {
		_ = json.Unmarshal(data, &info)
	}
	return info
}

// writeAgentInfoLocked writes the agent info map to disk atomically.
// Caller must hold h.mu.
func (h *StatusHandler) writeAgentInfoLocked(info map[string]interface{}) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling status: %w", err)
	}

	dir := filepath.Dir(h.StatusPath)
	tmpFile, err := os.CreateTemp(dir, "agent-info-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	tmpFile.Close()

	// CreateTemp uses mode 0600. Widen to 0644 so the broker process
	// (which may run as a different uid than the container init) can
	// read and converge the file after the container exits.
	os.Chmod(tmpPath, 0644) //nolint:errcheck

	if err := os.Rename(tmpPath, h.StatusPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}

	return nil
}

// isNewWorkEvent returns true for events that indicate new work is starting.
// These events unconditionally update activity, clearing any sticky state.
func isNewWorkEvent(name string) bool {
	switch name {
	case hooks.EventPromptSubmit, hooks.EventAgentStart, hooks.EventSessionStart:
		return true
	}
	return false
}

// eventToPhaseActivity maps normalized events to phase/activity pairs.
// Returns nil if the event doesn't trigger a state change.
func eventToPhaseActivity(event *hooks.Event) *eventResult {
	switch event.Name {
	case hooks.EventPreStart:
		return &eventResult{phase: state.PhaseStarting, isPhase: true}

	case hooks.EventPostStart:
		return &eventResult{phase: state.PhaseRunning, activity: state.ActivityWorking, isPhase: true}

	case hooks.EventSessionStart:
		// session-start clears sticky — treated as activity-only (working)
		return &eventResult{activity: state.ActivityWorking}

	case hooks.EventPromptSubmit, hooks.EventAgentStart:
		return &eventResult{activity: state.ActivityThinking}

	case hooks.EventModelStart:
		return &eventResult{activity: state.ActivityThinking}

	case hooks.EventModelEnd:
		return &eventResult{activity: state.ActivityWorking}

	case hooks.EventToolStart:
		return &eventResult{activity: state.ActivityExecuting}

	case hooks.EventToolEnd, hooks.EventAgentEnd:
		return &eventResult{activity: state.ActivityWorking}

	case hooks.EventResponseComplete:
		return &eventResult{activity: state.ActivityCompleted}

	case hooks.EventNotification:
		return &eventResult{activity: state.ActivityWaitingForInput}

	case hooks.EventPreStop:
		return &eventResult{phase: state.PhaseStopping, isPhase: true}

	case hooks.EventSessionEnd:
		return &eventResult{phase: state.PhaseStopped, isPhase: true}

	default:
		return nil
	}
}

// GetFormattedState returns the state with tool name if applicable.
func (h *StatusHandler) GetFormattedState(event *hooks.Event) string {
	result := eventToPhaseActivity(event)
	if result == nil {
		return ""
	}
	if result.activity == state.ActivityExecuting && event.Data.ToolName != "" {
		return fmt.Sprintf("%s (%s)", result.activity, event.Data.ToolName)
	}
	if result.isPhase {
		return string(result.phase)
	}
	return string(result.activity)
}
