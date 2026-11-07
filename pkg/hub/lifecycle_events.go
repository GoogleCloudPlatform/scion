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
// store), the outbox drainer, and the context annotations the hub's
// writers pass to the observer.
//
// The observer is installed only while the hub.lifecycle_events experiment
// is on, the hub ID is stable (design 3.3) and a sink is configured
// (design section 22 item 11). reconcileLifecycleEvents decides that, and
// which drainer runs, under rt.mu: at startup and on every settings apply
// (ApplySnapshot), so toggling the experiment needs no restart and no
// poll runs while it is off. With the experiment off and an empty outbox
// the only cost is one synchronous existence probe at startup (review r6
// N4); a backlog left from an earlier run is delivered by a backlog-only
// drainer, which then exits. Diagnostics are Hub metrics plus server log
// lines (design section 22 item 13).

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
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
	sinkKind string
	drainer  *lifecycleevents.Drainer
	cancel   context.CancelFunc
	done     chan struct{}
	// backlogOnly: the running drainer exits once the outbox is empty.
	backlogOnly bool
	// started: startLifecycleEvents ran and stopLifecycleEvents has not.
	started bool
	// testInstalled: the observer was installed by the test hook, so the
	// experiment reconcile leaves it alone.
	testInstalled bool
}

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
	kind := telemetryemit.KindOf(sink)
	if kind != rt.sinkKind {
		rt.sinkKind = kind
		slog.Info("hub.lifecycle_events sink", "lifecycle_sink_kind", kind)
	}
	rt.countersOrNop().SinkKind(kind)
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

// installObserverLocked installs the lifecycle observer on the store when
// the hub ID is stable and a sink is configured, and records the observer
// state (exported as a gauge and a server-log line, so a refusal never
// reads as zero activity). rt.mu is held. It returns the resulting state.
func (s *Server) installObserverLocked(ls lifecycleEventStore) (string, error) {
	rt := &s.lifecycleEvents
	state := s.lifecycleObserverState(true)
	if state == telemetryemit.ObserverStateEnabled && rt.sink == nil {
		// Without a sink no drainer runs, so nothing would deliver or trim
		// the outbox (max_rows/max_age): refuse rather than grow it.
		state = telemetryemit.ObserverStateDisabledNoSink
	}
	switch state {
	case telemetryemit.ObserverStateEnabled:
		obs, err := lifecycleevents.NewObserver(lifecycleevents.ObserverConfig{
			HubID:         s.HubID(),
			ProducerBuild: version.Short(),
			Counters:      rt.countersOrNop(),
			Logger:        logging.Subsystem("hub.lifecycle-events"),
			OnEnqueued:    s.wakeLifecycleDrainer,
		})
		if err != nil {
			return "", err
		}
		ls.SetAgentWriteObserver(obs)
	case telemetryemit.ObserverStateDisabledNoSink:
		if state != rt.state { // change-only: every settings apply re-runs this
			slog.Warn("hub.lifecycle_events has no sink configured; lifecycle observer not installed")
		}
		ls.SetAgentWriteObserver(nil)
	default:
		if state != rt.state {
			slog.Warn("hub.lifecycle_events requires a pinned hub ID; lifecycle observer not installed",
				"hub_id", s.HubID(), "hub_id_source", s.config.HubIDSource, "state", state)
		}
		ls.SetAgentWriteObserver(nil)
	}
	s.setLifecycleStateLocked(state)
	return state, nil
}

// setLifecycleStateLocked records the observer state: the gauge every
// time, and a server-log line when it changes (design section 22 item 13).
func (s *Server) setLifecycleStateLocked(state string) {
	rt := &s.lifecycleEvents
	if state != rt.state {
		slog.Info("hub.lifecycle_events observer state", "lifecycle_observer_state", state,
			"hub_id", s.HubID(), "lifecycle_sink_kind", telemetryemit.KindOf(rt.sink))
	}
	rt.state = state
	rt.countersOrNop().ObserverState(state)
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

// lifecycleContext is the server-lifetime context when there is one.
func (s *Server) lifecycleContext(ctx context.Context) context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return ctx
}

// startLifecycleEvents starts the lifecycle pipeline: it installs the
// observer when hub.lifecycle_events is on (reconcileLifecycleEvents) and
// starts the outbox drainer when there is work for it. It returns the
// running drainer's done channel (closed when that drainer exits), or nil
// when no drainer runs.
func (s *Server) startLifecycleEvents(ctx context.Context) <-chan struct{} {
	ls, ok := s.store.(lifecycleEventStore)
	if !ok {
		return nil
	}
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.started = true
	s.reconcileLocked(ctx, ls)
	// Read under the same lock: a backlog-only drainer that exits at once
	// cannot clear rt.done before it is returned.
	return rt.done
}

