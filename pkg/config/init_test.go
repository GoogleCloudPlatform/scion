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

	"gopkg.in/yaml.v3"
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

func TestGetDefaultSettingsDataYAML_OSSpecific(t *testing.T) {
	data, err := GetDefaultSettingsDataYAML()
	if err != nil {
		t.Fatalf("GetDefaultSettingsDataYAML failed: %v", err)
	}

	var settings Settings
	if err := yaml.Unmarshal(data, &settings); err != nil {
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

func TestGenerateProjectIDForDir_NoGitRepo(t *testing.T) {
	// Create a non-git directory
	tmpDir := t.TempDir()

	// GenerateProjectIDForDir should return a UUID
	id := GenerateProjectIDForDir(tmpDir)
	if id == "" {
		t.Error("expected non-empty project ID")
	}

	// Should look like a UUID (contains hyphens, ~36 chars)
	if !strings.Contains(id, "-") || len(id) != 36 {
		t.Errorf("expected UUID format, got: %q", id)
	}
}

func TestIsInsideProject(t *testing.T) {
	// Unset Hub context to avoid synthetic project root detection
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_GROVE_ID", "SCION_PROJECT_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			_ = os.Unsetenv(e)
			defer func() { _ = os.Setenv(e, val) }()
		}
	}

	// Create a directory with .scion
	tmpProject := t.TempDir()
	scionDir := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	origWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(origWd) }()

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// When in the project directory
	if err := os.Chdir(tmpProject); err != nil {
		t.Fatal(err)
	}
	if !IsInsideProject() {
		t.Error("expected IsInsideProject=true when in project directory")
	}

	// When in a subdirectory of the project
	subDir := filepath.Join(tmpProject, "subdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}
	if !IsInsideProject() {
		t.Error("expected IsInsideProject=true when in subdirectory of project")
	}

	// When outside any project
	outsideDir := t.TempDir()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	if IsInsideProject() {
		t.Error("expected IsInsideProject=false when outside any project")
	}
}

func TestGetEnclosingProjectPath(t *testing.T) {
	// Create a directory with .scion
	tmpProject := t.TempDir()
	scionDir := filepath.Join(tmpProject, ".scion")
	if err := os.Mkdir(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	origWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(origWd) }()

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Create a subdirectory
	subDir := filepath.Join(tmpProject, "subdir", "deep")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	// When in the subdirectory, should find the enclosing project
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}

	projectPath, rootDir, found := GetEnclosingProjectPath()
	if !found {
		t.Fatal("expected to find enclosing project")
	}

	evalProjectPath, _ := filepath.EvalSymlinks(projectPath)
	evalScionDir, _ := filepath.EvalSymlinks(scionDir)
	if evalProjectPath != evalScionDir {
		t.Errorf("expected projectPath=%q, got %q", evalScionDir, evalProjectPath)
	}

	evalRootDir, _ := filepath.EvalSymlinks(rootDir)
	evalTmpProject, _ := filepath.EvalSymlinks(tmpProject)
	if evalRootDir != evalTmpProject {
		t.Errorf("expected rootDir=%q, got %q", evalTmpProject, evalRootDir)
	}
}

func TestGetEnclosingProjectPath_NotFound(t *testing.T) {
	// Create a directory without .scion
	tmpDir := t.TempDir()

	origWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(origWd) }()

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	_, _, found := GetEnclosingProjectPath()
	if found {
		t.Error("expected found=false when no enclosing project")
	}
}

func TestSeedAgnosticTemplate(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "default")

	if err := SeedAgnosticTemplate(targetDir, false); err != nil {
		t.Fatalf("SeedAgnosticTemplate failed: %v", err)
	}

	// Verify all expected files exist (including home/ directory files)
	expectedFiles := []string{"scion-agent.yaml", "agents.md", "system-prompt.md", "home/.tmux.conf", "home/.zshrc"}
	for _, f := range expectedFiles {
		path := filepath.Join(targetDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s to exist", f)
		}
	}

	// Verify scion-agent.yaml has no harness field and no default_harness_config
	// (default_harness_config should be set at the settings level, not in the template)
	data, err := os.ReadFile(filepath.Join(targetDir, "scion-agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "harness: claude") || strings.Contains(content, "harness: gemini") {
		t.Error("agnostic template should not contain harness-specific field")
	}
	if strings.Contains(content, "default_harness_config:") {
		t.Error("agnostic template should not contain default_harness_config (set in settings instead)")
	}
}

