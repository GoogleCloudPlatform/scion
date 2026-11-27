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
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagerollupdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagescopecounterdaily"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func i64p(v int64) *int64 { return &v }

func seedRunForIngestTest(t *testing.T, cs *CompositeStore, projectID, agentID, runID string, billingUserID *string) {
	t.Helper()
	created, err := cs.EnsureRunAttribution(context.Background(), &store.RunAttribution{
		RunID:         runID,
		Generation:    1,
		HubID:         "hub-test",
		ProjectID:     projectID,
		AgentID:       agentID,
		AgentSlug:     "worker-1",
		BrokerID:      "broker-1",
		BillingUserID: billingUserID,
		Harness:       "claude",
		HarnessConfig: "claude",
		Template:      "default",
		CreatedAt:     time.Now().UTC().Truncate(time.Microsecond),
	})
	require.NoError(t, err)
	require.True(t, created)
}

func makeStoreUsageEvent(hubID, eventID, runID, projectID, agentID string, billingUserID *string, seq int64, eventTS time.Time, in, out, cr, cw int64) *store.UsageEvent {
	return &store.UsageEvent{
		HubID:          hubID,
		EventID:        eventID,
		RunID:          runID,
		ProjectID:      projectID,
		AgentID:        agentID,
		BillingUserID:  billingUserID,
		BrokerID:       "broker-1",
		Harness:        "claude",
		Model:          "claude-sonnet-4-6",
		ModelSource:    telemetrycontract.ModelSourceEvent,
		UsageSource:    store.UsageSourceNative,
		SourceKind:     "claude.api_request",
		SourceIdentity: telemetrycontract.SourceIdentityRequestID,
		Status:         telemetrycontract.StatusSuccess,
		Calls:          1,
		Input:          i64p(in),
		Output:         i64p(out),
		CacheRead:      i64p(cr),
		CacheWrite:     i64p(cw),
		TokenStatus:    telemetrycontract.TokenStatusReported,
		EventTS:        eventTS,
		ReceivedAt:     eventTS,
		ProducerEpoch:  "ep-1",
		ProducerSeq:    seq,
	}
}

func TestUsageIngest_IdempotentAndLostAckReplay(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ingest-idempotent")
	userID := createTestUser(t, cs, "ingest-user@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 14, 30, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	ev1 := makeStoreUsageEvent("hub-test", "evt-1", runID, projectID, agentID, &userID, 1, ts, 100, 50, 20, 10)
	ev2 := makeStoreUsageEvent("hub-test", "evt-2", runID, projectID, agentID, &userID, 2, ts.Add(time.Second), 200, 80, 0, 0)

	// 1. Initial batch with an intra-batch duplicate of evt-1.
	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1, ev1, ev2},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(2 * time.Second),
			ProducerSeq:   2,
			ReceivedSeqs:  []int64{1, 2},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Accepted)
	assert.Equal(t, 1, res.Duplicates)

	// 2. Duplicate replay (exact same batch — simulates lost ACK).
	res2, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1, ev2},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res2.Accepted)
	assert.Equal(t, 2, res2.Duplicates)

	// 3. Partial overlap replay (evt-2 already present, evt-3 is new).
	ev3 := makeStoreUsageEvent("hub-test", "evt-3", runID, projectID, agentID, &userID, 3, ts.Add(5*time.Second), 50, 25, 5, 0)
	res3, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev2, ev3},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(6 * time.Second),
			ProducerSeq:   3,
			ReceivedSeqs:  []int64{2, 3},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res3.Accepted)
	assert.Equal(t, 1, res3.Duplicates)

	// Verify rollups reflect exact sum of evt-1 + evt-2 + evt-3 (never double counted).
	rollup, err := client.UsageRollupDaily.Query().
		Where(
			usagerollupdaily.BucketStartEQ(dayUTC),
			usagerollupdaily.ProjectIDEQ(projectID),
			usagerollupdaily.BillingUserIDEQ(userID),
			usagerollupdaily.AgentIDEQ(agentID),
		).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), rollup.Calls)
	assert.Equal(t, int64(3), rollup.Events)
	assert.Equal(t, int64(350), rollup.Input)
	assert.Equal(t, int64(155), rollup.Output)
	assert.Equal(t, int64(25), rollup.CacheRead)
	assert.Equal(t, int64(10), rollup.CacheWrite)

	// Verify all 3 scopes (project, user, user_project) have exact summable tokens: 350+155+25+10 = 540.
	wantSummable := int64(540)
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
		assert.Equal(t, wantSummable, sc.TokensSummable, "scope %s", tc.kind)
		assert.Equal(t, int64(3), sc.Calls, "scope %s", tc.kind)
	}

	// Verify meter_health advanced cleanly to seq 3 with no missing ranges.
	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(3), mhs[0].LastContiguousSeq)
	assert.Equal(t, int64(3), mhs[0].MaxSeqSeen)
	assert.Empty(t, mhs[0].MissingRanges)
}

func TestUsageIngest_ConcurrentOverlappingBatches(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ingest-concurrent")
	userID := createTestUser(t, cs, "concurrent-user@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	// 20 unique events; 8 goroutines each ingest overlapping slices of them.
	const totalEvents = 20
	allEvents := make([]*store.UsageEvent, totalEvents)
	for i := 0; i < totalEvents; i++ {
		allEvents[i] = makeStoreUsageEvent(
			"hub-test", fmt.Sprintf("conc-evt-%02d", i+1), runID, projectID, agentID, &userID,
			int64(i+1), ts.Add(time.Duration(i)*time.Second), 10, 5, 2, 3,
		)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	totalAccepted := make(chan int, workers)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			// Each worker sends a different overlapping window covering all events across the pool.
			start := (workerIdx * 2) % totalEvents
			batch := make([]*store.UsageEvent, 0, 12)
			seqs := make([]int64, 0, 12)
			for j := 0; j < 12; j++ {
				ev := allEvents[(start+j)%totalEvents]
				batch = append(batch, ev)
				seqs = append(seqs, ev.ProducerSeq)
			}
			res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
				AgentID: agentID,
				Events:  batch,
				MeterHealth: &store.UsageMeterHealthUpdate{
					AgentID:       agentID,
					RunID:         runID,
					ProducerEpoch: "ep-1",
					LastReceiptAt: ts.Add(time.Minute),
					ProducerSeq:   int64(totalEvents),
					ReceivedSeqs:  seqs,
				},
			})
			if err != nil {
				errs <- err
				return
			}
			totalAccepted <- res.Accepted
		}(w)
	}
	wg.Wait()
	close(errs)
	close(totalAccepted)

	for err := range errs {
		require.NoError(t, err)
	}
	sumAccepted := 0
	for n := range totalAccepted {
		sumAccepted += n
	}
	assert.Equal(t, totalEvents, sumAccepted, "each unique event must be accepted exactly once")

	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.BucketStartEQ(dayUTC), usagerollupdaily.AgentIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(totalEvents), rollup.Events)
	assert.Equal(t, int64(totalEvents*10), rollup.Input)
	assert.Equal(t, int64(totalEvents*5), rollup.Output)
	assert.Equal(t, int64(totalEvents*2), rollup.CacheRead)
	assert.Equal(t, int64(totalEvents*3), rollup.CacheWrite)

	sc, err := client.UsageScopeCounterDaily.Query().
		Where(
			usagescopecounterdaily.ScopeKindEQ(store.UsageScopeKindProject),
			usagescopecounterdaily.ScopeIDEQ(projectID),
			usagescopecounterdaily.DayUtcEQ(dayUTC),
		).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(totalEvents*20), sc.TokensSummable)
	assert.Equal(t, int64(totalEvents), sc.Calls)
}

