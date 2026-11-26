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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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

	t.Run("same-value PATCH writes no audit record and omits suspendPolicy from disposition.applied", func(t *testing.T) {
		sameAgent := &store.Agent{
			ID: tid("ns-p3-same-val"), Slug: "ns-p3-same-val", Name: "ns-p3-same-val",
			ProjectID: projectID, OwnerID: ownerID, CreatedBy: ownerID,
			Phase: string(state.PhaseRunning), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, sameAgent))

		// Same-value auto -> auto: 200, no audit record, not in disposition.applied.
		recAuto := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+sameAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyAuto,
		})
		require.Equal(t, http.StatusOK, recAuto.Code, "body: %s", recAuto.Body.String())
		var respAuto agentUpdateResponse
		require.NoError(t, json.Unmarshal(recAuto.Body.Bytes(), &respAuto))
		assert.Equal(t, store.SuspendPolicyAuto, respAuto.SuspendPolicy)
		assert.NotContains(t, respAuto.Disposition.Applied, "suspendPolicy")

		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			MutationType: mutationTypeAgentSuspendPolicySet,
			TargetID:     sameAgent.ID,
		})
		require.NoError(t, err)
		assert.Empty(t, audits, "same-value auto->auto PATCH must write no audit record")

		// Transition auto -> never: 1 audit record, suspendPolicy in disposition.applied.
		recNever := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+sameAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, recNever.Code, "body: %s", recNever.Body.String())
		var respNever agentUpdateResponse
		require.NoError(t, json.Unmarshal(recNever.Body.Bytes(), &respNever))
		assert.Contains(t, respNever.Disposition.Applied, "suspendPolicy")

		// Same-value never -> never: still only 1 audit record, not in disposition.applied.
		recNeverAgain := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+sameAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, recNeverAgain.Code, "body: %s", recNeverAgain.Body.String())
		var respNeverAgain agentUpdateResponse
		require.NoError(t, json.Unmarshal(recNeverAgain.Body.Bytes(), &respNeverAgain))
		assert.Equal(t, store.SuspendPolicyNever, respNeverAgain.SuspendPolicy)
		assert.NotContains(t, respNeverAgain.Disposition.Applied, "suspendPolicy")

		audits, _, err = s.ListMutationAudits(ctx, store.MutationAuditFilter{
			MutationType: mutationTypeAgentSuspendPolicySet,
			TargetID:     sameAgent.ID,
		})
		require.NoError(t, err)
		assert.Len(t, audits, 1, "same-value never->never PATCH must not write a second audit record")
	})

	t.Run("interleaved opposite PATCHes audit both transitions with exact before/after", func(t *testing.T) {
		srv2, s2 := testServer(t)
		setNeverSuspendExperiment(t, srv2, true)
		p2ID := tid("ns-p3-race-proj")
		require.NoError(t, s2.CreateProject(ctx, &store.Project{
			ID: p2ID, Name: "NS P3 Race", Slug: "ns-p3-race",
			GitRemote: "https://github.com/example/ns-p3-race.git",
			OwnerID:   DevUserID, CreatedBy: DevUserID,
		}))
		raceAgent := &store.Agent{
			ID: tid("ns-p3-race-agent"), Slug: "ns-p3-race-agent", Name: "ns-p3-race-agent",
			ProjectID: p2ID, OwnerID: DevUserID, CreatedBy: DevUserID,
			Phase: string(state.PhaseRunning), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s2.CreateAgent(ctx, raceAgent))

		// Hook WithTx so the first PATCH (targeting "auto" on an initially "auto"
		// row) pauses after reading the stale handler-entry snapshot ("auto")
		// while a second PATCH (targeting "never") runs and commits "auto->never".
		// When the first PATCH resumes, conditional SetAgentSuspendPolicy must
		// observe the committed "never" row, transition "never->auto", write its
		// audit record, and report suspendPolicy in disposition.applied.
		enteredB := make(chan struct{})
		releaseB := make(chan struct{})
		hook, _ := installStoreFault(t, srv2, func(inner store.Store, _ *storeFaultSwitch) *interleaveWithTxStore {
			return &interleaveWithTxStore{
				Store:   inner,
				entered: enteredB,
				release: releaseB,
			}
		})
		hook.armed.Store(true)

		var wg sync.WaitGroup
		var respB agentUpdateResponse
		var codeB int
		wg.Add(1)
		go func() {
			defer wg.Done()
			recB := doRequest(t, srv2, http.MethodPatch, "/api/v1/agents/"+raceAgent.ID, map[string]any{
				"suspendPolicy": store.SuspendPolicyAuto,
			})
			codeB = recB.Code
			_ = json.Unmarshal(recB.Body.Bytes(), &respB)
		}()

		// Wait until PATCH B has read its stale entry snapshot ("auto") and
		// reached WithTx, then execute PATCH A ("auto->never") to completion.
		<-enteredB
		recA := doRequest(t, srv2, http.MethodPatch, "/api/v1/agents/"+raceAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, recA.Code, "body: %s", recA.Body.String())

		// Let PATCH B run its transaction now that the row is "never".
		close(releaseB)
		wg.Wait()
		require.Equal(t, http.StatusOK, codeB)
		assert.Equal(t, store.SuspendPolicyAuto, respB.SuspendPolicy)
		assert.Contains(t, respB.Disposition.Applied, "suspendPolicy",
			"disposition.applied must reflect the atomic never->auto transition")

		stored, err := s2.GetAgent(ctx, raceAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, stored.SuspendPolicy)

		audits, _, err := s2.ListMutationAudits(ctx, store.MutationAuditFilter{
			MutationType: mutationTypeAgentSuspendPolicySet,
			TargetID:     raceAgent.ID,
		})
		require.NoError(t, err)
		require.Len(t, audits, 2, "both auto->never and never->auto transitions must be audited")
		transitions := map[string]int{}
		for _, a := range audits {
			transitions[a.BeforeSummary+"->"+a.AfterSummary]++
		}
		assert.Equal(t, 1, transitions[store.SuspendPolicyAuto+"->"+store.SuspendPolicyNever])
		assert.Equal(t, 1, transitions[store.SuspendPolicyNever+"->"+store.SuspendPolicyAuto])
	})
}

