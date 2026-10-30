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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestStart_RerenderKeysFromInlineConfig: the start hands the manager the
// provision-rendered keys the hub's inline config lists as changed or
// cleared (ptone/scion#3982), and nothing else; a start without them (an
// older hub, or no change) hands none.
func TestStart_RerenderKeysFromInlineConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"changed and cleared", `{"inlineConfig": {"system_prompt": "new", "changed_fields": ["system_prompt", "skills", "model"], "cleared_fields": ["services", "max_turns"]}}`, []string{"services", "skills", "system_prompt"}},
		{"none listed", `{"inlineConfig": {"system_prompt": "new"}}`, nil},
		{"no inline config", `{}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Fatalf("Start calls = %d, want 1", mgr.startCalls)
			}
			if got := mgr.lastStartOpts.RerenderKeys; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("opts.RerenderKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestApplyInlineConfigUpdate_ChangedFieldsNotPersisted: the changed list
// is wire-only; the broker never writes it to scion-agent.json, while the
// changed values themselves are merged.
func TestApplyInlineConfigUpdate_ChangedFieldsNotPersisted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "changed-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude", SystemPrompt: "old"})
	srv.applyInlineConfigUpdate(context.Background(), agentName, projectDir, &api.ScionConfig{
		SystemPrompt:  "new",
		ChangedFields: []string{"system_prompt"},
	}, false)
	got, raw := readAgentConfig(t, cfgPath)
	if got.SystemPrompt != "new" {
		t.Errorf("system_prompt = %q, want the merged %q", got.SystemPrompt, "new")
	}
	if strings.Contains(raw, "changed_fields") {
		t.Errorf("changed_fields written to scion-agent.json: %s", raw)
	}
}
