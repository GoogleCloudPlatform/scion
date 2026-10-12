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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMentionPairLimiter_WindowCap(t *testing.T) {
	now := time.Now()
	l := newMentionPairLimiterWithParams(3, time.Minute, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		require.True(t, l.Reserve("a", "b"), "delivery %d should be within the cap", i)
	}
	require.False(t, l.Reserve("a", "b"), "the 4th delivery within the window must be refused")

	// Advancing past the window resets the cap.
	now = now.Add(2 * time.Minute)
	require.True(t, l.Reserve("a", "b"), "a fresh window must allow again")
}

// The shipped production cap and window — not a test-shrunk stand-in — gate
// admission: exactly mentionPairLimiterCap reserves succeed inside
// mentionPairLimiterWindow, the next one is refused, and a fresh window
// (advanced past mentionPairLimiterWindow on an injected clock) allows again.
// This pins the actual constants; changing either one would fail this test
// even though every other limiter test uses its own small cap and window.
func TestMentionPairLimiter_ProductionCapAndWindow(t *testing.T) {
	// Literal expectations, not derived from the constants under test: if
	// mentionPairLimiterCap or mentionPairLimiterWindow ever changes, this
	// test must fail rather than silently re-deriving a new "production"
	// value from the very thing it exists to pin.
	require.Equal(t, 10, mentionPairLimiterCap, "this test's literals assume the shipped cap is 10; update both together")
	require.Equal(t, 10*time.Minute, mentionPairLimiterWindow, "this test's literals assume the shipped window is 10m; update both together")

	now := time.Now()
	l := newMentionPairLimiterWithParams(mentionPairLimiterCap, mentionPairLimiterWindow, func() time.Time { return now })

	for i := 0; i < 10; i++ {
		require.True(t, l.Reserve("a", "b"), "delivery %d must be within the production cap", i)
	}
	require.False(t, l.Reserve("a", "b"), "the 11th delivery must be refused")

	now = now.Add(9*time.Minute + 59*time.Second)
	require.False(t, l.Reserve("a", "b"), "1 second short of the production window must still refuse")

	now = now.Add(2 * time.Second)
	require.True(t, l.Reserve("a", "b"), "past the production window, a reserve must succeed again")
}

func TestMentionPairLimiter_RefusalDoesNotConsume(t *testing.T) {
	now := time.Now()
	l := newMentionPairLimiterWithParams(1, time.Minute, func() time.Time { return now })

	require.True(t, l.Reserve("a", "b"))
	require.False(t, l.Reserve("a", "b"), "second call within the window and over cap must be refused")
	// Refusal must not have consumed anything further: a second refusal
	// should behave identically to the first, not compound.
	require.False(t, l.Reserve("a", "b"))

	now = now.Add(2 * time.Minute)
	require.True(t, l.Reserve("a", "b"), "a fresh window must allow exactly one more")
}

func TestMentionPairLimiter_UnorderedKey(t *testing.T) {
	now := time.Now()
	l := newMentionPairLimiterWithParams(1, time.Minute, func() time.Time { return now })

	require.True(t, l.Reserve("agent-a", "agent-b"))
	// The reverse direction shares the same budget (A-mentions-B and
	// B-mentions-A are one pair, not two).
	require.False(t, l.Reserve("agent-b", "agent-a"))

	assert.Equal(t, mentionPairKey("agent-a", "agent-b"), mentionPairKey("agent-b", "agent-a"))
}

func TestMentionPairLimiter_IndependentPairs(t *testing.T) {
	now := time.Now()
	l := newMentionPairLimiterWithParams(1, time.Minute, func() time.Time { return now })

	require.True(t, l.Reserve("a", "b"))
	require.False(t, l.Reserve("a", "b"), "pair a|b is now over cap")
	require.True(t, l.Reserve("a", "c"), "an unrelated pair must have its own budget")
	require.True(t, l.Reserve("b", "c"), "another unrelated pair must have its own budget")
}

func TestMentionPairLimiter_NilLimiterAllowsEverything(t *testing.T) {
	var l *mentionPairLimiter
	for i := 0; i < 100; i++ {
		require.True(t, l.Reserve("a", "b"), "a nil limiter must fail open, matching chatSendLimiter's convention")
		l.Release("a", "b") // must not panic
	}
}

// TestMentionPairLimiter_ReleaseGivesBackASlot proves the reserve-then-settle
// contract a non-delivered mention relies on: reserving and then releasing
// leaves the pair exactly as if the reservation never happened.
func TestMentionPairLimiter_ReleaseGivesBackASlot(t *testing.T) {
	now := time.Now()
	l := newMentionPairLimiterWithParams(1, time.Minute, func() time.Time { return now })

	require.True(t, l.Reserve("a", "b"))
	l.Release("a", "b")
	require.True(t, l.Reserve("a", "b"), "releasing a reservation must give the slot back")
	require.False(t, l.Reserve("a", "b"), "the cap still applies to the slot that was actually kept")
}

