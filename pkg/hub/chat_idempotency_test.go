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
	"testing"
	"time"
)

// peekIdempotency reports a finished entry without Begin's side effect of
// marking an unseen key in flight.
func peekIdempotency(c *ChatIdempotencyCache, senderID, key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[idempotencyCacheKey{senderID: senderID, idempotencyKey: key}]
	if !ok || time.Now().After(entry.expiresAt) || entry.messageID == "" {
		return "", false
	}
	return entry.messageID, true
}

func TestChatIdempotencyCache_CheckAndRecord(t *testing.T) {
	c := NewChatIdempotencyCache()

	// Before recording, Check should return false.
	_, ok := peekIdempotency(c, "user1", "key1")
	if ok {
		t.Fatal("expected Check to return false for unknown key")
	}

	// Record and then Check should return the message ID.
	c.Record("user1", "key1", "msg-abc")
	id, ok := peekIdempotency(c, "user1", "key1")
	if !ok {
		t.Fatal("expected Check to return true after Record")
	}
	if id != "msg-abc" {
		t.Errorf("expected message ID %q, got %q", "msg-abc", id)
	}
}

func TestChatIdempotencyCache_DifferentSenders(t *testing.T) {
	c := NewChatIdempotencyCache()

	c.Record("user1", "key1", "msg-1")
	c.Record("user2", "key1", "msg-2")

	id1, ok1 := peekIdempotency(c, "user1", "key1")
	id2, ok2 := peekIdempotency(c, "user2", "key1")

	if !ok1 || id1 != "msg-1" {
		t.Errorf("user1: expected msg-1, got %q (ok=%v)", id1, ok1)
	}
	if !ok2 || id2 != "msg-2" {
		t.Errorf("user2: expected msg-2, got %q (ok=%v)", id2, ok2)
	}
}

func TestChatIdempotencyCache_EmptyKeySkipped(t *testing.T) {
	c := NewChatIdempotencyCache()

	c.Record("user1", "", "msg-no-key")
	_, ok := peekIdempotency(c, "user1", "")
	if ok {
		t.Fatal("empty idempotency key should not be recorded")
	}
}

func TestChatIdempotencyCache_ExpiresAfterTTL(t *testing.T) {
	// An expired finished entry is forgotten: Begin admits the key again.
	c := NewChatIdempotencyCache()

	c.Record("user1", "key1", "msg-1")

	// Manually expire the entry.
	c.mu.Lock()
	for k := range c.entries {
		c.entries[k] = chatIdempotencyEntry{
			messageID: "msg-1",
			expiresAt: time.Now().Add(-1 * time.Second),
		}
	}
	c.mu.Unlock()

	if _, r := c.Begin("user1", "key1"); r != IdempotencyNew {
		t.Fatalf("Begin after expiry = %v, want new", r)
	}
}

func TestChatIdempotencyCache_BeginRecordAbandon(t *testing.T) {
	c := NewChatIdempotencyCache()

	if _, r := c.Begin("u", "k"); r != IdempotencyNew {
		t.Fatalf("first Begin = %v, want new", r)
	}
	if _, r := c.Begin("u", "k"); r != IdempotencyInFlight {
		t.Fatalf("second Begin = %v, want in flight", r)
	}
	if _, ok := peekIdempotency(c, "u", "k"); ok {
		t.Fatal("Check must not report an unfinished send")
	}
	c.Record("u", "k", "msg-1")
	if id, r := c.Begin("u", "k"); r != IdempotencyDone || id != "msg-1" {
		t.Fatalf("Begin after Record = %q %v, want msg-1 done", id, r)
	}
	// Abandon leaves a finished key alone.
	c.Abandon("u", "k")
	if id, ok := peekIdempotency(c, "u", "k"); !ok || id != "msg-1" {
		t.Fatalf("Check after Abandon of finished key = %q %v", id, ok)
	}

	// Abandon frees an unfinished key.
	c.Begin("u", "k2")
	c.Abandon("u", "k2")
	if _, r := c.Begin("u", "k2"); r != IdempotencyNew {
		t.Fatalf("Begin after Abandon = %v, want new", r)
	}
}

func TestChatIdempotencyCache_BeginEmptyKeyNeverMarked(t *testing.T) {
	c := NewChatIdempotencyCache()
	for i := 0; i < 2; i++ {
		if _, r := c.Begin("u", ""); r != IdempotencyNew {
			t.Fatalf("empty key Begin = %v, want new", r)
		}
	}
}
