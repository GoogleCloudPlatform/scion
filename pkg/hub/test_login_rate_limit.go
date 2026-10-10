// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"net"
	"sync"
	"time"
)

// Defaults for the per-source-IP rate limit on POST /api/v1/auth/test-login.
//
// test-login is a test-only endpoint (enabled by --enable-test-login). An
// end-to-end suite signs in a handful of fixture identities per run, usually
// from one address, so the limit is sized to stay out of the way of a full
// suite while capping a runaway caller: a burst of testLoginRateBurst calls,
// refilled at testLoginRatePerSecond (60 per minute) after that.
const (
	testLoginRateBurst     = 60
	testLoginRatePerSecond = 1.0
	// testLoginLimiterMaxAge is how long an idle source keeps its bucket.
	// A bucket idle this long has fully refilled, so dropping it is
	// equivalent to keeping it.
	testLoginLimiterMaxAge = 10 * time.Minute
	// testLoginLimiterMaxBuckets caps how many sources are tracked. The
	// limiter runs before the challenge token is checked, so the table
	// must stay bounded whatever callers send. At the cap, known sources
	// keep their buckets and new sources are refused (fail closed) until
	// idle buckets age out.
	testLoginLimiterMaxBuckets = 4096
	// testLoginLimiterSweepInterval is the minimum time between sweeps of
	// idle buckets, so a flood of new sources at the cap cannot make every
	// call scan the whole table.
	testLoginLimiterSweepInterval = time.Minute
)

// testLoginLimiter is a per-source-IP token-bucket limiter for test-login.
// It is per WebServer instance: with several hub instances the effective
// limit scales with the instance count.
type testLoginLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	burst      float64
	perSecond  float64
	maxBuckets int
	lastSweep  time.Time
	now        func() time.Time
}

func newTestLoginLimiter() *testLoginLimiter {
	return &testLoginLimiter{
		buckets:    make(map[string]*tokenBucket),
		burst:      testLoginRateBurst,
		perSecond:  testLoginRatePerSecond,
		maxBuckets: testLoginLimiterMaxBuckets,
		now:        time.Now,
	}
}

// Allow reports whether a call from source key may proceed, consuming one
// token if so.
func (l *testLoginLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxBuckets {
			l.sweepLocked(now)
			if len(l.buckets) >= l.maxBuckets {
				return false
			}
		}
		l.buckets[key] = &tokenBucket{tokens: l.burst - 1, lastCheck: now}
		return true
	}

	b.tokens += now.Sub(b.lastCheck).Seconds() * l.perSecond
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.lastCheck = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked drops buckets idle longer than testLoginLimiterMaxAge, at most
// once per testLoginLimiterSweepInterval. The caller holds l.mu.
func (l *testLoginLimiter) sweepLocked(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < testLoginLimiterSweepInterval {
		return
	}
	l.lastSweep = now
	cutoff := now.Add(-testLoginLimiterMaxAge)
	for k, b := range l.buckets {
		if b.lastCheck.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

// testLoginRateKey returns the rate-limit key for a request's remote
// address: the IP for IPv4, and the /64 prefix for IPv6, since a single
// IPv6 client usually controls a whole /64.
func testLoginRateKey(remoteAddr string) string {
	host := remoteIP(remoteAddr)
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
