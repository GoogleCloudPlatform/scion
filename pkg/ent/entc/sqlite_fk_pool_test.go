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
	"database/sql/driver"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// sqliteDB returns the *sql.DB behind an Ent client opened by OpenSQLite.
func sqliteDB(t *testing.T, client *ent.Client) *sql.DB {
	t.Helper()
	drv, ok := SQLDriver(client.Driver())
	require.True(t, ok, "ent client driver does not expose a *sql.DB")
	return drv.DB()
}

// requireForeignKeysOn asserts that conn enforces foreign keys: the pragma
// reads 1 and an insert with a dangling reference is refused.
func requireForeignKeysOn(t *testing.T, ctx context.Context, conn *sql.Conn, label string) {
	t.Helper()
	var on int
	require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on), label)
	require.Equal(t, 1, on, "%s: PRAGMA foreign_keys", label)
	_, err := conn.ExecContext(ctx, `INSERT INTO fk_child (parent_id) VALUES ('missing')`)
	require.Error(t, err, "%s: insert with a dangling reference must fail", label)
	require.Contains(t, err.Error(), "FOREIGN KEY constraint failed", label)
}

func openFKPoolTestClient(t *testing.T, dsn string, pool PoolConfig) (*ent.Client, *sql.DB) {
	t.Helper()
	client, err := OpenSQLite(dsn, pool)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	db := sqliteDB(t, client)
	_, err = db.Exec(`CREATE TABLE fk_parent (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE fk_child (id INTEGER PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES fk_parent(id))`)
	require.NoError(t, err)
	return client, db
}

// TestOpenSQLiteForeignKeysOnEveryPooledConnection checks that foreign-key
// enforcement, a per-connection SQLite setting, is on for every connection
// the pool opens, not only the first one.
func TestOpenSQLiteForeignKeysOnEveryPooledConnection(t *testing.T) {
	ctx := context.Background()
	dsns := map[string]string{
		"file":          "file:" + filepath.Join(t.TempDir(), "fk.db"),
		"file-pragma":   "file:" + filepath.Join(t.TempDir(), "fk.db") + "?_pragma=busy_timeout(5000)",
		"shared-memory": "file:" + t.Name() + "?mode=memory&cache=shared",
		// A caller-supplied foreign_keys(0) is removed from the DSN.
		"file-fk-off": "file:" + filepath.Join(t.TempDir(), "fk.db") + "?_pragma=foreign_keys(0)",
	}
	for name, dsn := range dsns {
		t.Run(name, func(t *testing.T) {
			_, db := openFKPoolTestClient(t, dsn, PoolConfig{MaxOpenConns: 4, MaxIdleConns: 4})
			// Hold several connections at once so the pool must open new ones.
			conns := make([]*sql.Conn, 4)
			for i := range conns {
				c, err := db.Conn(ctx)
				require.NoError(t, err)
				conns[i] = c
			}
			for i, c := range conns {
				requireForeignKeysOn(t, ctx, c, "connection "+string(rune('0'+i)))
			}
			for _, c := range conns {
				require.NoError(t, c.Close())
			}
		})
	}
}

