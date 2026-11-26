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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func bindSystemRoleForUsageReadTest(t *testing.T, s store.Store, userID, roleName string) {
	t.Helper()
	ctx := context.Background()
	role, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: role.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func doAgentUsageGET(t *testing.T, srv *Server, path, agentTok string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Scion-Agent-Token", agentTok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func doUATUsageGET(t *testing.T, srv *Server, path, uatToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+uatToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestScopedUsageRead_ExperimentOffReturns404(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-scope-off")
	setUsageLedgerExperimentForTest(t, srv, false)

	admin := &store.User{ID: uuid.NewString(), Email: "scope-admin-off@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)
	proj := &store.Project{ID: tid("scope-off-proj"), Name: "scope-off", Slug: "scope-off"}
	require.NoError(t, s.CreateProject(ctx, proj))

	for _, path := range []string{
		"/api/v1/projects/" + proj.ID + "/usage",
		"/api/v1/users/" + admin.ID + "/usage",
		"/api/v1/me/usage",
		"/api/v1/usage",
	} {
		rec := doUserUsageReadRequest(t, srv, path, admin)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path %s should return 404 when experiment is off", path)
	}
}

func TestUsageRoutes_NamespaceDistinctFromQuotaUsageMe(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-ns")
	setUsageLedgerExperimentForTest(t, srv, true)

	admin := &store.User{ID: uuid.NewString(), Email: "ns-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)

	// GET /api/v1/usage -> global token usage (UsageSummaryResponse with scope.kind="global").
	recGlobal := doUserUsageReadRequest(t, srv, "/api/v1/usage", admin)
	require.Equal(t, http.StatusOK, recGlobal.Code, recGlobal.Body.String())
	var globalResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recGlobal.Body.Bytes(), &globalResp))
	assert.Equal(t, store.UsageScopeKindGlobal, globalResp.Scope.Kind)

	// GET /api/v1/usage/me -> quota usage (myUsageResponse with "items"), NOT shadowed by /api/v1/usage.
	recQuotaMe := doUserUsageReadRequest(t, srv, "/api/v1/usage/me", admin)
	require.Equal(t, http.StatusOK, recQuotaMe.Code, recQuotaMe.Body.String())
	var quotaBody map[string]any
	require.NoError(t, json.Unmarshal(recQuotaMe.Body.Bytes(), &quotaBody))
	_, hasItems := quotaBody["items"]
	_, hasScope := quotaBody["scope"]
	assert.True(t, hasItems, "/api/v1/usage/me must resolve to quota usage handler")
	assert.False(t, hasScope, "/api/v1/usage/me must not resolve to Track 1 token usage handler")

	// GET /api/v1/me/usage -> caller's own Track 1 token usage (UsageSummaryResponse with scope.kind="user").
	recTokenMe := doUserUsageReadRequest(t, srv, "/api/v1/me/usage", admin)
	require.Equal(t, http.StatusOK, recTokenMe.Code, recTokenMe.Body.String())
	var meResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recTokenMe.Body.Bytes(), &meResp))
	assert.Equal(t, store.UsageScopeKindUser, meResp.Scope.Kind)
	assert.Equal(t, admin.ID, meResp.Scope.ID)
}

