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

package eventbus

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// stubEventBus is a minimal EventBus for testing fan-out behavior.
type stubEventBus struct {
	publishFunc   func(ctx context.Context, topic string, msg *messages.StructuredMessage) error
	subscribeFunc func(pattern string, handler EventHandler) (Subscription, error)
	closeFunc     func() error

	mu        sync.Mutex
	published []*messages.StructuredMessage
	closed    bool
}

func newStubEventBus() *stubEventBus {
	s := &stubEventBus{}
	s.publishFunc = func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.published = append(s.published, msg)
		return nil
	}
	s.subscribeFunc = func(_ string, _ EventHandler) (Subscription, error) {
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

func (s *stubEventBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	return s.publishFunc(ctx, topic, msg)
}

func (s *stubEventBus) Subscribe(pattern string, handler EventHandler) (Subscription, error) {
	return s.subscribeFunc(pattern, handler)
}

func (s *stubEventBus) Close() error {
	return s.closeFunc()
}

type stubSubscription struct{}

func (s *stubSubscription) Unsubscribe() error { return nil }

func TestFanOutEventBus_PublishFansOutToAll(t *testing.T) {
	b1 := newStubEventBus()
	b2 := newStubEventBus()
	b3 := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "b1", Bus: b1},
		{Name: "b2", Bus: b2},
		{Name: "b3", Bus: b3},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	if err := fan.Publish(context.Background(), "test.topic", msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, sb := range []*stubEventBus{b1, b2, b3} {
		sb.mu.Lock()
		if len(sb.published) != 1 {
			t.Errorf("expected 1 message, got %d", len(sb.published))
		}
		sb.mu.Unlock()
	}
}

func TestFanOutEventBus_ObserverErrorNotReturned(t *testing.T) {
	critical := newStubEventBus()
	observer := newStubEventBus()
	observer.publishFunc = func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
		return errors.New("observer failed")
	}

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "critical", Bus: critical},
		{Name: "observer", Bus: observer, Observer: true},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	err := fan.Publish(context.Background(), "test.topic", msg)
	if err != nil {
		t.Fatalf("observer error should not be returned, got: %v", err)
	}
}

func TestFanOutEventBus_CriticalErrorReturned(t *testing.T) {
	failing := newStubEventBus()
	failing.publishFunc = func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
		return errors.New("critical failed")
	}
	ok := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "failing", Bus: failing},
		{Name: "ok", Bus: ok},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	err := fan.Publish(context.Background(), "test.topic", msg)
	if err == nil {
		t.Fatal("expected error from critical event bus")
	}
	if !errors.Is(err, err) {
		t.Fatalf("unexpected error: %v", err)
	}

	// The ok event bus should still have received the message.
	ok.mu.Lock()
	if len(ok.published) != 1 {
		t.Errorf("ok event bus should have received message, got %d", len(ok.published))
	}
	ok.mu.Unlock()
}

func TestFanOutEventBus_CloseCallsAllChildren(t *testing.T) {
	b1 := newStubEventBus()
	b2 := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "b1", Bus: b1},
		{Name: "b2", Bus: b2},
	}, slog.Default())

	if err := fan.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for name, sb := range map[string]*stubEventBus{"b1": b1, "b2": b2} {
		sb.mu.Lock()
		if !sb.closed {
			t.Errorf("event bus %s was not closed", name)
		}
		sb.mu.Unlock()
	}
}

func TestFanOutEventBus_ConcurrentPublish(t *testing.T) {
	var started atomic.Int32
	gate := make(chan struct{})

	slow := newStubEventBus()
	slow.publishFunc = func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		started.Add(1)
		<-gate
		slow.mu.Lock()
		defer slow.mu.Unlock()
		slow.published = append(slow.published, msg)
		return nil
	}

	fast := newStubEventBus()
	fast.publishFunc = func(_ context.Context, _ string, msg *messages.StructuredMessage) error {
		started.Add(1)
		<-gate
		fast.mu.Lock()
		defer fast.mu.Unlock()
		fast.published = append(fast.published, msg)
		return nil
	}

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "slow", Bus: slow},
		{Name: "fast", Bus: fast},
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

	for name, sb := range map[string]*stubEventBus{"slow": slow, "fast": fast} {
		sb.mu.Lock()
		if len(sb.published) != 1 {
			t.Errorf("event bus %s: expected 1 message, got %d", name, len(sb.published))
		}
		sb.mu.Unlock()
	}
}

