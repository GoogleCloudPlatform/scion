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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestPooledGroupSeeder_MidMonthD6AndIdempotency(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)

	wallNow := time.Now().UTC().Truncate(time.Second)
	tPre := time.Date(wallNow.Year(), wallNow.Month(), 1, 10, 0, 0, 0, time.UTC)
	if !tPre.Before(wallNow) {
		tPre = wallNow.Add(-5 * time.Minute)
	}
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

	// 2. Mid-month: create pooled monthly binding (cap = 1000).
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
	tAct := createdBind.UpdatedAt.UTC()
	if tAct.IsZero() {
		tAct = createdBind.CreatedAt.UTC()
	}
	now = tAct

	// 3. Concurrent ingest right after binding creation: A ingests 200 tokens (charged via origin='ingest').
	evPostA := e2eMakeWireUsageEvent(hubID, agA.ID, proj.ID, runA, "post-a", 2, tAct.Add(10*time.Second), 120, 80)
	recPostA := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agA.ID+"/usage/events", tokA, telemetrycontract.UsageIngestRequest{
		RunID: runA, ProducerEpoch: "ep-1", ProducerSeq: 2, Events: []telemetrycontract.UsageEvent{evPostA},
	})
	require.Equal(t, http.StatusOK, recPostA.Code, recPostA.Body.String())

	monthStart, monthEnd := utcMonthWindow(tAct)

	// 4. Immediate seed run: seeds A(300) + B(250) = 550 without double-counting post-a(200). Total = 750 (warn).
	seedPayload := GroupBudgetSeedPayload{
		GroupID:        groupG.ID,
		ProjectScopeID: "",
		WindowStartUTC: monthStart,
		WindowEndUTC:   monthEnd,
		ActivatedAt:    tAct,
		IsFinalPass:    false,
	}
	require.NoError(t, seeder.RunSeed(ctx, seedPayload))
	// Idempotent re-run produces identical total.
	require.NoError(t, seeder.RunSeed(ctx, seedPayload))

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

	// 5. +2m final pass completes and marks seed final (D6).
	now = tAct.Add(2 * time.Minute)
	seedPayload.IsFinalPass = true
	require.NoError(t, seeder.RunSeed(ctx, seedPayload))

	// 6. After finalization: A leaves G, C (who had 400 pre-activation spend) joins G.
	//    Re-running seeder OR nightly reconcile MUST NOT refund A or charge C's pre-join 400!
	require.NoError(t, st.RemoveGroupMember(ctx, groupG.ID, store.GroupMemberTypeUser, userA.ID))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userC.ID, Role: store.GroupMemberRoleMember}))
	srv.InvalidatePooledGroupCacheAll()

	require.NoError(t, seeder.RunSeed(ctx, seedPayload))
	enq, err := seeder.ReconcileMissingSeeds(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, enq, "finalized seed must not be re-enqueued by nightly reconcile")

	totalAfter, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthEnd)
	require.NoError(t, err)
	assert.Equal(t, int64(750), totalAfter, "finalized seed must preserve A's spend and exclude C's pre-join spend")
}

func TestGroupUsageBudgets_ReadAndAuthz(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	srv.TokenBudgetEvaluator().nowFn = func() time.Time { return now }
	srv.PooledGroupResolver().nowFn = func() time.Time { return now }

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

	// 1. Hub admin sees pool total + per-member breakdown.
	recHubAdmin := doAdminJSONRequest(t, srv, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, recHubAdmin.Code, recHubAdmin.Body.String())
	var respHubAdmin GroupUsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recHubAdmin.Body.Bytes(), &respHubAdmin))
	assert.True(t, respHubAdmin.NonAdditive)
	assert.Equal(t, store.UsageScopeKindGroup, respHubAdmin.Scope.Kind)
	assert.Equal(t, groupG.ID, respHubAdmin.Scope.ID)
	require.Len(t, respHubAdmin.Budgets, 1)
	assert.Equal(t, int64(1000), respHubAdmin.Budgets[0].Consumed)
	require.NotNil(t, respHubAdmin.Budgets[0].Cap)
	assert.Equal(t, int64(2000), *respHubAdmin.Budgets[0].Cap)
	assert.Equal(t, store.UsageBudgetStateWarn, respHubAdmin.Budgets[0].State)
	require.Len(t, respHubAdmin.Budgets[0].Members, 2)
	assert.Equal(t, uMember.ID, respHubAdmin.Budgets[0].Members[0].UserID)
	assert.Equal(t, int64(600), respHubAdmin.Budgets[0].Members[0].TokensSummable)
	assert.Equal(t, uNested.ID, respHubAdmin.Budgets[0].Members[1].UserID)
	assert.Equal(t, int64(400), respHubAdmin.Budgets[0].Members[1].TokensSummable)

	// 2. Group owner and group admin also see per-member breakdown.
	for _, u := range []*store.User{uOwner, uAdmin} {
		rec := doUserGETRequest(t, srv, u, path)
		require.Equal(t, http.StatusOK, rec.Code, "user %s: %s", u.Email, rec.Body.String())
		var resp GroupUsageBudgetsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Budgets, 1)
		assert.Equal(t, int64(1000), resp.Budgets[0].Consumed)
		assert.Len(t, resp.Budgets[0].Members, 2, "owner/admin %s must receive per-member breakdown", u.Email)
	}

	// 3. Regular direct member and nested member see pool totals ONLY (no members field).
	for _, u := range []*store.User{uMember, uNested} {
		rec := doUserGETRequest(t, srv, u, path)
		require.Equal(t, http.StatusOK, rec.Code, "user %s: %s", u.Email, rec.Body.String())
		var resp GroupUsageBudgetsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Budgets, 1)
		assert.Equal(t, int64(1000), resp.Budgets[0].Consumed)
		assert.Empty(t, resp.Budgets[0].Members, "regular member %s must NOT receive per-member breakdown", u.Email)
	}

	// 4. Outsider gets 403 Forbidden.
	recOut := doUserGETRequest(t, srv, uOutsider, path)
	assert.Equal(t, http.StatusForbidden, recOut.Code, recOut.Body.String())

	// 5. Experiment OFF -> 404 Not Found.
	setTokenomicsExperimentsForTest(t, srv, true, false)
	recOff := doAdminJSONRequest(t, srv, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusNotFound, recOff.Code)
}
