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
// the Postgres web chat store's table-existence check is limited to one
// schema, passed as a parameter, and that the schema the store passes (and
// names in its qualified statements) comes from current_schema().
func TestPgTableExistsQuery_FiltersCurrentSchema(t *testing.T) {
	q := strings.Join(strings.Fields(pgTableExistsQuery), " ")
	assert.Contains(t, q, "FROM information_schema.tables")
	assert.Contains(t, q, "table_schema = $1")
	assert.Contains(t, q, "table_name = $2")
	assert.Equal(t, "SELECT current_schema()", pgCurrentSchemaQuery)
}

// TestPgWebChatStore_TableExistenceChecksUseHelper pins, without a database,
// that pgTableExistsQuery is the only information_schema lookup in the
// Postgres web chat store, so no call site can reintroduce an unfiltered
// check.
func TestPgWebChatStore_TableExistenceChecksUseHelper(t *testing.T) {
	src, err := os.ReadFile("webchannel_store_postgres.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`(?i)information_schema|pg_catalog|pg_tables|pg_indexes|pg_class|pg_namespace|to_regclass`)
	assert.Len(t, re.FindAllString(string(src), -1), 1,
		"catalog lookups in webchannel_store_postgres.go must go through pgTableExists")
}

// TestPgWebChatStore_ExternalTablesQualified pins, without a database, that
// every statement in the Postgres web chat store that references messages or
// conversations (tables the Ent migration creates, not the store) names the
// current schema through pgWebChatStore.table. An unqualified reference would
// resolve through the search_path and could reach a same-named table in a
// later schema. SQL keywords in the store are upper case, so lower-case prose
// in comments does not match.
func TestPgWebChatStore_ExternalTablesQualified(t *testing.T) {
	src, err := os.ReadFile("webchannel_store_postgres.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`\b(FROM|JOIN|INTO|UPDATE|ON)\s+(messages|conversations)\b`)
	assert.Empty(t, re.FindAllString(string(src), -1),
		"messages and conversations must be referenced through s.table(ctx, ...) so they resolve in the current schema")
}

// TestWebChatStore_Postgres_MessagesInOtherSchema checks that the store's
// messages and conversations tables are the ones in the current schema, even
// when same-named tables exist in another schema, including one later on the
// search_path. The store names the current schema in its statements on those
// tables, and a failed existence check is an error. Requires
// SCION_TEST_POSTGRES_DSN. Each run uses its own throwaway schemas, and each
// subtest has its own current schema so the subtests can run on their own.
func TestWebChatStore_Postgres_MessagesInOtherSchema(t *testing.T) {
	dsn := requirePostgresDSN(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	other := fmt.Sprintf("wc_schema_other_%d", suffix)

	baseCfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	openWithSearchPath := func(t *testing.T, searchPath string) *sql.DB {
		cfg := baseCfg.Copy()
		cfg.RuntimeParams["search_path"] = searchPath
		db := stdlib.OpenDB(*cfg)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	// createSchema registers the drop before creating, so a failed create
	// later in the test never leaves a schema behind.
	admin := openWithSearchPath(t, other)
	createSchema := func(t *testing.T, schema string) {
		t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE") })
		_, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema)
		require.NoError(t, err)
	}

	const messagesDDL = `CREATE TABLE %s.messages (
    id uuid PRIMARY KEY, created timestamptz NOT NULL, channel text, thread_id text,
    project_id text, msg text, sender text, sender_id text, recipient text, recipient_id text)`
	// The columns the store's conversation statements use (the real table
	// is created by the Ent migration).
	const conversationsDDL = `CREATE TABLE %s.conversations (
    id uuid PRIMARY KEY, project_id uuid, kind text, surface text, external_ref text,
    parent_ref text, display_name text, default_agent_id uuid, drift_state text,
    last_activity_at timestamptz, created_at timestamptz, deleted_at timestamptz)`
	const foreignID = "00000000-0000-4000-8000-000000000001"
	const ownID = "00000000-0000-4000-8000-000000000002"

	createSchema(t, other)
	// A row the thread_id backfill would rewrite if it ran against this table.
	_, err = admin.ExecContext(ctx, fmt.Sprintf(messagesDDL, other))
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `INSERT INTO `+other+`.messages
		(id, created, channel, thread_id, project_id, msg, sender, sender_id, recipient, recipient_id)
		VALUES ($1, now(), 'web', 'agent:agent-a', 'project-1', 'schema-needle foreign', 'agent:agent-a', 'agent-a', 'user:user-1', 'user-1')`,
		foreignID)
	require.NoError(t, err)

	foreignThreadID := func(t *testing.T) string {
		var tid string
		require.NoError(t, admin.QueryRowContext(ctx,
			`SELECT thread_id FROM `+other+`.messages WHERE id = $1`, foreignID).Scan(&tid))
		return tid
	}
	indexCount := func(t *testing.T, schema string) int {
		var n int
		require.NoError(t, admin.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 'idx_messages_thread_id'`,
			schema).Scan(&n))
		return n
	}
	search := func(t *testing.T, db *sql.DB) []ChatSearchResult {
		res, _, err := NewWebChatStore(db, "postgres").SearchChatMessages(ctx, ChatSearchFilter{
			Query: "schema-needle", ProjectID: "project-1",
		})
		require.NoError(t, err)
		return res
	}

	t.Run("other schema not on search_path", func(t *testing.T) {
		own := fmt.Sprintf("wc_schema_own1_%d", suffix)
		createSchema(t, own)
		db := openWithSearchPath(t, own)
		require.NoError(t, NewWebChatStore(db, "postgres").Init())
		assert.Empty(t, search(t, db))
	})

	t.Run("other schema later on search_path", func(t *testing.T) {
		own := fmt.Sprintf("wc_schema_own2_%d", suffix)
		createSchema(t, own)
		db := openWithSearchPath(t, own+","+other)
		require.NoError(t, NewWebChatStore(db, "postgres").Init())

		assert.Equal(t, "agent:agent-a", foreignThreadID(t), "backfill must not touch another schema's messages")
		assert.Zero(t, indexCount(t, other), "thread_id index must not be created in another schema")
		assert.Empty(t, search(t, db), "search must not read another schema's messages")

		// Once the current schema has its own messages table, the store uses it.
		_, err := admin.ExecContext(ctx, fmt.Sprintf(messagesDDL, own))
		require.NoError(t, err)
		_, err = admin.ExecContext(ctx, `INSERT INTO `+own+`.messages
			(id, created, channel, thread_id, project_id, msg, sender, sender_id, recipient, recipient_id)
			VALUES ($1, now(), 'web', 'topic:topic-1', 'project-1', 'schema-needle own', 'user:user-1', 'user-1', 'agent:agent-a', 'agent-a')`,
			ownID)
		require.NoError(t, err)
		res := search(t, db)
		require.Len(t, res, 1)
		assert.Equal(t, ownID, res[0].MessageID)
	})

	// Statements on tables the store does not create (messages and
	// conversations) name the current schema, so with the tables missing
	// there they fail instead of reading or writing the later schema's
	// same-named tables.
	t.Run("statements do not reach another schema later on search_path", func(t *testing.T) {
		own := fmt.Sprintf("wc_schema_own3_%d", suffix)
		createSchema(t, own)
		_, err := admin.ExecContext(ctx, fmt.Sprintf(conversationsDDL, other))
		require.NoError(t, err)
		db := openWithSearchPath(t, own+","+other)
		store := NewWebChatStore(db, "postgres")
		require.NoError(t, store.Init())

		_, err = store.CountMessages(ctx, "agent:agent-a")
		require.Error(t, err, "CountMessages must not count another schema's messages")
		_, err = store.UpdateThreadID(ctx, "agent:agent-a", "moved")
		require.Error(t, err, "UpdateThreadID must not re-key another schema's messages")
		require.Error(t, store.UpdateMessageContent(ctx, foreignID, "rewritten"),
			"UpdateMessageContent must not edit another schema's messages")
		assert.Equal(t, "agent:agent-a", foreignThreadID(t))
		var msg string
		require.NoError(t, admin.QueryRowContext(ctx,
			`SELECT msg FROM `+other+`.messages WHERE id = $1`, foreignID).Scan(&msg))
		assert.Equal(t, "schema-needle foreign", msg)

		err = store.CreateTopic(ctx, WebChatTopic{
			ID: "00000000-0000-4000-8000-000000000010", ProjectID: "00000000-0000-4000-8000-000000000011",
			Name: "t", CreatedBy: "user-1", CreatedAt: time.Now(), LastActivityAt: time.Now(),
		})
		require.Error(t, err, "CreateTopic must not mint its conversation in another schema")
		var convs int
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+other+`.conversations`).Scan(&convs))
		assert.Zero(t, convs, "no conversation may be written to another schema")
	})

	// A failed table-existence query is an error, not "table absent". A
	// cancelled context makes the existence query fail against a real server
	// (the current schema is already resolved and cached by Init).
	t.Run("failed existence query is an error", func(t *testing.T) {
		own := fmt.Sprintf("wc_schema_own4_%d", suffix)
		createSchema(t, own)
		db := openWithSearchPath(t, own)
		store := NewWebChatStore(db, "postgres")
		require.NoError(t, store.Init())

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, _, err := store.SearchChatMessages(cancelled, ChatSearchFilter{Query: "schema-needle", ProjectID: "project-1"})
		require.ErrorIs(t, err, context.Canceled)
	})

	// With no existing schema on the search_path, current_schema() is NULL
	// and Init fails rather than running unqualified statements.
	t.Run("no schema on search_path", func(t *testing.T) {
		db := openWithSearchPath(t, fmt.Sprintf("wc_schema_missing_%d", suffix))
		err := NewWebChatStore(db, "postgres").Init()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "current_schema() is NULL")
	})
}
