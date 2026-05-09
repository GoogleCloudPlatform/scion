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

func TestGetGlobalDir(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	dir, err := GetGlobalDir()
	if err != nil {
		t.Fatalf("GetGlobalDir failed: %v", err)
	}
	expected := filepath.Join(tmpHome, GlobalDir)
	if dir != expected {
		t.Errorf("expected %q, got %q", expected, dir)
	}
}

func TestGetProjectName(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		path string
		want string
	}{
		{filepath.Join(tmpDir, "My Project", ".scion"), "my-project"},
		{filepath.Join(tmpDir, "simple", ".scion"), "simple"},
		{filepath.Join(tmpDir, "CamelCase", ".scion"), "camelcase"},
	}

	for _, tt := range tests {
		if err := os.MkdirAll(tt.path, 0755); err != nil {
			t.Fatal(err)
		}
		if got := GetProjectName(tt.path); got != tt.want {
			t.Errorf("GetProjectName(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestGetResolvedProjectDir(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	globalDir := filepath.Join(tmpHome, GlobalDir)
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		explicit string
		want     string
	}{
		{"home", globalDir},
		{"global", globalDir},
		// tmpHome contains a .scion dir (globalDir), so it should resolve to that
		{tmpHome, globalDir},
	}

	for _, tt := range tests {
		got, err := GetResolvedProjectDir(tt.explicit)
		if err != nil {
			t.Errorf("GetResolvedProjectDir(%q) error: %v", tt.explicit, err)
			continue
		}

		evalGot, _ := filepath.EvalSymlinks(got)
		evalWant, _ := filepath.EvalSymlinks(tt.want)

		if evalGot != evalWant {
			t.Errorf("GetResolvedProjectDir(%q) = %q, want %q", tt.explicit, evalGot, evalWant)
		}
	}
}

func TestGetResolvedProjectDir_WalkUp(t *testing.T) {
	// Create structure:
	// /tmp/grove/.scion
	// /tmp/grove/subdir/deep

	tmpProject := t.TempDir()
	scionDir := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	subDir := filepath.Join(tmpProject, "subdir", "deep")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	// Set HOME to a clean temp dir so we don't fall back to real global .scion
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}

	// Expect to find the .scion dir in the parent
	got, err := GetResolvedProjectDir("")
	if err != nil {
		t.Fatalf("GetResolvedProjectDir failed: %v", err)
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalScion, _ := filepath.EvalSymlinks(scionDir)

	if evalGot != evalScion {
		t.Errorf("Expected %q, got %q", evalScion, evalGot)
	}
}

func TestRequireProjectPath_ExplicitGlobal(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	globalDir := filepath.Join(tmpHome, GlobalDir)

	// Test "global" path
	got, isGlobal, err := RequireProjectPath("global")
	if err != nil {
		t.Fatalf("RequireProjectPath(global) error: %v", err)
	}
	if !isGlobal {
		t.Error("expected isGlobal=true")
	}
	if got != globalDir {
		t.Errorf("expected %q, got %q", globalDir, got)
	}

	// Test "home" path
	got, isGlobal, err = RequireProjectPath("home")
	if err != nil {
		t.Fatalf("RequireProjectPath(home) error: %v", err)
	}
	if !isGlobal {
		t.Error("expected isGlobal=true")
	}
	if got != globalDir {
		t.Errorf("expected %q, got %q", globalDir, got)
	}
}

func TestRequireProjectPath_NoProjectError(t *testing.T) {
	// Create a clean temp dir with no .scion
	tmpDir := t.TempDir()

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Ensure no hub context
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_GROVE_ID", "")

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Should error when no project found and no explicit path
	_, _, err := RequireProjectPath("")
	if err == nil {
		t.Fatal("expected error when no project found, got nil")
	}

	// Error message should suggest --global
	if !containsSubstring(err.Error(), "--global") && !containsSubstring(err.Error(), "global") {
		t.Errorf("error should suggest using --global, got: %v", err)
	}
}

func TestRequireProjectPath_HubContextFallback(t *testing.T) {
	// When SCION_HUB_ENDPOINT is set and no .scion exists,
	// RequireProjectPath should succeed (hub context fallback).
	tmpDir := t.TempDir()

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.example.com")

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	got, _, err := RequireProjectPath("")
	if err != nil {
		t.Fatalf("expected no error in hub context, got: %v", err)
	}

	// Should return a synthetic .scion path under CWD
	expected := filepath.Join(tmpDir, DotScion)
	if got != expected {
		t.Errorf("RequireProjectPath() = %q, want %q", got, expected)
	}
}

func TestFindProjectRoot_HubContextNoScion(t *testing.T) {
	// When SCION_HUB_ENDPOINT is set and no .scion exists anywhere,
	// FindProjectRoot should return a synthetic path.
	tmpDir := t.TempDir()

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.example.com")

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	got, found := FindProjectRoot()
	if !found {
		t.Fatal("expected FindProjectRoot to succeed in hub context")
	}

	expected := filepath.Join(tmpDir, DotScion)
	if got != expected {
		t.Errorf("FindProjectRoot() = %q, want %q", got, expected)
	}
}

func TestFindProjectRoot_HubContextNoScion_Disabled(t *testing.T) {
	// Without any hub env vars, FindProjectRoot should still fail
	// when no .scion exists.
	tmpDir := t.TempDir()

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_GROVE_ID", "")

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	_, found := FindProjectRoot()
	if found {
		t.Fatal("expected FindProjectRoot to fail without hub context")
	}
}

func TestFindProjectRoot_MarkerWithHubFallback(t *testing.T) {
	// When a .scion marker file exists but the external project-configs path
	// doesn't, and hub context is available, FindProjectRoot should succeed.
	tmpDir := t.TempDir()

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	// Set HOME to a dir where project-configs won't exist
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.example.com")

	// Write a valid marker file
	marker := &ProjectMarker{
		ProjectID:   "test-grove-id-1234",
		ProjectName: "test-project",
		ProjectSlug: "test-project",
	}
	WriteProjectMarker(filepath.Join(tmpDir, ".scion"), marker)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	got, found := FindProjectRoot()
	if !found {
		t.Fatal("expected FindProjectRoot to succeed with marker + hub context")
	}

	// External grove path doesn't exist on this filesystem, so with hub
	// context we should fall back to the synthetic workspace .scion path.
	expectedPath := filepath.Join(tmpDir, ".scion")
	if got != expectedPath {
		t.Errorf("FindProjectRoot() = %q, want %q", got, expectedPath)
	}
}

func TestRequireProjectPath_ProjectExists(t *testing.T) {
	// Create a project with .scion
	tmpProject := t.TempDir()
	scionDir := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	if err := os.Chdir(tmpProject); err != nil {
		t.Fatal(err)
	}

	// Should succeed when project found
	got, isGlobal, err := RequireProjectPath("")
	if err != nil {
		t.Fatalf("RequireProjectPath failed: %v", err)
	}
	if isGlobal {
		t.Error("expected isGlobal=false for project grove")
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalScion, _ := filepath.EvalSymlinks(scionDir)
	if evalGot != evalScion {
		t.Errorf("expected %q, got %q", evalScion, evalGot)
	}
}

func TestResolveProjectPath_ExplicitProjectRoot(t *testing.T) {
	// When passing a project root (not ending in .scion) that contains a .scion dir,
	// ResolveProjectPath should resolve to the .scion subdirectory.
	// This is the -g / --grove flag use case.

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create a project with .scion
	tmpProject := t.TempDir()
	projectScion := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(projectScion, 0755); err != nil {
		t.Fatal(err)
	}

	// Pass the project root (without .scion) as explicit path
	got, isGlobal, err := ResolveProjectPath(tmpProject)
	if err != nil {
		t.Fatalf("ResolveProjectPath(%q) error: %v", tmpProject, err)
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalExpected, _ := filepath.EvalSymlinks(projectScion)

	if evalGot != evalExpected {
		t.Errorf("ResolveProjectPath(%q) = %q, want %q", tmpProject, evalGot, evalExpected)
	}
	if isGlobal {
		t.Error("expected isGlobal=false for project grove")
	}
}

func TestResolveProjectPath_ExplicitDotScionPath(t *testing.T) {
	// When passing a path already ending in .scion, it should be used as-is.

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	tmpProject := t.TempDir()
	projectScion := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(projectScion, 0755); err != nil {
		t.Fatal(err)
	}

	got, _, err := ResolveProjectPath(projectScion)
	if err != nil {
		t.Fatalf("ResolveProjectPath(%q) error: %v", projectScion, err)
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalExpected, _ := filepath.EvalSymlinks(projectScion)

	if evalGot != evalExpected {
		t.Errorf("ResolveProjectPath(%q) = %q, want %q", projectScion, evalGot, evalExpected)
	}
}

func TestResolveProjectPath_ExplicitPathNoDotScion(t *testing.T) {
	// When passing a path that doesn't contain a .scion dir, it should be returned as-is.

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	tmpDir := t.TempDir()

	got, _, err := ResolveProjectPath(tmpDir)
	if err != nil {
		t.Fatalf("ResolveProjectPath(%q) error: %v", tmpDir, err)
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalExpected, _ := filepath.EvalSymlinks(tmpDir)

	if evalGot != evalExpected {
		t.Errorf("ResolveProjectPath(%q) = %q, want %q", tmpDir, evalGot, evalExpected)
	}
}

func TestRequireProjectPath_ExplicitProjectRoot(t *testing.T) {
	// RequireProjectPath should also resolve project root to .scion subdirectory.

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	tmpProject := t.TempDir()
	projectScion := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(projectScion, 0755); err != nil {
		t.Fatal(err)
	}

	got, isGlobal, err := RequireProjectPath(tmpProject)
	if err != nil {
		t.Fatalf("RequireProjectPath(%q) error: %v", tmpProject, err)
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalExpected, _ := filepath.EvalSymlinks(projectScion)

	if evalGot != evalExpected {
		t.Errorf("RequireProjectPath(%q) = %q, want %q", tmpProject, evalGot, evalExpected)
	}
	if isGlobal {
		t.Error("expected isGlobal=false for project grove")
	}
}

func TestResolveProjectPath_GlobalViaWalkUp(t *testing.T) {
	// Test that when FindProjectRoot walks up and finds ~/.scion,
	// it is correctly identified as the global grove (isGlobal=true)

	// Create a temp home with .scion
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	globalDir := filepath.Join(tmpHome, GlobalDir)
	if err := os.Mkdir(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory under home (simulating ~/Desktop/some-sub)
	subDir := filepath.Join(tmpHome, "Desktop", "some-sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}

	// ResolveProjectPath should walk up and find ~/.scion,
	// and recognize it as the global grove
	got, isGlobal, err := ResolveProjectPath("")
	if err != nil {
		t.Fatalf("ResolveProjectPath failed: %v", err)
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalGlobal, _ := filepath.EvalSymlinks(globalDir)

	if evalGot != evalGlobal {
		t.Errorf("expected path %q, got %q", evalGlobal, evalGot)
	}
	if !isGlobal {
		t.Errorf("expected isGlobal=true when global grove found via walk-up, got false")
	}
}

func TestResolveProjectPath_ProjectNotGlobal(t *testing.T) {
	// Test that a project grove (not at ~/) is correctly identified as NOT global

	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create a project with .scion outside of home
	tmpProject := t.TempDir()
	projectScion := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(projectScion, 0755); err != nil {
		t.Fatal(err)
	}

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	if err := os.Chdir(tmpProject); err != nil {
		t.Fatal(err)
	}

	got, isGlobal, err := ResolveProjectPath("")
	if err != nil {
		t.Fatalf("ResolveProjectPath failed: %v", err)
	}

	evalGot, _ := filepath.EvalSymlinks(got)
	evalProject, _ := filepath.EvalSymlinks(projectScion)

	if evalGot != evalProject {
		t.Errorf("expected path %q, got %q", evalProject, evalGot)
	}
	if isGlobal {
		t.Errorf("expected isGlobal=false for project grove, got true")
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstringHelper(s, substr))
}

func containsSubstringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
