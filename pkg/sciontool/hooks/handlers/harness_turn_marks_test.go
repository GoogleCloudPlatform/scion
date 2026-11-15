/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTempTurnMarkHandler(t *testing.T, now time.Time) *TurnMarkHandler {
	t.Helper()
	dir := t.TempDir()
	return &TurnMarkHandler{
		TurnEventsPath:   filepath.Join(dir, "scion-turn-events"),
		PendingErrorPath: filepath.Join(dir, "scion-last-harness-error.json"),
		Now:              func() time.Time { return now },
	}
}

func TestTurnMarkHandler_TurnStartEventsTouchMarker(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 123, time.UTC)
	for _, name := range []string{hooks.EventPromptSubmit, hooks.EventAgentStart, hooks.EventModelStart, hooks.EventToolStart} {
		t.Run(name, func(t *testing.T) {
			h := newTempTurnMarkHandler(t, now)
			require.NoError(t, h.Handle(&hooks.Event{Name: name}))
			at, ok := ReadLastTurnEventAt(h.TurnEventsPath)
			require.True(t, ok)
			assert.True(t, at.Equal(now), "marker time %v, want %v", at, now)
			_, pending := ReadPendingHarnessError(h.PendingErrorPath)
			assert.False(t, pending)
		})
	}
	for _, name := range []string{hooks.EventSessionStart, hooks.EventToolEnd, hooks.EventNotification, hooks.EventSubagentEnd} {
		t.Run("not "+name, func(t *testing.T) {
			h := newTempTurnMarkHandler(t, now)
			require.NoError(t, h.Handle(&hooks.Event{Name: name}))
			_, ok := ReadLastTurnEventAt(h.TurnEventsPath)
			assert.False(t, ok)
			_, pending := ReadPendingHarnessError(h.PendingErrorPath)
			assert.False(t, pending, "an error-free %s records no pending error", name)
		})
	}
}

// An error-free model-end or agent-end also proves the harness ran a turn:
// OpenCode and pi send no turn-start event for the initial prompt.
func TestTurnMarkHandler_ErrorFreeTurnEndsTouchMarker(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{hooks.EventModelEnd, hooks.EventAgentEnd} {
		t.Run(name, func(t *testing.T) {
			h := newTempTurnMarkHandler(t, now)
			require.NoError(t, h.Handle(&hooks.Event{Name: name}))
			at, ok := ReadLastTurnEventAt(h.TurnEventsPath)
			require.True(t, ok)
			assert.True(t, at.Equal(now))
		})
		t.Run(name+" with an error does not", func(t *testing.T) {
			h := newTempTurnMarkHandler(t, now)
			require.NoError(t, h.Handle(&hooks.Event{Name: name, Data: hooks.EventData{Error: "APIError (status 400)"}}))
			_, ok := ReadLastTurnEventAt(h.TurnEventsPath)
			assert.False(t, ok)
		})
	}
}

func TestTurnMarkHandler_ErroredModelAndToolEndRecordPendingError(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	t.Run("model-end", func(t *testing.T) {
		h := newTempTurnMarkHandler(t, now)
		require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{
			Error: "400 Bad Request\napi_key=hunter2hunter2",
		}}))
		p, ok := ReadPendingHarnessError(h.PendingErrorPath)
		require.True(t, ok)
		assert.True(t, p.At.Equal(now))
		assert.Equal(t, hooks.EventModelEnd, p.Event)
		assert.Contains(t, p.Reason, "model API: 400 Bad Request")
		assert.NotContains(t, p.Reason, "hunter2hunter2")
		_, turn := ReadLastTurnEventAt(h.TurnEventsPath)
		assert.False(t, turn)
	})
	t.Run("tool-end", func(t *testing.T) {
		h := newTempTurnMarkHandler(t, now)
		require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventToolEnd, Data: hooks.EventData{ToolName: "Bash", Error: "exit 1"}}))
		p, ok := ReadPendingHarnessError(h.PendingErrorPath)
		require.True(t, ok)
		assert.Equal(t, "tool Bash: exit 1", p.Reason)
	})
	t.Run("a later error replaces the earlier one", func(t *testing.T) {
		h := newTempTurnMarkHandler(t, now)
		require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Error: "first"}}))
		require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Error: "second"}}))
		p, ok := ReadPendingHarnessError(h.PendingErrorPath)
		require.True(t, ok)
		assert.Equal(t, "model API: second", p.Reason)
	})
	t.Run("does not set harness_error", func(t *testing.T) {
		h := newTempTurnMarkHandler(t, now)
		status := newTempStatusHandler(t)
		writeAgentInfoJSON(t, status, `{"phase":"running","activity":"thinking"}`)
		ev := &hooks.Event{Name: hooks.EventModelEnd, Data: hooks.EventData{Error: "503"}}
		require.NoError(t, status.Handle(ev))
		require.NoError(t, h.Handle(ev))
		assert.NotEqual(t, "harness_error", readAgentInfoMap(t, status.StatusPath)["activity"])
	})
}

