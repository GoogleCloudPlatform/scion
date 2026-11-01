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

package hub

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// seedFailed adds n failed agent messages of project, a minute apart,
// starting 30 days before the clock.
func (h *chatRetentionUnitHarness) seedFailed(n int, project string) {
	base := h.clock.t.Add(-30 * 24 * time.Hour)
	for i := 0; i < n; i++ {
		h.st.add(fmt.Sprintf("%s-f%06d", project, i), base.Add(time.Duration(i)*time.Minute), project, store.MessageDispatchFailed)
	}
}

// TestFailedPurge_DeletesOnlyFailedAgentRows: the walk deletes every failed
// agent message older than the cutoff over several chunks and stops once a
// scan reaches the cutoff; pending, deferred, delivered, "user:" recipient
// and newer rows stay. It counts under Failed, saves its own cursor (not
// the main one) and opens no run.
func TestFailedPurge_DeletesOnlyFailedAgentRows(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedFailed(5, "p1")
	old := h.clock.t.Add(-20 * 24 * time.Hour)
	h.st.add("keep-pending", old, "p1", store.MessageDispatchPending)
	h.st.add("keep-deferred", old, "p1", store.MessageDispatchDeferred)
	h.st.add("keep-dispatched", old, "p1", store.MessageDispatchDispatched)
	h.st.msgs = append(h.st.msgs, fakeChatRetentionMsg{id: "keep-user", created: old, project: "p1", state: store.MessageDispatchFailed, recipient: "user:alice"})
	h.st.add("keep-recent", h.clock.t.Add(-time.Hour), "p1", store.MessageDispatchFailed)
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.ChunkSize = 2 })

	res, err := h.sw.PurgeFailed(context.Background(), h.clock.t.Add(-7*24*time.Hour), cfg)
	require.NoError(t, err)
	assert.True(t, res.Done)
	assert.False(t, res.BudgetExhausted)
	assert.Equal(t, 3, res.Chunks)
	assert.Equal(t, 3, res.Scans)
	assert.Equal(t, []int{2, 2, 2}, res.ChunkSizes)
	assert.Equal(t, store.ChatRetentionCounts{Failed: 5, Mentions: 5}, res.Total())
	assert.Len(t, h.pauses, 2, "a pause after each full chunk")

	var kept []string
	for _, m := range h.st.msgs {
		kept = append(kept, m.id)
	}
	assert.ElementsMatch(t, []string{"keep-pending", "keep-deferred", "keep-dispatched", "keep-user", "keep-recent"}, kept)
	assert.False(t, h.st.hasCursor, "the main cursor is not written")
	require.True(t, h.st.hasFailedCursor, "the failed cursor is saved")
	assert.Equal(t, store.ChatRetentionPhaseFailed, h.st.failedCursor.Phase)
	assert.True(t, h.st.failedCursor.Position.Created.Equal(old), "it rests at the last row scanned")
	assert.Empty(t, h.st.runs, "no run is opened")
	assert.Zero(t, h.st.purgeCalls, "the main purge is not used")
}

// TestFailedPurge_SlowTxShrinksAndBudgetStops: slow transactions halve the
// chunk down to the floor, every transaction runs under the chunk timeout,
// the pause is chat_retention.sweep.pause_ms, and the walk stops before
// the handler deadline leaves no room for a last chunk.
func TestFailedPurge_SlowTxShrinksAndBudgetStops(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedFailed(10000, "p1")
	h.st.purgeCost = func(int) time.Duration { return 300 * time.Millisecond }
	cfg := chatRetentionTestConfig(nil)
	ctx, cancel := context.WithTimeout(context.Background(), failedMessageRetentionTimeout)
	defer cancel()

	res, err := h.sw.PurgeFailed(ctx, h.clock.t.Add(-7*24*time.Hour), cfg)
	require.NoError(t, err)
	assert.True(t, res.BudgetExhausted)
	assert.False(t, res.Done)
	require.GreaterOrEqual(t, len(res.ChunkSizes), 5)
	assert.Equal(t, []int{200, 100, 50, 25, 25}, res.ChunkSizes[:5])
	assert.Equal(t, 300*time.Millisecond, res.MaxTx)
	// The 30 s deadline leaves 30 - 5 (chunk timeout) - 2 (margin) = 23 s
	// of walk: (300 ms tx + 500 ms pause) per chunk.
	assert.InDelta(t, 29, res.Chunks, 1)
	for _, p := range h.pauses {
		assert.Equal(t, 500*time.Millisecond, p)
	}
	for _, d := range h.st.deadlines {
		assert.True(t, d, "each chunk runs under a timeout")
	}
}

