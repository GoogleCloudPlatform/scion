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
	"bytes"
	"errors"
	"log/slog"
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

// TestWriteRuntimeOpError_LogsRawErrorRecordsSpanWritesFixedBody proves
// writeRuntimeOpError is the one call site every runtime-op handler makes
// on failure, and that it never silently discards the raw error: it logs
// the raw error (identity and all) via s.agentLifecycleLog, and only the
// fixed, identity-free message reaches the HTTP response body.
func TestWriteRuntimeOpError_LogsRawErrorRecordsSpanWritesFixedBody(t *testing.T) {
	srv := newTestServer(t)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logBuf, nil))

	rawErr := errors.New(identityLeakingRuntimeError)
	w := httptest.NewRecorder()
	srv.writeRuntimeOpError(w, t.Context(), "stop agent", rawErr, "agent_id", "test-agent-1")

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "stop agent") {
		t.Errorf("log output missing op %q: %s", "stop agent", logOutput)
	}
	if !strings.Contains(logOutput, "test-agent-1") {
		t.Errorf("log output missing extra field %q: %s", "test-agent-1", logOutput)
	}
	if !strings.Contains(logOutput, "my-actor-7f3") {
		t.Errorf("log output missing the raw error's identity detail (my-actor-7f3): %s", logOutput)
	}

	body := w.Body.String()
	if strings.Contains(body, "my-actor-7f3") {
		t.Errorf("response body leaked the raw error's identity detail: %s", body)
	}
	if !strings.Contains(body, "Failed to stop agent") {
		t.Errorf("response body missing the fixed message: %s", body)
	}
}

// TestWriteStartContextError_PassesThroughStatusAndMessageVerbatim proves
// a *startContextError's own Status and Message — broker-composed at
// buildStartContext, not raw runtime topology — reach the client
// unchanged, never replaced by runtimeOpError's generic "Failed to <op>"
// message.
func TestWriteStartContextError_PassesThroughStatusAndMessageVerbatim(t *testing.T) {
	srv := newTestServer(t)
	sce := &startContextError{
		Status:  http.StatusBadRequest,
		Message: "image must be pinned by digest",
	}
	w := httptest.NewRecorder()
	srv.writeStartContextError(w, sce, "start agent")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	body := w.Body.String()
	if !strings.Contains(body, "image must be pinned by digest") {
		t.Errorf("response body = %s, want the curated message verbatim", body)
	}
	if strings.Contains(body, "Failed to") {
		t.Errorf("response body = %s, want the curated message, not runtimeOpError's generic form", body)
	}
}

// TestWriteStartContextError_HidesHubHydrationTextButPassesCuratedMessage
// proves writeStartContextError's routing: an IsHubError *startContextError
// (as buildStartContext returns from a template/harness-config hydration
// failure) must reach the response body only as the fixed "Failed to <op>"
// text — its Message embeds the hydration failure's own error text, which
// is never shown verbatim outside createAgent's own hub-connectivity
// special case — while a non-IsHubError, 4xx *startContextError's Message
// (buildStartContext's own curated, client-safe text) is written verbatim.
func TestWriteStartContextError_HidesHubHydrationTextButPassesCuratedMessage(t *testing.T) {
	srv := newTestServer(t)
	// A hydration failure's identity-bearing text, deliberately free of any
	// of templatecache.IsHubConnectivityError's own connectivity-pattern
	// substrings (e.g. "connection refused", "timeout"): this subtest
	// exercises the non-connectivity IsHubError branch specifically, so
	// OriginalErr here must not accidentally also satisfy the connectivity
	// check and route into the unrelated 503 branch instead.
	const hydrationIdentityLeak = "rpc error: container my-actor-7f3 on node gke-pool-2 in namespace tenant-acme: permission denied"
	hubErr := &startContextError{
		Status:      http.StatusInternalServerError,
		Message:     "Failed to hydrate harness-config: " + hydrationIdentityLeak,
		IsHubError:  true,
		OriginalErr: errors.New(hydrationIdentityLeak),
	}
	w := httptest.NewRecorder()
	srv.writeStartContextError(w, hubErr, "start agent")
	body := w.Body.String()
	if strings.Contains(body, "my-actor-7f3") {
		t.Errorf("IsHubError case: response body leaked the hydration error's identity detail: %s", body)
	}
	if !strings.Contains(body, "Failed to start agent") {
		t.Errorf("IsHubError case: response body = %s, want the fixed op message", body)
	}

	curatedErr := &startContextError{
		Status:  http.StatusBadRequest,
		Message: "image must be pinned by digest",
	}
	w2 := httptest.NewRecorder()
	srv.writeStartContextError(w2, curatedErr, "start agent")
	if w2.Code != http.StatusBadRequest {
		t.Errorf("curated case: status = %d, want %d", w2.Code, http.StatusBadRequest)
	}
	body2 := w2.Body.String()
	if !strings.Contains(body2, "image must be pinned by digest") {
		t.Errorf("curated case: response body = %s, want the curated message verbatim", body2)
	}
	if strings.Contains(body2, "Failed to") {
		t.Errorf("curated case: response body = %s, want the curated message, not the generic op message", body2)
	}
}

// TestWriteStartContextError_LogsOriginalErrNotFixedMessage proves a
// startContextError whose curated Message is a fixed string with the real
// diagnostic detail in OriginalErr (e.g. buildStartContext's
// global-config-dir or hub-endpoint-resolution failures) still gets that
// detail into the server's own log — not just the same fixed string the
// client already received. err.Error() on a *startContextError returns
// Message itself, so a log call that used it directly (rather than
// OriginalErr) would otherwise discard the one place this detail could
// reach any diagnostic surface.
func TestWriteStartContextError_LogsOriginalErrNotFixedMessage(t *testing.T) {
	srv := newTestServer(t)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&logBuf, nil))

	rawErr := errors.New(identityLeakingRuntimeError)
	sce := &startContextError{
		Status:      http.StatusInternalServerError,
		Message:     "Failed to resolve the hub endpoint",
		OriginalErr: rawErr,
	}
	w := httptest.NewRecorder()
	srv.writeStartContextError(w, sce, "start agent")

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "my-actor-7f3") {
		t.Errorf("log output missing the OriginalErr detail (my-actor-7f3): %s", logOutput)
	}
	body := w.Body.String()
	if strings.Contains(body, "my-actor-7f3") {
		t.Errorf("response body leaked the OriginalErr detail: %s", body)
	}
	if !strings.Contains(body, "Failed to start agent") {
		t.Errorf("response body = %s, want the fixed, op-labeled message", body)
	}
}

// TestBrokerRuntimeOpHandlers_RedactRawErrorFromResponseBody is the table
// test over the broker's runtime-op handler set: for each handler, a raw
// (identity-bearing) runtime error injected at the manager boundary must
// never reach the HTTP response body — only the fixed, per-operation
// message must appear there.
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
