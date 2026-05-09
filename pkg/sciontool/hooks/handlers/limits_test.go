/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitLimitsFile(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 50, 200)
	require.NoError(t, err)

	data, err := os.ReadFile(limitsPath)
	require.NoError(t, err)

	var ls LimitsState
	err = json.Unmarshal(data, &ls)
	require.NoError(t, err)

	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
	assert.Equal(t, 50, ls.MaxTurns)
	assert.Equal(t, 200, ls.MaxModelCalls)
	assert.NotEmpty(t, ls.StartedAt)
}

func TestInitLimitsFile_ZeroValues(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 0, 0)
	require.NoError(t, err)

	data, err := os.ReadFile(limitsPath)
	require.NoError(t, err)

	var ls LimitsState
	err = json.Unmarshal(data, &ls)
	require.NoError(t, err)

	assert.Equal(t, 0, ls.MaxTurns)
	assert.Equal(t, 0, ls.MaxModelCalls)
}

func TestLimitsHandler_TurnCounting(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	// Initialize the limits file
	err := InitLimitsFile(limitsPath, 5, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      5,
		maxModelCalls: 0,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Simulate 3 turns
	for i := 0; i < 3; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
		require.NoError(t, err)
	}

	// Check the count
	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 3, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
}

func TestLimitsHandler_ModelCallCounting(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 0, 10)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      0,
		maxModelCalls: 10,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Simulate 5 model calls
	for i := 0; i < 5; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 5, ls.ModelCallCount)
}

func TestLimitsHandler_IgnoresIrrelevantEvents(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 10, 10)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      10,
		maxModelCalls: 10,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// These events should not change any counters
	events := []string{
		hooks.EventToolStart,
		hooks.EventToolEnd,
		hooks.EventSessionStart,
		hooks.EventPromptSubmit,
		hooks.EventModelStart,
	}

	for _, eventName := range events {
		err := h.Handle(&hooks.Event{Name: eventName})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
}

func TestLimitsHandler_NilHandler(t *testing.T) {
	var h *LimitsHandler
	err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	assert.NoError(t, err)
}

func TestLimitsHandler_NoLimitsConfigured(t *testing.T) {
	// When maxTurns=0 and maxModelCalls=0, events are silently ignored
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 0, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      0,
		maxModelCalls: 0,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// agent-end with no max_turns should be ignored
	err = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	require.NoError(t, err)

	// model-end with no max_model_calls should be ignored
	err = h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
	require.NoError(t, err)

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 0, ls.TurnCount)
	assert.Equal(t, 0, ls.ModelCallCount)
}

func TestLimitsHandler_TurnLimitDetection(t *testing.T) {
	// Test that the handler detects when the turn limit is reached.
	// We can't test the SIGUSR1 signal in unit tests (it would kill the test process),
	// so we verify the status file is updated and the limits file has the right count.
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	triggerPath := filepath.Join(tmpDir, "scion-limits-exceeded")

	err := InitLimitsFile(limitsPath, 3, 0)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:        3,
		maxModelCalls:   0,
		limitsPath:      limitsPath,
		triggerFilePath: triggerPath,
		statusHandler:   &StatusHandler{StatusPath: statusPath},
	}

	// Simulate 2 turns (under limit)
	for i := 0; i < 2; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 2, ls.TurnCount)

	// The 3rd turn hits the limit - this will attempt SIGUSR1 to PID 1
	// which won't work in tests (PID 1 is the test runner's init), but
	// the status update and file write should succeed.
	_ = h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})

	ls = readLimitsFile(t, limitsPath)
	assert.Equal(t, 3, ls.TurnCount)

	// Verify the agent-info.json was updated to limits_exceeded
	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity)
}

func TestLimitsHandler_ModelCallLimitDetection(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	statusPath := filepath.Join(tmpDir, "agent-info.json")
	triggerPath := filepath.Join(tmpDir, "scion-limits-exceeded")

	err := InitLimitsFile(limitsPath, 0, 2)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:        0,
		maxModelCalls:   2,
		limitsPath:      limitsPath,
		triggerFilePath: triggerPath,
		statusHandler:   &StatusHandler{StatusPath: statusPath},
	}

	// First model call - under limit
	err = h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
	require.NoError(t, err)

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 1, ls.ModelCallCount)

	// Second model call hits the limit
	_ = h.Handle(&hooks.Event{Name: hooks.EventModelEnd})

	ls = readLimitsFile(t, limitsPath)
	assert.Equal(t, 2, ls.ModelCallCount)

	info := readAgentInfo(t, statusPath)
	assert.Equal(t, "limits_exceeded", info.Activity)
}

