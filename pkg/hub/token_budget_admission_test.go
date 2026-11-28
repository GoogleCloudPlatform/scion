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
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// stopRecordingDispatcher wraps mockDispatcher and records DispatchAgentStop
// calls so tests can assert refused restarts and reincarnations never stop
// the running agent (O1).
type stopRecordingDispatcher struct {
	*mockDispatcher
	stopCalls atomic.Int32
}

func (d *stopRecordingDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	d.stopCalls.Add(1)
	return d.mockDispatcher.DispatchAgentStop(ctx, agent)
}

func setTokenBudgetExperimentsForTest(t *testing.T, srv *Server, usageLedgerOn, tokenBudgetsOn, idleSuspendOn bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.TokenomicsUsageLedger:
			e.Default = usageLedgerOn
		case experiments.TokenomicsTokenBudgets:
			e.Default = tokenBudgetsOn
		case experiments.IdleSuspend:
			e.Default = idleSuspendOn
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

func setupBudgetAdmissionServer(t *testing.T, now time.Time) (*Server, store.Store, *mockDispatcher, *store.Project, string) {
	t.Helper()
	ctx := context.Background()
	srv, s, _, disp := testBootstrapServer(t)
	srv.SetHubID("hub-admit-test")
	setTokenBudgetExperimentsForTest(t, srv, true, true, true)
	srv.reconcileTokenBudgets(ctx)
	srv.TokenBudgetEvaluator().nowFn = func() time.Time { return now }

	projID, brokerID := setupProjectAndBroker(t, s)
	proj, err := s.GetProject(ctx, projID)
	require.NoError(t, err)
	return srv, s, disp, proj, brokerID
}

func createTokenLimitBinding(t *testing.T, s store.Store, limitName, subjectType, subjectID, scopeType, scopeID string, capVal int64, tokenAction string) *store.LimitDefinition {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, limitName)
	require.NoError(t, err)
	var actionPtr *string
	if tokenAction != "" {
		a := tokenAction
		actionPtr = &a
	}
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: def.ID,
		SubjectType:       subjectType,
		SubjectID:         subjectID,
		ScopeType:         scopeType,
		ScopeID:           scopeID,
		Value:             capVal,
		TokenAction:       actionPtr,
	})
	require.NoError(t, err)
	return def
}

func decodeAPIErrorEnvelope(t *testing.T, body []byte) (code, msg string, details map[string]interface{}) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &env), "raw body: %s", string(body))
	return env.Error.Code, env.Error.Message, env.Error.Details
}

