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
	"slices"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// The all-live-hubs check for asynchronous start (ptone/scion#4669, design
// wave 3 D-a(6)). Asynchronous start relies on code every hub node must
// run (the kind-aware launch report, claim settlement and the self-report
// guard), so a start is sent asynchronously only while every live hub
// instance advertises api.HubInstanceFeatureAsyncStartV1 in its registry
// row. The answer is cached: a start reads the cached value (false until
// the first refresh) and, when it is older than asyncStartGateTTL, starts
// one refresh in the background, so a slow registry read never delays a
// start. An answer older than asyncStartGateMaxAge (two TTLs: an idle gate)
// is not used: the start runs synchronously while the refresh runs, as for
// a fresh process. Stale and stopped rows are ignored: a node whose registry writes
// fail stops blocking after hubInstanceStaleAfter. The check fails safe:
// on any doubt the start runs synchronously. It does not replace the
// rollback rule (turn the experiment off, then wait launch_timeout plus
// 15 seconds before downgrading any node).

const (
	// asyncStartGateTTL is how long a computed answer is used before a
	// start triggers a refresh.
	asyncStartGateTTL = 30 * time.Second
	// asyncStartGateMaxAge is the oldest answer a start may use. Steady
	// traffic refreshes every TTL and never reaches it; after an idle
	// period the first starts run synchronously until the refresh lands.
	asyncStartGateMaxAge = 2 * asyncStartGateTTL
	// asyncStartGateRefreshTimeout bounds one registry read.
	asyncStartGateRefreshTimeout = 5 * time.Second
)

// asyncStartGate is the cached answer. The zero value reads false and
// has never refreshed.
type asyncStartGate struct {
	mu          sync.Mutex
	value       bool
	refreshedAt time.Time // zero until the first refresh completes
	inFlight    bool
	// reason is the last logged reason the value is false ("" while true):
	// the answer is logged once per change of its value or reason.
	reason string
	logged bool
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
// refresh is running, it starts one in the background; it never waits for
// it.
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
// it. It runs the read with its own bounded context. It logs when the
// answer or the reason for a false answer changes (a different node, or a
// different error), so each distinct cause is logged once.
func (s *Server) refreshAsyncStartGate(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, asyncStartGateRefreshTimeout)
	defer cancel()
	value, reason := s.computeAsyncStartGate(rctx)
	g := &s.asyncStartGate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight = false
	g.refreshedAt = asyncStartGateNow()
	changed := !g.logged || g.value != value || g.reason != reason
	g.value = value
	g.reason = reason
	g.logged = true
	if !changed {
		return
	}
	log := s.agentLifecycleLog
	if log == nil {
		log = slog.Default()
	}
	if value {
		log.Info("async start enabled: every live hub instance advertises " + api.HubInstanceFeatureAsyncStartV1)
	} else {
		log.Warn("async start disabled: " + reason)
	}
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
		return false, fmt.Sprintf("hub instance registry read failed: %v", err)
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
