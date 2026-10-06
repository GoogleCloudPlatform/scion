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
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
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

// oldUserAccessTokensDDL is the legacy "user_access_tokens" table shape:
// no boundary_kind column and a required (NOT NULL) project_id. Migrate must
// upgrade a database in this shape in place.
const oldUserAccessTokensDDL = "CREATE TABLE `user_access_tokens` (" +
	"`id` uuid NOT NULL, `user_id` uuid NOT NULL, `name` text NOT NULL, " +
	"`prefix` text NOT NULL, `key_hash` text NOT NULL, `project_id` uuid NOT NULL, " +
	"`scopes` text NOT NULL, `ceiling_version` integer NOT NULL DEFAULT (0), " +
	"`ceiling_permission_ids` text NULL, `revoked` bool NOT NULL DEFAULT (false), " +
	"`expires_at` datetime NULL, `last_used` datetime NULL, `created` datetime NOT NULL, " +
	"`purpose` text NULL, `labels` text NULL, " +
	"PRIMARY KEY (`id`))"

const (
	oldUserAccessTokensKeyHashIndex  = "CREATE UNIQUE INDEX `user_access_tokens_key_hash_key` ON `user_access_tokens` (`key_hash`)"
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
		projectID := uuid.NewString()
		id, keyHash := insertLegacyUATRow(t, raw, projectID)
		ids = append(ids, id)
		projectIDs = append(projectIDs, projectID)
		keyHashes = append(keyHashes, keyHash)
	}
	return dsn, ids, projectIDs, keyHashes
}

// insertLegacyUATRow inserts one row into a legacy-shape user_access_tokens
// table with the given raw project_id text, which need not be a valid UUID.
// It returns the row ID and key_hash.
func insertLegacyUATRow(t *testing.T, raw *sql.DB, projectID string) (id, keyHash string) {
	t.Helper()
	id = uuid.NewString()
	keyHash = uuid.NewString()
	// created uses the same strftime expression the migration-α raw SQL
	// path (pkg/ent/entc/migrate_alpha.go's nowExpr) relies on for a
	// SQLite DATETIME column ent can scan back, rather than a
	// Go-formatted string that might not match what the driver expects.
	_, err := raw.ExecContext(context.Background(),
		"INSERT INTO user_access_tokens (id, user_id, name, prefix, key_hash, project_id, scopes, ceiling_version, revoked, created) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
		id, uuid.NewString(), "legacy-token", "scion_pat_legacy", keyHash, projectID, `["agent:read"]`)
	require.NoError(t, err)
	return id, keyHash
}

// insertLegacyUATRowInto opens dsn, inserts one legacy row with the given
// raw project_id, and closes the connection.
func insertLegacyUATRowInto(t *testing.T, dsn, projectID string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	id, _ := insertLegacyUATRow(t, raw, projectID)
	return id
}

