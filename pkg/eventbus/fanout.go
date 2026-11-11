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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

const InProcessBusName = "inprocess"

// NamedEventBus pairs an EventBus with a name and an observer flag.
// Observer event buses are fire-and-forget: publish errors are logged but
// not returned to the caller.
//
// Handler invocation contract: only the inprocess spoke invokes event
// handlers. External spokes (plugin adapters, gRPC brokers) receive
// subscription patterns so they can filter on the remote side, but they
// discard the handler — inbound delivery happens via the hub API endpoint.
// FanOutEventBus.Subscribe enforces this by passing nil to non-inprocess
// spokes.
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
	// state is the per-registration publish state, set by the
	// FanOutEventBus when the spoke is added.
	state *spokeState
}

// InProcessSpoke is implemented by event buses that run in the hub process
// (the inprocess bus, the web channel bus). Publish always waits for an
// in-process spoke, observers included, so its writes never outlive the
// Publish call and stay ordered. The spoke named InProcessBusName is
// in-process whatever its bus implements.
type InProcessSpoke interface {
	InProcessSpoke()
}

// inProcess reports whether Publish must wait for this spoke.
func (nb NamedEventBus) inProcess() bool {
	if nb.Name == InProcessBusName {
		return true
	}
	_, ok := nb.Bus.(InProcessSpoke)
	return ok
}

// boundedObserver reports whether this spoke is an out-of-process observer
// whose Publish the fan-out waits for only up to ObserverWait.
func (nb NamedEventBus) boundedObserver() bool {
	return nb.Observer && !nb.inProcess()
}

// spokeState counts the observer calls still running after their Publish
// stopped waiting for them (overruns), and how many publishes were skipped
// because of them.
type spokeState struct {
	mu       sync.Mutex
	overruns int
	skipped  int64
	lastWarn time.Time
}

// observerCall is one call to a bounded observer spoke. done is closed,
// under spokeState.mu, when the call returns; overran is set, under the
// same lock, when its Publish stopped waiting for it first.
type observerCall struct {
	done    chan struct{}
	overran bool
}

// Default publish bounds of a FanOutEventBus.
const (
	// DefaultSpokePublishTimeout bounds a spoke call whose ctx has no
	// deadline (for example a publish from a background context).
	DefaultSpokePublishTimeout = 30 * time.Second
	// DefaultObserverWait is how long Publish waits for an out-of-process
	// observer spoke.
	DefaultObserverWait = 2 * time.Second
	// DefaultObserverCallTimeout bounds an out-of-process observer call,
	// which does not end with the caller's ctx.
	DefaultObserverCallTimeout = 30 * time.Second
)

// observerSkipWarnInterval rate-limits the skipped-publish warning per spoke.
const observerSkipWarnInterval = 10 * time.Second

// FanOutEventBus implements EventBus by delegating to N child event buses.
// Publish fans out concurrently. Subscribe and Close delegate to all children.
//
// Every spoke call is bounded. A call whose ctx has no deadline gets
// SpokePublishTimeout. Publish waits for non-observer and in-process spokes
// until they return. It waits for an out-of-process observer spoke at most
// ObserverWait; that call runs on a ctx detached from the caller's
// cancellation and bounded by ObserverCallTimeout. While any such call is
// still running after its Publish returned (an overrun), later publishes
// skip that spoke and are counted. The overruns of one spoke are the calls
// that were already running when the first of them overran, so their
// number is bounded by the publishes started within one ObserverWait.
type FanOutEventBus struct {
	mu    sync.RWMutex
	buses []NamedEventBus
	log   *slog.Logger

	subMu         sync.Mutex
	subscriptions []string

	// SpokePublishTimeout, ObserverWait and ObserverCallTimeout are the
	// publish bounds described on the type. Set them before the first
	// Publish; zero values are replaced by the defaults at construction.
	SpokePublishTimeout time.Duration
	ObserverWait        time.Duration
	ObserverCallTimeout time.Duration
}