type interleaveWithTxStore struct {
	store.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (f *interleaveWithTxStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	if f.armed.CompareAndSwap(true, false) {
		close(f.entered)
		<-f.release
	}
	return f.Store.WithTx(ctx, fn)
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

	t.Run("provisioning + GatherEnv honors auto->never", func(t *testing.T) {
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
	})

	t.Run("provisioning + GatherEnv honors never->(omitted)", func(t *testing.T) {
		rec1 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-n2a",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec1.Code)
		var resp1 CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
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
	})

	t.Run("existing agent with active create launch or unpinned broker returns 409 without silent 200 or pin write", func(t *testing.T) {
		// Case 1: auto agent with an active create launch -> POST with suspendPolicy=never returns 409 (not 200).
		launching := &store.Agent{
			ID: tid("ns-eg-launching"), Slug: "ns-eg-launching", Name: "ns-eg-launching",
			ProjectID: projectID, OwnerID: ownerID, CreatedBy: ownerID,
			RuntimeBrokerID: brokerID, Phase: string(state.PhaseCreated), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, launching))
		_, err := s.BeginLaunch(ctx, launching.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)

		recLaunch := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-launching",
			"projectId":     projectID,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusConflict, recLaunch.Code, "body: %s", recLaunch.Body.String())
		var errLaunch ErrorResponse
		require.NoError(t, json.Unmarshal(recLaunch.Body.Bytes(), &errLaunch))
		assert.Equal(t, ErrCodeSuspendPolicyCreateOnly, errLaunch.Error.Code)

		// Case 2: unpinned auto agent without a broker -> 409 and no broker pin is persisted.
		unpinned := &store.Agent{
			ID: tid("ns-eg-unpinned"), Slug: "ns-eg-unpinned", Name: "ns-eg-unpinned",
			ProjectID: projectID, OwnerID: ownerID, CreatedBy: ownerID,
			Phase: string(state.PhaseCreated), SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, s.CreateAgent(ctx, unpinned))

		recUnpinned := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-eg-unpinned",
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
		setNeverSuspendExperiment(t, srv, true)
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
