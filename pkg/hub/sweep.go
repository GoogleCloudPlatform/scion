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
	"time"
)

const stuckMessageThreshold = 5 * time.Minute

const stuckMessageExpireTTL = 24 * time.Hour

// brokerMessageSweepHandler returns a handler that counts messages still in
// dispatch_state='pending' beyond the stuck threshold and logs/emits metrics.
// Messages stuck beyond stuckMessageExpireTTL are transitioned to failed.
// After Phase 4 (no-queuing delivery), no code path creates pending rows — any
// count > 0 indicates a bug. Registered as a RecurringSingleton guarded by
// LockBrokerMessageSweep (B5-2).
func (s *Server) brokerMessageSweepHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		cutoff := time.Now().Add(-stuckMessageThreshold)
		count, err := s.store.CountStuckPendingMessages(ctx, cutoff)
		if err != nil {
			s.agentLifecycleLog.Error("sweep: count stuck pending messages failed", "error", err)
			return
		}

		if count > 0 {
			s.agentLifecycleLog.Warn("sweep: stuck pending messages detected",
				"count", count, "threshold", stuckMessageThreshold.String())
		}

		if rec := s.dispatchMetrics; rec != nil {
			rec.ObserveMessageStuck(ctx, int64(count))
		}

		expireCutoff := time.Now().Add(-stuckMessageExpireTTL)
		expired, err := s.store.ExpireStuckPendingMessages(ctx, expireCutoff, "expired: stuck in pending state beyond TTL")
		if err != nil {
			s.agentLifecycleLog.Error("sweep: expire stuck pending messages failed", "error", err)
			return
		}
		if expired > 0 {
			s.agentLifecycleLog.Info("sweep: expired stuck pending messages",
				"expired", expired, "ttl", stuckMessageExpireTTL.String())
		}
	}
}
