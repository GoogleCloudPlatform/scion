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
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unreadableStamp is time.Time.String() for Asia/Kathmandu: the zone
// abbreviation is a four-digit numeric offset, which the SQLite driver
// cannot scan back.
const unreadableStamp = "2026-10-01 14:45:00 +0545 +0545"

type repairFixture struct {
	cfg       *config.GlobalConfig
	dbPath    string
	userID    string
	agentID   string
	projectID string
	tokenID   string
}

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite"
	cfg.Database.URL = filepath.Join(dir, "hub.db")
	return &repairFixture{cfg: cfg, dbPath: cfg.Database.URL}
}

func (f *repairFixture) boot(t *testing.T) store.Store {
	t.Helper()
	s, entClient, err := initStore(context.Background(), f.cfg)
	require.NoError(t, err, "initStore")
	t.Cleanup(func() { _ = entClient.Close() })
	return s
}

func (f *repairFixture) snapshots(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(f.dbPath + "." + utcTimestampSnapshotLabel + "-*.bak")
	require.NoError(t, err)
	return m
}

// seed creates one row in each of users, agents and user_access_tokens
// (maintenance_operations is seeded by Migrate).
func (f *repairFixture) seed(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	f.projectID = uuid.NewString()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: f.projectID, Name: "tz-repair", Slug: "tz-repair-" + f.projectID[:8],
	}))
	f.userID = uuid.NewString()
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: f.userID, Email: f.userID + "@example.com", DisplayName: "u",
		Role: "member", Status: store.UserStatusActive,
	}))
	f.agentID = uuid.NewString()
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: f.agentID, Name: "tz-agent", Slug: "tz-agent-" + f.agentID[:8], ProjectID: f.projectID,
	}))
	f.tokenID = uuid.NewString()
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID: f.tokenID, UserID: f.userID, Name: "t", Prefix: "scion_pat_tz",
		KeyHash: uuid.NewString(), BoundaryKind: "project", ProjectID: f.projectID,
		Scopes: []string{"agent:read"}, Created: time.Now(),
	}))
}

// corrupt writes unreadableStamp into a time column of users,
// maintenance_operations, agents and user_access_tokens with raw SQL, as a
// writer without .UTC() did on a hub in a +05:45 zone.
func (f *repairFixture) corrupt(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"UPDATE users SET created = ? WHERE id = '" + f.userID + "'",
		"UPDATE maintenance_operations SET created = ?",
		"UPDATE agents SET created = ? WHERE id = '" + f.agentID + "'",
		"UPDATE user_access_tokens SET created = ? WHERE id = '" + f.tokenID + "'",
	} {
		res, err := db.Exec(q, unreadableStamp)
		require.NoError(t, err, q)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.NotZero(t, n, q)
	}
}

func (f *repairFixture) readAll(ctx context.Context, s store.Store) error {
	if _, err := s.ListMaintenanceOperations(ctx); err != nil {
		return err
	}
	if _, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{}); err != nil {
		return err
	}
	if _, err := s.GetAgent(ctx, f.agentID); err != nil {
		return err
	}
	_, err := s.ListUserAccessTokens(ctx, f.userID)
	return err
}

