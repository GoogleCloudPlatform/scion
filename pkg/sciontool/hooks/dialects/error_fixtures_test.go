/*
Copyright 2026 The Scion Authors.
*/

package dialects

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readFixture reads every JSON record of testdata/<harness>/<name>.
func readFixture(t *testing.T, harness, name string) []map[string]interface{} {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", harness, name))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var records []map[string]interface{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}
		var rec map[string]interface{}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
		records = append(records, rec)
	}
	require.NoError(t, scanner.Err())
	return records
}

func TestClaudeStopFailureError(t *testing.T) {
	tests := []struct {
		name string
		data map[string]interface{}
		want string
	}{
		{"unknown category gives the message", map[string]interface{}{"error": "unknown", "last_assistant_message": "API Error: 400 denied"}, "API Error: 400 denied"},
		{"category and message", map[string]interface{}{"error": "rate_limit", "last_assistant_message": "API Error: 429"}, "rate_limit: API Error: 429"},
		{"category alone", map[string]interface{}{"error": "billing_error"}, "billing_error"},
		{"message alone", map[string]interface{}{"last_assistant_message": "API Error: 500"}, "API Error: 500"},
		{"unknown alone", map[string]interface{}{"error": "unknown"}, ""},
		{"neither", map[string]interface{}{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, claudeStopFailureError(tt.data))
		})
	}
}

// TestClaudeDialect_StopFailureFixture parses the captured Claude Code
// 2.1.325 StopFailure payloads: agent-error with the provider text.
func TestClaudeDialect_StopFailureFixture(t *testing.T) {
	d := NewClaudeDialect()
	var got []string
	for _, name := range []string{"api-error-400-2.1.325.jsonl", "api-error-401-2.1.325.jsonl"} {
		for _, rec := range readFixture(t, "claude", name) {
			if rec["hook_event_name"] != "StopFailure" {
				continue
			}
			event, err := d.Parse(rec)
			require.NoError(t, err)
			assert.Equal(t, hooks.EventAgentError, event.Name)
			got = append(got, event.Data.Error)
		}
	}
	text := "Permission 'aiplatform.endpoints.predict' denied on resource (or it may not exist)."
	assert.Equal(t, []string{
		"API Error: 400 " + text,
		"API Error: 400 " + text,
		"authentication_failed: Please run /login · API Error: 401 " + text,
	}, got)
}

// TestGrokDialect_PrefersPascalCaseEventName checks the grok-build dialect
// against grok 1.0.77, which sends a snake_case hookEventName next to the
// PascalCase hook_event_name the mappings use.
func TestGrokDialect_PrefersPascalCaseEventName(t *testing.T) {
	md, err := LoadMappingDialect(testutil.HarnessDialectPath(t, "grok-build"))
	require.NoError(t, err)
	var names []string
	for _, rec := range readFixture(t, "grok-build", "api-error-400-1.0.77.jsonl") {
		require.NotEqual(t, rec["hookEventName"], rec["hook_event_name"], "fixture has both spellings")
		event, err := md.Parse(rec)
		require.NoError(t, err)
		names = append(names, event.Name)
	}
	assert.Equal(t, []string{
		hooks.EventSessionStart, hooks.EventPromptSubmit, hooks.EventIgnored, hooks.EventAgentError,
		hooks.EventPromptSubmit, hooks.EventIgnored, hooks.EventAgentError,
		hooks.EventSessionEnd, hooks.EventIgnored,
	}, names)
}

// TestCopilotFixture_ErrorOccurredDoesNotSeparateFatal pins why the copilot
// dialect maps errorOccurred to model-end (a failed model call) and not to
// agent-error (a failed turn): copilot 1.0.112 sends
// the same errorContext and recoverable values for a fatal 400 (no
// agentStop follows) and for a 503 it retries to success (agentStop
// follows), so the payload cannot tell a failed turn from a retried call.
func TestCopilotFixture_ErrorOccurredDoesNotSeparateFatal(t *testing.T) {
	type shape struct {
		context     interface{}
		recoverable interface{}
		agentStop   bool
	}
	read := func(name string) shape {
		var s shape
		var errors int
		for _, rec := range readFixture(t, "copilot", name) {
			switch rec["hook_event_name"] {
			case "errorOccurred":
				errors++
				s.context, s.recoverable = rec["errorContext"], rec["recoverable"]
			case "agentStop":
				s.agentStop = true
			}
		}
		require.Equal(t, 1, errors, name)
		return s
	}
	fatal := read("api-error-400-1.0.112.jsonl")
	retried := read("transient-503-then-200-1.0.112.jsonl")
	assert.False(t, fatal.agentStop)
	assert.True(t, retried.agentStop)
	assert.Equal(t, fatal.context, retried.context)
	assert.Equal(t, fatal.recoverable, retried.recoverable)

	md, err := LoadMappingDialect(testutil.HarnessDialectPath(t, "copilot"))
	require.NoError(t, err)
	for _, rec := range readFixture(t, "copilot", "api-error-400-1.0.112.jsonl") {
		if rec["hook_event_name"] != "errorOccurred" {
			continue
		}
		event, err := md.Parse(rec)
		require.NoError(t, err)
		assert.Equal(t, hooks.EventModelEnd, event.Name)
		assert.True(t, event.Data.Uncounted)
		assert.Contains(t, event.Data.Error, "Permission 'aiplatform.endpoints.predict' denied")
	}

	// Only errorContext model_call was captured; any other context is ignored.
	event, err := md.Parse(map[string]interface{}{
		"hook_event_name": "errorOccurred", "errorContext": "tool_call",
		"error": map[string]interface{}{"message": "boom"},
	})
	require.NoError(t, err)
	assert.Equal(t, hooks.EventIgnored, event.Name)
}

// TestGrokDialect_IgnoresAgentErrorNotification checks that only grok's
// agent_error Notification is ignored; other notifications still map.
func TestGrokDialect_IgnoresAgentErrorNotification(t *testing.T) {
	md, err := LoadMappingDialect(testutil.HarnessDialectPath(t, "grok-build"))
	require.NoError(t, err)
	for notificationType, want := range map[string]string{
		"agent_error":       hooks.EventIgnored,
		"permission_prompt": hooks.EventNotification,
		"":                  hooks.EventNotification,
	} {
		rec := map[string]interface{}{"hook_event_name": "Notification", "message": "m"}
		if notificationType != "" {
			rec["notificationType"] = notificationType
		}
		event, err := md.Parse(rec)
		require.NoError(t, err)
		assert.Equal(t, want, event.Name, notificationType)
	}
}