// NewFanOutEventBus creates a FanOutEventBus that delegates to the given children.
func NewFanOutEventBus(buses []NamedEventBus, log *slog.Logger) *FanOutEventBus {
	buses = append([]NamedEventBus(nil), buses...)
	for i := range buses {
		buses[i].state = &spokeState{}
	}
	return &FanOutEventBus{
		buses:               buses,
		log:                 log,
		SpokePublishTimeout: DefaultSpokePublishTimeout,
		ObserverWait:        DefaultObserverWait,
		ObserverCallTimeout: DefaultObserverCallTimeout,
	}
}

// ObserverSkipped returns how many publishes skipped the named spoke because
// an earlier observer call to it was still running. It is 0 for an unknown
// spoke.
func (f *FanOutEventBus) ObserverSkipped(name string) int64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, nb := range f.buses {
		if nb.Name == name && nb.state != nil {
			nb.state.mu.Lock()
			defer nb.state.mu.Unlock()
			return nb.state.skipped
		}
	}
	return 0
}

// spokeResult is the outcome of one spoke call in a Publish. err is nil for
// a skipped spoke and for an observer call still running when the wait
// ended (its result is logged when it returns).
type spokeResult struct {
	err error
}

// publishSpokes calls Publish on every spoke concurrently and waits as
// described on FanOutEventBus. The result has one entry per spoke, in order.
func (f *FanOutEventBus) publishSpokes(ctx context.Context, topic string, msg *messages.StructuredMessage, spokes []NamedEventBus) []spokeResult {
	results := make([]spokeResult, len(spokes))
	var wg sync.WaitGroup
	type pendingObserver struct {
		idx   int
		state *spokeState
		call  *observerCall
		errp  *error
	}
	var observers []pendingObserver
	for i, nb := range spokes {
		if nb.boundedObserver() {
			call, errp := f.startObserverCall(ctx, topic, msg, nb)
			if call == nil {
				continue // skipped: an earlier call still runs
			}
			observers = append(observers, pendingObserver{idx: i, state: nb.state, call: call, errp: errp})
			continue
		}
		wg.Add(1)
		go func(idx int, b NamedEventBus) {
			defer wg.Done()
			results[idx].err = f.callSpoke(ctx, topic, msg, b)
		}(i, nb)
	}

	if len(observers) > 0 {
		timer := time.NewTimer(f.ObserverWait)
		defer timer.Stop()
		expired := false
		for _, o := range observers {
			if !expired {
				select {
				case <-o.call.done:
					results[o.idx].err = *o.errp
					continue
				case <-timer.C:
					expired = true
				}
			}
			o.state.mu.Lock()
			select {
			case <-o.call.done:
				results[o.idx].err = *o.errp
			default:
				// Still running: an overrun. Later publishes skip the
				// spoke until every overrun has returned.
				o.call.overran = true
				o.state.overruns++
				f.log.Warn("observer publish still running after wait bound",
					"bus", spokes[o.idx].Name, "topic", topic, "wait", f.ObserverWait)
			}
			o.state.mu.Unlock()
		}
	}
	wg.Wait()
	return results
}

// startObserverCall starts a call to a bounded observer spoke, or returns a
// nil call when the spoke still has an overrun (the publish is skipped and
// counted). Calls started concurrently, before any of them overran, all
// run; once one overruns, no new call starts until every overrun has
// returned. The call's error is stored in *errp before done is closed.
func (f *FanOutEventBus) startObserverCall(ctx context.Context, topic string, msg *messages.StructuredMessage, nb NamedEventBus) (*observerCall, *error) {
	st := nb.state
	if st == nil {
		st = &spokeState{}
	}
	st.mu.Lock()
	if st.overruns > 0 {
		st.skipped++
		skipped := st.skipped
		warn := time.Since(st.lastWarn) >= observerSkipWarnInterval
		if warn {
			st.lastWarn = time.Now()
		}
		st.mu.Unlock()
		if warn {
			f.log.Warn("observer publish skipped: earlier call still running",
				"bus", nb.Name, "topic", topic, "skipped_total", skipped)
		}
		return nil, nil
	}
	st.mu.Unlock()

	call := &observerCall{done: make(chan struct{})}
	errp := new(error)
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.ObserverCallTimeout)
	go func() {
		defer cancel()
		err := nb.Bus.Publish(callCtx, topic, msg)
		st.mu.Lock()
		*errp = err
		overran := call.overran
		if overran {
			st.overruns--
		}
		close(call.done)
		st.mu.Unlock()
		if overran && err != nil {
			f.log.Error("observer publish failed after wait bound",
				"bus", nb.Name, "topic", topic, "error", err)
		}
	}()
	return call, errp
}

