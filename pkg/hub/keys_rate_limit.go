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
	"math"
	"slices"
	"sync"
	"time"
)

// keysRateLimiterIdleTTL and keysRateLimiterMaxBuckets bound the memory a
// keysRateLimiter can hold, mirroring chatSendLimiter's eviction policy
// (chat_ratelimit.go): a bucket untouched for the TTL is evicted, and
// hitting the bucket cap forces an immediate sweep that drops the
// least-recently-used half. The agent-keys contract requires "limit-map
// entries must expire when inactive" (.design/agent-keys-contract.md
// "Concrete defaults") without specifying a policy of its own, so this task
// reuses the one already proven for the chat send path rather than
// inventing a second eviction strategy.
const (
	keysRateLimiterIdleTTL    = 10 * time.Minute
	keysRateLimiterMaxBuckets = 10000
)

// keysRateBucket is one key's token bucket.
type keysRateBucket struct {
	tokens float64
	last   time.Time
}

// keysRateLimiter is a single independent token-bucket limiter keyed by an
// arbitrary string. ExecuteAgentKeys (execute_agent_keys.go) constructs two
// instances -- one for the per-principal-per-project bucket, one for the
// per-target bucket (contract "Concrete defaults": 5 req/s burst 10, and
// 10 req/s burst 20, respectively) -- and both must allow a request for it
// to proceed; neither bucket can be topped up from the other, so a caller
// cannot spend a busy target's headroom to escape its own principal limit
// or vice versa.
//
// It is safe for concurrent use. now is injectable so tests can exhaust and
// refill a bucket deterministically instead of sleeping in wall-clock time.
type keysRateLimiter struct {
	mu            sync.Mutex
	buckets       map[string]*keysRateBucket
	ratePerSecond float64
	burst         float64
	lastSweep     time.Time
	now           func() time.Time
}

// newKeysRateLimiter creates a limiter with the production clock.
func newKeysRateLimiter(ratePerSecond, burst float64) *keysRateLimiter {
	return newKeysRateLimiterWithClock(ratePerSecond, burst, time.Now)
}

// newKeysRateLimiterWithClock is the test seam: it lets a test control time
// without sleeping.
func newKeysRateLimiterWithClock(ratePerSecond, burst float64, now func() time.Time) *keysRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &keysRateLimiter{
		buckets:       make(map[string]*keysRateBucket),
		ratePerSecond: ratePerSecond,
		burst:         burst,
		lastSweep:     now(),
		now:           now,
	}
}

// Allow consumes one token from key's bucket if available. A nil limiter
// (a hand-constructed Server that never called the production
// constructor) allows everything: rate limiting is a protection, not a
// correctness requirement.
//
// When refused, nothing is consumed, and the returned duration is the wait
// before key would next have a token available (floored at one second, the
// same floor chatSendLimiter uses, since a sub-second Retry-After is not a
// meaningful backoff signal).
//
// Allow is a single-bucket convenience built on reserve; a caller that must
// check two independent limiters together and consume from neither unless
// both allow (as ExecuteAgentKeys does for its principal+project and target
// buckets) should use the package-level allowBoth instead, not two separate
// Allow calls. Production code never calls Allow directly for that reason --
// it exists as a test helper (see execute_agent_keys_test.go's
// TestExecuteAgentKeysAllowBoth_SameLimiterInstance and
// TestExecuteAgentKeys_TargetSaturationDoesNotChargePrincipal, and
// keys_rate_limit_test.go's own unit tests), to drain or probe a single
// bucket without needing a second limiter to pair it with.
func (l *keysRateLimiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	release, retryAfter, ok := l.reserve(key)
	if !ok {
		return false, retryAfter
	}
	release(true)
	return true, 0
}

// reserve refills key's bucket and, if it currently holds at least one
// token, returns a release function that must be called exactly once to
// either consume that token (release(true)) or return it untouched
// (release(false)), along with unlocking the limiter. The limiter's lock is
// held from reserve until release runs, so a concurrent request cannot
// invalidate this decision in between — this is what lets allowBoth check
// two limiters and commit to both only if both allow, rather than consuming
// from the first before it is known whether the second will refuse.
//
// If refused, the lock is already released before reserve returns and the
// nil release function must not be called.
//
// A nil limiter always allows, with a no-op release, matching Allow's
// nil-safety.
func (l *keysRateLimiter) reserve(key string) (release func(consume bool), retryAfter time.Duration, allowed bool) {
	if l == nil {
		return func(bool) {}, 0, true
	}

	l.mu.Lock()

	now := l.now()
	l.sweepLocked(now)
	b := l.refillLocked(key, now)

	if b.tokens < 1 {
		wait := l.waitLocked(b)
		l.mu.Unlock()
		return nil, wait, false
	}

	return func(consume bool) {
		if consume {
			b.tokens--
		}
		l.mu.Unlock()
	}, 0, true
}

