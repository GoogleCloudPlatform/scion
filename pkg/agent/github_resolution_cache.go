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
	"io"
	"math/rand"
	"net/http"
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

	// failureCacheTTL is how long a non-retryable resolution failure (see
	// cacheableFailure) is remembered for its cacheKey. Within this window a
	// resolution of the same ref with the same credential returns the
	// remembered error without calling GitHub. It is kept short because the
	// cause can be fixed at any time (the path is added, access is granted).
	//
	// The key carries a fingerprint of the credential value, so a credential
	// minted fresh for each create (a GitHub App installation token) gets a
	// new key every time: a not_found remembered for one create is not used
	// by the next one, only by other resolutions within the same create.
	failureCacheTTL = time.Minute

	// maxRememberedFailures bounds how many failures are remembered at once
	// (see recordFailure). When the limit is reached after dropping expired
	// entries, a new failure is not remembered; not remembering is always
	// safe, it only means the next resolution asks GitHub again.
	maxRememberedFailures = 1024

	// refreshFailureBackoff bounds how often a background stale-refresh is
	// retried for the same flight key after it fails. Without this, a
	// persistently failing ref (rate limit, outage) would start a brand new
	// refresh attempt — and its retry/backoff cost — on every single stale
	// hit, while silently continuing to serve the stale value regardless.
	refreshFailureBackoff = 1 * time.Minute

	resolutionCacheFileName = "github-resolution-cache.json"

	// resolutionCacheDirMode and resolutionCacheFileMode are the permissions
	// of the cache directory and file. Keys carry a fingerprint of the
	// credential in use, so the file is kept readable by its owner only.
	resolutionCacheDirMode  os.FileMode = 0o700
	resolutionCacheFileMode os.FileMode = 0o600

	// ttlJitterFraction bounds how far JitteredTTL spreads a TTL from its
	// nominal value, as a fraction of that TTL (plus or minus 10%). See
	// JitteredTTL.
	ttlJitterFraction = 0.10
)

// DefaultResolutionCacheSaveDelay is how long a Put waits before the cache
// file is rewritten, unless WithResolutionCacheSaveDelay sets another value.
// Puts that arrive during this window share one write, so a burst of
// resolutions rewrites the file once instead of once per Put.
const DefaultResolutionCacheSaveDelay = 2 * time.Second

// ResolutionCacheOption configures a GitHubResolutionCache at construction
// (see NewGitHubResolutionCache).
type ResolutionCacheOption func(*GitHubResolutionCache)

// WithResolutionCacheSaveDelay sets how long a Put waits before the cache
// file is rewritten (default DefaultResolutionCacheSaveDelay). A
// non-positive d leaves the default in place. Tests use a long delay so the
// delayed write never fires on its own and they call Flush explicitly.
func WithResolutionCacheSaveDelay(d time.Duration) ResolutionCacheOption {
	return func(c *GitHubResolutionCache) {
		if d > 0 {
			c.saveDelay = d
		}
	}
}

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

	// failureMu guards failures, which holds recent non-retryable
	// resolution failures by cacheKey (see failureCacheTTL). They are kept
	// in memory only and are never written to the cache file.
	failureMu sync.Mutex
	failures  map[string]cachedFailure

	// causeMu guards flightCauses, which holds, per flight key, the record
	// of the flight currently running for it (see flightCause).
	causeMu      sync.Mutex
	flightCauses map[string]*flightCause

	// lifecycleMu guards closing and the Add side of refreshWG, so Close
	// never waits on refreshWG while a new refresh is being added to it.
	// refreshWG counts background stale-refresh goroutines (see
	// resolveWithFetchAccept); closing, once set by Close, stops new ones
	// from starting.
	lifecycleMu sync.Mutex
	closing     bool
	refreshWG   sync.WaitGroup

	// drainOnce starts the single goroutine that closes drained once
	// refreshWG reaches zero (see refreshesDone).
	drainOnce sync.Once
	drained   chan struct{}

	// saveDelay is how long a Put waits before the file is rewritten (see
	// WithResolutionCacheSaveDelay and scheduleSave).
	saveDelay time.Duration

	// pendingMu guards savePending and saveTimer: whether a rewrite of the
	// file has been requested and not yet started.
	pendingMu   sync.Mutex
	savePending bool
	saveTimer   *time.Timer

	// saveMu serializes rewrites of the file, so two writers never race on
	// the rename and the newest snapshot is always the one left on disk.
	saveMu sync.Mutex

	// writeData writes the encoded cache to the temp file before it is
	// synced and renamed into place. Tests replace it to simulate a failed
	// write; nil means a plain write.
	writeData func(w io.Writer, data []byte) error

	// saveCount counts completed rewrites of the file. Tests use it to
	// check that writes are coalesced.
	saveCount atomic.Int64

	// onFlush, when non-nil, is called at the end of every Flush that wrote
	// the file. Tests use it to wait for the delayed write without sleeping.
	onFlush func()
}

