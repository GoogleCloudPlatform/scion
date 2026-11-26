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
	// decisionRetentionReconcileTimeout bounds startup reconciliation of
	// interrupted running rows.
	decisionRetentionReconcileTimeout = 10 * time.Second
	// decisionRetentionRunMaxAge is the fallback age threshold for failing
	// running rows when another replica currently holds LockDecisionRetention.
	decisionRetentionRunMaxAge = decisionRetentionRunWindow + time.Hour
	// decisionRetentionInterruptedError is recorded on running rows failed by
	// ReconcileInterruptedDecisionRetentionRuns after a crash/restart.
	decisionRetentionInterruptedError = "interrupted by hub restart"
)

// decisionRetentionPolicy is the policy snapshot of one tick, preview, or
// manual run.
type decisionRetentionPolicy struct {
	// Active is true iff experiment is on && enabled && at least one project
	// has an effective window (hub default or a live days override).
	Active bool
	Config opsettings.DecisionRetentionConfig
	Now    time.Time
	// GlobalCutoff is now - min(window in use); zero when no window is set.
	GlobalCutoff time.Time
}

func newDecisionRetentionPolicy(experimentOn bool, cfg opsettings.DecisionRetentionConfig, now time.Time) decisionRetentionPolicy {
	minDays, hasWindow := cfg.MinEffectiveDays()
	p := decisionRetentionPolicy{
		Active: experimentOn && cfg.Enabled && hasWindow,
		Config: cfg,
		Now:    now.UTC(),
	}
	if !p.Active {
		return p
	}
	p.GlobalCutoff = p.Now.Add(-time.Duration(minDays) * 24 * time.Hour)
	return p
}

// CutoffForProject returns the retention cutoff timestamp for projectID under
// p, or (zero, false) when p is inactive, the project is held (keep_forever),
// or no effective window applies.
func (p decisionRetentionPolicy) CutoffForProject(projectID string) (time.Time, bool) {
	if !p.Active {
		return time.Time{}, false
	}
	days, ok := p.Config.EffectiveDays(projectID)
	if !ok {
		return time.Time{}, false
	}
	return p.Now.Add(-time.Duration(days) * 24 * time.Hour), true
}

func decisionRetentionRunPolicy(p decisionRetentionPolicy) store.DecisionRetentionPolicy {
	out := store.DecisionRetentionPolicy{
		RetentionDays: p.Config.RetentionDays,
		Cutoff:        p.GlobalCutoff.UTC(),
	}
	if len(p.Config.ProjectOverrides) > 0 {
		out.ProjectOverrides = make(map[string]store.DecisionRetentionProjectOverride, len(p.Config.ProjectOverrides))
		for pid, ov := range p.Config.ProjectOverrides {
			var days *int
			if ov.Days != nil {
				d := *ov.Days
				days = &d
			}
			out.ProjectOverrides[pid] = store.DecisionRetentionProjectOverride{
				Mode: ov.Mode,
				Days: days,
			}
		}
	}
	return out
}

func (s *Server) decisionRetentionConfig() opsettings.DecisionRetentionConfig {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.DecisionRetention()
	}
	return opsettings.DefaultDecisionRetentionConfig()
}

// decisionRetentionConfigForJob returns the resolved decision_retention config
// with overrides for deleted projects stripped so the job and maintenance ops
// ignore them (design-8984 §3.1).
func (s *Server) decisionRetentionConfigForJob(ctx context.Context) (opsettings.DecisionRetentionConfig, error) {
	cfg := s.decisionRetentionConfig()
	if len(cfg.ProjectOverrides) == 0 {
		return cfg, nil
	}
	live, err := s.filterLiveProjectOverrides(ctx, cfg.ProjectOverrides)
	if err != nil {
		return opsettings.DecisionRetentionConfig{}, err
	}
	cfg.ProjectOverrides = live
	return cfg, nil
}

