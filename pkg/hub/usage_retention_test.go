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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func retentionIntPtr(v int) *int { return &v }

func TestUsageRetention_ScopeCountersMonthPreservation(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		days int
		want time.Time
	}{
		{
			name: "mid-year 90d keeps earlier age cutoff",
			now:  time.Date(2026, 7, 15, 14, 30, 0, 0, time.UTC),
			days: 90,
			want: time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "january rolls back to december of previous year",
			now:  time.Date(2026, 1, 20, 8, 0, 0, 0, time.UTC),
			days: 30, // even if shorter, clamped to Dec 1 2025
			want: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "march 31 with 65d preserves feb and mar",
			now:  time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC),
			days: 65,
			want: time.Date(2026, 1, 25, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "short window clamped to first of previous month",
			now:  time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
			days: 40, // 40d ago is Jul 22, which is inside previous month Jul -> clamp to Jul 1
			want: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scopeCountersRetentionCutoff(tc.now, tc.days)
			assert.Equal(t, tc.want, got)
			prevMonthStart := time.Date(tc.now.UTC().Year(), tc.now.UTC().Month()-1, 1, 0, 0, 0, 0, time.UTC)
			assert.False(t, got.After(prevMonthStart), "cutoff must never enter previous calendar month")
		})
	}
}

func TestUsageRetention_ExperimentOffNoOpAndOnSweeps(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Seed a project and live agent (current run) plus a closed superseded run.
	proj := &store.Project{ID: uuid.NewString(), Name: "ret-proj", Slug: "ret-proj"}
	require.NoError(t, st.CreateProject(ctx, proj))

	liveAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ret-live",
		Name:      "ret-live",
		ProjectID: proj.ID,
	}
	require.NoError(t, st.CreateAgent(ctx, liveAgent))
	_, err := st.SetAgentRunID(ctx, liveAgent.ID, "run-live", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	oldTS := now.AddDate(0, 0, -400)

	// Seed run_attribution for both live current run and old closed run.
	_, err = st.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: "run-live", HubID: "hub-1", ProjectID: proj.ID, AgentID: liveAgent.ID, CreatedAt: oldTS,
	})
	require.NoError(t, err)
	_, err = st.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: "run-old", HubID: "hub-1", ProjectID: proj.ID, AgentID: liveAgent.ID, CreatedAt: oldTS,
	})
	require.NoError(t, err)

	// Ingest an old event for run-old (populates usage_events, hourly/daily rollups, scope counters, meter_health).
	inTok := int64(100)
	_, err = st.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: liveAgent.ID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "evt-old-1", RunID: "run-old",
				ProjectID: proj.ID, AgentID: liveAgent.ID,
				Model: "claude-sonnet-4-6", ModelSource: "reported", UsageSource: "native",
				Status: "ok", Calls: 1, Input: &inTok, TokenStatus: "complete",
				EventTS: oldTS, ReceivedAt: oldTS, ProducerEpoch: "ep-old", ProducerSeq: 1,
			},
		},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID: liveAgent.ID, RunID: "run-old", ProducerEpoch: "ep-old",
			LastReceiptAt: oldTS, ProducerSeq: 1, FinalState: store.MeterHealthFinalStateFinal,
		},
	})
	require.NoError(t, err)

	// Experiment OFF: handler must do nothing.
	setUsageLedgerExperimentForTest(t, srv, false)
	srv.usageRetentionHandler()(ctx)

	hasEvents, err := st.HasUsageEventsForRun(ctx, liveAgent.ID, "run-old")
	require.NoError(t, err)
	assert.True(t, hasEvents, "experiment off must not delete usage_events")
	_, err = st.GetRunAttribution(ctx, "run-old")
	require.NoError(t, err, "experiment off must not delete run_attribution")

	// Experiment ON: handler sweeps old rows and preserves live agent's current run.
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.UsageRetentionSweeper().SetBatchYield(0)
	srv.usageRetentionHandler()(ctx)

	hasEvents, err = st.HasUsageEventsForRun(ctx, liveAgent.ID, "run-old")
	require.NoError(t, err)
	assert.False(t, hasEvents, "experiment on must sweep expired usage_events")

	// Live agent current run attribution must still exist.
	_, err = st.GetRunAttribution(ctx, "run-live")
	require.NoError(t, err, "live agent current-run attribution must never be deleted")

	// Note: meter_health.updated_at was set to time.Now() on insert by Ent UpdateDefault,
	// so run-old's meter_health is fresh (< 30d) and protects run-old's run_attribution until
	// meter_health ages out; verify that safety invariant held!
	mhs, err := st.GetMeterHealth(ctx, liveAgent.ID, "run-old")
	require.NoError(t, err)
	assert.Len(t, mhs, 1, "fresh meter_health row must not be deleted")
	_, err = st.GetRunAttribution(ctx, "run-old")
	require.NoError(t, err, "run_attribution with remaining meter_health must not be deleted")
}

