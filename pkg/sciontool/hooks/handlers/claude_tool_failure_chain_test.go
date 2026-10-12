/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/harnesses"
	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests send a failed Claude Code tool call through a subset of the
// chain the sciontool hook command builds (cmd/sciontool/commands/hook.go
// processHookData): a HarnessProcessor with the built-in dialects, the status
// handler and the hub handler (NewHubHandler against a test server), fed raw
// JSON through ParseRaw and Dispatch. They do not build the other stages
// processHookData adds: the mapping-dialect override, the ready marker, the
// logging, prompt and limits handlers, the hub client budget, the hook
// response and hook telemetry. The dialect name comes from the
// PostToolUseFailure hook command in the bundled claude settings, so a
// settings, dialect, status or hub regression in the failed-tool path shows
// up here.

const (
	chainPreToolUse = `{"hook_event_name":"PreToolUse","session_id":"00000000-0000-0000-0000-000000000000",` +
		`"tool_name":"Bash","tool_input":{"command":"false"},"tool_use_id":"toolu_synthetic"}`
	chainNotification = `{"hook_event_name":"Notification","session_id":"00000000-0000-0000-0000-000000000000",` +
		`"message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`
	chainPostToolUseFailure = `{"hook_event_name":"PostToolUseFailure","session_id":"00000000-0000-0000-0000-000000000000",` +
		`"tool_name":"Bash","tool_input":{"command":"false"},"tool_use_id":"toolu_synthetic",` +
		`"error":"Command exited with non-zero status code 1","is_interrupt":false}`
)

// claudeToolFailureDialect returns the --dialect value of the sciontool hook
// that the bundled claude settings register for PostToolUseFailure.
func claudeToolFailureDialect(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(harnesses.FS, "claude/home/.claude/settings.json")
	require.NoError(t, err)
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &settings))
	for _, group := range settings.Hooks["PostToolUseFailure"] {
		for _, h := range group.Hooks {
			if rest, ok := strings.CutPrefix(h.Command, "sciontool hook --dialect="); ok {
				return strings.Fields(rest)[0]
			}
		}
	}
	t.Fatal("bundled claude settings register no sciontool hook for PostToolUseFailure")
	return ""
}

type toolFailureChain struct {
	processor *hooks.HarnessProcessor
	status    *StatusHandler
	dialect   string

	mu       sync.Mutex
	statuses []string
}

func newToolFailureChain(t *testing.T) *toolFailureChain {
	t.Helper()
	c := &toolFailureChain{dialect: claudeToolFailureDialect(t)}

	home := t.TempDir()
	t.Setenv("HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		s, _ := payload["status"].(string)
		c.mu.Lock()
		c.statuses = append(c.statuses, s)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent-id")

	c.status = NewStatusHandler()
	require.Equal(t, filepath.Join(home, "agent-info.json"), c.status.StatusPath)
	hub := NewHubHandler()
	require.NotNil(t, hub)

	c.processor = hooks.NewHarnessProcessor()
	dialects.RegisterBuiltins(c.processor)
	c.processor.AddHandler(c.status.Handle)
	c.processor.AddHandler(hub.Handle)
	return c
}

// send runs one raw hook payload through the chain and returns the hub
// statuses it posted.
func (c *toolFailureChain) send(t *testing.T, payload string) []string {
	t.Helper()
	c.mu.Lock()
	before := len(c.statuses)
	c.mu.Unlock()

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(payload), &raw))
	event, err := c.processor.ParseRaw(raw, c.dialect)
	require.NoError(t, err)
	require.NoError(t, c.processor.Dispatch(event))

	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.statuses[before:]...)
}

func TestClaudeToolFailureHandlerChain_WaitClearing(t *testing.T) {
	t.Run("notification-set wait clears", func(t *testing.T) {
		c := newToolFailureChain(t)
		c.send(t, chainPreToolUse)
		assert.Equal(t, []string{"waiting_for_input"}, c.send(t, chainNotification))
		info := readAgentInfoMap(t, c.status.StatusPath)
		require.Equal(t, "waiting_for_input", info["activity"])
		require.Equal(t, activitySourceNotification, info[activitySourceKey])

		assert.Equal(t, []string{"working"}, c.send(t, chainPostToolUseFailure),
			"hub must leave waiting_for_input after the failed tool call")
		info = readAgentInfoMap(t, c.status.StatusPath)
		assert.Equal(t, "working", info["activity"], "failed tool call must leave a notification-set waiting_for_input")
		assert.NotContains(t, info, activitySourceKey, "marker must be dropped with the activity it described")
	})

	t.Run("agent-set wait stays", func(t *testing.T) {
		c := newToolFailureChain(t)
		c.send(t, chainPreToolUse)
		// The write sciontool status ask_user makes while the Bash tool runs.
		require.NoError(t, c.status.UpdateActivity(state.ActivityWaitingForInput, ""))

		assert.Empty(t, c.send(t, chainPostToolUseFailure), "no hub update expected for an agent-set wait")
		info := readAgentInfoMap(t, c.status.StatusPath)
		assert.Equal(t, "waiting_for_input", info["activity"], "failed tool call must not clear an agent-set waiting_for_input")
		assert.NotContains(t, info, activitySourceKey)
	})
}
