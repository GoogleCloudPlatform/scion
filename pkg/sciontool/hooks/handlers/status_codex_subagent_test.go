/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// These tests cover codex hook events sent by a sub-agent. Codex puts the
// root session's session_id on a sub-agent's hook events and names the child
// only in agent_id, so such an event must not clear a status the parent
// agent set and still holds. All payloads are synthetic.

const (
	codexSubagentRootSession = "11111111-1111-1111-1111-111111111111"
	codexSubagentChildID     = "22222222-2222-2222-2222-222222222222"
)

// loadCodexDialect loads the real harnesses/codex/dialect.yaml, the spec
// staged into codex agents, so these tests follow its event mapping.
func loadCodexDialect(t *testing.T) *dialects.MappingDialect {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	specPath := filepath.Join(root, "harnesses", "codex", "dialect.yaml")
	md, err := dialects.LoadMappingDialect(specPath)
	require.NoError(t, err, "LoadMappingDialect(%s)", specPath)
	require.Equal(t, "codex", md.Name())
	return md
}

// codexEvent parses a synthetic codex hook payload. A non-empty agentID adds
// the agent_id field that marks a sub-agent's event.
func codexEvent(t *testing.T, hookEventName, agentID string) *hooks.Event {
	t.Helper()
	payload := map[string]interface{}{
		"hook_event_name": hookEventName,
		"session_id":      codexSubagentRootSession,
		"cwd":             "/workspace",
	}
	switch hookEventName {
	case "UserPromptSubmit":
		payload["prompt"] = "synthetic prompt"
	case "PreToolUse", "PostToolUse":
		payload["tool_name"] = "Bash"
		payload["tool_input"] = map[string]interface{}{"command": "ls"}
	}
	if agentID != "" {
		payload["agent_id"] = agentID
	}
	event, err := loadCodexDialect(t).Parse(payload)
	require.NoError(t, err)
	require.Equal(t, "codex", event.Dialect)
	return event
}

func codexSubagentEvent(t *testing.T, hookEventName string) *hooks.Event {
	return codexEvent(t, hookEventName, codexSubagentChildID)
}

func codexParentEvent(t *testing.T, hookEventName string) *hooks.Event {
	return codexEvent(t, hookEventName, "")
}

func TestCodexSubagent_DialectMapsEvents(t *testing.T) {
	for raw, want := range map[string]string{
		"UserPromptSubmit": hooks.EventPromptSubmit,
		"PreToolUse":       hooks.EventToolStart,
		"PostToolUse":      hooks.EventToolEnd,
		"Stop":             hooks.EventAgentEnd,
	} {
		event := codexSubagentEvent(t, raw)
		assert.Equal(t, want, event.Name, raw)
		assert.True(t, isCodexSubagentEvent(event), raw)
		assert.False(t, isCodexSubagentEvent(codexParentEvent(t, raw)), raw)
	}
}

func TestCodexSubagent_IsSubagentEvent(t *testing.T) {
	sameAsSession := codexEvent(t, "UserPromptSubmit", codexSubagentRootSession)
	assert.False(t, isCodexSubagentEvent(sameAsSession), "agent_id equal to session_id is the parent")

	notString := codexParentEvent(t, "UserPromptSubmit")
	notString.Data.Raw["agent_id"] = 42
	assert.False(t, isCodexSubagentEvent(notString), "non-string agent_id is ignored")

	claude := waitClearClaudeEvent(t, map[string]interface{}{
		"hook_event_name": "UserPromptSubmit",
		"session_id":      codexSubagentRootSession,
		"agent_id":        codexSubagentChildID,
		"prompt":          "synthetic prompt",
	})
	assert.False(t, isCodexSubagentEvent(claude), "only codex events are checked")
}

// A sub-agent event while the parent holds an agent-set waiting_for_input
// (sciontool status ask_user) leaves the parent's status unchanged.
func TestCodexSubagent_AgentSetWaitingKept(t *testing.T) {
	for _, raw := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		t.Run(raw, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			waitClearAskUser(t, h)

			require.NoError(t, h.Handle(codexSubagentEvent(t, raw)))

			info := readAgentInfo(t, statusPath)
			assert.Equal(t, "waiting_for_input", info.Activity,
				"a sub-agent %s must not clear the parent's agent-set waiting_for_input", raw)
			assert.Empty(t, info.ToolName)
		})
	}
}

// Every other status the parent holds (completed, blocked, limits_exceeded)
// is kept too.
func TestCodexSubagent_OtherHeldActivitiesKept(t *testing.T) {
	for _, held := range []state.Activity{state.ActivityCompleted, state.ActivityBlocked, state.ActivityLimitsExceeded} {
		for _, raw := range []string{"UserPromptSubmit", "PreToolUse"} {
			t.Run(string(held)+"/"+raw, func(t *testing.T) {
				statusPath := filepath.Join(t.TempDir(), "agent-info.json")
				h := &StatusHandler{StatusPath: statusPath}
				require.NoError(t, h.UpdateActivity(held, ""))

				require.NoError(t, h.Handle(codexSubagentEvent(t, raw)))

				assert.Equal(t, string(held), readAgentInfo(t, statusPath).Activity)
			})
		}
	}
}