// TestOpenSQLiteForeignKeysAfterPoolChurn checks that a connection opened to
// replace a discarded one also enforces foreign keys, with the single-
// connection pool the hub uses for SQLite.
func TestOpenSQLiteForeignKeysAfterPoolChurn(t *testing.T) {
	ctx := context.Background()
	_, db := openFKPoolTestClient(t, "file:"+filepath.Join(t.TempDir(), "fk.db"), PoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
	for i := 0; i < 3; i++ {
		c, err := db.Conn(ctx)
		require.NoError(t, err)
		requireForeignKeysOn(t, ctx, c, "connection generation "+string(rune('0'+i)))
		// Returning ErrBadConn from Raw makes database/sql discard the
		// connection and close c, so the next Conn call dials a fresh one.
		require.ErrorIs(t, c.Raw(func(any) error { return driver.ErrBadConn }), driver.ErrBadConn)
		require.ErrorIs(t, c.Close(), sql.ErrConnDone)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO fk_child (parent_id) VALUES ('missing')`)
	require.Error(t, err, "insert with a dangling reference through the client's pool must fail")
}

// TestOpenSQLiteReadOnlyPragmasAfterPoolChurn checks that every connection
// OpenSQLiteReadOnly hands out, including a replacement for a discarded
// one, enforces foreign keys and refuses writes (query_only).
func TestOpenSQLiteReadOnlyPragmasAfterPoolChurn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fk.db")
	rw, _ := openFKPoolTestClient(t, "file:"+path, PoolConfig{MaxOpenConns: 1})
	require.NoError(t, rw.Close())

	client, err := OpenSQLiteReadOnly("file:" + path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	db := sqliteDB(t, client)
	for i := 0; i < 3; i++ {
		label := "connection generation " + string(rune('0'+i))
		c, err := db.Conn(ctx)
		require.NoError(t, err)
		var fk, qo int
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk), label)
		require.Equal(t, 1, fk, "%s: PRAGMA foreign_keys", label)
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA query_only").Scan(&qo), label)
		require.Equal(t, 1, qo, "%s: PRAGMA query_only", label)
		_, err = c.ExecContext(ctx, `INSERT INTO fk_parent (id) VALUES ('p')`)
		require.Error(t, err, "%s: a write must be refused", label)
		require.ErrorIs(t, c.Raw(func(any) error { return driver.ErrBadConn }), driver.ErrBadConn)
		require.ErrorIs(t, c.Close(), sql.ErrConnDone)
	}
}

func TestWithSQLitePragmas(t *testing.T) {
	tests := []struct {
		name        string
		dsn         string
		wantBase    string
		wantPragmas []string
		wantOther   map[string]string
	}{
		{
			name:        "bare path",
			dsn:         "/data/hub.db",
			wantBase:    "/data/hub.db",
			wantPragmas: []string{"foreign_keys(1)"},
		},
		{
			name:        "file URI with a query",
			dsn:         "file:hub.db?mode=memory&cache=shared",
			wantBase:    "file:hub.db",
			wantPragmas: []string{"foreign_keys(1)"},
			wantOther:   map[string]string{"mode": "memory", "cache": "shared"},
		},
		{
			name:        "other caller pragmas are kept",
			dsn:         "file:hub.db?_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)",
			wantBase:    "file:hub.db",
			wantPragmas: []string{"busy_timeout(5000)", "synchronous(NORMAL)", "foreign_keys(1)"},
		},
		{
			name: "caller foreign_keys pragmas are removed in any spelling",
			dsn: "file:hub.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)&_pragma=foreign_keys(off)" +
				"&_pragma=foreign_keys%3D0&_pragma=FOREIGN_KEYS%20%3D%20OFF&_pragma=main.foreign_keys%3Dfalse" +
				"&_pragma=%22foreign_keys%22(no)&_pragma=foreign_keys(1)",
			wantBase:    "file:hub.db",
			wantPragmas: []string{"busy_timeout(5000)", "foreign_keys(1)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withSQLitePragmas(tc.dsn, "foreign_keys(1)")
			base, rawQuery, found := strings.Cut(got, "?")
			require.True(t, found, "rewritten dsn must carry a query string: %q", got)
			require.Equal(t, tc.wantBase, base)
			values, err := url.ParseQuery(rawQuery)
			require.NoError(t, err)
			require.Equal(t, tc.wantPragmas, values["_pragma"])
			require.Len(t, values, len(tc.wantOther)+1, "unexpected option set: %v", values)
			for k, v := range tc.wantOther {
				require.Equal(t, v, values.Get(k), "option %q", k)
			}
		})
	}

	got := withSQLitePragmas("file:hub.db?_pragma=query_only(0)&_pragma=foreign_keys(off)", "foreign_keys(1)", "query_only(1)")
	_, rawQuery, _ := strings.Cut(got, "?")
	values, err := url.ParseQuery(rawQuery)
	require.NoError(t, err)
	require.Equal(t, []string{"foreign_keys(1)", "query_only(1)"}, values["_pragma"])

	malformed := "file:/data/hub.db?mode=ro&_pragma=foo%zz"
	require.Equal(t, malformed, withSQLitePragmas(malformed, "foreign_keys(1)"),
		"a malformed query must be returned unchanged")
}

// TestOpenSQLiteRemovesCallerForeignKeysOff checks that a caller DSN option
// turning foreign keys off, in any spelling, does not reach any connection.
func TestOpenSQLiteRemovesCallerForeignKeysOff(t *testing.T) {
	ctx := context.Background()
	for name, pragma := range map[string]string{
		"paren-0":   "foreign_keys(0)",
		"paren-off": "foreign_keys(off)",
		"eq-0":      "foreign_keys=0",
		"eq-OFF":    "FOREIGN_KEYS = OFF",
		"schema":    "main.foreign_keys=false",
	} {
		t.Run(name, func(t *testing.T) {
			dsn := "file:" + filepath.Join(t.TempDir(), "fk.db") + "?_pragma=" + url.QueryEscape(pragma)
			_, db := openFKPoolTestClient(t, dsn, PoolConfig{MaxOpenConns: 2, MaxIdleConns: 2})
			conns := make([]*sql.Conn, 2)
			for i := range conns {
				c, err := db.Conn(ctx)
				require.NoError(t, err)
				conns[i] = c
			}
			for i, c := range conns {
				requireForeignKeysOn(t, ctx, c, "connection "+string(rune('0'+i)))
			}
			for _, c := range conns {
				require.NoError(t, c.Close())
			}
		})
	}
}

// TestSQLiteOpenersFailClosed checks that an opener refuses to return a
// database whose connections lack a required pragma. The DSN builder is
// stubbed so the pragma is not set.
func TestSQLiteOpenersFailClosed(t *testing.T) {
	orig := sqliteDSNPragmas
	t.Cleanup(func() { sqliteDSNPragmas = orig })
	path := filepath.Join(t.TempDir(), "plain.db")

	sqliteDSNPragmas = func(dsn string, _ ...string) string { return dsn }
	client, err := OpenSQLite("file:"+path, PoolConfig{})
	require.Error(t, err)
	require.Nil(t, client)
	require.EqualError(t, err, "enabling foreign keys: the connection's foreign_keys pragma is 0 after opening, want 1")

	client, err = OpenSQLiteReadOnly("file:" + path)
	require.Error(t, err)
	require.Nil(t, client)
	require.EqualError(t, err, "enabling foreign keys: the connection's foreign_keys pragma is 0 after opening, want 1")

	sqliteDSNPragmas = func(dsn string, _ ...string) string { return orig(dsn, "foreign_keys(1)") }
	client, err = OpenSQLiteReadOnly("file:" + path)
	require.Error(t, err)
	require.Nil(t, client)
	require.EqualError(t, err, "enabling query_only mode: the connection's query_only pragma is 0 after opening, want 1")
}

func TestSQLitePragmaName(t *testing.T) {
	for in, want := range map[string]string{
		"foreign_keys(1)":             "foreign_keys",
		"FOREIGN_KEYS = OFF":          "foreign_keys",
		" foreign_keys (0)":           "foreign_keys",
		"main.foreign_keys=false":     "foreign_keys",
		"main . \"foreign_keys\"(no)": "foreign_keys",
		"[query_only](1)":             "query_only",
		"busy_timeout(5000)":          "busy_timeout",
		"synchronous":                 "synchronous",
	} {
		require.Equal(t, want, sqlitePragmaName(in), "sqlitePragmaName(%q)", in)
	}
}
