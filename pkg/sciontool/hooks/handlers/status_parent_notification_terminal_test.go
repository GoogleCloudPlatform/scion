/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the parent agent's own Notification (no sub-agent)
// meeting a status the agent holds, for every dialect that emits one:
//   - limits_exceeded and crashed (the terminal statuses) stay as they are:
//     agent-info.json is not written and the Hub gets no update;
//   - working, blocked and completed still move to a notification-set
//     waiting_for_input that is reported to the Hub;
//   - the parent's own question (a claude AskUserQuestion or ExitPlanMode)
//     still moves blocked and completed to waiting_for_input.
// All payloads are synthetic and go through the real dialects.

type parentNotificationDialect struct {
	name   string
	notify func(t *testing.T) *hooks.Event
}

// parentNotificationParse parses payload with d. Callers check the result
// with requireParentNotification.
func parentNotificationParse(t *testing.T, d hooks.Dialect, payload map[string]interface{}) *hooks.Event {
	t.Helper()
	event, err := d.Parse(payload)
	require.NoError(t, err)
	return event
}

// parentNotificationMapping loads a harness's YAML dialect.
func parentNotificationMapping(t *testing.T, name string) hooks.Dialect {
	t.Helper()
	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, name))
	require.NoError(t, err)
	return md
}

// parentNotificationDialects are the parent Notifications of every dialect
// that maps an event to hooks.EventNotification.
func parentNotificationDialects() []parentNotificationDialect {
	return []parentNotificationDialect{
		{"claude", func(t *testing.T) *hooks.Event {
			return parentHeldParent(t, "Notification", "")
		}},
		{"gemini", func(t *testing.T) *hooks.Event {
			return parentNotificationParse(t, dialects.NewGeminiDialect(), map[string]interface{}{
				"hook_event_name": "Notification", "session_id": "gemini-synthetic-session", "message": "Approve tool?",
			})
		}},
		{"codex built-in", func(t *testing.T) *hooks.Event {
			return parentNotificationParse(t, dialects.NewCodexDialect(), map[string]interface{}{
				"type": "notification", "session_id": "codex-synthetic-session", "message": "Approve command?",
			})
		}},
		{"codex mapping", func(t *testing.T) *hooks.Event {
			return codexParentEvent(t, "notification")
		}},
		{"copilot", func(t *testing.T) *hooks.Event {
			return parentNotificationParse(t, parentNotificationMapping(t, "copilot"), map[string]interface{}{
				"hook_event_name": "errorOccurred", "sessionId": "copilot-synthetic-session",
				"error": map[string]interface{}{"message": "synthetic error"},
			})
		}},
		{"grok-build", func(t *testing.T) *hooks.Event {
			return parentNotificationParse(t, parentNotificationMapping(t, "grok-build"), map[string]interface{}{
				"hookEventName": "Notification", "message": "permission",
			})
		}},
		{"opencode", func(t *testing.T) *hooks.Event {
			return parentNotificationParse(t, parentNotificationMapping(t, "opencode"), map[string]interface{}{
				"hook_event_name": "permission.asked", "message": "permission",
			})
		}},
		{"muse-code", func(t *testing.T) *hooks.Event {
			return parentNotificationParse(t, parentNotificationMapping(t, "muse-code"), map[string]interface{}{
				"hook_event_name": "PermissionRequest", "tool_name": "Bash",
			})
		}},
	}
}

// requireParentNotification checks that event is the parent's Notification.
func requireParentNotification(t *testing.T, event *hooks.Event) {
	t.Helper()
	require.Equal(t, hooks.EventNotification, event.Name, "a Notification event")
	require.False(t, isSubagentEvent(event), "the parent's own event")
}

// A parent Notification leaves limits_exceeded and crashed unchanged:
// agent-info.json is byte for byte the same, it has no notification marker,
// and the Hub gets no update.
func TestParentNotificationTerminal_TerminalKept(t *testing.T) {
	for _, d := range parentNotificationDialects() {
		for _, held := range []state.Activity{state.ActivityLimitsExceeded, state.ActivityCrashed} {
			t.Run(d.name+"/"+string(held), func(t *testing.T) {
				event := d.notify(t)
				requireParentNotification(t, event)

				sh, hh, sent := claudeHubSetup(t)
				parentHeldHold(t, sh, string(held))
				before, err := os.ReadFile(sh.StatusPath)
				require.NoError(t, err)

				got, info := parentHeldStep(t, sh, hh, sent, event)
				assert.Equal(t, string(held), info["activity"], "the terminal status is kept")
				assert.NotContains(t, info, activitySourceKey, "no notification marker")
				assert.Empty(t, got, "no hub update")
				after, err := os.ReadFile(sh.StatusPath)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "agent-info.json is not written")
			})
		}
	}
}

// A parent Notification over working still sets a notification-set
// waiting_for_input and reports it, as before.
func TestParentNotificationTerminal_WorkingBecomesWaiting(t *testing.T) {
	for _, d := range parentNotificationDialects() {
		t.Run(d.name, func(t *testing.T) {
			event := d.notify(t)
			requireParentNotification(t, event)

			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, string(state.ActivityWorking))

			got, info := parentHeldStep(t, sh, hh, sent, event)
			assert.Equal(t, string(state.ActivityWaitingForInput), info["activity"])
			assert.Equal(t, activitySourceNotification, info[activitySourceKey])
			assert.Equal(t, []string{parentHeldWaiting}, got)
		})
	}
}

// A parent Notification still replaces blocked and completed. For opencode
// (permission.asked) and muse-code (PermissionRequest) the Notification is
// the question that asks the user, so the agent really is waiting again.
func TestParentNotificationTerminal_BlockedCompletedStillWaiting(t *testing.T) {
	for _, d := range parentNotificationDialects() {
		for _, held := range []state.Activity{state.ActivityBlocked, state.ActivityCompleted} {
			t.Run(d.name+"/"+string(held), func(t *testing.T) {
				event := d.notify(t)
				requireParentNotification(t, event)

				sh, hh, sent := claudeHubSetup(t)
				parentHeldHold(t, sh, string(held))

				got, info := parentHeldStep(t, sh, hh, sent, event)
				assert.Equal(t, string(state.ActivityWaitingForInput), info["activity"])
				assert.Equal(t, activitySourceNotification, info[activitySourceKey])
				assert.Equal(t, []string{parentHeldWaiting}, got)
			})
		}
	}
}

// The parent's own question (a claude AskUserQuestion or ExitPlanMode
// tool-start) still moves blocked and completed to an agent-set
// waiting_for_input and reports it.
func TestParentNotificationTerminal_ParentQuestionMovesHeld(t *testing.T) {
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		for _, held := range []state.Activity{state.ActivityBlocked, state.ActivityCompleted} {
			t.Run(tool+"/"+string(held), func(t *testing.T) {
				event := parentHeldParent(t, "PreToolUse", tool)
				require.True(t, isClaudeUserWaitTool(event), "a question event")
				require.False(t, isSubagentEvent(event), "the parent's own event")

				sh, hh, sent := claudeHubSetup(t)
				parentHeldHold(t, sh, string(held))

				got, info := parentHeldStep(t, sh, hh, sent, event)
				assert.Equal(t, string(state.ActivityWaitingForInput), info["activity"])
				assert.NotContains(t, info, activitySourceKey, "the wait is agent-set")
				assert.Equal(t, []string{parentHeldWaiting}, got)
			})
		}
	}
}
