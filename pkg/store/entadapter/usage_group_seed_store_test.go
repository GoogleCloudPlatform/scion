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
	"sync/atomic"
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

func TestUsageGroupSeedStore_EpisodeLifecycleAndDayMonthNoCollision(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	proj1 := uuid.NewString()
	proj2 := uuid.NewString()
	agentID := uuid.NewString()
	runID := "run-ep-" + uuid.NewString()

	gTarget := &store.Group{ID: uuid.NewString(), Name: "g-target", Slug: "gt-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, cs.CreateGroup(ctx, gTarget))
	gOther := uuid.NewString()

	userA := &store.User{ID: uuid.NewString(), Email: "ep-a@example.com", Role: store.UserRoleMember, Status: "active"}
	userB := &store.User{ID: uuid.NewString(), Email: "ep-b@example.com", Role: store.UserRoleMember, Status: "active"}
	userC := &store.User{ID: uuid.NewString(), Email: "ep-c@example.com", Role: store.UserRoleMember, Status: "active"}
	for _, u := range []*store.User{userA, userB, userC} {
		require.NoError(t, cs.CreateUser(ctx, u))
	}
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: gTarget.ID, MemberType: store.GroupMemberTypeUser, MemberID: userA.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: gTarget.ID, MemberType: store.GroupMemberTypeUser, MemberID: userB.ID, Role: store.GroupMemberRoleMember}))

	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: runID, AgentID: agentID, ProjectID: proj1, BillingUserID: &userA.ID, Harness: "claude",
		CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	// 1. Pre-activation spend on Nov 10 08:00: A=300 (proj1, uncharged),
	//    B=200 (proj2, charged only to gOther -> uncharged for gTarget),
	//    A=175 (proj1, ALREADY charged to gTarget -> O6 must be excluded from seed),
	//    plus an anomalous event and a NULL-token event (must coalesce to 0).
	tPre := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "ep-ev-a", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(200), Output: i64Ptr(100),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ReceivedAt: tPre, ProducerEpoch: "ep-1", ProducerSeq: 1,
			},
			{
				HubID: "hub-1", EventID: "ep-ev-b", RunID: runID, ProjectID: proj2, AgentID: agentID,
				BillingUserID: &userB.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(150), Output: i64Ptr(50),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ReceivedAt: tPre, ProducerEpoch: "ep-1", ProducerSeq: 2,
				ChargedGroupIDs: []string{gOther},
			},
			{
				HubID: "hub-1", EventID: "ep-ev-already-charged", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(125), Output: i64Ptr(50),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ReceivedAt: tPre, ProducerEpoch: "ep-1", ProducerSeq: 5,
				ChargedGroupIDs: []string{gTarget.ID},
			},
			{
				HubID: "hub-1", EventID: "ep-ev-anom", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(60_000_000),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ReceivedAt: tPre, ProducerEpoch: "ep-1", ProducerSeq: 3,
				Anomalous: true,
			},
			{
				HubID: "hub-1", EventID: "ep-ev-null", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userB.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1,
				TokenStatus: telemetrycontract.TokenStatusNotReported, EventTS: tPre, ReceivedAt: tPre, ProducerEpoch: "ep-1", ProducerSeq: 4,
			},
		},
	})
	require.NoError(t, err)

	// 2. Episode 1 opens Nov 10 10:00 (e.g. month binding activates), and finalizes at cutoff.
	tAct1 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	ep1, opened, err := cs.OpenUsageGroupChargingEpisode(ctx, gTarget.ID, tAct1)
	require.NoError(t, err)
	require.True(t, opened)
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), ep1.SeedFromUTC)
	assert.Equal(t, tAct1.Add(2*time.Minute), ep1.CutoffAt)

	res1, err := cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: ep1.ID, GroupID: gTarget.ID, Now: tAct1.Add(2 * time.Minute), IsFinalPass: true,
	})
	require.NoError(t, err)
	assert.True(t, res1.DidMutate)
	assert.True(t, res1.Episode.Finalized)
	assert.ElementsMatch(t, []string{proj1, proj2}, res1.TouchedProjectScope)

	nov01 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dec01 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	nov10 := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	nov11 := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)

	// Both month and day windows read the exact same episode seed (500) + ingest (175) = 675
	// (O6: ep-ev-already-charged MUST NOT be double-counted by the seed).
	sumMonth, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(675), sumMonth)

	sumDay, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, "", nov10, nov11)
	require.NoError(t, err)
	assert.Equal(t, int64(675), sumDay)

	sumProj1, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, proj1, nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(475), sumProj1)

	sumProj2, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, proj2, nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(200), sumProj2)

	// O3: Late uncharged event uploaded AFTER episode 1 finalized (EventTS=tPre, ReceivedAt=tAct1+10m)
	// must be excluded from QueryGroupMemberUsageBreakdown so breakdown matches the pool total.
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "ep-ev-late-after-finalize", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(999), Output: i64Ptr(1),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ReceivedAt: tAct1.Add(10 * time.Minute),
				ProducerEpoch: "ep-late", ProducerSeq: 1,
			},
		},
	})
	require.NoError(t, err)

	// 3. Reproduced 500 -> 500 regression (D6a / R1):
	//    A leaves G; then a second binding (day limit) activates Nov 10 16:00 while episode 1 is open.
	//    OpenUsageGroupChargingEpisode MUST return opened=false and leave total at 675.
	require.NoError(t, cs.RemoveGroupMember(ctx, gTarget.ID, store.GroupMemberTypeUser, userA.ID))
	epSame, openedSecond, err := cs.OpenUsageGroupChargingEpisode(ctx, gTarget.ID, time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.False(t, openedSecond, "second binding inside an open episode must NOT open a new episode")
	assert.Equal(t, ep1.ID, epSame.ID)

	// Re-running finalized episode 1 is a no-op.
	resNoop, err := cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: ep1.ID, GroupID: gTarget.ID, Now: time.Date(2026, 11, 10, 16, 5, 0, 0, time.UTC), IsFinalPass: true,
	})
	require.NoError(t, err)
	assert.False(t, resNoop.DidMutate)

	sumMonthAfter, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(675), sumMonthAfter, "leaver A's spend must NEVER be refunded")

	// 4. Deactivate -> gap spend -> reactivate (Episode 2):
	//    Close episode 1 at Nov 10 18:00 (no active bindings exist in store).
	tEnd1 := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	closedEp, didClose, err := cs.CloseUsageGroupChargingEpisodeIfInactive(ctx, gTarget.ID, tEnd1)
	require.NoError(t, err)
	require.True(t, didClose)
	require.NotNil(t, closedEp.EndedAt)
	assert.Equal(t, tEnd1, *closedEp.EndedAt)

	// C joins G during the gap; B spends 120 and C spends 80 at Nov 10 19:00 (uncharged because group inactive).
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: gTarget.ID, MemberType: store.GroupMemberTypeUser, MemberID: userC.ID, Role: store.GroupMemberRoleMember}))
	tGap := time.Date(2026, 11, 10, 19, 0, 0, 0, time.UTC)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "ep2-gap-b", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userB.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(100), Output: i64Ptr(20),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tGap, ReceivedAt: tGap, ProducerEpoch: "ep-2", ProducerSeq: 1,
			},
			{
				HubID: "hub-1", EventID: "ep2-gap-c", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userC.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(50), Output: i64Ptr(30),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tGap, ReceivedAt: tGap, ProducerEpoch: "ep-2", ProducerSeq: 2,
			},
		},
	})
	require.NoError(t, err)

	// Episode 2 opens at Nov 10 20:00 -> seed_from_utc == tEnd1 (only seeds the gap, never touches ep1's rows!).
	tAct2 := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	ep2, opened2, err := cs.OpenUsageGroupChargingEpisode(ctx, gTarget.ID, tAct2)
	require.NoError(t, err)
	require.True(t, opened2)
	assert.NotEqual(t, ep1.ID, ep2.ID)
	assert.Equal(t, tEnd1, ep2.SeedFromUTC, "episode 2 must seed only from previous episode's ended_at")

	res2, err := cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: ep2.ID, GroupID: gTarget.ID, Now: tAct2.Add(2 * time.Minute), IsFinalPass: true,
	})
	require.NoError(t, err)
	assert.True(t, res2.DidMutate)
	assert.True(t, res2.Episode.Finalized)

	// Total = ep1 (500 seed + 175 ingest = 675) + ep2 gap (200) = 875; proj1 = 475 + 200 = 675; proj2 = 200.
	sumTotal, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(875), sumTotal)

	sumProj1After, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, proj1, nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(675), sumProj1After)

	// Breakdown across overlapping episodes sums to 875 (A=475, B=320, C=80); late 1000-token event excluded (O3).
	eps, err := cs.ListUsageGroupSeedEpisodesOverlapping(ctx, gTarget.ID, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, eps, 2)
	breakdown, err := cs.QueryGroupMemberUsageBreakdown(ctx, gTarget.ID, "", eps, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, breakdown, 3)
	assert.Equal(t, userA.ID, breakdown[0].UserID)
	assert.Equal(t, int64(475), breakdown[0].TokensSummable)
	assert.Equal(t, userB.ID, breakdown[1].UserID)
	assert.Equal(t, int64(320), breakdown[1].TokensSummable)
	assert.Equal(t, userC.ID, breakdown[2].UserID)
	assert.Equal(t, int64(80), breakdown[2].TokensSummable)
}

