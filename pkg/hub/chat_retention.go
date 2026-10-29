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
// experiment is on and the section has enabled=true and a window; otherwise
// it purges nothing. Within a tick, the walk over the created index resumes
// from a persisted (created, id) cursor, which is reset to the start once a
// day so messages skipped while pending or deferred are picked up once
// dispatched. Every chunk is one purge transaction with a 5 s timeout; the
// chunk size adapts to the transaction time, the sweeper pauses pause_ms
// after each chunk, and the tick stops after tick_budget_seconds. The open
// sweep run (one chat_retention_runs row per 24 h) accumulates per-project
// counts and the policy, never message IDs or content.

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
}

// purges reports whether the policy deletes anything in Phase 1 (the main
// window only).
func (p chatRetentionPolicy) purges() bool { return p.Active && !p.MainCutoff.IsZero() }

// newChatRetentionPolicy resolves the policy for one tick at now.
func newChatRetentionPolicy(experimentOn bool, cfg opsettings.ChatRetentionConfig, now time.Time) chatRetentionPolicy {
	p := chatRetentionPolicy{Active: experimentOn && cfg.Enabled, Config: cfg, Now: now.UTC()}
	if p.Active && cfg.HasWindow() {
		p.MainCutoff = p.Now.Add(-time.Duration(cfg.MessagesRetentionDays) * 24 * time.Hour)
	}
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
		res, err := sw.RunTick(ctx, p)
		if res.LastAppliedCutoff != nil {
			s.noteChatRetentionCutoff(*res.LastAppliedCutoff, time.Now())
		}
		if err != nil {
			slog.Error("chat retention: tick failed", "error", err, "chunks", res.Chunks, "purged", res.Purged.Main)
			return
		}
		if res.Ran && res.Purged.Main > 0 {
			slog.Info("chat retention: purged messages", "purged", res.Purged.Main, "chunks", res.Chunks,
				"max_tx_ms", res.MaxTx.Milliseconds(), "reached_cutoff", res.ReachedCutoff, "cutoff", p.MainCutoff)
		}
	}
}

// chatRetentionSweeper runs ticks against a store. now and sleep are
// injectable so tests can drive the clock and observe pacing.
type chatRetentionSweeper struct {
	store store.ChatRetentionStore
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
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
	// Chunks is the number of purge transactions committed.
	Chunks int
	// Purged is the total of the deleted rows.
	Purged store.ChatRetentionCounts
	// ReachedCutoff is true when the walk returned a short chunk: every
	// eligible message older than the cutoff is gone.
	ReachedCutoff bool
	// BudgetExhausted is true when the tick stopped at its budget.
	BudgetExhausted bool
	// MaxTx is the longest purge transaction.
	MaxTx time.Duration
	// ChunkSizes are the requested chunk sizes, in order.
	ChunkSizes []int
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
	if run.Counts == nil {
		run.Counts = map[string]store.ChatRetentionCounts{}
	}

	cursor, err := sw.store.GetChatRetentionCursor(ctx, store.ChatRetentionPhaseMain)
	if err != nil {
		return res, err
	}
	cursorDirty := false
	if cursor.ResetAt == nil || start.Sub(*cursor.ResetAt) >= chatRetentionCursorResetEvery {
		t := start.UTC()
		cursor.ResetAt = &t
		cursor.Position = store.ChatRetentionPosition{}
		cursorDirty = true
	}

	tickErr := sw.walk(ctx, p, &cursor, run, &res, chatRetentionBudgetEnd(start, remaining, hasDeadline, cfg), &cursorDirty)

	// The end-of-tick writes record what the tick committed even when ctx
	// is done (the scheduler's handler deadline, or a shutdown): they run
	// on a context that keeps ctx's values but not its cancellation, under
	// their own short timeout.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatRetentionFinalWriteTimeout)
	defer cancel()

	now := sw.now().UTC()
	// last_applied_cutoff only moves forward: after a longer window is set,
	// history still starts at the newer cutoff the walk already reached.
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

	run.Policy = store.ChatRetentionPolicy{MessagesRetentionDays: cfg.MessagesRetentionDays, MainCutoff: p.MainCutoff.UTC()}
	run.FinishedAt = &now
	// A sweep run that failed stays failed, with its first error, until it
	// closes (24 h after it started); later clean ticks still add their
	// counts to it. The next run row starts clean.
	alreadyFailed := run.Status == store.ChatRetentionStatusFailed
	switch {
	case alreadyFailed:
		if run.Error == "" && tickErr != nil {
			run.Error = tickErr.Error()
		}
	case tickErr != nil:
		run.Status = store.ChatRetentionStatusFailed
		run.Error = tickErr.Error()
	case res.ReachedCutoff:
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
	return res, tickErr
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
	end := start.Add(cfg.TickBudget())
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
	for project, c := range pr.ByProject {
		res.Purged.Add(c)
		acc := run.Counts[project]
		acc.Add(c)
		run.Counts[project] = acc
	}
	return len(cands) < limit, tx, nil
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
