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
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testResolutionCacheSaveDelay is the save delay of caches built by
// newTestResolutionCache: long enough that the delayed write never fires on
// its own during a test, where it could otherwise run after the test has
// returned, possibly while t.TempDir() is being removed.
const testResolutionCacheSaveDelay = time.Hour

// newTestResolutionCache is NewGitHubResolutionCache with
// testResolutionCacheSaveDelay. Tests that check what reaches disk call
// Flush; TestGitHubResolutionCache_DelayedWriteFires covers the timer itself
// with its own short delay.
func newTestResolutionCache(dir string, ttl time.Duration) (*GitHubResolutionCache, error) {
	return NewGitHubResolutionCache(dir, ttl, WithResolutionCacheSaveDelay(testResolutionCacheSaveDelay))
}

func TestGitHubResolutionCache_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
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

	cache.putEntry("gh://owner/repo/my-skill@main", skill, false)

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
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
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
	cache, err := newTestResolutionCache(dir, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	skill := ResolvedSkill{
		Name: "expiring-skill",
		URI:  "gh://owner/repo/expiring@main",
	}
	cache.putEntry("gh://owner/repo/expiring@main", skill, false)

	// Wait for expiry
	time.Sleep(5 * time.Millisecond)

	_, ok := cache.Get("gh://owner/repo/expiring@main")
	if ok {
		t.Fatal("expected cache miss after expiry, got hit")
	}
}

func TestGitHubResolutionCache_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	skill := ResolvedSkill{
		Name:    "persist-skill",
		URI:     "gh://owner/repo/persist@main",
		Version: "abc123def456",
		Hash:    "sha256:persist",
	}
	cache.putEntry("gh://owner/repo/persist@main", skill, false)
	cache.Flush()

	// Verify file exists on disk
	cacheFile := filepath.Join(dir, resolutionCacheFileName)
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file not persisted: %v", err)
	}

	// Create a new cache instance from the same directory
	cache2, err := newTestResolutionCache(dir, 5*time.Minute)
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

// testCredKey returns a cache key in the format resolutionCacheKey produces
// for a credential-scoped ref.
func testCredKey(ref, credential string) string {
	return ref + "#" + credentialFingerprint(credential)
}

// TestGitHubResolutionCache_CredentialEntryPersisted verifies that an entry
// resolved with a credential survives a save and a reload, and that the
// credential value itself never appears in the file.
func TestGitHubResolutionCache_CredentialEntryPersisted(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const credential = "ghp_exampleCredentialValue0123456789"
	credKey := testCredKey("gh://owner/repo/my-skill@main", credential)
	skill := ResolvedSkill{
		Name:    "private-skill",
		URI:     "gh://owner/repo/my-skill@main",
		Version: "abc123def456",
		Hash:    "sha256:private",
		Files: []ResolvedFile{
			{Path: "SKILL.md", URL: "https://raw.githubusercontent.com/owner/repo/abc/SKILL.md", Content: []byte("private content")},
		},
	}
	cache.putEntry(credKey, skill, true)
	cache.Flush()

	data, err := os.ReadFile(filepath.Join(dir, resolutionCacheFileName))
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	if strings.Contains(string(data), credential) {
		t.Fatal("cache file contains the raw credential value")
	}
	if strings.Contains(string(data), "private content") {
		t.Fatal("cache file contains file content; Content must not be persisted")
	}

	cache2, err := newTestResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache (reload): %v", err)
	}
	got, ok := cache2.Get(credKey)
	if !ok {
		t.Fatal("credential-scoped entry did not survive a reload")
	}
	if got.Name != "private-skill" || len(got.Files) != 1 {
		t.Fatalf("unexpected reloaded entry: %+v", got)
	}
	if got.Files[0].Content != nil {
		t.Error("reloaded entry unexpectedly has file content")
	}
}

