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
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedDirChangeDispatcher records the shared dir backend change on the
// config each reprovision is dispatched with.
type sharedDirChangeDispatcher struct {
	*reincarnateTestDispatcher
	mu         sync.Mutex
	changes    map[string]string
	allowEmpty bool
}

func (d *sharedDirChangeDispatcher) DispatchAgentReprovision(ctx context.Context, agent *store.Agent) error {
	d.mu.Lock()
	if agent.AppliedConfig != nil {
		d.changes = agent.AppliedConfig.SharedDirBackendChanges
		d.allowEmpty = agent.AppliedConfig.AllowEmptySharedDir
	}
	d.mu.Unlock()
	return d.reincarnateTestDispatcher.DispatchAgentReprovision(ctx, agent)
}

func sessionAdminFor(t *testing.T, s store.Store, project *store.Project, suffix string) Identity {
	t.Helper()
	user := newReincarnateAuthzUser(t, s, suffix)
	grantProjectRole(t, s, user.ID, project.ID, store.ProjectRoleAdmin)
	return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
}

func TestReincarnateAgent_SharedDirBackends_DryRunShowsChange(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	identity := sessionAdminFor(t, s, project, "sd-dry")

	req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{
		DryRun: true, SharedDirBackends: map[string]string{"notes": "nfs"}, AllowEmptySharedDir: true,
	})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, map[string]string{"notes": "nfs"}, resp.Plan.SharedDirBackends)
	assert.True(t, resp.Plan.AllowEmptySharedDir)
	assert.Zero(t, disp.reprovisionCalls)
}

func TestReincarnateAgent_SharedDirBackends_InvalidRequests(t *testing.T) {
	for name, body := range map[string]ReincarnateAgentRequest{
		"to local":         {SharedDirBackends: map[string]string{"notes": "local"}},
		"invalid name":     {SharedDirBackends: map[string]string{"Bad_Name": "nfs"}},
		"allow empty only": {AllowEmptySharedDir: true},
	} {
		t.Run(name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			identity := sessionAdminFor(t, s, project, "sd-bad")
			before := agent.StateVersion

			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, identity, body), agent.ID)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			after, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after.StateVersion)
		})
	}
}

// An agent cannot change its own shared dir backend, even though it may
// otherwise reincarnate itself.
func TestReincarnateAgent_SharedDirBackends_SelfRefused(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{
		DryRun: true, SharedDirBackends: map[string]string{"notes": "nfs"},
	}), agent.ID)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true}), agent.ID)
	assert.Equal(t, http.StatusOK, rec.Code, "a plain self dry run is still allowed: %s", rec.Body.String())
}

// A real reincarnation sends the change on the reprovision, and the next
// reincarnation does not carry it forward.
func TestReincarnateAgent_SharedDirBackends_SentOnReprovisionOnly(t *testing.T) {
	disp := &sharedDirChangeDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	identity := sessionAdminFor(t, s, project, "sd-real")

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{
		Handoff: "h", SharedDirBackends: map[string]string{"notes": "nfs"},
	}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	disp.mu.Lock()
	assert.Equal(t, map[string]string{"notes": "nfs"}, disp.changes)
	assert.False(t, disp.allowEmpty)
	disp.mu.Unlock()

	rec = httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{Handoff: "h2"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	// The new record is created before the 202, so this waits for it.
	r = waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	require.Equal(t, 3, r.ToGeneration)
	disp.mu.Lock()
	assert.Nil(t, disp.changes, "a later reincarnation does not repeat the change")
	disp.mu.Unlock()
}

func reprovisionDispatchFixture(t *testing.T, echo bool) (*HTTPAgentDispatcher, *store.Agent, *RemoteCreateAgentRequest) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID: tid("host-1"), Name: "test-host", Slug: "test-host",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))
	agent := &store.Agent{
		ID: tid("agent-1"), Name: "test-agent", Slug: "test-agent",
		ProjectID: tid("project-1"), RuntimeBrokerID: tid("host-1"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig:           "claude",
			SharedDirBackendChanges: map[string]string{"notes": "nfs"},
			AllowEmptySharedDir:     true,
		},
	}
	captured := &RemoteCreateAgentRequest{}
	mockClient := &mockRuntimeBrokerClient{
		createWithGatherFunc: func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			*captured = *req
			return &RemoteAgentResponse{
				Agent:                    &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name},
				Created:                  true,
				Reprovisioned:            req.Reprovision,
				SharedDirBackendsChanged: echo && len(req.SharedDirBackendChanges) > 0,
			}, nil, nil
		},
	}
	return NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default()), agent, captured
}

func TestHTTPAgentDispatcher_ReprovisionCarriesSharedDirBackendChange(t *testing.T) {
	d, agent, captured := reprovisionDispatchFixture(t, true)
	require.NoError(t, d.DispatchAgentReprovision(context.Background(), agent))
	assert.True(t, captured.Reprovision)
	assert.Equal(t, map[string]string{"notes": "nfs"}, captured.SharedDirBackendChanges)
	assert.True(t, captured.AllowEmptySharedDir)
}

// A broker that does not confirm the change (one that predates it) fails
// the reprovision.
func TestHTTPAgentDispatcher_ReprovisionSharedDirChangeMissingEchoFails(t *testing.T) {
	d, agent, _ := reprovisionDispatchFixture(t, false)
	err := d.DispatchAgentReprovision(context.Background(), agent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not confirm the shared dir backend change")
}

// A plain provision never sends the change.
func TestHTTPAgentDispatcher_ProvisionOmitsSharedDirBackendChange(t *testing.T) {
	d, agent, captured := reprovisionDispatchFixture(t, true)
	require.NoError(t, d.DispatchAgentProvision(context.Background(), agent))
	assert.False(t, captured.Reprovision)
	assert.Nil(t, captured.SharedDirBackendChanges)
	assert.False(t, captured.AllowEmptySharedDir)
}

// A shared dir backend change cannot be combined with a move to another
// broker; a dry run is refused with 400 and nothing is written.
func TestReincarnateAgent_SharedDirBackends_WithMoveRefused(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)
	identity := sessionAdminFor(t, f.s, f.project, "sd-move")
	for name, body := range map[string]ReincarnateAgentRequest{
		"change":      {DryRun: true, TargetBroker: f.dst.ID, SharedDirBackends: map[string]string{"notes": "nfs"}},
		"allow empty": {DryRun: true, TargetBroker: f.dst.ID, SharedDirBackends: map[string]string{"notes": "nfs"}, AllowEmptySharedDir: true},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.agent.ID, identity, body), f.agent.ID)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "cannot be combined with a move")
			f.assertNoMoveSideEffects(t, count)
		})
	}

	// The same change with the agent's current broker as the target is a
	// plain reincarnation and is accepted.
	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.agent.ID, identity, ReincarnateAgentRequest{
		DryRun: true, TargetBroker: f.src.ID, SharedDirBackends: map[string]string{"notes": "nfs"},
	}), f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
