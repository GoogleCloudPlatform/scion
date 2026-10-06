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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A synchronous create whose agent is deleted, or held by a delete, while
// the broker create is in flight answers 409 delete_in_progress, not 201
// (ptone/scion#3099). A failed or lapsed delete leaves the agent live, so
// the create still answers 201 (the deletedOrDeleteHeld rule).

// requireDeletedDuringCreate checks rec is the 409 delete_in_progress answer
// for agentID, with no agent body, and returns its details.warnings.
func requireDeletedDuringCreate(t *testing.T, rec *httptest.ResponseRecorder, agentID string) []string {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "agent", "the 409 carries no agent body")
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeDeleteInProgress, body.Error.Code)
	assert.Equal(t, deletedDuringCreateMessage, body.Error.Message)
	assert.Equal(t, agentID, body.Error.Details["agentId"])
	var warnings []string
	if ws, ok := body.Error.Details["warnings"].([]interface{}); ok {
		for _, w := range ws {
			warnings = append(warnings, w.(string))
		}
	}
	return warnings
}

// Every way a delete can hold the row when the broker create answers
// (landingDeletes): a delete that holds the row or removed it answers 409,
// after exactly one compensating delete scoped to the run that landed and
// with no created; a failed or expired delete, or none, answers 201 with the
// agent.
func TestSyncCreate_DeletedDuringCreate_Answers409(t *testing.T) {
	for i, del := range landingDeletes {
		t.Run(del.name, func(t *testing.T) {
			srv, s, project, client, broker := newRunBrokerServer(t)
			pub := recordCreatedEvents(t, srv)

			var sent *RemoteCreateAgentRequest
			client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				sent = req
				require.False(t, req.AsyncLaunch, "the create is dispatched synchronously")
				del.apply(t, s, req.ID)
				broker.land(req.RunID)
				return syncRunningAnswer(req), nil, nil
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
				"name": "gone-" + string(rune('a'+i)), "projectId": project.ID, "task": "do it",
			})
			require.NotNil(t, sent, "dispatch ran: %d %s", rec.Code, rec.Body.String())

			if !del.compensate {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				assert.Equal(t, sent.ID, resp.Agent.ID, "the agent body is returned")
				assert.Empty(t, broker.deletes, "no compensating delete")
				assert.Equal(t, 1, pub.count("created"), "created is published: %v", pub.kinds())
				return
			}

			warnings := requireDeletedDuringCreate(t, rec, sent.ID)
			assert.Contains(t, warnings, "agent was deleted while it was starting; its container was removed",
				"the compensating delete's outcome is reported")
			assert.Equal(t, []string{sent.RunID}, broker.deletes,
				"exactly one broker delete, scoped to the run that landed")
			assert.False(t, broker.has(sent.RunID), "the landed run is removed")
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
		})
	}
}

// The same rule on the env-gather branch: a gatherEnv create whose broker
// asks for env (202) answers 409 when a delete holds or removed the row by
// then, and 202 with the agent and the env requirements when there is no
// delete or it failed or expired (the agent is live). Nothing ran on the
// broker, so there is no compensating delete either way.
func TestSyncCreate_EnvGather_DeletedDuringCreate_Answers409(t *testing.T) {
	for i, del := range landingDeletes {
		t.Run(del.name, func(t *testing.T) {
			srv, s, project, client, broker := newRunBrokerServer(t)
			pub := recordCreatedEvents(t, srv)

			var sent *RemoteCreateAgentRequest
			client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				sent = req
				require.False(t, req.AsyncLaunch, "the create is dispatched synchronously")
				del.apply(t, s, req.ID)
				return nil, &RemoteEnvRequirementsResponse{Required: []string{"API_KEY"}, Needs: []string{"API_KEY"}}, nil
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
				"name": "gather-" + string(rune('a'+i)), "projectId": project.ID, "task": "do it", "gatherEnv": true,
			})
			require.NotNil(t, sent, "dispatch ran: %d %s", rec.Code, rec.Body.String())
			assert.Empty(t, broker.deletes, "nothing ran, so no compensating delete")

			if !del.compensate {
				require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				assert.Equal(t, sent.ID, resp.Agent.ID, "the agent body is returned")
				assert.NotNil(t, resp.EnvGather, "the env requirements are returned")
				assert.Equal(t, 1, pub.count("created"), "created is published: %v", pub.kinds())
				return
			}

			requireDeletedDuringCreate(t, rec, sent.ID)
			assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
		})
	}
}