// openLegacyStore opens a CompositeStore over the SQLite file at dsn, with
// its boundary-validation report captured in the returned buffer.
func openLegacyStore(t *testing.T, dsn string) (*CompositeStore, *uatBoundaryLogCapture) {
	t.Helper()
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	capture := &uatBoundaryLogCapture{}
	cs.uatBoundaryLogger = slog.New(slog.NewJSONHandler(&capture.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return cs, capture
}

// uatBoundaryLogCapture collects the JSON log lines a CompositeStore's
// uatBoundaryLogger writes.
type uatBoundaryLogCapture struct {
	buf bytes.Buffer
}

// invalidTokenIDs returns the token IDs reported across every
// invalid-boundary log record, in order, and the number of such records.
func (c *uatBoundaryLogCapture) invalidTokenIDs(t *testing.T) (ids []string, records int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level    string   `json:"level"`
			Msg      string   `json:"msg"`
			Count    int      `json:"count"`
			TokenIDs []string `json:"token_ids"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line: %s", line)
		if !strings.Contains(rec.Msg, "invalid boundary") {
			continue
		}
		assert.Equal(t, "ERROR", rec.Level)
		assert.Equal(t, len(rec.TokenIDs), rec.Count)
		ids = append(ids, rec.TokenIDs...)
		records++
	}
	return ids, records
}

// rawUATRow returns every column of one user_access_tokens row exactly as
// the driver returns it, for byte-level before/after comparison.
func rawUATRow(t *testing.T, db *sql.DB, id string) map[string]any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT * FROM user_access_tokens WHERE id = ?", id)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	require.True(t, rows.Next(), "row %s not found", id)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	require.NoError(t, rows.Scan(ptrs...))
	require.False(t, rows.Next())
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		if b, ok := vals[i].([]byte); ok {
			vals[i] = append([]byte(nil), b...)
		}
		out[c] = vals[i]
	}
	return out
}

func countUATRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM user_access_tokens").Scan(&n))
	return n
}

// assertUpgradedUATSchema asserts the shape of an upgraded SQLite
// user_access_tokens table: the boundary CHECK is present and enforced, the
// key_hash unique index and the user_id and project_id indexes exist,
// boundary_kind is NOT NULL with default 'project', project_id is nullable,
// and the table holds exactly wantRows rows.
func assertUpgradedUATSchema(t *testing.T, cs *CompositeStore, wantRows int) {
	t.Helper()
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	assert.Equal(t, wantRows, countUATRows(t, db), "upgrade must neither add nor drop rows")

	var tableSQL string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'user_access_tokens'").Scan(&tableSQL))
	assert.Contains(t, tableSQL, "user_access_tokens_boundary_kind_check")

	indexes := map[string]string{}
	rows, err := db.QueryContext(ctx,
		"SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND tbl_name = 'user_access_tokens'")
	require.NoError(t, err)
	for rows.Next() {
		var name, ddl string
		require.NoError(t, rows.Scan(&name, &ddl))
		indexes[name] = ddl
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()
	require.Contains(t, indexes, "user_access_tokens_key_hash_key")
	assert.Contains(t, strings.ToUpper(indexes["user_access_tokens_key_hash_key"]), "UNIQUE")
	assert.Contains(t, indexes, "useraccesstoken_user_id")
	assert.Contains(t, indexes, "useraccesstoken_project_id")

	type colInfo struct {
		notNull bool
		dflt    sql.NullString
	}
	cols := map[string]colInfo{}
	rows, err = db.QueryContext(ctx, "SELECT name, \"notnull\", dflt_value FROM pragma_table_info('user_access_tokens')")
	require.NoError(t, err)
	for rows.Next() {
		var name string
		var notNull int
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&name, &notNull, &dflt))
		cols[name] = colInfo{notNull: notNull != 0, dflt: dflt}
	}
	require.NoError(t, rows.Err())
	_ = rows.Close()
	require.Contains(t, cols, "boundary_kind")
	assert.True(t, cols["boundary_kind"].notNull, "boundary_kind must be NOT NULL")
	assert.True(t, cols["boundary_kind"].dflt.Valid)
	assert.Contains(t, cols["boundary_kind"].dflt.String, "'project'")
	require.Contains(t, cols, "project_id")
	assert.False(t, cols["project_id"].notNull, "project_id must be nullable")

	userID, projectID := seedProjectAndUser(t, cs)
	now := time.Now().UTC().Format(time.RFC3339)
	insert := "INSERT INTO user_access_tokens (id, user_id, name, prefix, key_hash, project_id, boundary_kind, scopes, ceiling_version, revoked, created) " +
		"VALUES (?, ?, 'bad', 'scion_pat_bad', ?, ?, ?, '[]', 0, 0, ?)"
	for _, c := range []struct {
		name      string
		projectID any
		kind      string
	}{
		{"hub boundary with a project_id", projectID, "hub"},
		{"project boundary with a null project_id", nil, "project"},
		{"empty boundary kind", projectID, ""},
		{"unrecognized boundary kind", projectID, "org"},
	} {
		_, err := db.ExecContext(ctx, insert, uuid.NewString(), userID, uuid.NewString(), c.projectID, c.kind, now)
		if assert.Error(t, err, "the upgraded table's CHECK must reject: %s", c.name) {
			assert.Contains(t, err.Error(), "CHECK constraint failed", c.name)
		}
	}
	assert.Equal(t, wantRows, countUATRows(t, db), "rejected inserts must not add rows")
}

// TestUATBoundary_UpgradeFromLegacySchema pins the upgrade path: a legacy
// raw-DDL database with existing rows, migrated via the real
// CompositeStore.Migrate (AutoMigrate + ValidateUserAccessTokenBoundaries),
// must come out with every row classified "project", its original
// project_id preserved, the exact row count, the boundary CHECK enforced,
// the indexes and column defaults in place, and no invalid row reported.
func TestUATBoundary_UpgradeFromLegacySchema(t *testing.T) {
	dsn, ids, projectIDs, _ := seedOldSchemaDB(t, oldUserAccessTokensRowCountRows)
	cs, capture := openLegacyStore(t, dsn)

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	for i, id := range ids {
		tok, err := cs.GetUserAccessToken(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, string(permissions.BoundaryKindProject), tok.BoundaryKind)
		assert.Equal(t, projectIDs[i], tok.ProjectID, oldUserAccessTokensLegacyProject)
		assert.NoError(t, tok.ValidateBoundary())
	}

	reported, _ := capture.invalidTokenIDs(t)
	assert.Empty(t, reported, "clean legacy rows must not be reported as invalid")
	assertUpgradedUATSchema(t, cs, oldUserAccessTokensRowCountRows)
}

// TestUATBoundary_UpgradeLegacyEmptyProjectID covers a legacy row whose
// project_id is the empty string (the legacy column is text-affinity, and
// NOT NULL admits an empty string). Migrate succeeds and the CHECK admits the row, since
// an empty string IS NOT NULL. The row reads back as a project boundary with the nil
// UUID, which ValidateBoundary rejects, so it can never authenticate.
// Migrate's boundary step reports its ID, writes nothing, and the clean
// rows beside it are untouched.
func TestUATBoundary_UpgradeLegacyEmptyProjectID(t *testing.T) {
	dsn, ids, projectIDs, _ := seedOldSchemaDB(t, 2)
	badID := insertLegacyUATRowInto(t, dsn, "")
	cs, capture := openLegacyStore(t, dsn)

	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx), "an empty legacy project_id must not fail boot")

	bad, err := cs.GetUserAccessToken(ctx, badID)
	require.NoError(t, err)
	assert.Equal(t, string(permissions.BoundaryKindProject), bad.BoundaryKind)
	assert.Equal(t, uuid.Nil.String(), bad.ProjectID)
	assert.ErrorIs(t, bad.ValidateBoundary(), store.ErrInvalidUATBoundary)

	reported, records := capture.invalidTokenIDs(t)
	assert.Equal(t, []string{badID}, reported, "Migrate must run the boundary step, which reports the row's ID")
	assert.Equal(t, 1, records)

	for i, id := range ids {
		tok, err := cs.GetUserAccessToken(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, projectIDs[i], tok.ProjectID)
		assert.NoError(t, tok.ValidateBoundary())
	}
	assertUpgradedUATSchema(t, cs, 3)
}

// TestUATBoundary_UpgradeLegacyNonUUIDProjectIDFailsMigrate covers a legacy
// row whose project_id is text that is not a UUID. Only a hand-edited SQLite
// row can hold one: Postgres's uuid column and the API cannot. Migrate
// fails, so the hub does not boot, with an error that names the user access
// token step and the project_id column. The remediation is to correct or
// delete the row. Migrate rewrites nothing in the row on the way out.
func TestUATBoundary_UpgradeLegacyNonUUIDProjectIDFailsMigrate(t *testing.T) {
	dsn, _, _, _ := seedOldSchemaDB(t, 1)
	badID := insertLegacyUATRowInto(t, dsn, "not-a-uuid")
	cs, _ := openLegacyStore(t, dsn)

	err := cs.Migrate(context.Background())
	require.Error(t, err, "a non-UUID legacy project_id must fail Migrate")
	t.Logf("Migrate failed as required: %v", err)
	assert.Contains(t, err.Error(), "user access token")
	assert.Contains(t, err.Error(), "project_id")

	var stored string
	require.NoError(t, cs.DB().QueryRowContext(context.Background(),
		"SELECT project_id FROM user_access_tokens WHERE id = ?", badID).Scan(&stored))
	assert.Equal(t, "not-a-uuid", stored)
}

// TestValidateUserAccessTokenBoundaries_ReadsOnly seeds a row the CHECK
// admits but ValidateBoundary rejects (a project boundary whose project_id
// is an empty string, which reads back as the nil UUID) and asserts the step returns nil,
// leaves the row byte-identical, and logs the row's ID.
func TestValidateUserAccessTokenBoundaries_ReadsOnly(t *testing.T) {
	dsn, ids, _, _ := seedOldSchemaDB(t, 1)
	cs, capture := openLegacyStore(t, dsn)
	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))

	userID, _ := seedProjectAndUser(t, cs)
	badID := uuid.NewString()
	_, err := cs.DB().ExecContext(ctx,
		"INSERT INTO user_access_tokens (id, user_id, name, prefix, key_hash, project_id, boundary_kind, scopes, ceiling_version, revoked, created) "+
			"VALUES (?, ?, 'nil-project', 'scion_pat_nil', ?, '', 'project', '[\"agent:read\"]', 1, 0, strftime('%Y-%m-%dT%H:%M:%fZ','now'))",
		badID, userID, uuid.NewString())
	require.NoError(t, err, "the CHECK admits a project row with an empty project_id")

	beforeBad := rawUATRow(t, cs.DB(), badID)
	beforeGood := rawUATRow(t, cs.DB(), ids[0])
	capture.buf.Reset()

	require.NoError(t, cs.ValidateUserAccessTokenBoundaries(ctx))

	assert.Equal(t, beforeBad, rawUATRow(t, cs.DB(), badID), "the step must not write the invalid row")
	assert.Equal(t, beforeGood, rawUATRow(t, cs.DB(), ids[0]), "the step must not write a valid row")
	assert.Equal(t, 2, countUATRows(t, cs.DB()))

	reported, records := capture.invalidTokenIDs(t)
	assert.Equal(t, []string{badID}, reported)
	assert.Equal(t, 1, records)
	assert.NotContains(t, capture.buf.String(), userID, "only token IDs are logged")
}

// TestValidateUserAccessTokenBoundaries_Paginates shrinks the per-store page
// size so the scan spans several pages, and asserts every invalid row is
// reported exactly once, across page boundaries, in one log record.
func TestValidateUserAccessTokenBoundaries_Paginates(t *testing.T) {
	dsn, _, _, _ := seedOldSchemaDB(t, 4)
	var badIDs []string
	for i := 0; i < 3; i++ {
		badIDs = append(badIDs, insertLegacyUATRowInto(t, dsn, ""))
	}
	cs, capture := openLegacyStore(t, dsn)
	cs.uatBoundaryValidatePageSize = 2
	ctx := context.Background()
	require.NoError(t, cs.Migrate(ctx))
	require.Equal(t, 7, countUATRows(t, cs.DB()))

	capture.buf.Reset()
	require.NoError(t, cs.ValidateUserAccessTokenBoundaries(ctx))

	reported, records := capture.invalidTokenIDs(t)
	assert.ElementsMatch(t, badIDs, reported)
	assert.Len(t, reported, len(badIDs), "each invalid row is reported exactly once")
	assert.Equal(t, 1, records)
}

func TestUATBoundaryValidatePageSizeOrDefault(t *testing.T) {
	cs := &CompositeStore{}
	assert.Equal(t, defaultUATBoundaryValidatePageSize, cs.uatBoundaryValidatePageSizeOrDefault())
	cs.uatBoundaryValidatePageSize = -1
	assert.Equal(t, defaultUATBoundaryValidatePageSize, cs.uatBoundaryValidatePageSizeOrDefault())
	cs.uatBoundaryValidatePageSize = 3
	assert.Equal(t, 3, cs.uatBoundaryValidatePageSizeOrDefault())
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
				"VALUES (?, ?, 'bad-hub', 'scion_pat_bad', ?, ?, 'hub', '[]', 0, false, ?)",
			args: []any{uuid.NewString(), userID, uuid.NewString(), projectID, time.Now().UTC()},
		},
		{
			name: "project boundary with a null project_id",
			sql: "INSERT INTO user_access_tokens (id, user_id, name, prefix, key_hash, project_id, boundary_kind, scopes, ceiling_version, revoked, created) " +
				"VALUES (?, ?, 'bad-project', 'scion_pat_bad', ?, NULL, 'project', '[]', 0, false, ?)",
			args: []any{uuid.NewString(), userID, uuid.NewString(), time.Now().UTC()},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// This test runs on Postgres too (enttest.NewClient), so the
			// placeholders are rebound and the error must name the CHECK
			// constraint: any other failure (a syntax or type error) would
			// otherwise pass vacuously.
			_, err := db.ExecContext(ctx, rebindForDialect(cs.Dialect(), c.sql), c.args...)
			require.Error(t, err, "the DB-level CHECK constraint must reject this row even without any Go-level validator in the path")
			require.Contains(t, strings.ToLower(err.Error()), "check constraint",
				"the row must be rejected by the CHECK constraint, not by some other error")
			t.Logf("DB CHECK constraint rejected the invalid row, as required: %v", err)
		})
	}
}
