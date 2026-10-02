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
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

	// refreshFailureBackoff bounds how often a background stale-refresh is
	// retried for the same flight key after it fails. Without this, a
	// persistently failing ref (rate limit, outage) would start a brand new
	// refresh attempt — and its retry/backoff cost — on every single stale
	// hit, while silently continuing to serve the stale value regardless.
	refreshFailureBackoff = 1 * time.Minute

	resolutionCacheFileName = "github-resolution-cache.json"

	// ttlJitterFraction bounds how far JitteredTTL spreads a TTL from its
	// nominal value, as a fraction of that TTL (plus or minus 10%). See
	// JitteredTTL.
	ttlJitterFraction = 0.10
)

// JitteredTTL returns ttl adjusted by a uniformly random amount within
// +/-ttlJitterFraction of ttl, so cache entries written together — the
// common case during a burst of concurrent creates that all fill the cache
// at once — do not all expire at exactly the same instant and stampede
// GitHub again together. Shared by the broker cache (putEntry, below) and
// the hub's GitHubResolutionStore (pkg/hub/github_resolution_store.go) so
// both sides spread expiry the same way.
//
// randFloat64 must return a value in [0,1); pass math/rand's top-level
// Float64 for production use (its global source is internally
// synchronized, so this is safe for concurrent callers) or a seeded
// *rand.Rand's Float64 method for a deterministic test.
func JitteredTTL(ttl time.Duration, randFloat64 func() float64) time.Duration {
	spread := float64(ttl) * ttlJitterFraction
	delta := (randFloat64()*2 - 1) * spread
	return ttl + time.Duration(delta)
}

// MaxJitteredTTL returns the largest value JitteredTTL(ttl, ...) can ever
// return. Callers that must derive a safe bound from a stored ExpiresAt
// without knowing the exact jitter that was applied when it was written
// (see GetStale and PurgeExpired in pkg/hub/github_resolution_store.go,
// which infer a row's last-resolved time as ExpiresAt - TTL and must never
// overestimate how recently that was) use this upper bound instead of the
// nominal TTL, so a row is never treated as fresher than it could possibly
// be.
func MaxJitteredTTL(ttl time.Duration) time.Duration {
	return ttl + time.Duration(float64(ttl)*ttlJitterFraction)
}

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
	credSlots map[string]*credSlot

	// refreshMu guards lastRefreshFailure, which records the last time a
	// background stale-refresh failed for a given flight key (see
	// refreshFailureBackoff).
	refreshMu          sync.Mutex
	lastRefreshFailure map[string]time.Time
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
		ExpiresAt:   now.Add(JitteredTTL(c.ttl, rand.Float64)),
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

// credSlot is a per-credentialID semaphore plus a reference count of callers
// currently holding or waiting on it, so acquireCredentialSlot can delete the
// entry once nothing needs it anymore (see the map-growth comment there).
type credSlot struct {
	sem  chan struct{}
	refs int // guarded by GitHubResolutionCache.credMu
}

// acquireCredentialSlot blocks until a slot is free for credentialID (see
// maxInFlightPerCredential) or ctx is done, whichever comes first. The
// returned release func must be called exactly once to free the slot.
//
// credSlots entries are reference-counted and deleted once nothing holds or
// is waiting on them. Without this, the map would grow without bound:
// credentialID includes a fingerprint of the credential's own value (see
// flightIdentity), so a GitHub App token minted fresh for every create — one
// of the credential sources flightIdentity documents — leaves a permanent
// ~300B entry behind for the life of the process, one per fallback
// resolution, since that credentialID is never seen again.
func (c *GitHubResolutionCache) acquireCredentialSlot(ctx context.Context, credentialID string) (release func(), err error) {
	c.credMu.Lock()
	if c.credSlots == nil {
		c.credSlots = make(map[string]*credSlot)
	}
	slot, ok := c.credSlots[credentialID]
	if !ok {
		slot = &credSlot{sem: make(chan struct{}, maxInFlightPerCredential)}
		c.credSlots[credentialID] = slot
	}
	slot.refs++
	c.credMu.Unlock()

	// releaseRef drops this call's reservation on slot and deletes
	// credSlots[credentialID] once nothing references it anymore. Called
	// either way below: on a successful acquire (paired with releasing the
	// semaphore itself) or on ctx.Done() (the reservation was never turned
	// into a held slot).
	releaseRef := func() {
		c.credMu.Lock()
		slot.refs--
		if slot.refs == 0 && c.credSlots[credentialID] == slot {
			delete(c.credSlots, credentialID)
		}
		c.credMu.Unlock()
	}

	select {
	case slot.sem <- struct{}{}:
		return func() {
			<-slot.sem
			releaseRef()
		}, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

// recentRefreshFailure reports whether a background refresh for flightKey
// failed within the last refreshFailureBackoff.
func (c *GitHubResolutionCache) recentRefreshFailure(flightKey string) bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	t, ok := c.lastRefreshFailure[flightKey]
	return ok && time.Since(t) < refreshFailureBackoff
}

func (c *GitHubResolutionCache) recordRefreshFailure(flightKey string) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.lastRefreshFailure == nil {
		c.lastRefreshFailure = make(map[string]time.Time)
	}
	c.lastRefreshFailure[flightKey] = time.Now()
}

