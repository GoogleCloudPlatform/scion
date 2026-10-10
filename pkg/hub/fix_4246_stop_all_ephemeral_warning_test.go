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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4246: stop-all runs the same pre-stop ephemeral-workspace
// check as a single stop, and reports its warning on the agent's result.
// An agent with a persistent workspace is left as before: no exec, no
// warning, no record.

func stopAllResultsByID(t *testing.T, body []byte) map[string]stopAllResult {
	t.Helper()
	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	out := make(map[string]stopAllResult, len(resp.Results))
	for _, r := range resp.Results {
		out[r.ID] = r
	}
	return out
}

func TestStopAll_EphemeralWorkspaceWarns(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	eph := newWorkspaceAgent(t, s, broker, project, "ws-stopall-eph", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)
	persistent := newWorkspaceAgent(t, s, broker, project, "ws-stopall-docker", "docker", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results := stopAllResultsByID(t, rec.Body.Bytes())

	require.Contains(t, results, eph.ID, rec.Body.String())
	assert.Equal(t, "stopped", results[eph.ID].Status)
	assert.Equal(t, []string{stopWarning23}, results[eph.ID].Warnings, "the warning a single stop answers with")
	assert.JSONEq(t, `{"commits":2,"files":3}`, workspaceAnnotation(t, s, eph.ID))

	require.Contains(t, results, persistent.ID, rec.Body.String())
	assert.Equal(t, "stopped", results[persistent.ID].Status)
	assert.Empty(t, results[persistent.ID].Warnings)
	assert.Empty(t, workspaceAnnotation(t, s, persistent.ID))

	assert.Equal(t, 1, disp.calls(), "the check runs only for the ephemeral workspace")
}

// As for a single stop, a failed stop dispatch reports no discard warning
// and removes the record the check wrote.
func TestStopAll_EphemeralWorkspaceFailedStopClearsRecord(t *testing.T) {
	disp := &workspaceCheckDispatcher{execOutput: workAt23, stopErr: errors.New("broker failed the stop")}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	eph := newWorkspaceAgent(t, s, broker, project, "ws-stopall-fail", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results := stopAllResultsByID(t, rec.Body.Bytes())
	require.Contains(t, results, eph.ID, rec.Body.String())
	assert.Equal(t, "error", results[eph.ID].Status)
	assert.Empty(t, results[eph.ID].Warnings)
	assert.Equal(t, 1, disp.calls(), "the check ran before the failed dispatch")
	assert.Empty(t, workspaceAnnotation(t, s, eph.ID), "a failed stop leaves no record")
}