func TestSeedAgnosticTemplate_NoOverwrite(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "default")
	_ = os.MkdirAll(targetDir, 0755)

	// Write a custom file first
	customContent := "custom content"
	_ = os.WriteFile(filepath.Join(targetDir, "agents.md"), []byte(customContent), 0644)

	// Write a custom home/.tmux.conf
	homeDir := filepath.Join(targetDir, "home")
	_ = os.MkdirAll(homeDir, 0755)
	_ = os.WriteFile(filepath.Join(homeDir, ".tmux.conf"), []byte(customContent), 0644)

	// Seed without force — should not overwrite
	if err := SeedAgnosticTemplate(targetDir, false); err != nil {
		t.Fatalf("SeedAgnosticTemplate failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "agents.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != customContent {
		t.Error("SeedAgnosticTemplate overwrote existing file when force=false")
	}

	// Verify home/.tmux.conf was not overwritten either
	data, err = os.ReadFile(filepath.Join(homeDir, ".tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != customContent {
		t.Error("SeedAgnosticTemplate overwrote home/.tmux.conf when force=false")
	}
}

func TestSeedAgnosticTemplate_ForceOverwrite(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "default")
	_ = os.MkdirAll(targetDir, 0755)

	// Write custom files first
	_ = os.WriteFile(filepath.Join(targetDir, "agents.md"), []byte("custom"), 0644)
	homeDir := filepath.Join(targetDir, "home")
	_ = os.MkdirAll(homeDir, 0755)
	_ = os.WriteFile(filepath.Join(homeDir, ".tmux.conf"), []byte("custom"), 0644)

	// Seed with force — should overwrite
	if err := SeedAgnosticTemplate(targetDir, true); err != nil {
		t.Fatalf("SeedAgnosticTemplate failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "agents.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "custom" {
		t.Error("SeedAgnosticTemplate did not overwrite existing file when force=true")
	}

	// Verify home/.tmux.conf was also overwritten
	data, err = os.ReadFile(filepath.Join(homeDir, ".tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "custom" {
		t.Error("SeedAgnosticTemplate did not overwrite home/.tmux.conf when force=true")
	}
}

func TestInitProject_EmptyTemplatesDir(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")
	mockIsGitRepo(t, true)

	// Override HOME for global templates and external project-config dirs
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Use explicit targetDir to avoid CWD-based resolution issues
	projectDir := filepath.Join(tmpDir, "project", DotScion)

	if err := InitProject(projectDir, GetMockHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Templates always live in the in-repo projectDir (for git projects) or in the
	// external config dir (for non-git projects). Since tests run inside a git repo,
	// projectDir is used directly.
	templatesDir := filepath.Join(projectDir, "templates")
	if info, err := os.Stat(templatesDir); err != nil || !info.IsDir() {
		t.Fatalf("expected templates/ directory to exist at %s", templatesDir)
	}

	// Verify templates/default/ does NOT exist (default template lives in global project only)
	defaultTplDir := filepath.Join(projectDir, "templates", "default")
	if _, err := os.Stat(defaultTplDir); !os.IsNotExist(err) {
		t.Errorf("expected templates/default/ to NOT exist at project level, but it does at %s", defaultTplDir)
	}
}

func TestInitProject_NoHarnessConfigs(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")
	mockIsGitRepo(t, true)

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectDir := filepath.Join(tmpDir, "project", DotScion)

	if err := InitProject(projectDir, GetMockHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Verify harness-configs directory was NOT created at project level
	harnessConfigsDir := filepath.Join(projectDir, "harness-configs")
	if _, err := os.Stat(harnessConfigsDir); !os.IsNotExist(err) {
		t.Errorf("expected harness-configs directory to NOT exist at project level, but it does at %s", harnessConfigsDir)
	}

	// Verify per-harness template directories were NOT created
	for _, name := range []string{"gemini", "claude"} {
		perHarnessTplDir := filepath.Join(projectDir, "templates", name)
		if _, err := os.Stat(perHarnessTplDir); !os.IsNotExist(err) {
			t.Errorf("expected per-harness template dir %s to NOT exist at project level", perHarnessTplDir)
		}
	}
}

func TestInitMachine_SeedsAll(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)

	// Verify settings.yaml was created
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		t.Error("expected settings.yaml to exist in global directory")
	}

	// Verify default agnostic template was created (including home/ files)
	defaultTplDir := filepath.Join(globalDir, "templates", "default")
	expectedFiles := []string{"scion-agent.yaml", "agents.md", "system-prompt.md", "home/.tmux.conf", "home/.zshrc"}
	for _, f := range expectedFiles {
		path := filepath.Join(defaultTplDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected default template file %s to exist at %s", f, path)
		}
	}

	// Verify per-harness template directories were NOT created
	for _, name := range []string{"gemini", "claude"} {
		perHarnessTplDir := filepath.Join(globalDir, "templates", name)
		if _, err := os.Stat(perHarnessTplDir); !os.IsNotExist(err) {
			t.Errorf("expected per-harness template dir %s to NOT exist", perHarnessTplDir)
		}
	}

	// Verify agents directory was created
	agentsDir := filepath.Join(globalDir, "agents")
	if _, err := os.Stat(agentsDir); os.IsNotExist(err) {
		t.Error("expected agents directory to exist in global directory")
	}

	// Verify broker ID was pre-populated in settings
	settings, err := LoadSettings(globalDir)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	if settings.Hub == nil || settings.Hub.BrokerID == "" {
		t.Error("expected broker ID to be pre-populated in global settings")
	}
	// Should look like a UUID
	if settings.Hub != nil && settings.Hub.BrokerID != "" {
		if !strings.Contains(settings.Hub.BrokerID, "-") || len(settings.Hub.BrokerID) != 36 {
			t.Errorf("expected UUID format for broker ID, got: %q", settings.Hub.BrokerID)
		}
	}
}

func TestInitMachine_DoesNotOverwriteExistingBrokerID(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// First init to seed settings and broker ID
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("first InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	settings, err := LoadSettings(globalDir)
	if err != nil {
		t.Fatalf("failed to load settings: %v", err)
	}
	originalBrokerID := settings.Hub.BrokerID

	// Second init should not overwrite the broker ID
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("second InitMachine failed: %v", err)
	}

	settings, err = LoadSettings(globalDir)
	if err != nil {
		t.Fatalf("failed to reload settings: %v", err)
	}
	if settings.Hub.BrokerID != originalBrokerID {
		t.Errorf("expected broker ID to be preserved across re-init, got %q (was %q)",
			settings.Hub.BrokerID, originalBrokerID)
	}
}

func TestInitGlobal_IsAliasForInitMachine(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// InitGlobal should work the same as InitMachine
	if err := InitGlobal(GetMockHarnesses()); err != nil {
		t.Fatalf("InitGlobal failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)

	// Verify the same structure as InitMachine
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		t.Error("expected settings.yaml to exist in global directory")
	}

	defaultTplDir := filepath.Join(globalDir, "templates", "default")
	if _, err := os.Stat(defaultTplDir); os.IsNotExist(err) {
		t.Error("expected default template directory to exist")
	}
}

func TestInitMachine_WithImageRegistry(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	opts := InitMachineOpts{ImageRegistry: "ghcr.io/testorg"}
	if err := InitMachine(GetMockHarnesses(), opts); err != nil {
		t.Fatalf("InitMachine with ImageRegistry failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	vs, _, err := LoadEffectiveSettings(globalDir)
	if err != nil {
		t.Fatalf("LoadEffectiveSettings failed: %v", err)
	}
	if vs.ImageRegistry != "ghcr.io/testorg" {
		t.Errorf("expected image_registry 'ghcr.io/testorg', got %q", vs.ImageRegistry)
	}
}

func TestInitMachine_FailsWithNoRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetectionNone(t)

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	err := InitMachine(GetMockHarnesses())
	if err == nil {
		t.Fatal("expected InitMachine to fail when no container runtime is available")
	}
	if !strings.Contains(err.Error(), "no supported container runtime found") {
		t.Errorf("expected error about missing runtime, got: %v", err)
	}
}

func TestInitMachine_SkipRuntimeCheck(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetectionNone(t)

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	err := InitMachine(GetMockHarnesses(), InitMachineOpts{SkipRuntimeCheck: true})
	if err != nil {
		t.Fatalf("expected InitMachine with SkipRuntimeCheck to succeed, got: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	settingsPath := GetSettingsPath(globalDir)
	if settingsPath == "" {
		t.Fatal("expected settings.yaml to be created")
	}

	// Verify the settings file contains a valid runtime (not empty)
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read settings: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "runtime: \n") || strings.Contains(content, "runtime:  ") {
		t.Fatal("expected a valid runtime in settings.yaml when SkipRuntimeCheck is true, got empty runtime")
	}
	if !strings.Contains(content, "runtime: docker") {
		t.Errorf("expected runtime to default to 'docker' when SkipRuntimeCheck is true, got:\n%s", content)
	}
}

func TestInitProject_FailsWithNoRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetectionNone(t)

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectDir := filepath.Join(tmpDir, "project", DotScion)
	err := InitProject(projectDir, GetMockHarnesses())
	if err == nil {
		t.Fatal("expected InitProject to fail when no container runtime is available")
	}
	if !strings.Contains(err.Error(), "no supported container runtime found") {
		t.Errorf("expected error about missing runtime, got: %v", err)
	}
}

func TestInitMachine_UsesDetectedRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "podman")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	// Read the seeded settings and verify runtime is "podman"
	globalDir := filepath.Join(tmpDir, GlobalDir)
	data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	if err != nil {
		t.Fatalf("failed to read settings.yaml: %v", err)
	}

	var settings Settings
	if err := yaml.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal settings: %v", err)
	}

	localProfile, ok := settings.Profiles["local"]
	if !ok {
		t.Fatal("local profile not found in seeded settings")
	}
	if localProfile.Runtime != "podman" {
		t.Errorf("expected runtime 'podman' from detection, got %q", localProfile.Runtime)
	}
}

func TestInitProject_UsesDetectedRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "podman")
	mockIsGitRepo(t, true)

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	projectDir := filepath.Join(tmpDir, "project", DotScion)
	if err := InitProject(projectDir, GetMockHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Project settings should not contain profiles or runtimes; those live in global settings.
	// For git projects settings.yaml is in the external config dir; use GetProjectConfigDir
	// to find the canonical location regardless of project type.
	configDir := GetProjectConfigDir(projectDir)
	data, err := os.ReadFile(filepath.Join(configDir, "settings.yaml"))
	if err != nil {
		t.Fatalf("failed to read settings.yaml: %v", err)
	}

	var settings VersionedSettings
	if err := yaml.Unmarshal(data, &settings); err != nil {
		t.Fatalf("failed to unmarshal settings: %v", err)
	}

	if len(settings.Profiles) != 0 {
		t.Errorf("expected project settings.yaml to have no profiles block, got %d profiles", len(settings.Profiles))
	}
	if len(settings.Runtimes) != 0 {
		t.Errorf("expected project settings.yaml to have no runtimes block, got %d runtimes", len(settings.Runtimes))
	}
	if settings.ActiveProfile != "local" {
		t.Errorf("expected active_profile 'local', got %q", settings.ActiveProfile)
	}
}

func TestInitMachine_RestoresDeletedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// First init seeds everything
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("first InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	defaultTplDir := filepath.Join(globalDir, "templates", "default")

	// Use scion-agent.yaml to test restore — it's guaranteed to have content.
	// (agents.md is intentionally empty since its content moved to workspace skills.)
	targetPath := filepath.Join(defaultTplDir, "scion-agent.yaml")
	originalContent, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read scion-agent.yaml: %v", err)
	}
	if len(originalContent) == 0 {
		t.Fatal("expected scion-agent.yaml to have content")
	}

	// Delete the file
	if err := os.Remove(targetPath); err != nil {
		t.Fatalf("failed to delete scion-agent.yaml: %v", err)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatal("expected scion-agent.yaml to be deleted")
	}

	// Re-run init — should restore the deleted file
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("second InitMachine failed: %v", err)
	}

	restoredContent, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("scion-agent.yaml was not restored after re-init: %v", err)
	}
	if string(restoredContent) != string(originalContent) {
		t.Error("restored scion-agent.yaml content does not match original embedded content")
	}
}

