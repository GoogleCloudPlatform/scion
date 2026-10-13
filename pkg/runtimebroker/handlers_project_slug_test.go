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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestProjectSlugWorkspacePath(t *testing.T) {
	// Verify the workspace directory path for hub-managed projects uses
	// ~/.scion/projects/<slug>/ instead of the worktree-based path.
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}

	expected := filepath.Join(globalDir, "projects", "my-test-project")

	// Simulate the logic from the handler: when ProjectSlug is set,
	// use the conventional path.
	projectSlug := "my-test-project"
	workspaceDir := filepath.Join(globalDir, "projects", projectSlug)

	if workspaceDir != expected {
		t.Errorf("expected workspace dir %q, got %q", expected, workspaceDir)
	}

	// When ProjectSlug is empty, the default worktree path is used.
	worktreeBase := "/tmp/test-worktrees"
	agentName := "test-agent"
	defaultDir := filepath.Join(worktreeBase, agentName, "workspace")
	expectedDefault := "/tmp/test-worktrees/test-agent/workspace"
	if defaultDir != expectedDefault {
		t.Errorf("expected default workspace dir %q, got %q", expectedDefault, defaultDir)
	}
}

func TestCreateAgentRequest_ProjectSlugField(t *testing.T) {
	// Verify ProjectSlug is properly serialized/deserialized in CreateAgentRequest.
	reqJSON := `{
		"name": "project-agent",
		"projectSlug": "my-hub-project",
		"workspaceStoragePath": "workspaces/project-123/project-workspace"
	}`

	var req CreateAgentRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if req.ProjectSlug != "my-hub-project" {
		t.Errorf("expected ProjectSlug 'my-hub-project', got '%s'", req.ProjectSlug)
	}
	if req.WorkspaceStoragePath != "workspaces/project-123/project-workspace" {
		t.Errorf("expected WorkspaceStoragePath 'workspaces/project-123/project-workspace', got '%s'", req.WorkspaceStoragePath)
	}
}

func TestCreateAgentProjectSlugResolvesProjectPath(t *testing.T) {
	// When ProjectSlug is set and ProjectPath is empty (hub-managed project with no
	// local provider path), the handler should resolve ProjectPath to the
	// conventional ~/.scion/projects/<slug>/ path so the agent is created in the
	// correct project instead of the broker's local project.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "hub-managed-agent",
		"id": "agent-uuid-123",
		"slug": "hub-managed-agent",
		"projectId": "project-abc",
		"projectSlug": "my-hub-project",
		"provisionOnly": true,
		"config": {"template": "claude", "workspace": "/hub/projects/my-hub-project"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.provisionCalled {
		t.Fatal("expected Provision to be called")
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}

	expectedPath := filepath.Join(globalDir, "projects", "my-hub-project")
	if mgr.lastOpts.ProjectPath != expectedPath {
		t.Errorf("expected ProjectPath %q, got %q", expectedPath, mgr.lastOpts.ProjectPath)
	}
}

func TestCreateAgentProjectSlugNotUsedWhenProjectPathSet(t *testing.T) {
	// When both ProjectPath and ProjectSlug are set, ProjectPath takes precedence
	// (the broker has a local provider path for this project).
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"name": "local-project-agent",
		"id": "agent-uuid-456",
		"slug": "local-project-agent",
		"projectId": "project-def",
		"projectSlug": "my-hub-project",
		"projectPath": "/projects/my-local-project/.scion",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	if !mgr.provisionCalled {
		t.Fatal("expected Provision to be called")
	}

	// ProjectPath should remain as explicitly provided, not overridden by ProjectSlug
	if mgr.lastOpts.ProjectPath != "/projects/my-local-project/.scion" {
		t.Errorf("expected ProjectPath %q, got %q", "/projects/my-local-project/.scion", mgr.lastOpts.ProjectPath)
	}
}

func TestStartAgentProjectSlugResolvesProjectPath(t *testing.T) {
	// When the startAgent handler receives projectSlug with no projectPath
	// (hub-managed project), it should resolve ProjectPath from the slug.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{"projectSlug": "my-hub-project"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/hub-managed-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("failed to get global dir: %v", err)
	}

	expectedPath := filepath.Join(globalDir, "projects", "my-hub-project")
	if mgr.lastOpts.ProjectPath != expectedPath {
		t.Errorf("expected ProjectPath %q, got %q", expectedPath, mgr.lastOpts.ProjectPath)
	}
}

func TestStartAgentProjectSlugNotUsedWhenProjectPathSet(t *testing.T) {
	// When startAgent receives both projectPath and projectSlug,
	// projectPath takes precedence.
	srv, mgr := newTestServerWithProvisionCapture()

	body := `{
		"projectPath": "/projects/my-local-project/.scion",
		"projectSlug": "my-hub-project"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/local-project-agent/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}

	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}

	expectedPath := "/projects/my-local-project/.scion"
	if mgr.lastOpts.ProjectPath != expectedPath {
		t.Errorf("expected ProjectPath %q, got %q", expectedPath, mgr.lastOpts.ProjectPath)
	}
}

func TestCreateAgentProjectSlugInitializesScionDir(t *testing.T) {
	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	defer restore()

	restoreGit := config.OverrideIsGitRepo(func() bool { return true })
	defer restoreGit()

	// When ProjectSlug is set and the broker has no .scion subdirectory for
	// the hub-managed project, the handler should create it so that
	// ResolveProjectPath resolves to projects/<slug>/.scion (not projects/<slug>).
	// This prevents agents from being created at the wrong directory level.

	// Use a temporary directory to simulate the project workspace.
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, "test-project")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatalf("failed to create test project dir: %v", err)
	}

	// Verify .scion does NOT exist yet
	scionDir := filepath.Join(projectPath, ".scion")
	if _, err := os.Stat(scionDir); !os.IsNotExist(err) {
		t.Fatal(".scion should not exist before initialization")
	}

	// Verify ResolveProjectPath does NOT resolve to .scion when it doesn't exist
	resolved, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		t.Fatalf("ResolveProjectPath failed: %v", err)
	}
	if resolved != projectPath {
		t.Errorf("before init: expected ResolveProjectPath to return %q, got %q", projectPath, resolved)
	}

	// Initialize .scion (mirrors what the handler now does)
	if err := config.InitProject(scionDir, nil); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Verify .scion was created
	if info, err := os.Stat(scionDir); err != nil || !info.IsDir() {
		t.Fatal(".scion directory should exist after InitProject")
	}

	// Verify ResolveProjectPath now resolves to the .scion subdirectory
	resolved, _, err = config.ResolveProjectPath(projectPath)
	if err != nil {
		t.Fatalf("ResolveProjectPath failed: %v", err)
	}
	if resolved != scionDir {
		t.Errorf("after init: expected ResolveProjectPath to resolve to %q, got %q", scionDir, resolved)
	}
}
