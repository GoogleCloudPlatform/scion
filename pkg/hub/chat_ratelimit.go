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
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// Per-sender send limits for chat messages (#1054). Without them a single
// looping agent can fill a thread faster than any reader can scroll, and the
// cost lands on every consumer of that thread: the store, the SSE fan-out and
// the notification pipeline.
//
// THE ENFORCED CEILING IS THE AGGREGATE PER SENDER: 30/min for a human,
// 60/min for an agent, whatever the caller puts in the request body. Class
// sub-limits below are reservations carved out of that aggregate, never extra
// budget — a send is charged to its class bucket *and* to the sender's
// aggregate bucket, and is refused if either is empty.
//
// The limits are per sender, not global: a busy fleet of many agents is normal
// traffic, a single sender emitting more than one message per second is not.
// Bucket capacity equals the per-minute allowance, so a burst (an agent
// reporting completion to a dozen recipients at once, a human pasting a series
// of messages) passes untouched and only sustained flooding is throttled.
//
// Hub-wide rate limiting is tracked separately in issue #302; these limits are
// deliberately scoped to the chat send paths and should stay consistent with
// whatever that work lands.
const (
	// chatSendHumanRatePerMinute is the total send rate allowed for a single
	// human sender.
	chatSendHumanRatePerMinute = 30

	// chatSendAgentRatePerMinute is the total send rate allowed for a single
	// agent, across every kind of traffic it produces. Agents legitimately
	// send more than humans (status reports, replies to several recipients),
	// so they get a higher ceiling.
	chatSendAgentRatePerMinute = 60

	// chatSendAgentMirrorRatePerMinute is the share of an agent's aggregate
	// allowance that the automatic assistant-reply transcript mirror may
	// consume. It is a sub-cap inside chatSendAgentRatePerMinute, not an
	// addition to it: the mirror is machine generated and must not be able to
	// spend the whole allowance an agent needs for a completion report or a
	// blocker escalation, but it cannot raise the agent's total either.
	//
	// Sized so a flooding mirror always leaves at least
	// chatSendAgentRatePerMinute - chatSendAgentMirrorRatePerMinute of
	// headroom for the agent's own messages.
	chatSendAgentMirrorRatePerMinute = 30

	// chatSendLimiterIdleTTL is how long an untouched bucket is kept before
	// being evicted. It also bounds how often the sweep runs.
	chatSendLimiterIdleTTL = 10 * time.Minute

	// chatSendLimiterMaxBuckets caps the tracked senders so the map cannot
	// grow without bound. Reaching it forces an immediate sweep.
	chatSendLimiterMaxBuckets = 10000
)

// chatSenderClass distinguishes both who is sending and what kind of traffic
// it is. Buckets are keyed by class as well as by ID, so a user and an agent
// that share an ID cannot drain each other's allowance and — more
// importantly — an agent's automatic transcript mirror cannot starve the
// messages that agent writes itself.
//
// A class that is not its own aggregate (see aggregate) is a reservation
// inside another class's ceiling rather than a ceiling of its own.
type chatSenderClass int

const (
	chatSenderHuman chatSenderClass = iota
	chatSenderAgent
	chatSenderAgentMirror

	// chatSenderClassCount is one past the last class, not a class itself.
	// The startup check walks the enum with it, so a class added above is
	// checked without anyone having to remember a separate list.
	chatSenderClassCount
)

// keyPrefix namespaces a class's buckets. An unrecognised class gets its own
// namespace rather than sharing a real one, so it can neither drain nor be
// drained by a legitimate sender that happens to have the same ID.
func (c chatSenderClass) keyPrefix() string {
	switch c {
	case chatSenderHuman:
		return "user:"
	case chatSenderAgent:
		return "agent:"
	case chatSenderAgentMirror:
		return "agent-mirror:"
	default:
		return "unknown-class:"
	}
}

// aggregate returns the class holding the sender's real ceiling. The mirror
// spends an agent's allowance, so its aggregate is the agent class; every
// other class is its own aggregate.
//
// This is what makes the traffic class safe to derive from a caller-supplied
// field: whichever class a sender claims, the same aggregate bucket is
// charged, so relabelling traffic cannot buy a second allowance. It can only
// move the sender into a *smaller* reservation.
func (c chatSenderClass) aggregate() chatSenderClass {
	if c == chatSenderAgentMirror {
		return chatSenderAgent
	}
	return c
}

// chatSenderClassForMessageType maps an outbound message type to its traffic
// class. Only the transcript mirror's own type gets the mirror reservation;
// every other type — including one this build does not recognise — is charged
// as an agent-authored message, so an unfamiliar label can never be used to
// claim a reservation it is not entitled to.
//
// The class is only ever a reservation *within* the sender's aggregate
// allowance (see aggregate), so a sender cannot gain budget by choosing a
// class either way.
func chatSenderClassForMessageType(msgType string) chatSenderClass {
	if msgType == messages.TypeAssistantReply {
		return chatSenderAgentMirror
	}
	return chatSenderAgent
}

