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

package entadapter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestValidateRelayRole(t *testing.T) {
	assert.NoError(t, ValidateRelayRole(""))
	assert.NoError(t, ValidateRelayRole("tfha-gke-h3-relay"))
	assert.NoError(t, ValidateRelayRole(strings.Repeat("a", 63)))

	for _, bad := range []struct {
		name string
		role string
		want string
	}{
		{"leading space", " tfha-gke-h3-relay", "leading or trailing whitespace"},
		{"trailing newline", "tfha-gke-h3-relay\n", "leading or trailing whitespace"},
		{"whitespace only", "   ", "leading or trailing whitespace"},
		{"too long", strings.Repeat("a", 64), "63-byte identifier limit"},
		{"nul byte", "relay\x00role", "NUL or control byte"},
		{"control char", "relay\x1frole", "NUL or control byte"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			err := ValidateRelayRole(bad.role)
			require.Error(t, err)
			assert.Contains(t, err.Error(), bad.want)
		})
	}
}

func TestRelayRoleGrantStatements(t *testing.T) {
	t.Run("connect and usage already present", func(t *testing.T) {
		stmts := relayRoleGrantStatements("tfha-gke-h3-relay", "tfha_gke_h3", "public", true, true)
		assert.Equal(t, []string{
			`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "public"."relay_instances", "public"."conduit_sessions" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT, INSERT, UPDATE ON TABLE "public"."conduit_principal_epochs" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT ON TABLE "public"."hub_settings" TO "tfha-gke-h3-relay"`,
		}, stmts)
		for _, s := range stmts {
			assert.NotContains(t, s, "ALTER DEFAULT PRIVILEGES",
				"blanket default privileges would expose future hub-only tables to the relay role")
		}
	})

	t.Run("connect and usage missing", func(t *testing.T) {
		stmts := relayRoleGrantStatements("tfha-gke-h3-relay", "tfha_gke_h3", "custom_schema", false, false)
		assert.Equal(t, []string{
			`GRANT CONNECT ON DATABASE "tfha_gke_h3" TO "tfha-gke-h3-relay"`,
			`GRANT USAGE ON SCHEMA "custom_schema" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "custom_schema"."relay_instances", "custom_schema"."conduit_sessions" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT, INSERT, UPDATE ON TABLE "custom_schema"."conduit_principal_epochs" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT ON TABLE "custom_schema"."hub_settings" TO "tfha-gke-h3-relay"`,
		}, stmts)
	})

	t.Run("identifier quotes are escaped", func(t *testing.T) {
		stmts := relayRoleGrantStatements(`role"with"quotes`, `db"name`, `sch"ema`, false, false)
		assert.Equal(t, `GRANT CONNECT ON DATABASE "db""name" TO "role""with""quotes"`, stmts[0])
		assert.Equal(t, `GRANT USAGE ON SCHEMA "sch""ema" TO "role""with""quotes"`, stmts[1])
		assert.Contains(t, stmts[2], `"sch""ema"."relay_instances"`)
		assert.Contains(t, stmts[2], `TO "role""with""quotes"`)
	})
}

func TestEnsureRelayRoleGrants_DialectAndValidationGates(t *testing.T) {
	closedDB, err := sql.Open("pgx", "postgres://placeholder.invalid/db")
	require.NoError(t, err)
	require.NoError(t, closedDB.Close())
	ctx := context.Background()

	// Empty role is a no-op on every dialect, even with a closed *sql.DB.
	assert.NoError(t, ensureRelayRoleGrants(ctx, dialect.Postgres, closedDB, "", nil))
	assert.NoError(t, ensureRelayRoleGrants(ctx, dialect.SQLite, closedDB, "", nil))

	// Valid role on SQLite or nil DB is a no-op without touching closedDB.
	assert.NoError(t, ensureRelayRoleGrants(ctx, dialect.SQLite, closedDB, "tfha-gke-h3-relay", nil))
	assert.NoError(t, ensureRelayRoleGrants(ctx, dialect.Postgres, nil, "tfha-gke-h3-relay", nil))

	// Invalid role fails validation even on SQLite before checking the dialect.
	err = ensureRelayRoleGrants(ctx, dialect.SQLite, closedDB, " bad ", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "leading or trailing whitespace")

	// Valid role on Postgres reaches the database handle.
	err = ensureRelayRoleGrants(ctx, dialect.Postgres, closedDB, "tfha-gke-h3-relay", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking PostgreSQL relay role")
}

func TestEnsureRelayRoleGrants_ExecutionFlows(t *testing.T) {
	ctx := context.Background()

	t.Run("missing role refuses with actionable error and runs zero grants", func(t *testing.T) {
		db, state := openFakeRelayGrantDB(t, &fakeRelayGrantState{
			roleExists: false,
			dbName:     "tfha_gke_h3",
			schemaName: sql.NullString{String: "public", Valid: true},
		})
		err := ensureRelayRoleGrants(ctx, dialect.Postgres, db, "tfha-gke-h3-relay", nil)
		var notFound *RelayRoleNotFoundError
		require.ErrorAs(t, err, &notFound)
		assert.Equal(t, "tfha-gke-h3-relay", notFound.Role)
		assert.Contains(t, err.Error(), "server.database.relay_role")
		assert.Contains(t, err.Error(), "SCION_SERVER_DATABASE_RELAYROLE")
		assert.Empty(t, state.executed())
	})

	t.Run("unset current_schema fails closed before running grants", func(t *testing.T) {
		db, state := openFakeRelayGrantDB(t, &fakeRelayGrantState{
			roleExists: true,
			dbName:     "tfha_gke_h3",
			schemaName: sql.NullString{Valid: false},
		})
		err := ensureRelayRoleGrants(ctx, dialect.Postgres, db, "tfha-gke-h3-relay", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "current_schema()")
		assert.Empty(t, state.executed())
	})

	t.Run("grants missing connect plus explicit table privileges in order", func(t *testing.T) {
		db, state := openFakeRelayGrantDB(t, &fakeRelayGrantState{
			roleExists: true,
			dbName:     "tfha_gke_h3",
			schemaName: sql.NullString{String: "public", Valid: true},
			hasConnect: false,
			hasUsage:   true,
		})
		require.NoError(t, ensureRelayRoleGrants(ctx, dialect.Postgres, db, "tfha-gke-h3-relay", nil))
		assert.Equal(t, "tfha-gke-h3-relay", state.queriedRole())
		assert.Equal(t, []string{
			`GRANT CONNECT ON DATABASE "tfha_gke_h3" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "public"."relay_instances", "public"."conduit_sessions" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT, INSERT, UPDATE ON TABLE "public"."conduit_principal_epochs" TO "tfha-gke-h3-relay"`,
			`GRANT SELECT ON TABLE "public"."hub_settings" TO "tfha-gke-h3-relay"`,
		}, state.executed())
	})

	t.Run("grant exec error is surfaced", func(t *testing.T) {
		db, _ := openFakeRelayGrantDB(t, &fakeRelayGrantState{
			roleExists: true,
			dbName:     "tfha_gke_h3",
			schemaName: sql.NullString{String: "public", Valid: true},
			hasConnect: true,
			hasUsage:   true,
			failOnExec: "conduit_principal_epochs",
		})
		err := ensureRelayRoleGrants(ctx, dialect.Postgres, db, "tfha-gke-h3-relay", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "granting PostgreSQL relay role privileges")
		assert.Contains(t, err.Error(), "permission denied")
	})
}

type fakeRelayGrantState struct {
	mu          sync.Mutex
	roleExists  bool
	dbName      string
	schemaName  sql.NullString
	hasConnect  bool
	hasUsage    bool
	failOnExec  string
	lastRoleArg string
	execStmts   []string
}

func (s *fakeRelayGrantState) executed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.execStmts...)
}

