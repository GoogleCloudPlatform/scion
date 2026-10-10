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
	"context"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/require"
)

// maintenanceOpener describes one maintenance command whose store opener
// runs the schema migration: its --db and --execute flag variables and the
// opener itself.
type maintenanceOpener struct {
	name    string
	db      *string
	execute *bool
	open    func(context.Context) (*entadapter.CompositeStore, error)
}

func maintenanceOpeners() []maintenanceOpener {
	return []maintenanceOpener{
		{
			name:    "server backfill",
			db:      &backfillDB,
			execute: &backfillExecute,
			open: func(ctx context.Context) (*entadapter.CompositeStore, error) {
				return openBackfillStore(ctx)
			},
		},
		{
			name:    "server migrate-dm-keys",
			db:      &dmMigrationDB,
			execute: &dmMigrationExecute,
			open:    openDMMigrationStore,
		},
	}
}

// openMaintenanceStore points the command at a fresh SQLite file seeded with
// duplicate rows and opens its store with the given --execute value. It
// returns the database path and the opener's error.
func openMaintenanceStore(t *testing.T, o maintenanceOpener, execute bool) (string, error) {
	t.Helper()
	origDB, origExecute, origConfigPath := *o.db, *o.execute, serverConfigPath
	t.Cleanup(func() {
		*o.db, *o.execute, serverConfigPath = origDB, origExecute, origConfigPath
	})

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "hub.db")
	enttest.SeedPreMigrateDuplicates(t, dbPath)

	*o.db = dbPath
	*o.execute = execute
	serverConfigPath = filepath.Join(tmpDir, "nonexistent.yaml")
	s, err := o.open(context.Background())
	if s != nil {
		require.NoError(t, s.Close())
	}
	return dbPath, err
}

// With --execute, server backfill and server migrate-dm-keys run the same
// pre-migration step as server start, so a hub database holding duplicate
// access_policies, delegation_edges and agent_session_metrics rows migrates
// instead of failing on the unique indexes.
func TestMaintenanceStoreExecute_RunsPreMigrate(t *testing.T) {
	for _, o := range maintenanceOpeners() {
		t.Run(o.name, func(t *testing.T) {
			dbPath, err := openMaintenanceStore(t, o, true)
			require.NoError(t, err)
			enttest.AssertPreMigrateDeduplicated(t, dbPath)
		})
	}
}

// A dry run (the default) must not modify the database, so it skips the
// pre-migration step: the duplicate rows stay, the schema migration fails on
// the unique indexes, and the error points at --execute.
func TestMaintenanceStoreDryRun_KeepsDuplicateRows(t *testing.T) {
	for _, o := range maintenanceOpeners() {
		t.Run(o.name, func(t *testing.T) {
			dbPath, err := openMaintenanceStore(t, o, false)
			require.Error(t, err)
			require.Contains(t, err.Error(), "--execute")
			enttest.AssertPreMigrateDuplicatesKept(t, dbPath)
		})
	}
}
