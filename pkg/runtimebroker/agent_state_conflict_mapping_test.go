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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestAgentStateConflictMapsTo409 pins every broker path's mapping of an
// agent-state error (config.ErrAgentStateDirUnavailable or
// config.ErrAgentStateConflict, wrapped) from the manager to 409 conflict:
// synchronous create, provision-only create, start and restart.
func TestAgentStateConflictMapsTo409(t *testing.T) {
	for _, sentinel := range []error{config.ErrAgentStateDirUnavailable, config.ErrAgentStateConflict} {
		wrapped := fmt.Errorf("%w: test", sentinel)
		for _, tc := range []struct {
			name, path, body string
			arm              func(*mockManager)
		}{
			{"sync create", "/api/v1/agents", `{"name": "state-agent", "config": {"template": "claude"}}`,
				func(m *mockManager) { m.startErr = wrapped }},
			{"provision-only create", "/api/v1/agents", `{"name": "state-agent", "provisionOnly": true, "config": {"template": "claude"}}`,
				func(m *mockManager) { m.provisionErr = wrapped }},
			{"start", "/api/v1/agents/test-agent-1/start", `{}`,
				func(m *mockManager) { m.startErr = wrapped }},
			{"restart", "/api/v1/agents/test-agent-1/restart", `{}`,
				func(m *mockManager) { m.startErr = wrapped }},
		} {
			t.Run(sentinel.Error()+"/"+tc.name, func(t *testing.T) {
				srv := newTestServer(t)
				tc.arm(srv.manager.(*mockManager))
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				if w.Code != http.StatusConflict {
					t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

// TestClassifyStartError_AgentState: the async launch report distinguishes
// an unavailable state directory from an unusable state record.
func TestClassifyStartError_AgentState(t *testing.T) {
	ctx := context.Background()
	if code, _ := classifyStartError(ctx, fmt.Errorf("%w: x", config.ErrAgentStateDirUnavailable), ""); code != LaunchErrCodeAgentStateUnavailable {
		t.Errorf("unavailable dir: code %q", code)
	}
	if code, _ := classifyStartError(ctx, fmt.Errorf("%w: x", config.ErrAgentStateConflict), ""); code != LaunchErrCodeAgentStateConflict {
		t.Errorf("unusable record: code %q", code)
	}
}

// TestAgentInfoToResponse_HarnessConfigSource: the response carries the
// harness-config source the hub's applyBrokerAgentConfig records.
func TestAgentInfoToResponse_HarnessConfigSource(t *testing.T) {
	resp := AgentInfoToResponse(api.AgentInfo{Name: "a", HarnessConfigSource: "hub-hydrated"})
	if resp.HarnessConfigSource != "hub-hydrated" {
		t.Fatalf("HarnessConfigSource = %q", resp.HarnessConfigSource)
	}
}

// TestBuildStartContext_ReusesPrehydratedTemplate (final-broker #5): a create
// whose preflights already hydrated the template launches with that bundle
// and does not hydrate again. The hub connection here cannot hydrate (no
// fetcher), so a second hydration would fail the dispatch.
func TestBuildStartContext_ReusesPrehydratedTemplate(t *testing.T) {
	srv, _, _ := dispatchTestEnv(t, false)
	attachHubHCStub(t, srv)
	prehydrated := t.TempDir()
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "prehydrated-agent",
		Config: &CreateAgentConfig{
			Template:     "hub-template",
			TemplateID:   "tpl-id",
			TemplateHash: "sha256:tpl",
		},
		Prehydrated: prehydratedBundle{TemplateDone: true, TemplatePath: prehydrated},
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v (a second template hydration was attempted?)", err)
	}
	if sc.Opts.Template != prehydrated {
		t.Fatalf("launch template = %q, want the prehydrated %q", sc.Opts.Template, prehydrated)
	}
}
