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

package entadapter

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// messageFailedCreatedIDIndex is the partial index that serves the
// failed-message retention purge (dispatch_state = 'failed', created <
// cutoff, ORDER BY created, id).
const messageFailedCreatedIDIndex = "message_failed_created_id"

// purgeAllFailedMessages calls PurgeFailedMessagesChunk with chunks of limit
// rows until the pass is done and returns the total deleted. It fails the
// test if a chunk examines more than limit rows.
func purgeAllFailedMessages(t *testing.T, s *MessageStore, cutoff time.Time, limit int) int {
	t.Helper()
	total, _ := purgeFailedMessagesCountingChunks(t, s, cutoff, limit)
	return total
}

// purgeFailedMessagesCountingChunks is purgeAllFailedMessages that also
// returns the number of chunk calls.
func purgeFailedMessagesCountingChunks(t *testing.T, s *MessageStore, cutoff time.Time, limit int) (deleted, calls int) {
	t.Helper()
	ctx := context.Background()
	var after store.MessagePosition
	for {
		calls++
		require.Less(t, calls, 1000, "the purge must finish")
		res, err := s.PurgeFailedMessagesChunk(ctx, cutoff, after, limit, 0)
		require.NoError(t, err)
		require.LessOrEqual(t, res.Examined, limit, "a chunk transaction must not exceed the chunk size")
		require.LessOrEqual(t, res.Deleted, res.Examined)
		deleted += res.Deleted
		if res.Done {
			return deleted, calls
		}
		require.NotEqual(t, after, res.Next, "an unfinished chunk must advance the cursor")
		after = res.Next
	}
}

func createFailedPurgeMessage(t *testing.T, s *MessageStore, projectID, recipient, state string, created time.Time) string {
	t.Helper()
	m := newTestMessage(projectID, uuid.NewString())
	m.Recipient = recipient
	m.DispatchState = state
	m.CreatedAt = created
	require.NoError(t, s.CreateMessage(context.Background(), m))
	return m.ID
}

// TestPurgeFailedMessagesChunk_DrainsBacklogOverSeveralChunks pins that a
// backlog larger than one chunk drains over several calls, no call examines
// more than the chunk size, and rows the purge must keep survive: failed
// rows newer than the cutoff, failed user: rows interleaved with the
// backlog, and old rows in other states (ptone/scion#6954). Some rows share
// a created timestamp so the (created, id) tiebreak of the cursor is
// exercised.
func TestPurgeFailedMessagesChunk_DrainsBacklogOverSeveralChunks(t *testing.T) {
	s := newTestMessageStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	base := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	var purge, keep []string
	for i := 0; i < 11; i++ {
		// Pairs of rows share a created timestamp.
		at := base.Add(time.Duration(i/2) * time.Minute)
		purge = append(purge, createFailedPurgeMessage(t, s, projectID, "agent:coder", store.MessageDispatchFailed, at))
		if i%4 == 0 {
			keep = append(keep, createFailedPurgeMessage(t, s, projectID, "user:alice", store.MessageDispatchFailed, at))
		}
	}
	keep = append(keep,
		createFailedPurgeMessage(t, s, projectID, "agent:coder", store.MessageDispatchDispatched, base),
		createFailedPurgeMessage(t, s, projectID, "agent:coder", store.MessageDispatchFailed, cutoff.Add(time.Hour)),
		createFailedPurgeMessage(t, s, projectID, "agent:coder", store.MessageDispatchFailed, time.Now().UTC()),
	)

	deleted, calls := purgeFailedMessagesCountingChunks(t, s, cutoff, 3)
	assert.Equal(t, len(purge), deleted)
	// 11 agent rows plus 3 user rows are candidates: at least 5 chunks of 3.
	assert.GreaterOrEqual(t, calls, 5, "the backlog must drain over several chunks")

	for _, id := range purge {
		_, err := s.GetMessage(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "old failed agent row %s is purged", id)
	}
	for _, id := range keep {
		_, err := s.GetMessage(ctx, id)
		assert.NoError(t, err, "row %s must survive the purge", id)
	}

	// A second pass finds nothing to delete and finishes in one chunk.
	deleted, calls = purgeFailedMessagesCountingChunks(t, s, cutoff, 3)
	assert.Equal(t, 0, deleted)
	assert.Equal(t, 2, calls, "the kept user: rows still take one full chunk to walk past")
}

