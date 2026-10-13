/*
Copyright 2025 The Scion Authors.
*/

package dialects

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexDialectParse_TurnComplete(t *testing.T) {
	d := NewCodexDialect()
	event, err := d.Parse(map[string]interface{}{
		"type":  "agent-turn-complete",
		"title": "Done",
	})
	require.NoError(t, err)
	assert.Equal(t, hooks.EventResponseComplete, event.Name)
	assert.Equal(t, "Done", event.Data.Message)
	assert.Equal(t, "codex", event.Dialect)
}

func TestCodexDialectParse_FallbackEventField(t *testing.T) {
	d := NewCodexDialect()
	event, err := d.Parse(map[string]interface{}{
		"event":   "notification",
		"message": "Need approval",
	})
	require.NoError(t, err)
	assert.Equal(t, hooks.EventNotification, event.Name)
	assert.Equal(t, "Need approval", event.Data.Message)
}

func TestCodexDialect_EventMappings(t *testing.T) {
	d := NewCodexDialect()

	tests := []struct {
		rawName  string
		wantName string
	}{
		{"tool-start", hooks.EventToolStart},
		{"tool-end", hooks.EventToolEnd},
		{"model-start", hooks.EventModelStart},
		{"model-end", hooks.EventModelEnd},
		{"session-start", hooks.EventSessionStart},
		{"session-end", hooks.EventSessionEnd},
	}

	for _, tt := range tests {
		t.Run(tt.rawName, func(t *testing.T) {
			event, err := d.Parse(map[string]interface{}{"type": tt.rawName})
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, event.Name)
		})
	}
}

func TestCodexDialect_TokenExtraction(t *testing.T) {
	d := NewCodexDialect()
	event, err := d.Parse(map[string]interface{}{
		"type": "model-end",
		"usage": map[string]interface{}{
			"prompt_tokens":     float64(800),
			"completion_tokens": float64(200),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(800), event.Data.InputTokens)
	assert.Equal(t, int64(200), event.Data.OutputTokens)
}

func TestCodexDialect_StatusFields(t *testing.T) {
	d := NewCodexDialect()
	event, err := d.Parse(map[string]interface{}{
		"type":    "tool-end",
		"success": true,
		"error":   "something failed",
	})
	require.NoError(t, err)
	assert.True(t, event.Data.Success)
	assert.Equal(t, "something failed", event.Data.Error)
}

// loadStagedCodexDialect loads harnesses/codex/dialect.yaml, the dialect file
// staged into codex agents.
func loadStagedCodexDialect(t *testing.T) *MappingDialect {
	t.Helper()
	md, err := LoadMappingDialect(testutil.HarnessDialectPath(t, "codex"))
	require.NoError(t, err)
	require.Equal(t, "codex", md.Name())
	return md
}

// TestCodexDialect_MatchesStagedDialect checks that the built-in codex
// dialect maps every event the staged codex dialect file maps, to the same
// normalized event name, so the two cannot drift apart.
//
// The check covers event names only and runs in one direction: every raw
// name in the staged file must normalize identically in the built-in
// dialect, but raw names that only the built-in dialect maps are not
// checked, and neither is field extraction. That is sufficient because the
// staged file is the reference: agent status and the other handlers key off
// the normalized event name, so a staged mapping the built-in dialect lacks
// or maps differently is the drift that changes behaviour, while a built-in
// extra only matters for payloads the staged dialect would pass through
// unmapped anyway. TestCodexDialect_HookEventMappings pins the expected
// names independently of the staged file.
func TestCodexDialect_MatchesStagedDialect(t *testing.T) {
	staged := loadStagedCodexDialect(t)
	builtin := NewCodexDialect()

	require.NotEmpty(t, staged.spec.Mappings)
	for rawName, entry := range staged.spec.Mappings {
		t.Run(rawName, func(t *testing.T) {
			event, err := builtin.Parse(map[string]interface{}{"hook_event_name": rawName})
			require.NoError(t, err)
			assert.Equal(t, entry.Event, event.Name, "built-in codex dialect maps %q differently from harnesses/codex/dialect.yaml", rawName)
		})
	}
}

func TestCodexDialect_HookEventMappings(t *testing.T) {
	d := NewCodexDialect()

	tests := []struct {
		rawName  string
		wantName string
	}{
		{"UserPromptSubmit", hooks.EventPromptSubmit},
		{"PreToolUse", hooks.EventToolStart},
		{"PostToolUse", hooks.EventToolEnd},
		{"Stop", hooks.EventAgentEnd},
		{"SubagentStop", hooks.EventSubagentEnd},
	}

	for _, tt := range tests {
		t.Run(tt.rawName, func(t *testing.T) {
			event, err := d.Parse(map[string]interface{}{"hook_event_name": tt.rawName})
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, event.Name)
			assert.Equal(t, tt.rawName, event.RawName)
		})
	}
}

func TestCodexDialect_PromptExtraction(t *testing.T) {
	event, err := NewCodexDialect().Parse(map[string]interface{}{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      "s-1",
		"prompt":          "fix the build",
	})
	require.NoError(t, err)
	assert.Equal(t, hooks.EventPromptSubmit, event.Name)
	assert.Equal(t, "fix the build", event.Data.Prompt)
	assert.Equal(t, "s-1", event.Data.SessionID)
}