func TestUsageRetention_UnlimitedZeroExportCursorAndCustomTarget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	var (
		mu       sync.Mutex
		calls    = map[string]int{}
		lastCur  *store.UsageExportCursor
		batchSeq = map[string][]int64{
			"usage_events":               {5, 5, 2}, // tests multi-batch termination
			"usage_rollups_hourly":       {3},
			"usage_rollups_daily":        {1},
			"usage_scope_counters_daily": {0},
			"meter_health":               {4},
			"run_attribution":            {2},
		}
	)

	fake := &fakeUsageRetentionStore{
		onSweep: func(table string, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error) {
			mu.Lock()
			defer mu.Unlock()
			idx := calls[table]
			calls[table]++
			if table == "usage_events" {
				lastCur = cur
			}
			seq := batchSeq[table]
			if idx < len(seq) {
				return seq[idx], nil
			}
			return 0, nil
		},
	}

	sw := NewUsageRetentionSweeper(fake, func(context.Context) (time.Time, error) { return now, nil })
	sw.SetBatchYield(0)

	// Register a synthetic future table (extensibility check for P3/P5).
	var customCalled int
	sw.RegisterTarget(UsageRetentionTarget{
		Name:          "usage_group_rollups_daily",
		RetentionDays: func(cfg config.ResolvedTelemetryRetention) int { return cfg.DailyRollupsDays },
		SweepBatch: func(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
			customCalled++
			return 3, nil
		},
	})

	// 1. Test explicit 0 = unlimited skips usage_events, daily_rollups, run_attribution, and custom target.
	cfgUnlimited := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{
		UsageEventsDays:  retentionIntPtr(0),
		DailyRollupsDays: retentionIntPtr(0),
		BatchSize:        retentionIntPtr(5),
	})
	rep := sw.Run(ctx, cfgUnlimited)
	assert.Equal(t, 0, rep.ErrorCount)
	assert.True(t, rep.PassCompleted)
	assert.Equal(t, 0, calls["usage_events"])
	assert.Equal(t, 0, calls["usage_rollups_daily"])
	assert.Equal(t, 0, calls["run_attribution"])
	assert.Equal(t, 0, customCalled)
	assert.Equal(t, 1, calls["usage_rollups_hourly"])
	assert.Equal(t, 1, calls["meter_health"])

	// Reset persisted LastCompletedAt between sub-scenarios.
	fake.resetState()

	// 2. Test P8 export cursor provider: fail-closed on error does not abort other tables.
	calls = map[string]int{}
	sw.SetExportCursorProvider(funcExportCursorProvider(func(context.Context) (*store.UsageExportCursor, error) {
		return nil, errors.New("gcs cursor store unavailable")
	}))
	cfgDefault := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{BatchSize: retentionIntPtr(5)})
	rep = sw.Run(ctx, cfgDefault)
	assert.Equal(t, 1, rep.ErrorCount)
	assert.False(t, rep.PassCompleted)
	assert.Equal(t, 0, calls["usage_events"], "must fail-closed and not call SweepUsageEventsBatch on cursor error")
	assert.Equal(t, 1, calls["usage_rollups_hourly"], "other tables must still run")
	assert.Equal(t, 1, customCalled, "registered custom target must run")

	// 2b. Test O1/O-d: installed provider returning (nil, nil) must ALSO fail closed (skip usage_events)
	// and record explicit skipped_reason="export_cursor_unavailable".
	fake.resetState()
	calls = map[string]int{}
	sw.SetExportCursorProvider(funcExportCursorProvider(func(context.Context) (*store.UsageExportCursor, error) {
		return nil, nil
	}))
	rep = sw.Run(ctx, cfgDefault)
	assert.Equal(t, 0, rep.ErrorCount)
	assert.Equal(t, 0, calls["usage_events"], "installed cursor provider returning (nil, nil) must fail closed without calling SweepUsageEventsBatch")
	var foundEventsReport *UsageRetentionTableReport
	for i := range rep.Tables {
		if rep.Tables[i].Table == "usage_events" {
			foundEventsReport = &rep.Tables[i]
			break
		}
	}
	require.NotNil(t, foundEventsReport)
	assert.True(t, foundEventsReport.Skipped)
	assert.Equal(t, "export_cursor_unavailable", foundEventsReport.SkippedReason)
	assert.False(t, foundEventsReport.Cutoff.IsZero())

	// 3. Test P8 export cursor provider returning a valid cursor + multi-batch loop.
	fake.resetState()
	calls = map[string]int{}
	wantCursor := &store.UsageExportCursor{ReceivedAt: now.Add(-48 * time.Hour), EventID: "evt-watermark"}
	sw.SetExportCursorProvider(funcExportCursorProvider(func(context.Context) (*store.UsageExportCursor, error) {
		return wantCursor, nil
	}))
	rep = sw.Run(ctx, cfgDefault)
	assert.Equal(t, 0, rep.ErrorCount)
	assert.True(t, rep.PassCompleted)
	assert.Equal(t, 3, calls["usage_events"], "batch sequence [5, 5, 2] with batchSize=5 must take exactly 3 calls")
	require.NotNil(t, lastCur)
	assert.Equal(t, wantCursor.EventID, lastCur.EventID)
}

