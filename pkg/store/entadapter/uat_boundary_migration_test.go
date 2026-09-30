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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oldUserAccessTokensDDL is the frozen "user_access_tokens" schema as it
// existed immediately before ptone/scion#2123 added boundary_kind and made
// project_id nullable: project_id is a required (NOT NULL) UUID and there is
// no boundary_kind column at all. This is what an already-deployed database
// looks like right before upgrade.
const oldUserAccessTokensDDL = "CREATE TABLE `user_access_tokens` (" +
	"`id` uuid NOT NULL, `user_id` uuid NOT NULL, `name` text NOT NULL, " +
	"`prefix` text NOT NULL, `key_hash` text NOT NULL, `project_id` uuid NOT NULL, " +
	"`scopes` text NOT NULL, `ceiling_version` integer NOT NULL DEFAULT (0), " +
	"`ceiling_permission_ids` text NULL, `revoked` bool NOT NULL DEFAULT (false), " +
	"`expires_at` datetime NULL, `last_used` datetime NULL, `created` datetime NOT NULL, " +
	"`purpose` text NULL, `labels` text NULL, " +
	"PRIMARY KEY (`id`))"

const (
	oldUserAccessTokensKeyHashIndex  = "CREATE UNIQUE INDEX `useraccesstoken_key_hash` ON `user_access_tokens` (`key_hash`)"
	oldUserAccessTokensUserIDIndex   = "CREATE INDEX `useraccesstoken_user_id` ON `user_access_tokens` (`user_id`)"
	oldUserAccessTokensProjectIDIdx  = "CREATE INDEX `useraccesstoken_project_id` ON `user_access_tokens` (`project_id`)"
	oldUserAccessTokensRowCountRows  = 3
	oldUserAccessTokensLegacyProject = "legacy project rows must keep their existing, non-null project_id"
)

// seedOldSchemaDB creates a fresh SQLite file with the legacy
// user_access_tokens shape (plus its supporting indexes) and inserts n
// legacy rows, each with a distinct, non-null project_id and key_hash. It
// returns the DSN and the seeded row IDs.
func seedOldSchemaDB(t *testing.T, n int) (dsn string, ids []string, projectIDs []string, keyHashes []string) {
	t.Helper()
	dsn = "file:" + filepath.Join(t.TempDir(), "legacy-uat-schema.db")

	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	ctx := context.Background()
	_, err = raw.ExecContext(ctx, oldUserAccessTokensDDL)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, oldUserAccessTokensKeyHashIndex)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, oldUserAccessTokensUserIDIndex)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, oldUserAccessTokensProjectIDIdx)
	require.NoError(t, err)

	for i := 0; i < n; i++ {
		id := uuid.NewString()
		userID := uuid.NewString()
		projectID := uuid.NewString()
		keyHash := uuid.NewString()
		// created uses the same strftime expression the migration-α raw SQL
		// path (pkg/ent/entc/migrate_alpha.go's nowExpr) relies on for a
		// SQLite DATETIME column ent can scan back, rather than a
		// Go-formatted string that might not match what the driver expects.
		_, err = raw.ExecContext(ctx,
			"INSERT INTO user_access_tokens (id, user_id, name, prefix, key_hash, project_id, scopes, ceiling_version, revoked, created) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
			id, userID, "legacy-token", "scion_pat_legacy", keyHash, projectID, `["agent:read"]`)
		require.NoError(t, err)

		ids = append(ids, id)
		projectIDs = append(projectIDs, projectID)
		keyHashes = append(keyHashes, keyHash)
	}
	return dsn, ids, projectIDs, keyHashes
}

// TestUATBoundary_UpgradeFromPreD1Schema pins the upgrade path: a legacy
// raw-DDL database with existing rows, migrated via the real
// CompositeStore.Migrate (AutoMigrate + ValidateUserAccessTokenBoundaries),
// must come out with every row classified "project", its original
// project_id preserved, and no row count change.
func TestUATBoundary_UpgradeFromPreD1Schema(t *testing.T) {
	dsn, ids, projectIDs, _ := seedOldSchemaDB(t, oldUserAccessTokensRowCountRows)

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	for i, id := range ids {
		tok, err := cs.GetUserAccessToken(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, string(permissions.BoundaryKindProject), tok.BoundaryKind)
		assert.Equal(t, projectIDs[i], tok.ProjectID, oldUserAccessTokensLegacyProject)
		assert.NoError(t, tok.ValidateBoundary())
	}
}

