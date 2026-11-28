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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func doUserGETRequest(t *testing.T, srv *Server, user *store.User, path string) *httptest.ResponseRecorder {
	t.Helper()
	tok, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func doScopedUserGETRequest(
	t *testing.T,
	srv *Server,
	user *store.User,
	boundary TokenBoundary,
	scopes []string,
	permIDs []string,
	groupID string,
) *httptest.ResponseRecorder {
	t.Helper()
	baseUser := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, string(ClientTypeWeb))
	scoped := NewScopedUserIdentityWithBoundary(
		baseUser,
		boundary,
		scopes,
		"uat-test-"+user.ID,
		permissions.FrozenPermissionCeiling{
			Version:       permissions.CeilingVersionV1,
			PermissionIDs: permIDs,
		},
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/groups/"+groupID+"/usage/budgets", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), scoped))
	rec := httptest.NewRecorder()
	srv.handleGroupUsageBudgets(rec, req, groupID)
	return rec
}

// drainGroupSeedQueueAt claims all ready usage.group_budget_seed items from
// durable_queue_items at clock `at`, dispatches each through seeder.HandleQueueItem,
// and acknowledges completed items. Returns the number of drained items.
func drainGroupSeedQueueAt(t *testing.T, ctx context.Context, st store.Store, seeder *GroupBudgetSeeder, at time.Time) int {
	t.Helper()
	dqs, ok := st.(store.DurableQueueStore)
	require.True(t, ok, "store must implement DurableQueueStore")
	drained := 0
	for {
		claimed, err := dqs.ClaimDurableItems(ctx, usageGroupBudgetSeedQueueKind, "test-drainer", at, at.Add(60*time.Second), 10)
		require.NoError(t, err)
		if len(claimed) == 0 {
			break
		}
		for _, rec := range claimed {
			item := durablequeue.Item{
				Seq:       rec.Seq,
				Kind:      rec.Kind,
				DedupKey:  rec.DedupKey,
				ProjectID: rec.ProjectID,
				Payload:   rec.Payload,
				NotBefore: rec.NotBefore,
			}
			res := seeder.HandleQueueItem(ctx, item, nil)
			require.True(t, res.Done, "HandleQueueItem failed: %v", res.Err)
			completed, err := dqs.CompleteDurableItem(ctx, rec.Seq, "test-drainer")
			require.NoError(t, err)
			require.True(t, completed)
			drained++
		}
	}
	return drained
}

func TestPooledGroupSeeder_MidMonthD6AndIdempotency(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)
	// Pause the wall-clock background drainer so this test deterministically
	// drains the durable_queue_items at controlled clock steps (tAct, tAct+2m).
	srv.stopGroupBudgetSeedDrainer(ctx)

	// Deterministic clock (O5): no time.Now() anywhere in this test.
	tPre := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	tAct := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	now := tPre
	srv.TokenBudgetEvaluator().nowFn = func() time.Time { return now }
	srv.PooledGroupResolver().nowFn = func() time.Time { return now }
	seeder := srv.GroupBudgetSeeder()
	require.NotNil(t, seeder)
	seeder.nowFn = func() time.Time { return now }

	proj := &store.Project{ID: uuid.NewString(), Name: "p-seed", Slug: "p-seed"}
	require.NoError(t, st.CreateProject(ctx, proj))

	groupG := createTestGroupForBudgets(t, st, "seed-g", store.GroupTypeExplicit, "")
	childG := createTestGroupForBudgets(t, st, "seed-child", store.GroupTypeExplicit, "")

	userA := createTestUserForBudgets(t, st, "seed-a@example.com")
	userB := createTestUserForBudgets(t, st, "seed-b@example.com")
	userC := createTestUserForBudgets(t, st, "seed-c@example.com")

	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userA.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeGroup, MemberID: childG.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: childG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userB.ID, Role: store.GroupMemberRoleMember}))

	agA, runA, tokA := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "ag-seed-a", userA.ID, tPre)
	agB, runB, tokB := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "ag-seed-b", userB.ID, tPre)
	agC, runC, tokC := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "ag-seed-c", userC.ID, tPre)

	hubID := srv.config.HubID

	// 1. Pre-activation spend (Nov 5): A=300, B=250 (nested member), C=400 (non-member).
	for _, item := range []struct {
		agID, runID, tok, evID string
		in, out                int64
	}{
		{agA.ID, runA, tokA, "pre-a", 200, 100},
		{agB.ID, runB, tokB, "pre-b", 150, 100},
		{agC.ID, runC, tokC, "pre-c", 300, 100},
	} {
		ev := e2eMakeWireUsageEvent(hubID, item.agID, proj.ID, item.runID, item.evID, 1, tPre, item.in, item.out)
		rec := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+item.agID+"/usage/events", item.tok, telemetrycontract.UsageIngestRequest{
			RunID: item.runID, ProducerEpoch: "ep-1", ProducerSeq: 1, Events: []telemetrycontract.UsageEvent{ev},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	// 2. Mid-month (Nov 15): create pooled monthly binding (cap = 1000) via HTTP POST.
	//    This records the activation event and enqueues initial (tAct) + ttl_cover (tAct+2m) items in durable_queue_items.
	now = tAct
	monthDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupMonth)
	require.NoError(t, err)
	recBind := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+monthDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       1000,
	})
	require.Equal(t, http.StatusCreated, recBind.Code, recBind.Body.String())
	var createdBind store.EntitlementBinding
	require.NoError(t, json.Unmarshal(recBind.Body.Bytes(), &createdBind))

	// 3. Concurrent ingest right after binding creation: A ingests 200 tokens (charged via origin='ingest').
	evPostA := e2eMakeWireUsageEvent(hubID, agA.ID, proj.ID, runA, "post-a", 2, tAct.Add(10*time.Second), 120, 80)
	recPostA := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agA.ID+"/usage/events", tokA, telemetrycontract.UsageIngestRequest{
		RunID: runA, ProducerEpoch: "ep-1", ProducerSeq: 2, Events: []telemetrycontract.UsageEvent{evPostA},
	})
	require.Equal(t, http.StatusOK, recPostA.Code, recPostA.Body.String())

	monthStart, monthEnd := utcMonthWindow(tAct)

	// 4. O5: Drain immediate seed pass from durable_queue_items via HandleQueueItem.
	//    Only the initial pass (NotBefore == tAct) is ready at tAct; ttl_cover (tAct+2m) waits.
	drainedInitial := drainGroupSeedQueueAt(t, ctx, st, seeder, tAct)
	assert.Equal(t, 1, drainedInitial, "only the immediate pass should be ready at tAct")

	// Idempotent direct re-run of initial pass produces identical total.
	require.NoError(t, seeder.RunSeed(ctx, GroupBudgetSeedPayload{
		GroupID:        groupG.ID,
		ProjectScopeID: "",
		WindowKind:     store.UsageBudgetWindowMonth,
		WindowStartUTC: monthStart,
		WindowEndUTC:   monthEnd,
		ActivatedAt:    tAct,
		IsFinalPass:    false,
	}))

	total, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(750), total, "must equal pre-activation seed (550) + concurrent ingest (200) without double counting")

	stRow, err := st.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID,
		ScopeKind:         store.UsageScopeKindGroup,
		ScopeID:           groupG.ID,
		ProjectScopeID:    "",
		WindowStartUTC:    monthStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, stRow.State)
	assert.Equal(t, int64(750), stRow.ConsumedAtEval)

	// 5. +2m: Drain the delayed ttl_cover pass from durable_queue_items -> marks seed finalized (D6).
	now = tAct.Add(2 * time.Minute)
	drainedFinal := drainGroupSeedQueueAt(t, ctx, st, seeder, now)
	assert.Equal(t, 1, drainedFinal, "delayed +2m ttl_cover pass should drain at tAct+2m")

	seedRec, err := st.GetUsageGroupSeedRecord(ctx, groupG.ID, "", store.UsageBudgetWindowMonth, monthStart)
	require.NoError(t, err)
	assert.True(t, seedRec.Finalized)
	assert.ElementsMatch(t, []string{userA.ID, userB.ID}, seedRec.FrozenMemberIDs)

	// 6. R1 regression test:
	//    After finalization: A leaves G, C (who had 400 pre-activation spend) joins G.
	//    Then an admin edits the binding cap (1000 -> 5000) and token_action (warn_only -> deny_new_runs),
	//    which bumps binding.UpdatedAt.
	//    Neither nightly reconcile nor any re-run may refund A or charge C's pre-join 400!
	require.NoError(t, st.RemoveGroupMember(ctx, groupG.ID, store.GroupMemberTypeUser, userA.ID))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userC.ID, Role: store.GroupMemberRoleMember}))
	srv.InvalidatePooledGroupCacheAll()

	now = tAct.Add(1 * time.Hour)
	recEdit := doAdminJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+createdBind.ID, map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       5000,
		"tokenAction": "pause_new",
	})
	require.Equal(t, http.StatusOK, recEdit.Code, recEdit.Body.String())

	// No queue items were enqueued by the ordinary cap/action edit.
	assert.Equal(t, 0, drainGroupSeedQueueAt(t, ctx, st, seeder, now.Add(5*time.Minute)))

	enq, err := seeder.ReconcileMissingSeeds(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, enq, "cap/token_action edit after finalization must NOT trigger reconcile re-seeding (R1)")

	totalAfter, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(750), totalAfter, "finalized seed must preserve A's spend and exclude C's pre-join spend (never 850)")

	// 7. Genuine 0 -> >0 re-activation DOES record a new activation event and enqueue new seed passes.
	now = tAct.Add(2 * time.Hour)
	recDisable := doAdminJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+createdBind.ID, map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       0,
		"tokenAction": "notify_only",
	})
	require.Equal(t, http.StatusOK, recDisable.Code, recDisable.Body.String())
	assert.Equal(t, 0, drainGroupSeedQueueAt(t, ctx, st, seeder, now.Add(5*time.Minute)))

	now = tAct.Add(3 * time.Hour)
	recReactivate := doAdminJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+createdBind.ID, map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       6000,
		"tokenAction": "notify_only",
	})
	require.Equal(t, http.StatusOK, recReactivate.Code, recReactivate.Body.String())

	seedRecAfterReactivate, err := st.GetUsageGroupSeedRecord(ctx, groupG.ID, "", store.UsageBudgetWindowMonth, monthStart)
	require.NoError(t, err)
	assert.False(t, seedRecAfterReactivate.Finalized, "0 -> >0 re-activation must open a new activation pass")
	assert.Equal(t, now, seedRecAfterReactivate.ActivatedAt)
	assert.Equal(t, 2, drainGroupSeedQueueAt(t, ctx, st, seeder, now.Add(2*time.Minute)))
}