func TestUsageRetention_TickBudgetNoStarvationAndCadence(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	t.Run("deterministic resume-past-undrained on ctx expiry and cadence", func(t *testing.T) {
		var mu sync.Mutex
		curNow := now
		eventsInfinite := true
		eventsCallsInTick1 := 0
		swept := map[string]int{}

		ctx1, cancel1 := context.WithCancel(context.Background())
		defer cancel1()

		fake := &fakeUsageRetentionStore{
			onSweep: func(table string, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error) {
				mu.Lock()
				swept[table]++
				inf := eventsInfinite
				if table == "usage_events" && inf {
					eventsCallsInTick1++
					if eventsCallsInTick1 == 2 {
						// Deterministically expire ctx1 mid-usage_events on batch 2.
						cancel1()
					}
					mu.Unlock()
					return int64(batchSize), nil
				}
				mu.Unlock()
				return 1, nil
			},
		}

		sw := NewUsageRetentionSweeper(fake, func(context.Context) (time.Time, error) {
			mu.Lock()
			defer mu.Unlock()
			return curNow, nil
		})
		sw.SetBatchYield(0)
		// Large tickBudget so tick 1 ends strictly via deterministic ctx1 cancellation.
		sw.SetTickBudget(10 * time.Second)

		cfg := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{
			SweepInterval: "24h",
			BatchSize:     retentionIntPtr(5),
		})

		// Tick 1: ctx1 cancels mid-usage_events -> must save NextTargetIndex=1 and InProgress=true.
		rep1 := sw.Run(ctx1, cfg)
		assert.False(t, rep1.PassCompleted, "incomplete tick must not mark pass completed")
		assert.False(t, rep1.Skipped)
		st1, err := fake.GetUsageRetentionState(context.Background())
		require.NoError(t, err)
		assert.True(t, st1.InProgress)
		assert.Equal(t, 1, st1.NextTargetIndex, "must advance NextTargetIndex past interrupted target 0")

		// Tick 2: starts at NextTargetIndex=1 (usage_rollups_hourly) and sweeps all later targets
		// even if usage_events still returns full batches once visited last.
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		fake.onSweep = func(table string, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error) {
			mu.Lock()
			swept[table]++
			inf := eventsInfinite
			if table == "usage_events" && inf {
				cancel2()
				mu.Unlock()
				return int64(batchSize), nil
			}
			mu.Unlock()
			return 1, nil
		}
		rep2 := sw.Run(ctx2, cfg)
		assert.False(t, rep2.PassCompleted, "usage_events still undrained")
		mu.Lock()
		assert.Equal(t, 1, swept["usage_rollups_hourly"], "hourly rollups must not be starved")
		assert.Equal(t, 1, swept["usage_rollups_daily"], "daily rollups must not be starved")
		assert.Equal(t, 1, swept["usage_scope_counters_daily"], "scope counters must not be starved")
		assert.Equal(t, 1, swept["meter_health"], "meter_health must not be starved")
		assert.Equal(t, 1, swept["run_attribution"], "run_attribution must not be starved")
		eventsInfinite = false
		mu.Unlock()

		// Tick 3: drains remaining targets and completes the pass.
		rep3 := sw.Run(context.Background(), cfg)
		assert.True(t, rep3.PassCompleted)
		assert.Equal(t, 0, rep3.ErrorCount)

		// Immediate next tick (1 minute later) must wait sweep_interval (24h).
		mu.Lock()
		curNow = curNow.Add(time.Minute)
		beforeSkipped := swept["usage_events"]
		mu.Unlock()
		rep4 := sw.Run(context.Background(), cfg)
		assert.True(t, rep4.Skipped, "must wait sweep_interval after completed pass")
		mu.Lock()
		assert.Equal(t, beforeSkipped, swept["usage_events"])
		mu.Unlock()

		// Live cadence update (O3): advance 2 hours and lower sweep_interval to 1h -> runs immediately!
		mu.Lock()
		curNow = curNow.Add(2 * time.Hour)
		mu.Unlock()
		cfgShorter := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{
			SweepInterval: "1h",
			BatchSize:     retentionIntPtr(5),
		})
		rep5 := sw.Run(context.Background(), cfgShorter)
		assert.False(t, rep5.Skipped, "shorter sweep_interval must apply live on next tick")
		assert.True(t, rep5.PassCompleted)

		// Mid-tick experiment disable (O4): disabling experiment prevents further work.
		mu.Lock()
		curNow = curNow.Add(2 * time.Hour)
		beforeDisabled := swept["usage_events"]
		mu.Unlock()
		sw.SetEnabledCheck(func() bool { return false })
		rep6 := sw.Run(context.Background(), cfgShorter)
		assert.True(t, rep6.Skipped)
		mu.Lock()
		assert.Equal(t, beforeDisabled, swept["usage_events"])
		mu.Unlock()
	})

	t.Run("deterministic fair-share slice prevents single-tick starvation", func(t *testing.T) {
		var mu sync.Mutex
		swept := map[string]int{}
		fake := &fakeUsageRetentionStore{
			onSweep: func(table string, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error) {
				mu.Lock()
				swept[table]++
				mu.Unlock()
				if table == "usage_events" {
					// Exceeds the 1/6 fair-share slice of tickBudget (60ms/6 = 10ms) in one batch,
					// while leaving ~40ms in the tick for the remaining 5 targets.
					time.Sleep(15 * time.Millisecond)
					return int64(batchSize), nil
				}
				return 1, nil
			},
		}
		sw := NewUsageRetentionSweeper(fake, func(context.Context) (time.Time, error) { return now, nil })
		sw.SetBatchYield(0)
		sw.SetTickBudget(60 * time.Millisecond)

		cfg := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{BatchSize: retentionIntPtr(5)})
		rep := sw.Run(context.Background(), cfg)
		assert.False(t, rep.PassCompleted, "usage_events did not drain")
		mu.Lock()
		assert.Equal(t, 1, swept["usage_events"], "fair-share slice must yield after target 0 exceeds slice")
		assert.Equal(t, 1, swept["usage_rollups_hourly"], "later targets must run in the same tick")
		assert.Equal(t, 1, swept["usage_rollups_daily"])
		assert.Equal(t, 1, swept["usage_scope_counters_daily"])
		assert.Equal(t, 1, swept["meter_health"])
		assert.Equal(t, 1, swept["run_attribution"])
		mu.Unlock()
	})
}