func TestTokenBudgetAdmission_ActionsOverridesAndExperimentGate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	t.Run("pause_new blocks with 429 budget_exhausted and full details", func(t *testing.T) {
		srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)
		dayDef := createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
			store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 1000, store.TokenActionPauseNew)

		// Under cap -> allowed.
		ingestTestTokens(t, s, proj.ID, nil, now, 600, false)
		ag := &store.Agent{ID: uuid.NewString(), ProjectID: proj.ID}
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, ag, proj))

		// Exhaust cap -> blocked with 429 budget_exhausted.
		ingestTestTokens(t, s, proj.ID, nil, now, 500, false)
		err := srv.checkTokenBudgetAdmission(ctx, ag, proj)
		var bErr *TokenBudgetAdmissionError
		require.ErrorAs(t, err, &bErr)
		assert.Equal(t, http.StatusTooManyRequests, bErr.HTTPStatus)
		assert.Equal(t, api.LaunchCodeBudgetExhausted, bErr.Code)
		assert.Equal(t, store.LimitLLMTokensPerProjectDay, bErr.Details["limit"])
		assert.Equal(t, store.UsageScopeKindProject, bErr.Details["scopeKind"])
		assert.Equal(t, proj.ID, bErr.Details["scopeId"])
		assert.Equal(t, int64(1100), bErr.Details["consumed"])
		assert.Equal(t, int64(1000), bErr.Details["max"])
		assert.Equal(t, "2026-11-28T00:00:00Z", bErr.Details["windowEnd"])
		assert.Equal(t, "unknown", bErr.Details["coverageStatus"])

		// Active override_until > now -> allowed.
		dayStart, _ := utcDayWindow(now)
		st, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
			LimitDefinitionID: dayDef.ID,
			ScopeKind:         store.UsageScopeKindProject,
			ScopeID:           proj.ID,
			WindowStartUTC:    dayStart,
		})
		require.NoError(t, err)
		future := now.Add(time.Hour)
		st.OverrideUntil = &future
		_, err = s.CompareAndSwapUsageBudgetState(ctx, st.StateVersion, st)
		require.NoError(t, err)
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, ag, proj))

		// Expired override_until < now -> blocked again.
		st2, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
			LimitDefinitionID: dayDef.ID,
			ScopeKind:         store.UsageScopeKindProject,
			ScopeID:           proj.ID,
			WindowStartUTC:    dayStart,
		})
		require.NoError(t, err)
		past := now.Add(-time.Minute)
		st2.OverrideUntil = &past
		_, err = s.CompareAndSwapUsageBudgetState(ctx, st2.StateVersion, st2)
		require.NoError(t, err)
		require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, ag, proj), &bErr)

		// Experiment off -> allowed.
		setTokenBudgetExperimentsForTest(t, srv, true, false, true)
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, ag, proj))
		setTokenBudgetExperimentsForTest(t, srv, false, true, true)
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, ag, proj))
	})

	t.Run("suspend_and_pause blocks; notify_only never blocks", func(t *testing.T) {
		srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)
		createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
			store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 500, store.TokenActionSuspendAndPause)
		ingestTestTokens(t, s, proj.ID, nil, now, 600, false)

		ag := &store.Agent{ID: uuid.NewString(), ProjectID: proj.ID}
		var bErr *TokenBudgetAdmissionError
		require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, ag, proj), &bErr)
		assert.Equal(t, http.StatusTooManyRequests, bErr.HTTPStatus)
		assert.Equal(t, api.LaunchCodeBudgetExhausted, bErr.Code)

		// Separate project with notify_only -> allowed even at 100%+.
		proj2 := &store.Project{ID: uuid.NewString(), Name: "notify-only-proj", Slug: "notify-only-proj"}
		require.NoError(t, s.CreateProject(ctx, proj2))
		createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
			store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj2.ID, 500, store.TokenActionNotifyOnly)
		ingestTestTokens(t, s, proj2.ID, nil, now, 1000, false)

		ag2 := &store.Agent{ID: uuid.NewString(), ProjectID: proj2.ID}
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, ag2, proj2))
	})
}

func TestTokenBudgetAdmission_SubAgentAncestryAndPrincipalLess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)

	// Create a real user row so ResolveBillingUserID matches it.
	u := &store.User{
		ID:          uuid.NewString(),
		Email:       "billing-root@example.com",
		DisplayName: "Billing Root",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, u))

	// User daily budget exhausted (500 tokens).
	createTokenLimitBinding(t, s, store.LimitLLMTokensPerUserDay,
		store.EntitlementSubjectUser, u.ID, store.QuotaScopeSystem, "", 500, store.TokenActionPauseNew)
	ingestTestTokens(t, s, proj.ID, tbStrPtr(u.ID), now, 600, false)

	// Sub-agent with no OwnerID/CreatedBy user, only Ancestry = [u.ID, parentAgentID]:
	// must resolve billing user = u.ID via shared ResolveBillingUserID and be blocked!
	subAgent := &store.Agent{
		ID:        uuid.NewString(),
		ProjectID: proj.ID,
		CreatedBy: uuid.NewString(), // parent agent ID (not a user)
		Ancestry:  []string{u.ID, uuid.NewString()},
	}
	var bErr *TokenBudgetAdmissionError
	require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, subAgent, proj), &bErr)
	assert.Equal(t, api.LaunchCodeBudgetExhausted, bErr.Code)
	assert.Equal(t, store.LimitLLMTokensPerUserDay, bErr.Details["limit"])
	assert.Equal(t, store.UsageScopeKindUser, bErr.Details["scopeKind"])
	assert.Equal(t, u.ID, bErr.Details["scopeId"])

	// Principal-less agent (no matching user in OwnerID / Ancestry / CreatedBy):
	// must NOT be blocked by the user budget!
	principalLess := &store.Agent{
		ID:        uuid.NewString(),
		ProjectID: proj.ID,
	}
	require.NoError(t, srv.checkTokenBudgetAdmission(ctx, principalLess, proj))

	// Now exhaust the project budget: principal-less agent IS blocked by project budget.
	createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
		store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 400, store.TokenActionPauseNew)
	require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, principalLess, proj), &bErr)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, bErr.Details["limit"])
	assert.Equal(t, store.UsageScopeKindProject, bErr.Details["scopeKind"])
}

