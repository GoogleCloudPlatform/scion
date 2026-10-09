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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Broker Kubernetes identity mapping refusals translated by the hub
// (ptone/scion#4024).

const (
	testMappingAccount = "agent@example-project.iam.gserviceaccount.com"
	testMappingBroker  = "broker-a"
)

// brokerIdentityMappingErr is a broker identity mapping refusal as it
// reaches the hub. Its message is the broker's operator text, which the hub
// does not pass on; extra adds details beyond account, profile and broker.
func brokerIdentityMappingErr(status int, code string, extra map[string]any) error {
	details := map[string]any{
		api.BrokerErrDetailServiceAccount: testMappingAccount,
		api.BrokerErrDetailProfile:        "gke",
		api.BrokerErrDetailRuntimeEntry:   "k8s",
		api.BrokerErrDetailBroker:         testMappingBroker,
	}
	for k, v := range extra {
		details[k] = v
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code":    code,
		"message": "broker operator text",
		"details": details,
	}})
	return &brokerStatusError{StatusCode: status, Body: string(body)}
}

func brokerNotMappedErr() error {
	return brokerIdentityMappingErr(http.StatusBadRequest, ErrCodeIdentityNotMapped, nil)
}

func brokerKSAMismatchErr() error {
	return brokerIdentityMappingErr(http.StatusBadRequest, ErrCodeIdentityKSAMismatch, map[string]any{
		api.BrokerErrDetailRequestedKSA: "other-ksa",
		api.BrokerErrDetailMappedKSA:    "mapped-ksa",
	})
}

// identityMappingRefusals are the broker refusals the hub translates, with
// the substrings its message must contain.
var identityMappingRefusals = []struct {
	name     string
	err      func() error
	code     string
	contains []string
}{
	{
		name: "not mapped",
		err:  brokerNotMappedErr,
		code: ErrCodeIdentityNotMapped,
		contains: []string{
			`GCP service account "` + testMappingAccount + `" has no Kubernetes service account mapping`,
			`on profile "gke" of broker "broker-a"`,
			"A broker operator must add it to kubernetes_service_account_mappings",
			kubernetesIdentityMappingDocsURL,
		},
	},
	{
		name: "ksa mismatch",
		err:  brokerKSAMismatchErr,
		code: ErrCodeIdentityKSAMismatch,
		contains: []string{
			`The requested Kubernetes service account "other-ksa" does not match "mapped-ksa"`,
			`GCP service account "` + testMappingAccount + `"`,
			`on profile "gke" of broker "broker-a"`,
			"kubernetes_service_account_mappings",
			kubernetesIdentityMappingDocsURL,
		},
	},
}

// assertIdentityMappingRelayed asserts rec is a 400 with code and a message
// containing every substring, details naming the scope, and none of the
// broker's own text.
func assertIdentityMappingRelayed(t *testing.T, rec *httptest.ResponseRecorder, code string, contains []string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, code, resp.Error.Code)
	for _, s := range contains {
		assert.Contains(t, resp.Error.Message, s)
	}
	assert.NotContains(t, resp.Error.Message, "broker operator text")
	assert.Equal(t, testMappingAccount, resp.Error.Details[api.BrokerErrDetailServiceAccount])
	assert.Equal(t, "gke", resp.Error.Details[api.BrokerErrDetailProfile])
	assert.Equal(t, testMappingBroker, resp.Error.Details[api.BrokerErrDetailBroker])
	assert.Equal(t, kubernetesIdentityMappingDocsURL, resp.Error.Details["docs"])
}

func TestDispatchCreateErrorResponse_TranslatesIdentityMappingRefusal(t *testing.T) {
	for _, tc := range identityMappingRefusals {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, tc.err(), "")
			assertIdentityMappingRelayed(t, w, tc.code, tc.contains)
		})
	}
}

