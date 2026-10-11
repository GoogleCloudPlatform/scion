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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preClaimReadStore returns agent rows as they read before a delete claim
// (deletion columns cleared), so the start gate passes and the delete claim
// is met only by the start's own writes: the race in which a delete claims
// the row between the gate and the running-intent write.
type preClaimReadStore struct {
	store.Store
}

func (s preClaimReadStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if err != nil || a == nil {
		return a, err
	}
	return preClaim(a), nil
}

func (s preClaimReadStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	a, err := s.Store.GetAgentBySlug(ctx, projectID, slug)
	if err != nil || a == nil {
		return a, err
	}
	return preClaim(a), nil
}

func preClaim(a *store.Agent) *store.Agent {
	c := *a
	c.DeletionState = ""
	c.DeletionLeaseAt = nil
	return &c
}

// requireIntentDeleteInProgress asserts the delete_in_progress answer of a
// refused running-intent write, in the shape every delete_in_progress
// answer has: details.agentId names the agent (round 6 n1).
func requireIntentDeleteInProgress(t *testing.T, rec *httptest.ResponseRecorder, agentID string) {
	t.Helper()
	requireDeleteInProgress(t, rec)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
	assert.Equal(t, agentID, body.Error.Details["agentId"], "details.agentId")
}

// A delete that claims the row after the start gate passed refuses the
// start's running-intent write: lifecycle start and restart answer 409
// delete_in_progress, nothing is dispatched (restart's stop leg included),
// and the stored intent stays as the delete left it (ptone/scion#2550,
// round 5 N2). Create-on-existing and the workspace sync-to finalize are
// covered by the tests below.
func TestRunIntent_DeleteClaimAfterGateRefusesStart(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, base := testServer(t)
			disp := &deleteGuardDispatcher{}
			srv.SetDispatcher(disp)
			agent := setupBrokerAgentInPhase(t, base, "ri-claim-"+action, state.PhaseStopped)
			srv.store = preClaimReadStore{Store: base}
			ctx := context.Background()
			_, err := base.SetRunIntent(ctx, agent.ID, store.RunIntentStopped)
			require.NoError(t, err)
			seedAgentDeletion(t, base, agent.ID, seedLiveDeleting)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			requireIntentDeleteInProgress(t, rec, agent.ID)
			assert.Zero(t, disp.starts, "no start dispatch")
			assert.Zero(t, disp.stops, "no stop dispatch")

			got, err := base.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, store.RunIntentStopped, got.RunIntent, "intent is not left running")
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
		})
	}
}

// The DM wake path maps the refused running intent to delete_in_progress
// too, not to a runtime error (round 5 N2).
func TestRunIntent_DeleteClaimAfterGateRefusesWake(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteGuardDispatcher{}
	srv.SetDispatcher(disp)
	agent := setupBrokerAgentInPhase(t, s, "ri-claim-wake", state.PhaseSuspended)
	ctx := context.Background()
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentStopped)
	require.NoError(t, err)
	// The wake's caller read the row before the claim.
	stale, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)

	_, dmErr := srv.wakeAgentForDM(ctx, stale)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Zero(t, disp.starts, "no start dispatch")
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
}

// claimBetweenLegsDispatcher is quotaLifecycleDispatcher whose start fails
// the way beginRun does when a delete claimed the row between a restart's
// stop and start legs.
type claimBetweenLegsDispatcher struct {
	quotaLifecycleDispatcher
	failStart bool
}

func (d *claimBetweenLegsDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	if d.failStart {
		d.startCount.Add(1)
		return fmt.Errorf("begin run: %w", store.ErrDeleteInProgress)
	}
	return d.quotaLifecycleDispatcher.DispatchAgentStart(ctx, agent, task, resume)
}

// A restart whose start leg is refused because a delete claimed the row
// after the stop leg leaves the row to the delete engine: no stopped
// status write and no release of the reservation the agent held; the
// caller gets 409 delete_in_progress (ptone/scion#2550, round 5 N3).
func TestRestart_DeleteClaimBetweenLegsLeavesRowToEngine(t *testing.T) {
	disp := &claimBetweenLegsDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "restart-claim", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.EqualValues(t, 1, brokerReservationCount(t, s, brokerID))
	before, err := s.GetAgent(context.Background(), created.Agent.ID)
	require.NoError(t, err)
	require.NotEqual(t, string(state.PhaseStopped), before.Phase)

	disp.failStart = true
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/restart", nil)
	requireDeleteInProgress(t, rec)
	require.EqualValues(t, 1, disp.stopCount.Load(), "the stop leg ran")
	require.EqualValues(t, 1, disp.startCount.Load(), "the start leg was attempted")

	assert.EqualValues(t, 1, brokerReservationCount(t, s, brokerID), "the reservation is left to the delete engine")
	got, err := s.GetAgent(context.Background(), created.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Phase, got.Phase, "no stopped status write")
	assert.NotEqual(t, "stopped", got.ContainerStatus)
}

// A stopped agent holds no reservation, so its restart reserves a slot.
// When the start leg is refused because a delete claimed the row, that
// slot is this call's and is rolled back; it does not wait for reconcile
// (ptone/scion#2550, round 6 N3).
func TestRestart_DeleteClaimBetweenLegsRollsBackOwnReservation(t *testing.T) {
	disp := &claimBetweenLegsDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 1)
	brokerID := project.DefaultRuntimeBrokerID

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "restart-claim-stopped", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/stop", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	require.EqualValues(t, 0, brokerReservationCount(t, s, brokerID), "a stopped agent holds no reservation")

	disp.failStart = true
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/restart", nil)
	requireIntentDeleteInProgress(t, rec, created.Agent.ID)
	require.EqualValues(t, 1, disp.startCount.Load(), "the start leg was attempted")
	assert.EqualValues(t, 0, brokerReservationCount(t, s, brokerID), "the restart's own reservation is rolled back")
}

