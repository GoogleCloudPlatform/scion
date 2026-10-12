/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover how a status the parent agent holds meets later hook
// events, with the local status and the Hub report checked together:
//   - a parent tool-start keeps blocked, crashed, completed and
//     limits_exceeded on both sides and replaces only waiting_for_input;
//   - a harness prompt (a sub-agent's question, or any Notification) leaves a
//     waiting_for_input the agent set itself alone, and sends no Hub update
//     that would replace its message;
//   - with several sub-agents asking at once, each clears only its own
//     question.
// Payloads are synthetic, except the captured fixture replayed at the end,
// and all go through the real claude (or codex) dialect.

const (
	parentHeldSubA = "a0000000000000aa1"
	parentHeldSubB = "a0000000000000bb2"
	parentHeldSubC = "a0000000000000cc3"
)

var (
	parentHeldWaiting   = state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}.DisplayStatus()
	parentHeldWorking   = state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWorking}.DisplayStatus()
	parentHeldExecuting = state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityExecuting}.DisplayStatus()
	parentHeldThinking  = state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityThinking}.DisplayStatus()
)

// parentHeldClaude parses a synthetic claude payload. agentID is set as the
// agent_id field when it is not nil, so a test can pass a non-string value.
func parentHeldClaude(t *testing.T, hookEventName, toolName string, agentID interface{}) *hooks.Event {
	t.Helper()
	payload := map[string]interface{}{
		"hook_event_name": hookEventName,
		"session_id":      claudeSubagentParentSession,
		"cwd":             "/workspace",
	}
	switch hookEventName {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure":
		payload["tool_name"] = toolName
		payload["tool_input"] = map[string]interface{}{}
	case "Notification":
		payload["message"] = "Claude needs your permission to use Bash"
		payload["notification_type"] = "permission_prompt"
	case "UserPromptSubmit":
		payload["prompt"] = "synthetic prompt"
	}
	if hookEventName == "PostToolUseFailure" {
		payload["error"] = "The user doesn't want to proceed with this tool use."
	}
	if agentID != nil {
		payload["agent_id"] = agentID
		payload["agent_type"] = "general-purpose"
	}
	event, err := dialects.NewClaudeDialect().Parse(payload)
	require.NoError(t, err)
	return event
}

func parentHeldSub(t *testing.T, hookEventName, toolName, agentID string) *hooks.Event {
	return parentHeldClaude(t, hookEventName, toolName, agentID)
}

func parentHeldParent(t *testing.T, hookEventName, toolName string) *hooks.Event {
	return parentHeldClaude(t, hookEventName, toolName, nil)
}

// parentHeldStep runs one event through both handlers and returns the Hub
// statuses this event sent and the local agent-info.json afterwards.
func parentHeldStep(t *testing.T, sh *StatusHandler, hh *HubHandler, sent func() []string, event *hooks.Event) ([]string, map[string]interface{}) {
	t.Helper()
	before := len(sent())
	claudeHubHandle(t, sh, hh, event)
	return sent()[before:], readAgentInfoMap(t, sh.StatusPath)
}

// parentHeldHold puts the parent in a held state: an agent-set wait (the
// ask_user write), a notification-set wait, or any other activity.
func parentHeldHold(t *testing.T, sh *StatusHandler, held string) {
	t.Helper()
	if held == "notification" {
		require.NoError(t, sh.setWaitingFromNotification())
		return
	}
	require.NoError(t, sh.UpdateActivity(state.Activity(held), ""))
}

func waitOwnersOf(info map[string]interface{}) []string {
	return notificationWaitOwners(info)
}