// TestGitHubResolutionCache_MixedPublicAndCredential verifies that public and
// credential-scoped entries are both persisted.
func TestGitHubResolutionCache_MixedPublicAndCredential(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	publicKey := "gh://owner/repo/pub-skill@main"
	credKey := testCredKey("gh://owner/repo/priv-skill@main", "cred-value")

	cache.putEntry(publicKey, ResolvedSkill{Name: "pub-skill", URI: publicKey}, false)
	cache.putEntry(credKey, ResolvedSkill{Name: "priv-skill", URI: "gh://owner/repo/priv-skill@main"}, false)
	cache.Flush()

	cache2, err := newTestResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache (reload): %v", err)
	}
	if _, ok := cache2.Get(publicKey); !ok {
		t.Error("public entry: expected disk hit after reload")
	}
	if _, ok := cache2.Get(credKey); !ok {
		t.Error("credential entry: expected disk hit after reload")
	}
}

func TestGitHubResolutionCache_ExpiredNotLoaded(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	skill := ResolvedSkill{Name: "expired-skill", URI: "gh://o/r/s@main"}
	cache.putEntry("gh://o/r/s@main", skill, false)
	cache.Flush()

	time.Sleep(5 * time.Millisecond)

	// Reload — expired entries should not be loaded
	cache2, err := newTestResolutionCache(dir, 5*time.Minute)
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
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
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
				"coalesce-key", "coalesce-flight", "coalesce-cred", "test-ref", false, nil, fetch)
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
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
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
				fmt.Sprintf("cap-cache-%d", i), fmt.Sprintf("cap-flight-%d", i), "shared-cred", "test-ref", false, nil, fetch)
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

	// Bounded: if a slot were leaked (release() not called on some path),
	// wg.Wait() would never return and this would otherwise hang until the
	// surrounding test binary's own timeout, far later than a credential-slot
	// leak needs to be caught.
	wgDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(5 * time.Second):
		t.Fatal("wg.Wait() did not return — a credential slot was likely leaked (release() not called on some path)")
	}

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
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
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
				fmt.Sprintf("projA-cache-%d", i), fmt.Sprintf("projA-flight-%d", i), "project-A|default", "test-ref", false, nil, fetch)
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
		_, _ = cache.ResolveWithFetch(context.Background(), "projB-cache", "projB-flight", "project-B|default", "test-ref", false, nil, fetch)
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

