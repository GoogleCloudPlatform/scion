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

package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/githubresolutioncache"
)

// TestGitHubResolutionStore_GetPut tests basic cache operations.
func TestGitHubResolutionStore_GetPut(t *testing.T) {
	client, err := ent.Open("sqlite3", "file:ent?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ctx := context.Background()
	err = client.Schema.Create(ctx)
	require.NoError(t, err)

	store := NewGitHubResolutionStore(client)

	cacheKey := "test-cache-key-123"
	entry := GitHubCacheEntry{
		CommitSHA: "abcdef1234567890abcdef1234567890abcdef12",
		FileEntries: []GitHubFileEntry{
			{Path: "SKILL.md", URL: "https://raw.githubusercontent.com/test/repo/main/SKILL.md", Hash: "sha256:abc", Size: 100},
		},
		BundleHash:  "sha256:bundlehash",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		OriginalURI: "gh://test/repo/skill",
	}

	// Put entry
	err = store.Put(ctx, cacheKey, entry)
	require.NoError(t, err)

	// Get entry (should hit)
	retrieved, hit, err := store.Get(ctx, cacheKey)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, entry.CommitSHA, retrieved.CommitSHA)
	require.Equal(t, entry.BundleHash, retrieved.BundleHash)
	require.Len(t, retrieved.FileEntries, 1)

	// Get non-existent entry (should miss)
	_, hit, err = store.Get(ctx, "nonexistent")
	require.NoError(t, err)
	require.False(t, hit)
}

// TestGitHubResolutionStore_Put_UpsertUpdatesExistingRow is the repro for the
// unqualified-upsert defect: Put must update the existing row for a
// cache_key it has already written, not fail or silently insert a duplicate.
// This exercises the actual upsert path end to end (SQLite accepts the
// unqualified "ON CONFLICT DO UPDATE" the code used to emit, by inferring the
// lone eligible unique index, which is why this symptom never reproduced
// against SQLite — see TestGitHubResolutionStore_Put_ConflictTargetInSQL for
// the generated-SQL assertion that would catch it on a dialect that doesn't).
func TestGitHubResolutionStore_Put_UpsertUpdatesExistingRow(t *testing.T) {
	client, err := ent.Open("sqlite3", "file:ent?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ctx := context.Background()
	err = client.Schema.Create(ctx)
	require.NoError(t, err)

	store := NewGitHubResolutionStore(client)
	cacheKey := "upsert-key"

	first := GitHubCacheEntry{
		CommitSHA:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com/a", Hash: "sha256:a", Size: 1}},
		BundleHash:  "sha256:first",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		OriginalURI: "gh://test/repo/skill",
	}
	require.NoError(t, store.Put(ctx, cacheKey, first))

	second := first
	second.CommitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	second.BundleHash = "sha256:second"
	require.NoError(t, store.Put(ctx, cacheKey, second))

	retrieved, hit, err := store.Get(ctx, cacheKey)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, second.CommitSHA, retrieved.CommitSHA, "upsert must update the existing row, not leave the first value in place")
	require.Equal(t, second.BundleHash, retrieved.BundleHash)

	// Exactly one row for this cache_key: a conflict-target-less upsert that
	// instead fell back to always inserting (the failure mode this guards
	// against) would leave two. Filtered by cache_key, not a bare Count(),
	// since this DSN (file:ent?mode=memory&cache=shared) is shared across the
	// package's tests and could otherwise pick up rows left by another test.
	count, err := client.GitHubResolutionCache.Query().
		Where(githubresolutioncache.CacheKeyEQ(cacheKey)).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

// TestGitHubResolutionStore_Put_ConflictTargetInSQL asserts on the SQL Put
// generates, rather than against a live Postgres instance: the repo's T1 CI
// job (pkg/store/integrationtest and pkg/store/entadapter only, see
// .github/workflows/ci.yml) does not cover pkg/hub, so there is no Postgres
// harness here to run an integration test against. ent's debug driver lets
// the test capture the exact statement without a real Postgres connection.
//
// The defect: Put used OnConflict().UpdateNewValues(), which omits a conflict
// target. Postgres rejects "INSERT ... ON CONFLICT DO UPDATE" outright
// without one ("ON CONFLICT DO UPDATE requires inference specification or
// constraint name") — every write failed there, silently, because the error
// was only logged as a WARN by the caller. The fix adds
// OnConflictColumns(cache_key), which must appear in the generated statement
// on every dialect, SQLite included.
func TestGitHubResolutionStore_Put_ConflictTargetInSQL(t *testing.T) {
	var captured []string
	client, err := ent.Open("sqlite3", "file:ent?mode=memory&cache=shared&_fk=1",
		ent.Log(func(args ...any) { captured = append(captured, fmt.Sprint(args...)) }),
		ent.Debug())
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, client.Schema.Create(ctx))

	store := NewGitHubResolutionStore(client)
	entry := GitHubCacheEntry{
		CommitSHA:   "abcdef1234567890abcdef1234567890abcdef12",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:abc", Size: 100}},
		BundleHash:  "sha256:bundlehash",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		OriginalURI: "gh://test/repo/skill",
	}
	require.NoError(t, store.Put(ctx, "conflict-target-key", entry))

	var insertStmt string
	for _, line := range captured {
		if strings.Contains(line, "INSERT INTO") && strings.Contains(line, "github_resolution_cache") {
			insertStmt = line
			break
		}
	}
	require.NotEmpty(t, insertStmt, "expected an INSERT statement against github_resolution_cache to be logged")
	require.Contains(t, insertStmt, "ON CONFLICT", "upsert must use ON CONFLICT")
	require.Contains(t, insertStmt, "cache_key", "the conflict target must name cache_key explicitly — a bare \"ON CONFLICT DO UPDATE\" is rejected by Postgres")
	// The conflict target must appear between ON CONFLICT and DO UPDATE, not
	// merely somewhere later in the SET clause (every column is in the SET
	// clause via UpdateNewValues, including cache_key itself).
	conflictIdx := strings.Index(insertStmt, "ON CONFLICT")
	doUpdateIdx := strings.Index(insertStmt, "DO UPDATE")
	require.True(t, conflictIdx >= 0 && doUpdateIdx > conflictIdx, "expected ON CONFLICT ... DO UPDATE in %q", insertStmt)
	target := insertStmt[conflictIdx:doUpdateIdx]
	require.Contains(t, target, "cache_key", "conflict target (between ON CONFLICT and DO UPDATE) must name cache_key: got %q", target)
}

