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
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	usageRetentionJobName           = "usage-retention"
	defaultUsageRetentionBatchYield = 10 * time.Millisecond
	// defaultUsageRetentionTickBudget bounds one 1-minute scheduler tick safely
	// below the scheduler's 55s handler timeout, leaving room to persist shared
	// sweeper state in hub_settings (R1).
	defaultUsageRetentionTickBudget = 45 * time.Second
	// usageRetentionStateWriteTimeout bounds the final hub_settings state save
	// even if the tick deadline fired.
	usageRetentionStateWriteTimeout = 5 * time.Second
	// usageRetentionTickMargin reserves time before an external ctx deadline
	// so state persistence completes cleanly.
	usageRetentionTickMargin = 2 * time.Second
)

// UsageExportCursorProvider returns the analytics export watermark for
// usage_events (Tokenomics P8 hook, D8/O1).
//   - When no provider is installed (nil), usage_events is swept by age only.
//   - When a provider IS installed, it fails closed: a non-nil error OR a nil
//     cursor (meaning nothing has been exported yet) skips usage_events without
//     deleting any rows.
type UsageExportCursorProvider interface {
	UsageExportCursor(ctx context.Context) (*store.UsageExportCursor, error)
}

// UsageRetentionTarget describes one Hub DB table managed by
// UsageRetentionSweeper. Later phases (P3: usage_group_rollups_daily,
// usage_budget_state; P5: trajectory_*) register additional targets without
// modifying the core batching/yield/observability loop.
type UsageRetentionTarget struct {
	// Name identifies the table in logs and reports.
	Name string
	// RetentionDays returns the effective retention window in days for this
	// table from cfg. Returning <= 0 means unlimited retention (skip).
	RetentionDays func(cfg config.ResolvedTelemetryRetention) int
	// ComputeCutoff optionally overrides the default (now - retentionDays*24h)
	// cutoff computation (e.g. UTC hour/day truncation or calendar-month
	// preservation for usage_scope_counters_daily).
	ComputeCutoff func(now time.Time, retentionDays int) time.Time
	// SweepBatch deletes at most batchSize expired rows older than cutoff in a
	// single transaction and returns the deleted row count.
	SweepBatch func(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)
}

// UsageRetentionTableReport records the outcome of sweeping one table in a tick.
type UsageRetentionTableReport struct {
	Table         string
	Deleted       int64
	Batches       int
	Duration      time.Duration
	Skipped       bool
	SkippedReason string
	Drained       bool
	Cutoff        time.Time
	Err           error
}

// UsageRetentionRunReport summarizes one tick (or full pass) of UsageRetentionSweeper.
type UsageRetentionRunReport struct {
	StartedAt     time.Time
	Duration      time.Duration
	TotalDeleted  int64
	ErrorCount    int
	Skipped       bool // true when sweep_interval has not elapsed since last completed pass or experiment is off
	PassCompleted bool // true when every target drained and LastCompletedAt was updated
	Tables        []UsageRetentionTableReport
}

// UsageRetentionSweeper executes bounded-batch retention sweeps across all
// registered Track 1 (and future Track 2) usage tables. Follows the
// decision-retention pattern: registered on a 1-minute tick, persists shared
// pass progress and the hourly swept-through watermark in hub_settings, bounds
// work per tick below the scheduler timeout, and prevents any single table's
// backlog from starving later tables via per-target time slices and round-robin
// resumption across ticks.
type UsageRetentionSweeper struct {
	mu                   sync.RWMutex
	store                store.UsageRetentionStore
	targets              []UsageRetentionTarget
	batchYield           time.Duration
	tickBudget           time.Duration
	nowFn                func(ctx context.Context) (time.Time, error)
	isEnabledFn          func() bool
	exportCursorProvider UsageExportCursorProvider
}

// NewUsageRetentionSweeper constructs a sweeper pre-registered with the six
// existing Track 1 usage tables in dependency order (usage_events,
// usage_rollups_hourly, usage_rollups_daily, usage_scope_counters_daily,
// meter_health, run_attribution).
func NewUsageRetentionSweeper(st store.UsageRetentionStore, dbNowFn func(ctx context.Context) (time.Time, error)) *UsageRetentionSweeper {
	sw := &UsageRetentionSweeper{
		store:      st,
		batchYield: defaultUsageRetentionBatchYield,
		tickBudget: defaultUsageRetentionTickBudget,
		nowFn:      dbNowFn,
	}
	if sw.nowFn == nil {
		sw.nowFn = func(context.Context) (time.Time, error) { return time.Now().UTC(), nil }
	}
	sw.registerDefaultTargets(st)
	return sw
}

