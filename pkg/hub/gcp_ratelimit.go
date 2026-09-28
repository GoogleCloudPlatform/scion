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
	"sync"
	"time"
)

// GCPTokenRateLimiter enforces per-agent rate limits on GCP token requests.
type GCPTokenRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*tokenBucket
	rate     float64       // tokens per second
	burst    int           // max burst
	cleanup  time.Duration // how often to clean up stale entries
}

// NewGCPTokenRateLimiter creates a rate limiter with the given rate (tokens/sec) and burst size.
// Call StartCleanup to begin the background cleanup goroutine.
func NewGCPTokenRateLimiter(ratePerSecond float64, burst int) *GCPTokenRateLimiter {
	return &GCPTokenRateLimiter{
		limiters: make(map[string]*tokenBucket),
		rate:     ratePerSecond,
		burst:    burst,
		cleanup:  10 * time.Minute,
	}
}

// StartCleanup starts the background goroutine that removes stale entries.
// It exits when ctx is cancelled.
func (rl *GCPTokenRateLimiter) StartCleanup(ctx context.Context) {
	go rl.cleanupLoop(ctx)
}

// Allow returns true if the request is allowed for the given agent ID.
func (rl *GCPTokenRateLimiter) Allow(agentID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, ok := rl.limiters[agentID]
	if !ok {
		b = &tokenBucket{
			tokens:    float64(rl.burst) - 1, // consume one token
			lastCheck: now,
		}
		rl.limiters[agentID] = b
		return true
	}

	// Refill tokens based on elapsed time
	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastCheck = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (rl *GCPTokenRateLimiter) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(rl.cleanup)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-30 * time.Minute)
			for id, b := range rl.limiters {
				if b.lastCheck.Before(cutoff) {
					delete(rl.limiters, id)
				}
			}
			rl.mu.Unlock()
		}
	}
}
