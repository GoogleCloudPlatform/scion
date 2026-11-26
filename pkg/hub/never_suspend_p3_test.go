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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func setProjectNeverSuspendCeiling(t *testing.T, s store.Store, ceiling int64) {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
	require.NoError(t, err, "max_never_suspend_agents_per_project must be seeded by New()/seedLimitDefinitions")
	def.DefaultValue = ceiling
	_, err = s.UpdateLimitDefinition(ctx, def)
	require.NoError(t, err)
}

func TestNeverSuspend_PatchPolicy_AuthzAuditExperimentAndWarning(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("ns-p3-owner")
	viewerID := tid("ns-p3-viewer")
	createTestUserWithRole(t, s, ownerID, "ns-p3-owner@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	createTestUserWithRole(t, s, viewerID, "ns-p3-viewer@test.com", store.UserRoleViewer, store.SystemRoleHubMember)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	viewer, err := s.GetUser(ctx, viewerID)
	require.NoError(t, err)

	projectID := tid("ns-p3-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS P3 Project", Slug: "ns-p3-project",
		GitRemote: "https://github.com/example/ns-p3.git",
		OwnerID:   ownerID, CreatedBy: ownerID,
	}))
	createTestUserWithProjectRole(t, s, ownerID, owner.Email, projectID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, viewerID, viewer.Email, projectID, store.ProjectRoleViewer)

	brokerID := tid("ns-p3-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "NS P3 Broker", Slug: "ns-p3-broker", Status: store.BrokerStatusOnline,
		CreatedBy: ownerID,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS P3 Broker",
		Status: store.BrokerStatusOnline,
	}))
	srv.SetDispatcher(&createAgentDispatcher{})

	// Create an agent while experiment is off (defaults to auto).
	createRec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":          "ns-p3-agent",
		"projectId":     projectID,
		"provisionOnly": true,
	})
	require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	agentID := created.Agent.ID

	t.Run("experiment off returns 400 experiment_disabled", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, false)
		rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeExperimentDisabled, errResp.Error.Code)

		got, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, got.SuspendPolicy)
	})

	setNeverSuspendExperiment(t, srv, true)

	t.Run("viewer without agent.update gets 403", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, viewer, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("invalid value returns 400", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": "bogus",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("owner toggles auto->never->auto without bumping state_version and writes audit", func(t *testing.T) {
		before, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)

		rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp agentUpdateResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.SuspendPolicyNever, resp.SuspendPolicy)
		assert.False(t, resp.SuspensionInFlight)
		assert.Contains(t, resp.Disposition.Applied, "suspendPolicy")

		afterNever, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, afterNever.SuspendPolicy)
		assert.Equal(t, before.StateVersion, afterNever.StateVersion, "PATCH of only suspendPolicy must not bump state_version")

		// Clear back to auto.
		recAuto := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": store.SuspendPolicyAuto,
		})
		require.Equal(t, http.StatusOK, recAuto.Code, "body: %s", recAuto.Body.String())
		var respAuto agentUpdateResponse
		require.NoError(t, json.Unmarshal(recAuto.Body.Bytes(), &respAuto))
		assert.Equal(t, store.SuspendPolicyAuto, respAuto.SuspendPolicy)
		assert.Contains(t, respAuto.Disposition.Applied, "suspendPolicy")

		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			TargetType:   "agent",
			MutationType: mutationTypeAgentSuspendPolicySet,
			TargetID:     agentID,
		})
		require.NoError(t, err)
		require.Len(t, audits, 2)
		transitions := map[string]string{}
		for _, a := range audits {
			transitions[a.BeforeSummary+"->"+a.AfterSummary] = a.ActorPrincipalID
		}
		assert.Equal(t, ownerID, transitions[store.SuspendPolicyAuto+"->"+store.SuspendPolicyNever])
		assert.Equal(t, ownerID, transitions[store.SuspendPolicyNever+"->"+store.SuspendPolicyAuto])
	})

	t.Run("project admin with agent.update succeeds and records admin actor", func(t *testing.T) {
		adminID := tid("ns-p3-admin")
		createTestUserWithRole(t, s, adminID, "ns-p3-admin@test.com", store.UserRoleMember, store.SystemRoleHubMember)
		adminUser, err := s.GetUser(ctx, adminID)
		require.NoError(t, err)
		createTestUserWithProjectRole(t, s, adminID, adminUser.Email, projectID, store.ProjectRoleAdmin)

		patchRec := doRequestAsUser(t, srv, adminUser, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, patchRec.Code, "body: %s", patchRec.Body.String())

		got, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy)

		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			TargetType:   "agent",
			MutationType: mutationTypeAgentSuspendPolicySet,
			TargetID:     agentID,
		})
		require.NoError(t, err)
		var foundAdmin bool
		for _, a := range audits {
			if a.ActorPrincipalID == adminID && a.AfterSummary == store.SuspendPolicyNever {
				foundAdmin = true
				break
			}
		}
		assert.True(t, foundAdmin, "expected agent.suspend_policy.set audit attributed to admin actor")

		// Restore back to auto for cleanliness.
		clearRec := doRequestAsUser(t, srv, adminUser, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": store.SuspendPolicyAuto,
		})
		require.Equal(t, http.StatusOK, clearRec.Code)
	})

	t.Run("agent JWT is denied PATCH by existing agent.update credential scope (frozen AK1)", func(t *testing.T) {
		parent := &store.Agent{
			ID:        tid("ns-p3-parent-agent"),
			Slug:      "ns-p3-parent",
			Name:      "ns-p3-parent",
			ProjectID: projectID,
			Phase:     string(state.PhaseRunning),
			CreatedBy: ownerID,
			OwnerID:   ownerID,
			Ancestry:  []string{ownerID},
			AppliedConfig: &store.AgentAppliedConfig{
				AgentRole: string(AgentRoleFull),
			},
		}
		require.NoError(t, s.CreateAgent(ctx, parent))
		require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalUser,
			DelegatorID:   ownerID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    parent.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       projectID,
			Role:          string(AgentRoleFull),
			Active:        true,
		}))

		parentToken, err := srv.GenerateAgentToken(
			parent.ID, projectID, []string{ownerID}, AgentRoleFull, nil,
		)
		require.NoError(t, err)

		patchRec := doRequestWithToken(t, srv, parentToken, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		assert.Equal(t, http.StatusForbidden, patchRec.Code, "body: %s", patchRec.Body.String())
	})

	t.Run("non-resumable harness is allowed and returns a warning", func(t *testing.T) {
		nonRes := &store.Agent{
			ID:            tid("ns-p3-nonresumable"),
			Slug:          "ns-p3-nonresumable",
			Name:          "ns-p3-nonresumable",
			ProjectID:     projectID,
			OwnerID:       ownerID,
			CreatedBy:     ownerID,
			Phase:         string(state.PhaseRunning),
			SuspendPolicy: store.SuspendPolicyAuto,
			AppliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: "generic",
			},
		}
		require.NoError(t, s.CreateAgent(ctx, nonRes))

		rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+nonRes.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp agentUpdateResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.SuspendPolicyNever, resp.SuspendPolicy)
		require.NotEmpty(t, resp.Warnings, "non-resumable harness must return a warning")
		assert.Contains(t, resp.Warnings[0], "does not support resume")
	})
}

