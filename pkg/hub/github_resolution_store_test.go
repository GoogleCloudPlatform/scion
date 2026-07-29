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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
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

	// Add expired entry
	expiredKey := "expired-key"
	expiredEntry := GitHubCacheEntry{
		CommitSHA:   "abcdef1234567890abcdef1234567890abcdef12",
		FileEntries: []GitHubFileEntry{{Path: "SKILL.md", URL: "http://example.com", Hash: "sha256:abc", Size: 100}},
		BundleHash:  "sha256:bundlehash",
		TokenScope:  "public",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
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

	entries, err := ghListContents(context.Background(), srv.URL, "https://raw.githubusercontent.com",
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

	entries, err := ghListContents(context.Background(), srv.URL, "https://raw.ghe.example.com",
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