// TestUsageRetention_ResolveWindowAndGranularity_WatermarkAndRetention tests
// resolveUsageWindowAndGranularity directly (without the D7a cross-check) so
// disabling either hourlySweptThrough or hourlyRetentionDays fails immediately.
func TestUsageRetention_ResolveWindowAndGranularity_WatermarkAndRetention(t *testing.T) {
	now := time.Date(2026, 11, 26, 15, 30, 0, 0, time.UTC)
	covStart := now.AddDate(0, 0, -60).Truncate(time.Hour)
	watermark := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)

	tests := []struct {
		name            string
		since           time.Time
		retentionDays   int
		sweptThrough    time.Time
		requireHourly   bool
		wantGranularity string
		wantErrContains string
	}{
		{
			name:            "1h before watermark falls back to day (retention=30d)",
			since:           watermark.Add(-time.Hour),
			retentionDays:   30,
			sweptThrough:    watermark,
			wantGranularity: store.UsageGranularityDay,
		},
		{
			name:            "1h before watermark rejects explicit groupBy=hour (retention=30d)",
			since:           watermark.Add(-time.Hour),
			retentionDays:   30,
			sweptThrough:    watermark,
			requireHourly:   true,
			wantErrContains: "swept",
		},
		{
			name:            "1h before watermark falls back to day even when retention=0 unlimited",
			since:           watermark.Add(-time.Hour),
			retentionDays:   0,
			sweptThrough:    watermark,
			wantGranularity: store.UsageGranularityDay,
		},
		{
			name:            "1h before watermark rejects groupBy=hour even when retention=0 unlimited",
			since:           watermark.Add(-time.Hour),
			retentionDays:   0,
			sweptThrough:    watermark,
			requireHourly:   true,
			wantErrContains: "swept",
		},
		{
			name:            "exact watermark allows hour granularity",
			since:           watermark,
			retentionDays:   30,
			sweptThrough:    watermark,
			requireHourly:   true,
			wantGranularity: store.UsageGranularityHour,
		},
		{
			name:            "non-hour-aligned watermark truncates down to UTC hour",
			since:           watermark, // 14:00 == truncate(14:37)
			retentionDays:   30,
			sweptThrough:    watermark.Add(37 * time.Minute),
			requireHourly:   true,
			wantGranularity: store.UsageGranularityHour,
		},
		{
			name:            "1h before non-hour-aligned watermark rejected",
			since:           watermark.Add(-time.Hour), // 13:00 < truncate(14:37)
			retentionDays:   30,
			sweptThrough:    watermark.Add(37 * time.Minute),
			requireHourly:   true,
			wantErrContains: "swept",
		},
		{
			name:            "configured retention days (7d) rejects 8d ago when watermark is zero",
			since:           now.Truncate(time.Hour).AddDate(0, 0, -8),
			retentionDays:   7,
			sweptThrough:    time.Time{},
			requireHourly:   true,
			wantErrContains: "7-day hourly retention",
		},
		{
			name:            "configured retention days (7d) allows exact 7d cutoff when watermark is zero",
			since:           now.Truncate(time.Hour).AddDate(0, 0, -7),
			retentionDays:   7,
			sweptThrough:    time.Time{},
			requireHourly:   true,
			wantGranularity: store.UsageGranularityHour,
		},
		{
			name:            "unlimited retention (0) allows 40d ago after coverage when watermark is zero",
			since:           now.Truncate(time.Hour).AddDate(0, 0, -40),
			retentionDays:   0,
			sweptThrough:    time.Time{},
			requireHourly:   true,
			wantGranularity: store.UsageGranularityHour,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			win, err := resolveUsageWindowAndGranularity(
				tc.since.Format(time.RFC3339),
				now.Format(time.RFC3339),
				now,
				covStart,
				true,
				tc.requireHourly,
				tc.retentionDays,
				tc.sweptThrough,
			)
			if tc.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantGranularity, win.Granularity)
		})
	}
}

