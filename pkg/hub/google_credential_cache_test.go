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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingBaseValidator is a GoogleCredentialValidator whose ID/access token
// results are scripted per-call, counting how many times each method is
// actually invoked. Used to prove the caching decorator's cache-hit and
// singleflight behaviour: a mutation that skips the cache lookup, or that
// lets concurrent callers each dial upstream, must move this counter.
type countingBaseValidator struct {
	mu sync.Mutex

	idTokenCalls     int
	accessTokenCalls int

	idTokenResult *ValidatedGoogleIdentity
	idTokenErr    error

	accessTokenResult *ValidatedGoogleIdentity
	accessTokenErr    error

	// delay, if set, is slept before returning — used to widen the window
	// for concurrent callers to race into the same singleflight key.
	delay time.Duration
}

func (v *countingBaseValidator) ValidateIDToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	v.mu.Lock()
	v.idTokenCalls++
	v.mu.Unlock()
	if v.delay > 0 {
		time.Sleep(v.delay)
	}
	return v.idTokenResult, v.idTokenErr
}

func (v *countingBaseValidator) ValidateAccessToken(_ context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	v.mu.Lock()
	v.accessTokenCalls++
	v.mu.Unlock()
	if v.delay > 0 {
		time.Sleep(v.delay)
	}
	return v.accessTokenResult, v.accessTokenErr
}

func (v *countingBaseValidator) totalIDTokenCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.idTokenCalls
}

func (v *countingBaseValidator) totalAccessTokenCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.accessTokenCalls
}

// ---------------------------------------------------------------------------
// Cache key.
// ---------------------------------------------------------------------------

func TestGoogleCredentialCache_KeyDependsOnTokenAndAudience(t *testing.T) {
	k1 := cacheKey("token-a", []string{"aud-1"})
	k2 := cacheKey("token-b", []string{"aud-1"})
	if k1 == k2 {
		t.Error("different tokens must not collide to the same cache key")
	}
	k3 := cacheKey("token-a", []string{"aud-2"})
	if k1 == k3 {
		t.Error("different audiences must not collide to the same cache key")
	}
}

func TestGoogleCredentialCache_KeyIgnoresAudienceOrder(t *testing.T) {
	k1 := cacheKey("token-a", []string{"aud-1", "aud-2"})
	k2 := cacheKey("token-a", []string{"aud-2", "aud-1"})
	if k1 != k2 {
		t.Error("cache key must not depend on the order of the audience list (design §4.2(iii): sorted(aud))")
	}
}

// ---------------------------------------------------------------------------
// Repeated validation of the same token within the TTL costs exactly one
// upstream call.
// ---------------------------------------------------------------------------

func TestGoogleCredentialCache_HitAvoidsUpstreamCall(t *testing.T) {
	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base)

	for i := 0; i < 5; i++ {
		id, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"})
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if id.Subject != "sub-1" {
			t.Fatalf("call %d: subject = %q, want sub-1", i, id.Subject)
		}
	}
	if got := base.totalIDTokenCalls(); got != 1 {
		t.Errorf("base validator called %d times, want 1 (5 requests for the same token must share one upstream call)", got)
	}
}

func TestGoogleCredentialCache_DifferentTokensDoNotShareEntry(t *testing.T) {
	base := &countingBaseValidator{
		accessTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base)

	if _, err := cache.ValidateAccessToken(context.Background(), "token-1", []string{"aud"}); err != nil {
		t.Fatalf("token-1: %v", err)
	}
	if _, err := cache.ValidateAccessToken(context.Background(), "token-2", []string{"aud"}); err != nil {
		t.Fatalf("token-2: %v", err)
	}
	if got := base.totalAccessTokenCalls(); got != 2 {
		t.Errorf("base validator called %d times, want 2 (distinct tokens must not share a cache entry)", got)
	}
}

// ---------------------------------------------------------------------------
// A cache entry never outlives the credential's own UpstreamExpiry, even
// when that is shorter than maxTTL.
// ---------------------------------------------------------------------------

