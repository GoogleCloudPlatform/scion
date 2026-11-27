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
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func setTokenBudgetsExperimentsForTest(t *testing.T, srv *Server, ledgerOn, budgetsOn bool) {
	t.Helper()
	var defs []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		switch e.Name {
		case experiments.TokenomicsUsageLedger:
			e.Default = ledgerOn
		case experiments.TokenomicsTokenBudgets:
			e.Default = budgetsOn
		}
		defs = append(defs, e)
	}
	reg, err := experiments.NewRegistry(defs, nil)
	require.NoError(t, err)
	srv.experiments = reg
	if budgetsOn {
		srv.reconcileTokenBudgets(context.Background())
	}
}

func doUserJSONRequest(t *testing.T, srv *Server, method, path string, user *store.User, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewReader(raw)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	jwtTok, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func doAgentJSONRequest(t *testing.T, srv *Server, method, path, agentTok string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewReader(raw)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Scion-Agent-Token", agentTok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func ingestBudgetTestTokens(t *testing.T, ctx context.Context, srv *Server, s store.Store, hubID, eventID, runID, projectID, agentID string, billingUserID *string, ts time.Time, tokens int64) {
	t.Helper()
	_, err := s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: agentID,
		Events: []*store.UsageEvent{{
			HubID:         hubID,
			EventID:       eventID,
			RunID:         runID,
			ProjectID:     projectID,
			AgentID:       agentID,
			BillingUserID: billingUserID,
			Harness:       "claude",
			Model:         "claude-sonnet-4-6",
			ModelSource:   telemetrycontract.ModelSourceEvent,
			UsageSource:   store.UsageSourceNative,
			Status:        telemetrycontract.StatusSuccess,
			Calls:         1,
			Input:         usageInt64p(tokens),
			Output:        usageInt64p(0),
			TokenStatus:   telemetrycontract.TokenStatusReported,
			EventTS:       ts,
			ReceivedAt:    ts,
			ProducerEpoch: "ep-1",
			ProducerSeq:   1,
		}},
	})
	require.NoError(t, err)
	// Evaluate synchronously as the ingest handler does.
	srv.TokenBudgetEvaluator().EvaluateIngestedEvents(ctx, []*store.UsageEvent{{
		ProjectID:     projectID,
		BillingUserID: billingUserID,
		EventTS:       ts,
	}})
}

func TestUsageBudgets_ExperimentOffReturns404(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-bud-off")

	admin := &store.User{ID: uuid.NewString(), Email: "bud-off-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)
	proj := &store.Project{ID: tid("bud-off-proj"), Name: "bud-off", Slug: "bud-off"}
	require.NoError(t, s.CreateProject(ctx, proj))

	cases := []struct {
		name      string
		ledgerOn  bool
		budgetsOn bool
	}{
		{"both_off", false, false},
		{"only_ledger_on", true, false},
		{"only_budgets_on", false, true},
	}

	fakeBudgetID := uuid.NewString()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setTokenBudgetsExperimentsForTest(t, srv, tc.ledgerOn, tc.budgetsOn)
			for _, route := range []struct {
				method string
				path   string
			}{
				{http.MethodGet, "/api/v1/projects/" + proj.ID + "/usage/budgets"},
				{http.MethodGet, "/api/v1/users/" + admin.ID + "/usage/budgets"},
				{http.MethodGet, "/api/v1/me/usage/budgets"},
				{http.MethodPost, "/api/v1/projects/" + proj.ID + "/usage/budgets/" + fakeBudgetID + ":override"},
				{http.MethodPost, "/api/v1/users/" + admin.ID + "/usage/budgets/" + fakeBudgetID + ":override"},
			} {
				rec := doUserJSONRequest(t, srv, route.method, route.path, admin, map[string]any{}, nil)
				assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s should 404 when %s", route.method, route.path, tc.name)
			}
		})
	}
}

