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
	"sync/atomic"
	"testing"
	"time"
)

// testClock is a manually advanced clock so limiter tests can exhaust and
// refill a bucket without sleeping a real minute.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A sender gets a full minute's allowance as burst, then is refused until
// tokens refill — the refusal carries a usable retry delay.
func TestChatSendLimiter_BurstThenRefusesUntilRefill(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithRates(30, 60, clock.Now)

	for i := range chatSendHumanRatePerMinute {
		if allowed, _ := lim.Allow("u1", chatSenderHuman); !allowed {
			t.Fatalf("send %d of the first 30 was refused; the whole minute's allowance should burst", i+1)
		}
	}

	allowed, retryAfter := lim.Allow("u1", chatSenderHuman)
	if allowed {
		t.Fatal("the 31st send in the same instant should be refused")
	}
	if retryAfter < time.Second {
		t.Errorf("retryAfter = %v, want at least 1s so Retry-After is a usable number", retryAfter)
	}

	// At 30/min a token accrues every 2 seconds.
	clock.Advance(time.Second)
	if allowed, _ := lim.Allow("u1", chatSenderHuman); allowed {
		t.Error("half a token later the sender should still be refused")
	}
	clock.Advance(time.Second)
	if allowed, _ := lim.Allow("u1", chatSenderHuman); !allowed {
		t.Error("a full token later the sender should be allowed again")
	}

	// Refill is capped at the burst size: idling for an hour does not bank
	// an hour's worth of sends.
	clock.Advance(time.Hour)
	for i := range chatSendHumanRatePerMinute {
		if allowed, _ := lim.Allow("u1", chatSenderHuman); !allowed {
			t.Fatalf("send %d after a long idle was refused", i+1)
		}
	}
	if allowed, _ := lim.Allow("u1", chatSenderHuman); allowed {
		t.Error("a long idle should refill at most one burst, not more")
	}
}

// One flooding sender must not spend anybody else's allowance, and the two
// rate classes are tracked separately even for the same ID.
func TestChatSendLimiter_BucketsAreScopedToSenderAndClass(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithRates(2, 3, clock.Now)

	for range 2 {
		if allowed, _ := lim.Allow("shared-id", chatSenderHuman); !allowed {
			t.Fatal("the human's own allowance should not be exhausted yet")
		}
	}
	if allowed, _ := lim.Allow("shared-id", chatSenderHuman); allowed {
		t.Fatal("the human is over its limit and should be refused")
	}

	if allowed, _ := lim.Allow("someone-else", chatSenderHuman); !allowed {
		t.Error("a different sender must not be throttled by the flooder")
	}
	for i := range 3 {
		if allowed, _ := lim.Allow("shared-id", chatSenderAgent); !allowed {
			t.Errorf("agent send %d refused: the agent class has its own bucket and a higher limit", i+1)
		}
	}
}

// The limits shipped to production are the ones issue #1054 asks for.
func TestChatSendLimiter_ProductionLimits(t *testing.T) {
	lim := newChatSendLimiter()
	if got := lim.limitFor(chatSenderHuman); got != 30 {
		t.Errorf("human limit = %v, want 30/min", got)
	}
	if got := lim.limitFor(chatSenderAgent); got != 60 {
		t.Errorf("agent limit = %v, want 60/min", got)
	}
}

// Bucket state is bounded: senders that stop sending are forgotten.
func TestChatSendLimiter_EvictsIdleBuckets(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithRates(30, 60, clock.Now)

	for _, id := range []string{"a", "b", "c"} {
		lim.Allow(id, chatSenderHuman)
	}
	if got := len(lim.buckets); got != 3 {
		t.Fatalf("tracked buckets = %d, want 3", got)
	}

	clock.Advance(chatSendLimiterIdleTTL + time.Minute)
	lim.Allow("d", chatSenderHuman)

	if got := len(lim.buckets); got != 1 {
		t.Errorf("tracked buckets = %d, want 1 (only the active sender survives the sweep)", got)
	}
}

// A nil limiter allows everything rather than panicking: limiting is a
// protection, not a correctness requirement.
func TestChatSendLimiter_NilAllows(t *testing.T) {
	var lim *chatSendLimiter
	if allowed, retryAfter := lim.Allow("u1", chatSenderHuman); !allowed || retryAfter != 0 {
		t.Errorf("nil limiter returned (%v, %v), want (true, 0)", allowed, retryAfter)
	}
}

// Concurrent senders hitting one bucket hand out exactly the allowance — no
// more (the point of the limit) and no fewer (run with -race).
func TestChatSendLimiter_ConcurrentSendersGetExactlyTheAllowance(t *testing.T) {
	clock := newTestClock()
	const limit = 20
	lim := newChatSendLimiterWithRates(limit, limit, clock.Now)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := lim.Allow("flooder", chatSenderAgent); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != limit {
		t.Errorf("allowed %d of 100 concurrent sends, want exactly %d", got, limit)
	}
}
