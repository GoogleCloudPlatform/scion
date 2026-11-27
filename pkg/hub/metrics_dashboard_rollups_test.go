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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func i64ptr(v int64) *int64 { return &v }

// setHourlyCoverageStartForTest sets the internal hourly coverage marker directly
// so tests can deterministically exercise hourly vs daily granularity selection
// on both SQLite and Postgres.
func setHourlyCoverageStartForTest(t *testing.T, client *ent.Client, startedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(map[string]string{
		"started_at": startedAt.UTC().Truncate(time.Hour).Format(time.RFC3339),
	})
	require.NoError(t, err)
	_, err = client.HubSetting.Delete().Where(hubsetting.SectionEQ(store.UsageRollupHourlyCoverageSection)).Exec(ctx)
	require.NoError(t, err)
	_, err = client.HubSetting.Create().
		SetSection(store.UsageRollupHourlyCoverageSection).
		SetValue(raw).
		SetRevision(1).
		Save(ctx)
	require.NoError(t, err)
}

func seedDashboardAgentAndRun(t *testing.T, cs *entadapter.CompositeStore, projectID, userID, slug, harness, phase string, createdAt time.Time) (agentID, runID string) {
	t.Helper()
	ctx := context.Background()
	agentID = uuid.NewString()
	runID = "run-" + uuid.NewString()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Slug:      slug + "-" + agentID[:8],
		Name:      slug,
		ProjectID: projectID,
		OwnerID:   userID,
		CreatedBy: userID,
		Phase:     phase,
		RunID:     runID,
	}))
	var uid *string
	if userID != "" {
		uid = &userID
	}
	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		Generation:    1,
		HubID:         "hub-dash",
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: uid,
		Harness:       harness,
		CreatedAt:     createdAt,
	})
	require.NoError(t, err)
	return agentID, runID
}

func makeDashUsageEvent(eventID, runID, projectID, agentID string, billingUserID *string, harness, model string, seq int64, ts time.Time, calls int64, in, out, cr, cw *int64) *store.UsageEvent {
	tokenStatus := telemetrycontract.TokenStatusReported
	if in == nil && out == nil && cr == nil && cw == nil {
		tokenStatus = telemetrycontract.TokenStatusNotReported
	} else if in == nil || out == nil || cr == nil || cw == nil {
		tokenStatus = telemetrycontract.TokenStatusPartial
	}
	return &store.UsageEvent{
		HubID:         "hub-dash",
		EventID:       eventID,
		RunID:         runID,
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: billingUserID,
		Harness:       harness,
		Model:         model,
		ModelSource:   telemetrycontract.ModelSourceEvent,
		UsageSource:   store.UsageSourceNative,
		Status:        "ok",
		Calls:         calls,
		Input:         in,
		Output:        out,
		CacheRead:     cr,
		CacheWrite:    cw,
		TokenStatus:   tokenStatus,
		EventTS:       ts.UTC(),
		ReceivedAt:    ts.UTC(),
		ProducerEpoch: "ep-1",
		ProducerSeq:   seq,
	}
}

func TestComputeCacheHitRate(t *testing.T) {
	// Fully known ratio: 36000 / (12000 + 36000 + 2000) = 0.72, partial=false
	r, partial := computeCacheHitRate(i64ptr(12000), i64ptr(36000), i64ptr(2000), 0, 0, 0, false)
	require.NotNil(t, r)
	assert.InDelta(t, 0.72, *r, 1e-9)
	assert.False(t, partial)

	// Zero cache_read with positive input -> 0.0 (known zero, not nil), partial=false
	rZero, partialZero := computeCacheHitRate(i64ptr(1000), i64ptr(0), i64ptr(0), 0, 0, 0, false)
	require.NotNil(t, rZero)
	assert.Equal(t, 0.0, *rZero)
	assert.False(t, partialZero)

	// Zero denominator -> (nil, false) (never divide by zero / NaN)
	rNil, partialNil := computeCacheHitRate(i64ptr(0), i64ptr(0), i64ptr(0), 0, 0, 0, false)
	assert.Nil(t, rNil)
	assert.False(t, partialNil)

	// Unknown input or cacheRead -> (nil, false)
	rInNil, pInNil := computeCacheHitRate(nil, i64ptr(100), i64ptr(0), 0, 0, 0, false)
	assert.Nil(t, rInNil)
	assert.False(t, pInNil)
	rCRNil, pCRNil := computeCacheHitRate(i64ptr(100), nil, i64ptr(0), 0, 0, 0, false)
	assert.Nil(t, rCRNil)
	assert.False(t, pCRNil)

	// Token-bearing unknown cacheRead > 0 (D11b: numerator understated) -> (nil, false)
	rCRUnk, pCRUnk := computeCacheHitRate(i64ptr(200), i64ptr(0), i64ptr(0), 0, 1, 0, false)
	assert.Nil(t, rCRUnk)
	assert.False(t, pCRUnk)

	// Unknown cacheWrite (e.g. legacy rows) -> true upper bound 50/(100+50) with partial=true
	rCWNil, pCWNil := computeCacheHitRate(i64ptr(100), i64ptr(50), nil, 0, 0, 1, false)
	require.NotNil(t, rCWNil)
	assert.InDelta(t, 50.0/150.0, *rCWNil, 1e-9)
	assert.True(t, pCWNil)

	// Mixed known+unknown cacheWrite -> true upper bound 900/(1100+900+0)=0.45 with partial=true
	rMixed, pMixed := computeCacheHitRate(i64ptr(1100), i64ptr(900), i64ptr(0), 0, 0, 1, false)
	require.NotNil(t, rMixed)
	assert.InDelta(t, 0.45, *rMixed, 1e-9)
	assert.True(t, pMixed)

	// Mixed known+unknown input with known cacheRead -> true upper bound with partial=true
	rInUnk, pInUnk := computeCacheHitRate(i64ptr(100), i64ptr(100), i64ptr(0), 1, 0, 0, false)
	require.NotNil(t, rInUnk)
	assert.InDelta(t, 0.50, *rInUnk, 1e-9)
	assert.True(t, pInUnk)

	// Untracked legacy portion present (events > presence_tracked_events) -> partial=true
	rUntracked, pUntracked := computeCacheHitRate(i64ptr(500), i64ptr(200), i64ptr(100), 0, 0, 0, true)
	require.NotNil(t, rUntracked)
	assert.InDelta(t, 200.0/800.0, *rUntracked, 1e-9)
	assert.True(t, pUntracked)
}

