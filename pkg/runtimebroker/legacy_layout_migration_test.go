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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// noopMigrationReporter discards every config.MigrateLegacyGlobalLayout
// report; the test below only cares about the resulting filesystem state.
type noopMigrationReporter struct{}

func (noopMigrationReporter) Migrated(old, new string, tracked bool)      {}
func (noopMigrationReporter) Conflict(old, new, detail string)            {}
func (noopMigrationReporter) Skipped(old, reason, manual string)          {}
func (noopMigrationReporter) EnvIgnored(name, replacement string)         {}
func (noopMigrationReporter) PrecedenceChanged(path, value, other string) {}

// TestLegacyLayoutMigration_FindAgentAndStopSucceed proves that an agent
// created before config.MigrateLegacyGlobalLayout moved the project out of
// ~/.scion/groves (leaving a per-entry symlink at the old path) still has
// its container's scion.project_path label pointing at the old, pre-rename
// absolute path. After the migration runs, findAgentInHubManagedProjects
// must find the project under its new, canonical location, and both stop
// and delete must still succeed for that agent, resolving its stale label
// path through the symlink the migration leaves behind.
func TestLegacyLayoutMigration_FindAgentAndStopSucceed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const projectID = "44444444-dddd-dddd-dddd-444444444444"
	const agentName = "dev"

	// Pre-migration layout: the project lives under the legacy groves/ root.
	legacyScionDir := filepath.Join(home, ".scion", "groves", "proj", ".scion")
	if err := os.MkdirAll(filepath.Join(legacyScionDir, "agents", agentName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(legacyScionDir, projectID); err != nil {
		t.Fatal(err)
	}
	agentHome := config.GetAgentHomePath(legacyScionDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), []byte(`{"name":"`+agentName+`","phase":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	config.MigrateLegacyGlobalLayout(filepath.Join(home, ".scion"), noopMigrationReporter{})

	canonicalScionDir := filepath.Join(home, ".scion", "projects", "proj", ".scion")
	if _, err := os.Stat(canonicalScionDir); err != nil {
		t.Fatalf("migration did not create the canonical project dir: %v", err)
	}

	// findAgentInHubManagedProjects scans the canonical directory directly:
	// the migration has already moved the real content there before this
	// function is ever reached in a real boot.
	resolved, err := findAgentInHubManagedProjects(agentName, projectID)
	if err != nil {
		t.Fatalf("findAgentInHubManagedProjects: %v", err)
	}
	if resolved != canonicalScionDir {
		t.Errorf("findAgentInHubManagedProjects: got %q, want %q", resolved, canonicalScionDir)
	}

	// A container created before the migration still carries the old,
	// pre-rename absolute path in its scion.project_path label (and, being
	// pre-existing, carries no project-id label at all). It must still
	// resolve to this project through the symlink MigrateLegacyGlobalLayout
	// left at the old name, so both stop and delete succeed for it.
	mgr := &filteringMockManager{}
	mgr.agents = []api.AgentInfo{legacyEntry(agentName, "cid-legacy", legacyScionDir)}
	srv, _ := newScopeTestServer(t, mgr)

	stopReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+projectID, nil)
	stopRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(stopRec, stopReq)
	if stopRec.Code != http.StatusAccepted {
		t.Fatalf("stop: expected 202, got %d: %s", stopRec.Code, stopRec.Body.String())
	}
	if mgr.StopCalls() != 1 || mgr.LastStopAgentID() != "cid-legacy" {
		t.Errorf("stop: expected 1 call on cid-legacy, got %d call(s) on %q", mgr.StopCalls(), mgr.LastStopAgentID())
	}

	rec := doDelete(t, srv, agentName, "projectId="+projectID+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if mgr.LastDeleteContainerID() != "cid-legacy" {
		t.Errorf("delete: deleted container %q, want cid-legacy", mgr.LastDeleteContainerID())
	}
	if mgr.LastDeleteProjectPath() != legacyScionDir {
		t.Errorf("delete: file deletion project path %q, want %q", mgr.LastDeleteProjectPath(), legacyScionDir)
	}
}
