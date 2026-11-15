/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

func TestFormatHarnessErrorReason(t *testing.T) {
	t.Run("source and text", func(t *testing.T) {
		assert.Equal(t, "model API: APIError (status 400)",
			formatHarnessErrorReason("model API", "APIError (status 400)"))
	})
	t.Run("empty text keeps the source", func(t *testing.T) {
		assert.Equal(t, "model API", formatHarnessErrorReason("model API", " \n\t"))
	})
	t.Run("single line without terminal escapes", func(t *testing.T) {
		got := formatHarnessErrorReason("model API", "\x1b[31m400 Bad Request\x1b[0m\r\n  \x1b]0;title\x07Permission\tdenied\n")
		assert.Equal(t, "model API: 400 Bad Request Permission denied", got)
	})
	t.Run("credentials are redacted", func(t *testing.T) {
		got := formatHarnessErrorReason("model API",
			"401: Authorization: Bearer sk-abcdefghijklmnopqrstuvwxyz0123 rejected; api_key=hunter2hunter2")
		assert.NotContains(t, got, "sk-abcdefghijklmnopqrstuvwxyz0123")
		assert.NotContains(t, got, "hunter2hunter2")
		assert.Contains(t, got, "[REDACTED]")
		assert.True(t, strings.HasPrefix(got, "model API: 401"), got)
	})
	t.Run("a multi-line PEM block is redacted after single-lining", func(t *testing.T) {
		got := formatHarnessErrorReason("model API",
			"bad key:\n-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----\ndone")
		assert.NotContains(t, got, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC")
		assert.Contains(t, got, "[REDACTED]")
	})
	t.Run("capped at 300 runes on a rune boundary", func(t *testing.T) {
		got := formatHarnessErrorReason("model API", strings.Repeat("é ", 400))
		assert.True(t, utf8.ValidString(got))
		assert.Equal(t, harnessErrorReasonMaxRunes, utf8.RuneCountInString(got))
		assert.True(t, strings.HasSuffix(got, "…"), got)
	})
}

// writeAgentInfo writes info as the handler's agent-info.json.
func writeAgentInfoJSON(t *testing.T, h *StatusHandler, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(h.StatusPath, []byte(body), 0o644))
}

func newTempStatusHandler(t *testing.T) *StatusHandler {
	t.Helper()
	return &StatusHandler{StatusPath: filepath.Join(t.TempDir(), "agent-info.json")}
}

func turnEndErrorEvent(errText string) *hooks.Event {
	return &hooks.Event{Name: hooks.EventAgentEnd, Dialect: "opencode", Data: hooks.EventData{Error: errText}}
}

func TestStatusHandler_TurnEndErrorSetsHarnessError(t *testing.T) {
	for _, from := range []string{"working", "thinking", "executing", "blocked", "waiting_for_input", "initializing"} {
		t.Run("from "+from, func(t *testing.T) {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, `{"phase":"running","activity":"`+from+`","detail":{"message":"Ready"}}`)

			require.NoError(t, h.Handle(turnEndErrorEvent("APIError (status 400)")))

			info := readAgentInfoMap(t, h.StatusPath)
			assert.Equal(t, "harness_error", info["activity"])
			assert.Equal(t, "running", info["phase"])
			assert.Equal(t, "model API: APIError (status 400)", detailMessage(info))
		})
	}
	for _, from := range []string{"completed", "limits_exceeded", "crashed"} {
		t.Run("keeps "+from, func(t *testing.T) {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, `{"phase":"running","activity":"`+from+`"}`)

			require.NoError(t, h.Handle(turnEndErrorEvent("APIError (status 400)")))

			info := readAgentInfoMap(t, h.StatusPath)
			assert.Equal(t, from, info["activity"])
			assert.Empty(t, detailMessage(info))
		})
	}
	t.Run("a sub-agent's failed stop is not the parent's turn", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, `{"phase":"running","activity":"working"}`)
		event := &hooks.Event{Name: hooks.EventAgentEnd, Dialect: "claude", Data: hooks.EventData{
			SessionID: "parent", Error: "boom", Raw: map[string]interface{}{"agent_id": "child"},
		}}

		require.NoError(t, h.Handle(event))

		assert.Equal(t, "working", readAgentInfoMap(t, h.StatusPath)["activity"])
	})
	t.Run("model-end and tool-end errors do not set it", func(t *testing.T) {
		for _, name := range []string{hooks.EventModelEnd, hooks.EventToolEnd} {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, `{"phase":"running","activity":"thinking"}`)
			require.NoError(t, h.Handle(&hooks.Event{Name: name, Data: hooks.EventData{Error: "503"}}))
			assert.Equal(t, "working", readAgentInfoMap(t, h.StatusPath)["activity"], name)
		}
	})
}