// TestHubRollupSource_SummaryNullableTokensAndHitRate runs against SQLite by
// default and against real Postgres under `-tags integration` with
// SCION_TEST_POSTGRES_URL set (via enttest.NewClient).
func TestHubRollupSource_SummaryNullableTokensAndHitRate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	eventTS := dbNow.Add(-2 * time.Hour).Truncate(time.Minute)

	u1 := &store.User{ID: uuid.NewString(), Email: "dash-sum-1@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	proj := &store.Project{ID: uuid.NewString(), Name: "dash-sum", Slug: "dash-sum-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))
	projNull := &store.Project{ID: uuid.NewString(), Name: "dash-null", Slug: "dash-null-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, projNull))

	ag1, run1 := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-measured", "claude", string(state.PhaseRunning), eventTS.Add(-time.Hour))
	agNull, runNull := seedDashboardAgentAndRun(t, cs, projNull.ID, u1.ID, "ag-null", "antigravity", string(state.PhaseRunning), eventTS.Add(-time.Hour))

	// Measured project with known cache_read/write -> hit rate = 36000 / 50000 = 0.72.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag1,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("sum-1", run1, proj.ID, ag1, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 4, i64ptr(12000), i64ptr(4500), i64ptr(36000), i64ptr(2000)),
		},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       ag1,
			RunID:         run1,
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-10 * time.Second),
			ProducerSeq:   1,
			ReceivedSeqs:  []int64{1},
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// Null-cache project: input & output known, cacheRead & cacheWrite unknown (nil).
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agNull,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("sum-2", runNull, projNull.ID, agNull, &u1.ID, "antigravity", "gemini-2.5-pro", 1, eventTS, 2, i64ptr(500), i64ptr(200), nil, nil),
		},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agNull,
			RunID:         runNull,
			ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-10 * time.Second),
			ProducerSeq:   1,
			ReceivedSeqs:  []int64{1},
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// Set hourly coverage before the 7d window start so hourly granularity is selected.
	setHourlyCoverageStartForTest(t, client, dbNow.AddDate(0, 0, -10))

	src := NewHubRollupSource(cs)

	// 1. Measured project summary
	sum, err := src.QuerySummary(ctx, 7, WithProjectID(proj.ID))
	require.NoError(t, err)
	assert.Equal(t, MetricsSourceHubRollups, sum.Source)
	assert.Equal(t, store.UsageGranularityHour, sum.Granularity)
	assert.Equal(t, int64(4), sum.TotalAPICalls)
	assert.Equal(t, int64(54500), sum.TotalTokens)
	assert.Equal(t, 1, sum.UniqueAgents)
	require.NotNil(t, sum.Tokens)
	require.NotNil(t, sum.Tokens.Input)
	require.NotNil(t, sum.Tokens.Output)
	require.NotNil(t, sum.Tokens.CacheRead)
	require.NotNil(t, sum.Tokens.CacheWrite)
	require.NotNil(t, sum.Tokens.SummableTokens)
	assert.Equal(t, int64(12000), *sum.Tokens.Input)
	assert.Equal(t, int64(4500), *sum.Tokens.Output)
	assert.Equal(t, int64(36000), *sum.Tokens.CacheRead)
	assert.Equal(t, int64(2000), *sum.Tokens.CacheWrite)
	assert.Equal(t, int64(54500), *sum.Tokens.SummableTokens)
	assert.Nil(t, sum.Tokens.CacheWrite1h, "unknown cacheWrite1h must be nil, never 0")
	assert.Nil(t, sum.Tokens.Reasoning, "unknown reasoning must be nil, never 0")
	require.NotNil(t, sum.CacheHitRate)
	assert.InDelta(t, 0.72, *sum.CacheHitRate, 1e-9)
	require.NotNil(t, sum.Measurement)
	assert.Equal(t, MeasurementStatusMeasured, sum.Measurement.Status)

	// 2. Null-cache project summary: cacheRead/cacheWrite/cacheHitRate MUST be JSON null (nil)
	sumNull, err := src.QuerySummary(ctx, 7, WithProjectID(projNull.ID))
	require.NoError(t, err)
	assert.Equal(t, int64(2), sumNull.TotalAPICalls)
	assert.Equal(t, int64(700), sumNull.TotalTokens)
	require.NotNil(t, sumNull.Tokens)
	assert.Nil(t, sumNull.Tokens.CacheRead, "unknown cacheRead must remain nil")
	assert.Nil(t, sumNull.Tokens.CacheWrite, "unknown cacheWrite must remain nil")
	assert.Nil(t, sumNull.CacheHitRate, "hit rate with unknown cache components must be nil")

	// Verify JSON serialization emits explicit null for unknown token buckets.
	rawJSON, err := json.Marshal(sumNull)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(rawJSON, &decoded))
	tokMap := decoded["tokens"].(map[string]any)
	val, exists := tokMap["cacheRead"]
	assert.True(t, exists, "cacheRead key must be present")
	assert.Nil(t, val, "unknown cacheRead must serialize as JSON null")
}

func TestHubRollupSource_SessionsNeverSumsAcrossBuckets(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	day2TS := dbNow.Add(-2 * time.Hour).Truncate(time.Minute)
	day1TS := day2TS.Add(-24 * time.Hour)

	u1 := &store.User{ID: uuid.NewString(), Email: "dash-sess@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	proj := &store.Project{ID: uuid.NewString(), Name: "dash-sess", Slug: "dash-sess-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))

	ag1, run1 := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-1", "claude", string(state.PhaseRunning), day1TS.Add(-time.Hour))
	ag2, run2 := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-2", "claude", string(state.PhaseRunning), day2TS.Add(-time.Hour))

	// Session s1 spans Day 1 and Day 2; session s2 is on Day 2.
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag1, ProjectID: proj.ID, SessionID: "s1", StartedAt: day1TS,
	}))
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag1, ProjectID: proj.ID, SessionID: "s1", StartedAt: day2TS,
	}))
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag2, ProjectID: proj.ID, SessionID: "s2", StartedAt: day2TS,
	}))

	// Usage events: ag1 active on Day 1 (two hours) and Day 2; ag2 active on Day 2.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag1,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("sess-e1", run1, proj.ID, ag1, &u1.ID, "claude", "claude-sonnet-4-6", 1, day1TS, 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("sess-e2", run1, proj.ID, ag1, &u1.ID, "claude", "claude-sonnet-4-6", 2, day1TS.Add(time.Hour), 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("sess-e3", run1, proj.ID, ag1, &u1.ID, "claude", "claude-sonnet-4-6", 3, day2TS, 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
		},
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag2,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("sess-e4", run2, proj.ID, ag2, &u1.ID, "claude", "claude-sonnet-4-6", 1, day2TS, 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
		},
	})
	require.NoError(t, err)
	setHourlyCoverageStartForTest(t, client, dbNow.AddDate(0, 0, -10))

	src := NewHubRollupSource(cs)
	sum, err := src.QuerySummary(ctx, 7, WithProjectID(proj.ID))
	require.NoError(t, err)
	assert.Equal(t, int64(2), sum.TotalSessions, "TotalSessions must be exact COUNT(DISTINCT), never sum of daily buckets (1+2=3)")
	assert.Equal(t, 2, sum.UniqueAgents, "UniqueAgents must be exact COUNT(DISTINCT), never sum of daily buckets (1+2=3)")

	sessView, err := src.QuerySessions(ctx, 7, WithProjectID(proj.ID))
	require.NoError(t, err)
	assert.Equal(t, MetricsSourceHubRollups, sessView.Source)
	// Sum of per-day buckets is 1+2=3, proving TotalSessions (2) did NOT sum buckets.
	var dailySessSum, dailyAgentSum int64
	for _, p := range sessView.DailyCounts {
		dailySessSum += p.Value
	}
	for _, p := range sessView.ActiveAgents {
		dailyAgentSum += p.Value
	}
	assert.Equal(t, int64(3), dailySessSum)
	assert.Equal(t, int64(3), dailyAgentSum)
}

