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

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The auto-suspend worker (ptone/scion#4387). The stalled-detection tick
// only marks agents stalled, publishes their status and hands the agents to
// this worker, so the tick returns within its own bound
// (stalledDetectionTimeout) and never holds a scheduler slot while agents
// are suspended. The worker is owned by the server:
//
//   - At most one worker goroutine runs at a time, so two batches are never
//     processed at once. It is started on demand by a hand-off and exits
//     when nothing is pending, so an idle server runs no goroutine for it.
//   - The hand-off is a pending set deduplicated by agent ID: an agent
//     already waiting, or in the batch being processed, is not added again,
//     so overlapping ticks cannot suspend an agent twice. The set holds at
//     most autoSuspendQueueLimit agents; agents beyond it are dropped (and
//     logged) and stay stalled.
//   - Agents handed off while a batch is being processed wait in the set.
//     When the batch ends, the worker takes everything pending as the next
//     batch, which gets its own start window (autoSuspendStalledAgents).
//   - A panic in a batch is recovered and logged (runAutoSuspendBatch); the
//     batch's agents leave the queued set and the worker carries on.
//   - The worker runs on the server-lifetime ctx. Shutdown cancels it, so
//     no new agent's auto-suspend starts, and stopAutoSuspendWorker waits
//     (bounded) for the one in flight.

// autoSuspendQueueLimit bounds the agents waiting for, or being processed
// by, the auto-suspend worker. A variable so tests can change it.
var autoSuspendQueueLimit = 1000

// autoSuspendStopGrace bounds how long shutdown waits for an auto-suspend
// in flight. A variable so tests can change it.
var autoSuspendStopGrace = 10 * time.Second

// autoSuspendWorkerExitHook, when set (tests), is called by the worker
// goroutine right after it has decided to exit and released the lock.
var autoSuspendWorkerExitHook func()

// autoSuspendItem pairs an agent snapshot with the suspend_reason the worker
// should record if the agent is still eligible when processed.
type autoSuspendItem struct {
	agent  store.Agent
	reason state.SuspendReason
}

// autoSuspendWorker is the state of the server's auto-suspend worker. The
// zero value is ready to use.
type autoSuspendWorker struct {
	mu sync.Mutex
	// pending are the agents waiting for the next batch, in hand-off order.
	pending []autoSuspendItem
	// queued holds the IDs of the pending agents and of the agents in the
	// batch being processed.
	queued map[string]bool
	// running is set while the worker goroutine runs.
	running bool
	// stopped is set by stopAutoSuspendWorker; later hand-offs are dropped.
	stopped bool
	// wg tracks the worker goroutine.
	wg sync.WaitGroup
}

// lifetimeCtx returns the server-lifetime context, cancelled on shutdown,
// or context.Background() for a server built without New (tests).
func (s *Server) lifetimeCtx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// enqueueAutoSuspend hands agents that were just marked stalled to the
// auto-suspend worker, starting it if it is not running. It does not wait
// for them to be suspended.
func (s *Server) enqueueAutoSuspend(agents []store.Agent) {
	s.enqueueAutoSuspendWithReason(agents, state.SuspendReasonStalled)
}

// enqueueAutoSuspendWithReason hands candidate agents to the auto-suspend
// worker with the given suspend reason, starting the worker if it is not
// already running. It does not wait for them to be suspended.
func (s *Server) enqueueAutoSuspendWithReason(agents []store.Agent, reason state.SuspendReason) {
	w := &s.autoSuspend
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		slog.Info("Scheduler: auto-suspend worker stopped; agents not handed off",
			"count", len(agents), "reason", reason)
		return
	}
	if w.queued == nil {
		w.queued = map[string]bool{}
	}
	added, duplicates, dropped := 0, 0, 0
	for i := range agents {
		id := agents[i].ID
		switch {
		case w.queued[id]:
			duplicates++
		case len(w.queued) >= autoSuspendQueueLimit:
			dropped++
		default:
			w.queued[id] = true
			w.pending = append(w.pending, autoSuspendItem{agent: agents[i], reason: reason})
			added++
		}
	}
	if duplicates > 0 {
		slog.Debug("Scheduler: agents already queued for auto-suspend",
			"count", duplicates, "reason", reason)
	}
	if dropped > 0 {
		slog.Warn("Scheduler: auto-suspend queue full; agents left running",
			"dropped", dropped, "limit", autoSuspendQueueLimit, "reason", reason)
	}
	if added > 0 && !w.running {
		w.running = true
		w.wg.Add(1)
		go s.runAutoSuspendWorker()
	}
}

// runAutoSuspendWorker processes the pending agents batch by batch until
// none are left, the worker is stopped or the server shuts down.
func (s *Server) runAutoSuspendWorker() {
	w := &s.autoSuspend
	defer w.wg.Done()
	ctx := s.lifetimeCtx()
	for {
		w.mu.Lock()
		batch := w.pending
		w.pending = nil
		if len(batch) == 0 || w.stopped || ctx.Err() != nil {
			for i := range batch {
				delete(w.queued, batch[i].agent.ID)
			}
			w.running = false
			w.mu.Unlock()
			if autoSuspendWorkerExitHook != nil {
				autoSuspendWorkerExitHook()
			}
			return
		}
		w.mu.Unlock()

		s.runAutoSuspendBatch(ctx, batch)
	}
}

// runAutoSuspendBatch runs one batch and then removes its agents from the
// queued set. A panic in the batch (dispatcher, store or harness) is
// recovered and logged, as the scheduler did when the batch
// ran on its goroutine: the hub keeps running, the batch's agents are
// still removed from the queued set, and the worker goes on with the next
// batch or exits normally, so later hand-offs are still processed.
func (s *Server) runAutoSuspendBatch(ctx context.Context, batch []autoSuspendItem) {
	w := &s.autoSuspend
	defer func() {
		w.mu.Lock()
		for i := range batch {
			delete(w.queued, batch[i].agent.ID)
		}
		w.mu.Unlock()
	}()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Scheduler: auto-suspend worker panicked",
				"panic", r, "batch_size", len(batch), "stack", string(debug.Stack()))
		}
	}()
	s.autoSuspendAgents(ctx, batch)
}

// haltAutoSuspendWorker drops later hand-offs and pending agents without
// waiting for the auto-suspend in flight (stopAutoSuspendWorker does).
// The hub calls it when it begins shutting down (Server.BeginShutdown,
// ptone/scion#5054). It is idempotent.
func (s *Server) haltAutoSuspendWorker() {
	w := &s.autoSuspend
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

// stopAutoSuspendWorker stops the auto-suspend worker: later hand-offs and
// pending agents are dropped, and it waits for the worker goroutine to end,
// at most until ctx is done or autoSuspendStopGrace has passed. The caller
// cancels the server-lifetime ctx first, so no new agent's auto-suspend
// starts; the one in flight runs to the end of its own bound.
func (s *Server) stopAutoSuspendWorker(ctx context.Context) {
	w := &s.autoSuspend
	s.haltAutoSuspendWorker()

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	grace := time.NewTimer(autoSuspendStopGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("Scheduler: shutdown deadline reached while an auto-suspend was in flight")
	case <-grace.C:
		slog.Warn("Scheduler: auto-suspend still in flight after the shutdown grace", "grace", autoSuspendStopGrace)
	}
}
