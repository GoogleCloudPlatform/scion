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
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// stubBroker is a minimal MessageBroker for testing fan-out behavior.
type stubBroker struct {
	publishFunc   func(ctx context.Context, topic string, msg *messages.StructuredMessage) error
	subscribeFunc func(pattern string, handler MessageHandler) (Subscription, error)
	closeFunc     func() error

	mu        sync.Mutex
	published []*messages.StructuredMessage
	closed    bool
}

func newStubBroker() *stubBroker {
	s := &stubBroker{}
	s.publishFunc = func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.published = append(s.published, msg)
		return nil
	}
	s.subscribeFunc = func(_ string, _ MessageHandler) (Subscription, error) {
		return &stubSubscription{}, nil
	}
	s.closeFunc = func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		return nil
	}
	return s
}

func (s *stubBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	return s.publishFunc(ctx, topic, msg)
}

func (s *stubBroker) Subscribe(pattern string, handler MessageHandler) (Subscription, error) {
	return s.subscribeFunc(pattern, handler)
}

func (s *stubBroker) Close() error {
	return s.closeFunc()
}

type stubSubscription struct{}

func (s *stubSubscription) Unsubscribe() error { return nil }

func TestFanOutBroker_PublishFansOutToAll(t *testing.T) {
	b1 := newStubBroker()
	b2 := newStubBroker()
	b3 := newStubBroker()

	fan := NewFanOutBroker([]NamedBroker{
		{Name: "b1", Broker: b1},
		{Name: "b2", Broker: b2},
		{Name: "b3", Broker: b3},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	if err := fan.Publish(context.Background(), "test.topic", msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, sb := range []*stubBroker{b1, b2, b3} {
		sb.mu.Lock()
		if len(sb.published) != 1 {
			t.Errorf("expected 1 message, got %d", len(sb.published))
		}
		sb.mu.Unlock()
	}
}

func TestFanOutBroker_ObserverErrorNotReturned(t *testing.T) {
	critical := newStubBroker()
	observer := newStubBroker()
	observer.publishFunc = func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
		return errors.New("observer failed")
	}

	fan := NewFanOutBroker([]NamedBroker{
		{Name: "critical", Broker: critical},
		{Name: "observer", Broker: observer, Observer: true},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	err := fan.Publish(context.Background(), "test.topic", msg)
	if err != nil {
		t.Fatalf("observer error should not be returned, got: %v", err)
	}
}

func TestFanOutBroker_CriticalErrorReturned(t *testing.T) {
	failing := newStubBroker()
	failing.publishFunc = func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
		return errors.New("critical failed")
	}
	ok := newStubBroker()

	fan := NewFanOutBroker([]NamedBroker{
		{Name: "failing", Broker: failing},
		{Name: "ok", Broker: ok},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	err := fan.Publish(context.Background(), "test.topic", msg)
	if err == nil {
		t.Fatal("expected error from critical broker")
	}
	if !errors.Is(err, err) {
		t.Fatalf("unexpected error: %v", err)
	}

	// The ok broker should still have received the message.
	ok.mu.Lock()
	if len(ok.published) != 1 {
		t.Errorf("ok broker should have received message, got %d", len(ok.published))
	}
	ok.mu.Unlock()
}

func TestFanOutBroker_CloseCallsAllChildren(t *testing.T) {
	b1 := newStubBroker()
	b2 := newStubBroker()

	fan := NewFanOutBroker([]NamedBroker{
		{Name: "b1", Broker: b1},
		{Name: "b2", Broker: b2},
	}, slog.Default())

	if err := fan.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for name, sb := range map[string]*stubBroker{"b1": b1, "b2": b2} {
		sb.mu.Lock()
		if !sb.closed {
			t.Errorf("broker %s was not closed", name)
		}
		sb.mu.Unlock()
	}
}

func TestFanOutBroker_ConcurrentPublish(t *testing.T) {
	var started atomic.Int32
	gate := make(chan struct{})

	slow := newStubBroker()
	slow.publishFunc = func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		started.Add(1)
		<-gate
		slow.mu.Lock()
		defer slow.mu.Unlock()
		slow.published = append(slow.published, msg)
		return nil
	}

	fast := newStubBroker()
	fast.publishFunc = func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		started.Add(1)
		<-gate
		fast.mu.Lock()
		defer fast.mu.Unlock()
		fast.published = append(fast.published, msg)
		return nil
	}

	fan := NewFanOutBroker([]NamedBroker{
		{Name: "slow", Broker: slow},
		{Name: "fast", Broker: fast},
	}, slog.Default())

	done := make(chan error, 1)
	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	go func() {
		done <- fan.Publish(context.Background(), "test.topic", msg)
	}()

	// Wait for both goroutines to be running concurrently.
	deadline := time.After(2 * time.Second)
	for started.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for concurrent publish")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	close(gate)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for publish to complete")
	}

	for name, sb := range map[string]*stubBroker{"slow": slow, "fast": fast} {
		sb.mu.Lock()
		if len(sb.published) != 1 {
			t.Errorf("broker %s: expected 1 message, got %d", name, len(sb.published))
		}
		sb.mu.Unlock()
	}
}

func TestFanOutBroker_Subscribe(t *testing.T) {
	b1 := newStubBroker()
	b2 := newStubBroker()

	fan := NewFanOutBroker([]NamedBroker{
		{Name: "b1", Broker: b1},
		{Name: "b2", Broker: b2},
	}, slog.Default())

	sub, err := fan.Subscribe("test.>", func(_ context.Context, _ string, _ *messages.StructuredMessage) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("unexpected unsubscribe error: %v", err)
	}
}
