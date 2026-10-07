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
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/reapermetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// launchReaperTickInterval is the launch reaper's own ticker cadence, fixed
// at 15s regardless of the scheduler's root tickInterval (design §3.7). It
// is also passed as ReaperParams.ReaperInterval, so arming's "no replica
// completed a tick within the interval" check (launchReaperArmingMargin in
// the store) uses the real cadence.
const launchReaperTickInterval = 15 * time.Second

// launchReaperDisarmedWarnAfter is the design's own threshold (§3.7): a
// warning fires once the cluster has been disarmed for more than this long.
// Kept separate from launchReaperDisarmedWarnEvery below —
// one is a design-mandated threshold, the other is purely a log-throttling
// interval, and changing the throttle must not silently change the
// threshold too.
const launchReaperDisarmedWarnAfter = 10 * time.Minute

// launchReaperDisarmedWarnEvery throttles the "disarmed for more than 10
// minutes" warning to at most once per this interval while the cluster
// stays disarmed, so a prolonged outage logs periodically rather than once
// per 15s tick.
const launchReaperDisarmedWarnEvery = 10 * time.Minute

// registerLaunchReaper wires the async-create launch reaper onto its own
// dedicated scheduler ticker (design §3.7). This runs unconditionally,
// regardless of hub.asyncAgentLaunch. With the flag off no agent ever has
// launch_state='active', so the deadline/staleness/wind-down selections
// always use the partial index on launch_deadline (§3.3) and return zero
// rows — but every tick is still a real write transaction on one pooled
// connection (pkg/store/entadapter/launch_reaper.go, RunLaunchReaperTick),
// not a cheap read-only check:
//  1. a Postgres transaction-scoped advisory lock (a no-op on sqlite);
//  2. two `SET LOCAL` statements (Postgres only);
//  3. `SELECT now()` for the store clock on Postgres (a Go-side time.Now()
//     on sqlite, no statement);
//  4. an upsert, then a read of the single arming row — `FOR UPDATE` only on
//     Postgres, since sqlite has one writer;
//  5. the deadline selection (always);
//  6. the staleness selection, once the cluster has been armed for about
//     120s (8x the keepalive interval) — skipped before that;
//  7. the wind-down selection, also gated on armed;
//  8. an UPDATE of `ok_at`/`armed_since` on every tick, whether or not
//     anything was reaped;
//  9. a commit.
//
// A tick that fails (e.g. a DB blip) logs at Warn and issues a best-effort
// disarm write on a fresh connection — so a store outage produces Warn log
// lines even with the feature off. See RunLaunchReaperTick's doc comment for
// the full step-by-step; nothing here adds to it.
func (s *Server) registerLaunchReaper() {
	s.scheduler.RegisterLaunchReaper(launchReaperTickInterval, s.launchReaperTickHandler())
}

// launchReaperTickHandler returns the launch reaper's per-tick handler
// (design §3.7). It calls the store's RunLaunchReaperTick (which applies its
// own 10s tick timeout internally), records the tick-outcome/row-error/
// disarmed-time metrics, publishes AgentStatus for every reaped agent, and
// warns (throttled) once the cluster has been disarmed for more than 10
// minutes.
func (s *Server) launchReaperTickHandler() func(ctx context.Context) {
	// lastWarnAt is safe as a plain closure variable, not requiring a mutex:
	// RegisterLaunchReaper's ticks run synchronously on one dedicated
	// goroutine and never overlap, so this handler is never invoked
	// concurrently with itself.
	var lastWarnAt time.Time

	return func(ctx context.Context) {
		s.mu.RLock()
		keepalive := time.Duration(s.config.LaunchKeepaliveSeconds) * time.Second
		s.mu.RUnlock()

		result, err := s.store.RunLaunchReaperTick(ctx, store.ReaperParams{
			KeepaliveInterval: keepalive,
			ReaperInterval:    launchReaperTickInterval,
		})
		if err != nil {
			// RunLaunchReaperTick's documented outcomes (not_acquired,
			// unavailable, completed, failed) are all returned with a nil
			// error; a non-nil error here would be a store implementation
			// bug, not a normal operating condition.
			slog.Error("launch reaper: tick returned an unexpected error", "error", err)
			return
		}

		if rec := s.reaperMetrics; rec != nil {
			rec.IncTicks(ctx, 1, attribute.String(reapermetrics.AttrTickOutcome, string(result.Outcome)))
		}

		if result.Outcome != store.ReaperTickCompleted {
			return
		}

		if rec := s.reaperMetrics; rec != nil {
			if result.RowErrors > 0 {
				rec.IncRowErrors(ctx, int64(result.RowErrors))
			}
			rec.RecordDisarmedFor(ctx, result.DisarmedFor.Seconds())
		}

		if result.DisarmedFor > launchReaperDisarmedWarnAfter {
			now := time.Now()
			if now.Sub(lastWarnAt) >= launchReaperDisarmedWarnEvery {
				lastWarnAt = now
				slog.Warn("launch reaper: cluster has been disarmed for more than 10 minutes; staleness detection is off (the deadline rule still bounds every launch)",
					"disarmedFor", result.DisarmedFor.Truncate(time.Second).String())
			}
		}

		// Publish after commit (design §3.7 step 5): a publish failure must
		// not disarm. PublishAgentStatus has no error return — its delivery
		// is fire-and-forget over in-memory subscriber channels — so there is
		// nothing to check or that could feed back into arming state here.
		for i := range result.Reaped {
			s.events.PublishAgentStatus(ctx, &result.Reaped[i])
		}
	}
}