func TestUsageGroupSeedStore_CloseBeforeCutoffNoDoubleSeed(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	proj := uuid.NewString()
	agentID := uuid.NewString()
	runID := "run-r1-" + uuid.NewString()
	userA := &store.User{ID: uuid.NewString(), Email: "r1-close@example.com", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, userA))

	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: runID, AgentID: agentID, ProjectID: proj, BillingUserID: &userA.ID, Harness: "claude",
		CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	nov01 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dec01 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	// Variant A: close at T+30s, uncharged 1000-token event at T+60s, reopen day+10.
	gA := &store.Group{ID: uuid.NewString(), Name: "r1-ga", Slug: "r1-ga-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, cs.CreateGroup(ctx, gA))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: gA.ID, MemberType: store.GroupMemberTypeUser, MemberID: userA.ID, Role: store.GroupMemberRoleMember}))

	t0 := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ep1, opened, err := cs.OpenUsageGroupChargingEpisode(ctx, gA.ID, t0)
	require.NoError(t, err)
	require.True(t, opened)

	_, closed, err := cs.CloseUsageGroupChargingEpisodeIfInactive(ctx, gA.ID, t0.Add(30*time.Second))
	require.NoError(t, err)
	require.True(t, closed)

	tEv := t0.Add(60 * time.Second)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "r1-ev-a", RunID: runID, ProjectID: proj, AgentID: agentID,
				BillingUserID: &userA.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(700), Output: i64Ptr(300),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tEv, ReceivedAt: tEv,
				ProducerEpoch: "ep-r1-a", ProducerSeq: 1,
			},
		},
	})
	require.NoError(t, err)

	_, err = cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: ep1.ID, GroupID: gA.ID, Now: t0.Add(2 * time.Minute), IsFinalPass: true,
	})
	require.NoError(t, err)

	ep2, opened2, err := cs.OpenUsageGroupChargingEpisode(ctx, gA.ID, t0.AddDate(0, 0, 10))
	require.NoError(t, err)
	require.True(t, opened2)
	assert.Equal(t, ep1.CutoffAt, ep2.SeedFromUTC, "R1: seed_from_utc must be at least prev.cutoff_at")

	_, err = cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: ep2.ID, GroupID: gA.ID, Now: t0.AddDate(0, 0, 10).Add(2 * time.Minute), IsFinalPass: true,
	})
	require.NoError(t, err)

	poolSumA, err := cs.SumUsageGroupRollupTokens(ctx, gA.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), poolSumA, "R1: pool must not double-count [ended_at, cutoff_at)")

	epsA, err := cs.ListUsageGroupSeedEpisodesOverlapping(ctx, gA.ID, nov01, dec01)
	require.NoError(t, err)
	bdA, err := cs.QueryGroupMemberUsageBreakdown(ctx, gA.ID, "", epsA, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, bdA, 1)
	assert.Equal(t, poolSumA, bdA[0].TokensSummable, "R1: pool total must equal breakdown sum")

	// Variant B: toggle-back-within-2m (close at T+30s, reopen at T+90s < prev.cutoff_at = T+120s).
	gB := &store.Group{ID: uuid.NewString(), Name: "r1-gb", Slug: "r1-gb-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, cs.CreateGroup(ctx, gB))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: gB.ID, MemberType: store.GroupMemberTypeUser, MemberID: userA.ID, Role: store.GroupMemberRoleMember}))

	epB1, _, err := cs.OpenUsageGroupChargingEpisode(ctx, gB.ID, t0)
	require.NoError(t, err)
	_, closedB, err := cs.CloseUsageGroupChargingEpisodeIfInactive(ctx, gB.ID, t0.Add(30*time.Second))
	require.NoError(t, err)
	require.True(t, closedB)

	epB2, openedB2, err := cs.OpenUsageGroupChargingEpisode(ctx, gB.ID, t0.Add(90*time.Second))
	require.NoError(t, err)
	require.True(t, openedB2)
	assert.Equal(t, epB1.CutoffAt, epB2.SeedFromUTC, "R1 toggle-within-2m: ep2 seed_from_utc must be prev.cutoff_at")

	_, err = cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: epB1.ID, GroupID: gB.ID, Now: t0.Add(2 * time.Minute), IsFinalPass: true,
	})
	require.NoError(t, err)
	_, err = cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: epB2.ID, GroupID: gB.ID, Now: t0.Add(4 * time.Minute), IsFinalPass: true,
	})
	require.NoError(t, err)

	poolSumB, err := cs.SumUsageGroupRollupTokens(ctx, gB.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), poolSumB, "R1 toggle-within-2m: must not double-count")

	epsB, err := cs.ListUsageGroupSeedEpisodesOverlapping(ctx, gB.ID, nov01, dec01)
	require.NoError(t, err)
	bdB, err := cs.QueryGroupMemberUsageBreakdown(ctx, gB.ID, "", epsB, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, bdB, 1)
	assert.Equal(t, poolSumB, bdB[0].TokensSummable)
}