// noun describes a class in a rate-limit error message.
func (c chatSenderClass) noun() string {
	if c == chatSenderAgentMirror {
		return "assistant-reply messages"
	}
	return "messages"
}

// chatSendBucket is one sender's token bucket.
type chatSendBucket struct {
	tokens float64
	last   time.Time // last time this bucket was touched
}

// chatSendLimiter is a per-sender token-bucket limiter for the chat send
// paths. It is safe for concurrent use.
type chatSendLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*chatSendBucket
	lastSweep time.Time

	// ratesPerMinute is the allowance per rate class. Every class the
	// production limiter can see has an entry (enforced at construction by
	// validateChatSendRates); a class without one is not unlimited, it falls
	// back to strictestRate. See limitFor.
	ratesPerMinute map[chatSenderClass]float64

	// strictestRate is the smallest configured allowance, used for any class
	// with no rate of its own.
	strictestRate float64

	// now is the clock, injectable so tests can exhaust and refill a bucket
	// without sleeping a real minute.
	now func() time.Time
}

// newChatSendLimiter creates a limiter with the production limits.
func newChatSendLimiter() *chatSendLimiter {
	return newChatSendLimiterWithClock(time.Now)
}

// newChatSendLimiterWithClock creates a limiter with the production limits
// and an injectable clock.
//
// It panics if a class has no rate. That is a programming error, not a runtime
// condition: it is deterministic, it fires the first time a Server is
// constructed (so any test catches it), and the alternative — starting up with
// a class nobody is limiting — is exactly the silent failure this check
// exists to prevent.
func newChatSendLimiterWithClock(now func() time.Time) *chatSendLimiter {
	rates := map[chatSenderClass]float64{
		chatSenderHuman:       chatSendHumanRatePerMinute,
		chatSenderAgent:       chatSendAgentRatePerMinute,
		chatSenderAgentMirror: chatSendAgentMirrorRatePerMinute,
	}
	if err := validateChatSendRates(rates); err != nil {
		panic(err)
	}
	return newChatSendLimiterWithRates(rates, now)
}

// validateChatSendRates reports whether every sender class has a positive
// allowance. A class added to the enum without a rate would otherwise be
// limited at the strictest rate instead of its own, which is safe but not what
// the author meant — better to hear about it at startup.
func validateChatSendRates(ratesPerMinute map[chatSenderClass]float64) error {
	for class := chatSenderClass(0); class < chatSenderClassCount; class++ {
		if ratesPerMinute[class] <= 0 {
			return fmt.Errorf("chat send limiter: sender class %d has no positive rate configured", class)
		}
	}
	return nil
}

// newChatSendLimiterWithRates creates a limiter with explicit limits and clock.
// It is the test seam: production goes through newChatSendLimiterWithClock,
// which additionally requires every class to be configured. A class omitted
// here is limited at the strictest rate given, never left unlimited.
func newChatSendLimiterWithRates(ratesPerMinute map[chatSenderClass]float64, now func() time.Time) *chatSendLimiter {
	if now == nil {
		now = time.Now
	}
	strictest := 0.0
	for _, rate := range ratesPerMinute {
		if rate > 0 && (strictest == 0 || rate < strictest) {
			strictest = rate
		}
	}
	return &chatSendLimiter{
		buckets:        make(map[string]*chatSendBucket),
		lastSweep:      now(),
		ratesPerMinute: ratesPerMinute,
		strictestRate:  strictest,
		now:            now,
	}
}

// limitFor returns the per-minute allowance for a rate class.
//
// A class with no rate of its own falls back to the strictest configured rate.
// It deliberately does not fall back to "unlimited": a rate limiter is a
// security control and fail-open is the wrong default for one, however
// unreachable the path looks today. It equally does not deny outright, which
// would turn a future missing rate into a total outage of the send path — the
// strictest known rate closes the bypass without that risk.
func (l *chatSendLimiter) limitFor(class chatSenderClass) float64 {
	if l == nil {
		return 0
	}
	if rate, ok := l.ratesPerMinute[class]; ok && rate > 0 {
		return rate
	}
	return l.strictestRate
}

// chatSendDecision is the outcome of a rate-limit check.
type chatSendDecision struct {
	// Allowed reports whether the send may proceed.
	Allowed bool
	// RetryAfter is how long to wait before retrying. When both the
	// aggregate bucket and the class reservation are empty it is the longer
	// of the two waits. Zero when Allowed.
	RetryAfter time.Duration
	// Limit is the per-minute allowance of the bucket that refused, and
	// LimitClass is the class that bucket belongs to, so the error can name
	// the limit the sender actually hit. Zero when Allowed.
	Limit      float64
	LimitClass chatSenderClass
}

