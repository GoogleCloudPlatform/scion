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
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerolluphourly"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagescopecounterdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// The concurrency tests below exercise real Postgres row locks, the shared
// advisory lock hashtext('usage_rollup:' || agent_id), concurrent
// ON CONFLICT ... DO NOTHING RETURNING, and the meter_health first-insert-race
// merge path (row.ID != newID). They run in CI's T1 entadapter Postgres job
// (make test-store-postgres-entadapter, which builds with -tags integration
// and sets SCION_TEST_POSTGRES_URL):
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/postgres?sslmode=disable' \
//	  go test -tags integration -run 'TestUsageIngest_.*_Postgres|TestMeterHealth_.*_Postgres' ./pkg/store/entadapter/

func TestUsageIngest_ConcurrentOverlappingBatches_Postgres(t *testing.T) {
	requirePostgres(t)
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.Equal(t, dialect.Postgres, cs.client.Driver().Dialect())

	projectID := createTestProject(t, cs, "ingest-pg-concurrent")
	userID := createTestUser(t, cs, "pg-concurrent-user@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Slug:      "pg-conc-agent",
		Name:      "pg-conc-agent",
		ProjectID: projectID,
		OwnerID:   userID,
		CreatedBy: userID,
		Phase:     string(state.PhaseRunning),
	}))
	_, err := cs.SetAgentRunID(ctx, agentID, runID, nil)
	require.NoError(t, err)
	ts := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	_, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		Generation:    1,
		HubID:         "hub-pg",
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: &userID,
		Harness:       "claude",
		CreatedAt:     ts.Add(-time.Minute),
	})
	require.NoError(t, err)

	const totalEvents = 30
	allEvents := make([]*store.UsageEvent, totalEvents)
	for i := 0; i < totalEvents; i++ {
		allEvents[i] = makeStoreUsageEvent(
			"hub-pg", fmt.Sprintf("pg-conc-evt-%02d", i+1), runID, projectID, agentID, &userID,
			int64(i+1), ts.Add(time.Duration(i)*time.Second), 10, 5, 2, 3,
		)
	}
	// Populate JSONB array fields on event 1 so we can assert jsonb_typeof = 'array' below (O4).
	allEvents[0].MalformedFields = []string{"cache_read_tokens"}
	allEvents[0].ChargedGroupIDs = []string{"group-1"}

	const workers = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	accepted := make(chan int, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			// Deliberately pass overlapping windows in reversed/shifted orders
			// to verify canonical sorting prevents Postgres deadlocks.
			batch := make([]*store.UsageEvent, 0, 15)
			seqs := make([]int64, 0, 15)
			if workerIdx%2 == 0 {
				for j := 0; j < 15; j++ {
					ev := allEvents[(workerIdx*3+j)%totalEvents]
					batch = append(batch, ev)
					seqs = append(seqs, ev.ProducerSeq)
				}
			} else {
				for j := 14; j >= 0; j-- {
					ev := allEvents[(workerIdx*3+j)%totalEvents]
					batch = append(batch, ev)
					seqs = append(seqs, ev.ProducerSeq)
				}
			}
			<-start
			res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
				AgentID: agentID,
				Events:  batch,
				MeterHealth: &store.UsageMeterHealthUpdate{
					AgentID:       agentID,
					RunID:         runID,
					ProducerEpoch: "ep-pg",
					LastReceiptAt: ts.Add(time.Minute),
					ProducerSeq:   int64(totalEvents),
					ReceivedSeqs:  seqs,
				},
			})
			if err != nil {
				errs <- err
				return
			}
			accepted <- res.Accepted
		}(w)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(accepted)

	for err := range errs {
		require.NoError(t, err)
	}
	sumAccepted := 0
	for n := range accepted {
		sumAccepted += n
	}
	assert.Equal(t, totalEvents, sumAccepted, "each unique event must be accepted exactly once under Postgres concurrency")

	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.BucketStartEQ(dayUTC), usagerollupdaily.AgentIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(totalEvents), rollup.Events)
	assert.Equal(t, int64(totalEvents*10), rollup.Input)
	assert.Equal(t, int64(totalEvents*5), rollup.Output)
	assert.Equal(t, int64(totalEvents*2), rollup.CacheRead)
	assert.Equal(t, int64(totalEvents*3), rollup.CacheWrite)
	assert.Equal(t, int64(totalEvents), rollup.InputKnownEvents)
	assert.Equal(t, int64(totalEvents), rollup.PresenceTrackedEvents)

	// Hourly rollups and hourly coverage marker must be exact under concurrent Postgres ingest.
	hourUTC := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)
	hRollup, err := client.UsageRollupHourly.Query().
		Where(
			usagerolluphourly.BucketStartEQ(hourUTC),
			usagerolluphourly.AgentIDEQ(agentID),
		).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(totalEvents), hRollup.Events)
	assert.Equal(t, int64(totalEvents*10), hRollup.Input)
	assert.Equal(t, int64(totalEvents*5), hRollup.Output)
	assert.Equal(t, int64(totalEvents*2), hRollup.CacheRead)
	assert.Equal(t, int64(totalEvents*3), hRollup.CacheWrite)
	assert.Equal(t, int64(totalEvents), hRollup.InputKnownEvents)
	assert.Equal(t, int64(totalEvents), hRollup.PresenceTrackedEvents)

	covStart, hasCov, err := cs.GetUsageHourlyCoverageStart(ctx)
	require.NoError(t, err)
	require.True(t, hasCov)
	dbNow, err := cs.DBNow(ctx)
	require.NoError(t, err)
	assert.False(t, covStart.Before(utcHourStart(dbNow)), "coverage marker %s must be >= current UTC hour %s", covStart, utcHourStart(dbNow))

	for _, tc := range []struct {
		kind string
		id   string
	}{
		{store.UsageScopeKindProject, projectID},
		{store.UsageScopeKindUser, userID},
		{store.UsageScopeKindUserProject, userID + ":" + projectID},
	} {
		sc, err := client.UsageScopeCounterDaily.Query().
			Where(
				usagescopecounterdaily.ScopeKindEQ(tc.kind),
				usagescopecounterdaily.ScopeIDEQ(tc.id),
				usagescopecounterdaily.DayUtcEQ(dayUTC),
			).Only(ctx)
		require.NoError(t, err, "scope %s", tc.kind)
		assert.Equal(t, int64(totalEvents*20), sc.TokensSummable, "scope %s", tc.kind)
		assert.Equal(t, int64(totalEvents), sc.Calls, "scope %s", tc.kind)
	}

	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(totalEvents), mhs[0].LastContiguousSeq)
	assert.Equal(t, int64(totalEvents), mhs[0].MaxSeqSeen)
	assert.Empty(t, mhs[0].MissingRanges)

	// O4: Verify JSONB array columns stored with jsonb_typeof = 'array' on Postgres.
	rows := &entsql.Rows{}
	require.NoError(t, cs.client.Driver().Query(
		ctx,
		"SELECT jsonb_typeof(malformed_fields), jsonb_typeof(charged_group_ids) FROM usage_events WHERE hub_id = $1 AND event_id = $2",
		[]any{"hub-pg", "pg-conc-evt-01"},
		rows,
	))
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var malformedType, chargedType string
	require.NoError(t, rows.Scan(&malformedType, &chargedType))
	assert.Equal(t, "array", malformedType)
	assert.Equal(t, "array", chargedType)

	// Exercise QueryUsageRollups (hourly, day-from-hourly date_trunc, limit/truncated) and GetUsageScopeCoverage on Postgres.
	qRes, err := cs.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: store.UsageGranularityHour,
		Since:       hourUTC,
		Until:       hourUTC,
		ProjectID:   projectID,
		GroupBy:     []string{store.UsageGroupByDay, store.UsageGroupByModel},
		Limit:       10,
	})
	require.NoError(t, err)
	assert.False(t, qRes.Truncated)
	require.Len(t, qRes.Groups, 1)
	require.NotNil(t, qRes.Groups[0].BucketStart)
	assert.Equal(t, dayUTC, *qRes.Groups[0].BucketStart)
	assert.Equal(t, int64(totalEvents*10), qRes.Totals.Input)
	assert.Equal(t, int64(totalEvents), qRes.Totals.PresenceTrackedEvents)

	cov, err := cs.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:  projectID,
		Since:      ts.Add(-time.Hour),
		Until:      ts.Add(2 * time.Minute),
		StaleAfter: 3 * time.Minute,
		DBNow:      ts.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	require.NotNil(t, cov)
	require.Len(t, cov.Buckets, 1, "Postgres coverage query must match the running agent via CAST(a.id AS TEXT)")
	assert.Equal(t, 1, cov.Buckets[0].AgentCount)
	assert.True(t, cov.Buckets[0].IsRunning)
	assert.True(t, cov.Buckets[0].HasMeterHealth)
	assert.Equal(t, store.MeterHealthFinalStateOpen, cov.Buckets[0].LatestFinalState)
	assert.False(t, cov.Buckets[0].IsReceiptStale)
	assert.True(t, cov.Buckets[0].HasUsageData)
	require.NotNil(t, cov.LastReceiptAt)
	assert.Equal(t, ts.Add(time.Minute), *cov.LastReceiptAt)
}

