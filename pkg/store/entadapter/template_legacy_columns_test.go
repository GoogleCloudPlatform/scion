// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package entadapter

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"entgo.io/ent/dialect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Tests for the removal of the retired templates.config and templates.image
// columns (ptone/scion#4224). The Postgres case is compiled in the default
// (!no_sqlite) build and skips
// unless the enttest Postgres backend is built (-tags integration) and
// SCION_TEST_POSTGRES_URL is set; CI runs it in the
// make test-store-postgres-entadapter jobs:
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/postgres?sslmode=disable' \
//	  go test -tags integration -run 'TestTemplateLegacyColumns' ./pkg/store/entadapter/

// templateTableColumns returns the physical column names of the templates
// table in the connection's current schema, for SQLite and Postgres.
func templateTableColumns(t *testing.T, d string, db *sql.DB) map[string]bool {
	t.Helper()
	query := "SELECT name FROM pragma_table_info('templates')"
	if d == dialect.Postgres {
		query = `SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'templates'`
	}
	rows, err := db.Query(query)
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

func requireTemplateLegacyColumnsGone(t *testing.T, d string, db *sql.DB) {
	t.Helper()
	cols := templateTableColumns(t, d, db)
	require.False(t, cols["config"], "templates.config must be dropped (%s)", d)
	require.False(t, cols["image"], "templates.image must be dropped (%s)", d)
	require.True(t, cols["agent_config"], "templates.agent_config must be kept (%s)", d)
	require.True(t, cols["files"], "templates.files must be kept (%s)", d)
}

// seedTemplateLegacyColumns gives a migrated templates table back its
// retired config and image columns and seeds one row with values in both,
// which is the shape of a database created before ptone/scion#4224.
func seedTemplateLegacyColumns(t *testing.T, d string, db *sql.DB, id string) {
	t.Helper()
	colType, insert := "TEXT", `INSERT INTO templates (id, name, slug, harness, scope, created, updated, config, image)
		VALUES (?, 'legacy-row', 'legacy-row', 'claude', 'global', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', ?, ?)`
	if d == dialect.Postgres {
		colType, insert = "character varying", `INSERT INTO templates (id, name, slug, harness, scope, created, updated, config, image)
		VALUES ($1, 'legacy-row', 'legacy-row', 'claude', 'global', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00', $2, $3)`
	}
	for _, col := range []string{"image", "config"} {
		_, err := db.Exec("ALTER TABLE templates ADD COLUMN " + col + " " + colType)
		require.NoError(t, err, col)
	}
	_, err := db.Exec(insert, id, `{"model":"legacy-model","env":{"K":"v"},"hubAccess":{"scopes":["x"]}}`, "legacy-image:v1")
	require.NoError(t, err)
	cols := templateTableColumns(t, d, db)
	require.True(t, cols["config"] && cols["image"], "fixture must have the retired columns")
}

// requireLegacyRowReadable reads the seeded row through the template store.
func requireLegacyRowReadable(t *testing.T, cs *CompositeStore, id string) {
	t.Helper()
	got, err := cs.GetTemplate(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "legacy-row", got.Name)
	assert.Equal(t, "claude", got.Harness)
	assert.Nil(t, got.AgentConfig)
}

// TestTemplateLegacyColumnsDrop_SQLite: on a SQLite database with the
// retired columns, the Hub startup migration drops both, keeps the row, and
// a re-run is a no-op.
func TestTemplateLegacyColumnsDrop_SQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	client, err := entc.OpenSQLite(path, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.Migrate(ctx))

	id := uuid.New().String()
	seedTemplateLegacyColumns(t, dialect.SQLite, cs.DB(), id)
	for range 3 {
		require.NoError(t, cs.Migrate(ctx))
		requireTemplateLegacyColumnsGone(t, dialect.SQLite, cs.DB())
	}
	requireLegacyRowReadable(t, cs, id)
}

// TestTemplateLegacyColumnsDrop_Postgres is the Postgres counterpart: the
// migration Hub startup runs (MigrateWithSchemaLock) drops both columns with
// DROP COLUMN IF EXISTS, keeps the row, and a re-run is a no-op.
func TestTemplateLegacyColumnsDrop_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres migration case")
	}
	ctx := context.Background()
	url := enttest.NewSchemaURL(t) // fresh, fully migrated schema
	raw, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	id := uuid.New().String()
	seedTemplateLegacyColumns(t, dialect.Postgres, raw, id)

	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	for range 3 {
		require.NoError(t, cs.MigrateWithSchemaLock(ctx))
		requireTemplateLegacyColumnsGone(t, dialect.Postgres, raw)
	}
	requireLegacyRowReadable(t, cs, id)
}

// TestTemplateRoundTrip_NoLegacyColumns runs the template store round trip
// (create, get, update, list, UpdateTemplateContent, SetTemplateAgentConfig)
// against a schema without the retired columns, on whichever backend enttest
// provides (SQLite, or Postgres under -tags integration).
func TestTemplateRoundTrip_NoLegacyColumns(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	ts := NewTemplateStore(client)
	db := clientDB(client)
	require.NotNil(t, db)
	requireTemplateLegacyColumnsGone(t, client.Driver().Dialect(), db)

	tmpl := &store.Template{
		ID: uuid.New().String(), Name: "rt", Slug: "rt", Harness: "claude",
		DefaultHarnessConfig: "claude-web", Scope: store.TemplateScopeGlobal,
		Status: store.TemplateStatusActive, ContentHash: "hash-1", Layout: store.TemplateLayoutBlobs,
		Description: "round trip", Files: []store.TemplateFile{{Path: "a.md", Size: 1, Hash: "sha256:a"}},
	}
	require.NoError(t, ts.CreateTemplate(ctx, tmpl))

	got, err := ts.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	assert.Equal(t, "rt", got.Name)
	assert.Equal(t, "claude-web", got.DefaultHarnessConfig)
	assert.Equal(t, tmpl.Files, got.Files)

	got.DisplayName = "Round Trip"
	require.NoError(t, ts.UpdateTemplate(ctx, got))

	list, err := ts.ListTemplates(ctx, store.TemplateFilter{Scope: store.TemplateScopeGlobal}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "Round Trip", list.Items[0].DisplayName)

	ok, err := ts.SetTemplateAgentConfig(ctx, tmpl.ID, &api.ScionConfig{Model: "snap"},
		store.TemplateContentPrecondition{ContentHash: "hash-1", Layout: store.TemplateLayoutBlobs})
	require.NoError(t, err)
	require.True(t, ok)

	commit, err := ts.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	require.NotNil(t, commit.AgentConfig)
	assert.Equal(t, "snap", commit.AgentConfig.Model)
	commit.ContentHash = "hash-2"
	commit.Harness = "gemini"
	require.NoError(t, ts.UpdateTemplateContent(ctx, commit,
		store.TemplateContentPrecondition{ContentHash: "hash-1", Layout: store.TemplateLayoutBlobs}))

	final, err := ts.GetTemplateBySlug(ctx, "rt", store.TemplateScopeGlobal, "")
	require.NoError(t, err)
	assert.Equal(t, "hash-2", final.ContentHash)
	assert.Equal(t, "gemini", final.Harness)
	assert.Equal(t, "Round Trip", final.DisplayName)
	assert.Equal(t, "round trip", final.Description)
	require.NotNil(t, final.AgentConfig)
	assert.Equal(t, "snap", final.AgentConfig.Model)
	requireTemplateLegacyColumnsGone(t, client.Driver().Dialect(), db)
}