// TestGitHubResolutionCache_ResolveWithFetch_CredSlotsReturnsToEmpty is the
// acceptance test for bounding credSlots' size: each credentialID (which
// includes a fingerprint of the credential's own value, see flightIdentity)
// is typically used for only one resolution when it comes from a per-mint
// credential, such as a GitHub App installation token minted fresh for every
// create on the broker fallback path — so every entry must be removed once
// nothing still references it, or the map grows forever.
func TestGitHubResolutionCache_ResolveWithFetch_CredSlotsReturnsToEmpty(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetch := func(ctx context.Context) (ResolvedSkill, error) {
				return ResolvedSkill{Name: fmt.Sprintf("skill-%d", i)}, nil
			}
			_, _ = cache.ResolveWithFetch(context.Background(),
				fmt.Sprintf("credslot-cache-%d", i), fmt.Sprintf("credslot-flight-%d", i),
				fmt.Sprintf("credslot-cred-%d", i), "test-ref", false, nil, fetch)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent resolutions did not complete")
	}

	cache.credMu.Lock()
	got := len(cache.credSlots)
	cache.credMu.Unlock()
	if got != 0 {
		t.Fatalf("expected credSlots to be empty once every resolution finished, got %d entries", got)
	}
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
	cache, err := newTestResolutionCache(dir, 5*time.Minute)
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
	hook := func(key string) {
		if key != flightKey {
			return
		}
		if atomic.AddInt32(&joinCount, 1) == 2 {
			close(waiterJoined)
		}
	}
	flightJoinHook.Store(&hook)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	ctxLeader, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()

	var resLeader, resWaiter ResolvedSkill
	var errLeader, errWaiter error
	doneLeader := make(chan struct{})
	go func() {
		resLeader, errLeader = cache.ResolveWithFetch(ctxLeader, "cancel-leader-key", flightKey, "cancel-leader-cred", "test-ref", false, nil, fetch)
		close(doneLeader)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader's fetch never started")
	}

	doneWaiter := make(chan struct{})
	go func() {
		resWaiter, errWaiter = cache.ResolveWithFetch(context.Background(), "cancel-leader-key", flightKey, "cancel-leader-cred", "test-ref", false, nil, fetch)
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

// TestGitHubResolutionCache_ResolveWithFetch_ShortDeadlineLeaderDoesNotFailWaiter
// is the acceptance test for bounding the flight by the fixed ceiling only:
// a leader with a short deadline must not fail a waiter that has none (or a
// later one), once that deadline passes. The flight itself keeps running —
// fetch here honors its own context (the flight's, not any one caller's) and
// only returns early if *that* is cancelled, which a correct bound never
// does from the leader's deadline alone.
func TestGitHubResolutionCache_ResolveWithFetch_ShortDeadlineLeaderDoesNotFailWaiter(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), 5*time.Minute)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const flightKey = "short-deadline-flight"

	entered := make(chan struct{})
	var enterOnce sync.Once
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	closeProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	t.Cleanup(closeProceed)
	// flightCtx captures the context fetch actually runs under — the
	// flight's own, shared by leader and waiter alike — the first (and only;
	// singleflight calls fetch once here) time fetch runs, so it can be
	// inspected deterministically below instead of racing a timer against
	// whether a wrongly-applied leader deadline fires.
	var flightCtx context.Context
	fetch := func(fctx context.Context) (ResolvedSkill, error) {
		enterOnce.Do(func() {
			flightCtx = fctx
			close(entered)
		})
		select {
		case <-proceed:
			return ResolvedSkill{Name: "ok"}, nil
		case <-fctx.Done():
			return ResolvedSkill{}, fctx.Err()
		}
	}

	var joinCount int32
	waiterJoined := make(chan struct{})
	hook := func(key string) {
		if key != flightKey {
			return
		}
		if atomic.AddInt32(&joinCount, 1) == 2 {
			close(waiterJoined)
		}
	}
	flightJoinHook.Store(&hook)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	leaderCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() {
		_, _ = cache.ResolveWithFetch(leaderCtx, "short-deadline-key", flightKey, "short-deadline-cred", "test-ref", false, nil, fetch)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("leader's fetch never started")
	}

	var werr error
	var wres ResolvedSkill
	done := make(chan struct{})
	go func() {
		wres, werr = cache.ResolveWithFetch(context.Background(), "short-deadline-key", flightKey, "short-deadline-cred", "test-ref", false, nil, fetch)
		close(done)
	}()

	select {
	case <-waiterJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never joined")
	}

	<-leaderCtx.Done() // let the leader's own deadline pass

	// Deterministic check, no wait: the flight's own context must still be
	// alive, with a deadline later than the leader's — proving the flight was
	// never tied to the leader's own deadline, the moment that deadline has
	// passed, rather than racing a timer against whether the wrongly-applied
	// bound happens to have fired yet.
	if err := flightCtx.Err(); err != nil {
		t.Fatalf("flight context ended when the leader's own deadline passed: %v", err)
	}
	leaderDeadline, _ := leaderCtx.Deadline()
	flightDeadline, ok := flightCtx.Deadline()
	if !ok {
		t.Fatal("expected the flight context to carry the fixed ceiling deadline")
	}
	if !flightDeadline.After(leaderDeadline) {
		t.Fatalf("flight deadline %v is not after the leader's deadline %v — the flight may be bound to the leader's own deadline", flightDeadline, leaderDeadline)
	}

	closeProceed()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter hung")
	}

	if werr != nil {
		t.Fatalf("waiter with no deadline failed after the leader's deadline passed: %v", werr)
	}
	if wres.Name != "ok" {
		t.Fatalf("expected resWaiter.Name = %q, got %q", "ok", wres.Name)
	}
}

