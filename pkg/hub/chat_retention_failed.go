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

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The failed-message purge (design chat message retention §7.4) deletes
// messages to agents whose dispatch failed once they are older than
// messaging.failed_message_retention_days. It is not gated by
// hub.chat_retention or by chat_retention.enabled. It walks the partial
// failed (created, id) index in capped scans and deletes the failed agent
// messages each scan finds with the chat retention cascade, so no
// message_addressees, webchat_mention, webchat_message_ext,
// webchat_message_attachment or webchat_message_star row is left behind.
// The attachments a purge leaves with no message link lose their bytes and
// then their metadata row, as in the chat retention sweep; a failed byte
// delete keeps the row for the attachment GC. The pacing is the chat
// retention sweep's: chat_retention.sweep's chunk size (adapted to the
// transaction time), pause and tick budget, with the budget also bounded by
// the handler's deadline.
//
// Each scan ranges over the partial (created, id) WHERE dispatch_state =
// 'failed' index, so it reads failed rows only, and at most
// chatRetentionFailedScanCap of them, under the chunk timeout. A run that
// stops before the cutoff saves its position in a persisted cursor (phase
// "failed"), and the next run resumes from it. A run that reaches the
// cutoff completes the pass and saves the cursor at the start, so the next
// run begins again at the oldest failed row. A row can turn failed long
// after it was created (a pending message expires, a dispatched or
// long-deferred one fails), behind the position of a pass in progress; the
// next pass reaches it. A full pass reads failed rows only, so it is cheap.

// chatRetentionFailedScanCap bounds the rows one failed-message scan reads.
const chatRetentionFailedScanCap = 1000

// failedPurgeResult describes one failed-message purge run.
type failedPurgeResult struct {
	// Chunks is the number of purge transactions committed.
	Chunks int
	// Scans is the number of capped scans run.
	Scans int
	// ByProject holds the deleted rows per project ID; messages are
	// counted under Failed.
	ByProject map[string]store.ChatRetentionCounts
	// Done is true when the walk reached the cutoff: every failed agent
	// message older than it, from the cursor on, is gone, and the cursor
	// is saved at the start.
	Done bool
	// BudgetExhausted is true when the run stopped at its budget.
	BudgetExhausted bool
	// MaxTx is the longest purge transaction.
	MaxTx time.Duration
	// ChunkSizes are the requested chunk sizes, in order.
	ChunkSizes []int
	// AttachmentDeleteFailures counts unlinked attachments whose bytes
	// could not be deleted; their rows stay for the attachment GC.
	AttachmentDeleteFailures int
}

// Total sums r over every project.
func (r failedPurgeResult) Total() store.ChatRetentionCounts {
	var t store.ChatRetentionCounts
	for _, c := range r.ByProject {
		t.Add(c)
	}
	return t
}

// failedScanCap returns the rows one failed-message scan may read.
func (sw *chatRetentionSweeper) failedScanCap() int {
	if sw.scanCap > 0 {
		return sw.scanCap
	}
	return chatRetentionFailedScanCap
}

// samePosition reports whether a and b are the same walk position.
func samePosition(a, b store.ChatRetentionPosition) bool {
	return a.ID == b.ID && a.Created.Equal(b.Created)
}

// PurgeFailed deletes failed agent messages created before cutoff, oldest
// first from the persisted cursor, in paced steps, until the walk reaches
// the cutoff, the budget passes, ctx ends or a step fails. A step is one
// capped scan, under the chunk timeout, and a purge transaction of the
// failed rows it found, under the chunk timeout too. The cursor is saved at
// the end, also when ctx is done: at the start when the walk reached the
// cutoff, else at the last position scanned.
func (sw *chatRetentionSweeper) PurgeFailed(ctx context.Context, cutoff time.Time, cfg opsettings.ChatRetentionConfig) (failedPurgeResult, error) {
	res := failedPurgeResult{ByProject: map[string]store.ChatRetentionCounts{}}
	start := sw.now()
	remaining, hasDeadline := chatRetentionTimeLeft(ctx)
	budgetEnd := chatRetentionBudgetEnd(start, remaining, hasDeadline, cfg)

	cursor, err := sw.store.GetChatRetentionCursor(ctx, store.ChatRetentionPhaseFailed)
	if err != nil {
		return res, err
	}
	pos := cursor.Position
	walkErr := sw.walkFailed(ctx, cutoff, cfg, budgetEnd, &pos, &res)

	next := pos
	if res.Done {
		next = store.ChatRetentionPosition{} // the pass is complete
	}
	if !samePosition(next, cursor.Position) {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatRetentionFinalWriteTimeout)
		defer cancel()
		cursor.Phase = store.ChatRetentionPhaseFailed
		cursor.Position = next
		cursor.UpdatedAt = sw.now().UTC()
		if err := sw.store.SaveChatRetentionCursor(wctx, cursor); err != nil && walkErr == nil {
			walkErr = err
		}
	}
	return res, walkErr
}