// TestGitHubResolutionStore_Expiration tests TTL expiration.
func TestGitHubResolutionStore_Expiration(t *testing.T) {
	client, err := ent.Open("sqlite3", "file:ent?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ctx := context.Background()
	err = client.Schema.Create(ctx)
	require.NoError(t, err)

	store := NewGitHubResolutionStore(client)

	cacheKey := "test-expired-key"
	entry := GitHubCacheEntry{
		CommitSHA:   "abcdef1234567890abcdef1234567890abcdef12",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:abc", Size: 100}},
		BundleHash:  "sha256:bundlehash",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(-1 * time.Hour), // Already expired
		OriginalURI: "gh://test/repo/skill",
	}

	// Put expired entry
	err = store.Put(ctx, cacheKey, entry)
	require.NoError(t, err)

	// Get should miss (expired)
	_, hit, err := store.Get(ctx, cacheKey)
	require.NoError(t, err)
	require.False(t, hit, "expired entry should not be returned")
}

// TestGitHubResolutionStore_PurgeExpired tests TTL eviction.
func TestGitHubResolutionStore_PurgeExpired(t *testing.T) {
	client, err := ent.Open("sqlite3", "file:ent?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ctx := context.Background()
	err = client.Schema.Create(ctx)
	require.NoError(t, err)

	store := NewGitHubResolutionStore(client)

	// Add an entry past staleCutoff for a branch ref — GetStale could never
	// serve this one stale again, under any ref type, so it is safe to purge.
	expiredKey := "expired-key"
	expiredEntry := GitHubCacheEntry{
		CommitSHA:   "abcdef1234567890abcdef1234567890abcdef12",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:abc", Size: 100}},
		BundleHash:  "sha256:bundlehash",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(-(agent.MaxResolutionStaleAge + time.Hour)),
		OriginalURI: "gh://expired/repo/skill",
	}
	err = store.Put(ctx, expiredKey, expiredEntry)
	require.NoError(t, err)

	// Add valid entry
	validKey := "valid-key"
	validEntry := GitHubCacheEntry{
		CommitSHA:   "1234567890abcdef1234567890abcdef12345678",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:def", Size: 200}},
		BundleHash:  "sha256:bundlehash2",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		OriginalURI: "gh://valid/repo/skill",
	}
	err = store.Put(ctx, validKey, validEntry)
	require.NoError(t, err)

	// Purge expired
	err = store.PurgeExpired(ctx)
	require.NoError(t, err)

	// Expired entry should be gone
	_, hit, err := store.Get(ctx, expiredKey)
	require.NoError(t, err)
	require.False(t, hit)

	// Valid entry should still exist
	_, hit, err = store.Get(ctx, validKey)
	require.NoError(t, err)
	require.True(t, hit)
}

// TestGitHubResolutionStore_PurgeExpired_KeepsStaleServableBranchRow is the
// acceptance test for R5-1: a branch-ref row past its TTL but still within
// MaxResolutionStaleAge of its last resolution must survive PurgeExpired —
// otherwise the hub's 10-minute eviction tick would delete it long before
// GetStale's 24h stale-serve window actually ends, leaving GetStale with
// nothing to serve during exactly the outage it exists to absorb. A second
// row past MaxResolutionStaleAge confirms PurgeExpired still deletes rows
// that are genuinely beyond anyone's stale horizon.
func TestGitHubResolutionStore_PurgeExpired_KeepsStaleServableBranchRow(t *testing.T) {
	client, err := ent.Open("sqlite3", "file:ent?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck

	ctx := context.Background()
	require.NoError(t, client.Schema.Create(ctx))

	store := NewGitHubResolutionStore(client)

	// A branch row whose TTL expired an hour ago: well past ExpiresAt, but
	// its last resolution (ExpiresAt - DefaultResolutionCacheTTL) is nowhere
	// near MaxResolutionStaleAge (24h) ago. GetStale must still be able to
	// serve this one.
	staleServableKey := "stale-servable-key"
	require.NoError(t, store.Put(ctx, staleServableKey, GitHubCacheEntry{
		CommitSHA:   "1111111111111111111111111111111111111111",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:a", Size: 1}},
		BundleHash:  "sha256:stale-servable",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(-time.Hour),
		OriginalURI: "gh://acme/repo/skill@main",
	}))

	// A branch row whose last resolution is well past MaxResolutionStaleAge:
	// GetStale could never serve this one again, so purging it is correct.
	tooOldKey := "too-old-key"
	require.NoError(t, store.Put(ctx, tooOldKey, GitHubCacheEntry{
		CommitSHA:   "2222222222222222222222222222222222222222",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:b", Size: 1}},
		BundleHash:  "sha256:too-old",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(-(agent.MaxResolutionStaleAge + time.Hour)),
		OriginalURI: "gh://acme/repo/skill@main",
	}))

	require.NoError(t, store.PurgeExpired(ctx))

	_, hit, err := store.Get(ctx, staleServableKey)
	require.NoError(t, err)
	require.False(t, hit, "the row is past its TTL, so a fresh Get must miss")
	stale, ok, err := store.GetStale(ctx, staleServableKey, agent.DefaultResolutionCacheTTL, agent.MaxResolutionStaleAge)
	require.NoError(t, err)
	require.True(t, ok, "a branch row within MaxResolutionStaleAge must survive PurgeExpired and remain stale-servable")
	require.Equal(t, "1111111111111111111111111111111111111111", stale.CommitSHA)

	_, ok, err = store.GetStale(ctx, tooOldKey, agent.DefaultResolutionCacheTTL, agent.MaxResolutionStaleAge)
	require.NoError(t, err)
	require.False(t, ok, "a row past MaxResolutionStaleAge must not survive PurgeExpired")
}