func TestScopedUsageRead_AuthzMatrix(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-authz")
	setUsageLedgerExperimentForTest(t, srv, true)

	admin := &store.User{ID: uuid.NewString(), Email: "u-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	member := &store.User{ID: uuid.NewString(), Email: "u-member@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	outsider := &store.User{ID: uuid.NewString(), Email: "u-outsider@example.com", DisplayName: "Outsider", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)
	require.NoError(t, s.CreateUser(ctx, member))
	require.NoError(t, s.CreateUser(ctx, outsider))

	projA := &store.Project{ID: tid("authz-proj-a"), Name: "authz-proj-a", Slug: "authz-proj-a"}
	projB := &store.Project{ID: tid("authz-proj-b"), Name: "authz-proj-b", Slug: "authz-proj-b"}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))
	bindProjectRoleForUsageReadTest(t, s, member.ID, projA.ID, "project-member")

	agA := &store.Agent{
		ID: tid("authz-ag-a"), Slug: "authz-ag-a", Name: "authz-ag-a",
		ProjectID: projA.ID, OwnerID: member.ID, CreatedBy: member.ID, Phase: string(state.PhaseRunning),
	}
	agB := &store.Agent{
		ID: tid("authz-ag-b"), Slug: "authz-ag-b", Name: "authz-ag-b",
		ProjectID: projB.ID, OwnerID: outsider.ID, CreatedBy: outsider.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agA))
	require.NoError(t, s.CreateAgent(ctx, agB))

	tokA := srv.generateRecordedAgentToken(t, agA.ID, projA.ID, []AgentTokenScope{ScopeProjectRead}, nil)
	tokB := srv.generateRecordedAgentToken(t, agB.ID, projB.ID, []AgentTokenScope{ScopeProjectRead}, nil)

	// 1. GET /api/v1/projects/{id}/usage
	assert.Equal(t, http.StatusUnauthorized, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+projA.ID+"/usage", nil).Code)
	assert.Equal(t, http.StatusOK, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+projA.ID+"/usage", member).Code)
	assert.Equal(t, http.StatusOK, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+projA.ID+"/usage", admin).Code)
	assert.Equal(t, http.StatusForbidden, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+projA.ID+"/usage", outsider).Code)
	assert.Equal(t, http.StatusOK, doAgentUsageGET(t, srv, "/api/v1/projects/"+projA.ID+"/usage", tokA).Code, "agent may read its own project usage")
	assert.Equal(t, http.StatusForbidden, doAgentUsageGET(t, srv, "/api/v1/projects/"+projA.ID+"/usage", tokB).Code, "agent from another project must get 403")

	// 2. GET /api/v1/me/usage
	assert.Equal(t, http.StatusUnauthorized, doUserUsageReadRequest(t, srv, "/api/v1/me/usage", nil).Code)
	assert.Equal(t, http.StatusOK, doUserUsageReadRequest(t, srv, "/api/v1/me/usage", member).Code)
	assert.Equal(t, http.StatusForbidden, doAgentUsageGET(t, srv, "/api/v1/me/usage", tokA).Code, "agent must get 403 on /api/v1/me/usage")

	// 3. GET /api/v1/users/{id}/usage
	assert.Equal(t, http.StatusUnauthorized, doUserUsageReadRequest(t, srv, "/api/v1/users/"+member.ID+"/usage", nil).Code)
	assert.Equal(t, http.StatusOK, doUserUsageReadRequest(t, srv, "/api/v1/users/"+member.ID+"/usage", member).Code, "user reads own usage")
	assert.Equal(t, http.StatusOK, doUserUsageReadRequest(t, srv, "/api/v1/users/"+member.ID+"/usage", admin).Code, "admin reads another user's usage")
	assert.Equal(t, http.StatusForbidden, doUserUsageReadRequest(t, srv, "/api/v1/users/"+member.ID+"/usage", outsider).Code, "unrelated user must get 403")
	assert.Equal(t, http.StatusForbidden, doAgentUsageGET(t, srv, "/api/v1/users/"+member.ID+"/usage", tokA).Code, "agent must get 403 on /api/v1/users/{id}/usage")

	// 4. GET /api/v1/usage (hub admin only)
	assert.Equal(t, http.StatusUnauthorized, doUserUsageReadRequest(t, srv, "/api/v1/usage", nil).Code)
	assert.Equal(t, http.StatusOK, doUserUsageReadRequest(t, srv, "/api/v1/usage", admin).Code)
	assert.Equal(t, http.StatusForbidden, doUserUsageReadRequest(t, srv, "/api/v1/usage", member).Code)
	assert.Equal(t, http.StatusForbidden, doAgentUsageGET(t, srv, "/api/v1/usage", tokA).Code)
}

func TestMeUsageRead_SelfTokenReadsOwn(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-me-uat")
	setUsageLedgerExperimentForTest(t, srv, true)

	member := &store.User{ID: uuid.NewString(), Email: "me-uat@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, member))
	bindSystemRoleForUsageReadTest(t, s, member.ID, store.SystemRoleHubMember)

	rawUAT := mintHubConfigToken(t, srv, member.ID, hubBoundary(), "user:read")

	rec := doUATUsageGET(t, srv, "/api/v1/me/usage", rawUAT)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, member.ID, resp.Scope.ID)
}

