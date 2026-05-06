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
	"net"
	"net/http"
	"strings"
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

// bucketEntry wraps a token bucket with LRU tracking.
type bucketEntry struct {
	bucket   *tokenBucket
	lastUsed time.Time
}

// RateLimiter provides per-key token bucket rate limiting with LRU eviction.
type RateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucketEntry
	rate       float64
	burst      int
	maxBuckets int
}

// NewRateLimiter creates a rate limiter with the given per-key rate and burst.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		buckets:    make(map[string]*bucketEntry),
		rate:       rate,
		burst:      burst,
		maxBuckets: 10000,
	}
}

func (rl *RateLimiter) getBucket(key string) *tokenBucket {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	entry, ok := rl.buckets[key]
	if ok {
		entry.lastUsed = now
		return entry.bucket
	}
	if len(rl.buckets) >= rl.maxBuckets {
		rl.evictOldest()
	}
	b := newTokenBucket(rl.rate, rl.burst)
	rl.buckets[key] = &bucketEntry{bucket: b, lastUsed: now}
	return b
}

// evictOldest removes the least recently used bucket. Must be called with rl.mu held.
func (rl *RateLimiter) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, entry := range rl.buckets {
		if first || entry.lastUsed.Before(oldestTime) {
			oldestKey = k
			oldestTime = entry.lastUsed
			first = false
		}
	}
	if oldestKey != "" {
		delete(rl.buckets, oldestKey)
	}
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
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if cfg.TrustProxy {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					if i := strings.Index(xff, ","); i >= 0 {
						host = strings.TrimSpace(xff[:i])
					} else {
						host = strings.TrimSpace(xff)
					}
				}
			}
			key = host
		}

		if !limiter.Allow(key) {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}
