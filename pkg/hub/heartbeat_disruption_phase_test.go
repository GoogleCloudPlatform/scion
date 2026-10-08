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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2669: a preempted or evicted Kubernetes agent leaves
// running. The broker reports stopped when the workspace survives the pod and
// error otherwise (k8sDisruptionPhase in pkg/runtime), with the reason
// preempted or evicted, already while the pod is committed to termination.

// TestHeartbeatDisruption_RunningAgentLeavesRunning covers the broker's
// report of a committed disruption on a running agent.
func TestHeartbeatDisruption_RunningAgentLeavesRunning(t *testing.T) {
	for _, tc := range []struct {
		name, phase, reason, wantMessage string
	}{
		{"preempted-recoverable", "stopped", "preempted", "Agent pod was preempted"},
		{"preempted-not-recoverable", "error", "preempted", "Agent pod was preempted"},
		{"evicted-recoverable", "stopped", "evicted", "Agent pod was evicted"},
		{"evicted-not-recoverable", "error", "evicted", "Agent pod was evicted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)
			// The pod is still terminating: no exit code yet.
			code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
				Slug:            agentSlug,
				Phase:           tc.phase,
				ContainerStatus: "Running",
				ExitReason:      tc.reason,
			})
			require.Equal(t, http.StatusOK, code)

			got := getAgentState(t, s, agentSlug, projectID)
			assert.Equal(t, tc.phase, got.Phase)
			assert.Equal(t, tc.reason, got.ExitReason)
			assert.Equal(t, tc.wantMessage, got.Message)
		})
	}
}

// TestHeartbeatDisruption_ShutdownReportsDoNotReplace: the dying container's
// own shutdown reports (stopping, then a plain stopped) arrive after the
// heartbeat that settled the disruption and must not replace its phase or
// message.
func TestHeartbeatDisruption_ShutdownReportsDoNotReplace(t *testing.T) {
	for _, phase := range []string{"stopped", "error"} {
		t.Run(phase, func(t *testing.T) {
			srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)
			require.Equal(t, http.StatusOK, sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
				Slug:       agentSlug,
				Phase:      phase,
				ExitReason: "preempted",
			}))
			agent := getAgentState(t, s, agentSlug, projectID)
			require.Equal(t, phase, agent.Phase)

			for _, report := range []map[string]string{
				{"phase": "stopping", "message": "Agent shutting down"},
				{"phase": "stopped", "message": "Agent stopped"},
			} {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/status", report)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				got := getAgentState(t, s, agentSlug, projectID)
				assert.Equal(t, phase, got.Phase, "report %v must not replace the settled phase", report)
				assert.Equal(t, "preempted", got.ExitReason)
				assert.Equal(t, "Agent pod was preempted", got.Message)
			}
		})
	}
}

// TestHeartbeatDisruption_UserStopRaceStaysStopped: a stop the user asked
// for (run intent stopped) that races a preemption stays stopped, even when
// the broker reports the disruption as error.
func TestHeartbeatDisruption_UserStopRaceStaysStopped(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)
	agent := getAgentState(t, s, agentSlug, projectID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	stopped := getAgentState(t, s, agentSlug, projectID)
	require.Equal(t, "stopped", stopped.Phase)
	require.Equal(t, store.RunIntentStopped, stopped.RunIntent)

	ec := 137
	require.Equal(t, http.StatusOK, sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "error",
		ExitCode:   &ec,
		ExitReason: "preempted",
	}))

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "stopped", got.Phase, "a user stop must not be turned into an error by a racing disruption")
}
