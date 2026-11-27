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
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/meterhealth"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runattribution"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagescopecounterdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestUsageRetention_UsageEvents_CutoffAndExportCursor(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ret-events")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, nil)

	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cursorTS := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	seedEvent := func(eventID string, eventTS, receivedAt time.Time) {
		t.Helper()
		_, err := client.UsageEvent.Create().
			SetHubID("hub-1").
			SetEventID(eventID).
			SetRunID(runID).
			SetProjectID(projectID).
			SetAgentID(agentID).
			SetModel("claude-sonnet-4-6").
			SetModelSource("reported").
			SetUsageSource("native").
			SetStatus("ok").
			SetCalls(1).
			SetTokenStatus("complete").
			SetEventTs(eventTS).
			SetReceivedAt(receivedAt).
			Save(ctx)
		require.NoError(t, err)
	}

	// 1. Expired & before cursor -> eligible under cursor.
	seedEvent("evt-a", cutoff.Add(-24*time.Hour), cursorTS.Add(-time.Hour))
	// 2. Expired & exact cursorTS with event_id <= "evt-b" -> eligible under tie-breaker.
	seedEvent("evt-b", cutoff.Add(-12*time.Hour), cursorTS)
	// 3. Expired & exact cursorTS with event_id > "evt-b" -> protected by tie-breaker.
	seedEvent("evt-c", cutoff.Add(-6*time.Hour), cursorTS)
	// 4. Expired & received_at after cursor -> protected by cursor.
	seedEvent("evt-d", cutoff.Add(-time.Hour), cursorTS.Add(time.Hour))
	// 5. Exactly at cutoff -> NEVER deleted.
	seedEvent("evt-at-cutoff", cutoff, cursorTS.Add(-2*time.Hour))
	// 6. After cutoff -> NEVER deleted.
	seedEvent("evt-newer", cutoff.Add(time.Hour), cursorTS.Add(-2*time.Hour))

	// First sweep with export cursor + tie-breaker "evt-b": deletes evt-a and evt-b only.
	n, err := cs.SweepUsageEventsBatch(ctx, cutoff, &store.UsageExportCursor{
		ReceivedAt: cursorTS,
		EventID:    "evt-b",
	}, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	remaining, err := client.UsageEvent.Query().Select(usageevent.FieldEventID).Strings(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"evt-c", "evt-d", "evt-at-cutoff", "evt-newer"}, remaining)

	// Second sweep with ReceivedAt-only cursor (empty EventID): deletes evt-c (received_at == cursorTS), keeps evt-d.
	n, err = cs.SweepUsageEventsBatch(ctx, cutoff, &store.UsageExportCursor{
		ReceivedAt: cursorTS,
	}, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	remaining, err = client.UsageEvent.Query().Select(usageevent.FieldEventID).Strings(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"evt-d", "evt-at-cutoff", "evt-newer"}, remaining)

	// Third sweep with nil cursor (age-only): deletes evt-d, keeps exact cutoff & newer.
	n, err = cs.SweepUsageEventsBatch(ctx, cutoff, nil, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	remaining, err = client.UsageEvent.Query().Select(usageevent.FieldEventID).Strings(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"evt-at-cutoff", "evt-newer"}, remaining)
}