// TestUsageRetention_HourlyWatermarkAdvanceInvariants tests UsageRetentionSweeper
// directly against a fake store to pin HourlySweptThrough persistence,
// monotonicity, unlimited-mode no-op, and error safety.
func TestUsageRetention_HourlyWatermarkAdvanceInvariants(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 15, 42, 0, 0, time.UTC)

	var hourlyErr error
	fake := &fakeUsageRetentionStore{
		onSweep: func(table string, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error) {
			if table == "usage_rollups_hourly" && hourlyErr != nil {
				return 0, hourlyErr
			}
			return 0, nil
		},
	}

	sw := NewUsageRetentionSweeper(fake, func(context.Context) (time.Time, error) { return now, nil })
	sw.SetBatchYield(0)

	// 1. Hourly error must NOT advance HourlySweptThrough.
	hourlyErr = errors.New("db unavailable")
	cfg14 := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{HourlyRollupsDays: retentionIntPtr(14)})
	repErr := sw.Run(ctx, cfg14)
	assert.Equal(t, 1, repErr.ErrorCount)
	st, err := fake.GetUsageRetentionState(ctx)
	require.NoError(t, err)
	assert.Nil(t, st.HourlySweptThrough, "watermark must not advance when hourly sweep errors")

	// 2. Unlimited (0) must NOT advance HourlySweptThrough and records SkippedReason="unlimited".
	hourlyErr = nil
	fake.resetState()
	cfgUnlim := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{HourlyRollupsDays: retentionIntPtr(0)})
	repUnlim := sw.Run(ctx, cfgUnlim)
	assert.Equal(t, 0, repUnlim.ErrorCount)
	st, err = fake.GetUsageRetentionState(ctx)
	require.NoError(t, err)
	assert.Nil(t, st.HourlySweptThrough, "watermark must not advance when hourly retention is unlimited (0)")
	for _, tr := range repUnlim.Tables {
		if tr.Table == "usage_rollups_hourly" {
			assert.True(t, tr.Skipped)
			assert.Equal(t, "unlimited", tr.SkippedReason)
		}
	}

	// 3. Normal run at 14d advances HourlySweptThrough to the UTC hour-aligned cutoff.
	fake.resetState()
	rep14 := sw.Run(ctx, cfg14)
	assert.True(t, rep14.PassCompleted)
	st, err = fake.GetUsageRetentionState(ctx)
	require.NoError(t, err)
	wantWM := hourlyRollupsRetentionCutoff(now, 14)
	require.NotNil(t, st.HourlySweptThrough)
	assert.Equal(t, wantWM, *st.HourlySweptThrough)

	// 4. Raising hourly_rollups_days to 30d must NEVER move HourlySweptThrough backward.
	st.LastCompletedAt = nil // allow immediate next pass
	require.NoError(t, fake.SaveUsageRetentionState(ctx, st))
	cfg30 := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{HourlyRollupsDays: retentionIntPtr(30)})
	rep30 := sw.Run(ctx, cfg30)
	assert.True(t, rep30.PassCompleted)
	st, err = fake.GetUsageRetentionState(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.HourlySweptThrough)
	assert.Equal(t, wantWM, *st.HourlySweptThrough, "watermark must be monotonic and never move backward")
}

func TestUsageRetention_HourlyDefaultLimitAndWireStripping(t *testing.T) {
	// O-c: default hourly limit derived from configured retention days, capped at 500.
	n14, err := parseUsageLimit("", true, 14)
	require.NoError(t, err)
	assert.Equal(t, 337, n14)

	n20, err := parseUsageLimit("", true, 20)
	require.NoError(t, err)
	assert.Equal(t, 481, n20)

	n30, err := parseUsageLimit("", true, 30)
	require.NoError(t, err)
	assert.Equal(t, maxUsageGroupsLimit, n30)

	nUnlim, err := parseUsageLimit("", true, 0)
	require.NoError(t, err)
	assert.Equal(t, maxUsageGroupsLimit, nUnlim)

	nShort, err := parseUsageLimit("", true, 2)
	require.NoError(t, err)
	assert.Equal(t, defaultUsageGroupsLimit, nShort)

	nNonHour, err := parseUsageLimit("", false, 20)
	require.NoError(t, err)
	assert.Equal(t, defaultUsageGroupsLimit, nNonHour)

	// O-e: wireTelemetry strips Hub-only Retention before sending to brokers.
	td := telemetryDispatch{
		hubDefault: &api.TelemetryConfig{
			Retention: &api.TelemetryRetentionConfig{UsageEventsDays: retentionIntPtr(40)},
		},
	}
	wire := td.wireTelemetry()
	require.NotNil(t, wire)
	assert.Nil(t, wire.Retention, "Hub-only Retention must be cleared from broker wire telemetry")
}

