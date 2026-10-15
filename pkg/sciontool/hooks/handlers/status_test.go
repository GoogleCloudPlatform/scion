/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusHandler_UpdateActivity(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	h := &StatusHandler{
		StatusPath: statusPath,
	}

	// Test updating activity
	err := h.UpdateActivity(state.ActivityThinking, "")
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "thinking", info.Activity)

	// Test updating to sticky activity (waiting_for_input)
	err = h.UpdateActivity(state.ActivityWaitingForInput, "")
	require.NoError(t, err)

	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity)
}

func TestStatusHandler_UpdatePhase(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	h := &StatusHandler{StatusPath: statusPath}

	err := h.UpdatePhase(state.PhaseStarting, "", "")
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "starting", info.Phase)
	assert.Equal(t, "", info.Activity)

	err = h.UpdatePhase(state.PhaseRunning, state.ActivityWorking, "")
	require.NoError(t, err)

	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "running", info.Phase)
	assert.Equal(t, "working", info.Activity)
}

func TestStatusHandler_Handle(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	h := &StatusHandler{
		StatusPath: statusPath,
	}

	tests := []struct {
		name         string
		event        *hooks.Event
		wantPhase    string
		wantActivity string
	}{
		{
			name:         "PreStart sets starting phase",
			event:        &hooks.Event{Name: hooks.EventPreStart},
			wantPhase:    "starting",
			wantActivity: "",
		},
		{
			name:         "PostStart sets running/initializing",
			event:        &hooks.Event{Name: hooks.EventPostStart},
			wantPhase:    "running",
			wantActivity: "initializing",
		},
		{
			name:         "SessionStart sets working activity",
			event:        &hooks.Event{Name: hooks.EventSessionStart},
			wantActivity: "working",
		},
		{
			name:         "PreStop sets stopping phase",
			event:        &hooks.Event{Name: hooks.EventPreStop},
			wantPhase:    "stopping",
			wantActivity: "",
		},
		{
			name:         "PromptSubmit sets thinking",
			event:        &hooks.Event{Name: hooks.EventPromptSubmit},
			wantActivity: "thinking",
		},
		{
			name:         "ToolStart sets executing",
			event:        &hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "Bash"}},
			wantActivity: "executing",
		},
		{
			name:         "ToolEnd sets working",
			event:        &hooks.Event{Name: hooks.EventToolEnd},
			wantActivity: "working",
		},
		{
			name:         "AgentEnd sets working",
			event:        &hooks.Event{Name: hooks.EventAgentEnd},
			wantActivity: "working",
		},
		{
			name:         "SessionEnd sets stopped phase",
			event:        &hooks.Event{Name: hooks.EventSessionEnd},
			wantPhase:    "stopped",
			wantActivity: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.Handle(tt.event)
			require.NoError(t, err)

			info := readAgentInfo(t, statusPath)
			if tt.wantPhase != "" {
				assert.Equal(t, tt.wantPhase, info.Phase)
			}
			assert.Equal(t, tt.wantActivity, info.Activity)
		})
	}
}

func TestStatusHandler_ToolStartSetsToolName(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	err := h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "Bash"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "executing", info.Activity)
	assert.Equal(t, "Bash", info.ToolName)

	// Tool-end should clear toolName
	err = h.Handle(&hooks.Event{Name: hooks.EventToolEnd})
	require.NoError(t, err)

	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "working", info.Activity)
	assert.Equal(t, "", info.ToolName)
}

func TestStatusHandler_StickyWaitingClearedByToolStart(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	h := &StatusHandler{StatusPath: statusPath}

	// Set activity to waiting_for_input (sticky)
	err := h.UpdateActivity(state.ActivityWaitingForInput, "")
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity)

	// Tool-start should clear waiting_for_input (user has responded)
	err = h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "Bash"},
	})
	require.NoError(t, err)

	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "executing", info.Activity, "tool-start should clear waiting_for_input")
}

func TestStatusHandler_StickyCompletedNotClearedByToolStart(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	h := &StatusHandler{StatusPath: statusPath}

	// Set activity to completed (sticky)
	err := h.UpdateActivity(state.ActivityCompleted, "")
	require.NoError(t, err)

	// Tool-start should NOT clear completed
	err = h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "Bash"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "completed should not be cleared by tool-start")
}

