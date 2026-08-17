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

func TestChatIdempotencyCache_CheckAndRecord(t *testing.T) {
	c := NewChatIdempotencyCache()

	// Before recording, Check should return false.
	_, ok := c.Check("user1", "key1")
	if ok {
		t.Fatal("expected Check to return false for unknown key")
	}

	// Record and then Check should return the message ID.
	c.Record("user1", "key1", "msg-abc")
	id, ok := c.Check("user1", "key1")
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

	id1, ok1 := c.Check("user1", "key1")
	id2, ok2 := c.Check("user2", "key1")

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
	_, ok := c.Check("user1", "")
	if ok {
		t.Fatal("empty idempotency key should not be recorded")
	}
}

func TestChatIdempotencyCache_ExpiresAfterTTL(t *testing.T) {
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

	_, ok := c.Check("user1", "key1")
	if ok {
		t.Fatal("expected expired entry to be cleaned up")
	}
}