func (s *Server) decisionRetentionPolicyAt(ctx context.Context, now time.Time) (decisionRetentionPolicy, error) {
	if !s.experimentEnabled(experiments.DecisionRetention) {
		return decisionRetentionPolicy{Now: now.UTC()}, nil
	}
	cfg, err := s.decisionRetentionConfigForJob(ctx)
	if err != nil {
		return decisionRetentionPolicy{Now: now.UTC()}, err
	}
	return newDecisionRetentionPolicy(true, cfg, now), nil
}

// decisionRetentionState holds the Server's decision retention wiring.
type decisionRetentionState struct {
	mu        sync.Mutex
	store     store.DecisionRetentionStore
	running   atomic.Bool
	sleepHook func(context.Context, time.Duration) error // test seam for inter-page yield
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

func (s *Server) tryDecisionRetentionLock(ctx context.Context) (func(), bool, error) {
	locker, ok := s.store.(store.AdvisoryLocker)
	if !ok {
		return func() {}, true, nil
	}
	acquired, release, err := locker.TryAdvisoryLock(ctx, store.LockDecisionRetention)
	if err != nil {
		return nil, false, err
	}
	if !acquired {
		_ = release()
		return nil, false, nil
	}
	return func() { _ = release() }, true, nil
}

// ReconcileInterruptedDecisionRetentionRuns marks any decision_retention_runs
// rows left in status=running by a crash/restart as failed so they never stay
// stuck in running if retention is subsequently disabled or idle. Call once at
// startup after SetDecisionRetentionStore.
//
// When this process acquires LockDecisionRetention, no replica is actively
// executing a sweep, manual run, or preview, so every running row started
// before this process's startTime is marked failed. If another replica
// currently holds the lock, only rows older than decisionRetentionRunMaxAge
// are failed so a live run on the lock-holding replica is not prematurely
// marked failed.
func (s *Server) ReconcileInterruptedDecisionRetentionRuns(ctx context.Context) int {
	st := s.decisionRetentionStore()
	if st == nil {
		return 0
	}
	s.mu.RLock()
	started := s.startTime
	s.mu.RUnlock()
	now := time.Now()
	if started.IsZero() {
		started = now
	}
	ctx, cancel := context.WithTimeout(ctx, decisionRetentionReconcileTimeout)
	defer cancel()

	lockedBefore := now.Add(-decisionRetentionRunMaxAge)
	if release, acquired, err := s.tryDecisionRetentionLock(ctx); err != nil {
		slog.Warn("decision retention: reconcile: advisory lock", "error", err)
	} else if acquired {
		lockedBefore = started
		defer release()
	}
	if lockedBefore.After(started) {
		lockedBefore = started
	}

	allTriggers := []string{
		store.DecisionRetentionTriggerSweep,
		store.DecisionRetentionTriggerManual,
		store.DecisionRetentionTriggerPreview,
	}
	total, err := st.FailRunningDecisionRetentionRuns(ctx, allTriggers, lockedBefore, decisionRetentionInterruptedError, now)
	if err != nil {
		slog.Warn("decision retention: reconcile interrupted runs", "triggers", allTriggers, "error", err)
		return 0
	}
	if total > 0 {
		slog.Warn("decision retention: marked interrupted runs failed", "count", total)
	}
	return total
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
		p, err := s.decisionRetentionPolicyAt(ctx, time.Now())
		if err != nil {
			slog.Error("decision retention: failed to resolve policy", "error", err)
			return
		}
		if !p.Active {
			return
		}
		if !s.decisionRetention.running.CompareAndSwap(false, true) {
			return
		}
		defer s.decisionRetention.running.Store(false)

		sw := newDecisionRetentionSweeper(st, s.isSQLiteStore())
		sw.policy = s.decisionRetentionPolicyAt
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
				"reached_cutoff", res.ReachedCutoff, "global_cutoff", p.GlobalCutoff)
		}
	}
}

