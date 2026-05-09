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

func TestGroveMarker_ShortUUID(t *testing.T) {
	tests := []struct {
		groveID string
		want    string
	}{
		{"550e8400-e29b-41d4-a716-446655440000", "550e8400"},
		{"abcdef12-3456-7890-abcd-ef1234567890", "abcdef12"},
		{"short", "short"},
		{"12345678", "12345678"},
	}
	for _, tt := range tests {
		m := GroveMarker{ProjectID: tt.groveID, GroveSlug: "test"}
		if got := m.ShortUUID(); got != tt.want {
			t.Errorf("ShortUUID(%q) = %q, want %q", tt.groveID, got, tt.want)
		}
	}
}

func TestGroveMarker_DirName(t *testing.T) {
	m := GroveMarker{
		ProjectID:   "550e8400-e29b-41d4-a716-446655440000",
		GroveName: "My Project",
		GroveSlug: "my-project",
	}
	want := "my-project__550e8400"
	if got := m.DirName(); got != want {
		t.Errorf("DirName() = %q, want %q", got, want)
	}
}

func TestGroveMarker_ExternalGrovePath(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	m := GroveMarker{
		ProjectID:   "550e8400-e29b-41d4-a716-446655440000",
		GroveName: "My Project",
		GroveSlug: "my-project",
	}

	got, err := m.ExternalGrovePath()
	if err != nil {
		t.Fatalf("ExternalGrovePath() error: %v", err)
	}

	want := filepath.Join(tmpHome, ".scion", "grove-configs", "my-project__550e8400", ".scion")
	if got != want {
		t.Errorf("ExternalGrovePath() = %q, want %q", got, want)
	}
}

func TestWriteAndReadGroveMarker(t *testing.T) {
	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, ".scion")

	original := &GroveMarker{
		ProjectID:   "550e8400-e29b-41d4-a716-446655440000",
		GroveName: "Test Project",
		GroveSlug: "test-project",
	}

	if err := WriteGroveMarker(markerPath, original); err != nil {
		t.Fatalf("WriteGroveMarker failed: %v", err)
	}

	// Verify it's a file, not a directory
	info, err := os.Stat(markerPath)
	if err != nil {
		t.Fatalf("marker file does not exist: %v", err)
	}
	if info.IsDir() {
		t.Fatal("marker should be a file, not a directory")
	}

	// Read it back
	got, err := ReadGroveMarker(markerPath)
	if err != nil {
		t.Fatalf("ReadGroveMarker failed: %v", err)
	}

	if got.ProjectID != original.ProjectID {
		t.Errorf("ProjectID = %q, want %q", got.ProjectID, original.ProjectID)
	}
	if got.GroveName != original.GroveName {
		t.Errorf("GroveName = %q, want %q", got.GroveName, original.GroveName)
	}
	if got.GroveSlug != original.GroveSlug {
		t.Errorf("GroveSlug = %q, want %q", got.GroveSlug, original.GroveSlug)
	}
}

func TestReadGroveMarker_InvalidContent(t *testing.T) {
	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, ".scion")

	// Write invalid marker (missing required fields)
	os.WriteFile(markerPath, []byte("grove-name: test\n"), 0644)

	_, err := ReadGroveMarker(markerPath)
	if err == nil {
		t.Fatal("expected error for invalid marker, got nil")
	}
}

func TestResolveGroveMarker(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, ".scion")

	marker := &GroveMarker{
		ProjectID:   "abcdef12-3456-7890-abcd-ef1234567890",
		GroveName: "My App",
		GroveSlug: "my-app",
	}
	WriteGroveMarker(markerPath, marker)

	resolved, err := ResolveGroveMarker(markerPath)
	if err != nil {
		t.Fatalf("ResolveGroveMarker failed: %v", err)
	}

	want := filepath.Join(tmpHome, ".scion", "grove-configs", "my-app__abcdef12", ".scion")
	if resolved != want {
		t.Errorf("ResolveGroveMarker() = %q, want %q", resolved, want)
	}
}

