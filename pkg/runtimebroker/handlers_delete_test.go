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
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// ============================================================================
// Project Cleanup Endpoint Tests
// ============================================================================

func TestDeleteProject_RemovesDirectory(t *testing.T) {
	srv := newTestServer(t)

	// Create a temporary projects directory structure
	tmpHome := t.TempDir()
	projectsDir := filepath.Join(tmpHome, ".scion", "projects")
	projectDir := filepath.Join(projectsDir, "test-project")
	scionDir := filepath.Join(projectDir, ".scion")

	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatalf("failed to create test project dir: %v", err)
	}

	// Write a dummy file so we can verify deletion
	dummyFile := filepath.Join(scionDir, "settings.yaml")
	if err := os.WriteFile(dummyFile, []byte("test: true"), 0o644); err != nil {
		t.Fatalf("failed to write dummy file: %v", err)
	}

	// Override HOME so config.GetGlobalDir resolves to our temp dir
	t.Setenv("HOME", tmpHome)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/test-project", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify directory was removed
	if _, err := os.Stat(projectDir); !os.IsNotExist(err) {
		t.Errorf("expected project directory to be removed, but it still exists")
	}
}

func TestDeleteProject_NonExistent_Returns204(t *testing.T) {
	srv := newTestServer(t)

	tmpHome := t.TempDir()
	// Create the projects parent but NOT the specific project directory
	projectsDir := filepath.Join(tmpHome, ".scion", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("failed to create projects dir: %v", err)
	}

	t.Setenv("HOME", tmpHome)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/nonexistent-project", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for non-existent project, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProject_PathTraversal_Blocked(t *testing.T) {
	srv := newTestServer(t)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Attempt path traversal
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/..%2F..%2Fetc", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal attempt, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFindAgentInHubManagedProjects(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create hub-managed project structure with an agent directory
	projectSlug := "my-project"
	scionDir := filepath.Join(tmpHome, ".scion", "projects", projectSlug, ".scion")
	agentDir := filepath.Join(scionDir, "agents", "test-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}

	// Should find the agent in the hub-managed project
	result, _ := findAgentInHubManagedProjects("test-agent", "")
	if result != scionDir {
		t.Errorf("expected %q, got %q", scionDir, result)
	}

	// Should not find a non-existent agent
	result, _ = findAgentInHubManagedProjects("nonexistent-agent", "")
	if result != "" {
		t.Errorf("expected empty string for nonexistent agent, got %q", result)
	}

	// Should handle missing projects directory gracefully
	t.Setenv("HOME", t.TempDir())
	result, _ = findAgentInHubManagedProjects("test-agent", "")
	if result != "" {
		t.Errorf("expected empty string when projects dir missing, got %q", result)
	}
}

func TestDeleteAgent_HubManagedProject_NoContainer(t *testing.T) {
	// Verify that deleting an agent in a hub-managed project resolves the correct
	// project path even when the container doesn't exist (e.g. created-only
	// agent, pruned container).
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{
		agents: []api.AgentInfo{}, // No containers
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create hub-managed project with an agent directory and config file
	projectSlug := "hub-project"
	scionDir := filepath.Join(tmpHome, ".scion", "projects", projectSlug, ".scion")
	agentName := "orphaned-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("failed to create agent dir: %v", err)
	}
	// Write a scion-agent.json so it looks like a real agent
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Send delete request — no container exists for this agent
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/agents/"+agentName+"?deleteFiles=true&removeBranch=false", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// Should succeed (204)
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the mock manager's Delete was called with the correct project path
	if mgr.DeleteCalls() != 1 {
		t.Fatalf("expected 1 Delete call, got %d", mgr.DeleteCalls())
	}
	if mgr.LastDeleteProjectPath() != scionDir {
		t.Errorf("expected projectPath %q, got %q", scionDir, mgr.LastDeleteProjectPath())
	}
	if mgr.LastDeleteAgentID() != agentName {
		t.Errorf("expected agentID %q, got %q", agentName, mgr.LastDeleteAgentID())
	}
}

// TestDeleteAgent_RejectsTraversalName is the regression anchor for
// deleteAgent's containment check: an agent id must be a single, clean path
// element that resolves under the project's agents root before any file
// operation runs, checked independently of whatever the request routing
// already filtered.
func TestDeleteAgent_RejectsTraversalName(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"

	mgr := &mockManager{agents: []api.AgentInfo{}}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectSlug := "hub-project"
	scionDir := filepath.Join(tmpHome, ".scion", "projects", projectSlug, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatalf("failed to create scionDir/agents: %v", err)
	}
	// Sentinel representing project-level state that must remain untouched
	// when the request's agent id fails containment.
	markerPath := filepath.Join(scionDir, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("failed to write marker: %v", err)
	}

	// Decodes to id="..", action="".
	req := httptest.NewRequest(http.MethodDelete,
		"/api/v1/agents/..%2F?deleteFiles=true&removeBranch=false", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Fatalf("expected no Delete calls, got %d", mgr.DeleteCalls())
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("project directory must survive untouched, but stat failed: %v", err)
	}
	if _, err := os.Stat(scionDir); err != nil {
		t.Fatalf("project .scion directory must survive untouched, but stat failed: %v", err)
	}
}

// TestDeleteAgent_ContainerOnlyRemovalSurvivesBadName is the converse of
// TestDeleteAgent_RejectsTraversalName: a request that never asks for file
// cleanup (the default -- no deleteFiles, no softDelete) must still remove a
// legacy agent recorded under a bad name, by container ID alone, with the
// containment check never entering the picture. Runtime.Delete acts on
// containerID, not on any path built from the name, so nothing here is
// unsafe to allow. This is the independent, non-file-touching removal path
// that keeps the containment fix from stranding a pre-existing bad-named
// agent: the check added for the traversal case above must gate only
// requests that actually touch a file, not every request naming a bad
// agent.
func TestDeleteAgent_ContainerOnlyRemovalSurvivesBadName(t *testing.T) {
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{Name: "..", ContainerID: "legacy-bad-name-container"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt, config.RuntimeBuildRecord{})

	// No deleteFiles, no softDelete: a plain container-record removal.
	// Decodes to id="..", action="".
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/..%2F", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 1 {
		t.Fatalf("expected 1 Delete call (container-only removal must still succeed), got %d", mgr.DeleteCalls())
	}
	if mgr.LastDeleteFiles() {
		t.Fatal("expected deleteFiles to be false for a container-only removal")
	}
}

// TestDeleteAgent_InvalidIDWithDeleteFilesRejected proves that a DELETE with
// deleteFiles=true and an id that is not a single valid path element is
// still rejected: deleteAgent's own gate (isSingleCleanPathElement plus
// containment) is reached and enforced even though handleAgentByID exempts
// a bare DELETE from its own gate. Unlike
// TestDeleteAgent_ContainerOnlyRemovalSurvivesBadName, this request does
// touch files, so deleteAgent's own validation must still apply.
func TestDeleteAgent_InvalidIDWithDeleteFilesRejected(t *testing.T) {
	mgr := &mockManager{
		agents: []api.AgentInfo{
			{Name: `a\b`, ContainerID: "bad-name-container"},
		},
	}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(DefaultServerConfig(), mgr, rt, config.RuntimeBuildRecord{})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/a%5Cb?deleteFiles=true", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if mgr.DeleteCalls() != 0 {
		t.Fatalf("expected no Delete call for a rejected id, got %d", mgr.DeleteCalls())
	}
}