// TestGitHubResolutionCache_ResolveWithFetch_StaleServesImmediately is the
// acceptance test for stale-on-expiry: a branch ref past its TTL but within
// MaxResolutionStaleAge must be served immediately from the stale entry,
// without waiting on a fetch at all — the fetch here blocks forever on an
// unclosed channel, so the test would hang if ResolveWithFetch waited on it.
func TestGitHubResolutionCache_ResolveWithFetch_StaleServesImmediately(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, time.Hour)
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

	skill, err := cache.ResolveWithFetch(context.Background(), key, "stale-flight", "stale-cred", "test-ref", true, nil, fetch)
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
	cache, err := newTestResolutionCache(dir, time.Hour)
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

	// Each of the two ResolveWithFetch calls below independently sees the
	// entry as stale (neither has refreshed it yet) and so spawns its own
	// background refresh goroutine. Both goroutines call injectFlightJoin,
	// an unsynchronized package-var read/write before this test's own
	// atomic.Pointer fix — install a hook that waits for both before this
	// test does anything that could race with a later test's own hook, so
	// neither goroutine is ever still mid-read when that happens.
	const flightKey = "refresh-once-flight"
	var joinCount int32
	bothJoined := make(chan struct{})
	var joinedOnce sync.Once
	hook := func(k string) {
		if k != flightKey {
			return
		}
		if atomic.AddInt32(&joinCount, 1) == 2 {
			joinedOnce.Do(func() { close(bothJoined) })
		}
	}
	flightJoinHook.Store(&hook)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	skill1, err1 := cache.ResolveWithFetch(context.Background(), key, flightKey, "refresh-once-cred", "test-ref", true, nil, fetch)
	skill2, err2 := cache.ResolveWithFetch(context.Background(), key, flightKey, "refresh-once-cred", "test-ref", true, nil, fetch)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errors: %v, %v", err1, err2)
	}
	if skill1.Name != "old" || skill2.Name != "old" {
		t.Fatalf("both concurrent stale hits must get the stale value: got %q, %q", skill1.Name, skill2.Name)
	}

	<-entered

	select {
	case <-bothJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("did not observe both background refresh goroutines joining the flight")
	}

	close(proceed)

	// Deterministically wait for the (possibly still in-flight) refresh to
	// land: calling coalesceFetch directly with the same flight key either
	// joins the still-running flight or, if it already finished, hits the
	// fresh-cache re-check — either way it must not invoke fetch again.
	refreshed, err := cache.coalesceFetch(context.Background(), flightKey, "refresh-once-cred", key, "test-ref", true, fetch)
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
	cache, err := newTestResolutionCache(dir, time.Hour)
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

	skill, err := cache.ResolveWithFetch(context.Background(), key, "too-old-flight", "too-old-cred", "test-ref", true, nil, fetch)
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

