/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// These tests cover how a tool-end is classified (toolOutcome) and what each
// outcome does in telemetry, the session aggregator and the log line: a
// failure the harness reports counts as a failure even with an empty error
// string, and a call the user interrupted is not counted as an error. All
// payloads are synthetic and go through the real dialects.

// toolOutcomeClaude parses a synthetic claude tool-end for the Bash tool.
func toolOutcomeClaude(t *testing.T, hookEventName string, extra map[string]interface{}) *hooks.Event {
	t.Helper()
	payload := map[string]interface{}{
		"hook_event_name": hookEventName,
		"session_id":      "00000000-0000-0000-0000-000000000000",
		"tool_name":       "Bash",
		"tool_input":      map[string]interface{}{"command": "ls"},
	}
	for k, v := range extra {
		payload[k] = v
	}
	event, err := dialects.NewClaudeDialect().Parse(payload)
	require.NoError(t, err)
	require.Equal(t, hooks.EventToolEnd, event.Name)
	return event
}

// toolOutcomeCopilot parses a synthetic copilot postToolUse whose
// toolResult has resultType and no error string.
func toolOutcomeCopilot(t *testing.T, resultType string) *hooks.Event {
	t.Helper()
	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "copilot"))
	require.NoError(t, err)
	event, err := md.Parse(map[string]interface{}{
		"hook_event_name": "postToolUse",
		"sessionId":       "copilot-synthetic-session",
		"toolName":        "Bash",
		"toolResult":      map[string]interface{}{"resultType": resultType, "textResultForLlm": ""},
	})
	require.NoError(t, err)
	require.Equal(t, hooks.EventToolEnd, event.Name)
	return event
}

type toolOutcomeCase struct {
	name    string
	event   func(t *testing.T) *hooks.Event
	want    telemetry.ToolOutcome
	status  string // agent.tool.calls status label
	logLine string
}

func toolOutcomeCases() []toolOutcomeCase {
	return []toolOutcomeCase{
		{"claude PostToolUse", func(t *testing.T) *hooks.Event {
			return toolOutcomeClaude(t, "PostToolUse", map[string]interface{}{"tool_response": map[string]interface{}{"stdout": ""}})
		}, telemetry.ToolSucceeded, "success", "Tool Bash completed"},
		{"claude PostToolUseFailure with error", func(t *testing.T) *hooks.Event {
			return toolOutcomeClaude(t, "PostToolUseFailure", map[string]interface{}{"error": "exit status 1", "is_interrupt": false})
		}, telemetry.ToolFailed, "error", "Tool Bash failed"},
		{"claude PostToolUseFailure with empty error", func(t *testing.T) *hooks.Event {
			return toolOutcomeClaude(t, "PostToolUseFailure", map[string]interface{}{"error": "", "is_interrupt": false})
		}, telemetry.ToolFailed, "error", "Tool Bash failed"},
		{"claude interrupted", func(t *testing.T) *hooks.Event {
			return toolOutcomeClaude(t, "PostToolUseFailure", map[string]interface{}{"error": "Interrupted by user", "is_interrupt": true})
		}, telemetry.ToolInterrupted, "interrupted", "Tool Bash interrupted"},
		{"success false with empty error", func(t *testing.T) *hooks.Event {
			return toolOutcomeClaude(t, "PostToolUse", map[string]interface{}{"success": false})
		}, telemetry.ToolFailed, "error", "Tool Bash failed"},
		{"success true", func(t *testing.T) *hooks.Event {
			return toolOutcomeClaude(t, "PostToolUse", map[string]interface{}{"success": true})
		}, telemetry.ToolSucceeded, "success", "Tool Bash completed"},
		{"copilot failure with no error", func(t *testing.T) *hooks.Event {
			return toolOutcomeCopilot(t, "failure")
		}, telemetry.ToolFailed, "error", "Tool Bash failed"},
		{"copilot success", func(t *testing.T) *hooks.Event {
			return toolOutcomeCopilot(t, "success")
		}, telemetry.ToolSucceeded, "success", "Tool Bash completed"},
	}
}

func TestToolOutcome_Classify(t *testing.T) {
	for _, tc := range toolOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, toolOutcome(tc.event(t)))
		})
	}
}

// Both counting sites of agent.tool.calls (paired with a tool-start in the
// same process, and the unpaired end event of hook-per-process mode) label
// the call by its outcome.
func TestToolOutcome_ToolCallsStatus(t *testing.T) {
	for _, tc := range toolOutcomeCases() {
		for _, paired := range []bool{false, true} {
			name := tc.name + "/unpaired"
			if paired {
				name = tc.name + "/paired"
			}
			t.Run(name, func(t *testing.T) {
				scrubTelemetryEnv(t)
				reader := sdkmetric.NewManualReader()
				mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
				defer func() { _ = mp.Shutdown(context.Background()) }()
				h := NewTelemetryHandler(nil, nil, nil, mp)

				if paired {
					require.NoError(t, h.Handle(waitClearClaudeToolStart(t)))
				}
				require.NoError(t, h.Handle(tc.event(t)))

				assert.Equal(t, map[string]int64{tc.status: 1}, claudeToolCallStatuses(t, reader))
			})
		}
	}
}

// The session aggregator counts every call; a failure as an error, a
// success as a success, and an interrupted call as neither.
func TestToolOutcome_Aggregator(t *testing.T) {
	for _, tc := range toolOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			a := telemetry.NewAggregator()
			_, _ = aggregatorUpdates[hooks.EventToolEnd](a, tc.event(t))
			got := a.Finalize(0, 0, 0, 0, "").ToolCalls["Bash"]
			want := telemetry.ToolCallStats{Calls: 1}
			switch tc.want {
			case telemetry.ToolSucceeded:
				want.Success = 1
			case telemetry.ToolFailed:
				want.Error = 1
			}
			assert.Equal(t, want, got)
		})
	}
}

// The log line says whether the tool completed, failed or was interrupted.
func TestToolOutcome_LogLine(t *testing.T) {
	h := &LoggingHandler{}
	for _, tc := range toolOutcomeCases() {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.logLine, h.formatLogMessage(tc.event(t)))
		})
	}
	t.Run("no tool name", func(t *testing.T) {
		event := &hooks.Event{Name: hooks.EventToolEnd, Data: hooks.EventData{Failed: true}}
		assert.Equal(t, "Tool execution failed", h.formatLogMessage(event))
	})
}
