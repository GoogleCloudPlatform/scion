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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestSchemaShadowGuard_Postgres runs the hub-startup migration path
// (MigrateWithSchemaLock with the shadow guard enabled) against two-schema
// search_paths: one schema holds a migrated hub table set, the other is
// empty.
//
// Runs under -tags integration with SCION_TEST_POSTGRES_URL set; skips
// otherwise.
func TestSchemaShadowGuard_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres schema shadow guard cases")
	}
	ctx := context.Background()

	// openRaw opens a plain connection pool on dsn, closed at cleanup.
	openRaw := func(t *testing.T, dsn string) *sql.DB {
		t.Helper()
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	// schemaOf returns the current schema a dsn's search_path selects.
	schemaOf := func(t *testing.T, dsn string) string {
		t.Helper()
		var s string
		require.NoError(t, openRaw(t, dsn).QueryRowContext(ctx, `SELECT current_schema()`).Scan(&s))
		return s
	}
	// tableCount counts the base tables in schema.
	tableCount := func(t *testing.T, db *sql.DB, schema string) int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_catalog.pg_class c
			   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')`, schema).Scan(&n))
		return n
	}
	// withPath returns dsn with its search_path replaced.
	withPath := func(t *testing.T, dsn, searchPath string) string {
		t.Helper()
		out, err := enttest.WithConnParam(dsn, "search_path", searchPath)
		require.NoError(t, err)
		return out
	}
	// migrate runs the hub-startup migration on dsn with the guard enabled
	// and returns the guard's log output and the migration error.
	migrate := func(t *testing.T, dsn string, allowShadowed bool) (string, error) {
		t.Helper()
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 3, MaxIdleConns: 1})
		require.NoError(t, err)
		cs := NewCompositeStore(client)
		t.Cleanup(func() { _ = cs.Close() })
		var logs bytes.Buffer
		cs.schemaShadowLogger = slog.New(slog.NewTextHandler(&logs, nil))
		cs.EnableSchemaShadowGuard(allowShadowed)
		err = cs.MigrateWithSchemaLock(ctx)
		return logs.String(), err
	}

	t.Run("refuses custom search_path", func(t *testing.T) {
		hubURL := enttest.NewSchemaURL(t)
		emptyURL := enttest.NewEmptySchemaURL(t)
		hub, empty := schemaOf(t, hubURL), schemaOf(t, emptyURL)
		raw := openRaw(t, hubURL)
		require.Positive(t, tableCount(t, raw, hub))

		_, err := migrate(t, withPath(t, hubURL, empty+","+hub), false)
		require.Error(t, err)
		var shadow *SchemaShadowError
		require.True(t, errors.As(err, &shadow), "want *SchemaShadowError, got %v", err)
		assert.Equal(t, empty, shadow.CurrentSchema)
		assert.Equal(t, hub, shadow.ShadowedSchema)
		assert.Contains(t, err.Error(), "allow_shadowed_schema")
		assert.Zero(t, tableCount(t, raw, empty), "a refused start must not create tables in the empty schema")
	})

	t.Run("refuses empty $user schema", func(t *testing.T) {
		hubURL := enttest.NewSchemaURL(t)
		hub := schemaOf(t, hubURL)
		raw := openRaw(t, hubURL)

		// A schema named after the connecting role, as "$user" in the
		// default search_path resolves to.
		var user string
		require.NoError(t, raw.QueryRowContext(ctx, `SELECT current_user`).Scan(&user))
		quoted := `"` + strings.ReplaceAll(user, `"`, `""`) + `"`
		_, err := raw.ExecContext(ctx, "CREATE SCHEMA "+quoted)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+quoted+" CASCADE") })

		_, err = migrate(t, withPath(t, hubURL, `"$user",`+hub), false)
		var shadow *SchemaShadowError
		require.True(t, errors.As(err, &shadow), "want *SchemaShadowError, got %v", err)
		assert.Equal(t, user, shadow.CurrentSchema)
		assert.Equal(t, hub, shadow.ShadowedSchema)
		assert.Zero(t, tableCount(t, raw, user))
	})

	t.Run("override continues with a warning", func(t *testing.T) {
		hubURL := enttest.NewSchemaURL(t)
		emptyURL := enttest.NewEmptySchemaURL(t)
		hub, empty := schemaOf(t, hubURL), schemaOf(t, emptyURL)
		raw := openRaw(t, hubURL)

		logs, err := migrate(t, withPath(t, hubURL, empty+","+hub), true)
		require.NoError(t, err)
		assert.Contains(t, logs, "shadows existing hub tables")
		assert.Contains(t, logs, "current_schema="+empty)
		assert.Contains(t, logs, "shadowed_schema="+hub)
		assert.Positive(t, tableCount(t, raw, empty), "the override creates the new table set in the current schema")
	})

	t.Run("fresh install starts", func(t *testing.T) {
		firstURL := enttest.NewEmptySchemaURL(t)
		secondURL := enttest.NewEmptySchemaURL(t)
		first, second := schemaOf(t, firstURL), schemaOf(t, secondURL)
		raw := openRaw(t, firstURL)

		logs, err := migrate(t, withPath(t, firstURL, first+","+second), false)
		require.NoError(t, err)
		assert.NotContains(t, logs, "shadows existing hub tables")
		assert.Positive(t, tableCount(t, raw, first))
		assert.Zero(t, tableCount(t, raw, second))
	})

	t.Run("existing install in current schema starts", func(t *testing.T) {
		hubURL := enttest.NewSchemaURL(t)
		emptyURL := enttest.NewEmptySchemaURL(t)
		hub, empty := schemaOf(t, hubURL), schemaOf(t, emptyURL)
		raw := openRaw(t, hubURL)

		logs, err := migrate(t, withPath(t, hubURL, hub+","+empty), false)
		require.NoError(t, err)
		assert.NotContains(t, logs, "shadows existing hub tables")
		assert.Zero(t, tableCount(t, raw, empty))
	})
}