func TestUsageBudgets_ReadAndAuthzMatrix(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-bud-read")
	setTokenBudgetsExperimentsForTest(t, srv, true, true)

	admin := &store.User{ID: uuid.NewString(), Email: "br-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	member := &store.User{ID: uuid.NewString(), Email: "br-member@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	outsider := &store.User{ID: uuid.NewString(), Email: "br-outsider@example.com", DisplayName: "Outsider", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)
	require.NoError(t, s.CreateUser(ctx, member))
	bindSystemRoleForUsageReadTest(t, s, member.ID, store.SystemRoleHubMember)
	require.NoError(t, s.CreateUser(ctx, outsider))
	bindSystemRoleForUsageReadTest(t, s, outsider.ID, store.SystemRoleHubMember)

	projA := &store.Project{ID: tid("br-proj-a"), Name: "br-a", Slug: "br-a"}
	projB := &store.Project{ID: tid("br-proj-b"), Name: "br-b", Slug: "br-b"}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))
	bindProjectRoleForUsageReadTest(t, s, member.ID, projA.ID, "project-member")
	bindProjectRoleForUsageReadTest(t, s, outsider.ID, projB.ID, "project-member")

	agA := &store.Agent{ID: tid("br-ag-a"), Slug: "br-ag-a", Name: "br-ag-a", ProjectID: projA.ID, OwnerID: member.ID, CreatedBy: member.ID, Phase: string(state.PhaseRunning)}
	agB := &store.Agent{ID: tid("br-ag-b"), Slug: "br-ag-b", Name: "br-ag-b", ProjectID: projB.ID, OwnerID: outsider.ID, CreatedBy: outsider.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, agA))
	require.NoError(t, s.CreateAgent(ctx, agB))
	_, err := s.SetAgentRunID(ctx, agA.ID, "run-br-a", nil)
	require.NoError(t, err)
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: "run-br-a", Generation: 1, HubID: "hub-bud-read", ProjectID: projA.ID, AgentID: agA.ID, BillingUserID: &member.ID, Harness: "claude",
	})
	require.NoError(t, err)

	tokA := srv.generateRecordedAgentToken(t, agA.ID, projA.ID, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead}, nil)
	tokB := srv.generateRecordedAgentToken(t, agB.ID, projB.ID, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead}, nil)

	// Set a cap on projA day limit (leave month unlimited) and user_project day on both projA and projB for member.
	defProjDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	suspendAct := store.TokenActionSuspendAndPause
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: defProjDay.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           projA.ID,
		Value:             1000,
		TokenAction:       &suspendAct,
	})
	require.NoError(t, err)

	defUPDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerUserProjectDay)
	require.NoError(t, err)
	for _, pid := range []string{projA.ID, projB.ID} {
		_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
			LimitDefinitionID: defUPDay.ID,
			SubjectType:       store.EntitlementSubjectUser,
			SubjectID:         member.ID,
			ScopeType:         store.QuotaScopeProject,
			ScopeID:           pid,
			Value:             500,
		})
		require.NoError(t, err)
	}

	dbNow, err := s.DBNow(ctx)
	require.NoError(t, err)
	ingestBudgetTestTokens(t, ctx, srv, s, "hub-bud-read", "ev-br-1", "run-br-a", projA.ID, agA.ID, &member.ID, dbNow, 600)

	// 1. Project budgets: member, admin, same-project agent -> 200; outsider, cross-project agent -> 403.
	for _, tc := range []struct {
		name string
		rec  *httptest.ResponseRecorder
		want int
	}{
		{"member", doUserJSONRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/usage/budgets", member, nil, nil), http.StatusOK},
		{"admin", doUserJSONRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/usage/budgets", admin, nil, nil), http.StatusOK},
		{"outsider", doUserJSONRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/usage/budgets", outsider, nil, nil), http.StatusForbidden},
		{"same_project_agent", doAgentJSONRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/usage/budgets", tokA, nil), http.StatusOK},
		{"other_project_agent", doAgentJSONRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projA.ID+"/usage/budgets", tokB, nil), http.StatusForbidden},
	} {
		require.Equal(t, tc.want, tc.rec.Code, "%s: %s", tc.name, tc.rec.Body.String())
		if tc.want == http.StatusOK {
			var resp UsageBudgetsResponse
			require.NoError(t, json.Unmarshal(tc.rec.Body.Bytes(), &resp))
			assert.Equal(t, store.UsageScopeKindProject, resp.Scope.Kind)
			assert.Equal(t, projA.ID, resp.Scope.ID)
			require.Len(t, resp.Budgets, 2)

			byName := map[string]UsageBudgetEntry{}
			for _, b := range resp.Budgets {
				byName[b.LimitName] = b
			}
			day := byName[store.LimitLLMTokensPerProjectDay]
			assert.NotEmpty(t, day.ID)
			require.NotNil(t, day.Cap)
			assert.Equal(t, int64(1000), *day.Cap)
			assert.Equal(t, store.TokenActionSuspendAndPause, day.TokenAction)
			assert.Equal(t, int64(600), day.Consumed)
			assert.Equal(t, store.UsageBudgetStateWarn, day.State)
			assert.Equal(t, int64(1), day.StateVersion)

			month := byName[store.LimitLLMTokensPerProjectMonth]
			assert.Empty(t, month.ID, "unlimited scope has no state row")
			assert.Nil(t, month.Cap, "unlimited cap serializes as null")
			assert.Equal(t, int64(600), month.Consumed)
			assert.Equal(t, store.UsageBudgetStateOK, month.State)
			assert.Equal(t, int64(0), month.StateVersion)
		}
	}

	// 2. User budgets & /me/usage/budgets:
	// Member sees own user limits (2) + projA user_project (1), NOT projB (caller cannot see projB).
	recMe := doUserJSONRequest(t, srv, http.MethodGet, "/api/v1/me/usage/budgets", member, nil, nil)
	require.Equal(t, http.StatusOK, recMe.Code, recMe.Body.String())
	var meResp UsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recMe.Body.Bytes(), &meResp))
	assert.Equal(t, store.UsageScopeKindUser, meResp.Scope.Kind)
	assert.Equal(t, member.ID, meResp.Scope.ID)
	require.Len(t, meResp.Budgets, 3, "2 user limits + 1 visible user_project limit (projA only)")
	for _, b := range meResp.Budgets {
		if b.ScopeKind == store.UsageScopeKindUserProject {
			assert.Equal(t, projA.ID, b.ProjectScopeID)
			assert.Equal(t, store.UsageBudgetStateExhausted, b.State)
			assert.Equal(t, int64(600), b.Consumed)
		}
	}

	// Admin reading member's budgets sees BOTH projA and projB user_project rows (admin sees all projects).
	recAdminUser := doUserJSONRequest(t, srv, http.MethodGet, "/api/v1/users/"+member.ID+"/usage/budgets", admin, nil, nil)
	require.Equal(t, http.StatusOK, recAdminUser.Code, recAdminUser.Body.String())
	var adminUserResp UsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recAdminUser.Body.Bytes(), &adminUserResp))
	require.Len(t, adminUserResp.Budgets, 4, "admin sees 2 user limits + projA and projB user_project limits")

	// Outsider reading member's budgets -> 403; agent reading /me or /users -> 403.
	assert.Equal(t, http.StatusForbidden, doUserJSONRequest(t, srv, http.MethodGet, "/api/v1/users/"+member.ID+"/usage/budgets", outsider, nil, nil).Code)
	assert.Equal(t, http.StatusForbidden, doAgentJSONRequest(t, srv, http.MethodGet, "/api/v1/me/usage/budgets", tokA, nil).Code)
	assert.Equal(t, http.StatusForbidden, doAgentJSONRequest(t, srv, http.MethodGet, "/api/v1/users/"+member.ID+"/usage/budgets", tokA, nil).Code)
}

