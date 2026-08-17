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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
	lim := newChatSendLimiterWithClock(clock.Now)

	for i := range chatSendHumanRatePerMinute {
		if !lim.Allow("u1", chatSenderHuman).Allowed {
			t.Fatalf("send %d of the first 30 was refused; the whole minute's allowance should burst", i+1)
		}
	}

	decision := lim.Allow("u1", chatSenderHuman)
	if decision.Allowed {
		t.Fatal("the 31st send in the same instant should be refused")
	}
	if decision.RetryAfter < time.Second {
		t.Errorf("RetryAfter = %v, want at least 1s so Retry-After is a usable number", decision.RetryAfter)
	}
	if decision.Limit != chatSendHumanRatePerMinute {
		t.Errorf("Limit = %v, want the human limit %d so the error names the limit that was hit", decision.Limit, chatSendHumanRatePerMinute)
	}

	// At 30/min a token accrues every 2 seconds.
	clock.Advance(time.Second)
	if lim.Allow("u1", chatSenderHuman).Allowed {
		t.Error("half a token later the sender should still be refused")
	}
	clock.Advance(time.Second)
	if !lim.Allow("u1", chatSenderHuman).Allowed {
		t.Error("a full token later the sender should be allowed again")
	}

	// Refill is capped at the burst size: idling for an hour does not bank
	// an hour's worth of sends.
	clock.Advance(time.Hour)
	for i := range chatSendHumanRatePerMinute {
		if !lim.Allow("u1", chatSenderHuman).Allowed {
			t.Fatalf("send %d after a long idle was refused", i+1)
		}
	}
	if lim.Allow("u1", chatSenderHuman).Allowed {
		t.Error("a long idle should refill at most one burst, not more")
	}
}

// One flooding sender must not spend anybody else's allowance, and the two
// rate classes are tracked separately even for the same ID.
func TestChatSendLimiter_BucketsAreScopedToSenderAndClass(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman: 2,
		chatSenderAgent: 3,
	}, clock.Now)

	for range 2 {
		if !lim.Allow("shared-id", chatSenderHuman).Allowed {
			t.Fatal("the human's own allowance should not be exhausted yet")
		}
	}
	if lim.Allow("shared-id", chatSenderHuman).Allowed {
		t.Fatal("the human is over its limit and should be refused")
	}

	if !lim.Allow("someone-else", chatSenderHuman).Allowed {
		t.Error("a different sender must not be throttled by the flooder")
	}
	for i := range 3 {
		if !lim.Allow("shared-id", chatSenderAgent).Allowed {
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
	if got := lim.limitFor(chatSenderAgentMirror); got != 30 {
		t.Errorf("assistant-reply mirror limit = %v, want 30/min (a reservation inside the agent's 60)", got)
	}
}

// The mirror's sub-cap is a reservation inside the agent's ceiling: it bounds
// the mirror without shrinking what the agent itself may send.
func TestChatSendLimiter_MirrorReservationDoesNotStarveAgentMessages(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderAgent:       4,
		chatSenderAgentMirror: 2,
	}, clock.Now)

	for range 2 {
		if !lim.Allow("a1", chatSenderAgentMirror).Allowed {
			t.Fatal("the mirror's own reservation should not be exhausted yet")
		}
	}
	decision := lim.Allow("a1", chatSenderAgentMirror)
	if decision.Allowed {
		t.Fatal("the mirror is over its reservation and should be refused")
	}
	if decision.LimitClass != chatSenderAgentMirror {
		t.Errorf("refused by class %v, want the mirror reservation: the aggregate still has room", decision.LimitClass)
	}

	// The mirror spent 2 of the aggregate 4, so exactly 2 remain for the
	// agent's own messages: the reservation caps the mirror, and what the
	// mirror spent still came out of the one shared ceiling.
	for i := range 2 {
		if !lim.Allow("a1", chatSenderAgent).Allowed {
			t.Errorf("agent-authored send %d refused: a flooding mirror must leave headroom", i+1)
		}
	}
	decision = lim.Allow("a1", chatSenderAgent)
	if decision.Allowed {
		t.Fatal("the third agent-authored send should be refused: the mirror's 2 sends came out of the same aggregate 4")
	}
	if decision.LimitClass != chatSenderAgent {
		t.Errorf("refused by class %v, want the agent aggregate", decision.LimitClass)
	}
}

// With the mirror idle, agent-authored sends may use the whole aggregate
// allowance: the sub-cap must not silently become the agent's ceiling.
func TestChatSendLimiter_AgentMayUseFullAggregateWhenMirrorIdle(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithClock(clock.Now)

	for i := range chatSendAgentRatePerMinute {
		if !lim.Allow("a1", chatSenderAgent).Allowed {
			t.Fatalf("agent-authored send %d of %d refused; the full aggregate should be available", i+1, chatSendAgentRatePerMinute)
		}
	}
	if lim.Allow("a1", chatSenderAgent).Allowed {
		t.Error("the aggregate ceiling should be reached after the whole allowance is spent")
	}
}

