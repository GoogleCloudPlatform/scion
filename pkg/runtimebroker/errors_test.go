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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// TestAgentLookupUnavailable_MessageText pins the exact response produced by
// AgentLookupUnavailable for both retrySuffix forms in use today ("" for
// stop/restart/exec/reset_auth, "the attach" for PTY attach). Nothing
// previously asserted the exact wording or that the retrySuffix argument
// still reaches the response: a call site could swap "" in for "the attach"
// (or vice versa) and every existing test would still pass, since they only
// check for substrings like "retry" or "temporarily".
func TestAgentLookupUnavailable_MessageText(t *testing.T) {
	tests := []struct {
		name        string
		retrySuffix string
		wantMessage string
	}{
		{
			name:        "no retry suffix",
			retrySuffix: "",
			wantMessage: `Unable to look up agent "coord": the container runtime is temporarily unavailable. Please retry in a moment.`,
		},
		{
			name:        "attach retry suffix",
			retrySuffix: "the attach",
			wantMessage: `Unable to look up agent "coord": the container runtime is temporarily unavailable. Please retry the attach in a moment.`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawErr := errors.New("docker ps failed: exit status 1")
			w := httptest.NewRecorder()

			AgentLookupUnavailable(w, rawErr, "coord", "test_op", tt.retrySuffix)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d (%s)", w.Code, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
			}
			if resp.Error.Code != ErrCodeRuntimeUnavailable {
				t.Errorf("expected error code %q, got %q", ErrCodeRuntimeUnavailable, resp.Error.Code)
			}
			if resp.Error.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", resp.Error.Message, tt.wantMessage)
			}
			if strings.Contains(w.Body.String(), rawErr.Error()) {
				t.Errorf("response body must not leak the raw runtime error text: %s", w.Body.String())
			}
		})
	}
}

// TestWriteStartContextError_Honors4xxStatus pins that writeStartContextError
// (errors.go) writes the exact status a *startContextError carries, for any
// 4xx value — not just the 400 the Kubernetes/"block" rejection happens to
// use — and still falls back to a generic 500 for a status of 0 (e.g. an
// older or incomplete *startContextError that never set Status).
func TestWriteStartContextError_Honors4xxStatus(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus int
		wantCode   string
	}{
		{name: "409 conflict", status: http.StatusConflict, wantStatus: http.StatusConflict, wantCode: ErrCodeValidationError},
		{name: "422 unprocessable", status: http.StatusUnprocessableEntity, wantStatus: http.StatusUnprocessableEntity, wantCode: ErrCodeValidationError},
		{name: "zero status falls back to 500", status: 0, wantStatus: http.StatusInternalServerError, wantCode: ErrCodeRuntimeError},
	}

	srv := newTestServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			sce := &startContextError{Status: tt.status, Message: "test message"}

			gotStatus := srv.writeStartContextError(w, sce, "test_op")

			if gotStatus != tt.wantStatus {
				t.Errorf("writeStartContextError returned %d, want %d", gotStatus, tt.wantStatus)
			}
			if w.Code != tt.wantStatus {
				t.Errorf("response status = %d, want %d (%s)", w.Code, tt.wantStatus, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
			}
			if resp.Error.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", resp.Error.Code, tt.wantCode)
			}
		})
	}
}

