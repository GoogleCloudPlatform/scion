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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2087: the create-failure cleanup (runtime delete, agent row
// delete, quota release) must not run on the request's context. When the
// request is canceled before the failure is handled — the client
// disconnected, or a dispatch hit its control-channel timeout — every one of
// those calls failed with "context canceled", leaving the reservations held
// and, when the row delete happened to succeed first, no agent row left for
// the normal delete path to reclaim them from.

// requestCancelWait bounds how long a mock waits to observe the request ctx
// being canceled after it fires cancelRequest. If a refactor stops passing
// the request ctx through, the test fails on ctxNotCanceled instead of
// hanging until the package timeout.
const requestCancelWait = 5 * time.Second

// awaitCanceled waits up to requestCancelWait for ctx to be canceled and
// reports whether it was.
func awaitCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(requestCancelWait):
		return false
	}
}

// setAgentQuotaLimits gives both create-time limits a positive value so
// that Reserve actually records a reservation for each: both are seeded
// unlimited (0), and QuotaService.Reserve skips an unlimited limit entirely,
// which would make a "no reservation left" assertion vacuous.
func setAgentQuotaLimits(t *testing.T, s store.Store) {
	t.Helper()
	setBrokerAgentCeiling(t, s, 5)
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	def.DefaultValue = 5
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

// reservationsHeld records whether each create-time reservation is held for
// an agent ID at some point, so a test can prove the reservations existed
// before asserting the cleanup released them.
type reservationsHeld struct {
	broker, project bool
}

func observeReservations(t *testing.T, s store.Store, agentID string) reservationsHeld {
	return reservationsHeld{
		broker:  hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID),
		project: hasReservation(t, s, store.LimitMaxAgentsPerProject, agentID),
	}
}

func assertReservationsHeldBeforeCleanup(t *testing.T, held reservationsHeld) {
	t.Helper()
	require.True(t, held.broker, "precondition: the per-broker reservation must be held before cleanup")
	require.True(t, held.project, "precondition: the per-project reservation must be held before cleanup")
}

func assertNoReservations(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID),
		"a create canceled before its failure cleanup must release the per-broker reservation")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, agentID),
		"a create canceled before its failure cleanup must release the per-project reservation")
}

// ctxObservation records how a cleanup step's ctx looked when it ran.
type ctxObservation struct {
	called      bool
	err         error
	hasDeadline bool
	budget      time.Duration
}

func observeCtx(ctx context.Context) ctxObservation {
	o := ctxObservation{called: true, err: ctx.Err()}
	var deadline time.Time
	deadline, o.hasDeadline = ctx.Deadline()
	if o.hasDeadline {
		o.budget = time.Until(deadline)
	}
	return o
}

// assertLiveBoundedCtx asserts a cleanup step ran on a live,
// deadline-bounded ctx whose budget exceeds minBudget.
func assertLiveBoundedCtx(t *testing.T, o ctxObservation, step string, minBudget time.Duration) {
	t.Helper()
	require.True(t, o.called, "cleanup must still run the %s", step)
	assert.NoError(t, o.err, "the %s must not run on the canceled request context", step)
	assert.True(t, o.hasDeadline, "the %s must run under its own bounded budget", step)
	assert.Greater(t, o.budget, minBudget, "the %s budget is too small", step)
}

// cancelingCreateDispatcher cancels the in-flight request's context from
// inside DispatchAgentCreateWithGather, then fails the create (either with an
// error or with missing env vars), so the handler's failure cleanup runs
// with the request context already canceled.
type cancelingCreateDispatcher struct {
	createAgentDispatcher
	t             *testing.T
	s             store.Store
	cancelRequest context.CancelFunc
	createErr     error

	heldBeforeCleanup   reservationsHeld
	dispatchCtxCanceled bool
	// reqCtx is the request's context; requestDoneAtCleanup records whether
	// it was done when the failure cleanup ran.
	reqCtx               context.Context
	requestDoneAtCleanup bool
	delete               ctxObservation
	// credJTI is a credential the mock records for the agent, standing in
	// for the one a real dispatcher mints, so the test can observe whether
	// the cleanup revoked it.
	credJTI string
}