func TestUserUsageRead_SelfTokenReadsOwn(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-user-uat")
	setUsageLedgerExperimentForTest(t, srv, true)

	member := &store.User{ID: uuid.NewString(), Email: "user-uat@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	other := &store.User{ID: uuid.NewString(), Email: "other-uat@example.com", DisplayName: "Other", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, member))
	bindSystemRoleForUsageReadTest(t, s, member.ID, store.SystemRoleHubMember)
	require.NoError(t, s.CreateUser(ctx, other))

	rawUAT := mintHubConfigToken(t, srv, member.ID, hubBoundary(), "user:read")

	recSelf := doUATUsageGET(t, srv, "/api/v1/users/"+member.ID+"/usage", rawUAT)
	require.Equal(t, http.StatusOK, recSelf.Code, recSelf.Body.String())

	recOther := doUATUsageGET(t, srv, "/api/v1/users/"+other.ID+"/usage", rawUAT)
	assert.Equal(t, http.StatusForbidden, recOther.Code)
}

func TestScopedUsageRead_GranularityHourlyCoverageGroupByAndNullability(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-gran")
	setUsageLedgerExperimentForTest(t, srv, true)

	admin := &store.User{ID: uuid.NewString(), Email: "gran-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)
	proj := &store.Project{ID: tid("gran-proj"), Name: "gran-proj", Slug: "gran-proj"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag1 := &store.Agent{
		ID: tid("gran-ag-1"), Slug: "gran-ag-1", Name: "gran-ag-1",
		ProjectID: proj.ID, OwnerID: admin.ID, CreatedBy: admin.ID,
		Phase: string(state.PhaseRunning),
	}
	ag2 := &store.Agent{
		ID: tid("gran-ag-2"), Slug: "gran-ag-2", Name: "gran-ag-2",
		ProjectID: proj.ID, OwnerID: admin.ID, CreatedBy: admin.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag1))
	require.NoError(t, s.CreateAgent(ctx, ag2))
	_, err := s.SetAgentRunID(ctx, ag1.ID, "run-gran-1", nil)
	require.NoError(t, err)
	ag1.RunID = "run-gran-1"
	_, err = s.SetAgentRunID(ctx, ag2.ID, "run-gran-2", nil)
	require.NoError(t, err)
	ag2.RunID = "run-gran-2"

	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: ag1.RunID, Generation: 1, HubID: "hub-gran", ProjectID: proj.ID,
		AgentID: ag1.ID, BillingUserID: &admin.ID, Harness: "claude",
	})
	require.NoError(t, err)
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: ag2.RunID, Generation: 1, HubID: "hub-gran", ProjectID: proj.ID,
		AgentID: ag2.ID, BillingUserID: &admin.ID, Harness: "codex",
	})
	require.NoError(t, err)

	// Before any hourly writes: explicit groupBy=hour must return 400; default falls back to day.
	recNoCov := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since=24h&groupBy=hour", admin)
	assert.Equal(t, http.StatusBadRequest, recNoCov.Code)

	recDefaultNoCov := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since=24h", admin)
	require.Equal(t, http.StatusOK, recDefaultNoCov.Code)
	var noCovResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recDefaultNoCov.Body.Bytes(), &noCovResp))
	assert.Equal(t, store.UsageGranularityDay, noCovResp.Window.Granularity)
	assert.Equal(t, MeasurementStatusUnmeasured, noCovResp.MeasurementStatus)
	assert.Equal(t, 2, noCovResp.UnmeasuredAgents)
	assert.Equal(t, 0, noCovResp.StaleAgents)

	// Ingest two events in distinct recent hours (sets hourly coverage start at now-2h).
	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)
	h1 := time.Date(dbNow.Year(), dbNow.Month(), dbNow.Day(), dbNow.Hour(), 0, 0, 0, time.UTC).Add(-2 * time.Hour)
	h2 := h1.Add(time.Hour)

	ev1 := &store.UsageEvent{
		HubID: "hub-gran", EventID: "g-1", RunID: ag1.RunID, ProjectID: proj.ID, AgentID: ag1.ID,
		BillingUserID: &admin.ID, Harness: "claude", Model: "claude-sonnet-4-6",
		ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: store.UsageSourceNative,
		Status: telemetrycontract.StatusSuccess, Calls: 1,
		Input: usageInt64p(8), Output: usageInt64p(5), CacheRead: usageInt64p(3), CacheWrite: usageInt64p(2),
		CacheWrite1h: nil, Reasoning: usageInt64p(2),
		TokenStatus: telemetrycontract.TokenStatusReported, EventTS: h1.Add(10 * time.Minute), ReceivedAt: dbNow,
		ProducerEpoch: "ep-1", ProducerSeq: 1,
	}
	ev2 := &store.UsageEvent{
		HubID: "hub-gran", EventID: "g-2", RunID: ag1.RunID, ProjectID: proj.ID, AgentID: ag1.ID,
		BillingUserID: &admin.ID, Harness: "claude", Model: "claude-haiku-4-5",
		ModelSource: telemetrycontract.ModelSourceEvent, UsageSource: store.UsageSourceNative,
		Status: telemetrycontract.StatusError, Calls: 1,
		Input: usageInt64p(10), Output: usageInt64p(4), CacheRead: usageInt64p(0), CacheWrite: nil,
		CacheWrite1h: nil, Reasoning: nil,
		TokenStatus: telemetrycontract.TokenStatusReported, EventTS: h2.Add(5 * time.Minute), ReceivedAt: dbNow,
		ProducerEpoch: "ep-1", ProducerSeq: 2,
	}
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ag1.ID,
		Events:  []*store.UsageEvent{ev1, ev2},
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID: ag1.ID, RunID: ag1.RunID, ProducerEpoch: "ep-1",
			LastReceiptAt: dbNow.Add(-10 * time.Second), ProducerSeq: 2, ReceivedSeqs: []int64{1, 2},
			FinalState: store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)

	// Window starting BEFORE hourly coverage start (h1 - 24h): falls back to day; explicit groupBy=hour returns 400.
	beforeStart := h1.Add(-24 * time.Hour).Format(time.RFC3339)
	recBefore := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since="+beforeStart, admin)
	require.Equal(t, http.StatusOK, recBefore.Code, recBefore.Body.String())
	var beforeResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recBefore.Body.Bytes(), &beforeResp))
	assert.Equal(t, store.UsageGranularityDay, beforeResp.Window.Granularity)

	recBeforeHour := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since="+beforeStart+"&groupBy=hour", admin)
	assert.Equal(t, http.StatusBadRequest, recBeforeHour.Code)

	// Window starting AT hourly coverage start (h1) and within 14d: selects hourly!
	atStart := h1.Format(time.RFC3339)
	recHour := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since="+atStart+"&groupBy=model,hour", admin)
	require.Equal(t, http.StatusOK, recHour.Code, recHour.Body.String())
	var hourResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recHour.Body.Bytes(), &hourResp))
	assert.Equal(t, store.UsageGranularityHour, hourResp.Window.Granularity)
	assert.Equal(t, int64(2), hourResp.Calls)
	assert.Equal(t, int64(1), hourResp.ErrorCalls)
	assert.Equal(t, int64(2), hourResp.Events)
	require.NotNil(t, hourResp.Tokens.Input)
	assert.Equal(t, int64(18), *hourResp.Tokens.Input)
	require.NotNil(t, hourResp.Tokens.CacheWrite)
	assert.Equal(t, int64(2), *hourResp.Tokens.CacheWrite)
	assert.Nil(t, hourResp.Tokens.CacheWrite1h)
	require.NotNil(t, hourResp.Tokens.UnknownEvents)
	assert.Equal(t, int64(1), hourResp.Tokens.UnknownEvents.CacheWrite, "ev2 had NULL cache_write")
	assert.Equal(t, int64(1), hourResp.Tokens.UnknownEvents.Reasoning, "ev2 had NULL reasoning")
	require.Len(t, hourResp.Groups, 2)
	// Second group (claude-haiku-4-5) has measured 0 cache_read and NULL cache_write!
	require.NotNil(t, hourResp.Groups[1].Tokens.CacheRead)
	assert.Equal(t, int64(0), *hourResp.Groups[1].Tokens.CacheRead)
	assert.Nil(t, hourResp.Groups[1].Tokens.CacheWrite)
	assert.Equal(t, MeasurementStatusMeasured, hourResp.MeasurementStatus)
	assert.Equal(t, 1, hourResp.UnmeasuredAgents, "ag2 has no events")
	assert.Equal(t, 0, hourResp.StaleAgents)
	require.NotNil(t, hourResp.LastReceiptAt)

	// Size bound: limit=1 returns 1 group with truncated=true and exact full totals.
	recBound := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since="+atStart+"&groupBy=model&limit=1", admin)
	require.Equal(t, http.StatusOK, recBound.Code, recBound.Body.String())
	var boundResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recBound.Body.Bytes(), &boundResp))
	assert.True(t, boundResp.Truncated)
	assert.Len(t, boundResp.Groups, 1)
	assert.Equal(t, int64(2), boundResp.Calls, "totals remain exact when groups are truncated")

	// Validation errors: invalid groupBy, day+hour together, limit > 500, conflicting project_id.
	assert.Equal(t, http.StatusBadRequest, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?groupBy=bogus", admin).Code)
	assert.Equal(t, http.StatusBadRequest, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?groupBy=day,hour", admin).Code)
	assert.Equal(t, http.StatusBadRequest, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?limit=501", admin).Code)
	assert.Equal(t, http.StatusBadRequest, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?project_id=other", admin).Code)

	// Window older than 14d falls back to day and rejects groupBy=hour.
	oldSince := dbNow.AddDate(0, 0, -20).Format("2006-01-02")
	recOld := doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since="+oldSince, admin)
	require.Equal(t, http.StatusOK, recOld.Code)
	var oldResp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(recOld.Body.Bytes(), &oldResp))
	assert.Equal(t, store.UsageGranularityDay, oldResp.Window.Granularity)
	assert.Equal(t, http.StatusBadRequest, doUserUsageReadRequest(t, srv, "/api/v1/projects/"+proj.ID+"/usage?since="+oldSince+"&groupBy=hour", admin).Code)
}