func TestUTCTimestampRepair_BootRepairsUnreadableTables(t *testing.T) {
	ctx := context.Background()
	f := newRepairFixture(t)

	// Boot 1: a fresh database (no hub_settings table when the probe runs)
	// is skipped cleanly.
	logs, restore := captureSlog(t)
	defer restore()
	s := f.boot(t)
	assert.Empty(t, f.snapshots(t), "a fresh database must not be snapshotted")
	assert.NotContains(t, logs.String(), "UTC timestamp repair")
	f.seed(t, s)
	f.corrupt(t, s.(timestampRepairDB).DB())
	require.Error(t, f.readAll(ctx, s), "fixture must make ent reads fail")
	require.NoError(t, s.Close())

	// Without the repair, Store.Migrate fails on these rows; this is why the
	// repair runs before migrateStore and not in runBootDataMigrations.
	unrepaired := filepath.Join(filepath.Dir(f.dbPath), "unrepaired.db")
	require.NoError(t, copyFile(f.dbPath, unrepaired))
	c, err := entc.OpenSQLite("file:"+unrepaired, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	migErr := entadapter.NewCompositeStore(c).Migrate(ctx)
	require.Error(t, migErr, "Migrate must fail on unreadable rows")
	assert.Contains(t, migErr.Error(), "Scan error")
	_ = c.Close()

	// Boot 2 repairs the tables, snapshots first, and sets the marker.
	logs.Reset()
	s = f.boot(t)
	snaps := f.snapshots(t)
	require.Len(t, snaps, 1, logs.String())
	require.NoError(t, f.readAll(ctx, s), "the repaired tables must scan")
	done, err := IsMigrationComplete(ctx, s, MigrationUTCTimestampRepair)
	require.NoError(t, err)
	assert.True(t, done, "marker must be set after a completed repair")
	out := logs.String()
	assert.Contains(t, out, snaps[0], "the snapshot path must be logged")
	assert.Contains(t, out, "users")
	assert.Contains(t, out, "maintenance_operations")
	assert.Contains(t, out, "agents")
	assert.Contains(t, out, "user_access_tokens")
	assert.NotContains(t, out, "+0545", "a stored value reached the log")

	// The snapshot holds the database as it was before the repair.
	snapDB, err := sql.Open("sqlite", "file:"+snaps[0]+"?mode=ro")
	require.NoError(t, err)
	var created string
	require.NoError(t, snapDB.QueryRow("SELECT CAST(created AS TEXT) FROM users WHERE id = ?", f.userID).Scan(&created))
	assert.Equal(t, unreadableStamp, created)
	_ = snapDB.Close()
	require.NoError(t, s.Close())

	// Boot 3 does nothing: the probe finds nothing, no new snapshot.
	logs.Reset()
	s = f.boot(t)
	assert.Equal(t, snaps, f.snapshots(t), "a second boot must not snapshot again")
	assert.NotContains(t, logs.String(), "UTC timestamp repair")
	require.NoError(t, f.readAll(ctx, s))
	done, err = IsMigrationComplete(ctx, s, MigrationUTCTimestampRepair)
	require.NoError(t, err)
	assert.True(t, done)
}

func TestUTCTimestampRepair_SnapshotFailureWritesNothing(t *testing.T) {
	ctx := context.Background()
	f := newRepairFixture(t)
	s := f.boot(t)
	f.seed(t, s)
	db := s.(timestampRepairDB).DB()
	f.corrupt(t, db)

	orig := snapshotSQLite
	snapshotSQLite = func(context.Context, *sql.DB, string, time.Time) (string, error) {
		return "", errors.New("no space left on device")
	}
	t.Cleanup(func() { snapshotSQLite = orig })

	logs, restore := captureSlog(t)
	defer restore()
	res := repairUnreadableTimestamps(ctx, s.(timestampRepairDB))
	assert.False(t, res.Completed)
	assert.Contains(t, logs.String(), "refusing to write")
	var created string
	require.NoError(t, db.QueryRow("SELECT CAST(created AS TEXT) FROM users WHERE id = ?", f.userID).Scan(&created))
	assert.Equal(t, unreadableStamp, created, "nothing may be written when the snapshot fails")
	assert.Empty(t, f.snapshots(t))
}

func TestUTCTimestampRepair_SkipsNonSQLite(t *testing.T) {
	ctx := context.Background()
	f := newRepairFixture(t)
	s := f.boot(t)
	f.seed(t, s)
	db := s.(timestampRepairDB).DB()
	f.corrupt(t, db)

	// A non-SQLite dialect is skipped without touching the database.
	res := repairUnreadableTimestamps(ctx, fakeRepairDB{db: db, dialect: "postgres"})
	assert.False(t, res.Completed)
	var created string
	require.NoError(t, db.QueryRow("SELECT CAST(created AS TEXT) FROM users WHERE id = ?", f.userID).Scan(&created))
	assert.Equal(t, unreadableStamp, created)
}

func TestUTCTimestampRepair_BoundedLog(t *testing.T) {
	logs, restore := captureSlog(t)
	defer restore()
	b := &boundedRepairLog{}
	for i := 0; i < maxBootLogErrors+5; i++ {
		_, _ = b.Write([]byte("unparseable value left unchanged: table=users column=created rowid=1\n"))
	}
	_, _ = b.Write([]byte("done: rewrote 0 values, 15 unparseable values left unchanged\n"))
	b.flush("UTC timestamp repair")
	out := logs.String()
	assert.Equal(t, maxBootLogErrors, strings.Count(out, "unparseable value left unchanged: table"))
	assert.Contains(t, out, "count=5")
	assert.Contains(t, out, "done: rewrote 0 values")
}

type fakeRepairDB struct {
	db      *sql.DB
	dialect string
}

func (f fakeRepairDB) DB() *sql.DB     { return f.db }
func (f fakeRepairDB) Dialect() string { return f.dialect }
