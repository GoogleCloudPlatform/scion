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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Upgrade path — AutoMigrate over a pre-scope (v1) table
// =============================================================================

// TestPreStartHook_UpgradeFromPreScopeSchema exercises the schema migration on
// a database that predates the `scope` column. The v1 DDL is inlined here on
// purpose: it is a frozen snapshot of what already-deployed hubs have on disk,
// so it must not be regenerated from the current schema.
//
// Three things are load-bearing and asserted below:
//  1. Adding the `scope` enum backfills existing rows with "project".
//  2. `project_id` widening from NOT NULL to nullable succeeds on SQLite
//     (Atlas rebuilds the table) without losing rows.
//  3. The unique index swap (project_id, slug) -> (scope, project_id, slug)
//     still enforces hub-scope slug uniqueness, which only holds because
//     hub rows store project_id as "" rather than NULL.
func TestPreStartHook_UpgradeFromPreScopeSchema(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)

	// Old v1 DDL.
	old := []string{
		`CREATE TABLE ` + "`project_pre_start_hooks`" + ` (
			` + "`id`" + ` uuid NOT NULL,
			` + "`project_id`" + ` text NOT NULL,
			` + "`name`" + ` text NOT NULL,
			` + "`slug`" + ` text NOT NULL,
			` + "`description`" + ` text NULL,
			` + "`script`" + ` text NOT NULL,
			` + "`status`" + ` text NOT NULL DEFAULT ('active'),
			` + "`created_by`" + ` text NULL,
			` + "`updated_by`" + ` text NULL,
			` + "`created`" + ` datetime NOT NULL,
			` + "`updated`" + ` datetime NOT NULL,
			PRIMARY KEY (` + "`id`" + `)
		)`,
		"CREATE UNIQUE INDEX `projectprestarthook_project_id_slug` ON `project_pre_start_hooks` (`project_id`, `slug`)",
		"CREATE INDEX `projectprestarthook_project_id_status` ON `project_pre_start_hooks` (`project_id`, `status`)",
		`INSERT INTO project_pre_start_hooks
		   (id, project_id, name, slug, script, status, created, updated)
		 VALUES ('11111111-1111-1111-1111-111111111111','proj-1','Legacy','legacy',
		         '#!/bin/sh
echo legacy
','active','2026-01-01 00:00:00+00:00','2026-01-01 00:00:00+00:00')`,
	}
	for _, stmt := range old {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	require.NoError(t, raw.Close())

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)

	// Now upgrade.
	require.NoError(t, entc.AutoMigrate(ctx, client))

	s := NewProjectPreStartHookStore(client)

	// Legacy row survived and defaulted to scope="project".
	legacy, err := s.GetActiveProjectPreStartHook(ctx, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", legacy.ID)
	assert.Equal(t, store.PreStartHookScopeProject, legacy.Scope)
	assert.Equal(t, "Legacy", legacy.Name)

	// Hub hooks work on the upgraded DB.
	hub, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name: "Hub", Slug: "hub", Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.PreStartHookScopeHub, hub.Scope)

	// Hub slug uniqueness is enforced (proves project_id is '' not NULL).
	_, err = s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name: "Hub dup", Slug: "hub", Script: "#!/bin/sh\n",
	})
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

	// Legacy project hook still active — hub create did not disturb it.
	legacy2, err := s.GetActiveProjectPreStartHook(ctx, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, legacy2.Status)

	require.NoError(t, client.Close())
}
