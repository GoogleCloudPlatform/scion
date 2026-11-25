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

// Settled-decision retention (hub.decision_retention, design-8984 §3.3):
// a hub admin sets a hub-wide age window (minimum 30 days) in the
// decision_retention settings section, and the "decision-retention" singleton
// purges whole prompt messages whose every decision is settled and inactive
// past the cutoff, oldest first, in paced transactions.

const (
	// decisionRetentionPageTimeout bounds the candidate scan and purge
	// transaction of one page.
	decisionRetentionPageTimeout = 5 * time.Second
	// Adaptive page sizing thresholds (matching chat retention): a page tx
	// slower than 250 ms halves pageSize (down to DecisionRetentionMinPageSize);
	// one faster than 50 ms grows it by 25% (up to cfg.PageSize).
	decisionRetentionShrinkAbove = 250 * time.Millisecond
	decisionRetentionGrowBelow   = 50 * time.Millisecond
	decisionRetentionMinPage     = opsettings.DecisionRetentionMinPageSize
	// decisionRetentionSQLiteYield pauses briefly between pages on SQLite so
	// interactive writes interleave cleanly on the single writer.
	decisionRetentionSQLiteYield = 5 * time.Millisecond
	// decisionRetentionCursorResetEvery is how often the (created_at, id)
	// cursor resets to the start of the walk so previously ineligible rows
	// (e.g. a message with a once-open sibling) are revisited.
	decisionRetentionCursorResetEvery = 24 * time.Hour
	// decisionRetentionRunWindow is how long a sweep run row stays open and
	// accumulates counts of later ticks.
	decisionRetentionRunWindow = 24 * time.Hour
	// decisionRetentionRunKeep is how long decision_retention_runs rows are kept.
	decisionRetentionRunKeep = 365 * 24 * time.Hour
	// decisionRetentionDeadlineMargin leaves room before the scheduler handler
	// deadline for end-of-tick writes.
	decisionRetentionDeadlineMargin = 2 * time.Second
	// decisionRetentionFinalWriteTimeout bounds end-of-tick cursor, run and
	// prune writes.
	decisionRetentionFinalWriteTimeout = 5 * time.Second
	// decisionRetentionTickSlack absorbs scheduler jitter when checking whether
	// tick_interval_minutes has passed since the last tick.
	decisionRetentionTickSlack = 5 * time.Second
)

// decisionRetentionPolicy is the policy snapshot of one tick.
type decisionRetentionPolicy struct {
	// Active is true iff experiment is on && enabled && a window is set.
	Active bool
	Config opsettings.DecisionRetentionConfig
	Now    time.Time
	// Cutoff is now - retention_days; zero when not active.
	Cutoff time.Time
}

func newDecisionRetentionPolicy(experimentOn bool, cfg opsettings.DecisionRetentionConfig, now time.Time) decisionRetentionPolicy {
	p := decisionRetentionPolicy{
		Active: experimentOn && cfg.Enabled && cfg.HasWindow(),
		Config: cfg,
		Now:    now.UTC(),
	}
	if !p.Active {
		return p
	}
	p.Cutoff = p.Now.Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
	return p
}

func (s *Server) decisionRetentionConfig() opsettings.DecisionRetentionConfig {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.DecisionRetention()
	}
	return opsettings.DefaultDecisionRetentionConfig()
}

func (s *Server) decisionRetentionPolicyAt(now time.Time) decisionRetentionPolicy {
	return newDecisionRetentionPolicy(s.experimentEnabled(experiments.DecisionRetention), s.decisionRetentionConfig(), now)
}

// decisionRetentionState holds the Server's decision retention wiring.
type decisionRetentionState struct {
	mu      sync.Mutex
	store   store.DecisionRetentionStore
	running atomic.Bool
}

// SetDecisionRetentionStore wires the decision retention store.
func (s *Server) SetDecisionRetentionStore(st store.DecisionRetentionStore) {
	s.decisionRetention.mu.Lock()
	s.decisionRetention.store = st
	s.decisionRetention.mu.Unlock()
}

func (s *Server) decisionRetentionStore() store.DecisionRetentionStore {
	s.decisionRetention.mu.Lock()
	defer s.decisionRetention.mu.Unlock()
	return s.decisionRetention.store
}

