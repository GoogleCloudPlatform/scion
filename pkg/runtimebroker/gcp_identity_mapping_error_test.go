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

package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const testMappingErrorGlobalSettingsYAML = `schema_version: "1"
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            mapped@example-project.iam.gserviceaccount.com: mapped-ksa
`

// buildAssignStartContextErr runs buildStartContext for a Kubernetes GCP
// identity "assign" create with saEmail and an optional explicit
// Kubernetes ServiceAccount, on a broker named "broker-a", and returns its
// *startContextError.
func buildAssignStartContextErr(t *testing.T, saEmail, explicitKSA string) *startContextError {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.BrokerName = "broker-a"
	srv := newTestServerForStartContextRuntime(t, cfg, "kubernetes")
	projectDir := newTestProjectSettings(t, testKubernetesProjectSettingsYAML)
	newTestGlobalSettings(t, testMappingErrorGlobalSettingsYAML)

	createCfg := &CreateAgentConfig{
		GCPIdentity: &GCPIdentityConfig{MetadataMode: "assign", SAEmail: saEmail},
	}
	if explicitKSA != "" {
		createCfg.Kubernetes = &api.KubernetesConfig{ServiceAccountName: explicitKSA}
	}
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-mapping-error",
		ProjectPath: projectDir,
		Config:      createCfg,
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	var sce *startContextError
	if !errors.As(err, &sce) {
		t.Fatalf("expected a *startContextError, got %T: %v", err, err)
	}
	return sce
}

// TestBuildStartContext_IdentityNotMappedCarriesCode pins that the broker's
// missing-mapping refusal carries the identity_not_mapped code and the
// details the hub builds its message from, and keeps its operator text
// (ptone/scion#4024).
func TestBuildStartContext_IdentityNotMappedCarriesCode(t *testing.T) {
	sce := buildAssignStartContextErr(t, "unmapped@example-project.iam.gserviceaccount.com", "")
	if sce.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", sce.Status)
	}
	if sce.Code != ErrCodeIdentityNotMapped {
		t.Errorf("code = %q, want %q", sce.Code, ErrCodeIdentityNotMapped)
	}
	if !strings.Contains(sce.Message, `has no Kubernetes ServiceAccount mapped for "unmapped@example-project.iam.gserviceaccount.com"`) {
		t.Errorf("message lost its operator text: %q", sce.Message)
	}
	want := map[string]interface{}{
		api.BrokerErrDetailServiceAccount: "unmapped@example-project.iam.gserviceaccount.com",
		api.BrokerErrDetailRuntimeEntry:   "kubernetes",
		api.BrokerErrDetailBroker:         "broker-a",
		// The test runtime has no Kubernetes client, so the annotation
		// lookup could not run.
		api.BrokerErrDetailDiscovery: api.BrokerKSADiscoveryUnavailable,
		api.BrokerErrDetailNamespace: scionrt.DefaultKubernetesNamespace(),
	}
	assertDetails(t, sce.Details, want)
}

// TestBuildStartContext_IdentityKSAMismatchCarriesCode pins the same for an
// explicit Kubernetes ServiceAccount that differs from the mapped one.
func TestBuildStartContext_IdentityKSAMismatchCarriesCode(t *testing.T) {
	sce := buildAssignStartContextErr(t, "mapped@example-project.iam.gserviceaccount.com", "other-ksa")
	if sce.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", sce.Status)
	}
	if sce.Code != ErrCodeIdentityKSAMismatch {
		t.Errorf("code = %q, want %q", sce.Code, ErrCodeIdentityKSAMismatch)
	}
	if !strings.Contains(sce.Message, `explicit Kubernetes ServiceAccount "other-ksa" does not match`) {
		t.Errorf("message lost its operator text: %q", sce.Message)
	}
	want := map[string]interface{}{
		api.BrokerErrDetailServiceAccount: "mapped@example-project.iam.gserviceaccount.com",
		api.BrokerErrDetailRuntimeEntry:   "kubernetes",
		api.BrokerErrDetailBroker:         "broker-a",
		api.BrokerErrDetailRequestedKSA:   "other-ksa",
		api.BrokerErrDetailMappedKSA:      "mapped-ksa",
		api.BrokerErrDetailKSASource:      api.BrokerKSASourceMapped,
	}
	assertDetails(t, sce.Details, want)
}

func assertDetails(t *testing.T, got, want map[string]interface{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("details = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("details[%q] = %v, want %v", k, got[k], v)
		}
	}
}

// TestWriteStartContextError_WritesCodeAndDetails pins that a 4xx
// startContextError with a Code is written with that code and its details,
// and that one without a Code still gets validation_error and no details.
func TestWriteStartContextError_WritesCodeAndDetails(t *testing.T) {
	srv := newTestServer(t)
	for _, tc := range []struct {
		name        string
		sce         *startContextError
		wantCode    string
		wantDetails bool
	}{
		{
			name: "coded",
			sce: &startContextError{Status: http.StatusBadRequest, Message: "m", Code: ErrCodeIdentityNotMapped,
				Details: map[string]interface{}{api.BrokerErrDetailServiceAccount: "unmapped@example-project.iam.gserviceaccount.com"}},
			wantCode:    ErrCodeIdentityNotMapped,
			wantDetails: true,
		},
		{
			name:     "uncoded",
			sce:      &startContextError{Status: http.StatusBadRequest, Message: "m"},
			wantCode: ErrCodeValidationError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if got := srv.writeStartContextError(w, tc.sce, "test_op"); got != http.StatusBadRequest {
				t.Errorf("returned %d, want 400", got)
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decoding %q: %v", w.Body.String(), err)
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", resp.Error.Code, tc.wantCode)
			}
			if resp.Error.Message != "m" {
				t.Errorf("message = %q, want %q", resp.Error.Message, "m")
			}
			if tc.wantDetails {
				if resp.Error.Details[api.BrokerErrDetailServiceAccount] != "unmapped@example-project.iam.gserviceaccount.com" {
					t.Errorf("details = %v, want the service account", resp.Error.Details)
				}
			} else if resp.Error.Details != nil {
				t.Errorf("details = %v, want none", resp.Error.Details)
			}
		})
	}
}
