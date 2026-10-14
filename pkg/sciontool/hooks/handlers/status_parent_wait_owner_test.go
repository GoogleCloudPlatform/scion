/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the parent's own wait inside a wait that sub-agents hold
// (parentWaitKey) and the parent's Stop that reports no background work in
// flight (background_tasks). Local status and Hub report are checked
// together. Payloads are synthetic and go through the real claude dialect,
// except the captured Stop record replayed at the end.

// claudeWithFields parses a synthetic claude payload like parentHeldClaude
// (agentID "" means the parent) with extra fields added.
func claudeWithFields(t *testing.T, hookEventName, toolName, agentID string, extra map[string]interface{}) *hooks.Event {
	t.Helper()
	payload := map[string]interface{}{
		"hook_event_name": hookEventName,
		"session_id":      claudeSubagentParentSession,
		"cwd":             "/workspace",
	}
	if toolName != "" {
		payload["tool_name"] = toolName
		payload["tool_input"] = map[string]interface{}{}
	}
	if agentID != "" {
		payload["agent_id"] = agentID
		payload["agent_type"] = "general-purpose"
	}
	for k, v := range extra {
		payload[k] = v
	}
	event, err := dialects.NewClaudeDialect().Parse(payload)
	require.NoError(t, err)
	return event
}

// parentStopWithTasks is the parent's Stop carrying background_tasks.
func parentStopWithTasks(t *testing.T, tasks interface{}) *hooks.Event {
	t.Helper()
	e := claudeWithFields(t, "Stop", "", "", map[string]interface{}{
		"stop_hook_active": false,
		"background_tasks": tasks,
		"session_crons":    []interface{}{},
	})
	require.Equal(t, hooks.EventAgentEnd, e.Name)
	return e
}

// ownerHeldWithParentQuestion lets sub-agent A ask, then the parent ask with
// tool (AskUserQuestion or ExitPlanMode).
func ownerHeldWithParentQuestion(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string, tool string) ([]string, map[string]interface{}) {
	t.Helper()
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))
	return parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", tool))
}

// ownerHeldWithParentNotification lets each of ids send a Notification, then
// the parent send its own.
func ownerHeldWithParentNotification(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string, ids ...string) {
	t.Helper()
	parentHeldHold(t, sh, "working")
	for _, id := range ids {
		parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", id))
	}
	_, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Notification", ""))
	require.Equal(t, ids, waitOwnersOf(info))
	require.Equal(t, parentWaitNotification, info[parentWaitKey])
}

// Edge 1: the parent's own AskUserQuestion or ExitPlanMode during a wait that
// a sub-agent holds keeps the marker and the owner list and records the
// parent's question. The Hub reports the parent's wait.
func TestParentWaitOwners_ParentQuestionKeepsOwners(t *testing.T) {
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		t.Run(tool, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			got, info := ownerHeldWithParentQuestion(t, sh, hh, sent, tool)
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.Equal(t, activitySourceNotification, info[activitySourceKey])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.Equal(t, parentWaitAgent, info[parentWaitKey])
			assert.Equal(t, []string{parentHeldWaiting}, got)
		})
	}
}

// Edge 1: the last owner's SubagentStop or its own answer then leaves the
// parent's question as an agent-set wait, with no Hub update. The parent's
// next tool-start clears it as it would any agent-set wait.
func TestParentWaitOwners_LastOwnerLeavesParentQuestion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(*testing.T) *hooks.Event
	}{
		{"SubagentStop", func(t *testing.T) *hooks.Event { return bgSubagentStop(t, parentHeldSubA) }},
		{"own answer", func(t *testing.T) *hooks.Event {
			return parentHeldSub(t, "PostToolUse", "AskUserQuestion", parentHeldSubA)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			ownerHeldWithParentQuestion(t, sh, hh, sent, "AskUserQuestion")

			got, info := parentHeldStep(t, sh, hh, sent, tc.event(t))
			assert.Equal(t, "waiting_for_input", info["activity"], "the parent's question is still open")
			assert.NotContains(t, info, activitySourceKey, "an agent-set wait")
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.NotContains(t, info, parentWaitKey)
			assert.Empty(t, got)

			got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
			assert.Equal(t, "executing", info["activity"])
			assert.Equal(t, []string{parentHeldExecuting}, got)
		})
	}
}

