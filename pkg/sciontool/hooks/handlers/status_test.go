/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
			name:         "PostStart sets running/working",
			event:        &hooks.Event{Name: hooks.EventPostStart},
			wantPhase:    "running",
			wantActivity: "working",
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

func TestStatusHandler_Handle_ToolEndDoesNotClearWaiting(t *testing.T) {
	tmpDir := t.TempDir()
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Pre-set activity to waiting_for_input
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
		"grove":         "my-grove",
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
	assert.Equal(t, "my-grove", result["grove"], "grove field should be preserved")
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
