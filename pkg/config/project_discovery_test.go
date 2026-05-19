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

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverProjects_EmptyHome(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(projects))
	}
}

func TestDiscoverProjects_GlobalOnly(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	globalDir := filepath.Join(tmpHome, ".scion")
	os.MkdirAll(filepath.Join(globalDir, "agents"), 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}
	if projects[0].Type != ProjectTypeGlobal {
		t.Errorf("expected global type, got %s", projects[0].Type)
	}
	if projects[0].Name != "global" {
		t.Errorf("expected name 'global', got %s", projects[0].Name)
	}
	if projects[0].Status != ProjectStatusOK {
		t.Errorf("expected status ok, got %s", projects[0].Status)
	}
}

func TestDiscoverProjects_ExternalProject(t *testing.T) {
	// Unset Hub environment variables to avoid pollution
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_GROVE_ID", "SCION_HUB_GROVE_ID", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL"} {
		if val, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			defer os.Setenv(e, val)
		}
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create global dir
	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create an external project config
	projectConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "myproject__abcd1234", ".scion")
	os.MkdirAll(filepath.Join(projectConfigDir, "agents", "agent1"), 0755)

	// Create a workspace directory with a marker file
	workspace := filepath.Join(tmpHome, "projects", "myproject")
	os.MkdirAll(workspace, 0755)

	// Write marker file
	marker := &ProjectMarker{
		ProjectID:   "abcd1234-0000-0000-0000-000000000000",
		ProjectName: "myproject",
		ProjectSlug: "myproject",
	}
	WriteProjectMarker(filepath.Join(workspace, DotScion), marker)

	// Write settings with workspace_path
	settingsContent := "workspace_path: " + workspace + "\ngrove_id: abcd1234-0000-0000-0000-000000000000\n"
	os.WriteFile(filepath.Join(projectConfigDir, "settings.yaml"), []byte(settingsContent), 0644)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should find global + external
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}

	ext := projects[1]
	if ext.Type != ProjectTypeExternal {
		t.Errorf("expected external type, got %s", ext.Type)
	}
	if ext.Name != "myproject" {
		t.Errorf("expected name 'myproject', got %s", ext.Name)
	}
	if ext.Status != ProjectStatusOK {
		t.Errorf("expected status ok, got %s", ext.Status)
	}
	if ext.AgentCount != 1 {
		t.Errorf("expected 1 agent, got %d", ext.AgentCount)
	}
	if ext.WorkspacePath != workspace {
		t.Errorf("expected workspace %s, got %s", workspace, ext.WorkspacePath)
	}
}

func TestDiscoverProjects_OrphanedExternal(t *testing.T) {
	// Unset Hub environment variables to avoid pollution
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_GROVE_ID", "SCION_HUB_GROVE_ID", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL"} {
		if val, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			defer os.Setenv(e, val)
		}
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create external project config pointing to a non-existent workspace
	projectConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "gone-project__deadbeef", ".scion")
	os.MkdirAll(filepath.Join(projectConfigDir, "agents"), 0755)

	settingsContent := "workspace_path: /nonexistent/workspace\n"
	os.WriteFile(filepath.Join(projectConfigDir, "settings.yaml"), []byte(settingsContent), 0644)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the external project
	var ext *ProjectInfo
	for i := range projects {
		if projects[i].Type == ProjectTypeExternal {
			ext = &projects[i]
			break
		}
	}
	if ext == nil {
		t.Fatal("expected to find external project")
	}
	if ext.Status != ProjectStatusOrphaned {
		t.Errorf("expected status orphaned, got %s", ext.Status)
	}
}

func TestFindOrphanedProjectConfigs(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create one orphaned project
	orphanedDir := filepath.Join(tmpHome, ".scion", "project-configs", "orphan__12345678", ".scion")
	os.MkdirAll(filepath.Join(orphanedDir, "agents"), 0755)
	os.WriteFile(filepath.Join(orphanedDir, "settings.yaml"), []byte("workspace_path: /does/not/exist\n"), 0644)

	orphaned, err := FindOrphanedProjectConfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned, got %d", len(orphaned))
	}
	if orphaned[0].Name != "orphan" {
		t.Errorf("expected name 'orphan', got %s", orphaned[0].Name)
	}
}

