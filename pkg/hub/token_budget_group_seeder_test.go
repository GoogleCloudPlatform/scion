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
	// drains durable_queue_items at controlled clock steps (tAct, tAct+2m).
	srv.stopGroupBudgetSeedDrainer(ctx)

	// Deterministic clock (O5): no time.Now() anywhere in this test.
	tPre := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	tAct := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
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

	// 1. Pre-activation spend (Nov 10 08:00): A=300, B=200 (nested member), C=400 (non-member).
	for _, item := range []struct {
		agID, runID, tok, evID string
		in, out                int64
	}{
		{agA.ID, runA, tokA, "pre-a", 200, 100},
		{agB.ID, runB, tokB, "pre-b", 120, 80},
		{agC.ID, runC, tokC, "pre-c", 300, 100},
	} {
		ev := e2eMakeWireUsageEvent(hubID, item.agID, proj.ID, item.runID, item.evID, 1, tPre, item.in, item.out)
		rec := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+item.agID+"/usage/events", item.tok, telemetrycontract.UsageIngestRequest{
			RunID: item.runID, ProducerEpoch: "ep-1", ProducerSeq: 1, Events: []telemetrycontract.UsageEvent{ev},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	// 2. Nov 10 10:00: create pooled monthly binding (cap = 1000) via HTTP POST.
	//    Opens charging episode 1 and enqueues initial (tAct) + ttl_cover (tAct+2m).
	now = tAct
	monthDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupMonth)
	require.NoError(t, err)
	dayDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupDay)
	require.NoError(t, err)

	recBind := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+monthDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       1000,
	})
	require.Equal(t, http.StatusCreated, recBind.Code, recBind.Body.String())
	var monthBind store.EntitlementBinding
	require.NoError(t, json.Unmarshal(recBind.Body.Bytes(), &monthBind))

	ep1, err := st.GetOpenUsageGroupChargingEpisode(ctx, groupG.ID)
	require.NoError(t, err)
	assert.False(t, ep1.Finalized)

	// 3. Concurrent ingest right after binding creation: A ingests 200 tokens (charged via origin='ingest').
	evPostA := e2eMakeWireUsageEvent(hubID, agA.ID, proj.ID, runA, "post-a", 2, tAct.Add(10*time.Second), 120, 80)
	recPostA := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agA.ID+"/usage/events", tokA, telemetrycontract.UsageIngestRequest{
		RunID: runA, ProducerEpoch: "ep-1", ProducerSeq: 2, Events: []telemetrycontract.UsageEvent{evPostA},
	})
	require.Equal(t, http.StatusOK, recPostA.Code, recPostA.Body.String())

	monthStart, monthEnd := utcMonthWindow(tAct)
	dayStart, dayEnd := utcDayWindow(tAct)

	// 4. O5: Drain immediate seed pass from durable_queue_items via HandleQueueItem.
	assert.Equal(t, 1, drainGroupSeedQueueAt(t, ctx, st, seeder, tAct), "only the immediate pass should be ready at tAct")
	// Idempotent direct re-run of initial pass produces identical total.
	require.NoError(t, seeder.RunSeed(ctx, GroupBudgetSeedPayload{
		EpisodeID: ep1.ID, GroupID: groupG.ID, IsFinalPass: false,
	}))

	// Total = pre-activation seed (300+200=500) + concurrent ingest (200) = 700 (warn).
	total, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(700), total)

	stRow, err := st.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID,
		ScopeKind:         store.UsageScopeKindGroup,
		ScopeID:           groupG.ID,
		ProjectScopeID:    "",
		WindowStartUTC:    monthStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, stRow.State)
	assert.Equal(t, int64(700), stRow.ConsumedAtEval)

	// 5. +2m: Drain delayed ttl_cover pass -> marks episode 1 finalized forever (D6a).
	now = tAct.Add(2 * time.Minute)
	assert.Equal(t, 1, drainGroupSeedQueueAt(t, ctx, st, seeder, now))

	ep1After, err := st.GetUsageGroupSeedEpisode(ctx, ep1.ID)
	require.NoError(t, err)
	assert.True(t, ep1After.Finalized)
	assert.ElementsMatch(t, []string{userA.ID, userB.ID}, ep1After.FrozenMemberIDs)

	// 6. R1 / D6a regression:
	//    A leaves G, C (who had 400 pre-activation spend) joins G.
	//    a) Admin edits month binding cap (1000 -> 5000) & token_action (pause_new).
	//    b) Admin creates a SECOND binding (day limit) on the same day while episode 1 is open.
	//    NEITHER may trigger a seed, refund A's 300, or charge C's pre-join 400!
	require.NoError(t, st.RemoveGroupMember(ctx, groupG.ID, store.GroupMemberTypeUser, userA.ID))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userC.ID, Role: store.GroupMemberRoleMember}))
	srv.InvalidatePooledGroupCacheAll()

	now = tAct.Add(1 * time.Hour)
	recEdit := doAdminJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+monthBind.ID, map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       5000,
		"tokenAction": "pause_new",
	})
	require.Equal(t, http.StatusOK, recEdit.Code, recEdit.Body.String())

	now = tAct.Add(6 * time.Hour)
	recDayBind := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+dayDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       2000,
	})
	require.Equal(t, http.StatusCreated, recDayBind.Code, recDayBind.Body.String())
	var dayBind store.EntitlementBinding
	require.NoError(t, json.Unmarshal(recDayBind.Body.Bytes(), &dayBind))

	// Zero items enqueued by either the cap edit or the second binding inside the open episode!
	assert.Equal(t, 0, drainGroupSeedQueueAt(t, ctx, st, seeder, now.Add(5*time.Minute)))
	enq, err := seeder.ReconcileMissingSeeds(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, enq)

	// Month AND day windows BOTH read the exact same 700 tokens (A's 300 preserved, C's 400 excluded).
	totalMonth, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(700), totalMonth, "month total must remain 700 (never refund leaver A)")

	totalDay, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", dayStart, dayEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(700), totalDay, "day limit must read the same episode seed+ingest rows (700)")

	// 7. Deactivate -> gap spend -> reactivate (Episode 2):
	//    Deleting only the month binding keeps episode 1 OPEN because dayBind is still active.
	now = tAct.Add(7 * time.Hour)
	recDelMonth := doAdminJSONRequest(t, srv, http.MethodDelete, "/api/v1/admin/entitlements/"+monthBind.ID, nil)
	require.Equal(t, http.StatusNoContent, recDelMonth.Code)
	stillOpen, err := st.GetOpenUsageGroupChargingEpisode(ctx, groupG.ID)
	require.NoError(t, err)
	assert.Equal(t, ep1.ID, stillOpen.ID, "episode must stay open while dayBind is still active")

	//    Deactivating the last active binding (dayBind -> 0) CLOSES episode 1 at tEnd1.
	tEnd1 := tAct.Add(8 * time.Hour)
	now = tEnd1
	recDisableDay := doAdminJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+dayBind.ID, map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       0,
		"tokenAction": "notify_only",
	})
	require.Equal(t, http.StatusOK, recDisableDay.Code)
	_, err = st.GetOpenUsageGroupChargingEpisode(ctx, groupG.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "last active binding deactivating must close episode 1")

	//    Spend in the inactive gap (Nov 10 19:00): C spends 150 tokens (uncharged because group has no active bindings).
	tGap := tAct.Add(9 * time.Hour)
	now = tGap
	evGapC := e2eMakeWireUsageEvent(hubID, agC.ID, proj.ID, runC, "gap-c", 2, tGap, 100, 50)
	recGapC := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agC.ID+"/usage/events", tokC, telemetrycontract.UsageIngestRequest{
		RunID: runC, ProducerEpoch: "ep-1", ProducerSeq: 2, Events: []telemetrycontract.UsageEvent{evGapC},
	})
	require.Equal(t, http.StatusOK, recGapC.Code, recGapC.Body.String())

	//    Reactivate dayBind (0 -> 6000) at Nov 10 20:00 -> opens Episode 2 with SeedFromUTC == tEnd1.
	tAct2 := tAct.Add(10 * time.Hour)
	now = tAct2
	recReactivate := doAdminJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+dayBind.ID, map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       6000,
		"tokenAction": "notify_only",
	})
	require.Equal(t, http.StatusOK, recReactivate.Code)

	ep2, err := st.GetOpenUsageGroupChargingEpisode(ctx, groupG.ID)
	require.NoError(t, err)
	assert.NotEqual(t, ep1.ID, ep2.ID)
	assert.Equal(t, tEnd1, ep2.SeedFromUTC, "episode 2 must seed only the gap since episode 1 ended")

	assert.Equal(t, 2, drainGroupSeedQueueAt(t, ctx, st, seeder, tAct2.Add(2*time.Minute)))

	// Total = ep1 (700) + ep2 gap (150) = 850 (C's pre-ep1 400 is STILL excluded; ep1's rows untouched).
	totalFinal, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(850), totalFinal, "episode 2 must add only the gap spend (150) on top of episode 1 (700)")
}

