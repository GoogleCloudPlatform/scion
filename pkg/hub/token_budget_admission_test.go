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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
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
			Recorded: hubfire.RecordedAuthority{
				DefinitionKind: hubfire.DefinitionKindTrigger,
			},
		},
		Identity:    adminIdent,
		CreatorName: "trigger",
		EdgeCeiling: store.EffectCeiling{
			Kind: store.EffectCeilingPrincipal,
		},
		EdgeProvenance: store.AuthorityProvenance{
			SourcePrincipalKind:         store.DelegationPrincipalUser,
			SourcePrincipalID:           adminIdent.ID(),
			SourceCredentialKind:        store.SourceCredentialScheduler,
			SourceEventID:               "ev-trig-1",
			SourceAuthorizationRevision: 1,
		},
		resolvedRole: &resolvedRole,
		Name:         "scheduled-blocked",
		Task:         "run nightly",
	})
	var bErr *TokenBudgetAdmissionError
	require.ErrorAs(t, headlessErr, &bErr)
	assert.Equal(t, api.LaunchCodeBudgetExhausted, bErr.Code)
	assert.Contains(t, headlessErr.Error(), "trigger dispatch of agent", "O4: wrap text must reflect dispatch kind")
	_, lookupErr := s.GetAgentBySlug(ctx, proj.ID, "scheduled-blocked")
	assert.True(t, errors.Is(lookupErr, store.ErrNotFound), "blocked headless create must not persist agent row")

	// 6b. handleExistingAgent (POST /projects/{id}/agents with existing stopped agent name) -> 429 budget_exhausted.
	stoppedAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "existing-stopped-ag",
		Name:            "existing-stopped-ag",
		ProjectID:       proj.ID,
		RuntimeBrokerID: brokerID,
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAg))
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents", CreateAgentRequest{
		Name:   stoppedAg.Name,
		Resume: true,
	})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "existing stopped agent resume via POST /agents must be blocked: %s", rec.Body.String())
	code, _, details = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, details["limit"])
	assert.Equal(t, store.UsageScopeKindProject, details["scopeKind"])
	assert.Equal(t, proj.ID, details["scopeId"])
	assert.Empty(t, disp.startedAgents)

	// 6c. handleExistingAgent Branch 3 (env-gather recreate of a provisioning agent):
	// must refuse with 429 budget_exhausted BEFORE tearing down the provisioning row or its secrets.
	secBackend := secret.NewLocalBackend(s, "hub-admit-test", "test-signing-key-32-bytes-long!!")
	srv.SetSecretBackend(secBackend)
	provGatherAg := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "prov-gather-ag",
		Name:            "prov-gather-ag",
		ProjectID:       proj.ID,
		RuntimeBrokerID: brokerID,
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(ctx, provGatherAg))
	_, _, err = secBackend.Set(ctx, &secret.SetSecretInput{
		Name:       "AGENT_KEEP_ME",
		Value:      "secret-value",
		Scope:      secret.ScopeAgent,
		ScopeID:    provGatherAg.ID,
		SecretType: secret.TypeEnvironment,
	})
	require.NoError(t, err)
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents", CreateAgentRequest{
		Name:      provGatherAg.Name,
		GatherEnv: true,
	})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "env-gather recreate must be blocked: %s", rec.Body.String())
	code, _, details = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, details["limit"])
	gotProv, err := s.GetAgent(ctx, provGatherAg.ID)
	require.NoError(t, err, "provisioning agent row must survive refused env-gather recreate")
	assert.Equal(t, string(state.PhaseProvisioning), gotProv.Phase)
	keptSec, err := secBackend.Get(ctx, "AGENT_KEEP_ME", secret.ScopeAgent, provGatherAg.ID)
	require.NoError(t, err, "agent-scoped secret must survive refused env-gather recreate")
	assert.Equal(t, "secret-value", keptSec.Value)

	// 6d. submitAgentEnv (POST /projects/{id}/agents/{agentId}/env) -> 429 budget_exhausted, no finalize dispatched.
	dispatchedBefore := len(disp.dispatchedAgents)
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents/"+provGatherAg.ID+"/env", SubmitEnvRequest{
		Env: map[string]string{"MY_VAR": "1"},
	})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "submitAgentEnv must be blocked: %s", rec.Body.String())
	code, _, details = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, details["limit"])
	assert.Len(t, disp.dispatchedAgents, dispatchedBefore)

	// 6e. handleWorkspaceSyncToFinalize (POST /agents/{id}/workspace/sync-to/finalize) -> 429 budget_exhausted, no create dispatched.
	srv.SetStorage(newMockStorage("test-bucket"))
	rec = doBootstrapRequest(t, srv, http.MethodPost, "/api/v1/agents/"+provGatherAg.ID+"/workspace/sync-to/finalize", SyncToFinalizeRequest{
		Manifest: &transfer.Manifest{Version: "1.0"},
	})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "workspace sync-to finalize must be blocked: %s", rec.Body.String())
	code, _, details = decodeAPIErrorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, api.LaunchCodeBudgetExhausted, code)
	assert.Equal(t, store.LimitLLMTokensPerProjectDay, details["limit"])
	assert.Len(t, disp.dispatchedAgents, dispatchedBefore)

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

	// 7. O3: User-scope heartbeat fallback (0 events, cold attrCache -> ResolveBillingUserID):
	//    returns user-scope warn notice.
	billingUser := &store.User{
		ID:          uuid.NewString(),
		Email:       "notice-user@example.com",
		DisplayName: "Notice User",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, billingUser))
	createTokenLimitBinding(t, s, store.LimitLLMTokensPerUserDay,
		store.EntitlementSubjectUser, billingUser.ID, store.QuotaScopeSystem, "", 1000, store.TokenActionPauseNew)
	// Seed user-scope counter to 600/1000 (60% -> warn) and evaluate it.
	ingestTestTokens(t, s, proj.ID, tbStrPtr(billingUser.ID), now, 600, false)
	srv.TokenBudgetEvaluator().EvaluateIngestedEvents(ctx, []*store.UsageEvent{
		{ProjectID: proj.ID, BillingUserID: tbStrPtr(billingUser.ID), EventTS: now},
	})

	// Send an empty heartbeat from a fresh agent owned by billingUser (no prior
	// ingest events, so attrCache has no entry and the handler must resolve
	// billingUser via ResolveBillingUserID).
	userAg := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "user-notice-hb-ag",
		Name:      "user-notice-hb-ag",
		ProjectID: proj.ID,
		OwnerID:   billingUser.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, userAg))
	userRunID := "run-user-notice-1"
	_, err = s.SetAgentRunID(ctx, userAg.ID, userRunID, nil)
	require.NoError(t, err)
	userTok := srv.generateRecordedAgentToken(t, userAg.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(userAg.ID), userTok, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{},
		RunID:         userRunID,
		ProducerEpoch: "ep-user-1",
		ProducerSeq:   1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	resp = telemetrycontract.UsageIngestResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.BudgetNotice, "heartbeat fallback must surface user-scope warn notice")
	assert.Equal(t, store.UsageBudgetStateWarn, resp.BudgetNotice.State)
	assert.Equal(t, store.UsageScopeKindUser, resp.BudgetNotice.ScopeKind)
	assert.Equal(t, store.LimitLLMTokensPerUserDay, resp.BudgetNotice.Limit)
	assert.Equal(t, int64(600), resp.BudgetNotice.Consumed)
	assert.Equal(t, int64(1000), resp.BudgetNotice.Max)
	assert.Equal(t, store.TokenActionPauseNew, resp.BudgetNotice.TokenAction)
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