// TestFailedPurge_PurgeErrorStops: a failed purge stops the walk with the
// error and keeps the counts of the chunks committed before it.
func TestFailedPurge_PurgeErrorStops(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedFailed(10, "p1")
	h.st.purgeErr = errors.New("disk on fire")
	res, err := h.sw.PurgeFailed(context.Background(), h.clock.t, chatRetentionTestConfig(nil))
	require.ErrorContains(t, err, "disk on fire")
	assert.Zero(t, res.Chunks)
	assert.Len(t, h.st.msgs, 10)
	assert.Zero(t, h.st.failedSaves, "the cursor does not move past rows that were not purged")
}

// TestFailedPurge_ContextDone: when the handler's context ends mid-walk the
// walk stops with its error and reports what it committed.
func TestFailedPurge_ContextDone(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.seedFailed(1000, "p1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.st.afterPurge = func(call int) {
		if call == 2 {
			cancel()
		}
	}
	res, err := h.sw.PurgeFailed(ctx, h.clock.t, chatRetentionTestConfig(nil))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, res.Chunks)
	assert.Equal(t, int64(400), res.Total().Failed)
	require.True(t, h.st.hasFailedCursor, "the cursor is saved although the context is done")
	assert.Equal(t, h.st.msgs[0].created, h.st.failedCursor.Position.Created.Add(time.Minute), "it rests at the last purged row")
}

// TestFailedPurge_SparseRowsCappedScans: failed agent rows scattered among
// many failed "user:" rows (which the failed index also holds) and
// delivered rows are all found by scans that each read at most the scan
// cap; no other row is deleted, and delivered rows are not read.
func TestFailedPurge_SparseRowsCappedScans(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.sw.scanCap = 10
	base := h.clock.t.Add(-30 * 24 * time.Hour)
	var failed []string
	for i := 0; i < 210; i++ {
		id := fmt.Sprintf("m-%04d", i)
		at := base.Add(time.Duration(i) * time.Minute)
		recipient := "user:alice"
		if i%30 == 7 {
			recipient = ""
			failed = append(failed, id)
		}
		h.st.msgs = append(h.st.msgs, fakeChatRetentionMsg{id: id, created: at, project: "p1", state: store.MessageDispatchFailed, recipient: recipient})
		h.st.add(fmt.Sprintf("d-%04d", i), at.Add(30*time.Second), "p1", store.MessageDispatchDispatched)
	}
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.PauseMS = 0 })

	res, err := h.sw.PurgeFailed(context.Background(), h.clock.t.Add(-7*24*time.Hour), cfg)
	require.NoError(t, err)
	assert.True(t, res.Done)
	assert.Equal(t, int64(len(failed)), res.Total().Failed)
	assert.Equal(t, len(failed), res.Chunks)
	assert.Equal(t, 22, res.Scans, "210 rows in scans of at most 10, the last one short")
	for i, n := range h.st.failedScanned {
		assert.LessOrEqual(t, n, 10, "scan %d", i)
	}
	assert.Len(t, h.st.msgs, 420-len(failed))
	for _, m := range h.st.msgs {
		assert.False(t, m.state == store.MessageDispatchFailed && m.recipient == "", "failed agent row %s left", m.id)
	}
}

