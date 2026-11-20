/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
)

// These tests cover a parent wait that comes BEFORE a sub-agent's question:
// the parent asks (AskUserQuestion, ExitPlanMode, or sciontool status
// ask_user), then a sub-agent asks. The sub-agent is recorded as an owner of
// the wait and the parent's own wait as parentWaitAgent, so the parent's
// next tool-start clears only the parent's part: the sub-agent's question
// stays open until its own event. This is the reverse order of the case in
// status_parent_wait_owner_test.go. All payloads are synthetic and go
// through the real claude dialect.

// questionFirstHolds are the ways the parent can hold its own wait before the
// sub-agent asks.
func questionFirstHolds() []struct {
	name string
	hold func(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string)
} {
	parentAsks := func(tool string) func(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string) {
		return func(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string) {
			got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", tool))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey, "the parent's wait is agent-set")
			assert.Equal(t, []string{parentHeldWaiting}, got)
		}
	}
	return []struct {
		name string
		hold func(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string)
	}{
		{"AskUserQuestion", parentAsks("AskUserQuestion")},
		{"ExitPlanMode", parentAsks("ExitPlanMode")},
		{"ask_user", func(t *testing.T, sh *StatusHandler, _ *HubHandler, _ func() []string) {
			parentHeldHold(t, sh, "waiting_for_input")
		}},
	}
}

// questionFirstSubagentAsks sends a sub-agent's question during the parent's
// wait and checks that it is recorded next to the parent's wait, with no Hub
// update that would replace the parent's message.
func questionFirstSubagentAsks(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string) {
	t.Helper()
	got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, activitySourceNotification, info[activitySourceKey], "the sub-agent owns a wait")
	assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
	assert.Equal(t, parentWaitAgent, info[parentWaitKey], "the parent's wait is recorded")
	assert.Empty(t, got, "no hub update replaces the parent's wait")
}

// The parent's next tool-start (its question was answered) clears the
// parent's part only: the sub-agent's question keeps waiting_for_input, and
// the Hub gets no executing update. The sub-agent's own end then clears the
// wait to working.
func TestParentQuestionFirst_SubagentWaitSurvivesParentToolStart(t *testing.T) {
	for _, h := range questionFirstHolds() {
		for _, end := range []struct {
			name  string
			event func(*testing.T) *hooks.Event
		}{
			{"answered", func(t *testing.T) *hooks.Event {
				return parentHeldSub(t, "PostToolUse", "AskUserQuestion", parentHeldSubA)
			}},
			{"sub-agent stops", func(t *testing.T) *hooks.Event {
				return parentHeldSub(t, "SubagentStop", "", parentHeldSubA)
			}},
		} {
			t.Run(h.name+"/"+end.name, func(t *testing.T) {
				sh, hh, sent := claudeHubSetup(t)
				h.hold(t, sh, hh, sent)
				questionFirstSubagentAsks(t, sh, hh, sent)

				got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
				assert.Equal(t, "waiting_for_input", info["activity"], "the sub-agent's question is still open")
				assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
				assert.NotContains(t, info, parentWaitKey, "the parent's wait is cleared")
				assert.Empty(t, got, "no executing update")

				got, info = parentHeldStep(t, sh, hh, sent, end.event(t))
				assert.Equal(t, "working", info["activity"])
				assert.NotContains(t, info, notificationWaitAgentsKey)
				assert.NotContains(t, info, parentWaitKey)
				assert.Equal(t, []string{parentHeldWorking}, got)
			})
		}
	}
}

// When the sub-agent's question ends first, the parent's own wait is left as
// it was before (agent-set, no marker), and the parent's tool-start then
// clears it as usual.
func TestParentQuestionFirst_SubagentEndLeavesParentWait(t *testing.T) {
	for _, h := range questionFirstHolds() {
		t.Run(h.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			h.hold(t, sh, hh, sent)
			questionFirstSubagentAsks(t, sh, hh, sent)

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "SubagentStop", "", parentHeldSubA))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey, "the parent's agent-set wait is back")
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.NotContains(t, info, parentWaitKey)
			assert.Empty(t, got)

			got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PreToolUse", "Bash"))
			assert.Equal(t, "executing", info["activity"])
			assert.Equal(t, []string{parentHeldExecuting}, got)
		})
	}
}

// A parent Notification during the combined wait keeps the parent's part as
// parentWaitAgent, and the parent's tool-end keeps it too: only the parent's
// tool-start answers the parent's own question.
func TestParentQuestionFirst_ParentNotificationAndToolEndKeepParentWait(t *testing.T) {
	for _, h := range questionFirstHolds() {
		t.Run(h.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			h.hold(t, sh, hh, sent)
			questionFirstSubagentAsks(t, sh, hh, sent)

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Notification", ""))
			assert.Equal(t, parentWaitAgent, info[parentWaitKey])
			assert.Empty(t, got, "no hub update replaces the parent's wait")

			got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PostToolUse", "Agent"))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.Equal(t, parentWaitAgent, info[parentWaitKey])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.Empty(t, got)
		})
	}
}