func TestIsGroveMarkerFile(t *testing.T) {
	tmpDir := t.TempDir()

	// File case
	filePath := filepath.Join(tmpDir, "marker")
	os.WriteFile(filePath, []byte("test"), 0644)
	if !IsProjectMarkerFile(filePath) {
		t.Error("expected file to be recognized as marker")
	}

	// Directory case
	dirPath := filepath.Join(tmpDir, "dir")
	os.MkdirAll(dirPath, 0755)
	if IsProjectMarkerFile(dirPath) {
		t.Error("expected directory to NOT be recognized as marker")
	}

	// Non-existent case
	if IsProjectMarkerFile(filepath.Join(tmpDir, "nonexistent")) {
		t.Error("expected non-existent path to NOT be recognized as marker")
	}
}

func TestIsOldStyleNonGitGrove(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create global .scion — should NOT be flagged
	globalDir := filepath.Join(tmpHome, ".scion")
	os.MkdirAll(globalDir, 0755)
	if IsOldStyleNonGitGrove(globalDir) {
		t.Error("global ~/.scion should NOT be flagged as old-style")
	}

	// Create a non-git project .scion dir — SHOULD be flagged
	nonGitDir := t.TempDir()
	scionDir := filepath.Join(nonGitDir, ".scion")
	os.MkdirAll(scionDir, 0755)
	if !IsOldStyleNonGitGrove(scionDir) {
		t.Error("non-git .scion directory should be flagged as old-style")
	}

	// Create a git project .scion dir — should NOT be flagged
	gitDir := t.TempDir()
	os.MkdirAll(filepath.Join(gitDir, ".git"), 0755)
	gitScionDir := filepath.Join(gitDir, ".scion")
	os.MkdirAll(gitScionDir, 0755)
	if IsOldStyleNonGitGrove(gitScionDir) {
		t.Error("git .scion directory should NOT be flagged as old-style")
	}

	// .scion as a file — should NOT be flagged
	fileDir := t.TempDir()
	markerFile := filepath.Join(fileDir, ".scion")
	os.WriteFile(markerFile, []byte("grove-id: test"), 0644)
	if IsOldStyleNonGitGrove(markerFile) {
		t.Error(".scion file should NOT be flagged as old-style")
	}
}

func TestExtractSlugFromExternalDir(t *testing.T) {
	tests := []struct {
		dirName string
		want    string
	}{
		{"my-project__abc12345", "my-project"},
		{"simple__12345678", "simple"},
		{"no-uuid-separator", ""},
		{"", ""},
		{"slug__", "slug"},
	}
	for _, tt := range tests {
		got := ExtractSlugFromExternalDir(tt.dirName)
		if got != tt.want {
			t.Errorf("ExtractSlugFromExternalDir(%q) = %q, want %q", tt.dirName, got, tt.want)
		}
	}
}

func TestFindProjectRoot_MarkerFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a project directory with a .scion marker file
	projectDir := t.TempDir()
	marker := &GroveMarker{
		ProjectID:   "550e8400-e29b-41d4-a716-446655440000",
		GroveName: "test-project",
		GroveSlug: "test-project",
	}
	WriteGroveMarker(filepath.Join(projectDir, ".scion"), marker)

	// Create the external directory so resolution works
	externalPath, _ := marker.ExternalGrovePath()
	os.MkdirAll(externalPath, 0755)

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	os.Chdir(projectDir)

	got, found := FindProjectRoot()
	if !found {
		t.Fatal("expected FindProjectRoot to find the marker file")
	}

	if got != externalPath {
		t.Errorf("FindProjectRoot() = %q, want %q", got, externalPath)
	}
}

func TestWriteAndReadGroveID(t *testing.T) {
	tmpDir := t.TempDir()
	scionDir := filepath.Join(tmpDir, ".scion")
	os.MkdirAll(scionDir, 0755)

	groveID := "550e8400-e29b-41d4-a716-446655440000"
	if err := WriteGroveID(scionDir, groveID); err != nil {
		t.Fatalf("WriteGroveID failed: %v", err)
	}

	got, err := ReadProjectID(scionDir)
	if err != nil {
		t.Fatalf("ReadProjectID failed: %v", err)
	}
	if got != groveID {
		t.Errorf("ReadProjectID() = %q, want %q", got, groveID)
	}
}