func (d *cancelingCreateDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.capturedAgent = agent
	d.heldBeforeCleanup = observeReservations(d.t, d.s, agent.ID)
	d.credJTI = "p0-cred-" + agent.ID
	now := time.Now()
	require.NoError(d.t, d.s.CreateAgentCredential(context.Background(), &store.AgentCredential{
		AgentID:      agent.ID,
		ProjectID:    agent.ProjectID,
		TokenJTIHash: hashJTI(d.credJTI),
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	}))
	d.cancelRequest()
	// A create-and-start runs under a start claim on a context detached from
	// the request (a start runs to its outcome, bounded by
	// start_max_duration), so the dispatch does not see the cancel; the
	// handler's own context, which the failure cleanup must not depend on,
	// is canceled.
	d.dispatchCtxCanceled = ctx.Err() != nil
	if d.createErr != nil {
		return nil, d.createErr
	}
	return envReqsResult(d.envReqs), nil
}

func (d *cancelingCreateDispatcher) DispatchAgentDelete(ctx context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	d.delete = observeCtx(ctx)
	if d.reqCtx != nil {
		d.requestDoneAtCleanup = d.reqCtx.Err() != nil
	}
	return nil
}

// newCancelableCreate builds a create request whose context the returned
// cancel func cancels; serve runs it through the handler. Tests hand cancel
// to a mock that fires it mid-handler, before the failure cleanup runs.
func newCancelableCreate(t *testing.T, srv *Server, body CreateAgentRequest) (cancel context.CancelFunc, serve func()) {
	cancel, serve, _ = newCancelableCreateCtx(t, srv, body)
	return cancel, serve
}

// newCancelableCreateCtx is newCancelableCreate that also returns the
// request's context.
func newCancelableCreateCtx(t *testing.T, srv *Server, body CreateAgentRequest) (cancel context.CancelFunc, serve func(), reqCtx context.Context) {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	reqCtx, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(bodyBytes)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	return cancel, func() { srv.Handler().ServeHTTP(httptest.NewRecorder(), req) }, reqCtx
}

func TestCreateAgent_CanceledRequest_FailureCleanupStillRuns(t *testing.T) {
	cases := []struct {
		name      string
		gatherEnv bool
		createErr error
		envReqs   *RemoteEnvRequirementsResponse
		// wantRevoked: only the missing-env path revokes in the cleanup. On
		// a dispatch error the real dispatcher has already revoked (this mock
		// does not), so the cleanup must leave the credential alone rather
		// than revoke a second time.
		wantRevoked bool
	}{
		{name: "dispatch failure", createErr: fmt.Errorf("simulated broker dispatch failure")},
		{name: "gather-env dispatch failure", gatherEnv: true, createErr: fmt.Errorf("simulated broker dispatch failure")},
		{name: "missing env vars", envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}}, wantRevoked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &cancelingCreateDispatcher{t: t, createErr: tc.createErr}
			disp.envReqs = tc.envReqs
			srv, s, project := setupCreateAgentServer(t, disp)
			disp.s = s
			setAgentQuotaLimits(t, s)

			cancel, serve, reqCtx := newCancelableCreateCtx(t, srv, CreateAgentRequest{
				Name:      "canceled-create-agent",
				ProjectID: project.ID,
				Task:      "do something",
				GatherEnv: tc.gatherEnv,
			})
			disp.cancelRequest = cancel
			disp.reqCtx = reqCtx
			serve()

			require.NotNil(t, disp.capturedAgent, "dispatcher must have observed the create-time agent")
			require.False(t, disp.dispatchCtxCanceled, "the create runs detached from the canceled request")
			if tc.createErr != nil {
				require.True(t, disp.requestDoneAtCleanup, "the request ctx is done when the failure cleanup runs")
			}
			agentID := disp.capturedAgent.ID
			assertReservationsHeldBeforeCleanup(t, disp.heldBeforeCleanup)

			// The budget must leave a cross-node delete's full deferred wait
			// (dispatchDeleteTimeout) intact.
			assertLiveBoundedCtx(t, disp.delete, "runtime delete", dispatchDeleteTimeout)

			_, err := s.GetAgent(context.Background(), agentID)
			assert.ErrorIs(t, err, store.ErrNotFound, "cleanup must delete the agent row despite the canceled request")

			assertNoReservations(t, s, agentID)

			cred := getTestAgentCredential(t, s, disp.credJTI)
			if tc.wantRevoked {
				require.NotNil(t, cred.RevokedAt, "the missing-env cleanup must revoke the credential despite the canceled request")
				require.NotNil(t, cred.RevokeReason)
				assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
			} else {
				assert.Nil(t, cred.RevokedAt, "the cleanup must not revoke on a dispatch error: the dispatcher already did")
			}
		})
	}
}

