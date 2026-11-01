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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSweepTestServer builds a minimal Server backed by an in-memory SQLite
// store, sufficient for exercising the recurring sweep handlers directly
// (bypassing the scheduler).
func newSweepTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	srv := &Server{
		store:             cs,
		agentLifecycleLog: slog.Default(),
	}
	return srv, cs
}

func TestFailedMessageRetentionHandler_DefaultsWhenUnconfigured(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	// Older than the default 7-day window.
	oldFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: projectID,
		Sender: "user:x", Recipient: "agent:a", Msg: "old",
		DispatchState: store.MessageDispatchFailed,
		CreatedAt:     time.Now().Add(-8 * 24 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, oldFailed))

	// Within the default 7-day window.
	recentFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: projectID,
		Sender: "user:x", Recipient: "agent:b", Msg: "recent",
		DispatchState: store.MessageDispatchFailed,
	}
	require.NoError(t, cs.CreateMessage(ctx, recentFailed))

	// srv.config.FailedMessageRetentionDays is left at its zero value —
	// the handler must fall back to defaultFailedMessageRetentionDays (7).
	srv.failedMessageRetentionHandler()(ctx)

	_, err := cs.GetMessage(ctx, oldFailed.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "message past the default retention window is purged")
	_, err = cs.GetMessage(ctx, recentFailed.ID)
	require.NoError(t, err, "message within the default retention window survives")
}

func TestFailedMessageRetentionHandler_HonorsConfiguredWindow(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	srv.config.FailedMessageRetentionDays = 1
	ctx := context.Background()
	projectID := uuid.NewString()

	old := &store.Message{
		ID: uuid.NewString(), ProjectID: projectID,
		Sender: "user:x", Recipient: "agent:a", Msg: "old",
		DispatchState: store.MessageDispatchFailed,
		CreatedAt:     time.Now().Add(-2 * 24 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, old))

	srv.failedMessageRetentionHandler()(ctx)

	_, err := cs.GetMessage(ctx, old.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "configured 1-day window purges the 2-day-old message")
}

