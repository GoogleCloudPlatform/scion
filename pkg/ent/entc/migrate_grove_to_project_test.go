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

package entc

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockProjectStore implements ProjectSlugResolver for testing.
type mockProjectStore struct {
	projects map[string]*store.Project
}

func (m *mockProjectStore) GetProjectBySlug(_ context.Context, slug string) (*store.Project, error) {
	p, ok := m.projects[slug]
	if !ok {
		return nil, fmt.Errorf("project %q not found", slug)
	}
	return p, nil
}

// setupEntDB creates an in-memory Ent database with the schema applied via
// AutoMigrate, then returns the full SQLite DSN for use with the internal
// migration function.
func setupEntDB(t *testing.T) string {
	t.Helper()
	dbName := t.Name()
	dsn := "file:" + dbName + "?mode=memory&cache=shared"
	client, err := OpenSQLite(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	require.NoError(t, AutoMigrate(context.Background(), client))
	return dsn
}

// rawDB opens a raw SQL connection to the in-memory Ent database for seeding
// test data directly (bypassing Ent's enum validation).
func rawDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// now returns the current time formatted for SQLite.
var now = time.Now().Format(time.RFC3339)

// insertGroup inserts a group with all required fields into the test database.
func insertGroup(t *testing.T, db *sql.DB, id, name, slug, groupType string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO groups (id, name, slug, group_type, created, updated)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, name, slug, groupType, now, now)
	require.NoError(t, err)
}

func TestMigrateGroveToProjectData_RenamesSlugs(t *testing.T) {
	dsn := setupEntDB(t)
	db := rawDB(t, dsn)
	ctx := context.Background()

	// Seed groups with old "grove:" slugs.
	grpID1 := uuid.New().String()
	grpID2 := uuid.New().String()
	insertGroup(t, db, grpID1, "Agents", "grove:myproj:agents", "grove_agents")
	insertGroup(t, db, grpID2, "Members", "grove:myproj:members", "grove_members")

	projectID := uuid.New().String()
	mockStore := &mockProjectStore{
		projects: map[string]*store.Project{
			"myproj": {ID: projectID, Slug: "myproj"},
		},
	}

	err := migrateGroveToProjectDataWithDSN(ctx, dsn, mockStore)
	require.NoError(t, err)

	// Verify slugs were renamed.
	var slug1, slug2 string
	err = db.QueryRowContext(ctx, `SELECT slug FROM groups WHERE id = ?`, grpID1).Scan(&slug1)
	require.NoError(t, err)
	assert.Equal(t, "project:myproj:agents", slug1)

	err = db.QueryRowContext(ctx, `SELECT slug FROM groups WHERE id = ?`, grpID2).Scan(&slug2)
	require.NoError(t, err)
	assert.Equal(t, "project:myproj:members", slug2)
}

func TestMigrateGroveToProjectData_FixesGroupTypes(t *testing.T) {
	dsn := setupEntDB(t)
	db := rawDB(t, dsn)
	ctx := context.Background()

	grpID1 := uuid.New().String()
	grpID2 := uuid.New().String()
	insertGroup(t, db, grpID1, "Agents", "grove:proj1:agents", "grove_agents")
	insertGroup(t, db, grpID2, "Members", "grove:proj1:members", "grove_members")

	mockStore := &mockProjectStore{projects: map[string]*store.Project{
		"proj1": {ID: uuid.New().String(), Slug: "proj1"},
	}}

	err := migrateGroveToProjectDataWithDSN(ctx, dsn, mockStore)
	require.NoError(t, err)

	// Verify group types were fixed.
	var type1, type2 string
	err = db.QueryRowContext(ctx, `SELECT group_type FROM groups WHERE id = ?`, grpID1).Scan(&type1)
	require.NoError(t, err)
	assert.Equal(t, "project_agents", type1)

	err = db.QueryRowContext(ctx, `SELECT group_type FROM groups WHERE id = ?`, grpID2).Scan(&type2)
	require.NoError(t, err)
	assert.Equal(t, "explicit", type2)
}