// cancelingManagedAgentBackend cancels the request context from inside
// managedAgentCreate's CreateInteraction call, then fails it.
type cancelingManagedAgentBackend struct {
	failingManagedAgentBackend
	t             *testing.T
	s             store.Store
	projectID     string
	cancelRequest context.CancelFunc

	agentID           string
	heldBeforeCleanup reservationsHeld
	ctxNotCanceled    bool
}

func (b *cancelingManagedAgentBackend) CreateInteraction(ctx context.Context, _ managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	// The backend is not handed the agent; the row is already written, so
	// recover its ID from the store.
	result, err := b.s.ListAgents(context.Background(), store.AgentFilter{ProjectID: b.projectID}, store.ListOptions{})
	if err == nil && len(result.Items) == 1 {
		b.agentID = result.Items[0].ID
		b.heldBeforeCleanup = observeReservations(b.t, b.s, b.agentID)
	}
	b.cancelRequest()
	b.ctxNotCanceled = !awaitCanceled(ctx)
	return nil, fmt.Errorf("simulated managed agent create failure")
}

// Must not run in parallel: it swaps the package-level managedBackendInst.
func TestCreateAgent_CanceledRequest_ManagedFailureCleanupStillRuns(t *testing.T) {
	backend := &cancelingManagedAgentBackend{t: t}
	managedBackendMu.Lock()
	prevBackend := managedBackendInst
	managedBackendInst = backend
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prevBackend
		managedBackendMu.Unlock()
	})

	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	backend.s, backend.projectID = s, project.ID
	setAgentQuotaLimits(t, s)

	cancel, serve := newCancelableCreate(t, srv, CreateAgentRequest{
		Name:      "canceled-managed-agent",
		ProjectID: project.ID,
		Task:      "do something",
		Profile:   ManagedAgentsProfile,
	})
	backend.cancelRequest = cancel
	serve()

	require.NotEmpty(t, backend.agentID, "backend must have observed the create-time agent row")
	require.False(t, backend.ctxNotCanceled, "the backend must see the request ctx canceled")
	assertReservationsHeldBeforeCleanup(t, backend.heldBeforeCleanup)

	_, err := s.GetAgent(context.Background(), backend.agentID)
	assert.ErrorIs(t, err, store.ErrNotFound, "cleanup must delete the agent row despite the canceled request")

	assertNoReservations(t, s, backend.agentID)
}