func TestPooledGroupSeeder_ReconcileStep2AndProjectScopeE2E(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)
	srv.stopGroupBudgetSeedDrainer(ctx)

	tPre := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	tCreate := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	now := tPre
	srv.TokenBudgetEvaluator().nowFn = func() time.Time { return now }
	srv.PooledGroupResolver().nowFn = func() time.Time { return now }
	seeder := srv.GroupBudgetSeeder()
	require.NotNil(t, seeder)
	seeder.nowFn = func() time.Time { return now }

	proj1 := &store.Project{ID: uuid.NewString(), Name: "p-e2e-1", Slug: "p-e2e-1"}
	proj2 := &store.Project{ID: uuid.NewString(), Name: "p-e2e-2", Slug: "p-e2e-2"}
	require.NoError(t, st.CreateProject(ctx, proj1))
	require.NoError(t, st.CreateProject(ctx, proj2))

	groupG := createTestGroupForBudgets(t, st, "proj-seed-g", store.GroupTypeExplicit, "")
	userA := createTestUserForBudgets(t, st, "proj-seed-a@example.com")
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userA.ID, Role: store.GroupMemberRoleMember}))

	// Pre-activation spend: 300 in proj1, 250 in proj2.
	ag1, run1, tok1 := createTestAgentWithRunForBudgets(t, srv, st, proj1.ID, "ag-p1", userA.ID, tPre)
	ag2, run2, tok2 := createTestAgentWithRunForBudgets(t, srv, st, proj2.ID, "ag-p2", userA.ID, tPre)
	for _, it := range []struct {
		agID, projID, runID, tok, evID string
		in, out                        int64
	}{
		{ag1.ID, proj1.ID, run1, tok1, "ps-ev-1", 200, 100},
		{ag2.ID, proj2.ID, run2, tok2, "ps-ev-2", 150, 100},
	} {
		ev := e2eMakeWireUsageEvent(srv.config.HubID, it.agID, it.projID, it.runID, it.evID, 1, tPre, it.in, it.out)
		rec := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+it.agID+"/usage/events", it.tok, telemetrycontract.UsageIngestRequest{
			RunID: it.runID, ProducerEpoch: "ep-1", ProducerSeq: 1, Events: []telemetrycontract.UsageEvent{ev},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	// O5: Insert a project-scoped pooled binding directly into the store (bypassing admin HTTP API),
	// so no charging episode exists yet — exercises ReconcileMissingSeeds step 2 AND project-scope E2E.
	now = tCreate.Add(5 * time.Minute)
	monthDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupMonth)
	require.NoError(t, err)
	_, err = st.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		ID:                uuid.NewString(),
		LimitDefinitionID: monthDef.ID,
		SubjectType:       store.EntitlementSubjectGroup,
		SubjectID:         groupG.ID,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           proj1.ID,
		Value:             500,
		CreatedAt:         tCreate,
		UpdatedAt:         tCreate,
	})
	require.NoError(t, err)

	enq, err := seeder.ReconcileMissingSeeds(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, enq, "reconcile step 2 must recover missing episode for in-window binding")

	ep, err := st.GetOpenUsageGroupChargingEpisode(ctx, groupG.ID)
	require.NoError(t, err)
	now = ep.CutoffAt
	assert.Equal(t, 1, drainGroupSeedQueueAt(t, ctx, st, seeder, ep.CutoffAt))

	// Verify GET /api/v1/groups/{id}/usage/budgets?project_id=proj1 sees ONLY proj1's 300 tokens (warn),
	// while system rollups received both projects (550).
	recProj := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/groups/"+groupG.ID+"/usage/budgets?project_id="+proj1.ID, nil)
	require.Equal(t, http.StatusOK, recProj.Code, recProj.Body.String())
	var resp GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recProj.Body.Bytes(), &resp))
	require.Len(t, resp.Budgets, 1)
	assert.Equal(t, proj1.ID, resp.Budgets[0].ProjectScopeID)
	assert.Equal(t, int64(300), resp.Budgets[0].Consumed)
	assert.Equal(t, store.UsageBudgetStateWarn, resp.Budgets[0].State)
	require.Len(t, resp.Budgets[0].Members, 1)
	assert.Equal(t, userA.ID, resp.Budgets[0].Members[0].UserID)
	assert.Equal(t, int64(300), resp.Budgets[0].Members[0].TokensSummable)

	monthStart, monthEnd := utcMonthWindow(now)
	sysSum, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(550), sysSum, "episode seed mirrors ingest by writing both system (550) and project (300/250) rows")
}