func TestUsageGroupSeedStore_LargeMemberSetExceedsSQLiteParamLimit(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	proj := uuid.NewString()
	agentID := uuid.NewString()
	runID := "run-large-" + uuid.NewString()
	realUser := uuid.NewString()
	gLarge := &store.Group{ID: uuid.NewString(), Name: "g-large", Slug: "gl-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, cs.CreateGroup(ctx, gLarge))

	tPre := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	tAct := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	_, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: runID, AgentID: agentID, ProjectID: proj, BillingUserID: &realUser, Harness: "claude", CreatedAt: tPre,
	})
	require.NoError(t, err)
	_, err = cs.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{
			{
				HubID: "hub-1", EventID: "large-ev-1", RunID: runID, ProjectID: proj, AgentID: agentID,
				BillingUserID: &realUser, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(300), Output: i64Ptr(150),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ReceivedAt: tPre,
				ProducerEpoch: "ep-large", ProducerSeq: 1,
			},
		},
	})
	require.NoError(t, err)

	// Build a 35,000-member snapshot (> modernc SQLite's 32,766 SQLITE_MAX_VARIABLE_NUMBER
	// parameter limit) and store it on an open episode so an unbounded IN (?, ...)
	// query would fail with "too many SQL variables".
	const memberCount = 35000
	members := make([]string, memberCount)
	members[0] = realUser
	for i := 1; i < memberCount; i++ {
		members[i] = uuid.NewString()
	}

	ep, err := client.UsageGroupBudgetSeed.Create().
		SetID(uuid.New()).
		SetGroupID(gLarge.ID).
		SetActivatedAt(tAct).
		SetSeedFromUtc(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)).
		SetCutoffAt(tAct.Add(2 * time.Minute)).
		SetFrozenMemberIds(members).
		SetMemberSnapshotSet(true).
		SetFinalized(false).
		SetStateVersion(1).
		Save(ctx)
	require.NoError(t, err)

	res, err := cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
		EpisodeID: ep.ID.String(), GroupID: gLarge.ID, Now: tAct.Add(2 * time.Minute), IsFinalPass: true,
	})
	require.NoError(t, err, "O2: seed pass must handle >32766 frozen members without SQL parameter overflow")
	assert.True(t, res.DidMutate)

	nov01 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dec01 := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	sum, err := cs.SumUsageGroupRollupTokens(ctx, gLarge.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(450), sum)

	eps, err := cs.ListUsageGroupSeedEpisodesOverlapping(ctx, gLarge.ID, nov01, dec01)
	require.NoError(t, err)
	bd, err := cs.QueryGroupMemberUsageBreakdown(ctx, gLarge.ID, "", eps, nov01, dec01)
	require.NoError(t, err, "O2: breakdown query must handle >32766 frozen members without SQL parameter overflow")
	require.Len(t, bd, 1)
	assert.Equal(t, realUser, bd[0].UserID)
	assert.Equal(t, int64(450), bd[0].TokensSummable)
}