// decisionRetentionHandler is the "decision-retention" recurring singleton.
// Registered every minute; RunTick skips ticks until tick_interval_minutes have
// passed since the last one.
func (s *Server) decisionRetentionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		st := s.decisionRetentionStore()
		if st == nil {
			return
		}
		p := s.decisionRetentionPolicyAt(time.Now())
		if !p.Active {
			return
		}
		if !s.decisionRetention.running.CompareAndSwap(false, true) {
			return
		}
		defer s.decisionRetention.running.Store(false)

		sw := newDecisionRetentionSweeper(st, s.isSQLiteStore())
		res, err := sw.RunTick(ctx, p)
		if err != nil {
			slog.Error("decision retention: tick failed", "error", err,
				"pages", res.Pages, "decisions", res.Purged.Decisions, "answers", res.Purged.Answers)
			return
		}
		if res.Ran && (res.Purged.Decisions > 0 || res.Purged.Answers > 0) {
			slog.Info("decision retention: purged settled decisions",
				"decisions", res.Purged.Decisions, "answers", res.Purged.Answers,
				"messages", res.PurgedMessages, "skipped_messages", res.SkippedMessages,
				"pages", res.Pages, "max_tx_ms", res.MaxTx.Milliseconds(),
				"reached_cutoff", res.ReachedCutoff, "cutoff", p.Cutoff)
		}
	}
}

// decisionRetentionSweeper executes ticks against a DecisionRetentionStore.
type decisionRetentionSweeper struct {
	store    store.DecisionRetentionStore
	isSQLite bool
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
}

func newDecisionRetentionSweeper(st store.DecisionRetentionStore, isSQLite bool) *decisionRetentionSweeper {
	return &decisionRetentionSweeper{
		store:    st,
		isSQLite: isSQLite,
		now:      time.Now,
		sleep:    sleepCtx,
	}
}

type decisionRetentionTickResult struct {
	Ran             bool
	Pages           int
	PurgedMessages  int
	SkippedMessages int
	Purged          store.DecisionRetentionCounts
	ReachedCutoff   bool
	BudgetExhausted bool
	MaxTx           time.Duration
	PageSizes       []int
}

func nextDecisionRetentionPage(cur, max int, tx time.Duration) int {
	switch {
	case tx > decisionRetentionShrinkAbove:
		cur /= 2
		if cur < decisionRetentionMinPage {
			cur = decisionRetentionMinPage
		}
	case tx < decisionRetentionGrowBelow:
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

func decisionRetentionBudgetEnd(start time.Time, remaining time.Duration, hasDeadline bool, cfg opsettings.DecisionRetentionConfig) time.Time {
	end := start.Add(cfg.TickBudget())
	if hasDeadline {
		if byDeadline := start.Add(remaining - decisionRetentionPageTimeout - decisionRetentionDeadlineMargin); byDeadline.Before(end) {
			end = byDeadline
		}
	}
	return end
}

// RunTick runs one retention tick under policy p. Returns early with Ran=false
// when p is inactive or tick_interval_minutes has not yet elapsed.
func (sw *decisionRetentionSweeper) RunTick(ctx context.Context, p decisionRetentionPolicy) (decisionRetentionTickResult, error) {
	var res decisionRetentionTickResult
	if !p.Active || p.Cutoff.IsZero() {
		return res, nil
	}
	cfg := p.Config
	start := sw.now()
	remaining, hasDeadline := chatRetentionTimeLeft(ctx)

	run, err := sw.store.LatestDecisionRetentionRun(ctx, store.DecisionRetentionTriggerSweep, start.Add(-decisionRetentionRunWindow))
	if err != nil {
		return res, err
	}
	if run != nil && run.FinishedAt != nil {
		minGap := cfg.TickInterval() - cfg.TickBudget() - decisionRetentionTickSlack
		if start.Sub(*run.FinishedAt) < minGap {
			return res, nil
		}
	}
	res.Ran = true
	if run == nil {
		run = &store.DecisionRetentionRun{
			Trigger:   store.DecisionRetentionTriggerSweep,
			StartedAt: start.UTC(),
			Status:    store.DecisionRetentionStatusRunning,
			Counts:    map[string]store.DecisionRetentionCounts{},
		}
	}
	if run.Counts == nil {
		run.Counts = map[string]store.DecisionRetentionCounts{}
	}

	budgetEnd := decisionRetentionBudgetEnd(start, remaining, hasDeadline, cfg)
	return res, sw.sweepAndRecord(ctx, p, run, &res, start, budgetEnd)
}

func (sw *decisionRetentionSweeper) sweepAndRecord(
	ctx context.Context,
	p decisionRetentionPolicy,
	run *store.DecisionRetentionRun,
	res *decisionRetentionTickResult,
	start, budgetEnd time.Time,
) error {
	cursor, tickErr := sw.store.GetDecisionRetentionCursor(ctx)
	cursorDirty := false
	if tickErr == nil {
		if cursor.ResetAt == nil || start.Sub(*cursor.ResetAt) >= decisionRetentionCursorResetEvery {
			t := start.UTC()
			cursor.ResetAt = &t
			cursor.Position = store.DecisionRetentionPosition{}
			cursorDirty = true
		}
		tickErr = sw.walk(ctx, p, &cursor, run, res, budgetEnd, &cursorDirty)
	}

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), decisionRetentionFinalWriteTimeout)
	defer cancel()

	now := sw.now().UTC()
	if cursorDirty {
		cursor.UpdatedAt = now
		if err := sw.store.SaveDecisionRetentionCursor(wctx, cursor); err != nil && tickErr == nil {
			tickErr = err
		}
	}

	run.Policy = store.DecisionRetentionPolicy{
		RetentionDays: p.Config.RetentionDays,
		Cutoff:        p.Cutoff.UTC(),
	}
	run.FinishedAt = &now
	alreadyFailed := run.Status == store.DecisionRetentionStatusFailed
	switch {
	case alreadyFailed:
		if run.Error == "" && tickErr != nil {
			run.Error = tickErr.Error()
		}
	case tickErr != nil:
		run.Status = store.DecisionRetentionStatusFailed
		run.Error = tickErr.Error()
	case res.ReachedCutoff:
		run.Status = store.DecisionRetentionStatusOK
		run.Error = ""
	default:
		run.Status = store.DecisionRetentionStatusPartial
		run.Error = ""
	}
	if err := sw.store.SaveDecisionRetentionRun(wctx, run); err != nil && tickErr == nil {
		tickErr = err
	}
	if _, err := sw.store.PruneDecisionRetentionRuns(wctx, now.Add(-decisionRetentionRunKeep)); err != nil && tickErr == nil {
		tickErr = err
	}
	return tickErr
}