func TestStatusHandler_Handle_ClearsWaitingOnActivity(t *testing.T) {
	activityEvents := []struct {
		name  string
		event *hooks.Event
	}{
		{
			name:  "ToolStart clears waiting",
			event: &hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "Bash"}},
		},
		{
			name:  "PromptSubmit clears waiting",
			event: &hooks.Event{Name: hooks.EventPromptSubmit},
		},
		{
			name:  "AgentStart clears waiting",
			event: &hooks.Event{Name: hooks.EventAgentStart},
		},
	}

	for _, tt := range activityEvents {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			statusPath := filepath.Join(tmpDir, "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}

			// Pre-set activity to waiting_for_input
			err := h.UpdateActivity(state.ActivityWaitingForInput, "")
			require.NoError(t, err)

			// Handle the activity event
			err = h.Handle(tt.event)
			require.NoError(t, err)

			info := readAgentInfo(t, statusPath)
			assert.NotEqual(t, "waiting_for_input", info.Activity, "waiting_for_input should be cleared")
		})
	}
}

func TestStatusHandler_Handle_DoesNotClearCompletedOnToolStart(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Pre-set activity to completed
	err := h.UpdateActivity(state.ActivityCompleted, "")
	require.NoError(t, err)

	// Handle a tool-start event — tools may fire after task_completed as wrap-up
	err = h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "Bash"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "completed should not be cleared by tool-start")
}

func TestStatusHandler_Handle_DoesNotClearCompletedOnAgentEnd(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Pre-set activity to completed
	err := h.UpdateActivity(state.ActivityCompleted, "")
	require.NoError(t, err)

	// Handle agent-end events — should not clear completed
	err = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "completed should not be cleared by agent-end")

	// Second agent-end (e.g., SubagentStop)
	err = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	require.NoError(t, err)

	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "completed should survive multiple agent-end events")
}

func TestStatusHandler_Handle_DoesNotClearCompletedOnToolEnd(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Pre-set activity to completed
	err := h.UpdateActivity(state.ActivityCompleted, "")
	require.NoError(t, err)

	// Handle tool-end event
	err = h.Handle(&hooks.Event{Name: hooks.EventToolEnd})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "completed should not be cleared by tool-end")
}

func TestStatusHandler_Handle_ClearsCompletedOnNewWork(t *testing.T) {
	newWorkEvents := []struct {
		name  string
		event *hooks.Event
	}{
		{
			name:  "PromptSubmit clears completed",
			event: &hooks.Event{Name: hooks.EventPromptSubmit},
		},
		{
			name:  "AgentStart clears completed",
			event: &hooks.Event{Name: hooks.EventAgentStart},
		},
		{
			name:  "SessionStart clears completed",
			event: &hooks.Event{Name: hooks.EventSessionStart},
		},
	}

	for _, tt := range newWorkEvents {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			statusPath := filepath.Join(tmpDir, "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}

			// Pre-set activity to completed
			err := h.UpdateActivity(state.ActivityCompleted, "")
			require.NoError(t, err)

			// Handle the new-work event
			err = h.Handle(tt.event)
			require.NoError(t, err)

			info := readAgentInfo(t, statusPath)
			assert.NotEqual(t, "completed", info.Activity, "completed should be cleared by new work event")
		})
	}
}

func TestStatusHandler_Handle_CompletedLifecycle(t *testing.T) {
	// Simulate the full lifecycle: task completes, wrap-up tools fire,
	// agent stops, then new prompt arrives.
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// 1. Agent completes task
	err := h.UpdateActivity(state.ActivityCompleted, "")
	require.NoError(t, err)
	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity)

	// 2. Wrap-up tool fires (e.g., TaskUpdate)
	err = h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "TaskUpdate"},
	})
	require.NoError(t, err)
	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "should survive tool-start")

	// 3. Tool completes
	err = h.Handle(&hooks.Event{Name: hooks.EventToolEnd})
	require.NoError(t, err)
	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "should survive tool-end")

	// 4. Agent turn ends (Stop event)
	err = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	require.NoError(t, err)
	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "should survive agent-end")

	// 5. Another Stop event (SubagentStop)
	err = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	require.NoError(t, err)
	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "should survive second agent-end")

	// 6. New prompt arrives — completed should now be cleared
	err = h.Handle(&hooks.Event{Name: hooks.EventPromptSubmit})
	require.NoError(t, err)
	info = readAgentInfo(t, statusPath)
	assert.NotEqual(t, "completed", info.Activity, "should be cleared by new prompt")
	assert.Equal(t, "thinking", info.Activity)
}

// An agent-set waiting_for_input (sciontool status ask_user writes it with
// UpdateActivity) is not cleared by tool-end. Only a waiting_for_input that
// a Notification set is; see status_waiting_clear_test.go.
func TestStatusHandler_Handle_ToolEndDoesNotClearWaiting(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Pre-set activity to waiting_for_input, as sciontool status ask_user does
	err := h.UpdateActivity(state.ActivityWaitingForInput, "")
	require.NoError(t, err)

	// Handle a tool-end event (should NOT clear)
	err = h.Handle(&hooks.Event{Name: hooks.EventToolEnd})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity, "tool-end should not clear waiting")
}

