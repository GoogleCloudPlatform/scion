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
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

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

	// Historical window (until < dbNow - staleAfter):
	// 1. A running, stale agent with NO runs, receipts, or rollups in the past
	//    window must NOT be counted for that window.
	// 2. A running, stale agent that DID have a rollup in the past window is
	//    counted as measured (IsRunning=false, IsReceiptStale=false), not stale.
	histSince := base.AddDate(0, 0, -10)
	histUntil := base.AddDate(0, 0, -5)
	covHistEmpty, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:  projA,
		Since:      histSince,
		Until:      histUntil,
		StaleAfter: 3 * time.Minute,
		DBNow:      dbNow,
	})
	require.NoError(t, err)
	assert.Empty(t, covHistEmpty.Buckets, "running stale agents with no activity in a past window must not be counted")

	// Seed an older run attribution + historical daily rollup for agStale
	// (which is currently running with a stale open meter at base+10m).
	histRunID := "run-hist-" + uuid.NewString()
	_, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         histRunID,
		Generation:    0,
		HubID:         "hub-q",
		ProjectID:     projA,
		AgentID:       agStale.ID,
		BillingUserID: &user1,
		Harness:       "claude",
		CreatedAt:     histSince.Add(time.Hour),
	})
	require.NoError(t, err)
	evHist := makeStoreUsageEvent("hub-q", "q-hist-1", histRunID, projA, agStale.ID, &user1, 1, histSince.Add(2*time.Hour), 50, 25, 0, 0)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agStale.ID,
		Events:  []*store.UsageEvent{evHist},
	})
	require.NoError(t, err)

	covHistActive, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:  projA,
		Since:      histSince,
		Until:      histUntil,
		StaleAfter: 3 * time.Minute,
		DBNow:      dbNow,
	})
	require.NoError(t, err)
	require.Len(t, covHistActive.Buckets, 1)
	assert.Equal(t, 1, covHistActive.Buckets[0].AgentCount)
	assert.False(t, covHistActive.Buckets[0].IsRunning, "historical window must not set IsRunning")
	assert.False(t, covHistActive.Buckets[0].IsReceiptStale, "historical window must not mark a running agent's current stale meter as stale")
	assert.True(t, covHistActive.Buckets[0].HasUsageData)

	// User-scoped coverage (driven from run_attribution(billing_user_id, created_at)
	// with ra2/rd_w/rd narrowed by billing_user_id): exercises user_agents CTE
	// on both SQLite and Postgres.
	covUser, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		BillingUserID: user1,
		Since:         base,
		Until:         dbNow,
		StaleAfter:    3 * time.Minute,
		DBNow:         dbNow,
	})
	require.NoError(t, err)
	totalUser := 0
	for _, b := range covUser.Buckets {
		totalUser += b.AgentCount
	}
	assert.Equal(t, 3, totalUser, "user1 owns the 3 started agents in projA")
}