func TestPooledGroupSeeder_MultiReplicaConcurrency(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)
	// Pause the wall-clock background drainer so this test deterministically
	// drains the durable_queue_items at controlled clock steps (tAct, tAct+2m).
	srv.stopGroupBudgetSeedDrainer(ctx)

	tPre := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	tAct := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	monthStart, monthEnd := utcMonthWindow(tAct)

	proj := &store.Project{ID: uuid.NewString(), Name: "p-multi", Slug: "p-multi"}
	require.NoError(t, st.CreateProject(ctx, proj))

	g1 := createTestGroupForBudgets(t, st, "multi-g1", store.GroupTypeExplicit, "")
	g2 := createTestGroupForBudgets(t, st, "multi-g2", store.GroupTypeExplicit, "")
	u1 := createTestUserForBudgets(t, st, "multi-u1@example.com")
	u2 := createTestUserForBudgets(t, st, "multi-u2@example.com")
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: g1.ID, MemberType: store.GroupMemberTypeUser, MemberID: u1.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: g2.ID, MemberType: store.GroupMemberTypeUser, MemberID: u2.ID, Role: store.GroupMemberRoleMember}))

	ag1, run1, tok1 := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "ag-m1", u1.ID, tPre)
	ag2, run2, tok2 := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "ag-m2", u2.ID, tPre)
	for _, it := range []struct {
		agID, runID, tok, evID string
		in, out                int64
	}{
		{ag1.ID, run1, tok1, "m-ev-1", 300, 100},
		{ag2.ID, run2, tok2, "m-ev-2", 500, 100},
	} {
		ev := e2eMakeWireUsageEvent(srv.config.HubID, it.agID, proj.ID, it.runID, it.evID, 1, tPre, it.in, it.out)
		rec := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+it.agID+"/usage/events", it.tok, telemetrycontract.UsageIngestRequest{
			RunID: it.runID, ProducerEpoch: "ep-1", ProducerSeq: 1, Events: []telemetrycontract.UsageEvent{ev},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	// Construct two independent GroupBudgetSeeder instances sharing the same store (simulating two Hub replicas).
	seederA := NewGroupBudgetSeeder(st, srv.durableQueue, srv.TokenBudgetEvaluator(), nil, func() bool { return true }, func() int { return 90 })
	seederB := NewGroupBudgetSeeder(st, srv.durableQueue, srv.TokenBudgetEvaluator(), nil, func() bool { return true }, func() int { return 90 })
	seederA.nowFn = func() time.Time { return tAct.Add(2 * time.Minute) }
	seederB.nowFn = func() time.Time { return tAct.Add(2 * time.Minute) }

	p1 := GroupBudgetSeedPayload{
		GroupID: g1.ID, WindowKind: store.UsageBudgetWindowMonth,
		WindowStartUTC: monthStart, WindowEndUTC: monthEnd, ActivatedAt: tAct, IsFinalPass: true,
	}
	p2 := GroupBudgetSeedPayload{
		GroupID: g2.ID, WindowKind: store.UsageBudgetWindowMonth,
		WindowStartUTC: monthStart, WindowEndUTC: monthEnd, ActivatedAt: tAct, IsFinalPass: true,
	}

	// Run concurrently across both replicas for both different groups and the same group.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	wg.Add(4)
	go func() { defer wg.Done(); errs <- seederA.RunSeed(ctx, p1) }()
	go func() { defer wg.Done(); errs <- seederB.RunSeed(ctx, p2) }()
	go func() { defer wg.Done(); errs <- seederB.RunSeed(ctx, p1) }()
	go func() { defer wg.Done(); errs <- seederA.RunSeed(ctx, p2) }()
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}

	// Both records survive and are finalized with exact totals.
	rec1, err := st.GetUsageGroupSeedRecord(ctx, g1.ID, "", store.UsageBudgetWindowMonth, monthStart)
	require.NoError(t, err)
	assert.True(t, rec1.Finalized)
	assert.Equal(t, []string{u1.ID}, rec1.FrozenMemberIDs)

	rec2, err := st.GetUsageGroupSeedRecord(ctx, g2.ID, "", store.UsageBudgetWindowMonth, monthStart)
	require.NoError(t, err)
	assert.True(t, rec2.Finalized)
	assert.Equal(t, []string{u2.ID}, rec2.FrozenMemberIDs)

	sum1, err := st.SumUsageGroupRollupTokens(ctx, g1.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(400), sum1)

	sum2, err := st.SumUsageGroupRollupTokens(ctx, g2.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(600), sum2)
}

