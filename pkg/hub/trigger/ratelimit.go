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

package trigger

import (
	"math"
	"net"
	"sync"
	"time"
)

// Stage 1 webhook ingress token-bucket parameters (§8.1, §9.2).
const (
	PerIPRatePerSecond              = 20.0
	PerIPBurst                      = 100
	PerIPOverflowRatePerSecond      = 500.0
	PerIPOverflowBurst              = 1000
	RejectionRatePerSecond          = 500.0
	RejectionBurst                  = 2000
	PerEndpointRatePerSecond        = 10.0
	PerEndpointBurst                = 60
	PerProjectVerifiedRatePerSecond = 50.0
	PerProjectVerifiedBurst         = 300
	HubVerifiedRatePerSecond        = 200.0
	HubVerifiedBurst                = 1000

	LimiterMaxEntries = 10000
	LimiterMaxAge     = 30 * time.Minute
)

// ClientRateKey returns the per-client-IP bucket key: the IPv4 address as-is,
// or the /64 prefix of an IPv6 address (§8.1 step 2, matching cspReportClientKey).
func ClientRateKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

type tokenBucket struct {
	tokens    float64
	lastCheck time.Time
}

// singleTokenBucket is a concurrency-safe global token bucket (used for the
// hub-wide rejection and hub-wide verified tiers, which have no caller key and
// therefore no map-capacity bound).
type singleTokenBucket struct {
	mu          sync.Mutex
	tokens      float64
	lastCheck   time.Time
	initialized bool
	rate        float64
	burst       int
}

func newSingleTokenBucket(rate float64, burst int) *singleTokenBucket {
	return &singleTokenBucket{
		rate:  rate,
		burst: burst,
	}
}

func (b *singleTokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.burst < 1 {
		return false
	}
	if !b.initialized {
		b.initialized = true
		b.tokens = float64(b.burst) - 1
		b.lastCheck = now
		return true
	}
	elapsed := now.Sub(b.lastCheck).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > float64(b.burst) {
			b.tokens = float64(b.burst)
		}
		b.lastCheck = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type keyedBucketSet struct {
	mu           sync.Mutex
	buckets      *boundedTable[string, *tokenBucket]
	rate         float64
	burst        int
	refillPeriod time.Duration
	maxEntries   int
	maxAge       time.Duration
	overflow     *singleTokenBucket
}

// computeRefillPeriod computes burst/rate as a time.Duration, clamping against
// int64 overflow for extremely small rates so refillPeriod never wraps negative.
func computeRefillPeriod(rate float64, burst int) time.Duration {
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) || burst <= 0 {
		return 0
	}
	ns := float64(time.Second) * float64(burst) / rate
	if math.IsNaN(ns) || math.IsInf(ns, 0) || ns >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	if ns < 0 {
		return 0
	}
	return time.Duration(ns)
}

func newKeyedBucketSet(rate float64, burst, maxEntries int, maxAge time.Duration, overflow *singleTokenBucket, onScan func(int)) *keyedBucketSet {
	return &keyedBucketSet{
		buckets:      newBoundedTable[string, *tokenBucket](maxEntries, onScan, nil),
		rate:         rate,
		burst:        burst,
		refillPeriod: computeRefillPeriod(rate, burst),
		maxEntries:   maxEntries,
		maxAge:       maxAge,
		overflow:     overflow,
	}
}