func TestPooledGroupSeeder_MultiReplicaConcurrency(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)
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

	ep1, _, err := st.OpenUsageGroupChargingEpisode(ctx, g1.ID, tAct)
	require.NoError(t, err)
	ep2, _, err := st.OpenUsageGroupChargingEpisode(ctx, g2.ID, tAct)
	require.NoError(t, err)

	// Construct two independent GroupBudgetSeeder instances sharing the same store (simulating two Hub replicas).
	seederA := NewGroupBudgetSeeder(st, srv.durableQueue, srv.TokenBudgetEvaluator(), nil, func() bool { return true }, func() int { return 90 })
	seederB := NewGroupBudgetSeeder(st, srv.durableQueue, srv.TokenBudgetEvaluator(), nil, func() bool { return true }, func() int { return 90 })
	seederA.nowFn = func() time.Time { return tAct.Add(2 * time.Minute) }
	seederB.nowFn = func() time.Time { return tAct.Add(2 * time.Minute) }

	p1 := GroupBudgetSeedPayload{EpisodeID: ep1.ID, GroupID: g1.ID, IsFinalPass: true}
	p2 := GroupBudgetSeedPayload{EpisodeID: ep2.ID, GroupID: g2.ID, IsFinalPass: true}

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

	got1, err := st.GetUsageGroupSeedEpisode(ctx, ep1.ID)
	require.NoError(t, err)
	assert.True(t, got1.Finalized)
	assert.Equal(t, []string{u1.ID}, got1.FrozenMemberIDs)

	got2, err := st.GetUsageGroupSeedEpisode(ctx, ep2.ID)
	require.NoError(t, err)
	assert.True(t, got2.Finalized)
	assert.Equal(t, []string{u2.ID}, got2.FrozenMemberIDs)

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
	groupG := createTestGroupForBudgets(t, st, "off-g", store.GroupTypeExplicit, "")

	seeder := srv.GroupBudgetSeeder()
	require.NotNil(t, seeder)
	seeder.nowFn = func() time.Time { return tAct }

	require.NoError(t, seeder.OnGroupBindingActivated(ctx, groupG.ID, tAct))
	require.NoError(t, seeder.OnGroupBindingDeactivated(ctx, groupG.ID, tAct))
	n, err := seeder.ReconcileMissingSeeds(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	_, err = st.GetOpenUsageGroupChargingEpisode(ctx, groupG.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "experiment-off must write zero usage_group_budget_seeds rows")
}