// TestDispatchCreateErrorResponse_IdentityCodeOnlyAt400 keeps the
// translation narrow: the codes are honoured only on a 400, the broker's
// text alone is never matched, and other broker errors keep the 502.
func TestDispatchCreateErrorResponse_IdentityCodeOnlyAt400(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "500 identity_not_mapped", err: brokerIdentityMappingErr(http.StatusInternalServerError, ErrCodeIdentityNotMapped, nil)},
		{name: "500 runtime_error", err: brokerIdentityMappingErr(http.StatusInternalServerError, "runtime_error", nil)},
		{name: "text only", err: &brokerStatusError{StatusCode: http.StatusInternalServerError,
			Body: `{"error":{"code":"runtime_error","message":"has no Kubernetes ServiceAccount mapped for \"x\""}}`}},
		{name: "400 validation_error", err: brokerIdentityMappingErr(http.StatusBadRequest, "validation_error", nil)},
		{name: "transport error", err: errors.New("dial tcp: connection refused")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, tc.err, "")
			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
		})
	}
}

// A 400 with another code is not an identity refusal.
func TestRelayIdentityMappingError_OtherCodeNotRelayed(t *testing.T) {
	w := httptest.NewRecorder()
	err := brokerIdentityMappingErr(http.StatusBadRequest, "validation_error", nil)
	assert.False(t, relayIdentityMappingError(w, err))
	assert.Equal(t, 0, w.Body.Len())
}

// A mismatch whose details lack the Kubernetes ServiceAccount names reads
// without empty quotes.
func TestIdentityMappingDispatchError_MismatchWithoutKSANames(t *testing.T) {
	ime, ok := identityMappingDispatchError(brokerIdentityMappingErr(http.StatusBadRequest, ErrCodeIdentityKSAMismatch, nil))
	require.True(t, ok)
	assert.NotContains(t, ime.Message, `""`)
	assert.True(t, strings.HasPrefix(ime.Message,
		`The requested Kubernetes service account does not match the one mapped to GCP service account "`+testMappingAccount+`" on profile "gke" of broker "broker-a".`), ime.Message)
}

// A provision-only create that fails for a mapping refusal stays a 201 and
// carries the hub's coded message in its warning, not the broker's text.
func TestCreateAgent_ProvisionOnlyWarningUsesIdentityMappingMessage(t *testing.T) {
	disp := &skillFailDispatcher{provisionErr: brokerNotMappedErr()}
	srv, _, project := setupCreateAgentServer(t, disp)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "provision-only-idmap",
		ProjectID:     project.ID,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var warning string
	for _, w := range resp.Warnings {
		if strings.HasPrefix(w, api.ProvisionFailedWarningPrefix) {
			warning = w
		}
	}
	require.NotEmpty(t, warning, "warnings: %v", resp.Warnings)
	assert.Contains(t, warning, ErrCodeIdentityNotMapped+": ")
	assert.Contains(t, warning, `on profile "gke" of broker "broker-a"`)
	assert.NotContains(t, warning, "broker operator text")
}

// Without a profile the message names the runtime entry, and without a
// broker name it names the selected broker.
func TestIdentityMappingScopeText(t *testing.T) {
	assert.Equal(t, `profile "gke" of broker "b"`, identityMappingScopeText("gke", "k8s", "b"))
	assert.Equal(t, `runtime entry "k8s" of broker "b"`, identityMappingScopeText("", "k8s", "b"))
	assert.Equal(t, "the default runtime of the selected broker", identityMappingScopeText("", "", ""))
}

func TestCreateAgent_TranslatesIdentityMappingRefusal(t *testing.T) {
	for _, tc := range identityMappingRefusals {
		for _, gather := range []bool{false, true} {
			name := tc.name
			if gather {
				name += "/gather env"
			}
			t.Run(name, func(t *testing.T) {
				srv, _, project := setupCreateAgentServer(t, &failingCreateDispatcher{createErr: tc.err()})
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
					Name:      "identity-map-" + tidSlugSafe(name),
					ProjectID: project.ID,
					Task:      "x",
					GatherEnv: gather,
				})
				assertIdentityMappingRelayed(t, rec, tc.code, tc.contains)
			})
		}
	}
}

