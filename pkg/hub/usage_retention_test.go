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
	assert.Equal(t, 0, calls["usage_events"])
	assert.Equal(t, 0, calls["usage_rollups_daily"])
	assert.Equal(t, 0, calls["run_attribution"])
	assert.Equal(t, 0, customCalled)
	assert.Equal(t, 1, calls["usage_rollups_hourly"])
	assert.Equal(t, 1, calls["meter_health"])

	// 2. Test P8 export cursor provider: fail-closed on error does not abort other tables.
	calls = map[string]int{}
	sw.SetExportCursorProvider(funcExportCursorProvider(func(context.Context) (*store.UsageExportCursor, error) {
		return nil, errors.New("gcs cursor store unavailable")
	}))
	cfgDefault := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{BatchSize: retentionIntPtr(5)})
	rep = sw.Run(ctx, cfgDefault)
	assert.Equal(t, 1, rep.ErrorCount)
	assert.Equal(t, 0, calls["usage_events"], "must fail-closed and not call SweepUsageEventsBatch on cursor error")
	assert.Equal(t, 1, calls["usage_rollups_hourly"], "other tables must still run")
	assert.Equal(t, 1, customCalled, "registered custom target must run")

	// 3. Test P8 export cursor provider returning a valid cursor + multi-batch loop.
	calls = map[string]int{}
	wantCursor := &store.UsageExportCursor{ReceivedAt: now.Add(-48 * time.Hour), EventID: "evt-watermark"}
	sw.SetExportCursorProvider(funcExportCursorProvider(func(context.Context) (*store.UsageExportCursor, error) {
		return wantCursor, nil
	}))
	rep = sw.Run(ctx, cfgDefault)
	assert.Equal(t, 0, rep.ErrorCount)
	assert.Equal(t, 3, calls["usage_events"], "batch sequence [5, 5, 2] with batchSize=5 must take exactly 3 calls")
	require.NotNil(t, lastCur)
	assert.Equal(t, wantCursor.EventID, lastCur.EventID)
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
	onSweep func(table string, cutoff time.Time, cur *store.UsageExportCursor, batchSize int) (int64, error)
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