func TestGoogleCredentialCache_TTLCappedByUpstreamExpiry(t *testing.T) {
	// A fake clock far from the real wall clock, so the test can't
	// accidentally pass by coincidence if a real-time call sneaks in — e.g. a
	// regression to time.Until(identity.UpstreamExpiry) (the real clock)
	// instead of identity.UpstreamExpiry.Sub(c.now()) (the fake one) would
	// compute a wildly wrong remaining lifetime here and fail loudly, instead
	// of silently passing because the fake clock happened to start near the
	// real one.
	now := time.Now().AddDate(50, 0, 0)
	clock := now
	nowFunc := func() time.Time { return clock }

	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			// Expires in 10s — far shorter than the 5-minute maxTTL default.
			UpstreamExpiry: now.Add(10 * time.Second),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base, withCacheNowFunc(nowFunc))

	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := base.totalIDTokenCalls(); got != 1 {
		t.Fatalf("after first call, base calls = %d, want 1", got)
	}

	// Still within the credential's own remaining lifetime: must be a hit.
	clock = now.Add(5 * time.Second)
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := base.totalIDTokenCalls(); got != 1 {
		t.Errorf("after second call (within upstream expiry), base calls = %d, want 1 (still cached)", got)
	}

	// Past the credential's own expiry, even though maxTTL (5m) has not
	// elapsed: the entry must be gone, and the next call must hit upstream.
	clock = now.Add(11 * time.Second)
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if got := base.totalIDTokenCalls(); got != 2 {
		t.Errorf("after third call (past upstream expiry), base calls = %d, want 2 (cache entry must not outlive UpstreamExpiry)", got)
	}
}

func TestGoogleCredentialCache_MaxTTLCapsLongLivedCredential(t *testing.T) {
	// A fake clock far from the real wall clock, so the test can't
	// accidentally pass by coincidence if a real-time call sneaks in.
	now := time.Now().AddDate(50, 0, 0)
	clock := now
	nowFunc := func() time.Time { return clock }

	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			// Far outlives the configured 1-minute maxTTL.
			UpstreamExpiry: now.Add(24 * time.Hour),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base, withCacheNowFunc(nowFunc), WithCacheMaxTTL(time.Minute))

	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	clock = now.Add(61 * time.Second)
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := base.totalIDTokenCalls(); got != 2 {
		t.Errorf("base calls = %d, want 2 (maxTTL must cap even a long-lived credential)", got)
	}
}

// ---------------------------------------------------------------------------
// An upstream fault (ErrGoogleUpstreamError) is never negatively cached:
// the next request must retry upstream, not replay the failure.
// ---------------------------------------------------------------------------

func TestGoogleCredentialCache_UpstreamErrorNeverCached(t *testing.T) {
	base := &countingBaseValidator{accessTokenErr: ErrGoogleUpstreamError}
	cache := NewCachingGoogleCredentialValidator(base)

	for i := 0; i < 3; i++ {
		_, err := cache.ValidateAccessToken(context.Background(), "token", []string{"aud"})
		if err == nil {
			t.Fatalf("call %d: expected an error", i)
		}
	}
	if got := base.totalAccessTokenCalls(); got != 3 {
		t.Errorf("base validator called %d times, want 3 (ErrGoogleUpstreamError must never be cached; every call retries upstream)", got)
	}
}

// TestGoogleCredentialCache_NegativeCacheOnlyForFourListedErrors proves the
// negative-cache allowlist is exact: the four named errors are cached (one
// upstream call for repeated attempts within negTTL); everything else,
// including errors that are neither on the allowlist nor ErrGoogleUpstreamError,
// is never cached, matching the conservative "only for the four listed
// errors" policy.
func TestGoogleCredentialCache_NegativeCacheOnlyForFourListedErrors(t *testing.T) {
	cacheableErrs := []error{
		ErrGoogleInvalidCredential,
		ErrGoogleExpiredCredential,
		ErrGoogleUntrustedAudience,
		ErrGoogleUnverifiedEmail,
	}
	for _, wantErr := range cacheableErrs {
		t.Run(wantErr.Error(), func(t *testing.T) {
			base := &countingBaseValidator{idTokenErr: wantErr}
			cache := NewCachingGoogleCredentialValidator(base)
			for i := 0; i < 3; i++ {
				if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err == nil {
					t.Fatalf("call %d: expected an error", i)
				}
			}
			if got := base.totalIDTokenCalls(); got != 1 {
				t.Errorf("base validator called %d times, want 1 (%v must be negatively cached)", got, wantErr)
			}
		})
	}

	nonCacheableErrs := []error{
		ErrGoogleUpstreamError,
		ErrGoogleFieldDisagreement,
		ErrGoogleMissingSubject,
		ErrGoogleMissingField,
		ErrGENotConfigured,
		ErrGENoRemainingLifetime,
	}
	for _, wantErr := range nonCacheableErrs {
		t.Run(wantErr.Error(), func(t *testing.T) {
			base := &countingBaseValidator{idTokenErr: wantErr}
			cache := NewCachingGoogleCredentialValidator(base)
			for i := 0; i < 3; i++ {
				if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err == nil {
					t.Fatalf("call %d: expected an error", i)
				}
			}
			if got := base.totalIDTokenCalls(); got != 3 {
				t.Errorf("base validator called %d times, want 3 (%v must never be cached)", got, wantErr)
			}
		})
	}
}