// TestSkillResolutionFailed_StatusMapping pins the decided cause→status
// mapping (#2546 R3, O1): each classified cause gets the status whose
// semantics fit it, the rate-limited cause also carries a Retry-After header
// when known, and every uncategorized or Hub-originated cause — including the
// empty "resolve_failed" and the Hub's own per-URI codes for PreResolvedSkills
// (storage_error, internal_error, federation_error) — stays on the existing
// 500 path instead of being guessed at as a 4xx.
func TestSkillResolutionFailed_StatusMapping(t *testing.T) {
	tests := []struct {
		name           string
		code           string
		retryAfter     string
		wantStatus     int
		wantRetryAfter string
	}{
		{"not_found maps to 404", agent.SkillErrCodeNotFound, "", http.StatusNotFound, ""},
		{"rate_limited maps to 429 with Retry-After", agent.SkillErrCodeRateLimited, "120", http.StatusTooManyRequests, "120"},
		{"rate_limited without a known Retry-After omits the header", agent.SkillErrCodeRateLimited, "", http.StatusTooManyRequests, ""},
		{"timeout maps to 504", agent.SkillErrCodeTimeout, "", http.StatusGatewayTimeout, ""},
		{"upstream_unavailable maps to 502", agent.SkillErrCodeUpstreamUnavailable, "", http.StatusBadGateway, ""},
		{"unreachable maps to 502", agent.SkillErrCodeUnreachable, "", http.StatusBadGateway, ""},
		{"hub forbidden maps to 403", "forbidden", "", http.StatusForbidden, ""},
		{"uncategorized resolve_failed stays 500", "resolve_failed", "", http.StatusInternalServerError, ""},
		{"empty code stays 500", "", "", http.StatusInternalServerError, ""},
		{"hub-originated storage_error stays 500", "storage_error", "", http.StatusInternalServerError, ""},
		{"hub-originated internal_error stays 500", "internal_error", "", http.StatusInternalServerError, ""},
		{"hub-originated federation_error stays 500", "federation_error", "", http.StatusInternalServerError, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			SkillResolutionFailed(w, &agent.SkillResolutionError{
				URI: "gh://owner/repo/my-skill@main", Code: tt.code, Message: "could not resolve", RetryAfter: tt.retryAfter,
			})

			if w.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d: %s", tt.wantStatus, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("expected Retry-After %q, got %q", tt.wantRetryAfter, got)
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
			}
			if !strings.Contains(resp.Error.Message, "gh://owner/repo/my-skill@main") {
				t.Errorf("expected message to name the skill ref, got: %s", resp.Error.Message)
			}
			if resp.Error.Details["skill"] != "gh://owner/repo/my-skill@main" {
				t.Errorf("expected details.skill to name the ref, got: %v", resp.Error.Details)
			}
			if resp.Error.Details["cause"] != tt.code {
				t.Errorf("expected details.cause %q, got: %v", tt.code, resp.Error.Details)
			}
		})
	}
}

// startContextSentinel stands in for identity-bearing detail a real
// buildStartContext failure's error text can carry: a hydration failure's
// own error can name a template path or a storage bucket/object name, and a
// generic failure can carry anything a lower layer's error wraps (a
// container ID, a node name, a namespace). None of this is something a
// broker HTTP client is entitled to see in the response body, but it must
// still reach the server's own log so the failure is diagnosable.
const startContextSentinel = "gs://acme-templates-bucket/tenant-7f3/template.tar.gz"

// TestWriteStartContextError_RedactsAndLogsEachFallbackBranch is the
// RED/GREEN proof for the three branches writeStartContextError redacts: a
// sentinel carrying identity-bearing detail must never reach the HTTP
// response body, but must reach the server-side log first. The two branches
// writeStartContextError does NOT redact (the Hub-connectivity 503 and the
// 4xx client-validation passthrough) are proven unchanged by
// TestEnvGather_TemplateHydrationConnectivityFailure_MatchesLaunch and
// TestWriteStartContextError_Honors4xxStatus respectively — both continue
// to pass unmodified by this change, so they are not duplicated here.
func TestWriteStartContextError_RedactsAndLogsEachFallbackBranch(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "non-startContextError falls back to a redacted generic 500",
			err:        errors.New(startContextSentinel),
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeRuntimeError,
		},
		{
			name: "IsHubError but not a connectivity failure redacts the template error",
			err: &startContextError{
				Message:     "Failed to hydrate template: " + startContextSentinel,
				IsHubError:  true,
				OriginalErr: errors.New(startContextSentinel),
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeTemplateError,
		},
		{
			name: "a *startContextError with no 4xx Status falls back to a redacted generic 500",
			err: &startContextError{
				Message:     startContextSentinel,
				OriginalErr: errors.New(startContextSentinel),
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrCodeRuntimeError,
		},
	}

	srv := newTestServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			w := httptest.NewRecorder()
			gotStatus := srv.writeStartContextError(w, tt.err, "redact_test_op")

			if gotStatus != tt.wantStatus {
				t.Errorf("writeStartContextError returned %d, want %d", gotStatus, tt.wantStatus)
			}
			if w.Code != tt.wantStatus {
				t.Errorf("response status = %d, want %d", w.Code, tt.wantStatus)
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
			}
			if resp.Error.Code != tt.wantCode {
				t.Errorf("error code = %q, want %q", resp.Error.Code, tt.wantCode)
			}
			if strings.Contains(w.Body.String(), startContextSentinel) {
				t.Errorf("response body must not leak the sentinel, got: %s", w.Body.String())
			}
			if resp.Error.Message != "Failed to redact_test_op" {
				t.Errorf("response message = %q, want the fixed, op-labeled message", resp.Error.Message)
			}
			logged := logBuf.String()
			if !strings.Contains(logged, startContextSentinel) {
				t.Errorf("server log must contain the sentinel so the failure stays diagnosable, got: %s", logged)
			}
		})
	}
}

