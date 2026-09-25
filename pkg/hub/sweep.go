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

// defaultFailedMessageRetentionDays is the fallback retention window (in days)
// for dispatch_state="failed" messages when
// Server.Config.FailedMessageRetentionDays is unset or non-positive.
const defaultFailedMessageRetentionDays = 7

// missingRecipientFailureReason is recorded on messages that are failed early
// by brokerMessageSweepHandler because their recipient agent no longer exists.
const missingRecipientFailureReason = "recipient agent no longer exists"

// brokerMessageSweepHandler returns a handler that counts messages still in
// dispatch_state='pending' beyond the stuck threshold and logs/emits metrics.
// Messages stuck beyond stuckMessageExpireTTL are transitioned to failed.
// After Phase 4 (no-queuing delivery), no code path creates pending rows — any
// count > 0 indicates a bug. Registered as a RecurringSingleton guarded by
// LockBrokerMessageSweep (B5-2).
func (s *Server) brokerMessageSweepHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		// Tight timeout: fail fast if DB connections are saturated rather than
		// holding a connection while waiting, which worsens the thundering herd.
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		cutoff := time.Now().UTC().Add(-stuckMessageThreshold)
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

		expireCutoff := time.Now().UTC().Add(-stuckMessageExpireTTL)
		expired, err := s.store.ExpireStuckPendingMessages(ctx, expireCutoff, "expired: stuck in pending state beyond TTL")
		if err != nil {
			s.agentLifecycleLog.Error("sweep: expire stuck pending messages failed", "error", err)
			return
		}
		if expired > 0 {
			s.agentLifecycleLog.Info("sweep: expired stuck pending messages",
				"expired", expired, "ttl", stuckMessageExpireTTL.String())
		}

		// Fail pending messages early when their recipient agent has already
		// been deleted — no point waiting out stuckMessageExpireTTL for a
		// delivery that can never succeed.
		orphaned, err := s.store.FailPendingMessagesWithMissingRecipient(ctx, missingRecipientFailureReason)
		if err != nil {
			s.agentLifecycleLog.Error("sweep: fail pending messages with missing recipient failed", "error", err)
			return
		}
		if orphaned > 0 {
			s.agentLifecycleLog.Info("sweep: failed pending messages with missing recipient",
				"count", orphaned)
		}
	}
}

// failedMessageRetentionHandler returns a recurring handler that purges
// messages in dispatch_state="failed" older than the configured retention
// window (Server.Config.FailedMessageRetentionDays, default
// defaultFailedMessageRetentionDays). Registered as a RecurringSingleton
// guarded by LockFailedMessageRetention.
//
// This filters strictly on dispatch_state, unlike the pre-existing
// Store.PurgeOldMessages (read/unread semantics), which would also delete
// successfully delivered message history.
func (s *Server) failedMessageRetentionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		retentionDays := s.config.FailedMessageRetentionDays
		if retentionDays <= 0 {
			retentionDays = defaultFailedMessageRetentionDays
		}
		cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

		purged, err := s.store.PurgeFailedMessages(ctx, cutoff)
		if err != nil {
			s.agentLifecycleLog.Error("failed-message-retention: purge failed", "error", err)
			return
		}
		if purged > 0 {
			s.agentLifecycleLog.Info("failed-message-retention: purged failed messages",
				"count", purged, "retentionDays", retentionDays, "cutoff", cutoff)
		}
	}
}