func TestHubRollupSource_TimezoneBucketingAndDailyFallback(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	// Pick 02:00 UTC on yesterday (guaranteed in the past, inside 7d window):
	// In UTC -> day D; in America/Chicago (UTC-5 or UTC-6) -> day D-1; in Asia/Tokyo (UTC+9) -> day D.
	yesterdayUTC := time.Date(dbNow.UTC().Year(), dbNow.UTC().Month(), dbNow.UTC().Day(), 2, 0, 0, 0, time.UTC).AddDate(0, 0, -1)

	u1 := &store.User{ID: uuid.NewString(), Email: "dash-tz@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	proj := &store.Project{ID: uuid.NewString(), Name: "dash-tz", Slug: "dash-tz-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))
	ag, runID := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-tz", "claude", string(state.PhaseRunning), yesterdayUTC.Add(-time.Hour))

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("tz-1", runID, proj.ID, ag, &u1.ID, "claude", "claude-sonnet-4-6", 1, yesterdayUTC, 5, i64ptr(100), i64ptr(50), i64ptr(0), i64ptr(0)),
		},
	})
	require.NoError(t, err)
	setHourlyCoverageStartForTest(t, client, dbNow.AddDate(0, 0, -10))

	chicago, err := time.LoadLocation("America/Chicago")
	require.NoError(t, err)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)

	src := NewHubRollupSource(cs)

	// Hourly mode: America/Chicago shifts 02:00Z to the previous local calendar day.
	callsChi, err := src.QueryModelCalls(ctx, 7, WithProjectID(proj.ID), WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Equal(t, "America/Chicago", callsChi.TimeZone)
	assert.Equal(t, store.UsageGranularityHour, callsChi.Granularity)
	require.Len(t, callsChi.ByModel, 1)
	require.Len(t, callsChi.ByModel[0].Points, 1)
	assert.Equal(t, dayKey(yesterdayUTC, chicago), callsChi.ByModel[0].Points[0].Timestamp)
	assert.Equal(t, int64(5), callsChi.ByModel[0].Points[0].Value)

	// Hourly mode: Asia/Tokyo keeps 02:00Z on the UTC day (11:00 JST).
	callsTok, err := src.QueryModelCalls(ctx, 7, WithProjectID(proj.ID), WithTimeZone(tokyo))
	require.NoError(t, err)
	assert.Equal(t, "Asia/Tokyo", callsTok.TimeZone)
	require.Len(t, callsTok.ByModel, 1)
	assert.Equal(t, dayKey(yesterdayUTC, tokyo), callsTok.ByModel[0].Points[0].Timestamp)

	// Daily fallback (period=30 > 14d hourly retention): must fall back to daily and report timeZone="UTC".
	callsDaily, err := src.QueryModelCalls(ctx, 30, WithProjectID(proj.ID), WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Equal(t, "UTC", callsDaily.TimeZone, "daily rollup fallback must honestly report timeZone=UTC")
	assert.Equal(t, store.UsageGranularityDay, callsDaily.Granularity)
	require.Len(t, callsDaily.ByModel, 1)
	assert.Equal(t, dayKey(yesterdayUTC, time.UTC), callsDaily.ByModel[0].Points[0].Timestamp)

	// Hourly vs daily consistency mismatch (D7a): add a daily-only rollup row inside the 7d window
	// so CheckUsageHourlyConsistency returns false -> must fall back to daily and report timeZone="UTC".
	yesterdayDay := time.Date(yesterdayUTC.Year(), yesterdayUTC.Month(), yesterdayUTC.Day(), 0, 0, 0, 0, time.UTC)
	_, err = client.UsageRollupDaily.Create().
		SetBucketStart(yesterdayDay).
		SetProjectID(proj.ID).
		SetBillingUserID(u1.ID).
		SetAgentID(ag).
		SetHarness("claude").
		SetModel("claude-haiku-4-5").
		SetUsageSource(store.UsageSourceNative).
		SetCalls(3).
		SetInput(50).
		SetOutput(25).
		SetEvents(1).
		SetInputKnownEvents(1).
		SetOutputKnownEvents(1).
		SetPresenceTrackedEvents(1).
		SetFirstEventTs(yesterdayUTC).
		SetLastEventTs(yesterdayUTC).
		Save(ctx)
	require.NoError(t, err)

	callsMismatch, err := src.QueryModelCalls(ctx, 7, WithProjectID(proj.ID), WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Equal(t, "UTC", callsMismatch.TimeZone, "hourly/daily mismatch must fall back to daily and report timeZone=UTC")
	assert.Equal(t, store.UsageGranularityDay, callsMismatch.Granularity)
	require.Len(t, callsMismatch.ByModel, 2, "daily fallback must include the daily-only row")

	// D7b coverage-day edge case (R2): set hourlyCoverageStart to the middle of yesterdayUTC
	// (the first UTC day overlapped by a 2-day window) so the daily-only discrepancy is on the
	// coverage day itself -> must STILL fall back to daily and report timeZone="UTC".
	setHourlyCoverageStartForTest(t, client, yesterdayDay.Add(1*time.Hour))
	callsCoverageEdge, err := src.QueryModelCalls(ctx, 2, WithProjectID(proj.ID), WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Equal(t, "UTC", callsCoverageEdge.TimeZone, "D7b: coverage-day discrepancy must fall back to daily and report timeZone=UTC")
	assert.Equal(t, store.UsageGranularityDay, callsCoverageEdge.Granularity)
}

func TestHubRollupSource_GroupedSeriesModelHarnessUser(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	ts := dbNow.Add(-2 * time.Hour).Truncate(time.Minute)

	u1 := &store.User{ID: uuid.NewString(), Email: "grp-u1@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	u2 := &store.User{ID: uuid.NewString(), Email: "grp-u2@example.com", DisplayName: "U2", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	require.NoError(t, cs.CreateUser(ctx, u2))
	proj := &store.Project{ID: uuid.NewString(), Name: "dash-grp", Slug: "dash-grp-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))

	ag1, run1 := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-claude", "claude", string(state.PhaseRunning), ts.Add(-time.Hour))
	ag2, run2 := seedDashboardAgentAndRun(t, cs, proj.ID, u2.ID, "ag-gemini", "gemini-cli", string(state.PhaseRunning), ts.Add(-time.Hour))

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag1,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("grp-1", run1, proj.ID, ag1, &u1.ID, "claude", "claude-sonnet-4-6", 1, ts, 3, i64ptr(100), i64ptr(50), i64ptr(300), i64ptr(100)),
		},
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag2,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("grp-2", run2, proj.ID, ag2, &u2.ID, "gemini-cli", "", 1, ts, 2, i64ptr(200), i64ptr(80), nil, nil),
		},
	})
	require.NoError(t, err)
	setHourlyCoverageStartForTest(t, client, dbNow.AddDate(0, 0, -10))

	src := NewHubRollupSource(cs)

	// ModelCalls with groupBy=user populates byModel, byHarness, AND byUser; empty model -> "(unknown)".
	calls, err := src.QueryModelCalls(ctx, 7, WithProjectID(proj.ID), WithGroupBy(store.UsageGroupByUser))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGroupByUser, calls.GroupBy)
	require.Len(t, calls.ByModel, 2)
	assert.Equal(t, "(unknown)", calls.ByModel[0].Label)
	assert.Equal(t, "claude-sonnet-4-6", calls.ByModel[1].Label)
	require.Len(t, calls.ByHarness, 2)
	require.Len(t, calls.ByUser, 2)

	// Tokens grouped by user: u2 has nil cacheRead/cacheWrite so u2 must NOT appear in CacheRead/CacheWrite series.
	toksByUser, err := src.QueryTokens(ctx, 7, WithProjectID(proj.ID), WithGroupBy(store.UsageGroupByUser))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGroupByUser, toksByUser.GroupBy)
	assert.Len(t, toksByUser.Input, 2)
	assert.Len(t, toksByUser.Output, 2)
	require.Len(t, toksByUser.CacheRead, 1, "u2 has unknown cacheRead and must not emit fabricated 0 series")
	assert.Equal(t, u1.ID, toksByUser.CacheRead[0].Label)
	require.Len(t, toksByUser.CacheWrite, 1)
	assert.Equal(t, u1.ID, toksByUser.CacheWrite[0].Label)
	require.NotNil(t, toksByUser.Totals)
	assert.Equal(t, int64(300), *toksByUser.Totals.Input)
	require.Len(t, toksByUser.DailyCacheHitRate, 1)
	// ag2 is a token-bearing event with unknown cacheRead -> per D11b, day hit rate is nil (not partial).
	assert.Nil(t, toksByUser.DailyCacheHitRate[0].Value)
	assert.False(t, toksByUser.DailyCacheHitRate[0].Partial)

	// Tokens grouped by harness
	toksByHarness, err := src.QueryTokens(ctx, 7, WithProjectID(proj.ID), WithGroupBy(store.UsageGroupByHarness))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGroupByHarness, toksByHarness.GroupBy)
	require.Len(t, toksByHarness.Input, 2)
	assert.Equal(t, "claude", toksByHarness.Input[0].Label)
	assert.Equal(t, "gemini-cli", toksByHarness.Input[1].Label)
}

func TestHubRollupSource_ProjectSummary24h(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	tsRecent := dbNow.Add(-2 * time.Hour).Truncate(time.Minute)
	tsOld := dbNow.Add(-72 * time.Hour).Truncate(time.Minute)

	u1 := &store.User{ID: uuid.NewString(), Email: "ps24@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	proj := &store.Project{ID: uuid.NewString(), Name: "ps24", Slug: "ps24-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))
	ag, runID := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-ps24", "claude", string(state.PhaseRunning), tsOld)

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("ps24-old", runID, proj.ID, ag, &u1.ID, "claude", "claude-sonnet-4-6", 1, tsOld, 99, i64ptr(9999), i64ptr(9999), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("ps24-new", runID, proj.ID, ag, &u1.ID, "claude", "claude-sonnet-4-6", 2, tsRecent, 3, i64ptr(100), i64ptr(50), i64ptr(300), i64ptr(100)),
		},
	})
	require.NoError(t, err)
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag, ProjectID: proj.ID, SessionID: "s-recent", StartedAt: tsRecent,
	}))
	setHourlyCoverageStartForTest(t, client, dbNow.AddDate(0, 0, -10))

	src := NewHubRollupSource(cs)
	ps, err := src.QueryProjectSummary(ctx, proj.ID)
	require.NoError(t, err)
	assert.Equal(t, MetricsSourceHubRollups, ps.Source)
	assert.Equal(t, store.UsageGranularityHour, ps.Granularity)
	assert.Equal(t, "Last 24h (hourly buckets)", ps.PeriodLabel)
	assert.Equal(t, int64(3), ps.APICalls24h)
	assert.Equal(t, int64(550), ps.TokenUsage24h)
	assert.Equal(t, int64(1), ps.SessionsCount24h)
	assert.Equal(t, 1, ps.ActiveAgents24h)
	require.NotNil(t, ps.CacheHitRate)
	assert.InDelta(t, 300.0/500.0, *ps.CacheHitRate, 1e-9)
}