// TestTokenBudgetEvaluator_IngestAndAdmissionScopeParity pins R1: the exact set
// of (limit, scopeKind, scopeID, projectScopeID) evaluated by
// EvaluateIngestedEventsWithNotice must equal the set checked by
// IsScopeExhaustedForAction for both principal-less and billing-user events.
func TestTokenBudgetEvaluator_IngestAndAdmissionScopeParity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, _, _, _ := setupBudgetAdmissionServer(t, now)
	eval := srv.TokenBudgetEvaluator()

	type scopeTuple struct {
		limitName      string
		scopeKind      string
		scopeID        string
		projectScopeID string
	}

	limitIDToName := make(map[string]string, 8)
	for _, name := range []string{
		store.LimitLLMTokensPerProjectDay,
		store.LimitLLMTokensPerProjectMonth,
		store.LimitLLMTokensPerUserDay,
		store.LimitLLMTokensPerUserMonth,
		store.LimitLLMTokensPerUserProjectDay,
		store.LimitLLMTokensPerUserProjectMonth,
		store.LimitLLMTokensPerGroupDay,
		store.LimitLLMTokensPerGroupMonth,
	} {
		def, err := s.GetLimitDefinitionByName(ctx, name)
		require.NoError(t, err)
		limitIDToName[def.ID] = name
	}

	seedAllCaps := func(projID, userID, groupID string) {
		createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
			store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, projID, 10000, store.TokenActionPauseNew)
		createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectMonth,
			store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, projID, 100000, store.TokenActionPauseNew)
		if userID != "" {
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerUserDay,
				store.EntitlementSubjectUser, userID, store.QuotaScopeSystem, "", 10000, store.TokenActionPauseNew)
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerUserMonth,
				store.EntitlementSubjectUser, userID, store.QuotaScopeSystem, "", 100000, store.TokenActionPauseNew)
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerUserProjectDay,
				store.EntitlementSubjectUser, userID, store.QuotaScopeProject, projID, 10000, store.TokenActionPauseNew)
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerUserProjectMonth,
				store.EntitlementSubjectUser, userID, store.QuotaScopeProject, projID, 100000, store.TokenActionPauseNew)
		}
		if groupID != "" {
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupDay,
				store.EntitlementSubjectGroup, groupID, store.QuotaScopeSystem, "", 10000, store.TokenActionPauseNew)
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupMonth,
				store.EntitlementSubjectGroup, groupID, store.QuotaScopeSystem, "", 100000, store.TokenActionPauseNew)
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupDay,
				store.EntitlementSubjectGroup, groupID, store.QuotaScopeProject, projID, 10000, store.TokenActionPauseNew)
			createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupMonth,
				store.EntitlementSubjectGroup, groupID, store.QuotaScopeProject, projID, 100000, store.TokenActionPauseNew)
		}
	}

	collectPersistedScopes := func(projID, userID, groupID string) map[scopeTuple]bool {
		out := make(map[scopeTuple]bool)
		norm := func(v string) string {
			switch v {
			case projID:
				return "PROJ"
			case userID:
				if userID != "" {
					return "USER"
				}
			case groupID:
				if groupID != "" {
					return "GROUP"
				}
			}
			return v
		}
		collect := func(kind, scopeID, projScopeID string) {
			rows, err := s.ListUsageBudgetStatesForScope(ctx, kind, scopeID, projScopeID, now)
			require.NoError(t, err)
			for _, r := range rows {
				out[scopeTuple{
					limitName:      limitIDToName[r.LimitDefinitionID],
					scopeKind:      r.ScopeKind,
					scopeID:        norm(r.ScopeID),
					projectScopeID: norm(r.ProjectScopeID),
				}] = true
			}
		}
		collect(store.UsageScopeKindProject, projID, "")
		if userID != "" {
			collect(store.UsageScopeKindUser, userID, "")
			collect(store.UsageScopeKindUserProject, userID, projID)
		}
		if groupID != "" {
			collect(store.UsageScopeKindGroup, groupID, "")
			collect(store.UsageScopeKindGroup, groupID, projID)
		}
		return out
	}

	for _, tc := range []struct {
		name      string
		withUser  bool
		withGroup bool
		wantCount int
	}{
		{name: "principal-less", withUser: false, withGroup: false, wantCount: 2},
		{name: "with-billing-user", withUser: true, withGroup: false, wantCount: 6},
		{name: "with-billing-user-and-group", withUser: true, withGroup: true, wantCount: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projIngest := &store.Project{ID: uuid.NewString(), Name: "parity-ingest-" + tc.name, Slug: "parity-ingest-" + tc.name}
			projAdmit := &store.Project{ID: uuid.NewString(), Name: "parity-admit-" + tc.name, Slug: "parity-admit-" + tc.name}
			require.NoError(t, s.CreateProject(ctx, projIngest))
			require.NoError(t, s.CreateProject(ctx, projAdmit))

			var userIngest, userAdmit string
			var userIngestPtr *string
			if tc.withUser {
				uIn := &store.User{ID: uuid.NewString(), Email: "in-" + tc.name + "@example.com", DisplayName: "In", Role: store.UserRoleMember, Status: "active"}
				uAd := &store.User{ID: uuid.NewString(), Email: "ad-" + tc.name + "@example.com", DisplayName: "Ad", Role: store.UserRoleMember, Status: "active"}
				require.NoError(t, s.CreateUser(ctx, uIn))
				require.NoError(t, s.CreateUser(ctx, uAd))
				userIngest = uIn.ID
				userAdmit = uAd.ID
				userIngestPtr = &userIngest
			}

			var grpIngest, grpAdmit string
			var chargedGroups []string
			if tc.withGroup {
				gIn := &store.Group{ID: uuid.NewString(), Name: "gin-" + tc.name, Slug: "gin-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
				gAd := &store.Group{ID: uuid.NewString(), Name: "gad-" + tc.name, Slug: "gad-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
				require.NoError(t, s.CreateGroup(ctx, gIn))
				require.NoError(t, s.CreateGroup(ctx, gAd))
				require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
					GroupID: gIn.ID, MemberType: store.GroupMemberTypeUser, MemberID: userIngest, Role: store.GroupMemberRoleMember,
				}))
				require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
					GroupID: gAd.ID, MemberType: store.GroupMemberTypeUser, MemberID: userAdmit, Role: store.GroupMemberRoleMember,
				}))
				grpIngest = gIn.ID
				grpAdmit = gAd.ID
				chargedGroups = []string{grpIngest}
			}

			seedAllCaps(projIngest.ID, userIngest, grpIngest)
			seedAllCaps(projAdmit.ID, userAdmit, grpAdmit)
			if resolver := srv.PooledGroupResolver(); resolver != nil {
				resolver.InvalidateAll()
			}

			// Under-cap consumption so IsScopeExhaustedForAction evaluates every spec without short-circuiting.
			ingestTestTokens(t, s, projIngest.ID, userIngestPtr, now, 100, false)
			if tc.withUser {
				ingestTestTokens(t, s, projAdmit.ID, &userAdmit, now, 100, false)
			} else {
				ingestTestTokens(t, s, projAdmit.ID, nil, now, 100, false)
			}

			_ = eval.EvaluateIngestedEventsWithNotice(ctx, []*store.UsageEvent{
				{ProjectID: projIngest.ID, BillingUserID: userIngestPtr, ChargedGroupIDs: chargedGroups, EventTS: now},
			})
			status, err := eval.IsScopeExhaustedForAction(ctx, projAdmit.ID, userAdmit, now, store.TokenActionPauseNew)
			require.NoError(t, err)
			require.False(t, status.Exhausted)

			ingestScopes := collectPersistedScopes(projIngest.ID, userIngest, grpIngest)
			admitScopes := collectPersistedScopes(projAdmit.ID, userAdmit, grpAdmit)
			assert.Len(t, ingestScopes, tc.wantCount)
			assert.Equal(t, ingestScopes, admitScopes, "ingest evaluator and admission check must evaluate identical scope sets")
		})
	}
}

