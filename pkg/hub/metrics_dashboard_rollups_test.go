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
	// Normal ratio: 36000 / (12000 + 36000 + 2000) = 0.72
	r := computeCacheHitRate(i64ptr(12000), i64ptr(36000), i64ptr(2000))
	require.NotNil(t, r)
	assert.InDelta(t, 0.72, *r, 1e-9)

	// Zero cache_read with positive input -> 0.0 (known zero, not nil)
	rZero := computeCacheHitRate(i64ptr(1000), i64ptr(0), i64ptr(0))
	require.NotNil(t, rZero)
	assert.Equal(t, 0.0, *rZero)

	// Zero denominator -> nil (never divide by zero / NaN)
	assert.Nil(t, computeCacheHitRate(i64ptr(0), i64ptr(0), i64ptr(0)))

	// Any nil component (unknown) -> nil
	assert.Nil(t, computeCacheHitRate(nil, i64ptr(100), i64ptr(0)))
	assert.Nil(t, computeCacheHitRate(i64ptr(100), nil, i64ptr(0)))
	assert.Nil(t, computeCacheHitRate(i64ptr(100), i64ptr(50), nil))
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
	require.NotNil(t, toksByUser.DailyCacheHitRate[0].Value)
	// Combined day totals: input=300, cacheRead=300, cacheWrite=100 -> 300 / 700
	assert.InDelta(t, 300.0/700.0, *toksByUser.DailyCacheHitRate[0].Value, 1e-9)

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
	assert.Equal(t, "Last 24 hours", ps.PeriodLabel)
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

	// Case 2: experiment OFF + CM configured -> cloud_monitoring; groupBy=user -> 400.
	srv.mu.Lock()
	srv.metricsDashboard = cmSvc
	srv.mu.Unlock()

	rec = doGET("/api/v1/metrics/?view=summary&period=7")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sumCM DashboardSummary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sumCM))
	assert.Equal(t, MetricsSourceCloudMonitoring, sumCM.Source)

	rec = doGET("/api/v1/metrics/?view=tokens&period=7&groupBy=user")
	assert.Equal(t, http.StatusBadRequest, rec.Code)

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
