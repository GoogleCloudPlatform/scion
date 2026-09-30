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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpaqueError_NeverExposesWrappedText proves the core redaction
// primitive runtimeOpError builds on: an OpaqueError's own Error() text is
// exactly the fixed message it was constructed with, regardless of what
// identity-bearing detail the wrapped error carries (a container ID, a
// node name, a namespace — the kind of thing a real Docker/Kubernetes/
// substrate backend error routinely embeds), while Unwrap still exposes
// the original for server-side telemetry/logging.
func TestOpaqueError_NeverExposesWrappedText(t *testing.T) {
	raw := errors.New("rpc error: container my-actor-7f3 on node gke-pool-2 in namespace tenant-acme: connection refused")
	opaque := runtimeOpError("stop agent", raw)

	if opaque.Error() != "Failed to stop agent" {
		t.Errorf("Error() = %q, want %q", opaque.Error(), "Failed to stop agent")
	}
	for _, leaked := range []string{"my-actor-7f3", "gke-pool-2", "tenant-acme"} {
		if strings.Contains(opaque.Error(), leaked) {
			t.Errorf("Error() leaked %q from the wrapped error: %q", leaked, opaque.Error())
		}
	}
	if errors.Unwrap(opaque) != raw {
		t.Errorf("Unwrap() = %v, want the original raw error for telemetry/logging", errors.Unwrap(opaque))
	}
}

// identityLeakingRuntimeError stands in for a real container/pod runtime
// backend error: it carries a container id, a node name, and a namespace,
// none of which a broker HTTP client is entitled to see, whether or not it
// is authorized to act on the agent itself.
const identityLeakingRuntimeError = "rpc error: container my-actor-7f3 on node gke-pool-2 in namespace tenant-acme: connection refused"

// TestBrokerRuntimeOpHandlers_RedactRawErrorFromResponseBody is the table
// test over the broker's runtime-op handler set required by this class of
// fix: for each handler, a raw (identity-bearing) runtime error injected at
// the manager boundary must never reach the HTTP response body — only the
// fixed, per-operation message must appear there.
func TestBrokerRuntimeOpHandlers_RedactRawErrorFromResponseBody(t *testing.T) {
	cases := []struct {
		name        string
		makeReq     func() *http.Request
		injectErr   func(mgr *mockManager)
		wantMessage string
	}{
		{
			name: "stop",
			makeReq: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/stop", nil)
			},
			injectErr:   func(mgr *mockManager) { mgr.stopErr = errors.New(identityLeakingRuntimeError) },
			wantMessage: "Failed to stop agent",
		},
		{
			name: "delete",
			makeReq: func() *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/api/v1/agents/test-agent-1", nil)
			},
			injectErr:   func(mgr *mockManager) { mgr.deleteTargetErr = errors.New(identityLeakingRuntimeError) },
			wantMessage: "Failed to delete agent",
		},
		{
			name: "restart (start failure)",
			makeReq: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", nil)
			},
			injectErr:   func(mgr *mockManager) { mgr.startErr = errors.New(identityLeakingRuntimeError) },
			wantMessage: "Failed to restart agent",
		},
		{
			name: "message",
			makeReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/message", strings.NewReader(`{"message":"hi"}`))
				req.Header.Set("Content-Type", "application/json")
				return req
			},
			injectErr:   func(mgr *mockManager) { mgr.messageErr = errors.New(identityLeakingRuntimeError) },
			wantMessage: "Failed to send message to agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			tc.injectErr(mgr)

			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, tc.makeReq())

			body := w.Body.String()
			for _, leaked := range []string{"my-actor-7f3", "gke-pool-2", "tenant-acme"} {
				if strings.Contains(body, leaked) {
					t.Errorf("%s: response body leaked runtime identity %q: %s", tc.name, leaked, body)
				}
			}
			if !strings.Contains(body, tc.wantMessage) {
				t.Errorf("%s: expected the fixed message %q, got: %s", tc.name, tc.wantMessage, body)
			}
		})
	}
}
