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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerolluphourly"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagescopecounterdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestLegacySessionMetrics_ConvertRow_NullsAndIdempotency(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "legacy-conv")
	userID := createTestUser(t, cs, "legacy-conv@example.com")
	agentID := uuid.NewString()
	runID := "run-legacy-" + uuid.NewString()[:8]

	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:            agentID,
		Slug:          "legacy-agent",
		Name:          "Legacy Agent",
		ProjectID:     projectID,
		OwnerID:       userID,
		CreatedBy:     userID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"},
		Template:      "default",
		Phase:         "running",
	}))
	_, err := cs.SetAgentRunID(ctx, agentID, runID, nil)
	require.NoError(t, err)

	t1 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	end1 := t1.Add(5 * time.Minute)
	row1 := &store.AgentSessionMetrics{
		ID:              uuid.NewString(),
		AgentID:         agentID,
		ProjectID:       projectID,
		SessionID:       "sess-1",
		StartedAt:       t1,
		EndedAt:         &end1,
		Status:          "completed",
		TurnCount:       4,
		Model:           "claude-sonnet-4-6",
		TokensInput:     1000,
		TokensOutput:    250,
		TokensCached:    0, // 0 in legacy table -> NULL, never 0
		TokensReasoning: 0,
	}
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, row1))

	converted, err := cs.ConvertLegacySessionMetricsRow(ctx, "hub-legacy", row1, runID)
	require.NoError(t, err)
	require.True(t, converted)

	// Source row must have converted_at stamped atomically.
	gotRow1, err := cs.GetAgentSessionMetrics(ctx, row1.ID)
	require.NoError(t, err)
	require.NotNil(t, gotRow1.ConvertedAt, "converted_at must be stamped on source row")

	// Ledger row must have NULL for cache_read, cache_write, cache_write_1h, reasoning.
	evID := store.LegacySessionMetricsEventID(agentID, "sess-1", t1)
	ev, err := client.UsageEvent.Query().Where(usageevent.EventIDEQ(evID)).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, store.UsageSourceLegacySessionMetrics, ev.UsageSource)
	assert.Equal(t, runID, ev.RunID)
	assert.Equal(t, int64(0), ev.Calls)
	require.NotNil(t, ev.Input)
	assert.Equal(t, int64(1000), *ev.Input)
	require.NotNil(t, ev.Output)
	assert.Equal(t, int64(250), *ev.Output)
	assert.Nil(t, ev.CacheRead, "0 cached in legacy table must be NULL")
	assert.Nil(t, ev.CacheWrite, "cache_write is unmeasured and must be NULL")
	assert.Nil(t, ev.CacheWrite1h, "cache_write_1h must be NULL")
	assert.Nil(t, ev.Reasoning, "0 reasoning in legacy table must be NULL")
	assert.Equal(t, telemetrycontract.TokenStatusPartial, ev.TokenStatus)
	require.NotNil(t, ev.BillingUserID)
	assert.Equal(t, userID, *ev.BillingUserID)
	assert.Equal(t, "claude", ev.Harness)

	// Re-converting the exact same row must be strictly idempotent.
	convertedAgain, err := cs.ConvertLegacySessionMetricsRow(ctx, "hub-legacy", row1, runID)
	require.NoError(t, err)
	assert.False(t, convertedAgain)

	// A second resumed segment with a new started_at must add its own increment.
	t2 := t1.Add(30 * time.Minute)
	end2 := t2.Add(5 * time.Minute)
	row2 := &store.AgentSessionMetrics{
		ID:              uuid.NewString(),
		AgentID:         agentID,
		ProjectID:       projectID,
		SessionID:       "sess-1",
		StartedAt:       t2,
		EndedAt:         &end2,
		Status:          "completed",
		TurnCount:       2,
		Model:           "claude-sonnet-4-6",
		TokensInput:     400,
		TokensOutput:    100,
		TokensCached:    300,
		TokensReasoning: 50,
	}
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, row2))
	converted2, err := cs.ConvertLegacySessionMetricsRow(ctx, "hub-legacy", row2, runID)
	require.NoError(t, err)
	require.True(t, converted2)

	// Verify daily rollup sums and presence counters.
	day := utcDayStart(t1)
	dr, err := client.UsageRollupDaily.Query().
		Where(
			usagerollupdaily.AgentIDEQ(agentID),
			usagerollupdaily.BucketStartEQ(day),
			usagerollupdaily.UsageSourceEQ(store.UsageSourceLegacySessionMetrics),
		).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1400), dr.Input)
	assert.Equal(t, int64(350), dr.Output)
	assert.Equal(t, int64(300), dr.CacheRead)
	assert.Equal(t, int64(0), dr.CacheWrite)
	assert.Equal(t, int64(0), dr.CacheWriteKnownEvents, "cache_write must remain unknown in rollups")
	assert.Equal(t, int64(1), dr.CacheReadKnownEvents)
	assert.Equal(t, int64(50), dr.Reasoning)
	assert.Equal(t, int64(1), dr.ReasoningKnownEvents)
	assert.Equal(t, int64(2), dr.Events)

	// Verify scope counters (project + user + user_project).
	sc, err := client.UsageScopeCounterDaily.Query().
		Where(
			usagescopecounterdaily.ScopeKindEQ(store.UsageScopeKindProject),
			usagescopecounterdaily.ScopeIDEQ(projectID),
			usagescopecounterdaily.DayUtcEQ(day),
		).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2050), sc.TokensSummable) // 1400 + 350 + 300 (reasoning excluded)
}

