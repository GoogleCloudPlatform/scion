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
	"sync"
	"time"
)

// chatIdempotencyTTL is how long an idempotency key is remembered.
const chatIdempotencyTTL = 5 * time.Minute

// idempotencyCacheCleanupThreshold is the number of entries beyond which
// an amortized sweep of expired entries is triggered.
const idempotencyCacheCleanupThreshold = 100

// idempotencyCacheKey is the map key for the cache. Using a struct instead
// of string concatenation avoids the need for a separator character and
// makes collisions impossible.
type idempotencyCacheKey struct {
	senderID       string
	idempotencyKey string
}

// chatIdempotencyEntry stores a cached idempotency result.
type chatIdempotencyEntry struct {
	messageID string
	expiresAt time.Time
}

// ChatIdempotencyCache is a lightweight in-memory cache keyed by
// (senderID, idempotencyKey). Entries expire after 5 minutes.
// It is safe for concurrent use.
type ChatIdempotencyCache struct {
	mu          sync.Mutex
	entries     map[idempotencyCacheKey]chatIdempotencyEntry
	nextCleanup time.Time
}

// NewChatIdempotencyCache creates a new empty idempotency cache.
func NewChatIdempotencyCache() *ChatIdempotencyCache {
	return &ChatIdempotencyCache{
		entries: make(map[idempotencyCacheKey]chatIdempotencyEntry),
	}
}

// Check returns the existing message ID if the (senderID, idempotencyKey)
// pair was seen within the TTL window.
func (c *ChatIdempotencyCache) Check(senderID, idempotencyKey string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	// Rate-limited amortized cleanup: only sweep when cache grows beyond
	// threshold AND enough time has passed since the last sweep.
	if len(c.entries) > idempotencyCacheCleanupThreshold && now.After(c.nextCleanup) {
		c.cleanExpiredLocked(now)
		c.nextCleanup = now.Add(chatIdempotencyTTL)
	}

	key := idempotencyCacheKey{senderID: senderID, idempotencyKey: idempotencyKey}
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, key)
		return "", false
	}
	return entry.messageID, true
}

// Record stores a (senderID, idempotencyKey) -> messageID mapping.
func (c *ChatIdempotencyCache) Record(senderID, idempotencyKey, messageID string) {
	if idempotencyKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	// Rate-limited amortized cleanup in Record too, so entries never
	// accumulate unboundedly even if Check() is rarely called.
	if len(c.entries) > idempotencyCacheCleanupThreshold && now.After(c.nextCleanup) {
		c.cleanExpiredLocked(now)
		c.nextCleanup = now.Add(chatIdempotencyTTL)
	}

	key := idempotencyCacheKey{senderID: senderID, idempotencyKey: idempotencyKey}
	c.entries[key] = chatIdempotencyEntry{
		messageID: messageID,
		expiresAt: now.Add(chatIdempotencyTTL),
	}
}

// cleanExpiredLocked removes entries whose TTL has passed.
// Must be called with c.mu held.
func (c *ChatIdempotencyCache) cleanExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}
