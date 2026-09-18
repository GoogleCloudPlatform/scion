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
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// GE exchange endpoint rate limiter — per-client-IP token bucket.
//
// Constants are chosen for a pre-auth public endpoint that bridges a handful
// of Google Enterprise replicas to the Hub. Normal workload: a few exchanges
// per minute per bridge replica. The limiter defends against brute-force
// credential stuffing and outbound Google API amplification.
//
// Cache/exchange metrics (INFO-2) are NOT implemented here because #1620
// harness already records explicit per-replica cache hit/miss/exchange
// counters at the bridge layer, which is the correct observation point.
// ---------------------------------------------------------------------------

const (
	// geExchangeRatePerSecond is the sustained token refill rate per IP.
	// 10 requests/minute ≈ 0.167/s — generous enough for several bridge
	// replicas behind the same egress IP, tight enough to bound outbound
	// Google calls under abuse.
	geExchangeRatePerSecond = 10.0 / 60.0

	// geExchangeBurst is the maximum burst allowed per IP before the
	// sustained rate kicks in. Covers cold-start bursts from multiple
	// bridge replicas simultaneously initializing.
	geExchangeBurst = 20

	// geExchangeLimiterMaxEntries is the maximum number of tracked client
	// IPs. Under hostile unique-IP churn, this bounds memory to O(maxEntries).
	// When full, new IPs are rejected (fail closed) until cleanup frees slots.
	geExchangeLimiterMaxEntries = 10000

	// geExchangeLimiterMaxAge is the maximum age of a limiter entry before
	// it is eligible for cleanup. Entries older than this are evicted even
	// if the bucket has remaining tokens.
	geExchangeLimiterMaxAge = 30 * time.Minute

	// geExchangeCleanupInterval is how often the background goroutine
	// sweeps stale entries.
	geExchangeCleanupInterval = 5 * time.Minute

	// geExchangeMaxBodyBytes is the maximum request body size for the
	// exchange endpoint. Google ID tokens are typically 800-1200 bytes;
	// access tokens are ~170 bytes. The JSON envelope adds ~50 bytes.
	// 8 KB provides generous headroom without accepting unbounded bodies.
	geExchangeMaxBodyBytes = 8 * 1024
)

// geExchangeRateLimiter is a per-client-IP token bucket rate limiter for the
// GE exchange endpoint. It is bounded in size: at most MaxEntries IPs are
// tracked, with deterministic expiry/cleanup under hostile churn.
type geExchangeRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*geExchangeBucket
	// Configurable for testing.
	rate       float64
	burst      int
	maxEntries int
	maxAge     time.Duration
	nowFunc    func() time.Time
}

type geExchangeBucket struct {
	tokens    float64
	lastCheck time.Time
}

// newGEExchangeRateLimiter creates a new rate limiter with production defaults.
func newGEExchangeRateLimiter() *geExchangeRateLimiter {
	return &geExchangeRateLimiter{
		buckets:    make(map[string]*geExchangeBucket),
		rate:       geExchangeRatePerSecond,
		burst:      geExchangeBurst,
		maxEntries: geExchangeLimiterMaxEntries,
		maxAge:     geExchangeLimiterMaxAge,
		nowFunc:    time.Now,
	}
}

// Allow checks whether the given client IP is within the rate limit.
// Returns (allowed bool, retryAfterSeconds int).
// When the limiter is at capacity (maxEntries), new IPs are rejected to
// prevent unbounded memory growth under hostile churn.
func (l *geExchangeRateLimiter) Allow(clientIP string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFunc()
	b, ok := l.buckets[clientIP]
	if !ok {
		// New IP — check capacity bound.
		if len(l.buckets) >= l.maxEntries {
			// At capacity: fail closed. Retry after one cleanup interval.
			return false, int(math.Ceil(geExchangeCleanupInterval.Seconds()))
		}
		l.buckets[clientIP] = &geExchangeBucket{
			tokens:    float64(l.burst) - 1,
			lastCheck: now,
		}
		return true, 0
	}

	// Refill tokens.
	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastCheck = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Compute Retry-After: time until 1 token refills.
	deficit := 1.0 - b.tokens
	retrySeconds := int(math.Ceil(deficit / l.rate))
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	return false, retrySeconds
}

// Cleanup removes entries older than maxAge.
func (l *geExchangeRateLimiter) Cleanup(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.maxAge)
	for ip, b := range l.buckets {
		if b.lastCheck.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}

// Len returns the number of tracked IPs (for testing).
func (l *geExchangeRateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// StartCleanup runs the background cleanup goroutine. It exits when ctx is
// cancelled. Call this once during server startup.
func (l *geExchangeRateLimiter) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(geExchangeCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				l.Cleanup(t)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Client IP extraction — safe trusted-proxy semantics.
//
// Reuses the Hub-wide parseTrustedProxies/isTrustedProxy from auth.go.
// When the immediate peer is in TrustedProxies, parses X-Forwarded-For
// right-to-left to find the first untrusted hop (the real client). This is
// the only safe approach: the leftmost entry is attacker-controlled if the
// proxy chain is not fully trusted.
// ---------------------------------------------------------------------------

// geExchangeClientIP extracts the rate-limit key IP from the request.
// When the immediate peer is a trusted proxy, walks X-Forwarded-For
// right-to-left past trusted hops to find the first untrusted client IP.
// Falls back to RemoteAddr. Never trusts forwarding headers from untrusted
// peers.
func geExchangeClientIP(r *http.Request, trustedNets []*net.IPNet) string {
	peerIP := remoteIP(r.RemoteAddr)

	// If no trusted proxies configured or peer is not trusted, use RemoteAddr.
	if len(trustedNets) == 0 || !isTrustedProxy(r, trustedNets) {
		return normalizeIPForRateLimit(peerIP)
	}

	// Peer is trusted — walk X-Forwarded-For right-to-left past trusted hops.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(parts[i])
			ip := net.ParseIP(candidate)
			if ip == nil {
				// Malformed entry — stop and use the peer IP (fail closed).
				break
			}
			// Check if this hop is also trusted.
			trusted := false
			for _, n := range trustedNets {
				if n.Contains(ip) {
					trusted = true
					break
				}
			}
			if !trusted {
				// First untrusted hop = the real client.
				return normalizeIPForRateLimit(candidate)
			}
		}
	}

	// All XFF hops were trusted (or XFF absent) — try X-Real-IP.
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		if net.ParseIP(realIP) != nil {
			return normalizeIPForRateLimit(realIP)
		}
	}

	// Fallback to peer.
	return normalizeIPForRateLimit(peerIP)
}

// normalizeIPForRateLimit normalizes IPv4/IPv6 to a canonical string form.
// IPv4-mapped IPv6 (::ffff:1.2.3.4) is normalized to the IPv4 form so that
// dual-stack clients cannot evade the limiter.
func normalizeIPForRateLimit(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return raw // unparseable — use as-is
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