// Edge 1: when the parent's question is answered first, its tool-end keeps
// the question recorded (as for an agent-set wait), and its next tool-start
// clears it while the owner's wait stays. The owner's SubagentStop then
// releases the wait to working and reports it once.
func TestParentWaitOwners_ParentAnswersFirst(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	ownerHeldWithParentQuestion(t, sh, hh, sent, "AskUserQuestion")

	got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PostToolUse", "AskUserQuestion"))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, parentWaitAgent, info[parentWaitKey], "tool-end keeps the parent's question")
	assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
	assert.Equal(t, "waiting_for_input", info["activity"], "A still holds the wait")
	assert.NotContains(t, info, parentWaitKey, "tool-start clears the parent's question")
	assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))
	assert.Equal(t, "working", info["activity"])
	assert.Equal(t, activitySourceSubagentEnd, info[activitySourceKey])
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// Edge 1: the parent's Stop keeps its recorded question, a later parent
// Notification does not turn it into a notification wait, and neither that
// Notification nor another sub-agent's question (AskUserQuestion or
// Notification) replaces the parent's message on the Hub. Each other
// sub-agent is still recorded as an owner.
func TestParentWaitOwners_ParentQuestionNotDowngraded(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	ownerHeldWithParentQuestion(t, sh, hh, sent, "ExitPlanMode")

	got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Stop", ""))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, parentWaitAgent, info[parentWaitKey])
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Notification", ""))
	assert.Equal(t, parentWaitAgent, info[parentWaitKey], "not downgraded")
	assert.Empty(t, got, "the plan approval message is kept")

	// A sub-agent's question (the HubHandler's tool-start path).
	got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubB))
	assert.Equal(t, []string{parentHeldSubA, parentHeldSubB}, waitOwnersOf(info), "B is recorded")
	assert.Equal(t, parentWaitAgent, info[parentWaitKey])
	assert.Empty(t, got, "B's question does not replace the plan approval message")

	// A sub-agent's Notification (the HubHandler's Notification path).
	got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubC))
	assert.Equal(t, []string{parentHeldSubA, parentHeldSubB, parentHeldSubC}, waitOwnersOf(info), "C is recorded")
	assert.Equal(t, parentWaitAgent, info[parentWaitKey])
	assert.Empty(t, got, "C's Notification does not replace the plan approval message")
}

// Edge 2: the last owner's own answer leaves the parent's Notification wait
// in place, with no Hub update.
func TestParentWaitOwners_ParentNotificationOutlivesOwnAnswer(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	ownerHeldWithParentNotification(t, sh, hh, sent, parentHeldSubA)

	got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PostToolUse", "Bash", parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, activitySourceNotification, info[activitySourceKey])
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.NotContains(t, info, parentWaitKey)
	assert.Empty(t, got)
}

// Edge 2: with two owners, the first SubagentStop removes only its entry and
// keeps the parent's wait recorded; the second leaves the parent's
// Notification wait alone.
func TestParentWaitOwners_ParentNotificationTwoOwners(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	ownerHeldWithParentNotification(t, sh, hh, sent, parentHeldSubA, parentHeldSubB)

	got, info := parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, []string{parentHeldSubB}, waitOwnersOf(info))
	assert.Equal(t, parentWaitNotification, info[parentWaitKey])
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubB))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, activitySourceNotification, info[activitySourceKey])
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.NotContains(t, info, parentWaitKey)
	assert.Empty(t, got)
}

// Edge 2: the parent's tool-end (its prompt was answered) or its Stop clears
// the parent's recorded Notification and keeps the owner's wait. The owner's
// SubagentStop then releases the wait to working, as with no parent wait.
func TestParentWaitOwners_ParentNotificationAnswered(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(*testing.T) *hooks.Event
	}{
		{"parent tool-end", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PostToolUse", "Bash") }},
		{"parent Stop", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "Stop", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			ownerHeldWithParentNotification(t, sh, hh, sent, parentHeldSubA)

			got, info := parentHeldStep(t, sh, hh, sent, tc.event(t))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.NotContains(t, info, parentWaitKey)
			assert.Empty(t, got)

			got, info = parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))
			assert.Equal(t, "working", info["activity"])
			assert.Equal(t, activitySourceSubagentEnd, info[activitySourceKey])
			assert.Equal(t, []string{parentHeldWorking}, got)
		})
	}
}