func TestLegacySessionMetrics_Backfill_TotalsCrashResumeAndRetention(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "legacy-bf")
	userID := createTestUser(t, cs, "legacy-bf@example.com")
	liveAgentID := uuid.NewString()
	deletedAgentID := uuid.NewString() // hard-deleted: no row in agents table

	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:            liveAgentID,
		Slug:          "bf-live-agent",
		Name:          "BF Live Agent",
		ProjectID:     projectID,
		OwnerID:       userID,
		CreatedBy:     userID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "codex", HarnessConfig: "codex"},
		Phase:         "stopped",
	}))

	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var rowIDs []string
	for i := 0; i < 5; i++ {
		aid := liveAgentID
		if i == 4 {
			aid = deletedAgentID
		}
		start := base.Add(time.Duration(i) * time.Hour)
		end := start.Add(10 * time.Minute)
		m := &store.AgentSessionMetrics{
			ID:           uuid.NewString(),
			AgentID:      aid,
			ProjectID:    projectID,
			SessionID:    "bf-sess-" + uuid.NewString()[:6],
			StartedAt:    start,
			EndedAt:      &end,
			Status:       "completed",
			Model:        "gpt-5",
			TokensInput:  100,
			TokensOutput: 50,
			TokensCached: 25,
		}
		require.NoError(t, cs.CreateAgentSessionMetrics(ctx, m))
		rowIDs = append(rowIDs, m.ID)
	}

	// Simulate a crash after batch 1 (pageSize=2 -> 2 rows committed, 3 still unconverted).
	crashErr := errors.New("simulated mid-run crash")
	res1, err := cs.backfillLegacySessionMetricsPaged(ctx, "hub-bf", 2, 0, func(batch int, _ store.LegacySessionMetricsBackfillResult) error {
		if batch == 1 {
			return crashErr
		}
		return nil
	}, nil)
	require.ErrorIs(t, err, crashErr)
	assert.Equal(t, 2, res1.Processed)
	assert.Equal(t, 2, res1.Converted)

	// Resume backfill to completion.
	res2, err := cs.backfillLegacySessionMetricsPaged(ctx, "hub-bf", 2, 0, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, res2.Processed)
	assert.Equal(t, 3, res2.Converted)

	// Verify all 5 rows are marked converted_at != nil.
	for _, id := range rowIDs {
		got, err := cs.GetAgentSessionMetrics(ctx, id)
		require.NoError(t, err)
		assert.NotNil(t, got.ConvertedAt, "row %s should be marked converted", id)
	}

	// Verify exact project rollup totals: 5 * (100 in, 50 out, 25 cache_read) = 500, 250, 125.
	day := utcDayStart(base)
	qRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		ProjectID:   projectID,
		Since:       day,
		Until:       day,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(500), qRes.Totals.Input)
	assert.Equal(t, int64(250), qRes.Totals.Output)
	assert.Equal(t, int64(125), qRes.Totals.CacheRead)
	assert.Equal(t, int64(0), qRes.Totals.CacheWrite)
	assert.Equal(t, int64(0), qRes.Totals.CacheWriteKnownEvents)
	assert.Equal(t, int64(5), qRes.Totals.Events)

	// Re-run backfill immediately: must be a no-op.
	res3, err := cs.BackfillLegacySessionMetrics(ctx, "hub-bf")
	require.NoError(t, err)
	assert.Equal(t, 0, res3.Processed)
	assert.Equal(t, 0, res3.Converted)

	// Retention sweeper deletes ALL usage_events rows; re-running backfill must
	// STILL be idempotent and never re-ingest or inflate rollups (D9/D9a).
	deletedCount, err := client.UsageEvent.Delete().Exec(ctx)
	require.NoError(t, err)
	require.Equal(t, 5, deletedCount)

	res4, err := cs.BackfillLegacySessionMetrics(ctx, "hub-bf")
	require.NoError(t, err)
	assert.Equal(t, 0, res4.Processed)
	assert.Equal(t, 0, res4.Converted)

	evCount, err := client.UsageEvent.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, evCount, "deleted ledger rows must not be re-ingested after retention")

	qResAfter, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		ProjectID:   projectID,
		Since:       day,
		Until:       day,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(500), qResAfter.Totals.Input)
	assert.Equal(t, int64(250), qResAfter.Totals.Output)

	// A new row posted while the experiment was OFF (converted_at = NULL) is
	// converted cleanly on the next catch-up run without touching old rows.
	lateStart := base.Add(6 * time.Hour)
	lateEnd := lateStart.Add(5 * time.Minute)
	lateRow := &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      liveAgentID,
		ProjectID:    projectID,
		SessionID:    "late-sess-off",
		StartedAt:    lateStart,
		EndedAt:      &lateEnd,
		Status:       "completed",
		Model:        "gpt-5",
		TokensInput:  70,
		TokensOutput: 30,
	}
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, lateRow))

	res5, err := cs.BackfillLegacySessionMetrics(ctx, "hub-bf")
	require.NoError(t, err)
	assert.Equal(t, 1, res5.Processed)
	assert.Equal(t, 1, res5.Converted)

	qResLate, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		ProjectID:   projectID,
		Since:       day,
		Until:       day,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(570), qResLate.Totals.Input)
	assert.Equal(t, int64(280), qResLate.Totals.Output)
}

