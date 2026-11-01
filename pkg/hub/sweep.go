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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const stuckMessageThreshold = 5 * time.Minute

const stuckMessageExpireTTL = 24 * time.Hour

// stuckMessageLogIDCap bounds how many message IDs the stuck-pending sweep
// line names (ptone/scion#4827). 20 UUIDs keep the line under about 1 KB;
// message_ids_truncated on the line says when more rows were stuck.
const stuckMessageLogIDCap = 20

// stuckMessageCountCap bounds the stuck-pending count the sweep reports on
// its line and on the scion.dispatch.message.stuck gauge. The sweep reads at
// most this many rows plus one, so a reported value of stuckMessageCountCap
// means "at least this many" (count_capped on the line). A var (not const)
// so tests can shrink it.
var stuckMessageCountCap = 1000

// defaultFailedMessageRetentionDays is the fallback retention window (in days)
// for dispatch_state="failed" messages when
// Server.Config.FailedMessageRetentionDays is unset or non-positive.
const defaultFailedMessageRetentionDays = 7

// The failed-message retention purge deletes in chunks so it never holds
// the database (on SQLite, the single connection) for long
// (ptone/scion#6954). Vars (not consts) so tests can shrink them.
var (
	// failedPurgeChunkSize bounds the rows one purge transaction examines.
	failedPurgeChunkSize = 500
	// failedPurgeTxBudget bounds how long one purge transaction runs; the
	// store stops the chunk early after the delete batch in flight.
	failedPurgeTxBudget = 250 * time.Millisecond
	// failedPurgeChunkPause is the wait between chunk transactions, so
	// requests queued on the released connection run before the next chunk.
	failedPurgeChunkPause = 50 * time.Millisecond
	// failedPurgeTickBudget bounds the wall time of one purge tick, well
	// inside the handler's 30 s context. A tick always runs at least one
	// chunk; an unfinished pass resumes from the cursor at the next tick.
	failedPurgeTickBudget = 20 * time.Second
)

// failedPurgeState is the in-memory resume cursor of the failed-message
// retention purge. The cursor resets to the start when a pass finishes, so
// rows that become failed after the walk passed their (created, id) — the
// stuck-pending expiry keeps a row's original created — are reached on the
// next pass. Losing it on restart only costs a rescan from the start.
type failedPurgeState struct {
	mu     sync.Mutex
	cursor store.MessagePosition
}

// missingRecipientFailureReason is recorded on messages that are failed early
// by brokerMessageSweepHandler because their recipient agent no longer exists.
const missingRecipientFailureReason = "recipient agent no longer exists"

// brokerMessageSweepHandler returns a handler that counts messages still in
// dispatch_state='pending' beyond the stuck threshold, up to
// stuckMessageCountCap, and logs/emits metrics.
// Messages stuck beyond stuckMessageExpireTTL are transitioned to failed.
// After Phase 4 (no-queuing delivery), the only writer of pending rows is
// ExecuteAgentDM: it persists the row "pending" for the length of its
// dispatch, then marks it dispatched or failed. The dispatch is detached
// from the caller's cancellation (dispatchContext), so a row stays pending
// past the threshold only by design after a deadline-ambiguous dispatch
// (AC-4 of #1689), or by a fault: a failed finalization write, a hub that
// exited mid-dispatch, or a writer that left the state empty
// (ptone/scion#4827). Every write of a pending row is logged as
// store.MessageEnteredPendingLogMsg. Registered as a RecurringSingleton
// guarded by LockBrokerMessageSweep (B5-2).
func (s *Server) brokerMessageSweepHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		// Tight timeout: fail fast if DB connections are saturated rather than
		// holding a connection while waiting, which worsens the thundering herd.
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		// The gauge is a bounded count (ptone/scion#6648): the sweep reads at
		// most stuckMessageCountCap+1 stuck IDs from the partial pending index
		// instead of running a full COUNT every run, so a large backlog cannot
		// hold the database connection. The oldest stuckMessageLogIDCap IDs
		// are named on the line (ptone/scion#4827).
		cutoff := time.Now().UTC().Add(-stuckMessageThreshold)
		ids, err := s.store.ListStuckPendingMessageIDs(ctx, cutoff, stuckMessageCountCap+1)
		if err != nil {
			s.agentLifecycleLog.Error("sweep: list stuck pending messages failed", "error", err)
			return
		}
		count, countCapped := len(ids), len(ids) > stuckMessageCountCap
		if countCapped {
			count = stuckMessageCountCap
		}

		if count > 0 {
			named := ids[:min(count, stuckMessageLogIDCap)]
			s.agentLifecycleLog.Warn("sweep: stuck pending messages detected",
				"count", count, "count_capped", countCapped,
				"threshold", stuckMessageThreshold.String(),
				"message_ids", named, "message_ids_truncated", len(ids) > len(named))
		}

		if rec := s.dispatchMetrics; rec != nil {
			rec.ObserveMessageStuck(ctx, int64(count))
		}

		expireNow := time.Now().UTC()
		expireCutoff := expireNow.Add(-stuckMessageExpireTTL)
		// A failed expiry is logged and does not stop the sweep: the
		// missing-recipient step below must still run (ptone/scion#5211).
		expired, err := s.store.ExpireAndListStuckPendingMessages(ctx, expireCutoff, store.MessageExpiredStuckPendingReason)
		if err != nil {
			s.agentLifecycleLog.Error("sweep: expire stuck pending messages failed", "error", err)
		} else if len(expired) > 0 {
			s.agentLifecycleLog.Info("sweep: expired stuck pending messages",
				expiredPendingLogAttrs(expired, expireNow, expiredMessageLogIDCap)...)
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

		purged, chunks, done, err := s.purgeFailedMessages(ctx, cutoff)
		if purged > 0 {
			s.agentLifecycleLog.Info("failed-message-retention: purged failed messages",
				"count", purged, "chunks", chunks, "passComplete", done,
				"retentionDays", retentionDays, "cutoff", cutoff)
		}
		if err != nil {
			s.agentLifecycleLog.Error("failed-message-retention: purge failed", "error", err)
			return
		}

		// Drop webchat mention rows whose message is gone. The webchat
		// tables live outside the Ent graph, so the purge above cannot
		// cascade to them; sweeping orphans here also covers any other
		// hard delete of messages.
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs != nil {
			orphans, err := wcs.PurgeOrphanMentions(ctx)
			if err != nil {
				s.agentLifecycleLog.Error("failed-message-retention: mention sweep failed", "error", err)
				return
			}
			if orphans > 0 {
				s.agentLifecycleLog.Info("failed-message-retention: purged orphan mention rows",
					"count", orphans)
			}
		}
	}
}