func TestTokenBudgetAdmission_AllLaunchEntryPointsAndNeverBlocked(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, baseDisp, proj, brokerID := setupBudgetAdmissionServer(t, now)
	disp := &stopRecordingDispatcher{mockDispatcher: baseDisp}
	srv.SetDispatcher(disp)

	// Exhaust project daily budget with suspend_and_pause.
	createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
		store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 500, store.TokenActionSuspendAndPause)
	ingestTestTokens(t, s, proj.ID, nil, now, 700, false)

	// 1. HTTP create (non-provision-only) -> 429 budget_exhausted, no agent created.
	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents", CreateAgentRequest{
		Name: "blocked-create",
	})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "body: %s", rec.Body.String())
	code, _, details := decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, details["limit"])
	assert.Empty(t, disp.dispatchedAgents)

	// 2. HTTP create with ProvisionOnly=true -> succeeds (no container launched),
	//    then POST /start -> 429 budget_exhausted.
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents", CreateAgentRequest{
		Name:          "prov-only-agent",
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "provisionOnly should not be blocked: %s", rec.Body.String())
	var createdResp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createdResp))
	require.NotNil(t, createdResp.Agent)

	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+createdResp.Agent.ID+"/start", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "start of provisioned agent must be blocked: %s", rec.Body.String())
	code, _, _ = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Empty(t, disp.startedAgents)

	// 3. HTTP start (resume) of suspended agent -> 429 budget_exhausted.
	suspendedAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "suspended-ag",
		Name:            "suspended-ag",
		ProjectID:       proj.ID,
		RuntimeBrokerID: brokerID,
		Phase:           string(state.PhaseSuspended),
		SuspendReason:   string(state.SuspendReasonIdle),
	}
	require.NoError(t, s.CreateAgent(ctx, suspendedAg))

	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+suspendedAg.ID+"/start", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "resume must be blocked: %s", rec.Body.String())
	code, _, _ = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Empty(t, disp.startedAgents)

	// 4. HTTP restart of running agent -> 429 budget_exhausted, running agent NOT stopped.
	runningAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "running-ag",
		Name:            "running-ag",
		ProjectID:       proj.ID,
		RuntimeBrokerID: brokerID,
		Phase:           string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			Workspace: "/tmp/shared-ws",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, runningAg))

	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+runningAg.ID+"/restart", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "restart must be blocked: %s", rec.Body.String())
	code, _, _ = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Zero(t, disp.stopCalls.Load(), "refused restart must not dispatch stop")
	gotRunning, err := s.GetAgent(ctx, runningAg.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotRunning.Phase, "refused restart must leave running agent untouched")

	// 5. DM --wake and wakeAgentForDM on suspended agent -> typed 429 budget_exhausted
	//    without dispatching a start (matches existing wake refusal semantics).
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+suspendedAg.ID+"/message", map[string]any{
		"message": "please wake up",
		"wake":    true,
	})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "DM wake must be blocked: %s", rec.Body.String())
	code, _, _ = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Empty(t, disp.startedAgents)

	_, wakeErr := srv.wakeAgentForDM(ctx, suspendedAg)
	require.NotNil(t, wakeErr)
	assert.Equal(t, http.StatusTooManyRequests, wakeErr.HTTPStatus)
	assert.Equal(t, api.LaunchCodeBudgetExhausted, wakeErr.Code)
	assert.Empty(t, disp.startedAgents)

	// 6. Scheduled / trigger headless create (createAgentHeadless) -> refused with TokenBudgetAdmissionError.
	adminIdent := NewAuthenticatedUser(uuid.NewString(), "sched-admin@example.com", "Sched Admin", "admin", "api")
	resolvedRole := headlessResolvedRole{preCapRole: AgentRoleBaseline}
	_, headlessErr := srv.createAgentHeadless(ctx, HeadlessCreate{
		ProjectID: proj.ID,
		Authority: FireAuthority{
			PrincipalKind: store.DelegationPrincipalUser,
			PrincipalID:   adminIdent.ID(),
		},
		Identity:    adminIdent,
		CreatorName: "scheduler",
		EdgeCeiling: store.EffectCeiling{
			Kind: store.EffectCeilingPrincipal,
		},
		EdgeProvenance: store.AuthorityProvenance{
			SourcePrincipalKind:         store.DelegationPrincipalUser,
			SourcePrincipalID:           adminIdent.ID(),
			SourceCredentialKind:        store.SourceCredentialScheduler,
			SourceEventID:               "ev-sched-1",
			SourceScheduleID:            "sched-1",
			SourceAuthorizationRevision: 1,
		},
		resolvedRole: &resolvedRole,
		Name:         "scheduled-blocked",
		Task:         "run nightly",
	})
	var bErr *TokenBudgetAdmissionError
	require.ErrorAs(t, headlessErr, &bErr)
	assert.Equal(t, api.LaunchCodeBudgetExhausted, bErr.Code)
	_, lookupErr := s.GetAgentBySlug(ctx, proj.ID, "scheduled-blocked")
	assert.True(t, errors.Is(lookupErr, store.ErrNotFound), "blocked headless create must not persist agent row")

	// 7. Reincarnate -> 429 budget_exhausted, running agent stays running.
	if proj.Labels == nil {
		proj.Labels = map[string]string{}
	}
	proj.Labels[store.LabelWorkspaceMode] = store.WorkspaceModeShared
	require.NoError(t, s.UpdateProject(ctx, proj))
	brk, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	brk.Capabilities = &store.BrokerCapabilities{
		Reprovision:                  true,
		ReprovisionResetHomeRuntimes: []string{"docker"},
	}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, brk))
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+runningAg.ID+"/reincarnate", map[string]any{})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "reincarnate must be blocked: %s", rec.Body.String())
	code, _, _ = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Zero(t, disp.stopCalls.Load(), "refused reincarnate must not dispatch stop")
	gotRunning, err = s.GetAgent(ctx, runningAg.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotRunning.Phase)

	// 8. NEVER BLOCKED: message to running agent, suspend, stop, delete all succeed!
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+runningAg.ID+"/message", map[string]any{
		"message": "wrap up your work",
	})
	assert.Equal(t, http.StatusOK, rec.Code, "message to running agent must never be blocked: %s", rec.Body.String())

	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+runningAg.ID+"/suspend", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "suspend must never be blocked: %s", rec.Body.String())

	// Put runningAg back to running to verify stop is never blocked.
	require.NoError(t, s.UpdateAgentStatus(ctx, runningAg.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+runningAg.ID+"/stop", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "stop must never be blocked: %s", rec.Body.String())

	rec = doBootstrapRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+runningAg.ID, nil)
	assert.True(t, rec.Code == http.StatusOK || rec.Code == http.StatusNoContent,
		"delete must never be blocked: status=%d body=%s", rec.Code, rec.Body.String())
}

