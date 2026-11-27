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
	"bytes"
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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func createTestGroupForBudgets(t *testing.T, st store.Store, name, groupType, projectID string) *store.Group {
	t.Helper()
	g := &store.Group{
		ID:        uuid.NewString(),
		Name:      name,
		Slug:      name + "-" + uuid.NewString()[:8],
		GroupType: groupType,
		ProjectID: projectID,
	}
	require.NoError(t, st.CreateGroup(context.Background(), g))
	return g
}

func createTestUserForBudgets(t *testing.T, st store.Store, email string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          uuid.NewString(),
		Email:       email,
		DisplayName: email,
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(context.Background(), u))
	return u
}

func createTestAgentWithRunForBudgets(t *testing.T, srv *Server, st store.Store, projID, name, billingUserID string, now time.Time) (*store.Agent, string, string) {
	t.Helper()
	ctx := context.Background()
	runID := "run-" + uuid.NewString()
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      name + "-" + uuid.NewString()[:8],
		Name:      name,
		ProjectID: projID,
		Phase:     string(state.PhaseRunning),
		RunID:     runID,
		OwnerID:   billingUserID,
		CreatedBy: billingUserID,
	}
	require.NoError(t, st.CreateAgent(ctx, ag))
	var buPtr *string
	if billingUserID != "" {
		u := billingUserID
		buPtr = &u
	}
	_, err := st.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		AgentID:       ag.ID,
		ProjectID:     projID,
		BillingUserID: buPtr,
		Harness:       "claude",
		CreatedAt:     now,
	})
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, projID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	return ag, runID, tok
}

func TestPooledGroupLimits_SeedAndBindingShapeValidation(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Experiment OFF: pooled group limits are not visible in /api/v1/admin/limits.
	setTokenomicsExperimentsForTest(t, srv, true, false)
	recOff := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusOK, recOff.Code)
	var listOff listLimitDefinitionsResponse
	require.NoError(t, json.Unmarshal(recOff.Body.Bytes(), &listOff))
	for _, d := range listOff.Items {
		assert.NotEqual(t, store.LimitResourceTypeLLMTokens, d.ResourceType)
	}

	// Experiment ON: reconcile seeds both pooled group limit definitions.
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)

	dayDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupDay)
	require.NoError(t, err)
	assert.Equal(t, store.LimitResourceTypeLLMTokens, dayDef.ResourceType)
	assert.Equal(t, int64(0), dayDef.DefaultValue)

	monthDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupMonth)
	require.NoError(t, err)
	assert.Equal(t, store.LimitResourceTypeLLMTokens, monthDef.ResourceType)
	assert.Equal(t, int64(0), monthDef.DefaultValue)

	proj := &store.Project{ID: uuid.NewString(), Name: "p-val", Slug: "p-val"}
	require.NoError(t, st.CreateProject(ctx, proj))
	explicitGrp := createTestGroupForBudgets(t, st, "eng-pool", store.GroupTypeExplicit, "")
	projectAgentsGrp := createTestGroupForBudgets(t, st, "proj-agents-pool", store.GroupTypeProjectAgents, proj.ID)

	badPayloads := []struct {
		name string
		body map[string]any
	}{
		{
			name: "user subject rejected",
			body: map[string]any{"subjectType": "user", "subjectId": uuid.NewString(), "scopeType": "system", "scopeId": "", "value": 1000},
		},
		{
			name: "system_default subject rejected",
			body: map[string]any{"subjectType": "system_default", "subjectId": "", "scopeType": "system", "scopeId": "", "value": 1000},
		},
		{
			name: "broker scope rejected",
			body: map[string]any{"subjectType": "group", "subjectId": explicitGrp.ID, "scopeType": "broker", "scopeId": "b1", "value": 1000},
		},
		{
			name: "system scope with non-empty scopeId rejected",
			body: map[string]any{"subjectType": "group", "subjectId": explicitGrp.ID, "scopeType": "system", "scopeId": proj.ID, "value": 1000},
		},
		{
			name: "project scope with empty scopeId rejected",
			body: map[string]any{"subjectType": "group", "subjectId": explicitGrp.ID, "scopeType": "project", "scopeId": "", "value": 1000},
		},
		{
			name: "project_agents group rejected",
			body: map[string]any{"subjectType": "group", "subjectId": projectAgentsGrp.ID, "scopeType": "system", "scopeId": "", "value": 1000},
		},
		{
			name: "nonexistent group rejected",
			body: map[string]any{"subjectType": "group", "subjectId": uuid.NewString(), "scopeType": "system", "scopeId": "", "value": 1000},
		},
	}

	for _, tc := range badPayloads {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+monthDef.ID+"/entitlements", tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		})
	}

	// Valid system-scoped binding succeeds.
	recSys := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+monthDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   explicitGrp.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       50000,
	})
	require.Equal(t, http.StatusCreated, recSys.Code, recSys.Body.String())
	var createdBinding store.EntitlementBinding
	require.NoError(t, json.Unmarshal(recSys.Body.Bytes(), &createdBinding))

	// Valid project-scoped binding succeeds.
	recProj := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+dayDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   explicitGrp.ID,
		"scopeType":   "project",
		"scopeId":     proj.ID,
		"value":       10000,
	})
	require.Equal(t, http.StatusCreated, recProj.Code, recProj.Body.String())

	// PUT cannot reshape a valid pooled binding into an invalid shape.
	recBadPut := doAdminJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+createdBinding.ID, map[string]any{
		"subjectType": "user",
		"subjectId":   uuid.NewString(),
		"scopeType":   "system",
		"scopeId":     "",
		"value":       50000,
	})
	assert.Equal(t, http.StatusBadRequest, recBadPut.Code, recBadPut.Body.String())
}

