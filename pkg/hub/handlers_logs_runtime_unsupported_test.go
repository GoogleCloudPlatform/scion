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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createLoggableAgent creates a running agent through the same dispatcher
// used by the logs test, so GET .../logs has an agent to dispatch to.
func createLoggableAgent(t *testing.T, srv *Server, projectID string) string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "logs-test-agent",
		ProjectID: projectID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	return resp.Agent.ID
}

// TestHandleAgentLogs_RuntimeLogsUnsupported_Passthrough: a broker
// 501/runtime_logs_unsupported error (the substrate runtime's
// ErrLogsNotSupported, relayed as a *brokerStatusError) reaches the caller
// with the same status and code and the hub's own fixed message — never the
// broker-supplied text. The broker's body here deliberately carries a
// hostile, namespace-like string in its message to prove the hub does not
// repeat it: any broker that reports this status and code gets the same
// hub-side response, regardless of what it puts in its own body.
func TestHandleAgentLogs_RuntimeLogsUnsupported_Passthrough(t *testing.T) {
	const hostileBrokerMessage = "leaked: scion-tenant-namespace/other-actor-name worker-pod-7"
	disp := &createAgentDispatcher{
		createPhase: string(state.PhaseRunning),
		logsErr: &brokerStatusError{
			StatusCode: http.StatusNotImplemented,
			Body:       `{"error":{"code":"runtime_logs_unsupported","message":"` + hostileBrokerMessage + `"}}`,
		},
	}
	srv, _, project := setupCreateAgentServer(t, disp)

	agentID := createLoggableAgent(t, srv, project.ID)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/logs", nil)

	require.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, "runtime_logs_unsupported", errResp.Error.Code)
	assert.Equal(t, runtimeLogsUnsupportedMessage, errResp.Error.Message)
	// The hub's own constant, not the broker's arbitrary text, no matter
	// what the broker put in its body.
	assert.NotContains(t, errResp.Error.Message, "scion-tenant-namespace")
	assert.NotContains(t, errResp.Error.Message, "other-actor-name")
	assert.NotContains(t, errResp.Error.Message, "worker-pod-7")
	assert.NotContains(t, rec.Body.String(), "scion-tenant-namespace")
}

// TestHandleAgentLogs_SameStatusDifferentCode_UnchangedPath: a broker error
// with the same 501 status but a different code must not be mistaken for
// the substrate sentinel — it keeps the generic 502 path.
func TestHandleAgentLogs_SameStatusDifferentCode_UnchangedPath(t *testing.T) {
	disp := &createAgentDispatcher{
		createPhase: string(state.PhaseRunning),
		logsErr: &brokerStatusError{
			StatusCode: http.StatusNotImplemented,
			Body:       `{"error":{"code":"some_other_code","message":"unrelated not-implemented condition"}}`,
		},
	}
	srv, _, project := setupCreateAgentServer(t, disp)

	agentID := createLoggableAgent(t, srv, project.ID)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/logs", nil)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInternalError, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "Failed to retrieve logs from broker")
}

// TestHandleAgentLogs_UnrelatedBrokerError_UnchangedPath: an ordinary
// (non-*brokerStatusError) dispatch failure is unaffected and keeps the
// existing 502 mapping byte-for-byte.
func TestHandleAgentLogs_UnrelatedBrokerError_UnchangedPath(t *testing.T) {
	disp := &createAgentDispatcher{
		createPhase: string(state.PhaseRunning),
		logsErr:     assertAnError{},
	}
	srv, _, project := setupCreateAgentServer(t, disp)

	agentID := createLoggableAgent(t, srv, project.ID)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/logs", nil)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInternalError, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "Failed to retrieve logs from broker")
	assert.Contains(t, errResp.Error.Message, "connection reset")
}

// assertAnError is a plain error, deliberately not a *brokerStatusError, to
// prove the passthrough only fires for that concrete type.
type assertAnError struct{}

func (assertAnError) Error() string { return "connection reset" }
