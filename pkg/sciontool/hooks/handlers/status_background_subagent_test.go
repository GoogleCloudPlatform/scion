/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover a wait that sub-agents hold (a notification-set
// waiting_for_input with a non-empty owner list) against the parent's own
// events. A background sub-agent keeps running after the parent's turn
// ends, so its question must survive the parent's Stop and the parent's
// tool activity, and is released by the sub-agent's own answer or its
// SubagentStop. The parent's next prompt, session start and any status
// write still clear it, which bounds a sub-agent that never sends an end
// event. Local status and Hub report are checked together. Payloads are
// synthetic and go through the real claude dialect.

// bgAgentTool parses the parent's Agent tool event for a background
// sub-agent launch (tool_input.run_in_background true, as in the captured
// fixture's shape).
func bgAgentTool(t *testing.T, hookEventName string) *hooks.Event {
	t.Helper()
	event, err := dialects.NewClaudeDialect().Parse(map[string]interface{}{
		"hook_event_name": hookEventName,
		"session_id":      claudeSubagentParentSession,
		"cwd":             "/workspace",
		"tool_name":       "Agent",
		"tool_input": map[string]interface{}{
			"description":       "Background task",
			"prompt":            "synthetic prompt",
			"run_in_background": true,
			"subagent_type":     "general-purpose",
		},
	})
	require.NoError(t, err)
	return event
}

// bgSubagentStop is a sub-agent's SubagentStop.
func bgSubagentStop(t *testing.T, agentID string) *hooks.Event {
	t.Helper()
	e := parentHeldSub(t, "SubagentStop", "", agentID)
	require.Equal(t, hooks.EventSubagentEnd, e.Name)
	require.True(t, isSubagentEvent(e))
	return e
}

// bgHeldAfterStop launches a background sub-agent, lets each of ids ask,
// and ends the parent's turn. It checks that the wait survived the Stop.
func bgHeldAfterStop(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string, ids ...string) {
	t.Helper()
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, bgAgentTool(t, "PreToolUse"))
	parentHeldStep(t, sh, hh, sent, bgAgentTool(t, "PostToolUse"))
	for _, id := range ids {
		parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", id))
	}
	got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Stop", ""))
	require.Equal(t, "waiting_for_input", info["activity"], "parent Stop keeps the wait")
	require.Equal(t, ids, waitOwnersOf(info))
	require.Empty(t, got)
}

// The parent's Stop keeps a wait that a background sub-agent holds: the
// activity, the notification marker and the owner list stay, and no Hub
// update replaces the waiting_for_input.
func TestBackgroundSubagentWait_SurvivesParentStop(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, bgAgentTool(t, "PreToolUse"))
	parentHeldStep(t, sh, hh, sent, bgAgentTool(t, "PostToolUse"))

	got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))
	require.Equal(t, []string{parentHeldWaiting}, got)
	require.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Stop", ""))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, activitySourceNotification, info[activitySourceKey])
	assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
	assert.Empty(t, got, "no working report replaces the wait")
}

// The parent's own tool activity keeps a wait that sub-agents hold, during
// the turn and after its Stop, with no Hub update.
func TestBackgroundSubagentWait_ParentToolActivityKeeps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(*testing.T) *hooks.Event
	}{
		{"tool-start Bash", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PreToolUse", "Bash") }},
		{"tool-start Agent", func(t *testing.T) *hooks.Event { return bgAgentTool(t, "PreToolUse") }},
		{"tool-end Bash", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PostToolUse", "Bash") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "working")
			parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))

			got, info := parentHeldStep(t, sh, hh, sent, tc.event(t))
			assert.Equal(t, "waiting_for_input", info["activity"], "during the turn")
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.Empty(t, got)

			parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Stop", ""))
			got, info = parentHeldStep(t, sh, hh, sent, tc.event(t))
			assert.Equal(t, "waiting_for_input", info["activity"], "after the parent's Stop")
			assert.Equal(t, activitySourceNotification, info[activitySourceKey])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.NotContains(t, info, "toolName")
			assert.Empty(t, got)
		})
	}
}

