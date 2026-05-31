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
	"log/slog"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const (
	// defaultSubscriberBuffer is the channel buffer size for each subscriber.
	defaultSubscriberBuffer = 64
)

// subscriber holds a handler function and its dispatch goroutine channel.
type subscriber struct {
	pattern string
	handler EventHandler
	ch      chan publishedMessage
	done    chan struct{}
}

// publishedMessage pairs a topic with its message for channel delivery.
// The publisher's context travels with the message so that subscriber
// handlers can honor cancellation and deadlines from the publish side.
type publishedMessage struct {
	ctx   context.Context
	topic string
	msg   *messages.StructuredMessage
}

// inProcessSubscription implements Subscription for the InProcessEventBus.
type inProcessSubscription struct {
	bus *InProcessEventBus
	sub *subscriber
}

func (s *inProcessSubscription) Unsubscribe() error {
	s.bus.unsubscribe(s.sub)
	return nil
}

// InProcessEventBus is an in-process event bus that routes messages using
// Go channels with NATS-style subject pattern matching. Suitable for single-node
// deployments with no external dependencies.
type InProcessEventBus struct {
	mu          sync.RWMutex
	subscribers []*subscriber
	closed      bool
	log         *slog.Logger
}

// NewInProcessEventBus creates a new in-process event bus.
func NewInProcessEventBus(log *slog.Logger) *InProcessEventBus {
	return &InProcessEventBus{
		log: log,
	}
}

// Publish sends a message to all subscribers whose patterns match the topic.
// Publishing is non-blocking: messages are dropped if a subscriber's buffer is full.
func (b *InProcessEventBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return ErrEventBusClosed
	}

	pm := publishedMessage{ctx: ctx, topic: topic, msg: msg}

	for _, sub := range b.subscribers {
		if subjectMatchesPattern(sub.pattern, topic) {
			select {
			case sub.ch <- pm:
			default:
				b.log.Warn("Message dropped: subscriber buffer full",
					"pattern", sub.pattern, "topic", topic)
			}
		}
	}

	return nil
}

// Subscribe registers a handler for messages matching the given pattern.
// Each subscriber gets a dedicated goroutine for dispatch to avoid blocking the publisher.
func (b *InProcessEventBus) Subscribe(pattern string, handler EventHandler) (Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, ErrEventBusClosed
	}

	sub := &subscriber{
		pattern: pattern,
		handler: handler,
		ch:      make(chan publishedMessage, defaultSubscriberBuffer),
		done:    make(chan struct{}),
	}

	// Start a dispatch goroutine for this subscriber. The publisher's context
	// rides along in pm.ctx so handlers can honor cancellation/deadlines.
	go func() {
		defer close(sub.done)
		for pm := range sub.ch {
			ctx := pm.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			sub.handler(ctx, pm.topic, pm.msg)
		}
	}()

	b.subscribers = append(b.subscribers, sub)
	b.log.Debug("Subscription registered", "pattern", pattern)

	return &inProcessSubscription{bus: b, sub: sub}, nil
}

// Close shuts down the event bus and all subscriber goroutines.
func (b *InProcessEventBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}
	b.closed = true

	// Close all subscriber channels to signal their goroutines to exit
	for _, sub := range b.subscribers {
		close(sub.ch)
	}

	// Wait for all dispatch goroutines to finish
	for _, sub := range b.subscribers {
		<-sub.done
	}

	b.subscribers = nil
	b.log.Info("In-process event bus closed")
	return nil
}

// unsubscribe removes a subscriber and shuts down its dispatch goroutine.
// The write lock is released before waiting for the goroutine to finish
// so that slow handlers do not block Publish or Subscribe callers.
func (b *InProcessEventBus) unsubscribe(target *subscriber) {
	b.mu.Lock()
	found := false
	for i, sub := range b.subscribers {
		if sub == target {
			b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
			found = true
			break
		}
	}
	b.mu.Unlock()

	if found {
		close(target.ch)
		<-target.done
		b.log.Debug("Subscription removed", "pattern", target.pattern)
	}
}

// subjectMatchesPattern checks if a subject matches a NATS-style pattern.
// '*' matches exactly one token, '>' matches one or more remaining tokens.
// Tokens are dot-separated.
func subjectMatchesPattern(pattern, subject string) bool {
	patternParts := strings.Split(pattern, ".")
	subjectParts := strings.Split(subject, ".")

	for i, pp := range patternParts {
		if pp == ">" {
			return i < len(subjectParts)
		}
		if i >= len(subjectParts) {
			return false
		}
		if pp == "*" {
			continue
		}
		if pp != subjectParts[i] {
			return false
		}
	}

	return len(patternParts) == len(subjectParts)
}