// TestGitHubResolutionCache_ResolveWithFetch_SHARefNeverServedStale is the
// acceptance test for excluding commit-SHA refs from stale-serve: a SHA ref
// is immutable once resolved, so "stale" has no meaning for it the way it
// does for a branch ref — an expired SHA-ref entry must always trigger a
// synchronous re-resolution, never the stale-serve path, regardless of how
// recently it was cached (even well within MaxResolutionStaleAge).
func TestGitHubResolutionCache_ResolveWithFetch_SHARefNeverServedStale(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const key = "gh://o/r/s@abc123"
	now := time.Now()
	cache.mu.Lock()
	cache.entries[key] = &resolutionCacheEntry{
		Skill:       ResolvedSkill{Name: "old-sha-content", URI: key},
		CachedAt:    now.Add(-time.Minute), // well within MaxResolutionStaleAge
		ExpiresAt:   now.Add(-time.Second), // already past TTL
		IsBranchRef: false,                 // a SHA-ref entry
	}
	cache.mu.Unlock()

	var fetchCount int32
	fetch := func(ctx context.Context) (ResolvedSkill, error) {
		atomic.AddInt32(&fetchCount, 1)
		return ResolvedSkill{Name: "fresh-sha-content", URI: key}, nil
	}

	// isBranchRef=false, matching what resolveOne computes for a full commit
	// SHA ref — this is the one thing that must keep it off the stale path.
	skill, err := cache.ResolveWithFetch(context.Background(), key, "sha-flight", "sha-cred", "test-ref", false, nil, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.Name != "fresh-sha-content" {
		t.Fatalf("a SHA ref past its TTL must never be served stale, even within MaxResolutionStaleAge: got %q", skill.Name)
	}
	if got := atomic.LoadInt32(&fetchCount); got != 1 {
		t.Fatalf("expected exactly 1 synchronous fetch for an expired SHA ref, got %d", got)
	}
}

// TestGitHubResolutionCache_ResolveWithFetch_RefreshFailureBackoffSkipsRetry
// is the acceptance test for the refresh-failure backoff: once a background
// refresh has failed recently for a flight key, a later stale hit under that
// key must not start another one — it must keep serving the stale value,
// with no background refresh launched at all, until the backoff window
// passes. The failure is primed directly via recordRefreshFailure, exactly
// as the background goroutine in ResolveWithFetch would have left it after a
// real failure, rather than racing to observe that goroutine's own
// completion from an actual failing fetch.
//
// Checking "no flight was started" still needs a bound, since the decision
// to skip is made synchronously inside ResolveWithFetch but a wrongly
// launched refresh runs in its own goroutine: flightJoinHook (fired as the
// first statement of coalesceFetch, before any actual fetch work) gives the
// earliest possible signal of that goroutine running, so the bound below
// only has to cover it getting scheduled at all, not completing any work.
func TestGitHubResolutionCache_ResolveWithFetch_RefreshFailureBackoffSkipsRetry(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const key = "gh://o/r/s@main"
	const flightKey = "backoff-flight"
	now := time.Now()
	cache.mu.Lock()
	cache.entries[key] = &resolutionCacheEntry{
		Skill:       ResolvedSkill{Name: "old", URI: key},
		CachedAt:    now.Add(-time.Hour),
		ExpiresAt:   now.Add(-time.Minute),
		IsBranchRef: true,
	}
	cache.mu.Unlock()

	cache.recordRefreshFailure(flightKey)

	flightStarted := make(chan struct{})
	var startedOnce sync.Once
	hook := func(key string) {
		if key == flightKey {
			startedOnce.Do(func() { close(flightStarted) })
		}
	}
	flightJoinHook.Store(&hook)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	var fetchCount int32
	fetch := func(ctx context.Context) (ResolvedSkill, error) {
		atomic.AddInt32(&fetchCount, 1)
		return ResolvedSkill{Name: "new", URI: key}, nil
	}

	skill, err := cache.ResolveWithFetch(context.Background(), key, flightKey, "backoff-cred", "test-ref", true, nil, fetch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skill.Name != "old" {
		t.Fatalf("expected the stale value %q while backed off, got %q", "old", skill.Name)
	}

	select {
	case <-flightStarted:
		t.Fatal("a background refresh was started while within the refresh-failure backoff window")
	case <-time.After(200 * time.Millisecond):
	}

	if got := atomic.LoadInt32(&fetchCount); got != 0 {
		t.Fatalf("expected no fetch while within the refresh-failure backoff window, got %d", got)
	}
}

// TestGitHubResolutionCache_ResolveWithFetch_FailureNeverCached is the
// acceptance test that a failed fetch is never cached or served stale: a
// regression here would store the zero ResolvedSkill as a successful result,
// serving an empty skill for the full TTL instead of surfacing the error (or
// retrying) on the next call.
func TestGitHubResolutionCache_ResolveWithFetch_FailureNeverCached(t *testing.T) {
	dir := t.TempDir()
	cache, err := newTestResolutionCache(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewGitHubResolutionCache: %v", err)
	}

	const key = "gh://o/r/s@main"
	wantErr := errors.New("boom")
	var fetchCount int32
	fetch := func(ctx context.Context) (ResolvedSkill, error) {
		atomic.AddInt32(&fetchCount, 1)
		return ResolvedSkill{}, wantErr
	}

	_, err1 := cache.ResolveWithFetch(context.Background(), key, "failure-flight", "failure-cred", "test-ref", true, nil, fetch)
	if !errors.Is(err1, wantErr) {
		t.Fatalf("expected the first call to fail with %v, got %v", wantErr, err1)
	}

	// Nothing from the failed call should have been cached: a second call
	// must run fetch again, not return a cached (zero-value) success.
	_, err2 := cache.ResolveWithFetch(context.Background(), key, "failure-flight", "failure-cred", "test-ref", true, nil, fetch)
	if !errors.Is(err2, wantErr) {
		t.Fatalf("expected the second call to fail again (a prior failure must never be cached), got %v", err2)
	}
	if got := atomic.LoadInt32(&fetchCount); got != 2 {
		t.Fatalf("expected fetch to run on both calls (no cached failure or empty success), got %d calls", got)
	}
}

// TestJitteredTTL_DeterministicWithSeededRand pins JitteredTTL's two
// contracts using an injectable rand source (no reliance on math/rand's
// global state, so this is reproducible): the same source sequence always
// produces the same result, and the result always falls within
// +/-ttlJitterFraction of the nominal TTL.
func TestJitteredTTL_DeterministicWithSeededRand(t *testing.T) {
	const ttl = 30 * time.Minute
	spread := time.Duration(float64(ttl) * ttlJitterFraction)

	got1 := JitteredTTL(ttl, rand.New(rand.NewSource(1)).Float64)
	got2 := JitteredTTL(ttl, rand.New(rand.NewSource(1)).Float64)
	if got1 != got2 {
		t.Fatalf("expected the same seed to reproduce the same jittered TTL, got %v and %v", got1, got2)
	}
	if got1 < ttl-spread || got1 > ttl+spread {
		t.Fatalf("expected a jittered TTL within +/-%v of %v, got %v", spread, ttl, got1)
	}

	// A different seed must be able to produce a different result — this is
	// the whole point: entries written together must not all land on
	// exactly the same ExpiresAt.
	got3 := JitteredTTL(ttl, rand.New(rand.NewSource(2)).Float64)
	if got3 == got1 {
		t.Fatalf("expected a different seed to produce a different jittered TTL (or this test's two seeds collided), got %v for both", got1)
	}
}

// TestJitteredTTL_Extremes pins the exact endpoints and midpoint of the
// jitter range against known rand.Float64 outputs (0, 0.5, and just under
// 1), rather than only the seeded-reproducibility property above.
func TestJitteredTTL_Extremes(t *testing.T) {
	const ttl = time.Hour
	spread := time.Duration(float64(ttl) * ttlJitterFraction)

	cases := []struct {
		name string
		r    float64
		want time.Duration
	}{
		{"minimum", 0, ttl - spread},
		{"midpoint", 0.5, ttl},
		{"just under maximum", 1 - 1e-9, ttl + spread}, // rand.Float64 never returns exactly 1
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JitteredTTL(ttl, func() float64 { return tc.r })
			diff := got - tc.want
			if diff < 0 {
				diff = -diff
			}
			if diff > time.Microsecond {
				t.Errorf("JitteredTTL(%v, r=%v) = %v, want %v", ttl, tc.r, got, tc.want)
			}
		})
	}
}

