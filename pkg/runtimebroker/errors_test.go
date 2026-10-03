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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			sce := &startContextError{Status: tt.status, Message: "test message"}

			gotStatus := writeStartContextError(w, sce)

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
