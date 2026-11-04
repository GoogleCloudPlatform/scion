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
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Chat message retention (hub.chat_retention): a hub admin sets one
// hub-wide age window in the chat_retention settings section, and the
// "chat-retention" singleton purges older messages, oldest first, in small
// paced transactions (design chat message retention §6).
//
// Each tick takes one policy snapshot. The sweep is active only while the
// experiment is on and the section has enabled=true; otherwise it purges
// nothing. An active tick runs two phases:
//
//   - the deleted-conversation phase (design §6.2 Phase B), always: the
//     messages of conversations and topics soft-deleted more than
//     deleted_conversation_grace_days ago, whatever the window. Candidates
//     are read in ID pages from the partial deleted_at indexes, and each
//     candidate's messages are purged in chunks by conversation_id (a
//     legacy topic's by thread_id). It has no cursor: a candidate whose
//     messages are gone costs one probe per tick. When a window is set it
//     gets at most half of the tick, so neither phase starves the other.
//   - the main window (Phase A), only when a window is set: the walk over
//     the created index resumes from a persisted (created, id) cursor,
//     which is reset to the start once a day so messages skipped while
//     pending or deferred are picked up once dispatched.
//
// Every chunk is one purge transaction with a 5 s timeout; the chunk size
// adapts to the transaction time, the sweeper pauses pause_ms after each
// chunk, and the tick stops after tick_budget_seconds. After each purge
// commits, the attachments it left with no message link lose their bytes
// first and then their metadata row; a failed byte delete keeps the row,
// so the attachment GC retries it (design §6.3). The open sweep run (one
// chat_retention_runs row per 24 h) accumulates per-project counts and the
// policy, never message IDs or content.

const (
	// chatRetentionChunkTimeout bounds the selection and the purge
	// transaction of one chunk.
	chatRetentionChunkTimeout = 5 * time.Second
	// A chunk transaction slower than chatRetentionShrinkAbove halves the
	// chunk size (not below chatRetentionMinChunk); one faster than
	// chatRetentionGrowBelow grows it by 25% (not above chunk_size).
	chatRetentionShrinkAbove = 250 * time.Millisecond
	chatRetentionGrowBelow   = 50 * time.Millisecond
	chatRetentionMinChunk    = opsettings.ChatRetentionMinChunkSize
	// chatRetentionCursorResetEvery is how often the cursor goes back to
	// the start of the walk.
	chatRetentionCursorResetEvery = 24 * time.Hour
	// chatRetentionRunWindow is how long a sweep run row stays open and
	// accumulates the counts of later ticks.
	chatRetentionRunWindow = 24 * time.Hour
	// chatRetentionRunKeep is how long run rows are kept.
	chatRetentionRunKeep = 365 * 24 * time.Hour
	// chatRetentionCutoffCacheTTL bounds how stale the history marker's
	// cached last applied cutoff may be.
	chatRetentionCutoffCacheTTL = time.Minute
	// chatRetentionDeadlineMargin is kept free before the handler context's
	// deadline besides one chunk timeout, for the end-of-tick writes.
	chatRetentionDeadlineMargin = 2 * time.Second
	// chatRetentionFinalWriteTimeout bounds the end-of-tick cursor, run and
	// prune writes, which run even when the tick's context is done.
	chatRetentionFinalWriteTimeout = 5 * time.Second
	// chatRetentionTickSlack absorbs scheduler jitter when deciding whether
	// tick_interval_minutes has passed since the last tick.
	chatRetentionTickSlack = 5 * time.Second
	// chatRetentionCandidatePage is how many deleted conversations or
	// topics the deleted-conversation phase reads at a time.
	chatRetentionCandidatePage = 100
)

// chatRetentionPolicy is the policy snapshot of one tick (design §4.2).
type chatRetentionPolicy struct {
	// Active is experiment && enabled.
	Active bool
	Config opsettings.ChatRetentionConfig
	Now    time.Time
	// MainCutoff is now - messages_retention_days; zero when the sweep is
	// not active or no window is set.
	MainCutoff time.Time
	// GraceCutoff is now - deleted_conversation_grace_days: the messages
	// of conversations and topics deleted before it are purged. Zero when
	// the sweep is not active.
	GraceCutoff time.Time
	// GraceDays is the deleted_conversation_grace_days GraceCutoff was
	// computed with.
	GraceDays int
}

// purges reports whether the policy deletes anything: an active sweep
// always runs the deleted-conversation phase.
func (p chatRetentionPolicy) purges() bool { return p.Active && !p.GraceCutoff.IsZero() }

