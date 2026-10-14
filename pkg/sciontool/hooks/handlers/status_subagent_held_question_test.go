/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover a sub-agent's question (a claude AskUserQuestion or
// ExitPlanMode tool-start, or a claude or codex Notification) meeting a
// status the agent holds. blocked, completed, limits_exceeded and crashed
// stay as they are: agent-info.json is not written, no owner is recorded and
// no Hub update is sent. Every other status still moves to a
// notification-set waiting_for_input owned by the sub-agent, and a wait the
// agent set itself is kept. All payloads are synthetic.

var heldQuestionHeldStates = []state.Activity{
	state.ActivityBlocked,
	state.ActivityCompleted,
	state.ActivityLimitsExceeded,
	state.ActivityCrashed,
}

type heldQuestionEvent struct {
	name  string
	event func(t *testing.T) *hooks.Event
}

// heldQuestionSubagentEvents are the sub-agent questions, through the real
// claude and codex dialects.
func heldQuestionSubagentEvents() []heldQuestionEvent {
	return []heldQuestionEvent{
		{"claude AskUserQuestion", func(t *testing.T) *hooks.Event {
			return claudeEvent(t, "PreToolUse", "AskUserQuestion", claudeSubagentChildID)
		}},
		{"claude ExitPlanMode", func(t *testing.T) *hooks.Event {
			return claudeEvent(t, "PreToolUse", "ExitPlanMode", claudeSubagentChildID)
		}},
		{"claude Notification", func(t *testing.T) *hooks.Event {
			return parentHeldSub(t, "Notification", "", claudeSubagentChildID)
		}},
		{"codex notification", func(t *testing.T) *hooks.Event {
			return codexEvent(t, "notification", codexSubagentChildID)
		}},
	}
}

// requireSubagentQuestion checks that event is a sub-agent's question.
func requireSubagentQuestion(t *testing.T, event *hooks.Event) {
	t.Helper()
	require.True(t, isSubagentEvent(event), "a sub-agent event")
	require.True(t, isClaudeUserWaitTool(event) || event.Name == hooks.EventNotification, "a question event")
}

// A sub-agent's question leaves blocked, completed, limits_exceeded and
// crashed unchanged: agent-info.json is byte for byte the same, it has no
// notification marker or owner list, and the Hub gets no update.
func TestSubagentHeldQuestion_HeldStatusKept(t *testing.T) {
	for _, ev := range heldQuestionSubagentEvents() {
		for _, held := range heldQuestionHeldStates {
			t.Run(ev.name+"/"+string(held), func(t *testing.T) {
				sh, hh, sent := claudeHubSetup(t)
				require.NoError(t, sh.UpdateActivity(held, ""))
				before, err := os.ReadFile(sh.StatusPath)
				require.NoError(t, err)

				event := ev.event(t)
				requireSubagentQuestion(t, event)
				claudeHubHandle(t, sh, hh, event)

				after, err := os.ReadFile(sh.StatusPath)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "agent-info.json must not be written")
				info := readAgentInfoMap(t, sh.StatusPath)
				assert.Equal(t, string(held), info["activity"], "local status")
				assert.NotContains(t, info, activitySourceKey, "no notification marker")
				assert.NotContains(t, info, notificationWaitAgentsKey, "no owner entry")
				assert.Empty(t, sent(), "no hub update while local stays %s", held)
			})
		}
	}
}

// After a held status absorbed a sub-agent's question, the rest of that
// question (its tool-end when answered, then the sub-agent's end) changes
// nothing either.
func TestSubagentHeldQuestion_AnswerAfterHeldKept(t *testing.T) {
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		for _, held := range heldQuestionHeldStates {
			t.Run(tool+"/"+string(held), func(t *testing.T) {
				sh, hh, sent := claudeHubSetup(t)
				require.NoError(t, sh.UpdateActivity(held, ""))

				for _, event := range []*hooks.Event{
					claudeEvent(t, "PreToolUse", tool, claudeSubagentChildID),
					claudeEvent(t, "PostToolUse", tool, claudeSubagentChildID),
					claudeEvent(t, "SubagentStop", "", claudeSubagentChildID),
				} {
					claudeHubHandle(t, sh, hh, event)
					info := readAgentInfoMap(t, sh.StatusPath)
					assert.Equal(t, string(held), info["activity"], "after %s", event.Name)
					assert.NotContains(t, info, activitySourceKey, "after %s", event.Name)
					assert.NotContains(t, info, notificationWaitAgentsKey, "after %s", event.Name)
				}
				assert.Empty(t, sent(), "no hub update while local stays %s", held)
			})
		}
	}
}

