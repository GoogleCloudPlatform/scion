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
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setNeverSuspendExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.NeverSuspend: on,
	})
}

func setIdleAndNeverSuspendExperiments(t *testing.T, srv *Server, idleOn, neverOn bool) {
	t.Helper()
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.IdleSuspend:  idleOn,
		experiments.NeverSuspend: neverOn,
	})
}

// setAgentSuspendPolicySQL directly updates suspend_policy in the test SQLite
// DB to simulate a concurrent policy change in P1 (before PATCH exists in P3).
func setAgentSuspendPolicySQL(t *testing.T, s store.Store, agentID, policy string) {
	t.Helper()
	db := s.(*entadapter.CompositeStore).DB()
	_, err := db.ExecContext(context.Background(),
		"UPDATE agents SET suspend_policy = ? WHERE id = ?", policy, agentID)
	require.NoError(t, err)
}

func TestNeverSuspend_CreateGatingAuthzAndAudit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Seed a human project member (can create agents) and a project with a broker.
	userID := tid("ns-member")
	createTestUserWithRole(t, s, userID, "ns-member@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	projectID := tid("ns-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS Project", Slug: "ns-project",
		GitRemote: "https://github.com/example/ns.git",
		OwnerID:   userID, CreatedBy: userID,
	}))
	createTestUserWithProjectRole(t, s, userID, user.Email, projectID, store.ProjectRoleMember)

	brokerID := tid("ns-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "NS Broker", Slug: "ns-broker", Status: store.BrokerStatusOnline,
		CreatedBy: userID,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS Broker",
		Status: store.BrokerStatusOnline,
	}))
	srv.SetDispatcher(&createAgentDispatcher{})

	t.Run("experiment off refuses suspendPolicy with 400 experiment_disabled", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, false)
		for _, val := range []string{store.SuspendPolicyNever, store.SuspendPolicyAuto} {
			rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
				"name":          "ns-off-" + val,
				"projectId":     projectID,
				"provisionOnly": true,
				"suspendPolicy": val,
			})
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeExperimentDisabled, errResp.Error.Code)
		}
	})

	t.Run("experiment on rejects invalid suspendPolicy with 400", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-invalid",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": "always",
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("experiment on allows human member to create never-suspend agent and emits audit", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-lead",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		require.NotNil(t, created.Agent)
		assert.Equal(t, store.SuspendPolicyNever, created.Agent.SuspendPolicy)

		// Verify GET returns suspendPolicy=never.
		getRec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents/"+created.Agent.ID, nil)
		require.Equal(t, http.StatusOK, getRec.Code)
		var fetched store.Agent
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &fetched))
		assert.Equal(t, store.SuspendPolicyNever, fetched.SuspendPolicy)

		// Verify agent.suspend_policy.set audit record was written atomically.
		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			TargetType:   "agent",
			TargetID:     created.Agent.ID,
			MutationType: mutationTypeAgentSuspendPolicySet,
		})
		require.NoError(t, err)
		require.Len(t, audits, 1)
		assert.Equal(t, store.SuspendPolicyAuto, audits[0].BeforeSummary)
		assert.Equal(t, store.SuspendPolicyNever, audits[0].AfterSummary)
		assert.Equal(t, userID, audits[0].ActorPrincipalID)
	})

	t.Run("omitted suspendPolicy defaults to auto without emitting suspend_policy audit", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-default",
			"projectId":     projectID,
			"provisionOnly": true,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		require.NotNil(t, created.Agent)
		assert.Equal(t, store.SuspendPolicyAuto, created.Agent.SuspendPolicy)

		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			TargetType:   "agent",
			TargetID:     created.Agent.ID,
			MutationType: mutationTypeAgentSuspendPolicySet,
		})
		require.NoError(t, err)
		assert.Empty(t, audits)
	})

	t.Run("agent principal with agent.create can create never-suspend child (D3)", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		// Create a project owner + parent agent with full role so it has agent.create.
		ownerID := tid("ns-owner")
		createTestUserWithRole(t, s, ownerID, "ns-owner@test.com", store.UserRoleMember, store.SystemRoleHubMember)
		createTestUserWithProjectRole(t, s, ownerID, "ns-owner@test.com", projectID, store.ProjectRoleOwner)

		parent := &store.Agent{
			ID:        tid("ns-parent-agent"),
			Slug:      "ns-parent",
			Name:      "ns-parent",
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

		rec := doRequestWithToken(t, srv, parentToken, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-child-listener",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		require.NotNil(t, created.Agent)
		assert.Equal(t, store.SuspendPolicyNever, created.Agent.SuspendPolicy)

		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			TargetType:   "agent",
			TargetID:     created.Agent.ID,
			MutationType: mutationTypeAgentSuspendPolicySet,
		})
		require.NoError(t, err)
		require.Len(t, audits, 1)
		assert.Equal(t, parent.ID, audits[0].ActorPrincipalID)
	})
}