func TestMeUsageBudgetsRead_SelfTokenReadsOwn(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-me-bud-uat")
	setTokenBudgetsExperimentsForTest(t, srv, true, true)

	member := &store.User{ID: uuid.NewString(), Email: "me-bud-uat@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, member))
	bindSystemRoleForUsageReadTest(t, s, member.ID, store.SystemRoleHubMember)

	projA := &store.Project{ID: tid("mbu-proj-a"), Name: "mbu-a", Slug: "mbu-a"}
	projB := &store.Project{ID: tid("mbu-proj-b"), Name: "mbu-b", Slug: "mbu-b"}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))
	bindProjectRoleForUsageReadTest(t, s, member.ID, projA.ID, store.ProjectRoleOwner)
	bindProjectRoleForUsageReadTest(t, s, member.ID, projB.ID, store.ProjectRoleOwner)

	defUPDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerUserProjectDay)
	require.NoError(t, err)
	for _, pid := range []string{projA.ID, projB.ID} {
		_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
			LimitDefinitionID: defUPDay.ID,
			SubjectType:       store.EntitlementSubjectUser,
			SubjectID:         member.ID,
			ScopeType:         store.QuotaScopeProject,
			ScopeID:           pid,
			Value:             1000,
		})
		require.NoError(t, err)
	}

	// Project-boundary UAT scoped to projA restricts user_project rows to projA.
	projUAT := mintHubConfigToken(t, srv, member.ID, projectBoundary(projA.ID), "project:read")
	recProj := doUATUsageGET(t, srv, "/api/v1/me/usage/budgets", projUAT)
	require.Equal(t, http.StatusOK, recProj.Code, recProj.Body.String())
	var resp UsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recProj.Body.Bytes(), &resp))
	var seenProjects []string
	for _, b := range resp.Budgets {
		if b.ScopeKind == store.UsageScopeKindUserProject {
			seenProjects = append(seenProjects, b.ProjectScopeID)
		}
	}
	assert.Equal(t, []string{projA.ID}, seenProjects)
}

