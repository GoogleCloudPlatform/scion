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

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// shadowGuardCommand runs one maintenance command's migration against dsn.
// allowShadowed sets server.database.allow_shadowed_schema the way that
// command reads it: through the loaded config for commands handed a
// *config.GlobalConfig, and through the environment for commands that load
// the config themselves.
type shadowGuardCommand struct {
	name string
	run  func(t *testing.T, dsn string, allowShadowed bool) error
}

func shadowGuardCommands() []shadowGuardCommand {
	pgConfig := func(dsn string, allowShadowed bool) *config.GlobalConfig {
		cfg := &config.GlobalConfig{}
		cfg.Database.Driver = "postgres"
		cfg.Database.URL = dsn
		cfg.Database.AllowShadowedSchema = allowShadowed
		return cfg
	}
	// isolateConfig points config loading at an empty HOME and working
	// directory, with no settings file and the real (empty) --config value
	// of these commands, and sets the override in the environment.
	isolateConfig := func(t *testing.T, allowShadowed bool) {
		t.Helper()
		t.Setenv("HOME", t.TempDir())
		t.Chdir(t.TempDir())
		orig := serverConfigPath
		t.Cleanup(func() { serverConfigPath = orig })
		serverConfigPath = ""
		if allowShadowed {
			t.Setenv(allowShadowedSchemaEnv, "true")
		}
	}

	cmds := []shadowGuardCommand{
		{
			name: "server recover-authz",
			run: func(t *testing.T, dsn string, allowShadowed bool) error {
				cs, err := openRecoveryStore(context.Background(), pgConfig(dsn, allowShadowed))
				if cs != nil {
					_ = cs.Close()
				}
				return err
			},
		},
		{
			name: "hub secret migrate-names",
			run: func(t *testing.T, dsn string, allowShadowed bool) error {
				cs, err := openMigrateNamesStore(context.Background(), pgConfig(dsn, allowShadowed), false)
				if cs != nil {
					_ = cs.Close()
				}
				return err
			},
		},
		{
			name: "server test-infra mark",
			run: func(t *testing.T, dsn string, allowShadowed bool) error {
				client, err := openTestInfraClient(context.Background(), pgConfig(dsn, allowShadowed))
				if client != nil {
					_ = client.Close()
				}
				return err
			},
		},
		{
			name: "server migrate",
			run: func(t *testing.T, dsn string, allowShadowed bool) error {
				isolateConfig(t, allowShadowed)
				srcPath := filepath.Join(t.TempDir(), "hub.db")
				src, err := entc.OpenSQLite("file:"+srcPath+"?cache=shared", entc.PoolConfig{})
				require.NoError(t, err)
				require.NoError(t, entc.AutoMigrate(context.Background(), src))
				require.NoError(t, src.Close())

				origFrom, origTo, origDrop := migrateFrom, migrateTo, migrateDropSource
				t.Cleanup(func() { migrateFrom, migrateTo, migrateDropSource = origFrom, origTo, origDrop })
				migrateFrom, migrateTo, migrateDropSource = "sqlite://"+srcPath, dsn, false

				c := &cobra.Command{}
				c.SetContext(context.Background())
				c.SetOut(&bytes.Buffer{})
				return runServerMigrate(c, nil)
			},
		},
	}
	// server backfill and server migrate-dm-keys, in dry-run and --execute
	// mode: a dry run skips the pre-migration steps but not the guard.
	for _, o := range maintenanceOpeners() {
		for _, execute := range []bool{false, true} {
			name := o.name + " (dry run)"
			if execute {
				name = o.name + " --execute"
			}
			cmds = append(cmds, shadowGuardCommand{
				name: name,
				run: func(t *testing.T, dsn string, allowShadowed bool) error {
					if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
						t.Skip("--db detects PostgreSQL only from a postgres:// URL; SCION_TEST_POSTGRES_URL is a keyword/value DSN")
					}
					isolateConfig(t, allowShadowed)
					origDB, origExecute := *o.db, *o.execute
					t.Cleanup(func() { *o.db, *o.execute = origDB, origExecute })
					*o.db, *o.execute = dsn, execute
					cs, err := o.open(context.Background())
					if cs != nil {
						_ = cs.Close()
					}
					return err
				},
			})
		}
	}
	return cmds
}

