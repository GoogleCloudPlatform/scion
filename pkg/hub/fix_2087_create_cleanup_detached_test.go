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

// cancelingCreateDispatcher cancels the in-flight request's context from
// inside DispatchAgentCreateWithGather, then fails the create (either with an
// error or with missing env vars), so the handler's failure cleanup runs
// with the request context already canceled.
type cancelingCreateDispatcher struct {
	createAgentDispatcher
	cancelRequest context.CancelFunc
	createErr     error

	// What the cleanup's DispatchAgentDelete observed.
	deleteCtxErr      error
	deleteHasDeadline bool
	deleteBudget      time.Duration
}

func (d *cancelingCreateDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	d.capturedAgent = agent
	d.cancelRequest()
	<-ctx.Done() // the handler's ctx is the request's: confirm it is canceled
	if d.createErr != nil {
		return nil, d.createErr
	}
	return d.envReqs, nil
}

func (d *cancelingCreateDispatcher) DispatchAgentDelete(ctx context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	d.deleteCalled = true
	d.deleteCtxErr = ctx.Err()
	var deadline time.Time
	deadline, d.deleteHasDeadline = ctx.Deadline()
	if d.deleteHasDeadline {
		d.deleteBudget = time.Until(deadline)
	}
	return nil
}

// newCancelableCreate builds a create request whose context the returned
// cancel func cancels; serve runs it through the handler. Tests hand cancel
// to a mock that fires it mid-handler, before the failure cleanup runs.
func newCancelableCreate(t *testing.T, srv *Server, body CreateAgentRequest) (cancel context.CancelFunc, serve func()) {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)
	reqCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(bodyBytes)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	return cancel, func() { srv.Handler().ServeHTTP(httptest.NewRecorder(), req) }
}

func assertNoReservations(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, agentID),
		"a create canceled before its failure cleanup must release the per-broker reservation")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, agentID),
		"a create canceled before its failure cleanup must release the per-project reservation")
}

func TestCreateAgent_CanceledRequest_FailureCleanupStillRuns(t *testing.T) {
	cases := []struct {
		name      string
		gatherEnv bool
		createErr error
		envReqs   *RemoteEnvRequirementsResponse
	}{
		{name: "dispatch failure", createErr: fmt.Errorf("simulated broker dispatch failure")},
		{name: "gather-env dispatch failure", gatherEnv: true, createErr: fmt.Errorf("simulated broker dispatch failure")},
		{name: "missing env vars", envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &cancelingCreateDispatcher{createErr: tc.createErr}
			disp.envReqs = tc.envReqs
			srv, s, project := setupCreateAgentServer(t, disp)

			cancel, serve := newCancelableCreate(t, srv, CreateAgentRequest{
				Name:      "canceled-create-agent",
				ProjectID: project.ID,
				Task:      "do something",
				GatherEnv: tc.gatherEnv,
			})
			disp.cancelRequest = cancel
			serve()

			require.NotNil(t, disp.capturedAgent, "dispatcher must have observed the create-time agent")
			agentID := disp.capturedAgent.ID

			require.True(t, disp.deleteCalled, "cleanup must still dispatch the runtime delete")
			assert.NoError(t, disp.deleteCtxErr, "the runtime delete must not run on the canceled request context")
			assert.True(t, disp.deleteHasDeadline, "the runtime delete must run under its own bounded budget")
			assert.Greater(t, disp.deleteBudget, dispatchDeleteTimeout,
				"the runtime delete budget must leave a cross-node delete's full deferred wait intact")

			_, err := s.GetAgent(context.Background(), agentID)
			assert.ErrorIs(t, err, store.ErrNotFound, "cleanup must delete the agent row despite the canceled request")

			assertNoReservations(t, s, agentID)
		})
	}
}

// cancelingManagedAgentBackend cancels the request context from inside
// managedAgentCreate's CreateInteraction call, then fails it.
type cancelingManagedAgentBackend struct {
	failingManagedAgentBackend
	cancelRequest context.CancelFunc
}

func (b *cancelingManagedAgentBackend) CreateInteraction(ctx context.Context, _ managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	b.cancelRequest()
	<-ctx.Done()
	return nil, fmt.Errorf("simulated managed agent create failure")
}

// Must not run in parallel: it swaps the package-level managedBackendInst.
func TestCreateAgent_CanceledRequest_ManagedFailureCleanupStillRuns(t *testing.T) {
	backend := &cancelingManagedAgentBackend{}
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
	ctx := context.Background()

	cancel, serve := newCancelableCreate(t, srv, CreateAgentRequest{
		Name:      "canceled-managed-agent",
		ProjectID: project.ID,
		Task:      "do something",
		Profile:   ManagedAgentsProfile,
	})
	backend.cancelRequest = cancel
	serve()

	result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, result.Items, "cleanup must delete the agent row despite the canceled request")

	assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID),
		"a canceled managed create must release the per-broker reservation")
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	projectReservations, err := s.CountActiveReservations(ctx, def.ID, DevUserID, store.QuotaScopeProject, project.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, projectReservations,
		"a canceled managed create must release the per-project reservation")
}
