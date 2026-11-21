/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"path/filepath"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover agent-set ask_user waits (activitySource == "ask-user",
// ptone/scion#8000):
//   - same-turn follow-up tool calls (e.g. `scion message` after `ask_user`)
//     keep waiting_for_input locally and on the Hub;
//   - a parent EventAgentEnd (Stop / session.idle) keeps waiting_for_input and
//     arms askUserTurnEnded: true;
//   - on a subsequent turn (after askUserTurnEnded: true), parent tool-start,
//     model-start, or agent-end clears the wait locally and on the Hub;
//   - new-work events (UserPromptSubmit, AgentStart, new SessionStart) clear
//     it immediately in any turn;
//   - Claude ExitPlanMode/AskUserQuestion stay unmarked and keep their
//     immediate clear-on-tool-start behavior;
//   - sub-agent questions during an ask-user wait preserve parentWaitAskUser
//     and askUserTurnEnded across restoreParentWait.

// AC1 & AC2: ask_user -> PreToolUse(Bash) -> PostToolUse -> Stop ends in
// waiting_for_input (with askUserTurnEnded: true) with no Hub overwrites;
// then UserPromptSubmit clears to thinking on both local and Hub.
func TestAskUser_SameTurnToolsKeepWaitThenPromptClears(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "ask_user")

	// Same-turn tool-start (e.g. `scion message`), tool-end, and Stop all keep
	// waiting_for_input and send no Hub updates.
	for _, ev := range []*hooks.Event{
		parentHeldParent(t, "PreToolUse", "Bash"),
		parentHeldParent(t, "PostToolUse", "Bash"),
	} {
		got, info := parentHeldStep(t, sh, hh, sent, ev)
		assert.Equal(t, "waiting_for_input", info["activity"], ev.Name)
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey], ev.Name)
		assert.NotContains(t, info, askUserTurnEndedKey, ev.Name)
		assert.Empty(t, got, "no hub update during same turn (%s)", ev.Name)
	}

	// Turn ends: stays waiting_for_input and arms askUserTurnEnded.
	got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Stop", ""))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
	assert.Equal(t, true, info[askUserTurnEndedKey])
	assert.Empty(t, got, "no hub update on turn-1 Stop")

	// Next turn starts with UserPromptSubmit (Claude/Codex/...): clears wait
	// and drops both activitySource and askUserTurnEnded.
	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "UserPromptSubmit", ""))
	assert.Equal(t, "thinking", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
	assert.NotContains(t, info, askUserTurnEndedKey)
	assert.Equal(t, []string{parentHeldThinking}, got)
}

