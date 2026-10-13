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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/require"
)

// For each table the pre-migration step de-duplicates, a database holding
// duplicate rows fails the schema migration on its own (the unique index
// cannot be created), and migrates once PreMigrate runs first: the unique
// index exists and only the rows that are not duplicates remain.
func TestPreMigrate_DuplicateRowsPerTable(t *testing.T) {
	for _, table := range enttest.PreMigrateTables {
		t.Run(table, func(t *testing.T) {
			ctx := context.Background()

			// Without PreMigrate the schema migration fails, so the fixture
			// really exercises the step.
			bare := filepath.Join(t.TempDir(), "hub.db")
			enttest.SeedPreMigrateDuplicates(t, bare, table)
			client, err := entc.OpenSQLite("file:"+bare, entc.PoolConfig{MaxOpenConns: 1})
			require.NoError(t, err)
			require.Error(t, entc.AutoMigrate(ctx, client),
				"the unique index on %s must not be creatable over duplicate rows", table)
			require.NoError(t, client.Close())

			dbPath := filepath.Join(t.TempDir(), "hub.db")
			enttest.SeedPreMigrateDuplicates(t, dbPath, table)
			client, err = entc.OpenSQLite("file:"+dbPath, entc.PoolConfig{MaxOpenConns: 1})
			require.NoError(t, err)
			require.NoError(t, PreMigrate(ctx, client))
			require.NoError(t, entc.AutoMigrate(ctx, client))
			require.NoError(t, client.Close())
			enttest.AssertPreMigrateDeduplicated(t, dbPath, table)
		})
	}
}

// CompositeStore.Migrate runs the shared pre-migration step, so a hub
// database with duplicate rows in all three tables migrates on server start.
func TestCompositeStoreMigrate_DuplicateRows(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	enttest.SeedPreMigrateDuplicates(t, dbPath)

	client, err := entc.OpenSQLite("file:"+dbPath, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	require.NoError(t, cs.Migrate(ctx))
	require.NoError(t, cs.Close())
	enttest.AssertPreMigrateDeduplicated(t, dbPath)
}

// PreMigrate is a no-op on a fresh database where the tables do not exist.
func TestPreMigrate_FreshDatabase(t *testing.T) {
	ctx := context.Background()
	client, err := entc.OpenSQLite("file:"+filepath.Join(t.TempDir(), "hub.db"), entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, PreMigrate(ctx, client))
	require.NoError(t, entc.AutoMigrate(ctx, client))
}