func TestGoogleCredentialCache_NegativeEntryExpiresAfterNegTTL(t *testing.T) {
	// A fake clock far from the real wall clock, so the test can't
	// accidentally pass by coincidence if a real-time call sneaks in.
	now := time.Now().AddDate(50, 0, 0)
	clock := now
	nowFunc := func() time.Time { return clock }

	base := &countingBaseValidator{idTokenErr: ErrGoogleUnverifiedEmail}
	cache := NewCachingGoogleCredentialValidator(base, withCacheNowFunc(nowFunc), WithCacheNegativeTTL(30*time.Second))

	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err == nil {
		t.Fatal("expected an error")
	}
	clock = now.Add(31 * time.Second)
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err == nil {
		t.Fatal("expected an error")
	}
	if got := base.totalIDTokenCalls(); got != 2 {
		t.Errorf("base validator called %d times, want 2 (negative entry must expire after negTTL)", got)
	}
}

// ---------------------------------------------------------------------------
// Concurrent first requests for the same token collapse into one
// upstream call (singleflight).
// ---------------------------------------------------------------------------

func TestGoogleCredentialCache_SingleflightCollapsesConcurrentMisses(t *testing.T) {
	base := &countingBaseValidator{
		delay: 20 * time.Millisecond,
		accessTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base)

	const n = 50
	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := cache.ValidateAccessToken(context.Background(), "same-token", []string{"aud"}); err != nil {
				errCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if errCount.Load() != 0 {
		t.Fatalf("%d of %d concurrent calls returned an error", errCount.Load(), n)
	}
	if got := base.totalAccessTokenCalls(); got != 1 {
		t.Errorf("base validator called %d times for %d concurrent requests of the same token, want 1", got, n)
	}
}

// ---------------------------------------------------------------------------
// Cached() — used by the external-bearer rate limiter to skip cache hits.
// ---------------------------------------------------------------------------

func TestGoogleCredentialCache_CachedReportsHitsAndMisses(t *testing.T) {
	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base).(*cachingGoogleCredentialValidator)

	if cache.Cached("token", []string{"aud"}) {
		t.Error("Cached() = true before any validation; want false")
	}
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("ValidateIDToken: %v", err)
	}
	if !cache.Cached("token", []string{"aud"}) {
		t.Error("Cached() = false after a successful validation; want true")
	}
	if cache.Cached("other-token", []string{"aud"}) {
		t.Error("Cached() = true for a different, never-validated token; want false")
	}
}

// ---------------------------------------------------------------------------
// Eviction — evict-expired-then-refuse-insert at maxEntries, with no LRU.
// ---------------------------------------------------------------------------

func TestGoogleCredentialCache_RefusesInsertWhenFullOfLiveEntries(t *testing.T) {
	// A fake clock far from the real wall clock, so the test can't
	// accidentally pass by coincidence if a real-time call sneaks in.
	now := time.Now().AddDate(50, 0, 0)
	clock := now
	nowFunc := func() time.Time { return clock }

	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: now.Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base, withCacheNowFunc(nowFunc), WithCacheMaxEntries(2)).(*cachingGoogleCredentialValidator)

	for _, tok := range []string{"token-1", "token-2"} {
		if _, err := cache.ValidateIDToken(context.Background(), tok, []string{"aud"}); err != nil {
			t.Fatalf("%s: %v", tok, err)
		}
	}
	// A third distinct token: the cache is full of two live entries, so this
	// result must not be cached (evict-expired-then-refuse-insert; no LRU
	// eviction of a live entry).
	if _, err := cache.ValidateIDToken(context.Background(), "token-3", []string{"aud"}); err != nil {
		t.Fatalf("token-3: %v", err)
	}
	if cache.Cached("token-3", []string{"aud"}) {
		t.Error("token-3 was cached despite the cache being full of live entries (want refuse-insert, no eviction of live entries)")
	}
	// The two original entries must still be live (proves nothing was
	// evicted to make room).
	if !cache.Cached("token-1", []string{"aud"}) || !cache.Cached("token-2", []string{"aud"}) {
		t.Error("an existing live entry was evicted to make room for a new insert; want refuse-insert instead")
	}
}

