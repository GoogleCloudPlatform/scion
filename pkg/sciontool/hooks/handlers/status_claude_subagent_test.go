/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover claude hook events sent by a sub-agent. Claude Code puts
// the parent session's session_id on a sub-agent's hook events and names the
// child in agent_id, so such an event must not clear a status the parent
// agent set and still holds. All payloads are synthetic and are parsed
// through the real claude dialect.

const (
	claudeSubagentParentSession = "33333333-3333-3333-3333-333333333333"
	claudeSubagentChildID       = "a1b2c3d4e5f60718"
)

// claudeEvent parses a synthetic claude hook payload. A non-empty agentID
// adds the agent_id and agent_type fields that mark a sub-agent's event.
func claudeEvent(t *testing.T, hookEventName, toolName, agentID string) *hooks.Event {
	t.Helper()
	payload := map[string]interface{}{
		"hook_event_name": hookEventName,
		"session_id":      claudeSubagentParentSession,
		"cwd":             "/workspace",
	}
	switch hookEventName {
	case "UserPromptSubmit":
		payload["prompt"] = "synthetic prompt"
	case "PreToolUse", "PostToolUse":
		payload["tool_name"] = toolName
		payload["tool_input"] = map[string]interface{}{"command": "ls"}
	}
	if agentID != "" {
		payload["agent_id"] = agentID
		payload["agent_type"] = "general-purpose"
	}
	event, err := dialects.NewClaudeDialect().Parse(payload)
	require.NoError(t, err)
	require.Equal(t, "claude", event.Dialect)
	return event
}

func claudeSubagentEvent(t *testing.T, hookEventName string) *hooks.Event {
	return claudeEvent(t, hookEventName, "Bash", claudeSubagentChildID)
}

func claudeParentEvent(t *testing.T, hookEventName string) *hooks.Event {
	return claudeEvent(t, hookEventName, "Bash", "")
}

func TestClaudeSubagent_IsSubagentEvent(t *testing.T) {
	for raw, want := range map[string]string{
		"UserPromptSubmit": hooks.EventPromptSubmit,
		"PreToolUse":       hooks.EventToolStart,
		"PostToolUse":      hooks.EventToolEnd,
	} {
		event := claudeSubagentEvent(t, raw)
		assert.Equal(t, want, event.Name, raw)
		assert.Equal(t, claudeSubagentChildID, event.Data.Raw["agent_id"], raw)
		assert.True(t, isSubagentEvent(event), raw)
		assert.False(t, isSubagentEvent(claudeParentEvent(t, raw)), raw)
	}

	sameAsSession := claudeEvent(t, "UserPromptSubmit", "", claudeSubagentParentSession)
	assert.False(t, isSubagentEvent(sameAsSession), "agent_id equal to session_id is the parent")
}

// A sub-agent prompt-submit or tool-start leaves every status the parent set
// and still holds unchanged: an agent-set waiting_for_input (sciontool status
// ask_user), blocked, completed, limits_exceeded and crashed.
func TestClaudeSubagent_HeldActivitiesKept(t *testing.T) {
	for _, held := range []state.Activity{state.ActivityWaitingForInput, state.ActivityBlocked, state.ActivityCompleted, state.ActivityLimitsExceeded, state.ActivityCrashed} {
		for _, raw := range []string{"UserPromptSubmit", "PreToolUse"} {
			t.Run(string(held)+"/"+raw, func(t *testing.T) {
				statusPath := filepath.Join(t.TempDir(), "agent-info.json")
				h := &StatusHandler{StatusPath: statusPath}
				require.NoError(t, h.UpdateActivity(held, ""))

				require.NoError(t, h.Handle(claudeSubagentEvent(t, raw)))

				info := readAgentInfo(t, statusPath)
				assert.Equal(t, string(held), info.Activity,
					"a sub-agent %s must not clear the parent's held %s", raw, held)
				assert.Empty(t, info.ToolName)
			})
		}
	}
}

