/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover when a waiting_for_input status is cleared by tool-end
// and agent-end events. A waiting_for_input that a Notification set (a
// harness permission or elicitation prompt) is cleared once the prompt is
// answered (the tool ran) or the turn ends. A waiting_for_input the agent
// set itself (sciontool status ask_user) and every other sticky activity
// stay in place.

// waitClearClaudeEvent parses a synthetic Claude Code hook payload through
// the real claude dialect.
func waitClearClaudeEvent(t *testing.T, payload map[string]interface{}) *hooks.Event {
	t.Helper()
	event, err := dialects.NewClaudeDialect().Parse(payload)
	require.NoError(t, err)
	return event
}

func waitClearClaudeToolStart(t *testing.T) *hooks.Event {
	return waitClearClaudeEvent(t, map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "00000000-0000-0000-0000-000000000000",
		"tool_name":       "Bash",
		"tool_input":      map[string]interface{}{"command": "ls"},
	})
}

func waitClearClaudeNotification(t *testing.T) *hooks.Event {
	return waitClearClaudeEvent(t, map[string]interface{}{
		"hook_event_name":   "Notification",
		"session_id":        "00000000-0000-0000-0000-000000000000",
		"message":           "Claude needs your permission to use Bash",
		"notification_type": "permission_prompt",
	})
}

func waitClearClaudeToolEnd(t *testing.T) *hooks.Event {
	return waitClearClaudeEvent(t, map[string]interface{}{
		"hook_event_name": "PostToolUse",
		"session_id":      "00000000-0000-0000-0000-000000000000",
		"tool_name":       "Bash",
		"tool_input":      map[string]interface{}{"command": "ls"},
		"tool_response":   map[string]interface{}{"stdout": ""},
	})
}

// waitClearClaudeToolFailure is the event Claude Code sends instead of
// PostToolUse when the tool call fails.
func waitClearClaudeToolFailure(t *testing.T) *hooks.Event {
	return waitClearClaudeEvent(t, map[string]interface{}{
		"hook_event_name": "PostToolUseFailure",
		"session_id":      "00000000-0000-0000-0000-000000000000",
		"tool_name":       "Bash",
		"tool_input":      map[string]interface{}{"command": "false"},
		"tool_use_id":     "toolu_synthetic",
		"error":           "Command exited with non-zero status code 1",
		"is_interrupt":    false,
	})
}

func waitClearClaudeStop(t *testing.T) *hooks.Event {
	return waitClearClaudeEvent(t, map[string]interface{}{
		"hook_event_name": "Stop",
		"session_id":      "00000000-0000-0000-0000-000000000000",
	})
}

// waitClearAskUser applies the status write that sciontool status ask_user
// (cmd/sciontool/commands/status.go runStatusUpdate) and the legacy
// runAskUser (hook.go) both make.
func waitClearAskUser(t *testing.T, h *StatusHandler) {
	t.Helper()
	require.NoError(t, h.UpdateActivity(state.ActivityWaitingForInput, ""))
}

func TestStatusWaitingClear_ClaudeNotificationThenEndEvent(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*testing.T) *hooks.Event
	}{
		{"PostToolUse", waitClearClaudeToolEnd},
		{"PostToolUseFailure", waitClearClaudeToolFailure},
		{"Stop", waitClearClaudeStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}

			require.NoError(t, h.Handle(waitClearClaudeToolStart(t)))
			require.NoError(t, h.Handle(waitClearClaudeNotification(t)))
			info := readAgentInfoMap(t, statusPath)
			require.Equal(t, "waiting_for_input", info["activity"])
			require.Equal(t, activitySourceNotification, info[activitySourceKey])

			require.NoError(t, h.Handle(tc.end(t)))
			info = readAgentInfoMap(t, statusPath)
			assert.Equal(t, "working", info["activity"], "answered prompt must leave waiting_for_input")
			assert.NotContains(t, info, activitySourceKey, "marker must be dropped with the activity it described")
		})
	}
}

func TestStatusWaitingClear_AgentSetWaitingStaysAcrossEndEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*testing.T) *hooks.Event
	}{
		{"PostToolUse", waitClearClaudeToolEnd},
		{"PostToolUseFailure", waitClearClaudeToolFailure},
		{"Stop", waitClearClaudeStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}

			// claude runs sciontool status ask_user through its Bash tool:
			// PreToolUse(Bash), the ask_user write, then PostToolUse(Bash).
			require.NoError(t, h.Handle(waitClearClaudeToolStart(t)))
			waitClearAskUser(t, h)
			require.NoError(t, h.Handle(tc.end(t)))

			assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity,
				"agent-set waiting_for_input must stay sticky")
		})
	}
}