// Create-on-existing (handleExistingAgent): each path that starts the
// existing agent writes the running intent first, and a delete claim that
// landed after the name lookup refuses it with 409 delete_in_progress and
// no start (ptone/scion#2550, round 6 N4).
func TestRunIntent_DeleteClaimAfterGateRefusesCreateOnExisting(t *testing.T) {
	cases := []struct {
		name  string
		phase state.Phase
		req   CreateAgentRequest
	}{
		{name: "suspended-resume", phase: state.PhaseSuspended},
		{name: "stopped-resume", phase: state.PhaseStopped, req: CreateAgentRequest{Resume: true}},
		{name: "provisioning-start", phase: state.PhaseProvisioning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newSiteIntentDispatcher(nil)
			srv, base, project := setupCreateAgentServer(t, disp)
			disp.s = base
			name := "ri-claim-existing-" + tc.name
			agent := createSiteAgent(t, base, project, name, tc.phase, store.RunIntentStopped)
			srv.store = preClaimReadStore{Store: base}
			seedAgentDeletion(t, base, agent.ID, seedLiveDeleting)

			req := tc.req
			req.Name, req.ProjectID, req.Task = name, project.ID, "work"
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
			requireIntentDeleteInProgress(t, rec, agent.ID)
			assert.Empty(t, disp.intents("start"), "no start dispatch")
			got, err := base.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, store.RunIntentStopped, got.RunIntent, "intent is not left running")
		})
	}
}

// The workspace sync-to finalize bootstrap dispatch writes the running
// intent first; a delete claim refuses it with 409 delete_in_progress and
// no create dispatch (ptone/scion#2550, round 6 N4).
func TestRunIntent_DeleteClaimAfterGateRefusesSyncFinalize(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, base, project := setupCreateAgentServer(t, disp)
	disp.s = base
	srv.SetStorage(newMockStorage("test-bucket"))
	agent := createSiteAgent(t, base, project, "ri-claim-sync", state.PhaseProvisioning, store.RunIntentStopped)
	srv.store = preClaimReadStore{Store: base}
	seedAgentDeletion(t, base, agent.ID, seedLiveDeleting)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize",
		map[string]any{"manifest": map[string]any{"version": "1.0", "files": []any{}}})
	requireIntentDeleteInProgress(t, rec, agent.ID)
	assert.Empty(t, disp.intents("create"), "no create dispatch")
	got, err := base.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent, "intent is not left running")
}

// The managed-agent lifecycle writes the running intent before it acts; a
// delete claim refuses it with 409 delete_in_progress (details.agentId)
// and the intent stays stopped (ptone/scion#2550, round 7 nit-3).
func TestRunIntent_DeleteClaimAfterGateRefusesManagedStart(t *testing.T) {
	managedBackendMu.Lock()
	prevBackend := managedBackendInst
	managedBackendInst = stubManagedAgentBackend{}
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prevBackend
		managedBackendMu.Unlock()
	})

	srv, base := testServer(t)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, base, "ri-claim-managed", state.PhaseStopped)
	agent.Runtime = ManagedRuntimePrefix + "stub"
	require.NoError(t, base.UpdateAgent(ctx, agent))
	_, err := base.SetRunIntent(ctx, agent.ID, store.RunIntentStopped)
	require.NoError(t, err)
	srv.store = preClaimReadStore{Store: base}
	seedAgentDeletion(t, base, agent.ID, seedLiveDeleting)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	requireIntentDeleteInProgress(t, rec, agent.ID)
	got, err := base.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent, "intent is not left running")
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
}

// claimOnRunningIntentStore lands a delete claim just before a running
// intent is written: for a fresh create, the window between the row's
// creation and its running-intent write.
type claimOnRunningIntentStore struct {
	store.Store
	t *testing.T
}

func (s claimOnRunningIntentStore) SwapRunIntent(ctx context.Context, agentID string, intent store.RunIntent) (store.RunIntent, time.Time, error) {
	if intent == store.RunIntentRunning {
		seedAgentDeletion(s.t, s.Store, agentID, seedLiveDeleting)
	}
	return s.Store.SwapRunIntent(ctx, agentID, intent)
}

// ClaimAgentStart is the running-intent write of a start that runs under a
// start claim.
func (s claimOnRunningIntentStore) ClaimAgentStart(ctx context.Context, agentID, owner string, kind store.StartClaimKind, target string, ttl time.Duration) (store.StartClaim, error) {
	seedAgentDeletion(s.t, s.Store, agentID, seedLiveDeleting)
	return s.Store.ClaimAgentStart(ctx, agentID, owner, kind, target, ttl)
}

// A fresh create whose running-intent write meets a delete claim answers
// 409 delete_in_progress (details.agentId) with no create dispatch
// (ptone/scion#2550, round 7 nit-3).
func TestRunIntent_DeleteClaimRefusesFreshCreate(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, base, project := setupCreateAgentServer(t, disp)
	disp.s = base
	srv.store = claimOnRunningIntentStore{Store: base, t: t}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "ri-claim-fresh", ProjectID: project.ID, Task: "work",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
	assert.NotEmpty(t, body.Error.Details["agentId"], "details.agentId")
	assert.Empty(t, disp.intents("create"), "no create dispatch")
}
