// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package entc

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/migrate"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const templateLegacyFixtureID = "00000000-0000-4000-8000-0000000042a4"

// sqliteTemplateColumns returns the templates table's column names.
func sqliteTemplateColumns(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info('templates')")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		cols[name] = true
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, cols, "templates table must exist")
	return cols
}

func requireNoTemplateLegacyColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	cols := sqliteTemplateColumns(t, db)
	require.False(t, cols["config"], "templates.config must be dropped")
	require.False(t, cols["image"], "templates.image must be dropped")
	require.True(t, cols["agent_config"], "templates.agent_config must be kept")
	require.True(t, cols["default_harness_config"], "templates.default_harness_config must be kept")
}

// addTemplateLegacyColumns returns a migrated templates table to the
// pre-ptone/scion#4224 shape for cols and seeds one row holding values in
// them.
func addTemplateLegacyColumns(t *testing.T, db *sql.DB, cols ...string) {
	t.Helper()
	for _, col := range cols {
		_, err := db.Exec("ALTER TABLE templates ADD COLUMN " + col + " TEXT")
		require.NoError(t, err)
	}
	_, err := db.Exec(`INSERT INTO templates (id, name, slug, harness, scope, created, updated)
		VALUES (?, 'legacy-row', 'legacy-row', 'claude', 'global', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		templateLegacyFixtureID)
	require.NoError(t, err)
	for _, col := range cols {
		value := `{"model":"legacy-model","env":{"K":"v"}}`
		if col == "image" {
			value = "legacy-image:v1"
		}
		_, err := db.Exec("UPDATE templates SET "+col+" = ? WHERE id = ?", value, templateLegacyFixtureID)
		require.NoError(t, err)
	}
	present := sqliteTemplateColumns(t, db)
	for _, col := range cols {
		require.True(t, present[col], col)
	}
}

func TestTemplateLegacyColumnsDrop_NotInSchema(t *testing.T) {
	for _, table := range migrate.Tables {
		if table.Name != "templates" {
			continue
		}
		for _, col := range table.Columns {
			require.NotEqual(t, "config", col.Name)
			require.NotEqual(t, "image", col.Name)
		}
		return
	}
	t.Fatal("templates table not in migrate.Tables")
}

func TestTemplateLegacyColumnsDrop_UpgradeRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	open := func() *ent.Client {
		client, err := OpenSQLite(path, PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		return client
	}
	client := open()
	require.NoError(t, AutoMigrate(ctx, client))
	db := client.Driver().(*entsql.Driver).DB()
	addTemplateLegacyColumns(t, db, "image", "config")
	require.NoError(t, client.Close())

	client = open()
	defer func() { require.NoError(t, client.Close()) }()
	db = client.Driver().(*entsql.Driver).DB()
	require.NoError(t, AutoMigrate(ctx, client))
	requireNoTemplateLegacyColumns(t, db)
	// Re-running AutoMigrate is a no-op.
	for range 2 {
		require.NoError(t, AutoMigrate(ctx, client))
		requireNoTemplateLegacyColumns(t, db)
	}
	// The row survives with its other columns, and the ent client reads it.
	row, err := client.Template.Get(ctx, uuid.MustParse(templateLegacyFixtureID))
	require.NoError(t, err)
	require.Equal(t, "legacy-row", row.Name)
	require.Equal(t, "claude", row.Harness)
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM templates").Scan(&count))
	require.Equal(t, 1, count)
}

func TestTemplateLegacyColumnsDrop_PartialSQLite(t *testing.T) {
	ctx := context.Background()
	client, err := OpenSQLite(filepath.Join(t.TempDir(), "partial.db"), PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Close()) }()
	require.NoError(t, AutoMigrate(ctx, client))
	db := client.Driver().(*entsql.Driver).DB()
	// A fresh database has neither column; the step is a no-op.
	require.NoError(t, dropTemplateLegacyColumns(ctx, client))
	requireNoTemplateLegacyColumns(t, db)
	// Only one column left behind: it is dropped and the absent one skipped.
	addTemplateLegacyColumns(t, db, "image")
	require.NoError(t, dropTemplateLegacyColumns(ctx, client))
	requireNoTemplateLegacyColumns(t, db)
	require.NoError(t, dropTemplateLegacyColumns(ctx, client))
	requireNoTemplateLegacyColumns(t, db)
}

type templateDropDriver struct {
	dialect.Driver
	name     string
	tx       *templateDropTx
	beginErr error
	begins   int
}

func (d *templateDropDriver) Dialect() string { return d.name }
func (d *templateDropDriver) Tx(context.Context) (dialect.Tx, error) {
	d.begins++
	return d.tx, d.beginErr
}

type templateDropTx struct {
	dialect.Tx
	queryErr, execErr, commitErr error
	statements                   []string
	queries, commits, rollbacks  int
}

func (tx *templateDropTx) Query(context.Context, string, any, any) error {
	tx.queries++
	return tx.queryErr
}
func (tx *templateDropTx) Exec(_ context.Context, query string, args, result any) error {
	tx.statements = append(tx.statements, query)
	return tx.execErr
}
func (tx *templateDropTx) Commit() error   { tx.commits++; return tx.commitErr }
func (tx *templateDropTx) Rollback() error { tx.rollbacks++; return nil }

func TestTemplateLegacyColumnsDrop_PostgresDialect(t *testing.T) {
	tx := &templateDropTx{}
	driver := &templateDropDriver{name: dialect.Postgres, tx: tx}
	client := ent.NewClient(ent.Driver(driver))
	for range 2 {
		require.NoError(t, dropTemplateLegacyColumns(context.Background(), client))
	}
	want := []string{
		"ALTER TABLE templates DROP COLUMN IF EXISTS config",
		"ALTER TABLE templates DROP COLUMN IF EXISTS image",
	}
	require.Equal(t, append(append([]string{}, want...), want...), tx.statements)
	require.Zero(t, tx.queries, "Postgres relies on IF EXISTS, not a column probe")
	require.Equal(t, 2, tx.commits)
	require.Zero(t, tx.rollbacks)
	driver.name = "unsupported"
	require.ErrorContains(t, dropTemplateLegacyColumns(context.Background(), client), "unsupported dialect")
	require.Equal(t, 2, driver.begins)
}

func TestTemplateLegacyColumnsDrop_AtomicFailure(t *testing.T) {
	fault := errors.New("fixture fault")
	for _, tc := range []struct{ stage, dialect string }{
		{"begin", dialect.Postgres},
		{"exec", dialect.Postgres},
		{"commit", dialect.Postgres},
		{"begin", dialect.SQLite},
		{"query", dialect.SQLite},
	} {
		t.Run(tc.dialect+"/"+tc.stage, func(t *testing.T) {
			tx := &templateDropTx{}
			driver := &templateDropDriver{name: tc.dialect, tx: tx}
			switch tc.stage {
			case "begin":
				driver.beginErr = fault
			case "query":
				tx.queryErr = fault
			case "exec":
				tx.execErr = fault
			case "commit":
				tx.commitErr = fault
			}
			client := ent.NewClient(ent.Driver(driver))
			require.ErrorIs(t, dropTemplateLegacyColumns(context.Background(), client), fault)
			require.Equal(t, 1, driver.begins)
			if tc.stage == "begin" {
				require.Zero(t, tx.rollbacks)
				require.Zero(t, tx.commits)
				return
			}
			require.Equal(t, 1, tx.rollbacks)
			if tc.stage != "commit" {
				require.Zero(t, tx.commits)
			}
			if tc.stage == "query" || tc.stage == "exec" {
				require.LessOrEqual(t, len(tx.statements), 1, "a failed step stops before further drops")
			}
		})
	}
}