func TestStatusHandler_Handle_ClaudeExitPlanMode(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Handle ExitPlanMode tool-start from Claude dialect
	err := h.Handle(&hooks.Event{
		Name:    hooks.EventToolStart,
		Dialect: "claude",
		Data:    hooks.EventData{ToolName: "ExitPlanMode"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity)
}

func TestStatusHandler_Handle_ClaudeAskUserQuestion(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Pre-set activity to waiting_for_input (simulating sciontool status ask_user)
	err := h.UpdateActivity(state.ActivityWaitingForInput, "")
	require.NoError(t, err)

	// Handle AskUserQuestion tool-start from Claude dialect
	err = h.Handle(&hooks.Event{
		Name:    hooks.EventToolStart,
		Dialect: "claude",
		Data:    hooks.EventData{ToolName: "AskUserQuestion"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity, "AskUserQuestion should maintain waiting_for_input")
}

func TestStatusHandler_Handle_NonClaudeExitPlanModeIgnored(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Handle ExitPlanMode from a non-claude dialect — should NOT set waiting_for_input
	err := h.Handle(&hooks.Event{
		Name:    hooks.EventToolStart,
		Dialect: "gemini",
		Data:    hooks.EventData{ToolName: "ExitPlanMode"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "executing", info.Activity, "non-claude ExitPlanMode should set executing, not waiting_for_input")
}

func TestStatusHandler_Handle_ClaudeExitPlanModeThenActivity(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// ExitPlanMode sets waiting_for_input
	err := h.Handle(&hooks.Event{
		Name:    hooks.EventToolStart,
		Dialect: "claude",
		Data:    hooks.EventData{ToolName: "ExitPlanMode"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity)

	// Tool-end for ExitPlanMode should NOT clear it (sticky)
	err = h.Handle(&hooks.Event{Name: hooks.EventToolEnd, Dialect: "claude"})
	require.NoError(t, err)

	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity)

	// User approves plan, next tool starts — should clear waiting_for_input
	err = h.Handle(&hooks.Event{
		Name:    hooks.EventToolStart,
		Dialect: "claude",
		Data:    hooks.EventData{ToolName: "Bash"},
	})
	require.NoError(t, err)

	info = readAgentInfo(t, statusPath)
	assert.Equal(t, "executing", info.Activity, "activity after plan approval should clear waiting_for_input")
}

func TestStatusHandler_PreservesExtraFields(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	// Seed agent-info.json with extra fields (as written at provisioning time)
	initial := map[string]interface{}{
		"phase":         "running",
		"activity":      "working",
		"status":        "working",
		"template":      "my-template",
		"harnessConfig": "claude",
		"runtime":       "docker",
		"project":       "my-project",
		"profile":       "default",
		"name":          "agent-1",
	}
	data, err := json.Marshal(initial)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statusPath, data, 0644))

	h := &StatusHandler{StatusPath: statusPath}

	// Update activity — this should NOT destroy the extra fields
	err = h.UpdateActivity(state.ActivityThinking, "")
	require.NoError(t, err)

	result := readAgentInfoMap(t, statusPath)
	assert.Equal(t, "thinking", result["activity"])
	assert.Nil(t, result["status"], "legacy status field should be removed")
	assert.Equal(t, "my-template", result["template"], "template field should be preserved")
	assert.Equal(t, "claude", result["harnessConfig"], "harnessConfig field should be preserved")
	assert.Equal(t, "docker", result["runtime"], "runtime field should be preserved")
	assert.Equal(t, "my-project", result["project"], "project field should be preserved")
	assert.Equal(t, "default", result["profile"], "profile field should be preserved")
	assert.Equal(t, "agent-1", result["name"], "name field should be preserved")

	// Update to waiting_for_input — extra fields should still be there
	err = h.UpdateActivity(state.ActivityWaitingForInput, "")
	require.NoError(t, err)

	result = readAgentInfoMap(t, statusPath)
	assert.Equal(t, "waiting_for_input", result["activity"])
	assert.Nil(t, result["status"], "legacy status field should be removed")
	assert.Equal(t, "my-template", result["template"], "template field should survive activity update")
	assert.Equal(t, "claude", result["harnessConfig"], "harnessConfig field should survive activity update")
}

func TestStatusHandler_RemovesLegacyFields(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	// Seed agent-info.json with legacy status and sessionStatus fields
	initial := map[string]interface{}{
		"phase":         "running",
		"activity":      "working",
		"status":        "working",
		"sessionStatus": "WAITING_FOR_INPUT",
	}
	data, err := json.Marshal(initial)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statusPath, data, 0644))

	h := &StatusHandler{StatusPath: statusPath}

	// Any UpdateActivity call should remove the legacy status and sessionStatus fields
	err = h.UpdateActivity(state.ActivityThinking, "")
	require.NoError(t, err)

	result := readAgentInfoMap(t, statusPath)
	assert.Equal(t, "thinking", result["activity"])
	assert.Nil(t, result["status"], "legacy status should be removed")
	assert.Nil(t, result["sessionStatus"], "legacy sessionStatus should be removed")
}

func TestStatusHandler_LimitsExceededIsStickyAgainstToolStart(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Set activity to limits_exceeded (sticky)
	err := h.UpdateActivity(state.ActivityLimitsExceeded, "")
	require.NoError(t, err)

	// Tool-start should NOT clear limits_exceeded
	err = h.Handle(&hooks.Event{
		Name: hooks.EventToolStart,
		Data: hooks.EventData{ToolName: "Bash"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity, "limits_exceeded should not be cleared by tool-start")
}

func TestStatusHandler_LimitsExceededIsStickyAgainstToolEnd(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	err := h.UpdateActivity(state.ActivityLimitsExceeded, "")
	require.NoError(t, err)

	err = h.Handle(&hooks.Event{Name: hooks.EventToolEnd})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity, "limits_exceeded should not be cleared by tool-end")
}

func TestStatusHandler_LimitsExceededIsStickyAgainstAgentEnd(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	err := h.UpdateActivity(state.ActivityLimitsExceeded, "")
	require.NoError(t, err)

	err = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity, "limits_exceeded should not be cleared by agent-end")
}

func TestStatusHandler_LimitsExceededNotClearedByCompleted(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Set limits_exceeded
	err := h.UpdateActivity(state.ActivityLimitsExceeded, "")
	require.NoError(t, err)

	// tool-end/agent-end should not overwrite limits_exceeded
	err = h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity, "limits_exceeded should not be cleared by model-end")
}

func TestStatusHandler_LimitsExceededClearedByNewWork(t *testing.T) {
	newWorkEvents := []struct {
		name  string
		event *hooks.Event
	}{
		{
			name:  "PromptSubmit clears limits_exceeded",
			event: &hooks.Event{Name: hooks.EventPromptSubmit},
		},
		{
			name:  "AgentStart clears limits_exceeded",
			event: &hooks.Event{Name: hooks.EventAgentStart},
		},
		{
			name:  "SessionStart clears limits_exceeded",
			event: &hooks.Event{Name: hooks.EventSessionStart},
		},
	}

	for _, tt := range newWorkEvents {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			statusPath := filepath.Join(tmpDir, "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}

			// Pre-set activity to limits_exceeded
			err := h.UpdateActivity(state.ActivityLimitsExceeded, "")
			require.NoError(t, err)

			// Handle the new-work event
			err = h.Handle(tt.event)
			require.NoError(t, err)

			info := readAgentInfo(t, statusPath)
			assert.NotEqual(t, "limits_exceeded", info.Activity, "limits_exceeded should be cleared by new work event")
		})
	}
}

func TestStatusHandler_NotificationSetsWaitingForInput(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Handle notification event
	err := h.Handle(&hooks.Event{
		Name: hooks.EventNotification,
		Data: hooks.EventData{Message: "Please confirm"},
	})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "waiting_for_input", info.Activity, "notification should set waiting_for_input")
}

