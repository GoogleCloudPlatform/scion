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
	"context"
	"net"
	"sync"
	"time"
)

// Stage 1 webhook ingress token-bucket parameters (§8.1, §9.2).
const (
	PerIPRatePerSecond              = 20.0
	PerIPBurst                      = 100
	RejectionRatePerSecond          = 500.0
	RejectionBurst                  = 2000
	PerEndpointRatePerSecond        = 10.0
	PerEndpointBurst                = 60
	PerProjectVerifiedRatePerSecond = 50.0
	PerProjectVerifiedBurst         = 300
	HubVerifiedRatePerSecond        = 200.0
	HubVerifiedBurst                = 1000

	LimiterMaxEntries      = 10000
	LimiterMaxAge          = 30 * time.Minute
	LimiterCleanupInterval = 5 * time.Minute

	globalBucketKey = "*"
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

type keyedBucketSet struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	rate       float64
	burst      int
	maxEntries int
	maxAge     time.Duration
}

func newKeyedBucketSet(rate float64, burst, maxEntries int, maxAge time.Duration) *keyedBucketSet {
	return &keyedBucketSet{
		buckets:    make(map[string]*tokenBucket),
		rate:       rate,
		burst:      burst,
		maxEntries: maxEntries,
		maxAge:     maxAge,
	}
}

func (s *keyedBucketSet) allow(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.buckets[key]
	if !ok {
		if len(s.buckets) >= s.maxEntries {
			// Try evicting expired entries first; if still full, fail closed.
			s.cleanupLocked(now)
			if len(s.buckets) >= s.maxEntries {
				return false
			}
		}
		if s.burst < 1 {
			return false
		}
		s.buckets[key] = &tokenBucket{
			tokens:    float64(s.burst) - 1,
			lastCheck: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastCheck).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * s.rate
		if b.tokens > float64(s.burst) {
			b.tokens = float64(s.burst)
		}
		b.lastCheck = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (s *keyedBucketSet) cleanup(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
}

func (s *keyedBucketSet) cleanupLocked(now time.Time) {
	cutoff := now.Add(-s.maxAge)
	for k, b := range s.buckets {
		if b.lastCheck.Before(cutoff) {
			delete(s.buckets, k)
		}
	}
}

// LimiterConfig overrides default token-bucket parameters for tests. Zero
// values fall back to Stage 1 production defaults (§8.1, §9.2).
type LimiterConfig struct {
	PerIPRate               float64
	PerIPBurst              int
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
	CleanupInterval         time.Duration
	Now                     func() time.Time
}

// IngressLimiter enforces the five split token-bucket budgets of §8.1:
//  1. Per-client-IP (IPv6 /64, 20/s burst 100) before lookup or body read
//  2. Hub-wide rejection (500/s burst 2000) ONLY for unknown or disabled endpoint IDs
//  3. Per-endpoint (10/s burst 60) ONLY for known active endpoints (including sig failures, R2)
//  4. Per-project verified (50/s burst 300) ONLY after HMAC verification succeeds (R3)
//  5. Hub-wide verified (200/s burst 1000) ONLY after per-project verified passes
type IngressLimiter struct {
	perIP              *keyedBucketSet
	rejection          *keyedBucketSet
	perEndpoint        *keyedBucketSet
	perProjectVerified *keyedBucketSet
	hubVerified        *keyedBucketSet
	cleanupInterval    time.Duration
	now                func() time.Time
}

// NewIngressLimiter constructs an IngressLimiter with Stage 1 defaults (or any
// non-zero overrides in cfg).
func NewIngressLimiter(cfg LimiterConfig) *IngressLimiter {
	valFloat := func(v, def float64) float64 {
		if v > 0 {
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
	cleanupInterval := cfg.CleanupInterval
	if cleanupInterval <= 0 {
		cleanupInterval = LimiterCleanupInterval
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	return &IngressLimiter{
		perIP:              newKeyedBucketSet(valFloat(cfg.PerIPRate, PerIPRatePerSecond), valInt(cfg.PerIPBurst, PerIPBurst), maxEntries, maxAge),
		rejection:          newKeyedBucketSet(valFloat(cfg.RejectionRate, RejectionRatePerSecond), valInt(cfg.RejectionBurst, RejectionBurst), 1, maxAge),
		perEndpoint:        newKeyedBucketSet(valFloat(cfg.PerEndpointRate, PerEndpointRatePerSecond), valInt(cfg.PerEndpointBurst, PerEndpointBurst), maxEntries, maxAge),
		perProjectVerified: newKeyedBucketSet(valFloat(cfg.PerProjectVerifiedRate, PerProjectVerifiedRatePerSecond), valInt(cfg.PerProjectVerifiedBurst, PerProjectVerifiedBurst), maxEntries, maxAge),
		hubVerified:        newKeyedBucketSet(valFloat(cfg.HubVerifiedRate, HubVerifiedRatePerSecond), valInt(cfg.HubVerifiedBurst, HubVerifiedBurst), 1, maxAge),
		cleanupInterval:    cleanupInterval,
		now:                nowFn,
	}
}

// AllowClientIP charges the per-client-IP bucket (keyed via ClientRateKey).
func (l *IngressLimiter) AllowClientIP(clientIP string) bool {
	return l.perIP.allow(ClientRateKey(clientIP), l.now())
}

// AllowRejection charges the hub-wide rejection bucket (unknown/disabled IDs only).
func (l *IngressLimiter) AllowRejection() bool {
	return l.rejection.allow(globalBucketKey, l.now())
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
	return l.hubVerified.allow(globalBucketKey, l.now())
}

// Cleanup evicts stale keyed buckets as of now.
func (l *IngressLimiter) Cleanup(now time.Time) {
	l.perIP.cleanup(now)
	l.perEndpoint.cleanup(now)
	l.perProjectVerified.cleanup(now)
}

// StartCleanup runs periodic bucket eviction until ctx is cancelled.
func (l *IngressLimiter) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(l.cleanupInterval)
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