// cachedFailure is a remembered non-retryable resolution failure.
type cachedFailure struct {
	err       error
	expiresAt time.Time
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

// entryAlive reports whether entry should still be retained in memory and on
// disk: either it is still fresh, or it is a
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
// given directory with the specified TTL. The directory is created with
// mode 0700, and an existing directory or cache file with looser
// permissions is tightened (see resolutionCacheDirMode). opts adjust the
// defaults (see ResolutionCacheOption).
func NewGitHubResolutionCache(dir string, ttl time.Duration, opts ...ResolutionCacheOption) (*GitHubResolutionCache, error) {
	if err := os.MkdirAll(dir, resolutionCacheDirMode); err != nil {
		return nil, err
	}
	// MkdirAll leaves an existing directory as it is, and a new one is
	// subject to the umask, so set the mode explicitly in both cases.
	if err := tightenMode(dir, resolutionCacheDirMode); err != nil {
		// For example a directory owned by another user. The cache still
		// works; only its permissions are not what this process would set.
		fmt.Fprintf(os.Stderr, "github: WARNING: cannot set mode of resolution cache directory: %v\n", err)
	}
	c := &GitHubResolutionCache{
		dir:       dir,
		ttl:       ttl,
		entries:   make(map[string]*resolutionCacheEntry),
		filePath:  filepath.Join(dir, resolutionCacheFileName),
		saveDelay: DefaultResolutionCacheSaveDelay,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.load()
	return c, nil
}

// tightenMode sets path's permission bits to mode if it grants anything
// more than mode does. A missing path is not an error.
func tightenMode(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode().Perm()&^mode == 0 {
		return nil
	}
	return chmod(path, mode)
}

// chmod is os.Chmod, replaceable by tests to simulate a path the process
// cannot change.
var chmod = os.Chmod

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
// branch ref (see resolutionCacheEntry.IsBranchRef), and schedules a
// rewrite of the cache file (see scheduleSave).
//
// Entries resolved with a credential are persisted too. Their key ends in
// "#" plus a SHA-256 fingerprint of the credential (see resolutionCacheKey);
// the credential itself is never part of an entry. ResolvedFile.Content is
// not written to disk (json:"-"), so an entry loaded after a restart has no
// file bytes. GitHubSkillResolver handles that case by passing the
// credential it resolved with to the install step for that skill's
// downloads (see ResolvedSkill.githubCredentialRef and gitHubDownloadToken),
// so a private repo read with a named credential is not downloaded with a
// different one. When the request cannot supply that credential at install,
// the resolver does not use a content-less entry at all and resolves the ref
// again (see GitHubSkillResolver.resolveOne).
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
	c.mu.Unlock()

	c.clearFailure(uri)
	c.scheduleSave()
}

// cacheableFailure reports whether a fetch error is worth remembering for
// failureCacheTTL: only a not_found for the ref or the skill directory,
// which does not change between attempts made close together. A file
// download that 404s after the listing named it, retryable causes (5xx, no
// response), timeouts, rate limits and unclassified errors are never
// remembered.
func cacheableFailure(err error) bool {
	var rerr *githubResolveError
	return errors.As(err, &rerr) && rerr.code == SkillErrCodeNotFound && !rerr.fileMissingAfterListing
}

// recordFailure remembers err for cacheKey until failureCacheTTL from now,
// and drops any expired failures so the map stays small. Once
// maxRememberedFailures unexpired failures are held, a failure for a new
// cacheKey is not remembered.
func (c *GitHubResolutionCache) recordFailure(cacheKey string, err error) {
	now := time.Now()
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	if c.failures == nil {
		c.failures = make(map[string]cachedFailure)
	}
	for k, f := range c.failures {
		if !now.Before(f.expiresAt) {
			delete(c.failures, k)
		}
	}
	if _, ok := c.failures[cacheKey]; !ok && len(c.failures) >= maxRememberedFailures {
		return
	}
	c.failures[cacheKey] = cachedFailure{err: err, expiresAt: now.Add(failureCacheTTL)}
}

// recentFailure returns the failure remembered for cacheKey, or nil if there
// is none or it has expired.
func (c *GitHubResolutionCache) recentFailure(cacheKey string) error {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	f, ok := c.failures[cacheKey]
	if !ok {
		return nil
	}
	if !time.Now().Before(f.expiresAt) {
		delete(c.failures, cacheKey)
		return nil
	}
	return f.err
}

func (c *GitHubResolutionCache) clearFailure(cacheKey string) {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	delete(c.failures, cacheKey)
}

// scheduleSave requests a rewrite of the cache file after saveDelay. If a
// rewrite is already pending, this Put is covered by it: the pending
// rewrite takes its snapshot when it runs, not when it was requested.
func (c *GitHubResolutionCache) scheduleSave() {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.savePending {
		return
	}
	c.savePending = true
	c.saveTimer = time.AfterFunc(c.saveDelay, c.Flush)
}

// Flush writes the current entries to disk now if a rewrite is pending, and
// cancels the pending timer. It is called by that timer, and may be called
// directly (for example on shutdown) so that no Put is lost. Safe for
// concurrent use.
//
// savePending is cleared before the snapshot is taken, so a Put that lands
// after the snapshot schedules a new rewrite rather than being dropped.
func (c *GitHubResolutionCache) Flush() {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()

	c.pendingMu.Lock()
	if !c.savePending {
		c.pendingMu.Unlock()
		return
	}
	c.savePending = false
	if c.saveTimer != nil {
		c.saveTimer.Stop()
		c.saveTimer = nil
	}
	c.pendingMu.Unlock()

	c.save(c.snapshot())
	if c.onFlush != nil {
		c.onFlush()
	}
}

// Close prepares the cache for process exit. It stops new background
// refreshes of stale entries, waits for the ones already running to finish
// or for ctx to be done, whichever comes first, and then writes any pending
// entries to disk (see Flush), so a refresh that completed during the wait
// is persisted. The write happens even when ctx is done first; Close then
// returns ctx.Err(), and a refresh still running may finish after the write
// without being persisted.
//
// The cache stays usable after Close: lookups and synchronous resolutions
// work as before, a stale entry is served without starting a refresh, and a
// later Put schedules a delayed write as usual. Safe for concurrent use and
// for calling more than once. All calls share one goroutine waiting for the
// refreshes (see refreshesDone); when Close returns on ctx, that goroutine
// keeps waiting until the refreshes finish, bounded by githubFlightTimeout.
func (c *GitHubResolutionCache) Close(ctx context.Context) error {
	c.lifecycleMu.Lock()
	c.closing = true
	c.lifecycleMu.Unlock()

	var err error
	select {
	case <-c.refreshesDone():
	case <-ctx.Done():
		err = ctx.Err()
	}
	c.Flush()
	return err
}

// refreshesDone returns a channel closed once every background refresh has
// finished. It must only be called after closing is set, so no refresh is
// added once the wait has started. The first call starts the one goroutine
// that waits; later calls return the same channel.
func (c *GitHubResolutionCache) refreshesDone() <-chan struct{} {
	c.drainOnce.Do(func() {
		c.drained = make(chan struct{})
		go func() {
			c.refreshWG.Wait()
			close(c.drained)
		}()
	})
	return c.drained
}

// startRefresh runs refresh in a background goroutine tracked by refreshWG
// and reports true, or reports false without running it once Close has been
// called.
func (c *GitHubResolutionCache) startRefresh(refresh func()) bool {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closing {
		return false
	}
	c.refreshWG.Add(1)
	go func() {
		defer c.refreshWG.Done()
		refresh()
	}()
	return true
}

// snapshot returns a copy of the live entries map for writing to disk.
// Entries are never modified after they are stored, so sharing the entry
// pointers with the copy is safe.
func (c *GitHubResolutionCache) snapshot() map[string]*resolutionCacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	out := make(map[string]*resolutionCacheEntry, len(c.entries))
	for k, v := range c.entries {
		if entryAlive(v, now) {
			out[k] = v
		}
	}
	return out
}

