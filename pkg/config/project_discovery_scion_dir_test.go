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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// setupScionDirTestHome points HOME at a fresh temporary directory with a
// global directory, and clears the hub environment variables that would
// otherwise leak into settings loading.
func setupScionDirTestHome(t *testing.T) string {
	t.Helper()
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_OTEL_ENDPOINT", "SCION_OTEL_PROTOCOL", "SCION_PROJECT_ID", "SCION_PROJECT"} {
		if val, ok := os.LookupEnv(e); ok {
			_ = os.Unsetenv(e)
			t.Cleanup(func() { _ = os.Setenv(e, val) })
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, GlobalDir), 0755); err != nil {
		t.Fatal(err)
	}
	return home
}

// writeExternalProject creates an external project config for workspace the
// way initExternalProject does: a marker file at <workspace>/.scion and a
// config under project-configs whose settings record workspace as the
// workspace path. It returns the config's .scion path.
func writeExternalProject(t *testing.T, workspace, projectID string) string {
	t.Helper()
	name := filepath.Base(workspace)
	marker := &ProjectMarker{ProjectID: projectID, ProjectName: name, ProjectSlug: api.Slugify(name)}
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectMarker(filepath.Join(workspace, DotScion), marker); err != nil {
		t.Fatal(err)
	}
	configPath, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configPath, "agents"), 0755); err != nil {
		t.Fatal(err)
	}
	settings := "workspace_path: " + workspace + "\nproject_id: " + projectID + "\n"
	if err := os.WriteFile(filepath.Join(configPath, "settings.yaml"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// discoveredByConfigPath returns the discovered project whose config path is
// configPath.
func discoveredByConfigPath(t *testing.T, configPath string) ProjectInfo {
	t.Helper()
	projects, err := DiscoverProjects()
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	for _, p := range projects {
		if filepath.Clean(p.ConfigPath) == filepath.Clean(configPath) {
			return p
		}
	}
	t.Fatalf("project with config %s not discovered (got %+v)", configPath, projects)
	return ProjectInfo{}
}

func isListedOrphan(t *testing.T, configPath string) bool {
	t.Helper()
	orphaned, err := FindOrphanedProjectConfigs()
	if err != nil {
		t.Fatalf("FindOrphanedProjectConfigs: %v", err)
	}
	for _, p := range orphaned {
		if filepath.Clean(p.ConfigPath) == filepath.Clean(configPath) {
			return true
		}
	}
	return false
}

// TestDiscoverProjects_StrayConfigForRepoScionDirIsOrphaned covers the
// config left behind when a provider path "<repo>/.scion" was initialized as
// "<repo>/.scion/.scion": its workspace path is the repository's own .scion
// directory, and the marker inside it points back at the config, so the
// workspace check alone accepts it.
func TestDiscoverProjects_StrayConfigForRepoScionDirIsOrphaned(t *testing.T) {
	home := setupScionDirTestHome(t)

	// A normal project, which must stay live.
	normalWorkspace := filepath.Join(home, "work", "normal")
	normalConfig := writeExternalProject(t, normalWorkspace, "11111111-0000-0000-0000-000000000000")

	// The repository's in-repo .scion directory, with its project-id file.
	repoScion := filepath.Join(home, "work", "repo", DotScion)
	if err := os.MkdirAll(filepath.Join(repoScion, "templates"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoScion, projectkeys.ProjectIDFile), []byte("22222222-0000-0000-0000-000000000000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	strayConfig := writeExternalProject(t, repoScion, "33333333-0000-0000-0000-000000000000")

	stray := discoveredByConfigPath(t, strayConfig)
	if stray.Status != ProjectStatusOrphaned {
		t.Errorf("stray config with workspace %s: status = %s, want %s", repoScion, stray.Status, ProjectStatusOrphaned)
	}
	if !isListedOrphan(t, strayConfig) {
		t.Errorf("stray config %s not returned by FindOrphanedProjectConfigs", strayConfig)
	}

	normal := discoveredByConfigPath(t, normalConfig)
	if normal.Status != ProjectStatusOK {
		t.Errorf("normal project: status = %s, want %s", normal.Status, ProjectStatusOK)
	}
	if isListedOrphan(t, normalConfig) {
		t.Errorf("normal project %s returned by FindOrphanedProjectConfigs", normalConfig)
	}

	// Pruning removes the stray config and leaves the normal one.
	if err := RemoveProjectConfig(strayConfig); err != nil {
		t.Fatalf("RemoveProjectConfig(stray): %v", err)
	}
	if _, err := os.Stat(filepath.Dir(strayConfig)); !os.IsNotExist(err) {
		t.Errorf("stray config dir still present after removal (stat err = %v)", err)
	}
	if _, err := os.Stat(normalConfig); err != nil {
		t.Errorf("normal project config affected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoScion, projectkeys.ProjectIDFile)); err != nil {
		t.Errorf("repository .scion directory affected: %v", err)
	}
}

// TestDiscoverProjects_StrayConfigForSettingsScionDirIsOrphaned covers a
// workspace path whose .scion directory holds a settings file (an older
// in-repo layout, or a project's own config directory).
func TestDiscoverProjects_StrayConfigForSettingsScionDirIsOrphaned(t *testing.T) {
	home := setupScionDirTestHome(t)

	scionDir := filepath.Join(home, "work", "legacy", DotScion)
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	strayConfig := writeExternalProject(t, scionDir, "44444444-0000-0000-0000-000000000000")

	if got := discoveredByConfigPath(t, strayConfig).Status; got != ProjectStatusOrphaned {
		t.Errorf("status = %s, want %s", got, ProjectStatusOrphaned)
	}
}

// TestDiscoverProjects_StrayConfigForGlobalDirIsOrphaned covers a workspace
// path that is the global directory itself, even with no settings file in
// it.
func TestDiscoverProjects_StrayConfigForGlobalDirIsOrphaned(t *testing.T) {
	home := setupScionDirTestHome(t)

	globalDir := filepath.Join(home, GlobalDir)
	strayConfig := writeExternalProject(t, globalDir, "55555555-0000-0000-0000-000000000000")

	if got := discoveredByConfigPath(t, strayConfig).Status; got != ProjectStatusOrphaned {
		t.Errorf("status = %s, want %s", got, ProjectStatusOrphaned)
	}
}

// TestDiscoverProjects_ProjectInOrdinaryDirNamedScionNotOrphaned covers a
// project created with 'scion init' in an ordinary, non-git directory that
// happens to be named .scion: its workspace path also ends in .scion, but the
// directory holds only the project's marker, so it stays live.
func TestDiscoverProjects_ProjectInOrdinaryDirNamedScionNotOrphaned(t *testing.T) {
	home := setupScionDirTestHome(t)

	workspace := filepath.Join(home, "data", DotScion)
	if err := os.MkdirAll(filepath.Join(workspace, "notes"), 0755); err != nil {
		t.Fatal(err)
	}
	configPath := writeExternalProject(t, workspace, "66666666-0000-0000-0000-000000000000")

	if got := discoveredByConfigPath(t, configPath).Status; got != ProjectStatusOK {
		t.Errorf("status = %s, want %s", got, ProjectStatusOK)
	}
	if isListedOrphan(t, configPath) {
		t.Errorf("project %s returned by FindOrphanedProjectConfigs", configPath)
	}
}