func TestRemoveProjectConfig(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	configDir := filepath.Join(tmpHome, ".scion", "project-configs", "test__aabbccdd", ".scion")
	os.MkdirAll(configDir, 0755)
	os.WriteFile(filepath.Join(configDir, "settings.yaml"), []byte(""), 0644)

	if err := RemoveProjectConfig(configDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	parentDir := filepath.Dir(configDir)
	if _, err := os.Stat(parentDir); !os.IsNotExist(err) {
		t.Errorf("expected directory to be removed, but it still exists")
	}
}

func TestRemoveProjectConfig_SafetyCheck(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Try to remove something outside project-configs — should fail
	outsideDir := filepath.Join(tmpHome, "projects", "important")
	os.MkdirAll(outsideDir, 0755)

	err := RemoveProjectConfig(outsideDir)
	if err == nil {
		t.Error("expected error when removing path outside project-configs")
	}
}

func TestReconnectProject(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create external project config
	configDir := filepath.Join(tmpHome, ".scion", "project-configs", "proj__11223344", ".scion")
	os.MkdirAll(configDir, 0755)
	os.WriteFile(filepath.Join(configDir, "settings.yaml"), []byte("workspace_path: /old/path\n"), 0644)

	newPath := filepath.Join(tmpHome, "new-workspace")
	os.MkdirAll(newPath, 0755)

	if err := ReconnectProject(configDir, newPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify settings updated
	settings, err := LoadSettings(configDir)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if settings.WorkspacePath != newPath {
		t.Errorf("expected workspace_path %s, got %s", newPath, settings.WorkspacePath)
	}
}

func TestCountAgents(t *testing.T) {
	tmpDir := t.TempDir()
	agentsDir := filepath.Join(tmpDir, "agents")
	os.MkdirAll(filepath.Join(agentsDir, "agent-a"), 0755)
	os.MkdirAll(filepath.Join(agentsDir, "agent-b"), 0755)
	os.MkdirAll(filepath.Join(agentsDir, ".hidden"), 0755)

	count := countAgents(agentsDir)
	if count != 2 {
		t.Errorf("expected 2 agents, got %d", count)
	}
}

func TestCountAgents_NonExistentDir(t *testing.T) {
	count := countAgents("/nonexistent/agents")
	if count != 0 {
		t.Errorf("expected 0 agents, got %d", count)
	}
}

func TestListAgentNames(t *testing.T) {
	tmpDir := t.TempDir()
	agentsDir := filepath.Join(tmpDir, "agents")
	os.MkdirAll(filepath.Join(agentsDir, "agent-a"), 0755)
	os.MkdirAll(filepath.Join(agentsDir, "agent-b"), 0755)
	os.MkdirAll(filepath.Join(agentsDir, ".hidden"), 0755)

	names := ListAgentNames(agentsDir)
	if len(names) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(names))
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["agent-a"] || !nameSet["agent-b"] {
		t.Errorf("expected agent-a and agent-b, got %v", names)
	}
}

func TestListAgentNames_NonExistentDir(t *testing.T) {
	names := ListAgentNames("/nonexistent/agents")
	if names != nil {
		t.Errorf("expected nil, got %v", names)
	}
}

func TestProjectInfo_AgentsDir(t *testing.T) {
	tests := []struct {
		name     string
		project  ProjectInfo
		expected string
	}{
		{
			name: "external project",
			project: ProjectInfo{
				Type:       ProjectTypeExternal,
				ConfigPath: "/home/user/.scion/project-configs/proj__abc123/.scion",
			},
			expected: "/home/user/.scion/project-configs/proj__abc123/.scion/agents",
		},
		{
			name: "git project",
			project: ProjectInfo{
				Type:       ProjectTypeGit,
				ConfigPath: "/home/user/.scion/project-configs/repo__def456/.scion",
			},
			expected: "/home/user/.scion/project-configs/repo__def456/.scion/agents",
		},
		{
			name: "global project",
			project: ProjectInfo{
				Type:       ProjectTypeGlobal,
				ConfigPath: "/home/user/.scion",
			},
			expected: "/home/user/.scion/agents",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.project.AgentsDir()
			if got != tt.expected {
				t.Errorf("AgentsDir() = %s, want %s", got, tt.expected)
			}
		})
	}
}