// reconcileLifecycleEvents makes the observer and the drainer match the
// hub.lifecycle_events experiment. It runs at startup and on every
// settings apply (ApplySnapshot); every decision is made under rt.mu, so
// a concurrent reconcile, install or drainer exit cannot interleave with
// it (design section 22 item 12, N3). Before startLifecycleEvents (and
// after stopLifecycleEvents) it does nothing.
func (s *Server) reconcileLifecycleEvents(ctx context.Context) {
	ls, ok := s.store.(lifecycleEventStore)
	if !ok {
		return
	}
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started {
		return
	}
	s.reconcileLocked(ctx, ls)
}

// reconcileLocked is reconcileLifecycleEvents with rt.mu held.
func (s *Server) reconcileLocked(ctx context.Context, ls lifecycleEventStore) {
	rt := &s.lifecycleEvents
	ctx = s.lifecycleContext(ctx)
	want := s.experimentEnabled(experiments.LifecycleEvents)
	installed := ls.AgentWriteObserverInstalled()
	switch {
	case rt.testInstalled && installed:
		// The test hook owns the observer.
	case want && !installed:
		if _, err := s.installObserverLocked(ls); err != nil {
			slog.Warn("hub.lifecycle_events: observer not installed", "error", err)
		}
	case !want && installed:
		ls.SetAgentWriteObserver(nil)
		s.setLifecycleStateLocked(telemetryemit.ObserverStateDisabledExperimentOff)
	case !want:
		s.setLifecycleStateLocked(s.lifecycleObserverState(false))
	case want && rt.state != telemetryemit.ObserverStateEnabled:
		// Installed but refused earlier (cannot happen today); re-check.
		if _, err := s.installObserverLocked(ls); err != nil {
			slog.Warn("hub.lifecycle_events: observer not installed", "error", err)
		}
	}
	s.ensureDrainerLocked(ctx, ls)
}

// ensureDrainerLocked starts, replaces or leaves the drainer so that a
// running drainer serves an installed observer, and a backlog-only one
// (exiting once the outbox is empty) delivers rows left while the observer
// is off. rt.mu is held.
func (s *Server) ensureDrainerLocked(ctx context.Context, ls lifecycleEventStore) {
	rt := &s.lifecycleEvents
	installed := ls.AgentWriteObserverInstalled()
	if rt.drainer != nil {
		if installed != rt.backlogOnly {
			return // the right kind already runs
		}
		// Replace it. The old drainer is cancelled and not awaited (its
		// exit takes rt.mu); two drainers overlapping briefly is safe,
		// since rows are leased.
		rt.cancel()
		rt.drainer, rt.cancel, rt.done = nil, nil, nil
	}
	if !installed {
		has, err := ls.LifecycleOutboxHasRows(ctx)
		if err != nil {
			slog.Warn("lifecycle outbox check failed; drainer not started", "error", err)
			rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
			return
		}
		if !has {
			rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
			return
		}
	}
	if rt.sink == nil {
		slog.Warn("lifecycle events: no sink configured; outbox rows are kept and not delivered")
		rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
		return
	}
	s.startDrainerLocked(ctx, ls, !installed)
}

// startDrainerLocked starts a drainer. rt.mu is held.
func (s *Server) startDrainerLocked(ctx context.Context, ls lifecycleEventStore, backlogOnly bool) {
	rt := &s.lifecycleEvents
	d, err := lifecycleevents.NewDrainer(lifecycleevents.DrainerConfig{
		Store:         ls,
		Sink:          rt.sink,
		Counters:      rt.countersOrNop(),
		Logger:        logging.Subsystem("hub.lifecycle-events"),
		ExitWhenEmpty: backlogOnly,
	})
	if err != nil {
		slog.Warn("lifecycle drainer not started", "error", err)
		rt.countersOrNop().DrainerState(telemetryemit.DrainerStateIdleDisabled)
		return
	}
	dctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.drainer, rt.cancel, rt.done, rt.backlogOnly = d, cancel, done, backlogOnly
	counters := rt.countersOrNop()
	if backlogOnly {
		counters.DrainerState(telemetryemit.DrainerStateBacklogOnly)
	} else {
		counters.DrainerState(telemetryemit.DrainerStateRunning)
	}
	go func() {
		defer close(done)
		d.Run(dctx)
		cancel()
		// The exit and the decision to start a successor are one step
		// under rt.mu (review r2 N3): a start that saw this drainer while
		// it was exiting relies on this check.
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if rt.drainer != d {
			return // replaced or stopped already
		}
		rt.drainer, rt.cancel, rt.done = nil, nil, nil
		counters.DrainerState(telemetryemit.DrainerStateIdleDisabled)
		// No successor on a cancelled server context: a shutdown in
		// progress must not loop final passes (wp1s2 review r1 O1).
		if rt.started && ctx.Err() == nil && ls.AgentWriteObserverInstalled() && rt.sink != nil {
			s.startDrainerLocked(ctx, ls, false)
		}
	}()
}