// The parent's own events behave as before: prompt-submit clears any held
// status, tool-start clears waiting_for_input.
func TestCodexSubagent_ParentEventsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		event    func(*testing.T) *hooks.Event
		held     state.Activity
		want     string
		wantTool string
	}{
		{"prompt clears ask_user", func(t *testing.T) *hooks.Event { return codexParentEvent(t, "UserPromptSubmit") }, state.ActivityWaitingForInput, "thinking", ""},
		{"prompt clears completed", func(t *testing.T) *hooks.Event { return codexParentEvent(t, "UserPromptSubmit") }, state.ActivityCompleted, "thinking", ""},
		{"prompt with agent_id equal to session clears ask_user", func(t *testing.T) *hooks.Event {
			return codexEvent(t, "UserPromptSubmit", codexSubagentRootSession)
		}, state.ActivityWaitingForInput, "thinking", ""},
		{"tool-start clears ask_user", func(t *testing.T) *hooks.Event { return codexParentEvent(t, "PreToolUse") }, state.ActivityWaitingForInput, "executing", "Bash"},
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
func TestCodexSubagent_NoHoldUpdatesAsBefore(t *testing.T) {
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

			require.NoError(t, h.Handle(codexSubagentEvent(t, tc.raw)))

			info := readAgentInfo(t, statusPath)
			assert.Equal(t, tc.want, info.Activity)
			assert.Equal(t, tc.wantTool, info.ToolName)
		})
	}
}

// A waiting_for_input that a Notification set is not a status the agent set
// itself, so a sub-agent event clears it, as tool-end and agent-end do.
func TestCodexSubagent_NotificationWaitingCleared(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}
	require.NoError(t, h.setWaitingFromNotification())

	require.NoError(t, h.Handle(codexSubagentEvent(t, "UserPromptSubmit")))

	info := readAgentInfoMap(t, statusPath)
	assert.Equal(t, "thinking", info["activity"])
	assert.NotContains(t, info, activitySourceKey)
}

// Claude events are not checked for agent_id: a claude prompt-submit that
// carries one still clears an agent-set wait, as before.
func TestCodexSubagent_ClaudeUnchanged(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"UserPromptSubmit", "thinking"},
		{"PreToolUse", "executing"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			statusPath := filepath.Join(t.TempDir(), "agent-info.json")
			h := &StatusHandler{StatusPath: statusPath}
			waitClearAskUser(t, h)

			require.NoError(t, h.Handle(waitClearClaudeEvent(t, map[string]interface{}{
				"hook_event_name": tc.raw,
				"session_id":      codexSubagentRootSession,
				"agent_id":        codexSubagentChildID,
				"prompt":          "synthetic prompt",
				"tool_name":       "Bash",
				"tool_input":      map[string]interface{}{"command": "ls"},
			})))

			assert.Equal(t, tc.want, readAgentInfo(t, statusPath).Activity)
		})
	}
}

// The Hub follows the local result: a sub-agent event that the parent's
// held status absorbs sends no Hub update.
func TestCodexSubagent_HubFollowsLocal(t *testing.T) {
	claudePrompt := func(t *testing.T) *hooks.Event {
		return waitClearClaudeEvent(t, map[string]interface{}{
			"hook_event_name": "UserPromptSubmit",
			"session_id":      codexSubagentRootSession,
			"agent_id":        codexSubagentChildID,
			"prompt":          "synthetic prompt",
		})
	}
	for _, tc := range []struct {
		name string
		// held is the parent's activity before the event; "" means
		// working, nothing held.
		held  state.Activity
		event func(*testing.T) *hooks.Event
		// wantStatus is the Hub status sent; "" means no Hub call.
		wantStatus string
		wantLocal  string
	}{
		{"sub-agent prompt with ask_user", state.ActivityWaitingForInput, func(t *testing.T) *hooks.Event { return codexSubagentEvent(t, "UserPromptSubmit") }, "", "waiting_for_input"},
		{"sub-agent tool-start with ask_user", state.ActivityWaitingForInput, func(t *testing.T) *hooks.Event { return codexSubagentEvent(t, "PreToolUse") }, "", "waiting_for_input"},
		{"sub-agent Stop with ask_user", state.ActivityWaitingForInput, func(t *testing.T) *hooks.Event { return codexSubagentEvent(t, "Stop") }, "", "waiting_for_input"},
		{"sub-agent prompt with completed", state.ActivityCompleted, func(t *testing.T) *hooks.Event { return codexSubagentEvent(t, "UserPromptSubmit") }, "", "completed"},
		{"sub-agent prompt with no hold", "", func(t *testing.T) *hooks.Event { return codexSubagentEvent(t, "UserPromptSubmit") }, "thinking", "thinking"},
		{"parent prompt with ask_user", state.ActivityWaitingForInput, func(t *testing.T) *hooks.Event { return codexParentEvent(t, "UserPromptSubmit") }, "thinking", "thinking"},
		{"parent tool-start with ask_user", state.ActivityWaitingForInput, func(t *testing.T) *hooks.Event { return codexParentEvent(t, "PreToolUse") }, "executing", "executing"},
		{"claude prompt with agent_id and ask_user", state.ActivityWaitingForInput, claudePrompt, "thinking", "thinking"},
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

			held := tc.held
			if held == "" {
				held = state.ActivityWorking
			}
			require.NoError(t, statusHandler.UpdateActivity(held, ""))

			event := tc.event(t)
			require.NoError(t, statusHandler.Handle(event))
			require.NoError(t, hubHandler.Handle(event))

			mu.Lock()
			got := append([]string(nil), statuses...)
			mu.Unlock()
			if tc.wantStatus == "" {
				assert.Empty(t, got, "no hub update expected")
			} else {
				assert.Equal(t, []string{tc.wantStatus}, got)
			}
			assert.Equal(t, tc.wantLocal, readAgentInfo(t, statusHandler.StatusPath).Activity)
		})
	}
}