// TestStatusHandler_HarnessErrorStickyPolicy covers clearHarnessError: every
// turn-progress hook clears harness_error and drops its reason; end-of-turn
// working, an errored model-end, readiness and an in-place session restart
// keep it.
func TestStatusHandler_HarnessErrorStickyPolicy(t *testing.T) {
	const errored = `{"phase":"running","activity":"harness_error","detail":{"message":"model API: APIError (status 400)"}}`

	clears := []struct {
		name  string
		event *hooks.Event
		want  string
	}{
		{"prompt-submit", &hooks.Event{Name: hooks.EventPromptSubmit}, "thinking"},
		{"agent-start", &hooks.Event{Name: hooks.EventAgentStart}, "thinking"},
		{"new session-start", &hooks.Event{Name: hooks.EventSessionStart, Data: hooks.EventData{Source: "startup"}}, "working"},
		{"model-start", &hooks.Event{Name: hooks.EventModelStart}, "thinking"},
		{"error-free model-end", &hooks.Event{Name: hooks.EventModelEnd}, "working"},
		{"tool-start", &hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "bash"}}, "executing"},
	}
	for _, tt := range clears {
		t.Run(tt.name+" clears", func(t *testing.T) {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, errored)

			require.NoError(t, h.Handle(tt.event))

			info := readAgentInfoMap(t, h.StatusPath)
			assert.Equal(t, tt.want, info["activity"])
			assert.Empty(t, detailMessage(info), "the cleared reason must not linger")
		})
	}

	keeps := []struct {
		name  string
		event *hooks.Event
	}{
		{"agent-end (end-of-turn working)", &hooks.Event{Name: hooks.EventAgentEnd}},
		{"tool-end", &hooks.Event{Name: hooks.EventToolEnd}},
		{"model-end with an error", &hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Error: "APIError (status 400)"}}},
		{"uncounted model-end with no error", &hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Uncounted: true}}},
		{"model-end with a whitespace-only error", &hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Error: " \n\t"}}},
		{"in-place session-start (compact)", &hooks.Event{Name: hooks.EventSessionStart, Dialect: "claude", Data: hooks.EventData{Source: "compact"}}},
		{"sub-agent tool-start", &hooks.Event{Name: hooks.EventToolStart, Dialect: "claude", Data: hooks.EventData{
			SessionID: "parent", ToolName: "Bash", Raw: map[string]interface{}{"agent_id": "child"},
		}}},
		{"sub-agent model-end", &hooks.Event{Name: hooks.EventModelEnd, Dialect: "claude", Data: hooks.EventData{
			SessionID: "parent", Raw: map[string]interface{}{"agent_id": "child"},
		}}},
	}
	for _, tt := range keeps {
		t.Run(tt.name+" keeps", func(t *testing.T) {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, errored)

			require.NoError(t, h.Handle(tt.event))

			info := readAgentInfoMap(t, h.StatusPath)
			assert.Equal(t, "harness_error", info["activity"])
			assert.Equal(t, "model API: APIError (status 400)", detailMessage(info))
		})
	}

	t.Run("readiness keeps", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, errored)
		replaced, err := h.ReplaceActivityIf(state.ActivityInitializing, state.ActivityWorking)
		require.NoError(t, err)
		assert.False(t, replaced)
		assert.Equal(t, "harness_error", readAgentInfoMap(t, h.StatusPath)["activity"])
	})

	t.Run("a write with its own message keeps that message", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, errored)
		require.NoError(t, h.UpdatePhaseAndMessage(state.PhaseError, "", "Agent crashed"))
		assert.Equal(t, "Agent crashed", detailMessage(readAgentInfoMap(t, h.StatusPath)))
	})
}