// The ceiling is the aggregate per sender, whatever class the traffic claims:
// an agent that relabels its sends cannot buy a second allowance (#1054).
func TestChatSendLimiter_RelabellingCannotExceedAggregate(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithClock(clock.Now)

	// Alternate the two classes the outbound path can reach, in one instant,
	// exactly as a caller flipping req.Type would.
	classes := []chatSenderClass{chatSenderAgent, chatSenderAgentMirror}
	allowed := 0
	for i := range 4 * chatSendAgentRatePerMinute {
		if lim.Allow("a1", classes[i%len(classes)]).Allowed {
			allowed++
		}
	}

	if allowed != chatSendAgentRatePerMinute {
		t.Errorf("relabelling agent got %d sends through, want exactly the aggregate %d/min", allowed, chatSendAgentRatePerMinute)
	}
}

// Only the transcript mirror's own type gets the mirror reservation. Defence
// in depth: the class comes from a caller-supplied field, so an unfamiliar
// label must never land in the cheaper bucket (#1054).
func TestChatSenderClassForMessageType(t *testing.T) {
	tests := []struct {
		msgType string
		want    chatSenderClass
	}{
		{messages.TypeAssistantReply, chatSenderAgentMirror},
		{messages.TypeInputNeeded, chatSenderAgent},
		{messages.TypeInstruction, chatSenderAgent},
		{"", chatSenderAgent},
		{"not-a-real-type", chatSenderAgent},
		{"Assistant-Reply", chatSenderAgent},
	}
	for _, tc := range tests {
		if got := chatSenderClassForMessageType(tc.msgType); got != tc.want {
			t.Errorf("chatSenderClassForMessageType(%q) = %v, want %v", tc.msgType, got, tc.want)
		}
	}
}

// A class the limiter has no rate for must not be waved through. Unreachable
// today — only three classes are constructed — but fail-open is the wrong
// default for a rate limiter, and this is the branch a future class lands in.
func TestChatSendLimiter_UnknownClassIsLimitedAtTheStrictestRate(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithClock(clock.Now)

	const unknown = chatSenderClass(99)
	// Derive the expectation from the rate set the limiter was actually built
	// with, not from a hand-picked subset of the constants: a test that decides
	// for itself which rates count drifts from production the day one of the
	// others becomes the smallest, and fails for a reason that reads like a bug
	// in the limiter.
	strictest := 0.0
	for _, rate := range lim.ratesPerMinute {
		if strictest == 0 || rate < strictest {
			strictest = rate
		}
	}
	if strictest == 0 {
		t.Fatal("the production limiter was built with no rates at all")
	}
	if got := lim.limitFor(unknown); got != strictest {
		t.Fatalf("limitFor(unknown) = %v, want the strictest configured rate %v", got, strictest)
	}

	allowed := 0
	for range 200 {
		if lim.Allow("prober", unknown).Allowed {
			allowed++
		}
	}
	if float64(allowed) != strictest {
		t.Errorf("an unknown class got %d sends through, want the strictest rate %v", allowed, strictest)
	}

	// And it does not share a bucket with a real class of the same sender ID.
	if !lim.Allow("prober", chatSenderHuman).Allowed {
		t.Error("a human sender must not be throttled by traffic in an unrecognised class")
	}
}

// The production rates must cover every class in the enum. A class added
// without a rate should fail loudly at startup rather than quietly being
// limited at somebody else's number.
func TestChatSendLimiter_ProductionRatesCoverEveryClass(t *testing.T) {
	lim := newChatSendLimiter()
	for class := chatSenderClass(0); class < chatSenderClassCount; class++ {
		if got := lim.limitFor(class); got <= 0 {
			t.Errorf("sender class %d has no positive rate (%v)", class, got)
		}
	}

	if err := validateChatSendRates(map[chatSenderClass]float64{
		chatSenderHuman: chatSendHumanRatePerMinute,
		chatSenderAgent: chatSendAgentRatePerMinute,
	}); err == nil {
		t.Error("a rate map missing a class must be rejected, not accepted and silently back-filled")
	}
}