// TestCleanupFailedCreate_CanceledCtx_EveryStepRunsDetached drives
// cleanupFailedCreate directly with an already-canceled ctx and a recording
// runtime-delete step. The managed end-to-end test above cannot observe its
// runtime step's ctx: managedAgentDelete only reaches the backend (with a
// ctx) when an interaction ID was recorded, and a create that failed in
// CreateInteraction never recorded one. This covers that step, and the
// helper's contract for any deleteRuntime, independently of the wiring.
func TestCleanupFailedCreate_CanceledCtx_EveryStepRunsDetached(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	ctx := context.Background()
	setAgentQuotaLimits(t, s)

	agent := &store.Agent{
		ID:              tid("cleanup-failed-create"),
		Name:            "cleanup-failed-create",
		Slug:            "cleanup-failed-create",
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err := srv.quotaService.Reserve(ctx, store.LimitMaxAgentsPerBroker, DevUserID, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.ID)
	require.NoError(t, err)
	_, err = srv.quotaService.Reserve(ctx, store.LimitMaxAgentsPerProject, DevUserID, store.QuotaScopeProject, project.ID, agent.ID)
	require.NoError(t, err)
	assertReservationsHeldBeforeCleanup(t, observeReservations(t, s, agent.ID))

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	var runtimeDelete ctxObservation
	srv.cleanupFailedCreate(canceled, agent, agent.RuntimeBrokerID, cleanupSkipRevoke, func(cctx context.Context) error {
		runtimeDelete = observeCtx(cctx)
		return nil
	})

	assertLiveBoundedCtx(t, runtimeDelete, "runtime delete", dispatchDeleteTimeout)
	_, err = s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "cleanup must delete the agent row despite the canceled ctx")
	assertNoReservations(t, s, agent.ID)
}

// cancelAfterDeleteStore cancels the request after DeleteAgent of targetID
// succeeds: the exact "stranded reservation" window from ptone/scion#2087,
// where the row is gone and only the quota release is left to run.
type cancelAfterDeleteStore struct {
	store.Store
	targetID      string
	cancelRequest context.CancelFunc
	deleted       bool
}

func (c *cancelAfterDeleteStore) DeleteAgent(ctx context.Context, id string) error {
	err := c.Store.DeleteAgent(ctx, id)
	if err == nil && id == c.targetID {
		c.deleted = true
		c.cancelRequest()
	}
	return err
}

// TestHandleExistingAgent_EnvGatherRecreate_CanceledAfterRowDelete_ReleasesQuota
// covers handleExistingAgent's env-gather re-provisioning branch, which
// hard-deletes an existing provisioning agent and then releases its
// reservations. A request canceled between the two must still release both:
// the stale-reservation reconcile only reclaims max_agents_per_broker, so a
// missed per-project release would be stranded for good.
func TestHandleExistingAgent_EnvGatherRecreate_CanceledAfterRowDelete_ReleasesQuota(t *testing.T) {
	disp := &createAgentDispatcher{
		// Non-nil requirements on a GatherEnv create: 202, agent left in
		// provisioning — the state handleExistingAgent's branch acts on.
		envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}},
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)

	body := CreateAgentRequest{
		Name:      "env-gather-recreate",
		ProjectID: project.ID,
		Task:      "do something",
		GatherEnv: true,
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.NotNil(t, disp.capturedAgent)
	oldID := disp.capturedAgent.ID
	assertReservationsHeldBeforeCleanup(t, observeReservations(t, s, oldID))

	cancel, serve := newCancelableCreate(t, srv, body)
	wrapped := &cancelAfterDeleteStore{Store: s, targetID: oldID, cancelRequest: cancel}
	srv.store = wrapped
	t.Cleanup(func() { srv.store = s })
	serve()

	require.True(t, wrapped.deleted, "the env-gather recreate must hard-delete the existing provisioning agent")
	assertNoReservations(t, s, oldID)
}

// TestCreateAgent_WorkspaceBootstrapNoStorage_CleansUp covers the
// workspace-bootstrap failure returns, which come after the row and
// reservations were written but before any dispatch: with no storage
// configured, the create must still remove the row and release both
// reservations.
func TestCreateAgent_WorkspaceBootstrapNoStorage_CleansUp(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &createAgentDispatcher{})
	require.Nil(t, srv.GetStorage(), "precondition: no storage configured")
	setAgentQuotaLimits(t, s)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:           "bootstrap-no-storage",
		ProjectID:      project.ID,
		Task:           "do something",
		WorkspaceFiles: []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}},
	})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "Storage not configured")

	result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "a failed workspace-bootstrap create must delete its agent row")

	assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID),
		"a failed workspace-bootstrap create must release the per-broker reservation")
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	projectReservations, err := s.CountActiveReservations(ctx, def.ID, DevUserID, store.QuotaScopeProject, project.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, projectReservations,
		"a failed workspace-bootstrap create must release the per-project reservation")
}