func TestMentionPairLimiter_ReleaseOnEmptyPairIsNoOp(t *testing.T) {
	l := newMentionPairLimiterWithParams(1, time.Minute, time.Now)
	l.Release("a", "b") // must not panic
	require.True(t, l.Reserve("a", "b"), "a pair with nothing reserved is unaffected by Release")
}

// TestMentionPairLimiter_ConcurrentReserveAdmitsExactlyCap drives many
// goroutines at a cap-1 limiter for the same pair and asserts that exactly
// one of them is admitted, proving Reserve's check-and-record is one atomic
// operation rather than two separate critical sections a race could slip
// between. Run with -race.
func TestMentionPairLimiter_ConcurrentReserveAdmitsExactlyCap(t *testing.T) {
	const goroutines = 50
	l := newMentionPairLimiterWithParams(1, time.Minute, time.Now)

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if l.Reserve("a", "b") {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, admitted, "exactly one concurrent Reserve call for a cap-1 pair must be admitted")
}

// TestMentionPairLimiter_PruningReclaimsExpiredEntries exercises the
// defensive sweep path directly by driving the tracked-pair count past
// mentionPairLimiterMaxPairs and confirming fully-idle pairs are dropped
// rather than accumulating forever.
func TestMentionPairLimiter_PruningReclaimsExpiredEntries(t *testing.T) {
	now := time.Now()
	l := newMentionPairLimiterWithParams(1, time.Minute, func() time.Time { return now })

	// Seed pairs well past the sweep threshold, all already expired.
	for i := 0; i < mentionPairLimiterMaxPairs+10; i++ {
		key := mentionPairKey("sender", string(rune('a'))+string(rune(i)))
		l.windows[key] = []time.Time{now.Add(-2 * time.Minute)}
	}
	require.GreaterOrEqual(t, len(l.windows), mentionPairLimiterMaxPairs)

	// A fresh Reserve call triggers sweepIfLargeLocked, which must reclaim
	// every expired entry (none is within the 1-minute window anymore).
	require.True(t, l.Reserve("fresh-a", "fresh-b"))
	require.Less(t, len(l.windows), mentionPairLimiterMaxPairs+10,
		"expired pairs must be swept once the tracked-pair count is large")
}

// The at-capacity sweep must not repeat on every Reserve call once the map
// is at capacity and nothing new has expired: that would turn every send
// into a full O(pairs) scan under the lock. It must still run again once
// mentionPairLimiterSweepInterval has elapsed, and reclaim whatever has
// expired by then. Correctness of the cap itself (via pruneBefore, scoped to
// the one pair being checked) never depends on whether a sweep ran at all —
// this test is only about how often the sweep itself executes.
func TestMentionPairLimiter_SweepIsRateLimited(t *testing.T) {
	fakeNow := time.Now()
	l := newMentionPairLimiterWithParams(1000, time.Minute, func() time.Time { return fakeNow })

	// Fill the map to the defensive cap with entries already expired
	// relative to the 1-minute window, so a real sweep has something to
	// reclaim.
	l.mu.Lock()
	for i := 0; i < mentionPairLimiterMaxPairs; i++ {
		key := fmt.Sprintf("stale-%d", i)
		l.windows[key] = []time.Time{fakeNow.Add(-2 * time.Minute)}
	}
	l.mu.Unlock()

	require.True(t, l.Reserve("a", "b"), "a fresh pair must still be admitted while the map is at the defensive cap")
	require.Equal(t, 1, l.sweepCount, "the first Reserve at capacity must trigger exactly one sweep")
	require.Less(t, len(l.windows), mentionPairLimiterMaxPairs,
		"the sweep must have reclaimed the fully-expired stale entries")

	// Re-fill back up to the cap with entries that are NOT expired, so the
	// map is at capacity again but there is nothing stale for a sweep to
	// find — and the sweep interval has not elapsed since the first sweep.
	l.mu.Lock()
	for i := 0; len(l.windows) < mentionPairLimiterMaxPairs; i++ {
		key := fmt.Sprintf("fresh-%d", i)
		l.windows[key] = []time.Time{fakeNow}
	}
	l.mu.Unlock()

	for i := 0; i < 5; i++ {
		l.Reserve("c", "d")
	}
	require.Equal(t, 1, l.sweepCount,
		"repeated Reserve calls at capacity within the sweep interval must not re-sweep")

	// Advance past the sweep interval and make the map stale again: the
	// next Reserve at capacity must sweep once more and reclaim it.
	fakeNow = fakeNow.Add(mentionPairLimiterSweepInterval + time.Second)
	l.mu.Lock()
	for k := range l.windows {
		l.windows[k] = []time.Time{fakeNow.Add(-2 * time.Minute)}
	}
	l.mu.Unlock()

	require.True(t, l.Reserve("e", "f"))
	require.Equal(t, 2, l.sweepCount,
		"a Reserve at capacity after the sweep interval has elapsed must sweep again")
	require.Less(t, len(l.windows), mentionPairLimiterMaxPairs,
		"the second sweep must reclaim entries that expired after the first one")
}
