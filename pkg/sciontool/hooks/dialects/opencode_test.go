/*
Copyright 2025 The Scion Authors.
*/

package dialects

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeDialect_Name(t *testing.T) {
	d := NewOpenCodeDialect()
	assert.Equal(t, "opencode", d.Name())
}

func TestOpenCodeDialect_NestedFormat(t *testing.T) {
	d := NewOpenCodeDialect()

	tests := []struct {
		rawName  string
		wantName string
		payload  map[string]interface{}
	}{
		{
			rawName:  "tool-start",
			wantName: hooks.EventToolStart,
			payload:  map[string]interface{}{"tool_name": "Bash"},
		},
		{
			rawName:  "tool-end",
			wantName: hooks.EventToolEnd,
			payload:  map[string]interface{}{"tool_name": "Bash", "success": true},
		},
		{
			rawName:  "session-start",
			wantName: hooks.EventSessionStart,
			payload:  map[string]interface{}{"session_id": "abc123"},
		},
		{
			rawName:  "session-end",
			wantName: hooks.EventSessionEnd,
			payload:  map[string]interface{}{"reason": "user-requested"},
		},
		{
			rawName:  "prompt-submit",
			wantName: hooks.EventPromptSubmit,
			payload:  map[string]interface{}{"prompt": "Fix the bug"},
		},
		{
			rawName:  "notification",
			wantName: hooks.EventNotification,
			payload:  map[string]interface{}{"message": "Need input"},
		},
		{
			rawName:  "model-start",
			wantName: hooks.EventModelStart,
			payload:  map[string]interface{}{},
		},
		{
			rawName:  "model-end",
			wantName: hooks.EventModelEnd,
			payload:  map[string]interface{}{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.rawName, func(t *testing.T) {
			data := map[string]interface{}{
				"name": tt.rawName,
				"data": tt.payload,
			}
			event, err := d.Parse(data)
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, event.Name)
			assert.Equal(t, "opencode", event.Dialect)
		})
	}
}

func TestOpenCodeDialect_FlatFormat(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"name":      "tool-start",
		"tool_name": "Read",
		"file_path": "/src/main.go",
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, hooks.EventToolStart, event.Name)
	assert.Equal(t, "Read", event.Data.ToolName)
	assert.Equal(t, "/src/main.go", event.Data.FilePath)
}

func TestOpenCodeDialect_ToolSuccessError(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"name": "tool-end",
		"data": map[string]interface{}{
			"tool_name": "Bash",
			"success":   false,
			"error":     "exit status 1",
		},
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.False(t, event.Data.Success)
	assert.Equal(t, "exit status 1", event.Data.Error)
	assert.Equal(t, "Bash", event.Data.ToolName)
}

func TestOpenCodeDialect_ToolInputOutput(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"name":        "tool-end",
		"tool_name":   "Bash",
		"tool_input":  "ls -la",
		"tool_output": "total 42\n...",
		"success":     true,
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, "ls -la", event.Data.ToolInput)
	assert.Equal(t, "total 42\n...", event.Data.ToolOutput)
}

func TestOpenCodeDialect_SessionEndAssistantText(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"name": "session-end",
		"data": map[string]interface{}{
			"assistant_text": "I fixed the bug in main.go",
			"reason":         "user-requested",
		},
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, hooks.EventSessionEnd, event.Name)
	assert.Equal(t, "I fixed the bug in main.go", event.Data.AssistantText)
}

func TestOpenCodeDialect_Heartbeat(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"name":             "model-end",
		"_scion_heartbeat": true,
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, hooks.EventModelEnd, event.Name)
	assert.NotNil(t, event.Data.Raw)
	isHB, ok := event.Data.Raw["_scion_heartbeat"].(bool)
	assert.True(t, ok)
	assert.True(t, isHB)
}

func TestOpenCodeDialect_ActivityControlEvent(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"name":    "_activity",
		"message": "thinking hard",
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, "", event.Name)
	assert.Equal(t, "opencode", event.Dialect)
}

func TestOpenCodeDialect_HookEventNameFallback(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"hook_event_name": "session-start",
		"session_id":      "xyz",
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, hooks.EventSessionStart, event.Name)
}

func TestOpenCodeDialect_TokenExtraction(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"name": "model-end",
		"data": map[string]interface{}{
			"input_tokens":  float64(500),
			"output_tokens": float64(150),
		},
	}

	event, err := d.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, int64(500), event.Data.InputTokens)
	assert.Equal(t, int64(150), event.Data.OutputTokens)
}

func TestOpenCodeDialect_MissingEventName(t *testing.T) {
	d := NewOpenCodeDialect()

	data := map[string]interface{}{
		"tool_name": "Bash",
	}

	_, err := d.Parse(data)
	assert.Error(t, err)
}