// Part A: a parent tool-start makes the Hub report what the local status
// decided. blocked, crashed, completed and limits_exceeded stay held on both
// sides with no Hub update; a waiting_for_input of either kind becomes
// executing on both sides.
func TestParentHeld_ToolStartHubMatchesLocal(t *testing.T) {
	codexParent := func(t *testing.T) *hooks.Event { return codexParentEvent(t, "PreToolUse") }
	claudeParent := func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PreToolUse", "Bash") }
	for _, dialect := range []struct {
		name  string
		event func(*testing.T) *hooks.Event
	}{{"claude", claudeParent}, {"codex", codexParent}} {
		for _, tc := range []struct {
			held       string
			wantLocal  string
			wantStatus string // "" means no Hub call
		}{
			{"blocked", "blocked", ""},
			{"crashed", "crashed", ""},
			{"completed", "completed", ""},
			{"limits_exceeded", "limits_exceeded", ""},
			{"waiting_for_input", "executing", parentHeldExecuting},
			{"notification", "executing", parentHeldExecuting},
			{"working", "executing", parentHeldExecuting},
		} {
			t.Run(dialect.name+"/"+tc.held, func(t *testing.T) {
				sh, hh, sent := claudeHubSetup(t)
				parentHeldHold(t, sh, tc.held)

				event := dialect.event(t)
				require.Equal(t, hooks.EventToolStart, event.Name)
				require.False(t, isSubagentEvent(event))
				got, info := parentHeldStep(t, sh, hh, sent, event)

				assert.Equal(t, tc.wantLocal, info["activity"], "local status")
				if tc.wantStatus == "" {
					assert.Empty(t, got, "the hub must not report executing while local stays %s", tc.held)
				} else {
					assert.Equal(t, []string{tc.wantStatus}, got)
				}
			})
		}
	}
}

// Part B1: while the parent holds a wait it set itself, a sub-agent asks the
// user (AskUserQuestion, ExitPlanMode or a permission Notification) and the
// user answers or rejects. The agent-set wait is still there afterwards, and
// no Hub update replaced it at any step.
func TestParentHeld_AgentSetWaitKeptThroughSubagentQuestion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ask    func(*testing.T) *hooks.Event
		answer func(*testing.T) *hooks.Event
	}{
		{"AskUserQuestion answered",
			func(t *testing.T) *hooks.Event {
				return parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA)
			},
			func(t *testing.T) *hooks.Event {
				return parentHeldSub(t, "PostToolUse", "AskUserQuestion", parentHeldSubA)
			}},
		{"ExitPlanMode answered",
			func(t *testing.T) *hooks.Event { return parentHeldSub(t, "PreToolUse", "ExitPlanMode", parentHeldSubA) },
			func(t *testing.T) *hooks.Event {
				return parentHeldSub(t, "PostToolUse", "ExitPlanMode", parentHeldSubA)
			}},
		{"ExitPlanMode rejected",
			func(t *testing.T) *hooks.Event { return parentHeldSub(t, "PreToolUse", "ExitPlanMode", parentHeldSubA) },
			func(t *testing.T) *hooks.Event {
				return parentHeldSub(t, "PostToolUseFailure", "ExitPlanMode", parentHeldSubA)
			}},
		{"permission Notification answered",
			func(t *testing.T) *hooks.Event { return parentHeldSub(t, "Notification", "", parentHeldSubA) },
			func(t *testing.T) *hooks.Event { return parentHeldSub(t, "PostToolUse", "Bash", parentHeldSubA) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "waiting_for_input")

			got, info := parentHeldStep(t, sh, hh, sent, tc.ask(t))
			assert.Equal(t, "waiting_for_input", info["activity"], "after the sub-agent asks")
			assert.NotContains(t, info, activitySourceKey, "the wait stays agent-set")
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.Empty(t, got, "no hub update replaces the agent-set wait")

			got, info = parentHeldStep(t, sh, hh, sent, tc.answer(t))
			assert.Equal(t, "waiting_for_input", info["activity"], "after the user answers")
			assert.NotContains(t, info, activitySourceKey)
			assert.Empty(t, got)
		})
	}
}

// B1 also covers a parent Notification (for example the idle notification
// after ask_user): the agent-set wait is kept with no marker and no Hub
// update, and the parent's next tool-start or prompt still clears it.
func TestParentHeld_ParentNotificationKeepsAgentSetWait(t *testing.T) {
	for _, tc := range []struct {
		name       string
		next       func(*testing.T) *hooks.Event
		wantLocal  string
		wantStatus string
	}{
		{"then tool-start", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PreToolUse", "Bash") }, "executing", parentHeldExecuting},
		{"then prompt", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "UserPromptSubmit", "") }, "thinking", parentHeldThinking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "waiting_for_input")

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "Notification", ""))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey)
			assert.Empty(t, got)

			// The tool-end and Stop that follow keep it.
			for _, raw := range []string{"PostToolUse", "Stop"} {
				got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, raw, "Bash"))
				assert.Equal(t, "waiting_for_input", info["activity"], raw)
				assert.Empty(t, got, raw)
			}

			got, info = parentHeldStep(t, sh, hh, sent, tc.next(t))
			assert.Equal(t, tc.wantLocal, info["activity"])
			assert.Equal(t, []string{tc.wantStatus}, got)
		})
	}
}