// After the parent's Stop, each owner's SubagentStop removes only its own
// entry; the last owner's clears the wait to working and reports it once.
// A SubagentStop from a sub-agent that is not listed, after a later write,
// and a SubagentStop without agent_id send nothing.
func TestBackgroundSubagentWait_LastOwnerSubagentStopReleases(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	bgHeldAfterStop(t, sh, hh, sent, parentHeldSubA, parentHeldSubB)

	got, info := parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"], "B still holds the wait")
	assert.Equal(t, []string{parentHeldSubB}, waitOwnersOf(info))
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubB))
	assert.Equal(t, "working", info["activity"])
	assert.Equal(t, activitySourceSubagentEnd, info[activitySourceKey])
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.Equal(t, []string{parentHeldWorking}, got, "the release is reported once")

	// A parent event never reports a release, even with the marker set.
	parentStop, err := dialects.NewClaudeDialect().Parse(map[string]interface{}{
		"hook_event_name": "SubagentStop",
		"session_id":      claudeSubagentParentSession,
	})
	require.NoError(t, err)
	require.Equal(t, hooks.EventSubagentEnd, parentStop.Name)
	require.False(t, isSubagentEvent(parentStop))
	got, info = parentHeldStep(t, sh, hh, sent, parentStop)
	assert.Equal(t, activitySourceSubagentEnd, info[activitySourceKey])
	assert.Empty(t, got, "no agent_id: not a sub-agent's event")

	// Any later write removes the marker, so an unlisted SubagentStop then
	// sends nothing.
	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
	assert.Equal(t, "executing", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
	assert.Equal(t, []string{parentHeldExecuting}, got)

	got, info = parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubC))
	assert.Equal(t, "executing", info["activity"])
	assert.Empty(t, got)
}

// Documented behaviour: the release marker stays until the next local
// write, so a second sub-agent's SubagentStop right after a release re-sends
// working. The re-send repeats the status the Hub already has.
func TestBackgroundSubagentWait_ReleaseMarkerResend(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	bgHeldAfterStop(t, sh, hh, sent, parentHeldSubA)

	got, _ := parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))
	require.Equal(t, []string{parentHeldWorking}, got)

	got, info := parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubC))
	assert.Equal(t, "working", info["activity"])
	assert.Equal(t, activitySourceSubagentEnd, info[activitySourceKey], "an unlisted SubagentStop writes nothing")
	assert.Equal(t, []string{parentHeldWorking}, got, "idempotent re-send")
}

// After the parent's Stop, the owner's own answer clears the wait as
// before: an accepted tool (PostToolUse) or a rejected one
// (PostToolUseFailure).
func TestBackgroundSubagentWait_OwnAnswerReleases(t *testing.T) {
	for _, raw := range []string{"PostToolUse", "PostToolUseFailure"} {
		t.Run(raw, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			bgHeldAfterStop(t, sh, hh, sent, parentHeldSubA)

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, raw, "Bash", parentHeldSubA))
			assert.Equal(t, "working", info["activity"])
			assert.NotContains(t, info, activitySourceKey)
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.Equal(t, []string{parentHeldWorking}, got)
		})
	}
}

// A stale owner (it never sends an end event) holds the wait through the
// parent's Stop and tool activity, and is released by the parent's next
// prompt, a session start or a status write. The prompt row runs both
// handlers and checks the Hub report too. The session start and status
// write rows call only the StatusHandler, so they check local state only
// (wantHub is nil and no Hub assertion is made).
func TestBackgroundSubagentWait_SafetyNetReleasesStaleOwner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		release   func(*testing.T, *StatusHandler, *HubHandler, func() []string) []string
		wantLocal string
		wantHub   []string // nil: local state only
	}{
		{"parent prompt", func(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string) []string {
			got, _ := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "UserPromptSubmit", ""))
			return got
		}, "thinking", []string{parentHeldThinking}},
		{"session start", func(t *testing.T, sh *StatusHandler, _ *HubHandler, _ func() []string) []string {
			require.NoError(t, sh.Handle(parentHeldParent(t, "SessionStart", "")))
			return nil
		}, "working", nil},
		{"status write", func(t *testing.T, sh *StatusHandler, _ *HubHandler, _ func() []string) []string {
			require.NoError(t, sh.UpdateActivity(state.ActivityBlocked, ""))
			return nil
		}, "blocked", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			bgHeldAfterStop(t, sh, hh, sent, parentHeldSubA)

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
			require.Equal(t, "waiting_for_input", info["activity"], "parent tool-start does not release it")
			require.Empty(t, got)

			got = tc.release(t, sh, hh, sent)
			info = readAgentInfoMap(t, sh.StatusPath)
			assert.Equal(t, tc.wantLocal, info["activity"])
			assert.NotContains(t, info, activitySourceKey)
			assert.NotContains(t, info, notificationWaitAgentsKey)
			if tc.wantHub != nil {
				assert.Equal(t, tc.wantHub, got)
			}
		})
	}
}

