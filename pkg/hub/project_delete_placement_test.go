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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
