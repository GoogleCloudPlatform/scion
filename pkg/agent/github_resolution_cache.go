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
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	// DefaultResolutionCacheTTL is how long a cached resolution result is
	// considered fresh. GitHub content can change, so this is a balance
	// between freshness and avoiding rate limits.
	DefaultResolutionCacheTTL = 5 * time.Minute

	resolutionCacheFileName = "github-resolution-cache.json"
)

// GitHubResolutionCache caches the mapping from skill URI → ResolvedSkill
// to avoid redundant GitHub API calls during repeated provisioning.
// Entries expire after a configurable TTL.
type GitHubResolutionCache struct {
	mu       sync.RWMutex
	dir      string
	ttl      time.Duration
	entries  map[string]*resolutionCacheEntry
	filePath string
}

type resolutionCacheEntry struct {
	Skill     ResolvedSkill `json:"skill"`
	CachedAt  time.Time     `json:"cachedAt"`
	ExpiresAt time.Time     `json:"expiresAt"`
}

type resolutionCacheFile struct {
	Entries map[string]*resolutionCacheEntry `json:"entries"`
}

// NewGitHubResolutionCache creates or loads a resolution cache at the
// given directory with the specified TTL.
func NewGitHubResolutionCache(dir string, ttl time.Duration) (*GitHubResolutionCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	c := &GitHubResolutionCache{
		dir:      dir,
		ttl:      ttl,
		entries:  make(map[string]*resolutionCacheEntry),
		filePath: filepath.Join(dir, resolutionCacheFileName),
	}
	c.load()
	return c, nil
}

// Get returns a cached ResolvedSkill for the given URI if it exists
// and has not expired. The returned value is a deep copy safe for
// concurrent use.
func (c *GitHubResolutionCache) Get(uri string) (ResolvedSkill, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[uri]
	if !ok {
		return ResolvedSkill{}, false
	}
	if time.Now().After(entry.ExpiresAt) {
		return ResolvedSkill{}, false
	}
	skill := entry.Skill
	if len(entry.Skill.Files) > 0 {
		skill.Files = make([]ResolvedFile, len(entry.Skill.Files))
		copy(skill.Files, entry.Skill.Files)
	}
	return skill, true
}

// Put stores a resolved skill in the cache.
func (c *GitHubResolutionCache) Put(uri string, skill ResolvedSkill) {
	c.mu.Lock()
	now := time.Now()
	c.entries[uri] = &resolutionCacheEntry{
		Skill:     skill,
		CachedAt:  now,
		ExpiresAt: now.Add(c.ttl),
	}
	c.evictExpired()
	snapshot := make(map[string]*resolutionCacheEntry, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
	}
	c.mu.Unlock()

	c.save(snapshot)
}

// load reads the cache from disk. Best-effort: errors are silently ignored.
func (c *GitHubResolutionCache) load() {
	data, err := os.ReadFile(c.filePath)
	if err != nil {
		return
	}
	var f resolutionCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	if f.Entries == nil {
		return
	}
	now := time.Now()
	for uri, entry := range f.Entries {
		if now.Before(entry.ExpiresAt) {
			c.entries[uri] = entry
		}
	}
	util.Debugf("github: loaded %d resolution cache entries from disk", len(c.entries))
}

// save persists the given entries snapshot to disk atomically. Best-effort.
func (c *GitHubResolutionCache) save(entries map[string]*resolutionCacheEntry) {
	f := resolutionCacheFile{Entries: entries}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	tmpPath := c.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmpPath, c.filePath)
}

// evictExpired removes expired entries. Must be called with lock held.
func (c *GitHubResolutionCache) evictExpired() {
	now := time.Now()
	for uri, entry := range c.entries {
		if now.After(entry.ExpiresAt) {
			delete(c.entries, uri)
		}
	}
}

// githubResolutionCacheDir returns the directory for storing GitHub
// resolution cache files.
func githubResolutionCacheDir() (string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, "cache", "github-resolution"), nil
}