// newChatRetentionPolicy resolves the policy for one tick at now.
func newChatRetentionPolicy(experimentOn bool, cfg opsettings.ChatRetentionConfig, now time.Time) chatRetentionPolicy {
	p := chatRetentionPolicy{Active: experimentOn && cfg.Enabled, Config: cfg, Now: now.UTC()}
	if !p.Active {
		return p
	}
	if cfg.HasWindow() {
		p.MainCutoff = p.Now.Add(-time.Duration(cfg.MessagesRetentionDays) * 24 * time.Hour)
	}
	grace := cfg.DeletedConversationGraceDays
	if grace < opsettings.ChatRetentionMinGraceDays {
		// A config not resolved from the section (zero value): the
		// compiled default, never "purge at once".
		grace = opsettings.ChatRetentionDefaultGraceDays
	}
	p.GraceDays = grace
	p.GraceCutoff = p.Now.Add(-time.Duration(grace) * 24 * time.Hour)
	return p
}

// chatRetentionConfig returns the resolved chat_retention section, or the
// disabled default without DB-backed operational settings.
func (s *Server) chatRetentionConfig() opsettings.ChatRetentionConfig {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.ChatRetention()
	}
	return opsettings.DefaultChatRetentionConfig()
}

// chatRetentionPolicyAt takes the policy snapshot at now.
func (s *Server) chatRetentionPolicyAt(now time.Time) chatRetentionPolicy {
	return newChatRetentionPolicy(s.experimentEnabled(experiments.ChatRetention), s.chatRetentionConfig(), now)
}

// chatRetentionState is the Server's chat retention wiring: the store, the
// in-process tick guard and the history marker's cached cutoff.
type chatRetentionState struct {
	mu    sync.Mutex
	store store.ChatRetentionStore

	// running guards against overlapping ticks in one process (on SQLite
	// the advisory lock is a no-op).
	running atomic.Bool

	// cutoff caches the main cursor's last applied cutoff for the history
	// marker; cutoffAt is when it was read (zero = never).
	cutoff   *time.Time
	cutoffAt time.Time

	// pendingFailed holds failed-message purge counts per project that
	// could not be added to the open sweep run while a tick held it
	// (addFailedPurgeToSweepRun); a later tick or purge adds them.
	pendingFailed map[string]store.ChatRetentionCounts
}

// SetChatRetentionStore wires the chat retention store. Without one the
// sweeper does nothing and history carries no retention marker.
func (s *Server) SetChatRetentionStore(st store.ChatRetentionStore) {
	s.chatRetention.mu.Lock()
	s.chatRetention.store = st
	s.chatRetention.cutoff = nil
	s.chatRetention.cutoffAt = time.Time{}
	s.chatRetention.mu.Unlock()
}

func (s *Server) chatRetentionStore() store.ChatRetentionStore {
	s.chatRetention.mu.Lock()
	defer s.chatRetention.mu.Unlock()
	return s.chatRetention.store
}

// noteChatRetentionCutoff records a cutoff the sweeper just applied, so the
// history marker on this replica sees it without waiting for the cache.
func (s *Server) noteChatRetentionCutoff(cutoff time.Time, now time.Time) {
	c := cutoff.UTC()
	s.chatRetention.mu.Lock()
	s.chatRetention.cutoff = &c
	s.chatRetention.cutoffAt = now
	s.chatRetention.mu.Unlock()
}

// chatRetentionLastAppliedCutoff returns the main cursor's last applied
// cutoff, read from the store at most once per chatRetentionCutoffCacheTTL.
// nil when none is set, there is no store or the read fails.
func (s *Server) chatRetentionLastAppliedCutoff(ctx context.Context, now time.Time) *time.Time {
	s.chatRetention.mu.Lock()
	st := s.chatRetention.store
	if st == nil {
		s.chatRetention.mu.Unlock()
		return nil
	}
	if !s.chatRetention.cutoffAt.IsZero() && now.Sub(s.chatRetention.cutoffAt) < chatRetentionCutoffCacheTTL {
		c := s.chatRetention.cutoff
		s.chatRetention.mu.Unlock()
		return c
	}
	s.chatRetention.mu.Unlock()

	cur, err := st.GetChatRetentionCursor(ctx, store.ChatRetentionPhaseMain)
	if err != nil {
		slog.Warn("chat retention: read last applied cutoff", "error", err)
		return nil
	}
	s.chatRetention.mu.Lock()
	s.chatRetention.cutoff = cur.LastAppliedCutoff
	s.chatRetention.cutoffAt = now
	s.chatRetention.mu.Unlock()
	return cur.LastAppliedCutoff
}