// RegisterTarget appends a retention target to the sweeper. Safe for concurrent
// registration before or between runs.
func (sw *UsageRetentionSweeper) RegisterTarget(t UsageRetentionTarget) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.targets = append(sw.targets, t)
}

// SetExportCursorProvider installs (or clears when nil) the P8 analytics
// export-cursor guard for usage_events.
func (sw *UsageRetentionSweeper) SetExportCursorProvider(p UsageExportCursorProvider) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.exportCursorProvider = p
}

// SetBatchYield overrides the inter-batch yield duration (primarily for tests).
func (sw *UsageRetentionSweeper) SetBatchYield(d time.Duration) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.batchYield = d
}

// SetTickBudget overrides the per-tick wall-clock budget (primarily for tests).
func (sw *UsageRetentionSweeper) SetTickBudget(d time.Duration) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.tickBudget = d
}

// SetEnabledCheck installs a predicate re-checked at tick start and before each
// batch so disabling tokenomics.usage_ledger mid-tick stops further deletes
// immediately (O4).
func (sw *UsageRetentionSweeper) SetEnabledCheck(fn func() bool) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.isEnabledFn = fn
}

func (sw *UsageRetentionSweeper) enabled() bool {
	sw.mu.RLock()
	fn := sw.isEnabledFn
	sw.mu.RUnlock()
	if fn == nil {
		return true
	}
	return fn()
}

// resolveExportCursor checks the optional P8 export cursor provider.
// Returns (cursor, skip, err):
//   - No provider installed: (nil, false, nil) -> age-only sweep.
//   - Provider installed, error: (nil, true, err) -> fail closed.
//   - Provider installed, nil cursor: (nil, true, nil) -> fail closed (O1: nothing exported yet).
//   - Provider installed, non-nil cursor: (cursor, false, nil) -> sweep up to cursor.
func (sw *UsageRetentionSweeper) resolveExportCursor(ctx context.Context) (*store.UsageExportCursor, bool, error) {
	sw.mu.RLock()
	p := sw.exportCursorProvider
	sw.mu.RUnlock()
	if p == nil {
		return nil, false, nil
	}
	cur, err := p.UsageExportCursor(ctx)
	if err != nil {
		return nil, true, fmt.Errorf("resolve usage_events export cursor: %w", err)
	}
	if cur == nil {
		return nil, true, nil
	}
	return cur, false, nil
}

func (sw *UsageRetentionSweeper) registerDefaultTargets(st store.UsageRetentionStore) {
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "usage_events",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.UsageEventsDays
		},
		SweepBatch: func(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
			cursor, skip, err := sw.resolveExportCursor(ctx)
			if err != nil {
				return 0, err
			}
			if skip {
				return 0, nil
			}
			return st.SweepUsageEventsBatch(ctx, cutoff, cursor, batchSize)
		},
	})
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "usage_rollups_hourly",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.HourlyRollupsDays
		},
		ComputeCutoff: hourlyRollupsRetentionCutoff,
		SweepBatch:    st.SweepUsageRollupsHourlyBatch,
	})
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "usage_rollups_daily",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.DailyRollupsDays
		},
		ComputeCutoff: dailyRollupsRetentionCutoff,
		SweepBatch:    st.SweepUsageRollupsDailyBatch,
	})
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "usage_scope_counters_daily",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.ScopeCountersDays
		},
		ComputeCutoff: scopeCountersRetentionCutoff,
		SweepBatch:    st.SweepUsageScopeCountersDailyBatch,
	})
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "usage_budget_state",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.BudgetStateDays
		},
		ComputeCutoff: budgetStateRetentionCutoff,
		SweepBatch:    st.SweepUsageBudgetStateBatch,
	})
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "meter_health",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.MeterHealthDays
		},
		SweepBatch: st.SweepMeterHealthBatch,
	})
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "run_attribution",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.RunAttributionDays
		},
		SweepBatch: st.SweepRunAttributionBatch,
	})
}