func TestMetricsDashboard_SourceSelectionMatrix(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-dash-matrix")

	admin := &store.User{ID: uuid.NewString(), Email: "dash-matrix-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)

	proj := &store.Project{ID: tid("dash-matrix-proj"), Name: "dash-matrix", Slug: "dash-matrix"}
	require.NoError(t, s.CreateProject(ctx, proj))

	cmClient := newFakeMetricsClient()
	cmSvc := &MetricsDashboardService{
		client:    cmClient,
		projectID: "gcp-proj-test",
		cache:     make(map[string]*cacheEntry),
	}

	doGET := func(path string) *httptest.ResponseRecorder {
		return doUserUsageReadRequest(t, srv, path, admin)
	}

	// Case 1: experiment OFF + CM unconfigured -> 503 on dashboard, available=false on project summary.
	setUsageLedgerExperimentForTest(t, srv, false)
	srv.mu.Lock()
	srv.metricsDashboard = nil
	srv.mu.Unlock()

	rec := doGET("/api/v1/metrics/?view=summary&period=7")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	recProjSum := doGET("/api/v1/projects/" + proj.ID + "/metrics-summary")
	require.Equal(t, http.StatusOK, recProjSum.Code)
	var avail map[string]any
	require.NoError(t, json.Unmarshal(recProjSum.Body.Bytes(), &avail))
	assert.Equal(t, false, avail["available"])

	// Case 2: experiment OFF + CM configured -> cloud_monitoring; source is omitted (O1) and groupBy is ignored (O5).
	srv.mu.Lock()
	srv.metricsDashboard = cmSvc
	srv.mu.Unlock()

	rec = doGET("/api/v1/metrics/?view=summary&period=7")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sumCMRaw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sumCMRaw))
	_, hasSourceOff := sumCMRaw["source"]
	assert.False(t, hasSourceOff, "experiment-off CM response must omit source field (O1)")

	for _, rawGroup := range []string{"user", "bogus"} {
		rec = doGET("/api/v1/metrics/?view=tokens&period=7&groupBy=" + rawGroup)
		require.Equal(t, http.StatusOK, rec.Code, "experiment off must ignore groupBy=%s (no new 400s)", rawGroup)
		var tokCMRaw map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tokCMRaw))
		_, tokHasSource := tokCMRaw["source"]
		assert.False(t, tokHasSource, "experiment-off CM tokens response must omit source field (O1)")
	}

	// Case 3: experiment ON + CM unconfigured -> hub_rollups for all views & project summary.
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.mu.Lock()
	srv.metricsDashboard = nil
	srv.mu.Unlock()

	for _, view := range []string{"summary", "sessions", "model-calls", "tokens"} {
		rec = doGET("/api/v1/projects/" + proj.ID + "/metrics?view=" + view + "&period=7")
		require.Equal(t, http.StatusOK, rec.Code, "view=%s body=%s", view, rec.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, MetricsSourceHubRollups, body["source"], "view=%s", view)
		assert.Empty(t, body["telemetryHint"], "hub_rollups must not attach GCP telemetryHint")
	}

	recProjSum = doGET("/api/v1/projects/" + proj.ID + "/metrics-summary")
	require.Equal(t, http.StatusOK, recProjSum.Code, recProjSum.Body.String())
	var psRollup ProjectMetricsSummary
	require.NoError(t, json.Unmarshal(recProjSum.Body.Bytes(), &psRollup))
	assert.Equal(t, MetricsSourceHubRollups, psRollup.Source)

	// Case 4: experiment ON + CM configured:
	// - default views use cloud_monitoring
	// - CM-unsupported breakdowns (groupBy=user; view=tokens&groupBy=harness) route to hub_rollups
	srv.mu.Lock()
	srv.metricsDashboard = cmSvc
	srv.mu.Unlock()

	rec = doGET("/api/v1/metrics/?view=summary&period=7")
	require.Equal(t, http.StatusOK, rec.Code)
	var sumDefault DashboardSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sumDefault))
	assert.Equal(t, MetricsSourceCloudMonitoring, sumDefault.Source)

	rec = doGET("/api/v1/metrics/?view=model-calls&period=7&groupBy=user")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var mcUser ModelCallsView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mcUser))
	assert.Equal(t, MetricsSourceHubRollups, mcUser.Source)
	assert.Equal(t, store.UsageGroupByUser, mcUser.GroupBy)

	rec = doGET("/api/v1/metrics/?view=tokens&period=7&groupBy=harness")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var tokHarness TokensView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tokHarness))
	assert.Equal(t, MetricsSourceHubRollups, tokHarness.Source)
	assert.Equal(t, store.UsageGroupByHarness, tokHarness.GroupBy)

	// Case 5: invalid groupBy or groupBy on unsupported view -> 400.
	assert.Equal(t, http.StatusBadRequest, doGET("/api/v1/metrics/?view=tokens&groupBy=bogus").Code)
	assert.Equal(t, http.StatusBadRequest, doGET("/api/v1/metrics/?view=summary&groupBy=user").Code)
}

type sessionFailStore struct {
	store.Store
	err error
}

func (s *sessionFailStore) CountSessionMetrics(_ context.Context, _ store.SessionMetricsCountQuery) (*store.SessionMetricsCountResult, error) {
	return nil, s.err
}