func TestUserUsageBudgetsRead_SelfTokenReadsOwn(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-user-bud-uat")
	setTokenBudgetsExperimentsForTest(t, srv, true, true)

	member := &store.User{ID: uuid.NewString(), Email: "u-bud-uat@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	other := &store.User{ID: uuid.NewString(), Email: "u-bud-other@example.com", DisplayName: "Other", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, member))
	bindSystemRoleForUsageReadTest(t, s, member.ID, store.SystemRoleHubMember)
	require.NoError(t, s.CreateUser(ctx, other))

	projA := &store.Project{ID: tid("ubu-proj-a"), Name: "ubu-a", Slug: "ubu-a"}
	projB := &store.Project{ID: tid("ubu-proj-b"), Name: "ubu-b", Slug: "ubu-b"}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))
	bindProjectRoleForUsageReadTest(t, s, member.ID, projA.ID, store.ProjectRoleOwner)
	bindProjectRoleForUsageReadTest(t, s, member.ID, projB.ID, store.ProjectRoleOwner)

	defUPDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerUserProjectDay)
	require.NoError(t, err)
	for _, pid := range []string{projA.ID, projB.ID} {
		_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
			LimitDefinitionID: defUPDay.ID,
			SubjectType:       store.EntitlementSubjectUser,
			SubjectID:         member.ID,
			ScopeType:         store.QuotaScopeProject,
			ScopeID:           pid,
			Value:             1000,
		})
		require.NoError(t, err)
	}

	projUAT := mintHubConfigToken(t, srv, member.ID, projectBoundary(projA.ID), "project:read")
	recSelf := doUATUsageGET(t, srv, "/api/v1/users/"+member.ID+"/usage/budgets", projUAT)
	require.Equal(t, http.StatusOK, recSelf.Code, recSelf.Body.String())
	var resp UsageBudgetsResponse
	require.NoError(t, json.Unmarshal(recSelf.Body.Bytes(), &resp))
	var seenProjects []string
	for _, b := range resp.Budgets {
		if b.ScopeKind == store.UsageScopeKindUserProject {
			seenProjects = append(seenProjects, b.ProjectScopeID)
		}
	}
	assert.Equal(t, []string{projA.ID}, seenProjects)

	// Same token cannot read another user's budgets.
	recOther := doUATUsageGET(t, srv, "/api/v1/users/"+other.ID+"/usage/budgets", projUAT)
	assert.Equal(t, http.StatusForbidden, recOther.Code)
}

