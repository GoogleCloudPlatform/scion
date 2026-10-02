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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// TestGitHubResolutionCache_ResolveWithFetch_Coalesces is the acceptance test
// for single-flight: N concurrent resolutions of the same ref must make
// exactly one upstream fetch. Synchronization is via channels (entered,
// proceed), not sleeps: the test blocks until the fetch has actually started
// — proving at least one real concurrent caller reached it — before allowing
// it to complete. A goroutine that is still scheduled-but-not-run when
// proceed closes does not invalidate the assertion either: it would pass
// through ResolveWithFetch's own re-check of the now-populated cache instead
// of calling fetch again, so fetchCount == 1 holds regardless of exactly how
// many of the n goroutines had started before the release.
func TestGitHubResolutionCache_ResolveWithFetch_Coalesces(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	var fetchCount int32
	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	fetch := func(ctx context.Context) (ResolvedSkill, error) {
		atomic.AddInt32(&fetchCount, 1)
		enterOnce.Do(func() { close(entered) })
		<-proceed
		return ResolvedSkill{Name: "coalesced", URI: "gh://o/r/s@main"}, nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]ResolvedSkill, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = cache.ResolveWithFetch(context.Background(),
				"coalesce-key", "coalesce-flight", "coalesce-cred", false, fetch)
		}(i)
	}

	<-entered
	close(proceed)
	wg.Wait()

	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("expected exactly 1 upstream fetch for %d concurrent callers, got %d", n, got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: unexpected error: %v", i, errs[i])
		}
		if results[i].Name != "coalesced" {
			t.Errorf("caller %d: expected name %q, got %q", i, "coalesced", results[i].Name)
		}
	}
}

// TestGitHubResolutionCache_ResolveWithFetch_PerCredentialCap is the
// acceptance test for the per-credential in-flight cap: it must be enforced
// across distinct refs (so single-flight cannot coalesce them), not just
// within one ref.
func TestGitHubResolutionCache_ResolveWithFetch_PerCredentialCap(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const extra = 3
	const n = maxInFlightPerCredential + extra
	entered := make(chan struct{}, n)
	proceed := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetch := func(ctx context.Context) (ResolvedSkill, error) {
				entered <- struct{}{}
				<-proceed
				return ResolvedSkill{Name: fmt.Sprintf("skill-%d", i)}, nil
			}
			// Distinct refs (cache/flight keys) but the same credential
			// identity: single-flight cannot coalesce these, so only the
			// per-credential cap can bound their concurrency.
			_, _ = cache.ResolveWithFetch(context.Background(),
				fmt.Sprintf("cap-cache-%d", i), fmt.Sprintf("cap-flight-%d", i), "shared-cred", false, fetch)
		}()
	}

	// Exactly maxInFlightPerCredential fetches can be running at once. This is
	// a correctness invariant, not a timing assumption: the semaphore has no
	// free slot for any more until one of these finishes, so a blocking
	// receive loop is guaranteed to collect exactly this many sends.
	for i := 0; i < maxInFlightPerCredential; i++ {
		<-entered
	}

	// No further caller can have reached the fetch body yet — there is no
	// free slot for it to acquire.
	select {
	case <-entered:
		t.Fatal("more than maxInFlightPerCredential fetches ran concurrently for the same credential")
	default:
	}

	close(proceed)
	wg.Wait()

	// The remaining callers must still have completed (each one eventually
	// acquired a slot once one freed up).
	for i := 0; i < extra; i++ {
		select {
		case <-entered:
		default:
			t.Fatalf("expected %d more fetches to have run after slots freed up", extra)
		}
	}
}

// TestGitHubResolutionCache_ResolveWithFetch_PerCredentialCapIsolatedAcrossProjects
// is the acceptance test that two different credential identities (as
// distinct projects now produce, see flightIdentity in
// github_skill_resolver.go) do not share one slot pool: saturating one
// identity's cap must not block a fetch under a different identity.
func TestGitHubResolutionCache_ResolveWithFetch_PerCredentialCapIsolatedAcrossProjects(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	// Saturate project A's cap.
	enteredA := make(chan struct{}, maxInFlightPerCredential)
	proceedA := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < maxInFlightPerCredential; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetch := func(ctx context.Context) (ResolvedSkill, error) {
				enteredA <- struct{}{}
				<-proceedA
				return ResolvedSkill{Name: fmt.Sprintf("a-%d", i)}, nil
			}
			_, _ = cache.ResolveWithFetch(context.Background(),
				fmt.Sprintf("projA-cache-%d", i), fmt.Sprintf("projA-flight-%d", i), "project-A|default", false, fetch)
		}()
	}
	for i := 0; i < maxInFlightPerCredential; i++ {
		<-enteredA
	}

	// A different project's identity must be able to run its own fetch
	// immediately, even though project A's cap is fully saturated.
	enteredB := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		fetch := func(ctx context.Context) (ResolvedSkill, error) {
			close(enteredB)
			return ResolvedSkill{Name: "b"}, nil
		}
		_, _ = cache.ResolveWithFetch(context.Background(), "projB-cache", "projB-flight", "project-B|default", false, fetch)
		close(doneB)
	}()

	select {
	case <-enteredB:
	case <-time.After(5 * time.Second):
		t.Fatal("project B's fetch never started — it appears to share project A's saturated credential cap")
	}
	<-doneB

	close(proceedA)
	wg.Wait()
}