// chatRetentionHandler is the "chat-retention" recurring singleton. It is
// registered every minute; RunTick skips a tick until tick_interval_minutes
// have passed since the last one.
func (s *Server) chatRetentionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		st := s.chatRetentionStore()
		if st == nil {
			return
		}
		p := s.chatRetentionPolicyAt(time.Now())
		if !p.purges() {
			return
		}
		if !s.chatRetention.running.CompareAndSwap(false, true) {
			return
		}
		defer s.chatRetention.running.Store(false)

		sw := newChatRetentionSweeper(st)
		s.mu.RLock()
		if as := s.attachmentStore; as != nil {
			sw.attachments = as
		}
		s.mu.RUnlock()
		res, err := sw.RunTick(ctx, p)
		if res.LastAppliedCutoff != nil {
			s.noteChatRetentionCutoff(*res.LastAppliedCutoff, time.Now())
		}
		// Failed-message purge counts kept while a tick held the run row
		// go into it now, while this handler holds the guard and the lock.
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), chatRetentionFinalWriteTimeout)
		if _, ferr := s.flushPendingFailedCounts(fctx, st, time.Now()); ferr != nil {
			slog.Error("chat retention: record failed-message purge counts", "error", ferr)
		}
		fcancel()
		if err != nil {
			slog.Error("chat retention: tick failed", "error", err, "chunks", res.Chunks,
				"purged", res.Purged.Main, "purged_deleted_conv", res.Purged.DeletedConv)
			return
		}
		if res.Ran && (res.Purged.Main > 0 || res.Purged.DeletedConv > 0 || res.Purged.AttachmentRows > 0) {
			slog.Info("chat retention: purged messages", "purged", res.Purged.Main,
				"purged_deleted_conv", res.Purged.DeletedConv, "attachments", res.Purged.AttachmentRows,
				"attachment_byte_failures", res.AttachmentDeleteFailures, "chunks", res.Chunks,
				"max_tx_ms", res.MaxTx.Milliseconds(), "reached_cutoff", res.ReachedCutoff,
				"deleted_done", res.DeletedDone, "cutoff", p.MainCutoff, "grace_cutoff", p.GraceCutoff)
		}
	}
}

// chatRetentionBlobStore deletes an attachment's bytes; AttachmentStore
// satisfies it.
type chatRetentionBlobStore interface {
	Delete(ctx context.Context, projectID, id string) error
}

// chatRetentionSweeper runs ticks against a store. now and sleep are
// injectable so tests can drive the clock and observe pacing. attachments
// deletes the bytes of attachments a purge unlinked; when it is nil their
// metadata rows are left for the attachment GC.
type chatRetentionSweeper struct {
	store       store.ChatRetentionStore
	attachments chatRetentionBlobStore
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	// scanCap overrides chatRetentionFailedScanCap when positive (tests).
	scanCap int
}

func newChatRetentionSweeper(st store.ChatRetentionStore) *chatRetentionSweeper {
	return &chatRetentionSweeper{store: st, now: time.Now, sleep: sleepCtx}
}

// sleepCtx sleeps for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// chatRetentionTickResult describes one tick.
type chatRetentionTickResult struct {
	// Ran is false when the tick was skipped (inactive policy, or
	// tick_interval_minutes not yet passed).
	Ran bool
	// Chunks is the number of purge transactions committed (both phases).
	Chunks int
	// Purged is the total of the deleted rows.
	Purged store.ChatRetentionCounts
	// ReachedCutoff is true when the main walk returned a short chunk:
	// every eligible message older than the cutoff is gone.
	ReachedCutoff bool
	// DeletedDone is true when the deleted-conversation phase went
	// through every candidate: every eligible message of a conversation
	// or topic deleted before the grace cutoff is gone.
	DeletedDone bool
	// BudgetExhausted is true when the tick stopped at its budget.
	BudgetExhausted bool
	// MaxTx is the longest purge transaction.
	MaxTx time.Duration
	// ChunkSizes are the main walk's requested chunk sizes, in order.
	ChunkSizes []int
	// AttachmentDeleteFailures counts unlinked attachments whose bytes
	// could not be deleted; their rows stay for the attachment GC.
	AttachmentDeleteFailures int
	// AttachmentRelinked counts attachments linked again between the link
	// re-check and the byte delete: their bytes are gone, their rows kept.
	AttachmentRelinked int
	// LastAppliedCutoff is the cursor's last applied cutoff after the
	// tick (it only moves forward); nil when none is set yet.
	LastAppliedCutoff *time.Time
}

// nextChatRetentionChunk adapts the chunk size to the last transaction
// time (design §6.4).
func nextChatRetentionChunk(cur, max int, tx time.Duration) int {
	switch {
	case tx > chatRetentionShrinkAbove:
		cur /= 2
		if cur < chatRetentionMinChunk {
			cur = chatRetentionMinChunk
		}
	case tx < chatRetentionGrowBelow:
		next := cur + cur/4
		if next == cur {
			next++
		}
		cur = next
	}
	if cur > max {
		cur = max
	}
	return cur
}

