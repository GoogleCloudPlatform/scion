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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	// DefaultResolutionCacheTTL is how long a cached resolution result is
	// considered fresh for branch/tag refs. GitHub content can change, so
	// this is a balance between freshness and avoiding rate limits.
	DefaultResolutionCacheTTL = 30 * time.Minute

	// DefaultSHAResolutionCacheTTL is how long a cached resolution result
	// for a full commit SHA is considered fresh. SHAs are immutable, so we
	// cache them for much longer to avoid redundant API calls.
	DefaultSHAResolutionCacheTTL = 24 * time.Hour

	// MaxResolutionStaleAge bounds how long a branch-ref entry may still be
	// served after its TTL has expired while a background refresh runs (see
	// ResolveWithFetch). Past this age the entry is treated as absent and a
	// resolution happens synchronously instead. Commit-SHA refs are
	// immutable and are never affected by this: they are only ever served
	// fresh (within their own, much longer, TTL) or re-resolved.
	MaxResolutionStaleAge = 24 * time.Hour

	// maxInFlightPerCredential bounds the number of concurrent upstream
	// GitHub fetches sharing one credential identity (see
	// acquireCredentialSlot). Single-flight alone only coalesces identical
	// refs; a burst of *distinct* refs resolved with the same credential
	// would otherwise still hit GitHub with unbounded concurrency.
	maxInFlightPerCredential = 4

	// githubFlightTimeout bounds a coalesced fetch (see coalesceFetch), once
	// detached from any specific caller's context. It is generous enough to
	// cover a full ref resolution — commit lookup, contents listing, and a
	// raw download per file — including GitHubSkillResolver's own retry
	// backoff, without hanging forever if upstream is completely
	// unresponsive.
	githubFlightTimeout = 5 * time.Minute

	resolutionCacheFileName = "github-resolution-cache.json"
)

// GitHubResolutionCache caches the mapping from skill URI → ResolvedSkill
// to avoid redundant GitHub API calls during repeated provisioning. Entries
// expire after a configurable TTL. It also coalesces concurrent fetches for
// the same ref (single-flight) and bounds concurrent fetches that share a
// credential (see ResolveWithFetch), so it is intended to be shared as a
// singleton across requests rather than constructed per request.
type GitHubResolutionCache struct {
	mu       sync.RWMutex
	dir      string
	ttl      time.Duration
	entries  map[string]*resolutionCacheEntry
	filePath string

	flight singleflight.Group

	credMu    sync.Mutex
	credSlots map[string]chan struct{}
}

type resolutionCacheEntry struct {
	Skill     ResolvedSkill `json:"skill"`
	CachedAt  time.Time     `json:"cachedAt"`
	ExpiresAt time.Time     `json:"expiresAt"`
	// IsBranchRef is true for branch/tag refs and false for full commit SHAs.
	// Only branch-ref entries are eligible for the stale-while-revalidate
	// behavior in ResolveWithFetch and the extended eviction horizon below —
	// a SHA-pinned entry's content can never change, so there is nothing to
	// revalidate and no reason to serve it past its own TTL.
	IsBranchRef bool `json:"isBranchRef"`
}