// stopLifecycleEvents stops the drainer and waits for its bounded final
// pass (lifecycleevents.DefaultShutdownBudget), or for ctx. A later
// settings apply no longer reconciles.
func (s *Server) stopLifecycleEvents(ctx context.Context) {
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	cancel, done := rt.cancel, rt.done
	rt.drainer, rt.cancel, rt.done = nil, nil, nil
	rt.started = false
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

// --- Context annotations for the observer (WP1 design 2.3, 2.4a) ---------
//
// The hub's writers call these thin helpers so the contended handler files
// carry one-call hunks and no new imports.

// lifecycleCreateRollback marks the writes made with the returned context
// as the rollback of a failed create at stage (the removal emits
// launch_failed(create_rolled_back) + deleted(create_rolled_back)).
func lifecycleCreateRollback(ctx context.Context, stage string) context.Context {
	return lifecycleevents.WithCause(ctx, lifecycleevents.CauseCreateRolledBack, stage)
}

// lifecycleCreateFailed marks a failed create's kept-row write
// (launch_failed(create_failed)).
func lifecycleCreateFailed(ctx context.Context, stage string) context.Context {
	return lifecycleevents.WithCause(ctx, lifecycleevents.CauseCreateFailed, stage)
}

// lifecycleRestartFailed marks recordRestartStopped's write
// (launch_failed(restart_failed)).
func lifecycleRestartFailed(ctx context.Context) context.Context {
	return lifecycleevents.WithStartOutcome(ctx, lifecycleevents.StartOutcomeRestartFailed)
}

// lifecycleStartUnconfirmed marks the rollback of a start whose outcome is
// unknown (no event; counted).
func lifecycleStartUnconfirmed(ctx context.Context) context.Context {
	return lifecycleevents.WithStartOutcome(ctx, lifecycleevents.StartOutcomeUnconfirmed)
}

// lifecycleConfigResolution marks a config-resolution write: a
// configuration change it commits is configured (slice 2 ruling Q1).
func lifecycleConfigResolution(ctx context.Context) context.Context {
	return lifecycleevents.WithConfigResolution(ctx)
}

// lifecycleDispatchResult marks the writes made with the returned context
// as the sync dispatch result of agent's run (E(iv), r7 N1/N2), from the
// broker answer settleLandedRun recorded on the request's dispatch
// collector. Without a recorded answer for this agent it returns ctx: no
// evidence is claimed from the Hub's request context alone.
func lifecycleDispatchResult(ctx context.Context, agent *store.Agent) context.Context {
	if agent == nil {
		return ctx
	}
	a, ok := dispatchAnswerFrom(ctx, agent.ID)
	if !ok {
		return ctx
	}
	return lifecycleevents.WithDispatchResult(ctx,
		lifecycleevents.NewDispatchResult(a.RunID, a.BrokerRunID, a.BrokerPhaseRunning))
}

// onSyncDispatchAnswer runs in settleLandedRun right after
// adoptBrokerRunID, where the broker's synchronous answer is visible
// (r7 N1). It records what the answer said about the run on the request's
// dispatch collector, for updateAgentAfterDispatch. For the env-gather
// two-pass the last answer wins. It then records the broker's effective
// configuration of the run (syncDispatchEcho, WP1 slice 3): before the
// dispatch-result write, so a sync create gives created, configured,
// [configured,] started.
func onSyncDispatchAnswer(ctx context.Context, d *HTTPAgentDispatcher, agent *store.Agent, resp *RemoteAgentResponse) {
	if agent == nil || agent.ID == "" || resp == nil || resp.Agent == nil {
		return
	}
	recordDispatchAnswer(ctx, dispatchAnswer{
		AgentID:            agent.ID,
		RunID:              agent.RunID,
		BrokerPhaseRunning: resp.Agent.Phase == string(state.PhaseRunning),
		BrokerRunID:        resp.Agent.RunID,
	})
	if d != nil {
		syncDispatchEcho(ctx, d.store, d.lifecycleCountersOrNop(), d.log, agent, resp)
	}
}

// lifecycleStopLegTimeout bounds the detached RecordRestartStopLeg write.
const lifecycleStopLegTimeout = 10 * time.Second

// recordRestartStopLeg records a restart's confirmed stop leg (design
// 2.4a step 3, r5 B1): it closes the agent's open run segment when it is
// runID. It runs before the start leg mints the next run. A failure is
// logged; the restart continues (the closure is then superseded later, the
// documented crash gap).
func (s *Server) recordRestartStopLeg(ctx context.Context, agentID, runID string) {
	r, ok := s.store.(interface {
		RecordRestartStopLeg(ctx context.Context, agentID, runID string) error
	})
	if !ok || runID == "" {
		return
	}
	// Detached from the request: the stop leg is confirmed, so a client
	// that goes away now must not lose the evidence (review r1 O4).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycleStopLegTimeout)
	defer cancel()
	if err := r.RecordRestartStopLeg(ctx, agentID, runID); err != nil {
		slog.Warn("lifecycle: restart stop leg not recorded", "agent_id", agentID, "run_id", runID, "error", err)
	}
}