// RunTick runs one tick under policy p. It returns early, with Ran false,
// when p purges nothing or the last tick was less than
// tick_interval_minutes ago.
func (sw *chatRetentionSweeper) RunTick(ctx context.Context, p chatRetentionPolicy) (chatRetentionTickResult, error) {
	var res chatRetentionTickResult
	if !p.purges() {
		return res, nil
	}
	cfg := p.Config
	start := sw.now()
	// The time left before ctx's deadline, measured at the same instant
	// as start, so budgetEnd applies both terms to one moment.
	remaining, hasDeadline := chatRetentionTimeLeft(ctx)

	run, err := sw.store.LatestChatRetentionRun(ctx, store.ChatRetentionTriggerSweep, start.Add(-chatRetentionRunWindow))
	if err != nil {
		return res, err
	}
	if run != nil && run.FinishedAt != nil {
		minGap := cfg.TickInterval() - cfg.TickBudget() - chatRetentionTickSlack
		if start.Sub(*run.FinishedAt) < minGap {
			return res, nil
		}
	}
	res.Ran = true
	if run == nil {
		run = &store.ChatRetentionRun{
			Trigger:   store.ChatRetentionTriggerSweep,
			StartedAt: start.UTC(),
			Status:    store.ChatRetentionStatusRunning,
		}
	}
	budgetEnd := chatRetentionBudgetEnd(start, remaining, hasDeadline, cfg)
	return res, sw.sweepAndRecord(ctx, p, run, &res, start, budgetEnd, chatRetentionSweepTick)
}

// chatRetentionSweepMode is how sweepAndRecord runs the two phases.
type chatRetentionSweepMode int

const (
	// chatRetentionSweepTick is a scheduled tick: the deleted-conversation
	// phase gets at most half of the time when the main walk follows, and
	// the main walk resumes at the cursor (reset to the start once a day).
	chatRetentionSweepTick chatRetentionSweepMode = iota
	// chatRetentionSweepManual is a manual run: each phase may use all of
	// the time, and the main walk starts at the beginning (the cursor is
	// reset as the daily reset does), so it picks up the messages a tick
	// skipped while they were pending or deferred, as a preview counts them.
	chatRetentionSweepManual
)

// sweepAndRecord runs both phases into run and res until budgetEnd, then
// records run (status, policy, finish time) and prunes old runs. mode
// decides how the time is shared and where the main walk starts.
func (sw *chatRetentionSweeper) sweepAndRecord(ctx context.Context, p chatRetentionPolicy, run *store.ChatRetentionRun, res *chatRetentionTickResult, start, budgetEnd time.Time, mode chatRetentionSweepMode) error {
	cfg := p.Config
	if run.Counts == nil {
		run.Counts = map[string]store.ChatRetentionCounts{}
	}
	hasWindow := !p.MainCutoff.IsZero()

	// The deleted-conversation phase first, with at most half of the tick
	// when the main walk follows: a long main backlog cannot hold back the
	// purge of deleted conversations, nor a large deleted conversation the
	// main walk.
	deletedEnd := budgetEnd
	if hasWindow && mode == chatRetentionSweepTick {
		deletedEnd = start.Add(budgetEnd.Sub(start) / 2)
	}
	pacer := &chatRetentionPacer{sw: sw, pause: cfg.Pause()}
	tickErr := sw.walkDeleted(ctx, p, run, res, pacer, deletedEnd)

	// A deleted-conversation failure does not hold back the main walk: it
	// is recorded on the run, and the main walk still runs unless the
	// tick's context is done.
	var cursor store.ChatRetentionCursor
	cursorDirty, mainRan := false, false
	if ctx.Err() == nil && hasWindow {
		// The pause owed by the last purge of the deleted-conversation
		// phase comes before the main walk's first chunk.
		ok, err := pacer.wait(ctx, budgetEnd)
		switch {
		case err != nil:
			tickErr = firstChatRetentionErr(tickErr, err)
		case !ok:
			res.BudgetExhausted = true
		default:
			cursor, err = sw.store.GetChatRetentionCursor(ctx, store.ChatRetentionPhaseMain)
			if err != nil {
				tickErr = firstChatRetentionErr(tickErr, err)
				break
			}
			mainRan = true
			if mode == chatRetentionSweepManual || cursor.ResetAt == nil || start.Sub(*cursor.ResetAt) >= chatRetentionCursorResetEvery {
				t := start.UTC()
				cursor.ResetAt = &t
				cursor.Position = store.ChatRetentionPosition{}
				cursorDirty = true
			}
			tickErr = firstChatRetentionErr(tickErr, sw.walk(ctx, p, &cursor, run, res, budgetEnd, &cursorDirty))
		}
	} else if tickErr == nil && !res.DeletedDone {
		res.BudgetExhausted = true
	}

	// The end-of-tick writes record what the tick committed even when ctx
	// is done (the scheduler's handler deadline, or a shutdown): they run
	// on a context that keeps ctx's values but not its cancellation, under
	// their own short timeout.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatRetentionFinalWriteTimeout)
	defer cancel()

	now := sw.now().UTC()
	if mainRan {
		// last_applied_cutoff only moves forward: after a longer window is
		// set, history still starts at the newer cutoff the walk already
		// reached.
		if res.ReachedCutoff && (cursor.LastAppliedCutoff == nil || p.MainCutoff.After(*cursor.LastAppliedCutoff)) {
			c := p.MainCutoff.UTC()
			cursor.LastAppliedCutoff = &c
			cursorDirty = true
		}
		res.LastAppliedCutoff = cursor.LastAppliedCutoff
		if cursorDirty {
			cursor.UpdatedAt = now
			if err := sw.store.SaveChatRetentionCursor(wctx, cursor); err != nil && tickErr == nil {
				tickErr = err
			}
		}
	}

	run.Policy = chatRetentionRunPolicy(p)
	run.FinishedAt = &now
	// A sweep run that failed stays failed, with its first error, until it
	// closes (24 h after it started); later clean ticks still add their
	// counts to it. The next run row starts clean.
	alreadyFailed := run.Status == store.ChatRetentionStatusFailed
	complete := res.DeletedDone && (!hasWindow || res.ReachedCutoff)
	switch {
	case alreadyFailed:
		if run.Error == "" && tickErr != nil {
			run.Error = tickErr.Error()
		}
	case tickErr != nil:
		run.Status = store.ChatRetentionStatusFailed
		run.Error = tickErr.Error()
	case complete:
		run.Status = store.ChatRetentionStatusOK
		run.Error = ""
	default:
		run.Status = store.ChatRetentionStatusPartial
		run.Error = ""
	}
	if err := sw.store.SaveChatRetentionRun(wctx, run); err != nil && tickErr == nil {
		tickErr = err
	}
	if _, err := sw.store.PruneChatRetentionRuns(wctx, now.Add(-chatRetentionRunKeep)); err != nil && tickErr == nil {
		tickErr = err
	}
	return tickErr
}

