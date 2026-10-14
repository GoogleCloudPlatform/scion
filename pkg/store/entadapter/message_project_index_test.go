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

// messageProjectCreatedIDIndex is the index that serves project-scoped
// ListMessages pages (filter on project_id, ORDER BY created, id).
const messageProjectCreatedIDIndex = "message_project_id_created_id"

// messageIndexColumns returns the key columns of the named index on the
// messages table, in index order, or nil when the index does not exist.
func messageIndexColumns(t *testing.T, client *ent.Client, name string) []string {
	t.Helper()
	ctx := context.Background()
	query := "SELECT name FROM pragma_index_info(?) ORDER BY seqno"
	if client.Driver().Dialect() == dialect.Postgres {
		query = `SELECT a.attname
			  FROM pg_class ic
			  JOIN pg_namespace n ON n.oid = ic.relnamespace
			  JOIN pg_index i ON i.indexrelid = ic.oid
			  JOIN pg_class tc ON tc.oid = i.indrelid
			  CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
			  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			 WHERE ic.relname = $1 AND n.nspname = current_schema() AND tc.relname = 'messages'
			 ORDER BY k.ord`
	}
	rows := &entsql.Rows{}
	require.NoError(t, client.Driver().Query(ctx, query, []any{name}, rows))
	var cols []string
	for rows.Next() {
		var col string
		require.NoError(t, rows.Scan(&col))
		cols = append(cols, col)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return cols
}

// TestMessagesProjectCreatedIDIndex_AutoMigrate pins that AutoMigrate
// creates the (project_id, created, id) index on the messages table, with
// its columns in that order, both on a fresh database and on an existing
// database that predates the index. It runs on SQLite and, under -tags
// integration with SCION_TEST_POSTGRES_URL set, on Postgres.
func TestMessagesProjectCreatedIDIndex_AutoMigrate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	want := []string{"project_id", "created", "id"}

	require.Equal(t, want, messageIndexColumns(t, client, messageProjectCreatedIDIndex),
		"a migrated database must have the project keyset index")

	// Upgrade: a database created before the index existed must acquire it
	// through the normal startup AutoMigrate.
	require.NoError(t, client.Driver().Exec(ctx, "DROP INDEX "+messageProjectCreatedIDIndex, []any{}, nil))
	require.Empty(t, messageIndexColumns(t, client, messageProjectCreatedIDIndex))
	require.NoError(t, entc.AutoMigrate(ctx, client))
	require.Equal(t, want, messageIndexColumns(t, client, messageProjectCreatedIDIndex),
		"AutoMigrate must add the project keyset index to an existing database")
}

// TestListMessages_ProjectPageUsesIndexOrder pins that a project-scoped
// ListMessages page (the shape the conversation backfill issues) is served
// by the (project_id, created, id) index in index order: the SQLite plan for
// the exact SQL ListMessages issues, for the first page and for a keyset
// page, must name the index and must not sort with a TEMP B-TREE.
func TestListMessages_ProjectPageUsesIndexOrder(t *testing.T) {
	enttest.SkipOnPostgres(t, "inspects SQLite EXPLAIN QUERY PLAN output")
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	s := NewMessageStore(ent.NewClient(ent.Driver(rec)))

	projectID := uuid.NewString()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		m := newTestMessage(projectID, "agent-plan")
		m.CreatedAt = at.Add(time.Duration(i) * time.Second)
		require.NoError(t, s.CreateMessage(ctx, m))
	}
	filter := store.MessageFilter{ProjectID: projectID}
	first, err := s.ListMessages(ctx, filter, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	require.NotEmpty(t, first.NextCursor)

	cases := []struct {
		name string
		opts store.ListOptions
	}{
		{"first-page", store.ListOptions{Limit: 2}},
		{"cursor-page", store.ListOptions{Limit: 2, Cursor: first.NextCursor}},
		{"cursor-page-asc", store.ListOptions{Limit: 2, Cursor: first.NextCursor, SortDir: "asc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec.mu.Lock()
			rec.queries = nil
			rec.mu.Unlock()
			_, err := s.ListMessages(ctx, filter, tc.opts)
			require.NoError(t, err)

			rec.mu.Lock()
			var q *recordedQuery
			for i := range rec.queries {
				if strings.Contains(rec.queries[i].sql, "ORDER BY") {
					q = &rec.queries[i]
				}
			}
			rec.mu.Unlock()
			require.NotNil(t, q, "ListMessages must issue an ordered SELECT")

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
			assert.Contains(t, plan, messageProjectCreatedIDIndex, "query: %s", q.sql)
			assert.NotContains(t, plan, "TEMP B-TREE FOR ORDER BY", "query: %s", q.sql)
		})
	}
}
