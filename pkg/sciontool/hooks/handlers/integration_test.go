/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeFullEventPipeline(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	statusPath := filepath.Join(tmpHome, "agent-info.json")
	statusHandler := &StatusHandler{StatusPath: statusPath}
	loggingHandler := NewLoggingHandler()

	var mu sync.Mutex
	var statusPayloads []map[string]interface{}
	var outboundPayloads []map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		var payload map[string]interface{}
		json.NewDecoder(r.Body).Decode(&payload)

		if _, ok := payload["msg"]; ok {
			outboundPayloads = append(outboundPayloads, payload)
		} else {
			statusPayloads = append(statusPayloads, payload)
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent-id")

	hubHandler := NewHubHandler()
	require.NotNil(t, hubHandler)

	d := dialects.NewOpenCodeDialect()

	events := []map[string]interface{}{
		{"name": "session-start", "data": map[string]interface{}{"session_id": "s1"}},
		{"name": "prompt-submit", "data": map[string]interface{}{"prompt": "Fix the bug"}},
		{"name": "model-start", "data": map[string]interface{}{}},
		{"name": "tool-start", "data": map[string]interface{}{"tool_name": "Bash"}},
		{"name": "tool-end", "data": map[string]interface{}{"tool_name": "Bash", "success": true}},
		{"name": "model-end", "data": map[string]interface{}{}},
		{"name": "session-end", "data": map[string]interface{}{"assistant_text": "Bug fixed"}},
	}

	for _, raw := range events {
		event, err := d.Parse(raw)
		require.NoError(t, err)

		err = statusHandler.Handle(event)
		require.NoError(t, err)

		err = loggingHandler.Handle(event)
		require.NoError(t, err)

		err = hubHandler.Handle(event)
		require.NoError(t, err)
	}

	info := readIntegrationAgentInfoMap(t, statusPath)
	assert.Equal(t, "stopped", info["phase"])

	mu.Lock()
	defer mu.Unlock()

	assert.GreaterOrEqual(t, len(statusPayloads), 1)
	assert.Equal(t, 1, len(outboundPayloads))
	if len(outboundPayloads) == 1 {
		assert.Equal(t, "Bug fixed", outboundPayloads[0]["msg"])
		assert.Equal(t, "assistant-reply", outboundPayloads[0]["type"])
	}
}

func TestOpenCodeStickyBreakdown(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	statusPath := filepath.Join(tmpHome, "agent-info.json")
	statusHandler := &StatusHandler{StatusPath: statusPath}

	d := dialects.NewOpenCodeDialect()

	sessionStart, _ := d.Parse(map[string]interface{}{
		"name": "session-start", "data": map[string]interface{}{},
	})
	require.NoError(t, statusHandler.Handle(sessionStart))

	promptSubmit, _ := d.Parse(map[string]interface{}{
		"name": "prompt-submit", "data": map[string]interface{}{"prompt": "Do something"},
	})
	require.NoError(t, statusHandler.Handle(promptSubmit))

	responseComplete, _ := d.Parse(map[string]interface{}{
		"name": "response-complete", "data": map[string]interface{}{"message": "Done"},
	})
	require.NoError(t, statusHandler.Handle(responseComplete))

	info := readIntegrationAgentInfoMap(t, statusPath)
	assert.Equal(t, "completed", info["activity"])

	modelEnd, _ := d.Parse(map[string]interface{}{
		"name": "model-end", "data": map[string]interface{}{},
	})
	require.NoError(t, statusHandler.Handle(modelEnd))

	info = readIntegrationAgentInfoMap(t, statusPath)
	assert.Equal(t, "completed", info["activity"])

	newPrompt, _ := d.Parse(map[string]interface{}{
		"name": "prompt-submit", "data": map[string]interface{}{"prompt": "New task"},
	})
	require.NoError(t, statusHandler.Handle(newPrompt))

	info = readIntegrationAgentInfoMap(t, statusPath)
	assert.Equal(t, "thinking", info["activity"])
}

func TestOpenCodeStickyWithHub(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	statusPath := filepath.Join(tmpHome, "agent-info.json")
	statusHandler := &StatusHandler{StatusPath: statusPath}

	var mu sync.Mutex
	var statusPayloads []map[string]interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		var payload map[string]interface{}
		json.NewDecoder(r.Body).Decode(&payload)

		if _, ok := payload["msg"]; !ok {
			statusPayloads = append(statusPayloads, payload)
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	scrubHubEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	t.Setenv("SCION_AGENT_ID", "test-agent-id")

	hubHandler := NewHubHandler()
	require.NotNil(t, hubHandler)

	d := dialects.NewOpenCodeDialect()

	events := []map[string]interface{}{
		{"name": "session-start", "data": map[string]interface{}{}},
		{"name": "prompt-submit", "data": map[string]interface{}{"prompt": "Task 1"}},
		{"name": "response-complete", "data": map[string]interface{}{"message": "Done 1"}},
	}

	for _, raw := range events {
		event, err := d.Parse(raw)
		require.NoError(t, err)
		require.NoError(t, statusHandler.Handle(event))
		require.NoError(t, hubHandler.Handle(event))
	}

	info := readIntegrationAgentInfoMap(t, statusPath)
	assert.Equal(t, "completed", info["activity"])

	mu.Lock()
	beforeCount := len(statusPayloads)
	mu.Unlock()

	staleEvents := []map[string]interface{}{
		{"name": "model-start", "data": map[string]interface{}{}},
		{"name": "model-end", "data": map[string]interface{}{}},
		{"name": "tool-start", "data": map[string]interface{}{"tool_name": "Bash"}},
		{"name": "tool-end", "data": map[string]interface{}{"tool_name": "Bash"}},
	}

	for _, raw := range staleEvents {
		event, err := d.Parse(raw)
		require.NoError(t, err)
		require.NoError(t, statusHandler.Handle(event))
		require.NoError(t, hubHandler.Handle(event))
	}

	mu.Lock()
	afterCount := len(statusPayloads)
	mu.Unlock()

	assert.Equal(t, beforeCount, afterCount, "sticky completed should suppress hub updates from stale events")

	info = readIntegrationAgentInfoMap(t, statusPath)
	assert.Equal(t, "completed", info["activity"])

	newPrompt, _ := d.Parse(map[string]interface{}{
		"name": "prompt-submit", "data": map[string]interface{}{"prompt": "Task 2"},
	})
	require.NoError(t, statusHandler.Handle(newPrompt))
	require.NoError(t, hubHandler.Handle(newPrompt))

	info = readIntegrationAgentInfoMap(t, statusPath)
	assert.Equal(t, "thinking", info["activity"])

	mu.Lock()
	finalCount := len(statusPayloads)
	mu.Unlock()
	assert.Greater(t, finalCount, afterCount, "new prompt should clear sticky and send hub update")
}

func readIntegrationAgentInfoMap(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &info))
	return info
}

func TestIsHeartbeatEvent(t *testing.T) {
	tests := []struct {
		name     string
		event    *hooks.Event
		expected bool
	}{
		{
			name:     "nil raw",
			event:    &hooks.Event{Name: hooks.EventModelEnd},
			expected: false,
		},
		{
			name: "heartbeat true",
			event: &hooks.Event{
				Name: hooks.EventModelEnd,
				Data: hooks.EventData{Raw: map[string]interface{}{"_scion_heartbeat": true}},
			},
			expected: true,
		},
		{
			name: "heartbeat false",
			event: &hooks.Event{
				Name: hooks.EventModelEnd,
				Data: hooks.EventData{Raw: map[string]interface{}{"_scion_heartbeat": false}},
			},
			expected: false,
		},
		{
			name: "no heartbeat key",
			event: &hooks.Event{
				Name: hooks.EventModelEnd,
				Data: hooks.EventData{Raw: map[string]interface{}{"other": "value"}},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isHeartbeatEvent(tt.event))
		})
	}
}