// Part B2: two parallel sub-agents each ask. The first one's answer clears
// only its own question; the parent's tool-end for that sub-agent (its Agent
// tool returning) keeps the sibling's wait; the second answer clears it.
func TestParentHeld_ParallelSubagentQuestions(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")

	got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
	assert.Equal(t, []string{parentHeldWaiting}, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubB))
	assert.Equal(t, []string{parentHeldSubA, parentHeldSubB}, waitOwnersOf(info))
	assert.Equal(t, []string{parentHeldWaiting}, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PostToolUse", "AskUserQuestion", parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"], "B's question is still open")
	assert.Equal(t, activitySourceNotification, info[activitySourceKey])
	assert.Equal(t, []string{parentHeldSubB}, waitOwnersOf(info))
	assert.Empty(t, got)

	// A keeps working while B's question is open.
	got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "Bash", parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PostToolUse", "Agent"))
	assert.Equal(t, "waiting_for_input", info["activity"], "the parent's Agent tool-end keeps B's wait")
	assert.Equal(t, []string{parentHeldSubB}, waitOwnersOf(info))
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PostToolUse", "Bash", parentHeldSubB))
	assert.Equal(t, "working", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// A sub-agent that holds no question leaves the owner list and the wait
// alone, with no Hub update.
func TestParentHeld_NonOwnerSubagentKeepsOwnerList(t *testing.T) {
	for _, raw := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure", "UserPromptSubmit", "Stop"} {
		t.Run(raw, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "working")
			parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, raw, "Bash", parentHeldSubC))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.Empty(t, got)
		})
	}
}

// SubagentStop removes only that sub-agent's entry, never changes the
// activity and sends no Hub update. Once no owner is left, the parent's
// Agent tool-end clears the wait as before.
func TestParentHeld_SubagentEndReleasesOnlyItsOwn(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")
	parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))
	parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "ExitPlanMode", parentHeldSubB))

	stop := func(id string) *hooks.Event {
		e := parentHeldSub(t, "SubagentStop", "", id)
		require.Equal(t, hooks.EventSubagentEnd, e.Name)
		return e
	}

	got, info := parentHeldStep(t, sh, hh, sent, stop(parentHeldSubC))
	assert.Equal(t, []string{parentHeldSubA, parentHeldSubB}, waitOwnersOf(info), "a non-owner's stop changes nothing")
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, stop(parentHeldSubA))
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, []string{parentHeldSubB}, waitOwnersOf(info))
	assert.Empty(t, got, "SubagentStop sends no hub update")

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PostToolUse", "Agent"))
	assert.Equal(t, "waiting_for_input", info["activity"], "B still holds the wait")
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, stop(parentHeldSubB))
	assert.Equal(t, "waiting_for_input", info["activity"], "SubagentStop never changes the activity")
	assert.NotContains(t, info, notificationWaitAgentsKey)
	assert.Empty(t, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldParent(t, "PostToolUse", "Agent"))
	assert.Equal(t, "working", info["activity"])
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// With nothing held, SubagentStop writes nothing and sends nothing.
func TestParentHeld_SubagentEndNoWaitIsNoOp(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	require.NoError(t, sh.UpdateActivity(state.ActivityExecuting, "Agent"))
	got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "SubagentStop", "", parentHeldSubA))
	assert.Equal(t, "executing", info["activity"])
	assert.Equal(t, "Agent", info["toolName"])
	assert.Empty(t, got)
}

