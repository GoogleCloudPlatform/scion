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

	day1 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
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

	// Duplicate replay should not change rollups or presence counts.
	resDup, err := cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events:  []*store.UsageEvent{ev1},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, resDup.Accepted)
	assert.Equal(t, 1, resDup.Duplicates)

	// ListUsageRollupsDailyByAgent full window
	rollups, err := cs.ListUsageRollupsDailyByAgent(ctx, agentID, day1, day2)
	require.NoError(t, err)
	require.Len(t, rollups, 2)
	assert.Equal(t, "claude-sonnet-4-6", rollups[0].Model)
	assert.Equal(t, int64(8), rollups[0].Input)
	assert.Equal(t, int64(5), rollups[0].Output)
	assert.Equal(t, int64(3), rollups[0].CacheRead)
	assert.Equal(t, int64(2), rollups[0].CacheWrite)
	assert.Equal(t, int64(2), rollups[0].Reasoning)
	assert.Equal(t, "claude-haiku-4-5", rollups[1].Model)

	// Narrow window (day1 only)
	rollupsDay1, err := cs.ListUsageRollupsDailyByAgent(ctx, agentID, day1, day1)
	require.NoError(t, err)
	require.Len(t, rollupsDay1, 1)
	assert.Equal(t, "claude-sonnet-4-6", rollupsDay1[0].Model)

	// GetUsageTokenPresenceByAgent distinguishes measured 0 vs NULL
	presence, err := cs.GetUsageTokenPresenceByAgent(ctx, agentID, day1.Add(-time.Hour), day2.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, presence, 2)
	// Sorted by model ASC: claude-haiku-4-5 then claude-sonnet-4-6
	assert.Equal(t, "claude-haiku-4-5", presence[0].Model)
	assert.Equal(t, int64(1), presence[0].CacheReadCount)  // measured 0
	assert.Equal(t, int64(0), presence[0].CacheWriteCount) // NULL
	assert.Equal(t, int64(0), presence[0].ReasoningCount)  // NULL

	assert.Equal(t, "claude-sonnet-4-6", presence[1].Model)
	assert.Equal(t, int64(1), presence[1].InputCount)
	assert.Equal(t, int64(1), presence[1].ReasoningCount)
	assert.Equal(t, int64(0), presence[1].CacheWrite1hCount) // NULL

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