// TestStartContextSpanText_PrefersOriginalErrOverCuratedMessage is the
// regression test for recording the real failure on an OTEL span, not a
// *startContextError's own curated, client-safe Message: a
// *startContextError's Error() method returns Message verbatim (see its own
// doc comment), so a caller that recorded err.Error() directly on a span
// would record the SAME curated text a client already receives in the HTTP
// response, defeating the point of a separate internal diagnostics surface.
// startContextSpanText must instead surface OriginalErr when the two
// differ, and must still behave like a plain err.Error() for a generic,
// non-startContextError error (buildStartContext does return those too).
func TestStartContextSpanText_PrefersOriginalErrOverCuratedMessage(t *testing.T) {
	sce := &startContextError{
		Message:     "Failed to resolve the hub endpoint",
		OriginalErr: errors.New(startContextSentinel),
	}
	if got := startContextSpanText(sce); got != startContextSentinel {
		t.Errorf("startContextSpanText(sce) = %q, want the OriginalErr text %q, not the curated Message", got, startContextSentinel)
	}

	noOriginalErr := &startContextError{Message: "image must be pinned by digest"}
	if got := startContextSpanText(noOriginalErr); got != "image must be pinned by digest" {
		t.Errorf("startContextSpanText(no OriginalErr) = %q, want the Message itself (nothing more specific to report)", got)
	}

	plain := errors.New("a generic, non-startContextError failure")
	if got := startContextSpanText(plain); got != plain.Error() {
		t.Errorf("startContextSpanText(plain error) = %q, want %q unchanged", got, plain.Error())
	}
}

// TestMethodNotAllowed_SetsAllowHeader pins the helper itself: RFC 9110
// section 15.5.6 requires a 405 to carry an Allow header (ptone/scion#2421).
func TestMethodNotAllowed_SetsAllowHeader(t *testing.T) {
	tests := []struct {
		name    string
		methods []string
		want    string
	}{
		{"single", []string{http.MethodGet}, "GET"},
		{"multiple", []string{http.MethodGet, http.MethodDelete}, "GET, DELETE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			MethodNotAllowed(w, tt.methods[0], tt.methods[1:]...)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
			if got := w.Header().Get("Allow"); got != tt.want {
				t.Errorf("Allow = %q, want %q", got, tt.want)
			}
			if _, present := w.Header()["Allow"]; tt.want == "" && present {
				t.Errorf("Allow header set with no methods")
			}
		})
	}
}

// TestMethodNotAllowed_AllowHeaderPerRoute drives every runtimebroker route
// that answers 405 with a method it does not accept, and asserts the Allow
// header lists exactly the methods that route does accept. One row per
// MethodNotAllowed call site fixed in ptone/scion#2421.
func TestMethodNotAllowed_AllowHeaderPerRoute(t *testing.T) {
	srv := newTestServer(t)
	tests := []struct {
		method, path, wantAllow string
	}{
		{http.MethodPost, "/healthz", "GET"},
		{http.MethodPost, "/readyz", "GET"},
		{http.MethodPost, "/api/v1/info", "GET"},
		{http.MethodPost, "/api/v1/hub-connections", "GET"},
		{http.MethodPut, "/api/v1/agents", "GET, POST"},
		{http.MethodPut, "/api/v1/agents/test-agent-1", "GET, DELETE"},
		{http.MethodGet, "/api/v1/agents/test-agent-1/stop", "POST"},
		{http.MethodPost, "/api/v1/agents/test-agent-1/logs", "GET"},
		{http.MethodGet, "/api/v1/projects/some-project", "DELETE"},
		{http.MethodPost, "/api/v1/images/status", "GET"},
		{http.MethodGet, "/api/v1/images/pull", "POST"},
		{http.MethodGet, "/api/v1/images/local", "DELETE"},
		{http.MethodGet, "/api/v1/workspace/upload", "POST"},
		{http.MethodGet, "/api/v1/workspace/apply", "POST"},
		{http.MethodGet, "/api/v1/workspace/project-upload", "POST"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusMethodNotAllowed, w.Body.String())
			}
			if got := w.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}
		})
	}
}

