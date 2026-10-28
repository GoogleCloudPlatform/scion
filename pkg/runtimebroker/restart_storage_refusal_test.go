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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// A restart of an agent whose profile carries a removed storage override is
// refused with 422 storage_migration_required BEFORE the stop: the agent is
// left running, and nothing is started (ptone/scion#6371).
func TestRestartAgent_StorageRefusalLeavesAgentRunning(t *testing.T) {
	f := newLifecycleFixture(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\n" +
		"active_profile: local\n" +
		"profiles:\n" +
		"    local:\n" +
		"        runtime: docker\n" +
		"        shared_dir_storage_backend: nfs\n" +
		"runtimes:\n" +
		"    docker:\n" +
		"        type: docker\n"
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	const name = "running-agent"
	f.defaultMgr.agents = []api.AgentInfo{lifecycleAgent(name, f.projectPath, "")}

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{
		"hubEndpoint": lifecycleHubEndpoint,
		"profile":     "local",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not an ErrorResponse: %v: %s", err, w.Body.String())
	}
	if resp.Error.Code != ErrCodeStorageMigrationRequired || !strings.Contains(resp.Error.Message, "profiles.local.shared_dir_storage_backend") {
		t.Fatalf("want %s naming the key, got %+v", ErrCodeStorageMigrationRequired, resp.Error)
	}
	if f.defaultMgr.stopCalls != 0 {
		t.Errorf("stop calls = %d, want 0: a refused restart must leave the agent running", f.defaultMgr.stopCalls)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
}
