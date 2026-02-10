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
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGetDefaultSettingsData_OSSpecific(t *testing.T) {
	data, err := GetDefaultSettingsData()
	if err != nil {
		t.Fatalf("GetDefaultSettingsData failed: %v", err)
	}

	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("Failed to unmarshal settings: %v", err)
	}

	localProfile, ok := settings.Profiles["local"]
	if !ok {
		t.Fatal("local profile not found in default settings")
	}

	expectedRuntime := "docker"
	if runtime.GOOS == "darwin" {
		expectedRuntime = "container"
	}

	if localProfile.Runtime != expectedRuntime {
		t.Errorf("expected runtime %q for OS %q, got %q", expectedRuntime, runtime.GOOS, localProfile.Runtime)
	}
}

func TestGenerateGroveIDForDir_NoGitRepo(t *testing.T) {
	// Create a non-git directory
	tmpDir := t.TempDir()

	// GenerateGroveIDForDir should return a UUID
	id := GenerateGroveIDForDir(tmpDir)
	if id == "" {
		t.Error("expected non-empty grove ID")
	}

	// Should look like a UUID (contains hyphens, ~36 chars)
	if !strings.Contains(id, "-") || len(id) != 36 {
		t.Errorf("expected UUID format, got: %q", id)
	}
}

func TestIsInsideGrove(t *testing.T) {
	// Create a directory with .scion
	tmpGrove := t.TempDir()
	scionDir := filepath.Join(tmpGrove, ".scion")
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

	// When in the grove directory
	if err := os.Chdir(tmpGrove); err != nil {
		t.Fatal(err)
	}
	if !IsInsideGrove() {
		t.Error("expected IsInsideGrove=true when in grove directory")
	}

	// When in a subdirectory of the grove
	subDir := filepath.Join(tmpGrove, "subdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}
	if !IsInsideGrove() {
		t.Error("expected IsInsideGrove=true when in subdirectory of grove")
	}

	// When outside any grove
	outsideDir := t.TempDir()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	if IsInsideGrove() {
		t.Error("expected IsInsideGrove=false when outside any grove")
	}
}

func TestGetEnclosingGrovePath(t *testing.T) {
	// Create a directory with .scion
	tmpGrove := t.TempDir()
	scionDir := filepath.Join(tmpGrove, ".scion")
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

	// Create a subdirectory
	subDir := filepath.Join(tmpGrove, "subdir", "deep")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	// When in the subdirectory, should find the enclosing grove
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}

	grovePath, rootDir, found := GetEnclosingGrovePath()
	if !found {
		t.Fatal("expected to find enclosing grove")
	}

	evalGrovePath, _ := filepath.EvalSymlinks(grovePath)
	evalScionDir, _ := filepath.EvalSymlinks(scionDir)
	if evalGrovePath != evalScionDir {
		t.Errorf("expected grovePath=%q, got %q", evalScionDir, evalGrovePath)
	}

	evalRootDir, _ := filepath.EvalSymlinks(rootDir)
	evalTmpGrove, _ := filepath.EvalSymlinks(tmpGrove)
	if evalRootDir != evalTmpGrove {
		t.Errorf("expected rootDir=%q, got %q", evalTmpGrove, evalRootDir)
	}
}

func TestGetEnclosingGrovePath_NotFound(t *testing.T) {
	// Create a directory without .scion
	tmpDir := t.TempDir()

	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	_, _, found := GetEnclosingGrovePath()
	if found {
		t.Error("expected found=false when no enclosing grove")
	}
}
