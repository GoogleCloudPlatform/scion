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

package broker

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func newTestBroker() *InProcessBroker {
	return NewInProcessBroker(slog.Default())
}

func TestInProcessBroker_PublishSubscribe(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	var received *messages.StructuredMessage
	var receivedTopic string
	var wg sync.WaitGroup
	wg.Add(1)

	_, err := b.Subscribe("scion.grove.g1.agent.myagent.messages", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		receivedTopic = topic
		received = msg
		wg.Done()
	})
	if err != nil {
		t.Fatal(err)
	}

	msg := messages.NewInstruction("user:alice", "agent:myagent", "hello")
	err = b.Publish(context.Background(), "scion.grove.g1.agent.myagent.messages", msg)
	if err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	if received == nil {
		t.Fatal("expected message to be received")
	}
	if received.Msg != "hello" {
		t.Errorf("expected msg 'hello', got %q", received.Msg)
	}
	if receivedTopic != "scion.grove.g1.agent.myagent.messages" {
		t.Errorf("expected topic 'scion.grove.g1.agent.myagent.messages', got %q", receivedTopic)
	}
}

func TestInProcessBroker_WildcardSubscribe(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	var mu sync.Mutex
	var received []string

	// Subscribe with wildcard — match all agent messages in grove g1
	_, err := b.Subscribe("scion.grove.g1.agent.*.messages", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		received = append(received, msg.Msg)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	msg1 := messages.NewInstruction("user:alice", "agent:a1", "msg1")
	msg2 := messages.NewInstruction("user:alice", "agent:a2", "msg2")

	b.Publish(ctx, "scion.grove.g1.agent.a1.messages", msg1)
	b.Publish(ctx, "scion.grove.g1.agent.a2.messages", msg2)

	// Should NOT match a different grove
	msg3 := messages.NewInstruction("user:alice", "agent:a3", "msg3")
	b.Publish(ctx, "scion.grove.g2.agent.a3.messages", msg3)

	// Wait for delivery
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(received), received)
	}
}

func TestInProcessBroker_GreaterThanWildcard(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	var mu sync.Mutex
	var received []string

	// Subscribe with > wildcard — match everything under grove g1
	_, err := b.Subscribe("scion.grove.g1.>", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		received = append(received, topic)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	b.Publish(ctx, "scion.grove.g1.agent.a1.messages", messages.NewInstruction("u:a", "a:b", "m1"))
	b.Publish(ctx, "scion.grove.g1.broadcast", messages.NewInstruction("u:a", "grove:g1", "m2"))
	b.Publish(ctx, "scion.grove.g2.broadcast", messages.NewInstruction("u:a", "grove:g2", "m3")) // should NOT match

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected 2 messages, got %d: %v", len(received), received)
	}
}

