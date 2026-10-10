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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Broker harness-config refusals relayed by the hub (ptone/scion#3132).

const (
	harnessUnusableMessage = `Failed to create agent: harness-config "scripted" provisioner is not usable: missing script; fix the harness-config`
	harnessPolicyMessage   = `harness-config "scripted" uses scripted provisioning but this broker has allow_container_script_harnesses=false.`
)

// brokerHarnessConfigErr is a broker error answer as it reaches the hub.
// Details carry the broker's start markers, which are not relayed.
func brokerHarnessConfigErr(status int, code, message string) error {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code":    code,
		"message": message,
		"details": map[string]any{"startAttempted": true, "runId": "run-1"},
	}})
	return &brokerStatusError{StatusCode: status, Body: string(body)}
}

// brokerRefusals are the broker's 4xx refusals relayBrokerRefusal relays.
var brokerRefusals = []struct {
	name    string
	status  int
	code    string
	message string
}{
	{name: "422 unusable", status: http.StatusUnprocessableEntity, code: "harness_config_unusable", message: harnessUnusableMessage},
	{name: "403 policy", status: http.StatusForbidden, code: "forbidden", message: harnessPolicyMessage},
	// A broker 400 validation_error is relayed by the same helper
	// (ptone/scion#2666).
	{name: "400 validation", status: http.StatusBadRequest, code: "validation_error", message: brokerValidationMessage},
}

// assertBrokerRefusalRelayed asserts rec carries the broker's status, code
// and message, with no details.
func assertBrokerRefusalRelayed(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, code, resp.Error.Code)
	assert.Equal(t, message, resp.Error.Message)
	assert.Empty(t, resp.Error.Details, "broker start markers are not relayed")
}

func TestDispatchCreateErrorResponse_RelaysHarnessConfigRefusal(t *testing.T) {
	for _, tc := range brokerRefusals {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, brokerHarnessConfigErr(tc.status, tc.code, tc.message), "")
			assertBrokerRefusalRelayed(t, w, tc.status, tc.code, tc.message)
		})
	}
}

// TestDispatchCreateErrorResponse_OtherBrokerErrorsStay502 keeps the relay
// narrow: a broker 5xx (even with a harness-config code), a 4xx with another
// code, and transport errors keep the generic 502 runtime_error.
func TestDispatchCreateErrorResponse_OtherBrokerErrorsStay502(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "500 runtime_error", err: brokerHarnessConfigErr(http.StatusInternalServerError, "runtime_error", "boom")},
		{name: "500 harness_config_unusable", err: brokerHarnessConfigErr(http.StatusInternalServerError, "harness_config_unusable", "boom")},
		{name: "503 forbidden", err: brokerHarnessConfigErr(http.StatusServiceUnavailable, "forbidden", "boom")},
		{name: "403 without broker envelope", err: &brokerStatusError{StatusCode: http.StatusForbidden, Body: "<html>Forbidden</html>"}},
		{name: "422 other code", err: brokerHarnessConfigErr(http.StatusUnprocessableEntity, "validation_error", "boom")},
		{name: "403 unusable code", err: brokerHarnessConfigErr(http.StatusForbidden, "harness_config_unusable", "boom")},
		{name: "400 other code", err: brokerHarnessConfigErr(http.StatusBadRequest, "bad_request", "boom")},
		{name: "400 without broker envelope", err: &brokerStatusError{StatusCode: http.StatusBadRequest, Body: "<html>Bad Request</html>"}},
		{name: "500 validation_error", err: brokerHarnessConfigErr(http.StatusInternalServerError, "validation_error", "boom")},
		{name: "transport error", err: errors.New("dial tcp: connection refused")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, tc.err, "")
			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, ErrCodeRuntimeError, resp.Error.Code)
		})
	}
}

func TestAgentLifecycle_StartRelaysHarnessConfigRefusal(t *testing.T) {
	for _, tc := range brokerRefusals {
		for _, action := range []string{"start", "restart"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				disp := &skillFailDispatcher{startErr: brokerHarnessConfigErr(tc.status, tc.code, tc.message)}
				srv, s, project := setupCreateAgentServer(t, disp)
				agent := createLifecycleTestAgent(t, s, project, "lc-hc-"+action, state.PhaseStopped)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)

				assertBrokerRefusalRelayed(t, rec, tc.status, tc.code, tc.message)
			})
		}
	}
}

