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
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// The all-live-hubs check for asynchronous start (ptone/scion#4669).
// Asynchronous start relies on code every hub node must run (the kind-aware
// launch report, claim settlement and the self-report guard), so a start is
// sent asynchronously only while every live hub instance advertises
// api.HubInstanceFeatureAsyncStartV1 in its registry row. The answer is
// cached and a start never waits for a registry read. While the
// hub.async_agent_start experiment is on, a background loop
// (startAsyncStartGateRefresher) re-reads the registry every
// asyncStartGateRefreshInterval, so the answer stays fresh however rarely
// starts arrive (ptone/scion#7250); while it is off, the loop reads nothing.
// A start also triggers one background refresh when the answer is missing
// or older than asyncStartGateTTL (for a server without the loop, and
// before its first tick). An answer older than asyncStartGateMaxAge is not
// used: that only happens when refreshes are failing or stalled, and the
// start then runs synchronously. Stale and stopped rows are ignored: a node
// whose registry writes fail stops blocking after hubInstanceStaleAfter.
// The check fails safe: on any doubt (a read error, a panic, a node without
// the feature) the start runs synchronously. It does not replace the
// rollback rule (turn the experiment off, then wait launch_timeout plus 15
// seconds before downgrading any node).

const (
	// asyncStartGateTTL is how long a computed answer is used before a
	// start triggers a refresh.
	asyncStartGateTTL = 30 * time.Second
	// asyncStartGateMaxAge is the oldest answer a start may use. The
	// refresh loop keeps the answer well below it while the experiment
	// is on; it is reached only when refreshes fail to land.
	asyncStartGateMaxAge = 2 * asyncStartGateTTL
	// asyncStartGateRefreshTimeout bounds one registry read.
	asyncStartGateRefreshTimeout = 5 * time.Second
	// asyncStartGateRefreshInterval is the refresh loop's cadence (±
	// hubInstanceTickJitter). With the read timeout, a healthy answer is
	// never older than about 22 seconds.
	asyncStartGateRefreshInterval = asyncStartGateTTL / 2
	// asyncStartGateReadFailed starts the reason computeAsyncStartGate
	// returns for a registry read error, and is the reason class the gate
	// stores for one (the error text can differ on every read). Such
	// refreshes bypass the once-per-change logging: their warning is
	// rate-limited by readFailWarnedAt (at most once per
	// hubInstanceWarnEvery).
	asyncStartGateReadFailed = "hub instance registry read failed"
	// asyncStartGatePanicked is the reason stored when a refresh panicked.
	// The panic is logged at error with a stack, rate-limited by
	// panicWarnedAt, separately from read errors.
	asyncStartGatePanicked = "hub instance registry refresh panicked"
	// asyncStartGateStopWait bounds the wait for the refresh loop on
	// shutdown, after the server context is cancelled: one read at most.
	asyncStartGateStopWait = asyncStartGateRefreshTimeout + time.Second
)

// asyncStartGate is the cached answer. The zero value reads false and
// has never refreshed.
type asyncStartGate struct {
	mu          sync.Mutex
	value       bool
	refreshedAt time.Time // zero until the first refresh completes
	inFlight    bool
	// reason is the current reason the value is false ("" while true):
	// asyncStartGateReadFailed for any read error, asyncStartGatePanicked
	// for a panic, otherwise the computed reason.
	reason string
	// loggedValue and loggedReason are the last answer logged by the
	// once-per-change rule (logged is false until the first). Read errors
	// and panics have their own rate limits below. A suppressed read-error
	// or panic line leaves them alone, so a registry flapping inside one
	// rate-limit window logs no recovery; an emitted one clears logged, so
	// the next answer is logged (the recovery after an outage), at most once
	// per emitted read-error or panic line.
	loggedValue  bool
	loggedReason string
	logged       bool
	// readFailWarnedAt is when a read error was last logged, and
	// readFailSuppressed counts the read errors since then that were not.
	readFailWarnedAt   time.Time
	readFailSuppressed int
	// panicWarnedAt is when a refresh panic was last logged, and
	// panicSuppressed counts the panics since then that were not.
	panicWarnedAt   time.Time
	panicSuppressed int
}

// asyncStartGateNow is the gate's clock (tests replace it).
var asyncStartGateNow = time.Now

// asyncStartAllowed reports whether a start may be sent asynchronously as
// far as the hub nodes are concerned: the hub.async_agent_start experiment
// is on and every live hub instance advertises async_start_v1. The
// registry is consulted only when the experiment is on.
func (s *Server) asyncStartAllowed() bool {
	if !s.experimentEnabled(experiments.AsyncAgentStart) {
		return false
	}
	return s.allLiveHubsHaveAsyncStart()
}