func TestDiscoverProjects_StaleExternalAfterMarkerRecreate(t *testing.T) {
	// Unset Hub environment variables to avoid pollution
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_GROVE_ID", "SCION_HUB_GROVE_ID", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL"} {
		if val, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			defer os.Setenv(e, val)
		}
	}

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	workspace := filepath.Join(tmpHome, "projects", "myproject")
	os.MkdirAll(workspace, 0755)

	// Simulate the old project-config (from a previous init)
	oldConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "myproject__aaaaaaaa", ".scion")
	os.MkdirAll(filepath.Join(oldConfigDir, "agents"), 0755)
	os.WriteFile(filepath.Join(oldConfigDir, "settings.yaml"),
		[]byte("workspace_path: "+workspace+"\ngrove_id: aaaaaaaa-0000-0000-0000-000000000000\n"), 0644)

	// Simulate new project-config (from re-init after marker was deleted)
	newConfigDir := filepath.Join(tmpHome, ".scion", "project-configs", "myproject__bbbbbbbb", ".scion")
	os.MkdirAll(filepath.Join(newConfigDir, "agents"), 0755)
	os.WriteFile(filepath.Join(newConfigDir, "settings.yaml"),
		[]byte("workspace_path: "+workspace+"\ngrove_id: bbbbbbbb-0000-0000-0000-000000000000\n"), 0644)

	// Workspace marker now points to the new project-config
	marker := &ProjectMarker{
		ProjectID:   "bbbbbbbb-0000-0000-0000-000000000000",
		ProjectName: "myproject",
		ProjectSlug: "myproject",
	}
	WriteProjectMarker(filepath.Join(workspace, DotScion), marker)

	// The old config should be orphaned because the marker resolves to the new config
	orphaned, err := FindOrphanedProjectConfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned project-config, got %d", len(orphaned))
	}
	if orphaned[0].Name != "myproject" {
		t.Errorf("expected orphaned name 'myproject', got %s", orphaned[0].Name)
	}
	// The orphaned one should be the old config
	if orphaned[0].ConfigPath != oldConfigDir {
		t.Errorf("expected orphaned config path %s, got %s", oldConfigDir, orphaned[0].ConfigPath)
	}
}

func TestDiscoverProjects_GitProjectExternal(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a git project external directory (agents only, no .scion subdir)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "myrepo__aabb1122")
	agentsDir := filepath.Join(projectDir, "agents", "worker1", "home")
	os.MkdirAll(agentsDir, 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Type == ProjectTypeGit {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project")
	}
	if gitProject.Name != "myrepo" {
		t.Errorf("expected name 'myrepo', got %s", gitProject.Name)
	}
	if gitProject.AgentCount != 1 {
		t.Errorf("expected 1 agent, got %d", gitProject.AgentCount)
	}
	if gitProject.Status != ProjectStatusOK {
		t.Errorf("expected status ok for git project with agents, got %s", gitProject.Status)
	}
}

func TestDiscoverProjects_GitProjectExternalEmptyAgents(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a git project external directory with an empty agents dir (no .scion subdir)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "leftover__deadbeef")
	os.MkdirAll(filepath.Join(projectDir, "agents"), 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Type == ProjectTypeGit {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project")
	}
	if gitProject.Status != ProjectStatusOrphaned {
		t.Errorf("expected orphaned status for git project with empty agents dir, got %s", gitProject.Status)
	}
}