func TestUsageRetention_AdminSettingsValidation(t *testing.T) {
	t.Run("DB-backed handlePutServerConfigDB rejects below-min and accepts 0 unlimited", func(t *testing.T) {
		srv, _ := testServerWithOps(t, nil)
		ops := srv.GetOperationalSettings()
		require.NotNil(t, ops)
		ops.server = srv

		rrBad := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rrBad, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
			`{"telemetry":{"retention":{"usage_events_days":10}}}`), ops)
		assert.Equal(t, http.StatusUnprocessableEntity, rrBad.Code, rrBad.Body.String())

		rrZero := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rrZero, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
			`{"telemetry":{"retention":{"usage_events_days":0}}}`), ops)
		require.Equal(t, http.StatusOK, rrZero.Code, rrZero.Body.String())
		snap := ops.Snapshot()
		require.NotNil(t, snap.TelemetryConfig)
		require.NotNil(t, snap.TelemetryConfig.Retention)
		require.NotNil(t, snap.TelemetryConfig.Retention.UsageEventsDays)
		assert.Equal(t, 0, *snap.TelemetryConfig.Retention.UsageEventsDays)
		assert.Equal(t, 0, srv.usageRetentionConfig().UsageEventsDays)
	})

	t.Run("file-mode handlePutServerConfig rejects below-min and accepts 0 unlimited", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		scionDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0644))

		srv, _ := testServer(t)

		rrBad := httptest.NewRecorder()
		srv.handlePutServerConfig(rrBad, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
			`{"telemetry":{"retention":{"usage_events_days":10}}}`))
		assert.Equal(t, http.StatusUnprocessableEntity, rrBad.Code, rrBad.Body.String())

		rrZero := httptest.NewRecorder()
		srv.handlePutServerConfig(rrZero, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
			`{"telemetry":{"retention":{"usage_events_days":0}}}`))
		require.Equal(t, http.StatusOK, rrZero.Code, rrZero.Body.String())
		assert.Equal(t, 0, srv.usageRetentionConfig().UsageEventsDays)
	})
}

func TestUsageRetention_HourlyHorizonSweptThroughWatermark(t *testing.T) {
	srv, st := testServer(t)
	setUsageLedgerExperimentForTest(t, srv, true)
	ctx := context.Background()

	admin := &store.User{ID: uuid.NewString(), Email: "wm-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, st.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, st, admin.ID, store.SystemRoleHubAdmin)

	proj := &store.Project{ID: uuid.NewString(), Name: "wm-proj", Slug: "wm-proj"}
	require.NoError(t, st.CreateProject(ctx, proj))
	agentID := uuid.NewString()
	runID := "run-wm-horizon"
	_, err := st.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: runID, HubID: "hub-1", ProjectID: proj.ID, AgentID: agentID,
	})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Hour)
	tokOld := int64(100)
	tokNew := int64(50)
	// Seed events at now-25d (older than 14d) and now-5d (within 14d).
	// Hourly coverage starts at now-25d.
	_, err = st.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "evt-wm-old", RunID: runID,
				ProjectID: proj.ID, AgentID: agentID,
				Model: "claude-sonnet-4-6", ModelSource: "reported", UsageSource: "native",
				Status: "ok", Calls: 1, Input: &tokOld, TokenStatus: "complete",
				EventTS: now.AddDate(0, 0, -25), ReceivedAt: now,
				ProducerEpoch: "ep-wm", ProducerSeq: 1,
			},
			{
				HubID: "hub-1", EventID: "evt-wm-new", RunID: runID,
				ProjectID: proj.ID, AgentID: agentID,
				Model: "claude-sonnet-4-6", ModelSource: "reported", UsageSource: "native",
				Status: "ok", Calls: 1, Input: &tokNew, TokenStatus: "complete",
				EventTS: now.AddDate(0, 0, -5), ReceivedAt: now,
				ProducerEpoch: "ep-wm", ProducerSeq: 2,
			},
		},
	})
	require.NoError(t, err)

	// Simulate an established hub whose hourly coverage started 25 days ago.
	markerPayload, err := json.Marshal(map[string]string{"started_at": now.AddDate(0, 0, -25).UTC().Format(time.RFC3339)})
	require.NoError(t, err)
	_, err = st.UpsertHubSetting(ctx, store.UsageRollupHourlyCoverageSection, markerPayload, "test", -1, "seeded")
	require.NoError(t, err)

	// Sweep at default 14d: deletes now-25d hourly rollup and persists HourlySweptThrough.
	srv.UsageRetentionSweeper().SetBatchYield(0)
	srv.usageRetentionHandler()(ctx)

	// Operator raises hourly_rollups_days to 30d.
	srv.mu.Lock()
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Retention: &api.TelemetryRetentionConfig{HourlyRollupsDays: retentionIntPtr(30)},
	}
	srv.mu.Unlock()

	since := now.AddDate(0, 0, -25)
	baseURL := "/api/v1/usage?since=" + since.Format(time.RFC3339) + "&until=" + now.Format(time.RFC3339)

	// Auto-granularity query must fall back to day and return full daily total (100 + 50 = 150),
	// never under-reporting from swept hourly rollups (50).
	rec := doUserUsageReadRequest(t, srv, baseURL, admin)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, store.UsageGranularityDay, resp.Window.Granularity)
	require.NotNil(t, resp.Tokens.Input)
	assert.Equal(t, int64(150), *resp.Tokens.Input)

	// Explicit groupBy=hour across the swept window must be rejected with 400.
	recHour := doUserUsageReadRequest(t, srv, baseURL+"&groupBy=hour", admin)
	assert.Equal(t, http.StatusBadRequest, recHour.Code)
}

