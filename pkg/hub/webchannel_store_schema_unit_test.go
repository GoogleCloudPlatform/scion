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
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file tests the Postgres web chat store's schema handling without a
// Postgres server. A scripted database/sql driver answers the store's catalog
// and migration-bookkeeping queries and records every statement, so the tests
// can check which schema the store passes and names, and that a failed
// table-existence query surfaces as an error.

var errScriptedExistsQuery = errors.New("scripted: table-existence query failed")

// scriptedPG is the behaviour and statement log shared by every connection
// a scriptedPG-backed *sql.DB opens.
type scriptedPG struct {
	// currentSchema is what SELECT current_schema() returns; nil returns NULL.
	currentSchema *string
	// existsErr, when set, fails the information_schema existence query.
	existsErr error
	// exists is what the existence query returns when existsErr is nil.
	exists bool

	mu        sync.Mutex
	queries   []string
	existArgs [][]driver.NamedValue
}

func (p *scriptedPG) record(q string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries = append(p.queries, strings.Join(strings.Fields(q), " "))
}

// statements returns the recorded statements containing substr.
func (p *scriptedPG) statements(substr string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, q := range p.queries {
		if strings.Contains(q, substr) {
			out = append(out, q)
		}
	}
	return out
}

func (p *scriptedPG) Open(string) (driver.Conn, error) { return &scriptedPGConn{p: p}, nil }

// openScriptedPG returns a *sql.DB backed by p and a pgWebChatStore using it.
func openScriptedPG(t *testing.T, p *scriptedPG) *pgWebChatStore {
	t.Helper()
	db := sql.OpenDB(scriptedPGConnector{p: p})
	t.Cleanup(func() { _ = db.Close() })
	return &pgWebChatStore{db: db}
}

type scriptedPGConnector struct{ p *scriptedPG }

func (c scriptedPGConnector) Connect(context.Context) (driver.Conn, error) { return c.p.Open("") }
func (c scriptedPGConnector) Driver() driver.Driver                        { return c.p }

type scriptedPGConn struct{ p *scriptedPG }

func (c *scriptedPGConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("scripted: Prepare not supported")
}
func (c *scriptedPGConn) Close() error { return nil }
func (c *scriptedPGConn) Begin() (driver.Tx, error) {
	return nil, errors.New("scripted: Begin not supported")
}

func (c *scriptedPGConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.p.record(q)
	switch {
	case strings.Contains(q, pgCurrentSchemaQuery):
		if c.p.currentSchema == nil {
			return &scriptedRows{cols: []string{"current_schema"}, vals: [][]driver.Value{{nil}}}, nil
		}
		return &scriptedRows{cols: []string{"current_schema"}, vals: [][]driver.Value{{*c.p.currentSchema}}}, nil
	case strings.Contains(q, "information_schema.tables"):
		c.p.mu.Lock()
		c.p.existArgs = append(c.p.existArgs, args)
		c.p.mu.Unlock()
		if c.p.existsErr != nil {
			return nil, c.p.existsErr
		}
		return &scriptedRows{cols: []string{"exists"}, vals: [][]driver.Value{{c.p.exists}}}, nil
	case strings.Contains(q, "FROM webchat_migrations"):
		return &scriptedRows{cols: []string{"count"}, vals: [][]driver.Value{{int64(0)}}}, nil
	case strings.Contains(q, "SELECT COUNT(*)"):
		return &scriptedRows{cols: []string{"count"}, vals: [][]driver.Value{{int64(7)}}}, nil
	}
	return nil, errors.New("scripted: unexpected query: " + q)
}

func (c *scriptedPGConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.p.record(q)
	return driver.RowsAffected(0), nil
}

type scriptedRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (r *scriptedRows) Columns() []string { return r.cols }
func (r *scriptedRows) Close() error      { return nil }
func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}

func scriptedSchema(s string) *string { return &s }