// TestHubHandler_TurnEndErrorReportsHarnessError runs the StatusHandler and
// HubHandler in hook order and checks the Hub follows the local result.
func TestHubHandler_TurnEndErrorReportsHarnessError(t *testing.T) {
	t.Run("reports harness_error with the redacted reason", func(t *testing.T) {
		sh, hh, reports := hubRecorderSetup(t)
		writeAgentInfoJSON(t, sh, `{"phase":"running","activity":"working"}`)
		event := turnEndErrorEvent("400 Bad Request\nAuthorization: Bearer sk-abcdefghijklmnopqrstuvwxyz0123")

		require.NoError(t, sh.Handle(event))
		require.NoError(t, hh.Handle(event))

		got := reports()
		require.Len(t, got, 1)
		assert.Equal(t, "harness_error", got[0].Activity)
		assert.Equal(t, "harness_error", got[0].Status)
		assert.True(t, strings.HasPrefix(got[0].Message, "model API: 400 Bad Request"), got[0].Message)
		assert.NotContains(t, got[0].Message, "sk-abcdefghijklmnopqrstuvwxyz0123")
		assert.NotContains(t, got[0].Message, "\n")
		assert.Equal(t, got[0].Message, detailMessage(readAgentInfoMap(t, sh.StatusPath)))
	})
	t.Run("sends nothing when the local status is held", func(t *testing.T) {
		sh, hh, reports := hubRecorderSetup(t)
		writeAgentInfoJSON(t, sh, `{"phase":"running","activity":"completed"}`)
		event := turnEndErrorEvent("APIError (status 400)")

		require.NoError(t, sh.Handle(event))
		require.NoError(t, hh.Handle(event))

		assert.Empty(t, reports())
	})
	t.Run("end-of-turn working is not sent over harness_error", func(t *testing.T) {
		sh, hh, reports := hubRecorderSetup(t)
		writeAgentInfoJSON(t, sh, `{"phase":"running","activity":"harness_error"}`)
		for _, event := range []*hooks.Event{
			{Name: hooks.EventAgentEnd},
			{Name: hooks.EventToolEnd},
			{Name: hooks.EventModelEnd, Data: hooks.EventData{Error: "APIError (status 400)"}},
		} {
			require.NoError(t, sh.Handle(event))
			require.NoError(t, hh.Handle(event))
		}
		assert.Empty(t, reports())
	})
	t.Run("model-start clears it on the Hub", func(t *testing.T) {
		sh, hh, reports := hubRecorderSetup(t)
		writeAgentInfoJSON(t, sh, `{"phase":"running","activity":"harness_error"}`)
		event := &hooks.Event{Name: hooks.EventModelStart}

		require.NoError(t, sh.Handle(event))
		require.NoError(t, hh.Handle(event))

		got := reports()
		require.Len(t, got, 1)
		assert.Equal(t, "thinking", got[0].Activity)
	})
}

// TestOpencodeHarnessErrorEndToEnd replays the OpenCode bridge's payloads
// through the real dialect.yaml, StatusHandler and HubHandler: a 400 turn's
// gated session.idle carrying the bridge's error sets harness_error on the
// Hub with the reason, the end-of-turn events that follow keep it, and a
// follow-up text-only turn (one step-finish, then a clean session.idle)
// clears it. OpenCode emits no prompt-submit, agent-start or model-start
// on a later turn, so the error-free model-end is what clears it.
func TestOpencodeHarnessErrorEndToEnd(t *testing.T) {
	sh, hh, reports := hubRecorderSetup(t)
	md := loadOpencodeDialect(t)
	writeAgentInfoJSON(t, sh, `{"phase":"running","activity":"working","detail":{"message":"Harness started"}}`)

	var stepFinish map[string]interface{}
	for _, rec := range loadOpencodeHookPayloadFixture(t) {
		if rec["hook_event_name"] == "message.part.updated.step-finish" {
			stepFinish = rec
			break
		}
	}
	require.NotNil(t, stepFinish)

	step := func(payload map[string]interface{}) []hubReport {
		t.Helper()
		before := len(reports())
		event, err := md.Parse(payload)
		require.NoError(t, err)
		require.NoError(t, sh.Handle(event))
		require.NoError(t, hh.Handle(event))
		return reports()[before:]
	}

	// 1. The 400 turn ends: the bridge attaches "APIError (status 400)".
	got := step(map[string]interface{}{
		"hook_event_name": "session.idle",
		"session_id":      "ses_err",
		"error":           "APIError (status 400)",
	})
	require.Len(t, got, 1)
	assert.Equal(t, hubReport{Activity: "harness_error", Status: "harness_error", Message: "model API: APIError (status 400)"}, got[0])
	assert.Equal(t, "harness_error", readAgentInfoMap(t, sh.StatusPath)["activity"])

	// 2. A clean session.idle (end-of-turn working) does not clear it.
	assert.Empty(t, step(map[string]interface{}{"hook_event_name": "session.idle", "session_id": "ses_err"}))
	assert.Equal(t, "harness_error", readAgentInfoMap(t, sh.StatusPath)["activity"])

	// 3. A follow-up text-only turn: its step-finish clears it.
	got = step(stepFinish)
	require.Len(t, got, 1)
	assert.Equal(t, "working", got[0].Activity)
	info := readAgentInfoMap(t, sh.StatusPath)
	assert.Equal(t, "working", info["activity"])
	assert.Empty(t, detailMessage(info))
}

