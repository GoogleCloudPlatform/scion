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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Broker harness-config refusals relayed by the hub (ptone/scion#3132).

const (
	harnessUnusableMessage  = `Failed to create agent: harness-config "scripted" provisioner is not usable: missing script; fix the harness-config`
	storageMigrationMessage = `Failed to start agent: the global settings file sets a removed storage override: profiles.p1.shared_dir_storage_backend ("nfs").`
	harnessPolicyMessage    = `harness-config "scripted" uses scripted provisioning but this broker has allow_container_script_harnesses=false.`
	// The broker's text names configuration fields, never the path
	// (runtime.ProjectShareUnavailableText).
	projectStorageMessage = `project storage share is not visible on this broker at its configured mount point: make the share (the same storage the hub uses) available in this broker's container at that path`
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

// brokerRefusals are the broker refusals relayBrokerRefusal relays.
var brokerRefusals = []struct {
	name    string
	status  int
	code    string
	message string
}{
	{name: "422 unusable", status: http.StatusUnprocessableEntity, code: "harness_config_unusable", message: harnessUnusableMessage},
	// The broker's refusal of a start whose storage would be placed
	// differently than configured (ptone/scion#6371) is relayed the same way.
	{name: "422 storage migration", status: http.StatusUnprocessableEntity, code: "storage_migration_required", message: storageMigrationMessage},
	{name: "403 policy", status: http.StatusForbidden, code: "forbidden", message: harnessPolicyMessage},
	// A broker 400 validation_error is relayed by the same helper
	// (ptone/scion#2666).
	{name: "400 validation", status: http.StatusBadRequest, code: "validation_error", message: brokerValidationMessage},
	// A broker that does not see the project storage share
	// (ptone/scion#5823): its path-free text and code reach the client.
	{name: "503 project storage", status: http.StatusServiceUnavailable, code: "project_storage_unavailable", message: projectStorageMessage},
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
		{name: "500 storage_migration_required", err: brokerHarnessConfigErr(http.StatusInternalServerError, "storage_migration_required", "boom")},
		{name: "503 forbidden", err: brokerHarnessConfigErr(http.StatusServiceUnavailable, "forbidden", "boom")},
		{name: "500 project_storage_unavailable", err: brokerHarnessConfigErr(http.StatusInternalServerError, "project_storage_unavailable", "boom")},
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

// A broker 503 project_storage_unavailable is relayed with its status and
// code, but its message only when it is one of the known path-free texts. A
// broker built before those texts named the host mount path in the message;
// the hub answers with a fixed text instead, never the path.
func TestDispatchCreateErrorResponse_ProjectStorageMessageFiltered(t *testing.T) {
	for _, known := range scionrt.ProjectShareClientTexts() {
		w := httptest.NewRecorder()
		dispatchCreateErrorResponse(w, brokerHarnessConfigErr(http.StatusServiceUnavailable, "project_storage_unavailable", known), "")
		assertBrokerRefusalRelayed(t, w, http.StatusServiceUnavailable, "project_storage_unavailable", known)
	}
	old := "project storage share is not mounted on this broker (/mnt/nfs/hub-a; mount it or set nfs.auto_mount)"
	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, brokerHarnessConfigErr(http.StatusServiceUnavailable, "project_storage_unavailable", old), "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "project_storage_unavailable", resp.Error.Code)
	assert.Equal(t, projectStorageRefusalFallback, resp.Error.Message)
	assert.NotContains(t, w.Body.String(), "/mnt/nfs", "an older broker's mount path must not reach the client")
}