func TestNeverSuspend_IdleSweep(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	idleSince := now.Add(-30 * time.Minute)
	lastSeen := now.Add(-15 * time.Second)

	t.Run("experiment on skips never-suspend agent and suspends auto agent", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)

		setIdleAndNeverSuspendExperiments(t, srv, true, true)
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = true
		srv.config.IdleSuspendAfter = 15 * time.Minute
		srv.mu.Unlock()

		neverAgent := newIdleCandidateBrokerAgent(t, s, "ns-idle-never", string(state.ActivityIdle), &idleSince, lastSeen)
		setAgentSuspendPolicySQL(t, s, neverAgent.ID, store.SuspendPolicyNever)
		autoAgent := newIdleCandidateBrokerAgent(t, s, "ns-idle-auto", string(state.ActivityIdle), &idleSince, lastSeen)

		srv.agentIdleSuspendHandler()(ctx)
		require.Eventually(t, func() bool {
			a, err := s.GetAgent(ctx, autoAgent.ID)
			return err == nil && a.Phase == string(state.PhaseSuspended)
		}, 5*time.Second, 20*time.Millisecond)

		gotAuto, err := s.GetAgent(ctx, autoAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), gotAuto.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), gotAuto.SuspendReason)

		gotNever, err := s.GetAgent(ctx, neverAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), gotNever.Phase)
		assert.Equal(t, store.SuspendPolicyNever, gotNever.SuspendPolicy)
		assert.True(t, store.BrokerQuotaCountedPhase(gotNever.Phase), "running never-suspend agent continues counting toward broker quota")
		assert.Equal(t, int32(1), disp.stops.Load(), "only the auto agent must be stopped")
	})

	t.Run("experiment off ignores stored never-suspend policy and suspends agent", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)

		setIdleAndNeverSuspendExperiments(t, srv, true, false)
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = true
		srv.config.IdleSuspendAfter = 15 * time.Minute
		srv.mu.Unlock()

		neverAgent := newIdleCandidateBrokerAgent(t, s, "ns-idle-off", string(state.ActivityIdle), &idleSince, lastSeen)
		setAgentSuspendPolicySQL(t, s, neverAgent.ID, store.SuspendPolicyNever)

		srv.agentIdleSuspendHandler()(ctx)
		require.Eventually(t, func() bool {
			a, err := s.GetAgent(ctx, neverAgent.ID)
			return err == nil && a.Phase == string(state.PhaseSuspended)
		}, 5*time.Second, 20*time.Millisecond)

		got, err := s.GetAgent(ctx, neverAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), got.SuspendReason)
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy, "stored policy is preserved when experiment is off")
	})
}

func TestNeverSuspend_StalledSweep(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	staleAct := now.Add(-30 * time.Minute)
	lastSeen := now.Add(-15 * time.Second)

	makeStalledCandidate := func(t *testing.T, s store.Store, name, policy string) *store.Agent {
		t.Helper()
		a := newIdleCandidateBrokerAgent(t, s, name, string(state.ActivityExecuting), nil, lastSeen)
		db := s.(*entadapter.CompositeStore).DB()
		_, err := db.ExecContext(ctx,
			"UPDATE agents SET last_activity_event = ?, suspend_policy = ? WHERE id = ?",
			staleAct.UTC(), policy, a.ID)
		require.NoError(t, err)
		loaded, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return loaded
	}

	t.Run("experiment on marks never-suspend agent stalled and notifies, but does not suspend", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)

		setNeverSuspendExperiment(t, srv, true)
		srv.mu.Lock()
		srv.config.StalledThreshold = 5 * time.Minute
		srv.config.AutoSuspendStalled = true
		srv.mu.Unlock()

		neverAgent := makeStalledCandidate(t, s, "ns-stalled-never", store.SuspendPolicyNever)
		autoAgent := makeStalledCandidate(t, s, "ns-stalled-auto", store.SuspendPolicyAuto)

		pub := &trackingEventPublisher{}
		srv.events = pub

		srv.agentStalledDetectionHandler()(ctx)
		require.Eventually(t, func() bool {
			a, err := s.GetAgent(ctx, autoAgent.ID)
			return err == nil && a.Phase == string(state.PhaseSuspended)
		}, 5*time.Second, 20*time.Millisecond)

		gotAuto, err := s.GetAgent(ctx, autoAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), gotAuto.Phase)
		assert.Equal(t, string(state.SuspendReasonStalled), gotAuto.SuspendReason)

		gotNever, err := s.GetAgent(ctx, neverAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), gotNever.Phase, "never-suspend agent must stay running")
		assert.Equal(t, string(state.ActivityStalled), gotNever.Activity, "never-suspend agent must still be marked stalled")
		assert.Equal(t, int32(1), disp.stops.Load(), "only auto agent must be stopped")

		var gotActivity string
		for _, a := range pub.publishedAgents() {
			if a.ID == neverAgent.ID && a.Activity == string(state.ActivityStalled) {
				gotActivity = a.Activity
				break
			}
		}
		assert.Equal(t, string(state.ActivityStalled), gotActivity, "stall status event must still fire for never-suspend agent")
	})

	t.Run("experiment off suspends stalled never-suspend agent", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &runIntentDispatcher{}
		srv.SetDispatcher(disp)

		setNeverSuspendExperiment(t, srv, false)
		srv.mu.Lock()
		srv.config.StalledThreshold = 5 * time.Minute
		srv.config.AutoSuspendStalled = true
		srv.mu.Unlock()

		neverAgent := makeStalledCandidate(t, s, "ns-stalled-off", store.SuspendPolicyNever)

		srv.agentStalledDetectionHandler()(ctx)
		require.Eventually(t, func() bool {
			a, err := s.GetAgent(ctx, neverAgent.ID)
			return err == nil && a.Phase == string(state.PhaseSuspended)
		}, 5*time.Second, 20*time.Millisecond)

		got, err := s.GetAgent(ctx, neverAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonStalled), got.SuspendReason)
	})
}