// hourlyRollupsRetentionCutoff returns the UTC hour-aligned cutoff matching
// resolveUsageWindowAndGranularity so read horizons and retention stay in sync.
func hourlyRollupsRetentionCutoff(now time.Time, retentionDays int) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC).
		AddDate(0, 0, -retentionDays)
}

// dailyRollupsRetentionCutoff returns the UTC day-aligned cutoff.
func dailyRollupsRetentionCutoff(now time.Time, retentionDays int) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -retentionDays)
}

// scopeCountersRetentionCutoff returns the UTC day-aligned cutoff for
// usage_scope_counters_daily, clamped to never exceed the first day of the
// previous calendar month so the current and previous calendar months are
// strictly preserved (Tokenomics §4.2).
func scopeCountersRetentionCutoff(now time.Time, retentionDays int) time.Time {
	u := now.UTC()
	ageCutoff := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -retentionDays)
	prevMonthStart := time.Date(u.Year(), u.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	if ageCutoff.After(prevMonthStart) {
		return prevMonthStart
	}
	return ageCutoff
}

// budgetStateRetentionCutoff returns the cutoff for closed usage_budget_state
// windows (window_end_utc < cutoff), clamped to never exceed now.UTC() so
// active windows (window_end_utc > now) are never deleted (Tokenomics §4.2).
func budgetStateRetentionCutoff(now time.Time, retentionDays int) time.Time {
	u := now.UTC()
	cutoff := u.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	if cutoff.After(u) {
		return u
	}
	return cutoff
}