func TestMeterHealth_FirstInsertRace_Postgres(t *testing.T) {
	requirePostgres(t)
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.Equal(t, dialect.Postgres, cs.client.Driver().Dialect())

	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	epoch := "ep-pg-race"
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	const workers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
			<-start
			_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
				AgentID: agentID,
				MeterHealth: &store.UsageMeterHealthUpdate{
					AgentID:       agentID,
					RunID:         runID,
					ProducerEpoch: epoch,
					LastReceiptAt: t0.Add(time.Duration(seq) * time.Second),
					ProducerSeq:   seq,
					ReceivedSeqs:  []int64{seq},
					FinalState:    store.MeterHealthFinalStateOpen,
				},
			})
			errs <- err
		}(int64(i))
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(workers), mhs[0].LastContiguousSeq, "highest contiguous seq must be preserved across Postgres first-insert race")
	assert.Equal(t, int64(workers), mhs[0].MaxSeqSeen)
	assert.Empty(t, mhs[0].MissingRanges)
}

func TestUsageIngest_PerRowDBRejection_Postgres(t *testing.T) {
	requirePostgres(t)
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.Equal(t, dialect.Postgres, cs.client.Driver().Dialect())

	projectID := createTestProject(t, cs, "pg-per-row-reject")
	userID := createTestUser(t, cs, "pg-per-row-reject@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 22, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	good1 := makeStoreUsageEvent("hub-pg", "pg-good-1", runID, projectID, agentID, &userID, 1, ts, 100, 40, 10, 5)
	// Postgres rejects NUL bytes in TEXT columns with SQLSTATE 22021.
	badNUL := makeStoreUsageEvent("hub-pg", "pg-bad-nul", runID, projectID, agentID, &userID, 2, ts.Add(time.Second), 500, 500, 0, 0)
	badNUL.Model = "claude\x00poison"
	good3 := makeStoreUsageEvent("hub-pg", "pg-good-3", runID, projectID, agentID, &userID, 3, ts.Add(2*time.Second), 200, 60, 20, 10)

	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{good1, badNUL, good3},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(3 * time.Second),
			ProducerSeq:   3,
			ReceivedSeqs:  []int64{1, 2, 3},
		},
	})
	require.NoError(t, err, "Postgres NUL byte in one row must not abort the batch")
	assert.Equal(t, 2, res.Accepted)
	assert.Equal(t, 0, res.Duplicates)
	require.Len(t, res.Rejected, 1)
	assert.Equal(t, "pg-bad-nul", res.Rejected[0].EventID)

	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.BucketStartEQ(dayUTC), usagerollupdaily.AgentIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), rollup.Events)
	assert.Equal(t, int64(300), rollup.Input)
	assert.Equal(t, int64(100), rollup.Output)

	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(1), mhs[0].LastContiguousSeq)
	assert.Equal(t, int64(3), mhs[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 2, End: 2}}, mhs[0].MissingRanges)
}