func TestDiscoverProjects_GitProjectWithExternalConfig(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a git project in the new layout: .scion/ (config + agents)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "newrepo__ccdd1122")
	scionDir := filepath.Join(projectDir, ".scion")
	agentsDir := filepath.Join(scionDir, "agents", "worker1", "home")
	os.MkdirAll(scionDir, 0755)
	os.MkdirAll(agentsDir, 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Name == "newrepo" {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project with external config")
	}
	if gitProject.Type != ProjectTypeGit {
		t.Errorf("expected ProjectTypeGit, got %s", gitProject.Type)
	}
	if gitProject.Status != ProjectStatusOK {
		t.Errorf("expected status ok, got %s", gitProject.Status)
	}
	if gitProject.AgentCount != 1 {
		t.Errorf("expected 1 agent, got %d", gitProject.AgentCount)
	}
	if gitProject.ConfigPath != scionDir {
		t.Errorf("expected ConfigPath %q, got %q", scionDir, gitProject.ConfigPath)
	}
	// AgentsDir() should point to .scion/agents/ inside the project config dir
	wantAgentsDir := filepath.Join(scionDir, "agents")
	if got := gitProject.AgentsDir(); got != wantAgentsDir {
		t.Errorf("AgentsDir() = %q, want %q", got, wantAgentsDir)
	}
}

func TestDiscoverProjects_GitProjectWithExternalConfigUsesWorkspaceMarkerProjectID(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpHome); err != nil {
		t.Fatalf("Setenv HOME failed: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME failed: %v", err)
		}
	}()

	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatalf("mkdir .scion: %v", err)
	}

	if projectID, ok := os.LookupEnv("SCION_GROVE_ID"); ok {
		if err := os.Unsetenv("SCION_GROVE_ID"); err != nil {
			t.Fatalf("Unsetenv SCION_GROVE_ID failed: %v", err)
		}
		defer func() {
			if err := os.Setenv("SCION_GROVE_ID", projectID); err != nil {
				t.Fatalf("restore SCION_GROVE_ID failed: %v", err)
			}
		}()
	}

	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "newrepo__ccdd1122")
	scionDir := filepath.Join(projectDir, ".scion")
	agentsDir := filepath.Join(scionDir, "agents", "worker1", "home")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}

	workspaceDir := filepath.Join(tmpHome, ".scion", "projects", "newrepo")
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := WriteWorkspaceMarker(workspaceDir, "3c619ec9-517e-4321-8c6a-4757f6a95607", "newrepo", "newrepo"); err != nil {
		t.Fatalf("WriteWorkspaceMarker failed: %v", err)
	}

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var gitProject *ProjectInfo
	for i := range projects {
		if projects[i].Name == "newrepo" {
			gitProject = &projects[i]
			break
		}
	}
	if gitProject == nil {
		t.Fatal("expected to find git project with external config")
	}
	if gitProject.ProjectID != "3c619ec9-517e-4321-8c6a-4757f6a95607" {
		t.Fatalf("ProjectID = %q, want %q", gitProject.ProjectID, "3c619ec9-517e-4321-8c6a-4757f6a95607")
	}
	if gitProject.WorkspacePath != workspaceDir {
		t.Fatalf("WorkspacePath = %q, want %q", gitProject.WorkspacePath, workspaceDir)
	}
}

func TestDiscoverProjects_ProjectConfigNoScionNoAgents(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create a project-config directory with no .scion and no agents subdir
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "empty-leftover__aabb1122")
	os.MkdirAll(projectDir, 0755)

	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var found *ProjectInfo
	for i := range projects {
		if projects[i].Name == "empty-leftover" {
			found = &projects[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected to find orphaned project-config dir with no .scion and no agents")
	}
	if found.Status != ProjectStatusOrphaned {
		t.Errorf("expected orphaned status, got %s", found.Status)
	}
}

func TestFindOrphanedProjectConfigs_IncludesEmptyGitProjects(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755)

	// Create leftover project-config with empty agents dir (typical test residue)
	projectDir := filepath.Join(tmpHome, ".scion", "project-configs", "ws-test__abcd1234")
	os.MkdirAll(filepath.Join(projectDir, "agents"), 0755)

	orphaned, err := FindOrphanedProjectConfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orphaned) != 1 {
		t.Fatalf("expected 1 orphaned, got %d", len(orphaned))
	}
	if orphaned[0].Name != "ws-test" {
		t.Errorf("expected name 'ws-test', got %s", orphaned[0].Name)
	}
}