// TestGitHubResolutionCache_ResolveWithFetch_CancelledLeaderDoesNotFailWaiter
// is the acceptance test for "a cancelled waiter does not cancel the shared
// flight", specifically for the case that matters most: the single-flight
// *leader* itself is cancelled, not some later waiter.
//
// It uses flightJoinHook to know, deterministically and without sleeping or
// polling, that the waiter has actually reached the point of joining the
// leader's still-in-flight call before the leader is cancelled — otherwise a
// race (the leader's flight already failing and being removed before the
// waiter calls DoChan) could let the waiter start a fresh flight of its own
// and still pass, without the test ever having exercised the "does not fail
// the flight" property it claims to. fetch also honors its own context,
// rather than only waiting on proceed: that is what makes the test fail
// under the mutation that removes WithoutCancel from coalesceFetch (which
// would tie the flight's context to the leader's, so the leader's
// cancellation would cancel fetch's context too, before proceed is ever
// closed, and that error would reach the waiter as well).
func TestGitHubResolutionCache_ResolveWithFetch_CancelledLeaderDoesNotFailWaiter(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const flightKey = "cancel-leader-flight"

	var fetchCount int32
	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	// Cleanups run in reverse declaration order, so this runs before
	// TempDir's removal: it releases fetch (a no-op if the test already
	// closed proceed itself) so no goroutine is left blocked past the test.
	t.Cleanup(closeProceed)
	fetch := func(fctx context.Context) (ResolvedSkill, error) {
		atomic.AddInt32(&fetchCount, 1)
		enterOnce.Do(func() { close(entered) })
		select {
		case <-proceed:
			return ResolvedSkill{Name: "ok", URI: "gh://o/r/s@main"}, nil
		case <-fctx.Done():
			return ResolvedSkill{}, fctx.Err()
		}
	}

	var joinCount int32
	waiterJoined := make(chan struct{})
	flightJoinHook = func(key string) {
		if key != flightKey {
			return
		}
		if atomic.AddInt32(&joinCount, 1) == 2 {
			close(waiterJoined)
		}
	}
	t.Cleanup(func() { flightJoinHook = nil })

	ctxLeader, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()

	var resLeader, resWaiter ResolvedSkill
	var errLeader, errWaiter error
	doneLeader := make(chan struct{})
	go func() {
		resLeader, errLeader = cache.ResolveWithFetch(ctxLeader, "cancel-leader-key", flightKey, "cancel-leader-cred", false, fetch)
		close(doneLeader)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader's fetch never started")
	}

	doneWaiter := make(chan struct{})
	go func() {
		resWaiter, errWaiter = cache.ResolveWithFetch(context.Background(), "cancel-leader-key", flightKey, "cancel-leader-cred", false, fetch)
		close(doneWaiter)
	}()

	select {
	case <-waiterJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never reached the flight join point")
	}

	cancelLeader()

	select {
	case <-doneLeader:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled leader did not return promptly")
	}
	if !errors.Is(errLeader, context.Canceled) {
		t.Fatalf("expected context.Canceled for the cancelled leader, got %v", errLeader)
	}
	if resLeader.Name != "" {
		t.Errorf("expected a zero-value result for the cancelled leader, got %+v", resLeader)
	}

	select {
	case <-doneWaiter:
		t.Fatal("the waiter returned before the flight was released — it should still be blocked on proceed")
	default:
	}

	closeProceed() // let the still-running flight finish for the waiter

	select {
	case <-doneWaiter:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not complete after the flight was released")
	}

	if errWaiter != nil {
		t.Errorf("unexpected error for the waiter: %v", errWaiter)
	}
	if resWaiter.Name != "ok" {
		t.Errorf("expected resWaiter.Name = %q, got %q", "ok", resWaiter.Name)
	}
	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("expected exactly 1 fetch — the waiter must join the leader's flight, not start its own — got %d", got)
	}
}

