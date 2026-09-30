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
