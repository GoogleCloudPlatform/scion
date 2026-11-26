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

package entadapter

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerolluphourly"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestUsageRead_RollupsPresenceMeterHealthAndDBNow(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	agentID := uuid.NewString()
	projectID := uuid.NewString()
	runID := "run-read-" + uuid.NewString()

	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:      runID,
		Generation: 1,
		HubID:      "hub-read-test",
		ProjectID:  projectID,
		AgentID:    agentID,
		Harness:    "claude",
	})
	require.NoError(t, err)

	// Hourly coverage marker is absent before any hourly rollup writes.
	_, hasCov, err := cs.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	assert.False(t, hasCov)

	day1 := time.Date(2026, 11, 20, 12, 15, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 21, 15, 30, 0, 0, time.UTC)

	int64p := func(v int64) *int64 { return &v }

	// Event 1 (day1): canonical (8, 5, 3, 2, reasoning=2), cache_write_1h=nil
	ev1 := &store.UsageEvent{
		HubID:         "hub-read-test",
		EventID:       "evt-read-1",
		RunID:         runID,
		ProjectID:     projectID,
		AgentID:       agentID,
		Harness:       "claude",
		Model:         "claude-sonnet-4-6",
		ModelSource:   telemetrycontract.ModelSourceEvent,
		UsageSource:   store.UsageSourceNative,
		Status:        telemetrycontract.StatusSuccess,
		Calls:         1,
		Input:         int64p(8),
		Output:        int64p(5),
		CacheRead:     int64p(3),
		CacheWrite:    int64p(2),
		CacheWrite1h:  nil,
		Reasoning:     int64p(2),
		TokenStatus:   telemetrycontract.TokenStatusReported,
		EventTS:       day1,
		ReceivedAt:    day1,
		ProducerEpoch: "ep-1",
		ProducerSeq:   1,
	}

	// Event 2 (day2): measured 0 cache_read, NULL cache_write & reasoning
	ev2 := &store.UsageEvent{
		HubID:         "hub-read-test",
		EventID:       "evt-read-2",
		RunID:         runID,
		ProjectID:     projectID,
		AgentID:       agentID,
		Harness:       "claude",
		Model:         "claude-haiku-4-5",
		ModelSource:   telemetrycontract.ModelSourceEvent,
		UsageSource:   store.UsageSourceNative,
		Status:        telemetrycontract.StatusSuccess,
		Calls:         1,
		Input:         int64p(10),
		Output:        int64p(4),
		CacheRead:     int64p(0),
		CacheWrite:    nil,
		CacheWrite1h:  nil,
		Reasoning:     nil,
		TokenStatus:   telemetrycontract.TokenStatusReported,
		EventTS:       day2,
		ReceivedAt:    day2,
		ProducerEpoch: "ep-1",
		ProducerSeq:   2,
	}

	dbNowBefore, err := cs.DBNow(ctx)
	require.NoError(t, err)

	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1, ev2},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: day2,
			ProducerSeq:   2,
			ReceivedSeqs:  []int64{1, 2},
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Accepted)

	// R1: Hourly coverage marker is set to ceil_hour(DB now) (the next complete
	// UTC hour after DB now), NOT the back-dated event timestamp (day1).
	covStart, hasCov, err := cs.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	require.True(t, hasCov)
	expectedMinCov := utcHourStart(dbNowBefore).Add(time.Hour)
	assert.False(t, covStart.Before(expectedMinCov), "coverage marker %s must be >= ceil_hour(dbNow) %s", covStart, expectedMinCov)
	assert.True(t, covStart.After(day2), "back-dated events must not pull coverage marker into the past")

	// Duplicate replay should not change daily/hourly rollups or presence counts.
	resDup, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, resDup.Accepted)
	assert.Equal(t, 1, resDup.Duplicates)

	// ListUsageRollupsDailyByAgent full window + presence counters
	rollups, err := cs.ListUsageRollupsDailyByAgent(ctx, agentID, day1, day2)
	require.NoError(t, err)
	require.Len(t, rollups, 2)
	assert.Equal(t, "claude-sonnet-4-6", rollups[0].Model)
	assert.Equal(t, int64(8), rollups[0].Input)
	assert.Equal(t, int64(5), rollups[0].Output)
	assert.Equal(t, int64(3), rollups[0].CacheRead)
	assert.Equal(t, int64(2), rollups[0].CacheWrite)
	assert.Equal(t, int64(2), rollups[0].Reasoning)
	assert.Equal(t, int64(1), rollups[0].InputKnownEvents)
	assert.Equal(t, int64(1), rollups[0].ReasoningKnownEvents)
	assert.Equal(t, int64(0), rollups[0].CacheWrite1hKnownEvents)
	assert.Equal(t, int64(1), rollups[0].PresenceTrackedEvents)

	assert.Equal(t, "claude-haiku-4-5", rollups[1].Model)
	assert.Equal(t, int64(1), rollups[1].CacheReadKnownEvents)  // measured 0
	assert.Equal(t, int64(0), rollups[1].CacheWriteKnownEvents) // NULL
	assert.Equal(t, int64(0), rollups[1].ReasoningKnownEvents)  // NULL
	assert.Equal(t, int64(1), rollups[1].PresenceTrackedEvents)

	// QueryUsageRollups on hourly table mirrors daily with UTC hour buckets.
	hourlyRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       day1,
		Until:       day2,
		AgentID:     agentID,
		GroupBy:     []string{store.UsageGroupByHour},
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, hourlyRes.Groups, 2)
	assert.Equal(t, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), *hourlyRes.Groups[0].BucketStart)
	assert.Equal(t, int64(8), hourlyRes.Groups[0].Input)
	assert.Equal(t, int64(1), hourlyRes.Groups[0].PresenceTrackedEvents)
	assert.Equal(t, time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC), *hourlyRes.Groups[1].BucketStart)
	assert.Equal(t, int64(1), hourlyRes.Groups[1].CacheReadKnownEvents)
	assert.Equal(t, int64(0), hourlyRes.Groups[1].CacheWriteKnownEvents)

	// Narrow window (day1 only)
	rollupsDay1, err := cs.ListUsageRollupsDailyByAgent(ctx, agentID, day1, day1)
	require.NoError(t, err)
	require.Len(t, rollupsDay1, 1)
	assert.Equal(t, "claude-sonnet-4-6", rollupsDay1[0].Model)

	// ListMeterHealthByAgent
	mhs, err := cs.ListMeterHealthByAgent(ctx, agentID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, runID, mhs[0].RunID)

	// HasUsageEventsForRun distinguishes runs with events from runs without.
	hasEv, err := cs.HasUsageEventsForRun(ctx, agentID, runID)
	require.NoError(t, err)
	assert.True(t, hasEv)
	hasEvNone, err := cs.HasUsageEventsForRun(ctx, agentID, "run-without-events")
	require.NoError(t, err)
	assert.False(t, hasEvNone)

	// DBNow returns a recent UTC timestamp
	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC(), dbNow, 30*time.Second)
}

