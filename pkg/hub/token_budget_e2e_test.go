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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func setTokenomicsExperimentsForTest(t *testing.T, srv *Server, usageLedgerOn, tokenBudgetsOn bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.TokenomicsUsageLedger:
			e.Default = usageLedgerOn
		case experiments.TokenomicsTokenBudgets:
			e.Default = tokenBudgetsOn
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

func doAdminJSONRequest(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func e2eDoAgentUsageRequest(t *testing.T, srv *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Scion-Agent-Token", token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func e2eMakeWireUsageEvent(hubID, agentID, projectID, runID, eventID string, seq int64, ts time.Time, in, out int64) telemetrycontract.UsageEvent {
	env := telemetrycontract.NewEnvelope(
		telemetrycontract.EventTypeUsage,
		eventID,
		"sciontool/agent/"+agentID,
		ts,
		ts,
	)
	env.HubID = hubID
	env.ProjectID = projectID
	env.AgentID = agentID
	inVal, outVal := in, out
	return telemetrycontract.UsageEvent{
		Envelope:            env,
		UsageSource:         telemetrycontract.UsageSourceNative,
		SourceKind:          "claude.api_request",
		SourceSchemaVersion: "1",
		SourceEventID:       "rid1:" + eventID,
		SourceIdentity:      telemetrycontract.SourceIdentityRequestID,
		SourceOccurrence:    1,
		ProducerEpoch:       "ep-1",
		ProducerSeq:         seq,
		RunID:               runID,
		Harness:             "claude",
		Model:               "claude-sonnet-4-6",
		ModelSource:         telemetrycontract.ModelSourceEvent,
		Status:              telemetrycontract.StatusSuccess,
		Calls:               1,
		Input:               &inVal,
		Output:              &outVal,
		InputDerivation:     telemetrycontract.InputDerivationReported,
		TokenStatus:         telemetrycontract.TokenStatusReported,
		MalformedFields:     []string{},
	}
}

func TestTokenBudget_EndToEnd_IngestToBudgetState(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-budget-e2e")
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileUsageLedger(ctx)
	srv.reconcileTokenBudgets(ctx)

	// Verify all 6 llm_tokens limits were seeded and are returned by GET /api/v1/admin/limits.
	listRec := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusOK, listRec.Code)
	var listResp listLimitDefinitionsResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listResp))

	limitsByName := make(map[string]*store.LimitDefinition)
	for _, def := range listResp.Items {
		limitsByName[def.Name] = def
	}
	for _, wantName := range []string{
		store.LimitLLMTokensPerProjectDay,
		store.LimitLLMTokensPerProjectMonth,
		store.LimitLLMTokensPerUserDay,
		store.LimitLLMTokensPerUserMonth,
		store.LimitLLMTokensPerUserProjectDay,
		store.LimitLLMTokensPerUserProjectMonth,
	} {
		def, ok := limitsByName[wantName]
		require.True(t, ok, "expected seeded limit %s", wantName)
		assert.Equal(t, store.LimitResourceTypeLLMTokens, def.ResourceType)
		assert.Equal(t, store.LimitUnitTokens, def.Unit)
		assert.Equal(t, int64(0), def.DefaultValue)
		assert.True(t, def.System)
	}

	proj := &store.Project{ID: tid("budget-e2e-proj"), Name: "budget-e2e", Slug: "budget-e2e"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:        tid("budget-e2e-agent"),
		Slug:      "budget-e2e-agent",
		Name:      "budget-e2e-agent",
		ProjectID: proj.ID,
		OwnerID:   DevUserID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-budget-e2e", nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	// Admin API validation checks:
	// 1. Reject tokenAction on non-llm_tokens limit.
	nonTokenDef := limitsByName[store.LimitMaxAgentsPerProject]
	require.NotNil(t, nonTokenDef)
	badResRec := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+nonTokenDef.ID+"/entitlements", map[string]any{
		"subjectType": store.EntitlementSubjectUser,
		"subjectId":   DevUserID,
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     proj.ID,
		"value":       5,
		"tokenAction": store.TokenActionNotifyOnly,
	})
	assert.Equal(t, http.StatusBadRequest, badResRec.Code)

	// 2. Reject invalid tokenAction value on llm_tokens limit.
	projDayDef := limitsByName[store.LimitLLMTokensPerProjectDay]
	badActRec := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+projDayDef.ID+"/entitlements", map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"subjectId":   "",
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     proj.ID,
		"value":       1000,
		"tokenAction": "explode",
	})
	assert.Equal(t, http.StatusBadRequest, badActRec.Code)

	// 3. Reject empty subjectId for user and group subjects (relaxation is ONLY for system_default).
	for _, st := range []string{store.EntitlementSubjectUser, store.EntitlementSubjectGroup} {
		badSubjRec := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+projDayDef.ID+"/entitlements", map[string]any{
			"subjectType": st,
			"subjectId":   "",
			"scopeType":   store.QuotaScopeProject,
			"scopeId":     proj.ID,
			"value":       1000,
		})
		assert.Equal(t, http.StatusBadRequest, badSubjRec.Code, "empty subjectId must be rejected for %s", st)
	}

	// 4. Create valid system_default project day binding (cap = 1000, tokenAction = pause_new) via HTTP API.
	createProjBindRec := doAdminJSONRequest(t, srv, http.MethodPost, "/api/v1/admin/limits/"+projDayDef.ID+"/entitlements", map[string]any{
		"subjectType": store.EntitlementSubjectSystemDefault,
		"subjectId":   "",
		"scopeType":   store.QuotaScopeProject,
		"scopeId":     proj.ID,
		"value":       1000,
		"tokenAction": store.TokenActionPauseNew,
	})
	require.Equal(t, http.StatusCreated, createProjBindRec.Code, createProjBindRec.Body.String())
	var createdBind store.EntitlementBinding
	require.NoError(t, json.Unmarshal(createProjBindRec.Body.Bytes(), &createdBind))
	require.NotNil(t, createdBind.TokenAction)
	assert.Equal(t, store.TokenActionPauseNew, *createdBind.TokenAction)

	// Install threshold crossing hook to observe ok -> warn -> exhausted.
	var hookMu sync.Mutex
	var crossings []BudgetThresholdCrossing
	srv.SetBudgetThresholdHook(func(_ context.Context, c BudgetThresholdCrossing) {
		hookMu.Lock()
		crossings = append(crossings, c)
		hookMu.Unlock()
	})

	now := time.Now().UTC().Truncate(time.Second)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	postEvents := func(events ...telemetrycontract.UsageEvent) telemetrycontract.UsageIngestResponse {
		t.Helper()
		rec := e2eDoAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
			RunID:         "run-budget-e2e",
			ProducerEpoch: "ep-1",
			ProducerSeq:   events[len(events)-1].ProducerSeq,
			Events:        events,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp telemetrycontract.UsageIngestResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, telemetrycontract.UsageEnforcementActionNone, resp.Enforcement.Action)
		return resp
	}

	// Event 1: 300 tokens -> state ok, no crossing hook.
	ev1 := e2eMakeWireUsageEvent("hub-budget-e2e", ag.ID, proj.ID, "run-budget-e2e", "ev-1", 1, now, 200, 100)
	resp1 := postEvents(ev1)
	assert.Equal(t, 1, resp1.Accepted)

	st, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           proj.ID,
		WindowStartUTC:    dayStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateOK, st.State)
	assert.Equal(t, int64(300), st.ConsumedAtEval)
	assert.Equal(t, int64(1000), st.CapAtEval)

	// Event 2: +300 tokens (total 600 >= 50%) -> state warn, hook fires ok->warn.
	ev2 := e2eMakeWireUsageEvent("hub-budget-e2e", ag.ID, proj.ID, "run-budget-e2e", "ev-2", 2, now, 200, 100)
	resp2 := postEvents(ev2)
	assert.Equal(t, 1, resp2.Accepted)

	st, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           proj.ID,
		WindowStartUTC:    dayStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, st.State)
	assert.Equal(t, int64(600), st.ConsumedAtEval)

	// Duplicate replay of ev2 -> 0 newly inserted, state stays warn, no extra hook.
	respDup := postEvents(ev2)
	assert.Equal(t, 0, respDup.Accepted)
	assert.Equal(t, 1, respDup.Duplicates)

	// Event 3: +500 tokens (total 1100 >= 100%) -> state exhausted, hook fires warn->exhausted.
	ev3 := e2eMakeWireUsageEvent("hub-budget-e2e", ag.ID, proj.ID, "run-budget-e2e", "ev-3", 3, now, 300, 200)
	resp3 := postEvents(ev3)
	assert.Equal(t, 1, resp3.Accepted)

	st, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           proj.ID,
		WindowStartUTC:    dayStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateExhausted, st.State)
	assert.Equal(t, int64(1100), st.ConsumedAtEval)

	hookMu.Lock()
	require.Len(t, crossings, 2)
	assert.Equal(t, store.UsageBudgetStateOK, crossings[0].PrevState)
	assert.Equal(t, store.UsageBudgetStateWarn, crossings[0].NewState)
	assert.Equal(t, store.UsageBudgetStateWarn, crossings[1].PrevState)
	assert.Equal(t, store.UsageBudgetStateExhausted, crossings[1].NewState)
	hookMu.Unlock()

	// Verify Seam 2 reports exhaustion for pause_new.
	ex, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, DevUserID, now, store.TokenActionPauseNew)
	require.NoError(t, err)
	assert.True(t, ex.Exhausted)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, ex.LimitName)
}

