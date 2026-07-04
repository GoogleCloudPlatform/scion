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

package discord

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

// TestAdvisoryLock_DedicatedConnection verifies that the advisory lock is held
// on a dedicated connection that survives pool churn (F1 regression test).
func TestAdvisoryLock_DedicatedConnection(t *testing.T) {
	url := testPostgresURL(t)
	store, err := NewPostgresStore(url)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	key := int64(0x5C10FFFF)

	acquired, handle, err := store.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.True(t, acquired, "should acquire lock")
	require.NotNil(t, handle)

	// Verify lock is held.
	require.NoError(t, handle.Verify(ctx))

	// Open a second store and try to acquire the same lock — should fail.
	store2, err := NewPostgresStore(url)
	require.NoError(t, err)
	defer store2.Close()

	acquired2, _, err := store2.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	assert.False(t, acquired2, "second store should not acquire while first holds")

	// Release and verify the second store can now acquire.
	require.NoError(t, handle.Release())

	acquired3, handle3, err := store2.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	assert.True(t, acquired3, "second store should acquire after release")
	if handle3 != nil {
		handle3.Release()
	}
}

// TestAdvisoryLock_ReleaseActuallyReleases verifies that pg_advisory_unlock
// returns true (actually unlocked) when called on the holding connection.
func TestAdvisoryLock_ReleaseActuallyReleases(t *testing.T) {
	url := testPostgresURL(t)
	store, err := NewPostgresStore(url)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	key := int64(0x5C10FFFE)

	acquired, handle, err := store.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.True(t, acquired)

	err = handle.Release()
	require.NoError(t, err, "release should succeed without error")

	// Re-acquire should work.
	acquired2, handle2, err := store.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	assert.True(t, acquired2, "lock should be acquirable after explicit release")
	if handle2 != nil {
		handle2.Release()
	}
}

// TestAdvisoryLock_ConnTerminationReleasesLock verifies that killing the
// holding connection server-side releases the lock (F2 failover test).
func TestAdvisoryLock_ConnTerminationReleasesLock(t *testing.T) {
	url := testPostgresURL(t)
	store, err := NewPostgresStore(url)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	key := int64(0x5C10FFFD)

	acquired, handle, err := store.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.True(t, acquired)

	// Find the backend PID of the lock-holding connection and terminate it.
	db, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(ctx, `
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE pid IN (
			SELECT pid FROM pg_locks
			WHERE locktype = 'advisory' AND objid = $1 AND granted = true
		)
	`, key)
	require.NoError(t, err)

	// Give Postgres a moment to process termination.
	time.Sleep(200 * time.Millisecond)

	// Verify the handle detects the dead connection.
	err = handle.Verify(ctx)
	assert.Error(t, err, "verify should fail on terminated connection")

	// Another store should now be able to acquire.
	store2, err := NewPostgresStore(url)
	require.NoError(t, err)
	defer store2.Close()

	acquired2, handle2, err := store2.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	assert.True(t, acquired2, "lock should be acquirable after conn termination")
	if handle2 != nil {
		handle2.Release()
	}
}

// TestAdvisoryLock_NotAcquiredReturnsNilHandle verifies that a failed
// acquisition returns nil handle (not an error).
func TestAdvisoryLock_NotAcquiredReturnsNilHandle(t *testing.T) {
	url := testPostgresURL(t)
	store, err := NewPostgresStore(url)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	key := int64(0x5C10FFFC)

	// First acquire.
	acquired, handle, err := store.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.True(t, acquired)
	defer handle.Release()

	// Second acquire from same store (different pool conn) should fail.
	store2, err := NewPostgresStore(url)
	require.NoError(t, err)
	defer store2.Close()

	acquired2, handle2, err := store2.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	assert.False(t, acquired2)
	assert.Nil(t, handle2)
}
