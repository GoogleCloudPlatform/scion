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

// chatIdempotencyEntry stores a cached idempotency result. A send moves
// through three states: admitted (in flight, no message yet), persisted
// (in flight, messageID known, dispatch still running) and done. Only a
// done entry is replayed: a persisted row's dispatch state is not final
// until its dispatch ends.
type chatIdempotencyEntry struct {
	messageID string
	done      bool
	expiresAt time.Time
}

// IdempotencyBeginResult is the outcome of ChatIdempotencyCache.Begin.
type IdempotencyBeginResult int

const (
	// IdempotencyNew: the key is unseen; the caller owns the send and must
	// end it with Record (its outcome is final) or Finish (deferred, so it
	// also runs on panic).
	IdempotencyNew IdempotencyBeginResult = iota
	// IdempotencyDone: a send with this key already created a message.
	IdempotencyDone
	// IdempotencyInFlight: a send with this key is still running.
	IdempotencyInFlight
)

// ChatIdempotencyCache is a lightweight in-memory cache keyed by
// (senderID, idempotencyKey): keys are scoped per user, not per
// conversation. Entries expire after 5 minutes. It is safe for concurrent
// use.
//
// Limit: the cache lives in one hub process. With several hub replicas,
// or after a hub restart, a retry with the same key is not recognised and
// sends again; and an entry (in flight or finished) is forgotten after
// chatIdempotencyTTL. Deduplication is therefore "at most once per hub
// process within the TTL", not durable; a store-backed key would be the
// durable fix.
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

// Begin admits a send for (senderID, idempotencyKey). It returns the
// existing message ID when a send with the key already finished
// (IdempotencyDone), IdempotencyInFlight while one is still running, and
// otherwise marks the key in flight and returns IdempotencyNew. A retry of
// a long send (for example a wake whose connection dropped) can thus learn
// the outcome instead of sending twice. An empty key is always new and
// never marked.
func (c *ChatIdempotencyCache) Begin(senderID, idempotencyKey string) (string, IdempotencyBeginResult) {
	if idempotencyKey == "" {
		return "", IdempotencyNew
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if len(c.entries) > idempotencyCacheCleanupThreshold && now.After(c.nextCleanup) {
		c.cleanExpiredLocked(now)
		c.nextCleanup = now.Add(chatIdempotencyTTL)
	}

	key := idempotencyCacheKey{senderID: senderID, idempotencyKey: idempotencyKey}
	if entry, ok := c.entries[key]; ok && !now.After(entry.expiresAt) {
		if !entry.done {
			return "", IdempotencyInFlight
		}
		return entry.messageID, IdempotencyDone
	}
	c.entries[key] = chatIdempotencyEntry{expiresAt: now.Add(chatIdempotencyTTL)}
	return "", IdempotencyNew
}

// MarkPersisted notes the message a send stored, keeping the key in
// flight: Begin keeps answering IdempotencyInFlight until Record or Finish,
// because the row's dispatch state is not final yet. It is what makes a
// panic or dropped request during dispatch safe: Finish then marks the key
// done instead of releasing it.
func (c *ChatIdempotencyCache) MarkPersisted(senderID, idempotencyKey, messageID string) {
	if idempotencyKey == "" || messageID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := idempotencyCacheKey{senderID: senderID, idempotencyKey: idempotencyKey}
	if entry, ok := c.entries[key]; ok && !entry.done {
		entry.messageID = messageID
		entry.expiresAt = time.Now().Add(chatIdempotencyTTL)
		c.entries[key] = entry
	}
}

// Finish ends a send that did not Record its outcome; deferred by the
// owner of an IdempotencyNew key, so it also runs on panic. A key with a
// persisted message becomes done (a retry is told about that message and
// does not send it again); a key without one is released, so a retry may
// send. A done key is left alone.
func (c *ChatIdempotencyCache) Finish(senderID, idempotencyKey string) {
	if idempotencyKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := idempotencyCacheKey{senderID: senderID, idempotencyKey: idempotencyKey}
	entry, ok := c.entries[key]
	if !ok || entry.done {
		return
	}
	if entry.messageID == "" {
		delete(c.entries, key)
		return
	}
	entry.done = true
	entry.expiresAt = time.Now().Add(chatIdempotencyTTL)
	c.entries[key] = entry
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
	// accumulate unboundedly even if Begin() is rarely called.
	if len(c.entries) > idempotencyCacheCleanupThreshold && now.After(c.nextCleanup) {
		c.cleanExpiredLocked(now)
		c.nextCleanup = now.Add(chatIdempotencyTTL)
	}

	key := idempotencyCacheKey{senderID: senderID, idempotencyKey: idempotencyKey}
	c.entries[key] = chatIdempotencyEntry{
		messageID: messageID,
		done:      true,
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