func TestUsageIngest_NullTokensAndNullBillingUser(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ingest-nulls")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	// NULL billing_user_id -> project scope counter ONLY (§3.2).
	seedRunForIngestTest(t, cs, projectID, agentID, runID, nil)

	ts := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	// Event 1: calls-only (all token fields NULL).
	callsOnly := &store.UsageEvent{
		HubID:         "hub-test",
		EventID:       "evt-null-1",
		RunID:         runID,
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: nil,
		Harness:       "antigravity",
		Model:         "gemini-3",
		ModelSource:   telemetrycontract.ModelSourceConfigured,
		UsageSource:   store.UsageSourceHooks,
		Status:        telemetrycontract.StatusSuccess,
		Calls:         2,
		TokenStatus:   telemetrycontract.TokenStatusNotReported,
		EventTS:       ts,
		ReceivedAt:    ts,
		ProducerEpoch: "ep-null",
		ProducerSeq:   1,
	}
	// Event 2: partial tokens (input=10, output=0 explicit zero, cache_read=NULL, cache_write=NULL).
	partial := &store.UsageEvent{
		HubID:         "hub-test",
		EventID:       "evt-null-2",
		RunID:         runID,
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: nil,
		Harness:       "antigravity",
		Model:         "gemini-3",
		ModelSource:   telemetrycontract.ModelSourceConfigured,
		UsageSource:   store.UsageSourceHooks,
		Status:        telemetrycontract.StatusError,
		Calls:         1,
		Input:         i64p(10),
		Output:        i64p(0),
		TokenStatus:   telemetrycontract.TokenStatusPartial,
		EventTS:       ts.Add(time.Second),
		ReceivedAt:    ts.Add(time.Second),
		ProducerEpoch: "ep-null",
		ProducerSeq:   2,
	}

	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{callsOnly, partial},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Accepted)

	// Verify NULL fields stayed SQL NULL in usage_events (never fabricated as 0).
	row1, err := client.UsageEvent.Query().Where(usageevent.EventIDEQ("evt-null-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, row1.Input)
	assert.Nil(t, row1.Output)
	assert.Nil(t, row1.CacheRead)
	assert.Nil(t, row1.CacheWrite)
	assert.Nil(t, row1.CacheWrite1h)
	assert.Nil(t, row1.Reasoning)
	assert.Nil(t, row1.BillingUserID)

	row2, err := client.UsageEvent.Query().Where(usageevent.EventIDEQ("evt-null-2")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, row2.Input)
	assert.Equal(t, int64(10), *row2.Input)
	require.NotNil(t, row2.Output, "explicit 0 must stay non-nil 0")
	assert.Equal(t, int64(0), *row2.Output)
	assert.Nil(t, row2.CacheRead)

	// Rollup uses billing_user_id="" and counts null_token_events=1, error_calls=1.
	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.AgentIDEQ(agentID), usagerollupdaily.BucketStartEQ(dayUTC)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "", rollup.BillingUserID)
	assert.Equal(t, int64(3), rollup.Calls)
	assert.Equal(t, int64(1), rollup.ErrorCalls)
	assert.Equal(t, int64(10), rollup.Input)
	assert.Equal(t, int64(0), rollup.Output)
	assert.Equal(t, int64(2), rollup.Events)
	assert.Equal(t, int64(1), rollup.NullTokenEvents)

	// Only project scope counter exists (no user or user_project rows).
	scopes, err := client.UsageScopeCounterDaily.Query().
		Where(usagescopecounterdaily.DayUtcEQ(dayUTC)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, store.UsageScopeKindProject, scopes[0].ScopeKind)
	assert.Equal(t, projectID, scopes[0].ScopeID)
	assert.Equal(t, int64(10), scopes[0].TokensSummable)
	assert.Equal(t, int64(3), scopes[0].Calls)
	assert.Equal(t, int64(2), scopes[0].UnmeasuredCalls)
}

func TestUsageIngest_AnomalousExcludedFromScopeCounters(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ingest-anomalous")
	userID := createTestUser(t, cs, "anomalous-user@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	normal := makeStoreUsageEvent("hub-test", "evt-norm", runID, projectID, agentID, &userID, 1, ts, 1000, 500, 0, 0)
	spike := makeStoreUsageEvent("hub-test", "evt-spike", runID, projectID, agentID, &userID, 2, ts.Add(time.Second), 60_000_000, 0, 0, 0)
	spike.Anomalous = true

	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{normal, spike},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(2 * time.Second),
			ProducerSeq:   2,
			ReceivedSeqs:  []int64{1, 2},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Accepted)

	// usage_rollups_daily includes the spike for operator visibility.
	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.AgentIDEQ(agentID), usagerollupdaily.BucketStartEQ(dayUTC)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(60_001_000), rollup.Input)

	// usage_scope_counters_daily excludes the anomalous event's tokens_summable
	// so P3 automatic hard quota enforcement is never tripped by a corrupt spike.
	sc, err := client.UsageScopeCounterDaily.Query().
		Where(
			usagescopecounterdaily.ScopeKindEQ(store.UsageScopeKindProject),
			usagescopecounterdaily.ScopeIDEQ(projectID),
			usagescopecounterdaily.DayUtcEQ(dayUTC),
		).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1500), sc.TokensSummable)
	assert.Equal(t, int64(2), sc.Calls)

	// meter_health.anomalous_count incremented by 1.
	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(1), mhs[0].AnomalousCount)
}

