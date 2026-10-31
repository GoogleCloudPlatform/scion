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

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messagePendingCreatedIDIndex is the partial index that serves the
// stuck-pending sweep (dispatch_state = 'pending', created < cutoff,
// ORDER BY created, id).
const messagePendingCreatedIDIndex = "message_created_id"

// messageIndexDefinition returns the stored definition of the named index on
// the messages table, or "" when the index does not exist.
func messageIndexDefinition(t *testing.T, client *ent.Client, name string) string {
	t.Helper()
	query := "SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'messages' AND name = ?"
	if client.Driver().Dialect() == dialect.Postgres {
		query = "SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'messages' AND indexname = $1"
	}
	rows := &entsql.Rows{}
	require.NoError(t, client.Driver().Query(context.Background(), query, []any{name}, rows))
	var def string
	for rows.Next() {
		require.NoError(t, rows.Scan(&def))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return def
}

// TestMessagesPendingCreatedIDIndex_AutoMigrate pins that AutoMigrate
// creates the partial (created, id) WHERE dispatch_state = 'pending' index on
// the messages table, on a fresh database and on an existing database that
// predates it. It runs on SQLite and, under -tags integration with
// SCION_TEST_POSTGRES_URL set, on Postgres.
func TestMessagesPendingCreatedIDIndex_AutoMigrate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	want := []string{"created", "id"}

	check := func(t *testing.T) {
		t.Helper()
		require.Equal(t, want, messageIndexColumns(t, client, messagePendingCreatedIDIndex))
		def := messageIndexDefinition(t, client, messagePendingCreatedIDIndex)
		assert.Contains(t, def, "dispatch_state", "the index must be partial on dispatch_state: %s", def)
		assert.Contains(t, def, "'pending'", "the index must be partial on pending rows: %s", def)
	}
	check(t)

	require.NoError(t, client.Driver().Exec(ctx, "DROP INDEX "+messagePendingCreatedIDIndex, []any{}, nil))
	require.Empty(t, messageIndexColumns(t, client, messagePendingCreatedIDIndex))
	require.NoError(t, entc.AutoMigrate(ctx, client))
	check(t)
}

// TestStuckPendingSweep_UsesPendingIndex pins that every stuck-pending sweep
// query, and the sweep's missing-recipient read, is served by the partial
// pending index: the SQLite plan for the
// exact SQL each store method issues must name the index. Without it the
// planner falls back to the created index and checks dispatch_state row by
// row over every message older than the cutoff (ptone/scion#6648).
func TestStuckPendingSweep_UsesPendingIndex(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	client := ent.NewClient(ent.Driver(rec))
	ms := NewMessageStore(client)
	ds := NewBrokerDispatchStore(client)

	projectID := uuid.NewString()
	at := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 5; i++ {
		m := newTestMessage(projectID, uuid.NewString())
		m.CreatedAt = at.Add(time.Duration(i) * time.Second)
		m.DispatchState = store.MessageDispatchPending
		if i%2 == 0 {
			m.DispatchState = store.MessageDispatchDispatched
		}
		require.NoError(t, ms.CreateMessage(ctx, m))
	}
	cutoff := time.Now().UTC().Add(-time.Hour)

	cases := []struct {
		name string
		run  func() error
	}{
		{"count", func() error { _, err := ds.CountStuckPendingMessages(ctx, cutoff); return err }},
		{"list_ids", func() error { _, err := ds.ListStuckPendingMessageIDs(ctx, cutoff, 10); return err }},
		// The expire-and-list select runs inside a transaction, which the
		// recording driver does not see; run its body on the plain client.
		{"expire_and_list", func() error { _, err := expireAndListStuckPending(ctx, client, cutoff, "test"); return err }},
		{"expire", func() error { _, err := ds.ExpireStuckPendingMessages(ctx, cutoff, "test"); return err }},
		{"missing_recipient", func() error {
			_, err := ds.FailPendingMessagesWithMissingRecipient(ctx, "test")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.mu.Lock()
			rec.queries = nil
			rec.mu.Unlock()
			require.NoError(t, tc.run())

			rec.mu.Lock()
			var q *recordedQuery
			for i := range rec.queries {
				if strings.Contains(rec.queries[i].sql, "dispatch_state") && strings.Contains(rec.queries[i].sql, "recipient") {
					q = &rec.queries[i]
					break
				}
			}
			rec.mu.Unlock()
			require.NotNil(t, q, "the sweep must issue a query filtered on dispatch_state and recipient")
			assert.Contains(t, q.sql, "= 'pending'", "dispatch_state must be a literal so the partial index applies")

			rows := &entsql.Rows{}
			require.NoError(t, base.Driver().Query(ctx, "EXPLAIN QUERY PLAN "+q.sql, q.args, rows))
			var plan string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan += detail + "\n"
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			t.Logf("query: %s\nplan:\n%s", q.sql, plan)
			assert.Contains(t, plan, messagePendingCreatedIDIndex, "query: %s", q.sql)
			assert.NotContains(t, plan, "TEMP B-TREE", "query: %s", q.sql)
		})
	}
}