// neverSuspendPostGetHookStore runs afterGet once immediately after GetAgent
// returns for targetID, deterministically placing a policy flip in the window
// between autoSuspendAgent's fresh read and ClaimIdleSuspend /
// SwapAutoSuspendRunIntent.
type neverSuspendPostGetHookStore struct {
	store.Store
	fault    *storeFaultSwitch
	targetID string
	afterGet func()
	fired    atomic.Bool
}

func (w *neverSuspendPostGetHookStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := w.Store.GetAgent(ctx, id)
	if err == nil && w.fault.Active() && id == w.targetID && w.afterGet != nil && w.fired.CompareAndSwap(false, true) {
		w.afterGet()
	}
	return a, err
}

// neverSuspendStopHookDispatcher runs onStop once during DispatchAgentStop
// (after ClaimIdleSuspend / SwapAutoSuspendRunIntent has already committed).
type neverSuspendStopHookDispatcher struct {
	runIntentDispatcher
	onStop  func()
	stopped atomic.Bool
}

func (d *neverSuspendStopHookDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if d.onStop != nil && d.stopped.CompareAndSwap(false, true) {
		d.onStop()
	}
	return d.runIntentDispatcher.DispatchAgentStop(ctx, agent)
}

func TestNeverSuspend_RaceRule(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	idleSince := now.Add(-30 * time.Minute)
	lastSeen := now.Add(-15 * time.Second)

	t.Run("idle: flag committed before ClaimIdleSuspend aborts before stop", func(t *testing.T) {
		srv, s := testServer(t)
		hook, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *neverSuspendPostGetHookStore {
			return &neverSuspendPostGetHookStore{Store: inner, fault: f}
		})
		setIdleAndNeverSuspendExperiments(t, srv, true, true)
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = true
		srv.config.IdleSuspendAfter = 15 * time.Minute
		srv.mu.Unlock()

		agent := newIdleCandidateBrokerAgent(t, s, "ns-race-idle-before", string(state.ActivityIdle), &idleSince, lastSeen)
		hook.targetID = agent.ID
		hook.afterGet = func() {
			setAgentSuspendPolicySQL(t, s, agent.ID, store.SuspendPolicyNever)
		}
		fault.Arm()
		disp := &runIntentDispatcher{}

		suspended := srv.autoSuspendAgent(ctx, disp, agent, state.SuspendReasonIdle)
		assert.False(t, suspended)
		assert.Zero(t, disp.stops.Load(), "ClaimIdleSuspend must abort before DispatchAgentStop")

		after, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), after.Phase)
		assert.Equal(t, store.RunIntentRunning, after.RunIntent)
	})

	t.Run("idle: flag committed after ClaimIdleSuspend does not cancel in-flight suspend", func(t *testing.T) {
		srv, s := testServer(t)
		setIdleAndNeverSuspendExperiments(t, srv, true, true)
		srv.mu.Lock()
		srv.config.AutoSuspendIdle = true
		srv.config.IdleSuspendAfter = 15 * time.Minute
		srv.mu.Unlock()

		agent := newIdleCandidateBrokerAgent(t, s, "ns-race-idle-after", string(state.ActivityIdle), &idleSince, lastSeen)
		disp := &neverSuspendStopHookDispatcher{
			onStop: func() {
				// Flip suspend_policy to never after ClaimIdleSuspend already committed.
				setAgentSuspendPolicySQL(t, s, agent.ID, store.SuspendPolicyNever)
			},
		}

		suspended := srv.autoSuspendAgent(ctx, disp, agent, state.SuspendReasonIdle)
		assert.True(t, suspended)
		assert.Equal(t, int32(1), disp.stops.Load())

		after, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), after.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), after.SuspendReason)
		assert.Equal(t, store.SuspendPolicyNever, after.SuspendPolicy)
	})

	t.Run("stalled: flag committed before SwapAutoSuspendRunIntent aborts before stop", func(t *testing.T) {
		srv, s := testServer(t)
		hook, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *neverSuspendPostGetHookStore {
			return &neverSuspendPostGetHookStore{Store: inner, fault: f}
		})
		setNeverSuspendExperiment(t, srv, true)

		agent := newIdleCandidateBrokerAgent(t, s, "ns-race-stalled-before", string(state.ActivityStalled), &idleSince, lastSeen)
		hook.targetID = agent.ID
		hook.afterGet = func() {
			setAgentSuspendPolicySQL(t, s, agent.ID, store.SuspendPolicyNever)
		}
		fault.Arm()
		disp := &runIntentDispatcher{}

		suspended := srv.autoSuspendAgent(ctx, disp, agent, state.SuspendReasonStalled)
		assert.False(t, suspended)
		assert.Zero(t, disp.stops.Load(), "SwapAutoSuspendRunIntent must abort before DispatchAgentStop")

		after, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), after.Phase)
		assert.Equal(t, store.RunIntentRunning, after.RunIntent)
	})

	t.Run("membership-loss hold still records stopped intent on never-suspend agent", func(t *testing.T) {
		srv, s := testServer(t)
		setNeverSuspendExperiment(t, srv, true)

		agent := newIdleCandidateBrokerAgent(t, s, "ns-hold-unchanged", string(state.ActivityIdle), &idleSince, lastSeen)
		setAgentSuspendPolicySQL(t, s, agent.ID, store.SuspendPolicyNever)

		srv.recordHeldRunIntent(ctx, agent.ID)
		after, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.RunIntentStopped, after.RunIntent, "membership-loss hold must not be blocked by never-suspend")
	})
}