// entryAlive reports whether entry should still be retained in memory (and,
// for non-credential entries, on disk): either it is still fresh, or it is a
// branch ref within MaxResolutionStaleAge of its original CachedAt and so
// might still be served stale. This is deliberately more permissive than the
// "is this fresh" check in Get — it governs retention, not freshness.
func entryAlive(entry *resolutionCacheEntry, now time.Time) bool {
	if now.Before(entry.ExpiresAt) {
		return true
	}
	return entry.IsBranchRef && now.Before(entry.CachedAt.Add(MaxResolutionStaleAge))
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

// Put stores a resolved skill in the cache as a non-branch (e.g. full-SHA)
// entry — see putEntry for the branch-ref variant used internally by
// ResolveWithFetch, and for the disk-persistence rules described below.
func (c *GitHubResolutionCache) Put(uri string, skill ResolvedSkill) {
	c.putEntry(uri, skill, false)
}

// putEntry stores a resolved skill in the cache, recording whether it is a
// branch ref (see resolutionCacheEntry.IsBranchRef).
//
// Credential-bearing entries — those whose URI key contains a token-hash
// suffix (the "#<hex>" appended by resolutionCacheKey when a GitHub token is
// present) — are kept in-memory only and never written to disk. This prevents
// the stale-404 class of bug described in issue #565: ResolvedFile.Content is
// tagged json:"-" and is not preserved through serialisation, so a disk-cache
// hit after a broker restart returns Content == nil. installOneSkill then
// falls back to downloadSkillFile using the broker's default GitHub token —
// not the per-URI named credential — causing a 404 on private repos. By
// keeping credential entries in-memory only, Content survives for the full
// TTL window within the same process, and there is no stale entry to load
// after a restart.
func (c *GitHubResolutionCache) putEntry(uri string, skill ResolvedSkill, isBranchRef bool) {
	c.mu.Lock()
	now := time.Now()
	c.entries[uri] = &resolutionCacheEntry{
		Skill:       skill,
		CachedAt:    now,
		ExpiresAt:   now.Add(c.ttl),
		IsBranchRef: isBranchRef,
	}
	c.evictExpired()

	// Credential-bearing URI keys contain a "#<tokenhash>" suffix.
	// Keep them in-memory only — do not persist to disk.
	if strings.Contains(uri, "#") {
		c.mu.Unlock()
		return
	}

	// For public-repo entries, persist to disk. Exclude any credential
	// entries that may be present in the map from earlier Puts.
	snapshot := make(map[string]*resolutionCacheEntry, len(c.entries))
	for k, v := range c.entries {
		if !strings.Contains(k, "#") {
			snapshot[k] = v
		}
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
		if entryAlive(entry, now) {
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

// evictExpired removes entries that are no longer alive (see entryAlive).
// Must be called with lock held.
func (c *GitHubResolutionCache) evictExpired() {
	now := time.Now()
	for uri, entry := range c.entries {
		if !entryAlive(entry, now) {
			delete(c.entries, uri)
		}
	}
}

// getStale returns the cached skill for uri even though its TTL has expired,
// provided the entry is a branch ref and was originally cached within
// MaxResolutionStaleAge. It must only be consulted after Get has already
// reported a miss or a non-fresh hit.
func (c *GitHubResolutionCache) getStale(uri string) (ResolvedSkill, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[uri]
	if !ok || !entry.IsBranchRef {
		return ResolvedSkill{}, false
	}
	if time.Since(entry.CachedAt) >= MaxResolutionStaleAge {
		return ResolvedSkill{}, false
	}
	skill := entry.Skill
	if len(entry.Skill.Files) > 0 {
		skill.Files = make([]ResolvedFile, len(entry.Skill.Files))
		copy(skill.Files, entry.Skill.Files)
	}
	return skill, true
}

// acquireCredentialSlot blocks until a slot is free for credentialID (see
// maxInFlightPerCredential) or ctx is done, whichever comes first. The
// returned release func must be called exactly once to free the slot.
func (c *GitHubResolutionCache) acquireCredentialSlot(ctx context.Context, credentialID string) (release func(), err error) {
	c.credMu.Lock()
	if c.credSlots == nil {
		c.credSlots = make(map[string]chan struct{})
	}
	sem, ok := c.credSlots[credentialID]
	if !ok {
		sem = make(chan struct{}, maxInFlightPerCredential)
		c.credSlots[credentialID] = sem
	}
	c.credMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// coalesceFetch runs fetch for cacheKey, using flightKey to coalesce
// concurrent calls for the same ref into a single upstream fetch, and
// credentialID to bound how many such fetches may run concurrently for a
// shared credential (acquireCredentialSlot).
//
// The work (semaphore wait plus fetch) runs on a context detached from ctx's
// cancellation — but still derived from it, so values such as request-scoped
// logging fields survive — and bounded by githubFlightTimeout. This mirrors
// cachingGoogleCredentialValidator.validate (google_credential_cache.go): the
// flight is shared by every caller waiting on flightKey, so whichever one
// happens to be the single-flight leader cancelling its own request must not
// fail every other caller's request too.
func (c *GitHubResolutionCache) coalesceFetch(
	ctx context.Context,
	flightKey, credentialID, cacheKey string,
	isBranchRef bool,
	fetch func(context.Context) (ResolvedSkill, error),
) (ResolvedSkill, error) {
	v, err, _ := c.flight.Do(flightKey, func() (interface{}, error) {
		// Re-check: another caller may have already populated cacheKey while
		// this call waited to become the flight leader — either a concurrent
		// flight for this exact key, or (since flightKey intentionally
		// ignores the per-mint token, see ResolveWithFetch) a different
		// cacheKey sharing this flightKey's ref and credential class.
		if skill, ok := c.Get(cacheKey); ok {
			return skill, nil
		}

		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubFlightTimeout)
		defer cancel()

		release, aerr := c.acquireCredentialSlot(flightCtx, credentialID)
		if aerr != nil {
			return ResolvedSkill{}, aerr
		}
		defer release()

		skill, ferr := fetch(flightCtx)
		if ferr != nil {
			return ResolvedSkill{}, ferr
		}
		c.putEntry(cacheKey, skill, isBranchRef)
		return skill, nil
	})
	if err != nil {
		return ResolvedSkill{}, err
	}
	return v.(ResolvedSkill), nil
}

// ResolveWithFetch is the single entry point for obtaining a resolved skill
// through the cache:
//
//   - A fresh hit under cacheKey is returned directly.
//   - For a branch ref with a stale (TTL-expired but within
//     MaxResolutionStaleAge) entry, the stale value is returned immediately
//     and a refresh is started in the background, coalesced with any other
//     refresh already in flight for flightKey.
//   - Otherwise, fetch runs synchronously, coalesced via flightKey and capped
//     per credentialID (see coalesceFetch).
//
// cacheKey identifies the exact (ref, credential) pair for Get/Put — it may
// include the credential's token, so that distinct per-mint tokens isolate
// their cache entries (see resolutionCacheKey). flightKey and credentialID
// must NOT be derived from the token itself: single-flight and the
// per-credential cap exist specifically to coalesce concurrent resolutions
// that share a ref and credential *class* even though each one mints its own
// token, so keying on the token would defeat them — every caller would get a
// distinct flightKey and never coalesce.
func (c *GitHubResolutionCache) ResolveWithFetch(
	ctx context.Context,
	cacheKey, flightKey, credentialID string,
	isBranchRef bool,
	fetch func(context.Context) (ResolvedSkill, error),
) (ResolvedSkill, error) {
	if skill, ok := c.Get(cacheKey); ok {
		return skill, nil
	}

	if isBranchRef {
		if skill, ok := c.getStale(cacheKey); ok {
			go func() {
				_, _ = c.coalesceFetch(context.Background(), flightKey, credentialID, cacheKey, isBranchRef, fetch)
			}()
			return skill, nil
		}
	}

	return c.coalesceFetch(ctx, flightKey, credentialID, cacheKey, isBranchRef, fetch)
}

// GitHubResolutionCacheDir returns the directory for storing GitHub
// resolution cache files.
func GitHubResolutionCacheDir() (string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, "cache", "github-resolution"), nil
}
