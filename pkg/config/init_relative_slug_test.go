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
	"strings"
	"testing"
)

// setupExternalLayoutInit isolates HOME, mocks runtime detection and forces
// the external (non-git) layout. It returns the temp HOME.
func setupExternalLayoutInit(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	mockRuntimeDetection(t, "docker")
	t.Cleanup(OverrideIsGitRepo(func() bool { return false }))
	return home
}

// mkProjectDir creates <fresh temp dir>/<name> and returns it.
func mkProjectDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

func TestInitProject_ExternalRelativeTargetMatchesAbsolute(t *testing.T) {
	setupExternalLayoutInit(t)

	// Relative ".scion" target, run from inside the project directory.
	relDir := mkProjectDir(t, "My Project")
	t.Chdir(relDir)
	if err := InitProject(DotScion, GetMockHarnesses()); err != nil {
		t.Fatalf("InitProject(relative) failed: %v", err)
	}
	relMarker, err := ReadProjectMarker(filepath.Join(relDir, DotScion))
	if err != nil {
		t.Fatalf("ReadProjectMarker(relative): %v", err)
	}

	// Absolute target for a directory with the same name, run from elsewhere.
	absDir := mkProjectDir(t, "My Project")
	t.Chdir(t.TempDir())
	if err := InitProject(filepath.Join(absDir, DotScion), GetMockHarnesses()); err != nil {
		t.Fatalf("InitProject(absolute) failed: %v", err)
	}
	absMarker, err := ReadProjectMarker(filepath.Join(absDir, DotScion))
	if err != nil {
		t.Fatalf("ReadProjectMarker(absolute): %v", err)
	}

	if relMarker.ProjectName != "My Project" || relMarker.ProjectSlug != "my-project" {
		t.Errorf("relative target: got name %q slug %q, want %q %q",
			relMarker.ProjectName, relMarker.ProjectSlug, "My Project", "my-project")
	}
	if relMarker.ProjectName != absMarker.ProjectName || relMarker.ProjectSlug != absMarker.ProjectSlug {
		t.Errorf("relative target gave name %q slug %q; absolute target gave name %q slug %q",
			relMarker.ProjectName, relMarker.ProjectSlug, absMarker.ProjectName, absMarker.ProjectSlug)
	}

	// The external directory is named after the slug.
	extPath, err := relMarker.ExternalProjectPath()
	if err != nil {
		t.Fatalf("ExternalProjectPath: %v", err)
	}
	if base := filepath.Base(filepath.Dir(extPath)); !strings.HasPrefix(base, "my-project__") {
		t.Errorf("external project directory %q, want prefix %q", base, "my-project__")
	}
}

func TestInitProject_ExternalEmptySlugIsRefused(t *testing.T) {
	home := setupExternalLayoutInit(t)

	dir := mkProjectDir(t, "!!!")
	t.Chdir(dir)

	err := InitProject(DotScion, GetMockHarnesses())
	if err == nil {
		t.Fatal("InitProject succeeded for a directory name that gives an empty slug; want an error")
	}
	if !strings.Contains(err.Error(), "empty project slug") {
		t.Errorf("error %q does not mention the empty project slug", err)
	}

	// Nothing is written: no marker in the project directory and no
	// external project directory under HOME.
	if _, statErr := os.Lstat(filepath.Join(dir, DotScion)); !os.IsNotExist(statErr) {
		t.Errorf("%s was written (stat err %v)", filepath.Join(dir, DotScion), statErr)
	}
	configs := filepath.Join(home, GlobalDir, ProjectConfigsDir)
	if entries, readErr := os.ReadDir(configs); readErr == nil && len(entries) > 0 {
		t.Errorf("%s has entries %v; want none", configs, entries)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("ReadDir(%s): %v", configs, readErr)
	}
}
