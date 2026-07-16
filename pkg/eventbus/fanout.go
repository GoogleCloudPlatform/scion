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
	mu    sync.RWMutex
	buses []NamedEventBus
	log   *slog.Logger

	subMu         sync.Mutex
	subscriptions []string
}

// NewFanOutEventBus creates a FanOutEventBus that delegates to the given children.
func NewFanOutEventBus(buses []NamedEventBus, log *slog.Logger) *FanOutEventBus {
	return &FanOutEventBus{
		buses: buses,
		log:   log,
	}
}

func (f *FanOutEventBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	f.mu.RLock()
	buses := make([]NamedEventBus, len(f.buses))
	copy(buses, f.buses)
	f.mu.RUnlock()

	if msg.Channel != "" {
		if msg.Channel == InProcessBusName {
			return fmt.Errorf("channel %q is reserved for internal use", InProcessBusName)
		}

		var inproc, target *NamedEventBus
		for i := range buses {
			if buses[i].Name == InProcessBusName {
				inproc = &buses[i]
				continue
			}
			channelKey := buses[i].ChannelID
			if channelKey == "" {
				channelKey = buses[i].Name
			}
			if msg != nil && channelKey == msg.Channel {
				target = &buses[i]
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
	errs := make([]error, len(buses))
	for i, nb := range buses {
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
	f.mu.RLock()
	buses := make([]NamedEventBus, len(f.buses))
	copy(buses, f.buses)
	f.mu.RUnlock()

	subs := make([]Subscription, 0, len(buses))
	for _, nb := range buses {
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

	f.subMu.Lock()
	f.subscriptions = append(f.subscriptions, pattern)
	f.subMu.Unlock()

	return &fanOutSubscription{subs: subs}, nil
}

// Close shuts down all child event buses and returns an aggregate error.
func (f *FanOutEventBus) Close() error {
	f.mu.RLock()
	buses := make([]NamedEventBus, len(f.buses))
	copy(buses, f.buses)
	f.mu.RUnlock()

	var errs []error
	for _, nb := range buses {
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
	f.mu.RLock()
	defer f.mu.RUnlock()

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

// AddSpoke adds a new spoke to the fan-out bus. Returns an error if a spoke
// with the same name already exists.
func (f *FanOutEventBus) AddSpoke(bus NamedEventBus) error {
	f.mu.Lock()
	for _, nb := range f.buses {
		if nb.Name == bus.Name {
			f.mu.Unlock()
			return fmt.Errorf("spoke %q already exists", bus.Name)
		}
	}
	f.buses = append(f.buses, bus)
	f.mu.Unlock()

	f.replaySubscriptions(bus)
	return nil
}

// ReplaceSpoke replaces an existing spoke by name. It tears down the old
// spoke's subscriptions (via Close) and inserts the new one at the same position.
func (f *FanOutEventBus) ReplaceSpoke(name string, newBus NamedEventBus) error {
	if name == InProcessBusName {
		return fmt.Errorf("cannot replace the %q spoke", InProcessBusName)
	}

	var oldBus EventBus
	f.mu.Lock()
	for i, nb := range f.buses {
		if nb.Name == name {
			oldBus = nb.Bus
			f.buses[i] = newBus
			break
		}
	}
	f.mu.Unlock()

	if oldBus == nil {
		return fmt.Errorf("spoke %q not found", name)
	}
	if err := oldBus.Close(); err != nil {
		f.log.Error("failed to close old spoke during replace", "bus", name, "error", err)
	}

	f.replaySubscriptions(newBus)
	return nil
}

// replaySubscriptions replays all existing subscriptions onto a spoke so it
// receives the same event patterns as the other spokes.
func (f *FanOutEventBus) replaySubscriptions(bus NamedEventBus) {
	f.subMu.Lock()
	subs := make([]string, len(f.subscriptions))
	copy(subs, f.subscriptions)
	f.subMu.Unlock()

	for _, pattern := range subs {
		if _, err := bus.Bus.Subscribe(pattern, nil); err != nil {
			f.log.Error("failed to replay subscription to spoke",
				"spoke", bus.Name, "pattern", pattern, "error", err.Error())
		}
	}
}

// RemoveSpoke removes a spoke by name and closes it.
func (f *FanOutEventBus) RemoveSpoke(name string) error {
	if name == InProcessBusName {
		return fmt.Errorf("cannot remove the %q spoke", InProcessBusName)
	}

	var oldBus EventBus
	f.mu.Lock()
	for i, nb := range f.buses {
		if nb.Name == name {
			oldBus = nb.Bus
			f.buses = append(f.buses[:i], f.buses[i+1:]...)
			break
		}
	}
	f.mu.Unlock()

	if oldBus == nil {
		return fmt.Errorf("spoke %q not found", name)
	}
	if err := oldBus.Close(); err != nil {
		f.log.Error("failed to close spoke during remove", "bus", name, "error", err)
	}
	return nil
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