func TestStatusHandler_ResponseCompleteSetsCompleted(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	err := h.Handle(&hooks.Event{Name: hooks.EventResponseComplete, Dialect: "codex"})
	require.NoError(t, err)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "completed", info.Activity, "response-complete should set completed")
}

// agentInfoFields is a test-only struct for reading fields from agent-info.json.
type agentInfoFields struct {
	Phase    string `json:"phase,omitempty"`
	Activity string `json:"activity,omitempty"`
	ToolName string `json:"toolName,omitempty"`
}

// readAgentInfo is a test helper that reads and parses agent-info.json.
func readAgentInfo(t *testing.T, path string) agentInfoFields {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var info agentInfoFields
	err = json.Unmarshal(data, &info)
	require.NoError(t, err)
	return info
}

// readAgentInfoMap is a test helper that reads agent-info.json as a raw map.
func readAgentInfoMap(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var info map[string]interface{}
	err = json.Unmarshal(data, &info)
	require.NoError(t, err)
	return info
}

func TestStatusHandler_SetMessage(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")

	h := &StatusHandler{StatusPath: statusPath}

	// Set phase to error first
	err := h.UpdatePhase(state.PhaseError, "", "")
	require.NoError(t, err)

	// Set message
	err = h.SetMessage("git clone failed: authentication required")
	require.NoError(t, err)

	// Verify message is in detail
	info := readAgentInfoMap(t, statusPath)
	detail, ok := info["detail"].(map[string]interface{})
	require.True(t, ok, "expected detail map in agent-info.json")
	assert.Equal(t, "git clone failed: authentication required", detail["message"])
	assert.Equal(t, "error", info["phase"])

	// Clear message
	err = h.SetMessage("")
	require.NoError(t, err)

	info = readAgentInfoMap(t, statusPath)
	_, hasDetail := info["detail"]
	assert.False(t, hasDetail, "detail should be removed when message is cleared")
}