func TestReadGroveID_NotExist(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := ReadProjectID(tmpDir)
	if err == nil {
		t.Fatal("expected error for missing grove-id")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist error, got: %v", err)
	}
}

func TestGetGitGroveExternalConfigDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)
	WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	got, err := GetGitGroveExternalConfigDir(projectDir)
	if err != nil {
		t.Fatalf("GetGitGroveExternalConfigDir failed: %v", err)
	}

	want := filepath.Join(tmpHome, ".scion", "grove-configs", "my-repo__550e8400", ".scion")
	if got != want {
		t.Errorf("GetGitGroveExternalConfigDir() = %q, want %q", got, want)
	}
}

func TestGetGitGroveExternalConfigDir_NoGroveID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)

	got, err := GetGitGroveExternalConfigDir(projectDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for missing grove-id, got %q", got)
	}
}

func TestGetGitGroveExternalAgentsDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a simulated git grove .scion dir with grove-id
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)
	WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	got, err := GetGitGroveExternalAgentsDir(projectDir)
	if err != nil {
		t.Fatalf("GetGitGroveExternalAgentsDir failed: %v", err)
	}

	want := filepath.Join(tmpHome, ".scion", "grove-configs", "my-repo__550e8400", ".scion", "agents")
	if got != want {
		t.Errorf("GetGitGroveExternalAgentsDir() = %q, want %q", got, want)
	}
}

func TestGetGitGroveExternalAgentsDir_NoGroveID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a .scion dir without grove-id
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)

	got, err := GetGitGroveExternalAgentsDir(projectDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for missing grove-id, got %q", got)
	}
}

func TestGetAgentHomePath_GitGroveSplitStorage(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a git grove with grove-id (split storage)
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)
	WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	got := GetAgentHomePath(projectDir, "test-agent")
	want := filepath.Join(tmpHome, ".scion", "grove-configs", "my-repo__550e8400", ".scion", "agents", "test-agent", "home")
	if got != want {
		t.Errorf("GetAgentHomePath() = %q, want %q", got, want)
	}
}

func TestGetAgentHomePath_NoGroveID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Create a .scion dir without grove-id (fallback to in-repo)
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)

	got := GetAgentHomePath(projectDir, "test-agent")
	want := filepath.Join(projectDir, "agents", "test-agent", "home")
	if got != want {
		t.Errorf("GetAgentHomePath() = %q, want %q", got, want)
	}
}

func TestGetAgentDir_SharedWorkspaceUsesExternal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Git grove with grove-id (split storage)
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)
	WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	got := GetAgentDir(projectDir, "test-agent", true)
	want := filepath.Join(tmpHome, ".scion", "grove-configs", "my-repo__550e8400", ".scion", "agents", "test-agent")
	if got != want {
		t.Errorf("GetAgentDir(sharedWorkspace=true) = %q, want %q", got, want)
	}
}

func TestGetAgentDir_WorktreeModeStaysInGrove(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Git grove with grove-id (split storage), but caller is NOT shared-workspace
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)
	WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	got := GetAgentDir(projectDir, "test-agent", false)
	want := filepath.Join(projectDir, "agents", "test-agent")
	if got != want {
		t.Errorf("GetAgentDir(sharedWorkspace=false) = %q, want %q", got, want)
	}
}

func TestGetAgentDir_SharedWorkspaceWithoutGroveIDFallsBack(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// .scion dir without grove-id — split storage not initialized
	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)

	got := GetAgentDir(projectDir, "test-agent", true)
	want := filepath.Join(projectDir, "agents", "test-agent")
	if got != want {
		t.Errorf("GetAgentDir(sharedWorkspace=true, no grove-id) = %q, want %q", got, want)
	}
}

func TestResolveAgentDir_PrefersExternalWhenScionAgentJSONExists(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)
	WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	// Populate the external dir with a scion-agent.json (shared-workspace layout)
	extAgentDir := filepath.Join(tmpHome, ".scion", "grove-configs", "my-repo__550e8400", ".scion", "agents", "test-agent")
	if err := os.MkdirAll(extAgentDir, 0755); err != nil {
		t.Fatalf("mkdir external agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extAgentDir, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write scion-agent.json: %v", err)
	}

	got := ResolveAgentDir(projectDir, "test-agent")
	if got != extAgentDir {
		t.Errorf("ResolveAgentDir() = %q, want %q", got, extAgentDir)
	}
}