// TestUsageRead_HourlyConsistencyMixedBinary verifies on both SQLite and
// Postgres that CheckUsageHourlyConsistency detects an old-binary write
// (a usage_events row plus a usage_rollups_daily increment with no hourly row)
// on the coverage-marker day itself (M+5m) and on later days, while passing
// for clean multi-day windows.
func TestUsageRead_HourlyConsistencyMixedBinary(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs1 := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs1.Close() })

	projID := createTestProject(t, cs1, "mixed-binary-consistency")
	userID := createTestUser(t, cs1, "mixed-binary@example.com")
	agentID := uuid.NewString()
	runID := "run-mixed-" + uuid.NewString()
	seedRunForIngestTest(t, cs1, projID, agentID, runID, &userID)

	// 1. New binary ingests an initial event, setting the write-once coverage marker M.
	dbNow, err := cs1.DBNow(ctx)
	require.NoError(t, err)
	evInit := makeStoreUsageEvent("hub-mix", "evt-mix-init", runID, projID, agentID, &userID, 1, dbNow, 10, 5, 0, 0)
	_, err = cs1.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{evInit},
	})
	require.NoError(t, err)

	covStart, hasCov, err := cs1.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	require.True(t, hasCov)

	// (a) Same-day M+5m probe on the coverage-marker day itself.
	evSameDayOld := makeStoreUsageEvent("hub-mix", "evt-mix-m5", runID, projID, agentID, &userID, 2, covStart.Add(5*time.Minute), 80, 20, 0, 0)
	_, err = cs1.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{evSameDayOld},
	})
	require.NoError(t, err)
	_, err = client.UsageRollupHourly.Delete().
		Where(usagerolluphourly.BucketStartEQ(covStart)).
		Exec(ctx)
	require.NoError(t, err)

	okSameDay, err := cs1.CheckUsageHourlyConsistency(ctx, store.UsageRollupQuery{
		Since:     covStart,
		Until:     covStart.Add(2 * time.Hour),
		ProjectID: projID,
	})
	require.NoError(t, err)
	assert.False(t, okSameDay, "old-binary write at M+5m on the coverage day must fail hourly consistency check")

	// (b) Later-day old-binary write + clean multi-day window.
	dayBad := utcDayStart(covStart).AddDate(0, 0, 1)
	dayClean1 := dayBad.AddDate(0, 0, 1)
	dayClean2 := dayClean1.AddDate(0, 0, 1)

	evOldBinary := makeStoreUsageEvent("hub-mix", "evt-mix-oldbin", runID, projID, agentID, &userID, 3, dayBad.Add(2*time.Hour+5*time.Minute), 100, 40, 0, 0)
	_, err = cs1.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{evOldBinary},
	})
	require.NoError(t, err)
	_, err = client.UsageRollupHourly.Delete().
		Where(usagerolluphourly.BucketStartEQ(dayBad.Add(2 * time.Hour))).
		Exec(ctx)
	require.NoError(t, err)

	cs2 := NewCompositeStore(client)
	evNewBadDay := makeStoreUsageEvent("hub-mix", "evt-mix-new1", runID, projID, agentID, &userID, 4, dayBad.Add(5*time.Hour), 20, 10, 0, 0)
	evNewClean1 := makeStoreUsageEvent("hub-mix", "evt-mix-new2", runID, projID, agentID, &userID, 5, dayClean1.Add(3*time.Hour), 30, 15, 0, 0)
	evNewClean2 := makeStoreUsageEvent("hub-mix", "evt-mix-new3", runID, projID, agentID, &userID, 6, dayClean2.Add(9*time.Hour), 45, 25, 0, 0)
	_, err = cs2.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{evNewBadDay, evNewClean1, evNewClean2},
	})
	require.NoError(t, err)

	okBad, err := cs2.CheckUsageHourlyConsistency(ctx, store.UsageRollupQuery{
		Since:     dayBad.Add(4 * time.Hour),
		Until:     dayBad.Add(6 * time.Hour),
		ProjectID: projID,
	})
	require.NoError(t, err)
	assert.False(t, okBad, "partial later-day window overlapping dayBad must fail hourly consistency check")

	okSpan, err := cs2.CheckUsageHourlyConsistency(ctx, store.UsageRollupQuery{
		Since:     dayBad,
		Until:     dayClean1,
		ProjectID: projID,
	})
	require.NoError(t, err)
	assert.False(t, okSpan, "range including dayBad must fail hourly consistency check")

	okCleanMulti, err := cs2.CheckUsageHourlyConsistency(ctx, store.UsageRollupQuery{
		Since:     dayClean1.Add(2 * time.Hour),
		Until:     dayClean2.Add(12 * time.Hour),
		ProjectID: projID,
	})
	require.NoError(t, err)
	assert.True(t, okCleanMulti, "clean multi-day window must pass hourly consistency check")
}

