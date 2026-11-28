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

	// 1. Pre-activation spend on Nov 10 08:00: A=300 (proj1), B=200 (proj2, charged only to gOther),
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
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ProducerEpoch: "ep-1", ProducerSeq: 1,
			},
			{
				HubID: "hub-1", EventID: "ep-ev-b", RunID: runID, ProjectID: proj2, AgentID: agentID,
				BillingUserID: &userB.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(150), Output: i64Ptr(50),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ProducerEpoch: "ep-1", ProducerSeq: 2,
				ChargedGroupIDs: []string{gOther},
			},
			{
				HubID: "hub-1", EventID: "ep-ev-anom", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userA.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(60_000_000),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tPre, ProducerEpoch: "ep-1", ProducerSeq: 3,
				Anomalous: true,
			},
			{
				HubID: "hub-1", EventID: "ep-ev-null", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userB.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1,
				TokenStatus: telemetrycontract.TokenStatusNotReported, EventTS: tPre, ProducerEpoch: "ep-1", ProducerSeq: 4,
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

	// Both month and day windows read the exact same episode seed rows (500 system; 300 proj1; 200 proj2).
	sumMonth, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(500), sumMonth)

	sumDay, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, "", nov10, nov11)
	require.NoError(t, err)
	assert.Equal(t, int64(500), sumDay)

	sumProj1, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, proj1, nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(300), sumProj1)

	sumProj2, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, proj2, nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(200), sumProj2)

	// 3. Reproduced 500 -> 500 regression (D6a / R1):
	//    A leaves G; then a second binding (day limit) activates Nov 10 16:00 while episode 1 is open.
	//    OpenUsageGroupChargingEpisode MUST return opened=false and leave total at 500 (never 200).
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
	assert.Equal(t, int64(500), sumMonthAfter, "leaver A's 300 must NEVER be refunded (stays 500)")

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
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tGap, ProducerEpoch: "ep-2", ProducerSeq: 1,
			},
			{
				HubID: "hub-1", EventID: "ep2-gap-c", RunID: runID, ProjectID: proj1, AgentID: agentID,
				BillingUserID: &userC.ID, Harness: "claude", Model: "claude-sonnet-4-6",
				ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: telemetrycontract.UsageSourceNative,
				Status: telemetrycontract.StatusSuccess, Calls: 1, Input: i64Ptr(50), Output: i64Ptr(30),
				TokenStatus: telemetrycontract.TokenStatusReported, EventTS: tGap, ProducerEpoch: "ep-2", ProducerSeq: 2,
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

	// Total = ep1 (500) + ep2 gap (200) = 700; proj1 = ep1(300) + ep2(200) = 500; proj2 = ep1(200).
	sumTotal, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, "", nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(700), sumTotal)

	sumProj1After, err := cs.SumUsageGroupRollupTokens(ctx, gTarget.ID, proj1, nov01, dec01)
	require.NoError(t, err)
	assert.Equal(t, int64(500), sumProj1After)

	// Breakdown across overlapping episodes sums to 700 (B=320, A=300, C=80).
	eps, err := cs.ListUsageGroupSeedEpisodesOverlapping(ctx, gTarget.ID, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, eps, 2)
	breakdown, err := cs.QueryGroupMemberUsageBreakdown(ctx, gTarget.ID, "", eps, nov01, dec01)
	require.NoError(t, err)
	require.Len(t, breakdown, 3)
	assert.Equal(t, userB.ID, breakdown[0].UserID)
	assert.Equal(t, int64(320), breakdown[0].TokensSummable)
	assert.Equal(t, userA.ID, breakdown[1].UserID)
	assert.Equal(t, int64(300), breakdown[1].TokensSummable)
	assert.Equal(t, userC.ID, breakdown[2].UserID)
	assert.Equal(t, int64(80), breakdown[2].TokensSummable)
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
