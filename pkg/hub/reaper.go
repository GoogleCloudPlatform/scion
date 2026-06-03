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
)

// Staleness thresholds for the broker-affinity reaper (B5-1, design §7.1).
//
// affinityStaleAge: 2× the defaultAffinityFreshness (90s) used by the routing
// layer in broker_routing.go — a broker that hasn't heartbeated in 3 minutes is
// certainly dead and its affinity is safe to clear.
//
// dispatchStuckAge: 3× the dispatchRollingTimeout (90s) from dispatch_wait.go —
// gives the rolling-timeout wait ample time to fail organically before the
// reaper force-transitions the row.
const (
	affinityStaleAge  = 2 * defaultAffinityFreshness  // 180s
	dispatchStuckAge  = 3 * dispatchRollingTimeout     // 270s
	dispatchMaxRetries = 3
)

// brokerAffinityReapHandler returns a recurring handler that clears stale broker
// affinity and re-drives (or fails) stuck dispatches. Registered as a singleton
// so at most one replica runs it per tick.
func (s *Server) brokerAffinityReapHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		now := time.Now()

		cleared, err := s.store.ReapStaleBrokerAffinity(ctx, now.Add(-affinityStaleAge))
		if err != nil {
			slog.Error("Scheduler: broker affinity reap failed", "error", err)
			return
		}

		requeued, failed, err := s.store.ReapStuckDispatch(ctx, now.Add(-dispatchStuckAge), dispatchMaxRetries)
		if err != nil {
			slog.Error("Scheduler: stuck dispatch reap failed", "error", err)
			return
		}

		if cleared > 0 || requeued > 0 || failed > 0 {
			slog.Info("Scheduler: broker affinity reap complete",
				"affinity_cleared", cleared,
				"dispatch_requeued", requeued,
				"dispatch_failed", failed)
		}
	}
}
