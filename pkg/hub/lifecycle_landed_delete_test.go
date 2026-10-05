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
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A synchronous start or restart whose broker start lands after a delete
// won answers 409 delete_in_progress, like the mid-dispatch case, instead
// of 200 with the agent body (row delete-claimed or soft-deleted) or 404
// (row hard-deleted). The compensating delete of the landed run still runs,
// once, and its warning is carried in the 409 details (ptone/scion#3255).

const landedRunRemovedWarning = "agent was deleted while it was starting; its container was removed"

// newLandedDeleteServer returns a server whose real HTTP dispatcher talks to
// a landingClient, and a stopped agent assigned to that broker.
func newLandedDeleteServer(t *testing.T) (*Server, store.Store, *store.Agent, *landingClient) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)

	broker := &store.RuntimeBroker{
		ID:       tid("landed-del-broker-" + t.Name()),
		Name:     "landed-del-broker",
		Slug:     "landed-del-broker-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{
		ID:                     tid("landed-del-project-" + t.Name()),
		Name:                   "landed-del-project",
		Slug:                   "landed-del-project-" + tidSlugSafe(t.Name()),
		DefaultRuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: broker.Status,
	}))
	agent := &store.Agent{
		ID:              tid("landed-del-agent-" + t.Name()),
		Name:            "landed-del-agent",
		Slug:            "landed-del-agent",
		ProjectID:       project.ID,
		OwnerID:         tid("landed-del-user"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	return srv, s, agent, client
}

func TestLifecycle_DeleteWinsAfterLanding(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		for _, del := range landingDeletes {
			t.Run(action+"/"+del.name, func(t *testing.T) {
				srv, s, agent, client := newLandedDeleteServer(t)
				client.onLand = func() { del.apply(t, s, agent.ID) }

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				sent := client.lastStartExtras.RunID
				require.NotEmpty(t, sent, "the start leg reached the broker")

				if !del.compensate {
					// The agent is live: 200 with the agent body, unchanged.
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					var resp struct {
						ID    string `json:"id"`
						Phase string `json:"phase"`
					}
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					assert.Equal(t, agent.ID, resp.ID)
					assert.Equal(t, string(state.PhaseRunning), resp.Phase)
					assert.Empty(t, client.deleteRuns, "no compensating delete")
					return
				}

				requireIntentDeleteInProgress(t, rec, agent.ID)
				var body ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, deletedWhileStartingMessage, body.Error.Message)
				assert.Contains(t, body.Error.Details["warnings"], landedRunRemovedWarning,
					"the compensation warning is carried in the details")
				var raw map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
				assert.NotContains(t, raw, "id", "no agent body")
				assert.NotContains(t, raw, "agent", "no agent body")
				assert.Equal(t, []string{sent}, client.deleteRuns,
					"the landed run is deleted once, scoped to its run ID")

				// The delete's state is left alone: the phase write is
				// skipped, not merely dropped by the guard.
				if got, err := s.GetAgent(context.Background(), agent.ID); err == nil {
					assert.NotEqual(t, string(state.PhaseRunning), got.Phase)
				}
			})
		}
	}
}

// landedFaultStore is set as srv.store (the dispatcher keeps the raw store,
// so compensation reads the real row) and armed from landingClient.onLand:
// the first GetAgent after arming is then the handler's re-read, and the
// status write with ClearExit is the handler's final start/restart write.
type landedFaultStore struct {
	store.Store
	armed      atomic.Bool
	failGet    atomic.Bool // fail the handler's re-read
	delOnWrite atomic.Bool // hard-delete the row inside the final status write
}

func (p *landedFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if p.armed.Load() && p.failGet.CompareAndSwap(true, false) {
		return nil, errors.New("db unavailable")
	}
	return p.Store.GetAgent(ctx, id)
}

func (p *landedFaultStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if p.armed.Load() && u.ClearExit && p.delOnWrite.CompareAndSwap(true, false) {
		if err := p.DeleteAgent(ctx, id); err != nil {
			return err
		}
	}
	return p.Store.UpdateAgentStatus(ctx, id, u)
}

// A row hard-deleted between the handler's re-read and its status write
// answers 409 delete_in_progress too, not 404.
func TestLifecycle_DeleteWinsAfterReRead_HardDelete409(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s, agent, client := newLandedDeleteServer(t)
			// The row is live when the dispatch's compensation reads it, and
			// gone by the handler's status write.
			p := &landedFaultStore{Store: s}
			p.delOnWrite.Store(true)
			srv.store = p
			client.onLand = func() { p.armed.Store(true) }
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			requireIntentDeleteInProgress(t, rec, agent.ID)
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, deletedWhileStartingMessage, body.Error.Message)
			assert.Empty(t, client.deleteRuns, "the row was live when the dispatch checked it")
		})
	}
}