func TestFanOutEventBus_ChannelRouting(t *testing.T) {
	inproc := newStubEventBus()
	telegram := newStubEventBus()
	gchat := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "telegram", Bus: telegram},
		{Name: "gchat", Bus: gchat},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	msg.Channel = "telegram"

	if err := fan.Publish(context.Background(), "test.topic", msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	inproc.mu.Lock()
	if len(inproc.published) != 1 {
		t.Errorf("inprocess bus: expected 1 message, got %d", len(inproc.published))
	}
	inproc.mu.Unlock()

	telegram.mu.Lock()
	if len(telegram.published) != 1 {
		t.Errorf("telegram bus: expected 1 message, got %d", len(telegram.published))
	}
	telegram.mu.Unlock()

	gchat.mu.Lock()
	if len(gchat.published) != 0 {
		t.Errorf("gchat bus: expected 0 messages, got %d", len(gchat.published))
	}
	gchat.mu.Unlock()
}

func TestFanOutEventBus_ChannelRoutingNoChannel(t *testing.T) {
	inproc := newStubEventBus()
	telegram := newStubEventBus()
	gchat := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "telegram", Bus: telegram},
		{Name: "gchat", Bus: gchat},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")

	if err := fan.Publish(context.Background(), "test.topic", msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for name, sb := range map[string]*stubEventBus{"inprocess": inproc, "telegram": telegram, "gchat": gchat} {
		sb.mu.Lock()
		if len(sb.published) != 1 {
			t.Errorf("bus %s: expected 1 message, got %d", name, len(sb.published))
		}
		sb.mu.Unlock()
	}
}

func TestFanOutEventBus_ChannelRoutingUnmatchedChannel(t *testing.T) {
	inproc := newStubEventBus()
	telegram := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "telegram", Bus: telegram},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	msg.Channel = "slack"

	err := fan.Publish(context.Background(), "test.topic", msg)
	if err == nil {
		t.Fatal("expected error for unmatched channel")
	}
	if !strings.Contains(err.Error(), "no broker registered for channel") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Even though the target channel is missing, inproc must still receive
	// the message so it is persisted and streamed via SSE (bug #945 fix).
	inproc.mu.Lock()
	if len(inproc.published) != 1 {
		t.Errorf("inprocess bus should receive message even on unmatched channel, got %d", len(inproc.published))
	}
	inproc.mu.Unlock()
}

func TestFanOutEventBus_ChannelRoutingReservedInprocess(t *testing.T) {
	inproc := newStubEventBus()
	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	msg.Channel = "inprocess"

	err := fan.Publish(context.Background(), "test.topic", msg)
	if err == nil {
		t.Fatal("expected error for reserved inprocess channel")
	}
	if !strings.Contains(err.Error(), "reserved for internal use") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if !errors.Is(err, ErrReservedChannel) {
		t.Fatalf("expected ErrReservedChannel, got %v", err)
	}
	if errors.Is(err, ErrInProcessPublish) {
		t.Fatalf("a reserved channel is not an inprocess publish failure: %v", err)
	}
	inproc.mu.Lock()
	defer inproc.mu.Unlock()
	if len(inproc.published) != 0 {
		t.Fatalf("nothing should be published for a reserved channel, got %d", len(inproc.published))
	}
}

func TestBusChannel_RoutingKey(t *testing.T) {
	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: newStubEventBus()},
		{Name: "chat-app", ChannelID: "gchat", Bus: newStubEventBus()},
		{Name: "slack", Bus: newStubEventBus()},
	}, slog.Default())
	got := map[string]string{}
	for _, ch := range fan.BusChannels() {
		got[ch.Name] = ch.RoutingKey()
	}
	if got["chat-app"] != "gchat" || got["slack"] != "slack" || len(got) != 2 {
		t.Fatalf("unexpected routing keys: %v", got)
	}
}

func TestFanOutEventBus_ChannelRoutingPublishError(t *testing.T) {
	inproc := newStubEventBus()
	failing := newStubEventBus()
	failing.publishFunc = func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
		return errors.New("connection refused")
	}

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "telegram", Bus: failing},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	msg.Channel = "telegram"

	err := fan.Publish(context.Background(), "test.topic", msg)
	if err == nil {
		t.Fatal("expected error from failing channel bus")
	}
	if !strings.Contains(err.Error(), "telegram") {
		t.Fatalf("error should mention channel name, got: %v", err)
	}
}

func TestFanOutEventBus_ChannelRoutingObserverError(t *testing.T) {
	inproc := newStubEventBus()
	observer := newStubEventBus()
	observer.publishFunc = func(_ context.Context, _ string, _ *messages.StructuredMessage) error {
		return errors.New("observer failed")
	}

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "broker-log", Bus: observer, Observer: true},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	msg.Channel = "broker-log"

	err := fan.Publish(context.Background(), "test.topic", msg)
	if err != nil {
		t.Fatalf("observer error should not be returned in channel-targeted path, got: %v", err)
	}
}

