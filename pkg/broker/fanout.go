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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// NamedBroker pairs a MessageBroker with a name and an observer flag.
// Observer brokers are fire-and-forget: publish errors are logged but
// not returned to the caller.
type NamedBroker struct {
	Name     string
	Broker   MessageBroker
	Observer bool
}

// FanOutBroker implements MessageBroker by delegating to N child brokers.
// Publish fans out concurrently. Subscribe and Close delegate to all children.
type FanOutBroker struct {
	brokers []NamedBroker
	log     *slog.Logger
}

// NewFanOutBroker creates a FanOutBroker that delegates to the given children.
func NewFanOutBroker(brokers []NamedBroker, log *slog.Logger) *FanOutBroker {
	return &FanOutBroker{
		brokers: brokers,
		log:     log,
	}
}

// Publish fans out the message to all child brokers concurrently.
// Observer broker errors are logged but not returned.
// Critical (non-observer) broker errors are aggregated and returned.
func (f *FanOutBroker) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	var wg sync.WaitGroup
	errs := make([]error, len(f.brokers))

	for i, nb := range f.brokers {
		wg.Add(1)
		go func(idx int, b NamedBroker) {
			defer wg.Done()
			if err := b.Broker.Publish(ctx, topic, msg); err != nil {
				f.log.Error("fan-out publish failed",
					"broker", b.Name, "topic", topic, "error", err)
				if !b.Observer {
					errs[idx] = err
				}
			}
		}(i, nb)
	}

	wg.Wait()
	return errors.Join(errs...)
}

// Subscribe delegates to all child brokers.
func (f *FanOutBroker) Subscribe(pattern string, handler MessageHandler) (Subscription, error) {
	subs := make([]Subscription, 0, len(f.brokers))
	for _, nb := range f.brokers {
		sub, err := nb.Broker.Subscribe(pattern, handler)
		if err != nil {
			f.log.Error("fan-out subscribe failed",
				"broker", nb.Name, "pattern", pattern, "error", err)
			for _, s := range subs {
				_ = s.Unsubscribe()
			}
			return nil, err
		}
		subs = append(subs, sub)
	}
	return &fanOutSubscription{subs: subs}, nil
}

// Close shuts down all child brokers and returns an aggregate error.
func (f *FanOutBroker) Close() error {
	var errs []error
	for _, nb := range f.brokers {
		if err := nb.Broker.Close(); err != nil {
			f.log.Error("fan-out close failed", "broker", nb.Name, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fanOutSubscription aggregates subscriptions from all child brokers.
type fanOutSubscription struct {
	subs []Subscription
}

func (s *fanOutSubscription) Unsubscribe() error {
	var errs []error
	for _, sub := range s.subs {
		if err := sub.Unsubscribe(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