func (s *fakeRelayGrantState) queriedRole() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRoleArg
}

var (
	fakeRelayGrantOnce     sync.Once
	fakeRelayGrantRegistry sync.Map
	fakeRelayGrantSeq      int64
	fakeRelayGrantSeqMu    sync.Mutex
)

func openFakeRelayGrantDB(t *testing.T, st *fakeRelayGrantState) (*sql.DB, *fakeRelayGrantState) {
	t.Helper()
	fakeRelayGrantOnce.Do(func() {
		sql.Register("fake_relay_grant_pg", fakeRelayGrantDriver{})
	})
	fakeRelayGrantSeqMu.Lock()
	fakeRelayGrantSeq++
	key := fmt.Sprintf("db-%d", fakeRelayGrantSeq)
	fakeRelayGrantSeqMu.Unlock()

	fakeRelayGrantRegistry.Store(key, st)
	t.Cleanup(func() { fakeRelayGrantRegistry.Delete(key) })

	db, err := sql.Open("fake_relay_grant_pg", key)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, st
}

type fakeRelayGrantDriver struct{}

func (fakeRelayGrantDriver) Open(name string) (driver.Conn, error) {
	v, ok := fakeRelayGrantRegistry.Load(name)
	if !ok {
		return nil, fmt.Errorf("unknown fake_relay_grant_pg dsn %q", name)
	}
	return &fakeRelayGrantConn{state: v.(*fakeRelayGrantState)}, nil
}

type fakeRelayGrantConn struct {
	state *fakeRelayGrantState
}

func (c *fakeRelayGrantConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}
func (c *fakeRelayGrantConn) Close() error              { return nil }
func (c *fakeRelayGrantConn) Begin() (driver.Tx, error) { return nil, errors.New("tx not implemented") }

func (c *fakeRelayGrantConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "pg_catalog.pg_roles") {
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
	if len(args) != 1 {
		return nil, fmt.Errorf("expected 1 query arg, got %d", len(args))
	}
	roleArg, _ := args[0].Value.(string)
	c.state.mu.Lock()
	c.state.lastRoleArg = roleArg
	var schemaVal driver.Value
	if c.state.schemaName.Valid {
		schemaVal = c.state.schemaName.String
	}
	row := []driver.Value{
		c.state.roleExists,
		c.state.dbName,
		schemaVal,
		c.state.hasConnect,
		c.state.hasUsage,
	}
	c.state.mu.Unlock()
	return &fakeRelayGrantRows{row: row}, nil
}

func (c *fakeRelayGrantConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.failOnExec != "" && strings.Contains(query, c.state.failOnExec) {
		return nil, errors.New("permission denied for table conduit_principal_epochs")
	}
	c.state.execStmts = append(c.state.execStmts, query)
	return driver.RowsAffected(0), nil
}

type fakeRelayGrantRows struct {
	row  []driver.Value
	read bool
}

func (r *fakeRelayGrantRows) Columns() []string {
	return []string{"role_exists", "db_name", "schema_name", "has_connect", "has_usage"}
}
func (r *fakeRelayGrantRows) Close() error { return nil }
func (r *fakeRelayGrantRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	copy(dest, r.row)
	return nil
}