func TestLegacySessionMetrics_Backfill_NoDoubleCountWithTrack1(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "legacy-overlap")
	agentA := uuid.NewString()
	agentB := uuid.NewString()
	agentC := uuid.NewString()

	runA := "run-a-" + uuid.NewString()[:8]
	runB := "run-b-" + uuid.NewString()[:8]
	runC := "run-c-" + uuid.NewString()[:8]
	seedRunForIngestTest(t, cs, projectID, agentA, runA, nil)
	seedRunForIngestTest(t, cs, projectID, agentB, runB, nil)
	seedRunForIngestTest(t, cs, projectID, agentC, runC, nil)

	t0 := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)

	// Agent A: Track 1 event matching session_id_hash (different timestamp).
	sumA := sha256.Sum256([]byte("sess-overlap-a"))
	hashA := hex.EncodeToString(sumA[:])
	evA := makeStoreUsageEvent("hub-ov", "t1-a", runA, projectID, agentA, nil, 1, t0.Add(2*time.Hour), 500, 100, 0, 0)
	evA.SessionIDHash = &hashA

	// Agent B: Track 1 event without session_id_hash, but inside [t0, t0+10m].
	evB := makeStoreUsageEvent("hub-ov", "t1-b", runB, projectID, agentB, nil, 1, t0.Add(5*time.Minute), 600, 200, 0, 0)

	// Agent C: Track 1 event on a different day (no overlap with legacy session).
	evC := makeStoreUsageEvent("hub-ov", "t1-c", runC, projectID, agentC, nil, 1, t0.Add(48*time.Hour), 700, 300, 0, 0)

	_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		Events: []*store.UsageEvent{evA, evB, evC},
	})
	require.NoError(t, err)

	end0 := t0.Add(10 * time.Minute)
	for _, tc := range []struct {
		agentID   string
		sessionID string
	}{
		{agentA, "sess-overlap-a"},
		{agentB, "sess-overlap-b"},
		{agentC, "sess-no-overlap-c"},
	} {
		require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
			ID:           uuid.NewString(),
			AgentID:      tc.agentID,
			ProjectID:    projectID,
			SessionID:    tc.sessionID,
			StartedAt:    t0,
			EndedAt:      &end0,
			Status:       "completed",
			Model:        "claude-sonnet-4-6",
			TokensInput:  999,
			TokensOutput: 111,
		}))
	}

	res, err := cs.BackfillLegacySessionMetrics(ctx, "hub-ov")
	require.NoError(t, err)
	assert.Equal(t, 3, res.Processed)
	assert.Equal(t, 1, res.Converted, "only agent C should be converted")
	assert.Equal(t, 2, res.Skipped, "agents A and B overlap Track 1 and must be skipped")

	// Confirm only agent C got a legacy_session_metrics event.
	legacyRows, err := client.UsageEvent.Query().
		Where(usageevent.UsageSourceEQ(store.UsageSourceLegacySessionMetrics)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, legacyRows, 1)
	assert.Equal(t, agentC, legacyRows[0].AgentID)
}