func TestAgentLifecycle_StartTranslatesIdentityMappingRefusal(t *testing.T) {
	for _, tc := range identityMappingRefusals {
		for _, action := range []string{"start", "restart"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				disp := &skillFailDispatcher{startErr: tc.err()}
				srv, s, project := setupCreateAgentServer(t, disp)
				agent := createLifecycleTestAgent(t, s, project, "lc-idmap-"+action, state.PhaseStopped)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)

				assertIdentityMappingRelayed(t, rec, tc.code, tc.contains)
			})
		}
	}
}

// TestCreateAgent_ExistingAgentStartTranslatesIdentityMappingRefusal covers
// the create calls that start an existing agent in place.
func TestCreateAgent_ExistingAgentStartTranslatesIdentityMappingRefusal(t *testing.T) {
	phases := []struct {
		name   string
		phase  state.Phase
		resume bool
	}{
		{name: "suspended", phase: state.PhaseSuspended},
		{name: "stopped resume", phase: state.PhaseStopped, resume: true},
		{name: "created", phase: state.PhaseCreated},
	}
	for _, tc := range identityMappingRefusals {
		for _, ph := range phases {
			t.Run(tc.name+"/"+ph.name, func(t *testing.T) {
				disp := &skillFailDispatcher{startErr: tc.err()}
				srv, s, project := setupCreateAgentServer(t, disp)
				agent := createLifecycleTestAgent(t, s, project, "existing-idmap-fail", ph.phase)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
					Name:      agent.Name,
					ProjectID: project.ID,
					Task:      "x",
					Resume:    ph.resume,
				})

				require.True(t, disp.startCalled, "the existing agent must have been started")
				assertIdentityMappingRelayed(t, rec, tc.code, tc.contains)
			})
		}
	}
}

func TestWorkspaceSyncToFinalize_TranslatesIdentityMappingRefusal(t *testing.T) {
	for _, tc := range identityMappingRefusals {
		t.Run(tc.name, func(t *testing.T) {
			disp := &skillFailDispatcher{createErr: tc.err()}
			srv, s, project := setupCreateAgentServer(t, disp)
			srv.SetStorage(newMockStorage("test-bucket"))
			agent := createSiteAgent(t, s, project, "ws-idmap-fail", state.PhaseProvisioning, store.RunIntentStopped)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize",
				map[string]any{"manifest": map[string]any{"version": "1.0", "files": []any{}}})

			assertIdentityMappingRelayed(t, rec, tc.code, tc.contains)
		})
	}
}

func TestSubmitAgentEnv_TranslatesIdentityMappingRefusal(t *testing.T) {
	for _, tc := range identityMappingRefusals {
		t.Run(tc.name, func(t *testing.T) {
			rec := submitEnvWithFinalizeErr(t, "env-idmap-fail", tc.err())
			assertIdentityMappingRelayed(t, rec, tc.code, tc.contains)
		})
	}
}

// A reincarnation records the hub's message, prefixed with its code, for a
// broker identity mapping refusal, and the raw error text for any other.
func TestDispatchFailureText_IdentityMappingRefusal(t *testing.T) {
	for _, tc := range identityMappingRefusals {
		t.Run(tc.name, func(t *testing.T) {
			got := dispatchFailureText(tc.err())
			assert.True(t, strings.HasPrefix(got, tc.code+": "), got)
			for _, s := range tc.contains {
				assert.Contains(t, got, s)
			}
			assert.NotContains(t, got, "broker operator text")
		})
	}
	other := errors.New("dial tcp: connection refused")
	assert.Equal(t, other.Error(), dispatchFailureText(other))
}