// walkFailed runs the steps of one failed-message purge from *pos, moving
// *pos past every row it scanned.
func (sw *chatRetentionSweeper) walkFailed(ctx context.Context, cutoff time.Time, cfg opsettings.ChatRetentionConfig, budgetEnd time.Time, pos *store.ChatRetentionPosition, res *failedPurgeResult) error {
	chunk := cfg.ChunkSize
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !sw.now().Before(budgetEnd) {
			res.BudgetExhausted = true
			return nil
		}
		res.ChunkSizes = append(res.ChunkSizes, chunk)
		scan, err := sw.scanFailed(ctx, cutoff, *pos, chunk)
		if err != nil {
			return err
		}
		res.Scans++
		if len(scan.Candidates) > 0 {
			ids := make([]string, len(scan.Candidates))
			projectOf := make(map[string]string, len(scan.Candidates))
			for i, c := range scan.Candidates {
				ids[i] = c.ID
				projectOf[c.ID] = c.ProjectID
			}
			tx, err := sw.purgeFailedChunk(ctx, ids, projectOf, res)
			if err != nil {
				return err
			}
			chunk = nextChatRetentionChunk(chunk, cfg.ChunkSize, tx)
		}
		*pos = scan.Next
		if scan.Exhausted {
			res.Done = true
			return nil
		}
		if !sw.now().Add(cfg.Pause()).Before(budgetEnd) {
			res.BudgetExhausted = true
			return nil
		}
		if err := sw.sleep(ctx, cfg.Pause()); err != nil {
			return err
		}
	}
}

// scanFailed runs one capped scan under the chunk timeout.
func (sw *chatRetentionSweeper) scanFailed(ctx context.Context, cutoff time.Time, pos store.ChatRetentionPosition, limit int) (store.FailedMessageScan, error) {
	cctx, cancel := context.WithTimeout(ctx, chatRetentionChunkTimeout)
	defer cancel()
	return sw.store.SelectFailedMessages(cctx, cutoff, pos, sw.failedScanCap(), limit)
}

// purgeFailedChunk deletes one chunk under the chunk timeout, then the
// bytes and rows of the attachments it left with no message link
// (deleteAttachments, each under its own chunk timeout), and adds the
// counts to res. It returns the purge transaction's duration.
func (sw *chatRetentionSweeper) purgeFailedChunk(ctx context.Context, ids []string, projectOf map[string]string, res *failedPurgeResult) (time.Duration, error) {
	cctx, cancel := context.WithTimeout(ctx, chatRetentionChunkTimeout)
	t0 := sw.now()
	pr, err := sw.store.PurgeFailedMessages(cctx, ids, projectOf)
	tx := sw.now().Sub(t0)
	cancel()
	if err != nil {
		return tx, err
	}
	res.Chunks++
	if tx > res.MaxTx {
		res.MaxTx = tx
	}
	ar := sw.deleteAttachments(ctx, pr.UnlinkedAttachments)
	res.AttachmentDeleteFailures += ar.Failures
	for _, byProject := range []map[string]store.ChatRetentionCounts{pr.ByProject, ar.ByProject} {
		for project, c := range byProject {
			acc := res.ByProject[project]
			acc.Add(c)
			res.ByProject[project] = acc
		}
	}
	return tx, nil
}

// Outcomes of recording a failed-message purge's counts in the chat
// retention sweep run.
const (
	failedRunRecorded = "recorded" // added to the open sweep run
	failedRunDeferred = "deferred" // kept; added by a later tick or purge
	failedRunNoRun    = "no_run"   // chat retention on, no run open: logged only
	failedRunInactive = "inactive" // chat retention off: logged only
)

