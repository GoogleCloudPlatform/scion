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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A broker name conflict reaches the client as a 409 with the broker's code
// and fixed message, keyed on the broker's status and code rather than on
// Docker's wording, which the broker never sends (ptone/scion#3501).

// brokerNameConflicts are the broker answers for an agent name still held by
// a stopped container: 409 conflict with the agent.ErrContainerNameInUse
// text (create, start, restart), and 409 name_in_use, the code the async
// launch gives the same conflict.
var brokerNameConflicts = []struct {
	name string
	code string
}{
	{name: "409 conflict", code: ErrCodeConflict},
	{name: "409 name_in_use", code: api.LaunchCodeNameInUse},
}

// nameInUseMessage is the broker's fixed text for the conflict.
var nameInUseMessage = agent.ErrContainerNameInUse.Error()

func TestDispatchCreateErrorResponse_BrokerNameConflictBecomes409(t *testing.T) {
	for _, tc := range brokerNameConflicts {
		t.Run(tc.name, func(t *testing.T) {
			err := fakeBrokerCreateErr(t, http.StatusConflict, brokerErrorBody(tc.code, nameInUseMessage))

			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, err, "")

			assertBrokerRefusalRelayed(t, w, http.StatusConflict, tc.code, nameInUseMessage)
		})
	}
}

// TestDispatchCreateErrorResponse_NameConflictKeyedOnStatusAndCode pins
// what the 409 relay must not touch: the sentinel text in a broker 5xx, a
// broker 409 with another code, and Docker's own wording outside a broker
// answer all keep the generic 502 runtime_error.
func TestDispatchCreateErrorResponse_NameConflictKeyedOnStatusAndCode(t *testing.T) {
	tests := []struct {
		name string
		err  func(t *testing.T) error
	}{
		{name: "500 runtime_error with the name-in-use text", err: func(t *testing.T) error {
			return fakeBrokerCreateErr(t, http.StatusInternalServerError, brokerErrorBody("runtime_error", nameInUseMessage))
		}},
		{name: "502 conflict code", err: func(t *testing.T) error {
			return fakeBrokerCreateErr(t, http.StatusBadGateway, brokerErrorBody(ErrCodeConflict, nameInUseMessage))
		}},
		{name: "409 other code", err: func(t *testing.T) error {
			return fakeBrokerCreateErr(t, http.StatusConflict, brokerErrorBody("start_refused", "start refused"))
		}},
		{name: "409 without broker envelope", err: func(t *testing.T) error {
			return fakeBrokerCreateErr(t, http.StatusConflict, "<html>Conflict</html>")
		}},
		{name: "docker wording outside a broker answer", err: func(*testing.T) error {
			return &dockerWordingError{}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, tc.err(t), "")

			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			assert.Equal(t, ErrCodeRuntimeError, decodeRelayErrorResponse(t, w).Error.Code)
		})
	}
}

// dockerWordingError carries Docker's raw name-conflict wording, which the
// hub used to match on.
type dockerWordingError struct{}

func (*dockerWordingError) Error() string {
	return `Conflict. The container name "/scion-agent-1" is already in use by container "abc123"`
}

func TestCreateAgent_BrokerNameConflictBecomes409(t *testing.T) {
	for _, tc := range brokerNameConflicts {
		t.Run(tc.name, func(t *testing.T) {
			createErr := fakeBrokerCreateErr(t, http.StatusConflict, brokerErrorBody(tc.code, nameInUseMessage))
			disp := &failingCreateDispatcher{createErr: createErr}
			srv, _, project := setupCreateAgentServer(t, disp)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:      "create-name-conflict",
				ProjectID: project.ID,
				Task:      "some task",
			})

			assertBrokerRefusalRelayed(t, rec, http.StatusConflict, tc.code, nameInUseMessage)
		})
	}
}