func TestMeterHealth_FirstInsertRaceAndShrinkingMissingRanges(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	epoch := "ep-race"
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	// 1. Concurrent first-insert race: 8 goroutines ingest seqs 1..8 on a brand-new key.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
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
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(8), mhs[0].LastContiguousSeq, "highest contiguous seq must not be lost on first-insert race")
	assert.Equal(t, int64(8), mhs[0].MaxSeqSeen)
	assert.Empty(t, mhs[0].MissingRanges)

	// 2. Monotonic gaps: advance to max=15 delivering {9,10,14,15} -> gap [11, 13].
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: epoch,
			LastReceiptAt: t0.Add(20 * time.Second),
			ProducerSeq:   15,
			ReceivedSeqs:  []int64{9, 10, 14, 15},
		},
	})
	require.NoError(t, err)

	// Stale heartbeat at max=14 must NOT regress contiguity, max, or grow [11, 13].
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: epoch,
			LastReceiptAt: t0.Add(15 * time.Second),
			ProducerSeq:   14,
		},
	})
	require.NoError(t, err)

	mhs, err = cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(10), mhs[0].LastContiguousSeq)
	assert.Equal(t, int64(15), mhs[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 11, End: 13}}, mhs[0].MissingRanges, "stale writer must not grow missing_ranges")

	// Receiving seqs 11 and 13 shrinks gap to [12, 12] and advances contiguous to 11.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: epoch,
			LastReceiptAt: t0.Add(25 * time.Second),
			ProducerSeq:   15,
			ReceivedSeqs:  []int64{11, 13},
		},
	})
	require.NoError(t, err)
	mhs, err = cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	assert.Equal(t, int64(11), mhs[0].LastContiguousSeq)
	assert.Equal(t, []store.SeqRange{{Start: 12, End: 12}}, mhs[0].MissingRanges)
}

func TestMeterHealth_EmittedVsReceivedContiguousScenarios(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "mh-emitted-vs-received")
	agentID := uuid.NewString()
	t0 := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)

	// Scenario (a): batch delivers seqs 1..500 while producer_seq=1200 (backlog pending)
	// -> contig=500, max=1200, missing=[501, 1200].
	runA := "run-a-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runA, nil)
	seqsA := make([]int64, 500)
	eventsA := make([]*store.UsageEvent, 500)
	for i := 0; i < 500; i++ {
		seq := int64(i + 1)
		seqsA[i] = seq
		eventsA[i] = makeStoreUsageEvent("hub-test", fmt.Sprintf("a-evt-%04d", seq), runA, projectID, agentID, nil, seq, t0, 1, 1, 0, 0)
		eventsA[i].ProducerEpoch = "ep-a"
	}
	resA, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  eventsA,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runA,
			ProducerEpoch: "ep-a",
			LastReceiptAt: t0,
			ProducerSeq:   1200,
			ReceivedSeqs:  seqsA,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 500, resA.Accepted)

	mhsA, err := cs.GetMeterHealth(ctx, agentID, runA)
	require.NoError(t, err)
	require.Len(t, mhsA, 1)
	assert.Equal(t, int64(500), mhsA[0].LastContiguousSeq)
	assert.Equal(t, int64(1200), mhsA[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 501, End: 1200}}, mhsA[0].MissingRanges)

	// Scenario (b) & (c):
	// First deliver 1..3 (contig=3, max=3, missing=[]).
	runB := "run-b-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runB, nil)
	makeEvB := func(seq int64) *store.UsageEvent {
		ev := makeStoreUsageEvent("hub-test", fmt.Sprintf("b-evt-%02d", seq), runB, projectID, agentID, nil, seq, t0, 1, 1, 0, 0)
		ev.ProducerEpoch = "ep-b"
		return ev
	}
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{makeEvB(1), makeEvB(2), makeEvB(3)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runB,
			ProducerEpoch: "ep-b",
			LastReceiptAt: t0,
			ProducerSeq:   3,
			ReceivedSeqs:  []int64{1, 2, 3},
		},
	})
	require.NoError(t, err)

	// Heartbeat raises producer_seq to 10 (no events delivered yet) -> contig=3, max=10, missing=[4, 10].
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runB,
			ProducerEpoch: "ep-b",
			LastReceiptAt: t0.Add(time.Second),
			ProducerSeq:   10,
		},
	})
	require.NoError(t, err)
	mhsB, err := cs.GetMeterHealth(ctx, agentID, runB)
	require.NoError(t, err)
	require.Len(t, mhsB, 1)
	assert.Equal(t, int64(3), mhsB[0].LastContiguousSeq)
	assert.Equal(t, int64(10), mhsB[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 4, End: 10}}, mhsB[0].MissingRanges)

	// (b) Deliver 4..6 -> contig=6, max=10, missing=[7, 10].
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{makeEvB(4), makeEvB(5), makeEvB(6)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runB,
			ProducerEpoch: "ep-b",
			LastReceiptAt: t0.Add(2 * time.Second),
			ProducerSeq:   10,
			ReceivedSeqs:  []int64{4, 5, 6},
		},
	})
	require.NoError(t, err)
	mhsB, err = cs.GetMeterHealth(ctx, agentID, runB)
	require.NoError(t, err)
	assert.Equal(t, int64(6), mhsB[0].LastContiguousSeq)
	assert.Equal(t, int64(10), mhsB[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 7, End: 10}}, mhsB[0].MissingRanges)

	// (c) Deliver 7..10 -> contig=10, max=10, missing=[].
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{makeEvB(7), makeEvB(8), makeEvB(9), makeEvB(10)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runB,
			ProducerEpoch: "ep-b",
			LastReceiptAt: t0.Add(3 * time.Second),
			ProducerSeq:   10,
			ReceivedSeqs:  []int64{7, 8, 9, 10},
		},
	})
	require.NoError(t, err)
	mhsB, err = cs.GetMeterHealth(ctx, agentID, runB)
	require.NoError(t, err)
	assert.Equal(t, int64(10), mhsB[0].LastContiguousSeq)
	assert.Equal(t, int64(10), mhsB[0].MaxSeqSeen)
	assert.Empty(t, mhsB[0].MissingRanges)
}

func TestUsageIngest_RollupInt64OverflowSaturates(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ingest-overflow")
	userID := createTestUser(t, cs, "overflow-user@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 18, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	makeBatch := func(prefix string, startSeq int64, count int) []*store.UsageEvent {
		batch := make([]*store.UsageEvent, count)
		for i := 0; i < count; i++ {
			seq := startSeq + int64(i)
			ev := makeStoreUsageEvent(
				"hub-test", fmt.Sprintf("%s-%04d", prefix, seq), runID, projectID, agentID, &userID,
				seq, ts, telemetrycontract.MaxSafeInteger, 0, 0, 0,
			)
			ev.Anomalous = true
			batch[i] = ev
		}
		return batch
	}

	// Two batches of 600 anomalous MaxSafeInteger-input events for the same
	// rollup key: 600 * (2^53-1) fits in int64, 1200 * (2^53-1) exceeds
	// math.MaxInt64 and must saturate cleanly on both SQLite and Postgres.
	res1, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  makeBatch("ovf-b1", 1, 600),
	})
	require.NoError(t, err)
	assert.Equal(t, 600, res1.Accepted)

	res2, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  makeBatch("ovf-b2", 601, 600),
	})
	require.NoError(t, err)
	assert.Equal(t, 600, res2.Accepted)

	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.BucketStartEQ(dayUTC), usagerollupdaily.AgentIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(math.MaxInt64), rollup.Input, "rollup input must saturate at math.MaxInt64")
	assert.Equal(t, int64(1200), rollup.Events)
	assert.Equal(t, int64(1200), rollup.Calls)
}