// TestStatusHandler_FifoAtStatusPathDoesNotHangUpdate proves a FIFO planted
// at agent-info.json (something the workload can always do, since it owns
// the containing directory) is refused immediately rather than blocking the
// caller. StatusHandler runs in root's own PID-1 process throughout the
// workload's lifetime, so a hang here would stall every root-side
// lifecycle control (heartbeat, shutdown, limits enforcement) — this test
// fails if the read is ever reverted to a plain os.ReadFile, or if the
// FIFO/non-regular-file refusal is dropped so the code proceeds to a
// blocking read.
func TestStatusHandler_FifoAtStatusPathDoesNotHangUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	if err := syscall.Mkfifo(statusPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	h := &StatusHandler{StatusPath: statusPath}

	done := make(chan error, 1)
	go func() { done <- h.UpdateActivity(state.ActivityThinking, "") }()

	select {
	case err := <-done:
		require.NoError(t, err, "UpdateActivity should treat a FIFO as absent state, not fail")
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateActivity blocked reading a FIFO planted at StatusPath")
	}
}

// TestStatusHandler_SymlinkAtStatusPathIsRefusedNotFollowed proves a
// symlink swapped in at agent-info.json — here pointing at a large file
// standing in for /dev/zero — is refused outright (the O_NOFOLLOW open
// itself fails on the symlink) rather than followed. This fails if the
// O_NOFOLLOW refusal is dropped (root would follow the symlink and, only
// then, run into whatever bound applies to the target).
func TestStatusHandler_SymlinkAtStatusPathIsRefusedNotFollowed(t *testing.T) {
	tmpDir := t.TempDir()
	big := filepath.Join(tmpDir, "huge")
	// One byte over agentInfoMaxBytes: enough to prove the bound is
	// enforced without actually allocating megabytes on every test run.
	if err := os.WriteFile(big, make([]byte, agentInfoMaxBytes+1), 0o600); err != nil {
		t.Fatalf("write huge file: %v", err)
	}
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	if err := os.Symlink(big, statusPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	h := &StatusHandler{StatusPath: statusPath}

	// A refusal to read a hostile StatusPath must not stop the handler from
	// working: it falls back to an empty prior-state map and still writes
	// its own update through the normal (non-symlink) path.
	err := h.UpdateActivity(state.ActivityThinking, "")
	require.NoError(t, err)

	// The symlink must not have been followed for the write either — the
	// huge file's content must be untouched, and a fresh regular file must
	// now sit at statusPath.
	hugeData, err := os.ReadFile(big)
	require.NoError(t, err)
	assert.Len(t, hugeData, agentInfoMaxBytes+1, "the huge file should be untouched")

	fi, err := os.Lstat(statusPath)
	require.NoError(t, err)
	assert.Zero(t, fi.Mode()&os.ModeSymlink, "statusPath should no longer be a symlink after a write")
}

// wantAgentInfoMaxBytes is this test's OWN, independently hardcoded copy of
// the size bound status.go documents for agentInfoMaxBytes (1 MiB) — not a
// reference to that constant. A test that sized its fixture as
// agentInfoMaxBytes+1 would track any future change to that constant
// automatically, including a regression that widens it: the fixture and the
// cap it's supposed to test would drift together, and the "one byte over"
// fixture would always be refused no matter how wide the cap became,
// silently proving nothing. Anchoring to an independent, hardcoded
// expectation of what the cap SHOULD be is what lets this test actually
// notice if agentInfoMaxBytes is ever widened.
const wantAgentInfoMaxBytes = 1 << 20

// jsonObjectOfSize returns a syntactically valid JSON object of exactly n
// bytes: {"k":"aaa...a"}, with the "a" run padded to make up the difference.
// n must be at least 8 (the length of `{"k":""}`).
func jsonObjectOfSize(n int) []byte {
	const prefix, suffix = `{"k":"`, `"}`
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		pad = 0
	}
	buf := make([]byte, 0, n)
	buf = append(buf, prefix...)
	for i := 0; i < pad; i++ {
		buf = append(buf, 'a')
	}
	buf = append(buf, suffix...)
	return buf
}