func TestFanOutEventBus_ChannelRoutingWithChannelID(t *testing.T) {
	inproc := newStubEventBus()
	chatApp := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "chat-app", Bus: chatApp, ChannelID: "gchat"},
	}, slog.Default())

	msg := messages.NewInstruction("agent:bot", "user:alice", "hello")
	msg.Channel = "gchat"

	if err := fan.Publish(context.Background(), "test.topic", msg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	inproc.mu.Lock()
	if len(inproc.published) != 1 {
		t.Errorf("inprocess bus: expected 1 message, got %d", len(inproc.published))
	}
	inproc.mu.Unlock()

	chatApp.mu.Lock()
	if len(chatApp.published) != 1 {
		t.Errorf("chat-app bus: expected 1 message (routed via ChannelID), got %d", len(chatApp.published))
	}
	chatApp.mu.Unlock()
}

func TestFanOutEventBus_AddSpokeReplaysSubscriptions(t *testing.T) {
	b1 := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "b1", Bus: b1},
	}, slog.Default())

	// Subscribe before adding the new spoke.
	if _, err := fan.Subscribe("events.>", func(_ context.Context, _ string, _ *messages.StructuredMessage) {}); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}
	if _, err := fan.Subscribe("commands.*", func(_ context.Context, _ string, _ *messages.StructuredMessage) {}); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	// Track subscriptions replayed to the new spoke.
	newBus := newStubEventBus()
	var replayed []string
	newBus.subscribeFunc = func(pattern string, _ EventHandler) (Subscription, error) {
		replayed = append(replayed, pattern)
		return &stubSubscription{}, nil
	}

	if err := fan.AddSpoke(NamedEventBus{Name: "new", Bus: newBus}); err != nil {
		t.Fatalf("AddSpoke failed: %v", err)
	}

	if len(replayed) != 2 {
		t.Fatalf("expected 2 replayed subscriptions, got %d", len(replayed))
	}
	if replayed[0] != "events.>" || replayed[1] != "commands.*" {
		t.Errorf("unexpected replayed patterns: %v", replayed)
	}
}

func TestFanOutEventBus_Subscribe(t *testing.T) {
	b1 := newStubEventBus()
	b2 := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "b1", Bus: b1},
		{Name: "b2", Bus: b2},
	}, slog.Default())

	sub, err := fan.Subscribe("test.>", func(_ context.Context, _ string, _ *messages.StructuredMessage) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("unexpected unsubscribe error: %v", err)
	}
}

func TestFanOutEventBus_HasSpoke(t *testing.T) {
	b1 := newStubEventBus()
	b2 := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "discord", Bus: b1},
		{Name: "inprocess", Bus: b2},
	}, slog.Default())

	if !fan.HasSpoke("discord") {
		t.Error("expected HasSpoke('discord') = true")
	}
	if !fan.HasSpoke("inprocess") {
		t.Error("expected HasSpoke('inprocess') = true")
	}
	if fan.HasSpoke("telegram") {
		t.Error("expected HasSpoke('telegram') = false")
	}
	if fan.HasSpoke("") {
		t.Error("expected HasSpoke('') = false")
	}
}

// --- Bug fix tests ---

func TestFanout_PublishNilMessageDoesNotPanic(t *testing.T) {
	inproc := newStubEventBus()
	external := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "external", Bus: external},
	}, slog.Default())

	// A nil message must not panic (bug #946). The publish should fan out
	// to all spokes because msg.Channel is effectively "".
	if err := fan.Publish(context.Background(), "test.topic", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	inproc.mu.Lock()
	if len(inproc.published) != 1 {
		t.Errorf("inprocess bus: expected 1 message, got %d", len(inproc.published))
	}
	inproc.mu.Unlock()

	external.mu.Lock()
	if len(external.published) != 1 {
		t.Errorf("external bus: expected 1 message, got %d", len(external.published))
	}
	external.mu.Unlock()
}

func TestFanout_ChannelMissingStillPersistsToInproc(t *testing.T) {
	inproc := newStubEventBus()

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
	}, slog.Default())

	msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
	msg.Channel = "nonexistent"

	err := fan.Publish(context.Background(), "test.topic", msg)
	if err == nil {
		t.Fatal("expected error for missing channel spoke")
	}
	if !strings.Contains(err.Error(), "no broker registered for channel") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Bug #945: inproc must receive the message for persistence/SSE
	// even when the target channel spoke is not registered.
	inproc.mu.Lock()
	if len(inproc.published) != 1 {
		t.Errorf("inprocess bus should still receive message, got %d", len(inproc.published))
	}
	inproc.mu.Unlock()
}