func TestNeverSuspend_PatchSuspensionInFlight(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setNeverSuspendExperiment(t, srv, true)

	now := time.Now().UTC()
	idleSince := now.Add(-30 * time.Minute)
	lastSeen := now.Add(-15 * time.Second)

	t.Run("idle claim already taken reports suspensionInFlight=true", func(t *testing.T) {
		agent := newIdleCandidateBrokerAgent(t, s, "ns-p3-inflight-idle", string(state.ActivityIdle), &idleSince, lastSeen)
		// Grant owner role so DevUser (default test user) can PATCH.
		agent.OwnerID = DevUserID
		require.NoError(t, s.UpdateAgent(ctx, agent))

		loaded, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		_, _, _, err = s.ClaimIdleSuspend(ctx, agent.ID, store.IdleSuspendCondition{
			RunID:                    loaded.RunID,
			Activity:                 loaded.Activity,
			ActivitySince:            *loaded.ActivitySince,
			IdleSuspendAfter:         15 * time.Minute,
			HeartbeatRecency:         2 * time.Minute,
			ClaimTTL:                 30 * time.Second,
			RequireSuspendPolicyAuto: true,
		})
		require.NoError(t, err)

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp agentUpdateResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.SuspendPolicyNever, resp.SuspendPolicy)
		assert.True(t, resp.SuspensionInFlight, "PATCH after idle claim must report suspensionInFlight=true")
	})

	t.Run("stalled auto-suspend intent swap reports suspensionInFlight=true", func(t *testing.T) {
		agent := newIdleCandidateBrokerAgent(t, s, "ns-p3-inflight-stalled", string(state.ActivityStalled), &idleSince, lastSeen)
		agent.OwnerID = DevUserID
		require.NoError(t, s.UpdateAgent(ctx, agent))

		_, _, err := s.SwapAutoSuspendRunIntent(ctx, agent.ID, store.RunIntentStopped, true)
		require.NoError(t, err)

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp agentUpdateResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.SuspendPolicyNever, resp.SuspendPolicy)
		assert.True(t, resp.SuspensionInFlight, "PATCH after auto-suspend intent swap must report suspensionInFlight=true")
	})
}