func TestUsageRetention_SchedulerRegistration(t *testing.T) {
	srv, st := testServer(t)
	sharedLocker := newContendedAdvisoryLocker()
	srv.scheduler = NewScheduler(&advisoryLockTestStore{Store: st, AdvisoryLocker: sharedLocker}, slog.Default())
	srv.registerSchedulerHandlers()

	srv.scheduler.mu.Lock()
	var found *RecurringHandler
	for i := range srv.scheduler.recurring {
		if srv.scheduler.recurring[i].Name == usageRetentionJobName {
			h := srv.scheduler.recurring[i]
			found = &h
			break
		}
	}
	srv.scheduler.mu.Unlock()

	require.NotNil(t, found, "usage-retention must be registered by registerSchedulerHandlers")
	assert.True(t, found.Singleton, "usage-retention must be a singleton")
	assert.Equal(t, 1, found.Interval, "usage-retention must run on a 1-minute tick")

	// Verify it is specifically guarded by store.LockUsageRetention: holding that lock
	// prevents the registered handler from running.
	acquired, release, err := sharedLocker.TryAdvisoryLock(context.Background(), store.LockUsageRetention)
	require.NoError(t, err)
	require.True(t, acquired)
	defer func() { _ = release() }()

	setUsageLedgerExperimentForTest(t, srv, true)
	found.Fn(context.Background())
	persisted, err := srv.store.GetUsageRetentionState(context.Background())
	require.NoError(t, err)
	assert.Nil(t, persisted.LastCompletedAt, "handler must skip while LockUsageRetention is held")
}

func TestUsageRetention_SingleLeaderTwoHubs(t *testing.T) {
	ctx := context.Background()
	sharedLocker := newContendedAdvisoryLocker()

	var concurrent, maxConcurrent, totalRuns atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	makeHandler := func() func(context.Context) {
		return func(ctx context.Context) {
			cur := concurrent.Add(1)
			for {
				prev := maxConcurrent.Load()
				if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
					break
				}
			}
			totalRuns.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			concurrent.Add(-1)
		}
	}

	// Two Hub schedulers backed by the same shared advisory lock seam.
	schedA := NewScheduler(&advisoryLockTestStore{AdvisoryLocker: sharedLocker}, slog.Default())
	schedB := NewScheduler(&advisoryLockTestStore{AdvisoryLocker: sharedLocker}, slog.Default())

	guardedA := schedA.singletonGuard(usageRetentionJobName, store.LockUsageRetention, makeHandler())
	guardedB := schedB.singletonGuard(usageRetentionJobName, store.LockUsageRetention, makeHandler())

	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		guardedA(ctx)
	}()

	<-entered
	// While Hub A holds LockUsageRetention, Hub B must skip without running.
	guardedB(ctx)
	assert.Equal(t, int32(1), totalRuns.Load(), "second hub replica must skip while leader holds lock")
	assert.Equal(t, int32(1), maxConcurrent.Load(), "at most one replica may execute concurrently")

	close(release)
	<-doneA

	// After Hub A releases, Hub B can acquire on the next tick.
	release2 := make(chan struct{})
	close(release2)
	var ranAfterRelease atomic.Bool
	schedB.singletonGuard(usageRetentionJobName, store.LockUsageRetention, func(context.Context) {
		ranAfterRelease.Store(true)
	})(ctx)
	assert.True(t, ranAfterRelease.Load(), "replica must be able to acquire lock after previous leader finishes")
}

func TestUsageRetention_HourlyHorizonFollowsConfig(t *testing.T) {
	srv, st := testServer(t)
	setUsageLedgerExperimentForTest(t, srv, true)
	ctx := context.Background()

	admin := &store.User{ID: uuid.NewString(), Email: "horizon-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, st.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, st, admin.ID, store.SystemRoleHubAdmin)

	proj := &store.Project{ID: uuid.NewString(), Name: "horizon-proj", Slug: "horizon-proj"}
	require.NoError(t, st.CreateProject(ctx, proj))
	agentID := uuid.NewString()
	runID := "run-horizon"
	_, err := st.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: runID, HubID: "hub-1", ProjectID: proj.ID, AgentID: agentID,
	})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Hour)
	// Seed hourly coverage starting 25 days ago so coverage covers both 5d and 20d windows.
	inTok := int64(50)
	_, err = st.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "evt-h-1", RunID: runID,
				ProjectID: proj.ID, AgentID: agentID,
				Model: "claude-sonnet-4-6", ModelSource: "reported", UsageSource: "native",
				Status: "ok", Calls: 1, Input: &inTok, TokenStatus: "complete",
				EventTS: now.AddDate(0, 0, -25), ReceivedAt: now,
				ProducerEpoch: "ep-h", ProducerSeq: 1,
			},
		},
	})
	require.NoError(t, err)

	// Simulate an established hub whose hourly coverage started 25 days ago.
	markerPayload, err := json.Marshal(map[string]string{"started_at": now.AddDate(0, 0, -25).UTC().Format(time.RFC3339)})
	require.NoError(t, err)
	_, err = st.UpsertHubSetting(ctx, store.UsageRollupHourlyCoverageSection, markerPayload, "test", -1, "seeded")
	require.NoError(t, err)

	reqUsage := func(since time.Time, groupByHour bool) *httptest.ResponseRecorder {
		u := "/api/v1/usage?since=" + since.Format(time.RFC3339) + "&until=" + now.Format(time.RFC3339)
		if groupByHour {
			u += "&groupBy=hour"
		}
		return doUserUsageReadRequest(t, srv, u, admin)
	}

	// 1. Configured hourly_rollups_days = 3: 5 days ago falls back to day, and groupBy=hour returns 400.
	srv.mu.Lock()
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Retention: &api.TelemetryRetentionConfig{HourlyRollupsDays: retentionIntPtr(3)},
	}
	srv.mu.Unlock()

	rec := reqUsage(now.AddDate(0, 0, -5), false)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, store.UsageGranularityDay, resp.Window.Granularity)

	recErr := reqUsage(now.AddDate(0, 0, -5), true)
	assert.Equal(t, http.StatusBadRequest, recErr.Code)

	// 2 days ago is within the 3-day hourly retention -> hour granularity.
	rec2 := reqUsage(now.AddDate(0, 0, -2), true)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Equal(t, store.UsageGranularityHour, resp.Window.Granularity)

	// 2. Configured hourly_rollups_days = 0 (unlimited): 20 days ago (after 25d coverage start) -> hour!
	srv.mu.Lock()
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Retention: &api.TelemetryRetentionConfig{HourlyRollupsDays: retentionIntPtr(0)},
	}
	srv.mu.Unlock()

	recUnlim := reqUsage(now.AddDate(0, 0, -20), true)
	require.Equal(t, http.StatusOK, recUnlim.Code)
	require.NoError(t, json.Unmarshal(recUnlim.Body.Bytes(), &resp))
	assert.Equal(t, store.UsageGranularityHour, resp.Window.Granularity)

	// Still respects coverage start: 30 days ago (before 25d coverage start) -> 400 on groupBy=hour.
	recBeforeCov := reqUsage(now.AddDate(0, 0, -30), true)
	assert.Equal(t, http.StatusBadRequest, recBeforeCov.Code)
}

