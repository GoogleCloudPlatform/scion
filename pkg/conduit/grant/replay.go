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

package grant

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ReplayCache records consumed grant IDs. Consume must be atomic: among
// concurrent calls with the same jti, exactly one returns fresh=true. exp is
// the latest time the grant could still verify; the entry may be forgotten
// after it. An implementation must return fresh=false once exp has passed on
// its own clock, so that forgetting an entry can never re-admit its jti.
type ReplayCache interface {
	Consume(ctx context.Context, jti string, exp time.Time) (fresh bool, err error)
}

// ErrReplayCacheFull is returned when the cache is at capacity with no
// expired entries to evict. Verify then refuses the grant (fail closed).
var ErrReplayCacheFull = errors.New("grant: replay cache full")

// DefaultReplayCacheCapacity bounds a MemoryReplayCache built with capacity 0.
const DefaultReplayCacheCapacity = 1 << 16

// MemoryReplayCache is an in-process ReplayCache with expiry-based eviction.
// A target keeps one per process; because grants bind to connection_epoch, a
// restarted target (an empty cache) cannot be fed an older connection's
// grants.
type MemoryReplayCache struct {
	mu       sync.Mutex
	seen     map[string]time.Time
	now      func() time.Time
	capacity int
	nextScan time.Time
}

// NewMemoryReplayCache returns an empty cache. now is the clock used for
// eviction (time.Now when nil); capacity bounds the number of live entries
// (DefaultReplayCacheCapacity when <= 0).
func NewMemoryReplayCache(now func() time.Time, capacity int) *MemoryReplayCache {
	if now == nil {
		now = time.Now
	}
	if capacity <= 0 {
		capacity = DefaultReplayCacheCapacity
	}
	return &MemoryReplayCache{seen: make(map[string]time.Time), now: now, capacity: capacity}
}

// Consume implements ReplayCache.
func (c *MemoryReplayCache) Consume(ctx context.Context, jti string, exp time.Time) (bool, error) {
	if jti == "" {
		return false, errors.New("grant: empty jti")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, used := c.seen[jti]; used {
		return false, nil
	}
	// A jti whose exp has passed on the cache's own clock is never fresh:
	// its entry may already have been evicted, so a caller whose now trails
	// this clock must not be able to consume it again.
	if !now.Before(exp) {
		return false, nil
	}
	if !now.Before(c.nextScan) || len(c.seen) >= c.capacity {
		c.evictLocked(now)
	}
	if len(c.seen) >= c.capacity {
		return false, ErrReplayCacheFull
	}
	c.seen[jti] = exp
	return true, nil
}

// Len returns the number of remembered jtis.
func (c *MemoryReplayCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func (c *MemoryReplayCache) evictLocked(now time.Time) {
	for jti, exp := range c.seen {
		if now.After(exp) {
			delete(c.seen, jti)
		}
	}
	c.nextScan = now.Add(MaxValidity)
}