// AC1 (reportsNoBackgroundWork path): Claude Stop with background_tasks: []
// also keeps an ask-user wait and sets askUserTurnEnded: true; a second such
// Stop clears it to working.
func TestAskUser_StopWithEmptyBackgroundTasksArmsAndThenClears(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "ask_user")

	stopNoBg := waitClearClaudeEvent(t, map[string]interface{}{
		"hook_event_name":  "Stop",
		"session_id":       claudeSubagentParentSession,
		"background_tasks": []interface{}{},
	})
	require.True(t, reportsNoBackgroundWork(stopNoBg))

	// Turn 1 Stop: keeps waiting_for_input and sets askUserTurnEnded.
	got, info := parentHeldStep(t, sh, hh, sent, stopNoBg)
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
	assert.Equal(t, true, info[askUserTurnEndedKey])
	assert.Empty(t, got)

	// Turn 2 Stop (backstop): clears to working.
	got, info = parentHeldStep(t, sh, hh, sent, stopNoBg)
	assert.Equal(t, "working", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
	assert.NotContains(t, info, askUserTurnEndedKey)
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// AC3: Real Antigravity fixture (antigravity_dialect_test.go).
// Turn 1: PreInvocation(0) -> PreToolUse(run_command) -> [ask_user] ->
// PostToolUse -> PostInvocation(0) -> PreInvocation(1) -> PostInvocation(1) ->
// Stop(turn 1). Turn 1 must end in waiting_for_input (askUserTurnEnded: true)
// with zero Hub status updates after ask_user.
// Turn 2: PreInvocation(0) clears to thinking locally and reports thinking to
// the Hub; PostInvocation(0) and Stop(turn 2) report working.
func TestAskUser_AntigravityFixture(t *testing.T) {
	_, events := loadAntigravityFixture(t)
	sh, hh, sent := claudeHubSetup(t)

	// events[0]: PreInvocation(0), events[1]: PreToolUse(run_command)
	_, _ = parentHeldStep(t, sh, hh, sent, events[0])
	_, _ = parentHeldStep(t, sh, hh, sent, events[1])

	// Inside run_command, the agent runs `sciontool status ask_user`.
	parentHeldHold(t, sh, "ask_user")

	// Rest of turn 1: events[2..5] (PostToolUse, PostInvocation(0),
	// PreInvocation(1), PostInvocation(1)) keep waiting_for_input, marker unset.
	for i := 2; i <= 5; i++ {
		got, info := parentHeldStep(t, sh, hh, sent, events[i])
		assert.Equal(t, "waiting_for_input", info["activity"], "turn 1 events[%d] (%s)", i, events[i].Name)
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey], "turn 1 events[%d]", i)
		assert.NotContains(t, info, askUserTurnEndedKey, "turn 1 events[%d]", i)
		assert.Empty(t, got, "no hub update on turn 1 events[%d] (%s)", i, events[i].Name)
	}

	// events[6]: Stop (turn 1) keeps waiting_for_input and sets askUserTurnEnded.
	got, info := parentHeldStep(t, sh, hh, sent, events[6])
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
	assert.Equal(t, true, info[askUserTurnEndedKey])
	assert.Empty(t, got, "no hub update on turn 1 Stop")

	// events[7]: PreInvocation (turn 2) clears ask-user wait -> thinking on local and Hub.
	got, info = parentHeldStep(t, sh, hh, sent, events[7])
	assert.Equal(t, "thinking", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
	assert.NotContains(t, info, askUserTurnEndedKey)
	assert.Equal(t, []string{parentHeldThinking}, got)

	// events[8]: PostInvocation (turn 2) -> working, events[9]: Stop (turn 2) -> working.
	got, info = parentHeldStep(t, sh, hh, sent, events[8])
	assert.Equal(t, "working", info["activity"])
	assert.Equal(t, []string{parentHeldWorking}, got)

	got, info = parentHeldStep(t, sh, hh, sent, events[9])
	assert.Equal(t, "working", info["activity"])
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// AC4: OpenCode dialect.
// Turn 1: ask_user -> tool.execute.before (same turn, kept) ->
// tool.execute.after -> step-finish -> session.idle (kept + marker armed).
// Turn 2a: tool.execute.before clears to executing (local + Hub).
// Turn 2b (text-only): step-finish keeps waiting_for_input, then session.idle
// clears to working (local + Hub).
func TestAskUser_OpenCodeDialect(t *testing.T) {
	md := loadOpencodeDialect(t)
	parse := func(t *testing.T, payload map[string]interface{}) *hooks.Event {
		t.Helper()
		ev, err := md.Parse(payload)
		require.NoError(t, err)
		return ev
	}

	toolStart := func(t *testing.T) *hooks.Event {
		ev := parse(t, map[string]interface{}{
			"hook_event_name": "tool.execute.before",
			"tool_name":       "bash",
			"session_id":      "ses_test",
		})
		require.Equal(t, hooks.EventToolStart, ev.Name)
		return ev
	}
	toolEnd := func(t *testing.T) *hooks.Event {
		ev := parse(t, map[string]interface{}{
			"hook_event_name": "tool.execute.after",
			"tool_name":       "bash",
			"session_id":      "ses_test",
		})
		require.Equal(t, hooks.EventToolEnd, ev.Name)
		return ev
	}
	stepFinish := func(t *testing.T) *hooks.Event {
		ev := parse(t, map[string]interface{}{
			"hook_event_name": "message.part.updated.step-finish",
			"session_id":      "ses_test",
		})
		require.Equal(t, hooks.EventModelEnd, ev.Name)
		return ev
	}
	sessionIdle := func(t *testing.T) *hooks.Event {
		ev := parse(t, map[string]interface{}{
			"hook_event_name": "session.idle",
			"session_id":      "ses_test",
		})
		require.Equal(t, hooks.EventAgentEnd, ev.Name)
		return ev
	}

	runTurn1 := func(t *testing.T) (*StatusHandler, *HubHandler, func() []string) {
		t.Helper()
		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "ask_user")

		for _, ev := range []*hooks.Event{toolStart(t), toolEnd(t), stepFinish(t)} {
			got, info := parentHeldStep(t, sh, hh, sent, ev)
			assert.Equal(t, "waiting_for_input", info["activity"], ev.Name)
			assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
			assert.NotContains(t, info, askUserTurnEndedKey)
			assert.Empty(t, got)
		}

		got, info := parentHeldStep(t, sh, hh, sent, sessionIdle(t))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
		assert.Equal(t, true, info[askUserTurnEndedKey])
		assert.Empty(t, got)
		return sh, hh, sent
	}

	t.Run("turn 2 tool.execute.before clears", func(t *testing.T) {
		sh, hh, sent := runTurn1(t)

		got, info := parentHeldStep(t, sh, hh, sent, toolStart(t))
		assert.Equal(t, "executing", info["activity"])
		assert.NotContains(t, info, activitySourceKey)
		assert.NotContains(t, info, askUserTurnEndedKey)
		assert.Equal(t, []string{parentHeldExecuting}, got)
	})

	t.Run("turn 2 text-only keeps on step-finish and clears on session.idle", func(t *testing.T) {
		sh, hh, sent := runTurn1(t)

		// EventModelEnd is intentionally excluded from clearing ask-user.
		got, info := parentHeldStep(t, sh, hh, sent, stepFinish(t))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
		assert.Equal(t, true, info[askUserTurnEndedKey])
		assert.Empty(t, got)

		// Turn 2 session.idle clears to working as backstop.
		got, info = parentHeldStep(t, sh, hh, sent, sessionIdle(t))
		assert.Equal(t, "working", info["activity"])
		assert.NotContains(t, info, activitySourceKey)
		assert.NotContains(t, info, askUserTurnEndedKey)
		assert.Equal(t, []string{parentHeldWorking}, got)
	})
}

