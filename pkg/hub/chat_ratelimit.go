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
)

// Per-sender send limits for chat messages (#1054). Without them a single
// looping agent can fill a thread faster than any reader can scroll, and the
// cost lands on every consumer of that thread: the store, the SSE fan-out and
// the notification pipeline.
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
	// chatSendHumanRatePerMinute is the sustained send rate allowed for a
	// single human sender.
	chatSendHumanRatePerMinute = 30

	// chatSendAgentRatePerMinute is the sustained send rate allowed for a
	// single agent. Agents legitimately send more than humans (status
	// reports, replies to several recipients), so they get a higher ceiling.
	chatSendAgentRatePerMinute = 60

	// chatSendLimiterIdleTTL is how long an untouched bucket is kept before
	// being evicted. It also bounds how often the sweep runs.
	chatSendLimiterIdleTTL = 10 * time.Minute

	// chatSendLimiterMaxBuckets caps the tracked senders so the map cannot
	// grow without bound. Reaching it forces an immediate sweep.
	chatSendLimiterMaxBuckets = 10000
)

// chatSenderClass distinguishes the rate classes. Buckets are keyed by class
// as well as ID so a user and an agent that share an ID cannot drain each
// other's allowance.
type chatSenderClass int

const (
	chatSenderHuman chatSenderClass = iota
	chatSenderAgent
)

// chatSenderClassFor maps an authenticated identity to its rate class.
// Anything that is not an agent is rated as a human.
func chatSenderClassFor(ident Identity) chatSenderClass {
	if ident != nil && ident.Type() == "agent" {
		return chatSenderAgent
	}
	return chatSenderHuman
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

	humanPerMinute float64
	agentPerMinute float64

	// now is the clock, injectable so tests can exhaust and refill a bucket
	// without sleeping a real minute.
	now func() time.Time
}

// newChatSendLimiter creates a limiter with the production limits.
func newChatSendLimiter() *chatSendLimiter {
	return newChatSendLimiterWithRates(chatSendHumanRatePerMinute, chatSendAgentRatePerMinute, time.Now)
}

// newChatSendLimiterWithRates creates a limiter with explicit limits and
// clock. A rate of zero or less disables limiting for that class.
func newChatSendLimiterWithRates(humanPerMinute, agentPerMinute float64, now func() time.Time) *chatSendLimiter {
	if now == nil {
		now = time.Now
	}
	return &chatSendLimiter{
		buckets:        make(map[string]*chatSendBucket),
		lastSweep:      now(),
		humanPerMinute: humanPerMinute,
		agentPerMinute: agentPerMinute,
		now:            now,
	}
}

// limitFor returns the per-minute allowance for a rate class.
func (l *chatSendLimiter) limitFor(class chatSenderClass) float64 {
	if l == nil {
		return 0
	}
	if class == chatSenderAgent {
		return l.agentPerMinute
	}
	return l.humanPerMinute
}

// Allow consumes one token for the sender and reports whether the send may
// proceed. When it may not, the returned duration is how long the caller
// should wait before retrying — suitable for a Retry-After header.
//
// A nil limiter allows everything: limiting is a protection, not a
// correctness requirement, and a hand-constructed Server must still work.
func (l *chatSendLimiter) Allow(senderID string, class chatSenderClass) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}

	perMinute, prefix := l.limitFor(class), "user:"
	if class == chatSenderAgent {
		prefix = "agent:"
	}
	if perMinute <= 0 {
		return true, 0
	}
	perSecond := perMinute / 60

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	key := prefix + senderID
	b, ok := l.buckets[key]
	if !ok {
		b = &chatSendBucket{tokens: perMinute, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(perMinute, b.tokens+now.Sub(b.last).Seconds()*perSecond)
		b.last = now
	}

	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / perSecond * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	b.tokens--
	return true, 0
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
	allowed, retryAfter := s.chatSendLimiter.Allow(senderID, class)
	if allowed {
		return true
	}

	seconds := int(math.Ceil(retryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited,
		fmt.Sprintf("send rate limit exceeded (%d messages per minute); retry in %ds",
			int(s.chatSendLimiter.limitFor(class)), seconds), nil)
	return false
}
