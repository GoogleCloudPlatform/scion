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