func TestMeterHealth_HubAuthorityIgnoresProducerMissingRanges(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "mh-hub-authority")
	agentID := uuid.NewString()
	t0 := time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)

	// Scenario A (RV1): state contiguous=2, max=2; heartbeat arrives with
	// producer_seq=10 and producer-supplied missing=[3,4]. Hub must keep
	// every unreceived sequence in 3..10 (including 5..10) in missing_ranges.
	runA := "run-rv1-a-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runA, nil)
	makeEv := func(runID, epoch string, seq int64) *store.UsageEvent {
		ev := makeStoreUsageEvent("hub-test", fmt.Sprintf("%s-evt-%02d", runID, seq), runID, projectID, agentID, nil, seq, t0, 1, 1, 0, 0)
		ev.ProducerEpoch = epoch
		return ev
	}

	_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{makeEv(runA, "ep-a", 1), makeEv(runA, "ep-a", 2)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runA,
			ProducerEpoch: "ep-a",
			LastReceiptAt: t0,
			ProducerSeq:   2,
			ReceivedSeqs:  []int64{1, 2},
		},
	})
	require.NoError(t, err)

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runA,
			ProducerEpoch: "ep-a",
			LastReceiptAt: t0.Add(time.Second),
			ProducerSeq:   10,
		},
	})
	require.NoError(t, err)

	mhsA, err := cs.GetMeterHealth(ctx, agentID, runA)
	require.NoError(t, err)
	require.Len(t, mhsA, 1)
	assert.Equal(t, int64(2), mhsA[0].LastContiguousSeq)
	assert.Equal(t, int64(10), mhsA[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 3, End: 10}}, mhsA[0].MissingRanges, "heartbeat to 10 must keep 3..10 missing")

	// Scenario B (RV1): from contiguous=4, max=10, missing=[5,10], a batch
	// delivering {5} must yield contiguous=5 and missing=[6,10] (never jumping
	// contiguous to 10).
	runB := "run-rv1-b-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runB, nil)

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			makeEv(runB, "ep-b", 1),
			makeEv(runB, "ep-b", 2),
			makeEv(runB, "ep-b", 3),
			makeEv(runB, "ep-b", 4),
		},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runB,
			ProducerEpoch: "ep-b",
			LastReceiptAt: t0,
			ProducerSeq:   10,
			ReceivedSeqs:  []int64{1, 2, 3, 4},
		},
	})
	require.NoError(t, err)

	mhsB, err := cs.GetMeterHealth(ctx, agentID, runB)
	require.NoError(t, err)
	require.Len(t, mhsB, 1)
	require.Equal(t, int64(4), mhsB[0].LastContiguousSeq)
	require.Equal(t, []store.SeqRange{{Start: 5, End: 10}}, mhsB[0].MissingRanges)

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{makeEv(runB, "ep-b", 5)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runB,
			ProducerEpoch: "ep-b",
			LastReceiptAt: t0.Add(time.Second),
			ProducerSeq:   10,
			ReceivedSeqs:  []int64{5},
		},
	})
	require.NoError(t, err)

	mhsB, err = cs.GetMeterHealth(ctx, agentID, runB)
	require.NoError(t, err)
	require.Len(t, mhsB, 1)
	assert.Equal(t, int64(5), mhsB[0].LastContiguousSeq, "contiguous must advance only to 5, never jumping over 6..10")
	assert.Equal(t, int64(10), mhsB[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 6, End: 10}}, mhsB[0].MissingRanges, "Hub-recorded gap [6,10] must remain intact")
}

func TestUsageIngest_PerRowDBRejectionDoesNotFailBatch(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "ingest-per-row-reject")
	userID := createTestUser(t, cs, "per-row-reject@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 22, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	good1 := makeStoreUsageEvent("hub-test", "evt-good-1", runID, projectID, agentID, &userID, 1, ts, 100, 40, 10, 5)
	good1.ID = uuid.NewString()
	// Shares good1's primary key UUID (with a distinct event_id) so it triggers
	// a real DB constraint violation (SQLITE_CONSTRAINT_PRIMARYKEY) while
	// belonging to the same (run_id, producer_epoch) as MeterHealth. This
	// exercises both per-row SAVEPOINT rollback and the store filter that
	// removes DB-rejected seqs from ReceivedSeqs.
	badRow := makeStoreUsageEvent("hub-test", "evt-poison-2", runID, projectID, agentID, &userID, 2, ts.Add(time.Second), 999, 999, 0, 0)
	badRow.ID = good1.ID
	good3 := makeStoreUsageEvent("hub-test", "evt-good-3", runID, projectID, agentID, &userID, 3, ts.Add(2*time.Second), 200, 60, 20, 10)

	batch := &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{good1, badRow, good3},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(3 * time.Second),
			ProducerSeq:   3,
			ReceivedSeqs:  []int64{1, 2, 3},
		},
	}

	res1, err := cs.IngestUsageBatch(ctx, batch)
	require.NoError(t, err, "one DB-rejected row must not fail the batch")
	assert.Equal(t, 2, res1.Accepted)
	assert.Equal(t, 0, res1.Duplicates)
	require.Len(t, res1.Rejected, 1)
	assert.Equal(t, "evt-poison-2", res1.Rejected[0].EventID)
	assert.NotEmpty(t, res1.Rejected[0].Reason)

	// Replay the exact same batch: good events are duplicates, bad row is still
	// rejected, and rollups/scope counters remain exact.
	res2, err := cs.IngestUsageBatch(ctx, batch)
	require.NoError(t, err)
	assert.Equal(t, 0, res2.Accepted)
	assert.Equal(t, 2, res2.Duplicates)
	require.Len(t, res2.Rejected, 1)
	assert.Equal(t, "evt-poison-2", res2.Rejected[0].EventID)

	// Verify rollups reflect ONLY the 2 good events (never the bad row).
	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.BucketStartEQ(dayUTC), usagerollupdaily.AgentIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), rollup.Events)
	assert.Equal(t, int64(300), rollup.Input)
	assert.Equal(t, int64(100), rollup.Output)
	assert.Equal(t, int64(30), rollup.CacheRead)
	assert.Equal(t, int64(15), rollup.CacheWrite)

	sc, err := client.UsageScopeCounterDaily.Query().
		Where(
			usagescopecounterdaily.ScopeKindEQ(store.UsageScopeKindProject),
			usagescopecounterdaily.ScopeIDEQ(projectID),
			usagescopecounterdaily.DayUtcEQ(dayUTC),
		).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(445), sc.TokensSummable)
	assert.Equal(t, int64(2), sc.Calls)

	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(3), mhs[0].LastContiguousSeq)
	assert.Equal(t, int64(3), mhs[0].MaxSeqSeen)
	assert.Empty(t, mhs[0].MissingRanges)
	assert.Equal(t, int64(1), mhs[0].RejectedCount)
	assert.Equal(t, []store.SeqRange{{Start: 2, End: 2}}, mhs[0].RejectedRanges)
}