// A later activity write drops the marker, so a Notification followed by
// an agent ask_user leaves an agent-set (sticky) waiting_for_input.
func TestStatusWaitingClear_AskUserAfterNotificationStaysSticky(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	require.NoError(t, h.Handle(waitClearClaudeNotification(t)))
	waitClearAskUser(t, h)
	assert.NotContains(t, readAgentInfoMap(t, statusPath), activitySourceKey)

	require.NoError(t, h.Handle(waitClearClaudeToolEnd(t)))
	require.NoError(t, h.Handle(waitClearClaudeStop(t)))
	assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity)
}

// A marker left in the file by any earlier write must not make an
// agent-set waiting_for_input clearable: the ask_user write drops it.
func TestStatusWaitingClear_StaleMarkerThenAskUserStaysSticky(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	stale := map[string]interface{}{
		"phase":           "running",
		"activity":        "executing",
		activitySourceKey: activitySourceNotification,
	}
	data, err := json.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statusPath, data, 0o644))

	h := &StatusHandler{StatusPath: statusPath}
	waitClearAskUser(t, h)

	require.NoError(t, h.Handle(waitClearClaudeToolEnd(t)))
	assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity, "tool-end")
	require.NoError(t, h.Handle(waitClearClaudeStop(t)))
	assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity, "Stop")
}

// Other sticky activities are kept by tool-end and agent-end, even when a
// stale notification marker is present in the file.
func TestStatusWaitingClear_OtherStickyActivitiesKept(t *testing.T) {
	for _, activity := range []state.Activity{
		state.ActivityCompleted,
		state.ActivityBlocked,
		state.ActivityLimitsExceeded,
		state.ActivityCrashed,
	} {
		for _, withMarker := range []bool{false, true} {
			for _, eventName := range []string{hooks.EventToolEnd, hooks.EventAgentEnd} {
				name := string(activity) + "/" + eventName
				if withMarker {
					name += "/stale-marker"
				}
				t.Run(name, func(t *testing.T) {
					statusPath := filepath.Join(t.TempDir(), "agent-info.json")
					info := map[string]interface{}{"activity": string(activity)}
					if withMarker {
						info[activitySourceKey] = activitySourceNotification
					}
					data, err := json.Marshal(info)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(statusPath, data, 0o644))

					h := &StatusHandler{StatusPath: statusPath}
					require.NoError(t, h.Handle(&hooks.Event{Name: eventName, Dialect: "claude"}))
					assert.Equal(t, string(activity), readAgentInfo(t, statusPath).Activity)
				})
			}
		}
	}
}

// model-end keeps a notification-set waiting_for_input (out of scope; claude
// does not emit it).
func TestStatusWaitingClear_ModelEndKeepsNotificationWaiting(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	require.NoError(t, h.Handle(waitClearClaudeNotification(t)))
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventModelEnd}))
	assert.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity)
}

// Other harnesses whose permission prompt maps to notification clear the
// same way on their tool-end event.
func TestStatusWaitingClear_OtherDialects(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	loadMapping := func(t *testing.T, name string) hooks.Dialect {
		t.Helper()
		md, err := dialects.LoadMappingDialect(filepath.Join(root, "harnesses", name, "dialect.yaml"))
		require.NoError(t, err)
		return md
	}

	for _, tc := range []struct {
		name    string
		dialect func(*testing.T) hooks.Dialect
		notify  map[string]interface{}
		toolEnd map[string]interface{}
	}{
		{
			name:    "gemini",
			dialect: func(*testing.T) hooks.Dialect { return dialects.NewGeminiDialect() },
			notify:  map[string]interface{}{"hook_event_name": "Notification", "notification_type": "ToolPermission", "message": "permission"},
			toolEnd: map[string]interface{}{"hook_event_name": "AfterTool", "tool_name": "run_shell_command"},
		},
		{
			name:    "muse-code",
			dialect: func(t *testing.T) hooks.Dialect { return loadMapping(t, "muse-code") },
			notify:  map[string]interface{}{"hook_event_name": "PermissionRequest", "tool_name": "Bash"},
			toolEnd: map[string]interface{}{"hook_event_name": "PostToolUse", "tool_name": "Bash"},
		},
		{
			name:    "grok-build",
			dialect: func(t *testing.T) hooks.Dialect { return loadMapping(t, "grok-build") },
			notify:  map[string]interface{}{"hookEventName": "Notification", "message": "permission"},
			toolEnd: map[string]interface{}{"hookEventName": "PostToolUse", "toolName": "bash"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.dialect(t)
			notify, err := d.Parse(tc.notify)
			require.NoError(t, err)
			require.Equal(t, hooks.EventNotification, notify.Name)
			toolEnd, err := d.Parse(tc.toolEnd)
			require.NoError(t, err)
			require.Equal(t, hooks.EventToolEnd, toolEnd.Name)

			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			require.NoError(t, h.Handle(notify))
			require.Equal(t, "waiting_for_input", readAgentInfo(t, statusPath).Activity)
			require.NoError(t, h.Handle(toolEnd))
			assert.Equal(t, "working", readAgentInfo(t, statusPath).Activity)
		})
	}
}