// TestGitHubResolutionCache_ResolveWithFetch_StaleServesImmediately is the
// acceptance test for stale-on-expiry: a branch ref past its TTL but within
// MaxResolutionStaleAge must be served immediately from the stale entry,
// without waiting on a fetch at all — the fetch here blocks forever on an
// unclosed channel, so the test would hang if ResolveWithFetch waited on it.
func TestGitHubResolutionCache_ResolveWithFetch_StaleServesImmediately(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const key = "gh://o/r/s@main"
	now := time.Now()
	cache.mu.Lock()
	cache.entries[key] = &resolutionCacheEntry{
		Skill:       ResolvedSkill{Name: "old", URI: key},
		CachedAt:    now.Add(-time.Hour),        // well within MaxResolutionStaleAge
		ExpiresAt:   now.Add(-30 * time.Minute), // already past TTL
		IsBranchRef: true,
	}
	cache.mu.Unlock()

	entered := make(chan struct{})
	block := make(chan struct{}) // proves ResolveWithFetch did not wait on fetch; released in cleanup
	t.Cleanup(func() {
		close(block)
		// Wait for the background refresh's flight to actually finish before
		// returning: cleanups run in reverse declaration order, so without
		// this, t.TempDir()'s RemoveAll can race the refresh goroutine's
		// putEntry -> save(), which writes github-resolution-cache.json.tmp
		// into the directory while it is being removed ("directory not
		// empty"). Do joins the already-running flight (started by
		// ResolveWithFetch's own background refresh) rather than starting a
		// new one.
		_, _, _ = cache.flight.Do("stale-flight", func() (interface{}, error) { return nil, nil })
	})
	fetch := func(ctx context.Context) (ResolvedSkill, error) {
		close(entered)
		<-block
		return ResolvedSkill{Name: "new", URI: key}, nil
	}

	skill, err := cache.ResolveWithFetch(context.Background(), key, "stale-flight", "stale-cred", true, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.Name != "old" {
		t.Fatalf("expected the stale value %q, got %q", "old", skill.Name)
	}

	// The background refresh must still have been triggered.
	<-entered
}

// TestGitHubResolutionCache_ResolveWithFetch_StaleRefreshesOnce is the
// acceptance test for "refreshes once": two concurrent stale hits for the
// same ref must coalesce into a single background fetch.
func TestGitHubResolutionCache_ResolveWithFetch_StaleRefreshesOnce(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const key = "gh://o/r/s@main"
	now := time.Now()
	cache.mu.Lock()
	cache.entries[key] = &resolutionCacheEntry{
		Skill:       ResolvedSkill{Name: "old", URI: key},
		CachedAt:    now.Add(-time.Hour),
		ExpiresAt:   now.Add(-time.Minute),
		IsBranchRef: true,
	}
	cache.mu.Unlock()

	var fetchCount int32
	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	fetch := func(ctx context.Context) (ResolvedSkill, error) {
		atomic.AddInt32(&fetchCount, 1)
		enterOnce.Do(func() { close(entered) })
		<-proceed
		return ResolvedSkill{Name: "new", URI: key}, nil
	}

	skill1, err1 := cache.ResolveWithFetch(context.Background(), key, "refresh-once-flight", "refresh-once-cred", true, fetch)
	skill2, err2 := cache.ResolveWithFetch(context.Background(), key, "refresh-once-flight", "refresh-once-cred", true, fetch)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v, %v", err1, err2)
	}
	if skill1.Name != "old" || skill2.Name != "old" {
		t.Fatalf("both concurrent stale hits must get the stale value: got %q, %q", skill1.Name, skill2.Name)
	}

	<-entered
	close(proceed)

	// Deterministically wait for the (possibly still in-flight) refresh to
	// land: calling coalesceFetch directly with the same flight key either
	// joins the still-running flight or, if it already finished, hits the
	// fresh-cache re-check — either way it must not invoke fetch again.
	refreshed, err := cache.coalesceFetch(context.Background(), "refresh-once-flight", "refresh-once-cred", key, true, fetch)
	if err != nil {
		t.Fatalf("unexpected error joining the refresh flight: %v", err)
	}
	if refreshed.Name != "new" {
		t.Fatalf("expected the refreshed value %q, got %q", "new", refreshed.Name)
	}

	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("expected exactly 1 upstream fetch for two concurrent stale hits, got %d", got)
	}
}

// TestGitHubResolutionCache_ResolveWithFetch_PastMaxStaleAgeResolvesSynchronously
// is the acceptance test for the hard staleness bound: an entry older than
// MaxResolutionStaleAge must not be served stale — it must be re-resolved
// synchronously instead.
func TestGitHubResolutionCache_ResolveWithFetch_PastMaxStaleAgeResolvesSynchronously(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewGitHubResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const key = "gh://o/r/s@main"
	now := time.Now()
	cache.mu.Lock()
	cache.entries[key] = &resolutionCacheEntry{
		Skill:       ResolvedSkill{Name: "ancient", URI: key},
		CachedAt:    now.Add(-(MaxResolutionStaleAge + time.Hour)), // past the hard cutoff
		ExpiresAt:   now.Add(-time.Hour),
		IsBranchRef: true,
	}
	cache.mu.Unlock()

	var fetchCount int32
	fetch := func(ctx context.Context) (ResolvedSkill, error) {
		atomic.AddInt32(&fetchCount, 1)
		return ResolvedSkill{Name: "fresh", URI: key}, nil
	}

	skill, err := cache.ResolveWithFetch(context.Background(), key, "too-old-flight", "too-old-cred", true, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.Name != "fresh" {
		t.Fatalf("an entry past MaxResolutionStaleAge must not be served stale: got %q", skill.Name)
	}
	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("expected exactly 1 synchronous fetch, got %d", got)
	}
}