// chatRetentionRunPolicy is the policy recorded on a run row.
func chatRetentionRunPolicy(p chatRetentionPolicy) store.ChatRetentionPolicy {
	return store.ChatRetentionPolicy{
		MessagesRetentionDays:        p.Config.MessagesRetentionDays,
		MainCutoff:                   p.MainCutoff.UTC(),
		DeletedConversationGraceDays: p.GraceDays,
		GraceCutoff:                  p.GraceCutoff.UTC(),
	}
}

// firstChatRetentionErr returns first unless it is nil, then next.
func firstChatRetentionErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// chatRetentionTimeLeft returns the wall-clock time left before ctx's
// deadline, and false when ctx has none.
func chatRetentionTimeLeft(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}

// chatRetentionBudgetEnd is when the tick's walk must stop: start +
// tick_budget_seconds, or earlier when the tick's context has a deadline
// (remaining, measured at start), so that a last chunk (at most
// chatRetentionChunkTimeout) and the end-of-tick writes still fit before
// it. remaining is wall-clock time applied to the sweeper's clock.
func chatRetentionBudgetEnd(start time.Time, remaining time.Duration, hasDeadline bool, cfg opsettings.ChatRetentionConfig) time.Time {
	return chatRetentionWalkEnd(start, cfg.TickBudget(), remaining, hasDeadline)
}

// chatRetentionWalkEnd is start + budget, or earlier when the context has a
// deadline (remaining, measured at start), so that a last chunk and the
// final writes still fit before it.
func chatRetentionWalkEnd(start time.Time, budget, remaining time.Duration, hasDeadline bool) time.Time {
	end := start.Add(budget)
	if hasDeadline {
		if byDeadline := start.Add(remaining - chatRetentionChunkTimeout - chatRetentionDeadlineMargin); byDeadline.Before(end) {
			end = byDeadline
		}
	}
	return end
}

// walk runs the chunks of one tick, from cursor, until the walk reaches the
// cutoff, budgetEnd passes, the context ends or a chunk fails. A pause that
// would run past budgetEnd ends the tick instead.
func (sw *chatRetentionSweeper) walk(ctx context.Context, p chatRetentionPolicy, cursor *store.ChatRetentionCursor, run *store.ChatRetentionRun, res *chatRetentionTickResult, budgetEnd time.Time, cursorDirty *bool) error {
	cfg := p.Config
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
		done, tx, err := sw.chunk(ctx, p, cursor, run, res, chunk)
		if err != nil {
			return err
		}
		if done {
			res.ReachedCutoff = true
			return nil
		}
		*cursorDirty = false // the purge transaction stored the cursor
		chunk = nextChatRetentionChunk(chunk, cfg.ChunkSize, tx)
		if !sw.now().Add(cfg.Pause()).Before(budgetEnd) {
			res.BudgetExhausted = true
			return nil
		}
		if err := sw.sleep(ctx, cfg.Pause()); err != nil {
			return err
		}
	}
}

