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
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPgTableExistsQuery_FiltersCurrentSchema pins, without a database, that
// the Postgres web chat store's table-existence check is limited to the
// current schema.
func TestPgTableExistsQuery_FiltersCurrentSchema(t *testing.T) {
	q := strings.Join(strings.Fields(pgTableExistsQuery), " ")
	assert.Contains(t, q, "FROM information_schema.tables")
	assert.Contains(t, q, "table_schema = current_schema()")
	assert.Contains(t, q, "table_name = $1")
}

// TestPgWebChatStore_TableExistenceChecksUseHelper pins, without a database,
// that pgTableExistsQuery is the only information_schema lookup in the
// Postgres web chat store, so no call site can reintroduce an unfiltered
// check.
func TestPgWebChatStore_TableExistenceChecksUseHelper(t *testing.T) {
	src, err := os.ReadFile("webchannel_store_postgres.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`(?i)information_schema|pg_catalog|pg_tables|to_regclass`)
	assert.Len(t, re.FindAllString(string(src), -1), 1,
		"catalog lookups in webchannel_store_postgres.go must go through pgTableExists")
}

// TestWebChatStore_Postgres_MessagesInOtherSchema checks that a messages
// table in another schema does not count as the store's own. The store's
// queries use unqualified names that resolve in the current schema, so only
// a table there counts. Requires SCION_TEST_POSTGRES_DSN; each run uses its
// own throwaway schemas.
func TestWebChatStore_Postgres_MessagesInOtherSchema(t *testing.T) {
	dsn := requirePostgresDSN(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	own := fmt.Sprintf("wc_schema_own_%d", suffix)
	other := fmt.Sprintf("wc_schema_other_%d", suffix)

	baseCfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	openWithSearchPath := func(searchPath string) *sql.DB {
		cfg := baseCfg.Copy()
		cfg.RuntimeParams["search_path"] = searchPath
		db := stdlib.OpenDB(*cfg)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	admin := openWithSearchPath(own)
	for _, s := range []string{own, other} {
		_, err := admin.ExecContext(ctx, "CREATE SCHEMA "+s)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA " + own + " CASCADE")
		_, _ = admin.Exec("DROP SCHEMA " + other + " CASCADE")
	})

	const messagesDDL = `CREATE TABLE %s.messages (
    id uuid PRIMARY KEY, created timestamptz NOT NULL, channel text, thread_id text,
    project_id text, msg text, sender text, sender_id text, recipient text, recipient_id text)`
	const foreignID = "00000000-0000-4000-8000-000000000001"
	const ownID = "00000000-0000-4000-8000-000000000002"
	// A row the thread_id backfill would rewrite if it ran against this table.
	_, err = admin.ExecContext(ctx, fmt.Sprintf(messagesDDL, other))
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `INSERT INTO `+other+`.messages
		(id, created, channel, thread_id, project_id, msg, sender, sender_id, recipient, recipient_id)
		VALUES ($1, now(), 'web', 'agent:agent-a', 'project-1', 'schema-needle foreign', 'agent:agent-a', 'agent-a', 'user:user-1', 'user-1')`,
		foreignID)
	require.NoError(t, err)

	foreignThreadID := func() string {
		var tid string
		require.NoError(t, admin.QueryRowContext(ctx,
			`SELECT thread_id FROM `+other+`.messages WHERE id = $1`, foreignID).Scan(&tid))
		return tid
	}
	indexCount := func(schema string) int {
		var n int
		require.NoError(t, admin.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 'idx_messages_thread_id'`,
			schema).Scan(&n))
		return n
	}
	search := func(db *sql.DB) []ChatSearchResult {
		res, _, err := NewWebChatStore(db, "postgres").SearchChatMessages(ctx, ChatSearchFilter{
			Query: "schema-needle", ProjectID: "project-1",
		})
		require.NoError(t, err)
		return res
	}

	t.Run("other schema not on search_path", func(t *testing.T) {
		db := openWithSearchPath(own)
		require.NoError(t, NewWebChatStore(db, "postgres").Init())
		assert.Empty(t, search(db))
	})

	t.Run("other schema later on search_path", func(t *testing.T) {
		db := openWithSearchPath(own + "," + other)
		// Init again in the same schema is a no-op for the marked
		// migrations; reset them so this subtest exercises the checks.
		_, err := db.ExecContext(ctx, `DELETE FROM webchat_migrations WHERE name IN ('thread_id_backfill', 'thread_id_index')`)
		require.NoError(t, err)
		require.NoError(t, NewWebChatStore(db, "postgres").Init())

		assert.Equal(t, "agent:agent-a", foreignThreadID(), "backfill must not touch another schema's messages")
		assert.Zero(t, indexCount(other), "thread_id index must not be created in another schema")
		assert.Empty(t, search(db), "search must not read another schema's messages")

		// Once the current schema has its own messages table, the store uses it.
		_, err = admin.ExecContext(ctx, fmt.Sprintf(messagesDDL, own))
		require.NoError(t, err)
		_, err = admin.ExecContext(ctx, `INSERT INTO `+own+`.messages
			(id, created, channel, thread_id, project_id, msg, sender, sender_id, recipient, recipient_id)
			VALUES ($1, now(), 'web', 'topic:topic-1', 'project-1', 'schema-needle own', 'user:user-1', 'user-1', 'agent:agent-a', 'agent-a')`,
			ownID)
		require.NoError(t, err)
		res := search(db)
		require.Len(t, res, 1)
		assert.Equal(t, ownID, res[0].MessageID)
	})
}