// exhaustOnStopDispatcher wraps reincarnateTestDispatcher and exhausts a
// project's token budget during DispatchAgentStop to exercise the worker-side
// TOCTOU re-check in runReincarnationWorker (R2).
type exhaustOnStopDispatcher struct {
	*reincarnateTestDispatcher
	onStop func()
}

func (d *exhaustOnStopDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if d.onStop != nil {
		d.onStop()
	}
	return d.reincarnateTestDispatcher.DispatchAgentStop(ctx, agent)
}

func TestTokenBudgetAdmission_ReincarnateWorkerRecheck(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	baseDisp := newReincarnateTestDispatcher()
	srv, s, proj, brk := setupReincarnateTestServer(t, baseDisp)
	setTokenBudgetExperimentsForTest(t, srv, true, true, true)
	srv.reconcileTokenBudgets(ctx)
	srv.TokenBudgetEvaluator().nowFn = func() time.Time { return now }

	// Cap 500 with 0 tokens consumed initially -> HTTP handler gate passes.
	createTokenLimitBinding(t, s, store.LimitLLMTokensPerProjectDay,
		store.EntitlementSubjectSystemDefault, "", store.QuotaScopeProject, proj.ID, 500, store.TokenActionPauseNew)

	// Exhaust the budget during the worker's Stop step so the worker's
	// pre-start re-check in runReincarnationWorker blocks DispatchAgentStart.
	disp := &exhaustOnStopDispatcher{
		reincarnateTestDispatcher: baseDisp,
		onStop: func() {
			ingestTestTokens(t, s, proj.ID, nil, now, 700, false)
		},
	}
	srv.SetDispatcher(disp)

	agent := newReincarnateTestAgent(t, s, proj, brk, nil)
	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Image: "patched-image:v9"})
	require.Equal(t, http.StatusAccepted, rec.Code, "handler must admit while under cap: %s", rec.Body.String())

	settled := waitForReincarnationSettled(t, s, agent.ID)
	assert.Equal(t, store.AgentReincarnationStateFailed, settled.State)
	assert.Contains(t, settled.Error, "start blocked by token budget:")

	stops, reprovisions, starts := dispatcherCalls(baseDisp)
	assert.Equal(t, 1, stops)
	// 1 reprovision for gen N+1 + 1 rollback re-render to gen N by failAfterProvision.
	assert.Equal(t, 2, reprovisions)
	assert.Zero(t, starts, "worker budget re-check must prevent DispatchAgentStart")

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.NotEqual(t, string(state.PhaseRunning), got.Phase, "agent must remain stopped after worker budget refusal")
}