func TestNeverSuspend_ExistingAgentConflict(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID := tid("ns-existing-member")
	createTestUserWithRole(t, s, userID, "ns-existing-member@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	user, err := s.GetUser(ctx, userID)
	require.NoError(t, err)

	projectID := tid("ns-existing-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS Existing Project", Slug: "ns-existing-project",
		GitRemote: "https://github.com/example/ns-existing.git",
		OwnerID:   userID, CreatedBy: userID,
	}))
	createTestUserWithProjectRole(t, s, userID, user.Email, projectID, store.ProjectRoleMember)

	brokerID := tid("ns-existing-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "NS Existing Broker", Slug: "ns-existing-broker", Status: store.BrokerStatusOnline,
		CreatedBy: userID,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS Existing Broker",
		Status: store.BrokerStatusOnline,
	}))
	srv.SetDispatcher(&createAgentDispatcher{})
	setNeverSuspendExperiment(t, srv, true)

	t.Run("mismatched suspendPolicy on existing agent returns 409 and preserves stored policy", func(t *testing.T) {
		// Provision-only create with default policy ("auto").
		createRec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-exist-mismatch",
			"projectId":     projectID,
			"provisionOnly": true,
		})
		require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
		require.NotNil(t, created.Agent)
		assert.Equal(t, store.SuspendPolicyAuto, created.Agent.SuspendPolicy)

		// Second POST with same name and mismatched suspendPolicy="never" -> 409.
		repostRec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-exist-mismatch",
			"projectId":     projectID,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusConflict, repostRec.Code, "body: %s", repostRec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(repostRec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeSuspendPolicyCreateOnly, errResp.Error.Code)

		// Verify the stored row still has suspend_policy="auto" and was not started.
		stored, err := s.GetAgent(ctx, created.Agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, stored.SuspendPolicy)
		assert.NotEqual(t, string(state.PhaseRunning), stored.Phase)
	})

	t.Run("never-suspend agent re-POSTed with no suspendPolicy keeps never", func(t *testing.T) {
		createRec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-exist-keep",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
		require.NotNil(t, created.Agent)

		repostRec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":      "ns-exist-keep",
			"projectId": projectID,
		})
		require.Equal(t, http.StatusOK, repostRec.Code, "body: %s", repostRec.Body.String())

		stored, err := s.GetAgent(ctx, created.Agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, stored.SuspendPolicy)
	})

	t.Run("matching suspendPolicy on existing agent passes", func(t *testing.T) {
		createRec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-exist-match",
			"projectId":     projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, createRec.Code, "body: %s", createRec.Body.String())
		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
		require.NotNil(t, created.Agent)

		repostRec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-exist-match",
			"projectId":     projectID,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, repostRec.Code, "body: %s", repostRec.Body.String())

		stored, err := s.GetAgent(ctx, created.Agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, stored.SuspendPolicy)
	})
}