// Run executes one tick of the retention sweeper, honoring cfg.SweepInterval
// between completed passes, bounding work within the tick budget, giving each
// target a fair time slice so an unbounded backlog on one table cannot starve
// later tables, resuming from NextTargetIndex on subsequent ticks, and
// advancing the durable hourly swept-through watermark (R1, R2, O4).
func (sw *UsageRetentionSweeper) Run(ctx context.Context, cfg config.ResolvedTelemetryRetention) UsageRetentionRunReport {
	started := time.Now().UTC()
	report := UsageRetentionRunReport{StartedAt: started}

	if !sw.enabled() {
		report.Skipped = true
		report.Duration = time.Since(started)
		return report
	}

	now, err := sw.nowFn(ctx)
	if err != nil {
		now = started
	} else {
		now = now.UTC()
	}

	// Load persisted state (last completed pass, resume cursor, hourly watermark).
	var state *store.UsageRetentionState
	if sw.store != nil {
		loaded, loadErr := sw.store.GetUsageRetentionState(ctx)
		if loadErr != nil {
			report.ErrorCount++
			report.Tables = append(report.Tables, UsageRetentionTableReport{
				Table: "usage_retention_state",
				Err:   fmt.Errorf("load state: %w", loadErr),
			})
			report.Duration = time.Since(started)
			return report
		}
		state = loaded
	} else {
		state = &store.UsageRetentionState{}
	}

	// Honor sweep_interval between completed passes unless a multi-tick pass is
	// already in progress.
	sweepInterval := cfg.SweepInterval
	if sweepInterval < config.MinRetentionSweepInterval {
		sweepInterval = config.DefaultRetentionSweepInterval
	}
	if !state.InProgress && state.LastCompletedAt != nil && now.Sub(*state.LastCompletedAt) < sweepInterval {
		report.Skipped = true
		report.Duration = time.Since(started)
		return report
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = config.DefaultRetentionBatchSize
	}

	sw.mu.RLock()
	targets := make([]UsageRetentionTarget, len(sw.targets))
	copy(targets, sw.targets)
	yield := sw.batchYield
	tickBudget := sw.tickBudget
	sw.mu.RUnlock()

	if len(targets) == 0 {
		report.PassCompleted = true
		report.Duration = time.Since(started)
		return report
	}

	// Compute the tick deadline: min(started+tickBudget, ctxDeadline-margin).
	tickDeadline := started.Add(tickBudget)
	if dl, ok := ctx.Deadline(); ok {
		if dl.Sub(started) > 2*usageRetentionTickMargin {
			if adj := dl.Add(-usageRetentionTickMargin); adj.Before(tickDeadline) {
				tickDeadline = adj
			}
		} else if dl.Before(tickDeadline) {
			tickDeadline = dl
		}
	}
	tickCtx, tickCancel := context.WithDeadline(ctx, tickDeadline)
	defer tickCancel()

	startIdx := state.NextTargetIndex
	if startIdx < 0 || startIdx >= len(targets) {
		startIdx = 0
	}

	report.Tables = make([]UsageRetentionTableReport, 0, len(targets))
	allDrained := true
	nextIdx := 0
	advancedNextIdx := false

	for step := 0; step < len(targets); step++ {
		idx := (startIdx + step) % len(targets)
		t := targets[idx]

		if !sw.enabled() || tickCtx.Err() != nil {
			allDrained = false
			if !advancedNextIdx {
				nextIdx = idx
			}
			break
		}

		days := 0
		if t.RetentionDays != nil {
			days = t.RetentionDays(cfg)
		}
		if days <= 0 {
			report.Tables = append(report.Tables, UsageRetentionTableReport{
				Table:         t.Name,
				Skipped:       true,
				SkippedReason: "unlimited",
				Drained:       true,
			})
			continue
		}

		var cutoff time.Time
		if t.ComputeCutoff != nil {
			cutoff = t.ComputeCutoff(now, days)
		} else {
			cutoff = now.Add(-time.Duration(days) * 24 * time.Hour)
		}

		// O-d: if usage_events has an export-cursor provider installed that
		// reports no cursor yet (nil, nil), fail closed and record an explicit
		// skipped_reason so operators see that usage_events was held back.
		if t.Name == "usage_events" {
			_, skipCursor, curErr := sw.resolveExportCursor(tickCtx)
			if curErr != nil {
				report.ErrorCount++
				allDrained = false
				if !advancedNextIdx {
					nextIdx = (idx + 1) % len(targets)
					advancedNextIdx = true
				}
				report.Tables = append(report.Tables, UsageRetentionTableReport{
					Table:  t.Name,
					Cutoff: cutoff,
					Err:    curErr,
				})
				continue
			}
			if skipCursor {
				report.Tables = append(report.Tables, UsageRetentionTableReport{
					Table:         t.Name,
					Cutoff:        cutoff,
					Skipped:       true,
					SkippedReason: "export_cursor_unavailable",
					Drained:       true,
				})
				continue
			}
		}

		// Fair-share slice for this target within the remaining tick budget so
		// an infinite backlog on one table cannot starve later tables.
		remainingTargets := len(targets) - step
		rem := time.Until(tickDeadline)
		sliceBudget := rem
		if remainingTargets > 1 && rem > 0 {
			sliceBudget = rem / time.Duration(remainingTargets)
		}
		sliceDeadline := time.Now().Add(sliceBudget)

		tStart := time.Now()
		tr := UsageRetentionTableReport{Table: t.Name, Cutoff: cutoff}

		for sw.enabled() {
			if tickCtx.Err() != nil {
				tr.Err = tickCtx.Err()
				break
			}
			n, sweepErr := t.SweepBatch(tickCtx, cutoff, batchSize)
			if sweepErr != nil {
				tr.Err = sweepErr
				break
			}
			tr.Deleted += n
			tr.Batches++

			// R2: advance the monotonic hourly swept-through watermark as soon
			// as usage_rollups_hourly runs a batch (or drains at cutoff).
			if t.Name == "usage_rollups_hourly" {
				c := cutoff.UTC()
				if state.HourlySweptThrough == nil || c.After(*state.HourlySweptThrough) {
					state.HourlySweptThrough = &c
				}
			}

			if n < int64(batchSize) {
				tr.Drained = true
				break
			}
			// If this target exhausted its fair-share slice, yield to the next
			// target in this tick and mark the pass incomplete.
			if remainingTargets > 1 && !time.Now().Before(sliceDeadline) {
				break
			}
			if yield > 0 {
				timer := time.NewTimer(yield)
				select {
				case <-tickCtx.Done():
					timer.Stop()
					tr.Err = tickCtx.Err()
				case <-timer.C:
				}
				if tr.Err != nil {
					break
				}
			}
		}

		tr.Duration = time.Since(tStart)
		report.TotalDeleted += tr.Deleted
		if tr.Err != nil {
			report.ErrorCount++
		}
		if !tr.Drained || tr.Err != nil {
			allDrained = false
			if !advancedNextIdx {
				// Advance past an interrupted/un-drained target so the next tick
				// starts on the following target and never starves later tables.
				nextIdx = (idx + 1) % len(targets)
				advancedNextIdx = true
			}
		}
		report.Tables = append(report.Tables, tr)
	}

	// Only mark the pass completed if every target was visited, drained, and had
	// no errors, and the experiment is still enabled.
	if allDrained && report.ErrorCount == 0 && len(report.Tables) == len(targets) && sw.enabled() {
		completedAt := now.UTC()
		state.LastCompletedAt = &completedAt
		state.InProgress = false
		state.NextTargetIndex = 0
		report.PassCompleted = true
	} else {
		state.InProgress = true
		state.NextTargetIndex = nextIdx
	}

	// Persist state using a detached timeout so a tick-budget expiry never loses
	// progress or the hourly swept-through watermark.
	if sw.store != nil {
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), usageRetentionStateWriteTimeout)
		if saveErr := sw.store.SaveUsageRetentionState(wctx, state); saveErr != nil {
			report.ErrorCount++
			report.Tables = append(report.Tables, UsageRetentionTableReport{
				Table: "usage_retention_state",
				Err:   fmt.Errorf("save state: %w", saveErr),
			})
		}
		wcancel()
	}

	report.Duration = time.Since(started)
	return report
}