func TestLegacySessionMetrics_Backfill_HourlyCoverageRule(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "legacy-hourly")
	agentID := uuid.NewString()

	// 1. Before hourly coverage exists, backfill writes daily rollups only and
	//    does NOT create the hourly coverage marker.
	preTS := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	preEnd := preTS.Add(5 * time.Minute)
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "pre-cov-1",
		StartedAt:    preTS,
		EndedAt:      &preEnd,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  100,
		TokensOutput: 20,
	}))

	res1, err := cs.BackfillLegacySessionMetrics(ctx, "hub-hr")
	require.NoError(t, err)
	assert.Equal(t, 1, res1.Converted)

	_, hasCov, err := cs.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	assert.False(t, hasCov, "backfill must not create the hourly coverage marker")

	hrCount, err := client.UsageRollupHourly.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, hrCount, "backfill must not write hourly rollups before coverage starts")

	// 2. Establish hourly coverage via a normal live Track 1 ingest (P2.1a sets
	//    coverage start to ceil_hour(DBNow)).
	liveTS := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	liveAgent := uuid.NewString()
	liveRun := "run-live-" + uuid.NewString()[:8]
	seedRunForIngestTest(t, cs, projectID, liveAgent, liveRun, nil)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: liveAgent,
		Events: []*store.UsageEvent{
			makeStoreUsageEvent("hub-hr", "live-cov-1", liveRun, projectID, liveAgent, nil, 1, liveTS, 10, 5, 0, 0),
		},
	})
	require.NoError(t, err)

	covTS, hasCov, err := cs.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	require.True(t, hasCov)

	// 3. Backfill two more rows: one before covTS, one after covTS.
	//    The pre-covTS row writes daily only; the post-covTS row writes both
	//    daily and hourly; the coverage marker stays pinned at covTS.
	beforeTS := covTS.Add(-2 * time.Hour)
	beforeEnd := beforeTS.Add(5 * time.Minute)
	afterTS := covTS.Add(2 * time.Hour)
	afterEnd := afterTS.Add(5 * time.Minute)

	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "before-cov-2",
		StartedAt:    beforeTS,
		EndedAt:      &beforeEnd,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  200,
		TokensOutput: 40,
	}))
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "after-cov-3",
		StartedAt:    afterTS,
		EndedAt:      &afterEnd,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  300,
		TokensOutput: 60,
	}))

	res2, err := cs.BackfillLegacySessionMetrics(ctx, "hub-hr")
	require.NoError(t, err)
	assert.Equal(t, 2, res2.Converted)

	// Coverage marker must NOT have moved backward.
	gotCovAfter, hasCovAfter, err := cs.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	require.True(t, hasCovAfter)
	assert.Equal(t, covTS, gotCovAfter)

	// Hourly rollups for agentID must contain ONLY the afterTS row.
	hourlyRows, err := client.UsageRollupHourly.Query().
		Where(usagerolluphourly.AgentIDEQ(agentID)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, hourlyRows, 1)
	assert.Equal(t, utcHourStart(afterEnd), hourlyRows[0].BucketStart.UTC())
	assert.Equal(t, int64(300), hourlyRows[0].Input)
	assert.Equal(t, int64(60), hourlyRows[0].Output)
}

