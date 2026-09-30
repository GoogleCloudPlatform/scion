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
	"sync"
	"time"
)

// Agent mention loop/storm protection: a per-pair cap bounds how many
// mention deliveries may pass between the same two agents within a sliding
// window. This is a second, tighter brake than the aggregate per-sender send
// budget (chatSendLimiter) — the aggregate budget is high enough that a
// two-agent ping-pong could run for a while before it bites.
//
// The cap is deliberately small and the window short, and both are easy to
// retune independently of the rest of the fan-out logic.
const (
	// mentionPairLimiterCap is the maximum number of mention deliveries
	// allowed between one unordered pair of agents inside the window.
	mentionPairLimiterCap = 10

	// mentionPairLimiterWindow is the sliding window over which the cap is
	// enforced.
	mentionPairLimiterWindow = 10 * time.Minute

	// mentionPairLimiterMaxPairs bounds the tracked pairs so the map cannot
	// grow without bound from one-off mention pairs that never repeat.
	// Reaching it triggers a sweep of fully-idle pairs, rate-limited by
	// mentionPairLimiterSweepInterval below.
	mentionPairLimiterMaxPairs = 10000

	// mentionPairLimiterSweepInterval bounds how often the at-capacity sweep
	// may actually run. Without this, once the map is at capacity and
	// nothing has expired yet, every single Reserve call would repeat a full
	// O(pairs) scan under the lock — a sweep that finds nothing to reclaim
	// is itself an expensive no-op under sustained load. Reserve's own cap
	// enforcement (pruneBefore, scoped to the one pair being checked) does
	// not depend on this sweep ever running at all; the sweep only bounds
	// the map's total size.
	mentionPairLimiterSweepInterval = 1 * time.Minute
)

// mentionPairLimiter is an in-memory sliding-window limiter keyed by the
// unordered pair of agent IDs, so A-mentions-B and B-mentions-A share one
// budget. It is safe for concurrent use.
//
// Like chatSendLimiter, this is in-memory and scoped to a single process —
// it does not synchronize across hub replicas.
type mentionPairLimiter struct {
	mu       sync.Mutex
	windows  map[string][]time.Time
	capacity int
	window   time.Duration

	// lastSweep is the clock time the at-capacity sweep last actually ran,
	// used to rate-limit it to at most once per mentionPairLimiterSweepInterval.
	lastSweep time.Time
	// sweepCount counts how many times the sweep has actually executed (not
	// merely been considered). Tests use it to observe sweep frequency
	// directly instead of inferring it from map-size side effects.
	sweepCount int

	// now is the clock, injectable so tests can exercise the window without
	// sleeping ten real minutes.
	now func() time.Time
}

// newMentionPairLimiter creates a limiter with the production cap and window.
func newMentionPairLimiter() *mentionPairLimiter {
	return newMentionPairLimiterWithParams(mentionPairLimiterCap, mentionPairLimiterWindow, time.Now)
}

// newMentionPairLimiterWithParams is the test seam: a small cap and window
// let tests exercise the limit without waiting on real time.
func newMentionPairLimiterWithParams(capacity int, window time.Duration, now func() time.Time) *mentionPairLimiter {
	if now == nil {
		now = time.Now
	}
	return &mentionPairLimiter{
		windows:  make(map[string][]time.Time),
		capacity: capacity,
		window:   window,
		now:      now,
	}
}

// mentionPairKey returns the unordered pair key for two agent IDs.
func mentionPairKey(idA, idB string) string {
	if idA > idB {
		idA, idB = idB, idA
	}
	return idA + "|" + idB
}

// Reserve attempts to admit one mention delivery for the pair and, if
// admitted, immediately records a provisional timestamp in the same
// critical section as the check. That atomicity matters: two concurrent
// callers for the same pair must not both observe room under the cap and
// both proceed, which a separate check-then-record pair of calls would
// allow. A caller whose reservation does not turn into an actual delivery
// (denied, failed to resolve a conversation, rejected by the sender's own
// send budget, ...) must call Release to give the slot back; a caller whose
// reservation succeeds keeps it — no further call is needed. Stale entries
// for the pair are pruned on every access.
//
// A nil limiter allows everything, matching chatSendLimiter's fail-open
// convention for a hand-constructed Server (e.g. in unit tests that never
// wire one up).
func (l *mentionPairLimiter) Reserve(idA, idB string) bool {
	if l == nil {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepIfLargeLocked(now)

	key := mentionPairKey(idA, idB)
	cutoff := now.Add(-l.window)
	kept := pruneBefore(l.windows[key], cutoff)

	if len(kept) >= l.capacity {
		l.setOrDeleteLocked(key, kept)
		return false
	}

	kept = append(kept, now)
	l.windows[key] = kept
	return true
}

// Release gives back one slot reserved by a prior successful Reserve call
// for the same pair, for use when that reservation did not end up producing
// an actual delivery. It removes the most recently added timestamp for the
// pair rather than tracking which exact entry each caller's own Reserve
// added — every entry counts identically toward the cap, so the count this
// leaves behind is always correct. The one thing this approximation can
// shift is which timestamp remains when a concurrent Reserve for the same
// pair lands between this call's Reserve and its Release: the window then
// ages out based on a slightly different timestamp than the one actually
// released, by at most the gap between the two calls. This is a no-op if
// the pair has no recorded entries.
//
// A nil limiter is a no-op, matching Reserve's fail-open convention.
func (l *mentionPairLimiter) Release(idA, idB string) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	key := mentionPairKey(idA, idB)
	times := l.windows[key]
	if len(times) == 0 {
		return
	}
	if len(times) == 1 {
		delete(l.windows, key)
		return
	}
	l.windows[key] = times[:len(times)-1]
}

// pruneBefore returns a new slice containing only the timestamps strictly
// after cutoff, preserving order. It always allocates rather than filtering
// in place, so the caller's existing slice (e.g. the one stored in
// l.windows) is left untouched until the caller explicitly reassigns the
// result.
func pruneBefore(times []time.Time, cutoff time.Time) []time.Time {
	if len(times) == 0 {
		return nil
	}
	kept := make([]time.Time, 0, len(times))
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

func (l *mentionPairLimiter) setOrDeleteLocked(key string, kept []time.Time) {
	if len(kept) == 0 {
		delete(l.windows, key)
		return
	}
	l.windows[key] = kept
}

// sweepIfLargeLocked drops fully-idle pairs when the tracked-pair count has
// hit the defensive cap, but at most once per mentionPairLimiterSweepInterval:
// once the map is at capacity, if nothing (or little) has expired yet, every
// Reserve call would otherwise repeat this full O(pairs) scan under the
// lock. The caller must hold l.mu.
func (l *mentionPairLimiter) sweepIfLargeLocked(now time.Time) {
	if len(l.windows) < mentionPairLimiterMaxPairs {
		return
	}
	if now.Sub(l.lastSweep) < mentionPairLimiterSweepInterval {
		return
	}
	l.lastSweep = now
	l.sweepCount++
	cutoff := now.Add(-l.window)
	for k, times := range l.windows {
		kept := pruneBefore(times, cutoff)
		if len(kept) == 0 {
			delete(l.windows, k)
		} else {
			l.windows[k] = kept
		}
	}
}
