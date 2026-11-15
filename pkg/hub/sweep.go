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

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
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

		// Rows left delivering by a conduit delivery whose hub went away
		// mid-call (design v2.22 §2.6) become partial, never dispatched:
		// the paste may have happened, so they are never re-driven.
		if n, err := s.store.SweepStaleDeliveries(ctx, time.Now().Add(-agentcontrol.StaleDelivering), staleDeliveryOutcome); err != nil {
			s.agentLifecycleLog.Error("sweep: stale conduit deliveries failed", "error", err)
		} else if n > 0 {
			s.agentLifecycleLog.Warn("sweep: stale conduit deliveries marked partial", "count", n)
		}

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

// failedMessageRetentionTimeout bounds one run of the failed-message purge.
const failedMessageRetentionTimeout = 30 * time.Second

// failedMessageRetentionDays returns messaging.failed_message_retention_days,
// or its compiled default without DB-backed operational settings.
func (s *Server) failedMessageRetentionDays() int {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.FailedMessageRetentionDays()
	}
	return DefaultFailedMessageRetentionDays
}

// failedMessageRetentionHandler returns a recurring handler that purges
// messages to agents in dispatch_state="failed" older than
// messaging.failed_message_retention_days (read on every run, so a change
// applies without a restart). Registered as a RecurringSingleton guarded by
// LockFailedMessageRetention.
//
// It filters strictly on dispatch_state and an agent recipient, so
// delivered history and "user:" rows are never touched, and deletes in
// paced, capped steps from a persisted cursor with the chat retention
// cascade (see PurgeFailed), so the dependent rows of every deleted message
// go with it, and the attachments it unlinks lose their bytes and rows. The
// counts are added to the open chat retention sweep run when there is one
// (see addFailedPurgeToSweepRun), and logged.
func (s *Server) failedMessageRetentionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, failedMessageRetentionTimeout)
		defer cancel()

		// The cascade spans the web chat tables, so the purge needs the
		// chat retention store, which is wired once they exist.
		st := s.chatRetentionStore()
		if st == nil {
			s.agentLifecycleLog.Warn("failed-message-retention: skipped, no chat retention store (web chat tables not initialized)")
			return
		}
		retentionDays := s.failedMessageRetentionDays()
		cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

		sw := newChatRetentionSweeper(st)
		// The attachments a purge unlinks lose their bytes, then their
		// rows; without an attachment store the rows stay for the
		// attachment GC.
		s.mu.RLock()
		if as := s.attachmentStore; as != nil {
			sw.attachments = as
		}
		s.mu.RUnlock()
		res, err := sw.PurgeFailed(ctx, cutoff, s.chatRetentionConfig())
		total := res.Total()
		var inRun string
		if total.Failed > 0 {
			// The run update must still happen when the purge stopped at
			// the handler's deadline.
			wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), chatRetentionFinalWriteTimeout)
			var rerr error
			inRun, rerr = s.addFailedPurgeToSweepRun(wctx, st, res.ByProject, time.Now())
			wcancel()
			if rerr != nil {
				s.agentLifecycleLog.Error("failed-message-retention: record counts in chat retention run failed", "error", rerr)
			}
		}
		attrs := []any{"count", total.Failed, "addressees", total.Addressees, "mentions", total.Mentions,
			"stars", total.Stars, "ext", total.Ext, "attachments", total.Attachments,
			"attachment_rows", total.AttachmentRows, "attachment_byte_failures", res.AttachmentDeleteFailures,
			"chunks", res.Chunks, "scans", res.Scans, "done", res.Done, "max_tx_ms", res.MaxTx.Milliseconds(),
			"chat_retention_run", inRun, "retentionDays", retentionDays, "cutoff", cutoff}
		switch {
		case err != nil:
			s.agentLifecycleLog.Error("failed-message-retention: purge failed", append([]any{"error", err}, attrs...)...)
		case total.Failed > 0:
			s.agentLifecycleLog.Info("failed-message-retention: purged failed messages", attrs...)
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
