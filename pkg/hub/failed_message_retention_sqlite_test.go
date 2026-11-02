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

//go:build !no_sqlite

package hub

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestFailedMessageRetention_SameRowsNoOrphansSQLite: with chat retention
// off and the default 7-day window, the handler purges exactly the rows the
// former unbounded DELETE removed (failed, agent recipient, older than the
// window), never pending, deferred, delivered or "user:" recipient rows,
// over several chunks, and leaves no dependent row behind. It opens no
// chat retention run.
func TestFailedMessageRetention_SameRowsNoOrphansSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	// Chat retention off (experiment and enabled); the smallest chunk and
	// no pause, so the 30 rows take two chunks.
	setChatRetentionSettings(t, f.srv, false, `{"sweep":{"chunk_size":25,"pause_ms":0}}`)
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)

	var purge []string
	for i := 0; i < 30; i++ {
		purge = append(purge, f.message(chatRetentionSeed{created: old.Add(time.Duration(i) * time.Second), state: store.MessageDispatchFailed, channel: "web"}))
	}
	keep := []string{
		f.message(chatRetentionSeed{created: old, state: store.MessageDispatchFailed, recipient: "user:alice@test.com"}),
		f.message(chatRetentionSeed{created: old, state: store.MessageDispatchPending}),
		f.message(chatRetentionSeed{created: old, state: store.MessageDispatchDeferred}),
		f.message(chatRetentionSeed{created: old, state: store.MessageDispatchDispatched}),
		f.message(chatRetentionSeed{created: now.Add(-6 * 24 * time.Hour), state: store.MessageDispatchFailed}),
	}

	f.srv.failedMessageRetentionHandler()(f.ctx)

	for _, id := range purge {
		assert.False(t, f.exists(id), "purged %s", id)
	}
	for _, id := range keep {
		assert.True(t, f.exists(id), "kept %s", id)
	}
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
	for _, table := range []string{"message_addressees", "webchat_message_ext", "webchat_mention", "webchat_message_attachment", "webchat_message_star"} {
		assert.Equal(t, len(keep), f.count(`SELECT COUNT(*) FROM `+table), table)
	}
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM chat_retention_runs`), "chat retention off: no run is opened")
	assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM chat_retention_cursors WHERE phase = ?`, store.ChatRetentionPhaseMain), "the main cursor is not written")
	assert.Zero(t, f.count(`SELECT COUNT(*) FROM chat_retention_cursors WHERE phase = ? AND cursor_created IS NOT NULL`, store.ChatRetentionPhaseFailed), "a complete pass leaves the failed cursor at the start")
}

// TestFailedMessageRetention_WindowChangeWithoutRestartSQLite: a PUT of
// messaging.failed_message_retention_days applies on the next run of the
// same handler, with no restart.
func TestFailedMessageRetention_WindowChangeWithoutRestartSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, false, "")
	id := f.message(chatRetentionSeed{created: time.Now().UTC().Add(-2 * 24 * time.Hour), state: store.MessageDispatchFailed})

	handler := f.srv.failedMessageRetentionHandler()
	handler(f.ctx)
	require.True(t, f.exists(id), "within the default 7-day window")

	rr := putMessaging(t, f.srv, `{"failed_message_retention_days": 1}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	handler(f.ctx)
	assert.False(t, f.exists(id), "the 1-day window applies on the next run")
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
}

// TestFailedMessageRetention_AddsToOpenSweepRunSQLite: with chat retention
// on and a sweep run open, the failed purge adds its counts under failed
// in that run; with chat retention on and no run open, it opens none.
func TestFailedMessageRetention_AddsToOpenSweepRunSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
	now := time.Now().UTC()

	// No run open yet: the counts are only logged.
	first := f.message(chatRetentionSeed{created: now.Add(-8 * 24 * time.Hour), state: store.MessageDispatchFailed})
	f.srv.failedMessageRetentionHandler()(f.ctx)
	require.False(t, f.exists(first))
	require.Equal(t, 0, f.count(`SELECT COUNT(*) FROM chat_retention_runs`), "no run is opened by the failed purge")

	// A chat retention tick opens the day's run.
	_ = f.message(chatRetentionSeed{created: now.Add(-40 * 24 * time.Hour)})
	f.srv.chatRetentionHandler()(f.ctx)
	run, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, run)
	require.Equal(t, int64(1), run.Counts[f.project.ID].Main)

	// Within the 30-day window, so only the failed purge deletes it.
	_ = f.message(chatRetentionSeed{created: now.Add(-8*24*time.Hour + time.Minute), state: store.MessageDispatchFailed})
	f.srv.failedMessageRetentionHandler()(f.ctx)

	after, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, run.ID, after.ID, "the open run is updated, not a new one")
	assert.Equal(t, store.ChatRetentionCounts{
		Main: 1, Failed: 1, Addressees: 2, Mentions: 2, Stars: 2, Ext: 2, Attachments: 2, UnlinkedAttachments: 2,
	}, after.Counts[f.project.ID])
	assert.Equal(t, run.Status, after.Status)
	assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM chat_retention_runs`))
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
}