// callSpoke calls one awaited spoke, applying SpokePublishTimeout when ctx
// has no deadline.
func (f *FanOutEventBus) callSpoke(ctx context.Context, topic string, msg *messages.StructuredMessage, nb NamedEventBus) error {
	if _, ok := ctx.Deadline(); ok {
		return nb.Bus.Publish(ctx, topic, msg)
	}
	capCtx, cancel := context.WithTimeout(ctx, f.SpokePublishTimeout)
	defer cancel()
	err := nb.Bus.Publish(capCtx, topic, msg)
	if errors.Is(capCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		f.log.Error("spoke publish hit the default timeout",
			"bus", nb.Name, "topic", topic, "timeout", f.SpokePublishTimeout, "error", err)
	}
	return err
}

func (f *FanOutEventBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	f.mu.RLock()
	buses := make([]NamedEventBus, len(f.buses))
	copy(buses, f.buses)
	f.mu.RUnlock()

	if msg != nil && msg.Channel != "" {
		if msg.Channel == InProcessBusName {
			return fmt.Errorf("%w: %q", ErrReservedChannel, InProcessBusName)
		}

		var inproc, target *NamedEventBus
		for i := range buses {
			if buses[i].Name == InProcessBusName {
				inproc = &buses[i]
				continue
			}
			if routingKey(buses[i].Name, buses[i].ChannelID) == msg.Channel {
				target = &buses[i]
			}
		}
		// Always publish to inproc first for persistence/SSE, even if the
		// target channel spoke is missing.
		var spokes []NamedEventBus
		inprocIdx, targetIdx := -1, -1
		if inproc != nil {
			inprocIdx = len(spokes)
			spokes = append(spokes, *inproc)
		}
		if target != nil {
			targetIdx = len(spokes)
			spokes = append(spokes, *target)
		}
		results := f.publishSpokes(ctx, topic, msg, spokes)
		errs := make([]error, 2)
		if inprocIdx >= 0 {
			if err := results[inprocIdx].err; err != nil {
				errs[0] = fmt.Errorf("%w: %w", ErrInProcessPublish, err)
			}
		}
		if targetIdx >= 0 {
			if err := results[targetIdx].err; err != nil {
				if target.Observer {
					f.log.Error("channel publish failed (observer)",
						"channel", msg.Channel, "topic", topic, "error", err)
				} else {
					errs[1] = fmt.Errorf("channel %q publish failed: %w", msg.Channel, err)
				}
			}
		}
		if target == nil {
			errs[1] = fmt.Errorf("no broker registered for channel %q", msg.Channel)
		}
		return errors.Join(errs...)
	}

	results := f.publishSpokes(ctx, topic, msg, buses)
	errs := make([]error, len(buses))
	for i, b := range buses {
		err := results[i].err
		if err == nil {
			continue
		}
		f.log.Error("fan-out publish failed",
			"bus", b.Name, "topic", topic, "error", err)
		if !b.Observer {
			if b.Name == InProcessBusName {
				err = fmt.Errorf("%w: %w", ErrInProcessPublish, err)
			}
			errs[i] = err
		}
	}
	return errors.Join(errs...)
}