// Every status that is not held still moves to waiting_for_input on a
// sub-agent's question: no file yet, initializing, working, thinking,
// executing, and a notification-set wait that a sibling sub-agent owns. The
// wait is notification-set, owned by the sub-agent, and reported to the Hub.
// A wait the agent set itself is kept with no Hub update.
func TestSubagentHeldQuestion_LegitimateTransitions(t *testing.T) {
	for _, ev := range heldQuestionSubagentEvents() {
		for _, tc := range []struct {
			name       string
			setup      func(t *testing.T, sh *StatusHandler)
			wantOwners func(event *hooks.Event) []string
		}{
			{"no file", func(t *testing.T, sh *StatusHandler) {}, nil},
			{"initializing", func(t *testing.T, sh *StatusHandler) {
				require.NoError(t, sh.UpdateActivity(state.ActivityInitializing, ""))
			}, nil},
			{"working", func(t *testing.T, sh *StatusHandler) {
				require.NoError(t, sh.UpdateActivity(state.ActivityWorking, ""))
			}, nil},
			{"thinking", func(t *testing.T, sh *StatusHandler) {
				require.NoError(t, sh.UpdateActivity(state.ActivityThinking, ""))
			}, nil},
			{"executing", func(t *testing.T, sh *StatusHandler) {
				require.NoError(t, sh.UpdateActivity(state.ActivityExecuting, "Bash"))
			}, nil},
			{"sibling notification wait", func(t *testing.T, sh *StatusHandler) {
				require.NoError(t, sh.setWaitingFromNotificationFor(parentHeldSubB))
			}, func(event *hooks.Event) []string { return []string{parentHeldSubB, subagentID(event)} }},
		} {
			t.Run(ev.name+"/"+tc.name, func(t *testing.T) {
				sh, hh, sent := claudeHubSetup(t)
				tc.setup(t, sh)

				event := ev.event(t)
				requireSubagentQuestion(t, event)
				claudeHubHandle(t, sh, hh, event)

				info := readAgentInfoMap(t, sh.StatusPath)
				assert.Equal(t, "waiting_for_input", info["activity"])
				assert.Equal(t, activitySourceNotification, info[activitySourceKey])
				wantOwners := []string{subagentID(event)}
				if tc.wantOwners != nil {
					wantOwners = tc.wantOwners(event)
				}
				assert.Equal(t, wantOwners, waitOwnersOf(info))
				assert.Equal(t, []string{parentHeldWaiting}, sent())
			})
		}

		t.Run(ev.name+"/agent-set wait", func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			require.NoError(t, sh.UpdateActivity(state.ActivityWaitingForInput, ""))

			event := ev.event(t)
			requireSubagentQuestion(t, event)
			claudeHubHandle(t, sh, hh, event)

			info := readAgentInfoMap(t, sh.StatusPath)
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey)
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.Empty(t, sent(), "no hub update replaces the agent-set wait")
		})
	}
}

// The parent's own question is not changed by this rule: over completed, the
// parent's AskUserQuestion and Notification still set waiting_for_input and
// report it.
func TestSubagentHeldQuestion_ParentQuestionUnchanged(t *testing.T) {
	for _, ev := range []heldQuestionEvent{
		{"claude AskUserQuestion", func(t *testing.T) *hooks.Event {
			return claudeEvent(t, "PreToolUse", "AskUserQuestion", "")
		}},
		{"claude Notification", func(t *testing.T) *hooks.Event {
			return parentHeldParent(t, "Notification", "")
		}},
	} {
		t.Run(ev.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			require.NoError(t, sh.UpdateActivity(state.ActivityCompleted, ""))

			event := ev.event(t)
			require.False(t, isSubagentEvent(event))
			claudeHubHandle(t, sh, hh, event)

			assert.Equal(t, "waiting_for_input", readAgentInfoMap(t, sh.StatusPath)["activity"])
			assert.Equal(t, []string{parentHeldWaiting}, sent())
		})
	}
}