// decisionRetentionSweeper executes ticks against a DecisionRetentionStore.
type decisionRetentionSweeper struct {
	store    store.DecisionRetentionStore
	isSQLite bool
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
	// policy, when non-nil, is re-read before each page so a mid-tick hold
	// (experiment off, enabled=false, or cleared window) stops further pages
	// immediately instead of waiting for the next tick.
	policy func(ctx context.Context, now time.Time) (decisionRetentionPolicy, error)
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
	if !p.Active || p.GlobalCutoff.IsZero() {
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
		// Initialize ResetAt on the first run; thereafter reset to the start of
		// the walk only once the walk has actually reached the cutoff since the
		// last reset AND at least 24h have elapsed. Resetting unconditionally
		// would let a backlog of ineligible old rows (open siblings, unindexed
		// rows, or held projects) larger than one day's scan budget starve newer
		// eligible rows forever.
		if cursor.ResetAt == nil {
			t := start.UTC()
			cursor.ResetAt = &t
			cursorDirty = true
		} else if cursor.ReachedCutoff && start.Sub(*cursor.ResetAt) >= decisionRetentionCursorResetEvery {
			t := start.UTC()
			cursor.ResetAt = &t
			cursor.Position = store.DecisionRetentionPosition{}
			cursor.ReachedCutoff = false
			cursorDirty = true
		}
		tickErr = sw.walk(ctx, &p, &cursor, run, res, budgetEnd, &cursorDirty)
		if tickErr == nil && res.ReachedCutoff && !cursor.ReachedCutoff {
			cursor.ReachedCutoff = true
			cursorDirty = true
		}
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

	run.Policy = decisionRetentionRunPolicy(p)
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
	p *decisionRetentionPolicy,
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
	globalCutoff := p.GlobalCutoff.UTC()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sw.policy != nil {
			curP, pErr := sw.policy(ctx, sw.now())
			if pErr != nil {
				return pErr
			}
			if !curP.Active || curP.GlobalCutoff.IsZero() {
				return nil
			}
			*p = curP
			cfg = curP.Config
			globalCutoff = curP.GlobalCutoff.UTC()
			if cfg.PageSize >= decisionRetentionMinPage && pageSize > cfg.PageSize {
				pageSize = cfg.PageSize
			}
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

		// Collect distinct message IDs and per-project cutoffs, applying each
		// project's effective window in Go (design-8984 §3.3). Projects held
		// via keep_forever or without a window are skipped.
		seenMsg := make(map[string]bool, len(cands))
		msgIDs := make([]string, 0, len(cands))
		cutoffByProject := make(map[string]time.Time)
		for _, c := range cands {
			cutoff, ok := p.CutoffForProject(c.ProjectID)
			if !ok || !c.CreatedAt.Before(cutoff) {
				continue
			}
			if !seenMsg[c.MessageID] {
				seenMsg[c.MessageID] = true
				msgIDs = append(msgIDs, c.MessageID)
			}
			cutoffByProject[c.ProjectID] = cutoff
		}

		var pr store.DecisionRetentionPurgeResult
		var tx time.Duration
		if len(msgIDs) > 0 {
			t0 := sw.now()
			pr, err = sw.store.PurgeDecisionMessages(pctx, msgIDs, cutoffByProject)
			tx = sw.now().Sub(t0)
		}
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

		// Persist cumulative counts after every purged page so a mid-tick
		// crash never leaves committed deletions without an audit record.
		if pr.PurgedMessages > 0 {
			run.Policy = decisionRetentionRunPolicy(*p)
			if run.Status != store.DecisionRetentionStatusFailed {
				run.Status = store.DecisionRetentionStatusRunning
			}
			run.FinishedAt = nil
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), decisionRetentionFinalWriteTimeout)
			saveErr := sw.store.SaveDecisionRetentionRun(rctx, run)
			rcancel()
			if saveErr != nil {
				return saveErr
			}
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