// perTokenExpiryValidator is a GoogleCredentialValidator that returns a
// distinct UpstreamExpiry per token, looked up from expiry (defaulting to one
// hour out for any token not listed). Unlike countingBaseValidator, whose
// idTokenResult/accessTokenResult is one fixed value shared by every call, so
// it can prove that different tokens are actually treated as expiring at
// different times.
type perTokenExpiryValidator struct {
	expiry map[string]time.Time
}

func (v *perTokenExpiryValidator) identityFor(token string) *ValidatedGoogleIdentity {
	exp, ok := v.expiry[token]
	if !ok {
		exp = time.Now().Add(time.Hour)
	}
	return &ValidatedGoogleIdentity{Subject: token, Email: "user@gmail.com", EmailVerified: true, UpstreamExpiry: exp}
}

func (v *perTokenExpiryValidator) ValidateIDToken(_ context.Context, token string, _ []string) (*ValidatedGoogleIdentity, error) {
	return v.identityFor(token), nil
}

func (v *perTokenExpiryValidator) ValidateAccessToken(_ context.Context, token string, _ []string) (*ValidatedGoogleIdentity, error) {
	return v.identityFor(token), nil
}

func TestGoogleCredentialCache_EvictsExpiredBeforeRefusing(t *testing.T) {
	// A fake clock far from the real wall clock, so the test can't
	// accidentally pass by coincidence if a real-time call sneaks in.
	now := time.Now().AddDate(50, 0, 0)
	clock := now
	nowFunc := func() time.Time { return clock }

	// token-1 is short-lived, so it expires quickly; token-2 is long-lived,
	// so once inserted its own TTL isn't the constraint under test — only
	// whether store() evicted token-1 to make room. A shared
	// countingBaseValidator with one fixed UpstreamExpiry can't express this
	// (it doesn't look at which token was passed), so this test needs its own
	// tiny per-token validator.
	base := &perTokenExpiryValidator{expiry: map[string]time.Time{
		"token-1": now.Add(5 * time.Second),
		"token-2": now.Add(time.Hour),
	}}
	cache := NewCachingGoogleCredentialValidator(base, withCacheNowFunc(nowFunc), WithCacheMaxEntries(1)).(*cachingGoogleCredentialValidator)

	if _, err := cache.ValidateIDToken(context.Background(), "token-1", []string{"aud"}); err != nil {
		t.Fatalf("token-1: %v", err)
	}
	// Let token-1's entry expire.
	clock = now.Add(6 * time.Second)
	// token-2 must now fit, because store() evicts expired entries before
	// refusing an insert into a "full" cache.
	if _, err := cache.ValidateIDToken(context.Background(), "token-2", []string{"aud"}); err != nil {
		t.Fatalf("token-2: %v", err)
	}
	if !cache.Cached("token-2", []string{"aud"}) {
		t.Error("token-2 was not cached even though the only existing entry had already expired (want evict-expired-then-insert)")
	}
}

// ---------------------------------------------------------------------------
// singleflight must not run the shared upstream call
// under the specific caller (the "leader") that happened to trigger it: that
// caller cancelling its own request must not fail every other concurrent
// waiter's request too.
// ---------------------------------------------------------------------------

// blockingValidator is a GoogleCredentialValidator whose single call blocks
// until release is closed, or until its context is cancelled — whichever
// comes first. Used to put a singleflight leader mid-flight so a test can
// cancel the leader's own context and observe whether that cancellation
// reaches the shared upstream call.
type blockingValidator struct {
	started chan struct{}
	release chan struct{}
	result  *ValidatedGoogleIdentity

	mu        sync.Mutex
	calls     int
	startOnce sync.Once
}

func (v *blockingValidator) ValidateIDToken(ctx context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	return v.validate(ctx)
}

func (v *blockingValidator) ValidateAccessToken(ctx context.Context, _ string, _ []string) (*ValidatedGoogleIdentity, error) {
	return v.validate(ctx)
}