func TestFanout_SubscribeOnlyInvokesHandlerOnInprocess(t *testing.T) {
	inproc := newStubEventBus()
	external := newStubEventBus()

	// Track what handler each spoke received.
	var inprocHandler, externalHandler EventHandler
	inproc.subscribeFunc = func(pattern string, handler EventHandler) (Subscription, error) {
		inprocHandler = handler
		return &stubSubscription{}, nil
	}
	external.subscribeFunc = func(pattern string, handler EventHandler) (Subscription, error) {
		externalHandler = handler
		return &stubSubscription{}, nil
	}

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "external", Bus: external},
	}, slog.Default())

	realHandler := func(_ context.Context, _ string, _ *messages.StructuredMessage) {}
	if _, err := fan.Subscribe("test.>", realHandler); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	// Bug #944: The inprocess spoke must receive the real handler.
	if inprocHandler == nil {
		t.Error("inprocess spoke should have received a non-nil handler")
	}

	// Bug #944: External spokes must receive nil (pattern-only subscription).
	if externalHandler != nil {
		t.Error("external spoke should have received a nil handler (pattern-only)")
	}
}

func TestFanout_ReplaySubscriptionsPassesNilHandler(t *testing.T) {
	inproc := newStubEventBus()
	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
	}, slog.Default())

	// Subscribe to establish a pattern.
	if _, err := fan.Subscribe("events.>", func(_ context.Context, _ string, _ *messages.StructuredMessage) {}); err != nil {
		t.Fatalf("subscribe failed: %v", err)
	}

	// Add a new external spoke and verify it gets nil handler via replay.
	newBus := newStubEventBus()
	var replayedHandler EventHandler
	newBus.subscribeFunc = func(pattern string, handler EventHandler) (Subscription, error) {
		replayedHandler = handler
		return &stubSubscription{}, nil
	}

	if err := fan.AddSpoke(NamedEventBus{Name: "new-spoke", Bus: newBus}); err != nil {
		t.Fatalf("AddSpoke failed: %v", err)
	}

	// replaySubscriptions should pass nil to external spokes.
	if replayedHandler != nil {
		t.Error("replayed subscription to external spoke should have nil handler")
	}
}

func TestFanOutEventBus_HasSpokeAfterAddRemove(t *testing.T) {
	b1 := newStubEventBus()
	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: "inprocess", Bus: b1},
	}, slog.Default())

	// Initially no discord spoke.
	if fan.HasSpoke("discord") {
		t.Fatal("expected HasSpoke('discord') = false before add")
	}

	// Add a spoke.
	b2 := newStubEventBus()
	if err := fan.AddSpoke(NamedEventBus{Name: "discord", Bus: b2}); err != nil {
		t.Fatalf("AddSpoke failed: %v", err)
	}
	if !fan.HasSpoke("discord") {
		t.Fatal("expected HasSpoke('discord') = true after add")
	}

	// Remove the spoke.
	if err := fan.RemoveSpoke("discord"); err != nil {
		t.Fatalf("RemoveSpoke failed: %v", err)
	}
	if fan.HasSpoke("discord") {
		t.Fatal("expected HasSpoke('discord') = false after remove")
	}
}

// newSaturatedSubscriberInproc builds an InProcessEventBus with a single
// subscriber on pattern whose buffer is already full for topic, so the next
// Publish on topic is guaranteed to be dropped (and, for a user-message
// topic, reported as ErrSubscriberBufferFull). Shared by
// TestInProcessEventBus_UserTopicBufferFullReturnsError,
// TestInProcessEventBus_NonUserTopicBufferFullStaysFireAndForget and
// TestFanOutEventBus_UserTopicBufferFullPropagatesSentinel.
func newSaturatedSubscriberInproc(t *testing.T, pattern, topic string, msg *messages.StructuredMessage) *InProcessEventBus {
	t.Helper()
	b := NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = b.Close() })

	block := make(chan struct{})
	started := make(chan struct{}, 1)
	_, err := b.Subscribe(pattern, func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-block
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(block) })

	// Prime the dispatch goroutine so it is blocked inside the handler,
	// then fill the channel buffer behind it.
	if err := b.Publish(context.Background(), topic, msg); err != nil {
		t.Fatalf("unexpected error priming dispatch goroutine: %v", err)
	}
	<-started

	for i := 0; i < defaultSubscriberBuffer; i++ {
		if err := b.Publish(context.Background(), topic, msg); err != nil {
			t.Fatalf("unexpected error filling buffer (iteration %d): %v", i, err)
		}
	}
	return b
}

