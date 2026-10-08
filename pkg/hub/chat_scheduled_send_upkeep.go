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

// Upkeep of scheduled chat messages (ptone/scion#3666): rows left in
// sending by a replica that stopped mid-delivery, retention, and deletion
// together with their conversation or sender.

const (
	// scheduledStuckMargin is added to the longest a delivery can take
	// before a row still in sending is treated as interrupted.
	scheduledStuckMargin = time.Minute
	// scheduledStuckBatch bounds how many interrupted rows one pass marks.
	scheduledStuckBatch = 50
	// scheduledFinalRetention is how long sent and cancelled rows are kept,
	// scheduledFailedRetention how long failed rows are kept.
	scheduledFinalRetention  = 7 * 24 * time.Hour
	scheduledFailedRetention = 30 * 24 * time.Hour
	// scheduledPurgeInterval is how often each replica purges old rows.
	scheduledPurgeInterval = time.Hour
	// scheduledUpkeepTimeout bounds one upkeep pass.
	scheduledUpkeepTimeout = 30 * time.Second
)

// scheduledStuckAfter is how long after its claim a row still in sending
// is treated as interrupted: longer than any delivery can run (the
// delivery bound plus the final state write) plus a margin, so a delivery
// still in progress is never marked. Such a row is the trace of a replica
// that stopped mid-delivery; the message may or may not have been sent.
func scheduledStuckAfter() time.Duration {
	return scheduledDeliveryBudget + scheduledFinalizeTimeout + scheduledStuckMargin
}

// scheduledUpkeep runs one upkeep pass at now: rows stuck in sending are
// marked interrupted, and once per scheduledPurgeInterval old final rows
// are purged. A pass that is still running makes the next one a no-op.
// Neither step sends anything, so both run whether or not the experiment
// is on.
func (s *Server) scheduledUpkeep(ctx context.Context, now time.Time) {
	rt := s.scheduledRuntime()
	if !rt.upkeepBusy.CompareAndSwap(false, true) {
		return
	}
	defer rt.upkeepBusy.Store(false)
	sms := s.scheduledMessageStore()
	if sms == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, scheduledUpkeepTimeout)
	defer cancel()

	s.markInterruptedScheduledMessages(ctx, sms, now)

	rt.mu.Lock()
	purge := rt.lastPurge.IsZero() || now.Sub(rt.lastPurge) >= scheduledPurgeInterval
	if purge {
		rt.lastPurge = now
	}
	rt.mu.Unlock()
	if purge {
		s.purgeScheduledMessages(ctx, sms, now)
	}
}

// markInterruptedScheduledMessages marks the rows that have been sending
// for longer than scheduledStuckAfter as failed/interrupted. They are
// never sent again automatically: the message may already be in the
// conversation, so only the sender can ask to send it again.
func (s *Server) markInterruptedScheduledMessages(ctx context.Context, sms ScheduledMessageStore, now time.Time) int {
	stuck, err := sms.ListStuckScheduledMessages(ctx, now.Add(-scheduledStuckAfter()), scheduledStuckBatch)
	if err != nil {
		scheduledSendLog().Warn("scheduled send: listing interrupted deliveries failed", "error", err)
		return 0
	}
	marked := 0
	for i := range stuck {
		m := &stuck[i]
		if m.ClaimedAt == nil {
			continue
		}
		ok, err := sms.MarkScheduledMessageInterrupted(ctx, m.ID, *m.ClaimedAt, now)
		if err != nil {
			scheduledSendLog().Warn("scheduled send: marking interrupted delivery failed", "id", m.ID, "error", err)
			continue
		}
		if !ok {
			continue // finalized or claimed again in the meantime
		}
		marked++
		m.Status = ScheduledMessageFailed
		m.FailureReason = ScheduledFailureInterrupted
		m.UpdatedAt = now
		s.auditScheduledMessage(ctx, ScheduledAuditInterrupted, m)
		s.publishScheduledMessage(ctx, "failed", m)
	}
	return marked
}

// purgeScheduledMessages deletes sent and cancelled rows older than
// scheduledFinalRetention and failed rows older than
// scheduledFailedRetention.
func (s *Server) purgeScheduledMessages(ctx context.Context, sms ScheduledMessageStore, now time.Time) {
	n, err := sms.PurgeScheduledMessages(ctx, now.Add(-scheduledFinalRetention), now.Add(-scheduledFailedRetention))
	if err != nil {
		scheduledSendLog().Warn("scheduled send: purge failed", "error", err)
		return
	}
	if n > 0 {
		scheduledSendLog().Info("scheduled send: purged old scheduled messages", "count", n)
	}
}

// deleteScheduledMessagesOfConversation deletes the scheduled messages of
// a conversation that was deleted. Best effort: one left behind fails at
// fire time and is purged later.
func (s *Server) deleteScheduledMessagesOfConversation(ctx context.Context, conversationKey string) {
	sms := s.scheduledMessageStore()
	if sms == nil {
		return
	}
	ctx, cancel := finalizeContext(ctx)
	defer cancel()
	n, err := sms.DeleteScheduledMessagesForConversation(ctx, conversationKey)
	if err != nil {
		scheduledSendLog().Warn("scheduled send: deleting scheduled messages of a deleted conversation failed",
			"conversation_key", conversationKey, "error", err)
		return
	}
	if n > 0 {
		scheduledSendLog().Info("scheduled send: deleted scheduled messages of a deleted conversation",
			"conversation_key", conversationKey, "count", n)
	}
}

// deleteScheduledMessagesOfSender deletes the scheduled messages of a user
// who was deleted. Best effort, like deleteScheduledMessagesOfConversation.
func (s *Server) deleteScheduledMessagesOfSender(ctx context.Context, userID string) {
	sms := s.scheduledMessageStore()
	if sms == nil {
		return
	}
	n, err := sms.DeleteScheduledMessagesForSender(ctx, userID)
	if err != nil {
		scheduledSendLog().Warn("scheduled send: deleting scheduled messages of a deleted user failed",
			"user_id", userID, "error", err)
		return
	}
	if n > 0 {
		scheduledSendLog().Info("scheduled send: deleted scheduled messages of a deleted user",
			"user_id", userID, "count", n)
	}
}