// A waiting_for_input that a Notification set is a harness prompt, not a
// status the agent set itself, so a sub-agent event clears it, as tool-end
// and agent-end do. This matches the codex sub-agent rule.
func TestClaudeSubagent_NotificationWaitingCleared(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		want     string
		wantTool string
	}{
		{"UserPromptSubmit", "thinking", ""},
		{"PreToolUse", "executing", "Bash"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			require.NoError(t, h.setWaitingFromNotification())

			require.NoError(t, h.Handle(claudeSubagentEvent(t, tc.raw)))

			info := readAgentInfoMap(t, statusPath)
			assert.Equal(t, tc.want, info["activity"])
			assert.NotContains(t, info, activitySourceKey)
			if tc.wantTool == "" {
				assert.NotContains(t, info, "toolName")
			} else {
				assert.Equal(t, tc.wantTool, info["toolName"])
			}
		})
	}
}

// The parent's own events behave as before: prompt-submit clears any held
// status, tool-start clears waiting_for_input. An agent_id equal to
// session_id is the parent.
func TestClaudeSubagent_ParentEventsUnchanged(t *testing.T) {
	prompt := func(t *testing.T) *hooks.Event { return claudeParentEvent(t, "UserPromptSubmit") }
	for _, tc := range []struct {
		name     string
		event    func(*testing.T) *hooks.Event
		held     state.Activity
		want     string
		wantTool string
	}{
		{"prompt clears ask_user", prompt, state.ActivityWaitingForInput, "thinking", ""},
		{"prompt clears blocked", prompt, state.ActivityBlocked, "thinking", ""},
		{"prompt clears completed", prompt, state.ActivityCompleted, "thinking", ""},
		{"prompt clears crashed", prompt, state.ActivityCrashed, "thinking", ""},
		{"prompt with agent_id equal to session clears ask_user", func(t *testing.T) *hooks.Event {
			return claudeEvent(t, "UserPromptSubmit", "", claudeSubagentParentSession)
		}, state.ActivityWaitingForInput, "thinking", ""},
		{"tool-start clears ask_user", func(t *testing.T) *hooks.Event { return claudeParentEvent(t, "PreToolUse") }, state.ActivityWaitingForInput, "executing", "Bash"},
		{"tool-start with agent_id equal to session clears ask_user", func(t *testing.T) *hooks.Event {
			return claudeEvent(t, "PreToolUse", "Bash", claudeSubagentParentSession)
		}, state.ActivityWaitingForInput, "executing", "Bash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			require.NoError(t, h.UpdateActivity(tc.held, ""))

			require.NoError(t, h.Handle(tc.event(t)))

			info := readAgentInfo(t, statusPath)
			assert.Equal(t, tc.want, info.Activity)
			assert.Equal(t, tc.wantTool, info.ToolName)
		})
	}
}

// When the parent holds nothing, a sub-agent event updates the status as a
// parent event would.
func TestClaudeSubagent_NoHoldUpdatesAsBefore(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		want     string
		wantTool string
	}{
		{"UserPromptSubmit", "thinking", ""},
		{"PreToolUse", "executing", "Bash"},
		{"PostToolUse", "working", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			require.NoError(t, h.UpdateActivity(state.ActivityWorking, ""))

			require.NoError(t, h.Handle(claudeSubagentEvent(t, tc.raw)))

			info := readAgentInfo(t, statusPath)
			assert.Equal(t, tc.want, info.Activity)
			assert.Equal(t, tc.wantTool, info.ToolName)
		})
	}
}

// claudeHubSetup points a StatusHandler and a HubHandler at a temp HOME and
// an httptest Hub, and returns them with a func that reports the Hub status
// values sent so far.
func claudeHubSetup(t *testing.T) (*StatusHandler, *HubHandler, func() []string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	var mu sync.Mutex
	var statuses []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		s, _ := payload["status"].(string)
		mu.Lock()
		statuses = append(statuses, s)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent-id")

	statusHandler := NewStatusHandler()
	require.Equal(t, filepath.Join(home, "agent-info.json"), statusHandler.StatusPath)
	hubHandler := NewHubHandler()
	require.NotNil(t, hubHandler)

	return statusHandler, hubHandler, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), statuses...)
	}
}