func TestLimitsHandler_BothLimitsIndependent(t *testing.T) {
	// Verify both counters are tracked independently
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	err := InitLimitsFile(limitsPath, 100, 100)
	require.NoError(t, err)

	h := &LimitsHandler{
		maxTurns:      100,
		maxModelCalls: 100,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Simulate 5 model calls
	for i := 0; i < 5; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventModelEnd})
		require.NoError(t, err)
	}

	// Simulate 2 turns
	for i := 0; i < 2; i++ {
		err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
		require.NoError(t, err)
	}

	ls := readLimitsFile(t, limitsPath)
	assert.Equal(t, 2, ls.TurnCount)
	assert.Equal(t, 5, ls.ModelCallCount)
}

func TestLimitsHandler_MissingLimitsFile(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "nonexistent", "agent-limits.json")

	h := &LimitsHandler{
		maxTurns:      10,
		maxModelCalls: 0,
		limitsPath:    limitsPath,
		statusHandler: &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}

	// Should not error - just logs and returns nil
	err := h.Handle(&hooks.Event{Name: hooks.EventAgentEnd})
	assert.NoError(t, err)
}

func TestNewLimitsHandler_NoLimits(t *testing.T) {
	// Ensure NewLimitsHandlerWithPath returns nil when no limits are configured
	h := NewLimitsHandlerWithPath(0, 0, "/tmp/test-limits.json")
	assert.Nil(t, h)
}

func TestNewLimitsHandler_WithTurnsOnly(t *testing.T) {
	h := NewLimitsHandlerWithPath(10, 0, "/tmp/test-limits.json")
	assert.NotNil(t, h)
	assert.Equal(t, 10, h.maxTurns)
	assert.Equal(t, 0, h.maxModelCalls)
}

func TestNewLimitsHandler_WithModelCallsOnly(t *testing.T) {
	h := NewLimitsHandlerWithPath(0, 50, "/tmp/test-limits.json")
	assert.NotNil(t, h)
	assert.Equal(t, 0, h.maxTurns)
	assert.Equal(t, 50, h.maxModelCalls)
}

func TestParseEnvInt(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
		want int
	}{
		{"valid integer", "TEST_INT_1", "42", 42},
		{"zero", "TEST_INT_2", "0", 0},
		{"empty string", "TEST_INT_3", "", 0},
		{"invalid string", "TEST_INT_4", "abc", 0},
		{"negative", "TEST_INT_5", "-1", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.val != "" {
				t.Setenv(tt.key, tt.val)
			}
			got := ParseEnvInt(tt.key)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExitCodeLimitsExceeded(t *testing.T) {
	assert.Equal(t, 10, ExitCodeLimitsExceeded)
}

func TestWriteLimitsState_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	ls := &LimitsState{
		TurnCount:      5,
		ModelCallCount: 12,
		MaxTurns:       50,
		MaxModelCalls:  200,
		StartedAt:      "2026-02-22T10:30:00Z",
	}

	err := writeLimitsState(limitsPath, ls)
	require.NoError(t, err)

	// Read and verify
	data, err := os.ReadFile(limitsPath)
	require.NoError(t, err)

	var read LimitsState
	err = json.Unmarshal(data, &read)
	require.NoError(t, err)

	assert.Equal(t, 5, read.TurnCount)
	assert.Equal(t, 12, read.ModelCallCount)
	assert.Equal(t, 50, read.MaxTurns)
	assert.Equal(t, 200, read.MaxModelCalls)
	assert.Equal(t, "2026-02-22T10:30:00Z", read.StartedAt)
}

func TestLimitsTriggerFileConstant(t *testing.T) {
	assert.Equal(t, "/tmp/scion-limits-exceeded", LimitsTriggerFile)
}

func TestSignalLimitsExceeded_CreatesTriggerFile(t *testing.T) {
	tmpDir := t.TempDir()
	triggerPath := filepath.Join(tmpDir, "scion-limits-exceeded")

	h := &LimitsHandler{
		triggerFilePath: triggerPath,
	}

	err := h.signalLimitsExceeded()
	assert.NoError(t, err)

	// Verify the trigger file was created
	_, err = os.Stat(triggerPath)
	assert.NoError(t, err, "trigger file should exist after signalLimitsExceeded")
}

// readLimitsFile reads and parses an agent-limits.json file for test assertions.
func readLimitsFile(t *testing.T, path string) LimitsState {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var ls LimitsState
	err = json.Unmarshal(data, &ls)
	require.NoError(t, err)
	return ls
}
