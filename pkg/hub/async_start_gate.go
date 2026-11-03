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
	"math/rand/v2"
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
	// asyncStartGateReadFailed starts the reason of every false answer
	// caused by a failed refresh (a registry read error or a panic). All
	// such reasons count as one for logging, and their warning is logged
	// at most once per hubInstanceWarnEvery, so a persistent failure does
	// not warn on every refresh.
	asyncStartGateReadFailed = "hub instance registry read failed"
)

// asyncStartGate is the cached answer. The zero value reads false and
// has never refreshed.
type asyncStartGate struct {
	mu          sync.Mutex
	value       bool
	refreshedAt time.Time // zero until the first refresh completes
	inFlight    bool
	// reason is the last reason the value is false ("" while true), with
	// every read failure folded into asyncStartGateReadFailed: the answer
	// is logged once per change of its value or reason.
	reason string
	logged bool
	// readFailWarnedAt is when a read failure was last logged, and
	// readFailSuppressed counts the read failures since then that were not.
	readFailWarnedAt   time.Time
	readFailSuppressed int
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
func (s *Server) refreshAsyncStartGate(ctx context.Context) {
	value, reason := func() (value bool, reason string) {
		defer func() {
			if p := recover(); p != nil {
				value, reason = false, fmt.Sprintf("%s: refresh panicked: %v", asyncStartGateReadFailed, p)
			}
		}()
		rctx, cancel := context.WithTimeout(ctx, asyncStartGateRefreshTimeout)
		defer cancel()
		return s.computeAsyncStartGate(rctx)
	}()
	g := &s.asyncStartGate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight = false
	if ctx.Err() != nil {
		return
	}
	now := asyncStartGateNow()
	g.refreshedAt = now
	readFailed := strings.HasPrefix(reason, asyncStartGateReadFailed+":")
	key := reason
	if readFailed {
		key = asyncStartGateReadFailed
	}
	changed := !g.logged || g.value != value || g.reason != key
	g.value = value
	g.reason = key
	g.logged = true
	log := s.agentLifecycleLog
	if log == nil {
		log = slog.Default()
	}
	if readFailed {
		// Rate-limited whether or not the answer changed: a failing or
		// flapping registry logs at most once per hubInstanceWarnEvery.
		if !g.readFailWarnedAt.IsZero() && now.Sub(g.readFailWarnedAt) < hubInstanceWarnEvery {
			g.readFailSuppressed++
			return
		}
		suppressed := g.readFailSuppressed
		g.readFailSuppressed = 0
		g.readFailWarnedAt = now
		log.Warn("async start disabled: "+reason, "suppressed_since_last_log", suppressed)
		return
	}
	if !changed {
		return
	}
	if value {
		log.Info("async start enabled: every live hub instance advertises " + api.HubInstanceFeatureAsyncStartV1)
	} else {
		log.Warn("async start disabled: " + reason)
	}
}

// startAsyncStartGateRefresher starts the gate's refresh loop on ctx (the
// server-lifetime context) and returns a channel closed when the loop has
// exited. See startAsyncStartGateRefresherEvery.
func (s *Server) startAsyncStartGateRefresher(ctx context.Context) <-chan struct{} {
	return s.startAsyncStartGateRefresherEvery(ctx, jitteredAsyncStartGateInterval)
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

// jitteredAsyncStartGateInterval returns asyncStartGateRefreshInterval ±
// hubInstanceTickJitter, so replicas started together do not read in lock
// step.
func jitteredAsyncStartGateInterval() time.Duration {
	f := 1 + hubInstanceTickJitter*(2*rand.Float64()-1)
	return time.Duration(float64(asyncStartGateRefreshInterval) * f)
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