// TestHubRollupSource_TruncationDegradesAndDropsBoundaryDay pins R1 and O1:
// when hourly grouped queries hit the internal limit, HubRollupSource degrades
// to daily granularity (timeZone="UTC"); when daily still truncates, it sets
// Truncated=true and drops the earliest boundary day so no day ever appears
// with partial values.
func TestHubRollupSource_TruncationDegradesAndDropsBoundaryDay(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	day2 := time.Date(dbNow.UTC().Year(), dbNow.UTC().Month(), dbNow.UTC().Day(), 10, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	day1 := day2.AddDate(0, 0, -1)

	u1 := &store.User{ID: uuid.NewString(), Email: "trunc@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	proj := &store.Project{ID: uuid.NewString(), Name: "dash-trunc", Slug: "dash-trunc-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))
	ag1, run1 := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-trunc-1", "claude", string(state.PhaseRunning), day1.Add(-time.Hour))
	ag2, run2 := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-trunc-2", "gemini-cli", string(state.PhaseRunning), day1.Add(-time.Hour))

	// Seed 2 days x 2 hours x 2 models = 8 hourly (hour, model) groups and 4 daily (day, model) groups.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag1,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("tr-1", run1, proj.ID, ag1, &u1.ID, "claude", "model-a", 1, day1, 2, i64ptr(100), i64ptr(50), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("tr-2", run1, proj.ID, ag1, &u1.ID, "claude", "model-a", 2, day1.Add(time.Hour), 3, i64ptr(100), i64ptr(50), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("tr-3", run1, proj.ID, ag1, &u1.ID, "claude", "model-a", 3, day2, 4, i64ptr(200), i64ptr(50), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("tr-4", run1, proj.ID, ag1, &u1.ID, "claude", "model-a", 4, day2.Add(time.Hour), 5, i64ptr(200), i64ptr(50), i64ptr(0), i64ptr(0)),
		},
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag2,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("tr-5", run2, proj.ID, ag2, &u1.ID, "gemini-cli", "model-b", 1, day1, 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("tr-6", run2, proj.ID, ag2, &u1.ID, "gemini-cli", "model-b", 2, day1.Add(time.Hour), 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("tr-7", run2, proj.ID, ag2, &u1.ID, "gemini-cli", "model-b", 3, day2, 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("tr-8", run2, proj.ID, ag2, &u1.ID, "gemini-cli", "model-b", 4, day2.Add(time.Hour), 1, i64ptr(10), i64ptr(5), i64ptr(0), i64ptr(0)),
		},
	})
	require.NoError(t, err)

	// Seed 4 session segments (2 on day1, 2 on day2) to exercise O1 session truncation too.
	for i, ts := range []time.Time{day1, day1.Add(time.Hour), day2, day2.Add(time.Hour)} {
		require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
			AgentID: ag1, ProjectID: proj.ID, SessionID: "s-" + string(rune('a'+i)), StartedAt: ts,
		}))
	}

	setHourlyCoverageStartForTest(t, client, dbNow.AddDate(0, 0, -10))
	chicago, err := time.LoadLocation("America/Chicago")
	require.NoError(t, err)

	// 1. groupLimit=5: hourly (8 rows) truncates -> degrades to daily (4 rows <= 5), NOT truncated, complete series!
	srcDegrade := NewHubRollupSource(cs)
	srcDegrade.groupLimit = 5

	tokDegrade, err := srcDegrade.QueryTokens(ctx, 7, WithProjectID(proj.ID), WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGranularityDay, tokDegrade.Granularity)
	assert.Equal(t, "UTC", tokDegrade.TimeZone)
	assert.False(t, tokDegrade.Truncated)
	assert.Len(t, tokDegrade.DailyCacheHitRate, 2, "both days must be complete after daily degradation")

	mcDegrade, err := srcDegrade.QueryModelCalls(ctx, 7, WithProjectID(proj.ID), WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGranularityDay, mcDegrade.Granularity)
	assert.Equal(t, "UTC", mcDegrade.TimeZone)
	assert.False(t, mcDegrade.Truncated)
	require.Len(t, mcDegrade.ByModel, 2)
	assert.Len(t, mcDegrade.ByModel[0].Points, 2)
	assert.Len(t, mcDegrade.ByModel[1].Points, 2)

	// 2. groupLimit=3: daily (4 rows) STILL truncates -> Truncated=true, boundary day1 dropped so no partial day appears!
	srcTrunc := NewHubRollupSource(cs)
	srcTrunc.groupLimit = 3
	day1Key := dayKey(day1, time.UTC)
	day2Key := dayKey(day2, time.UTC)

	tokTrunc, err := srcTrunc.QueryTokens(ctx, 7, WithProjectID(proj.ID))
	require.NoError(t, err)
	assert.True(t, tokTrunc.Truncated)
	assert.Equal(t, int64(640), *tokTrunc.Totals.Input, "window Totals stay exact even when series are truncated")
	for _, s := range tokTrunc.Input {
		for _, pt := range s.Points {
			assert.NotEqual(t, day1Key, pt.Timestamp, "partial boundary day1 must be dropped from token series")
			assert.Equal(t, day2Key, pt.Timestamp)
		}
	}
	require.Len(t, tokTrunc.DailyCacheHitRate, 1)
	assert.Equal(t, day2Key, tokTrunc.DailyCacheHitRate[0].Timestamp)

	mcTrunc, err := srcTrunc.QueryModelCalls(ctx, 7, WithProjectID(proj.ID))
	require.NoError(t, err)
	assert.True(t, mcTrunc.Truncated)
	for _, s := range mcTrunc.ByModel {
		for _, pt := range s.Points {
			assert.NotEqual(t, day1Key, pt.Timestamp, "partial boundary day1 must be dropped from model-calls series")
		}
	}

	// 3. O1: QuerySessions with groupLimit=3: hourly active-agents (8 pairs) degrades to daily (2 buckets, complete),
	// while sessions (4 segments > 3) truncates and drops partial boundary day1.
	sessTrunc, err := srcTrunc.QuerySessions(ctx, 7, WithProjectID(proj.ID), WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGranularityDay, sessTrunc.Granularity)
	assert.Equal(t, "UTC", sessTrunc.TimeZone)
	assert.True(t, sessTrunc.Truncated)
	require.Len(t, sessTrunc.DailyCounts, 1)
	assert.Equal(t, day2Key, sessTrunc.DailyCounts[0].Timestamp)
	assert.Equal(t, int64(2), sessTrunc.DailyCounts[0].Value)
}

// TestHubRollupSource_ProjectSummary24hDailyFallbackWindowAndLabel pins O2:
// when hourly coverage is absent, QueryProjectSummary falls back to UTC daily,
// labels PeriodLabel accordingly, and evaluates rollups and sessions on the
// exact same UTC daily window.
func TestHubRollupSource_ProjectSummary24hDailyFallbackWindowAndLabel(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	// Place a session and rollup at 00:05 UTC yesterday (inside yesterday's UTC
	// day, even if > 24h before dbNow).
	yesterdayStart := time.Date(dbNow.UTC().Year(), dbNow.UTC().Month(), dbNow.UTC().Day(), 0, 5, 0, 0, time.UTC).AddDate(0, 0, -1)

	u1 := &store.User{ID: uuid.NewString(), Email: "o2@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	proj := &store.Project{ID: uuid.NewString(), Name: "dash-o2", Slug: "dash-o2-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))
	ag, runID := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-o2", "claude", string(state.PhaseRunning), yesterdayStart.Add(-time.Hour))

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("o2-1", runID, proj.ID, ag, &u1.ID, "claude", "claude-sonnet-4-6", 1, yesterdayStart, 2, i64ptr(100), i64ptr(50), i64ptr(0), i64ptr(0)),
		},
	})
	require.NoError(t, err)
	require.NoError(t, cs.CreateAgentSessionMetrics(ctx, &store.AgentSessionMetrics{
		AgentID: ag, ProjectID: proj.ID, SessionID: "s-o2", StartedAt: yesterdayStart,
	}))
	// Remove hourly coverage marker so resolve24hWindow falls back to daily.
	_, err = client.HubSetting.Delete().Where(hubsetting.SectionEQ(store.UsageRollupHourlyCoverageSection)).Exec(ctx)
	require.NoError(t, err)

	src := NewHubRollupSource(cs)
	ps, err := src.QueryProjectSummary(ctx, proj.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageGranularityDay, ps.Granularity)
	wantSince := time.Date(dbNow.UTC().Add(-24*time.Hour).Year(), dbNow.UTC().Add(-24*time.Hour).Month(), dbNow.UTC().Add(-24*time.Hour).Day(), 0, 0, 0, 0, time.UTC)
	assert.Equal(t, "Since "+wantSince.Format(dayKeyLayout)+" (UTC daily)", ps.PeriodLabel)
	assert.Equal(t, int64(2), ps.APICalls24h)
	assert.Equal(t, int64(1), ps.SessionsCount24h, "sessions must use the same UTC daily window as rollups")
}