// ingestGroupTokensForAdmissionTest ingests a usage event attributed to
// billingUserID with ChargedGroupIDs populated so usage_group_rollups_daily
// reflects the group consumption.
func ingestGroupTokensForAdmissionTest(t *testing.T, s store.Store, projectID, billingUserID string, groupIDs []string, ts time.Time, tokens int64) {
	t.Helper()
	ctx := context.Background()
	runID := "run-grp-" + uuid.NewString()
	agentID := uuid.NewString()
	buPtr := tbStrPtr(billingUserID)
	_, err := s.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		HubID:         "hub-admit-test",
		ProjectID:     projectID,
		AgentID:       agentID,
		BillingUserID: buPtr,
		Harness:       "claude",
	})
	require.NoError(t, err)

	ev := &store.UsageEvent{
		EventID:         "ev-grp-" + uuid.NewString(),
		HubID:           "hub-admit-test",
		AgentID:         agentID,
		ProjectID:       projectID,
		RunID:           runID,
		BillingUserID:   buPtr,
		ChargedGroupIDs: groupIDs,
		Harness:         "claude",
		Model:           "claude-sonnet-4-6",
		ModelSource:     "event",
		UsageSource:     store.UsageSourceNative,
		Status:          "success",
		Calls:           1,
		Input:           tbI64Ptr(tokens),
		TokenStatus:     "reported",
		ProducerEpoch:   "ep-grp",
		ProducerSeq:     1,
		EventTS:         ts,
	}
	res, err := s.IngestUsageBatch(ctx, &store.UsageIngestBatchInput{
		AgentID: ev.AgentID,
		Events:  []*store.UsageEvent{ev},
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.Accepted)
}

