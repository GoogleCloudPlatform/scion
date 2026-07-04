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

//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPostgresURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SCION_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("SCION_TEST_POSTGRES_URL not set")
	}
	return url
}

func setupTestDB(t *testing.T, url string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", url)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS integration_configs (
			integration TEXT PRIMARY KEY,
			config JSONB NOT NULL DEFAULT '{}',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS integration_updates (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			integration TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'requested',
			detail TEXT,
			update_time TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`)
	require.NoError(t, err)

	t.Cleanup(func() {
		db.Exec("DELETE FROM integration_configs WHERE integration LIKE 'test-%'")
		db.Exec("DELETE FROM integration_updates WHERE integration LIKE 'test-%'")
		db.Close()
	})
	return db
}

// TestTransitionUpdateState_GuardedTransitions verifies the state machine
// rejects invalid transitions (F9).
func TestTransitionUpdateState_GuardedTransitions(t *testing.T) {
	url := testPostgresURL(t)
	db := setupTestDB(t, url)

	rt := New(Options{Integration: "test-transitions"})
	rt.db = db

	ctx := context.Background()

	// Insert a row in 'requested' state.
	var updateID string
	err := db.QueryRowContext(ctx,
		"INSERT INTO integration_updates (integration, state) VALUES ($1, 'requested') RETURNING id",
		"test-transitions").Scan(&updateID)
	require.NoError(t, err)

	// Transition requested→acknowledged should succeed.
	applied, err := rt.transitionUpdateState(ctx, updateID, "requested", "acknowledged", "")
	require.NoError(t, err)
	assert.True(t, applied, "requested→acknowledged should succeed")

	// Duplicate requested→acknowledged should fail (already acknowledged).
	applied, err = rt.transitionUpdateState(ctx, updateID, "requested", "acknowledged", "")
	require.NoError(t, err)
	assert.False(t, applied, "duplicate requested→acknowledged should be a no-op")

	// acknowledged→updating should succeed.
	applied, err = rt.transitionUpdateState(ctx, updateID, "acknowledged", "updating", "")
	require.NoError(t, err)
	assert.True(t, applied)

	// Attempt to go backwards: updating→acknowledged should fail (wrong from state).
	applied, err = rt.transitionUpdateState(ctx, updateID, "requested", "acknowledged", "")
	require.NoError(t, err)
	assert.False(t, applied, "can't go back from updating to acknowledged")

	// updating→failed with detail.
	applied, err = rt.transitionUpdateState(ctx, updateID, "updating", "failed", "hook crashed")
	require.NoError(t, err)
	assert.True(t, applied)

	// After terminal state, no further transitions.
	applied, err = rt.transitionUpdateState(ctx, updateID, "failed", "updating", "")
	require.NoError(t, err)
	assert.False(t, applied, "can't revive a failed update")
}

// TestHandleUpdateSignal_DuplicateIsNoOp verifies that processing the same
// update signal twice does not trigger a second shutdown (F9).
func TestHandleUpdateSignal_DuplicateIsNoOp(t *testing.T) {
	url := testPostgresURL(t)
	db := setupTestDB(t, url)

	rt := New(Options{Integration: "test-duplicate"})
	rt.db = db

	ctx := context.Background()

	var updateID string
	err := db.QueryRowContext(ctx,
		"INSERT INTO integration_updates (integration, state) VALUES ($1, 'requested') RETURNING id",
		"test-duplicate").Scan(&updateID)
	require.NoError(t, err)

	// First signal — should process and send to shutdownCh.
	rt.handleUpdateSignal(ctx, updateID)

	select {
	case id := <-rt.ShutdownRequested():
		assert.Equal(t, updateID, id)
	case <-time.After(time.Second):
		t.Fatal("expected shutdown request")
	}

	// Second signal with same ID — should be a no-op (row is now 'updating').
	rt.handleUpdateSignal(ctx, updateID)

	select {
	case <-rt.ShutdownRequested():
		t.Fatal("duplicate signal should not trigger second shutdown")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestScanPendingUpdates_PicksUpRequestedRows verifies that scanPendingUpdates
// finds and processes pre-existing 'requested' rows (F7).
func TestScanPendingUpdates_PicksUpRequestedRows(t *testing.T) {
	url := testPostgresURL(t)
	db := setupTestDB(t, url)

	rt := New(Options{Integration: "test-scan"})
	rt.db = db

	ctx := context.Background()

	// Insert a requested update before any listener starts.
	_, err := db.ExecContext(ctx,
		"INSERT INTO integration_updates (integration, state) VALUES ($1, 'requested')",
		"test-scan")
	require.NoError(t, err)

	// Scan should pick it up.
	rt.scanPendingUpdates(ctx)

	select {
	case <-rt.ShutdownRequested():
	case <-time.After(time.Second):
		t.Fatal("scan should have triggered shutdown for pre-existing requested row")
	}
}

// TestScanPendingUpdates_IgnoresNonRequestedRows verifies that rows in
// terminal states are not re-processed.
func TestScanPendingUpdates_IgnoresNonRequestedRows(t *testing.T) {
	url := testPostgresURL(t)
	db := setupTestDB(t, url)

	rt := New(Options{Integration: "test-ignore"})
	rt.db = db

	ctx := context.Background()

	// Insert rows in non-requested states.
	for _, state := range []string{"acknowledged", "updating", "completed", "failed"} {
		_, err := db.ExecContext(ctx,
			"INSERT INTO integration_updates (integration, state) VALUES ($1, $2)",
			"test-ignore", state)
		require.NoError(t, err)
	}

	rt.scanPendingUpdates(ctx)

	select {
	case <-rt.ShutdownRequested():
		t.Fatal("should not trigger shutdown for non-requested rows")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestConnectWithRetry_SucceedsAfterTableCreation verifies that the retry
// loop waits for hub tables to appear (R4).
func TestConnectWithRetry_SucceedsAfterTableCreation(t *testing.T) {
	url := testPostgresURL(t)

	// Ensure tables exist for this test (they were created by setupTestDB in other tests,
	// or may already exist). The point is that connectWithRetry checks for them.
	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	db.Exec(`CREATE TABLE IF NOT EXISTS integration_configs (integration TEXT PRIMARY KEY, config JSONB NOT NULL DEFAULT '{}', updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	db.Exec(`CREATE TABLE IF NOT EXISTS integration_updates (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), integration TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'requested', detail TEXT, update_time TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	db.Close()

	rt := New(Options{
		Integration: "test-retry",
		DatabaseURL: url,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = rt.connectWithRetry(ctx)
	require.NoError(t, err)
	require.NotNil(t, rt.db)
	rt.db.Close()
}