// TestHubRollupSource_SessionQueryFailureEmitsNullAndPartialFailure pins O3:
// on a failed CountSessionMetrics call, HubRollupSource emits JSON null for
// totalSessions / sessionsCount24h (never a fabricated 0) with partialFailure=true.
func TestHubRollupSource_SessionQueryFailureEmitsNullAndPartialFailure(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	proj := &store.Project{ID: uuid.NewString(), Name: "dash-o3", Slug: "dash-o3-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))

	faulty := &sessionFailStore{Store: cs, err: errors.New("session db down")}
	src := NewHubRollupSource(faulty)

	sum, err := src.QuerySummary(ctx, 7, WithProjectID(proj.ID))
	require.Error(t, err)
	require.NotNil(t, sum)
	assert.True(t, sum.PartialFailure)
	assert.NotEmpty(t, sum.PartialErrors)

	raw, mErr := json.Marshal(sum)
	require.NoError(t, mErr)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	val, exists := decoded["totalSessions"]
	assert.True(t, exists)
	assert.Nil(t, val, "failed session query must serialize totalSessions as JSON null, never 0")
	assert.Equal(t, true, decoded["partialFailure"])

	ps, err := src.QueryProjectSummary(ctx, proj.ID)
	require.Error(t, err)
	require.NotNil(t, ps)
	assert.True(t, ps.PartialFailure)
	rawPS, mErr := json.Marshal(ps)
	require.NoError(t, mErr)
	var decodedPS map[string]any
	require.NoError(t, json.Unmarshal(rawPS, &decodedPS))
	valPS, existsPS := decodedPS["sessionsCount24h"]
	assert.True(t, existsPS)
	assert.Nil(t, valPS, "failed session query must serialize sessionsCount24h as JSON null, never 0")
	assert.Equal(t, true, decodedPS["partialFailure"])
}

// TestHubRollupSource_SessionsMeasurementMatchesSummary pins O4: when rollup
// events exist without active agent coverage rows (e.g. historical/deleted agent),
// QuerySessions passes the rollup event total so summary and sessions agree on
// measurement.status = "measured".
func TestHubRollupSource_SessionsMeasurementMatchesSummary(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	todayDay := time.Date(dbNow.UTC().Year(), dbNow.UTC().Month(), dbNow.UTC().Day(), 0, 0, 0, 0, time.UTC)

	proj := &store.Project{ID: uuid.NewString(), Name: "dash-o4", Slug: "dash-o4-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// Seed a daily rollup row with no matching non-deleted agent row.
	_, err = client.UsageRollupDaily.Create().
		SetBucketStart(todayDay).
		SetProjectID(proj.ID).
		SetBillingUserID("").
		SetAgentID(uuid.NewString()).
		SetHarness("claude").
		SetModel("claude-sonnet-4-6").
		SetUsageSource(store.UsageSourceNative).
		SetCalls(1).
		SetInput(10).
		SetOutput(5).
		SetEvents(1).
		SetInputKnownEvents(1).
		SetOutputKnownEvents(1).
		SetPresenceTrackedEvents(1).
		SetFirstEventTs(dbNow.Add(-time.Hour)).
		SetLastEventTs(dbNow.Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	src := NewHubRollupSource(cs)
	sum, err := src.QuerySummary(ctx, 7, WithProjectID(proj.ID))
	require.NoError(t, err)
	sess, err := src.QuerySessions(ctx, 7, WithProjectID(proj.ID))
	require.NoError(t, err)
	require.NotNil(t, sum.Measurement)
	require.NotNil(t, sess.Measurement)
	assert.Equal(t, MeasurementStatusMeasured, sum.Measurement.Status)
	assert.Equal(t, sum.Measurement.Status, sess.Measurement.Status, "sessions and summary must agree on measurement.status")
}

// TestHubRollupSource_PartialCacheHitRateProbe pins R1 / D11b:
//   - Mixed known+unknown cache_write (E1 input=100, cache_read=900, cache_write=0;
//     E2 input=1000, cache_read=0, cache_write=NULL) computes the true upper-bound
//     rate 0.45 with cacheHitRatePartial=true and unknownEvents.cacheWrite=1 across
//     QuerySummary, QueryTokens (totals + dailyCacheHitRate), and QueryProjectSummary.
//   - Token-bearing unknown cache_read (E1 100/0/0 + E2 100/NULL/0) returns
//     cacheHitRate=nil, cacheHitRatePartial=false (numerator understated).
//   - Claude (100/900/0) mixed with calls-only token-less events (all buckets NULL)
//     excludes token-less events via null_token_events and returns normal unflagged
//     rate 0.90 with cacheHitRatePartial=false.
//   - All-known events emit cacheHitRatePartial=false and omit unknownEvents.
//   - Legacy-style all-NULL cache_write (100/50/NULL) computes true upper-bound
//     50/150 with cacheHitRatePartial=true.
func TestHubRollupSource_PartialCacheHitRateProbe(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	eventTS := dbNow.Add(-2 * time.Hour).Truncate(time.Minute)

	u1 := &store.User{ID: uuid.NewString(), Email: "partial-hr@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))

	projMixed := &store.Project{ID: uuid.NewString(), Name: "hr-mixed", Slug: "hr-mixed-" + uuid.NewString()[:8]}
	projKnown := &store.Project{ID: uuid.NewString(), Name: "hr-known", Slug: "hr-known-" + uuid.NewString()[:8]}
	projLegacy := &store.Project{ID: uuid.NewString(), Name: "hr-legacy", Slug: "hr-legacy-" + uuid.NewString()[:8]}
	projCRUnk := &store.Project{ID: uuid.NewString(), Name: "hr-cr-unk", Slug: "hr-cr-unk-" + uuid.NewString()[:8]}
	projCallsOnly := &store.Project{ID: uuid.NewString(), Name: "hr-calls-only", Slug: "hr-calls-only-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, projMixed))
	require.NoError(t, cs.CreateProject(ctx, projKnown))
	require.NoError(t, cs.CreateProject(ctx, projLegacy))
	require.NoError(t, cs.CreateProject(ctx, projCRUnk))
	require.NoError(t, cs.CreateProject(ctx, projCallsOnly))

	agMixed, runMixed := seedDashboardAgentAndRun(t, cs, projMixed.ID, u1.ID, "ag-mixed", "claude", string(state.PhaseRunning), eventTS.Add(-time.Hour))
	agKnown, runKnown := seedDashboardAgentAndRun(t, cs, projKnown.ID, u1.ID, "ag-known", "claude", string(state.PhaseRunning), eventTS.Add(-time.Hour))
	agLegacy, runLegacy := seedDashboardAgentAndRun(t, cs, projLegacy.ID, u1.ID, "ag-legacy", "claude", string(state.PhaseRunning), eventTS.Add(-time.Hour))
	agCRUnk, runCRUnk := seedDashboardAgentAndRun(t, cs, projCRUnk.ID, u1.ID, "ag-cr-unk", "claude", string(state.PhaseRunning), eventTS.Add(-time.Hour))
	agCallsOnly, runCallsOnly := seedDashboardAgentAndRun(t, cs, projCallsOnly.ID, u1.ID, "ag-calls-only", "claude", string(state.PhaseRunning), eventTS.Add(-time.Hour))

	// Mixed probe: E1 (100, 900, 0) + E2 (1000, 0, NULL) -> 900 / 2000 = 0.45, partial=true, unknownEvents.cacheWrite=1.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agMixed,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("hr-m1", runMixed, projMixed.ID, agMixed, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(100), i64ptr(50), i64ptr(900), i64ptr(0)),
			makeDashUsageEvent("hr-m2", runMixed, projMixed.ID, agMixed, &u1.ID, "claude", "claude-sonnet-4-6", 2, eventTS.Add(time.Minute), 1, i64ptr(1000), i64ptr(50), i64ptr(0), nil),
		},
	})
	require.NoError(t, err)

	// Fully known control: (100, 900, 0) -> 0.90, partial=false.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agKnown,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("hr-k1", runKnown, projKnown.ID, agKnown, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(100), i64ptr(50), i64ptr(900), i64ptr(0)),
		},
	})
	require.NoError(t, err)

	// Legacy-style all-NULL cache_write: (100, 50, NULL) -> 50/150, partial=true.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agLegacy,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("hr-l1", runLegacy, projLegacy.ID, agLegacy, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(100), i64ptr(20), i64ptr(50), nil),
		},
	})
	require.NoError(t, err)

	// Reviewer's D11b probe: E1 (100, 0, 0) + E2 (100, NULL, 0) -> token-bearing unknown cache_read=1 -> nil, partial=false.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agCRUnk,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("hr-cr1", runCRUnk, projCRUnk.ID, agCRUnk, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(100), i64ptr(20), i64ptr(0), i64ptr(0)),
			makeDashUsageEvent("hr-cr2", runCRUnk, projCRUnk.ID, agCRUnk, &u1.ID, "claude", "claude-sonnet-4-6", 2, eventTS.Add(time.Minute), 1, i64ptr(100), i64ptr(20), nil, i64ptr(0)),
		},
	})
	require.NoError(t, err)

	// Claude (100, 900, 0) mixed with calls-only token-less event (all buckets NULL) -> 0.90, partial=false.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agCallsOnly,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("hr-co1", runCallsOnly, projCallsOnly.ID, agCallsOnly, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(100), i64ptr(50), i64ptr(900), i64ptr(0)),
			makeDashUsageEvent("hr-co2", runCallsOnly, projCallsOnly.ID, agCallsOnly, &u1.ID, "antigravity", "gemini-2.5-pro", 2, eventTS.Add(time.Minute), 3, nil, nil, nil, nil),
		},
	})
	require.NoError(t, err)

	setHourlyCoverageStartForTest(t, client, dbNow.AddDate(0, 0, -10))
	src := NewHubRollupSource(cs)

	// 1. Mixed probe assertions
	sumMixed, err := src.QuerySummary(ctx, 7, WithProjectID(projMixed.ID))
	require.NoError(t, err)
	require.NotNil(t, sumMixed.CacheHitRate)
	assert.InDelta(t, 0.45, *sumMixed.CacheHitRate, 1e-9)
	assert.True(t, sumMixed.CacheHitRatePartial)
	require.NotNil(t, sumMixed.Tokens)
	assert.True(t, sumMixed.Tokens.CacheHitRatePartial)
	require.NotNil(t, sumMixed.Tokens.UnknownEvents)
	assert.Equal(t, int64(1), sumMixed.Tokens.UnknownEvents.CacheWrite)

	tokMixed, err := src.QueryTokens(ctx, 7, WithProjectID(projMixed.ID))
	require.NoError(t, err)
	require.NotNil(t, tokMixed.Totals)
	require.NotNil(t, tokMixed.Totals.CacheHitRate)
	assert.InDelta(t, 0.45, *tokMixed.Totals.CacheHitRate, 1e-9)
	assert.True(t, tokMixed.Totals.CacheHitRatePartial)
	require.NotNil(t, tokMixed.Totals.UnknownEvents)
	assert.Equal(t, int64(1), tokMixed.Totals.UnknownEvents.CacheWrite)
	require.Len(t, tokMixed.DailyCacheHitRate, 1)
	require.NotNil(t, tokMixed.DailyCacheHitRate[0].Value)
	assert.InDelta(t, 0.45, *tokMixed.DailyCacheHitRate[0].Value, 1e-9)
	assert.True(t, tokMixed.DailyCacheHitRate[0].Partial)

	psMixed, err := src.QueryProjectSummary(ctx, projMixed.ID)
	require.NoError(t, err)
	require.NotNil(t, psMixed.CacheHitRate)
	assert.InDelta(t, 0.45, *psMixed.CacheHitRate, 1e-9)
	assert.True(t, psMixed.CacheHitRatePartial)

	// 2. Fully-known control assertions
	sumKnown, err := src.QuerySummary(ctx, 7, WithProjectID(projKnown.ID))
	require.NoError(t, err)
	require.NotNil(t, sumKnown.CacheHitRate)
	assert.InDelta(t, 0.90, *sumKnown.CacheHitRate, 1e-9)
	assert.False(t, sumKnown.CacheHitRatePartial)
	require.NotNil(t, sumKnown.Tokens)
	assert.False(t, sumKnown.Tokens.CacheHitRatePartial)
	assert.Nil(t, sumKnown.Tokens.UnknownEvents)

	tokKnown, err := src.QueryTokens(ctx, 7, WithProjectID(projKnown.ID))
	require.NoError(t, err)
	require.Len(t, tokKnown.DailyCacheHitRate, 1)
	assert.False(t, tokKnown.DailyCacheHitRate[0].Partial)

	// 3. Legacy all-NULL cache_write assertions
	sumLegacy, err := src.QuerySummary(ctx, 7, WithProjectID(projLegacy.ID))
	require.NoError(t, err)
	require.NotNil(t, sumLegacy.CacheHitRate)
	assert.InDelta(t, 50.0/150.0, *sumLegacy.CacheHitRate, 1e-9)
	assert.True(t, sumLegacy.CacheHitRatePartial)
	assert.Nil(t, sumLegacy.Tokens.CacheWrite)

	// 4. Reviewer's D11b probe: (100,0,0)+(100,NULL,0) -> nil rate, partial=false everywhere
	sumCRUnk, err := src.QuerySummary(ctx, 7, WithProjectID(projCRUnk.ID))
	require.NoError(t, err)
	assert.Nil(t, sumCRUnk.CacheHitRate, "token-bearing unknown cache_read must return nil hit rate")
	assert.False(t, sumCRUnk.CacheHitRatePartial)
	require.NotNil(t, sumCRUnk.Tokens)
	assert.Nil(t, sumCRUnk.Tokens.CacheHitRate)
	assert.False(t, sumCRUnk.Tokens.CacheHitRatePartial)

	tokCRUnk, err := src.QueryTokens(ctx, 7, WithProjectID(projCRUnk.ID))
	require.NoError(t, err)
	assert.Nil(t, tokCRUnk.Totals.CacheHitRate)
	assert.False(t, tokCRUnk.Totals.CacheHitRatePartial)
	require.Len(t, tokCRUnk.DailyCacheHitRate, 1)
	assert.Nil(t, tokCRUnk.DailyCacheHitRate[0].Value)
	assert.False(t, tokCRUnk.DailyCacheHitRate[0].Partial)

	psCRUnk, err := src.QueryProjectSummary(ctx, projCRUnk.ID)
	require.NoError(t, err)
	assert.Nil(t, psCRUnk.CacheHitRate)
	assert.False(t, psCRUnk.CacheHitRatePartial)

	// 5. Claude + calls-only token-less mix -> normal unflagged rate 0.90, partial=false everywhere
	sumCO, err := src.QuerySummary(ctx, 7, WithProjectID(projCallsOnly.ID))
	require.NoError(t, err)
	require.NotNil(t, sumCO.CacheHitRate)
	assert.InDelta(t, 0.90, *sumCO.CacheHitRate, 1e-9)
	assert.False(t, sumCO.CacheHitRatePartial, "calls-only token-less events must not flag hit rate as partial")

	tokCO, err := src.QueryTokens(ctx, 7, WithProjectID(projCallsOnly.ID))
	require.NoError(t, err)
	require.NotNil(t, tokCO.Totals.CacheHitRate)
	assert.InDelta(t, 0.90, *tokCO.Totals.CacheHitRate, 1e-9)
	assert.False(t, tokCO.Totals.CacheHitRatePartial)
	require.Len(t, tokCO.DailyCacheHitRate, 1)
	require.NotNil(t, tokCO.DailyCacheHitRate[0].Value)
	assert.InDelta(t, 0.90, *tokCO.DailyCacheHitRate[0].Value, 1e-9)
	assert.False(t, tokCO.DailyCacheHitRate[0].Partial)

	psCO, err := src.QueryProjectSummary(ctx, projCallsOnly.ID)
	require.NoError(t, err)
	require.NotNil(t, psCO.CacheHitRate)
	assert.InDelta(t, 0.90, *psCO.CacheHitRate, 1e-9)
	assert.False(t, psCO.CacheHitRatePartial)
}