// claudeHubHandle runs one event through the StatusHandler and then the
// HubHandler, the hook process order.
func claudeHubHandle(t *testing.T, statusHandler *StatusHandler, hubHandler *HubHandler, event *hooks.Event) {
	t.Helper()
	require.NoError(t, statusHandler.Handle(event))
	require.NoError(t, hubHandler.Handle(event))
}

// claudeHubRun sets up a parent status, runs one event through both
// handlers against an httptest Hub, and returns the Hub status values sent
// and the local activity afterwards.
func claudeHubRun(t *testing.T, held state.Activity, notificationWait bool, event *hooks.Event) ([]string, string) {
	t.Helper()
	statusHandler, hubHandler, sent := claudeHubSetup(t)
	if notificationWait {
		require.NoError(t, statusHandler.setWaitingFromNotification())
	} else {
		require.NoError(t, statusHandler.UpdateActivity(held, ""))
	}

	claudeHubHandle(t, statusHandler, hubHandler, event)
	return sent(), readAgentInfo(t, statusHandler.StatusPath).Activity
}

// The Hub follows the local result: a sub-agent event that the parent's held
// status absorbs sends no Hub update, while the parent's own events and
// events with nothing held are reported as before.
func TestClaudeSubagent_HubFollowsLocal(t *testing.T) {
	sub := claudeSubagentEvent
	parent := claudeParentEvent
	for _, tc := range []struct {
		name             string
		held             state.Activity
		notificationWait bool
		event            func(*testing.T, string) *hooks.Event
		raw              string
		// wantStatus is the Hub status sent; "" means no Hub call.
		wantStatus string
		wantLocal  string
	}{
		{"sub-agent prompt with ask_user", state.ActivityWaitingForInput, false, sub, "UserPromptSubmit", "", "waiting_for_input"},
		{"sub-agent tool-start with ask_user", state.ActivityWaitingForInput, false, sub, "PreToolUse", "", "waiting_for_input"},
		{"sub-agent prompt with blocked", state.ActivityBlocked, false, sub, "UserPromptSubmit", "", "blocked"},
		{"sub-agent tool-start with blocked", state.ActivityBlocked, false, sub, "PreToolUse", "", "blocked"},
		{"sub-agent prompt with completed", state.ActivityCompleted, false, sub, "UserPromptSubmit", "", "completed"},
		{"sub-agent tool-start with crashed", state.ActivityCrashed, false, sub, "PreToolUse", "", "crashed"},
		{"sub-agent prompt with limits_exceeded", state.ActivityLimitsExceeded, false, sub, "UserPromptSubmit", "", "limits_exceeded"},
		{"sub-agent tool-start with limits_exceeded", state.ActivityLimitsExceeded, false, sub, "PreToolUse", "", "limits_exceeded"},
		{"sub-agent prompt with notification wait", "", true, sub, "UserPromptSubmit", "thinking", "thinking"},
		{"sub-agent tool-start with notification wait", "", true, sub, "PreToolUse", "executing", "executing"},
		{"sub-agent prompt with no hold", state.ActivityWorking, false, sub, "UserPromptSubmit", "thinking", "thinking"},
		{"sub-agent tool-start with no hold", state.ActivityWorking, false, sub, "PreToolUse", "executing", "executing"},
		{"parent prompt with ask_user", state.ActivityWaitingForInput, false, parent, "UserPromptSubmit", "thinking", "thinking"},
		{"parent prompt with blocked", state.ActivityBlocked, false, parent, "UserPromptSubmit", "thinking", "thinking"},
		{"parent tool-start with ask_user", state.ActivityWaitingForInput, false, parent, "PreToolUse", "executing", "executing"},
		{"parent tool-start with notification wait", "", true, parent, "PreToolUse", "executing", "executing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, local := claudeHubRun(t, tc.held, tc.notificationWait, tc.event(t, tc.raw))
			if tc.wantStatus == "" {
				assert.Empty(t, got, "no hub update expected")
			} else {
				assert.Equal(t, []string{tc.wantStatus}, got)
			}
			assert.Equal(t, tc.wantLocal, local)
		})
	}
}