// validResolutionCacheKey reports whether key has the format
// resolutionCacheKey produces today: "gh://..." optionally followed by "#"
// and a full 64-character lowercase hex SHA-256 fingerprint. Keys written
// by older builds (for example with a shorter fingerprint) can never be
// looked up again and are dropped on load.
func validResolutionCacheKey(key string) bool {
	if !strings.HasPrefix(key, "gh://") {
		return false
	}
	i := strings.IndexByte(key, '#')
	if i < 0 {
		return true
	}
	fp := key[i+1:]
	if len(fp) != 64 {
		return false
	}
	for _, r := range fp {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// load reads the cache from disk. It keeps every entry the current
// stale-serve rules can still use (see entryAlive) and drops entries past
// that horizon, entries with a key in an old format, and malformed entries.
// If anything was dropped, the file is rewritten once so it does not keep
// carrying them. Best-effort: a missing or unreadable file starts the cache
// empty.
func (c *GitHubResolutionCache) load() {
	data, err := os.ReadFile(c.filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			util.Debugf("github: cannot read resolution cache file: %v", err)
		}
		return
	}
	// The file may predate the 0600 mode; tighten it now rather than
	// waiting for the next rewrite.
	if err := tightenMode(c.filePath, resolutionCacheFileMode); err != nil {
		fmt.Fprintf(os.Stderr, "github: WARNING: cannot set mode of resolution cache file: %v\n", err)
	}

	var f resolutionCacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		// Unreadable content: replace it with an empty file.
		util.Debugf("github: resolution cache file is not valid JSON, rewriting it")
		c.save(map[string]*resolutionCacheEntry{})
		return
	}
	now := time.Now()
	dropped := 0
	for key, entry := range f.Entries {
		if entry == nil || !validResolutionCacheKey(key) || !entryAlive(entry, now) {
			dropped++
			continue
		}
		c.entries[key] = entry
	}
	util.Debugf("github: loaded %d resolution cache entries from disk, dropped %d", len(c.entries), dropped)
	if dropped > 0 {
		c.save(c.snapshot())
	}
}