// TestFanOutEventBus_UserTopicBufferFullPropagatesSentinel is a regression
// test for ptone/scion#2311: FanOutEventBus must preserve
// errors.Is(err, ErrSubscriberBufferFull) from a saturated InProcessEventBus
// spoke, on both the plain fan-out path (no msg.Channel, errors.Join over
// unwrapped errors) and the channel-routing path (msg.Channel set, the
// inproc error is %w-wrapped before errors.Join). A future regression to
// %v-wrapping or to dropping the join on either path would otherwise let the
// sentinel silently stop propagating through the production bus topology.
func TestFanOutEventBus_UserTopicBufferFullPropagatesSentinel(t *testing.T) {
	const pattern = "scion.project.g1.user.*.messages"
	const topic = "scion.project.g1.user.alice.messages"

	t.Run("without channel", func(t *testing.T) {
		msg := messages.NewInstruction("agent:a", "user:alice", "hi")
		inproc := newSaturatedSubscriberInproc(t, pattern, topic, msg)
		fan := NewFanOutEventBus([]NamedEventBus{
			{Name: InProcessBusName, Bus: inproc},
		}, slog.Default())

		if err := fan.Publish(context.Background(), topic, msg); !errors.Is(err, ErrSubscriberBufferFull) {
			t.Fatalf("expected ErrSubscriberBufferFull, got %v", err)
		}
	})

	t.Run("with channel", func(t *testing.T) {
		msg := messages.NewInstruction("agent:a", "user:alice", "hi")
		inproc := newSaturatedSubscriberInproc(t, pattern, topic, msg)
		fan := NewFanOutEventBus([]NamedEventBus{
			{Name: InProcessBusName, Bus: inproc},
		}, slog.Default())

		msg.Channel = "web"
		if err := fan.Publish(context.Background(), topic, msg); !errors.Is(err, ErrSubscriberBufferFull) {
			t.Fatalf("expected ErrSubscriberBufferFull, got %v", err)
		}
	})
}

// ErrInProcessPublish lets callers tell "the hub's own subscribers did not
// get the message" apart from "an external spoke failed" (ptone/scion#1906).
func TestFanOutEventBus_InProcessFailureWrappedInSentinel(t *testing.T) {
	failing := func() *stubEventBus {
		b := newStubEventBus()
		b.publishFunc = func(context.Context, string, *messages.StructuredMessage) error {
			return errors.New("spoke down")
		}
		return b
	}
	closedInproc := func() *stubEventBus {
		b := newStubEventBus()
		b.publishFunc = func(context.Context, string, *messages.StructuredMessage) error {
			return ErrEventBusClosed
		}
		return b
	}

	for _, channel := range []string{"", "telegram"} {
		t.Run("channel="+channel, func(t *testing.T) {
			newMsg := func() *messages.StructuredMessage {
				m := messages.NewInstruction("agent:a", "user:alice", "hi")
				m.Channel = channel
				return m
			}

			// A failing non-observer plugin spoke: error returned, but not
			// attributed to the inprocess spoke.
			fan := NewFanOutEventBus([]NamedEventBus{
				{Name: InProcessBusName, Bus: newStubEventBus()},
				{Name: "telegram", Bus: failing()},
			}, slog.Default())
			err := fan.Publish(context.Background(), "t", newMsg())
			if err == nil {
				t.Fatal("expected the plugin spoke error to be returned")
			}
			if errors.Is(err, ErrInProcessPublish) {
				t.Fatalf("plugin spoke failure must not match ErrInProcessPublish: %v", err)
			}

			// A failing inprocess spoke: wrapped, cause preserved.
			fan = NewFanOutEventBus([]NamedEventBus{
				{Name: InProcessBusName, Bus: closedInproc()},
				{Name: "telegram", Bus: newStubEventBus()},
			}, slog.Default())
			err = fan.Publish(context.Background(), "t", newMsg())
			if !errors.Is(err, ErrInProcessPublish) || !errors.Is(err, ErrEventBusClosed) {
				t.Fatalf("expected ErrInProcessPublish wrapping ErrEventBusClosed, got %v", err)
			}
		})
	}
}