func TestMigrateGroveToProjectData_MergesDuplicates(t *testing.T) {
	dsn := setupEntDB(t)
	db := rawDB(t, dsn)
	ctx := context.Background()

	oldGroupID := uuid.New().String()
	newGroupID := uuid.New().String()
	userID := uuid.New().String()
	membershipID := uuid.New().String()

	// Create user.
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, display_name, role, status, created)
		VALUES (?, 'user@test.com', 'User', 'member', 'active', ?)
	`, userID, now)
	require.NoError(t, err)

	// Old grove: group with a membership.
	insertGroup(t, db, oldGroupID, "Old Agents", "grove:proj2:agents", "grove_agents")
	_, err = db.ExecContext(ctx, `
		INSERT INTO group_memberships (id, group_id, user_id, role, added_at)
		VALUES (?, ?, ?, 'member', ?)
	`, membershipID, oldGroupID, userID, now)
	require.NoError(t, err)

	// New project: group already exists (no memberships yet).
	insertGroup(t, db, newGroupID, "New Agents", "project:proj2:agents", "project_agents")

	projectID := uuid.New().String()
	mockStore := &mockProjectStore{projects: map[string]*store.Project{
		"proj2": {ID: projectID, Slug: "proj2"},
	}}

	err = migrateGroveToProjectDataWithDSN(ctx, dsn, mockStore)
	require.NoError(t, err)

	// Old group should be deleted.
	var count int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE id = ?`, oldGroupID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// New group should still exist.
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE id = ?`, newGroupID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Membership should now be in the new group.
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_memberships WHERE group_id = ?`, newGroupID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// No memberships for old group.
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_memberships WHERE group_id = ?`, oldGroupID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestMigrateGroveToProjectData_BackfillsProjectID(t *testing.T) {
	dsn := setupEntDB(t)
	db := rawDB(t, dsn)
	ctx := context.Background()

	grpID := uuid.New().String()
	projectID := uuid.New().String()

	// Group with project: slug but no project_id.
	_, err := db.ExecContext(ctx, `
		INSERT INTO groups (id, name, slug, group_type, project_id, created, updated)
		VALUES (?, 'Agents', 'project:backfill-proj:agents', 'project_agents', NULL, ?, ?)
	`, grpID, now, now)
	require.NoError(t, err)

	mockStore := &mockProjectStore{projects: map[string]*store.Project{
		"backfill-proj": {ID: projectID, Slug: "backfill-proj"},
	}}

	err = migrateGroveToProjectDataWithDSN(ctx, dsn, mockStore)
	require.NoError(t, err)

	// Verify project_id was set.
	var gotProjectID string
	err = db.QueryRowContext(ctx, `SELECT project_id FROM groups WHERE id = ?`, grpID).Scan(&gotProjectID)
	require.NoError(t, err)
	assert.Equal(t, projectID, gotProjectID)
}

func TestMigrateGroveToProjectData_Idempotent(t *testing.T) {
	dsn := setupEntDB(t)
	db := rawDB(t, dsn)
	ctx := context.Background()

	grpID := uuid.New().String()
	projectID := uuid.New().String()

	insertGroup(t, db, grpID, "Agents", "grove:idempotent:agents", "grove_agents")

	mockStore := &mockProjectStore{projects: map[string]*store.Project{
		"idempotent": {ID: projectID, Slug: "idempotent"},
	}}

	// Run migration first time.
	err := migrateGroveToProjectDataWithDSN(ctx, dsn, mockStore)
	require.NoError(t, err)

	// Verify first run results.
	var slug string
	err = db.QueryRowContext(ctx, `SELECT slug FROM groups WHERE id = ?`, grpID).Scan(&slug)
	require.NoError(t, err)
	assert.Equal(t, "project:idempotent:agents", slug)

	// Run migration second time — should be a no-op.
	err = migrateGroveToProjectDataWithDSN(ctx, dsn, mockStore)
	require.NoError(t, err)

	// Verify results are unchanged.
	err = db.QueryRowContext(ctx, `SELECT slug FROM groups WHERE id = ?`, grpID).Scan(&slug)
	require.NoError(t, err)
	assert.Equal(t, "project:idempotent:agents", slug)

	// Verify only one group exists (no duplicates created).
	var count int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE slug = 'project:idempotent:agents'`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestMigrateGroveToProjectData_NoOldGroups(t *testing.T) {
	dsn := setupEntDB(t)
	ctx := context.Background()

	mockStore := &mockProjectStore{projects: map[string]*store.Project{}}

	// Migration should succeed with nothing to do.
	err := migrateGroveToProjectDataWithDSN(ctx, dsn, mockStore)
	require.NoError(t, err)
}

func TestParseProjectSlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"project:myproj:agents", "myproj"},
		{"project:myproj:members", "myproj"},
		{"grove:myproj:agents", "myproj"},
		{"invalid-slug", ""},
		{"too:many:parts:here", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseProjectSlug(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}