func TestUsageRead_CountStaleMetersByProject(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	now := time.Now().UTC().Truncate(time.Microsecond)
	staleCutoff := now.Add(-3 * time.Minute)
	staleReceipt := staleCutoff.Add(-30 * time.Second)
	freshReceipt := staleCutoff.Add(30 * time.Second)

	projA := uuid.NewString()
	projB := uuid.NewString()
	projExcluded := uuid.NewString()

	for _, pid := range []string{projA, projB, projExcluded} {
		require.NoError(t, cs.CreateProject(ctx, &store.Project{
			ID:   pid,
			Name: pid,
			Slug: pid,
		}))
	}

	addRun := func(projectID, agentID, currentRunID, phase string, deleted bool, mhRunID string, updates ...*store.UsageMeterHealthUpdate) {
		ag := &store.Agent{
			ID:        agentID,
			Slug:      "slug-" + agentID,
			Name:      "name-" + agentID,
			ProjectID: projectID,
			Phase:     phase,
			RunID:     currentRunID,
		}
		require.NoError(t, cs.CreateAgent(ctx, ag))
		updAg := client.Agent.UpdateOneID(uuid.MustParse(agentID)).SetRunID(currentRunID)
		if deleted {
			delAt := now.Add(-1 * time.Minute)
			updAg = updAg.SetDeletedAt(delAt)
		}
		require.NoError(t, updAg.Exec(ctx))
		_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
			RunID:      mhRunID,
			Generation: 1,
			HubID:      "hub-stale-test",
			ProjectID:  projectID,
			AgentID:    agentID,
			AgentSlug:  "slug-" + agentID,
			Harness:    "claude",
		})
		require.NoError(t, err)
		for _, upd := range updates {
			_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
				AgentID:     agentID,
				MeterHealth: upd,
			})
			require.NoError(t, err)
		}
	}

	newID := func() string { return uuid.NewString() }

	// Project A:
	// 1. Stale open run on active running agent -> counted
	agA1 := newID()
	addRun(projA, agA1, "run-a1", "running", false, "run-a1", &store.UsageMeterHealthUpdate{
		AgentID:       agA1,
		RunID:         "run-a1",
		ProducerEpoch: "ep-1",
		LastReceiptAt: staleReceipt,
		FinalState:    store.MeterHealthFinalStateOpen,
	})
	// 2. Fresh open run -> NOT counted
	agA2 := newID()
	addRun(projA, agA2, "run-a2", "running", false, "run-a2", &store.UsageMeterHealthUpdate{
		AgentID:       agA2,
		RunID:         "run-a2",
		ProducerEpoch: "ep-1",
		LastReceiptAt: freshReceipt,
		FinalState:    store.MeterHealthFinalStateOpen,
	})
	// 3. Closed (final) run with old receipt -> NOT counted
	agA3 := newID()
	addRun(projA, agA3, "run-a3", "running", false, "run-a3", &store.UsageMeterHealthUpdate{
		AgentID:       agA3,
		RunID:         "run-a3",
		ProducerEpoch: "ep-1",
		LastReceiptAt: staleReceipt,
		FinalState:    store.MeterHealthFinalStateFinal,
	})
	// 4. Multi-epoch run where ep-1 is stale open, ep-2 is fresh open -> NOT counted
	agA4 := newID()
	addRun(projA, agA4, "run-a4", "running", false, "run-a4",
		&store.UsageMeterHealthUpdate{
			AgentID:       agA4,
			RunID:         "run-a4",
			ProducerEpoch: "ep-1",
			LastReceiptAt: staleReceipt,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
		&store.UsageMeterHealthUpdate{
			AgentID:       agA4,
			RunID:         "run-a4",
			ProducerEpoch: "ep-2",
			LastReceiptAt: freshReceipt,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	)
	// 5. Multi-epoch run where ep-1 is stale open, ep-2 is newer final -> NOT counted
	agA5 := newID()
	addRun(projA, agA5, "run-a5", "running", false, "run-a5",
		&store.UsageMeterHealthUpdate{
			AgentID:       agA5,
			RunID:         "run-a5",
			ProducerEpoch: "ep-1",
			LastReceiptAt: staleReceipt,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
		&store.UsageMeterHealthUpdate{
			AgentID:       agA5,
			RunID:         "run-a5",
			ProducerEpoch: "ep-2",
			LastReceiptAt: staleReceipt.Add(10 * time.Second),
			FinalState:    store.MeterHealthFinalStateFinal,
		},
	)

	// Project B: 2 stale open runs on active running agents
	agB1 := newID()
	addRun(projB, agB1, "run-b1", "running", false, "run-b1", &store.UsageMeterHealthUpdate{
		AgentID:       agB1,
		RunID:         "run-b1",
		ProducerEpoch: "ep-1",
		LastReceiptAt: staleReceipt,
		FinalState:    store.MeterHealthFinalStateOpen,
	})
	agB2 := newID()
	addRun(projB, agB2, "run-b2", "running", false, "run-b2", &store.UsageMeterHealthUpdate{
		AgentID:       agB2,
		RunID:         "run-b2",
		ProducerEpoch: "ep-1",
		LastReceiptAt: staleReceipt.Add(-5 * time.Minute),
		FinalState:    store.MeterHealthFinalStateOpen,
	})

	// Excluded project (R3): all 3 must report 0 (absent from counts).
	// (a) Superseded run: agent now has run-excl-new, old open stale row is on run-excl-old
	agEx1 := newID()
	addRun(projExcluded, agEx1, "run-excl-new", "running", false, "run-excl-old", &store.UsageMeterHealthUpdate{
		AgentID:       agEx1,
		RunID:         "run-excl-old",
		ProducerEpoch: "ep-1",
		LastReceiptAt: staleReceipt,
		FinalState:    store.MeterHealthFinalStateOpen,
	})
	// (b) Soft-deleted agent with stale open row on its run
	agEx2 := newID()
	addRun(projExcluded, agEx2, "run-excl-del", "running", true, "run-excl-del", &store.UsageMeterHealthUpdate{
		AgentID:       agEx2,
		RunID:         "run-excl-del",
		ProducerEpoch: "ep-1",
		LastReceiptAt: staleReceipt,
		FinalState:    store.MeterHealthFinalStateOpen,
	})
	// (c) Stopped agent with stale open row on its run
	agEx3 := newID()
	addRun(projExcluded, agEx3, "run-excl-stop", "stopped", false, "run-excl-stop", &store.UsageMeterHealthUpdate{
		AgentID:       agEx3,
		RunID:         "run-excl-stop",
		ProducerEpoch: "ep-1",
		LastReceiptAt: staleReceipt,
		FinalState:    store.MeterHealthFinalStateOpen,
	})

	counts, err := cs.CountStaleMetersByProject(ctx, staleCutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts[projA])
	assert.Equal(t, int64(2), counts[projB])
	assert.Equal(t, int64(0), counts[projExcluded], "superseded, soft-deleted, and stopped runs must not count as stale")
	assert.Len(t, counts, 2)
}

// TestUsageRead_CoverageMarkerUpgradeHour verifies R1 on both SQLite and
// Postgres: pre-upgrade daily-only events written earlier in the upgrade hour
// (and back-dated events in the first post-upgrade batch) never cause an hourly
// read to silently under-count, and windows starting at or after the marker
// match daily totals identically.
func TestUsageRead_CoverageMarkerUpgradeHour(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	csPre := NewCompositeStore(client)
	t.Cleanup(func() { _ = csPre.Close() })

	projID := createTestProject(t, csPre, "r1-upgrade-hour")
	userID := createTestUser(t, csPre, "r1-user@example.com")
	agentID := uuid.NewString()
	runID := "run-r1-" + uuid.NewString()
	seedRunForIngestTest(t, csPre, projID, agentID, runID, &userID)

	dbNow, err := csPre.DBNow(ctx)
	require.NoError(t, err)
	upgradeHour := utcHourStart(dbNow)

	// 1. Simulate pre-upgrade binary ingesting event A at HH:05 (daily-only).
	evA := makeStoreUsageEvent("hub-r1", "evt-r1-pre", runID, projID, agentID, &userID, 1, upgradeHour.Add(5*time.Minute), 100, 20, 0, 0)
	_, err = csPre.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{AgentID: agentID, Events: []*store.UsageEvent{evA}})
	require.NoError(t, err)
	// Remove hourly rows and coverage marker to replicate pre-upgrade DB state.
	_, err = client.UsageRollupHourly.Delete().Exec(ctx)
	require.NoError(t, err)
	_, err = client.HubSetting.Delete().Where(hubsetting.SectionEQ(store.UsageRollupHourlyCoverageSection)).Exec(ctx)
	require.NoError(t, err)

	// 2. Fresh post-upgrade store ingests event B at HH:20 AND back-dated event C from 3 hours earlier.
	csPost := NewCompositeStore(client)
	evB := makeStoreUsageEvent("hub-r1", "evt-r1-post", runID, projID, agentID, &userID, 2, upgradeHour.Add(20*time.Minute), 10, 5, 0, 0)
	evC := makeStoreUsageEvent("hub-r1", "evt-r1-backdated", runID, projID, agentID, &userID, 3, upgradeHour.Add(-3*time.Hour), 50, 10, 0, 0)
	_, err = csPost.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{AgentID: agentID, Events: []*store.UsageEvent{evB, evC}})
	require.NoError(t, err)

	covStart, hasCov, err := csPost.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	require.True(t, hasCov)
	// Marker must be strictly after upgradeHour (ceil_hour(DB now) >= upgradeHour + 1h),
	// so upgradeHour is NOT considered hourly-covered and queries fall back to daily.
	assert.True(t, covStart.After(upgradeHour), "marker %s must be after upgrade hour %s", covStart, upgradeHour)

	// 3. Ingest event D in the covered hour (covStart + 5m) and verify hourly == daily.
	evD := makeStoreUsageEvent("hub-r1", "evt-r1-covered", runID, projID, agentID, &userID, 4, covStart.Add(5*time.Minute), 77, 33, 4, 2)
	_, err = csPost.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{AgentID: agentID, Events: []*store.UsageEvent{evD}})
	require.NoError(t, err)

	// Hourly vs daily for a full day starting at covStart's day (if same day, compare covStart hour via hourly):
	// Specifically verify covStart hour contains event D completely in hourly rollups.
	hRow, err := client.UsageRollupHourly.Query().
		Where(usagerolluphourly.BucketStartEQ(covStart), usagerolluphourly.AgentIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(77), hRow.Input)
	assert.Equal(t, int64(33), hRow.Output)

	// In a clean day starting at covStart (simulated by ingesting into covStart + 24h), hourly == daily.
	nextDay := utcDayStart(covStart).Add(24 * time.Hour)
	evE := makeStoreUsageEvent("hub-r1", "evt-r1-nextday", runID, projID, agentID, &userID, 5, nextDay.Add(15*time.Minute), 42, 18, 6, 3)
	_, err = csPost.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{AgentID: agentID, Events: []*store.UsageEvent{evE}})
	require.NoError(t, err)

	hRes, err := csPost.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       nextDay,
		Until:       nextDay.Add(23 * time.Hour),
		ProjectID:   projID,
	})
	require.NoError(t, err)
	dRes, err := csPost.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		Since:       nextDay,
		Until:       nextDay.Add(23 * time.Hour),
		ProjectID:   projID,
	})
	require.NoError(t, err)
	assert.Equal(t, dRes.Totals.Input, hRes.Totals.Input)
	assert.Equal(t, dRes.Totals.Output, hRes.Totals.Output)
	assert.Equal(t, dRes.Totals.Calls, hRes.Totals.Calls)
}