// chunk selects and purges one chunk. done is true when the walk returned
// fewer rows than requested (the cutoff is reached); tx is the purge
// transaction's duration.
func (sw *chatRetentionSweeper) chunk(ctx context.Context, p chatRetentionPolicy, cursor *store.ChatRetentionCursor, run *store.ChatRetentionRun, res *chatRetentionTickResult, limit int) (done bool, tx time.Duration, err error) {
	cctx, cancel := context.WithTimeout(ctx, chatRetentionChunkTimeout)
	defer cancel()

	cands, err := sw.store.SelectChatRetentionCandidates(cctx, p.MainCutoff, cursor.Position, limit)
	if err != nil {
		return false, 0, err
	}
	if len(cands) == 0 {
		return true, 0, nil
	}
	ids := make([]string, len(cands))
	projectOf := make(map[string]string, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
		projectOf[c.ID] = c.ProjectID
	}
	last := cands[len(cands)-1]
	next := *cursor
	next.Position = store.ChatRetentionPosition{Created: last.Created, ID: last.ID}
	next.UpdatedAt = sw.now().UTC()

	t0 := sw.now()
	pr, err := sw.store.PurgeChatMessages(cctx, ids, projectOf, next)
	tx = sw.now().Sub(t0)
	if err != nil {
		return false, tx, err
	}
	*cursor = next
	res.Chunks++
	if tx > res.MaxTx {
		res.MaxTx = tx
	}
	addChatRetentionCounts(run, res, pr.ByProject)
	addChatRetentionAttachments(run, res, sw.deleteAttachments(ctx, pr.UnlinkedAttachments))
	return len(cands) < limit, tx, nil
}

// addChatRetentionCounts adds per-project counts to the run and the tick
// result.
func addChatRetentionCounts(run *store.ChatRetentionRun, res *chatRetentionTickResult, byProject map[string]store.ChatRetentionCounts) {
	for project, c := range byProject {
		res.Purged.Add(c)
		acc := run.Counts[project]
		acc.Add(c)
		run.Counts[project] = acc
	}
}

// chatRetentionPacer keeps pause_ms between the purge transactions of the
// deleted-conversation phase, and between its last one and the main walk.
type chatRetentionPacer struct {
	sw    *chatRetentionSweeper
	pause time.Duration
	// owed is true after a purge, until the pause has been slept.
	owed bool
}

// wait sleeps the pause owed by the last purge, if any, and reports
// whether another chunk may start before end. A pause that would run past
// end ends the phase instead.
func (pc *chatRetentionPacer) wait(ctx context.Context, end time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if pc.owed {
		if !pc.sw.now().Add(pc.pause).Before(end) {
			return false, nil
		}
		if err := pc.sw.sleep(ctx, pc.pause); err != nil {
			return false, err
		}
		pc.owed = false
	}
	return pc.sw.now().Before(end), nil
}

