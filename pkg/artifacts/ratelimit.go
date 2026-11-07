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

package artifacts

import (
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// Rate limits of Ask for review (ptone/scion#5145): the notification a
// grant with notify sends, never the grant itself. NotifyPerHour and
// NotifyBurst are one requester's; NotifyGlobalPerHour and
// NotifyGlobalBurst bound all requesters together.
const (
	NotifyPerHour       = 30
	NotifyBurst         = 10
	NotifyGlobalPerHour = 600
	NotifyGlobalBurst   = 60
	// notifyMaxRequesters caps the requesters tracked at once, as
	// sharedMaxClients does for share links.
	notifyMaxRequesters = 10000
)

// Rate limits of the share-link route. A shared read needs no session, so
// the route is reachable by anyone; the limits bound how fast one client,
// and all clients together, can try tokens. A request is charged before
// its token is looked at, so a limited request costs the same whatever it
// carries.
const (
	// SharedClientPerMinute and SharedClientBurst are one client's
	// sustained rate and burst.
	SharedClientPerMinute = 30
	SharedClientBurst     = 30
	// SharedGlobalPerMinute and SharedGlobalBurst bound all clients
	// together.
	SharedGlobalPerMinute = 600
	SharedGlobalBurst     = 600
	// sharedMaxClients caps the clients tracked at once. A new client
	// arriving when every slot holds a bucket that is still refilling is
	// not tracked and is limited by the shared limit alone, so the table
	// cannot grow without bound.
	sharedMaxClients = 10000
	// sharedSweepEvery spaces the sweeps a full table triggers, so a
	// stream of new clients costs one pass over the table per interval,
	// not one per request.
	sharedSweepEvery = time.Second
)

// tokenBucket is a token bucket refilled continuously.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// refill brings b up to date at now.
func (b *tokenBucket) refill(now time.Time, perSecond, burst float64) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*perSecond)
	}
	b.last = now
}

// wait is how many whole seconds until b holds one token.
func (b *tokenBucket) wait(perSecond float64) int {
	if perSecond <= 0 {
		return 60
	}
	return max(1, int(math.Ceil((1-b.tokens)/perSecond)))
}

// rateLimiter is a per-client token bucket limiter under a global bucket.
// It is safe for concurrent use.
type rateLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	clientRate  float64 // tokens per second
	clientBurst float64
	globalRate  float64
	globalBurst float64
	maxClients  int
	lastSweep   time.Time
	global      tokenBucket
	clients     map[string]*tokenBucket
}

// newRateLimiter is newPeriodLimiter with per-minute rates, the share-link
// route's limiter.
func newRateLimiter(now func() time.Time, clientPerMinute, clientBurst, globalPerMinute, globalBurst, maxClients int) *rateLimiter {
	return newPeriodLimiter(now, clientPerMinute, time.Minute, clientBurst, globalPerMinute, time.Minute, globalBurst, maxClients)
}

// newPeriodLimiter returns a limiter that allows each client n requests per
// period per (burst burst) and all clients together globalN per globalPer
// (burst globalBurst), tracking at most maxClients clients.
func newPeriodLimiter(now func() time.Time, n int, per time.Duration, burst, globalN int, globalPer time.Duration, globalBurst, maxClients int) *rateLimiter {
	// Every count, period, burst and size is at least 1, so a bucket always
	// refills and wait never divides by zero.
	n, burst = max(n, 1), max(burst, 1)
	globalN, globalBurst, maxClients = max(globalN, 1), max(globalBurst, 1), max(maxClients, 1)
	per, globalPer = max(per, time.Second), max(globalPer, time.Second)
	t := now()
	return &rateLimiter{
		now:         now,
		clientRate:  float64(n) / per.Seconds(),
		clientBurst: float64(burst),
		globalRate:  float64(globalN) / globalPer.Seconds(),
		globalBurst: float64(globalBurst),
		maxClients:  maxClients,
		global:      tokenBucket{tokens: float64(globalBurst), last: t},
		clients:     map[string]*tokenBucket{},
	}
}

// allow charges one request to client key and to the global bucket, or,
// for a client the full table cannot track, to the global bucket alone. It
// returns false, and the seconds to wait, when a charged bucket is empty;
// a refused request charges nothing.
func (l *rateLimiter) allow(key string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.global.refill(now, l.globalRate, l.globalBurst)
	b, ok := l.clients[key]
	if !ok {
		if len(l.clients) >= l.maxClients && now.Sub(l.lastSweep) >= sharedSweepEvery {
			l.sweep(now)
		}
		if len(l.clients) >= l.maxClients {
			// The table is full of buckets that are still refilling: the
			// newcomer is not tracked (the table stays bounded, nothing is
			// evicted) and is limited by the shared limit alone, which
			// bounds all clients together.
			if l.global.tokens < 1 {
				return l.global.wait(l.globalRate), false
			}
			l.global.tokens--
			return 0, true
		}
		b = &tokenBucket{tokens: l.clientBurst, last: now}
		l.clients[key] = b
	}
	b.refill(now, l.clientRate, l.clientBurst)
	if b.tokens < 1 {
		return b.wait(l.clientRate), false
	}
	if l.global.tokens < 1 {
		return l.global.wait(l.globalRate), false
	}
	b.tokens--
	l.global.tokens--
	return 0, true
}

// sweep drops the buckets that have refilled completely: forgetting one
// changes nothing, because a new client starts with a full bucket.
func (l *rateLimiter) sweep(now time.Time) {
	l.lastSweep = now
	for k, b := range l.clients {
		b.refill(now, l.clientRate, l.clientBurst)
		if b.tokens >= l.clientBurst {
			delete(l.clients, k)
		}
	}
}

// remoteHost is the default client key: the host part of RemoteAddr.
func remoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