func TestInProcessBroker_BroadcastTopic(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// Two subscribers listening to the grove broadcast topic
	for i := 0; i < 2; i++ {
		_, err := b.Subscribe("scion.grove.g1.broadcast", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
			wg.Done()
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	msg := messages.NewInstruction("agent:lead", "grove:g1", "hello all")
	msg.Broadcasted = true
	b.Publish(context.Background(), "scion.grove.g1.broadcast", msg)

	wg.Wait()
}

// TestInProcessBroker_PropagatesPublisherContext verifies that the context
// passed to Publish is delivered to the subscriber handler. Regression for a
// bug where the dispatcher replaced the real ctx with context.Background(),
// preventing handlers from honoring cancellation or carrying publisher values.
func TestInProcessBroker_PropagatesPublisherContext(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	type ctxKey string
	const key ctxKey = "trace"

	got := make(chan string, 1)
	_, err := b.Subscribe("scion.grove.g1.broadcast", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		v, _ := ctx.Value(key).(string)
		got <- v
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.WithValue(context.Background(), key, "abc123")
	msg := messages.NewInstruction("u:a", "grove:g1", "hi")
	if err := b.Publish(ctx, "scion.grove.g1.broadcast", msg); err != nil {
		t.Fatal(err)
	}

	select {
	case v := <-got:
		if v != "abc123" {
			t.Fatalf("handler got ctx value %q, want %q — publisher ctx was not propagated", v, "abc123")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for handler")
	}
}

func TestInProcessBroker_Unsubscribe(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	callCount := 0
	sub, err := b.Subscribe("scion.grove.g1.broadcast", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		callCount++
	})
	if err != nil {
		t.Fatal(err)
	}

	msg := messages.NewInstruction("u:a", "g:g1", "m1")
	b.Publish(context.Background(), "scion.grove.g1.broadcast", msg)
	time.Sleep(50 * time.Millisecond)

	if callCount != 1 {
		t.Fatalf("expected 1 call before unsubscribe, got %d", callCount)
	}

	sub.Unsubscribe()

	b.Publish(context.Background(), "scion.grove.g1.broadcast", msg)
	time.Sleep(50 * time.Millisecond)

	if callCount != 1 {
		t.Fatalf("expected no additional calls after unsubscribe, got %d", callCount)
	}
}

func TestInProcessBroker_CloseStopsDelivery(t *testing.T) {
	b := newTestBroker()

	callCount := 0
	_, err := b.Subscribe("scion.>", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		callCount++
	})
	if err != nil {
		t.Fatal(err)
	}

	b.Close()

	err = b.Publish(context.Background(), "scion.grove.g1.broadcast",
		messages.NewInstruction("u:a", "g:g1", "after close"))
	if err != ErrBrokerClosed {
		t.Fatalf("expected ErrBrokerClosed, got %v", err)
	}

	_, err = b.Subscribe("scion.>", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {})
	if err != ErrBrokerClosed {
		t.Fatalf("expected ErrBrokerClosed on Subscribe after Close, got %v", err)
	}
}

func TestInProcessBroker_NoMatchNoDelivery(t *testing.T) {
	b := newTestBroker()
	defer b.Close()

	callCount := 0
	_, err := b.Subscribe("scion.grove.g1.agent.specific.messages", func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		callCount++
	})
	if err != nil {
		t.Fatal(err)
	}

	b.Publish(context.Background(), "scion.grove.g1.agent.other.messages",
		messages.NewInstruction("u:a", "a:other", "should not match"))
	time.Sleep(50 * time.Millisecond)

	if callCount != 0 {
		t.Fatalf("expected 0 calls for non-matching topic, got %d", callCount)
	}
}

func TestTopicHelpers(t *testing.T) {
	tests := []struct {
		name     string
		got      string
		expected string
	}{
		{"agent messages", TopicAgentMessages("g1", "myagent"), "scion.grove.g1.agent.myagent.messages"},
		{"grove broadcast", TopicGroveBroadcast("g1"), "scion.grove.g1.broadcast"},
		{"global broadcast", TopicGlobalBroadcast(), "scion.global.broadcast"},
		{"all agent messages", TopicAllAgentMessages("g1"), "scion.grove.g1.agent.*.messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, tt.got)
			}
		})
	}
}

func TestSubjectMatchesPattern(t *testing.T) {
	tests := []struct {
		pattern string
		subject string
		match   bool
	}{
		{"a.b.c", "a.b.c", true},
		{"a.b.c", "a.b.d", false},
		{"a.*.c", "a.b.c", true},
		{"a.*.c", "a.x.c", true},
		{"a.*.c", "a.b.d", false},
		{"a.>", "a.b", true},
		{"a.>", "a.b.c", true},
		{"a.>", "a.b.c.d", true},
		{"a.>", "b.c", false},
		{"scion.grove.*.broadcast", "scion.grove.g1.broadcast", true},
		{"scion.grove.g1.agent.*.messages", "scion.grove.g1.agent.myagent.messages", true},
		{"scion.grove.g1.agent.*.messages", "scion.grove.g2.agent.myagent.messages", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.subject, func(t *testing.T) {
			got := subjectMatchesPattern(tt.pattern, tt.subject)
			if got != tt.match {
				t.Errorf("subjectMatchesPattern(%q, %q) = %v, want %v", tt.pattern, tt.subject, got, tt.match)
			}
		})
	}
}