// --- Test doubles ---

type funcExportCursorProvider func(ctx context.Context) (*store.UsageExportCursor, error)

func (f funcExportCursorProvider) UsageExportCursor(ctx context.Context) (*store.UsageExportCursor, error) {
	return f(ctx)
}

type fakeUsageRetentionStore struct {
	mu      sync.Mutex
	state   store.UsageRetentionState
	onSweep func(table string, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error)
}

func (f *fakeUsageRetentionStore) resetState() {
	f.mu.Lock()
	f.state = store.UsageRetentionState{}
	f.mu.Unlock()
}

func (f *fakeUsageRetentionStore) GetUsageRetentionState(_ context.Context) (*store.UsageRetentionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := f.state
	if f.state.LastCompletedAt != nil {
		t := *f.state.LastCompletedAt
		cp.LastCompletedAt = &t
	}
	if f.state.HourlySweptThrough != nil {
		t := *f.state.HourlySweptThrough
		cp.HourlySweptThrough = &t
	}
	return &cp, nil
}

func (f *fakeUsageRetentionStore) SaveUsageRetentionState(_ context.Context, st *store.UsageRetentionState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st == nil {
		f.state = store.UsageRetentionState{}
		return nil
	}
	cp := *st
	if st.LastCompletedAt != nil {
		t := *st.LastCompletedAt
		cp.LastCompletedAt = &t
	}
	if st.HourlySweptThrough != nil {
		t := *st.HourlySweptThrough
		cp.HourlySweptThrough = &t
	}
	f.state = cp
	return nil
}

func (f *fakeUsageRetentionStore) SweepUsageEventsBatch(ctx context.Context, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error) {
	return f.onSweep("usage_events", cutoff, cur, batchSize)
}
func (f *fakeUsageRetentionStore) SweepUsageRollupsHourlyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	return f.onSweep("usage_rollups_hourly", cutoff, nil, batchSize)
}
func (f *fakeUsageRetentionStore) SweepUsageRollupsDailyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	return f.onSweep("usage_rollups_daily", cutoff, nil, batchSize)
}
func (f *fakeUsageRetentionStore) SweepUsageScopeCountersDailyBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	return f.onSweep("usage_scope_counters_daily", cutoff, nil, batchSize)
}
func (f *fakeUsageRetentionStore) SweepMeterHealthBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	return f.onSweep("meter_health", cutoff, nil, batchSize)
}
func (f *fakeUsageRetentionStore) SweepRunAttributionBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	return f.onSweep("run_attribution", cutoff, nil, batchSize)
}
func (f *fakeUsageRetentionStore) SweepUsageBudgetStateBatch(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	return f.onSweep("usage_budget_state", cutoff, nil, batchSize)
}

type contendedAdvisoryLocker struct {
	mu   sync.Mutex
	held map[store.AdvisoryLockKey]bool
}

func newContendedAdvisoryLocker() *contendedAdvisoryLocker {
	return &contendedAdvisoryLocker{held: make(map[store.AdvisoryLockKey]bool)}
}

func (l *contendedAdvisoryLocker) TryAdvisoryLock(_ context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[key] {
		return false, func() error { return nil }, nil
	}
	l.held[key] = true
	return true, func() error {
		l.mu.Lock()
		delete(l.held, key)
		l.mu.Unlock()
		return nil
	}, nil
}

func (l *contendedAdvisoryLocker) TryAdvisoryLockObject(_ context.Context, _ store.AdvisoryLockKey, _ int32) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

type advisoryLockTestStore struct {
	store.Store
	store.AdvisoryLocker
}
