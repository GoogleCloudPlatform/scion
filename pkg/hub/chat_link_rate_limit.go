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
	"sync"
	"time"
)

const (
	verifyRatePerSecond = 5.0 / 60.0
	verifyBurst         = 5
	verifyLimiterMaxAge = 30 * time.Minute
)

// linkVerifyLimiter is per service instance. With multiple Hub instances, the
// effective per-IP limit scales with the instance count; code entropy and
// expiration remain the primary protection against brute-force attempts.
type linkVerifyLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

func newLinkVerifyLimiter() linkVerifyLimiter {
	return linkVerifyLimiter{buckets: make(map[string]*tokenBucket)}
}

func (l *linkVerifyLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[ip]
	if !ok {
		l.buckets[ip] = &tokenBucket{
			tokens:    float64(verifyBurst) - 1,
			lastCheck: now,
		}
		return true
	}

	b.tokens += now.Sub(b.lastCheck).Seconds() * verifyRatePerSecond
	if b.tokens > float64(verifyBurst) {
		b.tokens = float64(verifyBurst)
	}
	b.lastCheck = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *linkVerifyLimiter) Cleanup(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-verifyLimiterMaxAge)
	for ip, b := range l.buckets {
		if b.lastCheck.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}
