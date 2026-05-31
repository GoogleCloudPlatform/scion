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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// NamedEventBus pairs an EventBus with a name and an observer flag.
// Observer event buses are fire-and-forget: publish errors are logged but
// not returned to the caller.
type NamedEventBus struct {
	Name     string
	Bus      EventBus
	Observer bool
}

// FanOutEventBus implements EventBus by delegating to N child event buses.
// Publish fans out concurrently. Subscribe and Close delegate to all children.
type FanOutEventBus struct {
	buses []NamedEventBus
	log   *slog.Logger
}

// NewFanOutEventBus creates a FanOutEventBus that delegates to the given children.
func NewFanOutEventBus(buses []NamedEventBus, log *slog.Logger) *FanOutEventBus {
	return &FanOutEventBus{
		buses: buses,
		log:   log,
	}
}

// Publish fans out the message to all child event buses concurrently.
// Observer event bus errors are logged but not returned.
// Critical (non-observer) event bus errors are aggregated and returned.
func (f *FanOutEventBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	var wg sync.WaitGroup
	errs := make([]error, len(f.buses))

	for i, nb := range f.buses {
		wg.Add(1)
		go func(idx int, b NamedEventBus) {
			defer wg.Done()
			if err := b.Bus.Publish(ctx, topic, msg); err != nil {
				f.log.Error("fan-out publish failed",
					"bus", b.Name, "topic", topic, "error", err)
				if !b.Observer {
					errs[idx] = err
				}
			}
		}(i, nb)
	}

	wg.Wait()
	return errors.Join(errs...)
}

// Subscribe delegates to all child event buses.
func (f *FanOutEventBus) Subscribe(pattern string, handler EventHandler) (Subscription, error) {
	subs := make([]Subscription, 0, len(f.buses))
	for _, nb := range f.buses {
		sub, err := nb.Bus.Subscribe(pattern, handler)
		if err != nil {
			f.log.Error("fan-out subscribe failed",
				"bus", nb.Name, "pattern", pattern, "error", err)
			for _, s := range subs {
				_ = s.Unsubscribe()
			}
			return nil, err
		}
		subs = append(subs, sub)
	}
	return &fanOutSubscription{subs: subs}, nil
}

// Close shuts down all child event buses and returns an aggregate error.
func (f *FanOutEventBus) Close() error {
	var errs []error
	for _, nb := range f.buses {
		if err := nb.Bus.Close(); err != nil {
			f.log.Error("fan-out close failed", "bus", nb.Name, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fanOutSubscription aggregates subscriptions from all child event buses.
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