// The hub sees the same result: the hook process runs the StatusHandler and
// then the HubHandler against the same agent-info.json.
func TestStatusWaitingClear_HubFollowsLocal(t *testing.T) {
	type step struct {
		event      func(*testing.T) *hooks.Event
		askUser    bool
		wantStatus string // "" means no hub call for this step
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{
			name: "notification then PostToolUse",
			steps: []step{
				{event: waitClearClaudeNotification, wantStatus: "waiting_for_input"},
				{event: waitClearClaudeToolEnd, wantStatus: "working"},
			},
		},
		{
			name: "notification then Stop",
			steps: []step{
				{event: waitClearClaudeNotification, wantStatus: "waiting_for_input"},
				{event: waitClearClaudeStop, wantStatus: "working"},
			},
		},
		{
			name: "ask_user then PostToolUse and Stop",
			steps: []step{
				{askUser: true},
				{event: waitClearClaudeToolEnd},
				{event: waitClearClaudeStop},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			defer server.Close()

			scrubHubEnv(t)
			t.Setenv("SCION_HUB_ENDPOINT", server.URL)
			t.Setenv("SCION_AUTH_TOKEN", "test-token")
			t.Setenv("SCION_AGENT_ID", "test-agent-id")

			statusHandler := NewStatusHandler()
			require.Equal(t, filepath.Join(home, "agent-info.json"), statusHandler.StatusPath)
			hubHandler := NewHubHandler()
			require.NotNil(t, hubHandler)

			for i, s := range tc.steps {
				mu.Lock()
				before := len(statuses)
				mu.Unlock()

				if s.askUser {
					waitClearAskUser(t, statusHandler)
				} else {
					event := s.event(t)
					require.NoError(t, statusHandler.Handle(event))
					require.NoError(t, hubHandler.Handle(event))
				}

				mu.Lock()
				got := append([]string(nil), statuses[before:]...)
				mu.Unlock()
				if s.wantStatus == "" {
					assert.Empty(t, got, "step %d: no hub update expected", i)
				} else {
					assert.Equal(t, []string{s.wantStatus}, got, "step %d", i)
				}
			}
		})
	}
}

// Every activity or phase write other than the Notification path drops the
// marker, so it can never describe an activity it did not set.
func TestStatusWaitingClear_OtherWritesDropMarker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*testing.T, *StatusHandler)
	}{
		{"UpdateActivity", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.UpdateActivity(state.ActivityBlocked, ""))
		}},
		{"UpdatePhase", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.UpdatePhase(state.PhaseRunning, state.ActivityWorking, ""))
		}},
		{"tool-start", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.Handle(waitClearClaudeToolStart(t)))
		}},
		{"tool-end", func(t *testing.T, h *StatusHandler) {
			require.NoError(t, h.Handle(waitClearClaudeToolEnd(t)))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			require.NoError(t, h.Handle(waitClearClaudeNotification(t)))
			require.Contains(t, readAgentInfoMap(t, statusPath), activitySourceKey)

			tc.write(t, h)
			assert.NotContains(t, readAgentInfoMap(t, statusPath), activitySourceKey)
		})
	}
}

// claude AskUserQuestion and ExitPlanMode set an unmarked waiting_for_input
// on PreToolUse. After a Notification, that wait still replaces the
// notification wait, so the tool's own PostToolUse does not clear it.
func TestStatusWaitingClear_ClaudeAskToolsAfterNotificationStaySticky(t *testing.T) {
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		t.Run(tool, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}

			require.NoError(t, h.Handle(waitClearClaudeNotification(t)))
			require.NoError(t, h.Handle(waitClearClaudeEvent(t, map[string]interface{}{
				"hook_event_name": "PreToolUse",
				"session_id":      "00000000-0000-0000-0000-000000000000",
				"tool_name":       tool,
				"tool_input":      map[string]interface{}{},
			})))
			info := readAgentInfoMap(t, statusPath)
			require.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey)

			require.NoError(t, h.Handle(waitClearClaudeEvent(t, map[string]interface{}{
				"hook_event_name": "PostToolUse",
				"session_id":      "00000000-0000-0000-0000-000000000000",
				"tool_name":       tool,
				"tool_input":      map[string]interface{}{},
				"tool_response":   map[string]interface{}{},
			})))
			info = readAgentInfoMap(t, statusPath)
			assert.Equal(t, "waiting_for_input", info["activity"])
			assert.NotContains(t, info, activitySourceKey)
		})
	}
}