// TestFailedMessageRetentionHandler_DrainsBacklogAcrossTicks pins the
// chunked purge's tick behavior (ptone/scion#6954): with the tick budget
// spent after one chunk, a backlog larger than a chunk drains over several
// ticks, each tick resumes from the saved cursor, the cursor resets when the
// pass completes, and kept rows (failed user: rows, failed rows newer than
// the cutoff) survive.
func TestFailedMessageRetentionHandler_DrainsBacklogAcrossTicks(t *testing.T) {
	prevChunk, prevPause, prevBudget := failedPurgeChunkSize, failedPurgeChunkPause, failedPurgeTickBudget
	failedPurgeChunkSize, failedPurgeChunkPause, failedPurgeTickBudget = 2, 0, 0
	t.Cleanup(func() {
		failedPurgeChunkSize, failedPurgeChunkPause, failedPurgeTickBudget = prevChunk, prevPause, prevBudget
	})

	srv, cs := newSweepTestServer(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	old := time.Now().Add(-8 * 24 * time.Hour)
	create := func(recipient string, created time.Time) string {
		m := &store.Message{
			ID: uuid.NewString(), ProjectID: projectID,
			Sender: "user:x", Recipient: recipient, Msg: "m",
			DispatchState: store.MessageDispatchFailed,
			CreatedAt:     created,
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		return m.ID
	}
	var purge []string
	for i := 0; i < 5; i++ {
		purge = append(purge, create("agent:a", old.Add(time.Duration(i)*time.Minute)))
	}
	keepUser := create("user:alice", old.Add(90*time.Second))
	keepRecent := create("agent:a", time.Now())

	remaining := func() int {
		n := 0
		for _, id := range purge {
			if _, err := cs.GetMessage(ctx, id); err == nil {
				n++
			}
		}
		return n
	}

	// Six candidates (five agent rows, one user row) in chunks of two: three
	// full chunks, then an empty one that completes the pass.
	var late string
	wantRemaining := []int{3, 2, 0}
	for tick, want := range wantRemaining {
		srv.failedMessageRetentionHandler()(ctx)
		assert.Equal(t, want, remaining(), "after tick %d", tick+1)
		assert.False(t, srv.failedPurge.cursor.IsStart(), "an unfinished pass keeps its cursor after tick %d", tick+1)
		if tick == 0 {
			// A row that turns failed mid-pass with a created older than
			// the saved cursor, as a stuck-pending expiry leaves it.
			require.True(t, old.Add(-time.Minute).Before(srv.failedPurge.cursor.Created))
			late = create("agent:a", old.Add(-time.Minute))
		}
	}
	_, err := cs.GetMessage(ctx, late)
	require.NoError(t, err, "a row behind the cursor is not reached in the current pass")

	srv.failedMessageRetentionHandler()(ctx)
	assert.True(t, srv.failedPurge.cursor.IsStart(), "a completed pass resets the cursor")
	_, err = cs.GetMessage(ctx, late)
	require.NoError(t, err, "the tick that completes the pass walks only past the cursor")

	// The next pass starts from the beginning and reaches it.
	srv.failedMessageRetentionHandler()(ctx)
	_, err = cs.GetMessage(ctx, late)
	assert.ErrorIs(t, err, store.ErrNotFound, "the next pass purges the row left behind the cursor")

	_, err = cs.GetMessage(ctx, keepUser)
	require.NoError(t, err, "failed user: rows are never purged")
	_, err = cs.GetMessage(ctx, keepRecent)
	require.NoError(t, err, "failed rows inside the retention window survive")
}

// setFailedPurgeKnobs sets the chunked purge's tuning vars for one test.
func setFailedPurgeKnobs(t *testing.T, chunk int, pause, budget time.Duration) {
	t.Helper()
	prevChunk, prevPause, prevBudget := failedPurgeChunkSize, failedPurgeChunkPause, failedPurgeTickBudget
	failedPurgeChunkSize, failedPurgeChunkPause, failedPurgeTickBudget = chunk, pause, budget
	t.Cleanup(func() {
		failedPurgeChunkSize, failedPurgeChunkPause, failedPurgeTickBudget = prevChunk, prevPause, prevBudget
	})
}

// createOldFailedAgentMessages creates n failed agent: messages past the
// default retention window, one minute apart, and returns their IDs.
func createOldFailedAgentMessages(t *testing.T, cs store.Store, n int) []string {
	t.Helper()
	ctx := context.Background()
	projectID := uuid.NewString()
	old := time.Now().Add(-8 * 24 * time.Hour)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		m := &store.Message{
			ID: uuid.NewString(), ProjectID: projectID,
			Sender: "user:x", Recipient: "agent:a", Msg: "m",
			DispatchState: store.MessageDispatchFailed,
			CreatedAt:     old.Add(time.Duration(i) * time.Minute),
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		ids = append(ids, m.ID)
	}
	return ids
}

// TestPurgeFailedMessages_RunsSeveralChunksInOneTick pins that within the
// tick budget one tick runs chunk after chunk, pausing between them, until
// the pass completes.
func TestPurgeFailedMessages_RunsSeveralChunksInOneTick(t *testing.T) {
	setFailedPurgeKnobs(t, 2, time.Millisecond, 10*time.Second)
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()
	ids := createOldFailedAgentMessages(t, cs, 5)

	cutoff := time.Now().UTC().AddDate(0, 0, -defaultFailedMessageRetentionDays)
	purged, chunks, done, err := srv.purgeFailedMessages(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 5, purged)
	assert.Equal(t, 3, chunks, "five rows in chunks of two: two full chunks and a short last one")
	assert.True(t, done)
	assert.True(t, srv.failedPurge.cursor.IsStart(), "a completed pass resets the cursor")
	for _, id := range ids {
		_, err := cs.GetMessage(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
}

// cancelAfterChunkStore, once its switch is armed, cancels a context after
// each successful purge chunk, so the purge loop sees the cancellation in
// the pause that follows.
type cancelAfterChunkStore struct {
	store.Store
	fault  *storeFaultSwitch
	cancel context.CancelFunc
}

func (s *cancelAfterChunkStore) PurgeFailedMessagesChunk(ctx context.Context, cutoff time.Time, after store.MessagePosition, limit int, maxDuration time.Duration) (store.FailedMessagePurgeChunk, error) {
	res, err := s.Store.PurgeFailedMessagesChunk(ctx, cutoff, after, limit, maxDuration)
	if err == nil && s.fault.Active() {
		s.cancel()
	}
	return res, err
}

// TestPurgeFailedMessages_CancelDuringPauseKeepsCommittedCursor pins that a
// context cancelled during the pause between chunks ends the tick with the
// context's error and leaves the cursor at the last committed chunk.
func TestPurgeFailedMessages_CancelDuringPauseKeepsCommittedCursor(t *testing.T) {
	// A long pause, so the select can only end on the cancelled context: a
	// short one could race its timer against the cancellation.
	setFailedPurgeKnobs(t, 2, time.Minute, 10*time.Minute)
	srv, cs := newSweepTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *cancelAfterChunkStore {
		return &cancelAfterChunkStore{Store: inner, fault: f, cancel: cancel}
	})
	ids := createOldFailedAgentMessages(t, cs, 5)
	fault.Arm()

	cutoff := time.Now().UTC().AddDate(0, 0, -defaultFailedMessageRetentionDays)
	purged, chunks, done, err := srv.purgeFailedMessages(ctx, cutoff)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, purged, "the first chunk committed before the cancellation")
	assert.Equal(t, 1, chunks)
	assert.False(t, done)
	assert.Equal(t, ids[1], srv.failedPurge.cursor.ID, "the cursor is the last row of the committed chunk")

	// The next tick resumes from that cursor. It runs on a fresh context,
	// which the armed wrapper's cancel does not reach.
	failedPurgeChunkPause = 0
	purged, _, done, err = srv.purgeFailedMessages(context.Background(), cutoff)
	require.NoError(t, err)
	assert.Equal(t, 3, purged)
	assert.True(t, done)
}

func TestBrokerMessageSweepHandler_FailsPendingMessagesWithMissingRecipient(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// A pending message addressed to an agent ID that does not exist
	// (simulating a deleted/purged recipient) must be failed early by the
	// sweep, well before the 24h stuck-pending TTL.
	orphan := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:gone", RecipientID: uuid.NewString(), Msg: "hi",
	}
	require.NoError(t, cs.CreateMessage(ctx, orphan))
	assert.Equal(t, store.MessageDispatchPending, orphan.DispatchState)

	srv.brokerMessageSweepHandler()(ctx)

	got, err := cs.GetMessage(ctx, orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	require.NotNil(t, got.DispatchFailureReason)
	assert.Equal(t, missingRecipientFailureReason, *got.DispatchFailureReason)
}

// TestBrokerMessageSweepHandler_ExpiresStuckPendingWithSharedReason pins the
// sweep's TTL-expiry reason string to store.MessageExpiredStuckPendingReason
// — the same constant cmd/boot_non_agent_dispatch_state_backfill.go matches
// on to find and repair rows the sweep mislabeled (nc-promote-busy round 3
// M9). Before this constant existed, the two sites carried independent
// literals that could silently drift; this test fails if sweep.go is ever
// edited back to a hand-written string instead of the shared constant.
func TestBrokerMessageSweepHandler_ExpiresStuckPendingWithSharedReason(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	stuck := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:a", Msg: "hi",
		CreatedAt: time.Now().Add(-25 * time.Hour), // past stuckMessageExpireTTL (24h)
	}
	require.NoError(t, cs.CreateMessage(ctx, stuck))
	assert.Equal(t, store.MessageDispatchPending, stuck.DispatchState)

	srv.brokerMessageSweepHandler()(ctx)

	got, err := cs.GetMessage(ctx, stuck.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	require.NotNil(t, got.DispatchFailureReason)
	assert.Equal(t, store.MessageExpiredStuckPendingReason, *got.DispatchFailureReason)
}

// TestBrokerMessageSweepHandler_LogsExpiredMessageFields checks that the
// TTL-expiry line names the expired row by ID, recipient and pending
// duration, and never carries the message body (ptone/scion#5211).
func TestBrokerMessageSweepHandler_LogsExpiredMessageFields(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	var logs bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	const body = "expired-body-sentinel-5211"
	stuck := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:ttl-target", Msg: body,
		CreatedAt: time.Now().Add(-25 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, stuck))

	srv.brokerMessageSweepHandler()(ctx)

	assert.NotContains(t, logs.String(), body, "no log line carries the message body")

	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(raw, &rec))
		if rec["msg"] == "sweep: expired stuck pending messages" {
			lines = append(lines, rec)
		}
	}
	require.Len(t, lines, 1, "one expiry line per sweep")
	rec := lines[0]
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, float64(1), rec["expired"])
	assert.Equal(t, []any{stuck.ID}, rec["message_ids"])
	assert.Equal(t, []any{"agent:ttl-target"}, rec["recipients"])
	assert.Equal(t, false, rec["message_ids_truncated"])
	durations, ok := rec["pending_durations"].([]any)
	require.True(t, ok)
	require.Len(t, durations, 1)
	d, err := time.ParseDuration(durations[0].(string))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, d, 25*time.Hour)
	assert.Less(t, d, 26*time.Hour)
}

// expiryFailingStore fails the TTL expiry and delegates everything else.
type expiryFailingStore struct {
	store.Store
}

func (expiryFailingStore) ExpireAndListStuckPendingMessages(context.Context, time.Time, string) ([]store.ExpiredPendingMessage, error) {
	return nil, errors.New("expiry failed")
}

// TestBrokerMessageSweepHandler_ExpiryErrorStillFailsMissingRecipient checks
// that a failed TTL expiry does not stop the sweep: pending messages whose
// recipient is gone are still failed (ptone/scion#5211).
func TestBrokerMessageSweepHandler_ExpiryErrorStillFailsMissingRecipient(t *testing.T) {
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	var logs bytes.Buffer
	srv := &Server{
		store:             expiryFailingStore{Store: cs},
		agentLifecycleLog: slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))
	orphan := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:gone", RecipientID: uuid.NewString(), Msg: "hi",
	}
	require.NoError(t, cs.CreateMessage(ctx, orphan))

	srv.brokerMessageSweepHandler()(ctx)

	assert.Contains(t, logs.String(), "sweep: expire stuck pending messages failed", "the expiry error is logged")
	got, err := cs.GetMessage(ctx, orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState, "the missing-recipient step still runs after a failed expiry")
}