// The owner list is capped; past the cap a sub-agent's question still sets
// the wait but is not recorded.
func TestParentHeld_OwnerListCap(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}
	for i := 0; i < maxNotificationWaitAgents+6; i++ {
		require.NoError(t, h.Handle(parentHeldSub(t, "Notification", "", fmt.Sprintf("a%016x", i))))
	}
	info := readAgentInfoMap(t, statusPath)
	assert.Equal(t, "waiting_for_input", info["activity"])
	got := waitOwnersOf(info)
	require.Len(t, got, maxNotificationWaitAgents)
	assert.Equal(t, fmt.Sprintf("a%016x", 0), got[0])
	assert.Equal(t, fmt.Sprintf("a%016x", maxNotificationWaitAgents-1), got[maxNotificationWaitAgents-1])

	// A repeated question from a listed sub-agent is recorded once.
	require.NoError(t, h.Handle(parentHeldSub(t, "Notification", "", fmt.Sprintf("a%016x", 0))))
	assert.Len(t, waitOwnersOf(readAgentInfoMap(t, statusPath)), maxNotificationWaitAgents)
}

// Sub-agents that never send an end event are cleaned up by the parent's
// turn end or next prompt, and by any other non-notification write.
func TestParentHeld_OwnerListCleanup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		event     func(*testing.T) *hooks.Event
		wantLocal string
	}{
		{"parent Stop", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "Stop", "") }, "working"},
		{"parent prompt", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "UserPromptSubmit", "") }, "thinking"},
		{"parent tool-start", func(t *testing.T) *hooks.Event { return parentHeldParent(t, "PreToolUse", "Bash") }, "executing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "working")
			parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PreToolUse", "AskUserQuestion", parentHeldSubA))

			got, info := parentHeldStep(t, sh, hh, sent, tc.event(t))
			assert.Equal(t, tc.wantLocal, info["activity"])
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.NotContains(t, info, activitySourceKey)
			assert.Len(t, got, 1)
		})
	}

	t.Run("ask_user write", func(t *testing.T) {
		statusPath := filepath.Join(t.TempDir(), "agent-info.json")
		h := &StatusHandler{StatusPath: statusPath}
		require.NoError(t, h.Handle(parentHeldSub(t, "Notification", "", parentHeldSubA)))
		require.NoError(t, h.UpdateActivity(state.ActivityWaitingForInput, ""))
		info := readAgentInfoMap(t, statusPath)
		assert.NotContains(t, info, notificationWaitAgentsKey)
		assert.NotContains(t, info, activitySourceKey)
	})
}

// Part D: a claude sub-agent's permission Notification sets a
// notification-set wait owned by that sub-agent and reports it; its own
// tool-end clears it on both sides.
func TestParentHeld_ClaudeSubagentNotification(t *testing.T) {
	sh, hh, sent := claudeHubSetup(t)
	parentHeldHold(t, sh, "working")

	event := parentHeldSub(t, "Notification", "", parentHeldSubA)
	require.Equal(t, hooks.EventNotification, event.Name)
	require.True(t, isSubagentEvent(event))

	got, info := parentHeldStep(t, sh, hh, sent, event)
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, activitySourceNotification, info[activitySourceKey])
	assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
	assert.Equal(t, []string{parentHeldWaiting}, got)

	got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PostToolUse", "Bash", parentHeldSubA))
	assert.Equal(t, "working", info["activity"])
	assert.Equal(t, []string{parentHeldWorking}, got)
}

// Part D: through the claude dialect, an event with agent_id but no
// session_id is a sub-agent's, and a non-string agent_id is the parent's.
func TestParentHeld_ClaudeAgentIDEdgeCases(t *testing.T) {
	t.Run("agent_id without session_id", func(t *testing.T) {
		event, err := dialects.NewClaudeDialect().Parse(map[string]interface{}{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Bash",
			"agent_id":        parentHeldSubA,
		})
		require.NoError(t, err)
		require.Empty(t, event.Data.SessionID)
		assert.True(t, isSubagentEvent(event))
		assert.Equal(t, parentHeldSubA, subagentID(event))

		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "blocked")
		got, info := parentHeldStep(t, sh, hh, sent, event)
		assert.Equal(t, "blocked", info["activity"])
		assert.Empty(t, got)
	})

	t.Run("non-string agent_id", func(t *testing.T) {
		event := parentHeldClaude(t, "PreToolUse", "Bash", 12345)
		assert.False(t, isSubagentEvent(event))
		assert.Empty(t, subagentID(event))

		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "waiting_for_input")
		got, info := parentHeldStep(t, sh, hh, sent, event)
		assert.Equal(t, "executing", info["activity"], "treated as the parent: the ask_user wait is cleared")
		assert.Equal(t, []string{parentHeldExecuting}, got)
	})
}