// validate is meant to be called exactly once (singleflight should collapse
// every concurrent caller into this one invocation). But under a mutation
// that disables the cache/singleflight sharing, a follower that misses the
// flight calls this a second time.
// startOnce guards close(v.started) against that: without it, the second
// call panics on close of an already-closed channel — a panic singleflight
// recovers and re-raises, aborting the whole test binary — instead of
// letting this test's own callCount() != 1 assertion report the regression.
func (v *blockingValidator) validate(ctx context.Context) (*ValidatedGoogleIdentity, error) {
	v.mu.Lock()
	v.calls++
	v.mu.Unlock()
	v.startOnce.Do(func() { close(v.started) })
	select {
	case <-v.release:
		return v.result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrGoogleUpstreamError, ctx.Err())
	}
}

func (v *blockingValidator) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func TestGoogleCredentialCache_LeaderCancellationDoesNotPoisonFollowers(t *testing.T) {
	base := &blockingValidator{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base).(*cachingGoogleCredentialValidator)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := cache.ValidateAccessToken(leaderCtx, "same-token", []string{"aud"})
		leaderDone <- err
	}()

	<-base.started // the leader's call has begun executing upstream
	cancelLeader() // cancel the leader's OWN request context

	// A follower using an unrelated, still-live context must still get the
	// shared result, unaffected by the leader's cancellation.
	followerDone := make(chan error, 1)
	go func() {
		_, err := cache.ValidateAccessToken(context.Background(), "same-token", []string{"aud"})
		followerDone <- err
	}()

	close(base.release) // let the shared upstream call complete

	if err := <-followerDone; err != nil {
		t.Fatalf("follower error: %v (the leader's cancellation must not fail a follower's request)", err)
	}
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader error: %v (the leader's own cancellation must not fail its own singleflight-shared call either, "+
			"since followers depend on the same result)", err)
	}
	if got := base.callCount(); got != 1 {
		t.Errorf("base validator called %d time(s), want 1 (singleflight must still collapse leader+follower into one upstream call)", got)
	}
	if !cache.Cached("same-token", []string{"aud"}) {
		t.Error("the result was not cached, or was cached negatively: the leader's cancellation must not cause a valid result to go uncached")
	}
}

// ---------------------------------------------------------------------------
// scion_hub_google_validator_cache_total{result=hit|miss|negative_hit}.
// Each result is proven through the decorator's public
// ValidateIDToken/ValidateAccessToken methods, not by calling
// RecordGoogleValidatorCache directly.
// ---------------------------------------------------------------------------

// fakeCacheMetrics records every RecordGoogleValidatorCache call. Safe for
// concurrent use (needed for the singleflight/concurrent scenarios above).
type fakeCacheMetrics struct {
	mu      sync.Mutex
	results []GoogleValidatorCacheResult
}

func (f *fakeCacheMetrics) RecordGoogleValidatorCache(result GoogleValidatorCacheResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, result)
}

func (f *fakeCacheMetrics) all() []GoogleValidatorCacheResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]GoogleValidatorCacheResult, len(f.results))
	copy(out, f.results)
	return out
}

func (f *fakeCacheMetrics) count(result GoogleValidatorCacheResult) int {
	n := 0
	for _, r := range f.all() {
		if r == result {
			n++
		}
	}
	return n
}

func TestGoogleCredentialCache_MetricsRecordsMissThenHit(t *testing.T) {
	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	fake := &fakeCacheMetrics{}
	cache := NewCachingGoogleCredentialValidator(base, WithCacheMetrics(fake))

	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if got := fake.all(); len(got) != 2 || got[0] != GoogleValidatorCacheMiss || got[1] != GoogleValidatorCacheHit {
		t.Fatalf("recorded results = %v, want [miss hit]", got)
	}
}

func TestGoogleCredentialCache_MetricsRecordsNegativeHit(t *testing.T) {
	base := &countingBaseValidator{idTokenErr: ErrGoogleUnverifiedEmail}
	fake := &fakeCacheMetrics{}
	cache := NewCachingGoogleCredentialValidator(base, WithCacheMetrics(fake))

	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err == nil {
		t.Fatal("expected an error")
	}

	if got := fake.all(); len(got) != 2 || got[0] != GoogleValidatorCacheMiss || got[1] != GoogleValidatorCacheNegativeHit {
		t.Fatalf("recorded results = %v, want [miss negative_hit]", got)
	}
}

