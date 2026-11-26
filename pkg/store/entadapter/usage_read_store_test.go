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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	// Hourly coverage marker was recorded at earliest UTC hour bucket (day1 12:00Z).
	covStart, hasCov, err := cs.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	require.True(t, hasCov)
	assert.Equal(t, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), covStart)

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

	// ListUsageRollupsHourlyByAgent mirrors daily with UTC hour buckets.
	hourly, err := cs.ListUsageRollupsHourlyByAgent(ctx, agentID, day1, day2)
	require.NoError(t, err)
	require.Len(t, hourly, 2)
	assert.Equal(t, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), hourly[0].BucketStart)
	assert.Equal(t, int64(8), hourly[0].Input)
	assert.Equal(t, int64(1), hourly[0].PresenceTrackedEvents)
	assert.Equal(t, time.Date(2026, 11, 21, 15, 0, 0, 0, time.UTC), hourly[1].BucketStart)
	assert.Equal(t, int64(1), hourly[1].CacheReadKnownEvents)
	assert.Equal(t, int64(0), hourly[1].CacheWriteKnownEvents)

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
	}
	require.NoError(t, cs.CreateAgent(ctx, agStale))
	_, err = cs.SetAgentRunID(ctx, agStale.ID, "run-s-1", nil)
	require.NoError(t, err)
	agStale.RunID = "run-s-1"

	agUnmeasured := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ag-unmeasured",
		Name:      "ag-unmeasured",
		ProjectID: projB,
		OwnerID:   user2,
	}
	require.NoError(t, cs.CreateAgent(ctx, agUnmeasured))
	_, err = cs.SetAgentRunID(ctx, agUnmeasured.ID, "run-u-1", nil)
	require.NoError(t, err)
	agUnmeasured.RunID = "run-u-1"

	seedRunForIngestTest(t, cs, projA, agMeasured.ID, agMeasured.RunID, &user1)
	seedRunForIngestTest(t, cs, projA, agStale.ID, agStale.RunID, &user1)
	seedRunForIngestTest(t, cs, projB, agUnmeasured.ID, agUnmeasured.RunID, &user2)

	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	ev1 := makeStoreUsageEvent("hub-q", "q-1", agMeasured.RunID, projA, agMeasured.ID, &user1, 1, base.Add(5*time.Minute), 100, 50, 10, 0)
	ev1.Model = "claude-sonnet-4-6"
	ev1.Reasoning = i64p(20)

	ev2 := makeStoreUsageEvent("hub-q", "q-2", agMeasured.RunID, projA, agMeasured.ID, &user1, 2, base.Add(65*time.Minute), 200, 80, 0, 5)
	ev2.Model = "claude-haiku-4-5"
	ev2.Reasoning = nil // mixed known+unknown reasoning

	ev3 := makeStoreUsageEvent("hub-q", "q-3", agStale.RunID, projA, agStale.ID, &user1, 1, base.Add(10*time.Minute), 40, 10, 0, 0)
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

	// Hourly query grouped by model with limit=2 -> truncated=true, exact totals preserved.
	res, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       base,
		Until:       base.Add(2 * time.Hour),
		ProjectID:   projA,
		GroupBy:     []string{store.UsageGroupByModel},
		Limit:       2,
	})
	require.NoError(t, err)
	assert.True(t, res.Truncated)
	assert.Len(t, res.Groups, 2)
	assert.Equal(t, int64(340), res.Totals.Input)
	assert.Equal(t, int64(140), res.Totals.Output)
	assert.Equal(t, int64(3), res.Totals.Events)
	assert.Equal(t, int64(3), res.Totals.PresenceTrackedEvents)
	assert.Equal(t, int64(1), res.Totals.ReasoningKnownEvents)
	assert.Equal(t, int64(2), res.Totals.CacheReadKnownEvents)

	// Hourly query grouped by hour -> ordered ASC by bucket_start.
	hourRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       base,
		Until:       base.Add(2 * time.Hour),
		ProjectID:   projA,
		GroupBy:     []string{store.UsageGroupByHour},
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, hourRes.Groups, 2)
	require.NotNil(t, hourRes.Groups[0].BucketStart)
	require.NotNil(t, hourRes.Groups[1].BucketStart)
	assert.Equal(t, base, *hourRes.Groups[0].BucketStart)
	assert.Equal(t, base.Add(time.Hour), *hourRes.Groups[1].BucketStart)

	// Hourly table grouped by day collapses both hours into 1 UTC day bucket.
	dayFromHourly, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       base,
		Until:       base.Add(2 * time.Hour),
		ProjectID:   projA,
		GroupBy:     []string{store.UsageGroupByDay},
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, dayFromHourly.Groups, 1)
	require.NotNil(t, dayFromHourly.Groups[0].BucketStart)
	assert.Equal(t, time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), *dayFromHourly.Groups[0].BucketStart)
	assert.Equal(t, int64(340), dayFromHourly.Groups[0].Input)

	// Scope coverage: projA has 1 measured + 1 stale; projB has 1 unmeasured; global has 3 total.
	dbNow := base.Add(2*time.Hour + 1*time.Minute)
	covA, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:  projA,
		StaleAfter: 3 * time.Minute,
		DBNow:      dbNow,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, covA.TotalAgents)
	assert.Equal(t, 1, covA.MeasuredAgents)
	assert.Equal(t, 1, covA.StaleAgents)
	assert.Equal(t, 0, covA.UnmeasuredAgents)
	require.NotNil(t, covA.LastReceiptAt)
	assert.Equal(t, base.Add(2*time.Hour), *covA.LastReceiptAt)

	covGlobal, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		StaleAfter: 3 * time.Minute,
		DBNow:      dbNow,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, covGlobal.TotalAgents)
	assert.Equal(t, 1, covGlobal.MeasuredAgents)
	assert.Equal(t, 1, covGlobal.StaleAgents)
	assert.Equal(t, 1, covGlobal.UnmeasuredAgents)

	covUser2, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		BillingUserID: user2,
		StaleAfter:    3 * time.Minute,
		DBNow:         dbNow,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, covUser2.TotalAgents)
	assert.Equal(t, 0, covUser2.MeasuredAgents)
	assert.Equal(t, 0, covUser2.StaleAgents)
	assert.Equal(t, 1, covUser2.UnmeasuredAgents)
}