// TestMaintenanceCommandsSchemaShadowGuard_Postgres runs each standalone
// command that migrates the hub database against a search_path whose first
// schema is empty and whose second holds a migrated hub table set. Each must
// refuse, as hub server startup does, without creating tables, and each must
// migrate into the empty schema when server.database.allow_shadowed_schema
// is set (ptone/scion#4432).
//
// Runs under -tags integration with SCION_TEST_POSTGRES_URL set; skips
// otherwise.
func TestMaintenanceCommandsSchemaShadowGuard_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres schema shadow guard cases")
	}

	// shadowedDSN returns a DSN whose search_path is "<empty>,<hub>", the
	// raw connection to that database, and the empty schema's name.
	shadowedDSN := func(t *testing.T) (string, *sql.DB, string) {
		t.Helper()
		hubURL := enttest.NewSchemaURL(t)
		emptyURL := enttest.NewEmptySchemaURL(t)
		raw, err := sql.Open("pgx", hubURL)
		require.NoError(t, err)
		t.Cleanup(func() { _ = raw.Close() })
		hub := currentSchema(t, hubURL)
		empty := currentSchema(t, emptyURL)
		require.Positive(t, schemaTableCount(t, raw, hub))
		dsn, err := enttest.WithConnParam(hubURL, "search_path", empty+","+hub)
		require.NoError(t, err)
		return dsn, raw, empty
	}

	for _, c := range shadowGuardCommands() {
		t.Run(c.name, func(t *testing.T) {
			t.Run("refuses", func(t *testing.T) {
				dsn, raw, empty := shadowedDSN(t)
				err := c.run(t, dsn, false)
				require.Error(t, err)
				var shadow *entadapter.SchemaShadowError
				require.True(t, errors.As(err, &shadow), "want *SchemaShadowError, got %v", err)
				assert.Equal(t, empty, shadow.CurrentSchema)
				assert.True(t, shadow.Maintenance, "a maintenance command refuses to migrate, not to start")
				assert.Contains(t, err.Error(), "refusing to migrate the hub database: ")
				assert.NotContains(t, err.Error(), "refusing to start")
				assert.Contains(t, err.Error(), "allow_shadowed_schema")
				assert.NotContains(t, err.Error(), "--execute", "a guard refusal is not a dry-run unique-index failure")
				assert.Zero(t, schemaTableCount(t, raw, empty), "a refused migration must not create tables in the empty schema")
			})
			t.Run("override migrates", func(t *testing.T) {
				dsn, raw, empty := shadowedDSN(t)
				require.NoError(t, c.run(t, dsn, true))
				assert.Positive(t, schemaTableCount(t, raw, empty), "with the override the migration creates the table set in the empty schema")
			})
		})
	}
}

// currentSchema returns the schema dsn's search_path selects.
func currentSchema(t *testing.T, dsn string) string {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var s string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT current_schema()`).Scan(&s))
	return s
}

// schemaTableCount counts the base tables in schema.
func schemaTableCount(t *testing.T, db *sql.DB, schema string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pg_catalog.pg_class c
		   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')`, schema).Scan(&n))
	return n
}

// TestMigrateHubHelpers_SQLite checks that the shared migrate helpers still
// migrate a SQLite hub database: the search_path guard is PostgreSQL-only.
func TestMigrateHubHelpers_SQLite(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T) (*entadapter.CompositeStore, *ent.Client) {
		t.Helper()
		client, err := entc.OpenSQLite("file:"+filepath.Join(t.TempDir(), "hub.db")+"?cache=shared", entc.PoolConfig{})
		require.NoError(t, err)
		cs := entadapter.NewCompositeStore(client)
		t.Cleanup(func() { _ = cs.Close() })
		return cs, client
	}

	t.Run("migrateHubStore", func(t *testing.T) {
		cs, _ := open(t)
		require.NoError(t, migrateHubStore(ctx, cs, false))
		require.NoError(t, cs.Ping(ctx))
		_, err := cs.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{})
		require.NoError(t, err, "the migration must have created the hub tables")
	})
	for _, preMigrate := range []bool{false, true} {
		t.Run(fmt.Sprintf("migrateHubSchema preMigrate=%v", preMigrate), func(t *testing.T) {
			cs, client := open(t)
			require.NoError(t, migrateHubSchema(ctx, client, false, preMigrate))
			_, err := cs.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{})
			require.NoError(t, err, "the migration must have created the hub tables")
		})
	}
}
