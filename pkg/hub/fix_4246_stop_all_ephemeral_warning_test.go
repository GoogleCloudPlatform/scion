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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

// stopAllCheckDispatcher answers the pre-stop workspace check per agent
// (outputs, by agent ID) or, with block, waits for the exec ctx to end. It
// records each exec's ctx error.
type stopAllCheckDispatcher struct {
	quotaLifecycleDispatcher
	outputs map[string]string
	block   bool

	mu       sync.Mutex
	execErrs map[string]error
}

func (d *stopAllCheckDispatcher) DispatchAgentExec(ctx context.Context, agent *store.Agent, _ []string, _ int) (string, int, error) {
	if d.block {
		<-ctx.Done()
		d.mu.Lock()
		d.execErrs[agent.ID] = ctx.Err()
		d.mu.Unlock()
		return "", 0, ctx.Err()
	}
	d.mu.Lock()
	d.execErrs[agent.ID] = nil
	d.mu.Unlock()
	return d.outputs[agent.ID], 0, nil
}

func newStopAllCheckServer(t *testing.T, disp *stopAllCheckDispatcher, suffix string) (*Server, store.Store, *store.RuntimeBroker, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetDispatcher(disp)
	broker, project := newQuotaTestBrokerAndProject(t, s, suffix)
	return srv, s, broker, project
}

// Two ephemeral agents whose checks find different work each get their
// own warning and record, not the other's.
func TestStopAll_EphemeralWorkspacePerAgentAttribution(t *testing.T) {
	disp := &stopAllCheckDispatcher{outputs: map[string]string{}, execErrs: map[string]error{}}
	srv, s, broker, project := newStopAllCheckServer(t, disp, "stopall-attr")
	a := newWorkspaceAgent(t, s, broker, project, "ws-stopall-a", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)
	b := newWorkspaceAgent(t, s, broker, project, "ws-stopall-b", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)
	disp.outputs[a.ID] = workAt23
	disp.outputs[b.ID] = "scion-workspace-check commits=1 files=0\n"

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results := stopAllResultsByID(t, rec.Body.Bytes())

	require.Contains(t, results, a.ID, rec.Body.String())
	require.Contains(t, results, b.ID, rec.Body.String())
	assert.Equal(t, []string{stopWarning23}, results[a.ID].Warnings)
	assert.Equal(t, []string{ephemeralWorkspaceStopWarning(ephemeralWorkspaceRecord{Commits: 1})}, results[b.ID].Warnings)
	assert.JSONEq(t, `{"commits":2,"files":3}`, workspaceAnnotation(t, s, a.ID))
	assert.JSONEq(t, `{"commits":1}`, workspaceAnnotation(t, s, b.ID))
}

// The check runs under stop-all's shared per-agent deadline: with a
// stopAllAgentOpTimeout far shorter than the check's own bound, a slow exec
// is cut at the stop-all deadline, recorded as unchecked, gives no warning,
// and the stop goes on.
func TestStopAll_EphemeralWorkspaceCheckUnderSharedDeadline(t *testing.T) {
	const opTimeout = 300 * time.Millisecond
	setStopAllAgentOpTimeout(t, opTimeout)
	setWorkspaceCheckTimeout(t, 6*time.Second)
	disp := &stopAllCheckDispatcher{block: true, execErrs: map[string]error{}}
	srv, s, broker, project := newStopAllCheckServer(t, disp, "stopall-deadline")
	a := newWorkspaceAgent(t, s, broker, project, "ws-stopall-slow", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)

	start := time.Now()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	disp.mu.Lock()
	execErr, ran := disp.execErrs[a.ID]
	disp.mu.Unlock()
	require.True(t, ran, "fixture check: the check exec ran")
	require.ErrorIs(t, execErr, context.DeadlineExceeded, "the exec ctx is cut")
	assert.Less(t, elapsed, 3*time.Second, "cut at the stop-all deadline, not the check's own 5s exec bound")

	results := stopAllResultsByID(t, rec.Body.Bytes())
	require.Contains(t, results, a.ID, rec.Body.String())
	assert.Equal(t, "stopped", results[a.ID].Status)
	assert.Empty(t, results[a.ID].Warnings)
	assert.JSONEq(t, `{"unchecked":true}`, workspaceAnnotation(t, s, a.ID))
}
