/*
Copyright 2026 The Scion Authors.
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

func TestMappingDialect_Name(t *testing.T) {
	md := NewMappingDialect(MappingDialectSpec{
		Dialect:        "test-harness",
		EventNameField: "event",
	})
	assert.Equal(t, "test-harness", md.Name())
}

func TestMappingDialect_Parse_SimpleMapping(t *testing.T) {
	spec := MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "hook_event_name",
		Mappings: map[string]MappingEntrySpec{
			"SessionBegin": {Event: hooks.EventSessionStart},
			"SessionDone":  {Event: hooks.EventSessionEnd},
			"BeforeTool":   {Event: hooks.EventToolStart},
			"AfterTool":    {Event: hooks.EventToolEnd},
		},
	}
	md := NewMappingDialect(spec)

	tests := []struct {
		name     string
		input    map[string]interface{}
		wantName string
	}{
		{
			name:     "SessionBegin maps to session-start",
			input:    map[string]interface{}{"hook_event_name": "SessionBegin"},
			wantName: hooks.EventSessionStart,
		},
		{
			name:     "SessionDone maps to session-end",
			input:    map[string]interface{}{"hook_event_name": "SessionDone"},
			wantName: hooks.EventSessionEnd,
		},
		{
			name:     "BeforeTool maps to tool-start",
			input:    map[string]interface{}{"hook_event_name": "BeforeTool", "tool_name": "shell"},
			wantName: hooks.EventToolStart,
		},
		{
			name:     "AfterTool maps to tool-end",
			input:    map[string]interface{}{"hook_event_name": "AfterTool"},
			wantName: hooks.EventToolEnd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := md.Parse(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, event.Name)
			assert.Equal(t, "test", event.Dialect)
		})
	}
}

func TestMappingDialect_Parse_UnknownEvent(t *testing.T) {
	md := NewMappingDialect(MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "hook_event_name",
		Mappings:       map[string]MappingEntrySpec{},
	})

	event, err := md.Parse(map[string]interface{}{
		"hook_event_name": "UnmappedEvent",
	})
	require.NoError(t, err)
	assert.Equal(t, "UnmappedEvent", event.Name, "unmapped events should pass through")
	assert.Equal(t, "UnmappedEvent", event.RawName)
}

func TestMappingDialect_Parse_EventNameFields(t *testing.T) {
	spec := MappingDialectSpec{
		Dialect:         "test",
		EventNameField:  "legacy_event",
		EventNameFields: []string{"type", "event", "hook_event_name"},
		Mappings: map[string]MappingEntrySpec{
			"Primary":  {Event: hooks.EventSessionStart},
			"Fallback": {Event: hooks.EventToolStart},
		},
	}
	md := NewMappingDialect(spec)

	t.Run("uses first non-empty field in order", func(t *testing.T) {
		event, err := md.Parse(map[string]interface{}{
			"type":            "",
			"event":           "Fallback",
			"hook_event_name": "Primary",
		})
		require.NoError(t, err)
		assert.Equal(t, hooks.EventToolStart, event.Name)
		assert.Equal(t, "Fallback", event.RawName)
	})

	t.Run("event_name_fields take precedence over legacy field", func(t *testing.T) {
		event, err := md.Parse(map[string]interface{}{
			"type":         "Primary",
			"legacy_event": "Fallback",
		})
		require.NoError(t, err)
		assert.Equal(t, hooks.EventSessionStart, event.Name)
		assert.Equal(t, "Primary", event.RawName)
	})

	t.Run("does not fall back to legacy field when ordered fields are configured", func(t *testing.T) {
		_, err := md.Parse(map[string]interface{}{
			"legacy_event": "Fallback",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "type")
		assert.Contains(t, err.Error(), "event")
		assert.Contains(t, err.Error(), "hook_event_name")
	})
}

func TestMappingDialect_Parse_CommonFields(t *testing.T) {
	md := NewMappingDialect(MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "event",
		Mappings: map[string]MappingEntrySpec{
			"test": {Event: "test-event"},
		},
	})

	event, err := md.Parse(map[string]interface{}{
		"event":      "test",
		"prompt":     "hello world",
		"tool_name":  "bash",
		"message":    "some message",
		"reason":     "done",
		"source":     "user",
		"session_id": "abc-123",
		"error":      "oops",
		"success":    true,
	})
	require.NoError(t, err)
	assert.Equal(t, "hello world", event.Data.Prompt)
	assert.Equal(t, "bash", event.Data.ToolName)
	assert.Equal(t, "some message", event.Data.Message)
	assert.Equal(t, "done", event.Data.Reason)
	assert.Equal(t, "user", event.Data.Source)
	assert.Equal(t, "abc-123", event.Data.SessionID)
	assert.Equal(t, "oops", event.Data.Error)
	assert.True(t, event.Data.Success)
}

func TestMappingDialect_Parse_FieldExtraction(t *testing.T) {
	spec := MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "hook_event_name",
		Mappings: map[string]MappingEntrySpec{
			"ToolCall": {
				Event: hooks.EventToolStart,
				Fields: map[string]string{
					"tool_name":  ".toolCall.name",
					"tool_input": ".toolCall.args",
				},
			},
			"ToolResult": {
				Event: hooks.EventToolEnd,
				Fields: map[string]string{
					"tool_name": ".toolCall.name",
					"error":     ".error",
					"success":   ".result.ok",
				},
			},
		},
	}
	md := NewMappingDialect(spec)

	t.Run("nested field extraction", func(t *testing.T) {
		event, err := md.Parse(map[string]interface{}{
			"hook_event_name": "ToolCall",
			"toolCall": map[string]interface{}{
				"name": "write_to_file",
				"args": map[string]interface{}{
					"TargetFile": "/tmp/test.txt",
					"Content":    "hello",
				},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, hooks.EventToolStart, event.Name)
		assert.Equal(t, "write_to_file", event.Data.ToolName)
		assert.Contains(t, event.Data.ToolInput, "TargetFile")
	})

	t.Run("boolean field extraction", func(t *testing.T) {
		event, err := md.Parse(map[string]interface{}{
			"hook_event_name": "ToolResult",
			"toolCall": map[string]interface{}{
				"name": "shell",
			},
			"result": map[string]interface{}{
				"ok": true,
			},
			"error": "",
		})
		require.NoError(t, err)
		assert.Equal(t, hooks.EventToolEnd, event.Name)
		assert.Equal(t, "shell", event.Data.ToolName)
		assert.True(t, event.Data.Success)
	})

	t.Run("missing nested path returns empty", func(t *testing.T) {
		event, err := md.Parse(map[string]interface{}{
			"hook_event_name": "ToolCall",
		})
		require.NoError(t, err)
		assert.Equal(t, hooks.EventToolStart, event.Name)
		assert.Empty(t, event.Data.ToolName)
	})
}

func TestMappingDialect_Parse_MissingEventName(t *testing.T) {
	md := NewMappingDialect(MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "hook_event_name",
		Mappings:       map[string]MappingEntrySpec{},
	})

	t.Run("missing event name field returns error", func(t *testing.T) {
		_, err := md.Parse(map[string]interface{}{
			"some_other_field": "value",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hook_event_name")
		assert.Contains(t, err.Error(), "missing or empty")
	})

	t.Run("empty event name returns error", func(t *testing.T) {
		_, err := md.Parse(map[string]interface{}{
			"hook_event_name": "",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing or empty")
	})

	t.Run("non-string event name returns error", func(t *testing.T) {
		_, err := md.Parse(map[string]interface{}{
			"hook_event_name": 42,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing or empty")
	})
}

func TestMappingDialect_Parse_AssistantTextField(t *testing.T) {
	spec := MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "event",
		Mappings: map[string]MappingEntrySpec{
			"AgentStop": {
				Event: hooks.EventAgentEnd,
				Fields: map[string]string{
					"assistant_text": ".response.text",
				},
			},
		},
	}
	md := NewMappingDialect(spec)

	event, err := md.Parse(map[string]interface{}{
		"event": "AgentStop",
		"response": map[string]interface{}{
			"text": "Here is the answer.",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "Here is the answer.", event.Data.AssistantText)
}

func TestMappingDialect_Parse_TokenFieldMappings(t *testing.T) {
	spec := MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "event",
		Mappings: map[string]MappingEntrySpec{
			"ModelDone": {
				Event: hooks.EventModelEnd,
				Fields: map[string]string{
					"input_tokens":  ".stats.promptTokens",
					"output_tokens": ".stats.completionTokens",
					"cached_tokens": ".stats.cachedTokens",
				},
			},
		},
	}
	md := NewMappingDialect(spec)

	event, err := md.Parse(map[string]interface{}{
		"event": "ModelDone",
		"stats": map[string]interface{}{
			"promptTokens":     float64(200),
			"completionTokens": float64(75),
			"cachedTokens":     float64(50),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(200), event.Data.InputTokens)
	assert.Equal(t, int64(75), event.Data.OutputTokens)
	assert.Equal(t, int64(50), event.Data.CachedTokens)
}

func TestMappingDialect_Parse_TokenExtraction(t *testing.T) {
	md := NewMappingDialect(MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "event",
		Mappings: map[string]MappingEntrySpec{
			"model_done": {Event: hooks.EventModelEnd},
		},
	})

	event, err := md.Parse(map[string]interface{}{
		"event": "model_done",
		"usage": map[string]interface{}{
			"input_tokens":  float64(100),
			"output_tokens": float64(50),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(100), event.Data.InputTokens)
	assert.Equal(t, int64(50), event.Data.OutputTokens)
}

func TestLoadMappingDialect(t *testing.T) {
	t.Run("valid spec with event_name_fields", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "dialect.yaml")
		err := os.WriteFile(specPath, []byte(`
dialect: my-harness
event_name_fields:
  - type
  - event
mappings:
  Start:
    event: session-start
`), 0644)
		require.NoError(t, err)

		md, err := LoadMappingDialect(specPath)
		require.NoError(t, err)
		assert.Equal(t, "my-harness", md.Name())
		assert.Empty(t, md.spec.EventNameField)
		assert.Equal(t, []string{"type", "event"}, md.spec.EventNameFields)
	})

	t.Run("valid spec", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "dialect.yaml")
		err := os.WriteFile(specPath, []byte(`
dialect: my-harness
event_name_field: event_type
mappings:
  Start:
    event: session-start
  Stop:
    event: agent-end
    fields:
      reason: .terminationReason
`), 0644)
		require.NoError(t, err)

		md, err := LoadMappingDialect(specPath)
		require.NoError(t, err)
		assert.Equal(t, "my-harness", md.Name())
		assert.Equal(t, "event_type", md.spec.EventNameField)
		assert.Len(t, md.spec.Mappings, 2)
		assert.Equal(t, "session-start", md.spec.Mappings["Start"].Event)
		assert.Equal(t, ".terminationReason", md.spec.Mappings["Stop"].Fields["reason"])
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := LoadMappingDialect("/nonexistent/dialect.yaml")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reading dialect spec")
	})

	t.Run("malformed YAML", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "dialect.yaml")
		err := os.WriteFile(specPath, []byte("not: valid: yaml: ["), 0644)
		require.NoError(t, err)

		_, err = LoadMappingDialect(specPath)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "parsing dialect spec")
	})

	t.Run("missing dialect field", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "dialect.yaml")
		err := os.WriteFile(specPath, []byte(`
event_name_field: event
mappings:
  Start:
    event: session-start
`), 0644)
		require.NoError(t, err)

		_, err = LoadMappingDialect(specPath)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing required 'dialect' field")
	})

	t.Run("missing event_name_field", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "dialect.yaml")
		err := os.WriteFile(specPath, []byte(`
dialect: test
mappings:
  Start:
    event: session-start
`), 0644)
		require.NoError(t, err)

		_, err = LoadMappingDialect(specPath)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing required 'event_name_field' or 'event_name_fields' field")
	})
}

func TestDiscoverMappingDialect(t *testing.T) {
	t.Run("found and name matches", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		bundleDir := filepath.Join(home, ".scion", "harness")
		require.NoError(t, os.MkdirAll(bundleDir, 0755))
		err := os.WriteFile(filepath.Join(bundleDir, "dialect.yaml"), []byte(`
dialect: my-harness
event_name_field: event
mappings:
  Ping:
    event: notification
`), 0644)
		require.NoError(t, err)

		md, err := DiscoverMappingDialect("my-harness")
		require.NoError(t, err)
		assert.Equal(t, "my-harness", md.Name())
	})

	t.Run("name mismatch", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		bundleDir := filepath.Join(home, ".scion", "harness")
		require.NoError(t, os.MkdirAll(bundleDir, 0755))
		err := os.WriteFile(filepath.Join(bundleDir, "dialect.yaml"), []byte(`
dialect: other-harness
event_name_field: event
mappings: {}
`), 0644)
		require.NoError(t, err)

		_, err = DiscoverMappingDialect("my-harness")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "declares dialect \"other-harness\" but \"my-harness\" was requested")
	})

	t.Run("no dialect.yaml present", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		_, err := DiscoverMappingDialect("anything")
		assert.Error(t, err)
	})
}

func TestMappingDialect_FullHarnessEvents(t *testing.T) {
	spec := MappingDialectSpec{
		Dialect:        "example-harness",
		EventNameField: "hook_event_name",
		Mappings: map[string]MappingEntrySpec{
			"PreInvocation":  {Event: hooks.EventModelStart},
			"PostInvocation": {Event: hooks.EventModelEnd},
			"PreToolUse": {
				Event: hooks.EventToolStart,
				Fields: map[string]string{
					"tool_name": ".toolCall.name",
				},
			},
			"PostToolUse": {
				Event: hooks.EventToolEnd,
				Fields: map[string]string{
					"tool_name": ".toolCall.name",
					"error":     ".error",
				},
			},
			"Stop": {
				Event: hooks.EventAgentEnd,
				Fields: map[string]string{
					"reason": ".terminationReason",
					"error":  ".error",
				},
			},
		},
	}
	md := NewMappingDialect(spec)

	tests := []struct {
		name     string
		input    map[string]interface{}
		wantName string
		wantTool string
		check    func(t *testing.T, e *hooks.Event)
	}{
		{
			name: "PreInvocation",
			input: map[string]interface{}{
				"hook_event_name": "PreInvocation",
				"conversationId":  "uuid-123",
				"invocationNum":   float64(0),
			},
			wantName: hooks.EventModelStart,
		},
		{
			name: "PostInvocation",
			input: map[string]interface{}{
				"hook_event_name": "PostInvocation",
				"conversationId":  "uuid-123",
				"invocationNum":   float64(1),
			},
			wantName: hooks.EventModelEnd,
		},
		{
			name: "PreToolUse with nested toolCall",
			input: map[string]interface{}{
				"hook_event_name": "PreToolUse",
				"stepIdx":         float64(3),
				"toolCall": map[string]interface{}{
					"name": "write_to_file",
					"args": map[string]interface{}{
						"TargetFile":  "/tmp/hello.txt",
						"CodeContent": "Hello",
					},
				},
			},
			wantName: hooks.EventToolStart,
			wantTool: "write_to_file",
		},
		{
			name: "PostToolUse with error",
			input: map[string]interface{}{
				"hook_event_name": "PostToolUse",
				"stepIdx":         float64(3),
				"toolCall": map[string]interface{}{
					"name": "run_command",
				},
				"error": "command failed",
			},
			wantName: hooks.EventToolEnd,
			wantTool: "run_command",
			check: func(t *testing.T, e *hooks.Event) {
				assert.Equal(t, "command failed", e.Data.Error)
			},
		},
		{
			name: "Stop",
			input: map[string]interface{}{
				"hook_event_name":   "Stop",
				"terminationReason": "NO_TOOL_CALL",
				"fullyIdle":         true,
				"executionNum":      float64(0),
				"error":             "",
			},
			wantName: hooks.EventAgentEnd,
			check: func(t *testing.T, e *hooks.Event) {
				assert.Equal(t, "NO_TOOL_CALL", e.Data.Reason)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := md.Parse(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, event.Name)
			assert.Equal(t, "example-harness", event.Dialect)
			if tt.wantTool != "" {
				assert.Equal(t, tt.wantTool, event.Data.ToolName)
			}
			if tt.check != nil {
				tt.check(t, event)
			}
		})
	}
}
