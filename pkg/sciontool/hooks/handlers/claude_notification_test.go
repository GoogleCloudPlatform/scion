/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClaudePermissionPromptNotificationSetsWaitingForInput feeds a
// synthetic Claude Code Notification payload of type permission_prompt (the
// shape documented at https://code.claude.com/docs/en/hooks#notification)
// through the claude dialect and the status handler, and checks that the
// agent ends up waiting_for_input.
func TestClaudePermissionPromptNotificationSetsWaitingForInput(t *testing.T) {
	const payload = `{
		"session_id": "00000000-0000-0000-0000-000000000000",
		"transcript_path": "/tmp/example/transcript.jsonl",
		"cwd": "/tmp/example",
		"hook_event_name": "Notification",
		"message": "Claude needs your permission to use Bash",
		"notification_type": "permission_prompt"
	}`
	var data map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(payload), &data))

	event, err := dialects.NewClaudeDialect().Parse(data)
	require.NoError(t, err)
	assert.Equal(t, hooks.EventNotification, event.Name)
	assert.Equal(t, "claude", event.Dialect)

	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	// Start from an executing state, as when Claude stops at a prompt
	// mid-tool-call.
	require.NoError(t, h.Handle(&hooks.Event{
		Name:    hooks.EventToolStart,
		Dialect: "claude",
		Data:    hooks.EventData{ToolName: "Bash"},
	}))
	require.Equal(t, "executing", readAgentInfo(t, statusPath).Activity)

	require.NoError(t, h.Handle(event))
	assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity)
}