func TestTokenBudget_ExperimentOff_UnchangedAndNoStateRows(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-budget-off")

	// Both off: baseline admin limits & usage responses.
	setTokenomicsExperimentsForTest(t, srv, false, false)
	srv.reconcileTokenBudgets(ctx)

	baseLimitsRec := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusOK, baseLimitsRec.Code)
	baseSummaryRec := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, baseSummaryRec.Code)
	baseMeRec := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/usage/me", nil)
	require.Equal(t, http.StatusOK, baseMeRec.Code)

	// None of the llm_tokens definitions were seeded while off.
	_, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.ErrorIs(t, err, store.ErrNotFound)

	// Toggle token_budgets ON (seeds rows), configure a cap, then toggle OFF again:
	// D5 requires responses to match baseline byte-for-byte even after ON -> OFF!
	setTokenomicsExperimentsForTest(t, srv, true, true)
	srv.reconcileUsageLedger(ctx)
	srv.reconcileTokenBudgets(ctx)

	proj := &store.Project{ID: tid("budget-off-proj"), Name: "budget-off", Slug: "budget-off"}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{ID: tid("budget-off-agent"), Slug: "budget-off-agent", Name: "budget-off-agent", ProjectID: proj.ID, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err = s.SetAgentRunID(ctx, ag.ID, "run-budget-off", nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	projDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: projDayDef.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           proj.ID,
		Value:             100,
	})
	require.NoError(t, err)

	// Turn token_budgets OFF while usage_ledger stays ON.
	setTokenomicsExperimentsForTest(t, srv, true, false)

	afterLimitsRec := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/admin/limits", nil)
	require.Equal(t, http.StatusOK, afterLimitsRec.Code)
	assert.JSONEq(t, baseLimitsRec.Body.String(), afterLimitsRec.Body.String(), "admin/limits must be unchanged when token_budgets is off")

	afterSummaryRec := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, afterSummaryRec.Code)
	assert.JSONEq(t, baseSummaryRec.Body.String(), afterSummaryRec.Body.String(), "admin/usage must be unchanged when token_budgets is off")

	afterMeRec := doAdminJSONRequest(t, srv, http.MethodGet, "/api/v1/usage/me", nil)
	require.Equal(t, http.StatusOK, afterMeRec.Code)
	assert.JSONEq(t, baseMeRec.Body.String(), afterMeRec.Body.String(), "usage/me must be unchanged when token_budgets is off")

	// Ingesting usage while token_budgets is OFF must NOT create any usage_budget_state rows.
	now := time.Now().UTC().Truncate(time.Second)
	ev := e2eMakeWireUsageEvent("hub-budget-off", ag.ID, proj.ID, "run-budget-off", "ev-off-1", 1, now, 500, 500)
	rec := e2eDoAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		RunID: "run-budget-off", ProducerEpoch: "ep-1", ProducerSeq: 1, Events: []telemetrycontract.UsageEvent{ev},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	states, err := s.ListUsageBudgetStatesForScope(ctx, store.UsageScopeKindProject, proj.ID, "", now)
	require.NoError(t, err)
	assert.Empty(t, states, "no usage_budget_state rows may be written when token_budgets is off")

	// With usage_ledger OFF and token_budgets ON, evaluator is also inactive.
	setTokenomicsExperimentsForTest(t, srv, false, true)
	ex, err := srv.TokenBudgetEvaluator().IsScopeExhaustedForAction(ctx, proj.ID, "", now, store.TokenActionPauseNew)
	require.NoError(t, err)
	assert.False(t, ex.Exhausted)
}