// TestFailedMessageRetention_DrainsBacklogAcrossRunsSQLite (ported from
// upstream's TestFailedMessageRetentionHandler_DrainsBacklogAcrossTicks):
// with a budget that allows one step per run, a backlog larger than a
// chunk drains over several runs, each run resumes from the saved cursor,
// the run that completes the pass saves the cursor at the start, and kept
// rows (failed "user:" rows, failed rows newer than the cutoff) survive. A
// dispatched row created behind every saved position that fails after the
// pass is purged by the next run.
func TestFailedMessageRetention_DrainsBacklogAcrossRunsSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	// A 1 s budget and a 1 s pause: each run takes one step of at most 25
	// rows (the smallest chunk) and stops before the pause.
	setChatRetentionSettings(t, f.srv, false, `{"sweep":{"chunk_size":25,"pause_ms":1000,"tick_budget_seconds":1}}`)
	now := time.Now().UTC()
	old := now.Add(-9 * 24 * time.Hour)
	var purge []string
	for i := 0; i < 60; i++ {
		purge = append(purge, f.message(chatRetentionSeed{created: old.Add(time.Duration(i) * time.Second), state: store.MessageDispatchFailed}))
	}
	keepUser := f.message(chatRetentionSeed{created: old.Add(90 * time.Second), state: store.MessageDispatchFailed, recipient: "user:alice@test.com"})
	late := f.message(chatRetentionSeed{created: old.Add(10500 * time.Millisecond), state: store.MessageDispatchDispatched})
	keepRecent := f.message(chatRetentionSeed{created: now.Add(-time.Hour), state: store.MessageDispatchFailed})
	remaining := func() int {
		n := 0
		for _, id := range purge {
			if f.exists(id) {
				n++
			}
		}
		return n
	}
	cursorID := func() string {
		cur, err := f.crs.GetChatRetentionCursor(f.ctx, store.ChatRetentionPhaseFailed)
		require.NoError(t, err)
		return cur.Position.ID
	}

	var seen []string
	for run, want := range []int{35, 10, 0} {
		f.srv.failedMessageRetentionHandler()(f.ctx)
		assert.Equal(t, want, remaining(), "after run %d", run+1)
		seen = append(seen, cursorID())
	}
	assert.NotEqual(t, seen[0], seen[1], "each run moves the cursor on")
	assert.Equal(t, purge[24], seen[0], "the first run stops after its first chunk")
	assert.Empty(t, seen[2], "the run that completes the pass saves the cursor at the start")

	// The dispatched row fails long after it was created (behind both
	// saved positions); the next run starts at the oldest failed row.
	require.True(t, f.exists(late))
	f.exec(`UPDATE messages SET dispatch_state = ? WHERE id = ?`, store.MessageDispatchFailed, late)
	f.srv.failedMessageRetentionHandler()(f.ctx)
	assert.False(t, f.exists(late), "a row that fails behind the cursor is purged by the next pass")
	assert.True(t, f.exists(keepUser), "failed user: rows are never purged")
	assert.True(t, f.exists(keepRecent), "failed rows inside the retention window survive")
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
}