// TestMaxJitteredTTL_NeverExceeded confirms MaxJitteredTTL is an actual
// upper bound on every value JitteredTTL can produce — the property
// staleCutoff (pkg/hub/github_resolution_store.go) depends on to derive a
// safe (never-too-fresh) lastResolvedAt from a jittered ExpiresAt.
func TestMaxJitteredTTL_NeverExceeded(t *testing.T) {
	const ttl = 45 * time.Minute
	max := MaxJitteredTTL(ttl)
	for _, r := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.9, 1 - 1e-9} {
		got := JitteredTTL(ttl, func() float64 { return r })
		if got > max {
			t.Errorf("JitteredTTL(%v, r=%v) = %v exceeds MaxJitteredTTL(%v) = %v", ttl, r, got, ttl, max)
		}
	}
}

// TestGitHubResolutionCache_PutEntryJittersExpiry reads back stored entries
// and checks the TTL was jittered on write: each entry's ExpiresAt is
// within +/-ttlJitterFraction of the nominal TTL after its CachedAt, and
// the entries do not all sit exactly on the nominal TTL.
func TestGitHubResolutionCache_PutEntryJittersExpiry(t *testing.T) {
	const ttl = 30 * time.Minute
	cache, err := newTestResolutionCache(t.TempDir(), ttl)
	if err != nil {
		t.Fatal(err)
	}
	spread := time.Duration(float64(ttl) * ttlJitterFraction)

	const n = 8
	for i := 0; i < n; i++ {
		cache.putEntry(fmt.Sprintf("gh://o/r/s%d@main", i), ResolvedSkill{Name: "s"}, true)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != n {
		t.Fatalf("got %d entries, want %d", len(cache.entries), n)
	}
	offNominal := 0
	for key, e := range cache.entries {
		got := e.ExpiresAt.Sub(e.CachedAt)
		if got < ttl-spread || got > ttl+spread {
			t.Errorf("%s: stored TTL %v outside +/-%v of %v", key, got, spread, ttl)
		}
		if got != ttl {
			offNominal++
		}
	}
	if offNominal == 0 {
		t.Errorf("all %d entries store exactly the nominal TTL %v; expiry is not jittered on write", n, ttl)
	}
}

// TestGitHubResolutionCache_SharedCredentialSlotOutlivesFirstRelease checks
// that when two callers hold slots for the same credential, the first
// release does not drop the credential's semaphore: the second holder's
// slot is still counted, so the cap still applies, and the entry is removed
// only after the last release.
func TestGitHubResolutionCache_SharedCredentialSlotOutlivesFirstRelease(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const cred = "shared-cred"

	releaseA, err := cache.acquireCredentialSlot(ctx, cred)
	if err != nil {
		t.Fatal(err)
	}
	releaseB, err := cache.acquireCredentialSlot(ctx, cred)
	if err != nil {
		t.Fatal(err)
	}
	cache.credMu.Lock()
	slot := cache.credSlots[cred]
	cache.credMu.Unlock()

	releaseA()

	cache.credMu.Lock()
	after := cache.credSlots[cred]
	cache.credMu.Unlock()
	if after != slot {
		t.Fatalf("credential slot entry replaced or removed after the first release while another caller still holds it")
	}

	// B's slot is still counted: only maxInFlightPerCredential-1 more fit.
	var more []func()
	for i := 0; i < maxInFlightPerCredential-1; i++ {
		r, err := cache.acquireCredentialSlot(ctx, cred)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		more = append(more, r)
	}
	full, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if r, err := cache.acquireCredentialSlot(full, cred); err == nil {
		r()
		t.Fatal("acquired a slot beyond the per-credential cap; the remaining holder's slot was not counted")
	}

	releaseB()
	for _, r := range more {
		r()
	}
	cache.credMu.Lock()
	defer cache.credMu.Unlock()
	if _, ok := cache.credSlots[cred]; ok {
		t.Fatal("credential slot entry still present after every holder released")
	}
}