// TestUATBoundary_UpgradeIdempotent runs Migrate twice against an upgraded
// legacy database and asserts the second run changes nothing.
func TestUATBoundary_UpgradeIdempotent(t *testing.T) {
	dsn, ids, projectIDs, _ := seedOldSchemaDB(t, 2)

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))
	require.NoError(t, cs.Migrate(ctx))

	for i, id := range ids {
		tok, err := cs.GetUserAccessToken(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, string(permissions.BoundaryKindProject), tok.BoundaryKind)
		assert.Equal(t, projectIDs[i], tok.ProjectID)
	}
}

// TestUATBoundary_KeyHashUniqueSurvivesUpgrade proves the key_hash unique
// constraint still holds after the table has been through the
// nullability-relaxing rebuild: creating a new token that collides with an
// already-migrated legacy row's key_hash must fail.
func TestUATBoundary_KeyHashUniqueSurvivesUpgrade(t *testing.T) {
	dsn, _, _, keyHashes := seedOldSchemaDB(t, 1)

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	userID, projectID := seedProjectAndUser(t, cs)
	dup := &store.UserAccessToken{
		ID: uuid.NewString(), UserID: userID, Name: "dup", Prefix: "scion_pat_dup",
		KeyHash: keyHashes[0], BoundaryKind: string(permissions.BoundaryKindProject), ProjectID: projectID,
		Scopes: []string{"agent:read"}, Created: time.Now(),
	}
	err = cs.CreateUserAccessToken(ctx, dup)
	assert.Error(t, err, "key_hash uniqueness must survive the SQLite table rebuild")
}

// TestUATBoundary_HubTokenCreatableAfterUpgrade proves a hub-boundary token
// (NULL project_id) can be created against a database that went through the
// legacy-to-current upgrade path, not just a freshly created one.
func TestUATBoundary_HubTokenCreatableAfterUpgrade(t *testing.T) {
	dsn, _, _, _ := seedOldSchemaDB(t, 1)

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	userID, _ := seedProjectAndUser(t, cs)
	hubTok := &store.UserAccessToken{
		ID: uuid.NewString(), UserID: userID, Name: "hub-token", Prefix: "scion_pat_hub",
		KeyHash: uuid.NewString(), BoundaryKind: string(permissions.BoundaryKindHub), ProjectID: "",
		Scopes: []string{"broker:create"}, Created: time.Now(),
	}
	require.NoError(t, cs.CreateUserAccessToken(ctx, hubTok))

	got, err := cs.GetUserAccessToken(ctx, hubTok.ID)
	require.NoError(t, err)
	assert.Equal(t, string(permissions.BoundaryKindHub), got.BoundaryKind)
	assert.Equal(t, "", got.ProjectID)
	assert.NoError(t, got.ValidateBoundary())
}

// TestUATBoundary_DBCheckConstraintRejectsInvalidCombination confirms the
// schema's CHECK constraint (user_access_tokens_boundary_kind_check) is
// enforced at the database level, by issuing raw SQL inserts that go
// directly against the driver, outside every Go-level validator
// (store.UserAccessToken.ValidateBoundary, the
// ExternalStore.CreateUserAccessToken default). Confirmed empirically: the
// CHECK annotation string must itself be fully parenthesized
// ("((a) OR (b))", not "(a) OR (b)") — Atlas inserts it verbatim after the
// CHECK keyword with no enclosing paren of its own, so an under-wrapped
// expression creates a table whose CREATE TABLE statement is itself a
// SQLite syntax error, failing every migration, not just an insert.
func TestUATBoundary_DBCheckConstraintRejectsInvalidCombination(t *testing.T) {
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	db := cs.DB()
	require.NotNil(t, db)

	userID, projectID := seedProjectAndUser(t, cs)

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "hub boundary with a non-null project_id",
			sql: "INSERT INTO user_access_tokens (id, user_id, name, prefix, key_hash, project_id, boundary_kind, scopes, ceiling_version, revoked, created) " +
				"VALUES (?, ?, 'bad-hub', 'scion_pat_bad', ?, ?, 'hub', '[]', 0, 0, ?)",
			args: []any{uuid.NewString(), userID, uuid.NewString(), projectID, time.Now().UTC().Format(time.RFC3339)},
		},
		{
			name: "project boundary with a null project_id",
			sql: "INSERT INTO user_access_tokens (id, user_id, name, prefix, key_hash, project_id, boundary_kind, scopes, ceiling_version, revoked, created) " +
				"VALUES (?, ?, 'bad-project', 'scion_pat_bad', ?, NULL, 'project', '[]', 0, 0, ?)",
			args: []any{uuid.NewString(), userID, uuid.NewString(), time.Now().UTC().Format(time.RFC3339)},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, c.sql, c.args...)
			require.Error(t, err, "the DB-level CHECK constraint must reject this row even without any Go-level validator in the path")
			t.Logf("DB CHECK constraint rejected the invalid row, as required: %v", err)
		})
	}
}