// TestFailedMessageRetention_DeferredCountsReachRunSQLite: counts of a purge
// that ran while a chat retention tick held the run row are kept, and the
// next chat retention tick adds them to the open run; counts kept again are
// added by the next failed purge, with its own.
func TestFailedMessageRetention_DeferredCountsReachRunSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	setChatRetentionSettings(t, f.srv, true, `{"enabled":true,"messages_retention_days":30,"sweep":{"pause_ms":0}}`)
	now := time.Now().UTC()
	_ = f.message(chatRetentionSeed{created: now.Add(-40 * 24 * time.Hour)})
	f.srv.chatRetentionHandler()(f.ctx) // opens the day's run
	failedCount := func() int64 {
		run, err := f.crs.LatestChatRetentionRun(f.ctx, store.ChatRetentionTriggerSweep, now.Add(-time.Hour))
		require.NoError(t, err)
		require.NotNil(t, run)
		return run.Counts[f.project.ID].Failed
	}

	// A tick holds the guard: the counts are kept, not recorded.
	_ = f.message(chatRetentionSeed{created: now.Add(-8 * 24 * time.Hour), state: store.MessageDispatchFailed})
	f.srv.chatRetention.running.Store(true)
	f.srv.failedMessageRetentionHandler()(f.ctx)
	f.srv.chatRetention.running.Store(false)
	assert.Equal(t, int64(0), failedCount())
	require.Equal(t, int64(1), f.srv.chatRetention.pendingFailed[f.project.ID].Failed)

	// The next tick (skipped by its interval gate) still adds them.
	f.srv.chatRetentionHandler()(f.ctx)
	assert.Equal(t, int64(1), failedCount())
	assert.Nil(t, f.srv.chatRetention.pendingFailed)

	// Kept again; this time the next failed purge adds them with its own.
	_ = f.message(chatRetentionSeed{created: now.Add(-8*24*time.Hour + time.Minute), state: store.MessageDispatchFailed})
	f.srv.chatRetention.running.Store(true)
	f.srv.failedMessageRetentionHandler()(f.ctx)
	f.srv.chatRetention.running.Store(false)
	assert.Equal(t, int64(1), failedCount())
	_ = f.message(chatRetentionSeed{created: now.Add(-8*24*time.Hour + 2*time.Minute), state: store.MessageDispatchFailed})
	f.srv.failedMessageRetentionHandler()(f.ctx)
	assert.Equal(t, int64(3), failedCount())
	assert.Nil(t, f.srv.chatRetention.pendingFailed)
	assert.Equal(t, chatRetentionNoOrphans(), chatRetentionOrphans(t, f.db))
}

// TestFailedMessageRetention_NoStoreSkipsSQLite: without the chat retention
// store (web chat tables not initialized) the handler deletes nothing.
func TestFailedMessageRetention_NoStoreSkipsSQLite(t *testing.T) {
	f := newChatRetentionHubFixture(t)
	id := f.message(chatRetentionSeed{created: time.Now().UTC().Add(-30 * 24 * time.Hour), state: store.MessageDispatchFailed})
	f.srv.SetChatRetentionStore(nil)
	f.srv.failedMessageRetentionHandler()(f.ctx)
	assert.True(t, f.exists(id))
}

// TestAdminMessaging_FailedMessageRetentionDaysAuthz: a hub member cannot
// change failed_message_retention_days; a hub admin can.
func TestAdminMessaging_FailedMessageRetentionDaysAuthz(t *testing.T) {
	srv, _, admin, member := chatRetentionAdminServer(t, false)
	const path = "/api/v1/admin/messaging"
	body := map[string]any{"failed_message_retention_days": 2}

	rec := doRequestAsUser(t, srv, member, http.MethodPut, path, body)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, DefaultFailedMessageRetentionDays, srv.GetOperationalSettings().FailedMessageRetentionDays())

	rec = doRequestAsUser(t, srv, admin, http.MethodPut, path, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 2, srv.GetOperationalSettings().FailedMessageRetentionDays())
}