func TestCountUsageActiveAgents_HourlyDailyAndScope(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projA := createTestProject(t, cs, "active-agents-proj-a")
	projB := createTestProject(t, cs, "active-agents-proj-b")
	user1 := createTestUser(t, cs, "active-agents-u1@example.com")
	user2 := createTestUser(t, cs, "active-agents-u2@example.com")

	ag1 := uuid.NewString()
	ag2 := uuid.NewString()
	agOtherProj := uuid.NewString()
	run1 := "run-" + uuid.NewString()
	run2 := "run-" + uuid.NewString()
	runOther := "run-" + uuid.NewString()

	seedRunForIngestTest(t, cs, projA, ag1, run1, &user1)
	seedRunForIngestTest(t, cs, projA, ag2, run2, &user1)
	seedRunForIngestTest(t, cs, projB, agOtherProj, runOther, &user2)

	day1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)

	// ag1 is active in two hours on day1 and one hour on day2; ag2 is active on day2.
	_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag1,
		Events: []*store.UsageEvent{
			makeStoreUsageEvent("hub-aa", "aa-1", run1, projA, ag1, &user1, 1, day1, 10, 5, 0, 0),
			makeStoreUsageEvent("hub-aa", "aa-2", run1, projA, ag1, &user1, 2, day1.Add(time.Hour), 20, 5, 0, 0),
			makeStoreUsageEvent("hub-aa", "aa-3", run1, projA, ag1, &user1, 3, day2, 30, 5, 0, 0),
		},
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag2,
		Events: []*store.UsageEvent{
			makeStoreUsageEvent("hub-aa", "aa-4", run2, projA, ag2, &user1, 1, day2.Add(time.Hour), 15, 5, 0, 0),
		},
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agOtherProj,
		Events: []*store.UsageEvent{
			makeStoreUsageEvent("hub-aa", "aa-5", runOther, projB, agOtherProj, &user2, 1, day2, 50, 10, 0, 0),
		},
	})
	require.NoError(t, err)

	// Daily granularity for projA: TotalUniqueAgents=2 (never 1+2=3), per-day buckets=[1, 2].
	dailyRes, err := cs.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
		Granularity:    store.UsageGranularityDay,
		Since:          day1.Add(-time.Hour),
		Until:          day2.Add(2 * time.Hour),
		ProjectID:      projA,
		IncludeBuckets: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, dailyRes.TotalUniqueAgents)
	require.Len(t, dailyRes.BucketCounts, 2)
	assert.Equal(t, time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), dailyRes.BucketCounts[0].BucketStart)
	assert.Equal(t, int64(1), dailyRes.BucketCounts[0].Count)
	assert.Equal(t, time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), dailyRes.BucketCounts[1].BucketStart)
	assert.Equal(t, int64(2), dailyRes.BucketCounts[1].Count)

	// Hourly granularity for projA: TotalUniqueAgents=2, 4 distinct (hour, agent_id) pairs.
	hourlyRes, err := cs.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
		Granularity:    store.UsageGranularityHour,
		Since:          day1,
		Until:          day2.Add(2 * time.Hour),
		ProjectID:      projA,
		IncludeBuckets: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, hourlyRes.TotalUniqueAgents)
	assert.False(t, hourlyRes.Truncated)
	require.Len(t, hourlyRes.BucketAgents, 4)

	// Hourly granularity with Limit=2 -> Truncated=true, keeps the 2 newest pairs (day2), TotalUniqueAgents stays exact.
	hourlyTrunc, err := cs.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
		Granularity:    store.UsageGranularityHour,
		Since:          day1,
		Until:          day2.Add(2 * time.Hour),
		ProjectID:      projA,
		IncludeBuckets: true,
		Limit:          2,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, hourlyTrunc.TotalUniqueAgents)
	assert.True(t, hourlyTrunc.Truncated)
	require.Len(t, hourlyTrunc.BucketAgents, 2)
	assert.Equal(t, day2, hourlyTrunc.BucketAgents[0].BucketStart)
	assert.Equal(t, day2.Add(time.Hour), hourlyTrunc.BucketAgents[1].BucketStart)

	// Global scope includes agOtherProj -> TotalUniqueAgents=3.
	globalRes, err := cs.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
		Granularity: store.UsageGranularityDay,
		Since:       day1.Add(-time.Hour),
		Until:       day2.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, globalRes.TotalUniqueAgents)
}

func TestCountSessionMetrics_DeduplicatesAcrossSegmentsAndDays(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projA := createTestProject(t, cs, "sess-count-proj-a")
	projB := createTestProject(t, cs, "sess-count-proj-b")
	ag1 := uuid.NewString()
	ag2 := uuid.NewString()

	t1 := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
	tOut := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)

	// Session s1 spans two days (two segments for same agent_id + session_id).
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag1, ProjectID: projA, SessionID: "s1", StartedAt: t1,
	}))
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag1, ProjectID: projA, SessionID: "s1", StartedAt: t2,
	}))
	// Session s2 on day 2 in projA.
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag2, ProjectID: projA, SessionID: "s2", StartedAt: t3,
	}))
	// Session outside window in projA.
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag1, ProjectID: projA, SessionID: "s-old", StartedAt: tOut,
	}))
	// Session in projB inside window.
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag2, ProjectID: projB, SessionID: "s-other", StartedAt: t2,
	}))

	resA, err := cs.CountSessionMetrics(ctx, store.SessionMetricsCountQuery{
		Since:         t1.Add(-time.Hour),
		Until:         t3.Add(time.Hour),
		ProjectID:     projA,
		IncludeEvents: true,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), resA.TotalSessions, "s1 across two days must be counted once in TotalSessions")
	assert.False(t, resA.Truncated)
	require.Len(t, resA.Sessions, 3)

	// Limit=2 -> Truncated=true, keeps the 2 newest segments (t2, t3), TotalSessions stays exact.
	resATrunc, err := cs.CountSessionMetrics(ctx, store.SessionMetricsCountQuery{
		Since:         t1.Add(-time.Hour),
		Until:         t3.Add(time.Hour),
		ProjectID:     projA,
		IncludeEvents: true,
		Limit:         2,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), resATrunc.TotalSessions)
	assert.True(t, resATrunc.Truncated)
	require.Len(t, resATrunc.Sessions, 2)
	assert.Equal(t, t2, resATrunc.Sessions[0].StartedAt)
	assert.Equal(t, t3, resATrunc.Sessions[1].StartedAt)

	resGlobal, err := cs.CountSessionMetrics(ctx, store.SessionMetricsCountQuery{
		Since: t1.Add(-time.Hour),
		Until: t3.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(3), resGlobal.TotalSessions)
}