// TestPgWebChatStore_TableExistsError_Surfaces checks that a failed
// table-existence query is returned as an error rather than treated as
// "table absent". For the two migrations, treating it as absent would also
// mark the migration completed, so it would never run.
func TestPgWebChatStore_TableExistsError_Surfaces(t *testing.T) {
	newStore := func(t *testing.T) (*pgWebChatStore, *scriptedPG) {
		p := &scriptedPG{currentSchema: scriptedSchema("hub"), existsErr: errScriptedExistsQuery}
		return openScriptedPG(t, p), p
	}

	t.Run("SearchChatMessages", func(t *testing.T) {
		s, _ := newStore(t)
		res, cursor, err := s.SearchChatMessages(context.Background(), ChatSearchFilter{Query: "x", ProjectID: "p"})
		require.ErrorIs(t, err, errScriptedExistsQuery)
		assert.Nil(t, res)
		assert.Empty(t, cursor)
	})

	for name, run := range map[string]func(*pgWebChatStore) error{
		"migrateThreadIDs": func(s *pgWebChatStore) error { return s.migrateThreadIDs(DefaultMigrationBatchSize) },
		"addThreadIDIndex": func(s *pgWebChatStore) error { return s.addThreadIDIndex() },
	} {
		t.Run(name, func(t *testing.T) {
			s, p := newStore(t)
			require.ErrorIs(t, run(s), errScriptedExistsQuery)
			assert.Empty(t, p.statements("INSERT INTO webchat_migrations"),
				"a failed existence check must not mark the migration completed")
		})
	}
}

// TestPgWebChatStore_ThreadMigrations_MessagesAbsent_NotRecorded checks that
// when messages does not exist yet, the thread_id backfill and thread_id
// index migrations do nothing and are not marked completed, so the next Init
// runs them once the Ent migration has created the table.
func TestPgWebChatStore_ThreadMigrations_MessagesAbsent_NotRecorded(t *testing.T) {
	for name, run := range map[string]func(*pgWebChatStore) error{
		"migrateThreadIDs": func(s *pgWebChatStore) error { return s.migrateThreadIDs(DefaultMigrationBatchSize) },
		"addThreadIDIndex": func(s *pgWebChatStore) error { return s.addThreadIDIndex() },
	} {
		t.Run(name, func(t *testing.T) {
			p := &scriptedPG{currentSchema: scriptedSchema("hub"), exists: false}
			s := openScriptedPG(t, p)
			require.NoError(t, run(s))
			require.Len(t, p.existArgs, 1, "the migration must check whether messages exists")
			assert.Empty(t, p.statements("INSERT INTO webchat_migrations"),
				"a migration with no messages table must not be marked completed")
			assert.Empty(t, p.statements("UPDATE"))
			assert.Empty(t, p.statements("CREATE INDEX"))
		})
	}
}

// TestPgWebChatStore_UsesResolvedCurrentSchema checks that the store resolves
// current_schema() once, passes that schema to the table-existence query, and
// names it in statements on the tables it does not create itself.
func TestPgWebChatStore_UsesResolvedCurrentSchema(t *testing.T) {
	ctx := context.Background()
	p := &scriptedPG{currentSchema: scriptedSchema("tenant-a"), exists: true}
	s := openScriptedPG(t, p)

	exists, err := s.tableExists(ctx, "messages")
	require.NoError(t, err)
	assert.True(t, exists)
	require.Len(t, p.existArgs, 1)
	require.Len(t, p.existArgs[0], 2)
	assert.Equal(t, "tenant-a", p.existArgs[0][0].Value, "existence check must filter on the resolved schema")
	assert.Equal(t, "messages", p.existArgs[0][1].Value)

	n, err := s.CountMessages(ctx, "thread-1")
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	n, err = s.CountPendingMessages(ctx, "thread-1")
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	require.NoError(t, s.addThreadIDIndex())

	assert.Len(t, p.statements(`SELECT COUNT(*) FROM "tenant-a"."messages" WHERE`), 2,
		"message counts must name the current schema")
	assert.Len(t, p.statements(`CREATE INDEX IF NOT EXISTS idx_messages_thread_id ON "tenant-a"."messages"`), 1,
		"the thread_id index must be created on the current schema's table")
	assert.Len(t, p.statements(pgCurrentSchemaQuery), 1, "current_schema() is resolved once and cached")
}

// TestPgWebChatStore_NullCurrentSchema_Errors checks that a connection with
// no existing schema on its search_path (current_schema() is NULL) fails
// Init and store statements instead of running unqualified SQL.
func TestPgWebChatStore_NullCurrentSchema_Errors(t *testing.T) {
	p := &scriptedPG{}
	s := openScriptedPG(t, p)

	err := s.Init()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "current_schema() is NULL")
	assert.Empty(t, p.statements("CREATE TABLE"), "Init must fail before creating tables")

	_, err = s.CountMessages(context.Background(), "thread-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "current_schema() is NULL")
	assert.Empty(t, p.statements("FROM messages"))
}