func TestPooledGroupResolver_CacheTTLAndInvalidation(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)

	monthDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupMonth)
	require.NoError(t, err)

	g1 := createTestGroupForBudgets(t, st, "g-cache-1", store.GroupTypeExplicit, "")
	g2 := createTestGroupForBudgets(t, st, "g-cache-2", store.GroupTypeExplicit, "")
	u1 := createTestUserForBudgets(t, st, "cache-u1@example.com")
	u2 := createTestUserForBudgets(t, st, "cache-u2@example.com")

	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{
		GroupID: g1.ID, MemberType: store.GroupMemberTypeUser, MemberID: u1.ID, Role: store.GroupMemberRoleMember,
	}))
	// Expired membership in g2 must be ignored.
	past := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{
		GroupID: g2.ID, MemberType: store.GroupMemberTypeUser, MemberID: u1.ID, Role: store.GroupMemberRoleMember, ExpiresAt: &past,
	}))

	_, err = st.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: monthDef.ID,
		SubjectType:       store.EntitlementSubjectGroup,
		SubjectID:         g1.ID,
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		Value:             10000,
	})
	require.NoError(t, err)
	_, err = st.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: monthDef.ID,
		SubjectType:       store.EntitlementSubjectGroup,
		SubjectID:         g2.ID,
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		Value:             10000,
	})
	require.NoError(t, err)

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	resolver := NewPooledGroupResolver(st)
	resolver.nowFn = func() time.Time { return now }

	got, err := resolver.ResolveForUsers(ctx, []string{u1.ID, u2.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{g1.ID}, got[u1.ID], "expired g2 membership must be excluded")
	assert.Empty(t, got[u2.ID])

	// Add u2 directly in store (bypassing HTTP invalidation): cached result stays stale until TTL or invalidation.
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{
		GroupID: g1.ID, MemberType: store.GroupMemberTypeUser, MemberID: u2.ID, Role: store.GroupMemberRoleMember,
	}))
	gotStale, err := resolver.ResolveForUser(ctx, u2.ID)
	require.NoError(t, err)
	assert.Empty(t, gotStale, "within 60s TTL cache should still serve previous value")

	// Targeted InvalidateUser refreshes u2 immediately.
	resolver.InvalidateUser(u2.ID)
	gotFresh, err := resolver.ResolveForUser(ctx, u2.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{g1.ID}, gotFresh)

	// Remove u2 directly in store and advance clock past 60s TTL -> expires and refreshes.
	require.NoError(t, st.RemoveGroupMember(ctx, g1.ID, store.GroupMemberTypeUser, u2.ID))
	now = now.Add(61 * time.Second)
	gotAfterTTL, err := resolver.ResolveForUser(ctx, u2.ID)
	require.NoError(t, err)
	assert.Empty(t, gotAfterTTL)
}

