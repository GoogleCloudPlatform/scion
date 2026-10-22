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

package hub

// This file wires the agent lifecycle event pipeline (WP1) into the hub:
// the store observer (pkg/lifecycleevents.Observer, installed on the
// store) and the outbox drainer.
//
// Slice 1 is inert in production (review r6 N4): there is no experiment
// yet (hub.lifecycle_events arrives in slice 2, design 11), so the
// observer is installed only by tests, through installLifecycleObserver.
// At startup the drainer starts only when an observer is installed or the
// outbox already holds rows (a backlog from an earlier run, delivered and
// then the drainer exits); otherwise the only cost is one synchronous
// existence probe. The observer is installed only with a sink, so an
// installed observer always has a drainer that delivers and trims.

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// lifecycleEventStore is what the pipeline needs from the store: the
// observer slot and the drainer's outbox access. The Ent-backed store
// implements it; store fakes do not, and then the pipeline stays off.
type lifecycleEventStore interface {
	store.LifecycleOutboxStore
	SetAgentWriteObserver(o store.AgentWriteObserver)
	AgentWriteObserverInstalled() bool
}

// lifecycleEventsRuntime is the hub's lifecycle pipeline state. The zero
// value is ready to use: off, with no sink.
type lifecycleEventsRuntime struct {
	mu       sync.Mutex
	sink     telemetryemit.Sink
	counters telemetryemit.LifecycleCounters
	state    string
	drainer  *lifecycleevents.Drainer
	cancel   context.CancelFunc
	done     chan struct{}
}

// errLifecycleUnsupported reports a store without lifecycle support.
var errLifecycleUnsupported = errors.New("lifecycle events: the store does not support the lifecycle observer")

// SetLifecycleEventsSink sets the sink the lifecycle drainer delivers to
// (Cloud Logging's scion-analytics in production) and the counters it
// reports to. It must be called before StartBackgroundServices. Without a
// sink the drainer does not start and rows stay in the outbox.
func (s *Server) SetLifecycleEventsSink(sink telemetryemit.Sink, counters telemetryemit.LifecycleCounters) {
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.sink = sink
	rt.counters = counters
}

func (rt *lifecycleEventsRuntime) countersOrNop() telemetryemit.LifecycleCounters {
	if rt.counters == nil {
		return telemetryemit.NopCounters{}
	}
	return rt.counters
}

// lifecycleObserverState returns the observer state this replica may run
// with given its hub ID's source (design 3.3, B4): only a pinned ID, or
// the persisted file on a single-process SQLite hub, is a stable
// producer_namespace.
func (s *Server) lifecycleObserverState(enabled bool) string {
	sqlite := false
	if d, ok := s.store.(interface{ Dialect() string }); ok {
		sqlite = d.Dialect() == dialect.SQLite
	}
	source := s.config.HubIDSource
	if s.config.HubIDPinned {
		source = lifecycleevents.HubIDSourcePinned
	}
	return lifecycleevents.ObserverState(enabled, source, sqlite)
}

// installLifecycleObserver installs the lifecycle observer on the store
// when the hub ID is stable, and records the observer state (exported as a
// gauge, so a refusal never reads as zero activity). It is the slice 1
// test hook: production has no caller until the hub.lifecycle_events
// experiment exists. It returns the resulting state.
func (s *Server) installLifecycleObserver() (string, error) {
	ls, ok := s.store.(lifecycleEventStore)
	if !ok {
		return "", errLifecycleUnsupported
	}
	rt := &s.lifecycleEvents
	state := s.lifecycleObserverState(true)
	rt.mu.Lock()
	if state == telemetryemit.ObserverStateEnabled && rt.sink == nil {
		// Without a sink no drainer runs, so nothing would deliver or trim
		// the outbox (max_rows/max_age): refuse rather than grow it.
		state = telemetryemit.ObserverStateDisabledNoSink
	}
	rt.state = state
	counters := rt.countersOrNop()
	rt.mu.Unlock()
	counters.ObserverState(state)
	if state == telemetryemit.ObserverStateDisabledNoSink {
		slog.Warn("hub.lifecycle_events has no sink configured; lifecycle observer not installed")
		ls.SetAgentWriteObserver(nil)
		return state, nil
	}
	if state != telemetryemit.ObserverStateEnabled {
		slog.Warn("hub.lifecycle_events requires a pinned hub ID; lifecycle observer not installed",
			"hub_id", s.HubID(), "hub_id_source", s.config.HubIDSource, "state", state)
		ls.SetAgentWriteObserver(nil)
		return state, nil
	}
	obs, err := lifecycleevents.NewObserver(lifecycleevents.ObserverConfig{
		HubID:         s.HubID(),
		ProducerBuild: version.Short(),
		Counters:      counters,
		Logger:        logging.Subsystem("hub.lifecycle-events"),
		OnEnqueued:    s.wakeLifecycleDrainer,
	})
	if err != nil {
		return "", err
	}
	ls.SetAgentWriteObserver(obs)
	return state, nil
}