// TestCreateAgent_ExistingAgentStartBrokerNameConflictBecomes409 covers the
// three create calls that start an existing agent in place.
func TestCreateAgent_ExistingAgentStartBrokerNameConflictBecomes409(t *testing.T) {
	phases := []struct {
		name   string
		phase  state.Phase
		resume bool
	}{
		{name: "suspended", phase: state.PhaseSuspended},
		{name: "stopped resume", phase: state.PhaseStopped, resume: true},
		{name: "created", phase: state.PhaseCreated},
	}
	for _, tc := range brokerNameConflicts {
		for _, ph := range phases {
			t.Run(tc.name+"/"+ph.name, func(t *testing.T) {
				startErr := fakeBrokerStartErr(t, http.StatusConflict, brokerErrorBody(tc.code, nameInUseMessage))
				disp := &skillFailDispatcher{startErr: startErr}
				srv, s, project := setupCreateAgentServer(t, disp)
				existing := createLifecycleTestAgent(t, s, project, "existing-name-conflict", ph.phase)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
					Name:      existing.Name,
					ProjectID: project.ID,
					Task:      "x",
					Resume:    ph.resume,
				})

				require.True(t, disp.startCalled, "the existing agent must have been started")
				assertBrokerRefusalRelayed(t, rec, http.StatusConflict, tc.code, nameInUseMessage)
			})
		}
	}
}

func TestAgentLifecycle_StartBrokerNameConflictBecomes409(t *testing.T) {
	for _, tc := range brokerNameConflicts {
		for _, action := range []string{"start", "restart"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				startErr := fakeBrokerStartErr(t, http.StatusConflict, brokerErrorBody(tc.code, nameInUseMessage))
				disp := &skillFailDispatcher{startErr: startErr}
				srv, s, project := setupCreateAgentServer(t, disp)
				a := createLifecycleTestAgent(t, s, project, "lc-name-conflict-"+action, state.PhaseStopped)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/"+action, nil)

				assertBrokerRefusalRelayed(t, rec, http.StatusConflict, tc.code, nameInUseMessage)
			})
		}
	}
}

// TestAgentLifecycle_StartBroker500WithNameInUseTextStays502: the sentinel
// text alone, in a broker 5xx, is not a name conflict.
func TestAgentLifecycle_StartBroker500WithNameInUseTextStays502(t *testing.T) {
	startErr := fakeBrokerStartErr(t, http.StatusInternalServerError, brokerErrorBody("runtime_error", nameInUseMessage))
	disp := &skillFailDispatcher{startErr: startErr}
	srv, s, project := setupCreateAgentServer(t, disp)
	a := createLifecycleTestAgent(t, s, project, "lc-name-conflict-500", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeRuntimeError, decodeRelayErrorResponse(t, rec).Error.Code)
}

func TestWorkspaceSyncToFinalize_BrokerNameConflictBecomes409(t *testing.T) {
	for _, tc := range brokerNameConflicts {
		t.Run(tc.name, func(t *testing.T) {
			createErr := fakeBrokerCreateErr(t, http.StatusConflict, brokerErrorBody(tc.code, nameInUseMessage))
			disp := &skillFailDispatcher{createErr: createErr}
			srv, s, project := setupCreateAgentServer(t, disp)
			srv.SetStorage(newMockStorage("test-bucket"))
			a := createSiteAgent(t, s, project, "ws-name-conflict", state.PhaseProvisioning, store.RunIntentStopped)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/workspace/sync-to/finalize",
				map[string]any{"manifest": map[string]any{"version": "1.0", "files": []any{}}})

			assertBrokerRefusalRelayed(t, rec, http.StatusConflict, tc.code, nameInUseMessage)
		})
	}
}

func TestSubmitAgentEnv_BrokerNameConflictBecomes409(t *testing.T) {
	for _, tc := range brokerNameConflicts {
		t.Run(tc.name, func(t *testing.T) {
			finalizeErr := fakeBrokerCreateErr(t, http.StatusConflict, brokerErrorBody(tc.code, nameInUseMessage))
			rec := submitEnvWithFinalizeErr(t, "env-name-conflict", finalizeErr)
			assertBrokerRefusalRelayed(t, rec, http.StatusConflict, tc.code, nameInUseMessage)
		})
	}
}