func (s *keyedBucketSet) allow(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.burst < 1 {
		return false
	}

	b, ok := s.buckets.get(key)
	if !ok {
		cutoff := now.Add(-s.maxAge)
		nb := &tokenBucket{
			tokens:    float64(s.burst) - 1,
			lastCheck: now,
		}
		// Evict a refilled/expired bucket in O(1) bounded probes; dropping a
		// fully refilled bucket changes no admission decision for timestamps >= now.
		inserted := s.buckets.tryPutEvictWhere(key, nb, boundedEvictProbes, func(_ string, cand *tokenBucket) bool {
			return cand.lastCheck.Before(cutoff) || s.isRefilled(cand, now)
		})
		if !inserted {
			if s.overflow != nil {
				return s.overflow.allow(now)
			}
			return false
		}
		return true
	}

	s.buckets.recordScan(0)
	elapsed := now.Sub(b.lastCheck).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * s.rate
		if b.tokens > float64(s.burst) {
			b.tokens = float64(s.burst)
		}
		b.lastCheck = now
		s.buckets.touch(key)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (s *keyedBucketSet) isRefilled(b *tokenBucket, now time.Time) bool {
	idle := now.Sub(b.lastCheck)
	if idle <= 0 {
		return b.tokens >= float64(s.burst)
	}
	if s.refillPeriod > 0 && idle >= s.refillPeriod {
		return true
	}
	return b.tokens+idle.Seconds()*s.rate >= float64(s.burst)
}

func (s *keyedBucketSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buckets.len()
}

// LimiterConfig overrides default token-bucket parameters for tests. Zero
// values fall back to Stage 1 production defaults (§8.1, §9.2).
type LimiterConfig struct {
	PerIPRate               float64
	PerIPBurst              int
	PerIPOverflowRate       float64
	PerIPOverflowBurst      int
	RejectionRate           float64
	RejectionBurst          int
	PerEndpointRate         float64
	PerEndpointBurst        int
	PerProjectVerifiedRate  float64
	PerProjectVerifiedBurst int
	HubVerifiedRate         float64
	HubVerifiedBurst        int
	MaxEntries              int
	MaxAge                  time.Duration
	Now                     func() time.Time
	OnEvictScan             func(steps int)
}

// IngressLimiter enforces the five split token-bucket budgets of §8.1:
//  1. Per-client-IP (IPv6 /64, 20/s burst 100) before lookup or body read
//  2. Hub-wide rejection (500/s burst 2000) ONLY for unknown or disabled endpoint IDs
//  3. Per-endpoint (10/s burst 60) ONLY for known active endpoints (including sig failures, R2)
//  4. Per-project verified (50/s burst 300) ONLY after HMAC verification succeeds (R3)
//  5. Hub-wide verified (200/s burst 1000) ONLY after per-project verified passes
//
// Keyed tables (perIP, perEndpoint, perProjectVerified) bound memory via
// boundedTable (maxEntries = 10,000) and evict refilled/stale entries on demand
// in O(1) bounded probes, with no background sweep goroutine or ticker.
type IngressLimiter struct {
	perIP              *keyedBucketSet
	rejection          *singleTokenBucket
	perEndpoint        *keyedBucketSet
	perProjectVerified *keyedBucketSet
	hubVerified        *singleTokenBucket
	now                func() time.Time
}

// NewIngressLimiter constructs an IngressLimiter with Stage 1 defaults (or any
// non-zero overrides in cfg).
func NewIngressLimiter(cfg LimiterConfig) *IngressLimiter {
	valFloat := func(v, def float64) float64 {
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v
		}
		return def
	}
	valInt := func(v, def int) int {
		if v > 0 {
			return v
		}
		return def
	}
	maxEntries := valInt(cfg.MaxEntries, LimiterMaxEntries)
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = LimiterMaxAge
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	perIPOverflow := newSingleTokenBucket(
		valFloat(cfg.PerIPOverflowRate, PerIPOverflowRatePerSecond),
		valInt(cfg.PerIPOverflowBurst, PerIPOverflowBurst),
	)

	return &IngressLimiter{
		perIP:              newKeyedBucketSet(valFloat(cfg.PerIPRate, PerIPRatePerSecond), valInt(cfg.PerIPBurst, PerIPBurst), maxEntries, maxAge, perIPOverflow, cfg.OnEvictScan),
		rejection:          newSingleTokenBucket(valFloat(cfg.RejectionRate, RejectionRatePerSecond), valInt(cfg.RejectionBurst, RejectionBurst)),
		perEndpoint:        newKeyedBucketSet(valFloat(cfg.PerEndpointRate, PerEndpointRatePerSecond), valInt(cfg.PerEndpointBurst, PerEndpointBurst), maxEntries, maxAge, nil, cfg.OnEvictScan),
		perProjectVerified: newKeyedBucketSet(valFloat(cfg.PerProjectVerifiedRate, PerProjectVerifiedRatePerSecond), valInt(cfg.PerProjectVerifiedBurst, PerProjectVerifiedBurst), maxEntries, maxAge, nil, cfg.OnEvictScan),
		hubVerified:        newSingleTokenBucket(valFloat(cfg.HubVerifiedRate, HubVerifiedRatePerSecond), valInt(cfg.HubVerifiedBurst, HubVerifiedBurst)),
		now:                nowFn,
	}
}

// AllowClientIP charges the per-client-IP bucket (keyed via ClientRateKey).
// When the bounded per-IP table is full even after evicting refilled buckets,
// new keys are routed through a shared overflow bucket (500/s burst 1000)
// rather than failing closed (which would lock out new senders hub-wide) or
// passing unlimited (SEC-2); the rejection, per-endpoint, verified, and
// pending-cap tiers remain enforced downstream.
func (l *IngressLimiter) AllowClientIP(clientIP string) bool {
	return l.perIP.allow(ClientRateKey(clientIP), l.now())
}

// AllowRejection charges the hub-wide rejection bucket (unknown/disabled IDs only).
func (l *IngressLimiter) AllowRejection() bool {
	return l.rejection.allow(l.now())
}

// AllowEndpoint charges the per-endpoint bucket for a known active endpoint.
func (l *IngressLimiter) AllowEndpoint(endpointID string) bool {
	return l.perEndpoint.allow(endpointID, l.now())
}

// AllowProjectVerified charges the per-project verified bucket after signature verification.
func (l *IngressLimiter) AllowProjectVerified(projectID string) bool {
	return l.perProjectVerified.allow(projectID, l.now())
}

// AllowHubVerified charges the hub-wide verified bucket after per-project verified admission.
func (l *IngressLimiter) AllowHubVerified() bool {
	return l.hubVerified.allow(l.now())
}

// PerIPLen returns the current number of tracked per-client-IP buckets.
func (l *IngressLimiter) PerIPLen() int {
	return l.perIP.len()
}