func TestUsageBudgets_OverrideAndEvaluatorInteraction(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-bud-ovr")
	setTokenBudgetsExperimentsForTest(t, srv, true, true)

	// Pin evaluator clock to midday UTC so override_until fits comfortably inside the day window.
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	eval := srv.TokenBudgetEvaluator()
	eval.nowFn = func() time.Time { return now }

	var hookMu sync.Mutex
	var crossings []BudgetThresholdCrossing
	eval.SetThresholdHook(func(_ context.Context, c BudgetThresholdCrossing) {
		hookMu.Lock()
		crossings = append(crossings, c)
		hookMu.Unlock()
	})

	hubAdmin := &store.User{ID: uuid.NewString(), Email: "ovr-hub-admin@example.com", DisplayName: "HubAdmin", Role: "admin", Status: "active"}
	projAdmin := &store.User{ID: uuid.NewString(), Email: "ovr-proj-admin@example.com", DisplayName: "ProjAdmin", Role: "member", Status: "active"}
	member := &store.User{ID: uuid.NewString(), Email: "ovr-member@example.com", DisplayName: "Member", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	bindSystemRoleForUsageReadTest(t, s, hubAdmin.ID, store.SystemRoleHubAdmin)
	require.NoError(t, s.CreateUser(ctx, projAdmin))
	bindSystemRoleForUsageReadTest(t, s, projAdmin.ID, store.SystemRoleHubMember)
	require.NoError(t, s.CreateUser(ctx, member))
	bindSystemRoleForUsageReadTest(t, s, member.ID, store.SystemRoleHubMember)

	proj := &store.Project{ID: tid("ovr-proj"), Name: "ovr-proj", Slug: "ovr-proj"}
	require.NoError(t, s.CreateProject(ctx, proj))
	bindProjectRoleForUsageReadTest(t, s, projAdmin.ID, proj.ID, "project-admin")
	bindProjectRoleForUsageReadTest(t, s, member.ID, proj.ID, "project-member")

	ag := &store.Agent{ID: tid("ovr-ag"), Slug: "ovr-ag", Name: "ovr-ag", ProjectID: proj.ID, OwnerID: member.ID, CreatedBy: member.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-ovr", nil)
	require.NoError(t, err)
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: "run-ovr", Generation: 1, HubID: "hub-bud-ovr", ProjectID: proj.ID, AgentID: ag.ID, BillingUserID: &member.ID, Harness: "claude",
	})
	require.NoError(t, err)

	defProjDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: defProjDay.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           proj.ID,
		Value:             1000,
	})
	require.NoError(t, err)

	defUserDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerUserDay)
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: defUserDay.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         member.ID,
		ScopeType:         store.QuotaScopeSystem,
		Value:             1000,
	})
	require.NoError(t, err)

	// Exhaust both project and user day budgets.
	ingestBudgetTestTokens(t, ctx, srv, s, "hub-bud-ovr", "ev-ovr-1", "run-ovr", proj.ID, ag.ID, &member.ID, now, 1200)

	dayStart, _ := utcDayWindow(now)
	projState, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: defProjDay.ID, ScopeKind: store.UsageScopeKindProject, ScopeID: proj.ID, WindowStartUTC: dayStart,
	})
	require.NoError(t, err)
	require.Equal(t, store.UsageBudgetStateExhausted, projState.State)

	// Seed thresholds_notified bits to verify override does NOT clear them.
	projState.ThresholdsNotified = store.UsageBudgetThresholdBit50 | store.UsageBudgetThresholdBit80 | store.UsageBudgetThresholdBit100
	projState, err = s.CompareAndSwapUsageBudgetState(ctx, projState.StateVersion, projState)
	require.NoError(t, err)

	userState, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: defUserDay.ID, ScopeKind: store.UsageScopeKindUser, ScopeID: member.ID, WindowStartUTC: dayStart,
	})
	require.NoError(t, err)
	require.Equal(t, store.UsageBudgetStateExhausted, userState.State)

	overrideUntil := now.Add(2 * time.Hour)
	projOverridePath := "/api/v1/projects/" + proj.ID + "/usage/budgets/" + projState.ID + ":override"
	userOverridePath := "/api/v1/users/" + member.ID + "/usage/budgets/" + userState.ID + ":override"

	// Authz denials: member cannot override project budget; member & projAdmin cannot override user budget.
	assert.Equal(t, http.StatusForbidden, doUserJSONRequest(t, srv, http.MethodPost, projOverridePath, member, map[string]any{
		"override_until": overrideUntil, "state_version": projState.StateVersion,
	}, nil).Code)
	assert.Equal(t, http.StatusForbidden, doUserJSONRequest(t, srv, http.MethodPost, userOverridePath, member, map[string]any{
		"override_until": overrideUntil, "state_version": userState.StateVersion,
	}, nil).Code)
	assert.Equal(t, http.StatusForbidden, doUserJSONRequest(t, srv, http.MethodPost, userOverridePath, projAdmin, map[string]any{
		"override_until": overrideUntil, "state_version": userState.StateVersion,
	}, nil).Code)

	// Cross-scope ID mismatch -> 404.
	assert.Equal(t, http.StatusNotFound, doUserJSONRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/usage/budgets/"+userState.ID+":override", projAdmin, map[string]any{
		"override_until": overrideUntil, "state_version": userState.StateVersion,
	}, nil).Code)

	// Missing CAS version -> 428.
	assert.Equal(t, http.StatusPreconditionRequired, doUserJSONRequest(t, srv, http.MethodPost, projOverridePath, projAdmin, map[string]any{
		"override_until": overrideUntil,
	}, nil).Code)

	// Stale CAS version -> 412.
	assert.Equal(t, http.StatusPreconditionFailed, doUserJSONRequest(t, srv, http.MethodPost, projOverridePath, projAdmin, map[string]any{
		"override_until": overrideUntil,
	}, map[string]string{"If-Match": `"999"`}).Code)

	// Out-of-window override_until -> 400.
	assert.Equal(t, http.StatusBadRequest, doUserJSONRequest(t, srv, http.MethodPost, projOverridePath, projAdmin, map[string]any{
		"override_until": dayStart.AddDate(0, 0, 2), "state_version": projState.StateVersion,
	}, nil).Code)

	// Valid override by project admin -> 200, state=ok, thresholds_notified preserved, audit written.
	recOvr := doUserJSONRequest(t, srv, http.MethodPost, projOverridePath, projAdmin, map[string]any{
		"override_until": overrideUntil,
		"state_version":  projState.StateVersion,
		"reason":         "incident mitigation",
	}, nil)
	require.Equal(t, http.StatusOK, recOvr.Code, recOvr.Body.String())
	var ovrEntry UsageBudgetEntry
	require.NoError(t, json.Unmarshal(recOvr.Body.Bytes(), &ovrEntry))
	assert.Equal(t, store.UsageBudgetStateOK, ovrEntry.State)
	assert.Equal(t, projState.StateVersion+1, ovrEntry.StateVersion)
	require.NotNil(t, ovrEntry.OverrideUntil)
	assert.True(t, ovrEntry.OverrideUntil.Equal(overrideUntil))
	assert.Equal(t, store.UsageBudgetThresholdBit50|store.UsageBudgetThresholdBit80|store.UsageBudgetThresholdBit100, ovrEntry.ThresholdsNotified,
		"override must NOT clear thresholds_notified")

	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetID: projState.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Equal(t, "usage_budget_override", audits[0].MutationType)
	assert.Equal(t, projAdmin.ID, audits[0].ActorPrincipalID)

	// Hub admin overrides user budget via If-Match header -> 200.
	recUserOvr := doUserJSONRequest(t, srv, http.MethodPost, userOverridePath, hubAdmin, map[string]any{
		"override_until": overrideUntil,
	}, map[string]string{"If-Match": `"1"`})
	require.Equal(t, http.StatusOK, recUserOvr.Code, recUserOvr.Body.String())

	// While override is active, further ingest keeps state=ok and fires NO new threshold crossings.
	hookMu.Lock()
	crossingsBefore := len(crossings)
	hookMu.Unlock()

	now = now.Add(30 * time.Minute)
	ingestBudgetTestTokens(t, ctx, srv, s, "hub-bud-ovr", "ev-ovr-2", "run-ovr", proj.ID, ag.ID, &member.ID, now, 500)

	projAfterIngest, err := s.GetUsageBudgetStateByID(ctx, projState.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateOK, projAfterIngest.State, "state must stay ok while override_until > now")
	assert.Equal(t, int64(1700), projAfterIngest.ConsumedAtEval)

	exhaustedStatus, err := eval.IsScopeExhaustedForAction(ctx, proj.ID, member.ID, now)
	require.NoError(t, err)
	assert.False(t, exhaustedStatus.Exhausted, "override must suppress exhaustion while active")

	hookMu.Lock()
	assert.Equal(t, crossingsBefore, len(crossings), "no threshold hook should fire while override is active")
	hookMu.Unlock()

	// Advance clock past override_until -> next evaluation resumes rising to exhausted and fires hook.
	now = overrideUntil.Add(time.Minute)
	ingestBudgetTestTokens(t, ctx, srv, s, "hub-bud-ovr", "ev-ovr-3", "run-ovr", proj.ID, ag.ID, &member.ID, now, 100)

	projAfterExpiry, err := s.GetUsageBudgetStateByID(ctx, projState.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, projAfterExpiry.State, "state must resume rising after override_until elapses")

	hookMu.Lock()
	assert.Greater(t, len(crossings), crossingsBefore, "threshold hook should fire once override expires and state re-crosses")
	hookMu.Unlock()

	// Set override again, then clear it with override_until=null -> immediately re-evaluates back to exhausted.
	overrideUntil2 := now.Add(time.Hour)
	recOvr2 := doUserJSONRequest(t, srv, http.MethodPost, projOverridePath, projAdmin, map[string]any{
		"override_until": overrideUntil2,
		"state_version":  projAfterExpiry.StateVersion,
	}, nil)
	require.Equal(t, http.StatusOK, recOvr2.Code, recOvr2.Body.String())
	var ovr2 UsageBudgetEntry
	require.NoError(t, json.Unmarshal(recOvr2.Body.Bytes(), &ovr2))
	assert.Equal(t, store.UsageBudgetStateOK, ovr2.State)

	recClear := doUserJSONRequest(t, srv, http.MethodPost, projOverridePath, projAdmin, map[string]any{
		"override_until": nil,
		"state_version":  ovr2.StateVersion,
	}, nil)
	require.Equal(t, http.StatusOK, recClear.Code, recClear.Body.String())
	var cleared UsageBudgetEntry
	require.NoError(t, json.Unmarshal(recClear.Body.Bytes(), &cleared))
	assert.Nil(t, cleared.OverrideUntil)
	assert.Equal(t, store.UsageBudgetStateExhausted, cleared.State)
}