// TestHubRollupSource_D7bCoverageDayConsistencyDeterministic pins R2
// independently of wall-clock time of day: tz=UTC, period=2, hourly coverage
// marker at 00:00Z of the window's first UTC day. A control assertion proves
// the window resolves to hourly when consistent, then a daily-only discrepancy
// on that first (coverage) UTC day forces fallback to daily. Any mutant that
// skips the first overlapped UTC day fails at every hour of the day.
func TestHubRollupSource_D7bCoverageDayConsistencyDeterministic(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	todayDay := time.Date(dbNow.UTC().Year(), dbNow.UTC().Month(), dbNow.UTC().Day(), 0, 0, 0, 0, time.UTC)
	firstDay := todayDay.AddDate(0, 0, -1)

	u1 := &store.User{ID: uuid.NewString(), Email: "d7b-det@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	proj := &store.Project{ID: uuid.NewString(), Name: "d7b-det", Slug: "d7b-det-" + uuid.NewString()[:8]}
	require.NoError(t, cs.CreateProject(ctx, proj))
	ag, runID := seedDashboardAgentAndRun(t, cs, proj.ID, u1.ID, "ag-d7b", "claude", string(state.PhaseRunning), firstDay)

	// Seed a consistent hourly+daily event on firstDay (02:00Z).
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("d7b-1", runID, proj.ID, ag, &u1.ID, "claude", "claude-sonnet-4-6", 1, firstDay.Add(2*time.Hour), 2, i64ptr(100), i64ptr(50), i64ptr(0), i64ptr(0)),
		},
	})
	require.NoError(t, err)

	// Coverage starts at exact 00:00Z of the 2-day UTC window's first day.
	setHourlyCoverageStartForTest(t, client, firstDay)

	src := NewHubRollupSource(cs)

	// Control assertion: with no discrepancy, period=2 in UTC MUST resolve hourly.
	ctrl, err := src.QueryModelCalls(ctx, 2, WithProjectID(proj.ID))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGranularityHour, ctrl.Granularity, "control: consistent 2-day UTC window starting at coverage 00:00Z must resolve hourly")

	// Inject a daily-only discrepancy on firstDay (the coverage day itself).
	_, err = client.UsageRollupDaily.Create().
		SetBucketStart(firstDay).
		SetProjectID(proj.ID).
		SetBillingUserID(u1.ID).
		SetAgentID(ag).
		SetHarness("claude").
		SetModel("claude-haiku-4-5").
		SetUsageSource(store.UsageSourceNative).
		SetCalls(1).
		SetInput(10).
		SetOutput(5).
		SetEvents(1).
		SetInputKnownEvents(1).
		SetOutputKnownEvents(1).
		SetPresenceTrackedEvents(1).
		SetFirstEventTs(firstDay.Add(3 * time.Hour)).
		SetLastEventTs(firstDay.Add(3 * time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	// Now D7b cross-check over [firstDay, todayDay] MUST detect the mismatch on firstDay and fall back to daily.
	after, err := src.QueryModelCalls(ctx, 2, WithProjectID(proj.ID))
	require.NoError(t, err)
	assert.Equal(t, store.UsageGranularityDay, after.Granularity, "D7b: discrepancy on first (coverage) UTC day must force daily fallback at any hour")
	assert.Equal(t, "UTC", after.TimeZone)
}

// TestHubRollupSource_PrePresenceLegacyMixedHitRate pins the conservative
// trackedNullLB + hasUntracked D11b formula when pre-P2.1a daily rows
// (presence_tracked_events == 0, null_token_events > 0) share a daily window
// with presence-tracked rows:
//   - Probe A: legacy (events=5, null=5, presence=0) + tracked (2 events, 1 with
//     cache_read NULL) -> nil hit rate (totals + daily point).
//   - Probe B: legacy (events=5, null=5, presence=0) + tracked (2 events, 1 with
//     cache_write NULL) -> partial=true hit rate (totals + daily point).
//   - Pure legacy with positive sums (events=5, null=0, presence=0) -> partial=true.
//   - Pure tracked fully known -> partial=false (unchanged).
func TestHubRollupSource_PrePresenceLegacyMixedHitRate(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	todayDay := time.Date(dbNow.UTC().Year(), dbNow.UTC().Month(), dbNow.UTC().Day(), 0, 0, 0, 0, time.UTC)
	eventTS := todayDay.Add(time.Hour)
	if eventTS.After(dbNow) {
		eventTS = dbNow.Add(-time.Minute)
	}

	u1 := &store.User{ID: uuid.NewString(), Email: "legacy-mix@example.com", DisplayName: "U1", Role: "member", Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))

	projProbeA := &store.Project{ID: uuid.NewString(), Name: "leg-a", Slug: "leg-a-" + uuid.NewString()[:8]}
	projProbeB := &store.Project{ID: uuid.NewString(), Name: "leg-b", Slug: "leg-b-" + uuid.NewString()[:8]}
	projPureLeg := &store.Project{ID: uuid.NewString(), Name: "leg-pure", Slug: "leg-pure-" + uuid.NewString()[:8]}
	projPureTrk := &store.Project{ID: uuid.NewString(), Name: "trk-pure", Slug: "trk-pure-" + uuid.NewString()[:8]}
	for _, p := range []*store.Project{projProbeA, projProbeB, projPureLeg, projPureTrk} {
		require.NoError(t, cs.CreateProject(ctx, p))
	}

	agA, runA := seedDashboardAgentAndRun(t, cs, projProbeA.ID, u1.ID, "ag-la", "claude", string(state.PhaseRunning), todayDay)
	agB, runB := seedDashboardAgentAndRun(t, cs, projProbeB.ID, u1.ID, "ag-lb", "claude", string(state.PhaseRunning), todayDay)
	agTrk, runTrk := seedDashboardAgentAndRun(t, cs, projPureTrk.ID, u1.ID, "ag-lt", "claude", string(state.PhaseRunning), todayDay)

	seedRawPrePresence := func(projectID string, events, nullEvents, in, cr, cw int64) {
		_, sErr := client.UsageRollupDaily.Create().
			SetBucketStart(todayDay).
			SetProjectID(projectID).
			SetBillingUserID(u1.ID).
			SetAgentID(uuid.NewString()).
			SetHarness("antigravity").
			SetModel("legacy-model").
			SetUsageSource(store.UsageSourceHooks).
			SetCalls(events).
			SetInput(in).
			SetOutput(0).
			SetCacheRead(cr).
			SetCacheWrite(cw).
			SetEvents(events).
			SetNullTokenEvents(nullEvents).
			SetPresenceTrackedEvents(0).
			SetFirstEventTs(eventTS).
			SetLastEventTs(eventTS).
			Save(ctx)
		require.NoError(t, sErr)
	}

	// Probe A: pre-presence legacy row (events=5, null=5, presence=0) + tracked (1 known + 1 cache_read NULL)
	seedRawPrePresence(projProbeA.ID, 5, 5, 0, 0, 0)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agA,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("la-1", runA, projProbeA.ID, agA, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(500), i64ptr(50), i64ptr(400), i64ptr(5)),
			makeDashUsageEvent("la-2", runA, projProbeA.ID, agA, &u1.ID, "claude", "claude-sonnet-4-6", 2, eventTS.Add(time.Second), 1, i64ptr(500), i64ptr(50), nil, i64ptr(5)),
		},
	})
	require.NoError(t, err)

	// Probe B: pre-presence legacy row (events=5, null=5, presence=0) + tracked (1 known + 1 cache_write NULL)
	seedRawPrePresence(projProbeB.ID, 5, 5, 0, 0, 0)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agB,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("lb-1", runB, projProbeB.ID, agB, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(500), i64ptr(50), i64ptr(400), i64ptr(5)),
			makeDashUsageEvent("lb-2", runB, projProbeB.ID, agB, &u1.ID, "claude", "claude-sonnet-4-6", 2, eventTS.Add(time.Second), 1, i64ptr(500), i64ptr(50), i64ptr(400), nil),
		},
	})
	require.NoError(t, err)

	// Pure legacy row with positive sums (events=5, null=0, presence=0)
	seedRawPrePresence(projPureLeg.ID, 5, 0, 500, 200, 100)

	// Pure tracked fully known
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agTrk,
		Events: []*store.UsageEvent{
			makeDashUsageEvent("lt-1", runTrk, projPureTrk.ID, agTrk, &u1.ID, "claude", "claude-sonnet-4-6", 1, eventTS, 1, i64ptr(100), i64ptr(50), i64ptr(900), i64ptr(0)),
		},
	})
	require.NoError(t, err)

	// Remove hourly coverage marker so all queries read daily rollups (where pre-presence rows live).
	_, err = client.HubSetting.Delete().Where(hubsetting.SectionEQ(store.UsageRollupHourlyCoverageSection)).Exec(ctx)
	require.NoError(t, err)

	src := NewHubRollupSource(cs)

	// Probe A -> nil hit rate on both summary totals and dailyCacheHitRate point
	sumA, err := src.QuerySummary(ctx, 7, WithProjectID(projProbeA.ID))
	require.NoError(t, err)
	assert.Nil(t, sumA.CacheHitRate, "Probe A: legacy null_token_events must not mask tracked unknown cache_read")
	assert.False(t, sumA.CacheHitRatePartial)
	tokA, err := src.QueryTokens(ctx, 7, WithProjectID(projProbeA.ID))
	require.NoError(t, err)
	assert.Nil(t, tokA.Totals.CacheHitRate)
	require.Len(t, tokA.DailyCacheHitRate, 1)
	assert.Nil(t, tokA.DailyCacheHitRate[0].Value)
	assert.False(t, tokA.DailyCacheHitRate[0].Partial)

	// Probe B -> non-nil rate flagged partial=true on both summary totals and dailyCacheHitRate point
	sumB, err := src.QuerySummary(ctx, 7, WithProjectID(projProbeB.ID))
	require.NoError(t, err)
	require.NotNil(t, sumB.CacheHitRate)
	assert.InDelta(t, 800.0/1805.0, *sumB.CacheHitRate, 1e-9)
	assert.True(t, sumB.CacheHitRatePartial, "Probe B: legacy null_token_events must not mask tracked unknown cache_write")
	tokB, err := src.QueryTokens(ctx, 7, WithProjectID(projProbeB.ID))
	require.NoError(t, err)
	require.NotNil(t, tokB.Totals.CacheHitRate)
	assert.True(t, tokB.Totals.CacheHitRatePartial)
	require.Len(t, tokB.DailyCacheHitRate, 1)
	require.NotNil(t, tokB.DailyCacheHitRate[0].Value)
	assert.True(t, tokB.DailyCacheHitRate[0].Partial)

	// Pure legacy with positive sums -> rate=200/800, partial=true (untracked legacy portion exists)
	sumLeg, err := src.QuerySummary(ctx, 7, WithProjectID(projPureLeg.ID))
	require.NoError(t, err)
	require.NotNil(t, sumLeg.CacheHitRate)
	assert.InDelta(t, 200.0/800.0, *sumLeg.CacheHitRate, 1e-9)
	assert.True(t, sumLeg.CacheHitRatePartial)

	// Pure tracked fully known -> rate=0.90, partial=false
	sumTrk, err := src.QuerySummary(ctx, 7, WithProjectID(projPureTrk.ID))
	require.NoError(t, err)
	require.NotNil(t, sumTrk.CacheHitRate)
	assert.InDelta(t, 0.90, *sumTrk.CacheHitRate, 1e-9)
	assert.False(t, sumTrk.CacheHitRatePartial)
}
