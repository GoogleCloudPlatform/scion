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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// messageFailedCreatedIDIndex is the partial index that serves the
// failed-message purge's scans (dispatch_state = 'failed', created <
// cutoff, ORDER BY created, id).
const messageFailedCreatedIDIndex = "message_failed_created_id"

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

// TestFailedMessageScan_UsesFailedIndex pins that the failed-message scan,
// from the start and from a cursor, is served by the partial failed index
// in index order: the SQLite plan for the exact SQL SelectFailedMessages
// issues (failedScanQuery) must name the index and must not sort. Without
// it the planner walks the created index and checks dispatch_state row by
// row over every message older than the cutoff.
func TestFailedMessageScan_UsesFailedIndex(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	f := newChatRetentionFixture(t)
	project := uuid.New()
	at := time.Now().UTC().Add(-10 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		state := store.MessageDispatchFailed
		if i%2 == 0 {
			state = store.MessageDispatchDispatched
		}
		_ = f.message(chatRetentionMsg{project: project, created: at.Add(time.Duration(i) * time.Second), state: state})
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	cases := []struct {
		name  string
		after store.ChatRetentionPosition
	}{
		{"from_start", store.ChatRetentionPosition{}},
		{"from_cursor", store.ChatRetentionPosition{Created: at.Add(time.Second), ID: uuid.NewString()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, args := f.st.failedScanQuery(cutoff, tc.after, 1000)
			assert.Contains(t, query, "dispatch_state = 'failed'", "dispatch_state must be a literal so the partial index applies")

			rows, err := f.db.QueryContext(f.ctx, "EXPLAIN QUERY PLAN "+query, args...)
			require.NoError(t, err)
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan.WriteString(detail + "\n")
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			t.Logf("query: %s\nplan:\n%s", query, plan.String())
			assert.Contains(t, plan.String(), messageFailedCreatedIDIndex, "query: %s", query)
			assert.NotContains(t, plan.String(), "TEMP B-TREE", "query: %s", query)

			// The statement the store runs is the one inspected.
			_, err = f.st.SelectFailedMessages(f.ctx, cutoff, tc.after, 1000, 100)
			require.NoError(t, err)
		})
	}
}

// TestChatRetentionStore_FailedDrainsBacklogOverSeveralChunks pins that a
// failed backlog larger than one chunk drains over several scan and purge
// calls, no purge exceeds the chunk size, and rows the purge must keep
// survive: failed rows newer than the cutoff, failed "user:" rows
// interleaved with the backlog, and old rows in other states. Pairs of
// rows share a created timestamp, so the (created, id) tiebreak of the
// cursor is exercised.
func TestChatRetentionStore_FailedDrainsBacklogOverSeveralChunks(t *testing.T) {
	f := newChatRetentionFixture(t)
	project := uuid.New()
	base := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	var purge, keep []string
	for i := 0; i < 11; i++ {
		at := base.Add(time.Duration(i/2) * time.Minute)
		id := f.message(chatRetentionMsg{project: project, created: at, state: store.MessageDispatchFailed, recipient: "agent:coder"})
		f.dependents(id, project)
		purge = append(purge, id)
		if i%4 == 0 {
			keep = append(keep, f.message(chatRetentionMsg{project: project, created: at, state: store.MessageDispatchFailed, recipient: "user:alice"}))
		}
	}
	keep = append(keep,
		f.message(chatRetentionMsg{project: project, created: base, state: store.MessageDispatchDispatched, recipient: "agent:coder"}),
		f.message(chatRetentionMsg{project: project, created: cutoff.Add(time.Hour), state: store.MessageDispatchFailed, recipient: "agent:coder"}),
		f.message(chatRetentionMsg{project: project, created: time.Now().UTC(), state: store.MessageDispatchFailed, recipient: "agent:coder"}),
	)

	drain := func() (deleted int64, calls int) {
		var pos store.ChatRetentionPosition
		for {
			calls++
			require.Less(t, calls, 100, "the walk must finish")
			scan, err := f.st.SelectFailedMessages(f.ctx, cutoff, pos, 1000, 3)
			require.NoError(t, err)
			require.LessOrEqual(t, len(scan.Candidates), 3, "a chunk must not exceed the chunk size")
			ids := candidateIDs(scan.Candidates)
			res, err := f.st.PurgeFailedMessages(f.ctx, ids, nil)
			require.NoError(t, err)
			deleted += res.Total().Failed
			if scan.Exhausted {
				return deleted, calls
			}
			require.NotEqual(t, pos, scan.Next, "an unfinished scan must advance the position")
			pos = scan.Next
		}
	}
	deleted, calls := drain()
	assert.Equal(t, int64(len(purge)), deleted)
	assert.GreaterOrEqual(t, calls, 4, "11 rows in chunks of 3 take at least 4 calls")
	for _, id := range purge {
		assert.Equal(t, 0, f.count(`SELECT COUNT(*) FROM messages WHERE id = ?`, id), "old failed agent row %s is purged", id)
	}
	for _, id := range keep {
		assert.Equal(t, 1, f.count(`SELECT COUNT(*) FROM messages WHERE id = ?`, id), "row %s must survive", id)
	}
	assert.Equal(t, noOrphans(), f.orphans())

	// A second pass finds nothing to delete: the kept "user:" rows are read
	// and passed in one scan.
	deleted, calls = drain()
	assert.Equal(t, int64(0), deleted)
	assert.Equal(t, 1, calls)
}

// TestChatRetentionStore_SelectFailedMessagesNonPositiveBounds: a scan with
// a non-positive limit or scan cap reads nothing and stays where it is.
func TestChatRetentionStore_SelectFailedMessagesNonPositiveBounds(t *testing.T) {
	f := newChatRetentionFixture(t)
	_ = f.message(chatRetentionMsg{project: uuid.New(), created: time.Now().UTC().Add(-30 * 24 * time.Hour), state: store.MessageDispatchFailed})
	pos := store.ChatRetentionPosition{Created: time.Now().UTC().Add(-40 * 24 * time.Hour), ID: uuid.NewString()}
	for _, b := range [][2]int{{0, 10}, {10, 0}, {-1, 10}} {
		got, err := f.st.SelectFailedMessages(f.ctx, time.Now().UTC(), pos, b[0], b[1])
		require.NoError(t, err)
		assert.Empty(t, got.Candidates, "%v", b)
		assert.Equal(t, pos, got.Next, "%v", b)
		assert.False(t, got.Exhausted, "%v", b)
	}
}