// TestCreateAgent_ExistingAgentStartRelaysHarnessConfigRefusal covers the
// create calls that start an existing agent in place.
func TestCreateAgent_ExistingAgentStartRelaysHarnessConfigRefusal(t *testing.T) {
	phases := []struct {
		name   string
		phase  state.Phase
		resume bool
	}{
		{name: "suspended", phase: state.PhaseSuspended},
		{name: "stopped resume", phase: state.PhaseStopped, resume: true},
		{name: "created", phase: state.PhaseCreated},
	}
	for _, tc := range brokerRefusals {
		for _, ph := range phases {
			t.Run(tc.name+"/"+ph.name, func(t *testing.T) {
				disp := &skillFailDispatcher{startErr: brokerHarnessConfigErr(tc.status, tc.code, tc.message)}
				srv, s, project := setupCreateAgentServer(t, disp)
				agent := createLifecycleTestAgent(t, s, project, "existing-hc-fail", ph.phase)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
					Name:      agent.Name,
					ProjectID: project.ID,
					Task:      "x",
					Resume:    ph.resume,
				})

				require.True(t, disp.startCalled, "the existing agent must have been started")
				assertBrokerRefusalRelayed(t, rec, tc.status, tc.code, tc.message)
			})
		}
	}
}

func TestWorkspaceSyncToFinalize_RelaysHarnessConfigRefusal(t *testing.T) {
	for _, tc := range brokerRefusals {
		t.Run(tc.name, func(t *testing.T) {
			disp := &skillFailDispatcher{createErr: brokerHarnessConfigErr(tc.status, tc.code, tc.message)}
			srv, s, project := setupCreateAgentServer(t, disp)
			srv.SetStorage(newMockStorage("test-bucket"))
			agent := createSiteAgent(t, s, project, "ws-hc-fail", state.PhaseProvisioning, store.RunIntentStopped)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize",
				map[string]any{"manifest": map[string]any{"version": "1.0", "files": []any{}}})

			assertBrokerRefusalRelayed(t, rec, tc.status, tc.code, tc.message)
		})
	}
}

// finalizeEnvErrDispatcher answers finalize-env with err.
type finalizeEnvErrDispatcher struct {
	createAgentDispatcher
	err error
}

func (d *finalizeEnvErrDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*CreateDispatchResult, error) {
	return nil, d.err
}

// submitEnvWithFinalizeErr submits env for a provisioning agent whose
// finalize-env dispatch fails with err.
func submitEnvWithFinalizeErr(t *testing.T, name string, err error) *httptest.ResponseRecorder {
	t.Helper()
	rec, _ := submitEnvWithFinalizeErrAgent(t, name, err)
	return rec
}

// submitEnvWithFinalizeErrAgent is submitEnvWithFinalizeErr that also
// returns the agent row as stored after the request.
func submitEnvWithFinalizeErrAgent(t *testing.T, name string, err error) (*httptest.ResponseRecorder, *store.Agent) {
	t.Helper()
	srv, s, project := setupCreateAgentServer(t, &finalizeEnvErrDispatcher{err: err})
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Name:            name,
		Slug:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/"+name+"/env",
		SubmitEnvRequest{Env: map[string]string{"API_KEY": "v"}})
	got, gerr := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, gerr)
	return rec, got
}

func TestSubmitAgentEnv_RelaysHarnessConfigRefusal(t *testing.T) {
	for _, tc := range brokerRefusals {
		t.Run(tc.name, func(t *testing.T) {
			rec := submitEnvWithFinalizeErr(t, "env-hc-fail", brokerHarnessConfigErr(tc.status, tc.code, tc.message))
			assertBrokerRefusalRelayed(t, rec, tc.status, tc.code, tc.message)
		})
	}
}

func TestSubmitAgentEnv_OtherBrokerErrorsStay502(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "500 runtime_error", err: brokerHarnessConfigErr(http.StatusInternalServerError, "runtime_error", "boom")},
		{name: "transport error", err: errors.New("dial tcp: connection refused")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := submitEnvWithFinalizeErr(t, "env-other-fail", tc.err)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
		})
	}
}