func TestNeverSuspend_QuotaLifecycleAndP1Backfill(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setNeverSuspendExperiment(t, srv, true)
	srv.mu.Lock()
	srv.config.SoftDeleteRetention = 24 * time.Hour
	srv.mu.Unlock()

	// Verify seeded unlimited limit definition.
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
	require.NoError(t, err)
	assert.Equal(t, int64(0), def.DefaultValue, "max_never_suspend_agents_per_project must be seeded unlimited (0)")

	ownerID := tid("ns-quota-owner")
	createTestUserWithRole(t, s, ownerID, "ns-quota-owner@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	projectID := tid("ns-quota-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS Quota Project", Slug: "ns-quota-project",
		GitRemote: "https://github.com/example/ns-quota.git",
		OwnerID:   ownerID, CreatedBy: ownerID,
	}))
	createTestUserWithProjectRole(t, s, ownerID, owner.Email, projectID, store.ProjectRoleOwner)

	brokerID := tid("ns-quota-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "NS Quota Broker", Slug: "ns-quota-broker", Status: store.BrokerStatusOnline,
		CreatedBy: ownerID,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS Quota Broker",
		Status: store.BrokerStatusOnline,
	}))
	srv.SetDispatcher(&createAgentDispatcher{})

	// Set cap to 2.
	setProjectNeverSuspendCeiling(t, s, 2)

	// 1. Seed a P1-style flagged row directly (no usage_reservation row) — Security L1.
	p1Agent := &store.Agent{
		ID:            tid("ns-quota-p1-row"),
		Slug:          "ns-quota-p1-row",
		Name:          "ns-quota-p1-row",
		ProjectID:     projectID,
		OwnerID:       ownerID,
		CreatedBy:     ownerID,
		Phase:         string(state.PhaseStopped), // non-running phase must still count
		SuspendPolicy: store.SuspendPolicyNever,
	}
	require.NoError(t, s.CreateAgent(ctx, p1Agent))

	// 2. Create agent 2 via API with suspendPolicy=never -> 201 (usage now 2/2: P1 row + agent 2).
	rec2 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":          "ns-quota-a2",
		"projectId":     projectID,
		"provisionOnly": true,
		"suspendPolicy": store.SuspendPolicyNever,
	})
	require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())
	var resp2 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	agent2ID := resp2.Agent.ID

	// Acceptance §9 item 10: flagged agent also holds broker + project reservations.
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agent2ID))
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, agent2ID))
	assert.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, agent2ID))

	// 3. Create agent 3 with suspendPolicy=never -> 429 quota_exceeded (P1 row + agent 2 = 2/2).
	rec3Fail := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":          "ns-quota-a3-fail",
		"projectId":     projectID,
		"provisionOnly": true,
		"suspendPolicy": store.SuspendPolicyNever,
	})
	require.Equal(t, http.StatusTooManyRequests, rec3Fail.Code, "body: %s", rec3Fail.Body.String())
	assert.Contains(t, rec3Fail.Body.String(), store.LimitMaxNeverSuspendAgentsPerProject)

	// 4. Create agent 3 as auto -> 201, then PATCH auto->never -> 429 quota_exceeded.
	rec3Auto := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":          "ns-quota-a3",
		"projectId":     projectID,
		"provisionOnly": true,
	})
	require.Equal(t, http.StatusCreated, rec3Auto.Code, "body: %s", rec3Auto.Body.String())
	var resp3 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec3Auto.Body.Bytes(), &resp3))
	agent3ID := resp3.Agent.ID

	patch3Fail := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent3ID, map[string]any{
		"suspendPolicy": store.SuspendPolicyNever,
	})
	require.Equal(t, http.StatusTooManyRequests, patch3Fail.Code, "body: %s", patch3Fail.Body.String())

	// 5. Clear flag on P1 row via PATCH never->auto -> frees slot (1/2) -> PATCH agent 3 succeeds!
	patchP1Clear := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+p1Agent.ID, map[string]any{
		"suspendPolicy": store.SuspendPolicyAuto,
	})
	require.Equal(t, http.StatusOK, patchP1Clear.Code, "body: %s", patchP1Clear.Body.String())

	patch3OK := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent3ID, map[string]any{
		"suspendPolicy": store.SuspendPolicyNever,
	})
	require.Equal(t, http.StatusOK, patch3OK.Code, "body: %s", patch3OK.Body.String())
	assert.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, agent3ID))

	// 6. Soft-delete agent 3 -> releases never-suspend slot (1/2).
	del3 := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/agents/"+agent3ID, nil)
	require.Equal(t, http.StatusNoContent, del3.Code, "body: %s", del3.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, agent3ID))

	// Re-flag p1Agent to fill the cap back to 2/2.
	patchP1Set := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+p1Agent.ID, map[string]any{
		"suspendPolicy": store.SuspendPolicyNever,
	})
	require.Equal(t, http.StatusOK, patchP1Set.Code, "body: %s", patchP1Set.Body.String())

	// 7. Restore soft-deleted flagged agent 3 while at cap (2/2) -> 409 Conflict!
	restoreFail := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+agent3ID+"/restore", nil)
	require.Equal(t, http.StatusConflict, restoreFail.Code, "body: %s", restoreFail.Body.String())
	assert.Contains(t, restoreFail.Body.String(), store.LimitMaxNeverSuspendAgentsPerProject)

	// 8. Clear flag on agent 2 (1/2) -> restore agent 3 now succeeds (200) and re-reserves!
	patch2Clear := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent2ID, map[string]any{
		"suspendPolicy": store.SuspendPolicyAuto,
	})
	require.Equal(t, http.StatusOK, patch2Clear.Code, "body: %s", patch2Clear.Body.String())
	assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, agent2ID))

	restoreOK := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+agent3ID+"/restore", nil)
	require.Equal(t, http.StatusOK, restoreOK.Code, "body: %s", restoreOK.Body.String())
	assert.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, agent3ID))
}

func TestNeverSuspend_ExistingAgentEnvGatherRecreate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setNeverSuspendExperiment(t, srv, true)

	ownerID := tid("ns-eg-owner")
	createTestUserWithRole(t, s, ownerID, "ns-eg-owner@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	projectID := tid("ns-eg-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS EG Project", Slug: "ns-eg-project",
		GitRemote: "https://github.com/example/ns-eg.git",
		OwnerID:   ownerID, CreatedBy: ownerID,
	}))
	createTestUserWithProjectRole(t, s, ownerID, owner.Email, projectID, store.ProjectRoleMember)

	brokerID := tid("ns-eg-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "NS EG Broker", Slug: "ns-eg-broker", Status: store.BrokerStatusOnline,
		CreatedBy: ownerID,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS EG Broker",
		Status: store.BrokerStatusOnline,
	}))
	srv.SetDispatcher(&createAgentDispatcher{})

	t.Run("non-env-gather mismatch message points at PATCH", func(t *testing.T) {
		rec1 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-msg",
			"projectId":     projectID,
			"provisionOnly": true,
		})
		require.Equal(t, http.StatusCreated, rec1.Code)

		rec2 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-msg",
			"projectId":     projectID,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusConflict, rec2.Code)
		assert.Contains(t, rec2.Body.String(), "PATCH")
	})

	t.Run("provisioning + GatherEnv honors auto->never and reserves quota", func(t *testing.T) {
		rec1 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-a2n",
			"projectId":     projectID,
			"provisionOnly": true,
		})
		require.Equal(t, http.StatusCreated, rec1.Code)
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		// Move to provisioning phase (the env-gather recreate precondition).
		require.NoError(t, s.UpdateAgentStatus(ctx, resp1.Agent.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseProvisioning),
		}))

		rec2 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-a2n",
			"projectId":     projectID,
			"gatherEnv":     true,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())
		var resp2 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.NotEqual(t, resp1.Agent.ID, resp2.Agent.ID, "env-gather recreate creates a fresh row")
		assert.Equal(t, store.SuspendPolicyNever, resp2.Agent.SuspendPolicy)
		assert.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp2.Agent.ID))
	})

	t.Run("provisioning + GatherEnv honors never->(omitted) and releases old quota", func(t *testing.T) {
		rec1 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-n2a",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec1.Code)
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		require.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp1.Agent.ID))
		require.NoError(t, s.UpdateAgentStatus(ctx, resp1.Agent.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseProvisioning),
		}))

		rec2 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-n2a",
			"projectId":     projectID,
			"gatherEnv":     true,
			"provisionOnly": true,
		})
		require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())
		var resp2 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		assert.Equal(t, store.SuspendPolicyAuto, resp2.Agent.SuspendPolicy)
		assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp1.Agent.ID))
		assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp2.Agent.ID))
	})
}