// TestUsageRead_SumOverflowSaturates verifies R2 on both SQLite and Postgres:
// summing multiple rollup rows near math.MaxInt64 never fails with integer
// overflow or bigint out of range, clamps to math.MaxInt64, and sets Saturated.
func TestUsageRead_SumOverflowSaturates(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projID := createTestProject(t, cs, "r2-overflow")
	day := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)

	// Seed two daily rollup rows in the same project with values near MaxInt64.
	_, err := client.UsageRollupDaily.Create().
		SetBucketStart(day).
		SetProjectID(projID).
		SetBillingUserID("u1").
		SetAgentID("ag-1").
		SetHarness("claude").
		SetModel("claude-sonnet-4-6").
		SetUsageSource(store.UsageSourceNative).
		SetCalls(10).
		SetInput(math.MaxInt64 - 100).
		SetOutput(math.MaxInt64 - 50).
		SetEvents(10).
		SetInputKnownEvents(10).
		SetOutputKnownEvents(10).
		SetPresenceTrackedEvents(10).
		SetFirstEventTs(day).
		SetLastEventTs(day).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.UsageRollupDaily.Create().
		SetBucketStart(day).
		SetProjectID(projID).
		SetBillingUserID("u1").
		SetAgentID("ag-2").
		SetHarness("claude").
		SetModel("claude-opus-4-6").
		SetUsageSource(store.UsageSourceNative).
		SetCalls(20).
		SetInput(math.MaxInt64 - 10).
		SetOutput(500).
		SetEvents(20).
		SetInputKnownEvents(20).
		SetOutputKnownEvents(20).
		SetPresenceTrackedEvents(20).
		SetFirstEventTs(day).
		SetLastEventTs(day).
		Save(ctx)
	require.NoError(t, err)

	// 1. Totals-only query.
	totRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		Since:       day,
		Until:       day,
		ProjectID:   projID,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), totRes.Totals.Input)
	assert.Equal(t, int64(math.MaxInt64), totRes.Totals.Output)
	assert.Equal(t, int64(30), totRes.Totals.Calls)
	assert.True(t, totRes.Totals.Saturated)

	// 2. Merged groupBy=project query.
	projRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		Since:       day,
		Until:       day,
		ProjectID:   projID,
		GroupBy:     []string{store.UsageGroupByProject},
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, projRes.Groups, 1)
	assert.Equal(t, int64(math.MaxInt64), projRes.Groups[0].Input)
	assert.Equal(t, int64(math.MaxInt64), projRes.Groups[0].Output)
	assert.True(t, projRes.Groups[0].Saturated)

	// 3. Ordered groupBy=model query (exercises ORDER BY across large rows).
	modelRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		Since:       day,
		Until:       day,
		ProjectID:   projID,
		GroupBy:     []string{store.UsageGroupByModel},
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, modelRes.Groups, 2)
}