func TestUsageRetention_RollupsHourlyDailyAndScopeCounters_Boundaries(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ret-rollups")
	agentID := uuid.NewString()

	hourCutoff := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	for _, ts := range []time.Time{
		hourCutoff.Add(-2 * time.Hour),
		hourCutoff.Add(-time.Hour),
		hourCutoff, // boundary: must be kept
		hourCutoff.Add(time.Hour),
	} {
		_, err := client.UsageRollupHourly.Create().
			SetBucketStart(ts).
			SetProjectID(projectID).
			SetAgentID(agentID).
			SetUsageSource("native").
			SetFirstEventTs(ts).
			SetLastEventTs(ts).
			Save(ctx)
		require.NoError(t, err)
	}

	n, err := cs.SweepUsageRollupsHourlyBatch(ctx, hourCutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	hourlyRows, err := client.UsageRollupHourly.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, hourlyRows, 2)
	for _, r := range hourlyRows {
		assert.False(t, r.BucketStart.UTC().Before(hourCutoff))
	}

	dayCutoff := time.Date(2025, 11, 20, 0, 0, 0, 0, time.UTC)
	for _, d := range []time.Time{
		dayCutoff.AddDate(0, 0, -2),
		dayCutoff.AddDate(0, 0, -1),
		dayCutoff, // boundary: must be kept
		dayCutoff.AddDate(0, 0, 1),
	} {
		_, err := client.UsageRollupDaily.Create().
			SetBucketStart(d).
			SetProjectID(projectID).
			SetAgentID(agentID).
			SetUsageSource("native").
			SetFirstEventTs(d).
			SetLastEventTs(d).
			Save(ctx)
		require.NoError(t, err)

		_, err = client.UsageScopeCounterDaily.Create().
			SetScopeKind(store.UsageScopeKindProject).
			SetScopeID(projectID).
			SetDayUtc(d).
			SetTokensSummable(100).
			Save(ctx)
		require.NoError(t, err)
	}

	n, err = cs.SweepUsageRollupsDailyBatch(ctx, dayCutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	dailyCount, err := client.UsageRollupDaily.Query().Where(usagerollupdaily.BucketStartGTE(dayCutoff)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, dailyCount)
	totalDaily, err := client.UsageRollupDaily.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, totalDaily)

	n, err = cs.SweepUsageScopeCountersDailyBatch(ctx, dayCutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	scopeCount, err := client.UsageScopeCounterDaily.Query().Where(usagescopecounterdaily.DayUtcGTE(dayCutoff)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, scopeCount)
	totalScope, err := client.UsageScopeCounterDaily.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, totalScope)
}

