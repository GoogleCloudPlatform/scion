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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A broker 400 validation_error reaches the client with the broker's
// status, code and message instead of the generic 502 (ptone/scion#2666).

const brokerValidationMessage = `GCP identity mode "block" is not supported on the Kubernetes runtime`

// fakeValidationBroker is a runtime broker HTTP endpoint that answers every request
// with status and body.
func fakeValidationBroker(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// brokerErrorBody is the broker's JSON error envelope, with the start
// markers it adds to a failed create or start.
func brokerErrorBody(code, message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code":    code,
		"message": message,
		"details": map[string]any{"startAttempted": true, "runId": "run-1"},
	}})
	return string(b)
}

// fakeBrokerCreateErr is the error the hub's broker client returns when the
// fake broker answers a create with status and body.
func fakeBrokerCreateErr(t *testing.T, status int, body string) error {
	t.Helper()
	broker := fakeValidationBroker(t, status, body)
	_, err := NewHTTPRuntimeBrokerClient().CreateAgent(context.Background(), tid("broker-1"), broker.URL,
		&RemoteCreateAgentRequest{ID: "hub-uuid-1", Slug: tid("agent-1"), Name: "agent-1", ProjectID: tid("project-1")})
	require.Error(t, err)
	return err
}

// fakeBrokerStartErr is the error the hub's broker client returns when the
// fake broker answers a start with status and body.
func fakeBrokerStartErr(t *testing.T, status int, body string) error {
	t.Helper()
	broker := fakeValidationBroker(t, status, body)
	_, err := NewHTTPRuntimeBrokerClient().StartAgent(context.Background(), tid("broker-1"), broker.URL,
		"agent-1", "", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{})
	require.Error(t, err)
	return err
}

func decodeRelayErrorResponse(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

func TestDispatchCreateErrorResponse_FakeBrokerValidationErrorBecomes400(t *testing.T) {
	err := fakeBrokerCreateErr(t, http.StatusBadRequest, brokerErrorBody("validation_error", brokerValidationMessage))

	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, err, "")

	assertHarnessConfigRelayed(t, w, http.StatusBadRequest, ErrCodeValidationError, brokerValidationMessage)
}

// TestDispatchCreateErrorResponse_FakeBrokerOtherStatusesUnchanged pins the
// mappings the 400 relay must not touch: a broker 500 (even one coded
// validation_error) stays 502 runtime_error, and the existing 404 and 409
// relays keep their status and code.
func TestDispatchCreateErrorResponse_FakeBrokerOtherStatusesUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode int
		wantErr  string
	}{
		{name: "500 runtime_error", status: http.StatusInternalServerError, body: brokerErrorBody("runtime_error", "boom"),
			wantCode: http.StatusBadGateway, wantErr: ErrCodeRuntimeError},
		{name: "500 validation_error", status: http.StatusInternalServerError, body: brokerErrorBody("validation_error", "boom"),
			wantCode: http.StatusBadGateway, wantErr: ErrCodeRuntimeError},
		{name: "404 not_found", status: http.StatusNotFound, body: brokerErrorBody("not_found", "template not found"),
			wantCode: http.StatusNotFound, wantErr: ErrCodeNotFound},
		{name: "409 container name conflict", status: http.StatusConflict,
			body:     brokerErrorBody("conflict", "container name \"agent-1\" is already in use"),
			wantCode: http.StatusConflict, wantErr: ErrCodeConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := fakeBrokerCreateErr(t, tc.status, tc.body)

			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, err, "")

			require.Equal(t, tc.wantCode, w.Code, w.Body.String())
			assert.Equal(t, tc.wantErr, decodeRelayErrorResponse(t, w).Error.Code)
		})
	}
}

func TestAgentLifecycle_StartFakeBrokerValidationErrorBecomes400(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			startErr := fakeBrokerStartErr(t, http.StatusBadRequest, brokerErrorBody("validation_error", brokerValidationMessage))
			disp := &skillFailDispatcher{startErr: startErr}
			srv, s, project := setupCreateAgentServer(t, disp)
			agent := createLifecycleTestAgent(t, s, project, "lc-val-"+action, state.PhaseStopped)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)

			assertHarnessConfigRelayed(t, rec, http.StatusBadRequest, ErrCodeValidationError, brokerValidationMessage)
		})
	}
}

func TestAgentLifecycle_StartFakeBroker500Stays502(t *testing.T) {
	startErr := fakeBrokerStartErr(t, http.StatusInternalServerError, brokerErrorBody("runtime_error", "boom"))
	disp := &skillFailDispatcher{startErr: startErr}
	srv, s, project := setupCreateAgentServer(t, disp)
	agent := createLifecycleTestAgent(t, s, project, "lc-val-500", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeRuntimeError, decodeRelayErrorResponse(t, rec).Error.Code)
}

func TestCreateAgent_FakeBrokerValidationErrorBecomes400(t *testing.T) {
	createErr := fakeBrokerCreateErr(t, http.StatusBadRequest, brokerErrorBody("validation_error", brokerValidationMessage))
	disp := &failingCreateDispatcher{createErr: createErr}
	srv, s, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "create-val-fail",
		ProjectID: project.ID,
		Task:      "some task",
	})

	assertHarnessConfigRelayed(t, rec, http.StatusBadRequest, ErrCodeValidationError, brokerValidationMessage)
	assert.True(t, disp.deleteCalled, "the failed create is still cleaned up on the broker")
	require.NotNil(t, disp.capturedAgent, "the dispatcher saw the create-time agent")
	_, err := s.GetAgent(context.Background(), disp.capturedAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the hub agent row is removed, as for the 422 and 403 relays")
}
