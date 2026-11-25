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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
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
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	const totalEvents = 30
	allEvents := make([]*store.UsageEvent, totalEvents)
	for i := 0; i < totalEvents; i++ {
		allEvents[i] = makeStoreUsageEvent(
			"hub-pg", fmt.Sprintf("pg-conc-evt-%02d", i+1), runID, projectID, agentID, &userID,
			int64(i+1), ts.Add(time.Duration(i)*time.Second), 10, 5, 2, 3,
		)
	}

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
			errs <- cs.UpsertMeterHealth(ctx, &store.MeterHealth{
				AgentID:           agentID,
				RunID:             runID,
				ProducerEpoch:     epoch,
				LastReceiptAt:     t0.Add(time.Duration(seq) * time.Second),
				LastContiguousSeq: seq,
				MaxSeqSeen:        seq,
				FinalState:        store.MeterHealthFinalStateOpen,
			})
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
}
