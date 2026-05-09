/*
Copyright 2025 The Scion Authors.
*/

package dialects

import (
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

func TestExtractFinalAssistantText(t *testing.T) {
	dir := t.TempDir()

	write := func(t *testing.T, name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	t.Run("single assistant turn with text block", func(t *testing.T) {
		path := write(t, "single.jsonl",
			`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`+"\n"+
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello there"}]}}`+"\n",
		)
		assert.Equal(t, "Hello there", extractFinalAssistantText(path))
	})

	t.Run("multiple blocks in final assistant message", func(t *testing.T) {
		path := write(t, "multiblock.jsonl",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Thinking..."},{"type":"tool_use","id":"t1","name":"Read","input":{}},{"type":"text","text":"Done."}]}}`+"\n",
		)
		assert.Equal(t, "Thinking...Done.", extractFinalAssistantText(path))
	})

	t.Run("contiguous assistant entries concatenated", func(t *testing.T) {
		path := write(t, "contiguous.jsonl",
			`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`+"\n"+
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"First"}]}}`+"\n"+
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`+"\n"+
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"..."}]}}`+"\n"+
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Second"}]}}`+"\n"+
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Third"}]}}`+"\n",
		)
		assert.Equal(t, "Second\n\nThird", extractFinalAssistantText(path))
	})

	t.Run("tool-use only final turn yields empty", func(t *testing.T) {
		path := write(t, "toolonly.jsonl",
			`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`+"\n"+
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`+"\n",
		)
		assert.Equal(t, "", extractFinalAssistantText(path))
	})

	t.Run("plain string content", func(t *testing.T) {
		path := write(t, "plainstr.jsonl",
			`{"type":"assistant","message":{"role":"assistant","content":"Legacy shape"}}`+"\n",
		)
		assert.Equal(t, "Legacy shape", extractFinalAssistantText(path))
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		assert.Equal(t, "", extractFinalAssistantText(filepath.Join(dir, "nope.jsonl")))
	})

	t.Run("malformed lines are skipped", func(t *testing.T) {
		path := write(t, "malformed.jsonl",
			`not json at all`+"\n"+
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Survived"}]}}`+"\n"+
				`{"type":"assistant","broken`+"\n",
		)
		assert.Equal(t, "Survived", extractFinalAssistantText(path))
	})
}

func TestClaudeDialect_StopEventExtractsAssistantText(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "t.jsonl")
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All done."}]}}` + "\n"
	require.NoError(t, os.WriteFile(transcriptPath, []byte(content), 0o600))

	d := NewClaudeDialect()
	event, err := d.Parse(map[string]interface{}{
		"hook_event_name": "Stop",
		"transcript_path": transcriptPath,
	})
	require.NoError(t, err)
	assert.Equal(t, hooks.EventAgentEnd, event.Name)
	assert.Equal(t, "All done.", event.Data.AssistantText)
}

func TestClaudeDialect_StopEventPrefersLastAssistantMessage(t *testing.T) {
	// Claude Code 2.1+ includes the final assistant text directly in
	// the Stop hook payload. When present, we must use it in preference
	// to reading the transcript file (which may be mid-flush when the
	// hook fires, producing an empty or stale extraction).
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "t.jsonl")
	// Write a transcript that would yield "from transcript" if read.
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"from transcript"}]}}` + "\n"
	require.NoError(t, os.WriteFile(transcriptPath, []byte(content), 0o600))

	d := NewClaudeDialect()
	event, err := d.Parse(map[string]interface{}{
		"hook_event_name":        "Stop",
		"transcript_path":        transcriptPath,
		"last_assistant_message": "from payload",
	})
	require.NoError(t, err)
	assert.Equal(t, "from payload", event.Data.AssistantText,
		"payload field must win over transcript extraction")
}

func TestClaudeDialect_StopEventFallsBackToTranscript(t *testing.T) {
	// When last_assistant_message is absent (older Claude Code or other
	// harnesses), fall back to reading the transcript file.
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "t.jsonl")
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fallback text"}]}}` + "\n"
	require.NoError(t, os.WriteFile(transcriptPath, []byte(content), 0o600))

	d := NewClaudeDialect()
	event, err := d.Parse(map[string]interface{}{
		"hook_event_name": "Stop",
		"transcript_path": transcriptPath,
	})
	require.NoError(t, err)
	assert.Equal(t, "fallback text", event.Data.AssistantText)
}

func TestClaudeDialect_SubagentStopDoesNotExtract(t *testing.T) {
	d := NewClaudeDialect()
	event, err := d.Parse(map[string]interface{}{
		"hook_event_name":        "SubagentStop",
		"last_assistant_message": "subagent output",
		"transcript_path":        "/does/not/matter",
	})
	require.NoError(t, err)
	assert.Equal(t, hooks.EventSubagentEnd, event.Name)
	assert.Empty(t, event.Data.AssistantText,
		"SubagentStop must not extract assistant text — only main agent Stop drives state")
}

func TestClaudeDialect_NonStopEventDoesNotExtract(t *testing.T) {
	d := NewClaudeDialect()
	event, err := d.Parse(map[string]interface{}{
		"hook_event_name":        "PreToolUse",
		"transcript_path":        "/does/not/exist",
		"last_assistant_message": "ignored",
		"tool_name":              "Read",
	})
	require.NoError(t, err)
	assert.Empty(t, event.Data.AssistantText)
}

func TestClaudeDialect_Parse(t *testing.T) {
	tests := []struct {
		name       string
		input      map[string]interface{}
		wantName   string
		wantTool   string
		wantPrompt string
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
