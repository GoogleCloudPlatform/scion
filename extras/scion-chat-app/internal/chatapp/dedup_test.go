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

package chatapp

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestMsgDedupKey(t *testing.T) {
	// Nil message returns empty.
	if got := msgDedupKey(nil); got != "" {
		t.Errorf("nil message: expected empty key, got %q", got)
	}

	// Empty Msg field returns empty (no fingerprint needed).
	if got := msgDedupKey(&messages.StructuredMessage{Sender: "a"}); got != "" {
		t.Errorf("empty Msg field: expected empty key, got %q", got)
	}

	// Deterministic: same input yields the same key.
	msg := &messages.StructuredMessage{
		Sender:    "agent:test",
		Recipient: "user:alice",
		Timestamp: "2026-08-11T12:00:00Z",
		Type:      messages.TypeInstruction,
		Msg:       "hello world",
	}
	key1 := msgDedupKey(msg)
	key2 := msgDedupKey(msg)
	if key1 == "" {
		t.Fatal("expected non-empty key for valid message")
	}
	if key1 != key2 {
		t.Errorf("determinism: key1 %q != key2 %q", key1, key2)
	}

	// Different messages produce different keys.
	msg2 := &messages.StructuredMessage{
		Sender:    "agent:test",
		Recipient: "user:alice",
		Timestamp: "2026-08-11T12:00:00Z",
		Type:      messages.TypeInstruction,
		Msg:       "different content",
	}
	key3 := msgDedupKey(msg2)
	if key1 == key3 {
		t.Errorf("expected different keys for different messages, both got %q", key1)
	}

	// Changing sender also produces a different key.
	msg3 := &messages.StructuredMessage{
		Sender:    "agent:other",
		Recipient: "user:alice",
		Timestamp: "2026-08-11T12:00:00Z",
		Type:      messages.TypeInstruction,
		Msg:       "hello world",
	}
	key4 := msgDedupKey(msg3)
	if key1 == key4 {
		t.Errorf("expected different keys for different senders, both got %q", key1)
	}

	// Changing timestamp produces a different key.
	msg4 := &messages.StructuredMessage{
		Sender:    "agent:test",
		Recipient: "user:alice",
		Timestamp: "2026-08-11T12:01:00Z",
		Type:      messages.TypeInstruction,
		Msg:       "hello world",
	}
	key5 := msgDedupKey(msg4)
	if key1 == key5 {
		t.Errorf("expected different keys for different timestamps, both got %q", key1)
	}
}

func TestBrokerPublish_Dedup(t *testing.T) {
	log := slog.Default()

	t.Run("same message within TTL is skipped", func(t *testing.T) {
		var callCount atomic.Int32
		handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
			callCount.Add(1)
			return nil
		}
		broker := NewBrokerServer(handler, log)

		msg := &messages.StructuredMessage{
			Sender:    "agent:test",
			Recipient: "user:alice",
			Timestamp: "2026-08-11T12:00:00Z",
			Type:      messages.TypeInstruction,
			Msg:       "hello world",
		}

		ctx := context.Background()

		// First publish should succeed.
		if err := broker.Publish(ctx, "topic.test", msg); err != nil {
			t.Fatalf("first Publish failed: %v", err)
		}
		if got := callCount.Load(); got != 1 {
			t.Fatalf("expected handler called once, got %d", got)
		}

		// Second publish with identical message should be skipped.
		if err := broker.Publish(ctx, "topic.test", msg); err != nil {
			t.Fatalf("second Publish failed: %v", err)
		}
		if got := callCount.Load(); got != 1 {
			t.Errorf("expected handler still called once (dedup), got %d", got)
		}
	})

	t.Run("different message is not skipped", func(t *testing.T) {
		var callCount atomic.Int32
		handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
			callCount.Add(1)
			return nil
		}
		broker := NewBrokerServer(handler, log)

		ctx := context.Background()

		msg1 := &messages.StructuredMessage{
			Sender: "agent:test", Recipient: "user:alice",
			Timestamp: "2026-08-11T12:00:00Z", Type: messages.TypeInstruction,
			Msg: "message one",
		}
		msg2 := &messages.StructuredMessage{
			Sender: "agent:test", Recipient: "user:alice",
			Timestamp: "2026-08-11T12:00:01Z", Type: messages.TypeInstruction,
			Msg: "message two",
		}

		if err := broker.Publish(ctx, "topic.test", msg1); err != nil {
			t.Fatalf("Publish msg1 failed: %v", err)
		}
		if err := broker.Publish(ctx, "topic.test", msg2); err != nil {
			t.Fatalf("Publish msg2 failed: %v", err)
		}
		if got := callCount.Load(); got != 2 {
			t.Errorf("expected handler called twice, got %d", got)
		}
	})

	t.Run("message after TTL expiry is not skipped", func(t *testing.T) {
		var callCount atomic.Int32
		handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
			callCount.Add(1)
			return nil
		}
		broker := NewBrokerServer(handler, log)

		msg := &messages.StructuredMessage{
			Sender:    "agent:test",
			Recipient: "user:alice",
			Timestamp: "2026-08-11T12:00:00Z",
			Type:      messages.TypeInstruction,
			Msg:       "hello world",
		}

		ctx := context.Background()

		// First publish.
		if err := broker.Publish(ctx, "topic.test", msg); err != nil {
			t.Fatalf("first Publish failed: %v", err)
		}

		// Simulate TTL expiry by backdating the entry.
		key := msgDedupKey(msg)
		broker.sentIDsMu.Lock()
		broker.sentIDs[key] = time.Now().Add(-(dedupTTL + time.Second))
		broker.sentIDsMu.Unlock()

		// Second publish should succeed after TTL expiry.
		if err := broker.Publish(ctx, "topic.test", msg); err != nil {
			t.Fatalf("second Publish after TTL failed: %v", err)
		}
		if got := callCount.Load(); got != 2 {
			t.Errorf("expected handler called twice after TTL expiry, got %d", got)
		}
	})

	t.Run("nil message is not deduped", func(t *testing.T) {
		broker := NewBrokerServer(nil, log)
		if err := broker.Publish(context.Background(), "topic.test", nil); err != nil {
			t.Fatalf("Publish(nil) failed: %v", err)
		}
	})

	t.Run("empty Msg field skips dedup", func(t *testing.T) {
		var callCount atomic.Int32
		handler := func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
			callCount.Add(1)
			return nil
		}
		broker := NewBrokerServer(handler, log)

		msg := &messages.StructuredMessage{
			Sender: "agent:test",
			Type:   messages.TypeInstruction,
			// Msg intentionally empty — no dedup key is generated.
		}

		ctx := context.Background()
		// Both publishes should go through because dedup is skipped.
		_ = broker.Publish(ctx, "topic.test", msg)
		_ = broker.Publish(ctx, "topic.test", msg)
		if got := callCount.Load(); got != 2 {
			t.Errorf("expected handler called twice for empty-msg messages, got %d", got)
		}
	})
}