func (sw *decisionRetentionSweeper) walk(
	ctx context.Context,
	p decisionRetentionPolicy,
	cursor *store.DecisionRetentionCursor,
	run *store.DecisionRetentionRun,
	res *decisionRetentionTickResult,
	budgetEnd time.Time,
	cursorDirty *bool,
) error {
	cfg := p.Config
	pageSize := cfg.PageSize
	if pageSize < decisionRetentionMinPage {
		pageSize = opsettings.DecisionRetentionDefaultPageSize
	}
	globalCutoff := p.Cutoff.UTC()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !sw.now().Before(budgetEnd) {
			res.BudgetExhausted = true
			return nil
		}
		res.PageSizes = append(res.PageSizes, pageSize)

		pctx, cancel := context.WithTimeout(ctx, decisionRetentionPageTimeout)
		cands, err := sw.store.SelectDecisionRetentionCandidates(pctx, globalCutoff, cursor.Position, pageSize)
		if err != nil {
			cancel()
			return err
		}
		if len(cands) == 0 {
			cancel()
			res.ReachedCutoff = true
			return nil
		}

		// Collect distinct message IDs and per-project cutoffs.
		seenMsg := make(map[string]bool, len(cands))
		msgIDs := make([]string, 0, len(cands))
		cutoffByProject := make(map[string]time.Time)
		for _, c := range cands {
			if !seenMsg[c.MessageID] {
				seenMsg[c.MessageID] = true
				msgIDs = append(msgIDs, c.MessageID)
			}
			cutoffByProject[c.ProjectID] = globalCutoff
		}

		t0 := sw.now()
		pr, err := sw.store.PurgeDecisionMessages(pctx, msgIDs, cutoffByProject)
		tx := sw.now().Sub(t0)
		cancel()
		if err != nil {
			return err
		}

		last := cands[len(cands)-1]
		cursor.Position = store.DecisionRetentionPosition{CreatedAt: last.CreatedAt, ID: last.ID}
		*cursorDirty = true

		res.Pages++
		res.PurgedMessages += pr.PurgedMessages
		res.SkippedMessages += pr.SkippedMessages
		if tx > res.MaxTx {
			res.MaxTx = tx
		}
		for pid, c := range pr.ByProject {
			res.Purged.Add(c)
			acc := run.Counts[pid]
			acc.Add(c)
			run.Counts[pid] = acc
		}

		if len(cands) < pageSize {
			res.ReachedCutoff = true
			return nil
		}

		pageSize = nextDecisionRetentionPage(pageSize, cfg.PageSize, tx)
		if sw.isSQLite {
			if !sw.now().Add(decisionRetentionSQLiteYield).Before(budgetEnd) {
				res.BudgetExhausted = true
				return nil
			}
			if err := sw.sleep(ctx, decisionRetentionSQLiteYield); err != nil {
				return err
			}
		}
	}
}
