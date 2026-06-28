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