func TestIsUsageEventRowDataError(t *testing.T) {
	assert.True(t, isUsageEventRowDataError(&pgconn.PgError{Code: "22021"}))  // invalid_byte_sequence (NUL byte)
	assert.True(t, isUsageEventRowDataError(&pgconn.PgError{Code: "22001"}))  // string_data_right_truncation
	assert.True(t, isUsageEventRowDataError(&pgconn.PgError{Code: "23503"}))  // foreign_key_violation
	assert.True(t, isUsageEventRowDataError(&pgconn.PgError{Code: "23514"}))  // check_violation
	assert.False(t, isUsageEventRowDataError(&pgconn.PgError{Code: "40001"})) // serialization_failure
	assert.False(t, isUsageEventRowDataError(&pgconn.PgError{Code: "40P01"})) // deadlock_detected
	assert.False(t, isUsageEventRowDataError(&pgconn.PgError{Code: "08006"})) // connection_failure
	assert.False(t, isUsageEventRowDataError(nil))
}

func TestMeterHealth_MissingRangesBoundedAndLinearUnderAdversarialProducer(t *testing.T) {
	// Scenario (R1): 250 requests of 500 alternating (even) seqs each = 125,000
	// received seqs and 125,000 unreceived odd seqs, followed by a large
	// producer_seq jump. Range count must stay <= maxMeterHealthMissingRanges,
	// per-request cost must stay bounded, and no unreceived seq may ever be
	// marked received or skipped by last_contiguous_seq.
	const (
		batches  = 250
		perBatch = 500
	)
	var (
		contig       int64
		maxSeq       int64
		missing      []store.SeqRange
		anyCoalesced bool
	)

	start := time.Now()
	for b := 0; b < batches; b++ {
		seqs := make([]int64, perBatch)
		for i := 0; i < perBatch; i++ {
			seqs[i] = int64((b*perBatch + i + 1) * 2) // 2, 4, 6, ... (all odd seqs unreceived)
		}
		var coalesced bool
		contig, maxSeq, missing, _, _, coalesced, _ = advanceIngestSeqState(contig, maxSeq, missing, 0, nil, seqs[len(seqs)-1], seqs, nil)
		if coalesced {
			anyCoalesced = true
		}
		require.LessOrEqual(t, len(missing), maxMeterHealthMissingRanges, "batch %d exceeded missing_ranges cap", b)
		require.Equal(t, int64(0), contig, "last_contiguous_seq must never advance past unreceived seq 1")
	}
	// Large producer_seq jump beyond maxSeq.
	jumpTarget := maxSeq + 1_000_000
	contig, maxSeq, missing, _, _, _, _ = advanceIngestSeqState(contig, maxSeq, missing, 0, nil, jumpTarget, nil, nil)
	elapsed := time.Since(start)

	assert.True(t, anyCoalesced, "adversarial every-other-seq producer must trigger conservative coalescing")
	assert.LessOrEqual(t, len(missing), maxMeterHealthMissingRanges)
	assert.Equal(t, int64(0), contig)
	assert.Equal(t, jumpTarget, maxSeq)
	assert.Less(t, elapsed, 2*time.Second, "250 adversarial batches must complete quickly with linear merge + cap (took %v)", elapsed)

	// Verify EVERY unreceived odd seq in [1, batches*perBatch*2] and the jumped
	// tail (batches*perBatch*2, jumpTarget] is covered by missing_ranges.
	contains := func(seq int64) bool {
		idx := sort.Search(len(missing), func(i int) bool { return missing[i].End >= seq })
		return idx < len(missing) && missing[idx].Start <= seq && seq <= missing[idx].End
	}
	totalEvenMax := int64(batches * perBatch * 2)
	for seq := int64(1); seq <= totalEvenMax; seq += 2 {
		if !contains(seq) {
			t.Fatalf("unreceived seq %d was not covered by missing_ranges", seq)
		}
	}
	assert.True(t, contains(totalEvenMax+1), "first jumped seq must be covered")
	assert.True(t, contains(jumpTarget), "last jumped seq must be covered")

	// End-to-end store persistence check through IngestUsageBatch.
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	agentID := uuid.NewString()
	runID := "run-adv-" + uuid.NewString()
	t0 := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)

	for b := 0; b < 6; b++ {
		seqs := make([]int64, 250)
		for i := 0; i < 250; i++ {
			seqs[i] = int64((b*250 + i + 1) * 2)
		}
		_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
			AgentID: agentID,
			MeterHealth: &store.UsageMeterHealthUpdate{
				AgentID:       agentID,
				RunID:         runID,
				ProducerEpoch: "ep-adv",
				LastReceiptAt: t0.Add(time.Duration(b) * time.Second),
				ProducerSeq:   seqs[len(seqs)-1],
				ReceivedSeqs:  seqs,
			},
		})
		require.NoError(t, err)
	}

	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.LessOrEqual(t, len(mhs[0].MissingRanges), maxMeterHealthMissingRanges)
	assert.Equal(t, int64(0), mhs[0].LastContiguousSeq)
	assert.Equal(t, int64(3000), mhs[0].MaxSeqSeen)
}

