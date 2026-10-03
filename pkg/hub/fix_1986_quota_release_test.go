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
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#1986: every branch of createAgentInProject that deletes the
// agent after the quota reservations were taken (dispatch failure, missing
// env vars, managed-agent create failure) must also release those
// reservations. Before this fix only store.CreateAgent's own failure path and
// the delete handler did; the four DeleteAgent call sites reached after a
// successful store.CreateAgent left both reservations stranded, permanently
// shrinking the broker's and the project's effective capacity.
//
// TestCreateAgent_DispatchFailure_CleansUpBroker (handlers_agent_test.go)
// covers the plain, non-GatherEnv dispatch-failure branch. The tests below
// cover the other three branches. TestProjectQuota_KeptAcrossBrokerReleases_ReleasedOnDelete
// (broker_quota_rollback_test.go) is the positive control: a successful
// create holds both reservations.

// TestCreateAgent_GatherEnvDispatchFailure_ReleasesQuota exercises the
// req.GatherEnv branch's DispatchAgentCreateWithGather failure path.
func TestCreateAgent_GatherEnvDispatchFailure_ReleasesQuota(t *testing.T) {
	disp := &failingCreateDispatcher{
		createErr: fmt.Errorf("simulated broker dispatch failure"),
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "gatherenv-fail-agent",
		ProjectID: project.ID,
		Task:      "do something",
		GatherEnv: true,
	})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

	require.NotNil(t, disp.capturedAgent, "dispatcher must have observed the create-time agent")
	_, err := s.GetAgent(ctx, disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "agent should be deleted from hub store after dispatch failure")

	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, disp.capturedAgent.ID),
		"a failed gather-env dispatch must release the per-broker reservation")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, disp.capturedAgent.ID),
		"a failed gather-env dispatch must release the per-project reservation")
}

// TestCreateAgent_MissingEnvVars_ReleasesQuota exercises the non-GatherEnv
// branch's "broker reported missing required env vars" failure, which
// responds 422 rather than 502 but still deletes the agent and must still
// release both reservations.
func TestCreateAgent_MissingEnvVars_ReleasesQuota(t *testing.T) {
	disp := &createAgentDispatcher{
		envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}},
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "missing-env-agent",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	require.NotNil(t, disp.capturedAgent, "dispatcher must have observed the create-time agent")
	_, err := s.GetAgent(ctx, disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "agent should be deleted from hub store after missing-env-vars failure")

	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, disp.capturedAgent.ID),
		"a missing-env-vars failure must release the per-broker reservation")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerProject, disp.capturedAgent.ID),
		"a missing-env-vars failure must release the per-project reservation")
}

// failingManagedAgentBackend is a managedagent.ManagedAgentBackend whose
// CreateInteraction always fails, simulating a cloud-provider error during
// managed-agent creation (handlers_managed_agents.go's managedAgentCreate).
// Swapping the package-level managedBackendInst under managedBackendMu is an
// existing test seam (see stubManagedAgentBackend in
// handlers_agent_messaging_test.go).
type failingManagedAgentBackend struct{}

func (failingManagedAgentBackend) Name() string { return "failing-managed" }

func (failingManagedAgentBackend) CreateAgent(_ context.Context, _ managedagent.CreateAgentConfig) (string, error) {
	return "", fmt.Errorf("simulated managed agent create failure")
}

func (failingManagedAgentBackend) DeleteAgent(_ context.Context, _ string) error { return nil }

func (failingManagedAgentBackend) CreateInteraction(_ context.Context, _ managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	return nil, fmt.Errorf("simulated managed agent create failure")
}

func (failingManagedAgentBackend) GetInteraction(_ context.Context, _ string) (*managedagent.InteractionState, error) {
	return nil, fmt.Errorf("not implemented")
}

func (failingManagedAgentBackend) CancelInteraction(_ context.Context, _ string) error { return nil }

func (failingManagedAgentBackend) StreamInteraction(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}

// TestCreateAgent_ManagedAgentCreateFailure_ReleasesQuota exercises the
// ManagedAgentsProfile branch, which bypasses broker dispatch entirely and
// so never touches the dispatcher mock — the agent's create-time ID has to
// be recovered from the quota reservation counters instead of the deleted
// row or an observed dispatch call.
//
// Must not run in parallel (no t.Parallel()): it swaps the package-level
// managedBackendInst var under managedBackendMu.
func TestCreateAgent_ManagedAgentCreateFailure_ReleasesQuota(t *testing.T) {
	managedBackendMu.Lock()
	prevBackend := managedBackendInst
	managedBackendInst = failingManagedAgentBackend{}
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prevBackend
		managedBackendMu.Unlock()
	})

	disp := &createAgentDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "managed-fail-agent",
		ProjectID: project.ID,
		Task:      "do something",
		Profile:   ManagedAgentsProfile,
	})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

	result, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, result.Items, "agent should be deleted from hub store after managed-agent create failure")

	assert.EqualValues(t, 0, brokerReservationCount(t, s, project.DefaultRuntimeBrokerID),
		"a failed managed-agent create must release the per-broker reservation")

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	projectReservations, err := s.CountActiveReservations(ctx, def.ID, DevUserID, store.QuotaScopeProject, project.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, projectReservations,
		"a failed managed-agent create must release the per-project reservation")
}