func TestInitMachine_RestoresDeletedCommonFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("first InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	homeDir := filepath.Join(globalDir, "templates", "default", "home")

	// Delete common home files
	filesToDelete := []string{".tmux.conf", ".zshrc", ".gitconfig"}
	for _, f := range filesToDelete {
		p := filepath.Join(homeDir, f)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue // skip if it wasn't seeded
		}
		if err := os.Remove(p); err != nil {
			t.Fatalf("failed to delete %s: %v", f, err)
		}
	}

	// Re-run init — should restore deleted files
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("second InitMachine failed: %v", err)
	}

	for _, f := range filesToDelete {
		p := filepath.Join(homeDir, f)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("expected %s to be restored after re-init", f)
		}
	}
}

func TestInitMachine_PreservesCustomizedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("first InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)

	// Customize agents.md
	agentsMdPath := filepath.Join(globalDir, "templates", "default", "agents.md")
	customContent := "my custom agent instructions"
	if err := os.WriteFile(agentsMdPath, []byte(customContent), 0644); err != nil {
		t.Fatalf("failed to write custom agents.md: %v", err)
	}

	// Customize home/.tmux.conf
	tmuxPath := filepath.Join(globalDir, "templates", "default", "home", ".tmux.conf")
	if err := os.WriteFile(tmuxPath, []byte("custom tmux"), 0644); err != nil {
		t.Fatalf("failed to write custom .tmux.conf: %v", err)
	}

	// Re-run init — should NOT overwrite customized files
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("second InitMachine failed: %v", err)
	}

	data, err := os.ReadFile(agentsMdPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != customContent {
		t.Error("re-init overwrote customized agents.md")
	}

	data, err = os.ReadFile(tmuxPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "custom tmux" {
		t.Error("re-init overwrote customized .tmux.conf")
	}
}

func TestInitMachine_PreservesSettings(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("first InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	settingsPath := filepath.Join(globalDir, "settings.yaml")

	// Read original settings
	originalSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	// Customize settings
	customSettings := string(originalSettings) + "\n# custom comment\n"
	if err := os.WriteFile(settingsPath, []byte(customSettings), 0644); err != nil {
		t.Fatal(err)
	}

	// Re-run init — should NOT overwrite settings
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("second InitMachine failed: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != customSettings {
		t.Error("re-init overwrote customized settings.yaml")
	}
}

func TestWriteProjectSettings_V1PlacesProjectIDUnderHub(t *testing.T) {
	tmpDir := t.TempDir()
	projectID := "test-grove-id-abc123"

	err := writeProjectSettings(tmpDir, "/tmp/project", projectID, InitProjectOpts{SkipRuntimeCheck: true})
	if err != nil {
		t.Fatalf("writeProjectSettings failed: %v", err)
	}

	// Read the written settings file
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	if err != nil {
		t.Fatalf("failed to read settings: %v", err)
	}

	// Parse into a generic map to verify the structure
	var settingsMap map[string]interface{}
	if err := yaml.Unmarshal(data, &settingsMap); err != nil {
		t.Fatalf("failed to parse settings YAML: %v", err)
	}

	// Verify schema_version is "1" (from default project settings)
	if v, _ := settingsMap["schema_version"].(string); v != "1" {
		t.Skipf("default project settings are not v1 format (schema_version=%q), skipping v1-specific test", v)
	}

	// project_id should NOT be at the top level
	if _, exists := settingsMap["project_id"]; exists {
		t.Error("project_id should not be at the top level in v1 format; expected it under hub.project_id")
	}

	// project_id should be under hub.project_id
	hub, ok := settingsMap["hub"].(map[string]interface{})
	if !ok {
		t.Fatal("expected hub section in settings")
	}
	if hub["project_id"] != projectID {
		t.Errorf("expected hub.project_id=%q, got %v", projectID, hub["project_id"])
	}
}

// TestInitMachine_CloudRunSandbox_SeedsCorrectProfile is the pin test for
// task #92. On a Cloud Run Instance with sandbox launcher, InitMachine must
// seed settings with a single "default" profile pointing to cloudrun-sandbox.
// The bug: without this, the workstation defaults (local/docker +
// remote/kubernetes) are seeded. buildInfoProfiles filters out the docker
// profile (local-only on a non-local broker), leaving only kubernetes —
// which cannot work on this tier.
//
// This test asserts the SEED FILE layer (pre-merge). Two kinds of assertions:
//
// LOAD-BEARING (determine behavior after koanf merge):
//   - active_profile == "default" → koanf scalar overwrite makes this the
//     effective active profile, which is what ResolveRuntime("") resolves.
//   - profiles["default"].runtime == "cloudrun-sandbox" → this profile
//     survives the merge and carries the correct runtime type.
//
// SEED-LAYER-ONLY (true of the seed file but NOT of effective settings):
//   - len(profiles) == 1 → the seed defines one profile, but koanf merge
//     adds local and remote from embedded defaults (effective has 3).
//   - no "local" / no "remote" profiles → absent from seed, but present
//     in effective settings after merge with embedded defaults.
//   - no "kubernetes" runtime → absent from seed, but present in effective.
//
// These supplementary assertions document what the template CONTAINS, not
// what the system USES. The end-to-end test
// TestInitMachine_CloudRunSandbox_EffectiveSettings_Task92 pins the
// post-merge state that actually governs behavior.
func TestInitMachine_CloudRunSandbox_SeedsCorrectProfile(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Simulate Cloud Run Instance with sandbox launcher.
	t.Setenv("CLOUD_RUN_INSTANCE", "test-instance-001")
	origSandboxBinExists := sandboxBinExists
	sandboxBinExists = func(path string) bool { return true }
	defer func() { sandboxBinExists = origSandboxBinExists }()

	// SkipRuntimeCheck=true is what hosted mode passes.
	if err := InitMachine(GetMockHarnesses(), InitMachineOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	// Load the seeded settings and verify.
	globalDir := filepath.Join(tmpDir, GlobalDir)
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read seeded settings.yaml: %v", err)
	}

	var settingsMap map[string]interface{}
	if err := yaml.Unmarshal(data, &settingsMap); err != nil {
		t.Fatalf("failed to parse settings.yaml: %v", err)
	}

	// Assert active_profile is "default" (not "local").
	activeProfile, ok := settingsMap["active_profile"].(string)
	if !ok || activeProfile != "default" {
		t.Errorf("active_profile = %q, want %q", activeProfile, "default")
	}

	// Assert the PROFILE LIST: exactly one profile, "default", with runtime
	// "cloudrun-sandbox". The old bug produced two profiles: "local" (docker)
	// and "remote" (kubernetes).
	profiles, ok := settingsMap["profiles"].(map[string]interface{})
	if !ok {
		t.Fatalf("profiles section missing or wrong type: %v", settingsMap["profiles"])
	}

	if len(profiles) != 1 {
		t.Errorf("expected exactly 1 profile, got %d: %v", len(profiles), profileNames(profiles))
	}

	defaultProfile, ok := profiles["default"].(map[string]interface{})
	if !ok {
		t.Fatalf("profile 'default' missing; profiles are: %v", profileNames(profiles))
	}
	if rt := defaultProfile["runtime"]; rt != "cloudrun-sandbox" {
		t.Errorf("profile 'default' runtime = %q, want %q", rt, "cloudrun-sandbox")
	}

	// Negative assertion: the profiles that caused the bug must NOT be present.
	if _, hasLocal := profiles["local"]; hasLocal {
		t.Error("profile 'local' is present — it should not exist on the Cloud Run sandbox tier")
	}
	if _, hasRemote := profiles["remote"]; hasRemote {
		t.Error("profile 'remote' is present — it should not exist on the Cloud Run sandbox tier")
	}

	// Assert runtimes: exactly cloudrun-sandbox.
	runtimes, ok := settingsMap["runtimes"].(map[string]interface{})
	if !ok {
		t.Fatalf("runtimes section missing: %v", settingsMap["runtimes"])
	}
	if _, hasCRS := runtimes["cloudrun-sandbox"]; !hasCRS {
		t.Errorf("runtime 'cloudrun-sandbox' missing; runtimes are: %v", runtimeNames(runtimes))
	}
	if _, hasK8s := runtimes["kubernetes"]; hasK8s {
		t.Error("runtime 'kubernetes' is present — it should not exist on the Cloud Run sandbox tier")
	}
}

// TestInitMachine_CloudRunSandbox_EffectiveSettings_Task92 is the R1 pin test
// for task #92. It runs real InitMachine (Cloud Run sandbox environment) →
// real LoadEffectiveSettings and asserts the POST-MERGE state that governs
// runtime selection. This is the load-bearing invariant: koanf loads embedded
// defaults first (lowest priority) then the seeded settings.yaml, so the
// scalar active_profile is OVERWRITTEN to "default" while the profiles map
// MERGES to three entries (local, remote, default). An empty profile
// selection ("Use broker default") resolves through ResolveRuntime("") →
// ActiveProfile → "default" → cloudrun-sandbox.
//
// Without this pin, a change to merge order, embedded defaults, or the
// template's active_profile key could silently re-break §1. (R1/rev2)
func TestInitMachine_CloudRunSandbox_EffectiveSettings_Task92(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Simulate Cloud Run Instance with sandbox launcher.
	t.Setenv("CLOUD_RUN_INSTANCE", "test-instance-r1")
	origSandboxBinExists := sandboxBinExists
	sandboxBinExists = func(path string) bool { return true }
	defer func() { sandboxBinExists = origSandboxBinExists }()

	// Seed via real InitMachine — same as a fresh deploy.
	if err := InitMachine(GetMockHarnesses(), InitMachineOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	// Load EFFECTIVE settings — the same call buildInfoProfiles and
	// resolveManagerForOpts make. This is post-koanf-merge: embedded defaults
	// (local/docker + remote/kubernetes) merged with seeded settings.yaml
	// (default/cloudrun-sandbox).
	globalDir := filepath.Join(tmpDir, GlobalDir)
	vs, _, err := LoadEffectiveSettings(globalDir)
	if err != nil {
		t.Fatalf("LoadEffectiveSettings failed: %v", err)
	}

	// LOAD-BEARING ASSERTION 1: ActiveProfile must be "default".
	// The koanf scalar overwrite (seed "default" overwrites embedded "local")
	// is the mechanism that makes the fix work. If this is "local", an empty
	// profile selection resolves to docker, which is the bug.
	if vs.ActiveProfile != "default" {
		t.Errorf("effective ActiveProfile = %q, want %q — koanf scalar overwrite failed", vs.ActiveProfile, "default")
	}

	// LOAD-BEARING ASSERTION 2: ResolveRuntime("") must yield cloudrun-sandbox.
	// This is the exact call path that fires when the UI sends an empty profile
	// ("Use broker default"): ResolveRuntime("") → uses ActiveProfile →
	// looks up profile "default" → runtime cloudrun-sandbox.
	_, runtimeType, err := vs.ResolveRuntime("")
	if err != nil {
		t.Fatalf("ResolveRuntime(\"\") failed: %v — profile 'default' not found in effective settings", err)
	}
	if runtimeType != "cloudrun-sandbox" {
		t.Errorf("ResolveRuntime(\"\") = %q, want %q", runtimeType, "cloudrun-sandbox")
	}

	// SUPPLEMENTARY: verify the merge produced the expected profile set.
	// Three profiles after merge: local (from embedded), remote (from embedded),
	// default (from seed). This is not load-bearing — the two assertions above
	// are — but it documents the merge shape.
	if len(vs.Profiles) < 3 {
		names := make([]string, 0, len(vs.Profiles))
		for k := range vs.Profiles {
			names = append(names, k)
		}
		t.Logf("effective profiles: %v (expected ≥3 from merge)", names)
	}
}

// TestDefaultSandboxBin_MatchesLiteral pins config's unexported
// defaultSandboxBin against the same literal that
// TestSandboxBinConstantSync_Task92 (in the external config_test package)
// pins runtime.DefaultSandboxBin against. Together these two tests catch
// drift in either direction. (O5)
func TestDefaultSandboxBin_MatchesLiteral(t *testing.T) {
	if defaultSandboxBin != "/usr/local/gcp/bin/sandbox" {
		t.Errorf("defaultSandboxBin = %q, want %q — update both config and runtime if this changes",
			defaultSandboxBin, "/usr/local/gcp/bin/sandbox")
	}
}

// TestInitMachine_CloudRunSandbox_WithoutSandboxBin_FallsBack verifies that
// when CLOUD_RUN_INSTANCE is set but the sandbox binary is absent,
// InitMachine falls back to the standard hosted-mode path (docker defaults).
// This guards against accidentally applying the single-node template to
// Cloud Run Instances that lack the sandbox launcher.
func TestInitMachine_CloudRunSandbox_WithoutSandboxBin_FallsBack(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// CLOUD_RUN_INSTANCE is set, but sandbox binary is NOT available.
	t.Setenv("CLOUD_RUN_INSTANCE", "test-instance-002")
	origSandboxBinExists := sandboxBinExists
	sandboxBinExists = func(path string) bool { return false }
	defer func() { sandboxBinExists = origSandboxBinExists }()

	if err := InitMachine(GetMockHarnesses(), InitMachineOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("failed to read seeded settings.yaml: %v", err)
	}

	var settingsMap map[string]interface{}
	if err := yaml.Unmarshal(data, &settingsMap); err != nil {
		t.Fatalf("failed to parse settings.yaml: %v", err)
	}

	// Without sandbox binary, should fall back to workstation defaults.
	activeProfile := settingsMap["active_profile"].(string)
	if activeProfile != "local" {
		t.Errorf("active_profile = %q, want %q (standard fallback)", activeProfile, "local")
	}

	profiles := settingsMap["profiles"].(map[string]interface{})
	if _, hasLocal := profiles["local"]; !hasLocal {
		t.Error("profile 'local' should be present in standard fallback")
	}
}

// TestInitMachine_NonCloudRun_SkipRuntimeCheck_UnchangedBehavior verifies
// that the fix does not alter behavior for non-Cloud-Run hosted deployments
// (e.g., a self-hosted instance on a VM).
func TestInitMachine_NonCloudRun_SkipRuntimeCheck_UnchangedBehavior(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// No CLOUD_RUN_INSTANCE env var.
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	origSandboxBinExists := sandboxBinExists
	sandboxBinExists = func(path string) bool { return false }
	defer func() { sandboxBinExists = origSandboxBinExists }()

	if err := InitMachine(GetMockHarnesses(), InitMachineOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	data, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	if err != nil {
		t.Fatalf("failed to read settings.yaml: %v", err)
	}

	var settingsMap map[string]interface{}
	if err := yaml.Unmarshal(data, &settingsMap); err != nil {
		t.Fatalf("failed to parse settings.yaml: %v", err)
	}

	// Standard hosted-mode defaults: active_profile=local, profiles include
	// local (docker) and remote (kubernetes).
	if ap := settingsMap["active_profile"].(string); ap != "local" {
		t.Errorf("active_profile = %q, want 'local'", ap)
	}
	profiles := settingsMap["profiles"].(map[string]interface{})
	if _, hasLocal := profiles["local"]; !hasLocal {
		t.Error("profile 'local' missing from standard hosted defaults")
	}
}

func profileNames(profiles map[string]interface{}) []string {
	names := make([]string, 0, len(profiles))
	for k := range profiles {
		names = append(names, k)
	}
	return names
}

func runtimeNames(runtimes map[string]interface{}) []string {
	names := make([]string, 0, len(runtimes))
	for k := range runtimes {
		names = append(names, k)
	}
	return names
}

// TestInitMachine_CloudRunSandbox_SkipRuntimeCheckFalse_SeedsCorrectTemplate
// is the load-bearing assertion for the environment-predicate-dominance fix.
//
// Scenario: CLOUD_RUN_INSTANCE set, sandbox binary present, but the caller
// passes SkipRuntimeCheck: false (e.g. the Hub /api/system/init handler).
// Before the fix, this fell into the else branch, called DetectLocalRuntime(),
// and failed — or, if runtime detection happened to succeed, seeded the
// workstation template instead of the cloudrun-sandbox template.
//
// After the fix, isCloudRunSandboxEnvironment() dominates: the machine fact
// settles the template choice regardless of the caller preference.
func TestInitMachine_CloudRunSandbox_SkipRuntimeCheckFalse_SeedsCorrectTemplate(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Simulate Cloud Run Instance with sandbox launcher.
	t.Setenv("CLOUD_RUN_INSTANCE", "test-instance-dominance")
	origSandboxBinExists := sandboxBinExists
	sandboxBinExists = func(path string) bool { return true }
	defer func() { sandboxBinExists = origSandboxBinExists }()

	// Mock runtime detection to succeed with "docker" — this ensures the
	// mutation test (inverting the condition to `if false`) seeds the WRONG
	// template (workstation defaults) rather than erroring out, so the
	// content assertion is the one that goes red. On a real Cloud Run Instance
	// detection would fail, but for this test the content assertion is more
	// valuable than the error assertion.
	mockRuntimeDetection(t, "docker")

	// SkipRuntimeCheck: false — the caller did NOT ask to skip detection.
	if err := InitMachine(GetMockHarnesses(), InitMachineOpts{SkipRuntimeCheck: false}); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	// Read the seeded file and assert its CONTENT matches the cloudrun-sandbox
	// template. Byte-for-byte comparison is not possible because ensureBrokerID()
	// mutates the file after seeding (adds broker_id, strips comments via YAML
	// round-trip). The semantic comparison below catches the actual bug: seeding
	// the wrong template.
	globalDir := filepath.Join(tmpDir, GlobalDir)
	seededPath := filepath.Join(globalDir, "settings.yaml")
	seeded, err := os.ReadFile(seededPath)
	if err != nil {
		t.Fatalf("failed to read seeded settings.yaml: %v", err)
	}

	// Parse the seeded file.
	var seededMap map[string]interface{}
	if err := yaml.Unmarshal(seeded, &seededMap); err != nil {
		t.Fatalf("failed to parse seeded settings.yaml: %v", err)
	}

	// Parse the embedded cloudrun-sandbox template for comparison.
	expectedBytes, err := EmbedsFS.ReadFile("embeds/default_settings_cloudrun_sandbox.yaml")
	if err != nil {
		t.Fatalf("failed to read embedded template: %v", err)
	}
	var expectedMap map[string]interface{}
	if err := yaml.Unmarshal(expectedBytes, &expectedMap); err != nil {
		t.Fatalf("failed to parse embedded template: %v", err)
	}

	// Assert the template-defining fields match: active_profile, profiles, runtimes.
	// These are the fields that distinguish the cloudrun-sandbox template from the
	// workstation template. Differences in broker_id or other post-seed mutations
	// are expected and not bugs.
	if ap := seededMap["active_profile"]; ap != expectedMap["active_profile"] {
		t.Errorf("active_profile = %q, want %q", ap, expectedMap["active_profile"])
	}

	seededProfiles, _ := seededMap["profiles"].(map[string]interface{})
	expectedProfiles, _ := expectedMap["profiles"].(map[string]interface{})

	if len(seededProfiles) != len(expectedProfiles) {
		t.Errorf("profile count: got %d (%v), want %d (%v)",
			len(seededProfiles), profileNames(seededProfiles),
			len(expectedProfiles), profileNames(expectedProfiles))
	}
	for name, ep := range expectedProfiles {
		sp, ok := seededProfiles[name]
		if !ok {
			t.Errorf("profile %q missing from seeded settings", name)
			continue
		}
		epMap, _ := ep.(map[string]interface{})
		spMap, _ := sp.(map[string]interface{})
		if epMap["runtime"] != spMap["runtime"] {
			t.Errorf("profile %q runtime: got %q, want %q", name, spMap["runtime"], epMap["runtime"])
		}
	}

	seededRuntimes, _ := seededMap["runtimes"].(map[string]interface{})
	expectedRuntimes, _ := expectedMap["runtimes"].(map[string]interface{})
	if len(seededRuntimes) != len(expectedRuntimes) {
		t.Errorf("runtime count: got %d (%v), want %d (%v)",
			len(seededRuntimes), runtimeNames(seededRuntimes),
			len(expectedRuntimes), runtimeNames(expectedRuntimes))
	}
	for name := range expectedRuntimes {
		if _, ok := seededRuntimes[name]; !ok {
			t.Errorf("runtime %q missing from seeded settings", name)
		}
	}

	// Negative: workstation-only profiles and runtimes must NOT be present.
	for _, bad := range []string{"local", "remote"} {
		if _, ok := seededProfiles[bad]; ok {
			t.Errorf("profile %q is present — Cloud Run sandbox must not have workstation profiles", bad)
		}
	}
	for _, bad := range []string{"docker", "podman", "kubernetes"} {
		if _, ok := seededRuntimes[bad]; ok {
			t.Errorf("runtime %q is present — Cloud Run sandbox must not have workstation runtimes", bad)
		}
	}
}

// TestInitMachine_NonCloudRun_SkipRuntimeCheckFalse_SeedsWorkstationTemplate
// is the negative assertion: on a non-Cloud-Run machine with SkipRuntimeCheck
// false, the workstation defaults must be seeded and DetectLocalRuntime must
// be consulted. This protects against accidentally seeding the cloudrun
// template on a laptop.
func TestInitMachine_NonCloudRun_SkipRuntimeCheckFalse_SeedsWorkstationTemplate(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	// Not a Cloud Run Instance.
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	origSandboxBinExists := sandboxBinExists
	sandboxBinExists = func(path string) bool { return false }
	defer func() { sandboxBinExists = origSandboxBinExists }()

	// Make runtime detection succeed — DetectLocalRuntime must be consulted.
	mockRuntimeDetection(t, "docker")

	// SkipRuntimeCheck: false — standard workstation init.
	if err := InitMachine(GetMockHarnesses(), InitMachineOpts{SkipRuntimeCheck: false}); err != nil {
		t.Fatalf("InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	seeded, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
	if err != nil {
		t.Fatalf("failed to read seeded settings.yaml: %v", err)
	}

	var settingsMap map[string]interface{}
	if err := yaml.Unmarshal(seeded, &settingsMap); err != nil {
		t.Fatalf("failed to parse settings.yaml: %v", err)
	}

	// Must be workstation defaults: active_profile=local, profiles include local.
	if ap, _ := settingsMap["active_profile"].(string); ap != "local" {
		t.Errorf("active_profile = %q, want %q", ap, "local")
	}
	profiles, _ := settingsMap["profiles"].(map[string]interface{})
	if _, hasLocal := profiles["local"]; !hasLocal {
		t.Error("profile 'local' missing from workstation defaults")
	}

	// Must NOT have the cloudrun-sandbox profile or runtime.
	if _, hasCRS := profiles["default"]; hasCRS {
		dp, _ := profiles["default"].(map[string]interface{})
		if rt, _ := dp["runtime"].(string); rt == "cloudrun-sandbox" {
			t.Error("cloudrun-sandbox profile 'default' is present — workstation must not get the Cloud Run template")
		}
	}

	// Verify the seeded content is NOT the cloudrun-sandbox template.
	cloudrunTemplate, _ := EmbedsFS.ReadFile("embeds/default_settings_cloudrun_sandbox.yaml")
	if string(seeded) == string(cloudrunTemplate) {
		t.Error("seeded settings.yaml matches the cloudrun-sandbox template — workstation should get workstation defaults")
	}
}