func TestEnsureRunAttributionForAgent_RechecksCurrentRunIDInTx(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "lazy-recheck")
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "lazy-recheck-agent",
		Name:      "lazy-recheck-agent",
		ProjectID: projectID,
		Phase:     "running",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))
	_, err := cs.SetAgentRunID(ctx, ag.ID, "run-current", nil)
	require.NoError(t, err)

	// Stale run_id that no longer matches agents.run_id must be rejected with
	// store.ErrNotFound and must NOT create a run_attribution row.
	_, err = cs.EnsureRunAttributionForAgent(ctx, ag.ID, "run-stale", "hub-test")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetRunAttribution(ctx, "run-stale")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Matching current run_id succeeds and creates attribution.
	attr, err := cs.EnsureRunAttributionForAgent(ctx, ag.ID, "run-current", "hub-test")
	require.NoError(t, err)
	assert.Equal(t, "run-current", attr.RunID)
	assert.Equal(t, projectID, attr.ProjectID)

	// Once the agent moves to a new run, a second call for the already-attributed
	// run still returns the existing row, while an unattributed old run is rejected.
	_, err = cs.SetAgentRunID(ctx, ag.ID, "run-next", nil)
	require.NoError(t, err)
	existing, err := cs.EnsureRunAttributionForAgent(ctx, ag.ID, "run-current", "hub-test")
	require.NoError(t, err)
	assert.Equal(t, "run-current", existing.RunID)
	_, err = cs.EnsureRunAttributionForAgent(ctx, ag.ID, "run-never-attributed", "hub-test")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestUsageIngest_500EventBatchTiming(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "bench-project",
		Slug: "bench-project-" + projectID[:8],
	}))
	userID := uuid.NewString()
	require.NoError(t, cs.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "bench-" + userID[:8] + "@example.com",
		DisplayName: "Bench User",
		Role:        store.UserRoleMember,
	}))
	agentID := uuid.NewString()
	runID := "run-bench-" + uuid.NewString()
	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		Generation:    1,
		HubID:         "hub-bench",
		ProjectID:     projectID,
		AgentID:       agentID,
		AgentSlug:     "bench-worker",
		BrokerID:      "broker-1",
		BillingUserID: &userID,
		Harness:       "claude",
		HarnessConfig: "claude",
		Template:      "default",
		CreatedAt:     time.Now().UTC().Truncate(time.Microsecond),
	})
	require.NoError(t, err)

	baseTS := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	const (
		batchSize  = 500
		iterations = 5
	)

	var total time.Duration
	for iter := 0; iter < iterations; iter++ {
		events := make([]*store.UsageEvent, batchSize)
		seqs := make([]int64, batchSize)
		baseSeq := int64(iter*batchSize + 1)
		for i := 0; i < batchSize; i++ {
			seq := baseSeq + int64(i)
			seqs[i] = seq
			events[i] = makeStoreUsageEvent(
				"hub-bench",
				fmt.Sprintf("bench-evt-%d-%d", iter, i),
				runID, projectID, agentID, &userID,
				seq,
				baseTS.Add(time.Duration(i)*time.Millisecond),
				100+int64(i), 50, 20, 10,
			)
		}
		in := &store.UsageIngestBatchInput{
			AgentID: agentID,
			Events:  events,
			MeterHealth: &store.UsageMeterHealthUpdate{
				AgentID:       agentID,
				RunID:         runID,
				ProducerEpoch: "ep-bench",
				LastReceiptAt: baseTS.Add(time.Duration(iter+1) * time.Second),
				ProducerSeq:   baseSeq + batchSize - 1,
				ReceivedSeqs:  seqs,
			},
		}
		start := time.Now()
		res, err := cs.IngestUsageBatch(ctx, in)
		elapsed := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, batchSize, res.Accepted)
		total += elapsed
	}
	t.Logf("500-event batch avg over %d runs (%s): %v", iterations, client.Driver().Dialect(), total/iterations)
}

func TestUsageIngest_RejectedSeqsVsLostGaps(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "rejected-vs-lost")
	userID := createTestUser(t, cs, "rejected-vs-lost@example.com")
	agentID := uuid.NewString()
	runID := "run-rej-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)

	// 1. Batch with seq 1 (valid), seq 2 (handler-rejected via RejectedSeqs), seq 3 (valid).
	ev1 := makeStoreUsageEvent("hub-test", "rej-evt-1", runID, projectID, agentID, &userID, 1, ts, 10, 5, 0, 0)
	ev3 := makeStoreUsageEvent("hub-test", "rej-evt-3", runID, projectID, agentID, &userID, 3, ts.Add(2*time.Second), 20, 10, 0, 0)
	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1, ev3},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(3 * time.Second),
			ProducerSeq:   3,
			ReceivedSeqs:  []int64{1, 3},
			RejectedSeqs:  []int64{2},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Accepted)

	mhs, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(3), mhs[0].LastContiguousSeq, "rejected seq 2 must not stall last_contiguous_seq")
	assert.Equal(t, int64(3), mhs[0].MaxSeqSeen)
	assert.Empty(t, mhs[0].MissingRanges, "rejected seq 2 must not appear in missing_ranges")
	assert.Equal(t, int64(1), mhs[0].RejectedCount)
	assert.Equal(t, []store.SeqRange{{Start: 2, End: 2}}, mhs[0].RejectedRanges)

	// 2. Idempotent retry of the exact same batch must not double-count rejected_count.
	resRetry, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1, ev3},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(4 * time.Second),
			ProducerSeq:   3,
			ReceivedSeqs:  []int64{1, 3},
			RejectedSeqs:  []int64{2},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, resRetry.Accepted)
	assert.Equal(t, 2, resRetry.Duplicates)

	mhs, err = cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), mhs[0].RejectedCount, "retry must not increment rejected_count")
	assert.Equal(t, []store.SeqRange{{Start: 2, End: 2}}, mhs[0].RejectedRanges)
	assert.Empty(t, mhs[0].MissingRanges)

	// 3. Batch skipping seq 4 (genuinely lost), delivering seq 5 (valid),
	//    seq 6 (DB-rejected via FK violation on missing run_id), seq 7 (valid).
	ev5 := makeStoreUsageEvent("hub-test", "rej-evt-5", runID, projectID, agentID, &userID, 5, ts.Add(5*time.Second), 30, 15, 0, 0)
	ev6Bad := makeStoreUsageEvent("hub-test", "rej-evt-6-bad", "nonexistent-run-fk", projectID, agentID, &userID, 6, ts.Add(6*time.Second), 99, 99, 0, 0)
	ev7 := makeStoreUsageEvent("hub-test", "rej-evt-7", runID, projectID, agentID, &userID, 7, ts.Add(7*time.Second), 40, 20, 0, 0)
	// Also test a DB-rejected event matching the same runID (empty event_id or
	// constraint error) by setting Handler RejectedSeqs for seq 6 alongside FK-rejected row.
	res3, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev5, ev6Bad, ev7},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(8 * time.Second),
			ProducerSeq:   7,
			ReceivedSeqs:  []int64{5, 7},
			RejectedSeqs:  []int64{6},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res3.Accepted)
	require.Len(t, res3.Rejected, 1)
	assert.Equal(t, "rej-evt-6-bad", res3.Rejected[0].EventID)

	mhs, err = cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), mhs[0].LastContiguousSeq, "genuine lost gap at seq 4 must still block contiguity")
	assert.Equal(t, int64(7), mhs[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 4, End: 4}}, mhs[0].MissingRanges, "only genuine lost seq 4 should be in missing_ranges")
	assert.Equal(t, int64(2), mhs[0].RejectedCount)
	assert.Equal(t, []store.SeqRange{{Start: 2, End: 2}, {Start: 6, End: 6}}, mhs[0].RejectedRanges)

	// 4. Late gap-fill for seq 4 closes missing_ranges (shrink-only) and jumps contiguity to 7.
	ev4 := makeStoreUsageEvent("hub-test", "rej-evt-4", runID, projectID, agentID, &userID, 4, ts.Add(4*time.Second), 15, 5, 0, 0)
	res4, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev4},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agentID,
			RunID:         runID,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(9 * time.Second),
			ProducerSeq:   7,
			ReceivedSeqs:  []int64{4},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res4.Accepted)

	mhs, err = cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	assert.Equal(t, int64(7), mhs[0].LastContiguousSeq)
	assert.Empty(t, mhs[0].MissingRanges)
	assert.Equal(t, int64(2), mhs[0].RejectedCount)
	assert.Equal(t, []store.SeqRange{{Start: 2, End: 2}, {Start: 6, End: 6}}, mhs[0].RejectedRanges)

	// 5. Verify rejected_ranges cap (maxMeterHealthMissingRanges = 1024) AND
	//    that rejected_count still increments when a later batch rejects a seq
	//    falling inside a coalesced gap.
	manyRejected := make([]int64, maxMeterHealthMissingRanges+50)
	for i := range manyRejected {
		manyRejected[i] = int64((i + 1) * 2) // 2, 4, 6, ... (odd seqs are gaps)
	}
	contigCap, maxCap, missCap, rejCount, rejRanges, _, rejCoalesced := advanceIngestSeqState(0, 0, nil, 0, nil, int64(len(manyRejected)*2), nil, manyRejected)
	assert.True(t, rejCoalesced)
	assert.LessOrEqual(t, len(rejRanges), maxMeterHealthMissingRanges)
	assert.Equal(t, int64(len(manyRejected)), rejCount)
	// Find an odd seq that was absorbed into a coalesced range (start < end).
	var gapSeq int64
	for _, r := range rejRanges {
		if r.End > r.Start {
			gapSeq = r.Start + 1 // odd seq between two even rejected seqs
			break
		}
	}
	require.NotZero(t, gapSeq, "expected at least one coalesced range spanning a gap")
	_, _, _, rejCountAfter, _, _, _ := advanceIngestSeqState(contigCap, maxCap, missCap, rejCount, rejRanges, maxCap, nil, []int64{gapSeq})
	assert.Equal(t, rejCount+1, rejCountAfter, "newly rejected seq inside a coalesced gap must still increment rejected_count")
}