// TestStatusHandler_OversizeRegularStatusPathReturnsEmptyMap proves the size
// bound is enforced on an actual regular file (not a symlink to one): an
// agent-info.json one byte over agentInfoMaxBytes is refused, falling back
// to the same empty-map state a missing file already produces. The fixture
// is syntactically valid JSON that would unmarshal to a non-empty map if the
// cap did not refuse it first — a fixture built purely from NUL bytes would
// fail json.Unmarshal regardless of the cap, so it would pass this test even
// with the cap removed or widened to any size; this fixture only passes
// because the SIZE bound is what refuses it. A symlink to an oversize file
// (see the refusal test above) never even reaches the size check, since
// O_NOFOLLOW refuses it first.
func TestStatusHandler_OversizeRegularStatusPathReturnsEmptyMap(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	if err := os.WriteFile(statusPath, jsonObjectOfSize(wantAgentInfoMaxBytes+1), 0o600); err != nil {
		t.Fatalf("write oversize agent-info.json: %v", err)
	}

	h := &StatusHandler{StatusPath: statusPath}
	info := h.readAgentInfoMap()
	assert.Empty(t, info, "an oversize regular StatusPath should read back as empty state, not error out")
}

// TestStatusHandler_AtCapRegularStatusPathIsRead is
// OversizeRegularStatusPathReturnsEmptyMap's companion: the same valid-JSON
// shape at exactly agentInfoMaxBytes (not one byte over) is read and parsed
// successfully, proving the boundary sits exactly at the documented cap and
// that jsonObjectOfSize itself produces parseable content.
func TestStatusHandler_AtCapRegularStatusPathIsRead(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	if err := os.WriteFile(statusPath, jsonObjectOfSize(wantAgentInfoMaxBytes), 0o600); err != nil {
		t.Fatalf("write at-cap agent-info.json: %v", err)
	}

	h := &StatusHandler{StatusPath: statusPath}
	info := h.readAgentInfoMap()
	if got, ok := info["k"]; !ok || len(got.(string)) == 0 {
		t.Errorf("readAgentInfoMap() = %v, want the at-cap file's own \"k\" value parsed back", info)
	}
}

// TestStatusHandler_NonRegularStatusPathReturnsEmptyMap proves that reading
// a non-regular agent-info.json (a FIFO, standing in for "anything that
// isn't a plain file") produces the same safe empty-map fallback a missing
// file already produces, rather than an error that could propagate and
// disrupt the lifecycle handler calling it.
func TestStatusHandler_NonRegularStatusPathReturnsEmptyMap(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	if err := syscall.Mkfifo(statusPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	h := &StatusHandler{StatusPath: statusPath}
	info := h.readAgentInfoMap()
	assert.Empty(t, info, "a non-regular StatusPath should read back as empty state, not error out")
}

// TestStatusHandler_WriteUsesMode0644 proves the agent-info.json write still
// widens the file to mode 0644 (the broker reads it after the container
// exits, potentially as a different uid) now that the widening happens via
// fchmod on the temp file's fd instead of a path-based os.Chmod.
func TestStatusHandler_WriteUsesMode0644(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	require.NoError(t, h.UpdateActivity(state.ActivityThinking, ""))

	fi, err := os.Stat(statusPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), fi.Mode().Perm())
}

// Every readiness event must move the local activity off initializing, so
// `sciontool init` can use the local file to avoid overwriting a hook-set
// activity.
func TestStatusHandler_ReadinessEventsReplaceInitializing(t *testing.T) {
	events := map[string]string{
		hooks.EventSessionStart:     "working",
		hooks.EventPromptSubmit:     "thinking",
		hooks.EventAgentStart:       "thinking",
		hooks.EventModelStart:       "thinking",
		hooks.EventModelEnd:         "working",
		hooks.EventToolStart:        "executing",
		hooks.EventToolEnd:          "working",
		hooks.EventAgentEnd:         "working",
		hooks.EventNotification:     "waiting_for_input",
		hooks.EventResponseComplete: "completed",
	}
	for name, want := range events {
		t.Run(name, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventPostStart}))
			require.Equal(t, "initializing", readAgentInfo(t, statusPath).Activity)
			require.NoError(t, h.Handle(&hooks.Event{Name: name, Data: hooks.EventData{ToolName: "Bash"}}))
			assert.Equal(t, want, readAgentInfo(t, statusPath).Activity)
		})
	}
}

