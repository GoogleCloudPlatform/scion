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

package bridge

import (
	"net/http"
	"sync"
	"time"
)

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	max      float64
	rate     float64
	lastFill time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{
		tokens:   float64(burst),
		max:      float64(burst),
		rate:     rate,
		lastFill: time.Now(),
	}
}

func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastFill).Seconds()
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.max {
		tb.tokens = tb.max
	}
	tb.lastFill = now

	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}

// RateLimiter provides per-key token bucket rate limiting.
type RateLimiter struct {
	mu             sync.Mutex
	buckets        map[string]*tokenBucket
	rate           float64
	burst          int
	maxBuckets     int
	overflowBucket *tokenBucket
}

// NewRateLimiter creates a rate limiter with the given per-key rate and burst.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		buckets:        make(map[string]*tokenBucket),
		rate:           rate,
		burst:          burst,
		maxBuckets:     10000,
		overflowBucket: newTokenBucket(0, 0),
	}
}

func (rl *RateLimiter) getBucket(key string) *tokenBucket {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= rl.maxBuckets {
			return rl.overflowBucket
		}
		b = newTokenBucket(rl.rate, rl.burst)
		rl.buckets[key] = b
	}
	return b
}

// Allow checks if a request from the given key is allowed.
func (rl *RateLimiter) Allow(key string) bool {
	return rl.getBucket(key).allow()
}

// RateLimitMiddleware wraps an http.Handler with rate limiting.
func RateLimitMiddleware(next http.Handler, cfg RateLimitConfig) http.Handler {
	if !cfg.Enabled {
		return next
	}

	rate := cfg.RequestsPerSec
	if rate == 0 {
		rate = 10
	}
	burst := cfg.Burst
	if burst == 0 {
		burst = 20
	}

	limiter := NewRateLimiter(rate, burst)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = r.RemoteAddr
		}

		if !limiter.Allow(key) {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}