func TestTokenBudgetAdmission_PooledGroupBudgets(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)

	t.Run("system-scoped pool blocks pause_new and suspend_and_pause, allows non-member", func(t *testing.T) {
		for _, action := range []string{store.TokenActionPauseNew, store.TokenActionSuspendAndPause} {
			t.Run(action, func(t *testing.T) {
				srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)
				member := &store.User{ID: uuid.NewString(), Email: "member-" + action + "@example.com", DisplayName: "Member", Role: store.UserRoleMember, Status: "active"}
				outsider := &store.User{ID: uuid.NewString(), Email: "outsider-" + action + "@example.com", DisplayName: "Outsider", Role: store.UserRoleMember, Status: "active"}
				require.NoError(t, s.CreateUser(ctx, member))
				require.NoError(t, s.CreateUser(ctx, outsider))

				grp := &store.Group{ID: uuid.NewString(), Name: "sys-pool-" + action, Slug: "sys-pool-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
				require.NoError(t, s.CreateGroup(ctx, grp))
				require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
					GroupID: grp.ID, MemberType: store.GroupMemberTypeUser, MemberID: member.ID, Role: store.GroupMemberRoleMember,
				}))

				createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupDay,
					store.EntitlementSubjectGroup, grp.ID, store.QuotaScopeSystem, "", 1000, action)
				srv.PooledGroupResolver().InvalidateAll()

				// Under cap -> admitted.
				ingestGroupTokensForAdmissionTest(t, s, proj.ID, member.ID, []string{grp.ID}, now, 600)
				memberAg := &store.Agent{ID: uuid.NewString(), ProjectID: proj.ID, OwnerID: member.ID}
				require.NoError(t, srv.checkTokenBudgetAdmission(ctx, memberAg, proj))

				// Exhaust system-scoped group pool -> 429 budget_exhausted.
				ingestGroupTokensForAdmissionTest(t, s, proj.ID, member.ID, []string{grp.ID}, now, 500)
				var bErr *TokenBudgetAdmissionError
				require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, memberAg, proj), &bErr)
				assert.Equal(t, http.StatusTooManyRequests, bErr.HTTPStatus)
				assert.Equal(t, api.LaunchCodeBudgetExhausted, bErr.Code)
				assert.Equal(t, store.LimitLLMTokensPerGroupDay, bErr.Details["limit"])
				assert.Equal(t, store.UsageScopeKindGroup, bErr.Details["scopeKind"])
				assert.Equal(t, grp.ID, bErr.Details["scopeId"])
				assert.Equal(t, int64(1100), bErr.Details["consumed"])
				assert.Equal(t, int64(1000), bErr.Details["max"])

				// Non-member in same project -> allowed.
				outsiderAg := &store.Agent{ID: uuid.NewString(), ProjectID: proj.ID, OwnerID: outsider.ID}
				require.NoError(t, srv.checkTokenBudgetAdmission(ctx, outsiderAg, proj))
			})
		}
	})

	t.Run("project-scoped pool blocks only in that project; notify_only never blocks", func(t *testing.T) {
		srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)
		projOther := &store.Project{ID: uuid.NewString(), Name: "other-proj", Slug: "other-proj-" + uuid.NewString()[:8]}
		require.NoError(t, s.CreateProject(ctx, projOther))

		member := &store.User{ID: uuid.NewString(), Email: "proj-member@example.com", DisplayName: "Proj Member", Role: store.UserRoleMember, Status: "active"}
		require.NoError(t, s.CreateUser(ctx, member))

		grpPause := &store.Group{ID: uuid.NewString(), Name: "proj-pause", Slug: "proj-pause-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
		grpNotify := &store.Group{ID: uuid.NewString(), Name: "proj-notify", Slug: "proj-notify-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
		require.NoError(t, s.CreateGroup(ctx, grpPause))
		require.NoError(t, s.CreateGroup(ctx, grpNotify))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: grpPause.ID, MemberType: store.GroupMemberTypeUser, MemberID: member.ID, Role: store.GroupMemberRoleMember,
		}))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: grpNotify.ID, MemberType: store.GroupMemberTypeUser, MemberID: member.ID, Role: store.GroupMemberRoleMember,
		}))

		// Project-scoped pause_new pool on proj.
		createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupDay,
			store.EntitlementSubjectGroup, grpPause.ID, store.QuotaScopeProject, proj.ID, 500, store.TokenActionPauseNew)
		// System-scoped notify_only pool on grpNotify.
		createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupDay,
			store.EntitlementSubjectGroup, grpNotify.ID, store.QuotaScopeSystem, "", 500, store.TokenActionNotifyOnly)
		srv.PooledGroupResolver().InvalidateAll()

		ingestGroupTokensForAdmissionTest(t, s, proj.ID, member.ID, []string{grpPause.ID, grpNotify.ID}, now, 700)

		// In proj -> blocked by project-scoped grpPause.
		var bErr *TokenBudgetAdmissionError
		require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, &store.Agent{ID: uuid.NewString(), ProjectID: proj.ID, OwnerID: member.ID}, proj), &bErr)
		assert.Equal(t, http.StatusTooManyRequests, bErr.HTTPStatus)
		assert.Equal(t, api.LaunchCodeBudgetExhausted, bErr.Code)
		assert.Equal(t, store.UsageScopeKindGroup, bErr.Details["scopeKind"])
		assert.Equal(t, grpPause.ID, bErr.Details["scopeId"])

		// In projOther -> grpPause doesn't apply and grpNotify is notify_only -> allowed.
		require.NoError(t, srv.checkTokenBudgetAdmission(ctx, &store.Agent{ID: uuid.NewString(), ProjectID: projOther.ID, OwnerID: member.ID}, projOther))
	})
}

