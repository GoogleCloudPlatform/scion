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

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// launchDrainMax bounds how long a shutdown waits for in-flight async
// launches to report broker_restarted and clean up (design wave 3 §1 C-e).
// It comes out of shutdownDeadline, not on top of it.
const launchDrainMax = 10 * time.Second

// brokerDrainingRetryAfter is the Retry-After, in seconds, sent with the
// broker_draining 503.
const brokerDrainingRetryAfter = "5"

// errBrokerShutdown is the cancel cause of an async launch's ctx' when the
// broker shuts down. It tells runLaunch the cancel is not a local stop or
// delete, so it reports broker_restarted (design wave 3 §1 C-e).
var errBrokerShutdown = errors.New("the runtime broker is shutting down")

// launchShutdownMessage is the report text of a launch the broker ended
// because it was shutting down.
const launchShutdownMessage = "the runtime broker shut down during the launch"

// launchDrainState is a Server's shutdown drain state: whether new work is
// refused, the async launches still running, and the contexts that bound
// terminal sends and boot recovery. Its zero value is usable (a Server built
// without New).
type launchDrainState struct {
	mu       sync.Mutex
	draining atomic.Bool
	active   map[*launchRecord]struct{}

	// sendCtx bounds every async launch terminal send; it is cancelled
	// when the drain's wait ends, so a send still retrying stops and its
	// terminal stays in the journal. expired is set at the same moment.
	ctxOnce    sync.Once
	sendCtx    context.Context
	sendCancel context.CancelFunc
	expired    atomic.Bool

	// recoveryCtx bounds boot recovery's sends and cleanup; cancelled by
	// the drain, leaving unrecovered records for the next boot.
	recoveryCtx    context.Context
	recoveryCancel context.CancelFunc
	recoveryWG     sync.WaitGroup
}

func (d *launchDrainState) init() {
	d.ctxOnce.Do(func() {
		d.sendCtx, d.sendCancel = context.WithCancel(context.Background())
		d.recoveryCtx, d.recoveryCancel = context.WithCancel(context.Background())
	})
}

// terminalSendContext is the parent of every async launch terminal send.
func (s *Server) terminalSendContext() context.Context {
	s.launchDrain.init()
	return s.launchDrain.sendCtx
}

// recoveryContext is the parent of boot recovery's work.
func (s *Server) recoveryContext() context.Context {
	s.launchDrain.init()
	return s.launchDrain.recoveryCtx
}

// Draining reports whether the broker has begun to drain for shutdown and
// refuses new creates, starts and restarts.
func (s *Server) Draining() bool {
	return s.launchDrain.draining.Load()
}

// drainExpired reports whether the drain's wait has ended. A launch that
// returns after that keeps its journal record for the next boot.
func (s *Server) drainExpired() bool {
	return s.launchDrain.expired.Load()
}

// trackAsyncLaunch adds an accepted async launch to the set the drain
// cancels. It refuses (false) once the drain has begun; checking and adding
// under one lock means a launch either is refused or is drained.
func (s *Server) trackAsyncLaunch(rec *launchRecord) bool {
	d := &s.launchDrain
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining.Load() {
		return false
	}
	if d.active == nil {
		d.active = make(map[*launchRecord]struct{})
	}
	d.active[rec] = struct{}{}
	return true
}

// untrackAsyncLaunch removes a launch from the drain set.
func (s *Server) untrackAsyncLaunch(rec *launchRecord) {
	d := &s.launchDrain
	d.mu.Lock()
	delete(d.active, rec)
	d.mu.Unlock()
}

// DrainLaunches begins the broker's shutdown drain (design wave 3 §1 C-e).
// From now on createAgent, startAgent and restartAgent answer 503
// broker_draining. Every async launch in flight is cancelled with
// errBrokerShutdown, so it reports failed{broker_restarted} and cleans up
// on a confirming answer; DrainLaunches waits for them up to max (and ctx).
// When the wait ends, sends still retrying are cancelled: their terminals
// stay in the launch journal, which the next boot delivers. Boot recovery
// still running is cancelled too. Safe to call more than once.
func (s *Server) DrainLaunches(ctx context.Context, max time.Duration) {
	d := &s.launchDrain
	d.init()
	d.mu.Lock()
	d.draining.Store(true)
	recs := make([]*launchRecord, 0, len(d.active))
	for rec := range d.active {
		recs = append(recs, rec)
	}
	d.mu.Unlock()

	for _, rec := range recs {
		if rec.shutdown != nil {
			rec.shutdown()
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, max)
	defer cancel()
	finished := 0
	for _, rec := range recs {
		select {
		case <-rec.done:
			finished++
		case <-waitCtx.Done():
		}
	}
	d.expired.Store(true)
	d.sendCancel()
	d.recoveryCancel()
	if len(recs) > 0 {
		s.agentLifecycleLog.Info("Async launch drain finished",
			"launches", len(recs), "finished", finished)
	}
}

// refuseIfDraining answers 503 broker_draining, with Retry-After, and
// reports true once the drain has begun. op names the refused operation.
// Callers run it before any tracker, registration or file work, so a
// refused request leaves nothing behind (C-AC3c).
func (s *Server) refuseIfDraining(w http.ResponseWriter, op, agentID string) bool {
	if !s.Draining() {
		return false
	}
	s.agentLifecycleLog.Info("Agent "+op+" refused: the runtime broker is shutting down", "agent_id", agentID)
	writeBrokerDraining(w, op)
	return true
}

// writeBrokerDraining writes the broker_draining 503.
func writeBrokerDraining(w http.ResponseWriter, op string) {
	w.Header().Set("Retry-After", brokerDrainingRetryAfter)
	writeError(w, http.StatusServiceUnavailable, api.BrokerErrCodeBrokerDraining,
		"Failed to "+op+" agent: the runtime broker is shutting down; retry shortly", nil)
}