func TestUsageBudgets_LimitChangeReevaluation(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-bud-reeval")
	setTokenBudgetsExperimentsForTest(t, srv, true, true)

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	eval := srv.TokenBudgetEvaluator()
	eval.nowFn = func() time.Time { return now }

	admin := &store.User{ID: uuid.NewString(), Email: "re-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)

	proj := &store.Project{ID: tid("re-proj"), Name: "re-proj", Slug: "re-proj"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{ID: tid("re-ag"), Slug: "re-ag", Name: "re-ag", ProjectID: proj.ID, OwnerID: admin.ID, CreatedBy: admin.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-re", nil)
	require.NoError(t, err)
	_, err = s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID: "run-re", Generation: 1, HubID: "hub-bud-reeval", ProjectID: proj.ID, AgentID: ag.ID, BillingUserID: &admin.ID, Harness: "claude",
	})
	require.NoError(t, err)

	defProjDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)

	// Ingest 1000 tokens before any cap exists.
	ingestBudgetTestTokens(t, ctx, srv, s, "hub-bud-reeval", "ev-re-1", "run-re", proj.ID, ag.ID, &admin.ID, now, 1000)
	dayStart, _ := utcDayWindow(now)

	// 1. Create a binding with cap=800 (lower than consumed=1000) -> immediately creates exhausted state.
	recCreate := doUserJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+defProjDay.ID+"/entitlements", admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     proj.ID,
		"value":       800,
	}, nil)
	require.Equal(t, http.StatusCreated, recCreate.Code, recCreate.Body.String())
	var binding store.EntitlementBinding
	require.NoError(t, json.Unmarshal(recCreate.Body.Bytes(), &binding))

	st, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: defProjDay.ID, ScopeKind: store.UsageScopeKindProject, ScopeID: proj.ID, WindowStartUTC: dayStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, st.State)

	// Seed all three notification bits.
	st.ThresholdsNotified = store.UsageBudgetThresholdBit50 | store.UsageBudgetThresholdBit80 | store.UsageBudgetThresholdBit100
	st, err = s.CompareAndSwapUsageBudgetState(ctx, st.StateVersion, st)
	require.NoError(t, err)

	// 2. Raise cap to 1500 (1000/1500 = 66.7% -> warn, >=50% and <80%) -> lowers to warn and clears 80% & 100% bits, keeping 50% bit.
	recRaiseWarn := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     proj.ID,
		"value":       1500,
	}, nil)
	require.Equal(t, http.StatusOK, recRaiseWarn.Code, recRaiseWarn.Body.String())

	stWarn, err := s.GetUsageBudgetStateByID(ctx, st.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, stWarn.State)
	assert.Equal(t, store.UsageBudgetThresholdBit50, stWarn.ThresholdsNotified, "80% and 100% bits cleared; 50% bit retained")

	// 3. Raise cap to 5000 (20% -> ok) -> lowers to ok and clears 50% bit too.
	recRaiseOK := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     proj.ID,
		"value":       5000,
	}, nil)
	require.Equal(t, http.StatusOK, recRaiseOK.Code, recRaiseOK.Body.String())

	stOK, err := s.GetUsageBudgetStateByID(ctx, st.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateOK, stOK.State)
	assert.Equal(t, int64(0), stOK.ThresholdsNotified)

	// 4. Lower cap back to 900 -> raises ok -> exhausted.
	recLower := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+binding.ID, admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     proj.ID,
		"value":       900,
	}, nil)
	require.Equal(t, http.StatusOK, recLower.Code, recLower.Body.String())

	stExh2, err := s.GetUsageBudgetStateByID(ctx, st.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, stExh2.State)

	// 5. Delete binding (reverts to default 0 = unlimited) -> lowers exhausted -> ok.
	recDel := doUserJSONRequest(t, srv, http.MethodDelete, "/api/v1/admin/entitlements/"+binding.ID, admin, nil, nil)
	require.Equal(t, http.StatusNoContent, recDel.Code, recDel.Body.String())

	stAfterDel, err := s.GetUsageBudgetStateByID(ctx, st.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateOK, stAfterDel.State)
	assert.Equal(t, int64(0), stAfterDel.CapAtEval)

	// 6. System-wide LimitDefinition.DefaultValue change: lower to 500 -> exhausted; raise to 5000 -> ok.
	recDefLower := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+defProjDay.ID, admin, map[string]any{
		"name":         defProjDay.Name,
		"resourceType": defProjDay.ResourceType,
		"unit":         defProjDay.Unit,
		"description":  defProjDay.Description,
		"defaultValue": 500,
	}, nil)
	require.Equal(t, http.StatusOK, recDefLower.Code, recDefLower.Body.String())
	stDefExh, err := s.GetUsageBudgetStateByID(ctx, st.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, stDefExh.State)

	recDefRaise := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/limits/"+defProjDay.ID, admin, map[string]any{
		"name":         defProjDay.Name,
		"resourceType": defProjDay.ResourceType,
		"unit":         defProjDay.Unit,
		"description":  defProjDay.Description,
		"defaultValue": 5000,
	}, nil)
	require.Equal(t, http.StatusOK, recDefRaise.Code, recDefRaise.Body.String())
	stDefOK, err := s.GetUsageBudgetStateByID(ctx, st.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateOK, stDefOK.State)
}