// TestGoogleCredentialCache_MetricsUpstreamErrorNeverCountsAsHitOrNegativeHit
// proves ErrGoogleUpstreamError — never cached, positively or negatively
// — is recorded as "miss" on every call, never "negative_hit": a
// mutation that started treating it as cacheable would move this count.
func TestGoogleCredentialCache_MetricsUpstreamErrorNeverCountsAsHitOrNegativeHit(t *testing.T) {
	base := &countingBaseValidator{accessTokenErr: ErrGoogleUpstreamError}
	fake := &fakeCacheMetrics{}
	cache := NewCachingGoogleCredentialValidator(base, WithCacheMetrics(fake))

	for i := 0; i < 3; i++ {
		if _, err := cache.ValidateAccessToken(context.Background(), "token", []string{"aud"}); err == nil {
			t.Fatalf("call %d: expected an error", i)
		}
	}
	if got := fake.count(GoogleValidatorCacheMiss); got != 3 {
		t.Errorf("miss count = %d, want 3 (every call retries upstream)", got)
	}
	if got := fake.count(GoogleValidatorCacheNegativeHit); got != 0 {
		t.Errorf("negative_hit count = %d, want 0 (ErrGoogleUpstreamError must never be counted as cached)", got)
	}
}

// TestGoogleCredentialCache_MetricsSetMetricsAfterConstruction proves the
// post-construction SetMetrics path (needed for production wiring, where the
// OTel exporter is only built after New() returns — see
// Server.SetGoogleValidatorCacheMetrics) behaves the same as wiring it via
// WithCacheMetrics at construction.
func TestGoogleCredentialCache_MetricsSetMetricsAfterConstruction(t *testing.T) {
	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base).(*cachingGoogleCredentialValidator)

	fake := &fakeCacheMetrics{}
	cache.SetMetrics(fake)

	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := fake.count(GoogleValidatorCacheMiss); got != 1 {
		t.Errorf("miss count = %d, want 1", got)
	}
}

// TestGoogleCredentialCache_MetricsNilRecorder_NoPanic proves the default
// (no WithCacheMetrics / SetMetrics call) never panics — this is the
// "metrics disabled" case every other test in this file already exercises
// implicitly, made explicit here.
func TestGoogleCredentialCache_MetricsNilRecorder_NoPanic(t *testing.T) {
	base := &countingBaseValidator{
		idTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	cache := NewCachingGoogleCredentialValidator(base)
	if _, err := cache.ValidateIDToken(context.Background(), "token", []string{"aud"}); err != nil {
		t.Fatalf("call: %v", err)
	}
}

// TestGoogleCredentialCache_MetricsSingleflightFollowersRecordMiss uses the
// same delay-widened-window technique as
// TestGoogleCredentialCache_SingleflightCollapsesConcurrentMisses to
// collapse N concurrent callers into one singleflight leader, then proves
// every one of them — not just the leader — records "miss": none was served
// from cache, even though only the leader actually dials upstream.
func TestGoogleCredentialCache_MetricsSingleflightFollowersRecordMiss(t *testing.T) {
	base := &countingBaseValidator{
		delay: 20 * time.Millisecond,
		accessTokenResult: &ValidatedGoogleIdentity{
			Subject: "sub-1", Email: "user@gmail.com", EmailVerified: true,
			UpstreamExpiry: time.Now().Add(10 * time.Minute),
		},
	}
	fake := &fakeCacheMetrics{}
	cache := NewCachingGoogleCredentialValidator(base, WithCacheMetrics(fake))

	const n = 6
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := cache.ValidateAccessToken(context.Background(), "same-token", []string{"aud"}); err != nil {
				t.Errorf("call: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := base.totalAccessTokenCalls(); got != 1 {
		t.Fatalf("base validator called %d time(s), want 1 (all %d concurrent callers must collapse into one upstream call)", got, n)
	}
	if got := fake.count(GoogleValidatorCacheMiss); got != n {
		t.Errorf("miss count = %d, want %d (every collapsed caller — leader and followers alike — was not served from cache)", got, n)
	}
	if got := len(fake.all()); got != n {
		t.Errorf("total recorded results = %d, want %d (no hit/negative_hit recorded for this key)", got, n)
	}
}