func TestTokenBudgetIngestNotice_PooledGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)

	member := &store.User{ID: uuid.NewString(), Email: "notice-grp-member@example.com", DisplayName: "Group Member", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(ctx, member))

	grp := &store.Group{ID: uuid.NewString(), Name: "notice-pool", Slug: "notice-pool-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, grp))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: grp.ID, MemberType: store.GroupMemberTypeUser, MemberID: member.ID, Role: store.GroupMemberRoleMember,
	}))

	createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupDay,
		store.EntitlementSubjectGroup, grp.ID, store.QuotaScopeSystem, "", 1000, store.TokenActionSuspendAndPause)
	srv.PooledGroupResolver().InvalidateAll()

	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "grp-notice-ag-1",
		Name:      "grp-notice-ag-1",
		ProjectID: proj.ID,
		OwnerID:   member.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	runID := "run-grp-notice-1"
	_, err := s.SetAgentRunID(ctx, ag.ID, runID, nil)
	require.NoError(t, err)
	tok := srv.generateRecordedAgentToken(t, ag.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	// Ingest event exhausting the group pool -> HTTP 200 with group exhausted budget_notice.
	rec := doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag.ID), tok, telemetrycontract.UsageIngestRequest{
		Events: []telemetrycontract.UsageEvent{
			makeWireUsageEvent("hub-admit-test", ag.ID, proj.ID, runID, "ev-grp-1", 1, now, 1100, 0),
		},
		RunID:         runID,
		ProducerEpoch: "ep-grp-1",
		ProducerSeq:   1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp telemetrycontract.UsageIngestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.BudgetNotice, "ingest response must surface group exhaustion notice")
	assert.Equal(t, store.UsageBudgetStateExhausted, resp.BudgetNotice.State)
	assert.Equal(t, store.UsageScopeKindGroup, resp.BudgetNotice.ScopeKind)
	assert.Equal(t, store.LimitLLMTokensPerGroupDay, resp.BudgetNotice.Limit)
	assert.Equal(t, int64(1100), resp.BudgetNotice.Consumed)
	assert.Equal(t, int64(1000), resp.BudgetNotice.Max)
	assert.Equal(t, store.TokenActionSuspendAndPause, resp.BudgetNotice.TokenAction)

	// Empty heartbeat on a second agent owned by member -> HighestActiveBudgetNotice
	// fallback resolves member's pooled group and returns the group exhaustion notice.
	ag2 := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "grp-notice-ag-2",
		Name:      "grp-notice-ag-2",
		ProjectID: proj.ID,
		OwnerID:   member.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, ag2))
	runID2 := "run-grp-notice-2"
	_, err = s.SetAgentRunID(ctx, ag2.ID, runID2, nil)
	require.NoError(t, err)
	tok2 := srv.generateRecordedAgentToken(t, ag2.ID, proj.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)

	rec = doAgentUsageRequest(t, srv, http.MethodPost, telemetrycontract.UsageIngestAgentPath(ag2.ID), tok2, telemetrycontract.UsageIngestRequest{
		Events:        []telemetrycontract.UsageEvent{},
		RunID:         runID2,
		ProducerEpoch: "ep-grp-2",
		ProducerSeq:   1,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	resp = telemetrycontract.UsageIngestResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.BudgetNotice, "heartbeat fallback must surface group exhaustion notice")
	assert.Equal(t, store.UsageBudgetStateExhausted, resp.BudgetNotice.State)
	assert.Equal(t, store.UsageScopeKindGroup, resp.BudgetNotice.ScopeKind)
	assert.Equal(t, store.LimitLLMTokensPerGroupDay, resp.BudgetNotice.Limit)
	assert.Equal(t, store.TokenActionSuspendAndPause, resp.BudgetNotice.TokenAction)
}