func TestUsageRead_QueryUsageRollupsAndScopeCoverage(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projA := createTestProject(t, cs, "usage-query-a")
	projB := createTestProject(t, cs, "usage-query-b")
	user1 := createTestUser(t, cs, "usage-u1@example.com")
	user2 := createTestUser(t, cs, "usage-u2@example.com")

	agMeasured := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ag-measured",
		Name:      "ag-measured",
		ProjectID: projA,
		OwnerID:   user1,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, cs.CreateAgent(ctx, agMeasured))
	_, err := cs.SetAgentRunID(ctx, agMeasured.ID, "run-m-1", nil)
	require.NoError(t, err)
	agMeasured.RunID = "run-m-1"

	agStale := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ag-stale",
		Name:      "ag-stale",
		ProjectID: projA,
		OwnerID:   user1,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, cs.CreateAgent(ctx, agStale))
	_, err = cs.SetAgentRunID(ctx, agStale.ID, "run-s-1", nil)
	require.NoError(t, err)
	agStale.RunID = "run-s-1"

	// O1b: Stopped agent with an old open meter_health must NOT be flagged as stale.
	agStoppedOpen := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ag-stopped-open",
		Name:      "ag-stopped-open",
		ProjectID: projA,
		OwnerID:   user1,
		Phase:     string(state.PhaseStopped),
	}
	require.NoError(t, cs.CreateAgent(ctx, agStoppedOpen))
	_, err = cs.SetAgentRunID(ctx, agStoppedOpen.ID, "run-so-1", nil)
	require.NoError(t, err)
	agStoppedOpen.RunID = "run-so-1"

	// O1a: Never-started agent (no run_attribution) must NOT be counted in coverage.
	agNeverStarted := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ag-never-started",
		Name:      "ag-never-started",
		ProjectID: projA,
		OwnerID:   user1,
		Phase:     string(state.PhaseCreated),
	}
	require.NoError(t, cs.CreateAgent(ctx, agNeverStarted))

	agUnmeasured := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ag-unmeasured",
		Name:      "ag-unmeasured",
		ProjectID: projB,
		OwnerID:   user2,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, cs.CreateAgent(ctx, agUnmeasured))
	_, err = cs.SetAgentRunID(ctx, agUnmeasured.ID, "run-u-1", nil)
	require.NoError(t, err)
	agUnmeasured.RunID = "run-u-1"

	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	seedRunAt := func(projID, agID, runID string, uid *string, createdAt time.Time) {
		_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
			RunID: runID, Generation: 1, HubID: "hub-q", ProjectID: projID,
			AgentID: agID, BillingUserID: uid, Harness: "claude", CreatedAt: createdAt,
		})
		require.NoError(t, err)
	}
	seedRunAt(projA, agMeasured.ID, agMeasured.RunID, &user1, base)
	seedRunAt(projA, agStale.ID, agStale.RunID, &user1, base)
	seedRunAt(projA, agStoppedOpen.ID, agStoppedOpen.RunID, &user1, base)
	seedRunAt(projB, agUnmeasured.ID, agUnmeasured.RunID, &user2, base)

	ev1 := makeStoreUsageEvent("hub-q", "q-1", agMeasured.RunID, projA, agMeasured.ID, &user1, 1, base.Add(5*time.Minute), 100, 50, 10, 0)
	ev1.Model = "claude-sonnet-4-6"
	ev1.Reasoning = i64p(20)

	ev2 := makeStoreUsageEvent("hub-q", "q-2", agMeasured.RunID, projA, agMeasured.ID, &user1, 2, base.Add(65*time.Minute), 200, 80, 0, 5)
	ev2.Model = "claude-haiku-4-5"
	ev2.Reasoning = nil

	ev3 := makeStoreUsageEvent("hub-q", "q-3", agStale.RunID, projA, agStale.ID, &user1, 1, base.Add(125*time.Minute), 40, 10, 0, 0)
	ev3.Model = "claude-opus-4-6"
	ev3.CacheRead = nil
	ev3.CacheWrite = nil

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agMeasured.ID,
		Events:  []*store.UsageEvent{ev1, ev2},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agMeasured.ID,
			RunID:         agMeasured.RunID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: base.Add(2 * time.Hour),
			ProducerSeq:   2,
			ReceivedSeqs:  []int64{1, 2},
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agStale.ID,
		Events:  []*store.UsageEvent{ev3},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agStale.ID,
			RunID:         agStale.RunID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: base.Add(10 * time.Minute), // stale relative to base+2h
			ProducerSeq:   1,
			ReceivedSeqs:  []int64{1},
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// Stopped agent with old open meter_health and an event.
	evStopped := makeStoreUsageEvent("hub-q", "q-4", agStoppedOpen.RunID, projA, agStoppedOpen.ID, &user1, 1, base.Add(15*time.Minute), 12, 8, 0, 0)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agStoppedOpen.ID,
		Events:  []*store.UsageEvent{evStopped},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agStoppedOpen.ID,
			RunID:         agStoppedOpen.RunID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: base.Add(15 * time.Minute),
			ProducerSeq:   1,
			ReceivedSeqs:  []int64{1},
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// Hourly query grouped by model with limit=2 -> truncated=true, exact totals preserved.
	res, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       base,
		Until:       base.Add(3 * time.Hour),
		ProjectID:   projA,
		GroupBy:     []string{store.UsageGroupByModel},
		Limit:       2,
	})
	require.NoError(t, err)
	assert.True(t, res.Truncated)
	assert.Len(t, res.Groups, 2)
	assert.Equal(t, int64(352), res.Totals.Input)
	assert.Equal(t, int64(148), res.Totals.Output)
	assert.Equal(t, int64(4), res.Totals.Events)
	assert.Equal(t, int64(4), res.Totals.PresenceTrackedEvents)

	// O3: Hourly query grouped by hour with limit=2 over 3 hours -> keeps the 2 MOST RECENT hours, presented ASC!
	hourRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       base,
		Until:       base.Add(3 * time.Hour),
		ProjectID:   projA,
		GroupBy:     []string{store.UsageGroupByHour},
		Limit:       2,
	})
	require.NoError(t, err)
	assert.True(t, hourRes.Truncated)
	require.Len(t, hourRes.Groups, 2)
	assert.Equal(t, base.Add(time.Hour), *hourRes.Groups[0].BucketStart, "must keep 2nd most recent hour first (ASC)")
	assert.Equal(t, base.Add(2*time.Hour), *hourRes.Groups[1].BucketStart, "must keep most recent hour last (ASC)")

	// Hourly table grouped by day collapses all hours into 1 UTC day bucket.
	dayFromHourly, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       base,
		Until:       base.Add(3 * time.Hour),
		ProjectID:   projA,
		GroupBy:     []string{store.UsageGroupByDay},
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, dayFromHourly.Groups, 1)
	assert.Equal(t, time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), *dayFromHourly.Groups[0].BucketStart)
	assert.Equal(t, int64(352), dayFromHourly.Groups[0].Input)

	// O1: Scope coverage buckets for projA (3 started agents; agNeverStarted excluded).
	dbNow := base.Add(2*time.Hour + 1*time.Minute)
	covA, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:  projA,
		Since:      base,
		Until:      dbNow,
		StaleAfter: 3 * time.Minute,
		DBNow:      dbNow,
	})
	require.NoError(t, err)
	totalA := 0
	runningStaleA := 0
	stoppedOpenA := 0
	for _, b := range covA.Buckets {
		totalA += b.AgentCount
		if b.IsRunning && b.IsReceiptStale {
			runningStaleA += b.AgentCount
		}
		if !b.IsRunning && b.HasMeterHealth && b.IsReceiptStale {
			stoppedOpenA += b.AgentCount
		}
	}
	assert.Equal(t, 3, totalA, "agNeverStarted must be excluded from coverage")
	assert.Equal(t, 1, runningStaleA, "only running agStale is stale")
	assert.Equal(t, 1, stoppedOpenA, "agStoppedOpen has IsRunning=false so Hub derives measured, not stale")
	require.NotNil(t, covA.LastReceiptAt)
	assert.Equal(t, base.Add(2*time.Hour), *covA.LastReceiptAt)

	// O1d: agent_id filter narrows coverage to the single agent.
	covSingle, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:  projA,
		AgentID:    agMeasured.ID,
		Since:      base,
		Until:      dbNow,
		StaleAfter: 3 * time.Minute,
		DBNow:      dbNow,
	})
	require.NoError(t, err)
	totalSingle := 0
	for _, b := range covSingle.Buckets {
		totalSingle += b.AgentCount
	}
	assert.Equal(t, 1, totalSingle)
}