// refillLocked returns key's bucket, creating it at full burst if it does
// not exist yet, and crediting the tokens accrued since it was last
// touched. Shared by reserve and allowSameLimiterBoth so the refill formula
// exists in exactly one place. The caller must hold l.mu.
func (l *keysRateLimiter) refillLocked(key string, now time.Time) *keysRateBucket {
	b, ok := l.buckets[key]
	if !ok {
		b = &keysRateBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.ratePerSecond)
		b.last = now
	}
	return b
}

// waitLocked returns how long until b would next have a token available,
// floored at one second (a sub-second Retry-After is not a meaningful
// backoff signal). Shared by reserve and allowSameLimiterBoth. The caller
// must hold l.mu.
func (l *keysRateLimiter) waitLocked(b *keysRateBucket) time.Duration {
	wait := time.Duration((1 - b.tokens) / l.ratePerSecond * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// allowBoth checks two independent keysRateLimiter buckets — used for the
// per-principal+project bucket (a) and the per-target bucket (b) — and
// consumes a token from each only if both currently allow. If either
// refuses, neither is charged: a request refused by a saturated target
// bucket must not also drain the caller's own principal+project budget for
// a request that could never have proceeded anyway. This both-or-neither
// guarantee holds unconditionally, including the a == b case (see below).
//
// When both allow, the returned retryAfter is zero. When either refuses,
// retryAfter is that bucket's own wait (a's if a refused, b's if a allowed
// but b refused).
//
// a is normally reserved before b, a fixed order that avoids deadlock since
// this is the only place either limiter's lock is held while another lock
// is acquired. Production never passes the same limiter twice (the
// principal+project and target limiters are always two separate
// *keysRateLimiter values), but a == b is handled correctly rather than
// merely guarded against: reserve/reserve on the same *sync.Mutex would
// deadlock (sync.Mutex is not reentrant), so this case instead refills and
// checks both keyA's and keyB's buckets under a single lock acquisition,
// consuming from neither unless both have a token. If keyA == keyB too,
// both checks and both decrements land on the same bucket, so a single
// key's bucket now correctly needs two available tokens rather than one.
func allowBoth(a *keysRateLimiter, keyA string, b *keysRateLimiter, keyB string) (allowed bool, retryAfter time.Duration) {
	if a == b {
		return a.allowSameLimiterBoth(keyA, keyB)
	}

	releaseA, waitA, okA := a.reserve(keyA)
	if !okA {
		return false, waitA
	}
	releaseB, waitB, okB := b.reserve(keyB)
	if !okB {
		releaseA(false)
		return false, waitB
	}
	releaseA(true)
	releaseB(true)
	return true, 0
}

// allowSameLimiterBoth is allowBoth's a == b case: it reserves keyA and
// keyB against the same limiter under one lock acquisition (never two
// nested reserve calls on the same mutex), preserving allowBoth's
// both-or-neither guarantee even when keyA == keyB. A nil limiter always
// allows, matching Allow/reserve's nil-safety.
func (l *keysRateLimiter) allowSameLimiterBoth(keyA, keyB string) (allowed bool, retryAfter time.Duration) {
	if l == nil {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	// If keyB == keyA, refillLocked's second call returns the same
	// *keysRateBucket refilled a second time with an identical now, which is
	// a harmless no-op (zero elapsed time to refill) -- not a second,
	// independent bucket. Checking both buckets' tokens up front (before
	// consuming either) would be wrong in that case: both checks would read
	// the same not-yet-decremented value, so a bucket holding only one token
	// would incorrectly pass both checks. Consuming A immediately and
	// checking B against the now-possibly-already-decremented bucket --
	// refunding A if B refuses -- is what makes a single-token bucket
	// correctly require two tokens for two reservations against it, while
	// still behaving like ordinary independent reserve/reserve when
	// keyA != keyB.
	bucketA := l.refillLocked(keyA, now)
	bucketB := l.refillLocked(keyB, now)

	if bucketA.tokens < 1 {
		return false, l.waitLocked(bucketA)
	}
	bucketA.tokens--
	if bucketB.tokens < 1 {
		bucketA.tokens++ // refund: both-or-neither, not "A only".
		return false, l.waitLocked(bucketB)
	}
	bucketB.tokens--
	return true, 0
}

// sweepLocked evicts idle buckets. It runs at most once per idle TTL, or
// immediately when the map has hit its cap. The caller must hold l.mu.
func (l *keysRateLimiter) sweepLocked(now time.Time) {
	atCap := len(l.buckets) >= keysRateLimiterMaxBuckets
	if !atCap && now.Sub(l.lastSweep) < keysRateLimiterIdleTTL {
		return
	}
	l.lastSweep = now

	for k, b := range l.buckets {
		if now.Sub(b.last) >= keysRateLimiterIdleTTL {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < keysRateLimiterMaxBuckets {
		return
	}

	keys := make([]string, 0, len(l.buckets))
	for k := range l.buckets {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return l.buckets[a].last.Compare(l.buckets[b].last)
	})
	for _, k := range keys[:len(keys)/2] {
		delete(l.buckets, k)
	}
}