// Edge 2: once the last owner has left the parent's Notification wait, the
// parent's tool-end clears it to working, as it clears any owner-less
// notification wait.
func TestParentWaitOwners_RestoredNotificationClearsAsBefore(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	ownerHeldWithParentNotification(t, sh, hh, sent, parentHeldSubA)
	parentHeldStep(t, sh, hh, sent, bgSubagentStop(t, parentHeldSubA))

	got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PostToolUse", "Bash"))
	assert.Equal(t, "working", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// Edge 3: the parent's Stop with background_tasks present and empty releases
// a stale owner: working on both sides, no marker and no owner list.
func TestBackgroundTasksReported_EmptyListReleasesStaleOwner(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))

	got, info := parentHeldStep(t, sh, hh, sent, parentStopWithTasks(t, []interface{}{}))
	assert.Equal(t, "working", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// Edge 3: a non-empty list of any task type, or a value that is not an
// array, keeps the owner-held wait as before.
func TestBackgroundTasksReported_NonEmptyListKeepsWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks interface{}
	}{
		{"subagent task", []interface{}{map[string]interface{}{
			"id": "t0000001", "type": "subagent", "status": "running", "description": "synthetic", "agent_type": "general-purpose",
		}}},
		{"shell task", []interface{}{map[string]interface{}{
			"id": "t0000002", "type": "shell", "status": "running", "description": "synthetic", "command": "sleep 1",
		}}},
		{"not an array", map[string]interface{}{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "working")
			parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))

			got, info := parentHeldStep(t, sh, hh, sent, parentStopWithTasks(t, tc.tasks))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.Empty(t, got)
		})
	}
}

// Edge 3: a Stop without the field keeps the owner-held wait as before.
func TestBackgroundTasksReported_AbsentFieldKeepsWait(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))

	stop := claudeWithFields(t, "Stop", "", "", map[string]interface{}{"stop_hook_active": false})
	require.NotContains(t, stop.Data.Raw, "background_tasks")
	got, info := parentHeldStep(t, sh, hh, sent, stop)
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
	assert.Empty(t, got)
}

// Edge 3: the empty-list Stop drops the owners but restores the parent's
// recorded wait first: the parent's question stays as an agent-set wait
// (no Hub update); the parent's Notification is then cleared by the Stop,
// as any owner-less notification wait is.
func TestBackgroundTasksReported_EmptyListRestoresParentWait(t *testing.T) {
	t.Run("parent question", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		ownerHeldWithParentQuestion(t, sh, hh, sent, "AskUserQuestion")

		got, info := parentHeldStep(t, sh, hh, sent, parentStopWithTasks(t, []interface{}{}))
		assert.Equal(t, "waiting_for_input", info["activity"])
		assert.NotContains(t, info, activitySourceKey)
		assert.NotContains(t, info, notificationWaitAgentsKey)
		assert.NotContains(t, info, parentWaitKey)
		assert.Empty(t, got)
	})
	t.Run("parent notification", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		ownerHeldWithParentNotification(t, sh, hh, sent, parentHeldSubA)

		got, info := parentHeldStep(t, sh, hh, sent, parentStopWithTasks(t, []interface{}{}))
		assert.Equal(t, "working", info["activity"])
		assert.NotContains(t, info, activitySourceKey)
		assert.NotContains(t, info, notificationWaitAgentsKey)
		assert.NotContains(t, info, parentWaitKey)
		assert.Equal(t, []string{parentHeldWorking}, got)
	})
}

// Edge 3: the captured Stop record (background_tasks is an empty array)
// replayed over an owner-held wait releases it.
func TestBackgroundTasksReported_CapturedStopRecord(t *testing.T) {
	stop := loadClaudeSubagentFixture(t)[6]
	require.Equal(t, "Stop", stop.RawName)
	tasks, ok := stop.Data.Raw["background_tasks"].([]interface{})
	require.True(t, ok)
	require.Empty(t, tasks)

	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))

	got, info := parentHeldStep(t, sh, hh, sent, stop)
	assert.Equal(t, "working", info["activity"])
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// Edge 3: only the parent's Stop uses the list. A parent tool-end, a
// sub-agent's SubagentStop and a Stop that carries a sub-agent's agent_id,
// each with an empty list, keep the owner-held wait.
func TestBackgroundTasksReported_OnlyParentStop(t *testing.T) {
	empty := map[string]interface{}{"background_tasks": []interface{}{}}
	for _, tc := range []struct {
		name  string
		event func(*testing.T) *hooks.Event
	}{
		{"parent tool-end", func(t *testing.T) *hooks.Event {
			return claudeWithFields(t, "PostToolUse", "Bash", "", empty)
		}},
		{"sibling SubagentStop", func(t *testing.T) *hooks.Event {
			return claudeWithFields(t, "SubagentStop", "", parentHeldSubC, empty)
		}},
		{"sub-agent Stop", func(t *testing.T) *hooks.Event {
			return claudeWithFields(t, "Stop", "", parentHeldSubC, empty)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "working")
			parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))

			got, info := parentHeldStep(t, sh, hh, sent, tc.event(t))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.Empty(t, got)
		})
	}
}