// Provision-only (what `scion create` sends to the hub) is a synchronous
// create too: a delete that finished while the broker provisioned answers
// 409. Nothing ran, so there is no compensating delete.
func TestSyncCreate_ProvisionOnly_DeletedDuringCreate_Answers409(t *testing.T) {
	srv, s, project, client, _ := newRunBrokerServer(t)
	pub := recordCreatedEvents(t, srv)

	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+req.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		return &RemoteAgentResponse{
			Agent:   &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseCreated)},
			Created: true,
		}, nil, nil
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "gone-provision", "projectId": project.ID, "provisionOnly": true,
	})
	require.NotNil(t, sent, "dispatch ran: %d %s", rec.Code, rec.Body.String())
	require.True(t, sent.ProvisionOnly, "the broker was asked to provision only")
	requireDeletedDuringCreate(t, rec, sent.ID)
	assert.NotContains(t, rec.Body.String(), "Failed to update agent phase", "no stale phase-write warning")
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
	assert.True(t, agentGone(t, s, sent.ID))
}

// Control: provision-only with no delete still answers 201 with the agent.
func TestSyncCreate_ProvisionOnly_NoDelete_Answers201(t *testing.T) {
	srv, _, project, client, _ := newRunBrokerServer(t)
	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		return &RemoteAgentResponse{
			Agent:   &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseCreated)},
			Created: true,
		}, nil, nil
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "live-provision", "projectId": project.ID, "provisionOnly": true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, sent.ID, resp.Agent.ID)
}

// Provision-only with a delete that holds the row but has not finished
// (a live deleting lease) when the broker provision answers: 409 too.
func TestSyncCreate_ProvisionOnly_DeleteClaimed_Answers409(t *testing.T) {
	srv, s, project, client, _ := newRunBrokerServer(t)
	pub := recordCreatedEvents(t, srv)

	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		claimForTest(t, s, req.ID, store.DeletionStateDeleting, time.Minute)
		return &RemoteAgentResponse{
			Agent:   &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: string(state.PhaseCreated)},
			Created: true,
		}, nil, nil
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "held-provision", "projectId": project.ID, "provisionOnly": true,
	})
	require.NotNil(t, sent, "dispatch ran: %d %s", rec.Code, rec.Body.String())
	requireDeletedDuringCreate(t, rec, sent.ID)
	assert.Zero(t, pub.count("created"), "no created: %v", pub.kinds())
	assert.Equal(t, store.DeletionStateDeleting, mustGetAgent(t, s, sent.ID).DeletionState, "the claim is untouched")
}

// The hub's re-reads fail after the broker answered (a database outage):
// the create cannot tell whether a delete won, so it treats the agent as
// live, as the created publish does, and answers 201 with the agent. A
// transient store error must not tell a client a live agent is gone.
func TestSyncCreate_ReReadFails_Answers201(t *testing.T) {
	srv, s, project, client, broker := newRunBrokerServer(t)
	pub := recordCreatedEvents(t, srv)
	fs := &failingGetStore{Store: s}
	srv.store = fs

	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		broker.land(req.RunID)
		// The handler's store fails from here on. That fails two handler
		// reads: preserveTerminalPhase's (which falls back to leaving the
		// phase alone on error) and the publish re-read that decides the
		// answer. The dispatcher's own store (used by the compensation
		// check) is unaffected.
		fs.fail = true
		return syncRunningAnswer(req), nil, nil
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "reread-fails", "projectId": project.ID, "task": "do it",
	})
	require.NotNil(t, sent, "dispatch ran: %d %s", rec.Code, rec.Body.String())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, sent.ID, resp.Agent.ID, "the agent body is returned")
	assert.Equal(t, 1, pub.count("created"), "created is published: %v", pub.kinds())
	assert.Empty(t, broker.deletes, "no compensating delete")
}
