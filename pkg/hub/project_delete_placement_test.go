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

// The project delete refusal flags the agents an agent delete would refuse
// for an inconsistent saved placement (ptone/scion#4960).

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectDeleteRefusalDetails decodes a 409 project delete refusal's
// message and raw details.
func projectDeleteRefusalDetails(t *testing.T, body []byte) (string, map[string]interface{}) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	assert.Equal(t, ErrCodeConflict, resp.Error.Code)
	return resp.Error.Message, resp.Error.Details
}

// placementFlags maps each listed agent's ID to its placementRefusal ("" if
// the key is absent).
func placementFlags(t *testing.T, details map[string]interface{}) map[string]string {
	t.Helper()
	raw, ok := details["agents"].([]interface{})
	require.True(t, ok, "details.agents: %v", details)
	out := map[string]string{}
	for _, r := range raw {
		a := r.(map[string]interface{})
		code, _ := a["placementRefusal"].(string)
		out[a["id"].(string)] = code
	}
	return out
}

// Only the agent whose saved placement is inconsistent is flagged, with
// the code an agent delete answers; the reason and the docs link say why.
func TestProjectDelete_RefusalFlagsPlacementRefusedAgents(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	ok := f.pinnedAgent(t, "pd-consistent", string(state.PhaseRunning))
	stale := f.stalePinnedAgent(t, "pd-stale", string(state.PhaseRunning))

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/projects/"+f.project.ID, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	msg, details := projectDeleteRefusalDetails(t, rec.Body.Bytes())
	flags := placementFlags(t, details)
	require.Len(t, flags, 2)
	assert.Equal(t, "", flags[ok.ID], "a consistent agent is not flagged")
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, flags[stale.ID])
	assert.Equal(t, inconsistentPlacementDocsURL, details["placementDocsUrl"])
	assert.Contains(t, msg, "delete the agents first")
	assert.Contains(t, msg, "cannot be deleted individually")

	// The agent delete refuses the flagged agent with the same code.
	del := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+stale.ID, nil)
	requireAPIError(t, del, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
}

// With no inconsistent agent, the refusal carries no flag, no docs link
// and the unchanged message.
func TestProjectDelete_RefusalWithoutPlacementRefusedAgentsUnchanged(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	f.pinnedAgent(t, "pd-only-consistent", string(state.PhaseRunning))

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/projects/"+f.project.ID, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	msg, details := projectDeleteRefusalDetails(t, rec.Body.Bytes())
	assert.NotContains(t, rec.Body.String(), "placementRefusal")
	assert.NotContains(t, details, "placementDocsUrl")
	assert.Equal(t, projectHasAgentsDeleteMessage, msg)
}

// A Runtime Broker row read that fails leaves the agent unflagged; the
// refusal itself still lists it.
func TestProjectDelete_PlacementCheckFailureLeavesAgentUnflagged(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	stale := f.stalePinnedAgent(t, "pd-stale-fault", string(state.PhaseRunning))

	faulty := &brokerReadErrStore{Store: f.s, err: errors.New("broker read failed")}
	err := checkProjectAgentsDeletedTx(context.Background(), faulty, f.project.ID)
	var refused *projectHasAgentsDeleteError
	require.ErrorAs(t, err, &refused)
	require.Len(t, refused.agents, 1)
	assert.Equal(t, stale.ID, refused.agents[0].ID)
	assert.Empty(t, refused.agents[0].PlacementRefusal)
	assert.False(t, refused.placementRefused())

	// The same agent with the row readable is flagged.
	err = checkProjectAgentsDeletedTx(context.Background(), f.s, f.project.ID)
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, refused.agents[0].PlacementRefusal)
}

// --- project delete dispatch log (ptone/scion#4964) --------------------------

const projectDeletePlacementLogMsg = "project deletion: agent delete refused for an inconsistent placement; its workload is left running on the Runtime Broker"

// The project delete's broker dispatch sends nothing for an agent whose
// saved placement is inconsistent and logs one error naming the agent, its
// Runtime Broker and its pin: for an agent the dispatcher refuses, and for
// a pinned agent with no Runtime Broker, which is never dispatched. An
// unpinned agent with no Runtime Broker logs no such error.
func TestDispatchAgentDeletions_PlacementRefusalLogged(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		agent    func(f *flatHubFixture) *store.Agent
		wantLog  bool
		wantCode string
	}{
		{
			name: "dispatcher refuses a stale pin",
			agent: func(f *flatHubFixture) *store.Agent {
				return f.stalePinnedAgent(t, "pd-log-stale", string(state.PhaseRunning))
			},
			wantLog:  true,
			wantCode: ErrCodeRuntimeTargetPinStale,
		},
		{
			name: "pinned agent with no Runtime Broker",
			agent: func(f *flatHubFixture) *store.Agent {
				a := f.pinnedAgent(t, "pd-log-empty", string(state.PhaseRunning))
				a.RuntimeBrokerID = ""
				require.NoError(t, f.s.UpdateAgent(ctx, a))
				got := mustGetAgent(t, f.s, a.ID)
				require.True(t, got.IsPinned())
				return got
			},
			wantLog:  true,
			wantCode: ErrCodeRuntimeTargetPinStale,
		},
		{
			name: "unpinned agent with no Runtime Broker",
			agent: func(f *flatHubFixture) *store.Agent {
				return f.unpinnedAgentOn(t, "pd-log-unpinned", "", string(state.PhaseCreated))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
			a := tc.agent(f)
			logs := &levelLogHandler{}
			f.srv.agentLifecycleLog = slog.New(logs)

			f.srv.dispatchAgentDeletions(ctx, []store.Agent{*a})

			assert.False(t, f.client.deleteCalled, "nothing is sent")
			recs := logs.named(projectDeletePlacementLogMsg)
			if !tc.wantLog {
				assert.Empty(t, recs)
				return
			}
			require.Len(t, recs, 1)
			r := recs[0]
			assert.Equal(t, slog.LevelError, r.level)
			assert.Equal(t, a.ID, r.attrs["agent_id"])
			assert.Equal(t, a.Slug, r.attrs["agent"])
			assert.Equal(t, a.ProjectID, r.attrs["project_id"])
			assert.Equal(t, tc.wantCode, r.attrs["code"])
			assert.Equal(t, a.RuntimeBrokerID, r.attrs["broker"])
			assert.Equal(t, a.PinnedRuntimeBrokerID, r.attrs["pinned_runtime_broker_id"])
			assert.Equal(t, a.PinnedRuntimeTargetID, r.attrs["pinned_runtime_target_id"])
			assert.NotEmpty(t, r.attrs["pinned_runtime_target_id"])
		})
	}
}