func TestStatusHandler_ReplaceActivityIf(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}
	require.NoError(t, h.UpdatePhase(state.PhaseRunning, state.ActivityInitializing, ""))

	swapped, err := h.ReplaceActivityIf(state.ActivityInitializing, state.ActivityWorking)
	require.NoError(t, err)
	assert.True(t, swapped)
	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "running", info.Phase)
	assert.Equal(t, "working", info.Activity)
	assert.Equal(t, state.ActivityWorking, h.CurrentActivity())

	// A hook-set activity is left alone.
	require.NoError(t, h.UpdateActivity(state.ActivityWaitingForInput, ""))
	swapped, err = h.ReplaceActivityIf(state.ActivityInitializing, state.ActivityWorking)
	require.NoError(t, err)
	assert.False(t, swapped)
	assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity)
}

// A clear or compact session-start restarts the session inside the running
// harness. It keeps blocked, completed, limits_exceeded, crashed and a
// waiting_for_input the agent set itself, writes nothing locally and sends
// no Hub update.
func TestSessionStart_InPlaceKeepsSticky(t *testing.T) {
	for _, source := range []string{"clear", "compact"} {
		for _, held := range []state.Activity{
			state.ActivityBlocked,
			state.ActivityCompleted,
			state.ActivityLimitsExceeded,
			state.ActivityCrashed,
			state.ActivityWaitingForInput, // agent-set: no notification marker
		} {
			t.Run(source+"/"+string(held), func(t *testing.T) {
				sh, hh, sent := hubRecorderSetup(t)
				require.NoError(t, sh.UpdateActivity(held, ""))
				before := readAgentInfoMap(t, sh.StatusPath)

				got, info := sessionStartStep(t, sh, hh, sent, claudeSessionStart(t, source))
				assert.Equal(t, before, info, "local state unchanged")
				assert.Equal(t, string(held), info["activity"])
				assert.NotContains(t, info, activitySourceKey)
				assert.Empty(t, got, "no Hub report")
			})
		}
	}
}

// A clear or compact during init says nothing about readiness:
// initializing is kept and no Hub update is sent.
func TestSessionStart_InPlaceKeepsInitializing(t *testing.T) {
	for _, source := range []string{"clear", "compact"} {
		t.Run(source, func(t *testing.T) {
			sh, hh, sent := hubRecorderSetup(t)
			require.NoError(t, sh.Handle(&hooks.Event{Name: hooks.EventPostStart}))
			before := readAgentInfoMap(t, sh.StatusPath)
			require.Equal(t, "initializing", before["activity"])

			got, info := sessionStartStep(t, sh, hh, sent, claudeSessionStart(t, source))
			assert.Equal(t, before, info, "local state unchanged")
			assert.Empty(t, got, "no Hub report")
		})
	}
}

// A clear or compact does not force working over a non-sticky activity.
func TestSessionStart_InPlaceDoesNotForceWorking(t *testing.T) {
	for _, source := range []string{"clear", "compact"} {
		for _, current := range []state.Activity{state.ActivityThinking, state.ActivityExecuting, state.ActivityWorking} {
			t.Run(source+"/"+string(current), func(t *testing.T) {
				sh, hh, sent := hubRecorderSetup(t)
				require.NoError(t, sh.UpdateActivity(current, ""))
				before := readAgentInfoMap(t, sh.StatusPath)

				got, info := sessionStartStep(t, sh, hh, sent, claudeSessionStart(t, source))
				assert.Equal(t, before, info, "local state unchanged")
				assert.Equal(t, string(current), info["activity"])
				assert.Empty(t, got, "no Hub report")
			})
		}
	}
}

// A clear or compact releases a notification-set waiting_for_input, with or
// without a sub-agent owner list: the local activity becomes working with
// the session-restart marker and no owner list, and the Hub gets
// running/working "Session restarted (<source>)".
func TestSessionStart_InPlaceReleasesNotificationWait(t *testing.T) {
	for _, source := range []string{"clear", "compact"} {
		for _, owners := range [][]string{nil, {"a0000000000000aa1", "a0000000000000bb2"}} {
			t.Run(fmt.Sprintf("%s/%d owners", source, len(owners)), func(t *testing.T) {
				sh, hh, sent := hubRecorderSetup(t)
				require.NoError(t, sh.setWaitingFromNotificationFor(""))
				for _, owner := range owners {
					require.NoError(t, sh.setWaitingFromNotificationFor(owner))
				}
				info := readAgentInfoMap(t, sh.StatusPath)
				require.Equal(t, activitySourceNotification, info[activitySourceKey])
				require.Equal(t, owners, notificationWaitOwners(info))

				got, info := sessionStartStep(t, sh, hh, sent, claudeSessionStart(t, source))
				assert.Equal(t, "working", info["activity"])
				assert.Equal(t, activitySourceSessionRestart, info[activitySourceKey])
				assert.NotContains(t, info, notificationWaitAgentsKey)
				assert.Equal(t, []hubReport{{Phase: "running", Activity: "working", Status: parentHeldWorking, Message: "Session restarted (" + source + ")"}}, got)
			})
		}
	}
}