func TestFormatHarnessTailReason(t *testing.T) {
	t.Run("short tail is kept whole", func(t *testing.T) {
		assert.Equal(t, "Agent crashed with exit code 1: a | b", FormatHarnessTailReason("Agent crashed with exit code 1", "a | b"))
	})
	t.Run("empty tail leaves the source", func(t *testing.T) {
		assert.Equal(t, "Agent crashed with exit code 1", FormatHarnessTailReason("Agent crashed with exit code 1", " \x1b[0m "))
	})
	t.Run("long tail is cut from its start", func(t *testing.T) {
		got := FormatHarnessTailReason("src", strings.Repeat("old ", 200)+"| Error: the end")
		assert.LessOrEqual(t, utf8.RuneCountInString(got), harnessErrorReasonMaxRunes)
		assert.True(t, strings.HasPrefix(got, "src: …"), got)
		assert.True(t, strings.HasSuffix(got, "| Error: the end"), got)
	})
	t.Run("redaction happens before the cut", func(t *testing.T) {
		// A credential straddling the cut point must not leave a
		// readable piece behind.
		// Sanitizing a cut text would miss it: the key's remaining
		// end is too short for the long-run rule.
		const key = "aB3dE5fG7hJ9kL1mN2pQ4rS6tU8vW0xYz2C4e6G8i0K2m4O6q8"
		got := FormatHarnessTailReason("src", "token sk-ant-api03-"+key+" "+strings.Repeat("y", 270)+" done")
		assert.LessOrEqual(t, utf8.RuneCountInString(got), harnessErrorReasonMaxRunes)
		assert.NotContains(t, got, key[len(key)-12:])
		assert.True(t, strings.HasSuffix(got, " done"), got)
	})
}

// TestStatusHandler_TurnEndErrorOutsideRunningIsIgnored checks the phase
// guard of setHarnessError: a turn error reported while the agent stops,
// or after it stopped, does not write harness_error, and the Hub is not
// told. A recorded running phase, or none, still writes it.
func TestStatusHandler_TurnEndErrorOutsideRunningIsIgnored(t *testing.T) {
	for _, phase := range []string{"stopping", "stopped", "error"} {
		t.Run(phase, func(t *testing.T) {
			sh, hh, reports := hubRecorderSetup(t)
			writeAgentInfoJSON(t, sh, `{"phase":"`+phase+`","activity":"working"}`)
			event := &hooks.Event{Name: hooks.EventAgentError, Dialect: "claude", Data: hooks.EventData{Error: "API Error: 400"}}

			require.NoError(t, sh.Handle(event))
			require.NoError(t, hh.Handle(event))

			info := readAgentInfoMap(t, sh.StatusPath)
			assert.Equal(t, phase, info["phase"])
			assert.Equal(t, "working", info["activity"])
			assert.Empty(t, reports())
		})
	}
	t.Run("no recorded phase", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, `{"activity":"working"}`)
		require.NoError(t, h.Handle(turnEndErrorEvent("APIError (status 400)")))
		assert.Equal(t, "harness_error", readAgentInfoMap(t, h.StatusPath)["activity"])
	})
}

// TestStatusHandler_IdleAndHarnessError pins how the agent-reported idle
// activity (ptone/scion#7826) and harness_error interact. idle is cleared
// only by the new-work events (isNewWorkEvent), like blocked: the
// harness-progress events that clear harness_error (model-start, an
// error-free model-end, tool-start) keep it. A turn that ends on an error
// after idle is newer than idle and replaces it, as it replaces blocked.
// The watchdogs never replace idle: see the idle rows of
// TestAPIErrorWatchdog_Guards and TestFirstTurnWatchdog's sticky table.
func TestStatusHandler_IdleAndHarnessError(t *testing.T) {
	const idle = `{"phase":"running","activity":"idle","detail":{"message":"no task assigned"}}`
	for _, tt := range []struct {
		name  string
		event *hooks.Event
	}{
		{"model-start", &hooks.Event{Name: hooks.EventModelStart}},
		{"error-free model-end", &hooks.Event{Name: hooks.EventModelEnd}},
		{"tool-start", &hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "bash"}}},
	} {
		t.Run(tt.name+" keeps idle", func(t *testing.T) {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, idle)
			require.NoError(t, h.Handle(tt.event))
			assert.Equal(t, "idle", readAgentInfoMap(t, h.StatusPath)["activity"])
		})
	}
	t.Run("prompt-submit clears idle", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, idle)
		require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventPromptSubmit}))
		assert.Equal(t, "thinking", readAgentInfoMap(t, h.StatusPath)["activity"])
	})
	t.Run("a turn-end error replaces idle", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, idle)
		require.NoError(t, h.Handle(turnEndErrorEvent("APIError (status 400)")))
		info := readAgentInfoMap(t, h.StatusPath)
		assert.Equal(t, "harness_error", info["activity"])
		assert.Equal(t, "model API: APIError (status 400)", detailMessage(info))
	})
}