// A rate below one per minute throttles, it does not lock the sender out.
// Capacity tracked the rate, so a 0.5/min bucket could hold at most half the
// single token a send costs and no amount of waiting ever bought one: the
// sender was refused forever. Capacity is floored at one token so the slow
// rate becomes a slow drip.
func TestChatSendLimiter_SubOnePerMinuteRateStillEventuallyAllows(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman:       0.5,
		chatSenderAgent:       0.5,
		chatSenderAgentMirror: 0.5,
	}, clock.Now)

	if !lim.Allow("u1", chatSenderHuman).Allowed {
		t.Fatal("the first send at 0.5/min was refused; a fresh bucket must hold at least the one token a send costs")
	}
	if lim.Allow("u1", chatSenderHuman).Allowed {
		t.Fatal("the second send in the same instant should be refused at 0.5/min")
	}

	// At 0.5/min a token accrues every two minutes.
	clock.Advance(time.Minute)
	if lim.Allow("u1", chatSenderHuman).Allowed {
		t.Error("half a token later the sender should still be refused")
	}
	clock.Advance(time.Minute)
	if !lim.Allow("u1", chatSenderHuman).Allowed {
		t.Error("two minutes on, a full token has accrued and the send should be allowed")
	}

	// The floor is a floor, not a new burst size: idling does not bank sends.
	clock.Advance(time.Hour)
	if !lim.Allow("u1", chatSenderHuman).Allowed {
		t.Fatal("the sender should be allowed after a long idle")
	}
	if lim.Allow("u1", chatSenderHuman).Allowed {
		t.Error("a long idle should refill at most one token, not a burst")
	}
}

// Bucket state is bounded: senders that stop sending are forgotten.
func TestChatSendLimiter_EvictsIdleBuckets(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithClock(clock.Now)

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

// The threat the idle sweep does not cover: a caller minting fresh sender IDs
// faster than the TTL. The hard cap is the only thing bounding memory then,
// and it must not hand a flooder a way to reset its own bucket by churning
// IDs around it.
func TestChatSendLimiter_HardCapBoundsMemoryAndKeepsFloodersThrottled(t *testing.T) {
	clock := newTestClock()
	lim := newChatSendLimiterWithClock(clock.Now)

	// Exhaust one agent's aggregate allowance.
	for range chatSendAgentRatePerMinute {
		lim.Allow("flooder", chatSenderAgent)
	}
	if lim.Allow("flooder", chatSenderAgent).Allowed {
		t.Fatal("the flooder should be over its limit before the churn starts")
	}

	// Mint far more senders than the cap, well inside the idle TTL so the
	// TTL sweep evicts nothing and only the hard cap can bound the map. The
	// clock creeps forward so "least recently used" is meaningful, but by far
	// less than the second it takes the flooder to earn one token back.
	const extra = 2000
	peak := 0
	for i := range chatSendLimiterMaxBuckets + extra {
		lim.Allow(fmt.Sprintf("churn-%d", i), chatSenderAgent)
		if got := len(lim.buckets); got > peak {
			peak = got
		}
		if got := len(lim.buckets); got > chatSendLimiterMaxBuckets {
			t.Fatalf("tracked buckets = %d after %d senders, want at most the cap %d", got, i+1, chatSendLimiterMaxBuckets)
		}
		// Keep the flooder active, as a real flooder would be.
		if i%500 == 0 {
			clock.Advance(time.Millisecond)
			lim.Allow("flooder", chatSenderAgent)
		}
	}

	if peak < chatSendLimiterMaxBuckets {
		t.Fatalf("peak tracked buckets = %d, never reached the cap %d: the test did not exercise the hard-cap branch", peak, chatSendLimiterMaxBuckets)
	}
	if got := len(lim.buckets); got >= chatSendLimiterMaxBuckets {
		t.Errorf("tracked buckets = %d after the churn, want the cap %d to have forced an eviction", got, chatSendLimiterMaxBuckets)
	}

	// The flooder stayed active throughout, so eviction must not have dropped
	// it: churning sender IDs is not a way to buy a fresh allowance.
	if _, ok := lim.buckets["agent:flooder"]; !ok {
		t.Fatal("the active flooder's bucket was evicted; churning IDs would reset its limit")
	}
	if lim.Allow("flooder", chatSenderAgent).Allowed {
		t.Error("the flooder is still over its limit and must stay refused after the eviction")
	}
}

// A nil limiter allows everything rather than panicking: limiting is a
// protection, not a correctness requirement.
func TestChatSendLimiter_NilAllows(t *testing.T) {
	var lim *chatSendLimiter
	if decision := lim.Allow("u1", chatSenderHuman); !decision.Allowed || decision.RetryAfter != 0 {
		t.Errorf("nil limiter returned %+v, want allowed with no retry delay", decision)
	}
}

// Concurrent senders hitting one bucket hand out exactly the allowance — no
// more (the point of the limit) and no fewer (run with -race).
func TestChatSendLimiter_ConcurrentSendersGetExactlyTheAllowance(t *testing.T) {
	clock := newTestClock()
	const limit = 20
	lim := newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderAgent: limit,
	}, clock.Now)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lim.Allow("flooder", chatSenderAgent).Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != limit {
		t.Errorf("allowed %d of 100 concurrent sends, want exactly %d", got, limit)
	}
}
