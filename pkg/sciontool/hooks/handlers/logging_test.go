/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoggingHandler_LogEvent(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "agent.log")
	log.SetLogPath(logPath)

	h := &LoggingHandler{}

	err := h.LogEvent("thinking", "Test message")
	require.NoError(t, err)

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "[sciontool]")
	assert.Contains(t, content, "[thinking]")
	assert.Contains(t, content, "Test message")
}

func TestLoggingHandler_Handle(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "agent.log")
	log.SetLogPath(logPath)

	h := &LoggingHandler{}

	tests := []struct {
		name        string
		event       *hooks.Event
		wantContain string
	}{
		{
			name:        "SessionStart",
			event:       &hooks.Event{Name: hooks.EventSessionStart, Data: hooks.EventData{Source: "cli"}},
			wantContain: "Session started (source: cli)",
		},
		{
			name:        "SessionEnd",
			event:       &hooks.Event{Name: hooks.EventSessionEnd, Data: hooks.EventData{Reason: "complete"}},
			wantContain: "Session ended (reason: complete)",
		},
		{
			name:        "PreStart",
			event:       &hooks.Event{Name: hooks.EventPreStart},
			wantContain: "Container initializing",
		},
		{
			name:        "PostStart",
			event:       &hooks.Event{Name: hooks.EventPostStart},
			wantContain: "Container ready",
		},
		{
			name:        "PreStop",
			event:       &hooks.Event{Name: hooks.EventPreStop},
			wantContain: "Container shutting down",
		},
		{
			name:        "ToolStart",
			event:       &hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "Bash"}},
			wantContain: "Running tool: Bash",
		},
		{
			name:        "ToolEnd",
			event:       &hooks.Event{Name: hooks.EventToolEnd, Data: hooks.EventData{ToolName: "Read"}},
			wantContain: "Tool Read completed",
		},
		{
			name:        "PromptSubmit",
			event:       &hooks.Event{Name: hooks.EventPromptSubmit, Data: hooks.EventData{Prompt: "Help me"}},
			wantContain: "User prompt: Help me",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear log file
			_ = os.WriteFile(logPath, []byte{}, 0644)

			err := h.Handle(tt.event)
			require.NoError(t, err)

			data, err := os.ReadFile(logPath)
			require.NoError(t, err)

			content := string(data)
			assert.True(t, strings.Contains(content, tt.wantContain),
				"log should contain %q, got: %s", tt.wantContain, content)
			assert.Contains(t, content, "[sciontool]")
		})
	}
}

func TestLoggingHandler_LongPromptTruncation(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "agent.log")
	log.SetLogPath(logPath)

	h := &LoggingHandler{}

	// Create a very long prompt
	longPrompt := strings.Repeat("a", 200)
	event := &hooks.Event{
		Name: hooks.EventPromptSubmit,
		Data: hooks.EventData{Prompt: longPrompt},
	}

	err := h.Handle(event)
	require.NoError(t, err)

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "...")
	// The full 200-char prompt should not be in the log
	assert.NotContains(t, content, longPrompt)
}