// A failed re-read with no delete answers 200 as before, with no
// compensating delete.
func TestLifecycle_ReReadFails_Answers200(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s, agent, client := newLandedDeleteServer(t)
			p := &landedFaultStore{Store: s}
			p.failGet.Store(true)
			srv.store = p
			client.onLand = func() { p.armed.Store(true) }
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Empty(t, client.deleteRuns)
		})
	}
}

// POST /agents on an existing agent (scion start / resume in hub mode,
// handleExistingAgent): each branch that starts the agent answers 409
// delete_in_progress when a delete wins after the broker start landed, like
// the lifecycle start, and 200 with the agent otherwise (ptone/scion#3255).
func TestCreateExisting_DeleteWinsAfterLanding(t *testing.T) {
	branches := []struct {
		name  string
		phase state.Phase
		body  map[string]interface{}
	}{
		{"resume-suspended", state.PhaseSuspended, nil},
		{"resume-stopped", state.PhaseStopped, map[string]interface{}{"resume": true}},
		{"force-recover", state.PhaseError, map[string]interface{}{"resume": true, "forceResume": true}},
		{"start-created", state.PhaseCreated, nil},
	}
	for _, br := range branches {
		for _, del := range landingDeletes {
			t.Run(br.name+"/"+del.name, func(t *testing.T) {
				f := handleExistingAgentAuthzSetup(t)
				agent := f.agent(t, "cx-landed", string(br.phase))
				client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
				client.onLand = func() { del.apply(t, f.store, agent.ID) }
				f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))

				req := map[string]interface{}{"name": agent.Slug, "projectId": f.project.ID}
				for k, v := range br.body {
					req[k] = v
				}
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", req)
				sent := client.lastStartExtras.RunID
				require.NotEmpty(t, sent, "the start reached the broker: %s", rec.Body.String())

				if !del.compensate {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					var resp CreateAgentResponse
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					require.NotNil(t, resp.Agent)
					assert.Equal(t, agent.ID, resp.Agent.ID)
					assert.Empty(t, client.deleteRuns, "no compensating delete")
					return
				}

				requireIntentDeleteInProgress(t, rec, agent.ID)
				var body ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, deletedWhileStartingMessage, body.Error.Message)
				assert.Contains(t, body.Error.Details["warnings"], landedRunRemovedWarning)
				var raw map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
				assert.NotContains(t, raw, "agent", "no agent body")
				assert.Equal(t, []string{sent}, client.deleteRuns,
					"the landed run is deleted once, scoped to its run ID")
			})
		}
	}
}

// deleteOnUpdateStore hard-deletes the row inside the first UpdateAgent
// after it is armed: for handleExistingAgent, the post-start agent update.
type deleteOnUpdateStore struct {
	store.Store
	armed atomic.Bool
}

func (p *deleteOnUpdateStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if p.armed.CompareAndSwap(true, false) {
		if err := p.DeleteAgent(ctx, a.ID); err != nil {
			return err
		}
	}
	return p.Store.UpdateAgent(ctx, a)
}

// A row hard-deleted between handleExistingAgent's re-read and its agent
// update answers 409 delete_in_progress, not 200 (existingAgentGoneAfterLanding).
func TestCreateExisting_DeleteWinsAfterReRead_HardDelete409(t *testing.T) {
	branches := []struct {
		name  string
		phase state.Phase
		body  map[string]interface{}
	}{
		{"resume-suspended", state.PhaseSuspended, nil},
		{"resume-stopped", state.PhaseStopped, map[string]interface{}{"resume": true}},
		{"start-created", state.PhaseCreated, nil},
	}
	for _, br := range branches {
		t.Run(br.name, func(t *testing.T) {
			f := handleExistingAgentAuthzSetup(t)
			agent := f.agent(t, "cx-reread", string(br.phase))
			client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
			f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))
			p := &deleteOnUpdateStore{Store: f.store}
			f.srv.store = p // the dispatcher keeps the raw store
			client.onLand = func() { p.armed.Store(true) }

			req := map[string]interface{}{"name": agent.Slug, "projectId": f.project.ID}
			for k, v := range br.body {
				req[k] = v
			}
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", req)
			require.NotEmpty(t, client.lastStartExtras.RunID, "the start reached the broker: %s", rec.Body.String())
			requireIntentDeleteInProgress(t, rec, agent.ID)
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, deletedWhileStartingMessage, body.Error.Message)
			assert.Empty(t, client.deleteRuns, "the row was live when the dispatch checked it")
		})
	}
}