// TestUsageRead_StaleAndGlobalCoverageEquivalenceAndExplain verifies that the
// restructured CountStaleMetersByProject and global GetUsageScopeCoverage
// queries produce identical results to the previous full-scan SQL queries on
// the same fixture (on both SQLite and Postgres), and verifies Postgres
// EXPLAIN (ANALYZE, BUFFERS) plans under plan_cache_mode = force_generic_plan.
func TestUsageRead_StaleAndGlobalCoverageEquivalenceAndExplain(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	now := time.Now().UTC().Truncate(time.Microsecond)
	staleCutoff := now.Add(-3 * time.Minute)
	staleReceipt := staleCutoff.Add(-2 * time.Minute)
	freshReceipt := now.Add(-15 * time.Second)
	histStart := now.AddDate(0, 0, -10)
	histEnd := now.AddDate(0, 0, -8)

	projA := createTestProject(t, cs, "equiv-proj-a")
	projB := createTestProject(t, cs, "equiv-proj-b")
	userID := createTestUser(t, cs, "equiv-user@example.com")

	createAgentWithRun := func(projID, runID, phase string, deleted bool, raCreatedAt time.Time) string {
		agID := uuid.NewString()
		require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
			ID:        agID,
			Slug:      "ag-" + agID[:8],
			Name:      "ag-" + agID[:8],
			ProjectID: projID,
			Phase:     phase,
			RunID:     runID,
		}))
		upd := client.Agent.UpdateOneID(uuid.MustParse(agID)).SetRunID(runID)
		if deleted {
			upd = upd.SetDeletedAt(now.Add(-1 * time.Hour))
		}
		require.NoError(t, upd.Exec(ctx))
		if runID != "" && !raCreatedAt.IsZero() {
			_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
				RunID:         runID,
				Generation:    1,
				HubID:         "hub-equiv",
				ProjectID:     projID,
				AgentID:       agID,
				AgentSlug:     "ag-" + agID[:8],
				BillingUserID: &userID,
				Harness:       "claude",
				CreatedAt:     raCreatedAt,
			})
			require.NoError(t, err)
		}
		return agID
	}

	// 1. Running agent with stale open meter_health (projA) -> stale
	ag1 := createAgentWithRun(projA, "run-eq-1", "running", false, now.Add(-2*time.Hour))
	_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag1,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag1,
			RunID:         "run-eq-1",
			ProducerEpoch: "ep-1",
			LastReceiptAt: staleReceipt,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// 2. Running agent with fresh open meter_health + event (projA) -> measured
	ag2 := createAgentWithRun(projA, "run-eq-2", "running", false, now.Add(-1*time.Hour))
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag2,
		Events:  []*store.UsageEvent{makeStoreUsageEvent("hub-equiv", "eq-ev-2", "run-eq-2", projA, ag2, &userID, 1, freshReceipt, 100, 50, 0, 0)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag2,
			RunID:         "run-eq-2",
			ProducerEpoch: "ep-1",
			LastReceiptAt: freshReceipt,
			ProducerSeq:   1,
			ReceivedSeqs:  []int64{1},
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// 3. Running agent with NO meter_health and NO events, created before histStart (tests running branch!)
	_ = createAgentWithRun(projB, "run-eq-3", "running", false, now.AddDate(0, 0, -20))

	// 4. Stopped agent with historical rollup only (active in [histStart, histEnd])
	ag4 := createAgentWithRun(projB, "run-eq-4", "stopped", false, histStart.Add(-24*time.Hour))
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag4,
		Events:  []*store.UsageEvent{makeStoreUsageEvent("hub-equiv", "eq-ev-4", "run-eq-4", projB, ag4, &userID, 1, histStart.Add(6*time.Hour), 200, 100, 0, 0)},
	})
	require.NoError(t, err)

	// 5. Stopped agent with historical meter_health receipt only in [histStart, histEnd]
	ag5 := createAgentWithRun(projB, "run-eq-5", "stopped", false, histStart.Add(-48*time.Hour))
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag5,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag5,
			RunID:         "run-eq-5",
			ProducerEpoch: "ep-1",
			LastReceiptAt: histStart.Add(12 * time.Hour),
			FinalState:    store.MeterHealthFinalStateFinal,
		},
	})
	require.NoError(t, err)

	// 6. Soft-deleted agent with stale open meter_health -> excluded from both
	ag6 := createAgentWithRun(projA, "run-eq-6", "running", true, now.Add(-2*time.Hour))
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag6,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag6,
			RunID:         "run-eq-6",
			ProducerEpoch: "ep-1",
			LastReceiptAt: staleReceipt,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// 7. Add many historical stopped agents (outside window) so Postgres planner
	//    has realistic cardinality to choose the partial/covering indexes and PK
	//    lookups instead of tiny-table sequential scans.
	oldTS := now.AddDate(0, 0, -60)
	drv := client.Driver()
	dialectName := drv.Dialect()
	if dialectName == dialect.Postgres {
		var res sql.Result
		require.NoError(t, drv.Exec(ctx, `
INSERT INTO agents (
	id, slug, name, project_id, phase, run_id,
	created, updated, state_version
)
SELECT
	(md5('bg-agent-' || g::text))::uuid,
	'bg-slug-' || g::text,
	'bg-name-' || g::text,
	$1::uuid,
	'stopped',
	'run-bg-' || g::text,
	$2, $2, 1
FROM generate_series(1, 3000) AS g`, []any{projA, oldTS}, &res))
		require.NoError(t, drv.Exec(ctx, `
INSERT INTO run_attribution (
	run_id, launch_id, generation, prior_run_id, hub_id,
	project_id, agent_id, agent_slug, broker_id,
	harness, harness_config, template,
	metering_writer_mode, usage_live_source, capture_policy, notice_version,
	backfill_existing, legal_hold, created_at
)
SELECT
	'run-bg-' || g::text, '', 1, '', 'hub-equiv',
	$1, (md5('bg-agent-' || g::text))::uuid::text, 'bg-slug-' || g::text, '',
	'claude', '', '',
	'', '', '', '',
	false, false, $2
FROM generate_series(1, 3000) AS g`, []any{projA, oldTS}, &res))
		require.NoError(t, drv.Exec(ctx, "ANALYZE agents, meter_health, run_attribution, usage_rollups_daily", []any{}, &res))
	} else {
		for i := 0; i < 50; i++ {
			_ = createAgentWithRun(projA, fmt.Sprintf("run-bg-%d", i), "stopped", false, oldTS)
		}
	}

	// --- Equivalence Check 1: CountStaleMetersByProject (old vs new) ---
	gotStale, err := cs.CountStaleMetersByProject(ctx, staleCutoff)
	require.NoError(t, err)

	oldStaleQuery := usageRebindSQL(dialectName, `
SELECT project_id, COUNT(*) AS stale_count
FROM (
	SELECT ra.project_id, mh.agent_id, mh.run_id
	FROM meter_health mh
	INNER JOIN agents a ON CAST(a.id AS TEXT) = mh.agent_id AND a.run_id = mh.run_id AND a.deleted_at IS NULL AND a.phase = 'running'
	INNER JOIN run_attribution ra ON ra.run_id = mh.run_id
	WHERE mh.final_state = 'open'
	  AND NOT EXISTS (
		SELECT 1
		FROM meter_health mh2
		WHERE mh2.agent_id = mh.agent_id
		  AND mh2.run_id = mh.run_id
		  AND mh2.final_state <> 'open'
		  AND mh2.last_receipt_at >= mh.last_receipt_at
	  )
	GROUP BY ra.project_id, mh.agent_id, mh.run_id
	HAVING MAX(mh.last_receipt_at) < ?
) AS stale_runs
GROUP BY project_id
ORDER BY project_id ASC`)
	oldStaleRows := &entsql.Rows{}
	require.NoError(t, drv.Query(ctx, oldStaleQuery, []any{staleCutoff}, oldStaleRows))
	wantStale := make(map[string]int64)
	for oldStaleRows.Next() {
		var pid string
		var cnt int64
		require.NoError(t, oldStaleRows.Scan(&pid, &cnt))
		if pid != "" && cnt > 0 {
			wantStale[pid] = cnt
		}
	}
	require.NoError(t, oldStaleRows.Err())
	require.NoError(t, oldStaleRows.Close())
	assert.Equal(t, wantStale, gotStale, "new CountStaleMetersByProject must match old query identically")
	assert.Equal(t, map[string]int64{projA: 1}, gotStale)

	// --- Equivalence Check 2: Global GetUsageScopeCoverage (old vs new) ---
	runOldGlobalCoverage := func(since, until time.Time, staleAfter time.Duration) *store.UsageScopeCoverage {
		sinceDay := utcDayStart(since)
		untilDay := utcDayStart(until)
		sc := now.Add(-staleAfter).Truncate(time.Microsecond)
		incNow := !until.Before(sc)
		var qArgs []any
		qArgs = append(qArgs, until, since, until)
		overlap := `EXISTS (SELECT 1 FROM run_attribution ra2 WHERE ra2.agent_id = CAST(a.id AS TEXT) AND ra2.created_at >= ? AND ra2.created_at <= ?)`
		if incNow {
			overlap += ` OR a.phase = ?`
			qArgs = append(qArgs, "running")
		}
		overlap += ` OR EXISTS (SELECT 1 FROM meter_health mh_w WHERE mh_w.agent_id = CAST(a.id AS TEXT) AND mh_w.last_receipt_at >= ? AND mh_w.last_receipt_at <= ?)`
		qArgs = append(qArgs, since, until)
		overlap += ` OR EXISTS (SELECT 1 FROM usage_rollups_daily rd_w WHERE rd_w.agent_id = CAST(a.id AS TEXT) AND rd_w.bucket_start >= ? AND rd_w.bucket_start <= ?)`
		qArgs = append(qArgs, sinceDay, untilDay)
		isRunExpr := "0"
		isStaleExpr := "0"
		if incNow {
			isRunExpr = "CASE WHEN am.phase = ? THEN 1 ELSE 0 END"
			isStaleExpr = "CASE WHEN am.latest_receipt_at IS NOT NULL AND am.latest_receipt_at < ? THEN 1 ELSE 0 END"
			qArgs = append(qArgs, "running", sc)
		}
		qArgs = append(qArgs, sinceDay, untilDay)
		oldQ := usageRebindSQL(dialectName, fmt.Sprintf(`
WITH scoped_agents AS (
	SELECT CAST(a.id AS TEXT) AS agent_id_text, COALESCE(a.run_id, '') AS run_id, COALESCE(a.phase, '') AS phase
	FROM agents a
	WHERE a.deleted_at IS NULL
	  AND EXISTS (SELECT 1 FROM run_attribution ra WHERE ra.agent_id = CAST(a.id AS TEXT) AND ra.created_at <= ?)
	  AND (%s)
),
agent_mh AS (
	SELECT sa.agent_id_text, sa.run_id, sa.phase, MAX(mh.last_receipt_at) AS latest_receipt_at,
		COALESCE(MAX(CASE WHEN mh.last_receipt_at = (
			SELECT MAX(mh2.last_receipt_at) FROM meter_health mh2 WHERE mh2.agent_id = sa.agent_id_text AND mh2.run_id = sa.run_id
		) THEN mh.final_state ELSE '' END), '') AS latest_final_state
	FROM scoped_agents sa
	LEFT JOIN meter_health mh ON mh.agent_id = sa.agent_id_text AND mh.run_id = sa.run_id AND sa.run_id <> ''
	GROUP BY sa.agent_id_text, sa.run_id, sa.phase
),
agent_flags AS (
	SELECT %s AS is_running,
		CASE WHEN am.latest_receipt_at IS NOT NULL THEN 1 ELSE 0 END AS has_meter_health,
		am.latest_final_state AS latest_final_state,
		%s AS is_receipt_stale,
		CASE
			WHEN am.run_id <> '' AND EXISTS (SELECT 1 FROM usage_events ue WHERE ue.agent_id = am.agent_id_text AND ue.run_id = am.run_id) THEN 1
			WHEN EXISTS (SELECT 1 FROM usage_rollups_daily rd WHERE rd.agent_id = am.agent_id_text AND rd.bucket_start >= ? AND rd.bucket_start <= ?) THEN 1
			ELSE 0
		END AS has_usage_data,
		am.latest_receipt_at AS latest_receipt_at
	FROM agent_mh am
)
SELECT is_running, has_meter_health, latest_final_state, is_receipt_stale, has_usage_data,
	CAST(COUNT(*) AS BIGINT) AS agent_count, MAX(latest_receipt_at) AS max_last_receipt_at
FROM agent_flags
GROUP BY is_running, has_meter_health, latest_final_state, is_receipt_stale, has_usage_data`, overlap, isRunExpr, isStaleExpr))

		rows := &entsql.Rows{}
		require.NoError(t, drv.Query(ctx, oldQ, qArgs, rows))
		defer func() { _ = rows.Close() }()
		out := &store.UsageScopeCoverage{}
		for rows.Next() {
			var ir, hmh, irs, hud int
			var lfs string
			var cnt int64
			var rawLR any
			require.NoError(t, rows.Scan(&ir, &hmh, &lfs, &irs, &hud, &cnt, &rawLR))
			out.Buckets = append(out.Buckets, store.UsageCoverageBucket{
				IsRunning:        ir != 0,
				HasMeterHealth:   hmh != 0,
				LatestFinalState: lfs,
				IsReceiptStale:   irs != 0,
				HasUsageData:     hud != 0,
				AgentCount:       int(cnt),
			})
			if rawLR != nil {
				ts, err := parseSQLTimestamp(rawLR)
				require.NoError(t, err)
				if ts != nil && (out.LastReceiptAt == nil || ts.After(*out.LastReceiptAt)) {
					out.LastReceiptAt = ts
				}
			}
		}
		require.NoError(t, rows.Err())
		return out
	}

	// (a) Window including now: [now-24h, now]
	gotNow, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		Since:      now.Add(-24 * time.Hour),
		Until:      now,
		StaleAfter: 3 * time.Minute,
		DBNow:      now,
	})
	require.NoError(t, err)
	wantNow := runOldGlobalCoverage(now.Add(-24*time.Hour), now, 3*time.Minute)
	assert.ElementsMatch(t, wantNow.Buckets, gotNow.Buckets, "global coverage (includes now) must match old query")
	assert.Equal(t, wantNow.LastReceiptAt, gotNow.LastReceiptAt)

	// (b) Historical window: [histStart, histEnd]
	gotHist, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		Since:      histStart,
		Until:      histEnd,
		StaleAfter: 3 * time.Minute,
		DBNow:      now,
	})
	require.NoError(t, err)
	wantHist := runOldGlobalCoverage(histStart, histEnd, 3*time.Minute)
	assert.ElementsMatch(t, wantHist.Buckets, gotHist.Buckets, "global coverage (historical) must match old query")
	assert.Equal(t, wantHist.LastReceiptAt, gotHist.LastReceiptAt)

	// --- Postgres EXPLAIN (ANALYZE, BUFFERS) verification under force_generic_plan ---
	if dialectName == dialect.Postgres {
		explainPlan := func(label, q string, qArgs ...any) string {
			tx, err := client.Tx(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			txDrv := tx.Client().Driver()
			var res sql.Result
			require.NoError(t, txDrv.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan", []any{}, &res))
			rows := &entsql.Rows{}
			require.NoError(t, txDrv.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q, qArgs, rows))
			defer func() { _ = rows.Close() }()
			var lines []string
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				lines = append(lines, line)
			}
			require.NoError(t, rows.Err())
			plan := strings.Join(lines, "\n")
			t.Logf("=== EXPLAIN %s ===\n%s", label, plan)
			return plan
		}

		stalePlan := explainPlan("CountStaleMetersByProject", countStaleMetersByProjectSQL(dialectName), staleCutoff)
		assert.NotContains(t, stalePlan, "Seq Scan on agents", "CountStaleMetersByProject must use partial index on agents, not Seq Scan")
		assert.Contains(t, stalePlan, "agent_running_active", "CountStaleMetersByProject must use agent_running_active partial index")

		covQuery, covArgs, ok := buildUsageScopeCoverageSQL(dialectName, store.UsageCoverageFilter{
			Since:      now.Add(-24 * time.Hour),
			Until:      now,
			StaleAfter: 3 * time.Minute,
			DBNow:      now,
		}, now)
		require.True(t, ok)
		covPlan := explainPlan("GetUsageScopeCoverage (global, includes now)", covQuery, covArgs...)
		assert.NotContains(t, covPlan, "Seq Scan on agents", "global GetUsageScopeCoverage must not Seq Scan agents")
		assert.Contains(t, covPlan, "agent_running_active", "global GetUsageScopeCoverage must use agent_running_active")
	}
}