// TestPurgeFailedMessagesChunk_TimeBoundStopsChunkEarly pins that a chunk
// transaction stops after the delete batch in flight once it has run for
// maxDuration, returns a cursor at the last examined row, and that the next
// call resumes from it.
func TestPurgeFailedMessagesChunk_TimeBoundStopsChunkEarly(t *testing.T) {
	prevBatch := failedPurgeDeleteBatch
	failedPurgeDeleteBatch = 2
	t.Cleanup(func() { failedPurgeDeleteBatch = prevBatch })

	s := newTestMessageStore(t)
	// The clock stands still until the chunk runs a DELETE, which advances
	// it by 100ms: the elapsed time the bound sees depends only on the
	// batches the chunk ran, not on how often it reads the clock.
	var mu sync.Mutex
	clock := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	s.client.Message.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if m.Op().Is(ent.OpDelete) {
				mu.Lock()
				clock = clock.Add(100 * time.Millisecond)
				mu.Unlock()
			}
			return next.Mutate(ctx, m)
		})
	})

	ctx := context.Background()
	projectID := uuid.NewString()
	base := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, createFailedPurgeMessage(t, s, projectID, "agent:coder", store.MessageDispatchFailed, base.Add(time.Duration(i)*time.Second)))
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	// The chunk has run 100ms (one DELETE) before the second batch, under
	// the 150ms bound, and 200ms before the third, over it.
	res, err := s.PurgeFailedMessagesChunk(ctx, cutoff, store.MessagePosition{}, 6, 150*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, 4, res.Examined, "the chunk stops after the batch in flight when the bound is reached")
	assert.Equal(t, 4, res.Deleted)
	assert.False(t, res.Done)
	assert.Equal(t, ids[3], res.Next.ID, "the cursor is the last examined row")
	_, err = s.GetMessage(ctx, ids[4])
	require.NoError(t, err, "rows past the early stop are left for the next chunk")

	res, err = s.PurgeFailedMessagesChunk(ctx, cutoff, res.Next, 6, 150*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Deleted)
	assert.True(t, res.Done)
	for _, id := range ids {
		_, err := s.GetMessage(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}

	// A bound already spent before the first batch still makes progress.
	more := createFailedPurgeMessage(t, s, projectID, "agent:coder", store.MessageDispatchFailed, base)
	res, err = s.PurgeFailedMessagesChunk(ctx, cutoff, store.MessagePosition{}, 6, time.Nanosecond)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Deleted, "a chunk always runs its first batch")
	_, err = s.GetMessage(ctx, more)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestPurgeFailedMessagesChunk_RejectsNonPositiveLimit(t *testing.T) {
	s := newTestMessageStore(t)
	_, err := s.PurgeFailedMessagesChunk(context.Background(), time.Now(), store.MessagePosition{}, 0, 0)
	require.Error(t, err)
}

// TestMessagesFailedCreatedIDIndex_AutoMigrate pins that AutoMigrate creates
// the partial (created, id) WHERE dispatch_state = 'failed' index on the
// messages table, on a fresh database and on an existing database that
// predates it, alongside the pending index. It runs on SQLite and, under
// -tags integration with SCION_TEST_POSTGRES_URL set, on Postgres.
func TestMessagesFailedCreatedIDIndex_AutoMigrate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	want := []string{"created", "id"}

	check := func(t *testing.T) {
		t.Helper()
		require.Equal(t, want, messageIndexColumns(t, client, messageFailedCreatedIDIndex))
		def := messageIndexDefinition(t, client, messageFailedCreatedIDIndex)
		assert.Contains(t, def, "dispatch_state", "the index must be partial on dispatch_state: %s", def)
		assert.Contains(t, def, "'failed'", "the index must be partial on failed rows: %s", def)
		pending := messageIndexDefinition(t, client, messagePendingCreatedIDIndex)
		assert.Contains(t, pending, "'pending'", "the pending index is unchanged: %s", pending)
	}
	check(t)

	require.NoError(t, client.Driver().Exec(ctx, "DROP INDEX "+messageFailedCreatedIDIndex, []any{}, nil))
	require.Empty(t, messageIndexColumns(t, client, messageFailedCreatedIDIndex))
	require.NoError(t, entc.AutoMigrate(ctx, client))
	check(t)
}

// TestFailedMessagePurge_UsesFailedIndex pins that the purge's candidate
// query, from the start and from a cursor, is served by the partial failed
// index in index order: the SQLite plan for the exact SQL the store issues
// must name the index and must not sort. Without it the planner walks the
// created index and checks dispatch_state row by row over every message
// older than the cutoff (ptone/scion#6954).
func TestFailedMessagePurge_UsesFailedIndex(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	client := ent.NewClient(ent.Driver(rec))
	ms := NewMessageStore(client)

	projectID := uuid.NewString()
	at := time.Now().UTC().Add(-10 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		state := store.MessageDispatchFailed
		if i%2 == 0 {
			state = store.MessageDispatchDispatched
		}
		createFailedPurgeMessage(t, ms, projectID, "agent:coder", state, at.Add(time.Duration(i)*time.Second))
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	cases := []struct {
		name  string
		after store.MessagePosition
	}{
		{"from_start", store.MessagePosition{}},
		{"from_cursor", store.MessagePosition{Created: at.Add(time.Second), ID: uuid.NewString()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.mu.Lock()
			rec.queries = nil
			rec.mu.Unlock()
			// The chunk runs this select inside a transaction, which the
			// recording driver does not see; run it on the plain client.
			_, err := failedPurgeCandidates(ctx, client, cutoff, tc.after, 500)
			require.NoError(t, err)

			rec.mu.Lock()
			require.Len(t, rec.queries, 1)
			q := rec.queries[0]
			rec.mu.Unlock()
			assert.Contains(t, q.sql, "= 'failed'", "dispatch_state must be a literal so the partial index applies")

			rows := &entsql.Rows{}
			require.NoError(t, base.Driver().Query(ctx, "EXPLAIN QUERY PLAN "+q.sql, q.args, rows))
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan.WriteString(detail + "\n")
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			t.Logf("query: %s\nplan:\n%s", q.sql, plan.String())
			assert.Contains(t, plan.String(), messageFailedCreatedIDIndex, "query: %s", q.sql)
			assert.NotContains(t, plan.String(), "TEMP B-TREE", "query: %s", q.sql)
		})
	}
}
