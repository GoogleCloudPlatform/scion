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
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const resourceLimitsTestMessage = `cloudrun: 4 vCPU (from resolved resources.limits.cpu) allows memory 2048Mi to 16384Mi, got 1024Mi (from resources.limits.memory); set resources.limits.cpu or resources.limits.memory`

// resourceLimitsErr is a runtime's refusal of the agent's resource spec,
// wrapped twice as the agent manager and a caller might.
func resourceLimitsErr() error {
	return fmt.Errorf("start: %w", fmt.Errorf("run: %w", runtime.NewResourceLimitsError("%s", resourceLimitsTestMessage)))
}

// Every synchronous start path answers a runtime's refusal of the resource
// spec with 400 validation_error and "Failed to <op>: " plus the public
// message, which the hub relays unchanged (ptone/scion#7103).
func TestResourceLimitsError_DispatchPathsAnswer400(t *testing.T) {
	cases := []struct {
		name, path, body, op string
		startAttempted       bool
	}{
		{"create sync start", "/api/v1/agents", `{"name": "r1", "id": "id-r1", "slug": "r1", "requestId": "req-r1", "config": {"template": "none"}}`, "create agent", false},
		{"start", "/api/v1/agents/r2/start", `{"runId": "run-r2"}`, "start agent", true},
		{"restart", "/api/v1/agents/r3/restart", `{"runId": "run-r3"}`, "restart agent", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := dispatchTestEnv(t, true)
			srv.manager = &unusableManager{at: "start", err: resourceLimitsErr()}
			code, body := postJSON(t, srv, tc.path, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", code, body)
			}
			var resp ErrorResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("not an ErrorResponse: %v: %s", err, body)
			}
			if resp.Error.Code != ErrCodeValidationError {
				t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeValidationError)
			}
			if want := "Failed to " + tc.op + ": " + resourceLimitsTestMessage; resp.Error.Message != want {
				t.Errorf("message = %q, want %q", resp.Error.Message, want)
			}
			if tc.startAttempted && resp.Error.Details[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected %s=true in details, got %v", api.BrokerErrorDetailStartAttempted, resp.Error.Details)
			}
			if tc.op == "create agent" {
				// The hub-facing dispatch attempt is marked failed with the
				// same 400 and the public message.
				srv.dispatchAttemptsMu.Lock()
				var marked bool
				for _, a := range srv.dispatchAttempts {
					if a.Status == dispatchAttemptFailed && a.HTTPStatus == http.StatusBadRequest && a.Error == resourceLimitsTestMessage {
						marked = true
					}
				}
				n := len(srv.dispatchAttempts)
				srv.dispatchAttemptsMu.Unlock()
				if !marked {
					t.Errorf("dispatch attempt (%d recorded) was not marked failed with 400 and the public message", n)
				}
			}
		})
	}
}

// Any other start failure keeps the fixed runtime_error text: only the
// typed refusal's public message reaches the caller.
func TestResourceLimitsError_OtherFailuresKeepRuntimeError(t *testing.T) {
	srv, _, _ := dispatchTestEnv(t, true)
	srv.manager = &unusableManager{at: "start", err: errors.New("cloudrun: allows memory 1Mi to 2Mi, got 3Mi in projects/p1/locations/r1")}
	code, body := postJSON(t, srv, "/api/v1/agents", `{"name": "r4", "id": "id-r4", "slug": "r4", "config": {"template": "none"}}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", code, body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not an ErrorResponse: %v: %s", err, body)
	}
	if resp.Error.Code != ErrCodeRuntimeError || resp.Error.Message != "Failed to create agent" {
		t.Errorf("error = %q %q, want runtime_error with the fixed text", resp.Error.Code, resp.Error.Message)
	}
}

// An async launch reports a runtime's refusal of the resource spec as
// container_config with the full public message, not the generic
// runtime_error text.
func TestClassifyStartError_ResourceLimits(t *testing.T) {
	for _, kind := range []string{store.LaunchKindCreate, store.LaunchKindStart} {
		code, _, msg := classifyStartError(context.Background(), resourceLimitsErr(), "", "", kind)
		if code != api.LaunchCodeContainerConfig {
			t.Errorf("%s: code = %q, want %q", kind, code, api.LaunchCodeContainerConfig)
		}
		if msg != resourceLimitsTestMessage {
			t.Errorf("%s: message = %q, want %q", kind, msg, resourceLimitsTestMessage)
		}
	}
}