func TestPooledGroupBudget_E2E_NestedLeaveJoinOverlapAndAdmission(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileTokenBudgets(ctx)

	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	srv.TokenBudgetEvaluator().nowFn = func() time.Time { return now }
	srv.PooledGroupResolver().nowFn = func() time.Time { return now }

	var hookMu sync.Mutex
	var crossings []BudgetThresholdCrossing
	srv.SetBudgetThresholdHook(func(_ context.Context, c BudgetThresholdCrossing) {
		hookMu.Lock()
		crossings = append(crossings, c)
		hookMu.Unlock()
	})

	proj := &store.Project{ID: uuid.NewString(), Name: "p-group-e2e", Slug: "p-group-e2e"}
	require.NoError(t, st.CreateProject(ctx, proj))

	// Nested group hierarchy: parent group G contains direct user A and child group ChildG (which contains user B).
	// Overlapping group G2 also contains user A. User C is initially outside G.
	groupG := createTestGroupForBudgets(t, st, "group-g", store.GroupTypeExplicit, "")
	childG := createTestGroupForBudgets(t, st, "child-g", store.GroupTypeExplicit, "")
	groupG2 := createTestGroupForBudgets(t, st, "group-g2", store.GroupTypeExplicit, "")

	userA := createTestUserForBudgets(t, st, "user-a@example.com")
	userB := createTestUserForBudgets(t, st, "user-b@example.com")
	userC := createTestUserForBudgets(t, st, "user-c@example.com")

	// Give userA project membership so removing userA from groupG does not
	// trigger project-access membership-loss revocation on userA's running agent.
	pmRole, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: pmRole.ID,
		PrincipalType:    store.PolicyPrincipalTypeUser,
		PrincipalID:      userA.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
	})
	require.NoError(t, err)

	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userA.ID, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupG.ID, MemberType: store.GroupMemberTypeGroup, MemberID: childG.ID, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{
		GroupID: childG.ID, MemberType: store.GroupMemberTypeUser, MemberID: userB.ID, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, st.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupG2.ID, MemberType: store.GroupMemberTypeUser, MemberID: userA.ID, Role: store.GroupMemberRoleMember,
	}))

	// Bindings via admin API:
	// - groupG: system monthly cap = 1000 (default action pause_new)
	// - groupG2: project-scoped daily cap = 800 (action notify_only)
	monthDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupMonth)
	require.NoError(t, err)
	dayDef, err := st.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerGroupDay)
	require.NoError(t, err)

	recB1 := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+monthDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   groupG.ID,
		"scopeType":   "system",
		"scopeId":     "",
		"value":       1000,
	})
	require.Equal(t, http.StatusCreated, recB1.Code, recB1.Body.String())

	notifyOnly := store.TokenActionNotifyOnly
	recB2 := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+dayDef.ID+"/entitlements", map[string]any{
		"subjectType": "group",
		"subjectId":   groupG2.ID,
		"scopeType":   "project",
		"scopeId":     proj.ID,
		"value":       800,
		"tokenAction": &notifyOnly,
	})
	require.Equal(t, http.StatusCreated, recB2.Code, recB2.Body.String())

	agA, runA, tokA := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "agent-a", userA.ID, now)
	agB, runB, tokB := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "agent-b", userB.ID, now)
	agC, runC, tokC := createTestAgentWithRunForBudgets(t, srv, st, proj.ID, "agent-c", userC.ID, now)

	hubID := srv.config.HubID

	// 1. User A ingests 600 tokens (including forged charged_group_ids in raw JSON to verify anti-spoofing).
	//    -> groupG reaches 600/1000 (warn), groupG2 reaches 600/800 (warn).
	evA1 := e2eMakeWireUsageEvent(hubID, agA.ID, proj.ID, runA, "ev-a-1", 1, now, 400, 200)
	rawBody, err := json.Marshal(telemetrycontract.UsageIngestRequest{
		RunID:         runA,
		ProducerEpoch: "ep-1",
		ProducerSeq:   1,
		Events:        []telemetrycontract.UsageEvent{evA1},
	})
	require.NoError(t, err)
	// Inject forged charged_group_ids & billing_user_id into payload JSON.
	var rawMap map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &rawMap))
	evList := rawMap["events"].([]any)
	evList[0].(map[string]any)["charged_group_ids"] = []string{"forged-group-id"}
	evList[0].(map[string]any)["billing_user_id"] = userC.ID
	forgedBytes, err := json.Marshal(rawMap)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agA.ID+"/usage/events", bytes.NewReader(forgedBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", tokA)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ingestResp telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ingestResp))
	require.Equal(t, 1, ingestResp.Accepted, "body: %s", rec.Body.String())

	monthStart := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

	stG, err := st.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID,
		ScopeKind:         store.UsageScopeKindGroup,
		ScopeID:           groupG.ID,
		ProjectScopeID:    "",
		WindowStartUTC:    monthStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, stG.State)
	assert.Equal(t, int64(600), stG.ConsumedAtEval)

	// Overlapping groupG2 also charged 600 in full.
	stG2, err := st.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: dayDef.ID,
		ScopeKind:         store.UsageScopeKindGroup,
		ScopeID:           groupG2.ID,
		ProjectScopeID:    proj.ID,
		WindowStartUTC:    dayStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, stG2.State)
	assert.Equal(t, int64(600), stG2.ConsumedAtEval)

	// Forged group was NOT charged.
	forgedTokens, err := st.SumUsageGroupRollupTokens(ctx, "forged-group-id", "", monthStart, monthStart.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.Equal(t, int64(0), forgedTokens)

	// 2. Child-group user B ingests 450 tokens -> nested groupG reaches 1050/1000 (exhausted).
	evB1 := e2eMakeWireUsageEvent(hubID, agB.ID, proj.ID, runB, "ev-b-1", 1, now, 300, 150)
	recB := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agB.ID+"/usage/events", tokB, telemetrycontract.UsageIngestRequest{
		RunID:         runB,
		ProducerEpoch: "ep-1",
		ProducerSeq:   1,
		Events:        []telemetrycontract.UsageEvent{evB1},
	})
	require.Equal(t, http.StatusOK, recB.Code, recB.Body.String())

	stG, err = st.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: monthDef.ID,
		ScopeKind:         store.UsageScopeKindGroup,
		ScopeID:           groupG.ID,
		ProjectScopeID:    "",
		WindowStartUTC:    monthStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, stG.State)
	assert.Equal(t, int64(1050), stG.ConsumedAtEval)

	// 3. IsScopeExhaustedForAction picks up groupG exhaustion for both A and B (pause_new),
	//    but NOT for non-member C.
	exA, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, userA.ID, now, store.TokenActionPauseNew)
	require.NoError(t, err)
	require.True(t, exA.Exhausted)
	assert.Equal(t, store.UsageScopeKindGroup, exA.ScopeKind)
	assert.Equal(t, groupG.ID, exA.ScopeID)
	assert.Equal(t, store.LimitLLMTokensPerGroupMonth, exA.LimitName)

	exB, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, userB.ID, now, store.TokenActionPauseNew)
	require.NoError(t, err)
	require.True(t, exB.Exhausted)
	assert.Equal(t, groupG.ID, exB.ScopeID)

	exC, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, userC.ID, now, store.TokenActionPauseNew)
	require.NoError(t, err)
	assert.False(t, exC.Exhausted)

	// 4. User A leaves groupG via HTTP API (invalidates cache):
	//    past spend in groupG is preserved (still 1050); A's new spend goes only to groupG2 (not groupG).
	recRm := doAdminJSONRequest(t, srv, http.MethodDelete, "/api/v1/groups/"+groupG.ID+"/members/user/"+userA.ID, nil)
	require.Equal(t, http.StatusNoContent, recRm.Code, recRm.Body.String())

	// A is no longer blocked by groupG.
	exAAfterLeave, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, userA.ID, now, store.TokenActionPauseNew)
	require.NoError(t, err)
	assert.False(t, exAAfterLeave.Exhausted)

	// A ingests 250 tokens -> groupG2 reaches 850/800 (exhausted, notify_only), groupG stays 1050.
	evA2 := e2eMakeWireUsageEvent(hubID, agA.ID, proj.ID, runA, "ev-a-2", 2, now, 150, 100)
	recA2 := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agA.ID+"/usage/events", tokA, telemetrycontract.UsageIngestRequest{
		RunID:         runA,
		ProducerEpoch: "ep-1",
		ProducerSeq:   2,
		Events:        []telemetrycontract.UsageEvent{evA2},
	})
	require.Equal(t, http.StatusOK, recA2.Code, recA2.Body.String())

	sumG, err := st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthStart.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.Equal(t, int64(1050), sumG, "leaving groupG must preserve past spend and stop charging future spend")

	// groupG2 is exhausted with notify_only: does NOT block pause_new, DOES match notify_only.
	exG2Pause, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, userA.ID, now, store.TokenActionPauseNew)
	require.NoError(t, err)
	assert.False(t, exG2Pause.Exhausted)
	exG2Notify, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, userA.ID, now, store.TokenActionNotifyOnly)
	require.NoError(t, err)
	require.True(t, exG2Notify.Exhausted)
	assert.Equal(t, groupG2.ID, exG2Notify.ScopeID)

	// 5. User C ingests 200 tokens BEFORE joining groupG, then joins groupG via HTTP API, then ingests 100 tokens:
	//    only the post-join 100 tokens are charged to groupG (1050 + 100 = 1150).
	evC1 := e2eMakeWireUsageEvent(hubID, agC.ID, proj.ID, runC, "ev-c-1", 1, now, 100, 100)
	recC1 := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agC.ID+"/usage/events", tokC, telemetrycontract.UsageIngestRequest{
		RunID:         runC,
		ProducerEpoch: "ep-1",
		ProducerSeq:   1,
		Events:        []telemetrycontract.UsageEvent{evC1},
	})
	require.Equal(t, http.StatusOK, recC1.Code, recC1.Body.String())

	recAddC := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/groups/"+groupG.ID+"/members", map[string]any{
		"memberType": "user",
		"memberId":   userC.ID,
		"role":       "member",
	})
	require.Equal(t, http.StatusCreated, recAddC.Code, recAddC.Body.String())

	evC2 := e2eMakeWireUsageEvent(hubID, agC.ID, proj.ID, runC, "ev-c-2", 2, now, 60, 40)
	recC2 := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agC.ID+"/usage/events", tokC, telemetrycontract.UsageIngestRequest{
		RunID:         runC,
		ProducerEpoch: "ep-1",
		ProducerSeq:   2,
		Events:        []telemetrycontract.UsageEvent{evC2},
	})
	require.Equal(t, http.StatusOK, recC2.Code, recC2.Body.String())

	sumG, err = st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthStart.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.Equal(t, int64(1150), sumG, "joining groupG must charge only future spend")

	// 6. Experiment OFF: ingest writes no group rollups and admission returns Exhausted=false.
	setTokenomicsExperimentsForTest(t, srv, true, false)
	evC3 := e2eMakeWireUsageEvent(hubID, agC.ID, proj.ID, runC, "ev-c-3", 3, now, 500, 500)
	recC3 := e2eDoAgentUsageRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agC.ID+"/usage/events", tokC, telemetrycontract.UsageIngestRequest{
		RunID:         runC,
		ProducerEpoch: "ep-1",
		ProducerSeq:   3,
		Events:        []telemetrycontract.UsageEvent{evC3},
	})
	require.Equal(t, http.StatusOK, recC3.Code, recC3.Body.String())

	sumG, err = st.SumUsageGroupRollupTokens(ctx, groupG.ID, "", monthStart, monthStart.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.Equal(t, int64(1150), sumG, "experiment-off ingest must not write group rollups")

	exOff, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, userC.ID, now)
	require.NoError(t, err)
	assert.False(t, exOff.Exhausted)

	// Confirm threshold crossings fired for groupG (warn, exhausted).
	hookMu.Lock()
	var groupGCrossings []string
	for _, c := range crossings {
		if c.ScopeKind == store.UsageScopeKindGroup && c.ScopeID == groupG.ID {
			groupGCrossings = append(groupGCrossings, c.NewState)
		}
	}
	hookMu.Unlock()
	assert.Equal(t, []string{store.UsageBudgetStateWarn, store.UsageBudgetStateExhausted}, groupGCrossings)
}