func TestTokenBudgetIngestNotice(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)

	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ingest-notice-ag",
		Name:      "ingest-notice-ag",
		ProjectID: proj.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	runID := "run-notice-1"
	_, err := s.SetAgentRunID(ctx, ag.ID, runID, nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	dayDef := createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
		store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 1000, store.TokenActionNotifyOnly)

	// 1. Under 50% (300/1000) -> HTTP 200, no budget_notice.
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events: []telemetrycontract.UsageEvent{
			makeWireUsageEvent("hub-admit-test", ag.ID, proj.ID, runID, "ev-1", 1, now, 300, 0),
		},
		RunID:         runID,
		ProducerEpoch: "ep-1",
		ProducerSeq:   1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "budget_notice")

	// 2. Cross 50% warn (300+250=550/1000) with notify_only -> HTTP 200, budget_notice state=warn.
	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events: []telemetrycontract.UsageEvent{
			makeWireUsageEvent("hub-admit-test", ag.ID, proj.ID, runID, "ev-2", 2, now, 250, 0),
		},
		RunID:         runID,
		ProducerEpoch: "ep-1",
		ProducerSeq:   2,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.BudgetNotice)
	assert.Equal(t, store.UsageBudgetStateWarn, resp.BudgetNotice.State)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, resp.BudgetNotice.Limit)
	assert.Equal(t, store.UsageScopeKindProject, resp.BudgetNotice.ScopeKind)
	assert.Equal(t, int64(550), resp.BudgetNotice.Consumed)
	assert.Equal(t, int64(1000), resp.BudgetNotice.Max)
	assert.Equal(t, "2026-11-28T00:00:00Z", resp.BudgetNotice.WindowEnd)
	assert.Equal(t, store.TokenActionNotifyOnly, resp.BudgetNotice.TokenAction)

	// 3. Cross 100% exhausted (550+500=1050/1000) -> HTTP 200 (never changes status code), state=exhausted.
	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events: []telemetrycontract.UsageEvent{
			makeWireUsageEvent("hub-admit-test", ag.ID, proj.ID, runID, "ev-3", 3, now, 500, 0),
		},
		RunID:         runID,
		ProducerEpoch: "ep-1",
		ProducerSeq:   3,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	resp = telemetrycontract.UsageIngestResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.BudgetNotice)
	assert.Equal(t, store.UsageBudgetStateExhausted, resp.BudgetNotice.State)
	assert.Equal(t, int64(1050), resp.BudgetNotice.Consumed)

	// 4. Empty heartbeat (0 events) when exhausted -> fallback still returns budget_notice.
	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{},
		RunID:         runID,
		ProducerEpoch: "ep-1",
		ProducerSeq:   3,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	resp = telemetrycontract.UsageIngestResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.BudgetNotice)
	assert.Equal(t, store.UsageBudgetStateExhausted, resp.BudgetNotice.State)

	// 5. Experiment off (tokenomics.token_budgets=false) -> budget_notice omitted (byte-for-byte unchanged).
	setTokenBudgetExperimentsForTest(t, srv, true, false, true)
	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{},
		RunID:         runID,
		ProducerEpoch: "ep-1",
		ProducerSeq:   3,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, strings.Contains(rec.Body.String(), "budget_notice"))

	// 6. Experiment back on with active override -> budget_notice omitted.
	setTokenBudgetExperimentsForTest(t, srv, true, true, true)
	dayStart, _ := utcDayWindow(now)
	st, err := s.GetUsageBudgetState(ctx, store.UsageBudgetStateKey{
		LimitDefinitionID: dayDef.ID,
		ScopeKind:         store.UsageScopeKindProject,
		ScopeID:           proj.ID,
		WindowStartUTC:    dayStart,
	})
	require.NoError(t, err)
	future := now.Add(time.Hour)
	st.OverrideUntil = &future
	_, err = s.CompareAndSwapUsageBudgetState(ctx, st.StateVersion, st)
	require.NoError(t, err)

	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{},
		RunID:         runID,
		ProducerEpoch: "ep-1",
		ProducerSeq:   3,
	})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, strings.Contains(rec.Body.String(), "budget_notice"))
}

