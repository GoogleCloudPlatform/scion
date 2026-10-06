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

// Unit-level tests for keysRateLimiter (keys_rate_limit.go) itself, using
// newKeysRateLimiterWithClock's injectable clock rather than sleeps. These
// need no handler or store fixture: every handler-level limiter test in
// execute_agent_keys_test.go either freezes the clock entirely
// (freezeKeysRateLimiters) or runs on the wall clock without ever advancing
// it far enough to observe a refill, so none of them exercises
// refillLocked's "credit tokens accrued since last touched" formula,
// waitLocked's one-second Retry-After floor, or sweepLocked's eviction
// policy. A limiter whose refill never credits any tokens -- locking every
// caller out of keys permanently once it exhausts its burst -- passed the
// whole keys test suite before these tests existed.

import (
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// TestKeysRateLimiter_RefillAndRetryAfter pins the refill formula and the
// Retry-After floor/cap at the unit level.
func TestKeysRateLimiter_RefillAndRetryAfter(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	// Production principal+project rate: 5 req/s, burst 10 (one token
	// every 200ms).
	l := newKeysRateLimiterWithClock(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, clock)

	for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
		if allowed, _ := l.Allow("k"); !allowed {
			t.Fatalf("call %d: expected allowed while the burst is not yet exhausted", i)
		}
	}

	// Burst exhausted: the next call is refused. The raw wait is 200ms
	// (1/5 req/s), but waitLocked floors it to one second.
	allowed, retryAfter := l.Allow("k")
	if allowed {
		t.Fatalf("expected refusal once the burst is exhausted")
	}
	if retryAfter != time.Second {
		t.Fatalf("retryAfter = %v, want 1s (the floor)", retryAfter)
	}

	// Advance by just under one token interval (199ms): still refused.
	now = now.Add(199 * time.Millisecond)
	if allowed, _ := l.Allow("k"); allowed {
		t.Fatalf("expected refusal at 199ms (just under one token interval)")
	}

	// Advance to exactly one full interval (200ms total): exactly one call
	// is allowed, then the next is refused again.
	now = now.Add(1 * time.Millisecond)
	if allowed, _ := l.Allow("k"); !allowed {
		t.Fatalf("expected exactly one call allowed at the 200ms mark")
	}
	if allowed, _ := l.Allow("k"); allowed {
		t.Fatalf("expected refusal immediately after consuming the single refilled token")
	}

	// Advance by far more than burst/rate (1 minute): refill caps at burst
	// rather than accumulating without bound (the math.Min in
	// refillLocked).
	now = now.Add(1 * time.Minute)
	allowedCount := 0
	for i := 0; i < agentkeys.PrincipalProjectBurst+5; i++ {
		if allowed, _ := l.Allow("k"); allowed {
			allowedCount++
		}
	}
	if allowedCount != agentkeys.PrincipalProjectBurst {
		t.Fatalf("allowed %d calls after a long idle period, want exactly burst (%d)", allowedCount, agentkeys.PrincipalProjectBurst)
	}

	// A case where the raw wait exceeds the 1s floor, so waitLocked returns
	// the computed value rather than the floor: rate 0.25/s (one token
	// every 4s), burst 1, already exhausted.
	slow := newKeysRateLimiterWithClock(0.25, 1, clock)
	if allowed, _ := slow.Allow("k"); !allowed {
		t.Fatalf("expected the first call on a fresh bucket to be allowed")
	}
	allowed, retryAfter = slow.Allow("k")
	if allowed {
		t.Fatalf("expected refusal immediately after exhausting a burst-1 bucket")
	}
	if retryAfter != 4*time.Second {
		t.Fatalf("retryAfter = %v, want 4s (the computed wait, not the 1s floor)", retryAfter)
	}
}

// TestKeysAllowBoth_ReturnsBsWaitWhenOnlyBRefuses pins allowBoth's retryAfter
// value in the (distinct-limiters) case where a allows and b alone refuses:
// the returned wait must be b's own wait, not a's (a never refused, so it
// has none worth reporting) and not zero (which would wrongly suggest the
// call could be retried immediately). a must also be left completely
// unconsumed, refunded after being found available but never committed.
func TestKeysAllowBoth_ReturnsBsWaitWhenOnlyBRefuses(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	a := newKeysRateLimiterWithClock(5, 10, clock)   // plenty of burst; never the one refusing.
	b := newKeysRateLimiterWithClock(0.25, 1, clock) // burst 1, one token every 4s.

	if allowed, _ := b.Allow("bkey"); !allowed {
		t.Fatalf("expected the first call on a fresh bucket to be allowed")
	}

	allowed, retryAfter := allowBoth(a, "akey", b, "bkey")
	if allowed {
		t.Fatalf("expected refusal: b's bucket is already exhausted")
	}
	if retryAfter != 4*time.Second {
		t.Errorf("retryAfter = %v, want 4s (b's own computed wait, not a's zero wait and not a's 1s floor)", retryAfter)
	}

	// a must not have been charged: allowBoth reserves a, finds it
	// available, then releases it untouched once b refuses.
	for i := 0; i < 10; i++ {
		if ok, _ := a.Allow("akey"); !ok {
			t.Fatalf("expected a's bucket to still hold its full burst after being refunded; failed at probe %d", i)
		}
	}
}