// AC6: Sub-agent question during an ask-user wait (both before and after
// parent Stop), then SubagentStop: the ask-user marker and askUserTurnEnded
// state come back intact.
func TestAskUser_SubagentQuestionPreservesMarkerAndTurnState(t *testing.T) {
	t.Run("subagent stops before parent Stop", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "ask_user")

		got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, activitySourceNotification, info[activitySourceKey])
		assert.Equal(t, parentWaitAskUser, info[parentWaitKey])
		assert.NotContains(t, info, askUserTurnEndedKey)
		assert.Empty(t, got)

		// Sub-agent Stop never sets askUserTurnEnded.
		got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Stop", "", parentHeldSubA))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
		assert.NotContains(t, info, askUserTurnEndedKey)
		assert.NotContains(t, info, parentWaitKey)
		assert.NotContains(t, info, notificationWaitAgentsKey)
		assert.Empty(t, got)

		// Still same turn: parent PreToolUse keeps the restored ask-user wait.
		got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
		assert.Empty(t, got)
	})

	t.Run("subagent asks after parent Stop, then SubagentStop restores turn-ended state", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "ask_user")

		// Parent turn ends -> askUserTurnEnded: true.
		got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Stop", ""))
		require.Equal(t, true, info[askUserTurnEndedKey])
		require.Empty(t, got)

		// Sub-agent asks while parent wait is turn-ended.
		got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, parentWaitAskUser, info[parentWaitKey])
		assert.Equal(t, true, info[askUserTurnEndedKey])
		assert.Empty(t, got)

		// SubagentStop restores ask-user wait with askUserTurnEnded: true intact.
		got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "SubagentStop", "", parentHeldSubA))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
		assert.Equal(t, true, info[askUserTurnEndedKey])
		assert.NotContains(t, info, parentWaitKey)
		assert.Empty(t, got)

		// Because askUserTurnEnded came back true, next parent tool-start clears it.
		got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
		assert.Equal(t, "executing", info["activity"])
		assert.NotContains(t, info, activitySourceKey)
		assert.NotContains(t, info, askUserTurnEndedKey)
		assert.Equal(t, []string{parentHeldExecuting}, got)
	})

	t.Run("in-place SessionStart restores parentWaitAskUser", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "ask_user")

		_, _ = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))
		got, info := parentHeldStep(t, sh, hh, sent, parentHeldSessionStart(t, "compact", ""))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.Equal(t, ActivitySourceAskUser, info[activitySourceKey])
		assert.NotContains(t, info, notificationWaitAgentsKey)
		assert.NotContains(t, info, parentWaitKey)
		assert.Empty(t, got)
	})
}

// Verify every write that removes activitySource also removes askUserTurnEnded.
func TestAskUser_WritesDropTurnEndedMarker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*testing.T, *StatusHandler)
	}{
		{"UpdateActivity", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.UpdateActivity(state.ActivityBlocked, ""))
		}},
		{"UpdateActivityWithSource(ask-user)", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.UpdateActivityWithSource(state.ActivityWaitingForInput, "", ActivitySourceAskUser))
		}},
		{"UpdatePhase", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.UpdatePhase(state.PhaseRunning, state.ActivityWorking, ""))
		}},
		{"Claude ExitPlanMode", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.Handle(&hooks.Event{
				Name:    hooks.EventToolStart,
				Dialect: "claude",
				Data:    hooks.EventData{ToolName: "ExitPlanMode"},
			}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			require.NoError(t, h.UpdateActivityWithSource(state.ActivityWaitingForInput, "", ActivitySourceAskUser))
			require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}))
			require.Equal(t, true, readAgentInfoMap(t, statusPath)[askUserTurnEndedKey])

			tc.write(t, h)
			assert.NotContains(t, readAgentInfoMap(t, statusPath), askUserTurnEndedKey)
		})
	}
}