func TestAdminEntitlements_IfMatchCAS(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	setTokenBudgetsExperimentsForTest(t, srv, true, true)

	admin := &store.User{ID: uuid.NewString(), Email: "cas-admin@example.com", DisplayName: "Admin", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, admin))
	bindSystemRoleForUsageReadTest(t, s, admin.ID, store.SystemRoleHubAdmin)

	defProjDay, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)

	suspendAct := store.TokenActionSuspendAndPause
	recCreate := doUserJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+defProjDay.ID+"/entitlements", admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     "proj-cas-http",
		"value":       10000,
		"tokenAction": suspendAct,
	}, nil)
	require.Equal(t, http.StatusCreated, recCreate.Code, recCreate.Body.String())
	etag1 := recCreate.Header().Get("ETag")
	require.NotEmpty(t, etag1)

	var created store.EntitlementBinding
	require.NoError(t, json.Unmarshal(recCreate.Body.Bytes(), &created))

	// GET returns the same ETag.
	recGet := doUserJSONRequest(t, srv, http.MethodGet, "/api/v1/admin/entitlements/"+created.ID, admin, nil, nil)
	require.Equal(t, http.StatusOK, recGet.Code)
	assert.Equal(t, etag1, recGet.Header().Get("ETag"))

	// PUT with matching If-Match succeeds and advances ETag.
	recPut1 := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+created.ID, admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     "proj-cas-http",
		"value":       20000,
		"tokenAction": store.TokenActionNotifyOnly,
	}, map[string]string{"If-Match": etag1})
	require.Equal(t, http.StatusOK, recPut1.Code, recPut1.Body.String())
	etag2 := recPut1.Header().Get("ETag")
	require.NotEmpty(t, etag2)
	assert.NotEqual(t, etag1, etag2)

	// PUT with stale If-Match returns 412 Precondition Failed.
	recStale := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+created.ID, admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     "proj-cas-http",
		"value":       30000,
	}, map[string]string{"If-Match": etag1})
	assert.Equal(t, http.StatusPreconditionFailed, recStale.Code, recStale.Body.String())

	// PUT without If-Match succeeds unconditionally (backwards-compatible) and
	// omitting tokenAction replaces it with nil (pause_new).
	recUncond := doUserJSONRequest(t, srv, http.MethodPut, "/api/v1/admin/entitlements/"+created.ID, admin, map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     "proj-cas-http",
		"value":       40000,
	}, nil)
	require.Equal(t, http.StatusOK, recUncond.Code, recUncond.Body.String())
	var finalBinding store.EntitlementBinding
	require.NoError(t, json.Unmarshal(recUncond.Body.Bytes(), &finalBinding))
	assert.Equal(t, int64(40000), finalBinding.Value)
	assert.Nil(t, finalBinding.TokenAction)
}
