// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handlers

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStampActivitySince covers the activitySince rule (ptone/scion#7855):
// a write that changes the activity restamps it, one that keeps it keeps
// the stamp, and one that removes the activity removes it.
func TestStampActivitySince(t *testing.T) {
	old := "2026-11-14T10:00:00Z"
	now := time.Date(2026, 11, 14, 11, 0, 0, 0, time.FixedZone("x", 3600))
	nowStamp := now.UTC().Format(time.RFC3339Nano)

	tests := []struct {
		name   string
		info   map[string]interface{}
		before string
		want   interface{} // nil means absent
	}{
		{"changed activity restamps", map[string]interface{}{"activity": "executing", activitySinceKey: old}, "working", nowStamp},
		{"same activity keeps the stamp", map[string]interface{}{"activity": "executing", activitySinceKey: old}, "executing", old},
		{"same activity without a stamp gets one", map[string]interface{}{"activity": "executing"}, "executing", nowStamp},
		{"non-string stamp is replaced", map[string]interface{}{"activity": "executing", activitySinceKey: 12.0}, "executing", nowStamp},
		{"removed activity removes the stamp", map[string]interface{}{activitySinceKey: old}, "working", nil},
		{"no activity, no stamp", map[string]interface{}{}, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stampActivitySince(tt.info, tt.before, now)
			got, ok := tt.info[activitySinceKey]
			if tt.want == nil {
				assert.False(t, ok, "stamp should be absent, got %v", got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestStatusHandler_ActivitySinceTracksActivityChanges drives the stamp
// through the handler's real writes and reads it back with
// CurrentActivitySince.
func TestStatusHandler_ActivitySinceTracksActivityChanges(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	act, since := h.CurrentActivitySince()
	assert.Equal(t, state.Activity(""), act)
	assert.True(t, since.IsZero(), "missing file has no stamp")

	start := time.Now()
	require.NoError(t, h.UpdateActivity(state.ActivityExecuting, "Bash"))
	act, first := h.CurrentActivitySince()
	assert.Equal(t, state.ActivityExecuting, act)
	assert.False(t, first.Before(start.Add(-time.Second)), "stamp %v predates the write %v", first, start)
	assert.False(t, first.After(time.Now()), "stamp in the future")

	// A message write and a second executing write keep the stamp.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, h.SetMessage("still going"))
	require.NoError(t, h.UpdateActivity(state.ActivityExecuting, "Read"))
	_, kept := h.CurrentActivitySince()
	assert.True(t, kept.Equal(first), "stamp moved on a write that kept the activity: %v -> %v", first, kept)

	// A change of activity restamps it.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, h.UpdateActivity(state.ActivityWorking, ""))
	act, next := h.CurrentActivitySince()
	assert.Equal(t, state.ActivityWorking, act)
	assert.True(t, next.After(first), "stamp not advanced: %v -> %v", first, next)

	// ReplaceActivityIf goes through the same write path.
	require.NoError(t, h.UpdatePhase(state.PhaseRunning, state.ActivityInitializing, ""))
	_, initSince := h.CurrentActivitySince()
	time.Sleep(5 * time.Millisecond)
	replaced, err := h.ReplaceActivityIf(state.ActivityInitializing, state.ActivityWorking)
	require.NoError(t, err)
	require.True(t, replaced)
	_, replacedSince := h.CurrentActivitySince()
	assert.True(t, replacedSince.After(initSince), "ReplaceActivityIf did not restamp")

	// Clearing the activity removes the stamp.
	require.NoError(t, h.UpdatePhase(state.PhaseStopped, "", ""))
	act, since = h.CurrentActivitySince()
	assert.Equal(t, state.Activity(""), act)
	assert.True(t, since.IsZero())
}

// TestStatusHandler_CurrentActivitySinceIgnoresBadStamp reads a record an
// older sciontool or a workload wrote: an unparsable stamp reads as zero
// (no tool-call liveness), never an error.
func TestStatusHandler_CurrentActivitySinceIgnoresBadStamp(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	require.NoError(t, os.WriteFile(statusPath, []byte(`{"activity":"executing","activitySince":"yesterday"}`), 0o644))
	h := &StatusHandler{StatusPath: statusPath}
	act, since := h.CurrentActivitySince()
	assert.Equal(t, state.ActivityExecuting, act)
	assert.True(t, since.IsZero())
}

// TestStatusHandler_ToolEndThenModelStartPreservesActivitySince verifies that
// tool-end transitions to thinking and a subsequent model-start (as emitted by
// gemini-cli, antigravity, and muse-code) preserves the activitySince stamp.
func TestStatusHandler_ToolEndThenModelStartPreservesActivitySince(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "agent-info.json")
	h := &StatusHandler{StatusPath: statusPath}

	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventToolStart, Data: hooks.EventData{ToolName: "Bash"}}))
	act, execSince := h.CurrentActivitySince()
	require.Equal(t, state.ActivityExecuting, act)

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventToolEnd, Data: hooks.EventData{ToolName: "Bash"}}))
	act, thinkSince := h.CurrentActivitySince()
	assert.Equal(t, state.ActivityThinking, act)
	assert.True(t, thinkSince.After(execSince), "tool-end should advance activitySince: %v -> %v", execSince, thinkSince)

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventModelStart}))
	act, afterModelStart := h.CurrentActivitySince()
	assert.Equal(t, state.ActivityThinking, act)
	assert.True(t, afterModelStart.Equal(thinkSince), "model-start after tool-end should preserve activitySince: %v -> %v", thinkSince, afterModelStart)
}
