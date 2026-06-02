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
	"fmt"
	"log/slog"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const InProcessBusName = "inprocess"

// NamedEventBus pairs an EventBus with a name and an observer flag.
// Observer event buses are fire-and-forget: publish errors are logged but
// not returned to the caller.
type NamedEventBus struct {
	Name     string
	Bus      EventBus
	Observer bool
	// ChannelID overrides Name for channel-based message routing. When a
	// message has msg.Channel set, the FanOutEventBus matches it against
	// ChannelID (if non-empty) or Name. This allows a plugin registered
	// under one name (e.g. "chat-app") to handle a different channel
	// identifier (e.g. "gchat").
	ChannelID string
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

func (f *FanOutEventBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	if msg.Channel != "" {
		if msg.Channel == InProcessBusName {
			return fmt.Errorf("channel %q is reserved for internal use", InProcessBusName)
		}

		var inproc, target *NamedEventBus
		for i := range f.buses {
			if f.buses[i].Name == InProcessBusName {
				inproc = &f.buses[i]
				continue
			}
			channelKey := f.buses[i].ChannelID
			if channelKey == "" {
				channelKey = f.buses[i].Name
			}
			if msg != nil && channelKey == msg.Channel {
				target = &f.buses[i]
			}
		}
		if target == nil {
			return fmt.Errorf("no broker registered for channel %q", msg.Channel)
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		if inproc != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := inproc.Bus.Publish(ctx, topic, msg); err != nil {
					errs[0] = fmt.Errorf("inprocess bus publish failed: %w", err)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := target.Bus.Publish(ctx, topic, msg); err != nil {
				if target.Observer {
					f.log.Error("channel publish failed (observer)",
						"channel", msg.Channel, "topic", topic, "error", err)
				} else {
					errs[1] = fmt.Errorf("channel %q publish failed: %w", msg.Channel, err)
				}
			}
		}()
		wg.Wait()
		return errors.Join(errs...)
	}

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

// BusChannel describes a registered event bus channel.
type BusChannel struct {
	Name      string
	Observer  bool
	ChannelID string
}

// BusChannels returns the list of registered bus names (excluding InProcessBus).
func (f *FanOutEventBus) BusChannels() []BusChannel {
	var channels []BusChannel
	for _, nb := range f.buses {
		if nb.Name == InProcessBusName {
			continue
		}
		channels = append(channels, BusChannel{
			Name:      nb.Name,
			Observer:  nb.Observer,
			ChannelID: nb.ChannelID,
		})
	}
	return channels
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
