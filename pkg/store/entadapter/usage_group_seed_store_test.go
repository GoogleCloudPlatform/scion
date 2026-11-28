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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestGroupStore_GetEffectiveGroupMemberUserIDs(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	root := &store.Group{ID: uuid.NewString(), Name: "root", Slug: "root-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	child := &store.Group{ID: uuid.NewString(), Name: "child", Slug: "child-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	grand := &store.Group{ID: uuid.NewString(), Name: "grand", Slug: "grand-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	for _, g := range []*store.Group{root, child, grand} {
		require.NoError(t, cs.CreateGroup(ctx, g))
	}

	uDirect := &store.User{ID: uuid.NewString(), Email: "direct@example.com", Role: store.UserRoleMember, Status: "active"}
	uNested := &store.User{ID: uuid.NewString(), Email: "nested@example.com", Role: store.UserRoleMember, Status: "active"}
	uGrand := &store.User{ID: uuid.NewString(), Email: "grand@example.com", Role: store.UserRoleMember, Status: "active"}
	uExpired := &store.User{ID: uuid.NewString(), Email: "expired@example.com", Role: store.UserRoleMember, Status: "active"}
	for _, u := range []*store.User{uDirect, uNested, uGrand, uExpired} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}

	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: root.ID, MemberType: store.GroupMemberTypeGroup, MemberID: child.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: child.ID, MemberType: store.GroupMemberTypeGroup, MemberID: grand.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: root.ID, MemberType: store.GroupMemberTypeUser, MemberID: uDirect.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: child.ID, MemberType: store.GroupMemberTypeUser, MemberID: uNested.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: grand.ID, MemberType: store.GroupMemberTypeUser, MemberID: uGrand.ID, Role: store.GroupMemberRoleMember}))
	// Also add uDirect to grand to verify deduplication.
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: grand.ID, MemberType: store.GroupMemberTypeUser, MemberID: uDirect.ID, Role: store.GroupMemberRoleMember}))
	// Expired membership must be excluded.
	past := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: child.ID, MemberType: store.GroupMemberTypeUser, MemberID: uExpired.ID, Role: store.GroupMemberRoleMember, ExpiresAt: &past}))

	got, err := cs.GetEffectiveGroupMemberUserIDs(ctx, root.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{uDirect.ID, uNested.ID, uGrand.ID}, got)
	assert.NotContains(t, got, uExpired.ID)
}

func TestUsageGroupSeedStore_ComputeAndReplaceIdempotent(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	runID := "run-seed-" + uuid.NewString()
	proj1 := uuid.NewString()
	proj2 := uuid.NewString()
	agentID := uuid.NewString()
	userA := uuid.NewString()
	userB := uuid.NewString()
	gTarget := uuid.NewString()
	gOther := uuid.NewString()

	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		AgentID:       agentID,
		ProjectID:     proj1,
		BillingUserID: &userA,
		Harness:       "claude",
		CreatedAt:     time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	d1 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			// Uncharged (nil charged_group_ids) -> included in seed.
			{
				HubID: "hub-1", EventID: "seed-ev-1", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(300), Output: i64Ptr(100),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: d1, ProducerEpoch: "ep-1", ProducerSeq: 1,
			},
			// Charged only to gOther (NOT gTarget) -> included in seed for gTarget.
			{
				HubID: "hub-1", EventID: "seed-ev-2", RunID: runID, ProjectID: proj2, AgentID: agentID,
				BillingUserID: &userB, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(200), Output: i64Ptr(50),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: d2, ProducerEpoch: "ep-1", ProducerSeq: 2,
				ChargedGroupIDs: []string{gOther},
			},
			// Already charged to gTarget -> MUST be excluded from seed (already in origin='ingest').
			{
				HubID: "hub-1", EventID: "seed-ev-3", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(500), Output: i64Ptr(200),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: d2, ProducerEpoch: "ep-1", ProducerSeq: 3,
				ChargedGroupIDs: []string{gTarget},
			},
			// Anomalous uncharged event -> tokens excluded from seed.
			{
				HubID: "hub-1", EventID: "seed-ev-anom", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(60_000_000),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: d1, ProducerEpoch: "ep-1", ProducerSeq: 4,
				Anomalous: true,
			},
		},
	})
	require.NoError(t, err)

	nov01 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dec01 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	// System-wide uncharged seed: 400 (d1) + 250 (d2) = 650 (seed-ev-3 excluded, anomalous tokens excluded).
	daysSys, err := cs.ComputeUnchargedGroupRollupSeed(ctx, gTarget, "", []string{userA, userB}, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, daysSys, 2)
	assert.Equal(t, int64(400), daysSys[0].TokensSummable)
	assert.Equal(t, int64(250), daysSys[1].TokensSummable)

	// Project-scoped (proj1) uncharged seed: 400 (d1 only; proj2 excluded).
	daysProj1, err := cs.ComputeUnchargedGroupRollupSeed(ctx, gTarget, proj1, []string{userA, userB}, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, daysProj1, 1)
	assert.Equal(t, int64(400), daysProj1[0].TokensSummable)

	// ReplaceUsageGroupRollupSeed is idempotent under re-run and concurrency, and never touches origin='ingest' (700).
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- cs.ReplaceUsageGroupRollupSeed(ctx, gTarget, "", nov01, dec01, daysSys)
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}

	// Total = seed (650) + ingest (700) = 1350 (zero double-counting).
	totalSys, err := cs.SumUsageGroupRollupTokens(ctx, gTarget, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(1350), totalSys)

	// Breakdown combines ingest (userA: 700) + seed (userA: 400, userB: 250) -> userA=1100, userB=250.
	breakdown, err := cs.QueryGroupMemberUsageBreakdown(ctx, gTarget, "", []string{userA, userB}, nov01, dec01, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, breakdown, 2)
	assert.Equal(t, userA, breakdown[0].UserID)
	assert.Equal(t, int64(1100), breakdown[0].TokensSummable)
	assert.Equal(t, userB, breakdown[1].UserID)
	assert.Equal(t, int64(250), breakdown[1].TokensSummable)
}

func TestUsageGroupSeedStore_SeedStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	empty, err := cs.GetUsageGroupSeedState(ctx)
	require.NoError(t, err)
	assert.Empty(t, empty.Records)

	t0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	st := &store.UsageGroupSeedState{
		Records: map[string]*store.UsageGroupSeedRecord{
			"g1||2026-11-01": {
				GroupID:           "g1",
				ProjectScopeID:    "",
				WindowStartUTC:    time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
				WindowEndUTC:      time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
				ActivatedAt:       t0,
				CutoffAt:          t0.Add(2 * time.Minute),
				FrozenMemberIDs:   []string{"u1", "u2"},
				MemberSnapshotSet: true,
				Finalized:         true,
				UpdatedAt:         t0.Add(2 * time.Minute),
			},
		},
	}
	require.NoError(t, cs.SaveUsageGroupSeedState(ctx, st))

	got, err := cs.GetUsageGroupSeedState(ctx)
	require.NoError(t, err)
	rec := got.Records["g1||2026-11-01"]
	require.NotNil(t, rec)
	assert.True(t, rec.Finalized)
	assert.Equal(t, []string{"u1", "u2"}, rec.FrozenMemberIDs)
}