func TestTokenBudgetAdmission_UnmeasuredPolicy(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)

	// Register harness configs with each capability class.
	createHC := func(slug string, usage *api.HarnessUsageCapabilities) {
		t.Helper()
		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID:      uuid.NewString(),
			Name:    slug,
			Slug:    slug,
			Harness: slug,
			Scope:   store.HarnessConfigScopeProject,
			ScopeID: proj.ID,
			Status:  store.HarnessConfigStatusActive,
			Config: &store.HarnessConfigData{
				Harness: slug,
				Usage:   usage,
			},
		}))
	}
	createHC("hc-measured", &api.HarnessUsageCapabilities{
		Live: api.HarnessUsageLiveCapabilities{
			Source:                  api.UsageLiveSourceNative,
			TokenTypes:              []string{"input", "output", "cache_read"},
			NotApplicableTokenTypes: []string{"cache_write"},
		},
	})
	createHC("hc-partial", &api.HarnessUsageCapabilities{
		Live: api.HarnessUsageLiveCapabilities{
			Source:     api.UsageLiveSourceNative,
			TokenTypes: []string{"input", "output"},
		},
	})
	createHC("hc-calls-only", &api.HarnessUsageCapabilities{
		Live: api.HarnessUsageLiveCapabilities{
			Source:     api.UsageLiveSourceHooks,
			TokenTypes: nil,
		},
	})
	createHC("hc-unmeasured", &api.HarnessUsageCapabilities{
		Live: api.HarnessUsageLiveCapabilities{
			Source: api.UsageLiveSourceNone,
		},
	})
	createHC("hc-unknown", nil)

	// Project settings API gating when experiment is off vs on (including /settings/resolved, R1).
	setTokenBudgetExperimentsForTest(t, srv, true, false, true)
	recOffBefore := doBootstrapRequest(t, srv, http.MethodGet, "/api/v1/projects/"+proj.ID+"/settings/resolved", nil)
	require.Equal(t, http.StatusOK, recOffBefore.Code)
	assert.NotContains(t, recOffBefore.Body.String(), projectSettingUsageUnmeasuredPolicy)
	assert.NotContains(t, recOffBefore.Body.String(), "usageUnmeasuredPolicy")

	rec := doBootstrapRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/settings", map[string]any{
		"usageUnmeasuredPolicy": "deny",
	})
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code, "PUT with experiment off must return 412: %s", rec.Body.String())

	setTokenBudgetExperimentsForTest(t, srv, true, true, true)
	rec = doBootstrapRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/settings", map[string]any{
		"usageUnmeasuredPolicy": "bogus",
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doBootstrapRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/settings", map[string]any{
		"usageUnmeasuredPolicy": "deny",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	rec = doBootstrapRequest(t, srv, http.MethodGet, "/api/v1/projects/"+proj.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var gotSettings hubclient.ProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotSettings))
	assert.Equal(t, UsageUnmeasuredPolicyDeny, gotSettings.UsageUnmeasuredPolicy)

	// Re-fetch project with updated annotation.
	proj, err := s.GetProject(ctx, proj.ID)
	require.NoError(t, err)

	agentWithHC := func(hc string) *store.Agent {
		return &store.Agent{
			ID:            uuid.NewString(),
			ProjectID:     proj.ID,
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: hc},
		}
	}

	// With deny policy but NO active non-zero token cap -> unmeasured harnesses are ALLOWED.
	for _, hc := range []string{"hc-unmeasured", "hc-unknown", "hc-calls-only"} {
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, agentWithHC(hc), proj), "without active cap, %s must be allowed", hc)
	}

	// Add an active non-zero project token cap.
	createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
		store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 10000, store.TokenActionPauseNew)

	// Under deny + active cap: unmeasured, unknown, and calls_only are REFUSED with 403 budget_unmeasured.
	for _, tc := range []struct {
		hc           string
		wantCoverage string
	}{
		{"hc-unmeasured", MeasurementStatusUnmeasured},
		{"hc-unknown", "unknown"},
		{"hc-calls-only", MeasurementStatusCallsOnly},
	} {
		var bErr *TokenBudgetAdmissionError
		require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, agentWithHC(tc.hc), proj), &bErr, "hc=%s must be refused", tc.hc)
		assert.Equal(t, http.StatusForbidden, bErr.HTTPStatus)
		assert.Equal(t, api.LaunchCodeBudgetUnmeasured, bErr.Code)
		assert.Equal(t, UsageUnmeasuredPolicyDeny, bErr.Details["policy"])
		assert.Equal(t, tc.hc, bErr.Details["harnessConfig"])
		assert.Equal(t, tc.wantCoverage, bErr.Details["coverageStatus"])
	}

	// Measured and partial harnesses are ALLOWED under deny + active cap.
	for _, hc := range []string{"hc-measured", "hc-partial"} {
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, agentWithHC(hc), proj), "hc=%s must be allowed", hc)
	}

	// D8: Shell execution containers consume no LLM tokens and are exempt from
	// BOTH budget_unmeasured (under deny + active cap) AND budget_exhausted.
	shellAg := agentWithHC(config.ShellHarnessConfigName)
	require.NoError(t, srv.checkTokenBudgetAdmission(ctx, shellAg, proj), "shell agent must be exempt from budget_unmeasured")

	// Stale meter alone on a measured harness NEVER blocks admission.
	staleAg := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "stale-meter-ag",
		Name:          "stale-meter-ag",
		ProjectID:     proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "hc-measured"},
		Phase:         string(state.PhaseSuspended),
	}
	require.NoError(t, s.CreateAgent(ctx, staleAg))
	_, err = s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: staleAg.ID,
		MeterHealth: &store.UsageMeterHealthUpdate{
			AgentID:       staleAg.ID,
			RunID:         "run-stale-1",
			ProducerEpoch: "ep-stale",
			LastReceiptAt: now.Add(-2 * time.Hour),
			FinalState:    store.MeterHealthFinalStateOpen,
		},
	})
	require.NoError(t, err)
	require.NoError(t, srv.checkTokenBudgetAdmission(ctx, staleAg, proj), "stale meter alone must never block")

	// Switch policy back to allow -> unmeasured / unknown / calls_only all ALLOWED.
	rec = doBootstrapRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/settings", map[string]any{
		"usageUnmeasuredPolicy": "allow",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	proj, err = s.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	for _, hc := range []string{"hc-unmeasured", "hc-unknown", "hc-calls-only"} {
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, agentWithHC(hc), proj), "under allow policy, %s must be allowed", hc)
	}

	// Restore deny annotation so the experiment-off check below verifies a stored
	// non-default value is completely hidden when tokenomics.token_budgets is off.
	rec = doBootstrapRequest(t, srv, http.MethodPut, "/api/v1/projects/"+proj.ID+"/settings", map[string]any{
		"usageUnmeasuredPolicy": "deny",
	})
	require.Equal(t, http.StatusOK, rec.Code)
	proj, err = s.GetProject(ctx, proj.ID)
	require.NoError(t, err)

	// Also verify D8 shell exemption when the project budget is exhausted:
	// shell agent is still admitted while an LLM agent is refused 429.
	ingestTestTokens(t, s, proj.ID, nil, now, 12000, false)
	require.NoError(t, srv.checkTokenBudgetAdmission(ctx, shellAg, proj), "shell agent must be exempt from budget_exhausted")
	var exhaustedErr *TokenBudgetAdmissionError
	require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, agentWithHC("hc-measured"), proj), &exhaustedErr)
	assert.Equal(t, api.LaunchCodeBudgetExhausted, exhaustedErr.Code)

	// When experiment is off with stored policy annotation, BOTH GET /settings
	// and GET /settings/resolved omit usageUnmeasuredPolicy / scion.io/usage-unmeasured-policy
	// and match the pre-change experiment-off body (R1).
	setTokenBudgetExperimentsForTest(t, srv, true, false, true)
	rec = doBootstrapRequest(t, srv, http.MethodGet, "/api/v1/projects/"+proj.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "usageUnmeasuredPolicy")

	recOffAfter := doBootstrapRequest(t, srv, http.MethodGet, "/api/v1/projects/"+proj.ID+"/settings/resolved", nil)
	require.Equal(t, http.StatusOK, recOffAfter.Code)
	assert.NotContains(t, recOffAfter.Body.String(), projectSettingUsageUnmeasuredPolicy)
	assert.NotContains(t, recOffAfter.Body.String(), "usageUnmeasuredPolicy")
	assert.JSONEq(t, recOffBefore.Body.String(), recOffAfter.Body.String(),
		"experiment-off /settings/resolved must be identical even when annotation is stored")
}