func TestTokenBudgetAdmission_UnmeasuredPolicyGroupCapOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	srv, s, _, proj, _ := setupBudgetAdmissionServer(t, now)

	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingUsageUnmeasuredPolicy] = UsageUnmeasuredPolicyDeny
	require.NoError(t, s.UpdateProject(ctx, proj))

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "hc-unmeasured-grp",
		Slug:    "hc-unmeasured-grp",
		Harness: "hermes",
		Scope:   store.HarnessConfigScopeProject,
		ScopeID: proj.ID,
		Status:  store.HarnessConfigStatusActive,
		Config: &store.HarnessConfigData{
			Harness: "hermes",
			Usage:   &api.HarnessUsageCapabilities{Live: api.HarnessUsageLiveCapabilities{Source: api.UsageLiveSourceNone}},
		},
	}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.NewString(),
		Name:    "hc-measured-grp",
		Slug:    "hc-measured-grp",
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

	member := &store.User{ID: uuid.NewString(), Email: "unmeas-member@example.com", DisplayName: "Member", Role: store.UserRoleMember, Status: "active"}
	outsider := &store.User{ID: uuid.NewString(), Email: "unmeas-outsider@example.com", DisplayName: "Outsider", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(ctx, member))
	require.NoError(t, s.CreateUser(ctx, outsider))

	grp := &store.Group{ID: uuid.NewString(), Name: "unmeas-pool", Slug: "unmeas-pool-" + uuid.NewString()[:8], GroupType: store.GroupTypeExplicit}
	require.NoError(t, s.CreateGroup(ctx, grp))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: grp.ID, MemberType: store.GroupMemberTypeUser, MemberID: member.ID, Role: store.GroupMemberRoleMember,
	}))

	// ONLY a group cap is active (no project/user/user_project caps).
	createTokenLimitBinding(t, s, store.LimitLLMTokensPerGroupDay,
		store.EntitlementSubjectGroup, grp.ID, store.QuotaScopeSystem, "", 10000, store.TokenActionPauseNew)
	srv.PooledGroupResolver().InvalidateAll()

	// Member with unmeasured harness -> refused 403 budget_unmeasured.
	var bErr *TokenBudgetAdmissionError
	require.ErrorAs(t, srv.checkTokenBudgetAdmission(ctx, &store.Agent{
		ID:            uuid.NewString(),
		ProjectID:     proj.ID,
		OwnerID:       member.ID,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "hc-unmeasured-grp"},
	}, proj), &bErr)
	assert.Equal(t, http.StatusForbidden, bErr.HTTPStatus)
	assert.Equal(t, api.LaunchCodeBudgetUnmeasured, bErr.Code)

	// Member with measured harness -> allowed.
	require.NoError(t, srv.checkTokenBudgetAdmission(ctx, &store.Agent{
		ID:            uuid.NewString(),
		ProjectID:     proj.ID,
		OwnerID:       member.ID,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "hc-measured-grp"},
	}, proj))

	// Non-member with unmeasured harness -> allowed (no active cap applies).
	require.NoError(t, srv.checkTokenBudgetAdmission(ctx, &store.Agent{
		ID:            uuid.NewString(),
		ProjectID:     proj.ID,
		OwnerID:       outsider.ID,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "hc-unmeasured-grp"},
	}, proj))
}