// allLiveHubsHaveAsyncStart returns the cached answer: false before the
// first refresh and false for an answer older than asyncStartGateMaxAge.
// When the answer is missing or older than asyncStartGateTTL and no
// refresh is running (the refresh loop normally keeps it younger), it
// starts one in the background; it never waits for it.
func (s *Server) allLiveHubsHaveAsyncStart() bool {
	g := &s.asyncStartGate
	g.mu.Lock()
	defer g.mu.Unlock()
	age := asyncStartGateNow().Sub(g.refreshedAt)
	if !g.inFlight && (g.refreshedAt.IsZero() || age >= asyncStartGateTTL) {
		g.inFlight = true
		go s.refreshAsyncStartGate(context.Background())
	}
	if g.refreshedAt.IsZero() || age >= asyncStartGateMaxAge {
		return false
	}
	return g.value
}

// refreshAsyncStartGate computes the answer from the registry and stores
// it; the caller has set inFlight, which it clears. It runs the read with
// its own bounded context. A panic in the read is recovered and stored as a
// false answer, so a background refresh can neither stop the hub process
// nor leave inFlight set. When ctx is cancelled (shutdown), nothing is
// stored or logged.
//
// Logging: a read error warns at most once per hubInstanceWarnEvery, and a
// panic logs at error with a stack at most once per hubInstanceWarnEvery
// (each with the number suppressed since its last line); any other answer
// is logged once per change of its value or reason.
func (s *Server) refreshAsyncStartGate(ctx context.Context) {
	var (
		value     bool
		reason    string
		recovered any
		stack     []byte
	)
	func() {
		defer func() {
			if p := recover(); p != nil {
				recovered, stack = p, debug.Stack()
			}
		}()
		rctx, cancel := context.WithTimeout(ctx, asyncStartGateRefreshTimeout)
		defer cancel()
		value, reason = s.computeAsyncStartGate(rctx)
	}()
	if recovered != nil {
		value, reason = false, asyncStartGatePanicked
	}
	g := &s.asyncStartGate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight = false
	if ctx.Err() != nil {
		return
	}
	now := asyncStartGateNow()
	readFailed := recovered == nil && strings.HasPrefix(reason, asyncStartGateReadFailed+":")
	g.refreshedAt = now
	g.value = value
	g.reason = reason
	if readFailed {
		g.reason = asyncStartGateReadFailed
	}
	log := s.asyncStartGateLog()
	switch {
	case recovered != nil:
		if !g.panicWarnedAt.IsZero() && now.Sub(g.panicWarnedAt) < hubInstanceWarnEvery {
			g.panicSuppressed++
			return
		}
		suppressed := g.panicSuppressed
		g.panicSuppressed = 0
		g.panicWarnedAt = now
		if len(stack) > hubInstancePanicStackBytes {
			stack = stack[:hubInstancePanicStackBytes]
		}
		log.Error("async start disabled: "+asyncStartGatePanicked+"; continuing",
			"panic", fmt.Sprint(recovered),
			"suppressed_since_last_log", suppressed,
			"stack", string(stack))
		g.logged = false
	case readFailed:
		if !g.readFailWarnedAt.IsZero() && now.Sub(g.readFailWarnedAt) < hubInstanceWarnEvery {
			g.readFailSuppressed++
			return
		}
		suppressed := g.readFailSuppressed
		g.readFailSuppressed = 0
		g.readFailWarnedAt = now
		log.Warn("async start disabled: "+reason, "suppressed_since_last_log", suppressed)
		g.logged = false
	case g.logged && g.loggedValue == value && g.loggedReason == reason:
		// Unchanged since the last answer logged by this rule.
	default:
		g.logged, g.loggedValue, g.loggedReason = true, value, reason
		if value {
			log.Info("async start enabled: every live hub instance advertises " + api.HubInstanceFeatureAsyncStartV1)
		} else {
			log.Warn("async start disabled: " + reason)
		}
	}
}

// asyncStartGateLog is the logger for the gate's lines: the agent
// lifecycle log, or slog.Default when it is not set.
func (s *Server) asyncStartGateLog() *slog.Logger {
	if s.agentLifecycleLog != nil {
		return s.agentLifecycleLog
	}
	return slog.Default()
}