func TestGetRunUsageSourceSummary(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "run-src-sum")
	agentID := uuid.NewString()
	runLegacy := "run-leg-" + uuid.NewString()[:8]
	runMixed := "run-mix-" + uuid.NewString()[:8]
	seedRunForIngestTest(t, cs, projectID, agentID, runLegacy, nil)
	seedRunForIngestTest(t, cs, projectID, agentID, runMixed, nil)

	// Empty run.
	empty, err := cs.GetRunUsageSourceSummary(ctx, agentID, runLegacy)
	require.NoError(t, err)
	assert.False(t, empty.HasTrack1Events)
	assert.False(t, empty.HasLegacyEvents)

	ts := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	legEv := makeStoreUsageEvent("hub-s", "ev-leg-1", runLegacy, projectID, agentID, nil, 0, ts, 10, 5, 0, 0)
	legEv.UsageSource = store.UsageSourceLegacySessionMetrics
	mixLeg := makeStoreUsageEvent("hub-s", "ev-mix-1", runMixed, projectID, agentID, nil, 0, ts, 10, 5, 0, 0)
	mixLeg.UsageSource = store.UsageSourceLegacySessionMetrics
	mixT1 := makeStoreUsageEvent("hub-s", "ev-mix-2", runMixed, projectID, agentID, nil, 1, ts.Add(time.Minute), 20, 10, 0, 0)

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{legEv, mixLeg, mixT1},
	})
	require.NoError(t, err)

	sumLeg, err := cs.GetRunUsageSourceSummary(ctx, agentID, runLegacy)
	require.NoError(t, err)
	assert.False(t, sumLeg.HasTrack1Events)
	assert.True(t, sumLeg.HasLegacyEvents)

	sumMix, err := cs.GetRunUsageSourceSummary(ctx, agentID, runMixed)
	require.NoError(t, err)
	assert.True(t, sumMix.HasTrack1Events)
	assert.True(t, sumMix.HasLegacyEvents)
}

func TestLegacySessionMetrics_ConvertRow_RevokedSinkLaterSessionConverted(t *testing.T) {
	// R1: A run with earlier Track 1 events (e.g. before sink revocation) must
	// still convert a later unmarked legacy segment with a new session_id and
	// non-overlapping window on the same run_id.
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "legacy-r1")
	agentID := uuid.NewString()
	runID := "run-r1-" + uuid.NewString()[:8]
	seedRunForIngestTest(t, cs, projectID, agentID, runID, nil)

	t10 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			makeStoreUsageEvent("hub-r1", "t1-10am", runID, projectID, agentID, nil, 1, t10, 500, 100, 0, 0),
		},
	})
	require.NoError(t, err)

	// Overlapping segment [09:55, 10:05] must be skipped (not double counted).
	ovEnd := t10.Add(5 * time.Minute)
	ovRow := &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "sess-r1-overlap",
		StartedAt:    t10.Add(-5 * time.Minute),
		EndedAt:      &ovEnd,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  999,
		TokensOutput: 999,
	}
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, ovRow))
	ovConv, err := cs.ConvertLegacySessionMetricsRow(ctx, "hub-r1", ovRow, runID)
	require.NoError(t, err)
	assert.False(t, ovConv, "overlapping segment must not double count")

	// Later non-overlapping segment [13:00, 13:10] with a new session_id on the
	// SAME run_id MUST be converted and included in totals.
	t13 := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	end13 := t13.Add(10 * time.Minute)
	laterRow := &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "sess-r1-after-revoke",
		StartedAt:    t13,
		EndedAt:      &end13,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  300,
		TokensOutput: 75,
	}
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, laterRow))
	laterConv, err := cs.ConvertLegacySessionMetricsRow(ctx, "hub-r1", laterRow, runID)
	require.NoError(t, err)
	assert.True(t, laterConv, "non-overlapping post-revocation segment on same run must be converted")

	day := utcDayStart(t10)
	qRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityDay,
		AgentID:     agentID,
		Since:       day,
		Until:       day,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(800), qRes.Totals.Input)
	assert.Equal(t, int64(175), qRes.Totals.Output)
}