func TestUsageIngest_MultiRowBatchFallbackAndDuplicates(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "multirow-fallback")
	userID := createTestUser(t, cs, "multirow-fallback@example.com")
	agentID := uuid.NewString()
	runID := "run-mr-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agentID, runID, &userID)

	ts := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	// Seed 3 existing events: mr-evt-0 and mr-evt-1 land in the first chunk
	// (exercising fast-path DB duplicates), while mr-evt-bad-dup sorts adjacent
	// to mr-evt-bad-fk in the final chunk (exercising the per-row fallback's
	// dbDuplicates branch).
	seed1 := makeStoreUsageEvent("hub-mr", "mr-evt-0", runID, projectID, agentID, &userID, 1, ts, 10, 5, 0, 0)
	seed2 := makeStoreUsageEvent("hub-mr", "mr-evt-1", runID, projectID, agentID, &userID, 2, ts.Add(time.Second), 10, 5, 0, 0)
	seedFallbackDup := makeStoreUsageEvent("hub-mr", "mr-evt-bad-dup", runID, projectID, agentID, &userID, 98, ts.Add(98*time.Second), 10, 5, 0, 0)
	res0, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{seed1, seed2, seedFallbackDup},
	})
	require.NoError(t, err)
	require.Equal(t, 3, res0.Accepted)

	// Build a 61-event batch (3 chunks on SQLite) containing:
	// - 2 fast-path DB duplicates (mr-evt-0, mr-evt-1)
	// - 1 fallback-chunk DB duplicate (mr-evt-bad-dup, sorts in the third SQLite chunk with mr-evt-bad-fk)
	// - 1 intra-batch duplicate (mr-evt-10 twice)
	// - 1 bad row (non-existent run_id FK violation) sorting last (third SQLite chunk)
	// - 56 new valid rows
	events := make([]*store.UsageEvent, 0, 61)
	events = append(events, seed1, seed2, seedFallbackDup)
	for i := 2; i < 58; i++ {
		ev := makeStoreUsageEvent("hub-mr", fmt.Sprintf("mr-evt-%d", i), runID, projectID, agentID, &userID, int64(i+1), ts.Add(time.Duration(i)*time.Second), 10, 5, 0, 0)
		events = append(events, ev)
		if i == 10 {
			events = append(events, ev) // intra-batch duplicate
		}
	}
	badEv := makeStoreUsageEvent("hub-mr", "mr-evt-bad-fk", "run-does-not-exist", projectID, agentID, &userID, 99, ts.Add(99*time.Second), 999, 999, 0, 0)
	events = append(events, badEv)

	res, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  events,
	})
	require.NoError(t, err)
	assert.Equal(t, 56, res.Accepted)
	assert.Equal(t, 4, res.Duplicates, "3 DB duplicates (2 fast-path + 1 fallback-chunk) + 1 intra-batch duplicate")
	require.Len(t, res.Rejected, 1)
	assert.Equal(t, "mr-evt-bad-fk", res.Rejected[0].EventID)
	assert.Len(t, res.InsertedEvents, 56)

	// Verify rollups reflect exact 59 total inserted rows (3 seeded + 56 new), never duplicates or rejected row.
	rollup, err := client.UsageRollupDaily.Query().
		Where(usagerollupdaily.BucketStartEQ(dayUTC), usagerollupdaily.AgentIDEQ(agentID)).
		Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(59), rollup.Events)
	assert.Equal(t, int64(590), rollup.Input)
	assert.Equal(t, int64(295), rollup.Output)
}

