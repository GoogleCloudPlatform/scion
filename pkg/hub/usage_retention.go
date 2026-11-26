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
)

// UsageExportCursorProvider returns the analytics export watermark for
// usage_events (Tokenomics P8 hook, D8). Until P8 installs a provider, the
// sweeper leaves this nil and deletes usage_events by age only. When installed,
// a nil cursor means no cursor cap is active; a non-nil error fails closed
// (skips usage_events for that run so unexported rows are never dropped).
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

// UsageRetentionTableReport records the outcome of sweeping one table.
type UsageRetentionTableReport struct {
	Table    string
	Deleted  int64
	Batches  int
	Duration time.Duration
	Skipped  bool
	Err      error
}

// UsageRetentionRunReport summarizes one full pass of UsageRetentionSweeper.
type UsageRetentionRunReport struct {
	StartedAt    time.Time
	Duration     time.Duration
	TotalDeleted int64
	ErrorCount   int
	Tables       []UsageRetentionTableReport
}

// UsageRetentionSweeper executes bounded-batch retention sweeps across all
// registered Track 1 (and future Track 2) usage tables in registration order.
type UsageRetentionSweeper struct {
	mu                   sync.RWMutex
	targets              []UsageRetentionTarget
	batchYield           time.Duration
	nowFn                func(ctx context.Context) (time.Time, error)
	exportCursorProvider UsageExportCursorProvider
}

// NewUsageRetentionSweeper constructs a sweeper pre-registered with the six
// existing Track 1 usage tables in dependency order (usage_events,
// usage_rollups_hourly, usage_rollups_daily, usage_scope_counters_daily,
// meter_health, run_attribution).
func NewUsageRetentionSweeper(st store.UsageRetentionStore, dbNowFn func(ctx context.Context) (time.Time, error)) *UsageRetentionSweeper {
	sw := &UsageRetentionSweeper{
		batchYield: defaultUsageRetentionBatchYield,
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

func (sw *UsageRetentionSweeper) exportCursor(ctx context.Context) (*store.UsageExportCursor, error) {
	sw.mu.RLock()
	p := sw.exportCursorProvider
	sw.mu.RUnlock()
	if p == nil {
		return nil, nil
	}
	return p.UsageExportCursor(ctx)
}

func (sw *UsageRetentionSweeper) registerDefaultTargets(st store.UsageRetentionStore) {
	sw.RegisterTarget(UsageRetentionTarget{
		Name: "usage_events",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int {
			return cfg.UsageEventsDays
		},
		SweepBatch: func(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
			cursor, err := sw.exportCursor(ctx)
			if err != nil {
				return 0, fmt.Errorf("resolve usage_events export cursor: %w", err)
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

// Run executes one retention pass across all registered targets.
// Per-table errors are recorded in the report and do not abort independent
// targets. Context cancellation stops further batches cleanly.
func (sw *UsageRetentionSweeper) Run(ctx context.Context, cfg config.ResolvedTelemetryRetention) UsageRetentionRunReport {
	started := time.Now().UTC()
	report := UsageRetentionRunReport{StartedAt: started}

	now, err := sw.nowFn(ctx)
	if err != nil {
		now = started
	} else {
		now = now.UTC()
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = config.DefaultRetentionBatchSize
	}

	sw.mu.RLock()
	targets := make([]UsageRetentionTarget, len(sw.targets))
	copy(targets, sw.targets)
	yield := sw.batchYield
	sw.mu.RUnlock()

	report.Tables = make([]UsageRetentionTableReport, 0, len(targets))

	for _, t := range targets {
		if ctx.Err() != nil {
			report.Tables = append(report.Tables, UsageRetentionTableReport{
				Table: t.Name,
				Err:   ctx.Err(),
			})
			report.ErrorCount++
			continue
		}

		days := 0
		if t.RetentionDays != nil {
			days = t.RetentionDays(cfg)
		}
		if days <= 0 {
			report.Tables = append(report.Tables, UsageRetentionTableReport{
				Table:   t.Name,
				Skipped: true,
			})
			continue
		}

		var cutoff time.Time
		if t.ComputeCutoff != nil {
			cutoff = t.ComputeCutoff(now, days)
		} else {
			cutoff = now.Add(-time.Duration(days) * 24 * time.Hour)
		}

		tStart := time.Now()
		tr := UsageRetentionTableReport{Table: t.Name}

		for {
			if ctx.Err() != nil {
				tr.Err = ctx.Err()
				break
			}
			n, sweepErr := t.SweepBatch(ctx, cutoff, batchSize)
			if sweepErr != nil {
				tr.Err = sweepErr
				break
			}
			tr.Deleted += n
			tr.Batches++
			if n < int64(batchSize) {
				break
			}
			if yield > 0 {
				timer := time.NewTimer(yield)
				select {
				case <-ctx.Done():
					timer.Stop()
					tr.Err = ctx.Err()
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
		report.Tables = append(report.Tables, tr)
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

func usageRetentionIntervalMinutes(cfg config.ResolvedTelemetryRetention) int {
	mins := int(cfg.SweepInterval / time.Minute)
	if mins < 1 {
		return 1
	}
	return mins
}

// UsageRetentionSweeper returns (lazily constructing on first call) the Hub's
// usage retention sweeper so later phases (P3/P5/P8) and tests can register
// additional targets or install an export cursor provider.
func (s *Server) UsageRetentionSweeper() *UsageRetentionSweeper {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usageRetention == nil && s.store != nil {
		s.usageRetention = NewUsageRetentionSweeper(s.store, s.store.DBNow)
	}
	return s.usageRetention
}

// usageRetentionHandler returns the recurring singleton handler registered with
// Scheduler under store.LockUsageRetention. Gated behind
// tokenomics.usage_ledger: returns immediately without DB work when off.
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
		logUsageRetentionReport(ctx, report)
	}
}

// reconcileUsageRetentionSchedule updates the registered recurring singleton
// interval if telemetry.retention.sweep_interval changed on live settings reload.
func (s *Server) reconcileUsageRetentionSchedule() {
	if s == nil || s.scheduler == nil {
		return
	}
	cfg := s.usageRetentionConfig()
	s.scheduler.UpsertRecurringSingleton(
		usageRetentionJobName,
		usageRetentionIntervalMinutes(cfg),
		store.LockUsageRetention,
		s.usageRetentionHandler(),
	)
}

func logUsageRetentionReport(ctx context.Context, report UsageRetentionRunReport) {
	deletedByTable := make(map[string]int64, len(report.Tables))
	var errs []string
	for _, tr := range report.Tables {
		if !tr.Skipped {
			deletedByTable[tr.Table] = tr.Deleted
		}
		if tr.Err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", tr.Table, tr.Err))
		}
	}
	attrs := []any{
		"total_deleted", report.TotalDeleted,
		"duration_ms", report.Duration.Milliseconds(),
		"deleted_by_table", deletedByTable,
		"error_count", report.ErrorCount,
	}
	if report.ErrorCount > 0 {
		slog.ErrorContext(ctx, "usage retention sweep completed with errors",
			append(attrs, "errors", errs)...)
		return
	}
	slog.InfoContext(ctx, "usage retention sweep completed", attrs...)
}
