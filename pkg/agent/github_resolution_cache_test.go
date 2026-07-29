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

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGitHubResolutionCache_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	skill := ResolvedSkill{
		Name:    "my-skill",
		URI:     "gh://owner/repo/my-skill@main",
		Version: "abc123def456",
		Hash:    "sha256:deadbeef",
		Files: []ResolvedFile{
			{Path: "SKILL.md", URL: "https://example.com/SKILL.md", Hash: "sha256:abc", Size: 42},
		},
	}

	cache.Put("gh://owner/repo/my-skill@main", skill)

	got, ok := cache.Get("gh://owner/repo/my-skill@main")
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if got.Name != "my-skill" {
		t.Errorf("expected name my-skill, got %s", got.Name)
	}
	if got.Hash != "sha256:deadbeef" {
		t.Errorf("expected hash sha256:deadbeef, got %s", got.Hash)
	}
	if len(got.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(got.Files))
	}
}

func TestGitHubResolutionCache_Miss(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	_, ok := cache.Get("gh://owner/repo/nonexistent@main")
	if ok {
		t.Fatal("expected cache miss, got hit")
	}
}

func TestGitHubResolutionCache_Expiry(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	skill := ResolvedSkill{
		Name: "expiring-skill",
		URI:  "gh://owner/repo/expiring@main",
	}
	cache.Put("gh://owner/repo/expiring@main", skill)

	// Wait for expiry
	time.Sleep(5 * time.Millisecond)

	_, ok := cache.Get("gh://owner/repo/expiring@main")
	if ok {
		t.Fatal("expected cache miss after expiry, got hit")
	}
}

func TestGitHubResolutionCache_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	skill := ResolvedSkill{
		Name:    "persist-skill",
		URI:     "gh://owner/repo/persist@main",
		Version: "abc123def456",
		Hash:    "sha256:persist",
	}
	cache.Put("gh://owner/repo/persist@main", skill)

	// Verify file exists on disk
	cacheFile := filepath.Join(dir, resolutionCacheFileName)
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file not persisted: %v", err)
	}

	// Create a new cache instance from the same directory
	cache2, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache (reload): %v", err)
	}

	got, ok := cache2.Get("gh://owner/repo/persist@main")
	if !ok {
		t.Fatal("expected cache hit after reload, got miss")
	}
	if got.Name != "persist-skill" {
		t.Errorf("expected name persist-skill, got %s", got.Name)
	}
}

// TestGitHubResolutionCache_CredentialEntryNotPersistedToDisk verifies that
// credential-bearing cache keys (those with a "#<tokenhash>" suffix, produced
// by resolutionCacheKey when a GitHub token is present) are kept in-memory
// only and never written to disk.
//
// This prevents the stale-404 bug from issue #565: ResolvedFile.Content is
// json:"-" and is stripped on serialisation. A disk-loaded entry would have
// Content == nil, causing installOneSkill to re-download using the wrong token
// (the broker's default, not the per-URI named credential) and 404 on private
// repos. Memory-only entries retain Content for the full TTL window.
func TestGitHubResolutionCache_CredentialEntryNotPersistedToDisk(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	// A key with a token-hash suffix (simulating a ?token= private-repo URI).
	credKey := "gh://owner/repo/my-skill@main#deadbeef12345678"
	skill := ResolvedSkill{
		Name:    "private-skill",
		URI:     "gh://owner/repo/my-skill@main",
		Version: "abc123def456",
		Hash:    "sha256:private",
		Files: []ResolvedFile{
			{Path: "SKILL.md", Content: []byte("private content")},
		},
	}
	cache.Put(credKey, skill)

	// In-memory Get must hit.
	got, ok := cache.Get(credKey)
	if !ok {
		t.Fatal("expected in-memory cache hit for credential entry, got miss")
	}
	if got.Name != "private-skill" {
		t.Errorf("expected name private-skill, got %s", got.Name)
	}

	// The cache file must not exist (credential entries are never written to disk).
	cacheFile := filepath.Join(dir, resolutionCacheFileName)
	if _, err := os.Stat(cacheFile); err == nil {
		t.Error("credential-bearing entry was persisted to disk; expected memory-only")
	}

	// A new cache instance loading from the same dir must NOT see the credential entry.
	cache2, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache (reload): %v", err)
	}
	if _, ok := cache2.Get(credKey); ok {
		t.Error("credential entry should not be loadable from disk after restart")
	}
}

// TestGitHubResolutionCache_MixedPublicAndCredential verifies that a cache
// containing both public-repo and credential-bearing entries persists only
// the public-repo entries to disk.
func TestGitHubResolutionCache_MixedPublicAndCredential(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	publicKey := "gh://owner/repo/pub-skill@main"
	credKey := "gh://owner/repo/priv-skill@main#deadbeef12345678"

	cache.Put(publicKey, ResolvedSkill{Name: "pub-skill", URI: publicKey})
	cache.Put(credKey, ResolvedSkill{Name: "priv-skill", URI: "gh://owner/repo/priv-skill@main"})

	// Both accessible in-memory.
	if _, ok := cache.Get(publicKey); !ok {
		t.Error("public entry: expected in-memory hit")
	}
	if _, ok := cache.Get(credKey); !ok {
		t.Error("credential entry: expected in-memory hit")
	}

	// After reload, only public entry survives.
	cache2, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache (reload): %v", err)
	}
	if _, ok := cache2.Get(publicKey); !ok {
		t.Error("public entry: expected disk hit after reload")
	}
	if _, ok := cache2.Get(credKey); ok {
		t.Error("credential entry: must not survive reload (should be memory-only)")
	}
}

func TestGitHubResolutionCache_ExpiredNotLoaded(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	skill := ResolvedSkill{Name: "expired-skill", URI: "gh://o/r/s@main"}
	cache.Put("gh://o/r/s@main", skill)

	time.Sleep(5 * time.Millisecond)

	// Reload — expired entries should not be loaded
	cache2, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache (reload): %v", err)
	}

	_, ok := cache2.Get("gh://o/r/s@main")
	if ok {
		t.Fatal("expected expired entry to not be loaded, got hit")
	}
}