// Allow consumes one token from the sender's aggregate bucket and one from
// the class reservation (when the class is not its own aggregate), and
// reports whether the send may proceed. It is refused if either bucket is
// empty, and nothing is consumed from either when it is refused.
//
// A nil limiter allows everything: limiting is a protection, not a
// correctness requirement, and a hand-constructed Server must still work.
func (l *chatSendLimiter) Allow(senderID string, class chatSenderClass) chatSendDecision {
	if l == nil {
		return chatSendDecision{Allowed: true}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	aggregate := l.refillLocked(senderID, class.aggregate(), now)
	var reservation *chatSendBucket
	if class != class.aggregate() {
		reservation = l.refillLocked(senderID, class, now)
	}

	// Refuse if either bucket is empty, reporting whichever has the longer
	// wait: that is the one the sender actually has to wait out.
	decision := chatSendDecision{Allowed: true}
	consider := func(b *chatSendBucket, c chatSenderClass) {
		if b == nil || b.tokens >= 1 {
			return
		}
		perMinute := l.limitFor(c)
		wait := time.Duration((1 - b.tokens) / (perMinute / 60) * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		if decision.Allowed || wait > decision.RetryAfter {
			decision = chatSendDecision{RetryAfter: wait, Limit: perMinute, LimitClass: c}
		}
	}
	consider(aggregate, class.aggregate())
	consider(reservation, class)
	if !decision.Allowed {
		return decision
	}

	if aggregate != nil {
		aggregate.tokens--
	}
	if reservation != nil {
		reservation.tokens--
	}
	return decision
}

// refillLocked returns the sender's bucket for a class, creating it if needed
// and crediting the tokens accrued since it was last touched. It returns nil
// when the limiter has no positive rate at all — an empty rate map means "no
// limiting configured", the same as a nil limiter, and nothing in production
// constructs one. The caller must hold l.mu.
//
// Capacity is floored at one token. A sub-1/minute rate would otherwise cap the
// bucket below the single token a send costs, so it could never accumulate
// enough to allow one — a permanent block rather than a slow drip. Flooring the
// capacity leaves the refill rate untouched: the sender still waits 1/perMinute
// minutes between sends, but the send eventually happens.
func (l *chatSendLimiter) refillLocked(senderID string, class chatSenderClass, now time.Time) *chatSendBucket {
	perMinute := l.limitFor(class)
	if perMinute <= 0 {
		return nil
	}
	capacity := math.Max(1, perMinute)

	key := class.keyPrefix() + senderID
	b, ok := l.buckets[key]
	if !ok {
		b = &chatSendBucket{tokens: capacity, last: now}
		l.buckets[key] = b
		return b
	}
	b.tokens = math.Min(capacity, b.tokens+now.Sub(b.last).Seconds()*perMinute/60)
	b.last = now
	return b
}

// sweepLocked evicts idle buckets. It runs at most once per idle TTL, or
// immediately when the map has hit its cap. The caller must hold l.mu.
func (l *chatSendLimiter) sweepLocked(now time.Time) {
	atCap := len(l.buckets) >= chatSendLimiterMaxBuckets
	if !atCap && now.Sub(l.lastSweep) < chatSendLimiterIdleTTL {
		return
	}
	l.lastSweep = now

	for k, b := range l.buckets {
		if now.Sub(b.last) >= chatSendLimiterIdleTTL {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < chatSendLimiterMaxBuckets {
		return
	}

	// Still at the cap with every bucket recently active: drop the
	// least-recently-used half rather than the whole map, so a burst of new
	// senders cannot reset the limits of the busiest ones.
	keys := make([]string, 0, len(l.buckets))
	for k := range l.buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return l.buckets[keys[i]].last.Before(l.buckets[keys[j]].last)
	})
	for _, k := range keys[:len(keys)/2] {
		delete(l.buckets, k)
	}
}

// allowChatSend applies the per-sender send limit. When the sender is over
// its limit it writes a 429 with a Retry-After header and returns false; the
// caller must stop. The error is deliberately explicit rather than a silent
// drop, so an agent that hits the limit can back off and resend (#1054).
func (s *Server) allowChatSend(w http.ResponseWriter, senderID string, class chatSenderClass) bool {
	decision := s.chatSendLimiter.Allow(senderID, class)
	if decision.Allowed {
		return true
	}

	seconds := int(math.Ceil(decision.RetryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	// The delay goes in the body as well as the header: no current client
	// reads Retry-After, so the message text is what a sending agent sees.
	writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
		fmt.Sprintf("send rate limit exceeded (%d %s per minute); retry in %ds",
			int(decision.Limit), decision.LimitClass.noun(), seconds), nil)
	return false
}