// usageRetentionConfig returns the resolved Hub DB retention settings from the
// server's active telemetry config.
func (s *Server) usageRetentionConfig() config.ResolvedTelemetryRetention {
	s.mu.RLock()
	telCfg := s.config.TelemetryConfig
	s.mu.RUnlock()
	if telCfg == nil {
		return config.DefaultResolvedTelemetryRetention()
	}
	return config.ResolveTelemetryRetention(telCfg.Retention)
}

// UsageRetentionSweeper returns (lazily constructing on first call) the Hub's
// usage retention sweeper so later phases (P3/P5/P8) and tests can register
// additional targets or install an export cursor provider.
func (s *Server) UsageRetentionSweeper() *UsageRetentionSweeper {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usageRetention == nil && s.store != nil {
		sw := NewUsageRetentionSweeper(s.store, s.store.DBNow)
		sw.SetEnabledCheck(func() bool {
			return s.experimentEnabled(experiments.TokenomicsUsageLedger)
		})
		s.usageRetention = sw
	}
	return s.usageRetention
}

// usageRetentionHandler returns the 1-minute recurring singleton handler
// registered with Scheduler under store.LockUsageRetention. Gated behind
// tokenomics.usage_ledger (re-checked on every tick and before each batch).
func (s *Server) usageRetentionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
			return
		}
		sw := s.UsageRetentionSweeper()
		if sw == nil {
			return
		}
		cfg := s.usageRetentionConfig()
		report := sw.Run(ctx, cfg)
		if report.Skipped {
			return
		}
		logUsageRetentionReport(ctx, report)
	}
}

func logUsageRetentionReport(ctx context.Context, report UsageRetentionRunReport) {
	deletedByTable := make(map[string]int64, len(report.Tables))
	cutoffByTable := make(map[string]string, len(report.Tables))
	skippedByTable := make(map[string]string)
	var errs []string
	for _, tr := range report.Tables {
		if tr.Skipped {
			if tr.SkippedReason != "" {
				skippedByTable[tr.Table] = tr.SkippedReason
			}
		} else {
			deletedByTable[tr.Table] = tr.Deleted
		}
		if !tr.Cutoff.IsZero() {
			cutoffByTable[tr.Table] = tr.Cutoff.UTC().Format(time.RFC3339)
		}
		if tr.Err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", tr.Table, tr.Err))
		}
	}
	attrs := []any{
		"total_deleted", report.TotalDeleted,
		"duration_ms", report.Duration.Milliseconds(),
		"pass_completed", report.PassCompleted,
		"deleted_by_table", deletedByTable,
		"cutoff_by_table", cutoffByTable,
		"error_count", report.ErrorCount,
	}
	if len(skippedByTable) > 0 {
		attrs = append(attrs, "skipped_by_table", skippedByTable)
	}
	if report.ErrorCount > 0 {
		slog.ErrorContext(ctx, "usage retention sweep tick completed with errors",
			append(attrs, "errors", errs)...)
		return
	}
	slog.InfoContext(ctx, "usage retention sweep tick completed", attrs...)
}