// TestWriteStartContextError_MissingLocalFileIsNotHubUnreachable is the
// regression test for ptone/scion#3531. A file the broker cannot find while
// hydrating a template or harness-config (for example, a Hub that stores
// resources locally handing a remote broker file:// URLs for paths that only
// exist on the Hub host) used to be reported as a 503 hub_unreachable:
// *fs.PathError satisfies net.Error, so IsHubConnectivityError classified it
// as a connectivity failure. It must be a 422 template_error that names the
// missing file's base name without leaking its full path.
func TestWriteStartContextError_MissingLocalFileIsNotHubUnreachable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "hub-host-only", "storage", "harness-configs", "claude", "settings.json")
	_, readErr := os.ReadFile(missing)
	if !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("expected a not-exist error reading %s, got %v", missing, readErr)
	}
	// Pin the precondition that made this a bug: without the dedicated
	// check, the raw file error reads as a Hub-connectivity error.
	if !templatecache.IsHubConnectivityError(readErr) {
		t.Fatalf("precondition changed: IsHubConnectivityError(%v) = false; the regression this test guards may no longer be reachable", readErr)
	}

	// Shaped as hydrateHarnessConfig/hydrateTemplate failures arrive: the
	// resolver wraps the transfer client's read error, and buildStartContext
	// marks it IsHubError.
	wrapped := fmt.Errorf("failed to download file settings.json: %w", readErr)

	tests := []struct {
		name string
		sce  *startContextError
	}{
		{
			name: "harness-config hydration",
			sce: &startContextError{
				Status:      http.StatusInternalServerError,
				Message:     "Failed to hydrate harness-config: " + wrapped.Error(),
				IsHubError:  true,
				OriginalErr: wrapped,
			},
		},
		{
			name: "template hydration, unwrapped path error",
			sce: &startContextError{
				Status:      http.StatusInternalServerError,
				Message:     "Failed to hydrate template: " + readErr.Error(),
				IsHubError:  true,
				OriginalErr: readErr,
			},
		},
		{
			name: "non-hub start-context step",
			sce: &startContextError{
				Status:      http.StatusInternalServerError,
				Message:     "prepare failed",
				OriginalErr: wrapped,
			},
		},
	}

	srv := newTestServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			w := httptest.NewRecorder()
			gotStatus := srv.writeStartContextError(w, tt.sce, "create agent")

			if gotStatus != http.StatusUnprocessableEntity || w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d (returned %d), want 422; body: %s", w.Code, gotStatus, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode error response %q: %v", w.Body.String(), err)
			}
			if resp.Error.Code != ErrCodeTemplateError {
				t.Errorf("error code = %q, want %q", resp.Error.Code, ErrCodeTemplateError)
			}
			if strings.Contains(strings.ToLower(resp.Error.Message), "unreachable") {
				t.Errorf("message must not blame Hub connectivity: %q", resp.Error.Message)
			}
			if !strings.HasPrefix(resp.Error.Message, "Failed to create agent: ") {
				t.Errorf("message should be op-labeled, got %q", resp.Error.Message)
			}
			if got := resp.Error.Details["file"]; got != "settings.json" {
				t.Errorf("details.file = %v, want settings.json", got)
			}
			if got := resp.Error.Details["cause"]; got != "file_not_found" {
				t.Errorf("details.cause = %v, want file_not_found", got)
			}
			if strings.Contains(w.Body.String(), "hub-host-only") {
				t.Errorf("response must not leak the full missing path: %s", w.Body.String())
			}
			if !strings.Contains(logBuf.String(), "hub-host-only") {
				t.Errorf("server log should record the full path, got: %s", logBuf.String())
			}
		})
	}

	t.Run("explicit 4xx status still wins", func(t *testing.T) {
		w := httptest.NewRecorder()
		sce := &startContextError{Status: http.StatusBadRequest, Message: "curated", OriginalErr: wrapped}
		if got := srv.writeStartContextError(w, sce, "create agent"); got != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", got, w.Body.String())
		}
	})
}