// suspendSpyDispatcher counts DispatchAgentStop calls and always succeeds.
type suspendSpyDispatcher struct {
	mockDispatcher
	stopCalls atomic.Int32
}

func (d *suspendSpyDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.stopCalls.Add(1)
	return nil
}

func TestNeverSuspend_ManualSuspendAndForce(t *testing.T) {
	srv, s := testServer(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.events = pub
	nd := NewNotificationDispatcher(s, srv.events, srv.GetDispatcher, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)
	srv.notificationDispatcher = nd

	disp := &suspendSpyDispatcher{}
	srv.SetDispatcher(disp)

	ctx := context.Background()

	creatorID := tid("ns-man-creator")
	adminID := tid("ns-man-admin")
	createTestUserWithRole(t, s, creatorID, "ns-man-creator@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	createTestUserWithRole(t, s, adminID, "ns-man-admin@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	creator, err := s.GetUser(ctx, creatorID)
	require.NoError(t, err)
	adminUser, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	projectID := tid("ns-man-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS Manual Project", Slug: "ns-man-project",
		GitRemote: "https://github.com/example/ns-man.git",
		OwnerID:   adminID, CreatedBy: adminID,
	}))
	createTestUserWithProjectRole(t, s, creatorID, creator.Email, projectID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, adminID, adminUser.Email, projectID, store.ProjectRoleOwner)

	brokerID := tid("ns-man-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "NS Manual Broker", Slug: "ns-man-broker", Status: store.BrokerStatusOnline,
		CreatedBy: adminID,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "NS Manual Broker",
		Status: store.BrokerStatusOnline,
	}))

	// Helper to create a running agent in this project with a resumable harness.
	makeRunningAgent := func(t *testing.T, slug, owner, policy, activity string) *store.Agent {
		t.Helper()
		ag := &store.Agent{
			ID:              tid("ns-man-" + slug),
			Slug:            slug,
			Name:            slug,
			ProjectID:       projectID,
			RuntimeBrokerID: brokerID,
			Phase:           string(state.PhaseRunning),
			Activity:        activity,
			RunIntent:       store.RunIntentRunning,
			RunID:           "run-" + slug,
			SuspendPolicy:   policy,
			CreatedBy:       owner,
			OwnerID:         owner,
			Ancestry:        []string{owner},
			AppliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: "claude",
			},
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
		_, err := srv.recordRunIntent(ctx, ag, store.RunIntentRunning)
		require.NoError(t, err)
		return ag
	}

	// Create an agent principal with full role (has agent.lifecycle) under adminID.
	actorAgent := &store.Agent{
		ID:        tid("ns-man-actor-agent"),
		Slug:      "ns-actor-agent",
		Name:      "ns-actor-agent",
		ProjectID: projectID,
		Phase:     string(state.PhaseRunning),
		CreatedBy: adminID,
		OwnerID:   adminID,
		Ancestry:  []string{adminID},
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleFull),
		},
	}
	require.NoError(t, s.CreateAgent(ctx, actorAgent))
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   adminID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    actorAgent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
	}))
	actorAgentToken, err := srv.GenerateAgentToken(
		actorAgent.ID, projectID, []string{adminID}, AgentRoleFull, nil,
	)
	require.NoError(t, err)

	t.Run("flagged agent without force returns 409 agent_never_suspend and leaves agent running", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		ag := makeRunningAgent(t, "no-force", creatorID, store.SuspendPolicyNever, string(state.ActivityThinking))
		beforeStops := disp.stopCalls.Load()

		rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend", nil)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())

		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeAgentNeverSuspend, errResp.Error.Code)
		assert.Equal(t, beforeStops, disp.stopCalls.Load(), "broker stop must not be dispatched")

		got, err := s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent, "run intent must not be mutated before refusal")
	})

	t.Run("agent principal with force returns 403 force_suspend_forbidden", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		ag := makeRunningAgent(t, "agent-force", adminID, store.SuspendPolicyNever, string(state.ActivityIdle))
		beforeStops := disp.stopCalls.Load()

		// Without force: 409 agent_never_suspend.
		rec409 := doRequestWithToken(t, srv, actorAgentToken, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend", nil)
		require.Equal(t, http.StatusConflict, rec409.Code, "body: %s", rec409.Body.String())
		var errResp409 ErrorResponse
		require.NoError(t, json.Unmarshal(rec409.Body.Bytes(), &errResp409))
		assert.Equal(t, ErrCodeAgentNeverSuspend, errResp409.Error.Code)

		// With force: 403 force_suspend_forbidden.
		rec403 := doRequestWithToken(t, srv, actorAgentToken, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend",
			AgentLifecycleStopRequest{Force: true})
		require.Equal(t, http.StatusForbidden, rec403.Code, "body: %s", rec403.Body.String())
		var errResp403 ErrorResponse
		require.NoError(t, json.Unmarshal(rec403.Body.Bytes(), &errResp403))
		assert.Equal(t, ErrCodeForceSuspendForbidden, errResp403.Error.Code)
		assert.Equal(t, beforeStops, disp.stopCalls.Load(), "broker stop must not be dispatched")

		got, err := s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent)
	})

	t.Run("human third-party force succeeds, writes audit, notifies creator, sets forced=true event, preserves flag", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		ag := makeRunningAgent(t, "human-force-3p", creatorID, store.SuspendPolicyNever, string(state.ActivityThinking))

		evCh, unsub := pub.Subscribe("agent." + ag.ID + ".status")
		defer unsub()

		rec := doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend",
			AgentLifecycleStopRequest{Force: true, Reason: "emergency maintenance"})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		got, err := s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)
		assert.False(t, state.SuspendReason(got.SuspendReason).AutoWakes(), "forced suspend must never auto-wake")
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy, "forced suspend must preserve suspend_policy=never")

		// Verify status event payload carries forced=true.
		select {
		case evt := <-evCh:
			var statusEvt AgentStatusEvent
			require.NoError(t, json.Unmarshal(evt.Data, &statusEvt))
			assert.Equal(t, string(state.PhaseSuspended), statusEvt.Phase)
			assert.True(t, statusEvt.Forced, "status event must carry forced=true")
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for AgentStatusEvent")
		}

		// Verify agent.suspend.forced mutation audit record.
		require.Eventually(t, func() bool {
			audits, _, aerr := s.ListMutationAudits(ctx, store.MutationAuditFilter{
				TargetType:   "agent",
				TargetID:     ag.ID,
				MutationType: mutationTypeAgentSuspendForced,
			})
			if aerr != nil || len(audits) != 1 {
				return false
			}
			assert.Equal(t, adminID, audits[0].ActorPrincipalID)
			assert.Contains(t, audits[0].BeforeSummary, "activity="+string(state.ActivityThinking))
			assert.Contains(t, audits[0].AfterSummary, "suspend_reason=user")
			return true
		}, 3*time.Second, 20*time.Millisecond, "expected agent.suspend.forced mutation audit")

		// Verify third-party creator notification fired (actor=admin != creator).
		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		var found bool
		for _, n := range notifs {
			if n.AgentID == ag.ID && n.Status == "SUSPENDED" {
				found = true
				assert.Contains(t, n.Message, "emergency maintenance")
			}
		}
		assert.True(t, found, "creator must receive third-party notification when another human force-suspends")
	})

	t.Run("creator self force-suspend succeeds without third-party notification", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		ag := makeRunningAgent(t, "creator-force-self", creatorID, store.SuspendPolicyNever, string(state.ActivityIdle))

		rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend",
			AgentLifecycleStopRequest{Force: true})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		srv.thirdPartyStopDispatches.Wait()

		got, err := s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy)

		// No third-party notification for ag.ID because actor == creator.
		notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, creatorID, false)
		require.NoError(t, err)
		for _, n := range notifs {
			assert.NotEqual(t, ag.ID, n.AgentID, "creator self-force-suspend must not emit a third-party notification")
		}
	})

	t.Run("experiment off allows plain suspend on flagged agent and ignores force", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, false)
		ag := makeRunningAgent(t, "exp-off-plain", creatorID, store.SuspendPolicyNever, string(state.ActivityIdle))

		evCh, unsub := pub.Subscribe("agent." + ag.ID + ".status")
		defer unsub()

		rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		got, err := s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy)

		select {
		case evt := <-evCh:
			var statusEvt AgentStatusEvent
			require.NoError(t, json.Unmarshal(evt.Data, &statusEvt))
			assert.False(t, statusEvt.Forced, "plain suspend with experiment off must not set forced=true")
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for AgentStatusEvent")
		}
	})

	t.Run("unflagged agent unaffected by force", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		ag := makeRunningAgent(t, "unflagged-force", creatorID, store.SuspendPolicyAuto, string(state.ActivityIdle))

		evCh, unsub := pub.Subscribe("agent." + ag.ID + ".status")
		defer unsub()

		rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend",
			AgentLifecycleStopRequest{Force: true})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		got, err := s.GetAgent(ctx, ag.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase)

		select {
		case evt := <-evCh:
			var statusEvt AgentStatusEvent
			require.NoError(t, json.Unmarshal(evt.Data, &statusEvt))
			assert.False(t, statusEvt.Forced, "force on an unflagged agent must not set forced=true")
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for AgentStatusEvent")
		}

		// No agent.suspend.forced audit for an unflagged agent.
		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			TargetType:   "agent",
			TargetID:     ag.ID,
			MutationType: mutationTypeAgentSuspendForced,
		})
		require.NoError(t, err)
		assert.Empty(t, audits)
	})
}