// save writes entries to the cache file atomically: it writes a temp file in
// the same directory with mode 0600, syncs it, and renames it over the cache
// file, so a reader never sees a partial file. Callers other than load must
// hold saveMu. A failure is logged and otherwise ignored: the in-memory
// cache keeps working and resolution never fails because of it. Log lines
// name the file only, never a key.
func (c *GitHubResolutionCache) save(entries map[string]*resolutionCacheEntry) {
	data, err := json.MarshalIndent(resolutionCacheFile{Entries: entries}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "github: WARNING: cannot encode resolution cache: %v\n", err)
		return
	}
	if err := c.writeFileAtomic(data); err != nil {
		fmt.Fprintf(os.Stderr, "github: WARNING: cannot write resolution cache file: %v\n", err)
		return
	}
	c.saveCount.Add(1)
}

func (c *GitHubResolutionCache) writeFileAtomic(data []byte) (err error) {
	// os.CreateTemp creates the file with mode 0600 (resolutionCacheFileMode),
	// and a umask can only narrow that, so no further chmod is needed. An
	// existing cache file with a wider mode is tightened in load.
	tmp, err := os.CreateTemp(c.dir, resolutionCacheFileName+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	write := c.writeData
	if write == nil {
		write = func(w io.Writer, b []byte) error {
			_, werr := w.Write(b)
			return werr
		}
	}
	if err = write(tmp, data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, c.filePath); err != nil {
		return err
	}
	// Sync the directory so the rename itself survives a crash. Not all
	// platforms support this; a failure here does not undo the write.
	if d, derr := os.Open(c.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
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

// staleServeHook, when non-nil, is called synchronously each time
// ResolveWithFetch serves a stale entry, with the flight key and whether a
// background refresh was started for it. Tests use it to assert that no
// refresh was started (for example during a rate-limit cooldown) without
// waiting for one that might never come. Atomic for the same reason as
// flightJoinHook.
var staleServeHook atomic.Pointer[func(flightKey string, refreshStarted bool)]

func injectStaleServe(flightKey string, refreshStarted bool) {
	if hook := staleServeHook.Load(); hook != nil {
		(*hook)(flightKey, refreshStarted)
	}
}

// flightCause records the classified failure of the latest attempt a
// shared fetch made and is retrying past (see recordAttemptCause). A caller
// that stops waiting for the flight on its own deadline reports this cause
// instead of a bare timeout (see coalesceFetchAccept). Safe for concurrent
// use.
type flightCause struct {
	mu    sync.Mutex
	cause *githubResolveError
}

func (f *flightCause) set(cause *githubResolveError) {
	f.mu.Lock()
	f.cause = cause
	f.mu.Unlock()
}

func (f *flightCause) get() *githubResolveError {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cause
}

type flightCauseKey struct{}

// contextWithFlightCause returns ctx carrying f, so the fetch running under
// ctx can record attempt failures in it (see recordAttemptCause).
func contextWithFlightCause(ctx context.Context, f *flightCause) context.Context {
	return context.WithValue(ctx, flightCauseKey{}, f)
}

// recordAttemptCause records cause as the latest attempt failure of the
// shared fetch running under ctx, if any; a nil cause clears it. cause.msg
// may reach a caller, so it must not carry credential material.
func recordAttemptCause(ctx context.Context, cause *githubResolveError) {
	if f, ok := ctx.Value(flightCauseKey{}).(*flightCause); ok {
		f.set(cause)
	}
}

// beginFlightCause registers a fresh flightCause for the flight now running
// for flightKey and returns it with a func that removes it again. Flights
// for one key never overlap (singleflight), so the registered record is
// always that of the current flight; a later flight replaces it.
func (c *GitHubResolutionCache) beginFlightCause(flightKey string) (*flightCause, func()) {
	f := &flightCause{}
	c.causeMu.Lock()
	if c.flightCauses == nil {
		c.flightCauses = make(map[string]*flightCause)
	}
	c.flightCauses[flightKey] = f
	c.causeMu.Unlock()
	return f, func() {
		c.causeMu.Lock()
		if c.flightCauses[flightKey] == f {
			delete(c.flightCauses, flightKey)
		}
		c.causeMu.Unlock()
	}
}

// usableRetryAfter reports whether v is worth passing on to a caller as a
// Retry-After: a positive number of seconds or an HTTP date. A "0" (or an
// unparseable value) tells the caller nothing.
func usableRetryAfter(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if secs, ok := parseRetryAfterSeconds(v); ok {
		return secs > 0
	}
	_, err := http.ParseTime(v)
	return err == nil
}

// lastFlightCause returns the latest attempt failure recorded by the flight
// running for flightKey, or nil if there is none.
func (c *GitHubResolutionCache) lastFlightCause(flightKey string) *githubResolveError {
	c.causeMu.Lock()
	f := c.flightCauses[flightKey]
	c.causeMu.Unlock()
	if f == nil {
		return nil
	}
	return f.get()
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
	return c.coalesceFetchAccept(ctx, flightKey, credentialID, cacheKey, logRef, isBranchRef, nil, fetch)
}

// coalesceFetchAccept is coalesceFetch with accept applied to the in-flight
// re-check of the cache (see resolveWithFetchAccept). Callers passing a
// non-nil accept must use a flightKey distinct from callers that do not, so
// they never join a flight whose result accept would reject.
func (c *GitHubResolutionCache) coalesceFetchAccept(
	ctx context.Context,
	flightKey, credentialID, cacheKey, logRef string,
	isBranchRef bool,
	accept func(ResolvedSkill) bool,
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
		if skill, ok := c.Get(cacheKey); ok && (accept == nil || accept(skill)) {
			return skill, nil
		}
		if ferr := c.recentFailure(cacheKey); ferr != nil {
			util.Debugf("github: returning remembered not_found for %s", logRef)
			return ResolvedSkill{}, ferr
		}

		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubFlightTimeout)
		defer cancel()
		cause, endCause := c.beginFlightCause(flightKey)
		defer endCause()
		flightCtx = contextWithFlightCause(flightCtx, cause)

		release, aerr := c.acquireCredentialSlot(flightCtx, credentialID)
		if aerr != nil {
			return ResolvedSkill{}, aerr
		}
		defer release()

		skill, ferr := fetch(flightCtx)
		if ferr != nil {
			if cacheableFailure(ferr) {
				c.recordFailure(cacheKey, ferr)
			}
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
		// one; plain cancellation stays unclassified. When the flight is
		// retrying past a classified failure (a 5xx, or no response), the
		// waiter reports that cause and its Retry-After instead, so the
		// caller sees why the flight has not finished. Both errors are
		// wrapped so errors.As finds the code and errors.Is still matches
		// the context error. logRef only: no credential-derived material.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			werr := &githubResolveError{
				code: SkillErrCodeTimeout,
				msg:  fmt.Sprintf("timed out waiting for GitHub skill resolution of %s", logRef),
			}
			if last := c.lastFlightCause(flightKey); last != nil {
				werr.code = last.code
				if usableRetryAfter(last.retryAfter) {
					werr.retryAfter = last.retryAfter
				}
				werr.msg = fmt.Sprintf("timed out waiting for GitHub skill resolution of %s, still retrying after: %s", logRef, last.msg)
			}
			return ResolvedSkill{}, fmt.Errorf("%w: %w", werr, ctx.Err())
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
//     value is served without starting another one. The same applies when
//     refreshAllowed is non-nil and returns false (the credential is in a
//     GitHub rate-limit cooldown, see GitHubCooldown): the stale value is
//     served and no refresh is started, and likewise once Close has been
//     called.
//   - A not_found from a fetch for cacheKey within the last failureCacheTTL
//     is returned again without fetching (see cacheableFailure). A
//     successful fetch for cacheKey clears it.
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
	refreshAllowed func() bool,
	fetch func(context.Context) (ResolvedSkill, error),
) (ResolvedSkill, error) {
	return c.resolveWithFetchAccept(ctx, cacheKey, flightKey, credentialID, logRef, isBranchRef, refreshAllowed, nil, fetch)
}

// resolveWithFetchAccept is ResolveWithFetch, except that a cached value
// (fresh or stale) is only used if accept is nil or returns true for it;
// otherwise the ref is fetched synchronously, as on a miss. A fetched value
// is always returned, and stored as usual. See coalesceFetchAccept for the
// flightKey requirement.
func (c *GitHubResolutionCache) resolveWithFetchAccept(
	ctx context.Context,
	cacheKey, flightKey, credentialID, logRef string,
	isBranchRef bool,
	refreshAllowed func() bool,
	accept func(ResolvedSkill) bool,
	fetch func(context.Context) (ResolvedSkill, error),
) (ResolvedSkill, error) {
	acceptable := func(skill ResolvedSkill) bool { return accept == nil || accept(skill) }

	if skill, ok := c.Get(cacheKey); ok && acceptable(skill) {
		return skill, nil
	}

	if isBranchRef {
		if skill, ok := c.getStale(cacheKey); ok && acceptable(skill) {
			refreshStarted := false
			if refreshAllowed != nil && !refreshAllowed() {
				fmt.Fprintf(os.Stderr, "github: WARNING: serving stale entry for %s; skipping refresh during a rate-limit cooldown\n", logRef)
			} else if c.recentRefreshFailure(flightKey) {
				fmt.Fprintf(os.Stderr, "github: WARNING: serving stale entry for %s; skipping refresh after a recent failure\n", logRef)
			} else {
				// A panic in fetch is recovered inside coalesceFetch's DoChan
				// closure (see its comment), so this goroutine itself cannot
				// panic from that; no recover needed at this level.
				refreshStarted = c.startRefresh(func() {
					_, ferr := c.coalesceFetchAccept(context.Background(), flightKey, credentialID, cacheKey, logRef, isBranchRef, accept, fetch)
					if ferr != nil {
						c.recordRefreshFailure(flightKey)
					} else {
						c.clearRefreshFailure(flightKey)
					}
				})
				if !refreshStarted {
					util.Debugf("github: serving stale entry for %s; not refreshing after Close", logRef)
				}
			}
			injectStaleServe(flightKey, refreshStarted)
			return skill, nil
		}
	}

	if ferr := c.recentFailure(cacheKey); ferr != nil {
		util.Debugf("github: returning remembered not_found for %s", logRef)
		return ResolvedSkill{}, ferr
	}

	return c.coalesceFetchAccept(ctx, flightKey, credentialID, cacheKey, logRef, isBranchRef, accept, fetch)
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
