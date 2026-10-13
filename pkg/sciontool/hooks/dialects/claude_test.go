/*
Copyright 2025 The Scion Authors.
*/

package dialects

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeDialect_Name(t *testing.T) {
	d := NewClaudeDialect()
	assert.Equal(t, "claude", d.Name())
}

func TestClaudeDialect_Parse(t *testing.T) {
	tests := []struct {
		name       string
		input      map[string]interface{}
		wantName   string
		wantTool   string
		wantPrompt string
		wantError  string
	}{
		{
			name: "PreToolUse event",
			input: map[string]interface{}{
				"hook_event_name": "PreToolUse",
				"tool_name":       "Bash",
			},
			wantName: hooks.EventToolStart,
			wantTool: "Bash",
		},
		{
			name: "PostToolUse event",
			input: map[string]interface{}{
				"hook_event_name": "PostToolUse",
				"tool_name":       "Read",
			},
			wantName: hooks.EventToolEnd,
			wantTool: "Read",
		},
		{
			// Claude Code sends a failed tool call as PostToolUseFailure,
			// with the failure text in a string "error" field.
			name: "PostToolUseFailure event",
			input: map[string]interface{}{
				"hook_event_name": "PostToolUseFailure",
				"session_id":      "00000000-0000-0000-0000-000000000000",
				"tool_name":       "Bash",
				"tool_input":      map[string]interface{}{"command": "false"},
				"tool_use_id":     "toolu_synthetic",
				"error":           "Command exited with non-zero status code 1",
				"is_interrupt":    false,
			},
			wantName:  hooks.EventToolEnd,
			wantTool:  "Bash",
			wantError: "Command exited with non-zero status code 1",
		},
		{
			name: "SessionStart event",
			input: map[string]interface{}{
				"hook_event_name": "SessionStart",
				"source":          "cli",
			},
			wantName: hooks.EventSessionStart,
		},
		{
			name: "SessionEnd event",
			input: map[string]interface{}{
				"hook_event_name": "SessionEnd",
				"reason":          "user_exit",
			},
			wantName: hooks.EventSessionEnd,
		},
		{
			name: "UserPromptSubmit event",
			input: map[string]interface{}{
				"hook_event_name": "UserPromptSubmit",
				"prompt":          "Help me write tests",
			},
			wantName:   hooks.EventPromptSubmit,
			wantPrompt: "Help me write tests",
		},
		{
			name: "Stop event",
			input: map[string]interface{}{
				"hook_event_name": "Stop",
			},
			wantName: hooks.EventAgentEnd,
		},
		{
			name: "SubagentStop event",
			input: map[string]interface{}{
				"hook_event_name": "SubagentStop",
			},
			wantName: hooks.EventSubagentEnd,
		},
		{
			name: "Notification event",
			input: map[string]interface{}{
				"hook_event_name": "Notification",
				"message":         "Permission required",
			},
			wantName: hooks.EventNotification,
		},
		{
			name: "ModelResponse maps to model-end",
			input: map[string]interface{}{
				"hook_event_name": "ModelResponse",
			},
			wantName: hooks.EventModelEnd,
		},
		{
			name: "Unknown event preserves name",
			input: map[string]interface{}{
				"hook_event_name": "CustomEvent",
			},
			wantName: "CustomEvent",
		},
	}

	d := NewClaudeDialect()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := d.Parse(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, event.Name)
			assert.Equal(t, "claude", event.Dialect)

			if tt.wantTool != "" {
				assert.Equal(t, tt.wantTool, event.Data.ToolName)
			}
			if tt.wantPrompt != "" {
				assert.Equal(t, tt.wantPrompt, event.Data.Prompt)
			}
			assert.Equal(t, tt.wantError, event.Data.Error)
		})
	}
}