func TestTokenBudget_RetentionSweeper(t *testing.T) {
	ctx := context.Background()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	seedTokenBudgetLimitDefinitions(ctx, s)

	projDayDef, err := s.GetLimitDefinitionByName(ctx, store.LimitLLMTokensPerProjectDay)
	require.NoError(t, err)

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	// Closed window 100 days ago -> expired under 90-day default retention.
	expiredStart := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	expiredEnd := expiredStart.AddDate(0, 0, 1)
	_, err = s.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-ret",
		WindowStartUTC:    expiredStart,
		WindowEndUTC:      expiredEnd,
		State:             store.UsageBudgetStateExhausted,
		ConsumedAtEval:    1000,
		CapAtEval:         1000,
	})
	require.NoError(t, err)

	// Active window today -> must NEVER be deleted.
	activeStart := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	activeEnd := activeStart.AddDate(0, 0, 1)
	_, err = s.CreateUsageBudgetState(ctx, &store.UsageBudgetState{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-ret",
		WindowStartUTC:    activeStart,
		WindowEndUTC:      activeEnd,
		State:             store.UsageBudgetStateWarn,
		ConsumedAtEval:    600,
		CapAtEval:         1000,
	})
	require.NoError(t, err)

	sw := NewUsageRetentionSweeper(s, func(context.Context) (time.Time, error) { return now, nil })
	sw.SetBatchYield(0)

	days := 90
	cfg := config.ResolveTelemetryRetention(&api.TelemetryRetentionConfig{BudgetStateDays: &days})
	report := sw.Run(ctx, cfg)
	assert.Equal(t, 0, report.ErrorCount)
	assert.True(t, report.PassCompleted)

	var foundBudgetTable bool
	for _, tr := range report.Tables {
		if tr.Table == "usage_budget_state" {
			foundBudgetTable = true
			assert.Equal(t, int64(1), tr.Deleted)
		}
	}
	assert.True(t, foundBudgetTable, "usage_budget_state must be registered in UsageRetentionSweeper")

	_, err = s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-ret",
		WindowStartUTC:    expiredStart,
	})
	require.ErrorIs(t, err, store.ErrNotFound, "expired closed window should be deleted")

	kept, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: projDayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           "proj-ret",
		WindowStartUTC:    activeStart,
	})
	require.NoError(t, err)
	assert.Equal(t, store.UsageBudgetStateWarn, kept.State, "active window must be preserved")
}