// walkDeleted runs the deleted-conversation phase until every candidate is
// visited, end passes, the context ends or a candidate list fails.
// Candidates are the conversations deleted before the grace cutoff, then
// the topics deleted before it, each read in ID pages. A topic whose
// conversation was already a candidate is visited only for its legacy
// thread_id messages. A candidate whose purge fails is logged and skipped,
// so it cannot hold back the others; the first such error is returned and
// res.DeletedDone stays false.
func (sw *chatRetentionSweeper) walkDeleted(ctx context.Context, p chatRetentionPolicy, run *store.ChatRetentionRun, res *chatRetentionTickResult, pacer *chatRetentionPacer, end time.Time) error {
	chunk := p.Config.ChunkSize
	seen := map[string]bool{}
	var candidateErr error
	// visit drains one candidate. stop is true when the phase must end:
	// out of time, or the context is done (err is then set).
	visit := func(conversationID, threadID string) (stop bool, err error) {
		done, err := sw.drainDeleted(ctx, conversationID, threadID, run, res, pacer, end, &chunk, p.Config.ChunkSize)
		if err != nil {
			if ctx.Err() != nil {
				return true, err
			}
			slog.Warn("chat retention: deleted conversation purge failed; skipped this tick",
				"conversation", conversationID, "topic", threadID, "error", err)
			candidateErr = firstChatRetentionErr(candidateErr, err)
			return false, nil
		}
		return !done, nil
	}

	after := ""
	for {
		var ids []string
		err := chatRetentionWithTimeout(ctx, func(cctx context.Context) (err error) {
			ids, err = sw.store.ListChatRetentionDeletedConversations(cctx, p.GraceCutoff, after, chatRetentionCandidatePage)
			return err
		})
		if err != nil {
			// An earlier candidate's error stays the first one reported.
			return firstChatRetentionErr(candidateErr, err)
		}
		for _, id := range ids {
			seen[id] = true
			if stop, err := visit(id, ""); stop {
				return firstChatRetentionErr(candidateErr, err)
			}
		}
		if len(ids) < chatRetentionCandidatePage {
			break
		}
		after = ids[len(ids)-1]
	}

	after = ""
	for {
		var topics []store.ChatRetentionDeletedTopic
		err := chatRetentionWithTimeout(ctx, func(cctx context.Context) (err error) {
			topics, err = sw.store.ListChatRetentionDeletedTopics(cctx, p.GraceCutoff, after, chatRetentionCandidatePage)
			return err
		})
		if err != nil {
			return firstChatRetentionErr(candidateErr, err)
		}
		for _, t := range topics {
			conv := t.ConversationID
			if seen[conv] {
				conv = ""
			}
			if stop, err := visit(conv, t.ID); stop {
				return firstChatRetentionErr(candidateErr, err)
			}
		}
		if len(topics) < chatRetentionCandidatePage {
			break
		}
		after = topics[len(topics)-1].ID
	}
	res.DeletedDone = candidateErr == nil
	return candidateErr
}

// chatRetentionWithTimeout runs f under the chunk timeout.
func chatRetentionWithTimeout(ctx context.Context, f func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(ctx, chatRetentionChunkTimeout)
	defer cancel()
	return f(cctx)
}

// drainDeleted purges the messages of one deleted conversation (by
// conversationID, and by threadID for a legacy topic's messages) in
// chunks of at most *chunk messages, adapting *chunk to the transaction
// time up to maxChunk. done is false when the phase must stop first (end
// passed).
func (sw *chatRetentionSweeper) drainDeleted(ctx context.Context, conversationID, threadID string, run *store.ChatRetentionRun, res *chatRetentionTickResult, pacer *chatRetentionPacer, end time.Time, chunk *int, maxChunk int) (done bool, err error) {
	for {
		ok, err := pacer.wait(ctx, end)
		if err != nil || !ok {
			return false, err
		}
		limit := *chunk
		var cands []store.ChatRetentionCandidate
		var pr store.ChatRetentionPurgeResult
		var tx time.Duration
		err = chatRetentionWithTimeout(ctx, func(cctx context.Context) error {
			var err error
			cands, err = sw.store.SelectChatRetentionConversationMessages(cctx, conversationID, threadID, limit)
			if err != nil || len(cands) == 0 {
				return err
			}
			ids := make([]string, len(cands))
			projectOf := make(map[string]string, len(cands))
			for i, c := range cands {
				ids[i] = c.ID
				projectOf[c.ID] = c.ProjectID
			}
			t0 := sw.now()
			// No cursor: the phase starts from its first candidate
			// every tick.
			pr, err = sw.store.PurgeChatMessages(cctx, ids, projectOf, store.ChatRetentionCursor{})
			tx = sw.now().Sub(t0)
			return err
		})
		if err != nil {
			return false, err
		}
		if len(cands) == 0 {
			return true, nil
		}
		pacer.owed = true
		res.Chunks++
		if tx > res.MaxTx {
			res.MaxTx = tx
		}
		// The store counts every deleted message under Main; these were
		// deleted for their conversation's deletion.
		for project, c := range pr.ByProject {
			c.DeletedConv, c.Main = c.Main, 0
			pr.ByProject[project] = c
		}
		addChatRetentionCounts(run, res, pr.ByProject)
		addChatRetentionAttachments(run, res, sw.deleteAttachments(ctx, pr.UnlinkedAttachments))
		*chunk = nextChatRetentionChunk(*chunk, maxChunk, tx)
		if len(cands) < limit {
			return true, nil
		}
	}
}

// chatRetentionAttachmentResult is what deleteAttachments did.
type chatRetentionAttachmentResult struct {
	// ByProject counts the rows and bytes deleted per project
	// (AttachmentRows, AttachmentBytes).
	ByProject map[string]store.ChatRetentionCounts
	// Failures counts attachments whose bytes could not be deleted; their
	// rows stay for the attachment GC.
	Failures int
	// Relinked counts attachments linked again between the link re-check
	// and the byte delete: their bytes are gone, their rows kept.
	Relinked int
}

