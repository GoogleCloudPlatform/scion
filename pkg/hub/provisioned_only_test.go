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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provisionedOnlyFrom decodes the provisionedOnly field from an agent JSON
// object (absent decodes as false, matching omitempty).
func provisionedOnlyFrom(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var v struct {
		ProvisionedOnly bool `json:"provisionedOnly"`
	}
	require.NoError(t, json.Unmarshal(raw, &v))
	return v.ProvisionedOnly
}

// getProvisionedOnly reads provisionedOnly for agentID from both the get
// and the list endpoints, failing if they disagree.
func getProvisionedOnly(t *testing.T, srv *Server, projectID, agentID string) bool {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := provisionedOnlyFrom(t, rec.Body.Bytes())

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents?projectId="+projectID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Agents []json.RawMessage `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	found := false
	for _, raw := range list.Agents {
		var id struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(raw, &id))
		if id.ID == agentID {
			found = true
			assert.Equal(t, got, provisionedOnlyFrom(t, raw), "list and get disagree")
		}
	}
	require.True(t, found, "agent %s missing from list", agentID)
	return got
}

// A provision-only create leaves the agent in phase created with run
// intent stopped: the create response, get and list all report it as
// provisioned but not started (ptone/scion#2929).
func TestProvisionedOnly_ProvisionOnlyCreate(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "po-provision", ProjectID: project.ID, ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp struct {
		Agent json.RawMessage `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, provisionedOnlyFrom(t, resp.Agent), "create response")

	var a struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(resp.Agent, &a))
	assert.True(t, getProvisionedOnly(t, srv, project.ID, a.ID), "get/list")
}

// A full create that has not left phase created yet has run intent
// running: it is starting, not provision-only. This case fails if the
// run-intent condition is dropped.
func TestProvisionedOnly_FullCreateStillCreated(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s
	agent := createSiteAgent(t, s, project, "po-full", state.PhaseCreated, store.RunIntentRunning)

	assert.False(t, getProvisionedOnly(t, srv, project.ID, agent.ID))
}

// Once started, the agent is no longer provision-only.
func TestProvisionedOnly_StartedAgent(t *testing.T) {
	disp := newSiteIntentDispatcher(nil)
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.s = s
	agent := createSiteAgent(t, s, project, "po-running", state.PhaseRunning, store.RunIntentRunning)

	assert.False(t, getProvisionedOnly(t, srv, project.ID, agent.ID))
}

// enrichAgent and enrichAgents compute the same view; an active launch or
// a delete in progress clears it.
func TestProvisionedOnly_Enrich(t *testing.T) {
	srv, _ := testServer(t)
	ctx := context.Background()
	project := &store.Project{Name: "p"}
	broker := &store.RuntimeBroker{Name: "b"}

	base := func() store.Agent {
		return store.Agent{
			ID:        "a1",
			Phase:     string(state.PhaseCreated),
			RunIntent: store.RunIntentStopped,
		}
	}
	cases := []struct {
		name   string
		mutate func(a *store.Agent)
		want   bool
	}{
		{name: "provision-only", mutate: func(*store.Agent) {}, want: true},
		{name: "run-intent-running", mutate: func(a *store.Agent) { a.RunIntent = store.RunIntentRunning }, want: false},
		{name: "run-intent-unknown", mutate: func(a *store.Agent) { a.RunIntent = "" }, want: false},
		{name: "active-launch", mutate: func(a *store.Agent) {
			a.LaunchID, a.LaunchState, a.LaunchKind = "l1", store.LaunchStateActive, "start"
		}, want: false},
		{name: "deleting", mutate: func(a *store.Agent) { a.DeletionState = "deleting" }, want: false},
		{name: "stopped-phase", mutate: func(a *store.Agent) { a.Phase = string(state.PhaseStopped) }, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			one := base()
			tc.mutate(&one)
			srv.enrichAgent(ctx, &one, project, broker)
			assert.Equal(t, tc.want, one.ProvisionedOnly, "enrichAgent")

			many := []store.Agent{base()}
			tc.mutate(&many[0])
			srv.enrichAgents(ctx, many)
			assert.Equal(t, tc.want, many[0].ProvisionedOnly, "enrichAgents")
		})
	}
}