func TestUsageGroupSeedStore_ConcurrentOpenAndPasses(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	g1 := &store.Group{ID: uuid.NewString(), Name: "g1", Slug: "g1-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	g2 := &store.Group{ID: uuid.NewString(), Name: "g2", Slug: "g2-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, cs.CreateGroup(ctx, g1))
	require.NoError(t, cs.CreateGroup(ctx, g2))

	u1 := &store.User{ID: uuid.NewString(), Email: "u1-ep@example.com", Role: store.UserRoleMember, Status: "active"}
	u2 := &store.User{ID: uuid.NewString(), Email: "u2-ep@example.com", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, cs.CreateUser(ctx, u1))
	require.NoError(t, cs.CreateUser(ctx, u2))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: g1.ID, MemberType: store.GroupMemberTypeUser, MemberID: u1.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, cs.AddGroupMember(ctx, &store.GroupMember{GroupID: g2.ID, MemberType: store.GroupMemberTypeUser, MemberID: u2.ID, Role: store.GroupMemberRoleMember}))

	tAct := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

	// Concurrent OpenUsageGroupChargingEpisode for g1: exactly one caller opens it.
	var openedCount atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			_, opened, err := cs.OpenUsageGroupChargingEpisode(ctx, g1.ID, tAct.Add(time.Duration(offset)*time.Second))
			if opened {
				openedCount.Add(1)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	assert.Equal(t, int32(1), openedCount.Load(), "concurrent opens for one group must create exactly one open episode")

	ep1, err := cs.GetOpenUsageGroupChargingEpisode(ctx, g1.ID)
	require.NoError(t, err)
	ep2, opened2, err := cs.OpenUsageGroupChargingEpisode(ctx, g2.ID, tAct)
	require.NoError(t, err)
	require.True(t, opened2)

	// Concurrent seed passes across both groups and same episode serialize cleanly.
	errs2 := make(chan error, 6)
	for i := 0; i < 3; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, e := cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
				EpisodeID: ep1.ID, GroupID: g1.ID, Now: tAct.Add(2 * time.Minute), IsFinalPass: true,
			})
			errs2 <- e
		}()
		go func() {
			defer wg.Done()
			_, e := cs.ExecuteUsageGroupEpisodeSeedPass(ctx, store.UsageGroupEpisodeSeedPassInput{
				EpisodeID: ep2.ID, GroupID: g2.ID, Now: tAct.Add(2 * time.Minute), IsFinalPass: true,
			})
			errs2 <- e
		}()
	}
	wg.Wait()
	close(errs2)
	for e := range errs2 {
		require.NoError(t, e)
	}

	got1, err := cs.GetUsageGroupSeedEpisode(ctx, ep1.ID)
	require.NoError(t, err)
	assert.True(t, got1.Finalized)
	assert.Equal(t, []string{u1.ID}, got1.FrozenMemberIDs)

	got2, err := cs.GetUsageGroupSeedEpisode(ctx, ep2.ID)
	require.NoError(t, err)
	assert.True(t, got2.Finalized)
	assert.Equal(t, []string{u2.ID}, got2.FrozenMemberIDs)
}
