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
	"database/sql"
	"testing"
)

// openTestMemorySQLite opens a private in-memory SQLite database on the given
// registered driver ("sqlite3" for mattn/go-sqlite3, "sqlite" for
// modernc.org/sqlite) and closes it in t.Cleanup. Callers may still close it
// themselves; closing twice is harmless.
//
// Every pkg/hub test that wants a raw *sql.DB on ":memory:" must use this
// helper instead of sql.Open (TestNoRawMemorySQLiteOpen enforces it).
//
// Why the pool is pinned to one connection: each connection database/sql
// opens to ":memory:" is a separate, empty database. With the default
// unbounded pool, any concurrent use (a handler's background notification
// goroutine reading while the request still holds a connection) makes the
// pool dial a second connection, and its queries fail with "no such table"
// (ptone/scion#2312, ptone/scion#3675). One connection keeps every query on
// the same database, and matches production, where applyDatabasePoolDefaults
// (pkg/config/hub_config.go) forces MaxOpenConns=1 for SQLite. A shared-cache
// named DSN was rejected: it lets several connections write at once, which
// trades "no such table" for SQLITE_LOCKED table-lock errors that the
// busy timeout does not retry.
//
// The cost of one connection: code that holds a *sql.Rows or *sql.Tx and
// issues a second query on the same *sql.DB blocks forever. Production SQLite
// has the same constraint, so such a hang is a real bug, not a test artifact.
func openTestMemorySQLite(t testing.TB, driver string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite (%s): %v", driver, err)
	}
	db.SetMaxOpenConns(1)
	// Never let the only connection be retired: closing it would drop the
	// in-memory database and every table in it.
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