// A clear or compact over a wait that sub-agents hold and that also records
// the parent's own question (parentWaitAgent) does not answer that question:
// the owner list, the notification marker and parentWait are removed, the
// parent's agent-set waiting_for_input is kept with no session-restart
// marker, and no Hub update is sent.
func TestSessionStart_InPlaceKeepsParentQuestionInOwnerHeldWait(t *testing.T) {
	for _, source := range []string{"clear", "compact"} {
		for _, owners := range [][]string{{"a0000000000000aa1"}, {"a0000000000000aa1", "a0000000000000bb2"}} {
			t.Run(fmt.Sprintf("%s/%d owners", source, len(owners)), func(t *testing.T) {
				sh, hh, sent := hubRecorderSetup(t)
				for _, owner := range owners {
					require.NoError(t, sh.setWaitingFromNotificationFor(owner))
				}
				require.NoError(t, sh.setParentQuestionWait())
				info := readAgentInfoMap(t, sh.StatusPath)
				require.Equal(t, activitySourceNotification, info[activitySourceKey])
				require.Equal(t, owners, notificationWaitOwners(info))
				require.Equal(t, parentWaitAgent, info[parentWaitKey])

				got, info := sessionStartStep(t, sh, hh, sent, claudeSessionStart(t, source))
				assert.Equal(t, "waiting_for_input", info["activity"], "the parent's question is still open")
				assert.NotContains(t, info, activitySourceKey, "an agent-set wait, no session-restart marker")
				assert.NotContains(t, info, notificationWaitAgentsKey)
				assert.NotContains(t, info, parentWaitKey)
				assert.Empty(t, got, "no Hub report")
			})
		}
	}
}

// Every other source (new, missing or unknown) behaves as before: any held
// status, including a notification wait with its owner list, becomes
// working with no marker, and the Hub gets running/working "Session
// started".
func TestSessionStart_NewSourcesUnchanged(t *testing.T) {
	for _, source := range []string{"startup", "resume", "new", "fork", "", "weird"} {
		for _, held := range []string{"blocked", "completed", "limits_exceeded", "crashed", "waiting_for_input", "notification", "initializing", "thinking"} {
			t.Run(source+"/"+held, func(t *testing.T) {
				sh, hh, sent := hubRecorderSetup(t)
				switch held {
				case "notification":
					require.NoError(t, sh.setWaitingFromNotificationFor("a0000000000000aa1"))
				case "initializing":
					require.NoError(t, sh.Handle(&hooks.Event{Name: hooks.EventPostStart}))
				default:
					require.NoError(t, sh.UpdateActivity(state.Activity(held), ""))
				}

				got, info := sessionStartStep(t, sh, hh, sent, claudeSessionStart(t, source))
				assert.Equal(t, "working", info["activity"])
				assert.NotContains(t, info, activitySourceKey)
				assert.NotContains(t, info, notificationWaitAgentsKey)
				assert.Equal(t, []hubReport{{Phase: "running", Activity: "working", Status: parentHeldWorking, Message: "Session started"}}, got)
			})
		}
	}
}

// A sub-agent's clear or compact session-start takes the held sub-agent
// path. Over a notification wait with no owner list it writes working, and
// the Hub follows with running/working "Session restarted (<source>)". Over
// a wait that a sibling sub-agent holds it changes nothing and sends no Hub
// update.
func TestSessionStart_InPlaceSubagentHubFollowsLocal(t *testing.T) {
	for _, source := range []string{"clear", "compact"} {
		t.Run(source+"/no-owner notification wait", func(t *testing.T) {
			sh, hh, sent := hubRecorderSetup(t)
			require.NoError(t, sh.setWaitingFromNotificationFor(""))

			got, info := sessionStartStep(t, sh, hh, sent, parentHeldSessionStart(t, source, parentHeldSubB))
			assert.Equal(t, "working", info["activity"])
			assert.NotContains(t, info, activitySourceKey)
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.Equal(t, []hubReport{{Phase: "running", Activity: "working", Status: parentHeldWorking, Message: "Session restarted (" + source + ")"}}, got)
		})
		t.Run(source+"/sibling-held wait", func(t *testing.T) {
			sh, hh, sent := hubRecorderSetup(t)
			require.NoError(t, sh.setWaitingFromNotificationFor(parentHeldSubA))
			before := readAgentInfoMap(t, sh.StatusPath)

			got, info := sessionStartStep(t, sh, hh, sent, parentHeldSessionStart(t, source, parentHeldSubB))
			assert.Equal(t, before, info, "local state unchanged")
			assert.Equal(t, []string{parentHeldSubA}, notificationWaitOwners(info))
			assert.Empty(t, got, "no Hub report")
		})
	}
}