func (c *GitHubResolutionCache) clearRefreshFailure(flightKey string) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	delete(c.lastRefreshFailure, flightKey)
}

// flightJoinHook, when non-nil, is called immediately before every caller —
// leader and followers alike — calls flight.DoChan for flightKey. Tests use
// it to know precisely when a second (or later) caller has reached the point
// of joining an in-flight resolution, without polling or sleeping: the first
// invocation for a key is the caller that will become the flight leader; any
// later invocation for the same key, made while that leader's call is still
// outstanding, is a caller that will join it as a follower.
//
// Held in an atomic.Pointer, not a plain var: a background refresh goroutine
// started by one test (see ResolveWithFetch's stale-serve path) can still be
// running when that test returns and a later test installs its own hook —
// reading and writing a plain var across those two goroutines with no
// synchronization is a data race. The atomic load/store here makes that
// interleaving race-free; it does not change which hook a given call
// observes, which is still whichever one was most recently installed when
// the call happened to run.
var flightJoinHook atomic.Pointer[func(string)]

func injectFlightJoin(flightKey string) {
	if hook := flightJoinHook.Load(); hook != nil {
		(*hook)(flightKey)
	}
}

// coalesceFetch runs fetch for cacheKey, using flightKey to coalesce
// concurrent calls for the same ref into a single upstream fetch, and
// credentialID to bound how many such fetches may run concurrently for a
// shared credential (acquireCredentialSlot).
//
// Every caller — leader and followers alike — waits via DoChan and a select
// on its own ctx, so a caller whose own context is done returns ctx.Err()
// immediately instead of blocking for the whole flight. The flight itself
// keeps running for whoever is still waiting on it: it is detached from any
// one caller's cancellation and bounded only by the fixed githubFlightTimeout
// ceiling, not by any one caller's own deadline — every waiter (including the
// leader) already returns on its own ctx.Done() via the select below, so no
// caller can wait past its own deadline regardless of this bound. Deriving
// the bound from the leader's deadline instead would fail every waiter with
// that leader's own "context deadline exceeded" the moment it expired —
// including waiters with no deadline, or a later one — the exact starvation
// this flight exists to prevent. This mirrors
// cachingGoogleCredentialValidator.validate (google_credential_cache.go) for
// the detach-and-bound shape, and adds the per-waiter DoChan/select on top so
// an individual caller's own cancellation is still honored promptly.
func (c *GitHubResolutionCache) coalesceFetch(
	ctx context.Context,
	flightKey, credentialID, cacheKey, logRef string,
	isBranchRef bool,
	fetch func(context.Context) (ResolvedSkill, error),
) (ResolvedSkill, error) {
	injectFlightJoin(flightKey)
	resultCh := c.flight.DoChan(flightKey, func() (result interface{}, ferr error) {
		// DoChan always runs this function in a goroutine it spawns itself
		// (see golang.org/x/sync/singleflight), never the calling goroutine —
		// unlike Do, there is no caller stack frame to recover a panic in. A
		// panic here otherwise crashes the process outright (singleflight
		// deliberately makes it unrecoverable once there is a channel
		// waiter). Recovering here, inside the function singleflight runs,
		// converts it into a normal error instead, delivered to every waiter
		// through resultCh like any other failure. logRef, not flightKey, goes
		// in the message: flightKey and credentialID carry a fingerprint of
		// the credential value plus its project/user scope, and this error
		// can reach a caller (see ResolveOpts/Resolve), so it must never
		// carry anything derived from the credential itself.
		defer func() {
			if r := recover(); r != nil {
				ferr = fmt.Errorf("panic during GitHub skill resolution for %s: %v", logRef, r)
			}
		}()

		// Re-check: another caller may have already populated cacheKey while
		// this call waited to become the flight leader — a concurrent flight
		// for this exact key that finished just before this one got to run.
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

	select {
	case res := <-resultCh:
		if res.Err != nil {
			return ResolvedSkill{}, res.Err
		}
		return res.Val.(ResolvedSkill), nil
	case <-ctx.Done():
		// A waiter whose own deadline (e.g. the resolve budget) expires
		// before the shared flight finishes is a timeout, so classify it as
		// one; plain cancellation stays unclassified. Both errors are wrapped
		// so errors.As finds the code and errors.Is still matches the
		// context error. logRef only: no credential-derived material.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ResolvedSkill{}, fmt.Errorf("%w: %w", &githubResolveError{
				code: SkillErrCodeTimeout,
				msg:  fmt.Sprintf("timed out waiting for GitHub skill resolution of %s", logRef),
			}, ctx.Err())
		}
		return ResolvedSkill{}, ctx.Err()
	}
}