func TestFanOutEventBus_SubscribeNilHandler(t *testing.T) {
	inproc := NewInProcessEventBus(slog.Default())
	external := newStubEventBus()
	var externalSubs int
	external.subscribeFunc = func(_ string, _ EventHandler) (Subscription, error) {
		externalSubs++
		return &stubSubscription{}, nil
	}

	fan := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: inproc},
		{Name: "external", Bus: external},
	}, slog.Default())
	defer func() { _ = fan.Close() }()

	sub, err := fan.Subscribe("test.>", nil)
	if !errors.Is(err, ErrNilHandler) {
		t.Fatalf("Subscribe(nil) error = %v, want ErrNilHandler", err)
	}
	if sub != nil {
		t.Fatalf("Subscribe(nil) returned a non-nil subscription")
	}
	if externalSubs != 0 {
		t.Errorf("external spoke subscribed %d times, want 0", externalSubs)
	}

	// The rejected pattern must not be replayed onto spokes added later.
	added := newStubEventBus()
	var replayed []string
	added.subscribeFunc = func(pattern string, _ EventHandler) (Subscription, error) {
		replayed = append(replayed, pattern)
		return &stubSubscription{}, nil
	}
	if err := fan.AddSpoke(NamedEventBus{Name: "added", Bus: added}); err != nil {
		t.Fatalf("AddSpoke: %v", err)
	}
	if len(replayed) != 0 {
		t.Errorf("rejected pattern replayed onto new spoke: %v", replayed)
	}
}

// blockingBus is an EventBus whose Publish blocks until release is closed
// or its ctx ends, and records the ctx it was called with.
type blockingBus struct {
	release chan struct{}
	started chan struct{}
	calls   atomic.Int32
	mu      sync.Mutex
	ctxs    []context.Context
	ended   chan error
}

func newBlockingBus() *blockingBus {
	return &blockingBus{
		release: make(chan struct{}),
		started: make(chan struct{}, 16),
		ended:   make(chan error, 16),
	}
}

func (b *blockingBus) Publish(ctx context.Context, _ string, _ *messages.StructuredMessage) error {
	b.calls.Add(1)
	b.mu.Lock()
	b.ctxs = append(b.ctxs, ctx)
	b.mu.Unlock()
	b.started <- struct{}{}
	var err error
	select {
	case <-b.release:
	case <-ctx.Done():
		err = ctx.Err()
	}
	b.ended <- err
	return err
}

func (b *blockingBus) Subscribe(string, EventHandler) (Subscription, error) {
	return &stubSubscription{}, nil
}

func (b *blockingBus) Close() error { return nil }

// inProcessBlockingBus is a blockingBus marked in-process.
type inProcessBlockingBus struct{ *blockingBus }

func (inProcessBlockingBus) InProcessSpoke() {}

func newBoundedFanOut(buses []NamedEventBus) *FanOutEventBus {
	f := NewFanOutEventBus(buses, slog.Default())
	f.ObserverWait = 50 * time.Millisecond
	f.ObserverCallTimeout = 2 * time.Second
	f.SpokePublishTimeout = 5 * time.Second
	return f
}

func TestFanOut_HungObserverBoundedByTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel string
	}{
		{name: "broadcast"},
		{name: "channel", channel: "relay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inproc := newStubEventBus()
			hung := newBlockingBus()
			defer close(hung.release)
			f := newBoundedFanOut([]NamedEventBus{
				{Name: InProcessBusName, Bus: inproc},
				{Name: "relay", Bus: hung, Observer: true},
			})
			msg := messages.NewInstruction("user:alice", "agent:bot", "hello")
			msg.Channel = tc.channel

			start := time.Now()
			if err := f.Publish(context.Background(), "test.topic", msg); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("Publish took %v, want about the 50ms observer wait", d)
			}
			inproc.mu.Lock()
			n := len(inproc.published)
			inproc.mu.Unlock()
			if n != 1 {
				t.Fatalf("inprocess got %d messages, want 1", n)
			}
		})
	}
}