func TestLegacySessionMetrics_Backfill_PartialBatchRejectionNoLivelock(t *testing.T) {
	// O1: A page-size batch containing one rejected row stamps the non-rejected
	// row and terminates cleanly without livelocking when no further stamping
	// progress can be made.
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "legacy-o1")
	agentID := uuid.NewString()
	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	end0 := t0.Add(5 * time.Minute)

	badRow := &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "o1-bad",
		StartedAt:    t0,
		EndedAt:      &end0,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  100,
		TokensOutput: 20,
	}
	goodRow := &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "o1-good",
		StartedAt:    t0.Add(time.Hour),
		EndedAt:      &end0,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  200,
		TokensOutput: 40,
	}
	goodEnd := t0.Add(time.Hour + 5*time.Minute)
	goodRow.EndedAt = &goodEnd
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, badRow))
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, goodRow))

	badEventID := store.LegacySessionMetricsEventID(agentID, "o1-bad", t0)
	// Inject a per-row FK constraint rejection on badRow's event.
	fault := func(in *store.UsageIngestBatchInput) {
		for _, ev := range in.Events {
			if ev.EventID == badEventID {
				ev.RunID = "nonexistent-fk-run-id"
			}
		}
	}

	// Page size = 2 (full batch with 1 rejected, 1 valid). First batch stamps
	// goodRow (progress=1), second batch fetches only badRow (progress=0) and
	// breaks with an error instead of livelocking.
	res, err := cs.backfillLegacySessionMetricsPaged(ctx, "hub-o1", 2, 0, nil, fault)
	require.Error(t, err, "expected clean break error when only rejected row remains")
	assert.Equal(t, 1, res.Converted)

	gotGood, err := cs.GetAgentSessionMetrics(ctx, goodRow.ID)
	require.NoError(t, err)
	assert.NotNil(t, gotGood.ConvertedAt, "non-rejected row must be stamped converted_at")

	gotBad, err := cs.GetAgentSessionMetrics(ctx, badRow.ID)
	require.NoError(t, err)
	assert.Nil(t, gotBad.ConvertedAt, "rejected row must remain unconverted for later retry")
}

func TestLegacySessionMetrics_Backfill_CurrentRunAttribution(t *testing.T) {
	// O4: Catch-up attributes to the agent's real current run when
	// row.StartedAt >= run_attribution.created_at, and uses synthetic
	// legacy-backfill:<agent_id> for older rows.
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "legacy-o4")
	agentID := uuid.NewString()
	currentRunID := "run-o4-curr-" + uuid.NewString()[:8]

	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:            agentID,
		Slug:          "o4-agent",
		Name:          "O4 Agent",
		ProjectID:     projectID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"},
		Phase:         "running",
	}))
	_, err := cs.SetAgentRunID(ctx, agentID, currentRunID, nil)
	require.NoError(t, err)

	runCreated := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	created, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:      currentRunID,
		Generation: 1,
		HubID:      "hub-o4",
		ProjectID:  projectID,
		AgentID:    agentID,
		Harness:    "claude",
		CreatedAt:  runCreated,
	})
	require.NoError(t, err)
	require.True(t, created)

	oldStart := runCreated.Add(-2 * time.Hour)
	oldEnd := oldStart.Add(5 * time.Minute)
	currStart := runCreated.Add(10 * time.Minute)
	currEnd := currStart.Add(5 * time.Minute)

	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "o4-old",
		StartedAt:    oldStart,
		EndedAt:      &oldEnd,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  100,
		TokensOutput: 20,
	}))
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		ID:           uuid.NewString(),
		AgentID:      agentID,
		ProjectID:    projectID,
		SessionID:    "o4-curr",
		StartedAt:    currStart,
		EndedAt:      &currEnd,
		Status:       "completed",
		Model:        "claude-sonnet-4-6",
		TokensInput:  250,
		TokensOutput: 50,
	}))

	res, err := cs.BackfillLegacySessionMetrics(ctx, "hub-o4")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Converted)

	oldEv, err := client.UsageEvent.Query().
		Where(usageevent.EventIDEQ(store.LegacySessionMetricsEventID(agentID, "o4-old", oldStart))).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "legacy-backfill:"+agentID, oldEv.RunID, "pre-run row must use synthetic backfill run_id")

	currEv, err := client.UsageEvent.Query().
		Where(usageevent.EventIDEQ(store.LegacySessionMetricsEventID(agentID, "o4-curr", currStart))).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, currentRunID, currEv.RunID, "row started at/after current run must attribute to current run_id")
}