// loadClaudeSubagentFixture parses each record of the captured Claude Code
// sub-agent hook payloads through the real claude dialect.
func loadClaudeSubagentFixture(t *testing.T) []*hooks.Event {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	path := filepath.Join(filepath.Dir(file), "..", "dialects", "testdata", "claude", "hook-payloads-2.1.296.jsonl")
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	var events []*hooks.Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &payload))
		event, err := dialects.NewClaudeDialect().Parse(payload)
		require.NoError(t, err)
		events = append(events, event)
	}
	require.NoError(t, scanner.Err())
	require.Len(t, events, 7)
	return events
}

// Part C: the captured sequence (parent Agent tool-start, sub-agent Bash
// tool-start and tool-end, SubagentStop, parent Agent tool-end, parent Stop)
// replayed while the parent is blocked or crashed keeps the held status on
// both sides with no Hub update at any step; the parent tool-starts follow
// the Part A rule. Replayed from working, the Hub reports each step's local
// result.
func TestParentHeld_CapturedClaudeSubagentReplay(t *testing.T) {
	events := loadClaudeSubagentFixture(t)
	wantSub := []bool{false, false, true, true, true, false, false}
	for i, e := range events {
		assert.Equal(t, wantSub[i], isSubagentEvent(e), "record %d (%s)", i, e.RawName)
	}
	// The SessionStart record is new work and clears any hold; replay the
	// rest of the turn.
	turn := events[1:]

	for _, held := range []string{"blocked", "crashed"} {
		t.Run(held, func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, held)
			for _, e := range turn {
				got, info := parentHeldStep(t, sh, hh, sent, e)
				assert.Equal(t, held, info["activity"], e.RawName)
				assert.Empty(t, got, e.RawName)
			}
		})
	}

	t.Run("working", func(t *testing.T) {
		sh, hh, sent := claudeHubSetup(t)
		parentHeldHold(t, sh, "working")
		want := []struct{ local, hub string }{
			{"executing", parentHeldExecuting}, // parent Agent tool-start
			{"executing", parentHeldExecuting}, // sub-agent Bash tool-start
			{"working", parentHeldWorking},     // sub-agent Bash tool-end
			{"working", ""},                    // SubagentStop: no change
			{"working", parentHeldWorking},     // parent Agent tool-end
			{"working", parentHeldWorking},     // parent Stop
		}
		for i, e := range turn {
			got, info := parentHeldStep(t, sh, hh, sent, e)
			assert.Equal(t, want[i].local, info["activity"], "step %d %s", i, e.RawName)
			if want[i].hub == "" {
				assert.Empty(t, got, "step %d %s", i, e.RawName)
			} else {
				assert.Equal(t, []string{want[i].hub}, got, "step %d %s", i, e.RawName)
			}
		}
	})
}

// setWaitingFromNotification sets a notification-set waiting_for_input with
// no owning sub-agent. Test-only helper: production code calls
// setWaitingFromNotificationFor.
func (h *StatusHandler) setWaitingFromNotification() error {
	return h.setWaitingFromNotificationFor("")
}