func TestGroupUsageBudgets_ReadAndAuthz(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)
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

	// 1. Hub admin sees pool totals + per-member breakdown, unlimited daily pool with cap: null (O4),
	//    and override_until explicitly present as null (O3).
	recHubAdmin := doAdminJSONRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, recHubAdmin.Code, recHubAdmin.Body.String())
	assert.Contains(t, recHubAdmin.Body.String(), `"override_until":null`, "override_until must always be emitted (O3)")
	var respHubAdmin GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recHubAdmin.Body.Bytes(), &respHubAdmin))
	assert.True(t, respHubAdmin.NonAdditive)
	assert.Equal(t, store.UsageScopeKindGroup, respHubAdmin.Scope.Kind)
	assert.Equal(t, groupG.ID, respHubAdmin.Scope.ID)
	require.Len(t, respHubAdmin.Budgets, 2)

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

	// 1b. O2: While an override is active (override_until > now), report stored state ("ok")
	//     even if consumed >= cap; once expired, fall back to max(stored, computed).
	monthStart, _ := utcMonthWindow(now)
	stRow, err := st.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID, ScopeKind: store.UsageScopeKindGroup,
		ScopeID: groupG.ID, ProjectScopeID: "", WindowStartUTC: monthStart,
	})
	require.NoError(t, err)
	overrideUntil := now.Add(1 * time.Hour)
	stRow.State = store.UsageBudgetStateOK
	stRow.OverrideUntil = &overrideUntil
	_, err = st.CompareAndSwapUsageBudgetState(ctx, stRow.StateVersion, stRow)
	require.NoError(t, err)

	recOverrideActive := doAdminJSONRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, recOverrideActive.Code)
	var respOverrideActive GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recOverrideActive.Body.Bytes(), &respOverrideActive))
	assert.Equal(t, store.UsageBudgetStateOK, respOverrideActive.Budgets[1].State, "active override must return stored state (O2)")

	now = overrideUntil.Add(1 * time.Minute)
	recOverrideExpired := doAdminJSONRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, recOverrideExpired.Code)
	var respOverrideExpired GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recOverrideExpired.Body.Bytes(), &respOverrideExpired))
	assert.Equal(t, store.UsageBudgetStateWarn, respOverrideExpired.Budgets[1].State, "expired override must resume max(stored, computed) (O2)")
	now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

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

	// 4. R4 / O4: Scoped UAT ceiling enforcement across group.update and membership branches.
	recNoQuota := doScopedUserGETRequest(t, srv, uOwner, TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:read"}, []string{"agent.read"}, groupG.ID)
	assert.Equal(t, http.StatusForbidden, recNoQuota.Code, "scoped UAT lacking quota:read must be denied even for group owner (R4)")

	// O4: Scoped UAT with group.update ceiling (but NOT quota.read) must be denied on branch 2 as well.
	recGroupUpdateOnly := doScopedUserGETRequest(t, srv, uOwner, TokenBoundary{Kind: BoundaryKindHub}, []string{"group:update"}, []string{"group.update"}, groupG.ID)
	assert.Equal(t, http.StatusForbidden, recGroupUpdateOnly.Code, "scoped UAT with group.update but without quota.read must be denied (O4)")

	recProjBound := doScopedUserGETRequest(t, srv, uOwner, TokenBoundary{Kind: BoundaryKindProject, ProjectID: proj.ID}, []string{"quota:read"}, []string{"quota.read"}, groupG.ID)
	assert.Equal(t, http.StatusForbidden, recProjBound.Code, "project-bounded UAT must be denied on hub/group quota read (R4)")

	recOwnerUAT := doScopedUserGETRequest(t, srv, uOwner, TokenBoundary{Kind: BoundaryKindHub}, []string{"quota:read"}, []string{"quota.read"}, groupG.ID)
	require.Equal(t, http.StatusOK, recOwnerUAT.Code, recOwnerUAT.Body.String())
	var respOwnerUAT GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recOwnerUAT.Body.Bytes(), &respOwnerUAT))
	require.Len(t, respOwnerUAT.Budgets, 2)
	assert.Len(t, respOwnerUAT.Budgets[1].Members, 2)

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