// purgeFailedMessages runs one tick of the chunked failed-message purge:
// chunk transactions of at most failedPurgeChunkSize rows and
// failedPurgeTxBudget each, separated by failedPurgeChunkPause, from the
// saved cursor until the pass completes or failedPurgeTickBudget is spent.
// It returns the rows deleted, the chunks run and whether the pass
// completed. On error the cursor keeps the last committed chunk's position.
func (s *Server) purgeFailedMessages(ctx context.Context, cutoff time.Time) (purged, chunks int, done bool, err error) {
	s.failedPurge.mu.Lock()
	defer s.failedPurge.mu.Unlock()

	start := time.Now()
	for {
		res, chunkErr := s.store.PurgeFailedMessagesChunk(ctx, cutoff, s.failedPurge.cursor, failedPurgeChunkSize, failedPurgeTxBudget)
		if chunkErr != nil {
			return purged, chunks, false, chunkErr
		}
		chunks++
		purged += res.Deleted
		if res.Done {
			s.failedPurge.cursor = store.MessagePosition{}
			return purged, chunks, true, nil
		}
		s.failedPurge.cursor = res.Next
		if time.Since(start)+failedPurgeChunkPause >= failedPurgeTickBudget {
			return purged, chunks, false, nil
		}
		if failedPurgeChunkPause > 0 {
			t := time.NewTimer(failedPurgeChunkPause)
			select {
			case <-ctx.Done():
				t.Stop()
				return purged, chunks, false, ctx.Err()
			case <-t.C:
			}
		}
	}
}

// expiredMessageLogIDCap bounds how many expired messages the TTL-expiry
// sweep line names (ptone/scion#5211). The count on the line is always the
// full number expired; message_ids_truncated says when more were expired
// than named.
const expiredMessageLogIDCap = 20

// expiredPendingLogAttrs builds the attributes of the "sweep: expired stuck
// pending messages" line: the number expired, the TTL, and for up to limit
// of the expired rows (oldest first) the message ID, the recipient and how
// long the row was pending at now. The three lists are index-aligned. Only
// identifiers and the routing target are logged, never a body.
func expiredPendingLogAttrs(expired []store.ExpiredPendingMessage, now time.Time, limit int) []any {
	named := expired
	if len(named) > limit {
		named = named[:limit]
	}
	ids := make([]string, 0, len(named))
	recipients := make([]string, 0, len(named))
	durations := make([]string, 0, len(named))
	for _, m := range named {
		ids = append(ids, m.ID)
		recipients = append(recipients, m.Recipient)
		durations = append(durations, now.Sub(m.CreatedAt).Round(time.Second).String())
	}
	return []any{
		"expired", len(expired),
		"ttl", stuckMessageExpireTTL.String(),
		"message_ids", ids,
		"recipients", recipients,
		"pending_durations", durations,
		"message_ids_truncated", len(expired) > len(named),
	}
}