// wakeLifecycleDrainer is the observer's post-commit signal.
func (s *Server) wakeLifecycleDrainer() {
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	d := rt.drainer
	rt.mu.Unlock()
	if d != nil {
		d.Wake()
	}
}

// startLifecycleEvents starts the outbox drainer when there is work for
// it: an installed observer, or rows already in the outbox (then it exits
// once they are delivered: design 5.4, a switched-off pipeline still
// drains its committed backlog). Otherwise the cost is one synchronous
// existence probe, with no goroutine and no poll (review r6 N4). The
// drainer state is exported as lifecycle_drainer_state. It returns the
// started drainer's done channel (closed when the drainer exits), or nil
// when no drainer started.
func (s *Server) startLifecycleEvents(ctx context.Context) <-chan struct{} {
	ls, ok := s.store.(lifecycleEventStore)
	if !ok {
		return nil
	}
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.drainer != nil {
		return rt.done
	}
	installed := ls.AgentWriteObserverInstalled()
	if !installed {
		if rt.state == "" {
			rt.state = s.lifecycleObserverState(false)
			rt.countersOrNop().ObserverState(rt.state)
		}
		has, err := ls.LifecycleOutboxHasRows(ctx)
		if err != nil {
			slog.Warn("lifecycle outbox check failed; drainer not started", "error", err)
			rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
			return nil
		}
		if !has {
			rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
			return nil
		}
	}
	if rt.sink == nil {
		slog.Warn("lifecycle events: no sink configured; outbox rows are kept and not delivered")
		rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
		return nil
	}
	d, err := lifecycleevents.NewDrainer(lifecycleevents.DrainerConfig{
		Store:         ls,
		Sink:          rt.sink,
		Counters:      rt.countersOrNop(),
		Logger:        logging.Subsystem("hub.lifecycle-events"),
		ExitWhenEmpty: !installed,
	})
	if err != nil {
		slog.Warn("lifecycle drainer not started", "error", err)
		rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
		return nil
	}
	if s.ctx != nil {
		ctx = s.ctx // the server-lifetime context, as the registry loop uses
	}
	dctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.drainer, rt.cancel, rt.done = d, cancel, done
	counters := rt.countersOrNop()
	if installed {
		counters.DrainerState(telemetryemit.DrainerStateRunning)
	} else {
		counters.DrainerState(telemetryemit.DrainerStateBacklogOnly)
	}
	go func() {
		defer close(done)
		d.Run(dctx)
		counters.DrainerState(telemetryemit.DrainerStateIdleDisabled)
		// Forget this drainer (a backlog-only one exits on its own), so a
		// later start can run a new one. A stop may already have cleared it.
		rt.mu.Lock()
		if rt.drainer == d {
			rt.drainer, rt.cancel, rt.done = nil, nil, nil
		}
		rt.mu.Unlock()
		cancel()
	}()
	return done
}

// stopLifecycleEvents stops the drainer and waits for its bounded final
// pass (lifecycleevents.DefaultShutdownBudget), or for ctx.
func (s *Server) stopLifecycleEvents(ctx context.Context) {
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	cancel, done := rt.cancel, rt.done
	rt.drainer, rt.cancel, rt.done = nil, nil, nil
	rt.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