// addFailedPurgeToSweepRun adds the counts of a failed-message purge, with
// any counts kept from earlier purges, to the open chat retention sweep run
// (design §6.6), and returns the outcome. It never opens a run: with chat
// retention off (experiment or enabled), or no sweep run started in the
// last chatRetentionRunWindow, the counts are only logged.
//
// The run row is read, updated and written back, so the write must not
// interleave with a chat retention tick, which holds the same row in memory
// and writes it at its end. The update holds the in-process tick guard and
// the chat retention advisory lock. When a tick holds either, the counts
// are kept on the server's chat retention state, and the next tick on this
// replica (flushPendingFailedCounts) or the next failed purge adds them.
func (s *Server) addFailedPurgeToSweepRun(ctx context.Context, st store.ChatRetentionStore, byProject map[string]store.ChatRetentionCounts, now time.Time) (string, error) {
	if !s.chatRetentionPolicyAt(now).Active {
		s.takePendingFailedCounts() // nothing to record them in
		return failedRunInactive, nil
	}
	counts := mergeChatRetentionCounts(s.takePendingFailedCounts(), byProject)
	if len(counts) == 0 {
		return failedRunNoRun, nil
	}
	if !s.chatRetention.running.CompareAndSwap(false, true) {
		s.keepPendingFailedCounts(counts)
		return failedRunDeferred, nil
	}
	defer s.chatRetention.running.Store(false)
	if locker, ok := s.store.(store.AdvisoryLocker); ok {
		acquired, release, err := locker.TryAdvisoryLock(ctx, store.LockChatRetention)
		if err != nil {
			s.keepPendingFailedCounts(counts)
			return failedRunDeferred, err
		}
		defer func() { _ = release() }()
		if !acquired {
			s.keepPendingFailedCounts(counts)
			return failedRunDeferred, nil
		}
	}
	return s.addCountsToOpenSweepRun(ctx, st, counts, now)
}

// flushPendingFailedCounts adds the counts kept by addFailedPurgeToSweepRun
// to the open sweep run. The chat retention handler calls it after its
// tick, while it holds the tick guard and (through the scheduler) the
// advisory lock.
func (s *Server) flushPendingFailedCounts(ctx context.Context, st store.ChatRetentionStore, now time.Time) (string, error) {
	counts := s.takePendingFailedCounts()
	if len(counts) == 0 {
		return "", nil
	}
	return s.addCountsToOpenSweepRun(ctx, st, counts, now)
}

// addCountsToOpenSweepRun adds counts to the sweep run started in the last
// chatRetentionRunWindow. With none, the counts are dropped (no_run). A
// failed write keeps them for the next attempt. The caller holds the tick
// guard and the advisory lock.
func (s *Server) addCountsToOpenSweepRun(ctx context.Context, st store.ChatRetentionStore, counts map[string]store.ChatRetentionCounts, now time.Time) (string, error) {
	run, err := st.LatestChatRetentionRun(ctx, store.ChatRetentionTriggerSweep, now.Add(-chatRetentionRunWindow))
	if err != nil {
		s.keepPendingFailedCounts(counts)
		return failedRunDeferred, err
	}
	if run == nil {
		return failedRunNoRun, nil
	}
	run.Counts = mergeChatRetentionCounts(run.Counts, counts)
	if err := st.SaveChatRetentionRun(ctx, run); err != nil {
		s.keepPendingFailedCounts(counts)
		return failedRunDeferred, err
	}
	return failedRunRecorded, nil
}

// mergeChatRetentionCounts adds b into a (allocating a when nil) and
// returns it.
func mergeChatRetentionCounts(a, b map[string]store.ChatRetentionCounts) map[string]store.ChatRetentionCounts {
	if a == nil {
		a = map[string]store.ChatRetentionCounts{}
	}
	for project, c := range b {
		acc := a[project]
		acc.Add(c)
		a[project] = acc
	}
	return a
}

// takePendingFailedCounts removes and returns the kept counts (nil when
// none).
func (s *Server) takePendingFailedCounts() map[string]store.ChatRetentionCounts {
	s.chatRetention.mu.Lock()
	defer s.chatRetention.mu.Unlock()
	m := s.chatRetention.pendingFailed
	s.chatRetention.pendingFailed = nil
	return m
}

// keepPendingFailedCounts adds counts to the kept counts.
func (s *Server) keepPendingFailedCounts(counts map[string]store.ChatRetentionCounts) {
	s.chatRetention.mu.Lock()
	defer s.chatRetention.mu.Unlock()
	s.chatRetention.pendingFailed = mergeChatRetentionCounts(s.chatRetention.pendingFailed, counts)
}