// addChatRetentionAttachments adds an attachment result to the run and the
// tick result.
func addChatRetentionAttachments(run *store.ChatRetentionRun, res *chatRetentionTickResult, ar chatRetentionAttachmentResult) {
	addChatRetentionCounts(run, res, ar.ByProject)
	res.AttachmentDeleteFailures += ar.Failures
	res.AttachmentRelinked += ar.Relinked
}

// deleteAttachments deletes, for each attachment a committed purge left
// with no message link, its bytes and then its metadata row (design
// §6.3). The order matters: the attachment GC needs the row to judge the
// bytes sweepable, so when the byte delete fails the row stays and the GC
// retries. An attachment linked again before the link re-check is
// skipped. One linked again between the re-check and the byte delete loses
// its bytes while its row is kept (the row delete is conditional on no
// link); that window is milliseconds, the legacy GC shares it, and it is
// logged. Without a blob store every row is left for the attachment GC.
// Failures are logged and never fail the purge.
func (sw *chatRetentionSweeper) deleteAttachments(ctx context.Context, atts []store.ChatRetentionAttachment) chatRetentionAttachmentResult {
	ar := chatRetentionAttachmentResult{ByProject: map[string]store.ChatRetentionCounts{}}
	if sw.attachments == nil {
		return ar
	}
	for _, a := range atts {
		if ctx.Err() != nil {
			return ar
		}
		// Attachment and project IDs are UUIDs the hub generated ("" for
		// an upload with no project); anything else is not a path to
		// hand to the blob store.
		if uuid.Validate(a.ID) != nil || (a.ProjectID != "" && uuid.Validate(a.ProjectID) != nil) {
			slog.Warn("chat retention: attachment with an unexpected ID left for the attachment GC", "attachment", a.ID)
			continue
		}
		_ = chatRetentionWithTimeout(ctx, func(cctx context.Context) error {
			linked, err := sw.store.ChatAttachmentLinked(cctx, a.ID)
			if err != nil {
				slog.Warn("chat retention: attachment link check failed; left for the attachment GC", "attachment", a.ID, "error", err)
				return nil
			}
			if linked {
				return nil
			}
			if err := sw.attachments.Delete(cctx, a.ProjectID, a.ID); err != nil {
				ar.Failures++
				slog.Warn("chat retention: attachment bytes not deleted; left for the attachment GC", "attachment", a.ID, "error", err)
				return nil
			}
			deleted, err := sw.store.DeleteUnlinkedChatAttachment(cctx, a.ID)
			if err != nil {
				slog.Warn("chat retention: attachment row not deleted after its bytes", "attachment", a.ID, "error", err)
				return nil
			}
			if !deleted {
				ar.Relinked++
				slog.Warn("chat retention: attachment bytes deleted but the attachment was linked again; row kept",
					"attachment", a.ID)
				return nil
			}
			acc := ar.ByProject[a.ProjectID]
			acc.Add(store.ChatRetentionCounts{AttachmentRows: 1, AttachmentBytes: a.Size})
			ar.ByProject[a.ProjectID] = acc
			return nil
		})
	}
	return ar
}

// chatHistoryRetentionMarker returns the retention marker for a history
// page (design §8.1), or nil. A page carries it when all of these hold:
//
//   - it is the oldest page (no nextCursor);
//   - chat retention is active (hub.chat_retention and enabled);
//   - the sweep has reached a cutoff (last applied cutoff set);
//   - the conversation started before that cutoff: the topic's creation
//     time for a topic, the conversation row's for a DM listed by
//     conversation; for a DM on the legacy thread path the start is not
//     known and no marker is shown. When the oldest message returned is
//     not newer than the cutoff (a pending or deferred message that was
//     kept), the page still reaches back past the cutoff and gets none.
//
// It costs no count: the policy and the cutoff are cached reads, and only
// a DM listed by conversation reads its conversation row by primary key.
func (s *Server) chatHistoryRetentionMarker(ctx context.Context, result *store.ListResult[store.Message], convID string, topic *WebChatTopic) *chatHistoryRetention {
	if result == nil || result.NextCursor != "" {
		return nil
	}
	now := time.Now()
	if !s.chatRetentionPolicyAt(now).Active {
		return nil
	}
	cutoff := s.chatRetentionLastAppliedCutoff(ctx, now)
	if cutoff == nil {
		return nil
	}
	if n := len(result.Items); n > 0 && !result.Items[n-1].CreatedAt.After(*cutoff) {
		return nil
	}
	var started time.Time
	switch {
	case topic != nil:
		started = topic.CreatedAt
	case convID != "":
		conv, err := s.store.GetConversation(ctx, convID)
		if err != nil || conv == nil {
			return nil
		}
		started = conv.CreatedAt
	default:
		return nil
	}
	if started.IsZero() || !started.Before(*cutoff) {
		return nil
	}
	return &chatHistoryRetention{PurgedBefore: cutoff.UTC()}
}