// ResolveWithFetch is the single entry point for obtaining a resolved skill
// through the cache:
//
//   - A fresh hit under cacheKey is returned directly.
//   - For a branch ref with a stale (TTL-expired but within
//     MaxResolutionStaleAge) entry, the stale value is returned immediately
//     and a refresh is started in the background, coalesced with any other
//     refresh already in flight for flightKey — unless a refresh for this key
//     failed within the last refreshFailureBackoff, in which case the stale
//     value is served without starting another one.
//   - Otherwise, fetch runs synchronously, coalesced via flightKey and capped
//     per credentialID (see coalesceFetch).
//
// cacheKey identifies the exact (ref, credential) pair for Get/Put — it
// includes a fingerprint of the credential's own value, so that distinct
// per-mint tokens isolate their cache entries (see resolutionCacheKey).
// flightKey and credentialID are built the same way (see flightIdentity in
// github_skill_resolver.go): each includes a cryptographic fingerprint of the
// credential value in use, plus project and user scope, so two different
// credential values never share a flight or a credential-cap slot, and two
// distinct credentials (e.g. two projects' same-named secret) never collide
// either — the same guarantee cacheKey gives Get/Put, applied here to
// coalescing and the cap. The accepted cost: a GitHub App token minted fresh
// for every create does not coalesce, or share a cap slot, with another mint
// for the same repo on the broker fallback path, since each mint is its own
// value and gets its own fingerprint.
//
// logRef is a credential-free label (ref plus the general kind of source,
// never the credential's value, its fingerprint, or a secret's name) used
// only for the log line and error message below — never flightKey or
// credentialID, which must not reach a log or a caller-visible error.
func (c *GitHubResolutionCache) ResolveWithFetch(
	ctx context.Context,
	cacheKey, flightKey, credentialID, logRef string,
	isBranchRef bool,
	fetch func(context.Context) (ResolvedSkill, error),
) (ResolvedSkill, error) {
	if skill, ok := c.Get(cacheKey); ok {
		return skill, nil
	}

	if isBranchRef {
		if skill, ok := c.getStale(cacheKey); ok {
			if c.recentRefreshFailure(flightKey) {
				fmt.Fprintf(os.Stderr, "github: WARNING: serving stale entry for %s; skipping refresh after a recent failure\n", logRef)
			} else {
				// A panic in fetch is recovered inside coalesceFetch's DoChan
				// closure (see its comment), so this goroutine itself cannot
				// panic from that; no recover needed at this level.
				go func() {
					_, ferr := c.coalesceFetch(context.Background(), flightKey, credentialID, cacheKey, logRef, isBranchRef, fetch)
					if ferr != nil {
						c.recordRefreshFailure(flightKey)
					} else {
						c.clearRefreshFailure(flightKey)
					}
				}()
			}
			return skill, nil
		}
	}

	return c.coalesceFetch(ctx, flightKey, credentialID, cacheKey, logRef, isBranchRef, fetch)
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