// Waits with no owner list behave as before: the parent's tool-start clears
// an agent-set wait and an owner-less notification wait, and the parent's
// Stop clears an owner-less notification wait.
func TestBackgroundSubagentWait_ParentSetWaitsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		held      string
		event     func(*testing.T) *hooks.Event
		wantLocal string
		wantHub   string
	}{
		{"agent-set wait, parent tool-start", "waiting_for_input", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PreToolUse", "Bash") }, "executing", parentHeldExecuting},
		{"agent-set wait, parent Stop", "waiting_for_input", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "Stop", "") }, "waiting_for_input", ""},
		{"notification wait, parent tool-start", "notification", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PreToolUse", "Bash") }, "executing", parentHeldExecuting},
		{"notification wait, parent Stop", "notification", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "Stop", "") }, "working", parentHeldWorking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, tc.held)

			got, info := parentHeldStep(t, sh, hh, sent, tc.event(t))
			assert.Equal(t, tc.wantLocal, info["activity"])
			assert.NotContains(t, info, notificationWaitAgentsKey)
			if tc.wantHub == "" {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, []string{tc.wantHub}, got)
			}
		})
	}
}

// A foreground sub-agent asks, the user answers, the sub-agent ends, and
// the parent's Agent tool returns before its Stop. The wait is reported
// once and released once (at the answer); the SubagentStop of a sub-agent
// that no longer holds the wait sends nothing; the end state is working.
// When the sub-agent ends with its question still open, its SubagentStop
// releases the wait instead, again reported once.
func TestBackgroundSubagentWait_ForegroundSequence(t *testing.T) {
	t.Run("answered then SubagentStop", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "working")
		steps := []struct {
			event     *hooks.Event
			wantLocal string
			wantHub   []string
		}{
			{parentHeldParent(t, "PreToolUse", "Agent"), "executing", []string{parentHeldExecuting}},
			{parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA), "waiting_for_input", []string{parentHeldWaiting}},
			{parentHeldSub(t, "PostToolUse", "AskUserQuestion", parentHeldSubA), "working", []string{parentHeldWorking}},
			{bgSubagentStop(t, parentHeldSubA), "working", nil},
			{parentHeldParent(t, "PostToolUse", "Agent"), "working", []string{parentHeldWorking}},
			{parentHeldParent(t, "Stop", ""), "working", []string{parentHeldWorking}},
		}
		for i, s := range steps {
			got, info := parentHeldStep(t, sh, hh, sent, s.event)
			assert.Equal(t, s.wantLocal, info["activity"], "step %d %s", i, s.event.RawName)
			if s.wantHub == nil {
				assert.Empty(t, got, "step %d %s", i, s.event.RawName)
			} else {
				assert.Equal(t, s.wantHub, got, "step %d %s", i, s.event.RawName)
			}
		}
		info := readAgentInfoMap(t, sh.StatusPath)
		assert.NotContains(t, info, activitySourceKey)
		assert.NotContains(t, info, notificationWaitAgentsKey)
		assert.Equal(t, 1, countString(sent(), parentHeldWaiting), "the wait is reported once")
	})

	t.Run("SubagentStop with the question open", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "working")
		parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Agent"))
		parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))

		got, info := parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))
		assert.Equal(t, "working", info["activity"])
		assert.Equal(t, []string{parentHeldWorking}, got, "the release is reported once")

		got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PostToolUse", "Agent"))
		assert.Equal(t, "working", info["activity"])
		assert.NotContains(t, info, activitySourceKey)
		assert.Equal(t, []string{parentHeldWorking}, got)

		got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Stop", ""))
		assert.Equal(t, "working", info["activity"])
		assert.Equal(t, []string{parentHeldWorking}, got)
	})
}

// Known limitation, pinned so it stays visible: the parent's own
// Notification (for example a permission prompt) during a wait that a
// sub-agent holds is folded into that wait and keeps the owner list, with
// nothing recording that the parent is waiting too. When the last owner then
// ends without answering, its SubagentStop releases the wait to working and
// reports working to the Hub, although the parent's prompt may still be
// open. Fixing it needs a change on the question path
// (setWaitingFromNotificationFor), which is out of scope here.
func TestBackgroundSubagentWait_ParentNotificationFoldedIntoOwnerHeldWait(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))

	got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Notification", ""))
	require.Equal(t, "waiting_for_input", info["activity"])
	require.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info), "the parent's prompt is folded into A's wait")
	require.Equal(t, []string{parentHeldWaiting}, got)

	got, info = parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))
	assert.Equal(t, "working", info["activity"], "limitation: the parent's open prompt is not kept")
	assert.Equal(t, activitySourceSubagentEnd, info[activitySourceKey])
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.Equal(t, []string{parentHeldWorking}, got)
}

func countString(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
