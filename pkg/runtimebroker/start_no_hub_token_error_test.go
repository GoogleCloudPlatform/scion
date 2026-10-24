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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestStartWithoutHubTokenReportsCause pins that a Manager.Start failure
// with agent.ErrNoHubToken reaches the hub with a message naming the cause
// (the hub issued no agent token; check its agent token service) on every
// start path: synchronous create, start, restart and the async launch
// report. The status and code are those of any other runtime failure (500
// runtime_error), and the message carries none of the wrapped error's own
// text.
//
// A restart first stops the running agent and then runs Manager.Start, so a
// restart without a hub-issued token ends with the agent stopped: an agent
// that has no hub token does not run. The restart case pins that order (one
// stop, then the failed start).
func TestStartWithoutHubTokenReportsCause(t *testing.T) {
	// Made-up text standing in for detail the wrapped error may carry.
	const innerDetail = "fake-inner-detail-7f3a"
	wrapped := fmt.Errorf("cannot start agent %q (%s): %w", "tok-agent", innerDetail, agent.ErrNoHubToken)

	assertCause := func(t *testing.T, msg, op string) {
		t.Helper()
		if !strings.HasPrefix(msg, "Failed to "+op+": ") {
			t.Errorf("message %q does not start with the %q lead", msg, op)
		}
		for _, want := range []string{"hub issued no agent token", "agent token service"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q does not contain %q", msg, want)
			}
		}
		if strings.Contains(msg, innerDetail) {
			t.Errorf("message %q carries the wrapped error's own text", msg)
		}
	}

	for _, tc := range []struct {
		name, path, body, op string
		// wantStopped: the handler stops the agent, then the start fails.
		wantStopped bool
	}{
		{"sync create", "/api/v1/agents", `{"name": "tok-agent", "config": {"template": "claude"}}`, opCreateAgent, false},
		{"start", "/api/v1/agents/test-agent-1/start", `{}`, "start agent", false},
		{"restart stops the agent, then the start fails", "/api/v1/agents/test-agent-1/restart", `{}`, "restart agent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			mgr.startErr = wrapped
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v: %s", err, w.Body.String())
			}
			if body.Error.Code != ErrCodeRuntimeError {
				t.Errorf("code %q, want %q", body.Error.Code, ErrCodeRuntimeError)
			}
			assertCause(t, body.Error.Message, tc.op)
			mgr.mu.Lock()
			stops, starts := mgr.stopCalls, mgr.startCalls
			mgr.mu.Unlock()
			if starts != 1 {
				t.Errorf("Manager.Start calls = %d, want 1", starts)
			}
			if tc.wantStopped && stops != 1 {
				t.Errorf("stop calls = %d, want 1: the restart stops the agent, then the start fails", stops)
			}
		})
	}

	t.Run("async launch report", func(t *testing.T) {
		code, _, msg := classifyStartError(context.Background(), wrapped, "", "")
		if code != api.LaunchCodeRuntimeError {
			t.Errorf("code %q, want %q", code, api.LaunchCodeRuntimeError)
		}
		assertCause(t, msg, opCreateAgent)
	})

	t.Run("other runtime failures get the fixed lead only", func(t *testing.T) {
		if got := runtimeOpError("start agent", fmt.Errorf("boom %s", innerDetail)).Error(); got != "Failed to start agent" {
			t.Errorf("runtimeOpError = %q, want the fixed lead only", got)
		}
	})
}