// The B1 guard applies to every dialect that sends a Notification. A gemini
// or codex Notification during a wait the agent set itself keeps that wait
// (no marker) and sends no Hub update. The parent's next tool-start still
// clears it on both sides.
func TestParentHeld_OtherDialectNotificationKeepsAgentSetWait(t *testing.T) {
	parse := func(t *testing.T, d interface {
		Parse(map[string]interface{}) (*hooks.Event, error)
	}, payload map[string]interface{}) *hooks.Event {
		t.Helper()
		e, err := d.Parse(payload)
		require.NoError(t, err)
		return e
	}
	for _, tc := range []struct {
		name      string
		notify    func(*testing.T) *hooks.Event
		toolStart func(*testing.T) *hooks.Event
	}{
		{"gemini",
			func(t *testing.T) *hooks.Event {
				return parse(t, dialects.NewGeminiDialect(), map[string]interface{}{
					"hook_event_name": "Notification", "session_id": "gemini-synthetic-session", "message": "Approve tool?",
				})
			},
			func(t *testing.T) *hooks.Event {
				return parse(t, dialects.NewGeminiDialect(), map[string]interface{}{
					"hook_event_name": "BeforeTool", "session_id": "gemini-synthetic-session", "tool_name": "run_shell_command",
				})
			}},
		{"codex",
			func(t *testing.T) *hooks.Event {
				return parse(t, dialects.NewCodexDialect(), map[string]interface{}{
					"type": "notification", "session_id": "codex-synthetic-session", "message": "Approve command?",
				})
			},
			func(t *testing.T) *hooks.Event {
				return parse(t, dialects.NewCodexDialect(), map[string]interface{}{
					"type": "tool-start", "session_id": "codex-synthetic-session", "tool_name": "shell",
				})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notify := tc.notify(t)
			require.Equal(t, hooks.EventNotification, notify.Name)
			require.Equal(t, tc.name, notify.Dialect)

			sh, hh, sent := claudeHubSetup(t)
			parentHeldHold(t, sh, "waiting_for_input")

			got, info := parentHeldStep(t, sh, hh, sent, notify)
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey, "the wait stays agent-set")
			assert.Empty(t, got, "no hub update replaces the agent-set wait")

			start := tc.toolStart(t)
			require.Equal(t, hooks.EventToolStart, start.Name)
			got, info = parentHeldStep(t, sh, hh, sent, start)
			assert.Equal(t, "executing", info["activity"])
			assert.Equal(t, []string{parentHeldExecuting}, got)
		})
	}
}

// parentHeldWriteInfo replaces agent-info.json with info.
func parentHeldWriteInfo(t *testing.T, path string, info map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// A malformed owner list is tolerated. A non-list value, or a list with no
// usable string entries, counts as no owners, so the wait behaves as an
// owner-less notification wait (the behaviour before owner tracking). A
// list already over the cap is read in full: listed owners can still release
// themselves, and no new owner is added.
func TestParentHeld_MalformedOwnerList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value interface{}
	}{
		{"string", "x"},
		{"number", 42},
		{"object", map[string]interface{}{"a": "b"}},
		{"non-string entries", []interface{}{1, nil, "", true}},
	} {
		t.Run(tc.name+"/sub-agent tool-end clears", func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldWriteInfo(t, sh.StatusPath, map[string]interface{}{
				"activity": "waiting_for_input", activitySourceKey: activitySourceNotification, notificationWaitAgentsKey: tc.value,
			})
			assert.Empty(t, waitOwnersOf(readAgentInfoMap(t, sh.StatusPath)))

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PostToolUse", "Bash", parentHeldSubC))
			assert.Equal(t, "working", info["activity"])
			assert.NotContains(t, info, notificationWaitAgentsKey)
			assert.Equal(t, []string{parentHeldWorking}, got)
		})
		t.Run(tc.name+"/sub-agent question replaces it", func(t *testing.T) {
			sh, hh, sent := claudeHubSetup(t)
			parentHeldWriteInfo(t, sh.StatusPath, map[string]interface{}{
				"activity": "waiting_for_input", activitySourceKey: activitySourceNotification, notificationWaitAgentsKey: tc.value,
			})

			got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.Equal(t, []string{parentHeldSubA}, waitOwnersOf(info))
			assert.Equal(t, []string{parentHeldWaiting}, got)
		})
	}

	t.Run("over the cap", func(t *testing.T) {
		list := make([]interface{}, maxNotificationWaitAgents+6)
		for i := range list {
			list[i] = fmt.Sprintf("a%016x", i)
		}
		sh, hh, sent := claudeHubSetup(t)
		parentHeldWriteInfo(t, sh.StatusPath, map[string]interface{}{
			"activity": "waiting_for_input", activitySourceKey: activitySourceNotification, notificationWaitAgentsKey: list,
		})

		got, info := parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "Notification", "", parentHeldSubA))
		assert.Len(t, waitOwnersOf(info), len(list), "no new owner past the cap")
		assert.NotContains(t, waitOwnersOf(info), parentHeldSubA)
		assert.Equal(t, []string{parentHeldWaiting}, got)

		got, info = parentHeldStep(t, sh, hh, sent, parentHeldSub(t, "PostToolUse", "Bash", fmt.Sprintf("a%016x", 0)))
		assert.Equal(t, "waiting_for_input", info["activity"], "other listed owners still hold the wait")
		assert.Len(t, waitOwnersOf(info), len(list)-1)
		assert.Empty(t, got)
	})
}
