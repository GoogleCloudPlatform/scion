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

package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDispatchCreateErrorResponse_BrokerNotFoundBecomes404 proves the fix for
// ptone/scion#1316 fault 3: when the runtime broker reports 404 for an
// unresolvable named resource (harness-config or template), the hub must
// surface a 404 naming the resource instead of folding every dispatch
// failure into RuntimeError's 502.
func TestDispatchCreateErrorResponse_BrokerNotFoundBecomes404(t *testing.T) {
	err := &brokerStatusError{
		StatusCode: http.StatusNotFound,
		Body:       `{"error":{"code":"not_found","message":"Failed to create agent: failed to find harness-config \"antigravity\": harness-config not found"}}`,
	}

	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, err)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, w.Code, w.Body.String())
	}

	var resp ErrorResponse
	if decErr := json.NewDecoder(w.Body).Decode(&resp); decErr != nil {
		t.Fatalf("failed to decode response: %v", decErr)
	}
	if resp.Error.Code != ErrCodeNotFound {
		t.Errorf("expected code %q, got %q", ErrCodeNotFound, resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "antigravity") {
		t.Errorf("expected message to name the unresolved resource, got: %s", resp.Error.Message)
	}
}

// TestDispatchCreateErrorResponse_ContainerConflictBecomes409 proves the
// existing container-name-conflict classification survives being folded
// into the shared helper.
func TestDispatchCreateErrorResponse_ContainerConflictBecomes409(t *testing.T) {
	err := fmt.Errorf("container name 'my-agent' is already in use")

	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, err)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, w.Code, w.Body.String())
	}
}

// TestDispatchCreateErrorResponse_OtherErrorStays502 proves the
// classification is narrow: a dispatch failure that is neither a container
// name conflict nor a broker 404 still gets the generic 502, unchanged from
// before this fix.
func TestDispatchCreateErrorResponse_OtherErrorStays502(t *testing.T) {
	err := fmt.Errorf("connection refused")

	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, err)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadGateway, w.Code, w.Body.String())
	}
}

// TestDispatchCreateErrorResponse_OtherBrokerStatusStays502 proves the 404
// classification does not widen to other broker status codes (e.g. a
// broker-side 500 must still map to the hub's 502, not a 404).
func TestDispatchCreateErrorResponse_OtherBrokerStatusStays502(t *testing.T) {
	err := &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}

	w := httptest.NewRecorder()
	dispatchCreateErrorResponse(w, err)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadGateway, w.Code, w.Body.String())
	}
}

// TestDispatchCreateErrorResponse_SkillResolutionRelaysStatusAndRetryAfter
// proves the fix for #2546 R2: a broker skill-resolution failure is relayed
// verbatim — status, message (naming the ref), and Retry-After header —
// instead of every status but 404 collapsing into RuntimeError's 502 with the
// ref reachable only inside an embedded raw JSON body.
func TestDispatchCreateErrorResponse_SkillResolutionRelaysStatusAndRetryAfter(t *testing.T) {
	tests := []struct {
		name           string
		statusCode     int
		retryAfter     string
		wantRetryAfter string
	}{
		{"429 rate limited carries Retry-After", http.StatusTooManyRequests, "120", "120"},
		{"504 timeout carries no Retry-After", http.StatusGatewayTimeout, "", ""},
		{"502 upstream_unavailable carries no Retry-After", http.StatusBadGateway, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"error":{"code":"skill_resolution_failed","message":"required skill \"gh://owner/repo/my-skill@main\" could not be resolved: boom","details":{"skill":"gh://owner/repo/my-skill@main","cause":"irrelevant-for-this-test"}}}`
			err := &brokerStatusError{StatusCode: tt.statusCode, Body: body, RetryAfter: tt.retryAfter}

			w := httptest.NewRecorder()
			dispatchCreateErrorResponse(w, err)

			if w.Code != tt.statusCode {
				t.Fatalf("expected status %d, got %d: %s", tt.statusCode, w.Code, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("expected Retry-After %q, got %q", tt.wantRetryAfter, got)
			}

			var resp ErrorResponse
			if decErr := json.NewDecoder(w.Body).Decode(&resp); decErr != nil {
				t.Fatalf("failed to decode response: %v", decErr)
			}
			if resp.Error.Code != skillResolutionErrorCode {
				t.Errorf("expected code %q, got %q", skillResolutionErrorCode, resp.Error.Code)
			}
			if !strings.Contains(resp.Error.Message, "gh://owner/repo/my-skill@main") {
				t.Errorf("expected message to name the unresolved skill ref as a top-level error, got: %s", resp.Error.Message)
			}
			// The broker message is relayed without a hub prefix (#2546 N4).
			if want := `required skill "gh://owner/repo/my-skill@main" could not be resolved: boom`; resp.Error.Message != want {
				t.Errorf("expected the broker message verbatim %q, got %q", want, resp.Error.Message)
			}
			// The broker's structured details are forwarded (#2546 N3).
			if resp.Error.Details["skill"] != "gh://owner/repo/my-skill@main" || resp.Error.Details["cause"] != "irrelevant-for-this-test" {
				t.Errorf("expected details {skill, cause} forwarded from the broker, got %v", resp.Error.Details)
			}
		})
	}
}