func TestTurnMarkHandler_UnwritablePathNeverFails(t *testing.T) {
	h := &TurnMarkHandler{TurnEventsPath: filepath.Join(t.TempDir(), "missing", "dir", "marker")}
	assert.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventToolStart}))
	var nilHandler *TurnMarkHandler
	assert.NoError(t, nilHandler.Handle(&hooks.Event{Name: hooks.EventToolStart}))
}

func TestReadTurnMarks_Malformed(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	require.NoError(t, os.WriteFile(bad, []byte("not a time or json"), 0o644))
	_, ok := ReadLastTurnEventAt(bad)
	assert.False(t, ok)
	_, ok = ReadPendingHarnessError(bad)
	assert.False(t, ok)
	_, ok = ReadPendingHarnessError(filepath.Join(dir, "missing"))
	assert.False(t, ok)
}

func TestStatusHandler_SetHarnessErrorIf(t *testing.T) {
	workingOnly := func(a state.Activity) bool { return a == state.ActivityWorking }
	t.Run("sets when allowed", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, `{"phase":"running","activity":"working","detail":{"message":"Ready"}}`)
		wrote, err := h.SetHarnessErrorIf("model API 400: denied", workingOnly)
		require.NoError(t, err)
		assert.True(t, wrote)
		info := readAgentInfoMap(t, h.StatusPath)
		assert.Equal(t, "harness_error", info["activity"])
		assert.Equal(t, "model API 400: denied", detailMessage(info))
	})
	for name, body := range map[string]string{
		"activity not allowed":           `{"phase":"running","activity":"thinking"}`,
		"hook-set harness_error wins":    `{"phase":"running","activity":"harness_error","detail":{"message":"model API: from hook"}}`,
		"phase not running":              `{"phase":"stopped","activity":"working"}`,
		"terminal activity is preserved": `{"phase":"running","activity":"crashed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, body)
			before, err := os.ReadFile(h.StatusPath)
			require.NoError(t, err)
			wrote, err := h.SetHarnessErrorIf("watchdog reason", func(state.Activity) bool { return name != "activity not allowed" })
			require.NoError(t, err)
			assert.False(t, wrote)
			after, err := os.ReadFile(h.StatusPath)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

func TestHarnessErrorStatusUpdate(t *testing.T) {
	u := HarnessErrorStatusUpdate("model API 401: bad key")
	assert.Equal(t, state.ActivityHarnessError, u.Activity)
	assert.Equal(t, "model API 401: bad key", u.Message)
	assert.NotEmpty(t, u.Status)
}

func TestStatusHandler_RestoreFromHarnessErrorIf(t *testing.T) {
	const reason = "harness did not start the initial task: pane"
	t.Run("own reason is restored", func(t *testing.T) {
		h := newTempStatusHandler(t)
		writeAgentInfoJSON(t, h, `{"phase":"running","activity":"harness_error","detail":{"message":"`+reason+`"}}`)
		restored, err := h.RestoreFromHarnessErrorIf(reason, state.ActivityWorking)
		require.NoError(t, err)
		assert.True(t, restored)
		info := readAgentInfoMap(t, h.StatusPath)
		assert.Equal(t, "working", info["activity"])
		assert.Empty(t, detailMessage(info))
	})
	for name, body := range map[string]string{
		"hook-path harness_error": `{"phase":"running","activity":"harness_error","detail":{"message":"model API: APIError (status 400)"}}`,
		"already cleared":         `{"phase":"running","activity":"executing","detail":{"message":"` + reason + `"}}`,
	} {
		t.Run(name+" is left alone", func(t *testing.T) {
			h := newTempStatusHandler(t)
			writeAgentInfoJSON(t, h, body)
			before, err := os.ReadFile(h.StatusPath)
			require.NoError(t, err)
			restored, err := h.RestoreFromHarnessErrorIf(reason, state.ActivityWorking)
			require.NoError(t, err)
			assert.False(t, restored)
			after, err := os.ReadFile(h.StatusPath)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}