func TestGetUsageScopeCoverage_CapabilitiesGraceAndDrift(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	projGlobal := createTestProject(t, cs, "cov-global")
	projOverride := createTestProject(t, cs, "cov-override")
	user1 := createTestUser(t, cs, "cov-u1@example.com")

	seedAgent := func(projID, slug, runID string, phase state.Phase) *store.Agent {
		ag := &store.Agent{
			ID:        uuid.NewString(),
			Slug:      slug,
			Name:      slug,
			ProjectID: projID,
			OwnerID:   user1,
			Phase:     string(phase),
		}
		require.NoError(t, cs.CreateAgent(ctx, ag))
		_, err := cs.SetAgentRunID(ctx, ag.ID, runID, nil)
		require.NoError(t, err)
		ag.RunID = runID
		return ag
	}

	// Seed an archived project-scoped harness config for slug "hermes" on projOverride
	// (no status filter, matching dispatch; only matching slug splits capability_project_id).
	require.NoError(t, cs.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "hermes",
		Slug:    "hermes",
		Harness: "hermes",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: projOverride,
		Status:  store.HarnessConfigStatusArchived,
	}))

	// 1. Agent on projGlobal: running past 60s grace, no meter_health, no events.
	agPastGrace := seedAgent(projGlobal, "ag-past-grace", "run-past-grace", state.PhaseRunning)
	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         agPastGrace.RunID,
		ProjectID:     projGlobal,
		AgentID:       agPastGrace.ID,
		Harness:       "claude",
		HarnessConfig: "claude",
		CreatedAt:     now.Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	// 2. Agent on projGlobal: running within 60s grace, no meter_health, no events.
	agInGrace := seedAgent(projGlobal, "ag-in-grace", "run-in-grace", state.PhaseRunning)
	_, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         agInGrace.RunID,
		ProjectID:     projGlobal,
		AgentID:       agInGrace.ID,
		Harness:       "claude",
		HarnessConfig: "claude",
		CreatedAt:     now.Add(-30 * time.Second),
	})
	require.NoError(t, err)

	// 3. Agent on projGlobal: heartbeat-only fresh meter with schema_drift_detected=true.
	agDrift := seedAgent(projGlobal, "ag-drift", "run-drift", state.PhaseRunning)
	_, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         agDrift.RunID,
		ProjectID:     projGlobal,
		AgentID:       agDrift.ID,
		Harness:       "antigravity",
		HarnessConfig: "antigravity",
		CreatedAt:     now.Add(-5 * time.Minute),
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agDrift.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:             agDrift.ID,
			RunID:               agDrift.RunID,
			ProducerEpoch:       "ep-1",
			LastReceiptAt:       now.Add(-15 * time.Second),
			ProducerSeq:         1,
			SchemaDriftDetected: true,
			FinalState:          store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// 4. Agent on projOverride: heartbeat-only fresh meter, project-scoped override present.
	agOverride := seedAgent(projOverride, "ag-override", "run-override", state.PhaseRunning)
	_, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         agOverride.RunID,
		ProjectID:     projOverride,
		AgentID:       agOverride.ID,
		Harness:       "hermes",
		HarnessConfig: "hermes",
		CreatedAt:     now.Add(-5 * time.Minute),
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agOverride.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agOverride.ID,
			RunID:         agOverride.RunID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: now.Add(-10 * time.Second),
			ProducerSeq:   1,
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	cov, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		Since:        now.Add(-1 * time.Hour),
		Until:        now,
		StaleAfter:   3 * time.Minute,
		StartupGrace: 60 * time.Second,
		DBNow:        now,
	})
	require.NoError(t, err)

	var foundPastGrace, foundInGrace, foundDrift, foundOverride bool
	for _, b := range cov.Buckets {
		switch {
		case b.HarnessConfig == "claude" && b.IsPastGrace && !b.HasMeterHealth:
			assert.Equal(t, "", b.CapabilityProjectID, "projGlobal has no project-scoped harness_config and must collapse to empty string")
			assert.Equal(t, 1, b.AgentCount)
			foundPastGrace = true
		case b.HarnessConfig == "claude" && !b.IsPastGrace && !b.HasMeterHealth:
			assert.Equal(t, "", b.CapabilityProjectID)
			assert.Equal(t, 1, b.AgentCount)
			foundInGrace = true
		case b.HarnessConfig == "antigravity" && b.HasSchemaDrift:
			assert.Equal(t, "", b.CapabilityProjectID)
			assert.True(t, b.HasMeterHealth)
			assert.False(t, b.HasUsageData)
			assert.False(t, b.IsReceiptStale)
			assert.Equal(t, 1, b.AgentCount)
			foundDrift = true
		case b.HarnessConfig == "hermes" && b.CapabilityProjectID == projOverride:
			assert.True(t, b.HasMeterHealth)
			assert.False(t, b.HasUsageData)
			assert.Equal(t, 1, b.AgentCount)
			foundOverride = true
		}
	}
	assert.True(t, foundPastGrace, "expected bucket for claude past 60s grace")
	assert.True(t, foundInGrace, "expected bucket for claude within 60s grace")
	assert.True(t, foundDrift, "expected bucket for antigravity with schema drift")
	assert.True(t, foundOverride, "expected bucket for hermes with projOverride capability_project_id")

	// 5. Restarted agent with prior-run rollups inside the window and a new
	// current run with NO events/meter must report HasUsageData=false (R1) so
	// past-grace staleness is not suppressed by prior-run rollups.
	agRestarted := seedAgent(projGlobal, "ag-restarted", "run-restarted-1", state.PhaseRunning)
	_, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         "run-restarted-1",
		ProjectID:     projGlobal,
		AgentID:       agRestarted.ID,
		Harness:       "claude",
		HarnessConfig: "claude",
		CreatedAt:     now.Add(-2 * time.Hour),
	})
	require.NoError(t, err)
	evPrior := makeStoreUsageEvent("hub-cov", "ev-prior-1", "run-restarted-1", projGlobal, agRestarted.ID, &user1, 1, now.Add(-2*time.Hour), 100, 50, 0, 0)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agRestarted.ID,
		Events:  []*store.UsageEvent{evPrior},
	})
	require.NoError(t, err)
	// Restart onto run-restarted-2 (5m ago, past 60s grace, no events, no meter).
	_, err = cs.SetAgentRunID(ctx, agRestarted.ID, "run-restarted-2", nil)
	require.NoError(t, err)
	_, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         "run-restarted-2",
		ProjectID:     projGlobal,
		AgentID:       agRestarted.ID,
		Harness:       "claude",
		HarnessConfig: "claude",
		CreatedAt:     now.Add(-5 * time.Minute),
	})
	require.NoError(t, err)

	covRestarted, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:    projGlobal,
		AgentID:      agRestarted.ID,
		Since:        now.Add(-24 * time.Hour),
		Until:        now,
		StaleAfter:   3 * time.Minute,
		StartupGrace: 60 * time.Second,
		DBNow:        now,
	})
	require.NoError(t, err)
	require.Len(t, covRestarted.Buckets, 1)
	assert.True(t, covRestarted.Buckets[0].IsRunning)
	assert.True(t, covRestarted.Buckets[0].IsPastGrace)
	assert.False(t, covRestarted.Buckets[0].HasMeterHealth)
	assert.False(t, covRestarted.Buckets[0].HasUsageData, "prior-run rollups must not set HasUsageData when current run has no events")
}
