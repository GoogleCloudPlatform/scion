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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"

	_ "github.com/jackc/pgx/v5/stdlib"
)

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

		tx, err := raw.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+qRole)
		require.NoError(t, err)

		// Allowed reads: hub_settings, relay_instances, conduit_sessions, conduit_principal_epochs.
		for _, tbl := range []string{"hub_settings", "relay_instances", "conduit_sessions", "conduit_principal_epochs"} {
			var n int
			qTbl := pgx.Identifier{schemaName, tbl}.Sanitize()
			require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM "+qTbl).Scan(&n), "SELECT on %s", tbl)
		}

		// Denied writes & hub-only tables: no write on hub_settings, no DELETE on
		// conduit_principal_epochs, no SELECT on secrets or agents.
		_, err = tx.ExecContext(ctx, "DELETE FROM "+pgx.Identifier{schemaName, "hub_settings"}.Sanitize())
		require.Error(t, err, "relay role must not DELETE from hub_settings")

		_, err = tx.ExecContext(ctx, "DELETE FROM "+pgx.Identifier{schemaName, "conduit_principal_epochs"}.Sanitize())
		require.Error(t, err, "relay role must not DELETE from conduit_principal_epochs")

		for _, denied := range []string{"secrets", "agents", "users"} {
			var n int
			err = tx.QueryRowContext(ctx, "SELECT count(*) FROM "+pgx.Identifier{schemaName, denied}.Sanitize()).Scan(&n)
			require.Error(t, err, "relay role must not SELECT from %s", denied)
		}
	})
}