func TestUsageRetention_MeterHealth_D8Rule(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ret-mh")
	projUUID := uuid.MustParse(projectID)
	cutoff := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

	// Live agent with current run "run-live-cur".
	liveAgentID := uuid.New()
	_, err := client.Agent.Create().
		SetID(liveAgentID).
		SetSlug("live-agent").
		SetName("live-agent").
		SetProjectID(projUUID).
		SetRunID("run-live-cur").
		SetStatus(agent.StatusRunning).
		Save(ctx)
	require.NoError(t, err)

	// Soft-deleted agent with run "run-softdel".
	softDelAgentID := uuid.New()
	deletedAt := cutoff.Add(-5 * 24 * time.Hour)
	_, err = client.Agent.Create().
		SetID(softDelAgentID).
		SetSlug("softdel-agent").
		SetName("softdel-agent").
		SetProjectID(projUUID).
		SetRunID("run-softdel").
		SetStatus(agent.StatusStopped).
		SetDeletedAt(deletedAt).
		Save(ctx)
	require.NoError(t, err)

	hardDelAgentID := uuid.NewString()

	seedMH := func(agentID, runID, epoch, finalState string, updatedAt time.Time) uuid.UUID {
		t.Helper()
		row, err := client.MeterHealth.Create().
			SetAgentID(agentID).
			SetRunID(runID).
			SetProducerEpoch(epoch).
			SetLastReceiptAt(updatedAt).
			SetFinalState(finalState).
			SetUpdatedAt(updatedAt).
			Save(ctx)
		require.NoError(t, err)
		return row.ID
	}

	// Protected: live agent current run, open, older than cutoff.
	keepLiveOpen := seedMH(liveAgentID.String(), "run-live-cur", "ep-1", store.MeterHealthFinalStateOpen, cutoff.Add(-48*time.Hour))
	// Protected: live agent current run, final, older than cutoff (never touch current run of live agent).
	keepLiveFinal := seedMH(liveAgentID.String(), "run-live-cur", "ep-2", store.MeterHealthFinalStateFinal, cutoff.Add(-48*time.Hour))
	// Protected: superseded run, exactly at cutoff.
	keepAtCutoff := seedMH(liveAgentID.String(), "run-live-old", "ep-at", store.MeterHealthFinalStateOpen, cutoff)

	// Sweepable: superseded run of live agent, still 'open', older than cutoff (D8 abandoned rule).
	sweepSupersededOpen := seedMH(liveAgentID.String(), "run-live-old", "ep-old", store.MeterHealthFinalStateOpen, cutoff.Add(-time.Hour))
	// Sweepable: soft-deleted agent, still 'open', older than cutoff.
	sweepSoftDelOpen := seedMH(softDelAgentID.String(), "run-softdel", "ep-sd", store.MeterHealthFinalStateOpen, cutoff.Add(-time.Hour))
	// Sweepable: hard-deleted agent (no agents row), 'final', older than cutoff.
	sweepHardDelFinal := seedMH(hardDelAgentID, "run-harddel", "ep-hd", store.MeterHealthFinalStateFinal, cutoff.Add(-time.Hour))

	n, err := cs.SweepMeterHealthBatch(ctx, cutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	remaining, err := client.MeterHealth.Query().All(ctx)
	require.NoError(t, err)
	var remIDs []uuid.UUID
	for _, r := range remaining {
		remIDs = append(remIDs, r.ID)
	}
	assert.ElementsMatch(t, []uuid.UUID{keepLiveOpen, keepLiveFinal, keepAtCutoff}, remIDs)
	assert.NotContains(t, remIDs, sweepSupersededOpen)
	assert.NotContains(t, remIDs, sweepSoftDelOpen)
	assert.NotContains(t, remIDs, sweepHardDelFinal)

	// Verify meter_health count via Ent predicate too.
	cnt, err := client.MeterHealth.Query().Where(meterhealth.IDIn(keepLiveOpen, keepLiveFinal, keepAtCutoff)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, cnt)
}

func TestUsageRetention_RunAttribution_D8aRule(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ret-ra")
	projUUID := uuid.MustParse(projectID)
	cutoff := time.Date(2025, 11, 20, 0, 0, 0, 0, time.UTC)

	liveAgentID := uuid.New()
	_, err := client.Agent.Create().
		SetID(liveAgentID).
		SetSlug("ra-live-agent").
		SetName("ra-live-agent").
		SetProjectID(projUUID).
		SetRunID("run-live-current").
		SetStatus(agent.StatusRunning).
		Save(ctx)
	require.NoError(t, err)

	softDelAgentID := uuid.New()
	_, err = client.Agent.Create().
		SetID(softDelAgentID).
		SetSlug("ra-softdel-agent").
		SetName("ra-softdel-agent").
		SetProjectID(projUUID).
		SetRunID("run-softdel-eligible").
		SetStatus(agent.StatusStopped).
		SetDeletedAt(cutoff.Add(-10 * 24 * time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	seedRA := func(runID, agentID string, createdAt time.Time, legalHold bool) {
		t.Helper()
		_, err := client.RunAttribution.Create().
			SetID(runID).
			SetHubID("hub-1").
			SetProjectID(projectID).
			SetAgentID(agentID).
			SetLegalHold(legalHold).
			SetCreatedAt(createdAt).
			Save(ctx)
		require.NoError(t, err)
	}

	oldTS := cutoff.Add(-48 * time.Hour)
	// Protected: live agent current run.
	seedRA("run-live-current", liveAgentID.String(), oldTS, false)
	// Protected: legal_hold = true.
	seedRA("run-legal-hold", liveAgentID.String(), oldTS, true)
	// Protected: exactly at cutoff.
	seedRA("run-at-cutoff", liveAgentID.String(), cutoff, false)
	// Protected: referenced by usage_events.
	seedRA("run-with-event", liveAgentID.String(), oldTS, false)
	_, err = client.UsageEvent.Create().
		SetHubID("hub-1").
		SetEventID("evt-ref-1").
		SetRunID("run-with-event").
		SetProjectID(projectID).
		SetAgentID(liveAgentID.String()).
		SetModel("claude-sonnet-4-6").
		SetModelSource("reported").
		SetUsageSource("native").
		SetStatus("ok").
		SetCalls(1).
		SetTokenStatus("complete").
		SetEventTs(oldTS).
		SetReceivedAt(oldTS).
		Save(ctx)
	require.NoError(t, err)

	// Protected: referenced by meter_health.
	seedRA("run-with-mh", liveAgentID.String(), oldTS, false)
	_, err = client.MeterHealth.Create().
		SetAgentID(liveAgentID.String()).
		SetRunID("run-with-mh").
		SetProducerEpoch("ep-1").
		SetLastReceiptAt(oldTS).
		SetFinalState(store.MeterHealthFinalStateFinal).
		SetUpdatedAt(oldTS).
		Save(ctx)
	require.NoError(t, err)

	// Sweepable: superseded run of live agent, no refs, legal_hold=false, older than cutoff.
	seedRA("run-superseded-eligible", liveAgentID.String(), oldTS, false)
	// Sweepable: soft-deleted agent's run, no refs, legal_hold=false, older than cutoff.
	seedRA("run-softdel-eligible", softDelAgentID.String(), oldTS, false)

	n, err := cs.SweepRunAttributionBatch(ctx, cutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	remaining, err := client.RunAttribution.Query().Select(runattribution.FieldID).Strings(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"run-live-current",
		"run-legal-hold",
		"run-at-cutoff",
		"run-with-event",
		"run-with-mh",
	}, remaining)

	// Once usage_events and meter_health are swept, those two runs become sweepable too.
	_, err = cs.SweepUsageEventsBatch(ctx, cutoff, nil, 100)
	require.NoError(t, err)
	_, err = cs.SweepMeterHealthBatch(ctx, cutoff, 100)
	require.NoError(t, err)

	n, err = cs.SweepRunAttributionBatch(ctx, cutoff, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	remaining, err = client.RunAttribution.Query().Select(runattribution.FieldID).Strings(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"run-live-current",
		"run-legal-hold",
		"run-at-cutoff",
	}, remaining)
}

func TestUsageRetention_BoundedBatching(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ret-batch")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, nil)

	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		ts := cutoff.Add(-time.Duration(12-i) * time.Hour)
		_, err := client.UsageEvent.Create().
			SetHubID("hub-1").
			SetEventID(fmt.Sprintf("batch-evt-%02d", i)).
			SetRunID(runID).
			SetProjectID(projectID).
			SetAgentID(agentID).
			SetModel("claude-sonnet-4-6").
			SetModelSource("reported").
			SetUsageSource("native").
			SetStatus("ok").
			SetCalls(1).
			SetTokenStatus("complete").
			SetEventTs(ts).
			SetReceivedAt(ts).
			Save(ctx)
		require.NoError(t, err)
	}

	b1, err := cs.SweepUsageEventsBatch(ctx, cutoff, nil, 5)
	require.NoError(t, err)
	assert.Equal(t, int64(5), b1)

	b2, err := cs.SweepUsageEventsBatch(ctx, cutoff, nil, 5)
	require.NoError(t, err)
	assert.Equal(t, int64(5), b2)

	b3, err := cs.SweepUsageEventsBatch(ctx, cutoff, nil, 5)
	require.NoError(t, err)
	assert.Equal(t, int64(2), b3)

	b4, err := cs.SweepUsageEventsBatch(ctx, cutoff, nil, 5)
	require.NoError(t, err)
	assert.Equal(t, int64(0), b4)

	// Invalid batchSize rejected.
	_, err = cs.SweepUsageEventsBatch(ctx, cutoff, nil, 0)
	require.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestUsageRetention_StatePersistence(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	// Initially empty (not an error).
	got, err := cs.GetUsageRetentionState(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.LastCompletedAt)
	assert.Nil(t, got.HourlySweptThrough)
	assert.False(t, got.InProgress)
	assert.Equal(t, 0, got.NextTargetIndex)

	// Insert initial state.
	t1 := time.Date(2026, 11, 20, 14, 30, 0, 0, time.UTC)
	wm1 := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)
	require.NoError(t, cs.SaveUsageRetentionState(ctx, &store.UsageRetentionState{
		LastCompletedAt:    &t1,
		NextTargetIndex:    2,
		InProgress:         true,
		HourlySweptThrough: &wm1,
	}))

	got, err = cs.GetUsageRetentionState(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.LastCompletedAt)
	require.NotNil(t, got.HourlySweptThrough)
	assert.Equal(t, t1, *got.LastCompletedAt)
	assert.Equal(t, wm1, *got.HourlySweptThrough)
	assert.True(t, got.InProgress)
	assert.Equal(t, 2, got.NextTargetIndex)

	// Upsert updated state.
	t2 := t1.Add(24 * time.Hour)
	wm2 := wm1.Add(24 * time.Hour)
	require.NoError(t, cs.SaveUsageRetentionState(ctx, &store.UsageRetentionState{
		LastCompletedAt:    &t2,
		NextTargetIndex:    0,
		InProgress:         false,
		HourlySweptThrough: &wm2,
	}))

	got, err = cs.GetUsageRetentionState(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.LastCompletedAt)
	require.NotNil(t, got.HourlySweptThrough)
	assert.Equal(t, t2, *got.LastCompletedAt)
	assert.Equal(t, wm2, *got.HourlySweptThrough)
	assert.False(t, got.InProgress)
	assert.Equal(t, 0, got.NextTargetIndex)
}