// TestFailedPurge_ResumesFromCursor: a run stopped by its budget saves its
// position, and the next run scans on from there instead of the oldest
// row.
func TestFailedPurge_ResumesFromCursor(t *testing.T) {
	h := newChatRetentionUnitHarness()
	h.sw.scanCap = 10
	base := h.clock.t.Add(-30 * 24 * time.Hour)
	for i := 0; i < 100; i++ {
		// Failed "user:" rows: read by every scan, never purged.
		h.st.msgs = append(h.st.msgs, fakeChatRetentionMsg{id: fmt.Sprintf("m-%04d", i), created: base.Add(time.Duration(i) * time.Minute), project: "p1", state: store.MessageDispatchFailed, recipient: "user:alice"})
	}
	cutoff := h.clock.t.Add(-7 * 24 * time.Hour)
	// A 1 s budget with a 500 ms pause: two scans.
	cfg := chatRetentionTestConfig(func(c *opsettings.ChatRetentionConfig) { c.TickBudgetSeconds = 1; c.PauseMS = 500 })

	res, err := h.sw.PurgeFailed(context.Background(), cutoff, cfg)
	require.NoError(t, err)
	assert.True(t, res.BudgetExhausted)
	assert.Equal(t, 2, res.Scans)
	require.True(t, h.st.hasFailedCursor)
	assert.Equal(t, "m-0019", h.st.failedCursor.Position.ID)

	// A 1 s budget with a 1 s pause: one scan, from the saved position.
	cfg.PauseMS = 1000
	res, err = h.sw.PurgeFailed(context.Background(), cutoff, cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scans)
	assert.Equal(t, "m-0019", h.st.lastSelect.ID, "the second run starts where the first stopped")
	assert.Equal(t, "m-0029", h.st.failedCursor.Position.ID)
}

// TestFailedPurge_CursorStaysBehindCutoff: the cursor is saved at least
// failedCursorLag behind the cutoff, so a row the walk passed before it
// failed (a pending message expires to failed only after
// stuckMessageExpireTTL) is purged by a later run.
func TestFailedPurge_CursorStaysBehindCutoff(t *testing.T) {
	h := newChatRetentionUnitHarness()
	cutoff := h.clock.t.Add(-7 * 24 * time.Hour)
	h.st.add("late", cutoff.Add(-12*time.Hour), "p1", store.MessageDispatchPending)
	h.st.add("recent-ok", cutoff.Add(-time.Hour), "p1", store.MessageDispatchFailed)
	h.st.msgs[1].recipient = "user:alice" // read by the scan, kept
	cfg := chatRetentionTestConfig(nil)

	res, err := h.sw.PurgeFailed(context.Background(), cutoff, cfg)
	require.NoError(t, err)
	assert.True(t, res.Done)
	assert.Zero(t, res.Total().Failed)
	require.True(t, h.st.hasFailedCursor)
	assert.True(t, h.st.failedCursor.Position.Created.Equal(cutoff.Add(-failedCursorLag)), "clamped to cutoff - lag: %v", h.st.failedCursor.Position.Created)
	assert.Equal(t, chatRetentionNilID, h.st.failedCursor.Position.ID)

	// The pending row expires to failed; the next run, with the same
	// cutoff, scans it again and purges it.
	h.st.msgs[0].state = store.MessageDispatchFailed
	res, err = h.sw.PurgeFailed(context.Background(), cutoff, cfg)
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Total().Failed)
	require.Len(t, h.st.msgs, 1)
	assert.Equal(t, "recent-ok", h.st.msgs[0].id)
}

// TestClampFailedCursor: the start and positions older than cutoff - lag
// are kept; newer ones move back to cutoff - lag.
func TestClampFailedCursor(t *testing.T) {
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	limit := cutoff.Add(-failedCursorLag)
	assert.True(t, clampFailedCursor(store.ChatRetentionPosition{}, cutoff).IsStart())
	old := store.ChatRetentionPosition{Created: limit.Add(-time.Second), ID: "x"}
	assert.Equal(t, old, clampFailedCursor(old, cutoff))
	for _, p := range []store.ChatRetentionPosition{{Created: limit, ID: "x"}, {Created: cutoff.Add(-time.Minute), ID: "y"}} {
		got := clampFailedCursor(p, cutoff)
		assert.True(t, got.Created.Equal(limit))
		assert.Equal(t, chatRetentionNilID, got.ID)
	}
}