// A sub-agent's AskUserQuestion or ExitPlanMode tool-start asks the user, so
// it still sets waiting_for_input locally and reports it to the Hub, as the
// parent's does. It overwrites whatever the parent held, including blocked,
// completed, limits_exceeded and crashed; that is unchanged from before the
// sub-agent check. The wait is recorded as notification-set.
func TestClaudeSubagent_UserWaitToolSetsWaiting(t *testing.T) {
	waitingStatus := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}.DisplayStatus()
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		for _, held := range []state.Activity{state.ActivityWorking, state.ActivityWaitingForInput, state.ActivityBlocked, state.ActivityCompleted, state.ActivityLimitsExceeded, state.ActivityCrashed} {
			t.Run(tool+"/"+string(held), func(t *testing.T) {
				event := claudeEvent(t, "PreToolUse", tool, claudeSubagentChildID)
				require.True(t, isSubagentEvent(event))

				statusHandler, hubHandler, sent := claudeHubSetup(t)
				require.NoError(t, statusHandler.UpdateActivity(held, ""))
				claudeHubHandle(t, statusHandler, hubHandler, event)

				info := readAgentInfoMap(t, statusHandler.StatusPath)
				assert.Equal(t, "waiting_for_input", info["activity"])
				assert.Equal(t, activitySourceNotification, info[activitySourceKey])
				assert.Equal(t, []string{waitingStatus}, sent())
			})
		}
	}
}

// A sub-agent asks the user, the user answers, and the sub-agent keeps
// working: its own tool-end clears the wait and the next tool-start is
// reported, locally and to the Hub. The wait does not outlive the answer.
func TestClaudeSubagent_UserWaitAnsweredThenContinues(t *testing.T) {
	waitingStatus := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}.DisplayStatus()
	workingStatus := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWorking}.DisplayStatus()
	executingStatus := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityExecuting}.DisplayStatus()
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		t.Run(tool, func(t *testing.T) {
			statusHandler, hubHandler, sent := claudeHubSetup(t)
			require.NoError(t, statusHandler.UpdateActivity(state.ActivityWorking, ""))

			claudeHubHandle(t, statusHandler, hubHandler, claudeEvent(t, "PreToolUse", tool, claudeSubagentChildID))
			assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusHandler.StatusPath).Activity, "after the sub-agent asks")

			claudeHubHandle(t, statusHandler, hubHandler, claudeEvent(t, "PostToolUse", tool, claudeSubagentChildID))
			assert.Equal(t, "working", readAgentInfo(t, statusHandler.StatusPath).Activity, "after the user answers")

			claudeHubHandle(t, statusHandler, hubHandler, claudeSubagentEvent(t, "PreToolUse"))
			info := readAgentInfo(t, statusHandler.StatusPath)
			assert.Equal(t, "executing", info.Activity, "sub-agent work after the answer")
			assert.Equal(t, "Bash", info.ToolName)

			assert.Equal(t, []string{waitingStatus, workingStatus, executingStatus}, sent())
		})
	}
}

// The parent's own AskUserQuestion or ExitPlanMode wait is unchanged: it is
// an agent-set wait, kept by the tool-end that follows and cleared by the
// parent's next tool-start.
func TestClaudeSubagent_ParentUserWaitUnchanged(t *testing.T) {
	waitingStatus := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}.DisplayStatus()
	executingStatus := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityExecuting}.DisplayStatus()
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		t.Run(tool, func(t *testing.T) {
			statusHandler, hubHandler, sent := claudeHubSetup(t)
			require.NoError(t, statusHandler.UpdateActivity(state.ActivityWorking, ""))

			claudeHubHandle(t, statusHandler, hubHandler, claudeEvent(t, "PreToolUse", tool, ""))
			info := readAgentInfoMap(t, statusHandler.StatusPath)
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey)

			claudeHubHandle(t, statusHandler, hubHandler, claudeEvent(t, "PostToolUse", tool, ""))
			assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusHandler.StatusPath).Activity, "the parent's tool-end keeps its wait")

			claudeHubHandle(t, statusHandler, hubHandler, claudeParentEvent(t, "PreToolUse"))
			assert.Equal(t, "executing", readAgentInfo(t, statusHandler.StatusPath).Activity)

			assert.Equal(t, []string{waitingStatus, executingStatus}, sent())
		})
	}
}