func TestResolveAgentDir_FallsBackToInGroveWhenExternalAbsent(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	os.MkdirAll(projectDir, 0755)
	WriteGroveID(projectDir, "550e8400-e29b-41d4-a716-446655440000")

	// Worktree-mode layout: scion-agent.json lives in-grove, only home/ is external.
	// (We do not create the external scion-agent.json.)
	got := ResolveAgentDir(projectDir, "test-agent")
	want := filepath.Join(projectDir, "agents", "test-agent")
	if got != want {
		t.Errorf("ResolveAgentDir() = %q, want %q", got, want)
	}
}

func TestIsHubContext(t *testing.T) {
	// Clear all hub env vars
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_GROVE_ID", "")

	if IsHubContext() {
		t.Error("expected IsHubContext() = false when no hub env vars are set")
	}

	// SCION_HUB_ENDPOINT alone
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.example.com")
	if !IsHubContext() {
		t.Error("expected IsHubContext() = true when SCION_HUB_ENDPOINT is set")
	}
	t.Setenv("SCION_HUB_ENDPOINT", "")

	// SCION_HUB_URL alone (legacy)
	t.Setenv("SCION_HUB_URL", "http://hub.example.com")
	if !IsHubContext() {
		t.Error("expected IsHubContext() = true when SCION_HUB_URL is set")
	}
	t.Setenv("SCION_HUB_URL", "")

	// SCION_GROVE_ID alone (broker-dispatched)
	t.Setenv("SCION_GROVE_ID", "grove-uuid-123")
	if !IsHubContext() {
		t.Error("expected IsHubContext() = true when SCION_GROVE_ID is set")
	}
}

func TestWriteWorkspaceMarker(t *testing.T) {
	tmpDir := t.TempDir()
	workspaceDir := filepath.Join(tmpDir, "workspace")
	os.MkdirAll(workspaceDir, 0755)

	err := WriteWorkspaceMarker(workspaceDir, "grove-id-123", "my-project", "my-project")
	if err != nil {
		t.Fatalf("WriteWorkspaceMarker failed: %v", err)
	}

	// Read back the marker
	markerPath := filepath.Join(workspaceDir, ".scion")
	marker, err := ReadGroveMarker(markerPath)
	if err != nil {
		t.Fatalf("ReadGroveMarker failed: %v", err)
	}

	if marker.ProjectID != "grove-id-123" {
		t.Errorf("ProjectID = %q, want %q", marker.ProjectID, "grove-id-123")
	}
	if marker.GroveName != "my-project" {
		t.Errorf("GroveName = %q, want %q", marker.GroveName, "my-project")
	}
	if marker.GroveSlug != "my-project" {
		t.Errorf("GroveSlug = %q, want %q", marker.GroveSlug, "my-project")
	}
}

func TestWriteWorkspaceMarker_MissingRequiredFields(t *testing.T) {
	tmpDir := t.TempDir()

	// Missing grove-id
	err := WriteWorkspaceMarker(tmpDir, "", "name", "slug")
	if err == nil {
		t.Error("expected error when grove-id is empty")
	}

	// Missing grove-slug
	err = WriteWorkspaceMarker(tmpDir, "id", "name", "")
	if err == nil {
		t.Error("expected error when grove-slug is empty")
	}
}

func TestGetGroveName_ExternalDir(t *testing.T) {
	// Test that GetProjectName extracts the slug from external directory names
	tests := []struct {
		dir  string
		want string
	}{
		{"/home/user/.scion/grove-configs/my-project__abc12345/.scion", "my-project"},
		{"/home/user/.scion/grove-configs/cool-app__12345678/.scion", "cool-app"},
		{"/home/user/projects/simple/.scion", "simple"},
	}
	for _, tt := range tests {
		got := GetProjectName(tt.dir)
		if got != tt.want {
			t.Errorf("GetProjectName(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}