// TestKeysRateLimiter_IdleBucketExpiry pins sweepLocked's TTL-based
// eviction: a bucket untouched for keysRateLimiterIdleTTL is evicted on the
// next call that triggers a sweep, while a more recently touched one
// survives.
func TestKeysRateLimiter_IdleBucketExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	l := newKeysRateLimiterWithClock(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, func() time.Time { return now })

	l.Allow("stale") // touched at t0

	now = now.Add(keysRateLimiterIdleTTL - time.Second)
	l.Allow("recent") // touched at t0+TTL-1s; no sweep yet (elapsed since
	// the limiter's own construction-time lastSweep is still under the TTL)

	l.mu.Lock()
	_, staleStillPresent := l.buckets["stale"]
	l.mu.Unlock()
	if !staleStillPresent {
		t.Fatalf("expected 'stale' to still be present before its idle TTL has fully elapsed")
	}

	now = now.Add(time.Second) // t0+TTL: triggers the next sweep
	l.Allow("trigger")

	l.mu.Lock()
	_, staleSurvived := l.buckets["stale"]
	_, recentSurvived := l.buckets["recent"]
	l.mu.Unlock()
	if staleSurvived {
		t.Errorf("expected 'stale' (idle for the full TTL) to be evicted")
	}
	if !recentSurvived {
		t.Errorf("expected 'recent' (touched just under the TTL ago) to survive")
	}
}

// TestKeysRateLimiter_MaxBucketsTriggersHalfEviction pins sweepLocked's
// cap-triggered path: filling the map to keysRateLimiterMaxBuckets forces an
// immediate sweep (regardless of the idle-TTL schedule) that keeps the most
// recently used half.
func TestKeysRateLimiter_MaxBucketsTriggersHalfEviction(t *testing.T) {
	now := time.Unix(0, 0)
	l := newKeysRateLimiterWithClock(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, func() time.Time { return now })

	l.mu.Lock()
	for i := 0; i < keysRateLimiterMaxBuckets; i++ {
		// Staggered last-touch times, strictly increasing with i, so "the
		// most recently used half" has an unambiguous meaning: the lowest
		// indices are the oldest.
		l.buckets[fmt.Sprintf("k%d", i)] = &keysRateBucket{tokens: l.burst, last: now.Add(time.Duration(i) * time.Millisecond)}
	}
	l.lastSweep = now // a sweep "just happened" on its own TTL schedule, so
	// only the cap (not the TTL) triggers the next one.
	l.mu.Unlock()

	if allowed, _ := l.Allow(fmt.Sprintf("k%d", keysRateLimiterMaxBuckets)); !allowed {
		t.Fatalf("expected the triggering call's own fresh bucket to be allowed")
	}

	l.mu.Lock()
	n := len(l.buckets)
	_, oldestSurvived := l.buckets["k0"]
	_, newestSurvived := l.buckets[fmt.Sprintf("k%d", keysRateLimiterMaxBuckets-1)]
	_, lowerBoundarySurvived := l.buckets[fmt.Sprintf("k%d", keysRateLimiterMaxBuckets/2-1)] // oldest of the evicted half
	_, upperBoundarySurvived := l.buckets[fmt.Sprintf("k%d", keysRateLimiterMaxBuckets/2)]   // oldest of the surviving half
	l.mu.Unlock()

	// Exactly the surviving half, plus the one new bucket the triggering
	// call itself created -- not "roughly half": a sweep that evicts too
	// much (e.g. keeping only the single newest bucket) would still satisfy
	// a loose upper-bound check, but resets every other active caller's
	// budget to a fresh burst, which defeats the limiter under exactly the
	// load the cap exists to protect against.
	wantSurvivors := keysRateLimiterMaxBuckets/2 + 1
	if n != wantSurvivors {
		t.Errorf("expected exactly %d buckets to survive a cap-triggered sweep (the most-recently-used half, plus the triggering call's new bucket), got %d (cap %d)", wantSurvivors, n, keysRateLimiterMaxBuckets)
	}
	if oldestSurvived {
		t.Errorf("expected the oldest bucket (k0) to be evicted by a cap-triggered sweep")
	}
	if !newestSurvived {
		t.Errorf("expected the newest bucket to survive a cap-triggered sweep")
	}
	if lowerBoundarySurvived {
		t.Errorf("expected k%d (the oldest bucket of the evicted half) to be evicted", keysRateLimiterMaxBuckets/2-1)
	}
	if !upperBoundarySurvived {
		t.Errorf("expected k%d (the oldest bucket of the surviving half) to survive", keysRateLimiterMaxBuckets/2)
	}
}