// asyncStartGateRefresherStop is the handle CleanupBackgroundResources
// uses to stop the gate's refresh loop: cancel ends the loop's own context
// and done is closed when its goroutine has exited.
type asyncStartGateRefresherStop struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// startAsyncStartGateRefresher starts the gate's refresh loop on a child of
// ctx (the server-lifetime context) and records its stop handle on the
// server, so CleanupBackgroundResources can stop and join the loop
// (stopAsyncStartGateRefresher) before the registry row is marked stopped.
// It starts nothing once stopAsyncStartGateRefresher has run: cleanup can
// race startup, and a loop started after it would never be joined. Both
// run under s.mu, so either the stop sees the handle or the start sees the
// stop. See startAsyncStartGateRefresherEvery.
func (s *Server) startAsyncStartGateRefresher(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asyncStartGateRefresherStopped {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := s.startAsyncStartGateRefresherEvery(loopCtx, func() time.Duration {
		return jitteredInterval(asyncStartGateRefreshInterval)
	})
	s.asyncStartGateRefresherStop = &asyncStartGateRefresherStop{cancel: cancel, done: done}
}

// stopAsyncStartGateRefresher cancels the refresh loop's context and waits
// for the loop to exit, at most asyncStartGateStopWait (one bounded read).
// CleanupBackgroundResources calls it before it marks this instance's
// registry row stopped, so a tick cannot read the registry during shutdown
// (a lone replica would then see no live row and warn), and before the
// store closes. It is a no-op when the loop was never started, and safe to
// call twice; after it, startAsyncStartGateRefresher starts nothing.
func (s *Server) stopAsyncStartGateRefresher() {
	s.mu.Lock()
	h := s.asyncStartGateRefresherStop
	s.asyncStartGateRefresherStop = nil
	s.asyncStartGateRefresherStopped = true
	s.mu.Unlock()
	if h == nil {
		return
	}
	h.cancel()
	timer := time.NewTimer(asyncStartGateStopWait)
	defer timer.Stop()
	select {
	case <-h.done:
	case <-timer.C:
		s.asyncStartGateLog().Warn("async start gate: refresh loop did not stop in time", "wait", asyncStartGateStopWait)
	}
}

// startAsyncStartGateRefresherEvery runs one refresh tick at once and then
// one per interval until ctx is cancelled. Ticks run on the loop's
// goroutine and never overlap.
func (s *Server) startAsyncStartGateRefresherEvery(ctx context.Context, interval func() time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.asyncStartGateRefreshTick(ctx)
		for {
			timer := time.NewTimer(interval())
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.asyncStartGateRefreshTick(ctx)
			}
		}
	}()
	return done
}

// asyncStartGateRefreshTick refreshes the answer when the experiment is on
// and no refresh is running. While the experiment is off it does nothing
// beyond the in-memory experiment check: no lock and no registry read.
func (s *Server) asyncStartGateRefreshTick(ctx context.Context) {
	if !s.experimentEnabled(experiments.AsyncAgentStart) {
		return
	}
	g := &s.asyncStartGate
	g.mu.Lock()
	if g.inFlight {
		g.mu.Unlock()
		return
	}
	g.inFlight = true
	g.mu.Unlock()
	s.refreshAsyncStartGate(ctx)
}

// computeAsyncStartGate reads the hub-instance registry and reports
// whether every live row advertises async_start_v1, with the reason when
// it does not: the first live row (in ID order) without it, a row whose
// stats cannot be read, or the read error.
func (s *Server) computeAsyncStartGate(ctx context.Context) (bool, string) {
	if s.store == nil {
		return false, "no store"
	}
	rows, now, err := s.store.ListHubInstances(ctx, hubInstanceStaleAfter)
	if err != nil {
		return false, fmt.Sprintf("%s: %v", asyncStartGateReadFailed, err)
	}
	live := 0
	for _, row := range rows {
		if hubInstanceState(row, now) != HubInstanceStateLive {
			continue
		}
		live++
		var stats api.HubInstanceStats
		if len(row.Stats) > 0 {
			if err := json.Unmarshal(row.Stats, &stats); err != nil {
				return false, fmt.Sprintf("hub instance %s (version %s) has unreadable stats", row.Label, row.Version)
			}
		}
		if !slices.Contains(stats.Features, api.HubInstanceFeatureAsyncStartV1) {
			return false, fmt.Sprintf("hub instance %s (version %s) lacks %s", row.Label, row.Version, api.HubInstanceFeatureAsyncStartV1)
		}
	}
	if live == 0 {
		// Not even this process's own row yet: wait for its first write.
		return false, "no live hub instance in the registry yet"
	}
	return true, ""
}
