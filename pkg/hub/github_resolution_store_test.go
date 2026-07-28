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
	defer client.Close()

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
	defer client.Close()

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
	defer client.Close()

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
	require.False(t, isFullCommitSHA("abcdef123456")) // Too short
	require.False(t, isFullCommitSHA("ABCDEF1234567890ABCDEF1234567890ABCDEF12")) // Uppercase
	require.False(t, isFullCommitSHA("main")) // Not a SHA
	require.False(t, isFullCommitSHA("")) // Empty
}
