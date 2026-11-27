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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagegrouprollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func i64Ptr(v int64) *int64 { return &v }

func TestUsageGroupRollupStore_IngestAdditiveAndBothScopes(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	runID := "run-grp-" + uuid.NewString()
	projID := uuid.NewString()
	agentID := uuid.NewString()
	userID := uuid.NewString()
	g1 := uuid.NewString()
	g2 := uuid.NewString()

	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		AgentID:       agentID,
		ProjectID:     projID,
		BillingUserID: &userID,
		Harness:       "claude",
		CreatedAt:     time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	day1 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)

	ev1 := &store.UsageEvent{
		HubID:           "hub-1",
		EventID:         "ev-1",
		RunID:           runID,
		ProjectID:       projID,
		AgentID:         agentID,
		BillingUserID:   &userID,
		Harness:         "claude",
		Model:           "claude-sonnet-4-6",
		ModelSource:     telemetrycontract.ModelSourceEvent,
		UsageSource:     telemetrycontract.UsageSourceNative,
		Status:          telemetrycontract.StatusSuccess,
		Calls:           1,
		Input:           i64Ptr(100),
		Output:          i64Ptr(50),
		CacheRead:       i64Ptr(25),
		CacheWrite:      i64Ptr(10),
		TokenStatus:     telemetrycontract.TokenStatusReported,
		EventTS:         day1,
		ProducerEpoch:   "ep-1",
		ProducerSeq:     1,
		ChargedGroupIDs: []string{g1, g2, g1}, // duplicate g1 must dedupe
	}
	ev2 := &store.UsageEvent{
		HubID:           "hub-1",
		EventID:         "ev-2",
		RunID:           runID,
		ProjectID:       projID,
		AgentID:         agentID,
		BillingUserID:   &userID,
		Harness:         "claude",
		Model:           "claude-sonnet-4-6",
		ModelSource:     telemetrycontract.ModelSourceEvent,
		UsageSource:     telemetrycontract.UsageSourceNative,
		Status:          telemetrycontract.StatusSuccess,
		Calls:           2,
		Input:           i64Ptr(200),
		Output:          i64Ptr(100),
		TokenStatus:     telemetrycontract.TokenStatusReported,
		EventTS:         day2,
		ProducerEpoch:   "ep-1",
		ProducerSeq:     2,
		ChargedGroupIDs: []string{g1},
	}

	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1, ev2},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Accepted)

	// Re-ingesting the same batch must be a duplicate no-op (no double-count).
	resDup, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1, ev2},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, resDup.Accepted)
	assert.Equal(t, 2, resDup.Duplicates)

	// Verify charged_group_ids persisted on usage_events.
	storedEv1, err := client.UsageEvent.Query().Where(usageevent.EventIDEQ("ev-1")).Only(ctx)
	require.NoError(t, err)
	assert.Contains(t, storedEv1.ChargedGroupIds, g1)
	assert.Contains(t, storedEv1.ChargedGroupIds, g2)

	nov01 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dec01 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	// g1 has 185 (day1) + 300 (day2) = 485 in both system ("") and project (projID) scopes.
	g1Sys, err := cs.SumUsageGroupRollupTokens(ctx, g1, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(485), g1Sys)

	g1Proj, err := cs.SumUsageGroupRollupTokens(ctx, g1, projID, nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(485), g1Proj)

	// Unrelated project scope has 0.
	g1OtherProj, err := cs.SumUsageGroupRollupTokens(ctx, g1, "other-proj", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(0), g1OtherProj)

	// g2 has 185 (day1 only) in both system and project scopes.
	g2Sys, err := cs.SumUsageGroupRollupTokens(ctx, g2, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(185), g2Sys)

	// Verify detailed counters on (day1, g1, "", "ingest").
	row, err := client.UsageGroupRollupDaily.Query().Where(
		usagegrouprollupdaily.DayUtcEQ(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)),
		usagegrouprollupdaily.GroupIDEQ(g1),
		usagegrouprollupdaily.ProjectScopeIDEQ(""),
		usagegrouprollupdaily.OriginEQ(store.UsageGroupRollupOriginIngest),
	).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(185), row.TokensSummable)
	assert.Equal(t, int64(100), row.Input)
	assert.Equal(t, int64(50), row.Output)
	assert.Equal(t, int64(25), row.CacheRead)
	assert.Equal(t, int64(10), row.CacheWrite)
	assert.Equal(t, int64(1), row.Calls)
	assert.Equal(t, int64(1), row.Events)
}

