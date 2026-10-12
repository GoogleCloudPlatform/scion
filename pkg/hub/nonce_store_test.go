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

package hub

import (
	"context"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

func setupTestNonceCacheStore(t *testing.T) *NonceCacheStore {
	t.Helper()
	client, err := ent.Open("sqlite3", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	err = client.Schema.Create(ctx)
	require.NoError(t, err)

	return NewNonceCacheStore(client)
}

func TestNonceCacheStore_NewNonce(t *testing.T) {
	store := setupTestNonceCacheStore(t)
	ctx := context.Background()

	isNew, err := store.CheckAndStore(ctx, "nonce-abc-123", 10*time.Minute)
	require.NoError(t, err)
	require.True(t, isNew, "first use of a nonce should return true")
}

func TestNonceCacheStore_DuplicateNonce(t *testing.T) {
	store := setupTestNonceCacheStore(t)
	ctx := context.Background()

	// First use should succeed
	isNew, err := store.CheckAndStore(ctx, "nonce-duplicate", 10*time.Minute)
	require.NoError(t, err)
	require.True(t, isNew)

	// Second use of the same nonce should be detected as a replay
	isNew, err = store.CheckAndStore(ctx, "nonce-duplicate", 10*time.Minute)
	require.NoError(t, err)
	require.False(t, isNew, "duplicate nonce should return false (replay detected)")
}

func TestNonceCacheStore_PurgeExpired(t *testing.T) {
	store := setupTestNonceCacheStore(t)
	ctx := context.Background()

	// Insert a nonce with a TTL that has already expired (1 nanosecond)
	isNew, err := store.CheckAndStore(ctx, "nonce-expired", 1*time.Nanosecond)
	require.NoError(t, err)
	require.True(t, isNew)

	// Wait a moment so expires_at is in the past
	time.Sleep(5 * time.Millisecond)

	// Insert another nonce with a long TTL (should survive purge)
	isNew, err = store.CheckAndStore(ctx, "nonce-alive", 10*time.Minute)
	require.NoError(t, err)
	require.True(t, isNew)

	// Purge expired entries
	purged, err := store.PurgeExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, purged, "should have purged exactly 1 expired nonce")

	// The expired nonce should now be gone — re-inserting should succeed
	isNew, err = store.CheckAndStore(ctx, "nonce-expired", 10*time.Minute)
	require.NoError(t, err)
	require.True(t, isNew, "purged nonce should be accepted again")

	// The alive nonce should still be present — re-inserting should fail
	isNew, err = store.CheckAndStore(ctx, "nonce-alive", 10*time.Minute)
	require.NoError(t, err)
	require.False(t, isNew, "non-expired nonce should still be rejected")
}

func TestNonceCacheStore_ConcurrentDuplicateDetection(t *testing.T) {
	store := setupTestNonceCacheStore(t)
	ctx := context.Background()

	const nonce = "nonce-concurrent"
	const goroutines = 20

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
	)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			isNew, err := store.CheckAndStore(ctx, nonce, 10*time.Minute)
			if err != nil {
				// DB contention errors are acceptable but shouldn't happen
				// with SQLite in-memory in a single process.
				t.Logf("concurrent CheckAndStore error: %v", err)
				return
			}
			if isNew {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	require.Equal(t, 1, successes,
		"exactly one goroutine should succeed for a given nonce; got %d", successes)
}

func TestNonceCacheStore_DistinctNonces(t *testing.T) {
	store := setupTestNonceCacheStore(t)
	ctx := context.Background()

	// Different nonces should all be accepted
	for _, nonce := range []string{"aaa", "bbb", "ccc"} {
		isNew, err := store.CheckAndStore(ctx, nonce, 10*time.Minute)
		require.NoError(t, err)
		require.True(t, isNew, "distinct nonce %q should be accepted", nonce)
	}
}
