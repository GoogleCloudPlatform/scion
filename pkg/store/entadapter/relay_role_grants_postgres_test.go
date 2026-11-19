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
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const pgInsufficientPrivilege = "42501"

func TestRelayRoleGrants_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres relay role grant cases")
	}
	ctx := context.Background()

	openRaw := func(t *testing.T, dsn string) *sql.DB {
		t.Helper()
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	t.Run("missing role refuses MigrateWithSchemaLock", func(t *testing.T) {
		dsn := enttest.NewEmptySchemaURL(t)
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 3, MaxIdleConns: 1})
		require.NoError(t, err)
		cs := NewCompositeStore(client)
		t.Cleanup(func() { _ = cs.Close() })
		cs.SetRelayRole("nonexistent-relay-role-8425")

		err = cs.MigrateWithSchemaLock(ctx)
		var notFound *RelayRoleNotFoundError
		require.True(t, errors.As(err, &notFound), "want *RelayRoleNotFoundError, got %v", err)
		assert.Equal(t, "nonexistent-relay-role-8425", notFound.Role)
	})

	t.Run("grants exact least-privilege permissions and is idempotent on re-migrate", func(t *testing.T) {
		dsn := enttest.NewEmptySchemaURL(t)
		raw := openRaw(t, dsn)

		var schemaName string
		require.NoError(t, raw.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schemaName))

		roleName := fmt.Sprintf("scion_relay_%d", time.Now().UnixNano()%1_000_000_000)
		qRole := pgx.Identifier{roleName}.Sanitize()
		qSchema := pgx.Identifier{schemaName}.Sanitize()

		_, err := raw.ExecContext(ctx, "CREATE ROLE "+qRole+" NOLOGIN NOINHERIT")
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = raw.ExecContext(context.Background(), "REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA "+qSchema+" FROM "+qRole)
			_, _ = raw.ExecContext(context.Background(), "REVOKE USAGE ON SCHEMA "+qSchema+" FROM "+qRole)
			_, _ = raw.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+qRole)
		})

		migrateOnce := func() {
			client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 3, MaxIdleConns: 1})
			require.NoError(t, err)
			cs := NewCompositeStore(client)
			defer func() { _ = cs.Close() }()
			cs.SetRelayRole(roleName)
			require.NoError(t, cs.MigrateWithSchemaLock(ctx))
		}

		migrateOnce()
		migrateOnce() // Idempotent second boot.

		// Allowed reads and DML in a clean transaction (rolled back afterward).
		tx, err := raw.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+qRole)
		require.NoError(t, err)

		for _, tbl := range []string{"hub_settings", "relay_instances", "conduit_sessions", "conduit_principal_epochs"} {
			var n int
			qTbl := pgx.Identifier{schemaName, tbl}.Sanitize()
			require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM "+qTbl).Scan(&n), "SELECT on %s", tbl)
		}

		// Allowed INSERT ... ON CONFLICT DO UPDATE RETURNING on conduit_principal_epochs
		// (exercises INSERT + UPDATE + SELECT together, matching ConduitRegistryStore).
		qEpochs := pgx.Identifier{schemaName, "conduit_principal_epochs"}.Sanitize()
		upsertEpochSQL := "INSERT INTO " + qEpochs + " AS cpe (principal_kind, principal_id, epoch) " +
			"VALUES ('agent', 'relay-grant-test', 1) " +
			"ON CONFLICT (principal_kind, principal_id) DO UPDATE SET epoch = cpe.epoch + 1 RETURNING epoch"
		var epoch int64
		require.NoError(t, tx.QueryRowContext(ctx, upsertEpochSQL).Scan(&epoch))
		assert.Equal(t, int64(1), epoch)
		require.NoError(t, tx.QueryRowContext(ctx, upsertEpochSQL).Scan(&epoch))
		assert.Equal(t, int64(2), epoch)

		for _, writable := range []string{"relay_instances", "conduit_sessions"} {
			qTbl := pgx.Identifier{schemaName, writable}.Sanitize()
			_, err = tx.ExecContext(ctx, "DELETE FROM "+qTbl+" WHERE false")
			require.NoError(t, err, "DELETE on %s", writable)
		}
		require.NoError(t, tx.Rollback())

		// Denied writes & hub-only tables: run each negative check in its own
		// fresh transaction so an expected permission error never leaves the
		// transaction in PostgreSQL's aborted state (SQLSTATE 25P02), which would
		// make later assertions pass vacuously. Also assert SQLSTATE 42501
		// (insufficient_privilege) on every denial.
		requireInsufficientPrivilege := func(desc string, op func(tx *sql.Tx) error) {
			t.Helper()
			dtx, txErr := raw.BeginTx(ctx, nil)
			require.NoError(t, txErr, "%s: begin tx", desc)
			defer func() { _ = dtx.Rollback() }()

			_, txErr = dtx.ExecContext(ctx, "SET LOCAL ROLE "+qRole)
			require.NoError(t, txErr, "%s: set local role", desc)

			opErr := op(dtx)
			require.Error(t, opErr, desc)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, opErr, &pgErr, "%s: expected *pgconn.PgError, got %v", desc, opErr)
			assert.Equal(t, pgInsufficientPrivilege, pgErr.Code,
				"%s: expected SQLSTATE %s (insufficient_privilege), got %s (%s)",
				desc, pgInsufficientPrivilege, pgErr.Code, pgErr.Message)
		}

		requireInsufficientPrivilege("relay role must not DELETE from hub_settings", func(dtx *sql.Tx) error {
			_, e := dtx.ExecContext(ctx, "DELETE FROM "+pgx.Identifier{schemaName, "hub_settings"}.Sanitize())
			return e
		})

		requireInsufficientPrivilege("relay role must not DELETE from conduit_principal_epochs", func(dtx *sql.Tx) error {
			_, e := dtx.ExecContext(ctx, "DELETE FROM "+pgx.Identifier{schemaName, "conduit_principal_epochs"}.Sanitize())
			return e
		})

		for _, denied := range []string{"secrets", "agents", "users"} {
			denied := denied
			requireInsufficientPrivilege("relay role must not SELECT from "+denied, func(dtx *sql.Tx) error {
				var n int
				return dtx.QueryRowContext(ctx, "SELECT count(*) FROM "+pgx.Identifier{schemaName, denied}.Sanitize()).Scan(&n)
			})
		}
	})
}