// Subscribe registers a handler for the given pattern across all spokes.
// The handler is only installed on the inprocess spoke; external spokes
// receive the pattern with a nil handler so they can set up remote-side
// filtering without invoking callbacks locally. This prevents double
// delivery and nil-handler panics on external spokes. A nil handler is
// rejected with ErrNilHandler, matching InProcessEventBus.
func (f *FanOutEventBus) Subscribe(pattern string, handler EventHandler) (Subscription, error) {
	if handler == nil {
		return nil, ErrNilHandler
	}

	f.mu.RLock()
	buses := make([]NamedEventBus, len(f.buses))
	copy(buses, f.buses)
	f.mu.RUnlock()

	subs := make([]Subscription, 0, len(buses))
	for _, nb := range buses {
		// Only the inprocess spoke invokes handlers. External spokes
		// get the pattern for remote filtering but discard the handler.
		h := handler
		if nb.Name != InProcessBusName {
			h = nil
		}
		sub, err := nb.Bus.Subscribe(pattern, h)
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

// RoutingKey is the value a message's Channel must equal for Publish to
// target this spoke: ChannelID when set, otherwise Name.
func (c BusChannel) RoutingKey() string {
	return routingKey(c.Name, c.ChannelID)
}

func routingKey(name, channelID string) string {
	if channelID != "" {
		return channelID
	}
	return name
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
// with the same name already exists. Once added, the existing subscriptions
// are replayed onto it; the spoke stays added if a replay fails, and the
// joined replay errors are returned.
func (f *FanOutEventBus) AddSpoke(bus NamedEventBus) error {
	bus.state = &spokeState{}
	f.mu.Lock()
	for _, nb := range f.buses {
		if nb.Name == bus.Name {
			f.mu.Unlock()
			return fmt.Errorf("spoke %q already exists", bus.Name)
		}
	}
	f.buses = append(f.buses, bus)
	f.mu.Unlock()

	return f.replaySubscriptions(bus)
}

// ReplaceSpoke replaces an existing spoke by name. It tears down the old
// spoke's subscriptions (via Close) and inserts the new one at the same
// position. It returns an error wrapping ErrSpokeNotFound when no spoke has
// that name; otherwise the spoke is replaced and the joined errors of
// replaying the subscriptions onto it are returned.
func (f *FanOutEventBus) ReplaceSpoke(name string, newBus NamedEventBus) error {
	if name == InProcessBusName {
		return fmt.Errorf("cannot replace the %q spoke", InProcessBusName)
	}

	newBus.state = &spokeState{}
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
		return fmt.Errorf("%w: %q", ErrSpokeNotFound, name)
	}
	if err := oldBus.Close(); err != nil {
		f.log.Error("failed to close old spoke during replace", "bus", name, "error", err)
	}

	return f.replaySubscriptions(newBus)
}

// replaySubscriptions replays all existing subscriptions onto a spoke so it
// receives the same event patterns as the other spokes. The handler is nil
// because only the inprocess spoke invokes handlers — external spokes use
// the pattern for remote-side filtering only. It returns the joined errors
// of the failed Subscribe calls.
func (f *FanOutEventBus) replaySubscriptions(bus NamedEventBus) error {
	if bus.Name == InProcessBusName {
		return nil
	}
	f.subMu.Lock()
	subs := make([]string, len(f.subscriptions))
	copy(subs, f.subscriptions)
	f.subMu.Unlock()

	var errs []error
	for _, pattern := range subs {
		if _, err := bus.Bus.Subscribe(pattern, nil); err != nil {
			f.log.Error("failed to replay subscription to spoke",
				"spoke", bus.Name, "pattern", pattern, "error", err.Error())
			errs = append(errs, fmt.Errorf("replay subscription %q to spoke %q: %w", pattern, bus.Name, err))
		}
	}
	return errors.Join(errs...)
}

// HasSpoke reports whether a spoke with the given name exists.
func (f *FanOutEventBus) HasSpoke(name string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, nb := range f.buses {
		if nb.Name == name {
			return true
		}
	}
	return false
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
		return fmt.Errorf("%w: %q", ErrSpokeNotFound, name)
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