func TestUsageIngest_AntigravityUnmeasuredCallsReconciliation(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "agy-unmeasured")
	userID := createTestUser(t, cs, "agy-unmeasured@example.com")
	ts := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC)
	dayUTC := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)

	makeHookCall := func(eventID, runID, agentID, epoch string, seq int64) *store.UsageEvent {
		return &store.UsageEvent{
			HubID:          "hub-agy",
			EventID:        eventID,
			RunID:          runID,
			ProjectID:      projectID,
			AgentID:        agentID,
			BillingUserID:  &userID,
			Harness:        "antigravity",
			Model:          "gemini-3.1-pro",
			ModelSource:    telemetrycontract.ModelSourceEvent,
			UsageSource:    store.UsageSourceHooks,
			SourceKind:     "hook.antigravity.model_end",
			SourceIdentity: telemetrycontract.SourceIdentityComposite,
			Status:         telemetrycontract.StatusSuccess,
			Calls:          1,
			TokenStatus:    telemetrycontract.TokenStatusNotReported,
			EventTS:        ts.Add(time.Duration(seq) * time.Second),
			ReceivedAt:     ts.Add(time.Duration(seq) * time.Second),
			ProducerEpoch:  epoch,
			ProducerSeq:    seq,
		}
	}

	getProjectCounter := func() (calls, unmeasured int64) {
		row, err := client.UsageScopeCounterDaily.Query().
			Where(
				usagescopecounterdaily.ScopeKindEQ(store.UsageScopeKindProject),
				usagescopecounterdaily.ScopeIDEQ(projectID),
				usagescopecounterdaily.DayUtcEQ(dayUTC),
			).
			Only(ctx)
		require.NoError(t, err)
		return row.Calls, row.UnmeasuredCalls
	}

	// 1. Rebuilt antigravity (signals include tokens, no drift): hook event arrives
	//    BEFORE any sqlite_native tailer event -> calls=1, unmeasured_calls=0.
	agRebuilt := uuid.NewString()
	runRebuilt := "run-agy-rebuilt-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agRebuilt, runRebuilt, &userID)
	_, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agRebuilt,
		Events:  []*store.UsageEvent{makeHookCall("agy-rebuilt-1", runRebuilt, agRebuilt, "ep-1", 1)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:             agRebuilt,
			RunID:               runRebuilt,
			ProducerEpoch:       "ep-1",
			LastReceiptAt:       ts,
			ProducerSeq:         1,
			ReceivedSeqs:        []int64{1},
			SupportedSignals:    []string{"calls", "input", "output", "cache_read", "cache_write", "reasoning"},
			SchemaDriftDetected: false,
		},
	})
	require.NoError(t, err)
	calls, unmeasured := getProjectCounter()
	assert.Equal(t, int64(1), calls)
	assert.Equal(t, int64(0), unmeasured, "rebuilt antigravity hook call before tailer event must not count as unmeasured")

	// 2. Harvest batch without SupportedSignals (both empty slice and nil MeterHealth)
	//    falls back to persisted meter_health -> still unmeasured_calls=0.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agRebuilt,
		Events:  []*store.UsageEvent{makeHookCall("agy-rebuilt-harvest-1", runRebuilt, agRebuilt, "ep-1", 2)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agRebuilt,
			RunID:         runRebuilt,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(2 * time.Second),
			ProducerSeq:   2,
			ReceivedSeqs:  []int64{2},
		},
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agRebuilt,
		Events:  []*store.UsageEvent{makeHookCall("agy-rebuilt-harvest-2", runRebuilt, agRebuilt, "ep-1", 3)},
	})
	require.NoError(t, err)
	calls, unmeasured = getProjectCounter()
	assert.Equal(t, int64(3), calls)
	assert.Equal(t, int64(0), unmeasured, "harvest batches without signals must use persisted meter_health")

	// 3. Mid-run schema drift: drift latches on meter_health; subsequent hook call
	//    (even if the later request has SchemaDriftDetected=false and omits signals)
	//    must count in unmeasured_calls.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agRebuilt,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:             agRebuilt,
			RunID:               runRebuilt,
			ProducerEpoch:       "ep-1",
			LastReceiptAt:       ts.Add(4 * time.Second),
			ProducerSeq:         3,
			SupportedSignals:    []string{"calls"},
			SchemaDriftDetected: true,
		},
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agRebuilt,
		Events:  []*store.UsageEvent{makeHookCall("agy-rebuilt-post-drift", runRebuilt, agRebuilt, "ep-1", 4)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       agRebuilt,
			RunID:         runRebuilt,
			ProducerEpoch: "ep-1",
			LastReceiptAt: ts.Add(5 * time.Second),
			ProducerSeq:   4,
			ReceivedSeqs:  []int64{4},
		},
	})
	require.NoError(t, err)
	calls, unmeasured = getProjectCounter()
	assert.Equal(t, int64(4), calls)
	assert.Equal(t, int64(1), unmeasured, "post-drift hook call must count as unmeasured")

	// 4. Unrebuilt antigravity image (supported_signals=["calls"]): hook calls count as unmeasured.
	agOld := uuid.NewString()
	runOld := "run-agy-old-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agOld, runOld, &userID)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agOld,
		Events:  []*store.UsageEvent{makeHookCall("agy-old-1", runOld, agOld, "ep-1", 1)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:          agOld,
			RunID:            runOld,
			ProducerEpoch:    "ep-1",
			LastReceiptAt:    ts.Add(6 * time.Second),
			ProducerSeq:      1,
			ReceivedSeqs:     []int64{1},
			SupportedSignals: []string{"calls"},
		},
	})
	require.NoError(t, err)
	calls, unmeasured = getProjectCounter()
	assert.Equal(t, int64(5), calls)
	assert.Equal(t, int64(2), unmeasured, "unrebuilt calls-only antigravity image must count as unmeasured")

	// 5. Non-hooks event with nil tokens on a healthy token-metered run still counts as unmeasured.
	nativeNoTok := makeHookCall("agy-native-notok", runRebuilt, agRebuilt, "ep-2", 1)
	nativeNoTok.UsageSource = store.UsageSourceNative
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agRebuilt,
		Events:  []*store.UsageEvent{nativeNoTok},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:          agRebuilt,
			RunID:            runRebuilt,
			ProducerEpoch:    "ep-2",
			LastReceiptAt:    ts.Add(7 * time.Second),
			ProducerSeq:      1,
			ReceivedSeqs:     []int64{1},
			SupportedSignals: []string{"calls", "input", "output"},
		},
	})
	require.NoError(t, err)
	calls, unmeasured = getProjectCounter()
	assert.Equal(t, int64(6), calls)
	assert.Equal(t, int64(3), unmeasured, "non-hooks event with missing tokens must still count as unmeasured")

	// 6. Independent schema_drift_detected check (D12): producer declares token
	//    signals AND schema_drift_detected=true. Both the in-request path
	//    (resolvedMHState) and the persisted path (MeterHealth == nil) must
	//    count the hooks calls-only event in unmeasured_calls.
	agDriftTok := uuid.NewString()
	runDriftTok := "run-agy-drift-tok-" + uuid.NewString()
	seedRunForIngestTest(t, cs, projectID, agDriftTok, runDriftTok, &userID)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agDriftTok,
		Events:  []*store.UsageEvent{makeHookCall("agy-drift-tok-req", runDriftTok, agDriftTok, "ep-1", 1)},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:             agDriftTok,
			RunID:               runDriftTok,
			ProducerEpoch:       "ep-1",
			LastReceiptAt:       ts.Add(8 * time.Second),
			ProducerSeq:         1,
			ReceivedSeqs:        []int64{1},
			SupportedSignals:    []string{"calls", "input", "output", "cache_read", "cache_write", "reasoning"},
			SchemaDriftDetected: true,
		},
	})
	require.NoError(t, err)
	calls, unmeasured = getProjectCounter()
	assert.Equal(t, int64(7), calls)
	assert.Equal(t, int64(4), unmeasured, "schema_drift_detected=true with token signals (request path) must count as unmeasured")

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agDriftTok,
		Events:  []*store.UsageEvent{makeHookCall("agy-drift-tok-persisted", runDriftTok, agDriftTok, "ep-1", 2)},
	})
	require.NoError(t, err)
	calls, unmeasured = getProjectCounter()
	assert.Equal(t, int64(8), calls)
	assert.Equal(t, int64(5), unmeasured, "schema_drift_detected=true with token signals (persisted meter_health path) must count as unmeasured")
}
