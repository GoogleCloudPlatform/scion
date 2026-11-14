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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pre-start hook failure relayed by the broker heartbeat (sciontool
// writes it to agent-info.json only) logs one hub-side warning with the
// agent ID, failure stage and sanitized message; the heartbeats confirming
// the error phase afterwards log nothing (ptone/scion#7927).
func TestHeartbeatErrorPhase_LogsOnce(t *testing.T) {
	f := newReconcileFixture(t)
	logs := &captureHandler{}
	f.srv.agentLifecycleLog = slog.New(logs)
	a := f.addAgent("hookfail", "starting", "")

	exit := 1
	hb := brokerHeartbeatRequest{
		Status: store.BrokerStatusOnline,
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{{
			Slug: a.Slug, Phase: "error", ContainerStatus: "exited (1)", ExitCode: &exit,
			Message: provisionFailureMessage,
		}}}},
	}
	for range 2 {
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/runtime-brokers/"+f.brokerID+"/heartbeat", hb)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	require.Equal(t, "error", f.get(a.ID).Phase)

	recs := logs.records(logAgentEnteredErrorPhase)
	require.Len(t, recs, 1, "only the write that entered phase error logs")
	attrs := recs[0].attrs
	assert.Equal(t, a.ID, attrs["agent_id"])
	assert.Equal(t, f.projectID, attrs["project_id"])
	assert.Equal(t, f.brokerID, attrs["broker_id"])
	assert.Equal(t, errorPhaseSourceHeartbeat, attrs["source"])
	assert.Equal(t, failureStageHarnessProvision, attrs["failure_stage"])
	assert.Equal(t, "20-harness-provision", attrs["hook"])
	msg, _ := attrs["message"].(string)
	assert.Contains(t, msg, "harness provisioner failed")
	assert.NotContains(t, msg, "sk-ant-not-a-real-key")
}

// The same for an error phase the agent reports itself (a clone or init
// failure sciontool sends straight to the hub).
func TestUpdateAgentStatus_ErrorPhase_Logs(t *testing.T) {
	f := newReconcileFixture(t)
	logs := &captureHandler{}
	f.srv.agentLifecycleLog = slog.New(logs)
	a := f.addAgent("clonefail", "starting", "")

	body, err := json.Marshal(store.AgentStatusUpdate{Phase: "error", Message: "git clone failed: exit status 128"})
	require.NoError(t, err)
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/status", bytes.NewReader(body))
		req = req.WithContext(contextWithIdentity(req.Context(), agentIdentityFor(a.ID, f.projectID, ScopeAgentStatusUpdate)))
		rec := httptest.NewRecorder()
		f.srv.updateAgentStatus(rec, req, a.ID)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.Equal(t, "error", got.Phase)

	recs := logs.records(logAgentEnteredErrorPhase)
	require.Len(t, recs, 1)
	assert.Equal(t, a.ID, recs[0].attrs["agent_id"])
	assert.Equal(t, errorPhaseSourceAgentStatus, recs[0].attrs["source"])
	assert.Equal(t, failureStageGitClone, recs[0].attrs["failure_stage"])
}
