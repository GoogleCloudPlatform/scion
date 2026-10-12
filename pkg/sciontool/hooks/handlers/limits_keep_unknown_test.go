/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newKeepUnknownLimitsHandler returns a LimitsHandler on limitsPath whose
// limits are high enough that the test's increments never trigger shutdown.
func newKeepUnknownLimitsHandler(tmpDir, limitsPath string) *LimitsHandler {
	return &LimitsHandler{
		maxTurns:        100,
		maxModelCalls:   100,
		limitsPath:      limitsPath,
		triggerFilePath: filepath.Join(tmpDir, "limits-trigger"),
		statusHandler:   &StatusHandler{StatusPath: filepath.Join(tmpDir, "agent-info.json")},
	}
}

// TestLimitsHandler_IncrementKeepsUnknownKeys: a top-level key LimitsState
// does not know survives the read-modify-write of an increment
// (ptone/scion#4679), after the known keys, in sorted order, with its value
// as read.
func TestLimitsHandler_IncrementKeepsUnknownKeys(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")
	orig := `{
  "zeta": 1,
  "turn_count": 0,
  "model_call_count": 0,
  "max_turns": 100,
  "max_model_calls": 100,
  "started_at": "2026-01-02T03:04:05Z",
  "future_key": {"nested": ["a", "<b>"]},
  "alpha": "x"
}`
	require.NoError(t, os.WriteFile(limitsPath, []byte(orig), 0600))

	h := newKeepUnknownLimitsHandler(tmpDir, limitsPath)
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}))
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventModelEnd}))

	got, err := os.ReadFile(limitsPath)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &raw))
	for _, k := range []string{"alpha", "future_key", "zeta"} {
		assert.Contains(t, raw, k, "unknown key %q was dropped by the increment:\n%s", k, got)
	}

	want := `{
  "turn_count": 1,
  "model_call_count": 1,
  "max_turns": 100,
  "max_model_calls": 100,
  "started_at": "2026-01-02T03:04:05Z",
  "alpha": "x",
  "future_key": {
    "nested": [
      "a",
      "<b>"
    ]
  },
  "zeta": 1
}`
	assert.Equal(t, want, string(got), "updated agent-limits.json bytes")
}

// TestLimitsHandler_IncrementKnownOnlyOutputUnchanged: with no unknown keys,
// or a file that is JSON null, the increment writes exactly the plain
// json.MarshalIndent encoding of the struct, as before ptone/scion#4679. A
// key matching a known name case-insensitively is read into the struct by
// json.Unmarshal and so counts as known, not carried as a duplicate.
func TestLimitsHandler_IncrementKnownOnlyOutputUnchanged(t *testing.T) {
	start := LimitsState{TurnCount: 2, ModelCallCount: 3, MaxTurns: 100, MaxModelCalls: 100, StartedAt: "2026-01-02T03:04:05Z"}
	startData, err := json.MarshalIndent(start, "", "  ")
	require.NoError(t, err)

	for _, tc := range []struct {
		name, content string
		base          LimitsState
	}{
		{name: "known keys only", content: string(startData), base: start},
		{name: "json null", content: "null", base: LimitsState{}},
		{name: "case-folded known key", content: `{"TURN_COUNT": 2, "Max_Turns": 100}`, base: LimitsState{TurnCount: 2, MaxTurns: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scrubHubEnv(t)
			tmpDir := t.TempDir()
			limitsPath := filepath.Join(tmpDir, "agent-limits.json")
			require.NoError(t, os.WriteFile(limitsPath, []byte(tc.content), 0600))

			h := newKeepUnknownLimitsHandler(tmpDir, limitsPath)
			require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}))

			want := tc.base
			want.TurnCount++
			wantData, err := json.MarshalIndent(want, "", "  ")
			require.NoError(t, err)
			got, err := os.ReadFile(limitsPath)
			require.NoError(t, err)
			assert.Equal(t, string(wantData), string(got), "known-only agent-limits.json bytes")
		})
	}
}

// TestLimitsHandler_IncrementUnreadableFileUnchanged: a missing, empty or
// non-object (or corrupt) agent-limits.json is still a logged read error
// that leaves the file as it was and fails no hook, as before
// ptone/scion#4679.
func TestLimitsHandler_IncrementUnreadableFileUnchanged(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		scrubHubEnv(t)
		tmpDir := t.TempDir()
		limitsPath := filepath.Join(tmpDir, "agent-limits.json")

		h := newKeepUnknownLimitsHandler(tmpDir, limitsPath)
		require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}))

		_, err := os.Lstat(limitsPath)
		assert.True(t, os.IsNotExist(err), "missing agent-limits.json was created: Lstat err = %v", err)
	})

	for _, tc := range []struct{ name, content string }{
		{name: "empty", content: ""},
		{name: "json array", content: `[{"turn_count": 1, "extra": 2}]`},
		{name: "json string", content: `"turn_count"`},
		{name: "corrupt object", content: `{"turn_count": 1, "extra":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scrubHubEnv(t)
			tmpDir := t.TempDir()
			limitsPath := filepath.Join(tmpDir, "agent-limits.json")
			require.NoError(t, os.WriteFile(limitsPath, []byte(tc.content), 0600))

			h := newKeepUnknownLimitsHandler(tmpDir, limitsPath)
			require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}))

			got, err := os.ReadFile(limitsPath)
			require.NoError(t, err)
			assert.Equal(t, tc.content, string(got), "unreadable agent-limits.json bytes")
		})
	}
}

// TestLimitsHandler_IncrementOverCapKeepsFileReadable: an unknown value
// whose input is under agentLimitsMaxBytes but whose re-indented form is
// over it is dropped, so the file stays the plain encoding and every later
// read (and so limit enforcement) still works.
func TestLimitsHandler_IncrementOverCapKeepsFileReadable(t *testing.T) {
	scrubHubEnv(t)
	tmpDir := t.TempDir()
	limitsPath := filepath.Join(tmpDir, "agent-limits.json")

	// A flat array of 200000 ones: 2 bytes per element compact, 7 bytes
	// ("\n    1,") per element once re-indented under a top-level key.
	big := strings.TrimSuffix(strings.Repeat("1,", 200000), ",")
	orig := `{"turn_count":0,"model_call_count":0,"max_turns":100,"max_model_calls":100,"started_at":"2026-01-02T03:04:05Z","big":[` + big + `]}`
	require.Less(t, len(orig), agentLimitsMaxBytes, "test input must be under the read cap")
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, []byte(orig), "", "  "))
	require.Greater(t, indented.Len(), agentLimitsMaxBytes, "re-indented test input must be over the read cap")
	require.NoError(t, os.WriteFile(limitsPath, []byte(orig), 0600))

	h := newKeepUnknownLimitsHandler(tmpDir, limitsPath)
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentEnd}))

	want, err := json.MarshalIndent(LimitsState{TurnCount: 1, MaxTurns: 100, MaxModelCalls: 100, StartedAt: "2026-01-02T03:04:05Z"}, "", "  ")
	require.NoError(t, err)
	got, err := os.ReadFile(limitsPath)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got), "over-cap agent-limits.json bytes: got %d bytes, want plain %d bytes", len(got), len(want))

	ls, err := h.readLimitsState()
	require.NoError(t, err, "over-cap agent-limits.json readable after increment")
	assert.Equal(t, 1, ls.TurnCount, "turn_count after over-cap increment")
}