func TestUsageGroupRollupStore_AnomalousExcluded(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	runID := "run-anom-" + uuid.NewString()
	projID := uuid.NewString()
	agentID := uuid.NewString()
	userID := uuid.NewString()
	g1 := uuid.NewString()

	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		AgentID:       agentID,
		ProjectID:     projID,
		BillingUserID: &userID,
		Harness:       "claude",
		CreatedAt:     time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	day := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID:           "hub-1",
				EventID:         "ev-anom",
				RunID:           runID,
				ProjectID:       projID,
				AgentID:         agentID,
				BillingUserID:   &userID,
				Harness:         "claude",
				Model:           "claude-sonnet-4-6",
				ModelSource:     telemetrycontract.ModelSourceEvent,
				UsageSource:     telemetrycontract.UsageSourceNative,
				Status:          telemetrycontract.StatusSuccess,
				Calls:           1,
				Input:           i64Ptr(60_000_000),
				Output:          i64Ptr(1_000_000),
				TokenStatus:     telemetrycontract.TokenStatusReported,
				EventTS:         day,
				ProducerEpoch:   "ep-1",
				ProducerSeq:     1,
				Anomalous:       true,
				ChargedGroupIDs: []string{g1},
			},
		},
	})
	require.NoError(t, err)

	sum, err := cs.SumUsageGroupRollupTokens(ctx, g1, "",
		time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC),
	)
	require.NoError(t, err)
	assert.Equal(t, int64(0), sum, "anomalous event tokens must be excluded from group rollups")

	row, err := client.UsageGroupRollupDaily.Query().Where(
		usagegrouprollupdaily.GroupIDEQ(g1),
		usagegrouprollupdaily.ProjectScopeIDEQ(""),
	).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), row.TokensSummable)
	assert.Equal(t, int64(0), row.Input)
	assert.Equal(t, int64(1), row.Calls)
	assert.Equal(t, int64(1), row.Events)
}

func TestUsageGroupRollupStore_SumBothOriginsAndSaturation(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	g1 := uuid.NewString()
	gSat := uuid.NewString()
	d1 := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)

	_, err := client.UsageGroupRollupDaily.Create().
		SetDayUtc(d1).SetGroupID(g1).SetProjectScopeID("").
		SetOrigin(store.UsageGroupRollupOriginSeed).SetTokensSummable(1000).Save(ctx)
	require.NoError(t, err)
	_, err = client.UsageGroupRollupDaily.Create().
		SetDayUtc(d1).SetGroupID(g1).SetProjectScopeID("").
		SetOrigin(store.UsageGroupRollupOriginIngest).SetTokensSummable(400).Save(ctx)
	require.NoError(t, err)
	_, err = client.UsageGroupRollupDaily.Create().
		SetDayUtc(d2).SetGroupID(g1).SetProjectScopeID("").
		SetOrigin(store.UsageGroupRollupOriginIngest).SetTokensSummable(250).Save(ctx)
	require.NoError(t, err)
	// Different project scope must not leak into system scope.
	_, err = client.UsageGroupRollupDaily.Create().
		SetDayUtc(d1).SetGroupID(g1).SetProjectScopeID("proj-x").
		SetOrigin(store.UsageGroupRollupOriginIngest).SetTokensSummable(9999).Save(ctx)
	require.NoError(t, err)

	sum, err := cs.SumUsageGroupRollupTokens(ctx, g1, "", d1, d2.AddDate(0, 0, 1))
	require.NoError(t, err)
	assert.Equal(t, int64(1650), sum)

	// Saturation check across origins.
	_, err = client.UsageGroupRollupDaily.Create().
		SetDayUtc(d1).SetGroupID(gSat).SetProjectScopeID("").
		SetOrigin(store.UsageGroupRollupOriginSeed).SetTokensSummable(math.MaxInt64 - 10).Save(ctx)
	require.NoError(t, err)
	_, err = client.UsageGroupRollupDaily.Create().
		SetDayUtc(d1).SetGroupID(gSat).SetProjectScopeID("").
		SetOrigin(store.UsageGroupRollupOriginIngest).SetTokensSummable(100).Save(ctx)
	require.NoError(t, err)

	satSum, err := cs.SumUsageGroupRollupTokens(ctx, gSat, "", d1, d2)
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), satSum)
}

func TestUsageGroupRollupStore_SweepUsageGroupRollupsDailyBatch(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	g1 := uuid.NewString()
	old1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	old2 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	keep := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, d := range []time.Time{old1, old2, keep} {
		_, err := client.UsageGroupRollupDaily.Create().
			SetDayUtc(d).SetGroupID(g1).SetProjectScopeID("").
			SetOrigin(store.UsageGroupRollupOriginIngest).SetTokensSummable(100).Save(ctx)
		require.NoError(t, err)
	}

	n, err := cs.SweepUsageGroupRollupsDailyBatch(ctx, cutoff, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	n, err = cs.SweepUsageGroupRollupsDailyBatch(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	n, err = cs.SweepUsageGroupRollupsDailyBatch(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	remaining, err := client.UsageGroupRollupDaily.Query().Where(usagegrouprollupdaily.GroupIDEQ(g1)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, remaining)
}
