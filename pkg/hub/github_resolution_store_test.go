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
// acceptance test for the purge horizon: a branch-ref row past its TTL but
// still within MaxResolutionStaleAge of its last resolution must survive
// PurgeExpired —
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

// TestGitHubResolutionStore_StaleCutoff_AccountsForTTLJitter confirms
// GetStale and PurgeExpired still agree on one horizon (see staleCutoff) now
// that ExpiresAt is written with a jittered TTL (agent.JitteredTTL, applied
// in fetchAndCacheGitHubSkill): a row placed just inside the jitter-aware
// cutoff survives PurgeExpired and remains stale-servable, and a row just
// outside it does not.
//
// wantCutoff is computed independently of staleCutoff itself — directly from
// agent.MaxJitteredTTL and agent.MaxResolutionStaleAge, the same inputs
// staleCutoff is supposed to combine — rather than by calling staleCutoff and
// asserting around whatever it happens to return: that would make this test
// pass unconditionally, since entries placed relative to staleCutoff's own
// (possibly wrong) output always look internally consistent with it. The ~3
// minute gap between MaxJitteredTTL(30m) and the unjittered 30m is what the
// one-minute margins below are sized to land on either side of.
func TestGitHubResolutionStore_StaleCutoff_AccountsForTTLJitter(t *testing.T) {
	client, err := ent.Open("sqlite3", "file:ent?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer client.Close() //nolint:errcheck
	ctx := context.Background()
	require.NoError(t, client.Schema.Create(ctx))

	store := NewGitHubResolutionStore(client)
	cutoff := time.Now().Add(agent.MaxJitteredTTL(agent.DefaultResolutionCacheTTL) - agent.MaxResolutionStaleAge)

	justInsideKey := "just-inside"
	require.NoError(t, store.Put(ctx, justInsideKey, GitHubCacheEntry{
		CommitSHA:   "3333333333333333333333333333333333333333",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:c", Size: 1}},
		BundleHash:  "sha256:just-inside",
		TokenScope:  "public",
		ExpiresAt:   cutoff.Add(time.Minute),
		OriginalURI: "gh://acme/repo/skill@main",
	}))

	justOutsideKey := "just-outside"
	require.NoError(t, store.Put(ctx, justOutsideKey, GitHubCacheEntry{
		CommitSHA:   "4444444444444444444444444444444444444444",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:d", Size: 1}},
		BundleHash:  "sha256:just-outside",
		TokenScope:  "public",
		ExpiresAt:   cutoff.Add(-time.Minute),
		OriginalURI: "gh://acme/repo/skill@main",
	}))

	require.NoError(t, store.PurgeExpired(ctx))

	_, ok, err := store.GetStale(ctx, justInsideKey, agent.DefaultResolutionCacheTTL, agent.MaxResolutionStaleAge)
	require.NoError(t, err)
	require.True(t, ok, "a row just inside the jitter-aware cutoff must survive PurgeExpired and remain stale-servable")

	_, ok, err = store.GetStale(ctx, justOutsideKey, agent.DefaultResolutionCacheTTL, agent.MaxResolutionStaleAge)
	require.NoError(t, err)
	require.False(t, ok, "a row just outside the jitter-aware cutoff must not be stale-servable, and PurgeExpired must have removed it")
}

// TestComputeCacheKey tests cache key computation.
func TestComputeCacheKey(t *testing.T) {
	key1 := computeCacheKey("owner", "repo", "skills/test", "main", "public")
	key2 := computeCacheKey("owner", "repo", "skills/test", "main", "public")
	require.Equal(t, key1, key2, "same inputs should produce same key")

	key3 := computeCacheKey("owner", "repo", "skills/test", "main", "12345")
	require.NotEqual(t, key1, key3, "different token scope should produce different key")

	key4 := computeCacheKey("Owner", "Repo", "skills/test", "main", "public")
	require.Equal(t, key1, key4, "owner/repo should be lowercased")
}

// TestIsFullCommitSHA tests SHA validation.
func TestIsFullCommitSHA(t *testing.T) {
	require.True(t, isFullCommitSHA("abcdef1234567890abcdef1234567890abcdef12"))
	require.True(t, isFullCommitSHA("0000000000000000000000000000000000000000"))
	require.False(t, isFullCommitSHA("abcdef123456"))                             // Too short
	require.False(t, isFullCommitSHA("ABCDEF1234567890ABCDEF1234567890ABCDEF12")) // Uppercase
	require.False(t, isFullCommitSHA("main"))                                     // Not a SHA
	require.False(t, isFullCommitSHA(""))                                         // Empty
}

func TestGHRawContentURL(t *testing.T) {
	cases := []struct {
		name      string
		rawBase   string
		owner     string
		repo      string
		commitSHA string
		filePath  string
		want      string
	}{
		{
			name:      "simple path",
			rawBase:   "https://raw.githubusercontent.com",
			owner:     "acme",
			repo:      "skills",
			commitSHA: "abcdef1234567890abcdef1234567890abcdef12",
			filePath:  "skills/deploy/SKILL.md",
			want:      "https://raw.githubusercontent.com/acme/skills/abcdef1234567890abcdef1234567890abcdef12/skills/deploy/SKILL.md",
		},
		{
			name:      "trailing slash on base is not doubled",
			rawBase:   "https://raw.githubusercontent.com/",
			owner:     "acme",
			repo:      "skills",
			commitSHA: "abcdef1234567890abcdef1234567890abcdef12",
			filePath:  "SKILL.md",
			want:      "https://raw.githubusercontent.com/acme/skills/abcdef1234567890abcdef1234567890abcdef12/SKILL.md",
		},
		{
			name:      "spaces are escaped but separators are preserved",
			rawBase:   "https://raw.githubusercontent.com",
			owner:     "acme",
			repo:      "skills",
			commitSHA: "abcdef1234567890abcdef1234567890abcdef12",
			filePath:  "my skills/read me.md",
			want:      "https://raw.githubusercontent.com/acme/skills/abcdef1234567890abcdef1234567890abcdef12/my%20skills/read%20me.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ghRawContentURL(tc.rawBase, tc.owner, tc.repo, tc.commitSHA, tc.filePath)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestGHListContents_PermanentURLs is the regression test for the expiring
// download URL defect: the Contents API hands back a CDN link carrying a
// short-lived token for private repos, which would be dead long before this
// entry's cache TTL elapses. The stored URL must instead be built from the
// pinned commit SHA.
func TestGHListContents_PermanentURLs(t *testing.T) {
	const commitSHA = "abcdef1234567890abcdef1234567890abcdef12"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/acme/skills/contents/skills/deploy", r.URL.Path)
		require.Equal(t, commitSHA, r.URL.Query().Get("ref"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"SKILL.md","path":"skills/deploy/SKILL.md","sha":"ce013625030ba8dba906f756967f9e9ca394464a","size":6,"type":"file",
			 "download_url":"https://raw.githubusercontent.com/acme/skills/` + commitSHA + `/skills/deploy/SKILL.md?token=EXPIRES_SOON"},
			{"name":"helper.py","path":"skills/deploy/helper.py","sha":"95d09f2b10159347eece71399a7e2e907ea3df4f","size":11,"type":"file",
			 "download_url":"https://raw.githubusercontent.com/acme/skills/` + commitSHA + `/skills/deploy/helper.py?token=EXPIRES_SOON"},
			{"name":"nested","path":"skills/deploy/nested","sha":"1111111111111111111111111111111111111111","size":0,"type":"dir",
			 "download_url":null}
		]`))
	}))
	defer srv.Close()

	entries, err := ghListContents(context.Background(), agent.NewGitHubCooldown(nil), "anon", srv.URL, "https://raw.githubusercontent.com",
		"acme", "skills", "skills/deploy", commitSHA, "")
	require.NoError(t, err)

	// Directories are skipped.
	require.Len(t, entries, 2)

	require.Equal(t, "SKILL.md", entries[0].Path)
	require.Equal(t,
		"https://raw.githubusercontent.com/acme/skills/"+commitSHA+"/skills/deploy/SKILL.md",
		entries[0].URL)
	require.Equal(t, "ce013625030ba8dba906f756967f9e9ca394464a", entries[0].Hash)
	require.Equal(t, int64(6), entries[0].Size)

	require.Equal(t, "helper.py", entries[1].Path)
	require.Equal(t,
		"https://raw.githubusercontent.com/acme/skills/"+commitSHA+"/skills/deploy/helper.py",
		entries[1].URL)

	for _, e := range entries {
		require.NotContains(t, e.URL, "token=", "stored URL must not carry an expiring CDN token")
	}
}

// TestGHListContents_RawBaseOverride confirms the raw origin is configurable,
// which is what lets a GitHub Enterprise deployment work.
func TestGHListContents_RawBaseOverride(t *testing.T) {
	const commitSHA = "abcdef1234567890abcdef1234567890abcdef12"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"SKILL.md","path":"s/SKILL.md","sha":"ce013625030ba8dba906f756967f9e9ca394464a","size":6,"type":"file","download_url":"https://example.invalid/x"}]`))
	}))
	defer srv.Close()

	entries, err := ghListContents(context.Background(), agent.NewGitHubCooldown(nil), "anon", srv.URL, "https://raw.ghe.example.com",
		"acme", "skills", "s", commitSHA, "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t,
		"https://raw.ghe.example.com/acme/skills/"+commitSHA+"/s/SKILL.md",
		entries[0].URL)
}

// TestComputeCacheKey_EmptyRefDiffersFromHEAD documents why resolveGitHubSkill
// must default an omitted ref before computing the cache key: the two spellings
// resolve to the same commit but key differently, so leaving the ref empty
// would halve the hit rate for every unpinned gh:// URI.
func TestComputeCacheKey_EmptyRefDiffersFromHEAD(t *testing.T) {
	empty := computeCacheKey("acme", "skills", "skills/deploy", "", "public")
	head := computeCacheKey("acme", "skills", "skills/deploy", "HEAD", "public")
	require.NotEqual(t, empty, head)
}