func TestClaudeDialect_ParseFilePath(t *testing.T) {
	d := NewClaudeDialect()

	t.Run("file_path from tool_input object", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "Write",
			"tool_input": map[string]interface{}{
				"file_path": "/path/to/file.txt",
				"content":   "file content",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "/path/to/file.txt", event.Data.FilePath)
	})

	t.Run("file_path from tool_response camelCase", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "Write",
			"tool_response": map[string]interface{}{
				"filePath": "/path/to/written.txt",
				"success":  true,
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "/path/to/written.txt", event.Data.FilePath)
	})

	t.Run("tool_input takes priority over tool_response", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "Write",
			"tool_input": map[string]interface{}{
				"file_path": "/from/input.txt",
			},
			"tool_response": map[string]interface{}{
				"filePath": "/from/response.txt",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "/from/input.txt", event.Data.FilePath)
	})

	t.Run("no file_path when tool_input is string", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "Bash",
			"tool_input":      "ls -la",
		})
		require.NoError(t, err)
		assert.Empty(t, event.Data.FilePath)
	})

	t.Run("no file_path when absent", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name": "PostToolUse",
			"tool_name":       "Bash",
		})
		require.NoError(t, err)
		assert.Empty(t, event.Data.FilePath)
	})
}

// --- Content-type filtering tests ---

func TestClaudeDialect_ParseTokens(t *testing.T) {
	d := NewClaudeDialect()

	t.Run("top-level token fields", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name": "ModelResponse",
			"input_tokens":    float64(1500),
			"output_tokens":   float64(500),
		})
		require.NoError(t, err)
		assert.Equal(t, int64(1500), event.Data.InputTokens)
		assert.Equal(t, int64(500), event.Data.OutputTokens)
	})

	t.Run("nested usage object", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name": "ModelResponse",
			"usage": map[string]interface{}{
				"input_tokens":  float64(2000),
				"output_tokens": float64(800),
				"cached_tokens": float64(300),
			},
		})
		require.NoError(t, err)
		assert.Equal(t, int64(2000), event.Data.InputTokens)
		assert.Equal(t, int64(800), event.Data.OutputTokens)
		assert.Equal(t, int64(300), event.Data.CachedTokens)
	})

	t.Run("cache_read_input_tokens", func(t *testing.T) {
		event, err := d.Parse(map[string]interface{}{
			"hook_event_name":         "ModelResponse",
			"input_tokens":            float64(1000),
			"output_tokens":           float64(400),
			"cache_read_input_tokens": float64(600),
		})
		require.NoError(t, err)
		assert.Equal(t, int64(1000), event.Data.InputTokens)
		assert.Equal(t, int64(400), event.Data.OutputTokens)
		assert.Equal(t, int64(600), event.Data.CachedTokens)
	})
}

// TestClaudeDialect_CapturedSubagentPayloads parses the captured, scrubbed
// Claude Code sub-agent hook payloads (testdata/claude) and checks the event
// names and that only the sub-agent's events carry agent_id.
func TestClaudeDialect_CapturedSubagentPayloads(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "claude", "hook-payloads-2.1.296.jsonl"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	want := []struct {
		name    string
		tool    string
		agentID string
	}{
		{hooks.EventSessionStart, "", ""},
		{hooks.EventToolStart, "Agent", ""},
		{hooks.EventToolStart, "Bash", "a0000000000000001"},
		{hooks.EventToolEnd, "Bash", "a0000000000000001"},
		{hooks.EventSubagentEnd, "", "a0000000000000001"},
		{hooks.EventToolEnd, "Agent", ""},
		{hooks.EventAgentEnd, "", ""},
	}

	d := NewClaudeDialect()
	scanner := bufio.NewScanner(f)
	i := 0
	for scanner.Scan() {
		require.Less(t, i, len(want), "more records than expected")
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &payload))
		event, err := d.Parse(payload)
		require.NoError(t, err)
		assert.Equal(t, want[i].name, event.Name, "record %d", i)
		assert.Equal(t, want[i].tool, event.Data.ToolName, "record %d", i)
		assert.Equal(t, "11111111-1111-1111-1111-111111111111", event.Data.SessionID, "record %d", i)
		agentID, hasAgentID := event.Data.Raw["agent_id"]
		if want[i].agentID == "" {
			assert.False(t, hasAgentID, "record %d", i)
		} else {
			assert.Equal(t, want[i].agentID, agentID, "record %d", i)
		}
		i++
	}
	require.NoError(t, scanner.Err())
	assert.Equal(t, len(want), i)
}
