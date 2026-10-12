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

//go:build !no_sqlite

package hub

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/stretchr/testify/require"
)

// liveSubCountingBus wraps an EventBus and counts its live subscriptions
// per pattern.
type liveSubCountingBus struct {
	eventbus.EventBus
	mu   sync.Mutex
	live map[string]int
}

func (b *liveSubCountingBus) Subscribe(pattern string, h eventbus.EventHandler) (eventbus.Subscription, error) {
	sub, err := b.EventBus.Subscribe(pattern, h)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.live[pattern]++
	b.mu.Unlock()
	return &liveSubCountingSub{Subscription: sub, bus: b, pattern: pattern}, nil
}

func (b *liveSubCountingBus) count(pattern string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live[pattern]
}

type liveSubCountingSub struct {
	eventbus.Subscription
	bus     *liveSubCountingBus
	pattern string
	once    sync.Once
}

func (s *liveSubCountingSub) Unsubscribe() error {
	s.once.Do(func() {
		s.bus.mu.Lock()
		s.bus.live[s.pattern]--
		s.bus.mu.Unlock()
	})
	return s.Subscription.Unsubscribe()
}

// TestMessageBrokerProxy_StopRemovesGlobalBroadcastSubscription pins that Stop
// unsubscribes the global broadcast subscription Start registers. It used to
// be dropped, leaving the bus's dispatch goroutine — and through its handler
// the proxy and its store — alive after Stop; pkg/hub tests leaked one per
// proxy and the leftovers helped trip the package memory guard.
func TestMessageBrokerProxy_StopRemovesGlobalBroadcastSubscription(t *testing.T) {
	s := newBrokerTestStore(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)

	inproc := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inproc.Close() })
	bus := &liveSubCountingBus{EventBus: inproc, live: map[string]int{}}

	proxy := NewMessageBrokerProxy(bus, s, events,
		func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	proxy.Start()

	topic := eventbus.TopicGlobalBroadcast()
	require.Equal(t, 1, bus.count(topic), "Start subscribes to global broadcasts")

	proxy.Stop()
	require.Equal(t, 0, bus.count(topic), "Stop removes the global broadcast subscription")
}
