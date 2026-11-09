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

package entc

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// openSearchReaderFixture creates a file database through OpenSQLite (the
// hub's main handle, WAL mode) holding rows web messages, and opens the
// search reader on the same file. The main handle stays open, as in the hub.
func openSearchReaderFixture(t *testing.T, rows int) (main *sql.DB, reader *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	dsn := "file:" + path + "?cache=shared"
	client, err := OpenSQLite(dsn, PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	main = sqliteDB(t, client)
	_, err = main.Exec(`CREATE TABLE messages (
		id TEXT PRIMARY KEY, channel TEXT NOT NULL, msg TEXT NOT NULL, created INTEGER NOT NULL)`)
	require.NoError(t, err)
	_, err = main.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		INSERT INTO messages (id, channel, msg, created)
		SELECT printf('m%08d', i), 'web', hex(randomblob(60)), i FROM n`, rows)
	require.NoError(t, err)

	reader, err = OpenSQLiteSearchReader(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	return main, reader
}

// slowSearchQuery cannot finish within the test's deadline at the fixture
// size: it tests a LIKE pattern that matches nothing against every pair of
// rows. If the driver does not interrupt the statement, the call runs for
// minutes and the test fails on its elapsed-time assertion.
const slowSearchQuery = `SELECT count(*) FROM messages a JOIN messages b ON b.id > a.id
	WHERE a.channel = 'web' AND (a.msg || b.msg) LIKE '%' || ? || '%' ESCAPE '\'`

// runSlowSearch runs slowSearchQuery on conn under a deadline of d and
// returns the elapsed time and error.
func runSlowSearch(conn *sql.Conn, d time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	var n int
	err := conn.QueryRowContext(ctx, slowSearchQuery, "no-such-term").Scan(&n)
	return time.Since(start), err
}

// TestSQLiteSearchReaderCancelsOnDeadline is the Phase 1 gate of the chat
// search design (ptone/scion#7586, design sections 6.2 and 14): a search
// whose context deadline passes while SQLite is stepping the statement must
// stop within about 100 ms with context.DeadlineExceeded, and the connection
// must be usable afterwards. A timeout that only abandons the Go call while
// the statement keeps running would still hold a search connection and pin
// the WAL.
//
// The positive control runs the same query on the same connection with a
// 1 s deadline. It must still be running well past 100 ms, so the 50 ms
// case shows a real interrupt and not a query that was simply fast.
func TestSQLiteSearchReaderCancelsOnDeadline(t *testing.T) {
	_, reader := openSearchReaderFixture(t, 20000)
	reader.SetMaxOpenConns(1) // one connection, so reuse below is the same one

	conn, err := reader.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }() // closed explicitly below; this covers early failures

	// Positive control: the statement outlasts 100 ms by a wide margin and
	// is stopped only by its own (longer) deadline.
	controlElapsed, controlErr := runSlowSearch(conn, time.Second)
	t.Logf("control: deadline 1s, elapsed %v, deadline_exceeded=%t", controlElapsed,
		errors.Is(controlErr, context.DeadlineExceeded))
	require.GreaterOrEqual(t, controlElapsed, 900*time.Millisecond,
		"control: the query finished before 1 s, so the fixture cannot show an interrupt")
	require.ErrorIs(t, controlErr, context.DeadlineExceeded, "control")

	for round := 0; round < 3; round++ {
		elapsed, err := runSlowSearch(conn, 50*time.Millisecond)
		t.Logf("round %d: deadline 50ms, elapsed %v, deadline_exceeded=%t", round, elapsed,
			errors.Is(err, context.DeadlineExceeded))
		require.ErrorIs(t, err, context.DeadlineExceeded, "round %d", round)
		// The deadline expired while the statement was running, not before
		// it started.
		require.GreaterOrEqual(t, elapsed, 45*time.Millisecond, "round %d", round)
		// A loose upper bound for default CI (loaded runners, -race). The
		// design's "about 100 ms" was measured in the gate run (51.5 to 51.9
		// ms) and is held by L2 in the pkg/hub chatsearchbench harness.
		require.Less(t, elapsed, 50*time.Millisecond+time.Second,
			"round %d: the statement was not stopped near the 50 ms deadline", round)

		// The same connection serves the next statement normally.
		var hits int
		require.NoError(t, conn.QueryRowContext(context.Background(),
			`SELECT count(*) FROM messages WHERE msg LIKE '%' || ? || '%' ESCAPE '\'`, "no-such-term").Scan(&hits),
			"round %d", round)
		require.Equal(t, 0, hits)
	}

	// The pool hands the connection out again and it works.
	require.NoError(t, conn.Close())
	var total int
	require.NoError(t, reader.QueryRowContext(context.Background(), `SELECT count(*) FROM messages`).Scan(&total))
	require.Equal(t, 20000, total)
}

// TestSQLiteSearchReaderIsReadOnly checks the search reader's connection
// settings: query_only and busy_timeout on every connection, and a write is
// refused.
func TestSQLiteSearchReaderIsReadOnly(t *testing.T) {
	ctx := context.Background()
	main, reader := openSearchReaderFixture(t, 10)
	reader.SetMaxOpenConns(2)
	for i := 0; i < 2; i++ {
		c, err := reader.Conn(ctx)
		require.NoError(t, err)
		var qo, bt int
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA query_only").Scan(&qo))
		require.Equal(t, 1, qo)
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt))
		require.Equal(t, 2000, bt)
		_, err = c.ExecContext(ctx, `INSERT INTO messages (id, channel, msg, created) VALUES ('w', 'web', 'x', 0)`)
		require.Error(t, err, "connection %d: a write must be refused", i)
		require.NoError(t, c.Close())
	}

	// The reader sees rows the main handle commits.
	_, err := main.Exec(`INSERT INTO messages (id, channel, msg, created) VALUES ('new', 'web', 'fresh', 99)`)
	require.NoError(t, err)
	var n int
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE id = 'new'`).Scan(&n))
	require.Equal(t, 1, n)
}

// TestSQLiteSearchReaderDoesNotBlockWriter checks that a read in progress on
// the search reader does not block a write on the main handle. With a shared
// cache the writer would fail with SQLITE_LOCKED; with a private cache and
// WAL the read keeps its snapshot and the write commits.
func TestSQLiteSearchReaderDoesNotBlockWriter(t *testing.T) {
	ctx := context.Background()
	main, reader := openSearchReaderFixture(t, 100)
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var before int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&before))

	_, err = main.ExecContext(ctx, `INSERT INTO messages (id, channel, msg, created) VALUES ('w', 'web', 'x', 0)`)
	require.NoError(t, err, "a write must not wait for or fail on the search reader's open read")

	var during int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&during))
	require.Equal(t, before, during, "the read keeps its snapshot")
}

func TestSQLiteDSNIsMemory(t *testing.T) {
	for dsn, want := range map[string]bool{
		":memory:":                            true,
		"file::memory:?cache=shared":          true,
		"file:hub?mode=memory&cache=shared":   true,
		"":                                    true,
		"file:/var/lib/scion/hub.db":          false,
		"/var/lib/scion/hub.db?cache=shared":  false,
		"file:/var/lib/scion/hub.db?mode=rwc": false,
	} {
		require.Equal(t, want, SQLiteDSNIsMemory(dsn), dsn)
	}
}

func TestOpenSQLiteSearchReaderRefusesMemoryDSN(t *testing.T) {
	_, err := OpenSQLiteSearchReader("file::memory:?cache=shared")
	require.Error(t, err)
}

func TestWithSQLiteURIParams(t *testing.T) {
	got := withSQLiteURIParams("/data/hub.db?cache=shared&_timezone=UTC",
		map[string]string{"mode": "ro", "cache": "private"})
	require.Equal(t, "file:/data/hub.db?_timezone=UTC&cache=private&mode=ro", got)
}
