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
		t.Error("expected non-empty grove ID")
	}

	// Should look like a UUID (contains hyphens, ~36 chars)
	if !strings.Contains(id, "-") || len(id) != 36 {
		t.Errorf("expected UUID format, got: %q", id)
	}
}

func TestIsInsideProject(t *testing.T) {
	// Unset Hub context to avoid synthetic project root detection
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_GROVE_ID"} {
		if val, ok := os.LookupEnv(e); ok {
			os.Unsetenv(e)
			defer os.Setenv(e, val)
		}
	}

	// Create a directory with .scion
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

	// When in the grove directory
	if err := os.Chdir(tmpProject); err != nil {
		t.Fatal(err)
	}
	if !IsInsideProject() {
		t.Error("expected IsInsideProject=true when in grove directory")
	}

	// When in a subdirectory of the grove
	subDir := filepath.Join(tmpProject, "subdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}
	if !IsInsideProject() {
		t.Error("expected IsInsideProject=true when in subdirectory of grove")
	}

	// When outside any grove
	outsideDir := t.TempDir()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	if IsInsideProject() {
		t.Error("expected IsInsideProject=false when outside any grove")
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
	defer os.Chdir(origWd)

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create a subdirectory
	subDir := filepath.Join(tmpProject, "subdir", "deep")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	// When in the subdirectory, should find the enclosing grove
	if err := os.Chdir(subDir); err != nil {
		t.Fatal(err)
	}

	projectPath, rootDir, found := GetEnclosingProjectPath()
	if !found {
		t.Fatal("expected to find enclosing grove")
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
	defer os.Chdir(origWd)

	// Set HOME to a clean temp dir
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	_, _, found := GetEnclosingProjectPath()
	if found {
		t.Error("expected found=false when no enclosing grove")
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
	os.MkdirAll(targetDir, 0755)

	// Write a custom file first
	customContent := "custom content"
	os.WriteFile(filepath.Join(targetDir, "agents.md"), []byte(customContent), 0644)

	// Write a custom home/.tmux.conf
	homeDir := filepath.Join(targetDir, "home")
	os.MkdirAll(homeDir, 0755)
	os.WriteFile(filepath.Join(homeDir, ".tmux.conf"), []byte(customContent), 0644)

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
	os.MkdirAll(targetDir, 0755)

	// Write custom files first
	os.WriteFile(filepath.Join(targetDir, "agents.md"), []byte("custom"), 0644)
	homeDir := filepath.Join(targetDir, "home")
	os.MkdirAll(homeDir, 0755)
	os.WriteFile(filepath.Join(homeDir, ".tmux.conf"), []byte("custom"), 0644)

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

	// Override HOME for global templates and external grove-config dirs
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Use explicit targetDir to avoid CWD-based resolution issues
	projectDir := filepath.Join(tmpDir, "project", DotScion)

	if err := InitProject(projectDir, GetMockHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Templates always live in the in-repo projectDir (for git groves) or in the
	// external config dir (for non-git groves). Since tests run inside a git repo,
	// projectDir is used directly.
	templatesDir := filepath.Join(projectDir, "templates")
	if info, err := os.Stat(templatesDir); err != nil || !info.IsDir() {
		t.Fatalf("expected templates/ directory to exist at %s", templatesDir)
	}

	// Verify templates/default/ does NOT exist (default template lives in global grove only)
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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	for _, name := range []string{"gemini", "claude", "opencode", "codex"} {
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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	for _, name := range []string{"gemini", "claude", "opencode", "codex"} {
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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	err := InitMachine(GetMockHarnesses())
	if err == nil {
		t.Fatal("expected InitMachine to fail when no container runtime is available")
	}
	if !strings.Contains(err.Error(), "no supported container runtime found") {
		t.Errorf("expected error about missing runtime, got: %v", err)
	}
}

func TestInitProject_FailsWithNoRuntime(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetectionNone(t)

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	projectDir := filepath.Join(tmpDir, "project", DotScion)
	if err := InitProject(projectDir, GetMockHarnesses()); err != nil {
		t.Fatalf("InitProject failed: %v", err)
	}

	// Project settings should not contain profiles or runtimes; those live in global settings.
	// For git groves settings.yaml is in the external config dir; use GetProjectConfigDir
	// to find the canonical location regardless of grove type.
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
		t.Errorf("expected grove settings.yaml to have no profiles block, got %d profiles", len(settings.Profiles))
	}
	if len(settings.Runtimes) != 0 {
		t.Errorf("expected grove settings.yaml to have no runtimes block, got %d runtimes", len(settings.Runtimes))
	}
	if settings.ActiveProfile != "local" {
		t.Errorf("expected active_profile 'local', got %q", settings.ActiveProfile)
	}
}

func TestInitMachine_RestoresDeletedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// First init seeds everything
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("first InitMachine failed: %v", err)
	}

	globalDir := filepath.Join(tmpDir, GlobalDir)
	defaultTplDir := filepath.Join(globalDir, "templates", "default")

	// Read the original agents.md content
	agentsMdPath := filepath.Join(defaultTplDir, "agents.md")
	originalContent, err := os.ReadFile(agentsMdPath)
	if err != nil {
		t.Fatalf("failed to read agents.md: %v", err)
	}
	if len(originalContent) == 0 {
		t.Fatal("expected agents.md to have content")
	}

	// Delete agents.md
	if err := os.Remove(agentsMdPath); err != nil {
		t.Fatalf("failed to delete agents.md: %v", err)
	}
	if _, err := os.Stat(agentsMdPath); !os.IsNotExist(err) {
		t.Fatal("expected agents.md to be deleted")
	}

	// Re-run init — should restore the deleted file
	if err := InitMachine(GetMockHarnesses()); err != nil {
		t.Fatalf("second InitMachine failed: %v", err)
	}

	restoredContent, err := os.ReadFile(agentsMdPath)
	if err != nil {
		t.Fatalf("agents.md was not restored after re-init: %v", err)
	}
	if string(restoredContent) != string(originalContent) {
		t.Error("restored agents.md content does not match original embedded content")
	}
}

func TestInitMachine_RestoresDeletedCommonFiles(t *testing.T) {
	tmpDir := t.TempDir()
	mockRuntimeDetection(t, "docker")

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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

func TestWriteGroveSettings_V1PlacesProjectIDUnderHub(t *testing.T) {
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

	// Verify schema_version is "1" (from default grove settings)
	if v, _ := settingsMap["schema_version"].(string); v != "1" {
		t.Skipf("default grove settings are not v1 format (schema_version=%q), skipping v1-specific test", v)
	}

	// grove_id should NOT be at the top level
	if _, exists := settingsMap["grove_id"]; exists {
		t.Error("grove_id should not be at the top level in v1 format; expected it under hub.grove_id")
	}

	// grove_id should be under hub.grove_id
	hub, ok := settingsMap["hub"].(map[string]interface{})
	if !ok {
		t.Fatal("expected hub section in settings")
	}
	if hub["grove_id"] != projectID {
		t.Errorf("expected hub.grove_id=%q, got %v", projectID, hub["grove_id"])
	}
}