func TestNeverSuspend_MembershipLossHoldStillApplies(t *testing.T) {
	f := newMSFixture(t, "ns-memloss")
	setNeverSuspendExperiment(t, f.srv, true)
	setAgentSuspendPolicySQL(t, f.s, f.agentA.ID, store.SuspendPolicyNever)

	ctx := mmrServiceCtx(f.ownerID, f.ownerID+"@test.com")
	_, d := f.srv.membershipService.RemoveMember(ctx, MembershipRequest{
		Op:        MembershipOpRemove,
		ProjectID: f.projectID,
		Actor:     f.ownerIdentity(),
		BindingID: f.userBinding(f.userID).ID,
	})
	require.Nil(t, d)
	requireStandingReason(t, f.srv.agentStanding(context.Background(), f.agentA.ID), standingReasonRootNotAdmitted)
	requireCheck(t, f.s, f.userID, store.MembershipLossTriggerMemberRemove)
	f.requireTreeHeldAndRefused()
}

func TestNeverSuspend_AlreadySuspendedConversion(t *testing.T) {
	srv, s := testServer(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.events = pub
	nd := NewNotificationDispatcher(s, srv.events, srv.GetDispatcher, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)
	srv.notificationDispatcher = nd

	ctx := context.Background()

	creatorID := tid("ns-conv-creator")
	adminID := tid("ns-conv-admin")
	createTestUserWithRole(t, s, creatorID, "ns-conv-creator@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	createTestUserWithRole(t, s, adminID, "ns-conv-admin@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	creator, err := s.GetUser(ctx, creatorID)
	require.NoError(t, err)
	adminUser, err := s.GetUser(ctx, adminID)
	require.NoError(t, err)

	projectID := tid("ns-conv-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "NS Conv Project", Slug: "ns-conv-project",
		GitRemote: "https://github.com/example/ns-conv.git",
		OwnerID:   adminID, CreatedBy: adminID,
	}))
	createTestUserWithProjectRole(t, s, creatorID, creator.Email, projectID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, adminID, adminUser.Email, projectID, store.ProjectRoleOwner)

	actorAgent := &store.Agent{
		ID:        tid("ns-conv-actor-agent"),
		Slug:      "ns-conv-actor",
		Name:      "ns-conv-actor",
		ProjectID: projectID,
		Phase:     string(state.PhaseRunning),
		CreatedBy: adminID,
		OwnerID:   adminID,
		Ancestry:  []string{adminID},
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleFull),
		},
	}
	require.NoError(t, s.CreateAgent(ctx, actorAgent))
	require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   adminID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    actorAgent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       projectID,
		Role:          string(AgentRoleFull),
		Active:        true,
	}))
	actorAgentToken, err := srv.GenerateAgentToken(
		actorAgent.ID, projectID, []string{adminID}, AgentRoleFull, nil,
	)
	require.NoError(t, err)

	makeSuspendedAgent := func(t *testing.T, slug, suspendReason string) *store.Agent {
		t.Helper()
		ag := &store.Agent{
			ID:            tid("ns-conv-" + slug),
			Slug:          slug,
			Name:          slug,
			ProjectID:     projectID,
			Phase:         string(state.PhaseSuspended),
			SuspendReason: suspendReason,
			SuspendPolicy: store.SuspendPolicyNever,
			CreatedBy:     creatorID,
			OwnerID:       creatorID,
			Ancestry:      []string{creatorID},
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
		return ag
	}

	for _, initialReason := range []string{string(state.SuspendReasonIdle), string(state.SuspendReasonStalled)} {
		t.Run("reason="+initialReason, func(t *testing.T) {
			t.Run("user without force gets 409 and reason unchanged", func(t *testing.T) {
				setNeverSuspendExperiment(t, srv, true)
				ag := makeSuspendedAgent(t, "conv-noforce-"+initialReason, initialReason)

				rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend", nil)
				require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
				assert.Equal(t, ErrCodeAgentNeverSuspend, errResp.Error.Code)

				got, err := s.GetAgent(ctx, ag.ID)
				require.NoError(t, err)
				assert.Equal(t, initialReason, got.SuspendReason)
			})

			t.Run("agent principal with force gets 403 and reason unchanged", func(t *testing.T) {
				setNeverSuspendExperiment(t, srv, true)
				ag := makeSuspendedAgent(t, "conv-agentforce-"+initialReason, initialReason)

				rec := doRequestWithToken(t, srv, actorAgentToken, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend",
					AgentLifecycleStopRequest{Force: true})
				require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
				assert.Equal(t, ErrCodeForceSuspendForbidden, errResp.Error.Code)

				got, err := s.GetAgent(ctx, ag.ID)
				require.NoError(t, err)
				assert.Equal(t, initialReason, got.SuspendReason)
			})

			t.Run("human with force converts to user, writes one audit, and sets forced=true event", func(t *testing.T) {
				setNeverSuspendExperiment(t, srv, true)
				ag := makeSuspendedAgent(t, "conv-humanforce-"+initialReason, initialReason)

				evCh, unsub := pub.Subscribe("agent." + ag.ID + ".status")
				defer unsub()

				rec := doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend",
					AgentLifecycleStopRequest{Force: true, Reason: "convert to manual"})
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				srv.thirdPartyStopDispatches.Wait()

				got, err := s.GetAgent(ctx, ag.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)
				assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy)

				select {
				case evt := <-evCh:
					var statusEvt AgentStatusEvent
					require.NoError(t, json.Unmarshal(evt.Data, &statusEvt))
					assert.True(t, statusEvt.Forced, "forced conversion status event must carry forced=true")
				case <-time.After(2 * time.Second):
					t.Fatal("timed out waiting for AgentStatusEvent")
				}

				require.Eventually(t, func() bool {
					audits, _, aerr := s.ListMutationAudits(ctx, store.MutationAuditFilter{
						TargetType:   "agent",
						TargetID:     ag.ID,
						MutationType: mutationTypeAgentSuspendForced,
					})
					return aerr == nil && len(audits) == 1 && audits[0].ActorPrincipalID == adminID
				}, 3*time.Second, 20*time.Millisecond, "expected exactly one agent.suspend.forced audit")
			})

			t.Run("experiment off converts without force as before", func(t *testing.T) {
				setNeverSuspendExperiment(t, srv, false)
				ag := makeSuspendedAgent(t, "conv-expoff-"+initialReason, initialReason)

				rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend", nil)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

				got, err := s.GetAgent(ctx, ag.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.SuspendReasonUser), got.SuspendReason)

				audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
					TargetType:   "agent",
					TargetID:     ag.ID,
					MutationType: mutationTypeAgentSuspendForced,
				})
				require.NoError(t, err)
				assert.Empty(t, audits)
			})
		})
	}

	t.Run("already user-suspended never-suspend agent remains idempotent 200 without force", func(t *testing.T) {
		setNeverSuspendExperiment(t, srv, true)
		ag := makeSuspendedAgent(t, "conv-already-user", string(state.SuspendReasonUser))

		rec := doRequestAsUser(t, srv, creator, http.MethodPost, "/api/v1/agents/"+ag.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
			TargetType:   "agent",
			TargetID:     ag.ID,
			MutationType: mutationTypeAgentSuspendForced,
		})
		require.NoError(t, err)
		assert.Empty(t, audits)
	})
}