func TestTokenBudgetAdmission_ReincarnateHarnessSwitch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, baseDisp, proj, brokerID := setupBudgetAdmissionServer(t, now)
	disp := &stopRecordingDispatcher{mockDispatcher: baseDisp}
	srv.SetDispatcher(disp)

	// Enable tokenomics + reincarnate_harness_switch.
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.TokenomicsUsageLedger,
			experiments.TokenomicsTokenBudgets,
			experiments.ReincarnateHarnessSwitch:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg

	// Configure project for reincarnation + usage.unmeasured_policy=deny + active token cap.
	if proj.Labels == nil {
		proj.Labels = map[string]string{}
	}
	proj.Labels[store.LabelWorkspaceMode] = store.WorkspaceModeShared
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingUsageUnmeasuredPolicy] = UsageUnmeasuredPolicyDeny
	require.NoError(t, s.UpdateProject(ctx, proj))

	brk, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	brk.Capabilities = &store.BrokerCapabilities{
		Reprovision:                  true,
		ReprovisionResetHomeRuntimes: []string{"docker"},
	}
	require.NoError(t, s.UpdateRuntimeBroker(ctx, brk))

	createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
		store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 10000, store.TokenActionPauseNew)

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "hc-measured",
		Slug:    "hc-measured",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: proj.ID,
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Harness: "claude",
			Usage: &api.HarnessUsageCapabilities{
				Live: api.HarnessUsageLiveCapabilities{
					Source:                  api.UsageLiveSourceNative,
					TokenTypes:              []string{"input", "output", "cache_read"},
					NotApplicableTokenTypes: []string{"cache_write"},
				},
			},
		},
	}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "hc-unmeasured",
		Slug:    "hc-unmeasured",
		Harness: "hermes",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: proj.ID,
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Harness: "hermes",
			Usage: &api.HarnessUsageCapabilities{
				Live: api.HarnessUsageLiveCapabilities{
					Source: api.UsageLiveSourceNone,
				},
			},
		},
	}))

	// Case (b): measured -> unmeasured under deny + active cap:
	// refused at handler with 403 budget_unmeasured; running agent stays running and is never stopped.
	measuredAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "switch-to-unmeasured",
		Name:            "switch-to-unmeasured",
		ProjectID:       proj.ID,
		RuntimeBrokerID: brokerID,
		Runtime:         "docker",
		Phase:           string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "hc-measured",
			Workspace:     "/tmp/shared-ws",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, measuredAg))

	rec := doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+measuredAg.ID+"/reincarnate", map[string]any{
		"harnessConfig": "hc-unmeasured",
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "switching measured -> unmeasured must be refused 403: %s", rec.Body.String())
	code, _, details := decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetUnmeasured, code)
	assert.Equal(t, "hc-unmeasured", details["harnessConfig"])
	assert.Zero(t, disp.stopCalls.Load(), "refused harness-switch reincarnate must not stop running agent")
	gotMeasured, err := s.GetAgent(ctx, measuredAg.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), gotMeasured.Phase)

	// Case (a): unmeasured -> measured under deny + active cap:
	// must be ADMITTED (not refused with 403 budget_unmeasured).
	unmeasuredAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "switch-to-measured",
		Name:            "switch-to-measured",
		ProjectID:       proj.ID,
		RuntimeBrokerID: brokerID,
		Runtime:         "docker",
		Phase:           string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "hc-unmeasured",
			Workspace:     "/tmp/shared-ws",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, unmeasuredAg))

	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+unmeasuredAg.ID+"/reincarnate", map[string]any{
		"harnessConfig": "hc-measured",
	})
	require.Equal(t, http.StatusAccepted, rec.Code, "switching unmeasured -> measured must be admitted: %s", rec.Body.String())
}