func TestFanOut_NonObserverStillAwaited(t *testing.T) {
	slow := newBlockingBus()
	f := newBoundedFanOut([]NamedEventBus{{Name: "relay", Bus: slow}})
	done := make(chan error, 1)
	go func() {
		done <- f.Publish(context.Background(), "test.topic", messages.NewInstruction("user:alice", "agent:bot", "hi"))
	}()
	<-slow.started
	select {
	case err := <-done:
		t.Fatalf("Publish returned %v before the non-observer spoke finished", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(slow.release)
	if err := <-done; err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestFanOut_ObserverOverrunNotCancelledByCaller(t *testing.T) {
	hung := newBlockingBus()
	f := newBoundedFanOut([]NamedEventBus{{Name: "relay", Bus: hung, Observer: true}})
	f.ObserverCallTimeout = 300 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	if err := f.Publish(ctx, "test.topic", messages.NewInstruction("user:alice", "agent:bot", "hi")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	cancel()
	select {
	case err := <-hung.ended:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("observer call ended with %v, want its own deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observer call did not end at its own deadline")
	}
	hung.mu.Lock()
	callCtx := hung.ctxs[0]
	hung.mu.Unlock()
	if _, ok := callCtx.Deadline(); !ok {
		t.Fatal("observer call ctx has no deadline")
	}
}

func TestFanOut_ObserverSingleOverrunSkipsAndCounts(t *testing.T) {
	hung := newBlockingBus()
	f := newBoundedFanOut([]NamedEventBus{{Name: "relay", Bus: hung, Observer: true}})
	msg := messages.NewInstruction("user:alice", "agent:bot", "hi")

	if err := f.Publish(context.Background(), "test.topic", msg); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := f.Publish(context.Background(), "test.topic", msg); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	if got := hung.calls.Load(); got != 1 {
		t.Fatalf("observer called %d times while overrunning, want 1", got)
	}
	if got := f.ObserverSkipped("relay"); got != 3 {
		t.Fatalf("ObserverSkipped = %d, want 3", got)
	}

	// Once the overrun returns, the spoke is called again.
	close(hung.release)
	if err := <-hung.ended; err != nil {
		t.Fatalf("overrun call ended with %v, want nil after release", err)
	}
	waitOverruns(t, f, "relay", 0)
	if err := f.Publish(context.Background(), "test.topic", msg); err != nil {
		t.Fatalf("Publish after overrun: %v", err)
	}
	if got := hung.calls.Load(); got != 2 {
		t.Fatalf("observer called %d times after the overrun ended, want 2", got)
	}
}

func TestFanOut_ConcurrentInTimeObserverCallsNotSkipped(t *testing.T) {
	obs := newStubEventBus()
	f := newBoundedFanOut([]NamedEventBus{{Name: "relay", Bus: obs, Observer: true}})
	f.ObserverWait = 2 * time.Second
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Publish(context.Background(), "test.topic", messages.NewInstruction("user:alice", "agent:bot", "hi"))
		}()
	}
	wg.Wait()
	obs.mu.Lock()
	n := len(obs.published)
	obs.mu.Unlock()
	if n != 20 || f.ObserverSkipped("relay") != 0 {
		t.Fatalf("published %d, skipped %d; want 20 and 0", n, f.ObserverSkipped("relay"))
	}
}

func TestFanOut_InProcessSpokesStaySynchronous(t *testing.T) {
	slow := newBlockingBus()
	f := newBoundedFanOut([]NamedEventBus{
		{Name: "web", Bus: inProcessBlockingBus{slow}, Observer: true},
	})
	done := make(chan error, 1)
	go func() {
		done <- f.Publish(context.Background(), "test.topic", messages.NewInstruction("user:alice", "agent:bot", "hi"))
	}()
	<-slow.started
	select {
	case err := <-done:
		t.Fatalf("Publish returned %v before the in-process observer finished", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(slow.release)
	if err := <-done; err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestFanOut_DefaultCapWhenNoDeadline(t *testing.T) {
	hung := newBlockingBus()
	f := newBoundedFanOut([]NamedEventBus{{Name: InProcessBusName, Bus: hung}})
	f.SpokePublishTimeout = 100 * time.Millisecond
	err := f.Publish(context.Background(), "test.topic", messages.NewInstruction("user:alice", "agent:bot", "hi"))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrInProcessPublish) {
		t.Fatalf("Publish = %v, want the default-cap deadline wrapped in ErrInProcessPublish", err)
	}

	// A caller deadline is used as is.
	hung2 := newBlockingBus()
	close(hung2.release)
	f2 := newBoundedFanOut([]NamedEventBus{{Name: InProcessBusName, Bus: hung2}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := f2.Publish(ctx, "test.topic", messages.NewInstruction("user:alice", "agent:bot", "hi")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want, _ := ctx.Deadline()
	got, _ := hung2.ctxs[0].Deadline()
	if !got.Equal(want) {
		t.Fatalf("spoke deadline %v, want the caller's %v", got, want)
	}
}

func TestFanOut_ReplaceSpokeReturnsReplayError(t *testing.T) {
	f := NewFanOutEventBus([]NamedEventBus{
		{Name: InProcessBusName, Bus: newStubEventBus()},
		{Name: "relay", Bus: newStubEventBus()},
	}, slog.Default())
	if _, err := f.Subscribe("agent.>", func(context.Context, string, *messages.StructuredMessage) {}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	failing := newStubEventBus()
	failing.subscribeFunc = func(string, EventHandler) (Subscription, error) {
		return nil, errors.New("session lost")
	}
	err := f.ReplaceSpoke("relay", NamedEventBus{Name: "relay", Bus: failing})
	if err == nil || errors.Is(err, ErrSpokeNotFound) || !strings.Contains(err.Error(), "session lost") {
		t.Fatalf("ReplaceSpoke = %v, want the replay error", err)
	}
	if !f.HasSpoke("relay") {
		t.Fatal("spoke not replaced")
	}

	err = f.ReplaceSpoke("missing", NamedEventBus{Name: "missing", Bus: newStubEventBus()})
	if !errors.Is(err, ErrSpokeNotFound) {
		t.Fatalf("ReplaceSpoke(missing) = %v, want ErrSpokeNotFound", err)
	}

	err = f.AddSpoke(NamedEventBus{Name: "other", Bus: failing})
	if err == nil || !f.HasSpoke("other") {
		t.Fatalf("AddSpoke = %v, HasSpoke = %v; want replay error and the spoke added", err, f.HasSpoke("other"))
	}
}

// tokenBus is an EventBus whose Publish blocks until it takes a token.
type tokenBus struct {
	tokens  chan struct{}
	started chan struct{}
	ended   chan struct{}
	calls   atomic.Int32
}

func newTokenBus() *tokenBus {
	return &tokenBus{tokens: make(chan struct{}, 64), started: make(chan struct{}, 64), ended: make(chan struct{}, 64)}
}

func (b *tokenBus) Publish(ctx context.Context, _ string, _ *messages.StructuredMessage) error {
	b.calls.Add(1)
	b.started <- struct{}{}
	defer func() { b.ended <- struct{}{} }()
	select {
	case <-b.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *tokenBus) Subscribe(string, EventHandler) (Subscription, error) {
	return &stubSubscription{}, nil
}

func (b *tokenBus) Close() error { return nil }

func TestFanOut_ConcurrentOverrunsSkipUntilAllReturn(t *testing.T) {
	const n = 10
	hung := newTokenBus()
	f := newBoundedFanOut([]NamedEventBus{{Name: "relay", Bus: hung, Observer: true}})
	f.ObserverWait = 500 * time.Millisecond
	f.ObserverCallTimeout = time.Minute // the overruns end only when released
	msg := messages.NewInstruction("user:alice", "agent:bot", "hi")

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Publish(context.Background(), "test.topic", msg)
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-hung.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d observer calls started", i, n)
		}
	}
	wg.Wait() // every Publish stopped waiting: n overruns

	publish := func() {
		if err := f.Publish(context.Background(), "test.topic", msg); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	publish()
	if got := hung.calls.Load(); got != n {
		t.Fatalf("calls = %d after the overruns started, want %d", got, n)
	}

	// The skip lasts until every overrun has returned, not just one.
	for i := 0; i < n-1; i++ {
		hung.tokens <- struct{}{}
		<-hung.ended
	}
	waitOverruns(t, f, "relay", 1)
	publish()
	if got := hung.calls.Load(); got != n {
		t.Fatalf("calls = %d with one overrun left, want %d", got, n)
	}
	if got := f.ObserverSkipped("relay"); got != 2 {
		t.Fatalf("ObserverSkipped = %d, want 2", got)
	}

	hung.tokens <- struct{}{}
	<-hung.ended
	waitOverruns(t, f, "relay", 0)
	hung.tokens <- struct{}{} // for the next call
	publish()
	if got := hung.calls.Load(); got != n+1 {
		t.Fatalf("calls = %d after all overruns returned, want %d", got, n+1)
	}
}

// recordingHandler is a slog.Handler that keeps the records it handles.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func TestFanOut_DefaultCapLogsError(t *testing.T) {
	hung := newBlockingBus()
	defer close(hung.release)
	rec := &recordingHandler{}
	f := NewFanOutEventBus([]NamedEventBus{{Name: "relay", Bus: hung}}, slog.New(rec))
	f.SpokePublishTimeout = 50 * time.Millisecond
	_ = f.Publish(context.Background(), "test.topic", messages.NewInstruction("user:alice", "agent:bot", "hi"))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	found := 0
	for _, r := range rec.records {
		if r.Level != slog.LevelError || r.Message != "spoke publish hit the default timeout" {
			continue
		}
		attrs := map[string]string{}
		r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
		if attrs["bus"] == "relay" && attrs["topic"] == "test.topic" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("got %d default-timeout ERROR records with bus and topic, want 1", found)
	}
}

// waitOverruns waits until the named spoke's overrun count is want. A
// bus's Publish returns before the fan-out goroutine records the end of an
// overrun, so tests wait for the count rather than for Publish.
func waitOverruns(t *testing.T, f *FanOutEventBus, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.RLock()
		var st *spokeState
		for _, nb := range f.buses {
			if nb.Name == name {
				st = nb.state
			}
		}
		f.mu.RUnlock()
		if st != nil {
			st.mu.Lock()
			got := st.overruns
			st.mu.Unlock()
			if got == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("spoke %q overruns did not reach %d", name, want)
}