func TestNeverSuspend_ForcePrincipalAllowlist(t *testing.T) {
	srv, _ := testServer(t)
	setNeverSuspendExperiment(t, srv, true)
	flagged := &store.Agent{
		ID:            tid("ns-allowlist-agent"),
		Name:          "lead",
		SuspendPolicy: store.SuspendPolicyNever,
	}

	allowed := []struct {
		name string
		id   Identity
	}{
		{"AuthenticatedUser", NewAuthenticatedUser("u1", "u1@test.com", "User", "member", "session")},
		{"DevUser", NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev"})},
	}
	for _, tc := range allowed {
		t.Run("allow/"+tc.name, func(t *testing.T) {
			assert.True(t, isHumanForceSuspendPrincipal(tc.id))
			ctx := withLifecycleSuspendForce(contextWithIdentity(context.Background(), tc.id))
			outCtx, forced, err := srv.neverSuspendOverride(ctx, flagged)
			require.NoError(t, err)
			assert.True(t, forced)
			assert.True(t, lifecycleSuspendForcedFromContext(outCtx))
		})
	}

	denied := []struct {
		name string
		id   Identity
	}{
		{"nil", nil},
		{"DelegatedAgentIdentity", &DelegatedAgentIdentity{agentID: tid("ns-delegated-agent")}},
		{"FederatedServiceIdentity", NewFederatedServiceIdentity("https://issuer.example", "svc", "svc@test.com", nil)},
		{"FederatedUserIdentity", NewFederatedUserIdentity("https://issuer.example", "sub", "fed@test.com", "Fed", "member", nil)},
	}
	for _, tc := range denied {
		t.Run("deny/"+tc.name, func(t *testing.T) {
			assert.False(t, isHumanForceSuspendPrincipal(tc.id))
			ctx := withLifecycleSuspendForce(contextWithIdentity(context.Background(), tc.id))
			_, forced, err := srv.neverSuspendOverride(ctx, flagged)
			require.Error(t, err)
			var forbidden *errForceSuspendForbidden
			assert.ErrorAs(t, err, &forbidden)
			assert.False(t, forced)
		})
	}
}