func TestNeverSuspend_FilterQueryAndMetric(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("ns-fm-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS FM Project", Slug: "ns-fm-project",
		GitRemote: "https://github.com/example/ns-fm.git",
		OwnerID:   DevUserID, CreatedBy: DevUserID,
	}))

	aRunningNever := &store.Agent{
		ID: tid("ns-fm-run-never"), Slug: "ns-fm-run-never", Name: "ns-fm-run-never",
		ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseRunning), SuspendPolicy: store.SuspendPolicyNever,
	}
	aStoppedNever := &store.Agent{
		ID: tid("ns-fm-stop-never"), Slug: "ns-fm-stop-never", Name: "ns-fm-stop-never",
		ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
	}
	aRunningAuto := &store.Agent{
		ID: tid("ns-fm-run-auto"), Slug: "ns-fm-run-auto", Name: "ns-fm-run-auto",
		ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseRunning), SuspendPolicy: store.SuspendPolicyAuto,
	}
	require.NoError(t, s.CreateAgent(ctx, aRunningNever))
	require.NoError(t, s.CreateAgent(ctx, aStoppedNever))
	require.NoError(t, s.CreateAgent(ctx, aRunningAuto))

	t.Run("filter query suspendPolicy:never and -suspendPolicy:never on global and project endpoints", func(t *testing.T) {
		setExperimentOverrides(t, srv, map[string]bool{
			experiments.FiltersQueryDialect: true,
			experiments.NeverSuspend:        true,
		})
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q=suspendPolicy:never", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var listResp struct {
			Agents []store.Agent `json:"agents"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
		gotIDs := make([]string, len(listResp.Agents))
		for i, a := range listResp.Agents {
			gotIDs[i] = a.ID
		}
		assert.ElementsMatch(t, []string{aRunningNever.ID, aStoppedNever.ID}, gotIDs)

		recProj := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/agents?q=-suspendPolicy:never", nil)
		require.Equal(t, http.StatusOK, recProj.Code, "body: %s", recProj.Body.String())
		var projList struct {
			Agents []store.Agent `json:"agents"`
		}
		require.NoError(t, json.Unmarshal(recProj.Body.Bytes(), &projList))
		require.Len(t, projList.Agents, 1)
		assert.Equal(t, aRunningAuto.ID, projList.Agents[0].ID)

		recInvalid := doRequest(t, srv, http.MethodGet, "/api/v1/agents?q=suspendPolicy:bogus", nil)
		assert.Equal(t, http.StatusBadRequest, recInvalid.Code)
	})

	t.Run("metric scion.hub.never_suspend_agents observes running flagged agents when experiment is on", func(t *testing.T) {
		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		require.NoError(t, srv.RegisterNeverSuspendMetrics(mp))

		collect := func() []metricdata.DataPoint[int64] {
			var rm metricdata.ResourceMetrics
			require.NoError(t, reader.Collect(ctx, &rm))
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name == "scion.hub.never_suspend_agents" {
						if g, ok := m.Data.(metricdata.Gauge[int64]); ok {
							return g.DataPoints
						}
					}
				}
			}
			return nil
		}

		// Experiment off -> no observations.
		setNeverSuspendExperiment(t, srv, false)
		assert.Empty(t, collect())

		// Experiment on -> observes only the 1 running flagged agent.
		setNeverSuspendExperiment(t, srv, true)
		_ = experiments.NeverSuspend
		pts := collect()
		require.Len(t, pts, 1)
		assert.Equal(t, int64(1), pts[0].Value)
		pVal, pOK := pts[0].Attributes.Value("project")
		bVal, bOK := pts[0].Attributes.Value("runtime_broker")
		assert.True(t, pOK)
		assert.True(t, bOK)
		assert.Equal(t, projectID, pVal.AsString())
		assert.Equal(t, "broker-1", bVal.AsString())
	})
}

type failReleaseNeverSuspendStore struct {
	store.Store
	store.AdvisoryLocker
	armed         atomic.Bool
	targetLimitID string
}

func (f *failReleaseNeverSuspendStore) ReleaseReservation(ctx context.Context, limitDefinitionID, resourceID string) error {
	if f.armed.Load() && limitDefinitionID == f.targetLimitID {
		return errors.New("injected release failure")
	}
	return f.Store.ReleaseReservation(ctx, limitDefinitionID, resourceID)
}

type softDeleteOnReserveStore struct {
	store.Store
	store.AdvisoryLocker
	armed   atomic.Bool
	agentID string
	onHook  func()
}

func (f *softDeleteOnReserveStore) CreateUsageReservation(ctx context.Context, r *store.UsageReservation) (*store.UsageReservation, error) {
	res, err := f.Store.CreateUsageReservation(ctx, r)
	if err == nil && f.armed.CompareAndSwap(true, false) && r.ResourceID == f.agentID && f.onHook != nil {
		f.onHook()
	}
	return res, err
}

func TestNeverSuspend_QuotaReconcile_SoftDeleteRace_AndUsageParity(t *testing.T) {
	ctx := context.Background()

	t.Run("hard-delete with failed release returns slot after one reconcile pass", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)
		def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
		require.NoError(t, err)
		setProjectNeverSuspendCeiling(t, s, 1)

		wrapped, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *failReleaseNeverSuspendStore {
			return &failReleaseNeverSuspendStore{Store: inner, AdvisoryLocker: inner.(store.AdvisoryLocker), targetLimitID: def.ID}
		})
		srv.quotaService.store = wrapped

		projectID := tid("ns-rec-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS Rec Project", Slug: "ns-rec-project",
			GitRemote: "https://github.com/example/ns-rec.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))
		brokerID := tid("ns-rec-broker")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "NS Rec Broker", Slug: "ns-rec-broker", Status: store.BrokerStatusOnline,
			CreatedBy: DevUserID,
		}))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS Rec Broker",
			Status: store.BrokerStatusOnline,
		}))
		srv.SetDispatcher(&createAgentDispatcher{})

		// Create flagged agent 1 (1/1).
		rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-rec-a1",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
		require.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp1.Agent.ID))

		// Hard-delete agent 1 while never-suspend release is injected to fail.
		wrapped.armed.Store(true)
		delRec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+resp1.Agent.ID, nil)
		require.Equal(t, http.StatusNoContent, delRec.Code, "body: %s", delRec.Body.String())
		wrapped.armed.Store(false)

		// Reservation was stranded by the failed release.
		require.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp1.Agent.ID))

		// While younger than reconcileMinReservationAge, a missing-row reservation
		// is treated as an in-flight create and survives reconcile.
		srv.ReconcileStaleNeverSuspendQuotaReservations(ctx)
		assert.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp1.Agent.ID),
			"young missing-row reservation must survive reconcile to protect in-flight creates")

		// Backdate the stranded reservation past reconcileMinReservationAge:
		// one reconcile pass releases it and frees the slot.
		db := s.(*entadapter.CompositeStore).DB()
		_, err = db.ExecContext(ctx,
			"UPDATE usage_reservations SET created_at = ? WHERE resource_id = ? AND limit_definition_id = ?",
			time.Now().Add(-2*reconcileMinReservationAge), resp1.Agent.ID, def.ID)
		require.NoError(t, err)

		srv.ReconcileStaleNeverSuspendQuotaReservations(ctx)
		assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, resp1.Agent.ID))

		rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-rec-a2",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())
	})

	t.Run("PATCH auto->never racing soft-delete is refused and leaves no orphan reservation", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)
		setProjectNeverSuspendCeiling(t, s, 1)

		projectID := tid("ns-race-sd-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS Race SD Project", Slug: "ns-race-sd-project",
			GitRemote: "https://github.com/example/ns-race-sd.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))
		a := &store.Agent{
			ID: tid("ns-race-sd-agent"), Slug: "ns-race-sd-agent", Name: "ns-race-sd-agent",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseCreated), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, a))

		hook, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *softDeleteOnReserveStore {
			return &softDeleteOnReserveStore{Store: inner, AdvisoryLocker: inner.(store.AdvisoryLocker), agentID: a.ID}
		})
		srv.quotaService.store = hook
		hook.onHook = func() {
			// Soft-delete the agent after PATCH reserved quota, before SetAgentSuspendPolicy runs.
			softDeleteAgentRow(t, s, a.ID)
		}
		hook.armed.Store(true)

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+a.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		assert.Equal(t, http.StatusNotFound, rec.Code, "PATCH against concurrently soft-deleted row must fail; body: %s", rec.Body.String())

		// Row must still be auto and reservation must have been rolled back.
		stored, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, stored.SuspendPolicy)
		assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, a.ID), "reservation must be rolled back on SetAgentSuspendPolicy failure")
	})

	t.Run("Phase-1-flagged row without reservation matches across admin usage views and 429 details", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)
		def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
		require.NoError(t, err)
		setProjectNeverSuspendCeiling(t, s, 1)

		projectID := tid("ns-parity-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS Parity Project", Slug: "ns-parity-project",
			GitRemote: "https://github.com/example/ns-parity.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))
		brokerID := tid("ns-parity-broker")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "NS Parity Broker", Slug: "ns-parity-broker", Status: store.BrokerStatusOnline,
			CreatedBy: DevUserID,
		}))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS Parity Broker",
			Status: store.BrokerStatusOnline,
		}))
		srv.SetDispatcher(&createAgentDispatcher{})

		// Seed a P1-style flagged row directly (no usage_reservation row).
		p1 := &store.Agent{
			ID: tid("ns-parity-p1"), Slug: "ns-parity-p1", Name: "ns-parity-p1",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseRunning), SuspendPolicy: store.SuspendPolicyNever,
		}
		require.NoError(t, s.CreateAgent(ctx, p1))

		// Admin usage summary must report activeCount == 1.
		sumRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
		require.Equal(t, http.StatusOK, sumRec.Code, "body: %s", sumRec.Body.String())
		var sumResp usageSummaryResponse
		require.NoError(t, json.Unmarshal(sumRec.Body.Bytes(), &sumResp))
		var foundSummary bool
		for _, item := range sumResp.Items {
			if item.LimitDefinition != nil && item.LimitDefinition.Name == store.LimitMaxNeverSuspendAgentsPerProject {
				foundSummary = true
				assert.Equal(t, 1, item.ActiveCount, "getUsageSummary must count P1-flagged row")
			}
		}
		require.True(t, foundSummary)

		// Admin usage detail for the limit must report totalActive == 1 and include p1.ID.
		detRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
		require.Equal(t, http.StatusOK, detRec.Code, "body: %s", detRec.Body.String())
		var detResp usageByLimitResponse
		require.NoError(t, json.Unmarshal(detRec.Body.Bytes(), &detResp))
		assert.Equal(t, 1, detResp.TotalActive, "getUsageByLimit must count P1-flagged row")
		require.Len(t, detResp.Reservations, 1)
		assert.Equal(t, p1.ID, detResp.Reservations[0].ResourceID)

		// Enforcement 429 details must report the exact same count (1 of 1).
		createRec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-parity-over",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusTooManyRequests, createRec.Code, "body: %s", createRec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &errResp))
		assert.Equal(t, float64(1), errResp.Error.Details["count"])
		assert.Equal(t, float64(1), errResp.Error.Details["limit"])
	})

	t.Run("soft-deleted row with young reservation counts until past min age then reconcile frees slot", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)
		def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
		require.NoError(t, err)
		setProjectNeverSuspendCeiling(t, s, 1)

		projectID := tid("ns-sd-young-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS SD Young Project", Slug: "ns-sd-young-project",
			GitRemote: "https://github.com/example/ns-sd-young.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))
		brokerID := tid("ns-sd-young-broker")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "NS SD Young Broker", Slug: "ns-sd-young-broker", Status: store.BrokerStatusOnline,
			CreatedBy: DevUserID,
		}))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS SD Young Broker",
			Status: store.BrokerStatusOnline,
		}))
		srv.SetDispatcher(&createAgentDispatcher{})

		// Seed a soft-deleted flagged agent with a brand-new (young) reservation
		// (same state as an in-flight restore or a soft-delete whose release failed).
		deletedAgent := &store.Agent{
			ID: tid("ns-sd-young-agent"), Slug: "ns-sd-young-agent", Name: "ns-sd-young-agent",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		require.NoError(t, s.CreateAgent(ctx, deletedAgent))
		_, err = s.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: def.ID,
			SubjectID:         DevUserID,
			ScopeType:         store.QuotaScopeProject,
			ScopeID:           projectID,
			ResourceID:        deletedAgent.ID,
			Reserved:          1,
		})
		require.NoError(t, err)
		softDeleteAgentRow(t, s, deletedAgent.ID)

		// While young, the reservation counts and survives reconcile to protect in-flight restores.
		usage, err := neverSuspendQuotaUsage(ctx, s, def.ID, projectID, time.Now())
		require.NoError(t, err)
		assert.Len(t, usage, 1, "young reservation on soft-deleted row must count")
		srv.ReconcileStaleNeverSuspendQuotaReservations(ctx)
		assert.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, deletedAgent.ID),
			"young reservation on soft-deleted row must survive reconcile")

		// Backdate past reconcileMinReservationAge: reconcile releases it and frees the slot.
		db := s.(*entadapter.CompositeStore).DB()
		_, err = db.ExecContext(ctx,
			"UPDATE usage_reservations SET created_at = ? WHERE resource_id = ? AND limit_definition_id = ?",
			time.Now().Add(-2*reconcileMinReservationAge), deletedAgent.ID, def.ID)
		require.NoError(t, err)

		srv.ReconcileStaleNeverSuspendQuotaReservations(ctx)
		assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, deletedAgent.ID))

		createRec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-sd-young-new",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
	})

	t.Run("R1: no-op never PATCH and labels+never PATCH on Phase-1 rows at cap succeed while N+1th auto fails 429", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)
		setProjectNeverSuspendCeiling(t, s, 2)

		projectID := tid("ns-r1-cap-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS R1 Cap Project", Slug: "ns-r1-cap-project",
			GitRemote: "https://github.com/example/ns-r1-cap.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))

		// Seed N=2 Phase-1-style flagged rows with no usage_reservations.
		p1a := &store.Agent{
			ID: tid("ns-r1-p1a"), Slug: "ns-r1-p1a", Name: "ns-r1-p1a",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		p1b := &store.Agent{
			ID: tid("ns-r1-p1b"), Slug: "ns-r1-p1b", Name: "ns-r1-p1b",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		autoAgent := &store.Agent{
			ID: tid("ns-r1-auto"), Slug: "ns-r1-auto", Name: "ns-r1-auto",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, p1a))
		require.NoError(t, s.CreateAgent(ctx, p1b))
		require.NoError(t, s.CreateAgent(ctx, autoAgent))
		require.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, p1a.ID))
		require.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, p1b.ID))

		// No-op PATCH {"suspendPolicy":"never"} at cap=2 -> 200.
		recNoop := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+p1a.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, recNoop.Code, "body: %s", recNoop.Body.String())

		// Labels + suspendPolicy="never" PATCH at cap=2 -> 200 and labels persisted.
		recCombined := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+p1b.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
			"labels":        map[string]string{"env": "prod"},
		})
		require.Equal(t, http.StatusOK, recCombined.Code, "body: %s", recCombined.Body.String())
		storedB, err := s.GetAgent(ctx, p1b.ID)
		require.NoError(t, err)
		assert.Equal(t, "prod", storedB.Labels["env"])
		assert.Equal(t, store.SuspendPolicyNever, storedB.SuspendPolicy)

		// Flagging an (N+1)th auto agent at cap=2 -> 429 quota_exceeded.
		recOver := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+autoAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusTooManyRequests, recOver.Code, "body: %s", recOver.Body.String())
	})

	t.Run("R2: in-flight restore reservation blocks concurrent create, PATCH auto->never, and second restore", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)
		srv.mu.Lock()
		srv.config.SoftDeleteRetention = 24 * time.Hour
		srv.mu.Unlock()
		setProjectNeverSuspendCeiling(t, s, 1)

		projectID := tid("ns-r2-inflight-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS R2 InFlight Project", Slug: "ns-r2-inflight-project",
			GitRemote: "https://github.com/example/ns-r2-inflight.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))
		brokerID := tid("ns-r2-inflight-broker")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "NS R2 InFlight Broker", Slug: "ns-r2-inflight-broker", Status: store.BrokerStatusOnline,
			CreatedBy: DevUserID,
		}))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS R2 InFlight Broker",
			Status: store.BrokerStatusOnline,
		}))
		srv.SetDispatcher(&createAgentDispatcher{})

		// Two soft-deleted flagged agents + one live auto agent.
		restoring := &store.Agent{
			ID: tid("ns-r2-restoring"), Slug: "ns-r2-restoring", Name: "ns-r2-restoring",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		secondDel := &store.Agent{
			ID: tid("ns-r2-second-del"), Slug: "ns-r2-second-del", Name: "ns-r2-second-del",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		autoAgent := &store.Agent{
			ID: tid("ns-r2-auto"), Slug: "ns-r2-auto", Name: "ns-r2-auto",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, restoring))
		require.NoError(t, s.CreateAgent(ctx, secondDel))
		require.NoError(t, s.CreateAgent(ctx, autoAgent))
		softDeleteAgentRow(t, s, restoring.ID)
		softDeleteAgentRow(t, s, secondDel.ID)

		// Simulate restoreAgent's Reserve step completing before restoreAgentTx commits.
		created, err := srv.quotaService.Reserve(ctx, store.LimitMaxNeverSuspendAgentsPerProject,
			DevUserID, store.QuotaScopeProject, projectID, restoring.ID)
		require.NoError(t, err)
		require.True(t, created)

		// 1. Concurrent create with suspendPolicy=never -> 429.
		recCreate := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-r2-concurrent-create",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		assert.Equal(t, http.StatusTooManyRequests, recCreate.Code, "body: %s", recCreate.Body.String())

		// 2. Concurrent PATCH auto->never -> 429.
		recPatch := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+autoAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		assert.Equal(t, http.StatusTooManyRequests, recPatch.Code, "body: %s", recPatch.Body.String())

		// 3. Concurrent second restore of another flagged agent -> 409 quota_exceeded.
		recRestore := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+secondDel.ID+"/restore", nil)
		assert.Equal(t, http.StatusConflict, recRestore.Code, "body: %s", recRestore.Body.String())
	})

	t.Run("restore over cap succeeds when lifecycle.never_suspend experiment is off", func(t *testing.T) {
		srv, s := testServer(t)
		srv.mu.Lock()
		srv.config.SoftDeleteRetention = 24 * time.Hour
		srv.mu.Unlock()
		setProjectNeverSuspendCeiling(t, s, 1)

		projectID := tid("ns-rest-off-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS Rest Off Project", Slug: "ns-rest-off-project",
			GitRemote: "https://github.com/example/ns-rest-off.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))

		// Fill the cap with a live flagged agent.
		live := &store.Agent{
			ID: tid("ns-rest-off-live"), Slug: "ns-rest-off-live", Name: "ns-rest-off-live",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		require.NoError(t, s.CreateAgent(ctx, live))

		// Soft-deleted flagged agent.
		deleted := &store.Agent{
			ID: tid("ns-rest-off-del"), Slug: "ns-rest-off-del", Name: "ns-rest-off-del",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		require.NoError(t, s.CreateAgent(ctx, deleted))
		softDeleteAgentRow(t, s, deleted.ID)

		// Experiment off: restore must not enforce never-suspend quota (design §4.2 point 7).
		setNeverSuspendExperiment(t, srv, false)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+deleted.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, deleted.ID))
	})

	t.Run("existing agent with active create launch or unpinned broker returns 409 without silent 200 or pin write", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)

		projectID := tid("ns-r1-project")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS R1 Project", Slug: "ns-r1-project",
			GitRemote: "https://github.com/example/ns-r1.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))
		brokerID := tid("ns-r1-broker")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "NS R1 Broker", Slug: "ns-r1-broker", Status: store.BrokerStatusOnline,
			CreatedBy: DevUserID,
		}))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS R1 Broker",
			Status: store.BrokerStatusOnline,
		}))
		srv.SetDispatcher(&createAgentDispatcher{})

		// Case 1: auto agent with an active create launch -> POST with suspendPolicy=never returns 409 (not 200).
		launching := &store.Agent{
			ID: tid("ns-r1-launching"), Slug: "ns-r1-launching", Name: "ns-r1-launching",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			RuntimeBrokerID: brokerID, Phase: string(state.PhaseCreated), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, launching))
		_, err := s.BeginLaunch(ctx, launching.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)

		recLaunch := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-r1-launching",
			"projectId":     projectID,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusConflict, recLaunch.Code, "body: %s", recLaunch.Body.String())
		var errLaunch ErrorResponse
		require.NoError(t, json.Unmarshal(recLaunch.Body.Bytes(), &errLaunch))
		assert.Equal(t, ErrCodeSuspendPolicyCreateOnly, errLaunch.Error.Code)

		// Case 2: unpinned auto agent without a broker -> 409 and no broker pin is persisted.
		unpinned := &store.Agent{
			ID: tid("ns-r1-unpinned"), Slug: "ns-r1-unpinned", Name: "ns-r1-unpinned",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseCreated), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, unpinned))

		recUnpinned := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-r1-unpinned",
			"projectId":     projectID,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusConflict, recUnpinned.Code, "body: %s", recUnpinned.Body.String())
		storedUnpinned, err := s.GetAgent(ctx, unpinned.ID)
		require.NoError(t, err)
		assert.Empty(t, storedUnpinned.RuntimeBrokerID, "refused mismatch must not assign or pin a broker")
		assert.Empty(t, storedUnpinned.PinnedRuntimeBrokerID, "refused mismatch must not persist a pin")
	})
}

// TestNeverSuspend_QuotaStateModelTable verifies every cell of the 4x3 state
// model documented on neverSuspendQuotaUsage:
//
//	agent row ∈ {missing, soft-deleted, live auto, live never}
//	  × reservation ∈ {none, young, old}
//
// asserting both neverSuspendQuotaUsage count and
// ReconcileStaleNeverSuspendQuotaReservations outcome, plus Reserve idempotency
// for a live "never" row that already owns its slot without a reservation.
func TestNeverSuspend_QuotaStateModelTable(t *testing.T) {
	ctx := context.Background()

	type rowKind string
	const (
		rowMissing     rowKind = "missing"
		rowSoftDeleted rowKind = "soft-deleted"
		rowLiveAuto    rowKind = "live-auto"
		rowLiveNever   rowKind = "live-never"
	)
	type resKind string
	const (
		resNone  resKind = "none"
		resYoung resKind = "young"
		resOld   resKind = "old"
	)

	cases := []struct {
		row          rowKind
		res          resKind
		wantCount    int
		wantKept     bool // meaningful when res != resNone
		wantReleased bool // meaningful when res != resNone
	}{
		{rowMissing, resNone, 0, false, false},
		{rowMissing, resYoung, 1, true, false},
		{rowMissing, resOld, 0, false, true},

		{rowSoftDeleted, resNone, 0, false, false},
		{rowSoftDeleted, resYoung, 1, true, false},
		{rowSoftDeleted, resOld, 0, false, true},

		{rowLiveAuto, resNone, 0, false, false},
		{rowLiveAuto, resYoung, 1, true, false},
		{rowLiveAuto, resOld, 0, false, true},

		{rowLiveNever, resNone, 1, false, false},
		{rowLiveNever, resYoung, 1, true, false},
		{rowLiveNever, resOld, 1, true, false},
	}

	for _, tc := range cases {
		name := string(tc.row) + "/" + string(tc.res)
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			setNeverSuspendExperiment(t, srv, true)
			def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
			require.NoError(t, err)

			projectID := tid("ns-tbl-proj-" + name)
			require.NoError(t, s.CreateProject(ctx, &store.Project{
				ID: projectID, Name: "NS Tbl " + name, Slug: "ns-tbl-" + string(tc.row) + "-" + string(tc.res),
				GitRemote: "https://github.com/example/ns-tbl.git",
				OwnerID:   DevUserID, CreatedBy: DevUserID,
			}))

			agentID := tid("ns-tbl-agent-" + name)
			switch tc.row {
			case rowMissing:
				// No agent row created.
			case rowSoftDeleted:
				require.NoError(t, s.CreateAgent(ctx, &store.Agent{
					ID: agentID, Slug: "a-" + string(tc.res), Name: "a-" + string(tc.res),
					ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
					Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
				}))
				softDeleteAgentRow(t, s, agentID)
			case rowLiveAuto:
				require.NoError(t, s.CreateAgent(ctx, &store.Agent{
					ID: agentID, Slug: "a-" + string(tc.res), Name: "a-" + string(tc.res),
					ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
					Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyAuto,
				}))
			case rowLiveNever:
				require.NoError(t, s.CreateAgent(ctx, &store.Agent{
					ID: agentID, Slug: "a-" + string(tc.res), Name: "a-" + string(tc.res),
					ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
					Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
				}))
			}

			if tc.res != resNone {
				_, err = s.CreateUsageReservation(ctx, &store.UsageReservation{
					LimitDefinitionID: def.ID,
					SubjectID:         DevUserID,
					ScopeType:         store.QuotaScopeProject,
					ScopeID:           projectID,
					ResourceID:        agentID,
					Reserved:          1,
				})
				require.NoError(t, err)
				if tc.res == resOld {
					db := s.(*entadapter.CompositeStore).DB()
					_, err = db.ExecContext(ctx,
						"UPDATE usage_reservations SET created_at = ? WHERE resource_id = ? AND limit_definition_id = ?",
						time.Now().Add(-2*reconcileMinReservationAge), agentID, def.ID)
					require.NoError(t, err)
				}
			}

			// 1. Check neverSuspendQuotaUsage count.
			usage, err := neverSuspendQuotaUsage(ctx, s, def.ID, projectID, time.Now())
			require.NoError(t, err)
			assert.Len(t, usage, tc.wantCount, "neverSuspendQuotaUsage count for %s", name)

			// 2. Check ReconcileStaleNeverSuspendQuotaReservations outcome.
			srv.ReconcileStaleNeverSuspendQuotaReservations(ctx)
			if tc.res != resNone {
				gotHas := hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, agentID)
				if tc.wantKept {
					assert.True(t, gotHas, "reconcile must keep reservation for %s", name)
				}
				if tc.wantReleased {
					assert.False(t, gotHas, "reconcile must release reservation for %s", name)
				}
			}
		})
	}

	t.Run("Reserve for already-counted live never row without reservation succeeds at cap", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)
		setProjectNeverSuspendCeiling(t, s, 1)

		projectID := tid("ns-tbl-res-proj")
		require.NoError(t, s.CreateProject(ctx, &store.Project{
			ID: projectID, Name: "NS Tbl Res", Slug: "ns-tbl-res",
			GitRemote: "https://github.com/example/ns-tbl-res.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))

		liveNever := &store.Agent{
			ID: tid("ns-tbl-res-a1"), Slug: "ns-tbl-res-a1", Name: "ns-tbl-res-a1",
			ProjectID: projectID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseStopped), SuspendPolicy: store.SuspendPolicyNever,
		}
		require.NoError(t, s.CreateAgent(ctx, liveNever))
		require.False(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, liveNever.ID))

		// Re-reserving the already-counted live never row succeeds without creating a rollback-owned slot.
		created, err := srv.quotaService.Reserve(ctx, store.LimitMaxNeverSuspendAgentsPerProject,
			DevUserID, store.QuotaScopeProject, projectID, liveNever.ID)
		require.NoError(t, err)
		assert.False(t, created)
		assert.True(t, hasReservation(t, s, store.LimitMaxNeverSuspendAgentsPerProject, liveNever.ID),
			"Reserve should backfill the reservation unchecked for future fast-path hits")

		// A distinct resource at cap=1 is still rejected.
		createdOther, errOther := srv.quotaService.Reserve(ctx, store.LimitMaxNeverSuspendAgentsPerProject,
			DevUserID, store.QuotaScopeProject, projectID, tid("ns-tbl-res-other"))
		assert.False(t, createdOther)
		require.ErrorIs(t, errOther, store.ErrQuotaExceeded)
	})
}
