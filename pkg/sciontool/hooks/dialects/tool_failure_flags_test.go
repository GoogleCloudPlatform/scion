/*
Copyright 2026 The Scion Authors.
*/

package dialects

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every dialect records an explicit failure in EventData.Failed: a success
// field that is present and false. A missing success field is not a
// failure, since most harnesses send none for a call that succeeded.
func TestDialects_SuccessFieldSetsFailed(t *testing.T) {
	for _, d := range []hooks.Dialect{NewClaudeDialect(), NewCodexDialect(), NewGeminiDialect(), NewMappingDialect(MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "hook_event_name",
		Mappings:       map[string]MappingEntrySpec{"tool-end": {Event: hooks.EventToolEnd}},
	})} {
		for _, tc := range []struct {
			name       string
			success    interface{} // nil: no success field
			wantFailed bool
		}{
			{"success false", false, true},
			{"success true", true, false},
			{"no success field", nil, false},
		} {
			t.Run(d.Name()+"/"+tc.name, func(t *testing.T) {
				payload := map[string]interface{}{"hook_event_name": "tool-end", "type": "tool-end", "tool_name": "Bash"}
				if tc.success != nil {
					payload["success"] = tc.success
				}
				event, err := d.Parse(payload)
				require.NoError(t, err)
				assert.Equal(t, tc.wantFailed, event.Data.Failed)
				assert.False(t, event.Data.Interrupted)
			})
		}
	}
}

// A mapping dialect's custom success path sets Failed when the value is
// found and is not a success.
func TestMappingDialect_CustomSuccessPathSetsFailed(t *testing.T) {
	md := NewMappingDialect(MappingDialectSpec{
		Dialect:        "test",
		EventNameField: "hook_event_name",
		Mappings: map[string]MappingEntrySpec{"ToolResult": {
			Event:  hooks.EventToolEnd,
			Fields: map[string]string{"success": "result.status"},
		}},
	})
	for _, tc := range []struct {
		name       string
		result     map[string]interface{}
		wantFailed bool
	}{
		{"failure word", map[string]interface{}{"status": "failure"}, true},
		{"success word", map[string]interface{}{"status": "success"}, false},
		{"path missing", map[string]interface{}{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event, err := md.Parse(map[string]interface{}{"hook_event_name": "ToolResult", "result": tc.result})
			require.NoError(t, err)
			assert.Equal(t, tc.wantFailed, event.Data.Failed)
		})
	}
}

// A claude PostToolUseFailure is a failure even with no error string, and
// is_interrupt marks it interrupted. PostToolUse is neither.
func TestClaudeDialect_PostToolUseFailureFlags(t *testing.T) {
	for _, tc := range []struct {
		name            string
		payload         map[string]interface{}
		wantFailed      bool
		wantInterrupted bool
	}{
		{"PostToolUse", map[string]interface{}{"hook_event_name": "PostToolUse"}, false, false},
		{"failure with error", map[string]interface{}{"hook_event_name": "PostToolUseFailure", "error": "exit 1", "is_interrupt": false}, true, false},
		{"failure with no error", map[string]interface{}{"hook_event_name": "PostToolUseFailure"}, true, false},
		{"interrupted", map[string]interface{}{"hook_event_name": "PostToolUseFailure", "error": "Interrupted", "is_interrupt": true}, true, true},
		{"is_interrupt not a bool", map[string]interface{}{"hook_event_name": "PostToolUseFailure", "is_interrupt": "true"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.payload["tool_name"] = "Bash"
			event, err := NewClaudeDialect().Parse(tc.payload)
			require.NoError(t, err)
			assert.Equal(t, hooks.EventToolEnd, event.Name)
			assert.Equal(t, tc.wantFailed, event.Data.Failed)
			assert.Equal(t, tc.wantInterrupted, event.Data.Interrupted)
		})
	}
}