func TestPooledGroupSeeder_ExperimentOffNoWrites(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, false)

	tAct := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	monthStart, monthEnd := utcMonthWindow(tAct)
	groupG := createTestGroupForBudgets(t, st, "off-g", store.GroupTypeExplicit, "")

	seeder := srv.GroupBudgetSeeder()
	require.NotNil(t, seeder)
	seeder.nowFn = func() time.Time { return tAct }

	require.NoError(t, seeder.EnqueueForBindingActivation(ctx, store.LimitLLMTokensPerGroupMonth, groupG.ID, store.QuotaScopeSystem, "", tAct))
	require.NoError(t, seeder.RunSeed(ctx, GroupBudgetSeedPayload{
		GroupID: groupG.ID, WindowKind: store.UsageBudgetWindowMonth,
		WindowStartUTC: monthStart, WindowEndUTC: monthEnd, ActivatedAt: tAct, IsFinalPass: true,
	}))
	n, err := seeder.ReconcileMissingSeeds(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	_, err = st.GetUsageGroupSeedRecord(ctx, groupG.ID, "", store.UsageBudgetWindowMonth, monthStart)
	assert.ErrorIs(t, err, store.ErrNotFound, "experiment-off must write zero usage_group_budget_seeds rows")
}

func TestGroupUsageBudgets_ReadAndAuthz(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)
	// Pause the wall-clock background drainer so this test deterministically
	// drains the durable_queue_items at controlled clock steps (tAct, tAct+2m).
	srv.stopGroupBudgetSeedDrainer(ctx)

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	srv.TokenBudgetEvaluator().nowFn = func() time.Time { return now }
	srv.PooledGroupResolver().nowFn = func() time.Time { return now }
	srv.GroupBudgetSeeder().nowFn = func() time.Time { return now }

	proj := &store.Project{ID: uuid.NewString(), Name: "p-read", Slug: "p-read"}
	require.NoError(t, st.CreateProject(ctx, proj))

	groupG := createTestGroupForBudgets(t, st, "read-g", store.GroupTypeExplicit, "")
	childG := createTestGroupForBudgets(t, st, "read-child", store.GroupTypeExplicit, "")

	uOwner := createTestUserForBudgets(t, st, "owner-g@example.com")
	uAdmin := createTestUserForBudgets(t, st, "admin-g@example.com")
	uMember := createTestUserForBudgets(t, st, "member-g@example.com")
	uNested := createTestUserForBudgets(t, st, "nested-g@example.com")
	uOutsider := createTestUserForBudgets(t, st, "outsider-g@example.com")

	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: uOwner.ID, Role: store.GroupMemberRoleOwner}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: uAdmin.ID, Role: store.GroupMemberRoleAdmin}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: uMember.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeGroup, MemberID: childG.ID, Role: store.GroupMemberRoleMember}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: childG.ID, MemberType: store.GroupMemberTypeUser, MemberID: uNested.ID, Role: store.GroupMemberRoleMember}))

	// Capped monthly binding (2000) + explicit unlimited daily binding (value = 0, O4).
	monthDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupMonth)
	require.NoError(t, err)
	recBind := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+monthDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       2000,
	})
	require.Equal(t, http.StatusCreated, recBind.Code, recBind.Body.String())

	dayDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupDay)
	require.NoError(t, err)
	recDayBind := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+dayDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       0,
	})
	require.Equal(t, http.StatusCreated, recDayBind.Code, recDayBind.Body.String())

	// Ingest 600 tokens from uMember and 400 tokens from uNested -> total 1000/2000 (50%, warn).
	agM, runM, tokM := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "ag-m", uMember.ID, now)
	agN, runN, tokN := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "ag-n", uNested.ID, now)
	hubID := srv.config.HubID
	for _, it := range []struct {
		agID, runID, tok, evID string
		in, out                int64
	}{
		{agM.ID, runM, tokM, "read-ev-m", 400, 200},
		{agN.ID, runN, tokN, "read-ev-n", 250, 150},
	} {
		ev := e2eMakeWireUsageEvent(hubID, it.agID, proj.ID, it.runID, it.evID, 1, now, it.in, it.out)
		rec := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+it.agID+"/usage/events", it.tok, telemetrycontract.UsageIngestRequest{
			RunID: it.runID, ProducerEpoch: "ep-1", ProducerSeq: 1, Events: []telemetrycontract.UsageEvent{ev},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	path := "/api/v1/groups/" + groupG.ID + "/usage/budgets"

	// 1. Hub admin sees pool totals + per-member breakdown, including unlimited daily pool with cap: null (O4).
	recHubAdmin := doAdminJSONRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, recHubAdmin.Code, recHubAdmin.Body.String())
	var respHubAdmin GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recHubAdmin.Body.Bytes(), &respHubAdmin))
	assert.True(t, respHubAdmin.NonAdditive)
	assert.Equal(t, store.UsageScopeKindGroup, respHubAdmin.Scope.Kind)
	assert.Equal(t, groupG.ID, respHubAdmin.Scope.ID)
	require.Len(t, respHubAdmin.Budgets, 2)

	// Budgets are sorted by LimitName: day comes before month.
	dayItem := respHubAdmin.Budgets[0]
	monthItem := respHubAdmin.Budgets[1]
	assert.Equal(t, store.LimitLLMTokensPerGroupDay, dayItem.LimitName)
	assert.Nil(t, dayItem.Cap, "unlimited pool must be emitted with cap: null (O4)")
	assert.Equal(t, store.UsageBudgetStateOK, dayItem.State)
	assert.Equal(t, int64(1000), dayItem.Consumed)

	assert.Equal(t, store.LimitLLMTokensPerGroupMonth, monthItem.LimitName)
	assert.Equal(t, int64(1000), monthItem.Consumed)
	require.NotNil(t, monthItem.Cap)
	assert.Equal(t, int64(2000), *monthItem.Cap)
	assert.Equal(t, store.UsageBudgetStateWarn, monthItem.State)
	require.Len(t, monthItem.Members, 2)
	assert.Equal(t, uMember.ID, monthItem.Members[0].UserID)
	assert.Equal(t, int64(600), monthItem.Members[0].TokensSummable)
	assert.Equal(t, uNested.ID, monthItem.Members[1].UserID)
	assert.Equal(t, int64(400), monthItem.Members[1].TokensSummable)

	// 2. Group owner and group admin also see per-member breakdown.
	for _, u := range []*store.User{uOwner, uAdmin} {
		rec := doUserGETRequest(t, srv, u, path)
		require.Equal(t, http.StatusOK, rec.Code, "user %s: %s", u.Email, rec.Body.String())
		var resp GroupUsageBudgetsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Budgets, 2)
		assert.Equal(t, int64(1000), resp.Budgets[1].Consumed)
		assert.Len(t, resp.Budgets[1].Members, 2, "owner/admin %s must receive per-member breakdown", u.Email)
	}

	// 3. Regular direct member and nested member see pool totals ONLY (no members field).
	for _, u := range []*store.User{uMember, uNested} {
		rec := doUserGETRequest(t, srv, u, path)
		require.Equal(t, http.StatusOK, rec.Code, "user %s: %s", u.Email, rec.Body.String())
		var resp GroupUsageBudgetsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Budgets, 2)
		assert.Equal(t, int64(1000), resp.Budgets[1].Consumed)
		assert.Empty(t, resp.Budgets[1].Members, "regular member %s must NOT receive per-member breakdown", u.Email)
	}

	// 4. R4: Scoped UAT ceiling enforcement on membership branches.
	//    a) Group owner with hub UAT lacking quota:read -> 403 Forbidden.
	recNoQuota := doScopedUserGETRequest(t, srv, uOwner, TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:read"}, []string{"agent.read"}, groupG.ID)
	assert.Equal(t, http.StatusForbidden, recNoQuota.Code, "scoped UAT lacking quota:read must be denied even for group owner (R4)")

	//    b) Group owner with project-bounded UAT carrying quota:read -> 403 Forbidden (boundary mismatch).
	recProjBound := doScopedUserGETRequest(t, srv, uOwner, TokenBoundary{Kind: BoundaryKindProject, ProjectID: proj.ID}, []string{"quota:read"}, []string{"quota.read"}, groupG.ID)
	assert.Equal(t, http.StatusForbidden, recProjBound.Code, "project-bounded UAT must be denied on hub/group quota read (R4)")

	//    c) Group owner with hub UAT carrying quota:read -> 200 OK with breakdown.
	recOwnerUAT := doScopedUserGETRequest(t, srv, uOwner, TokenBoundary{Kind: BoundaryKindHub}, []string{"quota:read"}, []string{"quota.read"}, groupG.ID)
	require.Equal(t, http.StatusOK, recOwnerUAT.Code, recOwnerUAT.Body.String())
	var respOwnerUAT GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recOwnerUAT.Body.Bytes(), &respOwnerUAT))
	require.Len(t, respOwnerUAT.Budgets, 2)
	assert.Len(t, respOwnerUAT.Budgets[1].Members, 2)

	//    d) Regular member with hub UAT carrying quota:read -> 200 OK without breakdown.
	recMemberUAT := doScopedUserGETRequest(t, srv, uMember, TokenBoundary{Kind: BoundaryKindHub}, []string{"quota:read"}, []string{"quota.read"}, groupG.ID)
	require.Equal(t, http.StatusOK, recMemberUAT.Code, recMemberUAT.Body.String())
	var respMemberUAT GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recMemberUAT.Body.Bytes(), &respMemberUAT))
	require.Len(t, respMemberUAT.Budgets, 2)
	assert.Empty(t, respMemberUAT.Budgets[1].Members)

	// 5. Outsider gets 403 Forbidden.
	recOut := doUserGETRequest(t, srv, uOutsider, path)
	assert.Equal(t, http.StatusForbidden, recOut.Code, recOut.Body.String())

	// 6. Experiment OFF -> 404 Not Found.
	setTokenomicsExperimentsForTest(t, srv, true, false)
	recOff := doAdminJSONRequest(t, srv, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusNotFound, recOff.Code)
}
