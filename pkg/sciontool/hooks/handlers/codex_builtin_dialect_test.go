/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"path/filepath"
	"runtime"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodexBuiltinDialect_StatusMatchesStagedDialect checks that codex
// prompt-submit, tool-start, tool-end and stop hook events change the agent
// status the same way whether they are parsed by the built-in codex dialect
// or by the staged harnesses/codex/dialect.yaml.
func TestCodexBuiltinDialect_StatusMatchesStagedDialect(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	staged, err := dialects.LoadMappingDialect(filepath.Join(root, "harnesses", "codex", "dialect.yaml"))
	require.NoError(t, err)
	builtin := dialects.NewCodexDialect()

	tests := []struct {
		name    string
		payload map[string]interface{}
		// before is a non-sticky activity different from want, so an
		// event that changes nothing fails the check.
		before state.Activity
		want   state.Activity
	}{
		{
			name: "prompt-submit",
			payload: map[string]interface{}{
				"hook_event_name": "UserPromptSubmit",
				"session_id":      "s-1",
				"prompt":          "fix the build",
			},
			before: state.ActivityWorking,
			want:   state.ActivityThinking,
		},
		{
			name: "tool-start",
			payload: map[string]interface{}{
				"hook_event_name": "PreToolUse",
				"session_id":      "s-1",
				"tool_name":       "Bash",
				"tool_input":      map[string]interface{}{"command": "ls"},
			},
			before: state.ActivityThinking,
			want:   state.ActivityExecuting,
		},
		{
			name: "tool-end",
			payload: map[string]interface{}{
				"hook_event_name": "PostToolUse",
				"session_id":      "s-1",
				"tool_name":       "Bash",
			},
			before: state.ActivityExecuting,
			want:   state.ActivityWorking,
		},
		{
			name: "stop",
			payload: map[string]interface{}{
				"hook_event_name": "Stop",
				"session_id":      "s-1",
			},
			before: state.ActivityThinking,
			want:   state.ActivityWorking,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activityAfter := func(d hooks.Dialect) string {
				statusPath := filepath.Join(t.TempDir(), "agent-info.json")
				h := &StatusHandler{StatusPath: statusPath}
				require.NoError(t, h.UpdateActivity(tt.before, ""))
				event, err := d.Parse(tt.payload)
				require.NoError(t, err)
				require.NoError(t, h.Handle(event))
				return readAgentInfo(t, statusPath).Activity
			}

			stagedActivity := activityAfter(staged)
			builtinActivity := activityAfter(builtin)
			assert.Equal(t, string(tt.want), stagedActivity, "staged dialect")
			assert.Equal(t, stagedActivity, builtinActivity, "built-in dialect status differs from staged dialect")
		})
	}
}
