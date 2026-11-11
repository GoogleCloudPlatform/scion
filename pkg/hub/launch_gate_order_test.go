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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The start gate checks a pending delete before the create launch: an agent
// that is both being deleted and has an incomplete or in-flight create gets
// 409 delete_in_progress on every start path, not agent_create_incomplete,
// agent_launching or a 200 launching answer.

// gateOrderDelete is a way to make a delete block start.
type gateOrderDelete struct {
	name string
	seed func(t *testing.T, s store.Store, agentID string)
}

var gateOrderDeletes = []gateOrderDelete{
	{"pending delete intent", func(t *testing.T, s store.Store, agentID string) {
		// The intent alone, as before the delete claim moves the phase.
		require.NoError(t, s.InsertBrokerDispatch(context.Background(), &store.BrokerDispatch{
			ID: uuid.NewString(), BrokerID: uuid.NewString(), AgentID: agentID, Op: brokerDispatchOpDelete,
		}))
	}},
	{"live delete", func(t *testing.T, s store.Store, agentID string) {
		seedAgentDeletion(t, s, agentID, seedLiveDeleting)
	}},
}

var gateOrderLaunches = []struct {
	name string
	seed launchSeed
}{
	{"incomplete create", seedIncompleteActive},
	{"in flight", seedInFlight},
}

func TestStartGate_DeleteBeforeLaunch(t *testing.T) {
	for li, launch := range gateOrderLaunches {
		for di, del := range gateOrderDeletes {
			suffix := string(rune('a'+li)) + string(rune('a'+di))
			name := launch.name + "/" + del.name

			for _, action := range []string{"start", "restart"} {
				t.Run(name+"/"+action, func(t *testing.T) {
					srv, s := testServer(t)
					agent := setupBrokerAgentInPhase(t, s, "gord-"+action+"-"+suffix, state.PhaseCreated)
					seedLaunch(t, s, agent, launch.seed)
					del.seed(t, s, agent.ID)
					client := &mockRuntimeBrokerClient{}
					srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

					rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
					requireDeleteInProgress(t, rec)
					assert.False(t, client.stopCalled)
					assert.False(t, client.startCalled)
				})
			}

			t.Run(name+"/create-existing resume", func(t *testing.T) {
				f := handleExistingAgentAuthzSetup(t)
				disp := &createAgentDispatcher{}
				f.srv.SetDispatcher(disp)
				agent := f.agent(t, "gord-hea-"+suffix, string(state.PhaseCreated))
				seedLaunch(t, f.store, agent, launch.seed)
				del.seed(t, f.store, agent.ID)

				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
					"name": agent.Slug, "projectId": f.project.ID, "resume": true,
				})
				requireDeleteInProgress(t, rec)
				assert.False(t, disp.startCalled)
				assert.False(t, disp.deleteCalled)
			})

			t.Run(name+"/reincarnate", func(t *testing.T) {
				disp := newReincarnateTestDispatcher()
				srv, s, project, broker := setupReincarnateTestServer(t, disp)
				agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) { a.Phase = string(state.PhaseCreated) })
				seedLaunch(t, s, agent, launch.seed)
				del.seed(t, s, agent.ID)

				req := reincarnateRequest(t, agent.ID, agentIdentityFor(agent.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"})
				rec := httptest.NewRecorder()
				srv.handleReincarnateAgent(rec, req, agent.ID)
				requireDeleteInProgress(t, rec)
				assert.Equal(t, store.ReincarnationStateNone, mustAgent(t, s, agent.ID).ReincarnationState)
			})
		}
	}

	// DM wake runs the gate in every phase when a launch refusal applies:
	// an in-flight create (created/provisioning) and, for a suspended agent,
	// an incomplete create. The delete answer wins in both.
	for di, del := range gateOrderDeletes {
		t.Run("in flight/"+del.name+"/DM wake", func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "gord-wakef-"+string(rune('a'+di)), state.PhaseCreated)
			seedLaunch(t, s, agent, seedInFlight)
			del.seed(t, s, agent.ID)
			agent = mustAgent(t, s, agent.ID)
			require.True(t, agent.IsInFlight())
			client := &mockRuntimeBrokerClient{}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

			res, dmErr := srv.wakeAgentForDM(context.Background(), agent)
			assert.Nil(t, res)
			require.NotNil(t, dmErr)
			assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
			assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
			assert.False(t, client.startCalled)
		})
	}
	for di, del := range gateOrderDeletes {
		t.Run("incomplete create/"+del.name+"/DM wake", func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "gord-wake-"+string(rune('a'+di)), state.PhaseCreated)
			seedLaunch(t, s, agent, seedInFlight)
			require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))
			del.seed(t, s, agent.ID)
			agent = mustAgent(t, s, agent.ID)
			require.True(t, agent.IsIncompleteCreate())
			client := &mockRuntimeBrokerClient{}
			srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

			res, dmErr := srv.wakeAgentForDM(context.Background(), agent)
			assert.Nil(t, res)
			require.NotNil(t, dmErr)
			assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
			assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
			assert.False(t, client.startCalled)
		})
	}
}
