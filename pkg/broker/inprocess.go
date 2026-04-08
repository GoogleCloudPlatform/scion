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
	handler MessageHandler
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

// inProcessSubscription implements Subscription for the InProcessBroker.
type inProcessSubscription struct {
	broker *InProcessBroker
	sub    *subscriber
}

func (s *inProcessSubscription) Unsubscribe() error {
	s.broker.unsubscribe(s.sub)
	return nil
}

// InProcessBroker is an in-process message broker that routes messages using
// Go channels with NATS-style subject pattern matching. Suitable for single-node
// deployments with no external dependencies.
type InProcessBroker struct {
	mu          sync.RWMutex
	subscribers []*subscriber
	closed      bool
	log         *slog.Logger
}

// NewInProcessBroker creates a new in-process message broker.
func NewInProcessBroker(log *slog.Logger) *InProcessBroker {
	return &InProcessBroker{
		log: log,
	}
}

// Publish sends a message to all subscribers whose patterns match the topic.
// Publishing is non-blocking: messages are dropped if a subscriber's buffer is full.
func (b *InProcessBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return ErrBrokerClosed
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
func (b *InProcessBroker) Subscribe(pattern string, handler MessageHandler) (Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, ErrBrokerClosed
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

	return &inProcessSubscription{broker: b, sub: sub}, nil
}

// Close shuts down the broker and all subscriber goroutines.
func (b *InProcessBroker) Close() error {
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
	b.log.Info("In-process message broker closed")
	return nil
}

// unsubscribe removes a subscriber and shuts down its dispatch goroutine.
func (b *InProcessBroker) unsubscribe(target *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, sub := range b.subscribers {
		if sub == target {
			b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
			close(sub.ch)
			<-sub.done
			b.log.Debug("Subscription removed", "pattern", sub.pattern)
			return
		}
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
