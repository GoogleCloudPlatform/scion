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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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

	// 1. Concurrent first-insert race: 8 goroutines upsert seqs 1..8 on a brand-new key.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(seq int64) {
			defer wg.Done()
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

	// 2. Shrinking-only missing_ranges (P1.1 review O6):
	// Advance to max=15 with gap [11, 13].
	require.NoError(t, cs.UpsertMeterHealth(ctx, &store.MeterHealth{
		AgentID:           agentID,
		RunID:             runID,
		ProducerEpoch:     epoch,
		LastReceiptAt:     t0.Add(20 * time.Second),
		LastContiguousSeq: 10,
		MaxSeqSeen:        15,
		MissingRanges:     []store.SeqRange{{Start: 11, End: 13}},
	}))

	// Stale writer tries to grow missing_ranges to [9, 14] at max=14 — must NOT grow [11, 13].
	require.NoError(t, cs.UpsertMeterHealth(ctx, &store.MeterHealth{
		AgentID:           agentID,
		RunID:             runID,
		ProducerEpoch:     epoch,
		LastReceiptAt:     t0.Add(15 * time.Second),
		LastContiguousSeq: 8,
		MaxSeqSeen:        14,
		MissingRanges:     []store.SeqRange{{Start: 9, End: 14}},
	}))

	mhs, err = cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, mhs, 1)
	assert.Equal(t, int64(10), mhs[0].LastContiguousSeq)
	assert.Equal(t, int64(15), mhs[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 11, End: 13}}, mhs[0].MissingRanges, "stale writer must not grow missing_ranges")

	// Writer shrinks gap to [12, 12] -> allowed.
	require.NoError(t, cs.UpsertMeterHealth(ctx, &store.MeterHealth{
		AgentID:           agentID,
		RunID:             runID,
		ProducerEpoch:     epoch,
		LastReceiptAt:     t0.Add(25 * time.Second),
		LastContiguousSeq: 11,
		MaxSeqSeen:        15,
		MissingRanges:     []store.SeqRange{{Start: 12, End: 12}},
	}))
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
