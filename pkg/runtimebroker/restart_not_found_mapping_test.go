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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A restart whose start fails on a missing harness-config or template
// reports that resource with 404 not_found, the create text, not 404
// agent_not_found: the agent exists (ptone/scion#5433). Other "not found"
// start errors keep the agent_not_found mapping.
func TestRestartAgent_MissingHarnessConfigOrTemplateIsNotAgentNotFound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startErr error
		code     string
		message  string
	}{
		{
			name:     "missing harness-config",
			startErr: fmt.Errorf("provision: %w", &config.HarnessConfigNotFoundError{Name: "claude-custom", Searched: []string{"/home/x/.scion/harness-configs"}}),
			code:     ErrCodeNotFound,
			message:  `Failed to restart agent: harness-config "claude-custom" not found`,
		},
		{
			name:     "missing template",
			startErr: fmt.Errorf("provision: %w", &config.TemplateNotFoundError{Name: "web-tpl"}),
			code:     ErrCodeNotFound,
			message:  `Failed to restart agent: template "web-tpl" not found`,
		},
		{
			name:     "other not found",
			startErr: errors.New("container not found"),
			code:     ErrCodeAgentNotFound,
			message:  "Agent not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.manager.(*mockManager).startErr = tc.startErr
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", w.Body.String(), err)
			}
			if body.Error.Code != tc.code {
				t.Errorf("code = %q, want %q (body %s)", body.Error.Code, tc.code, w.Body.String())
			}
			if body.Error.Message != tc.message {
				t.Errorf("message = %q, want %q", body.Error.Message, tc.message)
			}
			if strings.Contains(w.Body.String(), "/home/x") {
				t.Errorf("the response names a broker path: %s", w.Body.String())
			}
		})
	}
}
